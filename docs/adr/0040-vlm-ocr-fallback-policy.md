# ADR-0040: VLM OCR fallback policy, confidence heuristic and privacy boundary

Date: 2026-10-17
Status: accepted
Owners: david (m3-vlm-ocr-fallback)
Related requirements/tests: docs/spec/05 (OCR pipeline, "Compare lightweight OCR
against optional VLM fallback for difficult fonts; include both and the
discrepancy"; "Fast LLM/VLM called only for unresolved OCR"), docs/spec/14
(privacy: crop only, redact in traces), docs/status/blocked.md
(vision_ocr_fallback cleared 2026-10-17)

## Context and constraints

docs/spec/05 requires the recognition stage to compare the local OCR engine
(Tesseract, ADR-0012) against an optional vision-language model for difficult
fonts, to record **both** readings and their discrepancy, and to call the fast
model **only** for unresolved OCR. The configured gateway's `qwen3-8-27b`
accepts images (cleared 2026-10-17); JSON mode is **not** available on this
gateway, so the fallback asks for plain text and validates it. The model
exposes no token logprobs, so any confidence it is assigned is a documented
heuristic, not a measurement. The pipeline (`internal/pipeline`) is the only
component allowed to wrap the configured OCR engine, and `internal/runtime`
(which owns the provider clients) must be able to switch the fallback on later
without touching this decision.

## Decision

1. **Seam.** `ocr.VLMEngine` (internal/vision/ocr) implements `ocr.Engine`
   over a minimal `ocr.VLMClient` interface (`Chat(ctx, VLMRequest{Instruction,
   PNG, MaxTokens}) (string, error)`). The instruction is terse and fixed:
   "Transcribe the text in this game dialogue box exactly. Output only the
   text. If there is no readable text, output NONE." The answer is plain text;
   `NONE`/empty map to an empty read, and quotes/markdown wrappers are
   stripped. The production adapter over `openai.Client` is the **runtime's**
   job (it owns the role-configured clients; role `vision_ocr_fallback`
   carries the defaults: reasoning effort `none`, timeout 10 s, and the
   `MaxTokens` bound of 256).
2. **Policy.** `ocr.FallbackEngine{Primary, Secondary, Policy}` runs the
   primary first and for free. The secondary is called only when the primary
   is *unresolved*: an error, empty text on a presence-gated frame (docs/spec/
   05 names this case), mean confidence below `ConfidenceThreshold` (default
   0.6), or the profile's `DifficultFont` flag. Per session (one engine per
   Runner session) it carries a token bucket — 1 call/s, burst 3 by default,
   non-blocking: a frame that cannot spend a token keeps the local read (or
   surfaces the primary's error) and the next identical frame hits the cache —
   and a crop cache keyed on the SHA-256 of the prepared crop's PNG (≤1024 px
   long side, configurable; the policy and its VLM engine must share
   `MaxSide`). Only successful reads are cached.
3. **Confidence heuristic** (no logprobs available): a lone VLM read carries a
   documented prior of **0.6**, never 1.0. When both readings are on record:
   equal after normalisation → keep the primary's raw text at corroboration
   confidence **0.9** (two independent readers agreeing); primary below
   threshold and VLM non-empty → take the VLM read at the prior **scaled down
   by the discrepancy**; otherwise keep the primary. The chosen confidence is
   carried on the Result and the audit row.
4. **Discrepancy and audit.** `NormaliseForCompare` (case-folded,
   letters+digits only; no NFKC — this toolchain's stdlib has no
   `unicode/normalize`, a documented gap) feeds `normalizedDistance`, the
   normalised Levenshtein distance in [0,1] (0 when either side is empty:
   "no comparison possible", not "total disagreement"). Every fallback result
   carries `ocr.FallbackOutcome` — both raw readings, primary confidence,
   discrepancy, chosen read and a one-phrase reason — which the pipeline
   copies into its trace row as `ocr.fallback {primary_conf, discrepancy,
   chosen, reason}` and counts in `Stats.OCRFallbackFrames` /
   `OCRFallbackSecondaryCalls` (the latter excludes cache hits and
   rate-limited frames: it is the number that bills the gateway).
5. **Pipeline option.** `pipeline.Config.OCRFallback` (nil by default) wraps
   `Config.OCR` with the `FallbackEngine` in `normalize`, so `Runner.Config()`
   reports the engine that actually runs. The runtime sets it only when the
   `vision_ocr_fallback` role is configured; nil keeps today's single-engine
   behaviour, which is what the role's preflight "NOT CONFIGURED" continues
   to mean. The option validates a non-nil secondary.

