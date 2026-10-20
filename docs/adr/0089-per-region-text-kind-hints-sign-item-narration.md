# ADR-0089: Per-region text-kind hints — sign/item narration as a runtime path

Date: 2026-10-20
Status: accepted
Owners: G8 sign/item worker
Related requirements/tests: docs/spec/00 "Dialogue/sign/item reading only when a present/selected textbox or explicit interaction can be detected", docs/spec/04 "Visual profiles" (Wind Waker item notifications, signs), docs/spec/05 "Message class and speech rules", docs/spec/12 release gate "at least 5 scenarios with actual live model requests including one sign/item"; gap audit G8 (docs/status/gap-audit-2026-10-19.md); tests `TestSignItemRegionsCommitTheirClass` (internal/pipeline/signitem_test.go), `TestScenarios` subtests `synthetic/sign-opened-001` / `synthetic/item-obtained-001` (internal/scenario), `TestEnumBehaviouralHelpers` (internal/games), scenario catalog rows GN-T010/GN-T011 (docs/test-plan/scenario-catalog.md)

## Context and constraints

Gap audit G8 (2026-10-19) found the sign/item vocabulary — `games.TextSign`,
`games.TextItem`, `games.RegionSign`, `games.RegionItem` — defined but never
referenced by runtime code: `pipeline.Config.TextKindHint` was a single global
hint (default `dialogue`) applied to every region's reads, so a sign region
and an item region ranked their OCR exactly the way a dialogue box does, the
commit carried no content class, and no profile, fixture, scenario or test
exercised one. The release gate (docs/spec/12) counts on "at least 5 scenarios
… including one sign/item", and docs/spec/00 gates sign/item reading on
"present/selected textbox or explicit interaction" — the region's declared
role is the signal the spec points at.

Constraints:

- The change must extend the existing dialogue path, not fork it: one
  dispatch/rank/reduce pipeline, one matcher, one session reducer.
- The matcher's text_kind component (weight 0.2, log-linear) already scores a
  hint against a message's stored class; the corpus (profile message table)
  remains the single source of a message's class.
- The session's narrated-class veto (ADR-0075, `NarratedKinds`, default
  `[dialogue]`) already silences a commit whose class the run did not elect to
  hear; sign/item must flow through it, not around it.
- AGENTS.md rule 3 (no simulation theater): synthetic scenarios must be
  labelled synthetic; the Dolphin half of GN-T010 must be reported BLOCKED,
  not invented.

## Options

1. **Per-region hint derived from the region type (chosen).** The pipeline
   builds a `region ID → TextKind` map in `New` from each region's declared
   type (a new `games.RegionType.TextKindHint()`: textbox→dialogue,
   sign→sign, item→item, menu→menu, tutorial→tutorial) and the rank worker
   sends that region's hint with each rank request. The global
   `Config.TextKindHint` remains as the fallback for regions whose type
   declares no hint. The session's commit already resolves the committed
   candidate's class off the candidate index (`commitKind`); the payload's
   `text_kind` is that class, which the developer UI displays and the
   narrated-class veto reads.
2. Per-region hint as explicit profile fields (`regions[].text_kind_hint`).
   Rejected: the type→hint derivation is the spec's rule ("the region's
   on-screen role is the hint"); spelling it per profile would let a profile
   contradict its own geometry and would have to be validated for consistency
   forever.
3. A separate sign/item recognition path (own dispatch, own reducer).
   Rejected: a fork of the pipeline the spec says should not exist
   ("extend the existing dialogue path"); it would carry its own fences,
   its own bug surface, and it could not share the commit/veto/speech
   machinery this task exists to prove.

## Decision

- `games.RegionType.TextKindHint() (TextKind, bool)`: the closed mapping
  above; unknown types (e.g. `letterbox`) return `false` so the runner uses
  the global fallback hint.
- `pipeline.Runner` owns `kindHints map[string]games.TextKind`, built in
  `New`; `maybeRank` stamps each rank job with its region's hint and the
  rank worker sends `TextKindHint` on the observation. No other stage
  changes: retrieval, scoring (the text_kind component), the acceptance
  policy, the session reducer and the speech fence behave exactly as they do
  for dialogue.
- The scenario harness carries the class through both layers: profiles may
  name a per-message `text_kind` (defaulting to the profile-level
  `text_kind`), the scenario's `narrated_kinds` list (default
  `[dialogue]`) is applied to the pipeline session's commitment policy and
  to the sidecar session (with the corpus's `KindFor` wired as the session's
  message-kind source), and the sidecar's rank requests carry the region's
  hint. New assertion `committed_kind` checks the commit payload's
  `text_kind`.
