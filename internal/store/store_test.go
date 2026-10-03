package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// openTestStore returns a store on a throwaway database.
//
// SQLite is always exercised. Setting TEST_PG_DSN runs the same tests against a
// real PostgreSQL as well, which is how "works on both" is meant to be checked
// rather than asserted: without it these tests speak for SQLite alone, and the
// log line says which one ran.
func openTestStore(t *testing.T) Store {
	t.Helper()
	ctx := context.Background()

	if dsn := os.Getenv("TEST_PG_DSN"); dsn != "" {
		t.Logf("running against postgres")
		opened, err := Open(ctx, Config{Driver: "postgres", DSN: dsn})
		if err != nil {
			t.Fatalf("opening postgres: %v", err)
		}
		// A shared server has no per-test isolation, so the tables are emptied
		// to give each test the fresh database the SQLite path gets for free.
		reset(t, opened)
		t.Cleanup(func() { opened.Close() })
		return opened
	}

	t.Logf("running against sqlite (set TEST_PG_DSN to also run postgres)")
	opened, err := Open(ctx, Config{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "test.db"),
	})
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	t.Cleanup(func() { opened.Close() })
	return opened
}

// reset empties both tables through the store's own API, so it needs no SQL of
// its own and stays dialect agnostic.
func reset(t *testing.T, opened Store) {
	t.Helper()
	ctx := context.Background()
	entries, err := opened.Pending().Load(ctx, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("listing pending: %v", err)
	}
	for _, entry := range entries {
		if err := opened.Pending().Delete(ctx, entry.Token); err != nil {
			t.Fatalf("clearing pending: %v", err)
		}
	}
	barred, err := opened.Blacklist().List(ctx, 1000)
	if err != nil {
		t.Fatalf("listing the blacklist: %v", err)
	}
	for _, entry := range barred {
		if err := opened.Blacklist().Remove(ctx, entry.ID); err != nil {
			t.Fatalf("clearing the blacklist: %v", err)
		}
	}
}

// TestRewrite covers the one difference between the dialects.
func TestRewrite(t *testing.T) {
	postgres := dialect{name: "postgres", postgres: true}
	sqlite := dialect{name: "sqlite"}

	cases := map[string]struct {
		dialect dialect
		in      string
		want    string
	}{
		"sqlite is left alone": {sqlite, `SELECT a FROM b WHERE c = ?`, `SELECT a FROM b WHERE c = ?`},
		"postgres numbers them": {postgres, `SELECT a FROM b WHERE c = ? AND d = ?`,
			`SELECT a FROM b WHERE c = $1 AND d = $2`},
		"a question mark in a literal is data": {postgres,
			`SELECT a FROM b WHERE c = ? AND d = 'what? really' AND e = ?`,
			`SELECT a FROM b WHERE c = $1 AND d = 'what? really' AND e = $2`},
		"a doubled quote inside a literal": {postgres,
			`SELECT a FROM b WHERE c = 'it''s ?' AND d = ?`,
			`SELECT a FROM b WHERE c = 'it''s ?' AND d = $1`},
		"a quoted identifier": {postgres,
			`SELECT "?" FROM b WHERE c = ?`, `SELECT "?" FROM b WHERE c = $1`},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := testCase.dialect.rewrite(testCase.in); got != testCase.want {
				t.Errorf("rewrite(%q) = %q, want %q", testCase.in, got, testCase.want)
			}
		})
	}
}

