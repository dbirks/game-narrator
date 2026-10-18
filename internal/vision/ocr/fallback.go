package ocr

import (
	"context"
	"image"
	"strings"
	"sync"
	"time"
)

// FallbackPolicy is the docs/spec/05 fallback rule: "compare lightweight OCR
// against an optional VLM fallback for difficult fonts; include both and the
// discrepancy". FallbackEngine implements it: the primary (Tesseract) always
// runs first and for free, and the secondary (a VLM) runs only when the
// primary read is unresolved.
//
// The policy is per session because its two stateful parts — the rate limiter
// and the crop cache — belong to one live run: a rate budget that survived a
// session restart would throttle a fresh session, and a cache that did would
// answer a new page with a stale reading. FallbackEngine owns both; a Runner
// is single-use, so one engine is one session.
type FallbackPolicy struct {
	// ConfidenceThreshold: a primary read below it is unresolved. Default
	// 0.6, the same bar docs/spec/05's stability discussion uses for trusting
	// a local read at all.
	ConfidenceThreshold float64
	// MaxSide is the long-side bound (pixels) of the prepared crop the cache
	// hashes. It must equal the secondary's own bound (VLMEngine.MaxSide) so
	// the cached key is the hash of the very bytes that will be sent.
	// Default 1024.
	MaxSide int
	// RatePerSecond and RateBurst bound secondary calls: at most
	// RatePerSecond per second, burst capacity RateBurst. Default 1/s, burst 3:
	// a stable box costs one call (the cache), a flipping box one per second,
	// and a page flip may spend three tokens before the refill catches up.
	RatePerSecond float64
	RateBurst     int
	// DifficultFont is the profile flag: when true the secondary runs on every
	// frame of this region, resolved or not, because the font is known to
	// defeat the local engine. The rate limit and cache still apply, so a
	// stable box still costs one call.
	DifficultFont bool
	// MaxCache entries retained; the oldest is evicted on overflow. Default 64.
	MaxCache int
	// Now is the clock the rate limiter refills against. nil uses time.Now;
	// tests inject one so a limiter is deterministic.
	Now func() time.Time
}

// FallbackOutcome is the audit row docs/spec/05 requires the fallback to
// produce: "include both and the discrepancy". FallbackEngine attaches it to
// the Result it returns; the pipeline copies it into the frame's trace row
// (the developer UI's ocr.fallback field) and the evidence bundle keeps it
// with the timeline. A Result without a Fallback row is a single-engine read.
type FallbackOutcome struct {
	// PrimaryEngine / PrimaryText / PrimaryConf are the local read, raw and
	// verbatim; empty text and 0 conf when the primary errored.
	PrimaryEngine string
	PrimaryText   string
	PrimaryConf   float64
	// SecondaryEngine / SecondaryText are the VLM read; SecondaryText is
	// empty when the secondary was not called this frame (rate-limited or
	// failed) — SecondaryReason then says why.
	SecondaryEngine string
	SecondaryText   string
	// SecondaryReason is non-empty only when the secondary was skipped
	// (rate limit) or failed: the operator-facing cause.
	SecondaryReason string
	// Discrepancy is the normalised edit distance between the two readings
	// after NormaliseForCompare, in [0,1]; 0 when either reading is empty
	// (nothing to compare). It never measures the raw texts: raw punctuation
	// and case would make two identical readings look 10% apart.
	Discrepancy float64
	// ChosenConf is the confidence the chosen reading leaves with, after the
	// documented heuristic (see decide). 0 means "keep the reading's own".
	ChosenConf float64
	// Chosen is "primary" or "secondary": the read that left the engine.
	Chosen string
	// Reason names the decision in one phrase the UI can show as-is:
	// "primary_errored", "secondary_agreement", "primary_below_threshold",
	// "primary_confident", "secondary_empty", "secondary_failed",
	// "secondary_rate_limited", "secondary_cached".
	Reason string
}

