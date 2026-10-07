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
// Two things can name the quoted message, and only one of them is a name:
//
//   - an index the cache holds, which identifies it outright. The scene's
//     ref_msg_idx and the quoted element's msg_idx are both this, and either may
//     be the usable one -- a quote of a message that is itself a quote comes with a
//     temporary index in the scene, which no event ever carries and the cache can
//     never hold, while the element may still name the message properly;
//   - the text the quote showed, which identifies nothing by itself. Two members
//     can say the same thing, so the text only means something next to the author
//     the platform named, and it is used only when no index names the message at
//     all.
//
// When neither is available the honest answer is that the message cannot be told
// apart from the rest, and that is returned rather than guessed at: the caller
// answers the group about a report that could not be resolved, and nobody is
// judged from a name that does not exist.
//
// A miss on a real index is waited on, because the mention carrying the command
// can be delivered before the message it quotes arrives. Waiting on a temporary
// one is waiting for nothing.
func (h *handler) resolveWindow(ctx context.Context, groupOpenID string,
	quoted feature.QuotedMessage) ([]CachedMessage, string, error) {
	span := time.Duration(h.config().ChainMinutes) * time.Minute
	index := usableIndex(quoted)

	if index == "" {
		// Nothing names the message, so the text and the author are all there is.
		window, anchor, err := h.locateWindow(ctx, groupOpenID, quoted, span)
		if err == nil {
			h.deps.Logger.Info("the quoted message was found by its text and author",
				"group", groupOpenID, "author", quoted.Author, "anchor", anchor)
			return window, anchor, nil
		}
		return nil, "", fmt.Errorf("%w: %v", feature.ErrUnidentifiedQuote, err)
	}

	var err error
	for attempt := 1; attempt <= cacheRetryAttempts; attempt++ {
		var chain []CachedMessage
		chain, err = h.cache.Context(ctx, groupOpenID, index,
			h.config().ContextBefore, h.config().ContextAfter, span)
		if err == nil {
			if attempt > 1 {
				h.deps.Logger.Info("the quoted message arrived while waiting for it",
					"attempt", attempt)
			}
			return chain, index, nil
		}
		if !errors.Is(err, ErrNotCached) {
			return nil, "", err
		}
		// The quote may name the message with an index the cache stored under a
		// different one, which is what the text and the author are for. It is only
		// tried when the platform named the sender: see Locate.
		if window, anchor, locateErr := h.locateWindow(ctx, groupOpenID, quoted, span); locateErr == nil {
			h.deps.Logger.Info("the quoted message was found by its text and author",
				"group", groupOpenID, "author", quoted.Author, "anchor", anchor,
				"attempt", attempt)
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

// unjudgeablePicture reports whether a report cannot be judged because of the
// pictures on the message it is about.
//
// Two cases, and both are refusals rather than judgements:
//
//   - the model is not allowed to be shown pictures. Then a message that carries one
//     is a message this deployment cannot judge at all, whatever else it says: the
//     picture is the part that matters and it would be silently missing.
//   - the model may see them, but this message's pictures are no longer in the cache
//     -- the download failed, they were over the size limit, or they have been swept.
//     That is only fatal for a message that is pictures and nothing else: a message
//     with words in it is still something the judge can read, and refusing it would
//     throw away a report the group made about words it can see.
func unjudgeablePicture(reported CachedMessage, vision bool) error {
	if reported.ImgCount == 0 {
		return nil
	}
	if !vision {
		return fmt.Errorf("%w: the reported message carries %d picture(s)",
			feature.ErrPictureNotJudgeable, reported.ImgCount)
	}
	if len(reported.Imgs) == 0 && strings.TrimSpace(reported.Text) == "" {
		return fmt.Errorf("%w: the reported message is pictures and nothing else, "+
			"and none of them are in the cache any more", ErrUnjudged)
	}
	return nil
}

// usableIndex is the message's own name, or nothing when the platform gave none.
//
// The scene's index first because it is the documented one, then the quoted
// element's, which is where a quote of a quote names the message properly when the
// scene has fallen back to a temporary handle. Both are the same value for an
// ordinary quote, so only the temporary case can tell them apart.
func usableIndex(quoted feature.QuotedMessage) string {
	for _, index := range []string{quoted.Index, quoted.ElementIndex} {
		trimmed := strings.TrimSpace(index)
		if trimmed == "" || temporaryIndex(trimmed) {
			continue
		}
		return trimmed
	}
	return ""
}

// locateWindow finds a quoted message by what the quote showed of it and who the
// platform said sent it, then reads the window around it by the index that message
// really has.
//
// The index it returns is the point of the second return value: a message found this
// way is named in the window by its own index, not by the temporary one the quote
// carried, and anything that goes looking for the anchor has to be told which.
func (h *handler) locateWindow(ctx context.Context, groupOpenID string,
	quoted feature.QuotedMessage, span time.Duration) ([]CachedMessage, string, error) {
	if strings.TrimSpace(quoted.Text) == "" {
		return nil, "", ErrNotCached
	}
	located, err := h.cache.Locate(ctx, groupOpenID, quoted.Text, quoted.Author, locateWithin)
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
func (h *handler) JudgeQuoted(ctx context.Context, groupOpenID string,
	quoted feature.QuotedMessage, reporterOpenID string) (feature.ModerationVerdict, error) {
	report, err := h.judgeQuoted(ctx, groupOpenID, quoted)
	if errors.Is(err, feature.ErrAlreadyPunished) {
		// A second record would say a second judgement happened, and none did:
		// the answer is that this message was dealt with already, which the
		// record of the first judgement already says. Writing a row of errors
		// here would make "nobody looked" out of "there was nothing left to
		// look at", which is the confusion the verdict column exists to avoid.
		return report, err
	}
	return h.recordJudgement(ctx, groupOpenID, reporterOpenID, report, err), err
}

// judgeQuoted does the judging itself: find the window, ask the model, work out
// what the answer means.
//
// Everything that is not a judgement is an error. A cache miss and an unreadable
// answer both mean nobody knows whether the message is acceptable, and a caller
// that confused that with acceptable would quietly drop a real report.
func (h *handler) judgeQuoted(ctx context.Context, groupOpenID string,
	quoted feature.QuotedMessage) (feature.ModerationVerdict, error) {
	if strings.TrimSpace(quoted.Index) == "" && strings.TrimSpace(quoted.ElementIndex) == "" {
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
	chain, anchor, err := h.resolveWindow(ctx, groupOpenID, quoted)
	if err != nil {
		// Two answers are not "the judgement failed", and they go out as themselves:
		// a message that was already taken back, and a quote the platform did not
		// name well enough to tell apart from the rest. Wrapping either in "nobody
		// looked" would tell the group the opposite of what happened.
		if errors.Is(err, feature.ErrAlreadyPunished) ||
			errors.Is(err, feature.ErrUnidentifiedQuote) {
			return feature.ModerationVerdict{}, err
		}
		return feature.ModerationVerdict{}, fmt.Errorf("%w: %v", ErrUnjudged, err)
	}

	// The subject is the author of the message that was reported, not whoever is
	// loudest in the window: the report is about that message, and what follows
	// follows the message.
	subject, quotedID := "", ""
	reported := CachedMessage{}
	judged := make([]string, 0, len(chain))
	for _, message := range chain {
		if message.ID != "" {
			judged = append(judged, message.ID)
		}
		if message.Idx == anchor {
			subject, quotedID = message.User, message.ID
			reported = message
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

	// A picture nobody can look at is not something to judge around. The reported
	// message is mostly the picture, so a verdict picked out of whatever text
	// happened to sit next to it would be a verdict about a message nobody
	// reported -- and the honest answer, "this cannot be judged here", is one the
	// group can act on.
	if err := unjudgeablePicture(reported, h.config().Model.Vision); err != nil {
		return feature.ModerationVerdict{
			SubjectOpenID:    subject,
			QuotedMessageID:  quotedID,
			JudgedMessageIDs: judged,
		}, err
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
	report.Reasoning = verdict.Reasoning
	report.Model = verdict.Model
	report.RecallMessages = resolveRecall(chain, subject, verdict.Recall, quotedID, anchor)
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
