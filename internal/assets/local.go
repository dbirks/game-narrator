package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Layout and permission constants (docs/spec/02 "Asset store contract",
// docs/spec/14 "Filesystem/import").
const (
	blobsDirName   = "blobs"
	stagingDirName = "staging"

	dirPerm  = 0o700 // private: assets can be copyrighted user material
	filePerm = 0o600

	// sniffLen is what http.DetectContentType consumes.
	sniffLen = 512
	// copyBufSize bounds per-call streaming memory. Nothing in this package
	// buffers a whole blob.
	copyBufSize = 64 * 1024
)

// Local is the filesystem-backed Store. The layout is
// <root>/blobs/ab/cd/<64-hex> with staged writes in <root>/staging/<id>; the
// two-character fan-out keeps directory sizes sane on every filesystem we
// target while the full hash stays the last component, so an address is still
// the only thing a reader needs.
//
// A Local is safe for concurrent use. Staged entries are process-local; the
// committed blob namespace is the shared one.
type Local struct {
	root    string
	maxSize int64

	mu     sync.Mutex
	staged map[StagingID]Info
}

// Option configures a Local.
type Option func(*Local)

// WithMaxSize caps how large a single blob may be, in bytes. Staging aborts
// with ErrTooLarge as soon as the stream exceeds n, so an import bomb cannot
// fill the disk (docs/spec/14 "Filesystem/import"). n == 0 (the default) means
// unlimited; a negative n is rejected by NewLocal.
func WithMaxSize(n int64) Option {
	return func(l *Local) { l.maxSize = n }
}

// NewLocal prepares a private, content-addressed asset root. It creates root,
// root/blobs and root/staging with mode 0700 and refuses to run if root is (or
// is reached through) a symlink, so a caller cannot be pointed at someone
// else's tree.
func NewLocal(root string, opts ...Option) (*Local, error) {
	if root == "" {
		return nil, errors.New("assets: NewLocal requires a root directory")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("assets: resolve root %s: %w", root, err)
	}
	// Clean, but explicitly not EvalSymlinks: resolving the path would follow
	// exactly the links we refuse.
	abs = filepath.Clean(abs)

	if err := refuseSymlinkRoot(abs); err != nil {
		return nil, err
	}

	l := &Local{root: abs, staged: make(map[StagingID]Info)}
	for _, opt := range opts {
		opt(l)
	}
	if l.maxSize < 0 {
		return nil, fmt.Errorf("assets: WithMaxSize(%d): size limit must be >= 0", l.maxSize)
	}

	if err := l.ensureRoot(abs); err != nil {
		return nil, err
	}
	for _, dir := range []string{filepath.Join(abs, blobsDirName), filepath.Join(abs, stagingDirName)} {
		if err := l.ensurePrivateDir(dir); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// Root reports the absolute asset root.
func (l *Local) Root() string { return l.root }

var _ Store = (*Local)(nil)

// refuseSymlinkRoot checks the root itself. Ancestors are not inspected: a
// parent directory may legitimately be a link (say /var), and refusing it would
// make the store unusable, while everything *under* the root is checked.
func refuseSymlinkRoot(abs string) error {
	fi, err := os.Lstat(abs)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil // created below
	case err != nil:
		return fmt.Errorf("assets: inspect root %s: %w", abs, err)
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("assets: root %s is a symlink: %w", abs, ErrUnsafePath)
	case !fi.IsDir():
		return fmt.Errorf("assets: root %s is not a directory", abs)
	}
	return nil
}

//
// Path safety
//

// blobPath maps a validated hash to blobs/ab/cd/<hash>.
func (l *Local) blobPath(hash string) string {
	return filepath.Join(l.root, blobsDirName, hash[:2], hash[2:4], hash)
}

// stagingPath maps a validated staging ID to staging/<id>.
func (l *Local) stagingPath(id StagingID) string {
	return filepath.Join(l.root, stagingDirName, string(id))
}

// insideRoot reports whether path is the root or below it, lexically. Combined
// with normalizeHash and validateStagingID, which admit only [0-9a-f], no caller
// can name a path that escapes; the check stays because paths also arrive from
// directory walks.
func (l *Local) insideRoot(path string) error {
	rel, err := filepath.Rel(l.root, path)
	if err != nil {
		return fmt.Errorf("assets: path %s relative to %s: %w", path, l.root, ErrUnsafePath)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("assets: path %s escapes root %s: %w", path, l.root, ErrUnsafePath)
	}
	return nil
}

// checkChain Lstats every component from the root down to path and refuses
// symlinks. A missing component ends the walk (nothing below it can exist),
// which is the normal case while creating directories.
func (l *Local) checkChain(path string) error {
	if err := l.insideRoot(path); err != nil {
		return err
	}
	rel, err := filepath.Rel(l.root, path)
	if err != nil || rel == "." {
		return err
	}
	cur := l.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("assets: inspect %s: %w", cur, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("assets: symlink at %s: %w", cur, ErrUnsafePath)
		}
	}
	return nil
}

// ensureRoot creates the asset root, including any missing parents — a first
// run against $XDG_DATA_HOME/game-narrator has none of them yet. The parents are
// created with the same 0700 so the whole private chain is born closed; the
// leaf is chmodded explicitly because MkdirAll masks the mode with the umask and
// never re-modes a directory that already existed.
func (l *Local) ensureRoot(root string) error {
	_, statErr := os.Lstat(root)
	missing := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !missing {
		return fmt.Errorf("assets: inspect root %s: %w", root, statErr)
	}
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return fmt.Errorf("assets: create root %s: %w", root, err)
	}
	if err := refuseSymlinkRoot(root); err != nil {
		return err
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("assets: inspect root %s: %w", root, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("assets: root %s is not a directory", root)
	}
	if fi.Mode().Perm() != dirPerm {
		if err := os.Chmod(root, dirPerm); err != nil {
			return fmt.Errorf("assets: chmod root %s: %w", root, err)
		}
	}
	return nil
}

// ensurePrivateDir creates one directory under the root (and any missing
// directories between the root and it) at mode 0700, refusing to traverse or
// overwrite a symlink at any step.
//
// Directories are made one component at a time rather than with MkdirAll so that
// each newly created fan-out directory can be fsynced through its parent. A blob
// renamed into a directory entry that never reached the journal is a blob a crash
// can lose, and Commit must not report success for bytes that then disappear
// (docs/spec/02 "atomic temp+fsync+rename").
func (l *Local) ensurePrivateDir(dir string) error {
	if err := l.insideRoot(dir); err != nil {
		return err
	}
	rel, err := filepath.Rel(l.root, dir)
	if err != nil {
		return fmt.Errorf("assets: path %s relative to %s: %w", dir, l.root, err)
	}
	if rel == "." {
		return nil // the root itself: ensureRoot owns it
	}
	cur := l.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if err := l.mkdirPrivate(cur); err != nil {
			return err
		}
	}
	return nil
}