// FallbackEngine implements Engine as primary-then-fallback. It is stateful
// per session (rate limiter + crop cache, see FallbackPolicy) and safe for
// concurrent Recognize calls.
type FallbackEngine struct {
	Primary   Engine
	Secondary Engine
	Policy    FallbackPolicy

	mu       sync.Mutex
	lim      tokenBucket
	cache    map[string]cacheEntry
	cacheGen []string // insertion order, oldest first, for eviction
}

const (
	defaultConfThreshold = 0.6
	defaultMaxSide       = 1024
	defaultRatePerSecond = 1.0
	defaultRateBurst     = 3
	defaultMaxCache      = 64

	// corroborationConf is the confidence a primary read earns when the VLM
	// reads the same text: two independent readers agreeing is the strongest
	// signal this pipeline has, short of certainty — which no engine in this
	// product may claim (docs/spec/14, ADR-0039 §3).
	corroborationConf = 0.9
)

// Name implements Namer. The label is stable (not user- or model-derived) so
// the provenance field on an observation names the policy.
func (f *FallbackEngine) Name() string { return "ocr-fallback" }

// Recognize implements Engine. The common path — a resolved primary — runs
// exactly what the bare primary would run: no crop, no hash, no network.
// Everything below only happens for an unresolved read. With the single
// input, both readers see the same image; the pipeline's RawCropRecognizer
// path is the one that splits them (ADR-0040, the 2026-10-18 amendment).
func (f *FallbackEngine) Recognize(ctx context.Context, img image.Image) (Result, error) {
	return f.recognize(ctx, img, img)
}

// RecognizeWithRawCrop implements RawCropRecognizer (ADR-0040, the 2026-10-18 amendment): the
// primary runs on the preprocessed input — the recipe the local engine was
// tuned on — and the secondary runs on the raw crop, the actual game
// pixels. The prepared-crop bytes that are hashed and sent are the RAW
// crop's: the cache key stays "the SHA-256 of the bytes that leave the
// appliance", and the privacy story is unchanged (crop only, downscaled,
// no full frame).
func (f *FallbackEngine) RecognizeWithRawCrop(ctx context.Context, preprocessed, rawCrop image.Image) (Result, error) {
	if rawCrop == nil || rawCrop.Bounds().Empty() {
		return f.recognize(ctx, preprocessed, preprocessed)
	}
	return f.recognize(ctx, preprocessed, rawCrop)
}

func (f *FallbackEngine) recognize(ctx context.Context, pre, raw image.Image) (Result, error) {
	p := f.normalizedPolicy()
	primary, perr := f.Primary.Recognize(ctx, pre)
	if perr == nil && !p.DifficultFont && isResolved(primary, p.ConfidenceThreshold) {
		return primary, nil
	}

	png, cerr := encodeCrop(raw, p.MaxSide)
	if cerr != nil {
		if perr != nil {
			return Result{}, perr
		}
		return primary, nil // a crop that cannot be encoded is not a failed read
	}
	hash := cropHash(png)

	f.mu.Lock()
	if entry, ok := f.cache[hash]; ok {
		f.mu.Unlock()
		out := f.decide(primary, perr, entry.sec, nil, p.ConfidenceThreshold)
		out.Reason = "secondary_cached"
		return applyOutcome(primary, perr, entry.sec, out)
	}
	allowed := f.lim.allow(f.clock(), p.RatePerSecond, p.RateBurst)
	f.mu.Unlock()

	if !allowed {
		out := &FallbackOutcome{
			PrimaryEngine:   engineNameOf(primary, "primary"),
			PrimaryText:     primary.Text,
			PrimaryConf:     primary.MeanConf,
			SecondaryEngine: engineNameOf(f.Secondary, "secondary"),
			Chosen:          "primary",
			Reason:          "secondary_rate_limited",
		}
		if perr != nil {
			// The frame is unread and the one reader who could save it is
			// budgeted out: surface the primary's error so the pipeline's
			// retry and stale machinery owns the frame, not a silent zero.
			return Result{}, perr
		}
		return applyOutcome(primary, nil, Result{}, out)
	}

	sec, serr := f.callSecondary(ctx, raw, png)
	out := f.decide(primary, perr, sec, serr, p.ConfidenceThreshold)
	if serr == nil {
		// Only successful reads are cached: a gateway blip must not poison a
		// stable textbox for the rest of the session with an empty answer.
		f.mu.Lock()
		f.cachePut(hash, cacheEntry{sec: sec}, p.MaxCache)
		f.mu.Unlock()
	}
	return applyOutcome(primary, perr, sec, out)
}

