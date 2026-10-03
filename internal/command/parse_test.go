package command

import "testing"

// TestParseCommand covers the prefix and the mention the platform leaves in the
// text when the bot receives every group message.
func TestParseCommand(t *testing.T) {
	parsed, ok := Parse("/禁言 30m", "/")
	if !ok {
		t.Fatal("a plain command should parse")
	}
	if parsed.Name != "禁言" || len(parsed.Args) != 1 || parsed.Args[0] != "30m" {
		t.Errorf("command = %+v", parsed)
	}

	// In full receive mode the mention of the bot stays in the text.
	parsed, ok = Parse("<@BOT-OPENID> /mute 1h", "/")
	if !ok {
		t.Fatal("a mention before the command should be stripped")
	}
	if parsed.Name != "mute" || parsed.Args[0] != "1h" {
		t.Errorf("command = %+v", parsed)
	}

	for _, bad := range []string{"", "禁言 30m", "/", "  ", "hello /禁言"} {
		if _, ok := Parse(bad, "/"); ok {
			t.Errorf("Parse(%q) should not have matched", bad)
		}
	}
}
