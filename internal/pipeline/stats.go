package pipeline

import (
	"sort"
	"sync"
	"time"
)

// Latency is measured on wall clocks and reported in buckets, not as raw samples:
// docs/spec/05 "Timings and benchmarks" asks for p50/p95 of frame→ocr,
// ocr→ranked and frame→committed, and a 4-core appliance cannot afford to retain
// every sample of a 30 fps run to answer it. Bucket edges are fixed, so a p95 is
// reproducible across runs and comparable across builds — at the cost of only
// bucket resolution, which is what the reported edge value means and what the
// doc comment on Percentile repeats rather than hides.
var latencyBucketsMS = []int64{1, 2, 5, 10, 20, 30, 50, 75, 100, 150, 250, 400, 600, 1000, 2000, 5000}

// hist is a mutex-guarded bucketed counter. The Runner holds three; they are only
// written by the goroutine that owns the corresponding stage transition, but the
// lock is what lets Stats() be called from an HTTP handler mid-run without
// stopping that stage.
type hist struct {
	mu      sync.Mutex
	n       uint64
	sumUS   int64
	max     int64
	buckets []int64 // one per latencyBucketsMS edge, plus an overflow slot
}

func newHist() *hist { return &hist{buckets: make([]int64, len(latencyBucketsMS)+1)} }

// observe books one duration. A negative interval (a timestamp source that went
// backwards, e.g. a replay whose manifest was spliced) is booked at zero rather
// than dropped, because a dropped sample makes p95 optimistic — the failure mode
// this table exists to catch.
func (h *hist) observe(d time.Duration) {
	if d < 0 {
		d = 0
	}
	us := d.Microseconds()
	h.mu.Lock()
	h.n++
	h.sumUS += us
	if us > h.max {
		h.max = us
	}
	i := sort.Search(len(latencyBucketsMS), func(i int) bool {
		return latencyBucketsMS[i]*1000 >= us
	})
	h.buckets[i]++
	h.mu.Unlock()
}

func (h *hist) summary() LatencySummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := LatencySummary{Count: h.n}
	if h.n == 0 {
		return s
	}
	s.Mean = time.Duration(h.sumUS/int64(h.n)) * time.Microsecond
	s.Max = time.Duration(h.max) * time.Microsecond
	s.P50 = h.percentileLocked(0.50)
	s.P95 = h.percentileLocked(0.95)
	return s
}

// percentileLocked returns the bucket edge at or above the p-th sample. The final
// slot has no edge, so an overflow sample reports the last named edge — the
// honest reading of ">5s" as a number that is at least not invented.
func (h *hist) percentileLocked(p float64) time.Duration {
	want := int64(p*float64(h.n) + 0.999999)
	if want < 1 {
		want = 1
	}
	var cum int64
	for i, c := range h.buckets {
		cum += c
		if cum >= want {
			if i >= len(latencyBucketsMS) {
				return time.Duration(latencyBucketsMS[len(latencyBucketsMS)-1]) * time.Millisecond
			}
			return time.Duration(latencyBucketsMS[i]) * time.Millisecond
		}
	}
	return time.Duration(latencyBucketsMS[len(latencyBucketsMS)-1]) * time.Millisecond
}

// LatencySummary is one stage's readable latency table row.
type LatencySummary struct {
	Count uint64        `json:"count"`
	P50   time.Duration `json:"p50_ns"`
	P95   time.Duration `json:"p95_ns"`
	Mean  time.Duration `json:"mean_ns"`
	Max   time.Duration `json:"max_ns"`
}

// Latency holds the three stage intervals docs/spec/05 names.
type Latency struct {
	FrameToOCR       LatencySummary `json:"frame_to_ocr"`
	OCRToRanked      LatencySummary `json:"ocr_to_ranked"`
	FrameToCommitted LatencySummary `json:"frame_to_committed"`
}

