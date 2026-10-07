// Package muteguard lifts the mutes other people apply to the members a group has
// named as ones it wants to keep talking.
//
// The platform helps on neither end of this. Nothing is delivered when a mute is
// applied, and the group's mute state says nothing about who applied one: a mute
// this bot set and a mute a QQ administrator set read exactly alike. What the
// platform does offer is the whole of a group's mute state in one call, so this
// feature looks at each group it watches on a timer and lifts what is left of that
// state once the bot's own mutes are taken out of it.
//
// Telling the bot's own work apart from everybody else's is the whole of the
// difficulty, and it is answered from two records rather than one:
//
//   - the mutes written down as they are applied, in internal/mutelog. That is how
//     an administrator's /禁言 and a judgement's punishment are recognised: both
//     are asked for by name, and neither should be undone by this feature.
//   - the holds the verification feature keeps in the data layer, read here as
//     records rather than asked of that feature. A hold is a real mute on a real
//     person whether or not the feature that wrote it is running, and it outlives
//     a restart, which a feature built again does not. A member who is verifying
//     must never be let out by this one, or the verification would be something
//     they can walk out of by waiting.
//
// Whatever is left after both is somebody else's, and is lifted.
//
// A group-wide mute is deliberately not touched. That is the group's own setting,
// made by its owner for the group rather than against one member, and lifting it
// would be this feature overruling the group instead of restoring a member to it.
// A group that is muted as a whole is said out loud instead, because a whitelisted
// member who still cannot speak is the one case where this feature's silence would
// be mistaken for it working.
package muteguard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// Name is the feature's name: its configuration key and its log field.
const Name = "mute_guard"

const (
	// DefaultInterval is how often a group is looked at when the file says nothing.
	DefaultInterval = 30 * time.Second
	// MinInterval and MaxInterval bound what the file may ask for. A look is one
	// API call per group, so the lower bound is what keeps the bot out of the
	// platform's rate limits; the upper bound is how long somebody else's mute may
	// stand before it is lifted.
	MinInterval = 10 * time.Second
	MaxInterval = time.Hour
	// lookTimeout bounds one pass, so a platform that stops answering cannot hold
	// the sweep -- and therefore Close -- up for ever.
	lookTimeout = 30 * time.Second
)

// Config is the feature's own section.
type Config struct {
	// Enabled is the switch every section may carry.
	Enabled *bool `yaml:"enabled"`
	// IntervalSeconds is how often each watched group is looked at.
	IntervalSeconds *int `yaml:"interval_seconds"`
	// Members is the list of members to keep talking, for every group that does
	// not name its own.
	Members []string `yaml:"members"`
	// Groups is the per-group override.
	Groups map[string]GroupOverride `yaml:"groups"`
}

// GroupOverride is one group's own settings.
type GroupOverride struct {
	// Enabled turns the guard off for this group while it stays on elsewhere.
	Enabled *bool `yaml:"enabled"`
	// Members replaces the shared list rather than adding to it. A group that
	// names an empty list has named nobody, which is different from not naming
	// one -- the second takes the shared list, the first does not.
	Members *[]string `yaml:"members"`
}

// Settings is what one group resolved to.
//
// Exported the way the verification feature's Settings is: what a group ends up
// with is a question worth being able to ask from a test without reaching into the
// file.
type Settings struct {
	// Enabled reports whether this group is watched at all.
	Enabled bool
	// Members are the members whose mutes are lifted.
	Members []string
	// Interval is how often the group is looked at.
	Interval time.Duration
}

// defaults is what a group that names nothing of its own inherits.
func (c Config) defaults() Settings {
	return Settings{
		Enabled:  true,
		Members:  c.Members,
		Interval: c.interval(),
	}
}

