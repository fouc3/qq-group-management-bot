package admincmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/disclaimer"
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

// receiptCommand answers /违规查询 in the group it was asked in.
//
// Any member may ask. That is the point of the summary being what it is: it holds
// nothing a model wrote about anybody, and the punishment it describes was already
// public -- it happened in this group, in front of these people. The half that is
// a model's words about a member stays behind a button that only an administrator
// may press.
//
// It answers about this group's records and no others, and the refusal says "not
// in this group" rather than where the record really is: the second sentence would
// tell an ordinary member that the number exists somewhere.
func (h *handler) receiptCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	cmd command.Parsed) error {
	ask := strings.TrimSpace(strings.Join(cmd.Args, " "))
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
		h.deps.Logger.Warn("refused a receipt from another group",
			"group", data.GroupOpenID, "member", data.Author.MemberOpenID,
			"receipt", ask, "belongs_to", entry.GroupOpenID)
		h.reply(ctx, data, "本群没有这条记录。回执单号只能在它所属的群里查，或者私聊机器人查。")
		return nil
	}
	h.replyWithKeyboard(ctx, data, h.receiptSummary(entry, true),
		detailKeyboard(entry.ID, true))
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
	if !h.router.Seen.First(data.ID) {
		return nil
	}
	return h.privateCommand(ctx, data)
}

// privateCommand answers a command in a single chat.
//
// Which commands a single chat answers is the table's business, not a list kept
// here: a command that answers in a group and in a single chat says so once, in
// its own definition, and one that does not is not offered there.
func (h *handler) privateCommand(ctx context.Context, data *qqbotsdk.C2CMessageCreateData) error {
	cmd, ok := command.Parse(data.Content, h.cfg.Prefix)
	if !ok {
		// A single chat with the bot is not a place to answer everything that is
		// typed into it. Only something that is plainly a command gets an answer.
		return nil
	}
	if def, listed := h.commands().Lookup(cmd.Name); listed && def.Private != nil {
		return def.Private(ctx, data, cmd)
	}
	h.replyPrivately(ctx, data, "私聊只支持："+h.privateUsageText())
	return nil
}

// privateMenu answers the command list in a single chat.
//
// What comes back is the single chat's own list and not the group's: a group
// command typed into a single chat is not answered at all, so listing it would
// advertise something that does not work. The words are the same ones anyway,
// because a member who has learnt /菜单 should not have to learn a second name to
// find out what a single chat can do.
func (h *handler) privateMenu(ctx context.Context, data *qqbotsdk.C2CMessageCreateData,
	_ command.Parsed) error {
	h.replyPrivately(ctx, data, h.privateUsageText())
	return nil
}

