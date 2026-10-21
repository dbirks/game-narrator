# ADR-0152: advance-prompt reveal-complete signal for the typewriter settle

Status: accepted. Scope: `internal/games/adapter`, `internal/games/adapter/windwaker`,
`internal/recognition`, `internal/session`, `internal/pipeline`, `internal/vision`,
`internal/runtime`, `cmd/game-narrator`.

## Context

docs/spec/05's dialogue observation state machine declares a line stable when the
accepted text has not changed for `StableWindow` (REVEALING→STABILIZING) and then
again for one more frame with sufficient confidence (STABILIZING→READY). That is
the safe reading for a game whose box gives no "finished typing" cue. Wind Waker
gives one: when the typewriter has finished a line, the game draws a teal-green
"advance prompt" swirl centred horizontally just below the bottom dialogue box
(measured on 19 frames of a 640×480 Dolphin capture: a ≈28 px disc whose centre
sits ≈14 px below the region rect's bottom edge, pulsing between saturated teal
and pale white-green; the matching pixel count is 502–544 with the prompt present
and 0 on every frame without it).

Two failure modes of the stability-only settle this signal closes:

1. **Settle latency.** A full line still pays the whole `StableWindow` plus the
   READY hold frame after the last glyph lands. The game's own marker says the
   reveal is over; the state machine waits out a timer it does not need.
2. **Unmatched prefix commits.** An OCR read that no corpus message matches
   (ABSTAINED) may commit once it *looks* stable — including a box that is still
   mid-reveal and whose remaining glyphs would have changed the read. The marker
   distinguishes "the game considers this line finished" from "the text happened
   to stop changing".

## Measurement (orchestrator, 2026-10-21)

Source: `artifacts/av-harness/av-20261021T202536Z/media/session.mp4` (640×480),
19 frames. The detector is a pixel count over a fixed ROI under the box:

- ROI: `x ∈ [cx−40, cx+40)`, `y ∈ [bottom−20, bottom+60)`, where `cx` is the
  region's horizontal centre in pixels and `bottom` the region rect's bottom edge
  in pixels, clamped to the frame.
- Pixel rule (RGB normalised 0..1): `g > 0.40 && g−r > 0.12 && g >= b−0.05 && b > 0.35`. The blue floor was added after the island scene: without it, grass and foliage under a middle-height box put 1386–3050 pixels in the ROI (a false "reveal complete"); with it the scene scores 0 and the prompt 493–523.
- Score = count of matching pixels. Present → 493–544; absent → 0 on all 19
  frames. Threshold: present when `score >= 120`, scaled by `(frameW/640)²` for
  other frame widths.

The detector is game knowledge. It lives in the Wind Waker adapter, not in
recognition: docs/spec/04 forbids Zelda assumptions in the core runtime, and the
recognition package's own charter says there must never be game-specific logic
there.

## Options

1. **Game-specific settle branch in the pipeline.** The pipeline special-cases
   Wind Waker frames. Rejected: docs/spec/04 condition in core code; every new
   game with a marker would add another branch.
2. **Optional adapter capability + generic signal on the observation.** The
   adapter exposes the detector; the pipeline evaluates it where the crop is
   taken (the only stage that holds the frame image) and carries the boolean on
   `recognition.Observation`; the state machine and the commitment policy act on
   the boolean alone. Chosen: it is the same capability-assertion pattern the
   adapter package already uses for `DiscExtractor`, `MarkupDecoder` and
   `EditionIdentifier`, and it keeps recognition game-free — the signal is a
   documented Observation field, not a game.
3. **Pass the frame image into the tracker.** Rejected: the tracker's purity
   contract (immutable observation, no I/O, deterministic replay) exists so the
   scripted timelines in the tests are the live runtime's code path; a pixel
   image in `Observation` would drag a copy of the frame into every reducer
   input and make byte-identical replay depend on frame bytes.

## Decisions (as implemented)

1. **Capability.** `internal/games/adapter` gains the optional `RevealCompleteDetector { RevealComplete(img image.Image, region image.Rectangle) bool }`. `img` is the full frame and `region` is the analysed band in frame pixels. The Wind Waker adapter implements it via `windwaker.AdvancePromptPresent` (the measurement above). No other adapter does. `pipeline.RevealCompleteDetector` is the same one-method interface, so the pipeline does not import the adapter package.
2. **Resolution.** `runtime.Options.Adapters` (the process's adapter registry; `cmd/game-narrator` and the e2e harnesses pass it) is used at session start. `Manager.revealDetector` takes the first source whose parser (`"<adapter id>@<version>"`, e.g. `windwaker-bmg@1.1.0` on the board's profile) resolves to an adapter offering the capability, and stores it as `pipeline.Config.RevealDetector`. With no registry, or no capable adapter, the field stays nil and behaviour is unchanged.
3. **Transport.** `Runner.analyze`, the stage that holds the frame and the band (`vision.CropRect`, the band actually cut, so a placement-aware region (ADR-0137) measures under its selected band), calls the detector. `recognition.Observation` carries `RevealDetector` (looked for) and `RevealComplete` (verdict).
4. **Immediate settle.** In `Tracker.progress`, when the marker is present and the text is unchanged since the previous read (`elapsed > 0`), the read counts as stable at once. The line goes REVEALING→STABILIZING on that frame and READY one frame later; the READY hold frame and its contradiction and confidence guards are kept. The window alone needs `StableWindow` (600 ms default) plus the hold frame.
5. **Mid-line pause.** The tracker remembers per region that it has seen the marker (`promptSeen`), which proves the detector works on this source's pixels. After that, a REVEALING line without the marker is not stable until the marker shows or `revealPromptFallback` (2.5 s) passes. This removes the A/V run's "That's. That's why Grandma…" stutter, where a dramatic pause read as the end of the line. On a source where the marker never registers (other colour handling, another game), the plain window is unchanged, so an unproven detector can never add latency.
6. **No per-profile switch yet.** The gate is on exactly when the adapter provides the detector. Disabling it means removing the registry entry or reverting this change. A per-region `ocr_settings` knob is left for when a game needs it.

## Tradeoffs

- The fast settle trusts the game's marker: a READY one frame after the marker shows is the game's own claim that the line is complete.
- A marker-less box on a proven source waits up to 2.5 s before it settles (a cut-scene box that hides the prompt). This bounds the cost of a hidden marker.
- The colour rule is measured on Dolphin frames. On the V316B capture card the prompt may render differently. Rule 5 is self-calibrating: if the board never registers the marker, nothing changes there.

## Validation

- `internal/games/adapter/windwaker/advance_prompt_test.go`: synthetic teal disc and pale disc → present; none, blue disc, grass-green disc, a patch above the box bottom, or a sub-threshold disc → absent; scaled frames and clamped ROIs.
- `internal/recognition/reveal_prompt_test.go`:
  - the marker settles a line within 2 frames, where the window needs 6;
  - a proven source holds a 1 s mid-line pause, then settles the full line with the marker;
  - an unproven source keeps the plain window;
  - a proven source still settles a marker-less box after the 2.5 s fallback.
  - All three tests fail with the tracker logic disabled.
- Packages: `internal/recognition`, `pipeline`, `games/...` and `session` pass under `-race`.
- Live: `make test-av` before/after (settled→audio and the stutter in the ASR of the run).

## Reversal plan

Remove `Options.Adapters` from the runtime construction (or the Wind Waker `RevealComplete` method). The pipeline then never sets `RevealDetector`, and the tracker branch is inert.
