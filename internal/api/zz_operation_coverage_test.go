package api

// The operation-coverage gate (ADR-0017 clause (c)) lives in a file that
// sorts after every other test file: the gate reads the contract hits the
// other tests recorded, and Go runs tests in file order, so the gate must
// run last. When it lived in profiles_test.go, any route whose exercising
// tests live alphabetically after "profiles" (speakerlink's did, from
// speakerlink_test.go) failed the gate in full-package runs even though the
// tests that exercise it ran fine — they simply ran after it.

import (
	"strings"
	"testing"
)

// TestEveryCatalogOperationWasExercised makes clause (c) a coverage claim rather
// than a hope: each operation this milestone added must have had a real success
// body validated at least once. A route that no test reached fails here even if
// the document describes it perfectly.
func TestEveryCatalogOperationWasExercised(t *testing.T) {
	// The flows live in the other test files of this package (catalog,
	// import/export, disc import, speaker-link, …); running only a subset
	// with -run would report the others as missing, so the dependency is
	// stated rather than hidden — and this file's name is the ordering
	// statement (see the file comment).
	seen := map[string]bool{}
	contractMu.Lock()
	for _, hit := range contractHits {
		seen[hit.op] = true
	}
	contractMu.Unlock()
	if len(seen) == 0 {
		t.Skip("no catalog flow ran in this invocation (go test -run selects a subset)")
	}

	a := newTestAuth(t)
	s := New(testBuild, testStarted, discardLogger, WithWebAssets(uiBuild()), WithAuth(a.svc, a.repo))
	s.Handler()

	for _, rt := range s.routes {
		if !milestoneCatalogRoute(rt.method, rt.path) {
			continue
		}
		key := rt.method + " " + rt.path
		if !seen[key] {
			t.Errorf("operation %s produced no validated success body in these tests: the contract covers what the handlers did, and nothing exercised it", key)
		}
	}
}

// milestoneCatalogRoute selects the operations ADR-0017 added.
func milestoneCatalogRoute(method, path string) bool {
	switch {
	case strings.HasPrefix(path, "/api/v1/games"),
		strings.HasPrefix(path, "/api/v1/editions"),
		strings.HasPrefix(path, "/api/v1/profiles"),
		path == openAPIPath:
		return true
	}
	_ = method
	return false
}
