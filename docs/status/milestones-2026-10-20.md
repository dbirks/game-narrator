# Milestone & gap status — 2026-10-20

Tree: HEAD `baf8aec` (Apache-2.0). Uncommitted in the main working tree:
165 modified + 169 untracked + 1 deleted (335 `git status` rows; diff ≈
+32 258/−13 436 across 166 files) — the merged gap-closure worker output
(ADRs 0074–0104 are untracked; several workers still running at 05:41:
g8-telescope, g14, rd-phase0, live-status-api, agent-dock-api).

Strictness rule (per brief): **VERIFIED** = work committed AND proven by a
gate run on the committed tree. Last full gate run on a committed tree:
`3625f55` (2026-10-19, `docs/status/acceptance.md` re-issued `3afe9a4`).
Since the gate run the only committed code delta is `7bbc442` (managed
replay-clip store, ex-acceptance Gap 3) — committed, **not yet re-gated**.
All post-audit gap work is in the uncommitted diff: **PARTIAL
(uncommitted)** until it is committed and the gate set re-runs on the
clean tree.

## Milestones (docs/spec/16)

| M | Exit criteria (abridged) | Status | Evidence | Remains |
|---|---|---|---|---|
| M0 | Bootstrap, env probes, ADRs, make targets, diagnostic report | **VERIFIED** | Gate run on `3625f55` (acceptance M0 rows); `f2b3344` install/HDMI-capture guides + read-only preflight; `cef9312` UNO Q appliance Agent Skill; `d7c2ed0` truthful capture preflight + boot enablement (committed) | None material |
| M1 | Foundation, profiles, ACL, OpenAPI, jobs; two synthetic profiles | **VERIFIED** | Acceptance M1 + DoD rows (create→publish→revise→export→reimport, migration/restart/backup, 10k search) on `3625f55` | ADR-0080 (monotonic ULID audit) is uncommitted polish |
| M2 | FrameSources, OCR+voting, state machine, matcher, inspector | **VERIFIED** | Acceptance M2 + `make bench` on `3625f55`: top-1 19/19 dense captured holdout, 0 dup/stale (9a99985); inspector crop-basis (ADR-0082) uncommitted | Re-gate after the uncommitted crop/OCR changes land |
| M3 | Model integration + knowledge agent | **VERIFIED** | `make test-models` 29 PASS on `3625f55`; live onboarding chain recorded `12f8c9f` | Research-only web import (G4) uncommitted |
| M4 | Voice + playback pipeline | **VERIFIED** | Acceptance M4 rows on `3625f55` (audition/approval, restart survival, cache miss→hit, rapid advance `TestSpeechRapidAdvanceCancelsStale`) | Physical USB DAC leg = G5/M8 hardware |
| M5 | Complete React product, Playwright per route | **VERIFIED** (at `3625f55`; since: `7bbc442` un-gated) | `make test-ui` 22 specs + 112 unit on `3625f55`; `7bbc442` clip upload committed since, no gate run on it | Commit+gate the clip store; ADR-0093 redesign + new pages (status/search/assistant) uncommitted |
| M6 | Dolphin driver, ≥12 scenarios, reports, no sim hacks | **VERIFIED** (scenario growth uncommitted) | `make test-dolphin` on `3625f55`: 7/7 real + 16 scenarios PASS / 0 BLOCKED; today (uncommitted tree): `TestRealSavestateRoundTripGameplay` PASS ×4 (`artifacts/runs/dolphin-roundtrip2|3`, ADR-0102, issue #12 fix) | `dolphin/item-telescope` in flight (worker running); `dolphin/sign-opened` BLOCKED (route) |
| M7 | Live-model + adverse validation, bench targets | **VERIFIED** | Bench targets met on `3625f55` (speaker 100%, recall 19/19, top-1 19/19, 0 dup/stale; calibration adopted rev 2); soak 1 h+2 h `bea3d94` (recorded) | G19 (1 h on UNO Q); recorded-run cadence (acceptance Gap 4) |
| M8 | UNO Q install/hardware, packages, signed releases | **PARTIAL** | On-device (`cfd936f`, 2026-10-19): hardware smoke 19 PASS / 3 NOT RUN; sustained replay 30 min PASS (61052a1; `artifacts/hw-sustained/sustained-20261019T134452Z/`); `.deb`/Arch/OCI + lifecycle 9/9 (`c5e9b78`/`e8cea3a`/61052a1); signed edge publishes (`f62ce8d`/`4afdffc`); tagged path proven locally (dde81e6). On-device 2026-10-20: real v4l2 capture session healthy (1899 delivered / 211 dropped / 0 restarts — `scratchpad/board-live/REPORT.md`) but ready 503 (issue #9) and 100 % OCR cancel (issue #8) → 0 recognition | 0-NOT-RUN hardware run (dongle+DAC+voice); sustained `--source capture`; v* tag publish (user decision) |

## Gap rows (gap-audit-2026-10-19.md; the audit has rows G1–G20, the brief
named G1–G16 — all rows covered)

| # | Status | Evidence (uncommitted unless noted) | Remains / unblock |
|---|---|---|---|
| G1 capture session | **PARTIAL (uncommitted)** | ADR-0078: `buildFrameSource` v4l2 + `internal/capture/stack`; Playwright `capture-device`; on-device live v4l2 session 2026-10-20 (capture leg proven) | Commit + re-gate; re-run on device with ADR-0096/0100 fixes; sustained `--source capture` |
| G2 device API/UI | **PARTIAL (uncommitted)** | ADR-0078 routes + Settings/Play UI; issue-#10 fixes (v4l-utils 1.30.1 caps parsing, `%2F` ids 400-not-307, probe PSM 6) with golden tests | Commit + re-gate; real-dongle enumeration still NOT RUN |
| G3 per-character voices in play | **PARTIAL (uncommitted, core proof missing)** | ADR-0101 portable voice mappings + narrator fallback (migration 0015, `speech_fallback_test.go`, `voice_roundtrip_test.go` — uncommitted); named-speaker bench metric committed at 3625f55 (ADR-0070) | Still no test/session where two characters each speak a committed line in their distinct approved voice (gap-audit unit a/b/c) |
| G4 research-only onboarding | **PARTIAL (uncommitted)** | ADR-0077 rights-gated web-source import: `internal/agent/tools/websource.go` (+tests, untracked) | Commit + re-gate; scenario tests "no disc → publishable" / "malicious dump refused" |
| G5 USB DAC product surface | **PARTIAL (uncommitted; hardware NOT VERIFIED)** | ADR-0074: `internal/api/audio.go` (devices/selection/test tone/per-voice gain) + `internal/bt` (ADR-0091) + Settings UI; Playwright `audio` project 2/2 (worker report) | Commit + re-gate; DAC tone/latency on hardware (blocked.md row 2) |
| G6 restore from UI | **PARTIAL (uncommitted)** | ADR-0090: `internal/restore/` + `POST /api/v1/admin/restore` (validated upload, pre-restore safety copy, scheduled restart, recovery on failed swap); live walk evidence (backup→mutate→restore 202→healthy restart, g6-g11 worker log 2026-10-20 02:33); Playwright `settings-restore` | Commit + re-gate |
| G7 player controls | **PARTIAL (uncommitted)** | ADR-0075: pause/resume routes + Play UI, narrated-class setting (scenario `narrated_kinds` loader in tree), mid-session mode; `internal/runtime/controls_test.go` (untracked) | Commit + re-gate; Playwright pause-mutes-speech assertion |
| G8 sign/item narration | **PARTIAL (uncommitted) + BLOCKED (Dolphin real-capture half)** | ADR-0089 committed-class path + `TestSignItemRegionsCommitTheirClass` (PASS in worker gate run, 109/0 junit); synthetic `sign-opened-001`/`item-obtained-001` scenarios (untracked); ADR-0102/0103 checkpoint + recorder content-kind marks; `TestRealSavestateRoundTripGameplay` PASS ×4 today. Dolphin sign half: no savestate route to a readable sign — deck→village route broken (walkway gap), `scratchpad/board-live` + blocked.md progress 2026-10-20 | **Unblock (blocked.md row 1):** 1. record a fixture reaching a real sign from `windwaker-first-dialogue.state` (CLOSE to deck now reliable); 2. pin manifest SHA-256 in `sign-opened.yaml`; 3. name the sign line in the corpus + measured sign rect; 4. drop `status/reason`, assertions run unmodified. Item half: g8-telescope worker in flight (Aryll Telescope route). Until then reported BLOCKED, never pass (ADR-0037) |
| G9 first-run wizard | **PARTIAL (uncommitted)** | ADR-0087: Setup wizard steps (device/gateway/audio/privacy) + Playwright setup walk (g9-g10 worker) | Commit + re-gate |
| G10 diagnostics completeness | **PARTIAL (uncommitted)** | ADR-0087: host report (RAM/storage/thermal), per-role probes, redacted logs, speaker-test links in `/diagnostics` (g9-g10 worker; diagnostics API in tree) | Commit + re-gate |
| G11 updates/notifications | **PARTIAL (uncommitted)** | ADR-0088: `internal/updates/` (bounded check, offline-safe cache, no auto-install) + Settings section + doctor row + fake-releases Playwright walk | Commit + re-gate |
| G12 second-game recognition/TTS leg | **PARTIAL (uncommitted)** | ADR-0079: `testdata/bench/bench-synthetic-002/` (profile+truth), `internal/bench/generate002.go`, `secondset_test.go`/`secondclip_test.go` | Commit + run the bench leg through real OCR/matcher/TTS/dev-UI; keep `assertNoGameVocabulary` green |
| G13 developer dashboard actions | **PARTIAL (uncommitted)** | ADR-0083: per-event correction actions, save-regression-fixture, evidence download on developer page (g13 worker exited; code in tree) | Commit + re-gate; waveform (audit sub-item) not evidenced |
| G14 library lifecycle | **PARTIAL (in progress)** | g14 worker still running (started 05:36): duplicate/clone/disable/delete in `internal/store` + API (worktree `wt-g14` merged into main diff) | Worker finish; migrations + backup/restore + restart tests per task brief; commit + re-gate |
| G15 successor prewarming | **PARTIAL (uncommitted)** | ADR-0095: `speechWiring.initPrewarm/prewarm/cancelPrewarmBatch` in `speech.go`; top-k weighted successors at `PriorityBackground`, entropy+byte-quota gates, versioned cache key; `internal/speech/prewarm_test.go` (unit green, `make test-replay` 102/0 in worker run) | Commit + re-gate (incl. bench hit-rate stats line) |
| G16 player-page a11y during play | **PARTIAL (uncommitted)** | Axe walk extension + committed-lines `aria-live` in the uncommitted web diff | Graph "table view" alternative NOT evidenced in the diff; commit + axe run over live-session state |
| G17 metrics surface | **NOT STARTED** (no uncommitted work found) | Bottleneck report (ADR-0069, committed) covers the diagnostic slice only | `/metrics` or equivalent time-series surface per spec 13 |
| G18 runtime config changes | **PARTIAL (uncommitted, documented deviation)** | Settings store-backed updates now land with ADR-0074/0075/0087 surfaces (sink/device/gain, capture, controls persisted, not env-only) | Commit + re-gate; full `PUT /api/v1/config` contract remains the ADR-0047 deviation until declared |
| G19 1 h UNO Q replay soak | **BLOCKED (hardware)** | 30-min replay source on device PASS (61052a1); 1 h+2 h on Arch host `bea3d94` (not the UNO Q profile) | **Unblock (blocked.md row 2):** attach UNO Q + hub + UVC dongle + USB DAC + TTS voice; `make test-hardware` to 0 NOT RUN; sustained run `--source capture` 1 h; archive CPU/RSS/temp report |
| G20 tagged v* release | **BLOCKED (user decision)** | Local never-pushed `v0.1.0-rc.0` run proven (dde81e6, image-tag defect fixed); signed edge/snapshot publishes live (f62ce8d/4afdffc) | **Unblock:** user picks version; `git tag -s vX.Y.Z && git push origin vX.Y.Z`; watch `release` job; verify per docs/manuals/17 (cosign verify + attestation) |

## Active-goal done-criteria mapping

No goal is registered in this session (`get_goal`: none), so the six
done-criteria are restated from the milestone-status brief, each mapped to
this document:

1. Per-milestone status M0–M8 — table above (5 VERIFIED at `3625f55`,
   M8 PARTIAL, M5 with the `7bbc442` caveat). **Met in this file.**
2. Per-gap-row status — all 20 audit rows above. **Met in this file.**
3. Evidence per row (commit/ADR/test/gate/run id) — cited inline;
   uncommitted work cited by ADR + test + worker run, never as a gate pass
   on the committed tree. **Met in this file.**
4. What remains per row — "Remains" column. **Met in this file.**
5. Unblock steps for BLOCKED rows — G8, G19, G20 carry the blocked.md
   recipes. **Met in this file.**
6. Open-issue dispositions — next section. **Met in this file.**

## Open GitHub issues (13 open, #4 closed) and proposed disposition

| # | Issue | Disposition |
|---|---|---|
| 1 | One-shot `game-narrator ocr` | **Fix — nearly done:** ADR-0085 + `cmd/game-narrator/ocr.go` uncommitted, device-verified (p95 698 ms); commit with the gate re-run |
| 2 | Bluetooth speaker output | **Fix — uncommitted:** ADR-0091 + `internal/bt` + UI; hardware-pairing leg needs the device |
| 3 | Pair/select BT from web UI | **Fix — uncommitted:** bt-ui worker done (Playwright 2/2, stale-badge bug fixed); commit + hardware leg |
| 4 | (closed) UNO Q setup Agent Skill | Closed 2026-10-20 by `cef9312` |
| 5 | HDMI/DP audio out of UNO Q USB-C | **Defer — hardware research:** no device-side code path; needs powered-dongle/EDID experiments on the unit; keep as hardware-track note until G19 unblock |
| 6 | Fence on non-speakable commits / duplicate terminals / empty skip reason | **Fix — uncommitted:** ADR-0092, 3 regressions, replay 102/0 in worker run; commit |
| 7 | Stale X lockfile slots | **Fix — uncommitted:** `freeDisplay` PID liveness + reclaim, 5 tests green in worker run; commit |
| 8 | Live OCR too slow on A53 | **Fix — uncommitted:** ADR-0096 (single-OCR default on ≤4-core boards, stale-observation drop, VLM on fresh 15 s bound after cancel) + ADR-0100 warm pool (p50 576 ms measured on device); needs on-device re-verification of a live session with >0 recognition before close |
| 9 | Readiness always 503 on device | **Fix — uncommitted:** ADR-0097 cached integrity verdict; re-run device `/ready` check |
| 10 | v4l-utils parsing / id encoding / PSM 7 | **Fix — uncommitted:** all three fixed with golden tests + contract pin; re-run `make test-hardware` device leg |
| 11 | Replay skip `no_approved_voice`; voice mappings don't survive export | **Fix — uncommitted:** ADR-0101 (portable mappings + narrator fallback, migration 0015); re-import the board profile and re-replay the HDMI line to close |
| 12 | Mid-game savestates don't restore | **Fix — code uncommitted, issue still open:** ADR-0102 root cause (truncated LZ4 read) + `TestRealSavestateRoundTripGameplay` PASS ×4 today; close only after `dolphin/item-telescope` + `sign-opened` land (worker in flight) |
| 13 | ~0.6 s Tesseract startup per read | **Fix — uncommitted:** ADR-0100 two-slot warm pool, device p50 576 / p95 593 ms; commit + re-run `TestHWOCRLatencyDevice` |

**Campaign-level next step (applies to all "Fix — uncommitted"):** one
clean commit of the merged diff with its ADRs, then a full gate set
(`make test-full` minus hardware) on that clean tree; until then nothing
above may be labelled VERIFIED, and the acceptance report must be
re-issued per its own caveat (Gap 3 landed at `7bbc442` without a
re-issue or re-gate).
