package admincmd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// sendHarness builds just enough of the feature to send with: a client pointed
// at a server the test controls, and nothing else.
//
// respond is given the attempt number and answers with a status and a body, so a
// test can fail twice and then succeed.
func sendHarness(t *testing.T, respond func(attempt int) (int, string)) (*handler, func() int) {
	t.Helper()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		status, body := respond(attempts)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	client, err := qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AccessToken: "test-token",
		BaseURL:     server.URL,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	return &handler{deps: feature.Deps{
		Client: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}, func() int { return attempts }
}

// TestASendThatFailsIsTriedAgain covers the pause the sender is built around: a
// send that fails while the platform is briefly unready is tried again rather
// than dropped, because an answer that never arrives is indistinguishable, from
// the group's side, from a bot that ignored them.
func TestASendThatFailsIsTriedAgain(t *testing.T) {
	h, attempts := sendHarness(t, func(attempt int) (int, string) {
		if attempt < 3 {
			return http.StatusInternalServerError, `{"code":1,"message":"busy"}`
		}
		return http.StatusOK, `{}`
	})

	if err := h.sendMessage(context.Background(), testGroupOpenID, "hi", ""); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	// At least the two failures the sender sits through and the one that worked.
	// More would only mean the SDK retried as well, which is not this test's
	// business.
	if got := attempts(); got < 3 {
		t.Errorf("the send was tried %d times, want the failures and then the success", got)
	}
}

// TestASendThatKeepsFailingIsGivenUpOn covers the other end: the sender does not
// hammer a server that is genuinely refusing, and it reports the failure instead
// of swallowing it.
func TestASendThatKeepsFailingIsGivenUpOn(t *testing.T) {
	h, attempts := sendHarness(t, func(int) (int, string) {
		return http.StatusInternalServerError, `{"code":1,"message":"always busy"}`
	})

	if err := h.sendMessage(context.Background(), testGroupOpenID, "hi", ""); err == nil {
		t.Error("a send that never succeeds must report a failure")
	}
	if got := attempts(); got < sendAttempts {
		t.Errorf("the send was tried %d times, want at least the %d the sender promises",
			got, sendAttempts)
	}
}

// TestAnAnswerStillAnswersItsMessage covers what the single sender must not lose
// on the way: a command's answer is a reply to the message that carried it, which
// is the only kind of answer the platform allows to an event.
func TestAnAnswerStillAnswersItsMessage(t *testing.T) {
	var last map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&last)
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
	h := &handler{deps: feature.Deps{
		Client: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}

	data := &qqbotsdk.GroupMessageCreateData{ID: "COMMAND-MESSAGE", GroupOpenID: testGroupOpenID}
	h.reply(context.Background(), data, "ok")

	if last["msg_id"] != "COMMAND-MESSAGE" {
		t.Errorf("sent %v, want it to answer msg_id COMMAND-MESSAGE", last["msg_id"])
	}
	if last["msg_seq"] != float64(1) {
		t.Errorf("sent msg_seq %v, want 1", last["msg_seq"])
	}
}