// TestPendingRoundTrip covers saving, reading back and forgetting a hold.
func TestPendingRoundTrip(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	entry := Pending{
		Token:        "TOKEN-1",
		GroupOpenID:  "GROUP-1",
		MemberOpenID: "MEMBER-1",
		JoinedAt:     now.Unix(),
		Deadline:     now.Add(48 * time.Hour).Unix(),
		HeldUntil:    now.Add(29 * 24 * time.Hour).Unix(),
		Settings:     `{"mute_minutes":41760}`,
	}
	if err := opened.Pending().Put(ctx, entry); err != nil {
		t.Fatalf("Put: %v", err)
	}

	entries, err := opened.Pending().Load(ctx, now)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Load returned %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.Token != entry.Token || got.MemberOpenID != entry.MemberOpenID ||
		got.HeldUntil != entry.HeldUntil || got.Settings != entry.Settings || got.Reported {
		t.Errorf("Load returned %+v, want %+v", got, entry)
	}

	// A hold the platform has already released is not returned: there is nothing
	// left to answer for it, and returning it would invite acting on a mute that
	// no longer exists.
	released, err := opened.Pending().Load(ctx, now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("Load after release: %v", err)
	}
	if len(released) != 0 {
		t.Errorf("Load returned %d released entries, want none", len(released))
	}

	if err := opened.Pending().MarkReported(ctx, entry.Token, now); err != nil {
		t.Fatalf("MarkReported: %v", err)
	}
	moved := now.Add(96 * time.Hour)
	if err := opened.Pending().MoveDeadline(ctx, entry.Token, moved, moved); err != nil {
		t.Fatalf("MoveDeadline: %v", err)
	}
	entries, err = opened.Pending().Load(ctx, now)
	if err != nil {
		t.Fatalf("Load after the update: %v", err)
	}
	if len(entries) != 1 || !entries[0].Reported || entries[0].Deadline != moved.Unix() {
		t.Errorf("after the update: %+v", entries)
	}

	if err := opened.Pending().Delete(ctx, entry.Token); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	entries, err = opened.Pending().Load(ctx, now)
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Load returned %d entries after a delete, want none", len(entries))
	}
}

