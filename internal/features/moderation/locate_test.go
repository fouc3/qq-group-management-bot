package moderation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/config"
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

// TestAQuoteIsLocatedByItsText covers the way in that a temporary index does not
// have.
//
// A quote of a message that is itself a quote comes with an index the cache can never
// hold, so the text the quote showed is what has to find the message. The rules that
// matter are here: one match is a match, and anything else is a miss.
func TestAQuoteIsLocatedByItsText(t *testing.T) {
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

	found, err := cache.Locate(ctx, group, rendered, 30*time.Minute)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if found.Idx != "IDX-1" {
		t.Errorf("located %s, want the message that said it", found.Idx)
	}

	// A short line is not evidence: everybody says "在的", and matching on one would
	// point at the wrong message.
	if _, err := cache.Locate(ctx, group, "[消息内容] 在的", 30*time.Minute); err == nil {
		t.Error("a short line must not be enough to point at a message")
	}

	// Two messages saying the same thing is not a reason to pick either: the wrong
	// pick silences the wrong member.
	if err := cache.Record(ctx, group, CachedMessage{ID: "M-3", Idx: "IDX-3",
		User: "MEMBER-2", TS: now - 10_000, Text: "成果怎么样？我也想问"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := cache.Locate(ctx, group, rendered, 30*time.Minute); err == nil {
		t.Error("two matches must be a miss, not a coin toss")
	}

	// And a quote of something nobody said stays nothing.
	if _, err := cache.Locate(ctx, group, "[消息内容] 谁也没说过这一句",
		30*time.Minute); !errors.Is(err, ErrNotCached) {
		t.Errorf("err = %v, want ErrNotCached", err)
	}
}
