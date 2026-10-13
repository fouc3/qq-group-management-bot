package app

import (
	"bytes"
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/features/admincmd"
	"github.com/fouc3/qq-group-management-bot/internal/features/broadcast"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinrequest"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinverify"
	"github.com/fouc3/qq-group-management-bot/internal/features/memberlog"
	"github.com/fouc3/qq-group-management-bot/internal/features/moderation"
	"github.com/fouc3/qq-group-management-bot/internal/features/muteguard"
)

// exampleConfigPath is the file an operator copies from.
const exampleConfigPath = "../../config.example.yaml"

// frameworkKeys are the keys any feature section may carry, whatever the feature is.
//
// The app reads them itself, before the feature is built, so no feature's own struct
// declares them -- and a check that did not allow for them would report every section
// in the file.
var frameworkKeys = map[string]bool{
	"enabled": true,
}

// TestTheExampleConfigurationNamesOnlyKeysTheCodeReads covers the one file that tells
// an operator what the bot can be asked for.
//
// A key the code does not read is worse than a key that is missing. The file reads as
// if it did something, and it decodes without complaint -- a feature decodes its own
// section and ignores what it does not know, deliberately, so that an older binary can
// run beside a newer file -- so nothing anywhere says the setting was never honoured.
// That is how a `context_scope` outlived the idea it was named after while the example
// went on advertising it, with `group` written where the code only ever did `sender`.
//
// Each section goes through a strict decoder of its own, so this covers the sections
// above `features:` as well as the seven below it. It fails on a key, not on a value:
// whether the example's numbers are sensible is a judgement no test can make.
func TestTheExampleConfigurationNamesOnlyKeysTheCodeReads(t *testing.T) {
	raw, err := os.ReadFile(exampleConfigPath)
	if err != nil {
		t.Fatalf("reading %s: %v", exampleConfigPath, err)
	}

	var document struct {
		Bot               yaml.Node            `yaml:"bot"`
		OneBot            yaml.Node            `yaml:"onebot"`
		Database          yaml.Node            `yaml:"database"`
		Redis             yaml.Node            `yaml:"redis"`
		HumanVerification yaml.Node            `yaml:"human_verification"`
		Log               yaml.Node            `yaml:"log"`
		Features          map[string]yaml.Node `yaml:"features"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("reading the sections of %s: %v", exampleConfigPath, err)
	}

	cases := []struct {
		section string
		node    yaml.Node
		into    any
	}{
		{"bot", document.Bot, &config.Bot{}},
		{"onebot", document.OneBot, &config.OneBot{}},
		{"database", document.Database, &config.Database{}},
		{"redis", document.Redis, &config.Redis{}},
		{"human_verification", document.HumanVerification, &config.HumanVerification{}},
		{"log", document.Log, &config.Log{}},
		{"features." + admincmd.Name, document.Features[admincmd.Name], &admincmd.Config{}},
		{"features." + broadcast.Name, document.Features[broadcast.Name], &broadcast.Config{}},
		{"features." + joinrequest.Name, document.Features[joinrequest.Name], &joinrequest.Config{}},
		{"features." + joinverify.Name, document.Features[joinverify.Name], &joinverify.Config{}},
		{"features." + memberlog.Name, document.Features[memberlog.Name], &memberlog.Config{}},
		{"features." + moderation.Name, document.Features[moderation.Name], &moderation.Config{}},
		{"features." + muteguard.Name, document.Features[muteguard.Name], &muteguard.Config{}},
	}

	for _, test := range cases {
		if test.node.Kind == 0 {
			t.Errorf("%s has no %s section, so nothing tells an operator about "+
				"it", exampleConfigPath, test.section)
			continue
		}
		strictlyDecode(t, test.section, test.node, test.into)
	}
}

// strictlyDecode reads one section the way the app does not.
//
// The app decodes a feature's section leniently, which is what lets an older binary run
// beside a newer file. Reading it strictly is the whole of this check: a key the struct
// does not have is a key the code does not read.
func strictlyDecode(t *testing.T, section string, node yaml.Node, into any) {
	t.Helper()
	if node.Kind == yaml.MappingNode {
		kept := node.Content[:0:0]
		for index := 0; index+1 < len(node.Content); index += 2 {
			if frameworkKeys[node.Content[index].Value] {
				continue
			}
			kept = append(kept, node.Content[index], node.Content[index+1])
		}
		node.Content = kept
	}

	encoded, err := yaml.Marshal(&node)
	if err != nil {
		t.Fatalf("encoding the %s section: %v", section, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	if err := decoder.Decode(into); err != nil {
		t.Errorf("%s names something the code does not read, so an operator who "+
			"copies it writes a setting that does nothing: %v", section, err)
	}
}
