package broadcast

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/messaging"
)

// pressTimeout bounds how long answering one press may take: the presser is watching
// a spinner until it is answered.
const pressTimeout = 15 * time.Second

// openCard opens a card for a member and sends the first one.
func (h *handler) openCard(ctx context.Context, groupOpenID, memberOpenID, replyTo string) (
	*session, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	s := &session{
		token:       token,
		groupOpenID: groupOpenID,
		starter:     memberOpenID,
		replyTo:     replyTo,
		groups:      h.groupChoices(ctx),
		updated:     time.Now(),
		mu:          make(chan struct{}, 1),
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
			"这张广播卡片已经过期，请重新发送 /群广播。")
	}
	if !h.mayWork(s, press) {
		h.logger(s.groupOpenID).Warn("refused a press on somebody else's broadcast card",
			"pressed_by", press.Data.GroupMemberOpenID)
		return h.answerWith(press, qqbotsdk.InteractionCodeAdminOnly,
			"这张广播卡片不是这个群的管理员发起的。")
	}

	// One card, one press at a time: two presses reading the same state would send
	// two cards for it. Held across the calls that answer the press, which is why it
	// is a channel rather than a short critical section.
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
		// Shown so that an administrator knows it is not available, and refused every
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
			// Refused rather than filled in: the card exists so that an administrator
			// decides, and a default nobody looked at is not a decision.
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
		h.logger(s.groupOpenID).Error("could not send the broadcast card", "error", err)
		return h.answer(press, qqbotsdk.InteractionCodeFailed)
	}
	return h.answer(press, qqbotsdk.InteractionCodeSuccess)
}

