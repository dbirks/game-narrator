// Package webassets embeds the built React UI into the Go binary
// (docs/spec/09 "Technology", docs/adr/0001-go-react-embedded-ui.md).
//
// `make build` runs the Vite build in web/ and copies web/dist into this
// package's dist/ directory, which is compiled into the binary with
// go:embed. internal/api serves FS() for every non-/api/ path (docs/spec/09
// "Embedded binary test").
//
// A fresh clone carries only dist/.keep, so Built() is false and the API
// answers 503 for UI paths instead of shipping a binary that 404s silently.
package webassets

import (
	"embed"
	"io/fs"
)

// embedded holds the Vite output. The `all:` prefix is required so the
// placeholder file dist/.keep (whose name starts with a dot) is embedded too;
// without it go:embed would match nothing on a fresh clone and fail to build.
//
//go:embed all:dist
var embedded embed.FS

// indexFile is the SPA entry point every deep link falls back to.
const indexFile = "index.html"

// FS returns the embedded build rooted at dist/, so the entry point is the
// plain name "index.html" rather than "dist/index.html".
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		// Unreachable: "dist" is embedded by the directive above.
		panic("webassets: embedded dist directory is missing: " + err.Error())
	}
	return sub
}

// Built reports whether a real Vite build is embedded, i.e. whether
// index.html is present. false means only the clone placeholder shipped.
func Built() bool {
	info, err := fs.Stat(FS(), indexFile)
	return err == nil && !info.IsDir()
}
