package joinverify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/fouc3/qq-group-management-bot/internal/humanverify"
)

// The online mode is the other way a member can prove they are a person: instead
// of pressing a button in the group, they open a link and pass the service's
// check in a browser. What is covered here is what that changes for the member --
// a link button only they may press, and a hold that ends when the service says
// so rather than when a press arrives -- and what it must not change: the hold
// itself, the deadline, and the administrator's way past it.

// fakeOnline stands in for the verification service.
type fakeOnline struct {
	mu sync.Mutex
	// requests are the links that were asked for.
	requests []humanverify.Request
	// url is what the service hands back, unless issueErr says otherwise.
	url string
	// issueErr makes the service refuse, which is what a service that is down,
	// misconfigured or out of links looks like from here.
	issueErr error
	// connected answers the question the feature asks before it spends a link.
	connected bool
	// handler is what was subscribed, and canceled records that it was taken
	// back.
	handler  func(humanverify.Event)
	canceled bool
}

func newFakeOnline() *fakeOnline {
	return &fakeOnline{connected: true, url: "https://verify.test/?t=one"}
}

func (f *fakeOnline) Issue(_ context.Context, request humanverify.Request) (humanverify.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.issueErr != nil {
		return humanverify.Link{}, f.issueErr
	}
	return humanverify.Link{ID: "LINK-1", URL: f.url, TTLSeconds: 900}, nil
}

func (f *fakeOnline) OnVerified(handler func(humanverify.Event)) (cancel func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = handler
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.canceled = true
	}
}

func (f *fakeOnline) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

// asked returns the links that were asked for.
func (f *fakeOnline) asked() []humanverify.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]humanverify.Request(nil), f.requests...)
}

// deliver hands one result to whoever is subscribed, the way the service's
// connection does.
func (f *fakeOnline) deliver(event humanverify.Event) {
	f.mu.Lock()
	handler := f.handler
	f.mu.Unlock()
	if handler == nil {
		panic("the feature never subscribed to the service")
	}
	handler(event)
}

// resultFor is one result for the hold a link was asked for, as the service
// reports it.
func resultFor(token string) humanverify.Event {
	return humanverify.Event{
		Type:   humanverify.EventVerified,
		Action: onlineAction,
		Target: token,
		ID:     "LINK-1",
		Cdata:  humanverify.BusinessMarker,
		Client: humanverify.ClientInfo{IP: "203.0.113.7", Country: "CN"},
	}
}

// cardButtons returns the buttons of the last keyboarded message the feature
// sent, as the platform received them.
func (h *harness) cardButtons() []map[string]any {
	h.t.Helper()
	messages := h.callsOf("/messages")
	if len(messages) == 0 {
		h.t.Fatal("no message was sent")
	}
	keyboard, ok := messages[len(messages)-1].body["keyboard"].(map[string]any)
	if !ok {
		h.t.Fatal("the last message carried no keyboard")
	}
	content, _ := keyboard["content"].(map[string]any)
	rows, _ := content["rows"].([]any)
	var buttons []map[string]any
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		entries, _ := entry["buttons"].([]any)
		for _, button := range entries {
			parsed, _ := button.(map[string]any)
			buttons = append(buttons, parsed)
		}
	}
	return buttons
}

// actionOf and permissionOf read the two halves of a button a test has to judge.
func actionOf(t *testing.T, button map[string]any) map[string]any {
	t.Helper()
	action, ok := button["action"].(map[string]any)
	if !ok {
		t.Fatalf("a button carried no action: %v", button)
	}
	return action
}

// typeOf reads a button action's type, where an absent one is type zero.
//
// The SDK writes the type only when it is not the zero value, and the platform's
// zero is the link button: the member's button therefore arrives with no type at
// all, which is the same statement.
func typeOf(t *testing.T, action map[string]any) int {
	t.Helper()
	raw, present := action["type"]
	if !present {
		return 0
	}
	value, _ := raw.(float64)
	return int(value)
}

// permissionTypeOf reads a button permission's type, with the same caveat: an
// absent one is type zero, which is the specified-users permission.
func permissionTypeOf(t *testing.T, permission map[string]any) int {
	t.Helper()
	raw, present := permission["type"]
	if !present {
		return 0
	}
	value, _ := raw.(float64)
	return int(value)
}

func permissionOf(t *testing.T, action map[string]any) map[string]any {
	t.Helper()
	permission, ok := action["permission"].(map[string]any)
	if !ok {
		t.Fatalf("a button carried no permission: %v", action)
	}
	return permission
}

// onlineSection is a section with everything the flow needs and online
// verification turned on for the one group the harness manages.
const onlineSection = `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
online_verify: true
`

