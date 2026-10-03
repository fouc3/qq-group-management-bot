package broadcast

import (
	"context"
	"strings"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// withTrial builds the feature with the trial gate on and this list of members.
func withTrial(whitelist ...string) func(*platform) {
	return func(p *platform) {
		p.section = "beta:\n  enabled: true\n  whitelist:\n"
		for _, member := range whitelist {
			p.section += "    - " + member + "\n"
		}
	}
}

// TestTheTrialGateRefusesWhatItDoesNotName covers the gate: while the feature is being
// tried out, somebody it does not name is told so rather than given a card.
func TestTheTrialGateRefusesWhatItDoesNotName(t *testing.T) {
	p := newPlatform(t, withTrial("SOMEBODY-ELSE"))
	p.start(t, theAdmin)

	said := p.lastText()
	if !strings.Contains(said, closed) {
		t.Errorf("a member the trial does not name was told:\n%s", said)
	}
	if p.cardButtons()["MD：未选"] != "" {
		t.Error("a card was opened for a member the trial does not name")
	}
}

// TestTheTrialGateLetsTheNamedThrough covers the other half: the members it names are
// not affected in any way.
func TestTheTrialGateLetsTheNamedThrough(t *testing.T) {
	p := newPlatform(t, withTrial(theAdmin))
	p.start(t, theAdmin)

	if card := p.lastText(); !strings.Contains(card, "**群广播**") {
		t.Errorf("a member the trial names did not get a card:\n%s", card)
	}
}

// TestTheTrialGateAlsoClosesAGroup covers the gate where a group asks: the members it
// does not name are refused there too, before being told anything about how the feature
// works.
func TestTheTrialGateAlsoClosesAGroup(t *testing.T) {
	p := newPlatform(t, withTrial("SOMEBODY-ELSE"))
	p.askInGroup(t, theAdmin)

	if said := p.lastText(); !strings.Contains(said, closed) {
		t.Errorf("a group the trial does not name was told:\n%s", said)
	}
}

// TestABroadcastIsOnlyWrittenInASingleChat covers where the card can be opened.
//
// A draft read by the group is the one thing a broadcast cannot be: the group is who the
// notice is for, and watching it being written gives away the part anonymity is about. A
// group that asks is told where to write one.
func TestABroadcastIsOnlyWrittenInASingleChat(t *testing.T) {
	p := newPlatform(t)
	p.askInGroup(t, theAdmin)

	said := p.lastText()
	if !strings.Contains(said, "私聊") {
		t.Errorf("a group was not told where a broadcast is written:\n%s", said)
	}
	// And no card was opened anywhere: the group was answered, and that is all.
	if strings.Contains(said, "**群广播**") {
		t.Errorf("a card was opened in a group:\n%s", said)
	}
}

// TestTheCommandIsOfferedInThePrivatePanel covers the panels this command asks for.
//
// The single chat is where a broadcast is written, so that is the panel it belongs in;
// a group's menu offering it would put a command in front of a group that answers "not
// here".
func TestTheCommandIsOfferedInThePrivatePanel(t *testing.T) {
	p := newPlatform(t)
	var broadcast, audit *command.Def
	defs := p.handler.CommandDefs()
	for index := range defs {
		switch defs[index].Name {
		case "群广播":
			broadcast = &defs[index]
		case "广播审计":
			audit = &defs[index]
		}
	}
	if broadcast == nil || audit == nil {
		t.Fatal("the feature does not offer both of its commands")
	}
	if broadcast.Private == nil {
		t.Error("a single chat cannot write a broadcast, so the private panel would " +
			"offer a command that answers nothing")
	}
	if len(broadcast.Panels) != 1 || broadcast.Panels[0].Scene != command.InPrivate {
		t.Errorf("the broadcast is offered in %+v, want the single chat alone",
			broadcast.Panels)
	}
	// A record is consulted rather than offered: an entry in the menu would put a list of
	// names in front of a group.
	if len(audit.Panels) != 0 {
		t.Errorf("the audit is offered in a panel: %+v", audit.Panels)
	}
	if audit.Private == nil || audit.PrivateUsage == "" {
		t.Error("a single chat cannot read the record, so nothing says it can be asked " +
			"there")
	}
}

// TestTheCardOffersOnlyTheGroupsYouAdminister covers what a card may choose from.
//
// Being on one group's administrator list is not being on another's, so a card that
// offered every group the bot manages would be offering something the send would refuse.
func TestTheCardOffersOnlyTheGroupsYouAdminister(t *testing.T) {
	p := newPlatform(t, func(p *platform) {
		p.admins = admins{who: theAdmin, groups: []string{otherGroup}}
	})
	p.start(t, theAdmin)

	buttons := p.cardButtons()
	if _, ok := buttons["群-OTHER"]; !ok {
		t.Errorf("the group this member administers is not offered: %v", buttons)
	}
	for _, absent := range []string{"群-HERE", "群-THIRD"} {
		if _, ok := buttons[absent]; ok {
			t.Errorf("a group this member does not administer is offered: %s", absent)
		}
	}
}

// TestABroadcastCanBeWrittenInASingleChat covers the whole flow: the card, the choices,
// the text, and the groups it goes to.
func TestABroadcastCanBeWrittenInASingleChat(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)

	if card := p.lastText(); !strings.Contains(card, "**群广播**") {
		t.Fatalf("no card in the single chat:\n%s", card)
	}
	p.press(t, "MD：未选", theAdmin)
	p.press(t, "匿名：未选", theAdmin)
	p.press(t, "群-OTHER", theAdmin)
	p.press(t, "确认", theAdmin)
	p.press(t, "继续", theAdmin)
	p.say(t, theAdmin, "从私聊发的广播")

	p.press(t, "发送", theAdmin)

	posted := p.notices()
	if len(posted) != 1 {
		t.Fatalf("the broadcast was posted %d time(s), want once: %v", len(posted), posted)
	}
	if !strings.Contains(posted[0], "从私聊发的广播") {
		t.Errorf("the group reads:\n%s", posted[0])
	}
}

