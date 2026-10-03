package admincmd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

const (
	testGroupOpenID = "GROUP-OPENID"
	testOtherGroup  = "OTHER-GROUP"
	testAdmin       = "ADMIN-OPENID"
	testBystander   = "BYSTANDER-OPENID"
	testTarget      = "TARGET-OPENID"
)

// TestParseDuration covers every unit the commands accept, in both scripts.
func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"30s":  30 * time.Second,
		"30秒":  30 * time.Second,
		"10m":  10 * time.Minute,
		"10分":  10 * time.Minute,
		"10分钟": 10 * time.Minute,
		"2h":   2 * time.Hour,
		"2小时":  2 * time.Hour,
		"1d":   24 * time.Hour,
		"1天":   24 * time.Hour,
		"29d":  29 * 24 * time.Hour,
		" 5M ": 5 * time.Minute, // case and spaces are tolerated
	}
	for text, want := range cases {
		got, err := parseDuration(text)
		if err != nil {
			t.Errorf("parseDuration(%q): %v", text, err)
			continue
		}
		if got != want {
			t.Errorf("parseDuration(%q) = %s, want %s", text, got, want)
		}
	}

	// A space between the number and the unit is tolerated on purpose, so it
	// is not in the list below.
	if spaceSeparated, err := parseDuration("10 分钟"); err != nil || spaceSeparated != 10*time.Minute {
		t.Errorf("parseDuration(%q) = %s, %v; want 10m", "10 分钟", spaceSeparated, err)
	}
	for _, bad := range []string{"", "10", "abc", "0s", "-5m", "1w", "1.5h"} {
		if _, err := parseDuration(bad); err == nil {
			t.Errorf("parseDuration(%q) should have failed", bad)
		}
	}
}

// TestHumanDuration mirrors what the operator typed.
func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second:    "30秒",
		10 * time.Minute:    "10分钟",
		2 * time.Hour:       "2小时",
		29 * 24 * time.Hour: "29天",
		100 * time.Minute:   "100分钟",
		90 * time.Second:    "90秒",
	}
	for duration, want := range cases {
		if got := humanDuration(duration); got != want {
			t.Errorf("humanDuration(%s) = %q, want %q", duration, got, want)
		}
	}
}

// harness wires the feature against a stub official API.
type harness struct {
	t        *testing.T
	handler  *handler
	client   *qqbotsdk.Client
	verifier *stubVerifier

	mu    sync.Mutex
	calls []map[string]any
	// paths is parallel to calls: the endpoint each request went to, which is how
	// a test tells an answer to a group apart from one to a single chat.
	paths []string
	// methods is parallel to paths too, so a test can tell a send from a
	// withdrawal.
	methods []string
	// failDeletes makes the platform refuse every withdrawal, which is what a
	// group this application may not take messages down in looks like.
	failDeletes bool
	// sentIDs numbers the message ids the stub platform hands back, which is
	// what a receipt's recall button ends up pointing at.
	sentIDs int
	// sent numbers the messages this harness delivers, because the feature
	// ignores a message id it has already handled.
	sent int
}

// stubVerifier records what the commands asked of the verification feature.
type stubVerifier struct {
	mu        sync.Mutex
	reverify  []string
	simulated []string
	resent    []string
	// pending names the members the stub reports as waiting to verify.
	pending map[string]bool
	fail    bool
}

func (s *stubVerifier) Reverify(_ context.Context, groupOpenID, memberOpenID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errStub
	}
	s.reverify = append(s.reverify, groupOpenID+"/"+memberOpenID)
	return nil
}

func (s *stubVerifier) SimulateDeadline(_ context.Context, groupOpenID, memberOpenID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errStub
	}
	s.simulated = append(s.simulated, groupOpenID+"/"+memberOpenID)
	return nil
}

func (s *stubVerifier) IsPending(groupOpenID, memberOpenID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[groupOpenID+"/"+memberOpenID]
}

func (s *stubVerifier) Resend(_ context.Context, groupOpenID, memberOpenID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errStub
	}
	s.resent = append(s.resent, groupOpenID+"/"+memberOpenID)
	return nil
}

var errStub = jsonError("stub failure")

type jsonError string

func (e jsonError) Error() string { return string(e) }

// newHarness builds the feature with one administrator in one group.
func newHarness(t *testing.T, section string) *harness {
	return newHarnessWith(t, section, false)
}

// newHarnessWithRegistering builds a harness that publishes the instruction
// panel.
//
// Every other test leaves it off, so the calls it records are only the ones it
// is actually about -- a panel published on every start would otherwise sit at
// the front of the list for all of them.
func newHarnessWithRegistering(t *testing.T, section string) *harness {
	return newHarnessWith(t, section, true)
}

