# Acceptance report — 2026-10-20 — **DRAFT — final numbers pending**

**Audit update 2026-10-21 ~04:30, revised ~04:55** (per-milestone/per-gap
claims re-verified against the code — routes, tests, scenario YAMLs,
Makefile gates — and against CI/gate-run state; see
`milestones-2026-10-20.md` for the per-milestone table): the "other work"
diff cited below committed as `5fcf61e` (ADR
0113/0115/0116/0119/0122/0126; fixes #22/#26/#27/#31, refs #25/#28), and
its CI run **failed at audit** (run 39542298642, 04:22 UTC — `gates` job,
race unit, `TestSyntheticReplaySessionRealTesseract`, the #22
load-sensitivity class in a test #22's fix did not cover → **issue #38**).
Uncommitted at the revision: the dolphin-gate worker's diff
(internal/dolphin, Makefile, ADR 0123/0124/0125) **plus** the in-flight
ADR 0135 placement-aware textbox region (`internal/pipeline`,
`internal/vision/textpresence.go`) and the in-flight **#32 fix** (ADR 0131,
`packaging/Dockerfile`, `.github/workflows/publish.yml`,
`scripts/release/verify-apt-pins.sh` — its comments mis-cite ADR 0130,
must become 0131 before commit). Issues filed by the audit: **#32, #33,
#34, #35, #36, #38** (below). Gate placeholders for the orchestrator: see
the "Gates (orchestrator to fill in)" section.

Tree at audit time: HEAD `5fcf61e` (main). Uncommitted at the revision:
`Makefile`, `internal/dolphin/{drive,record,record_real_test,
savestate_fixture_test,telescope_test}.go`, ADRs 0123/0124/0125,
`docs/status/blocked.md` (dolphin-gate progress); ADR 0135 +
`internal/pipeline/{config,dispatch,roi,placement,placement_test}.go`,
`internal/vision/textpresence.go` (placement-aware textbox region);
ADR 0131 + `packaging/Dockerfile`, `.github/workflows/publish.yml`,
`scripts/release/verify-apt-pins.sh` (the in-flight #32 fix); this status
pair. **Nothing in this draft is claimed from the uncommitted diff.**

**Strictness rule** (carried from the milestones brief): **VERIFIED** =
committed AND proven by a gate run on the committed tree. Anchors at audit:

- Last **green** full CI (`ci` workflow): **`aaa5967`** (run 39528569626,
  2026-10-21 01:39, 17 m 27 s — lint, secret-scan tree+history+self-test,
  race unit, web contract/typecheck/lint/fmt, vitest, test-replay);
  earlier green: `b1db234` (39508975819), `661bccf` (39486299219),
  `551723c` (39462838939). CI since `aaa5967`: `e1e2c6a` **failure**
  (race unit 3253 PASS / 1 FAIL — `TestCaptureSessionCommitsSyntheticText`,
  the known load-sensitive flake, #22; fixed by `5fcf61e`); `5fcf61e`
  **failure** (run 39542298642, 04:07–04:22 — `gates` job, race unit:
  `TestSyntheticReplaySessionRealTesseract`, `internal/runtime`
  manager_test.go:90, `commits = 1, want 2` — 4 of 13 real-Tesseract reads
  at 5–12 s under CI load degraded, FrameToOCR P50 5 s / max 12.2 s; the
  #22 class in a test #22's fix did not cover → **issue #38**). **No green
  CI since `aaa5967` at audit.**
- `e1e2c6a` and `ff799d3` both failed on the same `TestCaptureSession…`
  flake (CI runner load); `a8123c8` failed at `make lint` (fixed by
  `b1db234`, which is green).
- Last full local gate run on a committed tree: `3625f55` (2026-10-19,
  `docs/status/acceptance.md` re-issued `3afe9a4`). Dev-host runs cited
  below that post-date it (`models-gate-final.log`, bench-g12-check) are
  labelled as working-tree runs where the diff was then uncommitted.
- **Publish edge:** last successful `edge` publish `661bccf` (2026-10-20
  20:04); `b1db234`/`aaa5967` edge jobs **failed** at the GoReleaser
  snapshot step — `packaging/Dockerfile:25-29` pins Debian 12 point-release
  versions no longer in the archive (apt-get exit 100) → **issue #32**;
  `e1e2c6a` publish skipped (CI red).

Method: every row states what the committed tree proves at its anchor
(green CI through `aaa5967` or the `3625f55` gate run) versus what landed
after and awaits the in-flight re-gate. Hardware rows cite the external
evidence bundle **hw-proof-20261020T1822** (real hardware, 2026-10-20
18:22) by kind and request id only — game text and paths never enter the
repo (AGENTS rule 5). Dev-host artifact bundles are cited by path under the
gitignored `artifacts/` tree.

## Milestones M0–M8 (docs/spec/16)

Per-milestone detail and the audited per-gap table: see
`milestones-2026-10-20.md` (2026-10-21 audit section). This table carries
the strict status only.

| M | Status | Evidence (committed anchor) | Remains |
|---|---|---|---|
| M0 | **VERIFIED** | Green CI through `aaa5967` (bootstrap/doctor/make targets, secret-scan `--history` + self-test); startup-logging fix landed `5fcf61e` (ADR 0126, #31) | `5fcf61e` CI re-gate **red at audit** (run 39542298642, #38); anchor stays `aaa5967` |
| M1 | **VERIFIED** | Green CI through `aaa5967`; G14 lifecycle `f35b429` (ADR 0107); portability/migration/backup/10k-search rows as recorded at `3625f55` | None material |
| M2 | **VERIFIED** | Green CI through `aaa5967`; bench re-run on the dev host at `d7c2ed0` (recall 97.9 %, top-1 100 %, 0 dup/stale); ADR 0115/0116 (escalation/retry, glyph-noise trim, typewriter-safe perceptual dedupe, OCR finalize timeout) committed `5fcf61e` (fixes #26) | `5fcf61e` CI re-gate **red at audit** (#38); ADR 0135 placement-aware textbox region in flight (uncommitted) |
| M3 | **VERIFIED** | `3625f55` anchor: `make test-models` 29 PASS / 0 NOT RUN; dev-host re-run after `e1e2c6a`'s web-import assertion scoping: **33 PASS / 0 NOT RUN** (working-tree run, `artifacts/gates/models-gate-final.log`); live onboarding chain `12f8c9f` | Recorded-run cadence (acceptance Gap 4); `5fcf61e` re-gate red at audit (#38) |
| M4 | **VERIFIED** (per-character proof recorded once; synthetic leg open) | `3625f55` rows (audition/approval, restart survival, cache miss→hit, rapid advance); ADR-0101 mappings; **live per-character proof 2026-10-20 05:30** (`TestLivePerCharacterVoices`, bundle `artifacts/per-character-live/perchar-20261020T053008Z/`); catch-up narration + no-backlog policy committed `5fcf61e` (ADR 0122, #25) | Synthetic two-voice regression test → **issue #36**; wire `TestLivePerCharacterVoices` into `make test-dolphin` (#25 acceptance) |
| M5 | **VERIFIED** (one open a11y defect, #30) | 38 Playwright specs; redesign Phase 0 `84acd86`; **all Phase 1 screens on main** (Library/⌘K/agent dock `661bccf`, setup board `d2bfc84`, Traces `93b5c09`, Health `ec516e0`, Settings+theme `39f45fb`, Cast `d835603`, Play `964746a` + merge fixes `aaa5967` — green CI through `aaa5967`) | **#30** (Lilac Night WCAG contrast on Cast/Play, 16 serious axe nodes); G16 a11y-during-play + graph table view → **issue #34**; `5fcf61e` re-gate red at audit (#38) |
| M6 | **VERIFIED** (scenario growth in flight) | `3625f55` anchor: 7/7 real Dolphin, 16/16 scenarios 0 BLOCKED; 17 dolphin scenario YAMLs now (16 green + `dolphin/sign-opened` reported BLOCKED, never passed); #12 closed (ADR 0102 round trip) | `dolphin/sign-opened` + `dolphin/item-telescope` real-capture fixtures BLOCKED (blocked.md rows 1–2); in flight (uncommitted dolphin-gate diff): ADR 0123 first-box persistence, 0124 newgame holdout bench leg, 0125 telescope re-speak + blocked gate |
| M7 | **VERIFIED** (live-HDMI fix re-gate pending; 1 h UNO Q soak BLOCKED) | Bench targets met at `3625f55` (named-speaker 100 %, recall 19/19, top-1 19/19, 0 dup/stale; calibration ADOPTED rev 2); soak 1 h + 2 h `bea3d94`; live proof 2026-10-20 18:22 (below) | ADR 0115/0116 + 0124 re-gates (the `5fcf61e` run red at audit — #38); G19 1 h soak on the UNO Q BLOCKED (hardware) |
| M8 | **PARTIAL** (live hardware proof achieved; formal re-gate + two decisions remain) | Live proof bundle hw-proof-20261020T1822 (below); hub-HDMI ALSA hw sink `a8123c8` (ADR 0114, `TestHWALSAHubHDMI` `hw_ptr` assertion — NOT RUN without the board); cold-boot ADSP recovery `0f9fe74` (ADR 0118, #24); lifecycle 9/9 on device; tagged path proven locally (`dde81e6`); 30-min sustained replay PASS (`61052a1`) | **Signed edge publishes stale at `661bccf` — edge job Debian-12 pin defect, issue #32 (fix in flight: uncommitted ADR 0131 snapshot-pins + `verify-apt-pins.sh` CI guard; its ADR 0130→0131 cross-refs must be fixed before commit)**; formal `make test-hardware` 0-NOT-RUN re-run (blocked.md row 3); sustained `--source capture` 1 h (G19); v* tag (G20); per-character board voice (G3, #36/#25) |

## Gap rows (gap-audit-2026-10-19.md, G1–G20)

All rows that were "PARTIAL (uncommitted)" at 05:41 on 2026-10-20 have
since been **committed** in the verified-backlog landing (`a54e4cf`) and the
integration commits, and sit inside the green `551723c`/`661bccf`/`aaa5967`
CI runs; strict status below, audited against the code at HEAD `5fcf61e`
(2026-10-21).

| # | Status | Evidence (committed unless noted) | Remains / unblock |
|---|---|---|---|
| G1 capture session | **VERIFIED** (capture leg; formal gate re-run pending) | ADR 0078 `buildFrameSource` v4l2 + `internal/capture/stack` (committed `a54e4cf`; `SourceKindV4L2` builds the real `ffmpegpipe.V4L2` — `internal/runtime/source.go:264`, the old 501 is gone); `TestCaptureSessionCommitsSyntheticText` (internal/runtime, unit gate); Playwright `capture-device.spec.ts`; live on-device v4l2 session 2026-10-20 and the 18:22 real-hardware chain (below); `d7c2ed0` truthful preflight; default-source fix (#28 backend: omitted `frame_source` resolves to a ready capture device) committed `5fcf61e` (ADR 0119) | `make test-hardware` device leg re-run (0 NOT RUN) with the dongle attached (blocked.md row 3); sustained `--source capture` |
| G2 device API/UI | **VERIFIED** (committed, gated at `aaa5967`) | ADR 0078 routes registered (`/api/v1/capture/devices` GET/PUT, `/preview`, `/probe` — api.go:713–716) + Settings/Play UI; `internal/api/capture_test.go` (9 tests); issue-#10 fixes with golden tests (committed) | Real-dongle enumeration on device re-run (blocked.md row 3) |
| G3 per-character voices in play | **PARTIAL** (live proof recorded once; synthetic leg + gate wiring open) | Resolution wired end to end (`resolveVoice`/`resolveVoiceForCharacter`, `speakerByMessage`, ADR-0101 narrator fallback, ADR-0070 speaker link); named-speaker bench metric (`internal/bench/metrics.go`); **live proof 2026-10-20 05:30**: `TestLivePerCharacterVoices` (`internal/e2e/perchar_live_test.go`) — two characters, two distinct designed voices, 5/5 committed lines in the linked character's voice, 5/5 ASR-QA passed, WAVs differ (bundle `artifacts/per-character-live/perchar-20261020T053008Z/`); board-cast in progress on the device | No synthetic two-character/two-voice regression test (all `internal/runtime` speech tests seed the narrator) → **issue #36**; `TestLivePerCharacterVoices` matches none of the `make test-dolphin` recipe's `-run` patterns (manual invocation only) → wire into the gate per #25's acceptance |
| G4 research-only onboarding | **VERIFIED** (committed, gated at `aaa5967`; live test re-scoped `e1e2c6a`) | ADR 0077 rights-gated web-source import (`internal/agent/tools/websource.go` + 6 tests, committed); `TestLiveWebSourceProposalForPublicTitle` PASS after `e1e2c6a` scoped the hash assertion to the proposed source (models-gate-final: 33/0, working-tree run) | "No disc → publishable" / "malicious dump refused" scenario legs |
| G5 USB DAC product surface | **PARTIAL** (committed; hardware NOT VERIFIED) | ADR 0074 audio devices/selection/tone/gain (`/api/v1/audio/devices`, `/test-tone` — api.go:689–690) + `internal/bt` (ADR 0091) + Settings UI (committed, gated at `aaa5967`); Playwright `audio-output.spec.ts` / `bluetooth-output.spec.ts` | DAC tone/latency on hardware (blocked.md row 3); note: the 2026-10-20 live proof played audio out of the **hub HDMI** (ADR 0114), not the USB DAC |
| G6 restore from UI | **VERIFIED** (committed, gated at `aaa5967`) | ADR 0090 `internal/restore/` + `POST/GET /api/v1/admin/restore` (api.go:798–799; validated upload, pre-restore safety copy, scheduled restart, recovery on failed swap); `TestAdminRestore*` + `TestValidateBackup*`/`TestApply*`; live walk evidence 2026-10-20 02:33 (worker log); Playwright `restore-walk.spec.ts` | None material |
| G7 player controls | **VERIFIED** (committed, gated at `aaa5967`) | ADR 0075 `POST /sessions/{id}/pause` + `/resume` + `PUT /sessions/{id}/settings` (api.go:723–725); narrated-class setting + mid-session mode + catch-up policy in the session reducer (`internal/runtime/session.go:43`, `internal/session/apply.go`); `internal/runtime/controls_test.go`; Playwright `play-controls.spec.ts`; catch-up narration committed `5fcf61e` (ADR 0122, #25, incl. `gn-t025-catch-up` synthetic scenario — scenario itself uncommitted with the dolphin-gate diff) | Playwright pause-mutes-speech assertion (functional leg in `play-controls.spec.ts`; the a11y/visible-state assertion is the audit's ask) |
| G8 sign/item narration | **PARTIAL** + **BLOCKED** (Dolphin real-capture halves) | Committed-class path + `TestSignItemRegionsCommitTheirClass` (`internal/pipeline/signitem_test.go`); ADR 0102/0103 checkpoint + content-kind marks (#12 closed); synthetic `sign-opened-001`/`item-obtained-001` green. `testdata/scenarios/dolphin/sign-opened.yaml` still `status: blocked` (no `manifest_sha256` pin); no `dolphin/item-telescope.yaml` exists. In flight (uncommitted dolphin-gate diff): ADR 0125 telescope re-speak + blocked gate, 0123 first-box persistence, 0124 newgame holdout bench leg | **Unblock (blocked.md rows 1–2):** sign — record a fixture reaching a real sign (deck walkway broken; ADR 0102 round trip makes the checkpoint reliable), pin SHA-256, name the sign line, drop `status/reason`. Item — measured SPEAK route does not hand over the telescope (2026-10-20, 7 runs); find the actual handover route, re-record, pin the manifest. Until then BLOCKED, never pass (ADR 0037; `TestBlockedScenariosAreReportedNotPassed` keeps the rule exercised) |
| G9 first-run wizard | **VERIFIED** (committed, gated at `aaa5967`) | ADR 0087 setup wizard steps devices/storage/models/audio/privacy/done (`web/src/pages/Setup.tsx`) + Playwright `setup-wizard.spec.ts` on its own packaged-binary appliance | None material |
| G10 diagnostics completeness | **VERIFIED** (committed, gated at `aaa5967`) | ADR 0087 + ADR 0111: `/diagnostics` redirects to `/health`; the health page carries host CPU/RAM/storage/environment (`web/src/pages/health/sections.tsx`), model-role probes, voice registry, redacted logs, job queue, and the speaker-test tile (`health/registry.tsx` — consented 440 Hz tone) | None material |
| G11 updates/notifications | **VERIFIED** (committed, gated at `aaa5967`) | ADR 0088 `internal/updates/` (bounded check, offline-safe cache, no auto-install) + Settings section + doctor row + `TestCheck*`/`TestSettingsUpdate*`/`TestUpdatesRow*`; Playwright walk asserts `update-notes-link` against the fake releases server (`restore-walk.spec.ts`) | None material |
| G12 second-game recognition/TTS leg | **PARTIAL** (bench leg green on the dev host; dev-UI leg untested; TTS leg one-shot) | ADR 0079 `testdata/bench/bench-synthetic-002/` + `generate002.go` committed; the second-set holdout leg runs in `make bench` (Makefile) and passed on the dev host (run 20261019T222205Z, sha `938f883`: recall 100 %, top-1 100 %, named-speaker 100 %, 0 dup/stale — `artifacts/bench-002/`); live TTS leg proven once (real Design+Generate, WAV + SHA-256 — `artifacts/second-game-live-tts/`), its runner **deleted after the run** per the manifest; `assertNoGameVocabulary` green | Dev-UI leg: no spec touches the second game (`grep -rn "Black Water\|bench-synthetic-002" web/e2e/` → nothing) → **issue #33** (dev-UI walk + repeatable live-TTS test + bench evidence in the run manifest) |
| G13 developer dashboard actions | **VERIFIED** (committed, gated at `aaa5967`) | ADR 0083 per-event correction actions, save-regression-fixture, evidence download (`web/src/pages/developer/ActionPanel.tsx`); **waveform sub-item now present** (the utterance's WAV peaks drawn on the inspector's canvas — `ActionPanel.tsx` + `developer/actions.ts`) | None |
| G14 library lifecycle | **VERIFIED** | Committed `f35b429` (ADR 0107) inside green CI: `POST /profiles/{id}/duplicate|clone`, `DELETE /profiles/{id}`, `POST /profiles/{id}/purge`, `POST /games/{id}/disable|enable` (api.go:872–878) + store tests | None |
| G15 successor prewarming | **VERIFIED** (committed, gated at `aaa5967`) | ADR 0095 `speechWiring.initPrewarm/prewarm/cancelPrewarmBatch`, top-k weighted successors, entropy+byte-quota gates, versioned cache key (`internal/runtime/speech.go`); `internal/speech/prewarm_test.go` | Bench hit-rate stats line |
| G16 player-page a11y during play | **PARTIAL** (committed diff; live-session axe + table view open) | Committed-lines `aria-live` regions (`web/src/pages/play/NowSpeaking.tsx` — the aria-live caption the suite pins; `ControlBar.tsx`, `KidMode.tsx`); axe walk extended to the new /cast and /health routes | Axe walk still settles /play on the **start form only** ("No session, audition or preview runs in this walk"); no table alternative to the virtualised canvas graph (`web/src/pages/Graph.tsx`) → **issue #34** |
| G17 metrics surface | **VERIFIED** | Committed `e3da35a` (ADR 0106): `GET /metrics` (Prometheus text, admin-gated) + `GET /api/v1/dev/metrics` (api.go:552–557, 732), `internal/telemetry/` package, `internal/api/metrics_test.go`; bottleneck report (ADR 0069) committed earlier | None |
| G18 runtime config changes | **CLOSED WITH DEVIATION** (decision recorded 2026-10-21, ADR 0133; #35 closed completed) | `PUT /api/v1/settings` answers 405 by design with the env list (`internal/api/settings.go`, `handleSettingsPut`; the 405 contract pinned by `internal/api/settings_test.go`); exactly one runtime-mutable setting: audio output + master volume via `PUT /api/v1/settings/audio` (ADR 0074, `internal/api/audio.go`), env override precedence documented | The spec-08 `GET/PUT /api/v1/config` contract is superseded by the `/api/v1/settings` contract per **ADR 0133** (deviation kept: single-operator LAN appliance; env plus restart; the audio setting is the one runtime-mutable exception, ADR 0074) — the spec row now points at `/api/v1/settings` with the ADR note. **issue #35 closed completed** |
| G19 1 h UNO Q replay soak | **BLOCKED** (hardware) | 30-min replay-source run PASS on device (`61052a1`, `artifacts/hw-sustained/sustained-20261019T134452Z/`); 1 h + 2 h on Arch host `bea3d94` (not the UNO Q profile) | **Unblock (blocked.md row 3):** attached hub + UVC dongle + USB DAC + TTS voice; `make test-hardware` to 0 NOT RUN; sustained run `--source capture` 1 h; archive the report |
| G20 tagged v* release | **BLOCKED** (user decision) | Local never-pushed `v0.1.0-rc.0` proven (`dde81e6`); signed edge/snapshot publishes live **through `661bccf`** (2026-10-20 20:04) — the edge job has since failed on the `packaging/Dockerfile` Debian-12 pin defect (**issue #32**; fix in flight, uncommitted ADR 0131), so the `edge` tag is stale until that lands | **Unblock:** user picks a version; `git tag -s vX.Y.Z && git push origin vX.Y.Z`; verify per docs/manuals/17; #32 (re-pin + edge re-publish) must land first or the tagged images inherit the same build defect |

## The goal's six done-criteria

1. **Per-milestone status M0–M8** — table above (2026-10-21 audit): M0–M4,
   M6–M7 VERIFIED (at their committed anchors; green CI through
   `aaa5967`; the `5fcf61e` re-gate **red at audit** — run 39542298642,
   `TestSyntheticReplaySessionRealTesseract`, #38 —), M5 VERIFIED with
   the #30/#34 a11y caveats, M8 PARTIAL. Per-gap strict statuses: G3, G5,
   G12, G16 PARTIAL (issues #36, blocked.md row 3, #33, #34
   respectively), G18 CLOSED WITH DEVIATION (ADR 0133, decided 2026-10-21 —
   #35 closed completed), G8 PARTIAL+BLOCKED, G19/G20 BLOCKED, the rest
   VERIFIED.
   **Met by this document.**
2. **Per-gap-row status** — all 20 audit rows above. **Met.**
3. **Evidence per row** — commit + ADR + gate run / CI run id cited
   inline; uncommitted work cited by ADR + file, never as a gate pass on
   the committed tree; hardware evidence cited by bundle kind + request
   id only. **Met.**
4. **What remains per row** — "Remains" columns above. **Met.**
5. **Unblock steps for BLOCKED rows** — G8 (blocked.md rows 1–2, measured
   2026-10-20 incl. the telescope route measurement), G19 (blocked.md row
   3), G20 (tag runbook). **Met.**
6. **Open-issue dispositions** — next section. **Met.**

## Redesign (Plum & Peach, ADR 0093) — per-screen status

All four previously approved-pending-merge screens are **merged on main**
(Health `ec516e0`, Settings+theme `39f45fb`, Cast `d835603`, Play
`964746a` + merge fixes `aaa5967` — green CI through `aaa5967`); polish per
the Phase 2 list. New defect found at audit: **#30** (Lilac Night theme
fails WCAG color-contrast on Cast and Play — 16 serious axe nodes).

| Screen | Status | Evidence | Phase-2 polish (open) |
|---|---|---|---|
| Phase 0 shell (tokens, themes, self-hosted fonts, status bar, kit) | **On main** | `84acd86` | At 390 the status bar wraps to 3 lines — collapse nav/user into a menu |
| Library | **On main** (approved run-3494091) | `661bccf` (ADR 0140-library), gated at green CI | DitherCover bands (peach→ink stack); 5-column shelf grid (responsive to 2 at 390); remove underline on pill links; agent pill overlaps headings at 390 (bottom padding or dock as bar) |
| ⌘K search | **On main** (approved run-3600828) | `661bccf` | Line-result id column (full ULIDs) squeezes titles at 390 — hide below ~600 px or short id |
| Agent dock | **On main** (approved run-3602450) | `661bccf` (ADR 0099) | Empty dock: short screen-aware "try asking…" hint list |
| Setup board | **On main** (approved run-3772315) | `d2bfc84` (ADR 0108; fake setup gateway + 621-line e2e walk) | Tool cards show raw run ULIDs (short id / "details"); board columns use ~2/3 width — widen cards |
| Traces | **On main** (approved, traces-width-*) | `93b5c09` (ADR 0141-traces; `/developer`→`/traces` redirect) — green re-run pending | "Why this line" candidate rows show message ULIDs — show line-text snippet (same fix as Play candidates) |
| Health | **On main** (approved run-4106405; merged `ec516e0`) | 3×2 status grid, UX-11 signal/preview tile, `/diagnostics` rename | Headline copy for unprobed states: human copy ("5 checks not run yet" / "Everything's mostly fine" / "2 need attention") |
| Settings | **On main** (approved run-4175830; merged `39f45fb`) | Real token-slot grid + kit form controls (`eeb69ff`), synthetic-tailnet allowlist (`d841a09`) | (review-1 findings resolved pre-approval) |
| Cast | **On main** (approved run-78545; merged `d835603`) | Characters+voices merged, auto-cast dial, undo, visual gates (`7bc1dec`/`4e7463c`) | Agent pill overlaps "3 more takes" (covered by the global pill-gutter fix); #30 contrast nodes on this screen |
| Play | **On main** (approved run-591998; merged `964746a` + `aaa5967`) | View-switch dispatch, race-tolerant stop, review fixes at 1440/1024/390 (`3ff3383`/`40c0604`/`52015b2`); main-side walks landed `aaa5967` | Now-speaking control bar lacks the coral Stop in the default view (mock has it; operator view only today); review-shot capture artifact (sticky status bar mid-page); #30 contrast nodes on this screen |

## Hardware (UNO Q)

**Proven live on 2026-10-20** (external evidence bundle
hw-proof-20261020T1822; device: Arduino UNO Q (the operator's board); capture:
RYBOZEN V316B HDMI dongle 640×480 YUYV @10 fps; source: a real GameCube
Wind Waker session; build recorded in the manifest as `b7e6008`):

- the full chain end-to-end: v4l2 frame → Tesseract OCR (conf 0.95) →
  matcher candidates → **Clef decision with a genuine collective request
  id** → commit `CLEF_DISAMBIGUATED` → TTS `speech.ready` → **playback
  started and ended out of the UNO Q hub HDMI, heard by the operator at
  the receiver**;
- the developer UI was reachable over the tailnet at the device's
  tailnet address (port 8080) and showed the session.

Supporting committed work: `a8123c8` (ADR 0114) — pure-Go ALSA hw sink
drives the hub-HDMI route (MultiMedia4 → SEC_MI2S → ANX7625 → USB-C DP →
HDMI) with the exact proven hw_params (S16_LE, 48 kHz, 2 ch,
period 6240×4), 24 kHz mono TTS up-converted to 48 kHz stereo, stream
kept alive with silence; a "hub HDMI (ANX7625)" device profile in the
audio API (`kind hdmi`), a read-only doctor row checking the SEC_MI2S
route with the amixer fix printed on a misroute, and a hardware-gated
test asserting `hw_ptr` advances (NOT RUN without the board).

**Gaps (still open):**

1. **Voice is the narrator fallback** — characters are unnamed
   placeholders; board-cast is in progress on the device (G3; #36 synthetic
   leg + #25 gate wiring).
2. **Frame/crop samples not retrievable** for the traced session (dev-trace
   asset eviction or not stored) and **the audio WAV was not exported** —
   both to be captured in a re-proof.
3. **Uncertain-line escalation fix — committed, re-gate pending** — ADR 0115
   (escalation retry on the live HDMI source, glyph-noise handling,
   typewriter-safe perceptual dedupe) + ADR 0116 (OCR finalize timeout)
   landed in `5fcf61e` (fixes #26); the `5fcf61e` CI re-gate **failed at
   audit** (run 39542298642 — `TestSyntheticReplaySessionRealTesseract`,
   #38), so the fix is committed but not yet re-gated green.
4. **Cold-boot audio quirk** (issue #24): on some cold boots the audio
   DSP's APR services never register, so there is no sound card — the
   recovery (ADSP restart) is committed `0f9fe74` (ADR 0118); the doctor
   row (ADR 0114) surfaces the SEC_MI2S route with the amixer fix on a
   misroute. Close on a cold-boot re-run on the board.
5. **Formal gate re-runs**: `make test-hardware` on the attached dongle +
   DAC to 0 NOT RUN (capture and hub-HDMI audio are proven live above,
   the gate itself not yet re-run at 0 NOT RUN); sustained 1 h
   `--source capture` (G19); v* tag (G20) — and **the edge-publish defect
   (#32) must land first**: the `edge` image build (Debian-12 apt pins in
   `packaging/Dockerfile`) has been failing since `b1db234`, so the `edge`
   tag is stale at `661bccf` and a v* cut would inherit the same build
   step. Fix in flight at audit: the uncommitted ADR 0131 diff
   (snapshot.debian.org pins + `verify-apt-pins.sh` CI guard), with the
   ADR 0130→0131 cross-refs to correct before commit.

## Open GitHub issues and dispositions (13 open at the 04:55 revision: 7 pre-existing — incl. #37, filed 04:18 by the Cast worker — + 6 filed by this audit)

| # | Issue | Disposition |
|---|---|---|
| 2 | Optional Bluetooth speaker output | **Fix — committed, hardware leg open:** ADR 0091 + `internal/bt` + Settings UI committed (gated through `aaa5967`); pairing/selection on the physical unit still needs the device (blocked.md row 3). Keep open until verified on hardware. |
| 5 | HDMI/DP audio out of the UNO Q USB-C port | **Fix — committed, proven live:** `a8123c8` (ADR 0114) lands the ALSA hw sink; the 18:22 live proof played narration out of the hub HDMI with the operator hearing it. Close on a clean re-proof + the `TestHWALSAHubHDMI` `hw_ptr` run on the board (NOT RUN without it). |
| 24 | UNO Q cold boot: ADSP APR services sometimes never register (no sound card) | **Hardware quirk — recovery committed:** `0f9fe74` (ADR 0118) recovers the sound card (ADSP restart path); the doctor row surfaces the SEC_MI2S route with the amixer fix on a misroute. Close on a cold-boot re-run on the board; keep the quirk documented in the manuals/skill. |
| 25 | Responsive live narration: drop backlog, react to new boxes, catch-up for fast A-presses, per-character Dolphin E2E | **Fix — largely landed, E2E legs open:** catch-up narration + no-backlog policy committed `5fcf61e` (ADR 0122); remaining acceptance: wire the per-character live E2E into `make test-dolphin` + a replay scenario per behaviour (the per-character test exists and ran once — see G3/#36), frame-to-voice latency in the bench. |
| 28 | Starting narration from the UI picks the synthetic test_pattern source even when a real capture card is selected and ready | **Fix — backend committed:** default frame source lands `5fcf61e` (ADR 0119; commit refs #28): omitted `frame_source` resolves to a ready capture device, else test pattern in dev mode, else 409. Close once the UI walk asserts the resolved source and the `5fcf61e` re-gate is green. |
| 30 | Lilac Night theme fails WCAG color-contrast on the Cast and Play screens (16 serious axe nodes) | **Open defect (new at audit):** fix the theme token contrasts for Cast/Play and re-run the axe walk; part of the M5 a11y close-out together with #34. |
| 32 | Edge publish broken: `packaging/Dockerfile` Debian 12 version pins no longer in the archive (filed by this audit) | **Bug — release infrastructure:** edge tag stale at `661bccf`; re-pin (or un-pin) the bookworm versions, fail-fast smoke, re-publish + cosign verify per docs/manuals/17. Blocks the G20 tagged images from building cleanly. |
| 33 | G12: second-game dev-UI leg untested; live-TTS evidence runner one-shot (filed by this audit) | **Enhancement:** dev-UI walk for `bench-synthetic-002` + repeatable live-TTS test + bench evidence in the run manifest. |
| 34 | G16: a11y during a live session untested; no table alternative to the canvas graph (filed by this audit) | **Enhancement:** axe scan over the live-session /play state + keyboard-accessible graph table view. |
| 35 | G18: decide the spec-08 `PUT /api/v1/config` contract — ADR-0047 deviation (filed by this audit) | **Decision: keep the deviation — recorded 2026-10-21 (ADR 0133):** the `/api/v1/settings` contract stands (405 PUT by design; single-operator LAN appliance; env plus restart; the audio setting is the one runtime-mutable exception, ADR 0074). The spec-08 row now points at `/api/v1/settings` with the ADR-0133 note, and G18 is marked CLOSED WITH DEVIATION above. **Closed completed.** |
| 36 | G3: synthetic two-character, two-voice speech test (filed by this audit) | **Enhancement:** the synthetic regression leg of G3; with the #25 gate wiring, closes G3's PARTIAL. |
| 37 | Cast: "0 of N voiced" ignores approved voices; roster buries voiced characters under unnamed/retired rows (filed by the Cast worker, 04:18) | **Open defect (pre-existing at audit):** UX redesign in flight on `redesign-cast2`; not a gap-row item. |
| 38 | `TestSyntheticReplaySessionRealTesseract` load-sensitive on CI: real-Tesseract reads 5–12 s under runner load → 4 degraded frames → `commits = 1, want 2` → the `5fcf61e` CI re-gate (run 39542298642) red (filed by this audit) | **Bug — gate infrastructure:** #22's class in a test #22's fix did not cover; scale the read budget / make the commit assertion load-tolerant per #38; blocks every post-`aaa5967` green re-gate. |

Closed since the previous section (for the record): **#29** (voice-registry
slug for two-word roster names — `fe62a51`), **#22** (systemic load
sensitivity — `5fcf61e`: capture-clock channel sync + race-scaled bounds,
ADR 0113), **#26** (ADR-0115 §3 perceptual skip riding typewriter reveals —
`5fcf61e`: typewriter-safe dedupe), **#27** (job-queue foreground-pause
visibility — `5fcf61e`: status/jobs surface), **#31** (startup data-dir /
DB-path logging — `5fcf61e`: ADR 0126); earlier on 2026-10-20: #1, #3, #4,
#6, #7, #8, #9, #10, #11, #13, #14, #16–#23 as recorded; **#12** (mid-game
savestates) closed 2026-10-20 (ADR 0102, round trip PASS ×4).

## Gates (orchestrator to fill in)

Placeholders — the orchestrator fills each cell from the run it performs on
the final tree (command, tree SHA, result, and the artifact path). A gate
is only green from an actually-run evidence file, never from this table
(AGENTS rule 12: no release until gates pass).

| Gate | Command | Tree SHA | Result | Artifact / run id | Notes |
|---|---|---|---|---|---|
| test-unit | `make test-unit` (go vet + `go test -race -timeout 25m -skip TestDolphinScenarios ./...`) | `____` | ____ (PASS / FAIL / NOT RUN) | `artifacts/gates/test-unit.json` + CI run `____` | Race-enabled; CI also runs it (`-json`, annotations) |
| test-replay | `make test-replay` (`./internal/scenario/... ./internal/pipeline/...`, BLOCKED reported, never passed) | `____` | ____ | `artifacts/gates/test-replay.json` + CI run `____` | Includes the synthetic sign/item scenarios + `gn-t025-catch-up` once the dolphin-gate diff lands |
| test-ui | `make test-ui` (typecheck/lint/fmt + vitest + Playwright vs the embedded binary) | `____` | ____ | `web/playwright-report/` + `artifacts/gates/full-test-ui.log` | Assert visible state incl. the #30/#34 a11y fixes once landed |
| test-dolphin | `make test-dolphin` (real drives + `TestDolphinScenarios` + `TestLivePlayUI`) | `____` | ____ | `artifacts/scenarios/<run-id>/index.html`, `artifacts/live-play-ui/<run-id>/` | `sign-opened`/`item-telescope` stay BLOCKED until blocked.md rows 1–2 clear; the per-character test is not yet in the recipe (#25/#36) |
| test-models | `make test-models` (live gateway, `-run Live`, NOT RUN per unset capability) | `____` | ____ | `artifacts/gates/models-gate-*.log` | Dev-host gate (CI never runs live); last dev-host run: 33 PASS / 0 NOT RUN (working-tree run, 2026-10-20 23:07) |
| bench | `make bench` (synthetic-001 + synthetic-002 holdouts; captured legs NOT RUN without the fixtures/image) | `____` | ____ | `artifacts/bench/report.md`, `artifacts/bench-second/report.md` | Second-set leg in the same gate (G12/#33) |
| CI | `ci` workflow on the final tree (lint, secret-scan history+self-test, race unit, web checks, test-replay) | `____` | ____ (run id `____`) | check-run annotations (public) | Last green: `aaa5967` (39528569626); `e1e2c6a` and `5fcf61e` both red at audit (load-sensitive real-OCR unit tests, #22 class; the `5fcf61e` failure is #38) |
| hardware | `make test-hardware` on the attached UNO Q (0 NOT RUN) + sustained 1 h `--source capture` | `____` | ____ | `artifacts/hw-*/` evidence bundle | blocked.md row 3; until then `make test-full` must report this gate NOT RUN, not PASS (G19) |

## Gates and next steps before the final issue

1. The `ci` run on `5fcf61e` **failed at audit** (run 39542298642 —
   `TestSyntheticReplaySessionRealTesseract`, the #22 load-sensitivity
   class in a test #22's fix did not cover; **issue #38**). Land the #38
   fix (load-scaled read budget / load-tolerant assertion) and get a green
   run; until then the anchor for the post-`aaa5967` commits stays
   `aaa5967`, and the `e1e2c6a` red (the #22 capture flake, fixed by
   `5fcf61e`) is superseded by the `5fcf61e` red.
2. Land **issue #32** (Dockerfile Debian-12 re-pin — in flight as the
   uncommitted ADR 0131 diff, `verify-apt-pins.sh` guard included; fix its
   ADR 0130→0131 cross-references before commit) and re-run the `edge`
   publish; the tagged images for G20 inherit the same build step, so the
   v* cut must not happen before the edge job is green again.
3. Land the dolphin-gate diff (ADR 0123/0124/0125: first-box persistence,
   newgame holdout bench leg, telescope re-speak + blocked gate) as a clean
   unit; then re-run `make test-dolphin` + `make bench`.
4. On the attached UNO Q: re-proof exporting frame samples + WAV; board-cast
   a character voice and replay the chain with it (G3); `make test-hardware`
   to 0 NOT RUN; sustained 1 h `--source capture` (G19).
5. Close the a11y items #30 (Cast/Play contrast) and #34 (live-session
   axe + graph table view) so M5/G16 can flip to fully green.
6. Then re-issue this report as final: replace DRAFT, anchor on the new
   green tree + runs (fill the orchestrator table above), and clear the
   blocked.md rows as they verify.

*Secret-scan gating: `scripts/gates/secret-scan.sh` run and gated on exit
code per the task brief (rc=0).*
