package moderation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// What a miss is waited on with: long enough to cover the delay measured in
// production (an ordinary message arriving more than four seconds after the
// mention that quoted it), and short enough that a report about something that
// will never arrive is still answered.
const (
	cacheRetryAttempts = 12
	cacheRetryDelay    = time.Second
)

// contextWithRetry looks the window up, waiting a moment for a message that is
// still on its way.
//
// Only a miss is retried. A cache that is down, or a window that was found but
// does not contain the quoted message, are not things that improve by waiting.
func (h *handler) contextWithRetry(ctx context.Context, groupOpenID,
	quotedIndex string) ([]CachedMessage, error) {
	span := time.Duration(h.cfg.ChainMinutes) * time.Minute
	var err error
	for attempt := 1; attempt <= cacheRetryAttempts; attempt++ {
		var chain []CachedMessage
		chain, err = h.cache.Context(ctx, groupOpenID, quotedIndex,
			h.cfg.ContextBefore, h.cfg.ContextAfter, span)
		if err == nil {
			if attempt > 1 {
				h.deps.Logger.Info("the quoted message arrived while waiting for it",
					"attempt", attempt)
			}
			return chain, nil
		}
		if !errors.Is(err, ErrNotCached) || attempt == cacheRetryAttempts {
			return nil, err
		}
		// Waited out rather than asked again immediately, and abandoned the moment
		// the caller's context ends: a shutdown should not be held up by this.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(cacheRetryDelay):
		}
	}
	return nil, err
}

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

	// A report can be delivered before the message it quotes is. The platform
	// pushes the mention that carries the command ahead of the ordinary messages
	// that came just before it, which was measured the hard way: the same report
	// failed when it followed the message by four seconds and worked when it
	// followed it by ten. A miss is therefore waited on rather than believed.
	chain, err := h.contextWithRetry(ctx, groupOpenID, quotedIndex)
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
	report.RecallMessageIDs, report.RecallNumbers =
		resolveRecall(chain, subject, verdict.Recall, quotedID, quotedIndex)
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
