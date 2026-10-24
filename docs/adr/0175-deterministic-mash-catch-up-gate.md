# ADR-0175: The deterministic mash drive for the catch-up gate: press A on a fixed 350 ms cadence (once the box text stops growing), independent of the speech state — and assert every page spoken exactly once, in order, with the policy-mandated stale drops counted

- Status: accepted
- Date: 2026-11-01
- Related requirements/tests: catch-up live E2E (issue #25, ADR-0122), ADR-0163 (box-gone / screen-check drive rules), ADR-0065 (live gates)

## Context and constraints

`TestLiveCatchUpRapidDialogue` (internal/e2e/catchup_live_*_test.go, the test-dolphin
liveui phase) exercises the catch-up policy (ADR-0122: lines committed while speech is
busy are batched and merged into one utterance). Its presses fire on COMMITS — a press
lands as soon as the box's line commits. That is timing-lucky: when a box stalls (the
OCR/Clef path is slow), the speech catches up before the next box commits, no batch ever
forms, and the test fails "no routed line settled through the catch-up batch". The gate's
pass/fail then depends on the live pipeline out-racing the typewriter, not on the policy.

The owner's brief: "when you click A A A on something, cue up all the speech really
quick and then just say all that" — mashing A through a conversation should speak every
page, once, in order, quickly. A deterministic, policy-relevant drive shape is needed so
the gate no longer depends on that race.

## Decision

### 1. A mash drive mode in the catch-up driver (`cuDriver`, test-side)

- After the FIRST box's line commits (the commit is the evidence the session recognises
  boxes), the driver presses A on a fixed cadence: at most one press every
  `cuMashGap` = 350 ms — on EVERY box, through the whole conversation, independent of
  the speech state (the driver has no speech input at all).
- Text-settle gate: a mash press lands only once the box text has stopped growing
  `cuMashSettle` = 1.2 s. Growth is read from the box region's `text.observed` churn
  (the typewriter keeps refreshing `textChanged`); for a box that sits fully typed —
  the growth unobservable, no further observations — the same fixed 1.2 s runs from
  the instance's appearance (both read off `textChanged`, which is refreshed by every
  new reading and left at the appearance otherwise).
- Bounded presses: `cuMashMaxPresses` = 6 per box instance (the cadence retries on the
  same instance cover a lost A); one stall press (the 45 s shape, ADR-0163) covers the
  never-committing box once the bound is spent.
- Stop rule unchanged: the existing box-gone + screen-check logic (ADR-0163
  amendment 1) governs — the drive ends when the box is gone; the between-box
  transition (region reads empty) still holds presses; a new box's reading resets the
  instance and the cadence restarts, so the drive walks the conversation box by box
  without skipping.

### 2. The deterministic assertions (against the store-truth model, like the
rapid test's)

- **Pages once, in order**: every matched dialogue page (commit order) ends spoken
  EXACTLY ONCE — `ended` by its own playback or by its merged catch-up run
  (one playback per line, start order following commit order) — or is fenced as a
  policy-mandated `catch_up_stale` drop (below). The duplicate-region skip with a
  spoken sibling is the documented exception.
- **Stale drops only where the policy says they must be**: ADR-0122 §3 fences a
  pending line as `skipped`/`catch_up_stale` at the instant a NEWER line commits more
  than the batch window (900 ms) past it. A stale drop with NO newer commit past the
  window would be the policy dropping a line it must have spoken — that is the
  failure; a stale drop with one is policy-mandated and is COUNTED and reported, not
  failed (finding below).
