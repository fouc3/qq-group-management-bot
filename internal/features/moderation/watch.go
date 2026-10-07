package moderation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// The high risk list is the members whose messages are judged as they arrive.
//
// It covers the case a report cannot. A report is somebody noticing, and somebody
// noticing needs the group to still be reading when the advertisement appears; a
// marked member is judged whether anybody is watching or not, which is what a
// group wants for an account it has already caught once.
//
// What it is not is a second moderation path. The window, the judge, the record and
// the punishment are the ones a report reaches; the differences are that nobody
// asked and that nothing is said in the group -- what the group would have been
// told is in the record instead, which is where an administrator looks afterwards.

// on reports whether the list is turned on. A block written without the switch is
// on, like the feature's own: writing it out is already a statement of intent.
func (r HighRisk) on() bool { return r.Enabled == nil || *r.Enabled }

// autoMarkFor is how long a mark nobody chose lasts.
func (r HighRisk) autoMarkFor() time.Duration {
	parsed, err := time.ParseDuration(r.AutoMarkFor)
	if err != nil {
		// Refused at startup, so this cannot happen in a running bot. A mark that
		// lasts an hour is better than one that lasts for ever, which is what the
		// zero would otherwise become.
		return time.Hour
	}
	return parsed
}

// watchStore is the list as the data layer keeps it, or nil when this deployment
// has none.
func (h *handler) watchStore() store.WatchStore {
	if h.deps.Store == nil {
		return nil
	}
	return h.deps.Store.Watches()
}

// loadWatches reads the list into memory, which is where the message path asks
// about it.
func (h *handler) loadWatches(ctx context.Context) {
	watches := h.watchStore()
	if watches == nil {
		h.deps.Logger.Warn("no data layer is configured, so the high risk list is " +
			"unavailable and nobody is judged as they speak")
		return
	}
	entries, err := watches.List(ctx, time.Now())
	if err != nil {
		// Starting with an empty list would leave the members somebody marked
		// unjudged while the log said nothing, which reads as the list having been
		// emptied on purpose.
		h.deps.Logger.Error("could not read the high risk list, so nobody is being "+
			"judged as they speak", "error", err)
		return
	}
	h.watchMu.Lock()
	h.watched = make(map[string]int64, len(entries))
	for _, entry := range entries {
		h.watched[entry.MemberOpenID] = entry.ExpiresAt
	}
	h.watchMu.Unlock()
	if len(entries) > 0 {
		h.deps.Logger.Info("the high risk list is in force", "members", len(entries))
		return
	}
	h.deps.Logger.Info("the high risk list is empty, so nobody is judged as they speak")
}

// isWatched reports whether this member's messages are to be judged as they
// arrive.
//
// The list is consulted from memory, because it is asked about every message a
// group receives, and answered under the lock rather than with a query: what a
// query per message would cost is the busiest thing this feature does.
func (h *handler) isWatched(memberOpenID string) bool {
	if memberOpenID == "" || !h.config().HighRisk.on() {
		return false
	}
	now := time.Now().Unix()
	h.watchMu.Lock()
	defer h.watchMu.Unlock()
	until, found := h.watched[memberOpenID]
	if !found {
		return false
	}
	if until <= now {
		// Run out, and dropped as it is found: the row is still in the data layer
		// until somebody reads the list, and a mark that has ended must not go on
		// being enforced from memory in the meantime.
		delete(h.watched, memberOpenID)
		return false
	}
	return true
}

// remember puts one mark into the in-memory list, replacing any earlier one.
func (h *handler) remember(memberOpenID string, expiresAt int64) {
	if memberOpenID == "" {
		return
	}
	h.watchMu.Lock()
	defer h.watchMu.Unlock()
	if h.watched == nil {
		h.watched = map[string]int64{}
	}
	if expiresAt <= time.Now().Unix() {
		delete(h.watched, memberOpenID)
		return
	}
	h.watched[memberOpenID] = expiresAt
}