## Alternatives considered

- **Always-on VLM comparison.** Rejected: docs/spec/05 limits the fast model
  to unresolved OCR; an always-on call would bill the gateway every frame of a
  stable box (the crop cache mitigates but not the first call per page) and
  would add a network dependency to the common path.
- **VLM as primary with Tesseract as fallback.** Rejected: the local engine is
  free, offline and deterministic (fixture-replay contract, docs/spec/12);
  inverting it would make every fixture replay network-dependent.
- **Confidence from VLM self-report ("I am 90% sure").** Rejected: unbounded
  by any measurement and easy for a model to overstate; the documented prior
  + agreement rule is reproducible and auditable.
- **Blocking rate limiter (queue the frame).** Rejected: the pipeline's
  backpressure is frame-oriented (a newer frame supersedes); a queue would
  only deliver the VLM's answer to a screen that has moved. Skipping and
  relying on the cache is the honest reading of "≤1 call/s".
- **Cache key on the full-resolution crop hash (vision.ImageHash).** Rejected:
  the pipeline already dedupes on that hash, so the cache would rarely see a
  repeat; keying on the *prepared* (downscaled) bytes means two near-identical
  frames that collapse to the same sent image are sent once — the key is the
  exact content that leaves the appliance (verified by the unit tests).

## Consequences and risks

- New failure modes, all counted and visible: gateway blips degrade to the
  local read with `reason=secondary_failed` (never cached); rate-limited
  frames keep the local read; a frame unread by both surfaces the primary's
  error so `Stats.OCRErrors` and the replay retry policy own it.
- The 0.6 prior and 0.9 corroboration are heuristics: a VLM read of a hard
  font is not "90% likely". They are bounded below 1.0 on purpose, and the
  evidence bundle keeps both raw readings so a bad choice is auditable.
- Privacy: the request carries the downscaled crop PNG only — no full frame,
  no scene context, no session state (docs/spec/14). The unit tests assert
  the sent image decodes to ≤ the configured long side and never equals the
  full frame; the cassettes (recorded provider) store image digests, not
  bytes.
- Cost: one gateway call per distinct unstable crop at ≤1/s per session;
  stable boxes cost one call each (cache).
- `MaxSide` must be shared between policy and engine by the caller (the
  runtime); a mismatch degrades the cache (misses), not privacy or
  correctness.

## Evidence and test plan

- Unit (fake provider, `internal/vision/ocr`): resolved → VLM never called;
  unresolved → exactly one call per crop hash across six identical frames;
  distinct crops → distinct calls; rate limit (1/s burst 3) with a pinned
  clock; `NONE` → empty read; quotes/markdown stripped; discrepancy computed
  and choice rule pinned per case; agreement → 0.9, disagreement scaled down;
  privacy: sent PNG decodes to ≤1024 long side, aspect preserved, never the
  full frame; MaxTokens bound passes through; failed secondary not cached.
- Pipeline (`internal/pipeline/fallback_test.go`): option off by default
  (no wrap, zero counters); option wraps and validates; resolved clip → VLM
  calls only on the corpus's distinct empty box crops; unresolved clip →
  counters agree with the engine's call log, trace rows carry
  `ocr.fallback`, and the session still commits both lines.
- Live (`internal/vision/ocr/vlm_live_test.go`, `-run Live`, sequential):
  magick-rendered textbox crop — serif italic font, dim on a dark box,
  downscaled, then heavy Gaussian noise and a soft blur, tuned so the local
  engine comes back garbled (reference run: tesseract CER 0.304, conf 0.46,
  "The hartonsr bell rang rewe") while the VLM resolves the line. The test
  (a) calls the VLM directly on the prepared crop and asserts its normalised
  CER < 10% against the known text (reference run: 0.000), (b) runs the
  policy with the `DifficultFont` profile flag — the documented knob for a
  font the local engine is known to defeat — so both readings and the
  discrepancy are recorded on every run (reference run: chosen=secondary,
  reason=primary_below_threshold, discrepancy=0.304). Skips NOT RUN without
  the gateway env, magick or tesseract. The model resolves role-model →
  `GAME_NARRATOR_LIVE_MODEL` → `qwen3-8-27b`, the same pattern the provider
  live tests use, because the role's *capability* is what the test probes —
  the role's *configuration* is what turns the app's fallback on (see
  blocked.md).
