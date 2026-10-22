# ADR-0158: An unnamed speaker of a confident apparent kind voices through the generic kind character

Date: 2026-10-22
Status: accepted
Owners: agent worker (speaker-kind)
Related requirements/tests: docs/spec/02 (Speaker attribution is not necessarily a speaker-name), docs/spec/04 (never invent a speaker), docs/spec/05 (Clef adjudication and commit policy), docs/spec/07 (voice lifecycle), docs/spec/12 (correlation proof), ADR-0070 (Wind Waker speaker attribution from the BMG INF1 per-message attribute byte), ADR-0101 (portable voice mappings and narrator fallback), ADR-0135 (Cast UI / auto-cast character names), ADR-0154 (full-frame Clef decision), ADR-0156 (speaker attribution beyond slots). Tests: `internal/providers/systemone` (adjudication), `internal/pipeline` (decision reply), `internal/session` (decision message / commit payload), `internal/runtime` (speaker gate, commit path), `internal/runtime/speech_test.go`, `internal/runtime/speech_fallback_test.go`, `internal/runtime/speaker_attribution_internal_test.go`.

## Context and constraints

ADR-0156 made the adjudicator (which reads the full frame, ADR-0154) the per-occurrence speaker identity: a speaker named at or above `SpeakerConfidenceFloor` (0.5) overrides the Wind Waker's shared BMG speaker slot; a slot that stays shared with an unconfident or unknown answer falls back to the neutral narrator voice instead of trusting the curator's slot→character link.

The 2026-10-22 live play that ADR-0156 fixed exposed the next shape of the same defect: the slot is shared by *many* NPCs, and when the adjudicator could not confidently name the speaker, the honest degraded mode was the narrator voice — including for an unnamed villager the owner knew was a man. The owner asked for the second half of the fix: an unnamed villager should at least sound like what they are. The owner must not hear a woman for a man, a child for an old man, and so on.

