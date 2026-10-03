package store

import (
	"context"
	"testing"
	"time"
)

// TestAMemberEventIsRecorded covers the log that is the only copy of who came and
// went.
//
// The platform refuses this application the member-list endpoints, so nothing can
// be read back from it later: whatever is not written here is not written
// anywhere.
func TestAMemberEventIsRecorded(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Minute).Unix()

	entry := MemberEvent{
		GroupOpenID:  "GROUP-1",
		GroupName:    "苹果社区AI中转站",
		GroupQQID:    1082100371,
		MemberOpenID: "MEMBER-1",
		Kind:         MemberJoined,
		EventAt:      at,
	}
	if err := opened.MemberEvents().Record(ctx, entry); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := opened.MemberEvents().Record(ctx, MemberEvent{
		GroupOpenID:  "GROUP-1",
		GroupName:    "苹果社区AI中转站",
		GroupQQID:    1082100371,
		MemberOpenID: "MEMBER-2",
		Kind:         MemberLeft,
		EventAt:      at + 60,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	events, err := opened.MemberEvents().List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("List returned %d events, want 2", len(events))
	}
	// Newest first, and the group's name travels with the row: a name looked up
	// later is a name the group may have changed by then.
	if events[0].MemberOpenID != "MEMBER-2" || events[0].Kind != MemberLeft {
		t.Errorf("events[0] = %+v, want the later departure", events[0])
	}
	if events[1].GroupName != "苹果社区AI中转站" || events[1].GroupQQID != 1082100371 {
		t.Errorf("events[1] = %+v, want the group it happened in", events[1])
	}
	if events[1].EventAt != at {
		t.Errorf("event at %d, want the platform's own %d", events[1].EventAt, at)
	}
}

// TestAMemberEventIsRecordedOnce covers the identity of a row.
//
// The platform sends an event once, and "once" is a claim not worth relying on: a
// reconnect can replay what was already delivered. The row is identified by the
// fact itself -- group, member, kind and time -- so a replay writes nothing rather
// than a second copy of the same thing.
func TestAMemberEventIsRecordedOnce(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	entry := MemberEvent{
		GroupOpenID:  "GROUP-1",
		GroupName:    "一个群",
		MemberOpenID: "MEMBER-1",
		Kind:         MemberJoined,
		EventAt:      1791000000,
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := opened.MemberEvents().Record(ctx, entry); err != nil {
			t.Fatalf("Record %d: %v", attempt+1, err)
		}
	}
	events, err := opened.MemberEvents().List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("List returned %d events, want one", len(events))
	}

	// The same member leaving and rejoining in the same second is two facts, and
	// the kind is what tells them apart.
	if err := opened.MemberEvents().Record(ctx, MemberEvent{
		GroupOpenID:  "GROUP-1",
		MemberOpenID: "MEMBER-1",
		Kind:         MemberLeft,
		EventAt:      1791000000,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if events, err = opened.MemberEvents().List(ctx, 10); err != nil || len(events) != 2 {
		t.Errorf("List = %d events (err %v), want both kinds", len(events), err)
	}
}

// TestAMemberEventNeedsAGroupAndAMember covers the two fields without which a row
// answers nothing.
func TestAMemberEventNeedsAGroupAndAMember(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()

	cases := map[string]MemberEvent{
		"no group":  {MemberOpenID: "MEMBER-1", Kind: MemberJoined},
		"no member": {GroupOpenID: "GROUP-1", Kind: MemberJoined},
		"a kind nobody defined": {GroupOpenID: "GROUP-1", MemberOpenID: "MEMBER-1",
			Kind: "promoted"},
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			if err := opened.MemberEvents().Record(ctx, entry); err == nil {
				t.Error("a member event that answers nothing must be refused")
			}
		})
	}

	// And an event with no time from the platform is stamped rather than refused:
	// the arrival is a worse time than the platform's, but it is a time.
	if err := opened.MemberEvents().Record(ctx, MemberEvent{
		GroupOpenID: "GROUP-1", MemberOpenID: "MEMBER-1", Kind: MemberJoined,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	events, err := opened.MemberEvents().List(ctx, 1)
	if err != nil || len(events) != 1 {
		t.Fatalf("List = %d events (err %v), want one", len(events), err)
	}
	if events[0].EventAt == 0 {
		t.Error("an event with no time was stored without one")
	}
}

// TestAListingIsBounded covers the default a caller who asks for nothing gets.
func TestAListingIsBounded(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	for index := 1; index <= 3; index++ {
		if err := opened.MemberEvents().Record(ctx, MemberEvent{
			GroupOpenID:  "GROUP-1",
			MemberOpenID: string(rune('A' + index)),
			Kind:         MemberJoined,
			EventAt:      int64(1791000000 + index),
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	events, err := opened.MemberEvents().List(ctx, 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("List returned %d events, want the two asked for", len(events))
	}
	if events[0].MemberOpenID != "D" {
		t.Errorf("events[0] = %+v, want the newest", events[0])
	}
	// A limit the caller does not give falls back rather than meaning "no limit".
	if events, err := opened.MemberEvents().List(ctx, 0); err != nil || len(events) != 3 {
		t.Errorf("List(0) = %d events (err %v), want the default limit", len(events), err)
	}
}
