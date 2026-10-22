# Milestone status — 2026-10-21 — **#41 close-out refresh**

## UPDATE — 2026-10-21 — the #41 close-out (supersedes the 2026-10-20 update above)

Method: the per-milestone status for this close-out is re-stated from the
#41 close-out acceptance report (`docs/status/acceptance-2026-10-21.md`,
which supersedes the 2026-10-20 DRAFT). The `make test-av` orchestrator-measured
runs of 2026-10-21 (`av-20261021T190256Z` … `av-20261021T212013Z`) are the
evidence for the M2/M7/M8 performance legs; the per-milestone claims were
re-verified against the code and gates before this refresh was written. Nothing
here is claimed from the uncommitted diff.

**Tree at close-out:** HEAD `bc2d89d` — the #41 board-latency and missed-line
fix chain landed on main (ADR 0136/0137/0143/0144/0150–0153 plus amendments);
the final `make test-av` PASS run (`av-20261021T212013Z`, commit `3d2ed35`) is the
last orchestrator-measured A/V evidence.

### Milestones M0–M8 (docs/spec/16)

| M | Status | Evidence (close-out) | Remains |
|---|---|---|---|
| M0 | VERIFIED (anchor `aaa5967`; CI re-gate red at audit, #38) | Green CI through `aaa5967`; ADR 0126/0113/0150 landed post-audit; honest doctor + bootstrap on main | Green CI re-gate on the current tree (blocked on the #38 load-sensitive replay flake) |
| M1 | VERIFIED | G14 lifecycle `f35b429` inside green CI; portability/migration/backup/search rows as recorded | None material |
| M2 | VERIFIED | Bench at `d7c2ed0` (recall 97.9 %, top-1 100 %, 0 dup/stale); ADR 0137 amendment (sea false positive) landed #41/#44; ADR 0136 read-through landed #41 | Re-gate the ADR 0137 placement leg |
| M3 | VERIFIED | `make test-models` 33 PASS / 0 NOT RUN; live onboarding chain recorded `12f8c9f` | Recorded-run cadence (acceptance Gap 4) |
| M4 | VERIFIED (per-character proof recorded once; synthetic leg open) | Catch-up narration ADR 0122 + ADR 0122 amendment (idle flush) landed #41; per-character live proof `artifacts/per-character-live/perchar-20261020T053008Z/` | Synthetic two-voice regression → #36; wire the live per-character test into `make test-dolphin` |
| M5 | VERIFIED (one open a11y defect, #30) | Redesign Phases 0–1 on main through `aaa5967`; #30 contrast defect open; #34 live-session axe + graph table open | #30, #34 close-out |
| M6 | VERIFIED (scenario growth in progress) | 16/16 real Dolphin scenarios + `TestLivePlayUI`; `dolphin/sign-opened` + `dolphin/item-telescope` BLOCKED (blocked.md rows 1–2) | Unblock the two Dolphin halves |
| M7 | VERIFIED (live-HDMI fix re-gate pending; 1 h UNO Q soak BLOCKED) | Bench targets met; ADR 0115/0116 + ADR 0122 amendment re-gates; live proof `hw-proof-20261020T1822` | Re-gate; G19 soak on hardware |
| M8 | PARTIAL (live hardware proof achieved; formal re-gate + two decisions remain) | Board `0.1.0-bc2d89d`; #5 HDMI tone evidence; #37 cast 29/29; presynth job 4331 lines / 4229 synthesized / 4 failed / 1.57 GB / 71 min | Signed edge publish stale (→ #32); formal `make test-hardware` re-run; sustained `--source capture` 1 h (G19); v* tag (G20) |

## Outstanding per-milestone work (close-out)

- M0/M2/M3/M5/M7: re-run the full gate set on the current tree and close the
  re-gate items flagged above (the `5fcf61e` CI re-gate is red at audit, #38).
- M4: synthetic two-voice regression (→ #36) and the live per-character proof
  wiring into `make test-dolphin`.
- M6: unblock the two BLOCKED Dolphin halves (blocked.md rows 1–2).
- M8: the two remaining decisions (signed edge publish, v* tag) and the formal
  hardware re-gate (`make test-hardware`, blocked.md row 3).

## Gap rows — strict status (close-out)

G1/G2/G4/G6/G7/G9/G10/G11/G13/G14/G15/G17 **IMPLEMENTED** (committed and
gated at `aaa5967` or the dev-host gate cited per row); G3, G5, G12, G16, G18
**PARTIAL** (remains per the acceptance close-out); G8 **PARTIAL + BLOCKED**
(Dolphin real-capture half); G19, G20 **BLOCKED** (hardware / user decision).
Full per-row evidence: `docs/status/acceptance-2026-10-21.md`.

## Open GitHub issues — close-out dispositions

Restated from the #41 close-out acceptance report
(`docs/status/acceptance-2026-10-21.md`, "Open GitHub issues and dispositions"):
#2 deferred, #5 fix-committed + proven live, #24 hardware quirk + recovery
committed, #34 enhancement, #37 fix-committed + verified 29/29, #41 open — close
when the Done-means list is met — plus the new issues #42/#43/#44/#45/#46/#47
filed 2026-10-21.

---

*Superseded for its "re-gate in flight" and issue-disposition claims by the
2026-10-21 close-out update above; the historical 2026-10-20 record is kept as-is
in `docs/status/milestones-2026-10-20.md`.*
