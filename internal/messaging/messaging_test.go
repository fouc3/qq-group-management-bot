package messaging

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// recorder is a stand-in platform: it remembers what was sent and how many
// attempts it took, and its answer is decided by the test.
type recorder struct {
	client   *qqbotsdk.Client
	bodies   []map[string]any
	path     string
	attempts int
}

// platform builds a client pointed at a server the test controls.
//
// respond is given the attempt number and answers with a status and a body, so a
// test can fail twice and then succeed.
func platform(t *testing.T, respond func(attempt int) (int, string)) *recorder {
	t.Helper()
	recording := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recording.attempts++
		recording.path = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		recording.bodies = append(recording.bodies, body)

		status, answer := respond(recording.attempts)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(server.Close)

	client, err := qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AccessToken: "test-token",
		BaseURL:     server.URL,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	recording.client = client
	return recording
}

// ok is the platform answering a send.
func ok(int) (int, string) { return http.StatusOK, `{}` }

// last is the body of the message that went out.
func (r *recorder) last() map[string]any {
	if len(r.bodies) == 0 {
		return nil
	}
	return r.bodies[len(r.bodies)-1]
}

// TestASendThatFailsIsTriedAgain covers the pause the sender is built around: a
// send that fails while the platform is briefly unready is tried again rather
// than dropped, because an answer that never arrives is indistinguishable, from
// the group's side, from a bot that ignored them.
func TestASendThatFailsIsTriedAgain(t *testing.T) {
	platform := platform(t, func(attempt int) (int, string) {
		if attempt < 3 {
			return http.StatusInternalServerError, `{"code":1,"message":"busy"}`
		}
		return ok(attempt)
	})

	err := sendToGroup(t, platform, Message{Text: "hi"})
	if err != nil {
		t.Fatalf("the send was given up on: %v", err)
	}
	// At least the two failures the sender sits through and the one that worked.
	// More would only mean the SDK retried as well, which is not this test's
	// business.
	if platform.attempts < 3 {
		t.Errorf("the send was tried %d times, want the failures and then the success",
			platform.attempts)
	}
}

// TestASendThatKeepsFailingIsGivenUpOn covers the other end: the sender does not
// hammer a server that is genuinely refusing, and it reports the failure instead
// of swallowing it.
func TestASendThatKeepsFailingIsGivenUpOn(t *testing.T) {
	platform := platform(t, func(int) (int, string) {
		return http.StatusInternalServerError, `{"code":1,"message":"always busy"}`
	})

	if err := sendToGroup(t, platform, Message{Text: "hi"}); err == nil {
		t.Error("a send that never succeeds must report a failure")
	}
	if platform.attempts < sendAttempts {
		t.Errorf("the send was tried %d times, want at least the %d the sender promises",
			platform.attempts, sendAttempts)
	}
}

// TestEveryMessageIsMarkdown covers the rule that is easy to miss.
//
// Buttons ride on a markdown message, and the platform drops them from a plain
// one without reporting anything. Nothing here can make a plain message, so no
// caller can accidentally send one.
func TestEveryMessageIsMarkdown(t *testing.T) {
	platform := platform(t, ok)
	keyboard := &qqbotsdk.Keyboard{Content: &qqbotsdk.KeyboardContent{Rows: []qqbotsdk.Row{
		{Buttons: []qqbotsdk.Button{{
			ID:         "one",
			RenderData: &qqbotsdk.RenderData{Label: "标签", VisitedLabel: "标签"},
			Action: &qqbotsdk.Action{
				Type: qqbotsdk.ActionTypeCallback, Data: "payload",
				Permission:    &qqbotsdk.Permission{Type: qqbotsdk.PermissionTypeEveryone},
				UnsupportTips: "请升级 QQ 客户端",
			},
		}}},
	}}}

	err := sendToGroup(t, platform, Message{Text: "**正文**", Keyboard: keyboard})
	if err != nil {
		t.Fatalf("sending: %v", err)
	}

	body := platform.last()
	if body["msg_type"] != float64(qqbotsdk.MsgTypeMarkdown) {
		t.Errorf("msg_type = %v, want markdown", body["msg_type"])
	}
	markdown, _ := body["markdown"].(map[string]any)
	if markdown["content"] != "**正文**" {
		t.Errorf("markdown content = %v, want the text", markdown["content"])
	}
	if _, ok := body["keyboard"].(map[string]any); !ok {
		t.Error("the buttons were dropped on the way out")
	}
}

