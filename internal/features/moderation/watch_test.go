package moderation

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

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/mutelog"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// The high risk list is the one thing this feature keeps about a member rather
// than about a message. These tests cover the two halves of it: the list itself,
// which is a mark with a moment it ends, and what the message path does with a
// member who is on it.

const (
	watchGroup  = "GROUP-OPENID"
	watchMember = "MEMBER-WATCHED"
	watchOther  = "MEMBER-SOMEONE"
)

// TestAMarkIsEnforcedWhileItLasts covers the in-memory list the message path asks
// about: a mark in force answers yes, a mark that has run out answers no and is
// dropped, and a list that is turned off enforces nothing.
func TestAMarkIsEnforcedWhileItLasts(t *testing.T) {
	h := judgeHarness(t, &modelStub{answer: `{"verdict":"ok"}`}, "")

	if h.isWatched(watchMember) {
		t.Error("somebody was watched before anybody marked them")
	}
	h.remember(watchMember, time.Now().Add(time.Hour).Unix())
	if !h.isWatched(watchMember) {
		t.Error("a mark in force was not enforced")
	}
	if h.isWatched(watchOther) {
		t.Error("a mark was enforced against somebody else")
	}

	// A mark that has run out is not enforced, and remembering it is what the
	// message path finds -- the row is still in the table until somebody reads it.
	h.remember(watchMember, time.Now().Add(-time.Minute).Unix())
	if h.isWatched(watchMember) {
		t.Error("a mark that had run out was still enforced")
	}

	// And the switch is the whole list, not only the marking of it.
	h.remember(watchMember, time.Now().Add(time.Hour).Unix())
	off := false
	fresh := h.config()
	fresh.HighRisk.Enabled = &off
	h.cfg = fresh
	if h.isWatched(watchMember) {
		t.Error("a mark was enforced with the list turned off")
	}
}

// TestTheListRoundTripsThroughTheDataLayer covers the list as it is kept: marking
// writes it down and remembers it, unmarking forgets both, and unmarking somebody
// who was not marked says so instead of claiming a change.
func TestTheListRoundTripsThroughTheDataLayer(t *testing.T) {
	h := judgeHarness(t, &modelStub{answer: `{"verdict":"ok"}`}, "")
	h.deps.Store = openWatchStore(t)

	until := time.Now().Add(7 * 24 * time.Hour).Truncate(time.Second)
	if err := h.Watch(context.Background(), store.Watch{
		MemberOpenID: watchMember,
		Reason:       "广告",
		AddedBy:      "ADMIN-OPENID",
		ExpiresAt:    until.Unix(),
	}); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !h.isWatched(watchMember) {
		t.Error("a mark that was written down is not being enforced")
	}

	entries, err := h.Watches(context.Background())
	if err != nil {
		t.Fatalf("Watches: %v", err)
	}
	if len(entries) != 1 || entries[0].MemberOpenID != watchMember ||
		entries[0].Reason != "广告" || entries[0].ExpiresAt != until.Unix() {
		t.Errorf("Watches returned %+v, want the mark that was made", entries)
	}

	removed, err := h.Unwatch(context.Background(), watchMember)
	if err != nil {
		t.Fatalf("Unwatch: %v", err)
	}
	if !removed {
		t.Error("Unwatch reported nothing removed although the mark was there")
	}
	if h.isWatched(watchMember) {
		t.Error("a mark that was removed is still being enforced from memory")
	}
	removed, err = h.Unwatch(context.Background(), watchMember)
	if err != nil {
		t.Fatalf("Unwatch again: %v", err)
	}
	if removed {
		t.Error("Unwatch reported a removal for somebody who was not marked")
	}
}

// TestAMarkMustEnd covers the rule the command is built around: there is no
// permanent entry, and the refusal comes from the layer that would keep it.
func TestAMarkMustEnd(t *testing.T) {
	h := judgeHarness(t, &modelStub{answer: `{"verdict":"ok"}`}, "")
	h.deps.Store = openWatchStore(t)

	if err := h.Watch(context.Background(), store.Watch{
		MemberOpenID: watchMember,
	}); err == nil {
		t.Fatal("a mark with no moment it ends was accepted")
	}
	if h.isWatched(watchMember) {
		t.Error("a mark that was refused is being enforced")
	}
}

