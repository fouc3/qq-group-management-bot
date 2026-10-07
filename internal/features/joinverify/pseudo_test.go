package joinverify

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// The pseudo-mute is the other way of holding a member who has not verified: the
// platform is not asked to silence anybody, and what they say is taken back as it
// arrives instead. These tests cover what that changes -- the mute that is never
// applied, the hold that never runs out, and the hold that ends with a removal --
// and, just as much, what it must not change: the mute mode's own behaviour.

// muteCalls counts how many times the platform was asked to change somebody's
// mute state, which is what a pseudo-mute must never do.
func (h *harness) muteCalls() int {
	return len(h.callsOf("/restrict_chat_setting"))
}

// TestAPseudoHoldMutesNobody covers the difference the mode is for: the member is
// held, the prompt is sent, and the platform is never asked to silence anybody.
func TestAPseudoHoldMutesNobody(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_mode: pseudo
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()

	if h.tokenFromPrompt() == "" {
		t.Fatal("no hold was created")
	}
	if got := h.muteCalls(); got != 0 {
		t.Errorf("mute calls = %d, want none: a pseudo-mute applies nothing to the "+
			"member", got)
	}
	if !h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("the member is not being held")
	}
	if !h.verifier.IsPseudoMuted(testGroupOpenID, testMemberOpenID) {
		t.Error("the hold is not reported as one taken back rather than muted")
	}
}

// TestAPseudoHoldTakesBackWhatTheMemberSays covers the hold itself. Under a real
// mute this only closes the gap before the mute lands; here there is no mute, so
// a message that is not taken back is a message the group reads from somebody the
// verification claims to be holding.
func TestAPseudoHoldTakesBackWhatTheMemberSays(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_mode: pseudo
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	h.dispatch(qqbotsdk.EventGroupMessageCreate, `{
		"id": "MSG-PSEUDO-1",
		"author": {"member_openid": "`+testMemberOpenID+`"},
		"content": "大家好",
		"group_openid": "`+testGroupOpenID+`",
		"timestamp": "2026-10-02T10:00:00+08:00"
	}`)

	if got := len(h.callsOf("/messages/MSG-PSEUDO-1")); got != 1 {
		t.Errorf("recalls of the held member's message = %d, want 1", got)
	}
}

// TestAPseudoHoldTakesNothingBackInADryRun covers the two modes' dry runs having
// different work to hold back. A take-back is the whole of what the pseudo-mute
// does to a member, so a dry run that took a message back would be the real thing
// rather than a run of it -- and the message would be missing from the group while
// the record of it says nobody was touched.
func TestAPseudoHoldTakesNothingBackInADryRun(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_mode: pseudo
dry_run: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	h.dispatch(qqbotsdk.EventGroupMessageCreate, `{
		"id": "MSG-PSEUDO-DRY",
		"author": {"member_openid": "`+testMemberOpenID+`"},
		"content": "大家好",
		"group_openid": "`+testGroupOpenID+`",
		"timestamp": "2026-10-02T10:00:00+08:00"
	}`)

	if got := len(h.callsOf("/messages/MSG-PSEUDO-DRY")); got != 0 {
		t.Errorf("recalls = %d, want none in a dry run", got)
	}
	// And the message is not one the rest of the bot has to pretend it never saw:
	// it stayed in the group, so a record of it is what the group saw.
	if h.verifier.IsPseudoMuted(testGroupOpenID, testMemberOpenID) {
		t.Error("a message is hidden from the cache in a dry run, where nothing was " +
			"taken back and the group saw it")
	}
}

