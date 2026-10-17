# ADR-0037: Scenario fault injection — a test-only adversity layer on the capture clock, and "blocked ≠ pass" for scenarios
Date: 2026-10-17
Status: accepted
Records decisions made in this change (`internal/scenario`: `faults.go`, `gateways.go`, `run.go`, `truthocr.go`; `testdata/scenarios/synthetic/gn-t0*.yaml`; `docs/status/blocked.md`).
Owners: project
Related requirements/tests: docs/spec/16 M7 ("Exercise concurrency, cache and provider failure injection … no silent voice mismatch or stale speech"); AGENTS.md minimum quality policy ("Failure-injection tests cover dropped frames, delayed/duplicated model responses, endpoint loss, voice missing on a replica, and rapid textbox advances"); docs/test-plan/scenario-catalog.md rows GN-T032, GN-T034, GN-T044, GN-T046, GN-T047; docs/spec/12 "Frame replay"; docs/spec/11 "Fixture origins"

## Context and constraints

docs/spec/16 M7 requires the product to be exercised under adversity — dropped frames, delayed or duplicated model responses, endpoint loss, a voice missing on a replica, rapid textbox advances — and to fail loudly rather than silently (no stale speech, no wrong voice, discarded late results). AGENTS.md adds two hard rules that shape this design:

1. **No simulation theater.** A test that labels a mocked workflow as real end-to-end is forbidden. Faults must therefore be *named* on the timeline, and a scenario that cannot be run honestly must say so instead of passing.
2. **Test-only code stays out of the production path.** The fault layer must never ship in the runtime.

Constraints inherited from earlier ADRs:

- **One event hub, one session** (ADR-0027/0033 shape): the pipeline and the provider layers publish into a single hub, and one `session.Reducer` owns the utterance generation counter. Fault evidence and the events it provokes must land on that one timeline.
- **The session's fences are the product's** (docs/spec/01 invariant 2): late or duplicated provider results are discarded by the reducer's generation/observation/duplicate-seq fences. The harness must drive those fences for real, not re-implement them.
- **Event names are a closed, dotted vocabulary** (`internal/events`): a new name goes through `events.Register`, and the name is validated at the wire boundary.
- **Replay must be the recording it claims to be** (docs/spec/12 "Frame replay"): every frame's OCR read must be recoverable in ModeReplay; a lost read is a failed run.

The M2 baseline already replayed the synthetic clip through the real pipeline with a real Tesseract. What M7 adds is the adversities: someone has to drop frames, delay answers, duplicate replies, kill endpoints and lose voices — and the harness must be able to prove each adversity did what the scenario claims.

## Decision

### The fault-injection layer (`internal/scenario/faults.go`)

A scenario YAML may carry a `faults:` block of typed entries. The kind vocabulary is closed and validated per kind the way assertions are (extra or missing fields are line-numbered load errors):

| kind | fields | where the fault is applied |
| --- | --- | --- |
| `drop_frames` | `from_ms`, `to_ms` | a `DropFrameSource` wrapper around the `capture.Source`: frames whose capture timestamp sits in `[from_ms, to_ms)` are withheld; Seq gaps remain visible downstream |
| `decision_delay` | `after_ms`, `delay_ms` | the fake decision gateway holds replies for requests captured at/after `after_ms` for `delay_ms`, on the request's context (a reply held past the deadline is a timeout, the same abstain path a live budget would take) |
| `decision_duplicate` | `message` | the harness applies the same `DecisionMsg` value twice — same header seq — so the session's duplicate-seq fence is the component under test |
| `endpoint_down` | `role` (`decision`\|`tts`), `from_ms`, `to_ms` | the fake decision gateway answers 503 inside the window (the real client classifies it `providers.KindUnavailable`); the TTS window rides a `faultedTTS` wrapper, because the synthesis payload carries no capture clock — it returns the typed unavailable error a 503 classifies to |
| `voice_missing_on_replica` | `voice` | the fake TTS gateway answers the gateway's unknown-voice 400 for that voice (the real client classifies it `providers.KindVoiceMissing`); other voices synthesize |

Two invariants govern the layer:

