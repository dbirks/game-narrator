# ADR-0132: The scenario harness resolves per-character voices — the synthetic two-voice regression for gap G3

Date: 2026-10-21
Status: accepted
Owners: agent (issue #36 task)
Related requirements/tests: AGENTS.md (rule 3 "No simulation theater"; "File improvements as GitHub issues"), docs/spec/00 (mission: "synthesizes distinct, consistent character voices"), docs/spec/07 (voice lifecycle; FR-08), docs/spec/12 ("Frame replay"; "Fixture origins"), docs/spec/17 ("E2E scenario YAML"), docs/test-plan/scenario-catalog.md (GN-T007, GN-T016, GN-T020 — the per-voice rows the synthetic mode owes), docs/status/acceptance-2026-10-20.md (gap row G3), ADR-0037 (blocked ≠ pass), ADR-0101 (narrator fallback), ADR-0122 (catch-up narration; the shared speaker-run split this extends), issue #36.
Tests: `make test-replay` (the new scenario `synthetic/two-voices-001` runs in `TestScenarios`), `go test -race ./internal/scenario/...`, `scripts/fixtures/gen-synthetic-two-voices-replay.sh` (byte-identical re-run), the loader and evaluator unit tests added to `internal/scenario`.

## Context

The 2026-10-21 acceptance audit (issue #36, gap row G3 "Per-character voices
actually speaking in play") verified the resolution path end to end —
`resolveVoice`/`resolveVoiceForCharacter` (`internal/runtime/speech.go`), the
`speakerByMessage` built from the published profile's `SpeakerID`, the
narrator fallback (ADR-0101) — and one live proof: `TestLivePerCharacterVoices`
ran once on 2026-10-20 (two characters, two designed voices, 5/5 ASR-QA, the
WAVs differ; bundle in `artifacts/per-character-live/`, gitignored).

What is missing is the regression leg:

1. **No synthetic test asserts the behaviour.** Every `internal/runtime`
   speech test seeds the narrator only. The scenario harness — the CI gate's
   synthetic layer (`make test-replay`, category (a) synthetic stubs) —
   speaks every line of every scenario with ONE voice: `run.voice`, the
   scenario's `inference.voice`. A regression in per-character resolution
   (a `speakerByMessage`/fallback mixup, a merged run resolving the wrong
   speaker) would surface only in the live run, which is manual and one-shot
   (`TestLivePerCharacterVoices` matches none of `make test-dolphin`'s
   `-run` patterns).
2. **The harness cannot express two voices at all.** The scenario schema has
   no per-character voice field, the harness has no speaker→voice
   resolution (only the speaker *label* table the catch-up merge reads), and
   no assertion type can say "this line's TTS request named this character's
   voice".

## Options

1. **An `internal/runtime` synthetic test with two approved voices** (the
   issue's suggested shape): extend the `seedNarratorVoice` fixture pattern,
   publish a profile with two characters, approve one voice each, commit one
   line per character, assert distinct `voice_id` per line. Pro: exercises
   the real `resolveVoiceForCharacter` and the real voice registry. Con: it
   is a unit-scale wiring test, not the end-to-end commit→TTS→evidence-WAV
   chain; it does not run in the replay gate; and the task for this change
   asks for the scenario leg (the gate leg of G3), which is the one the
   acceptance report counts.
2. **Extend the scenario harness with per-speaker voices** (this ADR): a
   `voices` mapping in the scenario YAML (speaker label → approved voice
   name), the harness resolving each utterance's voice from the line's
   speaker the way the runtime does (the character's voice, the narrator's
   when the line is unattributed or the speaker has none), a new
   `speech_voice` assertion, and a new three-box synthetic clip. Pro: the
   full commit→per-character TTS request→evidence-WAV chain runs in CI under
   the replay gate, deterministically, with only invented content; it is the
   same evidence shape the report already shows (events + sink WAVs). Con:
   the harness keeps its established parallel implementation of the
   runtime's resolution at harness scale (the same cut every other harness
   behaviour takes — the runtime's own resolution is covered by its unit
   tests and the live E2E).
3. **Neither; wait for #25's test-dolphin wiring** to put the live test in a
   gate. Con: the issue's whole point is that the live proof is one-shot and
   manual; a regression between live runs stays invisible in CI.

## Decision

Option 2, additive and bounded:

1. **Schema: `voices` (top-level, optional).** A mapping of speaker label to
   voice name, keyed by the same speaker labels the scenario's `speakers`
   block declares. Every value must pass the registry's name validation
   (`tts.ValidateName` — the production `gn_…` scheme or the automated-test
   `gn_test_<hex8>` scheme; a voice the registry would refuse is a load
   error, the loader's usual "a promise nobody can keep is a typo"). The
   block is allowed only with `catch_up: catch_up`: the speaker attribution
   it keys on is, by its own documented contract, read only under that
   policy, and a `voices` block under `latest_only` would be dead config.
   The narrator's voice stays `inference.voice` — the scenario's default —
   exactly as ADR-0101's fallback keeps the narrator's voice the profile's
   default: the loader refuses a `narrator` entry in `voices` so the
   narrator's voice has exactly one source.
2. **Harness resolution (`internal/scenario/run.go`).** A committed line's
   speaker is the scenario's `speakers` attribution or `narrator` (the
   existing `speakerFor`); the utterance's voice is the `voices` entry for
   that speaker or the narrator voice (`voiceFor`). The resolved voice rides
   every record of the utterance: the TTS request (`tts.GenerateRequest`'s
   `voice`), the session's ready/started/ended lifecycle events (`voice_id`),
   the catch-up merged run's request and started event, and the cache key.
   The immediate path (`speak`) and the catch-up merged runs (`speakMerged`)
   both resolve; under `latest_only` and without a `voices` block the
   behaviour is byte-for-byte the historical one (every speaker resolves to
   `narrator` → `inference.voice`), so no pre-existing scenario's graded
   behaviour changes.
3. **Assertion: `speech_voice { message_id, voice }`.** Every
   `speech.started` record that covers the line — the session's immediate
   started or the merged run's started with its covered ids, the same
   `coversMessage` rule `speech_spoken_count` uses — must name the expected
   voice, and at least one such record must exist. A line that resolved to
   another character's voice, or fell back to the narrator's when its
   character has an approved one, fails with the foreign voice named.
4. **Evidence: per-utterance WAVs.** The implicit `audio_evidence_clean`
   check is strengthened from the aggregate count to per-utterance presence:
   every started occurrence (immediate and merged) must have an evidence WAV
   whose file name carries the sink's `SafeName` of the occurrence — a
   started utterance with no WAV is lost evidence even when the total count
   happens to match.
5. **Fixture.** `scripts/fixtures/gen-synthetic-two-voices-replay.sh`
   generates `testdata/synthetic/replay/two-voices-001`: 18 frames at 10 fps,
   three textboxes (each revealed in one typewriter step and held three
   frames), two invented characters and one unattributed line, all text
   invented for the script, `origin: generated_synthetic` on every truth
   row, byte-identical re-runs (fixed seed, no date chunks, 20-colour
   palette, no dithering) the way the other synthetic generators do. The
   profile `two-voices-001` carries the three real lines plus invented
   distractors. The scenario `synthetic/two-voices-001` runs `catch_up:
   catch_up` with a tone (1000 ms) longer than the clip's 400 ms commit
   cadence, so the second and third lines commit while the previous
   utterance is still playing and speak back-to-back — line 1 immediately in
   its character's voice, lines 2 and 3 as two separate speaker runs in
   their own voices (the second unattributed, spoken by the narrator's).

## Tradeoffs

- The harness re-implements the resolution at harness scale instead of
  calling the runtime's: consistent with the harness's existing design (its
  catch-up batcher mirrors the runtime's wiring "at harness scale", ADR-0122)
  and keeps the synthetic gate free of the runtime's store/registry
  dependencies. The two implementations stay in sync by shape, not by code —
  the residual risk (the harness resolving while the runtime drifts) is the
  one the live per-character E2E and the runtime unit tests keep covered.
- `speech_voice` reads the timeline (the started record carries the voice
  the harness requested), not the raw gateway request: the same evidence
  standard every other harness assertion uses; the client's wire payload is
  pinned by the provider client's own tests.
- `voices` is gated to `catch_up`, so a `latest_only` scenario cannot declare
  per-character voices. That matches the `speakers` contract today; if a
  scenario ever needs per-character voices under `latest_only`, the gate
  lifts (a one-line loader change), no schema reversion.

## Validation

- `go test -race ./internal/scenario/...` — the new scenario passes in
  `TestScenarios` (its assertions: three commits, three distinct voices —
  the two characters' `gn_test_` names and the narrator's, each line's
  started record naming its own; no line resolving to another's voice or to
  the narrator's; the unattributed line spoken by the narrator; per-utterance
  evidence WAVs; no stale or duplicated speech).
- `make test-replay` green on a fixture-less CI host (the scenario is
  synthetic; no Dolphin fixture involved).
- The generator re-runs byte-identical (SHA-256 of every artifact unchanged).
- Loader unit tests: a `voices` value the registry scheme refuses is a
  line-numbered load error; a `voices` block under `latest_only` is a load
  error; a valid two-voice block loads.

## Consequences

- The scenario schema (v1) gains one optional top-level field and the
  assertion vocabulary one type; both are additive — every pre-existing
  scenario loads and grades unchanged.
- Gap G3's synthetic leg is in the replay gate; G3 closes (PARTIAL →
  IMPLEMENTED) when the live leg's gate wiring lands with #25.

## Reversal plan

Delete the scenario YAML, the profile and the fixture dir, the generator
script, and revert the three harness files (`scenario.go`, `run.go`,
`assertions.go`/`evaluate.go`); `audio_evidence_clean` returns to its
aggregate form. No data, wire or store change — the reversal is a clean
revert with no migration.
