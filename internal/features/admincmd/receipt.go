package admincmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// receiptLength is how much of a judgement id a group is shown.
//
// The id is the primary key, so it is unique and unstable on purpose: shortening
// it is what makes a receipt something a person can read off a screen and type
// back. Eight characters of a random one are far more than a group will ever
// produce, and the lookup accepts any prefix, so a receipt that did collide
// still works when the whole id is given.
const receiptLength = 8

// receiptShort is the form shown to a group and accepted back as an argument.
func receiptShort(id string) string {
	trimmed := strings.TrimSpace(id)
	if len(trimmed) <= receiptLength {
		return trimmed
	}
	// Byte slicing is safe here: an id is generated as lowercase hex.
	return trimmed[:receiptLength]
}

// commandInput is the platform's tappable command chip.
//
// Tapping it writes text into the input box and sends nothing: the member still
// decides. That is exactly what a receipt needs. The group sees a short number,
// and the administrator who wants to know what it stood for gets the whole
// command without typing a Chinese command name and eight hex characters by hand,
// which is the difference between a receipt people look up and one they don't.
//
// Both values are percent-encoded, because the platform requires it and the
// failure is silent: an unencoded value containing a slash or a space arrives cut
// short at the first one.
func commandInput(text, show string) string {
	return `<qqbot-cmd-input text="` + urlEncode(text) + `" show="` + urlEncode(show) +
		`" reference="false" />`
}

// urlEncode percent-encodes a value for an embedded tag.
//
// QueryEscape is the wrong function on its own: it writes a space as "+", and
// this is not a query string, so a plus would arrive as a plus.
func urlEncode(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

// receiptSentence is the part of a group's answer that carries the receipt
// number, or nothing when there is no record to point at.
//
// One function because two answers need it -- the one after a violation and the
// one a dry run gets -- and because a sentence assembled twice is a sentence that
// drifts: a chip in one and a bare number in the other would look like two
// different features.
func (h *handler) receiptSentence(judgementID string) string {
	receipt := receiptShort(judgementID)
	if receipt == "" {
		return ""
	}
	return "回执单号 " + commandInput(h.cfg.Prefix+"违规查询 "+receipt, receipt) + "。"
}

// receiptNames are the command names that ask for one receipt.
//
// Written once because the group path and the private path have to answer to
// exactly the same words: a name that worked in a group and not in a single
// chat would look like the feature being broken rather than being scoped.
var receiptNames = []string{"违规查询", "回执", "receipt", "violation"}

// receiptCommand answers /违规查询 in the group it was asked in.
//
// It is reached only from run(), which is already behind the administrator
// check, and it answers about this group's records and no others: an
// administrator of one group has no business reading another group's
// punishments, and a group is the one place where a reply is read by people who
// are not administrators at all.
func (h *handler) receiptCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	command parsedCommand) error {
	ask := strings.TrimSpace(strings.Join(command.args, " "))
	if ask == "" {
		h.reply(ctx, data, "用法：/违规查询 <回执单号>。回执单号在每次判定为违规时会一起发出来。")
		return nil
	}

	entry, err := h.findJudgement(ctx, ask)
	if err != nil {
		h.reply(ctx, data, receiptProblem(ask, err))
		return nil
	}
	if entry.GroupOpenID != data.GroupOpenID {
		// Said as "no such record here" rather than "that is another group's":
		// the second sentence tells an ordinary member that the number exists.
		// The administrator asking is logged either way.
		h.deps.Logger.Warn("refused a receipt from another group",
			"group", data.GroupOpenID, "member", data.Author.MemberOpenID,
			"receipt", ask, "belongs_to", entry.GroupOpenID)
		h.reply(ctx, data, "本群没有这条记录。回执单号只能在它所属的群里查，或者私聊机器人查。")
		return nil
	}
	h.reply(ctx, data, h.receiptText(entry))
	return nil
}

// onPrivateMessage handles a message in a single chat.
//
// It is the same handler shape as the group one, minus everything that belongs to
// a group: there is no membership to check, no mention that could have carried the
// command, and no group command worth running. What is left is the one thing a
// single chat is for here: reading a receipt without the group reading it too.
func (h *handler) onPrivateMessage(ctx context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		return err
	}
	data, ok := value.(*qqbotsdk.C2CMessageCreateData)
	if !ok {
		return fmt.Errorf("administrator commands got %T for a single chat", value)
	}
	if data.Author == nil || strings.TrimSpace(data.Author.UserOpenID) == "" {
		return nil
	}
	// Logged before any filtering, for the same reason the group path logs first:
	// a message that is dropped would otherwise leave no trace at all.
	h.deps.Logger.Debug("a private message arrived",
		"member", data.Author.UserOpenID, "message", data.ID, "content", data.Content)
	if !h.firstSight(data.ID) {
		return nil
	}
	return h.privateCommand(ctx, data)
}

