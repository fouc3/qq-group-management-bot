package admincmd

import "testing"

// publishedPanel returns the panel this start published, or nil.
//
// Read out of the recorded call rather than from a client the harness would have
// had to fake: what the platform receives is the thing worth asserting, and this
// is that body.
func (h *harness) publishedPanel() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, call := range h.calls {
		if panel, ok := call["panel"].(map[string]any); ok {
			return panel
		}
	}
	return nil
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

// TestThePanelPublishesEveryCommandButDebug covers what the panel is for: a
// member opening the command menu sees what the bot answers.
//
// /debug is the exception the configuration asks for. It is a tool for whoever
// runs the bot, and putting it in front of a group invites somebody to press it.
func TestThePanelPublishesEveryCommandButDebug(t *testing.T) {
	h := newHarnessWithRegistering(t, baseSection)

	panel := h.publishedPanel()
	if panel == nil {
		t.Fatal("no instruction panel was published")
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

	entries := panelEntries(t, h.publishedPanel())
	if len(entries) == 0 {
		t.Fatal("the panel is empty")
	}
	for name, entry := range entries {
		if entry["only_admin"] == true {
			t.Errorf("entry %q is restricted to the platform's own administrators, "+
				"which is not the list this bot obeys", name)
		}
	}
	// The management commands are still offered: refusing them at run time is
	// what keeps them out of the wrong hands, and a menu nobody can see is a
	// command nobody knows exists.
	if _, ok := entries["/黑名单"]; !ok {
		t.Error("the panel does not offer the blacklist command")
	}
}

// TestThePanelIsNotPublishedWhenTheSectionSaysSo covers the switch, so a
// deployment that maintains its own panel is left alone.
func TestThePanelIsNotPublishedWhenTheSectionSaysSo(t *testing.T) {
	h := newHarness(t, baseSection)

	if panel := h.publishedPanel(); panel != nil {
		t.Error("a panel was published even though register_commands is off")
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
		for _, item := range h.panelItems() {
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
