# ADR-0096: Live OCR within the frame budget on the UNO Q — one read at a time, coalescing, fallback on a dead context

Date: 2026-10-20
Status: accepted
Owners: issue8-arm-ocr worker
Related requirements/tests: GitHub issue #8 (live OCR on the UNO Q must finish
within the frame budget), docs/spec/05 (OCR pipeline, 4 s deadline, "compare
lightweight OCR against an optional VLM fallback"), docs/spec/01 (live
semantics: latest-frame queue, bounded deadlines), docs/spec/13 (backpressure
and memory ceilings), docs/spec/14 (privacy: crop only), ADR-0012 (Tesseract
wrapper contract), ADR-0040 (VLM fallback policy), ADR-0085 (one-shot `ocr`
command), docs/manuals/18 ("On-device OCR latency" measurements); tests
`TestTesseractRunsRealImageThroughInputFile`,
`TestFallbackTimedOutPrimaryGetsFreshSecondaryContext`,
`TestLiveOCRNeverRunsMoreThanTheCap`,
`TestAgeOutDropsStaleObservationNotTheSharedRead`,
`TestHWOCRLatencyDevice`

## Context and constraints

The 2026-10-20 board run (issue #8) measured the live session on the UNO Q
(A53, 4 cores, 3.6 GB) end to end: `ocr_calls 3363 = ocr_cancelled 3363`, 0
observations, 0 rankings, 0 speech, `ocr_fallback_frames 0`. Tesseract
processes were observed dying every 0.5–1 s (the operator's ~40 s per-crop
report is the effective cost of a read that is killed mid-flight on every
frame), while the same crop read cleanly at ~1.1 s by hand.

Measurements on the board (this change, the pipeline's 1024×182 dialogue
crop, tesseract 5.5.0 / leptonica 1.84.1, 4.1 MB "fast" `eng`, the
`DefaultSteps()` recipe — 2× nearest scale, Otsu, median, pad — unless
stated; full table in docs/manuals/18):

| variant | wall time |
|---|---|
| engine call, tuned recipe (psm 6, scale 2, 2064×376) | **1.12–1.14 s** |
| — of which fixed startup (process + model load, 16×16 input) | **0.58–0.60 s** |
| — of which inference at scale 2 | ~0.54 s |
| psm 6, scale 1 / 3 | 1.00 s / 1.27 s |
| psm 7 / 13 at scale 1 (the sub-0.7 s settings) | 0.61 s / 0.67 s — **and they destroy the 3-line crop** (empty / garbled output) |
| OEM 1 / 3 / unset (5.x default: LSTM only) | 1.14 s — no effect |
| `OMP_NUM_THREADS=1` / `OMP_THREAD_LIMIT=1` | 1.14 s — no effect |
| stdin pipe vs file operand (same pre-encoded PNG) | 1.11–1.13 s vs 1.12–1.13 s — **no measurable difference** |
| **two concurrent reads, unpinned cores** | **19.0 s each (vs 1.13 s: 17×)** |
| two concurrent reads, pinned to disjoint core pairs | 2.14 s each |
| in-product one-shot `ocr --image <crop> --json` (this change, 3 runs) | `duration_ms` 1123 / 1136 / 1164; text correct (26 words, conf 0.852) |

Three causes of the total failure, each measured:

1. **The installed build supersede-cancelled the in-flight read on every
   changed-hash frame.** The dialogue box sits over an animated scene, so its
   crop hash changes every frame (~0.69 s apart at the 1.46 fps board
   source); a read killed 0.5–1 s after start dies inside its load/inference
   window and never produces a result. The effective cost per crop is
   unbounded and the process death interval is the observed 0.5–1 s.
2. **Two concurrent reads thrash each other 17–32×** (19.0 s unpinned, 2.14 s
   pinned, on the A53). The pipeline's default pool is two workers and the
   profile has two regions, so two Tesseract processes ran at once; every
   read outran the 4 s live deadline → `ocr_calls 3363 = ocr_cancelled 3363`.
   This is the 60× factor, measured.
3. **The VLM fallback could not fire on a cancelled or timed-out read**
   (ADR-0040's policy says an errored primary is unresolved and the
   secondary gets the look): the job context was already done when the
   primary failed, and the secondary was handed that dead context, where it
   failed before its first byte. `ocr_fallback_frames 0`.

Constraints: the hard OCR budget is the 4 s per-call deadline (docs/spec/05);
issue #8 additionally targets p95 < 700 ms for the dialogue crop on the
board; the appliance has 4 cores shared with capture, the Go runtime and
pipewire; the privacy boundary is crop-only (docs/spec/14); the release
target is pure Go (`CGO_ENABLED=0`); replay determinism (docs/spec/12) is
untouched.

## Decision

1. **Live mode bounds concurrent OCR processes; the default is one on a
   small board.** `pipeline.Config.MaxConcurrentOCR` bounds how many engine
   calls — and therefore engine child processes — run at once (clamped to
   `[1, Workers]`; `Workers` stays the jobs buffer and rank pool size). The
   auto-default (zero) is 1 for `ModeLive` on a machine with ≤ 4 cores
   (`smallBoardCPUs`) and `Workers` otherwise; replay keeps the pool width.
   The board measurement is the reason: two concurrent reads cost 19 s each
   there. An explicit value (or a bigger machine) overrides the default.
2. **A newer frame coalesces onto the in-flight read; only a genuinely
   superseded read is cancelled.** The live path already rides the in-flight
   read on a changed-hash frame (the docs/spec/12 "delayed model responses"
   fix, `dispatch.go accept`); this change closes the remaining cancellation
   hole: the ROI stage's probe-tick age-out (`ageOut`) dropped the head
   observation and killed the read with it, even when that read was the
   region's shared in-flight read the newer frames were riding on. Age-out
   now drops the stale observation and cancels the job only when nobody is
   riding it. Cancellation remains correct where the content changed
   materially: the box disappearing (`discardInflight`), queue-bound
   eviction of a job no longer shared, and shutdown. A read that outruns its
   head still settles and its riders resolve with its text; a settle with no
   riders books `lastOK` so the dedupe remembers the pixels.
3. **A cancelled or timed-out primary read is eligible for the VLM fallback
   on its own bound.** In `ocr.FallbackEngine`, when the primary returns an
   error and the caller's context is already done (the live deadline expired
   or the pipeline superseded the read), the secondary runs on a fresh
   context bounded by `FallbackPolicy.SecondaryTimeout` (new field, default
   15 s; the production VLM role client carries its own 10 s transport
   timeout, ADR-0040, so the effective bound is the smaller). The rescue is
   for an *unresolved* read only: a primary that answered resolved before the
   deadline passes through untouched and spends no rate-limit token. The
   decision rule, cache and rate limit of ADR-0040 are unchanged; the
   caller's fences (sequence/generation) still decide whether a late rescue
   is still wanted, and the rate limit (1/s, burst 3) plus the crop cache
   bound what repeated rescues cost.
