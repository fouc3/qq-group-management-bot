// Package logring keeps the newest log records in memory, where the console can
// reach them.
//
// The bot logs to stderr and the service manager takes it from there, which is
// the right place for a log and the wrong one for a web page: reading it back
// would mean running journalctl, and a page in a browser cannot do that. So every
// record is also kept here -- in a fixed-size buffer that holds the newest few
// hundred and lets the oldest fall off the end.
//
// This is a copy of the log, never the log itself. A buffer that fills drops a
// record from the copy and counts it; stderr still has every line. Nothing in
// this package may block or fail, because it runs inside every log call the bot
// makes: a log call that could stall is worse than a log line nobody kept.
package logring

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Attr is one key/value pair of a captured record, already rendered to text.
//
// Rendered on the way in rather than kept as a slog.Value: the buffer holds
// copies, and a value that points at memory the caller still owns would be a
// record that changes after it was logged.
type Attr struct {
	Key   string
	Value string
}

// Record is one captured log line.
type Record struct {
	// Seq counts from 1 and never repeats, which is what lets a reader that
	// reconnects ask for "everything after the last one I saw".
	Seq     uint64
	Time    time.Time
	Level   slog.Level
	Message string
	Attrs   []Attr
}

// defaultSize is how many records are kept when none is asked for.
const defaultSize = 512

// Ring is the buffer. It is safe for concurrent use, which it has to be: the
// bot logs from every goroutine it runs.
type Ring struct {
	mu      sync.Mutex
	records []Record
	// start is the index of the oldest record in records.
	start int
	count int
	// seq is the sequence number of the newest record.
	seq uint64
	// dropped counts the records pushed out of the buffer because it was full.
	dropped uint64
	subs    map[int]*Subscription
	nextSub int
}

// New builds a buffer that holds size records.
func New(size int) *Ring {
	if size <= 0 {
		size = defaultSize
	}
	return &Ring{records: make([]Record, size), subs: make(map[int]*Subscription)}
}

// Capacity is how many records the buffer holds.
func (r *Ring) Capacity() int {
	if r == nil {
		return 0
	}
	return len(r.records)
}

// Kept is how many records are in the buffer now.
func (r *Ring) Kept() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// Dropped is how many records have fallen out of the buffer since the process
// started. It is reported rather than hidden: a reader who cannot see that the
// buffer wrapped would take a gap for quiet.
func (r *Ring) Dropped() uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// Newest is the sequence number of the newest record, or 0 for an empty buffer.
func (r *Ring) Newest() uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// Tail returns up to limit of the newest records at or above minLevel, oldest
// first. An empty result is an empty slice, so a JSON answer is [] rather than
// null.
func (r *Ring) Tail(limit int, minLevel slog.Level) []Record {
	if r == nil || limit <= 0 {
		return []Record{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Record, 0, min(limit, r.count))
	for offset := 0; offset < r.count && len(out) < limit; offset++ {
		record := r.records[(r.start+offset)%len(r.records)]
		if record.Level >= minLevel {
			out = append(out, cloneRecord(record))
		}
	}
	return out
}

// add stores a record and hands it to every subscriber that wants it.
//
// Called with the lock taken by the writer path; see Handler.Handle.
func (r *Ring) add(record Record) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seq++
	record.Seq = r.seq
	record.Attrs = cloneAttrs(record.Attrs)

	if r.count == len(r.records) {
		// Full: the oldest record is overwritten, which is the whole point of a
		// fixed-size buffer.
		r.records[r.start] = record
		r.start = (r.start + 1) % len(r.records)
		r.dropped++
	} else {
		r.records[(r.start+r.count)%len(r.records)] = record
		r.count++
	}

	for _, sub := range r.subs {
		sub.deliver(record)
	}
}

// cloneRecord copies a record so a caller cannot hold the buffer's memory.
func cloneRecord(record Record) Record {
	record.Attrs = cloneAttrs(record.Attrs)
	return record
}

func cloneAttrs(attrs []Attr) []Attr {
	if attrs == nil {
		return []Attr{}
	}
	out := make([]Attr, len(attrs))
	copy(out, attrs)
	return out
}

// Subscription is a live reader. Records arrive on C until Close is called.
//
// It is a channel with a small buffer rather than a blocking send, and a full
// channel drops records instead of waiting: the writer is the bot's own logging,
// and a subscriber that stopped reading must never be able to slow the bot down.
type Subscription struct {
	ring  *Ring
	id    int
	level slog.Level
	C     chan Record
	// lost counts what was dropped for this reader only.
	lost atomic.Uint64
	once sync.Once
}

// Lost is how many records this reader was too slow to take.
func (s *Subscription) Lost() uint64 {
	return s.lost.Load()
}

