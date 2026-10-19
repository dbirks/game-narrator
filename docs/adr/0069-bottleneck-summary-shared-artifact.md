# ADR-0069: The bottleneck summary is one shared function, surfaced as a flagged artifact — named on every surface, graded nowhere

Date: 2026-10-19
Status: accepted
Owners: agent worker (diagnostics expose bottlenecks)
Related requirements/tests: docs/spec/16 M7 ("diagnostics expose bottlenecks", acceptance gap 9), docs/spec/13 ("Diagnostics": the support bundle is the diagnostics export), docs/spec/09 (developer inspector), ADR-0064 (wall-clock stage intervals are host-load dependent: the bench's verdicts stay load-invariant, so a load-dependent diagnostic may live only as a flagged figure), ADR-0024 (developer evidence is developer-only), `internal/runtime/bottleneck.go`, `internal/api` (`/api/v1/dev/sessions/{id}/bottlenecks`), `internal/bench` (report + `testdata/bench/latency-bounds.json`), `internal/soak` (report), `internal/support` (bundle `bottlenecks.json`), `web` (developer page block)

## Context and constraints

The bench, the soak and the session dev traces each carry per-stage timings
(capture → OCR, OCR → rank, capture → committed) from the pipeline's own
bucketed stage histograms. M7 asks that bottlenecks be a visible, tested
artifact: a per-stage latency summary (p50/p95/max per stage, share of the
end-to-end) that names the slowest stage, computed from the run's own
events/stats — not re-derived per surface.

These intervals are wall-clock (ADR-0064): they move with host load, exactly
like the bench's duration figure, which the bench already reports without
grading on. Whatever we build must therefore *report and name*, not *grade*:
a slow stage is evidence about a host or a run, never a regression verdict on
the recognition itself.

## Options

1. **Per-surface summaries.** Each surface (bench report, soak report,
   developer API, support bundle, web page) reduces the stage stats with its
   own code. Cheap to start; the five copies of "p95 and a share" drift, and
   "which stage is slowest" could legitimately differ between the page and
   the bundle for the same run — the exact confusion the artifact is meant
   to remove.
2. **One shared reducer, flagged where it meets a number.**
   `runtime.Bottlenecks(pipeline.Latency) → BottleneckSummary` is the single
   call: three rows in pipeline order (end-to-end last, as the baseline the
   shares divide by), p50/p95 as the histograms' own bucket edges (honest
   quantization, never an invented exact value), the named slowest proper
   sub-stage (the end-to-end stage is the baseline, never a candidate — it
   contains both sub-stages and would otherwise always win). Every surface
   renders that one summary, and the bench pairs it with the recorded-bounds
   file as a flag.

## Decision

Option 2.

- `runtime.Bottlenecks` is the shared function; the unit test
  `TestBottlenecksNamesTheSlowOCRStage` is the load-bearing assertion (a
  synthetic stream whose OCR stage is slow must read "the bottleneck is
  OCR").
- The bench and soak reports render the summary and name the slowest stage.
  The bench additionally checks the run's merged per-stage p95s against
  `testdata/bench/latency-bounds.json` and *names* any exceeded stage in the
  report's `latency_bounds` section — a flag with headroom (recorded as two
  times the worst observed p95 edge across the measured provenance runs),
  never a gate: it cannot change a capture-clock metric or a verdict.
- The developer API answers `GET /api/v1/dev/sessions/{id}/bottlenecks`
  (developer-only, like the other dev inspection routes) from the session's
  own stats; a still-running session answers with its stats so far, which is
  usually an empty summary the view says honestly.
- The support bundle carries `bottlenecks.json`: the shared summary for the
  most recent finished sessions, read from their persisted terminal-event
  stats, stage names/counts/latencies only (no trace rows, no game text).
- The developer page shows the same rows for the settled session and names
  the slowest stage; the Playwright developer spec asserts the block agrees
  with the API it was read from.

## Tradeoffs

- Histogram edges are quantized (the pipeline buckets by design), so p50/p95
  are bucket edges and a sub-millisecond stage's share can read above 100%.
  We report the edge and the ratio as measured instead of clamping a
  measurement to a prettier number; the surfaces say so.
- The bounds file is host-class evidence, not a gate: a quiet host may flag
  a noisy one and vice versa. That is the point — the flag carries the
  provenance note and tells the engineer what to do (fix or re-record with
  evidence), while the verdicts stay load-invariant per ADR-0064.
- One more bundle entry and one more dev route widen the developer-only
  surface; both are read-only over the run's own stats and carry no content
  beyond stage names, counts and latencies.

## Validation

- `go test ./internal/runtime` (the slow-OCR/slow-rank naming tests),
  `./internal/bench` (the bounds flag tests, the report-naming test and the
  end-to-end bench run asserting any exceeded stage is self-consistent and
  named in report.md — flagged, not gated: ADR-0064), `./internal/soak`
  (report names the slowest stage, JSON carries the summary),
  `./internal/api` (`dev_bottlenecks_test.go`: developer-only 403s, the
  live-session view), `./internal/support` (the bundle's bottlenecks.json
  names OCR from a seeded terminal event and never copies a trace-row hash;
  the content guard lists the new entry).
- Web: `pnpm typecheck` (the generated contract carries the route), the
  Playwright developer spec asserts the visible block and its agreement with
  the API over a real replay session.
- Measured provenance for the bounds file: synthetic holdout, 49 samples,
  single-threaded Tesseract, quiet-host runs 20261019T095914Z (frame_to_ocr
  p95 edge 400 ms), 20261019T100751Z (600 ms) and 20261019T102937Z (400 ms;
  frame_to_committed 50 ms in all three; ocr_to_ranked unsampled — recorded
  as a 100 ms floor bound). A loaded host once measured 1000 ms on
  frame_to_committed under concurrent test load: the flag names that, the
  verdicts do not move (ADR-0064).

## Consequences

Every bottleneck reading in the product is one arithmetic — the page, the
API, the bundle and the reports cannot disagree about a run's slow stage.
The bounds flag is a standing piece of evidence: re-recording it is an
evidence act (a measured run plus its note), not a threshold tweak.

## Reversal plan

Delete the route, the bundle section and the page block; keep
`runtime.Bottlenecks` (the bench/soak summary is still useful standalone).
The bounds file is inert data — removing it restores the pre-ADR behavior
(no latency_bounds section). No schema change was made; nothing migrates.
