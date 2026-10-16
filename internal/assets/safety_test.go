package assets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// badHashes are the shapes docs/spec/14 "Filesystem/import" worries about, plus
// the trivially wrong lengths. A hash doubles as a path component, so anything
// that is not exactly 64 lowercase hex characters must be refused before the
// filesystem is touched.
var badHashes = []struct {
	name string
	hash string
}{
	{"path traversal", "../x"},
	{"nested traversal", "../../../../etc/passwd"},
	{"embedded traversal", "ab/cd/../../ef01234567890123456789012345678901234567890123456789012345"},
	{"uppercase", strings.ToUpper(hashOf([]byte("content")))},
	{"too short", hashOf([]byte("content"))[:63]},
	{"too long", hashOf([]byte("content")) + "00"},
	{"dot dot", ".."},
	{"absolute", "/tmp/x"},
	{"non hex", strings.Repeat("g", 64)},
	{"separator", strings.Repeat("a", 32) + "/" + strings.Repeat("b", 32)},
	{"nul byte", strings.Repeat("a", 63) + "\x00"},
	{"dot segment", "." + strings.Repeat("a", 63)},
	{"space padded", " " + hashOf([]byte("content"))},
}

// TestInvalidHashRejectedWithoutFilesystemAccess: validation precedes I/O for
// every method that takes a hash.
func TestInvalidHashRejectedWithoutFilesystemAccess(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	before := treePaths(t, l.Root())

	for _, tc := range badHashes {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := l.Get(ctx, tc.hash); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("Get(%q): err = %v, want ErrInvalidHash", tc.hash, err)
			}
			if _, err := l.Stat(ctx, tc.hash); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("Stat(%q): err = %v, want ErrInvalidHash", tc.hash, err)
			}
			if err := l.Verify(ctx, tc.hash); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("Verify(%q): err = %v, want ErrInvalidHash", tc.hash, err)
			}
			if err := l.Export(ctx, []string{tc.hash}, &discardWriter{}); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("Export(%q): err = %v, want ErrInvalidHash", tc.hash, err)
			}
			if _, _, err := l.PutStaged(ctx, strings.NewReader("x"), tc.hash); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("PutStaged(expected %q): err = %v, want ErrInvalidHash", tc.hash, err)
			}
			if got := treePaths(t, l.Root()); strings.Join(got, ",") != strings.Join(before, ",") {
				t.Fatalf("filesystem changed for %q:\n got %v\nwant %v", tc.hash, got, before)
			}
		})
	}
}

// TestEmptyExpectedHashMeansUnknown is the other half of the rule: "" is not an
// invalid hash, it is the documented "caller does not know" sentinel, so the
// store must accept it and report the address it computed itself.
func TestEmptyExpectedHashMeansUnknown(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	content := []byte("content staged without an expectation")
	id, info, err := l.PutStaged(ctx, bytesReader(content), "")
	if err != nil {
		t.Fatalf("PutStaged with empty expected hash: %v", err)
	}
	if info.SHA256 != hashOf(content) {
		t.Fatalf("computed SHA256 = %s, want %s", info.SHA256, hashOf(content))
	}
	if err := l.Abort(ctx, id); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got := stagingFiles(t, l); len(got) != 0 {
		t.Fatalf("staging after Abort = %v, want empty", got)
	}
}

type discardWriter struct{}

