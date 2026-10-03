package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Broadcast is one message an administrator had the bot post, and where it went.
//
// It exists because anonymity is about the group rather than about the record: what a group
// reads does not say who asked for it, and whoever runs the bot -- or, for their own group,
// its administrators -- can still find out. A notice nobody can trace is a notice anybody
// can abuse.
//
// One row per notice rather than one per group it reached: it is one act by one member, and
// an audit that answered "who sent this here" out of three copies of the same sentence would
// be three answers to one question. Where it went is the targets.
type Broadcast struct {
	// Token is the card the broadcast was written on. It is the identity of the row, so a
	// send that is somehow repeated does not double the record.
	Token string
	// SenderOpenID is who asked for it. This is the field the record exists for.
	//
	// It is a member's openid in a single chat, which is where a broadcast is written: the
	// card lives there, so the sender is the person who wrote it rather than anybody in a
	// group.
	SenderOpenID string
	// Anonymous reports whether the notice said who asked for it.
	Anonymous bool
	// Markdown reports whether the text was rendered as markdown rather than taken
	// literally.
	Markdown bool
	// Content is what was posted, kept rather than summarised.
	//
	// A record that cannot be read back is not a record of what a group was told, and the
	// question an audit answers is often about the words themselves.
	Content string
	// SentAt is when it was posted, in Unix seconds.
	SentAt int64
	// Targets are the groups it reached, in the order they were sent to.
	Targets []Target
}

// Target is one group a broadcast reached.
type Target struct {
	GroupOpenID string
	// MessageID is what the platform called the message there, or empty when the send
	// answered without one.
	MessageID string
}

// BroadcastStore is the record of what was broadcast, and by whom.
type BroadcastStore interface {
	// Record writes one broadcast and the groups it reached.
	//
	// The same broadcast delivered twice is the same fact rather than an error: the identity
	// of the row is the card it was written on, and a group it already reached is not added
	// twice.
	Record(ctx context.Context, broadcast Broadcast) error
	// ListByGroups returns the broadcasts that reached any of these groups, newest first,
	// up to limit. A broadcast that reached two of them is one answer, not two.
	ListByGroups(ctx context.Context, groups []string, limit int) ([]Broadcast, error)
}

// broadcastStore implements BroadcastStore.
type broadcastStore struct{ store *sqlStore }

// Record implements BroadcastStore.
func (b broadcastStore) Record(ctx context.Context, broadcast Broadcast) error {
	if strings.TrimSpace(broadcast.Token) == "" {
		return errors.New("store: a broadcast needs the card it was written on")
	}
	if strings.TrimSpace(broadcast.SenderOpenID) == "" {
		return errors.New("store: a broadcast needs the member who asked for it")
	}
	if len(broadcast.Targets) == 0 {
		return errors.New("store: a broadcast needs the groups it went to")
	}
	for _, target := range broadcast.Targets {
		if strings.TrimSpace(target.GroupOpenID) == "" {
			return errors.New("store: a broadcast target needs its group")
		}
	}
	if broadcast.SentAt == 0 {
		broadcast.SentAt = time.Now().Unix()
	}

	// Both writes or neither: a row without its targets is a notice that reached nowhere, and
	// a target without its row is a group told something no record accounts for.
	tx, err := b.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("recording a broadcast: %w", err)
	}
	defer tx.Rollback()

	// DO NOTHING rather than an update: the same broadcast recorded twice says nothing new,
	// and an update would only rewrite a row with itself.
	if _, err := tx.ExecContext(ctx, b.store.query(`
INSERT INTO broadcasts
    (token, sender_openid, anonymous, markdown, content, sent_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (token) DO NOTHING`),
		broadcast.Token, broadcast.SenderOpenID, boolValue(broadcast.Anonymous),
		boolValue(broadcast.Markdown), broadcast.Content, broadcast.SentAt); err != nil {
		return fmt.Errorf("recording a broadcast: %w", err)
	}
	for index, target := range broadcast.Targets {
		if _, err := tx.ExecContext(ctx, b.store.query(`
INSERT INTO broadcast_targets (token, group_openid, message_id, position)
VALUES (?, ?, ?, ?)
ON CONFLICT (token, group_openid) DO NOTHING`),
			broadcast.Token, target.GroupOpenID, target.MessageID, index); err != nil {
			return fmt.Errorf("recording where a broadcast went: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recording a broadcast: %w", err)
	}
	return nil
}

