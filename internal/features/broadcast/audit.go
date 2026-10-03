package broadcast

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/disclaimer"
	"github.com/fouc3/qq-group-management-bot/internal/messaging"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// How much of the record one page shows, and how far back it is read.
//
// A page is bounded because a chat message is: five of these entries plus the disclaimer is
// a screenful, and a record nobody scrolls is a record nobody reads. The reading is bounded
// for the same reason one step earlier -- the whole table is not something to pull into
// memory to answer "who sent that", ten pages of it is enough to find them in, and a record
// kept for its own sake is read out of the database rather than out of a chat.
const (
	auditPageSize = 5
	auditMaxPages = 10
)

// auditLineLimit is how much of a broadcast one line of the record shows.
//
// Cut rather than shown whole: what the line is for is recognising a notice, and the words
// themselves belong to the groups it went to.
const auditLineLimit = 40

// pager is a record being read a page at a time in one place.
//
// It is held rather than derived from the press, because the platform's interaction event
// says which button was pressed and nothing about which message carried it -- and turning a
// page has to take the last one back. So who is reading, where, and what is on screen now
// are remembered here, and a press only says which way to turn.
type pager struct {
	token string
	// groupOpenID is the group the record is about, or empty for a single chat reading the
	// groups its owner administers.
	groupOpenID string
	// member is who may turn the pages: the member who asked. In a group it is that group's
	// administrator, and in a single chat it is the one whose groups are shown.
	member string
	// messageID is the page on screen now, so that turning it can take it back and the chat
	// is not left reading a pile of pages.
	messageID string
	updated   time.Time
}

// rememberPage records a record being read, and starts its life over.
//
// The stamping happens here rather than at every construction: a pager built without it
// looks like one nobody has touched since the year zero, which the sweep drops on the next
// press -- a page whose buttons answer "过期" from the moment they arrive.
func (h *handler) rememberPage(p *pager) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropExpired()
	p.updated = time.Now()
	h.pages[p.token] = p
}

// lookupPage returns a record being read.
func (h *handler) lookupPage(token string) (*pager, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropExpired()
	p, found := h.pages[token]
	return p, found
}

// forgetPage stops a record being read, so that a press on an old page cannot bring it back.
func (h *handler) forgetPage(p *pager) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.pages, p.token)
}

// auditCommand shows what was broadcast in this group, and who asked for it.
//
// Asked in a group, and about that group: the record is what the group's own administrators
// get to see, which is the answer to a notice they cannot trace otherwise.
//
// It is not behind the trial gate. The gate is about writing a broadcast -- the half that
// puts words in front of a group -- and this is the half that makes an anonymous notice
// answerable at all: a group whose administrators cannot read the record of their own group
// has no way to find out what was sent in its name.
func (h *handler) auditCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	_ command.Parsed) error {
	member := data.Author.MemberOpenID
	// Asked again here rather than left to the table's own gate: who may read a record of
	// who said what is worth one check of its own, and the two must agree.
	if !h.adminsOf(data.GroupOpenID, member) {
		return h.say(ctx, data.GroupOpenID, "", "只有本群管理员能看广播记录。")
	}
	return h.showRecord(ctx, pager{
		token:       newTokenOrNothing(),
		groupOpenID: data.GroupOpenID,
		member:      member,
	}, 1, data.ID, nil)
}

// auditPrivately shows a member what was broadcast in the groups they administer.
//
// In a single chat, and only about their own groups: the record is what somebody who
// answers for a group needs, and a list of what other groups were told is not theirs to
// read. Not behind the trial gate either, for the same reason as the group's own.
func (h *handler) auditPrivately(ctx context.Context, data *qqbotsdk.C2CMessageCreateData,
	_ command.Parsed) error {
	chat := data.Author.UserOpenID
	if len(h.administers(chat)) == 0 {
		return h.say(ctx, chat, "", "你不在任何群的管理员名单里，没有可看的广播记录。")
	}
	return h.showRecord(ctx, pager{token: newTokenOrNothing(), member: chat}, 1, data.ID, nil)
}

