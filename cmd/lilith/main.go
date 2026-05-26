package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ernado/svetik"
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
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/ernado/svetik/internal/db"
	"github.com/ernado/svetik/internal/prompt"
)

type Application struct {
	api    *tg.Client
	client *telegram.Client
	ai     *openrouter.Client
	db     lilith.DB
	self   *tg.User

	model string

	waiter *floodwait.Waiter
	trace  trace.Tracer
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
		}
	}

	return cc, nil
}

func (a *Application) resolveChannel(ctx context.Context, channel *tg.Channel, userID int64) (*chatContext, error) {
	if err := a.fetchChannelParticipants(ctx, channel); err != nil {
		return nil, errors.Wrap(err, "fetch channel participants")
	}

	cc := &chatContext{
		chatID:   channel.ID,
		chatInfo: channel.Title,
	}

	if self, err := a.db.GetChatMember(ctx, channel.ID, a.self.ID); err == nil {
		cc.selfRank = self.Rank
	}

	if member, err := a.db.GetChatMember(ctx, channel.ID, userID); err == nil {
		cc.userIsAdmin = member.IsAdmin
		cc.userIsCreator = member.IsCreator
		cc.userRank = member.Rank
	}

	return cc, nil
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
		zap.Int64("user_id", user.ID),
	)

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

	if err := a.db.UpsertChatMember(ctx, lilith.ChatMember{
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

	if err := a.db.SaveMessage(ctx, lilith.Message{
		ChatID:        cc.chatID,
		MessageID:     int64(m.ID),
		UserID:        user.ID,
		Date:          time.Unix(int64(m.Date), 0),
		Text:          m.Message,
		IsMyself:      m.Out,
		ReplyToID:     replyToID,
		ReplyToText:   replyToText,
		ReplyToMyself: replyToMyself,
	}); err != nil {
		lg.Error("save message", zap.Error(err))
	}

	if m.Out {
		return nil
	}

	switch m.Message {
	case "/start", "/start@" + a.self.Username:
		if _, err := reply.Text(ctx, "Привет, "+user.FirstName+"!"); err != nil {
			return errors.Wrap(err, "send message")
		}
	case "/lobotomy", "/lobotomy@" + a.self.Username:
		if _, err := reply.Text(ctx, "Не реализовано"); err != nil {
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
		} {
			if strings.Contains(strings.ToLower(m.Message), name) {
				shouldResponse = true
			}
		}

		if !shouldResponse {
			lg.Info("Ignoring message")
			return nil
		}

		dialog := []openrouter.ChatCompletionMessage{
			openrouter.SystemMessage(strings.Join([]string{
				prompt.Protocol, prompt.Character,
			}, "\n")),
		}

		lastMessages, err := a.db.GetLastMessages(ctx, cc.chatID, 150)
		if err != nil {
			return errors.Wrap(err, "get last messages")
		}

		for _, msg := range lastMessages {
			member, err := a.db.GetChatMember(ctx, msg.ChatID, msg.UserID)
			if err != nil {
				return errors.Wrap(err, "get member")
			}

			dialogContext := lilith.Context{
				Message: &msg,
				User:    member,
				Self: &lilith.Self{
					Name:     a.self.FirstName,
					Nickname: a.self.Username,
					Rank:     cc.selfRank,
				},
			}

			data, err := json.Marshal(dialogContext)
			if err != nil {
				return errors.Wrap(err, "marshal dialog context")
			}

			dialog = append(dialog, openrouter.UserMessage(string(data)))
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
			Model:    a.model,
			Messages: dialog,
		})
		close(done)

		if err != nil {
			return errors.Wrap(err, "generate content")
		}

		replyText := resp.Choices[0].Message.Content.Text
		if strings.TrimSpace(replyText) == "" {
			return nil
		}
		replyUpdate, err := reply.Text(ctx, replyText)
		if err != nil {
			return errors.Wrap(err, "send reply")
		}

		switch v := replyUpdate.(type) {
		case *tg.UpdateShortSentMessage:
			if err := a.db.SaveMessage(ctx, lilith.Message{
				ChatID:    cc.chatID,
				MessageID: int64(v.ID),
				UserID:    a.self.ID,
				Date:      time.Unix(int64(v.Date), 0),
				Text:      replyText,
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
						Text:      replyText,
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

			if err := a.db.UpsertChatMember(ctx, lilith.ChatMember{
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
					api:    tg.NewClient(client),
					ai:     ai,
					model:  aiModel,
					db:     db.New(databaseConnection),
					client: client,
					waiter: waiter,
					trace:  t.TracerProvider().Tracer("svetik.bot"),
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