// settingsFor is one group's settings: the shared ones with its own written over
// them, field by field, so that naming one thing does not silently reset the rest.
func (c Config) settingsFor(groupOpenID string) Settings {
	settings := c.defaults()
	override, found := c.Groups[groupOpenID]
	if !found {
		return settings
	}
	if override.Enabled != nil {
		settings.Enabled = *override.Enabled
	}
	if override.Members != nil {
		settings.Members = *override.Members
	}
	return settings
}

// interval is how often a group is looked at.
func (c Config) interval() time.Duration {
	if c.IntervalSeconds == nil {
		return DefaultInterval
	}
	return time.Duration(*c.IntervalSeconds) * time.Second
}

// validate refuses a section at startup rather than at the first look.
//
// An interval outside the bounds is a mistake that shows up as mutes that are not
// lifted, or as a bot the platform starts refusing; a member named by nothing is a
// typo that would silently match nobody.
func (c Config) validate() error {
	if interval := c.interval(); interval < MinInterval || interval > MaxInterval {
		return fmt.Errorf("%s: interval_seconds must be between %d and %d, got %s",
			Name, int(MinInterval.Seconds()), int(MaxInterval.Seconds()), interval)
	}
	for _, members := range c.lists() {
		for _, memberOpenID := range members {
			if strings.TrimSpace(memberOpenID) == "" {
				return fmt.Errorf("%s: a member in the list is empty", Name)
			}
		}
	}
	return nil
}

// lists is every list the file writes, the shared one and each group's own.
func (c Config) lists() [][]string {
	lists := [][]string{c.Members}
	for _, groupOpenID := range c.groupNames() {
		override := c.Groups[groupOpenID]
		if override.Members != nil {
			lists = append(lists, *override.Members)
		}
	}
	return lists
}

// groupNames is the groups the file writes, in a settled order so that a message
// about one of them does not depend on which order a map happened to be read in.
func (c Config) groupNames() []string {
	names := make([]string, 0, len(c.Groups))
	for groupOpenID := range c.Groups {
		names = append(names, groupOpenID)
	}
	sort.Strings(names)
	return names
}

// member is one person in one group: a mute is per group, because the same person
// is a different member of every group they are in.
type member struct {
	groupOpenID  string
	memberOpenID string
}

// handler implements feature.Feature.
type handler struct {
	deps feature.Deps
	cfg  Config

	// stopping ends the sweep; stopped is closed once it has ended.
	//
	// A rebuild builds the replacement while this one is still sweeping, and two
	// sweeps lifting mutes in the same group would be two answers to one question.
	stopping chan struct{}
	stopped  chan struct{}

	// said is the last state reported for each thing said about each group -- a
	// group whose mute state cannot be read, a group muted as a whole -- so that a
	// state which persists is reported where it appears rather than on every look.
	// The key is the group and what is being said about it.
	mu   sync.Mutex
	said map[string]string
}

