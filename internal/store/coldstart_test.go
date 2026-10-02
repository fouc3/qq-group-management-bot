package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestANewMachineCanOpenItsDatabase covers the first run on a machine that has
// never run this bot.
//
// SQLite creates the file but not the directory, so a deployment moving to a new
// host would otherwise fail to start with nothing more helpful than "unable to
// open database file". It is also the only path that runs both migrations from
// nothing, which is what a fresh deployment does.
func TestANewMachineCanOpenItsDatabase(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state", "qq-group-management-bot")
	dsn := filepath.Join(directory, "bot.db")
	ctx := context.Background()

	opened, err := Open(ctx, Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("opening a database in a directory that does not exist yet: %v", err)
	}
	defer opened.Close()

	// Writable, not merely opened: a fresh database has to have both tables.
	if err := opened.Meta().Set(ctx, "cold_start", "yes"); err != nil {
		t.Fatalf("writing to a fresh database: %v", err)
	}
	if err := opened.Pending().Put(ctx, Pending{
		Token: "COLD-1", GroupOpenID: "GROUP-1", MemberOpenID: "MEMBER-1", HeldUntil: 1,
	}); err != nil {
		t.Fatalf("a fresh database has no pending table: %v", err)
	}
	if err := opened.Blacklist().Add(ctx, Barred{
		ID: "COLD-1", MemberOpenID: "MEMBER-1",
	}); err != nil {
		t.Fatalf("a fresh database has no blacklist table: %v", err)
	}
}
