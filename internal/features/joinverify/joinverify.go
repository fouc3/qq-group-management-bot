// Package joinverify holds a member who has just joined a group until they
// prove they are a person by pressing a button in that group.
//
// The whole exchange stays inside the group, which is what makes it work on
// this platform: a private message would need the member to have added the bot
// first, while a button press reports both the group and the member that
// pressed it, so nobody has to be messaged privately at all.
//
// A member who never presses the button is held for the configured mute and
// then reported, and optionally removed, for the administrators.
package joinverify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/onebot-ext/onebot"
	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// Name is the feature's name: its configuration key and its log field.
const Name = "join_verification"

// buttonPrefix marks a button as ours, so another feature's button data is
// never mistaken for a verification token.
const buttonPrefix = "v:"

// LongestMuteMinutes is the platform's ceiling for one mute: thirty days.
const LongestMuteMinutes = 30 * 24 * 60

// Defaults for a section that leaves a field out.
const (
	// DefaultMuteMinutes holds a new member for twenty nine days: just inside
	// the platform's thirty day ceiling, so a stranger cannot wait it out and
	// then speak.
	DefaultMuteMinutes = 29 * 24 * 60
	// DefaultDeadlineHours is how long a member has to verify before the
	// administrators are told about them.
	DefaultDeadlineHours = 48

	DefaultPrompt = "{at}\n**欢迎新成员！**\n请点击下方按钮完成验证。"
	// DefaultNotifyMessage mentions the configured administrators and the
	// member who has not verified.
	DefaultNotifyMessage = "{at} 有成员进群 {hours} 小时仍未完成验证，请处理：{target}"

	DefaultButtonLabel  = "✅ 我是真人"
	DefaultVisitedLabel = "已验证"
	DefaultPassMessage  = "验证通过，欢迎你！"
	// DefaultSkipMessage is posted when an administrator skips a member.
	DefaultSkipMessage = "管理员已跳过 {target} 的验证"

	// tokenBytes is the size of a button token. Eight bytes is far more than
	// a guessing game inside a group needs, and keeps the button data short.
	tokenBytes = 8

	// sweepInterval is how often deadlines and expired holds are checked.
	sweepInterval = time.Minute
)

// What to do with a member who misses the deadline.
const (
	// OnDeadlineNone does nothing.
	OnDeadlineNone = "none"
	// OnDeadlineNotify mentions the configured administrators.
	OnDeadlineNotify = "notify"
	// OnDeadlineRemove removes the member once the deadline passes.
	//
	// The administrators are deliberately not mentioned in this mode: the
	// removal is automatic, so there is nothing for them to do, and mentioning
	// them on every timeout would be noise.
	//
	// A removal that fails is only logged today, which is a known hole: the
	// member stays in the group and nobody is told. notify_members is still
	// required by the validation, so a report has somewhere to go once that is
	// closed.
	OnDeadlineRemove = "remove"
	// OnDeadlineRemute holds the member again at the deadline instead of
	// removing them: both the deadline and the mute's expiry move forward, so
	// the check repeats for as long as they stay and nobody is ever removed for
	// not verifying.
	//
	// It is a removal with another hold in place of the removal, which is why it
	// shares the deadline the other modes act on.
	OnDeadlineRemute = "remute"
	// muteRetryDelay is how long to wait before trying to renew a hold that
	// could not be renewed, so a member who cannot be muted at all -- a group
	// administrator, which the platform refuses with 40103004 -- is not retried
	// every minute for as long as they stay.
	muteRetryDelay = time.Hour
)

// Which side performs a removal.
const (
	// BackendOneBot hands the removal to an OneBot account, which can remove
	// members as long as it administers the group.
	BackendOneBot = "onebot"
	// BackendOfficial asks the official API. Production answers 40012010
	// "应用无接口访问权限" for the removal endpoint even when the bot
	// administers the group, so this is kept only for applications that were
	// granted the permission.
	BackendOfficial = "official"
)

// How a member who has not verified is held back.
const (
	// MuteModeMute silences the member through the platform: a real mute, applied
	// for the configured mute_minutes and lifted the moment they verify.
	//
	// It is the default, and it is the stronger of the two: the platform refuses
	// the message instead of the bot taking it back, so nothing of what they said
	// was ever readable by the group.
	MuteModeMute = "mute"
	// MuteModePseudo leaves the platform alone and takes the member's messages
	// back as they arrive, keeping them out of the message cache at the same
	// time. Nobody is muted.
	//
	// It exists because a mute is not always available or not always wanted: the
	// platform refuses to mute a group administrator with 40103004, a mute is
	// visible in the group's member list and on the member's own client, and a
	// group may simply prefer that the bot not silence anybody at all. What it
	// costs is said out loud at startup, because none of it is visible from the
	// inside: the message does reach the group for as long as the recall takes, a
	// recall of somebody else's message needs the bot to administer the group,
	// and a take-back needs the message to arrive at all -- a group set to
	// deliver only mentions gives this mode nothing to work with.
	//
	// A pseudo-mute has no expiry of its own: it lasts until the member verifies,
	// which is also why mute_minutes and on_deadline: remute say nothing in this
	// mode.
	MuteModePseudo = "pseudo"
)

// Config is this feature's section in the configuration file.
type Config struct {
	// Enabled turns the switch on. A section written without it is on.
	Enabled bool `yaml:"enabled"`
	// StateFile is where the members who are still waiting to verify are
	// recorded, so a restart does not forget them. It should be an absolute
	// path; empty turns persistence off, which leaves every hold unaccounted
	// for after a restart.
	StateFile string `yaml:"state_file"`
	// MuteMinutes is how long a new member is held back. The default sits just
	// inside the platform's thirty day ceiling. It says nothing in the
	// pseudo-mute mode, where the hold lasts until the member verifies.
	MuteMinutes int `yaml:"mute_minutes"`
	// MuteMode is how a member who has not verified is held back: mute has the
	// platform silence them, pseudo takes their messages back as they arrive and
	// keeps them out of the message cache. Empty means mute, which is what every
	// deployment did before pseudo existed.
	MuteMode string `yaml:"mute_mode"`
	// DeadlineHours is how long they have to verify before the administrators
	// are told. Under a real mute it should be far shorter than MuteMinutes,
	// which is what the validation asks of that mode and not of the pseudo-mute.
	DeadlineHours int `yaml:"deadline_hours"`
	// OnDeadline is none, notify or remove.
	OnDeadline string `yaml:"on_deadline"`
	// RemoveBackend is onebot or official, and only matters when OnDeadline is
	// remove.
	RemoveBackend string `yaml:"remove_backend"`
	// NotifyMembers are the group member openids to mention when a deadline
	// passes.
	NotifyMembers []string `yaml:"notify_members"`
	// NotifyMessage is the message posted when a deadline passes. It may use
	// {at} for the mentioned administrators, {target} for the member who has
	// not verified, and {hours} for the deadline.
	NotifyMessage string `yaml:"notify_message"`
	// MentionNewMember puts an at-mention of the new member into the prompt.
	MentionNewMember bool `yaml:"mention_new_member"`
	// Prompt is the markdown shown with the button. {at} is where the mention
	// of the new member goes.
	Prompt string `yaml:"prompt"`
	// ButtonLabel is the text on the button.
	ButtonLabel string `yaml:"button_label"`
	// VisitedLabel is the text shown after a press.
	VisitedLabel string `yaml:"visited_label"`
	// PassMessage is sent in the group once verification succeeds. Empty
	// sends nothing.
	PassMessage string `yaml:"pass_message"`
	// SkipMessage is sent when an administrator skips a member's verification
	// by pressing that member's button. {target} is the member's mention.
	SkipMessage string `yaml:"skip_message"`
	// DryRun sends the prompt and answers the button but never mutes, unmutes
	// or removes, for trying the flow on a live group without silencing anyone.
	DryRun bool `yaml:"dry_run"`
	// Groups overrides the fields above for named groups, keyed by group
	// openid.
	//
	// The fields above are the defaults for every group; a group entry changes
	// only what it names. Two groups rarely want the same rules once one of
	// them is a large public group, and one global setting cannot express that.
	Groups map[string]Override `yaml:"groups"`
}