// privateReceipt answers /违规查询 in a single chat.
//
// Private is where a receipt can be read in full without the group seeing it,
// which is the point of allowing it at all -- and it is also the reason the
// check behind the buttons is per record rather than a flat "is an administrator
// somewhere": what the sender may see is exactly the records of the groups they
// administer, and nothing else.
func (h *handler) privateReceipt(ctx context.Context, data *qqbotsdk.C2CMessageCreateData,
	cmd command.Parsed) error {
	ask := strings.TrimSpace(strings.Join(cmd.Args, " "))
	if ask == "" {
		h.replyPrivately(ctx, data, "用法：/违规查询 <回执单号>。")
		return nil
	}

	entry, err := h.findJudgement(ctx, ask)
	if err != nil {
		h.replyPrivately(ctx, data, receiptProblem(ask, err))
		return nil
	}
	// The same two layers as in a group: a summary anybody may read, and the
	// details behind a button. Who may press it is decided when the press
	// arrives, which is where the record's own group is known -- the summary can
	// say which group a receipt belongs to, so the button cannot take that back.
	h.replyPrivatelyWithKeyboard(ctx, data, h.receiptSummary(entry, false),
		detailKeyboard(entry.ID, false))
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

// receiptSummary is the part of a receipt anybody may read.
//
// It is written to be short on purpose: who was judged, what was found and what
// was done about it. Nothing a model wrote about a member is here -- no reason, no
// chain of thought, no withdrawn text -- because this is the version a group gets,
// and a group is the one place where free text from a model must not be published.
// Everything else is in receiptDetails, which an administrator asks for.
//
// inGroup decides how the two members are named, and it is not cosmetic: a single
// chat refuses the mention tag outright, so a summary written for a group cannot
// be sent to one. It was, once -- the whole private answer came back 40034106 and
// the person asking saw nothing at all.
func (h *handler) receiptSummary(entry store.Judgement, inGroup bool) string {
	var out strings.Builder
	out.WriteString("**违规回执 " + entry.ID + "**\n")
	out.WriteString(receiptDivider + "\n")
	fmt.Fprintf(&out, "**时间**：%s\n",
		time.Unix(entry.CreatedAt, 0).Format("2006-01-02 15:04:05"))
	// The QQ number, not the openid: the number is the group somebody knows they
	// are in, and it is short. The openid is what an administrator needs when
	// they go to the configuration, so it is in the details.
	fmt.Fprintf(&out, "**群**：%s\n", h.groupNumber(entry.GroupOpenID))

	fmt.Fprintf(&out, "**判定**：%s\n", h.verdictLabel(entry))
	fmt.Fprintf(&out, "**被判定人**：%s\n", memberRef(entry.SubjectOpenID, inGroup))
	fmt.Fprintf(&out, "**举报人**：%s\n", memberRef(entry.ReporterOpenID, inGroup))
	if entry.Model != "" {
		fmt.Fprintf(&out, "**模型**：%s\n", entry.Model)
	}
	fmt.Fprintf(&out, "**送检消息**：%d 条\n", len(entry.MessageIDs))
	// No line of its own for the mute: the action already spells out what was
	// done, and the duration is inside it -- "已撤回 1 条消息 已禁言 10分钟", or
	// the reason it could not be applied. A field that repeats half of the line
	// above it is a field a reader has to check twice.
	fmt.Fprintf(&out, "**处理**：%s\n", orNone(entry.Action))
	return out.String()
}

// receiptDetails is what an administrator asks for by pressing a button.
//
// This is the half that has to be kept away from a group: the model's own words
// about a member, and the text of what was taken back. The last one is the reason
// the record exists at all -- after a successful withdrawal nobody can read the
// message again, not the group, not an administrator, not this bot, so the copy
// here is the only one left anywhere.
func (h *handler) receiptDetails(entry store.Judgement) string {
	var out strings.Builder
	out.WriteString("**违规回执 " + entry.ID + " 详细信息**\n")
	out.WriteString(receiptDivider + "\n")
	fmt.Fprintf(&out, "**群 openid**：%s\n", orNone(entry.GroupOpenID))
	if entry.Reason != "" {
		fmt.Fprintf(&out, "**理由**：%s\n", entry.Reason)
	}
	h.writeRecalls(&out, entry)
	if strings.TrimSpace(entry.Reasoning) != "" {
		fmt.Fprintf(&out, "**思考过程**：\n%s\n",
			oneBlock(entry.Reasoning, receiptReasoningLimit))
	}
	// Almost every field above is somebody else's text: the reason and the chain of thought
	// are the model's, and the withdrawn messages are the members'. It goes out under this
	// bot's name, which is exactly the confusion the disclaimer exists to prevent.
	return disclaimer.After(out.String(), receiptDivider)
}

// receiptDivider is the rule under a receipt's title.
//
// Drawn rather than written as "---": the platform's markdown is not the whole of
// markdown, and a line of dashes that is not understood as a horizontal rule comes
// out as three stray characters. A run of box-drawing characters is a visible rule
// either way.
const receiptDivider = "────────────────────"

// memberRef names a member in a receipt.
//
// In a group it is a mention, which the platform renders as the member's own name
// and which is how the group already saw them. In a single chat it is the openid,
// because a single chat does not accept the mention tag at all: the send is
// refused whole with 40034106 -- "C2C消息不支持qqbot-at-user", measured -- so a
// mention there is not a cosmetic difference, it is a message that never arrives.
//
// The openid is a poor name to read, and it is the honest one: it is what an
// administrator can search the configuration and the member log for, and the
// alternative is nothing at all.
func memberRef(openID string, inGroup bool) string {
	if inGroup {
		return atUser(openID)
	}
	return orNone(openID)
}

// atUser renders a member the way the platform's markdown expects a mention.
//
// An openid is not something a person can read: "被判定人：F9BBF0F4311C..." tells an
// administrator nothing at a glance, and those two fields are exactly the two
// facts a receipt is about. A mention renders as the member's own name.
//
// An empty openid becomes a word rather than an empty mention, because an empty
// mention renders as nothing at all and would leave the line looking truncated.
func atUser(openID string) string {
	trimmed := strings.TrimSpace(openID)
	if trimmed == "" {
		return "无"
	}
	return `<qqbot-at-user id="` + trimmed + `"/>`
}

// writeRecalls writes what was taken back, and what was not.
//
// The text is the point of the whole section. A withdrawal is the one action that
// destroys the evidence for itself: after a successful one, nobody -- not the
// group, not an administrator, not this bot -- can read the message again, so the
// only copy left is here.
func (h *handler) writeRecalls(out *strings.Builder, entry store.Judgement) {
	if len(entry.Recalls) == 0 {
		// Nothing was attempted, and the record says why in a sentence of its
		// own: "未发现违规，未执行撤回", "试运行：未执行撤回".
		if entry.RecallReason != "" {
			fmt.Fprintf(out, "**撤回**：%s\n", entry.RecallReason)
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
	// The counts, and then what happened to each message with the reason it did
	// not come back. The summary reason is deliberately not printed here: with
	// one line per message below it, an aggregate line can only repeat them or
	// disagree with them.
	fmt.Fprintf(out, "**撤回**：已撤回 %d 条，未撤回 %d 条\n", taken, left)
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
// A message that is one line goes into the line that introduces it -- "· 已撤回：
// 试试" -- because indenting it four spaces onto a line of its own reads like a
// stray fragment. One that has newlines of its own is laid out as a block: a price
// list is unreadable without them.
//
// Either way the text is inert. It was written by a member, so nothing in it may
// look like part of this receipt's own layout, and a line pretending to be another
// field must not read as one.
func oneBlock(text string, limit int) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "（空）"
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) == 1 {
		return cutRunes(strings.TrimRight(lines[0], "\r"), limit)
	}
	// The first line stays on the line that introduces it and only the rest is
	// indented, so that a block reads as one entry rather than as a fragment
	// floating under a colon.
	var out strings.Builder
	out.WriteString(strings.TrimRight(lines[0], "\r"))
	for _, line := range lines[1:] {
		out.WriteString("\n    " + strings.TrimRight(line, "\r"))
	}
	return cutRunes(out.String(), limit)
}

// cutRunes truncates on a rune boundary and says that it did.
func cutRunes(text string, limit int) string {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "\n    ……（已截断）"
}

// groupNumber names a group by the number its members know it by.
//
// The openid is what the configuration and every API call speak, but nobody in a
// group has ever seen it; the number is what they are in. A group whose number is
// not configured yet falls back to the openid, because a blank field answers
// nothing, and the receipt is the one place it can be read back from.
func (h *handler) groupNumber(groupOpenID string) string {
	if id, ok := h.deps.GroupQQID(groupOpenID); ok && id != 0 {
		return fmt.Sprintf("%d", id)
	}
	return orNone(groupOpenID)
}

// verdictLabel is what a receipt says was decided.
//
// The column holds the value the code acts on -- ok, violation, error -- and a
// receipt that printed it would be showing the reader a key out of a database.
// It says the same three things in words, and a violation says which one, in the
// configuration's own wording rather than in the key it is filed under.
func (h *handler) verdictLabel(entry store.Judgement) string {
	switch entry.Verdict {
	case store.JudgementViolation:
		if label := h.categoryLabel(entry.Category); label != "" {
			return "违规·" + label
		}
		return "违规"
	case store.JudgementOK:
		return "无违规"
	case store.JudgementError:
		// Nobody looked: the window was not in the cache, or the model could not
		// be read. Saying "无违规" here would be the one mistake this column
		// exists to prevent, because it is the answer somebody would act on.
		return "未能判定"
	default:
		// A record written by a version that knew a value this one does not:
		// shown as it is rather than guessed at.
		return orNone(entry.Verdict)
	}
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

// The button payloads that mark a keyboard as this feature's, and as which of
// its two actions. A feature that shares a connection with another has to be able
// to recognise its own buttons: a press is delivered to whoever listens.
const (
	receiptDetailPrefix = "qgb-receipt-detail:"
	receiptRecallPrefix = "qgb-receipt-recall:"
)

// receiptRecallWindow is how long a recall button stays meaningful.
//
// The platform only lets a bot take back its own message for a couple of minutes,
// so a button older than that cannot work however long the record is kept. The
// window is generous because what matters is that the entry is gone before the
// map grows, not that it is gone the moment the button stops working.
const receiptRecallWindow = 30 * time.Minute

// detailKeyboard is the button under a summary receipt.
//
// The permission differs by where the receipt is read. In a group the platform's
// own gate applies, and a button an ordinary member may not press is greyed out
// before anybody presses it. In a single chat there is no such thing as an
// administrator, so the button is shown to the one reader and the handler is what
// decides -- which it does either way, because the platform's administrator list
// and this bot's are two different lists.
func detailKeyboard(judgementID string, inGroup bool) *qqbotsdk.Keyboard {
	permission := qqbotsdk.PermissionTypeEveryone
	if inGroup {
		permission = qqbotsdk.PermissionTypeAdmin
	}
	return receiptKeyboard("receipt_detail", "显示详细信息",
		receiptDetailPrefix+judgementID, permission)
}

// recallKeyboard is the button under a detailed receipt.
//
// Group only. A detailed receipt in a single chat has no button: there is one
// reader, they are the person who asked for it, and nothing else will ever see it.
func recallKeyboard(token string) *qqbotsdk.Keyboard {
	return receiptKeyboard("receipt_recall", "撤回详细信息",
		receiptRecallPrefix+token, qqbotsdk.PermissionTypeAdmin)
}

// receiptKeyboard builds the one-button keyboard a receipt carries.
func receiptKeyboard(id, label, data string, permission int) *qqbotsdk.Keyboard {
	button := qqbotsdk.Button{
		ID: id,
		RenderData: &qqbotsdk.RenderData{
			Label:        label,
			VisitedLabel: label,
			Style:        qqbotsdk.KeyboardStyleBlue,
		},
		Action: &qqbotsdk.Action{
			Type:          qqbotsdk.ActionTypeCallback,
			Data:          data,
			Permission:    &qqbotsdk.Permission{Type: permission},
			UnsupportTips: "请升级 QQ 客户端",
		},
	}
	return &qqbotsdk.Keyboard{
		Content: &qqbotsdk.KeyboardContent{
			Rows: []qqbotsdk.Row{{Buttons: []qqbotsdk.Button{button}}},
		},
	}
}

// receiptRecall is one sent detailed receipt, waiting to be taken back.
type receiptRecall struct {
	groupOpenID string
	messageID   string
	expires     time.Time
}

// newReceiptToken returns the short name a recall button carries.
//
// A token rather than the message id: a message id is about a hundred characters
// of opaque text, and how much a button's data field holds is not something to
// find out in production.
func newReceiptToken() (string, error) {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generating a receipt token: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// rememberRecall records what a recall button will take back.
func (h *handler) rememberRecall(token, groupOpenID, messageID string) {
	h.receiptsMu.Lock()
	defer h.receiptsMu.Unlock()
	if h.receipts == nil {
		h.receipts = map[string]receiptRecall{}
	}
	h.dropExpiredReceipts()
	h.receipts[token] = receiptRecall{
		groupOpenID: groupOpenID,
		messageID:   messageID,
		expires:     time.Now().Add(receiptRecallWindow),
	}
}

// lookupRecall returns what a recall button points at.
func (h *handler) lookupRecall(token string) (receiptRecall, bool) {
	h.receiptsMu.Lock()
	defer h.receiptsMu.Unlock()
	h.dropExpiredReceipts()
	target, found := h.receipts[token]
	return target, found
}

// forgetRecall drops a button that has been used, so that pressing it twice
// cannot take back a message that has already gone -- or, worse, a second message
// that happens to hold the same token.
func (h *handler) forgetRecall(token string) {
	h.receiptsMu.Lock()
	defer h.receiptsMu.Unlock()
	delete(h.receipts, token)
}

// dropExpiredReceipts forgets the buttons that can no longer work.
//
// Called under the lock, from both the write and the read, because a map that is
// only pruned on the way in grows with every receipt a group ever reads.
func (h *handler) dropExpiredReceipts() {
	now := time.Now()
	for token, target := range h.receipts {
		if now.After(target.expires) {
			delete(h.receipts, token)
		}
	}
}

// showDetailsPress answers the button that asks for the rest of a receipt.
//
// The routing happened before this was called: a press reaches here when its
// button carries the detail namespace, and what the payload holds is the receipt
// the button was made for.
func (h *handler) showDetailsPress(ctx context.Context, press command.Press) error {
	return h.showDetails(ctx, press.Data, press.Payload)
}

// recallDetailsPress answers the button that takes a detailed receipt back.
func (h *handler) recallDetailsPress(ctx context.Context, press command.Press) error {
	return h.recallDetails(ctx, press.Data, press.Payload)
}

// showDetails answers the button that asks for the rest of a receipt.
//
// The same button is under a group's summary and under a single chat's, and what
// differs is only where the answer goes and whether it can be taken back again: a
// detailed receipt in a group carries a recall button, and one in a single chat
// has nobody to hide it from.
func (h *handler) showDetails(ctx context.Context, data *qqbotsdk.InteractionCreateData,
	judgementID string) error {
	presser := interactionPresser(data)
	log := h.deps.Logger.With("scene", data.Scene, "group", data.GroupOpenID,
		"member", presser, "receipt", judgementID)

	entry, err := h.findJudgement(ctx, judgementID)
	if err != nil {
		log.Warn("a receipt button was pressed for a record that cannot be read",
			"error", err)
		return h.answer(data.ID, qqbotsdk.InteractionCodeFailed)
	}
	// A press in a group may only be about that group's records, and a press in a
	// single chat carries no group at all: the record's own group is the one that
	// decides, and the presser has to administer it.
	if data.Scene == qqbotsdk.InteractionSceneGroup && entry.GroupOpenID != data.GroupOpenID {
		log.Warn("refused the details of another group's receipt",
			"belongs_to", entry.GroupOpenID)
		return h.answer(data.ID, qqbotsdk.InteractionCodeAdminOnly)
	}
	if !h.IsAdmin(entry.GroupOpenID, presser) {
		// The platform greys the button out for a group member who is not an
		// administrator, and this is the check that actually decides: the
		// configured list is this bot's, and it is not the same list.
		log.Warn("refused the details of a receipt to somebody who may not read them")
		return h.answer(data.ID, qqbotsdk.InteractionCodeAdminOnly)
	}

	// The details are a message of their own rather than an edit: the summary is
	// what was asked for and stays where it is, so that the details can be taken
	// back without taking the summary with them.
	if data.Scene != qqbotsdk.InteractionSceneGroup {
		if err := h.sendPrivateMessage(ctx, presser, h.receiptDetails(entry),
			""); err != nil {
			log.Error("could not send the details of a receipt", "error", err)
			return h.answer(data.ID, qqbotsdk.InteractionCodeFailed)
		}
		log.Info("an administrator read the details of a receipt privately")
		return h.answer(data.ID, qqbotsdk.InteractionCodeSuccess)
	}

	token, err := newReceiptToken()
	if err != nil {
		log.Error("could not make a recall token", "error", err)
		return h.answer(data.ID, qqbotsdk.InteractionCodeFailed)
	}
	response, err := h.sendMessageWithKeyboard(ctx, data.GroupOpenID,
		h.receiptDetails(entry), "", recallKeyboard(token))
	if err != nil {
		log.Error("could not send the details of a receipt", "error", err)
		return h.answer(data.ID, qqbotsdk.InteractionCodeFailed)
	}
	if response != nil {
		h.rememberRecall(token, data.GroupOpenID, response.ID)
	}
	log.Info("an administrator read the details of a receipt")
	return h.answer(data.ID, qqbotsdk.InteractionCodeSuccess)
}

// recallDetails answers the button that takes a detailed receipt back.
//
// Nothing is said in the group afterwards. The button said what it would do, the
// person who pressed it is watching the message disappear, and an announcement
// would be the bot talking to itself in front of everybody.
func (h *handler) recallDetails(ctx context.Context, data *qqbotsdk.InteractionCreateData,
	token string) error {
	if data.Scene != qqbotsdk.InteractionSceneGroup {
		// A recall button only ever exists under a group's detailed receipt, so a
		// press from anywhere else is either a stale button or not ours. Refused
		// rather than ignored, because the presser is waiting on a spinner.
		return h.answer(data.ID, qqbotsdk.InteractionCodeFailed)
	}
	log := h.deps.Logger.With("group", data.GroupOpenID,
		"member", data.GroupMemberOpenID, "token", token)

	target, found := h.lookupRecall(token)
	if !found {
		// Unknown or expired: answering with a failure leaves the button
		// pressable, which is what somebody retrying needs.
		log.Info("a recall button was pressed after its window closed")
		return h.answer(data.ID, qqbotsdk.InteractionCodeFailed)
	}
	if target.groupOpenID != data.GroupOpenID ||
		!h.IsAdmin(target.groupOpenID, data.GroupMemberOpenID) {
		log.Warn("refused a recall to a member who may not press it")
		return h.answer(data.ID, qqbotsdk.InteractionCodeNoPermission)
	}
	if err := h.deps.Client.RecallGroupMessage(ctx, target.groupOpenID,
		target.messageID); err != nil {
		// A failure the platform decides, and the most likely one by far is the
		// two-minute limit on taking back a bot's own message.
		log.Warn("could not take back a detailed receipt", "error", err)
		return h.answer(data.ID, qqbotsdk.InteractionCodeFailed)
	}
	h.forgetRecall(token)
	log.Info("a detailed receipt was taken back")
	return h.answer(data.ID, qqbotsdk.InteractionCodeSuccess)
}

// answer reports the outcome to the client that pressed the button.
//
// Every path through a button press has to answer, because an unanswered
// interaction leaves the presser on a spinner until it times out.
//
// It takes no context from its caller on purpose: an answer has to be sent even
// though the event that carried the press has been dealt with, and what it waits
// on is the feature rather than the request, so that a press is never answered by
// an instance that has already been replaced.
func (h *handler) answer(interactionID string, code qqbotsdk.InteractionCode) error {
	if interactionID == "" {
		return errors.New("the interaction event carried no id to answer")
	}
	answerCtx, cancel := context.WithTimeout(h.part, 10*time.Second)
	defer cancel()
	if err := h.deps.Client.RespondInteraction(answerCtx, interactionID, code); err != nil {
		return fmt.Errorf("answering the interaction: %w", err)
	}
	return nil
}

// interactionPresser is who pressed a button.
//
// The two scenes name a person in two different fields, and only one of them is
// ever set: a group press carries the member openid, a single chat press the user
// openid. Both are the same kind of value to the administrator list, which is
// keyed by openid either way.
func interactionPresser(data *qqbotsdk.InteractionCreateData) string {
	if data.Scene == qqbotsdk.InteractionSceneC2C {
		return data.UserOpenID
	}
	return data.GroupMemberOpenID
}
