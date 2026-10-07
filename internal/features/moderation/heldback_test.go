package moderation

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// A member who is being held back without a mute has their messages taken back as
// they arrive, so a message of theirs must not be written down here. The cache is
// what a report is judged against, and a message that was taken back before the
// group could read it would otherwise stay quotable for as long as the cache
// remembers -- which is how a report comes to be filed about something nobody saw.

const (
	heldGroup  = "GROUP-OPENID"
	heldMember = "MEMBER-OPENID"
)

// stubVerification is the join verification as this feature asks it one question.
type stubVerification struct {
	// held are the members being held without a mute, keyed as the feature keys
	// them.
	held map[string]bool
}

func (s stubVerification) IsPseudoMuted(groupOpenID, memberOpenID string) bool {
	return s.held[groupOpenID+"/"+memberOpenID]
}

// The rest of the contract is not this feature's to use, and answering with the
// zero value keeps that visible: a moderation that starts asking whether somebody
// is merely pending would be asking a different question, and one that drives the
// verification at all would be doing somebody else's job.
func (s stubVerification) Reverify(context.Context, string, string) error         { return nil }
func (s stubVerification) SimulateDeadline(context.Context, string, string) error { return nil }
func (s stubVerification) IsPending(string, string) bool                          { return false }
func (s stubVerification) Resend(context.Context, string, string) error           { return nil }

// TestAMessageFromAMemberHeldWithoutAMuteIsNotWrittenDown covers the question
// itself, and the two ways of answering no: a member who is not being held, and a
// deployment where no verification runs at all.
func TestAMessageFromAMemberHeldWithoutAMuteIsNotWrittenDown(t *testing.T) {
	held := &handler{verifier: stubVerification{held: map[string]bool{
		heldGroup + "/" + heldMember: true,
	}}}

	if !held.heldBack(heldGroup, heldMember) {
		t.Error("a message from a member held without a mute would be written down")
	}
	if held.heldBack(heldGroup, "SOMEONE-ELSE") {
		t.Error("a message from somebody who is not being held would be left out")
	}
	if held.heldBack("ANOTHER-GROUP", heldMember) {
		t.Error("a hold in one group was read as a hold in another")
	}

	// No verification, nobody held, nothing left out: a deployment that runs the
	// cache without the verification is the ordinary one.
	without := &handler{}
	if without.heldBack(heldGroup, heldMember) {
		t.Error("a message was left out with no verification running at all")
	}
}

// TestAMessageThatIsTakenBackNeverReachesTheCache covers the order the two have to
// be in: the decision has to be made before the cache is touched, or the message
// is written down and the decision changes nothing.
//
// There is no cache at all here on purpose. Reaching it panics, which is what makes
// this an assertion about the write rather than about what a write would have
// returned -- a cache that is merely unreachable would accept the call and answer
// with an error that the handler swallows.
func TestAMessageThatIsTakenBackNeverReachesTheCache(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("the message reached the message cache: %v", recovered)
		}
	}()

	h := &handler{
		deps:     feature.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		verifier: stubVerification{held: map[string]bool{heldGroup + "/" + heldMember: true}},
	}
	if err := h.onMessage(context.Background(), groupMessage(heldGroup, heldMember)); err != nil {
		t.Fatalf("onMessage: %v", err)
	}
}

// groupMessage builds the event the platform delivers for one group message.
func groupMessage(groupOpenID, memberOpenID string) *qqbotsdk.Event {
	body, _ := json.Marshal(map[string]any{
		"id":           "MSG-1",
		"group_openid": groupOpenID,
		"content":      "大家好",
		"timestamp":    "2026-10-02T10:00:00+08:00",
		"author":       map[string]any{"member_openid": memberOpenID, "username": "somebody"},
	})
	return qqbotsdk.NewEvent(&qqbotsdk.Payload{
		ID:   "EVENT-ID",
		Op:   qqbotsdk.OpDispatch,
		Type: qqbotsdk.EventGroupMessageCreate,
		Data: body,
	}, "test")
}