// callSecondary runs the secondary, preferring the prepared-crop seam so the
// request's bytes are exactly the ones the cache hashed.
func (f *FallbackEngine) callSecondary(ctx context.Context, img image.Image, png []byte) (Result, error) {
	if pr, ok := f.Secondary.(PreparedRecognizer); ok {
		return pr.RecognizePrepared(ctx, png)
	}
	return f.Secondary.Recognize(ctx, img)
}

// decide is the documented choice rule (ADR-0039 §4). It is pure and total:
//
//   - both readings equal after normalisation → keep the primary's raw text
//     (it is local and free) at the corroboration confidence 0.9: two
//     independent readers agreeing, capped below 1.0.
//   - the primary errored and the VLM read is non-empty → take the VLM read at
//     the prior: the fallback exists precisely for frames the local engine
//     could not read, and there is no local reading to compare against
//     (discrepancy stays 0, which is correct: nothing was compared).
//   - the primary read is below the threshold and the VLM read is non-empty →
//     take the VLM read at the prior scaled down by the discrepancy: the
//     further the readers disagree, the less the VLM's lone word is worth.
//   - otherwise → keep the primary: a confident local read is not overturned
//     by a distant model's disagreement, and an empty VLM read corroborates
//     nothing (the primary may itself be empty: that is the "empty box" case
//     and the reading stays empty, which the caller's unresolved test sees).
//
// A failed secondary never decides: the primary's read (or error) stands and
// the row says why, so the developer UI can show "VLM unavailable" instead of
// pretending the frame was read twice.
func (f *FallbackEngine) decide(primary Result, perr error, sec Result, serr error, threshold float64) *FallbackOutcome {
	primaryNorm := NormaliseForCompare(primary.Text)
	secondaryNorm := NormaliseForCompare(sec.Text)
	secondaryEmpty := strings.TrimSpace(sec.Text) == ""
	disc := 0.0
	if primaryNorm != "" && secondaryNorm != "" {
		disc = normalizedDistance(primaryNorm, secondaryNorm)
	}
	out := &FallbackOutcome{
		PrimaryEngine:   engineNameOf(primary, "primary"),
		PrimaryText:     primary.Text,
		PrimaryConf:     primary.MeanConf,
		SecondaryEngine: engineNameOf(f.Secondary, "secondary"),
		SecondaryText:   sec.Text,
		Discrepancy:     disc,
		Chosen:          "primary",
	}
	switch {
	case serr != nil:
		out.Reason = "secondary_failed"
		out.SecondaryReason = serr.Error()
	case primaryNorm != "" && primaryNorm == secondaryNorm:
		out.Reason = "secondary_agreement"
		out.ChosenConf = corroborationConf
	case perr != nil && !secondaryEmpty:
		out.Chosen = "secondary"
		out.Reason = "primary_errored"
		out.ChosenConf = vlmConfidencePrior
	case !secondaryEmpty && primary.MeanConf < threshold:
		out.Chosen = "secondary"
		out.Reason = "primary_below_threshold"
		out.ChosenConf = vlmConfidencePrior * (1 - disc)
	default:
		if secondaryEmpty {
			out.Reason = "secondary_empty"
		} else {
			out.Reason = "primary_confident"
		}
	}
	return out
}

// isResolved is the unresolved test docs/spec/05's fallback keys on: an
// errored read is never resolved; an empty read on a presence-gated frame is
// either an empty box or a failed read and the VLM gets to look; a
// low-confidence read is what the fallback exists for.
func isResolved(r Result, threshold float64) bool {
	if strings.TrimSpace(r.Text) == "" {
		return false
	}
	return r.MeanConf >= threshold
}

