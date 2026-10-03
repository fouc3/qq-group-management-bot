package app

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
)

// reloadFor is a reloader over a running set of features, with the configuration
// in force given.
func reloadFor(t *testing.T, live *parts, before *config.Config, logged *bytes.Buffer) *reloader {
	t.Helper()
	return &reloader{
		parts:   live,
		logger:  slog.New(slog.NewTextHandler(logged, nil)),
		level:   new(slog.LevelVar),
		current: before,
	}
}

// started starts a set of features and returns the configuration they were
// started from.
func started(t *testing.T, live *parts, names ...string) *config.Config {
	t.Helper()
	cfg := configOf(t, names...)
	if err := live.startFromConfig(cfg); err != nil {
		t.Fatalf("starting: %v", err)
	}
	return cfg
}

// TestAChangedSectionRebuildsOnlyThatFeature covers the ordinary reload: one
// feature's configuration changed, so that feature is built again and no other is.
func TestAChangedSectionRebuildsOnlyThatFeature(t *testing.T) {
	live, built, _ := manager(t, "one", "two")
	before := started(t, live, "one", "two")

	fresh := configOf(t, "one", "two")
	fresh.Features["one"] = sectionFrom(t, "enabled: true\nsomething: else")
	var logged bytes.Buffer
	reloadFor(t, live, before, &logged).apply(fresh, "test")

	if len(built["one"]) != 2 {
		t.Errorf("the changed feature was built %d time(s), want 2", len(built["one"]))
	}
	if len(built["two"]) != 1 {
		t.Errorf("a feature whose configuration did not change was built again")
	}
}

// TestAFeatureThatAppearsIsStarted covers the section that was added to the file
// while the bot was running.
func TestAFeatureThatAppearsIsStarted(t *testing.T) {
	live, built, client := manager(t, "one", "two")
	before := started(t, live, "one")

	var logged bytes.Buffer
	reloadFor(t, live, before, &logged).apply(configOf(t, "one", "two"), "test")

	if len(built["two"]) != 1 {
		t.Fatalf("a feature that appeared in the configuration was not started")
	}
	message(t, client)
	if built["two"][0].(*groupFeature).answered != 1 {
		t.Error("the feature that was started does not answer")
	}
}

// TestAFeatureThatDisappearsIsStopped covers the section that was taken out, and
// the one that was turned off: both stop the feature.
func TestAFeatureThatDisappearsIsStopped(t *testing.T) {
	for why, fresh := range map[string]*config.Config{
		"removed": configOf(t, "two"),
		"turned off": {Features: map[string]yaml.Node{
			"one": sectionFrom(t, "enabled: false"),
			"two": sectionFrom(t, "enabled: true"),
		}},
	} {
		live, built, client := manager(t, "one", "two")
		before := started(t, live, "one", "two")

		var logged bytes.Buffer
		reloadFor(t, live, before, &logged).apply(fresh, "test")

		message(t, client)
		if answered := built["one"][0].(*groupFeature).answered; answered != 0 {
			t.Errorf("a feature that was %s answered %d message(s)", why, answered)
		}
		if answered := built["two"][0].(*groupFeature).answered; answered != 1 {
			t.Errorf("the feature that should still run answered %d message(s)", answered)
		}
	}
}

// TestTheLogLevelChangesWithoutRebuildingAnything covers the one setting that
// reaches the logger rather than a feature.
func TestTheLogLevelChangesWithoutRebuildingAnything(t *testing.T) {
	live, built, _ := manager(t, "one")
	before := started(t, live, "one")

	fresh := configOf(t, "one")
	fresh.Log.Level = "debug"
	var logged bytes.Buffer
	reload := reloadFor(t, live, before, &logged)
	reload.apply(fresh, "test")

	if got := reload.level.Level(); got != slog.LevelDebug {
		t.Errorf("the log level = %v, want debug", got)
	}
	if len(built["one"]) != 1 {
		t.Error("changing the log level built a feature again")
	}
}

// TestAnImmovableChangeIsRefusedAndTheRestIsApplied covers the judgement every
// section gets on its own: the database cannot be swapped while the bot runs, and
// a feature changed in the same edit still is.
//
// Refusing the whole file would make a reload useless in exactly the case it is
// for: an operator editing one feature while the credentials above it stay as they
// were.
func TestAnImmovableChangeIsRefusedAndTheRestIsApplied(t *testing.T) {
	live, built, _ := manager(t, "one")
	before := started(t, live, "one")

	fresh := configOf(t, "one")
	fresh.Features["one"] = sectionFrom(t, "enabled: true\nsomething: else")
	fresh.Database.DSN = "/somewhere/else.db"
	var logged bytes.Buffer
	reloadFor(t, live, before, &logged).apply(fresh, "test")

	if len(built["one"]) != 2 {
		t.Error("the feature that changed was not built again")
	}
	if output := logged.String(); !strings.Contains(output, "not applied") {
		t.Errorf("the change that cannot be adopted was not reported:\n%s", output)
	}
}

