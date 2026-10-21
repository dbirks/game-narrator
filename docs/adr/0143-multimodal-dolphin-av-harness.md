# ADR-0143: Multimodal Dolphin A/V Harness (`make test-av`) and Dual-Key Character Voice Resolution

Date: 2026-10-21
Status: accepted
Related: issue #41, AGENTS.md (rule 2 "Maintain ADRs", rule 3 "No simulation theater", rule 4 "First reproduce, then repair"), docs/spec/05 (recognition & commitment), docs/spec/07 (voice lifecycle), docs/spec/12 (testing & evaluation), docs/spec/16 (implementation & acceptance), ADR 0023 (audio sink), ADR 0054 (Dolphin fixture gate), ADR 0101 (portable voice mappings), ADR 0109 (auto-cast), ADR 0122 (catch-up narration), ADR 0132 (per-character voice scenario), ADR 0135 (Cast redesign & shared voice count), ADR 0136 (unmatched read-through), ADR 0137 (placement-aware textbox region).

## Context

1. **Character voice lookup mismatch (`ID` vs `Name`).**
   Two voice-creation paths coexisted with different values in `voice_profiles.character`:
   - `profileio.ImportManifest` (ADR-0101) writes the roster character's **ULID** (`characters.id`) into `voice_profiles.character`.
   - The Cast UI and auto-cast workflow (`POST /api/v1/voices/auditions`, ADR-0109 / ADR-0135) write the character's **display name** (`characters.name`, e.g. `"Aryll"`) into `voice_profiles.character`, and `Store.ProfileCharacterVoiceCounts` (ADR-0135) joined solely on `vp.character = c.name`.
   - Meanwhile, at session startup (`internal/runtime/session.go`), `speakerByMessage` mapped each message ID to `resolveSpeaker(msg.SpeakerID)`, which returns `characters.id` (the ULID). When `speechWiring.approvedVoice` queried `ListVoiceProfiles(ctx, characterID)`, any voice created via the Cast UI or auto-cast (keyed by `c.Name`) returned zero rows, causing every line in live play to fall back to the `"narrator"` voice even when 10 character voices were approved on the appliance. Conversely, `ProfileCharacterVoiceCounts` missed voices keyed by `c.ID`.

2. **Observability of live narration timing and consistency.**
   Existing Dolphin E2E tests (`internal/e2e/dolphin_live_test.go`, `perchar_live_test.go`, `catchup_live_test.go`) verify event envelopes and file-sink WAV files separately, without a shared audio/video clock. When a user reports that live narration feels slow, skips a box, or speaks in the wrong voice, event timestamps alone do not show the exact visual frame where the textbox appeared/settled relative to the acoustic onset of speech on the audio sink. Furthermore, coding agents now have native multimodal video (`.mp4`) and audio (`.wav`) inspection capabilities (`read_file`), enabling direct visual and auditory verification of recorded gameplay alongside deterministic metrics.

3. **End-to-end latency budget.**
   In live play, the perceived delay from text settling in a dialogue box (`t_text_settled`) to audible speech (`t_audio_start`) was dominated by:
   - uncached TTS synthesis round-trips (~800–2500 ms on first play);
   - unconditional Clef adjudication round-trips (~500–800 ms) even when the local trigram/edit-distance matcher had a near-exact top-1 match;
   - cold OCR worker startup or oversized crop scales; and
   - a 600 ms catch-up batch window that was slightly narrower than Dolphin's ~779 ms commit cadence during rapid `A` presses.

## Options

1. **Event-only assertions + file-sink WAVs (status quo).**
   Rejected: cannot prove shared-clock visual-to-acoustic latency (`t_box_appear` / `t_text_settled` → acoustic RMS onset) and gives multimodal agents no synchronized video+audio container to inspect.
2. **External screen recorder only, without event correlation.**
   Rejected: without joining the session's `dev_events.json` (`text.observed`, `message.committed`, `decision.completed`, `speech.ready`, `speech.started`, `speech.ended`) onto the recording's wall-clock start `T0`, per-stage bottlenecks cannot be localized automatically.
3. **Shared-clock FFmpeg X11+PipeWire A/V harness (`make test-av`) + deterministic `timeline.json` + per-line `.mp4`/`.wav` clips + dual-key character voice resolution + cache/matcher/OCR/catch-up tuning (adopted).**

## Decision

