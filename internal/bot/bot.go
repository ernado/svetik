// Package bot implements the Telegram transport and message-handling
// orchestration. It depends only on the root contracts (lilith.DB, lilith.AI,
// lilith.Memory, lilith.FileStore) for cross-layer communication.
package bot

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/zctx"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/ernado/lilith"
	"github.com/ernado/lilith/internal/thread"
)

const (
	// channelParticipantsTTL is the minimum interval between participant list refreshes.
	channelParticipantsTTL = 10 * time.Minute

	// chatMemberTTL is the TTL for application-level chat member cache entries.
	chatMemberTTL = 5 * time.Minute

	// chatContextWindowMessages is total messages passed to model context.
	chatContextWindowMessages = 150

	// implicitResponseProbability is the probability of responding to a message
	// that does not explicitly mention the bot.
	implicitResponseProbability = 0.05

	// idleCheckInterval is how often the idle checker runs.
	idleCheckInterval = 1 * time.Minute

	// idleMinDuration is the minimum inactivity time before the bot may write unprompted.
	idleMinDuration = 30 * time.Minute

	// idleMaxDuration is the upper bound of the random inactivity threshold.
	idleMaxDuration = 2 * time.Hour

	// scrapeTimeout bounds fetching an unresolved link preview so a slow page
	// cannot stall message handling. It is generous because the scraper drives a
	// headless browser that must wait out redirects and bot checks.
	scrapeTimeout = 60 * time.Second

	// maxScrapeTextLen caps how many runes of scraped body text are kept for
	// model context.
	maxScrapeTextLen = 2000
)

// chatMemberKey is the cache key for a chat member.
type chatMemberKey struct {
	chatID int64
	userID int64
}

// cachedChatMember wraps a chat member with its fetch timestamp.
type cachedChatMember struct {
	member    *lilith.ChatMember
	fetchedAt time.Time
}

// App is the Telegram bot orchestrator.
type App struct {
	api     *tg.Client
	client  *telegram.Client
	db      lilith.DB
	ai      lilith.AI
	memory  lilith.Memory
	files   lilith.FileStore
	scraper lilith.Scraper
	self    *tg.User

	waiter *floodwait.Waiter
	trace  trace.Tracer

	// channelParticipantsMu guards channelParticipantsFetchedAt.
	channelParticipantsMu        sync.Mutex
	channelParticipantsFetchedAt map[int64]time.Time

	// chatMemberMu guards chatMemberCache.
	chatMemberMu    sync.Mutex
	chatMemberCache map[chatMemberKey]*cachedChatMember

	// chatPeersMu guards chatPeers.
	chatPeersMu sync.Mutex
	chatPeers   map[int64]tg.InputPeerClass
}

// New constructs an App. files may be nil to disable media handling, and
// scraper may be nil to disable fetching unresolved link previews.
func New(
	client *telegram.Client,
	db lilith.DB,
	ai lilith.AI,
	mem lilith.Memory,
	files lilith.FileStore,
	scraper lilith.Scraper,
	waiter *floodwait.Waiter,
	tracer trace.Tracer,
) *App {
	return &App{
		api:                          tg.NewClient(client),
		client:                       client,
		db:                           db,
		ai:                           ai,
		memory:                       mem,
		files:                        files,
		scraper:                      scraper,
		waiter:                       waiter,
		trace:                        tracer,
		channelParticipantsFetchedAt: make(map[int64]time.Time),
		chatMemberCache:              make(map[chatMemberKey]*cachedChatMember),
		chatPeers:                    make(map[int64]tg.InputPeerClass),
	}
}

// Register wires the App's handlers into the update dispatcher.
func (a *App) Register(d tg.UpdateDispatcher) {
	d.OnChannelParticipant(a.onChannelParticipant)
	d.OnNewMessage(a.onNewMessage)
	d.OnNewChannelMessage(a.onNewChannelMessage)
}

func (a *App) Run(ctx context.Context) error {
	return a.waiter.Run(ctx, func(ctx context.Context) error {
		return a.client.Run(ctx, func(ctx context.Context) error {
			lg := zctx.From(ctx)
			if self, err := a.client.Self(ctx); err != nil || !self.Bot {
				if _, err := a.client.Auth().Bot(ctx, os.Getenv("BOT_TOKEN")); err != nil {
					return errors.Wrap(err, "auth bot")
				}
			} else {
				lg.Info("Already authenticated")
			}
			if self, err := a.client.Self(ctx); err == nil {
				lg.Info("Bot info",
					zap.Int64("id", self.ID),
					zap.String("username", self.Username),
					zap.String("first_name", self.FirstName),
				)

				a.self = self
			}
			if _, err := a.api.BotsSetBotCommands(ctx, &tg.BotsSetBotCommandsRequest{
				Scope:    &tg.BotCommandScopeDefault{},
				LangCode: "en",
				Commands: []tg.BotCommand{
					{
						Command:     "start",
						Description: "Начать",
					},
					{
						Command:     "lobotomy",
						Description: "Очистить память",
					},
					{
						Command:     "model",
						Description: "Показать или установить модель",
					},
				},
			}); err != nil {
				return errors.Wrap(err, "set commands")
			}
			if err := a.loadChatPeersFromDB(ctx); err != nil {
				zctx.From(ctx).Error("load chat peers from db", zap.Error(err))
			}
			go a.runIdleChecker(ctx)
			<-ctx.Done()
			return ctx.Err()
		})
	})
}

func (a *App) addChannel(ctx context.Context, channel *tg.Channel) error {
	zctx.From(ctx).Info("Channel added",
		zap.Int64("id", channel.ID),
		zap.String("title", channel.Title),
	)
	return nil
}

