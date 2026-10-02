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

// TestThePanelKeepsTheMenuOpenToEverybody covers the entry that has to stay
// visible: hiding the help from ordinary members would leave them with no way to
// find out what the bot answers.
func TestThePanelKeepsTheMenuOpenToEverybody(t *testing.T) {
	h := newHarnessWithRegistering(t, baseSection)
	entries := panelEntries(t, h.publishedPanel())

	if _, ok := entries["/菜单"]; !ok {
		t.Fatal("the panel does not offer the menu")
	}
	if entries["/菜单"]["only_admin"] == true {
		t.Error("the menu is restricted to administrators, so an ordinary member " +
			"has no way to discover the commands")
	}
	if entries["/禁言"]["only_admin"] != true {
		t.Error("the mute command is offered to everybody in the panel")
	}
}

// TestThePanelFollowsTheWhoisSetting covers the one entry whose audience is
// configurable: a panel that hid /whois from the members who are allowed to use
// it would be advertising the wrong thing.
func TestThePanelFollowsTheWhoisSetting(t *testing.T) {
	h := newHarnessWithRegistering(t, baseSection+"\nwhois_admin_only: false\n")
	entries := panelEntries(t, h.publishedPanel())

	if entries["/whois"]["only_admin"] == true {
		t.Error("/whois is restricted in the panel while the section says " +
			"everybody may use it")
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
