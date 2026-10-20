package telemetry

import (
	"strings"
	"time"
)

// DefaultBuckets is the product catalog's histogram edge set, seconds:
// 1 ms to 5 s, the same edges the pipeline's own latency table uses
// (internal/pipeline/stats.go), so a number this package reports and a number
// the run's stats report read off the same buckets. The OCR stage, the
// adjudication and the TTS call all live inside these bounds on the
// appliance; a sample past 5 s lands in the overflow slot and says so.
var DefaultBuckets = []float64{
	0.001, 0.002, 0.005, 0.01, 0.02, 0.03, 0.05, 0.075,
	0.1, 0.15, 0.25, 0.4, 0.6, 1, 2, 5,
}

// Product is the process's metric catalog (docs/spec/13 "Metrics"): the full
// set of product counters, histograms and gauges declared once at process
// start, every one labelled by the two bounded dimensions (game,
// profile_revision) except the process-wide gauges. NewProduct builds one per
// process; the hot paths call the hooks below, which are nil-safe — a nil
// *Product is the documented "off" shape, so a harness that wires no catalog
// keeps the same call sites and pays nothing.
//
// The catalog is deliberately the spec's named counter list, no more
// (docs/spec/13 "Metrics"): captured/dropped frames, OCR attempts and
// fallbacks, matcher outcomes, model decisions with latency, cached-audio
// hits, cancellations, playback underruns, the job queue and the stage
// latencies. What is NOT here is a per-message, per-utterance or per-trace
// label: that cardinality is the failure mode docs/spec/13 names, and the
// developer UI gets those detail rows from the event log and the dev traces,
// not from this surface.
type Product struct {
	Registry *Registry

	// Frames
	framesDelivered *Counter
	framesDropped   *Counter
	// OCR
	ocrCalls     *Counter
	ocrCancelled *Counter
	ocrErrors    *Counter
	ocrFallback  *Counter
	ocrDuration  *Histogram
	// Matcher
	matcherRankings         *Counter
	matcherRankingStale     *Counter
	matcherRankingAbstained *Counter
	// Decisions (the optional Clef adjudication)
	decisions             *Counter
	decisionAnswered      *Counter
	decisionProviderError *Counter
	decisionNotConfigured *Counter
	decisionApplied       *Counter
	decisionFenced        *Counter
	decisionDuration      *Histogram
	// TTS
	ttsCacheHits   *Counter
	ttsCacheMisses *Counter
	ttsDuration    *Histogram
	// Playback
	playbackUnderruns *Counter
	prewarmHits       *Counter
	// End-to-end
	frameToVoice *Histogram
	// Gauges (process-wide)
	jobQueueDepth  *Gauge
	activeSessions *Gauge
}

