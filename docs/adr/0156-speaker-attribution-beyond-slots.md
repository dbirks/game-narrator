# ADR-0156: Speaker attribution beyond the Wind Waker's shared BMG speaker slot

Date: 2026-10-22
Status: accepted
Owners: agent worker (speaker-attrib)
Related requirements/tests: docs/spec/02 (Speaker attribution is not necessarily a speaker-name), docs/spec/04 (never invent a speaker), docs/spec/05 (Clef adjudication and commit policy), docs/spec/07 (voice lifecycle), docs/spec/12 (correlation proof), ADR-0070 (Wind Waker speaker attribution from the BMG INF1 per-message attribute byte), ADR-0101 (portable voice mappings and narrator fallback), ADR-0154 (full-frame Clef decision), ADR-0152 (advance prompt / mid-line pauses).
Tests: `internal/providers/systemone` (adjudication), `internal/pipeline` (decision reply), `internal/session` (decision message / commit payload), `internal/runtime` (speaker gate, commit path), `internal/runtime/speech_test.go`, `internal/runtime/speech_fallback_test.go`.

## Context and constraints

The Wind Waker adapter (ADR-0070) attributes every imported line through the
BMG's INF1 per-message attribute byte: a u32 text offset plus a fixed set of
attribute bytes (entry size 24 on the real disc, uniform across all 4,411
entries, INF1 order equal to pool order), with attribute byte 0 as the speaker
slot and the low-cardinality side bytes (7, 11, 12, 18) as per-speaker
presentation properties. The parser keeps the bytes opaque (ADR-0029) and the
adapter emits one opaque key per line (`ww:spk:<n>`).

The live play of 2026-10-22 showed the slot is not a speaker identity: the owner
talked to a male villager and he spoke in Aryll's voice. The session's
speech-ready readout named the character Aryll for that villager's lines, and
the profile's speaker link carried the placeholder keys `ww:spk:11` and
`ww:spk:3` to Aryll (aliases on her character row) — so every message carrying
slot 11 or slot 3 spoke as Aryll.

The 2026-10-22 live evidence is the defect this decision fixes. The BMG speaker
attribute is evidently a shared slot (portrait / voice-type / camera) rather
than a per-NPC identity: many different NPCs use the same slot.

## Research findings (step 1 of the build)

What the BMG INF1 entry carries, and whether any other field is a better
per-NPC identity:

- Entry size 24 (u32 text offset + 20 attribute bytes), uniform across all 4,411
  entries; INF1 order equals pool order (0 inversions). Speaker = attribute byte 0.
- The side fields near byte 0 (bytes 7, 11, 12, 18) are near-functions of the slot
  value and are per-speaker presentation properties, not the speaker.
- No in-text speaker marker exists (all 145 control-tag families carry a constant
  first argument; the sparse voice/emotion tag groups occur on ~1.4% of messages).
- No other attribute, message-id range, file or group was found in the parser or
  the adapter that carries a per-NPC identity; the adapter keeps the mapping
  opaque (ADR-0029, DoD 6) and the parser invents no names (ADR-0070).

Counts over the board's exported script (`/tmp/board-cast/rev3_messages.json`,
counts only, no game text recorded):

- 4,331 message rows; 4,320 with source-field attribution evidence; 50 distinct
  speaker keys; slots `ww:spk:<n>` for n in 0..56 observed, with 16 to 199 messages
  per key.
- Slots `ww:spk:11` and `ww:spk:3` carried 67 and 44 lines respectively; both were
  linked to the same character (Aryll) by the curator, so every one of those lines
  voiced as Aryll — including the male villager's.

Conclusion: no better per-NPC field exists in the imported data, and the
per-occurrence identity has to come from the adjudicator that reads the frame.

## Options

1. **Trust the adjudicator's per-occurrence speaker answer when it is confident.**
   Adopted.
2. **Lower the fallback threshold / use the raw speaker id.** Rejected: the raw id
   is the slot key, which is exactly the non-identity the live play exposed.
3. **Keep the slot→character link and add a per-NPC roster.** Rejected: no source
   field provides per-NPC identity, and inventing one would violate ADR-0070's
   "never invent a speaker".

## Decision

- The adjudicator's speaker answer is gated on confidence: a speaker named at or
  above `SpeakerConfidenceFloor` (0.5, the same half-threshold the safe-to-speak
  gate already applies) is a name; a speaker named below it is not, and the line
  falls back instead of trusting the slot→character link.
- When the answer is confident, it overrides the slot→character assignment for
  that occurrence (the per-occurrence override), so the line's voice follows the
  speaker the frame actually named.
- When a slot is shared and the answer is not confident, the line's voice falls
  back to the neutral narrator voice — not to the slot's linked character.
