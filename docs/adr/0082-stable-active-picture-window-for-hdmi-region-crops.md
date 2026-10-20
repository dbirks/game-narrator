# ADR-0082: Stable active-picture window for HDMI region crops (vision.Tracker)

Date: 2026-10-19
Status: accepted
Owners: hdmi-characterize worker
Related requirements/tests: docs/spec/04 "Coordinates must adapt to 4:3 vs 16:9 stretch, letterboxing, overscan", docs/spec/05 "Timings", docs/spec/12 "repeated identical frames"; tests `TestTracker*`, `TestCropNormalizedWithTracker`, `TestTrackerNilKeepsStatelessBehaviour` (internal/vision/active_test.go)

## Context and constraints

The real GameCube HDMI feed was characterized end-to-end for the first time
(fixture `hdmi-v316b`, recorded on the UNO Q through the RYBOZEN V316B
345f:2131 card; frames stored outside the repo, manifest in the fixture dir).
Measured facts (200 frames per session, 3.33 fps, `scripts/hw/hdmi-characterize`):

- 1080p canvas: the active picture window is **x 72..1835, y 136..989**
  (1764×854) and **constant across every one of the 200 frames**. 720p canvas:
  x 48..1223, y 92..660 (1176×569), likewise constant.
- The window is **2.07:1, not 4:3**: the converter stretches the GameCube's
  4:3 output ~1.55× horizontally into the canvas, and the borders are true
  black (luma < 1). There is no inner pillarbox — the "halo" outside a
  hypothetical 4:3 box is real picture (mean luma ~110, texture std in the
  same band as the picture).
- Luma is **full range** (0..255 present; 200/200 frames full-range) in both
  sessions; the card declares MJPG but the driver streams raw YUY2
  (yuyv422), so no yuvj range remap is in play.
- Sharpness is low and soft (Laplacian variance p50 17 at 1080p, 29 at 720p,
  vs 418–422 on the 640×480 Dolphin bench fixtures) — an upscaled 480i/p
  source with scanline/ringing, not a crisp framebuffer.
- Combing score is low (max 0.072) — no significant interlace in this window.

The perception pipeline maps a stored region (fractions of the *game
picture*) onto the captured frame. In production
(`internal/runtime/session.go`) the crop options are `vision.CropOptions{}`,
i.e. `Letterbox: LetterboxNone`: the rect is mapped onto the **whole canvas**.
On this feed that mis-places the Wind Waker textbox crop (region
x 0.05..0.85, y 0.62..0.87) by up to ~65 px horizontally and ~70 px at the
bottom edge at 1080p, and includes canvas black in the crop margins. The
existing stateless `LetterboxDetect` mode is not sufficient either: it scans
`ActiveArea` per crop and, on a fade or black frame, reports "no bars" and
maps the rect onto the whole canvas — the ROI jumps by the bar width for
exactly those frames, the region hash churns (OCR cache invalidated,
"unchanged frames" broken, docs/spec/12), and a black frame can be read as
text. The window itself, meanwhile, is stable for the life of a session on a
fixed converter: it only changes on a real event (converter re-centres,
cutscene letterbox appears or clears).

## Decision

Add a stateful, per-frame **active-picture tracker** to `internal/vision`
(`active.go`, `Tracker` / `NewTracker` / `TrackerConfig`) and a `Tracker`
field on `CropOptions`:

- `Tracker.Update(img)` runs once per frame on the frame-ordered stage. It
  runs the existing stateless `ActiveArea` scan and applies asymmetric
  hysteresis:
  - detection failed (black/fade/4:3/degenerate frame) → **nothing moves**;
    the accepted window (if any) is carried forward;
  - candidate within tolerance of the accepted window → **nothing moves**,
    and the accepted window is kept *exactly* (no snap), so bar-edge flicker
    cannot move the crop or change region hashes; tolerance defaults to
    `max(2, min(w,h)/250)` px (≈4 px at 1080p);
  - candidate outside tolerance → it must be observed **identical for 3
    consecutive frames** (the settle count, configurable, clamped to 2..60)
    before the crop moves; any other observation restarts the count.
- `Tracker.Box()` reports the accepted window; before the first window
  settles, callers map against the whole frame (the `LetterboxNone`
  behaviour), so a session starting on black invents no window.
- `CropNormalized` with `Letterbox: LetterboxDetect` and a non-nil
  `Tracker` reads `Box()` — it never mutates tracker state — so every region
  of a frame maps the same window, and the K-regions-per-frame case cannot
  advance the hysteresis counts faster than the frame rate.
