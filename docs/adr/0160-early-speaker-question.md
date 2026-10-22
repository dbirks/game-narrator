# ADR-0160: Ask Clef "who is speaking" early for shared-slot lines the local policy already committed confidently

- Status: accepted (implementation landed; live-board validation still pending)
- Date: 2026-10-22
- Related requirements/tests: docs/spec/05 (Clef adjudication and commit policy), docs/spec/07 (voice lifecycle), docs/spec/12 (correlation proof), docs/spec/13 (observability and resilience); ADR-0070 (Wind Waker speaker attribution from the BMG INF1 per-message attribute byte), ADR-0072 (positional wire option aliases), ADR-0101 (portable voice mappings and narrator fallback), ADR-0135 (Cast UI / auto-cast character names), ADR-0154 (full-frame Clef decision), ADR-0156 (speaker attribution beyond slots), ADR-0158 (speaker kind generic voices).

## Context and constraints

The 2026-10-22 live play exposed the next shape of the shared-slot defect ADR-0156 fixed: the boy NPC's line committed `MATCH_FUZZY` with a high local confidence, so the commitment policy never asked Clef for a decision — the adjudicator that reads the frame and names the per-occurrence speaker (and, since ADR-0158, the apparent kind) was never consulted. No speaker answer existed, the slot was shared, and the line fell back to the narrator.

ADR-0156's fallback was honest but lossy: the owner heard the narrator for a boy the frame could have named, and the ADR-0158 generic kind voices — the right answer for "an unnamed villager who is a man" — were reachable only through a Clef answer that never came.

Shared slots are the Wind Waker's normal shape, not an edge case: the BMG INF1 per-message attribute byte carries a speaker *slot*, and the curator merges several slots onto one character (ADR-0156's amendment), so many cast characters share one slot. When the local policy commits such a line confidently, there is no ambiguity to adjudicate — and therefore no attribution to ask for.

The fix has to cost no latency: the question "who is speaking" can be asked while the line is already certain, before the commit speaks, and the answer must not delay the commit by more than a small, named budget. The first revision of this ADR wired that ask synchronously into the rank worker and gave the ask the early budget (300 ms) as its own deadline; the UNO Q board's Clef round trip (~0.6–1 s) plus gateway network (~1.1 s) routinely exceeds it, so the ask gave up before answering and the defect the ADR set out to fix stayed live. A review-redesign moved the answer through the delivery redesign recorded below; this section records the final shape.

## Options considered

1. **Ask Clef "who is speaking" early, for shared-slot lines the local policy committed confidently.** Adopted.
2. **Lower the local acceptance floor / ask Clef for every line.** Rejected: it spends a GPU round trip on lines the local evidence already settled, on lines that were never ambiguous — the latency the proposal exists to remove.
3. **Ask only the speaker question, skip the kind.** Rejected: the kind is the next evidence source the frame carries (ADR-0158) and rides the same answer plumbing for free; a speaker-only ask without the kind could not reach the generic kind voice.
4. **Infer the kind locally from the frame.** Rejected: the kind is still a claim about a person; the local path's evidence (OCR + placement) is not the adjudicator's frame reading (ADR-0154).

## Decision

