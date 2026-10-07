package store

import (
	"context"
	"database/sql"
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
	// Reasoning is the model's own chain of thought, when the configuration kept
	// one. It is what turns "the judge said this was an advertisement" into
	// something an administrator can argue with, and it is the same free text as
	// Reason: kept for the record, never for the group.
	Reasoning string
	// RecallReason says why the messages were or were not taken back, in the
	// words of whoever did it.
	//
	// Separate from Action because Action is a list of what was done and this is
	// the answer to the only question anybody asks afterwards: the message is
	// still in the group, why?
	RecallReason string
	// Recalls is what happened to each message that was to be taken back,
	// including its text: the message itself is gone by the time anybody asks,
	// and a record that named only an id would be a record of nothing.
	Recalls   []RecallOutcome
	CreatedAt int64
}

// RecallOutcome is one message's fate in a judgement that was acted on.
//
// The text is stored rather than looked up later because the reason to look is
// always that the message is gone.
type RecallOutcome struct {
	// ID is the platform message id, which is what a recall needs.
	ID string `json:"id"`
	// Number is the position the judge was shown it at, when it was the judge
	// that named it. It is for the log rather than for the group.
	Number int `json:"number,omitempty"`
	// Text is what the message said.
	Text string `json:"text,omitempty"`
	// Recalled reports whether it was actually taken back.
	Recalled bool `json:"recalled"`
	// Reason says why not, when it was not.
	Reason string `json:"reason,omitempty"`
}

