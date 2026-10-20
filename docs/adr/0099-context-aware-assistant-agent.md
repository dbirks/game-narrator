# ADR-0099: Context-aware assistant agent (the agent dock's backend)

Date: 2026-07-06
Status: accepted
Owners: agent worker (task agent-dock-api)
Related requirements/tests: docs/design/plum-peach/product-notes.md UX-01
(context payload on every agent message), UX-04 (non-blocking agent
questions), UX-05 (corrections feed back to the agent); docs/spec/03 (agent
loop, durable run log, tool registry), docs/spec/08 (API, SSE, role ACL),
docs/spec/14 (trust zones, tool executor verifies allowlist/args/role/budget),
ADR-0017 (hand-written OpenAPI contract), ADR-0025/0026/0028 (onboarding
agent core, tools, API), ADR-0098 (⌘K global search store layer),
`internal/agent` (reused loop, executor, run log), `internal/assistant`
(new), `internal/api/assistant.go` (new),
`internal/store/assistant_store.go` (new), migration
`0016_assistant_threads.sql` (new)

## Context and constraints

The design review (docs/design/plum-peach) puts a context-aware assistant on
every screen (the ⌘J dock, UX-01): every message carries
`{route, gameId, profileId, selection: {type, id}, visibleIds[]}` so
"make her older" resolves against the selected character. Its questions must
never pause a run (UX-04), and corrections must feed back: when the user
marks a line wrong, the agent checks similar lines (same speaker plate, same
scene, same OCR pattern) and fixes in bulk only after explicit confirmation
(UX-05). The UI (Phase 1) consumes the surface this ADR builds; this ADR is
backend only.

Constraints:

- **Reuse the agent runtime.** The loop, the guarded executor (allowlist,
  args schema, role, budget, timeout), the durable run log and the job-queue
  crash boundary are ADR-0025's contract; a second bespoke loop would fork
  the total-worklist property (scenario 7) that resume correctness depends
  on.
- **The model is the AGENT_REASONING role through the provider router**
  (docs/spec/06): today qwen3-8-27b, which rejects `response_format` and
  `tool_choice: "required"`. Tool calls therefore arrive as plain-text
  tool_calls the openai client already parses, and the prompt is
  configuration-derived (prompt-and-validate, as the onboarding agent does).
- **Questions never block (UX-04).** The onboarding loop stops a run in
  `review` on `ReviewRequired`; the assistant's questions must not. A run
  that asks a question keeps advancing and ends; the answer, when it
  arrives, is applied by a fresh attempt on the same durable log.
- **Mutations are confirmed by default.** Every mutating tool shows a
  confirmation card unless the user's setting says otherwise, writes an
  audit entry, and is role-checked by the executor (docs/spec/14).
- **Public-repo rule 5:** tests use invented synthetic data only.

## Options considered

1. **New thread model: one long-lived agent run per user thread.** A thread
   row maps to one `agent_runs` row; the durable run log *is* the thread's
   conversation. Each `POST /api/v1/assistant/messages` appends the user
   turn to the log and enqueues a resumed `assistant.run` job — exactly the
   onboarding message flow (ADR-0028). Chosen: it reuses the total-worklist
   resume, the SSE agent events (agent.step, tool.started, tool.completed,
   already emitted by the loop with the run as trace id), and the per-run
   budgets, with one thread row and one questions table added.
2. **One run per message.** Rejected: the conversation would not persist
   across turns without re-rendering history into every prompt, the durable
   log's replay property would go unused, and every turn would re-run the
   full system-prompt + budget machinery.
3. **Questions as `ReviewRequired`.** Rejected: it stops the run in
   `review`, which is precisely the blocking UX-04 forbids.

## Decision

