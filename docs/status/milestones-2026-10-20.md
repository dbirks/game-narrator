# Milestone & gap status — 2026-10-20

## UPDATE — 2026-10-21 ~04:30, revised ~04:55 (acceptance audit; supersedes the 22:40 update below)

Method: per-milestone and per-gap claims re-verified against the code
(grep of routes, tests, scenario YAMLs, Makefile gates) and against CI/
gate-run state, not from the docs alone. Nothing in this section is
claimed from the uncommitted diff.

**Tree at audit time:** HEAD `5fcf61e` (the in-flight "other work" diff —
ADR 0113/0115/0116/0119/0122/0126, catch-up narration, perceptual dedupe,
default capture source, job-queue pause, #22/#26/#27/#31 — committed by its
worker; `Fixes #26 #27 #22 #31`, `Refs #25 #28`). Uncommitted at audit
(revised ~04:55): the dolphin-gate worker diff — `internal/dolphin/` (drive,
record, record_real_test, savestate_fixture_test, telescope_test), `Makefile`,
ADRs 0123/0124/0125 (first-box persistence, newgame holdout bench leg,
telelescope re-speak + blocked gate) — **plus two more workers' in-flight
diffs that landed in the shared tree during the audit**: (a) the
placement-aware textbox region — ADR 0135, `internal/pipeline/` (config,
dispatch, roi, new `placement.go` + `placement_test.go`),
`internal/vision/textpresence.go`; (b) the in-flight **#32 fix** — ADR 0131
(OCI image apt pins resolved from snapshot.debian.org timestamps),
`packaging/Dockerfile`, `.github/workflows/publish.yml` (guard step in the
edge and release jobs), `scripts/release/verify-apt-pins.sh`. **Cross-ref
defect in (b), to fix before commit:** the Dockerfile/publish.yml/
verify-apt-pins.sh comments cite "ADR 0130" (8 places) but the apt-pins ADR
is **0131** — 0135 is the placement ADR. Plus this status pair and
`docs/status/blocked.md` (dolphin-gate progress notes).

