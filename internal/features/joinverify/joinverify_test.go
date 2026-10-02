package joinverify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/onebot-ext/onebot"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// Constants used across the tests.
const (
	testGroupOpenID  = "GROUP-OPENID"
	testQQGroupID    = int64(1015779284)
	testMemberOpenID = "MEMBER-OPENID"
	testJoinTime     = int64(1790926337)
	// testBotQQ is the QQ number the official bot appears as in the group, which
	// is what a message read back through OneBot must match.
	testBotQQ = int64(4016438750)
)

// call is one request the official API received.
type call struct {
	method string
	path   string
	body   map[string]any
}

// harness wires a feature against two stub servers.
type harness struct {
	t        *testing.T
	verifier *verifier
	client   *qqbotsdk.Client

	mu      sync.Mutex
	calls   []call
	members []onebot.Member
	kicks   []kick
	// forged makes the history report the notice as coming from a member
	// instead of the bot.
	forged   bool
	failSend bool
}

// kick is one removal requested from OneBot.
type kick struct {
	groupID int64
	userID  int64
}

// newHarness builds a verifier whose official calls and OneBot calls both land
// on stubs, so a test can assert on either side.
func newHarness(t *testing.T, section string) *harness {
	return newHarnessWithStore(t, section, "")
}

// newHarnessWithStore builds the feature with a data layer on a file of its own.
//
// Handing the same file to two harnesses is what a restart looks like, and it is
// the only way to check that a hold survives one.
func newHarnessWithStore(t *testing.T, section, databasePath string) *harness {
	t.Helper()
	h := &harness{t: t, members: []onebot.Member{
		{UserID: 3884506253, Nickname: "btrfs", JoinTime: testJoinTime, Role: "member"},
		{UserID: 9999999999, Nickname: "other", JoinTime: testJoinTime - 4000, Role: "member"},
	}}

	official := httptest.NewServer(http.HandlerFunc(h.serveOfficial))
	t.Cleanup(official.Close)
	oneBotServer := httptest.NewServer(http.HandlerFunc(h.serveOneBot))
	t.Cleanup(oneBotServer.Close)

	client, err := qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AccessToken: "test-token",
		BaseURL:     official.URL,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	h.client = client

	oneBotClient, err := onebot.New(onebot.Options{BaseURL: oneBotServer.URL})
	if err != nil {
		t.Fatalf("building the onebot client: %v", err)
	}

	deps := feature.Deps{
		Client:        client,
		OneBot:        oneBotClient,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Groups:        config.Groups{{OpenID: testGroupOpenID, QQGroupID: testQQGroupID}},
		BotQQ:         testBotQQ,
		JoinTolerance: 15,
	}
	if databasePath != "" {
		opened, err := store.Open(context.Background(), store.Config{
			Driver: "sqlite",
			DSN:    databasePath,
		})
		if err != nil {
			t.Fatalf("opening the store: %v", err)
		}
		t.Cleanup(func() { opened.Close() })
		deps.Store = opened
	}

	instance, err := New(sectionNode(t, section), deps)
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	h.verifier = instance.(*verifier)
	if err := h.verifier.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = h.verifier.Close(context.Background()) })
	return h
}

// serveOfficial records an official API call and answers it.
func (h *harness) serveOfficial(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	h.mu.Lock()
	h.calls = append(h.calls, call{method: r.Method, path: r.URL.Path, body: body})
	fail := h.failSend && strings.HasSuffix(r.URL.Path, "/messages")
	h.mu.Unlock()

	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"err_code":500001,"message":"boom"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

// serveOneBot answers the two OneBot actions the feature uses.
func (h *harness) serveOneBot(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch strings.TrimPrefix(r.URL.Path, "/") {
	case "get_group_member_list":
		payload, _ := json.Marshal(h.members)
		_, _ = w.Write([]byte(`{"status":"ok","retcode":0,"data":` + string(payload) + `}`))
	case "send_group_msg":
		// Kept so an accidental use of OneBot to send the notice is visible:
		// the tag it carries would arrive as plain text, with no mention to
		// resolve.
		_, _ = w.Write([]byte(`{"status":"ok","retcode":0,"data":{"message_id":7}}`))
	case "get_group_msg_history":
		// The history reflects what the official bot posted, because that is
		// the message whose mention the platform resolves. The harness lock is
		// already held by serveOneBot.
		texts := make([]string, 0, len(h.calls))
		for _, entry := range h.calls {
			if !strings.Contains(entry.path, "/messages") {
				continue
			}
			markdown, ok := entry.body["markdown"].(map[string]any)
			if !ok {
				continue
			}
			if content, ok := markdown["content"].(string); ok {
				texts = append(texts, content)
			}
		}
		forged := h.forged

		messages := make([]map[string]any, 0, len(texts))
		for _, sent := range texts {
			sender := testBotQQ
			if forged {
				sender = 3866370858 // a member standing in for a forged notice
			}
			messages = append(messages, map[string]any{
				"user_id":     sender,
				"raw_message": sent,
				"message": []map[string]any{
					// The platform resolves the mention into a real QQ number.
					{"type": "at", "data": map[string]any{"qq": "3884506253"}},
					{"type": "text", "data": map[string]any{"text": sent}},
				},
			})
		}
		payload, _ := json.Marshal(messages)
		_, _ = w.Write([]byte(`{"status":"ok","retcode":0,"data":{"messages":` + string(payload) + `}}`))
	case "set_group_kick":
		var request struct {
			GroupID int64 `json:"group_id"`
			UserID  int64 `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		h.kicks = append(h.kicks, kick{groupID: request.GroupID, userID: request.UserID})
		_, _ = w.Write([]byte(`{"status":"ok","retcode":0,"data":null}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":"failed","retcode":1404,"wording":"unknown action"}`))
	}
}

// callsOf returns the recorded calls whose path contains the fragment.
func (h *harness) callsOf(fragment string) []call {
	h.mu.Lock()
	defer h.mu.Unlock()
	var found []call
	for _, entry := range h.calls {
		if strings.Contains(entry.path, fragment) {
			found = append(found, entry)
		}
	}
	return found
}

// lastCall returns the last recorded call, failing when there is none.
func (h *harness) lastCall() call {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) == 0 {
		h.t.Fatal("no request reached the official API")
	}
	return h.calls[len(h.calls)-1]
}

// kickCount returns how many removals OneBot was asked to perform.
func (h *harness) kickCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.kicks)
}

