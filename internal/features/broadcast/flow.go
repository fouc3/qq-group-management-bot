package broadcast

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/messaging"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// pressTimeout bounds how long answering one press may take: the presser is watching
// a spinner until it is answered.
const pressTimeout = 15 * time.Second

// openCard opens a card in a single chat and sends the first one.
func (h *handler) openCard(ctx context.Context, chat, replyTo string) (*session, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	s := &session{
		token:   token,
		chat:    chat,
		replyTo: replyTo,
		groups:  h.groupChoices(ctx, chat),
		updated: time.Now(),
		mu:      make(chan struct{}, 1),
	}
	h.remember(s)
	if err := h.showCard(ctx, s, ""); err != nil {
		h.forget(s)
		return nil, err
	}
	return s, nil
}

// onPress answers a button on a card.
func (h *handler) onPress(ctx context.Context, press command.Press) error {
	asked, ok := readAction(press.Payload)
	if !ok {
		return h.answer(press, qqbotsdk.InteractionCodeFailed)
	}
	s, found := h.lookup(asked.token)
	if !found {
		// The card outlived its lifetime, or the feature was built again since it was
		// opened. Said out loud rather than ignored, because the presser is looking at
		// something that will never answer.
		return h.answerWith(press, qqbotsdk.InteractionCodeFailed,
			"这张广播卡片已经过期，请在私聊里重新发 /群广播。")
	}
	if !h.mayWork(s, press.Data) {
		h.loggerIn(s.chat).Warn("refused a press on somebody else's broadcast card",
			"pressed_by", press.Data.UserOpenID)
		return h.answerWith(press, qqbotsdk.InteractionCodeAdminOnly,
			"这张广播卡片不是发起它的人在操作。")
	}

	// One card, one press at a time: two presses reading the same state would send two
	// cards for it. Held across the calls that answer the press, which is why it is a
	// channel rather than a short critical section.
	release := s.lock()
	defer release()
	defer h.touch(s)

	return h.work(ctx, s, asked, press)
}

// work carries out one press.
func (h *handler) work(ctx context.Context, s *session, asked action, press command.Press) error {
	switch asked.kind {
	case kindMarkdown:
		s.markdown = s.markdown.toggled()
	case kindAnonymous:
		s.anonymous = s.anonymous.toggled()

	case kindRichText:
		// Shown so that whoever is writing knows it is not available, and refused every
		// time it is pressed: the platform has no rich-text message this bot can send.
		return h.answerWith(press, qqbotsdk.InteractionCodeFailed,
			"富文本消息平台还不支持，暂时发不了。")

	case kindGroup:
		for index := range s.groups {
			if s.groups[index].openID == asked.group {
				s.groups[index].selected = !s.groups[index].selected
			}
		}

	case kindCancel:
		h.forget(s)
		h.recallCards(ctx, s)
		return h.answer(press, qqbotsdk.InteractionCodeSuccess)

	case kindConfirm:
		if waiting := s.missing(); len(waiting) > 0 {
			// Refused rather than filled in: the card exists so that a person decides,
			// and a default nobody looked at is not a decision.
			return h.answerWith(press, qqbotsdk.InteractionCodeFailed,
				"还要选："+strings.Join(waiting, "、"))
		}
		return h.showSummary(ctx, s, press)

	case kindBack:
		// Back to the card, which is how a broadcast is thought about again without
		// losing the choices already made.
		return h.showCardThen(ctx, s, press)

	case kindContinue, kindEdit:
		s.waiting = true
		s.content = ""
		return h.showPrompt(ctx, s, press)

	case kindSend:
		return h.deliver(ctx, s, press)
	}

	return h.showCardThen(ctx, s, press)
}

// showCardThen sends the card as it now stands, and reports the press.
func (h *handler) showCardThen(ctx context.Context, s *session, press command.Press) error {
	if err := h.showCard(ctx, s, press.EventID); err != nil {
		h.loggerIn(s.chat).Error("could not send the broadcast card", "error", err)
		return h.answer(press, qqbotsdk.InteractionCodeFailed)
	}
	return h.answer(press, qqbotsdk.InteractionCodeSuccess)
}

