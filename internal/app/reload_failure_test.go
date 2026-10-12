package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// partsWithOneFeatureThatCannotBeBuiltAgain returns a set of features whose named one
// is built once and refuses afterwards.
//
// That is what a wide change finds when one section was edited into something that
// cannot be built: the build at startup worked, the one the change asks for does not.
func partsWithOneFeatureThatCannotBeBuiltAgain(t *testing.T, refuses string,
	names ...string) *parts {
	t.Helper()
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	database, err := store.Open(ctx, store.Config{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "test.db"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	client, err := qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AccessToken: "test-token",
		BaseURL:     "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}

	attempts := 0
	registry := feature.NewRegistry()
	for _, name := range names {
		registry.Add(name, func(_ yaml.Node, deps feature.Deps) (feature.Feature, error) {
			if name == refuses {
				attempts++
				if attempts > 1 {
					return nil, errBrokenSection
				}
			}
			return &groupFeature{name: name, deps: deps}, nil
		})
	}
	return newParts(registry, feature.Deps{
		Client: client,
		Logger: quiet,
		Store:  database,
	}, ctx, quiet)
}

// TestAWideChangeThatCannotBeBuiltLeavesEveryFeatureRunning covers what a change to
// the group list, the bot's own QQ number, the cache address or the fallback client
// has to do when one section cannot be built.
//
// Rebuilding every feature must not cost the running ones. A change that cannot be
// completed is a change that did not happen, and until it is recorded as such the bot
// has to go on serving from what it had -- otherwise one edited file takes every
// feature out of service at once, which for a bot that answers commands and judges
// messages means it is running and doing nothing at all.
func TestAWideChangeThatCannotBeBuiltLeavesEveryFeatureRunning(t *testing.T) {
	live := partsWithOneFeatureThatCannotBeBuiltAgain(t, "beta", "alpha", "beta")
	if err := live.startFromConfig(configOf(t, "alpha", "beta")); err != nil {
		t.Fatalf("starting the features: %v", err)
	}

	if err := live.restartEverything(configOf(t, "alpha", "beta")); err == nil {
		t.Fatal("a wide change whose feature cannot be built reported success")
	}

	for _, name := range []string{"alpha", "beta"} {
		if !live.running(name) {
			t.Errorf("%s is no longer running: a wide change that cannot be completed "+
				"stopped every feature and put none of them back, so the bot serves "+
				"nothing at all until the file is edited again", name)
		}
	}
}

// TestAWideChangeThatFailedIsNotRecordedAsApplied covers the other half of the same
// failure.
//
// What a reload measures a change against has to be what is actually in force. A
// failure recorded as applied is never looked at again: the next look at the same file
// sees nothing that changed, so the edit is dropped silently until the operator
// happens to touch the file a second time -- and the log that said the rebuild failed
// scrolls away long before that.
func TestAWideChangeThatFailedIsNotRecordedAsApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, `
bot:
  access_token: "test-token"
  groups: []
database:
  driver: sqlite
  dsn: "`+filepath.Join(t.TempDir(), "test.db")+`"
log:
  level: info
features:
  alpha:
    enabled: true
  beta:
    enabled: true
`)
	before, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading the first file: %v", err)
	}

	live := partsWithOneFeatureThatCannotBeBuiltAgain(t, "beta", "alpha", "beta")
	if err := live.startFromConfig(before); err != nil {
		t.Fatalf("starting the features: %v", err)
	}

	var logged bytes.Buffer
	reload := reloadFor(t, live, before, &logged)
	reload.path = path

	// The group list changes, which reaches every feature, and the rebuild cannot be
	// done.
	writeConfig(t, path, `
bot:
  access_token: "test-token"
  groups:
    - openid: "GROUP-NEW"
database:
  driver: sqlite
  dsn: "`+filepath.Join(t.TempDir(), "test.db")+`"
log:
  level: info
features:
  alpha:
    enabled: true
  beta:
    enabled: true
`)
	reload.once("test")

	if groups := len(reload.current.Bot.Groups); groups != 0 {
		t.Errorf("the failed wide change was recorded as applied: the configuration "+
			"in force names %d group(s), want the 0 it had before. The next look at "+
			"the same file sees no change, so the edit is dropped for good and only "+
			"a second edit would bring it back", groups)
	}
}
