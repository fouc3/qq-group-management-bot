package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/onebot-ext/onebot"
	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/features/admincmd"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinrequest"
	"github.com/fouc3/qq-group-management-bot/internal/humanverify"
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
	// humanVerify is the online verification service in force, or an empty
	// holder when the file names none.
	//
	// Held here rather than handed to the console as a client because a reload
	// replaces the client, and the console answers requests while that happens:
	// what it has to read is the service in force now, not the one that existed
	// when the process started. atomic.Value because the write is on the reload
	// goroutine and the read is on a request.
	humanVerify atomic.Value
}

// humanVerifyHolder is what the atomic holds.
//
// A one-field struct rather than the interface itself, because atomic.Value
// refuses a nil interface and "this deployment has no service" is a state it has
// to be able to hold.
type humanVerifyHolder struct{ service feature.HumanVerify }

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
	built := &parts{
		registry: registry,
		deps:     deps,
		ctx:      ctx,
		logger:   logger,
		live:     map[string]*part{},
	}
	built.humanVerify.Store(humanVerifyHolder{service: deps.HumanVerify})
	return built
}

// startFromConfig starts every feature the configuration turns on.
//
// They are all built before any of them is registered, and wired in between, so
// that a feature is never reachable while what it drives still has nothing behind
// it -- and so that the startup log says what was connected rather than repeating
// itself once per feature.
func (p *parts) startFromConfig(cfg *config.Config) error {
	built, err := p.buildFromConfig(cfg)
	if err != nil {
		return err
	}
	return p.adopt(built...)
}

// buildFromConfig builds every feature the configuration turns on, without putting
// any of them to work.
//
// What comes back is handed to adopt, or thrown away whole when the change that asked
// for it cannot be completed. That is the point of separating it: a caller rebuilding
// everything has to be able to fail without having taken anything away first, or one
// section that cannot be built costs the features that were serving.
func (p *parts) buildFromConfig(cfg *config.Config) ([]*part, error) {
	for _, name := range cfg.FeatureNames() {
		if _, known := p.registry.Factory(name); !known {
			return nil, fmt.Errorf("unknown feature %q; registered features are: %s",
				name, strings.Join(p.registry.Names(), ", "))
		}
	}

	var built []*part
	for _, name := range p.registry.Names() {
		section, configured := cfg.Feature(name)
		if !configured {
			continue
		}
		on, err := feature.Enabled(name, section)
		if err != nil {
			return nil, err
		}
		if !on {
			continue
		}
		instance, err := p.build(name, section)
		if err != nil {
			return nil, err
		}
		built = append(built, instance)
	}
	if len(built) == 0 {
		return nil, errors.New("app: no feature is enabled; turn one on under features:")
	}
	return built, nil
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
	// The wiring is left until the replacement is in place, deliberately: wiring in
	// between would hand every consumer nothing for the length of that gap, and a
	// consumer whose question is answered by a feature -- whether a member is still
	// verifying, say -- would read "there is nobody" out of a nil and act on it. The
	// instance being replaced answers correctly until it is replaced.
	if err := p.halt(name); err != nil {
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
//
// Every replacement is built before anything is stopped. A wide change is
// all-or-nothing, and stopping first would make a section that cannot be built cost
// the features that were serving: the bot stays connected, answers nothing, and only
// a second edit to the file would bring them back.
func (p *parts) restartEverything(cfg *config.Config) error {
	built, err := p.buildFromConfig(cfg)
	if err != nil {
		return err
	}
	if err := p.stopAll(); err != nil {
		return err
	}
	return p.adopt(built...)
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
	// A record of the handlers this instance declares, made per build rather than shared
	// the way the button registry is: it is given back when the instance stops, and a
	// shared one would let this feature's Stop take another feature's handlers with it.
	deps.Events = command.NewEvents(deps.Client)
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

// Running reports whether a named feature is built and registered.
//
// It is running() for the management console, which is another package and cannot
// see an unexported method. The distinction it makes is the one the console needs:
// a feature written in the file but not running is a fact worth showing, because
// the alternative is an operator who believes they turned something on.
func (p *parts) Running(name string) bool { return p.running(name) }

// Instance returns the running instance of a feature, or nil.
//
// Handed out so that the console can ask a feature what it already answers
// questions about -- the command table is the one that matters today -- rather
// than keeping a second copy of an answer that would drift from the first.
func (p *parts) Instance(name string) feature.Feature { return p.instanceOf(name) }

// CommandDefs lists the commands the bot answers, for the console.
//
// The table's keeper is asked first, because its table is the whole list: its own
// commands plus everything the other features offered. Only when there is no keeper
// -- a bot whose admin_commands feature is off -- is what the other features offer
// on their own the truth. Asking the keeper for its *sources* instead of its table
// is what made the console answer an empty list on a bot with a full command table.
func (p *parts) CommandDefs() []command.Def {
	if keeper := p.instanceOf(admincmd.Name); keeper != nil {
		if whole, ok := any(keeper).(interface{ Definitions() []command.Def }); ok {
			return whole.Definitions()
		}
	}
	return feature.CommandSources(p.features(), nil)
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

// setHumanVerify replaces the verification service every feature built from now
// on is handed, and gives back the one it replaced so the caller can close it.
//
// A service that is not there is written as no interface at all rather than as a
// nil pointer: the feature asks whether it has one, and a nil pointer behind an
// interface answers yes.
func (p *parts) setHumanVerify(client *humanverify.Client) *humanverify.Client {
	previous, _ := p.deps.HumanVerify.(*humanverify.Client)
	p.deps.HumanVerify = nil
	if client != nil {
		p.deps.HumanVerify = client
	}
	// The console reads this one, so it is replaced in the same step as the copy
	// the features are handed: a console still answering about the closed client
	// would report a service nobody is connected to.
	//
	// A client that is not there is stored as no interface at all, for the same
	// reason the deps above are: a nil pointer inside an interface is not a nil
	// interface, and asking it whether it is connected dereferences the pointer.
	if client == nil {
		p.humanVerify.Store(humanVerifyHolder{})
	} else {
		p.humanVerify.Store(humanVerifyHolder{service: client})
	}
	return previous
}

// HumanVerifyConnected reports whether the online verification service could
// deliver a result right now.
//
// The console asks, because the answer decides what a held member can be given: a
// service that is not connected gets no link -- the service delivers each result
// once, so a link issued with nobody listening is a member who passed the check
// and is still held -- which means that group falls back to the button in the
// group. False is also the answer for a deployment that configured no service:
// nobody is connected to a service that does not exist.
func (p *parts) HumanVerifyConnected() bool {
	holder, _ := p.humanVerify.Load().(humanVerifyHolder)
	if holder.service == nil {
		return false
	}
	return holder.service.Connected()
}