// deliver hands one record over if it is new enough and there is room.
func (s *Subscription) deliver(record Record) {
	if record.Level < s.level {
		return
	}
	select {
	case s.C <- record:
	default:
		s.lost.Add(1)
	}
}

// Subscribe starts a live reader that receives records at or above level.
func (r *Ring) Subscribe(level slog.Level) *Subscription {
	if r == nil {
		// A deployment with no buffer still gets a subscription, just one that
		// never receives anything, so the caller needs no special case.
		return &Subscription{C: make(chan Record)}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sub := &Subscription{ring: r, id: r.nextSub, level: level, C: make(chan Record, subscriberBuffer)}
	r.nextSub++
	r.subs[sub.id] = sub
	return sub
}

// subscriberBuffer is how far behind a live reader may fall before it starts
// losing records. It is deeper than a screenful so that a slow repaint is not
// a lost line.
const subscriberBuffer = 256

// Close ends the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	s.once.Do(func() {
		if s.ring == nil {
			close(s.C)
			return
		}
		s.ring.mu.Lock()
		delete(s.ring.subs, s.id)
		s.ring.mu.Unlock()
		close(s.C)
	})
}

// Handler wraps another handler and keeps a copy of everything it sees.
//
// Wrapped around the handler that writes to stderr rather than instead of it: the
// buffer is bounded and will forget, and a log that only exists in a 512-line
// buffer would be a log that eats its own history.
type Handler struct {
	next   slog.Handler
	ring   *Ring
	attrs  []Attr
	groups []string
}

// Wrap returns next with a copy of every record going into the buffer as well.
//
// A nil buffer returns next unchanged, so a caller that has no console keeps the
// plain handler.
func (r *Ring) Wrap(next slog.Handler) slog.Handler {
	if r == nil {
		return next
	}
	return &Handler{next: next, ring: r}
}

// Enabled asks the wrapped handler. The buffer copies what the log is willing to
// print and nothing more, which is what keeps "why is DEBUG missing" a question
// about log.level rather than about this package.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle copies the record and passes it on.
func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	h.ring.add(h.capture(record))
	return h.next.Handle(ctx, record)
}

// WithAttrs keeps the attributes for the copy as well as passing them on.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := h.clone()
	next.next = h.next.WithAttrs(attrs)
	prefix := strings.Join(h.groups, ".")
	for _, attr := range attrs {
		appendAttr(&next.attrs, prefix, attr)
	}
	return next
}

// WithGroup opens a group for the copy, so a component's fields stay labelled
// the way the wrapped handler labels them.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := h.clone()
	next.next = h.next.WithGroup(name)
	next.groups = append(next.groups, name)
	return next
}

// clone copies the handler and its captured attributes outright.
//
// The copy is not optional: two handlers derived from the same parent share the
// parent's backing array, and appending to both would have one overwrite the
// other's attributes.
func (h *Handler) clone() *Handler {
	next := &Handler{
		next:   h.next,
		ring:   h.ring,
		attrs:  make([]Attr, len(h.attrs), len(h.attrs)+4),
		groups: make([]string, len(h.groups), len(h.groups)+1),
	}
	copy(next.attrs, h.attrs)
	copy(next.groups, h.groups)
	return next
}

// capture renders a record into the form the console shows.
func (h *Handler) capture(record slog.Record) Record {
	entry := Record{
		Time:    record.Time,
		Level:   record.Level,
		Message: record.Message,
		Attrs:   make([]Attr, 0, len(h.attrs)+record.NumAttrs()),
	}
	entry.Attrs = append(entry.Attrs, h.attrs...)
	prefix := strings.Join(h.groups, ".")
	record.Attrs(func(attr slog.Attr) bool {
		appendAttr(&entry.Attrs, prefix, attr)
		return true
	})
	if record.Time.IsZero() {
		entry.Time = time.Now()
	}
	return entry
}

// appendAttr flattens one attribute into a key and a string, opening groups into
// dotted keys: a console shows a line, not a tree.
func appendAttr(dst *[]Attr, prefix string, attr slog.Attr) {
	if attr.Equal(slog.Attr{}) {
		return
	}
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		inner := join(prefix, attr.Key)
		for _, child := range value.Group() {
			appendAttr(dst, inner, child)
		}
		return
	}
	*dst = append(*dst, Attr{Key: join(prefix, attr.Key), Value: valueString(value)})
}

// join qualifies a key with its group path, skipping either side when empty.
func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	if key == "" {
		return prefix
	}
	return prefix + "." + key
}

// valueString renders a value the way the text handler would.
func valueString(value slog.Value) string {
	if value.Kind() == slog.KindString {
		return value.String()
	}
	return fmt.Sprint(value.Any())
}

// LevelName is the level as the console shows it: DEBUG, INFO, WARN, ERROR.
func LevelName(level slog.Level) string {
	return level.String()
}
