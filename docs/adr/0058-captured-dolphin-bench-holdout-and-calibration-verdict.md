# ADR-0058: Captured-Dolphin bench holdout — the M7 real-frame evidence and the calibration verdict

Date: 2026-10-18
Status: accepted
Owners: agent (M7 bench/calibration)
Related requirements/tests: docs/spec/12 "Frame replay", "Metrics" and
"Calibration" (provisional objectives), docs/spec/16 M7 ("benchmark targets met
or ADR-adjusted with evidence"), docs/spec/13 evidence correlation, ADR-0035
(calibration: a refusal is a verdict, never a T), ADR-0038 (bench harness and
the scripted speech sidecar), ADR-0054 (Dolphin fixture factory), AGENTS.md
rules 3 (provenance classes) and 5 (no game material in tracked files).
Tests: `TestCapturedBench*` (internal/bench/captured_test.go), `TestBenchE2E`
(unchanged synthetic assertions), `TestTop1GradingWindowTrailingMessage`
(internal/bench/tail_message_test.go, amendments 2 and 3),
`TestTraceRingOverflowCountsDrops`/`TestTraceRingDefaultKeepsShortRuns`
(internal/pipeline/runner_test.go, amendment 3), `make bench`.

## Context and constraints

M7's gate is "benchmark targets met or ADR-adjusted with evidence", and the
evidence must distinguish provenance classes (AGENTS.md rule 3). The synthetic
holdout (class (a)) had been running since ADR-0038; M7 requires a class (b)
captured-frame replay holdout reported separately, and a calibration fit over
the combined real-frame observations.

Inputs for this ADR:

- **Fixture**: `artifacts/fixtures/windwaker-newgame` (gitignored, AGENTS.md
  rule 5) — 25 frames at 640x480 from a real Dolphin drive, sampled on a
  ≈2.1 s grid, with per-frame oracle truth (`box_present`, `text`) and a
  manifest carrying each frame's SHA-256. It contains two dialogue lines:
  line *a* present in 3 consecutive frames, line *b* present in 1 frame (the
  fixture's last), and empty (non-dialogue) frames between and around them.
- **Script**: imported at run time from the user's Wind Waker image via the
  windwaker adapter (the image's SHA-256 must match the manifest's; the image
  is never stored or committed). The Base-Master-Game script carries **no
  speaker labels**, so every committed message answers with speaker `unknown`.
- **Calibration data**: the synthetic tuning split (95 samples) plus the
  captured fixture (3 samples).

## Decision

1. **A captured holdout lives in `internal/bench`** (`captured.go`, new CLI
   subcommand `bench captured`). It verifies every fixture frame against the
   manifest's per-frame SHA-256 before running, builds the matcher index from
   the run-time-imported script, drives the same real pipeline (Tesseract,
   tracker, matcher, session) over the fixture's frames with oracle-driven
   region presence, and reports the docs/spec/12 metrics **separately** from
   the synthetic holdout, labelled class (b) captured-frame replay in the
   report and manifest. `make bench` runs both holdouts and prints **NOT RUN**
   with the two setup steps (fixture dir, image path) when either is absent —
   never a pass.

2. **The sidecar answers each commit during the replay, not after it.** The
   session's speech lifecycle refuses a start whose occurrence the box no
   longer stands behind (`session/apply.go`, `unknown_occurrence`). The old
   sidecar answered all commits after the frame loop, so any captured sample
   whose segment contains its advance frame (its region is already `DISMISSED`)
   could never speak: `speech.started` was silently dropped and the
   speech-scheduling metrics (precision, duplicate, stale) were permanently
   n/a. Answering at the commit moment — the product's decision provider and
   TTS worker run concurrently with the capture; this is that, scripted —
   restored the metrics. The synthetic sidecar's numbers are unchanged by the
   move (n=48 for every speech metric, identical values), because its samples
   never drop region presence.

3. **Measured result on real frames** (this fixture; `make bench` run
   20261018T094941Z, tesseract 5.5.3, 3 samples):

   | metric | value | n | objective | met |
   |---|---|---|---|---|
   | recognition_recall | 50.0% | 2 | ≥95% (provisional) | **not_met** |
   | recognition_precision | 100.0% | 1 | — | informational |
   | ocr_cer / ocr_wer | 0.0% / 0.0% | 2 | — | informational |
   | top1_exact_accuracy | 100.0% | 1 | ≥95% (provisional) | met |
   | abstention_rate | 50.0% | 2 | — | informational |
   | named_speaker_accuracy | n/a | 0 | ≥95% (provisional) | n/a (no labels in BMG) |
   | positive_speech_precision | 100.0% | 1 | ≥99% (provisional) | met |
   | duplicate_speech | 0 | 1 | none (provisional) | met |
   | stale_speech_after_advance | 0 | 1 | zero (provisional) | met |
   | commit_latency p50/p95 | 4436 ms (bound 6663 ms) | 1 | — | resolution-limited |

4. **Target adjustment for the one not-met line (the M7 "ADR-adjusted with
   evidence" clause).** Line *b* appears in exactly **one** sampled frame.
   The tracker must observe a reveal and a stable prefix across observations
   before it reaches READY and commits; a single sampled frame can never
   commit, whatever the pipeline does. In game the line is on screen for
   seconds; the fixture's 2.1 s sampling happened to catch it in one frame.
   The miss is therefore a **sampling artifact of the fixture, not evidence
   that the pipeline fails on player-visible dialogue**. Adjustment: the
   ≥95% recall objective is graded over lines sampled in ≥2 frames (here 1/1
   = 100%); single-frame lines are reported as not_met as measured and the
   50% figure stands in the report. The adjustment expires when the recording
   resolves it: capture at ≥10 Hz (≥3 frames per line) with the fixture
   factory (ADR-0054) and re-bench.

5. **Calibration: NOT FITTED, T = 1 retained** (ADR-0035: a degenerate fit is
   a refusal, never a fitted T). Combined fit data, per source:

   | source | class | used | incorrect top-1 | skipped |
   |---|---|---|---|---|
   | bench-synthetic-001 (tuning split, 95 samples) | a | 92 | 0 | 3 |
   | windwaker-newgame (captured, 3 samples) | b | 2 | 0 | 1 |

   94 fit samples with **0 incorrect top-1** across both sources; the fit
   requires ≥50 samples and ≥10 incorrect top-1 examples (and refuses a
   boundary minimum). A 100%-correct candidate set leaves the NLL flat in T,
   so any fitted T would be a choice, not a measurement. **What the recording
   must provide to unblock the fit**: labelled frames where the matcher's
   top-1 genuinely errs — OCR-degraded lines (blur, shadow, partial reveals),
   near-duplicate message pairs, font/zoom variants — i.e. M6
   fixture-factory captures with per-frame labelled error data. The synthetic
   tuning set is already saturated at 100% top-1, so error data must come
   from real frames.

## Tradeoffs

- The captured holdout's n is small (3 samples). It is honest evidence of the
  pipeline's *behaviour* on real frames (OCR fidelity, commit correctness,
  speech scheduling), not statistical strength; the report says so in its n
  columns.
- The sidecar timing fix changes both holdouts' speech path; the synthetic
  numbers are verified unchanged before and after.
- Named-speaker accuracy is n/a by construction of the BMG script (no
  speaker labels); it stays n/a until the recording or a labelled script
  provides ground truth.
- The single-frame recall adjustment (item 4) is the only objective modified
  in this ADR, and it is scoped to lines sampled in <2 frames.

## Validation

- `gofmt -l internal/bench` (empty), `go vet ./internal/bench/`,
  `go test -race -count=1 ./internal/bench/` — ok (fixture load/SHA checks,
  segment construction, truth mapping, Levenshtein, NOT RUN contract,
  end-to-end captured run on the fixture).
- `make bench` with the fixture dir and image configured: synthetic holdout
  (49 samples) — recall 100% (n=48), top-1 100% (n=48), named speaker 100%
  (n=34), speech precision 100% (n=48), duplicate 0 (n=48), stale 0 (n=48),
  commit latency p50 200 ms (n=48, resolution-limited at 10 Hz); captured
  holdout — the table in item 3.
- `make bench` without the fixture dir or image: both NOT RUN branches print
  the setup steps (verified with the env vars unset).
- `bench calibrate` over the combined sources: prints
  `NOT FITTED: … 94 tuning samples with 0 incorrect top-1 …` with the
  per-source breakdown and writes no calibration.json.

## Consequences and reversal

- `make bench` now covers both provenance classes; M7's gate evidence is the
  two reports plus this ADR's adjustment and the calibration refusal.
- The calibration refusal is a live target: the unblock plan is a number (≥10
  labelled incorrect top-1 examples from real frames), owned by the M6
  fixture-factory recording.
- Reversal: delete the `bench captured`/`bench calibrate --fixture` surfaces,
  `internal/bench/captured.go`, and the Makefile's captured block, and restore
  the sidecar's end-of-replay answer loop; the synthetic holdout and its
  metrics are unaffected by either.

## Amendment 2 (2026-10-18): the dense holdout's single top-1 miss (cx0014) is a
grading-window artifact, not a matcher defect; the objective is graded on the
display window (measured 19/19)

The `windwaker-legend-dense` holdout (run 20261018T194640Z; two deterministic
re-runs agree) measures top-1 17/18 = 94.4% against the ≥95% provisional
objective. The one miss is sample cx0014. The diagnosis below is measured
(replayed sample by sample through the real pipeline; message references are
stable BMG source ids — no game text, AGENTS.md rule 5):

- cx0014's line is BMG source message **471** (oracle text 72 runes; retrieval
  key 71 runes), present in fixture frames 296–306 (the fixture's own
  classifier: `story_subtitle`/`storysub`). The segmentation rule extends the
  span to the next occurrence to exercise the advance/duplicate-speech paths,
  so the span carries a 9-frame boxless tail (fixture frames 307–315): 4
  cutscene frames and 5 frames the fixture factory classifies
  `prompt`/`continue:yes`.
- On those 5 prompt frames the subtitle bar renders the game's continuation
  prompt, which is itself an in-script message: BMG source message **472**
  (31 runes; 6 words; word rune-lengths 6,3,4,3,3,7; 4 × U+002E; leading
  3-dot run; trailing two alphanumerics + U+002E). The pipeline's OCR reading
  of those frames is a 31-rune string whose retrieval key is byte-identical to
  message 472's stored key (a unique exact-table hit) — a verbatim rendering
  of a script message, not a hallucination (the fixture's per-frame
  classifier independently labels the same frames `prompt continue:yes`).
- The matcher is correct on every observable line of the sample: while line
  471 is displayed its top-1 = 471, `MATCH_EXACT`, text component 1.0,
  top-1 probability 0.6328 (the line's OCR reading is 0 characters off the
  oracle at the first OCR call; the tracker-stable reading differs from the
  oracle only in three case folds and one line-break-to-space join — all
  folded by the retrieval key by design). On the prompt frames the top-1 =
  472, `MATCH_EXACT`, text component 1.0, probability 0.6912 — the reported
  miss (top_prob identical to four digits).
- `top1_exact_accuracy` grades each in-script sample on the LAST ranking row
  of the whole sample span (the harness's `sampleOutcomes.topID`). For cx0014
  that row is the prompt-frame ranking of 472 — a different, correctly
  recognized in-script message — hence the 17/18.
- Companion denominator artifact, same tail design: cx0020 (final line;
  680-frame span, 580 of them trailing `prompt continue:yes` frames) overflows
  the pipeline's 512-row trace ring, so the harness readout keeps no ranking
  rows for it (all 512 retained rows are box-absent observation rows) and the
  sample drops out of the top-1 denominator (18 of 19 in-script samples are
  "ranked"). Its truth was nonetheless committed first (recall 19/19): the
  committed message is the ranking's top candidate (`session/apply.go`
  decide), so the truth was top-1 in its own display window.

**Verdict.** The ≥95% top-1 objective is met on the display window:
**19/19 = 100%** of in-script samples have their truth message accepted as
top-1 while it is on screen (the commit-side reading of the same rankings;
recall 19/19 is that reading). The raw last-row figure stays **17/18 = 94.4%
not_met in the report** — it is not hidden, and no matcher threshold or fold
is tuned to move it: no change in `internal/matcher` or
`internal/games/textnorm` is warranted by this evidence, and the synthetic
holdout (48 samples) and every other metric are unaffected. The calibration
verdict is unchanged: NOT FITTED (0 incorrect top-1 among fit-eligible
samples; the fit requires ≥10).

**Reproduction.** `TestTop1GradingWindowTrailingMessage`
(internal/bench/tail_message_test.go) reproduces the class on class (a)
synthetic frames with invented text: a sample whose trailing frames show a
different in-script message. It asserts the shown line commits first, the
trailing line is ranked top-1 while displayed, the last-row grading window
records exactly the cx0014-shaped miss, and the scripted speech side speaks
the truth line exactly once.

**Reversal.** If a future fixture/grading change trims the span to the
line's display window (or re-grades the top-1 readout on it, per a new ADR),
raw and adjusted figures coincide and this amendment's adjustment expires.

## Amendment 3 (2026-10-19): top-1 is graded on the truth line's display
window; the legacy last-row figure stays visible; the harness loses no trace
rows (ring overflow is a loud failure, not a shrunken denominator)

Amendment 2 measured the ≥95% top-1 objective on the display window (19/19)
and kept the raw last-row figure (17/18) in the report as not_met. This
amendment makes the display window the *graded* reading — so both holdouts
are graded one way — and fixes the companion denominator artifact (cx0020
overflowing the 512-row trace ring and dropping out of the top-1 denominator).
No matcher threshold or fold changes; no change in `internal/matcher` or
`internal/games/textnorm`.

**1. The display window (precise definition).** For an in-script sample the
display window is the set of frames the oracle says the truth line is on
screen:

- **class (b) captured**: the fixture's per-frame oracle rows (truth.json) —
  the rows where `box_present` is true **and** the row's line label equals the
  span's line label. Within a span's range the segmenting rule guarantees every
  line row is the span's line, so the window is the line's frames exactly and
  the trailing frames (the next occurrence's lead-in, which carry a different
  label or no box) fall outside it. This is cx0014's shape made structural:
  the trailing prompt frames are outside line 471's window.
- **class (a) synthetic**: the sample's box-present frames (its text.png
  frames). A synthetic sample's trailing frames are boxless, so the window and
  the ranked span coincide there and the two classes are graded one way.

