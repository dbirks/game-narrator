package assets

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

// testCtx returns a context with no deadline: these tests assert filesystem
// behaviour, and a deadline would make a timeout indistinguishable from a bug.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

// newStore returns a Local rooted in a fresh t.TempDir(), removed with the
// test. Nothing in this package's tests writes outside a temporary directory.
func newStore(t *testing.T, opts ...Option) *Local {
	t.Helper()
	root := filepath.Join(t.TempDir(), "assets")
	l, err := NewLocal(root, opts...)
	if err != nil {
		t.Fatalf("NewLocal(%s): %v", root, err)
	}
	return l
}

// mustStage stages content, failing the test if staging fails.
func mustStage(t *testing.T, l *Local, content []byte, expected string) (StagingID, Info) {
	t.Helper()
	id, info, err := l.PutStaged(testCtx(t), bytesReader(content), expected)
	if err != nil {
		t.Fatalf("PutStaged: %v", err)
	}
	return id, info
}

// commitPut stages and commits content, returning the committed Info.
func commitPut(t *testing.T, l *Local, content []byte) Info {
	t.Helper()
	id, _ := mustStage(t, l, content, "")
	info, err := l.Commit(testCtx(t), id)
	if err != nil {
		t.Fatalf("Commit(%s): %v", id, err)
	}
	return info
}

// commitStage commits an already-staged ID.
func commitStage(t *testing.T, l *Local, id StagingID) Info {
	t.Helper()
	info, err := l.Commit(testCtx(t), id)
	if err != nil {
		t.Fatalf("Commit(%s): %v", id, err)
	}
	return info
}

// bytesReader hands content to the store as a stream. The content itself is
// small in most tests; the large-blob test builds its own reader.
func bytesReader(content []byte) io.Reader { return strings.NewReader(string(content)) }

// contentWithHashPrefix searches for content whose SHA-256 begins with prefix,
// so a test can place a blob at a chosen fan-out path (used to prove a linked
// directory cannot redirect a commit). Average work is 16^len(prefix) hashes.
func contentWithHashPrefix(t *testing.T, prefix string) []byte {
	t.Helper()
	for i := 0; i < 1_000_000; i++ {
		c := []byte("gn-asset-fixture-" + strconv.Itoa(i))
		if strings.HasPrefix(hashOf(c), prefix) {
			return c
		}
	}
	t.Fatalf("no content hashing to prefix %q found", prefix)
	return nil
}

func hashOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// blobPath mirrors the on-disk layout so tests can tamper with the tree the way
// a fault or an attacker would, instead of reaching for internal helpers.
func blobPath(t *testing.T, l *Local, hash string) string {
	t.Helper()
	return filepath.Join(l.Root(), "blobs", hash[:2], hash[2:4], hash)
}

func stagingPath(t *testing.T, l *Local, id StagingID) string {
	t.Helper()
	return filepath.Join(l.Root(), "staging", string(id))
}

func stagingFiles(t *testing.T, l *Local) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(l.Root(), "staging"))
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// treePaths lists every path under root, relative and sorted, so a test can
// assert that a rejected call left the filesystem exactly as it found it.
func treePaths(t *testing.T, root string) []string {
	t.Helper()
	if _, err := os.Lstat(root); err != nil {
		return nil
	}
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// blobHashes lists committed blobs by hash by walking the tree — the same walk
// DeleteUnreachable performs, so a test that disagrees with the store about what
// exists is caught here.
func blobHashes(t *testing.T, l *Local) []string {
	t.Helper()
	root := filepath.Join(l.Root(), "blobs")
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if h, ok := l.blobHashAt(p); ok {
			out = append(out, h)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk blobs: %v", err)
	}
	sort.Strings(out)
	return out
}

func modePerm(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", path, err)
	}
	return fi.Mode().Perm()
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

// deterministicReader yields exactly size bytes from a reproducible stream, so
// the same content can be hashed once through the store and once directly
// without either side holding it in memory.
func deterministicReader(seed uint64, size int64) io.Reader {
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:8], seed)
	rng := rand.NewChaCha8(key)
	return io.LimitReader(rng, size)
}

// hashStream reports the digest and length of a reader without buffering more
// than one chunk.
func hashStream(t *testing.T, r io.Reader) (hash string, size int64) {
	t.Helper()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		t.Fatalf("hash stream: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n
}

// oneByteErrReader yields nothing but an error, to prove failures propagate
// out of staging rather than being swallowed as a short write.
func oneByteErrReader() io.Reader { return iotest.ErrReader(errBoom) }

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "assets test: reader failed" }