// Override is one group's settings. A nil field inherits the default.
//
// Every field is a pointer so that "not written" is distinguishable from
// "written as the zero value": a group that only turns the removal on has to
// keep the shared prompt, labels and timings.
type Override struct {
	MuteMinutes      *int      `yaml:"mute_minutes"`
	MuteMode         *string   `yaml:"mute_mode"`
	DeadlineHours    *int      `yaml:"deadline_hours"`
	OnDeadline       *string   `yaml:"on_deadline"`
	RemoveBackend    *string   `yaml:"remove_backend"`
	NotifyMembers    *[]string `yaml:"notify_members"`
	NotifyMessage    *string   `yaml:"notify_message"`
	MentionNewMember *bool     `yaml:"mention_new_member"`
	Prompt           *string   `yaml:"prompt"`
	ButtonLabel      *string   `yaml:"button_label"`
	VisitedLabel     *string   `yaml:"visited_label"`
	PassMessage      *string   `yaml:"pass_message"`
	SkipMessage      *string   `yaml:"skip_message"`
	DryRun           *bool     `yaml:"dry_run"`
}

// Settings is one group's settings, after the defaults and that group's
// overrides have been merged.
type Settings struct {
	MuteMinutes      int
	MuteMode         string
	DeadlineHours    int
	OnDeadline       string
	RemoveBackend    string
	NotifyMembers    []string
	NotifyMessage    string
	MentionNewMember bool
	Prompt           string
	ButtonLabel      string
	VisitedLabel     string
	PassMessage      string
	SkipMessage      string
	DryRun           bool
}

// defaults returns the settings every group starts from.
func (c *Config) defaults() Settings {
	return Settings{
		MuteMinutes:      c.MuteMinutes,
		MuteMode:         c.MuteMode,
		DeadlineHours:    c.DeadlineHours,
		OnDeadline:       c.OnDeadline,
		RemoveBackend:    c.RemoveBackend,
		NotifyMembers:    c.NotifyMembers,
		NotifyMessage:    c.NotifyMessage,
		MentionNewMember: c.MentionNewMember,
		Prompt:           c.Prompt,
		ButtonLabel:      c.ButtonLabel,
		VisitedLabel:     c.VisitedLabel,
		PassMessage:      c.PassMessage,
		SkipMessage:      c.SkipMessage,
		DryRun:           c.DryRun,
	}
}

// settingsFor returns one group's settings, with that group's overrides applied.
func (c *Config) settingsFor(groupOpenID string) Settings {
	settings := c.defaults()
	override, known := c.Groups[groupOpenID]
	if !known {
		return settings
	}
	if override.MuteMinutes != nil {
		settings.MuteMinutes = *override.MuteMinutes
	}
	if override.MuteMode != nil {
		settings.MuteMode = *override.MuteMode
	}
	if override.DeadlineHours != nil {
		settings.DeadlineHours = *override.DeadlineHours
	}
	if override.OnDeadline != nil {
		settings.OnDeadline = *override.OnDeadline
	}
	if override.RemoveBackend != nil {
		settings.RemoveBackend = *override.RemoveBackend
	}
	if override.NotifyMembers != nil {
		settings.NotifyMembers = *override.NotifyMembers
	}
	if override.NotifyMessage != nil {
		settings.NotifyMessage = *override.NotifyMessage
	}
	if override.MentionNewMember != nil {
		settings.MentionNewMember = *override.MentionNewMember
	}
	if override.Prompt != nil {
		settings.Prompt = *override.Prompt
	}
	if override.ButtonLabel != nil {
		settings.ButtonLabel = *override.ButtonLabel
	}
	if override.VisitedLabel != nil {
		settings.VisitedLabel = *override.VisitedLabel
	}
	if override.PassMessage != nil {
		settings.PassMessage = *override.PassMessage
	}
	if override.SkipMessage != nil {
		settings.SkipMessage = *override.SkipMessage
	}
	if override.DryRun != nil {
		settings.DryRun = *override.DryRun
	}
	return settings
}

// asConfig turns merged settings back into a Config, so the same rules can be
// checked against them instead of duplicating every check.
func (s Settings) asConfig() *Config {
	return &Config{
		MuteMinutes:      s.MuteMinutes,
		MuteMode:         s.MuteMode,
		DeadlineHours:    s.DeadlineHours,
		OnDeadline:       s.OnDeadline,
		RemoveBackend:    s.RemoveBackend,
		NotifyMembers:    s.NotifyMembers,
		NotifyMessage:    s.NotifyMessage,
		MentionNewMember: s.MentionNewMember,
		Prompt:           s.Prompt,
		ButtonLabel:      s.ButtonLabel,
		VisitedLabel:     s.VisitedLabel,
		PassMessage:      s.PassMessage,
		SkipMessage:      s.SkipMessage,
		DryRun:           s.DryRun,
	}
}

// pseudoHolds reports whether this group holds a member back by taking their
// messages back rather than by muting them.
//
// It is asked wherever the two modes differ, and it is deliberately about the
// mode alone: a dry run is a separate question, answered where the hold is
// actually applied and where a message is actually recalled.
func (s Settings) pseudoHolds() bool {
	return s.MuteMode == MuteModePseudo
}

// anyPseudoHolds reports whether any group this configuration covers is held
// under the pseudo-mute.
//
// It is what the declared intents are read from: a take-back is driven by the
// message events, so a bot whose groups are held this way has to subscribe to
// them itself instead of relying on another feature happening to want them.
func (c *Config) anyPseudoHolds() bool {
	if c.defaults().pseudoHolds() {
		return true
	}
	for groupOpenID := range c.Groups {
		if c.settingsFor(groupOpenID).pseudoHolds() {
			return true
		}
	}
	return false
}

// validateGroups checks every group's merged settings at startup.
//
// It matters more here than for the defaults: an override that asks for no
// notification while removing people, or a duration the platform would refuse,
// has to be found when the file is read rather than when a member has already
// joined.
func (c *Config) validateGroups() error {
	for groupOpenID := range c.Groups {
		if strings.TrimSpace(groupOpenID) == "" {
			return errors.New("groups has an entry with an empty group openid")
		}
		if err := c.settingsFor(groupOpenID).asConfig().applyDefaults(); err != nil {
			return fmt.Errorf("groups[%s]: %w", groupOpenID, err)
		}
	}
	return nil
}