// New builds the feature from its configuration section.
func New(section yaml.Node, deps feature.Deps) (feature.Feature, error) {
	var cfg Config
	if err := section.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("reading the %s section: %w", Name, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &handler{
		deps:     deps,
		cfg:      cfg,
		stopping: make(chan struct{}),
		stopped:  make(chan struct{}),
		said:     map[string]string{},
	}, nil
}

// Name implements feature.Feature.
func (h *handler) Name() string { return Name }

// Intents implements feature.Feature.
//
// None: the platform delivers no event when a mute is applied, so there is nothing
// for this feature to be handed. It works on a clock instead.
func (h *handler) Intents() qqbotsdk.Intent { return 0 }

// Register implements feature.Feature.
func (h *handler) Register(_ context.Context) error {
	watched := h.watched()
	if len(watched) == 0 {
		h.deps.Logger.Warn("no group names anybody to keep talking, so no mute " +
			"will be lifted for anybody")
	}
	h.deps.Logger.Info("the mutes other people apply are being lifted",
		"groups", len(watched), "interval", h.cfg.interval().String())
	go h.sweepLoop()
	return nil
}

// Close implements feature.Feature.
//
// It waits for a sweep in flight to finish before returning: the sweep lifts mutes,
// and one still running after this feature was taken down would be acting for an
// instance nobody is running.
func (h *handler) Close(context.Context) error {
	close(h.stopping)
	<-h.stopped
	return nil
}

// sweepLoop looks at every watched group until Close.
//
// The first look happens at once rather than one interval away, because the file
// having just been edited to name somebody is the ordinary way a mute comes to need
// lifting, and a member waiting to speak should not wait for a tick to be noticed.
func (h *handler) sweepLoop() {
	defer close(h.stopped)
	ticker := time.NewTicker(h.cfg.interval())
	defer ticker.Stop()
	for {
		h.look()
		select {
		case <-h.stopping:
			return
		case <-ticker.C:
		}
	}
}

// look makes one pass with a deadline of its own.
func (h *handler) look() {
	ctx, cancel := context.WithTimeout(context.Background(), lookTimeout)
	defer cancel()
	h.sweep(ctx)
}

// sweep is one pass over every watched group.
func (h *handler) sweep(ctx context.Context) {
	now := time.Now()
	held, err := h.heldByTheVerification(ctx, now)
	if err != nil {
		// Nothing is lifted when the holds cannot be read. Leaving somebody
		// else's mute standing for one interval is a delay; lifting a member who
		// is verifying is a hole in the verification, and this feature is not
		// allowed to make one.
		h.deps.Logger.Error("the members still verifying could not be read, so "+
			"nobody was lifted this time", "error", err)
		return
	}
	for _, groupOpenID := range h.watched() {
		h.sweepGroup(ctx, groupOpenID, held, now)
	}
}

// sweepGroup looks at one group and lifts what is left of its mutes.
func (h *handler) sweepGroup(ctx context.Context, groupOpenID string, held map[member]bool,
	now time.Time) {
	setting, err := h.deps.Client.GetGroupRestrictChatSetting(ctx, groupOpenID)
	if err != nil {
		// Said once per state rather than once per look: a group the bot may not
		// read would otherwise fill the log with the same line for ever.
		h.say(groupOpenID, "read", "failed", func() {
			h.deps.Logger.Warn("a group's mute state could not be read, so nobody "+
				"was lifted there this time", "group", groupOpenID, "error", err)
		})
		return
	}
	h.say(groupOpenID, "read", "", nil)

	// A group-wide mute is the group's own decision and is left standing, but a
	// named member who still cannot speak is the one case where saying nothing
	// would look like this feature working.
	mode := ""
	if setting.GlobalRule != nil {
		mode = setting.GlobalRule.Mode
	}
	if mode != "" && mode != qqbotsdk.GroupMuteNone {
		h.say(groupOpenID, "whole-group", mode, func() {
			h.deps.Logger.Warn("the whole group is muted, which this feature does "+
				"not lift, so the named members cannot speak either",
				"group", groupOpenID, "mode", mode)
		})
	} else {
		h.say(groupOpenID, "whole-group", "", nil)
	}

	settings := h.cfg.settingsFor(groupOpenID)
	var (
		lift    []string
		expires = map[string]string{}
	)
	for _, muted := range setting.Members {
		if muted.MemberOpenID == "" || !contains(settings.Members, muted.MemberOpenID) {
			continue
		}
		if held[member{groupOpenID: groupOpenID, memberOpenID: muted.MemberOpenID}] {
			continue
		}
		if h.deps.Mutes.Applied(groupOpenID, muted.MemberOpenID, now) {
			continue
		}
		lift = append(lift, muted.MemberOpenID)
		expires[muted.MemberOpenID] = muted.MuteExpireAt
	}
	if len(lift) == 0 {
		return
	}

	// Batched, because one request changes at most this many members, and in a
	// settled order so that a failure says something reproducible.
	sort.Strings(lift)
	for start := 0; start < len(lift); start += qqbotsdk.MaxGroupMuteTargets {
		end := min(start+qqbotsdk.MaxGroupMuteTargets, len(lift))
		batch := lift[start:end]
		changes := make([]qqbotsdk.SetMemberMuteState, 0, len(batch))
		for _, memberOpenID := range batch {
			changes = append(changes, qqbotsdk.SetMemberMuteState{
				Op:           qqbotsdk.MemberMuteDelete,
				MemberOpenID: memberOpenID,
			})
		}
		err := h.deps.Client.SetGroupMemberMute(ctx, groupOpenID,
			&qqbotsdk.SetGroupMemberMuteRequest{Members: changes})
		if err != nil {
			h.deps.Logger.Error("a mute somebody else applied could not be lifted",
				"group", groupOpenID, "members", len(batch), "error", err)
			continue
		}
		for _, memberOpenID := range batch {
			h.deps.Logger.Info("lifted a mute this bot did not apply",
				"group", groupOpenID, "member", memberOpenID,
				"was_until", expires[memberOpenID])
		}
	}
}

// heldByTheVerification is the members this bot is holding for verification.
//
// The records are read here rather than asked of the feature that writes them: a
// hold is a mute that is really in force whether or not that feature is running,
// and reading the layer costs one query for the whole pass instead of one call per
// group. The error matters as much as the answer -- not knowing is not the same as
// knowing there is nobody, and the difference is somebody being let out of a
// verification.
func (h *handler) heldByTheVerification(ctx context.Context, now time.Time) (map[member]bool, error) {
	if h.deps.Store == nil {
		return nil, errNoStore
	}
	entries, err := h.deps.Store.Pending().Load(ctx, now)
	if err != nil {
		return nil, err
	}
	held := make(map[member]bool, len(entries))
	for _, entry := range entries {
		held[member{groupOpenID: entry.GroupOpenID, memberOpenID: entry.MemberOpenID}] = true
	}
	return held, nil
}

// watched is the groups this feature looks at, in a settled order.
//
// A group is watched when it has somebody named to keep talking and the bot manages
// it. A group whose list is empty is not looked at at all: a look is an API call,
// and there would be nothing to do with the answer.
func (h *handler) watched() []string {
	names := map[string]bool{}
	for groupOpenID := range h.cfg.Groups {
		names[groupOpenID] = true
	}
	if len(h.cfg.Members) > 0 {
		for _, group := range h.deps.Groups {
			names[group.OpenID] = true
		}
	}
	watched := make([]string, 0, len(names))
	for groupOpenID := range names {
		settings := h.cfg.settingsFor(groupOpenID)
		switch {
		case !settings.Enabled:
			// Turned off for this group.
		case len(settings.Members) == 0:
			// Nobody to keep talking.
		case !h.deps.InGroup(groupOpenID):
			// Not a group this bot manages: it is told nothing about events
			// there, and has no business lifting a mute in it.
		default:
			watched = append(watched, groupOpenID)
		}
	}
	sort.Strings(watched)
	return watched
}

// say reports a state once per change, per thing said about a group.
//
// Two things are said about a group -- whether its mute state could be read, and
// whether it is muted as a whole -- and they change independently, so they are
// tracked apart: a group whose read failed and then worked must still be reported
// when it is later muted as a whole. An empty state clears what was said about
// that one thing, so a group that works again and then fails again is reported
// both times rather than once and never again.
func (h *handler) say(groupOpenID, about, state string, report func()) {
	key := groupOpenID + "\x00" + about
	h.mu.Lock()
	changed := h.said[key] != state
	if changed {
		if state == "" {
			delete(h.said, key)
		} else {
			h.said[key] = state
		}
	}
	h.mu.Unlock()
	if changed && report != nil {
		report()
	}
}

// contains reports whether one of the members is the given one.
func contains(members []string, memberOpenID string) bool {
	for _, member := range members {
		if member == memberOpenID {
			return true
		}
	}
	return false
}

// errNoStore is returned when the feature is built without the data layer, which
// the application never does and a test might.
var errNoStore = errors.New("muteguard: the data layer is needed to tell the bot's own holds apart")