**Gate anchors:** last **green** full CI (`ci` workflow) is `aaa5967`
(run 39528569626, 2026-10-21 01:39, 17 m 27 s — lint, secret-scan tree+
history+self-test, race unit, web contract/typecheck/lint/fmt, vitest,
test-replay). Since: `e1e2c6a` **failure** (race unit: 3253 PASS / 1 FAIL —
`TestCaptureSessionCommitsSyntheticText`, `internal/runtime` capture_test.go:
175, the known load-sensitive flake, #22 — since fixed by `5fcf61e`) and
`5fcf61e` **failure** (run 39542298642, 04:07–04:22: `gates` job, race unit —
`TestSyntheticReplaySessionRealTesseract`, `internal/runtime`
manager_test.go:90, `commits = 1, want 2`: 4 of 13 real-Tesseract reads at
5–12 s under CI load exceeded the replay read budget and degraded,
FrameToOCR P50 5 s / max 12.2 s; the #22 class of load sensitivity in a test
#22's fix did not cover → **issue #38**, filed by this audit; the `5fcf61e`
publish workflow_run skipped on the red CI). **No green CI since `aaa5967`
at audit.** Dev-host
`make test-models` is green at 33 PASS / 0 NOT RUN after `e1e2c6a`'s
web-import assertion scoping (`artifacts/gates/models-gate-final.log`,
`TestLiveWebSourceProposalForPublicTitle` PASS). The last green
`publish` edge job is `661bccf` (2026-10-20 20:04); `b1db234` and
`aaa5967` edge publishes **failed** at the GoReleaser snapshot step —
`packaging/Dockerfile:25-29` pins Debian 12 point-release versions that
no longer exist in the archive (apt-get exit 100); filed as **issue #32**.

### Milestones M0–M8 (docs/spec/16)

| M | Status | Evidence (verified at audit) | Remains |
|---|---|---|---|
| M0 | **IMPLEMENTED** | `scripts/bootstrap-arch.sh` (idempotent, reviewed), `.env.example` role placeholders, all AGENTS.md `make` targets present (Makefile 66–380), `internal/doctor` honest report (OK/DEGRADED/BLOCKED/NOT CONFIGURED/FAIL), ADR 0001–0013 infra decisions; gate set green at `aaa5967` (CI) incl. secret-scan `--history` + self-test | Green CI re-gate for the post-`aaa5967` commits: `5fcf61e` run red at audit (#38 load-sensitive real-Tesseract replay test); anchor stays `aaa5967` until a green run lands |
| M1 | **IMPLEMENTED** | Two synthetic profiles create→publish→revise→export→reimport (`internal/profileio/portability_test.go`), migration/restart/backup/restore (`internal/store/migrate_test.go`, `TestCorruptThenRestore`, admin/backup+restore routes ADR-0090), 10k-message search (e2e catalog/ksearch specs; store FTS), auth + role ACL (ADR-0016), OpenAPI + generated TS client (CI `check:api`); G14 lifecycle committed `f35b429` (ADR 0107, duplicate/clone/disable/enable/soft-delete/purge routes, api.go:867–878) | None material |
| M2 | **IMPLEMENTED** | FrameSources wired: `SourceKindV4L2` builds the real `ffmpegpipe.V4L2` through `internal/capture/stack` (`internal/runtime/source.go:264`, no longer 501) + dolphin/replay/push; OCR + temporal voting, text state machine, exact/prefix/trigram/fuzzy matcher, scene/message graph, policy + abstention (all committed, gated at `aaa5967`); `make bench` on the dev host (bench-g12-check, 2026-10-20 03:57, at `d7c2ed0`: recall 97.9 %, top-1 100 %, 0 dup/stale); dev inspector (developer page + Traces); in flight (uncommitted): ADR 0135 placement-aware textbox region (`internal/pipeline/placement.go` + `placement_test.go`, `internal/vision/textpresence.go`) | ADR 0115/0116 (live-HDMI escalation/retry, glyph-noise trim, typewriter-safe perceptual dedupe — `5fcf61e`, fixes #26) awaits green CI (`5fcf61e` run red at audit — #38); ADR 0135 placement work needs commit + re-gate |
| M3 | **IMPLEMENTED** | `make test-models` dev host: **33 PASS / 0 NOT RUN** (models-gate-final.log; Clef schema probes + live decisions, VLM OCR fallback `TestLiveVLMOCRFallbackHardFontCrop`, ASR client + QA, rights-gated web import `TestLiveWebSourceProposalForPublicTitle` — PASS after `e1e2c6a`, agent onboarding `TestLiveSyntheticAdventureOnboarding`); live onboarding chain recorded `12f8c9f` | Recorded-run cadence (acceptance Gap 4); `5fcf61e` re-gate red at audit (#38) |
| M4 | **IMPLEMENTED** (per-character proof recorded once; synthetic leg open) | Audition/approval + restart survival (`TestVoiceRoundTripNarration`), cache miss→hit (`TestSpeechCacheHitSecondSession`), rapid advance (`TestSpeechRapidAdvanceCancelsStale`); **per-character live proof 2026-10-20 05:30** (`TestLivePerCharacterVoices`, `internal/e2e/perchar_live_test.go`: two characters, two distinct designed voices, 5/5 lines in the linked character's voice, 5/5 ASR-QA passed, WAVs differ — bundle `artifacts/per-character-live/perchar-20261020T053008Z/`); catch-up narration + no-backlog policy committed `5fcf61e` (ADR 0122, #25) | G3 synthetic two-voice regression test → **issue #36**; wire the live per-character test into `make test-dolphin` (tracked by #25's acceptance) |
| M5 | **IMPLEMENTED** (one open a11y defect, #30) | 38 Playwright specs; `make test-ui` = typecheck/lint/fmt + vitest + Playwright against the embedded binary; redesign (ADR 0093): Phase 0 `84acd86`, Phase 1 screens **all on main** — Library/⌘K/agent dock `661bccf`, setup board `d2bfc84`, Traces `93b5c09`, Health `ec516e0`, Settings+theme `39f45fb`, Cast `d835603`, Play `964746a` + merge fixes `aaa5967` (**green CI** incl. web contract checks); SSE reconnect, a11y idle-state walk committed; related open: **#37** (Cast "0 of N voiced" ignores approved voices; being redesigned on `redesign-cast2`) | **#30** (open): Lilac Night theme fails WCAG contrast on Cast/Play (16 serious axe nodes); G16 a11y-during-play + graph table view → **issue #34**; `5fcf61e` re-gate red at audit (#38) |
| M6 | **IMPLEMENTED** (scenario growth in flight) | `make test-dolphin` three phases: real drives (`TestReal*`, savestate round trip ADR-0102 — issue #12 closed), `TestDolphinScenarios` (17 dolphin scenario YAMLs: 16 green + `dolphin/sign-opened` reported BLOCKED, never passed — ADR 0037), `TestLivePlayUI` (real-UI live play, evidence bundles under `artifacts/live-play-ui/`) | `dolphin/sign-opened` real-capture fixture BLOCKED (blocked.md row 2); `dolphin/item-telescope` BLOCKED (blocked.md row 1; measured SPEAK route does not hand over the telescope, 2026-10-20 ×7 runs); in flight (uncommitted dolphin-gate diff): ADR 0123 first-box persistence, 0124 newgame holdout bench leg, 0125 telescope re-speak + blocked gate |
| M7 | **IMPLEMENTED** (live-HDMI fix re-gate pending; 1 h UNO Q soak BLOCKED) | Bench targets met (`3625f55` anchor: named-speaker 100 %, recall 19/19, top-1 19/19, 0 dup/stale; calibration ADOPTED rev 2); soak 1 h + 2 h recorded `bea3d94`; live proof 2026-10-20 18:22: real GameCube HDMI frame → Tesseract OCR (0.95) → matcher → genuine Clef decision → commit → TTS → playback out of the UNO Q hub HDMI, heard by the operator (external bundle hw-proof-20261020T1822); ADR-0124 newgame holdout bench leg in flight | ADR 0115/0116 + 0124 re-gates with `5fcf61e`/dolphin-gate; G19 1 h soak on the UNO Q BLOCKED (hardware, blocked.md row 3) |
| M8 | **PARTIAL** (live hardware proof achieved; formal re-gate + two decisions remain) | On-device (`cfd936f` 2026-10-19): hardware smoke 19 PASS / 3 NOT RUN; 30-min sustained replay PASS (`61052a1`, `artifacts/hw-sustained/sustained-20261019T134452Z/`); `.deb`/Arch/OCI lifecycle 9/9; live proof 2026-10-20 18:22 (above); hub-HDMI ALSA hw sink committed `a8123c8` (ADR 0114, `TestHWALSAHubHDMI` asserts `hw_ptr` advances — NOT RUN without the board); cold-boot ADSP recovery committed `0f9fe74` (ADR 0118, #24); tagged path proven locally (`dde81e6`) | Signed edge publishes stale at `661bccf` — the edge job's Debian-12 pin defect → **issue #32**, fix in flight (uncommitted ADR 0131: snapshot.debian.org pins + `verify-apt-pins.sh` CI guard; its Dockerfile/publish.yml comments mis-cite ADR 0130, must become 0131 before commit); formal `make test-hardware` 0-NOT-RUN re-run (dongle+DAC attached, blocked.md row 3); sustained `--source capture` 1 h (G19); v* tag (G20, user decision); per-character board voice (G3/#36/#25) |

### Gap rows — strict status (full evidence in `acceptance-2026-10-20.md`)

G1/G2/G4/G6/G7/G9/G10/G11/G13/G14/G15/G17 **IMPLEMENTED** (committed and
gated at `aaa5967` or the dev-host gate cited per row; G13's waveform
sub-item now present in `web/src/pages/developer/ActionPanel.tsx`).
G3, G5, G12, G16, G18 **PARTIAL** (remains per the acceptance table; new
issues filed: **#32** publish defect, **#33** G12, **#34** G16, **#35** G18,
**#36** G3; plus **#38** — the #22-class load-sensitive
`TestSyntheticReplaySessionRealTesseract` that turned the `5fcf61e` CI
re-gate red at audit (gate infrastructure, not a gap row)). G8 **PARTIAL
+ BLOCKED** (Dolphin real-capture halves; synthetic halves green).
G19, G20 **BLOCKED** (hardware / user decision; unblock steps in
`blocked.md`). Open pre-existing issue not covered by a gap row: **#37**
(Cast voiced-count; `redesign-cast2` in flight).

### Issues filed by this audit (2026-10-21)

| # | Issue | Row / area |
|---|---|---|
| 32 | Edge publish broken: `packaging/Dockerfile` Debian 12 version pins no longer in the archive (apt-get exit 100); edge tag stale at `661bccf` | M8 release infrastructure (new defect found while verifying the "signed edge publishes live" claim) |
| 33 | G12: second-game dev-UI leg has no test coverage; the live-TTS evidence runner was one-shot and deleted | G12 |
| 34 | G16: a11y of the player page during a live session untested (axe stops at the start form); no table alternative to the canvas graph | G16 |
| 35 | G18: decide the spec-08 `PUT /api/v1/config` contract — declare or close the ADR-0047 deviation (405 by design; audio-only mutable via ADR-0074) | G18 |
| 36 | G3: synthetic two-character, two-voice speech regression test (the live per-character proof is one-shot and outside every gate recipe) | G3 |
| 38 | `TestSyntheticReplaySessionRealTesseract` load-sensitive on CI: real-Tesseract reads 5–12 s under runner load → 4 degraded frames → `commits = 1, want 2`; the `5fcf61e` CI re-gate (run 39542298642) is red; #22's fix did not cover this test | Gate infrastructure (M0/M2/M3/M5/M7 re-gates all wait on a green run; found while verifying the post-`aaa5967` re-gate state) |

(Not filed by this audit but open at audit: **#37** Cast voiced-count UX —
being redesigned on `redesign-cast2`.)

*The 22:40 update below is superseded for its "re-gate in flight" and
issue-disposition claims (the diff is committed as `5fcf61e`; CI moved on;
issues #22/#26/#27/#31 closed, #29 closed by `fe62a51`, #12 closed earlier
the same day). Its historical record of the verified backlog is kept as-is.*

---

## UPDATE — 2026-10-20 ~22:40 (supersedes the 05:41 state below)

Tree: HEAD `b1db234` (main). Uncommitted: 46 modified + 14 untracked —
in-flight work only (draft ADRs 0113/0115/0116/0122 (other work), 0123/0124/0125 (dolphin-gate), catch-up
narration `internal/speech/catchup.go`, perceptual hash
`internal/vision/phash.go`, G8 telescope re-speak gate).

**The verified backlog has been committed.** Since `baf8aec`: `c2e2f57`,
`45c203f` (#16), `c24ec20` (#17), `85a80f6` (#18–20), `b14e24f` (warm OCR
pool, #8/#13, ADR 0100), `c28bfca` (#14), `e3da35a` (G17, ADR 0106),
`f35b429` (G14, ADR 0107), `fd9c6e6` (UX-06), `a9df278` (UX-03),
`49e5f68` (UX-11/UX-13), `551723c` (merge fallout, full gates), then the
redesign landings `84acd86` (Phase 0), `661bccf` (Library/⌘K/agent dock),
`d2bfc84` (setup board), `93b5c09` (Traces), `10e8163` (merge fallout),
`b7e6008` (#21/#23), `a8123c8` (hub-HDMI ALSA hw sink, ADR 0114, #5),
`b1db234` (doctor test timeout scale).

CI: last **green** full `ci` run is `661bccf` (39486299219); since:
`10e8163` cancelled (×2), `b7e6008` failure (load-sensitive), `a8123c8`
failure at `make lint` (fixed by `b1db234`), `b1db234` **in progress**.
So the rows below that say "PARTIAL (uncommitted)" are now **committed**;
those inside the green `551723c`/`661bccf` runs are re-gated, and
`d2bfc84`/`93b5c09`/`10e8163`/`b7e6008`/`a8123c8`/`b1db234` await the
in-flight green re-gate. **Current per-milestone/per-gap statuses: see
`docs/status/acceptance-2026-10-20.md` (DRAFT — final numbers pending).**

Live hardware proof 2026-10-20 18:22 (external bundle
hw-proof-20261020T1822): real GameCube HDMI frame → Tesseract OCR (0.95) →
matcher → genuine Clef decision (request id recorded in the manifest) →
commit → TTS → **playback out of the UNO Q hub HDMI, heard by the
operator**; tailnet UI up. Voice = narrator fallback; frame samples + WAV
export and the uncertain-line escalation fix (ADR 0115, in flight) are
the recorded gaps.

---

## State as of 05:41 (historical)

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

*Superseded for the "Fix — uncommitted" rows by the 22:40 update at the
top: the diff is committed (verified backlog + integration commits);
re-gating is in flight and the draft acceptance report carries the
current row statuses.*
