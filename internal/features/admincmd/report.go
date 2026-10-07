package admincmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
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
	data *qqbotsdk.GroupMessageCreateData, _ command.Parsed) error {
	if h.moderation == nil {
		h.reply(ctx, data, "本机器人没有配置违规检测，无法送检。")
		return nil
	}
	reporter := senderOpenID(data)
	if !h.allowReport(reporter) {
		h.reply(ctx, data, "举报太频繁了，休息一下再来。")
		return nil
	}

	// The quoted message is named by the platform in two places, and either may be
	// the one that works. The scene context carries ref_msg_idx for the message the
	// quote points at; the element that carries the quoted content carries msg_idx
	// for the same message. An ordinary quote has the same value in both, so only a
	// quote of a message that is itself a quote can tell them apart -- and there the
	// scene falls back to a temporary index the cache can never hold, which is
	// exactly the case that has to have somewhere else to look.
	quoted := feature.QuotedMessage{
		Index: strings.TrimSpace(ext(data, "ref_msg_idx")),
	}
	if len(data.MsgElements) > 0 {
		quoted.ElementIndex = strings.TrimSpace(data.MsgElements[0].MsgIdx)
		quoted.Text = data.MsgElements[0].Content
		if data.MsgElements[0].Author != nil {
			// Named only sometimes, and never in production so far. It is what makes
			// the text usable at all, so it is carried rather than dropped.
			quoted.Author = data.MsgElements[0].Author.MemberOpenID
		}
	}
	if quoted.Index == "" && quoted.ElementIndex == "" {
		h.reply(ctx, data, "请**引用**要举报的那条消息，再发这个命令。")
		return nil
	}

	// Written down because the platform's identifiers here are not what they
	// looked like: a quote names the message it points at, and that name turned out
	// not to be the one the same message carried when it arrived, so a lookup by it
	// found nothing even though the message was in the cache. Both names, and the
	// author, are what make that visible afterwards -- and which one the judgement
	// ends up using is the answer to whether the element's name can rescue the
	// cases the scene's cannot.
	h.deps.Logger.Info("a report quoted a message",
		"group", data.GroupOpenID,
		"ref_msg_idx", quoted.Index,
		"element_msg_idx", quoted.ElementIndex,
		"quoted_author", quoted.Author,
		"quoted_text", quoted.Text)

	h.reply(ctx, data, reportWaiting)

	// On its own goroutine, under the feature's own context: the handler runs on
	// the path that delivers every event, and a model call of several seconds
	// would stall every group behind it. The feature's context rather than the
	// request's, so that the judgement ends with the feature -- a judgement still
	// running against an instance that has been replaced is a receipt posted for
	// a command nobody is holding any more.
	go h.judgeReport(h.part, data.GroupOpenID, quoted, reporter)
	return nil
}

// ext returns one value from the message's scene context, or nothing.
func ext(data *qqbotsdk.GroupMessageCreateData, key string) string {
	if data.MessageScene == nil {
		return ""
	}
	value, _ := data.MessageScene.ExtValue(key)
	return value
}

