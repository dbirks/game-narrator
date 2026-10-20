# ADR-0102: Savestate round trip to a mid-game scene (issue #12)

Date: 2026-10-20
Status: accepted
Owners: issue12-savestate worker
Related requirements/tests: GitHub issue #12 (harness savestates do not
restore on a fresh Dolphin boot — the boot lands on the copyright/title
screen); the G8 Dolphin sign/item rows (docs/status/blocked.md);
ADR-0043 (savestate hotkeys over the keyboard device, F1/F2, the
`StateSaves/` folder, the mtime-verified `SaveState`); docs/spec/11
(evidence bundles); tests `TestRealSavestateRoundTripGameplay`
(internal/dolphin, `GN_GATE=test-dolphin`), `TestCloseDetector*` and
`TestParseStateHeader*` (internal/dolphin, unit gate),
`TestRealRecordSavestateFixtures` (the proven fixture savestates)

## Context and constraints

The harness drives the real game (Wii Wind Waker) under Dolphin in `--batch`
mode. A run is resumed by booting a FRESH Dolphin process with
`--save_state=<file>`: Dolphin boots the disc and, on the CPU thread, calls
`State::LoadAs` before the core transitions Starting→Running. If the load
succeeds the first rendered frame is the saved scene; if it is rejected the
game boots normally to the title/copyright screen and the load is silent to
the harness (the "lands on the copyright screen" symptom of issue #12).

The M6 savestate fixtures (`windwaker-legend-start.state`,
`windwaker-first-dialogue.state`, recorded 2026-10-18) always restore. But
the 2026-10-20 item/sign exploration (docs/status/blocked.md, the G8 Dolphin
row) found that an INTERMEDIATE savestate written at the mid-game lookout
deck did not restore: a fresh `--save_state` boot from it landed on the
copyright/title screen, so every run had to restart from the proven
first-dialogue state. That blocked the G8 sign/item scenarios (which need a
checkpoint at the deck to navigate to the village/house/sign) and is the
reproduction of issue #12.

Constraints: the save/load must stay on the proven hotkey path (ADR-0043:
F1/F2 over the X11 keyboard device, `StateSaves/<GAMEID>.sNN`, the
mtime-verified `SaveState`); the dev host is Xvfb + llvmpipe (deterministic
renders); the game image is user-supplied and never logged; no simulation
theater (AGENTS.md rule 3) — the round trip is only proven by a real Dolphin
run.

## What the source says (Dolphin 2609, Source/Core/Core/State.cpp +
System.cpp, measured for this ADR)

`--save_state` is consumed in `main()` (`save_state_path`) and passed to the
CPU thread, which calls `::State::LoadAs(system, *savestate_path)` BEFORE the
Starting→Running transition. `LoadAs` → `CheckIfStateLoadIsAllowed` (core
Running/Starting, not netplay, not RetroAchievements-hardcore) →
`RunOnCPUThread` (inlined because we are already on the CPU thread) →
`LoadAsFromCore`:

1. `LoadFileStateData` opens the file and reads the header. `ReadStateHeader
   || !ValidateHeaders` → SILENT return (empty buffer, no OSD).
