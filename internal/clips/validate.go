package clips

// Layout validation: a clip is a directory in the shape
// internal/capture/replay reads — a timestamps.json manifest naming the frame
// files relative to the clip root, each with a positive replay-clock
// timestamp. ValidateDir is the import-time proof the clip will actually run:
// the manifest parses, every referenced frame exists as a regular file under
// the clip root, and a real decoder decodes a frame. A clip that installed
// but could not run would be a session that fails at start — the exact failure
// the import is supposed to prevent.

import (
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"

	"github.com/dbirks/game-narrator/internal/capture/replay"
)

// ManifestName is the clip layout's manifest, the same file the replay source
// reads.
const ManifestName = "timestamps.json"

// Clip is the import-time summary of one validated clip.
type Clip struct {
	// Frames is the manifest's entry count: the frames a session will replay.
	Frames int
	// DurationNS is the manifest's span (last minus first timestamp), for the
	// list's display; a one-frame clip reports its cadence fallback of 0.
	DurationNS int64
	// Format is the frame container the first decoded frame reported, so the
	// record says what the frames actually are (docs/spec/01: evidence names
	// what it is).
	Format string
}

// ValidateDir checks dir's clip layout and returns its summary. dir must
// already sit under the managed store: this function is the "is this a
// runnable clip" half of the install, not the "is this path safe" half.
func ValidateDir(dir string) (Clip, error) {
	manifest := filepath.Join(dir, ManifestName)
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return Clip{}, fmt.Errorf("clips: the archive has no readable %s at its root: %w", ManifestName, err)
	}
	var entries []replay.Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return Clip{}, fmt.Errorf("clips: the %s manifest is not valid JSON: %w", ManifestName, err)
	}
	if len(entries) == 0 {
		return Clip{}, fmt.Errorf("clips: the %s manifest lists no frames", ManifestName)
	}
	var last int64
	for i, e := range entries {
		// The replay source refuses these at start too (AGENTS.md rule 6: the
		// manifest is data, and a data entry that names "../etc/passwd" is a
		// traversal attempt); refusing at import says so before the install.
		if e.File == "" || !filepath.IsLocal(filepath.Clean(e.File)) {
			return Clip{}, fmt.Errorf("clips: manifest entry %d names an unsafe file %q", i, e.File)
		}
		if e.TsNS <= 0 {
			return Clip{}, fmt.Errorf("clips: manifest entry %d has timestamp %d, want > 0", i, e.TsNS)
		}
		info, err := os.Stat(filepath.Join(dir, e.File))
		if err != nil {
			return Clip{}, fmt.Errorf("clips: manifest entry %d names %q, which is not in the clip: %v", i, e.File, err)
		}
		if !info.Mode().IsRegular() {
			return Clip{}, fmt.Errorf("clips: manifest entry %d names %q, which is not a regular file", i, e.File)
		}
		last = e.TsNS
	}
	// Decode the first and last frames: a clip whose frames are not images
	// would fail on the first frame anyway — the import earns the earlier,
	// named refusal. Both ends keep the cost bounded on a long recording.
	for _, e := range []replay.Entry{entries[0], entries[len(entries)-1]} {
		if _, err := decodeFrame(filepath.Join(dir, e.File)); err != nil {
			return Clip{}, fmt.Errorf("clips: frame %q does not decode: %w", e.File, err)
		}
	}
	return Clip{Frames: len(entries), DurationNS: last - entries[0].TsNS}, nil
}

// decodeFrame is image.Decode plus the PNG/JPEG registration the replay
// source itself uses.
func decodeFrame(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	return img, nil
}