4. **The Tesseract wrapper hands the crop to the engine as a file operand,
   not a stdin pipe.** The preprocessed crop is encoded as PNG (no
   compression — the same-lifetime 0600 temp file is the engine's private
   business, not an egress; encode measured at 1.4 ms on the board) into a
   fresh `os.CreateTemp` file in `os.TempDir()`, the argv becomes
   `tesseract <file> stdout --psm … -l eng … tsv`, and the file is removed on
   every exit path (a deferred `os.Remove`; `TempDir` is a test seam). The
   file is the only input change: pixels, argv flags, supervision (process
   group kill, bounded pipes, typed failures) and the 4 s deadline are
   unchanged. Measured on the board, file vs stdin input is the same speed
   (1.12 s either way); the change is contract hygiene, not a latency fix —
   the wrapper's stdin pipe is an extra blocking path that a kill mid-read
   can wedge (a dead child never closes the write end), and the 0600
   same-lifetime file matches ADR-0012's `<in> <out>` argv shape. On the
   release target `/tmp` is tmpfs (wiped on reboot); a file left by a killed
   process is a single crop — already the unit of hash and VLM egress
   (docs/spec/14) — and 0600-bounded.

## What the board measurement says the p95 target cannot buy

The p95 < 700 ms target for the dialogue crop **is not reachable on the A53
with a per-call engine process**, and this is now measured rather than
assumed: the fixed startup cost (process, leptonica, 4.1 MB model load, LSTM
init) is 0.58–0.60 s, and the tuned recipe's inference adds ~0.54 s
(1.13 s total; scale 1 is the fastest psm-6 variant at 1.00 s). The only
settings under 0.7 s (psm 7/13) are single-line modes and they destroy a
3-line crop (empty or garbled output, measured). The two ways to reach sub-700
ms are both outside this change's scope: a **persistent engine** (a long-
running process amortising the 0.59 s startup — a new component with its own
supervision and a C/cgo artifact against the pure-Go release rule) or a
**smaller/faster model** (no smaller `eng` ships in tessdata 4.1). Neither
was taken; the follow-up is recorded below. What this change does deliver on
the board: a healthy read settles in 1.13 s — inside the 4 s hard deadline
with 3.5× headroom (issue #8's frame-budget question is answered by
settle-and-coalesce: the read spans ~1.6 source frames and the riders
resolve with its text instead of the read dying every frame), at most one
engine process at a time, and a reachable VLM fallback for the genuinely
unresolved reads.