// TestAGroupThatVerifiesOnlineGetsALinkOnlyItsMemberCanOpen covers the card
// itself. The member's way to verify is a link the platform shows to one person,
// and the administrators keep a button of their own -- a link reports nothing
// back, so without it nobody could let a member through by hand.
func TestAGroupThatVerifiesOnlineGetsALinkOnlyItsMemberCanOpen(t *testing.T) {
	online := newFakeOnline()
	h := newHarnessWithOnline(t, onlineSection, "", online)
	h.join()

	// The member is held exactly as before: what changed is how they get out.
	if !h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Fatal("a member asked to verify online was not held")
	}
	if calls := h.muteCalls(); calls != 1 {
		t.Errorf("mute calls = %d, want the one hold", calls)
	}

	buttons := h.cardButtons()
	if len(buttons) != 2 {
		t.Fatalf("buttons = %d, want a link for the member and one for the "+
			"administrators", len(buttons))
	}

	member := actionOf(t, buttons[0])
	if got := typeOf(t, member); got != 0 {
		t.Errorf("the member's button is type %d, want the link type", got)
	}
	if got := member["data"]; got != online.url {
		t.Errorf("the member's button opens %v, want the issued link", got)
	}
	permission := permissionOf(t, member)
	if got := permissionTypeOf(t, permission); got != 0 {
		t.Errorf("the member's button permission is type %d, want specified users", got)
	}
	allowed, _ := permission["specify_user_ids"].([]any)
	if len(allowed) != 1 || allowed[0] != testMemberOpenID {
		t.Errorf("the member's button is shown to %v, want only %s",
			allowed, testMemberOpenID)
	}

	skip := actionOf(t, buttons[1])
	if got := typeOf(t, skip); got != 1 {
		t.Errorf("the administrators' button is type %d, want the callback type", got)
	}
	if got := permissionTypeOf(t, permissionOf(t, skip)); got != 1 {
		t.Errorf("the administrators' button permission is type %d, want administrators", got)
	}

	// And the service was asked for the link in the way it documents: an action
	// that says what the link is for, and a target that is this bot's name for
	// the hold, which is what comes back with the result.
	asked := online.asked()
	if len(asked) != 1 {
		t.Fatalf("the service was asked %d times, want once", len(asked))
	}
	if asked[0].Action != onlineAction {
		t.Errorf("action = %q, want %q", asked[0].Action, onlineAction)
	}
	if asked[0].Target == "" {
		t.Error("the link carried no target, so a result could not find the member")
	}
	// The token the result will carry is the one the administrators' button shows.
	data, _ := skip["data"].(string)
	if !strings.HasSuffix(data, asked[0].Target) {
		t.Errorf("the button presses %q and the service was told %q; they have to "+
			"be the same hold", data, asked[0].Target)
	}
}

// TestAnOnlineResultLetsTheHeldMemberOut covers the ending: the service reports
// that the member passed, and the hold is lifted -- the mute comes off, the
// record goes, and the group is told.
func TestAnOnlineResultLetsTheHeldMemberOut(t *testing.T) {
	online := newFakeOnline()
	h := newHarnessWithOnline(t, onlineSection, "", online)
	h.join()

	token := online.asked()[0].Target
	online.deliver(resultFor(token))

	if h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("the member is still held after passing the online verification")
	}
	if calls := h.muteCalls(); calls != 2 {
		t.Errorf("mute calls = %d, want the hold and the release", calls)
	}
	// The prompt, and then the pass message: there is no interaction to answer
	// and no message to reply to, so the group is told in an ordinary message.
	if text := h.promptText(); text != DefaultPassMessage {
		t.Errorf("the last message was %q, want the pass message %q",
			text, DefaultPassMessage)
	}
	if calls := len(h.callsOf("/messages")); calls != 2 {
		t.Errorf("messages sent = %d, want the prompt and the pass message", calls)
	}
}

// TestAnOnlineResultIsActedOnOnce covers the duplicate: the same result can
// arrive twice around a lost connection, and the second one has nothing left to
// do -- it must not release somebody else, or say the group's welcome twice.
func TestAnOnlineResultIsActedOnOnce(t *testing.T) {
	online := newFakeOnline()
	h := newHarnessWithOnline(t, onlineSection, "", online)
	h.join()

	token := online.asked()[0].Target
	online.deliver(resultFor(token))
	online.deliver(resultFor(token))

	if calls := h.muteCalls(); calls != 2 {
		t.Errorf("mute calls = %d, want one hold and one release", calls)
	}
	if calls := len(h.callsOf("/messages")); calls != 2 {
		t.Errorf("messages sent = %d, want the prompt and one pass message", calls)
	}
}

