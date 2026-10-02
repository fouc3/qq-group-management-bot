package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// schemaVersion is the layout this build writes. A database above it was written
// by a newer build and is refused rather than guessed at.
const schemaVersion = 2

// migrations are applied in order, so a database created by an older build
// reaches the current layout without anybody running anything by hand.
//
// They use the portable subset described in the package comment: every statement
// here has to be accepted by both dialects unchanged. Notes that are easy to
// forget and expensive to remember later:
//
//   - no BOOLEAN (SQLite has none, and PostgreSQL is strict about it);
//   - no SERIAL or AUTOINCREMENT, so keys are TEXT;
//   - no NOW() or CURRENT_TIMESTAMP, so times are Unix seconds written by Go;
//   - no upper case or camel case identifiers, because PostgreSQL folds an
//     unquoted identifier to lower case while SQLite does not.
var migrations = []string{
	// 1: the initial layout.
	`
CREATE TABLE IF NOT EXISTS pending_verifications (
    token         TEXT   PRIMARY KEY,
    group_openid  TEXT   NOT NULL,
    member_openid TEXT   NOT NULL,
    joined_at     BIGINT NOT NULL,
    deadline      BIGINT NOT NULL,
    held_until    BIGINT NOT NULL,
    reported      INTEGER NOT NULL DEFAULT 0,
    settings      TEXT   NOT NULL DEFAULT '',
    created_at    BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS pending_by_member ON pending_verifications (group_openid, member_openid);
CREATE INDEX IF NOT EXISTS pending_by_deadline ON pending_verifications (deadline);
CREATE TABLE IF NOT EXISTS join_blacklist (
    id            TEXT   PRIMARY KEY,
    member_openid TEXT   NOT NULL DEFAULT '',
    union_openid  TEXT   NOT NULL DEFAULT '',
    reason        TEXT   NOT NULL DEFAULT '',
    added_at      BIGINT NOT NULL,
    added_by      TEXT   NOT NULL DEFAULT '',
    expires_at    BIGINT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS blacklist_by_member ON join_blacklist (member_openid);
CREATE UNIQUE INDEX IF NOT EXISTS blacklist_by_union ON join_blacklist (union_openid);
`,
	// 2: the blacklist identities have to be nullable.
	//
	// The first layout made them NOT NULL DEFAULT '', which a unique index then
	// refuses to hold twice: an entry naming only a union openid -- somebody
	// barred before they ever applied here -- would collide with the next such
	// entry on the empty string, so only one of them could ever exist.
	//
	// NULLIF turns the old empty strings into NULLs on the way across. Rows that
	// name neither identity are still refused, by the store rather than by the
	// column, which is where that rule belongs.
	`
CREATE TABLE join_blacklist_rebuilt (
    id            TEXT   PRIMARY KEY,
    member_openid TEXT,
    union_openid  TEXT,
    reason        TEXT   NOT NULL DEFAULT '',
    added_at      BIGINT NOT NULL,
    added_by      TEXT   NOT NULL DEFAULT '',
    expires_at    BIGINT NOT NULL DEFAULT 0
);
INSERT INTO join_blacklist_rebuilt
    (id, member_openid, union_openid, reason, added_at, added_by, expires_at)
SELECT id, NULLIF(member_openid, ''), NULLIF(union_openid, ''), reason, added_at,
       added_by, expires_at
FROM join_blacklist;
DROP TABLE join_blacklist;
ALTER TABLE join_blacklist_rebuilt RENAME TO join_blacklist;
CREATE UNIQUE INDEX IF NOT EXISTS blacklist_by_member ON join_blacklist (member_openid);
CREATE UNIQUE INDEX IF NOT EXISTS blacklist_by_union ON join_blacklist (union_openid);
`,
}

// sqlStore is the database/sql implementation, shared by both dialects.
type sqlStore struct {
	db      *sql.DB
	dialect dialect
}

// query rewrites a canonical query for this dialect. Every statement in this
// package goes through it, so the ?/$1 difference lives in exactly one place.
func (s *sqlStore) query(statement string) string { return s.dialect.rewrite(statement) }

func (s *sqlStore) Pending() PendingStore     { return pendingStore{s} }
func (s *sqlStore) Blacklist() BlacklistStore { return blacklistStore{s} }
func (s *sqlStore) Meta() MetaStore           { return metaStore{s} }
func (s *sqlStore) Close() error              { return s.db.Close() }

// metaStore implements MetaStore.
type metaStore struct{ store *sqlStore }

// Get implements MetaStore.
func (m metaStore) Get(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := m.store.db.QueryRowContext(ctx,
		m.store.query(`SELECT value FROM meta WHERE key = ?`), key).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("reading %q: %w", key, err)
	}
	return value, true, nil
}

