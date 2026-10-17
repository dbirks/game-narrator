# ADR-0025: Onboarding agent core — tool-calling client, typed registry, guarded executor, durable run log, single-loop resume

Date: 2026-10-17
Status: accepted
Owners: project
Related requirements/tests: docs/spec/03 (single orchestrator agent, tool registry, durable tool/job log, scenario 7 "resumes without duplicates"); docs/spec/14 "Prompt injection" and "Abuse and resource controls" (executor verifies allowlist, args schema, role and cost before action; bound tool budget); docs/spec/17 "Agent tool result" (wrapped envelope); docs/spec/08 "Agent chat interface"; docs/spec/12 "Agent and sources"; docs/agents/system-prompt-onboarding.md (versioned prompt template); builds on ADR-0013 (recorded providers), ADR-0019 (model router)

## Context and constraints

- docs/spec/03 mandates **one** orchestrator agent with typed tool calls, a
  durable tool/job log and a resumable plan — not a swarm. Deterministic
  sub-workflows (extraction, validation, scoring) live outside the model loop.
- The collective model `qwen3-8-27b` (role `agent_reasoning`) was verified
  live on 2026-10-17 to support OpenAI `tools` + `tool_choice:"auto"` and to
  return **parallel** `tool_calls` (`finish_reason:"tool_calls"`,
  `content:null`, each call `{id, type:"function", function:{name,
  arguments:<JSON string>}}`). The OpenAI client had no tool support.
- docs/spec/14 is binding: retrieved web text and imported game text are
  untrusted *data* (zones C/D); no privileged instruction or tool schema may
  be built from retrieved data; the tool executor verifies allowlist, args
  schema, role and cost **before** action; tool budgets are bounded.
- docs/spec/03 scenario 7: an interrupted run must resume **without
  duplicates** — a call with a durable outcome is never re-executed.
- This task builds the **core only**. The concrete research/import/voice
  tools (identify_game, web_search, fetch_page, parse_dialogue, ...), the
  API surface and the UI come in later tasks against the contract defined
  here.

## Decision

### 1. OpenAI tool-calling in the existing client (no new module)

`internal/providers/openai` gained:

- `ToolDef{Name, Description, Parameters json.RawMessage}` and
  `ToolChoice` (auto|none|required|named) on `ChatRequest`; `Tools` empty
  sends **no** `tools`/`tool_choice` field, keeping pre-tool requests
  byte-identical for gateways that reject the field (docs/spec/06
  compatibility).
- `Message.ToolCalls` (assistant) and `Message.ToolCallID` (role `tool`);
  `ChatResponse.ToolCalls`.
- `arguments` arrive as a JSON **string** on the wire; the client parses and
  validates them, and an unparseable arguments string fails the whole
  response as `bad_schema` — the executor never runs garbage.
- Keyword `tool_choice` values go out as the plain string form (the verified
  gateway shape); a named tool goes out as the `{"type":"function",...}`
  object form.
- Tests: wire-shape, parallel-call parsing and malformed-args rejection over
  an httptest fake gateway (category (a) synthetic stub), plus one live test
  against the configured gateway (NOT RUN without env).

### 2. Single orchestrator, typed registry, no shell