// judgeReport runs one judgement and carries out whatever it asks for.
//
// quoted is how the report names the message it is about: two possible indexes and
// the author, and no assumption about which of them will work. Text travels with
// them as a locator and never as evidence -- the message it locates is judged as its
// author's own words.
func (h *handler) judgeReport(ctx context.Context, groupOpenID string,
	quoted feature.QuotedMessage, reporter string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	verdict, err := h.moderation.JudgeQuoted(ctx, groupOpenID, quoted, reporter)
	if errors.Is(err, feature.ErrUnidentifiedQuote) {
		// A report that names no message could not be resolved, which is neither a
		// failure to judge nor something the reporter can fix by reporting the same
		// message again. Said in those words, with the receipt so an administrator
		// can look at what the platform actually sent.
		h.deps.Logger.Warn("a report quoted a message that cannot be identified",
			"group", groupOpenID, "reporter", reporter,
			"ref_msg_idx", quoted.Index, "element_msg_idx", quoted.ElementIndex,
			"quoted_author", quoted.Author, "error", err,
			"receipt", verdict.JudgementID)
		h.sayInGroup(ctx, groupOpenID,
			"这条引用机器人拿不到可用的消息编号（被举报的多半是转发/合并消息，或"+
				"引用了一条本身就是引用的消息），无法确定它是哪一条，所以这次不判定。"+
				"可以改为直接引用对方那条普通消息再举报。"+
				h.receiptSentence(verdict.JudgementID))
		return
	}
	if errors.Is(err, feature.ErrAlreadyPunished) {
		// The message is gone from the group, so there is nothing to judge and
		// nothing to answer about it. Said plainly, because the other wording --
		// "the judgement failed" -- would suggest the report was lost.
		h.deps.Logger.Info("a report was about a message that was already taken back",
			"group", groupOpenID, "reporter", reporter)
		h.sayInGroup(ctx, groupOpenID, "这条消息已经处理过了（已被撤回），不再重复判定。")
		return
	}
	if errors.Is(err, feature.ErrPictureNotJudgeable) {
		// The message is a picture and the model is not allowed to be shown
		// pictures. Named as its own answer rather than as a failed judgement:
		// nothing went wrong here, and a reporter told "送检失败" would keep trying,
		// because nothing in that sentence says an image cannot be reported in this
		// group at all.
		h.deps.Logger.Info("a report was about a picture, which this deployment "+
			"does not judge", "group", groupOpenID, "reporter", reporter,
			"error", err, "receipt", verdict.JudgementID)
		h.sayInGroup(ctx, groupOpenID,
			"无法举报图片消息：这条消息带图，而本群没有开启识图判定"+
				"（moderation.model.vision），看不到图就没法判。"+
				h.receiptSentence(verdict.JudgementID))
		return
	}
	if err != nil {
		// No judgement is not a clean verdict: nobody is touched, and the group is
		// told why rather than left wondering.
		//
		// The receipt goes on anyway. A judgement that could not be reached is
		// recorded too -- "nobody looked" and "nothing was found" are different
		// facts -- and the number is the only way an administrator can find out
		// afterwards which of the two happened and why.
		h.deps.Logger.Warn("could not judge a reported message",
			"group", groupOpenID, "reporter", reporter, "error", err,
			"receipt", verdict.JudgementID)
		h.sayInGroup(ctx, groupOpenID,
			"送检失败，未采取任何处理。"+h.receiptSentence(verdict.JudgementID))
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
			h.sayInGroup(ctx, groupOpenID,
				"未发现违规，未采取任何处理。"+h.receiptSentence(verdict.JudgementID))
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
				penalty, shortReason(err))+h.receiptSentence(verdict.JudgementID))
			return
		}
		action, muteSeconds = "report_penalty", penalty
		h.deps.Logger.Info("a report found nothing, so the reporter was silenced",
			"group", groupOpenID, "reporter", reporter, "seconds", penalty)
		h.sayInGroup(ctx, groupOpenID, "未发现违规。举报前请自行确认，已禁言举报者 "+
			humanDuration(duration)+"。"+h.receiptSentence(verdict.JudgementID))
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
	// A report about the reporter's own message is answered the same way, with
	// half the silence: the message is still wrong and still goes, and the member
	// who pointed at it has saved somebody else the trouble of doing it. Half
	// rather than none, because reporting yourself is not a way out of being
	// reported.
	//
	// The group is told, because a duration that does not match what the
	// configuration says would otherwise look like a bot that cannot count.
	muteFor := verdict.MuteSeconds
	selfReported := reporter != "" && reporter == verdict.SubjectOpenID
	if selfReported && muteFor > 0 {
		muteFor /= 2
		if muteFor < 1 {
			muteFor = 1
		}
		h.deps.Logger.Info("the reporter reported themselves, so the silence is halved",
			"group", groupOpenID, "member", verdict.SubjectOpenID,
			"seconds", verdict.MuteSeconds, "applied", muteFor)
		notes = append(notes, "自己举报自己，刑期减半")
	}
	if muteFor > 0 && verdict.SubjectOpenID != "" {
		duration := time.Duration(muteFor) * time.Second
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
	action, muteSeconds = strings.Join(notes, " "), muteFor
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