// applyDefaults fills in what the section leaves out, then validates it.
func (c *Config) applyDefaults() error {
	if c.MuteMinutes == 0 {
		c.MuteMinutes = DefaultMuteMinutes
	}
	if c.DeadlineHours == 0 {
		c.DeadlineHours = DefaultDeadlineHours
	}
	if strings.TrimSpace(c.OnDeadline) == "" {
		c.OnDeadline = OnDeadlineNotify
	}
	if strings.TrimSpace(c.RemoveBackend) == "" {
		c.RemoveBackend = BackendOneBot
	}
	if strings.TrimSpace(c.MuteMode) == "" {
		// Every file written before this field existed asked for a real mute,
		// and writing the section out is not a way to ask for something else.
		c.MuteMode = MuteModeMute
	}
	if strings.TrimSpace(c.Prompt) == "" {
		c.Prompt = DefaultPrompt
	}
	if c.NotifyMessage == "" {
		c.NotifyMessage = DefaultNotifyMessage
	}
	if strings.TrimSpace(c.ButtonLabel) == "" {
		c.ButtonLabel = DefaultButtonLabel
	}
	if strings.TrimSpace(c.VisitedLabel) == "" {
		c.VisitedLabel = DefaultVisitedLabel
	}
	if c.PassMessage == "" {
		c.PassMessage = DefaultPassMessage
	}
	if c.SkipMessage == "" {
		c.SkipMessage = DefaultSkipMessage
	}
	// A prompt that starts with the mention is what an operator writes when
	// they want the member addressed, so the switch follows the text.
	if strings.Contains(c.Prompt, "{at}") {
		c.MentionNewMember = true
	}

	switch {
	case c.MuteMinutes < 1:
		return fmt.Errorf("mute_minutes must be at least 1, got %d", c.MuteMinutes)
	case c.MuteMinutes > LongestMuteMinutes:
		return fmt.Errorf("mute_minutes must be at most %d (thirty days), got %d",
			LongestMuteMinutes, c.MuteMinutes)
	case c.DeadlineHours < 1:
		return fmt.Errorf("deadline_hours must be at least 1, got %d", c.DeadlineHours)
	}
	// The room rule applies to every mode that acts on a deadline, remute
	// included: the deadline is when the member is held again, so it has to come
	// before the mute runs out, or there would be a stretch in which they are
	// free to talk before the next hold lands.
	//
	// It is not asked of the pseudo-mute, where the hold has no expiry at all:
	// nothing runs out for the deadline to beat, and a group may well report
	// somebody after a week of holding them.
	if c.MuteMode != MuteModePseudo && c.DeadlineHours*60 >= c.MuteMinutes {
		return fmt.Errorf("deadline_hours (%d) does not leave room inside mute_minutes (%d), "+
			"so a reported member would already have been released",
			c.DeadlineHours, c.MuteMinutes)
	}
	switch c.MuteMode {
	case MuteModeMute, MuteModePseudo:
	default:
		return fmt.Errorf("mute_mode %q is not %s or %s",
			c.MuteMode, MuteModeMute, MuteModePseudo)
	}
	switch c.OnDeadline {
	case OnDeadlineNone, OnDeadlineNotify, OnDeadlineRemove, OnDeadlineRemute:
	default:
		return fmt.Errorf("on_deadline %q is not %s, %s, %s or %s",
			c.OnDeadline, OnDeadlineNone, OnDeadlineNotify, OnDeadlineRemove,
			OnDeadlineRemute)
	}
	switch c.RemoveBackend {
	case BackendOneBot, BackendOfficial:
	default:
		return fmt.Errorf("remove_backend %q is not %s or %s",
			c.RemoveBackend, BackendOneBot, BackendOfficial)
	}
	if c.OnDeadline == OnDeadlineRemove && len(c.NotifyMembers) == 0 {
		return fmt.Errorf("on_deadline: %s should name who to tell in notify_members, "+
			"otherwise nobody learns that a member was removed", OnDeadlineRemove)
	}
	if len([]rune(c.ButtonLabel)) > 10 {
		return fmt.Errorf("button_label must be at most 10 characters, got %d",
			len([]rune(c.ButtonLabel)))
	}
	// A group override is held to the same rules as the defaults, and the check
	// runs here so a broken file fails at startup rather than when a member has
	// already joined.
	return c.validateGroups()
}

// New builds the feature from its configuration section.
func New(section yaml.Node, deps feature.Deps) (feature.Feature, error) {
	var cfg Config
	if err := section.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("reading the %s section: %w", Name, err)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	instance := &verifier{
		cfg:       cfg,
		deps:      deps,
		buttons:   command.ButtonsOrOwn(deps.Buttons),
		byToken:   map[string]*pending{},
		byMember:  map[string]*pending{},
		stateFile: cfg.StateFile,
		stopping:  make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	if deps.Store != nil {
		instance.pending = deps.Store.Pending()
		instance.meta = deps.Store.Meta()
	}
	instance.part, instance.stopPart = context.WithCancel(context.Background())
	// Holds that outlived an earlier process are restored here, before anything
	// can answer a button or sweep a deadline for them. The older JSON file is
	// imported first, so the changeover does not forget anybody.
	restoreCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	instance.restore(restoreCtx, time.Now())
	cancel()
	return instance, nil
}

// pending is one member waiting to prove they are a person.
type pending struct {
	token        string
	groupOpenID  string
	memberOpenID string
	// joinedAt is the join time the official platform reported, as a Unix
	// timestamp. It is what identifies the same person in OneBot's member
	// list, since an openid means nothing there.
	joinedAt int64
	// deadline is when the administrators are told about this member.
	deadline time.Time
	// heldUntil is when the platform releases the mute by itself.
	heldUntil time.Time
	// reported records that the deadline has already been acted on, so the
	// administrators are told once rather than every sweep.
	reported bool
	// settings are the group's settings at the moment the member was held.
	//
	// They travel with the entry so that the deadline and the button press act
	// on the rules the member was actually held under, and so that no handler
	// has to work out which group it is dealing with.
	settings Settings
}

// verifier implements feature.Feature.
type verifier struct {
	cfg  Config
	deps feature.Deps

	mu sync.Mutex
	// byToken answers a button press.
	byToken map[string]*pending
	// byMember supersedes an older entry for the same member, so a member who
	// leaves and rejoins does not accumulate buttons.
	byMember map[string]*pending

	// admins is shared by the app, so an administrator pressing somebody
	// else's button can be told apart from a stranger pressing it.
	admins feature.AdminDirectory

	// stateFile is where the pending list is written, empty when persistence is
	// off.
	stateFile string

	// pending and meta are the data layer. They are narrow slices of it rather
	// than the whole store: this feature keeps the members it is holding, and
	// nothing else.
	//
	// Both are nil when the app passes no store. The feature then runs with
	// nothing written down, which is what the tests want and what a deployment
	// without a database would get.
	pending store.PendingStore
	meta    store.MetaStore

	// part is cancelled when this feature stops, and everything that answers a
	// press runs under it: an interaction has to be answered even though the event
	// that carried the press is over, and it must not be answered by an instance
	// that has already been replaced.
	part     context.Context
	stopPart context.CancelFunc

	// buttons is where the verification button's namespace is claimed. It is
	// shared with the other features that have buttons, so a press reaches one of
	// them, and the claim is taken back in Close so that this feature can be built
	// again while the bot runs.
	buttons *command.Buttons

	registrations []*qqbotsdk.Registration
	stopping      chan struct{}
	stopped       chan struct{}
	closeOnce     sync.Once
}

// SetAdminDirectory implements feature.AdminAware.
func (v *verifier) SetAdminDirectory(directory feature.AdminDirectory) {
	v.admins = directory
}

// Name implements feature.Feature.
func (v *verifier) Name() string { return Name }

// Intents implements feature.Feature.
func (v *verifier) Intents() qqbotsdk.Intent {
	// Group member events announce the join; interaction events report the
	// button press.
	intents := qqbotsdk.IntentGroupMemberEvent | qqbotsdk.IntentInteraction
	// A pseudo-mute is applied to the messages themselves, so the group's
	// messages have to reach this feature and not merely the others that want
	// them: without them there is nothing to take back, and the member talks as
	// freely as anybody else while still looking held.
	if v.cfg.anyPseudoHolds() {
		intents |= qqbotsdk.IntentGroupAndC2CEvent
	}
	return intents
}

// Register implements feature.Feature.
func (v *verifier) Register(_ context.Context) error {
	v.registrations = append(v.registrations,
		v.deps.Client.RegisterFunc(qqbotsdk.EventGroupMemberAdd, v.onMemberAdd),
		// A held member's messages are watched in both modes, for two different
		// reasons. Under the pseudo-mute they are how the hold is applied at
		// all: nothing else stops them talking. Under a real mute they only
		// close the gap between the join and the mute landing, which the
		// platform does not make instant. Both event types are registered
		// because which one carries a group message depends on the group's
		// receive setting, not on the message.
		v.deps.Client.RegisterFunc(qqbotsdk.EventGroupMessageCreate, v.onHeldMemberMessage),
		v.deps.Client.RegisterFunc(qqbotsdk.EventGroupAtMessageCreate, v.onHeldMemberMessage),
	)
	// The verification button is claimed by the namespace its data carries rather
	// than looked for in every press. A press arrives with nothing but its
	// button's data to say whose it is, so the registry is what settles that once
	// instead of every feature deciding for itself.
	//
	// The dispatcher it declares is deliberately not in v.registrations: it is
	// shared with the other features that claim buttons, so cancelling it here
	// would take their presses with it. What Close gives back instead is the
	// claim itself, which is what lets this feature be built again while the bot
	// runs.
	if err := v.buttons.Set(Name, []command.ButtonClaim{{
		Namespace: buttonPrefix,
		// A verification button is only ever put in a group, and a press the
		// platform reports as coming from a single chat is not ours to act on.
		Scenes: command.InGroup,
		Handle: v.onInteractionPress,
	}}); err != nil {
		return err
	}
	v.buttons.Register(v.deps.Client)
	v.warnAboutRemoval()
	v.warnAboutPseudoHold()
	go v.sweep()
	// One line per group, because the rules are per group now.
	v.eachSettings(func(label string, settings Settings) {
		v.deps.Logger.Info("holding new members for verification",
			"group", label,
			"mute_mode", settings.MuteMode,
			"mute_minutes", settings.MuteMinutes,
			"deadline_hours", settings.DeadlineHours,
			"on_deadline", settings.OnDeadline,
			"notify_members", len(settings.NotifyMembers))
	})
	return nil
}

// warnAboutRemoval says at startup what a removal would and would not do,
// rather than only failing once a deadline has already passed.
func (v *verifier) warnAboutRemoval() {
	v.eachSettings(func(label string, settings Settings) {
		switch {
		case settings.OnDeadline != OnDeadlineRemove:
			return
		case settings.DryRun:
			v.deps.Logger.Warn("dry run is on: nobody will be muted or removed",
				"group", label)
		case settings.RemoveBackend == BackendOneBot && v.deps.OneBot == nil:
			v.deps.Logger.Error("on_deadline: remove is configured with the onebot backend, "+
				"but no onebot url is set, so a member who misses the deadline can "+
				"only be reported", "group", label)
		case settings.RemoveBackend == BackendOfficial:
			v.deps.Logger.Warn("on_deadline: remove uses the official backend, which the "+
				"platform refused with 40012010 on this application; the onebot "+
				"backend is the one that works without an extra permission",
				"group", label)
		}
	})
}

// warnAboutPseudoHold says at startup what holding somebody without muting them
// rests on, because none of it is visible from the inside.
//
// Both conditions are the operator's to fix, and both fail silently: the take-back
// needs the bot to administer the group -- a recall of somebody else's message is
// refused otherwise -- and it needs the message to arrive at all, which a group set
// to deliver only mentions never does. Without either, the member is held in this
// feature's books and talks freely in the group, which reads as the verification
// having quietly stopped working.
func (v *verifier) warnAboutPseudoHold() {
	v.eachSettings(func(label string, settings Settings) {
		if !settings.pseudoHolds() {
			return
		}
		v.deps.Logger.Info("holding members without muting them: a message from "+
			"somebody who has not verified is taken back as it arrives and is left "+
			"out of the message cache", "group", label)
		v.deps.Logger.Warn("the pseudo-mute needs the bot to administer the group and "+
			"the group to deliver all messages; if a member who has not verified is "+
			"seen talking, one of the two is missing",
			"group", label, "what_to_check", "the bot's role, and the group's bot settings")
	})
}

// eachSettings reports the settings of every group the file configures, so the
// startup checks and the startup summary cover each group rather than only the
// settings a group happens to inherit.
//
// With no group entries there is only the default set to report.
func (v *verifier) eachSettings(report func(label string, settings Settings)) {
	if len(v.cfg.Groups) == 0 {
		report("(defaults)", v.cfg.defaults())
		return
	}
	for groupOpenID := range v.cfg.Groups {
		report(groupOpenID, v.cfg.settingsFor(groupOpenID))
	}
}

// Close implements feature.Feature.
//
// It stops answering, gives the button namespace back, and waits for the sweep to
// finish -- in that order, because a rebuild that started while the old instance
// was still sweeping would have two of them acting on the same holds. The waiting
// is what makes this feature rebuildable rather than merely stoppable.
func (v *verifier) Close(_ context.Context) error {
	for _, registration := range v.registrations {
		registration.Cancel()
	}
	v.registrations = nil
	v.buttons.Release(Name)
	v.stopPart()
	v.closeOnce.Do(func() { close(v.stopping) })
	<-v.stopped
	return nil
}

// sweep acts on deadlines and forgets entries the platform has released.
func (v *verifier) sweep() {
	defer close(v.stopped)
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-v.stopping:
			return
		case <-ticker.C:
			due, renew := v.takeDueDeadlines(time.Now())
			// Renewals come first: they concern somebody who is able to talk
			// again right now, while a report can wait for the next tick.
			if len(renew) > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				for _, entry := range renew {
					v.renew(ctx, entry)
				}
				cancel()
			}
			for _, entry := range due {
				v.handleDeadline(entry)
			}
		}
	}
}

