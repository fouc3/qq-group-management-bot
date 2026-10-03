package store

import (
	"context"
	"testing"
	"time"
)

// TestABroadcastIsRecorded covers the record that makes an anonymous notice answerable: the
// group is not told who asked, and this is where it is kept.
func TestABroadcastIsRecorded(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Minute).Unix()

	entry := Broadcast{
		Token:        "TOKEN-1",
		SenderOpenID: "ADMIN-1",
		Anonymous:    true,
		Markdown:     true,
		Content:      "第一行\n第二行",
		SentAt:       at,
		Targets: []Target{
			{GroupOpenID: "GROUP-2", MessageID: "MESSAGE-1"},
			{GroupOpenID: "GROUP-3", MessageID: "MESSAGE-2"},
		},
	}
	if err := opened.Broadcasts().Record(ctx, entry); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// And the same notice again changes nothing: the card it was written on is the identity,
	// whatever the platform answered the second time.
	entry.Targets[0].MessageID = "MESSAGE-1-AGAIN"
	if err := opened.Broadcasts().Record(ctx, entry); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Asked of either group, it is one answer: one notice, with where it went.
	for _, group := range []string{"GROUP-2", "GROUP-3"} {
		posted, err := opened.Broadcasts().ListByGroups(ctx, []string{group}, 10)
		if err != nil {
			t.Fatalf("ListByGroups: %v", err)
		}
		if len(posted) != 1 {
			t.Fatalf("%s has %d broadcast(s), want 1: %+v", group, len(posted), posted)
		}
		got := posted[0]
		if got.SenderOpenID != "ADMIN-1" {
			t.Errorf("sender = %q, want the member who asked", got.SenderOpenID)
		}
		if !got.Anonymous || !got.Markdown {
			t.Errorf("the switches were not kept: %+v", got)
		}
		if got.Content != "第一行\n第二行" {
			t.Errorf("content = %q, want what was posted, newlines and all", got.Content)
		}
		if got.SentAt != at {
			t.Errorf("sent_at = %d, want %d", got.SentAt, at)
		}
		if len(got.Targets) != 2 {
			t.Fatalf("the notice reached %d group(s), want both: %+v", len(got.Targets), got.Targets)
		}
		if got.Targets[0].GroupOpenID != "GROUP-2" || got.Targets[1].GroupOpenID != "GROUP-3" {
			t.Errorf("the targets are %+v, want the groups it went to, in the order sent",
				got.Targets)
		}
		if got.Targets[0].MessageID != "MESSAGE-1" {
			t.Errorf("message = %q, want the platform's first id for it: the second recording "+
				"is the same fact, not a newer one", got.Targets[0].MessageID)
		}
	}

	// Asked of both at once it is still one answer rather than one per group.
	both, err := opened.Broadcasts().ListByGroups(ctx, []string{"GROUP-2", "GROUP-3"}, 10)
	if err != nil {
		t.Fatalf("ListByGroups: %v", err)
	}
	if len(both) != 1 {
		t.Errorf("a notice that reached two of these groups counts %d time(s)", len(both))
	}

	if empty, err := opened.Broadcasts().ListByGroups(ctx, []string{"GROUP-NOBODY"}, 10); err != nil {
		t.Fatalf("ListByGroups: %v", err)
	} else if len(empty) != 0 {
		t.Errorf("a group with no broadcasts has %d", len(empty))
	}
	if none, err := opened.Broadcasts().ListByGroups(ctx, nil, 10); err != nil {
		t.Fatalf("ListByGroups: %v", err)
	} else if len(none) != 0 {
		t.Errorf("asking about no groups at all answered %d", len(none))
	}
}

