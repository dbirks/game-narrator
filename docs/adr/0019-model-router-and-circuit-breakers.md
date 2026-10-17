# ADR-0019: Model router — per-role circuit breakers, bounded retries with a generation guard, priority slots, and health

Date: 2026-10-17
Status: accepted
Owners: project
Related requirements/tests: docs/spec/06 "Interface requirements" (`ModelRouter.Resolve`), "Routing, reliability and replicas", "Timeout and cancellation defaults", "Failure taxonomy"; docs/spec/01 "Parallelism and deadline contracts" (per-call circuit breaker and returned-generation check), "Runtime and setup isolation" (live > background priority, background pause); docs/spec/13 resilience matrix rows "Decision/VLM down", "TTS down"; docs/spec/12 "Decision/provider" (provider 429/503, request late after session reset, per-role circuit breaker recovery); `go vet ./internal/providers/router/...`, `go test -race -count=3 ./internal/providers/router/...`; builds on ADR-0009 (roles + taxonomy), ADR-0013 (recorded cassettes), ADR-0021/0022 (System One and TTS clients)

## Context and constraints

- docs/spec/06 requires `ModelRouter.Resolve(role, affinity, capabilities) ->
  target, route_id, health_state`, role-based route tables with circuit
  breakers, and "retry idempotent inference calls with bounded exponential
  backoff **only if the event is still current**". docs/spec/01 adds a
  per-call circuit breaker **and returned-generation check** for
  decision/VLM calls, plus "runtime and setup isolation": live session work
  takes resources before background work, and background work is "rate
  limited or paused while a live session needs resources".
- ADR-0009 deliberately left retry/circuit policy out of the adapters ("no
  retries: retry/failover policy belongs to ModelRouter, which knows whether
  the originating observation is still current"). By this milestone three
  adapter families exist (`openai`, `systemone`, `tts`) that all return
  `*providers.Error` and inject `*http.Client`, so the router can be
  protocol-agnostic: it wraps any `fn(ctx, Target) (T, error)` and needs no
  per-adapter code.
- The taxonomy (`providers.Kind`, `Error.Retryable`) is frozen this
  milestone and the `internal/providers` package is not in this milestone's
  boundary, so anything the router needs that `providers.Error` lacks
  (Retry-After) travels as a wrapper type in the router package, found via
  `errors.As`.
- docs/spec/01 (OTel note) forbids prompt text or request ids in metric
  labels: the router's health surface is keyed by role only.
- docs/spec/12 "Decision/provider" pins the acceptance cases: provider
  429/503, a request that arrives late after a session reset (stale
  result), and per-role circuit breaker recovery.

## Decision

Add `internal/providers/router`, the single path every model caller takes.

1. **Routing.** `New(reg *providers.Registry, opts Options) *Router` builds
   per-role state for every role in the registry. `Resolve(role,
   Affinity{Key}, caps) (Target{Role, RouteID, BaseURL, Model, Health},
   error)` returns the single configured target with a stable
   `RouteID` = `<role>/<model>` and the current health state. It fails with
   `unsupported_capability` when the role is unknown or not configured, or
   when a requested capability is not declared: capabilities
   (`chat`, `vision`, `json_mode`, `tools`, `systemone`, `tts_design`,
   `tts_clone`, `asr`) are declared per role in `Options.Capabilities`; a
   role with no entry is permissive (declaration unknown — preflight's job
   to probe), a role with an entry must list every requested capability.
   `Affinity` is **recorded** per role (`LastAffinity`) but not acted on:
   there is one target per role today; it exists so sticky cloned-voice
   routing (docs/spec/06 "Routing, reliability and replicas") can build on
   it without a new call shape.
2. **Circuit breaker (per role).** Two internal states, four reported
   states:
   - closed: calls pass. Every transient failure — exactly the kinds
     `timeout`, `unavailable`, `rate_limited` — is timestamped; failures
     older than `BreakerWindow` (default 60 s) are pruned, so a slow
     trickle cannot open the breaker. When the in-window consecutive count
     reaches `BreakerThreshold` (default 3) the breaker opens.
   - open: while now < openUntil every call fails fast with a retryable
     `unavailable` error *before* occupying a concurrency slot. After the
     `BreakerCooldown` (default 30 s) the breaker is half-open: exactly one
     call is admitted as a probe; concurrent callers fail fast. Probe
     success closes (streak cleared); probe transient failure reopens for a
     fresh cooldown; probe permanent failure keeps the half-open phase so
     the next call probes again.
   - reported: `ok` (closed, no transient failures), `degraded` (closed
     with a non-empty streak, or half-open), `open` (cooling down),
     `not_configured`. `Options.OnStateChange(role, from, to)` fires once
     per transition from the Do path; the app publishes `provider.degraded`
     events from it.
   - **Permanent kinds** (`auth`, `bad_schema`, `wrong_checkpoint`,
     `unsupported_capability`, `payload_too_large`, `content_refused`, and
     the rest of the taxonomy) are returned immediately and never trip the
     breaker; they also do not clear a transient streak — a 503-then-401
     endpoint is still half dead.
3. **Retries.** `Do(ctx, r, role, CallOptions{Idempotent, Priority,
   AffinityKey, Capabilities, Generation}, fn)` retries only when
   `Idempotent` and the error is `Retryable`, up to `MaxAttempts` (default
   3). Backoff is exponential with **equal jitter** in `[d/2, d)`:
   200 ms, 400 ms, … capped at 5 s (`RetryBase`/`RetryCap`, configurable),
   drawn through a mutex-guarded source (`math/rand`'s `*Rand` is not
   goroutine-safe; `-race` caught the shared draw). When the error carries a
   `Retry-After` hint the router waits the longer of the computed backoff
   and the hint — since `providers.Error` has no field for it and
   `internal/providers` is out of boundary, the hint travels as
   `router.RetryAfterError{Err *providers.Error, RetryAfter time.Duration}`,
   unwrapped via `errors.As`. Non-taxonomy errors are classified
   defensively: caller cancellation → `cancelled` (permanent), deadline →
   `timeout` (retryable), anything else → `unavailable` (retryable).
4. **Generation guard (stale_result).** `CallOptions.Generation func() bool`
   is the returned-generation check (docs/spec/01). It is consulted **before
   each retry** and **before returning a success**: if the observation is no
   longer current the router returns `stale_result` (non-retryable) and
   **drops the value**, counted in `StaleDropped`. A stale result is not a
   failure: the attempt itself succeeded. This is the "request late after
   session reset" acceptance case.
5. **Concurrency and priority.** Each role has a counting semaphore of
   `Options.Concurrency[role]` (default 2). Admission is priority-ordered:
   a free slot goes to the earliest waiting **Live** caller before any
   waiting Background caller, regardless of queue order; a Live caller
   arriving later still preempts queued Background callers.
   `Router.SetBackgroundPaused(true)` blocks Background callers — even when
   slots are free — while Live callers proceed; unpausing releases queued
   background work. Paused callers block, they are not failed. Cancellation
   of the caller's ctx wakes the wait.
6. **Per-attempt timeout.** Each attempt gets a fresh
   `context.WithTimeout(ctx, RoleConfig.Timeout)`; a zero role timeout means
   the caller's ctx is the only deadline (matching the adapters'
   context-owns-the-deadline convention). Latency is measured with the
   router clock (`Options.Now`, injectable for tests) and recorded in a
   per-role ring of the last 100 attempt latencies (successes **and**
   failures, so a timing-out role shows a rising P95).