// Set implements MetaStore.
//
// An upsert, for the same reason the schema version is written with one: the key
// usually does not exist yet the first time it is set.
func (m metaStore) Set(ctx context.Context, key, value string) error {
	if _, err := m.store.db.ExecContext(ctx, m.store.query(`
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value`), key, value); err != nil {
		return fmt.Errorf("writing %q: %w", key, err)
	}
	return nil
}

// create brings a fresh database up to schemaVersion and reports what it found.
func (s *sqlStore) create(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, s.query(`
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
)`)); err != nil {
		return fmt.Errorf("creating meta: %w", err)
	}

	current := 0
	var raw string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT value FROM meta WHERE key = ?`),
		"schema_version").Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A new database: everything is applied below.
	case err != nil:
		return fmt.Errorf("reading the schema version: %w", err)
	default:
		if _, err := fmt.Sscanf(raw, "%d", &current); err != nil {
			return fmt.Errorf("schema version %q is not a number", raw)
		}
	}
	if current > schemaVersion {
		return fmt.Errorf("%w: it is at %d and this build writes %d",
			ErrNewerSchema, current, schemaVersion)
	}

	for index := current; index < schemaVersion; index++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("starting migration %d: %w", index+1, err)
		}
		if _, err := tx.ExecContext(ctx, s.query(migrations[index])); err != nil {
			tx.Rollback()
			return fmt.Errorf("applying migration %d: %w", index+1, err)
		}
		// An upsert, not an UPDATE: a fresh database has no version row yet, so
		// an UPDATE would affect nothing, the version would never be recorded,
		// and every later start would apply the migrations again -- while the
		// "written by a newer build" check could never fire.
		if _, err := tx.ExecContext(ctx, s.query(`
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value`),
			"schema_version", fmt.Sprint(index+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("recording migration %d: %w", index+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %d: %w", index+1, err)
		}
	}
	return nil
}

// pendingStore implements PendingStore.
type pendingStore struct{ store *sqlStore }

// Load implements PendingStore. Entries the platform has already released are
// left out: nothing is left to answer for them, and returning them would invite
// a caller to act on a mute that no longer exists.
func (p pendingStore) Load(ctx context.Context, now time.Time) ([]Pending, error) {
	rows, err := p.store.db.QueryContext(ctx, p.store.query(`
SELECT token, group_openid, member_openid, joined_at, deadline, held_until, reported, settings
FROM pending_verifications
WHERE held_until > ?
ORDER BY deadline`), now.Unix())
	if err != nil {
		return nil, fmt.Errorf("loading pending verifications: %w", err)
	}
	defer rows.Close()

	var entries []Pending
	for rows.Next() {
		var entry Pending
		var reported int
		if err := rows.Scan(&entry.Token, &entry.GroupOpenID, &entry.MemberOpenID,
			&entry.JoinedAt, &entry.Deadline, &entry.HeldUntil, &reported,
			&entry.Settings); err != nil {
			return nil, fmt.Errorf("reading a pending verification: %w", err)
		}
		// Scanned as an integer and converted here: SQLite has no boolean type,
		// and the two drivers do not agree on whether one can be scanned into a
		// Go bool.
		entry.Reported = reported != 0
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// Put implements PendingStore.
func (p pendingStore) Put(ctx context.Context, entry Pending) error {
	reported := 0
	if entry.Reported {
		reported = 1
	}
	_, err := p.store.db.ExecContext(ctx, p.store.query(`
