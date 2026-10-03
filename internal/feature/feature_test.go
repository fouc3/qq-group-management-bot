package feature

import (
	"context"
	"io"
	"log/slog"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/config"
)

// stub is a feature that only writes down what it was built with.
type stub struct {
	name string
	// buttons is the registry this feature was handed, kept so that a test can
	// compare it with its neighbour's.
	buttons *command.Buttons
}

func (s *stub) Name() string                   { return s.name }
func (s *stub) Intents() qqbotsdk.Intent       { return 0 }
func (s *stub) Register(context.Context) error { return nil }
func (s *stub) Close(context.Context) error    { return nil }

// section is one feature's configuration section.
func section(t *testing.T) yaml.Node {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal([]byte("enabled: true"), &document); err != nil {
		t.Fatalf("decoding the section: %v", err)
	}
	return *document.Content[0]
}

// TestTheFeaturesOfOneBotShareTheButtons covers the invariant behind routing a
// press to its owner.
//
// A bot has one connection, so the features it runs have to claim their buttons
// in one registry: a feature that made its own would declare a dispatcher of its
// own, and every press would be looked at once per feature -- which is the state
// the registry exists to leave behind.
func TestTheFeaturesOfOneBotShareTheButtons(t *testing.T) {
	built := map[string]*stub{}
	registry := NewRegistry()
	for _, name := range []string{"first", "second"} {
		registry.Add(name, func(_ yaml.Node, deps Deps) (Feature, error) {
			instance := &stub{name: name, buttons: deps.Buttons}
			built[name] = instance
			return instance, nil
		})
	}

	node := section(t)
	cfg := &config.Config{Features: map[string]yaml.Node{"first": node, "second": node}}
	features, err := registry.Build(cfg, Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	if len(features) != 2 {
		t.Fatalf("built %d features, want 2", len(features))
	}

	first, second := built["first"].buttons, built["second"].buttons
	if first == nil || second == nil {
		t.Fatal("a feature was built with no registry to claim its buttons in")
	}
	if first != second {
		t.Error("the features of one bot were given two registries, so one press " +
			"would be routed twice")
	}
}