7. **Health.** `Router.Health() map[Role]RoleHealth` snapshots
   `State`, `ConsecutiveFailures`, `LastError(kind)`, `LastErrorAt`,
   `LastSuccessAt`, `P50`/`P95` (nearest-rank over the ring), and
   `Calls`/`Failures`/`Retries`/`StaleDropped` (calls = attempts sent to
   the provider; breaker fail-fasts and slot-wait cancellations are not
   counted as calls). `ObserveOrigin(role, origin)` / `Origins()` record the
   ADR-0013 provenance label per role so a report can state that a stage was
   recorded, never live.
8. **Label hygiene.** Every counter and latency is keyed by role only —
   never by prompt text, model request id, or any free-form value
   (docs/spec/01 OTel note). `Target.BaseURL` is a log-safe label; the
   router never logs request content.

## Alternatives considered

- **Retry inside each adapter.** Rejected by ADR-0009 and docs/spec/06: the
  adapter cannot know whether the observation is still current, and three
  adapter families would then carry three copies of the breaker/retry
  policy.
- **golang.org/x/sync/semaphore for concurrency.** Rejected: it is FIFO
  only, cannot express Live-before-Background priority or a global
  background pause, and would add a dependency (AGENTS.md "bounded
  dependencies").
- **A time-windowed failure count (N failures in M seconds) instead of
  consecutive-within-window.** Rejected: a burst of three failures followed
  by one success and more failures would reset a window counter while the
  endpoint is clearly still broken; "consecutive" plus a window that prunes
  old failures is the stricter, more conservative reading of docs/spec/06.
- **Let `providers.Error` carry Retry-After after all.** Tempting, but
  `internal/providers` is outside this milestone's boundary and the taxonomy
  is shared with code that predates it; the wrapper type is additive,
  discoverable via `errors.As`, and costs nothing at the call sites that do
  not use it.
- **Fail-fast callers count as failures in health.** Rejected: they never
  reached the provider and counting them would double-count the breaker's
  own protection in `Failures`.

## Consequences and risks

- Positive: every model caller (decision, VLM, fast reasoning, agent, TTS,
  ASR when it lands) goes through one predictable path with one breaker,
  one retry policy, one priority rule and one health surface; preflight and
  the developer UI can read `Health()` without touching the network.
- Positive: the generation guard makes "late answer after session reset" a
  typed, counted outcome (`stale_result`, `StaleDropped`) instead of a
  narrated ghost.
- Risk: **half-open probes are not time-boxed.** A probe whose context is
  cancelled does not resolve the probe flag in this milestone (cancellation
  is reported as `cancelled` and the breaker stays half-open with the probe
  released on the next transient/permanent resolution); a pathological
  stream of cancelled probes could delay recovery. Mitigation: probe
  contexts carry the role timeout, and the cooldown re-arms only on a
  resolved transient failure; revisit with real traffic.
- Risk: `Options.Rand` is wrapped behind one mutex, so jitter draws
  serialize; the draw is nanosecond-scale and this is negligible next to a
  provider round-trip, but it is a shared lock and should be rechecked if
  retry volume grows.
- Risk: per-role P50/P95 are a ring of attempt latencies, not end-to-end
  observation age; a caller that queues long for a slot will see slot wait
  time in its own accounting, not here. The latency ring measures the
  provider attempt only (documented on `RoleHealth`).
- Risk: the router sees `BaseURL` from `RoleConfig` and passes it through as
  a label; a misconfigured URL fails at the adapter (ADR-0009), not here.

## Evidence and test plan

Recorded (every PR, no network) in `internal/providers/router`:
`TestRetryPolicyMatrix` (non-idempotent / non-retryable fail fast; retry
count 1→3 and backoff schedule in `[100ms,200ms)` / `[200ms,400ms)` under a
fake sleeper; cap honoured), `TestRetryAfterHonoured` (900 ms hint beats the
200 ms backoff), `TestStaleGenerationDropsLateValues` (stale before retry
stops the retry and drops the value; stale on success drops the value, not a
failure), `TestBreakerHalfOpenSingleProbe` (one probe admitted, concurrent
callers fail fast, probe success closes, probe failure reopens),
`TestBreakerWindowPrunesOldFailures` (failures outside the window do not
open the breaker; the in-window third does; fail-fast makes no provider
call), `TestPermanentErrorsDoNotTripBreaker`,
`TestUnclassifiedErrorsAreDefensivelyClassified`, `TestPriorityLiveBefore-
Background` (Live served before an earlier-queued Background waiter at
concurrency 1), `TestBackgroundPause` (order A, C, B: live proceeds while
paused, background released on unpause), `TestPerAttemptTimeout` (each
attempt gets a ≤100 ms deadline), `TestConcurrentCallersRaceFree` (64
goroutines, `-race`), `TestHealthSnapshotCountersAndPercentiles`,
`TestStateChangeCallback` (ok→degraded→open→ok transitions),
`TestResolveReturnsConfiguredTarget` / `TestResolveUnconfiguredRoleIs-
Unsupported` / `TestResolveCapabilityGate` / `TestDoRefusesUnconfiguredRole`.
Integration with the real adapters behind httptest fakes:
`TestDoDrivesTheOpenAIClient` (429 then success: 2 requests, 1 failure, 1
retry, answer `pong`), `TestDoDrivesTheSystemOneClient` and
`TestDoDrivesTheTTSClient` (3×5xx: 3 requests, `unavailable`), and
`TestRecordedTransportBehindRouter`: an `openai.Client` whose HTTP client
uses the `recorded` replay transport (cassette
`internal/providers/recorded/testdata/fast_reasoning.jsonl`) behind the
router returns the recorded answer `pong` with `request_id=fake-chat-1`,
`transport.Stats()` reports 1 hit / 0 misses / **0 dials**, and
`Origins()[fast_reasoning] == "recorded"` — nothing is labelled live.

Commands: `go vet ./internal/providers/router/...`,
`go test -race -count=3 ./internal/providers/router/...` (both run under
`systemd-run --user --scope -p MemoryMax=3G -p MemorySwapMax=0`).

## Deferred (revisit with an ADR)

- **Multi-target routing / failover**: one target per role today; the
  route table is a single entry per role. Gateway administrative discovery
  (docs/spec/06: "provider discovery can query gateway administrative API
  if available") is not implemented; targets stay configured.
- **Sticky TTS affinity**: `Affinity` is recorded, not acted on; cloned-
  voice routing through a sticky dedicated Base endpoint (docs/spec/06)
  needs per-instance targets first.
- **Replica-aware health** (`voice_missing`, `replica_mismatch` recovery
  paths) and preflight capability probing that fills
  `Options.Capabilities` from live probes (docs/spec/06 "Model-serving
  version and health negotiation").
- **Probe time-boxing** for cancelled half-open probes (see risks).

## Migration / rollback

Purely additive: `internal/providers/router` plus this ADR. No schema, no
API, no call-site change; nothing outside the boundary is modified.
Rollback is deleting the package; adapters keep their ADR-0009 behaviour
(no retries) until the runtime is rewired to `router.Do`.
