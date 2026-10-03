// Package app assembles the bot: it turns a configuration file into a client,
// a set of features and a transport, then runs them until asked to stop.
package app

import (
	"context"
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

// NewLogger builds the logger the rest of the program uses, and the switch its
// level is set through.
//
// The level is a variable rather than a constant because it is the one logging
// setting that can change while the bot runs: the handler is fixed once built, and
// every feature holds a copy of the logger, so a new level has to reach the same
// handler rather than replace it.
func NewLogger(cfg config.Log) (*slog.Logger, *slog.LevelVar) {
	level := new(slog.LevelVar)
	level.Set(levelOf(cfg.Level))
	options := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, options)
	} else {
		handler = slog.NewTextHandler(os.Stderr, options)
	}
	return slog.New(handler), level
}

// Run builds the bot and serves until ctx is cancelled.
//
// Every feature is registered before the connection is opened, so no event can
// arrive before the handlers that should see it exist. Shutdown reverses that
// order: the connection closes first, then the features release what they hold.
//
// What is built here is not built once and for all: a configuration change builds
// a feature again, which is why the assembly and the lifetimes live in parts.
func Run(ctx context.Context, cfg *config.Config, configPath string,
	registry *feature.Registry, logger *slog.Logger, level *slog.LevelVar) error {
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

	// The features, and the lifetimes that a configuration change can restart. They
	// are built, wired and registered in that order, so that nothing can arrive at
	// a feature while what it drives still has nothing behind it.
	live := newParts(registry, feature.Deps{
		Client: client,
		OneBot: oneBot,
		Logger: logger,
		Groups: cfg.Bot.Groups,
		BotQQ:  cfg.Bot.QQ,
		Store:  database,
		Redis:  cfg.RedisConfig(),
	}, ctx, logger)
	if err := live.startFromConfig(cfg); err != nil {
		return err
	}
	defer func() {
		if err := live.stopAll(); err != nil {
			logger.Warn("a feature did not stop cleanly", "error", err)
		}
	}()

	// The file is watched from here on: a change is adopted without a restart, by
	// building the feature it names again. Started after the features are up, so
	// that nothing can be reconfigured before it is connected.
	reload := &reloader{
		path:    configPath,
		parts:   live,
		logger:  logger,
		level:   level,
		current: cfg,
	}
	go watchReloads(ctx, reload)

	intents := live.intents()
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