// showCard sends the card, as the member left it.
//
// Where it answers depends on what asked for it: the message the command arrived in when
// the card was opened, and the press's event afterwards. Both are passive answers, so a
// card does not cost anything from what the bot may say unasked -- and the event is
// preferred where there is one, because a press carries the id for it.
func (h *handler) showCard(ctx context.Context, s *session, pressed string) error {
	card := messaging.Message{
		UserOpenID: s.chat,
		Text:       h.cardText(s),
		Keyboard:   h.cardKeyboard(s),
	}
	if pressed != "" {
		card.ReplyToEvent = pressed
	} else {
		card.ReplyTo = s.replyTo
		card.Sequence = s.sequence
	}

	response, err := h.send(ctx, card)
	if err != nil {
		// The window on an answer closes, and a card being worked on is still wanted:
		// the same message goes out on its own rather than not at all.
		h.loggerIn(s.chat).Warn("the card could not be sent as an answer, so it is "+
			"being sent on its own", "error", err)
		fallback := messaging.Message{
			UserOpenID: s.chat,
			Text:       h.cardText(s),
			Keyboard:   h.cardKeyboard(s),
		}
		response, err = h.send(ctx, fallback)
		if err != nil {
			return err
		}
	}
	if pressed == "" {
		// Only the answers to one message are numbered; an answer to an event is the
		// only one there is.
		s.sequence++
	}
	h.replace(ctx, s, &s.cardMessageID, response)
	return nil
}

// replace takes the previous card back.
//
// Best effort, and deliberately so: the platform only lets a bot take its own message
// back for a couple of minutes, and a card left behind is readable rather than harmful --
// it carries the same token, so pressing it still works.
func (h *handler) replace(ctx context.Context, s *session, held *string,
	sent *qqbotsdk.MessageResponse) {
	if *held != "" {
		h.recall(ctx, s, *held)
	}
	if sent != nil {
		*held = sent.ID
	}
}

// recall takes one of the bot's own messages in a single chat back.
func (h *handler) recall(ctx context.Context, s *session, messageID string) {
	if err := h.deps.Client.RecallC2CMessage(ctx, s.chat, messageID); err != nil {
		h.loggerIn(s.chat).Debug("an earlier message could not be taken back", "error", err)
	}
}

// recallCards takes back what this broadcast left behind: the card, and the preview of a
// message that was written but not sent.
func (h *handler) recallCards(ctx context.Context, s *session) {
	for _, held := range []string{s.cardMessageID, s.previewMessageID} {
		if held == "" {
			continue
		}
		h.recall(ctx, s, held)
	}
	s.cardMessageID, s.previewMessageID = "", ""
}

// showSummary says what is about to be sent, and asks whether to go on.
func (h *handler) showSummary(ctx context.Context, s *session, press command.Press) error {
	lines := []string{
		"**广播参数**",
		"",
		"Markdown 渲染：" + s.markdown.words(),
		"匿名发送：" + s.anonymous.words(),
		"发送到：" + chosenNames(s),
		"",
		"按“继续”写广播内容，按“返回”回去改。",
	}
	keyboard := &qqbotsdk.Keyboard{Content: &qqbotsdk.KeyboardContent{Rows: []qqbotsdk.Row{
		{Buttons: []qqbotsdk.Button{
			plainButton("继续", kindContinue, s.token, ""),
			plainButton("返回", kindBack, s.token, ""),
			plainButton("取消", kindCancel, s.token, ""),
		}},
	}}}
	if err := h.sendNote(ctx, s, press.EventID, strings.Join(lines, "\n"), keyboard, nil); err != nil {
		h.loggerIn(s.chat).Error("could not send the broadcast summary", "error", err)
		return h.answer(press, qqbotsdk.InteractionCodeFailed)
	}
	return h.answer(press, qqbotsdk.InteractionCodeSuccess)
}