func (*discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestValidHashAcceptsExactlyLowercase64 documents the positive side of the rule.
func TestValidHashAcceptsExactlyLowercase64(t *testing.T) {
	if !ValidHash(hashOf([]byte("x"))) {
		t.Fatal("ValidHash rejected a genuine digest")
	}
	if ValidHash(strings.ToUpper(hashOf([]byte("x")))) {
		t.Fatal("ValidHash accepted uppercase")
	}
}

// TestInvalidStagingIDRejected: IDs become paths under staging/, so the same rule
// applies to them.
func TestInvalidStagingIDRejected(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	before := treePaths(t, l.Root())
	for _, id := range []StagingID{"", "..", "../../etc/passwd", "deadbeef", StagingID(strings.Repeat("A", 32)), StagingID(strings.Repeat("z", 32)), StagingID(strings.Repeat("ab", 15) + "\x00")} {
		if _, err := l.Commit(ctx, id); !errors.Is(err, ErrInvalidStagingID) {
			t.Fatalf("Commit(%q): err = %v, want ErrInvalidStagingID", id, err)
		}
		if err := l.Abort(ctx, id); !errors.Is(err, ErrInvalidStagingID) {
			t.Fatalf("Abort(%q): err = %v, want ErrInvalidStagingID", id, err)
		}
	}
	if got := treePaths(t, l.Root()); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("filesystem changed for invalid staging IDs:\n got %v\nwant %v", got, before)
	}
}

// TestCommitUnknownButWellFormedIDIsNotFound: a well-formed ID that was never
// staged must read as absent, not as a filesystem error.
func TestCommitUnknownButWellFormedIDIsNotFound(t *testing.T) {
	l := newStore(t)
	if _, err := l.Commit(testCtx(t), StagingID(strings.Repeat("ab", 16))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Commit(unknown id): err = %v, want ErrNotFound", err)
	}
}

// TestNewLocalRefusesSymlinkedRoot: pointing the asset store at a link would let
// a configuration value relocate the whole private tree (docs/spec/14).
func TestNewLocalRefusesSymlinkedRoot(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink root: %v", err)
	}

	if _, err := NewLocal(link); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("NewLocal(symlink): err = %v, want ErrUnsafePath", err)
	}

	// The link target must stay untouched: refusing means refusing, not
	// half-initialising through the link.
	if _, err := os.Lstat(filepath.Join(target, "blobs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlinked root was populated through the link: %v", err)
	}

	// A symlink whose target does not exist is still a symlink.
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatalf("symlink dangling: %v", err)
	}
	if _, err := NewLocal(dangling); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("NewLocal(dangling symlink): err = %v, want ErrUnsafePath", err)
	}
}

// TestNewLocalRefusesFileRoot guards against a config value that names a file.
func TestNewLocalRefusesFileRoot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := NewLocal(p); err == nil {
		t.Fatal("NewLocal(file): want error, got nil")
	}
}

// TestGetRefusesSymlinkedBlob: a blob replaced by a link must never be served.
// This is the classic attack on a content-addressed cache that an untrusted
// import can write into.
func TestGetRefusesSymlinkedBlob(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	secret := []byte("the target of the symlink: someone else's file")
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, secret, 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	victim := commitPut(t, l, []byte("victim content"))
	p := blobPath(t, l, victim.SHA256)
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove real blob: %v", err)
	}
	if err := os.Symlink(outside, p); err != nil {
		t.Fatalf("plant symlink blob: %v", err)
	}

	if _, _, err := l.Get(ctx, victim.SHA256); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Get(symlinked blob): err = %v, want ErrUnsafePath", err)
	}
	if _, err := l.Stat(ctx, victim.SHA256); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Stat(symlinked blob): err = %v, want ErrUnsafePath", err)
	}
	if err := l.Verify(ctx, victim.SHA256); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Verify(symlinked blob): err = %v, want ErrUnsafePath", err)
	}
	// Commit must refuse to land a new blob on a symlink too, which would be an
	// arbitrary-file write.
	id, _ := mustStage(t, l, []byte("victim content"), victim.SHA256)
	if _, err := l.Commit(ctx, id); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Commit over symlink: err = %v, want ErrUnsafePath", err)
	}
	// The link target is intact and unread through the store.
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read outside file: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatalf("outside file modified: %q", got)
	}
}

