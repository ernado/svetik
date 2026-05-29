package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ernado/svetik"
	"github.com/ernado/svetik/internal/reaction"
	"github.com/ernado/svetik/internal/weather"
	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/app"
	"github.com/go-faster/sdk/zctx"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
	"github.com/revrost/go-openrouter"
	"github.com/revrost/go-openrouter/jsonschema"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/singleflight"

	"github.com/ernado/svetik/internal/db"
	"github.com/ernado/svetik/internal/prompt"
)

const (
	// channelParticipantsTTL is the minimum interval between participant list refreshes.
	channelParticipantsTTL = 10 * time.Minute

	// chatMemberTTL is the TTL for application-level chat member cache entries.
	chatMemberTTL = 5 * time.Minute

	// chatContextWindowMessages is total messages passed to model context.
	chatContextWindowMessages = 150

	// maxTokens is the max_tokens parameter for the AI response, controlling its length.
	maxTokens = 450

	// maxNotesTokens is the max_tokens parameter for notes generation.
	maxNotesTokens = 1024

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

type Application struct {
	api     *tg.Client
	client  *telegram.Client
	ai      *openrouter.Client
	db      lilith.DB
	self    *tg.User
	model   string
	weather *weather.Client

	waiter *floodwait.Waiter
	trace  trace.Tracer

	// channelParticipantsMu guards channelParticipantsFetchedAt.
	channelParticipantsMu        sync.Mutex
	channelParticipantsFetchedAt map[int64]time.Time

	// chatMemberMu guards chatMemberCache.
	chatMemberMu    sync.Mutex
	chatMemberCache map[chatMemberKey]*cachedChatMember

	// notesSFG deduplicates concurrent note-generation calls for the same chat.
	notesSFG singleflight.Group
}

func getEmojiTool() openrouter.Tool {
	toolParams := jsonschema.Definition{
		Type: jsonschema.Object,
		Properties: map[string]jsonschema.Definition{
			"emoji": {
				Type:        jsonschema.String,
				Description: "Emoji to reply",
			},
		},
		Required: []string{"emoji"},
	}
	functionDefinition := openrouter.FunctionDefinition{
		Name:        "reply_emoji",
		Description: "Repl to message with emoji. Allowed reactions:" + strings.Join(reaction.Allowed, ""),
		Parameters:  toolParams,
	}
	t := openrouter.Tool{
		Type:     openrouter.ToolTypeFunction,
		Function: &functionDefinition,
	}
	return t
}

func getWeatherTool() openrouter.Tool {
	toolParams := jsonschema.Definition{
		Type: jsonschema.Object,
		Properties: map[string]jsonschema.Definition{
			"city": {
				Type:        jsonschema.String,
				Description: "City name, Moscow",
			},
			"country_code": {
				Type:        jsonschema.String,
				Description: "Country code, RU",
			},
		},
	}
	functionDefinition := openrouter.FunctionDefinition{
		Name:        "get_weather",
		Description: "Get weather",
		Parameters:  toolParams,
	}
	t := openrouter.Tool{
		Type:     openrouter.ToolTypeFunction,
		Function: &functionDefinition,
	}
	return t
}

func (a *Application) Run(ctx context.Context) error {
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

func (a *Application) addChannel(ctx context.Context, channel *tg.Channel) error {
	zctx.From(ctx).Info("Channel added",
		zap.Int64("id", channel.ID),
		zap.String("title", channel.Title),
	)
	return nil
}

func (a *Application) removeChannel(ctx context.Context, channel *tg.Channel) error {
	zctx.From(ctx).Info("Channel removed",
		zap.Int64("id", channel.ID),
		zap.String("title", channel.Title),
	)
	return nil
}

func (a *Application) onChannelParticipant(ctx context.Context, e tg.Entities, update *tg.UpdateChannelParticipant) error {
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

func (a *Application) resolveRegularChat(ctx context.Context, chatID int64, userID int64) (*chatContext, error) {
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

func (a *Application) resolveChannel(ctx context.Context, channel *tg.Channel, userID int64) (*chatContext, error) {
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
func (a *Application) fetchChannelParticipantsCached(ctx context.Context, channel *tg.Channel) error {
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
func (a *Application) getChatMember(ctx context.Context, chatID, userID int64) (*lilith.ChatMember, error) {
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
func (a *Application) upsertChatMemberCached(ctx context.Context, m lilith.ChatMember) error {
	if err := a.db.UpsertChatMember(ctx, m); err != nil {
		return err
	}

	key := chatMemberKey{chatID: m.ChatID, userID: m.UserID}

	a.chatMemberMu.Lock()
	a.chatMemberCache[key] = &cachedChatMember{member: &m, fetchedAt: time.Now()}
	a.chatMemberMu.Unlock()

	return nil
}

func (a *Application) resolveChatContext(ctx context.Context, e tg.Entities, userID int64) (*chatContext, error) {
	for _, chat := range e.Chats {
		return a.resolveRegularChat(ctx, chat.ID, userID)
	}

	for _, channel := range e.Channels {
		return a.resolveChannel(ctx, channel, userID)
	}

	return nil, errors.New("no chat or channel in entities")
}

// isNotesNeeded returns true when at least chatContextWindowMessages messages
// have been recorded in the chat since the last notes snapshot.
func (a *Application) isNotesNeeded(ctx context.Context, chatID, currentMsgID int64) (bool, error) {
	chat, err := a.db.GetChat(ctx, chatID)
	if err != nil {
		return false, errors.Wrap(err, "get chat")
	}

	count, err := a.db.CountMessagesSince(ctx, chatID, chat.LastNotesMsgID, currentMsgID)
	if err != nil {
		return false, errors.Wrap(err, "count messages since")
	}

	zctx.From(ctx).Info("isNotesNeeded",
		zap.Int64("chatID", chat.ID),
		zap.Int64("currentMsgID", currentMsgID),
		zap.Int64("count", count),
	)

	return count >= chatContextWindowMessages, nil
}

// generateNotes generates and persists a notes snapshot for the given chat at
// currentMsgID. Concurrent calls for the same chat are coalesced via singleflight:
// only one AI request is made and all waiters receive the same result.
func (a *Application) generateNotes(ctx context.Context, chatID, currentMsgID int64) error {
	key := strconv.FormatInt(chatID, 10)

	_, err, _ := a.notesSFG.Do(key, func() (any, error) {
		return nil, a.doGenerateNotes(ctx, chatID, currentMsgID)
	})

	return err
}

// doGenerateNotes is the actual (non-deduplicated) note generation logic.
func (a *Application) doGenerateNotes(ctx context.Context, chatID, currentMsgID int64) error {
	lg := zctx.From(ctx).With(zap.Int64("chat_id", chatID))
	lg.Info("Generating notes snapshot")

	lastMessages, err := a.db.GetLastMessages(ctx, chatID, chatContextWindowMessages, currentMsgID)
	if err != nil {
		return errors.Wrap(err, "get last messages")
	}

	existingNotes, err := a.db.GetChatNotes(ctx, chatID)
	if err != nil {
		return errors.Wrap(err, "get chat notes")
	}

	var notesLines []string
	for _, n := range existingNotes {
		notesLines = append(notesLines, n.Text)
	}

	dialog := []openrouter.ChatCompletionMessage{
		openrouter.SystemMessage(strings.Join([]string{
			prompt.Protocol,
			prompt.Notes,
		}, "\n")),
	}

	if len(notesLines) > 0 {
		dialog = append(dialog, openrouter.UserMessage(
			"Существующие заметки:\n"+strings.Join(notesLines, "\n"),
		))
	}

	for _, msg := range lastMessages {
		data, err := json.Marshal(msg)
		if err != nil {
			return errors.Wrap(err, "marshal message")
		}

		dialog = append(dialog, openrouter.UserMessage(string(data)))
	}

	dialog = append(dialog, openrouter.UserMessage("Сгенерируй заметки"))

	resp, err := a.ai.CreateChatCompletion(ctx, openrouter.ChatCompletionRequest{
		Model:     a.model,
		Messages:  dialog,
		MaxTokens: maxNotesTokens,
	})
	if err != nil {
		return errors.Wrap(err, "generate notes")
	}

	text := strings.TrimSpace(resp.Choices[0].Message.Content.Text)

	if text == "" {
		lg.Info("No new notes generated")
		return nil
	}

	if _, err := a.db.AddChatNote(ctx, chatID, text); err != nil {
		return errors.Wrap(err, "add chat note")
	}

	if _, err := a.db.SetLastNotesMsgID(ctx, chatID, currentMsgID); err != nil {
		return errors.Wrap(err, "set last notes msg id")
	}

	lg.Info("Notes generated",
		zap.Int64("msg_id", currentMsgID),
		zap.String("text", text),
	)

	return nil
}

// generateNoteForMessage decides whether a single message is worth noting and,
// if so, persists the note. The call is blocking and serialised per chat via
// the shared notesSFG singleflight group.
func (a *Application) generateNoteForMessage(ctx context.Context, chatID int64, msg lilith.Message) error {
	key := "msg:" + strconv.FormatInt(msg.MessageID, 10)

	_, err, _ := a.notesSFG.Do(key, func() (any, error) {
		return nil, a.doGenerateNoteForMessage(ctx, chatID, msg)
	})

	return err
}

// doGenerateNoteForMessage is the actual (non-deduplicated) single-message note logic.
func (a *Application) doGenerateNoteForMessage(ctx context.Context, chatID int64, msg lilith.Message) error {
	lg := zctx.From(ctx).With(
		zap.Int64("chat_id", chatID),
		zap.Int64("msg_id", msg.MessageID),
	)

	lg.Info("doGenerateNoteForMessage")

	existingNotes, err := a.db.GetChatNotes(ctx, chatID)
	if err != nil {
		return errors.Wrap(err, "get chat notes")
	}

	dialog := []openrouter.ChatCompletionMessage{
		openrouter.SystemMessage(prompt.NoteSingle),
	}

	if len(existingNotes) > 0 {
		var noteLines []string

		for _, n := range existingNotes {
			noteLines = append(noteLines, n.Text)
		}

		dialog = append(dialog, openrouter.SystemMessage(
			"Существующие заметки:\n"+strings.Join(noteLines, "\n"),
		))
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return errors.Wrap(err, "marshal message")
	}

	dialog = append(dialog, openrouter.UserMessage(string(data)))

	resp, err := a.ai.CreateChatCompletion(ctx, openrouter.ChatCompletionRequest{
		Model:     a.model,
		Messages:  dialog,
		MaxTokens: maxNotesTokens,
	})
	if err != nil {
		return errors.Wrap(err, "generate note for message")
	}

	text := strings.TrimSpace(resp.Choices[0].Message.Content.Text)

	if text == "" || text == "Empty line." || len(text) < 40 {
		lg.Info("No note needed for message")
		return nil
	}

	if _, err := a.db.AddChatNote(ctx, chatID, text); err != nil {
		return errors.Wrap(err, "add chat note")
	}

	lg.Info("Note generated for message")

	return nil
}

// completeWithTools runs the AI completion loop, handling tool calls (e.g. emoji
// reactions) until the model produces a text reply or the iteration limit is hit.
// It returns the final message text (may be empty if the model produced no text).
func (a *Application) completeWithTools(
	ctx context.Context,
	lg *zap.Logger,
	dialog []openrouter.ChatCompletionMessage,
	action *message.TypingActionBuilder,
	answer *message.RequestBuilder,
	msgID int,
) (string, error) {
	const maxIterations = 4

	tools := []openrouter.Tool{
		getEmojiTool(),
		getWeatherTool(),
	}

	for i := range maxIterations {
		if i > 0 {
			lg.Info("Retrying after tool call", zap.Int("iteration", i))
		}

		done := make(chan struct{})
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					if err := action.Typing(ctx); err != nil {
						lg.Error("Failed to send typing action", zap.Error(err))
						return
					}
				}
			}
		}()

		resp, err := a.ai.CreateChatCompletion(ctx, openrouter.ChatCompletionRequest{
			Model:     a.model,
			Messages:  dialog,
			MaxTokens: maxTokens,
			Tools:     tools,
		})
		close(done)

		if err != nil {
			lg.Warn("Failed to create completion", zap.Error(err))
			return "", errors.Wrap(err, "generate content")
		}

		msg := resp.Choices[0].Message

		for _, tool := range msg.ToolCalls {
			lg.Info("Function call",
				zap.String("id", tool.ID),
			)
			switch tool.Function.Name {
			case "reply_emoji":
				var args struct {
					Emoji string `json:"emoji"`
				}

				toolContent, err := json.Marshal(struct {
					Emoji string `json:"reply_emoji"`
					MsgID int    `json:"msg_id"`
				}{
					Emoji: args.Emoji,
					MsgID: msgID,
				})
				if err != nil {
					return "", errors.Wrap(err, "marshal emoji")
				}
				assistantContent, err := json.Marshal(tool)
				if err != nil {
					return "", errors.Wrap(err, "marshal tool")
				}

				dialog = append(dialog,
					openrouter.ChatCompletionMessage{
						Role: openrouter.ChatMessageRoleAssistant,
						Content: openrouter.Content{
							Text: string(assistantContent),
						},
					},
					openrouter.ChatCompletionMessage{
						Role: openrouter.ChatMessageRoleTool,
						Content: openrouter.Content{
							Text: string(toolContent),
						},
						ToolCallID: tool.ID,
					},
				)

				if err := json.Unmarshal([]byte(tool.Function.Arguments), &args); err != nil {
					return "", errors.Wrap(err, "unmarshal arguments")
				}
				lg.Info("Setting reaction to message")
				if text, ok := reaction.Canonicalize(args.Emoji); ok {
					if _, err := answer.Reaction(ctx, msgID,
						&tg.ReactionEmoji{Emoticon: text},
					); err != nil {
						lg.Warn("Failed to set reaction", zap.Error(err))
					}
				}

			case "get_weather":
				var args struct {
					City        string `json:"city"`
					CountryCode string `json:"country_code"`
				}

				if err := json.Unmarshal([]byte(tool.Function.Arguments), &args); err != nil {
					return "", errors.Wrap(err, "unmarshal arguments")
				}

				info, err := a.weather.GetCurrentByName(ctx, args.City, args.CountryCode)
				if err != nil {
					return "", errors.Wrap(err, "get weather")
				}

				desc := args.City
				if len(info.Current.WeatherDescriptions) > 0 {
					desc = info.Current.WeatherDescriptions[0]
				}

				weatherInfo := fmt.Sprintf(
					"Погода в %s (%s): %s, %d °C, ощущается как %d °C, влажность %d%%, ветер %d м/с %s",
					info.Location.Name,
					info.Location.Country,
					desc,
					info.Current.Temperature,
					info.Current.FeelsLike,
					info.Current.Humidity,
					info.Current.WindSpeed,
					info.Current.WindDir,
				)

				lg.Info("Adding weather info to dialog", zap.String("weather_info", weatherInfo))

				dialog = append(dialog,
					openrouter.ChatCompletionMessage{
						Role: openrouter.ChatMessageRoleTool,
						Content: openrouter.Content{
							Text: weatherInfo,
						},
						ToolCallID: tool.ID,
					},
				)
			default:
				lg.Warn("Unknown function call", zap.String("name", tool.Function.Name))
			}
		}

		// Only loop again when the model called a tool but produced no text yet.
		if len(msg.ToolCalls) > 0 {
			continue
		}

		return msg.Content.Text, nil
	}

	lg.Error("Too many tool-call iterations")

	return "", nil
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

func (a *Application) onMessage(ctx context.Context, e tg.Entities, m *tg.Message, u message.AnswerableMessageUpdate) error {
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

		notesNeeded, err := a.isNotesNeeded(ctx, cc.chatID, int64(m.ID))
		if err != nil {
			return errors.Wrap(err, "isNotesNeeded")
		}

		if notesNeeded {
			go func() {
				lg.Info("Notes needed")
				if err := a.generateNotes(ctx, cc.chatID, int64(m.ID)); err != nil {
					lg.Error("generate notes", zap.Error(err))
				}
			}()
		} else {
			go func() {
				lg.Info("Notes not needed")
				if err := a.generateNoteForMessage(ctx, cc.chatID, savedMsg); err != nil {
					lg.Error("generate note for message", zap.Error(err))
				}
			}()
		}

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

		dialog := []openrouter.ChatCompletionMessage{
			openrouter.SystemMessage(strings.Join([]string{
				prompt.Protocol, prompt.Character, currentTime,
			}, "\n")),
		}

		notes, err := a.db.GetChatNotes(ctx, cc.chatID)
		if err != nil {
			return errors.Wrap(err, "get chat notes")
		}

		if len(notes) > 0 {
			var noteLines []string

			for _, n := range notes {
				noteLines = append(noteLines, n.Text)
			}

			dialog = append(dialog, openrouter.SystemMessage(
				"Заметки о чате:\n"+strings.Join(noteLines, "\n"),
			))
		}

		members, err := a.db.GetChatMembers(ctx, cc.chatID)
		if err != nil {
			return errors.Wrap(err, "get chat members")
		}

		if len(members) > 0 {
			membersData, err := json.Marshal(members)
			if err != nil {
				return errors.Wrap(err, "marshal members")
			}

			dialog = append(dialog, openrouter.SystemMessage(
				"Участники чата:\n"+string(membersData),
			))
		}

		lastMessages, err := a.db.GetLastMessages(ctx, cc.chatID, chatContextWindowMessages, int64(m.ID))
		if err != nil {
			return errors.Wrap(err, "get last messages")
		}

		{
			// Add self reflection.
			member, err := a.getChatMember(ctx, cc.chatID, a.self.ID)
			if err != nil {
				return errors.Wrap(err, "get chat member")
			}
			self := lilith.Self{
				Name:     a.self.FirstName,
				Nickname: a.self.Username,
				Rank:     member.Rank,
			}
			selfData, err := json.Marshal(&self)
			if err != nil {
				return errors.Wrap(err, "marshal self")
			}
			dialog = append(dialog,
				openrouter.SystemMessage("Информация о себе:"),
				openrouter.SystemMessage(string(selfData)),
			)
		}

		dialog = append(dialog, openrouter.UserMessage("Предыдущая переписка:"))

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

			data, err := json.Marshal(dialogContext)
			if err != nil {
				return errors.Wrap(err, "marshal dialog context")
			}

			dialog = append(dialog, openrouter.UserMessage(string(data)))
		}

		{
			// Append current message.
			member, err := a.getChatMember(ctx, savedMsg.ChatID, savedMsg.UserID)
			if err != nil {
				return errors.Wrap(err, "get member")
			}

			dialogContext := lilith.Context{
				Message:      &savedMsg,
				User:         member,
				UserMetadata: userMeta,
			}

			data, err := json.Marshal(dialogContext)
			if err != nil {
				return errors.Wrap(err, "marshal dialog context")
			}

			dialog = append(dialog,
				openrouter.UserMessage("Текущее сообщение:"),
				openrouter.UserMessage(string(data)),
			)
		}

		messageText, err := a.completeWithTools(ctx, lg, dialog, action, answer, m.ID)
		if err != nil {
			return errors.Wrap(err, "complete with tools")
		}

		if strings.TrimSpace(messageText) == "" {
			lg.Warn("Empty response from AI")
			return nil
		}

		replyUpdate, err := reply.Text(ctx, messageText)
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
				Text:      messageText,
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
						Text:      messageText,
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

func (a *Application) fetchChannelParticipants(ctx context.Context, channel *tg.Channel) error {
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

func (a *Application) onNewChannelMessage(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
	m, ok := u.Message.(*tg.Message)
	if !ok {
		return nil
	}

	return a.onMessage(ctx, e, m, u)
}

func (a *Application) onNewMessage(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
	m, ok := u.Message.(*tg.Message)
	if !ok {
		return nil
	}

	return a.onMessage(ctx, e, m, u)
}

func newJSONSessionStorage(filePath string) (*jsonSessionStorage, error) {
	return &jsonSessionStorage{filePath: filePath}, nil
}

type jsonSessionStorage struct {
	filePath string
}

func (j *jsonSessionStorage) LoadSession(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(j.filePath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

func (j *jsonSessionStorage) StoreSession(_ context.Context, data []byte) error {
	return os.WriteFile(j.filePath, data, 0600)
}

var _ telegram.SessionStorage = (*jsonSessionStorage)(nil)

func Root() *cobra.Command {
	var forceMigration bool
	cmd := &cobra.Command{
		Use: "svetik",
		RunE: func(cmd *cobra.Command, args []string) error {
			databaseURI := "postgres://postgres:postgres@localhost:5432/svetik?sslmode=disable"

			{
				// Database migrations.
				d, err := iofs.New(db.Migrations, "_migrations")
				if err != nil {
					return errors.Wrap(err, "create iofs driver")
				}

				uri := strings.ReplaceAll(databaseURI, "postgres://", "pgx5://")
				m, err := migrate.NewWithSourceInstance("iofs", d, uri)
				if err != nil {
					return errors.Wrap(err, "create migrate")
				}

				if forceMigration {
					// Only migrate and return.
					version, dirty, err := m.Version()
					if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
						return errors.Wrap(err, "get version")
					}

					if dirty {
						if err := m.Force(int(version)); err != nil {
							return errors.Wrap(err, "force version")
						}

						fmt.Printf("Forced dirty migration to version %d\n", version)
					} else {
						fmt.Printf("Nothing to do anyway\n")
					}

					return nil
				}

				if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
					return errors.Wrap(err, "migrate up")
				} else {
					if errors.Is(err, migrate.ErrNoChange) {
						fmt.Println("No migrations to apply")
					} else {
						fmt.Println("Migrations applied successfully")
					}
				}

				sourceErr, dbErr := m.Close()
				if sourceErr != nil {
					return errors.Wrap(sourceErr, "close source")
				}
				if dbErr != nil {
					return errors.Wrap(dbErr, "close db")
				}
			}

			weatherAPIKey := os.Getenv("WEATHER_API_KEY")
			if weatherAPIKey == "" {
				return errors.New("WEATHER_API_KEY environment variable not set")
			}
			weatherClient := weather.New(weatherAPIKey, weather.Options{})

			app.Run(func(ctx context.Context, lg *zap.Logger, t *app.Telemetry) error {
				botToken := os.Getenv("BOT_TOKEN")
				if botToken == "" {
					return errors.New("BOT_TOKEN is empty")
				}
				appID, err := strconv.Atoi(os.Getenv("APP_ID"))
				if err != nil {
					return errors.Wrap(err, "parse APP_ID")
				}
				appHash := os.Getenv("APP_HASH")
				if appHash == "" {
					return errors.New("APP_HASH is empty")
				}
				waiter := floodwait.NewWaiter()
				dispatcher := tg.NewUpdateDispatcher()
				sessionStorage, err := newJSONSessionStorage("session.json")
				if err != nil {
					return errors.Wrap(err, "create session storage")
				}
				client := telegram.NewClient(appID, appHash, telegram.Options{
					Logger:         zctx.From(ctx).Named("tg"),
					TracerProvider: t.TracerProvider(),
					SessionStorage: sessionStorage,
					UpdateHandler:  dispatcher,
					Middlewares: []telegram.Middleware{
						waiter,
					},
				})
				ai := openrouter.NewClient(os.Getenv("AI_TOKEN"))
				aiModel := os.Getenv("AI_MODEL")
				if aiModel == "" {
					aiModel = "deepseek/deepseek-v4-flash"
				}
				databaseConnection, err := db.Open(ctx, databaseURI, t)
				if err != nil {
					return errors.Wrap(err, "open database")
				}
				if err := databaseConnection.Ping(ctx); err != nil {
					return errors.Wrap(err, "ping database")
				}

				a := &Application{
					api:                          tg.NewClient(client),
					ai:                           ai,
					weather:                      weatherClient,
					model:                        aiModel,
					db:                           db.New(databaseConnection),
					client:                       client,
					waiter:                       waiter,
					trace:                        t.TracerProvider().Tracer("svetik.bot"),
					channelParticipantsFetchedAt: make(map[int64]time.Time),
					chatMemberCache:              make(map[chatMemberKey]*cachedChatMember),
				}
				dispatcher.OnChannelParticipant(a.onChannelParticipant)
				dispatcher.OnNewMessage(a.onNewMessage)
				dispatcher.OnNewChannelMessage(a.onNewChannelMessage)
				return a.Run(ctx)
			},
				app.WithZapConfig(func() zap.Config {
					cfg := zap.NewProductionConfig()
					cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
					return cfg
				}()),
			)

			return nil
		},
	}

	f := cmd.Flags()
	f.BoolVarP(&forceMigration, "force-migration", "f", false, "force migration")

	return cmd
}

func main() {
	root := Root()
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: %+v\n", err)
		os.Exit(1)
	}
}
