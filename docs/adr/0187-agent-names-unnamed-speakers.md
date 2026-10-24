# ADR 0187: The agent names the import's placeholder speakers: `name_speaker` + `list_unnamed_speakers` and the dedicated naming run

- Status: accepted (implemented)
- Date: 2026-10-24
- Task: speaker-naming (issue #41, "Voices")

## Context and constraints

On the real board (build 486619b), the Cast page's "Name speakers with the
agent" button started a *generic* onboarding run: its user request was
"Onboard the game …" and the run's four steps were identify_game,
preview_profile, source_quality — it named nobody. The Wind Waker BMG import
attributes lines to opaque per-message attribute ids; profileio (ADR-0070)
lands each one on a proposed placeholder character **named by the key**
(`ww:spk:12`). The cast audit then reports dozens of UNKNOWN rows, because
the agent tool registry (`internal/agent/tools`) has no tool that names an
existing placeholder: `stage_character` stages researched characters,
`analyze_character` briefs cited lines, and the curator's manual
identification is the speaker-link API, which the agent never calls.

Requirements fixed by the task: a naming tool pair the agent can drive;
a run whose prompt is the naming job (not generic onboarding); a step
budget scaled to the work; draft-only writes that end in the existing
review/approve flow; and the cast audit (ADR-0171) must pick the naming
up.

## Options

1. **Agent re-implements speaker-link (re-point + retire + assert).** Rejected:
   duplicating ADR-0070's transaction (message re-pointing, speaker
   assertions, retirement) inside a tool is a second source of truth for the
   same curation action, and the placeholder's lines already point at the
   placeholder row, so nothing needs re-pointing.
2. **Rename the placeholder row in place; list tool + naming run.** Chosen —
   smallest change that closes the loop: the placeholder IS the character
   row its lines point at, so renaming it names every line at once; the key
   folds into aliases exactly the way speaker-link folds it (ADR-0070), so a
   re-import binds the key straight to the named row; the row stays proposed,
   so the naming run's output is review work, never publication.
3. **A new REST endpoint the agent calls over HTTP.** Rejected: every other
   draft write is a direct store call inside the tool with the store's
   content-write gate; an HTTP hop adds an auth/boundary the tool layer does
   not need.

## Decision

**Tool pair** (`internal/agent/tools/speaker_naming.go`):

- `list_unnamed_speakers(profile_id)` — read-only, viewer-level,
  `UntrustedOutput` (it carries sample lines). The shared scan
  `tools.UnnamedSpeakers` lists every non-retired character whose name
  matches the opaque-key pattern `^[a-z0-9][a-z0-9._-]*:spk:[0-9]+$`, with
  per-speech line count and up to 5 sample lines (240-char truncation).
- `name_speaker(profile_id, speaker_key, name, gender_age, evidence,
  confidence)` — curator-level, draft side effect. It refuses a non-draft
  revision outright, finds the placeholder row (by name, or by alias after
  an idempotent retry), renames the row, folds the key into its aliases, and
  appends to the personality summary the voice-class phrase ("Voice class:
  adult male.") and the provenance sentence ("Named by the agent from speaker
  key ww:spk:12 (confidence 0.90): …"). A second call naming the same key
  with a *different* name is refused: overwriting a name is the curator's
  speaker-link path, not the run's. `gender_age` is one of
  `adult_male|adult_female|boy|girl|elder_male|elder_female|creature|unknown`.

**Why the summary:** the cast audit's `RowFor` reads `PersonalitySummary +
Role` (then the voice brief) as the expected-class profile text
(`internal/cast/audit.go`, ADR-0171); its keyword table recognises
"male/man", "female/woman", "boy", "girl", "man", "woman", "creature".
"unknown"/"creature" deliberately carry no class claim. No new audit input
was invented — the naming run writes into the field the audit already reads.

**Naming run** (`internal/api/onboarding.go`): `POST
/api/v1/onboarding/runs` gains `task: "name_speakers"` (the Cast button now
sends it). The handler validates the task, requires `profile_id` for it
(draft-checked by the existing path), and scales the attempt's step budget:
`4 + 2 × unnamed-speaker count`, capped at 48, carried on the job payload
(`agent.JobPayload.MaxSteps`) so a resumed attempt keeps the same ceiling.
The run's user request is a dedicated naming prompt (list → name each
identifiable speaker with evidence → leave the rest → summary → no
publish), not the generic onboarding prompt. The run ends in the existing
review flow: names are proposed rows on the draft, the curator approves on
the review surface, and the cast audit then classifies from the summary.

## Tradeoffs

- Rename-in-place means a naming run cannot *merge* two placeholders into
  one named character; that stays the speaker-link action. Deliberate:
  merging is a curation decision with a human, naming is a model job.
- The summary is a prose field; the provenance sentence lives in it rather
  than in a new column. No schema migration; the cast audit and the review
  UI both read summaries today.
- The key pattern is narrow (`ns:spk:N`): a placeholder whose import used a
  different key shape is simply not listed, and the curator's manual path
  still works. The pattern is one variable in one file.
- The step cap (48) means a whole-disc cast beyond ~22 unnamed speakers gets
  one run's worth of naming per attempt; the remaining placeholders are
  still listed, and a second run continues. The per-tool budget
  (`name_speaker` MaxCallsPerRun 80) is the backstop.

## Validation

- Tool unit tests (`internal/agent/tools/speaker_naming_test.go`, synthetic
  store, invented speakers/lines): listing excludes named and retired rows
  and counts correctly; naming renames the row, folds the alias, stores
  provenance, keeps the lines, leaves the profile a draft; retries are
  idempotent; a review revision is refused (the error names the draft-only
  rule); bad args (unknown key, key-shaped name, bad gender, confidence > 1,
  missing confidence, no such key) are refused; a second name for an
  already-named key is refused.
- Agent-loop test: a scripted model calls list_unnamed_speakers then
  name_speaker twice and stops → the run ends `succeeded`, both rows named
  with provenance stored, three tool-call rows on the run, profile still
  draft.
- Cast-audit test inside the naming tests: after a naming with
  `gender_age: adult_female`, `cast.ExpectedClass(summary+role)` returns the
  female class.
- API test (`TestOnboardingNamingRunScalesBudget`): the naming run's payload
  carries `max_steps = 4 + 2 × count` and the naming prompt (never the
  generic "Onboard the game" prompt); `task` without `profile_id` and an
  unknown task are 422; a generic run carries no override.
- OpenAPI contract: `OnboardingRunRequest.properties.task` added to the
  checked-in contract, the generated `schema.d.ts` regenerated, and the
  enum-vocabulary test pinned the value to the one handler constant
  `onbTaskNameSpeakers`.
- `go test -race ./internal/agent/... ./internal/api/... ./internal/cast/...`
  and `make lint` pass; `pnpm exec tsc -b` passes.

## Consequences / reversal

- The Cast button's run is now a real naming run: the tool call timeline
  shows the listings and each naming, and the review flow is where names
  land.
- Reversal: drop the two registry entries and the `task` branch; the cast
  audit, speaker-link and the roster are untouched by this change, so no
  data reversal is needed. The summary provenance sentences remain as
  editorial text on named rows (harmless).