- **The capture clock is the clock.** Every window runs on the clip's capture timestamps (the decision requests carry `capture_ts_ms`), not the wall clock. A scenario's timing is a property of the recording, so the test is deterministic: the same YAML plays back to the same adversity at the same capture time.
- **First activation is the event.** Each fault entry publishes exactly one `fault.injected` event — dotted name, registered via `events.Register` in `internal/scenario` (the events package documents `Register` as the extension point for exactly this), carrying kind, role and the capture-clock time. Repeated triggers of the same entry (three frames dropped in one window) are the same fault still active; the per-trigger totals stay on the source/gateway counters. The events package rejects `fault_injected` (a single segment) because its wire grammar is dotted lower-case identifiers; `fault.injected` is the same fact in the legal shape.

### The scenario harness is two layers over one timeline

A runnable scenario (`mode: frame_replay`, `origin: generated_synthetic`) runs as:

- **Layer A — recognition.** The real `pipeline.Runner` in ModeReplay over the frames, with the OCR engine answered from the clip's truth rows (docs/spec/11 "Fixture origins"; the OCR-quality gate for the corpus remains the M2 real-Tesseract replay test). This layer commits the lines and produces the `message.committed` events of record.
- **Layer B — providers.** A sidecar `session.Reducer` (same component, same profile revision, same tracker settings) paced on the capture clock and fed the clip's truth, so its commits land at the same capture times as the pipeline's. For every commit the harness issues a real `systemone.Decide` against the fake decision gateway and, when the decision applies, a real `tts.Generate` against the fake TTS gateway. The answers run through the real clients' wire parsing and error taxonomy; the session's fences decide what applies.

Both layers publish into one hub, so assertions evaluate one merged timeline. The sidecar's own `message.committed` events are **not** forwarded (the pipeline's commits are the commitment of record; merging both would double-count every line) — the sidecar's speech lifecycle events are forwarded, because they are the provider layer's evidence.

Two harness-specific stamping choices, documented so the fence evidence reads honestly:

- A decision reply is stamped with the region's observation seq **at reply time**, not at ask time: the sidecar feeds one observation per frame, so a stable box keeps advancing the seq, and ask-time stamping would make a healthy immediate reply race the next observation for the reducer's lock — a scheduling coin flip, and failure tests must not be flaky. The staleness the fences owe the delayed-reply scenario is the dialogue's, and the **generation fence carries it**: between ask and reply, the commit that advanced the dialogue bumped the generation.
- The decision/speech messages carry the **commit's generation**, not the session's current one, so a reply that lands after a newer commit names the generation the session has passed and is discarded, not applied.

### "blocked ≠ pass"

A scenario that cannot be run honestly carries `status: blocked` with a required `reason:` in its YAML (validated: non-empty; `status` is a closed vocabulary with exactly one value). The runner reports it as `BLOCKED: <id>: <reason>` — it is neither a pass nor a failure, and its report is empty. Every blocked item — scenarios and the standing infrastructure items — is listed in `docs/status/blocked.md` with milestone, why, exact unblock steps and verification. A blocked scenario's `assertions:` may still be written (documenting what it will check once unblocked); they are validated but not run.

### Scenario authoring rules

- Faults are allowed **only** for `origin: generated_synthetic`: a captured, dolphin or hardware scenario naming faults would present an injected adversity as a property of the real source, and the loader refuses the combination.
- Each M7 scenario is labelled with its `docs/test-plan/scenario-catalog.md` row (`catalog: GN-T032`, shape-validated), so a scenario can be traced to the row it covers.
- `inference.voice` is required whenever `inference.tts` is on: a scenario that asks for speech must name the voice it expects, or its `no_voice_substitution` assertion protects nothing.

The first M7 scenarios (all over the existing synthetic clip): GN-T032 rapid advance with `drop_frames`; GN-T034 delayed decision past the advance (`decision_delay`; asserts the discard and silence for the old line) and duplicated delivery (`decision_duplicate`; asserts one speech only); GN-T044 decision route offline (`endpoint_down` role=decision; asserts the visible degraded state and its recovery, abstention, no invented line); GN-T046 voice missing on the replica (`voice_missing_on_replica`; asserts never a different voice — the correct voice or silence with a reason). GN-T047 (TTS stream partial/cancel) is `status: blocked`: the TTS contract is atomic, so the scenario would assert a property the product does not have.

