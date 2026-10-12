package store

import (
	"context"
	"testing"
	"time"
)

// TestPutReplacesTheHoldOfOneMember covers what identifies a hold.
//
// A member is held once per group: the feature that holds them keys its own entries
// by (group, member) and replaces the earlier one when they are held again, which is
// what asking somebody to verify a second time produces -- a fresh token, because the
// button of the first prompt is still out there and must not be the one that answers.
//
// The store has to agree about the identity, and not merely about the token. A row
// left behind by a replaced hold is not harmless: a hold with no expiry of its own --
// which is what a pseudo-mute is -- is released by nothing, so Load hands it back for
// as long as the database keeps it, and every reader of that list treats the member
// as held. One of those readers is the feature that lifts the mutes other people
// apply, which then refuses to act for that member for good, and says nothing about
// having skipped them.
func TestPutReplacesTheHoldOfOneMember(t *testing.T) {
	ctx := context.Background()
	pending := openTestStore(t).Pending()

	// One member, held twice: the second hold is what a renewed verification writes.
	if err := pending.Put(ctx, Pending{
		Token: "TOKEN-FIRST", GroupOpenID: "GROUP-1", MemberOpenID: "MEMBER-1",
		Settings: "first",
	}); err != nil {
		t.Fatalf("recording the first hold: %v", err)
	}
	if err := pending.Put(ctx, Pending{
		Token: "TOKEN-SECOND", GroupOpenID: "GROUP-1", MemberOpenID: "MEMBER-1",
		Settings: "second",
	}); err != nil {
		t.Fatalf("recording the second hold: %v", err)
	}

	// Somebody else in the same group, and the same member in another group. Neither
	// is the first member's hold, so neither may be replaced by it: a mute is per
	// group, because the same person is a different member of every group they are in.
	if err := pending.Put(ctx, Pending{
		Token: "TOKEN-OTHER-MEMBER", GroupOpenID: "GROUP-1", MemberOpenID: "MEMBER-2",
	}); err != nil {
		t.Fatalf("recording another member's hold: %v", err)
	}
	if err := pending.Put(ctx, Pending{
		Token: "TOKEN-OTHER-GROUP", GroupOpenID: "GROUP-2", MemberOpenID: "MEMBER-1",
	}); err != nil {
		t.Fatalf("recording the same member's hold in another group: %v", err)
	}

	entries, err := pending.Load(ctx, time.Now())
	if err != nil {
		t.Fatalf("loading the holds in force: %v", err)
	}

	held := map[string]string{}
	for _, entry := range entries {
		key := entry.GroupOpenID + "/" + entry.MemberOpenID
		if previous, already := held[key]; already {
			t.Errorf("%s is held twice (%q and %q): the hold that was replaced is "+
				"still there, so a reader that treats the list as \"who is being "+
				"held\" counts one member twice and never sees them released",
				key, previous, entry.Token)
		}
		held[key] = entry.Token
	}

	want := map[string]string{
		"GROUP-1/MEMBER-1": "TOKEN-SECOND",
		"GROUP-1/MEMBER-2": "TOKEN-OTHER-MEMBER",
		"GROUP-2/MEMBER-1": "TOKEN-OTHER-GROUP",
	}
	if len(held) != len(want) {
		t.Errorf("load returned %d hold(s), want %d", len(held), len(want))
	}
	for key, token := range want {
		if held[key] != token {
			t.Errorf("%s holds %q, want %q", key, held[key], token)
		}
	}
}
