package assets

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// DeleteUnreachable removes blobs that nothing references any more.
//
// Reachability, not a refcount, decides deletion (docs/spec/02 "source_blobs":
// "reference counts are advisory, GC via reachability"), because a crashed
// import leaves a row missing as easily as it leaves a file missing, and a
// predicate that reads the live catalogue cannot drift out of step with it. The
// walk is confined to <root>/blobs: staging is RecoverStaging's business and the
// rest of the filesystem is nobody's business here.
//
// A nil predicate would mean "delete everything", which is never what a caller
// meant mid-loop; callers start with a dry run instead.
func (l *Local) DeleteUnreachable(ctx context.Context, reachable func(hash string) bool, dryRun bool) ([]string, error) {
	if reachable == nil {
		return nil, errors.New("assets: DeleteUnreachable requires a reachability predicate")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	blobsRoot := filepath.Join(l.root, blobsDirName)
	if err := l.checkChain(blobsRoot); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(blobsRoot); errors.Is(err, os.ErrNotExist) {
		return nil, nil // nothing stored yet
	} else if err != nil {
		return nil, fmt.Errorf("assets: inspect %s: %w", blobsRoot, err)
	}

	var unreachable []string
	dirs := map[string]int{} // fan-out directory -> depth, for bottom-up cleanup

	err := filepath.WalkDir(blobsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A dangling entry (unreadable directory) stops the GC rather than
			// letting it act on an incomplete picture of what is reachable.
			return err
		}
		if path == blobsRoot {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			// Never follow, never delete: a link inside the blob tree is a
			// tampering signal, not garbage (docs/spec/14 "Filesystem/import").
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			dirs[path] = depthUnder(blobsRoot, path)
			return nil
		}
		// Anything that is not a regular file — a symlink, a socket, a device
		// node planted under blobs/ — is neither followed nor deleted: it is a
		// tampering signal, and deleting it could delete what it points at
		// (docs/spec/14 "Filesystem/import").
		if !d.Type().IsRegular() {
			return nil
		}
		hash, ok := l.blobHashAt(path)
		if !ok {
			return nil
		}
		if reachable(hash) {
			return nil
		}
		unreachable = append(unreachable, hash)
		if dryRun {
			return nil
		}
		if err := removeIfExists(path); err != nil {
			return fmt.Errorf("assets: delete %s: %w", hash, err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("assets: delete unreachable: %w", err)
	}
	sort.Strings(unreachable)

	if dryRun {
		return unreachable, nil
	}
	if err := removeEmptyFanout(dirs); err != nil {
		return unreachable, err
	}
	return unreachable, nil
}

// removeEmptyFanout prunes emptied ab/cd directories, deepest first, so that
// removing a whole prefix leaves no skeleton behind. The blobs root itself is
// never removed — the store must stay usable without re-running NewLocal.
func removeEmptyFanout(dirs map[string]int) error {
	paths := make([]string, 0, len(dirs))
	for p := range dirs {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		if dirs[paths[i]] != dirs[paths[j]] {
			return dirs[paths[i]] > dirs[paths[j]]
		}
		return paths[i] > paths[j]
	})

	for _, p := range paths {
		entries, err := os.ReadDir(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("assets: prune %s: %w", p, err)
		}
		if len(entries) > 0 {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrInvalid) {
			// ENOTEMPTY from a racing commit is the good outcome: keep the dir.
			return fmt.Errorf("assets: prune %s: %w", p, err)
		}
	}
	return nil
}

func depthUnder(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return 0
	}
	if rel == "." {
		return 0
	}
	n := 1
	for _, c := range rel {
		if c == filepath.Separator {
			n++
		}
	}
	return n
}

// RecoverStaging discards staging files left behind by an interrupted write
// (docs/spec/02 "Transaction workflows": "interrupted asset staged writes
// discarded or resumed"). We discard: a half-written blob has no resume token,
// and re-uploading is cheaper than reconciling an unknown byte count.
//
// Service startup calls this before serving reads, so the staging directory
// never accumulates garbage across restarts.
func (l *Local) RecoverStaging(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	stagingRoot := filepath.Join(l.root, stagingDirName)
	if err := l.checkChain(stagingRoot); err != nil {
		return 0, err
	}
	if _, err := os.Lstat(stagingRoot); errors.Is(err, os.ErrNotExist) {
		l.forgetAll()
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("assets: inspect %s: %w", stagingRoot, err)
	}

	discarded := 0
	// Removal is confined to staging/ and never follows a link: os.RemoveAll on
	// a symlinked subdirectory would unlink the link rather than its target,
	// which is still inside our namespace and still what we want gone.
	err := filepath.WalkDir(stagingRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == stagingRoot {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() && d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("assets: discard staging %s: %w", path, err)
		}
		discarded++
		return nil
	})
	if err != nil {
		return discarded, fmt.Errorf("assets: recover staging: %w", err)
	}

	// Drop the in-memory bookkeeping: those IDs no longer exist on disk.
	l.forgetAll()
	return discarded, nil
}

func (l *Local) forgetAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.staged = make(map[StagingID]Info)
}
