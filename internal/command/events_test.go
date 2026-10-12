package command

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlyThisPackageDeclaresHandlers covers the rule that makes Events worth having.
//
// A feature that calls the connection itself is a feature whose handlers can be left
// behind: the instance that replaces it goes on answering every event a second time,
// with the configuration it was replaced for. Three features did exactly that, and the
// registration they forgot was invisible -- the compiler cannot see it, and neither can
// a reader who is looking at one feature at a time.
//
// It is checked by reading the source rather than by behaviour, because what it has to
// catch is a call that has not been written yet.
//
// The button dispatcher is not an exception here: it is declared in this package, once
// per bot, and deliberately outlives every feature.
func TestOnlyThisPackageDeclaresHandlers(t *testing.T) {
	const features = "../features"

	visited := 0
	err := filepath.WalkDir(features, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		visited++
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(source), ".RegisterFunc(") {
			t.Errorf("%s declares an event handler on the connection directly. A feature "+
				"declares what it answers in the command.Events it was built with, which "+
				"gives every one of them back in Close: a registration made here is one "+
				"nobody takes back, and the feature built again answers everything twice",
				path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", features, err)
	}
	if visited == 0 {
		t.Fatalf("no feature source was read under %s, so this check passed by looking "+
			"at nothing", features)
	}
}
