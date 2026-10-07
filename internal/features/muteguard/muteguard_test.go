package muteguard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/mutelog"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

const (
	testGroup  = "GROUP-OPENID"
	testOther  = "OTHER-GROUP-OPENID"
	testMember = "MEMBER-OPENID"
	testSecond = "SECOND-MEMBER-OPENID"
)

// platform is the group's mute state as the platform would report and change it.
//
// It answers the two calls this feature makes and nothing else, and it keeps what
// it was asked to do rather than what the feature intended -- the difference
// between the two is what a test is for.
type platform struct {
	mu sync.Mutex
	// muted is the members each group currently reports as muted.
	muted map[string][]qqbotsdk.MemberMuteState
	// rules is the whole-group mute each group reports, when it has one.
	rules map[string]*qqbotsdk.GroupMuteRule
	// reads counts the looks at each group, so that "not looked at" is checkable.
	reads map[string]int
	// failRead makes every read answer with a failure.
	failRead bool
	// lifted is every member the bot asked to have unmuted, in the order asked.
	lifted []string
	// calls counts the mute requests, so that batching is checkable.
	calls []int
}

func newPlatform() *platform {
	return &platform{
		muted: map[string][]qqbotsdk.MemberMuteState{},
		rules: map[string]*qqbotsdk.GroupMuteRule{},
		reads: map[string]int{},
	}
}

