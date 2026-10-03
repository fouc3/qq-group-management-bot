package broadcast

import (
	"strings"
	"testing"

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

// TestTheTrialGateAlsoClosesASingleChat covers the gate where a member administers a
// group: a single chat has no administrator list, so the list the trial names is what
// decides there.
func TestTheTrialGateAlsoClosesASingleChat(t *testing.T) {
	p := newPlatform(t, withTrial("SOMEBODY-ELSE"))
	p.startPrivately(t, theAdmin)

	if said := p.lastText(); !strings.Contains(said, closed) {
		t.Errorf("a member the trial does not name was told:\n%s", said)
	}
}

// TestTheCommandIsOfferedInBothPanels covers the panels this command asks for.
//
// A single chat is where somebody who administers several groups writes one broadcast
// and picks where it goes, so it is offered there as well as in a group -- and the card
// itself is what narrows the choice to the groups that person administers.
func TestTheCommandIsOfferedInBothPanels(t *testing.T) {
	p := newPlatform(t)
	defs := p.handler.CommandDefs()
	if len(defs) != 1 {
		t.Fatalf("the feature offers %d commands, want 1", len(defs))
	}
	def := defs[0]
	if def.Private == nil {
		t.Error("a single chat cannot write a broadcast, so the private panel would " +
			"offer a command that answers nothing")
	}
	for _, scene := range []command.Scene{command.InGroup, command.InPrivate} {
		offered := false
		for _, place := range def.Panels {
			if place.Scene == scene {
				offered = true
			}
		}
		if !offered {
			t.Errorf("the command is not offered in scene %v", scene)
		}
	}
}

// TestTheCardOffersOnlyTheGroupsYouAdminister covers what a card may choose from.
//
// Being on one group's administrator list is not being on another's, so a card that
// offered every group the bot manages would be offering something the send would
// refuse.
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

// TestABroadcastCanBeWrittenInASingleChat covers the whole private flow: the card, the
// choices, the text, and the group it goes to.
func TestABroadcastCanBeWrittenInASingleChat(t *testing.T) {
	p := newPlatform(t)
	p.startPrivately(t, theAdmin)

	if card := p.lastText(); !strings.Contains(card, "**群广播**") {
		t.Fatalf("no card in the single chat:\n%s", card)
	}
	p.pressPrivately(t, "MD：未选", theAdmin)
	p.pressPrivately(t, "匿名：未选", theAdmin)
	p.pressPrivately(t, "群-OTHER", theAdmin)
	p.pressPrivately(t, "确认", theAdmin)
	p.pressPrivately(t, "继续", theAdmin)
	p.sayPrivately(t, theAdmin, "从私聊发的广播")

	before := len(p.sent)
	p.pressPrivately(t, "发送", theAdmin)

	var posted []string
	for _, message := range p.sent[before:] {
		markdown, _ := message["markdown"].(map[string]any)
		if text, _ := markdown["content"].(string); strings.Contains(text, divider) {
			posted = append(posted, text)
		}
	}
	if len(posted) != 1 {
		t.Fatalf("the broadcast was posted %d time(s), want once: %v", len(posted), posted)
	}
	if !strings.Contains(posted[0], "从私聊发的广播") {
		t.Errorf("the group reads:\n%s", posted[0])
	}
}

// TestSomebodyElseCannotWorkAPrivateCard covers who a card belongs to when there is no
// group to be an administrator of: it is the member who opened it, in that chat.
func TestSomebodyElseCannotWorkAPrivateCard(t *testing.T) {
	p := newPlatform(t)
	p.startPrivately(t, theAdmin)

	before := len(p.sent)
	p.pressPrivately(t, "MD：未选", somebodyElse)

	if len(p.sent) != before+1 {
		t.Errorf("a press from somebody else sent %d message(s), want only the refusal",
			len(p.sent)-before)
	}
	if said := p.lastText(); !strings.Contains(said, "管理员") {
		t.Errorf("the refusal does not say who may work the card:\n%s", said)
	}
}

// TestASingleChatWithNobodyInChargeGetsNothing covers the gate a single chat needs:
// there is no administrator list there, so the card is only for somebody who
// administers a group somewhere.
func TestASingleChatWithNobodyInChargeGetsNothing(t *testing.T) {
	p := newPlatform(t)
	p.startPrivately(t, somebodyElse)

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
