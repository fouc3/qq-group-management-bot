package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MemberEvent is one member joining a group, or one leaving it.
//
// It exists because nothing else keeps this. The platform refuses this
// application the member-list endpoints (40012010, measured), so a group's
// membership cannot be read back at any point: a departure is invisible unless it
// was written down while the event was in hand. The log is therefore the only
// answer to "who was in this group, and when did they go".
type MemberEvent struct {
	// GroupOpenID is the group it happened in.
	GroupOpenID string
	// GroupName is what the group was called when it happened.
	//
	// Stored rather than looked up later, because later is exactly when it cannot
	// be: the group may be renamed, or left, and the name it had at the time is
	// the one a person reading the log will recognise.
	GroupName string
	// GroupQQID is the group's number, or zero when the configuration does not
	// name it. The openid is what the platform speaks; the number is what a
	// person does.
	GroupQQID int64
	// MemberOpenID is who it happened to.
	MemberOpenID string
	// Kind is MemberJoined or MemberLeft.
	Kind string
	// EventAt is the platform's own timestamp, in Unix seconds. It is the time
	// the event is about, which is not always the time it arrived.
	EventAt int64
}

// The two kinds of event there are.
//
// There is deliberately no separate kind for being removed by an administrator:
// the platform reports a departure as one event with no operator in it, so
// "this member left" and "this member was removed" cannot be told apart from
// what is delivered. A log that guessed would be worse than one that says less.
const (
	// MemberJoined is somebody joining a group.
	MemberJoined = "join"
	// MemberLeft is somebody leaving a group, whether by choice or by force.
	MemberLeft = "leave"
)

// MemberEventStore is the log of who came and went.
type MemberEventStore interface {
	// Record writes one event.
	//
	// An event that is already in the log is not an error and does not replace
	// anything: the same event delivered twice is the same fact, and the identity
	// of the row is the fact itself -- group, member, kind and time.
	Record(ctx context.Context, event MemberEvent) error
	// List returns events, newest first, up to limit.
	List(ctx context.Context, limit int) ([]MemberEvent, error)
}

// memberEventStore implements MemberEventStore.
type memberEventStore struct{ store *sqlStore }

// Record implements MemberEventStore.
func (m memberEventStore) Record(ctx context.Context, event MemberEvent) error {
	if strings.TrimSpace(event.GroupOpenID) == "" ||
		strings.TrimSpace(event.MemberOpenID) == "" {
		return errors.New("store: a member event needs a group and a member")
	}
	if event.Kind != MemberJoined && event.Kind != MemberLeft {
		return fmt.Errorf("store: %q is not a kind of member event", event.Kind)
	}
	if event.EventAt == 0 {
		event.EventAt = time.Now().Unix()
	}
	// DO NOTHING rather than an update: nothing about an event changes when it
	// arrives twice, and an update would only rewrite a row with itself.
	if _, err := m.store.db.ExecContext(ctx, m.store.query(`
INSERT INTO member_events
    (group_openid, group_name, group_qq, member_openid, kind, event_at, recorded_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (group_openid, member_openid, kind, event_at) DO NOTHING`),
		event.GroupOpenID, event.GroupName, event.GroupQQID, event.MemberOpenID,
		event.Kind, event.EventAt, time.Now().Unix()); err != nil {
		return fmt.Errorf("recording a member event: %w", err)
	}
	return nil
}

// List implements MemberEventStore.
func (m memberEventStore) List(ctx context.Context, limit int) ([]MemberEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := m.store.db.QueryContext(ctx, m.store.query(`
SELECT group_openid, group_name, group_qq, member_openid, kind, event_at
FROM member_events
ORDER BY event_at DESC, recorded_at DESC
LIMIT ?`), limit)
	if err != nil {
		return nil, fmt.Errorf("listing member events: %w", err)
	}
	defer rows.Close()

	var events []MemberEvent
	for rows.Next() {
		var event MemberEvent
		if err := rows.Scan(&event.GroupOpenID, &event.GroupName, &event.GroupQQID,
			&event.MemberOpenID, &event.Kind, &event.EventAt); err != nil {
			return nil, fmt.Errorf("reading a member event: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// compile-time proof that the shape satisfies the contract.
var _ MemberEventStore = memberEventStore{}
