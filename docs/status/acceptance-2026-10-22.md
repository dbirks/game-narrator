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
- **Dolphin scenarios.** The truth OCR engine indexed only the base rect while the pipeline reads placement-aware bands, so 2 of 17 scenarios had been failing since before this session (`70527d9`). All 17 pass.
- **Live OCR-fallback test.** Its synthetic band had no crisp edge for the presence text floor, found by bisecting to `6053975`. It now carries a panel marker (`6d3816d`). Live: primary confidence 0.19, then the vision model read the line at CER 0.
- **Thinking leak and vision capability (ADR 0161).** flint leaks `<think>`, which is now split by tag, and takes no images, so vision legs use an image-capable model.
- **Telescope walk.** The vision probe uses the vision model, and the walk now reaches Aryll by reading the SPEAK label (`6945fbb`).
- **Harness metrics.** The catch-up harness's "45 s stalls" and the A/V harness's 66 s appear→audio outlier are measurement artifacts (a stale last read after a conversation ends, and an early box-appear stamp). They are not narrator stalls.

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
  - A re-capture on the current build is pending. It needs the model node (below) plus a line that reaches Clef.
- [ ] **#34, #37 and #5 are closed; #2 and #24 are closed or deferred with reasons; no new issue lacks an owner.**
  - #5, #34 and #37 are closed. #2 and #24 are deferred with reasons (table below).
  - Every issue filed in this session (#48–#52) has a suggested fix shape. #49 and #50 are closed.
- [ ] **All gates green on one SHA, CI green, and this report final.** Pending the final sweep (table below).

## Measured: `make test-av` (settled→audible gate: p50 ≤ 500 ms, p95 ≤ 800 ms)

| Run | Commit | settled→audio p50/p95 | appear→audio p50/p95 | Lines | Missed / dup / stale / phantom | Verdict |
|---|---|---|---|---|---|---|
| av-20261021T212013Z | 3d2ed35 | 102/199 ms | n/a | 14 | 0/0/0/0 | PASS (2026-10-21 baseline) |
| av-20261022T050835Z | 6f75ea6 | 7/404 ms | 3951/10790 ms | 12 | 0/0/0/0 | **FAIL, voice gate.** 9/12 lines spoke the narrator voice: the first ADR 0156 cut counted name + alias as a "shared slot". Fixed in 1c4ee38. |
| av-20261022T051337Z | 1c4ee38 | 164/346 ms | 4407/66475 ms | 16 (12 spoken, 4 catch-up skips) | 0/0/0/0 | PASS; WER 0.000 on 11/12 spoken lines (0.040 on one) |
| av-20261022T063548Z | a566dfa + the 0b5d52c change (uncommitted at run time) | 243/384 ms | 4320/63193 ms | 18 | 0/0/0/0 | PASS |

The large appear→audio p95 comes from the box's own on-screen life, not from narration. Lost-A retries keep one box up for about 60 s, and its line spoke within 4 ms of the text settling.

## Hardware (UNO Q)

- **Build:** `0.1.0-0f44d45`, user-level unit with linger. Env adds `OMP_THREAD_LIMIT=1`, `OMP_NUM_THREADS=1` and `GAME_NARRATOR_ROLE_DECISION_TIMEOUT=4s`; models use flint.
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

## Blocker at the time of writing

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
| lint | `make lint` | every commit since 4cb5f17 | PASS | Commits are gated on it |
| test-replay | `make test-replay` | 5577ab6 tree | PASS (142/0/0/0) | |
| test-unit (race) | touched packages per commit; CI runs `./...` | 5577ab6 / b60499f | PASS locally | CI flakes #50 (fixed) and #52 (hardened) |
| test-ui | `make test-ui` | 3ec2f53 | PASS (54 passed) after fixing the developer walk's post-logout expectation | |
| bench | `make bench` | 3ec2f53 | PASS (recall 100%, top-1 100%, 0 dup, 0 stale) | |
| test-models | `make test-models` | 3ec2f53 | **NOT RUN (meaningfully)** | 26 failures, all `model "flint" is not currently available` (GPU lost) |
| test-av | `make test-av` | a566dfa+ | PASS (see above) | Re-run on the final SHA after the node recovers |
| test-dolphin | `make test-dolphin` | pending | pending | Needs the model node |
| CI | `ci` workflow | pending | pending | Last green: 6f75ea6 |
| hardware | `make test-hardware` | not run | NOT RUN | |

## Issues

| # | Disposition |
|---|---|
| 2 | Deferred: BT headphones crackled; the PipeWire latency/quantum follow-up stays open. |
| 5 | Closed (proved on hardware). |
| 24 | Deferred: ADSP APR cold-boot quirk. Recovery is committed (ADR 0118), and `audiodsp-recover` now polls 60 s for the late signature. |
| 34, 37, 45, 46, 49, 50 | Closed. |
| 42 | Open. No API to create character rows; ADR 0158's voice-key route avoids needing it for kind voices. |
| 44 | Open. The sea false-positive fix landed; the follow-up is triage. |
| 47 | Open. Private-capture placement replays run in no gate. |
| 48 | Open. Speak the on-screen player name instead of the placeholder. |
| 51 | Open. Cast UI for generic kind voices, with a pitch sanity check. |
| 52 | Open. CI flake in the decision-deadline test (hardened; watch). |

## Secret scan

- Tree mode (inside `make lint`) is clean on every commit of this session.
- `--history` locally flags 4 lines in `bde7427`. That is a local-only, never-pushed snapshot commit on no branch, so CI's history scan, which covers pushed refs, does not see it.