// TestTheSecondReplyToAMessageSaysSo covers the count a bot has to keep when it
// answers one message more than once: the platform treats replies sharing a
// message and a sequence as duplicates of each other.
func TestTheSecondReplyToAMessageSaysSo(t *testing.T) {
	for _, reply := range []struct {
		sequence int
		want     float64
	}{
		{sequence: 0, want: 1}, // the first reply, whether it says so or not
		{sequence: 1, want: 1},
		{sequence: 2, want: 2},
		{sequence: 5, want: 5},
	} {
		platform := platform(t, ok)
		err := sendToGroup(t, platform, Message{
			Text: "hi", ReplyTo: "COMMAND-MESSAGE", Sequence: reply.sequence,
		})
		if err != nil {
			t.Fatalf("sending: %v", err)
		}
		if got := platform.last()["msg_seq"]; got != reply.want {
			t.Errorf("sequence %d was sent as msg_seq %v, want %v",
				reply.sequence, got, reply.want)
		}
	}
}

// TestAMessageOfItsOwnCarriesNoReply covers the other kind of send: one that
// answers nothing, which is how a bot speaks without being asked.
func TestAMessageOfItsOwnCarriesNoReply(t *testing.T) {
	platform := platform(t, ok)
	if err := sendToGroup(t, platform, Message{Text: "hi"}); err != nil {
		t.Fatalf("sending: %v", err)
	}

	body := platform.last()
	if _, sent := body["msg_id"]; sent {
		t.Errorf("a message of its own was sent as a reply: %v", body["msg_id"])
	}
	if _, sent := body["msg_seq"]; sent {
		t.Errorf("a message of its own carried a sequence: %v", body["msg_seq"])
	}
}

// TestAnAnswerToAnEventAnswersTheEvent covers the third way a message can be
// attached to something: a button press, which is an event rather than a message.
//
// It is what makes words after a press cost nothing: an answer to an event is not
// counted against what the bot may say unasked, and a press carries its own event id
// for exactly this.
func TestAnAnswerToAnEventAnswersTheEvent(t *testing.T) {
	platform := platform(t, ok)
	if err := sendToGroup(t, platform, Message{Text: "hi", ReplyToEvent: "EVENT-ID"}); err != nil {
		t.Fatalf("sending: %v", err)
	}

	body := platform.last()
	if body["event_id"] != "EVENT-ID" {
		t.Errorf("event_id = %v, want the event the press came in", body["event_id"])
	}
	// The platform refuses both at once, so an answer to an event must not also
	// claim to answer a message.
	if _, sent := body["msg_id"]; sent {
		t.Errorf("an answer to an event also answered a message: %v", body["msg_id"])
	}
}

// TestADestinationIsRequired covers what the platform would otherwise be asked to
// work out: which of the two endpoints to use.
func TestADestinationIsRequired(t *testing.T) {
	platform := platform(t, ok)
	cases := map[string]Message{
		"neither": {Text: "hi"},
		"both":    {GroupOpenID: "G", UserOpenID: "U", Text: "hi"},
	}
	for why, message := range cases {
		if _, err := Send(context.Background(), platform.client, message); err == nil {
			t.Errorf("%s was accepted: %+v", why, message)
		}
	}
	if platform.attempts != 0 {
		t.Errorf("%d request(s) reached the platform for a message that has no "+
			"destination", platform.attempts)
	}
}

// TestTheTwoDestinationsUseTheirOwnEndpoints covers the pair: a group and a
// single chat are two endpoints, not one with a flag.
func TestTheTwoDestinationsUseTheirOwnEndpoints(t *testing.T) {
	platform := platform(t, ok)
	ctx := context.Background()

	if _, err := Send(ctx, platform.client, Message{GroupOpenID: "GROUP-OPENID", Text: "hi"}); err != nil {
		t.Fatalf("sending to a group: %v", err)
	}
	if got := platform.path; got != "/v2/groups/GROUP-OPENID/messages" {
		t.Errorf("a group message went to %q", got)
	}

	if _, err := Send(ctx, platform.client, Message{UserOpenID: "USER-OPENID", Text: "hi"}); err != nil {
		t.Fatalf("sending to a single chat: %v", err)
	}
	if got := platform.path; got != "/v2/users/USER-OPENID/messages" {
		t.Errorf("a single-chat message went to %q", got)
	}
}

// TestThereIsNothingToSendWith covers the mistake a half-wired feature makes: no
// client at all.
func TestThereIsNothingToSendWith(t *testing.T) {
	_, err := Send(context.Background(), nil, Message{GroupOpenID: "G", Text: "hi"})
	if err == nil {
		t.Error("sending with no client was accepted")
	}
}

// sendToGroup is Send to one group, with the failure spelled out by the test.
func sendToGroup(t *testing.T, platform *recorder, message Message) error {
	t.Helper()
	if message.GroupOpenID == "" && message.UserOpenID == "" {
		message.GroupOpenID = "GROUP-OPENID"
	}
	_, err := Send(context.Background(), platform.client, message)
	return err
}