// takeDueDeadlines returns the entries whose deadline has passed, and the
// entries whose hold the platform has just released and whose group asks for a
// renewal instead.
//
// The two are returned separately because they lead to opposite actions: one
// tells an administrator about somebody, the other silences them again. Neither
// is performed here, since both need to talk to the platform and this holds the
// lock.
func (v *verifier) takeDueDeadlines(now time.Time) (due, renew []*pending) {
	v.mu.Lock()
	defer v.mu.Unlock()

	for token, entry := range v.byToken {
		if entry.settings.pseudoHolds() {
			// A pseudo-mute has no expiry of its own, so nothing here ever
			// releases anybody: the hold ends when the member verifies, or when
			// a removal takes them out of the group.
			//
			// remute asks for the hold to be applied again and there is nothing
			// to apply -- the group is already held until they verify -- so it
			// comes to the same thing as none. notify and remove are the two that
			// act on a deadline, and they act on it exactly as they do under a
			// real mute.
			switch entry.settings.OnDeadline {
			case OnDeadlineNotify, OnDeadlineRemove:
			default:
				continue
			}
			if entry.reported || now.Before(entry.deadline) {
				continue
			}
			entry.reported = true
			// Recorded before the work is attempted, so a restart in the middle
			// does not report or remove the same member twice.
			if v.pending != nil {
				if err := v.pending.MarkReported(context.Background(), token, now); err != nil {
					v.deps.Logger.Error("could not record a report in the data layer",
						"token", token, "error", err)
				}
			}
			copied := *entry
			due = append(due, &copied)
			continue
		}
		if entry.settings.OnDeadline == OnDeadlineRemute {
			// The same moment a removal would happen, but the member is held
			// again instead: the deadline is pushed forward and the mute is
			// applied afresh, so the check repeats rather than ending in a
			// removal.
			//
			// Nothing here looks at the hold's own expiry. The deadline is
			// always the earlier of the two, so renewing at the deadline keeps
			// the hold from ever running out.
			if !now.Before(entry.deadline) {
				copied := *entry
				renew = append(renew, &copied)
			}
			continue
		}
		switch {
		case !now.Before(entry.heldUntil):
			// The platform has released this member, so there is nothing left
			// to do or to remember.
			v.forgetLocked(token)
		case !entry.reported && !now.Before(entry.deadline):
			entry.reported = true
			// Recorded before the work is attempted, so a restart in the middle
			// does not report the same member to the administrators twice.
			if v.pending != nil {
				if err := v.pending.MarkReported(context.Background(), token, now); err != nil {
					v.deps.Logger.Error("could not record a report in the data layer",
						"token", token, "error", err)
				}
			}
			copied := *entry
			due = append(due, &copied)
		}
	}
	return due, renew
}

