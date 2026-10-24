# ADR 0172: Read a dialogue box wherever it is — a frame-level panel finder and one dynamic region

- Status: accepted (implemented)
- Date: 2026-10-24
- Issue: #53 (ADR 0154 step 2, re-shaped)

## Context and constraints

A profile declares fixed regions; the pipeline only reads what those regions
crop. Wind Waker draws its box at the top, middle or bottom (the rev-5
profile added the top region on 2026-10-22 after a top box went unread), and
other games put boxes in other places entirely — a box outside every region
is never read.

ADR 0154 step 2 planned a "Clef ask with the whole frame when no region
covers a panel" path. Three ~90-minute attempts (2026-10-22) failed and were
not merged (issue #53):

1. **Frame-Clef routed through the dispatch → rankWorker channels** —
   deadlocked: the core loops blocked each other; the test hung 300 s.
2. **A side goroutine owned by the Runner** — left `Runner.Run` waiting on a
   goroutine that never exited (`TestLiveOCRReadSettlesUnderFrameChurn`
   hangs at the WaitGroup), and the diff sprawled into the matcher,
   recognition, tracker and text-presence code.

Issue #53's suggested third shape moved the scan into the runtime (a
session-context goroutine sampling `Runner.latestImage()` every ~250 ms) and
answered hits with a one-shot Clef ask fed through the decision-message path.

Constraints for the re-do:

- The UNO Q is CPU-bound at ~7.5 fps: the per-frame cost must be
  microsecond-scale at the scan cadence, not another millisecond-scale
  full-frame pass on every frame.
- No new goroutine that `Run` waits on; no new channel into the
  dispatch/rank machinery; no edits to the matcher, recognition or tracker.
- Same ids/events contract as a configured region, so the session reducer,
  the per-region tracker and the dev UI read the dynamic region's
  observations like any other region's.

## Options

1. **The issue's suggested shape** (runtime-side sampling goroutine +
   one-shot Clef ask per appearance). Avoids the pipeline, but keeps the
   side-goroutine `Run`-wait shape the second attempt proved fragile, adds a
   full-frame copy every 250 ms at the runtime layer, and answers the box
   with a model call instead of the pipeline's own OCR — the line never
   goes through the presence → tracker → matcher path, so the dynamic box
   has different reliability and event semantics from every other box.
2. **A frame-level panel finder in the pipeline, feeding one dynamic
   region** (chosen). A pure-function row scan of a *downsampled* luma
   frame (160×120) finds dialogue-panel bands; a scheduler hook on the
   existing frame-ordered ROI stage turns a hit into one dynamic region's
   rect, and the region rides the existing presence → OCR → tracker →
   matcher path. No new goroutine, no new channel, no matcher/tracker
   changes.
3. **Extend the ADR-0137 placement bands** (top/middle/bottom) until the
   box is covered. Per-profile calibration that never generalises to
   arbitrary placements — the reported problem itself.

## Decision

Option 2, implemented in `internal/pipeline/panelfind.go` (the finder) and
`internal/pipeline/dispatch.go` (the scheduler hook):

- **The finder is a pure function** (`FindPanels(luma, w, h, cfg)`),
  over a downsampled luma frame produced by `panelLuma`. The luma walk is
  bytes-per-pixel aware (3 or 4): `*image.RGBA`/`*image.NRGBA` (what a
  PNG decode and a UVC capture return) are 4-byte pixels, and the original
  walk read them at a 3-byte offset — silently producing a garbage luma
  on every real frame and no panel on any of them. That was the root
  cause of the live scenario finding nothing; the fixture corpus
  (`*image.Paletted`) took the correct slow path, which is why the
  synthetic tests passed while the live one failed. The scan: one pass
  over the rows — a row is a panel row when one polarity holds at least
  `MinToneShare` (40%) of the row's pixels — a **share, not a contiguous
  run**: a dialogue panel is often translucent, and the content behind it
  breaks the tone into short runs while the share stays high. A row whose
  dark and bright pixels merely balance out (a busy scene) is below
  `MinToneShare` in each polarity and stays scene. A panel is a run of
  same-tone panel rows within the height bounds, bridging at most
  `MaxRowGap` neutral rows at a time (the letters ride the panel in the
  opposite tone and interrupt it). If a bridged run is taller than
  `MaxHeight`, it was two same-tone bands joined across a scene gap (a
  dark strip above, the box below), so it is split at the gaps and each
  sub-band is considered on its own. Each band's pixels must also carry
  **text-like edge density** — enough local contrast for letters, not so
  much for a busy scene — the gate that rejects a smooth gradient (the
  sea). Candidates rank by panel-ness; at most `MaxCandidates` (3) are
  returned.