// forget drops one mark from the in-memory list.
func (h *handler) forget(memberOpenID string) {
	h.watchMu.Lock()
	defer h.watchMu.Unlock()
	delete(h.watched, memberOpenID)
}

// Watch implements feature.Moderation: it marks one member, replacing any earlier
// mark for them.
func (h *handler) Watch(ctx context.Context, entry store.Watch) error {
	if !h.config().HighRisk.on() {
		return errors.New("high_risk is turned off, so nobody can be marked")
	}
	watches := h.watchStore()
	if watches == nil {
		return errors.New("no data layer is configured, so the high risk list " +
			"cannot be kept")
	}
	if entry.ID == "" {
		id, err := newWatchID()
		if err != nil {
			return err
		}
		entry.ID = id
	}
	if entry.AddedAt == 0 {
		entry.AddedAt = time.Now().Unix()
	}
	if err := watches.Add(ctx, entry); err != nil {
		return err
	}
	h.remember(entry.MemberOpenID, entry.ExpiresAt)
	return nil
}

// Unwatch implements feature.Moderation: it takes one member off the list.
//
// It works whether or not the list is turned on: a member marked before the switch
// went off is one somebody may well want to take off, and refusing to remove an
// entry is not how a list is emptied.
func (h *handler) Unwatch(ctx context.Context, memberOpenID string) (bool, error) {
	watches := h.watchStore()
	if watches == nil {
		return false, errors.New("no data layer is configured, so the high risk " +
			"list cannot be read")
	}
	removed, err := watches.Remove(ctx, memberOpenID)
	if err != nil {
		return false, err
	}
	h.forget(memberOpenID)
	return removed, nil
}

// Watches implements feature.Moderation: it lists the marks still in force.
func (h *handler) Watches(ctx context.Context) ([]store.Watch, error) {
	watches := h.watchStore()
	if watches == nil {
		return nil, errors.New("no data layer is configured, so the high risk list " +
			"cannot be read")
	}
	return watches.List(ctx, time.Now())
}

