package admincmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// reportWaiting is what a member is told the moment they report something.
//
// Fixed wording, because it answers the one thing they cannot see: whether
// anything is happening at all. The judgement takes seconds, and silence would
// read as a command that does not work.
const reportWaiting = "我们正在获取相关上下文送检AI来判断违规行为，请稍候…"

// reportRateLimitPerHour is how often one member may report.
const reportRateLimitPerHour = 5

// reportCommand judges the message a member quoted.
//
// Any member may report, which is the point of the command: the people who see an
// advertisement are not only the administrators. Nothing else about managing the
// group is reachable this way.
func (h *handler) reportCommand(ctx context.Context,
	data *qqbotsdk.GroupMessageCreateData, _ parsedCommand) error {
	if h.moderation == nil {
		h.reply(ctx, data, "本机器人没有配置违规检测，无法送检。")
		return nil
	}
	reporter := senderOpenID(data)
	if !h.allowReport(reporter) {
		h.reply(ctx, data, "举报太频繁了，休息一下再来。")
		return nil
	}

	// The quoted message is named by the index the platform put on it, which is
	// the only handle a quote carries. The id needed to recall it is in the cache,
	// not in the event.
	quotedIndex, ok := data.MessageScene.ExtValue("ref_msg_idx")
	if !ok || strings.TrimSpace(quotedIndex) == "" {
		h.reply(ctx, data, "请**引用**要举报的那条消息，再发这个命令。")
		return nil
	}

	// Some quotes carry an index the cache can never hold -- a quote of a message that
	// is itself a quote comes with a temporary one -- and the judgement finds the
	// message by the quoted text instead. That happens behind this call: what is
	// passed in is a locator, never evidence, and the message it locates is judged as
	// its author's own words.

	// Written down because the platform's identifiers here are not what they
	// looked like: a quote names the message it points at, and that name turned out
	// not to be the one the same message carried when it arrived, so a lookup by it
	// found nothing even though the message was in the cache. What the quote
	// actually carries -- the ref_msg_idx, and the quoted author and text that come
	// with it -- is the only way to see the difference.
	message := ""
	author := ""
	if len(data.MsgElements) > 0 {
		message = data.MsgElements[0].Content
		if data.MsgElements[0].Author != nil {
			author = data.MsgElements[0].Author.MemberOpenID
		}
	}
	h.deps.Logger.Info("a report quoted a message",
		"group", data.GroupOpenID,
		"ref_msg_idx", quotedIndex,
		"quoted_author", author,
		"quoted_text", message)

	h.reply(ctx, data, reportWaiting)

	// On its own goroutine, with a context that outlives this handler: the handler
	// runs on the path that delivers every event, and a model call of several
	// seconds would stall every group behind it.
	go h.judgeReport(context.WithoutCancel(ctx), data.GroupOpenID, quotedIndex,
		message, reporter)
	return nil
}

