# ADR-0058: Captured-Dolphin bench holdout — the M7 real-frame evidence and the calibration verdict

Date: 2026-10-18
Status: accepted
Owners: agent (M7 bench/calibration)
Related requirements/tests: docs/spec/12 "Frame replay", "Metrics" and
"Calibration" (provisional objectives), docs/spec/16 M7 ("benchmark targets met
or ADR-adjusted with evidence"), docs/spec/13 evidence correlation, ADR-0035
(calibration: a refusal is a verdict, never a T), ADR-0038 (bench harness and
the scripted speech sidecar), ADR-0054 (Dolphin fixture factory), AGENTS.md
rules 3 (provenance classes) and 5 (no game material in tracked files).
Tests: `TestCapturedBench*` (internal/bench/captured_test.go), `TestBenchE2E`
(unchanged synthetic assertions), `make bench`.

## Context and constraints

M7's gate is "benchmark targets met or ADR-adjusted with evidence", and the
evidence must distinguish provenance classes (AGENTS.md rule 3). The synthetic
holdout (class (a)) had been running since ADR-0038; M7 requires a class (b)
captured-frame replay holdout reported separately, and a calibration fit over
the combined real-frame observations.

Inputs for this ADR:

- **Fixture**: `artifacts/fixtures/windwaker-newgame` (gitignored, AGENTS.md
  rule 5) — 25 frames at 640x480 from a real Dolphin drive, sampled on a
  ≈2.1 s grid, with per-frame oracle truth (`box_present`, `text`) and a
  manifest carrying each frame's SHA-256. It contains two dialogue lines:
  line *a* present in 3 consecutive frames, line *b* present in 1 frame (the
  fixture's last), and empty (non-dialogue) frames between and around them.
- **Script**: imported at run time from the user's Wind Waker image via the
  windwaker adapter (the image's SHA-256 must match the manifest's; the image
  is never stored or committed). The Base-Master-Game script carries **no
  speaker labels**, so every committed message answers with speaker `unknown`.
- **Calibration data**: the synthetic tuning split (95 samples) plus the
  captured fixture (3 samples).

## Decision

1. **A captured holdout lives in `internal/bench`** (`captured.go`, new CLI
   subcommand `bench captured`). It verifies every fixture frame against the
   manifest's per-frame SHA-256 before running, builds the matcher index from
   the run-time-imported script, drives the same real pipeline (Tesseract,
   tracker, matcher, session) over the fixture's frames with oracle-driven
   region presence, and reports the docs/spec/12 metrics **separately** from
   the synthetic holdout, labelled class (b) captured-frame replay in the
   report and manifest. `make bench` runs both holdouts and prints **NOT RUN**
   with the two setup steps (fixture dir, image path) when either is absent —
   never a pass.

2. **The sidecar answers each commit during the replay, not after it.** The
   session's speech lifecycle refuses a start whose occurrence the box no
   longer stands behind (`session/apply.go`, `unknown_occurrence`). The old
   sidecar answered all commits after the frame loop, so any captured sample
   whose segment contains its advance frame (its region is already `DISMISSED`)
   could never speak: `speech.started` was silently dropped and the
   speech-scheduling metrics (precision, duplicate, stale) were permanently
   n/a. Answering at the commit moment — the product's decision provider and
   TTS worker run concurrently with the capture; this is that, scripted —
   restored the metrics. The synthetic sidecar's numbers are unchanged by the
   move (n=48 for every speech metric, identical values), because its samples
   never drop region presence.

3. **Measured result on real frames** (this fixture; `make bench` run
   20261018T094941Z, tesseract 5.5.3, 3 samples):

   | metric | value | n | objective | met |
   |---|---|---|---|---|
   | recognition_recall | 50.0% | 2 | ≥95% (provisional) | **not_met** |
   | recognition_precision | 100.0% | 1 | — | informational |
   | ocr_cer / ocr_wer | 0.0% / 0.0% | 2 | — | informational |
   | top1_exact_accuracy | 100.0% | 1 | ≥95% (provisional) | met |
   | abstention_rate | 50.0% | 2 | — | informational |
   | named_speaker_accuracy | n/a | 0 | ≥95% (provisional) | n/a (no labels in BMG) |
   | positive_speech_precision | 100.0% | 1 | ≥99% (provisional) | met |
   | duplicate_speech | 0 | 1 | none (provisional) | met |
   | stale_speech_after_advance | 0 | 1 | zero (provisional) | met |
   | commit_latency p50/p95 | 4436 ms (bound 6663 ms) | 1 | — | resolution-limited |

4. **Target adjustment for the one not-met line (the M7 "ADR-adjusted with
   evidence" clause).** Line *b* appears in exactly **one** sampled frame.
   The tracker must observe a reveal and a stable prefix across observations
   before it reaches READY and commits; a single sampled frame can never
   commit, whatever the pipeline does. In game the line is on screen for
   seconds; the fixture's 2.1 s sampling happened to catch it in one frame.
   The miss is therefore a **sampling artifact of the fixture, not evidence
   that the pipeline fails on player-visible dialogue**. Adjustment: the
   ≥95% recall objective is graded over lines sampled in ≥2 frames (here 1/1
   = 100%); single-frame lines are reported as not_met as measured and the
   50% figure stands in the report. The adjustment expires when the recording
   resolves it: capture at ≥10 Hz (≥3 frames per line) with the fixture
   factory (ADR-0054) and re-bench.

5. **Calibration: NOT FITTED, T = 1 retained** (ADR-0035: a degenerate fit is
   a refusal, never a fitted T). Combined fit data, per source:

   | source | class | used | incorrect top-1 | skipped |
   |---|---|---|---|---|
   | bench-synthetic-001 (tuning split, 95 samples) | a | 92 | 0 | 3 |
   | windwaker-newgame (captured, 3 samples) | b | 2 | 0 | 1 |

   94 fit samples with **0 incorrect top-1** across both sources; the fit
   requires ≥50 samples and ≥10 incorrect top-1 examples (and refuses a
   boundary minimum). A 100%-correct candidate set leaves the NLL flat in T,
   so any fitted T would be a choice, not a measurement. **What the recording
   must provide to unblock the fit**: labelled frames where the matcher's
   top-1 genuinely errs — OCR-degraded lines (blur, shadow, partial reveals),
   near-duplicate message pairs, font/zoom variants — i.e. M6
   fixture-factory captures with per-frame labelled error data. The synthetic
   tuning set is already saturated at 100% top-1, so error data must come
   from real frames.

## Tradeoffs

- The captured holdout's n is small (3 samples). It is honest evidence of the
  pipeline's *behaviour* on real frames (OCR fidelity, commit correctness,
  speech scheduling), not statistical strength; the report says so in its n
  columns.
- The sidecar timing fix changes both holdouts' speech path; the synthetic
  numbers are verified unchanged before and after.
- Named-speaker accuracy is n/a by construction of the BMG script (no
  speaker labels); it stays n/a until the recording or a labelled script
  provides ground truth.
- The single-frame recall adjustment (item 4) is the only objective modified
  in this ADR, and it is scoped to lines sampled in <2 frames.

## Validation

- `gofmt -l internal/bench` (empty), `go vet ./internal/bench/`,
  `go test -race -count=1 ./internal/bench/` — ok (fixture load/SHA checks,
  segment construction, truth mapping, Levenshtein, NOT RUN contract,
  end-to-end captured run on the fixture).
- `make bench` with the fixture dir and image configured: synthetic holdout
  (49 samples) — recall 100% (n=48), top-1 100% (n=48), named speaker 100%
  (n=34), speech precision 100% (n=48), duplicate 0 (n=48), stale 0 (n=48),
  commit latency p50 200 ms (n=48, resolution-limited at 10 Hz); captured
  holdout — the table in item 3.
- `make bench` without the fixture dir or image: both NOT RUN branches print
  the setup steps (verified with the env vars unset).
- `bench calibrate` over the combined sources: prints
  `NOT FITTED: … 94 tuning samples with 0 incorrect top-1 …` with the
  per-source breakdown and writes no calibration.json.

## Consequences and reversal

- `make bench` now covers both provenance classes; M7's gate evidence is the
  two reports plus this ADR's adjustment and the calibration refusal.
- The calibration refusal is a live target: the unblock plan is a number (≥10
  labelled incorrect top-1 examples from real frames), owned by the M6
  fixture-factory recording.
- Reversal: delete the `bench captured`/`bench calibrate --fixture` surfaces,
  `internal/bench/captured.go`, and the Makefile's captured block, and restore
  the sidecar's end-of-replay answer loop; the synthetic holdout and its
  metrics are unaffected by either.
