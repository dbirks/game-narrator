# Coding-agent workflow and autonomy charter

Your task is to **implement Game Narrator end to end** from the specs, not just a vertical-slice proof. You have an Arch Linux development host with permission to install dependencies via passwordless sudo; user will provide their lawful game image and inference endpoints when available. The final repo must include reliable Dolphin automation and rich failure inspection.

## Work start checklist

- Inventory host: uname, arch, shell, graphics/video, Dolphin build/version, Go, node/pnpm, docker/podman, Tesseract, FFmpeg, network, working dir. Read repo before modifying it. Never assume Dolphin `--batch` means displayless rendering; experimentally verify actual pixel frames and accepted controller input.
- Summarize verified prerequisites and gaps in local `docs/dev-environment.md` (redact secrets), propose ADR for selected approaches.
- Establish task board and identify independent workstreams; implement unit tests and stable interfaces before parallel feature work; merge with API/schema contract tests.
- Create synthetic user-owned test data and non-gamedump fixtures that can always run in CI. Begin authoring real Dolphin fixtures once supplied game is available.

## Autonomous coding loop

1. Select next requirement/acceptance ID from parts 00/16.
2. Inspect relevant source and upstream API docs. If an assumption is uncertain, write a proof-of-capability test before adopting it.
3. Create/update ADR if decision qualifies.
4. Implement through clean interfaces; avoid hardcoding model host IDs, Wind Waker conditions or UI test-only switches in production core.
5. Run focused unit/contract tests. If failing, produce reproducer and root-cause analysis, repair and rerun.
6. Run frame replay and real Go UI/Playwright verification. For integration milestones, run real Dolphin and inference tests when resources available.
7. Inspect correlated events, generated audio and browser report. Add a new regression scenario for any discovered failure.
8. Commit logically cohesive code/test/docs, update progress and unresolved blockers.

## Failure triage guide

- No emulator image: use synthetic fixtures but keep real Dolphin gate blocked; request authorized path once.
- Headless Dolphin black screen: inspect video backend/permissions/display, try Xwayland/Xvfb/render environment, record screenshots. Do not invent screenshot evidence.
- OCR misses game text: save original frame and crop, compare PSM/preprocess variants, align actual source text; optimize region before switching models.
- Speaker incorrect: verify source edition/graph candidate list, recalibrate score and Clef schema; save erroneous frame as fixture; don't patch one magic string into code.
- TTS wrong voice: inspect voice registry per-instance, Base checkpoint selection, profile revision and cache key; verify output by audition/ASR; no silent built-in fallback.
- Audio plays after advance: inspect session epoch and active generation on every callback, add deterministic delayed-response and live rapid-advance tests.
- Dev UI doesn't match backend event: correlate trace/occurrence and SSE cursor, not just labels; test reload/reconnect.
- Gateway not forwarding multipart registration: use dedicated route or approved inline reference and register an ADR.

## Reporting template

For each milestone: `Completed`, `Tests actually run`, `Test artifacts path`, `Metrics`, `ADRs added`, `Known limitations`, `Blocked external dependencies`, `Next work`. Separate `PASS`, `FAIL`, `SKIP`, `BLOCKED`, `NOT RUN` explicitly. Do not claim features and tests run if source code alone seems plausible.

## Definition of completion

All ten checks in part 16 final definition of done are verified with evidence or explicitly blocked by unavailable user resources; do not stop at single dialogue or API mock. Release cannot be described hardware-validated while UNO Q is inaccessible.
