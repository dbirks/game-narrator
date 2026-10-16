package webassets

import (
	"io/fs"
	"testing"
)

// The dist directory must contain at least one embeddable file on a fresh
// clone, otherwise `//go:embed all:dist` matches nothing and `go build` fails
// before the UI has ever been built. This is the regression guard for that
// placeholder.
func TestEmbeddedPlaceholderIsPresent(t *testing.T) {
	info, err := fs.Stat(FS(), ".keep")
	if err != nil {
		t.Fatalf("dist/.keep is not embedded (go build on a fresh clone would fail): %v", err)
	}
	if info.IsDir() {
		t.Fatal("dist/.keep is a directory")
	}
}

// FS() must be rooted at dist/, so the entry point is a plain name.
func TestFSIsRootedAtDist(t *testing.T) {
	if _, err := fs.Stat(FS(), "dist/index.html"); err == nil {
		t.Fatal(`FS() exposes "dist/index.html": it is not rooted at dist/`)
	}

	// Built() must agree with what FS() actually contains.
	if _, err := fs.Stat(FS(), indexFile); err == nil {
		if !Built() {
			t.Fatal("Built() = false while index.html is present")
		}
		return
	}
	if Built() {
		t.Fatal("Built() = true while index.html is absent")
	}
}
