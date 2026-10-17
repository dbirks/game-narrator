package support

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// blobDoc is one stored dev-trace sample's metadata: hash and size only.
type blobDoc struct {
	SHA256     string    `json:"sha256"`
	Bytes      int64     `json:"bytes"`
	ModifiedAt time.Time `json:"modified_at"`
}

// sessionDoc is one dev-trace session's metadata. The id is the session's
// directory basename — an internal session id, never a path.
type sessionDoc struct {
	ID          string     `json:"id"`
	BlobCount   int        `json:"blob_count"`
	Bytes       int64      `json:"bytes"`
	FirstSample *time.Time `json:"first_sample,omitempty"`
	LastSample  *time.Time `json:"last_sample,omitempty"`
	Blobs       []blobDoc  `json:"blobs"`
}

// devTraceDoc is the dev-trace section (only with --include-dev-trace).
type devTraceDoc struct {
	Note     string       `json:"note"`
	Sessions []sessionDoc `json:"sessions"`
}

// devTraceRootName is the data-dir subdirectory the runtime gives to the
// dev-trace recorder (cmd/game-narrator: devTraceDir).
const devTraceRootName = "dev-trace"

// buildDevTrace walks <dataDir>/dev-trace/<session>/blobs/** and reports, per
// session: the id, the blob hashes, sizes and mtimes. It never opens a blob:
// the hashes and sizes are the metadata a support engineer correlates the
// trace against (docs/spec/13: "log hashes, counts and selected
// confidence"), and the frame/crop audio bytes stay on the machine.
func buildDevTrace(o Options) ([]byte, error) {
	root := filepath.Join(o.DataDir, devTraceRootName)
	doc := devTraceDoc{
		Note:     "metadata only: session ids and per-blob sha256/size/mtime. No frame, crop, audio or dialogue content is included (docs/spec/13).",
		Sessions: []sessionDoc{},
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return marshalDoc(doc)
	}
	sessions, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("support: list dev-trace sessions: %w", err)
	}
	since := time.Time{}
	if o.Since > 0 {
		since = o.Now().Add(-o.Since)
	}
	for _, sess := range sessions {
		if !sess.IsDir() {
			continue
		}
		blobsDir := filepath.Join(root, sess.Name(), "blobs")
		var s sessionDoc
		s.ID = sess.Name()
		err := filepath.WalkDir(blobsDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			if o.Since > 0 && fi.ModTime().Before(since) {
				return nil
			}
			sum := fi.Name()
			if len(sum) != 64 {
				// Not a content-addressed blob (staging file or stray): skip.
				return nil
			}
			s.Blobs = append(s.Blobs, blobDoc{SHA256: sum, Bytes: fi.Size(), ModifiedAt: fi.ModTime().UTC()})
			s.Bytes += fi.Size()
			if s.FirstSample == nil || fi.ModTime().Before(*s.FirstSample) {
				t := fi.ModTime().UTC()
				s.FirstSample = &t
			}
			if s.LastSample == nil || fi.ModTime().After(*s.LastSample) {
				t := fi.ModTime().UTC()
				s.LastSample = &t
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("support: walk dev-trace session %s: %w", sess.Name(), err)
		}
		if len(s.Blobs) == 0 {
			continue
		}
		s.BlobCount = len(s.Blobs)
		sort.Slice(s.Blobs, func(i, j int) bool { return s.Blobs[i].SHA256 < s.Blobs[j].SHA256 })
		doc.Sessions = append(doc.Sessions, s)
	}
	sort.Slice(doc.Sessions, func(i, j int) bool { return doc.Sessions[i].ID < doc.Sessions[j].ID })
	return marshalDoc(doc)
}