func newHarnessWith(t *testing.T, section string, registering bool) *harness {
	t.Helper()
	if !registering && !strings.Contains(section, "register_commands") {
		section += "\nregister_commands: false\n"
	}
	h := &harness{t: t, verifier: &stubVerifier{}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Only requests that carry a body are recorded, so a read this feature
		// makes for its own information does not shift what an assertion finds
		// at the front of the list.
		if r.Method != http.MethodGet {
			h.mu.Lock()
			h.calls = append(h.calls, body)
			h.paths = append(h.paths, r.URL.Path)
			h.methods = append(h.methods, r.Method)
			refuse := h.failDeletes && r.Method == http.MethodDelete
			h.mu.Unlock()
			if refuse {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":40062003,"message":"无操作权限"}`))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		// A send is answered with an id, because that is what the platform
		// returns and what a message's own recall button is built from. Every
		// other call gets an empty object, which is all any of them read.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
			h.mu.Lock()
			h.sentIDs++
			id := "SENT-" + strconv.Itoa(h.sentIDs)
			h.mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"` + id + `"}`))
			return
		}
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
	h.client = client

	instance, err := New(sectionNode(t, section), feature.Deps{
		Client: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Groups: config.Groups{{OpenID: testGroupOpenID}},
	})
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	h.handler = instance.(*handler)
	h.handler.SetVerifier(h.verifier)
	if err := h.handler.Register(context.Background()); err != nil {
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

// send delivers one group message as the platform sends it when the bot was
// mentioned, which is the event a command arrives in by default.
func (h *harness) send(content, sender, groupOpenID string, mentions ...string) {
	h.t.Helper()
	h.deliver(qqbotsdk.EventGroupAtMessageCreate, content, sender, groupOpenID, mentions...)
}

// sendPlain delivers one group message as the platform sends it in full receive
// mode, whether or not the bot was mentioned.
func (h *harness) sendPlain(content, sender, groupOpenID string, mentions ...string) {
	h.t.Helper()
	h.deliver(qqbotsdk.EventGroupMessageCreate, content, sender, groupOpenID, mentions...)
}

// TestTheMentionSwitch covers the setting that decides whether an unmentioned
// command is read at all.
func TestTheMentionSwitch(t *testing.T) {
	// Off by default: an ordinary group message is not a command.
	strict := newHarness(t, baseSection)
	strict.sendPlain("/禁言 1h", testAdmin, testGroupOpenID, testTarget)
	if strict.muteCount() != 0 {
		t.Error("with require_mention on, a message that did not mention the bot is not a command")
	}

	// Turned off: the bot reads every group message.
	relaxed := newHarness(t, `
enabled: true
require_mention: false
groups:
  GROUP-OPENID:
    admins: ["ADMIN-OPENID"]
`)
	relaxed.sendPlain("/禁言 1h", testAdmin, testGroupOpenID, testTarget)
	if relaxed.muteCount() != 1 {
		t.Errorf("with require_mention off, a plain command should be read; mute calls = %d",
			relaxed.muteCount())
	}
}

// deliver hands one group message event to the dispatcher.
func (h *harness) deliver(eventType, content, sender, groupOpenID string, mentions ...string) {
	mentionJSON := "[]"
	if len(mentions) > 0 {
		parts := make([]string, 0, len(mentions))
		for _, mention := range mentions {
			parts = append(parts, `{"member_openid":"`+mention+`"}`)
		}
		mentionJSON = "[" + strings.Join(parts, ",") + "]"
	}
	h.mu.Lock()
	h.sent++
	messageID := "MSG-" + strconv.Itoa(h.sent)
	h.mu.Unlock()
	body := `{
		"id": "` + messageID + `",
		"author": {"member_openid": "` + sender + `"},
		"content": ` + jsonString(content) + `,
		"group_openid": "` + groupOpenID + `",
		"mentions": ` + mentionJSON + `
	}`
	payload := &qqbotsdk.Payload{
		ID:   "EVENT-ID",
		Op:   qqbotsdk.OpDispatch,
		Type: eventType,
		Data: json.RawMessage(body),
	}
	if err := h.client.Dispatcher().DispatchSync(context.Background(),
		qqbotsdk.NewEvent(payload, "test")); err != nil {
		h.t.Fatalf("dispatching: %v", err)
	}
}

// jsonString renders a Go string as a JSON string.
func jsonString(text string) string {
	raw, _ := json.Marshal(text)
	return string(raw)
}

// groupReplies counts the messages sent into a group rather than a single chat,
// which is how a test asserts that something stayed private.
func (h *harness) groupReplies() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for i, call := range h.calls {
		if _, ok := call["markdown"]; !ok {
			continue
		}
		if !strings.HasPrefix(h.paths[i], "/v2/users/") {
			count++
		}
	}
	return count
}

// dispatch hands one raw event body to the dispatcher, which is how a test
// delivers an event this feature does not otherwise build.
func (h *harness) dispatch(eventType, body string) {
	h.t.Helper()
	payload := &qqbotsdk.Payload{
		ID:   "EVENT-ID",
		Op:   qqbotsdk.OpDispatch,
		Type: eventType,
		Data: json.RawMessage(body),
	}
	if err := h.client.Dispatcher().DispatchSync(context.Background(),
		qqbotsdk.NewEvent(payload, "test")); err != nil {
		h.t.Fatalf("dispatching %s: %v", eventType, err)
	}
}

// press clicks one of the bot's buttons in a group.
func (h *harness) press(interactionID, buttonData, memberOpenID, groupOpenID string) {
	h.t.Helper()
	h.dispatch(qqbotsdk.EventInteractionCreate, `{
		"id": "`+interactionID+`",
		"type": 11,
		"scene": "group",
		"chat_type": 1,
		"group_openid": "`+groupOpenID+`",
		"group_member_openid": "`+memberOpenID+`",
		"data": {"type": 11, "resolved": {"button_data": `+jsonString(buttonData)+`}}
	}`)
}

// pressPrivate clicks one of the bot's buttons in a single chat.
//
// The scene is the only thing that says where the answer goes and who the presser
// is, and a single chat names them with a different field than a group does.
func (h *harness) pressPrivate(interactionID, buttonData, userOpenID string) {
	h.t.Helper()
	h.dispatch(qqbotsdk.EventInteractionCreate, `{
		"id": "`+interactionID+`",
		"type": 11,
		"scene": "c2c",
		"chat_type": 2,
		"user_openid": "`+userOpenID+`",
		"data": {"type": 11, "resolved": {"button_data": `+jsonString(buttonData)+`}}
	}`)
}

// lastAnswer returns the code the bot answered the last button press with, which
// is how the client is told what happened.
func (h *harness) lastAnswer() (float64, bool) {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.calls) - 1; i >= 0; i-- {
		if !strings.HasPrefix(h.paths[i], "/interactions/") {
			continue
		}
		code, ok := h.calls[i]["code"].(float64)
		return code, ok
	}
	return 0, false
}

// buttonOf returns the data field of the first button of the last message that
// carried a keyboard.
func (h *harness) buttonOf() string {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.calls) - 1; i >= 0; i-- {
		button := firstButtonOf(h.calls[i])
		if button == nil {
			continue
		}
		action, _ := button["action"].(map[string]any)
		data, _ := action["data"].(string)
		return data
	}
	return ""
}

// buttonPermissionOf returns the permission of that same button, which is what
// decides whether a client greys it out.
func (h *harness) buttonPermissionOf() float64 {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.calls) - 1; i >= 0; i-- {
		button := firstButtonOf(h.calls[i])
		if button == nil {
			continue
		}
		action, _ := button["action"].(map[string]any)
		permission, _ := action["permission"].(map[string]any)
		value, _ := permission["type"].(float64)
		return value
	}
	return -1
}

// firstButtonOf digs the first button out of a sent message, or nil when the
// message carried no keyboard.
func firstButtonOf(call map[string]any) map[string]any {
	keyboard, ok := call["keyboard"].(map[string]any)
	if !ok {
		return nil
	}
	content, _ := keyboard["content"].(map[string]any)
	rows, _ := content["rows"].([]any)
	if len(rows) == 0 {
		return nil
	}
	row, _ := rows[0].(map[string]any)
	buttons, _ := row["buttons"].([]any)
	if len(buttons) == 0 {
		return nil
	}
	button, _ := buttons[0].(map[string]any)
	return button
}

// lastSentID returns the id the stub platform handed back for the most recent
// message sent, which is what a message's own recall button points at.
func (h *harness) lastSentID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sentIDs == 0 {
		return ""
	}
	return "SENT-" + strconv.Itoa(h.sentIDs)
}

// mutedSeconds returns how long the most recent mute was applied for.
//
// The platform is told an absolute expiry rather than a duration, so the length
// is what is left between now and it.
func (h *harness) mutedSeconds() time.Duration {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.calls) - 1; i >= 0; i-- {
		members, ok := h.calls[i]["members"].([]any)
		if !ok || len(members) == 0 {
			continue
		}
		entry, _ := members[0].(map[string]any)
		raw, ok := entry["mute_expire_at"].(string)
		if !ok {
			continue
		}
		expiry, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			h.t.Fatalf("mute_expire_at %q: %v", raw, err)
		}
		return time.Until(expiry)
	}
	return 0
}

// privateReplies counts the messages sent into a single chat.
func (h *harness) privateReplies() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for i, call := range h.calls {
		if _, ok := call["markdown"]; !ok {
			continue
		}
		if strings.HasPrefix(h.paths[i], "/v2/users/") {
			count++
		}
	}
	return count
}

// lastMessageCarriedKeyboard reports whether the most recent message sent had
// buttons under it.
func (h *harness) lastMessageCarriedKeyboard() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.calls) - 1; i >= 0; i-- {
		if _, ok := h.calls[i]["markdown"]; !ok {
			continue
		}
		_, has := h.calls[i]["keyboard"]
		return has
	}
	return false
}

// recalledMessages returns the message ids the bot asked the platform to take
// back, in order.
func (h *harness) recalledMessages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ids []string
	for i, path := range h.paths {
		if !strings.Contains(path, "/messages/") {
			continue
		}
		if method := h.methods[i]; method != http.MethodDelete {
			continue
		}
		parts := strings.Split(path, "/messages/")
		ids = append(ids, parts[len(parts)-1])
	}
	return ids
}

// lastReply returns the markdown of the last message sent, which is how the
// feature answers a command.
func (h *harness) lastReply() string {
	h.t.Helper()
	return h.replyOn("")
}

// lastPrivateReply returns the markdown of the last message sent into a single
// chat, which is where a receipt may be read without the group reading it too.
func (h *harness) lastPrivateReply() string {
	h.t.Helper()
	return h.replyOn("/v2/users/")
}

// replyOn returns the last message sent to an endpoint starting with prefix, or
// to anything when the prefix is empty.
func (h *harness) replyOn(prefix string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.calls) - 1; i >= 0; i-- {
		if prefix != "" && !strings.HasPrefix(h.paths[i], prefix) {
			continue
		}
		markdown, ok := h.calls[i]["markdown"].(map[string]any)
		if !ok {
			continue
		}
		if content, ok := markdown["content"].(string); ok {
			return content
		}
	}
	return ""
}

// muteCount returns how many mute calls were made.
func (h *harness) muteCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, entry := range h.calls {
		if _, ok := entry["members"]; ok {
			count++
		}
	}
	return count
}

// unmuteCount counts the calls that lift a mute, as opposed to applying one.
func (h *harness) unmuteCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, entry := range h.calls {
		members, ok := entry["members"].([]any)
		if !ok || len(members) == 0 {
			continue
		}
		if members[0].(map[string]any)["op"] == qqbotsdk.MemberMuteDelete {
			count++
		}
	}
	return count
}

// TestUnmuteLiftsAMute covers the command that releases a member.
func TestUnmuteLiftsAMute(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("<@BOT-OPENID> /解禁 <@TARGET-OPENID>", testAdmin, testGroupOpenID,
		"BOT-OPENID", testTarget)

	if got := h.unmuteCount(); got != 1 {
		t.Errorf("unmutes = %d, want the mute lifted", got)
	}
}

// TestUnmuteGivesWayToTheVerification covers the priority the command was asked
// to have: a member who is being held has to verify, so the command must not
// release them, however convenient that would be for the operator.
func TestUnmuteGivesWayToTheVerification(t *testing.T) {
	h := newHarness(t, baseSection)
	h.verifier.pending = map[string]bool{testGroupOpenID + "/" + testTarget: true}
	h.send("<@BOT-OPENID> /解禁 <@TARGET-OPENID>", testAdmin, testGroupOpenID,
		"BOT-OPENID", testTarget)

	if got := h.unmuteCount(); got != 0 {
		t.Errorf("unmutes = %d, want none: the holder is verifying", got)
	}
	if reply := h.lastReply(); !strings.Contains(reply, "正在验证") {
		t.Errorf("reply = %q, want it to say the member is verifying", reply)
	}
}

// TestResendVerification covers the command that posts the prompt again.
func TestResendVerification(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("<@BOT-OPENID> /重新发送验证 <@TARGET-OPENID>", testAdmin, testGroupOpenID,
		"BOT-OPENID", testTarget)

	if len(h.verifier.resent) != 1 {
		t.Errorf("resent = %v, want one resend", h.verifier.resent)
	}
	if reply := h.lastReply(); !strings.Contains(reply, "已重新发送") {
		t.Errorf("reply = %q, want a confirmation", reply)
	}
}

// TestWhoisNamesOthersOnlyForAdministrators covers the rule that naming another
// member is an administrator's ability.
//
// A member can reach the command at all only while the group has no
// administrators yet, which is the setup case: they get their own id, because
// that is what they need in order to be put on the list, and nothing about
// anybody else.
func TestWhoisNamesOthersOnlyForAdministrators(t *testing.T) {
	t.Run("a member sees only themselves", func(t *testing.T) {
		h := newHarness(t, `
enabled: true
groups: {}
`)
		h.send("<@BOT-OPENID> /whois <@SOMEONE-ELSE>", "ORDINARY-MEMBER", testGroupOpenID,
			"BOT-OPENID", "SOMEONE-ELSE")

		reply := h.lastReply()
		if strings.Contains(reply, "SOMEONE-ELSE") {
			t.Errorf("reply = %q, want no other member's openid", reply)
		}
		if !strings.Contains(reply, "ORDINARY-MEMBER") {
			t.Errorf("reply = %q, want the sender's own openid", reply)
		}
	})

	t.Run("an administrator sees the mentioned member", func(t *testing.T) {
		h := newHarness(t, baseSection)
		h.send("<@BOT-OPENID> /whois <@TARGET-OPENID>", testAdmin, testGroupOpenID,
			"BOT-OPENID", testTarget)

		if reply := h.lastReply(); !strings.Contains(reply, testTarget) {
			t.Errorf("reply = %q, want the mentioned member's openid", reply)
		}
	})
}

// TestAMentionOfSomebodyElseIsNotAMentionOfTheBot covers the production bug
// this check was written for: "@AIRY /菜单" was answered, because the leading
// mention was assumed to be the bot's without being compared against it.
func TestAMentionOfSomebodyElseIsNotAMentionOfTheBot(t *testing.T) {
	h := newHarness(t, baseSection)
	h.handler.botOpenID = "BOT-OPENID"
	// An ordinary message, because that is how a group that receives everything
	// delivers a message that mentions somebody other than the bot.
	h.sendPlain("<@SOMEONE-ELSE> /菜单", testAdmin, testGroupOpenID, "SOMEONE-ELSE")

	if reply := h.lastReply(); reply != "" {
		t.Errorf("reply = %q, want nothing: the message mentions somebody else", reply)
	}
}

// TestAMentionOfTheBotIsAccepted covers the other half, so the check cannot be
// satisfied by refusing everything.
func TestAMentionOfTheBotIsAccepted(t *testing.T) {
	h := newHarness(t, baseSection)
	h.handler.botOpenID = "BOT-OPENID"
	h.sendPlain("<@BOT-OPENID> /菜单", testAdmin, testGroupOpenID, "BOT-OPENID")

	if reply := h.lastReply(); !strings.Contains(reply, "可用命令") {
		t.Errorf("reply = %q, want the command list", reply)
	}
}

// TestWhoisAnswersInAGroupThatIsNotConfiguredYet covers the case that makes the
// command useful at all: a group whose openid nobody knows yet is exactly the
// group whose configuration has to be written, and its messages used to be
// dropped before the command was even looked at.
func TestWhoisAnswersInAGroupThatIsNotConfiguredYet(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("<@BOT-OPENID> /whois", testAdmin, "UNCONFIGURED-GROUP")

	reply := h.lastReply()
	if !strings.Contains(reply, "UNCONFIGURED-GROUP") {
		t.Errorf("reply = %q, want the group's own openid", reply)
	}
	if !strings.Contains(reply, "ADMIN-OPENID") {
		t.Errorf("reply = %q, want the sender's own openid", reply)
	}
}

// TestWhoisInAGroupTheBotDoesNotManageNamesNobodyElse covers the half of the
// administrator check that is about the group rather than about the member.
//
// An administrator list is written per group, and a group the bot is not in can
// still have one. /whois answers there -- that is how the group gets configured
// at all -- and what it must report is the sender, whoever the sender administers
// somewhere the bot does manage.
func TestWhoisInAGroupTheBotDoesNotManageNamesNobodyElse(t *testing.T) {
	h := newHarness(t, baseSection+`
  UNCONFIGURED-GROUP:
    admins: ["ADMIN-OPENID"]
`)
	h.send("<@BOT-OPENID> /whois <@TARGET-OPENID>", testAdmin, "UNCONFIGURED-GROUP",
		"BOT-OPENID", testTarget)

	reply := h.lastReply()
	if !strings.Contains(reply, "UNCONFIGURED-GROUP") {
		t.Errorf("reply = %q, want the group's own openid", reply)
	}
	if strings.Contains(reply, "被 @ 的 member_openid") {
		t.Errorf("reply = %q, want nothing about another member", reply)
	}
}

// TestOtherCommandsAreStillDroppedInAnUnconfiguredGroup covers that opening
// /whois did not open everything: without a configured group there is no
// administrator list to check a sender against, so nothing else may run.
func TestOtherCommandsAreStillDroppedInAnUnconfiguredGroup(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("<@BOT-OPENID> /禁言 1h <@TARGET-OPENID>", testAdmin, "UNCONFIGURED-GROUP",
		"BOT-OPENID", testTarget)

	if reply := h.lastReply(); reply != "" {
		t.Errorf("reply = %q, want nothing in a group that is not configured", reply)
	}
	if mutes := h.muteCount(); mutes != 0 {
		t.Errorf("mutes applied = %d, want none", mutes)
	}
}

// TestWhoisDoesNotListTheBotItself covers the noise: a command reaches the bot by
// mentioning it, so the bot is in the mention list nearly every time, and listing
// it reads as though somebody had asked about the bot.
func TestWhoisDoesNotListTheBotItself(t *testing.T) {
	h := newHarness(t, baseSection)
	h.handler.botOpenID = "BOT-OPENID"
	h.send("<@BOT-OPENID> /whois <@TARGET-OPENID>", testAdmin, testGroupOpenID,
		"BOT-OPENID", testTarget)

	reply := h.lastReply()
	if !strings.Contains(reply, testTarget) {
		t.Errorf("reply = %q, want the member that was asked about", reply)
	}
	if strings.Contains(reply, "被 @ 的 member_openid：`BOT-OPENID`") {
		t.Errorf("reply = %q, want the bot itself left out", reply)
	}
}

const baseSection = `
enabled: true
debug: true
groups:
  GROUP-OPENID:
    admins: ["ADMIN-OPENID"]
`

// TestAMemberCannotCommand covers the list: being an administrator in QQ grants
// nothing, only the configured list does.
func TestAMemberCannotCommand(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("/禁言 30m", testBystander, testGroupOpenID, testTarget)

	if h.muteCount() != 0 {
		t.Error("a member who is not on the list must not mute anybody")
	}
	if reply := h.lastReply(); !strings.Contains(reply, "权限") {
		t.Errorf("reply = %q, want a permission refusal", reply)
	}
}

// TestAnAdministratorMayMute covers the happy path.
func TestAnAdministratorMayMute(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("/禁言 29d", testAdmin, testGroupOpenID, testTarget)

	if h.muteCount() != 1 {
		t.Fatalf("mute calls = %d, want 1", h.muteCount())
	}
	h.mu.Lock()
	members, _ := h.calls[0]["members"].([]any)
	h.mu.Unlock()
	if len(members) != 1 {
		t.Fatalf("members = %v", members)
	}
	entry := members[0].(map[string]any)
	if entry["member_openid"] != testTarget {
		t.Errorf("muted %v, want the mentioned target", entry["member_openid"])
	}
	if entry["op"] != qqbotsdk.MemberMuteAdd {
		t.Errorf("op = %v", entry["op"])
	}
	// The platform's ceiling is thirty days, so a 29 day command must survive.
	expiry, err := time.Parse(time.RFC3339, entry["mute_expire_at"].(string))
	if err != nil {
		t.Fatalf("mute_expire_at %q: %v", entry["mute_expire_at"], err)
	}
	if until := time.Until(expiry); until < 29*24*time.Hour-time.Minute {
		t.Errorf("muted until %s, want about 29 days", until)
	}
	if !strings.Contains(h.lastReply(), "已禁言") {
		t.Errorf("reply = %q", h.lastReply())
	}
}

// TestTheEnglishAliasWorks covers /mute.
func TestTheEnglishAliasWorks(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("/mute 2h", testAdmin, testGroupOpenID, testTarget)
	if h.muteCount() != 1 {
		t.Errorf("mute calls = %d, want 1 for the alias", h.muteCount())
	}
}

// TestAMuteBeyondTheCapIsRefused covers the ceiling, which must be reported
// rather than silently shortened.
func TestAMuteBeyondTheCapIsRefused(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("/禁言 30d", testAdmin, testGroupOpenID, testTarget)

	if h.muteCount() != 0 {
		t.Error("a mute beyond the cap must not be applied")
	}
	if reply := h.lastReply(); !strings.Contains(reply, "上限") {
		t.Errorf("reply = %q, want the cap explained", reply)
	}
}

// TestAGroupMayLowerItsOwnCap covers the per group override.
func TestAGroupMayLowerItsOwnCap(t *testing.T) {
	h := newHarness(t, `
enabled: true
groups:
  GROUP-OPENID:
    admins: ["ADMIN-OPENID"]
    max_mute: 1h
`)
	h.send("/禁言 2h", testAdmin, testGroupOpenID, testTarget)
	if h.muteCount() != 0 {
		t.Error("the group's own cap must be enforced")
	}
	if reply := h.lastReply(); !strings.Contains(reply, "1小时") {
		t.Errorf("reply = %q, want the group's cap named", reply)
	}
}

// TestNoTargetIsRefusedWithThePlatformReason covers the answer an operator gets
// when they try to name somebody without mentioning them.
func TestNoTargetIsRefusedWithThePlatformReason(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("/禁言 30m", testAdmin, testGroupOpenID)

	if h.muteCount() != 0 {
		t.Error("a command with no target must not mute anybody")
	}
	reply := h.lastReply()
	if !strings.Contains(reply, "@") {
		t.Errorf("reply = %q, want it to ask for a mention", reply)
	}
	if !strings.Contains(reply, "QQ") {
		t.Errorf("reply = %q, want the platform limitation explained", reply)
	}
}

// TestAnotherGroupIsNotCovered covers the per group lists.
func TestAnotherGroupIsNotCovered(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("/禁言 30m", testAdmin, testOtherGroup, testTarget)

	if h.muteCount() != 0 {
		t.Error("an administrator of one group must not command the bot in another")
	}
}

// TestDebugIsRefusedWhileOff covers the global debug switch.
func TestDebugIsRefusedWhileOff(t *testing.T) {
	h := newHarness(t, `
enabled: true
debug: false
groups:
  GROUP-OPENID:
    admins: ["ADMIN-OPENID"]
`)
	h.send("/debug 超时测试", testAdmin, testGroupOpenID, testTarget)

	if len(h.verifier.simulated) != 0 {
		t.Error("a debug command must do nothing while debug is off")
	}
	if reply := h.lastReply(); !strings.Contains(reply, "未开启") {
		t.Errorf("reply = %q, want the switch explained", reply)
	}
}

// TestDebugTimeoutDrivesVerification covers the subcommand that runs the
// deadline path on demand.
func TestDebugTimeoutDrivesVerification(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("/debug 超时测试", testAdmin, testGroupOpenID, testTarget)

	if len(h.verifier.simulated) != 1 {
		t.Fatalf("simulated = %v, want one run", h.verifier.simulated)
	}
	if h.verifier.simulated[0] != testGroupOpenID+"/"+testTarget {
		t.Errorf("simulated = %v", h.verifier.simulated)
	}
	// Nothing is answered on success: the run posts its own notices, so a
	// "done" line would only be noise on top of them.
	if reply := h.lastReply(); reply != "" {
		t.Errorf("reply = %q, want no answer for a successful timeout simulation", reply)
	}
}

// TestTheMenuAnswersEveryone covers the command that lists what the bot can do.
//
// It must demand no target and refuse nobody: the dispatch used to resolve a
// target before reaching the command, so /菜单 answered "请 @ 目标成员" instead of
// the list, which is exactly what an operator hit in production.
func TestTheMenuAnswersEveryone(t *testing.T) {
	for name, sender := range map[string]string{
		"an administrator":   testAdmin,
		"an ordinary member": "SOMEONE-ELSE",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, baseSection)
			h.send("/菜单", sender, testGroupOpenID)

			list := h.lastReply()
			if !strings.Contains(list, "可用命令") {
				t.Errorf("reply = %q, want the command list", list)
			}
			if strings.Contains(list, "请 @ 目标成员") {
				t.Errorf("reply = %q, want the list rather than a missing target", list)
			}
		})
	}
}

// TestAnUnknownCommandStillListsTheCommands covers that a typo is answered with
// the list rather than with a complaint about a target.
func TestAnUnknownCommandStillListsTheCommands(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("/不存在的命令", testAdmin, testGroupOpenID)

	if list := h.lastReply(); !strings.Contains(list, "可用命令") {
		t.Errorf("reply = %q, want the command list", list)
	}
}

// TestReverifyDrivesVerification covers the re-verification command.
func TestReverifyDrivesVerification(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("/重新验证", testAdmin, testGroupOpenID, testTarget)

	if len(h.verifier.reverify) != 1 {
		t.Fatalf("reverify = %v, want one run", h.verifier.reverify)
	}
	if reply := h.lastReply(); !strings.Contains(reply, "已重新") {
		t.Errorf("reply = %q", reply)
	}
}

// TestTheMentionMarkupIsStripped covers a command sent while the bot receives
// every group message, where the platform leaves the mention in the text.
func TestTheMentionMarkupIsStripped(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("<@BOT-OPENID> /禁言 1h", testAdmin, testGroupOpenID, testTarget)

	if h.muteCount() != 1 {
		t.Errorf("mute calls = %d, want 1 after the mention is stripped", h.muteCount())
	}
}

// TestConfigurationGuards covers sections that would otherwise fail silently.
func TestConfigurationGuards(t *testing.T) {
	cases := map[string]string{
		"a group with no administrators": `
enabled: true
groups:
  GROUP-OPENID:
    admins: []
`,
		"an unreadable cap": `
enabled: true
groups:
  GROUP-OPENID:
    admins: ["ADMIN-OPENID"]
    max_mute: "forever"
`,
		"a group with no openid": `
enabled: true
groups:
  "":
    admins: ["ADMIN-OPENID"]
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

// TestWhoisVisibility covers the rule that /whois is for administrators, with
// the deliberate exception that keeps an operator from locking themselves out
// of the group they are setting up.
func TestWhoisVisibility(t *testing.T) {
	// A group with no administrators is being set up, so anybody may ask.
	settingUp := newHarness(t, `
enabled: true
groups: {}
`)
	settingUp.send("/whois", testBystander, testGroupOpenID)
	if reply := settingUp.lastReply(); !strings.Contains(reply, "群 openid") {
		t.Errorf("a group being set up should answer /whois; reply = %q", reply)
	}

	// Once a group has administrators, only they may ask.
	configured := newHarness(t, baseSection)
	configured.send("/whois", testBystander, testGroupOpenID)
	if reply := configured.lastReply(); !strings.Contains(reply, "权限") {
		t.Errorf("a stranger should be refused; reply = %q", reply)
	}

	configured.send("/whois", testAdmin, testGroupOpenID)
	if reply := configured.lastReply(); !strings.Contains(reply, "群 openid") {
		t.Errorf("an administrator should be answered; reply = %q", reply)
	}

	// The switch can hand the command back to everybody.
	open := newHarness(t, `
enabled: true
whois_admin_only: false
groups:
  GROUP-OPENID:
    admins: ["ADMIN-OPENID"]
`)
	open.send("/whois", testBystander, testGroupOpenID)
	if reply := open.lastReply(); !strings.Contains(reply, "群 openid") {
		t.Errorf("with the switch off anybody may ask; reply = %q", reply)
	}
}

// TestTheTargetIsNotTheBot covers the exact shape a full receive event
// delivered in production:
//
//	content="<@BOT> /重新验证 <@TARGET>"   mentions=2
//
// The mentions list contains the bot as well as the target, so taking the first
// entry acts on the bot, which the platform refuses with 40103004
// (目标成员为机器人/群主/管理员，不允许被禁言). The bot must be skipped.
func TestTheTargetIsNotTheBot(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("<@BOT-OPENID> /禁言 1h <@TARGET-OPENID>", testAdmin, testGroupOpenID,
		"BOT-OPENID", testTarget)

	if h.muteCount() != 1 {
		t.Fatalf("mute calls = %d, want 1", h.muteCount())
	}
	h.mu.Lock()
	members, _ := h.calls[0]["members"].([]any)
	h.mu.Unlock()
	if len(members) != 1 {
		t.Fatalf("members = %v", members)
	}
	if got := members[0].(map[string]any)["member_openid"]; got != testTarget {
		t.Errorf("muted %v, want the target and not the bot", got)
	}
}

// TestTheMentionsListFallsBack covers a target that only the list names, with
// the bot still present and still skipped.
func TestTheMentionsListFallsBack(t *testing.T) {
	h := newHarness(t, baseSection)
	// The bot is mentioned in the text, which is why the list contains it; the
	// target appears only in the list.
	h.send("<@BOT-OPENID> /禁言 1h", testAdmin, testGroupOpenID, "BOT-OPENID", testTarget)

	h.mu.Lock()
	members, _ := h.calls[0]["members"].([]any)
	h.mu.Unlock()
	if len(members) != 1 {
		t.Fatalf("members = %v", members)
	}
	if got := members[0].(map[string]any)["member_openid"]; got != testTarget {
		t.Errorf("muted %v, want the second entry rather than the bot", got)
	}
}

// TestTheMuteAcceptsEitherOrder covers the orders an operator may naturally
// write. The target is read from the mention rather than from an argument, so
// its position must not matter, and an argument that is a mention must never be
// read as a duration.
func TestTheMuteAcceptsEitherOrder(t *testing.T) {
	cases := map[string]string{
		"duration first": "/禁言 5d <@TARGET-OPENID>",
		"target first":   "/禁言 <@TARGET-OPENID> 5d",
		"only duration":  "/禁言 5d",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, baseSection)
			h.send("<@BOT-OPENID> "+content, testAdmin, testGroupOpenID, "BOT-OPENID", testTarget)

			if h.muteCount() != 1 {
				t.Fatalf("mute calls = %d, want 1 for %q", h.muteCount(), content)
			}
			h.mu.Lock()
			members, _ := h.calls[0]["members"].([]any)
			h.mu.Unlock()
			entry := members[0].(map[string]any)
			if entry["member_openid"] != testTarget {
				t.Errorf("muted %v, want the mentioned target", entry["member_openid"])
			}
			expiry, err := time.Parse(time.RFC3339, entry["mute_expire_at"].(string))
			if err != nil {
				t.Fatalf("mute_expire_at %q: %v", entry["mute_expire_at"], err)
			}
			until := time.Until(expiry)
			if until < 5*24*time.Hour-time.Minute || until > 5*24*time.Hour+time.Minute {
				t.Errorf("muted for %s, want five days", until)
			}
		})
	}
}
