package moderation

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/config"
)

// timeline builds messages spaced evenly from start.
func timeline(start time.Time, gap time.Duration, count int) []CachedMessage {
	ordered := make([]CachedMessage, 0, count)
	for index := 0; index < count; index++ {
		ordered = append(ordered, CachedMessage{
			ID:   fmt.Sprintf("M-%02d", index),
			Idx:  fmt.Sprintf("IDX-%02d", index),
			User: "MEMBER-1",
			TS:   start.Add(time.Duration(index) * gap).UnixMilli(),
		})
	}
	return ordered
}

// chainIdx lists what a chain selected, which is what the assertions are about.
func chainIdx(chain []CachedMessage) []string {
	indexes := make([]string, 0, len(chain))
	for _, message := range chain {
		indexes = append(indexes, message.Idx)
	}
	return indexes
}

// TestTheChainTakesTenEitherSide covers the count limit on its own: with room to
// spare, the quoted message pulls in ten neighbours in each direction.
func TestTheChainTakesTenEitherSide(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	// One minute apart, and a span generous enough not to bind.
	ordered := timeline(start, time.Minute, 41)

	chain := expandChain(ordered, 20, 10, 10, time.Hour)
	if len(chain) != 21 {
		t.Fatalf("the chain has %d messages, want the quoted one and ten either "+
			"side", len(chain))
	}
	if chain[0].Idx != "IDX-10" || chain[len(chain)-1].Idx != "IDX-30" {
		t.Errorf("the chain runs %s..%s, want IDX-10..IDX-30",
			chain[0].Idx, chain[len(chain)-1].Idx)
	}
}

// TestTheChainStopsAtTheSpan covers the other limit: ten either side one minute
// apart is twenty minutes end to end, so a ten minute span cuts it to five.
func TestTheChainStopsAtTheSpan(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	ordered := timeline(start, time.Minute, 41)

	chain := expandChain(ordered, 20, 10, 10, 10*time.Minute)
	if got := chainIdx(chain); len(got) != 11 {
		t.Fatalf("the chain has %d messages, want 11: five either side is ten "+
			"minutes end to end", len(got))
	}
	if chain[0].Idx != "IDX-15" || chain[len(chain)-1].Idx != "IDX-25" {
		t.Errorf("the chain runs %s..%s, want IDX-15..IDX-25",
			chain[0].Idx, chain[len(chain)-1].Idx)
	}
}

// TestTheChainStopsAtAGap covers the case the span limit exists for: a long
// quiet gap on one side stops that side without shortening the other.
func TestTheChainStopsAtAGap(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	ordered := timeline(start, time.Minute, 30)
	// Push everything before the quoted message half an hour earlier, leaving a
	// single long gap immediately to its left.
	for index := 0; index < 15; index++ {
		ordered[index].TS = start.Add(-time.Hour).Add(time.Duration(index) * time.Minute).UnixMilli()
	}

	chain := expandChain(ordered, 15, 10, 10, 10*time.Minute)
	for _, message := range chain {
		if strings.HasSuffix(message.Idx, "14") && message.Idx == "IDX-14" {
			t.Errorf("the chain crossed the gap: %v", chainIdx(chain))
			break
		}
	}
	if len(chain) < 11 {
		t.Errorf("the right side was shortened too: %v", chainIdx(chain))
	}
	if chain[0].Idx != "IDX-15" {
		t.Errorf("the chain starts at %s, want the quoted message itself", chain[0].Idx)
	}
}

// TestTheChainAlwaysHoldsTheQuotedMessage covers the report that is about one
// message and nothing around it.
func TestTheChainAlwaysHoldsTheQuotedMessage(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	ordered := timeline(start, time.Minute, 5)

	chain := expandChain(ordered, 2, 0, 0, time.Minute)
	if len(chain) != 1 || chain[0].Idx != "IDX-02" {
		t.Errorf("chain = %v, want only the quoted message", chainIdx(chain))
	}
}

// TestTheChainHandlesTheEnds covers a quoted message with nothing on one side.
func TestTheChainHandlesTheEnds(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	ordered := timeline(start, time.Second, 5)

	first := expandChain(ordered, 0, 10, 10, time.Hour)
	if len(first) != 5 || first[0].Idx != "IDX-00" {
		t.Errorf("chain at the start = %v, want all five", chainIdx(first))
	}
	last := expandChain(ordered, 4, 10, 10, time.Hour)
	if len(last) != 5 || last[len(last)-1].Idx != "IDX-04" {
		t.Errorf("chain at the end = %v, want all five", chainIdx(last))
	}
	if missing := expandChain(ordered, 9, 10, 10, time.Hour); missing != nil {
		t.Errorf("a rank outside the timeline gave %v, want nothing", chainIdx(missing))
	}
}

