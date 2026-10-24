# ADR-0167: Ask Clef "who is speaking" at commit time for shared-slot lines

Date: 2026-10-23
Status: accepted
Owners: agent worker (speaker-at-commit)
Related requirements/tests: docs/spec/05 (Clef adjudication and commit policy), docs/spec/07 (voice lifecycle), docs/spec/12 (correlation proof), docs/spec/13 (observability and resilience); ADR-0070 (Wind Waker speaker attribution from the BMG INF1 per-message attribute byte), ADR-0072 (positional wire option aliases), ADR-0101 (portable voice mappings and narrator fallback), ADR-0135 (Cast UI / auto-cast character names), ADR-0154 (full-frame Clef decision), ADR-0156 (speaker attribution beyond slots), ADR-0158 (speaker kind generic voices); GitHub issue #54, slug `speaker-at-commit`.
Tests: `internal/providers/systemone` (speaker-only bundle), `internal/runtime` (speaker-at-commit ask, wait, fallback, cap), `internal/runtime/speech_fallback_test.go`.

## Context and constraints

ADR-0160 asked Clef "who is speaking" early — off the rank worker, on the first read of a typing box whose top candidate was a shared-slot line the local policy committed confidently — and was reverted after a board regression (about 90 minutes of real play; 930 decision requests, mostly junk reads; clean matched lines stopped committing; the answer could not attach to the occurrence).

The retry's suggested shape (GitHub issue #54): gate the ask on a *committed* line — fire at commit time, then wait at most 300 ms in speech — and cap the asks.

ADR-0156 amendment 3 restored assigned voices for shared slots: a male villager on a slot linked to a female character (Rose) is voiced as her. That is the defect this decision closes.

## Options considered

1. **Ask Clef "who is speaking" at commit time, for a committed shared-slot line whose payload carries no confident speaker.** Adopted (the ADR-0167 shape).
2. **Ask at the first read of the typing box (the ADR-0160 shape, redesigned).** Rejected for the retry: the firing generation and the commit generation do not coincide, the answer could not attach, and the burst of junk reads defeated the per-occurrence dedupe.
3. **Infer the speaker locally from the frame.** Rejected: no local source carries the per-occurrence identity (ADR-0156's research), and inventing one would violate the never-invent-a-speaker rule.

## Decision

1. **The trigger is a committed line**: in `speechWiring.onCommit`, for a matched commit whose message is a shared slot (`speakerSlotByMessage`) and whose payload carries no confident speaker, start one goroutine.
2. **The ask is speaker-only**: the goroutine asks the decision role through the decision wiring's own System One client — `decisionWiring.AskSpeaker(ctx, frame, candidateMessageID, speakers)` — which builds the speaker + speaker_kind questions only (`systemone.BuildAdjudicationSpeakerOnly`), rides the full frame (`encodeDecisionFrame`, ADR-0154), and runs through the model router (breaker, priority slot, per-attempt timeout, decision budget). It returns the adjudicator's attribution; it never narrows a candidate (the caller holds the line).
3. **The utterance waits, bounded**: the utterance's voice resolution waits at most `DefaultSpeakerAskBudget` (300 ms, the ADR-0160 proposal's number) for the answer, through a per-occurrence wait registry (`beginAsk` / `resolveAsk` / `waitAsk`), and resolves its voice with the existing `commitVoice` gate with the answer overlaid on the commit payload (the ADR-0156 confidence floor, the ADR-0158 kind fallback). Late, failed or cancelled answers resolve empty and the line speaks the assigned voice, as today.
4. **The wait lives in the routing worker**: the wait is taken in the TTS worker (`speak`) — the utterance that owed the ask — never on `onCommit`'s path, which stays the commit watcher's fast synchronous half; the ask runs in its own goroutine started at commit time.

## Tradeoffs

- One decision round trip per committed shared-slot line, bounded by the 300 ms wait and the global 20 asks/minute cap (the issue's "cap it at a few requests per minute").
- The wait adds up to 300 ms of commit→voice latency on the lines that owe an ask, which the owner accepted (the ADR-0160 proposal's budget).

## Validation

- `go test -race -timeout 25m ./internal/runtime/ ./internal/providers/systemone/`
- `systemd-run --user --scope -q -p MemoryMax=4G make test-replay` (junit line).
- `make lint`.

## Reversal plan

Drop the commit-time ask (the shared-slot assigned voice is ADR-0156 amendment 3's fallback), or widen the ask budget. The ADR-0156 speaker gate and the ADR-0158 kind gate stand on their own and are unchanged by this wiring.

## Consequences

- Shared-slot lines the local policy committed confidently now get an attribution answer before the line speaks, so the narrator fallback is the exception rather than the shape of the day.
- Unnamed shared-slot lines whose kind is unconfident or unknown still degrade to the narrator — the honest fallback (ADR-0101), not a substitute.

## Links / references

- ADR-0070, ADR-0072, ADR-0101, ADR-0135, ADR-0154, ADR-0156, ADR-0158, ADR-0160 (reverted), ADR-0167.
- GitHub issue #54 (slug `speaker-at-commit`).

## Amendment 2026-10-23: a confident kind that contradicts the assigned voice

A Clef speaker answer can only name a cast character. The villager on the board isn't in the cast, so his line kept the slot's assigned voice (Rose, measured at 203 Hz), even when the frame plainly showed a man.

Each assigned character's approved voice is now measured once at session start (`warmVoiceClasses`: YIN median F0 of the reference WAV, low means below 165 Hz). On a shared slot, a confident apparent kind (≥ 0.5) on the other side of the male/not-male line from that measurement takes the generic kind voice (ADR 0158).

Guard rails, so the narrowest condition is the only override:
- an agreeing kind, an unconfident kind, the creature kind, and an unmeasured voice all keep the assignment;
- a confident named speaker still wins over everything.

Test: `TestCommitVoiceGenderConflictTakesKindVoice`.

## Amendment 1 (2026-10-24): the ask's speaker list is capped

Evidence: on the real UNO Q board (build 486619b, rev 5 profile with about 80 characters), every speaker-at-commit ask failed as `provider.degraded` `bad_schema`. A shared-slot line (a boy NPC whose BMG slot is linked to another child character) therefore kept the slot's assigned voice. The bundle `hw-proof-20261024T064020Z` records `voice_route: assigned` with `speaker_confidence: 0`. Reproduced live against the decision role: a speaker-only request passes with 5 declared speakers and is refused at 20, 40, 60 and 81 ("question speaker has N options, over the 16 option cap"). The main adjudication already capped its per-request speakers at `SpeakerOptions`; the commit-time ask passed the whole roster.

Decision: `speakerAskOptions` narrows the roster to `SpeakerOptions` (at most `systemone.MaxOptions-1` declared speakers, plus "unknown"). The line's assigned character comes first, so the answer can confirm or overrule it; the rest keep roster order. Validation: `TestSpeakerAskOptionsCapsTheRoster`, plus the existing speaker-at-commit tests.

Follow-up: roster order is a weak relevance signal. A better shortlist would take the scene's or recent speakers, or the characters whose voices match Clef's apparent kind.