// renew holds a member again after the platform released their mute.
//
// It is what makes "no timeout" work: a member who never verifies stays silent
// until they do, without ever being removed. Their token is left alone, so the
// button they already have keeps working.
//
// The sweep only renews a real mute, because that is the only hold that runs out.
// A pseudo-mute reaching here -- which is the debug command asking for the
// deadline by hand -- moves the deadline and nothing else: there is no expiry to
// push forward, and the hold goes on either way.
func (v *verifier) renew(ctx context.Context, entry *pending) {
	log := v.deps.Logger.With("group", entry.groupOpenID, "member", entry.memberOpenID)
	now := time.Now()

	// Both stamps move forward and the mute is applied again, which is the same
	// shape as a removal with the removal replaced by another hold.
	//
	// Counting both from now is what makes the cycle repeat: the next deadline
	// is a whole deadline_hours away, so the member is checked again then, and
	// the mute outlives that deadline by construction.
	deadline := now.Add(time.Duration(entry.settings.DeadlineHours) * time.Hour)
	var heldUntil time.Time
	if !entry.settings.pseudoHolds() {
		heldUntil = now.Add(time.Duration(entry.settings.MuteMinutes) * time.Minute)
	}

	if err := v.mute(ctx, entry.groupOpenID, entry.memberOpenID, heldUntil,
		entry.settings); err != nil {
		// The record is kept even so, because it is also the button's record and
		// dropping it would leave the member unable to verify at all. Only the
		// deadline moves, so the next attempt is a short while away rather than
		// every tick; the hold is left as it was, since the mute that failed is
		// the one that may still be in force.
		retryAt := now.Add(muteRetryDelay)
		v.mu.Lock()
		if live, ok := v.byToken[entry.token]; ok {
			live.deadline = retryAt
			if v.pending != nil {
				// Only the deadline moves here; the hold is left as it was, since
				// the mute that failed is the one that may still be in force.
				if err := v.pending.MoveDeadline(context.Background(), entry.token,
					retryAt, live.heldUntil); err != nil {
					log.Error("could not record the retry in the data layer", "error", err)
				}
			}
		}
		v.mu.Unlock()
		log.Error("could not hold a member again, so they are no longer muted",
			"error", err, "retry_at", retryAt.Format(time.RFC3339))
		return
	}

	// The live entry is updated rather than the copy the sweep handed over: the
	// copy is not what the next sweep looks at or what the state file records,
	// so writing to it would leave the hold looking due on every tick and renew
	// it again and again.
	v.mu.Lock()
	if live, ok := v.byToken[entry.token]; ok {
		live.heldUntil = heldUntil
		live.deadline = deadline
		if v.pending != nil {
			if err := v.pending.MoveDeadline(context.Background(), entry.token,
				deadline, heldUntil); err != nil {
				log.Error("could not record the renewal in the data layer", "error", err)
			}
		}
	}
	v.mu.Unlock()
	log.Info("held a member again and pushed their deadline forward, "+
		"because they have not verified",
		"held_until", heldUntil.Format(time.RFC3339),
		"next_deadline", deadline.Format(time.RFC3339))
}

// handleDeadline tells the administrators about a member who has not verified,
// and removes that member when the configuration asks for it.
func (v *verifier) handleDeadline(entry *pending) {
	// The entry carries the settings its group had when the member was held.
	if entry.settings.OnDeadline == OnDeadlineNone {
		return
	}
	log := v.deps.Logger.With("group", entry.groupOpenID, "member", entry.memberOpenID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The renewal is this mode's action, and it is the same one the sweep
	// performs when the deadline passes by itself.
	//
	// Without this branch the mode fell through both of the cases below and the
	// debug command reported success while doing nothing at all, which is how
	// "I ran it and nothing happened" was produced: no removal, no report, and
	// since the confirmation was dropped there was nothing to see anywhere.
	if entry.settings.OnDeadline == OnDeadlineRemute {
		v.renew(ctx, entry)
		return
	}

	// The administrators are told only when a person has to act. With the
	// removal automated there is nothing for them to do, and mentioning them
	// anyway would be noise on every timeout.
	if entry.settings.OnDeadline == OnDeadlineNotify {
		if err := v.report(ctx, entry); err != nil {
			log.Error("could not report a member who missed the deadline", "error", err)
		} else {
			log.Warn("reported a member who missed the deadline",
				"deadline_hours", entry.settings.DeadlineHours)
		}
	}

	if entry.settings.OnDeadline != OnDeadlineRemove || entry.settings.DryRun {
		return
	}
	removed := v.remove(ctx, entry, log)
	if !entry.settings.pseudoHolds() {
		// A real hold ends at the removal in the group's name either way: the
		// mute runs out by itself whether or not the removal worked, and that is
		// what releases the record.
		return
	}
	if !removed {
		// Nothing was taken out of the group. A mute would have run out of its
		// own accord by now and released them; a pseudo-mute has no such ending,
		// so the hold goes on and the member keeps being taken back -- which is
		// the safer of the two halves, since the group's rule for them was to
		// leave. Nobody is told again: the deadline has been acted on.
		log.Warn("the member could not be removed, so they stay held back until "+
			"they verify or an administrator lets them through",
			"deadline_hours", entry.settings.DeadlineHours)
		return
	}
	// The pseudo-mute ends here rather than at an expiry of its own: the member
	// this record was holding is out of the group, and a record that outlived
	// them would go on taking back messages from somebody who is no longer there
	// to send them.
	v.forget(entry.token)
	log.Info("the pseudo-mute ended with the removal: the member is out of the group")
}

// report posts the notice that names the administrators and the member.
func (v *verifier) report(ctx context.Context, entry *pending) error {
	message := renderTemplate(entry.settings.NotifyMessage, map[string]string{
		"{at}":     v.mentionTags(entry.settings.NotifyMembers),
		"{target}": atTag(entry.memberOpenID),
		"{hours}":  fmt.Sprint(entry.settings.DeadlineHours),
	})
	_, err := v.sendMarkdown(ctx, entry.groupOpenID, message, "")
	return err
}

// remove takes one member out of the group, and reports whether they are gone.
//
// The answer matters to the pseudo-mute and to nobody else: a hold that never
// expires has to end somewhere, and being out of the group is the one ending
// this can act on.
func (v *verifier) remove(ctx context.Context, entry *pending, log *slog.Logger) bool {
	if entry.settings.RemoveBackend == BackendOfficial {
		return v.removeWithOfficial(ctx, entry, log)
	}
	return v.removeWithOneBot(ctx, entry, log)
}

// removeWithOneBot hands the removal to OneBot.
//
// The openid means nothing to OneBot and no API converts one into a QQ number,
// but the platform converts it for us when the bot mentions the member: the
// mention reaches every other client as a real QQ number, which OneBot can read
// back. The message is accepted only when it came from the bot's own account,
// because anyone in the group could otherwise copy its marker and mention
// whoever they liked.
func (v *verifier) removeWithOneBot(ctx context.Context, entry *pending, log *slog.Logger) bool {
	if v.deps.OneBot == nil {
		log.Error("cannot remove the member: on_deadline asks for a removal but no " +
			"onebot url is configured, so the member was only reported")
		return false
	}
	groupQQID, known := v.deps.GroupQQID(entry.groupOpenID)
	if !known || groupQQID == 0 {
		log.Error("cannot remove the member: this group has no qq_group_id configured, "+
			"and onebot acts on QQ group numbers rather than openids",
			"group", entry.groupOpenID)
		return false
	}
	if v.deps.BotQQ == 0 {
		log.Error("cannot remove the member: bot.qq is not configured, so a message " +
			"read back through onebot could not be proven to be the bot's own, " +
			"and a forged copy would remove the wrong person")
		return false
	}

	// The notice has to be sent by the official bot, because the platform
	// resolves an openid into a QQ number only for a mention that bot sends.
	// Sending it through OneBot instead would post the official tag as plain
	// text and resolve nothing, which is what production showed.
	if _, err := v.deps.Client.SendGroupMessage(ctx, entry.groupOpenID, &qqbotsdk.Message{
		MsgType:  qqbotsdk.MsgTypeMarkdown,
		Markdown: &qqbotsdk.MessageMarkdown{Content: targetNotice(entry)},
	}); err != nil {
		log.Error("could not post the notice that resolves the member, so nobody "+
			"was removed", "error", err)
		return false
	}

	qq, err := v.deps.OneBot.WaitForMentionedQQ(ctx, onebot.ResolveRequest{
		GroupID:  groupQQID,
		SenderQQ: v.deps.BotQQ,
		Marker:   entry.token,
	})
	if err != nil {
		log.Error("could not resolve the member into a QQ number, so nobody was removed",
			"error", err)
		return false
	}
	log = log.With("qq_user_id", qq)

	if err := v.deps.OneBot.KickGroupMember(ctx, groupQQID, qq, false); err != nil {
		log.Error("onebot could not remove the member", "error", err)
		return false
	}
	log.Warn("removed a member who missed the deadline")
	return true
}

// targetNotice is the message whose mention resolves a member into a QQ number.
//
// The token is part of the text so the answer can be tied back to this member
// and to nobody else, and so a copy of the text sent from another account can
// be told apart from the bot's own message.
func targetNotice(entry *pending) string {
	return atTag(entry.memberOpenID) + " 未通过验证，即将移出群聊。(#" + entry.token + ")"
}

// removeWithOfficial asks the official API to remove the member.
func (v *verifier) removeWithOfficial(ctx context.Context, entry *pending, log *slog.Logger) bool {
	result, err := v.deps.Client.BatchRemoveGroupMembers(ctx, entry.groupOpenID,
		&qqbotsdk.BatchRemoveMembersRequest{MemberOpenIDs: []string{entry.memberOpenID}})
	switch {
	case err == nil:
		log.Warn("removed a member who missed the deadline", "result", result.Result)
		return true
	case qqbotsdk.IsOpenAPIError(err, qqbotsdk.ErrGroupNoAPIPermission):
		log.Error("cannot remove the member: this application has no permission to "+
			"remove group members, which is exactly why the onebot backend exists; "+
			"the member stays held until they verify", "error", err)
		return false
	default:
		log.Error("could not remove a member who missed the deadline", "error", err)
		return false
	}
}

// onHeldMemberMessage takes back what a member who is still verifying says.
//
// Under the pseudo-mute this is the hold itself: nothing was applied to the
// member, so every message they get out is one the group would have read, and
// taking it back is all there is between them and the group.
//
// Under a real mute it closes the gap before the mute lands instead. The hold is
// applied through an API call, and the platform's mute is not instant even once
// that call succeeds, so a member who joins and types quickly gets a message out
// -- and as far as the group is concerned they then stayed silent, rather than
// having said something that the verification asks them to be judged on.
//
// Only members who are being held are touched, so this is not a general
// moderation tool, and the bot's own notices are never affected.
func (v *verifier) onHeldMemberMessage(ctx context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		return err
	}
	data, ok := value.(*qqbotsdk.GroupMessageCreateData)
	if !ok || data.Author == nil || data.ID == "" {
		return nil
	}
	if !v.deps.InGroup(data.GroupOpenID) {
		return nil
	}
	entry, held := v.findByMember(data.GroupOpenID, data.Author.MemberOpenID)
	if !held {
		return nil
	}
	if entry.settings.pseudoHolds() && entry.settings.DryRun {
		// The take-back is the whole of what a pseudo-mute does to a member, so
		// a dry run must not do it: taking a message back is exactly as visible
		// in the group as silencing somebody. What the dry run leaves behind is
		// the record of a member who is waiting to verify.
		//
		// A real mute keeps what it did before, where this only closes the gap
		// before a mute that the dry run is not applying either.
		return nil
	}

	if err := v.deps.Client.RecallGroupMessage(ctx, data.GroupOpenID, data.ID); err != nil {
		// Recalling somebody else's message needs the bot to be a group
		// administrator, so this warns rather than fails: under a real mute the
		// hold either applied in time or it did not, and under the pseudo-mute
		// the recall is the hold, so a group where this keeps failing is a group
		// where the verification does nothing. Either way the operator is the
		// one who can grant that permission.
		v.deps.Logger.Warn("could not recall a message from a member who is still verifying",
			"group", data.GroupOpenID, "member", data.Author.MemberOpenID,
			"mute_mode", entry.settings.MuteMode, "error", err)
		return nil
	}
	v.deps.Logger.Info("recalled a message from a member who is still verifying",
		"group", data.GroupOpenID, "member", data.Author.MemberOpenID,
		"mute_mode", entry.settings.MuteMode)
	return nil
}

