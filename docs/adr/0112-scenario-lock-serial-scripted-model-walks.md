# ADR-0112: The scenario lock — serialising the shared scripted-model gateway's walks at two workers

Date: 2026-10-20
Status: accepted
Owners: redesign-b3 (Health/Settings merge integration)
Related requirements/tests: docs/spec/12 ("UI" mode, the Playwright suite), AGENTS.md rules 2 and 3, `web/e2e/fake-model.mjs` (the scripted model gateway), `web/playwright.config.ts` (the "agent-dock" webServer entry names the contract), `web/e2e/onboarding.spec.ts`, `web/e2e/library.spec.ts`, `web/e2e/agent-dock.spec.ts`
Links/references: ADR-0028 (the onboarding agent), ADR-0099 (the agent dock's scripted provider boundary), ADR-0109 (the settings/theme page — its library in-setup seed uses the gateway's `stall` scenario)
Supersedes / superseded by: —

## Context

The Playwright suite's agent-facing specs point their servers' `agent_reasoning` role at one shared scripted model gateway (`web/e2e/fake-model.mjs`, the suite's `fakeModel` webServer). The gateway holds **one global scenario** (`import`, `webimport`, `malicious`, `stall`, `assistant`), and each spec sets it with `POST /_scenario` before its first chat request. The config names the contract in the agent-dock entry: *"Its scenario is global, so each spec sets its own before the first chat request and the one-worker run keeps the specs serial."*

Three projects consume the gateway: **onboarding** (three tests, three scenarios, one appliance), **library** (the in-setup seed's `stall` scenario) and **agent-dock** (`assistant`). The contract holds only while those walks run serially: at the config's `workers: 1` default, `fullyParallel: true` is inert and every spec serialises behind the single worker.

The Phase 1 gate runs the full suite with `--workers=2` (halving the wall time). Under two workers, two scenario walks run concurrently, and one walk's `setScenario` lands inside another walk's chat conversation: the gateway then answers the first walk's agent from the second walk's script. Observed in the post-merge run (2026-10-20, `--workers=2`): the onboarding **import** walk's timeline carried the **malicious**-scenario answer ("I fetched the page … I will not call any tool other than the registered ones … stopping here") — the malicious test of the same file had set the global scenario while the import walk's agent was mid-conversation — and the walk never called `import_user_asset`, so the test timed out on its 90 s tool-timeline assertion. The same hazard applies to any interleaving of the library's `stall` seed with an onboarding walk, and of agent-dock's `assistant` set with any of them.

A secondary consequence of the same parallelism: the onboarding tests share the run's single appliance (the claim is once per appliance, ADR-0016 §7), so two of them starting on an unclaimed appliance contest the claim.

## Options

1. **Run the suite at one worker (the status quo the contract assumes).** Rejected: the Phase 1 gate names `--workers=2`; the suite's wall time roughly doubles, and the one-worker contract is exactly what this gate is meant to retire — keeping it means every future shared-stub spec must assume serial execution.
2. **One gateway instance per consuming project (distinct `fakeModel` ports per project).** Rejected: it fixes the cross-project hazard only — the onboarding file's three tests still share their project's gateway and race among themselves at two workers (the observed failure was intra-file), and it triples the shared machinery's surface (three more webServer entries, three more ports, per-project model names) for a fix that is still per-project, not per-walk.
3. **Derive the scenario per chat request from the conversation, dropping the global switch.** Rejected: the `import` and `stall` scenarios are conversationally identical up to the quality report (the stall is the import happy path with a deliberate loop afterwards), so the gateway cannot tell them apart from the messages the appliance sends; prompt-shape inference would silently break whenever a spec's seed prompt changes, and it re-implements the specs' intent inside the stub — a second source of truth for what each walk scripts.
4. **A scenario lock that serialises the scenario-dependent walks across the run's workers.** Accepted: the smallest change that restores the contract's invariant (one scenario walk at a time) at any worker count, without touching the gateway, the servers or the product.

## Decision

`web/e2e/support/scenario-lock.ts` provides `acquireScenarioLock(testInfo, scenario)`, a per-run lock:

- **The lock file** lives under the run's shared root (`<os.tmpdir()>/<GN_E2E_RUN_ROOT>/.scenario-lock.json` — the same root serve.mjs's manifests and the specs' data dirs live in, ADR-0094): the config sets `GN_E2E_RUN_ROOT` once per runner process and every worker and server of the run inherits it, so the file is unique per Playwright run and visible to every worker — the run's output dir, by contrast, is re-derived per process (one `run-<pid>` dir per runner *and* worker) and a lock there serialises only within a single worker (that was the first cut of this fix; it left the cross-worker hazard in place). Two concurrent runs in the same checkout carry distinct run roots — and thus distinct gateways — so they never contend on each other's lock. The claim is an exclusive open (`O_EXCL`); the file records the holder's pid, scenario, test title, acquire time and expiry for a failed run's evidence.
- **The lease** is 660 s — larger than the scenario walks' longest test timeout (the library walk's 600 s) — so a crashed or timed-out holder unblocks the next walk by expiry instead of dead-locking the run; an unreadable lock is treated as a crashed writer and re-claimed.
- **The wait** is 150 s (against the walks' seconds-to-minutes real durations); an acquirer that exhausts it fails its test with the lock's path and holder record — a red run with evidence, never a wrong-scenario walk.
- **The scope** is the whole test for the five scenario walks (onboarding ×3, library ×1, agent-dock ×1): each acquires at the test's start and releases in a `finally` at the test's end. Holding from the start also serialises the onboarding file's appliance claim (once per appliance, ADR-0016 §7), which the same parallelism contests on an unclaimed appliance (observed: a walk's claim submit losing to a sibling worker's, leaving it on the claim form).

The gateway, its `_scenario` control endpoint, the servers' wiring and every spec's assertions are unchanged; the specs' "the one-worker run keeps the specs serial" comments are updated to name the lock.

## Consequences

- The five scenario walks no longer parallelise with each other (worst-case added suite time: the sum of their mutual queue waits, bounded by one holder's walk at a time — seconds to a minute each at two workers); every other spec keeps full parallelism.
- A scenario walk that fails or times out leaves its lease in place for up to 660 s; the next walk waits it out (150 s) and then fails loudly with the holder record — a degraded but honest failure mode, never silent cross-talk.
- The lock is test machinery only: no product code, protocol, persistence or provider boundary is touched. AGENTS.md rule 3 stands as before: the gateway is a labelled synthetic stub, and this ADR changes only when its walks may interleave.
- Future specs that point a server at the shared gateway must acquire the lock around their scenario walk; the config's per-server comments and this ADR are the reference.

## Validation

- The post-merge `--workers=2` run that motivated this ADR is the reproduction: the onboarding import walk failed with the malicious scenario's text in its timeline and no `import_user_asset` call (evidence: the run's `error-context.md` page snapshot); the audio-output tone assertion failed in the same run under the same load (server answered the tone in 1 s; the page's 5 s assertion budget lost to box-wide contention — see the run report).
- After the lock landed: the onboarding, library and agent-dock projects run green under `--workers=2` in the full suite (this branch's Phase 1 gate), and the ksearch ⌘K walk — the merge's deterministic failure (the ADR-0111 `/health` rename: the walk's `/health` stop settles on the Health screen's `health-page` testid, and the health page's 409 `capture-signal-unavailable` signal read is allowlisted on the walk's console-cleanliness list, the machine's own answer, ADR-0110 §1) — is green in the same run.
- Unit/type: `web` typecheck, lint and format gates pass with the new helper and the five spec edits.

## Reversal plan

Delete `web/e2e/support/scenario-lock.ts`, the five tests' `acquireScenarioLock`/`try`/`finally` wraps and their `testInfo` parameters, and restore the "one-worker run keeps the specs serial" comments; run the suite at `workers: 1` (the config's default). The gateway and the product are untouched by this decision, so nothing else reverts.
