package app

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// reloadInterval is how often the file is looked at. Polling rather than watching
// because it needs no dependency, and a three second lag is not a cost anybody pays
// attention to.
const reloadInterval = 3 * time.Second

// reloader adopts a changed configuration while the bot runs.
//
// Two ways in, both ending in the same place: the file is looked at on a timer, and
// SIGHUP asks immediately.
type reloader struct {
	path   string
	parts  *parts
	logger *slog.Logger
	// level is the switch log.level is set through. A logger's handler is fixed
	// once it is built -- and every feature holds a copy of it -- so the level is
	// the one logging setting that can change without building anything again.
	level *slog.LevelVar
	// current is the configuration in force, which is what a change is measured
	// against.
	current *config.Config
	// lastError is the last load failure, so a file somebody is halfway through
	// editing is reported once instead of every three seconds.
	lastError string
}

// watchReloads reloads until the context ends.
func watchReloads(ctx context.Context, reload *reloader) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	defer signal.Stop(signals)
	ticker := time.NewTicker(reloadInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			reload.once("asked to reload")
		case <-ticker.C:
			reload.once("the file changed")
		}
	}
}

// once reads the file and adopts whatever changed.
func (r *reloader) once(why string) {
	fresh, err := config.Load(r.path)
	if err != nil {
		// A file that cannot be read leaves the bot exactly as it was. The
		// alternative -- acting on half a file somebody is still editing -- is worse
		// than being out of date for a few seconds, and it is why the file is
		// parsed and validated as a whole before anything is adopted from it.
		if message := err.Error(); message != r.lastError {
			r.lastError = message
			r.logger.Error("the configuration could not be read, so nothing changed",
				"path", r.path, "error", err)
		}
		return
	}
	r.lastError = ""

	if sameConfig(r.current, fresh) {
		r.logger.Debug("no configuration change to apply", "why", why)
		return
	}
	r.apply(fresh, why)
	r.current = fresh
}

// apply adopts what the file changed into what is running.
//
// Every section is judged on its own. What can change while the bot runs changes
// here, and what cannot is said out loud and left alone -- the rest of the file is
// still adopted. Refusing the whole file because one section cannot move would make
// a reload useless in exactly the case it is for: an operator editing a feature
// while the credentials above it stay as they were.
func (r *reloader) apply(fresh *config.Config, why string) {
	for _, what := range r.immovable(fresh) {
		r.logger.Warn("a change cannot be adopted while the bot runs, so it was "+
			"not applied; the rest of the file was", "what", what, "path", r.path)
	}

	if fresh.Log.Level != r.current.Log.Level {
		r.level.Set(levelOf(fresh.Log.Level))
		r.logger.Info("the log level changed", "level", fresh.Log.Level, "why", why)
	}

	if wideChange(r.current, fresh) {
		r.adoptWide(fresh, why)
		return
	}
	r.adoptPerFeature(fresh, why)
}

// immovable lists what changed and cannot change while the bot runs, with the
// reason it cannot.
func (r *reloader) immovable(fresh *config.Config) []string {
	before := r.current
	var reasons []string
	if connectionChanged(before.Bot, fresh.Bot) {
		reasons = append(reasons, "bot 段的连接设置改了（appid / client_secret / "+
			"access_token / sandbox / base_url）：连接只在启动时建立")
	}
	if !reflect.DeepEqual(before.Database, fresh.Database) {
		reasons = append(reasons, "database 段改了：换一个库等于换一套扣留记录，"+
			"运行中切换会让正被禁言的成员在新库里查不到记录")
	}
	if before.Log.Format != fresh.Log.Format {
		reasons = append(reasons, "log.format 改了：日志器在启动时建立，"+
			"每个功能手里都是它")
	}
	return reasons
}

// adoptWide rebuilds every feature, for a change that reaches all of them.
func (r *reloader) adoptWide(fresh *config.Config, why string) {
	if !reflect.DeepEqual(r.current.OneBot, fresh.OneBot) {
		client, err := buildOneBot(fresh.OneBot, r.logger)
		if err != nil {
			r.logger.Error("the fallback client could not be built, so the onebot "+
				"change was not adopted", "error", err)
			return
		}
		r.parts.setOneBot(client)
	}
	r.parts.setRedis(fresh)
	if err := r.parts.restartEverything(fresh); err != nil {
		r.logger.Error("the features could not all be built again", "error", err)
		return
	}
	r.logger.Info("every feature was built again, because the change reaches all "+
		"of them", "why", why)
}

// adoptPerFeature adopts the changes that reach one feature each.
func (r *reloader) adoptPerFeature(fresh *config.Config, why string) {
	for _, name := range r.parts.names() {
		section, configured := fresh.Feature(name)
		switch {
		case !configured:
			r.stop(name, "它这一段从配置里删掉了")
			continue
		case !r.enabled(name, section, true):
			r.stop(name, "配置里关掉了")
			continue
		}
		held, _ := r.parts.sectionOf(name)
		if sameSection(held, section) {
			// Nothing of this feature's changed.
			continue
		}
		r.change(name, section, why)
	}

	// A feature that is configured and turned on, and is not running.
	for _, name := range r.parts.registry.Names() {
		section, configured := fresh.Feature(name)
		if !configured || r.parts.running(name) {
			continue
		}
		if !r.enabled(name, section, false) {
			continue
		}
		if err := r.parts.start(name, section); err != nil {
			r.logger.Error("a feature could not be started, so it is not running",
				"feature", name, "error", err)
			continue
		}
		r.logger.Info("a feature was started while the bot runs", "feature", name)
	}
}

