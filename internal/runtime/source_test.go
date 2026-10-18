package runtime_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/runtime"
)

// TestStartValidation covers the request gate: every bad shape is refused
// before the session exists, with the sentinel the API maps to a problem type.
func TestStartValidation(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t, withFakeOCR(map[string]string{}, 0))
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	clip := syntheticClipDir()
	cases := []struct {
		name   string
		req    runtime.StartRequest
		wants  error
		needle string
	}{
		{
			"unknown profile",
			runtime.StartRequest{ProfileID: "01H0000000000000000000000Y", FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: clip}},
			runtime.ErrProfileNotFound, "",
		},
		{
			"invalid mode",
			runtime.StartRequest{ProfileID: profileID, Mode: "yolo", FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: clip}},
			runtime.ErrInvalidMode, "",
		},
		{
			"unknown source kind",
			runtime.StartRequest{ProfileID: profileID, FrameSource: runtime.FrameSource{Kind: "magic"}},
			runtime.ErrUnknownSourceKind, "",
		},
		{
			"v4l2 not implemented",
			runtime.StartRequest{ProfileID: profileID, FrameSource: runtime.FrameSource{Kind: runtime.SourceKindV4L2}},
			runtime.ErrSourceUnsupported, "",
		},
		{
			"dolphin not implemented",
			runtime.StartRequest{ProfileID: profileID, FrameSource: runtime.FrameSource{Kind: runtime.SourceKindDolphin}},
			runtime.ErrSourceUnsupported, "",
		},
		{
			// The manual seam is unwired in this manager (nil closure), so the
			// kind is refused exactly like the unimplemented live kinds.
			"manual source unwired",
			runtime.StartRequest{ProfileID: profileID, FrameSource: runtime.FrameSource{Kind: runtime.SourceKindManual}},
			runtime.ErrSourceUnsupported, "",
		},
		{
			"replay dir outside allowlist",
			runtime.StartRequest{ProfileID: profileID, FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: "/etc"}},
			runtime.ErrSourceNotAllowed, "",
		},
		{
			"replay traversal above the root",
			runtime.StartRequest{ProfileID: profileID, FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: traversalClip(t)}},
			runtime.ErrSourceNotAllowed, "",
		},
		{
			"replay dir missing",
			runtime.StartRequest{ProfileID: profileID, FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: filepath.Join(clip, "no-such-clip")}},
			runtime.ErrSourceMissing, "",
		},
		{
			"replay without a dir",
			runtime.StartRequest{ProfileID: profileID, FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay}},
			nil, "requires a dir",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.start(tc.req)
			if err == nil {
				t.Fatalf("start succeeded, want an error")
			}
			if tc.wants != nil {
				if !errors.Is(err, tc.wants) {
					t.Fatalf("start = %v, want %v", err, tc.wants)
				}
			} else if !strings.Contains(err.Error(), tc.needle) {
				t.Fatalf("start = %q, want a message containing %q", err, tc.needle)
			}
		})
	}
}

// TestDolphinSourceAdmission pins the dev-only gate on the dolphin frame
// source (docs/spec/11, ADR-0007): the kind is refused without the opt-in
// (the "dolphin not implemented" case above), refused with the opt-in but no
// bridge closure wired into the manager (the production shape — the runtime
// never launches the emulator itself), and only admitted when both are
// present, at which point the closure is the one seam that hands a running
// bridge's frame source to the session.
func TestDolphinSourceAdmission(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t, withFakeOCR(map[string]string{}, 0))
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	// The two packages each own a copy of the opt-in name (the core runtime
	// keeps no dependency on the dev-only package); a typo in either would
	// make the opt-in a silent no-op, so the names are pinned equal here.
	if runtime.EnvDevDolphin != dolphin.EnvDevDolphin {
		t.Fatalf("the opt-in names disagree: runtime=%q dolphin=%q", runtime.EnvDevDolphin, dolphin.EnvDevDolphin)
	}

	// Opt-in set, no closure wired: still refused — the opt-in alone must
	// not admit the kind.
	m := mustManagerWithDolphin(t, e, nil)
	_, err := m.Start(context.Background(), runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindDolphin},
	})
	if !errors.Is(err, runtime.ErrSourceUnsupported) {
		t.Fatalf("opt-in without a wired closure = %v, want ErrSourceUnsupported", err)
	}

	// Opt-in set and the closure wired: the closure is consulted — the error
	// it returns is the one Start surfaces, proving the hand-off seam.
	bridgeErr := errors.New("dolphin-bridge-not-running")
	m = mustManagerWithDolphin(t, e, func() (capture.Source, error) { return nil, bridgeErr })
	_, err = m.Start(context.Background(), runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindDolphin},
	})
	if !errors.Is(err, bridgeErr) {
		t.Fatalf("the wired closure's error was not surfaced: %v, want %v", err, bridgeErr)
	}
}

