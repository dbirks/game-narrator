# ADR-0130: The setup-board shared-run completion barrier at two workers

Date: 2026-10-20
Status: accepted
Owners: redesign-p2 (Plum & Peach Phase 2 polish)
Related requirements/tests: docs/spec/12 ("UI" mode, the Playwright suite), ADR-0120 (the scenario lock), `web/e2e/setup-board.spec.ts`, `web/e2e/support/scenario-lock.ts`, `web/playwright.config.ts`
Links/references: ADR-0093 (the merged Cast seam the board's completion lands on), ADR-0108 (the board's snapshot ≡ replay contract, which the deep link depends on), ADR-0120 (the shared-gateway scenario lock this reuses)
Supersedes / superseded by: —

## Context

`web/e2e/setup-board.spec.ts` runs three tests against one server, one data directory and **one run**: test (a) signs in and starts the run through the scripted setup gateway; tests (b) (prefers-reduced-motion) and (c) (the three design widths) deep-link to the run (`?run=<id>`), which is the snapshot-rebuild path ADR-0108 names (a fresh load cannot trust a stream it never saw). The run's id is handed off through a module variable with a file fallback under the run's shared root (`<os.tmpdir()>/<GN_E2E_RUN_ROOT>/setup-board.run-id`).

The Phase 2 gate runs the full suite with `--workers=2`, and the config's `fullyParallel: true` lets tests of one file interleave across workers. The 2026-10-20 full-suite run exposed the handoff's race: a deep-linking test started before the walk wrote the id and failed on the empty fast path ("the walk's run id never appeared"). The interim fix made the deep linkers **poll** the id file instead of failing on a single read.

Polling the id fixes the first half of the race only. The id file is written the moment the run exists — the walk is then still running (the cards still move, Marla is not yet done, the draft is not yet published). A deep link that lands mid-walk rebuilds the board from a snapshot that is still moving, and (b)/(c) fail on their `data-stage="done"` assertions with the default five-second budget: a red run that is a scheduling artefact, not a product fault. The same hazard applies to the appliance claim: (b)/(c) sign in on the walk's appliance, and under the parallelism they can reach the claim form while the walk is mid-claim.

## Options

1. **Keep the id-file poll as the whole barrier.** Rejected: it proves the walk *started*, not that it *finished*; the mid-walk deep link is a load-dependent flake that a longer poll timeout only moves.
2. **`test.describe.configure({ mode: "serial" })` on the file.** Rejected: it serialises the three tests (which is the semantics the barrier needs), but it is a second cross-worker barrier idiom in the same suite next to ADR-0120's lock, it carries no holder record (a failure names no one), and it does not surface the hazard's evidence — AGENTS.md rule 4 asks for the correlated trace, and the lock's holder file (pid, test title, expiry) is exactly that.
3. **A two-stage barrier on the existing ADR-0120 primitive.** Accepted: the id-file poll proves the walk started; the walk's **run lock** — held by (a) from its start to its end, released in a `finally` — proves it finished. ADR-0120's lease (660 s, larger than the walk's 420 s test timeout) bounds a crashed holder; its holder record names a late failure's cause. No new mechanism, no new file, no product change.

## Decision

- `acquireScenarioLock(testInfo, scenario, timeoutMs = WAIT_MS)` takes an optional wait (the default keeps the five scenario walks' 150 s contract).
- Test (a) acquires the run lock (`scenario: "setup-board-run"`) before the appliance claim and releases it in a `finally`, so a failed or timed-out walk cannot pin the barrier past its lease.
- Tests (b) and (c) take the two-stage barrier: (1) poll the run id (the shared contract for *start*), then (2) acquire and immediately release the run lock (the shared contract for *completion*), then deep-link. Their test timeouts cover the barrier wait: the wait is the walk's 420 s test timeout plus a 30 s scheduling margin, so a walk that legitimately runs the full length never trips it.
- The deep-link views are read-only and may run together, so (b)/(c) treat the lock as a barrier (acquire + immediate release), not a claim.

## Consequences

- The three setup-board tests are correct at any worker count: a deep link only ever rebuilds a settled run, and the appliance claim is serialised behind the walk.
- A failed walk now also fails its deep-linkers, with a 450 s wait and the holder record naming the walk's pid and title — a louder, evidence-carrying failure than a mid-run flake, at the cost of the wait on the failure path only.
- The id file and its poll stay (they are the *start* contract and the cross-worker fast path); the lock is the *completion* contract.
- This is test machinery only: no product code, protocol, persistence or provider boundary is touched. AGENTS.md rule 3 stands as before.

## Validation

- The reproduction is the 2026-10-20 `--workers=2` run: the deep-linkers' "the walk's run id never appeared" failure (the empty fast path), and the mid-walk deep link's five-second `data-stage` miss under load.
- After the barrier landed: `GN_E2E_PORT_BASE=auto playwright test e2e/setup-board.spec.ts --workers=2` — 3 passed in 32.0 s, with the walk (13.8 s) and the reduced-motion test (15.2 s) running concurrently on two workers: the second waited on the walk's lock, then deep-linked the settled run.

## Addendum (2026-10-21): the same barrier for the cast walk's widths test

The first full-suite `--workers=2` gate run (2026-10-21) found the same hazard in `web/e2e/cast.spec.ts`: the walk and its "screenshots and axe at the three widths" test share the voices server, and the widths test runs on the walk's game **and approved take** — a state that exists only once the walk's audition has been approved. `fullyParallel` let the widths test start on a sibling worker mid-walk: its page loaded with "no voice for Boy yet" (the walk's take still generating), and the take player never rendered, so the visibility wait failed after its 60 s budget — a scheduling artefact, not a product fault (the error-context snapshot names the state).

- The walk now holds the scenario lock (`scenario: "cast-walk"`) from before its appliance claim to its end, released in a `finally` (the same shape as setup-board test (a)).
- The widths test acquires and immediately releases the lock before it signs in (the completion barrier; it is read-only over the walk's state, so a barrier, not a claim), with a wait of the walk's 600 s test timeout plus a 30 s margin; its own timeout covers the barrier plus its work.
- No new mechanism, no product change: the ADR-0120 primitive, this ADR's decision.
- Validated by the 2026-10-21 full-suite gate re-run (this commit).

The same run also found the health walk's appliance claim asserted the first claim's 201 strictly, while its two sibling tests on the same server already accepted the already-claimed 409: at two workers a sibling's claim can land first. The walk now accepts `[201, 409]` like its siblings (ADR-0016 §7: the claim is once per appliance; either way the admin exists for the sign-in).

A third instance of the same hazard, in the same run: `web/e2e/settings-theme.spec.ts`'s UX-12 and widths tests sign in on the UX-13 walk's appliance, and the widths test also reads the walk's theme state — at two workers UX-12 reached `/login` before the walk's claim landed, the page rendered the first-run setup form, and the login fields never appeared. The UX-13 walk now holds the scenario lock (`scenario: "settings-theme-walk"`) from its start to its end (finally); the two dependents take it as a completion barrier before they sign in (wait: the walk's 600 s test timeout + 30 s margin; their timeouts cover the barrier plus their work).

## Reversal plan

Remove the `acquireScenarioLock`/`finally` from test (a), the two-stage barrier from (b)/(c) and the `timeoutMs` parameter from `acquireScenarioLock`; restore the deep-linkers' original timeouts. The suite at `workers: 1` (the config default) makes the handoff serial again, so nothing else reverts.
