package scenario

// reclaim.go reclaims the per-run temp dirs a SIGKILLed harness run leaves
// behind (issue #14): a killed worker (go test -timeout panic, worker
// timeout, OOM, EDQUOT disk write) never reaches the cleanup the harness
// defers, so a 233 MB gn-scenario-dolphin-* replay dir survives under the
// system temp dir. The rule, same shape as the X lock owner check (issue #7):
//
//   - every per-run temp dir the harness creates (gn-scenario-dolphin-*,
//     gn-scenario-audio-*) carries a .gn-owner marker naming the PID of the
//     process that created it plus that process's start time (clock ticks
//     since boot where the OS exposes them), so a reused PID is not mistaken
//     for the live owner;
//   - at harness start the harness reclaims dirs whose owner process is
//     provably gone — never a dir whose owner is still alive, and never a
//     dir whose name does not start with one of the harness's own prefixes
//     (the system temp dir is shared; anything else is foreign);
//   - a prefixed dir with no readable marker (pre-marker builds) is only
//     reclaimed once its newest modification is older than a conservative
//     age threshold; and
//   - the bytes reclaimed are logged, so `make test-dolphin` reports them.
//
// The failure mode is always conservative: when the owner's liveness cannot
// be established (marker corrupt, platform without liveness probing) the dir
// is kept, never removed.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ownerMarkerName is the marker file inside each per-run temp dir.
const ownerMarkerName = ".gn-owner"

// tempDirPrefixes are the ONLY names the harness may reclaim from the
// system temp dir. Matching is exact-prefix (HasPrefix, dash included): a
// dir named "gn-scenario-dolphin" (no trailing dash) or
// "xgn-scenario-dolphin-foo" is foreign and must be left alone — the
// system temp dir is shared.
var tempDirPrefixes = []string{
	"gn-scenario-dolphin-",
	"gn-scenario-audio-",
}

// unmarkedMaxAgeDefault is how old a prefixed dir with no readable owner
// marker must be (its newest mtime) before the harness dares to remove it:
// the dirs are the harness's own, but a markerless dir could in principle be
// one this very run is still filling, so the floor is a full day, not
// milliseconds.
const unmarkedMaxAgeDefault = 24 * time.Hour

// tempOwner is the .gn-owner marker: who created the dir and when that
// process started, so a later reclaim can tell "same process" from "the PID
// wrapped around and a new one took it".
type tempOwner struct {
	// PID of the creating process.
	PID int `json:"pid"`
	// StartTicks is the process start time in clock ticks since boot (the
	// kernel's own field; it never wraps within a boot), 0 where the OS
	// does not expose it.
	StartTicks int64 `json:"start_ticks"`
	// WrittenAt is the wall-clock time the marker was written.
	WrittenAt time.Time `json:"written_at"`
}

// writeTempOwnerMarker records the creating process in dir. Best-effort by
// contract: a marker-write failure degrades the dir to the conservative
// unmarked-age rule, it never turns the dir into a reclaim candidate.
func writeTempOwnerMarker(dir string) {
	owner := tempOwner{
		PID:        os.Getpid(),
		StartTicks: processStartTicks(os.Getpid()),
		WrittenAt:  time.Now().UTC(),
	}
	raw, err := json.MarshalIndent(owner, "", "  ")
	if err != nil {
		slog.Error("scenario: encode owner marker", "dir", dir, "error", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, ownerMarkerName), append(raw, '\n'), 0o644); err != nil {
		slog.Error("scenario: write owner marker", "dir", dir, "error", err)
	}
}

// ReclaimReport is what a reclaim pass found and did.
type ReclaimReport struct {
	// Removed are the dirs that were taken, with the bytes they held.
	Removed []RemovedDir
	// Kept are the prefixed dirs that were left (live owner, or unmarked
	// and too recent to be sure of).
	Kept []string
	// Bytes is the total reclaimed (the sum over Removed).
	Bytes int64
}

// RemovedDir is one dir the reclaim pass removed.
type RemovedDir struct {
	Name  string
	Bytes int64
}

// String renders the report for the gate log line. Long lists are capped
// (a burst of stale dirs must not become a multi-kilobyte log line; the
// full accounting stays in the struct).
func (r ReclaimReport) String() string {
	if len(r.Removed) == 0 {
		return fmt.Sprintf("no stale per-run temp dirs reclaimed (kept: %s)", capJoin(r.Kept))
	}
	var parts []string
	for _, d := range r.Removed {
		parts = append(parts, fmt.Sprintf("%s (%d B)", d.Name, d.Bytes))
	}
	return fmt.Sprintf("reclaimed %d stale per-run temp dir(s), %d bytes: %s (kept: %d)",
		len(r.Removed), r.Bytes, capJoin(parts), len(r.Kept))
}

// capJoin joins names for logging, capping the tail.
func capJoin(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	const cap = 8
	if len(names) > cap {
		return strings.Join(names[:cap], ", ") + fmt.Sprintf(", ... +%d more", len(names)-cap)
	}
	return strings.Join(names, ", ")
}

// reclaimOurTempDirs is the harness-start hygiene pass (issue #14): before
// any scenario runs, reclaim the per-run temp dirs a previously KILLED run
// left under the system temp dir (the dirs this process creates clean up
// after themselves; only a SIGKILL skips the deferred cleanups). Safe by
// construction: it touches only our own prefixes and never a dir whose owner
// PID is alive. Called from every harness entry point (RunScenario,
// runDolphinScenario) so the API worker and every test gate get it.
func reclaimOurTempDirs() {
	rep := ReclaimStaleTempDirs(os.TempDir(), time.Now(), unmarkedMaxAgeDefault)
	if len(rep.Removed) > 0 {
		slog.Info("scenario: " + rep.String())
	}
}