func (a *App) removeChannel(ctx context.Context, channel *tg.Channel) error {
	zctx.From(ctx).Info("Channel removed",
		zap.Int64("id", channel.ID),
		zap.String("title", channel.Title),
	)
	return nil
}

func (a *App) onChannelParticipant(ctx context.Context, e tg.Entities, update *tg.UpdateChannelParticipant) error {
	switch update.NewParticipant.(type) {
	case *tg.ChannelParticipantBanned:
		// Bot was removed from channel.
		for _, c := range e.Channels {
			return a.removeChannel(ctx, c)
		}
	case *tg.ChannelParticipantAdmin:
		// Bot was added to channel.
		for _, c := range e.Channels {
			return a.addChannel(ctx, c)
		}
	default:
		if update.NewParticipant == nil {
			// Removed from channel.
			for _, c := range e.Channels {
				return a.removeChannel(ctx, c)
			}
		}
	}
	return nil
}

func extractUserID(m *tg.Message) (int64, bool) {
	if peerUser, ok := m.FromID.(*tg.PeerUser); ok {
		return peerUser.UserID, true
	}
	if peerUser, ok := m.PeerID.(*tg.PeerUser); ok {
		return peerUser.UserID, true
	}
	return 0, false
}

// chatContext holds resolved information about the chat and the message author's role.
type chatContext struct {
	chatID        int64
	chatInfo      string
	chatType      lilith.ChatType
	accessHash    int64
	selfRank      string
	userIsAdmin   bool
	userIsCreator bool
	userRank      string
}

func (a *App) resolveRegularChat(ctx context.Context, chat *tg.Chat, userID int64) (*chatContext, error) {
	full, err := a.api.MessagesGetFullChat(ctx, chat.ID)
	if err != nil {
		return nil, errors.Wrap(err, "get full chat")
	}

	chatFull, ok := full.FullChat.(*tg.ChatFull)
	if !ok {
		return nil, errors.New("unexpected full chat type")
	}

	// A regular group chat carries its name in the entity Title; chatFull.About
	// is the (usually empty) description. Fall back to the description only when
	// the title is missing.
	chatInfo := chat.Title
	if chatInfo == "" {
		chatInfo = chatFull.About
	}

	cc := &chatContext{
		chatID:   chatFull.ID,
		chatInfo: chatInfo,
		chatType: lilith.ChatTypeChat,
	}

	// Persist the chat before its members to satisfy the chat_members ->
	// chat foreign key constraint.
	if err := a.db.UpsertChat(ctx, lilith.Chat{
		ID:   cc.chatID,
		Info: cc.chatInfo,
		Type: cc.chatType,
	}); err != nil {
		return nil, errors.Wrap(err, "upsert chat")
	}

	// Build a user map from the full chat response for name lookups.
	users := make(map[int64]*tg.User, len(full.Users))
	for _, u := range full.Users {
		if user, ok := u.(*tg.User); ok {
			users[user.ID] = user
		}
	}

	if v, ok := chatFull.Participants.(*tg.ChatParticipants); ok {
		for _, participant := range v.Participants {
			var (
				id        int64
				isAdmin   bool
				isCreator bool
				rank      string
			)

			switch p := participant.(type) {
			case *tg.ChatParticipantAdmin:
				id = p.UserID
				isAdmin = true
				rank = p.Rank
			case *tg.ChatParticipantCreator:
				id = p.UserID
				isCreator = true
				rank = p.Rank
			case *tg.ChatParticipant:
				id = p.UserID
			}

			if id == a.self.ID {
				cc.selfRank = rank
			}

			if id == userID {
				cc.userIsAdmin = isAdmin
				cc.userIsCreator = isCreator
				cc.userRank = rank
			}

			user, ok := users[id]
			if !ok {
				continue
			}

			if err := a.upsertChatMemberCached(ctx, lilith.ChatMember{
				ChatID:    chatFull.ID,
				UserID:    id,
				Username:  user.Username,
				FirstName: user.FirstName,
				LastName:  user.LastName,
				IsAdmin:   isAdmin,
				IsCreator: isCreator,
				Rank:      rank,
			}); err != nil {
				return nil, errors.Wrap(err, "upsert chat member")
			}
		}
	}

	return cc, nil
}

func (a *App) resolveChannel(ctx context.Context, channel *tg.Channel, userID int64) (*chatContext, error) {
	if err := a.fetchChannelParticipantsCached(ctx, channel); err != nil {
		return nil, errors.Wrap(err, "fetch channel participants")
	}

	cc := &chatContext{
		chatID:     channel.ID,
		chatInfo:   channel.Title,
		chatType:   lilith.ChatTypeChannel,
		accessHash: channel.AccessHash,
	}

	if self, err := a.getChatMember(ctx, channel.ID, a.self.ID); err == nil {
		cc.selfRank = self.Rank
	}

	if member, err := a.getChatMember(ctx, channel.ID, userID); err == nil {
		cc.userIsAdmin = member.IsAdmin
		cc.userIsCreator = member.IsCreator
		cc.userRank = member.Rank
	}

	return cc, nil
}

// fetchChannelParticipantsCached fetches channel participants at most once per channelParticipantsTTL.
func (a *App) fetchChannelParticipantsCached(ctx context.Context, channel *tg.Channel) error {
	now := time.Now()

	a.channelParticipantsMu.Lock()
	fetchedAt, ok := a.channelParticipantsFetchedAt[channel.ID]
	if ok && now.Sub(fetchedAt) < channelParticipantsTTL {
		a.channelParticipantsMu.Unlock()
		return nil
	}
	a.channelParticipantsMu.Unlock()

	if err := a.fetchChannelParticipants(ctx, channel); err != nil {
		return err
	}

	a.channelParticipantsMu.Lock()
	a.channelParticipantsFetchedAt[channel.ID] = now
	a.channelParticipantsMu.Unlock()

	return nil
}