// TestAHoldSurvivesARestart covers the promise of the state file: a hold is a
// real mute that lasts up to twenty nine days, so a restart must not forget who
// is waiting. Losing the record leaves the member muted with a button that can
// only answer 操作失败.
func TestAHoldSurvivesARestart(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "bot.db")
	section := `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
state_file: ` + filepath.Join(t.TempDir(), "pending.json") + `
`
	first := newHarnessWithStore(t, section, databasePath)
	first.join()
	token := first.tokenFromPrompt()
	if token == "" {
		t.Fatal("no token in the prompt")
	}

	// A second verifier over the same database is what a restart produces.
	second := newHarnessWithStore(t, section, databasePath)
	second.press("INTERACTION-RESTORED", token, testMemberOpenID,
		qqbotsdk.InteractionSceneGroup)

	answers := second.callsOf("/interactions/")
	if len(answers) != 1 {
		t.Fatalf("interaction answers = %d, want 1", len(answers))
	}
	if answers[0].body["code"] != float64(qqbotsdk.InteractionCodeSuccess) {
		t.Errorf("code = %v, want the restored hold to answer its button",
			answers[0].body["code"])
	}
}

// TestAReleasedHoldIsNotRestored covers a hold the platform has already lifted:
// it has nothing left to answer or act on, so it must not come back.
//
// It is written in the JSON layout the earlier build used, because reading that
// file is the only thing the layout is still for.
func TestAReleasedHoldIsNotRestored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	payload, err := json.Marshal(storedState{
		Version: stateFileVersion,
		Pending: []storedEntry{
			{
				Token: "released", GroupOpenID: testGroupOpenID,
				MemberOpenID: testMemberOpenID,
				HeldUntil:    time.Now().Add(-time.Minute),
			},
			{
				Token: "held", GroupOpenID: testGroupOpenID,
				MemberOpenID: "ANOTHER-MEMBER",
				Deadline:     time.Now().Add(time.Hour),
				HeldUntil:    time.Now().Add(time.Hour),
			},
		},
	})
	if err != nil {
		t.Fatalf("encoding the state: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("writing the state: %v", err)
	}

	restored, err := readState(path, time.Now())
	if err != nil {
		t.Fatalf("reading the state: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("restored %d entries, want only the one still held", len(restored))
	}
	if restored[0].Token != "held" {
		t.Errorf("restored %q, want the entry that is still held", restored[0].Token)
	}
}

// TestACorruptStateFileDoesNotStopTheBot covers the choice to start with none
// rather than refusing to run: refusing would leave the whole group unmanaged.
func TestACorruptStateFileDoesNotStopTheBot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
state_file: `+path+`
`)
	// Still able to hold somebody, which is what "did not stop" means.
	h.join()
	if h.tokenFromPrompt() == "" {
		t.Error("the bot did not hold a new member after a corrupt state file")
	}
}

// TestRemuteHoldsThemAgain covers the mode with no timeout: when the platform
// releases the mute, the member is muted again rather than reported or removed,
// so not verifying keeps them silent for as long as they stay.
func TestRemuteHoldsThemAgain(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: remute
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	token := h.tokenFromPrompt()
	if token == "" {
		t.Fatal("no hold was created")
	}

	// The deadline passes, which is the moment a removal would happen.
	h.verifier.mu.Lock()
	h.verifier.byToken[token].deadline = time.Now().Add(-time.Minute)
	h.verifier.mu.Unlock()

	due, renew := h.verifier.takeDueDeadlines(time.Now())
	if len(due) != 0 {
		t.Errorf("due = %d, want the deadline turned into a hold, not a report", len(due))
	}
	if len(renew) != 1 {
		t.Fatalf("renew = %d, want the expired hold renewed", len(renew))
	}
	h.verifier.renew(context.Background(), renew[0])

	mutes := 0
	for _, call := range h.callsOf("/restrict_chat_setting") {
		members, _ := call.body["members"].([]any)
		if len(members) == 0 {
			continue
		}
		if members[0].(map[string]any)["op"] != qqbotsdk.MemberMuteDelete {
			mutes++
		}
	}
	if mutes != 2 {
		t.Errorf("mutes applied = %d, want the hold applied again", mutes)
	}
	// The token is untouched, so the button the member already has still works.
	h.press("INTERACTION-AFTER-RENEW", token, testMemberOpenID,
		qqbotsdk.InteractionSceneGroup)
	answers := h.callsOf("/interactions/")
	if len(answers) == 0 {
		t.Fatal("the press after a renewal went unanswered")
	}
	if got := answers[len(answers)-1].body["code"]; got != float64(qqbotsdk.InteractionCodeSuccess) {
		t.Errorf("code = %v, want the button to still work after a renewal", got)
	}
}

// TestRemuteIgnoresTheHoldExpiry covers that the hold's own expiry is not the
// trigger: the deadline is, and the configuration keeps it ahead of the expiry.
func TestRemuteIgnoresTheHoldExpiry(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: remute
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	h.verifier.mu.Lock()
	for _, entry := range h.verifier.byToken {
		// The hold has run out, but the deadline has not been reached.
		entry.heldUntil = time.Now().Add(-time.Minute)
		entry.deadline = time.Now().Add(time.Hour)
	}
	h.verifier.mu.Unlock()

	due, renew := h.verifier.takeDueDeadlines(time.Now())
	if len(due) != 0 || len(renew) != 0 {
		t.Errorf("due = %d and renew = %d, want neither: the deadline is the trigger, "+
			"and the configuration keeps it ahead of the hold's own expiry",
			len(due), len(renew))
	}
}

// TestRemuteActsOnTheDeadline covers the trigger: the member is held again when
// the deadline passes, which is the same moment a removal would happen, rather
// than when the hold's own expiry approaches.
func TestRemuteActsOnTheDeadline(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: remute
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	token := h.tokenFromPrompt()
	if token == "" {
		t.Fatal("no hold was created")
	}

	for name, deadline := range map[string]time.Duration{
		"before the deadline": time.Hour,
		"after the deadline":  -time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			h.verifier.mu.Lock()
			h.verifier.byToken[token].deadline = time.Now().Add(deadline)
			h.verifier.mu.Unlock()

			due, renew := h.verifier.takeDueDeadlines(time.Now())
			if len(due) != 0 {
				t.Errorf("due = %d, want nobody reported in remute mode", len(due))
			}
			wantRenewed := deadline < 0
			if got := len(renew) == 1; got != wantRenewed {
				t.Errorf("renewed = %v with the deadline %s, want %v",
					got, name, wantRenewed)
			}
		})
	}
}

// TestAMessageFromAHeldMemberIsRecalled covers the defence against the gap
// between a member joining and their hold taking effect: while the platform is
// still applying the mute the member can speak, so what they say is recalled.
func TestAMessageFromAHeldMemberIsRecalled(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	h.dispatch(qqbotsdk.EventGroupMessageCreate, `{
		"id": "MSG-HELD-1",
		"author": {"member_openid": "`+testMemberOpenID+`"},
		"content": "大家好",
		"group_openid": "`+testGroupOpenID+`",
		"timestamp": "2026-10-02T10:00:00+08:00"
	}`)

	if got := len(h.callsOf("/messages/MSG-HELD-1")); got != 1 {
		t.Errorf("recalls of the held member's message = %d, want 1", got)
	}
}

// TestAMessageFromAnybodyElseIsLeftAlone covers the other side: this is a
// defence for members being held, not a general moderation tool that quietly
// deletes what people say.
func TestAMessageFromAnybodyElseIsLeftAlone(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	h.dispatch(qqbotsdk.EventGroupMessageCreate, `{
		"id": "MSG-OTHER-1",
		"author": {"member_openid": "SOMEONE-ELSE"},
		"content": "大家好",
		"group_openid": "`+testGroupOpenID+`",
		"timestamp": "2026-10-02T10:00:00+08:00"
	}`)

	if got := len(h.callsOf("/messages/MSG-OTHER-1")); got != 0 {
		t.Errorf("recalls = %d, want none for a member who is not being held", got)
	}
}

// TestARenewalMovesTheLiveEntry covers what a renewal has to change: the entry
// the next sweep and the state file actually look at, with both stamps moved.
//
// Writing to the copy the sweep handed over left the live entry untouched, so
// the hold looked due on every tick: the member was muted again every minute,
// and the new stamps never reached the state file.
func TestARenewalMovesTheLiveEntry(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: remute
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	token := h.tokenFromPrompt()

	// The deadline has passed, so a renewal is due.
	h.verifier.mu.Lock()
	h.verifier.byToken[token].deadline = time.Now().Add(-time.Minute)
	h.verifier.mu.Unlock()

	_, renew := h.verifier.takeDueDeadlines(time.Now())
	if len(renew) != 1 {
		t.Fatalf("renew = %d, want the hold renewed", len(renew))
	}
	h.verifier.renew(context.Background(), renew[0])

	h.verifier.mu.Lock()
	got := h.verifier.byToken[token].heldUntil
	h.verifier.mu.Unlock()

	// The mute is applied afresh from now, and the deadline is pushed a whole
	// deadline_hours forward so the cycle repeats.
	want := time.Now().Add(time.Duration(DefaultMuteMinutes) * time.Minute)
	if diff := got.Sub(want); diff > time.Minute || diff < -time.Minute {
		t.Errorf("held_until = %s, want about %s: the mute applied afresh",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	h.verifier.mu.Lock()
	next := h.verifier.byToken[token].deadline
	h.verifier.mu.Unlock()
	if !next.After(time.Now().Add(time.Hour)) {
		t.Errorf("deadline = %s, want it pushed forward so the check repeats",
			next.Format(time.RFC3339))
	}
	if _, again := h.verifier.takeDueDeadlines(time.Now()); len(again) != 0 {
		t.Errorf("renew = %d on the very next sweep, want none", len(again))
	}
}

// TestSimulatingADeadlineInRemuteModeHoldsThemAgain covers the command that runs
// the deadline path on demand.
//
// The mode's action is a renewal, so the command has to perform one. Falling
// through both of the removal and report cases left it answering "done" while
// doing nothing at all -- and since the confirmation was then dropped at the
// operator's request, there was nothing to see anywhere.
func TestSimulatingADeadlineInRemuteModeHoldsThemAgain(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: remute
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	token := h.tokenFromPrompt()
	if token == "" {
		t.Fatal("no hold was created")
	}

	if err := h.verifier.SimulateDeadline(context.Background(), testGroupOpenID,
		testMemberOpenID); err != nil {
		t.Fatalf("SimulateDeadline: %v", err)
	}

	mutes := 0
	for _, call := range h.callsOf("/restrict_chat_setting") {
		members, _ := call.body["members"].([]any)
		if len(members) == 0 {
			continue
		}
		if members[0].(map[string]any)["op"] != qqbotsdk.MemberMuteDelete {
			mutes++
		}
	}
	if mutes != 2 {
		t.Errorf("mutes applied = %d, want the hold applied again", mutes)
	}
	h.verifier.mu.Lock()
	next := h.verifier.byToken[token].deadline
	h.verifier.mu.Unlock()
	if !next.After(time.Now().Add(time.Hour)) {
		t.Errorf("deadline = %s, want it pushed forward", next.Format(time.RFC3339))
	}
}

// TestTheLegacyFileIsImportedOnce covers the changeover and the trap inside it.
//
// The marker decides whether to import, not an empty table: the table empties
// again once the last member verifies, and a check based on it would read the
// stale file a second time and bring holds that are long over back to life.
func TestTheLegacyFileIsImportedOnce(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "bot.db")
	legacy := filepath.Join(t.TempDir(), "pending.json")
	now := time.Now()
	payload, err := json.Marshal(storedState{
		Version: stateFileVersion,
		Pending: []storedEntry{{
			Token:        "LEGACY-1",
			GroupOpenID:  testGroupOpenID,
			MemberOpenID: "LEGACY-MEMBER",
			JoinedAt:     now.Unix(),
			Deadline:     now.Add(time.Hour),
			HeldUntil:    now.Add(24 * time.Hour),
			Settings: Settings{
				MuteMinutes:   1440,
				DeadlineHours: 1,
				OnDeadline:    OnDeadlineNotify,
				NotifyMembers: []string{"ADMIN-OPENID"},
				Prompt:        "{at} 请验证",
				ButtonLabel:   "✅ 我是真人",
				VisitedLabel:  "已验证",
			},
		}},
	})
	if err != nil {
		t.Fatalf("encoding the older file: %v", err)
	}
	if err := os.WriteFile(legacy, payload, 0o600); err != nil {
		t.Fatalf("writing the older file: %v", err)
	}
	section := `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
state_file: ` + legacy + `
`

	first := newHarnessWithStore(t, section, databasePath)
	imported, found := first.verifier.lookup("LEGACY-1")
	if !found {
		t.Fatal("the record in the older file was not imported")
	}
	// The settings travel with it: a member who is already muted must keep the
	// rules they were held under, not pick up the current defaults.
	if imported.settings.MuteMinutes != 1440 || imported.settings.ButtonLabel != "✅ 我是真人" {
		t.Errorf("imported settings = %+v, want the ones from the file", imported.settings)
	}

	// The member verifies and the hold is forgotten, which empties the table.
	first.verifier.forget("LEGACY-1")

	// A restart must not read the file again.
	second := newHarnessWithStore(t, section, databasePath)
	if _, found := second.verifier.lookup("LEGACY-1"); found {
		t.Error("the older file was imported a second time, reviving a hold that is over")
	}
}

// TestACorruptLegacyFileDoesNotStopTheBot covers the choice to carry on rather
// than refuse to start: the file is no longer the source of truth, and taking the
// bot down over it would leave the whole group unmanaged.
func TestACorruptLegacyFileDoesNotStopTheBot(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "bot.db")
	legacy := filepath.Join(t.TempDir(), "pending.json")
	if err := os.WriteFile(legacy, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarnessWithStore(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
state_file: `+legacy+`
`, databasePath)

	// Still able to hold somebody, which is what "did not stop" means.
	h.join()
	if h.tokenFromPrompt() == "" {
		t.Error("the bot did not hold a new member after a corrupt older file")
	}
}

