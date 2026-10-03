// Package command holds what a command is, how one is recognised in a message,
// and the table of them a bot answers.
//
// It knows the platform's message shapes and nothing about what any command
// does. A definition says what a command is called, who may run it and where it
// is shown; the feature that owns the command supplies the function that runs
// it. That is what lets a second feature answer commands of its own without
// either feature importing the other, and without a second copy of the rules
// below.
//
// Three readers share one table: the dispatcher that runs a command, the help
// text a member can ask for, and the instruction panel the platform shows. Kept
// apart they drift, and the panel is the one that goes quietly out of date --
// nobody re-reads a panel they cannot see.
package command

import (
	"context"
	"errors"
	"fmt"
	"strings"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// prefixPlaceholder is where the configured prefix goes in a show line.
//
// A placeholder rather than a function because a show line is data: it is
// written once beside the command it describes, and the prefix is not known
// until the configuration has been read.
const prefixPlaceholder = "{prefix}"

// Audience says who may run a command.
//
// It is the command's own property rather than a check somewhere along the path
// that dispatches it. The check that runs is derived from the table, so what a
// command is restricted to and what the table says about it cannot come apart.
type Audience uint8

const (
	// Everyone: any member of a group the bot manages.
	Everyone Audience = iota

	// Admins: only the members the configuration names as this group's
	// administrators. Being an administrator in QQ grants nothing here.
	Admins

	// Whois: the administrators, a group that names none yet, or a deployment
	// that turned the restriction off. It is the command that discovers the
	// identifiers the administrator list is written with, so it cannot itself be
	// locked behind a list that is not written yet.
	Whois
)

// Runner carries out one command in a group.
//
// An error means the command could not be carried out at all, and it is reported
// to the dispatcher rather than to the group: everything a member is meant to
// read is said by the runner itself, because only it knows what was asked.
type Runner func(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	cmd Parsed) error

// PrivateRunner carries out one command in a single chat.
type PrivateRunner func(ctx context.Context, data *qqbotsdk.C2CMessageCreateData,
	cmd Parsed) error

// PanelPlacement is one command's place in one instruction panel.
type PanelPlacement struct {
	// Scene is the panel: the group instruction panel, or the one a single chat
	// shows.
	Scene Scene

	// OnlyPlatformAdmins asks the platform to show this entry only to the
	// administrators the platform knows -- a QQ group's owner and whoever they
	// appointed -- and not to the administrators the configuration names.
	//
	// The two sets differ in both directions, so a command whose gate is the
	// configured list should leave this off: sending it hides a management
	// command from exactly the people the configuration names, whenever one of
	// them holds no platform role, and shows it to whoever holds one without
	// being named. The check that matters happens when the command arrives; the
	// panel is a menu, not a lock.
	//
	// It is declared rather than dropped because a deployment whose configured
	// administrators are its groups' real ones can ask for the hiding too, one
	// command at a time.
	OnlyPlatformAdmins bool
}

// Def is one command a bot answers.
type Def struct {
	// Name invokes it. It is written with the prefix wherever it is shown, so
	// what a member copies is what the bot accepts.
	Name string

	// Aliases are the other words that invoke it, for the same reason a person
	// learns one name and types another.
	Aliases []string

	// Usage is the help line a group is shown, with {prefix} where the prefix
	// goes. It may explain more than one line's worth.
	Usage string

	// Desc is the instruction panel's explanation. The platform caps an entry at
	// thirty characters counting a Chinese character as two, so this stays short.
	Desc string

	// Audience decides who may run it.
	Audience Audience

	// Panels are the instruction panels this command is offered in, and which
	// they are is the plugin's decision rather than something worked out from
	// the runners.
	//
	// A command can work in a place without being worth a signpost there -- a
	// lookup still needs its argument typed after it, so the entry only
	// advertises a question -- and it can be worth a signpost where not every
	// reader may run it, because the panel is how a group learns that a
	// management command exists at all.
	//
	// Empty means no panel holds it. The help still lists it, which is where it
	// can say what it is for.
	Panels []PanelPlacement

	// Available hides the panel entry when it reports false, and is asked rather
	// than assumed.
	//
	// An entry is only worth a place in the menu when there is something behind
	// it: a command whose only answer is that it is not configured is worse than
	// no command at all. The help still lists it, because there it can say why.
	// Nil means always available.
	Available func() bool

	// InUnconfiguredGroup allows the command in a group that the bot's own group
	// list does not name yet.
	//
	// It is how a group gets configured at all: the command that reports the
	// identifiers the group's entry is written with has to answer before that
	// entry exists. Every other command is left out, because there would be no
	// administrator list to check the sender against.
	InUnconfiguredGroup bool

	// Run carries the command out. Nil means the name is not one the bot answers:
	// it still goes through the audience check, and what comes back is the list
	// of what the bot does answer.
	Run Runner

	// Private is how the command answers in a single chat, or nil when a single
	// chat does not answer it.
	//
	// A single chat is not a group without a group id: there is no membership to
	// check and no mention that could have carried the command. What is left is
	// worth answering, and it is a decision per command.
	Private PrivateRunner

	// PrivateUsage is the help line a single chat lists the command with, and an
	// empty one keeps it out of that list.
	//
	// It is separate from Usage because the two are not the same list: a group
	// command typed into a single chat is not answered there, so listing it would
	// advertise something that does not work.
	PrivateUsage string
}

// words is every word that invokes the command, the name first.
func (d Def) words() []string {
	return append([]string{d.Name}, d.Aliases...)
}

// Catalog is the table of commands one bot answers.
type Catalog struct {
	defs  []Def
	index map[string]int
}

// NewCatalog indexes a table.
//
// A word that invokes two commands is refused rather than resolved. It is a
// mistake made while editing the table, and a table that silently picked one
// would leave the other reachable by nothing at all -- which is exactly the kind
// of drift between what is written and what runs that one table exists to
// prevent.
func NewCatalog(defs []Def) (*Catalog, error) {
	catalog := &Catalog{
		defs:  append([]Def(nil), defs...),
		index: make(map[string]int, len(defs)),
	}
	for index, def := range catalog.defs {
		if strings.TrimSpace(def.Name) == "" {
			return nil, errors.New("command: a definition names no command")
		}
		for _, word := range def.words() {
			if taken, clash := catalog.index[word]; clash {
				return nil, fmt.Errorf("command: %q answers to %q, which %q already answers to",
					def.Name, word, catalog.defs[taken].Name)
			}
			catalog.index[word] = index
		}
	}
	return catalog, nil
}

// Definitions lists the table in the order it is written, which is the order it
// is shown in.
func (c *Catalog) Definitions() []Def {
	return append([]Def(nil), c.defs...)
}

// Lookup returns the command a word invokes.
func (c *Catalog) Lookup(word string) (Def, bool) {
	index, found := c.index[word]
	if !found {
		return Def{}, false
	}
	return c.defs[index], true
}

// Usage lists the help lines, in the order the table is written.
//
// Every command is here, the unregistered ones included: this is the list that
// can explain why a command is not in the panel, and leaving one out of it would
// leave a member with no way to find out that it exists.
func (c *Catalog) Usage(prefix string) []string {
	lines := make([]string, 0, len(c.defs))
	for _, def := range c.defs {
		lines = append(lines, fillPrefix(def.Usage, prefix))
	}
	return lines
}

// PrivateUsage lists what a single chat shows as available.
//
// A line and a way to answer it are the same claim, so both are required: a command a
// single chat does not answer has no line here, however it is written. Otherwise the
// help would advertise something the dispatcher refuses, which is the one thing this
// list exists to prevent.
func (c *Catalog) PrivateUsage(prefix string) []string {
	lines := make([]string, 0, len(c.defs))
	for _, def := range c.defs {
		if def.PrivateUsage == "" || def.Private == nil {
			continue
		}
		lines = append(lines, fillPrefix(def.PrivateUsage, prefix))
	}
	return lines
}

// Panel renders one instruction panel's entries, which is the menu a member
// opens.
//
// What a panel holds is what the table declares for that scene, so the group
// panel and the single-chat one are two readings of one list rather than two
// lists. only_admin is left unset for every entry unless a command asks for it by
// name: see PanelPlacement.OnlyPlatformAdmins for why the flag is not the gate.
func (c *Catalog) Panel(scene Scene, prefix string) []qqbotsdk.PanelItem {
	var items []qqbotsdk.PanelItem
	for _, def := range c.defs {
		place, offered := def.panelPlace(scene)
		if !offered {
			continue
		}
		// An entry is only worth a place in the menu when there is something
		// behind it: a command whose only answer is that it is not configured is
		// worse than no command at all.
		if def.Available != nil && !def.Available() {
			continue
		}
		items = append(items, qqbotsdk.PanelItem{
			Name:      prefix + def.Name,
			Desc:      def.Desc,
			Type:      qqbotsdk.PanelItemCommand,
			OnlyAdmin: place.OnlyPlatformAdmins,
		})
	}
	return items
}

// panelPlace reports where this command is offered in one scene.
func (d Def) panelPlace(scene Scene) (PanelPlacement, bool) {
	for _, place := range d.Panels {
		if place.Scene == scene {
			return place, true
		}
	}
	return PanelPlacement{}, false
}

// fillPrefix puts the configured prefix where a show line asked for it.
func fillPrefix(line, prefix string) string {
	return strings.ReplaceAll(line, prefixPlaceholder, prefix)
}
