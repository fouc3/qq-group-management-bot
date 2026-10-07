package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/onebot-ext/onebot"
	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/features/admincmd"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinrequest"
	"github.com/fouc3/qq-group-management-bot/internal/mutelog"
)

// parts is the set of features one bot is running, and the operations that keep
// it that way.
//
// It exists because a feature is not built once any more: a configuration change
// builds the feature it names again, and the bot keeps serving while that happens.
// The order that takes -- stop answering, give the capabilities back, end the work
// in flight, build again, wire again, register again -- is here rather than spread
// along a startup path that only ever ran once.
type parts struct {
	registry *feature.Registry
	logger   *slog.Logger
	// ctx is what a feature is registered with.
	ctx context.Context
	// deps is the infrastructure every feature is handed. It is held rather than
	// copied away, so that a feature built later gets what is current.
	deps feature.Deps
	// live is what is running, by name.
	live map[string]*part
}

// part is one feature that is running, and the section it was built from.
//
// The section is kept because a reload has to tell a change from a repeat: a file
// that is looked at every three seconds says the same thing nearly every time.
type part struct {
	name     string
	section  yaml.Node
	instance feature.Feature
}

// newParts returns the manager of one bot's features.
//
// The button registry is made here and handed to every feature: a bot has one
// connection and therefore one dispatcher for presses, so a feature that made its
// own would answer presses the others also see.
func newParts(registry *feature.Registry, deps feature.Deps, ctx context.Context,
	logger *slog.Logger) *parts {
	if deps.Buttons == nil {
		deps.Buttons = command.NewButtons()
	}
	// One record of the mutes this bot applied, for the same reason as the button
	// registry above: the features that mute and the feature that lifts somebody
	// else's mute are different ones, and the platform does not say who applied a
	// mute.
	if deps.Mutes == nil {
		deps.Mutes = mutelog.New()
	}
	return &parts{
		registry: registry,
		deps:     deps,
		ctx:      ctx,
		logger:   logger,
		live:     map[string]*part{},
	}
}

// startFromConfig starts every feature the configuration turns on.
//
// They are all built before any of them is registered, and wired in between, so
// that a feature is never reachable while what it drives still has nothing behind
// it -- and so that the startup log says what was connected rather than repeating
// itself once per feature.
func (p *parts) startFromConfig(cfg *config.Config) error {
	for _, name := range cfg.FeatureNames() {
		if _, known := p.registry.Factory(name); !known {
			return fmt.Errorf("unknown feature %q; registered features are: %s",
				name, strings.Join(p.registry.Names(), ", "))
		}
	}

	var pending []*part
	for _, name := range p.registry.Names() {
		section, configured := cfg.Feature(name)
		if !configured {
			continue
		}
		on, err := feature.Enabled(name, section)
		if err != nil {
			return err
		}
		if !on {
			continue
		}
		built, err := p.build(name, section)
		if err != nil {
			return err
		}
		pending = append(pending, built)
	}
	if len(pending) == 0 {
		return errors.New("app: no feature is enabled; turn one on under features:")
	}
	return p.adopt(pending...)
}

// start puts one feature to work, which is what a section that appeared in the
// configuration while the bot was running asks for.
func (p *parts) start(name string, section yaml.Node) error {
	if p.running(name) {
		return fmt.Errorf("app: %s is already running", name)
	}
	built, err := p.build(name, section)
	if err != nil {
		return err
	}
	return p.adopt(built)
}

// stop takes one feature out of service.
//
// The capabilities are wired again afterwards, and that is not a detail: a feature
// that stopped has to stop being handed to the others, or they would keep calling
// an instance nobody is running any more.
func (p *parts) stop(name string) error {
	if err := p.halt(name); err != nil {
		return err
	}
	return p.wire()
}

