package app

import (
	"bytes"
	"strings"
	"testing"
)

// TestAFeatureTheConnectionCannotHearIsSaidOutLoud covers the one part of a change a reload
// cannot adopt.
//
// The subscription is fixed when the connection is built: the intents are an option of the
// transport, and the transport owns the connection -- handshake, heartbeats, resuming,
// reconnecting -- from then on. So a feature enabled by an edit is a feature whose events
// never arrive: it does nothing at all, and nothing says why. This is that being said,
// because a change that quietly leaves a feature idle is the hardest kind to notice.
func TestAFeatureTheConnectionCannotHearIsSaidOutLoud(t *testing.T) {
	live, _, _ := manager(t, "one")
	before := started(t, live, "one")
	var logged bytes.Buffer
	reload := reloadFor(t, live, before, &logged)
	// The connection was subscribed before this feature existed.
	reload.subscribed = 0

	fresh := configOf(t, "one")
	fresh.Features["one"] = sectionFrom(t, "enabled: true\nsomething: else")
	if err := reload.apply(fresh, "test"); err != nil {
		t.Fatalf("applying: %v", err)
	}

	if !strings.Contains(logged.String(), "not subscribed to") {
		t.Errorf("a feature whose events never reach it was adopted silently:\n%s",
			logged.String())
	}

	// And nothing is said when the connection already hears everything the features need,
	// which is the ordinary case: the same edit, with a subscription that covers it.
	logged.Reset()
	reload.subscribed = live.intents()
	again := configOf(t, "one")
	again.Features["one"] = sectionFrom(t, "enabled: true\nsomething: again")
	if err := reload.apply(again, "test"); err != nil {
		t.Fatalf("applying again: %v", err)
	}
	if strings.Contains(logged.String(), "not subscribed to") {
		t.Errorf("a change the connection can already hear was reported as missing:\n%s",
			logged.String())
	}
}