// onPagePress turns a page of the record.
func (h *handler) onPagePress(ctx context.Context, press command.Press, asked action) error {
	p, found := h.lookupPage(asked.token)
	if !found {
		// The record outlived its reading, or the feature was built again since it was
		// opened. Said out loud rather than ignored, because the presser is looking at a
		// page that will never turn.
		return h.answerWith(press, qqbotsdk.InteractionCodeFailed,
			"这条广播记录已经过期，请重新发 /广播审计。")
	}
	if !h.mayRead(p, press.Data) {
		h.loggerIn(p.member).Warn("refused a press on somebody else's record",
			"pressed_by", presserOf(press.Data))
		return h.answerWith(press, qqbotsdk.InteractionCodeAdminOnly,
			"这条广播记录不是发起它的人在翻。")
	}

	page, err := strconv.Atoi(asked.extra)
	if err != nil {
		return h.answer(press, qqbotsdk.InteractionCodeFailed)
	}
	if asked.kind == kindPageClose {
		// Closing takes the page away rather than leaving it: the record is a thing to
		// consult, and a chat left holding one has nobody to tell it is finished with.
		h.forgetPage(p)
		h.recallPage(ctx, *p)
		return h.answer(press, qqbotsdk.InteractionCodeSuccess)
	}

	// The button carries the page it goes to rather than a direction to work out: what a
	// page holds is read from the store as it is sent, so a direction would be a second
	// opinion about something the record already knows.
	return h.showRecord(ctx, *p, page, "", &press)
}

// showRecord sends one page of the record, taking the page before it away.
//
// replyTo is the message a record asked for by a command answers; press is the button that
// turned it, when there is one. Both are passive answers, so reading a record costs nothing
// from what the bot may say unasked.
func (h *handler) showRecord(ctx context.Context, p pager, page int, replyTo string,
	press *command.Press) error {
	groups := h.recordGroups(p)
	posted, err := h.records(ctx, groups, auditMaxPages*auditPageSize)
	if err != nil {
		h.loggerIn(p.member).Error("could not read the broadcast record", "error", err)
		return h.say(ctx, p.where(), replyTo, "广播记录读取失败："+err.Error())
	}

	pages := (len(posted) + auditPageSize - 1) / auditPageSize
	if pages < 1 {
		pages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}

	message := messaging.Message{
		Text:     h.auditText(ctx, p, posted, page, pages),
		Keyboard: pageKeyboard(p, page, pages),
	}
	if p.token == "" {
		// No token means no working buttons (see newTokenOrNothing): a page that can be read
		// but not turned is worth more than no page at all, and a button that cannot be
		// answered leaves whoever presses it watching a spinner.
		message.Keyboard = nil
	}
	if p.groupOpenID != "" {
		message.GroupOpenID = p.groupOpenID
	} else {
		message.UserOpenID = p.member
	}
	if press != nil {
		message.ReplyToEvent = press.EventID
	} else {
		message.ReplyTo = replyTo
	}

	response, err := h.send(ctx, message)
	if err != nil && press != nil {
		// The window on an answer closes. A record being read is still wanted, so the page
		// goes out on its own rather than not at all.
		h.loggerIn(p.member).Warn("a page could not be sent as an answer to the press, "+
			"so it is being sent on its own", "error", err)
		message.ReplyToEvent = ""
		if response, err = h.send(ctx, message); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}

	h.recallPage(ctx, p)
	if response != nil {
		p.messageID = response.ID
	}
	h.rememberPage(&p)

	if press != nil {
		return h.answer(*press, qqbotsdk.InteractionCodeSuccess)
	}
	return nil
}

