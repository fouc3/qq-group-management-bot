package config

import (
	"path/filepath"
	"testing"
)

// TestDatabaseDefaults covers the section a deployment may leave out entirely:
// it still has to end up with a driver and with a place to put the file.
func TestDatabaseDefaults(t *testing.T) {
	if DefaultDatabaseDriver != "sqlite" {
		t.Errorf("DefaultDatabaseDriver = %q, want sqlite", DefaultDatabaseDriver)
	}

	path, err := DefaultDatabasePath()
	if err != nil {
		t.Fatalf("DefaultDatabasePath: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("DefaultDatabasePath = %q, want an absolute path", path)
	}
	if filepath.Base(path) != "bot.db" {
		t.Errorf("DefaultDatabasePath = %q, want it to end in bot.db", path)
	}

	// The state directory decides the location, so it is set explicitly rather
	// than read from whatever the machine running the test happens to use.
	t.Setenv("XDG_STATE_HOME", "/tmp/state-home")
	path, err = DefaultDatabasePath()
	if err != nil {
		t.Fatalf("DefaultDatabasePath: %v", err)
	}
	if want := "/tmp/state-home/qq-group-management-bot/bot.db"; path != want {
		t.Errorf("DefaultDatabasePath = %q, want %q", path, want)
	}
}

// TestTheDatabaseSectionIsValidated covers the two ways a file can get it wrong,
// so a typo is refused at startup rather than when the first member joins.
func TestTheDatabaseSectionIsValidated(t *testing.T) {
	cases := map[string]Database{
		"an unknown driver": {Driver: "mysql"},
		"a negative pool":   {Driver: "sqlite", MaxOpenConns: -1},
	}
	for name, section := range cases {
		t.Run(name, func(t *testing.T) {
			parsed := Config{Database: section}
			if err := parsed.applyDefaults(); err == nil {
				t.Error("the section must be refused")
			}
		})
	}
}
