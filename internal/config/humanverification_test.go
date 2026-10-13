package config

import (
	"strings"
	"testing"
)

// The online verification section is optional, and everything about it is
// checked before the bot starts: a deployment that means to use it and cannot
// reach it would otherwise hold every new member with no way to verify them,
// which is the one outcome the section's silence is not allowed to produce.

// withCredentials is a configuration that gets as far as the sections under
// test: the bot's own credentials are the first thing validated, and a section
// behind a missing token would never be reached.
func withCredentials(t *testing.T, section HumanVerification) *Config {
	t.Helper()
	parsed := Config{
		Bot:               Bot{AccessToken: "test-token"},
		HumanVerification: section,
	}
	return &parsed
}

// TestTheOnlineVerificationSectionIsOptional covers the deployment that does not
// verify online: no section, no complaint, and nothing filled in that would make
// it look configured.
func TestTheOnlineVerificationSectionIsOptional(t *testing.T) {
	parsed := withCredentials(t, HumanVerification{})
	if err := parsed.applyDefaults(); err != nil {
		t.Fatalf("a deployment without the section was refused: %v", err)
	}
	if parsed.HumanVerification.Configured() {
		t.Error("a section nobody wrote reports itself as configured")
	}
	if parsed.HumanVerification.TTLSeconds != 0 {
		t.Errorf("ttl = %d, want nothing filled in for a section that is not there",
			parsed.HumanVerification.TTLSeconds)
	}
}

// TestTheOnlineVerificationSectionFillsInWhatItLeavesOut covers the two values
// that have a right answer nobody should have to write down.
func TestTheOnlineVerificationSectionFillsInWhatItLeavesOut(t *testing.T) {
	parsed := withCredentials(t, HumanVerification{
		BaseURL:    "https://verify.test",
		IssueToken: "issue",
		WSToken:    "ws",
	})
	if err := parsed.applyDefaults(); err != nil {
		t.Fatalf("a complete section was refused: %v", err)
	}
	section := parsed.HumanVerification
	if !section.Configured() {
		t.Error("a section with an address does not report itself as configured")
	}
	if section.TTLSeconds != DefaultHumanVerificationTTLSeconds {
		t.Errorf("ttl = %d, want the default %d",
			section.TTLSeconds, DefaultHumanVerificationTTLSeconds)
	}
	if section.TimeoutSeconds != DefaultHumanVerificationTimeoutSeconds {
		t.Errorf("timeout = %d, want the default %d",
			section.TimeoutSeconds, DefaultHumanVerificationTimeoutSeconds)
	}
}

// TestTheOnlineVerificationSectionIsValidated covers the ways a file can get it
// wrong, each of which has to be refused at startup rather than at the first
// join.
func TestTheOnlineVerificationSectionIsValidated(t *testing.T) {
	cases := map[string]struct {
		section HumanVerification
		names   string
	}{
		"an address that is not http": {
			section: HumanVerification{
				BaseURL: "ftp://verify.test", IssueToken: "i", WSToken: "w"},
			names: "base_url",
		},
		"an address with no host": {
			section: HumanVerification{
				BaseURL: "https://", IssueToken: "i", WSToken: "w"},
			names: "base_url",
		},
		"a section with no issue token": {
			section: HumanVerification{
				BaseURL: "https://verify.test", WSToken: "w"},
			names: "issue_token",
		},
		"a section with no connection token": {
			section: HumanVerification{
				BaseURL: "https://verify.test", IssueToken: "i"},
			names: "ws_token",
		},
		"a marker Cloudflare would silently drop": {
			section: HumanVerification{
				BaseURL: "https://verify.test", IssueToken: "i", WSToken: "w",
				Cdata: "app=demo;uid=x"},
			names: "cdata",
		},
		"a lifetime below what the service accepts": {
			section: HumanVerification{
				BaseURL: "https://verify.test", IssueToken: "i", WSToken: "w",
				TTLSeconds: MinHumanVerificationTTLSeconds - 1},
			names: "ttl_seconds",
		},
		"a lifetime above what the service accepts": {
			section: HumanVerification{
				BaseURL: "https://verify.test", IssueToken: "i", WSToken: "w",
				TTLSeconds: MaxHumanVerificationTTLSeconds + 1},
			names: "ttl_seconds",
		},
		"a negative timeout": {
			section: HumanVerification{
				BaseURL: "https://verify.test", IssueToken: "i", WSToken: "w",
				TimeoutSeconds: -1},
			names: "timeout_seconds",
		},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			parsed := withCredentials(t, test.section)
			err := parsed.applyDefaults()
			if err == nil {
				t.Fatal("the section was accepted")
			}
			if !strings.Contains(err.Error(), test.names) {
				t.Errorf("err = %v, want it to name %s", err, test.names)
			}
		})
	}
}

// TestTheMarkerIsCheckedBecauseCloudflareDropsItSilently keeps the reason the
// marker is checked here at all, which is not obvious from the character set: a
// marker with anything else in it is dropped by Cloudflare without a word, and a
// result that comes back without its marker is discarded as somebody else's --
// so the mistake would look like the service being broken.
func TestTheMarkerIsCheckedBecauseCloudflareDropsItSilently(t *testing.T) {
	parsed := withCredentials(t, HumanVerification{
		BaseURL: "https://verify.test", IssueToken: "i", WSToken: "w",
		Cdata: "qq-bot-human-verification",
	})
	if err := parsed.applyDefaults(); err != nil {
		t.Fatalf("a marker of the documented shape was refused: %v", err)
	}
	if parsed.HumanVerification.TTLSeconds != DefaultHumanVerificationTTLSeconds {
		t.Errorf("ttl = %d, want the default", parsed.HumanVerification.TTLSeconds)
	}
}
