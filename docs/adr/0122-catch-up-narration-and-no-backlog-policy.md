# ADR-0122: Catch-up narration and the no-backlog speech policy: lines committed while an utterance is playing are batched, merged per speaker and spoken back-to-back; anything older than the batch window is fenced, and no utterance starts past its max age

Date: 2026-11-03
Status: accepted
Owners: agent (issue #25)
Related requirements/tests: GitHub issue #25 (responsive live
narration: drop backlog, catch-up narration, Dolphin E2E per character);
docs/spec/01 invariant 2 (commit_id/utterance_generation fence stale
asynchronous work), docs/spec/07 "Audio scheduler" and "Prewarm top weighted
successor messages", docs/spec/12 "Zero stale audio starts", docs/spec/13
"did it speak, and why not". ADR-0092 (speakable-only fence and idempotent
terminals — the fence this ADR builds on), ADR-0023 (generation-fenced
scheduler, single active playback), ADR-0057 (synthesis cancelled at the
source on a fence move), ADR-0039 (runtime speech pipeline), ADR-0095
(successor prewarming). Tests: `TestCatchUp*` (internal/speech batcher
unit tests), `TestSpeechCatchUp*` (internal/runtime white-box wiring:
merge-by-speaker over four rapid boxes, max-age drop, fence interplay, the
latest_only policy), the `synthetic/gn-t025-catch-up` replay scenario
(four rapid boxes from two speakers; make test-replay), the real Dolphin
rapid-A test `TestLiveCatchUpRapidDialogue` (make test-dolphin, the
first-dialogue savestate); the live per-character E2E
(internal/e2e, `TestLivePerCharacterVoices`) already covers the
issue's 4(a)/(b) half (legend narrator + townsperson's own voice).

## Context and constraints

The first live session on real hardware (issue #25, 2026-10-20) left three
audio behaviours wrong for fast play:

1. **The backlog question was answered by immediate cancellation, which
   cut live speech.** A speakable commit advanced the fence (ADR-0092) and
   cancelled the generation that was playing, so a tap that landed while
   the player was listening cut the line mid-sentence. The player wanted
   the opposite for taps: every line that was on screen, even briefly,
   should be heard — played back-to-back, merged so the TTS budget is not
   spent one round trip per line.
2. **The pipeline could build an unbounded backlog.** A line whose TTS
   round trip or queue time was long could start playing long after the
   box it belonged to was gone — "played late" speech the issue forbids:
   "stale reads, rankings and speech for superseded boxes are discarded
   rather than played late." ADR-0092's generation fence covers the
   superseded case, but it does not cover a line whose box was dismissed
   without a successor speakable commit (ADR-0092 explicitly deferred
   wiring a dismiss into the fence): nothing moves the fence, and the
   late utterance would start against an empty screen.
3. **The merged-utterance cache identity was undefined.** Merging several
   committed lines into one TTS request needs a cache key that names
   exactly the lines it covers, versioned, so a different merge of the
   same lines (or a changed merge scheme) computes a different key, and
   the profile purge (ADR-0107) still addresses the rows of a purged
   profile's messages.

Constraints that shaped the decision:

- docs/spec/01 invariant 2 and docs/spec/12 "Zero stale audio starts" stay
  intact: after a new speakable commit, no audio for a box the player left
  more than the batch window ago may start, and a started stale playback
  stops within the 200 ms target (the scheduler's own fence tick,
  unchanged).
- ADR-0092's terminal discipline: every requested utterance reaches
  exactly one terminal event, and a terminal is never re-emitted. A line
   that moves from its own standalone utterance into a merged one keeps
  its correlation (the same occurrence) but the standalone utterance
  reaches a terminal of its own (skipped, reason `catch_up_batched`), and
  the merged utterance carries the line's message id in its event payload.
- The scheduler (ADR-0023) stays the single owner of device-level playback
  and keeps exactly one active playback. Catch-up is an ordering policy on
  the scheduler's inbox, not a parallel device path: merged runs play
  back-to-back as ordinary submissions.
- One session event loop stays fast: the commit path does the batch
  decision synchronously (a bounded map/list operation under one lock) and
  the TTS work stays on the workers.

## Decision

### 1. The catch-up policy is a session setting, `catch_up` by default

`ses.Settings` gains `CatchUp` (`catch_up` | `latest_only`), the session's
`Config` seeds it (default `catch_up`), the settings control carries and
validates it, and the `session.settings_changed` event names the applied
word so the speech side and the developer UI cannot disagree. The runtime
exposes it as the session's `catch_up` snapshot field and the partial-body
`PUT /api/v1/sessions/{id}/settings` field (a word outside the vocabulary
is a 400, like the mode).

`catch_up` (default): lines committed while speech is busy are batched and
spoken (below). `latest_only`: the historical behaviour — a speakable
commit cancels the playing/queued audio of older generations and speaks
the newest line at once (the ADR-0092 immediate shape).

### 2. What a speakable commit does, per policy

Let the commit's generation be g. The fence (the session's committed
generation) always advances to g — ADR-0092 is unchanged: a commit with no
audio of its own never fences.