- When a slot maps to a single character, the assignment stands (matched-line
  behaviour unchanged).

Confidence plumbing (so the answer's confidence reaches the commit that decides
the line's voice): `systemone.Adjudication.SpeakerConfidence` (the chosen option's
probability) → `pipeline.DecisionReply.SpeakerConfidence` → `session.DecisionMsg
.SpeakerConfidence` → the `message.committed` payload's `speaker_confidence` →
the speech wiring's `commitPayload.SpeakerConfidence` → `speechWiring.commitSpeaker`.

## Tradeoffs

- The commit payload grows one numeric field; the developer UI's decision row can
  now show the speaker answer's confidence alongside the choice.
- A shared slot with an unconfident answer silently voices through the narrator:
  that is the honest degraded mode (ADR-0101), not a substitute.

## Validation

- `go test ./internal/providers/systemone/ ./internal/pipeline/ ./internal/session/ ./internal/runtime/`
- New tests with invented data: a shared slot used by two characters — the
  adjudicator's confident speaker wins; low confidence falls back to the neutral
  voice, not the slot's linked character; a single-character slot keeps its voice.

## Reversal plan

Revert the confidence gate and the override; the slot→character link is
restored as the line's voice source. No schema change is involved, so no forward
migration is needed.

## Consequences

- The Wind Waker's shared slots stop voicing every listener as the curator's
  linked character; the adjudicator is the per-occurrence identity source.
- Unconfident lines on shared slots degrade to the narrator voice.
- Single-speaker slots keep their behaviour unchanged.

## Links / references

- ADR-0070, ADR-0101, ADR-0154.

## Amendment 2026-10-22: what makes a slot "shared"

The first cut judged a line's slot shared when its resolved character answered to more than one key (its name plus its aliases). Every slot-linked character carries a name plus at least one `ww:spk:<n>` alias, so every slot-linked line counted as shared. Without a confident adjudicator answer, each one fell back to the narrator.

`make test-av` on 6f75ea6 caught this: 9 of 12 lines spoke in the narrator voice instead of the character's. The latency gate still passed, at p50/p95 7/404 ms.

A slot is now shared when the profile's lines reach the resolved character through **more than one distinct speaker key**. That is the curator merging slots onto one character, as on the board, where Aryll is linked from both `ww:spk:11` and `ww:spk:3`.

A character reached through a single key keeps its assignment, however many names and aliases it carries.

**Limitation:** a single slot used by several NPCs but linked to one character is not detectable from the script. It still voices through the link, unless the adjudicator confidently names someone else.

## Amendment 2 (2026-10-22): speaker-link erases the per-line key

Speaker-link (ADR-0070) re-points a placeholder's lines onto the target character, and retires the placeholder with its name folded into the target's aliases. After a link, every line carries the target's own ID. The distinct-key rule from amendment 1 therefore saw one key, and a boy NPC's line on the board still voiced as Aryll, who has three folded placeholders.

A character with **two or more retired placeholders folded into its aliases** now counts as a slot merge too. On the board's rev5 cast that covers 13 of 29 characters, from Tetra (5 slots) to Medli (2).

Those lines need a confident Clef speaker or kind answer to get a character or generic voice. Without one they fall back to the narrator.

A confident local match skips Clef, so ADR-0160 asks the speaker question early for these lines.

Test: the speaker-link case in `TestSharedSpeakerSlots`.

Evidence: `make test-av` passed on this change (settled→audio p50/p95 243/384 ms, voice gate green).

## Amendment 3 (2026-10-22): a shared slot keeps its assigned voice

The narrator default for shared slots took the voice away from the cast. Amendment 2's merge rule marks 13 of the 29 board characters as shared, including Aryll, Tetra, Medli and Grandma. A confident local match never asks Clef, and the early speaker question (ADR 0160) was reverted, so those characters' lines almost never get a confident speaker answer.

On the board (0.1.0-8058189, 2026-10-22 evening), every line spoken in a three-hour session went to the narrator, including Medli's, although Medli has her own approved voice.

**Decision:**
- A shared-slot line keeps its assigned character unless the adjudicator confidently names a different speaker; that override is unchanged.
- The kind voice (ADR 0158) and the narrator stay the fallback for unattributed lines only.

**Tradeoff:** a villager on a slot the curator linked to a named character can speak in that character's voice again. That is the original complaint, but it is the rarer case. Most lines on such a slot are the named character's own lines.

The fix for that case is a per-occurrence speaker answer that doesn't stall commits: the redesign tracked in #54.

Tests: `TestCommitSpeakerLowConfidenceKeepsAssigned` and `TestCommitSpeakerUnknownKeepsAssigned`. The kind-route tests now use unattributed lines.
