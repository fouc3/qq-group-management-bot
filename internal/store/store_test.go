package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestAHoldWithNoExpiryIsAlwaysLoaded covers the row a pseudo-mute leaves behind.
//
// held_until is zero for a hold that nobody releases -- the member is held until
// they verify -- so a load that asks whether the hold is still in the future has to
// keep it: read as an ordinary moment, the zero would be a hold that ended before
// the epoch and every one of them would be dropped as released.
func TestAHoldWithNoExpiryIsAlwaysLoaded(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	entry := Pending{
		Token:        "TOKEN-NO-EXPIRY",
		GroupOpenID:  "GROUP-1",
		MemberOpenID: "MEMBER-2",
		JoinedAt:     now.Unix(),
		Deadline:     now.Add(48 * time.Hour).Unix(),
		HeldUntil:    0,
		Settings:     `{"MuteMode":"pseudo"}`,
	}
	if err := opened.Pending().Put(ctx, entry); err != nil {
		t.Fatalf("Put: %v", err)
	}

	entries, err := opened.Pending().Load(ctx, now.Add(365*24*time.Hour))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Load returned %d entries a year later, want the hold that never "+
			"runs out", len(entries))
	}
	if entries[0].HeldUntil != 0 {
		t.Errorf("HeldUntil = %d, want the zero it was written with", entries[0].HeldUntil)
	}

	// Moving the deadline keeps that reading: the pseudo-mute is renewed by
	// nothing, so what reaches here is a deadline moving and the hold staying
	// open, and storing the zero moment's own year instead would turn it into a
	// hold that has already ended.
	if err := opened.Pending().MoveDeadline(ctx, entry.Token,
		now.Add(96*time.Hour), time.Time{}); err != nil {
		t.Fatalf("MoveDeadline: %v", err)
	}
	entries, err = opened.Pending().Load(ctx, now.Add(365*24*time.Hour))
	if err != nil {
		t.Fatalf("Load after the update: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Load returned %d entries after the update, want the open hold",
			len(entries))
	}
	if entries[0].HeldUntil != 0 || entries[0].Deadline != now.Add(96*time.Hour).Unix() {
		t.Errorf("after the update: %+v", entries[0])
	}
}

// TestAWatchRoundTrip covers the high risk list: what is written is what is read
// back, replacing a mark replaces it rather than adding to it, and removing it says
// whether there was one.
func TestAWatchRoundTrip(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := opened.Watches().Add(ctx, Watch{
		ID:           "WATCH-1",
		MemberOpenID: "MEMBER-1",
		Reason:       "广告",
		AddedAt:      now.Unix(),
		AddedBy:      "ADMIN-1",
		ExpiresAt:    now.Add(7 * 24 * time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	entries, err := opened.Watches().List(ctx, now)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("List returned %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.MemberOpenID != "MEMBER-1" || got.Reason != "广告" || got.AddedBy != "ADMIN-1" ||
		got.ExpiresAt != now.Add(7*24*time.Hour).Unix() {
		t.Errorf("List returned %+v, want what was written", got)
	}

	// Re-marking somebody replaces their entry rather than adding a second one:
	// two rows for one member would be two answers to "until when".
	later := now.Add(30 * 24 * time.Hour)
	if err := opened.Watches().Add(ctx, Watch{
		ID:           "WATCH-2",
		MemberOpenID: "MEMBER-1",
		Reason:       "again",
		AddedAt:      now.Unix(),
		ExpiresAt:    later.Unix(),
	}); err != nil {
		t.Fatalf("Add over an existing mark: %v", err)
	}
	entries, err = opened.Watches().List(ctx, now)
	if err != nil {
		t.Fatalf("List after re-marking: %v", err)
	}
	if len(entries) != 1 || entries[0].ExpiresAt != later.Unix() {
		t.Errorf("after re-marking: %+v, want one entry ending later", entries)
	}

	// A mark that has run out is not returned: there is nothing to enforce, and
	// handing it back would invite a caller to judge somebody for ever.
	expired, err := opened.Watches().List(ctx, later.Add(time.Second))
	if err != nil {
		t.Fatalf("List after the mark ran out: %v", err)
	}
	if len(expired) != 0 {
		t.Errorf("List returned %d marks that had run out, want none", len(expired))
	}

	removed, err := opened.Watches().Remove(ctx, "MEMBER-1")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("Remove reported nothing removed although the mark was there")
	}
	// Removing what is not there is a fact, not a failure: it is what keeps a
	// command from reporting a change that did not happen.
	removed, err = opened.Watches().Remove(ctx, "MEMBER-1")
	if err != nil {
		t.Fatalf("Remove again: %v", err)
	}
	if removed {
		t.Error("Remove reported a removal for a member who was not marked")
	}
}

// TestAWatchMustEnd covers the rule there is nowhere else to put: an entry with no
// moment it ends is refused rather than read as permanent.
func TestAWatchMustEnd(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()

	err := opened.Watches().Add(ctx, Watch{
		ID:           "WATCH-1",
		MemberOpenID: "MEMBER-1",
		AddedAt:      time.Now().Unix(),
	})
	if err == nil {
		t.Fatal("a mark with no expiry was accepted")
	}
	if !strings.Contains(err.Error(), "ends") {
		t.Errorf("err = %v, want it to say what is missing", err)
	}
	entries, err := opened.Watches().List(ctx, time.Now())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the refused mark was stored anyway: %+v", entries)
	}
}

// TestCountViolationsCountsOneMember covers what the automatic mark is decided
// from: the judgements that found this member breaking a rule, across every group,
// and only those -- a judgement that never happened is not a violation.
func TestCountViolationsCountsOneMember(t *testing.T) {
	opened := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	record := func(id, group, subject, verdict string) {
		t.Helper()
		if err := opened.Judgements().Record(ctx, Judgement{
			ID:            id,
			GroupOpenID:   group,
			SubjectOpenID: subject,
			Verdict:       verdict,
			CreatedAt:     now.Unix(),
		}); err != nil {
			t.Fatalf("Record %s: %v", id, err)
		}
	}
	record("J-1", "GROUP-1", "MEMBER-1", JudgementViolation)
	record("J-2", "GROUP-2", "MEMBER-1", JudgementViolation)
	record("J-3", "GROUP-1", "MEMBER-1", JudgementOK)
	record("J-4", "GROUP-1", "MEMBER-2", JudgementViolation)

	count, err := opened.Judgements().CountViolations(ctx, "MEMBER-1", time.Time{})
	if err != nil {
		t.Fatalf("CountViolations: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want the two violations of this member in either "+
			"group", count)
	}

	// A moment is a lower bound, so a count from after the records is nothing.
	count, err = opened.Judgements().CountViolations(ctx, "MEMBER-1", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("CountViolations since later: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d since a moment after every record, want none", count)
	}

	// Nobody is not somebody who was caught.
	count, err = opened.Judgements().CountViolations(ctx, "", time.Time{})
	if err != nil {
		t.Fatalf("CountViolations with no subject: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d for no subject, want none", count)
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