// normalizedPolicy fills defaults and clamps: zero fields mean the documented
// defaults, and a burst below 1 is read as one.
func (f *FallbackEngine) normalizedPolicy() FallbackPolicy {
	p := f.Policy
	if p.ConfidenceThreshold <= 0 {
		p.ConfidenceThreshold = defaultConfThreshold
	}
	if p.ConfidenceThreshold > 1 {
		p.ConfidenceThreshold = 1
	}
	if p.MaxSide <= 0 {
		p.MaxSide = defaultMaxSide
	}
	if p.RatePerSecond <= 0 {
		p.RatePerSecond = defaultRatePerSecond
	}
	if p.RateBurst < 1 {
		p.RateBurst = defaultRateBurst
	}
	if p.MaxCache < 1 {
		p.MaxCache = defaultMaxCache
	}
	return p
}

func (f *FallbackEngine) clock() time.Time {
	if f.Policy.Now != nil {
		return f.Policy.Now()
	}
	return time.Now()
}

// cacheEntry is one remembered secondary call: the read itself (text and
// confidence). The decision is recomputed per frame against the *current*
// primary — the cache saves the remote call, never the decision — so nothing
// else about the frame that paid for it is retained.
type cacheEntry struct {
	sec Result
}

// cachePut stores one entry; overflow evicts the oldest key. The map is
// created on first use rather than in a constructor: FallbackEngine is built
// as a struct literal (the pipeline's wrap is the only production site) and
// must work without one.
func (f *FallbackEngine) cachePut(hash string, entry cacheEntry, maxCache int) {
	if f.cache == nil {
		f.cache = make(map[string]cacheEntry)
	}
	if _, ok := f.cache[hash]; !ok {
		f.cacheGen = append(f.cacheGen, hash)
	}
	f.cache[hash] = entry
	if len(f.cacheGen) > maxCache {
		old := f.cacheGen[0]
		f.cacheGen = f.cacheGen[1:]
		delete(f.cache, old)
	}
}

// tokenBucket is the per-session rate limit: at most rate tokens per second,
// burst capacity. allow refills against the session clock and takes a token if
// one is due. It is deliberately not a blocking limiter: a frame that cannot
// spend a token is skipped, not queued, because the next frame of the same box
// hits the cache anyway and a queue would only delay the reducer.
type tokenBucket struct {
	tokens float64
	last   time.Time
	seen   bool
}

func (b *tokenBucket) allow(now time.Time, rate float64, burst int) bool {
	if !b.seen {
		b.seen = true
		b.last = now
		b.tokens = float64(burst)
	} else {
		b.tokens += now.Sub(b.last).Seconds() * rate
		b.last = now
	}
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// applyOutcome builds the engine's final Result from the decision: the chosen
// read with its chosen confidence and the audit row attached. It never turns a
// chosen-but-empty read into an error: emptiness is a reading, and the
// caller's unresolved/stability machinery is what acts on it.
func applyOutcome(primary Result, perr error, sec Result, out *FallbackOutcome) (Result, error) {
	var res Result
	if out.Chosen == "secondary" {
		res = sec
	} else {
		res = primary
	}
	if out.ChosenConf > 0 {
		res.MeanConf = out.ChosenConf
		for i := range res.Words {
			res.Words[i].Conf = out.ChosenConf
		}
		for i := range res.Lines {
			res.Lines[i].Conf = out.ChosenConf
		}
	}
	res.Fallback = out
	if out.Chosen == "primary" && strings.TrimSpace(res.Text) == "" && perr != nil {
		// Nothing could be read at all: the primary failed and the secondary
		// did not rescue it (failed, empty, or budgeted out). Report the
		// primary's error so the pipeline retries or counts it — the frame is
		// unread, and that must be visible in Stats.OCRErrors.
		return Result{}, perr
	}
	return res, nil
}

// engineNameOf labels a read or an engine for the audit row: the producer's
// own name when it has one, a positional label otherwise, so the row never
// shows an empty producer.
func engineNameOf(v any, fallback string) string {
	if n, ok := v.(Namer); ok {
		if s := n.Name(); s != "" {
			return s
		}
	}
	if r, ok := v.(Result); ok && r.Engine != "" {
		return r.Engine
	}
	return fallback
}
