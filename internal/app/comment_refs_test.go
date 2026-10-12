package app

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// declarationLine matches a top-level declaration and captures the name it declares.
var declarationLine = regexp.MustCompile(
	`^(?:func\s+(?:\([^)]*\)\s*)?|type\s+|var\s+|const\s+)([A-Za-z_]\w*)`)

// docLead matches the first word of a comment line.
var docLead = regexp.MustCompile(`^//\s*([A-Za-z_]\w*)\b`)

// mention matches a name written as a call inside a comment line.
//
// The bracket is against the name, the way a call is written: prose has "all (of which)"
// and "before (the message)", and a guard that also reports those is a guard that gets
// switched off. Only a name that begins in lower case is looked at: a capitalised word
// before a bracket in prose is as likely to be an acronym or an external API as a
// function.
var mention = regexp.MustCompile(`(?:^|[^.\w])([a-z][A-Za-z0-9_]*)\(`)

// aWord matches one identifier.
var aWord = regexp.MustCompile(`[A-Za-z_]\w*`)

// looksLikeAName matches a word that starts in lower case and has a capital later on,
// which is what an unexported Go name looks like and what English prose rarely does.
var looksLikeAName = regexp.MustCompile(`^[a-z][A-Za-z0-9_]*[A-Z]`)

// builtins are the names the language itself provides, which are declared nowhere.
var builtins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
	"bool": true, "byte": true, "error": true, "float32": true, "float64": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"rune": true, "string": true, "uint": true, "uint8": true, "uint16": true,
	"uint32": true, "uint64": true, "uintptr": true,
}

// TestDocCommentsNameTheThingTheyDocument covers one half of comment drift: a comment
// that names something which is no longer in the code.
//
// The comments in this codebase carry the reasoning -- why a check is where it is, what
// was measured, what would break if it moved -- so a comment naming something that is not
// there sends the next reader looking for the reasoning that went with it. Two of these
// were found by hand: a doc comment for one function carried a leftover line naming the
// function it used to be, in both cases a rename that the comment did not follow.
//
// A line is looked at when it could be a name and it opens a paragraph of the comment
// above a declaration -- which is how this codebase documents a parameter as well as a
// function ("onUnreadable is what an unreadable section counts as"). Both forms are
// legitimate, so what is reported is only a name that appears nowhere in the code.
func TestDocCommentsNameTheThingTheyDocument(t *testing.T) {
	known, sources := codeWords(t)

	for _, path := range sources {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		lines := strings.Split(string(source), "\n")
		for i, line := range lines {
			found := declarationLine.FindStringSubmatch(line)
			if found == nil {
				continue
			}
			// The comment is the unbroken run of comment lines directly above.
			start := i
			for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "//") {
				start--
			}
			// Every line of it, not just the first: one of the two repaired here was a
			// leftover line in the middle of a block whose opening line was correct.
			for at := start; at < i; at++ {
				lead := docLead.FindStringSubmatch(strings.TrimSpace(lines[at]))
				if lead == nil {
					continue
				}
				name := lead[1]
				if name == found[1] || known[name] || builtins[name] ||
					!looksLikeAName.MatchString(name) {
					continue
				}
				t.Errorf("%s:%d: the comment above %s opens a paragraph with %s, which "+
					"appears nowhere in this module's code. A paragraph that opens with a "+
					"name describes that name, so this one has outlived a rename: either "+
					"it names what is here, or the code it described is gone",
					filepath.Base(path), at+1, found[1], name)
			}
		}
	}
}

// TestCommentsNameThingsThatExist covers the other half: a comment that names something
// as a call which is not there to be called.
//
// It reads the source rather than behaviour, for the same reason the registration guard
// does: what it has to catch is a mistake in a comment, and no amount of running the bot
// reaches a comment.
func TestCommentsNameThingsThatExist(t *testing.T) {
	known, sources := codeWords(t)

	unknown := map[string][]string{}
	for _, path := range sources {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, line := range strings.Split(string(source), "\n") {
			comment, ok := strings.CutPrefix(strings.TrimSpace(line), "//")
			if !ok {
				continue
			}
			for _, found := range mention.FindAllStringSubmatch(comment, -1) {
				name := found[1]
				if known[name] || builtins[name] {
					continue
				}
				unknown[name] = append(unknown[name], filepath.Base(path))
			}
		}
	}

	names := make([]string, 0, len(unknown))
	for name := range unknown {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		files := dedupe(unknown[name])
		t.Errorf("comments name %s(), which appears nowhere in this module's code (in "+
			"%s). A comment that points at a function that is not there sends the next "+
			"reader looking for the reasoning that went with it: either the name is "+
			"wrong or the comment no longer describes the code",
			name, strings.Join(files, ", "))
	}
}

// codeWords returns every identifier that appears in code anywhere in the module, and
// every source file it read.
//
// Identifiers are collected from the code rather than from declarations, because a doc
// comment documents a parameter, a field and a local as well as a function (see the
// comment on the test above). What the two tests need to tell apart is a name that exists
// somewhere in the code -- which a comment is entitled to point at -- from a name that
// exists only in a comment.
func codeWords(t *testing.T) (map[string]bool, []string) {
	t.Helper()
	words := map[string]bool{}
	var sources []string

	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sources = append(sources, path)
		for _, line := range strings.Split(string(source), "\n") {
			code, _, _ := strings.Cut(line, "//")
			for _, found := range aWord.FindAllString(code, -1) {
				words[found] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	sort.Strings(sources)
	return words, sources
}

// dedupe keeps one of each name, in order.
func dedupe(names []string) []string {
	sort.Strings(names)
	kept := names[:0]
	for i, name := range names {
		if i == 0 || names[i-1] != name {
			kept = append(kept, name)
		}
	}
	return kept
}