// getChatMember returns a chat member, using the application-level cache to
// avoid repeated DB round-trips within chatMemberTTL.
func (a *App) getChatMember(ctx context.Context, chatID, userID int64) (*lilith.ChatMember, error) {
	key := chatMemberKey{chatID: chatID, userID: userID}
	now := time.Now()

	a.chatMemberMu.Lock()
	if entry, ok := a.chatMemberCache[key]; ok && now.Sub(entry.fetchedAt) < chatMemberTTL {
		m := entry.member
		a.chatMemberMu.Unlock()

		return m, nil
	}
	a.chatMemberMu.Unlock()

	member, err := a.db.GetChatMember(ctx, chatID, userID)
	if err != nil {
		return nil, err
	}

	a.chatMemberMu.Lock()
	a.chatMemberCache[key] = &cachedChatMember{member: member, fetchedAt: now}
	a.chatMemberMu.Unlock()

	return member, nil
}

// upsertChatMemberCached calls UpsertChatMember and updates the cache entry so
// subsequent calls within the TTL see the freshly written data.
func (a *App) upsertChatMemberCached(ctx context.Context, m lilith.ChatMember) error {
	if err := a.db.UpsertChatMember(ctx, m); err != nil {
		return err
	}

	key := chatMemberKey{chatID: m.ChatID, userID: m.UserID}

	a.chatMemberMu.Lock()
	a.chatMemberCache[key] = &cachedChatMember{member: &m, fetchedAt: time.Now()}
	a.chatMemberMu.Unlock()

	return nil
}

func (a *App) resolveChatContext(ctx context.Context, e tg.Entities, userID int64) (*chatContext, error) {
	for _, chat := range e.Chats {
		return a.resolveRegularChat(ctx, chat, userID)
	}

	for _, channel := range e.Channels {
		return a.resolveChannel(ctx, channel, userID)
	}

	// Private chat: no chat or channel entity; the peer is the user themselves.
	if user, ok := e.Users[userID]; ok {
		return a.resolvePrivateChat(ctx, user)
	}

	return nil, errors.New("no chat or channel in entities")
}

// resolvePrivateChat builds a chatContext for a one-on-one conversation where
// the chat ID equals the user ID.
func (a *App) resolvePrivateChat(ctx context.Context, user *tg.User) (*chatContext, error) {
	cc := &chatContext{
		chatID:     user.ID,
		chatInfo:   strings.TrimSpace(user.FirstName + " " + user.LastName),
		chatType:   lilith.ChatTypePrivate,
		accessHash: user.AccessHash,
	}

	// Persist the chat before its members to satisfy the chat_members ->
	// chat foreign key constraint.
	if err := a.db.UpsertChat(ctx, lilith.Chat{
		ID:         cc.chatID,
		Info:       cc.chatInfo,
		AccessHash: cc.accessHash,
		Type:       cc.chatType,
	}); err != nil {
		return nil, errors.Wrap(err, "upsert chat")
	}

	// A private chat has no participants endpoint, so persist self explicitly.
	// Otherwise self lookups during onMessage fail with "no rows in result set".
	// The other party is upserted by onMessage from the message sender.
	if err := a.upsertChatMemberCached(ctx, lilith.ChatMember{
		ChatID:    cc.chatID,
		UserID:    a.self.ID,
		Username:  a.self.Username,
		FirstName: a.self.FirstName,
		LastName:  a.self.LastName,
	}); err != nil {
		return nil, errors.Wrap(err, "upsert self chat member")
	}

	return cc, nil
}

// russianWeekday returns the Russian name of a weekday.
func russianWeekday(d time.Weekday) string {
	switch d {
	case time.Monday:
		return "понедельник"
	case time.Tuesday:
		return "вторник"
	case time.Wednesday:
		return "среда"
	case time.Thursday:
		return "четверг"
	case time.Friday:
		return "пятница"
	case time.Saturday:
		return "суббота"
	default:
		return "воскресенье"
	}
}

func maxSize(sizes []tg.PhotoSizeClass) string {
	var (
		maxSize string
		maxH    int
	)

	for _, size := range sizes {
		if s, ok := size.(interface {
			GetH() int
			GetType() string
		}); ok && s.GetH() > maxH {
			maxH = s.GetH()
			maxSize = s.GetType()
		}
	}

	return maxSize
}

// photoFromMedia extracts a non-empty photo from message media, handling both a
// directly attached photo and a photo embedded in a web-page link preview.
func photoFromMedia(media tg.MessageMediaClass) (*tg.Photo, bool) {
	switch m := media.(type) {
	case *tg.MessageMediaPhoto:
		return m.Photo.AsNotEmpty()
	case *tg.MessageMediaWebPage:
		page, ok := m.Webpage.(*tg.WebPage)
		if !ok {
			return nil, false
		}
		photo, ok := page.GetPhoto()
		if !ok {
			return nil, false
		}
		return photo.AsNotEmpty()
	default:
		return nil, false
	}
}