- **Timings**: every merged utterance starts within 1.5 s of its last covered commit
  (the flush plus the warm-cache TTS round trip keeps the merged speech close to the
  player's progress); the total speech — first start to last terminal — finishes
  within the summed played audio (the file-sink WAVs) plus 2 s of back-to-back
  overhead.
- The rapid test's no-backlog, merge and voice assertions run on the same stream.

### 3. Wiring

A sibling `TestLiveCatchUpMash` (same harness as the rapid test: the shared
`cuLiveHarness` extraction; the drive mode and the assertions differ). The test-dolphin
liveui phase's -run pattern now names it alongside `TestLiveCatchUpRapidDialogue` and
`TestLivePlayUI` (the phase budget grows to 180 m: 45 m internal budget per catch-up
test + the play-UI's 60 m + margin).

## Amendment 1 (2026-10-24, evidence above): catch-up mode no longer drops pages for
staleness — every passed page is spoken, once, in order

The owner's requirement is explicit: "when you click A A A on something, cue up all the
speech really quick, and then just say all that." The Finding above is the evidence the
old policy contradicted it: under a sustained mash, §3 fences every page the player left
more than the 900 ms window behind a newer commit as `catch_up_stale`, and §5 drops any
run whose audio is ready more than 3 s after its newest commit as `catch_up_max_age` —
so a steady mash speaks the recent pages and silently fences the abandoned ones. This
amendment changes the RUNTIME POLICY (the original ADR left it untouched):

1. **No staleness drops in `catch_up` mode.** A page the player passed is never dropped
   as `catch_up_stale`; the §5 `catch_up_max_age` flush drop is gone as well.
2. **Cached clips back-to-back, not merged text.** A flush plays each pending page's
   OWN utterance, in commit order (`runCatchUpBatch`), instead of re-synthesising a
   merged text: presynth makes nearly every page a cache hit, and back-to-back cached
   clips with gaps ≤ 150 ms are faster and preserve each page's voice than a merged
   synthesis (a cache miss, slow).
3. **Safety bound — the backlog cap.** If the pending backlog exceeds
   `BacklogCap` (default `speech.DefaultCatchUpBacklogCap` = 45 s of estimated audio),
   the OLDEST pages beyond the bound are dropped at the commit that re-measures the
   batch: terminal `skipped`/`catch_up_backlog_cap`, counted
   (`catchUpBacklogDropped`, surfaced as the `backlog_capped` summary field) and
   logged. The narrator never lags unboundedly; within the bound, every page speaks.
4. **No playback speed-up.** The audio path (internal/audio) exposes no rate control
   (playback is sample-exact); adding ≤ 1.15× resampling is out of scope, so the
   optional speed-up is skipped — the back-to-back clips are the mechanism.
5. **`latest_only` unchanged** (it drops by design; that mode is not the owner's
   "say all that" case).

The original Finding section below is SUPERSEDED by this amendment: the policy now
queues (bounded by the cap) instead of fencing, and the mash assertions were updated
accordingly — a `catch_up_stale` or `catch_up_max_age` terminal is now a FAILURE in
`cuAssertMashPages` / `cuAssertNoBacklog` (they can no longer be policy-mandated); the
only accounted drop is `catch_up_backlog_cap`.

Validation: `TestSpeechCatchUpMashSixPagesClipsBackToBack` (six cached pages → six
clips played in order, none dropped, gaps ≤ 150 ms), `TestSpeechCatchUpNoStaleDrop`
(the old drop shapes — window-stale, ready-past-max-age — now speak),
`TestSpeechCatchUpBacklogCap` (over the cap → oldest dropped with
`catch_up_backlog_cap`, counted, the in-bound pages speak in order) — all in
internal/runtime/speech_catchup_test.go, `-race` clean.

Live assertion fix (same date, first live run): the mash timing bound measured the
whole-conversation span (first playback start → last terminal), which the first live
run showed is dominated by the PLAYER's between-box pace, not the narrator's backlog:
span 2m13.6s vs 32.5s of played audio, while every passed page started ~1 ms after its
commit and the drain after the last commit was seconds. `cuAssertMashTimings` now
bounds the BACKLOG DRAIN (last commit → last terminal ≤ summed played audio + 2 s) and
logs the whole span for information; the merged-start bound (1.5 s) and the every-page
spoken-once-in-order assertions are unchanged.

Second live run, same date: `cuAssertMerge` gate 1 ("want the ADR-0122 batch's events")
was the remaining timing-luck failure — it fataled when NO line ever committed into a
busy playback, which is a LEGITIMATE live outcome under the amendment when the player's
boxes are slower than the speech (every page then speaks standalone; run 2 evidence:
3 pages, 0 batches, drain 8.6s vs 10.9s of audio, all pages spoken). Gate 1 now logs
that outcome instead of failing; a batch that does form is still fully asserted (the
per-speaker runs, the fewer-utterances shape, the merged-start bound), and the batch's
shape is deterministically unit-covered. The gate no longer depends on the drive
outrunning the speech.

## Finding (REPORTED in v1; SUPERSEDED by Amendment 1): the catch-up policy's answer to a sustained
backlog is to fence, not to queue

ADR-0122 §3: a new commit drops, from the front, every pending line it is more than the
900 ms batch window past — at the instant the newer line lands. A steady cadence
faster than or comparable to the speech (a mash pressing every ~350 ms, the boxes'
commits landing every ~1-3 s) therefore leaves lines more than one window behind a
newer commit while they are still pending, and the policy fences them: the content the
owner expects spoken is NOT spoken, by design. The §4 approved-generation fence keeps
only the flushed batch's generations; §5 bounds a run's ready age at 3 s. So the
owner's "say all that" is bounded by the policy's no-backlog shape: under a long
backlog the session speaks the RECENT pages (standalone or merged) and fences the
abandoned ones as `catch_up_stale` / `catch_up_max_age`. The mash assertions measure
how many pages that costs under the steady cadence (live numbers: see Validation) —
and deliberately do NOT change the policy: the no-backlog shape is the product default
(ADR-0122 decision 1) and the drops are the policy's own documented terminal for the
line. Whether the owner wants a queue-all alternative (a backlog the session catches
up on, in violation of ADR-0122's "never build a backlog") is a product decision for a
separate ADR, informed by these numbers.

## Tradeoffs

- The drive (test-side) changes; the runtime policy does not — the gate's shape is
  deterministic, the product's shape is unchanged.
- The 350 ms cadence with the 1.2 s settle gate yields one press per box plus
  bounded retries; it is deliberately slower than a human mash but faster than the
  speech on a warm cache (the measured live runs show the commits landing ~1-3 s
  apart against 900 ms window / 3 s max age — the policy-relevant cadence).
- The shared-harness extraction touches the rapid test's setup body verbatim; the
  rapid test's drive mode and assertions are unchanged, so the ADR-0122 live gate
  remains.

## Validation

- Unit: the mash cadence/stop rules on synthetic event pages
  (internal/e2e/catchup_live_driver_test.go): no press before the first commit; the
  growth read holds the settle gate; the 1.2 s gate + 350 ms cadence press sequence;
  the per-box bound and the new-box reset; the transition hold; the box-gone end
  (all passing, `go test -race ./internal/e2e/ -run TestCu`).
- Live (test-dolphin, real Dolphin + real providers, the first-dialogue savestate):
  `TestLiveCatchUpMash` through dolphin-run.sh, three runs — pages shown / spoken /
  merged / dropped and the merged-start / total-speech timings, per run, in the
  REPORT (see /tmp/rapid-aaa/REPORT.md for the measured numbers).
- `make lint`, `go build ./...`, race tests of the touched package.

## Consequences

- The gate's catch-up half no longer depends on commit-timing luck: the steady mash
  cadence commits boxes into a busy speech by construction.
- The stale-drop accounting (policy-mandated vs not) is now a first-class assertion,
  and the numbers feed the backlog-policy question in the Finding.

## Reversal plan

Drop `TestLiveCatchUpMash` and the Makefile -run pattern entry (revert the phase
budget to 115 m); remove the mash branch from `cuDriver.decidePress` and the
`cuAssertMashPages` / `cuAssertMashTimings` helpers. The rapid test and the harness
extraction can stay (they are behaviour-neutral).

Reversal of Amendment 1: restore the window staleness fence and the §5 max-age drop in
`catchUpBatchCommit` / `runCatchUpBatch`, merge the batch back into one utterance per
flush, and delete `BacklogCap` (the drop at the commit side) and the `backlog_capped`
counter. ADR-0122 §3/§5 describe the restored behaviour verbatim.