// TestBroadcastsAreNewestFirst covers the order an audit reads them in.
func TestBroadcastsAreNewestFirst(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	for index, at := range []int64{now - 300, now, now - 60} {
		if err := opened.Broadcasts().Record(ctx, Broadcast{
			Token:        "TOKEN-" + string(rune('a'+index)),
			SenderOpenID: "ADMIN-1",
			Content:      "内容",
			SentAt:       at,
			Targets:      []Target{{GroupOpenID: "GROUP-1"}},
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	posted, err := opened.Broadcasts().ListByGroups(ctx, []string{"GROUP-1"}, 10)
	if err != nil {
		t.Fatalf("ListByGroups: %v", err)
	}
	if len(posted) != 3 {
		t.Fatalf("read %d broadcasts, want 3", len(posted))
	}
	for index := 1; index < len(posted); index++ {
		if posted[index-1].SentAt < posted[index].SentAt {
			t.Errorf("the record is not newest first: %+v", posted)
			break
		}
	}

	// And a limit is a limit.
	if limited, err := opened.Broadcasts().ListByGroups(ctx, []string{"GROUP-1"}, 2); err != nil {
		t.Fatalf("ListByGroups: %v", err)
	} else if len(limited) != 2 {
		t.Errorf("read %d broadcasts with a limit of 2", len(limited))
	}
}

// TestABroadcastNeedsWhoAndWhere covers what a record without the point of it would be: a
// notice nobody is named for, or one that went nowhere.
func TestABroadcastNeedsWhoAndWhere(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()

	cases := map[string]Broadcast{
		"no card": {SenderOpenID: "ADMIN-1", Content: "内容",
			Targets: []Target{{GroupOpenID: "GROUP-1"}}},
		"no groups": {Token: "T", SenderOpenID: "ADMIN-1", Content: "内容"},
		"no sender": {Token: "T", Content: "内容",
			Targets: []Target{{GroupOpenID: "GROUP-1"}}},
		"a target with no group": {Token: "T", SenderOpenID: "ADMIN-1", Content: "内容",
			Targets: []Target{{MessageID: "MESSAGE-1"}}},
	}
	for why, entry := range cases {
		if err := opened.Broadcasts().Record(ctx, entry); err == nil {
			t.Errorf("%s was recorded: %+v", why, entry)
		}
	}
}

// TestTheOldRecordIsMovedIntoTheNewOne covers migration 7, which is the one that rewrites rows
// rather than adding a table: the copies of one notice that were one row per group become the
// targets of the one notice they were copies of.
//
// Built by hand from the layout the previous build wrote, because that is the shape a database
// in production is in.
func TestTheOldRecordIsMovedIntoTheNewOne(t *testing.T) {
	opened := openTestStoreAt(t, 6, `
INSERT INTO broadcasts
    (token, sender_openid, group_openid, anonymous, markdown, content, sent_at, message_id)
VALUES
    ('TOKEN-1', 'ADMIN-1', 'GROUP-A', 1, 1, '一条通报', 1000, 'MESSAGE-A'),
    ('TOKEN-1', 'ADMIN-1', 'GROUP-B', 1, 1, '一条通报', 1005, 'MESSAGE-B'),
    ('TOKEN-2', 'ADMIN-2', 'GROUP-A', 0, 0, '另一条', 2000, 'MESSAGE-C');
`)
	ctx := context.Background()

	one, err := opened.Broadcasts().ListByGroups(ctx, []string{"GROUP-A"}, 10)
	if err != nil {
		t.Fatalf("ListByGroups: %v", err)
	}
	if len(one) != 2 {
		t.Fatalf("GROUP-A has %d broadcast(s), want the two notices: %+v", len(one), one)
	}

	// The notice that reached two groups is one row, with both of them on it, and the moment
	// it went out is the earliest of the copies rather than the last.
	posted := one[1]
	if posted.Token != "TOKEN-1" || posted.SentAt != 1000 {
		t.Errorf("the moved notice is %+v, want the first send's token and time", posted)
	}
	if len(posted.Targets) != 2 {
		t.Fatalf("the moved notice has %d target(s), want both groups: %+v",
			len(posted.Targets), posted.Targets)
	}
	if posted.Targets[0].MessageID != "MESSAGE-A" || posted.Targets[1].MessageID != "MESSAGE-B" {
		t.Errorf("the message ids did not come along: %+v", posted.Targets)
	}
	if posted.SenderOpenID != "ADMIN-1" || !posted.Anonymous || !posted.Markdown {
		t.Errorf("the notice lost who or how: %+v", posted)
	}
	if posted.Content != "一条通报" {
		t.Errorf("content = %q, want the text that was posted", posted.Content)
	}

	// And the record is still found from the other group it reached.
	fromB, err := opened.Broadcasts().ListByGroups(ctx, []string{"GROUP-B"}, 10)
	if err != nil {
		t.Fatalf("ListByGroups: %v", err)
	}
	if len(fromB) != 1 || fromB[0].Token != "TOKEN-1" {
		t.Errorf("GROUP-B reads %+v, want the one notice that reached it", fromB)
	}
}
