# ADR-0046: Research-grounded character staging and voice briefs (web-only onboarding)
Date: 2026-10-18
Status: accepted
Owners: agent worker
Related requirements/tests: docs/spec/03 (tool registry, voice design workflow), docs/spec/04 (speaker evidence), docs/spec/07 (auditions), `TestLiveOnboarding` (internal/e2e)

## Context and constraints

The onboarding chain is `add game → agent web research → character list → voice briefs → VoiceDesign auditions` (docs/spec/03). Two real gaps break it for a web-research-only onboarding run (no user-supplied script):

1. **No tool stages researched characters into the roster.** Characters entered a profile only through `parse_dialogue` (speaker labels in an imported script). A run that researched a game purely on the web produced sources but no `characters` rows, so there was nothing for `analyze_character` to act on. docs/spec/03's DAG names this step ("infer characters+roles"); the ADR-0026 tool set did not implement it.
2. **`analyze_character` hard-blocked without cited lines.** Its contract was "cited lines only"; a character with no extracted messages failed with "import a labelled script first". For a web-only run that is the end of the chain: no brief, no audition.

docs/spec/03 constrains the fix: "every factual claim about game content must attach one or more `source_id` references"; "speaker identity must be based on direct source attribution … unknown is acceptable"; "character descriptions may be imaginative for voice auditions but cannot masquerade as verified lore". docs/spec/07 requires the brief to drive real VoiceDesign auditions. docs/spec/14 keeps fetched page content untrusted and out of model prompts.

## Decision

Two minimal changes in `internal/agent/tools`:

1. **`stage_character` tool** (new, `characters.go`). Inputs: `profile_id`, `name`, optional `role`/`personality_summary`, required `source_ids` (1–8). Every `source_id` must be a provenance row already registered on the profile (the run retrieved or imported it); the character is upserted with `status=proposed` and `verified_source_refs` set to those ids. Same-name (case-insensitive) restage updates the row in place. Side-effect class `draft`, `RoleCurator`, bounded budget — the same gates as the other content tools.
2. **Research-grounded mode in `analyze_character`** (`voice.go`). When the character has no cited lines *but* carries staged source grounding whose refs resolve to source rows on the profile, the brief is produced from the researched character facts (name, role, recorded summary) plus the *metadata* of the evidence sources (id, URI, rights, retrieval time — never page content, which is untrusted per docs/spec/14 and was never quoted into the prompt). The model call keeps the JSON-mode fallback; every creative field is still labelled `creative:true`; the result carries a `grounding` block (`mode: researched_character`, `source_ids`, note). With lines present, behaviour is unchanged apart from the added `grounding` block (`mode: cited_lines`). A character with neither lines nor resolvable grounding still fails with the existing lines-first instruction.

## Alternatives considered

- **Require a labelled script before any brief.** Rejected: it makes web-only onboarding (the user's explicit goal for a public game like Wind Waker) impossible to complete, contradicting docs/spec/03's "its job is not to guarantee full dialogue coverage when evidence is missing" — the profile must be useful with what the evidence supports.
- **A dedicated `research_brief` tool.** Rejected as non-minimal: it would duplicate the model-call, JSON-validation and creative-labelling machinery of `analyze_character` and give the UI two brief sources of truth. One tool, one brief shape, two evidence modes is the smaller surface.
- **Quoting fetched page content into the brief prompt.** Rejected: docs/spec/14 treats retrieved text as untrusted data; the brief's evidence needs provenance (which source, which rights status), not the bytes. Metadata-only evidence also keeps the prompt bounded.

## Consequences and risks

- A brief with an empty or non-string creative field is refused by the tool (validation error back to the model); the agent loop re-runs the call with the error in context, so the brief that drives an audition is complete by construction rather than silently degraded. Direct (non-loop) callers retry the call a bounded number of times.
- Web-only profiles can now reach auditions with honest provenance: every character row and every brief names the sources it was built from, rights status recorded per source (web sources are `unknown`, low trust — the publication gate still sees them).
- Risk: a brief built from a low-trust wiki page may be factually wrong. Mitigation is the existing one — creative fields are labelled `creative:true`, grounding is inspectable, and publication still requires the human approval boundary; the brief is editable before auditions (docs/spec/03 "structured editable voice_brief").
- `stage_character` writes draft revisions only; the store's draft-revision gate refuses it on a published profile, so the tool cannot bypass the publication boundary.
- The model prompt for the researched mode is configuration-derived (new constant), never built from web output, per the agent core's prompt rule.

## Evidence and test plan

- Unit tests (`internal/agent/tools/characters_test.go`): staging happy path with source grounding; refusal of an unregistered source; restage-in-place (no roster fork); research-grounded brief (grounding block, empty `cited_lines`, all creative fields labelled, prompt names the evidence, JSON-mode fallback kept); the lines-first failure survives for an ungrounded character; cited-lines mode regression.
- Live end-to-end (`internal/e2e/onboarding_live_test.go`): real gateway model, real Exa MCP web search, Wind Waker research → `stage_character` staging → research-grounded briefs → real VoiceDesign auditions via the voices service.

## Migration / rollback

No schema change (`verified_source_refs` already exists on `characters`). Rollback: remove the `stage_character` registration and the researched-mode branch; `analyze_character` returns to lines-only behaviour. Profiles staged with the tool remain valid rows (status `proposed`); nothing is published by either change.

## Follow-ups

- The production agent wiring (`cmd/game-narrator/main.go`) still passes `TTS: nil` to the tool deps, so `design_voice` cannot audition inside a live run; auditions run through the voices workflow (`voices.Service.CreateAuditions`, the UI path). Wiring a VoiceDesign client into the agent deps is a follow-up for the runtime worker (outside this task's boundary).
- Revisit once `infer_speakers`/`build_graph` land (docs/spec/03): researched staging should feed, not replace, script-grounded speaker evidence.