// platformStub answers the calls a punishment makes, and writes down what it was
// asked: what was taken back, who was silenced for how long, and whether anything
// was posted to the group.
type platformStub struct {
	mu      sync.Mutex
	recalls []string
	mutes   []muteRequest
	posts   int
}

// muteRequest is one silencing, as the platform received it.
type muteRequest struct {
	memberOpenID string
	expireAt     string
}

func (p *platformStub) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		switch {
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/messages/"):
			parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
			p.recalls = append(p.recalls, parts[len(parts)-1])
		case strings.HasSuffix(r.URL.Path, "/restrict_chat_setting"):
			var request struct {
				Members []struct {
					MemberOpenID string `json:"member_openid"`
					Op           string `json:"op"`
					MuteExpireAt string `json:"mute_expire_at"`
				} `json:"members"`
			}
			_ = json.Unmarshal(body, &request)
			for _, member := range request.Members {
				p.mutes = append(p.mutes, muteRequest{
					memberOpenID: member.MemberOpenID,
					expireAt:     member.MuteExpireAt,
				})
			}
		case strings.HasSuffix(r.URL.Path, "/messages"):
			p.posts++
		}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"SENT-1"}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func (p *platformStub) recalled() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.recalls...)
}

func (p *platformStub) muted() []muteRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]muteRequest(nil), p.mutes...)
}

func (p *platformStub) posted() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.posts
}

// openWatchStore returns a data layer on a throwaway database, for the tests whose
// subject is the list rather than the judgement.
func openWatchStore(t *testing.T) store.Store {
	t.Helper()
	opened, err := store.Open(context.Background(), store.Config{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "bot.db"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { opened.Close() })
	return opened
}

// watchHarness builds the feature with everything a punishment needs: a stub
// model, a stub platform, a data layer and the cache.
//
// It is skipped where the cache is unreachable, because the window a judgement is
// made in is read out of it and a fake would agree with whatever the code did.
func watchHarness(t *testing.T, stub *modelStub, platform *platformStub,
	extra string) *handler {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run the high risk tests against a real Redis " +
			"(the bot's own is at 127.0.0.1:6380)")
	}
	modelServer := stub.start(t)
	platformServer := platform.start(t)
	client, err := qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AccessToken: "test-token",
		BaseURL:     platformServer.URL,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}

	section := `
enabled: true
dry_run: false
model:
  base_url: "` + modelServer.URL + `/v1"
  api_key: "test-key"
  name: "stub-model"
  timeout_seconds: 5
categories:
  ad:
    label: "广告"
    mute: "10m"
high_risk:
  auto_mark_after: 2
  auto_mark_for: "168h"
` + extra

	var document yaml.Node
	if err := yaml.Unmarshal([]byte(section), &document); err != nil {
		t.Fatalf("decoding the section: %v", err)
	}
	if len(document.Content) == 0 {
		t.Fatal("the section produced no node")
	}
	instance, err := New(*document.Content[0], feature.Deps{
		Client: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Groups: config.Groups{{OpenID: watchGroup}},
		Store:  openWatchStore(t),
		Redis:  config.Redis{Addr: addr, Prefix: "qgb-test", DialTimeoutSeconds: 3},
		Mutes:  mutelog.New(),
	})
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	h := instance.(*handler)
	t.Cleanup(func() {
		// The window is read back out of Redis, and every test in this file records
		// into the same group. Messages left behind sit inside the next run's window
		// and shift the positions a judgement's numbers point at, which fails on the
		// second run and reads as "the wrong message was recalled" -- so the keys go
		// with the test that made them.
		ctx := context.Background()
		set, index := h.cache.keys(watchGroup)
		h.cache.client.Del(ctx, set, index)
		h.Close(ctx)
	})
	return h
}

