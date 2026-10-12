package store

import (
	"context"
	"testing"
	"time"
)

// TestABarIsTheGroupsThatMadeIt covers what the group column is for.
//
// A bar was keyed on the member alone, which made one group's decision binding on every
// group the bot is in -- while the command that writes one is authorised per group, so the
// two did not even agree with each other.
func TestABarIsTheGroupsThatMadeIt(t *testing.T) {
	ctx, now := blacklistTime(t)
	opened := openTestStore(t)

	if err := opened.Blacklist().Add(ctx, Barred{
		ID: "ENTRY-1", GroupOpenID: "GROUP-1", MemberOpenID: "MEMBER-1",
		Reason: "spam", AddedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	barred, err := opened.Blacklist().Barred(ctx, "GROUP-1", "MEMBER-1", "", now)
	if err != nil {
		t.Fatalf("Barred in the group that made it: %v", err)
	}
	if !barred {
		t.Error("the group that barred somebody does not refuse them")
	}

	barred, err = opened.Blacklist().Barred(ctx, "GROUP-2", "MEMBER-1", "", now)
	if err != nil {
		t.Fatalf("Barred in another group: %v", err)
	}
	if barred {
		t.Error("one group's bar refused somebody in another group")
	}

	// And the lists agree with the answers: the entry is in one group's list and not in
	// the other's.
	theirs, err := opened.Blacklist().List(ctx, "GROUP-1", 10)
	if err != nil {
		t.Fatalf("List of the group that made it: %v", err)
	}
	if len(theirs) != 1 {
		t.Errorf("the group that made a bar sees %d entries, want 1", len(theirs))
	}
	other, err := opened.Blacklist().List(ctx, "GROUP-2", 10)
	if err != nil {
		t.Fatalf("List of another group: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("another group sees %d of somebody else's entries, want none", len(other))
	}
}

// TestAnEntryWithNoGroupIsEveryGroups covers the entries that were there before the list
// was kept per group, and the ones an operator writes into the table by hand: with no group
// of their own they go on barring everybody, which is what they did.
func TestAnEntryWithNoGroupIsEveryGroups(t *testing.T) {
	ctx, now := blacklistTime(t)
	opened := openTestStore(t)

	if err := opened.Blacklist().Add(ctx, Barred{
		ID: "ENTRY-OLD", MemberOpenID: "MEMBER-1", Reason: "spam", AddedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, group := range []string{"GROUP-1", "GROUP-2"} {
		barred, err := opened.Blacklist().Barred(ctx, group, "MEMBER-1", "", now)
		if err != nil {
			t.Fatalf("Barred in %s: %v", group, err)
		}
		if !barred {
			t.Errorf("an entry with no group of its own did not bar %s", group)
		}
	}
}

// TestAnOlderListStillBarsEverybodyAndKeepsItsRows covers the tenth migration.
//
// Both tables are rebuilt, and what matters about that is the rows already in them: they
// have no group of their own, so they go on applying to every group -- which is what they
// did when the list was everybody's. Dropping them would quietly let those applicants back
// in and stop judging those members.
func TestAnOlderListStillBarsEverybodyAndKeepsItsRows(t *testing.T) {
	opened := openTestStoreAt(t, 9, `
INSERT INTO join_blacklist
    (id, member_openid, union_openid, reason, added_at, added_by, expires_at)
VALUES ('ENTRY-OLD', 'MEMBER-1', NULL, 'spam', 11, 'ADMIN-OPENID', 0);
INSERT INTO high_risk_watches
    (id, member_openid, reason, added_at, added_by, expires_at)
VALUES ('WATCH-OLD', 'MEMBER-1', 'spam', 11, 'ADMIN-OPENID', 4102444800)`)

	ctx := context.Background()
	now := time.Now()

	for _, group := range []string{"GROUP-1", "GROUP-2"} {
		barred, err := opened.Blacklist().Barred(ctx, group, "MEMBER-1", "", now)
		if err != nil {
			t.Fatalf("Barred in %s: %v", group, err)
		}
		if !barred {
			t.Errorf("the entry that was already there does not bar %s", group)
		}
	}

	entries, err := opened.Blacklist().List(ctx, "GROUP-1", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "ENTRY-OLD" || entries[0].GroupOpenID != "" {
		t.Errorf("the entry that was already there came back as %+v", entries)
	}

	watches, err := opened.Watches().List(ctx, now)
	if err != nil {
		t.Fatalf("listing the watches: %v", err)
	}
	if len(watches) != 1 || watches[0].ID != "WATCH-OLD" || watches[0].GroupOpenID != "" {
		t.Errorf("the watch that was already there came back as %+v", watches)
	}
}
