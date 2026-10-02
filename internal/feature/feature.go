// Package feature defines what a bot feature is and how one is registered.
//
// A feature is one independently switchable behaviour: it owns a section under
// features: in the configuration file, declares the gateway intents it needs,
// and registers its own event handlers. Adding a feature means adding a
// package, one registry line and one configuration section; no core file has
// to change.
package feature

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/onebot-ext/onebot"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// Deps is what a feature is handed to do its work.
type Deps struct {
	// Client is the configured SDK client.
	Client *qqbotsdk.Client
	// OneBot is the fallback for what the official bot is not allowed to do,
	// or nil when no fallback is configured.
	OneBot *onebot.Client
	// Logger already carries the feature name, so a feature logs without
	// repeating itself.
	Logger *slog.Logger
	// Groups is the managed group list from bot.groups. An empty list allows
	// every group.
	Groups config.Groups
	// BotQQ is the bot's own QQ number, which the OneBot fallback uses to
	// accept a message as the bot's own rather than a forged copy.
	BotQQ int64
	// JoinTolerance is how many seconds a join match may differ between the
	// official event timestamp and OneBot's recorded join time.
	JoinTolerance int64
	// Store is the data layer: what has to outlive a restart.
	//
	// A feature takes the part it needs rather than this package declaring a
	// narrow contract for each operation. A feature that needs its own seam
	// already declares one -- the join-request feature's blacklist is the
	// example, and it is what lets that feature be tested without a database.
	Store store.Store
	// Redis is where the volatile cache lives, with its defaults filled in.
	//
	// Lifted out of the configuration the way Groups and JoinTolerance are, so
	// that a feature which caches something does not have to read the file.
	Redis config.Redis
}

// InGroup reports whether the feature should act on a group.
//
// Without a configured list every group is allowed, which is what the file asks
// for when it leaves bot.groups out.
func (d Deps) InGroup(groupOpenID string) bool {
	return d.Groups.Allowed(groupOpenID)
}

// GroupQQID returns the QQ group number that belongs to an openid.
//
// Only the OneBot fallback needs it, because OneBot acts on QQ group numbers
// while everything on the official side speaks of openids.
func (d Deps) GroupQQID(groupOpenID string) (int64, bool) {
	return d.Groups.QQGroupID(groupOpenID)
}

// Feature is one independently switchable behaviour.
type Feature interface {
	// Name is the feature's name: its key under features: in the file, and
	// the value of the feature log field.
	Name() string
	// Intents reports what the feature needs to receive. The app unions the
	// answers, so a feature never lists intents that belong to another one.
	Intents() qqbotsdk.Intent
	// Register declares the feature's event handlers.
	Register(ctx context.Context) error
	// Close releases what the feature holds. It may run after a failed or
	// skipped Register, so it must tolerate having nothing to release.
	Close(ctx context.Context) error
}

// Factory builds one feature from its own configuration section.
type Factory func(section yaml.Node, deps Deps) (Feature, error)

// enabledKey is the switch any feature section may carry.
const enabledKey = "enabled"

// Registry maps feature names to the factories that build them.
type Registry struct {
	factories map[string]Factory
	order     []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

// Add registers a factory under name. Registering the same name twice is a
// programming mistake, so it panics at startup rather than picking one.
func (r *Registry) Add(name string, factory Factory) {
	if _, exists := r.factories[name]; exists {
		panic("feature: " + name + " is registered twice")
	}
	r.factories[name] = factory
	r.order = append(r.order, name)
}

// Names lists the registered features in registration order.
func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}

