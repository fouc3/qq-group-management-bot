package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestAJudgementIsRecorded covers the record a punishment is reviewed through.
func TestAJudgementIsRecorded(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	entry := Judgement{
		ID:             "JUDGE-1",
		GroupOpenID:    "GROUP-1",
		SubjectOpenID:  "SUBJECT-1",
		ReporterOpenID: "REPORTER-1",
		Category:       "ad",
		Verdict:        JudgementViolation,
		Model:          "stub-model",
		MessageIDs:     []string{"M-1", "M-2", "M-3"},
		Reason:         "卖号广告",
		CreatedAt:      now.Unix(),
	}
	if err := opened.Judgements().Record(ctx, entry); err != nil {
		t.Fatalf("Record: %v", err)
	}

	entries, err := opened.Judgements().Recent(ctx, "GROUP-1", "SUBJECT-1",
		now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Recent returned %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.Category != "ad" || got.Verdict != JudgementViolation ||
		got.Model != "stub-model" || got.Reason != "卖号广告" {
		t.Errorf("recorded %+v, want the judgement that was written", got)
	}
	// The list of messages travels as one value and has to come back whole: it is
	// how "which messages was this decided on" is answered later.
	if len(got.MessageIDs) != 3 || got.MessageIDs[0] != "M-1" {
		t.Errorf("message ids = %v, want all three", got.MessageIDs)
	}

	// What followed is recorded separately, because deciding and doing happen in
	// different places.
	outcome := Outcome{
		Action:       "已撤回 1 条消息 已禁言 10分钟",
		MuteSeconds:  600,
		RecallReason: "已撤回 1 条",
		Recalls: []RecallOutcome{
			{ID: "M-1", Number: 1, Text: "加群送皮肤", Recalled: true},
			{ID: "M-2", Number: 2, Text: "私聊我", Recalled: false, Reason: "无操作权限"},
		},
	}
	if err := opened.Judgements().SetOutcome(ctx, "JUDGE-1", outcome); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	entries, err = opened.Judgements().Recent(ctx, "GROUP-1", "SUBJECT-1",
		now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("Recent after the outcome: %v", err)
	}
	got = entries[0]
	if got.Action != outcome.Action || got.MuteSeconds != 600 {
		t.Errorf("outcome = %q, %d; want what was done", got.Action, got.MuteSeconds)
	}
	if got.RecallReason != "已撤回 1 条" {
		t.Errorf("recall reason = %q, want why it was taken back", got.RecallReason)
	}
	// Which messages went and which stayed is the question the whole column
	// exists for, and the text is why: the message is gone from the group.
	if len(got.Recalls) != 2 {
		t.Fatalf("recalls = %+v, want one line per message", got.Recalls)
	}
	if !got.Recalls[0].Recalled || got.Recalls[0].Text != "加群送皮肤" {
		t.Errorf("recalls[0] = %+v, want the one that was taken back, with its text",
			got.Recalls[0])
	}
	if got.Recalls[1].Recalled || got.Recalls[1].Reason != "无操作权限" {
		t.Errorf("recalls[1] = %+v, want the one that stayed, with the reason",
			got.Recalls[1])
	}
}

// TestAJudgementThatNeverHappenedIsNotClean covers the distinction the verdict
// column exists for: "nothing was found" and "nobody looked" are different facts,
// and only one of them says anything about the message.
func TestAJudgementThatNeverHappenedIsNotClean(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	for _, verdict := range []string{JudgementOK, JudgementError} {
		if err := opened.Judgements().Record(ctx, Judgement{
			ID: "J-" + verdict, GroupOpenID: "GROUP-2", SubjectOpenID: "SUBJECT-2",
			Verdict: verdict, CreatedAt: now.Unix(),
		}); err != nil {
			t.Fatalf("Record %s: %v", verdict, err)
		}
	}
	entries, err := opened.Judgements().Recent(ctx, "GROUP-2", "SUBJECT-2",
		now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Recent returned %d entries, want 2", len(entries))
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Verdict] = true
	}
	if !seen[JudgementOK] || !seen[JudgementError] {
		t.Errorf("verdicts read back as %v, want both to be distinguishable", seen)
	}
}

// TestARecordNeedsAnID covers the one field the caller must supply: without it
// there is nothing to attach an outcome to later.
func TestARecordNeedsAnID(t *testing.T) {
	opened := openTestStore(t)
	if err := opened.Judgements().Record(context.Background(),
		Judgement{GroupOpenID: "GROUP-1"}); err == nil {
		t.Error("a judgement without an id must be refused")
	}
	if err := opened.Judgements().SetOutcome(context.Background(), "", Outcome{}); err == nil {
		t.Error("an outcome without a judgement id must be refused")
	}
}

// TestAJudgementIsFoundByItsReceipt covers the lookup a receipt number is for.
//
// The number in a group is short so it can be read off a screen and typed back,
// which means the lookup has to accept a prefix -- and has to refuse one that
// means more than one record rather than pick.
func TestAJudgementIsFoundByItsReceipt(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	for _, entry := range []Judgement{
		{ID: "a1b2c3d4e5f60718", GroupOpenID: "GROUP-1", Category: "ad",
			Verdict: JudgementViolation, Reason: "卖号广告", CreatedAt: now.Unix()},
		{ID: "a1b2c3ffffffffff", GroupOpenID: "GROUP-2", Category: "fraud",
			Verdict: JudgementViolation, Reason: "骗钱", CreatedAt: now.Unix()},
		{ID: "ffffffffffffffff", GroupOpenID: "GROUP-2", Verdict: JudgementOK,
			Reason: "只是推荐链接", CreatedAt: now.Unix()},
	} {
		if err := opened.Judgements().Record(ctx, entry); err != nil {
			t.Fatalf("Record(%s): %v", entry.ID, err)
		}
	}

	cases := map[string]struct {
		ask     string
		want    string
		wantErr error
	}{
		"the whole id":            {ask: "a1b2c3d4e5f60718", want: "a1b2c3d4e5f60718"},
		"a prefix that is unique": {ask: "a1b2c3d", want: "a1b2c3d4e5f60718"},
		"the other one":           {ask: "a1b2c3fff", want: "a1b2c3ffffffffff"},
		"surrounded by spaces":    {ask: "  a1b2c3d  ", want: "a1b2c3d4e5f60718"},
		"a prefix of both":        {ask: "a1b2c3", wantErr: ErrAmbiguousJudgement},
		"nothing at all":          {ask: "zzzz", wantErr: ErrJudgementNotFound},
		"an empty receipt":        {ask: "   ", wantErr: ErrJudgementNotFound},
		// A pattern, not a prefix: without escaping this would match every row.
		"a pattern character": {ask: "%", wantErr: ErrJudgementNotFound},
		"an underscore":       {ask: "_", wantErr: ErrJudgementNotFound},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			entry, err := opened.Judgements().Find(ctx, testCase.ask)
			if testCase.wantErr != nil {
				if !errors.Is(err, testCase.wantErr) {
					t.Fatalf("Find(%q) err = %v, want %v", testCase.ask, err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Find(%q): %v", testCase.ask, err)
			}
			if entry.ID != testCase.want {
				t.Errorf("Find(%q) = %s, want %s", testCase.ask, entry.ID, testCase.want)
			}
		})
	}

	// The rest of the row travels with it: the reason is the whole point of
	// looking a receipt up.
	entry, err := opened.Judgements().Find(ctx, "ffffffff")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if entry.Reason != "只是推荐链接" || entry.GroupOpenID != "GROUP-2" {
		t.Errorf("found %+v, want the record that id belongs to", entry)
	}
}
