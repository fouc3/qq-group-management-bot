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
	"errors"
	"fmt"
	"log/slog"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/onebot-ext/onebot"
	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// Deps is what a feature is handed to do its work.
//
// What is here is what the bot is made of rather than what one feature promises
// another: a feature takes the part it needs, and nothing in this struct can be
// absent because another feature is switched off. A promise one feature makes to
// another is a capability instead, wired in the Inject calls below -- a promise
// can go unkept, and a feature on the receiving end has to be able to say so
// rather than fail later.
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
	// Store is the data layer: what has to outlive a restart.
	//
	// A feature takes the part it needs rather than this package declaring a
	// narrow contract for each operation. A feature that needs its own seam
	// already declares one -- the join-request feature's blacklist is the
	// example, and it is what lets that feature be tested without a database.
	Store store.Store
	// Redis is where the volatile cache lives, with its defaults filled in.
	//
	// Lifted out of the configuration the way Groups is, so that a feature which
	// caches something does not have to read the file.
	Redis config.Redis
	// Buttons is where the feature claims the button presses it answers.
	//
	// One registry is shared by every feature of a bot, because a bot has one
	// connection and therefore one dispatcher: a feature that made its own would
	// answer presses the others also see, and a press carries nothing but its
	// button's data to say whose it is. Build hands one over, and a feature built
	// without it -- which is what a test builds -- gets one of its own.
	Buttons *command.Buttons
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
//
// A feature is built, started and stopped more than once while a bot runs: a
// configuration change builds the feature it names again. What that asks of one is
// spelled out on Close, which is where it is easiest to get wrong.
type Feature interface {
	// Name is the feature's name: its key under features: in the file, and
	// the value of the feature log field.
	Name() string
	// Intents reports what the feature needs to receive. The app unions the
	// answers, so a feature never lists intents that belong to another one.
	Intents() qqbotsdk.Intent
	// Register declares the feature's event handlers.
	Register(ctx context.Context) error
	// Close releases what the feature holds.
	//
	// It may run after a failed or skipped Register, so it must tolerate having
	// nothing to release.
	//
	// It must also leave nothing of the feature running: the handlers it
	// registered are cancelled, the button namespaces it claimed are given back,
	// the work it has in flight is ended, and the waiting for its own goroutines
	// happens here rather than being left to chance. The instance that replaces it
	// is built immediately afterwards, and two instances of one feature on one
	// connection answer every event twice.
	Close(ctx context.Context) error
}

// Factory builds one feature from its own configuration section.
type Factory func(section yaml.Node, deps Deps) (Feature, error)

// enabledKey is the switch any feature section may carry.
const enabledKey = "enabled"

// Registry maps feature names to the factories that build them.
//
// It is the table and nothing more: what a feature is called and how one is
// built, not which of them are running. Assembling them -- and building one again
// later, which is what a reload amounts to -- belongs to the app, where the
// infrastructure they are handed is built.
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

// Factory returns how one registered feature is built.
//
// A name that was never registered is nearly always a typo in the configuration,
// and the caller is expected to say so: ignoring it would leave an operator
// believing a feature runs while it does not.
func (r *Registry) Factory(name string) (Factory, bool) {
	factory, known := r.factories[name]
	return factory, known
}