- **The rank is what the dynamic region crops** (`Score`), ordered: the
  band's **plain panel rows** (a box carries dozens of fill rows; a window
  is all mullions → zero plain rows; a letterbox a handful; a scene strip
  a few), then its **height** (a box is a tall band, a strip short), then a
  small bonus for carrying some letters (a live box vs a same-size
  letterbox). A box whose one line of dialogue is too thin to register on
  a given frame keeps its plain + height — it does not fall to a bordering
  strip just because a line is faint. Measured on the captured first-
  dialogue frames the box ranks first by ~30× the runner-up on every box
  frame, the window and letterbox far behind.
- **The scheduler** (`panelFindTick`) runs on the ROI stage's existing
  frame case, after every configured region has looked at the frame. It
  scans at most every `PanelFind.EveryNFrames` frames (default 4: at
  ~7.5 fps that is ~530 ms — a dialogue box stays on screen for seconds)
  and **only while no configured region reports a present box**
  (`regionState.lastPresent`). When a configured region sees a box, the
  fixed path owns the frame and the finder is skipped — the fixed-region
  path pays nothing when it works.
- **The dynamic region** is one extra `regionState` with the fixed id
  `panel-anywhere`, born with an empty rect. A found panel no configured
  region (stored band or any placement-aware candidate band) overlaps
  becomes its rect; the ordinary presence → OCR → tracker → matcher path
  does the rest, so the observations carry the same ids/events contract —
  the `panel-anywhere` region id is the "dynamic placement" mark the
  reducer and dev UI read. A panel the finder loses (rect stays
  unchanged/empty) degrades to "no evidence", never to "wrong text".
- **Config**: `pipeline.Config.PanelFind` (`PanelFindConfig`, all
  thresholds — scan size, panel-tone lumas, tone share, height bounds,
  edge delta/shares, row gap, cadence, candidate cap — with
  `DefaultPanelFind()` defaults). `Enabled` is **off by default**: the
  fixed-region path is the primary mechanism, and a profile that does not
  enable the finder pays exactly zero per-frame cost. The finder is
  generic — no game coordinates anywhere in the scan.
- **Clef ask with the whole frame (ADR 0154) is deferred.** The dynamic
  region reads the box with the pipeline's own OCR; the one-shot
  whole-frame Clef adjudication is optional reinforcement for the
  dynamic-region case and is not needed for the box to be read and
  committed. If it is built later, it must not re-introduce the shapes
  that failed: no side goroutine `Run` waits on, no new dispatch channel.

## Why this does not repeat the failed shapes

- The scan runs **inside** the ROI goroutine's existing frame case — no
  new goroutine, so nothing `Run`'s WaitGroup can wait on, and no
  `latestImage()` copy. The frame is already in the ROI stage; the scan
  only adds a downsample pass over it.
- The result is a **rect on an existing region**, not a message routed
  through new channels — the dispatch and rank machinery are untouched.
- The diff stays in `panelfind*.go`, `dispatch.go` (marked ADR-0172) and
  the `PanelFind` config field; the matcher, recognition, tracker and
  session reducer are untouched.

## Validation

