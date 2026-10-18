# ADR-0064: Bench replay is load-invariant — no per-sample deadlines, bounded workers, pinned engine threads, streamed degraded frames

Date: 2026-10-18
Status: accepted
Owners: agent worker (bench determinism)
Related requirements/tests: docs/spec/12 ("Frame replay": a replay's committed messages and their capture timestamps are identical run to run, on a fast box or a slow one), ADR-0037 (capture clock is the clock; scenario timing is a property of the recording), ADR-0054 (fixture factory and captured-replay gate), ADR-0058 (captured-Dolphin bench holdout and calibration verdict; its amendment 1 introduced the dense fixture and the 8-worker scheduler), ADR-0035 (calibration: a refusal is a verdict, never a T), AGENTS.md rule 3 (no simulation theater: class (b) replay verdicts are load-invariant evidence), `internal/bench` (run/captured/calibrate/degrade), `cmd/game-narrator/bench.go`, Makefile `bench` recipe

## Context and constraints

The captured-frame benchmark is class (b) captured-frame replay: its verdict
(recall, top-1, the sample count, the calibration counts) is the M7 gate
evidence, and per docs/spec/12 "Frame replay" it must be identical run to run
— on a fast box or a slow one. ModeReplay already guarantees the pipeline
half: lossless queue, per-region OCR strictly sequential in frame order, a
per-call deadline with a bounded retry policy (3 × 30 s), and a capture-clock
tracker. Tesseract itself is deterministic on a given crop (measured:
byte-identical output across repeated runs, single- or multi-threaded).

The harness half was not load-invariant. Two `make bench` runs of the same
fixture `windwaker-legend-dense` (1085 frames, 20 samples; the last sample is
680 frames and makes 627 OCR calls because the legend screen's background
changes nearly every frame) diverged:

- Run A (quieter host): 20/20 samples ran; recall 19/19 = 100%, top-1 17/18 = 94.4%.
- Run B (host with concurrent agent load): 18/20 samples; two small samples
  (12–14 frames each) came back `NOT RUN: pipeline run: context deadline
  exceeded`; recall 70.6% (n=17), top-1 90.9% (n=11). Same fixture, same
  script, different verdict.

Measured root causes, all on the bench host (8 cores, shared machine):

1. **Self-oversubscription.** 8 parallel sample workers, each with
   single-threaded-per-call Tesseract assumed — but Tesseract 5.5.3 spawns an
   OpenMP pool per call (measured 181% of one core on a subtitle crop).
   Eight concurrent samples ran ~16+ engine threads plus 8 Go runtimes; the
   bench's own load measured 8–12, and per-call OCR latency inflated from
   ~150 ms to several seconds.
2. **A load-sensitive per-sample wall deadline.** `runSample` bounded each
   sample at `max(120 s, 5 s × frames)`. Under the self-contention above, a
   12-frame sample whose ~10 OCR calls each inflate to >10 s outlives the
   120 s floor → NOT RUN → excluded from the metrics → n and every metric
   change. The deadline is load-dependent by construction: any wall-clock
   budget inside the replay is a timer that host load can trip.
3. **A fixed 30-minute whole-run deadline** shared by all workers — the same
   class of problem at the run scale, and un-scaled to the fixture.
4. **Disk-quota failure of the calibration fit.** The class b′ degraded
   variants (6 × 1085 JPEGs for the dense fixture, ~2.5 GB) were prepared up
   front in the shared 7.8 GB `/tmp` tmpfs; with other tenants present the
   render failed mid-run with `disk quota exceeded` and the whole fit died —
   a gate failure caused by the bench's own unbounded temp footprint.

Constraints: the verdict must not change with host load (docs/spec/12); the
pipeline's own deadlines (docs/spec/01) are unchanged and remain the wedge
guard; the bench must stay bounded (a wedged engine must not hang the gate);
the class b′ frames are "generated at bench time, never stored"
(ADR-0058 amendment 1); scope is `internal/bench`,
`cmd/game-narrator/bench.go` and the Makefile `bench` recipe.

## Decision

1. **No per-sample wall deadline.** `runSample` runs the pipeline under the
   caller's context only. The bounds that remain are the load-independent
   ones: the pipeline's per-OCR-call deadline and retry budget (ModeReplay —
   a wedged engine is killed per call, so a sample cannot hang) and the
   run-level bound below. Nothing inside the replay is timed against wall
   clock.
2. **The run-level bound is the only patience bound, scaled to the fixture.**
   `bench.ReplayRunBound(frames)` = max(30 min, 10 s × frames): 30 minutes
   for the small fixtures, ~3 h for the dense fixture. Measured per-frame
   cost is ~0.14 s on the dense fixture, so realistic runs sit ~70× under
   the bound; the bound is a wedge guard against a pathologically wedged
   run, not a load timer. `bench captured` sizes it from the fixture's frame
   count (the SHA-256 verification gate runs up front); `bench calibrate`
   sizes it over the synthetic floor plus each fixture's frame count
   multiplied by its source count (1 clean + 6 degraded variants).