// TestBlacklistBarredCoversBothIdentities covers the lookup, including the case
// that would silently refuse everybody: an applicant whose member openid is
// empty must not match the rows that only name a union openid.
func TestBlacklistBarredCoversBothIdentities(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := opened.Blacklist().Add(ctx, Barred{
		ID: "ENTRY-1", MemberOpenID: "MEMBER-1", UnionOpenID: "UNION-1",
		Reason: "test", AddedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for name, identity := range map[string][2]string{
		"by member openid": {"MEMBER-1", ""},
		"by union openid":  {"", "UNION-1"},
		"by both":          {"MEMBER-1", "UNION-1"},
	} {
		t.Run(name, func(t *testing.T) {
			barred, err := opened.Blacklist().Barred(ctx, identity[0], identity[1], now)
			if err != nil {
				t.Fatalf("Barred: %v", err)
			}
			if !barred {
				t.Error("Barred = false, want true")
			}
		})
	}

	t.Run("somebody else is not barred", func(t *testing.T) {
		barred, err := opened.Blacklist().Barred(ctx, "OTHER-MEMBER", "OTHER-UNION", now)
		if err != nil {
			t.Fatalf("Barred: %v", err)
		}
		if barred {
			t.Error("Barred = true for somebody who is not on the list")
		}
	})

	t.Run("no identity at all is not barred", func(t *testing.T) {
		barred, err := opened.Blacklist().Barred(ctx, "", "", now)
		if err != nil {
			t.Fatalf("Barred: %v", err)
		}
		if barred {
			t.Error("Barred = true with no identity, which would refuse everybody")
		}
	})

	t.Run("an expired entry stops barring", func(t *testing.T) {
		if err := opened.Blacklist().Add(ctx, Barred{
			ID: "ENTRY-2", MemberOpenID: "MEMBER-2",
			AddedAt: now.Unix(), ExpiresAt: now.Add(-time.Hour).Unix(),
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		barred, err := opened.Blacklist().Barred(ctx, "MEMBER-2", "", now)
		if err != nil {
			t.Fatalf("Barred: %v", err)
		}
		if barred {
			t.Error("Barred = true for an entry that has expired")
		}
	})
}

// TestBlacklistListAndRemove covers the calls an operator's command uses.
func TestBlacklistListAndRemove(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := opened.Blacklist().Add(ctx, Barred{
		ID: "ENTRY-1", MemberOpenID: "MEMBER-1", Reason: "spam", AddedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	entries, err := opened.Blacklist().List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Reason != "spam" {
		t.Fatalf("List returned %+v", entries)
	}

	// Removing by the identity an operator would have to hand is the useful case:
	// the row's ID is not something anyone types.
	if err := opened.Blacklist().Remove(ctx, "MEMBER-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	barred, err := opened.Blacklist().Barred(ctx, "MEMBER-1", "", now)
	if err != nil {
		t.Fatalf("Barred: %v", err)
	}
	if barred {
		t.Error("the entry is still barred after being removed")
	}
}

// TestTheSchemaIsNotDowngraded covers the refusal that matters: an older build
// must not open a database a newer one wrote, because the rows it would misread
// are real holds on real people.
func TestTheSchemaIsNotDowngraded(t *testing.T) {
	opened := openTestStore(t)
	sql, ok := opened.(*sqlStore)
	if !ok {
		t.Skip("the dialect under test does not expose its connection")
	}
	ctx := context.Background()
	if _, err := sql.db.ExecContext(ctx,
		sql.query(`UPDATE meta SET value = ? WHERE key = ?`),
		"999", "schema_version"); err != nil {
		t.Fatalf("raising the recorded version: %v", err)
	}
	if err := sql.create(ctx); err == nil {
		t.Error("a database at a newer version must be refused")
	}
}

// TestMetaRoundTrip covers the bookkeeping the one-off import depends on: a key
// that is absent has to be distinguishable from one set to an empty value, or a
// marker that was never written would read as written.
func TestMetaRoundTrip(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()

	if _, found, err := opened.Meta().Get(ctx, "IMPORTED"); err != nil || found {
		t.Fatalf("Get on an absent key = found %v, err %v; want false, nil", found, err)
	}
	if err := opened.Meta().Set(ctx, "IMPORTED", "2026-10-02"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value, found, err := opened.Meta().Get(ctx, "IMPORTED")
	if err != nil || !found || value != "2026-10-02" {
		t.Fatalf("Get = %q, %v, %v; want the value that was set", value, found, err)
	}
	// Setting again replaces, which is what a marker being re-stamped needs.
	if err := opened.Meta().Set(ctx, "IMPORTED", "later"); err != nil {
		t.Fatalf("Set again: %v", err)
	}
	if value, _, _ := opened.Meta().Get(ctx, "IMPORTED"); value != "later" {
		t.Errorf("Get = %q, want the replacement", value)
	}
}

// openTestStoreAt builds a database by hand at an older version, with the statements a test
// gives it, and opens it with this build -- which is what a deployment upgrading from an older
// release does. The migrations up to that version are the ones this build carries, so the test
// is against the layout the older build really wrote rather than a guess at it.
//
// SQLite only: it is built as a file, and the postgres path is the same code reached through
// the same migrations.
func openTestStoreAt(t *testing.T, version int, build string) Store {
	t.Helper()
	if dsn := os.Getenv("TEST_PG_DSN"); dsn != "" {
		t.Skip("a hand-built older database is a file, not a server")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "older.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	}
	for index := 0; index < version; index++ {
		statements = append(statements, migrations[index])
	}
	statements = append(statements,
		`INSERT INTO meta (key, value) VALUES ('schema_version', '`+strconv.Itoa(version)+`')`,
		build)
	for _, statement := range statements {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			raw.Close()
			t.Fatalf("building a version %d database: %v", version, err)
		}
	}
	raw.Close()

	opened, err := Open(ctx, Config{Driver: "sqlite", DSN: path})
	if err != nil {
		t.Fatalf("opening a version %d database: %v", version, err)
	}
	t.Cleanup(func() { opened.Close() })
	return opened
}
