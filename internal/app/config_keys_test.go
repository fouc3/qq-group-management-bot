package app

import (
	"reflect"
	"testing"

	"github.com/fouc3/qq-group-management-bot/internal/features/admincmd"
	"github.com/fouc3/qq-group-management-bot/internal/features/broadcast"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinrequest"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinverify"
	"github.com/fouc3/qq-group-management-bot/internal/features/memberlog"
	"github.com/fouc3/qq-group-management-bot/internal/features/moderation"
	"github.com/fouc3/qq-group-management-bot/internal/features/muteguard"
)

// TestNoFeatureDeclaresTheEnabledSwitch covers who owns the one key every feature
// section may carry.
//
// `enabled` is read by the app, of the raw section, before the feature is built --
// that is feature.Enabled, and it is what makes a section written without the key an
// enabled one. A feature's own struct therefore has no business declaring it. Six of
// them did, not one of them read it, and each was a place where a reader concludes
// that the switch is handled here and stops looking. A field that exists and is never
// read is worse than a missing one, because nothing tells anybody: the config decodes,
// the section looks complete, and the field is a fact about the past.
//
// Only each feature's top-level section is checked. An Enabled further down is a
// different statement -- whether judging happens in this group, whether the trial gate
// is on -- and those are read where they are declared.
func TestNoFeatureDeclaresTheEnabledSwitch(t *testing.T) {
	for _, test := range []struct {
		name   string
		config any
	}{
		{admincmd.Name, admincmd.Config{}},
		{broadcast.Name, broadcast.Config{}},
		{joinrequest.Name, joinrequest.Config{}},
		{joinverify.Name, joinverify.Config{}},
		{memberlog.Name, memberlog.Config{}},
		{moderation.Name, moderation.Config{}},
		{muteguard.Name, muteguard.Config{}},
	} {
		kind := reflect.TypeOf(test.config)
		for index := 0; index < kind.NumField(); index++ {
			field := kind.Field(index)
			if field.Name != "Enabled" {
				continue
			}
			t.Errorf("the %s section declares Enabled, which the app reads for itself "+
				"before the feature is built: nobody in %s reads this field, and it "+
				"says to a reader that the switch is handled here", test.name, test.name)
		}
	}
}
