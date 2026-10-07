package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func blacklistTime(t *testing.T) (context.Context, time.Time) {
	t.Helper()
	return context.Background(), time.Now()
}

// TestBarredFindsEitherIdentity covers the lookup, which is keyed on two
// identities because an applicant may be known by one, the other, or both.
func TestBarredFindsEitherIdentity(t *testing.T) {
	opened := openTestStore(t)
	ctx, now := blacklistTime(t)

	if err := opened.Blacklist().Add(ctx, Barred{
		ID: "ENTRY-1", MemberOpenID: "MEMBER-1", UnionOpenID: "UNION-1",
		Reason: "spam", AddedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	cases := map[string][2]string{
		"by member openid": {"MEMBER-1", ""},
		"by union openid":  {"", "UNION-1"},
		"by both":          {"MEMBER-1", "UNION-1"},
	}
	for name, identity := range cases {
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
		barred, err := opened.Blacklist().Barred(ctx, "OTHER", "OTHER-UNION", now)
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

	t.Run("an entry that only names a union openid", func(t *testing.T) {
		if err := opened.Blacklist().Add(ctx, Barred{
			ID: "ENTRY-2", UnionOpenID: "UNION-2", AddedAt: now.Unix(),
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		barred, err := opened.Blacklist().Barred(ctx, "", "UNION-2", now)
		if err != nil {
			t.Fatalf("Barred: %v", err)
		}
		if !barred {
			t.Error("Barred = false for an entry that only names a union openid")
		}
	})
}

// TestSeveralEntriesMayNameOnlyAUnionOpenid covers the defect the second
// migration exists for.
//
// The first layout made both identities NOT NULL DEFAULT ”, and a unique index
// refuses to hold two empty strings: the second entry that named only a union
// openid could not be inserted at all. Somebody barred before they ever applied
// here has no member openid, so that case is not hypothetical.
func TestSeveralEntriesMayNameOnlyAUnionOpenid(t *testing.T) {
	opened := openTestStore(t)
	ctx, now := blacklistTime(t)

	for _, id := range []string{"ENTRY-1", "ENTRY-2", "ENTRY-3"} {
		if err := opened.Blacklist().Add(ctx, Barred{
			ID: id, UnionOpenID: "UNION-" + id, AddedAt: now.Unix(),
		}); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}

	entries, err := opened.Blacklist().List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("List returned %d entries, want all three", len(entries))
	}
}

// TestABlacklistEntryNeedsAnIdentity covers the refusal that keeps a row from
// being written that nothing could ever match.
func TestABlacklistEntryNeedsAnIdentity(t *testing.T) {
	opened := openTestStore(t)
	ctx, now := blacklistTime(t)

	if err := opened.Blacklist().Add(ctx, Barred{ID: "ENTRY-1", AddedAt: now.Unix()}); err == nil {
		t.Error("an entry with no identity must be refused")
	}
}

// TestAnExpiredEntryStopsBarring covers the temporary ban: the row stays, and it
// simply stops counting.
func TestAnExpiredEntryStopsBarring(t *testing.T) {
	opened := openTestStore(t)
	ctx, now := blacklistTime(t)

	if err := opened.Blacklist().Add(ctx, Barred{
		ID: "ENTRY-1", MemberOpenID: "MEMBER-1",
		AddedAt: now.Unix(), ExpiresAt: now.Add(-time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	barred, err := opened.Blacklist().Barred(ctx, "MEMBER-1", "", now)
	if err != nil {
		t.Fatalf("Barred: %v", err)
	}
	if barred {
		t.Error("Barred = true for an entry that has expired")
	}
}

// TestRemoveTakesTheIdentityAnOperatorHas covers the useful spelling: nobody
// types a row's own id, they type the openid they were handed.
func TestRemoveTakesTheIdentityAnOperatorHas(t *testing.T) {
	opened := openTestStore(t)
	ctx, now := blacklistTime(t)

	if err := opened.Blacklist().Add(ctx, Barred{
		ID: "ENTRY-1", MemberOpenID: "MEMBER-1", AddedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
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

	// An empty key would be a delete by an identity nobody has.
	if err := opened.Blacklist().Remove(ctx, "  "); err == nil {
		t.Error("an empty key must be refused")
	}
}

// TestAVersionOneDatabaseUpgrades covers the path the live deployment takes: the
// database in production is at version 1, so the second migration is the one
// that will actually run there.
func TestAVersionOneDatabaseUpgrades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version1.db")
	ctx := context.Background()

	// Built by hand at version 1, with the layout that made the identities NOT
	// NULL: the migration can then be watched doing its work.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		migrations[0],
		`INSERT INTO meta (key, value) VALUES ('schema_version', '1')`,
		`INSERT INTO join_blacklist (id, member_openid, union_openid, reason, added_at, added_by, expires_at)
		 VALUES ('ENTRY-1', '', 'UNION-1', 'from the first layout', 1, '', 0)`,
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			raw.Close()
			t.Fatalf("building a version 1 database: %v", err)
		}
	}
	raw.Close()

	opened, err := Open(ctx, Config{Driver: "sqlite", DSN: path})
	if err != nil {
		t.Fatalf("opening a version 1 database: %v", err)
	}
	defer opened.Close()

	// The row that was there survives, with its identity intact.
	now := time.Unix(1000, 0)
	barred, err := opened.Blacklist().Barred(ctx, "", "UNION-1", now)
	if err != nil {
		t.Fatalf("Barred: %v", err)
	}
	if !barred {
		t.Error("the row written by the first layout was lost in the upgrade")
	}

	// And the case the upgrade exists for now works.
	if err := opened.Blacklist().Add(ctx, Barred{
		ID: "ENTRY-2", UnionOpenID: "UNION-2", AddedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("adding a second union-only entry after the upgrade: %v", err)
	}
}
