package clips

// The clip index: the provenance record the managed store keeps beside the
// clip directories. The directories are the truth for "which clips exist" —
// a directory without an index row still lists (with its provenance fields
// empty), and an index row without a directory lists as missing — but the
// index is where the archive hash, the actor and the install time live, and
// those are exactly the facts AGENTS.md rule 5 (provenance recorded) is about.
//
// The file is one small JSON document written atomically (temp + fsync +
// rename), the same discipline the asset store's staged writes use, so a
// crash cannot leave a half-written index behind.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// IndexFileName is the index's name at the managed store's root.
const IndexFileName = "index.json"

// stagingPrefix names an in-flight extraction; PruneStaging removes the
// leftovers a crashed install leaves.
const stagingPrefix = ".staging-"

// Record is one installed clip's provenance.
type Record struct {
	// ID is the clip's directory name and its session-selecting identity.
	ID string `json:"id"`
	// Name is the caller's display name when it differed from the id.
	Name string `json:"name,omitempty"`
	// SHA256 is the uploaded archive's content hash (in the asset store); it
	// is the provenance value, recorded streamed — the archive path is not.
	SHA256 string `json:"archive_sha256"`
	// Size is the uploaded archive's byte count.
	Size int64 `json:"archive_size"`
	// Frames is the manifest's entry count at install time.
	Frames int `json:"frames"`
	// Format is the archive container (tar or zip) the upload carried.
	Format string `json:"archive_format"`
	// CreatedAt is the install time, UTC.
	CreatedAt time.Time `json:"created_at"`
	// CreatedBy is the acting principal (username; "system" when the install
	// did not come from a request).
	CreatedBy string `json:"created_by,omitempty"`
}

// Index is the store's whole provenance table, in id order.
type Index struct {
	Records []Record `json:"records"`
}

// ErrCorruptIndex is a syntactically invalid index file. The managed store's
// other answers keep working (delete is path-scoped); the list reports it
// rather than guessing what the provenance was.
var ErrCorruptIndex = errors.New("clips: the clip index is corrupt")

// Load reads root's index. A missing file is the empty index (a store whose
// first install has not happened yet), not an error.
func Load(root string) (Index, error) {
	var idx Index
	raw, err := os.ReadFile(filepath.Join(root, IndexFileName))
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return idx, fmt.Errorf("clips: reading the index: %w", err)
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return idx, fmt.Errorf("%w: %v", ErrCorruptIndex, err)
	}
	sort.Slice(idx.Records, func(i, j int) bool { return idx.Records[i].ID < idx.Records[j].ID })
	return idx, nil
}

// Save writes the index atomically under root.
func Save(root string, idx Index) error {
	sort.Slice(idx.Records, func(i, j int) bool { return idx.Records[i].ID < idx.Records[j].ID })
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("clips: encoding the index: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("clips: creating the store directory: %w", err)
	}
	tmp, err := os.CreateTemp(root, IndexFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("clips: creating the index temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op after the rename succeeds
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("clips: writing the index: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("clips: syncing the index: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("clips: closing the index: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("clips: setting the index mode: %w", err)
	}
	return os.Rename(tmpName, filepath.Join(root, IndexFileName))
}

// Update is Load-then-Save with the index locked to this process by the
// single-writer rule the appliance already runs on (one serve per data
// directory, docs/adr/0008).
func Update(root string, fn func(*Index) error) error {
	idx, err := Load(root)
	if err != nil {
		return err
	}
	if err := fn(&idx); err != nil {
		return err
	}
	return Save(root, idx)
}

// Find returns the record for id, if the index has one.
func (idx Index) Find(id string) (Record, bool) {
	for _, r := range idx.Records {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

// StagingDir returns a fresh staging path under root. The random suffix is
// the one thing that makes two concurrent installs (a single-writer appliance
// should not have any, but the answer must be safe, not lucky) land in
// different directories.
func StagingDir(root string, seed string) (string, error) {
	name := stagingPrefix + seed
	return filepath.Join(root, name), nil
}

// PruneStaging removes staging directories under root — the leftovers an
// install that crashed between "extracted" and "moved into place" leaves.
// It removes only directories it owns by prefix and reports how many.
func PruneStaging(root string) (int, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("clips: reading the store for stale staging: %w", err)
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), stagingPrefix) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}
