# ADR-0157: Abandon a blocked onboarding run

- Status: accepted
- Date: 2026-10-22
- Issue: #45

## Context

The onboarding run (ADR-0025/0028) can stop on a human: the box asks a question,
it wants a source the curator must pick, or the provider answers with a fault.
The status line (`● agent blocked · …`) then holds forever because nothing lets the
operator end the run — and `POST /api/v1/onboarding/runs/{id}/cancel` with no attempt
in flight is a no-op that leaves the run `blocked`, i.e. resumable but never
finished. An operator who answers a question, picks a source or fixes the provider
outage has no button that says "stop"; the blocked bar is the answer they got, and
it stays.

## Decision

1. **A new terminal state.** `internal/agent` gains `StateAbandoned` (terminal,
   stop reason `StopReasonAbandoned`) alongside the existing states; the store
   vocabulary widens (migration 0019 rebuilds the `agent_runs.state` CHECK with
   `'abandoned'`) and the resume worklist refuses an abandoned run.
2. **An operator route.** `POST /api/v1/onboarding/runs/{id}/abandon`
   (curator through the onboarding prefix grant) answers:
   - **200** with the lifecycle answer (`run_id`, `state: "abandoned"`,
     `note`): the run was blocked/review/cancelled with no attempt in flight and
     is now terminal;
   - **409** `run-in-progress` when an attempt is in flight (the operator cancels
     the attempt first — `…/cancel` keeps the run resumable, it does not end it);
   - **409** `run-already-terminal` for a run already finished (succeeded/failed/
     abandoned) or otherwise not abandonable;
   - **404** for an unknown run.
   The stop is audit-logged as `onboarding.run.abandon`.
3. **The status line stops naming abandoned runs.** The abandoned state is
   terminal, so it leaves the active-run list (the status snapshot's `setup_runs`
   and the Library / Health surfaces that read it) exactly like a finished run:
   the bar, the Library and the Health setup-runs tab stop listing it, and a
   blocked run the operator abandoned never renders `● agent blocked` again.
4. **The web face.** The Health screen's setup-runs tab shows a **Dismiss** button
   on each live run (blocked / review); clicking it arms an inline confirm, and the
   confirm step calls the new route through `onboardingApi.abandon`.

## Options considered

- **Let the agent loop stop itself.** No: the loop only stops on terminal
  outcomes it computes; the operator's "I give up on this setup" is not a model
  decision.
- **Extend `/cancel`.** Cancel means *pause and resume later*; an abandoned run is
  terminal and must not come back. The two routes stay separate.
- **Fold pre-game blocked runs into the Health tab.** Unnecessary: the status
  line already drops terminal states, and the operator is looking at the Health
  tab when the bar is stuck on blocked.

## Tradeoffs

- Abandoned is terminal and irreversible: the run's durable logs stay (they are
  the audit trail), but no resume can resurrect it. The operator's only exit is the
  abandon action.
- Cancelled runs keep their resumable meaning; abandon applies to the states a
  human can legitimately stop a setup on (blocked, review, cancelled).

## Validation

- `internal/store`: `TestAgentRunAbandonTransition` — a blocked run moves to the
  terminal abandoned state (the widened CHECK admits it; the stop reason is the
  operator's).
- `internal/api`: `TestOnboardingAbandon` — 404 unknown run, 403 viewer,
  200 blocked→abandoned, 409 in-flight (`run-in-progress`); `TestStatusExcludesAbandonedSetupRuns` — the
  abandoned run no longer appears in the status snapshot's setup runs.
- `internal/api/openapi`: the run-state enums (OnboardingRunState, SetupBoard) admit
  `abandoned`, and the generated web contract matches.
- Web: the Health setup-runs tab's Dismiss button (inline confirm) calls
  `onboardingApi.abandon`; the vitest suite passes.

## Reversal

Remove the route and the web button; the state and stop reason can stay (a store
that refuses an unknown state is the point of the CHECK) or the migration can be
edited before release — migrations are forward-only and additive here, and the
abandoned vocabulary is a strict widening.