// IsPseudoMuted implements feature.Verifier: it reports whether this member is
// held back by a take-back rather than by a mute, which is what decides both
// whether their message may be written down and whether anything of theirs is
// answered at all.
func (v *verifier) IsPseudoMuted(groupOpenID, memberOpenID string) bool {
	entry, held := v.findByMember(groupOpenID, memberOpenID)
	// A dry run takes nothing back, so it must not hide anything either: the
	// message stays in the group, and a record of it is what lets the rest of the
	// bot see what the group saw.
	return held && entry.settings.pseudoHolds() && !entry.settings.DryRun
}

// IsPending implements feature.Verifier.
func (v *verifier) IsPending(groupOpenID, memberOpenID string) bool {
	_, found := v.findByMember(groupOpenID, memberOpenID)
	return found
}

// Resend implements feature.Verifier: it sends the prompt again for a member who
// is already waiting, with the button they already have.
//
// Nothing else changes: the member keeps the hold they were given, the token is
// the same one, and the button already posted keeps working. That is the point
// -- this is for the ordinary case of nobody noticing the first prompt, not for
// starting the round over.
func (v *verifier) Resend(ctx context.Context, groupOpenID, memberOpenID string) error {
	entry, found := v.findByMember(groupOpenID, memberOpenID)
	if !found {
		return errors.New("该成员不在验证中，无法重新发送验证通知")
	}
	return v.ask(ctx, groupOpenID, memberOpenID, entry.token, entry.settings)
}

// Reverify implements feature.Verifier: it holds the member again and sends a
// fresh prompt.
//
// An earlier entry for the same member is dropped first, so a button from the
// previous round cannot still release them.
func (v *verifier) Reverify(ctx context.Context, groupOpenID, memberOpenID string) error {
	if existing, found := v.findByMember(groupOpenID, memberOpenID); found {
		v.forget(existing.token)
	}
	return v.begin(ctx, groupOpenID, memberOpenID, time.Now().Unix())
}

// SimulateDeadline implements feature.Verifier: it runs the missed-deadline path
// for a member immediately.
//
// It deliberately does not mark the entry as reported, so an operator can run
// the simulation more than once while setting the flow up. The real deadline
// still fires on its own, because the sweep only acts once it has passed.
func (v *verifier) SimulateDeadline(_ context.Context, groupOpenID, memberOpenID string) error {
	entry, found := v.findByMember(groupOpenID, memberOpenID)
	if !found {
		return errors.New("no member is waiting to verify in this group")
	}
	v.handleDeadline(entry)
	return nil
}

// onMemberAdd handles GROUP_MEMBER_ADD.
func (v *verifier) onMemberAdd(ctx context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		return err
	}
	data, ok := value.(*qqbotsdk.GroupMemberAddData)
	if !ok {
		return fmt.Errorf("join verification got %T for a member add event", value)
	}
	if !v.deps.InGroup(data.GroupOpenID) {
		v.deps.Logger.Debug("ignoring a join outside the configured groups",
			"group", data.GroupOpenID)
		return nil
	}
	if data.MemberOpenID == "" {
		// The openid is what both the mute and the button check need, so
		// without it the feature cannot act at all.
		return errors.New("the join event carried no member openid")
	}
	return v.begin(ctx, data.GroupOpenID, data.MemberOpenID, data.Timestamp)
}