// TestAWatchedMessageIsJudgedAndPunished covers the whole of what being on the
// list does: the window around the message is judged, the message the judge named
// is taken back, the member is silenced for the category's duration, and the record
// says so -- with nothing posted to the group, which is the difference from a
// report.
func TestAWatchedMessageIsJudgedAndPunished(t *testing.T) {
	platform := &platformStub{}
	stub := &modelStub{answer: `{"verdict":"violation","category":"ad","recall":[2],` +
		`"reason":"推广竞品站点","confidence":0.9}`}
	h := watchHarness(t, stub, platform, "")

	start := time.Now().Add(-time.Minute)
	chain := []CachedMessage{
		{ID: "MSG-1", Idx: "IDX-1", User: watchOther, Name: "别人",
			Text: "正常聊天", TS: start.UnixMilli()},
		{ID: "MSG-2", Idx: "IDX-2", User: watchMember, Name: "可疑的人",
			Text: "0.01x dsv41f q1.1110103.xyz", TS: start.Add(time.Second).UnixMilli()},
		{ID: "MSG-3", Idx: "IDX-3", User: watchOther, Name: "别人",
			Text: "?什么", TS: start.Add(2 * time.Second).UnixMilli()},
	}
	ctx := context.Background()
	for _, message := range chain {
		if err := h.cache.Record(ctx, watchGroup, message); err != nil {
			t.Fatalf("caching a message: %v", err)
		}
	}
	if err := h.Watch(ctx, store.Watch{
		MemberOpenID: watchMember,
		Reason:       "广告",
		AddedBy:      "ADMIN-OPENID",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	h.judgeWatched(ctx, watchGroup, chain[1])

	// The judged message and only that one: the judge is shown the whole window and
	// may name any of it, and what is dealt with is what the marked member said.
	if recalls := platform.recalled(); len(recalls) != 1 || recalls[0] != "MSG-2" {
		t.Errorf("recalled %v, want only the watched member's message", recalls)
	}
	mutes := platform.muted()
	if len(mutes) != 1 || mutes[0].memberOpenID != watchMember {
		t.Fatalf("muted %+v, want the member who was judged", mutes)
	}
	expireAt, err := time.Parse(time.RFC3339, mutes[0].expireAt)
	if err != nil {
		t.Fatalf("reading the mute's expiry %q: %v", mutes[0].expireAt, err)
	}
	if remaining := time.Until(expireAt); remaining < 9*time.Minute ||
		remaining > 10*time.Minute {
		t.Errorf("the mute lasts %s, want the category's ten minutes", remaining)
	}
	if posted := platform.posted(); posted != 0 {
		t.Errorf("posted %d messages to the group, want none: nothing was reported, so "+
			"nothing is announced", posted)
	}

	// The record is what an administrator reads afterwards: it names no reporter,
	// because nobody reported it, and it says what was done.
	entries, err := h.deps.Store.Judgements().Recent(ctx, watchGroup, watchMember, time.Time{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("recorded %d judgements, want 1", len(entries))
	}
	got := entries[0]
	if got.Verdict != store.JudgementViolation || got.Category != "ad" {
		t.Errorf("recorded verdict %q category %q, want the violation that was found",
			got.Verdict, got.Category)
	}
	if got.ReporterOpenID != "" {
		t.Errorf("recorded reporter %q, want none: nobody reported it", got.ReporterOpenID)
	}
	if !strings.HasPrefix(got.Action, "自动送检：") ||
		!strings.Contains(got.Action, "已禁言") {
		t.Errorf("recorded action %q, want what was done and that it was automatic",
			got.Action)
	}
	if got.MuteSeconds != 600 {
		t.Errorf("recorded mute %d seconds, want the category's 600", got.MuteSeconds)
	}
	if len(got.Recalls) != 1 || !got.Recalls[0].Recalled {
		t.Errorf("recorded recalls %+v, want the one message that was taken back",
			got.Recalls)
	}
}

// TestAMessageFoundAcceptableIsRecordedAndNothingElse covers the other outcome: a
// marked member saying something ordinary is judged, written down, and left alone.
func TestAMessageFoundAcceptableIsRecordedAndNothingElse(t *testing.T) {
	platform := &platformStub{}
	stub := &modelStub{answer: `{"verdict":"ok","category":"","recall":[],` +
		`"reason":"正常聊天","confidence":0.9}`}
	h := watchHarness(t, stub, platform, "")

	message := CachedMessage{ID: "MSG-1", Idx: "IDX-1", User: watchMember,
		Name: "可疑的人", Text: "今天天气不错", TS: time.Now().UnixMilli()}
	ctx := context.Background()
	if err := h.cache.Record(ctx, watchGroup, message); err != nil {
		t.Fatalf("caching a message: %v", err)
	}
	h.judgeWatched(ctx, watchGroup, message)

	if recalled := platform.recalled(); len(recalled) != 0 {
		t.Errorf("recalled %v, want nothing for a message that was found acceptable",
			recalled)
	}
	if mutes := platform.muted(); len(mutes) != 0 {
		t.Errorf("muted %+v, want nobody", mutes)
	}
	entries, err := h.deps.Store.Judgements().Recent(ctx, watchGroup, watchMember, time.Time{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != 1 || entries[0].Verdict != store.JudgementOK {
		t.Fatalf("recorded %+v, want the one judgement, found acceptable", entries)
	}
	if entries[0].Action != "" {
		t.Errorf("recorded action %q, want none: nothing was done", entries[0].Action)
	}
}

// TestBeingCaughtEnoughTimesMarksSomebody covers the half nobody asked for: a
// member who has been found breaking a rule as many times as the configuration
// says is marked without an administrator doing it.
func TestBeingCaughtEnoughTimesMarksSomebody(t *testing.T) {
	h := judgeHarness(t, &modelStub{answer: `{"verdict":"ok"}`}, `
high_risk:
  auto_mark_after: 2
  auto_mark_for: "168h"
`)
	h.deps.Store = openWatchStore(t)
	ctx := context.Background()

	catch := func(id string) {
		t.Helper()
		if err := h.deps.Store.Judgements().Record(ctx, store.Judgement{
			ID:            id,
			GroupOpenID:   watchGroup,
			SubjectOpenID: watchMember,
			Verdict:       store.JudgementViolation,
			CreatedAt:     time.Now().Unix(),
		}); err != nil {
			t.Fatalf("record %s: %v", id, err)
		}
	}

	// Once is not twice: the threshold is the file's, and a member is not marked
	// for the first thing they are caught doing.
	catch("J-1")
	h.markIfCaughtEnough(ctx, watchMember)
	if h.isWatched(watchMember) {
		t.Fatal("marked after one violation, want the threshold the file asked for")
	}

	catch("J-2")
	h.markIfCaughtEnough(ctx, watchMember)
	if !h.isWatched(watchMember) {
		t.Fatal("not marked after the second violation")
	}
	entries, err := h.Watches(ctx)
	if err != nil {
		t.Fatalf("Watches: %v", err)
	}
	if len(entries) != 1 || !strings.Contains(entries[0].Reason, "自动标记") {
		t.Errorf("the list holds %+v, want the automatic mark and why it was made",
			entries)
	}
	// The automatic mark has the duration the file gave it, and no administrator.
	if entries[0].AddedBy != "" {
		t.Errorf("AddedBy = %q, want nobody: no administrator made this mark",
			entries[0].AddedBy)
	}
	if entries[0].ExpiresAt <= time.Now().Unix() {
		t.Error("the automatic mark has no moment it ends")
	}
	// And it is not made twice for the same member: a second mark would only move
	// the moment the first one ends.
	before := entries[0].ID
	catch("J-3")
	h.markIfCaughtEnough(ctx, watchMember)
	entries, err = h.Watches(ctx)
	if err != nil {
		t.Fatalf("Watches after the third violation: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != before {
		t.Errorf("the list holds %+v, want the mark that was already there", entries)
	}
}