// showPrompt asks for the text of the broadcast.
func (h *handler) showPrompt(ctx context.Context, s *session, press command.Press) error {
	lines := []string{
		"请直接把广播内容发出来，下一条消息就是它。",
		"",
		"换行可以直接粘贴，支持多行。",
	}
	keyboard := &qqbotsdk.Keyboard{Content: &qqbotsdk.KeyboardContent{Rows: []qqbotsdk.Row{
		{Buttons: []qqbotsdk.Button{plainButton("取消", kindCancel, s.token, "")}},
	}}}
	if err := h.sendNote(ctx, s, press.EventID, strings.Join(lines, "\n"), keyboard, nil); err != nil {
		h.loggerIn(s.chat).Error("could not ask for the broadcast text", "error", err)
		return h.answer(press, qqbotsdk.InteractionCodeFailed)
	}
	return h.answer(press, qqbotsdk.InteractionCodeSuccess)
}

// onPrivateMessage takes the text a broadcast is made of.
//
// Only while a member is waiting to write one, only from that member, and never a
// command: a command typed in the middle of writing has to be answered, and only the
// table knows what a command looks like.
func (h *handler) onPrivateMessage(ctx context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		return err
	}
	data, ok := value.(*qqbotsdk.C2CMessageCreateData)
	if !ok {
		return nil
	}
	s := h.waitingIn(data.Author.UserOpenID)
	if s == nil {
		return nil
	}
	if h.commands != nil && h.commands.LooksLikeACommand(data.Content) {
		return nil
	}
	text := textOf(data.Content)
	if text == "" {
		return nil
	}

	release := s.lock()
	defer release()

	// Checked again while holding the card: between the lookup above and here, another
	// message could have finished the same card.
	if !s.waiting {
		return nil
	}
	s.content = text
	s.waiting = false
	h.touch(s)
	h.deps.Logger.Info("a broadcast text was taken", "member", s.chat,
		"token", s.token, "runes", len([]rune(text)))
	return h.showPreview(ctx, s)
}

// textOf is what a member wrote, with a mention of the bot taken off.
//
// The mention is how the message arrived rather than part of what it says: a single chat
// set to deliver only mentions would otherwise put it at the front of the broadcast.
func textOf(content string) string {
	text := content
	if mentioned, ok := command.FirstMention(text); ok {
		text = strings.ReplaceAll(text, "<@"+mentioned+">", "")
	}
	return strings.TrimSpace(text)
}

// waitingIn is the card in this single chat that is waiting for this member to write.
func (h *handler) waitingIn(chat string) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropExpired()
	for _, s := range h.open {
		if s.chat == chat && s.waiting {
			return s
		}
	}
	return nil
}

// showPreview shows the broadcast as the groups will read it, and asks to send it.
func (h *handler) showPreview(ctx context.Context, s *session) error {
	lines := []string{"**预览**（按“发送”就会发出去）", "", h.broadcastText(s)}
	// Sent as a message of its own rather than as an answer to something: the text being
	// previewed came in as an ordinary message, and the preview is what the groups will
	// read rather than a reply to the writer.
	if err := h.sendNote(ctx, s, "", strings.Join(lines, "\n"), previewKeyboard(s),
		&s.previewMessageID); err != nil {
		h.loggerIn(s.chat).Error("could not send the broadcast preview", "error", err)
		return err
	}
	return nil
}

// previewKeyboard is under the preview: the last look before a group reads it.
func previewKeyboard(s *session) *qqbotsdk.Keyboard {
	return &qqbotsdk.Keyboard{Content: &qqbotsdk.KeyboardContent{Rows: []qqbotsdk.Row{
		{Buttons: []qqbotsdk.Button{
			plainButton("发送", kindSend, s.token, ""),
			plainButton("重新编辑", kindEdit, s.token, ""),
			plainButton("取消", kindCancel, s.token, ""),
		}},
	}}}
}