// mkdirPrivate creates a single directory, tolerating an existing one, pins its
// mode, and refuses symlinks and non-directories in its place.
func (l *Local) mkdirPrivate(dir string) error {
	if err := l.checkChain(dir); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(dir, dirPerm); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("assets: create %s: %w", dir, err)
		}
		if err := syncDir(filepath.Dir(dir)); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("assets: inspect %s: %w", dir, err)
	}

	fi, err = os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("assets: %s disappeared while being created: %w", dir, ErrUnsafePath)
	}
	if err != nil {
		return fmt.Errorf("assets: inspect %s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("assets: symlink at %s: %w", dir, ErrUnsafePath)
	}
	if !fi.IsDir() {
		return fmt.Errorf("assets: %s is not a directory: %w", dir, ErrUnsafePath)
	}
	if fi.Mode().Perm() != dirPerm {
		if err := os.Chmod(dir, dirPerm); err != nil {
			return fmt.Errorf("assets: chmod %s: %w", dir, err)
		}
	}
	return nil
}

// openValidated opens a path only as a regular file, then proves the descriptor
// still refers to the inode that Lstat described. Lstat-then-open is racy on its
// own (the file can be swapped in between), so os.SameFile closes that gap
// without O_NOFOLLOW, which is not portable across the platforms contributors
// build on.
func openValidated(path string) (*os.File, os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("assets: inspect %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("assets: symlink at %s: %w", path, ErrUnsafePath)
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("assets: %s is not a regular file: %w", path, ErrUnsafePath)
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, nil, fmt.Errorf("assets: open %s: %w", path, err)
	}
	openedFi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("assets: stat %s: %w", path, err)
	}
	if !os.SameFile(fi, openedFi) {
		f.Close()
		return nil, nil, fmt.Errorf("assets: %s changed while opening: %w", path, ErrUnsafePath)
	}
	return f, openedFi, nil
}