// deadlineEntry builds a pending entry the way production carries it, with the
// group's settings already resolved, because the handlers read the rules off
// the entry rather than from the configuration.
func (h *harness) deadlineEntry(token string) *pending {
	return &pending{
		token:        token,
		groupOpenID:  testGroupOpenID,
		memberOpenID: testMemberOpenID,
		joinedAt:     testJoinTime,
		settings:     h.verifier.cfg.settingsFor(testGroupOpenID),
	}
}

// TestOverridesApplyPerGroup covers the point of the per group settings: one
// section, two groups, a different rule each.
func TestOverridesApplyPerGroup(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_minutes: 10080
on_deadline: notify
notify_members: ["DEFAULT-ADMIN"]
groups:
  GROUP-OPENID:
    on_deadline: remove
    notify_members: ["GROUP-ADMIN"]
`)

	named := h.verifier.cfg.settingsFor(testGroupOpenID)
	if named.OnDeadline != OnDeadlineRemove {
		t.Errorf("OnDeadline = %q, want the group's own %q", named.OnDeadline, OnDeadlineRemove)
	}
	if len(named.NotifyMembers) != 1 || named.NotifyMembers[0] != "GROUP-ADMIN" {
		t.Errorf("NotifyMembers = %v, want the group's own list", named.NotifyMembers)
	}
	// Everything the group did not name is inherited.
	if named.MuteMinutes != 10080 {
		t.Errorf("MuteMinutes = %d, want the inherited 10080", named.MuteMinutes)
	}

	unnamed := h.verifier.cfg.settingsFor("ANOTHER-GROUP")
	if unnamed.OnDeadline != OnDeadlineNotify {
		t.Errorf("OnDeadline = %q, want the default for a group the file does not name",
			unnamed.OnDeadline)
	}
	if len(unnamed.NotifyMembers) != 1 || unnamed.NotifyMembers[0] != "DEFAULT-ADMIN" {
		t.Errorf("NotifyMembers = %v, want the default list", unnamed.NotifyMembers)
	}

	// And the grouping is not only resolved but acted on.
	h.verifier.handleDeadline(h.deadlineEntry("cafebabecafebabe"))
	if h.kickCount() != 1 {
		t.Errorf("onebot kicks = %d, want the removal this group asked for", h.kickCount())
	}
}

// TestABrokenGroupOverrideFailsAtStartup covers the validation of a merge, so a
// group asking for something the platform refuses is caught when the file is
// read rather than when a member has already joined.
func TestABrokenGroupOverrideFailsAtStartup(t *testing.T) {
	deps := feature.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := New(sectionNode(t, `
enabled: true
on_deadline: notify
notify_members: ["DEFAULT-ADMIN"]
groups:
  GROUP-OPENID:
    on_deadline: remove
    notify_members: []
`), deps)
	if err == nil {
		t.Error("a group that removes members with nobody to tell must be refused")
	}
	if err != nil && !strings.Contains(err.Error(), "GROUP-OPENID") {
		t.Errorf("err = %v, want it to name the group at fault", err)
	}
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

// dispatch hands one event to the client's dispatcher.
func (h *harness) dispatch(eventType, body string) {
	h.t.Helper()
	if err := h.dispatchAnyway(eventType, body); err != nil {
		h.t.Fatalf("dispatching %s: %v", eventType, err)
	}
}

// dispatchAnyway hands one event over and returns whatever the handler
// reported, for the paths that are expected to fail part way through.
func (h *harness) dispatchAnyway(eventType, body string) error {
	payload := &qqbotsdk.Payload{
		ID:   "EVENT-ID",
		Op:   qqbotsdk.OpDispatch,
		Type: eventType,
		Data: json.RawMessage(body),
	}
	return h.client.Dispatcher().DispatchSync(context.Background(),
		qqbotsdk.NewEvent(payload, "test"))
}

// join delivers a GROUP_MEMBER_ADD for the test member.
func (h *harness) join() {
	h.t.Helper()
	h.dispatch(qqbotsdk.EventGroupMemberAdd, h.joinBody())
}

// joinAnyway delivers a join whose handling is expected to fail.
func (h *harness) joinAnyway() {
	h.t.Helper()
	_ = h.dispatchAnyway(qqbotsdk.EventGroupMemberAdd, h.joinBody())
}

// joinBody is the GROUP_MEMBER_ADD body for the test member.
func (h *harness) joinBody() string {
	return `{
		"timestamp": ` + jsonNumber(testJoinTime) + `,
		"group_openid": "` + testGroupOpenID + `",
		"member_openid": "` + testMemberOpenID + `"
	}`
}

// press delivers an INTERACTION_CREATE for a button in a group.
func (h *harness) press(interactionID, token, memberOpenID, scene string) {
	h.t.Helper()
	h.dispatch(qqbotsdk.EventInteractionCreate, `{
		"id": "`+interactionID+`",
		"type": 11,
		"scene": "`+scene+`",
		"chat_type": 1,
		"group_openid": "`+testGroupOpenID+`",
		"group_member_openid": "`+memberOpenID+`",
		"data": {"type": 11, "resolved": {"button_data": "v:`+token+`"}}
	}`)
}

// jsonNumber renders an int64 for embedding in a JSON literal.
func jsonNumber(value int64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// tokenFromPrompt returns the token carried by the button of the last prompt.
func (h *harness) tokenFromPrompt() string {
	h.t.Helper()
	for _, entry := range h.callsOf("/messages") {
		keyboard, ok := entry.body["keyboard"].(map[string]any)
		if !ok {
			continue
		}
		content, ok := keyboard["content"].(map[string]any)
		if !ok {
			continue
		}
		rows, ok := content["rows"].([]any)
		if !ok || len(rows) == 0 {
			continue
		}
		row, _ := rows[0].(map[string]any)
		buttons, _ := row["buttons"].([]any)
		if len(buttons) == 0 {
			continue
		}
		button, _ := buttons[0].(map[string]any)
		action, _ := button["action"].(map[string]any)
		data, _ := action["data"].(string)
		if strings.HasPrefix(data, buttonPrefix) {
			return strings.TrimPrefix(data, buttonPrefix)
		}
	}
	h.t.Fatal("no verification button was sent")
	return ""
}

// promptText returns the markdown of the last prompt.
func (h *harness) promptText() string {
	h.t.Helper()
	for i := len(h.callsOf("/messages")) - 1; i >= 0; i-- {
		entry := h.callsOf("/messages")[i]
		markdown, ok := entry.body["markdown"].(map[string]any)
		if !ok {
			continue
		}
		if content, ok := markdown["content"].(string); ok {
			return content
		}
	}
	h.t.Fatal("no markdown message was sent")
	return ""
}

// botQQForHistory is the sender the history stub reports for the bot's own
// messages.
func (h *harness) botQQForHistory() int64 { return testBotQQ }

// TestJoinIsHeldAndAsked covers the first half of the flow: the member is muted
// and asked to verify with a button that mentions them.
func TestJoinIsHeldAndAsked(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_minutes: 41760
deadline_hours: 48
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
prompt: |
  {at}
  **欢迎新成员！**
`)

	h.join()

	mute := h.callsOf("/restrict_chat_setting")
	if len(mute) != 1 {
		t.Fatalf("mute calls = %d, want 1", len(mute))
	}
	if mute[0].method != http.MethodPost {
		t.Errorf("method = %s", mute[0].method)
	}
	members, ok := mute[0].body["members"].([]any)
	if !ok || len(members) != 1 {
		t.Fatalf("members = %v", mute[0].body["members"])
	}
	entry := members[0].(map[string]any)
	if entry["op"] != qqbotsdk.MemberMuteAdd {
		t.Errorf("op = %v, want %s", entry["op"], qqbotsdk.MemberMuteAdd)
	}
	if entry["member_openid"] != testMemberOpenID {
		t.Errorf("member_openid = %v", entry["member_openid"])
	}
	// The default hold is well inside the platform's thirty day ceiling.
	expiry, err := time.Parse(time.RFC3339, entry["mute_expire_at"].(string))
	if err != nil {
		t.Fatalf("mute_expire_at %q: %v", entry["mute_expire_at"], err)
	}
	held := time.Until(expiry)
	if held < 28*24*time.Hour || held > 29*24*time.Hour+time.Minute {
		t.Errorf("held for %s, want about twenty nine days", held)
	}

	if message := h.promptText(); !strings.Contains(message, `<qqbot-at-user id="`+testMemberOpenID+`" />`) {
		t.Errorf("prompt %q does not mention the new member", message)
	}
	h.tokenFromPrompt()
}

