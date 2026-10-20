# ADR-0092: Speech fence advances only on speakable progress; terminal results are idempotent; voice skips name their reason

Date: 2026-10-20
Status: accepted
Owners: agent (issue #6)
Related requirements/tests: docs/spec/01 invariant 2 (`commit_id`/`utterance_generation`
fence stale asynchronous work), docs/spec/07 "Audio scheduler", docs/spec/12 "Zero
stale audio starts" and "deterministic delay-injection suite", docs/spec/13 "did it
speak, and why not"; GitHub issue #6. ADR-0039 (runtime speech pipeline: the
"fence advances on every commit" decision this ADR amends), ADR-0023 (generation-
fenced scheduler), ADR-0057 (synthesis cancelled at the source on a fence move),
ADR-0059 (scheduler stop joins the terminal result). Tests: `TestSpeechNoiseCommitsDoNotFence`,
`TestSpeechTerminalIdempotent`, `TestSpeechVoiceSkipNamesReason` (internal/runtime,
white-box wiring); `TestSpeechRapidAdvanceCancelsStale`, `TestSpeechSessionEndTerminatesPending`
(stale + terminal contracts); `make test-replay`; bench `bench-synthetic-001 --split holdout`
(`stale_speech_after_advance` / `duplicate_speech` stay 0).

## Context and constraints

