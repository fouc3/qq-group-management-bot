package broadcast

import (
	"context"
	"strings"
	"testing"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/disclaimer"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// record writes one broadcast into the data layer, the way a finished one would.
//
// Written straight to the store rather than posted through the card: a test about reading a
// record of a busy group wants a record of a busy group, not seven trips through the flow.
// Each one is a second later than the last, so that "newest first" is a fact rather than a
// tie broken by whichever collation the database happens to use.
func (p *platform) record(t *testing.T, content string) {
	t.Helper()
	p.recorded++
	entry := store.Broadcast{
		Token:        "TOKEN-" + content,
		SenderOpenID: theAdmin,
		GroupOpenID:  hereGroup,
		Anonymous:    true,
		Markdown:     true,
		Content:      content,
		SentAt:       time.Now().Unix() + int64(p.recorded),
		MessageID:    "MESSAGE-" + content,
	}
	if err := p.store.Broadcasts().Record(context.Background(), entry); err != nil {
		t.Fatalf("recording %q: %v", content, err)
	}
}

// askPrivately asks for the record in a single chat, the way the command does.
func (p *platform) askPrivately(t *testing.T, member string) {
	t.Helper()
	data := &qqbotsdk.C2CMessageCreateData{
		ID: "AUDIT-CHAT", Content: "/广播审计",
		Author: &qqbotsdk.User{UserOpenID: member},
	}
	if err := p.handler.auditPrivately(context.Background(), data, command.Parsed{}); err != nil {
		t.Fatalf("asking for the record: %v", err)
	}
}

// askInGroupForRecord asks for the record in a group, the way the command does.
func (p *platform) askInGroupForRecord(t *testing.T, group, member string) {
	t.Helper()
	data := &qqbotsdk.GroupMessageCreateData{
		ID: "AUDIT-GROUP", GroupOpenID: group,
		Author: &qqbotsdk.User{MemberOpenID: member},
	}
	if err := p.handler.auditCommand(context.Background(), data, command.Parsed{}); err != nil {
		t.Fatalf("asking for the record: %v", err)
	}
}

// pressInGroup is press, for a page read in a group rather than in a single chat.
func (p *platform) pressInGroup(t *testing.T, label, group, member string) {
	t.Helper()
	data, ok := p.cardButtons()[label]
	if !ok {
		t.Fatalf("no button labelled %q on the page:\n%s", label, p.lastText())
	}
	press := command.Press{
		Data: &qqbotsdk.InteractionCreateData{
			ID:                "INTERACTION-" + label,
			Scene:             qqbotsdk.InteractionSceneGroup,
			GroupOpenID:       group,
			GroupMemberOpenID: member,
		},
		EventID: "EVENT-" + label,
		Payload: strings.TrimPrefix(data, buttonPrefix),
	}
	if err := p.handler.onPress(context.Background(), press); err != nil {
		t.Fatalf("pressing %q: %v", label, err)
	}
}

// TestTheRecordTurnsOnePageAtATime covers the reading: five entries a page, and the page
// before it taken back so that the chat is not left holding a pile of them.
func TestTheRecordTurnsOnePageAtATime(t *testing.T) {
	p := newPlatform(t)
	for _, content := range []string{"一", "二", "三", "四", "五", "六", "七"} {
		p.record(t, content)
	}
	p.askPrivately(t, theAdmin)

	first := p.lastText()
	if !strings.Contains(first, "第 1/2 页") || !strings.Contains(first, "共 7 条") {
		t.Errorf("the first page does not say where it is:\n%s", first)
	}
	if !strings.Contains(first, "七") || !strings.Contains(first, "三") {
		t.Errorf("the first page does not start at the newest:\n%s", first)
	}
	// Five of the seven: the two oldest are on the next page.
	for _, absent := range []string{"\n   一\n", "\n   二\n"} {
		if strings.Contains(first, absent) {
			t.Errorf("the first page shows more than five entries:\n%s", first)
		}
	}
	// Nothing to go back to on the first page, so no button offering it.
	if buttons := p.cardButtons(); buttons["上一页"] != "" {
		t.Error("the first page offers a page before it")
	}

	before := len(p.recalls)
	p.press(t, "下一页", theAdmin)

	second := p.lastText()
	if !strings.Contains(second, "第 2/2 页") {
		t.Errorf("the second page does not say where it is:\n%s", second)
	}
	if !strings.Contains(second, "二") || !strings.Contains(second, "一") {
		t.Errorf("the second page does not carry what was left over:\n%s", second)
	}
	if len(p.recalls) == before {
		t.Error("the page before was left in the chat")
	}
	if buttons := p.cardButtons(); buttons["下一页"] != "" {
		t.Error("the last page offers a page after it")
	}

	// And back: a page turned twice is a page read twice.
	p.press(t, "上一页", theAdmin)
	if back := p.lastText(); !strings.Contains(back, "第 1/2 页") {
		t.Errorf("turning back does not show the first page:\n%s", back)
	}
}

// TestClosingARecordTakesItAway covers the way out: the page goes, and the record stops
// being something that can be turned.
func TestClosingARecordTakesItAway(t *testing.T) {
	p := newPlatform(t)
	p.record(t, "一")
	p.askPrivately(t, theAdmin)

	before := len(p.recalls)
	p.press(t, "关闭", theAdmin)

	if len(p.recalls) == before {
		t.Error("closing a record left it in the chat")
	}
	// Forgotten rather than merely hidden: the same button pressed again finds nothing.
	p.press(t, "关闭", theAdmin)
	if said := p.lastText(); !strings.Contains(said, "已经过期") {
		t.Errorf("a closed record can still be turned:\n%s", said)
	}
}

// TestARecordOutOfItsTimeSaysSo covers the press that arrives after the reading has been
// dropped: said out loud, because the presser is looking at a page that will never turn.
func TestARecordOutOfItsTimeSaysSo(t *testing.T) {
	p := newPlatform(t)
	p.record(t, "一")
	p.askPrivately(t, theAdmin)

	// What a restart of the feature does to the records: the pages they were holding are
	// gone, and the buttons on them are still in the chat.
	p.handler.mu.Lock()
	p.handler.pages = map[string]*pager{}
	p.handler.mu.Unlock()

	p.press(t, "关闭", theAdmin)
	if said := p.lastText(); !strings.Contains(said, "已经过期") {
		t.Errorf("a record that is no longer held was not said to be expired:\n%s", said)
	}
}

// TestSomebodyElseCannotTurnTheRecord covers who a record belongs to, in both places it can
// be read.
func TestSomebodyElseCannotTurnTheRecord(t *testing.T) {
	// In a single chat: the member who asked, and nobody else.
	private := newPlatform(t)
	private.record(t, "一")
	private.askPrivately(t, theAdmin)
	private.press(t, "关闭", somebodyElse)
	if said := private.lastText(); !strings.Contains(said, "不是发起它的人") {
		t.Errorf("somebody else turned a record in a single chat:\n%s", said)
	}

	// In a group: that group's administrators, whoever is holding the phone.
	group := newPlatform(t, func(p *platform) { p.admins = admins{who: theAdmin} })
	group.record(t, "一")
	group.askInGroupForRecord(t, hereGroup, theAdmin)
	group.pressInGroup(t, "关闭", hereGroup, somebodyElse)
	if said := group.lastText(); !strings.Contains(said, "不是发起它的人") {
		t.Errorf("a member who does not administer the group turned its record:\n%s", said)
	}
	// And a record of this group cannot be turned from another one: the page is refused
	// rather than answered, because a press arriving in a different group is not a turn of
	// this record whatever it carries.
	group.pressInGroup(t, "关闭", otherGroup, theAdmin)
	if said := group.lastText(); !strings.Contains(said, "不是发起它的人") {
		t.Errorf("a record was turned from the wrong group:\n%s", said)
	}
}

// TestTheRecordIsReadEvenWhenTheTrialIsClosed covers the gate's edge.
//
// The trial is about writing a broadcast -- the half that puts words in front of a group.
// Reading the record is the half that makes an anonymous notice answerable, so it is not
// behind the gate: refusing it would leave a group unable to find out what was sent in its
// name.
func TestTheRecordIsReadEvenWhenTheTrialIsClosed(t *testing.T) {
	p := newPlatform(t, withTrial("SOMEBODY-ELSE"))
	p.record(t, "谁发的")

	// The command that writes one is refused, which is the gate doing its work.
	p.start(t, theAdmin)
	if said := p.lastText(); !strings.Contains(said, closed) {
		t.Errorf("the trial did not close the broadcast for a member it does not name:\n%s", said)
	}

	p.askPrivately(t, theAdmin)
	said := p.lastText()
	if strings.Contains(said, closed) {
		t.Fatalf("the record was refused by the trial:\n%s", said)
	}
	if !strings.Contains(said, "谁发的") || !strings.Contains(said, theAdmin) {
		t.Errorf("the record does not say who asked for what:\n%s", said)
	}
	// In a group, where the gate would otherwise be the first thing a member met.
	group := newPlatform(t, withTrial("SOMEBODY-ELSE"))
	group.record(t, "本群的")
	group.askInGroupForRecord(t, hereGroup, theAdmin)
	if said := group.lastText(); !strings.Contains(said, "本群的") {
		t.Errorf("a group's own record was refused by the trial:\n%s", said)
	}
}

// TestTheRecordQuotesOtherPeopleSoItSaysSo covers the disclaimer on the record.
//
// Each line shows the first line of somebody else's notice, so a page is mostly other
// people's words under this bot's name.
func TestTheRecordQuotesOtherPeopleSoItSaysSo(t *testing.T) {
	p := newPlatform(t)
	p.record(t, "一句话")
	p.askPrivately(t, theAdmin)

	if said := p.lastText(); !strings.HasSuffix(said,
		markdownRule+"\n**"+disclaimer.Text+"**") {
		t.Errorf("the record does not end with the disclaimer:\n%s", said)
	}
	// And it survives turning a page, because every page quotes somebody.
	for _, content := range []string{"二", "三", "四", "五", "六"} {
		p.record(t, content)
	}
	p.askPrivately(t, theAdmin)
	p.press(t, "下一页", theAdmin)
	if said := p.lastText(); !strings.Contains(said, disclaimer.Text) {
		t.Errorf("the second page carries no disclaimer:\n%s", said)
	}
}

// TestAnEmptyRecordSaysSoAndSaysSoUnderTheDisclaimer covers the group that never broadcast.
func TestAnEmptyRecordSaysSoAndSaysSoUnderTheDisclaimer(t *testing.T) {
	p := newPlatform(t)
	p.askPrivately(t, theAdmin)

	said := p.lastText()
	if !strings.Contains(said, "还没有广播记录") {
		t.Errorf("an empty record was not said plainly:\n%s", said)
	}
	if !strings.Contains(said, disclaimer.Text) {
		t.Errorf("an empty record carries no disclaimer:\n%s", said)
	}
}