The BMG still carries no per-NPC identity (ADR-0156's finding stands), so the kind is the next evidence source the frame can carry: the adjudicator's answer to "what apparent kind is this speaker?". The kind question is answered from the screenshot, exactly like the speaker answer; the kind is the neutral ground the generic voice rides, because a kind is coarser than a name and is safe to infer from the frame without inventing a person.

## Research findings (step 1 of the build)

What the adjudicator's kind answer can be, and how it maps onto voices:

- The speaker_kind choice question carries the apparent-kind options `adult_male`, `adult_female`, `boy`, `girl`, `elder_male`, `elder_female`, `creature`, plus the `unknown` fallback. The ids are the wire option ids ([a-z0-9_]{1,32}, docs/spec/06) and the roster's opaque kind keys are the same ids under the `kind:` prefix.
- `systemone.Interpret` maps the chosen option's probability onto `Adjudication.SpeakerKindConfidence`; the runtime's kind gate reads it against the same `SpeakerConfidenceFloor` the speaker gate applies, so the two "sure" judgements read at one threshold.
- The kind answer rides the same plumbing as the speaker confidence: `systemone.Decision` → `pipeline.DecisionReply.SpeakerKind` / `SpeakerKindConfidence` → `session.DecisionMsg.SpeakerKind` / `SpeakerKindConfidence` → the `message.committed` payload's `speaker_kind` / `speaker_kind_confidence` → the speech wiring's `commitPayload` → `commitVoice`.

## Options

1. **A confident kind routes the line through a roster character whose aliases carry the opaque `kind:<kind>` key.** Adopted.
2. **A confident kind answers with a dedicated generic voice per kind, minted on the fly.** Rejected: the product already has a voice model per character (ADR-0101, ADR-0135); a per-kind character row carrying `kind:<kind>` as an alias keeps one roster, one voice registry and one consent path, and needs no new store shape or migration.
3. **A confident kind answers with silence.** Rejected: the owner explicitly wanted the line voiced, and silence is the honest answer only when the kind could not be established; a confident kind is established.
4. **Infer the kind locally from the frame without the adjudicator.** Rejected: the kind is still a claim about a person, and the local path's evidence (OCR + placement) is not the adjudicator's frame reading (ADR-0154) — the same class of invented evidence ADR-0070's dump parser avoided.

## Decision

- `BuildAdjudication` gains one `speaker_kind` choice question, answered from the screenshot, with the kind options above and the `unknown` fallback; the existing questions and the positional option contract (spN/cN aliases) are unchanged.
- `Interpret` maps the kind answer into `Adjudication.SpeakerKind` (the chosen option id, `unknown` when the kind could not be established) and `Adjudication.SpeakerKindConfidence` (the chosen option's probability; zero when absent).
- The kind gate reads the same `SpeakerConfidenceFloor` (0.5): a kind named below it is not a kind, and the line keeps the narrator fallback.
- When the speaker gate falls back (a shared slot whose answer could not name the speaker, or an unattributed line) and the kind is confident and the roster carries a character whose aliases include the opaque `kind:<kind>` key, the line's voice is that character — `commitVoice` resolves the kind character, and the existing `resolveVoiceForCharacter` resolves that character's approved voice (falling back to the narrator when the kind character has no usable voice; the degraded mode, not a substitute).
- A confident named speaker still wins over the kind; a single-character slot keeps its assignment (the kind never reaches it).

## Tradeoffs

- The commit payload grows two fields (`speaker_kind`, `speaker_kind_confidence`); the kind answer rides the same confidence plumbing the speaker confidence already takes, so the developer UI's decision row can show the kind alongside the choice.
- The kind voice is generic (one voice per kind per profile), not per-NPC: the kind is coarser than a name, and the owner accepted that for unnamed villagers; the per-NPC voice stays the confident-named-speaker path.
- A kind-routed line is distinguishable from a narrator line through the utterance's `voice_route` on the speech lifecycle events (`speaker`, `speaker_kind`, `narrator`, `assigned`).

## Validation

- `go test ./internal/providers/systemone/ ./internal/pipeline/ ./internal/session/ ./internal/runtime/`
- New tests with invented data: confident kind + kind character → that voice; unconfident kind → narrator; no kind character → narrator; a confident named speaker still wins over kind; single-character slot unchanged.
- `systemd-run --user --scope -q -p MemoryMax=4G go test -race -timeout 20m ./internal/providers/systemone/ ./internal/pipeline/ ./internal/session/... ./internal/runtime/`
- `systemd-run --user --scope -q -p MemoryMax=4G make test-replay` (junit line: `junit: 142 PASS, 0 FAIL, 0 SKIP, 0 NOT RUN`).

## Reversal plan

Revert the kind question and the kind gate; the ADR-0156 speaker gate alone decides, and an unconfident or unknown speaker falls back to the narrator as before. No schema change is involved.

## Consequences

- Unnamed villagers on shared slots now sound like the kind the frame established, instead of always the curator's linked character or the narrator.
- The narrator fallback remains the honest degraded mode for lines whose kind is unknown or unconfident.

## Links / references

- ADR-0070, ADR-0101, ADR-0135, ADR-0154, ADR-0156, ADR-0109.

## Amendment 2026-10-22: a kind voice without a roster character

There is no API to create a profile character carrying a `kind:<kind>` alias: speaker-link only renames import placeholders. So the kind route also resolves the opaque key itself.

At session start, `speechWiring.addKindVoiceKeys` checks each named kind that no roster character claims. If a voice is approved under the character key `kind:<kind>` (for example `kind:boy`), that kind routes to the key, and the voice resolves through the ordinary approved-voice lookup.

The operator sets these up once, through the existing voices API (audition, approve, register), using `kind:adult_male` and so on as the character name. No profile edit is involved.

A roster character carrying the alias still takes precedence. A kind with neither keeps the narrator fallback.

Test: `TestKindVoiceKeyWithoutRosterCharacter`.
