package joinverify

import (
	"context"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// The per-group switch is the one thing here that is a decision rather than a
// rule: the fields around it say how somebody is held, `enabled` says whether
// they are at all. These tests cover what it turns off, what it must not
// disturb, and the two places it is deliberately not asked about.

// TestAGroupCanSwitchVerificationOff covers the switch itself: the section's own
// enabled turns the feature on for the deployment, and a group's own says
// whether that group takes part in it.
func TestAGroupCanSwitchVerificationOff(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
groups:
  GROUP-OPENID:
    enabled: false
`)

	settings := h.verifier.cfg.settingsFor(testGroupOpenID)
	if settings.Enabled {
		t.Fatal("the group's own enabled: false was not applied")
	}
	// The switch says whether people are held, and nothing about how: what the
	// group did not name is still inherited.
	if settings.OnDeadline != OnDeadlineNotify {
		t.Errorf("OnDeadline = %q, want the inherited %q",
			settings.OnDeadline, OnDeadlineNotify)
	}
	if len(settings.NotifyMembers) != 1 || settings.NotifyMembers[0] != "ADMIN-OPENID" {
		t.Errorf("NotifyMembers = %v, want the inherited list", settings.NotifyMembers)
	}

	// And it is acted on rather than merely resolved: a member joining this
	// group is left alone -- not muted, not asked, not written down.
	h.join()
	if h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("a member was held in a group whose verification is switched off")
	}
	if calls := h.muteCalls(); calls != 0 {
		t.Errorf("mute calls = %d, want none: nobody is held here", calls)
	}
	if calls := len(h.callsOf("/messages")); calls != 0 {
		t.Errorf("messages sent = %d, want none: nobody is asked to verify here", calls)
	}
}

// TestAnExplicitlyEnabledGroupStillHolds is the other side of the same switch,
// and the one an operator writes when they want a group named in the file: the
// switch being written out as true is not a way to turn the group off.
func TestAnExplicitlyEnabledGroupStillHolds(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
groups:
  GROUP-OPENID:
    enabled: true
`)
	if !h.verifier.cfg.settingsFor(testGroupOpenID).Enabled {
		t.Fatal("an explicit enabled: true was read as off")
	}
	h.join()
	if !h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("a member of a group that asks to verify was not held")
	}
}

// TestTheSwitchSpeaksOnlyForTheGroupThatWroteIt covers the boundary: a group
// entry is one group's, so switching one off must not reach a group the file
// does not name -- that one goes on inheriting the section's own rules.
func TestTheSwitchSpeaksOnlyForTheGroupThatWroteIt(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
groups:
  GROUP-OPENID:
    enabled: false
`)
	if !h.verifier.cfg.settingsFor("ANOTHER-GROUP").Enabled {
		t.Error("a group the file does not name was switched off with another one")
	}
}

// TestAnAdministratorCanStillReverifyInASwitchedOffGroup pins the deliberate
// gap in the switch: it decides what a group does by itself when somebody
// joins, and /重新验证 is an administrator naming one member and asking anyway.
//
// Refusing it would take away the only way to verify somebody in a group that
// does not verify on its own, which is exactly what running the command there
// asks for.
func TestAnAdministratorCanStillReverifyInASwitchedOffGroup(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
groups:
  GROUP-OPENID:
    enabled: false
`)
	if err := h.verifier.Reverify(context.Background(), testGroupOpenID,
		testMemberOpenID); err != nil {
		t.Fatalf("Reverify: %v", err)
	}
	if !h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("the administrator asked for a verification and nobody was held")
	}
}

// TestASwitchedOffGroupStillAsksForTheMessagesItMayBeHolding covers the one
// place the switch is deliberately not asked about, which is the intents.
//
// A hold already written down keeps the settings it was taken under, so a group
// that stopped verifying may still be holding somebody under a take-back --
// taking the message events away would leave that member talking freely while
// the bot believes they are held. The cost of the other mistake is a
// subscription nobody uses.
func TestASwitchedOffGroupStillAsksForTheMessagesItMayBeHolding(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
groups:
  GROUP-OPENID:
    enabled: false
    mute_mode: pseudo
    deadline_hours: 48
`)
	if !h.verifier.Intents().Has(qqbotsdk.IntentGroupAndC2CEvent) {
		t.Error("the message events were dropped although a group switched off may " +
			"still be holding somebody whose messages have to be taken back")
	}
}

// TestAGroupSwitchedOffHoldsNobodyEvenUnderThePseudoMute covers the same switch
// in the mode that never asks the platform for anything: there the hold is the
// recall itself, so a bug here shows up as messages coming back rather than as
// a mute call.
func TestAGroupSwitchedOffHoldsNobodyEvenUnderThePseudoMute(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_mode: pseudo
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
groups:
  GROUP-OPENID:
    enabled: false
`)
	h.join()
	if h.verifier.IsPseudoMuted(testGroupOpenID, testMemberOpenID) {
		t.Error("a member of a switched-off group was held by the take-back")
	}
	if h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("a member of a switched-off group was written down as waiting")
	}
}
