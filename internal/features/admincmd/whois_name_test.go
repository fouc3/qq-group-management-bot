package admincmd

import (
	"testing"

	"github.com/fouc3/qq-group-management-bot/internal/command"
)

// TestTheWhoisRefusalSurvivesAnotherNameForTheCommand covers how the refusal tells whois
// apart from every other command.
//
// The branch it decides is only about which line a refusal is logged on, so comparing
// the word that was typed with the name misses in the one case that matters: an alias.
// /whois has none today, which is exactly why this is worth pinning -- the day it gains
// one, a member refused for typing it would get the generic line, and nothing would fail
// to say so.
//
// The table here is built in the test rather than taken from the feature, so the alias
// does not have to exist in production for the rule to be checked.
func TestTheWhoisRefusalSurvivesAnotherNameForTheCommand(t *testing.T) {
	const alias = "whoami"

	catalog, err := command.NewCatalog([]command.Def{
		{Name: whoisCommand, Aliases: []string{alias}},
		{Name: "禁言"},
	})
	if err != nil {
		t.Fatalf("building the table: %v", err)
	}

	for _, written := range []string{whoisCommand, alias} {
		if !writtenIsWhois(catalog, written) {
			t.Errorf("%q is a name for the whois command, but a refusal for it would "+
				"be logged as an ordinary refusal: asked of the table, every name the "+
				"command answers to is the same command", written)
		}
	}
	if writtenIsWhois(catalog, "禁言") {
		t.Error("a refusal for /禁言 would be logged as the whois refusal, which " +
			"says the wrong thing about what was refused")
	}
}
