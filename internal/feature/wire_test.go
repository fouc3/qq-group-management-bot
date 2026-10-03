package feature

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// greeter is a capability one feature provides and another wants, standing in
// for the real ones (verification, moderation, the administrator list).
type greeter interface {
	Greeting() string
}

// greeterAware is what a feature that wants it implements.
type greeterAware interface {
	SetGreeter(greeter)
}

// providing is a feature that provides the capability.
type providing struct {
	stub
	greeting string
}

func (p *providing) Greeting() string { return p.greeting }

// wanting is a feature that takes it, and writes down what it was handed.
type wanting struct {
	stub
	got greeter
}

func (w *wanting) SetGreeter(g greeter) { w.got = g }

// TestTwoProvidersAreRefused covers the mistake the lookup refuses.
//
// Resolving it instead would hand the consumers whichever provider happened to
// be registered first, and nothing would say so.
func TestTwoProvidersAreRefused(t *testing.T) {
	features := []Feature{
		&providing{stub: stub{name: "first"}, greeting: "one"},
		&providing{stub: stub{name: "second"}, greeting: "two"},
	}
	_, _, err := FindProvider[greeter](features, "a greeting")
	if err == nil {
		t.Fatal("two providers of one capability were accepted")
	}
	for _, name := range []string{"first", "second"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %q: %v", name, err)
		}
	}
}

// TestNothingProvidesIsNotAnError covers a deployment rather than a mistake: a
// bot without a judge is a bot whose report command says so, and the consumer has
// to be told it got nothing instead of never being called.
func TestNothingProvidesIsNotAnError(t *testing.T) {
	consumer := &wanting{stub: stub{name: "consumer"}}
	var logged bytes.Buffer

	provider, providedBy, err := FindProvider[greeter]([]Feature{consumer}, "a greeting")
	if err != nil {
		t.Fatalf("a capability nobody provides was refused: %v", err)
	}
	if provider != nil || providedBy != "" {
		t.Errorf("provider = %v, providedBy = %q; want nothing", provider, providedBy)
	}

	Inject[greeter, greeterAware]([]Feature{consumer}, provider, providedBy, "a greeting",
		slog.New(slog.NewJSONHandler(&logged, nil)), greeterAware.SetGreeter)
	if consumer.got != nil {
		t.Errorf("a consumer was handed %v, which nothing provided", consumer.got)
	}
	// Said out loud as well: a capability that is provided by nobody and wanted
	// by somebody is the difference between a switched-off feature and a mistake,
	// and only this line tells them apart.
	if line := logged.String(); !strings.Contains(line, "nobody provides") {
		t.Errorf("a capability nobody provides was not reported: %s", line)
	}
}

// TestEveryFeatureThatWantsItGetsIt covers the wiring itself, and that a feature
// which does not implement the consumer interface is left alone.
func TestEveryFeatureThatWantsItGetsIt(t *testing.T) {
	giver := &providing{stub: stub{name: "giver"}, greeting: "hello"}
	first := &wanting{stub: stub{name: "first"}}
	second := &wanting{stub: stub{name: "second"}}
	indifferent := &stub{name: "indifferent"}
	features := []Feature{giver, first, second, indifferent}

	provider, providedBy, err := FindProvider[greeter](features, "a greeting")
	if err != nil {
		t.Fatalf("finding the provider: %v", err)
	}
	if providedBy != "giver" {
		t.Errorf("provided by %q, want giver", providedBy)
	}

	Inject[greeter, greeterAware](features, provider, providedBy, "a greeting", nil,
		greeterAware.SetGreeter)

	for _, consumer := range []*wanting{first, second} {
		if consumer.got == nil {
			t.Fatalf("%s was not handed the capability", consumer.Name())
		}
		if consumer.got.Greeting() != "hello" {
			t.Errorf("%s got %q, want hello", consumer.Name(), consumer.got.Greeting())
		}
	}
}

// TestTheWiringIsLogged covers the line that makes a wiring answerable: without
// it, a feature that forgot to take a capability is indistinguishable from one
// that does not want it.
func TestTheWiringIsLogged(t *testing.T) {
	giver := &providing{stub: stub{name: "giver"}, greeting: "hello"}
	consumer := &wanting{stub: stub{name: "consumer"}}
	features := []Feature{giver, consumer}

	provider, providedBy, err := FindProvider[greeter](features, "a greeting")
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	Inject[greeter, greeterAware](features, provider, providedBy, "a greeting",
		slog.New(slog.NewJSONHandler(&logged, nil)), greeterAware.SetGreeter)

	line := logged.String()
	for _, want := range []string{
		`"capability":"a greeting"`,
		`"provider":"giver"`,
		`"consumers":"consumer"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the wiring log does not say %s: %s", want, line)
		}
	}
}

// TestAWiringNothingWantsIsWarnedAbout covers the other half of the same idea: a
// capability with a provider and no consumer is a feature that is not running, or
// an interface nobody implements any more. Both are worth saying out loud.
func TestAWiringNothingWantsIsWarnedAbout(t *testing.T) {
	giver := &providing{stub: stub{name: "giver"}, greeting: "hello"}
	features := []Feature{giver}

	provider, providedBy, err := FindProvider[greeter](features, "a greeting")
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	Inject[greeter, greeterAware](features, provider, providedBy, "a greeting",
		slog.New(slog.NewJSONHandler(&logged, nil)), greeterAware.SetGreeter)

	line := logged.String()
	if !strings.Contains(line, `"level":"WARN"`) {
		t.Errorf("a capability nothing wants was not warned about: %s", line)
	}
}
