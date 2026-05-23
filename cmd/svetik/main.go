package main

import (
	"context"
	"os"
	"strconv"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/app"
	"github.com/go-faster/sdk/zctx"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/genai"
)

type Application struct {
	api    *tg.Client
	client *telegram.Client
	ai     *genai.Client

	waiter *floodwait.Waiter

	trace    trace.Tracer
	nickname string
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

				a.nickname = self.Username
			}
			if _, err := a.api.BotsSetBotCommands(ctx, &tg.BotsSetBotCommandsRequest{
				Scope:    &tg.BotCommandScopeDefault{},
				LangCode: "en",
				Commands: []tg.BotCommand{
					{
						Command:     "start",
						Description: "Start bot",
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

func generateSafetySettings() []*genai.SafetySetting {
	var settings []*genai.SafetySetting
	for _, category := range []genai.HarmCategory{
		genai.HarmCategoryHarassment,
		genai.HarmCategoryHateSpeech,
		genai.HarmCategorySexuallyExplicit,
	} {
		settings = append(settings, &genai.SafetySetting{
			Category:  category,
			Threshold: genai.HarmBlockThresholdBlockNone,
		})
	}
	return settings
}

const systemPrompt = `
- Ты 21-летняя умная и стеснительная девочка Светик (Света).
- Общайся так, как принято в русскоязычных мессенджерах:
  1) Не ставь длинное тире, используй минус
  2) Не ставь emoji, особенно в конце предложения
`

func (a *Application) onNewMessage(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
	ctx, span := a.trace.Start(ctx, "OnNewMessage")
	defer span.End()
	m, ok := u.Message.(*tg.Message)
	if !ok || m.Out {
		return nil
	}
	var (
		sender = message.NewSender(a.api)
		reply  = sender.Reply(e, u)
		lg     = zctx.From(ctx).With(zap.Int("msg.id", m.ID))
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

	switch m.Message {
	case "/start", "/start@" + a.nickname:
		if _, err := reply.Text(ctx, "Hello, "+user.FirstName+"!"); err != nil {
			return errors.Wrap(err, "send message")
		}
	default:
		resp, err := a.ai.Models.GenerateContent(ctx,
			"gemini-3.1-flash-lite-preview",
			[]*genai.Content{
				genai.NewContentFromText(m.Message, genai.RoleUser),
			},
			&genai.GenerateContentConfig{
				SafetySettings:    generateSafetySettings(),
				SystemInstruction: genai.NewContentFromText(systemPrompt, genai.RoleUser),
			},
		)
		if err != nil {
			return errors.Wrap(err, "generate content")
		}
		if _, err := reply.Text(ctx, resp.Text()); err != nil {
			return errors.Wrap(err, "send message")
		}
	}
	return nil
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

func main() {
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
		ai, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey:  os.Getenv("AI_TOKEN"),
			Backend: genai.BackendGeminiAPI,
		})
		if err != nil {
			return errors.Wrap(err, "create ai")
		}
		a := &Application{
			api:    tg.NewClient(client),
			ai:     ai,
			client: client,
			waiter: waiter,
			trace:  t.TracerProvider().Tracer("svetik.bot"),
		}
		dispatcher.OnChannelParticipant(a.onChannelParticipant)
		dispatcher.OnNewMessage(a.onNewMessage)
		return a.Run(ctx)
	},
		app.WithZapConfig(func() zap.Config {
			cfg := zap.NewProductionConfig()
			cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
			return cfg
		}()),
	)
}