// linkPreviewText renders the textual content of a web-page link preview so the
// model can see what a shared link is about. For a resolved preview
// (*tg.WebPage) it returns the site name, title and description. For an
// unresolved one (*tg.WebPageEmpty, which is what bots receive) it scrapes the
// linked page, falling back to the bare URL. Returns an empty string when no
// text is available.
func (a *App) linkPreviewText(ctx context.Context, m *tg.Message) string {
	media, ok := m.Media.(*tg.MessageMediaWebPage)
	if !ok {
		return ""
	}

	switch page := media.Webpage.(type) {
	case *tg.WebPage:
		var lines []string
		if site, ok := page.GetSiteName(); ok && site != "" {
			lines = append(lines, site)
		}
		if title, ok := page.GetTitle(); ok && title != "" {
			lines = append(lines, title)
		}
		if desc, ok := page.GetDescription(); ok && desc != "" {
			lines = append(lines, desc)
		}

		return strings.Join(lines, "\n")
	case *tg.WebPageEmpty:
		// Bots receive no resolved preview, so fetch the page ourselves. The URL
		// may be carried on the empty preview, otherwise it is in the message.
		url, _ := page.GetURL()
		if url == "" {
			url = firstURL(m.Message)
		}
		if url == "" {
			return ""
		}

		return a.scrapeLink(ctx, url)
	default:
		return ""
	}
}

// scrapeLink fetches url and renders its title, description and a truncated body
// for model context. It returns the bare URL when scraping is disabled or fails.
func (a *App) scrapeLink(ctx context.Context, url string) string {
	if a.scraper == nil {
		return url
	}

	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()

	res, err := a.scraper.Scrape(ctx, url)
	if err != nil {
		zctx.From(ctx).Warn("Failed to scrape link",
			zap.String("url", url),
			zap.Error(err),
		)

		return url
	}

	var lines []string
	if res.Title != "" {
		lines = append(lines, res.Title)
	}
	if res.Description != "" {
		lines = append(lines, res.Description)
	}
	if res.Text != "" {
		lines = append(lines, truncate(res.Text, maxScrapeTextLen))
	}

	if len(lines) == 0 {
		return url
	}

	return strings.Join(lines, "\n")
}

// firstURL returns the first http(s) URL found in s, or an empty string.
func firstURL(s string) string {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			return strings.TrimRight(f, ".,)];\"'")
		}
	}

	return ""
}

// truncate shortens s to at most maxRunes runes, appending an ellipsis when cut.
func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}

	return string(r[:maxRunes]) + "…"
}

func (a *App) persistPhoto(ctx context.Context, m *tg.Message) (string, error) {
	if a.files == nil {
		return "", nil
	}

	p, ok := photoFromMedia(m.Media)
	if !ok {
		return "", nil
	}

	out := new(bytes.Buffer)
	if _, err := downloader.NewDownloader().Download(a.api, &tg.InputPhotoFileLocation{
		ID:            p.ID,
		AccessHash:    p.AccessHash,
		FileReference: p.FileReference,
		ThumbSize:     maxSize(p.Sizes),
	}).Stream(ctx, out); err != nil {
		return "", errors.Wrap(err, "download photo")
	}

	photoURI, err := a.files.Upload(out)
	if err != nil {
		return "", errors.Wrap(err, "upload photo")
	}

	return photoURI, nil
}

// storeChatPeer records the input peer for a chat so it can be used for
// proactive (idle) messages later.
func (a *App) storeChatPeer(chatID int64, e tg.Entities) {
	var peer tg.InputPeerClass

	for _, chat := range e.Chats {
		peer = &tg.InputPeerChat{ChatID: chat.ID}
		break
	}

	for _, channel := range e.Channels {
		peer = &tg.InputPeerChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash}
		break
	}

	if peer == nil {
		if user, ok := e.Users[chatID]; ok {
			peer = &tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash}
		}
	}

	if peer == nil {
		return
	}

	a.chatPeersMu.Lock()
	a.chatPeers[chatID] = peer
	a.chatPeersMu.Unlock()
}

// loadChatPeersFromDB populates the in-memory chatPeers map from persisted
// chat records so idle messages can be sent after a bot restart.
func (a *App) loadChatPeersFromDB(ctx context.Context) error {
	chats, err := a.db.GetChats(ctx)
	if err != nil {
		return errors.Wrap(err, "get chats")
	}

	a.chatPeersMu.Lock()
	defer a.chatPeersMu.Unlock()

	for _, chat := range chats {
		switch chat.Type {
		case lilith.ChatTypeChannel:
			a.chatPeers[chat.ID] = &tg.InputPeerChannel{
				ChannelID:  chat.ID,
				AccessHash: chat.AccessHash,
			}
		case lilith.ChatTypePrivate:
			a.chatPeers[chat.ID] = &tg.InputPeerUser{
				UserID:     chat.ID,
				AccessHash: chat.AccessHash,
			}
		default:
			a.chatPeers[chat.ID] = &tg.InputPeerChat{ChatID: chat.ID}
		}
	}

	return nil
}

