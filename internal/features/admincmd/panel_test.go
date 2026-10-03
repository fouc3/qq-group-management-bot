package admincmd

import (
	"testing"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/command"
)

// publishedPanel returns the panel this start published in one scope, or nil.
//
// Read out of the recorded call rather than from a client the harness would have
// had to fake: what the platform receives is the thing worth asserting, and this
// is that body. The scope is in the body of the call that creates a panel, which
// is how one is told from the other -- and the single-chat panel is the one this
// bot has only started publishing.
func (h *harness) publishedPanel(scope string) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, call := range h.calls {
		if call["scope"] != scope {
			continue
		}
		if panel, ok := call["panel"].(map[string]any); ok {
			return panel
		}
	}
	return nil
}

// panelScopes returns the scopes this start published a panel in.
func (h *harness) panelScopes() map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	scopes := map[string]bool{}
	for _, call := range h.calls {
		if scope, ok := call["scope"].(string); ok {
			if _, panel := call["panel"].(map[string]any); panel {
				scopes[scope] = true
			}
		}
	}
	return scopes
}

// latestPanel returns the panel this bot published last in one scope, or nil.
//
// Read backwards, because a panel is published again when the table changes: the
// first one in the list is what the group was looking at before.
func (h *harness) latestPanel(scope string) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	for index := len(h.calls) - 1; index >= 0; index-- {
		call := h.calls[index]
		if call["scope"] != scope {
			continue
		}
		if panel, ok := call["panel"].(map[string]any); ok {
			return panel
		}
	}
	return nil
}

