# ADR-0166: The approach step — the play loop's pixel-only walk to the interaction arrow

- Status: accepted
- Date: 2026-10-22
- Issue: #55 (narrowed: vision-nav)
- Owners: agent worker (vision-nav task)

## Context and constraints

The play loop's route mode (ADR-0162) drives stick holds, A presses, waits and
segment marks through the runner. The starter route (`testdata/routes/outset-deck-walk.json`)
hand-tunes stick vectors, and the proven arrow-driven approach lived only in the
telescope fixture test (`telescope_test.go` → `runApproach`), so a route that
needs to reach a character other than the savestate's landing position had no
scripted input path to do it.

## Decision

Add the route step kind `{"approach": true, "max_steps": N}` (`RouteStep.Approach` /
`MaxSteps`, JSON `approach` / `max_steps`), turned by `RouteStep.Step()` into the
runner's own `Step{Approach: true, MaxSteps: N}`; the runner executes it via
`runApproachStep`:

1. screenshot → `oliveArrowClusters` (the LARGEST qualifying cluster);
2. if the cluster's centre x is off-centre by more than the 0.08 alignment floor, hold
   the stick that way for 300 ms, else hold forward for 400 ms;
3. stop after `max_steps`, or when the arrow cluster's area stops growing for three
   consecutive samples.

No vision model: the arrow's own pixels are the whole signal (the detector moved to
`internal/dolphin/arrow.go`); the step is pixel-only and deterministic.

## Options considered

- Hand-tuned stick vectors in the route JSON. Rejected: the telescope's measured
  approach was the proven map, and the deck's sunlit-cluster false positives made
  size alone an unreliable movement signal.
- The vision model's walk direction per iteration. Rejected for this step (narrowed
  scope): the pixel-only loop is deterministic and synthetic-frame-testable; the
  model's tiebreak remains in the telescope's approach.
- A second input path / driver for approaches. Rejected (ADR-0162): the route adds no
  second input protocol or surface.

## Consequences

- Positive: a route can walk to a character without hand-tuned vectors or vision
  asks; the approach is unit-tested on synthetic olive-blob frames.
- Negative: the approach ends on stall or `max_steps` and does not itself confirm
  the SPEAK label (that judgment stays in the telescope's approach / the route's A
  press).

## Evidence and test plan

- Unit (offline): `go vet ./internal/dolphin/`, `go test -timeout 10m ./internal/dolphin/`
  (new: `TestApproachStepHoldsTowardTheOliveArrow` — left/right/at-centre olive
  blobs choose the stick; `TestApproachStepStopsWhenAreaStopsGrowing`; `TestParseRouteApproachStep`).

## Rollback/migration path

Revert the route step kind, the runner branch and the detector move; no persistence,
protocol or schema change is involved.

## Links/references

- ADR-0162 (play loop routes), ADR-0161 (vision capability), ADR-0125 (telescope
  re-speak and blocked gate), ADR-0065 (live-gate marker).
- `internal/dolphin/arrow.go`, `internal/dolphin/route.go`, `internal/dolphin/runner.go`,
  `internal/dolphin/script.go`, `internal/dolphin/approach_test.go`.

Supersedes / superseded by: ADR-0162's "starter route" consequence (the vision-guided
approach must be promoted into a route step) is discharged by this ADR's step.
