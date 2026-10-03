package command

import (
	"reflect"
	"testing"
)

// table is a small catalog that exercises every way a definition is shown.
func table(t *testing.T) *Catalog {
	t.Helper()
	catalog, err := NewCatalog([]Def{
		{Name: "菜单", Aliases: []string{"menu", "help"}, Usage: "{prefix}菜单", Desc: "显示可用命令",
			Audience: Everyone, PrivateUsage: "{prefix}菜单"},
		{Name: "禁言", Aliases: []string{"mute"}, Usage: "{prefix}禁言 <时长>", Desc: "禁言成员",
			Audience: Admins},
		{Name: "违规举报", Usage: "{prefix}违规举报", Desc: "引用消息举报", Audience: Everyone,
			Available: func() bool { return false }},
		{Name: "debug", Usage: "{prefix}debug", Desc: "调试用", Audience: Admins,
			Unregistered: true},
	})
	if err != nil {
		t.Fatalf("building the table: %v", err)
	}
	return catalog
}

// TestEveryWordReachesItsCommand covers the point of the index: a word written
// in a definition has to be how the command is invoked, or a member typing what
// the bot itself told them gets nothing.
func TestEveryWordReachesItsCommand(t *testing.T) {
	catalog := table(t)
	for _, def := range catalog.Definitions() {
		for _, word := range def.words() {
			found, ok := catalog.Lookup(word)
			if !ok {
				t.Errorf("%q does not reach any command", word)
				continue
			}
			if found.Name != def.Name {
				t.Errorf("%q reaches %q, not %q", word, found.Name, def.Name)
			}
		}
	}
	if _, ok := catalog.Lookup("no-such-command"); ok {
		t.Error("a word the table does not hold reached a command")
	}
}

// TestTwoCommandsCannotShareAWord covers the mistake the index refuses.
//
// Resolving it instead would leave one of the two reachable by nothing, and
// nothing would say so: the command would simply never answer.
func TestTwoCommandsCannotShareAWord(t *testing.T) {
	cases := map[string][]Def{
		"one name twice":       {{Name: "禁言"}, {Name: "禁言"}},
		"a name used as alias": {{Name: "禁言", Aliases: []string{"mute"}}, {Name: "mute"}},
		"one alias twice":      {{Name: "禁言", Aliases: []string{"mute"}}, {Name: "解禁", Aliases: []string{"mute"}}},
	}
	for why, defs := range cases {
		if _, err := NewCatalog(defs); err == nil {
			t.Errorf("%s was accepted: %+v", why, defs)
		}
	}
	if _, err := NewCatalog([]Def{{Aliases: []string{"mute"}}}); err == nil {
		t.Error("a definition with no name was accepted")
	}
}

// TestThePanelAndTheHelpComeFromTheSameTable covers the drift this table exists
// to prevent: what the menu offers and what the bot answers are read from one
// place, so the menu cannot advertise something that is not there.
func TestThePanelAndTheHelpComeFromTheSameTable(t *testing.T) {
	catalog := table(t)

	var names []string
	for _, item := range catalog.Panel("/") {
		names = append(names, item.Name)
	}
	// The unregistered command and the unavailable one are both left out.
	if want := []string{"/菜单", "/禁言"}; !reflect.DeepEqual(names, want) {
		t.Errorf("panel = %v, want %v", names, want)
	}

	help := catalog.Usage("/")
	if len(help) != len(catalog.Definitions()) {
		t.Errorf("help lists %d of %d commands", len(help), len(catalog.Definitions()))
	}
	// The unregistered one is in the help, which is where it can say what it is.
	if want := "/debug"; help[len(help)-1] != want {
		t.Errorf("the last help line = %q, want %q", help[len(help)-1], want)
	}

	// A single chat lists only what it answers: one line, and not the group's.
	private := catalog.PrivateUsage("/")
	if want := []string{"/菜单"}; !reflect.DeepEqual(private, want) {
		t.Errorf("private usage = %v, want %v", private, want)
	}
}
