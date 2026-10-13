package joinverify

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The console drives these three through feature.VerifierActions. They are the
// feature's own steps -- the same unmute and the same removal the button press
// and the missed deadline use -- so what is tested here is the part that is new:
// what each one does to the record, and what it says when there is nobody left
// to act on.

// TestTheConsoleActionsSayWhenNobodyIsWaiting covers the ordinary race: the page
// was rendered a moment ago, and the member has since verified or been removed.
//
// The answer has to be its own type rather than a plain error, because "nobody is
// waiting" is an answer -- the console turns it into a 404 with wording for a
// person -- while a failure is a 500.
func TestTheConsoleActionsSayWhenNobodyIsWaiting(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	ctx := context.Background()

	for _, test := range []struct {
		name string
		call func() error
	}{
		{"resend", func() error { return h.verifier.Resend(ctx, testGroupOpenID, testMemberOpenID) }},
		{"approve", func() error { return h.verifier.Approve(ctx, testGroupOpenID, testMemberOpenID) }},
		{"reject", func() error { return h.verifier.Reject(ctx, testGroupOpenID, testMemberOpenID) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if err == nil {
				t.Fatal("actioning somebody who is not waiting succeeded")
			}
			var notHeld *NotHeld
			if !errors.As(err, &notHeld) {
				t.Fatalf("the error is %T, want a *NotHeld: %v", err, err)
			}
			if notHeld.Error() == "" {
				t.Error("the refusal has no sentence to show a person")
			}
		})
	}
}

// TestApprovingLetsThemThroughWithoutAButton covers what the console's approve
// is for: the member never presses anything, so the hold has to be lifted and the
// record closed by the operator's decision alone -- and the group has to be told,
// because that is where the consequence shows up.
func TestApprovingLetsThemThroughWithoutAButton(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
mute_mode: pseudo
`)
	h.join()
	if h.tokenFromPrompt() == "" {
		t.Fatal("no hold was created")
	}
	if !h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Fatal("the member is not waiting after joining")
	}

	if err := h.verifier.Approve(context.Background(), testGroupOpenID, testMemberOpenID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("the member is still waiting after being let through")
	}
	if h.verifier.IsPseudoMuted(testGroupOpenID, testMemberOpenID) {
		t.Error("the member is still being taken back after being let through")
	}

	// The same sentence the skip button in the group posts: this is the same
	// decision, taken from a page instead of from the group. A release that said
	// nothing would leave the group watching somebody who was being held start
	// talking again, with nothing to explain it.
	notice := h.promptText()
	if !strings.Contains(notice, "管理员已跳过") {
		t.Errorf("notice = %q, want the group told that an administrator let them through", notice)
	}
	if !strings.Contains(notice, `<qqbot-at-user id="`+testMemberOpenID+`" />`) {
		t.Errorf("notice = %q, want the released member mentioned", notice)
	}
}

// TestRejectingTakesThemOutOfTheGroup covers the removal, and that it is the
// removal the deadline path performs rather than a second one written here.
func TestRejectingTakesThemOutOfTheGroup(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	if h.tokenFromPrompt() == "" {
		t.Fatal("no hold was created")
	}
	h.kicks = nil
	// Every platform call the harness sees is recorded, so this is how "it did
	// not touch the hold" is checked: an unmute would appear here.
	callsBefore := len(h.calls)

	if err := h.verifier.Reject(context.Background(), testGroupOpenID, testMemberOpenID); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("the record is still in the queue after being closed")
	}
	if len(h.kicks) != 0 {
		t.Errorf("OneBot was asked to remove %d members; the bot cannot kick, so it must not try", len(h.kicks))
	}
	if len(h.calls) != callsBefore {
		t.Errorf("the action made %d platform calls; rejecting closes the record and must not "+
			"lift the hold", len(h.calls)-callsBefore)
	}
}
