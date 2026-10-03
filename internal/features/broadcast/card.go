package broadcast

import (
	"context"
	"strings"
	"sync"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/disclaimer"
)

// buttonPrefix is the namespace of every button on a broadcast card.
//
// It is what tells a press apart from the buttons another feature put under
// something: the data a button carries is the only thing that says whose it is, and
// the press arrives with no other clue.
const buttonPrefix = "qgb-broadcast:"

// markdownRule is the divider the platform draws itself.
//
// Three dashes alone on a line are a horizontal rule in markdown, so a message the
// platform renders gets the divider it draws rather than one made of characters.
const markdownRule = "---"

// plainDivider is the divider for a message the platform does not render.
//
// A run of characters, because the rule above is only a rule when markdown is rendered:
// taken literally it is three dashes, which looks like a mistake rather than a divider.
// Rich text would be the third case and is not supported, so the rule here is the one a
// message that is not rendered needs.
const plainDivider = "————————————"

// labelLimit is what a button's label holds.
//
// The platform caps a label at ten characters, and counts a Chinese one as two --
// which is how a group name has to be shortened before it goes on a button.
const labelLimit = 10

// action is what a button on the card asks for.
//
// extra is the one thing a press has to say besides which card and what was asked for,
// and what it means depends on the kind: which group, for a group's own switch, and which
// page, for a record being read a page at a time. One field rather than two, because a
// press never needs both.
type action struct {
	token string
	kind  string
	extra string
}

// The kinds of button there are on a card.
const (
	kindMarkdown  = "md"
	kindAnonymous = "anon"
	kindRichText  = "rich"
	kindGroup     = "group"
	kindConfirm   = "confirm"
	kindBack      = "back"
	kindContinue  = "continue"
	kindCancel    = "cancel"
	kindSend      = "send"
	kindEdit      = "edit"

	// The page buttons of a record: the same namespace, and a press on one is told from a
	// press on a card by knowing these, so that a page never has to be looked up in the
	// cards that are open.
	kindPagePrev  = "pprev"
	kindPageNext  = "pnext"
	kindPageClose = "pclose"
)

// isPage reports whether this press is one of a record's page buttons.
func (a action) isPage() bool {
	switch a.kind {
	case kindPagePrev, kindPageNext, kindPageClose:
		return true
	default:
		return false
	}
}

// buttonData is what goes in a button's data field.
func buttonData(token, kind, extra string) string {
	data := buttonPrefix + token + ":" + kind
	if extra != "" {
		data += ":" + extra
	}
	return data
}

// readAction takes apart what a press carried.
//
// The payload is the data with the namespace already taken off, which is what the
// command layer hands over: what is left is the record or card it belongs to, what was
// asked for, and at most one more thing.
func readAction(payload string) (action, bool) {
	parts := strings.Split(payload, ":")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return action{}, false
	}
	asked := action{token: parts[0], kind: parts[1]}
	if len(parts) > 2 {
		asked.extra = parts[2]
	}
	return asked, true
}

// cardText is what the card says above its buttons: the broadcast so far.
func (h *handler) cardText(s *session) string {
	lines := []string{
		"**群广播**",
		"",
		"Markdown 渲染：" + s.markdown.words(),
		"匿名发送：" + s.anonymous.words(),
		"富文本：平台还不支持，选不了",
		"",
		"发送到：" + chosenNames(s),
	}
	if waiting := s.missing(); len(waiting) > 0 {
		lines = append(lines, "", "还要选："+strings.Join(waiting, "、"))
	} else {
		lines = append(lines, "", "都选好了，按“确认”继续。")
	}
	return strings.Join(lines, "\n")
}

// cardKeyboard is the buttons under it.
func (h *handler) cardKeyboard(s *session) *qqbotsdk.Keyboard {
	rows := []qqbotsdk.Row{
		{Buttons: []qqbotsdk.Button{toggleButton("MD", kindMarkdown, s.markdown, s.token)}},
		{Buttons: []qqbotsdk.Button{toggleButton("匿名", kindAnonymous, s.anonymous, s.token)}},
		{Buttons: []qqbotsdk.Button{plainButton("富文本✗", kindRichText, s.token, "")}},
	}
	rows = append(rows, groupRows(s)...)
	rows = append(rows, qqbotsdk.Row{Buttons: []qqbotsdk.Button{
		plainButton("确认", kindConfirm, s.token, ""),
		plainButton("取消", kindCancel, s.token, ""),
	}})
	return &qqbotsdk.Keyboard{Content: &qqbotsdk.KeyboardContent{Rows: rows}}
}