3. **Bounded parallelism + pinned engine threads.** The bench's worker pool
   is `min(4, NumCPU/2, samples)` (was 8) — each sample's hot path is one
   Tesseract call at a time (ModeReplay is sequential per region), so a
   handful of concurrent samples uses the box without saturating it, leaving
   headroom for the concurrent load a shared host always has. The bench
   process pins `OMP_NUM_THREADS=1` (inherited by the Tesseract children;
   also set on the Makefile recipe's invocations): the engine's thread pool
   no longer multiplies the bench's own load, and single-threaded is the
   deterministic engine configuration (same crop, same threads, same
   answer). The worker count and thread count are scheduler details:
   per-frame OCR is deterministic and ModeReplay is sequential per region,
   so results do not depend on either.
4. **The calibration fit streams its sources.** Each fit source now carries
   `prepare`/`cleanup`: the class b′ variant's frame dir is rendered right
   before the source runs and freed right after (the synthetic and clean
   captured sources clean up the same way). Disk peak is one source's frames
   (~400 MB for a dense variant), not all sources' (~2.5 GB). A render
   failure still NOT RUNs the whole fit (the prepare error refuses), the
   same discipline as the synthetic render failure.

## Options considered

- **Scale the per-sample deadline more aggressively** (e.g. 90 s/frame, the
  full retry budget). Rejected: it still is a wall-clock budget inside the
  replay, so it is load-sensitive by construction — a slow-enough host
  outlives it, the sample is lost to NOT RUN, and the verdict changes. The
  retry budget already bounds a wedged engine per call; the sample level
  needs no wall timer at all.
- **Keep 8 workers and rely on the retry policy.** Rejected: the retry
  policy (3 × 30 s) preserves *results* under load, but 8 self-oversubscribed
  workers push the run's own wall time toward the run-level bound and turn a
  quiet host into the load. The verdict would stay deterministic while the
  gate's wall time becomes fragile; 4 pinned workers keep both.
- **Point the bench's temp at a project-local dir (larger disk) instead of
  streaming.** Rejected as the primary fix: it papers over the unbounded
  footprint instead of bounding it, and a large-enough /tmp-less host
  (container, CI) still fails. Streaming bounds the footprint regardless of
  where temp lives; (it was also applied here, where /home has 99 GB and
  /tmp is a 7.8 GB shared tmpfs.)
- **Cache OCR across the degraded variants.** Rejected: the variants' pixels
  differ by construction; caching would cross provenance classes (AGENTS.md
  rule 3) and the OCR cost is the fit's honest price.

## Validation

- **Determinism, proven by diff.** The captured holdout was run twice
  directly (`bench captured` per fixture, built binary) on the shared host;
  `report.json` metric blocks compared with the latency fields excluded:
  identical for `windwaker-legend-dense` (20/20 samples both runs; recall
  19/19 = 100%, top-1 17/18 = 94.4%, one miss on cx0014, 0.6912) and
  `windwaker-newgame`. `diagnostics.sample_ids`, `samples` and `not_run`
  are identical between runs; `make bench`'s own dense report matches the
  same metric block (per-run UUID message ids aside — ids are fresh per
  script import, correlation is within-run).
- **Gate.** `GAME_NARRATOR_FIXTURE_DIR=$PWD/artifacts/fixtures make bench`
  → rc 0: synthetic holdout (49 samples — recall 100% n=48, top-1 100%
  n=48, named speaker 100% n=34, unchanged from ADR-0058's verification);
  windwaker-newgame (3 samples; recall 50% n=2 not_met — the ADR-0058
  single-frame sampling artifact, reported as measured);
  windwaker-legend-dense (20 samples, 0 NOT RUN; recall 100% n=19 met,
  top-1 94.4% n=18 not_met); calibration NOT FITTED (184 tuning samples,
  0 incorrect top-1 — the refusal is a verdict, ADR-0035; exit 4 accepted by
  the gate). Dense bench wall 97.9 s, process CPU 20.6 s, peak RSS 108 MB.
- **Tests.** `gofmt -l internal cmd` clean; `go vet ./internal/bench/
  ./cmd/...` clean; `go test -race -count=1 ./internal/bench/` ok
  (segmentation, truth mapping, NOT RUN contract, end-to-end captured run,
  degraded-source shape — updated for the prepare/cleanup streaming — and
  the calibration refusal).
- **Load behaviour.** The pre-fix failure reproduced three times on the
  (quiet) host: 8 workers pushed load to 8–12 and 1–2 small samples per run
  exceeded the 120 s per-sample floor. Post-fix: 0 NOT RUN across all runs
  above; per-call OCR p50 150–250 ms, max 3 s on the quiet host — 10–200×
  inside the per-call retry budget, so realistic load cannot flip a frame
  degraded, and a degenerate frame (if one ever happens) is counted
  (replay_degraded_frames), never silently re-graded.

## Consequences and reversal

- The bench's verdicts are now load-invariant: NOT RUN can come only from a
  missing engine/fixture/image (the honest environment refusals) or from a
  run so slow it outruns the scaled run-level bound — a wedge guard that no
  realistic load approaches, whose firing would be evidence of a wedged
  engine, reported as such.
- `make bench` wall time on the 8-core host with both fixtures configured is
  ~30 min (synthetic ~3 min, newgame seconds, dense ~2 min, calibration
  ~23 min — the fit replays the dense fixture 7 times). That is the honest
  cost of the class b′ evidence; the test-full 5-min budget applies to the
  synthetic part, which is unchanged.
- Tesseract runs single-threaded under the bench everywhere
  (synthetic holdout included): per-call time is ~equal or faster on the
  small subtitle crops (measured 122 ms vs 150 ms multi-threaded), and the
  engine's configuration is now part of the replay's determinism story.
- The calibration fit's per-source `prepare`/`cleanup` hooks are the
  extension point for future sources (a new source type is a fitSource with
  its own lazy frame preparation).
- Reversal: restore the per-sample `max(120 s, 5 s × frames)` budget in
  `runSample`, the fixed 30-minute run contexts in `cmd/game-narrator/
  bench.go`, the 8-worker pools in `captured.go`/`calibrate.go`/`report.go`,
  the eager `degradedFitSources` (3-value return, no prepare/cleanup), and
  drop the `OMP_NUM_THREADS` pins — returning the bench to the load-sensitive
  scheduler documented in the Context. Nothing outside `internal/bench`,
  `cmd/game-narrator/bench.go` and the Makefile `bench` recipe is touched.