- **Unit** (`panelfind_test.go`): synthetic 160×120 frames — dark and
  bright panels at top/middle/bottom, an odd 97×61 size, a busy
  checkerboard scene (no panel), a smooth sea gradient (no panel — the
  edge gate), a uniform scene (no panel), a box whose tone share is below
  `MinToneShare` (no panel), and a full-texture line that splits the band
  into two half-bands with a letters-gap (one panel, the gap rule). Config
  normalisation is tested too.
- **Pipeline** (`panelfind_dispatch_test.go`, synthetic replay corpus):
  with the configured region shifted out of the box's reach and the
  finder ON, both lines commit and the observations carrying them come
  from region `panel-anywhere` while the shifted region never reports a
  present box; finder OFF, nothing commits (the counterfactual); with the
  configured region in place and the finder ON, the fixed path commits
  through the configured region and the dynamic region stays absent
  (the gate holds).
- **Captured frames** (`panelfind_probe_test.go`, gated on
  `GN_PANEL_PROBE_DIR` — NOT RUN when unset): pointed at the captured
  Wind Waker first-dialogue frames, the box frames report the bottom panel
  (unit rect x=0.000 y=0.600 w=1.000 h≈0.400) ranked **first by ~30× the
  runner-up on every box frame** — the bordering scene strip, the cabin
  window and the letterbox all rank far behind — while the box-less frames
  report none. Output is rectangles only (rule 5).
- **Live scenario** (`internal/e2e/box_anywhere_live_test.go`, gated on
  `GN_BOX_ANYWHERE_E2E=1` + the live env, run through the shared Dolphin
  lock): the real pipeline against the real Dolphin emulator from the
  verified first-dialogue savestate; the profile's single textbox region
  is registered at the TOP of the frame so the fixed path reads nothing;
  the dynamic region finds the bottom panel, reads it with Tesseract and
  commits the line. Measured run: drive complete in 1 m 45 s;
  **dynamic-region present observations 206, of which 198 carried OCR
  text**; the shifted top region reported present 0 times; **2 lines
  committed** (1 distinct message id). `PASS` (116 s).
- **Cost**: `BenchmarkFindPanelsFrame` measures the full
  downsample+scan on a 640×480 frame on the dev host; the REPORT carries
  the µs figure and the UNO Q projection.
- `make lint`, the package's unit tests and the race tests pass.

## Tradeoffs

- The finder only runs while every configured region is absent; a box
  that appears *inside* a configured region's band is that region's, and
  the finder stays quiet — the fixed path's behaviour is bit-for-bit
  unchanged when it works. The cost of that rule is a box that straddles
  a region edge: the region owns it.
- At the default cadence (every 4 frames) the dynamic region's rect lags
  the true panel edge by up to the scan period — harmless for presence
  and OCR (the panel is static while on screen) and the presence gate on
  the region's own crop is what reads the text, not the finder's rect.
- The finder's band is a coarse luma measurement; a bright panel over a
  bright scene or a dark scene over a dark panel can miss `MinToneShare`.
  The thresholds are config, and the configured regions remain the
  primary path.
- One dynamic region at a time: the scan's best candidate wins. Two
  genuine boxes in different bands is a game the fixed-region path
  already serves with two regions.

## Consequences

- A profile that enables `PanelFind` (off by default) reads boxes at any
  height without calibration; observations arrive under region id
  `panel-anywhere`, which the dev UI and any per-region consumer can
  distinguish from profile regions.
- The session reducer and tracker gain no region-specific logic: the
  dynamic region is one more region, so their behaviour is unchanged.
- The product-facing wiring (a knob in the runtime's session config that
  turns `PanelFind` on per profile) is the natural follow-up; the
  pipeline is ready for it. The whole-frame Clef ask (ADR 0154 step 2's
  original ask) remains optional reinforcement, to be designed around
  the constraints above if needed.

## Reversal plan

Delete `internal/pipeline/panelfind*.go`, the ADR-0172-marked block in
`dispatch.go` (the `dyn` region, `panelFindTick`, `panelCoveredBy`,
`rectsOverlap`, `lastPresent`) and the `PanelFind` config field. Nothing
else references the finder: with `Enabled=false` (the default) the
pipeline is already the fixed-region path unchanged.
