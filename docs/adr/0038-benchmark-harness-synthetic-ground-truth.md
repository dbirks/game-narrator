# ADR-0038: Benchmark harness — labelled synthetic ground truth, real pipeline, sidecar speech layer

Date: 2026-10-17
Status: accepted
Records decisions made in this change (`internal/bench/`: `doc.go`, `truth.go`, `profile.go`, `generate.go`, `run.go`, `metrics.go`, `report.go`, `bench_test.go`, `e2e_test.go`; `cmd/game-narrator/bench.go`; `testdata/bench/bench-synthetic-001/`; `Makefile` `bench` target).
Owners: project
Related requirements/tests: docs/spec/12 (the benchmark suite, provisional objectives, "Frame replay"); docs/spec/16 M7; AGENTS.md ("No simulation theater", "Keep third-party ROMs out", "make bench … do not mark pending targets green"); ADR-0035 (candidate matcher scorer — the probability the calibration rows measure); ADR-0037 (scenario harness — the M2 speech-sidecar pattern this reuses)

## Context and constraints

docs/spec/12 requires a benchmark that scores recognition (precision/recall, OCR CER/WER, top-1 exact accuracy, abstention), calibration (Brier + 10-bin ECE of the matcher's top-1 probability), speech scheduling (positive-speech precision, missed/duplicate/stale speech), and latency/resources (p50/p95 commit latency, CPU time, peak RSS) — every row carrying its sample count, every empty row `n/a`, and each provisional objective reported as met/not met/n-a, never "achieved".

Hard constraints that shaped the design:

1. **No game content.** AGENTS.md forbids committing ROMs, game-script dumps or copyrighted art. The set must therefore be synthetic: the scenario's own generated lines (`testdata/synthetic/profiles/dialogue-advance-001.json`), rendered to PNGs.
2. **No simulation theater.** The numbers must come from the *real* system under test: the pipeline in `ModeReplay` with real Tesseract, the real matcher index, the real session reducer. A stubbed OCR that answers nothing must score zero, not "pass".
3. **Tuning/holdout discipline.** The split must be stable across runs and independent of the outcome, or a future tuning pass can silently fit the holdout.
4. **test-full budget.** AGENTS.md: `make bench` joins `test-full` only if it completes in under five minutes.
5. **The M2 pipeline has no TTS worker.** Speech events are not produced by the pipeline; the established M2 evidence pattern (`internal/pipeline/replay_real_test.go`) is a *session-level sidecar*: real session state machine, real matcher rankings, real timing, scripted TTS-ready.

## Decision

### Package and runner

`internal/bench` owns the set format, the generator, the runner and the metrics. The runner is a **CLI subcommand** (`game-narrator bench --set DIR --split tuning|holdout|all --out DIR`), not a Go test.

Options considered: a `go test`-only runner, or a CLI runner exercised by tests.

- A test-only runner could not be invoked by an operator to produce an evidence bundle (`artifacts/<run-id>/bench/`) without `go test` plumbing, and re-scoring a set after a model change would mean recompiling the test binary.
- A CLI runner is directly invocable from the `Makefile` gate, writes the evidence files under a gitignored `artifacts/` tree, and stays deterministic; the E2E test (`e2e_test.go`) then drives the same `RunBench` entry point on ten generated samples and asserts the report is well-formed and recall > 0 (a stubbed OCR scores zero recall, so the assertion is what makes the run "real"). The test skips NOT RUN when tesseract/magick are absent.
- The CLI refuses to start (exit 3, `NOT RUN`) when tesseract or magick is missing, instead of burning 144 samples on per-sample failures.

### Ground truth

One set per directory: `testdata/bench/<set>/{profile.json, truth.json}`. `truth.json` is schema-versioned (`schema_version: 1`) and validated on load: unique `bn%04d` ids, strictly increasing frame timestamps, closed `text_kind` vocabulary, split must equal the deterministic split of the id, dialogue samples carry the script message id, text and a non-empty speaker (unattributed lines are the explicit `unknown`), and the whole set must be declared `generated_synthetic` with the seed base and source profile recorded.

**Split rule:** `SplitForID` hashes the sample id (FNV-64a) and puts roughly 30% in the holdout. The split depends only on the id, never on any measured result, and is recomputed and *checked* on every load, so a hand-edited `truth.json` cannot move a sample between tuning and holdout. The committed set `bench-synthetic-001` has 144 samples (95/49): 14 script lines × 3 fonts (DejaVu Sans, Noto Serif, Liberation Mono) × 3 resolutions (640×480, 720×480, 1280×720), plus 14 analog-noisy renders (scanline noise) and 4 empty-box samples.

**Frames are not committed.** The generator (`GenerateSamples`) is a deterministic Go function of (source profile, seed base); the frame PNGs are rendered into a temp directory at run time by `RenderSamples` (ImageMagick, fontconfig, per-resolution point-size fit, scanline noise for the noisy variant). Only the small, reviewable `truth.json`/`profile.json` are committed; the frames are reproducible at any time (`bench --generate` rewrites the set's JSON). This keeps the tree free of image blobs while the loader still refuses a set it cannot render.

### What is real and what is scripted

The runner replays each sample's frames through the **real pipeline** (`pipeline.Config{Mode: ModeReplay, OCR: ocr.Tesseract{}, Index: <matcher index of the set's script>, Tracker: VoteWindow 3 / StableWindow 100ms}`) and grades:

- recognition precision/recall, CER/WER, top-1 exact accuracy, abstention, Brier/ECE and commit latency from the pipeline's own events and trace (real OCR, real matcher probability, capture-clock timestamps);
- named-speaker accuracy from the pipeline's committed message answered by the **scripted decision provider** (the synthetic stand-in for Clef: the script's speaker attribution — in-script messages are eligible, unattributed lines never receive a forced speaker).

Speech-scheduling metrics come from the **M2 speech sidecar**: a fresh session, same tracker and matcher, fed the sample's *truth* frames (real timing), real matcher rankings, scripted decision and immediate TTS-ready. It measures the speech layer — exactly once per occurrence, right message, no stale generations — not OCR quality, which the pipeline rows already measure. TTS readiness is scripted; no audio is produced, and the report says so.

Samples run on a bounded 4-worker pool (independent sessions); the run's duration/CPU/RSS are read from `/proc/self` (bench process only — tesseract/magick are subprocesses, and the report labels that honestly).

### Objectives

The report prints docs/spec/12's provisional objectives verbatim and judges each `met` / `not_met` / `n-a` only (counts: met at 0 with samples; ratios: against the `≥X%` threshold). The objectives are labelled *provisional* in every row and the ADR-0038 limitation below applies: on synthetic content a `met` verdict is evidence the pipeline *works*, not evidence the product *meets* its quality bar.

## Tradeoffs

- **Synthetic-only scope.** The set proves the pipeline reads conventional rendered text and that the fences hold; it does not prove the product on Wind Waker. That is the price of AGENTS.md rule 5, and the report's manifest and header say the set is `generated_synthetic`. Real game frames must come from the Dolphin fixture factory with provenance (docs/spec/11) before any objective is re-validated on them.
- **Truth-fed sidecar** makes the speech rows measure the scheduling layer rather than "OCR → speech" end-to-end. The alternative (feeding the sidecar the pipeline's own trace) was tried and rejected: ModeReplay's trace intermixes ROI rows and ranking rows (FrameSeq 0) and hash-deduplicated OCR rows, so the sidecar's tracker never saw a clean stable reading. Perception and speech are therefore graded on different (both real) subsystems, and the report's details say which.
- **CLI runner over test runner** costs a small surface in `cmd/game-narrator` but is the only shape that writes operator evidence bundles and stays inside the 5-minute `test-full` budget (measured 2m46s holdout on an 8-core box).
- **`/proc/self` resources** exclude subprocess CPU; the honest number for "how expensive is a run" is the reported duration, and the CPU/RSS rows are labelled as process-only.

## Validation

- `go test -race ./internal/bench/` — loader validation (15 mutation cases), split determinism, every metric on known values (empty input → `n/a`, CER/WER, Brier 0.64 known case, ECE 0.5 and 0 known cases, nearest-rank percentile, verdict thresholds, speech/stale/dup counting), generator determinism, committed-set round-trip (144 samples, 14 messages).
- `go test -race -run TestBenchEndToEnd` — ten generated samples through the real pipeline + Tesseract: report well-formed (all 15 rows, n=0 ⇔ n/a, files on disk, manifest fields), recall > 0 (measured 9/9, CER 0.2%, p50 200ms).
- `make bench` — holdout of the committed set: 49 samples, measured 2m46s; report.md table with met/not met/n-a per provisional objective; artifacts in gitignored `artifacts/bench/`.

## Consequences

- `test-full` runs the holdout bench for real (gate `bench` in `RUN_GATES`); a broken pipeline (e.g. an OCR regression that zeroes recall) now fails the gate instead of the M2 clip.
- The provisional objectives are now re-checkable evidence: every model/matcher/OCR change gets a holdout table. When the Dolphin fixture factory provides real frames (with provenance), a second set `bench-ww-*` is generated the same way and the objectives are re-validated — and revised, via a new ADR, against the real numbers.
- The generator + truth loader are the seam where that real set will attach: same schema, same split rule, same runner.

## Reversal plan

Delete `internal/bench/`, `cmd/game-narrator/bench.go`, the `bench` case in `main.go`, the Makefile `bench` target and its `RUN_GATES` entry, and `testdata/bench/bench-synthetic-001/`. Nothing outside this ADR's file list imports `internal/bench`; the M2 pipeline and scenario harness are untouched by construction (the sidecar reuses their existing public APIs).