// mustManagerWithDolphin builds a manager with the dev opt-in set in the
// environment and the given bridge closure (nil = unwired, the production
// shape).
func mustManagerWithDolphin(t *testing.T, e *env, dev func() (capture.Source, error)) *runtime.Manager {
	t.Helper()
	t.Setenv(runtime.EnvDevDolphin, "1")
	m, err := runtime.New(runtime.Options{
		Store:       e.st,
		Hub:         e.hub,
		AssetsRoot:  e.assets,
		ReplayRoots: e.roots,
		OCR:         e.ocr,
		DevDolphin:  dev,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// TestDevTraceUnavailable is the honest 503: a runtime without a configured
// assets root cannot honour dev_trace, so the refusal happens at start — a
// session that pretended to trace but stored nothing would be a lie.
func TestDevTraceUnavailable(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t, withFakeOCR(map[string]string{}, 0))
	e.mgr = mustManagerWithoutAssets(t, e)
	profileID, _ := seedSyntheticProfile(t, e.st, true)
	_, err := e.start(runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: syntheticClipDir()},
		DevTrace:    true,
	})
	if !errors.Is(err, runtime.ErrDevTraceUnavailable) {
		t.Fatalf("start = %v, want ErrDevTraceUnavailable", err)
	}
}

// TestUnpublishedRevisionIsRefused proves the "published only" rule on a row
// that exists: a draft with full content, no publication.
func TestUnpublishedRevisionIsRefused(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t, withFakeOCR(map[string]string{}, 0))
	draftID, _ := seedSyntheticProfile(t, e.st, false)
	_, err := e.start(runtime.StartRequest{
		ProfileID:   draftID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: syntheticClipDir()},
	})
	if !errors.Is(err, runtime.ErrProfileNotPublished) {
		t.Fatalf("start = %v, want ErrProfileNotPublished", err)
	}
}

// TestDefaultReplayRoots pins the allowlist default: the env var wins, and
// without it the repo-relative testdata/ is admitted only when the process is
// running out of a directory that has one (a dev context) — nothing in
// production, where no such directory exists.
func TestDefaultReplayRoots(t *testing.T) {
	t.Run("env var wins", func(t *testing.T) {
		got := runtime.DefaultReplayRoots(func(k string) (string, bool) {
			if k == runtime.EnvReplayRoots {
				return " /a/b , /c ,", true
			}
			return "", false
		})
		if len(got) != 2 || got[0] != "/a/b" || got[1] != "/c" {
			t.Fatalf("roots = %v, want [/a/b /c]", got)
		}
	})
	t.Run("empty env var means no roots", func(t *testing.T) {
		got := runtime.DefaultReplayRoots(func(k string) (string, bool) {
			if k == runtime.EnvReplayRoots {
				return "  ", true
			}
			return "", false
		})
		if len(got) != 0 {
			t.Fatalf("roots = %v, want none", got)
		}
	})
	t.Run("dev context admits testdata", func(t *testing.T) {
		got := defaultRootsFrom(absPath(t, repoRoot))
		if len(got) != 1 || filepath.Base(got[0]) != "testdata" {
			t.Fatalf("roots from repo root = %v, want [repo/testdata]", got)
		}
	})
	t.Run("production context admits nothing", func(t *testing.T) {
		if got := defaultRootsFrom(t.TempDir()); len(got) != 0 {
			t.Fatalf("roots from empty dir = %v, want none", got)
		}
	})
}

// TestSymlinkEscapeRefused builds a replay root containing a symlink that
// points outside it and proves the allowlist is checked on the fully resolved
// path: the clip is refused even though its lexical path sits inside the root.
func TestSymlinkEscapeRefused(t *testing.T) {
	corpusPresent(t)
	base := t.TempDir()
	root := filepath.Join(base, "allowed")
	outside := filepath.Join(base, "outside-clip")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "sneaky")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	e := newEnv(t, withFakeOCR(map[string]string{}, 0))
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	_, err := e.start(runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: filepath.Join(root, "sneaky")},
	})
	if !errors.Is(err, runtime.ErrSourceNotAllowed) {
		t.Fatalf("start through symlink = %v, want ErrSourceNotAllowed", err)
	}
}

// traversalClip builds base/allowed and base/secret and returns the path
// base/allowed/../secret: a dot-dot path that lexically "escapes" the root to
// a real directory outside it. The allowlist must refuse it on the resolved
// path, not on the spelling.
func traversalClip(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "allowed")
	secret := filepath.Join(base, "secret")
	for _, d := range []string{root, secret} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "..", "secret")
}

func defaultRootsFrom(dir string) []string {
	old, err := os.Getwd()
	if err != nil {
		return nil
	}
	if err := os.Chdir(dir); err != nil {
		return nil
	}
	defer os.Chdir(old)
	return runtime.DefaultReplayRoots(os.LookupEnv)
}

func mustManagerWithoutAssets(t *testing.T, e *env) *runtime.Manager {
	t.Helper()
	m, err := runtime.New(runtime.Options{
		Store:       e.st,
		Hub:         e.hub,
		AssetsRoot:  "",
		ReplayRoots: e.roots,
		OCR:         e.ocr,
	})
	if err != nil {
		t.Fatalf("New without assets root: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}
