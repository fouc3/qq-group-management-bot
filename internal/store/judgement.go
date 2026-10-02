package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Judgement is one recorded verdict.
//
// It exists because a punishment has to be reviewable afterwards. The log lines
// say what happened once, in order, among everything else; this says what was
// decided about one message, which model decided it, which messages were looked
// at, and what was done about it. Without that, "why was I silenced" has no
// answer beyond whoever remembers.
type Judgement struct {
	ID string
	// GroupOpenID is where it happened.
	GroupOpenID string
	// SubjectOpenID is who was judged, empty when no judgement was reached.
	SubjectOpenID string
	// ReporterOpenID is who asked, empty for anything nobody reported.
	ReporterOpenID string
	// Category is the configured name of what was found, empty for nothing.
	Category string
	// Verdict is what came back overall: ok, violation, or error for a judgement
	// that could not be reached. The last is not a clean verdict, and the column
	// exists so the two are never confused after the fact.
	Verdict string
	// Model names what answered, empty when nothing was asked.
	Model string
	// MessageIDs are the cached messages that were sent for judgement.
	MessageIDs []string
	// Reason is the model's own explanation. It is kept here rather than in the
	// group on purpose: it is free text, and free text from a model is not
	// something to publish.
	Reason string
	// Action is what was done about it, filled in after the fact by whoever did
	// it. Empty means nothing was done, which is itself worth recording.
	Action string
	// MuteSeconds is how long the punishment lasted, zero when there was none.
	MuteSeconds int64
	CreatedAt   int64
}

// The verdicts a record can carry.
const (
	// JudgementOK is a message that was judged and found acceptable.
	JudgementOK = "ok"
	// JudgementViolation is a message that was found to break a rule.
	JudgementViolation = "violation"
	// JudgementError is a judgement that never happened: no cache, no model, an
	// answer that could not be read. It is deliberately not JudgementOK, because
	// "nothing was found" and "nobody looked" are different facts.
	JudgementError = "error"
)

// JudgementStore holds the record of what was judged and what followed.
type JudgementStore interface {
	// Record writes one judgement, once.
	Record(ctx context.Context, entry Judgement) error
	// SetOutcome records what was done about a judgement.
	//
	// Separate from Record because the decision and the act are separate: the
	// judgement is reached by the feature that owns the model, and the act is
	// performed by whoever is allowed to silence somebody.
	SetOutcome(ctx context.Context, id, action string, muteSeconds int64) error
	// Recent returns the judgements about one member since a moment, newest first.
	Recent(ctx context.Context, groupOpenID, subjectOpenID string, since time.Time) ([]Judgement, error)
}

// judgementStore implements JudgementStore.
type judgementStore struct{ store *sqlStore }

// Record implements JudgementStore.
func (j judgementStore) Record(ctx context.Context, entry Judgement) error {
	if strings.TrimSpace(entry.ID) == "" {
		return errors.New("store: a judgement needs an id")
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	// The message ids are a list and the column is one value, so they travel as
	// JSON. It is the same choice the pending holds make for their settings: a
	// column per element would need a table, and this list is only ever read back
	// whole.
	ids, err := json.Marshal(entry.MessageIDs)
	if err != nil {
		return fmt.Errorf("encoding the judged message ids: %w", err)
	}
	_, err = j.store.db.ExecContext(ctx, j.store.query(`
INSERT INTO moderation_judgements
    (id, group_openid, subject_openid, reporter_openid, category, verdict, model,
     message_ids, reason, action, mute_seconds, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		entry.ID, entry.GroupOpenID, entry.SubjectOpenID, entry.ReporterOpenID,
		entry.Category, entry.Verdict, entry.Model, string(ids), entry.Reason,
		entry.Action, entry.MuteSeconds, entry.CreatedAt)
	if err != nil {
		return fmt.Errorf("recording a judgement: %w", err)
	}
	return nil
}

// SetOutcome implements JudgementStore.
func (j judgementStore) SetOutcome(ctx context.Context, id, action string, muteSeconds int64) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("store: recording an outcome needs the judgement's id")
	}
	if _, err := j.store.db.ExecContext(ctx, j.store.query(`
UPDATE moderation_judgements SET action = ?, mute_seconds = ? WHERE id = ?`),
		action, muteSeconds, id); err != nil {
		return fmt.Errorf("recording what followed a judgement: %w", err)
	}
	return nil
}

// Recent implements JudgementStore.
func (j judgementStore) Recent(ctx context.Context, groupOpenID, subjectOpenID string,
	since time.Time) ([]Judgement, error) {
	rows, err := j.store.db.QueryContext(ctx, j.store.query(`
SELECT id, group_openid, subject_openid, reporter_openid, category, verdict, model,
       message_ids, reason, action, mute_seconds, created_at
FROM moderation_judgements
WHERE group_openid = ? AND subject_openid = ? AND created_at >= ?
ORDER BY created_at DESC
LIMIT 50`), groupOpenID, subjectOpenID, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("reading recent judgements: %w", err)
	}
	defer rows.Close()

	var entries []Judgement
	for rows.Next() {
		var entry Judgement
		var ids string
		if err := rows.Scan(&entry.ID, &entry.GroupOpenID, &entry.SubjectOpenID,
			&entry.ReporterOpenID, &entry.Category, &entry.Verdict, &entry.Model,
			&ids, &entry.Reason, &entry.Action, &entry.MuteSeconds,
			&entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("reading a judgement: %w", err)
		}
		if ids != "" {
			// A list that cannot be read back is not worth failing the whole row
			// over: everything else about the judgement is still true.
			_ = json.Unmarshal([]byte(ids), &entry.MessageIDs)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// compile-time proof that the shape satisfies the contract.
var _ JudgementStore = judgementStore{}
