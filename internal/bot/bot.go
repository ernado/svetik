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
	api    *tg.Client
	client *telegram.Client
	db     lilith.DB
	ai     lilith.AI
	memory lilith.Memory
	files  lilith.FileStore
	self   *tg.User

	waiter *floodwait.Waiter
	trace  trace.Tracer

	// channelParticipantsMu guards channelParticipantsFetchedAt.
	channelParticipantsMu        sync.Mutex
	channelParticipantsFetchedAt map[int64]time.Time

	// chatMemberMu guards chatMemberCache.
	chatMemberMu    sync.Mutex
	chatMemberCache map[chatMemberKey]*cachedChatMember
}

// New constructs an App. files may be nil to disable media handling.
func New(
	client *telegram.Client,
	db lilith.DB,
	ai lilith.AI,
	mem lilith.Memory,
	files lilith.FileStore,
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
		waiter:                       waiter,
		trace:                        tracer,
		channelParticipantsFetchedAt: make(map[int64]time.Time),
		chatMemberCache:              make(map[chatMemberKey]*cachedChatMember),
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
				},
			}); err != nil {
				return errors.Wrap(err, "set commands")
			}
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
	selfRank      string
	userIsAdmin   bool
	userIsCreator bool
	userRank      string
}

func (a *App) resolveRegularChat(ctx context.Context, chatID int64, userID int64) (*chatContext, error) {
	full, err := a.api.MessagesGetFullChat(ctx, chatID)
	if err != nil {
		return nil, errors.Wrap(err, "get full chat")
	}

	chatFull, ok := full.FullChat.(*tg.ChatFull)
	if !ok {
		return nil, errors.New("unexpected full chat type")
	}

	cc := &chatContext{
		chatID:   chatFull.ID,
		chatInfo: chatFull.About,
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
		chatID:   channel.ID,
		chatInfo: channel.Title,
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
		return a.resolveRegularChat(ctx, chat.ID, userID)
	}

	for _, channel := range e.Channels {
		return a.resolveChannel(ctx, channel, userID)
	}

	return nil, errors.New("no chat or channel in entities")
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

func (a *App) persistPhoto(ctx context.Context, m *tg.Message) (string, error) {
	if a.files == nil {
		return "", nil
	}

	dl := downloader.NewDownloader()

	// Checking for media.
	switch media := m.Media.(type) {
	case *tg.MessageMediaPhoto:
		p, ok := media.Photo.AsNotEmpty()
		if !ok {
			return "", nil
		}
		out := new(bytes.Buffer)
		if _, err := dl.Download(a.api, &tg.InputPhotoFileLocation{
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
	default:
		return "", nil
	}
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

	if err := a.db.UpsertChat(ctx, lilith.Chat{
		ID:   cc.chatID,
		Info: cc.chatInfo,
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
		replyToID     *int64
		replyToText   *string
		replyToMyself *bool
	)

	if replyHeader, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok {
		id := int64(replyHeader.ReplyToMsgID)

		replyToID = &id

		if replyHeader.QuoteText != "" {
			replyToText = &replyHeader.QuoteText
		}

		msg, err := a.db.GetMessage(ctx, cc.chatID, int64(replyHeader.ReplyToMsgID))
		if err != nil {
			zctx.From(ctx).Warn("Reply-to message not found in db",
				zap.Int64("chat_id", cc.chatID),
				zap.Int("reply_to_msg_id", replyHeader.ReplyToMsgID),
				zap.Error(err),
			)
		} else if msg.IsMyself {
			replyToMyself = &msg.IsMyself
		}
	}

	savedMsg := lilith.Message{
		ChatID:        cc.chatID,
		MessageID:     int64(m.ID),
		UserID:        user.ID,
		Date:          time.Unix(int64(m.Date), 0),
		Text:          m.Message,
		IsMyself:      m.Out,
		ReplyToID:     replyToID,
		ReplyToText:   replyToText,
		ReplyToMyself: replyToMyself,
	}

	if err := a.db.SaveMessage(ctx, savedMsg); err != nil {
		lg.Error("save message", zap.Error(err))
	}

	if m.Out {
		return nil
	}
	if user.Bot {
		lg.Info("Ignoring bot message")
		return nil
	}

	switch m.Message {
	case "/start", "/start@" + a.self.Username:
		if _, err := reply.Text(ctx, "Привет, "+user.FirstName+"!"); err != nil {
			return errors.Wrap(err, "send message")
		}
	case "/lobotomy", "/lobotomy@" + a.self.Username:
		if !cc.userIsAdmin && !cc.userIsCreator {
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
	default:
		var shouldResponse bool

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

		go func() {
			if err := a.memory.Maintain(ctx, cc.chatID, int64(m.ID), savedMsg); err != nil {
				lg.Error("maintain notes", zap.Error(err))
			}
		}()

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
			return errors.Wrap(err, "get chat member")
		}
		self := lilith.Self{
			Name:     a.self.FirstName,
			Nickname: a.self.Username,
			Rank:     selfMember.Rank,
		}

		var history []lilith.Context
		for _, msg := range lastMessages {
			if msg.MessageID == savedMsg.MessageID {
				continue
			}

			member, err := a.getChatMember(ctx, msg.ChatID, msg.UserID)
			if err != nil {
				return errors.Wrap(err, "get member")
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
			return errors.Wrap(err, "get member")
		}

		req := lilith.ResponseRequest{
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

		replyUpdate, err := reply.Text(ctx, result.Text)
		if err != nil {
			lg.Warn("Failed to send reply", zap.Error(err))
			return errors.Wrap(err, "send reply")
		}

		switch v := replyUpdate.(type) {
		case *tg.UpdateShortSentMessage:
			if err := a.db.SaveMessage(ctx, lilith.Message{
				ChatID:    cc.chatID,
				MessageID: int64(v.ID),
				UserID:    a.self.ID,
				Date:      time.Unix(int64(v.Date), 0),
				Text:      result.Text,
				ReplyToID: lilith.T(int64(m.ID)),
				IsMyself:  true,
			}); err != nil {
				lg.Error("save sent message", zap.Error(err))
			}
		case *tg.Updates:
			for _, update := range v.Updates {
				switch upd := update.(type) {
				case *tg.UpdateMessageID:
					if err := a.db.SaveMessage(ctx, lilith.Message{
						ChatID:    cc.chatID,
						MessageID: int64(upd.ID),
						UserID:    a.self.ID,
						Date:      time.Now(),
						Text:      result.Text,
						ReplyToID: lilith.T(int64(m.ID)),
						IsMyself:  true,
					}); err != nil {
						lg.Error("save sent message", zap.Error(err))
					}
				}
			}
		default:
			lg.Warn("Unexpected replyUpdate type",
				zap.String("t", fmt.Sprintf("%T", replyUpdate)),
			)
		}
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
		ID:   channel.ID,
		Info: channel.Title,
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