// Stats is the Runner's in-memory self-report, returned by Runner.Stats(). The
// counters answer the two questions docs/spec/16's M2 gate asks with numbers:
// did a repeated observation re-speak (Commits vs. observations) and were
// out-of-order results discarded (OCRStale, RankingStale) rather than applied.
type Stats struct {
	// FramesDelivered counts frames the capture loop received from the source.
	FramesDelivered uint64 `json:"frames_delivered"`
	// FramesDropped counts frames the queue refused — overwritten in the
	// single-slot queue under QueueLatest, shed by the bound under
	// QueueLossless. Always 0 for a healthy lossless replay.
	FramesDropped uint64 `json:"frames_dropped"`
	// FramesProcessed counts frames that reached the ROI stage.
	FramesProcessed uint64 `json:"frames_processed"`

	// Observations counts observations emitted toward the reducer (one per
	// processed frame per region, plus the reuse observations that skip OCR).
	Observations uint64 `json:"observations"`

	// OCRCalls counts engine invocations actually made.
	OCRCalls uint64 `json:"ocr_calls"`
	// OCRSkippedByHash counts regions whose crop hash equalled the last one, so
	// no OCR ran — the duplicate-suppression number docs/spec/05 requires, and
	// the reason OCRCalls < FramesProcessed in a stable box.
	OCRSkippedByHash uint64 `json:"ocr_skipped_by_hash"`
	// OCRCancelled counts recognitions killed because a newer frame of the same
	// region showed different pixels, or because Run is shutting down.
	OCRCancelled uint64 `json:"ocr_cancelled"`
	// OCRStale counts results that came back after the region had moved on. They
	// are dropped rather than applied; the counter is what proves the drop.
	OCRStale uint64 `json:"ocr_stale"`
	// OCRErrors counts recognitions that failed for a reason other than
	// cancellation (timeout, engine failure, unavailable).
	OCRErrors uint64 `json:"ocr_errors"`
	// OCRFallbackFrames counts reads that went through the VLM fallback path
	// (docs/spec/05 "compare lightweight OCR against optional VLM fallback",
	// ADR-0039): the read carries both readings and the discrepancy. The
	// number of frames where the fallback engaged, for the report and the
	// developer UI.
	OCRFallbackFrames uint64 `json:"ocr_fallback_frames"`
	// OCRFallbackSecondaryCalls counts remote secondary calls actually made —
	// cache hits and rate-limited frames are not included: this is the number
	// that bills the gateway, and the one the rate limit is measured against.
	OCRFallbackSecondaryCalls uint64 `json:"ocr_fallback_secondary_calls"`

	// ReplayDegradedFrames counts frames whose OCR read still failed after the
	// replay retry policy (ModeReplay only; a clean live run always reports 0).
	// A clean replay is the recording it claims to be (docs/spec/12 "Frame
	// replay"): any frame here means a frame's text was lost, and the
	// scenario's implicit replay_not_degraded assertion fails the report on
	// exactly this number.
	ReplayDegradedFrames uint64 `json:"replay_degraded_frames"`
	// ReplayDegradedFrameSeqs names the degraded frames, oldest first, bounded
	// FIFO: the count is what the assertion reads, the list is what a reviewer
	// names in a report.
	ReplayDegradedFrameSeqs []int64 `json:"replay_degraded_frame_seqs,omitempty"`

	// Rankings counts candidate rankings completed.
	Rankings uint64 `json:"rankings"`
	// RankingStale counts ranking replies the reducer refused (stale_observation,
	// stale_text, stale_generation). Mirrors session.Stats.Stale for rankings.
	RankingStale uint64 `json:"ranking_stale"`
	// RankingsAbstained counts rankings whose answer was ABSTAINED.
	RankingsAbstained uint64 `json:"rankings_abstained"`

	// Decisions counts the locally ambiguous rankings offered to the
	// configured Decider (docs/spec/05 "optional Clef/VLM adjudication",
	// ADR-0050). It is the number of adjudication requests the run asked,
	// not the number of provider calls (a breaker-open or not-configured
	// role still asks, and abstains on the reply).
	Decisions uint64 `json:"decisions"`
	// DecisionApplied counts adjudication answers the session's fences
	// accepted and that the commitment policy could read: the answers that
	// actually drove (or vetted) a commit.
	DecisionApplied uint64 `json:"decision_applied"`
	// DecisionFenced counts answers the session discarded — a reply that
	// landed after a newer commit (stale_generation), a redelivered one
	// (duplicate_seq), a superseded observation, or an answer that could not
	// be applied at all. It must stay distinct from DecisionApplied: a
	// discarded answer never narrows a commit.
	DecisionFenced uint64 `json:"decision_fenced"`

	// Commits counts message.committed events this pipeline's reducer emitted.
	Commits uint64 `json:"commits"`
	// Events counts every envelope published through the reducer.
	Events uint64 `json:"events"`
	// StaleApplied counts messages that were *applied* despite naming a stale
	// generation. It must stay 0: docs/spec/01 invariant 2 makes the generation
	// fence absolute, and this is the assertion that would notice it broken.
	StaleApplied uint64 `json:"stale_generation_applied"`

	Latency Latency `json:"latency"`
}

