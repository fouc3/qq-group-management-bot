package moderation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// JudgeQuoted judges one quoted message in its context.
//
// It is the whole path from a report to a verdict: find the quoted message in the
// cache, take the window around it, ask the model, and describe the result in the
// terms a caller acts on -- what was found, how long it is punished with, and who
// posted it.
//
// Everything that is not a judgement is an error. A cache miss and an unreadable
// answer both mean nobody knows whether the message is acceptable, and a caller
// that confused that with acceptable would quietly drop a real report.
func (h *handler) JudgeQuoted(ctx context.Context, groupOpenID,
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
	subject, quotedID, quotedText := "", "", ""
	judged := make([]string, 0, len(chain))
	for _, message := range chain {
		if message.ID != "" {
			judged = append(judged, message.ID)
		}
		if message.Idx == quotedIndex {
			subject, quotedID, quotedText = message.User, message.ID, message.Text
		}
	}
	if subject == "" {
		return feature.ModerationVerdict{}, fmt.Errorf("%w: the quoted message is not "+
			"in the window", ErrUnjudged)
	}

	// The group's list of what it considers legitimate is deliberately **not** a
	// veto here any more. It was one, and it was unsound: a rule about text is
	// satisfied by the text, so the advertisement that carried an allowed word as
	// a shield was never judged at all. Both attempts are written down in
	// allowedIn. The list is moving into the prompt as trusted context, which is
	// where the judgement it needs can actually be made.
	_ = quotedText

	verdict, err := h.Judge(ctx, chain)
	if err != nil {
		return feature.ModerationVerdict{}, err
	}

	report := feature.ModerationVerdict{
		Category:         verdict.Category,
		SubjectOpenID:    subject,
		QuotedMessageID:  quotedID,
		JudgedMessageIDs: judged,
		Reason:           verdict.Reason,
		Model:            verdict.Model,
	}
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