// sendNote sends one of the messages a card is made of: the summary, the request for
// text, the preview.
//
// It answers the press it came from when there is one, and held is where the id of a
// message that has to be taken back later is kept.
func (h *handler) sendNote(ctx context.Context, s *session, pressed, text string,
	keyboard *qqbotsdk.Keyboard, held *string) error {
	message := messaging.Message{UserOpenID: s.chat, Text: text, Keyboard: keyboard}
	if pressed != "" {
		message.ReplyToEvent = pressed
	}
	response, err := h.send(ctx, message)
	if err != nil {
		return err
	}
	if held != nil && response != nil {
		if *held != "" {
			h.recall(ctx, s, *held)
		}
		*held = response.ID
	}
	return nil
}

// deliver posts the broadcast in every group that was chosen.
//
// The message is the one the preview showed: what was approved is what the groups read.
func (h *handler) deliver(ctx context.Context, s *session, press command.Press) error {
	if strings.TrimSpace(s.content) == "" {
		return h.answerWith(press, qqbotsdk.InteractionCodeFailed, "还没有写广播内容。")
	}
	text := h.broadcastText(s)

	var sent, refused []string
	for _, groupOpenID := range s.selectedGroups() {
		// Who may broadcast into a group is asked per group, here rather than when the
		// card was opened: a single chat has no administrator list of its own, and being
		// on one group's list is not being on another's.
		if !h.adminsOf(groupOpenID, s.chat) {
			refused = append(refused, h.callOf(s, groupOpenID)+"（不是该群管理员）")
			continue
		}
		// A group the platform does not let the bot speak in unasked cannot be broadcast
		// to. Checked rather than discovered, because the failure would otherwise arrive
		// as an error nobody reading the card could see.
		if !h.acceptsUnaskedMessages(ctx, groupOpenID) {
			refused = append(refused, h.callOf(s, groupOpenID)+"（没开主动推送）")
			continue
		}
		response, err := h.send(ctx, messaging.Message{GroupOpenID: groupOpenID, Text: text})
		if err != nil {
			h.logger(groupOpenID).Error("could not post a broadcast", "error", err)
			refused = append(refused, h.callOf(s, groupOpenID)+"（平台拒绝了发送）")
			continue
		}
		sent = append(sent, groupOpenID)
		h.deposit(ctx, s, groupOpenID, response)
		h.logger(groupOpenID).Info("a broadcast was posted", "member", s.chat,
			"token", s.token, "anonymous", s.anonymous == on, "markdown", s.markdown == on)
	}

	h.forget(s)
	h.recallCards(ctx, s)

	h.deps.Logger.Info("a broadcast was finished", "member", s.chat,
		"posted_to", groupLogLine(sent), "refused", groupLogLine(refused))
	if len(refused) > 0 {
		return h.answerWith(press, qqbotsdk.InteractionCodeSuccess,
			"已发出 "+strconv.Itoa(len(sent))+" 个群；这些没有发出："+
				strings.Join(refused, "、")+"。")
	}
	return h.answerWith(press, qqbotsdk.InteractionCodeSuccess,
		"已发出 "+strconv.Itoa(len(sent))+" 个群。")
}

// deposit writes down what went out and who asked for it.
//
// A failure is loud rather than quiet: the group is not told who asked, so this record is
// the only thing that makes the notice answerable, and losing it is losing the audit. The
// message has already gone out by the time this runs -- a notice is not taken back for the
// sake of its record -- but nobody can claim afterwards that it was.
func (h *handler) deposit(ctx context.Context, s *session, groupOpenID string,
	sent *qqbotsdk.MessageResponse) {
	if h.deps.Store == nil {
		h.logger(groupOpenID).Warn("no data layer, so this broadcast is recorded "+
			"nowhere but the journal", "token", s.token, "member", s.chat)
		return
	}
	messageID := ""
	if sent != nil {
		messageID = sent.ID
	}
	err := h.deps.Store.Broadcasts().Record(ctx, store.Broadcast{
		Token:        s.token,
		SenderOpenID: s.chat,
		GroupOpenID:  groupOpenID,
		Anonymous:    s.anonymous == on,
		Markdown:     s.markdown == on,
		Content:      s.content,
		SentAt:       time.Now().Unix(),
		MessageID:    messageID,
	})
	if err != nil {
		h.logger(groupOpenID).Error("a broadcast was posted and could not be "+
			"recorded", "error", err, "token", s.token, "member", s.chat)
		return
	}
	h.logger(groupOpenID).Info("a broadcast was recorded", "token", s.token,
		"member", s.chat)
}

