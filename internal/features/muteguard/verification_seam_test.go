package muteguard

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/mutelog"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// TestARecordInTheTableIsNotWhatThisFeatureActsOn covers which side of the seam the
// answer comes from.
//
// The holds are the verification's records, and this feature asks that feature rather
// than reading them. A row left in the table by a run whose feature is no longer there
// is therefore not a hold: nobody is verifying that member, and a record that outlives
// the only thing that could release it must not be able to keep a mute standing for
// ever -- which is what the earlier repair of that table's own identity was about.
//
// The data layer is given here on purpose and never consulted: it is present in every
// real deployment, and this is the assertion that its records are not what decides.
func TestARecordInTheTableIsNotWhatThisFeatureActsOn(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, store.Config{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "test.db"),
	})
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	// A hold with no expiry of its own, left behind by a verification that is not
	// running any more.
	if err := database.Pending().Put(ctx, store.Pending{
		Token:        "TOKEN-" + testMember,
		GroupOpenID:  testGroup,
		MemberOpenID: testMember,
	}); err != nil {
		t.Fatalf("writing a hold: %v", err)
	}

	platform := newPlatform()
	server := platform.server(t)
	client, err := qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AccessToken: "test-token",
		BaseURL:     server.URL,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	built, err := New(sectionNode(t, oneMember), feature.Deps{
		Client: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Groups: config.Groups{{OpenID: testGroup}},
		Mutes:  mutelog.New(),
		Store:  database,
	})
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	guard := built.(*handler)

	platform.setMuted(testGroup, testMember)
	guard.sweep(ctx)

	if _, lifted, _ := platform.snapshot(); len(lifted) != 1 {
		t.Errorf("lifted %v, want the one mute nothing is verifying: a record whose "+
			"feature is not running is not a hold, and treating it as one keeps the "+
			"member muted for as long as the row lives", lifted)
	}
}
