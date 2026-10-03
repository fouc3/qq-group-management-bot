package memberlog

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

const (
	testGroupOpenID = "GROUP-OPENID"
	testGroupName   = "苹果社区AI中转站"
	testMember      = "MEMBER-OPENID"
)

// harness holds one built feature over a real database, with a stub platform
// behind it.
//
// The database is real because the record is the whole feature: a log written to
// a map would agree with whatever the code happened to do.
type harness struct {
	t       *testing.T
	feature *handler
	client  *qqbotsdk.Client
	store   store.Store

	mu       sync.Mutex
	nameFail bool
	lookups  []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.lookups = append(h.lookups, r.URL.Path)
		fail := h.nameFail
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"stub failure"}`))
			return
		}
		// The only read this feature makes is the group's name.
		payload, _ := json.Marshal(map[string]any{
			"group_openid": testGroupOpenID,
			"group_name":   testGroupName,
		})
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	client, err := qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AccessToken: "test-token",
		BaseURL:     server.URL,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	h.client = client

	opened, err := store.Open(context.Background(), store.Config{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "members.db"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { opened.Close() })
	h.store = opened

	instance, err := New(sectionNode(t, "enabled: true\n"), feature.Deps{
		Client: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  opened,
		Groups: config.Groups{{OpenID: testGroupOpenID, QQGroupID: 1082100371}},
	})
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	h.feature = instance.(*handler)
	if err := h.feature.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return h
}

// sectionNode decodes a YAML section, which is what the registry hands over.
func sectionNode(t *testing.T, text string) yaml.Node {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(text), &document); err != nil {
		t.Fatalf("decoding the section: %v", err)
	}
	if len(document.Content) == 0 {
		t.Fatalf("the section %q produced no node", text)
	}
	return *document.Content[0]
}

// deliver hands one member event to the dispatcher, for the configured group.
func (h *harness) deliver(eventType, memberOpenID string, at int64) {
	h.t.Helper()
	h.deliverFor(eventType, testGroupOpenID, memberOpenID, at)
}

// deliverFor is the same for a group of the caller's choosing, which is how the
// path for a group nothing was read for at startup is reached.
func (h *harness) deliverFor(eventType, groupOpenID, memberOpenID string, at int64) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"timestamp":     at,
		"group_openid":  groupOpenID,
		"member_openid": memberOpenID,
	})
	payload := &qqbotsdk.Payload{
		ID:   "EVENT-ID",
		Op:   qqbotsdk.OpDispatch,
		Type: eventType,
		Data: body,
	}
	if err := h.client.Dispatcher().DispatchSync(context.Background(),
		qqbotsdk.NewEvent(payload, "test")); err != nil {
		h.t.Fatalf("dispatching %s: %v", eventType, err)
	}
}

// events returns what the log holds.
func (h *harness) events() []store.MemberEvent {
	h.t.Helper()
	events, err := h.store.MemberEvents().List(context.Background(), 50)
	if err != nil {
		h.t.Fatalf("listing member events: %v", err)
	}
	return events
}

// TestJoinsAndDeparturesAreRecorded covers the two events this feature exists for.
//
// The departure is the one that matters: the platform refuses this application the
// member-list endpoints, so a member who left cannot be found anywhere else, ever.
func TestJoinsAndDeparturesAreRecorded(t *testing.T) {
	h := newHarness(t)
	at := time.Now().Add(-time.Minute).Unix()

	h.deliver(qqbotsdk.EventGroupMemberAdd, "JOINER-OPENID", at)
	h.deliver(qqbotsdk.EventGroupMemberRemove, "LEAVER-OPENID", at+30)

	events := h.events()
	if len(events) != 2 {
		t.Fatalf("the log holds %d events, want two: %+v", len(events), events)
	}
	latest, earliest := events[0], events[1]
	if latest.MemberOpenID != "LEAVER-OPENID" || latest.Kind != store.MemberLeft {
		t.Errorf("latest = %+v, want the departure", latest)
	}
	if earliest.MemberOpenID != "JOINER-OPENID" || earliest.Kind != store.MemberJoined {
		t.Errorf("earliest = %+v, want the join", earliest)
	}
	// The group's name is stored, not referenced: a name looked up later is a
	// name the group may have changed by then.
	for _, event := range events {
		if event.GroupName != testGroupName || event.GroupQQID != 1082100371 {
			t.Errorf("event = %+v, want the group it happened in", event)
		}
		if event.EventAt == 0 {
			t.Errorf("event = %+v, want the platform's own time", event)
		}
	}
}

// TestAnEventWithNobodyInItIsNotStored covers a member event that names no member.
//
// A row with an empty member openid cannot be read back as anything, and the
// platform has been known to send fields it does not document.
func TestAnEventWithNobodyInItIsNotStored(t *testing.T) {
	h := newHarness(t)
	h.deliver(qqbotsdk.EventGroupMemberAdd, "", time.Now().Unix())
	if events := h.events(); len(events) != 0 {
		t.Errorf("the log holds %+v, want nothing", events)
	}
}

// TestTheNameIsReadOncePerGroup covers the lookup the log makes.
//
// An event handler is on the path that delivers every event, so the name is read
// once and kept rather than fetched again for each of them.
func TestTheNameIsReadOncePerGroup(t *testing.T) {
	h := newHarness(t)

	h.deliver(qqbotsdk.EventGroupMemberAdd, "FIRST", 1791000000)
	h.deliver(qqbotsdk.EventGroupMemberAdd, "SECOND", 1791000060)

	h.mu.Lock()
	lookups := len(h.lookups)
	h.mu.Unlock()
	// One at startup, and none after it.
	if lookups != 1 {
		t.Errorf("the group's name was read %d times, want once", lookups)
	}
}

// TestAnUnreadableNameStillRecordsTheEvent covers what happens when the one lookup
// a group needs fails.
//
// The event is the fact that cannot be recovered -- there is no second chance at a
// departure -- so it is written down with an empty name rather than dropped. The
// failure is deliberately not remembered either: a group whose name could not be
// read once is one whose name is worth asking about again.
func TestAnUnreadableNameStillRecordsTheEvent(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	h.nameFail = true
	h.mu.Unlock()

	// A group the configuration does not name, so nothing was read for it at
	// startup and the lookup happens now -- and fails.
	const unconfigured = "A-GROUP-NOTHING-READ-FOR"
	h.deliverFor(qqbotsdk.EventGroupMemberAdd, unconfigured, "SOMEONE", 1791000000)
	h.deliverFor(qqbotsdk.EventGroupMemberRemove, unconfigured, "SOMEONE-ELSE", 1791000060)

	events := h.events()
	if len(events) != 2 {
		t.Fatalf("the log holds %d events, want both: %+v", len(events), events)
	}
	if events[0].MemberOpenID != "SOMEONE-ELSE" || events[0].Kind != store.MemberLeft {
		t.Errorf("events[0] = %+v, want the departure", events[0])
	}
	for _, event := range events {
		if event.GroupName != "" {
			t.Errorf("event = %+v, want no name rather than an invented one", event)
		}
		if event.GroupOpenID != unconfigured {
			t.Errorf("event = %+v, want the group it happened in", event)
		}
	}
}

// TestAnEventDeliveredTwiceIsOneRow covers a reconnect that replays what was
// already delivered.
func TestAnEventDeliveredTwiceIsOneRow(t *testing.T) {
	h := newHarness(t)
	h.deliver(qqbotsdk.EventGroupMemberAdd, testMember, 1791000000)
	h.deliver(qqbotsdk.EventGroupMemberAdd, testMember, 1791000000)

	if events := h.events(); len(events) != 1 {
		t.Errorf("the log holds %d events, want one: %+v", len(events), events)
	}
}

// TestTheFeatureAsksOnlyForMemberEvents covers the intents it declares.
//
// Intents are unioned and every feature is handed every event it asked for, so a
// feature that asks for more than it needs is handed more than it needs.
func TestTheFeatureAsksOnlyForMemberEvents(t *testing.T) {
	h := newHarness(t)
	if got := h.feature.Intents(); got != qqbotsdk.IntentGroupMemberEvent {
		t.Errorf("intents = %d, want only the member events", got)
	}
	if h.feature.Name() != Name {
		t.Errorf("name = %q, want %q", h.feature.Name(), Name)
	}
}
