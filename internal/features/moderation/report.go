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
	// locateWithin is how far back a quote's text is searched for. A quote points at
	// something the reporter just saw, so the message is normally seconds old: this is
	// wide enough to survive a busy group and narrow enough that two people saying the
	// same thing stays unlikely.
	locateWithin = 30 * time.Minute
)

// resolveWindow finds the messages to judge.
//
// Two ways in, because the platform gives two kinds of index. An ordinary quote
// carries the message's own index, which the cache holds -- and a miss there is worth
// waiting on, because the mention that carries the command can be delivered before
// the message it quotes. A quote of a message that is itself a quote carries a
// temporary index instead, which no ordinary event ever has and the cache can never
// hold, so waiting for it is waiting for nothing, and the quoted text is the only way
// in.
//
// The text is a locator and never evidence: it chooses which cached message is meant,
// and that message is then judged as its author's own words.
func (h *handler) resolveWindow(ctx context.Context, groupOpenID, quotedIndex,
	quotedText string) ([]CachedMessage, string, error) {
	span := time.Duration(h.config().ChainMinutes) * time.Minute

	if temporaryIndex(quotedIndex) {
		if window, anchor, err := h.locateWindow(ctx, groupOpenID, quotedText, span); err == nil {
			h.deps.Logger.Info("the quoted message was found by its text",
				"group", groupOpenID, "anchor", anchor)
			return window, anchor, nil
		}
	}

	var err error
	for attempt := 1; attempt <= cacheRetryAttempts; attempt++ {
		var chain []CachedMessage
		chain, err = h.cache.Context(ctx, groupOpenID, quotedIndex,
			h.config().ContextBefore, h.config().ContextAfter, span)
		if err == nil {
			if attempt > 1 {
				h.deps.Logger.Info("the quoted message arrived while waiting for it",
					"attempt", attempt)
			}
			return chain, quotedIndex, nil
		}
		if !errors.Is(err, ErrNotCached) {
			return nil, "", err
		}
		if window, anchor, locateErr := h.locateWindow(ctx, groupOpenID, quotedText, span); locateErr == nil {
			h.deps.Logger.Info("the quoted message was found by its text",
				"group", groupOpenID, "anchor", anchor, "attempt", attempt)
			return window, anchor, nil
		}
		if attempt == cacheRetryAttempts {
			return nil, "", err
		}
		// Waited out rather than asked again immediately, and abandoned the moment
		// the caller's context ends: a shutdown should not be held up by this.
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(cacheRetryDelay):
		}
	}
	return nil, "", err
}

// locateWindow finds a quoted message by what the quote showed of it, then reads the
// window around it by the index that message really has.
//
// The index it returns is the point of the second return value: a message found this
// way is named in the window by its own index, not by the temporary one the quote
// carried, and anything that goes looking for the anchor has to be told which.
func (h *handler) locateWindow(ctx context.Context, groupOpenID, quotedText string,
	span time.Duration) ([]CachedMessage, string, error) {
	if strings.TrimSpace(quotedText) == "" {
		return nil, "", ErrNotCached
	}
	located, err := h.cache.Locate(ctx, groupOpenID, quotedText, locateWithin)
	if err != nil {
		return nil, "", err
	}
	chain, err := h.cache.Context(ctx, groupOpenID, located.Idx,
		h.config().ContextBefore, h.config().ContextAfter, span)
	if err != nil {
		return nil, "", err
	}
	return chain, located.Idx, nil
}

// temporaryIndex reports whether an index is one the cache can never hold.
func temporaryIndex(index string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(index)), "TMP_")
}

// JudgeQuoted is the seam's entry point: it judges one quoted message and writes
// the judgement down.
//
// The record is made here rather than inside the judgement so that a judgement
// which could not be reached is recorded too. "Nothing was found" and "nobody
// looked" are different facts, and a record that only held the first would answer
// the wrong question afterwards.
func (h *handler) JudgeQuoted(ctx context.Context, groupOpenID, quotedIndex,
	quotedText, reporterOpenID string) (feature.ModerationVerdict, error) {
	report, err := h.judgeQuoted(ctx, groupOpenID, quotedIndex, quotedText)
	return h.recordJudgement(ctx, groupOpenID, reporterOpenID, report, err), err
}

// judgeQuoted does the judging itself: find the window, ask the model, work out
// what the answer means.
//
// Everything that is not a judgement is an error. A cache miss and an unreadable
// answer both mean nobody knows whether the message is acceptable, and a caller
// that confused that with acceptable would quietly drop a real report.
func (h *handler) judgeQuoted(ctx context.Context, groupOpenID, quotedIndex,
	quotedText string) (feature.ModerationVerdict, error) {
	if strings.TrimSpace(quotedIndex) == "" {
		return feature.ModerationVerdict{}, fmt.Errorf("%w: the report does not say "+
			"which message it is about", ErrUnjudged)
	}
	if !h.config().JudgingEnabled() {
		return feature.ModerationVerdict{}, fmt.Errorf("%w: no model is configured, "+
			"so nothing can be judged", ErrUnjudged)
	}
	if !h.config().judgingEnabledFor(groupOpenID) {
		return feature.ModerationVerdict{}, fmt.Errorf("%w: judging is turned off "+
			"for this group", ErrUnjudged)
	}

	// A report can be delivered before the message it quotes is. The platform
	// pushes the mention that carries the command ahead of the ordinary messages
	// that came just before it, which was measured the hard way: the same report
	// failed when it followed the message by four seconds and worked when it
	// followed it by ten. A miss is therefore waited on rather than believed.
	// anchor is the index the quoted message is known by *inside this window*. It is
	// the one the quote carried when that could be resolved, and the one the located
	// message really has when the quote only came with its text -- looking for the
	// temporary index here would never find anything, which is exactly the bug this
	// return value exists to prevent.
	chain, anchor, err := h.resolveWindow(ctx, groupOpenID, quotedIndex, quotedText)
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
		if message.Idx == anchor {
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
	if h.config().senderExempt(groupOpenID, subject) {
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
		resolveRecall(chain, subject, verdict.Recall, quotedID, anchor)
	if !verdict.Violation() {
		return report, nil
	}

	report.Label = h.config().LabelFor(verdict.Category)
	seconds, known := h.config().MuteForGroup(groupOpenID, verdict.Category)
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