### 1. Dual-key character voice resolution (`ID` and `Name`)
- In `internal/runtime/session.go` and `internal/runtime/speech.go`, `speechWiring` builds `speakerAliasByID` (`characters.id → characters.name`) from the profile's roster at session start.
- `speechWiring.approvedVoice(ctx, character)` first checks `ListVoiceProfiles(ctx, character)` (matching `characters.id` or `"narrator"`), and if no approved voice or fault is found, checks `ListVoiceProfiles(ctx, w.speakerAliasByID[character])` (matching `characters.name`).
- `Store.ProfileCharacterVoiceCounts` (`internal/store/status.go`) joins `voice_profiles` on `(vp.character = c.name OR vp.character = c.id)` so both Cast-created (name-keyed) and manifest-imported (ID-keyed) voices count consistently across the Library card, status bar, and Cast header.

### 2. Multimodal Dolphin A/V Harness (`make test-av`, `internal/e2e/av_harness_test.go`)
- Add `TestLiveDolphinAVHarness` in `internal/e2e/av_harness_test.go` and wire it to `make test-av`.
- **Isolated PipeWire null sink:** Create a temporary PipeWire null sink (`gn_av_<run-id>`) for the test run (cleaned up via `t.Cleanup`) and point the narrator's audio output (`SinkPipeWire` / `SinkPulse`) at that sink while Dolphin runs on its isolated `Xvfb` display (`:99`).
- **Shared-clock `.mp4` recording:** Launch `ffmpeg` capturing `x11grab` from the Dolphin `Xvfb` display at 30 fps (`libx264 -preset ultrafast -crf 23 -pix_fmt yuv420p`) and `pulse` audio from `gn_av_<run-id>.monitor` (`aac`) with `-use_wallclock_as_timestamps 1` into `artifacts/av-harness/<run-id>/session.mp4`, recording `T0` (wall-clock start).
- **Synchronized `timeline.json` and per-line clips:** Correlate every dialogue occurrence across `dev_events.json` and the recorded audio track:
  - `t_box_appear`: first `text.observed` for the box;
  - `t_text_settled`: last `text.observed` where text stabilizes before commit;
  - `t_commit`: `message.committed` timestamp;
  - `t_clef_done`: `decision.completed` timestamp (or `skipped` when local match is unambiguous);
  - `t_tts_ready`: `speech.ready` timestamp + `from_cache` boolean;
  - `t_audio_start_event`: `speech.started` timestamp;
  - `t_audio_start_rms`: first 20 ms window in the recorded audio track after `t_commit` whose RMS exceeds `-38 dBFS`;
  - `asr_transcript`, `asr_wer`, `asr_cer`: scored via the `speech_asr` role (`qwen3-asr-1-7b`) against both the OCR text and the matched script line;
  - `voice_id` and `speaker`: verified against the profile's expected character voice (and flagged if a character line fell back to `"narrator"`).
- Slice each spoken line `[t_box_appear - 500ms, t_audio_end + 500ms]` into `artifacts/av-harness/<run-id>/clips/line-NN.mp4` and `line-NN.wav` for direct multimodal inspection by the agent (`read_file`).
- **Latency gate:** Assert `t_text_settled → t_audio_start` **p50 ≤ 500 ms, p95 ≤ 800 ms** on warm/pre-synthesized runs, zero missed dialogue boxes, zero duplicate commits of the same box, and expected character `voice_id`s.

### 3. Latency & consistency optimizations proven by `make test-av`
- **Profile voice pre-synthesis & successor prewarming:** Pre-synthesize voiced script lines into the content-addressed TTS cache (`internal/speech`) so live gameplay hits `from_cache: true` (~5–20 ms lookup instead of 800–2500 ms live synthesis).
- **High-confidence local commit (Clef skip):** When the local matcher's top-1 candidate is high-confidence with a clear margin over top-2 and a known speaker, commit immediately without blocking 500–800 ms on a Clef round-trip; reserve Clef for genuinely ambiguous rankings.
- **OCR warm pool & catch-up batch window tuning:** Ensure warm `ocr.Pooled` slots at session start and tune the catch-up batch window so rapid `A`-press sequences in Dolphin (~750–800 ms cadence) batch and merge cleanly.

## Tradeoffs

- `make test-av` requires Dolphin, `Xvfb`, `ffmpeg`, PipeWire/PulseAudio, external gitignored Wind Waker fixtures (`GAME_NARRATOR_DOLPHIN_ISO`), and live model providers. Like `make test-dolphin` and `make test-models`, it is a live host gate (category (c)+(d) in `AGENTS.md` rule 3) and skips with `NOT RUN:` when prerequisites are absent.
- Dual-key voice lookup (`ID` then `Name`) adds at most one extra indexed SQLite read (`idx_voice_profiles_character`) only on the cache-miss / first lookup when a character has no voice under its ULID.