- **`latest_only`** (and a `catch_up` commit that finds nothing busy):
  unchanged — older unsettled generations are cancelled at the source
  (synthesis cancelled per ADR-0057, queued/playing items cancelled) and
  the new line speaks at once on its own worker.
- **`catch_up`**: the commit's line is *batched* when speech is busy —
  an utterance of this session is playing, a catch-up batch is pending, or
  an immediate utterance is still in its TTS round trip — and it speaks at
  once otherwise (the fast path keeps the commit→voice latency of the
  first line of a conversation). For the older generations, the commit
  distinguishes the one that is **playing** (its lines are already being
  heard: it is left to run out — that is the behaviour the player asked
  for) from the ones that are **queued or in their TTS round trip** (their
  lines are not being heard yet): those are *rescued* — their in-flight
  synthesis is cancelled at the source and their queued submission
  dropped, and their lines re-enter the pending batch as ordinary batch
  lines. The rescued line's standalone utterance reaches its terminal
  (speech.ended, result `skipped`, reason `catch_up_batched`) so the event
  log says exactly where the line went; the content is spoken once, in the
  merged utterance.

### 3. The batch: window at the front, runs at the flush

The pending batch is a commit-ordered list of lines (each with its
committed wall clock). A new line drops, from the front, every pending
line it is more than the **batch window** (default 600 ms, per-session
`catch_up_batch_window_ms`) after: that is the issue's "Fencing (ADR-0092)
still applies to anything older than the batch window", made local — a
line the player left more than the window ago is fenced (terminal
`skipped`, reason `catch_up_stale`) at the moment the newer line lands,
not at the flush. The dropped lines' standalone utterances were never
started, so the drop is the terminal for them.

The batch **flushes** on the first of: the playing utterance's terminal
(speech.ended or speech.cancelled of the session's active playback), the
window elapsing since the batch's first line (a timer the commit path
arms; a line committed into an empty batch while nothing plays never
arms one — it took the fast path), or the box-change hook (below). A
flush groups the batch's lines into **runs**: maximal consecutive
same-speaker groups in commit order (the speaker is the line's resolved
speaker character, the narrator for the unattributed). One run is one
utterance: its text is the lines joined with sentence spacing (each line
closed to sentence final punctuation when it lacks it, then joined with a
single space — deterministic, unit-tested), its voice is the run's
speaker's resolved voice, and its synthesis is exactly **one TTS request**
per run (the merge is the budget win). Runs play back-to-back in commit
order as ordinary scheduler submissions.

### 4. The fence admits a batch's generations until it settles