// runIdleChecker periodically checks all known chats for inactivity and
// triggers an unprompted bot message when the last message was not from the
// bot and the chat has been silent for 2–4 hours.
func (a *App) runIdleChecker(ctx context.Context) {
	ticker := time.NewTicker(idleCheckInterval)
	defer ticker.Stop()

	for {
		if err := a.checkIdleChats(ctx); err != nil {
			zctx.From(ctx).Error("idle checker failed", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) checkIdleChats(ctx context.Context) error {
	chats, err := a.db.GetChats(ctx)
	if err != nil {
		return errors.Wrap(err, "get chats")
	}

	for _, chat := range chats {
		if err := a.checkIdleChat(ctx, chat); err != nil {
			zctx.From(ctx).Error("check idle chat",
				zap.Error(err),
				zap.Int64("chat_id", chat.ID),
			)
		}
	}

	return nil
}

func (a *App) checkIdleChat(ctx context.Context, chat lilith.Chat) error {
	last, err := a.db.GetLastMessage(ctx, chat.ID)
	if err != nil {
		return errors.Wrap(err, "get last message")
	}

	if last == nil {
		return nil
	}

	if last.IsMyself {
		return nil
	}

	// Random threshold in [idleMinDuration, idleMaxDuration).
	threshold := idleMinDuration + time.Duration(rand.Int63n(int64(idleMaxDuration-idleMinDuration)))

	if time.Since(last.Date) < threshold {
		return nil
	}

	zctx.From(ctx).Info("Idle threshold reached, sending unprompted message",
		zap.Int64("chat_id", chat.ID),
		zap.Duration("idle", time.Since(last.Date)),
		zap.Duration("threshold", threshold),
	)

	return a.sendIdleMessage(ctx, chat, last)
}

// saveSentMessage extracts the message ID and date from a Telegram send update,
// merges them into base, optionally resolves the bot-reply thread when parent
// is non-nil, and persists the result. Errors are logged, not returned.
func (a *App) saveSentMessage(ctx context.Context, update tg.UpdatesClass, base lilith.Message, parent *lilith.Message) {
	lg := zctx.From(ctx)

	save := func(msgID int64, date time.Time) {
		msg := base
		msg.MessageID = msgID
		msg.Date = date

		if parent != nil {
			thread.ResolveBotReply(msg.MessageID, msg.ReplyToID, parent).Apply(&msg)
		}

		if err := a.db.SaveMessage(ctx, msg); err != nil {
			lg.Error("save sent message", zap.Error(err))
		}
	}

	switch v := update.(type) {
	case *tg.UpdateShortSentMessage:
		save(int64(v.ID), time.Unix(int64(v.Date), 0))
	case *tg.Updates:
		for _, upd := range v.Updates {
			if u, ok := upd.(*tg.UpdateMessageID); ok {
				save(int64(u.ID), time.Now())
			}
		}
	default:
		lg.Warn("Unexpected update type from send",
			zap.String("t", fmt.Sprintf("%T", update)),
		)
	}
}
func (a *App) sendIdleMessage(ctx context.Context, chat lilith.Chat, last *lilith.Message) error {
	lg := zctx.From(ctx).With(zap.Int64("chat_id", chat.ID))

	a.chatPeersMu.Lock()
	peer, ok := a.chatPeers[chat.ID]
	a.chatPeersMu.Unlock()

	if !ok {
		lg.Info("No known peer for chat, skipping idle message")
		return nil
	}

	now := time.Now()
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return errors.Wrap(err, "load location")
	}
	now = now.In(loc)
	currentTime := fmt.Sprintf("Текущее время: %s, %s.",
		now.Format(time.RFC822Z),
		russianWeekday(now.Weekday()),
	)

	notes, err := a.memory.Notes(ctx, chat.ID)
	if err != nil {
		return errors.Wrap(err, "get chat notes")
	}

	members, err := a.db.GetChatMembers(ctx, chat.ID)
	if err != nil {
		return errors.Wrap(err, "get chat members")
	}

	lastMessages, err := a.db.GetLastMessages(ctx, chat.ID, chatContextWindowMessages, last.MessageID)
	if err != nil {
		return errors.Wrap(err, "get last messages")
	}

	var selfRank string
	if selfMember, err := a.getChatMember(ctx, chat.ID, a.self.ID); err == nil {
		selfRank = selfMember.Rank
	}

	self := lilith.Self{
		Name:     a.self.FirstName,
		Nickname: a.self.Username,
		Rank:     selfRank,
	}

	var history []lilith.Context

	for _, msg := range lastMessages {
		member, err := a.getChatMember(ctx, msg.ChatID, msg.UserID)
		if err != nil {
			lg.Warn("Failed to get member for history",
				zap.Error(err),
				zap.Int64("user_id", msg.UserID),
			)

			continue
		}

		history = append(history, lilith.Context{
			Message: &msg,
			User:    member,
		})
	}

	req := lilith.ResponseRequest{
		Model:       chat.Model,
		CurrentTime: currentTime,
		Notes:       notes,
		Members:     members,
		Self:        self,
		History:     history,
		Idle:        true,
	}

	result, err := a.ai.Respond(ctx, req)
	if err != nil {
		return errors.Wrap(err, "respond")
	}

	if strings.TrimSpace(result.Text) == "" {
		lg.Warn("Empty idle response from AI")
		return nil
	}

	sender := message.NewSender(a.api)

	update, err := sender.To(peer).Text(ctx, result.Text)
	if err != nil {
		return errors.Wrap(err, "send idle message")
	}

	lg.Info("Sent idle message", zap.String("text", result.Text))

	a.saveSentMessage(ctx, update, lilith.Message{
		ChatID:   chat.ID,
		UserID:   a.self.ID,
		Text:     result.Text,
		IsMyself: true,
	}, nil)

	return nil
}

// maintainNotes folds a message into the chat's long-term notes in the
// background. Used for every recorded message, including those from bots.
func (a *App) maintainNotes(ctx context.Context, chatID int64, msg lilith.Message) {
	go func() {
		if err := a.memory.Maintain(ctx, chatID, msg); err != nil {
			zctx.From(ctx).Error("maintain notes", zap.Error(err))
		}
	}()
}

func (a *App) onMessage(ctx context.Context, e tg.Entities, m *tg.Message, u message.AnswerableMessageUpdate) error {
	ctx, span := a.trace.Start(ctx, "OnNewMessage")
	defer span.End()

	var (
		sender = message.NewSender(a.api)
		reply  = sender.Reply(e, u)
		lg     = zctx.From(ctx).With(zap.Int("msg.id", m.ID))
		answer = sender.Answer(e, u)
		action = answer.TypingAction()
	)

	userID, ok := extractUserID(m)
	if !ok {
		if _, err := reply.Text(ctx, "Invalid"); err != nil {
			return err
		}
		return nil
	}

	user := e.Users[userID]
	if user == nil {
		return nil
	}

	lg.Info("New message",
		zap.String("text", m.Message),
		zap.String("user", user.Username),
		zap.String("first_name", user.FirstName),
		zap.String("last_name", user.LastName),
		zap.Bool("user_is_bot", user.Bot),
		zap.Int64("user_id", user.ID),
	)

	photoURI, err := a.persistPhoto(ctx, m)
	if err != nil {
		lg.Warn("Failed to persist photo", zap.Error(err))
	}

	userMeta := &lilith.UserMetadata{
		IsBot: user.Bot,
	}

	cc, err := a.resolveChatContext(ctx, e, user.ID)
	if err != nil {
		lg.Warn("Failed to resolve chat context", zap.Error(err))
		return nil
	}

	lg.Info("Chat context resolved",
		zap.Int64("chat_id", cc.chatID),
		zap.String("chat_info", cc.chatInfo),
	)

	a.storeChatPeer(cc.chatID, e)

	if err := a.db.UpsertChat(ctx, lilith.Chat{
		ID:         cc.chatID,
		Info:       cc.chatInfo,
		AccessHash: cc.accessHash,
		Type:       cc.chatType,
	}); err != nil {
		return errors.Wrap(err, "upsert chat")
	}

	if err := a.upsertChatMemberCached(ctx, lilith.ChatMember{
		ChatID:    cc.chatID,
		UserID:    user.ID,
		Username:  user.Username,
		FirstName: user.FirstName,
		LastName:  user.LastName,
		IsAdmin:   cc.userIsAdmin,
		IsCreator: cc.userIsCreator,
		Rank:      cc.userRank,
	}); err != nil {
		return errors.Wrap(err, "upsert chat member")
	}

	var (
		replyToID       *int64
		replyToText     *string
		replyToMyself   *bool
		messageThreadID *int64
		parentMsg       *lilith.Message
	)

	if replyHeader, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok {
		topID, hasTop := replyHeader.GetReplyToTopID()

		// Resolve the Telegram forum topic (distinct from the logical thread).
		if replyHeader.ForumTopic {
			if hasTop {
				messageThreadID = lilith.T(int64(topID))
			} else {
				messageThreadID = lilith.T(int64(replyHeader.ReplyToMsgID))
			}
		}

		// In a forum topic without an explicit top id, ReplyToMsgID is the topic
		// root itself, not a genuine reply target.
		topicRootOnly := replyHeader.ForumTopic && !hasTop

		if replyHeader.ReplyToMsgID != 0 && !topicRootOnly {
			id := int64(replyHeader.ReplyToMsgID)

			replyToID = &id

			if replyHeader.QuoteText != "" {
				replyToText = &replyHeader.QuoteText
			}

			msg, err := a.db.GetMessage(ctx, cc.chatID, id)
			if err != nil {
				zctx.From(ctx).Warn("Reply-to message not found in db",
					zap.Int64("chat_id", cc.chatID),
					zap.Int64("reply_to_msg_id", id),
					zap.Error(err),
				)
			} else {
				parentMsg = msg

				if msg.IsMyself {
					replyToMyself = &msg.IsMyself
				}
			}
		}
	}

	// Augment the message text with the link-preview content so the model can
	// see what a shared link is about, not just its URL. Skip it when the body
	// already contains the preview text (e.g. a plain, visible URL).
	text := m.Message
	if preview := a.linkPreviewText(ctx, m); preview != "" && !strings.Contains(text, preview) {
		if text != "" {
			text += "\n\n"
		}
		text += preview
	}

	savedMsg := lilith.Message{
		ChatID:          cc.chatID,
		MessageID:       int64(m.ID),
		UserID:          user.ID,
		Date:            time.Unix(int64(m.Date), 0),
		Text:            text,
		IsMyself:        m.Out,
		ImageURL:        photoURI,
		ReplyToID:       replyToID,
		ReplyToText:     replyToText,
		ReplyToMyself:   replyToMyself,
		MessageThreadID: messageThreadID,
	}

	// Resolve the logical thread for this message.
	var lastAuthor *lilith.Message
	if replyToID == nil {
		la, err := a.db.GetLastMessageByAuthorInTopic(
			ctx, cc.chatID, user.ID, messageThreadID, int64(m.ID), thread.MaxInterveningMessages,
		)
		if err != nil {
			lg.Warn("Failed to look up last author message", zap.Error(err))
		} else {
			lastAuthor = la
		}
	}

	thread.ResolveIncoming(savedMsg, parentMsg, lastAuthor).Apply(&savedMsg)

	if err := a.db.SaveMessage(ctx, savedMsg); err != nil {
		lg.Error("save message", zap.Error(err))
	}

	if m.Out {
		return nil
	}
	if user.Bot {
		// Don't reply to bots, but the message is already persisted above; fold
		// it into the chat notes too so bot activity stays part of the history.
		lg.Info("Not responding to bot message")
		a.maintainNotes(ctx, cc.chatID, savedMsg)
		return nil
	}

	hasAdminRights := cc.userIsAdmin || cc.userIsCreator
	if cc.chatType == lilith.ChatTypePrivate {
		hasAdminRights = true
	}

	switch {
	case m.Message == "/start" || m.Message == "/start@"+a.self.Username:
		if _, err := reply.Text(ctx, "Привет, "+user.FirstName+"!"); err != nil {
			return errors.Wrap(err, "send message")
		}
	case m.Message == "/lobotomy" || m.Message == "/lobotomy@"+a.self.Username:
		if !hasAdminRights {
			if _, err := reply.Text(ctx, "Недостаточно прав."); err != nil {
				return errors.Wrap(err, "send message")
			}

			return nil
		}

		if err := a.db.Lobotomy(ctx, cc.chatID); err != nil {
			lg.Error("lobotomy failed", zap.Error(err))

			if _, err := reply.Text(ctx, "Ошибка при очистке памяти."); err != nil {
				return errors.Wrap(err, "send message")
			}

			return nil
		}

		lg.Info("Lobotomy performed", zap.Int64("chat_id", cc.chatID))

		if _, err := reply.Text(ctx, "Память очищена."); err != nil {
			return errors.Wrap(err, "send message")
		}
	case m.Message == "/model" || m.Message == "/model@"+a.self.Username:
		chat, err := a.db.GetChat(ctx, cc.chatID)
		if err != nil {
			lg.Error("get chat failed", zap.Error(err))

			if _, err := reply.Text(ctx, "Ошибка при получении данных чата."); err != nil {
				return errors.Wrap(err, "send message")
			}

			return nil
		}

		text := "Модель по умолчанию."
		if chat.Model != "" {
			text = "Текущая модель: " + chat.Model
		}

		if _, err := reply.Text(ctx, text); err != nil {
			return errors.Wrap(err, "send message")
		}
	case strings.HasPrefix(m.Message, "/model ") || strings.HasPrefix(m.Message, "/model@"+a.self.Username+" "):
		if !hasAdminRights {
			if _, err := reply.Text(ctx, "Недостаточно прав."); err != nil {
				return errors.Wrap(err, "send message")
			}

			return nil
		}

		arg := strings.TrimPrefix(m.Message, "/model@"+a.self.Username+" ")
		arg = strings.TrimPrefix(arg, "/model ")
		arg = strings.TrimSpace(arg)

		var newModel string
		if arg != "reset" {
			newModel = arg
		}

		if err := a.db.SetChatModel(ctx, cc.chatID, newModel); err != nil {
			lg.Error("set chat model failed", zap.Error(err))

			if _, err := reply.Text(ctx, "Ошибка при установке модели."); err != nil {
				return errors.Wrap(err, "send message")
			}

			return nil
		}

		lg.Info("Chat model updated", zap.Int64("chat_id", cc.chatID), zap.String("model", newModel))

		text := "Модель сброшена до умолчания."
		if newModel != "" {
			text = "Модель установлена: " + newModel
		}

		if _, err := reply.Text(ctx, text); err != nil {
			return errors.Wrap(err, "send message")
		}
	default:
		var shouldResponse bool

		if cc.chatType == lilith.ChatTypePrivate {
			shouldResponse = true
		}

		if replyToMyself != nil && *replyToMyself {
			shouldResponse = true
		}

		for _, name := range []string{
			"лилит",
			"лиля",
			"лилия",
			a.self.Username,
		} {
			if strings.Contains(strings.ToLower(m.Message), name) {
				shouldResponse = true
			}
		}

		if !shouldResponse && rand.Float64() < implicitResponseProbability {
			lg.Info("Random implicit response triggered")
			shouldResponse = true
		}

		a.maintainNotes(ctx, cc.chatID, savedMsg)

		if !shouldResponse {
			lg.Info("Ignoring message")
			return nil
		}

		now := time.Now()
		loc, err := time.LoadLocation("Europe/Moscow")
		if err != nil {
			return errors.Wrap(err, "load location")
		}
		now = now.In(loc)
		currentTime := fmt.Sprintf("Текущее время: %s, %s.",
			now.Format(time.RFC822Z),
			russianWeekday(now.Weekday()),
		)

		notes, err := a.memory.Notes(ctx, cc.chatID)
		if err != nil {
			return errors.Wrap(err, "get chat notes")
		}

		members, err := a.db.GetChatMembers(ctx, cc.chatID)
		if err != nil {
			return errors.Wrap(err, "get chat members")
		}

		lastMessages, err := a.db.GetLastMessages(ctx, cc.chatID, chatContextWindowMessages, int64(m.ID))
		if err != nil {
			return errors.Wrap(err, "get last messages")
		}

		selfMember, err := a.getChatMember(ctx, cc.chatID, a.self.ID)
		if err != nil {
			lg.Warn("Failed to get self chat member", zap.Error(err))
		}

		var selfRank string
		if selfMember != nil {
			selfRank = selfMember.Rank
		}

		self := lilith.Self{
			Name:     a.self.FirstName,
			Nickname: a.self.Username,
			Rank:     selfRank,
		}

		var history []lilith.Context
		candidates := thread.SelectHistoryCandidates(lastMessages, int64(m.ID), chatContextWindowMessages)
		for _, msg := range candidates {
			if msg.MessageID == savedMsg.MessageID {
				continue
			}

			member, err := a.getChatMember(ctx, msg.ChatID, msg.UserID)
			if err != nil {
				// Author isn't a known member (e.g. a bot or a user who left).
				// Keep the message in context with a minimal member rather than
				// dropping it from history entirely.
				lg.Warn("Member not found for history message, using fallback",
					zap.Error(err),
					zap.Int64("user_id", msg.UserID),
				)
				member = &lilith.ChatMember{ChatID: msg.ChatID, UserID: msg.UserID}
			}

			dialogContext := lilith.Context{
				Message: &msg,
				User:    member,
			}

			if member.UserID == user.ID {
				dialogContext.UserMetadata = userMeta
			}

			history = append(history, dialogContext)
		}

		currentMember, err := a.getChatMember(ctx, savedMsg.ChatID, savedMsg.UserID)
		if err != nil {
			lg.Warn("Failed to get current member, using user entity as fallback", zap.Error(err))
			currentMember = &lilith.ChatMember{
				ChatID:    cc.chatID,
				UserID:    user.ID,
				Username:  user.Username,
				FirstName: user.FirstName,
				LastName:  user.LastName,
			}
		}

		chat, err := a.db.GetChat(ctx, cc.chatID)
		if err != nil {
			return errors.Wrap(err, "get chat")
		}

		req := lilith.ResponseRequest{
			Model:       chat.Model,
			CurrentTime: currentTime,
			Notes:       notes,
			Members:     members,
			Self:        self,
			History:     history,
			Current: lilith.Context{
				Message:      &savedMsg,
				User:         currentMember,
				UserMetadata: userMeta,
			},
			ImageURL: photoURI,
			Typing: func(ctx context.Context) error {
				return action.Typing(ctx)
			},
		}

		result, err := a.ai.Respond(ctx, req)
		if err != nil {
			return errors.Wrap(err, "respond")
		}

		for _, r := range result.Reactions {
			lg.Info("Setting reaction to message")
			if _, err := answer.Reaction(ctx, m.ID, &tg.ReactionEmoji{Emoticon: r}); err != nil {
				lg.Warn("Failed to set reaction", zap.Error(err))
			}
		}

		if strings.TrimSpace(result.Text) == "" {
			lg.Warn("Empty response from AI")
			return nil
		}

		// In one-to-one chats there is no need to thread responses as replies;
		// send directly to the chat instead.
		sentMsg := lilith.Message{
			ChatID:          cc.chatID,
			UserID:          a.self.ID,
			Text:            result.Text,
			ReplyToID:       lilith.T(int64(m.ID)),
			IsMyself:        true,
			MessageThreadID: savedMsg.MessageThreadID,
		}

		send := reply.Text
		if cc.chatType == lilith.ChatTypePrivate {
			send = answer.Text
			sentMsg.ReplyToID = nil
		}

		sentUpdate, err := send(ctx, result.Text)
		if err != nil {
			lg.Warn("Failed to send response", zap.Error(err))
			return errors.Wrap(err, "send response")
		}

		a.saveSentMessage(ctx, sentUpdate, sentMsg, &savedMsg)
	}

	return nil
}

func (a *App) fetchChannelParticipants(ctx context.Context, channel *tg.Channel) error {
	const limit = 200

	inputChannel := &tg.InputChannel{
		ChannelID:  channel.ID,
		AccessHash: channel.AccessHash,
	}

	if err := a.db.UpsertChat(ctx, lilith.Chat{
		ID:         channel.ID,
		Info:       channel.Title,
		AccessHash: channel.AccessHash,
		Type:       lilith.ChatTypeChannel,
	}); err != nil {
		return errors.Wrap(err, "upsert chat")
	}

	lg := zctx.From(ctx).With(zap.Int64("channel_id", channel.ID))

	for offset := 0; ; {
		result, err := a.api.ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
			Channel: inputChannel,
			Filter:  &tg.ChannelParticipantsRecent{},
			Offset:  offset,
			Limit:   limit,
		})
		if err != nil {
			return errors.Wrap(err, "get participants")
		}

		participants, ok := result.(*tg.ChannelsChannelParticipants)
		if !ok {
			// Not modified or empty.
			return nil
		}

		users := make(map[int64]*tg.User, len(participants.Users))
		for _, u := range participants.Users {
			if user, ok := u.(*tg.User); ok {
				users[user.ID] = user
			}
		}

		for _, p := range participants.Participants {
			var (
				userID    int64
				isAdmin   bool
				isCreator bool
				rank      string
			)

			switch v := p.(type) {
			case *tg.ChannelParticipant:
				userID = v.UserID
			case *tg.ChannelParticipantSelf:
				userID = v.UserID
			case *tg.ChannelParticipantCreator:
				userID = v.UserID
				isCreator = true
				rank = v.Rank
			case *tg.ChannelParticipantAdmin:
				userID = v.UserID
				isAdmin = true
				rank = v.Rank
			default:
				// Banned or left — skip.
				continue
			}

			user, ok := users[userID]
			if !ok {
				continue
			}

			if err := a.upsertChatMemberCached(ctx, lilith.ChatMember{
				ChatID:    channel.ID,
				UserID:    userID,
				Username:  user.Username,
				FirstName: user.FirstName,
				LastName:  user.LastName,
				IsAdmin:   isAdmin,
				IsCreator: isCreator,
				Rank:      rank,
			}); err != nil {
				return errors.Wrap(err, "upsert chat member")
			}
		}

		lg.Info("Fetched channel participants",
			zap.Int("offset", offset),
			zap.Int("count", len(participants.Participants)),
			zap.Int("total", participants.Count),
		)

		offset += len(participants.Participants)

		if len(participants.Participants) < limit {
			break
		}
	}

	return nil
}

func (a *App) onNewChannelMessage(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
	m, ok := u.Message.(*tg.Message)
	if !ok {
		return nil
	}

	return a.onMessage(ctx, e, m, u)
}

func (a *App) onNewMessage(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
	m, ok := u.Message.(*tg.Message)
	if !ok {
		return nil
	}

	return a.onMessage(ctx, e, m, u)
}