// counters is the mutable half of Stats. One mutex for the whole struct is
// deliberate: the counters are read together by an operator looking for a
// pattern (high drops + high staleness = one slow worker), and a lock per field
// would let them read a torn version of that story.
type counters struct {
	mu sync.Mutex
	s  Stats

	frameOCR  *hist
	ocrRank   *hist
	frameComm *hist

	traceLog []TraceEntry
	traceOn  bool
	traceMax int
}

func newCounters(trace bool) *counters {
	return &counters{
		frameOCR:  newHist(),
		ocrRank:   newHist(),
		frameComm: newHist(),
		traceOn:   trace,
		traceMax:  512,
	}
}

// TraceEntry is one row of the frame→state→text→candidate→event timeline. It
// exists so docs/spec/16's evidence bundle and docs/spec/09's developer inspector
// can reconstruct one occurrence from the run itself rather than from a log
// scraped after the fact; the frame-replay test prints exactly these rows.
type TraceEntry struct {
	FrameSeq           int64   `json:"frame_seq"`
	CaptureTimestampNS int64   `json:"capture_timestamp_ns"`
	RegionID           string  `json:"region_id"`
	RegionPresent      bool    `json:"region_present"`
	RegionHash         string  `json:"region_hash"`
	OCRSkipped         bool    `json:"ocr_skipped"`
	OCRText            string  `json:"ocr_text,omitempty"`
	State              string  `json:"text_state"`
	AcceptedText       string  `json:"accepted_text,omitempty"`
	TopCandidate       string  `json:"top_candidate,omitempty"`
	TopScore           float64 `json:"top_score,omitempty"`
	Reason             string  `json:"reason,omitempty"`
	Event              string  `json:"event,omitempty"`
	// OCRFallback is the ocr.fallback trace field (docs/spec/05 "include both
	// and the discrepancy", ADR-0039): the primary's confidence, the
	// normalised distance between the two readings, which read won and why.
	// nil when the read took the single-engine path. The raw readings stay on
	// the ocr.Result side; this row carries what the developer UI shows.
	OCRFallback *OCRFallbackTrace `json:"ocr_fallback,omitempty"`
}

// OCRFallbackTrace is one fallback annotation on a trace row. Chosen is
// "primary" or "secondary"; Reason is the one-phrase decision (the full
// vocabulary is on ocr.FallbackOutcome.Reason).
type OCRFallbackTrace struct {
	PrimaryConf float64 `json:"primary_conf"`
	Discrepancy float64 `json:"discrepancy"`
	Chosen      string  `json:"chosen"`
	Reason      string  `json:"reason,omitempty"`
	// SecondaryText is the VLM's own reading, kept on the record beside the
	// primary's (docs/spec/05: "include both and the discrepancy") so the
	// developer UI and the evidence bundle show what each engine saw; empty
	// when the secondary was not called this frame (rate-limited or failed,
	// SecondaryReason then says why).
	SecondaryText   string `json:"secondary_text,omitempty"`
	SecondaryEngine string `json:"secondary_engine,omitempty"`
}

