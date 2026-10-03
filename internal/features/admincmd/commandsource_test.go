package admincmd

import (
	"context"
	"strings"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
)

// offering is a command another feature contributes to the table, with a runner
// that writes down that it ran.
//
// It stands in for a feature that answers commands of its own, which is what
// makes one table shared rather than one table per feature.
func offering(name string, ran *[]string, said string) command.Def {
	return command.Def{
		Name:     name,
		Usage:    "{prefix}" + name + " —— 别的功能提供的",
		Desc:     "别的功能",
		Audience: command.Everyone,
		Panels:   []command.PanelPlacement{{Scene: command.InGroup}},
		Run: func(_ context.Context, _ *qqbotsdk.GroupMessageCreateData,
			_ command.Parsed) error {
			*ran = append(*ran, said)
			return nil
		},
	}
}

// TestACommandOfferedByAnotherFeatureIsAnswered covers what one table means: a
// feature that contributes commands is answered, listed in the help, and offered in
// the menu, without the table's owner knowing anything about it.
func TestACommandOfferedByAnotherFeatureIsAnswered(t *testing.T) {
	h := newHarnessWithRegistering(t, baseSection)
	var ran []string
	if err := h.handler.SetCommandSources([]command.Def{
		offering("广播", &ran, "the offered command ran"),
	}); err != nil {
		t.Fatalf("taking the commands: %v", err)
	}

	if !strings.Contains(h.handler.usageText(), "/广播") {
		t.Error("the help does not list the command another feature offered")
	}
	// The menu is published from the table, so a command that arrives with a
	// source reaches it without the table's owner being built again.
	h.waitForPanelEntry(t, "group", "/广播")

	h.send("/广播", testAdmin, testGroupOpenID)
	if len(ran) != 1 {
		t.Fatalf("the offered command ran %d time(s), want 1", len(ran))
	}
}

// TestTwoFeaturesCannotShareAWord covers the mistake one table has to refuse, and
// where it has to be refused: a word that invokes two commands leaves one of them
// reachable by nothing, and which one is decided by the order the features happen
// to be registered in.
func TestTwoFeaturesCannotShareAWord(t *testing.T) {
	h := newHarness(t, baseSection)

	err := h.handler.SetCommandSources([]command.Def{
		{Name: "禁言", Usage: "{prefix}禁言", Audience: command.Everyone},
	})
	if err == nil {
		t.Fatal("a word that invokes two commands was accepted")
	}
	if !strings.Contains(err.Error(), "禁言") {
		t.Errorf("the refusal does not name the word: %v", err)
	}

	// Refused means refused: what was in the table before is still there, so a
	// reload that would break the table leaves the working one alone.
	if _, ok := h.handler.commands().Lookup("禁言"); !ok {
		t.Error("a refused wiring took the existing command away")
	}
}

// TestASourceBuiltAgainIsAnsweredByTheNewInstance covers what makes a contributed
// command survive a reload of the feature that offers it.
//
// The second wiring hands over the runners of the instance that is running now.
// A table that merged, or that was built once and kept, would go on answering from
// the instance that was replaced -- in a group, visibly: the same command answering
// from behind a feature that has stopped.
func TestASourceBuiltAgainIsAnsweredByTheNewInstance(t *testing.T) {
	h := newHarness(t, baseSection)
	var ran []string

	if err := h.handler.SetCommandSources([]command.Def{
		offering("广播", &ran, "the first instance"),
	}); err != nil {
		t.Fatalf("the first wiring: %v", err)
	}
	h.send("/广播", testAdmin, testGroupOpenID)

	if err := h.handler.SetCommandSources([]command.Def{
		offering("广播", &ran, "the second instance"),
	}); err != nil {
		t.Fatalf("the second wiring: %v", err)
	}
	h.send("/广播", testAdmin, testGroupOpenID)

	if len(ran) != 2 {
		t.Fatalf("the command ran %d time(s), want once per wiring", len(ran))
	}
	if ran[0] != "the first instance" || ran[1] != "the second instance" {
		t.Errorf("the command ran as %q, want the instance of each wiring", ran)
	}
}

// TestASourceThatStopsOfferingIsGone covers the other direction: a feature that
// was switched off stops contributing, and the table stops answering its command.
func TestASourceThatStopsOfferingIsGone(t *testing.T) {
	h := newHarness(t, baseSection)
	var ran []string
	if err := h.handler.SetCommandSources([]command.Def{
		offering("广播", &ran, "ran"),
	}); err != nil {
		t.Fatalf("wiring: %v", err)
	}

	if err := h.handler.SetCommandSources(nil); err != nil {
		t.Fatalf("wiring with nothing offered: %v", err)
	}
	if _, ok := h.handler.commands().Lookup("广播"); ok {
		t.Error("a command is still in the table after its feature stopped offering it")
	}

	h.send("/广播", testAdmin, testGroupOpenID)
	if len(ran) != 0 {
		t.Errorf("a command whose feature stopped offering it still ran: %v", ran)
	}
}