INSERT INTO pending_verifications
    (token, group_openid, member_openid, joined_at, deadline, held_until, reported, settings, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (token) DO UPDATE SET
    group_openid  = excluded.group_openid,
    member_openid = excluded.member_openid,
    joined_at     = excluded.joined_at,
    deadline      = excluded.deadline,
    held_until    = excluded.held_until,
    reported      = excluded.reported,
    settings      = excluded.settings`),
		entry.Token, entry.GroupOpenID, entry.MemberOpenID, entry.JoinedAt,
		entry.Deadline, entry.HeldUntil, reported, entry.Settings, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("saving a pending verification: %w", err)
	}
	return nil
}

// Delete implements PendingStore.
func (p pendingStore) Delete(ctx context.Context, token string) error {
	if _, err := p.store.db.ExecContext(ctx,
		p.store.query(`DELETE FROM pending_verifications WHERE token = ?`), token); err != nil {
		return fmt.Errorf("forgetting a pending verification: %w", err)
	}
	return nil
}

// MarkReported implements PendingStore.
func (p pendingStore) MarkReported(ctx context.Context, token string, _ time.Time) error {
	if _, err := p.store.db.ExecContext(ctx,
		p.store.query(`UPDATE pending_verifications SET reported = 1 WHERE token = ?`),
		token); err != nil {
		return fmt.Errorf("recording a report: %w", err)
	}
	return nil
}

// MoveDeadline implements PendingStore.
func (p pendingStore) MoveDeadline(ctx context.Context, token string, deadline, heldUntil time.Time) error {
	if _, err := p.store.db.ExecContext(ctx, p.store.query(`
UPDATE pending_verifications SET deadline = ?, held_until = ? WHERE token = ?`),
		deadline.Unix(), heldUntil.Unix(), token); err != nil {
		return fmt.Errorf("moving a deadline: %w", err)
	}
	return nil
}

// blacklistStore implements BlacklistStore.
type blacklistStore struct{ store *sqlStore }

// Barred implements BlacklistStore.
//
// The condition is built from the identities that were actually given, because
// an empty member openid would otherwise match the rows that only name a union
// openid -- and every applicant whose member openid this application never saw
// would be refused.
func (b blacklistStore) Barred(ctx context.Context, memberOpenID, unionOpenID string, now time.Time) (bool, error) {
	var identities []string
	var args []any
	if strings.TrimSpace(memberOpenID) != "" {
		identities = append(identities, "member_openid = ?")
		args = append(args, memberOpenID)
	}
	if strings.TrimSpace(unionOpenID) != "" {
		identities = append(identities, "union_openid = ?")
		args = append(args, unionOpenID)
	}
	if len(identities) == 0 {
		return false, nil
	}
	args = append(args, now.Unix())

	var found int
	err := b.store.db.QueryRowContext(ctx, b.store.query(`
SELECT 1 FROM join_blacklist
WHERE (`+strings.Join(identities, " OR ")+`)
  AND (expires_at = 0 OR expires_at > ?)
LIMIT 1`), args...).Scan(&found)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("checking the blacklist: %w", err)
	}
	return true, nil
}

// Add implements BlacklistStore.
//
// The matching identities are deleted first, in one transaction, because the
// entry has to replace any earlier one for either identity and `ON CONFLICT`
// cannot name two independent unique indexes portably.
func (b blacklistStore) Add(ctx context.Context, entry Barred) error {
	tx, err := b.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting the blacklist update: %w", err)
	}
	for _, identity := range []struct{ column, value string }{
		{"member_openid", entry.MemberOpenID},
		{"union_openid", entry.UnionOpenID},
	} {
		if strings.TrimSpace(identity.value) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, b.store.query(
			`DELETE FROM join_blacklist WHERE `+identity.column+` = ?`),
			identity.value); err != nil {
			tx.Rollback()
			return fmt.Errorf("replacing a blacklist entry: %w", err)
		}
	}
	// NULL rather than an empty string for an identity that was not given: the
	// unique index has to hold more than one entry that names only the other
	// identity, and it cannot do that for two empty strings.
	var member, union any
	if strings.TrimSpace(entry.MemberOpenID) != "" {
		member = entry.MemberOpenID
	}
	if strings.TrimSpace(entry.UnionOpenID) != "" {
		union = entry.UnionOpenID
	}
	if member == nil && union == nil {
		return errors.New("store: a blacklist entry needs a member openid or a union openid")
	}

	if _, err := tx.ExecContext(ctx, b.store.query(`
INSERT INTO join_blacklist
    (id, member_openid, union_openid, reason, added_at, added_by, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`),
		entry.ID, member, union, entry.Reason,
		entry.AddedAt, entry.AddedBy, entry.ExpiresAt); err != nil {
		tx.Rollback()
		return fmt.Errorf("adding a blacklist entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing a blacklist entry: %w", err)
	}
	return nil
}

// Remove implements BlacklistStore.
func (b blacklistStore) Remove(ctx context.Context, key string) error {
	if strings.TrimSpace(key) == "" {
		// An empty key would be a request to delete by an identity nobody has,
		// and it is refused rather than left to match nothing quietly.
		return errors.New("store: removing a blacklist entry needs an id or an openid")
	}
	if _, err := b.store.db.ExecContext(ctx, b.store.query(`
DELETE FROM join_blacklist WHERE id = ? OR member_openid = ? OR union_openid = ?`),
		key, key, key); err != nil {
		return fmt.Errorf("removing a blacklist entry: %w", err)
	}
	return nil
}

// List implements BlacklistStore.
func (b blacklistStore) List(ctx context.Context, limit int) ([]Barred, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := b.store.db.QueryContext(ctx, b.store.query(`
SELECT id, member_openid, union_openid, reason, added_at, added_by, expires_at
FROM join_blacklist
ORDER BY added_at DESC
LIMIT ?`), limit)
	if err != nil {
		return nil, fmt.Errorf("listing the blacklist: %w", err)
	}
	defer rows.Close()

	var entries []Barred
	for rows.Next() {
		var entry Barred
		// Scanned as nullable, because an entry may name only one of the two
		// identities, and the other is NULL rather than an empty string.
		var member, union sql.NullString
		if err := rows.Scan(&entry.ID, &member, &union, &entry.Reason,
			&entry.AddedAt, &entry.AddedBy, &entry.ExpiresAt); err != nil {
			return nil, fmt.Errorf("reading a blacklist entry: %w", err)
		}
		entry.MemberOpenID = member.String
		entry.UnionOpenID = union.String
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
