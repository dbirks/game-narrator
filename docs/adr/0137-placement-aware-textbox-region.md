# ADR-0137: The textbox region is placement-aware — the pipeline crops the band where the box actually is

Date: 2026-10-21
Status: accepted
Related requirements/tests: the live Wind Waker capture (session 01M5E8CG, rev3) whose fixed dialogue band cut the top-placed box in half and produced one empty `text.observed` (conf 0, 1×1 window) that nothing was spoken, docs/spec/05 "ROI/OCR stage" (a dialogue box is a region whose *position* the profile cannot know in advance), docs/spec/05 "suppress duplicate OCR calls on unchanged region hash" (the cost discipline this ADR must not break), ADR-0115 (escalation / glyph-noise / perceptual dedupe — the downstream of the crop this ADR feeds), ADR-0119 (the measurement discipline: choose by evidence on the real frames, record the choice)
Links/references: `internal/pipeline/roi.go` (`analyze`), `internal/pipeline/dispatch.go` (`regionState.accept`), `internal/vision/preprocess.go` (`ToGray`, `DarkShare`), `internal/vision/roi.go` (`CropNormalized`, `MapRect`), the live profile's textbox rows (`{x:0.05,y:0.25,w:0.8,h:0.37}` "dialogue box high", `{x:0.05,y:0.62,w:0.8,h:0.25}` "dialogue box")
Supersedes / superseded by: the implicit "one fixed band per region" assumption in the ROI stage (the crop is now one of a small candidate set, selected per frame on a cheap text-presence signal)

## Context

A region is a rectangle in normalised frame coordinates; the pipeline crops that one rectangle and OCRs it. That holds for a game whose dialogue box sits in one place. Wind Waker does not: the box renders in the **top**, **middle** and **bottom** of the active picture depending on the scene (a bottom character portrait pushes the box up to the middle; some scenes put it at the top). The profile calibrates *one* band — the band the operator saw when they drew the box — and the pipeline crops that band forever.

Measured on the real capture (640×480 JPEGs, the active picture is the whole frame — the 4:3 capture carries no bars, so `ActiveArea` reports the full canvas and the profile rect is already in frame coordinates):

- **rev3 (the failing session, box at the top):** the profile band `{x:0.05,y:0.25,w:0.8,h:0.37}` (frame y≈120–298) OCRs only the **bottom half** of the on-screen line, plus glyph garbage: `need to do is open your heart to / …Yeah, you heard me right! M'`. The **top** band `{x:0.05,y:0.0,w:0.8,h:0.37}` (frame y≈0–178) OCRs the **full clean line**: `If you come across wild pigs, you've got to be careful not to scare them. What you need to do is open your heart to them. …Yeah, you heard me right!`. The fixed band literally cut the text in half — the root cause of the empty, unspoken read.
- **A cheap, no-OCR signal separates the bands.** One luma pass over each candidate band (the same class of walk as `DarkShare`, ~0.1 ms a band at 640×480) reports two shares: the **dark share** (panel presence, luma < 90) and the **text share** (bright strokes on a dark panel: a bright pixel, luma > 140, with a dark neighbour, luma < 80). On the rev3 top-box frames the scores are:

  | band | dark | text |
  |------|------|------|
  | top  | 0.85 | **0.036** |
  | mid  | 0.63 | 0.018 |
  | bot  | 0.36 | 0.010 |

  the box's band scores 2–3.6× the others on the text share. The dark share alone cannot discriminate (a dark scene puts a panel on every band), which is why the **text** share is the selector and the **dark** share stays the presence gate.
- **The candidate set is small and known.** The box's horizontal position and its width/height are calibrated by the profile and stable; only the vertical placement varies. The three canonical placements — top (`y=0`), the profile's own `y`, and bottom (`y=1−h`) — cover every placement measured, and they are exactly the two rows the live profile already stores (`y:0.25` high and `y:0.62` bottom) plus the top.

The constraint that shapes the design (docs/spec/05, AGENTS.md "keep the runtime predictable"): **do not OCR every band every frame.** OCR is the 600 ms cost (`frame_to_ocr` p50); three OCRs per frame per textbox region would triple the hot path. The selection must therefore run on the cheap luma pass, and **one** band — the selected one — is cropped and OCRed through the existing dedupe/settle/escalation machinery unchanged.