// syncDir fsyncs a directory so a rename or a new entry is durable — the part
// of "atomic temp+fsync+rename" (docs/spec/02) that a file fsync cannot cover.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("assets: open dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("assets: fsync dir %s: %w", dir, err)
	}
	return nil
}

//
// Staging and commit
//

// PutStaged streams r into staging while hashing it, then returns the address
// the content will take on Commit. Memory stays bounded by copyBufSize
// regardless of blob size (docs/spec/14 "memory/time limits").
func (l *Local) PutStaged(ctx context.Context, r io.Reader, expectedSHA256 string) (StagingID, Info, error) {
	if r == nil {
		return "", Info{}, errors.New("assets: PutStaged requires a reader")
	}
	if err := ctx.Err(); err != nil {
		return "", Info{}, err
	}
	if expectedSHA256 != "" {
		if _, err := normalizeHash(expectedSHA256); err != nil {
			return "", Info{}, err
		}
	}

	id, err := newStagingID()
	if err != nil {
		return "", Info{}, err
	}
	path := l.stagingPath(id)
	if err := l.ensurePrivateDir(filepath.Dir(path)); err != nil {
		return "", Info{}, err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return "", Info{}, fmt.Errorf("assets: stage %s: %w", id, err)
	}
	// An existing file's mode is unaffected by the OpenFile argument, so pin it.
	if err := f.Chmod(filePerm); err != nil {
		f.Close()
		os.Remove(path)
		return "", Info{}, fmt.Errorf("assets: chmod staging %s: %w", id, err)
	}

	info, err := streamInto(ctx, f, r, l.maxSize)
	if err != nil {
		f.Close()
		os.Remove(path) // discard, never resume (docs/spec/02 "Crash recovery")
		if errors.Is(err, ErrTooLarge) {
			return "", Info{}, fmt.Errorf("assets: staging %s: %w (limit %d bytes)", id, ErrTooLarge, l.maxSize)
		}
		return "", Info{}, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return "", Info{}, fmt.Errorf("assets: fsync staging %s: %w", id, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", Info{}, fmt.Errorf("assets: close staging %s: %w", id, err)
	}

	if expectedSHA256 != "" && expectedSHA256 != info.SHA256 {
		// The caller's expectation lost to the bytes: keep nothing, so a
		// mislabelled import cannot leave a stray blob behind.
		os.Remove(path)
		l.forget(id)
		return "", Info{}, fmt.Errorf("%w: content hashed to %s, expected %s", ErrHashMismatch, info.SHA256, expectedSHA256)
	}

	l.remember(id, info)
	return id, info, nil
}

// streamInto copies r into f while computing the digest, size and media type.
// Nothing is buffered beyond one copyBufSize chunk: a source-file import is
// gigabytes, and holding it in memory would trade a disk problem for an OOM.
func streamInto(ctx context.Context, f *os.File, r io.Reader, maxSize int64) (Info, error) {
	w := &stagingWriter{dst: f, hash: sha256.New(), sniff: make([]byte, 0, sniffLen), max: maxSize}
	buf := make([]byte, copyBufSize)
	if _, err := io.CopyBuffer(w, &ctxReader{ctx: ctx, r: r}, buf); err != nil {
		var overflow *overflowError
		if errors.As(err, &overflow) {
			return Info{}, ErrTooLarge
		}
		return Info{}, fmt.Errorf("assets: stream staged content: %w", err)
	}
	return Info{SHA256: w.hex(), Size: w.total, MediaType: detectMediaType(w.sniff)}, nil
}

// overflowError is the sentinel a writer returns to io.CopyBuffer when the byte
// budget is crossed; io.Copy propagates it unchanged.
type overflowError struct{ limit int64 }

func (e *overflowError) Error() string {
	return fmt.Sprintf("assets: content exceeds size limit of %d bytes", e.limit)
}

func (e *overflowError) Is(target error) bool { return target == ErrTooLarge }

// stagingWriter is the io.Copy destination for a staged write: it forwards to
// the staging file, folds the bytes into the running digest, keeps only the
// sniff prefix, and refuses to cross the size limit.
type stagingWriter struct {
	dst   io.Writer
	hash  hash.Hash
	sniff []byte
	total int64
	max   int64
}

func (w *stagingWriter) Write(p []byte) (int, error) {
	if w.max > 0 && w.total+int64(len(p)) > w.max {
		// Refused before the write: the staged file never grows past the limit,
		// so an oversized import cannot fill the disk even one chunk at a time.
		return 0, &overflowError{limit: w.max}
	}
	if _, err := w.dst.Write(p); err != nil {
		return 0, err
	}
	if _, err := w.hash.Write(p); err != nil {
		return 0, err
	}
	w.total += int64(len(p))
	w.sniff = collectPrefix(w.sniff, p)
	return len(p), nil
}

// collectPrefix accumulates the first sniffLen bytes of a stream for content
// sniffing without ever holding more than that.
func collectPrefix(dst, src []byte) []byte {
	if len(dst) >= sniffLen {
		return dst
	}
	take := sniffLen - len(dst)
	if take > len(src) {
		take = len(src)
	}
	return append(dst, src[:take]...)
}

// ctxReader makes cancellation responsive during a long stream: io.Copy has no
// context of its own, and a 64 MiB import from a slow reader would otherwise
// ignore a deadline until the source itself stopped.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func (w *stagingWriter) hex() string { return fmt.Sprintf("%x", w.hash.Sum(nil)) }

// detectMediaType sniffs content with http.DetectContentType, which examines
// at most its first 512 bytes. The prefix is passed as-is rather than zero
// padded: padding would append NULs, which the text heuristics read as binary,
// and a short text file would be catalogued as application/octet-stream. For a
// file under 512 bytes the prefix *is* the file, so the answer matches what a
// reader computing it from the whole file would get.
func detectMediaType(sniff []byte) string {
	return http.DetectContentType(sniff)
}

func (l *Local) remember(id StagingID, info Info) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.staged[id] = info
}