The harness carries this per sample (`Sample.DisplayWindow`, `json:"-"` —
oracle plumbing the harness computes from the fixture's labels, never truth
data; the truth.json schema is unchanged).

**2. The graded top-1 (precise definition).** Over the pipeline's ranking
trace rows (rows with a top candidate) whose driving frame — now stamped on
the row with `frame_seq`/`capture_timestamp_ns` (the frame that drove the
ranking dispatch) — falls inside the window's capture-ts range, the graded
top-1 is:

- the top candidate of the line's **committed/stable reading** when the sample
  committed: the last window row whose accepted text equals the first commit's
  stable text (the `text` of message.committed). The session's own commit rule
  makes this the commit's reading: `session/apply.go decide` commits the
  applied ranking's top candidate, and an applied ranking's text must equal the
  region's text (`stale_text` fence), so the commit's stable text is exactly
  the accepted text of the row that committed it;
- else (the sample did not commit, or no such row exists) the top candidate of
  the window's **last ranking row**.

A sample with no ranking row in the window — or no oracle window at all — is
graded on the legacy last-row reading (the last ranking row of the whole span),
never silently dropped. `top1_exact_accuracy` is graded on this reading and the
≥95% objective applies to it.

**3. The legacy figure is not hidden.** The old whole-span last-row reading
stays in the report as its own row, `top1_last_row` (informational, no
objective): the same in-script samples graded on the last ranking row of the
whole span. The report's Top-1 misses table records both readings per sample
(`window` | `last_row`), so a trailing in-script message (cx0014's prompt,
message 472) still reads the legacy figure down on the record instead of
disappearing. The calibration row's incorrect-top-1 count and the fit's raw
material (`collectFit`) are taken from the graded reading / the committed
stable reading, so the fit's evidence and the graded row agree.

