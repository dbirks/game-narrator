// heapprofile.go is the soak's opt-in heap-profile capture for leak hunting
// (AGENTS.md "first reproduce"): GAME_NARRATOR_SOAK_HEAPPROFILE=dir writes a
// pprof heap profile at the run's warm-up mark (the 10% point the rss_growth
// verdict starts measuring from) and one at the end (after teardown, while
// every run-retained structure is still live). The diff between the two,
// `go tool pprof -top -diff_base early.pb.gz late.pb.gz`, names the growing
// allocation sites instead of guessing them.
//
// Capture is off unless the env var names a directory; nothing in the normal
// soak path changes.

package soak

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync"
)

// heapProfileEnv is the opt-in switch: the directory the profiles land in.
const heapProfileEnv = "GAME_NARRATOR_SOAK_HEAPPROFILE"

// heapProfiler captures the two profiles of one run. It is nil when the
// capture is off, so the run path stays untouched.
type heapProfiler struct {
	dir string

	mu        sync.Mutex
	earlyPath string
	latePath  string
	errs      []string
}

// newHeapProfiler prepares the directory; a directory that cannot be created
// is reported in the run's error rather than swallowed (a soak that cannot
// write its evidence is a failed evidence run).
func newHeapProfiler(dir string) (*heapProfiler, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &heapProfiler{dir: dir}, nil
}

// write takes one profile: runtime.GC first, so the profile is the heap the
// collector left behind (live objects), not garbage about to be swept.
func (p *heapProfiler) write(tag string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	runtime.GC()
	path := filepath.Join(p.dir, tag+".pb.gz")
	f, err := os.Create(path)
	if err != nil {
		p.errs = append(p.errs, tag+": create "+path+": "+err.Error())
		return
	}
	if err := pprof.WriteHeapProfile(f); err != nil {
		p.errs = append(p.errs, tag+": write "+path+": "+err.Error())
	}
	_ = f.Close()
	if tag == "early" {
		p.earlyPath = path
	} else {
		p.latePath = path
	}
}

// paths returns the written profile paths for the report (order: early, late).
func (p *heapProfiler) paths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	if p.earlyPath != "" {
		out = append(out, p.earlyPath)
	}
	if p.latePath != "" {
		out = append(out, p.latePath)
	}
	return out
}

func (p *heapProfiler) errors() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.errs...)
}