func (c *counters) add(f func(*Stats)) {
	c.mu.Lock()
	f(&c.s)
	c.mu.Unlock()
}

func (c *counters) framesDelivered(n uint64) { c.add(func(s *Stats) { s.FramesDelivered += n }) }
func (c *counters) framesDropped(n uint64)   { c.add(func(s *Stats) { s.FramesDropped += n }) }
func (c *counters) framesProcessed()         { c.add(func(s *Stats) { s.FramesProcessed++ }) }
func (c *counters) observations(n uint64)    { c.add(func(s *Stats) { s.Observations += n }) }
func (c *counters) ocrCall()                 { c.add(func(s *Stats) { s.OCRCalls++ }) }
func (c *counters) ocrSkipped()              { c.add(func(s *Stats) { s.OCRSkippedByHash++ }) }
func (c *counters) ocrCancelled()            { c.add(func(s *Stats) { s.OCRCancelled++ }) }
func (c *counters) ocrStale()                { c.add(func(s *Stats) { s.OCRStale++ }) }
func (c *counters) ocrError()                { c.add(func(s *Stats) { s.OCRErrors++ }) }
func (c *counters) ocrFallbackFrames()       { c.add(func(s *Stats) { s.OCRFallbackFrames++ }) }
func (c *counters) ocrFallbackSecondary()    { c.add(func(s *Stats) { s.OCRFallbackSecondaryCalls++ }) }

// replayDegraded books one frame whose read the replay could not recover after
// its retry budget. Rotations materialise a fresh backing array so a Stats copy
// returned earlier never sees one of its slots rewritten under it.
func (c *counters) replayDegraded(frameSeq int64) {
	c.mu.Lock()
	c.s.ReplayDegradedFrames++
	seqs := c.s.ReplayDegradedFrameSeqs
	if len(seqs) >= maxReplayDegradedSeqs {
		next := make([]int64, 0, maxReplayDegradedSeqs)
		c.s.ReplayDegradedFrameSeqs = append(next, seqs[1:]...)
	}
	c.s.ReplayDegradedFrameSeqs = append(c.s.ReplayDegradedFrameSeqs, frameSeq)
	c.mu.Unlock()
}

// maxReplayDegradedSeqs bounds the retained list of degraded frame sequences.
const maxReplayDegradedSeqs = 64

func (c *counters) ranking()          { c.add(func(s *Stats) { s.Rankings++ }) }
func (c *counters) rankingStale()     { c.add(func(s *Stats) { s.RankingStale++ }) }
func (c *counters) rankingAbstained() { c.add(func(s *Stats) { s.RankingsAbstained++ }) }
func (c *counters) decisions()        { c.add(func(s *Stats) { s.Decisions++ }) }
func (c *counters) decisionApplied()  { c.add(func(s *Stats) { s.DecisionApplied++ }) }
func (c *counters) decisionFenced()   { c.add(func(s *Stats) { s.DecisionFenced++ }) }
func (c *counters) commits(n uint64)  { c.add(func(s *Stats) { s.Commits += n }) }
func (c *counters) events(n uint64)   { c.add(func(s *Stats) { s.Events += n }) }
func (c *counters) staleApplied()     { c.add(func(s *Stats) { s.StaleApplied++ }) }

func (c *counters) snapshot() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.s
	s.Latency = Latency{
		FrameToOCR:       c.frameOCR.summary(),
		OCRToRanked:      c.ocrRank.summary(),
		FrameToCommitted: c.frameComm.summary(),
	}
	return s
}

func (c *counters) recordTrace(e TraceEntry) {
	if !c.traceOn {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.traceLog) >= c.traceMax {
		copy(c.traceLog, c.traceLog[1:])
		c.traceLog = c.traceLog[:len(c.traceLog)-1]
	}
	c.traceLog = append(c.traceLog, e)
}

func (c *counters) traceSnapshot() []TraceEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]TraceEntry(nil), c.traceLog...)
}
