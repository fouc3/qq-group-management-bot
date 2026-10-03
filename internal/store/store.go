// Package store is the bot's data layer: one set of semantics, written once
// against database/sql, with SQLite as the default dialect and PostgreSQL as the
// same code against a different one.
//
// What lives here is what has to outlive a restart: the members who are being
// held until they verify, and the applicants who are barred from joining. The
// configuration is not here and never will be -- it is a file a person edits,
// and its credentials have no business in a database.
//
// Every difference between the two dialects is confined to dialect.go. Nothing
// in this package may use a statement or a type that only one of them accepts,
// because the whole point is that a switch in the configuration is a switch and
// not a port.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Pending is one member held until they verify.
type Pending struct {
	Token        string
	GroupOpenID  string
	MemberOpenID string
	// JoinedAt, Deadline and HeldUntil are Unix seconds.
	//
	// Seconds rather than a time type because the column is an integer in both
	// dialects, and a stored time would otherwise be formatted by whichever
	// driver wrote it.
	JoinedAt  int64
	Deadline  int64
	HeldUntil int64
	// Reported records that the deadline has already been acted on.
	Reported bool
	// Settings is the feature's own configuration, frozen when the member was
	// held and stored as an opaque string.
	//
	// Opaque on purpose: the store has no opinion about verification settings,
	// and a schema that grew a column per setting would have to be migrated
	// every time one is added.
	Settings string
}

// Barred is one entry of the join blacklist.
type Barred struct {
	// ID is a random key rather than the applicant's openid, so an entry can name
	// a union openid for somebody who has never applied here and therefore has no
	// member openid to be keyed on.
	ID           string
	MemberOpenID string
	UnionOpenID  string
	Reason       string
	AddedAt      int64
	AddedBy      string
	// ExpiresAt is 0 for a permanent entry.
	ExpiresAt int64
}

// Store is the data layer as a whole.
type Store interface {
	// Pending holds the members waiting to verify.
	Pending() PendingStore
	// Blacklist holds the applicants barred from joining.
	Blacklist() BlacklistStore
	// Judgements holds the record of what was judged and what followed.
	Judgements() JudgementStore
	// MemberEvents holds who joined a group and who left it.
	//
	// It is the only place this is kept: the platform refuses this application
	// the member-list endpoints, so a group's membership cannot be read back.
	MemberEvents() MemberEventStore
	// Meta holds small bookkeeping values that have to survive a restart, such
	// as whether a one-off import has already run.
	//
	// Not a general dumping ground: nothing here is typed, so a value belongs
	// only when it has no better home.
	Meta() MetaStore
	// Close releases the database.
	Close() error
}

// MetaStore is small key/value storage.
type MetaStore interface {
	// Get returns a value and whether it was set.
	Get(ctx context.Context, key string) (string, bool, error)
	// Set stores a value, replacing any earlier one.
	Set(ctx context.Context, key, value string) error
}

// PendingStore holds the members waiting to verify.
type PendingStore interface {
	// Load returns the entries whose hold has not been released yet.
	//
	// Released entries are left out rather than returned and filtered by the
	// caller: their mute is over, so there is nothing left to answer or to act
	// on, and handing them back only invites a caller to do so by mistake.
	Load(ctx context.Context, now time.Time) ([]Pending, error)
	// Put records an entry, replacing any entry with the same token.
	Put(ctx context.Context, entry Pending) error
	// Delete forgets an entry.
	Delete(ctx context.Context, token string) error
	// MarkReported records that the deadline has been acted on.
	MarkReported(ctx context.Context, token string, at time.Time) error
	// MoveDeadline pushes both stamps forward, which is what a renewal does.
	MoveDeadline(ctx context.Context, token string, deadline, heldUntil time.Time) error
}

// BlacklistStore holds the applicants barred from joining.
type BlacklistStore interface {
	// Barred reports whether either identity is barred as of now.
	//
	// Both identities are looked up in one call, because a caller that checked
	// them itself would be doing the store's job and would have to be changed
	// whenever the list gains a way to be keyed.
	Barred(ctx context.Context, memberOpenID, unionOpenID string, now time.Time) (bool, error)
	// Add records an entry, replacing one with the same key.
	Add(ctx context.Context, entry Barred) error
	// Remove forgets an entry by ID, member openid or union openid.
	Remove(ctx context.Context, key string) error
	// List returns entries, newest first.
	List(ctx context.Context, limit int) ([]Barred, error)
}

// Config says which database to open and how.
type Config struct {
	// Driver is sqlite or postgres.
	Driver string
	// DSN is the file path for sqlite and a connection string for postgres.
	DSN string
	// MaxOpenConns caps the pool. SQLite wants one, because it has a single
	// writer and more connections only move the contention into the driver.
	MaxOpenConns int
}

// ErrNoDriver reports a driver this build cannot open.
//
// It is a distinct error because the fix is not in the configuration: either the
// name is wrong, or the build is missing the dependency for that dialect.
var ErrNoDriver = errors.New("store: unsupported or unregistered driver")

// ErrNewerSchema reports a database written by a newer build.
//
// Refusing to start is the point: an older build that guessed at a newer layout
// would drop or misread rows, and those rows are real holds on real people.
var ErrNewerSchema = errors.New("store: the database was written by a newer build")

// openSQLiteDriver and openPostgresDriver are set by the driver files, so a
// build that omits one reports a missing driver rather than failing to compile.
var (
	openSQLite = func(context.Context, string, int) (*sqlStore, error) {
		return nil, fmt.Errorf("%w: sqlite", ErrNoDriver)
	}
	openPostgres = func(context.Context, string, int) (*sqlStore, error) {
		return nil, fmt.Errorf("%w: postgres", ErrNoDriver)
	}
)

// Open opens the configured database and brings its layout up to date.
func Open(ctx context.Context, cfg Config) (Store, error) {
	connections := cfg.MaxOpenConns
	if connections <= 0 {
		connections = 1
	}

	var (
		opened *sqlStore
		err    error
	)
	switch cfg.Driver {
	case "sqlite", "":
		opened, err = openSQLite(ctx, cfg.DSN, connections)
	case "postgres":
		opened, err = openPostgres(ctx, cfg.DSN, connections)
	default:
		return nil, fmt.Errorf("%w: %q", ErrNoDriver, cfg.Driver)
	}
	if err != nil {
		return nil, err
	}

	// The layout is created here rather than inside each driver: it is the same
	// schema for both, and a database that is missing its tables must not be
	// handed to anything that would then read an empty list from it.
	if err := opened.create(ctx); err != nil {
		opened.Close()
		return nil, err
	}
	return opened, nil
}
