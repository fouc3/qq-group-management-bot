package moderation

import "time"

// CachedMessage is one group message as the cache holds it.
//
// The JSON field names are short because one of these is written for every
// message in every managed group; they are the on-the-wire shape in Redis, so
// renaming one is a cache migration rather than a rename.
type CachedMessage struct {
	// ID is the platform message id, which is what a recall needs.
	ID string `json:"id"`
	// Idx is the platform message index, which is what a quote points at.
	Idx string `json:"idx,omitempty"`
	// User is the sender's member openid.
	User string `json:"uid"`
	// Name is the sender's nickname, for the judge to read.
	Name string `json:"name,omitempty"`
	// Type is the platform message type.
	Type int `json:"type,omitempty"`
	// TS is the send time in Unix milliseconds.
	//
	// Milliseconds rather than seconds: two messages in the same second would
	// otherwise have the same score, and their order would fall back to sorting
	// the payloads as text -- which is exactly the order the window is built on.
	TS int64 `json:"ts"`
	// Text is the message body.
	Text string `json:"text,omitempty"`
	// Atts are short descriptions of the attachments, which is all the judge
	// needs and all that is worth keeping.
	Atts []string `json:"att,omitempty"`
}

// SentAt is when the message was sent.
func (m CachedMessage) SentAt() time.Time { return time.UnixMilli(m.TS) }

// expandChain picks the messages to judge, starting from the quoted one.
//
// ordered must be sorted by time and rank must be the quoted message's position
// in it. The chain grows outward one step at a time, and each side stops when
// either of its two limits is reached: at most before messages to the left, at
// most after to the right, and never a chain whose two ends are further apart
// than span.
//
// The span is measured between the ends of what has been selected so far. A long
// quiet gap therefore stops the side it is on without shortening the other,
// which is what "the chain's two ends" means.
//
// The quoted message is always included when it exists, even if the limits would
// otherwise exclude it: a report about that message is about that message.
func expandChain(ordered []CachedMessage, rank, before, after int,
	span time.Duration) []CachedMessage {
	if rank < 0 || rank >= len(ordered) {
		return nil
	}
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}

	low, high := rank, rank
	// canAdd reports whether taking one more message keeps both ends within the
	// span. The count limits are checked by the caller, per side.
	canAdd := func(next int) bool {
		if next < 0 || next >= len(ordered) {
			return false
		}
		newLow, newHigh := low, high
		if next < newLow {
			newLow = next
		}
		if next > newHigh {
			newHigh = next
		}
		return ordered[newHigh].SentAt().Sub(ordered[newLow].SentAt()) <= span
	}

	for {
		grew := false
		if left := rank - low; left < before {
			if next := low - 1; canAdd(next) {
				low = next
				grew = true
			}
		}
		if right := high - rank; right < after {
			if next := high + 1; canAdd(next) {
				high = next
				grew = true
			}
		}
		if !grew {
			break
		}
	}

	chain := make([]CachedMessage, 0, high-low+1)
	chain = append(chain, ordered[low:high+1]...)
	return chain
}
