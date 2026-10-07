package moderation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// locateCache is a cache on a real Redis, which is where the matching happens.
func locateCache(t *testing.T) *Cache {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run the cache tests against a real Redis " +
			"(the bot's own is at 127.0.0.1:6380)")
	}
	cache := NewCache(config.Redis{Addr: addr, Prefix: "qgb-test", DialTimeoutSeconds: 3},
		6*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		ctx := context.Background()
		set, index := cache.keys("G-" + t.Name())
		cache.client.Del(ctx, set, index)
		cache.Close()
	})
	return cache
}

// TestAQuoteIsLocatedByItsTextAndAuthor covers the way in that a temporary index
// does not have, and the rule that makes it safe.
//
// A quote of a message that is itself a quote comes with an index the cache can never
// hold, so what the quote showed has to find the message. What it showed is not
// enough on its own, though: two members can write the same words, and the platform
// names the sender only sometimes. The text is therefore matched *within one member's
// messages*, and a search that names nobody finds nothing rather than guessing whose
// message it was.
func TestAQuoteIsLocatedByItsTextAndAuthor(t *testing.T) {
	cache := locateCache(t)
	ctx := context.Background()
	group := "G-" + t.Name()
	now := time.Now().UnixMilli()

	for _, message := range []CachedMessage{
		{ID: "M-1", Idx: "IDX-1", User: "MEMBER-1", TS: now - 60_000, Text: "成果怎么样？"},
		{ID: "M-2", Idx: "IDX-2", User: "MEMBER-2", TS: now - 30_000, Text: "在的"},
	} {
		if err := cache.Record(ctx, group, message); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	// What the platform hands over: a small report the quoted message makes about
	// itself, scaffolding and all.
	rendered := "=== 消息 1 ===\n[消息内容]   成果怎么样？\n[消息类型] 引用消息\n"

	found, err := cache.Locate(ctx, group, rendered, "MEMBER-1", 30*time.Minute)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if found.Idx != "IDX-1" {
		t.Errorf("located %s, want the message that said it", found.Idx)
	}

	// Somebody else saying the same words is not that message, so it neither
	// becomes the answer nor makes the search ambiguous.
	if err := cache.Record(ctx, group, CachedMessage{ID: "M-3", Idx: "IDX-3",
		User: "MEMBER-9", TS: now - 20_000, Text: "成果怎么样？"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	found, err = cache.Locate(ctx, group, rendered, "MEMBER-1", 30*time.Minute)
	if err != nil || found.Idx != "IDX-1" {
		t.Errorf("located %q, %v; want the member's own message rather than somebody "+
			"else's copy of it", found.Idx, err)
	}
	// And asking for the member who never said it finds nothing: their messages do
	// not contain the text, however many other members' do.
	if _, err := cache.Locate(ctx, group, rendered, "MEMBER-2", 30*time.Minute); err == nil {
		t.Error("a text match was accepted for a member who never wrote it")
	}

	// The same member saying it twice is two matches and no answer: picking either
	// would silence a message the quote may not have meant.
	if err := cache.Record(ctx, group, CachedMessage{ID: "M-4", Idx: "IDX-4",
		User: "MEMBER-1", TS: now - 10_000, Text: "成果怎么样？我也想问"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := cache.Locate(ctx, group, rendered, "MEMBER-1", 30*time.Minute); err == nil {
		t.Error("two matches of one member must be a miss, not a coin toss")
	}

	// No sender named, no search. The text would match a message, and choosing it
	// would be choosing *whose* message it is, which is the one thing the text
	// cannot say -- so this is the refusal, not a guess.
	if _, err := cache.Locate(ctx, group, rendered, "", 30*time.Minute); !errors.Is(err, ErrNotCached) {
		t.Errorf("err = %v with no sender named, want a refusal", err)
	}
	if _, err := cache.Locate(ctx, group, rendered, "   ", 30*time.Minute); !errors.Is(err, ErrNotCached) {
		t.Errorf("err = %v with a blank sender, want the same refusal", err)
	}

	// A short line is not evidence: everybody says "在的", and matching on one would
	// point at the wrong message.
	if _, err := cache.Locate(ctx, group, "[消息内容] 在的", "MEMBER-2", 30*time.Minute); err == nil {
		t.Error("a short line must not be enough to point at a message")
	}

	// And a quote of something nobody said stays nothing.
	if _, err := cache.Locate(ctx, group, "[消息内容] 谁也没说过这一句", "MEMBER-1",
		30*time.Minute); !errors.Is(err, ErrNotCached) {
		t.Errorf("err = %v, want ErrNotCached", err)
	}
}

// TestATemporaryQuoteIsResolvedEndToEnd covers the join between locating a message
// and naming it, for the one case where the text may be used at all: the platform
// named the sender along with it.
//
// The bug it exists for: the window was found by the text, and then the anchor was
// still looked for by the temporary index the quote carried -- which is not in the
// window and never could be. So locating succeeded, and the whole report still came
// back as "the quoted message is not in the window", which is what a real member hit.
func TestATemporaryQuoteIsResolvedEndToEnd(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"violation","category":"ad",` +
		`"confidence":0.9,"recall":[1]}`}
	h, group := reportHarness(t, stub, "")
	// Two messages, and the quote's text names the second one. chainOf gives every
	// message the same author, which is the member the platform has to name for the
	// text to mean anything.
	cacheChain(t, h, group, "今天天气不错", "加群送皮肤 私聊我")

	// What the platform gives for a quote of a message that is itself a quote: a
	// temporary index, the text the quote showed, and -- when it says -- the sender.
	report, err := h.JudgeQuoted(context.Background(), group, feature.QuotedMessage{
		Index:  "TMP_94e31996-8fcd-4b2e-bdd3-09e9d23bb5e4",
		Text:   "=== 消息 1 ===\n[消息内容]   加群送皮肤 私聊我\n[消息类型] 引用消息\n",
		Author: "MEMBER-1",
	}, "REPORTER-1")
	if err != nil {
		t.Fatalf("a quote that can be located must be judged, not refused: %v", err)
	}
	if report.SubjectOpenID == "" {
		t.Error("nobody was identified as the one being judged")
	}
	if report.Category == "" {
		t.Errorf("report = %+v, want the verdict the model gave", report)
	}
	// The recall list is the located message, by its own id -- the message the
	// judgement actually saw, not the temporary name the quote carried.
	if len(report.RecallMessages) != 1 ||
		report.RecallMessages[0].ID == "TMP_94e31996-8fcd-4b2e-bdd3-09e9d23bb5e4" {
		t.Errorf("recall = %v, want the located message", report.RecallMessages)
	}
	// The index goes with it, because the caller has to find the message in the
	// cache again to mark it as taken back, and the temporary one finds nothing.
	if report.RecallMessages[0].Index == "" ||
		strings.HasPrefix(report.RecallMessages[0].Index, "TMP_") {
		t.Errorf("index = %q, want the located message's own index",
			report.RecallMessages[0].Index)
	}
}

// TestTheUsableNameIsTheOneThePlatformGaveProperly covers which of the two names a
// report carries gets used.
//
// An ordinary quote has the same value in both, so only a quote of a quote can tell
// them apart -- and there the scene falls back to a temporary index while the element
// may still hold the message's real one.
func TestTheUsableNameIsTheOneThePlatformGaveProperly(t *testing.T) {
	for name, testCase := range map[string]struct {
		index, element, want string
	}{
		"the scene's name, when it is real":          {"REFIDX_a", "REFIDX_b", "REFIDX_a"},
		"the element's, when the scene is temporary": {"TMP_1", "REFIDX_b", "REFIDX_b"},
		"the element's, when the scene is empty":     {"", "REFIDX_b", "REFIDX_b"},
		"nothing, when both are temporary":           {"TMP_1", "TMP_2", ""},
		"nothing, when neither names the message":    {"", "", ""},
		"nothing, when both are only blank":          {"   ", "  ", ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := usableIndex(feature.QuotedMessage{
				Index:        testCase.index,
				ElementIndex: testCase.element,
			})
			if got != testCase.want {
				t.Errorf("usableIndex(%q, %q) = %q, want %q",
					testCase.index, testCase.element, got, testCase.want)
			}
		})
	}
}

// TestARealElementIndexRescuesATemporaryQuote covers the case the scene cannot name:
// a quote of a message that is itself a quote, where the scene carries a temporary
// index and the element still carries the message's real one.
//
// Nothing is searched for here and no author is needed, because the cache is asked
// about the message by its own name. That is the difference between identifying a
// message and inferring one from words somebody else could equally well have written.
func TestARealElementIndexRescuesATemporaryQuote(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"violation","category":"ad",` +
		`"confidence":0.9,"recall":[1]}`}
	h, group := reportHarness(t, stub, "")
	realIndex := cacheChain(t, h, group, "今天天气不错", "加群送皮肤 私聊我")

	report, err := h.JudgeQuoted(context.Background(), group, feature.QuotedMessage{
		Index:        "TMP_94e31996-8fcd-4b2e-bdd3-09e9d23bb5e4",
		ElementIndex: realIndex,
		// Text travels as it always does and decides nothing here: the message is
		// named, so nothing has to be searched for -- and no sender is named either.
		Text: "=== 消息 1 ===\n[消息内容]   加群送皮肤 私聊我\n[消息类型] 引用消息\n",
	}, "REPORTER-1")
	if err != nil {
		t.Fatalf("a quote the element named properly must be judged: %v", err)
	}
	if report.Category == "" || report.SubjectOpenID == "" {
		t.Errorf("report = %+v, want a judgement about the named message", report)
	}
	// The message the element named is the one the judgement was pinned to: this is
	// the anchor, and it was found by its real index rather than by its words.
	if report.QuotedMessageID != "M-b" {
		t.Errorf("quoted message = %q, want the one the element named",
			report.QuotedMessageID)
	}
}

// TestAQuoteWithNoSenderIsNotGuessed covers the other half of that rule, and it is
// the reason the rule exists: with the text alone the search still finds exactly one
// message, so nothing but the missing sender stands between it and a punishment
// applied to whichever member happened to own that message.
//
// Production says this is the ordinary case: the platform named no sender in any of
// the 37 reports this deployment has received. So the answer is that the message
// cannot be told apart, and nobody is judged.
func TestAQuoteWithNoSenderIsNotGuessed(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"violation","category":"ad",` +
		`"confidence":0.9,"recall":[1]}`}
	h, group := reportHarness(t, stub, "")
	cacheChain(t, h, group, "今天天气不错", "加群送皮肤 私聊我")

	// Timed, because the answer is also meant to be immediate: a temporary index used
	// to sit through twelve one-second retries before saying no, and the reporter
	// watched a waiting message the whole way.
	started := time.Now()
	report, err := h.JudgeQuoted(context.Background(), group, feature.QuotedMessage{
		Index: "TMP_94e31996-8fcd-4b2e-bdd3-09e9d23bb5e4",
		Text:  "=== 消息 1 ===\n[消息内容]   加群送皮肤 私聊我\n[消息类型] 引用消息\n",
	}, "REPORTER-1")
	if waited := time.Since(started); waited > 2*time.Second {
		t.Errorf("the refusal took %v, want it at once rather than after the retries",
			waited)
	}
	if !errors.Is(err, feature.ErrUnidentifiedQuote) {
		t.Fatalf("err = %v, want the answer for a quote nobody named", err)
	}
	if report.SubjectOpenID != "" {
		t.Errorf("subject = %q, want nobody: no message was identified",
			report.SubjectOpenID)
	}
	// Nobody was asked: the refusal happens before the model is reached, and
	// before the twelve second wait a temporary index would otherwise sit through.
	if len(stub.requests) != 0 {
		t.Errorf("the model was asked %d time(s), want none", len(stub.requests))
	}
}