// waitForPanelEntry waits for the panel to offer a command.
//
// A table that changed while the bot runs is published again in the background --
// publishing talks to the platform, so it does not happen on the wiring's own
// goroutine -- which is why this waits rather than looks.
func (h *harness) waitForPanelEntry(t *testing.T, scope, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if panel := h.latestPanel(scope); panel != nil {
			if _, ok := panelEntries(t, panel)[name]; ok {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the panel published in %s never offered %s", scope, name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// panelEntries returns the published entries by name.
func panelEntries(t *testing.T, panel map[string]any) map[string]map[string]any {
	t.Helper()
	items, ok := panel["items"].([]any)
	if !ok {
		t.Fatalf("the panel carries no items: %+v", panel)
	}
	entries := map[string]map[string]any{}
	for _, raw := range items {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		entries[name] = entry
	}
	return entries
}

// TestTheGroupPanelPublishesEveryCommandButDebug covers what the panel is for: a
// member opening the command menu sees what the bot answers.
//
// /debug is the exception the configuration asks for. It is a tool for whoever
// runs the bot, and putting it in front of a group invites somebody to press it.
func TestTheGroupPanelPublishesEveryCommandButDebug(t *testing.T) {
	h := newHarnessWithRegistering(t, baseSection)

	panel := h.publishedPanel("group")
	if panel == nil {
		t.Fatal("no group instruction panel was published")
	}
	entries := panelEntries(t, panel)

	for _, want := range []string{
		"/菜单", "/whois", "/禁言", "/解禁", "/重新发送验证", "/重新验证", "/黑名单",
	} {
		if _, ok := entries[want]; !ok {
			t.Errorf("the panel does not offer %q", want)
		}
	}
	if _, ok := entries["/debug"]; ok {
		t.Error("/debug was published, but it is an operator's tool rather than " +
			"an entry to put in front of a group")
	}

	// Every entry is a command, not a link: pressing one has to put something the
	// bot answers into the composer.
	for name, entry := range entries {
		if entry["type"] != "command" {
			t.Errorf("entry %q has type %v, want command", name, entry["type"])
		}
		if desc, _ := entry["desc"].(string); desc == "" {
			t.Errorf("entry %q has no explanation, so nothing says what it does", name)
		}
	}
}

// TestASingleChatGetsItsOwnPanel covers the panel that only a single chat shows.
//
// It is not the group's panel under another name. The platform keeps them apart by
// scope, and what belongs in it is what a single chat answers -- a group's panel
// put here would offer commands that cannot run without a group, and the member
// pressing one would learn that by being refused.
func TestASingleChatGetsItsOwnPanel(t *testing.T) {
	h := newHarnessWithRegistering(t, baseSection)

	panel := h.publishedPanel("c2c")
	if panel == nil {
		t.Fatal("no single-chat instruction panel was published")
	}
	entries := panelEntries(t, panel)
	// /whois is in here as well: a single chat is where somebody reads their own
	// user_openid, which is the identifier a group's event never carries.
	for _, want := range []string{"/菜单", "/whois", "/违规查询"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("the single-chat panel does not offer %q, which is answered there", want)
		}
	}
	for _, unwanted := range []string{"/禁言", "/黑名单", "/debug", "/违规举报"} {
		if _, ok := entries[unwanted]; ok {
			t.Errorf("the single-chat panel offers %q, which a single chat does not answer",
				unwanted)
		}
	}

	// Everybody who can write to the bot: there is no list of users to keep, and a
	// panel applying to nobody would be a menu nobody ever sees.
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, call := range h.calls {
		if call["scope"] != "c2c" {
			continue
		}
		if call["target_type"] != "all" {
			t.Errorf("the single-chat panel applies to %v, want all", call["target_type"])
		}
		if groups, ok := call["group_openids"].([]any); ok && len(groups) > 0 {
			t.Errorf("the single-chat panel was given %d group(s)", len(groups))
		}
	}
}

// TestBothPanelsArePublished covers the pair: one start keeps one panel in each
// place, so a bot that has been running for a while does not leave the single-chat
// panel behind when the group one is updated.
func TestBothPanelsArePublished(t *testing.T) {
	h := newHarnessWithRegistering(t, baseSection)

	scopes := h.panelScopes()
	if !scopes["group"] || !scopes["c2c"] {
		t.Errorf("the panels published were %v, want one in each of group and c2c", scopes)
	}
	if len(scopes) != 2 {
		t.Errorf("the panels published were %v, want exactly two", scopes)
	}
}

// TestThePanelRestrictsNobody covers the flag that is deliberately left unset.
//
// The platform reads only_admin as its own notion of the role -- the group owner
// and whoever they appointed -- while this bot obeys the administrator list in
// the configuration. Marking an entry would hide it from exactly the people the
// configuration names, whenever one of them holds no platform role, which is how
// a management command becomes invisible to its own administrator.
//
// The bot still refuses an ordinary member when the command arrives.
func TestThePanelRestrictsNobody(t *testing.T) {
	h := newHarnessWithRegistering(t, baseSection)

	for _, scope := range []string{"group", "c2c"} {
		panel := h.publishedPanel(scope)
		if panel == nil {
			t.Fatalf("no panel was published in %s", scope)
		}
		entries := panelEntries(t, panel)
		if len(entries) == 0 {
			t.Fatalf("the panel published in %s is empty", scope)
		}
		for name, entry := range entries {
			if entry["only_admin"] == true {
				t.Errorf("entry %q in %s is restricted to the platform's own "+
					"administrators, which is not the list this bot obeys", name, scope)
			}
		}
	}

	// The management commands are still offered: refusing them at run time is
	// what keeps them out of the wrong hands, and a menu nobody can see is a
	// command nobody knows exists.
	if _, ok := panelEntries(t, h.publishedPanel("group"))["/黑名单"]; !ok {
		t.Error("the panel does not offer the blacklist command")
	}
}

// TestThePanelIsNotPublishedWhenTheSectionSaysSo covers the switch, so a
// deployment that maintains its own panel is left alone.
func TestThePanelIsNotPublishedWhenTheSectionSaysSo(t *testing.T) {
	h := newHarness(t, baseSection)

	if scopes := h.panelScopes(); len(scopes) != 0 {
		t.Errorf("panels were published in %v even though register_commands is off", scopes)
	}
}

// TestTheReportCommandIsOnlyOfferedWhenItWorks covers the entry that depends on
// another feature.
//
// The panel is built during registration, before the app hands the judge over, so
// the harness cannot inject one in time: the entry is asked for directly instead,
// with and without a judge behind it.
func TestTheReportCommandIsOnlyOfferedWhenItWorks(t *testing.T) {
	offered := func(h *handler) bool {
		for _, item := range h.panelItems(command.InGroup) {
			if item.Name == "/违规举报" {
				return true
			}
		}
		return false
	}

	// A cache with no judge: the command exists and would answer "not configured",
	// which is exactly what the menu should not advertise.
	without := &handler{cfg: Config{Prefix: "/"}}
	if offered(without) {
		t.Error("the menu offers the report command with no judge behind it")
	}

	with := &handler{cfg: Config{Prefix: "/"}, moderation: &stubJudge{}}
	if !offered(with) {
		t.Error("the menu does not offer the report command although a judge is present")
	}
}
