package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	// Registers the "sqlite" driver. The pure-Go implementation is deliberate: the
	// deployment builds a static binary with go build and runs it under systemd,
	// and cgo would mean a compiler on every machine that builds or runs it.
	_ "modernc.org/sqlite"
)

func init() { openSQLite = openSQLiteStore }

// openSQLiteStore opens a SQLite file and sets what the driver needs.
func openSQLiteStore(ctx context.Context, path string, connections int) (*sqlStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("store: sqlite needs a file path")
	}
	// SQLite creates the file but not the directory, and a machine that has never
	// run this bot has no state directory yet. Doing it here means every caller
	// gets it, rather than each one remembering.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// One connection: SQLite has a single writer, and a larger pool only moves
	// the contention into the driver, where it appears as SQLITE_BUSY rather than
	// as something a person can read.
	db.SetMaxOpenConns(connections)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// journal_mode is written into the file, so it survives reconnection;
	// busy_timeout is per connection, which is part of why the pool is one.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("applying %q: %w", pragma, err)
		}
	}
	return &sqlStore{db: db, dialect: dialect{name: "sqlite"}}, nil
}
