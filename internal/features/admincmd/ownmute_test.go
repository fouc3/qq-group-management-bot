package admincmd

import (
	"testing"
	"time"
)

// TestACommandMuteIsNotRecordedAsTheBotsOwn covers which mutes mutelog is for.
//
// What that record protects is the holds the verification applies: a member who is
// verifying must not be let out by the feature that lifts everything else. A mute an
// administrator asked for by name is not one of those -- it is this group's own decision
// about one of its members, and muteguard lifting it is muteguard doing what the group
// asked of it.
//
// It used to be written down, which made it the one mute whose meaning depended on when it
// was read: protected while the process that applied it was still running, and liftable
// after a restart.
func TestACommandMuteIsNotRecordedAsTheBotsOwn(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("/禁言 30m", testAdmin, testGroupOpenID, testTarget)

	// The command has to have worked, or the absence of a record proves nothing.
	if h.muteCount() != 1 {
		t.Fatalf("mute calls = %d, want 1", h.muteCount())
	}
	if h.handler.deps.Mutes.Applied(testGroupOpenID, testTarget, time.Now()) {
		t.Error("a command mute was written down as the bot's own, so muteguard " +
			"leaves it standing until a restart and lifts it afterwards")
	}
}
