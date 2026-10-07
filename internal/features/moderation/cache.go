package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
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

// quoteCandidates pulls the readable lines out of a quote's rendering.
//
// The platform hands a quoted message over as a small report about itself -- "===
// 消息 1 ===", "[消息内容] …", a nested "--- 第1条 ---" -- and all of that is
// scaffolding. What is left is what the message actually said, and that is the only
// thing worth matching against the cache.
func quoteCandidates(text string) []string {
	var candidates []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if marker := "[消息内容]"; strings.Contains(line, marker) {
			line = strings.TrimSpace(line[strings.Index(line, marker)+len(marker):])
		} else if strings.HasPrefix(line, "===") || strings.HasPrefix(line, "---") ||
			strings.HasPrefix(line, "[") {
			// A heading, a type line, an attachment line: none of it is something a
			// member wrote, and matching on it would match almost everything.
			continue
		}
		// Short lines are not evidence of anything: every group has people saying "?"
		// and "好", and matching one of those would point at the wrong message.
		if utf8.RuneCountInString(line) >= 4 {
			candidates = append(candidates, line)
		}
	}
	return candidates
}

// Locate finds the message a quote points at when its index cannot be used.
//
// A quote of a message that is itself a quote carries a temporary index, which no
// ordinary message event ever has, so the cache cannot be asked about it directly.
// The quoted text can be asked about instead: what the quote shows is what some
// cached message said, and within a few minutes of a report there is normally
// exactly one message of the named sender that says it.
//
// author is required, and it is the whole point of the search. Finding a message by
// its text alone is not finding it: two members can say the same thing, and the
// platform names the sender only sometimes. Without a name to check the text
// against, a match would be a guess about *whose* message it is -- and a guess here
// is what a punishment is then applied to. An empty author therefore locates
// nothing, and says so.
//
// Exactly one message *of that author*, or nothing. Two messages of theirs saying
// the same thing is not a reason to pick either -- the wrong pick would silence the
// wrong message -- so that is reported as a miss, and the caller is expected to
// treat it as no judgement rather than as a guess.
func (c *Cache) Locate(ctx context.Context, groupOpenID, quotedText, author string,
	within time.Duration) (CachedMessage, error) {
	candidates := quoteCandidates(quotedText)
	if groupOpenID == "" || len(candidates) == 0 {
		return CachedMessage{}, ErrNotCached
	}
	if strings.TrimSpace(author) == "" {
		// Said as its own refusal rather than as an empty search: the caller has to
		// tell the group something, and "the platform named no sender, so a text
		// match would not say whose message it is" is what actually happened.
		return CachedMessage{}, fmt.Errorf("%w: the platform named no sender, so a "+
			"text match would not say whose message it is", ErrNotCached)
	}
	if c.paused() {
		return CachedMessage{}, ErrUnavailable
	}
	set, _ := c.keys(groupOpenID)

	raws, err := c.client.ZRangeByScore(ctx, set, &redis.ZRangeBy{
		Min: strconv.FormatInt(time.Now().Add(-within).UnixMilli(), 10),
		Max: "+inf",
	}).Result()
	if err != nil {
		c.failed(err)
		return CachedMessage{}, fmt.Errorf("searching the cache for a quoted message: %w", err)
	}

	var found []CachedMessage
	for _, raw := range raws {
		var decoded CachedMessage
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			continue
		}
		// The name first, because it is what makes the text mean anything: a
		// message somebody else sent is not the message that was quoted, however
		// identical it reads.
		if decoded.User != author {
			continue
		}
		if decoded.Text == "" {
			continue
		}
		for _, candidate := range candidates {
			if strings.Contains(decoded.Text, candidate) {
				found = append(found, decoded)
				break
			}
		}
	}
	switch len(found) {
	case 1:
		c.succeeded()
		return found[0], nil
	case 0:
		return CachedMessage{}, ErrNotCached
	default:
		return CachedMessage{}, fmt.Errorf("%w: %d cached messages of that member match "+
			"the quoted text", ErrNotCached, len(found))
	}
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
	// The reported message itself having been taken back is the one case this
	// cannot answer with a window at all. It is refused rather than judged
	// without it, and refused with its own error, so that the group is told the
	// report was already dealt with instead of being told the judgement failed.
	if window[position].Punished {
		return nil, feature.ErrAlreadyPunished
	}

	c.succeeded()
	return expandChain(window, position, before, after, span), nil
}

// MarkPunished records that messages have been taken back.
//
// The mark goes into the cached message itself rather than into a second key,
// because that is what makes it impossible to read a message without also
// seeing that it is gone: every path that decodes a window gets the answer for
// free, and no path can forget to ask.
//
// A message that is not in the cache any more is not an error. It was cached,
// it was withdrawn, and then it aged out, which is the ordinary ending of
// everything here; there is nothing left to mark and nothing to report.
func (c *Cache) MarkPunished(ctx context.Context, groupOpenID string, indexes []string) error {
	if groupOpenID == "" || len(indexes) == 0 {
		return nil
	}
	if c.paused() {
		return ErrUnavailable
	}
	set, index := c.keys(groupOpenID)

	marked := 0
	for _, idx := range indexes {
		if strings.TrimSpace(idx) == "" {
			continue
		}
		// The score is what finds the message again: the index entry carries the
		// same score as the message it points at, which is the invariant the
		// whole cache is built on.
		score, err := c.client.ZScore(ctx, index, idx).Result()
		switch {
		case errors.Is(err, redis.Nil):
			continue
		case err != nil:
			c.failed(err)
			return fmt.Errorf("finding a message to mark: %w", err)
		}
		when := strconv.FormatFloat(score, 'f', -1, 64)
		sharing, err := c.client.ZRangeByScore(ctx, set, &redis.ZRangeBy{
			Min: when,
			Max: when,
		}).Result()
		if err != nil {
			c.failed(err)
			return fmt.Errorf("reading a message to mark: %w", err)
		}
		for _, raw := range sharing {
			var decoded CachedMessage
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				continue
			}
			if decoded.Idx != idx || decoded.Punished {
				continue
			}
			decoded.Punished = true
			replacement, err := json.Marshal(decoded)
			if err != nil {
				continue
			}
			// Removed and added in one transaction: between the two the message
			// would be missing from a window that is being read right now, and a
			// window with a hole in it is worse than a stale one.
			pipe := c.client.TxPipeline()
			pipe.ZRem(ctx, set, raw)
			pipe.ZAdd(ctx, set, redis.Z{Score: score, Member: string(replacement)})
			if _, err := pipe.Exec(ctx); err != nil {
				c.failed(err)
				return fmt.Errorf("marking a message as taken back: %w", err)
			}
			marked++
			break
		}
	}
	c.succeeded()
	c.log.Debug("messages were marked as taken back",
		"group", groupOpenID, "asked", len(indexes), "marked", marked)
	return nil
}
