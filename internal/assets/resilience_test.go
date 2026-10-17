package assets

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// env keys for the namespace re-exec pattern: the parent mounts a size-limited
// tmpfs it does not own and re-runs the test binary inside it, because a
// non-root user can still get a real full filesystem via user+mount namespaces
// (unshare -Urm) — the honest version of "disk full" for this contract.
const (
	diskFullEnv      = "GN_ASSETS_DISKFULL"
	diskFullRootEnv  = "GN_ASSETS_DISKFULL_ROOT"
	diskFullChildTag = "resilience: disk_full"
)

// TestDiskFullAssetsRefusesWritesWithoutCorruption is docs/spec/13's "Disk
// full" row for the asset store: when the filesystem cannot take more bytes,
// the failing write returns a TYPED error (ENOSPC, switchable via errors.Is),
// a partial asset never becomes visible under its final name, earlier
// committed data is intact, and reclamation (the eviction primitive Delete)
// recovers the store.
//
// Primary mode: a genuine ENOSPC from a real full filesystem — a ~20 MiB
// tmpfs mounted by the parent inside user+mount namespaces, so no root is
// needed and no fake error is involved. Fallback mode (namespaces blocked by
// the environment): a fault-injecting reader that fails with ENOSPC after N
// bytes, exercising the identical store code path (stream → error → cleanup).
func TestDiskFullAssetsRefusesWritesWithoutCorruption(t *testing.T) {
	if os.Getenv(diskFullEnv) == "1" {
		root := os.Getenv(diskFullRootEnv)
		if root == "" {
			t.Fatalf("%s=1 but %s is empty", diskFullEnv, diskFullRootEnv)
		}
		runDiskFullScenario(t, root, func() io.Reader {
			// The store root lives on the size-limited mount: the OS itself
			// supplies the ENOSPC once the stream out-runs the filesystem.
			const size = 25 << 20
			return deterministicReader(1, size)
		}, "real-enospc")
		return
	}

	out, err := runInLimitedTmpfs(t)
	switch {
	case err == nil:
		s := string(out)
		if !strings.Contains(s, diskFullChildTag) {
			t.Fatalf("namespaced child did not report the resilience outcome:\n%s", s)
		}
		t.Log(strings.TrimSpace(s))
	case strings.Contains(string(out), "--- FAIL") || strings.Contains(string(out), "panic:"):
		t.Fatalf("namespaced child failed (not an environment limitation):\n%s", out)
	default:
		t.Logf("real tmpfs unavailable (%v); running the fault-writer fallback", err)
		runDiskFullScenario(t, filepath.Join(t.TempDir(), "assets"), func() io.Reader {
			// The disk "fills up" after this many bytes: the store has already
			// written the committed baseline, so the failure lands mid-stream
			// exactly as a full disk would.
			const size = 1 << 20
			return &enospcAfter{r: deterministicReader(1, size), budget: size / 2}
		}, "fault-writer")
	}
}

// runInLimitedTmpfs mounts a ~20 MiB tmpfs in fresh user+mount namespaces and
// re-runs this test binary inside it. Returns the child's combined output.
func runInLimitedTmpfs(t *testing.T) ([]byte, error) {
	t.Helper()
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		return nil, errors.New("unshare not found")
	}
	prog, err := os.Executable()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, unshare, "-Urm", "sh", "-c",
		"mount -t tmpfs -o size=20M tmpfs /mnt && exec \"$1\" -test.run '^TestDiskFullAssetsRefusesWritesWithoutCorruption$' -test.count 1 -test.v -test.timeout 90s",
		"gn-reexec", prog)
	cmd.Env = append(os.Environ(), diskFullEnv+"=1", diskFullRootEnv+"=/mnt/gn-assets")
	return cmd.CombinedOutput()
}

// runDiskFullScenario is the shared contract: committed data survives, the
// failing write is typed, nothing partial is visible, and eviction recovers.
func runDiskFullScenario(t *testing.T, root string, fill func() io.Reader, mode string) {
	t.Helper()
	ctx := testCtx(t)
	l, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal(%s): %v", root, err)
	}

	// Committed data that must survive the failure (a voice-like reference and
	// an imported source blob — the two categories the matrix says must not
	// be corrupted).
	voice := commitPut(t, l, bytes.Repeat([]byte("approved voice reference alpha | "), 2048))  // 64 KiB
	source := commitPut(t, l, bytes.Repeat([]byte("imported game source blob beta | "), 2048)) // 64 KiB

	// Fill the store until the filesystem (or the injected budget) gives up.
	stream := fill()
	_, _, err = l.PutStaged(ctx, stream, "")
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("PutStaged when full: err = %v, want a typed error wrapping ENOSPC", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("context ended unexpectedly: %v", err)
	}

	// A partial asset never becomes visible under its final name, and the
	// failed staging file is discarded, not left to accumulate.
	if got := stagingFiles(t, l); len(got) != 0 {
		t.Fatalf("staging after a failed full-disk write = %v, want empty (the store must clean up)", got)
	}
	blobs := blobHashes(t, l)
	if len(blobs) != 2 || !contains(blobs, voice.SHA256) || !contains(blobs, source.SHA256) {
		t.Fatalf("blob tree after failure = %v, want exactly the two committed blobs", blobs)
	}

	// Earlier committed data is intact.
	if err := l.Verify(ctx, voice.SHA256); err != nil {
		t.Fatalf("Verify committed voice blob after disk-full: %v", err)
	}
	if err := l.Verify(ctx, source.SHA256); err != nil {
		t.Fatalf("Verify committed source blob after disk-full: %v", err)
	}

	// Recovery: evicting committed TRANSIENT-eligible blobs reclaims space and
	// the store accepts new writes again. (Which blobs a caller may evict is
	// the caller's reachability decision — here the test evicts the two it
	// owns; the safety argument for the speech cache's own entries lives in
	// EvictTransient's contract.)
	if err := l.Delete(ctx, source.SHA256); err != nil {
		t.Fatalf("Delete to reclaim space: %v", err)
	}
	if err := l.Delete(ctx, voice.SHA256); err != nil {
		t.Fatalf("Delete to reclaim space: %v", err)
	}
	recovered := commitPut(t, l, []byte("written after eviction reclaimed space"))
	if _, _, err := l.Get(ctx, recovered.SHA256); err != nil {
		t.Fatalf("store did not recover after eviction: %v", err)
	}

	t.Logf("%s assets %s: ENOSPC is typed (errors.Is), partial asset never visible, staging clean, committed data intact, eviction recovered writes", diskFullChildTag, mode)
}

// enospcAfter reads from r until budget bytes have been served, then fails
// every subsequent read with ENOSPC — the store-side shape of a disk that
// ran out of space mid-stream.
type enospcAfter struct {
	r      io.Reader
	budget int64
	served int64
}

func (e *enospcAfter) Read(p []byte) (int, error) {
	if e.served >= e.budget {
		return 0, syscall.ENOSPC
	}
	n, err := e.r.Read(p)
	e.served += int64(n)
	return n, err
}

func contains(hashes []string, h string) bool {
	for _, x := range hashes {
		if x == h {
			return true
		}
	}
	return false
}