## Alternatives considered

- **Persistent Tesseract (daemon) to amortise model load.** The only path to
  sub-700 ms on the A53, measured. Rejected here: no such mode in stock
  Tesseract 5.5; a bespoke daemon is a new component with its own
  supervision, lifecycle and crash surface, and a C/cgo build artifact
  against the `CGO_ENABLED=0` release rule. Follow-up (below) with the
  measurement that would justify it.
- **Core-pinning the Tesseract child (`taskset`).** It cut the parallel
  penalty to 2.14 s but needs CPU knowledge the pipeline has no business
  owning, breaks under cgroup/cpuset changes, and the product-side answer
  (one read at a time on a small board) removes the contention entirely.
- **`OMP_NUM_THREADS=1` in the child environment.** Measured no effect
  (1.14 s either way; the LSTM path is single-threaded and the thrash is
  scheduler migration, not OMP spin). Not added.
- **Lowering the preprocess scale to 1×.** Measured 1.00 s vs 1.13 s — a
  0.13 s saving against the accuracy recipe the fixtures were tuned on
  (docs/spec/05, ADR-0082's crop work), still 43% over the target. Rejected.
- **psm 7/13 to skip multi-line processing.** Measured 0.61/0.67 s — under
  the target, and measured to destroy the 3-line crop (empty / garbled).
  Rejected on accuracy.
- **Firing the fallback only on timeouts, not on pipeline-issued cancels.**
  The task's policy reading (and ADR-0040's "an error … makes the primary
  unresolved") says both qualify; after decision 2 the cancels that remain
  (box gone, eviction, shutdown) are rare and the fences plus the rate limit
  bound any waste. Keeping one rule is simpler than a second taxonomy of
  "useful" cancellations.

## Consequences and risks

- Live reads on the board settle at 1.13 s (measured), inside the 4 s hard
  deadline; a healthy read now survives the frame race, and the coalescing
  means a box that changes every frame still perceives text at the engine's
  ~0.9 Hz pace instead of zero.
- One OCR child at a time on ≤ 4-core live sessions: a wedged engine now
  serially blocks the second region's read behind it (bounded by the 4 s
  deadline and the queue bound); on the board that is strictly better than
  two reads each taking 19 s.
- A 0600 crop file exists in `/tmp` for the duration of one engine call
  (sub-second to ~1 s); a killed process can leave it until reboot. The 0600
  mode, the unique `CreateTemp` name, the deferred remove and the tmpfs
  `/tmp` of the release target are the privacy answer (docs/spec/14); it is
  a single crop, not a full frame, and it never leaves the appliance except
  as an OCR input.