- `Tracker.Reset()` returns to the fresh-start state (used when a session
  switches frame sources).
- A nil tracker keeps the exact historic stateless behaviour, and
  `LetterboxNone` is untouched: all existing callers and tests are
  unaffected.

## Alternatives considered

1. **Keep `LetterboxNone`, calibrate regions per capture mode.** Rejected:
   the window moves with every converter × card × mode combination (and with
   every resolution), so a region calibrated on one feed is wrong on the
   next, and fades still break the mapping.
2. **Stateless per-frame `ActiveArea` (today's `LetterboxDetect`).**
   Rejected: the measured feed proves the failure mode — a single fade frame
   moves the crop by ~70–180 px, and a one-frame decoder glitch could do the
   same. The region hash churn alone breaks the duplicate-OCR suppression the
   spec requires.
3. **Median/EMA smoothing of the per-frame box edges.** Rejected: smoothing
   still *moves* the window on bad frames (a median of 3 frames containing
   one black frame adopts the black frame's edges), and it lags real changes
   by the same amount the settle count would, without the hold-on-failure
   semantics a fade needs.
4. **Fixed per-profile window stored with the profile.** Rejected as the sole
   mechanism: it is a snapshot of one converter's output and silently wrong
   after any re-plug/re-negotiation; the tracker degrades to it only in the
   sense that a profile's regions are calibrated against captured frames.

## Consequences and risks

- The ROI is now stable under fades, black frames, one-frame glitches, and
  bar-edge flicker, and adopts real window changes within ~3 frames — late
  rather than early, which is the correct side of the error (a crop that
  moves on a glitch reads the glitch as dialogue).
- OCR cache and stability-window semantics are unchanged; on a genuine
  window change the region hashes churn for a few frames, which is desired
  (the picture layout actually changed).
- New per-frame cost: one `ActiveArea` scan per frame (already the cost of
  `LetterboxDetect`; stride-sampled, ~µs on these frames) plus bookkeeping.
- The runtime is not wired to the tracker yet (see Follow-ups): until then
  production behaviour is unchanged, which is exactly the backward-
  compatibility guarantee the nil-tracker tests pin.
- Failure mode: a tracker that settles on a bad window (e.g. the source
  switches to a genuinely different layout for exactly 3+ frames and then
  black) lags until the new layout re-settles; bounded by the settle count,
  and `Reset()` is the escape hatch.
- No schema, protocol or cache-key change.

## Evidence and test plan

- Synthetic-frame unit tests (no real frames in the repo, AGENTS.md rule 5):
  settle + hold across black/fade frames, hysteresis keeps the crop exactly
  stationary under 2 px edge jitter over 20 frames, one- and two-frame
  glitches rejected, settled change adopted on the 3rd identical frame and
  held, fresh start on black invents no window, Reset, and the
  `CropNormalized` integration (marker found via the held window; a black
  frame keeps the window-sized crop while the stateless path demonstrably
  jumps to the whole-frame mapping).
  Run: `go test -race -count=1 ./internal/vision/...`
- Real-feed measurement: `scripts/hdmi-characterize` over the
  `hdmi-v316b` fixture (external fixture dir, manifest with per-frame sha256)
  and the Dolphin bench fixtures in `artifacts/fixtures/windwaker-*`:
  numbers in docs/manuals/18 ("Real feed measurements").
- Determinism: the tracker is pure integer state with no maps or time; the
  vision package tests run with `-race` and are byte-stable.

## Migration / rollback

No migration: no persisted state, no schema change. Rollback: drop the
`Tracker` field usage (callers keep `CropOptions{Letterbox: LetterboxDetect}`
or `LetterboxNone` as before) and delete `active.go` + `active_test.go`; the
nil-tracker path is the historic code path verbatim.

## Follow-ups

- Wire the tracker into the production frame loop: the runtime session
  (`internal/runtime/session.go`, out of this worker's file scope) should own
  one `Tracker` per source, call `Update` on the frame stage, and pass it via
  `CropOptions` with `LetterboxDetect`. Until that lands, this ADR's
  behaviour is exercised only by the vision tests and the characterization
  tool.
- Developer inspector: show the tracker's accepted window next to the region
  overlay (docs/spec/09) so a mis-settled window is visible, not guessed at.
- Re-measure the window after any converter/card re-plug; the per-source
  values are recorded in docs/manuals/18, not hard-coded.

## Amendment 1 (2026-10-20): the tracker is wired into the session, the
one-shot reader, and the developer trace

The Follow-ups of this ADR — "wire the tracker into the production frame
loop" and "developer inspector: show the accepted window" — landed. This
amendment records what changed and what the wiring contract is now; the
decision itself (stateful hysteresis, settle count, nil-tracker
backward compatibility) is unchanged.

**Pipeline.** `Runner.roiLoop` (the frame-ordered stage, one goroutine)
feeds the tracker once per frame before any region crops the frame:
`CropOptions.Tracker.Update(f.Image)`. A nil tracker keeps the historic
stateless crop behaviour, so every caller that does not set one (the
scenario and bench harnesses, all existing tests) is untouched. The
tracker is a `vision.CropOptions` field the session sets —
`pipeline.Config.Tracker` remains the recognition tracker's config, so
there is no new pipeline knob and no `Config` field collision.

**Runtime.** `Manager.pipelineConfig` now gives every session run one
`vision.NewTracker(vision.TrackerConfig{})` with
`Crop: {Letterbox: LetterboxDetect, Tracker: <tracker>}`: one tracker per
session, fed by the ROI stage for the run's life, the tracker the session
switches nothing into. Borderless sources (Dolphin's framebuffer, the
4:3 synthetic corpus) never report bars, so the tracker never settles and
`CropNormalized` degrades to the whole-frame mapping — measured
consequence: the crop basis is the full frame and the crops are byte-
identical to the pre-amendment mapping, which is what keeps the scenario
and bench gates (recall ≥ the 97.9% baseline, top-1 100%) green without a
special case.

**One-shot.** `internal/oneshot` settles the window from the frame itself
with the new `Tracker.Settle(img)`: a single frame has no stream to
hysteresise over, so the settle count it would have to wait for can never
elapse, and the frame's own stateless `ActiveArea` scan is the whole
evidence there. `Settle` is the single-frame seam, not a bypass — the
frame loop keeps using `Update` (a test pins that a `Settle`-settled
tracker still refuses a one-frame change and adopts a real one only after
the settle count). A frame that reports no window (borderless, fade,
degenerate, or nil) settles nothing and every region — the profile's
stored regions and the no-game fallback alike — maps the whole frame, the
historic `LetterboxNone` behaviour. The one-shot reader's documented
contract ("cut the same box the session would cut") now holds for
bordered frames too.

**Developer trace.** The dev-trace recorder is handed the session's crop
options (`SetCropOptions`, once at session start, before any frame
flows): its stored ROI crops are the crops the pipeline reads, cut
through the same window basis. Each sample stamps the window it sampled —
`active_window` `{x, y, w, h}` (fractions of the frame) plus
`settled` — on the `text.observed` payload, and the developer inspector
shows the crop basis: the region overlay is drawn through the window (the
box the pipeline actually cut), a settled non-trivial window gets its own
outline, and the basis line says "full frame (window not settled)" when
nothing settled. A mis-settled window is visible on the page, not guessed
at (the inspector's half of the ADR's second follow-up).

**Concurrency.** `Tracker`'s state is now mutex-guarded: the capture
pump's `recordFrame` reads `Box` while the ROI stage runs `Update`, and
the pre-amendment "not safe for concurrent use" contract did not cover
the recorder. The mutex keeps cross-goroutine `Box` reads whole; the
hysteresis is still a pure sequence over `Update` calls on the frame
stage (the determinism claim of this ADR is untouched, and
`TestTrackerBoxIsSafeFromAnotherGoroutine` asserts it under `-race`).

**Evidence.** `TestBorderedHDMIReplayCommitsWithTrackedWindow` /
`TestBorderedHDMIReplayMissesWithoutTracker` (internal/pipeline): a
synthetic bordered-HDMI replay (generated frames — the ADR's measured
720p window, a dialogue box and a sign at the profile's regions inside
the active picture) through the real pipeline; with the session's crop
options both lines commit, and the same clip with the historic crop
options commits nothing because every crop lands in the scene. Vision:
`TestTrackerSettle*` (single-frame settle, borderless/blacks/nil invent
nothing, settle-then-update keeps the window, cross-goroutine `Box`
under `-race`). Runtime: `TestDevTraceSample*` (the sample carries the
settled fractions and the payload enrichment; borderless and no-tracker
sessions name the whole frame). Web: `window.test.ts` (the overlay math)
and the developer e2e step asserting the payload field and the basis line
on the real page. Run: `go test -race -count=1 ./internal/vision/...
./internal/pipeline/... ./internal/oneshot/... ./internal/runtime/...`,
`make test-replay`, and the bench holdout (unchanged baseline).