// judgeReport runs one judgement and carries out whatever it asks for.
//
// quotedText is what the quote showed of the message it points at: a locator, never
// evidence. It exists because the platform names such a message twice -- the quote
// carries an index, and for a message that is itself a quote that index is temporary
// and useless, while the text it showed is what the message actually said.
func (h *handler) judgeReport(ctx context.Context, groupOpenID, quotedIndex,
	quotedText, reporter string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	verdict, err := h.moderation.JudgeQuoted(ctx, groupOpenID, quotedIndex,
		quotedText, reporter)
	if errors.Is(err, feature.ErrAlreadyPunished) {
		// The message is gone from the group, so there is nothing to judge and
		// nothing to answer about it. Said plainly, because the other wording --
		// "the judgement failed" -- would suggest the report was lost.
		h.deps.Logger.Info("a report was about a message that was already taken back",
			"group", groupOpenID, "reporter", reporter)
		h.sayInGroup(ctx, groupOpenID, "这条消息已经处理过了（已被撤回），不再重复判定。")
		return
	}
	if err != nil {
		// No judgement is not a clean verdict: nobody is touched, and the group is
		// told why rather than left wondering.
		h.deps.Logger.Warn("could not judge a reported message",
			"group", groupOpenID, "reporter", reporter, "error", err)
		h.sayInGroup(ctx, groupOpenID, "送检失败，未采取任何处理。")
		return
	}

	// What was done is recorded separately from what was decided, and it is
	// recorded however this function leaves. An outcome that never got written is
	// the one thing that would leave a punishment unauditable afterwards.
	action, muteSeconds := "none", int64(0)
	recallReason := ""
	var recalls []store.RecallOutcome
	defer func() {
		h.recordOutcome(ctx, verdict.JudgementID, store.Outcome{
			Action:       action,
			MuteSeconds:  muteSeconds,
			RecallReason: recallReason,
			Recalls:      recalls,
		})
	}()

	// The reason is the model's own words, so it goes to the log and nowhere else:
	// the group is told the configured label and nothing more.
	h.deps.Logger.Info("a reported message was judged",
		"group", groupOpenID,
		"reporter", reporter,
		"subject", verdict.SubjectOpenID,
		"category", verdict.Category,
		"label", verdict.Label,
		"mute_seconds", verdict.MuteSeconds,
		"quoted_message", verdict.QuotedMessageID,
		"judged_messages", len(verdict.JudgedMessageIDs),
		"model", verdict.Model,
		"reason", verdict.Reason,
		"receipt", verdict.JudgementID,
		"dry_run", h.moderation.DryRun())

	if verdict.Category == "" {
		// Nothing was found, so nobody's message was wrong. Whether that costs the
		// reporter anything is the group's policy, and the default is that it does
		// not.
		recallReason = "未发现违规，未执行撤回"
		penalty := h.moderation.ReportPenaltySeconds()
		if penalty <= 0 || h.moderation.DryRun() {
			h.sayInGroup(ctx, groupOpenID, "未发现违规，未采取任何处理。")
			return
		}
		duration := time.Duration(penalty) * time.Second
		if err := h.muteMember(ctx, groupOpenID, reporter, duration); err != nil {
			h.deps.Logger.Error("could not silence a reporter whose report found nothing",
				"group", groupOpenID, "reporter", reporter, "error", err)
			action = "report_penalty_failed"
			// The judgement's own outcome is stated first and without hedging: nothing
			// was found is the answer to the report. What could not be done comes after,
			// with the reason, because a penalty that cannot be carried out is the
			// platform's rule and not a failure to report.
			h.sayInGroup(ctx, groupOpenID, fmt.Sprintf(
				"未发现违规。本应禁言举报者 %d 秒，但未执行：%s。",
				penalty, shortReason(err)))
			return
		}
		action, muteSeconds = "report_penalty", penalty
		h.deps.Logger.Info("a report found nothing, so the reporter was silenced",
			"group", groupOpenID, "reporter", reporter, "seconds", penalty)
		h.sayInGroup(ctx, groupOpenID, "未发现违规。举报前请自行确认，已禁言举报者 "+
			humanDuration(duration)+"。")
		return
	}
	if h.moderation.DryRun() {
		action = "dry_run"
		recallReason = "试运行：未执行撤回"
		h.sayInGroup(ctx, groupOpenID, fmt.Sprintf(
			"【试运行】判定为【%s】。试运行期间不禁言、不撤回。",
			verdict.Label)+h.receiptSentence(verdict.JudgementID))
		return
	}

	// Both actions are attempted whatever the other does: a recall that fails for
	// want of an administrator must not cost the mute, and the mute is the part
	// that stops the next message.
	var notes []string
	recalled, failed := 0, 0
	var marked []string
	firstFailure := ""
	for _, message := range verdict.RecallMessages {
		outcome := store.RecallOutcome{
			ID:     message.ID,
			Number: message.Number,
			Text:   message.Text,
		}
		if err := h.deps.Client.RecallGroupMessage(ctx, groupOpenID, message.ID); err != nil {
			h.deps.Logger.Warn("could not recall a message that was judged",
				"group", groupOpenID, "message", message.ID, "number", message.Number,
				"error", err)
			outcome.Reason = shortReason(err)
			if firstFailure == "" {
				firstFailure = outcome.Reason
			}
			failed++
		} else {
			outcome.Recalled = true
			recalled++
			// Only a message that really went is marked as dealt with. One that
			// stayed is still in the group, still readable, and still something a
			// later report may legitimately be about.
			if message.Index != "" {
				marked = append(marked, message.Index)
			}
		}
		recalls = append(recalls, outcome)
	}
	// The group is told how many were taken back, never which ones.
	//
	// The numbers a judgement works in are positions in the window that was sent
	// for judging: "2" means the second message of about twenty, most of which came
	// before the one that was reported and none of which anybody in the group ever
	// saw as a numbered list. Printing them invites exactly the question the reply
	// is meant to answer -- somebody counted, and got a different number.
	switch {
	case recalled > 0 && failed == 0:
		notes = append(notes, fmt.Sprintf("已撤回 %d 条消息", recalled))
		recallReason = fmt.Sprintf("已撤回 %d 条消息", recalled)
	case recalled > 0:
		notes = append(notes, fmt.Sprintf("已撤回 %d 条消息，另有 %d 条撤回失败",
			recalled, failed))
		recallReason = fmt.Sprintf("已撤回 %d 条，另有 %d 条未撤回：%s",
			recalled, failed, firstFailure)
	case failed > 0:
		notes = append(notes, fmt.Sprintf("撤回失败（%d 条）", failed))
		recallReason = fmt.Sprintf("撤回失败（%d 条）：%s", failed, firstFailure)
	default:
		recallReason = "没有需要撤回的消息"
	}
	// Marked after the recall rather than before it, and only what came back
	// successful: the mark means "the group cannot read this any more", and
	// nothing else is true of a message that is still standing there.
	if len(marked) > 0 {
		if err := h.moderation.MarkPunished(ctx, groupOpenID, marked); err != nil {
			h.deps.Logger.Warn("could not mark the messages that were taken back",
				"group", groupOpenID, "error", err)
		}
	}
	if verdict.MuteSeconds > 0 && verdict.SubjectOpenID != "" {
		duration := time.Duration(verdict.MuteSeconds) * time.Second
		if err := h.muteMember(ctx, groupOpenID, verdict.SubjectOpenID, duration); err != nil {
			h.deps.Logger.Error("could not mute a member who was judged",
				"group", groupOpenID, "member", verdict.SubjectOpenID, "error", err)
			notes = append(notes, "禁言失败（"+shortReason(err)+"）")
		} else {
			notes = append(notes, "已禁言 "+humanDuration(duration))
		}
	}

	answer := "判定为【" + verdict.Label + "】"
	if len(notes) > 0 {
		answer += "，" + strings.Join(notes, "，")
	}
	answer += "。"
	// The receipt number is what makes the punishment answerable to somebody who
	// did not see this conversation: any administrator can ask the bot what the
	// number means, in the group or in a private message, and read the reason, the
	// chain of thought and the message that was taken back.
	answer += h.receiptSentence(verdict.JudgementID)
	action, muteSeconds = strings.Join(notes, " "), verdict.MuteSeconds
	h.sayInGroup(ctx, groupOpenID, answer)
}