// privateCommand answers the same command in a single chat.
//
// Private is where a receipt can be read in full without the group seeing it,
// which is the point of allowing it at all -- and it is also the reason the
// check here is per record rather than a flat "is an administrator somewhere":
// what the sender may see is exactly the records of the groups they administer,
// and nothing else.
func (h *handler) privateCommand(ctx context.Context, data *qqbotsdk.C2CMessageCreateData) error {
	command, ok := parseCommand(data.Content, h.cfg.Prefix)
	if !ok {
		// A single chat with the bot is not a place to answer everything that is
		// typed into it. Only something that is plainly a command gets an answer.
		return nil
	}
	switch command.name {
	case "菜单", "menu", "help", "帮助":
		h.replyPrivately(ctx, data, privateUsage(h.cfg.Prefix))
		return nil
	}

	ask := strings.TrimSpace(strings.Join(command.args, " "))
	if !contains(receiptNames, command.name) {
		h.replyPrivately(ctx, data, "私聊只支持："+privateUsage(h.cfg.Prefix))
		return nil
	}
	if ask == "" {
		h.replyPrivately(ctx, data, "用法：/违规查询 <回执单号>。")
		return nil
	}

	entry, err := h.findJudgement(ctx, ask)
	if err != nil {
		h.replyPrivately(ctx, data, receiptProblem(ask, err))
		return nil
	}
	sender := data.Author.UserOpenID
	if !h.IsAdmin(entry.GroupOpenID, sender) {
		// One answer for "not an administrator anywhere" and for "an
		// administrator of a different group", because telling them apart tells
		// the sender which group the record belongs to.
		h.deps.Logger.Warn("refused a private receipt for a member who does not administer it",
			"member", sender, "receipt", ask, "group", entry.GroupOpenID)
		h.replyPrivately(ctx, data, "你没有权限查看这条记录：只有该群的管理员可以查。")
		return nil
	}
	h.deps.Logger.Info("an administrator read a receipt privately",
		"member", sender, "receipt", entry.ID, "group", entry.GroupOpenID)
	h.replyPrivately(ctx, data, h.receiptText(entry))
	return nil
}

// findJudgement looks one receipt up, normalising what was typed.
//
// Receipts are lowercase hex and people type them in whatever case their
// keyboard offers, and a leading hash is what a receipt looks like when it is
// copied out of a message. Both are undone here rather than in the store, which
// has no business knowing how a receipt is written down.
func (h *handler) findJudgement(ctx context.Context, ask string) (store.Judgement, error) {
	judgements := h.judgements()
	if judgements == nil {
		return store.Judgement{}, errNoReceiptStore
	}
	cleaned := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ask), "#")))
	return judgements.Find(ctx, cleaned)
}

// errNoReceiptStore reports a deployment that keeps no records at all.
var errNoReceiptStore = errors.New("this bot is not keeping a record of judgements")

// judgements is the record, or nil when this deployment has none.
func (h *handler) judgements() store.JudgementStore {
	if h.deps.Store == nil {
		return nil
	}
	return h.deps.Store.Judgements()
}

// receiptProblem turns a failed lookup into something a person can act on.
//
// Every one of these is answered rather than logged and dropped: somebody
// mistyping a number is the ordinary way to reach this, and silence would look
// exactly like the command not existing.
func receiptProblem(ask string, err error) string {
	switch {
	case errors.Is(err, store.ErrJudgementNotFound):
		return fmt.Sprintf("没有找到回执单号 %s 对应的记录。请核对号码，或改用更长的前缀。", ask)
	case errors.Is(err, store.ErrAmbiguousJudgement):
		return fmt.Sprintf("回执单号 %s 匹配到不止一条记录，请多写几位。", ask)
	case errors.Is(err, errNoReceiptStore):
		return "本机器人没有开启违规记录，查不到回执。"
	default:
		return "查询失败：读取记录时出错，请稍后再试。"
	}
}