1. **Routes (scope table: operator and above).**
   - `POST /api/v1/assistant/messages` — body
     `{thread_id?, text, context?: {route, game_id, profile_id,
     selection?: {type, id}, visible_ids?[]}}`. Creates the thread + run on
     the first message (202, `{thread_id, run_id, job_id, seq}`); on later
     messages appends the turn to the durable log and enqueues the resume.
     409 while an attempt is in flight, 404 for a thread the caller does not
     own.
   - `POST /api/v1/assistant/answers` — body
     `{thread_id, question_id, answer}`. Records the answer, appends the
     user-facing answer message to the durable log and enqueues the attempt
     that applies it (or hands it to the in-flight attempt's follow-up hook).
   - `GET /api/v1/assistant/threads` and `GET
     /api/v1/assistant/threads/{id}` — the thread list and the rebuild
     snapshot (conversation tail, open questions, compact tool cards
     `{name, state, duration_ms, result}` from the run's tool-call rows).
2. **The context payload reaches the prompt in the user turn, not the
   system prompt.** The turn is rendered as
   `[SCREEN CONTEXT] … [/SCREEN CONTEXT]` + the user's text, with every
   field bounded (route ≤ 300, ids validated, visible_ids ≤ 64 entries) so a
   hostile dock cannot grow the prompt. The system prompt stays
   configuration-derived (ADR-0025's prompt rule).
3. **Assistant run.** Job kind `assistant.run` (distinct from the
   onboarding `agent.run`: different registry and prompt), executing as a
   durable job with concurrency 1 and a 5-minute attempt timeout. Budgets:
   12 model steps, 24 tool calls, 300 s wall clock — an interactive turn is
   not a 30-minute research run.
4. **Questions are a table, and the run never waits on them.**
   `assistant_questions` rows (kind `question` from the model's `ask_user`
   tool, kind `confirmation` from a mutating tool) carry the prompt, the
   options, the screen context at ask time, the answer and the seq of the
   answer's durable message. Both kinds emit one `assistant.question` event
   (the NEEDS-YOU card / confirmation card, `{id, prompt, options, kind}`)
   and return a *normal* tool result — the loop continues. When the answer
   arrives, the handler appends a user message naming the question and the
   answer; if an attempt is in flight, the job handler's post-run hook sees
   the answer's seq is newer than the attempt's start and enqueues exactly
   one follow-up attempt (the run log is the total record, so the follow-up
   sees the answer). A `failed` run is terminal, as in the onboarding log.
5. **Confirmation gating is two-phase through the same question row.** A
   mutating tool called without a `confirmation` argument (while the user's
   `assistant_confirm_mutations` setting, default true, requires it) stages
   the exact action (tool name + canonical args) on a confirmation question
   with options `["confirm","cancel"]`, emits the card and returns
   `confirmation_required`. The model continues (it may answer other work or
   end with a summary). The answer "confirm" re-invokes the tool with
   `confirmation: <question_id>`; the tool re-validates the row (same
   thread, answered, answer=confirm, staged args byte-identical) before the
   side effect. "cancel" is recorded and the tool reports the cancellation —
   it never retries. The setting lives in `app_settings` under
   `assistant.<user_id>` (the table ADR-0074's audio settings established as
   the key/value home for runtime-mutable values).
6. **The tool allowlist** (all registered through the ADR-0025 executor, so
   allowlist/args/role/budget/timeout are enforced before any handler runs):
   - reads: `search` (the ADR-0098 global retrieval: games, characters,
     lines — the ⌘K backend's store layer), `get_character`, `get_line`,
     `get_trace` (developer tier: traces are developer evidence,
     docs/spec/08), `find_similar_lines` (UX-05: same speaker plate, same
     scene, or the same OCR text pattern — the read that precedes a bulk
     fix);
   - mutations (all confirmation-gated by default, all audited):
     `update_voice_brief` (the character's `personality_summary`, the
     editorial prose the voice-design agents read), `regenerate_voice`
     (through `voices.Service.CreateDraft` + the `voice.auditions` job: the
     previous revision stays, so the one-step undo of UX-03d is the existing
     revision history), `relink_speaker` (repoints the named lines to a
     target character with manual assertions, one transaction),
     `apply_speaker_fix` (the UX-05 bulk apply: same repoint over the
     `find_similar_lines` result set, refused until the user confirmed the
     exact line set), `mark_correction` (records a
     `corrections` row, ADR-0052's fixture contract), `start_narration` /
     `stop_narration` (the runtime manager's session lifecycle).
7. **Roles.** The scope table grants the whole `/api/v1/assistant` prefix to
   `operator` (the dock acts on the appliance: sessions, voices, content).
   Each attempt runs with the *caller's* role (carried on the job payload),
   so a viewer-token holder cannot reach the routes at all and a developer
   caller's `get_trace` passes its `RoleDeveloper` minimum while an
   operator's is refused by the executor.
8. **Streaming.** The reply streams over the existing SSE hub: the loop's
   `agent.step` / `tool.started` / `tool.completed` events (trace id = run
   id, so the dock filters by thread) plus two new registered names —
   `assistant.message` (the narrative text of a finished attempt) and
   `assistant.question` (the NEEDS-YOU / confirmation card). The thread GET
   is the rebuild for a client that missed the stream (docs/spec/08's
   snapshot+stream split, as for onboarding state).
9. **Contract.** The four operations join `openapi.json` with
   `x-required-role: operator`; the drift tests (route ↔ document, role ↔
   scope table, body ↔ schema) cover them like every other route.
10. **Model and degraded mode.** `agent.RouterCall` over
    `agent_reasoning` (ADR-0019's single router boundary). A missing or
    broken endpoint lands the attempt in the loop's existing degraded
    outcome (blocked + `provider_unavailable`, resumable) — the assistant
    never 500s on a gateway fault, it says the model is unavailable.

## Alternatives considered (rejected)

- **A second agent loop tuned for chat.** The loop's stop conditions,
  repetition recovery and resume worklist are the hard-won parts
  (ADR-0025); forking them for "chat" would silently lose scenario 7.
- **Live mid-attempt answer injection.** Appending to the durable log while
  an attempt is in flight is already safe (the log is append-only and the
  attempt's conversation is in memory); what needed a design was the
  *follow-up* guarantee, and the post-run hook (answer seq vs. attempt start
  seq) is the minimal deterministic form. A live-injection channel into the
  running loop would be a new prompt-injection surface for no observable
  gain: the answer is applied, just by the next attempt.
- **Per-message runs with history re-rendered into the prompt.** Doubles
  token cost every turn and abandons the durable log's exact replay.

## Evidence and test plan

- Migration + backup/restore: `internal/store/assistant_store_test.go`
  (forward migration on a blank DB; seeded thread/question rows survive
  `Backup` → `Restore` row-for-row, AGENTS.md rule 10).
- Hermetic agent tests (scripted model, in-memory everything-else):
  tool-call parsing through the real openai client path; the context payload
  reaching the prompt (asserted on the recorded request); a question
  raised mid-run with the run continuing to a terminal state while the
  question stays open (UX-04); confirmation gating (staged action refused
  without the token, applied with it, byte-identical args re-validated);
  role checks (operator run refuses `get_trace`, developer run passes).
- API tests: operator-only 403 for viewer/curator; first-message thread
  creation (202 + queued attempt); 409 while in flight; ownership on
  thread reads; the answer flow appending the durable message and enqueuing
  the applying attempt; the follow-up hook enqueuing exactly one attempt
  when the answer lands mid-attempt.
- Live (manual, `GN_GATE=test-models` only, skips cleanly otherwise): a
  seeded character + selection context, "who is the selected character?" —
  asserting a real provider request happened (request proof retained) and
  the reply named the character.

## Positive consequences

- The dock's backend is one durable, resumable, budgeted loop — no second
  prompt-injection surface, no second crash boundary.
- UX-04 is structural, not disciplinary: the loop has no code path that
  waits on a question.
- The confirmation flow is one table and one answer endpoint, shared by
  free-form questions and action confirmations.

## Negative consequences / risks

- An in-flight attempt can be minutes old when the user answers; the
  follow-up then re-runs from the log, which may repeat read-only tool
  calls (bounded by the per-attempt budget; mutations are idempotent by
  their staged-args re-validation).
- The `assistant.<user_id>` app_settings rows grow one per user — the same
  growth as the user table itself, bounded by design.
- `get_trace` inside a chat run widens the developer's data exposure to the
  dock; it is developer-gated (the executor's MinRole), the trace payload is
  rendered through the untrusted-data delimiters (gameplay text is
  untrusted zone-C data, docs/spec/14), and the run budget bounds how much
  one turn can pull.

## Migration / rollback path

Forward: migration 0016 (additive tables + indexes; the `characters`
schema is untouched — the voice brief is the existing
`personality_summary`). Rollback: drop the two tables, the
`assistant.run` job registration, the `internal/api/assistant.go` routes,
the scope-table grants, the OpenAPI operations and the `internal/assistant`
package; the onboarding agent is unaffected (its registry and kind are
separate). Existing `app_settings` rows are inert without the reader.

## Links / references

- docs/design/plum-peach/product-notes.md UX-01/UX-04/UX-05 and
  docs/design/plum-peach/plan.md (agent dock item)
- ADR-0017, ADR-0016, ADR-0025, ADR-0026, ADR-0028, ADR-0046, ADR-0052,
  ADR-0074, ADR-0098
- `internal/agent` (loop, executor, run log, job adapter),
  `internal/agent/tools` (onboarding tool set, the style this ADR's tools
  follow), `internal/voices` (regenerate path), `internal/runtime`
  (narration lifecycle), `internal/store/repo_search_global.go` (⌘K
  retrieval)