// server is a stub of the two endpoints this feature uses.
func (p *platform) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		groupOpenID := groupInPath(t, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		p.mu.Lock()
		if r.Method == http.MethodGet {
			p.reads[groupOpenID]++
			if p.failRead {
				p.mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"stub failure"}`))
				return
			}
			state := qqbotsdk.GroupRestrictChatSetting{
				GlobalRule: p.rules[groupOpenID],
				Members:    p.muted[groupOpenID],
			}
			p.mu.Unlock()
			payload, err := json.Marshal(state)
			if err != nil {
				t.Errorf("encoding the mute state: %v", err)
				return
			}
			_, _ = w.Write(payload)
			return
		}

		var request qqbotsdk.SetGroupMemberMuteRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			p.mu.Unlock()
			t.Errorf("reading the mute request: %v", err)
			return
		}
		p.calls = append(p.calls, len(request.Members))
		for _, change := range request.Members {
			if change.Op == qqbotsdk.MemberMuteDelete {
				p.lifted = append(p.lifted, change.MemberOpenID)
			}
		}
		p.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// groupInPath reads the group openid out of a request path.
func groupInPath(t *testing.T, path string) string {
	t.Helper()
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, part := range parts {
		if part == "groups" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	t.Fatalf("no group in the request path %q", path)
	return ""
}

// setMuted makes a group report these members as muted.
func (p *platform) setMuted(groupOpenID string, memberOpenIDs ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	states := make([]qqbotsdk.MemberMuteState, 0, len(memberOpenIDs))
	for _, memberOpenID := range memberOpenIDs {
		states = append(states, qqbotsdk.MemberMuteState{
			MemberOpenID: memberOpenID,
			MuteExpireAt: time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}
	p.muted[groupOpenID] = states
}

// snapshot is what the platform has been asked to do, and what it was asked.
func (p *platform) snapshot() (reads map[string]int, lifted []string, calls []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	reads = map[string]int{}
	for groupOpenID, count := range p.reads {
		reads[groupOpenID] = count
	}
	return reads, append([]string(nil), p.lifted...), append([]int(nil), p.calls...)
}

// counting is a logger that keeps the lines written, so that "said once" is
// something a test can check rather than assume.
type counting struct {
	mu    sync.Mutex
	lines []string
}

func (c *counting) Enabled(context.Context, slog.Level) bool { return true }
func (c *counting) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *counting) WithGroup(string) slog.Handler            { return c }

func (c *counting) Handle(_ context.Context, record slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, record.Message)
	return nil
}

// said counts the lines containing the given words.
func (c *counting) said(text string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, line := range c.lines {
		if strings.Contains(line, text) {
			count++
		}
	}
	return count
}

// harness is one built feature over a real database and a stub platform.
//
// The database is real because half of what this feature decides is read from it:
// the holds a member is verifying under are rows, and a stub that answered from a
// map would agree with whatever the code happened to look for.
type harness struct {
	t        *testing.T
	guard    *handler
	platform *platform
	logs     *counting
	store    store.Store
	mutes    *mutelog.Log
}

func newHarness(t *testing.T, section string) *harness {
	t.Helper()
	h := &harness{t: t, platform: newPlatform(), logs: &counting{}, mutes: mutelog.New()}

	server := h.platform.server(t)
	client, err := qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AccessToken: "test-token",
		BaseURL:     server.URL,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}

	opened, err := store.Open(context.Background(), store.Config{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "test.db"),
	})
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	h.store = opened

	deps := feature.Deps{
		Client: client,
		Logger: slog.New(h.logs),
		Groups: config.Groups{{OpenID: testGroup}, {OpenID: testOther}},
		Store:  opened,
		Mutes:  h.mutes,
	}
	built, err := New(sectionNode(t, section), deps)
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	h.guard = built.(*handler)
	return h
}

// sectionNode turns a section written as YAML into the node a feature is built
// from, which is what the application hands over.
func sectionNode(t *testing.T, body string) yaml.Node {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(body), &node); err != nil {
		t.Fatalf("reading the section: %v", err)
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return *node.Content[0]
	}
	return node
}

// sweep runs one pass, the way the timer does.
func (h *harness) sweep() {
	h.t.Helper()
	h.guard.sweep(context.Background())
}

// hold writes a verification hold the way the verification feature would.
func (h *harness) hold(memberOpenID string) {
	h.t.Helper()
	if err := h.store.Pending().Put(context.Background(), store.Pending{
		Token:        "TOKEN-" + memberOpenID,
		GroupOpenID:  testGroup,
		MemberOpenID: memberOpenID,
		HeldUntil:    time.Now().Add(24 * time.Hour).Unix(),
	}); err != nil {
		h.t.Fatalf("writing a hold: %v", err)
	}
}

// oneMember is a section naming one member in the group under test.
const oneMember = `
enabled: true
interval_seconds: 30
members:
  - "` + testMember + `"
groups:
  "` + testGroup + `":
    members:
      - "` + testMember + `"
`

// oneGroupMeanwhile names the member in one group only, so that a test about what
// is said about that group is not also hearing about the other one.
const oneGroupMeanwhile = `
enabled: true
interval_seconds: 30
groups:
  "` + testGroup + `":
    members:
      - "` + testMember + `"
`

// TestAMuteSomebodyElseAppliedIsLifted covers what the feature is for: a member
// the group named is muted by somebody who is not this bot, so the mute is lifted.
func TestAMuteSomebodyElseAppliedIsLifted(t *testing.T) {
	h := newHarness(t, oneMember)
	h.platform.setMuted(testGroup, testMember)

	h.sweep()

	_, lifted, _ := h.platform.snapshot()
	if len(lifted) != 1 || lifted[0] != testMember {
		t.Errorf("lifted %v, want the named member and nobody else", lifted)
	}
}

// TestAMuteTheBotAppliedIsLeftAlone covers the first half of "unless this bot
// muted them": an administrator asked for this one by name.
func TestAMuteTheBotAppliedIsLeftAlone(t *testing.T) {
	h := newHarness(t, oneMember)
	h.platform.setMuted(testGroup, testMember)
	h.mutes.Record(testGroup, testMember, time.Now().Add(time.Hour))

	h.sweep()

	if _, lifted, _ := h.platform.snapshot(); len(lifted) != 0 {
		t.Errorf("lifted %v, but this bot applied that mute itself", lifted)
	}
}

// TestAMemberStillVerifyingIsLeftAlone covers the other half: the hold a member is
// verifying under is a mute this bot applied, and lifting it would be a way out of
// the verification.
func TestAMemberStillVerifyingIsLeftAlone(t *testing.T) {
	h := newHarness(t, oneMember)
	h.platform.setMuted(testGroup, testMember)
	h.hold(testMember)

	h.sweep()

	if _, lifted, _ := h.platform.snapshot(); len(lifted) != 0 {
		t.Errorf("lifted %v, who is still verifying", lifted)
	}
}

// TestAMuteOfSomebodyTheGroupDidNotNameIsLeftAlone covers the list being the whole
// of the rule: a group that names one member is not asking for everybody to speak.
func TestAMuteOfSomebodyTheGroupDidNotNameIsLeftAlone(t *testing.T) {
	h := newHarness(t, oneMember)
	h.platform.setMuted(testGroup, testSecond)

	h.sweep()

	if _, lifted, _ := h.platform.snapshot(); len(lifted) != 0 {
		t.Errorf("lifted %v, who the group did not name", lifted)
	}
}

// TestAGroupTheBotDoesNotManageIsNotLookedAt covers a group named in this section
// that the bot's own group list does not carry: nothing is delivered there, and a
// mute in it is not this bot's to lift.
func TestAGroupTheBotDoesNotManageIsNotLookedAt(t *testing.T) {
	h := newHarness(t, `
enabled: true
groups:
  "NOT-MANAGED":
    members:
      - "`+testMember+`"
`)
	h.platform.setMuted("NOT-MANAGED", testMember)

	h.sweep()

	reads, lifted, _ := h.platform.snapshot()
	if reads["NOT-MANAGED"] != 0 {
		t.Error("a group the bot does not manage was looked at")
	}
	if len(lifted) != 0 {
		t.Errorf("lifted %v in a group the bot does not manage", lifted)
	}
}

// TestAGroupWithNobodyNamedIsNotLookedAt covers the call the file does not ask for:
// a group with an empty list has nothing to do with the answer, and a look is an
// API call the platform counts.
func TestAGroupWithNobodyNamedIsNotLookedAt(t *testing.T) {
	h := newHarness(t, `
enabled: true
interval_seconds: 30
groups:
  "`+testOther+`":
    members: []
`)
	h.platform.setMuted(testGroup, testMember)

	h.sweep()

	if reads, _, _ := h.platform.snapshot(); len(reads) != 0 {
		t.Errorf("groups were looked at (%v) although nobody was named", reads)
	}
}

// TestTheSharedListReachesEveryManagedGroup covers the list written once: it is
// what every group that does not name its own gets.
func TestTheSharedListReachesEveryManagedGroup(t *testing.T) {
	h := newHarness(t, `
enabled: true
interval_seconds: 30
members:
  - "`+testMember+`"
`)
	h.platform.setMuted(testGroup, testMember)

	h.sweep()

	reads, lifted, _ := h.platform.snapshot()
	if reads[testGroup] != 1 || reads[testOther] != 1 {
		t.Errorf("reads = %v, want one look at each managed group", reads)
	}
	if len(lifted) != 1 {
		t.Errorf("lifted %v, want the named member", lifted)
	}
}

// TestAGroupsOwnListReplacesTheSharedOne covers the override: naming members in a
// group is a decision about that group, not an addition to the shared list.
func TestAGroupsOwnListReplacesTheSharedOne(t *testing.T) {
	h := newHarness(t, `
enabled: true
interval_seconds: 30
members:
  - "`+testMember+`"
groups:
  "`+testGroup+`":
    members:
      - "`+testSecond+`"
`)
	h.platform.setMuted(testGroup, testMember, testSecond)

	h.sweep()

	_, lifted, _ := h.platform.snapshot()
	if len(lifted) != 1 || lifted[0] != testSecond {
		t.Errorf("lifted %v, want only the member this group named", lifted)
	}
}

// TestAGroupCanBeLeftOut covers the switch that is per group: one group opts out
// while the shared list stays in force everywhere else.
func TestAGroupCanBeLeftOut(t *testing.T) {
	h := newHarness(t, `
enabled: true
interval_seconds: 30
members:
  - "`+testMember+`"
groups:
  "`+testGroup+`":
    enabled: false
`)
	h.platform.setMuted(testGroup, testMember)

	h.sweep()

	reads, lifted, _ := h.platform.snapshot()
	if reads[testGroup] != 0 {
		t.Error("a group that was turned off was looked at")
	}
	if len(lifted) != 0 {
		t.Errorf("lifted %v although the group was turned off", lifted)
	}
}

// TestNothingIsLiftedWhenTheHoldsCannotBeRead covers the failure that has to leave
// the mutes alone: without the holds there is no telling a member who is verifying
// from one who is not, and letting the first out is worse than a delay.
func TestNothingIsLiftedWhenTheHoldsCannotBeRead(t *testing.T) {
	h := newHarness(t, oneMember)
	h.platform.setMuted(testGroup, testMember)
	if err := h.store.Close(); err != nil {
		t.Fatalf("closing the database: %v", err)
	}

	h.sweep()

	if _, lifted, _ := h.platform.snapshot(); len(lifted) != 0 {
		t.Errorf("lifted %v without knowing who is verifying", lifted)
	}
	if h.logs.said("could not be read, so nobody was lifted") == 0 {
		t.Error("the pass that could not read the holds said nothing about it")
	}
}

// TestAGroupThatCannotBeReadIsSaidOnce covers the log a persistent failure must
// not fill: a group the bot may not read would otherwise repeat the same line on
// every pass for as long as the bot runs.
func TestAGroupThatCannotBeReadIsSaidOnce(t *testing.T) {
	h := newHarness(t, oneGroupMeanwhile)
	h.platform.mu.Lock()
	h.platform.failRead = true
	h.platform.mu.Unlock()

	h.sweep()
	h.sweep()
	h.sweep()

	if count := h.logs.said("mute state could not be read"); count != 1 {
		t.Errorf("an unreadable group was reported %d time(s), want once", count)
	}
}

// TestAWholeGroupMuteIsSaidOutLoud covers the case this feature cannot answer: the
// group is muted as a whole, so the named members cannot speak either, and saying
// nothing would look exactly like the feature working.
func TestAWholeGroupMuteIsSaidOutLoud(t *testing.T) {
	h := newHarness(t, oneGroupMeanwhile)
	h.platform.mu.Lock()
	h.platform.rules[testGroup] = &qqbotsdk.GroupMuteRule{Mode: qqbotsdk.GroupMuteAlways}
	h.platform.mu.Unlock()

	h.sweep()
	h.sweep()

	if count := h.logs.said("the whole group is muted"); count != 1 {
		t.Errorf("a group-wide mute was reported %d time(s), want once", count)
	}

	// And once the group is speaking again and muted once more, it is said again:
	// the report is about a state appearing, not about the first time only.
	h.platform.mu.Lock()
	delete(h.platform.rules, testGroup)
	h.platform.mu.Unlock()
	h.sweep()
	h.platform.mu.Lock()
	h.platform.rules[testGroup] = &qqbotsdk.GroupMuteRule{Mode: qqbotsdk.GroupMuteAlways}
	h.platform.mu.Unlock()
	h.sweep()

	if count := h.logs.said("the whole group is muted"); count != 2 {
		t.Errorf("a group-wide mute appearing a second time was reported %d "+
			"time(s) in all, want 2", count)
	}
}

// TestTheLiftingIsBatched covers the request size the platform allows: more named
// members than one call may change are lifted in as many calls as it takes, in
// batches that do not exceed it.
func TestTheLiftingIsBatched(t *testing.T) {
	const many = qqbotsdk.MaxGroupMuteTargets + 5
	members := make([]string, 0, many)
	list := make([]string, 0, many)
	for i := 0; i < many; i++ {
		memberOpenID := "MEMBER-" + string(rune('A'+i))
		members = append(members, memberOpenID)
		list = append(list, `  - "`+memberOpenID+`"`)
	}
	h := newHarness(t, "enabled: true\ninterval_seconds: 30\nmembers:\n"+strings.Join(list, "\n")+"\n")
	h.platform.setMuted(testGroup, members...)

	h.sweep()

	_, lifted, calls := h.platform.snapshot()
	if len(lifted) != many {
		t.Fatalf("lifted %d members, want %d", len(lifted), many)
	}
	if len(calls) != 2 || calls[0] != qqbotsdk.MaxGroupMuteTargets || calls[1] != 5 {
		t.Errorf("the requests carried %v members, want [%d 5]",
			calls, qqbotsdk.MaxGroupMuteTargets)
	}
}

// TestAnIntervalOutsideTheBoundsIsRefused covers the settings the platform would
// punish: an interval so short it is a rate limit, and one so long a mute would
// outlive it.
func TestAnIntervalOutsideTheBoundsIsRefused(t *testing.T) {
	deps := feature.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for name, section := range map[string]string{
		"too short": "enabled: true\ninterval_seconds: 1\nmembers:\n  - \"" + testMember + "\"\n",
		"too long":  "enabled: true\ninterval_seconds: 86400\nmembers:\n  - \"" + testMember + "\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(sectionNode(t, section), deps); err == nil {
				t.Error("an interval outside the bounds was accepted")
			}
		})
	}
}

// TestAMemberNamedByNothingIsRefused covers the typo that would match nobody: an
// empty entry in a list is not a member, and it is not worth starting for.
func TestAMemberNamedByNothingIsRefused(t *testing.T) {
	deps := feature.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := New(sectionNode(t, "enabled: true\nmembers:\n  - \"   \"\n"), deps); err == nil {
		t.Error("a list with an empty member in it was accepted")
	}
}

// TestCloseWaitsForTheSweep covers the shutdown a rebuild depends on: Close must
// not return while a pass is still lifting mutes, or the replacement would be
// acting alongside an instance nobody is running.
func TestCloseWaitsForTheSweep(t *testing.T) {
	h := newHarness(t, oneMember)
	h.platform.setMuted(testGroup, testMember)
	if err := h.guard.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- h.guard.Close(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
}
