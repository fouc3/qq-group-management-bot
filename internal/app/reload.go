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
// SIGHUP asks immediately. Neither can change anything a running bot cannot change --
// what to do about that is decided in once, and a refusal says why.
type reloader struct {
	path     string
	features []feature.Feature
	logger   *slog.Logger
	// applied is the configuration in force, section by section. A section is replaced
	// only once its feature has accepted it, so a feature that refused is asked again
	// on the next look rather than being quietly assumed to be up to date.
	applied map[string]yaml.Node
	// current is the last configuration read whole, for the parts a reload must not
	// touch: the platform credentials, the data layer, the cache, the groups.
	current *config.Config
	// lastError is the last load failure, so a file somebody is halfway through
	// editing is reported once instead of every three seconds.
	lastError string
}

// watchReloads reloads until the context ends.
func watchReloads(ctx context.Context, path string, cfg *config.Config,
	features []feature.Feature, logger *slog.Logger) {
	watch := &reloader{
		path:     path,
		features: features,
		logger:   logger,
		applied:  map[string]yaml.Node{},
		current:  cfg,
	}
	for name, section := range cfg.Features {
		watch.applied[name] = section
	}

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
			watch.once("asked to reload")
		case <-ticker.C:
			watch.once("the file changed")
		}
	}
}

// once reads the file and applies whatever changed.
func (r *reloader) once(why string) {
	fresh, err := config.Load(r.path)
	if err != nil {
		// A file that cannot be read leaves the bot exactly as it was. The alternative
		// -- acting on half a file somebody is still editing -- is worse than being out
		// of date for a few seconds, and it is why validation happens before anything
		// is applied rather than during.
		if message := err.Error(); message != r.lastError {
			r.lastError = message
			r.logger.Error("the configuration could not be read, so nothing changed",
				"path", r.path, "error", err)
		}
		return
	}
	r.lastError = ""

	if reason := r.structuralChange(fresh); reason != "" {
		r.logger.Warn("the configuration asks for something a reload cannot do, "+
			"so nothing changed: restart the bot", "reason", reason, "path", r.path)
		return
	}

	applied := 0
	for _, instance := range r.features {
		section, ok := fresh.Features[instance.Name()]
		if !ok || sameSection(r.applied[instance.Name()], section) {
			continue
		}
		reloadable, ok := instance.(feature.Reloadable)
		if !ok {
			// Refused in structuralChange already; this is the belt to that pair of
			// braces.
			continue
		}
		if err := reloadable.Reload(section); err != nil {
			r.logger.Error("a feature refused its new configuration, so it keeps the "+
				"old one", "feature", instance.Name(), "error", err)
			continue
		}
		r.applied[instance.Name()] = section
		applied++
		r.logger.Info("a feature adopted a new configuration",
			"feature", instance.Name(), "why", why)
	}
	if applied == 0 {
		r.logger.Debug("no configuration change to apply", "why", why)
	}
	r.current = fresh
}

// structuralChange reports why the file cannot be adopted, or "" when it can.
func (r *reloader) structuralChange(fresh *config.Config) string {
	before := r.current
	switch {
	case !reflect.DeepEqual(before.Bot, fresh.Bot):
		return "bot 段改了（appid、凭证或群列表），这些只在启动时读取"
	case !reflect.DeepEqual(before.Database, fresh.Database):
		return "database 段改了，连接在启动时建立"
	case !reflect.DeepEqual(before.Redis, fresh.Redis):
		return "redis 段改了，连接在启动时建立"
	case !reflect.DeepEqual(before.OneBot, fresh.OneBot):
		return "onebot 段改了，客户端在启动时建立"
	case !reflect.DeepEqual(before.Log, fresh.Log):
		return "log 段改了，日志器在启动时建立"
	}
	for name := range fresh.Features {
		if _, ok := r.applied[name]; !ok {
			return "新增了功能 " + name + "，装配只在启动时进行"
		}
	}
	for name := range r.applied {
		if _, ok := fresh.Features[name]; !ok {
			return "删除了功能 " + name + "，装配只在启动时进行"
		}
	}
	for _, instance := range r.features {
		section, ok := fresh.Features[instance.Name()]
		if !ok || sameSection(r.applied[instance.Name()], section) {
			continue
		}
		if _, ok := instance.(feature.Reloadable); !ok {
			return "功能 " + instance.Name() + " 不支持热重载"
		}
	}
	return ""
}

// sameSection reports whether two configuration sections say the same thing.
//
// The comparison is by content, and deliberately not a DeepEqual on the nodes: a node
// carries the line and column it came from, so inserting a line anywhere above a
// section makes that section look changed even when not one character of it moved. That
// is not a corner case -- it is what every edit above another section looks like -- and
// it refused a real reload for a reason nobody could have seen from the file.
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