// auditLimit is how many broadcasts an audit shows, newest first.
//
// Fixed rather than asked for: the question is "who sent that", and the recent ones are
// where that is answered. A record kept for its own sake is read out of the database
// rather than out of a chat.
const auditLimit = 10

// auditCommand shows what was broadcast in this group, and who asked for it.
//
// Asked in a group, and about that group: the record is what the group's own
// administrators get to see, which is the answer to a notice they cannot trace
// otherwise.
func (h *handler) auditCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	_ command.Parsed) error {
	member := data.Author.MemberOpenID
	// Asked again here rather than left to the table's own gate: who may read a record of
	// who said what is worth one check of its own, and the two must agree.
	if !h.adminsOf(data.GroupOpenID, member) {
		return h.say(ctx, data.GroupOpenID, "",
			"只有本群管理员能看广播记录。")
	}
	posted, err := h.records(ctx, []string{data.GroupOpenID})
	if err != nil {
		h.logger(data.GroupOpenID).Error("could not read the broadcast record", "error", err)
		return h.say(ctx, data.GroupOpenID, "", "广播记录读取失败："+err.Error())
	}
	lines := []string{"**广播记录**（本群最近 " + strconv.Itoa(len(posted)) +
		" 条，只有本群管理员能看到）", ""}
	return h.say(ctx, data.GroupOpenID, "", joinAudit(lines, posted, false))
}

// auditPrivately shows a member what was broadcast in the groups they administer.
//
// In a single chat, and only about their own groups: the record is what somebody who
// answers for a group needs, and a list of what other groups were told is not theirs to
// read.
func (h *handler) auditPrivately(ctx context.Context, data *qqbotsdk.C2CMessageCreateData,
	_ command.Parsed) error {
	chat := data.Author.UserOpenID
	mine := h.administers(chat)
	if len(mine) == 0 {
		return h.say(ctx, chat, "", "你不在任何群的管理员名单里，没有可看的广播记录。")
	}
	posted, err := h.records(ctx, mine)
	if err != nil {
		h.loggerIn(chat).Error("could not read the broadcast record", "error", err)
		return h.say(ctx, chat, "", "广播记录读取失败："+err.Error())
	}
	lines := []string{"**广播记录**（你管理的 " + strconv.Itoa(len(mine)) + " 个群，最近 " +
		strconv.Itoa(len(posted)) + " 条）", ""}
	return h.say(ctx, chat, "", joinAudit(lines, posted, true))
}

// records reads the newest broadcasts of these groups, newest first across all of them.
func (h *handler) records(ctx context.Context, groups []string) ([]store.Broadcast, error) {
	if h.deps.Store == nil {
		return nil, errors.New("no data layer")
	}
	var posted []store.Broadcast
	for _, groupOpenID := range groups {
		found, err := h.deps.Store.Broadcasts().ListByGroup(ctx, groupOpenID, auditLimit)
		if err != nil {
			return nil, err
		}
		posted = append(posted, found...)
	}
	sort.SliceStable(posted, func(first, second int) bool {
		return posted[first].SentAt > posted[second].SentAt
	})
	if len(posted) > auditLimit {
		posted = posted[:auditLimit]
	}
	return posted, nil
}

