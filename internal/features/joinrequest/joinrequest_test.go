package joinrequest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

const (
	testGroupOpenID = "GROUP-OPENID"
	testApplicant   = "APPLICANT-OPENID"
)

// stubBlacklist answers whatever a test tells it to.
type stubBlacklist struct {
	barred bool
	err    error
	seen   []Applicant
}

func (s *stubBlacklist) Barred(_ context.Context, applicant Applicant) (bool, error) {
	s.seen = append(s.seen, applicant)
	return s.barred, s.err
}

// harness holds one built feature and the requests it answered.
type harness struct {
	t       *testing.T
	feature *handler
	mu      sync.Mutex
	calls   []map[string]any
}

func newHarness(t *testing.T, section string) *harness {
	t.Helper()
	h := &harness{t: t}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Only requests that carry a body are recorded, so a read the feature
		// makes for its own information does not shift what an assertion finds
		// at the front of the list.
		if r.Method != http.MethodGet {
			h.mu.Lock()
			h.calls = append(h.calls, body)
			h.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	client, err := qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AccessToken: "test-token",
		BaseURL:     server.URL,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}

	built, err := New(sectionNode(t, section), feature.Deps{
		Client: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Groups: config.Groups{{OpenID: testGroupOpenID, QQGroupID: 100000001}},
	})
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	instance, ok := built.(*handler)
	if !ok {
		t.Fatalf("built %T, want *handler", built)
	}
	h.feature = instance
	return h
}

// sectionNode decodes a YAML section, which is what the registry hands over.
func sectionNode(t *testing.T, section string) yaml.Node {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(section), &node); err != nil {
		t.Fatalf("parsing the section: %v", err)
	}
	return *node.Content[0]
}

// request delivers one join request from a plain applicant.
func (h *harness) request() error {
	h.t.Helper()
	body := `{
		"group_openid": "` + testGroupOpenID + `",
		"join_request_id": "REQUEST-1",
		"member_openid": "` + testApplicant + `",
		"union_openid": "UNION-1",
		"username": "申请人"
	}`
	return h.feature.onJoinRequest(context.Background(), &qqbotsdk.Event{
		Payload: &qqbotsdk.Payload{
			ID:   "EVENT-1",
			Type: string(qqbotsdk.EventGroupJoinRequest),
			Data: json.RawMessage(body),
		},
	})
}

// answered returns the op the request was answered with, or "" when it was left
// alone.
func (h *harness) answered() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) == 0 {
		return ""
	}
	op, _ := h.calls[0]["op"].(string)
	return op
}

// TestABarredApplicantIsRefusedWhateverTheAction covers the check that has to
// come first: action: approve admits everybody, and somebody on the list is
// exactly who it must not admit.
func TestABarredApplicantIsRefusedWhateverTheAction(t *testing.T) {
	h := newHarness(t, "enabled: true\naction: approve\n")
	blacklist := &stubBlacklist{barred: true}
	h.feature.blacklist = blacklist

	if err := h.request(); err != nil {
		t.Fatalf("onJoinRequest: %v", err)
	}
	if got := h.answered(); got != ActionDecline {
		t.Errorf("answered %q, want %q for a barred applicant", got, ActionDecline)
	}
	// The check is reached with the applicant's own identity, or a real list
	// could never be keyed on it.
	if len(blacklist.seen) != 1 || blacklist.seen[0].MemberOpenID != testApplicant {
		t.Errorf("blacklist saw %+v, want the applicant's member openid", blacklist.seen)
	}
}

// TestAnApplicantWhoIsNotBarredIsApproved covers the automatic approval this
// feature exists for, so the blacklist cannot be satisfied by refusing all.
func TestAnApplicantWhoIsNotBarredIsApproved(t *testing.T) {
	h := newHarness(t, "enabled: true\naction: approve\n")

	if err := h.request(); err != nil {
		t.Fatalf("onJoinRequest: %v", err)
	}
	if got := h.answered(); got != ActionApprove {
		t.Errorf("answered %q, want %q", got, ActionApprove)
	}
}