// TestAChangeThatReachesEveryFeatureRebuildsEveryFeature covers what the features
// hold a copy of: the group list is handed over when one is built, so one built
// before the change would keep the old one.
func TestAChangeThatReachesEveryFeatureRebuildsEveryFeature(t *testing.T) {
	live, built, _ := manager(t, "one", "two")
	before := started(t, live, "one", "two")

	fresh := configOf(t, "one", "two")
	fresh.Bot.Groups = []config.Group{{OpenID: "ANOTHER-GROUP"}}
	var logged bytes.Buffer
	reloadFor(t, live, before, &logged).apply(fresh, "test")

	for _, name := range []string{"one", "two"} {
		if len(built[name]) != 2 {
			t.Errorf("%s was built %d time(s), want 2", name, len(built[name]))
		}
	}
}

// TestTheSameFileIsNotAChange covers why a file which is looked at every three
// seconds does not rebuild anything: what is compared is what the sections say,
// not how they are written.
func TestTheSameFileIsNotAChange(t *testing.T) {
	before := configOf(t, "one")
	if !sameConfig(before, configOf(t, "one")) {
		t.Error("two identical configurations were taken for a change")
	}

	// A section that is written differently but says the same thing, which is what
	// every edit above another section looks like.
	for _, rewriting := range []string{
		"enabled:    true",
		"enabled: true # the same, with a comment",
	} {
		if !sameSection(sectionFrom(t, "enabled: true"), sectionFrom(t, rewriting)) {
			t.Errorf("%q was taken for a change", rewriting)
		}
	}

	changed := configOf(t, "one")
	changed.Features["one"] = sectionFrom(t, "enabled: true\nsomething: else")
	if sameConfig(before, changed) {
		t.Error("a changed section was taken for the same one")
	}
}

// sectionFrom decodes one configuration section, keeping what it says rather than
// where it was written.
func sectionFrom(t *testing.T, text string) yaml.Node {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(text), &document); err != nil {
		t.Fatalf("decoding the section: %v", err)
	}
	return *document.Content[0]
}

// TestOnceAdoptsAnEditedFile covers the whole path a reload takes: a file is read,
// edited, and read again.
//
// The timer and the signal are the two ways in, and both end here, so this is
// where a file that did not change has to do nothing and a file that did has to
// reach the feature it names.
func TestOnceAdoptsAnEditedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, `
bot:
  access_token: "test-token"
database:
  driver: sqlite
  dsn: "`+filepath.Join(t.TempDir(), "test.db")+`"
log:
  level: info
features:
  one:
    enabled: true
`)
	before, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	live, built, _ := manager(t, "one")
	if err := live.startFromConfig(before); err != nil {
		t.Fatalf("starting: %v", err)
	}
	var logged bytes.Buffer
	reload := reloadFor(t, live, before, &logged)
	reload.path = path

	// The same file, looked at again: nothing is built a second time.
	reload.once("test")
	if len(built["one"]) != 1 {
		t.Errorf("the same file built the feature again: %d instances",
			len(built["one"]))
	}

	// The same file with one thing changed.
	writeConfig(t, path, `
bot:
  access_token: "test-token"
database:
  driver: sqlite
  dsn: "`+filepath.Join(t.TempDir(), "test.db")+`"
log:
  level: info
features:
  one:
    enabled: true
    something: else
`)
	reload.once("test")
	if len(built["one"]) != 2 {
		t.Errorf("an edited file built the feature %d time(s), want 2",
			len(built["one"]))
	}
}

// writeConfig writes a configuration file for a test.
func writeConfig(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// TestTheWiringIsLoggedWhenItChanges is a spot check that a rebuild says what it
// did, which is the only way an operator knows what a reload amounted to.
func TestTheWiringIsLoggedWhenItChanges(t *testing.T) {
	live, _, _ := manager(t, "one")
	before := started(t, live, "one")

	fresh := configOf(t, "one")
	fresh.Features["one"] = sectionFrom(t, "enabled: true\nsomething: else")
	var logged bytes.Buffer
	reloadFor(t, live, before, &logged).apply(fresh, "test")

	output := logged.String()
	if !strings.Contains(output, "feature") || !strings.Contains(output, "built again") {
		t.Errorf("a rebuild was not reported:\n%s", output)
	}
}