// TestAnOnlineResultForNoHoldChangesNothing covers a result this bot cannot
// place: a target that names no hold, which is what a stale link and a
// misconfigured service both look like from here. Nothing is lifted, nobody is
// greeted, and the member being held is untouched.
func TestAnOnlineResultForNoHoldChangesNothing(t *testing.T) {
	online := newFakeOnline()
	h := newHarnessWithOnline(t, onlineSection, "", online)
	h.join()

	online.deliver(resultFor("no-such-token"))

	if !h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("an unplaceable result released the member who is being held")
	}
	if calls := h.muteCalls(); calls != 1 {
		t.Errorf("mute calls = %d, want the one hold", calls)
	}
	if calls := len(h.callsOf("/messages")); calls != 1 {
		t.Errorf("messages sent = %d, want the prompt only", calls)
	}
}

// TestOnlineVerificationFallsBackToTheButtonWhenTheServiceFails covers the one
// outcome that must not happen: a member held with nothing they can do. A
// service that is down, that refuses, or whose connection is not up leaves them
// the button they can press, and the group still gets a card.
func TestOnlineVerificationFallsBackToTheButtonWhenTheServiceFails(t *testing.T) {
	for _, test := range []struct {
		what   string
		broken func(*fakeOnline)
	}{
		{"the service refuses", func(f *fakeOnline) { f.issueErr = errors.New("401") }},
		{"the service is not connected", func(f *fakeOnline) { f.connected = false }},
	} {
		t.Run(test.what, func(t *testing.T) {
			online := newFakeOnline()
			test.broken(online)
			h := newHarnessWithOnline(t, onlineSection, "", online)
			h.join()

			buttons := h.cardButtons()
			if len(buttons) != 1 {
				t.Fatalf("buttons = %d, want the one the member can press", len(buttons))
			}
			action := actionOf(t, buttons[0])
			if got := typeOf(t, action); got != 1 {
				t.Errorf("the button is type %d, want the callback type", got)
			}
			if got := permissionTypeOf(t, permissionOf(t, action)); got != 2 {
				t.Errorf("the button permission is type %d, want everyone", got)
			}
			if data, _ := action["data"].(string); !strings.HasPrefix(data, buttonPrefix) {
				t.Errorf("button data = %q, want a verification token", data)
			}
			// And it works: the member can still verify by pressing it.
			h.press("PRESS-1", strings.TrimPrefix(action["data"].(string), buttonPrefix),
				testMemberOpenID, "group")
			if h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
				t.Error("the fallback button did not let the member verify")
			}
		})
	}
}

// TestOnlineVerificationNeedsAService covers the two sections disagreeing: a
// group asks for a check nobody can perform. Refusing at startup is the point --
// the alternative is a group that holds every new member and lets none of them
// out until their deadline.
func TestOnlineVerificationNeedsAService(t *testing.T) {
	_, err := buildFeature(t, onlineSection)
	if err == nil {
		t.Fatal("online verification was accepted with no service configured")
	}
	if !strings.Contains(err.Error(), "human_verification") {
		t.Errorf("err = %v, want it to name the section that is missing", err)
	}
}

// TestOnlineVerificationSpeaksForTheGroupThatWroteIt covers the boundary of the
// setting: it is each group's own, so one group verifying online says nothing
// about the next one -- and the default is the button, because that is what
// every deployment did before the service existed.
func TestOnlineVerificationSpeaksForTheGroupThatWroteIt(t *testing.T) {
	h := newHarnessWithOnline(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
groups:
  GROUP-OPENID:
    online_verify: true
`, "", newFakeOnline())
	if !h.verifier.cfg.settingsFor(testGroupOpenID).OnlineVerify {
		t.Error("the group that asked to verify online is not doing so")
	}
	if h.verifier.cfg.settingsFor("ANOTHER-GROUP").OnlineVerify {
		t.Error("a group that named nothing was switched to online verification")
	}
}

// TestTheSubscriptionIsGivenBackOnClose covers what makes a rebuild possible:
// the instance that stops must stop hearing results, or the instance replacing
// it and the one being replaced would both act on the same one.
func TestTheSubscriptionIsGivenBackOnClose(t *testing.T) {
	online := newFakeOnline()
	h := newHarnessWithOnline(t, onlineSection, "", online)

	if err := h.verifier.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	online.mu.Lock()
	canceled := online.canceled
	online.mu.Unlock()
	if !canceled {
		t.Error("the feature stopped but is still subscribed to the service")
	}
}

// TestTheButtonVersionIsUnchanged covers the deployment with no service at all,
// which is the ordinary one: the group gets the single button it has always had,
// and nothing about it moved.
func TestTheButtonVersionIsUnchanged(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()

	buttons := h.cardButtons()
	if len(buttons) != 1 {
		t.Fatalf("buttons = %d, want the single button this mode has always sent",
			len(buttons))
	}
	if got := typeOf(t, actionOf(t, buttons[0])); got != 1 {
		t.Errorf("the button is type %d, want the callback type", got)
	}
}
