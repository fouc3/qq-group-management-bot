package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fouc3/qq-group-management-bot/internal/config"
)

// Cache keeps recent group messages in Redis, so that a reported message can be
// judged next to what was said around it.
//
// It is a cache and is treated as one: every operation may fail, nothing here is
// a source of truth, and losing all of it costs context rather than data. A
// report that arrives while it is down is judged without context rather than
// refused.
type Cache struct {
	client    *redis.Client
	prefix    string
	retention time.Duration
	log       *slog.Logger

	mu sync.Mutex
	// downUntil holds off the next attempt after a failure, so that a dead
	// server is not dialled once per message in a busy group. It is a pause, not
	// a state machine, and any success ends it.
	downUntil time.Time
}

// ErrNotCached reports that nothing is known about the message asked for.
var ErrNotCached = errors.New("moderation: the message is not in the cache")

// ErrUnavailable reports that the cache could not be reached.
var ErrUnavailable = errors.New("moderation: the message cache is unavailable")

// NewCache builds the cache from the configuration section.
func NewCache(settings config.Redis, retention time.Duration, log *slog.Logger) *Cache {
	timeout := time.Duration(settings.DialTimeoutSeconds) * time.Second
	return &Cache{
		client: redis.NewClient(&redis.Options{
			Addr:         settings.Addr,
			Password:     settings.Password,
			DB:           settings.DB,
			DialTimeout:  timeout,
			ReadTimeout:  timeout,
			WriteTimeout: timeout,
		}),
		prefix:    settings.Prefix,
		retention: retention,
		log:       log,
	}
}

// Ping reports whether the cache server answers, which is how startup tells
// whether the cache is usable at all.
func (c *Cache) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := c.client.Ping(ctx).Err(); err != nil {
		c.failed(err)
		return err
	}
	c.succeeded()
	return nil
}

// Close releases the connection.
func (c *Cache) Close() error { return c.client.Close() }

// keys are the two keys one group's messages live in.
//
// Two, not one: the set holds the messages in time order, and the index maps a
// message index to its time. Without the index, finding the message a quote
// points at would mean reading the whole set and looking for it.
func (c *Cache) keys(groupOpenID string) (set, index string) {
	return c.prefix + ":msg:" + groupOpenID + ":z", c.prefix + ":msg:" + groupOpenID + ":i"
}

// paused reports whether the cache is inside its back-off window.
func (c *Cache) paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.downUntil)
}

// succeeded ends the back-off.
func (c *Cache) succeeded() {
	c.mu.Lock()
	c.downUntil = time.Time{}
	c.mu.Unlock()
}

// failed starts the back-off, saying so once per outage rather than once per
// message.
func (c *Cache) failed(err error) {
	const backoff = 30 * time.Second
	now := time.Now()
	c.mu.Lock()
	alreadyDown := now.Before(c.downUntil)
	c.downUntil = now.Add(backoff)
	c.mu.Unlock()
	if !alreadyDown && c.log != nil {
		c.log.Warn("the message cache is not answering, so reported messages are "+
			"judged without context until it does", "error", err)
	}
}

// Record writes one message, replacing any earlier copy of the same one.
//
// Replacing is what makes the two deliveries of one message harmless: the
// platform sends a message that mentions the bot as both a mention event and an
// ordinary one, and both carry the same payload, so the second write is the same
// write.
func (c *Cache) Record(ctx context.Context, groupOpenID string, message CachedMessage) error {
	if groupOpenID == "" || message.ID == "" {
		return nil
	}
	if c.paused() {
		return ErrUnavailable
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encoding a cached message: %w", err)
	}
	member := string(payload)
	set, index := c.keys(groupOpenID)
	cutoff := strconv.FormatInt(time.Now().Add(-c.retention).UnixMilli(), 10)

	pipe := c.client.TxPipeline()
	pipe.ZAdd(ctx, set, redis.Z{Score: float64(message.TS), Member: member})
	if message.Idx != "" {
		// The index entry carries the same score as the message, so one pruning
		// call trims both and an index entry can never outlive what it points at.
		pipe.ZAdd(ctx, index, redis.Z{Score: float64(message.TS), Member: message.Idx})
	}
	pipe.ZRemRangeByScore(ctx, set, "-inf", cutoff)
	pipe.ZRemRangeByScore(ctx, index, "-inf", cutoff)
	// A safety net under the pruning, for a group that goes quiet: the keys
	// should not outlive their usefulness even when nothing writes again.
	pipe.Expire(ctx, set, 2*c.retention)
	pipe.Expire(ctx, index, 2*c.retention)

	if _, err := pipe.Exec(ctx); err != nil {
		c.failed(err)
		return fmt.Errorf("caching a message: %w", err)
	}
	c.succeeded()
	return nil
}

// Context returns the messages to judge, built around the quoted one.
func (c *Cache) Context(ctx context.Context, groupOpenID, quotedIdx string,
	before, after int, span time.Duration) ([]CachedMessage, error) {
	if groupOpenID == "" || quotedIdx == "" {
		return nil, ErrNotCached
	}
	if c.paused() {
		return nil, ErrUnavailable
	}
	set, index := c.keys(groupOpenID)

	score, err := c.client.ZScore(ctx, index, quotedIdx).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, ErrNotCached
	case err != nil:
		c.failed(err)
		return nil, fmt.Errorf("looking up a quoted message: %w", err)
	}

	// The score is the millisecond, and more than one message can share it, so
	// the member whose own index matches is the one being asked about.
	when := strconv.FormatFloat(score, 'f', -1, 64)
	sharing, err := c.client.ZRangeByScore(ctx, set, &redis.ZRangeBy{
		Min: when,
		Max: when,
	}).Result()
	if err != nil {
		c.failed(err)
		return nil, fmt.Errorf("reading a quoted message: %w", err)
	}
	member := ""
	for _, candidate := range sharing {
		var decoded CachedMessage
		if err := json.Unmarshal([]byte(candidate), &decoded); err != nil {
			continue
		}
		if decoded.Idx == quotedIdx {
			member = candidate
			break
		}
	}
	if member == "" {
		return nil, ErrNotCached
	}

	rank, err := c.client.ZRank(ctx, set, member).Result()
	if err != nil {
		c.failed(err)
		return nil, fmt.Errorf("locating a quoted message: %w", err)
	}
	total, err := c.client.ZCard(ctx, set).Result()
	if err != nil {
		c.failed(err)
		return nil, fmt.Errorf("counting cached messages: %w", err)
	}

	// The widest window the limits allow is read in one call; the span limit is
	// then applied to what was selected, which is where the ends are known.
	low := rank - int64(before)
	if low < 0 {
		low = 0
	}
	high := rank + int64(after)
	if high > total-1 {
		high = total - 1
	}
	raws, err := c.client.ZRange(ctx, set, low, high).Result()
	if err != nil {
		c.failed(err)
		return nil, fmt.Errorf("reading a window of messages: %w", err)
	}

	window := make([]CachedMessage, 0, len(raws))
	position := -1
	for _, raw := range raws {
		var decoded CachedMessage
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			continue
		}
		if decoded.Idx == quotedIdx {
			position = len(window)
		}
		window = append(window, decoded)
	}
	if position < 0 {
		return nil, ErrNotCached
	}

	c.succeeded()
	return expandChain(window, position, before, after, span), nil
}
