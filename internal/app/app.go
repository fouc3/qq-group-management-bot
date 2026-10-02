// Package app assembles the bot: it turns a configuration file into a client,
// a set of features and a transport, then runs them until asked to stop.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/onebot-ext/onebot"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// stopTimeout bounds how long shutdown may take.
const stopTimeout = 15 * time.Second

// NewLogger builds the logger the rest of the program uses.
func NewLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	options := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, options)
	} else {
		handler = slog.NewTextHandler(os.Stderr, options)
	}
	return slog.New(handler)
}

// Run builds the bot and serves until ctx is cancelled.
//
// Features are registered before the connection is opened, so no event can
// arrive before the handlers that should see it exist. Shutdown reverses that
// order: the connection closes first, then the features release what they hold.
func Run(ctx context.Context, cfg *config.Config, registry *feature.Registry, logger *slog.Logger) error {
	client, err := buildClient(cfg)
	if err != nil {
		return err
	}

	if len(cfg.Bot.Groups) == 0 {
		logger.Warn("bot.groups is empty, so every group the bot is in is managed")
	}

	oneBot, err := buildOneBot(cfg.OneBot, logger)
	if err != nil {
		return err
	}

	// The data layer is opened before the features, and a failure stops the bot.
	// A bot that cannot record what it is holding would silence members and then
	// forget them, which is the outcome the store exists to prevent.
	database, err := store.Open(ctx, store.Config{
		Driver:       cfg.Database.Driver,
		DSN:          cfg.Database.DSN,
		MaxOpenConns: cfg.Database.MaxOpenConns,
	})
	if err != nil {
		return fmt.Errorf("app: opening the database: %w", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			logger.Warn("the database did not close cleanly", "error", err)
		}
	}()
	logger.Info("the data layer is open",
		"driver", cfg.Database.Driver, "dsn", cfg.Database.DSN)

	features, err := registry.Build(cfg, feature.Deps{
		Client:        client,
		OneBot:        oneBot,
		Logger:        logger,
		Groups:        cfg.Bot.Groups,
		BotQQ:         cfg.Bot.QQ,
		JoinTolerance: cfg.OneBot.Tolerance(),
		Store:         database,
	})
	if err != nil {
		return err
	}
	if len(features) == 0 {
		return errors.New("app: no feature is enabled; turn one on under features:")
	}
	// Features that drive another feature are wired here, after both are built
	// and before any event can arrive.
	if err := feature.InjectVerifier(features); err != nil {
		closeFeatures(ctx, features, logger)
		return err
	}
	feature.InjectAdminDirectory(features)
	for _, instance := range features {
		if err := instance.Register(ctx); err != nil {
			closeFeatures(ctx, features, logger)
			return fmt.Errorf("app: registering %s: %w", instance.Name(), err)
		}
		logger.Info("feature ready", "feature", instance.Name())
	}
	defer closeFeatures(ctx, features, logger)

	intents := feature.Intents(features)
	gateway, err := client.GetGateway(ctx)
	if err != nil {
		return fmt.Errorf("app: getting the gateway address: %w", err)
	}
	logger.Info("gateway reached", "url", gateway.URL, "intents", intents)

	client.UseTransport(qqbotsdk.NewWebSocketTransport(gateway.URL,
		qqbotsdk.WithIntents(intents)))

	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("app: starting the connection: %w", err)
	}
	logger.Info("connected, waiting for events")

	<-ctx.Done()
	logger.Info("shutting down")

	// The connection is closed with a fresh context: the one that was just
	// cancelled cannot be used to shut anything down cleanly.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		logger.Warn("the connection did not close cleanly", "error", err)
	}
	return nil
}

// buildClient turns the bot section into an SDK client.
//
// The configuration is assembled here and handed over, which is the SDK's
// intended division: it never reads the environment or a file itself.
func buildClient(cfg *config.Config) (*qqbotsdk.Client, error) {
	sdkConfig := qqbotsdk.Config{
		AppID:        cfg.Bot.AppID,
		ClientSecret: cfg.Bot.ClientSecret,
		AccessToken:  cfg.Bot.AccessToken,
		BaseURL:      cfg.Bot.ResolvedBaseURL(),
	}
	if err := sdkConfig.Validate(); err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	client, err := qqbotsdk.NewClientFromConfig(sdkConfig)
	if err != nil {
		return nil, fmt.Errorf("app: building the client: %w", err)
	}
	return client, nil
}

// buildOneBot returns the fallback client, or nil when none is configured.
//
// The fallback exists because the official platform refuses the group member
// removal endpoint, so that one action has to be performed by an OneBot account
// instead.
func buildOneBot(cfg config.OneBot, logger *slog.Logger) (*onebot.Client, error) {
	if !cfg.Enabled() {
		logger.Info("no onebot fallback is configured, so actions the official bot " +
			"is not allowed to perform are left to a person")
		return nil, nil
	}
	client, err := onebot.New(onebot.Options{
		BaseURL:     cfg.URL,
		AccessToken: cfg.AccessToken,
		Timeout:     cfg.Timeout(),
	})
	if err != nil {
		return nil, fmt.Errorf("app: building the onebot client: %w", err)
	}
	// The access token is deliberately not logged.
	logger.Info("onebot fallback is configured",
		"url", cfg.URL, "join_time_tolerance_seconds", cfg.Tolerance())
	return client, nil
}

// closeFeatures releases every feature, reporting rather than failing, because
// shutdown has to finish whatever the features report.
func closeFeatures(ctx context.Context, features []feature.Feature, logger *slog.Logger) {
	for _, instance := range features {
		if err := instance.Close(ctx); err != nil {
			logger.Warn("a feature did not close cleanly",
				"feature", instance.Name(), "error", err)
		}
	}
}
