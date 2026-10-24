# ADR 0170: The conversation invariants — the A/V harness proves a conversation is spoken exactly right

- Status: accepted (implemented; live validation pending — see the run
  evidence in the section below and `/tmp/conv-invariants/REPORT.md`)
- Date: 2026-10-23
- Slug: conv-invariants

## Context and constraints

The A/V harness (ADR-0143, Amendment 1) already reports per-line latency,
missed/duplicate/stale/phantom lines and the ASR WER. It did not check the
owner's acceptance bar for a spoken conversation, which is a property of the
CONVERSATION as a whole, not of single lines:

1. **Spoken exactly once** — each page the player saw has exactly one
   utterance covering it (a catch-up merged utterance counts as covering
   each page it merges). Two utterances of the same page (same
   message+page, or two ASR transcripts ≥ 0.8 similar within 20 s) is a
   DUPLICATE.
2. **No read-ahead** — an utterance's speech must not contain words of a
   page that was not yet on screen when its audio started (the ASR
   transcript and the speech.requested text compared against the box text
   visible at audio start); the utterance text must be ⊆ the page text.
3. **Order** — pages are spoken in the order shown.
4. **Queue, don't cut** — when the player advances while page N is still
   playing, page N's audio is not cut off (its speech.ended result is
   `ended`, its acoustic duration ≥ 85% of its synthesized duration) and
   page N+1 starts within 800 ms of page N's end. This rule only means
   anything on a SLOW reader: the existing rapid-A drive advances by
   design faster than the speech (ADR-0122's catch-up exists for exactly
   that), so a cut on the rapid drive is the expected, tested behaviour.
5. **Page-break sentences** — for a page whose text ends without terminal
   punctuation (the sentence continues on the next page), the gap between
   the two utterances is reported, and a gap > 1.5 s is flagged
   `PAGE_BREAK_GAP` (report-only, not a gate failure).

Multi-page messages exist in the harness's savestate conversations (the
deck/Aryll conversation).

## Options considered

- **Check the invariants in the report writer only.** Rejected: the report
  is evidence, and the owner's bar is a gate — a run that speaks a page
  twice must fail `make test-av`, not just print a table.
- **Weave the checks into `avBuildTimeline`.** Rejected: the invariant
  rules (similarity windows, page ordinals, the queue bounds) are policy,
  not correlation; keeping them as pure functions over the timeline types
  (`avLineRecord`) keeps them unit-testable on synthetic timelines without
  an emulator, and lets the live harness call one function.
- **Measure the queue rule on the rapid drive.** Rejected: the rapid
  drive's advance-into-playing behaviour is the catch-up policy's input
  (ADR-0122); the rule needs a drive where the player finishes the page —
  hence the slow-reader drive mode, and the rule gates on slow runs only.
- **Record the synthesized duration with a file sink.** Rejected for the
  harness: its whole point is the OBSERVABLE audio (the isolated PipeWire
  null sink + shared-clock capture stand in for the hardware speaker,
  ADR-0143). A file sink would make the run inaudible.

## Decision

The invariants are pure functions over the harness's timeline types, in
`internal/e2e` (the harness's test-only convention):

- `convCheckDuplicates`, `convCheckReadAhead`, `convCheckOrder`,
  `convCheckQueue`, `convPageBreakGaps`, grouped by
  `convCheckConversation(lines, pages, slowDrive)` — which returns the
  gate issues (DUPLICATE, READ-AHEAD, OUT-OF-ORDER, and CUT-OFF on the
  slow drive) and the report-only issues (PAGE_BREAK_GAP, plus
  READ-AHEAD-NOISE — transcript words outside the page that the ASR WER
  gate judges as fidelity, not read-ahead). Pinned on synthetic timelines
  by `TestConvInvariant*` (duplicate, read-ahead, out-of-order, cut-off,
  page-break gap, gate/report split).
- **Page ground truth**: each message's page texts in commit (stream)
  order, from the session's committed pages (`avConvPages` over the
  timeline's committed texts). Each page turn mints a new occurrence under
  the same message id; the line's ordinal among its message's occurrences
  is its page ordinal. Line 5 log-safety holds: findings name ids,
  ordinals and timings, never page text.
- **No-read-ahead word accounting**: word multisets (lowercased,
  punctuation-trimmed tokens; the player-name placeholder is dropped from
  the page side, and one unexplained word per side is admitted as the
  player-name substitution). A word of a LATER page (ordinal past the
  covered pages) is READ-AHEAD; remaining words outside the covered pages
  are READ-AHEAD on the deterministic requested-text side (the utterance
  must be ⊆ the page text) and report-only noise on the acoustic side.
- **The synthesized duration** is tapped, not recorded to disk: the
  harness's session sink is the pipewire sink built through the runtime's
  `SinkFactory` test seam wrapped by `avSynthDurSink`, which decodes each
  utterance's WAV before handing it to the device and records its full
  duration per utterance ID (a line's own occurrence; a merged run's
  occurrence — the batched lines' `RunOccurrenceID`). The observable
  audio, the null sink and the capture are untouched.
- **The slow-reader drive** (`avDriveSlow`, selected by
  `GN_AV_DRIVE=slow|rapid`, default rapid so `make test-av` is unchanged):
  it shares the rapid driver's box tracking (text.observed readings,
  commits, terminals, the box-gone end condition, the settle phase) but
  inverts the press rule — A is pressed only after the current page's
  speech terminal + 500 ms (the player finishing the page before
  advancing). A routed line that never terminals is waited on, an
  abstained (no-speech) line advances on a stable box, and a lost press is
  retried bounded, like the rapid drive.
- The harness stores the findings on the report (`ConvDrive`,
  `ConvGateIssues`, `ConvReportIssues`), the report.md gains a
  "Conversation invariants (ADR-0170)" section (gate issues, then
  report-only findings), and `avGateFailures` fails the run on every gate
  issue. The route mode (ADR-0162's scripted play loop) does not drive
  the conversation page by page, so it runs no invariants.

## Validation

- Unit: `TestConvInvariant*` (synthetic timelines) and the gate unit
  tests (a gate issue fails the run; report-only findings never gate).
- Live: `make test-av` (rapid) and `GN_AV_DRIVE=slow make test-av` on the
  real Dolphin + real providers, reported in `/tmp/conv-invariants/REPORT.md`
  with the measured numbers (see there for the run ids and the per-invariant
  results).

## Tradeoffs and consequences

- The duplicate rule's transcript half uses a word-multiset Dice floor
  (0.8) within a 20 s window: it catches a re-spoken page under a
  different id, at the cost of flagging two genuinely similar short pages
  spoken close together — the page-text comparison (identical committed
  texts at both ordinals) disambiguates the same-message case.
- The queue-don't-cut duration half is unmeasured, not failed, when the
  tap recorded no duration for the utterance (e.g. the sink failed before
  the tap) — the terminal half still gates.
- The page-break gap is report-only: a long gap there is usually the
  player (the drive's 500 ms hold plus TTS scheduling), not a defect —
  unless a real run shows a systemic gap, in which case the wiring is the
  defect and the row is the evidence (a gate decision change would then
  amend this ADR).
- Slow runs are longer by design (one page per speech); the drive deadline
  is 20 min.

## Reversal plan

Revert the `av_*` changes: drop the invariant section from the report and
gate, the `avDriveSlow`/`avSynthDurSink`/`avConvPages` helpers, and the
`GN_AV_DRIVE` env read — the rapid drive and the ADR-0143 gate are
untouched by this ADR, so the revert restores the previous harness
exactly.
