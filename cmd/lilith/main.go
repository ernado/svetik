package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/app"
	"github.com/go-faster/sdk/zctx"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/revrost/go-openrouter"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/ernado/lilith"
	"github.com/ernado/lilith/internal/ai"
	"github.com/ernado/lilith/internal/bot"
	"github.com/ernado/lilith/internal/db"
	"github.com/ernado/lilith/internal/memory"
	"github.com/ernado/lilith/internal/static"
	"github.com/ernado/lilith/internal/weather"
)

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

// databaseURL returns the Postgres connection string from DATABASE_URL, falling
// back to the local development default.
func databaseURL() string {
	if uri := os.Getenv("DATABASE_URL"); uri != "" {
		return uri
	}
	return "postgres://postgres:postgres@localhost:5432/lilith?sslmode=disable"
}

// migrateUp applies database migrations. When force is true it only resolves a
// dirty migration state and returns.
func migrateUp(databaseURI string, force bool) error {
	d, err := iofs.New(db.Migrations, "_migrations")
	if err != nil {
		return errors.Wrap(err, "create iofs driver")
	}

	uri := strings.ReplaceAll(databaseURI, "postgres://", "pgx5://")
	m, err := migrate.NewWithSourceInstance("iofs", d, uri)
	if err != nil {
		return errors.Wrap(err, "create migrate")
	}

	if force {
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
	} else if errors.Is(err, migrate.ErrNoChange) {
		fmt.Println("No migrations to apply")
	} else {
		fmt.Println("Migrations applied successfully")
	}

	sourceErr, dbErr := m.Close()
	if sourceErr != nil {
		return errors.Wrap(sourceErr, "close source")
	}
	if dbErr != nil {
		return errors.Wrap(dbErr, "close db")
	}

	return nil
}

func run(ctx context.Context, _ *zap.Logger, t *app.Telemetry) error {
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
	weatherAPIKey := os.Getenv("WEATHER_API_KEY")
	if weatherAPIKey == "" {
		return errors.New("WEATHER_API_KEY environment variable not set")
	}

	databaseURI := databaseURL()

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

	router := openrouter.NewClient(os.Getenv("AI_TOKEN"))
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
	database := db.New(databaseConnection)

	weatherClient := weather.New(weatherAPIKey, weather.Options{})

	var fileStore lilith.FileStore
	staticAddr := os.Getenv("STATIC_ADDR")
	staticURL := os.Getenv("STATIC_URL")
	var staticServer *static.Server
	if staticAddr != "" && staticURL != "" {
		staticServer = static.New(staticAddr, staticURL)
		fileStore = staticServer
	}

	aiClient := ai.New(router, aiModel, weatherClient)
	mem := memory.New(database, aiClient)
	botApp := bot.New(client, database, aiClient, mem, fileStore, waiter, t.TracerProvider().Tracer("lilith.bot"))
	botApp.Register(dispatcher)

	if staticServer != nil {
		go func() {
			if err := staticServer.Run(ctx); err != nil {
				zctx.From(ctx).Error("static server error", zap.Error(err))
			}
		}()
	}

	return botApp.Run(ctx)
}

func Root() *cobra.Command {
	var forceMigration bool
	cmd := &cobra.Command{
		Use: "lilith",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := migrateUp(databaseURL(), forceMigration); err != nil {
				return err
			}
			if forceMigration {
				return nil
			}

			app.Run(run,
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