In the live per-character Dolphin run (issue #6, evidence `artifacts/per-character-live/`),
a second profile region read 1–3 character OCR fragments off animated scenery (water).
Each fragment committed as a non-line (`message_id: "unknown"`, `speech_eligible: false`).
The runtime's commit watcher advanced the audio fence on **every** commit
(ADR-0039: "the fence advances on every commit — an ineligible box still means the
player moved on"), and `fenceAndCancel` asked the scheduler to stop every older
generation. Three defects followed:

1. **Non-speakable commits cancelled real speech that was playing.** A noise commit cut
   the line the player was listening to (48 noise commits landed between two real lines
   in one run).
2. **Terminal results were not idempotent.** `byGeneration` was never pruned, so a
   generation that had already *ended* was cancelled again on every later commit —
   dozens of `speech.cancelled {reason: "new_commit"}` records with empty message/voice
   ids (the correlation table had evicted the old generation).
3. **A voice skip carried an empty reason.** When `resolveVoice` found no approved voice
   for a non-narrator speaker and no configured narrator voice, the skip event carried
   `reason: ""`, so an all-skip session could not be diagnosed from the events.

Constraints that shaped the decision:

- docs/spec/01 invariant 2: the commit generation fences stale **asynchronous audio
  work**. A newer *speakable* line is the event that makes an older line's audio stale —
  a commit with no audio of its own does not, by itself, invalidate the line the player
  is hearing.
- docs/spec/12 "Zero stale audio starts" must be preserved: after a new speakable line
  commits, no audio for the old line may start (bench `stale_speech_after_advance` = 0).
- docs/spec/13 "did it speak, and why not": every requested utterance reaches a terminal
  event with a reason, and the reason must be diagnosable (including *whose* voice was
  missing on a skip).
- The scheduler (ADR-0023) is the single owner of device-level playback events; the
  wiring owns the TTS-lifecycle terminals. The two must not emit a second terminal for
  one generation.

## Decision

### 1. The fence advances only on speakable progress

`speechWiring.onCommit` reads the commit's `speech_eligible` flag, `message_id` and text,
and **fences only when the commit is speakable**: `eligible && message_id != "" &&
message_id != "unknown" && trim(text) != ""`. A commit that is not speakable returns
before the fence moves and before the occurrence is registered — it produces no speech
events and cancels nothing.

Exactly which events advance the fence:

| Event | Advances the fence? | Effect |
|---|---|---|
| A **new speakable commit** (`message.committed`, `speech_eligible: true`, known message, non-empty text) | **Yes** | `fenceAndCancel`: the fence moves to the new generation; every older *unsettled* generation is cancelled at the source (`sched.Cancel(reason: "new_commit")`) and its in-flight synthesis cancelled (ADR-0057). This is the stale-speech guarantee. |
| A **non-speakable commit** (`speech_eligible: false`, `message_id: "unknown"`, or empty text) | **No** | No fence move, no cancel, no registration, no speech events. A noise commit cannot cut the line the player is hearing. |
| A **session stop** | **Yes (terminal)** | `speechWiring.stop()` drains the workers (bounded) and `sched.Stop()` joins the active playback's terminal (ADR-0059). The session's end is a fence that ends every in-flight utterance with its reason. |
| An **operator pause** | **No advance; cancels the current** | `pause()` cancels every *unsettled* generation (the current one included) with `reason: "paused"` but does **not** advance the fence (a pause is not a textbox advance — the resume's next speakable commit fences against it). |

The "dialogue advance / box gone" case is covered by the first row: in this product a
dialogue advance is a new speakable commit, and that is the event that supersedes the
previous line's audio. A box that is dismissed with no successor line emits no commit the
wiring can observe (the session's own speech lifecycle is not driven at runtime — the
scheduler owns the audio), so a dismissed box's audio runs out its bounded line length
rather than being cut; the next speakable commit is what fences it. Wiring a dismiss
event into the fence is a follow-up (below), not part of this change.

The fence itself (`generationFence.IsCurrent`) is unchanged: it approves exactly the
generation the wiring last fenced, by equality. A non-speakable commit no longer moves
that value, so the last speakable line stays current across any number of noise commits.

### 2. Terminal results are idempotent

The wiring gains a `settled map[int64]bool`: a generation is *settled* once it has
reached a terminal. Every path that terminates a generation funnels through the same
fact, so "settled" and "a terminal was emitted for this generation" cannot diverge:

- `fenceAndCancel` settles each generation it asks to die (before `sched.Cancel`), and
  builds its cancel list **only from unsettled generations** — a settled generation is
  never cancelled a second time.
- `pause` settles each generation it cancels and skips settled ones.
- `finishEnded` (the wiring's own terminals: skip, mute, worker failure) settles the
  generation.
- `noteSchedulerEvent` settles a generation on its scheduler terminal
  (`speech.ended` / `speech.cancelled`); a `speech.started` is **not** a terminal — a
  started generation remains cancellable by a newer speakable commit (the stale
  guarantee is preserved).

`settle` also prunes the generation from `byGeneration` (the fence's cancel table), so
ended/cancelled generations are no longer enumerated by a later fence and no longer
evicted-then-referenced. The correlation tables (`utterances`, and `byGeneration` for the
duration of the terminal's own event) are kept long enough that the terminal event that
supersedes a playback still correlates its message/occurrence/voice ids; the empty-id
records of issue #6 came from re-cancelling a generation whose correlation had already
been evicted, and that re-cancel can no longer happen.

The consequence is the idempotency contract: **one generation emits at most one
`speech.cancelled` and at most one `speech.ended` from the fence path.** (A cancelled
playback may still carry its scheduler `speech.ended {result:"ended", reason:
"interrupted"}` alongside the fence's `speech.cancelled` — the stop request and the
sink's measured result are distinct facts, both retained; what is forbidden is a second
*fence* terminal for a generation that already terminated.)

### 3. A voice skip names its reason and the character

`resolveVoice` now returns `no_approved_voice` whenever neither the speaker's character
nor the configured narrator resolves to an approved voice (the previous code returned an
empty reason when a non-narrator speaker had no voice and the narrator fallback also had
none). The utterance carries the resolved `character` (the speaker's character, or
`narrator` when unattributed), and the skip's `speech.ended` event names it:
`result: "skipped"`, `reason: "no_approved_voice"`, `character_id: "<character>"`.
`character_id` also rides the `speech.requested` and `speech.ready` events so the
developer trace shows whose voice a line was trying to use. The skip is additionally
logged (`speech: voice skip — no approved voice`, with session/character/message/
generation/reason) so an all-skip session is diagnosable from the log, not only the
event stream.

## Alternatives considered

1. **Fence on every commit (the ADR-0039 rule, rejected here):** it treated a commit
   with no audio as a dialogue advance, which is false for the water-region noise —
   the exact defect in issue #6. An ineligible box does not mean the *line* moved on;
   only a speakable line does.
2. **Filter the noise upstream (suppress the non-line commits), rejected:** the
   commits are real text-machine facts the developer UI shows ("the box that was not
   narratable"); the speech layer's "no audio" answer is the eligibility flag, and the
   fence must honour it rather than delete the evidence.
3. **Prune `byGeneration` synchronously in `fenceAndCancel`, rejected in that form:**
   the terminal's own event is published asynchronously by the scheduler and correlates
   through the table; pruning before the event lands would reproduce the empty-id
   records. Pruning happens on `settle` (terminal observed), after the event that
   justified it is correlated.
4. **Watch a session "region dismissed" event to fence a gone box, deferred:** the
   runtime does not drive the session's speech lifecycle, so the reducer's
   `region_vanished` cancel is not emitted for the runtime's audio; there is no
   observable event to hook. Dismissed-box audio runs out (bounded by line length) and
   is fenced by the next speakable commit. Follow-up: if the product requires a hard
   stop on dismissal, the session must publish a dismiss event on the wiring's chain.

## Consequences and risks

- **Positive:** a noise commit can no longer cut the line the player is hearing; the
  developer log no longer fills with duplicate `cancelled/new_commit` records; an
  all-skip session names the missing voice and the character. The stale-speech
  guarantee is unchanged and re-pinned by `TestSpeechNoiseCommitsDoNotFence` (part 2)
  and the existing `TestSpeechRapidAdvanceCancelsStale`.
- **Behaviour change (intended):** a dialogue box that is dismissed with no successor
  line now runs its audio out instead of being cut by the next (noise) commit. Where a
  real successor line follows, it still cancels the previous line — the common
  advance case is unchanged.
- **Risk:** a generation that is settled but whose scheduler terminal never arrives
  (a wedged sink at session end) stays settled and pruned; the session is ending, so
  no later commit can re-cancel it, and the drain/stop bounds (ADR-0059,
  `speechStopDrainBound`) already degrade a wedged worker to a logged late event.
- **Correlation:** `settle` prunes `byGeneration`, so a generation's correlation lives
  until its terminal is observed. The bench's per-sample sidecar and the scenario
  harness keep their own fences (out of scope); the runtime's bench shadow
  (`internal/bench`) is unaffected because it drives the session reducer directly, not
  this wiring.

## Evidence and test plan

```
go test -race -count=1 ./internal/runtime/... ./internal/speech/... ./internal/session/...
# white-box fence/idempotency/skip tests:
go test -race -count=1 -run 'TestSpeechNoiseCommitsDoNotFence|TestSpeechTerminalIdempotent|TestSpeechVoiceSkipNamesReason' ./internal/runtime/
# replay gate + bench (stale/duplicate metrics stay 0):
make test-replay
go run ./cmd/game-narrator bench --set testdata/bench/bench-synthetic-001 --split holdout
```

- `TestSpeechNoiseCommitsDoNotFence`: line A (speakable) → 20 non-eligible noise
  commits → line A ends `ended` (not `cancelled`) with no `speech.cancelled` for its
  generation; then a real speakable commit cancels the still-playing previous line
  (`new_commit`/`stale_generation`) and the successor starts — the stale guarantee.
- `TestSpeechTerminalIdempotent`: a naturally-ended generation is settled and pruned; a
  later commit emits no second `speech.ended` and no `speech.cancelled` for it.
- `TestSpeechVoiceSkipNamesReason`: a line whose speaker (and the narrator) have no
  approved voice ends `skipped` with `reason: "no_approved_voice"` and
  `character_id: "link"`, and no TTS/playback occurs.
- Bench: `stale_speech_after_advance` and `duplicate_speech` remain 0 on the holdout.

## Migration / rollback

No schema change; no wire change to existing fields (only additive `character_id`).
Revert by restoring the ADR-0039 "fence on every commit" behaviour (remove the
speakable guard in `onCommit`) and dropping the `settled` map; the `character_id` field
is additive and safe to leave.

## Follow-ups

- If the product wants a hard audio stop when a dialogue box is dismissed with no
  successor, publish a region-dismiss event on the session's publisher chain and fence
  on it here (a session-event follow-up, out of scope here).
- The scenario harness (`internal/scenario/run.go advanceFence`) and the bench sidecar
  mirror the *old* fence-every-commit rule; align them to fence on speakable progress
  when their owners next touch them (kept out of this change's area).