// recallPage takes the page before this one back.
//
// Best effort: the platform only lets a bot take its own message back for a couple of
// minutes, and a page left behind is readable rather than harmful -- its buttons still work,
// which is why they carry the page number.
func (h *handler) recallPage(ctx context.Context, p pager) {
	if p.messageID == "" {
		return
	}
	if p.groupOpenID != "" {
		if err := h.deps.Client.RecallGroupMessage(ctx, p.groupOpenID, p.messageID); err != nil {
			h.loggerIn(p.member).Debug("an earlier page could not be taken back", "error", err)
		}
		return
	}
	if err := h.deps.Client.RecallC2CMessage(ctx, p.member, p.messageID); err != nil {
		h.loggerIn(p.member).Debug("an earlier page could not be taken back", "error", err)
	}
}

// recordGroups are the groups a reading covers.
func (h *handler) recordGroups(p pager) []string {
	if p.groupOpenID != "" {
		return []string{p.groupOpenID}
	}
	return h.administers(p.member)
}

// where is the endpoint one of this record's replies goes to.
func (p pager) where() string {
	if p.groupOpenID != "" {
		return p.groupOpenID
	}
	return p.member
}

// covers is the part of the header that says what the record is about.
func (p pager) covers(groups int) string {
	if p.groupOpenID != "" {
		return "本群"
	}
	return "你管理的 " + strconv.Itoa(groups) + " 个群"
}

// mayRead reports whether a press is one this record has to answer.
//
// It is the same question as who may ask in the first place, asked again on every turn: the
// page a press is on says nothing about who is allowed to read it, and in a group the
// presser is whoever is holding the phone.
func (h *handler) mayRead(p *pager, data *qqbotsdk.InteractionCreateData) bool {
	if data == nil {
		return false
	}
	if p.groupOpenID != "" {
		return data.Scene == qqbotsdk.InteractionSceneGroup &&
			data.GroupOpenID == p.groupOpenID &&
			h.adminsOf(p.groupOpenID, presserOf(data))
	}
	return data.Scene == qqbotsdk.InteractionSceneC2C && data.UserOpenID == p.member
}

// presserOf is who pressed, which each scene names in its own field.
func presserOf(data *qqbotsdk.InteractionCreateData) string {
	if data == nil {
		return ""
	}
	if data.Scene == qqbotsdk.InteractionSceneC2C {
		return data.UserOpenID
	}
	return data.GroupMemberOpenID
}

// pageKeyboard is under a page of the record.
//
// Only the directions that lead somewhere: a button that answers "there is nothing there" is
// a button that should not have been shown.
func pageKeyboard(p pager, page, pages int) *qqbotsdk.Keyboard {
	var buttons []qqbotsdk.Button
	if page > 1 {
		buttons = append(buttons,
			plainButton("上一页", kindPagePrev, p.token, strconv.Itoa(page-1)))
	}
	if page < pages {
		buttons = append(buttons,
			plainButton("下一页", kindPageNext, p.token, strconv.Itoa(page+1)))
	}
	buttons = append(buttons,
		plainButton("关闭", kindPageClose, p.token, strconv.Itoa(page)))
	return &qqbotsdk.Keyboard{Content: &qqbotsdk.KeyboardContent{Rows: []qqbotsdk.Row{
		{Buttons: buttons},
	}}}
}