The scheduler's fence is "equality with the committed generation" and the
scheduler's 100 ms tick stops whatever the fence rejects. A merged run
stamped with its batch's newest generation would be rejected by the tick
the moment a newer commit lands (the newer line batched, not spoken,
moved the fence) and the run's lines would be lost — the exact case
catch-up exists to speak. The wiring's fence therefore keeps a bounded
**approved set** of catch-up batch generations: a generation is approved
when its batch's runs are submitted and stays approved until every
utterance it names has settled (terminal event). `IsCurrent` is
`g == committed || approved[g]`. The approved set is bounded (oldest
evicted past 16 live entries, like the wiring's own cancel table) and is
dropped on pause and session end.

The window still binds: a commit that lands while a flushed batch's runs
are still in their TTS round trip cancels those runs at the source and
fences the approved generation when the batch's newest line is older than
the window at that commit (reason `catch_up_stale` / `catch_up_max_age` on
the lines' terminals). A batch that is merely late on a slow provider but
whose newest line is still inside the window keeps its approval — the
player has not left it, only the provider is slow.

### 5. The per-utterance max age: nothing starts late

Each run's **max age** (default 3 s, per-session `catch_up_max_utterance_age_ms`)
is measured from the newest line it covers, at the moment its audio is
ready to submit: if that instant is past the max age, the run is dropped
before it can start (terminal `skipped`, reason `catch_up_max_age` on each
covered line, no scheduler submission). This is the dismiss-without-
successor case ADR-0092 deferred: the box is gone, no commit moved the
fence, and only the age bound keeps the late utterance off the sink.
`latest_only` inherits the same bound on its immediate utterances.

### 6. The merged cache key names the merged message ids, versioned

A merged run's `KeyInput.OriginalMessageID` is
`m<version>:id1,id2,…` — the run's covered message ids in commit order
under a merge-scheme version (`m1`), the `speech.MergedOriginalID` form.
The single-line form (a bare id) is unchanged, so existing keys are
stable. The provenance column the ADR-0107 purge addresses by
`original_message_id IN (…)` cannot match the joined form, so
`DeleteSpeechCacheRowsForMessages` additionally matches rows whose column
contains any of the named ids as a whole id segment (the id alphabet has
no LIKE metacharacters, so a bounded `LIKE` per id is an exact-segment
match). Merged rows are therefore purged with the profile that owns any
of their messages.

### 7. The box-change hook: a small interface the pipeline can call later

The issue's item-2 half (a cheap pixel-level "box appeared/changed"
detector that prioritises the OCR read of the new box) belongs to the
capture/pipeline side and is out of scope here. This ADR provides only the
speech side's seam: `speech.BoxChangeNotifier` (one method,
`NotifyBoxChange()`), implemented by the session's speech wiring to flush
a pending batch immediately (the merged playback then starts on the new
box instead of at the window's end). The pipeline holds the interface and
wires its detector to it in a later change; a nil notifier is a no-op and
nothing in this ADR depends on the detector existing.

### 8. The scenario harness models the same policy

The replay harness (ADR-0037) speaks through its own sidecar, so the
catch-up policy is modelled there with the shared core of
`internal/speech` (the same `CatchUpOptions`, `SplitRuns`,
`JoinSpeechText`, `MergedOriginalID`, and the window-drop rule): the
sidecar's speak path batches lines under the scenario's `catch_up`
setting (YAML `inference.catch_up`, default `catch_up`), resolves each
line's speaker from the profile message's optional `speaker` field (the
scenario's `inference.voices` map names each speaker's voice; a
line with no speaker mapping uses `inference.voice`), merges into runs at
flush, and applies the session's per-occurrence speech lifecycle for every
line the merged utterance covers. The harness's playback fence carries the
same approved-set rule (decision 4). The TTS gateway records its request
texts so a report can show the merged text the merge produced.

## Tradeoffs

- **Batching while playing adds bounded latency to the next line's
  speech** (up to one window plus the running utterance's remainder)
  instead of cutting the running utterance. That is the requested
  behaviour for taps, and the window keeps the bound small; an operator
  who prefers the historical cut behaviour sets `latest_only` mid-session
  (the settings control applies from the next commit — a batch already
  flushed keeps its runs, the policy is read on the commit path).
- **Rescuing in-flight utterances means a cancelled TTS round trip** (the
  line's standalone synthesis dies so its line can re-enter the batch).
  The cost is one aborted provider call per rescue; the shared cache's
  singleflight means a retry of the same line (e.g. the line also in a
  merged run) is a shared generation, not a double spend.
- **The approved set weakens the fence's one-number simplicity**
  (docs/spec/01: "it is one number, not two"). The weakening is bounded
  and audited: the set holds only generations whose lines were inside the
  window at flush, it is pruned on every settle and on every commit that
  fences it, and its size is capped. A fence that could disagree is a
  fence that lies — so the set is dropped (not frozen) when a commit
  fences it, and the scheduler's own tick is the backstop that stops an
  approved-but-stale playback within 200 ms.
- **Merged rows leave the per-id purge's shape** (the joined id form
  needs the LIKE fallback of decision 6). The fallback is exact-segment
  for the id alphabet, and the alternative (not purging merged rows at
  all) leaks the profile's bytes on every purge.
- **The harness models the policy rather than running the wiring.** The
  replay test (class (b)) proves the merge/window/staleness decisions on
  the shared core over a real pipeline replay; the wiring's own
  white-box tests prove the production path end to end over the manual
  frame source, and the Dolphin test (class (d)) proves the live shape.
  Three proofs, one core.

## Validation

- `internal/speech` unit tests: the batcher's run split (consecutive
  same-speaker merge, speaker-change boundaries, commit order), the join
  rule, the merged-id form (versioned, commit order, single-line form
  unchanged), the window-drop rule, and the max-age predicate.
- `internal/runtime` white-box tests over the manual frame source
  (docs/spec/12 delay-injection shape): four rapid boxes from two
  speakers — every message spoken exactly once, the merged TTS requests
  grouped per speaker, the rescued line's terminal, no stale start;
  the max-age drop (a slow round trip past the age is dropped before the
  sink); the fence interplay (a commit outside the window fences the
  pending batch and the approved run); `latest_only` reproduces the
  ADR-0092 immediate cancellation contract.
- Replay: `synthetic/gn-t025-catch-up` (four rapid boxes, two speakers,
  invented text) asserts every committed message is covered exactly once
  by a started speech, the TTS request count equals the speaker-run count
  (the merge), and nothing stale ends after the advance.
- Store: the ADR-0107 purge deletes a merged row (joined id form) when any
  of its covered messages is purged, and leaves rows of unrelated
  messages alone (the exact-segment property: an id that is a prefix of
  another's segment does not match).
- API: the settings route accepts `catch_up`/`latest_only`, refuses a
  foreign word with 400, and the session snapshot carries the applied
  policy.
- Dolphin (test-dolphin gate, the first-dialogue savestate): rapid A
  presses through the first in-game conversation — every box commits, the
  catch-up speech covers every committed message, and no speech for a box
  starts after the window's staleness (the live half of the same three
  properties). The issue's 4(a)/(b) (the legend narrator and the
  townsperson's own approved voice) is the existing live per-character
  E2E (internal/e2e `TestLivePerCharacterVoices`), which runs the two
  recorded checkpoints and asserts the per-character voices and ASR QA.

## Consequences

- A fast A-tapper hears every line, merged per speaker, within one window
  of the tap; a slow tapper hears each line at once as before.
- The speech side owns three new tunables (policy, window, max age), all
  session-scoped, all defaulted, all visible in the session snapshot.
- The cache gains a second OriginalMessageID shape; the purge and any
  future provenance reader must know the `m<version>:` form.
- The pipeline's box-change detector (issue #25 item 2) has a ready seam
  (`speech.BoxChangeNotifier`) when it lands; until then the flush waits
  on the playback terminal or the window.

## Reversal plan

- Set `latest_only` (session setting, live) to restore the immediate
  cancel-and-speak behaviour without a redeploy.
- Reversing the change: the wiring's commit path, the batch state and the
  approved set are additive; removing them restores ADR-0092's shape.
  Merged cache rows (the `m1:` provenance form) are cache content: they
  are evicted by the LRU and deleted by the extended purge; deleting the
  cache table is a supported fallback (it is a cache). No migration is
  needed (no schema change; the purge query change is forward-compatible
  with pre-merge rows, which the LIKE clause simply does not match).
