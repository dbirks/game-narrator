# Acceptance report — 2026-10-21 — **#41 close-out: live narration made fast and reliable**

**Supersedes the 2026-10-20 DRAFT** (`docs/status/acceptance-2026-10-20.md` is kept as
history; this file is the 2026-10-21 close-out — the 10-20 report carries an add-one-line
pointer at its top). Per-milestone status for this close-out: `docs/status/milestones-2026-10-21.md`.

## Summary

The #41 handoff (2026-10-17 → 2026-10-21) closes out its evidence chain on the committed tree at
HEAD `bc2d89d`:

- The board-latency and missed-line fix chain of the #41 session landed on main (ADR 0136/0137/
  0143/0144/0150–0153 plus amendments): placement-aware textbox region and the open-sea
  false positive (#41, #44); the unmatched read-through and its plausibility gate (#41, #44);
  fragment-aware matcher scoring (#41); typewriter reveal-complete signal (#41); profile
  presynthesize runtime + job (#41); idle catch-up batch flush (#41); strict A/V gate + phantom
  speech + box position + voice rule + report (#41).
- `make test-av` (the multimodal Dolphin A/V harness, ADR-0143) moved from its first FAIL run
  through six orchestrator-measured runs to a final PASS at `av-20261021T212013Z` (`3d2ed35`,
  settled→audio p50/p95 102/199 ms; 0 missed / 0 stale / 0 phantom).

## Milestones M0–M8 (docs/spec/16)

| M | Status | Evidence (committed anchor) | Remains |
|---|---|---|---|
| M0 | VERIFIED (anchor `aaa5967`; the `5fcf61e` CI re-gate is red at audit, #38) | Green CI through `aaa5967`; ADR 0126 startup logging, ADR 0113 load scaling landed `5fcf61e`; ADR 0150 deploy script + doctor `network.listen` landed post-audit | Green CI re-gate on the current tree |
| M1 | VERIFIED | `f35b429` G14 lifecycle inside green CI; portability/migration/backup/search rows as recorded | None material |
| M2 | VERIFIED | Bench at `d7c2ed0` (recall 97.9 %, top-1 100 %, 0 dup/stale); ADR 0115/0116 landed `5fcf61e`; ADR 0137 amendment (sea false positive) landed #41/#44 | Re-gate the ADR 0137 placement leg |
| M3 | VERIFIED | `make test-models` 33 PASS / 0 NOT RUN; live onboarding chain recorded `12f8c9f` | Recorded-run cadence (acceptance Gap 4) |
| M4 | VERIFIED (per-character proof recorded once; synthetic leg open) | Catch-up narration ADR 0122 + ADR 0122 amendment (idle flush) landed #41; per-character live proof `artifacts/per-character-live/perchar-20261020T053008Z/` | Synthetic two-voice regression → #36; wire the live per-character test into `make test-dolphin` |
| M5 | VERIFIED (one open a11y defect, #30) | Redesign Phases 0–1 on main through `aaa5967`; #30 contrast defect open; #34 live-session axe + graph table open | #30, #34 close-out |
| M6 | VERIFIED (scenario growth in flight) | 16/16 real Dolphin scenarios + `TestLivePlayUI`; `dolphin/sign-opened` + `dolphin/item-telescope` BLOCKED (blocked.md rows 1–2) | Unblock the two Dolphin halves |
| M7 | VERIFIED (live-HDMI fix re-gate pending; 1 h UNO Q soak BLOCKED) | Bench targets met; ADR 0115/0116 + ADR 0122 amendment re-gates; live proof `hw-proof-20261020T1822` | Re-gate; G19 soak on hardware |
| M8 | PARTIAL (live hardware proof achieved; formal re-gate + two decisions remain) | Board `0.1.0-bc2d89d`; #5 HDMI tone evidence; #37 cast 29/29; presynth job 4331 lines / 4229 synthesized / 4 failed / 1.57 GB / 71 min | Signed edge publish stale (→ #32); formal `make test-hardware` re-run; sustained `--source capture` 1 h (G19); v* tag (G20) |

## #41 Done-means checklist (evidence column)

- [ ] The board runs current main; the owner plays a session where boxes at any screen height are
  read and spoken, in character voices, with measured box→audio latency within the agreed budget.
  **Evidence:** the ADR 0137 placement leg + ADR 0136 read-through + the ADR 0143 A/V timeline;
  `make test-av` final PASS at `3d2ed35` (settled→audio p50/p95 102/199 ms).
- [ ] The Dolphin A/V harness exists, runs from a make target, and its report shows per-line
  latency, missed or duplicate lines, ASR accuracy and voice correctness. **Evidence:**
  `make test-av` runs + `report.md`/`timeline.json`/`clips/` under `artifacts/av-harness/`; the
  final run (`av-20261021T212013Z`) shows 14 committed lines, 0 missed/stale/phantom, 10 cache hits.
- [ ] A criterion-4 hardware evidence bundle with a character-voiced line exists (outside git),
  and its manifest is referenced from the acceptance report. **Evidence:** `hw-proof-20261020T1822`
  (narrator-voice-only at that point); the board-cast run (`av-…` cast summary: 29/29 voiced) is the
  character-voice evidence; re-proof still open.
- [ ] #34, #37 and #5 are closed; #2 and #24 are closed or deferred with reasons; no new issue is
  left without an owner. **Evidence:** #5 proven live (`a8123c8`); #37 fixed by ADR 0135 + verified
  29/29 in the board-cast run; #34 in flight on `issue34`; #2/#24 deferred with reasons.
- [ ] All gates are green on one SHA, CI is green, and `docs/status` has the final acceptance
  report and per-milestone status. **Evidence:** the orchestrator fills the Gates table above; the
  final green run is pending (CI last green at `aaa5967`).

## Measured (make test-av) — before/after

| Run | Commit | settled→audio p50/p95 | Idle samples | Lines | Missed / dup / stale | Verdict |
|---|---|---|---|---|---|---|
| av-20261021T190256Z | 9638e5b | 2/2 ms (over 2 samples only) | 2 | 14 | 0 / 0 / 0 | measured after the mid-reveal abstention recovery; strict gate's idle floor not yet in force; Mesa OSD read aloud; duplicates |
| av-20261021T201114Z | 9c54d98 | 24808/172571 ms | 17 | 17 | 0 / 0 / 1 stale | FAIL — the OSD read for ~40 s (the stale start) |
| av-20261021T202536Z | 21d3f7b | 43/784 ms | 12 | 12 | 0 / 0 / 0 | PASS |
| av-20261021T204940Z | a1526a9 | 121/482 ms | 10 | 14 | 0 / 0 / 0 | FAIL — strict gate voice-rule bug (121/482, the latency itself passed) |
| av-20261021T210125Z | 4c8270b | 163/824 ms | 10 | 14 | 0 / 0 / 0 | FAIL — idle batch window (settled→audio p95 824 > 800 ms) |
| av-20261021T212013Z | 3d2ed35 | 102/199 ms | 9 | 14 | 0 / 0 / 0 | PASS — 102/199 ms PASS, 0 missed / 0 stale / 0 phantom |

## Hardware (UNO Q)

- Board: `0.1.0-bc2d89d` (the version reported by the live board status).
- #5 HDMI tone evidence: the ALSA hw sink (`a8123c8`, ADR 0114) drove the hub HDMI out; the
  2026-10-20 live proof (`hw-proof-20261020T1822`) played narration out of the UNO Q hub HDMI,
  heard by the operator.
- #37 cast 29/29: the 2026-10-21 board-cast run named + voiced the remaining Wind Waker speakers;
  29 voiced characters, 29/29 active with approved voice, 0 unnamed proposed placeholders.
- Presynth job: 4331 lines / 4229 synthesized / 4 failed / 1.57 GB / 71 min (the ADR 0153 job on the
  Wind Waker profile).
- Fix-chain findings of the #41 session, closed by the landed fixes: the open-sea text-presence
  false positive (ADR 0137 amendment, #41/#44) and the fragment-abstain cluster (ADR 0151, #41) —
  the board read open-sea watermark/sprite text aloud, and reads that were substrings of longer
  script messages abstained instead of committing.

## Gates (orchestrator to fill in)

| Gate | Command | Tree SHA | Result | Artifact / run id | Notes |
|---|---|---|---|---|---|
| test-unit | `make test-unit` | `____` | `____` (PASS / FAIL / NOT RUN) | `artifacts/gates/test-unit.json` + CI run `____` | Race-enabled; CI also runs it |
| test-replay | `make test-replay` | `____` | `____` | `artifacts/gates/test-replay.json` + CI run `____` | BLOCKED reported, never passed |
| test-ui | `make test-ui` | `____` | `____` | `web/playwright-report/` + `artifacts/gates/full-test-ui.log` | Assert visible state incl. the #30/#34 a11y fixes once landed |
| test-dolphin | `make test-dolphin` | `____` | `____` | `artifacts/scenarios/<run-id>/index.html`, `artifacts/live-play-ui/<run-id>/` | `sign-opened`/`item-telescope` stay BLOCKED |
| test-models | `make test-models` | `____` | `____` | `artifacts/gates/models-gate-*.log` | Dev-host gate (CI never runs live) |
| bench | `make bench` | `____` | `____` | `artifacts/bench/report.md`, `artifacts/bench-second/report.md` | Second-set leg in the same gate (G12/#33) |
| CI | `ci` workflow | `____` | `____` (run id `____`) | check-run annotations (public) | Last green: `aaa5967` |
| hardware | `make test-hardware` on the attached UNO Q | `____` | `____` | `artifacts/hw-*/` evidence bundle | blocked.md row 3; until then NOT RUN |

## Open GitHub issues and dispositions

| # | Issue | Disposition |
|---|---|---|
| 2 | Optional Bluetooth speaker output | **Deferred** (Bose QC headphones crackled; keep the PipeWire BT latency/quantum follow-up). |
| 5 | HDMI/DP audio out of the UNO Q USB-C port | **Fix — committed, proven live** (`a8123c8`, ADR 0114); close on a clean re-proof + board `TestHWALSAHubHDMI` run. |
| 24 | UNO Q cold boot: ADSP APR services sometimes never register | **Hardware quirk — recovery committed** (`0f9fe74`, ADR 0118); close on a cold-boot re-run; keep the manuals/skill row. |
| 34 | G16: a11y of the player page during a live session untested; no table alternative | **Enhancement** — axe scan over live-session /play + graph table view. |
| 37 | Cast: "0 of N voiced" ignores approved voices; roster buries voiced characters | **Fix — committed** (ADR 0135); verified 29/29 in the board-cast run; close on the board with the real profile. |
| 41 | Handoff: make live narration on the UNO Q fast and reliable, and prove it with recorded Dolphin A/V tests | **Open — close when the Done-means list is met** (final re-gate + report + hardware re-proof outstanding). |
| 42 | No HTTP API to create/edit a profile's character rows | **Enhancement** (filed 2026-10-21). |
| 43 | e2e harness: concurrent Playwright runs collide on fixed webServer ports | **Closed** (2026-10-21, per the issue list). |
| 44 | Text presence misfires with no dialogue box on screen | **Fix — landed** (ADR 0137 amendment, sea false positive); keep open until the issue's follow-up is triaged. |
| 45 | Onboarding: no way to abandon a blocked setup run | **Enhancement** (filed 2026-10-21). |
| 46 | Dev trace evicts the frames/crops of committed and spoken lines first | **Enhancement** (filed 2026-10-21). |
| 47 | Private-capture placement replays run in no gate, and `$GAME_NARRATOR_FIXTURE_DIR` means two different things | **Bug** (filed 2026-10-21). |

## Secret scan

- `scripts/gates/secret-scan.sh` (tracked tree) → **clean**, rc=0 (with documented allowlisted
  synthetic fixtures, incl. the redirect-policy hosts and the synthetic disc fixture).
- `scripts/gates/secret-scan.sh --history` → clean on the added lines of every commit
  (the one-time historical leak at commit `398cad0` is blessed commit-scoped, `private-host`,
  value-anchored, fixed in `2ef5bc5`).
- `grep -nE "ts\.net|[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+" docs/status/acceptance-2026-10-21.md
  docs/status/milestones-2026-10-21.md docs/status/acceptance-2026-10-20.md docs/status/milestones-2026-10-20.md` → no matches (no hostnames/IPs/tokens/game text in these files).
