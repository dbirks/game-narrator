# ADR-0162: The play loop: scripted Dolphin routes through the A/V harness

- Status: accepted
- Date: 2026-10-22
- Issue: #41

## Context

The owner asked for a loop the agent can iterate on alone: walk up to a character in Dolphin, press A, wait, and judge the result. The questions are whether audio is generated, whether it comes out right away or after ten seconds, and whether it sounds like a man, a woman or a child. Walking around should also show whether the narrator makes random "ah" sounds; the board had spoken "A", "a", "4" and "j" through the unmatched read-through before its plausibility gate. Dialogue at the top of the screen needs the same checks.

The A/V harness (ADR-0143) already recorded Dolphin's display and the narrator's audio on one clock, and scored latency, ASR and voice per line. But its only driver was the catch-up A-press loop over one savestate.

## Decision

**Route mode.** `GN_AV_ROUTE=<route.json>` (`make play ROUTE=…`) replaces the A-press loop with a scripted route:
- a savestate, relative to the fixture root;
- steps: `{"stick":[x,y],"ms":N}`, `{"press":"A"}`, `{"wait_ms":N}`, and `{"mark":"label"}` naming the segment the step begins.

A route is a `dolphin.Script` run by the existing runner and bridge. There is no second input path. The latency gate is off by default in route mode (`GN_AV_NO_LATENCY_GATE=1`), because a route measures behaviour, not a benchmark.

**Per-utterance report.** Every line row carries:
- its segment: the nearest preceding mark, at the step's real start instant from the runner trace;
- box presence and position;
- box→audio latency;
- the ASR transcript and WER;
- the voice;
- a pitch class: median F0 by YIN (`speech.MeasurePitch`). Below 165 Hz is "low (adult male)", 165–255 Hz "mid (adult female)", above 255 Hz "high (child)".

**Phantoms first.** An utterance with no box on screen at its audio start, or inside a `walk*` segment, is a PHANTOM row at the top of report.md, and the run fails on it.

**Starter route.** `testdata/routes/outset-deck-walk.json` boots Aryll's lookout deck, talks to Aryll first (the savestate opens facing her with the "Speak" label up), then walks the deck and idles with the HUD's A icon on screen.

## Fixes that landed with it (found by running it)

- **Checkpoint-free steps.** `dolphin.RunScript` never marked such a step OK, so every plain stick, press or wait step "failed" with an empty reason. Existing scripts all used checkpoints. Test: `TestRunScriptCheckpointFreeStepsSucceed`.
- **Segment attribution.** Marks were offsets from a zero `time.Time`, so every utterance fell into the last segment and a real line was reported as a phantom. Marks and presses are now read off the runner trace after the run. That also removes a data race: the run goroutine filled `presses` after signalling completion.
- **Pitch.** The first `MeasurePitch` was not YIN and divided the period by two. A child's 274 Hz voice read as 533 Hz, which would class a woman as a child and a man as a woman. It is now standard YIN (cumulative-mean-normalized difference, first dip under 0.12, parabolic interpolation, 45 ms frames). Test: `TestMeasurePitchNoOctaveError` (110/210/300 Hz within 5%).

## First findings (2026-10-22, real Dolphin + real providers)

- **No phantoms.** Walking and idling with the A icon on screen produced 0 phantom utterances.
- **Aryll's voice reads "high (child)".** The character voice measured 340 Hz, and my independent check matched.
- **Literal placeholder.** `{player}` reached TTS as written and was spoken "player" (#48).
- **Early commit.** A box committed mid-typewriter, at a punctuation and colour-word pause, so its speech was the visible prefix "…And I'm sure everyone". The same prefix was spoken again at the next generation. This is the next defect to work with this loop.
- **Row pairing.** The harness pairs a row's audio by message ID, so the boxes of one multi-box message mix and show negative latencies. It needs pairing by occurrence.

## Consequences

The agent can change code and re-run `make play ROUTE=…`, then read the report and the frames (montage) to judge each change.

Routes that reach other characters, including one with a top-of-screen box, need either recorded savestates or the vision-guided approach from `telescope_test.go` promoted to a route step.

## Reversal

Route mode is opt-in by env var. Removing `GN_AV_ROUTE` handling and the `play` target restores the A-press harness unchanged.
