package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheExampleConfigurationIsStructurallySound covers the file a new
// deployment copies.
//
// It cannot be loaded outright: it carries placeholder credentials on purpose,
// and a document that shipped working ones would be worse than useless. What can
// be checked is that it gets *as far as* complaining about those credentials,
// because the parse, the strict unknown-key check, the duplicate-key check and
// the comment expansion all run before credential validation does.
//
// That is exactly how the two defects this project already shipped through the
// example were shaped -- a key written twice, and a ${...} inside a comment that
// was expanded anyway into a duplicate -- so this is the failure mode worth
// holding onto.
func TestTheExampleConfigurationIsStructurallySound(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")

	_, err := Load(path)
	if err == nil {
		t.Log("the example now loads outright, which means its credentials are real")
	} else if !strings.Contains(err.Error(), "access_token") {
		t.Fatalf("the example failed for a reason other than its placeholder "+
			"credentials, so something in its structure is wrong: %v", err)
	}

	// The section a switch to PostgreSQL is made in has to be there to be found.
	// A text check is the only one available for a file that deliberately cannot
	// be loaded.
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the example: %v", err)
	}
	for _, section := range []string{"database:", "driver:", "max_open_conns:"} {
		if !strings.Contains(string(payload), section) {
			t.Errorf("the example does not document %q", section)
		}
	}
}

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
