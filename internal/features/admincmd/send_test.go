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

// The sending itself -- the retry, the pause between attempts, the sequence a
// repeated reply carries -- is internal/messaging, and is tested there. What is
// left to cover here is the part this feature adds on top: what a command's
// answer is an answer to.

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
