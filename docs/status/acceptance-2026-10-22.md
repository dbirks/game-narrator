# Acceptance report: 2026-10-22, #41 close-out (live narration on the UNO Q)

This report supersedes `docs/status/acceptance-2026-10-21.md`, which is kept as history; that file points here. Per-milestone status is in `docs/status/milestones-2026-10-21.md`, and its M8 row is updated below.

Rule 5 applies to this file: it contains no hostnames, IPs, tokens, game text or frames. Evidence bundles live under `artifacts/` (gitignored) and are named by their run ID only.

## Summary

Between 2026-10-21 22:00 and 2026-10-22 04:00 (EDT), the owner played on the real UNO Q and reported four problems:
- slow voices;
- off-screen text spoken ahead of the box;
- wrong-gender voices;
- top-of-screen boxes not read.

Each problem was reproduced from board events and frames and then fixed on main:

| Problem seen on the board | Fix | ADR |
|---|---|---|
| Nothing read with a dark-scene presence ceiling of 0 | A zero `MaxSaturatedDarkShare` means "unset" (default 0.5) | 0137 amendment |
| Tesseract took ~20 s per crop | `OMP_THREAD_LIMIT=1` / `OMP_NUM_THREADS=1` in the board env | n/a (board env) |
| Stale prefix commits under slow OCR | A ridden (lagging) OCR read is not evidence the text stopped changing | 0096 amendment |
| A top box was cut in half | Profile rev 5 adds a top region; every Clef call carries the full frame | 0154 |
| Whole multi-box message spoken at once, including off-screen text | Page-aware speech: one box = one utterance, cache key `<id>#p<n>` | 0155 |
| A male villager spoke in Aryll's voice | The BMG speaker slot is shared. Only a confident Clef speaker answer names the speaker; otherwise the narrator speaks | 0156 + 2 amendments |
| Unnamed villagers all sound like the narrator | Clef answers `speaker_kind` from the frame, and 7 generic kind voices are approved on the board | 0158 + amendment |
| Committed-line evidence evicted within ~20 min | Pinned dev-trace budget for committed reads | 0024 amendment (#46) |
| Old cache entries and deploy backups filled the board disk to 99% | Speech-cache prune job/API/CLI/doctor (#49); deploy keeps 3 backups | 0159 |

## Afternoon, 2026-10-22: the play loop and what it found

The owner restated the goal: a loop where the agent walks up to a character in Dolphin, presses A, and judges the result. Is audio generated? Is it a man, a woman or a child? Is it immediate or ten seconds late? Does the narrator say random "ah"s while walking? Does a top-of-screen box work?

**Delivered:** `make play ROUTE=…` (ADR 0162, `612276d`) runs scripted routes from a savestate:
- stick holds, A presses, waits and segment marks;
- shared-clock video and audio;
- per-utterance segment, box, latency, ASR, voice and YIN pitch class;
- PHANTOM rows first.

Running it found and fixed three defects in the new code:
- the runner never succeeded a checkpoint-free step;
- segment marks were offsets from a zero time (plus a data race);
- the first pitch estimator doubled every F0.

**Findings with real providers** (`outset-deck-walk` route):
- **No phantom speech.** Walking and idling with the A-button HUD on screen produced 0 phantom utterances. The board's history shows its "A"/"a"/"4" phantoms all predate the read-through plausibility gate.
- **Aryll's box.** Spoken whole, 124 ms after the text settled. Her voice measured 340 Hz, "high (child)", confirmed by an independent YIN measurement.
- **Literal placeholder.** `{player}` reached TTS as written and was spoken "player". A worker is fixing this (#48, ADR 0163).
- **Navigation.** Reaching a different NPC (and a top-of-screen box) needs navigation off the lookout: every committed savestate starts there. Dead-reckoned routes found the exit. The vision-guided step is #55, and owner-made savestates next to NPCs were requested.

**Also fixed today:**
- **Dolphin scenarios.** The truth OCR engine indexed only the base rect while the pipeline reads placement-aware bands, so 2 of 17 scenarios had been failing since before this session (`70527d9`). 16 now pass, and `sign-opened` is blocked (registered: no savestate reaches a sign). Until 2026-10-23 that blocked row was miscounted as a JUnit PASS; it now reports as SKIP (BLOCKED).
- **Live OCR-fallback test.** Its synthetic band had no crisp edge for the presence text floor, found by bisecting to `6053975`. It now carries a panel marker (`6d3816d`). Live: primary confidence 0.19, then the vision model read the line at CER 0.
- **Thinking leak and vision capability (ADR 0161).** flint leaks `<think>`, which is now split by tag, and takes no images, so vision legs use an image-capable model.
- **Telescope walk.** The vision probe uses the vision model, and the walk now reaches Aryll by reading the SPEAK label (`6945fbb`).
- **Harness metrics.** The catch-up harness's "45 s stalls" and the A/V harness's 66 s appear→audio outlier are measurement artifacts (a stale last read after a conversation ends, and an early box-appear stamp). They are not narrator stalls.

## 2026-10-23: harness, recognition and agent fixes; one-SHA sweep

- **Rapid-A test driver (ADR 0163 amendment 1, `a83aa1b`, `7d2f096`).**
  - A between-box gap no longer ends the drive.
  - A silent session is checked against the Dolphin screen (`dolphin.BoxPresent`) before the driver gives up. A fully typed, unchanged box makes the session emit nothing, and a sweep run had ended its conversation with the box still on screen.
- **Final typewriter crop never read (ADR 0115 amendment, `9ca5a25`).**
  - The finished box crop rode the in-flight read of its prefix. The exact-hash skip then reused the prefix text for every identical Dolphin frame, so the line never committed and the box stalled 45 s.
  - Fix: a settle that arms on pixels nobody read gets one confirming read.
  - A first version would have re-read every frame on analog HDMI and was rejected. Measured on an analog-noise clip: main 1 call, rejected version 15, final 2.
- **Open-sea presence false positive (ADR 0169, `3f91310`, #44).**
  - Open sea under a bright sky passed every presence clause.
  - A new `TopDark` clause rejects a weakly saturated band whose top is bright. The captured board fixtures still pass.
- **Agent loop nudges (ADR 0168, `229049c`, #56).**
  - A repeated tool call gets one nudge carrying its earlier result.
  - A first turn that skips a tool the request names gets one corrective turn.
  - Live: 9/9 passes on `david-birks/qwen3-8-27b`, one of them recovered by the nudge.
- **Speaker-at-commit (ADR 0167) and gender override, `860b2cd`.** CI's race job flaked on the 300 ms budget; the answer-path tests widen it (`a83aa1b`).
- **Models.** Per the owner, only `david-birks/*` models are used. pi workers and the live gates run on `david-birks/qwen3-8-27b`, which takes images, so the vision legs run again.
- **Known NOT RUN.** `TestRealRecordTelescopeFixture` (fixture recording) is BLOCKED in every sweep: the telescope item scene is not reachable from the committed deck savestate by the SPEAK flow. The gate reports it as NOT RUN, which gives `make test-dolphin` exit code 2 with no failures. Tracked with the navigation work in #55.

## #41 Done-means checklist

- [ ] **The board runs current main, and the owner plays a session where boxes at any height are read and spoken in character voices, within the latency budget.**
  - The board runs a recent main (user-level install via `scripts/board/deploy.sh --user`), with profile rev 5 and a live v4l2 session. It is redeployed on the final sweep SHA.
  - Real-frame read on 2026-10-22: a boy NPC's bottom box read exactly, committed `MATCH_FUZZY`, and spoke from the presynth cache 7 ms after commit.
    - After `0b5d52c`, that shared-slot line routes to the narrator instead of the slot-linked character. Verified on the board.
  - **Early speaker question (ADR 0160): merged, then reverted.** On the board it made 930 Clef calls in ~90 min, and matched lines stopped committing; details in ADR 0160 and #54.
  - **Box-anywhere (ADR 0154 step 2): deferred to #53** after three worker attempts. The board's real top/middle/bottom placements are covered by rev 5's regions and the full-frame Clef.
- [x] **The Dolphin A/V harness exists, runs from a make target, and reports per-line latency, missed or duplicate lines, ASR accuracy and voice correctness.**
  - `make test-av` writes `report.md`, `timeline.json`, `events.json`, `clips/*.mp4|wav` and `media/session.{mp4,wav}`.
  - Runs are tabled below. I inspected clip frames (montage) and per-clip RMS myself on `av-20261022T051337Z`: bottom and middle boxes, no OSD text, clips ending on the post-line cut-in.
- [ ] **A criterion-4 hardware bundle with a character-voiced line exists outside git.**
  - Preliminary: `artifacts/hw-proof-20261022T031217Z`. It has a Clef request (`CLEF_DISAMBIGUATED`) and a manifest, but the frame and crop had been evicted (fixed since by #46), and the WAV was a whole-message, wrong-speaker clip.
  - 2026-10-24, the same real box (a boy NPC's page-2 box, bottom, real GameCube over HDMI) captured three times. Each bundle has a real frame, a crop, a Clef request ID, a WAV and a manifest:
    - `hw-proof-20261024T035120Z` (`3f91310`): Clef answered, voice `kind:boy` (correct gender), but the line was unmatched (`COMMITTED_ABSTAINED`). The matcher scored the one box against the whole two-box message.
    - `hw-proof-20261024T064020Z` (`486619b`): matched (`MATCH_FUZZY`, the right message), but voiced by the slot's assigned character (a different child, `voice_route: assigned`). Every speaker-at-commit ask had failed as `bad_schema`, because the full roster exceeded Clef's 16-option cap. Fixed in `d3d9b3d` (ADR 0167 amendment 1).
    - `hw-proof-20261024T065348Z` (`d3d9b3d`): the ask works (no degraded event), and the line is voiced `kind:boy`. Unmatched again on a noisy read.
  - **Still open:** a bundle that is both matched and character-voiced. It needs the page-aware matching fix (ADR 0184, in progress) deployed, then one more board read.
- [ ] **#34, #37 and #5 are closed; #2 and #24 are closed or deferred with reasons; no new issue lacks an owner.**
  - #5, #34 and #37 are closed. #2 and #24 are deferred with reasons (table below).
  - **Re-verified on the board on 2026-10-24 (build `d3d9b3d`, real rev 5 profile):**
    - #5: `POST /audio/test-tone` played, and a monitor capture of the HDMI sink recorded about 1.1 s of 440 Hz at RMS 0.177, the expected 0.25-amplitude sine (`artifacts/board-evidence-20261024-hdmi-tone.wav`).
    - #37: the board's Cast page (Playwright, 1440 px) shows "29 of 29 voiced", 0 unnamed, 52 retired, and 0 console errors.
    - Speaker placeholders: every `ww:spk:N` slot is either an alias of a named, voiced character (for example Tetra carries five slots) or retired. Lines from unnamed slots speak through Clef's apparent-kind voices (ADR 0158), not the narrator.
  - **Cast voice audit (ADR 0171) on the board:** 8 MISMATCH, 6 OK, 67 UNKNOWN. On review, five mismatches are wrong expectations from wrong briefs (the voices are right). Two are real and fixed: Baito 175 → 95 Hz and Mila's Father 187 → 129 Hz, adult male, pending the owner's approval.
    - Two briefs belonged to other characters: Komali had Medli's description, and Garrickson had a woman's.
    - Re-auditioned with correct briefs: Komali at 286 or 372 Hz (child), Garrickson at 165 or 140 Hz (male). These are pending the owner's approval.
  - Every issue filed in this session (#48–#52) has a suggested fix shape. #49 and #50 are closed.
- [x] **All gates green on one SHA, CI green.**
  - On `3f91310`: lint, test-replay, bench, test-ui, test-models, test-av and test-dolphin pass. test-dolphin's only non-pass is the documented telescope NOT RUN.
  - The `ci` workflow is green on `3f91310`. The hardware smoke is not run, which is reported, not passed.

## Measured: `make test-av` (settled→audible gate: p50 ≤ 500 ms, p95 ≤ 800 ms)

| Run | Commit | settled→audio p50/p95 | appear→audio p50/p95 | Lines | Missed / dup / stale / phantom | Verdict |
|---|---|---|---|---|---|---|
| av-20261021T212013Z | 3d2ed35 | 102/199 ms | n/a | 14 | 0/0/0/0 | PASS (2026-10-21 baseline) |
| av-20261022T050835Z | 6f75ea6 | 7/404 ms | 3951/10790 ms | 12 | 0/0/0/0 | **FAIL, voice gate.** 9/12 lines spoke the narrator voice: the first ADR 0156 cut counted name + alias as a "shared slot". Fixed in 1c4ee38. |
| av-20261022T051337Z | 1c4ee38 | 164/346 ms | 4407/66475 ms | 16 (12 spoken, 4 catch-up skips) | 0/0/0/0 | PASS; WER 0.000 on 11/12 spoken lines (0.040 on one) |
| av-20261022T063548Z | a566dfa + the 0b5d52c change (uncommitted at run time) | 243/384 ms | 4320/63193 ms | 18 | 0/0/0/0 | PASS |
| av-20261023T035549Z | a83aa1b | 42/322 ms | 904/3997 ms | 10 | 0/0/0/0 | PASS; frames inspected (Aryll, bottom and middle boxes), child pitch on every spoken line |
| av-20261023T043630Z | a83aa1b (sweep) | 3/147 ms | 276/303 ms | 3 | 0/0/0/0 | **FAIL, idle-sample gate (2 < 3).** The driver ended the drive on a silent static box; fixed in 7d2f096 |
| av-20261023T051909Z | 7d2f096 (sweep) | 127/282 ms | 3855/9352 ms | 11 | 0/0/0/0 | PASS; the screen check kept the drive going |
| av-20261023T091038Z | 3f91310 (final sweep) | 102/182 ms | 2246/9005 ms | 9 (8 spoken, 1 catch-up skip) | 0/0/0/0 | PASS; WER 0.000 on 6/8 spoken lines (0.040, 0.048 on two). The box that stalled before the confirming read is spoken |

The large appear→audio p95 comes from the box's own on-screen life, not from narration. Lost-A retries keep one box up for about 60 s, and its line spoke within 4 ms of the text settling.

## Hardware (UNO Q)

- **2026-10-23:**
  - Redeployed `0.1.0-3f91310`, the final sweep SHA, and a live v4l2 session is running.
  - The agent role is switched to `david-birks/qwen3-8-27b`. On the board, `game-narrator doctor` reports `providers.agent_reasoning` OK: the chat probe answered in 428 ms.
  - Before that: the speech-cache prune removed 6,836 orphaned entries (2.17 GB), leaving 5.5 GB free after the deploy backup. Presynth on rev 5 covered 6,104/6,104 utterances: 5,752 cache hits, 330 synthesized, 0 failed.
- **Build (2026-10-22):** `0.1.0-0f44d45`, user-level unit with linger. Env adds `OMP_THREAD_LIMIT=1`, `OMP_NUM_THREADS=1` and `GAME_NARRATOR_ROLE_DECISION_TIMEOUT=4s`; models use flint.
- **#5:** closed. The HDMI monitor recorded a 440 Hz test tone at RMS 0.177 through the hub's HDMI audio.
- **#37:** closed. Cast shows 29/29 voiced on the real profile (`artifacts/board-evidence-20261021/`).
- **Generic kind voices (ADR 0158):** 7 approved on the board, all passing QA.
  - My own pitch check (YIN median F0) caught a bad first `kind:adult_male` take at ~219 Hz, which sounds female or young. Its re-take reads 99 Hz.
  - The other medians: elder_male 104 Hz, adult_female 271 Hz, girl 330 Hz.
  - The operator flow for this is #51.
- **Presynth on rev 5:**
  - The earlier whole-message pass ran 4331 lines (4248 synthesized, 4 failed).
  - The per-page, shared-slot-aware pass is queued and runs between sessions.
  - It refused to start at 334 MB free, a refusal that worked as designed. 7.5 GB was freed by pruning 10 stale deploy backups, and deploy now keeps 3.
- **Shared-slot fix on hardware.** Redeployed `0.1.0-0f44d45`; the same boy NPC's line re-committed as `MATCH_FUZZY` and now routes `voice_route: narrator`, from cache, instead of the slot-linked Aryll. The kind and character route waits on ADR 0160 and the model node.
- **#49 cache prune on hardware.**
  - The dry run reported 5,728 unreachable entries (2.35 GB), the old whole-message keys.
  - The real run pruned the same 5,728 entries, freeing 2.35 GB. Data-partition free space went from 3.7 GB to 5.9 GB; assets went to 1.7 GB.
  - Deploy keeps 2 backups, and prunes before writing when free space is under twice the newest backup.
- **Degradation:**
  - Synthetic-stub tests: `internal/runtime/degrade_outage_test.go` (404 then 503: local commits, the decision view reports degraded, cached TTS plays, uncached TTS fails cleanly).
  - Live: during tonight's model-node outages the board's Clef breaker opened and narration continued on local matching plus cached audio.

## Model-node outage (resolved)

The node was reset on 2026-10-22, and every model gate above ran afterwards. The original note follows.

At 03:00 EDT the model node lost its GPU. NVML reports `GPU_IS_LOST` (ret 15), the HAMi device plugin crash-loops, and the node advertises 0 GPUs.

Everything on that node is unavailable:
- flint (agent and workers);
- clef-omni;
- qwen3-tts base and voicedesign;
- qwen3-asr.

So the model gates, a fresh `make test-av` and the Clef-backed criterion-4 bundle cannot run. Recovery needs a host-level GPU reset or reboot of that node, which is the owner's call. Another member's model on the gateway still answers.

## Gates

| Gate | Command | Tree SHA | Result | Notes |
|---|---|---|---|---|
| lint | `make lint` | 3f91310 | PASS | Commits are gated on it |
| test-replay | `make test-replay` | 3f91310 | PASS (155/0/1 skip/0) | The skip is the private-frame presence probe (`GN_PRESENCE_FRAMES`) |
| test-unit (race) | CI runs `./...` with `-race` | 3f91310 | PASS (CI) | |
| test-ui | `make test-ui` | 3f91310 | PASS | |
| bench | `make bench` | 3f91310 | PASS | |
| test-models | `make test-models` | 3f91310 | PASS (34/0/0/0) | `david-birks/qwen3-8-27b` + clef-omni + qwen3-tts + qwen3-asr |
| test-av | `make test-av` | 3f91310 | PASS (102/182 ms) | `av-20261023T091038Z` |
| test-dolphin | `make test-dolphin` | 3f91310 | PASS with 1 NOT RUN (exit 2) | 8 real-bridge, 17 Dolphin scenarios (16 pass and 1 blocked, which JUnit counted as PASS until the 2026-10-23 fix), 1 presence test, and the catch-up and live-UI tests. NOT RUN: `TestRealRecordTelescopeFixture` (BLOCKED, see above) |
| CI | `ci` workflow | 3f91310 | PASS | |
| hardware | `make test-hardware` | not run | NOT RUN | The board is exercised through deploy, presynth and the live session instead |

## Issues

| # | Disposition |
|---|---|
| 2 | Deferred: BT headphones crackled; the PipeWire latency/quantum follow-up stays open. |
| 5 | Closed (proved on hardware). |
| 24 | Deferred: ADSP APR cold-boot quirk. Recovery is committed (ADR 0118), and `audiodsp-recover` now polls 60 s for the late signature. |
| 34, 37, 45, 46, 49, 50 | Closed. |
| 42 | Open. No API to create character rows; ADR 0158's voice-key route avoids needing it for kind voices. |
| 44 | Open. Two sea false-positive fixes landed (0137 amendment, 0169). New evidence: a translucent box read absent once its arrow vanished, not reproduced on the saved frames. |
| 47 | Open. Private-capture placement replays run in no gate. |
| 48, 56 | Closed (ADR 0165, ADR 0168). |
| 51 | Open. Cast UI for generic kind voices, with a pitch sanity check. |
| 52 | Open. CI flake in the decision-deadline test (hardened; watch). |
| 53, 54, 55 | Open follow-ups: box at any height, early-speaker redesign (superseded in practice by ADR 0167), vision-guided navigation. |

## Secret scan

- Tree mode (inside `make lint`) is clean on every commit of this session.
- `--history` locally flags 4 lines in `bde7427`. That is a local-only, never-pushed snapshot commit on no branch, so CI's history scan, which covers pushed refs, does not see it.
