package joinverify

import (
	"strings"
	"testing"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// messageFromTheHeldMember is one group message from the member who is being held.
func messageFromTheHeldMember(id string) string {
	return `{
		"id": "` + id + `",
		"author": {"member_openid": "` + testMemberOpenID + `"},
		"content": "大家好",
		"group_openid": "` + testGroupOpenID + `",
		"timestamp": "2026-10-02T10:00:00+08:00"
	}`
}

// TestAPseudoHoldTurnsIntoARealMuteAfterSoManyMessages covers what the count is for.
//
// The pseudo-mute is the bot taking the member's messages back, so nothing stops them
// talking; one who goes on talking is one the group has to silence for real. The mute is
// applied by the platform, and the member is sent the same card as the prompt with the
// words that send them to the phone -- because a muted member's desktop client refuses to
// run the verification at all.
func TestAPseudoHoldTurnsIntoARealMuteAfterSoManyMessages(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_mode: pseudo
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
pseudo_mute_after: 3
`)
	h.join()

	for _, id := range []string{"MSG-1", "MSG-2"} {
		h.dispatch(qqbotsdk.EventGroupMessageCreate, messageFromTheHeldMember(id))
	}
	if got := len(h.callsOf("/restrict_chat_setting")); got != 0 {
		t.Fatalf("muted after fewer messages than the count asks for: %d mutes", got)
	}

	h.dispatch(qqbotsdk.EventGroupMessageCreate, messageFromTheHeldMember("MSG-3"))

	if got := len(h.callsOf("/restrict_chat_setting")); got != 1 {
		t.Errorf("mutes applied = %d, want 1: the hold is supposed to become a real mute",
			got)
	}
	if !strings.Contains(h.promptText(), "手机端") {
		t.Errorf("the member was not told to verify from the phone:\n%s", h.promptText())
	}
	if h.verifier.IsPseudoMuted(testGroupOpenID, testMemberOpenID) {
		t.Error("the hold is still the soft one after it was turned into a real mute")
	}
}

// TestTheCardSentBackToAHeldMemberAnswersTheirMessage covers how a reply is addressed.
//
// The platform takes msg_id for "this is my answer to that message" and event_id for "this
// is my answer to that interaction"; they are mutually exclusive, and a *message* id in
// event_id is refused outright (40034025 请求参数event_id无效). The member's message has just
// been taken back, so the card has to be addressed to it -- get this wrong and nothing
// arrives at all: all the member sees is their own words disappearing, which is exactly the
// silently-broken-bot reading the reply exists to prevent.
func TestTheCardSentBackToAHeldMemberAnswersTheirMessage(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_mode: pseudo
`)
	h.join()
	h.dispatch(qqbotsdk.EventGroupMessageCreate, messageFromTheHeldMember("MSG-REPLY-1"))

	var sent []call
	for _, one := range h.callsOf("/messages") {
		if one.method == "POST" && strings.HasSuffix(one.path, "/messages") {
			sent = append(sent, one)
		}
	}
	// One proactive prompt when the member joined, and one card sent back to the message
	// that was just taken back.
	if len(sent) != 2 {
		t.Fatalf("cards sent = %d, want the join prompt and the one answering the message",
			len(sent))
	}
	card := sent[len(sent)-1]
	if got := card.body["msg_id"]; got != "MSG-REPLY-1" {
		t.Errorf("the card's msg_id = %v, want the id of the message it answers", got)
	}
	if got, present := card.body["event_id"]; present {
		t.Errorf("the card carries event_id = %v; a message is answered with msg_id", got)
	}
}

// TestAPseudoHoldIsNotEscalatedWithoutACount covers the default: a group that says nothing
// keeps the soft hold, however much the member talks.
func TestAPseudoHoldIsNotEscalatedWithoutACount(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_mode: pseudo
`)
	h.join()

	for index := 0; index < 5; index++ {
		h.dispatch(qqbotsdk.EventGroupMessageCreate,
			messageFromTheHeldMember("MSG-"+string(rune('a'+index))))
	}

	if got := len(h.callsOf("/restrict_chat_setting")); got != 0 {
		t.Errorf("mutes applied = %d, want 0: nobody asked for an escalation", got)
	}
}

// TestTheCountIsWhatDecidesTheEscalation pins the counting itself, away from the event
// plumbing: how many messages it takes, that zero means off, and that a member is told why
// at most once every half minute.
func TestTheCountIsWhatDecidesTheEscalation(t *testing.T) {
	now := time.Now()
	var counted pseudoMessages

	for sent := 1; sent < 3; sent++ {
		if _, escalate := counted.counted("TOKEN", 3, now); escalate {
			t.Fatalf("escalated after %d messages, want 3", sent)
		}
	}
	if _, escalate := counted.counted("TOKEN", 3, now); !escalate {
		t.Error("the third message did not escalate the hold")
	}

	// Zero is off, however many messages arrive.
	var off pseudoMessages
	for sent := 0; sent < 10; sent++ {
		if _, escalate := off.counted("TOKEN", 0, now); escalate {
			t.Fatal("a hold was escalated although the group set no count")
		}
	}

	// Told once, and not again inside the interval: ten messages in a row are one event,
	// and the group is watching.
	var told pseudoMessages
	if first, _ := told.counted("TOKEN", 0, now); !first {
		t.Error("the first message did not tell the member why it was taken back")
	}
	if again, _ := told.counted("TOKEN", 0, now.Add(time.Second)); again {
		t.Error("the member was told again inside the same half minute")
	}
	if later, _ := told.counted("TOKEN", 0, now.Add(toldEvery)); !later {
		t.Error("the member was not told after the interval had passed")
	}
}