// recordOutcome closes a judgement with what was actually done about it.
//
// A failure is reported and swallowed: the group has already been answered, and
// the record is for reviewing afterwards rather than for deciding now.
func (h *handler) recordOutcome(ctx context.Context, judgementID string,
	outcome store.Outcome) {
	if judgementID == "" || h.moderation == nil {
		return
	}
	if err := h.moderation.RecordOutcome(ctx, judgementID, outcome); err != nil {
		h.deps.Logger.Warn("could not record what followed a judgement",
			"judgement", judgementID, "action", outcome.Action, "error", err)
	}
}

// muteMember silences one member, which is the call the /禁言 command makes too.
func (h *handler) muteMember(ctx context.Context, groupOpenID, memberOpenID string,
	duration time.Duration) error {
	return h.deps.Client.SetGroupMemberMute(ctx, groupOpenID,
		&qqbotsdk.SetGroupMemberMuteRequest{
			Members: []qqbotsdk.SetMemberMuteState{{
				Op:           qqbotsdk.MemberMuteAdd,
				MemberOpenID: memberOpenID,
				MuteExpireAt: time.Now().Add(duration).Format(time.RFC3339),
			}},
		})
}

// sayInGroup sends a message that answers nothing, because a judgement finishes
// long after the report it is about did -- and long after the platform would let
// the bot reply to that report's event.
func (h *handler) sayInGroup(ctx context.Context, groupOpenID, text string) {
	if err := h.sendMessage(ctx, groupOpenID, text, ""); err != nil {
		h.deps.Logger.Warn("could not report a judgement in the group",
			"group", groupOpenID, "error", err)
	}
}

// allowReport is the rate limit: so many reports from one member an hour.
//
// Counted in memory, and so forgotten by a restart, which is the right amount of
// effort for something whose job is to stop one person flooding the model rather
// than to enforce a rule.
func (h *handler) allowReport(reporter string) bool {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reports == nil {
		h.reports = map[string][]time.Time{}
	}
	kept := make([]time.Time, 0, len(h.reports[reporter]))
	for _, at := range h.reports[reporter] {
		if now.Sub(at) < time.Hour {
			kept = append(kept, at)
		}
	}
	allowed := len(kept) < reportRateLimitPerHour
	if allowed {
		kept = append(kept, now)
	}
	h.reports[reporter] = kept
	return allowed
}

// shortReason is an error message fit to put in a group: the platform's wording
// is long and often carries an identifier nobody there can use.
// platformReason turns a platform refusal into something a group can read.
//
// The raw error is a request line, an HTTP status, an error code and a trace id:
// accurate, and useless to the people reading it. What matters is the one fact a
// member can act on, and the code for it is stable enough to name.
func shortReason(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())

	// Measured, from a real refusal: the platform will not silence a bot, the group's
	// owner or an administrator. Saying so matters, because the alternative message
	// looks like the bot broke rather than like a rule of the platform's.
	if strings.Contains(text, "40103004") {
		return "平台不允许禁言群主或管理员"
	}

	// Otherwise the code and its own wording, with the request line and the trace id
	// taken off: those belong in the log, not in a group.
	if index := strings.Index(text, "err_code"); index >= 0 {
		text = strings.TrimSpace(text[index:])
	}
	if index := strings.Index(text, "[trace_id"); index >= 0 {
		text = strings.TrimSpace(text[:index])
	}
	return shortened(text, 80)
}

// shortened keeps a message readable, counting characters rather than bytes so it
// cannot cut one in half.
func shortened(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
