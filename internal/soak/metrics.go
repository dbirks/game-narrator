// metrics.go is the resource sampling the soak takes every SampleEvery:
// VmRSS from /proc/self/status, the goroutine count, and the open-FD count
// from /proc/self/fd (docs/spec/10 "Resource budgets": the UNO Q 4 GB budget
// is only checkable if the run can say what it used). The leak detector is a
// least-squares slope over the RSS samples taken after the 10% mark: the
// early run is warm-up (page cache, lazy maps), and a straight-line fit over
// the rest is what separates a one-time jump from a monotonic leak.

package soak

import (
	"bufio"
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

// rssKB reads VmRSS from /proc/self/status. On a non-Linux host (never the
// soak's target) it returns 0 and the RSS verdict reports NOT MEASURED.
func rssKB() int64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "kB" {
			return 0
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return v
	}
	return 0
}

// fdCount counts the process's open file descriptors via /proc/self/fd.
// On a non-Linux host it returns 0 and the FD verdict reports NOT MEASURED.
func fdCount() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

// snap is one (t, rss) point for the slope fit.
type snap struct {
	t   time.Time
	rss int64 // KiB
}

// warmUpMark is the fraction of the run before leak fits start: 10% as the
// task's mark, with a 30 s floor, because one-time warm-up allocations
// (pipeline start, page cache, lazy maps, race runtime) settle in tens of
// seconds, not fractions of a short run.
func warmUpMark(cfg Config) time.Duration {
	if w := time.Duration(float64(cfg.Duration) * 0.10); w > 30*time.Second {
		return w
	}
	return 30 * time.Second
}

// leakSlopeMiBPerMin is the monotonic-growth detector over the post-warm-up
// samples: the Theil–Sen estimate (median of the pairwise slopes) in MiB per
// minute. A steady leak lifts every sample together, so the median pairwise
// slope lands at the leak rate; zero-drift GC jitter oscillates symmetrically
// around a flat baseline, so its median pairwise slope is ≈ 0 — neither the
// running-max envelope (which ratchets on every sawtooth peak) nor a plain
// least-squares fit (which the jitter's one-sided extremes skew) survives a
// short window. With fewer than three samples the fit is not meaningful and
// returns 0 (the caller reports the fit as not run).
func leakSlopeMiBPerMin(pts []snap) float64 {
	if len(pts) < 3 {
		return 0
	}
	slopes := make([]float64, 0, len(pts)*(len(pts)-1)/2)
	for i := 0; i < len(pts); i++ {
		for j := i + 1; j < len(pts); j++ {
			dt := pts[j].t.Sub(pts[i].t).Seconds()
			if dt <= 0 {
				continue
			}
			slopes = append(slopes, float64(pts[j].rss-pts[i].rss)/1024*60/dt) // MiB/min
		}
	}
	if len(slopes) == 0 {
		return 0
	}
	sort.Float64s(slopes)
	mid := len(slopes) / 2
	if len(slopes)%2 == 1 {
		return slopes[mid]
	}
	return (slopes[mid-1] + slopes[mid]) / 2
}

// sample is one row of the soak's resource table.
type sample struct {
	T          time.Time `json:"t"`
	RSSKB      int64     `json:"rss_kb"`
	Goroutines int       `json:"goroutines"`
	FDs        int       `json:"fds"`
	// Cumulative counters at the sample moment (harness + pipeline).
	Commits         int64  `json:"commits"`
	SidecarCommits  int64  `json:"sidecar_commits"`
	FramesDelivered uint64 `json:"frames_delivered"`
	FramesDropped   uint64 `json:"frames_dropped"`
	SpeechStarted   int64  `json:"speech_started"`
	SpeechEnded     int64  `json:"speech_ended"`
	ProviderErrors  int64  `json:"provider_errors"`
	FaultsFired     int64  `json:"faults_fired"`
	RecoveredPanics int64  `json:"recovered_panics"`
}

// sampler pulls one sample every period until the context is done. The first
// sample is taken immediately: the run's zero point must be on the table.
func sampler(ctx context.Context, period time.Duration, take func() sample, out chan<- sample) {
	defer close(out)
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		out <- take()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ringStore is a bounded events.Store for the soak's event hub: the hub's
// replay log must not grow with the run (docs/spec/02 "bounded operational
// store", docs/spec/10 "session event log buffered"). It keeps the newest n
// envelopes and answers After from the ring only; a cursor older than the
// ring is a resync, which is exactly what a bounded log owes its readers.
type ringStore struct {
	capacity int
	ring     []events.Envelope
	next     int // next write slot
	filled   bool
	lastSeq  int64
}

func newRingStore(n int) *ringStore {
	if n <= 0 {
		n = 1024
	}
	return &ringStore{capacity: n, ring: make([]events.Envelope, n)}
}

// Append implements events.Store.
func (s *ringStore) Append(_ context.Context, e events.Envelope) (int64, error) {
	// Envelopes arrive with Seq 0 from the hub: the store owns numbering (the
	// hub's contract, mirrored in MemStore.Append). Defaults are already
	// applied by the hub, so assign the seq first, then validate — a full
	// Validate on a zero seq would reject every event.
	e.Seq = s.lastSeq + 1
	if err := e.Validate(); err != nil {
		return 0, err
	}
	s.lastSeq = e.Seq
	s.ring[s.next] = e
	s.next = (s.next + 1) % s.capacity
	s.filled = true
	return e.Seq, nil
}

// After implements events.Store: seq > afterSeq, ascending, limited to the
// ring's contents. Events the ring has forgotten are not recoverable — the
// hub turns that into a resync.
func (s *ringStore) After(_ context.Context, afterSeq int64, f events.Filter, limit int) ([]events.Envelope, error) {
	if limit <= 0 {
		limit = events.DefaultStoreLimit
	}
	if !s.filled {
		return nil, nil
	}
	n := s.capacity
	if s.lastSeq < int64(n) {
		n = int(s.lastSeq)
	}
	var out []events.Envelope
	// Walk the ring oldest-first: the oldest slot is `next` once the ring is
	// full, otherwise 0.
	start := s.next
	if s.lastSeq < int64(s.capacity) {
		start = 0
	}
	for i := 0; i < n; i++ {
		e := s.ring[(start+i)%s.capacity]
		if e.Seq <= afterSeq {
			continue
		}
		if !matchesFilter(f, e) {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// matchesFilter is the soak's copy of the hub's filter semantics (SessionID
// restricts to one session, Names to the named set; a zero filter matches
// everything): the ring store is the hub's only reader-facing surface, so it
// must answer filters the way the ring would.
func matchesFilter(f events.Filter, e events.Envelope) bool {
	if f.SessionID != "" && e.SessionID != f.SessionID {
		return false
	}
	if len(f.Names) == 0 {
		return true
	}
	for _, n := range f.Names {
		if n == e.Name {
			return true
		}
	}
	return false
}