2. `ValidateHeaders` checks the GAME ID (`SConfig GetGameID` vs the state's
   `game_id`; mismatch → OSD "State belongs to a different game (ID …)") and
   the STATE VERSION (`version_cookie - COOKIE_BASE == STATE_VERSION`;
   mismatch → OSD "This savestate was created using the incompatible
   version …").
3. LZ4 decompression: `DecompressLZ4` failure → SILENT return (only a
   "Decompressing State…" OSD precedes it).
4. `LoadFromBuffer` (DoState): an over-read → `LoadAsFromCore` shows OSD
   "The savestate could not be loaded" and `UndoLoadState` (revert to the
   fresh-boot state).
5. Success → OSD "Loaded State from <file>".

Two facts matter for the failure mode: (a) a file that OPENS but whose
payload fails to decompress, or whose DoState over-reads, is a SILENT or
near-silent fallback to the fresh boot — the harness sees only the landing
frame (title/copyright), which is exactly the reported symptom; (b) the save
side (`SaveAs`/`CompressAndDumpState`) is ATOMIC — it writes a
`<file><tid>.tmp`, LZ4-compresses on a worker thread, then `rename`s it over
the slot. The slot file is complete only AFTER the rename.

## The measured reproduction and root cause

Measured 2026-10-20 (`GN_GATE=test-dolphin`, Xvfb + llvmpipe, the
user-supplied Wind Waker image):

- The proven `windwaker-first-dialogue.state` restores: a fresh `--save_state`
  boot lands on the first dialogue box (class `box`, lb=0.000) within seconds.
- The CLOSE step (A on a 2.5 s cadence, a 30 s boot guard, a 3-sample
  full-frame band window) advances the whole first lookout dialogue to the
  deck gameplay scene in ~40 s. The deck is a STABLE full-frame scene
  (class `dark`, mean≈81.5, letterbox band≈0.255, constant over 100+ s of
  sampling with NO input).
- A savestate written at the deck via the bridge `SaveState` (F1, slot 1,
  `StateSaves/GZLE01.s01`, mtime-verified) then KILLS Dolphin and re-boots a
  FRESH Dolphin with `--save_state=<that file>`: the fresh boot lands on the
  DECK scene — the restored frame matches the saved reference X with
  perceptual RMSE 0.042 (≤ the 0.10 cross-step control threshold) AND
  perceptual-hash (aHash64) Hamming distance 3 (≤ 12), full-frame band 0.255.
  Two consecutive runs PASS `TestRealSavestateRoundTripGameplay` (131 s /
  139 s).

So the CANONICAL bridge save→load path round-trips. The 2026-10-20
intermediate-deck failure was a SCRATCH-HARNESS artifact: the scratch's
`STATE,path` step read/copied the state file BEFORE the atomic
compress→rename completed (its LZ4 dump runs on Dolphin's worker thread),
so the captured file had a valid header but a truncated/under-written LZ4
payload. A fresh `--save_state` boot of that file fails SILENTLY at
`DecompressLZ4` (empty buffer, no "Loaded State" OSD) and falls back to the
fresh boot — landing on the copyright/title screen. The size spread of the
failed captures (24.9–26.4 MB vs the expected ~24.8 MB stable) is the
fingerprint of the partially-written payload.

The bridge `SaveState` already avoids this race: it records the slot's
pre-save mtime, fires F1, then POLLS (250 ms) until the slot file's mtime is
newer (the atomic rename has landed) before returning the path
(`hotkeySaveTimeout` 30 s bound). The fix for issue #12 is therefore NOT a
change to the save/load mechanism (which is correct) but making the
REPRODUCTION use the race-safe path and PROVING it with a real round-trip
test, plus a fast header oracle that names the two silent-fallback suspects
(game ID, state version) before a 20-minute fresh boot is spent.

## Decision

1. Keep the proven hotkey save/load path (ADR-0043) unchanged. `SaveState`
   stays mtime-verified (the atomic-write wait is the race guard).
2. Add the CLOSE step as a screen-driven, unit-testable policy
   (`internal/dolphin/close.go`): `CloseDetector` is the pure (timestamp,
   band) timing core — the 30 s boot guard (no settle detection; the
   restore's opening frame is the saved scene before the box reveal), the
   advance→settle phase transition (the first full-frame band>0.15 sample
   after the guard STOPS the A presses — pressing A on the deck triggers a
   letterbox blink, measured 2026-10-20), the 3-sample settle window (each
   ≥2 s apart, any below-floor sample resets to advance). `CloseAdvance` is
   the live loop that feeds it. Unit tests: boot guard, first-full-frame
   transition, three-sample fire, dialogue-frame reset, sub-cadence burst
   ignored.
3. Add the REAL round-trip test `TestRealSavestateRoundTripGameplay`
   (`GN_GATE=test-dolphin`): restore first-dialogue → CLOSE to the deck
   (capture reference X) → `SaveState` (slot 1) → validate the state header
   (game ID + Dolphin state version, `ParseStateHeader`) → stop → FRESH
   `--save_state` boot (a new isolated user dir) → assert the restored frame
   matches X (perceptual RMSE ≤ 0.10 AND aHash64 Hamming ≤ 12 AND full-frame
   band). NOT RUN when the image/tools/strays are absent (never labelled
   unit coverage).
4. Add the header oracle `ParseStateHeader` (`internal/dolphin/stateheader.go`):
   parse the 2609 state header (game ID, version cookie
   `0xBAADBABE+STATE_VERSION`, version string, LZ4/uncompressed facts) and
   reject a wrong-game or wrong-state-version file with a message naming the
   silent-fallback suspect. Unit-tested against synthetic 2609-layout files.

## Tradeoffs

- The round-trip test is a real ~2-minute Dolphin run (two boots). It is
  gated (`GN_GATE=test-dolphin`) and NOT RUN-safe, so it does not slow the
  default gate; it runs under `make test-dolphin` / the issue's command.
- aHash64 (8×8 average hash) is coarse on purpose: it must be insensitive to
  render-to-render jitter (llvmpipe near-identical frames) yet sensitive to a
  scene change (title vs deck). It is the "perceptual hash within tolerance"
  half of the assertion, independent of the calibrated RMSE metric (the
  harness's own 320×240 luma RMSE, render-to-render noise ≤0.025, scene
  change ≥0.05).
- The CLOSE advance→settle policy stops pressing A at the first full-frame
  sample after the guard. This is correct for the measured deck (stable, no
  input needed) but couples CLOSE to "the gameplay scene does not require A
  to hold." If a future checkpoint scene needs held input to stay, the settle
  phase would need a hold policy; that is out of scope here (the deck is the
  G8 checkpoint).

## Validation

- `TestCloseDetector*`, `TestParseStateHeader*`: PASS (unit gate, no
  Dolphin).
- `TestRealSavestateRoundTripGameplay`: PASS twice (131.59 s / 139.72 s),
  `GN_GATE=test-dolphin`; restored frame rmse 0.035/0.042 and hash dist 3,
  vs the 0.15–0.17 / 17–28 of the boot-transient frames (the tolerance sits
  between the two). Evidence PNGs under `artifacts/runs/dolphin-savestate-*/`
  (gitignored): `deck-x-saved-scene.png`, `restored-deck.png`, `close-iter-*.png`.
- The fixture savestates are unchanged (the test reads
  `windwaker-first-dialogue.state` read-only and writes its deck state to a
  temp dir).

## Consequences and reversal

- The G8 sign/item checkpoint is now reliable: a run can save at the deck,
  restart, and resume at the deck (previously every run had to restart from
  first-dialogue). The remaining G8 blocker is the NAVIGATION route (the
  broken deck walkway ends at open water; the route around it to the village
  path is not yet solved) — a separate item, not a savestate defect.
- Reversal: the additions are purely new (close.go, stateheader.go, the two
  test files). Deleting them restores the prior state with no behavior
  change; ADR-0043's hotkey path is untouched.