## Options

1. **Candidate bands + a cheap text-presence picker (chosen).** Derive a small candidate set (top / profile-y / bottom) from the profile rect; score each on a single luma pass (dark share for presence, bright-on-dark text share for selection); crop and OCR only the selected band. The selection is sticky (a competitor must beat the current band by a margin before the crop moves), so the crop does not flicker on noise and the perceptual dedupe keeps working on one stable stream.
2. **A full-frame band detector (a scan for "the" textbox somewhere in the picture).** Rejected: it is a new detector with its own tuning surface and failure modes (a dark scene, a HUD bar, a cutscene caption all look like "a band of text"), it changes the crop's horizontal/size contract (the profile no longer defines the box), and it is strictly more work per frame than scoring three known candidates. The profile already knows the box's width/height/horizontal position; only the vertical placement is unknown, so the detector only needs to answer "top, middle or bottom" — which the candidate picker answers directly.
3. **Three explicit textbox regions (one per placement) and let the matcher pick.** Rejected: it multiplies the OCR cost by three (three regions, three crops, up to three OCRs), it pushes the placement decision into the matcher (which scores *text*, not *where the box is*), and it makes the profile a per-placement artefact the operator must re-draw for every game. The picker keeps the profile one rectangle and keeps the OCR count at one.
4. **A per-profile "placement" column (schema change).** Rejected: the placement is a *runtime* property of how the game draws the box, not a property the operator calibrates; a schema column would require a migration and a re-publish for the behaviour to reach an existing profile, whereas the requirement is that the existing rev2/rev3 profiles pick the behaviour up without re-publishing. The candidate set is derived at runtime from the stored rect (a runtime default), so no schema change and no migration.

## Decision

**A `textbox` region is cropped placement-aware.** For a region whose `region_type` is `textbox`, the ROI stage keeps the profile rect as the **base** band (its `x`, `w`, `h` and its `y`) and derives the candidate set:

- **top** — `y = 0`
- **profile** — `y = base.y` (the calibrated band; the middle placement in practice)
- **bottom** — `y = 1 − base.h`

Duplicate anchors (a profile already at the top or bottom) collapse, so the set is 1–3 bands. Every other region type keeps the single fixed band exactly as it does today (the placement problem is specific to dialogue boxes; a speaker label or a sign is one place).

Per frame, on the frame-ordered stage (inside `regionState.accept`, before `analyze`), the stage scores each candidate on a **single luma pass** of its raw crop:

- `dark` — the share of luma pixels below the presence `DarkLuma` (reuses the presence threshold; the panel is dark).
- `text` — the share of bright pixels (luma > the text luma, 140) that have a dark neighbour (luma < the presence `DarkLuma`): bright strokes sitting on a dark panel, the text itself.

The **selected band** is chosen with sticky hysteresis. The load-bearing rule, measured on the real frames: **a band is a box candidate only when it carries text on a panel — its `text` share reaches `PlacementTextMin` AND its `dark` share reaches the presence floor.** Dark alone is never a box: a near-black scene puts a panel on every band, so if the dark share were the eligibility signal a boxless dark frame would drag the crop to whichever band happened to be darkest, change the crop's hash every frame, and reset the perceptual dedupe. Requiring the text share is what keeps the crop put on a dark, boxless frame — the region then reads as present-or-absent on the band it is already on, exactly the single-band behaviour.

1. **No candidate carries text on a panel** (no band with `text ≥ PlacementTextMin` and `dark ≥ MinDarkShare`): the crop does not move. The region's presence is read on the standing band (its dark share) the way the historic path always did, and the dedupe memory and the `a.present` gate behave exactly as before. The standing band is kept so re-acquisition is immediate the frame the box returns.
2. **Otherwise the best text-bearing candidate wins** (the eligible band with the highest `text` share), with sticky hysteresis: the standing band keeps the crop unless the best band beats it by a **relative margin** (the best's `text` share is at least `PlacementSwitchMargin`× the standing band's `text` share, default 1.25). The margin is the anti-flicker — a one-frame blip of a few percent must not move the crop and reset the perceptual dedupe. When the standing band itself carries no text (the box moved), the crop takes the best band outright.

