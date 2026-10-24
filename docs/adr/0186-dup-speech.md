# ADR 0186: The cross-region duplicate guard speaks a page once per box instance — window widened to the box's lifetime, gated on an intervening line

- Status: accepted (implemented; emulator validation in
  `/tmp/dup-speech/REPORT.md`)
- Date: 2026-10-24
- Slug: dup-speech

## Context and constraints

The cross-region duplicate guard (issue #41, `crossRegionDuplicate` in
`internal/runtime/speech.go`) exists because a profile can declare the same
dialogue box at two overlapping on-screen positions. Both regions then read
the same box and both commit the same message+page; without the guard each
commit is routed and the line is spoken twice (the A/V harness run
`av-20261021T190256Z` transcribed every such line twice).

The guard's original trigger: the same identity key (the message id, or
`<id>#p<ordinal>` for a multi-page message's page, or `rt:`+the read-through
text) routed from a DIFFERENT region within 3 s. It fired in one harness
run but missed the others, caught by the conversation-invariant gate
(ADR-0170) on the real emulator:

- slow drive, run `av-20261024T062050Z`: the same page spoken as line 10
  (middle region) AND line 11 (bottom region), 5.5 s apart; a second page
  as line 12 AND 13, 8.2 s apart.
- rapid drive, run `av-20261024T061536Z`: the same page as line 7 AND 8,
  7.6 s apart.

Why the 3 s window misses: the second region's commit does not arrive
100–200 ms after the first — it arrives after that region's own
settle/commit latency, 5–9 s after the first region's commit, while the
same box instance is still on screen and nothing else has been spoken.
A 3 s window is a guess at that latency, not a property of the situation.

The owner's acceptance bar (#41's owner): "making sure it doesn't happen
double time, like twice."

The opposite failure mode the guard must not introduce: a legitimately
repeated line — an NPC who says the same line again after a different line
in between, or after the box closed and reopened — must speak again.

## Options considered

1. **Widen the fixed window only (3 s → 20 s).** Simple, but over-suppresses:
   a cross-region re-show of the same page within 20 s (the box closed and
   re-shown, both regions reading it again) would be skipped even though
   the player saw a new box.
2. **Track box lifecycle (close/commit events) on the speech side.**
   Most faithful ("while the first box instance is still on screen"), but
   the speech side has no box-close signal today and adding one crosses
   into the commit/reducer glue another change owns; larger surface.
3. **Widened window + "no intervening different line" gate (chosen).**
   A duplicate is the same page committed from another region while the
   most recent routing is still that page and the first routing is inside
   the window. The moment ANY different line routes, the box has advanced
   (or the player moved on) and another region's same page is a repeat
   line, not a double-read. This encodes the observable property ("the
   first box instance is still the one on screen") with data the speech
   side already has.

## Decision

`crossRegionDuplicate` suppresses a commit iff
ALL of:

- the identity key was routed before from a **different region** (same-
  region re-commits stay the reducer's and the utterance table's concern —
  a same-region re-show is a new occurrence and must speak);
- the most recent routing (new `lastRoutedKey`, recorded on every routing,
  not updated by suppressions) is still the same key — no different line
  spoke in between;
- the first routing is within `crossRegionDupWindow`, now **20 s** — the
  box's practical lifetime under overlap (the conversation-invariant
  gate's duplicate bound is the same 20 s, so a run the gate would call
  a duplicate can no longer happen).

A suppressed duplicate keeps its one terminal — `speech.ended` result
`skipped`, reason `duplicate_region` — and settles its generation, as
before. The 256-entry bound on `recentRouted` is unchanged (marks are now
only pruned at 20 s, so the table holds ~a box-worth of pages longer, well
under the cap).

## Tradeoffs

- A same page re-shown from the *other* region alone within 20 s, with no
  different line in between, is still suppressed. In practice a re-show
  after a dismissal is followed by different dialogue (or the dismissing
  region's own new occurrence routes first, which re-arms the guard for
  the right copy). The owner's "spoken again" cases — repeat after a
  different line, re-shown box — are both covered and unit-tested.
- 20 s is a bound, not a measurement of box lifetime; the gate and the
  guard now share the same constant in spirit. If a game shows the same
  page in two genuinely distinct, same-region-less encounters back-to-back
  within 20 s, it would be under-spoken — no such case exists in the
  Wind Waker flow, and the A/V harness would surface one as a MISSING
  line.

## Validation

- `internal/runtime/speech_dup_window_internal_test.go` (new):
  - `TestCrossRegionDuplicateSlowCommitGap` — the bundle timing: middle
    routes the page, the bottom region commits the same page 5.5 s later
    with nothing in between; the second commit is skipped
    (`duplicate_region`), its generation settled, exactly one terminal.
    Failed before the change (both copies routed — the line spoken twice),
    passes after.
  - `TestCrossRegionDuplicateLegitRepeatsStillSpeak` — the same page from
    the other region AFTER a different line routed in between is a repeat
    line and speaks; the box closed and re-opened re-commits from the
    original region and speaks (while the box is still open, the other
    region's copy is skipped). Failed before the change in the
    intervening-line case (the 3 s guard suppressed a real repeat), passes
    after.
- `TestCrossRegionDuplicateSuppressesSecondRegionOnly` (the original guard
  test) still passes: same-window suppression, same-region repeat,
  out-of-window repeat, one terminal.
- Race: `go test ./internal/runtime/ -race` (the package).
- Emulator proof (ADR-0143 harness, ADR-0170 gate applied via
  `/tmp/conv-invariants/merge.patch`): `make test-av` rapid and
  `GN_AV_DRIVE=slow make test-av` under the shared Dolphin lock; the
  before runs reported 1 (rapid) and 2 (slow) DUPLICATE gate issues, the
  after runs must report 0 — measured in `/tmp/dup-speech/REPORT.md`.

## Consequences

- The duplicate-region skip now covers the slow cross-region commit gap
  that the owner heard as "said twice", while same-region repeats and
  repeat lines keep speaking.
- `lastRoutedKey` is a new `speechWiring` field under `w.mu`; no API,
  protocol, persistence or profile change.

## Reversal plan

Restore `crossRegionDupWindow` to 3 s and drop the `lastRoutedKey`
condition and field (one function, one field). The original 3 s behaviour
is fully recoverable; the new tests are updated or deleted with it.