// groupRows lays the groups out, four to a row.
func groupRows(s *session) []qqbotsdk.Row {
	var rows []qqbotsdk.Row
	var row qqbotsdk.Row
	for _, group := range s.groups {
		row.Buttons = append(row.Buttons, groupButton(s.token, group))
		if len(row.Buttons) == 4 {
			rows = append(rows, row)
			row = qqbotsdk.Row{}
		}
	}
	if len(row.Buttons) > 0 {
		rows = append(rows, row)
	}
	return rows
}

// groupButton is one group, with a mark in front of it while it is selected.
func groupButton(token string, group groupChoice) qqbotsdk.Button {
	label := groupLabel(group)
	if group.selected {
		label = fitLabel("✓" + label)
	}
	return button(label, kindGroup, token, group.openID, qqbotsdk.KeyboardStyleGrey)
}

// groupLabel is what a group is called on a button.
func groupLabel(group groupChoice) string {
	if group.name != "" {
		return fitLabel(group.name)
	}
	// A group whose name could not be read is still a group the broadcast can go
	// to, and it has to be recognisable: the tail of its openid is all there is.
	name := group.openID
	if len(name) > 4 {
		name = name[len(name)-4:]
	}
	return fitLabel("群 " + name)
}

// toggleButton is one option, showing what it is set to.
func toggleButton(label, kind string, state chosen, token string) qqbotsdk.Button {
	style := qqbotsdk.KeyboardStyleGrey
	if state != unset {
		style = qqbotsdk.KeyboardStyleBlue
	}
	return button(fitLabel(label+"："+state.words()), kind, token, "", style)
}

// plainButton is a button that asks for something rather than choosing.
func plainButton(label, kind, token, extra string) qqbotsdk.Button {
	return button(label, kind, token, extra, qqbotsdk.KeyboardStyleBlue)
}

// button is one button on the card.
//
// Every button is a callback: nothing here types a command for the member, because
// what a press means is decided when it arrives rather than by what the composer
// was given.
func button(label, kind, token, extra string, style int) qqbotsdk.Button {
	// Unique within the keyboard, which is what the platform asks of an id: the
	// kind alone repeats -- every group has its own button -- so the extra field is
	// part of it where there is one.
	id := kind
	if extra != "" {
		id += ":" + extra
	}
	return qqbotsdk.Button{
		ID: id,
		RenderData: &qqbotsdk.RenderData{
			Label:        label,
			VisitedLabel: label,
			Style:        style,
		},
		Action: &qqbotsdk.Action{
			Type: qqbotsdk.ActionTypeCallback,
			Data: buttonData(token, kind, extra),
			// Everybody: the platform's administrator gate is about QQ's
			// administrators, and who may work this card is decided here, against
			// the list the bot obeys. Greying the buttons out for the wrong people
			// would hide the card from its own administrator.
			Permission:    &qqbotsdk.Permission{Type: qqbotsdk.PermissionTypeEveryone},
			UnsupportTips: "请升级 QQ 客户端",
		},
	}
}

// fitLabel shortens what a button cannot hold.
//
// The platform counts a Chinese character as two, and refuses a label that is too
// long: shortening it here keeps the card sendable, and what is left is still
// enough to tell one group from another.
func fitLabel(text string) string {
	width := 0
	var kept []rune
	for _, letter := range text {
		step := 1
		if letter > 0x7f {
			step = 2
		}
		if width+step > labelLimit {
			break
		}
		width += step
		kept = append(kept, letter)
	}
	return string(kept)
}

// chosenNames lists the groups this broadcast goes to, for the card's own text.
func chosenNames(s *session) string {
	var names []string
	for _, group := range s.groups {
		if group.selected {
			names = append(names, displayName(group))
		}
	}
	if len(names) == 0 {
		return "还没选"
	}
	return strings.Join(names, "、")
}

// displayName is a group's full name, for somewhere that has room for it.
func displayName(group groupChoice) string {
	if group.name != "" {
		return group.name
	}
	return "群 " + group.openID
}

