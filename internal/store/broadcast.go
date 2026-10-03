package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Broadcast is one message an administrator had the bot post in one group.
//
// It exists because anonymity is about the group rather than about the record: what a
// group reads does not say who asked for it, and whoever runs the bot -- or, for their
// own group, its administrators -- can still find out. A notice nobody can trace is a
// notice anybody can abuse.
type Broadcast struct {
	// Token is the card the broadcast was written on. With the group it is the identity
	// of the row, so a send that is somehow repeated does not double the record.
	Token string
	// FromGroupOpenID is the group the card was opened in, or empty when it was opened in
	// a single chat with the bot.
	FromGroupOpenID string
	// SenderOpenID is who asked for it. This is the field the record exists for.
	SenderOpenID string
	// GroupOpenID is the group this row is about.
	//
	// One row per group: what an audit asks is "who had this posted here", group by
	// group, and a broadcast that went to three groups is three answers.
	GroupOpenID string
	// Anonymous reports whether the notice said who asked for it.
	Anonymous bool
	// Markdown reports whether the text was rendered as markdown rather than taken
	// literally.
	Markdown bool
	// Content is what was posted, kept rather than summarised.
	//
	// A record that cannot be read back is not a record of what a group was told, and
	// the question an audit answers is often about the words themselves.
	Content string
	// SentAt is when it was posted, in Unix seconds.
	SentAt int64
	// MessageID is what the platform called the message, or empty when the send answered
	// without one.
	MessageID string
}

// BroadcastStore is the record of what was broadcast, and by whom.
type BroadcastStore interface {
	// Record writes one group's copy of a broadcast.
	//
	// The same broadcast delivered twice is the same fact rather than an error: the
	// identity of the row is the card it was written on together with the group it went
	// to.
	Record(ctx context.Context, broadcast Broadcast) error
	// ListByGroup returns what was posted in one group, newest first, up to limit.
	ListByGroup(ctx context.Context, groupOpenID string, limit int) ([]Broadcast, error)
}

// broadcastStore implements BroadcastStore.
type broadcastStore struct{ store *sqlStore }

// Record implements BroadcastStore.
func (b broadcastStore) Record(ctx context.Context, broadcast Broadcast) error {
	if strings.TrimSpace(broadcast.Token) == "" {
		return errors.New("store: a broadcast needs the card it was written on")
	}
	if strings.TrimSpace(broadcast.GroupOpenID) == "" {
		return errors.New("store: a broadcast needs the group it went to")
	}
	if strings.TrimSpace(broadcast.SenderOpenID) == "" {
		return errors.New("store: a broadcast needs the member who asked for it")
	}
	if broadcast.SentAt == 0 {
		broadcast.SentAt = time.Now().Unix()
	}
	// DO NOTHING rather than an update: the same broadcast recorded twice says nothing
	// new, and an update would only rewrite a row with itself.
	if _, err := b.store.db.ExecContext(ctx, b.store.query(`
INSERT INTO broadcasts
    (token, from_group, sender_openid, group_openid, anonymous, markdown, content,
     sent_at, message_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (token, group_openid) DO NOTHING`),
		broadcast.Token, broadcast.FromGroupOpenID, broadcast.SenderOpenID,
		broadcast.GroupOpenID, boolValue(broadcast.Anonymous),
		boolValue(broadcast.Markdown), broadcast.Content, broadcast.SentAt,
		broadcast.MessageID); err != nil {
		return fmt.Errorf("recording a broadcast: %w", err)
	}
	return nil
}

// ListByGroup implements BroadcastStore.
func (b broadcastStore) ListByGroup(ctx context.Context, groupOpenID string, limit int) ([]Broadcast, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := b.store.db.QueryContext(ctx, b.store.query(`
SELECT token, from_group, sender_openid, group_openid, anonymous, markdown, content,
       sent_at, message_id
FROM broadcasts
WHERE group_openid = ?
ORDER BY sent_at DESC, token DESC
LIMIT ?`), groupOpenID, limit)
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
		if err := rows.Scan(&broadcast.Token, &broadcast.FromGroupOpenID,
			&broadcast.SenderOpenID, &broadcast.GroupOpenID, &anonymous, &markdown,
			&broadcast.Content, &broadcast.SentAt, &broadcast.MessageID); err != nil {
			return nil, fmt.Errorf("reading a broadcast: %w", err)
		}
		broadcast.Anonymous = anonymous != 0
		broadcast.Markdown = markdown != 0
		broadcasts = append(broadcasts, broadcast)
	}
	return broadcasts, rows.Err()
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