// Build turns the configured sections into features.
//
// A section that is absent, or present with enabled: false, is skipped. A
// section whose name was never registered is an error: it is nearly always a
// typo, and ignoring it would leave the operator believing a feature runs while
// it does not.
func (r *Registry) Build(cfg *config.Config, deps Deps) ([]Feature, error) {
	for _, name := range cfg.FeatureNames() {
		if _, known := r.factories[name]; !known {
			return nil, fmt.Errorf("unknown feature %q; registered features are: %s",
				name, strings.Join(r.Names(), ", "))
		}
	}

	built := make([]Feature, 0, len(r.order))
	for _, name := range r.order {
		section, configured := cfg.Feature(name)
		if !configured {
			continue
		}
		on, err := sectionEnabled(name, section)
		if err != nil {
			return nil, err
		}
		if !on {
			continue
		}

		featureDeps := deps
		featureDeps.Logger = deps.Logger.With("feature", name)
		instance, err := r.factories[name](section, featureDeps)
		if err != nil {
			return nil, fmt.Errorf("feature %s: %w", name, err)
		}
		if instance.Name() != name {
			return nil, fmt.Errorf("feature registered as %q calls itself %q",
				name, instance.Name())
		}
		built = append(built, instance)
	}
	return built, nil
}

// sectionEnabled reads the enabled switch.
//
// A section that is written out without the switch is on: writing it is already
// a statement of intent. Only an explicit enabled: false turns it off.
func sectionEnabled(name string, section yaml.Node) (bool, error) {
	var probe struct {
		Enabled *bool `yaml:"enabled"`
	}
	if err := section.Decode(&probe); err != nil {
		return false, fmt.Errorf("feature %s: %w", name, err)
	}
	if probe.Enabled == nil {
		return true, nil
	}
	return *probe.Enabled, nil
}

// AdminDirectory reports who may administer a group.
//
// The administrator list is written in the command feature's configuration,
// but more than one feature has to know it, so the app shares it here rather
// than letting the features import each other.
type AdminDirectory interface {
	// IsAdmin reports whether a member administers a group.
	IsAdmin(groupOpenID, memberOpenID string) bool
}

// AdminAware is implemented by a feature that needs the administrator list.
type AdminAware interface {
	SetAdminDirectory(AdminDirectory)
}

// InjectAdminDirectory hands the administrator list to every feature that
// wants it.
func InjectAdminDirectory(features []Feature) {
	var directory AdminDirectory
	for _, instance := range features {
		if candidate, ok := instance.(AdminDirectory); ok {
			directory = candidate
			break
		}
	}
	for _, instance := range features {
		if aware, ok := instance.(AdminAware); ok {
			aware.SetAdminDirectory(directory)
		}
	}
}

// Verifier is what the administrator commands need from the join verification
// feature.
//
// It keeps the two independent: neither package imports the other, and the app
// hands one to the other once both are built.
// Moderation is what the moderation feature provides to whatever reports
// content: it judges a quoted message against the configured model.
//
// It is the seam the /违规举报 command reaches through, so that the command, the
// cache and the model stay in packages that do not import each other.
type Moderation interface {
	// JudgeQuoted judges the window of messages around one quoted message.
	//
	// An error means no judgement was reached: the message is not in the cache,
	// the cache is down, or the model could not be read. None of those is a
	// violation, and a caller must never treat one as a violation.
	JudgeQuoted(ctx context.Context, groupOpenID, quotedIndex,
		reporterOpenID string) (ModerationVerdict, error)
	// DryRun reports whether judgements are only to be recorded and reported.
	//
	// It is asked rather than assumed, and it is asked here rather than inside the
	// judgement, because the things it holds back -- silencing somebody, taking a
	// message back -- are the caller's to do and not the judge's.
	DryRun() bool
	// ReportPenaltySeconds is how long a reporter is silenced when a report finds
	// nothing, or zero when the configuration does not ask for that at all.
	//
	// The policy is the moderation feature's, and the silencing is the caller's:
	// the verdict says what was found, this says what the group's rules make of a
	// report that found nothing.
	ReportPenaltySeconds() int64
	// JudgingEnabled reports whether a model is configured to judge with at all.
	//
	// It is what decides whether the report command is worth offering: a command
	// whose only answer is that nothing is configured should not be in the menu.
	JudgingEnabled() bool
	// RecordOutcome says what was done about a judgement that was reached.
	//
	// The decision and the act happen in different places, so the record of the
	// first is closed by whoever did the second. An empty action is a fact too:
	// it means nothing was done.
	RecordOutcome(ctx context.Context, judgementID, action string, muteSeconds int64) error
}