// TestAPseudoHoldEndsOnlyWhenTheMemberVerifies covers the choice that a
// pseudo-mute has no expiry of its own.
//
// A real hold runs out with its mute, so a sweep far in the future releases the
// member and forgets them. Here the same sweep must find them still held: nothing
// about the member ever runs out, and the only endings are verifying and being
// taken out of the group.
func TestAPseudoHoldEndsOnlyWhenTheMemberVerifies(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_mode: pseudo
mute_minutes: 1
deadline_hours: 1
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	token := h.tokenFromPrompt()
	if token == "" {
		t.Fatal("no hold was created")
	}

	// Far enough ahead that the mute of a real hold, and the deadline, are both
	// long past.
	due, renew := h.verifier.takeDueDeadlines(time.Now().Add(30 * 24 * time.Hour))
	if len(due) != 1 {
		t.Fatalf("due = %d, want the deadline to have been reached", len(due))
	}
	if len(renew) != 0 {
		t.Errorf("renew = %d, want nothing renewed: there is no mute to apply again",
			len(renew))
	}
	if !h.verifier.IsPseudoMuted(testGroupOpenID, testMemberOpenID) {
		t.Error("the hold ended when mute_minutes passed, so the member can talk " +
			"without having verified")
	}

	// Verifying is one of the two endings, and it asks nothing of the platform:
	// nothing was applied to this member, so there is nothing to lift.
	h.press("INTERACTION-PSEUDO-DONE", token, testMemberOpenID,
		qqbotsdk.InteractionSceneGroup)
	if h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("the member is still being held after verifying")
	}
	if got := h.muteCalls(); got != 0 {
		t.Errorf("mute calls = %d, want none: verifying a pseudo-hold has no mute to "+
			"lift", got)
	}
}

// TestAPseudoRemovalEndsTheHold covers the other ending, which is the only one a
// deadline can act on here: the group's rule is to remove the member, and the hold
// ends with them. A record that outlived the removal would go on taking back
// messages from somebody who is no longer in the group.
func TestAPseudoRemovalEndsTheHold(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_mode: pseudo
on_deadline: remove
remove_backend: onebot
notify_members: ["ADMIN-OPENID"]
`)
	h.join()
	token := h.tokenFromPrompt()
	if token == "" {
		t.Fatal("no hold was created")
	}

	h.verifier.mu.Lock()
	h.verifier.byToken[token].deadline = time.Now().Add(-time.Minute)
	h.verifier.mu.Unlock()

	due, _ := h.verifier.takeDueDeadlines(time.Now())
	if len(due) != 1 {
		t.Fatalf("due = %d, want the removal the group asked for", len(due))
	}
	h.verifier.handleDeadline(due[0])

	if h.kickCount() != 1 {
		t.Fatalf("onebot kicks = %d, want the removal this group asked for",
			h.kickCount())
	}
	if h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("the hold outlived the removal")
	}
	if h.verifier.IsPseudoMuted(testGroupOpenID, testMemberOpenID) {
		t.Error("a member who was removed is still reported as held back")
	}
}

// TestAPseudoHoldSurvivesAFailedRemoval covers the other half of that ending: a
// removal that did not happen leaves the member in the group, and a hold that
// ended anyway would leave them talking without having verified while the group
// believed it had removed them.
func TestAPseudoHoldSurvivesAFailedRemoval(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_mode: pseudo
on_deadline: remove
remove_backend: onebot
notify_members: ["ADMIN-OPENID"]
`)
	// The notice comes back from somebody other than the bot, which is the case
	// the onebot removal refuses, so nobody is removed.
	h.mu.Lock()
	h.forged = true
	h.mu.Unlock()

	h.join()
	token := h.tokenFromPrompt()
	if token == "" {
		t.Fatal("no hold was created")
	}

	h.verifier.mu.Lock()
	h.verifier.byToken[token].deadline = time.Now().Add(-time.Minute)
	h.verifier.mu.Unlock()

	due, _ := h.verifier.takeDueDeadlines(time.Now())
	if len(due) != 1 {
		t.Fatalf("due = %d, want the removal the group asked for", len(due))
	}
	h.verifier.handleDeadline(due[0])

	if h.kickCount() != 0 {
		t.Fatalf("onebot kicks = %d, want nobody removed by a forged notice",
			h.kickCount())
	}
	if !h.verifier.IsPseudoMuted(testGroupOpenID, testMemberOpenID) {
		t.Error("the hold ended although the member is still in the group")
	}
}