## Validation

- `go test -race ./internal/runtime/... ./internal/store/...` (`TestResolveVoiceByCharacterNameAlias`, `TestProfileCharacterVoiceCountsSharedDefinition`).
- `make test-av` producing `artifacts/av-harness/<run-id>/{session.mp4,timeline.json,summary.md,clips/line-*.mp4,clips/line-*.wav}`, inspected directly with the multimodal `read_file` tool.
- Live verification on the UNO Q board with GameCube HDMI capture (`artifacts/hardware-uno-q-<run-id>/`).

## Consequences and Reversal

- Voices created through the Cast UI / auto-cast immediately resolve to their character voices during live sessions without requiring a manifest export/re-import round-trip.
- Developers and multimodal agents have a single command (`make test-av`) that produces a synchronized video+audio recording and stage-by-stage latency breakdown of real Dolphin gameplay.
- Reversal: revert `internal/e2e/av_harness_test.go`, the `make test-av` target in `Makefile`, and the `speakerAliasByID` fallback in `internal/runtime/`.

## Amendment 1: gate strictness, phantom speech, box position

Date: 2026-10-21
Status: accepted

### Context

1. **The latency gate passed on two samples.** A `make test-av` run reported
   PASS with settled→audio p50/p95 = 2/2 ms while 12 of the 14 timeline
   lines had no audio start at all (they were batched into merged catch-up
   runs or queued behind speech, and are correctly excluded from the idle
   gate). The percentiles were computed over the remaining 2 samples — a
   "warm run" the gate cannot verify, because two samples are not a
   distribution. The gate had no floor on the sample count, so a run where
   almost every line is batched trivially passes.
2. **Phantom speech was uncounted.** On the real board the narrator spoke
   OCR junk while no dialogue box was on screen — a `speech.started` the
   timeline's committed lines do not explain (or explain only as a
   read-through line, ADR 0136). The timeline had no counter for speech
   with no line behind it, so the run passed while the audible defect was
   on the recording.
3. **The voice gate asserted nothing.** Every line carried one expected
   voice (the scene speaker's), so the unattributed lines — whose resolve
   is the narrator's voice, `speakerFor`'s documented fallback — were
   structurally mismatched, and the assertion helper recorded the voice
   rows without asserting them.
4. **The report had no box position and no explicit vs-script WER.** The
   placement-aware region (ADR 0137) exists precisely because the box
   renders top, middle or bottom by scene, but the timeline row did not
   say where the line's box was; and the ASR score carried a generic WER
   without naming the side it was scored against.

### Decision

All changes are in the harness's test code (`internal/e2e/av_harness_*_test.go`);
no runtime behaviour moves.

1. **`PhantomUtterances` (+ `PhantomReadThroughs`) on the timeline
   report.** The timeline scan counts every `speech.started` whose
   occurrence — or the merged run's occurrence the started names — maps to
   no committed line of the timeline, or to a read-through line. A started
   is *explained* when its occurrence is a committed line's own, or a
   merged run's whose `message_ids` joined it onto at least one committed
   line (the catch-up run shape, ADR 0122); the scheduler's raw event
   (no `dialogue_occurrence_id`) falls back to the utterance id, the same
   occurrence. The read-through half is the starteds whose occurrence is a
   commit with the read-through marker — `message_id: "unknown"`
   (`speech_eligible: false`) in the commit payload, ADR 0136. Read-through
   commits were already excluded from the timeline's lines; their audio is
   now counted as phantom instead of silently dropped, so the board's
   "speech with no box" shows up as a number.
2. **`BoxPosition` per line: `"top"` / `"middle"` / `"bottom"`.** From the
   profile's region rows (the `regions` table's normalised rectangles,
   ADR 0137's placement-aware rows): the row's centre Y over the frame
   height (`RectY + RectH/2`) — under a third is `top`, over two thirds is
   `bottom`, the band between is `middle`; a line whose region has no row
   is `""` (the honest "not measured", not a guess). The live test feeds
   the rows via `st.ListRegions` (the `avRegionGeos` helper); the unit
   tests feed a synthetic map, so the classifier stays deterministic
   without a store.
3. **Per-line `ExpectedVoiceID` from the commit's speaker attribution.**
   `avBuildTimeline` takes both expected voices (`avExpectedVoices{
   Character, Narrator }`) and applies the resolveVoice rule the catch-up
   test uses: a commit attributed to a speaker (`speaker_id` +
   `speaker_known`) expects the character's voice; every other line (the
   unattributed) expects the narrator's. A slot left empty leaves the line
   without an expectation, and the gate's voice rule skips it.
