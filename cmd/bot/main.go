// Command bot runs the QQ group management bot.
//
// It reads one YAML file, builds the features that file turns on, and serves
// until it is asked to stop. Everything the bot does is switched from that
// file, so no behaviour is hidden in the code.
//
// Usage:
//
//	bot [-config config.yaml]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/fouc3/qq-group-management-bot/internal/app"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/features/admincmd"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinrequest"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinverify"
	"github.com/fouc3/qq-group-management-bot/internal/features/moderation"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", config.DefaultConfigPath, "path to the YAML configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := app.NewLogger(cfg.Log)

	// Registering the features here is the whole extension point: a new
	// feature adds one line to this list and a section to the file.
	registry := feature.NewRegistry()
	registry.Add(joinverify.Name, joinverify.New)
	registry.Add(joinrequest.Name, joinrequest.New)
	registry.Add(admincmd.Name, admincmd.New)
	registry.Add(moderation.Name, moderation.New)
	logger.Debug("features registered", "features", registry.Names())

	// SIGHUP is deliberately not in this list: it asks for a reload rather than a
	// stop, and the app listens for it. Interrupt and SIGTERM stop the bot.
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, cfg, *configPath, registry, logger); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}
