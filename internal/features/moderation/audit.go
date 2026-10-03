package moderation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// JudgingEnabled implements feature.Moderation.
func (h *handler) JudgingEnabled() bool { return h.config().JudgingEnabled() }

// RecordOutcome implements feature.Moderation.
func (h *handler) RecordOutcome(ctx context.Context, judgementID string,
	outcome store.Outcome) error {
	judgements := h.judgementStore()
	if judgements == nil {
		return nil
	}
	if err := judgements.SetOutcome(ctx, judgementID, outcome); err != nil {
		return fmt.Errorf("recording what followed a judgement: %w", err)
	}
	return nil
}

// MarkPunished implements feature.Moderation.
//
// The messages are marked in the cache this feature owns, and the marking is the
// reason the cache can be trusted to answer "has this been dealt with": a message
// that was taken back never comes back in a window.
//
// A failure here is reported and not fatal. It costs the next report about the
// same messages some accuracy, which is where things already were; refusing to
// finish the punishment because the mark could not be written would trade a small
// future problem for a real one now.
func (h *handler) MarkPunished(ctx context.Context, groupOpenID string,
	messageIndexes []string) error {
	if h.cache == nil {
		return nil
	}
	if err := h.cache.MarkPunished(ctx, groupOpenID, messageIndexes); err != nil {
		return fmt.Errorf("marking messages as taken back: %w", err)
	}
	return nil
}

// LabelFor implements feature.Moderation.
func (h *handler) LabelFor(category string) string {
	return h.config().LabelFor(category)
}

// judgementStore is the record, or nil when this deployment has none.
func (h *handler) judgementStore() store.JudgementStore {
	if h.deps.Store == nil {
		return nil
	}
	return h.deps.Store.Judgements()
}

// recordJudgement writes down what was decided, including a judgement that was
// never reached, and returns the report carrying the record's id.
//
// A failure to write is reported and swallowed. The record is for reviewing a
// punishment afterwards; refusing to answer the group now because the record
// could not be kept would trade a future problem for a present one.
func (h *handler) recordJudgement(ctx context.Context, groupOpenID, reporterOpenID string,
	report feature.ModerationVerdict, judgeErr error) feature.ModerationVerdict {
	judgements := h.judgementStore()
	if judgements == nil {
		return report
	}
	id, err := newJudgementID()
	if err != nil {
		h.deps.Logger.Error("could not make a judgement id", "error", err)
		return report
	}

	entry := store.Judgement{
		ID:             id,
		GroupOpenID:    groupOpenID,
		SubjectOpenID:  report.SubjectOpenID,
		ReporterOpenID: reporterOpenID,
		Category:       report.Category,
		Model:          report.Model,
		MessageIDs:     report.JudgedMessageIDs,
		Reason:         report.Reason,
		Reasoning:      report.Reasoning,
	}
	switch {
	case judgeErr != nil:
		// Not "ok": nobody looked. That distinction is the whole reason the
		// verdict column exists, and the reason an unjudged report must never be
		// read later as a message that was found acceptable.
		entry.Verdict = store.JudgementError
	case report.Category != "":
		entry.Verdict = store.JudgementViolation
	default:
		entry.Verdict = store.JudgementOK
	}

	if err := judgements.Record(ctx, entry); err != nil {
		h.deps.Logger.Error("could not record a judgement", "error", err)
		return report
	}
	report.JudgementID = id
	return report
}

// newJudgementID returns the key a judgement is stored under.
func newJudgementID() (string, error) {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generating a judgement id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}
