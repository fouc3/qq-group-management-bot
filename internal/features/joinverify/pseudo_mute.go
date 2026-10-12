package joinverify

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// pseudoMessages counts what one held member has sent under the pseudo-mute, and when they
// were last told why their message went away.
//
// Keyed on the hold's token rather than on the member, which is what makes a new hold a
// fresh count: the token is new every time somebody is asked to verify, so a member who
// leaves and comes back is not escalated on their first message over a count from before.
//
// Counted in memory rather than written down, deliberately. A restart gives the member
// their messages again, which is the softer of the two mistakes; what is written down is
// the *result* -- the entry that records the hold became a real mute -- because that one
// has to outlive the process. Forgetting it would leave the member held the soft way again,
// on the one path where the bot is what stops them talking.
type pseudoMessages struct {
	mu   sync.Mutex
	sent map[string]int
	told map[string]time.Time
}

// toldEvery is how often one member is told why their message was taken back.
//
// Ten messages in a row are one event rather than ten, and the group is watching: the
// point of the reply is to be read, not to be a wall of text.
const toldEvery = 30 * time.Second

// counted records one message from a held member and answers two questions: whether they
// may be told why now, and whether they have gone on talking long enough to be silenced
// for real.
//
// after is the group's pseudo_mute_after. Zero -- what a group that says nothing gets --
// turns the escalation off and leaves the telling, which is the point of the reply.
func (p *pseudoMessages) counted(token string, after int, now time.Time) (told, escalate bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sent == nil {
		p.sent, p.told = map[string]int{}, map[string]time.Time{}
	}
	p.sent[token]++
	if now.Sub(p.told[token]) >= toldEvery {
		p.told[token] = now
		told = true
	}
	return told, after > 0 && p.sent[token] >= after
}

// forget drops what is remembered about one hold, which is what releasing it does.
func (p *pseudoMessages) forget(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sent, token)
	delete(p.told, token)
}

// answerPseudoHold is what a member is owed when their message is taken back: why it
// happened, and -- once they have gone on talking as often as the group allows -- the
// notice that sends them to the phone, because from then on a real mute is in force and a
// muted member's desktop client will not run the verification at all.
func (v *verifier) answerPseudoHold(ctx context.Context, eventID string, entry *pending) {
	log := v.deps.Logger.With("group", entry.groupOpenID, "member", entry.memberOpenID)
	told, escalate := v.pseudo.counted(entry.token, entry.settings.PseudoMuteAfter,
		time.Now())
	if escalate {
		v.escalate(ctx, entry, log)
		return
	}
	if !told {
		return
	}
	// The same card as the prompt, sent as a reply to the message that was just taken
	// back: the member already has this button, and being told to use it is the whole of
	// what can be done for somebody the hold is not stopping.
	if err := v.ask(ctx, entry.groupOpenID, entry.memberOpenID, entry.token,
		entry.settings, entry.settings.Prompt, eventID); err != nil {
		log.Warn("could not tell a member under the pseudo-mute why their message "+
			"was taken back", "error", err)
	}
}

// escalate turns a pseudo-mute into a real one: the member has been told to verify as often
// as the group allows and has gone on talking.
//
// The order is load-bearing. The entry's mode is switched to the real one *before* the mute
// is applied, because mute() does nothing at all while the settings say pseudo -- which is
// the whole of that mode. And it stays switched afterwards, because unmute() reads the same
// field: a member who verifies tomorrow has to be released from the mute applied today, and
// under a pseudo-mode entry that release is a no-op.
func (v *verifier) escalate(ctx context.Context, entry *pending, log *slog.Logger) {
	now := time.Now()
	settings := entry.settings
	settings.MuteMode = MuteModeMute
	heldUntil := now.Add(time.Duration(settings.MuteMinutes) * time.Minute)

	v.mu.Lock()
	entry.settings = settings
	entry.heldUntil = heldUntil
	entry.escalatedAt = now.Unix()
	record := store.Pending{
		Token:        entry.token,
		GroupOpenID:  entry.groupOpenID,
		MemberOpenID: entry.memberOpenID,
		JoinedAt:     entry.joinedAt,
		Deadline:     entry.deadline.Unix(),
		HeldUntil:    heldUntil.Unix(),
		Reported:     entry.reported,
		EscalatedAt:  now.Unix(),
	}
	v.mu.Unlock()

	// Written down before the mute, and reported rather than fatal if it cannot be: the
	// mute is what the member feels, and refusing to apply it because the record failed
	// would leave the group with somebody it has decided to silence and nothing silencing
	// them. What is lost is the memory of why.
	if v.pending != nil {
		encoded, err := encodeSettings(settings)
		if err != nil {
			log.Error("could not record the settings of an escalated hold", "error", err)
		}
		record.Settings = encoded
		if err := v.pending.Put(ctx, record); err != nil {
			log.Error("could not write down that a hold was turned into a real mute",
				"error", err)
		}
	}

	if err := v.mute(ctx, entry.groupOpenID, entry.memberOpenID, heldUntil,
		settings); err != nil {
		log.Error("could not turn a hold into a real mute", "error", err)
		return
	}
	// The card the member already has keeps working, and the notice goes through the same
	// sender as the prompt with different words: one implementation, two texts.
	if err := v.ask(ctx, entry.groupOpenID, entry.memberOpenID, entry.token, settings,
		settings.PseudoMuteNotice, ""); err != nil {
		log.Warn("could not tell a member that their hold became a real mute",
			"error", err)
	}
	log.Warn("a member went on talking under the pseudo-mute, so the hold became a real "+
		"mute", "until", heldUntil.Format(time.RFC3339))
}