// ListByGroups implements BroadcastStore.
func (b broadcastStore) ListByGroups(ctx context.Context, groups []string, limit int) ([]Broadcast, error) {
	if limit <= 0 {
		limit = 10
	}
	wanted := make([]string, 0, len(groups))
	for _, group := range groups {
		if strings.TrimSpace(group) != "" {
			wanted = append(wanted, group)
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	// Asked of the targets rather than of the notices, so that a broadcast which reached two
	// of these groups is one row here and not two. EXISTS rather than a join for the same
	// reason, one step further along.
	in, arguments := inClause(wanted)
	rows, err := b.store.db.QueryContext(ctx, b.store.query(`
SELECT b.token, b.sender_openid, b.anonymous, b.markdown, b.content, b.sent_at
FROM broadcasts b
WHERE EXISTS (SELECT 1 FROM broadcast_targets t
              WHERE t.token = b.token AND t.group_openid IN (`+in+`))
ORDER BY b.sent_at DESC, b.token DESC
LIMIT ?`), append(arguments, limit)...)
	if err != nil {
		return nil, fmt.Errorf("listing broadcasts: %w", err)
	}
	defer rows.Close()

	var broadcasts []Broadcast
	for rows.Next() {
		var (
			broadcast           Broadcast
			anonymous, markdown int
		)
		if err := rows.Scan(&broadcast.Token, &broadcast.SenderOpenID, &anonymous,
			&markdown, &broadcast.Content, &broadcast.SentAt); err != nil {
			return nil, fmt.Errorf("reading a broadcast: %w", err)
		}
		broadcast.Anonymous = anonymous != 0
		broadcast.Markdown = markdown != 0
		broadcasts = append(broadcasts, broadcast)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(broadcasts) == 0 {
		return nil, nil
	}

	// Where each of them went, in one more question rather than one per notice: an audit
	// shows five of them at a time, and five questions to answer "which groups" is five
	// round trips for a list.
	tokens := make([]string, 0, len(broadcasts))
	for _, broadcast := range broadcasts {
		tokens = append(tokens, broadcast.Token)
	}
	in, arguments = inClause(tokens)
	targetRows, err := b.store.db.QueryContext(ctx, b.store.query(`
SELECT token, group_openid, message_id
FROM broadcast_targets
WHERE token IN (`+in+`)
ORDER BY position`), arguments...)
	if err != nil {
		return nil, fmt.Errorf("reading where a broadcast went: %w", err)
	}
	defer targetRows.Close()

	byToken := map[string][]Target{}
	for targetRows.Next() {
		var token string
		var target Target
		if err := targetRows.Scan(&token, &target.GroupOpenID, &target.MessageID); err != nil {
			return nil, fmt.Errorf("reading where a broadcast went: %w", err)
		}
		byToken[token] = append(byToken[token], target)
	}
	if err := targetRows.Err(); err != nil {
		return nil, err
	}
	for index := range broadcasts {
		broadcasts[index].Targets = byToken[broadcasts[index].Token]
	}
	return broadcasts, nil
}

// inClause is a placeholders-and-arguments pair for a list of values.
//
// Built rather than written out because the lists here are as long as the caller's: the
// groups somebody administers, or the notices on one page of a record.
func inClause(values []string) (string, []any) {
	placeholders := make([]string, 0, len(values))
	arguments := make([]any, 0, len(values))
	for _, value := range values {
		placeholders = append(placeholders, "?")
		arguments = append(arguments, value)
	}
	return strings.Join(placeholders, ", "), arguments
}

// boolValue writes a bool the way both dialects accept it: neither has a BOOLEAN that
// this package can rely on.
func boolValue(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

// compile-time proof that the shape satisfies the contract.
var _ BroadcastStore = broadcastStore{}
