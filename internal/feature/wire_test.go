package feature

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/fouc3/qq-group-management-bot/internal/command"
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

// offering is a feature with commands of its own, the way a feature that answers
// commands is one.
type offering struct {
	stub
	defs []command.Def
}

func (o *offering) CommandDefs() []command.Def { return o.defs }

// keeping is the feature that keeps the table, and the only wiring whose consumer
// can refuse what it is handed.
type keeping struct {
	stub
	// handed is what it has now, and calls is how many times it was wired: the
	// table has to be replaced on every wiring rather than extended, or a source
	// built again would leave its old commands answering.
	handed []command.Def
	calls  int
	refuse error
}

func (k *keeping) SetCommandSources(defs []command.Def) error {
	k.calls++
	if k.refuse != nil {
		return k.refuse
	}
	k.handed = defs
	return nil
}

// aCommand is a definition that takes no arguments, so a test can tell two of
// them apart by name.
func aCommand(name string, run command.Runner) command.Def {
	return command.Def{Name: name, Usage: "{prefix}" + name, Audience: command.Everyone, Run: run}
}

// TestTheCommandsOfEveryFeatureAreGathered covers the plural wiring: several
// features add to one table, and a feature that has none is left alone.
func TestTheCommandsOfEveryFeatureAreGathered(t *testing.T) {
	first := &offering{stub: stub{name: "first"}, defs: []command.Def{aCommand("one", nil)}}
	second := &offering{stub: stub{name: "second"}, defs: []command.Def{
		aCommand("two", nil), aCommand("three", nil),
	}}
	indifferent := &stub{name: "indifferent"}
	table := &keeping{stub: stub{name: "table"}}

	if err := InjectCommandSources([]Feature{first, second, indifferent, table}, nil); err != nil {
		t.Fatalf("wiring: %v", err)
	}

	var names []string
	for _, def := range table.handed {
		names = append(names, def.Name)
	}
	if got, want := strings.Join(names, " "), "one two three"; got != want {
		t.Errorf("the table was handed %q, want %q in registration order", got, want)
	}
}

// TestWhatTheSourcesOfferIsTheWholeTruth covers why the wiring is not an
// addition: a feature built again offers new runners, and a table that merged
// would keep the old ones answering.
func TestWhatTheSourcesOfferIsTheWholeTruth(t *testing.T) {
	first := &offering{stub: stub{name: "first"}, defs: []command.Def{aCommand("one", nil)}}
	table := &keeping{stub: stub{name: "table"}}
	features := []Feature{first, table}

	if err := InjectCommandSources(features, nil); err != nil {
		t.Fatalf("wiring: %v", err)
	}
	if len(table.handed) != 1 {
		t.Fatalf("the table was handed %d commands, want 1", len(table.handed))
	}

	// The source is built again and now offers something else -- and then stops
	// being a source at all, which is what a feature that lost its commands does.
	first.defs = []command.Def{aCommand("two", nil)}
	if err := InjectCommandSources(features, nil); err != nil {
		t.Fatalf("wiring again: %v", err)
	}
	if len(table.handed) != 1 || table.handed[0].Name != "two" {
		t.Errorf("the table kept %+v, want the second wiring's commands only", table.handed)
	}

	first.defs = nil
	if err := InjectCommandSources(features, nil); err != nil {
		t.Fatalf("wiring with nothing offered: %v", err)
	}
	if len(table.handed) != 0 {
		t.Errorf("a source that offers nothing left %+v behind", table.handed)
	}
	if table.calls != 3 {
		t.Errorf("the table was wired %d times, want every one of the three", table.calls)
	}
}

// TestATableThatRefusesIsReported covers the one wiring that can fail: the table
// is the only place that can see two commands answering to one word.
func TestATableThatRefusesIsReported(t *testing.T) {
	first := &offering{stub: stub{name: "first"}, defs: []command.Def{aCommand("one", nil)}}
	table := &keeping{stub: stub{name: "table"}, refuse: errors.New("one word, two commands")}

	err := InjectCommandSources([]Feature{first, table}, nil)
	if err == nil {
		t.Fatal("a table that refuses what it was handed was not reported")
	}
	if !strings.Contains(err.Error(), "table") {
		t.Errorf("the refusal does not name who refused: %v", err)
	}
}

// TestCommandsNobodyKeepsATableForAreWarnedAbout covers a wiring mistake that
// would otherwise be silent: a feature offering commands while nothing is running
// that keeps a table, so they are answered by nobody.
func TestCommandsNobodyKeepsATableForAreWarnedAbout(t *testing.T) {
	offering := &offering{stub: stub{name: "offering"}, defs: []command.Def{aCommand("one", nil)}}
	var logged bytes.Buffer

	err := InjectCommandSources([]Feature{offering},
		slog.New(slog.NewJSONHandler(&logged, nil)))
	if err != nil {
		t.Fatalf("wiring: %v", err)
	}
	line := logged.String()
	if !strings.Contains(line, `"level":"WARN"`) {
		t.Errorf("commands nobody kept a table for were not warned about: %s", line)
	}
}

// TestNothingOfferedIsNotAnError covers a bot whose only commands are its own.
func TestNothingOfferedIsNotAnError(t *testing.T) {
	table := &keeping{stub: stub{name: "table"}}
	var logged bytes.Buffer

	if err := InjectCommandSources([]Feature{table},
		slog.New(slog.NewJSONHandler(&logged, nil))); err != nil {
		t.Fatalf("wiring with no sources: %v", err)
	}
	if len(table.handed) != 0 {
		t.Errorf("the table was handed %+v by nobody", table.handed)
	}
	if line := logged.String(); strings.Contains(line, `"level":"WARN"`) {
		t.Errorf("a bot with only its own commands was warned about: %s", line)
	}
}
