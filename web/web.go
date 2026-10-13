// Package web carries the management console's compiled front end.
//
// The front end is source in this directory -- Vite and TypeScript, see
// README.md next to it -- and dist/ is what that source compiles to. Only dist
// is embedded, so what ships is what was built, and a checkout that has not been
// built still compiles: dist/.gitkeep keeps the embed pattern matching.
//
// The package exists because go:embed cannot reach outside the directory of the
// file that uses it. The HTTP service lives in internal/httpapi and cannot embed
// web/dist itself, so this is the seam: one package that owns the directory and
// hands it over as a file system.
package web

import (
	"embed"
	"io/fs"
)

// dist is the compiled front end.
//
// all: rather than the default, so that files whose names begin with an
// underscore or a dot are embedded too -- Vite writes none today, but a build
// that started to would otherwise ship a page that references a file the binary
// does not have.
//
//go:embed all:dist
var dist embed.FS

// Dist returns the compiled front end, rooted so that the files are addressed
// the way the HTTP service serves them: index.html at the root rather than under
// a dist/ prefix.
//
// It is a function rather than the variable so that callers cannot walk into
// dist/ by accident and end up with two ways to name the same file.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Unreachable: the pattern above is a compile-time guarantee that dist
		// exists. Reported as a panic rather than an empty file system, because an
		// empty one would serve 404s that look like a routing bug.
		panic("web: dist is not in the embedded file system: " + err.Error())
	}
	return sub
}
