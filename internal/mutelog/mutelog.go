// Package mutelog remembers the mutes this bot applied itself.
//
// The platform reports a mute as a mute: the group's mute state says nothing
// about who applied it, and no event is delivered when somebody else applies one.
// A feature whose whole job is to lift the mutes other people apply therefore has
// to know the bot's own work from the outside's, and this is that record.
//
// It is the bot's own memory rather than a second read of the platform for the
// same reason a mute is known here at the moment it is applied: by the time
// somebody asks, the only question left is whether the mute is still in force.
//
// A record expires with the mute it describes. One that outlived its mute would
// make the next mute of the same member -- applied by somebody else -- look like
// the bot's own, and that is exactly the mute the caller exists to lift.
package mutelog

import (
	"sync"
	"time"
)

// Log is the record of mutes this bot applied.
//
// A nil *Log is usable and remembers nothing, which is what a feature built
// without a bot is handed. The alternative is a nil check at every call site, and
// the call sites are the ones applying mutes rather than the ones that care.
type Log struct {
	mu      sync.Mutex
	entries map[entry]time.Time
}

// entry is one member in one group: a mute is per group, because the same person
// is a different member of every group they are in.
type entry struct {
	groupOpenID  string
	memberOpenID string
}

// New returns an empty record.
func New() *Log { return &Log{entries: map[entry]time.Time{}} }

// Record remembers that this bot muted a member until the given moment.
//
// Called with the same moment the platform was given, so that the record and the
// mute end together. A member named by nothing is not recorded: there would be no
// way to recognise them again.
func (l *Log) Record(groupOpenID, memberOpenID string, until time.Time) {
	if l == nil || memberOpenID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = map[entry]time.Time{}
	}
	l.entries[entry{groupOpenID: groupOpenID, memberOpenID: memberOpenID}] = until
}

// Forget drops the record for one member, for a mute this bot lifted early.
//
// A mute that ran out is not forgotten by the caller: whether it still counts is
// decided by the clock in Applied, so a caller that has to lift a mute it did not
// apply has one question to ask rather than two.
func (l *Log) Forget(groupOpenID, memberOpenID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, entry{groupOpenID: groupOpenID, memberOpenID: memberOpenID})
}

// Applied reports whether this bot's own mute of the member is still in force.
//
// The entry is dropped as it is found to have run out, which is the only cleaning
// this record needs: nothing else reads it.
func (l *Log) Applied(groupOpenID, memberOpenID string, now time.Time) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := entry{groupOpenID: groupOpenID, memberOpenID: memberOpenID}
	until, found := l.entries[key]
	if !found {
		return false
	}
	if !until.After(now) {
		delete(l.entries, key)
		return false
	}
	return true
}
