package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// errBrokenSection stands for a section that cannot be built.
var errBrokenSection = errors.New("test: this section cannot be built")

// groupFeature is a feature that answers group messages and records what it was
// built with, which is what a test needs to see which instance is doing the work.
type groupFeature struct {
	name     string
	deps     feature.Deps
	router   *command.Router
	answered int
}

func (f *groupFeature) Name() string             { return f.name }
func (f *groupFeature) Intents() qqbotsdk.Intent { return 0 }

func (f *groupFeature) Register(context.Context) error {
	f.router = command.NewRouter(f.name, f.deps.Buttons)
	return f.router.Register(f.deps.Client, command.Handlers{
		Group: func(context.Context, *qqbotsdk.Event) error {
			f.answered++
			return nil
		},
	})
}

func (f *groupFeature) Close(context.Context) error {
	f.router.Stop()
	return nil
}

// adminsProvider is a feature that provides the administrator list, which is what
// one feature promises another.
type adminsProvider struct{ groupFeature }

func (f *adminsProvider) IsAdmin(string, string) bool { return false }

// adminsTaker is a feature that wants it, and writes down what it was handed.
type adminsTaker struct {
	groupFeature
	admins feature.AdminDirectory
}

func (f *adminsTaker) SetAdminDirectory(directory feature.AdminDirectory) {
	f.admins = directory
}

// manager builds a parts manager over a real store and client, the way Run does,
// and hands back every instance its factories made.
func manager(t *testing.T, names ...string) (*parts, map[string][]feature.Feature, *qqbotsdk.Client) {
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

	built := map[string][]feature.Feature{}
	registry := feature.NewRegistry()
	for _, name := range names {
		registry.Add(name, func(_ yaml.Node, deps feature.Deps) (feature.Feature, error) {
			base := groupFeature{name: name, deps: deps}
			var instance feature.Feature
			switch name {
			case "provider":
				instance = &adminsProvider{base}
			case "taker":
				instance = &adminsTaker{groupFeature: base}
			default:
				instance = &base
			}
			built[name] = append(built[name], instance)
			return instance, nil
		})
	}

	live := newParts(registry, feature.Deps{
		Client: client,
		Logger: quiet,
		Store:  database,
	}, ctx, quiet)
	return live, built, client
}

// section is one feature's configuration section.
func section(t *testing.T) yaml.Node {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal([]byte("enabled: true"), &document); err != nil {
		t.Fatalf("decoding the section: %v", err)
	}
	return *document.Content[0]
}

// configOf is a configuration with these features turned on.
func configOf(t *testing.T, names ...string) *config.Config {
	t.Helper()
	cfg := &config.Config{Features: map[string]yaml.Node{}}
	for _, name := range names {
		cfg.Features[name] = section(t)
	}
	return cfg
}

// message hands one group message to the client's dispatcher.
func message(t *testing.T, client *qqbotsdk.Client) {
	t.Helper()
	body := `{"id": "MESSAGE-1", "group_openid": "GROUP-OPENID",
		"author": {"member_openid": "MEMBER-OPENID"}, "content": "hello"}`
	payload := &qqbotsdk.Payload{
		ID:   "EVENT-ID",
		Op:   qqbotsdk.OpDispatch,
		Type: qqbotsdk.EventGroupMessageCreate,
		Data: json.RawMessage(body),
	}
	if err := client.Dispatcher().DispatchSync(context.Background(),
		qqbotsdk.NewEvent(payload, "test")); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
}

// TestTheFeaturesOfOneBotShareTheButtons covers the invariant behind routing a
// press to its owner.
//
// A bot has one connection, so the features it runs have to claim their buttons in
// one registry: a feature that made its own would declare a dispatcher of its own,
// and every press would be looked at once per feature.
func TestTheFeaturesOfOneBotShareTheButtons(t *testing.T) {
	live, built, _ := manager(t, "first", "second")
	if err := live.startFromConfig(configOf(t, "first", "second")); err != nil {
		t.Fatalf("starting: %v", err)
	}

	first := built["first"][0].(*groupFeature).deps.Buttons
	second := built["second"][0].(*groupFeature).deps.Buttons
	if first == nil || second == nil {
		t.Fatal("a feature was built with no registry to claim its buttons in")
	}
	if first != second {
		t.Error("the features of one bot were given two registries, so a press " +
			"would be routed twice")
	}
}