- The two provider vision live tests (`openai`, `recorded`) now run by
  default (the `GAME_NARRATOR_LIVE_VISION` opt-in is removed) and skip NOT RUN
  without the gateway env, as the other live tests do.

## Migration / rollback

No schema, protocol or data change. Revert: remove the
`pipeline.Config.OCRFallback` wiring (one normalize block) and delete
`internal/vision/ocr/{vlm,fallback,compare}.go`; the pipeline's OCR field and
the engines' `Engine` contract are unchanged, and `ocr.Result.Fallback` is a
nil-able field that no other producer sets.

## Follow-ups

- Runtime task: construct `ocr.VLMEngine` over the role-configured client
  (adapter applies the role's timeout, reasoning effort `none`, `MaxTokens`
  256) and set `Config.OCRFallback` when the role is configured. — **done
  2026-10-18, see the amendment below.**
- Profile task: surface the `DifficultFont` flag from `regions.ocr_settings`.
  — **done 2026-10-18, see the amendment below.**
- Developer UI task: render the `ocr.fallback` trace field (and the two
  `Stats` counters) in the inspector.
- Docs sync (outside this task's boundaries): `docs/dev-environment.md` and
  the Makefile comment for `test-models` still document the removed
  `GAME_NARRATOR_LIVE_VISION` opt-in; update when the runtime wiring lands.
- If a live profile shows the discrepancy measure is dominated by ligature /
  fullwidth glyph reads, vendor a small NFKC compatibility table (the
  documented gap in `NormaliseForCompare`).

## Amendment (2026-10-18): runtime wiring

Status: accepted. Owners: agent (runtime-wiring). The policy, confidence
heuristic and privacy boundary above are unchanged; this records how the
runtime turned the option on.

**Wiring.** `cmd/game-narrator/main.go` builds the fallback from the
`vision_ocr_fallback` role: the role's OpenAI-compatible client
(`providers.LoadRoles` + `openai.NewFromRole`, which applies the role's
timeout floor and reasoning-effort default) behind `ocr.OpenAIClient` (the
production adapter over the `ocr.VLMClient` seam, internal/vision/ocr) and
`ocr.NewVLMEngine`. `newOCRFallbackWiring` returns a nil fallback — not an
error — when the role is not configured: nil is the documented
"NOT CONFIGURED" degraded mode and sessions then run the single local
engine exactly as before. A half-configured fallback (option set, no
secondary) is refused at `runtime.New`, at construction rather than at the
first session start.

**Per-session wrap.** `runtime.Options.OCRFallback` carries the process
default; each session's `pipeline.Config` wraps its OCR engine in the
`ocr.FallbackEngine` with the secondary shared across sessions (stateless)
and a per-session policy (the rate limiter and crop cache are per-run
state, decision 2 above). The policy's `DifficultFont` is the documented
profile-level knob, merged in per session from `regions.ocr_settings`
(`difficultFontIn`): a region's `{"difficult_font": true}` runs the
secondary on every frame of every region of that session — the session's
OCR engine is one engine shared by all of the pipeline's regions, so the
session-level policy takes the conservative union, bounded by the rate
limit and the crop cache (one call per distinct crop, at most one per
second). Malformed `ocr_settings` reads as "no flag", never a session
failure. Unconfigured (nil option) keeps the single-engine behaviour and
the trace field off.

**The trace field in the session's event stream.** When the fallback is on,
the session enables the pipeline's bounded run timeline and the terminal
events (`session.stopped` / `session.failed`) carry it as `trace`: the
`ocr.fallback` field — both readings, primary confidence, discrepancy, the
chosen read and the reason — is the decision's home in the session's event
stream (docs/spec/05 "include both and the discrepancy"), with the
documentation's 512-row cap, in memory only (the event log stays the
durable record). When the fallback is off the trace stays off: the event
log remains the record and the timeline's cost is the cost of the feature,
not of every session.