`agent.Run` is the single loop: model → tool calls (parallel calls execute
sequentially, in the model's order — determinism over concurrency) → results
→ model, until a stop condition. `agent.Registry` is the allowlist:

- registration validates name shape (`[a-z][a-z0-9_]{1,99}`), schemas
  (object/required/type/enum/maxLength/maxItems subset, recursive), role
  ceiling, side-effect class (none|draft|external) and budgets;
- only registered tools are offered to the model (`Definitions()`) and only
  they execute;
- **no shell/exec tool exists and cannot be registered**: the names
  `shell|exec|bash|run_command` (plus `system|spawn|subprocess|run_script|
  command_exec`) are refused at registration (docs/spec/03 scenario 3,
  docs/spec/14). An injection attempt that makes the model call `shell`
  receives a *refused* result envelope — never an execution.

### 3. Guarded executor (docs/spec/14 "before action")

`Executor.Execute` runs in a fixed order, refusing before anything
expensive: **allowlist → args JSON + input schema → caller role ≥
MinRole → global per-run budget → per-tool budget → per-call timeout**.
Every outcome is wrapped into the docs/spec/17 envelope
`{schema_version:1, tool_call_id, name, status: succeeded|failed|refused|
timeout|cancelled, results|error, source_ids[]}` and that envelope is both
what the model sees and what the log stores. Budgets consume only
dispatched calls (refusals do not spend budget); on resume the counters are
seeded from the log so a restart cannot "refund" the budget.

### 4. Untrusted-data channel

Text captured from untrusted sources (web, imports) reaches the model only
through `QuoteUntrustedText` (agent-side equivalent of
`web.QuoteUntrusted`): a header declaring the block DATA-ONLY, `---` fences,
a footer. The delimiters mirror the web package's contract, so both channels
are recognisably the same protection. Tool results for tools flagged
`UntrustedOutput` are rendered inside the block; the system prompt is built
exclusively from configuration (prompt.go) and no code path threads tool or
web output into it. The happy-path test asserts an injected
"ignore previous instructions…" string appears **only** inside the
delimiters in the next request, and never in the system message.

### 5. Versioned prompt template

`docs/agents/system-prompt-onboarding.md` is vendored verbatim (header
stripped) into `internal/agent/prompt_onboarding.md` and embedded
(`go:embed`), so the binary is self-contained. `PromptVersion =
"onboarding-1"` is stored on every run row. Dynamic fields (caller role +
grants, enabled tools from the registry, frozen budgets, edition/source
policy) render as separate `[NAME] … [/NAME]` sections, each bounded to
4096 bytes; they are never interpolated into the template body and never
carry web content. Editing the template or section layout bumps the version.

### 6. Durable run log (migration 0007) + resume semantics

`agent_runs` (closed state set `queued|running|blocked|review|failed|
succeeded|cancelled`, `plan_json`, `budget_json`, `prompt_version`,
`model`, `stop_reason`), `agent_messages` (per-run `seq`, the exact replay
order) and `tool_calls` (per-run `seq`, `status` pending until a durable
outcome). All JSON columns are `json_valid`-guarded; the tool_call CHECK
ties `status='pending'` to `result_json IS NULL` so the resume predicate is
enforced by the schema, not by caller discipline.

The resume worklist is total: (1) assistant tool-call with no row → insert +
execute; (2) pending row → execute exactly once and record the outcome;
(3) finished row → never re-execute, re-render the tool message from the
stored result if the crash fell between the two writes. Durable writes use
fresh short contexts, not the run context: cancellation is exactly what the
log records, and a cancelled context must not fail the write that stores the
cancellation. `cancelled`, `blocked` and `review` are **resumable** states;
only `failed`/`succeeded` close a run.

### 7. Loop stop conditions

Final answer → `succeeded`; `max_steps` (default 24) → `blocked:
max_steps`; global tool budget exhausted → `blocked:
budget_exhausted`; identical (name+canonical-args) call requested for the
third time → `blocked: repetition` (the counter is seeded from the log on
resume so a restart cannot launder repetitions); a tool returning
`review_required` → `review` (+ `review.required` event); context
cancellation → `cancelled` (resumable); model error → `failed:
model_error`. Events emitted: `agent.step`, `tool.started`,
`tool.completed`, `review.required` (all pre-registered in the events
package). The model is called through `router.Do` over `agent_reasoning`
via `agent.RouterCall` (per-target client, so replica failover works);
tests inject a scripted model instead.

## Tradeoffs

- **A dedicated JSON-schema validator** (~150 lines, declared subset)
  instead of a third-party schema library: the subset is small and frozen by
  the registry table, the dependency ban (AGENTS.md: no new modules) makes
  the choice easy, and "total over the declared subset, honest about the
  rest" is easier to reason about than a partial engine. Unknown keywords
  are ignored, not errors.
- **Refusals are fed back to the model** (as the refused envelope) rather
  than failing the run: the model can course-correct (unknown tool, schema
  miss, role miss). Only run-level conditions (budget, repetition, review,
  steps) stop the run. This matches docs/spec/03's conversational model and
  makes the injection outcome observable to the model.
- **Vendored prompt file** instead of reading `docs/agents/` at runtime:
  `go:embed` cannot escape the package directory and the binary must be
  self-contained. Cost: the copy must be kept in sync with the doc; the
  prompt version on the run row is the audit trail.
- **Per-call `pending` rows before execution** (crash safety) at the cost of
  an extra write per call: on a 4 GB appliance with bounded tool budgets
  this is negligible, and it is what makes the resume predicate total.
- **Sequential parallel calls**: the loop is single-threaded, so
  `tool_calls` execute in the model's order. Deterministic, easier to
  persist, and the model gets all results in one next turn — matching the
  verified gateway behaviour.

## Validation

- `go vet` + `-race` over `internal/providers/openai`, `internal/agent`,
  `internal/store` (see evidence: all recorded/unit tests pass; the live
  openai tool test and the agent live smoke pass against
  `qwen3-8-27b`, logged as `model/steps/latency`, no URL/token logged).
- Scenario coverage: happy path (persisted conversation + ordered events),
  unknown tool → refused, schema miss → refused not executed, role too low →
  refused, budget → `blocked: budget_exhausted`, max_steps →
  `blocked: max_steps`, repetition (3× identical) → `blocked: repetition`,
  prompt injection → delimiters-only + `shell` refused, parallel order,
  review → `review` state + event, and the **resume** test: kill (cancel
  ctx) after the 2nd of 3 parallel calls, close and reopen the store,
  resume — handler counters prove A/B were not re-executed and C executed
  exactly once, ending `succeeded`.
- Store invariants: state/status enums, JSON write-edge validation, seq
  ordering, duplicate-finish conflict, pending worklist survives reopen.

## Consequences and reversal plan

- **Later tasks build on this**: the concrete tools (identify_game,
  web_search, fetch_page, import_user_asset, parse_dialogue, ...
  from docs/spec/03's registry table) register into `agent.Registry` and
  plug their outputs into the `Result` envelope (`SourceIDs`,
  `ReviewRequired`, `UntrustedOutput`); the API/UI task exposes
  `agent.Run` behind the authenticated routes of docs/spec/08 with the
  caller's `auth.Role` as `CallerRole`; the jobs integration can wrap a
  run in a durable job for background execution.
- Reversal: the package is additive. Dropping `internal/agent`, migration
  0007 (forward-only: superseded by a later migration, per AGENTS.md
  version discipline) and the openai tool fields returns the pre-ADR
  behaviour; the openai client stays backward compatible because empty
  `Tools`/`ToolChoice` change nothing on the wire.
