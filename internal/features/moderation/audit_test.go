package moderation

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// auditedHarness builds the feature with a cache, a stub model and a real
// database, which is what the record needs.
func auditedHarness(t *testing.T, stub *modelStub) (*handler, string, store.JudgementStore) {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run the audit tests against a real Redis " +
			"(the bot's own is at 127.0.0.1:6380)")
	}
	opened, err := store.Open(context.Background(), store.Config{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "audit.db"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { opened.Close() })

	server := stub.start(t)
	group := "G-" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	section := `
enabled: true
context_before: 10
context_after: 10
chain_minutes: 10
default_mute: "5m"
model:
  base_url: "` + server.URL + `/v1"
  name: "stub-model"
  timeout_seconds: 5
categories:
  ad:
    label: "广告"
    mute: "10m"
`
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(section), &document); err != nil {
		t.Fatalf("decoding the section: %v", err)
	}
	instance, err := New(*document.Content[0], feature.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Redis:  config.Redis{Addr: addr, Prefix: "qgb-test", DialTimeoutSeconds: 3},
		Store:  opened,
	})
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	h := instance.(*handler)
	t.Cleanup(func() {
		ctx := context.Background()
		set, index := h.cache.keys(group)
		h.cache.client.Del(ctx, set, index)
		h.cache.Close()
	})
	return h, group, opened.Judgements()
}

// TestAJudgementIsWrittenDown covers the record a punishment is reviewed through:
// it is written when the judgement is reached, before anybody acts on it.
func TestAJudgementIsWrittenDown(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"violation","category":"ad",` +
		`"reason":"卖号广告","confidence":0.9}`}
	h, group, judgements := auditedHarness(t, stub)
	quoted := cacheChain(t, h, group, "正常聊天", "加群送皮肤 私聊我")

	report, err := h.JudgeQuoted(context.Background(), group, quoted, "", "REPORTER-1")
	if err != nil {
		t.Fatalf("JudgeQuoted: %v", err)
	}
	if report.JudgementID == "" {
		t.Fatal("the judgement was not written down")
	}

	entries, err := judgements.Recent(context.Background(), group, report.SubjectOpenID,
		time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the record holds %d judgements, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Category != "ad" || entry.Verdict != store.JudgementViolation {
		t.Errorf("recorded %+v, want the violation", entry)
	}
	if entry.ReporterOpenID != "REPORTER-1" {
		t.Errorf("reporter = %q, want the one that was passed in", entry.ReporterOpenID)
	}
	if entry.Reason != "卖号广告" {
		t.Errorf("reason = %q, want the model's own words kept for review",
			entry.Reason)
	}
	if len(entry.MessageIDs) == 0 {
		t.Error("the record does not say which messages were judged")
	}
	// What was done is not known yet, and the record says so rather than claiming
	// something happened.
	if entry.Action != "" {
		t.Errorf("action = %q before anybody acted", entry.Action)
	}

	// The caller closes it once it has acted, with what was taken back and why.
	if err := h.RecordOutcome(context.Background(), report.JudgementID, store.Outcome{
		Action:       "已撤回 1 条消息 已禁言 10分钟",
		MuteSeconds:  600,
		RecallReason: "已撤回 1 条消息",
		Recalls: []store.RecallOutcome{
			{ID: "QUOTED-MESSAGE", Number: 1, Text: "卖号广告", Recalled: true},
		},
	}); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	entries, err = judgements.Recent(context.Background(), group, report.SubjectOpenID,
		time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("Recent after the outcome: %v", err)
	}
	if entries[0].Action != "已撤回 1 条消息 已禁言 10分钟" ||
		entries[0].MuteSeconds != 600 {
		t.Errorf("outcome = %q, %d; want what the caller did",
			entries[0].Action, entries[0].MuteSeconds)
	}
	if entries[0].RecallReason == "" || len(entries[0].Recalls) != 1 {
		t.Errorf("recall = %+v, %q; want what went and why",
			entries[0].Recalls, entries[0].RecallReason)
	}
}

// TestAJudgementThatWasNotReachedIsRecordedAsSuch covers the column that exists
// to keep two different facts apart: a message nobody could judge must not be
// recorded as a message that was found acceptable.
func TestAJudgementThatWasNotReachedIsRecordedAsSuch(t *testing.T) {
	stub := &modelStub{answer: "我不知道"} // unreadable, so no judgement happens
	h, group, judgements := auditedHarness(t, stub)
	quoted := cacheChain(t, h, group, "正常聊天")

	if _, err := h.JudgeQuoted(context.Background(), group, quoted, "", "REPORTER-1"); err == nil {
		t.Fatal("an unreadable answer must not be a judgement")
	}

	entries, err := judgements.Recent(context.Background(), group, "MEMBER-1",
		time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the record holds %d judgements, want 1 even for a failure",
			len(entries))
	}
	if entries[0].Verdict != store.JudgementError {
		t.Errorf("verdict = %q, want %q: nobody looked, and that is not the same "+
			"as nothing being there", entries[0].Verdict, store.JudgementError)
	}
}
