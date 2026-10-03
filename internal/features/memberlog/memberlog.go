// Package memberlog records who joined a group and who left it.
//
// It exists because nothing else keeps this and nothing else can: the platform
// refuses this application the member-list endpoints (40012010, measured), so a
// group's membership cannot be read back at any point in time. A departure is
// therefore invisible unless it was written down while the event was in hand --
// which is exactly what a log written after the fact cannot do.
//
// It decides nothing and answers nobody. Every event is written down and that is
// all, so a mistake in it can only be a missing row.
package memberlog

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// Name is the feature's name: its configuration key and its log field.
const Name = "member_log"

// groupNameTimeout bounds the one lookup an event may cost.
//
// A handler runs on the path that delivers every event, so a call that hangs
// would hold up every group behind it. The name is worth having and not worth
// waiting long for; a lookup that times out leaves the event recorded with an
// empty name rather than not recorded at all.
const groupNameTimeout = 5 * time.Second

// Config is the feature's own section.
type Config struct {
	// Enabled is the switch every section may carry.
	Enabled *bool `yaml:"enabled"`
}

// handler implements feature.Feature.
type handler struct {
	deps feature.Deps

	// names caches what each group is called.
	//
	// A name is read once per group and then kept: it is wanted on every event,
	// and an event is no place for a second round trip. A group that has been
	// renamed keeps the name it had when it was first seen, which is the same
	// bargain the record itself makes.
	mu    sync.Mutex
	names map[string]string
}

// New builds the feature from its configuration section.
func New(section yaml.Node, deps feature.Deps) (feature.Feature, error) {
	var cfg Config
	if err := section.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("reading the %s section: %w", Name, err)
	}
	return &handler{deps: deps, names: map[string]string{}}, nil
}

// Name implements feature.Feature.
func (h *handler) Name() string { return Name }

// Intents implements feature.Feature.
//
// Only the member events: this feature reads nothing else and has no business
// being handed anything else.
func (h *handler) Intents() qqbotsdk.Intent {
	return qqbotsdk.IntentGroupMemberEvent
}

// Register implements feature.Feature.
func (h *handler) Register(_ context.Context) error {
	// The names of the configured groups are read up front, so the events that
	// arrive later are never the thing waiting on the network. Groups that are
	// not configured are read when they are first seen instead.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, group := range h.deps.Groups {
		if _, err := h.groupName(ctx, group.OpenID); err != nil {
			h.deps.Logger.Warn("could not read a group's name",
				"group", group.OpenID, "error", err)
		}
	}

	h.deps.Client.RegisterFunc(qqbotsdk.EventGroupMemberAdd, h.onJoin)
	h.deps.Client.RegisterFunc(qqbotsdk.EventGroupMemberRemove, h.onLeave)
	h.deps.Logger.Info("members coming and going are being recorded",
		"groups", len(h.deps.Groups))
	return nil
}

// Close implements feature.Feature.
func (h *handler) Close(context.Context) error { return nil }

// onJoin records a member joining.
func (h *handler) onJoin(ctx context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		return err
	}
	data, ok := value.(*qqbotsdk.GroupMemberAddData)
	if !ok {
		return fmt.Errorf("the member log got %T for a join", value)
	}
	return h.record(ctx, data.GroupOpenID, data.MemberOpenID, store.MemberJoined,
		data.Timestamp)
}

// onLeave records a member leaving.
//
// Whether they left or were removed is not knowable from here. The platform
// reports both as this one event and puts nobody else in it -- there is no
// operator to blame -- so the log says "left" and leaves the question open rather
// than guessing at it.
func (h *handler) onLeave(ctx context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		return err
	}
	data, ok := value.(*qqbotsdk.GroupMemberRemoveData)
	if !ok {
		return fmt.Errorf("the member log got %T for a departure", value)
	}
	return h.record(ctx, data.GroupOpenID, data.MemberOpenID, store.MemberLeft,
		data.Timestamp)
}

// record writes one event down.
//
// A failure is logged and swallowed: the handler is on the path that delivers
// every event, and a log that cannot be written is not a reason to stall the
// groups behind it. The event is gone either way -- there is no second chance at
// it -- so the log line is what is left, which is worth having.
func (h *handler) record(ctx context.Context, groupOpenID, memberOpenID, kind string,
	at int64) error {
	if strings.TrimSpace(groupOpenID) == "" || strings.TrimSpace(memberOpenID) == "" {
		// An event with nobody in it says nothing, and a row with an empty
		// member would be a row that cannot be read back as anything.
		return nil
	}
	name, err := h.groupName(ctx, groupOpenID)
	if err != nil {
		// Recorded anyway, without the name: the departure is the fact that
		// cannot be recovered, and the name is the part that can wait.
		h.deps.Logger.Warn("could not read a group's name for the member log",
			"group", groupOpenID, "error", err)
	}
	qq, _ := h.deps.GroupQQID(groupOpenID)

	events := h.memberEvents()
	if events == nil {
		// No data layer, so nowhere to write. Said once per event rather than
		// failed, because the feature is configured and pretending otherwise
		// would hide the events silently.
		h.deps.Logger.Warn("this bot has no data layer, so member events are not kept",
			"group", groupOpenID, "member", memberOpenID, "kind", kind)
		return nil
	}
	if err := events.Record(ctx, store.MemberEvent{
		GroupOpenID:  groupOpenID,
		GroupName:    name,
		GroupQQID:    qq,
		MemberOpenID: memberOpenID,
		Kind:         kind,
		EventAt:      at,
	}); err != nil {
		h.deps.Logger.Error("could not record a member event",
			"group", groupOpenID, "member", memberOpenID, "kind", kind, "error", err)
		return nil
	}
	h.deps.Logger.Info("a member event was recorded",
		"group", groupOpenID, "group_name", name, "member", memberOpenID, "kind", kind)
	return nil
}

// memberEvents is the log, or nil when this deployment has none.
func (h *handler) memberEvents() store.MemberEventStore {
	if h.deps.Store == nil {
		return nil
	}
	return h.deps.Store.MemberEvents()
}

// groupName returns what a group is called, reading it once.
func (h *handler) groupName(ctx context.Context, groupOpenID string) (string, error) {
	h.mu.Lock()
	name, known := h.names[groupOpenID]
	h.mu.Unlock()
	if known {
		return name, nil
	}

	// Bounded here rather than trusted to the caller: this runs inside an event
	// handler, where the request comes from the platform and not from a person
	// waiting to see something happen.
	lookupCtx, cancel := context.WithTimeout(ctx, groupNameTimeout)
	defer cancel()
	info, err := h.deps.Client.GetGroupInfo(lookupCtx, groupOpenID)
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	h.names[groupOpenID] = info.GroupName
	h.mu.Unlock()
	return info.GroupName, nil
}