**4. The harness loses no trace rows.** The pipeline's trace ring is a bounded
in-memory readout. It now exposes:

- `Config.TraceMax` — the ring's row bound (zero keeps the runtime default of
  512, which is **unchanged**: this is a harness option, not a runtime-default
  change).
- `Stats.TraceDropped` — the count of rows evicted from a full ring, so a
  caller that sized the ring can assert none were lost.

The bench harness sizes the ring per sample to `frames*4+64` (a frame
contributes at most three rows: one dispatch, one observation, one ranking)
and treats a non-zero `Stats.TraceDropped` as a **hard failure of that sample**
(loud NOT RUN with the eviction count), not a shed row to grade around. This
removes the cx0020 denominator artifact: its 680-frame span no longer overflows
the ring, so its ranking rows are retained and the sample is graded like every
other in-script sample.

**5. Measured effect (run 20261019; two deterministic re-runs agree).** On the
`windwaker-legend-dense` holdout the graded top-1 is **19/19 = 100% met** on
the display window (all 19 in-script samples now ranked — cx0020 retained),
while `top1_last_row` stays **17/18 = 94.4%** (cx0014's trailing prompt, the
amendment-2 evidence, still visible). The `windwaker-newgame` and synthetic
holdouts are unaffected (their windows coincide with the ranked span). No
matcher change.

**Reproduction.** `TestTop1GradingWindowTrailingMessage`
(internal/bench/tail_message_test.go) reproduces the class on synthetic frames
with invented text and now asserts the graded reading (truth counted correct on
the display window) while the `last_row` reading still records the trailing
message. The overflow guard is `TestTraceRingOverflowCountsDrops` and
`TestTraceRingDefaultKeepsShortRuns` (internal/pipeline/runner_test.go): a ring
sized below the run's rows retains exactly `TraceMax` rows and counts every
eviction, and the default ring keeps a short run with zero drops.

**Reversal.** Revert the graded reading to the last ranking row of the whole
span (drop the `top1_exact_accuracy`/`top1_last_row` split and restore the
single last-row row), remove `Config.TraceMax`/`Stats.TraceDropped` and the
harness's per-sample ring sizing, and un-stamp the ranking trace row. The
pipeline's runtime default ring size is untouched by this amendment either way.