// joinAudit renders the record for a group or for a member's own groups.
//
// withGroup names the group each line is about, which a member reading about several
// groups needs and a group reading about itself does not.
func joinAudit(head []string, posted []store.Broadcast, withGroup bool) string {
	lines := head
	if len(posted) == 0 {
		return strings.Join(append(lines, "还没有广播记录。"), "\n")
	}
	for index, entry := range posted {
		when := time.Unix(entry.SentAt, 0).Format("01-02 15:04")
		how := "匿名发出"
		if !entry.Anonymous {
			how = "署名发出"
		}
		where := ""
		if withGroup {
			where = " · " + entry.GroupOpenID
		}
		lines = append(lines,
			strconv.Itoa(index+1)+". "+when+where+" · "+how+" · 发起人 `"+
				entry.SenderOpenID+"`",
			"   "+firstLine(entry.Content, 40))
	}
	return strings.Join(lines, "\n")
}

// firstLine is the beginning of what was broadcast, for a record read in a chat.
func firstLine(content string, limit int) string {
	line := content
	if cut := strings.IndexAny(line, "\n\r"); cut >= 0 {
		line = line[:cut]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "（没有文字内容）"
	}
	runes := []rune(line)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return line
}

// adminsOf reports whether this member administers this group.
//
// The failure of a question that cannot be asked counts as "no": a broadcast is not
// something to send on a guess, and the list is what decides it rather than the bot.
func (h *handler) adminsOf(groupOpenID, memberOpenID string) bool {
	return h.admins != nil && h.admins.IsAdmin(groupOpenID, memberOpenID)
}

// acceptsUnaskedMessages reports whether a group lets the bot speak without being asked,
// which is the only way a broadcast can arrive there.
func (h *handler) acceptsUnaskedMessages(ctx context.Context, groupOpenID string) bool {
	state, err := h.deps.Client.GetGroupBotState(ctx, groupOpenID)
	if err != nil {
		h.logger(groupOpenID).Warn("could not read whether this group accepts a "+
			"message of the bot's own", "error", err)
		return false
	}
	return state.AllowProactiveMsg
}

// callOf is what a group is called in an answer to the writer.
func (h *handler) callOf(s *session, groupOpenID string) string {
	for _, group := range s.groups {
		if group.openID == groupOpenID {
			return displayName(group)
		}
	}
	return groupOpenID
}

// mayWork reports whether a press is one this card has to answer.
//
// The card lives in one single chat, and it belongs to that member: there is no group to
// be an administrator of, so what they may broadcast is asked per group when the send
// happens rather than here.
func (h *handler) mayWork(s *session, data *qqbotsdk.InteractionCreateData) bool {
	if data == nil || data.Scene != qqbotsdk.InteractionSceneC2C {
		return false
	}
	return data.UserOpenID == s.chat
}

// answer reports the outcome to the client that pressed.
func (h *handler) answer(press command.Press, code qqbotsdk.InteractionCode) error {
	return h.answerWith(press, code, "")
}

// answerWith is answer, with something said in the chat the press came from as well.
//
// Every path through a press has to answer, because an unanswered interaction leaves the
// presser looking at a spinner until it times out. It runs under the feature's own
// context, so that a press is never answered by an instance that has already been
// replaced.
func (h *handler) answerWith(press command.Press, code qqbotsdk.InteractionCode,
	said string) error {
	ctx, cancel := context.WithTimeout(h.part, pressTimeout)
	defer cancel()

	if said != "" && press.Data != nil {
		explanation := messaging.Message{Text: said}
		if press.Data.Scene == qqbotsdk.InteractionSceneC2C {
			explanation.UserOpenID = press.Data.UserOpenID
		} else {
			explanation.GroupOpenID = press.Data.GroupOpenID
		}
		if press.EventID != "" {
			explanation.ReplyToEvent = press.EventID
		}
		if _, err := h.send(ctx, explanation); err != nil {
			h.loggerIn(explanation.UserOpenID).Warn("could not say why a press was "+
				"not taken", "error", err)
		}
	}
	if press.Data == nil || press.Data.ID == "" {
		return errors.New("the interaction event carried no id to answer")
	}
	return h.deps.Client.RespondInteraction(ctx, press.Data.ID, code)
}

// toggled is what one press does to an option.
func (c chosen) toggled() chosen {
	switch c {
	case unset, off:
		return on
	default:
		return off
	}
}