func (l *Local) forget(id StagingID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.staged, id)
}

func (l *Local) rememberOf(id StagingID) (Info, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	info, ok := l.staged[id]
	return info, ok
}

// Commit makes a staged entry readable under its content hash. The staged file
// is re-hashed here rather than trusting PutStaged's bookkeeping, so a
// process-restart, a concurrent GC, or on-disk tampering between the two calls
// cannot smuggle mismatched bytes into the blob namespace.
func (l *Local) Commit(ctx context.Context, id StagingID) (Info, error) {
	if err := validateStagingID(id); err != nil {
		return Info{}, err
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	sp := l.stagingPath(id)
	if err := l.checkChain(sp); err != nil {
		return Info{}, err
	}

	f, _, err := openValidated(sp)
	if err != nil {
		return Info{}, fmt.Errorf("assets: commit %s: %w", id, err)
	}
	info, err := hashReader(ctx, f)
	if err != nil {
		f.Close()
		return Info{}, fmt.Errorf("assets: commit %s: %w", id, err)
	}
	// Re-fsync before publishing the name. PutStaged already did this, but the
	// staged inode is the one that becomes the blob, and a committed blob must
	// never be a name whose bytes are still only in the page cache.
	if err := f.Sync(); err != nil {
		f.Close()
		return Info{}, fmt.Errorf("assets: commit %s: fsync: %w", id, err)
	}
	if err := f.Close(); err != nil {
		return Info{}, fmt.Errorf("assets: commit %s: close: %w", id, err)
	}
	if expected, ok := l.rememberOf(id); ok && expected.SHA256 != info.SHA256 {
		os.Remove(sp)
		l.forget(id)
		return Info{}, fmt.Errorf("%w: staging %s hashed to %s after PutStaged saw %s", ErrCorrupt, id, info.SHA256, expected.SHA256)
	}

	dp := l.blobPath(info.SHA256)
	if err := l.checkChain(dp); err != nil {
		return Info{}, err
	}
	if err := l.ensurePrivateDir(filepath.Dir(dp)); err != nil {
		return Info{}, err
	}

	// Land the blob with link(2) rather than rename(2). link fails with EEXIST
	// when the destination exists, which is the atomic "rename but do not
	// replace" the standard library does not expose — and replacing matters
	// here: two jobs that synthesize the same utterance produce the same address,
	// and a rename over an existing blob unlinks the inode a concurrent reader
	// just opened, so its os.SameFile integrity check fails on bytes that were
	// never wrong. Whoever wins the link owns the name; everyone else dedupes.
	switch err := os.Link(sp, dp); {
	case err == nil:
		if err := syncDir(filepath.Dir(dp)); err != nil {
			return Info{}, err
		}
	case errors.Is(err, os.ErrExist):
		return l.dedupeStaged(ctx, id, sp, info.SHA256)
	default:
		// A filesystem without hard-link support, or staging on a separate
		// mount, cannot use the primary path: fall back to a checked rename.
		return l.commitByRename(ctx, id, sp, dp, info)
	}

	// Discard the staging name. If this fails, the bytes are committed and
	// readable; RecoverStaging will collect the orphan.
	if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Info{}, fmt.Errorf("assets: commit %s: discard staging name: %w", id, err)
	}
	l.forget(id)
	return info, nil
}