// TestAPressByTheMemberReleases covers the second half: the hold is lifted, the
// press is answered, and the pass message replies to the interaction.
func TestAPressByTheMemberReleases(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_minutes: 41760
deadline_hours: 48
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
pass_message: "验证通过，欢迎你！"
`)
	h.join()
	token := h.tokenFromPrompt()
	h.press("INTERACTION-1", token, testMemberOpenID, qqbotsdk.InteractionSceneGroup)

	var release *call
	for _, entry := range h.callsOf("/restrict_chat_setting") {
		members, _ := entry.body["members"].([]any)
		if len(members) == 1 {
			if value, ok := members[0].(map[string]any)["op"].(string); ok &&
				value == qqbotsdk.MemberMuteDelete {
				copied := entry
				release = &copied
			}
		}
	}
	if release == nil {
		t.Fatal("the member was never released")
	}

	answers := h.callsOf("/interactions/")
	if len(answers) != 1 {
		t.Fatalf("interaction answers = %d, want 1", len(answers))
	}
	if answers[0].method != http.MethodPut {
		t.Errorf("answer method = %s, want PUT", answers[0].method)
	}
	if answers[0].body["code"] != float64(qqbotsdk.InteractionCodeSuccess) {
		t.Errorf("code = %v, want a success", answers[0].body["code"])
	}

	// The pass message must reply to the interaction event rather than start a
	// new conversation.
	var greeted bool
	for _, entry := range h.callsOf("/messages") {
		if entry.body["event_id"] == "EVENT-ID" {
			greeted = true
		}
	}
	if !greeted {
		t.Error("the pass message did not reply to the interaction event")
	}
}

// TestAPressBySomeoneElseIsRefused covers the anti-substitution check.
func TestAPressBySomeoneElseIsRefused(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	token := h.tokenFromPrompt()
	h.press("INTERACTION-2", token, "SOMEONE-ELSE", qqbotsdk.InteractionSceneGroup)

	answer := h.lastCall()
	if answer.body["code"] != float64(qqbotsdk.InteractionCodeNoPermission) {
		t.Errorf("code = %v, want the no permission code", answer.body["code"])
	}
	for _, entry := range h.callsOf("/restrict_chat_setting") {
		members, _ := entry.body["members"].([]any)
		if len(members) > 0 && members[0].(map[string]any)["op"] == qqbotsdk.MemberMuteDelete {
			t.Error("a press by someone else must not release the member")
		}
	}
}

// TestAnUnknownTokenIsAnsweredWithAFailure covers a press after the window.
func TestAnUnknownTokenIsAnsweredWithAFailure(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.press("INTERACTION-3", "deadbeefdeadbeef", testMemberOpenID, qqbotsdk.InteractionSceneGroup)

	if answer := h.lastCall(); answer.body["code"] != float64(qqbotsdk.InteractionCodeFailed) {
		t.Errorf("code = %v, want a failure so the button stays usable", answer.body["code"])
	}
	if len(h.callsOf("/restrict_chat_setting")) != 0 {
		t.Error("an unknown token must not touch anyone's mute")
	}
}

// TestAClickOutsideAGroupIsIgnored covers a private chat press with the same
// button data, which must not be acted on.
func TestAClickOutsideAGroupIsIgnored(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	token := h.tokenFromPrompt()
	h.press("INTERACTION-4", token, testMemberOpenID, qqbotsdk.InteractionSceneC2C)

	if answers := h.callsOf("/interactions/"); len(answers) != 0 {
		t.Errorf("a private chat press was answered %d time(s)", len(answers))
	}
}

// TestDryRunNeverMutes covers the switch that lets an operator try the flow on
// a live group.
func TestDryRunNeverMutes(t *testing.T) {
	h := newHarness(t, `
enabled: true
dry_run: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	h.tokenFromPrompt()
	h.press("INTERACTION-5", h.tokenFromPrompt(), testMemberOpenID, qqbotsdk.InteractionSceneGroup)

	if calls := h.callsOf("/restrict_chat_setting"); len(calls) != 0 {
		t.Errorf("dry run made %d mute calls, want none", len(calls))
	}
	if answers := h.callsOf("/interactions/"); len(answers) != 1 {
		t.Errorf("dry run answered %d time(s), want 1", len(answers))
	}
}