// Outcome is what was done about a judgement, as the store records it.
//
// One struct rather than a growing argument list: the record already has four
// things to say about an outcome and will have more, and a caller that passes
// them in the wrong order gets a wrong row rather than a compile error.
type Outcome struct {
	// Action is the summary line: what was done, in the words shown to the group.
	Action string
	// MuteSeconds is how long the member was silenced for.
	MuteSeconds int64
	// RecallReason says why the messages were or were not taken back.
	RecallReason string
	// Recalls is one entry per message that was to be taken back.
	Recalls []RecallOutcome
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

// The two ways a lookup by id can fail to answer.
var (
	// ErrJudgementNotFound reports a receipt nothing was recorded under.
	//
	// It is separate from any other failure because it is an answer rather than a
	// fault: a number that was mistyped is the ordinary way to get here.
	ErrJudgementNotFound = errors.New("store: no judgement has that id")
	// ErrAmbiguousJudgement reports a prefix that matches more than one record.
	//
	// Guessing between them is not an option: the caller is about to show
	// somebody a punishment's reason, and showing the wrong one would be worse
	// than showing none.
	ErrAmbiguousJudgement = errors.New("store: that judgement id matches more than one record")
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
	SetOutcome(ctx context.Context, id string, outcome Outcome) error
	// Recent returns the judgements about one member since a moment, newest first.
	Recent(ctx context.Context, groupOpenID, subjectOpenID string, since time.Time) ([]Judgement, error)
	// Find returns one judgement by its id, or by a prefix of it that matches
	// exactly one record.
	//
	// The prefix is the point: the id is what a group is shown as a receipt, and
	// a receipt is short enough to read aloud. It is deliberately the only
	// lookup that is not scoped to a group -- the caller decides who may see
	// what, because who may see it depends on where the question was asked.
	//
	// ErrJudgementNotFound when nothing matches, ErrAmbiguousJudgement when the
	// prefix is too short to mean one thing.
	Find(ctx context.Context, idOrPrefix string) (Judgement, error)
	// CountViolations counts the judgements that found one member breaking a rule.
	//
	// Scoped to the member rather than to a group, which is the opposite of Recent,
	// and deliberately: this is the question "has this person been caught doing this
	// before", and the answer is about them wherever they did it. A zero moment
	// counts every record there is.
	//
	// A judgement that never happened is not a violation and is not counted here:
	// the verdict column separates "found violating" from "nobody looked", and
	// this is the first of the two.
	CountViolations(ctx context.Context, subjectOpenID string, since time.Time) (int, error)
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
	recalls, err := json.Marshal(entry.Recalls)
	if err != nil {
		return fmt.Errorf("encoding what was taken back: %w", err)
	}
	_, err = j.store.db.ExecContext(ctx, j.store.query(`
INSERT INTO moderation_judgements
    (id, group_openid, subject_openid, reporter_openid, category, verdict, model,
     message_ids, reason, action, mute_seconds, created_at, reasoning, recall_reason,
     recalls)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		entry.ID, entry.GroupOpenID, entry.SubjectOpenID, entry.ReporterOpenID,
		entry.Category, entry.Verdict, entry.Model, string(ids), entry.Reason,
		entry.Action, entry.MuteSeconds, entry.CreatedAt, entry.Reasoning,
		entry.RecallReason, string(recalls))
	if err != nil {
		return fmt.Errorf("recording a judgement: %w", err)
	}
	return nil
}

// SetOutcome implements JudgementStore.
func (j judgementStore) SetOutcome(ctx context.Context, id string, outcome Outcome) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("store: recording an outcome needs the judgement's id")
	}
	recalls, err := json.Marshal(outcome.Recalls)
	if err != nil {
		return fmt.Errorf("encoding what was taken back: %w", err)
	}
	if _, err := j.store.db.ExecContext(ctx, j.store.query(`
UPDATE moderation_judgements
SET action = ?, mute_seconds = ?, recall_reason = ?, recalls = ?
WHERE id = ?`),
		outcome.Action, outcome.MuteSeconds, outcome.RecallReason, string(recalls),
		id); err != nil {
		return fmt.Errorf("recording what followed a judgement: %w", err)
	}
	return nil
}

// Recent implements JudgementStore.
func (j judgementStore) Recent(ctx context.Context, groupOpenID, subjectOpenID string,
	since time.Time) ([]Judgement, error) {
	rows, err := j.store.db.QueryContext(ctx, j.store.query(`
SELECT `+judgementColumns+`
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
		entry, err := scanJudgement(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a judgement: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// CountViolations implements JudgementStore.
func (j judgementStore) CountViolations(ctx context.Context, subjectOpenID string,
	since time.Time) (int, error) {
	if strings.TrimSpace(subjectOpenID) == "" {
		// Nobody is not a member who has been caught: counting the records that
		// name no subject would answer a different question with a real number.
		return 0, nil
	}
	var count int
	err := j.store.db.QueryRowContext(ctx, j.store.query(`
SELECT count(*) FROM moderation_judgements
WHERE subject_openid = ? AND verdict = ? AND created_at >= ?`),
		subjectOpenID, JudgementViolation, since.Unix()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting a member's violations: %w", err)
	}
	return count, nil
}

// Find implements JudgementStore.
//
// A prefix is read as a prefix and never as a pattern: the metacharacters LIKE
// gives meaning to are escaped, so a receipt typed with a percent sign in it
// looks for a receipt with a percent sign in it and finds nothing, rather than
// matching everything.
func (j judgementStore) Find(ctx context.Context, idOrPrefix string) (Judgement, error) {
	prefix := strings.TrimSpace(idOrPrefix)
	if prefix == "" {
		return Judgement{}, ErrJudgementNotFound
	}
	// Two rows is enough to know a prefix is not unique, and asking for three
	// would only tell the same story more slowly.
	rows, err := j.store.db.QueryContext(ctx, j.store.query(`
SELECT `+judgementColumns+`
FROM moderation_judgements
WHERE id LIKE ? ESCAPE '\'
ORDER BY created_at DESC
LIMIT 2`), escapeLike(prefix)+"%")
	if err != nil {
		return Judgement{}, fmt.Errorf("looking up a judgement: %w", err)
	}
	defer rows.Close()

	var found []Judgement
	for rows.Next() {
		entry, err := scanJudgement(rows)
		if err != nil {
			return Judgement{}, fmt.Errorf("reading a judgement: %w", err)
		}
		found = append(found, entry)
	}
	if err := rows.Err(); err != nil {
		return Judgement{}, fmt.Errorf("looking up a judgement: %w", err)
	}
	switch len(found) {
	case 0:
		return Judgement{}, ErrJudgementNotFound
	case 1:
		return found[0], nil
	default:
		return Judgement{}, ErrAmbiguousJudgement
	}
}

// judgementColumns is the column list every read of this table uses.
//
// Written once because two queries that selected different columns would come
// back as the same struct with different fields quietly left empty.
const judgementColumns = `id, group_openid, subject_openid, reporter_openid, category,
       verdict, model, message_ids, reason, action, mute_seconds, created_at,
       reasoning, recall_reason, recalls`

// scanJudgement reads one row in the order judgementColumns names them.
func scanJudgement(rows *sql.Rows) (Judgement, error) {
	var entry Judgement
	var ids, recalls string
	if err := rows.Scan(&entry.ID, &entry.GroupOpenID, &entry.SubjectOpenID,
		&entry.ReporterOpenID, &entry.Category, &entry.Verdict, &entry.Model,
		&ids, &entry.Reason, &entry.Action, &entry.MuteSeconds,
		&entry.CreatedAt, &entry.Reasoning, &entry.RecallReason,
		&recalls); err != nil {
		return Judgement{}, err
	}
	// A list that cannot be read back is not worth failing the whole row over:
	// everything else about the judgement is still true.
	if ids != "" {
		_ = json.Unmarshal([]byte(ids), &entry.MessageIDs)
	}
	if recalls != "" {
		_ = json.Unmarshal([]byte(recalls), &entry.Recalls)
	}
	return entry, nil
}

// escapeLike makes a value literal inside a LIKE pattern.
//
// The backslash is escaped first: doing it last would escape the escapes the
// other two replacements added.
func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "%", `\%`)
	return strings.ReplaceAll(value, "_", `\_`)
}

// compile-time proof that the shape satisfies the contract.
var _ JudgementStore = judgementStore{}
