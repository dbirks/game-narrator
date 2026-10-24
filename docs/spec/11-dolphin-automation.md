# 11 — Dolphin E2E laboratory: controllable gameplay, deterministic fixtures, agents and headless CI

## Objective

A coding agent on an authorized Arch Linux machine can provision the project, open a **user-supplied** GameCube image in Dolphin, drive an actual segment of gameplay using recorded or programmatic inputs, capture a real frame feed, run the actual production recognition/decision/TTS/audio pipeline, inspect the React developer UI with Playwright, generate comprehensive HTML evidence, and repeat the run after a code change.

This is a separate dev/test capability. The UNO Q will never emulate a game or inject button inputs into a real console.

## Emulator capability baseline vs validation

Dolphin documents `--user` isolated config directory, `--movie` DTM input replay, `--exec` game launch, `--save_state` and `--batch` mode. `--batch` means no Dolphin regular UI, **not** guaranteed off-screen GPU frame production. Headless test runner must confirm graphical output on real installed Dolphin + Mesa/Vulkan/X11/Wayland or Xvfb/virtual display. Record emulator build + config + shaders/video backend; if Xvfb cannot render or has no GL support, try an isolated real/Xwayland display or EGL setup and document validated path. Do not declare test passed because Dolphin process existed.

## Fixture origins

1. **Curated scenario**: authorized user plays to an interesting state, saves in-game progress or savestate; records DTM/controller sequence from known start.
2. **Agent-assisted scenario authoring**: a dev-only controller driver uses input pipe/virtual controller when supported, plus screenshot observation and optionally MemoryWatcher as **test oracle only**. The agent explores under human-provided time/budget boundaries, finds a state, creates DTM/recorded input, snapshots it, and writes expected events.
3. **Deterministic visual fixture**: record timestamped frames and expected events extracted from a verified run, replayable without Dolphin/games/models. Use generated synthetic footage for open CI.

Savestates can be incompatible across Dolphin versions and hardware settings; never accept unverified load blindly. Prefer DTM from clean game start when feasible. Verify checkpoints by snapshot perceptual hash/landmark/scene metadata, not clock time alone. Store pointers and SHA256 to game image, never distribute ISO or copyrighted savestate assets without rights.

## Dev-only emulator bridge contract

`Launch(isoPath, userDir, config, initialState, dtm)`; `Inject(buttons, axes, frames|duration)`; `Screenshot()`; `StartFrameCapture()`; `GetExecutionState()`; `Stop()`; `RecordInputs()`; `SaveSnapshot(label)`; `LoadSnapshot(path)`; `GetOracleSignals()` optional.

Implementation options to probe on Arch:
- Dolphin controller input pipes (supported configurations vary by version) or Linux virtual gamepad `uinput`; configure emulator controller mapping explicitly and isolate privileges.
- DTM input movies for deterministic playback; verify file begins at expected game state.
- X11/Xwayland window capture using FFmpeg `x11grab`, wlroots portal capture or renderer attachment; normalize timestamps and ensure gameplay frames are actually different during movement.
- MemoryWatcher can provide game-specific state hints in test context, but never feed emulator memory oracle into the **production OCR or speaker classifier**. It is ground truth / fixture annotation only.

The test driver should be independently testable; if an unverified input-pipe method fails, fallback to a documented virtual controller strategy and write an ADR documenting proof. Avoid brittle focus-dependent keyboard automation as the only option.

## Headless automation modes

**Mode A: replay pure frames** — no emulator/game required, handles every normal PR.  
**Mode B: Dolphin + synthetic provider responses** — real rendering/input/OCR, fixed deterministic model results, useful for most developer PRs.  
**Mode C: Dolphin + real Clef/VLM/TTS/ASR** — full actual model-quality and integrated audio; nightly or on-demand, model metrics treated as quality thresholds not exact byte outputs.  
**Mode D: UNO Q hardware** — physical capture/USB audio, available only with plugged-in board/cards, explicit not-run otherwise.

## E2E verification steps (authoritative)

1. Preflight Arch graphics/window capture/audio sink, deployed Go server, configured gateway, user-owned game bytes, profile revision.
2. Launch Dolphin with isolated `--user` and pinned version/config. Take baseline screenshot and assert it is a rendered game frame (not black/menu/no signal).
3. Position at known game state; apply DTM/controller sequence and checkpoint screenshots. Check elapsed simulated gameplay and dialogue text appearance.
4. Feed Dolphin video to same `FrameSource` consumer as production capture. No fixture-specific branches in OCR/matcher.
5. Assert API emits `frame.observed`, `text.observed`, `message.candidates`, `decision.requested` and real `decision.resolved` when policy demands it. Cross-check model trace ID, timestamp, images and question IDs.
6. Assert speech request references expected immutable `voice_revision`, goes to Base target and result WAV samples are nonempty/valid. ASR validates expected text; audio sink records start/end.
7. Launch Playwright in browser, navigate `/traces` (the old `/developer` URL
   redirects there), verify screenshot/crop, OCR, candidates, Clef probabilities, speaker attribution and playback all show **same** occurrence/trace IDs. Correct a speaker and save it as reusable label/fixture.
8. Advance text early and confirm cancellation/fading; reset/replay and assert cache hit without new TTS generation where valid.
9. Build evidence bundle + machine-readable assertions + HTML report with links to time-aligned frame, OCR, raw decision (redacted), generated audio and ASR result.
10. Repeat scenario after a code change and compare metric thresholds; preserve failed inputs and remediation note.

## Avoiding false positives

- A happy HTTP response is not proof the model was called; assert request receipt and response identity from provider-side or gateway audit trace, with redacted raw IDs.
- A WAV file is not proof it was audible; assert file sink samples AND playback device/sink events. For physical speaker acoustic proof, optional test mic required — do not claim acoustic verification from virtual sink.
- Browser showing “Speaker: Tetra” isn't proof matching; validate source trace and chosen voice against expected fixture labels, with independent ground truth.
- Emulator “batch” flag isn't proof of offscreen capture; image must have measured nontrivial content and display text.
- Agent “played game” isn't proof controlled inputs were accepted; verify oracle screenshots/known scene transitions.
- Synthetic fixtures must be clearly marked and excluded from claims of coverage against the actual game.

## Fixture specification

`scenario_id`, `game_profile_id`, `edition_hash`, `emulator_build`, `dolphin_user_config_hash`, `graphics_backend`, `resolution`, `start_mode`, `dtm_path`, `savestate_pointer`, `input_timeline`, `checkpoint_images`, `expected_text_events` with frame ranges/tolerances, expected speaker, expected audio/cancel events, provider policy (`real|recorded`), constraints (resource budget and privacy). Validate every fixture before accepting it as a golden test.

## Coding agent fixture-authoring strategy

Start with user-provided Wind Waker game in Dolphin. Record varied locations and text kinds after save progress, create scenario collections with short test sequences: first character introduction, long page, sign, inventory pickup, repeated NPC, branch, fast mash, unusual UI icons and death/reload. Save often **within in-game save system** as well as test checkpoints; use separate emulator user directories per suite to avoid contamination. Add synthetic second game fixtures with different typography, resolution and language; don't let the code learn emulator-only internals for production.
