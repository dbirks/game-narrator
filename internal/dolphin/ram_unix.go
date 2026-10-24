//go:build unix

package dolphin

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// OpenMem1 opens the emulated MEM1 arena of a running Dolphin process
// read-only. No ptrace, no writes: the arena is located in
// /proc/<pid>/maps and copied out (4 MiB, bounded) — from the backing
// /dev/shm file when it is still named there (a fast, permission-free
// path), else via /proc/<pid>/mem (same user or CAP_SYS_PTRACE only).
//
// Region selection (measured on this host, see ADR-0173): Dolphin maps
// its fastmem arena from /dev/shm/dolphin-emu.<pid> as several large VMAs
// at file offset 0 — MEM1 is the first 4 MiB of that file. We parse
// /proc/<pid>/maps, take every mapping at least mem1Size long, order
// them (fastmem mappings whose file offset is 0 first, then the rest in
// map order) and validate each with the live game markers (Mem1.valid);
// the first valid one wins.
//
// Deferred markers: the emulated state markers are NOT live while the
// game boots / loads its savestate (measured: the first game frame
// appears ~1 s in, but the player markers only became live 10–20 s
// later on this host, run to run). When no candidate validates but the
// NAMED arena mapping (/dev/shm/dolphin-emu.*, file offset 0) exists,
// OpenMem1 returns a handle on it anyway — the region is identified by
// the mapping, not the markers — and its reads return the typed
// ErrMem1Unrecognized until the markers go live. Callers that poll
// (the route step loops) then simply wait for the boot; readers of a
// game that never boots get a stable typed error.
func OpenMem1(pid int) (*Mem1, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("dolphin: mem1: %w", ErrMem1NoProcess)
	}
	proot := filepath.Join("/proc", strconv.Itoa(pid))
	mapsF, err := os.Open(filepath.Join(proot, "maps"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("dolphin: mem1: %w (pid %d)", ErrMem1NoProcess, pid)
		}
		return nil, fmt.Errorf("dolphin: mem1: maps: %w", err)
	}
	defer mapsF.Close()

	var cands []mapsRegion
	sc := bufio.NewScanner(mapsF)
	sc.Buffer(make([]byte, 1024), 1<<20)
	for sc.Scan() {
		r, ok := parseMapsLine(sc.Text())
		if !ok || r.size < mem1Size {
			continue
		}
		cands = append(cands, r)
	}
	if sc.Err() != nil {
		return nil, fmt.Errorf("dolphin: mem1: maps: %w", sc.Err())
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("dolphin: mem1: %w (pid %d)", ErrMem1NotFound, pid)
	}
	// Fastmem mappings of the emulated arena (file offset 0 holds MEM1)
	// first; the rest in map order. Maps order is stable, so this keeps
	// the arena front of the list in practice too.
	sort.SliceStable(cands, func(i, j int) bool {
		return arenaRank(cands[i]) < arenaRank(cands[j])
	})

	var tried []string
	var lastErr error
	var deferred *mapsRegion
	for _, r := range cands {
		read, closer, err := openArenaReader(pid, &r, snapshotSize(&r))
		if err != nil {
			lastErr = err
			tried = append(tried, fmt.Sprintf("%s: %v", r.rangeString(), err))
			continue
		}
		data, verr := read()
		closer()
		if verr != nil {
			lastErr = verr
			tried = append(tried, fmt.Sprintf("%s: %v", r.rangeString(), verr))
			continue
		}
		if valid(data) {
			// Re-open for the long-lived handle.
			read2, closer2, err2 := openArenaReader(pid, &r, snapshotSize(&r))
			if err2 != nil {
				return nil, err2
			}
			return &Mem1{pid: pid, read: read2, closer: closer2}, nil
		}
		// The named arena with not-yet-live markers: keep the LARGEST
		// offset-0 arena VMA (the one covering the most emulated RAM).
		if isArenaMapping(&r) && (deferred == nil || r.size > deferred.size) {
			deferred = &r
		}
		if len(tried) < maxTried {
			tried = append(tried, fmt.Sprintf("%s: markers not live", r.rangeString()))
		}
	}
	// Deferred: the arena is identified by its mapping; the emulated
	// markers will be checked on every read instead (see the doc).
	if deferred != nil {
		read2, closer2, err2 := openArenaReader(pid, deferred, snapshotSize(deferred))
		if err2 != nil {
			return nil, err2
		}
		return &Mem1{pid: pid, read: read2, closer: closer2}, nil
	}
	if len(tried) > maxTried {
		tried = append(tried, fmt.Sprintf("…and %d more candidate region(s)", len(cands)-maxTried))
	}
	_ = lastErr
	return nil, fmt.Errorf("dolphin: mem1: %w (pid %d, %d candidate region(s): %s)",
		ErrMem1Unrecognized, pid, len(cands), strings.Join(tried, "; "))
}

const maxTried = 12

// isArenaMapping reports whether r is the fastmem arena mapping: Dolphin's
// /dev/shm/dolphin-emu.<pid> shared-memory file at file offset 0 (MEM1 is
// the first 4 MiB of that file).
func isArenaMapping(r *mapsRegion) bool {
	return r.foff == 0 && strings.Contains(r.path, "/dev/shm/dolphin-emu")
}

// snapshotSize is how many bytes one snapshot pulls for r: the whole arena
// VMA for the fastmem arena (entity objects live up to ~11 MiB past MEM1,
// measured; the VMA itself is 32 MiB on this host), capped at 32 MiB;
// MEM1-only for every other candidate.
func snapshotSize(r *mapsRegion) int {
	if !isArenaMapping(r) {
		return mem1Size
	}
	n := int(r.size)
	const cap32 = 32 << 20
	if n > cap32 {
		n = cap32
	}
	return n
}

