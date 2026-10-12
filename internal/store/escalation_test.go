package store

import (
	"context"
	"testing"
	"time"
)

// TestAHoldRemembersThatItWasEscalated covers the record an escalation leaves.
//
// A member who goes on talking under a pseudo-mute is one the group silences for real,
// and that decision has to outlive the process that made it for the same reason the hold
// does: a restart that forgot it would leave the member held the soft way again, on the
// one path where the bot is what stops them talking.
func TestAHoldRemembersThatItWasEscalated(t *testing.T) {
	ctx := context.Background()
	pending := openTestStore(t).Pending()

	if err := pending.Put(ctx, Pending{
		Token: "TOKEN-1", GroupOpenID: "GROUP-1", MemberOpenID: "MEMBER-1",
		HeldUntil: 1000, EscalatedAt: 500, Settings: "settings",
	}); err != nil {
		t.Fatalf("recording an escalated hold: %v", err)
	}

	entries, err := pending.Load(ctx, time.Unix(999, 0))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("holds = %d, want the one just recorded", len(entries))
	}
	if entries[0].EscalatedAt != 500 {
		t.Errorf("the escalation came back as %d, want the moment it was recorded",
			entries[0].EscalatedAt)
	}
}

// TestAnOlderDatabaseGainsTheEscalationColumn covers the ninth migration.
//
// The column is added to the table rather than the table rebuilt, and what matters about
// that is the rows already in it: a hold written by the build before this one has to open
// with every field it had and read as never escalated. Zero is the whole of what makes
// that true, and a rebuild that lost the rows would take every member's hold with it.
func TestAnOlderDatabaseGainsTheEscalationColumn(t *testing.T) {
	opened := openTestStoreAt(t, 8, `
INSERT INTO pending_verifications
    (token, group_openid, member_openid, joined_at, deadline, held_until, reported, settings, created_at)
VALUES ('TOKEN-OLD', 'GROUP-1', 'MEMBER-1', 11, 22, 0, 0, 'settings', 33)`)

	// The deadline is behind us, but a hold with no expiry of its own -- which is what
	// held_until zero means -- is released by nothing, so it is still there to be read.
	entries, err := opened.Pending().Load(context.Background(), time.Unix(99, 0))
	if err != nil {
		t.Fatalf("loading from a database that was already there: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("holds = %d, want the one the older build wrote", len(entries))
	}
	entry := entries[0]
	if entry.Token != "TOKEN-OLD" || entry.GroupOpenID != "GROUP-1" ||
		entry.MemberOpenID != "MEMBER-1" || entry.JoinedAt != 11 || entry.Deadline != 22 ||
		entry.Settings != "settings" {
		t.Errorf("a hold that was already there came back changed: %+v", entry)
	}
	if entry.EscalatedAt != 0 {
		t.Errorf("a hold written before the column existed reads as escalated at %d",
			entry.EscalatedAt)
	}
}