// showCard sends the card, as the member left it.
//
// Where it answers depends on what asked for it: the command's own message when the
// card was opened, and the press's event when a button was pressed. Both are passive
// answers, so a card does not cost the group anything from what the bot may say
// unasked -- and the event is preferred where there is one, because a press carries
// the id for it.
func (h *handler) showCard(ctx context.Context, s *session, pressed string) error {
	card := messaging.Message{
		GroupOpenID: s.groupOpenID,
		Text:        h.cardText(s),
		Keyboard:    h.cardKeyboard(s),
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
		h.logger(s.groupOpenID).Warn("the card could not be sent as an answer, so it "+
			"is being sent on its own", "error", err)
		response, err = h.send(ctx, messaging.Message{
			GroupOpenID: s.groupOpenID,
			Text:        h.cardText(s),
			Keyboard:    h.cardKeyboard(s),
		})
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
// back for a couple of minutes, and a card left behind is readable rather than
// harmful -- it carries the same token, so pressing it still works.
func (h *handler) replace(ctx context.Context, s *session, held *string,
	sent *qqbotsdk.MessageResponse) {
	if *held != "" {
		if err := h.deps.Client.RecallGroupMessage(ctx, s.groupOpenID, *held); err != nil {
			h.logger(s.groupOpenID).Debug("an earlier card could not be taken back",
				"error", err)
		}
	}
	if sent != nil {
		*held = sent.ID
	}
}

// recallCards takes back what this broadcast left in the group: the card, and the
// preview of a message that was written but not sent.
func (h *handler) recallCards(ctx context.Context, s *session) {
	for _, held := range []string{s.cardMessageID, s.previewMessageID} {
		if held == "" {
			continue
		}
		if err := h.deps.Client.RecallGroupMessage(ctx, s.groupOpenID, held); err != nil {
			h.logger(s.groupOpenID).Debug("a broadcast card could not be taken back",
				"error", err)
		}
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
		h.logger(s.groupOpenID).Error("could not send the broadcast summary", "error", err)
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
	if !h.onlyMentions(s.groupOpenID) {
		lines = append(lines, "", "（本群设置成只有 @ 机器人才收到消息，请先 @ 我 再发内容。）")
	}
	keyboard := &qqbotsdk.Keyboard{Content: &qqbotsdk.KeyboardContent{Rows: []qqbotsdk.Row{
		{Buttons: []qqbotsdk.Button{plainButton("取消", kindCancel, s.token, "")}},
	}}}
	if err := h.sendNote(ctx, s, press.EventID, strings.Join(lines, "\n"), keyboard, nil); err != nil {
		h.logger(s.groupOpenID).Error("could not ask for the broadcast text", "error", err)
		return h.answer(press, qqbotsdk.InteractionCodeFailed)
	}
	return h.answer(press, qqbotsdk.InteractionCodeSuccess)
}

// onlyMentions reports whether a group only delivers messages that mention the bot,
// which decides whether the text has to address it to arrive at all.
//
// Asked rather than assumed, and a question that cannot be answered means "no": a
// group that receives everything does not need the advice, and giving it wrongly
// would only puzzle whoever is writing.
func (h *handler) onlyMentions(groupOpenID string) bool {
	ctx, cancel := context.WithTimeout(h.part, pressTimeout)
	defer cancel()
	state, err := h.deps.Client.GetGroupBotState(ctx, groupOpenID)
	return err == nil && state.RecvMsgSetting == qqbotsdk.GroupRecvMsgOnlyMention
}

// onGroupMessage takes the text a broadcast is made of.
//
// Only while a member is waiting to write one, only from that member, and never a
// command: a command typed in the middle of writing has to be answered, and only the
// table knows what a command looks like.
func (h *handler) onGroupMessage(ctx context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		return err
	}
	data, ok := value.(*qqbotsdk.GroupMessageCreateData)
	if !ok {
		return nil
	}
	s := h.waitingIn(data.GroupOpenID, data.Author.MemberOpenID)
	if s == nil {
		return nil
	}
	if h.commands != nil && h.commands.LooksLikeACommand(data.Content) {
		return nil
	}
	text := textOf(data)
	if text == "" {
		return nil
	}

	release := s.lock()
	defer release()

	// Checked again while holding the card: between the lookup above and here,
	// another message could have finished the same card.
	if !s.waiting {
		return nil
	}
	s.content = text
	s.waiting = false
	h.touch(s)
	h.deps.Logger.Info("a broadcast text was taken", "group", s.groupOpenID,
		"member", s.starter, "token", s.token, "runes", len([]rune(text)))
	return h.showPreview(ctx, s)
}

// textOf is what a member wrote, with a mention of the bot taken off.
//
// The mention is how the message arrived rather than part of what it says: in a group
// set to deliver only mentions, it would otherwise be the first thing a broadcast
// said.
func textOf(data *qqbotsdk.GroupMessageCreateData) string {
	text := data.Content
	if mentioned, ok := command.FirstMention(text); ok {
		text = strings.ReplaceAll(text, "<@"+mentioned+">", "")
	}
	return strings.TrimSpace(text)
}

// waitingIn is the card in this group that is waiting for this member to write.
func (h *handler) waitingIn(groupOpenID, memberOpenID string) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropExpired()
	for _, s := range h.open {
		if s.groupOpenID == groupOpenID && s.starter == memberOpenID && s.waiting {
			return s
		}
	}
	return nil
}

// showPreview shows the broadcast as the groups will read it, and asks to send it.
func (h *handler) showPreview(ctx context.Context, s *session) error {
	lines := []string{"**预览**（按“发送”就会发出去）", "", h.broadcastText(s)}
	// Answered as a message of its own rather than as a reply to something: the text
	// being previewed came in as an ordinary group message, and a preview that quoted
	// it would show the group what the broadcast is about to say.
	if err := h.sendNote(ctx, s, "", strings.Join(lines, "\n"), previewKeyboard(s),
		&s.previewMessageID); err != nil {
		h.logger(s.groupOpenID).Error("could not send the broadcast preview", "error", err)
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
	message := messaging.Message{GroupOpenID: s.groupOpenID, Text: text, Keyboard: keyboard}
	if pressed != "" {
		message.ReplyToEvent = pressed
	}
	response, err := h.send(ctx, message)
	if err != nil {
		return err
	}
	if held != nil && response != nil {
		if *held != "" {
			if err := h.deps.Client.RecallGroupMessage(ctx, s.groupOpenID, *held); err != nil {
				h.logger(s.groupOpenID).Debug("an earlier message could not be taken back",
					"error", err)
			}
		}
		*held = response.ID
	}
	return nil
}

// deliver posts the broadcast in every group that was chosen.
//
// The message is the one the preview showed: what the administrator approved is what
// the groups read.
func (h *handler) deliver(ctx context.Context, s *session, press command.Press) error {
	if strings.TrimSpace(s.content) == "" {
		return h.answerWith(press, qqbotsdk.InteractionCodeFailed, "还没有写广播内容。")
	}
	text := h.broadcastText(s)

	var sent, refused []string
	for _, groupOpenID := range s.selectedGroups() {
		// A group the platform does not let the bot speak in unasked cannot be
		// broadcast to. Checked rather than discovered, because the failure would
		// otherwise arrive as an error nobody reading the card could see.
		if !h.acceptsUnaskedMessages(ctx, groupOpenID) {
			refused = append(refused, h.callOf(s, groupOpenID))
			continue
		}
		if _, err := h.send(ctx, messaging.Message{GroupOpenID: groupOpenID, Text: text}); err != nil {
			h.logger(groupOpenID).Error("could not post a broadcast", "error", err)
			refused = append(refused, h.callOf(s, groupOpenID))
			continue
		}
		sent = append(sent, groupOpenID)
		h.logger(groupOpenID).Info("a broadcast was posted", "from", s.groupOpenID,
			"member", s.starter, "token", s.token, "anonymous", s.anonymous == on,
			"markdown", s.markdown == on)
	}

	h.forget(s)
	h.recallCards(ctx, s)

	h.deps.Logger.Info("a broadcast was finished", "from", s.groupOpenID,
		"member", s.starter, "posted_to", groupLogLine(sent),
		"refused", groupLogLine(refused))
	if len(refused) > 0 {
		return h.answerWith(press, qqbotsdk.InteractionCodeSuccess,
			"已发出 "+strconv.Itoa(len(sent))+" 个群；这些没有发出："+
				strings.Join(refused, "、")+"（群没开主动推送，或平台拒绝了这次发送）。")
	}
	return h.answerWith(press, qqbotsdk.InteractionCodeSuccess,
		"已发出 "+strconv.Itoa(len(sent))+" 个群。")
}

// acceptsUnaskedMessages reports whether a group lets the bot speak without being
// asked, which is the only way a broadcast can arrive there.
func (h *handler) acceptsUnaskedMessages(ctx context.Context, groupOpenID string) bool {
	state, err := h.deps.Client.GetGroupBotState(ctx, groupOpenID)
	if err != nil {
		h.logger(groupOpenID).Warn("could not read whether this group accepts a "+
			"message of the bot's own", "error", err)
		return false
	}
	return state.AllowProactiveMsg
}

// callOf is what a group is called in an answer to the administrator.
func (h *handler) callOf(s *session, groupOpenID string) string {
	for _, group := range s.groups {
		if group.openID == groupOpenID {
			return displayName(group)
		}
	}
	return groupOpenID
}

// mayWork reports whether a press is one this card has to answer.
func (h *handler) mayWork(s *session, press command.Press) bool {
	data := press.Data
	if data == nil || data.Scene != qqbotsdk.InteractionSceneGroup {
		return false
	}
	if data.GroupOpenID != s.groupOpenID || data.GroupMemberOpenID != s.starter {
		return false
	}
	// Checked again here rather than only when the card was opened: the
	// administrator list can change while a card is open, and a broadcast is not
	// something to finish after being taken off the list.
	return h.admins != nil && h.admins.IsAdmin(s.groupOpenID, data.GroupMemberOpenID)
}

// answer reports the outcome to the client that pressed.
func (h *handler) answer(press command.Press, code qqbotsdk.InteractionCode) error {
	return h.answerWith(press, code, "")
}

// answerWith is answer, with something said in the group as well.
//
// Every path through a press has to answer, because an unanswered interaction leaves
// the presser looking at a spinner until it times out. It runs under the feature's own
// context, so that a press is never answered by an instance that has already been
// replaced.
func (h *handler) answerWith(press command.Press, code qqbotsdk.InteractionCode,
	said string) error {
	ctx, cancel := context.WithTimeout(h.part, pressTimeout)
	defer cancel()

	if said != "" && press.Data != nil && press.Data.GroupOpenID != "" {
		explanation := messaging.Message{
			GroupOpenID: press.Data.GroupOpenID,
			Text:        said,
		}
		if press.EventID != "" {
			explanation.ReplyToEvent = press.EventID
		}
		if _, err := h.send(ctx, explanation); err != nil {
			h.logger(press.Data.GroupOpenID).Warn("could not say why a press was not "+
				"taken", "error", err)
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
