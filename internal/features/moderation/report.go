package moderation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// JudgeQuoted is the seam's entry point: it judges one quoted message and writes
// the judgement down.
//
// The record is made here rather than inside the judgement so that a judgement
// which could not be reached is recorded too. "Nothing was found" and "nobody
// looked" are different facts, and a record that only held the first would answer
// the wrong question afterwards.
func (h *handler) JudgeQuoted(ctx context.Context, groupOpenID, quotedIndex,
	reporterOpenID string) (feature.ModerationVerdict, error) {
	report, err := h.judgeQuoted(ctx, groupOpenID, quotedIndex)
	return h.recordJudgement(ctx, groupOpenID, reporterOpenID, report, err), err
}

// judgeQuoted does the judging itself: find the window, ask the model, work out
// what the answer means.
//
// Everything that is not a judgement is an error. A cache miss and an unreadable
// answer both mean nobody knows whether the message is acceptable, and a caller
// that confused that with acceptable would quietly drop a real report.
func (h *handler) judgeQuoted(ctx context.Context, groupOpenID,
	quotedIndex string) (feature.ModerationVerdict, error) {
	if strings.TrimSpace(quotedIndex) == "" {
		return feature.ModerationVerdict{}, fmt.Errorf("%w: the report does not say "+
			"which message it is about", ErrUnjudged)
	}
	if !h.cfg.JudgingEnabled() {
		return feature.ModerationVerdict{}, fmt.Errorf("%w: no model is configured, "+
			"so nothing can be judged", ErrUnjudged)
	}
	if !h.cfg.judgingEnabledFor(groupOpenID) {
		return feature.ModerationVerdict{}, fmt.Errorf("%w: judging is turned off "+
			"for this group", ErrUnjudged)
	}

	chain, err := h.cache.Context(ctx, groupOpenID, quotedIndex,
		h.cfg.ContextBefore, h.cfg.ContextAfter,
		time.Duration(h.cfg.ChainMinutes)*time.Minute)
	if err != nil {
		return feature.ModerationVerdict{}, fmt.Errorf("%w: %v", ErrUnjudged, err)
	}

	// The subject is the author of the message that was reported, not whoever is
	// loudest in the window: the report is about that message, and what follows
	// follows the message.
	subject, quotedID := "", ""
	judged := make([]string, 0, len(chain))
	for _, message := range chain {
		if message.ID != "" {
			judged = append(judged, message.ID)
		}
		if message.Idx == quotedIndex {
			subject, quotedID = message.User, message.ID
		}
	}
	if subject == "" {
		return feature.ModerationVerdict{}, fmt.Errorf("%w: the quoted message is not "+
			"in the window", ErrUnjudged)
	}

	// The one exemption the code makes is about identity: a sender this group has
	// put beyond judging. There is no sentence somebody can write to become that
	// account, which is exactly why this one is sound and the content version was
	// not -- see allowedIn for both attempts at that.
	if h.cfg.senderExempt(groupOpenID, subject) {
		return feature.ModerationVerdict{
			SubjectOpenID:    subject,
			QuotedMessageID:  quotedID,
			JudgedMessageIDs: judged,
			Reason:           "该群的豁免发送者",
		}, nil
	}

	verdict, err := h.Judge(ctx, groupOpenID, chain)

	// Built before the error is looked at, so that a judgement which could not be
	// reached still records who it was about. The subject is known by now, and a
	// record that says only "something went wrong" answers nothing.
	report := feature.ModerationVerdict{
		SubjectOpenID:    subject,
		QuotedMessageID:  quotedID,
		JudgedMessageIDs: judged,
	}
	if err != nil {
		return report, err
	}
	report.Category = verdict.Category
	report.Reason = verdict.Reason
	report.Model = verdict.Model
	if !verdict.Violation() {
		return report, nil
	}

	report.Label = h.cfg.LabelFor(verdict.Category)
	seconds, known := h.cfg.MuteForGroup(groupOpenID, verdict.Category)
	if !known {
		// The category came from the configuration's own list, so this cannot
		// happen in a running bot. Saying so is still better than punishing
		// somebody for a category that has no duration to serve.
		return feature.ModerationVerdict{}, fmt.Errorf("%w: category %q has no "+
			"duration configured", ErrUnjudged, verdict.Category)
	}
	report.MuteSeconds = seconds
	return report, nil
}