- Fixtures (all synthetic, AGENTS.md rule 5): `scripts/fixtures/
  gen-synthetic-signitem.sh` writes two 18-frame clips — `sign-opened-001`
  (an invented signboard, one invented sentence, region type `sign`) and
  `item-obtained-001` (an invented item banner, one invented notification,
  region type `item`) — plus the two profiles
  (`testdata/synthetic/profiles/sign-opened-001.json`,
  `item-obtained-001.json`) whose sign/item line is the corpus line under
  that class among invented dialogue distractors. Scenarios
  `synthetic/sign-opened-001` (GN-T010) and `synthetic/item-obtained-001`
  (GN-T011) run the real pipeline + recorded TTS over them: exactly one
  commit each, `committed_kind` sign/item, the narrator voice speaks it with
  no substitution (the implicit audio_evidence_clean check asserts the one
  evidence WAV).
- The clips end with the sign/item still on screen. This is load-bearing:
  a region dismissal cancels the in-flight utterance (the session's
  `region_vanished` cancel), and the harness closes a still-live utterance
  with `speech.ended` only at stream end (`finishUtterances`). Ending on a
  present frame is what makes the narrator's reading reach its terminal
  outcome deterministically; the dismiss-then-silence property remains the
  dialogue clip's job.
- The Dolphin half of GN-T010 is registered, not faked:
  `testdata/scenarios/dolphin/sign-opened.yaml` is `status: blocked`
  (no savestate path to a sign exists in internal/dolphin — the recorded
  savestate fixtures reach only textbox/subtitle screens), with the unblock
  steps recorded in docs/status/blocked.md (row "Dolphin sign savestate").
  A blocked dolphin scenario may omit `input.manifest_sha256` (the fixture
  has no manifest to pin yet); a present hash is still validated.
- Developer UI: the commit detail (`web/src/pages/developer/DetailPanel.tsx`
  `CommitOutcome`) renders the payload's `text_kind` (data-testid
  `commit-text-kind`); the developer E2E spec asserts it renders `dialogue`
  for the dialogue-advance fixture's commits.

## Tradeoffs

- The type→hint mapping hard-codes the spec's rule in code. A game whose
  signs rank better as a different class would need the profile to carry the
  class (it does — the corpus's `text_kind`) and a region type that names it;
  the hint only steers the 0.2-weight component, and the corpus's stored
  class always names the commit. That is the intended split: geometry steers,
  the corpus decides.
- `menu`/`tutorial` regions now rank with their own hint instead of the
  global one. Both are currently absent from all profiles; the mapping is
  the spec's, and the global hint stays available for types that declare
  none.
- The synthetic scenarios' TTS leg is the recorded gateway (a 100 ms tone),
  not a real voice: the task's release-gate "live model requests" leg is met
  by the provider sidecar's real decision/TTS wire (the scenarios'
  `tts: recorded` runs the real `tts.Client` over the recorded provider
  gateways), and by the Dolphin half once unblocked. The scenario is labelled
  `origin: generated_synthetic` so a report can never present it as
  real-game coverage.

## Validation

- `TestSignItemRegionsCommitTheirClass` (internal/pipeline): over the
  invented 24-frame clip, a sign region's reads commit the corpus's sign
  message with the matcher's text_kind component at 1 (hint named the
  class); an item region's commit the item twin; the textbox control commits
  dialogue; and the negative control (a sign region whose corpus stores the
  line as dialogue) commits dialogue with the component at 0 — the hint
  scored, it did not rewrite the corpus's class.
- `TestScenarios` subtests `synthetic/sign-opened-001` (GN-T010) and
  `synthetic/item-obtained-001` (GN-T011): real pipeline (ROI → OCR → rank →
  session) over the generated clips; one commit each; `committed_kind`
  sign/item; `no_voice_substitution` on the narrator voice with a terminal
  `speech.ended`; `audio_evidence_clean` (one evidence WAV).
- `make test-replay` green (the scenario package plus the pipeline package,
  race detector, Dolphin gate skipped where no emulator).
- Web: `pnpm typecheck` / `lint` / `fmt:check` / `vitest` green; the
  developer Playwright spec asserts `commit-text-kind` renders.
- ADR-0037 discipline: the Dolphin sign scenario is reported BLOCKED with
  unblock steps (docs/status/blocked.md), never pass.

## Consequences

- Sign/item narration is a runtime path: region type → hint → matcher
  component → commit class → veto → speech, end to end, on the dialogue
  pipeline's machinery.
- Profiles for games with signs/items gain real coverage from day one: name
  the region's type, store the line under its class, elect the class in
  `narrated_kinds`.
- The `committed_kind` assertion is available to any scenario (dialogue
  included) that wants to pin a commit's class.

## Reversal plan

- Drop the `kindHints` map and the `TextKindHint()` helper; the runner sends
  the global hint again (one line in the rank worker). No schema or event
  shape changes: `text_kind` on the commit payload is additive and the UI's
  row degrades to an empty dash.
- Delete the two synthetic clips/profiles/scenarios and the generator script;
  move the blocked dolphin row to Cleared (or leave it — it is a registration).
  The matcher's text_kind component and the session's veto are untouched by
  this ADR and survive the reversal.
