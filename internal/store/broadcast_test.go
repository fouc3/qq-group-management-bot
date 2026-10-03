package store

import (
	"context"
	"testing"
	"time"
)

// TestABroadcastIsRecorded covers the record that makes an anonymous notice
// answerable: the group is not told who asked, and this is where it is kept.
func TestABroadcastIsRecorded(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Minute).Unix()

	entry := Broadcast{
		Token:        "TOKEN-1",
		SenderOpenID: "ADMIN-1",
		GroupOpenID:  "GROUP-2",
		Anonymous:    true,
		Markdown:     true,
		Content:      "第一行\n第二行",
		SentAt:       at,
		MessageID:    "MESSAGE-1",
	}
	if err := opened.Broadcasts().Record(ctx, entry); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// The same card posted into a second group is a second row: what an audit asks is
	// group by group.
	if err := opened.Broadcasts().Record(ctx, Broadcast{
		Token: "TOKEN-1", SenderOpenID: "ADMIN-1",
		GroupOpenID: "GROUP-3", Anonymous: true, Markdown: true,
		Content: "第一行\n第二行", SentAt: at, MessageID: "MESSAGE-2",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// And the same card into the same group again changes nothing.
	if err := opened.Broadcasts().Record(ctx, entry); err != nil {
		t.Fatalf("Record: %v", err)
	}

	posted, err := opened.Broadcasts().ListByGroup(ctx, "GROUP-2", 10)
	if err != nil {
		t.Fatalf("ListByGroup: %v", err)
	}
	if len(posted) != 1 {
		t.Fatalf("GROUP-2 has %d broadcast(s), want 1: %+v", len(posted), posted)
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
	if got.MessageID != "MESSAGE-1" {
		t.Errorf("message = %q, want the platform's id for it", got.MessageID)
	}
	if got.SentAt != at {
		t.Errorf("sent_at = %d, want %d", got.SentAt, at)
	}

	// Another group's record is not in this group's list.
	other, err := opened.Broadcasts().ListByGroup(ctx, "GROUP-3", 10)
	if err != nil {
		t.Fatalf("ListByGroup: %v", err)
	}
	if len(other) != 1 || other[0].MessageID != "MESSAGE-2" {
		t.Errorf("GROUP-3 has %+v, want only its own copy", other)
	}
	if empty, err := opened.Broadcasts().ListByGroup(ctx, "GROUP-NOBODY", 10); err != nil {
		t.Fatalf("ListByGroup: %v", err)
	} else if len(empty) != 0 {
		t.Errorf("a group with no broadcasts has %d", len(empty))
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
			GroupOpenID:  "GROUP-1",
			Content:      "内容",
			SentAt:       at,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	posted, err := opened.Broadcasts().ListByGroup(ctx, "GROUP-1", 10)
	if err != nil {
		t.Fatalf("ListByGroup: %v", err)
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
	if limited, err := opened.Broadcasts().ListByGroup(ctx, "GROUP-1", 2); err != nil {
		t.Fatalf("ListByGroup: %v", err)
	} else if len(limited) != 2 {
		t.Errorf("read %d broadcasts with a limit of 2", len(limited))
	}
}

// TestABroadcastNeedsWhoAndWhere covers what a record without the point of it would
// be: a notice nobody is named for, or one that went nowhere.
func TestABroadcastNeedsWhoAndWhere(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()

	cases := map[string]Broadcast{
		"no card":   {SenderOpenID: "ADMIN-1", GroupOpenID: "GROUP-1", Content: "内容"},
		"no group":  {Token: "T", SenderOpenID: "ADMIN-1", Content: "内容"},
		"no sender": {Token: "T", GroupOpenID: "GROUP-1", Content: "内容"},
	}
	for why, entry := range cases {
		if err := opened.Broadcasts().Record(ctx, entry); err == nil {
			t.Errorf("%s was recorded: %+v", why, entry)
		}
	}
}
