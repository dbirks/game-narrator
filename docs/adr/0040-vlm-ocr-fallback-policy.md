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
  256) and set `Config.OCRFallback` when the role is configured.
- Profile task: surface the `DifficultFont` flag from `regions.ocr_settings`.
- Developer UI task: render the `ocr.fallback` trace field (and the two
  `Stats` counters) in the inspector.
- Docs sync (outside this task's boundaries): `docs/dev-environment.md` and
  the Makefile comment for `test-models` still document the removed
  `GAME_NARRATOR_LIVE_VISION` opt-in; update when the runtime wiring lands.
- If a live profile shows the discrepancy measure is dominated by ligature /
  fullwidth glyph reads, vendor a small NFKC compatibility table (the
  documented gap in `NormaliseForCompare`).
