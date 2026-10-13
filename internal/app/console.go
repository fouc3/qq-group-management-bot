package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/httpapi"
	"github.com/fouc3/qq-group-management-bot/internal/logring"
	"github.com/fouc3/qq-group-management-bot/internal/store"
	"github.com/fouc3/qq-group-management-bot/web"
)

// startConsole brings up the management console, when the file asks for one, and
// returns as soon as it is listening.
//
// It is built after the features are running and before the connection is
// opened, which is the same order the reloader follows and for the same reason:
// nothing should be able to ask a question before there is anything to answer
// with.
//
// A failure to bind stops the bot. That is the deliberate part: a port already in
// use is an operator's mistake, and discovering it at startup -- where somebody
// is watching the output -- is much better than a goroutine that logs into
// nowhere and leaves the console silently absent.
func startConsole(ctx context.Context, cfg *config.Config, configPath string,
	data store.Store, live *parts, reload *reloader, logs *logring.Ring,
	logger *slog.Logger) error {
	if !cfg.Admin.Enabled {
		logger.Debug("the management console is off")
		return nil
	}

	console, err := httpapi.New(httpapi.Options{
		Admin:      cfg.Admin,
		Config:     reload,
		ConfigPath: configPath,
		Store:      data,
		Features:   live,
		// The same object, because the online verification service is the one
		// thing here whose connection is replaced by a reload: the console has to
		// ask the parts in force, not one client captured at startup.
		HumanVerify: live,
		Reload:      reload,
		// The copy of the log the logger has been keeping, so the console can
		// show the newest lines without being able to read the journal.
		Logs: logs,
		// The compiled front end, embedded in the binary. admin.web_dir can
		// override it at request time, which is what working on the front end
		// uses.
		Web:     web.Dist(),
		Logger:  logger.With("component", "console"),
		Started: time.Now(),
	})
	if err != nil {
		return err
	}

	listener, err := console.Listen()
	if err != nil {
		return err
	}

	go func() {
		if err := console.Serve(ctx, listener); err != nil {
			// The console is not the bot: if it stops, the bot keeps serving the
			// groups. Said loudly rather than fatally, because a console that
			// died must not take a group's moderation with it.
			logger.Error("the management console stopped answering", "error", err)
		}
	}()
	return nil
}