// begin holds one new member and asks them to verify.
func (v *verifier) begin(ctx context.Context, groupOpenID, memberOpenID string, joinedAt int64) error {
	// The settings are resolved once here and carried by the entry, so the
	// deadline and the button press act on the rules this member was held
	// under, and no handler has to work out which group it is dealing with.
	settings := v.cfg.settingsFor(groupOpenID)
	now := time.Now()
	// The pseudo-mute has no expiry of its own: it ends when the member verifies
	// or when the deadline's action takes them out of the group, and not before.
	// A zero moment is how "no expiry" is written down, in memory and in the data
	// layer, because the column that holds it in the second is an integer.
	var heldUntil time.Time
	if !settings.pseudoHolds() {
		heldUntil = now.Add(time.Duration(settings.MuteMinutes) * time.Minute)
	}
	deadline := now.Add(time.Duration(settings.DeadlineHours) * time.Hour)

	token, err := v.hold(ctx, groupOpenID, memberOpenID, joinedAt, deadline, heldUntil, settings)
	if err != nil {
		return err
	}
	log := v.deps.Logger.With("group", groupOpenID, "member", memberOpenID)

	// The entry is recorded before anything is applied to the member, so a press
	// that arrives immediately can still be answered.
	if err := v.mute(ctx, groupOpenID, memberOpenID, heldUntil, settings); err != nil {
		v.forget(token)
		return fmt.Errorf("holding the new member: %w", err)
	}
	if settings.pseudoHolds() {
		log.Info("a new member is being held without a mute: their messages are "+
			"taken back until they verify",
			"deadline", deadline.Format(time.RFC3339))
	} else {
		log.Info("held a new member back for verification",
			"held_until", heldUntil.Format(time.RFC3339),
			"deadline", deadline.Format(time.RFC3339))
	}

	if err := v.ask(ctx, groupOpenID, memberOpenID, token, settings); err != nil {
		// Leaving someone held with no way to verify would lock them out,
		// so the hold is lifted again when the prompt cannot be delivered.
		v.forget(token)
		if undoErr := v.unmute(ctx, groupOpenID, memberOpenID, settings); undoErr != nil {
			log.Error("could not lift the hold after the prompt failed", "error", undoErr)
		} else {
			log.Warn("lifted the hold because the prompt could not be sent")
		}
		return fmt.Errorf("sending the verification prompt: %w", err)
	}
	log.Info("asked the new member to verify")
	return nil
}

// onInteractionPress answers a press of the verification button.
//
// The routing happened before this was called: a press reaches here when its
// button carries this feature's namespace and came from a group. What is left for
// this handler to decide is whether the press names anything it is holding.
func (v *verifier) onInteractionPress(ctx context.Context, press command.Press) error {
	token, data := press.Payload, press.Data
	if token == "" {
		// The namespace and nothing after it. There is no member to look up, and
		// answering would report a failure for a button nobody made.
		return nil
	}
	log := v.deps.Logger.With("group", data.GroupOpenID, "member", data.GroupMemberOpenID)

	entry, found := v.lookup(token)
	if !found {
		// Unknown or expired: answering with a failure keeps the button
		// pressable, which is what someone retrying needs.
		log.Info("a verification button was pressed after its window closed")
		return v.answer(data.ID, qqbotsdk.InteractionCodeFailed)
	}
	if data.GroupOpenID != entry.groupOpenID || data.GroupMemberOpenID != entry.memberOpenID {
		return v.foreignPress(ctx, data, press.EventID, entry)
	}

	if err := v.unmute(ctx, entry.groupOpenID, entry.memberOpenID, entry.settings); err != nil {
		// The member stays held, so the button must stay usable: answer with
		// a failure rather than a success.
		log.Error("could not lift the hold", "error", err)
		return v.answer(data.ID, qqbotsdk.InteractionCodeFailed)
	}
	v.forget(token)
	log.Info("a member verified and can speak again")

	if err := v.answer(data.ID, qqbotsdk.InteractionCodeSuccess); err != nil {
		log.Error("the member verified but the press went unanswered", "error", err)
	}
	v.greet(ctx, press.EventID, entry)
	return nil
}

// foreignPress deals with a press that did not come from the member the button
// was made for.
//
// An administrator may skip that member's verification, which lets a moderator
// let somebody in without making them press a button they may not touch. Any
// other presser is refused, and the entry stays alive so the member the button
// belongs to can still verify: otherwise anybody could lock another member out
// by pressing their button once.
func (v *verifier) foreignPress(ctx context.Context,
	data *qqbotsdk.InteractionCreateData, eventID string, entry *pending) error {
	log := v.deps.Logger.With("group", entry.groupOpenID, "member", entry.memberOpenID)

	if v.admins == nil || !v.admins.IsAdmin(entry.groupOpenID, data.GroupMemberOpenID) {
		log.Info("someone else pressed the verification button",
			"expected_member", entry.memberOpenID, "presser", data.GroupMemberOpenID)
		return v.answer(data.ID, qqbotsdk.InteractionCodeNoPermission)
	}

	if err := v.unmute(ctx, entry.groupOpenID, entry.memberOpenID, entry.settings); err != nil {
		// The member stays held, so the answer must not claim success.
		log.Error("an administrator skipped a verification but the hold could not be lifted",
			"error", err, "presser", data.GroupMemberOpenID)
		return v.answer(data.ID, qqbotsdk.InteractionCodeFailed)
	}
	v.forget(entry.token)
	log.Warn("an administrator skipped a verification",
		"presser", data.GroupMemberOpenID, "member", entry.memberOpenID)

	if err := v.answer(data.ID, qqbotsdk.InteractionCodeSuccess); err != nil {
		log.Error("the skip went unanswered", "error", err)
	}
	v.notify(eventID, entry.groupOpenID, renderTemplate(entry.settings.SkipMessage,
		map[string]string{"{target}": atTag(entry.memberOpenID)}))
	return nil
}

// notify posts a message as a passive reply to an interaction event.
//
// It takes no context from its caller: a notice has to be posted even though the
// event that carried the press is over, and what it waits on is the feature rather
// than the request, so that it cannot outlive the instance it came from.
func (v *verifier) notify(eventID, groupOpenID, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	notifyCtx, cancel := context.WithTimeout(v.part, 15*time.Second)
	defer cancel()
	if _, err := v.sendMarkdown(notifyCtx, groupOpenID, text, eventID); err != nil {
		v.deps.Logger.Warn("could not post a notice", "error", err)
	}
}

// answer reports the outcome to the client that pressed the button.
//
// Every path through a button press has to answer, because an unanswered
// interaction leaves the presser on a spinner until it times out.
func (v *verifier) answer(interactionID string, code qqbotsdk.InteractionCode) error {
	if interactionID == "" {
		return errors.New("the interaction event carried no id to answer")
	}
	answerCtx, cancel := context.WithTimeout(v.part, 10*time.Second)
	defer cancel()
	if err := v.deps.Client.RespondInteraction(answerCtx, interactionID, code); err != nil {
		return fmt.Errorf("answering the interaction: %w", err)
	}
	return nil
}

// greet sends the pass message after a successful verification.
//
// It replies to the interaction event rather than starting a new conversation,
// which the send documentation allows for INTERACTION_CREATE and which keeps
// the message inside the passive quota.
func (v *verifier) greet(ctx context.Context, eventID string, entry *pending) {
	if strings.TrimSpace(entry.settings.PassMessage) == "" {
		return
	}
	v.notify(eventID, entry.groupOpenID, entry.settings.PassMessage)
}