**Verification.** Unit (fake `ocr.VLMClient` behind a real `ocr.VLMEngine`,
`internal/runtime/ocr_fallback_test.go`): an unresolved primary hands every
frame to the secondary; the trace rows carry the decision
(`chosen=secondary`, the garbled primary disagrees on every annotated
frame); the terminal event carries the `ocr.fallback` field; the seam's
privacy contract (crop PNG + fixed instruction + the 256 MaxTokens bound,
never raised); the unconfigured negative (no secondary call, no field, both
lines still commit). Wiring unit (`TestDifficultFontInRegions`): the flag
merges from any region and malformed settings degrade to off. Live
(`TestLiveOCRFallbackSession`, NOT RUN without the gateway env / magick /
tesseract): a replay session on a hard-font synthetic crop rendered with the
ocr live test's recipe (same font pick, noise and blur), real Tesseract as
primary and the role's real VLM as secondary, with the hard-font region's
`difficult_font` flag landed in the draft and the revision published (a
published revision is immutable — the flag lands in the draft, then the
revision is published, the same shape a profile-level flag edit takes in
production). It asserts the secondary was called, at least one
`ocr.fallback` row with `chosen=secondary` in the session's terminal
`trace`, the `ocr.fallback` field present, and the secondary's reading
within the documented CER (<0.10) against the known text.

## Amendment (2026-10-18): the secondary reads the raw crop

Status: accepted. Owners: agent (runtime-wiring). The policy, confidence
heuristic and privacy boundary are unchanged; this records one input
contract the original text left open: **which pixels the secondary sees**.

**The finding.** The pipeline prepares each region crop through the local
engine's recipe (`vision.Preprocess`: grey, auto-invert, 2× nearest-neighbour
upscale, Otsu threshold, median filter, pad) and hands the recipe's output to
the configured OCR engine. The fallback wrapped that engine, so the VLM
secondary received the recipe's output too. On the corpus's hard crop this
was wrong for a vision model: the recipe's output is a tesseract tuning
(greyed, inverted, blocky, thresholded), not a picture a VLM was trained
on. Live evidence on this host (qwen3-8-27b behind the shared gateway,
`TestLiveOCRFallbackSession`): the same rendered crop, sent as the recipe's
output, drew hallucinated dialogue three times in a row ("The first time I
ever saw you", "He has a bad heart", "I'm sorry, I can't help with
that."); sent as the raw crop — the actual pixels, downscaled to the 1024
bound — it read "The harbour bell rang twice." at CER 0.000. The ocr
package's standalone live test, which sends the raw crop, had always passed.

**The decision.** `ocr.FallbackEngine` implements the new
`ocr.RawCropRecognizer` interface alongside `ocr.Engine`:
`RecognizeWithRawCrop(ctx, preprocessed, rawCrop)` runs the primary on the
preprocessed input — the recipe the local engine was tuned on, unchanged —
and the secondary on the raw crop, the actual game pixels. The prepared-crop
bytes that are hashed and sent are the RAW crop's, so the cache key stays
exactly what the original text promised: the SHA-256 of the bytes that
leave the appliance, and "one call per stable textbox" is a statement about
the game's own pixels (a stable box's pixels are stable), not about the
recipe's output. The privacy boundary is unchanged: the request still
carries the downscaled crop only — no full frame, no other role's input.

**The dispatch.** `pipeline.Runner` now carries the raw crop on the OCR job
alongside the preprocessed input and, at the single dispatch point, gives
engines that implement `ocr.RawCropRecognizer` the pair; every other engine
receives exactly what it received before (the interface assertion is the
only coupling point — no concrete-type branch, matching the wrap's own
comment in `Config.normalize`). A nil or empty raw crop degrades to the
single-input behaviour. The primary's identity (the dedupe hash, the
presence gate, the retry policy) is still computed on the preprocessed
input: the thing the local engine re-reads is what its cache and staleness
math describe.

**Verification.** `TestFallbackRawCropPath` (ocr package): the primary sees
the recipe's image, the secondary sees the raw crop; a stable raw crop whose
preprocessed input drifts still costs exactly one remote call (cache keyed
on the prepared raw bytes); a changed raw crop costs a fresh one.
`TestFallbackRawCropEmptyRawDegrades`: nil/empty raw degrades to the
single-input behaviour. `TestOCRFallbackUnresolvedEngages` (pipeline):
the scripted VLM's truth is now keyed on the raw crop's hash — the
pipeline-level proof that the secondary's pixels are the raw ones.
`TestLiveOCRFallbackSession` (runtime, live): passes on the raw crop at
CER 0.000 where it failed on the recipe's output; the test also captures
the exact bytes each call sent (hash logged; all crops dumpable under
`GAME_NARRATOR_LIVE_DBG_DIR`) and runs controlled direct VLM calls on the
same bytes, so a future failure separates wiring from model sampling by
evidence instead of guesswork.

**Reversal.** Drop the `RawCropRecognizer` implementation off
`FallbackEngine` and the type assertion out of `Runner.ocrRecognize` and the
fallback returns to sending the recipe's output to the secondary; nothing
else in the pipeline or store is affected.