1. **When a read's top local candidate is a shared-slot message, the runtime asks the early-speaker adjudication — even though the local policy committed the line confidently.** The trigger is the session's `SharedSpeakerMessages` view (ADR-0156): published message ids whose speaker slot the script reaches through more than one distinct speaker key. The ask fires on the first read whose top candidate is that message, before the commit for the occurrence — the ranking the commit would have consumed anyway, so the ordering is the ordinary one (score → decide → commit) and the speculative TTS prewarm hook (ADR-0154's overlap) already covers synthesis while the answer is in flight.
2. **The ask is speaker-only** (`pipeline.DecisionRequest.SpeakerOnly`): the speaker + speaker_kind questions, rendered by `systemone.BuildAdjudicationSpeakerOnly` — no candidate, eligibility or completeness questions, because the caller already holds the line the commit will speak. The positional option contract is kept (the speaker question's `spN` aliases over the speakers slice plus the `unknown` fallback; the kind question's kind ids plus the `unknown` fallback), and the answer maps back through `systemone.Interpret` exactly as a full-bundle answer does — the chosen option's probability is the confidence, which is what the runtime's speaker and kind gates read (ADR-0156, ADR-0158).
3. **The answer overlays onto the confident local top candidate** through the attribution-only path: the reply keeps the local line's choice, eligibility and reason, and carries the answer's speaker + speaker_kind + confidences as the per-occurrence overlay the commit payload reads. The commit therefore never narrows on an early answer, and the commit payload carries the early answer's attribution whether the line committed for an obvious local reason (e.g. a confident `MATCH_FUZZY`) or for another.
4. **The ask is fire-and-forget.** The ask runs in the rank worker's own goroutine and delivers the ranking reply itself, so the rank worker never blocks on it; the worker continues immediately. The ask's deadline is the decision role's normal timeout (`DecisionOptions.Timeout` → the role's `Timeout` → the default), **not** the early-speaker budget — the board's 4 s decision-role timeout is what a full adjudication would get, and the early ask pays no more than that. The early-speaker budget is a *speech-side* bound, recorded next.
5. **One ask per box occurrence, at most.** The occurrence key is the box (region + generation) plus the top message id the occurrence's ranking committed on; an in-flight ask is registered per region and the rest of a burst of reads of the same box drop through it. The ask resolves when its answer is delivered; a later reply for the same occurrence is fenced by the session's generation / observation seq / sequence fences, and the breaker bounds the call.
6. **Delivery: before the commit, or after it.** When the answer arrives it is sent to the reducer as the AttributionOnly DecisionMsg through the existing replies path (a `select` on the answers channel with the ask's context; dropped when the session is gone). Because the answer-bearing reply is what the ask itself delivers as the ranking reply, the delivery is ordered ahead of the commit that consumes the ranking — the late-arrival branch (publish an `attribution.resolved` event for the occurrence, or the equivalent) is the defensive fallback for a delivery that cannot make the ordering any more explicit, and it carries no speaker state: by then the commit payload has already spoken with the local evidence.
7. **The bounded wait lives on the speech side.** When a committed line is a shared-slot line and an early ask for its occurrence is still in flight, the speech wiring waits up to `DecisionOptions.EarlySpeakerBudget` (default `DefaultEarlySpeakerBudget` = 300 ms) for the attribution before resolving the voice; the wait is a per-occurrence registry in the runtime, fed by the session's attribution path (the answers the ask delivered). After the budget the ask has given up, the commit proceeded on the local evidence alone, and the line speaks the narrator (or the kind fallback) as today. The typical case: the ask fires at the first read of a typing box and the typewriter takes 1–3 s, so the answer is usually in before the commit and the wait is 0.
8. **One ask per box occurrence, at most** (the delivery of decision 5): the session's fences (generation / observation seq / sequence) discard a late reply, and the breaker and the ask's deadline bound the call. When the breaker is open or the gateway is unreachable the ask abstains and the behaviour is today's — nothing is sent, the commit speaks the local evidence.

## Tradeoffs

- **Latency:** the early ask no longer holds the commit at all — it runs fire-and-forget in the rank worker's goroutine and the commit proceeds on the confident local evidence immediately; the answer-bearing reply is delivered through the ordinary replies path before the commit consumes the ranking. The bounded wait moved to the speech side, where a committed shared-slot line may hold its voice resolution for up to the early-speaker budget (`DecisionOptions.EarlySpeakerBudget`, default 300 ms) before the answer arrives; beyond that the commit is not delayed further. The proposal names this budget as the small delay the owner accepted for the no-latency win.
- **Payload:** the early bundle uploads the frame (ADR-0154) plus the attribution questions only — smaller than a full bundle, but still a frame per ask.
- **Scope:** the early ask is a decision-role behaviour; sessions without a wired decision role (or without shared-slot lines) send nothing, unchanged.
- **Presynthesis:** shared-slot lines are presynthesized in the narrator voice today (the prewarm/presynth resolve the speaker through the slot link, and a shared slot resolves to the neutral voice), under the same cache key a commit with no attribution answer computes. A kind-routed commit resolves a different character (the generic kind character), so its cache key differs and it misses the narrator-keyed entry cleanly and synthesizes live — the presynth entry it would have wanted is not there, by design: the kind is only known after the adjudication. Presynthesizing shared-slot lines in each approved kind voice as well was considered and rejected: it would multiply the cache footprint for those lines (~7× for a slot shared by several characters), which the profile's presynth budget does not justify before the answer exists.

## Validation

- `go test ./internal/providers/systemone/ ./internal/pipeline/ ./internal/session/ ./internal/runtime/`
- New tests with invented data and a fake decider / fake gateway:
  - a confident local top candidate on a shared slot plus an early answer `{kind: boy, 0.9}` → the answer overlays on the local top candidate and the commit payload carries the speaker + kind attribution; the kind route (`speaker_kind`) is what the speech side resolves;
  - an answer late beyond the early budget → the ask gives up inside the budget, no stall, the commit proceeds on the local evidence and speaks the narrator;
  - a non-shared line → no early request is sent;
  - an open breaker → no request is sent;
  - exactly one early request per box occurrence;
  - the fire-and-forget redesign: a Decider that answers a 2 s late must not serialize a burst of reads of one box behind the answer — the first read's ask carries the sleeping decider, the reads after it drop through the per-occurrence in-flight dedupe and reply without queuing behind the sleep.
- `systemd-run --user --scope -q -p MemoryMax=4G go test -race -timeout 25m ./internal/runtime/ ./internal/pipeline/ ./internal/session/... ./internal/providers/systemone/`
- `systemd-run --user --scope -q -p MemoryMax=4G make test-replay` (junit line).
- `make lint`.

## Reversal plan

Drop the `SpeakerOnly` flag and the early-ask trigger (the session's `SharedSpeakerMessages` view), or widen the early budget to zero (the ask then never holds a commit). The ADR-0156 speaker gate and the ADR-0158 kind gate stand on their own and are unchanged by the early wiring, so no reversal of those is implied.

## Consequences

- Shared-slot lines that the local policy committed confidently now get an attribution answer before the commit speaks, so the narrator fallback is the exception rather than the shape of the day.
- Unnamed shared-slot lines whose kind is unconfident or unknown still degrade to the narrator — the honest fallback (ADR-0101), not a substitute.
- Kind-routed lines are distinguishable through the utterance's `voice_route` (`speaker_kind`) on the speech lifecycle events (ADR-0158).

## Links / references

- ADR-0070, ADR-0072, ADR-0101, ADR-0135, ADR-0154, ADR-0156, ADR-0158.

## Amendment 2026-10-22: read-quality floor (observed on the UNO Q)

The first board deploy (8e609ab) asked Clef every 2–5 s while the owner was in scenes with no dialogue box. Junk scene reads such as "ot", "vy 1 ©" and "a . \ a. dal" passed the fragment matcher (ADR 0151) onto a shared-slot line, and each new generation/top pair defeated the per-occurrence dedupe. No junk was spoken, because the commit-side gates held.

The early ask now also requires:
- a non-abstaining ranking;
- a read that looks like dialogue: at least two words of three or more letters (`earlyAskWorthy`).

A box still typing asks a moment later, once its text has grown.

Tests: `TestEarlyAskWorthy` uses the board's actual junk strings, and `TestEarlySpeakerAskNotFiredForAbstainingJunkRead` fails without the floor.