// change adopts one feature's new section.
func (r *reloader) change(name string, section yaml.Node, why string) {
	if reloadable, ok := r.parts.instanceOf(name).(feature.Reloadable); ok {
		// A feature that can take new configuration without being built again says
		// so, and keeps whatever it was holding: a cache, a list of holds, the
		// groups it has already read.
		if err := reloadable.Reload(section); err != nil {
			r.logger.Error("a feature refused its new configuration, so it keeps "+
				"the old one", "feature", name, "error", err)
			return
		}
		r.parts.setSection(name, section)
		r.logger.Info("a feature adopted a new configuration",
			"feature", name, "why", why)
		return
	}
	if err := r.parts.restart(name, section); err != nil {
		r.logger.Error("a feature could not be built again, so the one running "+
			"keeps the old configuration", "feature", name, "error", err)
		return
	}
	r.logger.Info("a feature was built again with its new configuration",
		"feature", name, "why", why)
}

// stop takes one feature out of service and says why.
func (r *reloader) stop(name, why string) {
	if err := r.parts.stop(name); err != nil {
		r.logger.Error("a feature could not be stopped", "feature", name, "error", err)
		return
	}
	r.logger.Info("a feature was stopped", "feature", name, "why", why)
}

// enabled reports whether a section turns its feature on.
//
// onUnreadable is what an unreadable section counts as, which differs by caller: a
// running feature keeps running rather than being stopped over a section nobody
// could parse, and one that is not running stays that way.
func (r *reloader) enabled(name string, section yaml.Node, onUnreadable bool) bool {
	on, err := feature.Enabled(name, section)
	if err != nil {
		r.logger.Error("a feature's section could not be read, so it keeps what "+
			"it has", "feature", name, "error", err)
		return onUnreadable
	}
	return on
}

// wideChange reports whether a change reaches every feature.
//
// It is about what a feature holds a copy of rather than what it reads: the group
// list, the bot's own QQ number, the cache address and the fallback client are all
// handed over when a feature is built, so one built before the change would keep
// the old one.
func wideChange(before, fresh *config.Config) bool {
	return !reflect.DeepEqual(before.Bot.Groups, fresh.Bot.Groups) ||
		before.Bot.QQ != fresh.Bot.QQ ||
		!reflect.DeepEqual(before.Redis, fresh.Redis) ||
		!reflect.DeepEqual(before.OneBot, fresh.OneBot)
}

// connectionChanged reports whether the settings that build the connection differ.
//
// They cannot be adopted while the bot runs: a new client is a new session and a new
// subscription, and the platform does not hand a running connection new
// credentials.
func connectionChanged(before, fresh config.Bot) bool {
	return before.AppID != fresh.AppID ||
		before.ClientSecret != fresh.ClientSecret ||
		before.AccessToken != fresh.AccessToken ||
		before.Sandbox != fresh.Sandbox ||
		before.BaseURL != fresh.BaseURL
}

// sameConfig reports whether two configurations say the same thing, section by
// section, so that a file which was not edited does nothing at all.
func sameConfig(before, fresh *config.Config) bool {
	switch {
	case !reflect.DeepEqual(before.Bot, fresh.Bot),
		!reflect.DeepEqual(before.OneBot, fresh.OneBot),
		!reflect.DeepEqual(before.Database, fresh.Database),
		!reflect.DeepEqual(before.Redis, fresh.Redis),
		!reflect.DeepEqual(before.Log, fresh.Log):
		return false
	}
	if len(before.Features) != len(fresh.Features) {
		return false
	}
	for name, section := range fresh.Features {
		held, known := before.Features[name]
		if !known || !sameSection(held, section) {
			return false
		}
	}
	return true
}

// sameSection reports whether two configuration sections say the same thing.
//
// The comparison is by content, and deliberately not a DeepEqual on the nodes: a
// node carries the line and column it came from, so inserting a line anywhere above
// a section makes that section look changed even when not one character of it moved.
// That is not a corner case -- it is what every edit above another section looks
// like -- and it refused a real reload for a reason nobody could have seen from the
// file.
//
// Comments are ignored on purpose as well: a section that only had its explanations
// rewritten still says the same thing, and re-adopting a configuration because of a
// comment is work nobody asked for.
func sameSection(before, after yaml.Node) bool {
	if before.Kind != after.Kind || before.Tag != after.Tag ||
		before.Value != after.Value || len(before.Content) != len(after.Content) {
		return false
	}
	for index := range before.Content {
		if !sameSection(*before.Content[index], *after.Content[index]) {
			return false
		}
	}
	return true
}

// levelOf turns a configured log level into the level it names.
func levelOf(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