// TestSomebodyElseCannotWorkACard covers who a card belongs to: the member who opened it,
// in that chat. There is no group to be an administrator of, so there is nobody else it
// could be.
func TestSomebodyElseCannotWorkACard(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)

	before := len(p.sent)
	p.press(t, "MD：未选", somebodyElse)

	if len(p.sent) != before+1 {
		t.Errorf("a press from somebody else sent %d message(s), want only the refusal",
			len(p.sent)-before)
	}
	if said := p.lastText(); !strings.Contains(said, "不是发起它的人") {
		t.Errorf("the refusal does not say who may work the card:\n%s", said)
	}
}

// TestASingleChatWithNobodyInChargeGetsNothing covers the gate a single chat needs: there
// is no administrator list there, so the card is only for somebody who administers a group
// somewhere.
func TestASingleChatWithNobodyInChargeGetsNothing(t *testing.T) {
	p := newPlatform(t)
	p.start(t, somebodyElse)

	if said := p.lastText(); !strings.Contains(said, "不在任何群的管理员名单") {
		t.Errorf("a member who administers nothing was told:\n%s", said)
	}
}

// TestTheAuditShowsOnlyYourOwnGroups covers the visibility rule the audit is built
// around: somebody who answers for one group reads that group's record, and not what
// another group was told.
func TestTheAuditShowsOnlyYourOwnGroups(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)
	p.choose(t, true, true)
	p.press(t, "群-OTHER", theAdmin)
	p.press(t, "确认", theAdmin)
	p.press(t, "继续", theAdmin)
	p.say(t, theAdmin, "发到两个群")
	p.press(t, "发送", theAdmin)

	// Somebody who only answers for one group asks in private.
	other := &qqbotsdk.C2CMessageCreateData{
		ID: "AUDIT-CHAT", Content: "/广播审计",
		Author: &qqbotsdk.User{UserOpenID: theAdmin},
	}
	p.handler.SetAdminDirectory(admins{who: theAdmin, groups: []string{otherGroup}})
	if err := p.handler.auditPrivately(context.Background(), other, command.Parsed{}); err != nil {
		t.Fatalf("the audit: %v", err)
	}

	said := p.lastText()
	if !strings.Contains(said, "你管理的 1 个群") {
		t.Errorf("the audit does not say what it covers:\n%s", said)
	}
	if !strings.Contains(said, otherGroup) {
		t.Errorf("the audit does not name the group it is about:\n%s", said)
	}
	if strings.Contains(said, hereGroup) {
		t.Errorf("the audit shows a group this member does not administer:\n%s", said)
	}
}

// TestASingleChatAuditWithNothingToShowSaysSo covers the member who administers nothing.
func TestASingleChatAuditWithNothingToShowSaysSo(t *testing.T) {
	p := newPlatform(t, func(p *platform) { p.admins = admins{} })
	data := &qqbotsdk.C2CMessageCreateData{
		ID: "AUDIT-CHAT", Content: "/广播审计",
		Author: &qqbotsdk.User{UserOpenID: theAdmin},
	}
	if err := p.handler.auditPrivately(context.Background(), data, command.Parsed{}); err != nil {
		t.Fatalf("the audit: %v", err)
	}
	if said := p.lastText(); !strings.Contains(said, "不在任何群的管理员名单") {
		t.Errorf("a member who administers nothing was told:\n%s", said)
	}
}

// TestTheTrialIsOffByDefault covers the switch itself: a deployment that says nothing
// about the trial has no gate.
func TestTheTrialIsOffByDefault(t *testing.T) {
	p := newPlatform(t)
	if !p.handler.cfg.allowed("anybody") {
		t.Error("the trial gate is on in a deployment that did not ask for it")
	}
	if !p.handler.cfg.allowed(theAdmin) {
		t.Error("a member was refused with the trial gate off")
	}
}

// guard: the interface this feature is wired through is the one the app uses.
var _ feature.AdminDirectory = admins{}
