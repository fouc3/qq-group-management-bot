package moderation

import (
	"context"
	"testing"
	"time"
)

// TestAPunishmentIsNotRecordedAsTheBotsOwn covers which mutes mutelog is for.
//
// The record is what stops the feature that lifts other people's mutes from letting a
// member out of their verification. A punishment this feature decided is the other thing:
// this group's own decision about one of its members, made through the list it keeps.
//
// It used to be written down, which made it the one mute whose meaning depended on when it
// was read -- protected while this process was running, liftable after a restart.
func TestAPunishmentIsNotRecordedAsTheBotsOwn(t *testing.T) {
	h := watchHarness(t, &modelStub{}, &platformStub{}, "")

	if err := h.muteWatched(context.Background(), watchGroup, watchMember,
		time.Minute); err != nil {
		t.Fatalf("muting: %v", err)
	}
	if h.deps.Mutes.Applied(watchGroup, watchMember, time.Now()) {
		t.Error("a punishment was written down as the bot's own, so muteguard leaves " +
			"it standing until a restart and lifts it afterwards")
	}
}