// auditText is one page of the record.
//
// The disclaimer is at the end of it because the page quotes what somebody else wrote: the
// record says who asked for a notice, and the notice's own first line is theirs.
//
// Every group is named rather than identified: an openid is not something a person can read,
// and "which group" is half of what the record is for. Names come from the platform and are
// read once per page, with the same budget the card's group list uses.
func (h *handler) auditText(ctx context.Context, p pager, posted []store.Broadcast, page,
	pages int) string {
	start := (page - 1) * auditPageSize
	end := start + auditPageSize
	if start > len(posted) {
		start = len(posted)
	}
	if end > len(posted) {
		end = len(posted)
	}

	lines := []string{
		"**广播记录**（" + p.covers(len(h.recordGroups(p))) + "，共 " + strconv.Itoa(len(posted)) +
			" 条）第 " + strconv.Itoa(page) + " 页 / 共 " + strconv.Itoa(pages) + " 页",
		"",
	}
	if len(posted) == 0 {
		lines = append(lines, "还没有广播记录。")
	}
	for index, entry := range posted[start:end] {
		when := time.Unix(entry.SentAt, 0).Format("01-02 15:04")
		how := "匿名发出"
		if !entry.Anonymous {
			how = "署名发出"
		}
		lines = append(lines,
			strconv.Itoa(start+index+1)+". "+when+" · "+strings.Join(h.targetNames(ctx, p, entry), "、")+
				" · "+how+" · 发起人 `"+entry.SenderOpenID+"`",
			"   "+firstLine(entry.Content, auditLineLimit))
	}
	return disclaimer.After(strings.Join(lines, "\n"), markdownRule)
}

// targetNames are the groups a notice reached that this reading is allowed to name.
//
// Only the ones it covers: a notice can reach several groups, and which other groups the bot
// was asked to post in is not what somebody answering for theirs is reading the record for.
func (h *handler) targetNames(ctx context.Context, p pager, entry store.Broadcast) []string {
	mine := map[string]bool{}
	for _, groupOpenID := range h.recordGroups(p) {
		mine[groupOpenID] = true
	}
	var names []string
	for _, target := range entry.Targets {
		if !mine[target.GroupOpenID] {
			continue
		}
		names = append(names, h.callOfGroup(ctx, target.GroupOpenID))
	}
	if len(names) == 0 {
		// A record whose targets are all somewhere else, which a group's own reading cannot
		// see. Said rather than left blank, so that the line does not look truncated.
		return []string{"本群之外的群"}
	}
	return names
}

// nameOf is one group's name, read from the platform, or an empty one when it will not say.
//
// Under the same budget the card's group list uses: a record is a message somebody is waiting
// for, and a group that does not answer must not hold it up.
func (h *handler) nameOf(ctx context.Context, group config.Group) groupChoice {
	named, cancel := context.WithTimeout(ctx, namingBudget)
	defer cancel()

	choice := groupChoice{openID: group.OpenID}
	info, err := h.deps.Client.GetGroupInfo(named, group.OpenID)
	if err != nil {
		h.logger(group.OpenID).Debug("a group's name could not be read, so it is called "+
			"by its id", "error", err)
		return choice
	}
	choice.name = info.GroupName
	return choice
}

// callOfGroup is what a group is called in a record: its name, or the openid when the platform
// will not say.
func (h *handler) callOfGroup(ctx context.Context, groupOpenID string) string {
	for _, group := range h.deps.Groups {
		if group.OpenID == groupOpenID {
			return displayName(h.nameOf(ctx, group))
		}
	}
	// A group that reached this record and is not in the configuration any more: named by what
	// is left of it rather than left out, because the line is about where a notice went.
	return displayName(groupChoice{openID: groupOpenID})
}

// records reads the newest broadcasts that reached any of these groups, newest first.
func (h *handler) records(ctx context.Context, groups []string, limit int) ([]store.Broadcast, error) {
	if h.deps.Store == nil {
		return nil, errors.New("no data layer")
	}
	// Asked of the store once for all of them rather than once per group: a notice that
	// reached two of these groups is one record, and asking per group would return it twice.
	return h.deps.Store.Broadcasts().ListByGroups(ctx, groups, limit)
}

// newTokenOrNothing is a page's token, or empty when one cannot be made.
//
// The only way that fails is a broken random source. An empty token is not a token: the page
// goes out without buttons rather than with ones that would answer nothing, because two
// records sharing a token would also mean one member turning another's pages.
func newTokenOrNothing() string {
	token, err := newToken()
	if err != nil {
		return ""
	}
	return token
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