// Enabled reports whether a feature's section turns it on.
//
// A section that is written out without the switch is on: writing it is already
// a statement of intent. Only an explicit enabled: false turns it off.
func Enabled(name string, section yaml.Node) (bool, error) {
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
func InjectAdminDirectory(features []Feature, logger *slog.Logger) error {
	directory, providedBy, err := FindProvider[AdminDirectory](features, "the administrator list")
	if err != nil {
		return err
	}
	Inject[AdminDirectory, AdminAware](features, directory, providedBy,
		"the administrator list", logger, AdminAware.SetAdminDirectory)
	return nil
}

// Commands is the bot's command table, as a question another feature can ask it.
//
// Only the table knows what a command looks like, because only the table knows the
// prefix it is written with and the words it holds. A feature that is waiting for
// free text from a member has to ask rather than guess: a command typed in the
// middle of that has to be answered, not swallowed as the text that was being
// waited for.
type Commands interface {
	// LooksLikeACommand reports whether a message is an attempt at a command.
	//
	// An attempt, not a command the table holds: a word that was mistyped is still
	// somebody asking the bot to do something, and answering it with the help is
	// what the bot does everywhere else.
	LooksLikeACommand(content string) bool
}

// CommandAware is implemented by a feature that has to tell a command from what a
// member typed.
type CommandAware interface {
	SetCommands(Commands)
}

// InjectCommands hands the command table to every feature that has to ask it.
func InjectCommands(features []Feature, logger *slog.Logger) error {
	commands, providedBy, err := FindProvider[Commands](features, "the command table")
	if err != nil {
		return err
	}
	Inject[Commands, CommandAware](features, commands, providedBy,
		"the command table as a question", logger, CommandAware.SetCommands)
	return nil
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
	// quotedText is what the quote showed of the message it points at. It is a
	// **locator and nothing else**: a quote of a message that is itself a quote comes
	// with a temporary index the cache can never hold, so the text is how that
	// message gets found. It is never sent for judging -- what is judged is the
	// message that was found, and it is judged as its author's own words.
	//
	// An error means no judgement was reached: the message is not in the cache,
	// the cache is down, or the model could not be read. None of those is a
	// violation, and a caller must never treat one as a violation.
	JudgeQuoted(ctx context.Context, groupOpenID, quotedIndex, quotedText,
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
	//
	// The outcome carries one line per message that was to be taken back, because
	// that is the part of a punishment nobody can check afterwards: the message
	// itself is gone by the time the question is asked.
	RecordOutcome(ctx context.Context, judgementID string, outcome store.Outcome) error
	// MarkPunished records that messages have been dealt with.
	//
	// Only messages that were actually taken back belong here, and only those:
	// one that stayed in the group is still there to be reported and looked at
	// again, while one that was withdrawn cannot be looked at a second time and
	// must never be judged again. Without this, a message that has just been
	// withdrawn arrives in the next report's window as if nothing had happened to
	// it -- which is how one advertisement came to be withdrawn twice and its
	// author silenced three times in three minutes.
	//
	// messageIndexes are the platform indexes of the messages, which is how the
	// cache addresses them.
	MarkPunished(ctx context.Context, groupOpenID string, messageIndexes []string) error
	// LabelFor is the configured display name for a category.
	//
	// The label is the word a group was told, and the configuration that holds it
	// belongs to this feature, so a caller that has recorded a category can say
	// that word about it without keeping a second copy of the configuration.
	LabelFor(category string) string
}

// JudgedMessage is one message of the window a judge was shown, named the three
// ways the rest of the bot needs it: by id to act on it, by index to find it in
// the cache, and by text because a withdrawal leaves nothing else behind.
type JudgedMessage struct {
	// ID is the platform message id, which is what a recall takes.
	ID string
	// Index is the platform message index, which is what the cache is keyed by.
	Index string
	// Number is the position in the window as the judge saw it, for the log.
	Number int
	// Text is what the message said.
	Text string
}

// ErrAlreadyPunished reports a report about a message that has already been
// withdrawn.
//
// It is a sentinel in this package rather than inside the moderation feature
// because the caller has to recognise it: "this was dealt with already" is an
// answer to give the group, and it is not the same answer as "the judgement
// failed", which is what every other error from the seam means.
var ErrAlreadyPunished = errors.New("the message has already been taken back")

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
	// RecallMessages are the messages to take back.
	//
	// The judge names them, and the feature that owns the cache turns the names
	// into messages: only ones that exist in the window and belong to the member
	// being judged survive that, because a model can name a message that is not
	// there, or one that is somebody else's.
	//
	// Each one carries its text as well as its id: the caller withdraws the
	// message, and the record of what was withdrawn has to hold the words, since
	// after a successful withdrawal the group no longer has them either.
	RecallMessages []JudgedMessage
	// JudgedMessageIDs is everything that was sent for judgement, for the record.
	JudgedMessageIDs []string
	// Reason is the model's own explanation, for the administrators and the audit
	// table and never for the group.
	Reason string
	// Reasoning is the model's chain of thought, when one was kept. Also for the
	// administrators and the record: it is what makes a surprising verdict
	// answerable.
	Reasoning string
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
func InjectVerifier(features []Feature, logger *slog.Logger) error {
	verifier, providedBy, err := FindProvider[Verifier](features, "verification")
	if err != nil {
		return err
	}
	Inject[Verifier, VerifierAware](features, verifier, providedBy, "verification",
		logger, VerifierAware.SetVerifier)
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
func InjectModeration(features []Feature, logger *slog.Logger) error {
	moderation, providedBy, err := FindProvider[Moderation](features, "moderation")
	if err != nil {
		return err
	}
	Inject[Moderation, ModerationAware](features, moderation, providedBy, "moderation",
		logger, ModerationAware.SetModeration)
	return nil
}

// CommandSource is a feature that has commands of its own to add to the group's
// table.
//
// There is one table rather than one per feature, so that the help a group is
// shown, the menu it taps and the dispatcher that runs a command cannot come
// apart -- and one table means somebody owns it and the others contribute. That
// is this contract: the owner keeps the table, and a feature that answers commands
// offers them.
//
// What a source hands over is read again whenever the features are wired, which
// is after any of them is built again. An entry kept from the last wiring would
// hold a runner belonging to an instance nobody is running.
type CommandSource interface {
	// CommandDefs are the commands this feature answers.
	CommandDefs() []command.Def
}

// CommandSourceAware is implemented by the feature that keeps the table.
//
// It is the one wiring whose consumer can refuse what it is handed: two commands
// answering to one word is a mistake in the table, and the table is the only place
// that can see it -- the sources do not know about each other.
type CommandSourceAware interface {
	SetCommandSources([]command.Def) error
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