// TestAFailedBlacklistLookupLeavesTheRequestToAPerson covers the failure policy:
// unknown is not a decision, so neither answer is given.
func TestAFailedBlacklistLookupLeavesTheRequestToAPerson(t *testing.T) {
	h := newHarness(t, "enabled: true\naction: approve\n")
	h.feature.blacklist = &stubBlacklist{err: errStub}

	if err := h.request(); err != nil {
		t.Fatalf("onJoinRequest: %v", err)
	}
	if got := h.answered(); got != "" {
		t.Errorf("answered %q, want nothing when the list cannot be read", got)
	}
}

// TestABarredApplicantIsNotToldTheyAreOnAList covers the reason sent: it must be
// the neutral one, not the configured reject_reason, which is meant for the
// ordinary decline.
func TestABarredApplicantIsNotToldTheyAreOnAList(t *testing.T) {
	h := newHarness(t, "enabled: true\naction: approve\n")
	h.feature.blacklist = &stubBlacklist{barred: true}

	if err := h.request(); err != nil {
		t.Fatalf("onJoinRequest: %v", err)
	}
	h.mu.Lock()
	reason, _ := h.calls[0]["reject_reason"].(string)
	h.mu.Unlock()
	if !strings.Contains(reason, "暂不接受") {
		t.Errorf("reject_reason = %q, want the neutral reason", reason)
	}
}

// TestTheEmptyBlacklistBarsNobody covers the implementation the feature runs on
// today: the hook exists, is the first thing a request passes through, and
// answers "no" until it is filled in.
func TestTheEmptyBlacklistBarsNobody(t *testing.T) {
	barred, err := emptyBlacklist{}.Barred(context.Background(), Applicant{})
	if err != nil || barred {
		t.Errorf("Barred() = %v, %v; want false, nil", barred, err)
	}
}

// TestAnIgnoredRequestIsNotAnswered covers that the check did not turn the
// default action into an answer: ignore still leaves it to a person.
func TestAnIgnoredRequestIsNotAnswered(t *testing.T) {
	h := newHarness(t, "enabled: true\n")

	if err := h.request(); err != nil {
		t.Fatalf("onJoinRequest: %v", err)
	}
	if got := h.answered(); got != "" {
		t.Errorf("answered %q, want nothing for the default action", got)
	}
}

var errStub = errors.New("stub failure")

// TestTheActionIsPerGroup covers the reason the override exists: one section,
// two groups, a different decision each.
func TestTheActionIsPerGroup(t *testing.T) {
	h := newHarness(t, `
enabled: true
action: approve
groups:
  GROUP-OPENID:
    action: ignore
`)

	if got := h.feature.cfg.actionFor(testGroupOpenID); got != ActionIgnore {
		t.Errorf("actionFor(the named group) = %q, want %q", got, ActionIgnore)
	}
	if got := h.feature.cfg.actionFor("ANOTHER-GROUP"); got != ActionApprove {
		t.Errorf("actionFor(a group the file does not name) = %q, want the default %q",
			got, ActionApprove)
	}

	// And the grouping is acted on, not only resolved.
	if err := h.request(); err != nil {
		t.Fatalf("onJoinRequest: %v", err)
	}
	if got := h.answered(); got != "" {
		t.Errorf("answered %q, want nothing for a group set to ignore", got)
	}
}

// TestATypoInAGroupActionFailsAtStartup covers that the override is validated
// before anybody is waiting at the door.
func TestATypoInAGroupActionFailsAtStartup(t *testing.T) {
	_, err := New(sectionNode(t, `
enabled: true
action: approve
groups:
  GROUP-OPENID:
    action: aprove
`), feature.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil {
		t.Error("a misspelled action must be refused")
	}
	if err != nil && !strings.Contains(err.Error(), "GROUP-OPENID") {
		t.Errorf("err = %v, want it to name the group at fault", err)
	}
}