// newWatchID returns the key a mark is stored under.
//
// Random rather than the member's openid, so that a mark which is replaced and
// then made again is not the same row by accident: the id is what the log of a
// decision refers to, and two decisions are two decisions.
func newWatchID() (string, error) {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generating a high risk id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// judgeWatcher judges one message from a marked member, in the background.
//
// In the background because a judgement is several seconds of a model call, and
// the event that carried the message is the path every other feature's work runs
// on. The room for these is bounded: every message of a marked member is judged,
// so a marked member talking quickly would otherwise open as many model calls as
// they have messages, which is a bill rather than a policy. What does not fit is
// said out loud and skipped.
func (h *handler) judgeWatcher(groupOpenID string, message CachedMessage) {
	select {
	case h.judging <- struct{}{}:
	default:
		h.deps.Logger.Warn("a message from a high risk member was not judged: as "+
			"many judgements are already running as this feature allows",
			"group", groupOpenID, "member", message.User, "message", message.ID,
			"max_in_flight", h.config().HighRisk.MaxInFlight)
		return
	}
	h.judgingOut.Add(1)
	go func() {
		defer func() {
			<-h.judging
			h.judgingOut.Done()
		}()
		// The feature's own context, so that an instance built again stops judging
		// rather than answering on behalf of a configuration nobody is running.
		timeout := time.Duration(h.config().Model.TimeoutSeconds)*time.Second +
			10*time.Second
		ctx, cancel := context.WithTimeout(h.part, timeout)
		defer cancel()
		h.judgeWatched(ctx, groupOpenID, message)
	}()
}

// judgeWatched reads the window around one marked member's message, judges it, and
// carries out what the judgement found.
//
// The window rather than the message alone, and the same window a report would
// get: an advertisement split across several messages is exactly what a marked
// member is likely to send, and it is invisible one message at a time.
func (h *handler) judgeWatched(ctx context.Context, groupOpenID string, message CachedMessage) {
	log := h.deps.Logger.With("group", groupOpenID, "member", message.User,
		"message", message.ID)
	if !h.config().JudgingEnabled() || !h.config().judgingEnabledFor(groupOpenID) {
		log.Debug("a high risk member was not judged: nothing judges this group")
		return
	}
	if h.config().senderExempt(groupOpenID, message.User) {
		// The one exemption, and it is about identity rather than content. A group
		// that has put this sender beyond judging means it here too, or the
		// exemption would be a rule that depends on how the message was noticed.
		log.Debug("a high risk member is exempt in this group, so nothing was judged")
		return
	}

	span := time.Duration(h.config().ChainMinutes) * time.Minute
	chain, err := h.cache.Context(ctx, groupOpenID, message.Idx,
		h.config().ContextBefore, h.config().ContextAfter, span)
	if err != nil {
		log.Warn("could not read the window around a high risk member's message, "+
			"so it was not judged", "error", err)
		return
	}
	judged := make([]string, 0, len(chain))
	for _, one := range chain {
		if one.ID != "" {
			judged = append(judged, one.ID)
		}
	}

	verdict, judgeErr := h.Judge(ctx, groupOpenID, chain)
	// Built before the error is looked at, so that a judgement which could not be
	// reached still records who it was about -- the same reason the report path
	// builds it there.
	report := feature.ModerationVerdict{
		SubjectOpenID:    message.User,
		QuotedMessageID:  message.ID,
		JudgedMessageIDs: judged,
	}
	if judgeErr != nil {
		h.recordJudgement(ctx, groupOpenID, "", report, judgeErr)
		log.Warn("could not judge a high risk member's message", "error", judgeErr)
		return
	}
	report.Category = verdict.Category
	report.Reason = verdict.Reason
	report.Reasoning = verdict.Reasoning
	report.Model = verdict.Model
	// Only this member's own messages: the judge is shown the whole window and may
	// name any of it, and what is being dealt with is what the marked member said.
	report.RecallMessages = resolveRecall(chain, message.User, verdict.Recall,
		message.ID, message.Idx)
	if !verdict.Violation() {
		h.recordJudgement(ctx, groupOpenID, "", report, nil)
		log.Debug("a high risk member's message was judged and found acceptable")
		return
	}

	report.Label = h.config().LabelFor(verdict.Category)
	seconds, known := h.config().MuteForGroup(groupOpenID, verdict.Category)
	if !known {
		// The category came from the configuration's own list, so this cannot
		// happen in a running bot; saying so beats punishing somebody for a
		// category with no duration.
		h.recordJudgement(ctx, groupOpenID, "", report,
			fmt.Errorf("%w: category %q has no duration configured", ErrUnjudged,
				verdict.Category))
		log.Error("a high risk member's message broke a rule with no duration",
			"category", verdict.Category)
		return
	}
	report.MuteSeconds = seconds

	recorded := h.recordJudgement(ctx, groupOpenID, "", report, nil)
	log.Warn("a high risk member's message was judged a violation",
		"label", report.Label, "receipt", recorded.JudgementID,
		"mute_seconds", report.MuteSeconds, "recalls", len(report.RecallMessages))
	h.applyOnWatched(ctx, groupOpenID, recorded, log)
}

// applyOnWatched does what the judgement asks for, silently.
//
// The acts are the ones a report leads to -- the messages the judge named are taken
// back and the member is silenced for the category's duration -- and what is left
// out is everything that was about being answered: no reply, no receipt in the
// group, and no penalty for a reporter, because there is no reporter.
func (h *handler) applyOnWatched(ctx context.Context, groupOpenID string,
	report feature.ModerationVerdict, log *slog.Logger) {
	if ctx.Err() != nil {
		// The feature stopped while the model was answering, so the answer is
		// nobody's to act on. It is recorded already, which is what keeps the
		// judgement auditable rather than lost.
		log.Info("a judgement of a high risk member was abandoned: the feature stopped",
			"receipt", report.JudgementID)
		return
	}
	if h.DryRun() {
		h.finishWatched(ctx, report, store.Outcome{
			Action:       "dry_run",
			RecallReason: "试运行：未执行撤回",
		}, log)
		return
	}

	var notes []string
	recalled, failed := 0, 0
	firstFailure := ""
	recalls := make([]store.RecallOutcome, 0, len(report.RecallMessages))
	var marked []string
	for _, message := range report.RecallMessages {
		outcome := store.RecallOutcome{
			ID:     message.ID,
			Number: message.Number,
			Text:   message.Text,
		}
		if err := h.deps.Client.RecallGroupMessage(ctx, groupOpenID, message.ID); err != nil {
			outcome.Reason = recallReason(err)
			if firstFailure == "" {
				firstFailure = outcome.Reason
			}
			failed++
			log.Warn("could not recall a message a judgement named",
				"message", message.ID, "number", message.Number, "error", err)
		} else {
			outcome.Recalled = true
			recalled++
			// Only what really went is marked as dealt with: a message that stayed
			// is still in the group, still readable, and still something a later
			// judgement may be about.
			if message.Index != "" {
				marked = append(marked, message.Index)
			}
		}
		recalls = append(recalls, outcome)
	}
	if len(marked) > 0 {
		if err := h.MarkPunished(ctx, groupOpenID, marked); err != nil {
			log.Warn("could not mark the messages that were taken back", "error", err)
		}
	}
	switch {
	case recalled > 0 && failed == 0:
		notes = append(notes, fmt.Sprintf("已撤回 %d 条消息", recalled))
	case recalled > 0:
		notes = append(notes, fmt.Sprintf("已撤回 %d 条消息，另有 %d 条撤回失败",
			recalled, failed))
	case failed > 0:
		notes = append(notes, fmt.Sprintf("撤回失败（%d 条）", failed))
	}

	muteSeconds := report.MuteSeconds
	if muteSeconds > 0 && report.SubjectOpenID != "" {
		duration := time.Duration(muteSeconds) * time.Second
		if err := h.muteWatched(ctx, groupOpenID, report.SubjectOpenID, duration); err != nil {
			log.Error("could not silence a member a judgement found violating",
				"error", err)
			notes = append(notes, "禁言失败（"+recallReason(err)+"）")
			muteSeconds = 0
		} else {
			notes = append(notes, "已禁言 "+humanDuration(duration))
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "无可执行动作")
	}

	outcome := store.Outcome{
		Action:       "自动送检：" + strings.Join(notes, " "),
		MuteSeconds:  muteSeconds,
		RecallReason: recallSentence(recalled, failed, firstFailure),
		Recalls:      recalls,
	}
	h.finishWatched(ctx, report, outcome, log)
}

// finishWatched closes the record with what was actually done.
func (h *handler) finishWatched(ctx context.Context, report feature.ModerationVerdict,
	outcome store.Outcome, log *slog.Logger) {
	if err := h.RecordOutcome(ctx, report.JudgementID, outcome); err != nil {
		log.Warn("could not record what followed a judgement",
			"receipt", report.JudgementID, "error", err)
	}
	log.Info("a judgement of a high risk member was carried out",
		"receipt", report.JudgementID, "action", outcome.Action)
}

// muteWatched silences one member, recording the mute as this bot's own.
//
// The record matters as much here as anywhere: the feature that lifts the mutes
// other people apply has to be able to tell this one apart from a group
// administrator's.
func (h *handler) muteWatched(ctx context.Context, groupOpenID, memberOpenID string,
	duration time.Duration) error {
	until := time.Now().Add(duration)
	if err := h.deps.Client.SetGroupMemberMute(ctx, groupOpenID,
		&qqbotsdk.SetGroupMemberMuteRequest{
			Members: []qqbotsdk.SetMemberMuteState{{
				Op:           qqbotsdk.MemberMuteAdd,
				MemberOpenID: memberOpenID,
				MuteExpireAt: until.Format(time.RFC3339),
			}},
		}); err != nil {
		return err
	}
	h.deps.Mutes.Record(groupOpenID, memberOpenID, until)
	return nil
}

// recallSentence says why the messages were or were not taken back, for the record.
func recallSentence(recalled, failed int, firstFailure string) string {
	switch {
	case recalled > 0 && failed == 0:
		return fmt.Sprintf("已撤回 %d 条消息", recalled)
	case recalled > 0:
		return fmt.Sprintf("已撤回 %d 条，另有 %d 条未撤回：%s", recalled, failed, firstFailure)
	case failed > 0:
		return fmt.Sprintf("撤回失败（%d 条）：%s", failed, firstFailure)
	default:
		return "没有需要撤回的消息"
	}
}

// recallReason shortens a platform refusal for the record.
//
// It says the same words the report path does, deliberately: an operator reading
// either row has to recognise the same refusal, and the one refusal worth naming is
// the platform refusing to silence a group's own administrator.
func recallReason(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	if strings.Contains(text, "40103004") {
		return "平台不允许禁言群主或管理员"
	}
	// The code and its own wording, with the request line and the trace id taken
	// off: those belong in the log rather than in a record somebody reads later.
	if start := strings.Index(text, "err_code"); start >= 0 {
		text = text[start:]
	}
	if end := strings.Index(text, " [trace_id="); end >= 0 {
		text = text[:end]
	}
	if len([]rune(text)) > 120 {
		text = string([]rune(text)[:120]) + "…"
	}
	return text
}

// humanDuration renders a duration the way the group is told it, in the same words
// the report path uses.
func humanDuration(duration time.Duration) string {
	switch {
	case duration >= 24*time.Hour && duration%(24*time.Hour) == 0:
		return fmt.Sprintf("%d天", int(duration/(24*time.Hour)))
	case duration >= time.Hour && duration%time.Hour == 0:
		return fmt.Sprintf("%d小时", int(duration/time.Hour))
	case duration >= time.Minute && duration%time.Minute == 0:
		return fmt.Sprintf("%d分钟", int(duration/time.Minute))
	default:
		return fmt.Sprintf("%d秒", int(duration/time.Second))
	}
}

// markIfCaughtEnough marks a member nobody marked, once they have been caught
// breaking a rule as many times as the configuration says.
//
// It runs from the record rather than from either path that reaches it, so that a
// report and an automatic judgement both count: what it is counting is judgements,
// and the question "has this happened before" must not depend on how the last one
// was noticed.
func (h *handler) markIfCaughtEnough(ctx context.Context, memberOpenID string) {
	rule := h.config().HighRisk
	if rule.AutoMarkAfter <= 0 || !rule.on() || memberOpenID == "" {
		return
	}
	if h.isWatched(memberOpenID) {
		// Already marked, by hand or by this rule: a second mark would only move
		// the moment the first one ends.
		return
	}
	judgements := h.judgementStore()
	if judgements == nil {
		return
	}
	count, err := judgements.CountViolations(ctx, memberOpenID, time.Time{})
	if err != nil {
		h.deps.Logger.Warn("could not count a member's violations, so they were "+
			"not marked automatically", "member", memberOpenID, "error", err)
		return
	}
	if count < rule.AutoMarkAfter {
		return
	}
	until := time.Now().Add(rule.autoMarkFor())
	entry := store.Watch{
		MemberOpenID: memberOpenID,
		Reason:       fmt.Sprintf("自动标记：累计 %d 次违规判定", count),
		AddedBy:      "",
		ExpiresAt:    until.Unix(),
	}
	if err := h.Watch(ctx, entry); err != nil {
		h.deps.Logger.Warn("could not mark a member automatically", "member",
			memberOpenID, "violations", count, "error", err)
		return
	}
	h.deps.Logger.Warn("a member was marked high risk automatically, so their "+
		"messages are judged as they arrive",
		"member", memberOpenID, "violations", count,
		"until", until.Format(time.RFC3339))
}