// dedupeStaged finishes a commit whose address was already taken by identical
// content. Stat reads the winner back from the filesystem rather than reusing
// the caller's Info, because the existing entry may have been written by another
// process — and if that entry is not a safe regular blob, Stat says so.
func (l *Local) dedupeStaged(ctx context.Context, id StagingID, sp, hash string) (Info, error) {
	existing, err := l.Stat(ctx, hash)
	if err != nil {
		return Info{}, err
	}
	if err := os.Remove(sp); err != nil {
		return Info{}, fmt.Errorf("assets: commit %s: discard duplicate staging file: %w", id, err)
	}
	l.forget(id)
	return existing, nil
}

// commitByRename is the fallback landing path: check, then rename. It is not
// atomic against a concurrent creator of the same address (the rename replaces),
// which is why it is only used where link(2) is unavailable.
func (l *Local) commitByRename(ctx context.Context, id StagingID, sp, dp string, info Info) (Info, error) {
	if existing, err := l.Stat(ctx, info.SHA256); err == nil {
		return l.dedupeStaged(ctx, id, sp, existing.SHA256)
	} else if !errors.Is(err, ErrNotFound) {
		return Info{}, err
	}
	if err := os.Rename(sp, dp); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Info{}, fmt.Errorf("assets: commit %s: %w (staging file vanished)", id, ErrNotFound)
		}
		return Info{}, fmt.Errorf("assets: commit %s: rename: %w", id, err)
	}
	if err := syncDir(filepath.Dir(dp)); err != nil {
		return Info{}, err
	}
	l.forget(id)
	return info, nil
}

// Abort discards a staged entry.
func (l *Local) Abort(ctx context.Context, id StagingID) error {
	if err := validateStagingID(id); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sp := l.stagingPath(id)
	if err := l.checkChain(sp); err != nil {
		return err
	}
	if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("assets: abort %s: %w", id, err)
	}
	l.forget(id)
	return nil
}

// hashReader computes Info from a stream without buffering it.
func hashReader(ctx context.Context, r io.Reader) (Info, error) {
	h := sha256.New()
	sniff := make([]byte, 0, sniffLen)
	buf := make([]byte, copyBufSize)
	var total int64
	src := &ctxReader{ctx: ctx, r: r}
	for {
		n, err := src.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
			sniff = collectPrefix(sniff, buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Info{}, err
		}
	}
	return Info{SHA256: fmt.Sprintf("%x", h.Sum(nil)), Size: total, MediaType: detectMediaType(sniff)}, nil
}

//
// Reading
//

// openBlob resolves a hash argument to an open, verified-regular blob.
func (l *Local) openBlob(ctx context.Context, hash string) (*os.File, os.FileInfo, error) {
	h, err := normalizeHash(hash)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	p := l.blobPath(h)
	if err := l.checkChain(p); err != nil {
		return nil, nil, err
	}
	f, fi, err := openValidated(p)
	if err != nil {
		return nil, nil, err
	}
	// The address is the name: a blob file whose basename is not the hash we
	// asked for means the tree was edited underneath us.
	if filepath.Base(f.Name()) != h {
		f.Close()
		return nil, nil, fmt.Errorf("assets: %s is not the blob for %s: %w", f.Name(), h, ErrUnsafePath)
	}
	return f, fi, nil
}

// Get opens a committed blob. The returned reader yields the full content.
func (l *Local) Get(ctx context.Context, hash string) (io.ReadCloser, Info, error) {
	f, fi, err := l.openBlob(ctx, hash)
	if err != nil {
		return nil, Info{}, err
	}
	prefix, err := readPrefix(f)
	if err != nil {
		f.Close()
		return nil, Info{}, err
	}
	info := Info{SHA256: filepath.Base(f.Name()), Size: fi.Size(), MediaType: detectMediaType(prefix)}
	return &blobReader{Reader: io.MultiReader(bytes.NewReader(prefix), f), f: f}, info, nil
}