The selected band's rectangle is what `analyze` crops: its `present` (dark share), perceptual hash, preprocessed hash and OCR all run on the selected band, so the tracker, the dedupe, the settle and the escalation see one stable stream exactly as before — the box's pixels are just read from the band where they are. The live view's last-crop record (`noteCrop`) reports the selected band, so the developer inspector shows the band that was actually used.

The behaviour is a **runtime default**: it is on for `textbox` regions with no profile, schema or migration change, so the existing rev2/rev3 profiles pick it up on the next run without re-publishing. A `Config` knob (`PlacementDisabled`, default false) turns it off for a profile that needs the historic single-band behaviour (a game whose box genuinely does not move), and the knobs `PlacementSwitchMargin` / `PlacementTextMin` are overridable for tuning against a measured capture — the same "thresholds, not code paths" discipline the presence detector already follows (docs/spec/11 step 4).

**Cost.** The selection is three cheap passes (top/profile/bottom) over the band — a crop, a luma conversion and the text-presence walk each — run on the frame-ordered stage, against the ~600 ms OCR it steers. Measured on the real live-hdmi capture (`TestPlacementOnLiveCapture`, 511 frames, 640×480): the three-band selection is **~7 ms p50** on the development host, about **1% of the `frame_to_ocr` p50 (600 ms, ADR-0115)** — a small, bounded addition to the ROI stage, not a new OCR call. No additional OCR is dispatched: exactly one band is cropped and recognised, through the unchanged dispatch/dedupe/cancellation machinery. The measured per-session standing band (the top band for the 211-frame top-box session, the profile band for the 300-frame mid-box session) is the selection's own evidence that it lands on the band the box occupies.

## Robustness (2026-10-21: the Dolphin probe run's banner lock)

The first real-Dolphin run through this selection (the issue-25 catch-up E2E on the dev host) found the failure the hysteresis above did not cover. The dev host renders with **llvmpipe** (no GPU): after a savestate load, Dolphin's renderer init prints its GL info block — vendor/renderer/version, several lines of small monospace text — into the **top of the framebuffer** for the first seconds of the session. Measured on that run's dev-trace frames (the 90-frame probe bundle, 640×480):

