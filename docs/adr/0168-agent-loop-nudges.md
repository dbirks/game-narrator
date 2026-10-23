# ADR-0168: One nudge before the agent loop gives up — repeat calls and skipped required tools

Date: 2026-10-24
Status: accepted
Owners: agent worker (agent-nudge)
Related requirements/tests: docs/spec/03 (agent loop, durable log, resume), docs/spec/13 (observability and resilience), docs/spec/06 (failure taxonomy); ADR-0025 (agent loop, repetition guard, recovery); GitHub issue #56, slug `agent-nudge`.
Tests: `internal/agent` (`TestRepetitionRecoversWithCorrectiveFeedback`, `TestRunRepetitionGuardBlocks`, `TestRecoveryBudgetSurvivesResume`, `TestSamplingVariationDegradesWhenProviderRefusesTemperature`, `TestMissingToolNudgeRecovers`, `TestMissingToolNudgeNotSentWhenRequestNamesNoTool`).

## Context and constraints

Two nondeterministic agent failures seen in `make test-models` gate sweeps (2026-10-23, issue #56):

1. On flint, a live onboarding run executed `steps=10 tool_calls=10 state=blocked stop_reason=repetition`: the model looped the same tool call until the repetition guard (amendment to ADR-0025 §7) stopped it. A rerun minutes later passed in 3 steps — the guard stopped the run safely, but the model never got a chance to move on before the run was over.
2. On `qwen3-8-27b`, a live smoke run answered a question that required `get_time` without calling any tool. The gateway refuses `tool_choice=required`, so the run uses `auto`, and the first assistant turn sometimes just answers.

Both are model nondeterminism, not faults of the loop. The loop already gives the model one chance to correct itself *after* it has repeated a call twice (corrective observation + varied sampling); the failure modes in #56 are the earlier moments: the first repeat and the first turn with no tool call at all.

## Options considered

1. **Nudge once, early (adopted).** The first time the model emits a tool call identical (name + canonical arguments) to one already executed in this run, the loop does not re-execute it and does not block it: it appends a durable refused tool result that quotes the previous result (truncated to ~1 KB) and says "Do not repeat it; choose a different next step or give your answer." A second identical repeat after the nudge falls through to the existing repetition guard — blocked, `stop_reason=repetition` — unchanged. Separately, when the user request literally names a registered tool and the model's first assistant turn ends with no tool call, the loop sends one corrective user turn ("Call <tool> before answering.") and continues; if the model still answers without the tool, the run succeeds as today.
2. **Leave the guard as-is and rely on reruns.** Rejected: the flint run burned all ten steps on the loop; a single early nudge costs one step and usually recovers (the qwen run and the flint rerun both recovered on their own within two turns).
3. **Retry the model turn with `tool_choice=required`.** Rejected: the gateway refuses the field (issue #56), so this is not portable; a corrective turn works on every provider.
4. **Block on the first repeat (no recovery).** Rejected: too aggressive for model nondeterminism; the existing amendment to ADR-0025 §7 chose bounded recovery, and #56 shows the bound started too late.

## Decision

1. **Repeat nudge** (`internal/agent/loop.go`, `internal/agent/recovery.go`): the repetition counter fires at the *second* identical call (`seen >= 2`) instead of the third; the nudge is the existing refused-envelope path (durable row, events, tool message, `repetitionMarker` error prefix, varied sampling on the next turn), so its exact text per #56 is "you already called <name> with these arguments; it was not re-executed. Its result was: <previous result, canonical JSON, truncated to ~1 KB>. Do not repeat it; choose a different next step or give your answer." `maxRecoveries` is 1: one nudge per (name, args), then the existing guard blocks with `stop_reason=repetition`.
2. **Missing-tool nudge** (`internal/agent/loop.go`): when the run's first assistant turn ends with no tool call, no tool was called yet in the run, and the first user message names a registered tool as a standalone token (longest match wins, so `get_time` in the text names `get_time`, not a hypothetical tool `get`), the loop persists and appends one user message "Call <tool> before answering." and continues. The next round-trip counts against the existing step budget; there is no new budget.
3. **Durability and resume**: both nudges flow through the existing durable-log machinery — the repeat nudge as a `refused` tool row carrying `repetitionMarker` (resume re-derives the spent recovery from those rows, clamped to `maxRecoveries=1`), the missing-tool nudge as a persisted user message (resume detects the " before answering." marker and never sends the corrective turn twice; the first-assistant-turn gate is re-derived from the persisted assistant message count). A restart refunds neither nudge.
4. **Trace**: both nudges emit the loop's existing `agent_step` event — the repeat nudge with `recovery`/`max_recoveries`/`repeated_call` fields, the missing-tool nudge with `missing_tool_nudge: <tool>`.

## Tradeoffs

- One corrective round-trip each costs one step of the existing step budget; a model that still loops after the nudge now reaches the block one step sooner than before (third identical call instead of the fifth). Accepted: the guard's stop reason and the closed state vocabulary are unchanged, and the durable log stays total (every refused call has a row the resume worklist can re-render).
- The repeat nudge quotes at most ~1 KB of the previous result, so a large payload cannot grow the conversation without bound; the full result stays on the executed call's durable row.
- The missing-tool nudge matches the tool name by literal appearance in the first user message only; a request that names a tool in a later message gets no nudge. Deliberately narrow: the nudge fires once on the first turn only, and a false positive costs one step, while a false positive on later turns (where the model may legitimately be answering a follow-up) would be worse.
- The nudge is model-facing untrusted-adjacent text built only from trusted material (the tool name and the tool's own result envelope), consistent with the trust model of the existing corrective observations — nothing fetched enters it, and nothing is threaded into the system prompt.

## Validation

- `go test -race -count=1 -timeout 15m ./internal/agent/...` — all pass, including the four rewritten/update cases: the stub that repeats a call twice then answers (handler ran once, nudge sent, run succeeded), the stub that repeats three times (blocked/repetition as before), the stub that answers without the named tool on its first turn then calls it (exactly one corrective turn, tool called, run succeeded), and the request that names no tool (no corrective turn).
- `TestRecoveryBudgetSurvivesResume` re-verified across a restart: one executed call + one nudge before the cancel, then the third identical call after resume blocks with the original reason — the budget is not refunded.
- `make lint`.
- No live model tests were run (rule: stub-provider tests only).

## Reversal plan

Revert the two changes in `internal/agent` (the `seen >= 2` threshold and `maxRecoveries = 1` in `recovery.go`; the missing-tool nudge branch in `run()` plus `namedTool`/`firstUserText` and the `missedToolNudged`/`assistantTurns` fields). The durable log format changes nothing — refused rows and user messages are already total records — so no migration or data repair is needed; resume logic is only stricter under the new constants.