// TestAFailedPromptLiftsTheHold covers the guard that keeps someone from being
// silenced with no way to verify.
func TestAFailedPromptLiftsTheHold(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.mu.Lock()
	h.failSend = true
	h.mu.Unlock()

	h.joinAnyway()

	released := false
	for _, entry := range h.callsOf("/restrict_chat_setting") {
		members, _ := entry.body["members"].([]any)
		if len(members) > 0 && members[0].(map[string]any)["op"] == qqbotsdk.MemberMuteDelete {
			released = true
		}
	}
	if !released {
		t.Error("a prompt that could not be sent must not leave the member muted")
	}
}

// TestDeadlineRemovalGoesThroughOneBot covers the whole removal path: report,
// identify the member by join time, then ask OneBot to remove them.
func TestDeadlineRemovalGoesThroughOneBot(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_minutes: 41760
deadline_hours: 48
on_deadline: remove
remove_backend: onebot
notify_members: ["ADMIN-OPENID"]
`)
	h.verifier.handleDeadline(h.deadlineEntry("cafebabecafebabe"))

	if h.kickCount() != 1 {
		t.Fatalf("onebot kicks = %d, want 1", h.kickCount())
	}
	h.mu.Lock()
	performed := h.kicks[0]
	h.mu.Unlock()
	if performed.userID != 3884506253 {
		t.Errorf("removed user %d, want the member whose join time matched", performed.userID)
	}
	if performed.groupID != testQQGroupID {
		t.Errorf("removed from group %d, want %d", performed.groupID, testQQGroupID)
	}
	// The removal is automatic, so the administrators are deliberately not
	// mentioned: there is nothing for them to do, and mentioning them on every
	// timeout would be noise.
	notice := h.promptText()
	if strings.Contains(notice, `<qqbot-at-user id="ADMIN-OPENID" />`) {
		t.Errorf("notice %q mentions the administrators during an automatic removal", notice)
	}
	// What must go out is the notice the platform resolves, which is what makes
	// the removal possible at all.
	if !strings.Contains(notice, `<qqbot-at-user id="`+testMemberOpenID+`" />`) {
		t.Errorf("notice %q does not mention the member being removed", notice)
	}
}

// TestAForgedNoticeRemovesNobody covers the guard that matters most: anybody in
// the group can read the notice, copy its marker, and mention whoever they like,
// so a message that did not come from the bot must never cause a removal.
func TestAForgedNoticeRemovesNobody(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: remove
remove_backend: onebot
notify_members: ["ADMIN-OPENID"]
`)
	h.mu.Lock()
	h.forged = true
	h.mu.Unlock()

	h.verifier.handleDeadline(h.deadlineEntry("cafebabecafebabe"))

	if h.kickCount() != 0 {
		t.Errorf("onebot kicks = %d, want none for a notice the bot did not send",
			h.kickCount())
	}
}