// TestARestartLeavesOneInstanceAnswering covers what makes a reload possible: the
// instance that stopped stops answering, and the one that replaced it is the only
// one left.
func TestARestartLeavesOneInstanceAnswering(t *testing.T) {
	live, built, client := manager(t, "one")
	if err := live.startFromConfig(configOf(t, "one")); err != nil {
		t.Fatalf("starting: %v", err)
	}

	message(t, client)
	first := built["one"][0].(*groupFeature)
	if first.answered != 1 {
		t.Fatalf("the first instance answered %d message(s), want 1", first.answered)
	}

	if err := live.restart("one", section(t)); err != nil {
		t.Fatalf("restarting: %v", err)
	}
	if len(built["one"]) != 2 {
		t.Fatalf("built %d instances, want 2", len(built["one"]))
	}
	second := built["one"][1].(*groupFeature)

	message(t, client)
	if first.answered != 1 {
		t.Errorf("the instance that was replaced answered a later message")
	}
	if second.answered != 1 {
		t.Errorf("the instance that replaced it answered %d message(s), want 1",
			second.answered)
	}
}

// TestARestartKeepsTheOldInstanceWhenTheNewOneCannotBeBuilt covers the failure
// that matters: a typo in a configuration file must not take a feature away from
// the groups it serves.
func TestARestartKeepsTheOldInstanceWhenTheNewOneCannotBeBuilt(t *testing.T) {
	live, built, client := manager(t, "one")
	if err := live.startFromConfig(configOf(t, "one")); err != nil {
		t.Fatalf("starting: %v", err)
	}

	live.registry.Add("broken", func(yaml.Node, feature.Deps) (feature.Feature, error) {
		return nil, errBrokenSection
	})
	if err := live.restart("broken", section(t)); err == nil {
		t.Fatal("a feature that cannot be built was accepted")
	}

	message(t, client)
	if first := built["one"][0].(*groupFeature); first.answered != 1 {
		t.Errorf("the running feature answered %d message(s), want 1", first.answered)
	}
}

// TestAWiringFollowsWhoIsRunning covers what a stopped provider must do to its
// consumers: they have to be told it is gone, rather than left holding an instance
// nobody is running.
func TestAWiringFollowsWhoIsRunning(t *testing.T) {
	live, built, _ := manager(t, "provider", "taker")
	if err := live.startFromConfig(configOf(t, "provider", "taker")); err != nil {
		t.Fatalf("starting: %v", err)
	}

	taker := built["taker"][0].(*adminsTaker)
	if taker.admins == nil {
		t.Fatal("the taker was not handed the administrator list")
	}

	if err := live.stop("provider"); err != nil {
		t.Fatalf("stopping: %v", err)
	}
	if taker.admins != nil {
		t.Error("a consumer still holds the provider that stopped")
	}
}

// TestStoppingEverythingLeavesNobodyAnswering covers shutdown, and the state a
// rebuild after it would find.
func TestStoppingEverythingLeavesNobodyAnswering(t *testing.T) {
	live, built, client := manager(t, "one", "two")
	if err := live.startFromConfig(configOf(t, "one", "two")); err != nil {
		t.Fatalf("starting: %v", err)
	}

	if err := live.stopAll(); err != nil {
		t.Fatalf("stopping everything: %v", err)
	}
	message(t, client)

	for _, name := range []string{"one", "two"} {
		instance := built[name][0].(*groupFeature)
		if instance.answered != 0 {
			t.Errorf("%s answered %d message(s) after everything stopped",
				name, instance.answered)
		}
	}
}

// TestStartFromConfigRefusesANameNobodyRegistered covers the typo that would
// otherwise leave an operator believing a feature runs.
func TestStartFromConfigRefusesANameNobodyRegistered(t *testing.T) {
	live, _, _ := manager(t, "one")
	cfg := &config.Config{Features: map[string]yaml.Node{"no-such-feature": section(t)}}
	if err := live.startFromConfig(cfg); err == nil {
		t.Error("a section naming a feature nobody registered was accepted")
	}
}