## Options considered

1. **Inject faults into the real `internal/runtime` via build tags.** Rejected: the runtime does not own these layers yet, and a compile-time backdoor for test-only code in the production binary is exactly the "simulation theater" risk AGENTS.md names.
2. **Fake the provider clients outright (interface mocks).** Rejected: the clients' wire parsing and error taxonomy are where a delayed 503 becomes an abstain and an unknown voice becomes `voice_missing`. Driving the real clients against fault-injecting HTTP endpoints keeps that contract under test.
3. **Run the faults on the wall clock.** Rejected: wall-clock timing makes the scenario's evidence depend on the host's load — the same YAML would pass on a fast CI and fail on a slow one. The capture clock makes the adversity a property of the recording.
4. **A single monolithic scenario runner instead of two layers.** Rejected: one session feeding both the pipeline and the providers would couple the recognition stage to the provider stage and hide which stage a fault actually hit. Two layers over one hub keep the stage boundary visible in the timeline.

## Positive consequences

- Every AGENTS.md failure-injection case has a named, deterministic, test-only implementation; each fault kind has a unit test proving it fires at the configured time.
- The session's fences are exercised by the harness as the product's component, not re-implemented: `stale_generation`, `duplicate_seq` and the speech phase fences are the things under test.
- Fault evidence and the events it provokes share one timeline, so a report can show *the drop at 1500 ms* next to *the cancelled speech at 1900 ms*.
- "blocked ≠ pass" makes the register honest: missing coverage is reported as missing, with unblock steps, instead of dressed up as green.

## Negative consequences / risks

- The sidecar duplicates a sliver of the pipeline's per-frame work (observations from truth instead of OCR). If the pipeline's observation path changes, the sidecar must follow it; the shared `session`/`recognition` types keep the surface small, and the scenarios are the compatibility test.
- The reply-time observation-seq stamping is a harness choice, documented above; when the real M4+ decision layer lands, the runtime's own stamping is what the live tests exercise — the scenario harness keeps proving the fence semantics, the live tests prove the producer.
- The dotted event name differs from the underscored wording in older notes (`fault_injected`); the events wire grammar is the authority.

## How to verify

```
go test ./internal/scenario/...            # unit tests (per-fault firing) + every scenario
```

- `TestFaultDropFramesFiresAtConfiguredTime`, `TestFaultDecisionDelayFiresAtConfiguredTime`, `TestFaultEndpointDownFiresAtConfiguredTime`, `TestFaultVoiceMissingFiresAtConfiguredTime`, `TestFaultDecisionDuplicateFiresAtConfiguredTime`, `TestFaultTTSEndpointDownViaWrapper`: each fault kind fires at the configured time, once, with the right kind/role.
- `TestScenarios`: every YAML under `testdata/scenarios/synthetic/` loads and runs; the five runnable M7 scenarios pass their fault assertions (including `late_result_discarded`, `duplicate_result_discarded`, `no_voice_substitution`, `provider_degraded` with recovery, `fault_count`); the blocked GN-T047 scenario is reported `BLOCKED: <reason>` and `TestBlockedScenariosAreReportedNotPassed` pins that a blocked scenario never reports a passing report.
- The M2 baseline (`dialogue-advance-001`) still passes with the harness in place: the new layers are additive.

## Rollback / migration path

The layer is additive: delete `faults.go`/`gateways.go`/`truthocr.go`/`run.go`, the six scenario YAMLs and the new assertion evaluators, and `internal/scenario` returns to its M2 shape. No persistence, wire or runtime surface is touched, so there is nothing to migrate; the `fault.injected` event name is unregistered with the package (process-lifetime registration only).

## Links / references

- `docs/status/blocked.md` — the BLOCKED register (this ADR's "blocked ≠ pass" rule is its governing rule).
- `docs/spec/05` "Preserve raw observed OCR"; `docs/spec/06` decision budget and abstention; `docs/spec/08` one hub, correlation ids; `docs/spec/12` frame replay; `docs/spec/11` fixture origins.
- ADR-0009 (provider roles), ADR-0013 (recorded provider cassettes), ADR-0023 (speech cache and scheduler), ADR-0033 (M5 replay E2E shape).
