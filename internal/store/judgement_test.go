package store

import (
	"context"
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
	if err := opened.Judgements().SetOutcome(ctx, "JUDGE-1", "mute+recall", 600); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	entries, err = opened.Judgements().Recent(ctx, "GROUP-1", "SUBJECT-1",
		now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("Recent after the outcome: %v", err)
	}
	if entries[0].Action != "mute+recall" || entries[0].MuteSeconds != 600 {
		t.Errorf("outcome = %q, %d; want what was done",
			entries[0].Action, entries[0].MuteSeconds)
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
	if err := opened.Judgements().SetOutcome(context.Background(), "", "none", 0); err == nil {
		t.Error("an outcome without a judgement id must be refused")
	}
}