| band (the profile's lower textbox region) | dark | text | on-panel |
|---|---|---|---|
| top (the banner's band) | 0.79 | **0.027** | **0.80** |
| profile (the box's band, the crop's start) | 0.96 | 0.004 | **1.00** |

The banner's text share clears the floor (0.027 ≥ 0.01) and its dark share clears the presence floor (a dark game scene behind it). The old picker therefore took the top band on the first frame — and **stayed there**: the box's own line scored 0.004, under the floor, so the box's band was never even a box candidate, and no band was ever eligible to pull the crop home. Every OCR ran against the banner band's empty scene. **Nothing was read from the box** — the user-visible failure, reproduced on the captured frames.

The pixel-level discriminator the measured data gives (and the one a frame count cannot: the banner's whole lifetime out-scores the box's band, and no persistence constant separates a 30-second banner from a 30-second top-placed box) is **where the letters sit**. The box's letters sit **in the panel's rows** — every measured text pixel of the box band is in a row that is majority panel-dark (on-panel 1.00, both the probe and the live-hdmi top-box capture). The banner's letters sit **in scene rows** (on-panel 0.80 on the probe's darkest-scene frames; 0.00 once the scene behind them brightens). The fix adds that signal to the selection, in three parts:

1. **A new TextPresence signal: `onPanel`.** The walk keeps a per-row dark count in the same pass and reports the fraction of its text's bright-on-dark pixels that sit in **panel rows** (rows whose own dark share reaches 0.5). A band with no text reads 1 (vacuously) — the text floor is what keeps an empty dark band from being a box.
2. **The box-likeness gate.** A band is a box candidate only when it is **box-like**: `text ≥ PlacementTextMin` **and** `dark ≥ MinDarkShare` **and** `onPanel ≥ PlacementTextOnPanel` (new knob, default **0.9**, measured between the box's 1.00 and the banner's 0.80). The text share alone is no longer enough to be a box: the text is exactly what a HUD strip carries. The selection's score becomes **`text × dark × onPanel`** — text riding the panel, the backdrop preference — and the standing-band margin rule runs on that score.
3. **Switch persistence.** A competing band takes the crop only after it has been the **best box-like band for `PlacementSwitchFrames` consecutive scored frames** (new knob, default **3** — a 0.3 s settle at the capture's 10 fps): **never on a single strong frame**, and any frame where the competitor is not the best resets the run. The standing band is the profile's own band on frame one, so the calibrated crop is exactly as hard to leave as any other, and harder in practice: it is already where the box's line is.

With the gate, the banner is **not a box** (on-panel 0.80 < 0.9): no band is box-like while it shows, the crop never leaves the calibrated band, the box's line — on that band, its faint read included — is what the presence path reads and OCRs, and when the banner fades the crop is still home. With the persistence, a genuine box move (top → bottom between scenes) is followed within 0.3 s, and a one- or two-frame blip of bright-on-dark noise never moves the crop and resets the perceptual dedupe.

**Measured and rejected:** the panel's **extent** (the longest contiguous run of panel rows over the band's height) was the other candidate backdrop signal — box 0.70–1.00, banner 0.48–0.72 across the measured frames. The ranges overlap (the live-hdmi top box's 0.70 is below the probe banner's 0.72), so an extent gate either rejects a real box or admits the banner: it is not a separator, and it is not in the gate. The on-panel share is. (ADR-0119's discipline: the choice is by measurement, and the rejected candidate is recorded with its numbers.)

**Validation on the captured frames** (`TestPlacementOnDolphinProbe`, class-(b) replay of the probe bundle under the gitignored `artifacts/`, NOT RUN when absent): all 90 frames replayed through the selection — the crop **never lands on the top band** on either of the run's textbox regions (0 of 90 frames), the box's region keeps its calibrated band for the whole run (90 of 90), and the bounded OCR leg reads **50 trimmed runes off the standing band's crop against 0 off the top band's** — the box's line is read, the old failure shape (the banner band read, the box band starved) is gone. The live-hdmi replays are unchanged in verdict: the top-box session's crop still lands on the top band (209 of 511 frames) and reads the full line (mean 158 runes against the fixed band's 77, 58 of 60 sampled frames carrying the fuller line), and the mid-box session stays on its profile band (60 of 60).

**Known limitation (measured shape only):** a text strip that floats over a **uniform dark panel** — a banner-like block whose scene behind it is dark and flat enough that its letters' rows read as panel rows (on-panel ≥ 0.9) and whose text share clears the floor — would be box-like, and if it persisted longer than the switch run it would take the crop from the standing band. That shape was not present in any measured frame (the real banner scores 0.80 on-panel, and its scene is textured, not flat); a flat-panel banner is a game HUD, not an emulator artefact, and the residual guard for it is the same one every region already has: the crop reads what is on its band, and the trace's last-crop record shows which band that was.

## Consequences

- The Wind Waker top/middle/bottom box is read in full from whichever band it occupies; the "cut in half" empty read is gone at the source, before the matcher or the speech path.
- A text strip at the top of the frame — the llvmpipe GL-info banner the dev host's no-GPU renderer draws after a savestate load — is not a box: its letters sit in scene rows (on-panel 0.80, under the 0.9 gate), so the crop never follows it, and the calibrated band's box line is what gets OCR'd (the 2026-10-21 Dolphin probe replay: 90/90 frames on the calibrated band, 50 runes read against the banner band's 0).
- A dark scene with no box still reads as "present but empty" (the dark share is high on a dark band) — which the tracker already handles as an empty read and the matcher already refuses to commit — and the sticky margin keeps the crop from flickering between empty bands, so the perceptual dedupe keeps suppressing the repeated empty OCR.
- When the box moves (top → bottom between scenes) the selected band switches, the crop changes, the perceptual hash re-anchors, and the new line is read as a fresh occurrence — the correct behaviour for a new message.
- The picker is a pure function of the frame plus the remembered band, testable on synthetic top/middle/bottom boxes without a capture; the real-frame assertion (the rev3 capture selects the top band and OCRs the full line) is a class-(b) captured-frame replay under `$GAME_NARRATOR_FIXTURE_DIR`, NOT RUN when the fixture is absent.
- Non-textbox regions and a `PlacementDisabled` profile are byte-for-byte the historic single-band path; the change cannot move a sign, a speaker label or a HUD.

## Validation

- Unit tests (always run): a synthetic frame with a bright-on-dark text box placed at the top, the middle and the bottom — the picker selects the band the box occupies and leaves the others alone; a frame with no box keeps the remembered band; a synthetic **top banner + bottom box** frame (the measured Dolphin probe's shape) — the box's band wins, the crop never visits the banner's band, for as long as the banner persists; `PlacementDisabled` returns the base band unchanged; non-textbox regions are never re-placed; the duplicate-anchor collapse (a profile already at the top) yields a single candidate.
- Measured cost (always run): the three-band selection on a 640×480 crop is reported and asserted to be a small fraction of the OCR budget (well under the ~600 ms `frame_to_ocr` p50).
- Captured-frame replay (`$GAME_NARRATOR_FIXTURE_DIR`, the live-hdmi rev3 capture; NOT RUN when absent): the picker selects the top band on the top-box frames, and the selected band's crop OCRs the full line while the fixed profile band OCRs the truncated half — the before/after the ADR exists to prove.
- Captured-frame replay (the gitignored `artifacts/catch-up-live/probe-*` Dolphin bundle; NOT RUN when absent): the banner's band never wins the crop and the box's line is read off the standing band's crop — the §Robustness regression on the frames the failure was measured on.
- `go test -race ./internal/pipeline/... ./internal/vision/...`; `make test-replay` (the fixture replay gate) and `make lint`.

## Reversal plan

Set `Config.PlacementDisabled` true (the runtime wiring) to restore the single-band crop for every region without a code change — the base-band path is kept as the disabled branch, so the reversal is a config flip. To remove the feature entirely, delete the selection from `regionState.accept` (crop `vision.RectOf` directly as it does today), the candidate/metric helpers in `internal/pipeline` and `internal/vision.TextPresence`, and the `Config` knobs. No schema or migration is touched, so the reversal touches no data.

## Amendment (2026-10-21): the sea false positive (#41, #44)

**Context.** On the UNO Q, while Link sailed with no dialogue box on screen, the presence rule (dark share ≥ 0.30 of the crop) called the open sea a box. The bottom bands of 640×480 V316B frames are mostly dark saturated blue. Tesseract then read junk ("™ a a", "gh\"") and the unmatched read-through spoke it. Private fixture: `board-sailing-20261021` (40 OCR'd crops plus 20 frames, all with no box; outside git).

**Decision.** Presence is a triple gate on the raw colour crop (`vision.PanelPresenceOf`, `PresenceConfig.Present`):

| Gate | Threshold | Purpose |
|---|---|---|
| dark share | ≥ `MinDarkShare` (0.30, unchanged) | it looks like a panel |
| saturated-dark share | ≤ `MaxSaturatedDarkShare` (0.5) | it isn't a saturated scene showing through |
| text share | ≥ `MinTextShare` (0.0002) | it carries at least about one character |

Measured values:

| Input | Saturated-dark share | Text share |
|---|---|---|
| sea crops | 0.86–0.97 | up to 0.0024 |
| Wind Waker boxes, Dolphin fixtures | 0.000–0.057 | — |
| Wind Waker boxes, raw board crops | 0.002–0.069 | — |
| desaturated no-box bands (board capture) | — | exactly 0 |
| a 15-character line | — | 0.0032 (≈ 0.0002 per character) |

The ceiling sits an order of magnitude from both sides. The text floor is set to one character so that a "Huh?"-length line (≈ 0.0008) stays present. The worker's first value, 0.001, would have silenced lines of about 4 characters. The sea itself is rejected by the saturation ceiling, not by the text floor.

**Validation.**
- `TestPresenceRejectsSeaAcceptsBoxes` (fixture-gated: NOT RUN without `$GAME_NARRATOR_FIXTURE_DIR`):
  - all 40 board crops and 20 frames read absent;
  - the positive box-row counts on the three Dolphin fixtures are identical to the old dark-only rule (72/98, 193/1085, 533/682);
  - with the new gates neutralized, the same test fails on the sea crops.
- `TestPresenceMatchesCorpusTruth` (synthetic corpus) and `make test-replay` stay green.

**Reversal.** Set `MaxSaturatedDarkShare: 1` and `MinTextShare` to a tiny positive value in the profile's presence config.
