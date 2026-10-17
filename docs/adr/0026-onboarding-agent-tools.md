# ADR-0026: Onboarding agent tools — the concrete research/import/quality/voice/publish tool set, side-effect classes, human approval tokens, and behavioral acceptance scenarios

Date: 2026-10-17
Status: accepted
Owners: project
Related requirements/tests: docs/spec/03 (tool table, search playbook, source-quality report, human-in-the-loop boundaries, behavioral acceptance scenarios 1–10); docs/spec/14 ("Prompt injection", "Abuse and resource controls", bounded search budget, SSRF filter); docs/spec/17 ("Agent tool result" envelope with `source_ids`); ADR-0025 (agent core: typed registry, guarded executor, durable run log, resume); migration 0008 `agent_approvals`

## Context and constraints

- ADR-0025 built the core (loop, registry, executor, run log, resume) with a
  contract for concrete tools; this ADR records the first tool
  implementation against that contract. The tool package is
  `internal/agent/tools`; it introduces no new dependency and no new Go
  module.
- docs/spec/03 binds each tool's MinRole, what it may touch, and the
  human-in-the-loop boundaries: the agent researches autonomously but
  publishes only on explicit human approval; voice design is reviewable;
  game artwork is never imported by the agent (AGENTS.md rule 5).
- docs/spec/14 is binding: retrieved web text and imported game text are
  untrusted data (zones C/D); web egress is SSRF-filtered
  (`web.Policy.ValidURL` + `web.Fetcher`); search goes through the budgeted
  provider wrapper; no privileged instruction is built from retrieved data
  (tool input schemas and prompts are fixed; model context wraps
  untrusted material in `agent.QuoteUntrustedText` for the creative tools).

## Decision

### 1. Tool catalogue (one handler per tool, all returning the docs/spec/17 envelope)

Every tool returns `agent.Result` (`schema_version:1`, `status`, `results`
JSON payload, `source_ids`, `review_required`, `error`). `DefaultTools(Deps)`
registers the catalogue; each tool declares MinRole, a per-run
`MaxCallsPerRun` budget, a timeout, and a SideEffect class.

| Tool | MinRole | MaxCallsPerRun | Timeout | SideEffect | Notes |
| --- | --- | --- | --- | --- | --- |
| `identify_game` | viewer | 4 | 60s | none (read) | catalog-first; web only when the catalog is empty; confidence tiers 0.9/0.7/0.4, plausible ≥0.5; ≥2 plausible candidates ⇒ `review_required` (scenario 1) |
| `web_search` | viewer | 10 | 45s | none (read) | goes through `Deps.Search` (budgeted provider); results carry trust 0.4 / rights `unknown`; untrusted channel (see §3) |
| `fetch_page` | viewer | 10 | 45s | none (read) | SSRF-filtered via `web.Policy.ValidURL` + `web.Fetcher`; page text is an untrusted channel |
| `import_user_asset` | curator | 4 | 300s | draft | user-uploaded bytes addressed by staged asset SHA-256; role whitelist (dialogue_script, script, text_dump, metadata, other), artwork roles refused; media-type + magic-byte safety gate before the parser; idempotent through `profileio.ImportFile` (reconciles by source + source_message_id) |
| `parse_dialogue` | curator | 4 | 300s | draft | re-imports an existing source with an explicit `parser_id` (adapter id or forced format), validated against detection before any write |
| `source_quality` | viewer | 8 | 30s | draft (flags only) | the docs/spec/03 report: message_count, unique_ids, nonempty_text_pct, speaker_labeled_pct, graph_linked_pct, language_consistency, conflicts; quarantines conflicting rows with durable flags (`PutMessages`), never deletes; conflict ⇒ `review_required`; standing note "incomplete by default until strong evidence confirms otherwise" (scenarios 2, 4, 10) |
| `preview_profile` | viewer | 4 | 10s | none (read) | side-effect-free projection of the draft for human review |
| `propose_artwork` | curator | 4 | 10s | none (stub) | **stub, §4** |
| `analyze_character` | curator | 6 | 90s | none (read) | cited lines only (no invented lines); LLM call through `Deps.Chat`; emits a JSON `voice_brief` with every creative field wrapped `{value, creative:true}` |
| `design_voice` | operator | 3 | 180s | draft (assets) | 3 synthetic auditions via the TTS designer; WAVs stored as draft assets; `review_required` (approval is a human step); never touches the preferred voice (scenarios 6, 10 boundary) |
| `publish_profile` | curator | **1** | 60s | publish gate | refuses without a valid, unspent, unexpired human approval token AND profile status `review`; the store's publication validation runs inside the publish transaction; one attempt per run (a refused publish stops the run for a human, it is not a retry loop) |

### 2. Side-effect classes and write discipline

- `SideEffectNone`: research/read tools. They never write to the store or
  asset store.