- The fresh-context rescue can bill one VLM call per distinct crop after a
  timeout/cancel (≤ 1/s, burst 3, cache-keyed) — the documented cost class
  of ADR-0040, now actually reachable instead of silently dead.
- `MaxConcurrentOCR` is a new Config field: existing callers (runtime,
  bench, scenario, soak) get the auto-default; nothing must change.
- Replay determinism is untouched: the cap's auto-default is the pool width
  in replay, and the age-out change only removes an early kill of a read the
  retry budget was still spending.

## Evidence and test plan

- Board measurements (2026-10-20/21, UNO Q, tesseract 5.5.0): the table
  above; the exact numbers and re-measurement recipe are recorded in
  docs/manuals/18 "On-device OCR latency".
- `TestTesseractRunsRealImageThroughInputFile` (internal/vision/ocr): the
  argv contract is now `<png-file> stdout --psm 6 -l eng tsv`, the file is a
  real PNG while the engine ran, and it is gone after the call returns.
- `TestLiveOCRNeverRunsMoreThanTheCap` (internal/pipeline): two regions both
  owed a read, the engine's calls held on a gate — at most one recognition
  runs at a time even though two jobs are ready, and both still settle.
- `TestAgeOutDropsStaleObservationNotTheSharedRead` (internal/pipeline): the
  probe-tick age-out drops the head observation while the shared in-flight
  read (an engine that ignores cancellation, the belt's raison d'être)
  settles and its riders' observations carry the text; `ocr_cancelled` stays
  0.
- `TestFallbackTimedOutPrimaryGetsFreshSecondaryContext` /
  `TestFallbackCancelledPrimaryGetsFreshSecondaryContext` /
  `TestFallbackResolvedPrimaryOnDoneContextDoesNotRescue` /
  `TestFallbackSecondaryTimeoutBindsTheFreshContext` (internal/vision/ocr):
  the rescue fires on a dead caller context with a bounded fresh context,
  and not for a resolved read.
- `TestHWOCRLatencyDevice` (internal/hwsmoke, `GN_GATE=test-hardware`): the
  one-shot `ocr --image` command over the controlled crop must read the text
  and finish its engine call inside 1.1 s on whatever hardware runs the gate
  (the measured A53 floor plus margin — the gate asserts the documented
  floor, not the unreachable 0.7 s, and the deviation is stated in the ADR).
- Full regression: `go test -race ./internal/vision/... ./internal/pipeline/...`
  (replay determinism suite included) — green.

## Migration / rollback

No data, protocol or schema change. Rollback is reverting the touched files
(`internal/vision/ocr/tesseract.go`, `internal/vision/ocr/fallback.go`,
`internal/pipeline/config.go`, `internal/pipeline/runner.go`,
`internal/pipeline/dispatch.go`) — the wrapper's stdin contract and the old
cancellation semantics come back exactly, and the new Config field is inert
(zero → auto-default). No migration needed in either direction; the
on-device binary is replaced by the next release.

## Follow-ups

- **Persistent engine (the sub-700 ms path).** Measured need: 0.59 s of the
  1.13 s is fixed startup. A long-running engine process (or an in-process
  engine via a Tesseract C binding, which breaks the pure-Go release rule)
  amortises it and would put the crop read at ~0.55 s — under the target.
  Decided against in this change; revisit with a new ADR if the board
  re-measurement after release shows perception lag still matters.
- Re-measure on the board after the release build: the live session should
  show `ocr_calls` with a high settle ratio, `ocr_cancelled` near zero,
  observations and rankings > 0, and (with the VLM role configured)
  `ocr_fallback_frames` > 0 only when the local read is genuinely
  unresolved.
- The ready probe's pure-Go integrity-check deadline on the A53 (board-run
  blocker 1, `internal/api`) is a separate issue and out of scope here.