// TestRemovalWithoutABotQQRemovesNobody covers a file that names the group but
// not the bot, which makes the sender unverifiable.
func TestRemovalWithoutABotQQRemovesNobody(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: remove
remove_backend: onebot
notify_members: ["ADMIN-OPENID"]
`)
	h.verifier.deps.BotQQ = 0

	h.verifier.handleDeadline(h.deadlineEntry("cafebabecafebabe"))

	if h.kickCount() != 0 {
		t.Errorf("onebot kicks = %d, want none without bot.qq", h.kickCount())
	}
}

// TestRemovalWithoutAQQGroupIDRemovesNobody covers a group that was configured
// with an openid only.
func TestRemovalWithoutAQQGroupIDRemovesNobody(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: remove
remove_backend: onebot
notify_members: ["ADMIN-OPENID"]
`)
	h.verifier.deps.Groups = config.Groups{{OpenID: testGroupOpenID}}

	h.verifier.handleDeadline(h.deadlineEntry("cafebabecafebabe"))

	if h.kickCount() != 0 {
		t.Errorf("onebot kicks = %d, want none without a qq_group_id", h.kickCount())
	}
}

// TestConfigurationGuards covers the settings that would otherwise fail silently
// or slowly.
func TestConfigurationGuards(t *testing.T) {
	cases := map[string]string{
		"a mute beyond the platform ceiling": `
enabled: true
mute_minutes: 44000
on_deadline: notify
notify_members: ["A"]
`,
		"a deadline with no room inside the mute": `
enabled: true
mute_minutes: 60
deadline_hours: 48
on_deadline: notify
notify_members: ["A"]
`,
		"an unknown deadline action": `
enabled: true
on_deadline: explode
notify_members: ["A"]
`,
		"a removal with nobody to tell": `
enabled: true
on_deadline: remove
notify_members: []
`,
		"an unknown removal backend": `
enabled: true
on_deadline: remove
remove_backend: telepathy
notify_members: ["A"]
`,
		"a button label beyond the platform limit": `
enabled: true
button_label: "这个按钮的文案实在是太长了"
on_deadline: notify
notify_members: ["A"]
`,
	}
	for name, section := range cases {
		t.Run(name, func(t *testing.T) {
			deps := feature.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if _, err := New(sectionNode(t, section), deps); err == nil {
				t.Error("expected a configuration error")
			}
		})
	}
}

