# ADR-0085: One-shot `game-narrator ocr` command (issue #1)

Date: 2026-10-20
Status: accepted
Owners: ocr-command worker
Related requirements/tests: GitHub issue #1 (one-shot `game-narrator ocr`), docs/spec/05 ("OCR pipeline" — preprocessing, region detection, word boxes and confidence, timings), docs/spec/10 (capture test — "never assume /dev/video0"), docs/spec/01 ("Key invariants" 2 — only the published revision runs), docs/manuals/18-hdmi-capture-on-uno-q.md ("First-boot checks"), ADR-0078 (capture product wiring — the per-device busy check and the single-consumer UVC bus), ADR-0082 (the production crop options are `vision.CropOptions{}` until the runtime is wired to the active-picture tracker); tests `TestRunOCR*` (cmd/game-narrator), `TestRun*` (internal/oneshot), `TestHWOCRCommandDevice` (internal/hwsmoke, the hardware gate)

## Context and constraints

Issue #1 records the evidence: on a real UNO Q with a UVC HDMI capture card
and a live GameCube feed, the operator wanted to know "what does the device
read right now?" and had to grab a frame with FFmpeg, crop it by hand and run
`tesseract` directly — which skips the product's preprocessing, region
detection, OCR configuration, normalization and script matching. The detour
proves the hardware, not the pipeline (AGENTS.md rule 3: no simulation
theater, and its twin — a hand-rolled pipeline is not the product's).

Constraints that bind the shape:

- The recognition must reuse the real pipeline code paths, not a parallel
  reimplementation (task brief for issue #1).
- The capture format negotiation must be the session's: `stack.Resolve`
  (the requested id, the persisted selection, the ADR-0076 policy) and the
  same source construction `serve` wires — `docs/spec/10`'s "never assume
  `/dev/video0"` holds for the command too.
- A held device must fail clearly (ADR-0078: the UVC bus is single-consumer;
  a running session holds the node and FFmpeg's open fails with EBUSY/EAGON
  rather than delivering).
- `--game` may only read the published revision (docs/spec/01 "Key
  invariants" 2), the exact load the session runtime applies.
- Debug images contain game frames: local only, never in support bundles
  (AGENTS.md rule 5), and the command must work while `serve` is stopped
  (a separate process; it cannot ask the service whether the device is held,
  so the busy answer comes from the open attempt itself).

## Options

- **A new subcommand on a parallel, hand-written pipeline.** Rejected: it
  would be a second spelling of crop/preprocess/OCR/rank that drifts from
  the session's, and the issue's complaint ("proves only that the hardware
  works, not that the pipeline does") would apply to the new command itself.
- **A `--once` flag on `serve` or a new API route.** Rejected: the point is
  the service-free diagnostic (the service is precisely what the operator
  cannot start or does not want to hold the device with); an API route would
  need an authenticated server for a single frame.
- **A small `internal/oneshot` package + a thin CLI command (chosen).** The
  package runs the pipeline's ROI-stage sequence — `vision.CropNormalized`,
  the presence walk (`vision.DarkShare`), `vision.Preprocess` with the
  pipeline's recipe, the engine call in the pipeline's `ocrRecognize` shape
  (raw-crop engines get both inputs, ADR-0040), the matcher fold and `Rank`
  — with the session machinery (reducer, tracker, queues, retries,
  supersession) removed because one frame has no newer frame to supersede
  it. The CLI wires the capture stack and the store exactly as `serve`
  does; the package is engine-injectable, so unit tests run it over
  synthetic frames with a scripted engine (class (a)), the cmd test runs it
  over the repo's synthetic controlled asset with the real Tesseract, and
  the hardware gate (`TestHWOCRCommandDevice`) runs it over a real UVC frame.

## Decisions

- **`game-narrator ocr [--game <profile-id>] (--device <node-or-id> | --image <file>) [--json] [--save-debug DIR] [--top N] [--data-dir DIR]`**,
  exit 0 on a report (including a read of nothing — a frame with no box is an
  answer), 1 on failure (no frame, device busy, unknown game, the engine
  failed on every region), 2 on bad usage (exactly one of `--device`/`--image`;
  `--top` in 1..50, the matcher's own bound).
- **The one-shot path takes the defaults the live pipeline takes**
  (`internal/oneshot.Options.normalize` mirrors `pipeline.Config.normalize`:
  `vision.DefaultSteps()`, `pipeline.DefaultPresence()`,
  `ocr.DefaultTimeout`, `games.TextDialogue`). The crop options are
  `vision.CropOptions{}` for profile regions — the production setting
  (ADR-0082: the runtime is not wired to the active-picture tracker yet, and
  the one-shot reader must not crop a single frame differently than the
  session would). The no-game mode reads the whole active picture through
  the stateless `ActiveArea` scan (`LetterboxDetect` without a tracker — the
  honest one-frame answer; a tracker needs a frame stream to hysteresise
  over). When the runtime wires the tracker (ADR-0082 follow-up), the
  one-shot path follows it through the same `CropOptions.Tracker` field.
- **Presence is measured on the raw crop, not the preprocessed one**, with
  the pipeline's parameters — the same fact the pipeline's `analyze`
  records, so a "no text" read can be told apart from a "no box" one. The
  pixel walk moved to `vision.DarkShare` (beside `ToGray`); the pipeline's
  private copy now delegates to it, so the detector has one implementation.
- **`--device` grabs one frame and closes the source** (the API's preview
  snapshot shape: build, start, one frame, close), through `stack.Resolve` +
  the ADR-0076 mode — the session's negotiation. A held device is detected
  from FFmpeg's failure tail (EBUSY/EAGAIN spellings) and answered with the
  operator action: stop the session, or use the running service's session
  API. No frame within 15 s is a wedged or held source, reported with the
  source's own state and last error.
- **`--game` loads the published revision exactly as `runtime.Start` does**
  (`GetProfile` → `PublishedProfile` → the pointer-and-status agreement
  check → `GetProfileContent`), and the matcher index is `matcher.Build`
  over that content with the zero `IndexOptions` — the session's index.
  Each read is ranked with the raw text, the engine's mean confidence and
  the pipeline's `TextKindHint`, and the report carries the matcher's
  reason, abstain verdict and the top-N candidates with both score
  spellings (raw log-linear sum and softmax probability).
- **`--json` is the report's faithful structured spelling** (one entry per
  region: the stored rect, the crop box in frame pixels, presence, empty,
  raw and normalized text kept separate — docs/spec/05 — confidence, engine
  name, duration, error, rank). `--save-debug DIR` writes `source.png` plus
  each region's `-crop.png` (raw pixels) and `-preprocess.png` (the recipe's
  output) through a `Debug` callback on the oneshot options; the images
  stay local and are never collected into a support bundle.
- **The VLM fallback is not wired into the one-shot reader.** The command is
  a local diagnostic; consulting a provider role for one frame is the
  session's job (ADR-0040's rate limit and crop cache are per-run state).
  The engine is the local one the runtime defaults to (`ocr.Tesseract{}`),
  and the raw-crop interface shape is still honoured, so a future local
  two-input engine plugs in unchanged.

## Alternatives considered

- **Reusing the API's `Probe` (POST /api/v1/capture/probe).** It OCRs a
  pixel box with the region editor's Tesseract (PSM 7) — not the pipeline's
  preprocess/region/matcher path, and it needs the running service. It
  remains the in-service probe; this command is the service-free reader.
- **Letting the command run a full session with a manual source.** A session
  drags in the reducer, the speech sink and the event log to read one frame;
  the one-shot package is the same recognition without that machinery.

## Validation

- `go test -race ./internal/oneshot/` — synthetic frames generated in-test
  (a letterboxed 640×360 canvas, a 640×480 panel frame), scripted engine:
  the fallback region reads the active picture (the exact 80 px bar trim),
  profile regions crop to the exact `MapRect` the pipeline's `analyze`
  computes, empty rects and nil images read empty without panicking, engine
  failure is per-region, the matcher fold and top-k bound hold, raw-crop
  engines get both inputs (raw crop = the actual pixels), a cancelled
  context stops the run.
- `GN_GATE=test-unit go test -race ./cmd/game-narrator/` — `TestRunOCRUsage`
  (exit-2 contract), `TestRunOCRImageJSON` (the real Tesseract over the
  controlled asset: 400×100 frame, `NARRATOR 42` within 5 % CER, the JSON
  schema), `TestRunOCRImageSaveDebug` (the three debug PNGs decode),
  `TestRunOCRUnknownGame` (exit 1, the id named), `TestRunOCRGameRanksAgainstThePublishedProfile`
  (a seeded published profile: the controlled read ranks the matching
  corpus line top-1, the `--top` bound held, the fold reported).
- `make test-hardware` (the hardware gate, `TestHWOCRCommandDevice`): the
  installed-or-built binary runs `ocr --device <node> --json` over the real
  UVC node — the frame's dimensions are real, the read is the real
  Tesseract engine with no per-region error (class (e) real hardware).

## Consequences

- The operator's "what does the device read" question has a one-liner that
  exercises the product's pipeline, and the evidence (the report, the debug
  images) is in the product's own vocabulary.
- `doctor` can reuse the oneshot package for an optional "capture + OCR
  smoke" row when a doctor check needs one (issue #1's suggestion) — the
  package is the seam; no command machinery has to be imported.
- One new CLI contract (flags, exit codes, the `--json` schema) — versioned
  by this ADR; the report field set may grow, not shrink silently.
- The dark-share walk moved to `vision` and the pipeline's copy delegates:
  behaviour-identical (same walk, same parameters), covered by the pipeline's
  existing presence tests.

## Reversal plan

Drop the `ocr` case from `run()` and delete `cmd/game-narrator/ocr.go`,
`internal/oneshot/` and the two test files; revert the `vision.DarkShare`
delegation in `internal/pipeline/roi.go` (the walk goes back inline). No
schema, protocol, store or cache-key change was made, so nothing else
reverts.