// stopAll stops everything, which is what shutting the bot down amounts to.
//
// The wiring is run once at the end rather than after each one: what is left is
// nothing, and a wiring between two stops is a wiring nobody can observe.
func (p *parts) stopAll() error {
	var failures []error
	for _, name := range p.names() {
		if err := p.halt(name); err != nil {
			failures = append(failures, fmt.Errorf("stopping %s: %w", name, err))
		}
	}
	if err := p.wire(); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// halt stops one feature without wiring the rest again.
func (p *parts) halt(name string) error {
	part, running := p.live[name]
	if !running {
		return nil
	}
	delete(p.live, name)
	p.logger.Info("a feature is stopping", "feature", name)
	return part.instance.Close(p.ctx)
}

// restart builds one feature again from a new section.
//
// The new one is built before the old one stops, so that a section that cannot be
// built leaves the running feature alone: a typo in a configuration file should
// not take a feature away from the groups it serves until somebody notices.
func (p *parts) restart(name string, section yaml.Node) error {
	built, err := p.build(name, section)
	if err != nil {
		return err
	}
	if err := p.stop(name); err != nil {
		return err
	}
	return p.adopt(built)
}

// restartEverything rebuilds every feature, for a change that reaches all of them.
//
// A change reaches all of them when it is to something they hold a copy of: the
// group list, the bot's own QQ number, the cache address, the fallback client.
// Rebuilding all of them rather than keeping a list of which feature reads which
// setting is deliberate -- that list would be a second place to be wrong, and the
// cost here is one rebuild of a handful of features.
func (p *parts) restartEverything(cfg *config.Config) error {
	if err := p.stopAll(); err != nil {
		return err
	}
	return p.startFromConfig(cfg)
}

// build makes one feature, without putting it to work yet.
//
// It does not care whether the name is already running: a restart builds the
// replacement while the instance it replaces is still serving, which is what keeps
// a section that cannot be built from taking the feature away.
func (p *parts) build(name string, section yaml.Node) (*part, error) {
	factory, known := p.registry.Factory(name)
	if !known {
		return nil, fmt.Errorf("unknown feature %q; registered features are: %s",
			name, strings.Join(p.registry.Names(), ", "))
	}
	deps := p.deps
	deps.Logger = p.deps.Logger.With("feature", name)
	instance, err := factory(section, deps)
	if err != nil {
		return nil, fmt.Errorf("feature %s: %w", name, err)
	}
	if instance.Name() != name {
		return nil, fmt.Errorf("feature registered as %q calls itself %q",
			name, instance.Name())
	}
	return &part{name: name, section: section, instance: instance}, nil
}

// adopt puts built features to work: they are wired to what they drive, and only
// then registered, so that nothing can arrive at a feature whose capabilities are
// not behind it yet.
func (p *parts) adopt(built ...*part) error {
	for _, part := range built {
		p.live[part.name] = part
	}
	if err := p.wire(); err != nil {
		for _, part := range built {
			delete(p.live, part.name)
		}
		return err
	}
	for _, part := range built {
		if err := part.instance.Register(p.ctx); err != nil {
			delete(p.live, part.name)
			_ = part.instance.Close(context.Background())
			_ = p.wire()
			return fmt.Errorf("app: registering %s: %w", part.name, err)
		}
		p.logger.Info("feature ready", "feature", part.name)
	}
	return nil
}

// wire hands every capability to the features that are running now.
//
// It runs after anything that changes who provides what, so that a feature which
// stopped stops being handed to the others and one which started is offered. It is
// the same description of what depends on what that the bot started with, and it
// being re-runnable is the point: a wiring that only happened once is what made
// building a feature again impossible.
func (p *parts) wire() error {
	running := p.features()
	if len(running) == 0 {
		// Nothing is running, so there is nothing to hand anything to. Saying so
		// through five lines about capabilities nobody provides would only be
		// noise on the way out.
		return nil
	}
	if err := feature.InjectVerifier(running, p.logger); err != nil {
		return err
	}
	if err := feature.InjectAdminDirectory(running, p.logger); err != nil {
		return err
	}
	if err := feature.InjectModeration(running, p.logger); err != nil {
		return err
	}
	// A feature that waits for free text has to be able to tell a command from what
	// a member typed, and only the table knows what a command looks like.
	if err := feature.InjectCommands(running, p.logger); err != nil {
		return err
	}
	// The commands other features answer are handed to whoever keeps the table, so
	// that the help a group is shown, the menu it taps and the dispatcher that runs
	// a command are still one list. It is wired after everything else because it is
	// the one wiring whose consumer can refuse what it is handed.
	if err := feature.InjectCommandSources(running, p.logger); err != nil {
		return err
	}
	// The blacklist does not come from a feature: it is the data layer's, in the
	// two shapes the two features want from it.
	feature.Inject[joinrequest.Blacklist, joinBarrierAware](running,
		barredFromJoining{blacklist: p.deps.Store.Blacklist()}, "the data layer",
		"the join barrier", p.logger, joinBarrierAware.SetBlacklist)
	feature.Inject[admincmd.Blacklist, blacklistAdminAware](running,
		p.deps.Store.Blacklist(), "the data layer", "the blacklist", p.logger,
		blacklistAdminAware.SetBlacklist)
	return nil
}

// features lists what is running, in registration order.
func (p *parts) features() []feature.Feature {
	var running []feature.Feature
	for _, name := range p.names() {
		running = append(running, p.live[name].instance)
	}
	return running
}

// names lists what is running, in the order the registry was written in.
func (p *parts) names() []string {
	var running []string
	for _, name := range p.registry.Names() {
		if _, live := p.live[name]; live {
			running = append(running, name)
		}
	}
	return running
}

// running reports whether a feature is.
func (p *parts) running(name string) bool {
	_, live := p.live[name]
	return live
}

// sectionOf returns the section a running feature was built from.
func (p *parts) sectionOf(name string) (yaml.Node, bool) {
	part, live := p.live[name]
	if !live {
		return yaml.Node{}, false
	}
	return part.section, true
}

// setSection records the section a running feature has adopted, so that a file
// which is looked at again does not look like a change a second time.
func (p *parts) setSection(name string, section yaml.Node) {
	if part, live := p.live[name]; live {
		part.section = section
	}
}

// instanceOf returns a running feature, so that a reload can ask whether it can
// take a new configuration without being built again.
func (p *parts) instanceOf(name string) feature.Feature {
	if part, live := p.live[name]; live {
		return part.instance
	}
	return nil
}

// intents is what the connection has to subscribe to.
func (p *parts) intents() qqbotsdk.Intent {
	return feature.Intents(p.features())
}

// setOneBot replaces the fallback client every feature built from now on is handed.
func (p *parts) setOneBot(client *onebot.Client) {
	p.deps.OneBot = client
}

// setRedis replaces the cache settings every feature built from now on is handed.
func (p *parts) setRedis(cfg *config.Config) {
	p.deps.Redis = cfg.RedisConfig()
}