// ModerationVerdict is what a judgement came to.
type ModerationVerdict struct {
	// JudgementID identifies the record written when the judgement was reached,
	// which the caller closes with RecordOutcome once it has acted.
	JudgementID string
	// Category is the configured name of what was found. Empty means nothing was
	// found, which is the only sense in which a verdict is "clean".
	Category string
	// Label is what a group may be told, and it comes from the configuration
	// rather than from the model.
	Label string
	// MuteSeconds is how long the member who posted the message is silenced for.
	// Zero means the finding is reported without silencing anybody.
	MuteSeconds int64
	// SubjectOpenID is the author of the message that was reported.
	SubjectOpenID string
	// QuotedMessageID is the reported message itself: the one a recall takes back.
	QuotedMessageID string
	// JudgedMessageIDs is everything that was sent for judgement, for the record.
	JudgedMessageIDs []string
	// Reason is the model's own explanation, for the administrators and the audit
	// table and never for the group.
	Reason string
	// Model names what answered.
	Model string
}

type Verifier interface {
	// Reverify holds a member again and sends a fresh prompt.
	Reverify(ctx context.Context, groupOpenID, memberOpenID string) error
	// SimulateDeadline runs the missed-deadline path for a member now, without
	// waiting for the deadline.
	SimulateDeadline(ctx context.Context, groupOpenID, memberOpenID string) error
	// IsPending reports whether a member is waiting to verify.
	//
	// A command asks before lifting a mute: a member who is being held has to
	// verify, and releasing them by hand would defeat the check. The command
	// gives way to the verification, not the other way round.
	IsPending(groupOpenID, memberOpenID string) bool
	// Resend sends the verification prompt again for a member who is already
	// waiting, using the button they already have.
	//
	// It is for the ordinary case of nobody noticing the first prompt: the
	// member is not held again and no fresh prompt replaces the old one, so the
	// button already in the group keeps working.
	Resend(ctx context.Context, groupOpenID, memberOpenID string) error
}

// VerifierAware is implemented by a feature that drives the join verification.
type VerifierAware interface {
	SetVerifier(Verifier)
}

// InjectVerifier hands the verification feature to every feature that wants it.
//
// It is the only place that knows both sides, which is what keeps the features
// from depending on each other. A nil verifier is handed over when no feature
// provides verification, so the receiver can say so instead of failing later.
func InjectVerifier(features []Feature) error {
	var verifier Verifier
	var providers []string
	for _, instance := range features {
		if candidate, ok := instance.(Verifier); ok {
			verifier = candidate
			providers = append(providers, instance.Name())
		}
	}
	if len(providers) > 1 {
		return fmt.Errorf("feature: %s both provide verification",
			strings.Join(providers, " and "))
	}
	for _, instance := range features {
		if aware, ok := instance.(VerifierAware); ok {
			aware.SetVerifier(verifier)
		}
	}
	return nil
}

// ModerationAware is implemented by a feature that reports content for judging.
type ModerationAware interface {
	SetModeration(Moderation)
}

// InjectModeration hands the moderation feature to whatever reports to it.
//
// A missing provider is not an error: it leaves every reporting feature with a
// nil judgement source, which is how a deployment without moderation tells a
// member that the command has nothing behind it, rather than pretending to judge
// and answering nothing.
func InjectModeration(features []Feature) error {
	var moderation Moderation
	var providers []string
	for _, instance := range features {
		if candidate, ok := instance.(Moderation); ok {
			moderation = candidate
			providers = append(providers, instance.Name())
		}
	}
	if len(providers) > 1 {
		return fmt.Errorf("feature: %s both provide moderation",
			strings.Join(providers, " and "))
	}
	for _, instance := range features {
		if aware, ok := instance.(ModerationAware); ok {
			aware.SetModeration(moderation)
		}
	}
	return nil
}

// Intents unions what every feature asked for, which is what the websocket
// connection subscribes to.
func Intents(features []Feature) qqbotsdk.Intent {
	var all qqbotsdk.Intent
	for _, instance := range features {
		all |= instance.Intents()
	}
	return all
}
