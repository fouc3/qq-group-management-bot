package mutelog

import (
	"testing"
	"time"
)

// TestAMuteIsKnownWhileItLasts covers the one question the record is asked.
func TestAMuteIsKnownWhileItLasts(t *testing.T) {
	now := time.Unix(1000, 0)
	log := New()
	log.Record("GROUP-1", "MEMBER-1", now.Add(10*time.Minute))

	if !log.Applied("GROUP-1", "MEMBER-1", now) {
		t.Error("a mute this bot applied is not known while it lasts")
	}
	if log.Applied("GROUP-1", "MEMBER-2", now) {
		t.Error("a mute was reported for a member who was never muted")
	}
	// The same person is a different member in every group, so a mute in one
	// group says nothing about another.
	if log.Applied("GROUP-2", "MEMBER-1", now) {
		t.Error("a mute in one group was reported for another group")
	}
}

// TestAMuteIsForgottenWhenItRunsOut covers the trap the expiry is for: a record
// that outlived its mute would make somebody else's next mute of the same member
// look like this bot's own, and that mute is the one the caller lifts.
func TestAMuteIsForgottenWhenItRunsOut(t *testing.T) {
	now := time.Unix(1000, 0)
	log := New()
	log.Record("GROUP-1", "MEMBER-1", now.Add(time.Minute))

	if log.Applied("GROUP-1", "MEMBER-1", now.Add(2*time.Minute)) {
		t.Error("a mute that had run out was still reported as in force")
	}
	// And the expiry is the mute's, not the record's: a mute whose moment has
	// arrived exactly is over.
	if log.Applied("GROUP-1", "MEMBER-1", now.Add(time.Minute)) {
		t.Error("a mute ending at this moment was still reported as in force")
	}
}

// TestALiftedMuteIsForgotten covers the other ending: the bot lifting its own
// mute before the clock says so.
func TestALiftedMuteIsForgotten(t *testing.T) {
	now := time.Unix(1000, 0)
	log := New()
	log.Record("GROUP-1", "MEMBER-1", now.Add(10*time.Minute))
	log.Forget("GROUP-1", "MEMBER-1")

	if log.Applied("GROUP-1", "MEMBER-1", now) {
		t.Error("a mute the bot lifted is still reported as in force")
	}
}

// TestNothingIsRecordedWithoutAMember covers the call that would record
// something unrecognisable: a member named by nothing cannot be recognised again,
// so writing the row down would only make the record longer.
func TestNothingIsRecordedWithoutAMember(t *testing.T) {
	now := time.Unix(1000, 0)
	log := New()
	log.Record("GROUP-1", "", now.Add(10*time.Minute))

	if log.Applied("GROUP-1", "", now) {
		t.Error("a mute of nobody was recorded")
	}
}

// TestAFeatureBuiltWithoutABotIsSafe covers the nil record: a feature built by a
// test is handed no bot, and applying a mute there must not panic.
func TestAFeatureBuiltWithoutABotIsSafe(t *testing.T) {
	var log *Log
	log.Record("GROUP-1", "MEMBER-1", time.Unix(2000, 0))
	log.Forget("GROUP-1", "MEMBER-1")

	if log.Applied("GROUP-1", "MEMBER-1", time.Unix(1000, 0)) {
		t.Error("a record that remembers nothing reported a mute")
	}
}
