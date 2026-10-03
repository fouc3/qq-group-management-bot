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
			Audience: Everyone, PrivateUsage: "{prefix}菜单",
			Panels: []PanelPlacement{{Scene: InGroup}, {Scene: InPrivate}}},
		{Name: "禁言", Aliases: []string{"mute"}, Usage: "{prefix}禁言 <时长>", Desc: "禁言成员",
			Audience: Admins, Panels: []PanelPlacement{{Scene: InGroup}}},
		{Name: "违规举报", Usage: "{prefix}违规举报", Desc: "引用消息举报", Audience: Everyone,
			Available: func() bool { return false },
			Panels:    []PanelPlacement{{Scene: InGroup}}},
		{Name: "回执", Usage: "{prefix}回执 <单号>", Desc: "查看回执", Audience: Everyone,
			Panels: []PanelPlacement{{Scene: InPrivate}}},
		{Name: "debug", Usage: "{prefix}debug", Desc: "调试用", Audience: Admins},
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

// TestEveryPanelIsReadOffTheTable covers what each panel holds: the commands that
// declare that scene, and nothing else.
//
// The two panels are deliberately not each other's complement -- one command
// declares only the group panel, another only the single-chat one -- because what
// a panel offers is the plugin's decision per scene, and a command can be worth a
// signpost in one place and not in the other.
func TestEveryPanelIsReadOffTheTable(t *testing.T) {
	catalog := table(t)

	var group []string
	for _, item := range catalog.Panel(InGroup, "/") {
		group = append(group, item.Name)
	}
	// The command that declares no panel is left out, and so is the one that
	// declares only the single-chat panel, and the one with nothing behind it.
	if want := []string{"/菜单", "/禁言"}; !reflect.DeepEqual(group, want) {
		t.Errorf("the group panel = %v, want %v", group, want)
	}

	var private []string
	for _, item := range catalog.Panel(InPrivate, "/") {
		private = append(private, item.Name)
	}
	if want := []string{"/菜单", "/回执"}; !reflect.DeepEqual(private, want) {
		t.Errorf("the single-chat panel = %v, want %v", private, want)
	}

	// Nothing is hidden from anybody unless a command asks for it by name.
	for _, scene := range []Scene{InGroup, InPrivate} {
		for _, item := range catalog.Panel(scene, "/") {
			if item.OnlyAdmin {
				t.Errorf("%s is restricted to the platform's administrators", item.Name)
			}
		}
	}

	help := catalog.Usage("/")
	if len(help) != len(catalog.Definitions()) {
		t.Errorf("help lists %d of %d commands", len(help), len(catalog.Definitions()))
	}
	// The command that declares no panel is in the help, which is where it can
	// say what it is.
	if want := "/debug"; help[len(help)-1] != want {
		t.Errorf("the last help line = %q, want %q", help[len(help)-1], want)
	}

	// A single chat lists only what it answers: one line, and not the group's.
	privateUsage := catalog.PrivateUsage("/")
	if want := []string{"/菜单"}; !reflect.DeepEqual(privateUsage, want) {
		t.Errorf("private usage = %v, want %v", privateUsage, want)
	}
}