// NewProduct builds the process catalog over a fresh Registry.
func NewProduct() *Product {
	reg := New()
	p := &Product{
		Registry:                reg,
		framesDelivered:         reg.Counter("frames_delivered_total", "Frames the pipeline's capture loop received."),
		framesDropped:           reg.Counter("frames_dropped_total", "Frames the queue policy refused: overwritten in the live latest-slot or shed under a bound."),
		ocrCalls:                reg.Counter("ocr_calls_total", "OCR engine invocations actually made (duplicate-suppressed frames are not counted)."),
		ocrCancelled:            reg.Counter("ocr_cancelled_total", "OCR invocations cancelled before they answered: superseded by newer pixels or a shutdown."),
		ocrErrors:               reg.Counter("ocr_errors_total", "OCR invocations that failed for a reason other than cancellation (timeout, engine failure, unavailable)."),
		ocrFallback:             reg.Counter("ocr_fallback_total", "OCR reads that engaged the VLM fallback path."),
		ocrDuration:             reg.Histogram("ocr_duration_seconds", "Wall time of an OCR invocation, dispatch to outcome, cancelled and failed reads included.", nil),
		matcherRankings:         reg.Counter("matcher_rankings_total", "Candidate rankings the reducer applied."),
		matcherRankingStale:     reg.Counter("matcher_ranking_stale_total", "Rankings the session's fences refused (stale observation or text, stale generation, duplicate)."),
		matcherRankingAbstained: reg.Counter("matcher_ranking_abstained_total", "Rankings whose local answer was ABSTAIN."),
		decisions:               reg.Counter("decisions_total", "Locally ambiguous rankings offered to the adjudicator."),
		decisionAnswered:        reg.Counter("decisions_answered_total", "Adjudications that returned a candidate choice."),
		decisionProviderError:   reg.Counter("decisions_provider_error_total", "Adjudications the provider could not answer (timeout, error)."),
		decisionNotConfigured:   reg.Counter("decisions_not_configured_total", "Adjudications abstained because the decision role is not configured."),
		decisionApplied:         reg.Counter("decisions_applied_total", "Adjudication answers the session's fences accepted."),
		decisionFenced:          reg.Counter("decisions_fenced_total", "Adjudication answers the session's fences discarded."),
		decisionDuration:        reg.Histogram("decisions_duration_seconds", "Wall time of the whole adjudication, retries included.", nil),
		ttsCacheHits:            reg.Counter("tts_cache_hits_total", "Utterances resolved from the TTS cache."),
		ttsCacheMisses:          reg.Counter("tts_cache_misses_total", "Utterances that required a fresh synthesis (the singleflight leader's share)."),
		ttsDuration:             reg.Histogram("tts_duration_seconds", "Wall time from the synthesis request to a resolved utterance, cache hits and misses together.", nil),
		playbackUnderruns:       reg.Counter("playback_underruns_total", "Playbacks that started and then ended in a sink failure: samples lost mid-utterance."),
		prewarmHits:             reg.Counter("prewarm_hits_total", "Commits resolved from audio THIS session prewarmed: the TTS round trip the commit paid nothing for."),
		frameToVoice:            reg.Histogram("frame_to_voice_seconds", "Wall time from the frame entering the pipeline to the committed line's audio starting.", nil),
		jobQueueDepth:           reg.Gauge("job_queue_depth", "Durable queue depth: jobs waiting (queued) plus in flight (running)."),
		activeSessions:          reg.Gauge("active_sessions", "Sessions the runtime currently owns."),
	}
	return p
}

// ── nil-safe hooks the hot paths call ──────────────────────────────────────
//
// Every hook nil-checks the product first: the pipeline, the speech wiring
// and the job store are all built with a nil catalog in processes that did
// not wire one, and the documented off shape is "the same call, no book".

// FramesDelivered books n frames the capture loop received.
func (p *Product) FramesDelivered(l Labels, n uint64) {
	if p == nil {
		return
	}
	p.framesDelivered.Add(l, n)
}

// FramesDropped books n frames the queue policy refused.
func (p *Product) FramesDropped(l Labels, n uint64) {
	if p == nil {
		return
	}
	p.framesDropped.Add(l, n)
}

// OCRCall books one engine invocation actually made.
func (p *Product) OCRCall(l Labels) {
	if p == nil {
		return
	}
	p.ocrCalls.Add(l, 1)
}

// OCRCancelled books one invocation cancelled before it answered.
func (p *Product) OCRCancelled(l Labels) {
	if p == nil {
		return
	}
	p.ocrCancelled.Add(l, 1)
}

// OCRError books one invocation that failed for a non-cancellation reason.
func (p *Product) OCRError(l Labels) {
	if p == nil {
		return
	}
	p.ocrErrors.Add(l, 1)
}

// OCRFallback books one read that engaged the VLM fallback path.
func (p *Product) OCRFallback(l Labels) {
	if p == nil {
		return
	}
	p.ocrFallback.Add(l, 1)
}

// OCRDuration books one invocation's wall time (dispatch to outcome; a
// cancelled or failed read is a real sample of how long the stage took).
func (p *Product) OCRDuration(l Labels, d time.Duration) {
	if p == nil {
		return
	}
	p.ocrDuration.Observe(l, d)
}