- `SideEffectDraft`: `import_user_asset`, `parse_dialogue`, `source_quality`,
  `design_voice`. Writes go only to the draft revision (messages, sources,
  characters, draft assets, durable flag rows). `source_quality`'s writes
  are quarantine flags — the conflict is quarantined, nothing is deleted,
  and a tie quarantines every conflicting row (scenario 4) while a strict
  trust winner is named and kept (scenario 10: extracted user-supplied
  material beats generated or fetched text).
- Publish gate: `publish_profile` is the only tool that can move a revision
  to `published`, and only through `review`, and only under a human token.
  Published revisions are immutable afterwards (store-enforced).

### 3. Untrusted channels

- `web_search` / `fetch_page` results are marked untrusted: the payload is
  wrapped with `agent.QuoteUntrustedText` on the model-facing tool result,
  and source rows they register carry trust 0.4 and rights `unknown` —
  they can never outrank extracted user material in a conflict (see
  §1 `source_quality`), and they are not publishable rights.
- `analyze_character` / `design_voice` never see raw imported text directly
  in a prompt that could steer them: the creative model input is built from
  cited, normalized lines wrapped in `QuoteUntrustedText`, and every
  creative output field is labelled `creative:true` so downstream steps
  (TTS) treat the brief as data to audition, not instructions.
- Import is provenance-aware: bytes enter only via the asset store
  (SHA-256 addressed), are re-verified against the hash before parsing,
  and are gated by media type + magic bytes (executable/archive blobs are
  refused).

### 4. What is stubbed: `propose_artwork`

`propose_artwork` is a labelled stub: it records candidate URLs in an
in-process ledger (`artworkLedger`) with state
`candidate_not_published` and license `unknown`, and returns
`candidate_not_published` in its envelope. It deliberately does **not**
fetch, store, or render any image bytes (AGENTS.md rule 5: game artwork is
never imported by the onboarding agent; the real candidate pipeline —
download with rights check, storage, UI review — is a later task with its
own approval surface). The stub exists so the model has a legal tool for
"change the cover" requests (scenario 5: only artwork candidates change;
messages, voice and asset state are untouched) and so the boundary is
testable now rather than accidental later.

### 5. Human approval tokens (migration 0008)

- New table `agent_approvals` (migration `0008_agent_approvals.sql`):
  `id`, `profile_id`, `token_hash` (SHA-256 of the raw token — the raw
  token is never persisted), `granted_by`, `created_at`, `expires_at`,
  `consumed_at`.
- `store.CreateApproval` / `store.ConsumeApproval` implement single-use
  consumption durably: the burn is one guarded UPDATE
  (`consumed_at IS NULL`), so a crash between read and write cannot
  double-spend, and expiry is checked against the stored `expires_at`.
- The tools layer defines `ApprovalStore` with `Issue` (human/UI step — no
  agent tool issues tokens) and `Consume` (used only by
  `publish_profile`). Implementations: `MemoryApprovals` (tests/dev, 24 h
  TTL) and `SQLiteApprovals` (production, backed by 0008).
- Token format `gnapp_` + 64 hex chars; unknown, spent, and expired tokens
  all fail the same refusal path, which ends the run (publish is
  `MaxCallsPerRun: 1`).

## Validation

- Unit + behavioral acceptance tests in `internal/agent/tools`:
  scenarios 1–10 of docs/spec/03 as `TestScenario_N_*`, plus the approval
  gate tests (`TestPublishProfileRequiresHumanApprovalToken`,
  `TestMemoryApprovalsSingleUse`, `TestSQLiteApprovalsSingleUse`). Scenario
  7's crash is simulated through a `Deps.CrashSim` seam that panics after
  the import's writes but before the durable outcome row — the pending-row
  shape a real process death leaves — and the resume re-executes the
  pending call exactly once, importing 3,000 lines without duplicates.
- Live run (skipped `NOT RUN` without gateway env):
  `TestLiveSyntheticAdventureOnboarding` drives the real
  `qwen3-8-27b` (`GAME_NARRATOR_LIVE_MODEL`) over the full tool set against
  the synthetic game "Synthetic Adventure"
  (`internal/games/adapter/generic/testdata/synthetic-adventure.csv`, the
  adapter's own synthetic fixture — no game text, AGENTS.md rule 5), with
  real Exa MCP search behind `search.Budgeted` and the real SSRF-filtering
  fetcher. Verified live on 2026-10-17: the model imported the fixture,
  produced the source-quality report, used zero web calls, and stopped in a
  draft revision within the step budget (no publish).
- `go vet ./internal/agent/...`, `go test -race ./internal/agent/...
  ./internal/store/...` pass (store covers migration 0007→0008 on open).

## Consequences and reversal plan

- The model's power is now exactly the tool catalogue: every capability
  has a named MinRole, a per-run budget, and an envelope with
  `source_ids`, so the run log answers "what did the agent do, and under
  which sources?" for every side effect.
- Review-required tools (`identify_game` ambiguity, conflicts, voice
  auditions) end the run at the human boundary instead of proceeding; the
  resume path (ADR-0025) picks the run up after the human decides.
- Reversal: delete `internal/agent/tools` and migration 0008 (and the two
  store methods); the agent core (ADR-0025) has no dependency on the tools
  package, so nothing else changes.