// broadcastText is the message the groups will see.
//
// It is built from the same switches the card shows, and the preview is this function's own
// answer: what the writer approves is the message, not a description of one. That includes
// the disclaimer at the end of it, which is a line of the message rather than something added
// on the way out.
//
// forGroups is which of the two places it is being written for, because one line of it cannot
// be the same in both: the platform's mention tag names a member in a group and a single chat
// refuses a message carrying one whole (40034106, measured), so the preview of a署名 notice
// says "你" where the notice says their name.
func (h *handler) broadcastText(s *session, forGroups bool) string {
	rendered, signed := s.markdown == on, s.signed()

	header := "来自管理员的广播"
	if signed {
		// The name is the one part of this line that has to arrive, so it is written the way
		// the platform reads as a mention and nothing is wrapped around it: whether a tag
		// inside emphasis is still read as a tag is not something measured here, and the
		// cost of being wrong is a署名 notice that reads exactly like an anonymous one.
		header = "来自 " + namedIn(s.chat, forGroups) + " 的广播"
	} else if rendered {
		// Bold where there is no name to get wrong.
		header = "**" + header + "**"
	}

	divider, body, gap := plainDivider, escapeMarkdown(s.content), "\n"
	if rendered {
		// Rendered as markdown: the divider is the rule the platform draws, and the blank line
		// in front of it is load-bearing rather than spacing. A rule on the line straight under
		// a line of text is a setext heading underline, which made the header a big title and
		// drew no divider at all -- measured in a group, not reasoned about.
		divider, body, gap = markdownRule, s.content, "\n\n"
	}
	return disclaimer.After(header+gap+divider+"\n"+body, divider)
}

// namedIn is how a member is written into a message, which depends on where it is going.
//
// In a group the platform's mention tag is what renders as their name, and it is the only
// spelling that does: written as plain text it is a string of characters nobody can read,
// which is a署名 notice that looks anonymous. A single chat is the other way round -- it
// refuses the tag whole -- so the preview of one says "你", which is who reads it.
func namedIn(openID string, forGroups bool) string {
	if forGroups {
		return `<qqbot-at-user id="` + openID + `" />`
	}
	return "你"
}

// escapeMarkdown takes the meaning out of the text.
//
// What an administrator wrote is what the groups read, which is what turning
// markdown rendering off means: without this, a message full of asterisks would
// quietly come out as bold text instead.
func escapeMarkdown(text string) string {
	const special = "\\`*_{}[]()#+-.!>~|"
	var escaped strings.Builder
	for _, letter := range text {
		if strings.ContainsRune(special, letter) {
			escaped.WriteRune('\\')
		}
		escaped.WriteRune(letter)
	}
	return escaped.String()
}

// groupChoices are the groups this member may broadcast into, with what to call them.
//
// Only the groups they administer: a card that offered the others would be offering
// something the send would refuse, and being on one group's administrator list is not
// being on another's.
//
// The names are read when a card is opened rather than on every press: the card is
// sent again each time a button is pressed, and a call per press for a label is paid
// for by whoever is looking at it. They are read together and under one budget,
// because a group that does not answer must not hold up the card -- and a group
// whose name cannot be read is still a group a broadcast can go to.
func (h *handler) groupChoices(ctx context.Context, memberOpenID string) []groupChoice {
	var candidates []string
	for _, group := range h.deps.Groups {
		if h.adminsOf(group.OpenID, memberOpenID) {
			candidates = append(candidates, group.OpenID)
		}
	}

	named, cancel := context.WithTimeout(ctx, namingBudget)
	defer cancel()

	choices := make([]groupChoice, len(candidates))
	var wg sync.WaitGroup
	for index, openID := range candidates {
		choices[index] = groupChoice{openID: openID}
		wg.Add(1)
		go func(index int, openID string) {
			defer wg.Done()
			info, err := h.deps.Client.GetGroupInfo(named, openID)
			if err != nil {
				h.logger(openID).Debug("a group's name could not be read, so it is "+
					"called by its id", "error", err)
				return
			}
			choices[index].name = info.GroupName
		}(index, openID)
	}
	wg.Wait()
	return choices
}

// namingBudget is how long the card waits for the group names.
const namingBudget = 3 * time.Second