4. **`ASRWERvsScript` per line, alongside the generic ASR WER.** The
   transcript's WER against the line's script text — the corpus ground
   truth `avASRScore` already scores against (`avCorpusTexts`) — is now an
   explicit field next to the generic `asr_wer`, and is the gate's spoken-
   line fidelity input. An unscored line (no ASR role, or no ground truth
   — the read-throughs) stays zero with the scored flag false, so "no
   evidence" never reads as "a perfect reading".
5. **The gate is strict (`avGateFailures`, pure over the report).** A run
   fails when: fewer than 2 committed lines; **`IdleSamples < 3`** ("too
   few idle lines to measure" — the 2/2 ms foolproof); settled→audio
   p50 > 500 ms or p95 > 800 ms (**evaluated only over the idle-sample
   floor** — a p50 over 0..2 samples is no estimate, and is exactly the
   case that passed before); `MissedLines > 0`; `PhantomUtterances > 0`;
   stale starts; duplicate commits; **any line with a known
   `ExpectedVoiceID` whose `VoiceMatch` is false**; **any spoken line
   (an audio start) whose `ASRWERvsScript > 0.35`**. Every failure message
   names the line index and the numbers. `GN_AV_NO_LATENCY_GATE=1` now
   bypasses **the latency bounds only** (an acoustic measurement the
   environment's audio path can degrade); the strictness rules — the
   idle floor, the missed, the phantoms, the voices, the WER — are the
   session's correctness and always assert.
6. **`report.md` next to `timeline.json`.** The run's summary numbers
   (the gate's inputs) and a per-line table: index, box position, appear,
   settled, commit, Clef, TTS ready, the audio start (event + RMS),
   settled→audio, appear→audio, cache, voice ok, WER vs script,
   batched/queued and the end result — an unobserved instant is `-`, so a
   failed gate names a line the reader can find in the table.

### Tradeoffs

- The gate is deliberately stricter: a warm run where most lines are
  batched (fewer than 3 idle samples) now FAILS instead of passing on
  percentiles over noise, and a run that speaks a read-through with no
  committed line behind it fails on the phantom count. The escape hatch
  (`GN_AV_NO_LATENCY_GATE=1`) no longer silences the correctness rules —
  recording a degraded acoustic run without asserting its latency is still
  the escape, asserting nothing about its speech is not.
- The per-line expected-voice rule assumes the harness scene's single
  designed character (the fixture's first dialogue, the same assumption
  the catch-up test's `expectedVoice` closure makes); a multi-character
  scene would need the expected voice per speaker id, which the commit's
  `speaker_id` already carries.
- Read-through audio is now a gate failure by construction: the ADR 0136
  feature speaks exactly the lines the timeline does not hold. That is the
  intended meaning of the board's phantom speech — a read-through the
  operator enabled is still a read-through, and the timeline must not
  pass a run whose audible half the corpus cannot account for.

### Validation

- `go vet ./internal/e2e/` and `go test -timeout 5m -run 'AVHarness'
  ./internal/e2e/`: `TestAVHarnessGateRules` (each gate rule on a
  synthetic report: the idle floor, the latency bounds, the missed, the
  phantoms, the stale, the duplicates, the per-line voice and WER rules,
  the no-expected-voice and unspoken-line skips), `TestAVHarnessBoxPosition`
  (the classifier's boundaries, including the third/two-thirds edges),
  `TestAVHarnessReportWriter` (the summary rows and the per-line table
  cells, including the `-` cells), and the extended
  `TestAVHarnessMetricsAndRMSOnset` (the timeline with a merged run, a
  read-through commit and a bare started: the run's started is explained,
  the phantoms are counted, the box positions and per-line expected
  voices are classified). The live `TestLiveDolphinAVHarness` skips as NOT
  RUN without its gate env, as before.
- The next real `make test-av` run is the evidence the amendment exists
  for: the idle floor, the phantom count and the box positions in its
  `report.md`.

### Reversal

Revert the Amendment 1 fields and rules from
`internal/e2e/av_harness_helpers_test.go`,
`internal/e2e/av_harness_live_helpers_test.go`,
`internal/e2e/av_harness_test.go`, `internal/e2e/av_harness_unit_test.go`,
`internal/e2e/av_harness_gate_test.go` and
`internal/e2e/av_harness_report_test.go`; the original ADR 0143 gate (the
single expected voice, the two-sample percentiles, no phantom count, no
report.md) is restored by the same diff in reverse.
