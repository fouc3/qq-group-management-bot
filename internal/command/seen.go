package command

import (
	"sync"
	"time"
)

// seenTTL is how long a handled message is remembered.
//
// A minute, because that is longer than any redelivery: the same message is
// never delivered twice after that.
const seenTTL = time.Minute

// Seen remembers the messages that have been dealt with, so that one message is
// acted on once.
//
// It is needed because the platform can report the same message twice. A group
// that receives every message reports one that mentions the bot both as a
// mention event and as an ordinary one, and which of the two arrives first is
// not something the bot is told. Acting twice would mute twice and answer twice.
//
// The caller decides when to ask. That is deliberate: a message that is dropped
// before it is ever looked at must not be recorded as handled, or the delivery
// that would have been answered would be dropped as a duplicate of the one that
// was not.
type Seen struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewSeen returns an empty record.
func NewSeen() *Seen {
	return &Seen{seen: map[string]time.Time{}}
}

// First reports whether this is the first sighting of a message.
//
// A message with no id is always the first sighting: there is nothing to
// recognise it by, and answering it twice is better than dropping the only
// delivery there is.
func (s *Seen) First(messageID string) bool {
	if messageID == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for known, at := range s.seen {
		if now.Sub(at) > seenTTL {
			delete(s.seen, known)
		}
	}
	if _, already := s.seen[messageID]; already {
		return false
	}
	s.seen[messageID] = now
	return true
}
