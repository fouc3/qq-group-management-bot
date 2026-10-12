package moderation

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// sectionNode reads one section the way the app hands it over.
func sectionNode(t *testing.T, text string) yaml.Node {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(text), &document); err != nil {
		t.Fatalf("decoding the section: %v", err)
	}
	if len(document.Content) == 0 {
		t.Fatalf("the section %q decoded to nothing", text)
	}
	return *document.Content[0]
}

// reloadable builds a running instance and hands back the thing a test has to call
// Reload on.
func reloadable(t *testing.T, section string) *handler {
	t.Helper()
	instance, err := New(sectionNode(t, section), feature.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Nothing answers at this address. The cache is not what these tests are
		// about, and a failure to reach it is reported and lived with rather than
		// being fatal.
		Redis: config.Redis{
			Addr: "127.0.0.1:1", Prefix: "qgb-test", DialTimeoutSeconds: 1,
		},
	})
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	t.Cleanup(func() { instance.Close(context.Background()) })

	built, ok := instance.(*handler)
	if !ok {
		t.Fatalf("the feature built is a %T, not the handler this test needs", instance)
	}
	return built
}

// TestReloadRefusesWhatThePictureCacheCannotAdopt covers the settings that are settled
// when the feature is built rather than when it is asked.
//
// A reload that accepted a new images.dir and went on writing pictures to the old one
// would be a file that lies about itself: the log would say the configuration was
// adopted, and the directory the operator changed would stay empty. The few settings
// that cannot be adopted are refused with a reason instead -- and only those, which is
// the second half of this test.
func TestReloadRefusesWhatThePictureCacheCannotAdopt(t *testing.T) {
	before := filepath.Join(t.TempDir(), "pictures-before")
	after := filepath.Join(t.TempDir(), "pictures-after")

	live := reloadable(t, fmt.Sprintf("enabled: true\nimages:\n  dir: %q\n", before))

	refused := sectionNode(t, fmt.Sprintf("enabled: true\nimages:\n  dir: %q\n", after))
	if err := live.Reload(refused); err == nil {
		t.Error("a reload that changes where pictures are kept reported success, but " +
			"the picture cache is built at startup and is not built again: the file " +
			"in force would name a directory nothing writes to, and the operator's " +
			"change would be silently half-applied")
	}

	// The other half: a setting that can be adopted still is. A reload that refused
	// everything would pass the check above and be useless.
	adopted := sectionNode(t, fmt.Sprintf(
		"enabled: true\nimages:\n  dir: %q\ndry_run: false\n", before))
	if err := live.Reload(adopted); err != nil {
		t.Errorf("a reload that only turns dry_run off was refused: %v", err)
	}
}