// receiptText is one judgement written out for an administrator.
//
// Everything the record holds is here, including the model's reason and the chain
// of thought behind it, which are the things that never go to a group: they are
// free text about a member, and free text from a model is not something to
// publish. Administrators are the people a punishment has to be answerable to, so
// they get all of it -- and the part they need most is the withdrawn message
// itself, which no longer exists anywhere else.
func (h *handler) receiptText(entry store.Judgement) string {
	var out strings.Builder
	out.WriteString("违规回执 " + entry.ID + "\n")
	fmt.Fprintf(&out, "时间：%s\n",
		time.Unix(entry.CreatedAt, 0).Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&out, "群：%s\n", h.groupLabel(entry.GroupOpenID))

	verdict := entry.Verdict
	if entry.Category != "" {
		verdict = "违规·" + h.categoryLabel(entry.Category)
	}
	fmt.Fprintf(&out, "判定：%s\n", verdict)
	fmt.Fprintf(&out, "被判定人：%s\n", orNone(entry.SubjectOpenID))
	fmt.Fprintf(&out, "举报人：%s\n", orNone(entry.ReporterOpenID))
	if entry.Model != "" {
		fmt.Fprintf(&out, "模型：%s\n", entry.Model)
	}
	fmt.Fprintf(&out, "送检消息：%d 条\n", len(entry.MessageIDs))
	fmt.Fprintf(&out, "处理：%s\n", orNone(entry.Action))
	if entry.MuteSeconds > 0 {
		fmt.Fprintf(&out, "禁言时长：%s\n",
			humanDuration(time.Duration(entry.MuteSeconds)*time.Second))
	}
	if entry.Reason != "" {
		fmt.Fprintf(&out, "理由：%s\n", entry.Reason)
	}
	h.writeRecalls(&out, entry)
	if strings.TrimSpace(entry.Reasoning) != "" {
		fmt.Fprintf(&out, "思考过程：\n%s\n", oneBlock(entry.Reasoning, receiptReasoningLimit))
	}
	return out.String()
}

// writeRecalls writes what was taken back, and what was not.
//
// The text is the point of the whole section. A withdrawal is the one action that
// destroys the evidence for itself: after a successful one, nobody -- not the
// group, not an administrator, not this bot -- can read the message again, so the
// only copy left is here.
func (h *handler) writeRecalls(out *strings.Builder, entry store.Judgement) {
	if len(entry.Recalls) == 0 {
		if entry.RecallReason != "" {
			fmt.Fprintf(out, "撤回：未执行（%s）\n", entry.RecallReason)
		}
		return
	}
	taken, left := 0, 0
	for _, recall := range entry.Recalls {
		if recall.Recalled {
			taken++
		} else {
			left++
		}
	}
	fmt.Fprintf(out, "撤回：已撤回 %d 条，未撤回 %d 条\n", taken, left)
	if entry.RecallReason != "" {
		fmt.Fprintf(out, "撤回说明：%s\n", entry.RecallReason)
	}
	for _, recall := range entry.Recalls {
		if recall.Recalled {
			fmt.Fprintf(out, "· 已撤回：%s\n", oneBlock(recall.Text, receiptTextLimit))
			continue
		}
		fmt.Fprintf(out, "· 未撤回（%s）：%s\n", orNone(recall.Reason),
			oneBlock(recall.Text, receiptTextLimit))
	}
}

// How much of a message or a chain of thought a receipt shows.
//
// Both are free text of unknown length, and a receipt goes out as one message
// that has to arrive at all: a price list the model quoted back verbatim can be
// thousands of characters, and the platform refuses what is merely long. The cut
// is stated rather than silent, so nobody reads a truncated message as the whole
// one.
const (
	receiptTextLimit      = 600
	receiptReasoningLimit = 3000
)

// oneBlock lays a piece of free text out under a heading.
//
// Newlines are kept -- a price list is unreadable without them -- but the text is
// inert: it was written by a member, so nothing in it may look like part of this
// receipt's own layout, and a line pretending to be another field must not read
// as one.
func oneBlock(text string, limit int) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "（空）"
	}
	var out strings.Builder
	for _, line := range strings.Split(trimmed, "\n") {
		out.WriteString("    " + strings.TrimRight(line, "\r") + "\n")
	}
	rendered := strings.TrimRight(out.String(), "\n")
	return cutRunes(rendered, limit)
}

// cutRunes truncates on a rune boundary and says that it did.
func cutRunes(text string, limit int) string {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "\n    ……（已截断）"
}

// groupLabel names a group the way the configuration does.
func (h *handler) groupLabel(groupOpenID string) string {
	if id, ok := h.deps.GroupQQID(groupOpenID); ok && id != 0 {
		return fmt.Sprintf("%d（%s）", id, groupOpenID)
	}
	return groupOpenID
}

// categoryLabel is the configured display name for a category.
//
// The stored category is the key the configuration chooses a duration by; the
// label is the word the group was told. An administrator reading a receipt wants
// the word the group saw, so the label comes from the feature that owns the
// categories rather than from a second copy of the configuration here.
func (h *handler) categoryLabel(category string) string {
	if h.moderation == nil {
		return category
	}
	if label := strings.TrimSpace(h.moderation.LabelFor(category)); label != "" {
		return label
	}
	return category
}

// orNone writes an empty field as a fact rather than as a blank.
func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "无"
	}
	return value
}

// privateUsage is what a single chat can be asked for.
func privateUsage(prefix string) string {
	return prefix + "违规查询 <回执单号> —— 查看一条违规判定的详细记录"
}