// TestAPseudoHoldSurvivesARestart covers the promise the state file keeps in this
// mode. A real mute that is forgotten stays applied with nobody to release it; a
// pseudo-mute that is forgotten simply stops, and the member talks as if they had
// verified -- which is the prompt's whole job, gone.
func TestAPseudoHoldSurvivesARestart(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "bot.db")
	section := `
enabled: true
mute_mode: pseudo
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`
	first := newHarnessWithStore(t, section, databasePath)
	first.join()
	token := first.tokenFromPrompt()
	if token == "" {
		t.Fatal("no hold was created")
	}

	// A second verifier over the same database is what a restart produces.
	second := newHarnessWithStore(t, section, databasePath)
	if !second.verifier.IsPseudoMuted(testGroupOpenID, testMemberOpenID) {
		t.Fatal("the hold was forgotten by the restart, so the member can talk " +
			"without having verified")
	}
	second.dispatch(qqbotsdk.EventGroupMessageCreate, `{
		"id": "MSG-AFTER-RESTART",
		"author": {"member_openid": "`+testMemberOpenID+`"},
		"content": "大家好",
		"group_openid": "`+testGroupOpenID+`",
		"timestamp": "2026-10-02T10:00:00+08:00"
	}`)
	if got := len(second.callsOf("/messages/MSG-AFTER-RESTART")); got != 1 {
		t.Errorf("recalls of the restored hold = %d, want the message taken back", got)
	}

	// And the button the member already has still answers.
	second.press("INTERACTION-AFTER-RESTART", token, testMemberOpenID,
		qqbotsdk.InteractionSceneGroup)
	answers := second.callsOf("/interactions/")
	if len(answers) != 1 {
		t.Fatalf("interaction answers = %d, want 1", len(answers))
	}
	if answers[0].body["code"] != float64(qqbotsdk.InteractionCodeSuccess) {
		t.Errorf("code = %v, want the restored hold to answer its button",
			answers[0].body["code"])
	}
}

// TestAHoldWithNoExpiryIsStoredAsNoExpiry covers what the data layer is handed:
// the zero moment is how "this hold does not run out" is written down, and it is
// stored as a zero rather than as the zero time's own year, which every filter
// asking "is this still in the future" would read as a hold that ended long ago.
func TestAHoldWithNoExpiryIsStoredAsNoExpiry(t *testing.T) {
	if got := heldUntilStamp(time.Time{}); got != 0 {
		t.Errorf("a hold with no expiry was stored as %d, want 0", got)
	}
	if got := heldUntilMoment(0); !got.IsZero() {
		t.Errorf("a hold with no expiry was read back as %s, want the zero moment", got)
	}
	moment := time.Now().Add(time.Hour).Truncate(time.Second)
	if got := heldUntilMoment(heldUntilStamp(moment)); !got.Equal(moment) {
		t.Errorf("a real expiry came back as %s, want %s", got, moment)
	}
}

// TestAnOlderRecordWithNoExpiryIsNotReadAsReleased covers the same rule on the
// other side of the changeover: the JSON file the earlier build wrote is read with
// the same reading of zero, so a record nobody ever released is not dropped as
// though its hold had ended.
func TestAnOlderRecordWithNoExpiryIsNotReadAsReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	payload, err := json.Marshal(storedState{
		Version: stateFileVersion,
		Pending: []storedEntry{{
			Token: "no-expiry", GroupOpenID: testGroupOpenID,
			MemberOpenID: testMemberOpenID,
			Deadline:     time.Now().Add(time.Hour),
		}},
	})
	if err != nil {
		t.Fatalf("encoding the state: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("writing the state: %v", err)
	}

	restored, err := readState(path, time.Now().Add(10*365*24*time.Hour))
	if err != nil {
		t.Fatalf("reading the state: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("restored %d entries, want the one that nobody ever released",
			len(restored))
	}
}

// TestAMuteModeHoldIsNotAHoldWithoutAMute covers the modes being separate
// questions: a member held by a real mute is waiting to verify and is not one
// whose messages are being taken back, so nothing about the message cache changes
// for them.
func TestAMuteModeHoldIsNotAHoldWithoutAMute(t *testing.T) {
	h := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	h.join()

	if !h.verifier.IsPending(testGroupOpenID, testMemberOpenID) {
		t.Error("the member is not being held")
	}
	if h.verifier.IsPseudoMuted(testGroupOpenID, testMemberOpenID) {
		t.Error("a hold applied as a real mute is reported as one taken back, which " +
			"would hide their messages from the cache for no reason")
	}
	if got := h.muteCalls(); got != 1 {
		t.Errorf("mute calls = %d, want the real mute this mode applies", got)
	}
}