// arenaRank orders candidate regions: the fastmem arena mapping at file
// offset 0 (MEM1 = the first 4 MiB of /dev/shm/dolphin-emu.<pid>) is the
// top choice; named backing files next; anonymous regions last.
func arenaRank(r mapsRegion) int {
	base := r.path
	if i := strings.Index(base, " ("); i >= 0 {
		base = base[:i]
	}
	switch {
	case strings.Contains(base, "/dev/shm/dolphin-emu") && r.foff == 0:
		return 0
	case strings.Contains(base, "/dev/shm/dolphin-emu"):
		return 1
	case base != "":
		return 2
	default:
		return 3
	}
}

type mapsRegion struct {
	start, size uint64
	foff        uint64 // backing-file offset (0 for anonymous)
	path        string // the backing file ("/dev/shm/…" or "" for anon)
}

func (r mapsRegion) rangeString() string {
	return fmt.Sprintf("[0x%x-0x%x)", r.start, r.start+r.size)
}

func parseMapsLine(line string) (mapsRegion, bool) {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return mapsRegion{}, false
	}
	bounds := strings.SplitN(fields[0], "-", 2)
	if len(bounds) != 2 {
		return mapsRegion{}, false
	}
	start, err1 := strconv.ParseUint(bounds[0], 16, 64)
	end, err2 := strconv.ParseUint(bounds[1], 16, 64)
	if err1 != nil || err2 != nil || end <= start {
		return mapsRegion{}, false
	}
	r := mapsRegion{start: start, size: end - start}
	if fo, err := strconv.ParseUint(fields[2], 16, 64); err == nil {
		r.foff = fo
	}
	// The path, if any, is everything after the 5th column; it may be
	// whitespace-separated in older kernels, so rejoin from the raw line.
	if idx := strings.Index(line, fields[4]); idx >= 0 {
		if p := strings.TrimSpace(line[idx+len(fields[4]):]); p != "" {
			r.path = p
		}
	}
	return r, true
}

// openArenaReader returns a reader that pulls one fresh bounded arena
// snapshot each call, plus a closer that releases the source handles.
func openArenaReader(pid int, r *mapsRegion, n int) (func() ([]byte, error), func(), error) {
	// Path 1: a still-named backing file (the /dev/shm case) — no
	// permission dance at all; the handle stays open for re-reads.
	// The emulated MEM1 arena sits at the mapping's file offset.
	if r.path != "" && !strings.HasPrefix(r.path, "[") {
		f, err := os.OpenFile(r.path, os.O_RDONLY, 0)
		if err == nil {
			st, serr := f.Stat()
			if serr == nil && st.Size() >= int64(r.foff)+int64(n) {
				return func() ([]byte, error) {
					arena := make([]byte, n)
					_, rerr := f.ReadAt(arena, int64(r.foff))
					return arena, rerr
				}, func() { _ = f.Close() }, nil
			}
			_ = f.Close()
		}
	}
	// Path 2: /proc/<pid>/mem — a plain pread per snapshot; no PTRACE
	// call is made, but the kernel still applies the ptrace access
	// check, so a denied open is a typed ErrMem1Access, not a mystery
	// failure.
	memPath := fmt.Sprintf("/proc/%d/mem", pid)
	probe, err := os.OpenFile(memPath, os.O_RDONLY, 0)
	if err != nil {
		if isPermission(err) {
			return nil, nil, fmt.Errorf("dolphin: mem1: %w (%s; run the test as the launching process's user or root)", ErrMem1Access, r.rangeString())
		}
		return nil, nil, fmt.Errorf("dolphin: mem1: /proc mem: %w", err)
	}
	_ = probe.Close()
	return func() ([]byte, error) {
		f, ferr := os.OpenFile(memPath, os.O_RDONLY, 0)
		if ferr != nil {
			return nil, ferr
		}
		defer f.Close()
		arena := make([]byte, n)
		got, err := readAtLoop(f, arena, int64(r.start))
		if err != nil {
			return nil, fmt.Errorf("dolphin: mem1: read %s: %w", r.rangeString(), err)
		}
		// A read that ended short of the arena's end hit an unmapped guard
		// page before the arena's tail; that is an access condition the
		// caller must not read past.
		if got < n {
			return nil, fmt.Errorf("dolphin: mem1: %w (short read %d/%d at %s)", ErrMem1Access, got, n, r.rangeString())
		}
		return arena, nil
	}, func() {}, nil
}

func isPermission(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "operation not permitted"))
}

// readAtLoop reads len(buf) bytes from f at abs in one bounded pass.
// Measured /proc/<pid>/mem behavior (ADR-0173, synthetic 32 MiB mapping
// with guard holes): a plain Read stops at the first unmapped page and
// returns the mapped prefix with err == nil, while ReadAt returns the
// mapped prefix plus EIO; a read that starts inside a hole returns
// 0 bytes with EIO and cannot be advanced. Returns (got, nil) when the
// whole buffer was collected or readable memory ended cleanly, and
// (got, err) when an unmapped page stopped the read; got < len(buf)
// always means the arena tail is unreadable (the caller reports
// ErrMem1Access rather than reading zeros).
func readAtLoop(f *os.File, buf []byte, abs int64) (int, error) {
	n, err := f.ReadAt(buf, abs)
	if err != nil && n == len(buf) {
		return n, nil // full buffer; trailing EIO is noise
	}
	return n, err
}