// Stat returns metadata for a committed blob. The media type is re-derived from
// the content rather than remembered, so a catalogue can always be rebuilt from
// the blob tree alone.
func (l *Local) Stat(ctx context.Context, hash string) (Info, error) {
	f, fi, err := l.openBlob(ctx, hash)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	prefix, err := readPrefix(f)
	if err != nil {
		return Info{}, err
	}
	return Info{SHA256: filepath.Base(f.Name()), Size: fi.Size(), MediaType: detectMediaType(prefix)}, nil
}

// Verify re-hashes a committed blob (docs/spec/02 "verify on read").
func (l *Local) Verify(ctx context.Context, hash string) error {
	h, err := normalizeHash(hash)
	if err != nil {
		return err
	}
	f, fi, err := l.openBlob(ctx, h)
	if err != nil {
		return err
	}
	defer f.Close()

	sum := sha256.New()
	buf := make([]byte, copyBufSize)
	if _, err := io.CopyBuffer(sum, &ctxReader{ctx: ctx, r: f}, buf); err != nil {
		return fmt.Errorf("assets: verify %s: %w", h, err)
	}
	if got := fmt.Sprintf("%x", sum.Sum(nil)); got != h {
		return fmt.Errorf("%w: %s hashes to %s (%d bytes)", ErrCorrupt, h, got, fi.Size())
	}
	return nil
}

// Delete removes exactly one committed blob and nothing else. It resolves the
// hash through the same safety chain as every other read (normalizeHash,
// checkChain, openValidated, base-name identity), so a tampered tree cannot
// redirect it. The fan-out parent is fsynced after removal for the same reason
// a rename into it is: the deletion of a name must be durable. Symlinks and
// non-regular files at the address are refused, never followed (docs/spec/14
// "Filesystem/import").
//
// Reachability is deliberately the caller's contract: the store cannot tell a
// blob that is about to be evicted from a voice reference with identical
// bytes, and deleting the latter would corrupt approved content
// (docs/spec/05 voice source of truth). Callers that hold a catalogue (the
// speech cache's index, the asset GC's predicate) decide it.
func (l *Local) Delete(ctx context.Context, hash string) error {
	h, err := normalizeHash(hash)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p := l.blobPath(h)
	if err := l.checkChain(p); err != nil {
		return err
	}
	f, _, err := openValidated(p)
	if err != nil {
		return err
	}
	// The address is the name, as in every other read path.
	if filepath.Base(f.Name()) != h {
		f.Close()
		return fmt.Errorf("assets: %s is not the blob for %s: %w", f.Name(), h, ErrUnsafePath)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("assets: delete %s: close: %w", h, err)
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("assets: delete %s: %w", h, ErrNotFound)
		}
		return fmt.Errorf("assets: delete %s: %w", h, err)
	}
	return syncDir(filepath.Dir(p))
}

// readPrefix reads up to sniffLen bytes for content sniffing, leaving the file
// offset just past them.
func readPrefix(f *os.File) ([]byte, error) {
	buf := make([]byte, sniffLen)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("assets: sniff %s: %w", f.Name(), err)
	}
	if _, err := f.Seek(int64(n), io.SeekStart); err != nil {
		return nil, fmt.Errorf("assets: seek %s: %w", f.Name(), err)
	}
	return buf[:n], nil
}

// blobReader keeps the underlying descriptor alive for the lifetime of the
// stream: Get returns a reader, and the file must not vanish mid-read.
type blobReader struct {
	io.Reader
	f *os.File
}

func (b *blobReader) Close() error {
	err := b.f.Close()
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

//
// Small-path helpers used by export.go and gc.go
//

// blobHashAt converts a path under <root>/blobs back to the hash it names, and
// reports false for anything that is not exactly blobs/ab/cd/<hash>: a stray
// file with a hash-shaped name in the wrong place is not ours to delete.
func (l *Local) blobHashAt(path string) (string, bool) {
	rel, err := filepath.Rel(filepath.Join(l.root, blobsDirName), path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	h := filepath.Base(rel)
	if !ValidHash(h) {
		return "", false
	}
	if filepath.ToSlash(rel) != filepath.ToSlash(filepath.Join(h[:2], h[2:4], h)) {
		return "", false
	}
	return h, true
}

// removeIfExists tolerates a file that is already gone.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