// ReclaimStaleTempDirs scans baseDir for per-run temp dirs (directories
// whose name starts with one of tempDirPrefixes) and removes the ones whose
// owner process is provably gone. It returns a report of what it removed
// and kept, with the bytes reclaimed.
//
//   - a dir with a readable owner marker is removed only when its owner PID
//     is dead — and, where the OS exposes process start times, only when the
//     live PID's start time does not match the marker's (a reused PID is a
//     dead owner);
//   - a dir with no readable marker is removed only when its newest
//     modification is older than unmarkedMaxAge measured against now;
//   - a dir whose name does not start with one of the prefixes, or a file,
//     is never touched.
//
// Removal errors are logged and reported as kept, not fatal: the next pass
// retries.
func ReclaimStaleTempDirs(baseDir string, now time.Time, unmarkedMaxAge time.Duration) ReclaimReport {
	var rep ReclaimReport
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		slog.Error("scenario: reclaim scan", "dir", baseDir, "error", err.Error())
		return rep
	}
	for _, e := range entries {
		if !e.IsDir() || !hasTempPrefix(e.Name()) {
			continue // foreign or a file: never touched
		}
		path := filepath.Join(baseDir, e.Name())
		reason, dead, bytes, bytesKnown := reclaimDecision(path, now, unmarkedMaxAge)
		if !dead {
			rep.Kept = append(rep.Kept, e.Name())
			slog.Debug("scenario: temp dir kept", "dir", path, "reason", reason)
			continue
		}
		if !bytesKnown {
			// A dir with a readable marker was not walked for the decision:
			// account its bytes before taking it (one walk, then remove).
			var err error
			bytes, err = dirBytes(path)
			if err != nil {
				// Cannot measure what we would take: keep it and retry next
				// time. Never remove what we could not account.
				slog.Error("scenario: temp dir kept, size unreadable", "dir", path, "error", err.Error())
				rep.Kept = append(rep.Kept, e.Name())
				continue
			}
		}
		if err := os.RemoveAll(path); err != nil {
			slog.Error("scenario: temp dir kept, remove failed", "dir", path, "error", err.Error())
			rep.Kept = append(rep.Kept, e.Name())
			continue
		}
		rep.Removed = append(rep.Removed, RemovedDir{Name: e.Name(), Bytes: bytes})
		rep.Bytes += bytes
	}
	return rep
}

// hasTempPrefix reports whether name starts with one of the harness's own
// per-run temp dir prefixes.
func hasTempPrefix(name string) bool {
	for _, p := range tempDirPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// reclaimDecision reports whether the prefixed dir at path may be removed,
// the reason a kept dir is kept, and — for the unmarked case, where the
// walk for the mtime already accounted the bytes — the size it holds.
// dead=true is returned only for the two cases the harness owns: a
// readable marker whose owner is provably gone, or no readable marker at
// all plus a newest mtime older than the age floor.
func reclaimDecision(path string, now time.Time, unmarkedMaxAge time.Duration) (reason string, dead bool, bytes int64, bytesKnown bool) {
	raw, err := os.ReadFile(filepath.Join(path, ownerMarkerName))
	if err != nil {
		// No readable marker: pre-marker build, or the marker write raced a
		// crash. Conservative: remove only when the newest modification is
		// older than the age floor. The walk takes the bytes too, so the
		// removal path does not walk a second time.
		var newest time.Time
		if bytes, newest, err = newestMtime(path); err != nil {
			return "unmarked dir unreadable", false, 0, false
		}
		if now.Sub(newest) <= unmarkedMaxAge {
			return "no readable owner marker, too recent to reclaim", false, 0, false
		}
		return "", true, bytes, true
	}
	var owner tempOwner
	if err := json.Unmarshal(raw, &owner); err != nil {
		return "corrupt owner marker", false, 0, false
	}
	if ownerProcessAlive(owner.PID, owner.StartTicks) {
		return fmt.Sprintf("owner pid %d is alive", owner.PID), false, 0, false
	}
	return fmt.Sprintf("owner pid %d is dead", owner.PID), true, 0, false
}

// dirBytes sums the sizes of every regular file under path (the bytes a
// remove would actually free, minus directory entries the fs reclaims on
// its own).
func dirBytes(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// newestMtime walks path and returns its total size plus the newest
// modification time of anything under it. A dir being written is a dir whose
// newest mtime is now — which is exactly why the unmarked rule measures the
// NEWEST mtime, not the dir's own (adding a file to the top level updates
// the top level, but a crash mid-recursion leaves deep files untouched).
func newestMtime(path string) (bytes int64, newest time.Time, err error) {
	walkErr := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			bytes += info.Size()
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if walkErr != nil {
		return 0, time.Time{}, walkErr
	}
	return bytes, newest, nil
}

// ownerProcessAlive reports whether the process named by the marker is still
// the one that wrote it. The start-ticks comparison (where the OS exposes
// them) is what defeats PID reuse: a live PID whose start time differs from
// the marker's is a different, newer process.
func ownerProcessAlive(pid int, startTicks int64) bool {
	if pid <= 0 {
		return false
	}
	if cur := processStartTicks(pid); cur > 0 && startTicks > 0 {
		return cur == startTicks
	}
	return pidAlive(pid)
}
