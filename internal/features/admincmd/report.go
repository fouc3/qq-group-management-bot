package admincmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
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

	h.reply(ctx, data, reportWaiting)

	// On its own goroutine, with a context that outlives this handler: the handler
	// runs on the path that delivers every event, and a model call of several
	// seconds would stall every group behind it.
	go h.judgeReport(context.WithoutCancel(ctx), data.GroupOpenID, quotedIndex, reporter)
	return nil
}

// judgeReport runs one judgement and carries out whatever it asks for.
func (h *handler) judgeReport(ctx context.Context, groupOpenID, quotedIndex,
	reporter string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	verdict, err := h.moderation.JudgeQuoted(ctx, groupOpenID, quotedIndex)
	if err != nil {
		// No judgement is not a clean verdict: nobody is touched, and the group is
		// told why rather than left wondering.
		h.deps.Logger.Warn("could not judge a reported message",
			"group", groupOpenID, "reporter", reporter, "error", err)
		h.sayInGroup(ctx, groupOpenID, "送检失败，未采取任何处理。")
		return
	}

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
		"dry_run", h.moderation.DryRun())

	if verdict.Category == "" {
		h.sayInGroup(ctx, groupOpenID, "未发现违规，未采取任何处理。")
		return
	}
	if h.moderation.DryRun() {
		h.sayInGroup(ctx, groupOpenID, fmt.Sprintf(
			"【试运行】判定为【%s】。试运行期间不禁言、不撤回。", verdict.Label))
		return
	}

	// Both actions are attempted whatever the other does: a recall that fails for
	// want of an administrator must not cost the mute, and the mute is the part
	// that stops the next message.
	var notes []string
	if verdict.QuotedMessageID != "" {
		if err := h.deps.Client.RecallGroupMessage(ctx, groupOpenID,
			verdict.QuotedMessageID); err != nil {
			h.deps.Logger.Warn("could not recall a reported message",
				"group", groupOpenID, "message", verdict.QuotedMessageID, "error", err)
			notes = append(notes, "撤回失败（"+shortReason(err)+"）")
		} else {
			notes = append(notes, "已撤回消息")
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
	h.sayInGroup(ctx, groupOpenID, answer+"。")
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

// sayInGroup sends a message that is not an answer to anything, because the
// judgement finishes long after the report did.
func (h *handler) sayInGroup(ctx context.Context, groupOpenID, text string) {
	if _, err := h.deps.Client.SendGroupMessage(ctx, groupOpenID, &qqbotsdk.Message{
		MsgType:  qqbotsdk.MsgTypeMarkdown,
		Markdown: &qqbotsdk.MessageMarkdown{Content: text},
	}); err != nil {
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
func shortReason(err error) string {
	text := strings.TrimSpace(err.Error())
	if len(text) > 60 {
		return text[:60] + "…"
	}
	return text
}