// TestCheckChainRefusesSymlinkedFanoutDir covers the intermediate components: a
// linked ab/ directory must not redirect reads or writes either.
func TestCheckChainRefusesSymlinkedFanoutDir(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	info := commitPut(t, l, []byte("blob under a linked directory"))
	p := blobPath(t, l, info.SHA256)

	twoChar := filepath.Dir(filepath.Dir(p)) // blobs/ab
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatalf("create elsewhere: %v", err)
	}
	if err := os.Rename(twoChar, filepath.Join(elsewhere, filepath.Base(twoChar))); err != nil {
		t.Fatalf("move fan-out dir out: %v", err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, filepath.Base(twoChar)), twoChar); err != nil {
		t.Fatalf("link fan-out dir: %v", err)
	}

	if _, _, err := l.Get(ctx, info.SHA256); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Get under linked dir: err = %v, want ErrUnsafePath", err)
	}
	if _, err := l.Stat(ctx, info.SHA256); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Stat under linked dir: err = %v, want ErrUnsafePath", err)
	}
	// A commit of other content must not escape through the linked directory.
	// The content is chosen so its own fan-out path runs through blobs/ab, the
	// component that is now a link.
	other := contentWithHashPrefix(t, info.SHA256[:2])
	id, otherInfo := mustStage(t, l, other, "")
	if _, err := l.Commit(ctx, id); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Commit under linked dir: err = %v, want ErrUnsafePath", err)
	}
	if _, err := os.Lstat(blobPath(t, l, otherInfo.SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("commit escaped through a symlinked directory: %v", err)
	}
}

// TestBlobReplacedByDirectoryIsRefused keeps a planted directory from being read
// as content.
func TestBlobReplacedByDirectoryIsRefused(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	info := commitPut(t, l, []byte("content to be replaced by a directory"))
	p := blobPath(t, l, info.SHA256)
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatalf("plant directory at blob path: %v", err)
	}
	if _, _, err := l.Get(ctx, info.SHA256); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Get(directory at blob path): err = %v, want ErrUnsafePath", err)
	}
}

// TestMaxSizeAbortsStage enforces the upload/decompression limit at staging time.
func TestMaxSizeAbortsStage(t *testing.T) {
	const limit = 1 << 20 // 1 MiB
	l := newStore(t, WithMaxSize(limit))
	ctx := testCtx(t)

	before := treePaths(t, l.Root())
	_, _, err := l.PutStaged(ctx, deterministicReader(11, 4*limit), "")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("PutStaged over limit: err = %v, want ErrTooLarge", err)
	}
	if got := treePaths(t, l.Root()); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("oversized stage left files behind:\n got %v\nwant %v", got, before)
	}

	// Exactly at the limit is allowed — the limit is inclusive, so a caller who
	// knows the size of a legitimate asset is not cut off by one byte.
	atLimit := deterministicReader(12, limit)
	id, info, err := l.PutStaged(ctx, atLimit, "")
	if err != nil {
		t.Fatalf("PutStaged at limit: %v", err)
	}
	if info.Size != limit {
		t.Fatalf("Size at limit = %d, want %d", info.Size, limit)
	}
	commitStage(t, l, id)

	// One byte over is not.
	if _, _, err := l.PutStaged(ctx, deterministicReader(13, limit+1), ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("PutStaged limit+1: err = %v, want ErrTooLarge", err)
	}
}

// TestMaxSizeUnlimitedByDefault documents that the cap is opt-in.
func TestMaxSizeUnlimitedByDefault(t *testing.T) {
	l := newStore(t)
	_, info, err := l.PutStaged(testCtx(t), deterministicReader(14, 3<<20), "")
	if err != nil {
		t.Fatalf("PutStaged 3 MiB with no limit: %v", err)
	}
	if info.Size != 3<<20 {
		t.Fatalf("Size = %d, want %d", info.Size, 3<<20)
	}
}

func TestNewLocalRejectsNegativeMaxSize(t *testing.T) {
	if _, err := NewLocal(filepath.Join(t.TempDir(), "assets"), WithMaxSize(-1)); err == nil {
		t.Fatal("NewLocal(WithMaxSize(-1)): want error, got nil")
	}
}

func TestNewLocalRequiresRoot(t *testing.T) {
	if _, err := NewLocal(""); err == nil {
		t.Fatal("NewLocal(\"\"): want error, got nil")
	}
}