// TestDefaultsAreTheDocumentedOnes pins the values an operator relies on when
// the section leaves them out.
func TestDefaultsAreTheDocumentedOnes(t *testing.T) {
	deps := feature.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	instance, err := New(sectionNode(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`), deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg := instance.(*verifier).cfg
	if cfg.MuteMinutes != DefaultMuteMinutes {
		t.Errorf("MuteMinutes = %d, want %d", cfg.MuteMinutes, DefaultMuteMinutes)
	}
	if cfg.MuteMinutes != 29*24*60 {
		t.Errorf("the default hold is %d minutes, want twenty nine days",
			cfg.MuteMinutes)
	}
	if cfg.DeadlineHours != DefaultDeadlineHours {
		t.Errorf("DeadlineHours = %d, want %d", cfg.DeadlineHours, DefaultDeadlineHours)
	}
	if !cfg.MentionNewMember {
		t.Error("the default prompt mentions the member, so the switch should follow")
	}
	if !strings.Contains(cfg.Prompt, "{at}") {
		t.Error("the default prompt should place the mention")
	}
}

// stubAdmins answers whether a member administers a group.
type stubAdmins map[string]bool

func (s stubAdmins) IsAdmin(_, memberOpenID string) bool { return s[memberOpenID] }

// TestAnAdministratorMaySkip covers an administrator pressing somebody else's
// button: the member is let in and the group is told, rather than the press
// being refused like a stranger's would be.
func TestAnAdministratorMaySkip(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.verifier.SetAdminDirectory(stubAdmins{"ADMIN-OPENID": true})
	h.join()
	token := h.tokenFromPrompt()
	h.press("INTERACTION-SKIP", token, "ADMIN-OPENID", qqbotsdk.InteractionSceneGroup)

	released := false
	for _, entry := range h.callsOf("/restrict_chat_setting") {
		members, _ := entry.body["members"].([]any)
		if len(members) > 0 && members[0].(map[string]any)["op"] == qqbotsdk.MemberMuteDelete {
			released = true
		}
	}
	if !released {
		t.Error("an administrator's skip must lift the hold")
	}

	answers := h.callsOf("/interactions/")
	if len(answers) != 1 {
		t.Fatalf("interaction answers = %d, want 1", len(answers))
	}
	if answers[0].body["code"] != float64(qqbotsdk.InteractionCodeSuccess) {
		t.Errorf("code = %v, want a success", answers[0].body["code"])
	}

	notice := h.promptText()
	if !strings.Contains(notice, "管理员已跳过") {
		t.Errorf("notice = %q, want the skip explained", notice)
	}
	if !strings.Contains(notice, `<qqbot-at-user id="`+testMemberOpenID+`" />`) {
		t.Errorf("notice = %q, want the skipped member mentioned", notice)
	}
}

// TestAStrangerIsStillRefusedAfterTheSkipFeature covers that adding the
// administrator path did not open the door to everybody.
func TestAStrangerIsStillRefusedAfterTheSkipFeature(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.verifier.SetAdminDirectory(stubAdmins{"ADMIN-OPENID": true})
	h.join()
	token := h.tokenFromPrompt()
	h.press("INTERACTION-STRANGER", token, "SOMEONE-ELSE", qqbotsdk.InteractionSceneGroup)

	if answer := h.lastCall(); answer.body["code"] != float64(qqbotsdk.InteractionCodeNoPermission) {
		t.Errorf("code = %v, want the no permission code", answer.body["code"])
	}
	for _, entry := range h.callsOf("/restrict_chat_setting") {
		members, _ := entry.body["members"].([]any)
		if len(members) > 0 && members[0].(map[string]any)["op"] == qqbotsdk.MemberMuteDelete {
			t.Error("a stranger's press must never release the member")
		}
	}
}
