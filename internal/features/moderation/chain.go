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
	// Punished records that this message has been taken back, and it is set only
	// once it really has been.
	//
	// A message that stays in the group after a failed recall is deliberately
	// left unmarked: it is still there for anybody to read and report, so it is
	// still something to judge. A message that was withdrawn is the opposite --
	// nobody can read it again, so judging it again would be punishing somebody
	// for words the group can no longer see, which is what happened when one
	// advertisement was withdrawn twice and its author silenced three times.
	Punished bool `json:"punished,omitempty"`
}

// SentAt is when the message was sent.
func (m CachedMessage) SentAt() time.Time { return time.UnixMilli(m.TS) }

// expandChain picks the messages to judge, starting from the quoted one.
//
// ordered must be sorted by time and rank must be the quoted message's position
// in it. Only the reported member's own messages are collected: the window is
// about one person's behaviour, and sending everybody's messages put other
// members' advertisements in front of the judge where nothing could be done about
// them, while putting bystanders' words into a judgement that was never about
// them. One person in, one person out.
//
// Messages that have already been taken back are left out, which is the whole
// point of the mark they carry: they were judged once, the group cannot read them
// any more, and putting them in front of the judge again can only lead to the
// same message being withdrawn a second time and its author silenced again. The
// quoted message itself is not filtered here -- a report about a message that is
// already gone is refused before this runs, with an answer of its own.
//
// The chain grows outward from the quoted message, skipping messages by other
// people rather than stopping at them. Each side stops when it has collected
// before (or after) of that member's messages, when the timeline runs out, or
// when going further would break the span rule below.
//
// The span is measured between the ends of what has been collected, so a long
// quiet gap stops the side it is on without shortening the other. It also bounds
// the walk: a message further than span from the quoted one could never be added
// to a chain containing it -- the two ends alone would be too far apart -- so the
// scan stops there instead of reading the whole history.
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

	author := ordered[rank].User
	anchor := ordered[rank].SentAt()

	low, high := rank, rank
	left, right := 0, 0
	leftDone, rightDone := before == 0, after == 0
	firstTS, lastTS := anchor, anchor

	// take reports whether one more of this member's messages still leaves the
	// chain's two ends within the span, and records it when it does.
	take := func(message CachedMessage) bool {
		candidateFirst, candidateLast := firstTS, lastTS
		if at := message.SentAt(); at.Before(candidateFirst) {
			candidateFirst = at
		} else if at.After(candidateLast) {
			candidateLast = at
		}
		if candidateLast.Sub(candidateFirst) > span {
			return false
		}
		firstTS, lastTS = candidateFirst, candidateLast
		return true
	}

	for {
		moved := false
		if !leftDone {
			switch next := low - 1; {
			case next < 0, anchor.Sub(ordered[next].SentAt()) > span:
				leftDone = true
			case ordered[next].User != author:
				// Somebody else's message: stepped over, never included, and the
				// walk continues past it.
				low, moved = next, true
			case take(ordered[next]):
				low, moved, left = next, true, left+1
				if left >= before {
					leftDone = true
				}
			default:
				// Taking it would break the span, and every message further out
				// would break it too. It stays outside the range, which matters:
				// the range is collected from at the end.
				leftDone = true
			}
		}
		if !rightDone {
			switch next := high + 1; {
			case next >= len(ordered),
				ordered[next].SentAt().Sub(anchor) > span:
				rightDone = true
			case ordered[next].User != author:
				high, moved = next, true
			case take(ordered[next]):
				high, moved, right = next, true, right+1
				if right >= after {
					rightDone = true
				}
			default:
				rightDone = true
			}
		}
		if !moved {
			break
		}
	}

	chain := make([]CachedMessage, 0, high-low+1)
	for _, message := range ordered[low : high+1] {
		if message.User == author && !message.Punished {
			chain = append(chain, message)
		}
	}
	return chain
}
