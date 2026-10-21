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