// testCache returns a cache on a real server, or skips saying why.
//
// It deliberately does not fabricate one: the whole point of these tests is that
// the window is built from what Redis really returns, and a fake would agree with
// whatever the code happens to do.
func testCache(t *testing.T, retention time.Duration) *Cache {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run the cache tests against a real Redis " +
			"(the bot's own is at 127.0.0.1:6380)")
	}
	group := "G-" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	cache := NewCache(config.Redis{Addr: addr, Prefix: "qgb-test", DialTimeoutSeconds: 3},
		retention, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		// Only this test's keys: the server may hold somebody else's data, so the
		// database is never flushed.
		ctx := context.Background()
		set, index := cache.keys(group)
		cache.client.Del(ctx, set, index)
		cache.Close()
	})
	if err := cache.Ping(context.Background()); err != nil {
		t.Skipf("the Redis at TEST_REDIS_ADDR did not answer: %v", err)
	}
	return cache
}

// TestContextBuildsTheWindow covers the round trip: what is written is what a
// quote can find,
func TestContextBuildsTheWindow(t *testing.T) {
	cache := testCache(t, 6*time.Hour)
	ctx := context.Background()
	group := "G-TestContextBuildsTheWindow"
	start := time.Now().Add(-time.Hour)

	ordered := timeline(start, time.Minute, 30)
	for _, message := range ordered {
		if err := cache.Record(ctx, group, message); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	chain, err := cache.Context(ctx, group, "IDX-15", 10, 10, 10*time.Minute)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if len(chain) != 11 {
		t.Fatalf("the chain has %d messages, want 11 (ten minutes at one a "+
			"minute)", len(chain))
	}
	if chain[0].Idx != "IDX-10" && chain[0].Idx != "IDX-11" {
		if chain[5].Idx != "IDX-15" {
			t.Errorf("the quoted message is not in the middle: %v", chainIdx(chain))
		}
	}

	// Twice the same message is one entry: the platform sends a message that
	// mentions the bot twice.
	before, err := cache.client.ZCard(ctx, cache.keys2(group, 0)).Result()
	if err != nil {
		t.Fatalf("ZCard: %v", err)
	}
	if before != 30 {
		t.Errorf("the cache holds %d messages, want 30", before)
	}
	if err := cache.Record(ctx, group, ordered[15]); err != nil {
		t.Fatalf("Record twice: %v", err)
	}
	after, err := cache.client.ZCard(ctx, cache.keys2(group, 0)).Result()
	if err != nil {
		t.Fatalf("ZCard: %v", err)
	}
	if after != before {
		t.Errorf("a second delivery added an entry: %d then %d", before, after)
	}
}

// keys2 exposes one of the two keys a group uses, for assertions.
func (c *Cache) keys2(groupOpenID string, which int) string {
	set, index := c.keys(groupOpenID)
	if which == 0 {
		return set
	}
	return index
}

// TestContextReportsWhatIsNotThere covers the paths where there is nothing to
// judge, which must be a miss rather than an empty judgement.
func TestContextReportsWhatIsNotThere(t *testing.T) {
	cache := testCache(t, 6*time.Hour)
	ctx := context.Background()
	group := "G-TestContextReportsWhatIsNotThere"

	if _, err := cache.Context(ctx, group, "NEVER-SEEN", 10, 10, time.Minute); err != ErrNotCached {
		t.Errorf("err = %v, want ErrNotCached", err)
	}
	if _, err := cache.Context(ctx, group, "", 10, 10, time.Minute); err != ErrNotCached {
		t.Errorf("err = %v for an empty index, want ErrNotCached", err)
	}
}

// TestPruningDropsWhatIsTooOld covers the retention: a message older than the
// window is gone, and so is its index entry, so the index cannot outlive it.
func TestPruningDropsWhatIsTooOld(t *testing.T) {
	cache := testCache(t, time.Hour)
	ctx := context.Background()
	group := "G-TestPruningDropsWhatIsTooOld"

	old := CachedMessage{ID: "OLD", Idx: "IDX-OLD", User: "MEMBER-1",
		TS: time.Now().Add(-3 * time.Hour).UnixMilli()}
	if err := cache.Record(ctx, group, old); err != nil {
		t.Fatalf("Record: %v", err)
	}
	fresh := CachedMessage{ID: "NEW", Idx: "IDX-NEW", User: "MEMBER-1",
		TS: time.Now().UnixMilli()}
	if err := cache.Record(ctx, group, fresh); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if messages, err := cache.client.ZCard(ctx, cache.keys2(group, 0)).Result(); err != nil || messages != 1 {
		t.Errorf("the set holds %d messages (err %v), want only the one inside the window",
			messages, err)
	}
	if _, err := cache.client.ZScore(ctx, cache.keys2(group, 1), "IDX-OLD").Result(); err == nil {
		t.Error("the index still points at a message that was pruned")
	}
	if _, err := cache.Context(ctx, group, "IDX-OLD", 10, 10, time.Minute); err != ErrNotCached {
		t.Errorf("err = %v for a pruned message, want ErrNotCached", err)
	}
}