// ask sends the prompt with the verification button.
//
// GROUP_MEMBER_ADD is not one of the events the send API accepts as event_id,
// and there is no message to reply to, so the prompt is a proactive message and
// counts against the active message quota.
func (v *verifier) ask(ctx context.Context, groupOpenID, memberOpenID, token string, settings Settings) error {
	prompt := renderTemplate(settings.Prompt, map[string]string{
		"{at}": mentionTag(memberOpenID, settings),
	})
	keyboard := &qqbotsdk.Keyboard{Content: &qqbotsdk.KeyboardContent{Rows: []qqbotsdk.Row{{
		Buttons: []qqbotsdk.Button{{
			ID: "verify_" + token,
			RenderData: &qqbotsdk.RenderData{
				Label:        settings.ButtonLabel,
				VisitedLabel: settings.VisitedLabel,
				Style:        qqbotsdk.KeyboardStyleBlue,
			},
			Action: &qqbotsdk.Action{
				Type:          qqbotsdk.ActionTypeCallback,
				Data:          buttonPrefix + token,
				Permission:    &qqbotsdk.Permission{Type: qqbotsdk.PermissionTypeEveryone},
				UnsupportTips: "请升级 QQ 客户端后重试",
			},
		}},
	}}}}
	_, err := v.deps.Client.SendGroupMessage(ctx, groupOpenID, &qqbotsdk.Message{
		MsgType:  qqbotsdk.MsgTypeMarkdown,
		Markdown: &qqbotsdk.MessageMarkdown{Content: prompt},
		Keyboard: keyboard,
	})
	return err
}

// sendMarkdown sends one markdown message, optionally as a passive reply.
func (v *verifier) sendMarkdown(ctx context.Context, groupOpenID, content, eventID string) (*qqbotsdk.MessageResponse, error) {
	return v.deps.Client.SendGroupMessage(ctx, groupOpenID, &qqbotsdk.Message{
		MsgType:  qqbotsdk.MsgTypeMarkdown,
		Markdown: &qqbotsdk.MessageMarkdown{Content: content},
		EventID:  eventID,
	})
}

// mute holds one member until the given time.
//
// It does nothing at all under the pseudo-mute: there nothing is applied to the
// member, and their hold is the message handler taking back what they say.
func (v *verifier) mute(ctx context.Context, groupOpenID, memberOpenID string, until time.Time, settings Settings) error {
	if settings.pseudoHolds() || settings.DryRun {
		return nil
	}
	if err := v.deps.Client.SetGroupMemberMute(ctx, groupOpenID, &qqbotsdk.SetGroupMemberMuteRequest{
		Members: []qqbotsdk.SetMemberMuteState{{
			Op:           qqbotsdk.MemberMuteAdd,
			MemberOpenID: memberOpenID,
			MuteExpireAt: until.Format(time.RFC3339),
		}},
	}); err != nil {
		return err
	}
	// Written down here rather than by the caller, because a hold is applied from
	// more than one place: a mute this bot applied is the bot's own wherever it
	// was applied, and a feature that lifts other people's mutes has to be able to
	// see that this one is not one of them.
	v.deps.Mutes.Record(groupOpenID, memberOpenID, until)
	return nil
}

// unmute lets one member speak again.
//
// Under the pseudo-mute there is nothing to lift, and nothing is asked of the
// platform: the member was never silenced by it. A mute somebody else applied
// meanwhile is deliberately left standing here, the same way it is under a real
// hold that this bot did not apply.
func (v *verifier) unmute(ctx context.Context, groupOpenID, memberOpenID string, settings Settings) error {
	if settings.pseudoHolds() || settings.DryRun {
		return nil
	}
	if err := v.deps.Client.SetGroupMemberMute(ctx, groupOpenID, &qqbotsdk.SetGroupMemberMuteRequest{
		Members: []qqbotsdk.SetMemberMuteState{{
			Op:           qqbotsdk.MemberMuteDelete,
			MemberOpenID: memberOpenID,
		}},
	}); err != nil {
		return err
	}
	v.deps.Mutes.Forget(groupOpenID, memberOpenID)
	return nil
}

// newMemberMention renders the at-mention of the member who just joined.
// mentionTag renders the at-mention of a member who just joined, or nothing
// when the group asks for no mention.
func mentionTag(memberOpenID string, settings Settings) string {
	if !settings.MentionNewMember {
		return ""
	}
	return atTag(memberOpenID)
}

// mentionTags renders the at-mentions of the configured administrators.
func (v *verifier) mentionTags(members []string) string {
	tags := make([]string, 0, len(members))
	for _, member := range members {
		if trimmed := strings.TrimSpace(member); trimmed != "" {
			tags = append(tags, atTag(trimmed))
		}
	}
	return strings.Join(tags, " ")
}

// hold records a member waiting to verify, replacing any earlier entry for the
// same member, and returns the token its button will carry.
func (v *verifier) hold(ctx context.Context, groupOpenID, memberOpenID string, joinedAt int64, deadline, heldUntil time.Time, settings Settings) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	entry := &pending{
		token:        token,
		groupOpenID:  groupOpenID,
		memberOpenID: memberOpenID,
		joinedAt:     joinedAt,
		deadline:     deadline,
		heldUntil:    heldUntil,
		settings:     settings,
	}

	// Written before anything is applied to the member, and a failure stops the
	// hold: a mute the data layer never recorded is the one outcome that leaves
	// somebody muted with nobody able to find out why, and a pseudo-mute the data
	// layer never recorded is one that does not happen at all. The caller already
	// treats an error here as "do not hold them".
	if v.pending != nil {
		encoded, err := encodeSettings(settings)
		if err != nil {
			return "", err
		}
		if err := v.pending.Put(ctx, store.Pending{
			Token:        token,
			GroupOpenID:  groupOpenID,
			MemberOpenID: memberOpenID,
			JoinedAt:     joinedAt,
			Deadline:     deadline.Unix(),
			// Nothing when the hold has no expiry of its own, which is the
			// pseudo-mute: the data layer reads a zero as "this one does not run
			// out", and the zero moment stored as its own year would instead
			// read as a hold that ended before the epoch.
			HeldUntil: heldUntilStamp(heldUntil),
			Settings:  encoded,
		}); err != nil {
			return "", fmt.Errorf("recording the hold: %w", err)
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	key := memberKey(groupOpenID, memberOpenID)
	if previous, ok := v.byMember[key]; ok {
		delete(v.byToken, previous.token)
	}
	v.byToken[token] = entry
	v.byMember[key] = entry
	return token, nil
}

// lookup returns the entry a token belongs to.
func (v *verifier) lookup(token string) (*pending, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	entry, ok := v.byToken[token]
	return entry, ok
}

// findByMember returns the entry for one member.
func (v *verifier) findByMember(groupOpenID, memberOpenID string) (*pending, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	entry, ok := v.byMember[memberKey(groupOpenID, memberOpenID)]
	return entry, ok
}

// forget drops an entry by token.
func (v *verifier) forget(token string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.forgetLocked(token)
}

// forgetLocked drops an entry by token while the lock is already held.
func (v *verifier) forgetLocked(token string) {
	entry, ok := v.byToken[token]
	if !ok {
		return
	}
	delete(v.byToken, token)
	key := memberKey(entry.groupOpenID, entry.memberOpenID)
	if current, ok := v.byMember[key]; ok && current.token == token {
		delete(v.byMember, key)
	}
	if v.pending != nil {
		// A background context, because this runs under the lock and without one
		// of its own -- from a button press or from the sweep. A failure is
		// reported and swallowed: the hold is over either way, and the leftover
		// row is at worst loaded again on the next start.
		if err := v.pending.Delete(context.Background(), token); err != nil {
			v.deps.Logger.Error("could not forget a hold in the data layer",
				"group", entry.groupOpenID, "member", entry.memberOpenID, "error", err)
		}
	}
}

// memberKey identifies one member inside one group.
func memberKey(groupOpenID, memberOpenID string) string {
	return groupOpenID + "/" + memberOpenID
}

// newToken returns a random token that fits in a button.
func newToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a verification token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// atTag renders one mention.
//
// The documented format for a mention inside a message is a text chain tag, and
// the id is the group member openid, which is the only identity the bot holds
// for a member of a group.
func atTag(memberOpenID string) string {
	return `<qqbot-at-user id="` + memberOpenID + `" />`
}

// renderTemplate substitutes the placeholders a message may use.
func renderTemplate(text string, values map[string]string) string {
	rendered := text
	for placeholder, value := range values {
		rendered = strings.ReplaceAll(rendered, placeholder, value)
	}
	return strings.TrimSpace(rendered)
}