// Ranking books one ranking the reducer applied.
func (p *Product) Ranking(l Labels) {
	if p == nil {
		return
	}
	p.matcherRankings.Add(l, 1)
}

// RankingStale books one ranking the session's fences refused.
func (p *Product) RankingStale(l Labels) {
	if p == nil {
		return
	}
	p.matcherRankingStale.Add(l, 1)
}

// RankingAbstained books one ranking whose local answer was ABSTAIN.
func (p *Product) RankingAbstained(l Labels) {
	if p == nil {
		return
	}
	p.matcherRankingAbstained.Add(l, 1)
}

// Decision books one ambiguous ranking offered to the adjudicator.
func (p *Product) Decision(l Labels) {
	if p == nil {
		return
	}
	p.decisions.Add(l, 1)
}

// DecisionVerdict books the adjudication's outcome. Verdict is the reply's
// own reason (docs/spec/05's answer classes): "answered", "not_configured"
// or the "provider_error" family ("provider_error" with the router's kind
// suffix). It is a metric name, not a label — a label's cardinality is this
// catalog's one bounded dimension pair, and a reason the catalog does not
// name books nowhere rather than inventing a series.
func (p *Product) DecisionVerdict(l Labels, verdict string) {
	if p == nil {
		return
	}
	switch {
	case verdict == "answered":
		p.decisionAnswered.Add(l, 1)
	case strings.HasPrefix(verdict, "provider_error"):
		p.decisionProviderError.Add(l, 1)
	case verdict == "not_configured":
		p.decisionNotConfigured.Add(l, 1)
	}
}

// DecisionDuration books the whole adjudication's wall time (retries
// included), regardless of verdict.
func (p *Product) DecisionDuration(l Labels, d time.Duration) {
	if p == nil {
		return
	}
	p.decisionDuration.Observe(l, d)
}

// DecisionApplied books one answer the session's fences accepted.
func (p *Product) DecisionApplied(l Labels) {
	if p == nil {
		return
	}
	p.decisionApplied.Add(l, 1)
}

// DecisionFenced books one answer the session's fences discarded.
func (p *Product) DecisionFenced(l Labels) {
	if p == nil {
		return
	}
	p.decisionFenced.Add(l, 1)
}

// TTSResolved books one utterance's resolution: the cache hit or miss it was
// and its wall time (the request-to-resolution interval, which for a hit is
// the cache read and for a miss the synthesis).
func (p *Product) TTSResolved(l Labels, hit bool, d time.Duration) {
	if p == nil {
		return
	}
	if hit {
		p.ttsCacheHits.Add(l, 1)
	} else {
		p.ttsCacheMisses.Add(l, 1)
	}
	p.ttsDuration.Observe(l, d)
}

// PlaybackUnderrun books one started playback that ended in a sink failure
// (docs/spec/13 "playback underruns", ADR-0074's definition: only playbacks
// that reached the device).
func (p *Product) PlaybackUnderrun(l Labels) {
	if p == nil {
		return
	}
	p.playbackUnderruns.Add(l, 1)
}

// PrewarmHit books one commit resolved from audio this session prewarmed.
func (p *Product) PrewarmHit(l Labels) {
	if p == nil {
		return
	}
	p.prewarmHits.Add(l, 1)
}

// FrameToVoice books one frame→voice sample: from the frame entering the
// pipeline to the committed line's audio starting.
func (p *Product) FrameToVoice(l Labels, d time.Duration) {
	if p == nil {
		return
	}
	p.frameToVoice.Observe(l, d)
}

// SetJobQueueDepth stores the durable queue's depth (queued + running).
func (p *Product) SetJobQueueDepth(n int64) {
	if p == nil {
		return
	}
	p.jobQueueDepth.Set(n)
}

// SessionStarted moves the active-session gauge up.
func (p *Product) SessionStarted() {
	if p == nil {
		return
	}
	p.activeSessions.Add(1)
}

// SessionStopped moves the active-session gauge down.
func (p *Product) SessionStopped() {
	if p == nil {
		return
	}
	p.activeSessions.Add(-1)
}
