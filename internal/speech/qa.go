package speech

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// ── ASR QA (docs/spec/07 "ASR loop", FR-10, ADR 0061) ──────────────────────
//
// The QA loop reads played audio back through the ASR provider and compares
// the transcript to the expected text on NORMALIZED text: case, punctuation,
// whitespace and a small British/American spelling map, because the endpoint
// normalizes spelling (verified 2026-10-18: "harbour" -> "harbor"). Metrics
// are WER (word error rate) and CER (character error rate) on the normalized
// forms. The bound is advisory and language-dependent per the spec; the
// default and the override live here so every QA site (audition approval,
// cache insert, doctor, live gate) reads one number.

// DefaultWERBound is the advisory WER bound a transcript must not exceed to
// pass (ADR 0061): 0.20 on normalized text. A TTS+ASR round trip over a
// single synthetic line measures well under half of that; a transcript that
// misses a fifth of the words is a clip to review, not a clip to approve.
const DefaultWERBound = 0.20

// envWERBound overrides the bound (a Go float, 0 < v < 1).
const envWERBound = "GAME_NARRATOR_ASR_WER_BOUND"

// WERBound is the bound in effect: the env override when it parses to a
// positive fraction below 1, else DefaultWERBound. A bound >= 1 would pass
// everything and 0 would fail everything; both are configuration faults the
// operator sees in the value the QA engine logs, not a silently useless gate.
func WERBound() float64 {
	raw := strings.TrimSpace(os.Getenv(envWERBound))
	if raw == "" {
		return DefaultWERBound
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 || v >= 1 {
		return DefaultWERBound
	}
	return v
}

// Transcription is one read-back of an utterance: the recognised text plus
// the correlation the evidence bundle carries on every clip (AGENTS.md
// rule 3). *asr.Client supplies it; tests use a scripted fake.
type Transcription struct {
	Text            string
	Model           string
	RequestID       string
	RequestIDSource string
	Latency         time.Duration
}

// ASRClient is the provider seam the QA loop reads audio back through. It is
// declared here (below the provider layer) so the QA policy owns the seam and
// the adapters satisfy it; *asr.Client satisfies it through TranscribeText.
type ASRClient interface {
	// TranscribeText transcribes wav (a RIFF/WAVE utterance) and returns the
	// transcript record. language is the hint (e.g. "en"); empty lets the
	// model detect.
	TranscribeText(ctx context.Context, wav []byte, language string) (Transcription, error)
}

// QARecord is the evidence of one ASR verdict against one expected text.
type QARecord struct {
	// WER and CER are measured on normalized text (NormalizeText); 0..1+
	// (a pure insertion can push CER over 1.0 — a ratio, not a percentage).
	WER float64
	CER float64
	// Transcript is the recognised text verbatim from the endpoint.
	Transcript string
	// Model is the ASR model id ("" when the check did not run).
	Model string
	// RequestID / RequestIDSource correlate the check with provider-side
	// logs ("" when the check did not run).
	RequestID       string
	RequestIDSource string
	// CheckedAt stamps the verdict (zero time when not run).
	CheckedAt time.Time
	// Pass is the verdict against WERBound() at check time.
	Pass bool
}

// Evaluate checks a transcript against an expected text (docs/spec/07
// "ASR loop"): normalize both sides, measure WER/CER, and decide against the
// bound in effect. It never errors — the ASR error is the caller's to
// classify; Evaluate only judges text it is given.
func Evaluate(expected, transcript string) QARecord {
	rec := QARecord{
		WER:        wer(normalizeText(expected), normalizeText(transcript)),
		CER:        cer(normalizeText(expected), normalizeText(transcript)),
		Transcript: transcript,
		CheckedAt:  time.Now().UTC(),
	}
	rec.Pass = rec.WER <= WERBound()
	return rec
}

// MarkQAVerdict persists the verdict and its evidence for a cache key (the
// store-backed index carries the columns, migration 0012). Unknown keys are
// an error, exactly like MarkQA: a verdict for a GC'd entry is a wiring bug.
func (c *Cache) MarkQAVerdict(key CacheKey, qa QAState, rec QARecord) error {
	if err := c.ensureLoaded(context.Background()); err != nil {
		return err
	}
	e, ok := c.lookup(key)
	if !ok {
		return fmt.Errorf("speech: MarkQAVerdict: key unknown")
	}
	e.QA = qa
	e.QAWER = rec.WER
	e.QACER = rec.CER
	e.QATranscript = rec.Transcript
	e.QAModel = rec.Model
	if !rec.CheckedAt.IsZero() {
		e.QACheckedAt = rec.CheckedAt.Format(time.RFC3339Nano)
	}
	return c.Index.Put(key, e)
}

// ── the QA-on-insert policy ────────────────────────────────────────────────

// QAEngine runs the ASR check over freshly generated utterances
// (docs/spec/07 "ASR loop", ADR 0061):
//
//   - QA runs on cache INSERT (a freshly committed line), never over a cache
//     hit — the verdict persists on the entry, and re-checking bytes that
//     already carry a verdict would spend ASR budget proving nothing.
//   - The transcription is retried once, and only on the retryable kinds
//     (docs/spec/13 "bounded autonomous troubleshooting": retry only
//     idempotent operations with max attempts); the transcription is a pure
//     function of the bytes, so the retry is safe.
//   - A transcript at or under the bound marks the entry QAPassed.
//   - A transcript over the bound regenerates ONCE (bounded attempts; the
//     current Base wire carries no seed — ADR 0057 — so the retry is the
//     same key, and a seed-bearing retry under a new key is the policy the
//     moment the checkpoint exposes seed), re-checked the same way. The
//     second verdict is the entry's: pass marks it QAPassed, fail marks it
//     QAFailed WITH the WER and transcript, and the line still plays
//     (override) — playback is never blocked by QA (docs/spec/13 "ASR
//     down"; AGENTS.md "graceful degraded modes").
//   - When the ASR endpoint is down (every attempt failed with a provider
//     error) or not configured, the entry keeps QAUnknown — QA "not run" —
//     and the line plays. The check is evidence, not a gate on audio.
type QAEngine struct {
	// Cache is the TTS cache the verdicts are recorded on.
	Cache *Cache
	// ASR is the transcriber; nil means QA never runs (degraded mode).
	ASR ASRClient
	// Log receives the diagnostic evidence the spec asks to retain
	// (docs/spec/12 "retain diagnostic evidence"): the first-attempt WER
	// when a regeneration runs, the ASR error when the check could not run.
	// Nil disables it.
	Log func(format string, args ...any)
}

// NewQAEngine wires one engine. ASR nil is the documented degraded mode
// (the role not configured); Cache nil is a wiring error the constructor
// refuses, the way every other collaborator is refused.
func NewQAEngine(cache *Cache, asr ASRClient) *QAEngine {
	if cache == nil {
		panic("speech: NewQAEngine requires a non-nil Cache")
	}
	return &QAEngine{Cache: cache, ASR: asr}
}

// QAInsert checks one freshly generated result (FromCache must be false)
// against the expected text and applies the policy above. It returns the
// result that should play (the original, or the regenerated bytes) and the
// final QARecord (Pass false and zero WER when the check did not run — the
// caller then logs QA "not run" instead of a number).
func (e *QAEngine) QAInsert(ctx context.Context, key CacheKey, expected string, res Result, gen GenerationFunc) (Result, QARecord) {
	if e == nil || e.ASR == nil || res.FromCache {
		return res, QARecord{}
	}
	rec, ok := e.checkOnce(ctx, key, expected, res)
	if !ok {
		// The check could not run at all (ASR down): the entry stays
		// QAUnknown and the line plays — never blocked (docs/spec/13).
		return res, QARecord{}
	}
	if rec.Pass {
		_ = e.Cache.MarkQAVerdict(key, QAPassed, rec)
		return res, rec
	}

	// Over the bound: regenerate once (bounded attempts) and re-check.
	e.logf("qa: %s exceeded the bound on the first attempt (wer=%.3f, bound=%.2f); regenerating once",
		key[:12], rec.WER, WERBound())
	retry, gerr := gen(ctx)
	if gerr != nil {
		e.logf("qa: regeneration for %s failed (%v); the first verdict stands", key[:12], gerr)
		_ = e.Cache.MarkQAVerdict(key, QAFailed, rec)
		return res, rec
	}
	replaced, rerr := e.Cache.ReplaceAudio(ctx, key, retry.WAV)
	if rerr != nil {
		e.logf("qa: committing the regenerated audio for %s failed (%v); the first verdict stands", key[:12], rerr)
		_ = e.Cache.MarkQAVerdict(key, QAFailed, rec)
		return res, rec
	}
	final, ok := e.checkOnce(ctx, key, expected, replaced)
	if !ok {
		// Regenerated but unverifiable: the ASR verdict cannot be claimed.
		// The entry keeps the first (failing) verdict — the evidence the
		// spec asks to retain — and the new bytes play under it.
		e.logf("qa: the regenerated audio for %s could not be re-checked (ASR unavailable); keeping the first verdict", key[:12])
		_ = e.Cache.MarkQAVerdict(key, QAFailed, rec)
		return replaced, rec
	}
	state := QAPassed
	if !final.Pass {
		state = QAFailed
	}
	_ = e.Cache.MarkQAVerdict(key, state, final)
	return replaced, final
}

// checkOnce transcribes wav (retrying once on the retryable kinds), evaluates
// the transcript against expected, and records the verdict. ok is false when
// the check could not run at all (the ASR endpoint answered no usable text);
// the caller then leaves the entry QAUnknown.
func (e *QAEngine) checkOnce(ctx context.Context, key CacheKey, expected string, res Result) (QARecord, bool) {
	tr, err := e.transcribe(ctx, res.WAV)
	if err != nil {
		e.logf("qa: %s could not be checked (%v)", key[:12], err)
		return QARecord{}, false
	}
	rec := Evaluate(expected, tr.Text)
	rec.Model = tr.Model
	rec.RequestID = tr.RequestID
	rec.RequestIDSource = tr.RequestIDSource
	return rec, true
}

// transcribe is the bounded retry (max two attempts, retryable kinds only).
func (e *QAEngine) transcribe(ctx context.Context, wav []byte) (Transcription, error) {
	var last error
	for attempt := 1; attempt <= 2; attempt++ {
		if ctx.Err() != nil {
			return Transcription{}, ctx.Err()
		}
		tr, err := e.ASR.TranscribeText(ctx, wav, "")
		if err == nil {
			return tr, nil
		}
		last = err
		var pe *providers.Error
		if !errors.As(err, &pe) || !pe.Retryable {
			return Transcription{}, err
		}
		e.logf("qa: transcription attempt %d failed retryably (%s); one attempt left", attempt, pe.Kind)
	}
	return Transcription{}, last
}

func (e *QAEngine) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log(format, args...)
	}
}