// TestThePseudoModeWantsTheMessagesItself covers the intent a take-back needs. The
// messages were only ever subscribed to by the features that cache and judge them,
// which a deployment may not run at all -- and then a pseudo-mute would receive
// nothing to take back while looking like it was working.
func TestThePseudoModeWantsTheMessagesItself(t *testing.T) {
	pseudo := newHarness(t, `
enabled: true
mute_mode: pseudo
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	if !pseudo.verifier.Intents().Has(qqbotsdk.IntentGroupAndC2CEvent) {
		t.Error("a group held without a mute does not subscribe to the messages it " +
			"has to take back")
	}

	// And the mode that does not need them does not ask for them: an intent is a
	// subscription the platform has to grant, not a wish list.
	muted := newHarness(t, `
enabled: true
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	if muted.verifier.Intents().Has(qqbotsdk.IntentGroupAndC2CEvent) {
		t.Error("the mute mode asked for the message events although nothing it does " +
			"needs them")
	}
}

// TestThePseudoModeCanBeChosenPerGroup covers the override, and with it the rule
// that is deliberately not asked of this mode: a deadline has to leave room inside
// the mute, because the mute is what runs out and frees the member. A pseudo-mute
// runs out for nobody, so a group may report somebody after a delay its
// mute_minutes would never have allowed.
func TestThePseudoModeCanBeChosenPerGroup(t *testing.T) {
	h := newHarness(t, `
enabled: true
mute_minutes: 120
deadline_hours: 1
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
groups:
  GROUP-OPENID:
    mute_mode: pseudo
    deadline_hours: 48
`)

	pseudo := h.verifier.cfg.settingsFor(testGroupOpenID)
	if pseudo.MuteMode != MuteModePseudo {
		t.Errorf("MuteMode = %q, want the group's own %q", pseudo.MuteMode, MuteModePseudo)
	}
	// The deadline the mute mode would have refused: two days out, with a hold
	// that lasts two hours.
	if pseudo.DeadlineHours != 48 {
		t.Errorf("DeadlineHours = %d, want the group's own 48", pseudo.DeadlineHours)
	}
	// Everything the group did not name is inherited.
	if pseudo.MuteMinutes != 120 {
		t.Errorf("MuteMinutes = %d, want the inherited 120", pseudo.MuteMinutes)
	}

	inherited := h.verifier.cfg.settingsFor("ANOTHER-GROUP")
	if inherited.MuteMode != MuteModeMute {
		t.Errorf("MuteMode = %q, want the mode a group that names nothing starts "+
			"from", inherited.MuteMode)
	}
	if inherited.MuteMinutes != 120 || inherited.DeadlineHours != 1 {
		t.Errorf("inherited %d minutes and %d hours, want the defaults",
			inherited.MuteMinutes, inherited.DeadlineHours)
	}
}

// TestAnUnknownMuteModeIsRefusedAtStartup covers the typo, which is the one
// mistake this field invites: writing "pseud" or "recall" would otherwise leave
// the operator believing they had switched a group over while it went on muting
// people.
func TestAnUnknownMuteModeIsRefusedAtStartup(t *testing.T) {
	_, err := buildFeature(t, `
enabled: true
mute_mode: pseud
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	if err == nil {
		t.Fatal("an unknown mute_mode was accepted")
	}
	if !strings.Contains(err.Error(), "pseud") {
		t.Errorf("err = %v, want it to name what was written", err)
	}
}

// TestAMuteModeWithoutRoomInsideTheMuteIsRefused keeps the rule the pseudo-mute is
// excused from: with a real mute the deadline has to come before the mute runs
// out, or the reported member has already been free to talk.
func TestAMuteModeWithoutRoomInsideTheMuteIsRefused(t *testing.T) {
	_, err := buildFeature(t, `
enabled: true
mute_minutes: 60
deadline_hours: 48
on_deadline: notify
notify_members: ["ADMIN-OPENID"]
`)
	if err == nil {
		t.Fatal("a deadline beyond the end of the mute was accepted")
	}
}

// buildFeature builds the feature from a section, for the tests about what the
// configuration refuses.
func buildFeature(t *testing.T, section string) (feature.Feature, error) {
	t.Helper()
	return New(sectionNode(t, section),
		feature.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}
