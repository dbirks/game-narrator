# ADR-0043: Dolphin drive-to-dialogue ships on the proven single-pipe controller; savestate hotkeys become an opt-in, unproven path
Date: 2026-10-17
Status: accepted
Owners: project
Related requirements/tests: docs/spec/11 (Dolphin automation, "E2E verification steps", "Avoiding false positives"), docs/spec/12 (Dolphin emulator mode), ADR-0007 (headless render + input), ADR-0034 (supervision + input-proof timing); tests `TestRealDolphinBridge`, `TestRealDriveToFirstDialogue` (internal/dolphin), unit tests `TestWriteUserConfigDefaultSinglePipe`, `TestLaunchHotkeysOptIn`

## Context and constraints

The M6 milestone needs the scripted drive (boot → title → START → memory-card
prompt → name entry → first in-game dialogue box) working on a controller
path that is proven. Two facts governed this ADR:

1. **The single-FIFO controller path is proven.** With exactly one Pipes
   device (`Pipes/pipe1`, `GCPadNew.ini` `Device = Pipe/0/pipe1`),
   `TestRealDolphinBridge` — including the `ProveInput` START control-vs-input
   proof — passes every time (commit d8394f3, reproduced repeatedly on this
   host).
2. **A second hotkey FIFO was not.** Four real runs (~4 h) went into making a
   second Pipes FIFO (`Pipes/hotkeys`) coexist with the controller FIFO so
   that savestate hotkeys could ride it. START over the game pad is still not
   reliably proven with two pipes in place: the control-vs-input window is
   repeatedly disturbed by the attract-movie loop, and the runs did not reach
   a repeatable round-trip (save X → advance to Y → load → X). The drive
   checkpoint calibration (below) was measured on the single-pipe path.

Host: llvmpipe dev box, render window 640x480 on a 1280x1024 private Xvfb,
Wind Waker image (user-supplied, referenced read-only, identified by hash only).

## Decision

- **The default launch is the proven single-pipe path.** `LaunchSpec.Hotkeys`
  defaults to **false**. When false the bridge writes exactly the d8394f3
  layout: one FIFO (`Pipes/pipe1`), `GCPadNew.ini` carrying
  `Device = Pipe/0/pipe1` (the raw-directory-order index resolution is kept —
  with `pipe1` the only FIFO it resolves to exactly that string), **no**
  `Config/Hotkeys.ini`, and **no** `[General] HotkeysRequireFocus` (the key is
  untouched). The temporary `GN_NO_HOTKEY_FIFO` env switch that carried the
  single-pipe experiment is removed; the flag on the spec is the only
  mechanism.
- **Savestate hotkeys are opt-in and unproven.** `LaunchSpec.Hotkeys=true`
  creates the second FIFO, measures its device string from the raw readdir
  order, writes `Config/Hotkeys.ini` (slot 1-4 save/load bindings) and
  `[General] HotkeysRequireFocus = False`. `SaveState`/`LoadState` refuse with
  the typed error `ErrHotkeysDisabled` when the launch did not opt in. The
  raw-directory-order index resolution code is kept for the opt-in path — it
  is correct per the measured raw order (below) — but it is not exercised by
  the default tests.
- **`TestRealDriveToFirstDialogue` runs the drive on the default path.** The
  savestate round-trip phases are gated on `LaunchSpec.Hotkeys`; on the
  default path they are logged `NOT RUN` and the test's promise is the drive
  itself (first in-game dialogue box with the structural `BoxPresent`
  checkpoint and a `TextboxPresent` reference read). The exit-rewrite
  comparison runs against every device line the launch actually wrote
  (`GCPadNew.ini` always; `Hotkeys.ini` only on the opt-in path).
- **Savestate evidence is carried in the BLOCKED register**, not presented as
  pass: `docs/status/blocked.md` row "Dolphin savestates (hotkeys over a
  second Pipes FIFO)".

## The two-pipe investigation findings (recorded, unproven)

These are the measured facts the next attempt inherits; none of them made the
two-pipe path reliable in the time budget spent:

- **Raw device order is `pipe1,hotkeys`.** 2609's `Pipes::PopulateDevices`
  numbers devices by *unsorted* directory-enumeration position
  (`ScanDirectoryTree` over `std::filesystem::directory_iterator`), so the
  device strings are computed from the directory's raw readdir order, never
  assumed. Measured on this host: `pipe1` is index 0
  (`Pipe/0/pipe1`), `hotkeys` is index 1 (`Pipe/1/hotkeys`), and the order is
  stable across repeated reads of the same directory. A single-FIFO directory
  resolves to exactly the d8394f3 string `Pipe/0/pipe1`.
- **The exit rewrite is the structural proof.** A clean Dolphin exit rewrites
  `Config/GCPadNew.ini` (and `Config/Hotkeys.ini` on the opt-in path) with the
  device strings the parser *resolved*; an unresolvable `Device =` line is
  dropped silently. The bridge therefore re-verifies after every Stop that the
  measured strings survived (mismatch = wrong pre-boot index = inputs silently
  ignored, logged at error level with the rewritten file). On the proven
  single-pipe path this check has matched every time.
- **`HotkeysRequireFocus` gates the savestate groups.** The 2609 default for
  `[General] HotkeysRequireFocus` is `True`, and the Load/Save State hotkey
  groups are focus-gated (`HotkeyManager.cpp s_groups_info`); the hotkey
  scheduler's input gate then demands window focus, which the unfocused Xvfb
  render window never gets. `HotkeysRequireFocus = False` is therefore written
  only on the opt-in path — the proven single-pipe config leaves the key
  untouched. This removes one failure mode; it did not by itself make the
  two-pipe START proof reliable.
- **Result:** with both pipes in place the runs could not produce a repeatable
  `TestRealDolphinBridge` (ProveInput control window repeatedly hit the
  attract movie) or a savestate round trip, in ~4 h of real runs. The
  savestate hotkey path is therefore not claimed proven by anything in the
  repo; `docs/status/blocked.md` carries it.

## Checkpoint calibration numbers (drive-to-dialogue, measured)

Calibrated on this host's captured evidence at the checkpoint's 320x240
working size; every number is measured, not guessed (the per-step evidence
PNGs + hashes are the record the calibration loop reads):

- **Title orbit:** moves 0.09–0.25 RMSE between samples (spike band 0.11–0.19
  between 1 s samples) while settled UI screens move 0.02–0.04 — so the
  scene-change floor `SceneChanged(0.18)` separates "a different screen" from
  "the same screen's animation".
- **Memory-card prompt** (post-START "create a save file in Slot A?"): a
  bright full-frame parchment panel over a MOVING sky — letterbox band 0.317
  (spike: 0.345) and 0.115 drift between 2 s samples; it never settles, so
  `Settled()` is the wrong predicate. Its lower third is bare sky
  (bright 0.031 / dark 0.896 / contrast 35.8) vs the title's bright logo text
  (bright 0.233–0.337) — `PromptCard()` is that measured triple and cannot fire
  on the title or the letterboxed movie.
- **First dialogue box:** a bright parchment band with dark glyph ink at
  x 0.31–0.75, y 0.69–0.84 of the frame. Box frames: band mean 128–137,
  horizontal-edge fraction 0.083–0.14 (|dluma| ≥ 30), full-frame luma entropy
  0.44–0.63; title/prompt/panel screens carry the same bright band with
  0.000–0.05 edge fraction; the opening letter scroll matches the edge
  signature (0.084–0.122) but its full-frame entropy is 0.84–0.87 — the
  entropy clause (max 0.70) is what rejects it. `BoxPresent()` = band mean ≥
  100 AND edge fraction ≥ 0.06 AND entropy ≤ 0.70. The aggregate
  `TextboxPresent` lower-third stats (bright ≥ 0.12 at luma ≥ 190, dark ≥
  0.03 at luma ≤ 90, contrast ≥ 35) cannot separate the boxless boat scene
  from the box, so the end assertion is structural (`BoxPresent`) with
  `TextboxPresent` logged as reference only.
- **Script timing:** 2 s poll cadence; step budgets boot-to-title 120 s,
  START/prompt/name-entry 60 s each (A+START pressed together — a lone A
  press on the post-START screen changed nothing), opening-observe 60 s,
  opening-skip 90 s (up to 8 A presses fast-forward the scroll), final
  first-dialogue-box 120 s with short (600 ms) stick nudges (a long hold in
  the boat scene walks the character away and dismisses the box).

## Alternatives considered

- **Ship the drive on the two-pipe default** — rejected: the two-pipe path is
  exactly the unproven one (finding above); the drive's evidence would be
  attributed to an input path that cannot be shown to carry it.
- **Drop the hotkey machinery entirely** — rejected: the measured raw-order /
  exit-rewrite / focus-gate facts are real progress and the code is small;
  keeping it opt-in costs nothing on the default path (no second FIFO, no
  config delta, no extra process) and leaves the unblock path one flag away.
- **Keep the env-var switch (`GN_NO_HOTKEY_FIFO`)** — rejected: an
  undocumented global that silently reshapes the device layout is a
  configuration surface the spec (typed `LaunchSpec`) should not have.

## Consequences and risks

- `TestRealDolphinBridge` and `TestRealDriveToFirstDialogue` now launch the
  proven single-pipe path by default; `SaveState`/`LoadState` are dead on it
  (typed `ErrHotkeysDisabled`), so no caller can accidentally treat a missing
  FIFO as a working savestate.
- The opt-in path remains structurally checked (unit tests assert both FIFOs,
  the measured `Hotkeys.ini`, the focus gate, and the exit-rewrite
  comparison) but has no real-hardware green claim — that is the BLOCKED row.
- A future ADR supersedes this one if the two-pipe path is proven; this
  ADR's raw-order, exit-rewrite and focus-gate findings survive as the
  measurement record.

## Evidence and test plan

- Unit (no emulator): `TestWriteUserConfigDefaultSinglePipe` (default =
  exactly the d8394f3 layout: one FIFO, no Hotkeys.ini, no focus gate,
  `Device = Pipe/0/pipe1`), `TestWriteUserConfigHotkeyFIFO` +
  `TestLaunchHotkeysOptIn` (opt-in = both FIFOs, measured device lines, focus
  gate), `TestDolphinIniHotkeyGate` (gate written only when opted in),
  `TestHotkeysIni`, `TestPipesDeviceOrder`, `TestStateSlotPath`; `go test
  -race -skip Real ./internal/dolphin/ ./internal/api/`.
- Real (foreground, one Dolphin at a time, `-timeout 900s`):
  `TestRealDolphinBridge` (boot, first game frame, ProveInput START, live
  frame pipeline, no process leftovers) and `TestRealDriveToFirstDialogue`
  (the seven-step drive, per-step timings, `BoxPresent` + `TextboxPresent` on
  a fresh final screenshot, evidence PNGs under
  `$GAME_NARRATOR_EVIDENCE_OUT`, GCPadNew exit-rewrite match) — both on the
  default single-pipe path.
- BLOCKED row (next steps for savestates): reproduce the two-pipe launch
  with the focus gate; bound the attract-movie confounder in ProveInput
  (control window must land on a title episode, not the movie) before
  trusting the control dispersion; then re-attempt the round trip
  (save → advance → load, rmse(load-X, X) ≤ the control threshold) with the
  evidence logged. Until it is proven, savestates stay opt-in and the
  register row stays open.

## Migration / rollback

- No schema, wire-format or public API change: `LaunchSpec.Hotkeys` is a new
  field defaulting to false (zero value = the old proven behaviour), and
  `ErrHotkeysDisabled` is a new sentinel. Rollback = revert the commit; the
  on-disk layout written by the default path is byte-identical to d8394f3, so
  nothing persisted needs migrating.
- Any existing isolated user dir from the env-var experiment (a leftover
  `Pipes/hotkeys` FIFO) is inert on the default path: the default launch
  measures the raw order of whatever is there and binds `pipe1` by its
  position, and the exit-rewrite check verifies the bound string survived.

## Follow-ups

- Unblock the two-pipe savestate path (BLOCKED row) or ADR-replace this
  decision with the proven mechanism.
- If the drive's final step proves flaky across boots, tune the
  first-dialogue-box step against its per-step evidence (short holds, low
  repeat cap — the boat-scene walk-away is the measured failure mode).

## Amendment (2026-10-17): screen-specific gates, the corrected Yes-sequence, loop detection, and the measured stop point

The M6 drive-to-first-dialogue was re-calibrated against the retained real
runs (run5/6/7) plus new discovery runs on this host. Every number below is
measured at the 320x240 working size, 2 s cadence; no game text or
screenshots are stored in the repo (the evidence PNGs live under the
`GAME_NARRATOR_EVIDENCE_OUT` dir named by the test trace).

**The false positive this amendment removes.** The old advance steps used a
"the scene changed" gate (`SceneChanged(0.18)`), so a step passed on ANY
screen change — including the wrong one. And the old input sequence pressed
A+START on the memory-card prompt, whose cursor starts on the RIGHT option
(No): it selected No and looped between the two Yes/No prompts, while the
"changed" gate kept passing. The drive never reached name entry.

**The corrected input sequence (the task's M6 fix).** The post-START prompt
is a Yes/No question, cursor on No (right). The drive now: (1) moves the
cursor to Yes with a D_LEFT press/release, gated on the cursor being OFF No
(the measured side or ambiguous); (2) presses A to confirm Yes. The old
false-positive steps (`advance-to-name-entry`, `confirm-character-name`,
both A+START with a `SceneChanged` gate) are replaced by screen-specific
steps.

**Screen-specific gates (measured classes, ADR-0043 signature).** Each step
now gates on a specific measured screen class, not "changed":

- `title` — full-frame scene, lower-third logo bright 0.233-0.337 (measured
  0.414); camera orbit moves 0.09-0.25 between samples.
- `prompt` — the memory-card Yes/No parchment: bright full-frame panel over a
  moving sky (letterbox band 0.29-0.49; never settles), lower third bare sky
  (bright 0.02-0.09) vs the title's logo (0.20+). The Yes/No cursor side is
  the near-white (≥220) blob in the left/right option windows; because the
  panel itself is near-white, the side test is AMBIGUOUS on some frames
  (measured left/right blob 0.024/0.043 after D_LEFT — the 2× ratio fails,
  side = none). The gate is therefore "cursor OFF No" (fails the negative
  check while the cursor is on No; holds on Yes or ambiguous), and the loop
  guard is the backstop. The retained prompt measures cursor on No
  (right.blob 0.024-0.027 > left.blob 0.002).
- `dialog` — the system DIALOG: a bright centre panel over a dark background
  (the save notices, the reading state). Measured full-frame mean 85-90,
  lower-third dark 0.84-0.89, centre-panel (mid) mean 160. Split from the
  overall-dark screen (`dark`, mean 32-72, centre mean 28-52) on the mid
  region (≥130 = dialog). Deliberately not the prompt (bright), not the
  file-select (bright hint bar).
- `filesel` — the save-file slot grid + bright bottom hint bar (hint-bar
  bright 0.941) with panel ink below the name-entry grid's.
- `nameent` — the keyboard grid's dense letter strokes (panel ink ≥ the grid
  threshold) + bright hint bar. Measured mean 155-158, hint-bar bright 0.941,
  top-right bright 0.60, box band smooth (edges 0.000).
- `box` — the first dialogue box: a bright parchment band with dark glyph ink
  at x 0.31-0.75, y 0.69-0.84 (band mean 128-137, edge fraction 0.083-0.14,
  full-frame entropy 0.44-0.63); the entropy clause (max 0.70) rejects the
  letter-scroll (0.84-0.87).

**Loop detection (fail fast, never pass).** The runner arms a loop guard on
every step that STARTS on a prompt. After the step's input, if the screen
stays on the same prompt (same signature key: class + cursor side) for three
consecutive samples, the step fails fast with `loop detected` — the press
left the same prompt (the Yes/No loop), so it must never pass. The guard is
prompt-only on purpose: a stable-class screen that legitimately changes
content while keeping its class (the opening letter's page turns) must not
read as a loop. Unit-tested on synthetic prompt/box doubles
(`TestLoopGuardPure`, `TestRunScriptPromptLoopFailsFast`,
`TestRunScriptPromptNoLoopOnAdvance`).

**Measured stop point (this host, confirmed by the real run).** The real
`TestRealDriveToFirstDialogue` run (foreground, default single-pipe path,
per-step `ok/elapsed/signature` logged) reaches the memory-card prompt
(`prompt:no`), moves the cursor off No (`D_LEFT` → cursor side off No / ambiguous),
presses A (Yes, 3 attempts), and lands on the save-creation DIALOG
(`dialog`: mean 85-90, centre mean 155-164). It then STOPS there: step
`advance-to-file-select` (5 A presses, 120 s budget) never leaves the dialog,
and the GC save file is NOT created — the isolated Dolphin user dir holds no
`MemoryCards` entry after the run, and the dialog does not resolve to the
file-select (A/START/D-pad on it do not advance it across the full poll;
the per-step signature trace shows `dialog` at the stop). The drive therefore
reports the furthest correctly-identified screen as the save-creation dialog
and the cause as "the save file was not created (the GC memory card was not
persisted by this Dolphin setup)", with the evidence PNG path in the test
trace. The retained run5/6/7 frames confirm the rest of the chain is
reachable on the No path (prompt-No → overall-dark → `nameent` → `dialog`
reading state) but the box was never reached: the opening monologue (the
`dialog` reading state) did not advance to the box under A or stick in any
retained run.

**Consequences.** The drive's promise on this host is now: reach and
identify the memory-card prompt, confirm Yes, and reach the save-creation
dialog — reporting the exact stop point and cause when the save file is not
created. The `advance-to-file-select` step (and the steps after it) are the
calibration target once the memory-card persistence is unblocked (a BLOCKED
row); their gates are already screen-specific so they will not false-pass.

## Amendment 3 (2026-10-17): the main-stick cursor fix, the measured quest-log file-selection class, and the corrected full drive

The previous real runs (the recorded run2 failure + the probe matrix) proved
the cursor fix the drive needed, and one discovery walk (probe8) mapped the
full no-card path to a NEW screen (the quest-log "File Selection" list) that
was previously unclassified (`unknown`). Every number below is measured at
the 320x240 working size, 2 s cadence, on this host (the retained evidence
PNGs + per-frame sigdumps are the record; no game text or screenshots in the
repo).

**The d-pad does not move the WOTW Yes/No cursor (the loop cause).** The
probe matrix on the continue-anyway dialog (one fresh boot, one input per
probe, each followed by a measured screenshot): D_LEFT x3 → `continue:no`
(unchanged); D_RIGHT-then-D_LEFT → `continue:no`; L → class flickered (no
cursor move); R → `continue:no`; C-stick left/right → `continue:no`. The
MAIN STICK is the one that moves it: `SET MAIN 0 0.5` (MainX=-1, ~200-900 ms
hold, then neutral) → `continue:yes` (measured left.blob 0.0262-0.0296,
right.blob 0.0000, cursor side = yes; the cursor's measured centroid moves
from cx 0.559-0.616 (No, right) to cx 0.44-0.445 (Yes, left)). On the bright
create-save prompt the same d-pad failure is recorded by the real runs:
three D_LEFT attempts (the old `move-cursor-to-yes` step) ended on
`prompt:no` with the cursor side still reading No/ambiguous — the old
"off-No" gate passed on the ambiguous `cursor=-` (the false positive this
amendment removes), the confirming A then selected No, and the drive looped
between the two prompts. The move-cursor steps now use the main stick
(`Stick{X:-1, Y:0}`, 200 ms) and gate on the STRICT cursor-on-Yes
predicates (`MemoryCardPromptYes` / `ContinuePromptYes`); `CursorNotNo` is
kept as a documented lenient predicate but the drive no longer uses it.
The create-save parchment prompt is the same Yes/No widget as the dark
continue dialog (the side test is the shared near-white blob 2x-ratio test);
the stick move is measured on the continue dialog and is the same input
shape on the parchment prompt (the real run verifies it — if it does not
register there, the loop guard fails the step fast, which is the required
behaviour).

**The measured no-card path (probe8, the full chain).**
title → START → `prompt:no` (the bright create-save prompt; mean 125,
letterbox 0.476, right.blob 0.0172 cx 0.616 = cursor on No) → A (confirms
No) → `continue:no` (mean 101.6, mid 160, right.blob 0.0212-0.0238 cx
0.559-0.560) → main-stick-left → `continue:yes` (mean 97.7, left.blob 0.0296
cx 0.445, right.blob 0.000) → A → `dialog` (the "no save file" info: mean
81.9, mid 131, blob 0.000 both sides, text edges 0.039) → A → fade → the
QUEST-LOG FILE SELECTION screen (below). A on the info dialog advances it to
the file selection (measured); A/START on the file selection itself do NOT
(see below).

**The quest-log "File Selection" class (new `filesel`).** The 3-slot save
list (the "File Selection" screen). Measured on its settled frames (16
frames over ~30 s, the screen is static): mean 95-106, entropy 0.74-0.84,
letterbox 0.15-0.22, lower-third bright 0.00-0.034, box-band window
(x 0.31-0.75, y 0.69-0.84) mean 139-144 with edges 0.043-0.049 (the slot
panel), top-right bright 0.044, bottom bar bright 0.000, mid mean 140. Its
confusable is the blobless info dialog (same dark base): the dialog measures
mean 82-100, entropy 0.52-0.68, box-band mean 65, box-band edges 0.000. The
measured separators are therefore the ENTROPY clause (filesel 0.74-0.84 vs
dialog 0.52-0.68; threshold 0.71) and the BOX-BAND-MEAN clause (filesel
139-144 vs dialog 65, overall-dark 28-52; threshold 100). Two findings that
shaped the class: (a) the screen does NOT enter the dark family — its
near-black lower third is diluted by the mid-luma slot panel (the lower-
third dark fraction sits at/below the 0.85 entry floor, so the class is
checked as its own branch after the dark family, not inside it); (b) the
slot icons' near-white appears only on the first 2-3 transition frames
(left+right blob 0.024-0.092) and is 0.000 on the settled frames — the
BLOB IS NOT A USABLE CLASS CLAUSE (an earlier calibration that used it was
wrong; it would have failed on every settled frame). The class also requires
mean < 110 (the dark cap — nameent is 155-159), lower-third bright < 0.10
and bottom-bar bright < 0.80 (excludes the hint-bar screens). Synthetic
unit tests pin the class + the confusable negatives
(`TestFileSelectClass`).

**What advances the file selection (measured).** A alone: no-op (16 A
presses over ~30 s, the screen's full signature unchanged). START alone:
no-op (the same walk's START presses). A+START together: the only measured
advance — runs 5/6/7 crossed the screen to the name entry with the
simultaneous A+START press (their `advance-to-name-entry` /
`confirm-character-name` steps). The `choose-file` step therefore presses
{A, START} together. The character name is confirmed the same way
(A+START; the A-only presses in runs 5/6/7 left the name entry in place).

**The slow name-entry fade (loop-guard hazard, measured).** After the
A+START name confirm, the screen fades out over ~30-50 s and the EARLY fade
frames keep the nameent class (runs 5/6/7: the first ~3-5 poll frames after
the confirm still measure mean 155-159, class nameent) before dimming to
the continue-anyway dialog. The loop guard is therefore NOT armed on
nameent (it would fire on the legitimate slow fade), and the name-confirm
gate never includes NameEntry (the step must LEAVE the name entry to pass;
a no-op burns the 120 s budget and fails honestly). The guard IS armed on
the `filesel` key (a stable menu with no side in its key: a press that
leaves it unchanged is a loop — measured: A/START alone do nothing there).

**The corrected drive (the task's M6 fix).** Boot → title → START →
`prompt:no` → main-stick-left (strict `prompt:yes` gate) → A → the screen
resolves to the save-creation notice (`dialog`) on a working Slot A card
(then the file selection / name entry) or to the continue-anyway dialog
when the save cannot be created. Each "advance" step's gate lists the
measured screens its input can land on (including the continue-anyway
dialog, which any save attempt can raise), and a continue-anyway HANDLER
PAIR (main-stick-left with the strict `continue:yes` gate, then A) follows
every advance step that can raise it; the handlers are SkipUnless-gated to
their own screen so they never fire on another screen. Both paths
(working card, no card) reach the opening → the first in-game dialogue box.
The YES path (the create-save notice → file selection with the new file) is
the untested half — the card's usability has never been measured with a Yes
answer (that is the point of the run); the no-card path is fully measured
above.

**Consequences.** The `move-cursor` steps are main-stick inputs with strict
cursor-on-Yes gates (the ambiguous-cursor false positive is gone); the
`filesel` screen is identified by its measured entropy + box-band clauses
(not the blob); the drive's file-selection and name-confirm steps press
A+START together; the loop guard arms on both prompt classes and the
stable `filesel` menu. `TestRealProbeContinueDialog` (temporary, deleted
before reporting) is the calibration aid that produced the probe matrix and
the no-card-path walk.

## Amendment 4 (2026-10-17): the create-save prompt is the dark dialog family, the dim title is the title, and the measured false pass

The 2026-10-17 real `TestRealDriveToFirstDialogue` run (evidence under the
run's `$GAME_NARRATOR_EVIDENCE_OUT`) stopped at the move-cursor step after
62 s, and the retained per-frame sigdumps of that run plus the probe8
evidence proved that the stop was a classifier false pass, not a missing
input. Every number below is measured at the 320x240 working size on this
host (the retained evidence PNGs + per-frame sigdumps are the record).

**The false pass (the run's root cause).** Step 2
(`start-to-memory-card-prompt`) "passed" 4.7 s after boot-to-title: its gate
(the old `MemoryCardPrompt` = the bright `prompt` class) held on a frame
that was the TITLE'S DIM "press START" PHASE, not the memory-card prompt.
The START press sent 1 s later landed on the title's BRIGHT LOGO PHASE and
was IGNORED there — the screen stayed on the title for the whole 62 s
move-cursor step (all 27 retained step-3 frames are title statistics: mean
90-135, letterbox 0.28-0.56, the hint/botbar mean 71-126), the main-stick
inputs did nothing on the title, and the step burned its budget (the loop
guard never armed: the step-start ref was the dim title, not a prompt key).

**The title's two measured phases.** Phase A (bright logo): mean 167-168,
lower-third bright 0.487-0.492, centre-panel (mid) mean 203-204, bottom
bar mean 172-175 (frames 00-final / 01-probe-000 / p00). Phase B (dim,
"press START"): mean 90-126 (flicker band dips to 90.4), letterbox
0.28-0.56, lower-third bright 0.00-0.12, centre-panel mid mean 100-132,
bottom bar mean 71-126 (frames p01, 01-final, 01-probe-002, and the step-3
flicker band). The OLD prompt clause (letterbox > 0.25, lower-third bright
< 0.10, text-window mean >= 110) matched phase B exactly — that is how the
dim title read as `prompt:no` (the "right.blob 0.0172 cx 0.616" the
amendment-3 record had logged as the prompt's cursor on No is the title's
ship/sail glow in the right option window, present on p01 too). The
measured separators from the dark dialog family are the CENTRE-PANEL MEAN
(title B mid 100-132 vs the prompt/dialog mid 160 — the 38+ margin) and
the LETTERBOX band (title B 0.28-0.56 vs the quest-log list 0.15-0.22):
the title's dim phase is now the TITLE class (a new clause before the
prompt check: letterbox >= 0.25, mean 90-130, lower-third bright < 0.10,
mid < 130), and the quest-log class gained the letterbox < 0.25 clause
(the step-3 flicker frames with mean 97-107, entropy 0.74-0.81 and box
band 102-115 that leaked into `filesel` under the old clause). The bright
`prompt` class now matches no retained real screen (only the synthetic
doubles the loop tests rely on); a note says so at the clause.

**The create-save prompt is the DARK dialog family (ClassContinue).** The
probe8 evidence re-read with the corrected classifier: p02/p03 (right
after START on the title) are the create-save "new save file in Slot A?"
Yes/No screen — mean 99.7-101.6, lower-third bright 0.000, centre-panel
mid mean 160, botbar 71-73, the near-white cursor blob 0.0212-0.0238 on
the RIGHT option (cx 0.559-0.560), 0.000 on the other → `continue:no`.
The main-stick-left 900 ms input moves the cursor to the LEFT option
(p04: left.blob 0.0296 cx 0.445, right.blob 0.000 → `continue:yes`), and
A on Yes resolves to the blobless "no save file" info dialog (p05: mean
81.9, mid 131, blob 0.000 → `dialog`) — the same measured chain amendment 3
logged as "prompt:no → continue:no → continue:yes → dialog", with the
labels corrected: what amendment 3 recorded as the "bright create-save
prompt" (p01, mean 125, lb 0.476) was the dim title's phase B (the
probe's pre-START reference frame, retained as p01-TITLE.png), and what it
recorded as "continue:no" (p02) is the real create-save prompt. The
amendment-3 input facts survive unchanged: the d-pad does not move the
WOTW Yes/No cursor, the MAIN STICK does, A+START advances the quest-log
file selection, A+START confirms the name.

**The measured START-registration fact.** START on the title registers on
phase B and is ignored on phase A (the only two measured observations:
phase B at the probe8 START → registered; phase A at the 2026-10-17 run's
START → ignored, the title persisting). The phase cycle itself (A→B
timing, whether B persists or re-cycles, the hint's on/off period) is NOT
yet measured — the `TestRealProbeTitlePhase` calibration aid (temporary,
deleted before reporting) samples the title at 1 s cadence for 30 s to map
it. The drive's START step therefore repeats the press (MaxRepeats 5, 90 s
budget, each attempt re-pressing START): a press that lands on phase A is
retried until one lands on phase B and the prompt appears. The gate is the
measured `continue:no` screen, so a title repeat (any phase) cannot pass
it — the step either reaches the prompt or fails its budget with a
screen-specific trace (the honest-failure property the false pass broke).

**The gates (the corrected drive).** `MemoryCardPrompt` = class continue
with the cursor measured on No (the measured entry state, p02/p03);
`MemoryCardPromptYes` = class continue with the cursor measured on Yes
(the strict pre-A gate, p04). Both the create-save prompt and the
save-failure continue-anyway dialog are the `continue` class, so the
move-cursor handler steps (the existing `ContinuePromptYes` gate) are
shared by both by construction. The move-cursor step's stick hold is 900 ms
(the measured move — p04 after the 900 ms input; the 200 ms hold is
measured NOT to move the cursor: the 2026-10-17 run's four 200 ms
attempts left the cursor where it was), and the loop guard arms on the
`continue:no` ref so a no-move fails the step fast.

**Consequences.** The dim title is the title class (the boot-to-title gate
now holds on either phase); the START gate cannot pass on the title (the
2026-10-17 false pass is structurally impossible: the gate requires the
dark `continue` screen, which the title never measures); the create-save
prompt is identified by its measured class + cursor side, the same as the
continue-anyway dialog it is the same measured shape as. Residual: some
title flicker frames still read `prompt`/`box`/`scene` (the title's
flicker band exceeds the dim-phase clause's mean/mid caps on the bright
end, e.g. mean 134.7 mid 123, or l3.bright 0.12) — those cannot pass any
drive gate (the gates require `continue` + side, which no measured title
frame reaches: the dark-family entry needs l3.dark > 0.85 AND mid >= 130
AND a blob, and no measured title frame has that combination), so they
stay a trace-cosmetic issue, not a false-pass vector.

## Amendment 5 (2026-10-17): the screen-driven drive, the measured name-entry class, and the blink-off prompt

The 2026-10-17 linear `WindWakerNewGameScript` drive (amendments 2-4) reached
the name-entry screen and then sat there for 166 s: the linear step list could
not represent the measured flow, so its steps "passed" pre-satisfied or
skipped (the false-positive structure the spec forbids) and the opening-skip
step pressed A repeatedly on the name entry, typing "AAAAAA" (the retained
frames show the name field filling with A's) without ever leaving the screen.
Amendment 5 replaces the linear drive with a SCREEN-DRIVEN LOOP and adds the
two measured classifier clauses the loop needs: the name entry's own class
and the blink-off prompt's discriminator. Every number below is measured at
the 320x240 working size on this host; the retained evidence PNGs (the
2026-10-17 drive's per-frame probes and the probe walk) are the record.

**The screen-driven loop (the M6 drive).** The drive is now a loop, not a
step list. Each iteration: (1) classify the CURRENT frame into one of the
measured screen classes (`title`, `prompt` with a measured cursor side,
`save_notice`/blobless dialog, `file_select`, `name_entry`, `name_confirm`,
`cutscene`/letterbox, `dialogue_box`, `unknown`); (2) send the ONE action
that screen's class prescribes (or wait, for wait-only screens); (3) let the
screen settle; (4) re-classify, logging `(screen, side) --(action)--> next`
with the frame's measured signature line. There are no pre-satisfied steps
and no skipped steps: the action always comes from the screen actually on
screen. Termination: the `dialogue_box` class (success); the SAME screen key
(class:side) on >= 4 consecutive iterations with no visible change — max
frame RMSE < 0.02 (a static screen renders within ~0.005 of its own previous
sample; a cursor blink moves ~0.03+; a text advance 0.05+ — so an animated
cutscene or a blinking prompt never reads as a loop, while a dead screen
fails fast) -> "loop detected"; or the iteration cap (90) / time budget
(15 min) -> failure with the FULL transition log and the last retained PNG.

**The name entry is its own class (measured).** The old `nameent` clause
(bright bottom hint bar, `botbar.bright >= 0.80` and `text.edges >= 0.10`)
matched only the name entry's BRIGHT frame-pulse phase — the 2026-10-17 run
read the dim phase as `filesel`/`dark` and the bright phase as `title`. The
keyboard grid's window (x 0.20-0.80, y 0.40-0.75) is measured STABLE across
the screen's frame pulse on the 110+ retained frames: `namegrid` edges
0.066-0.080, mean 120-130 (dim phase: full-frame mean 91-92; bright phase:
142.8). Its confusables differ measurably: the blobless save-notice dialog
measures `namegrid` edges 0.021 (a single sparse text line), the create-save
prompt's dense text panel measures grid mean 148-150 (above the ceiling),
the quest-log file selection measures grid mean 143 (above the ceiling), and
the title->prompt FADE frame measures letterbox 0.428 (the screen itself
measures 0.166-0.173 in both pulse phases). The new clause (checked BEFORE
filesel, since the quest-log clause's measured frames were, it turns out,
this screen mislabelled): `namegrid.edges >= 0.055`, `namegrid.mean` in
[110, 140], and `letterbox < 0.30`. The name entry's action is START (the
cursor moves to the END button) then A — ONE compound action; NEVER A alone
(A alone types the letter under the cursor into the name — the 2026-10-17
run's nine A presses typed "AAAAAA" and never left the screen, measured).
The dimmest pulse frames can enter the dark family (measured: mean 91.3,
l3.dark > 0.85 -> `dark` -> cutscene -> wait): wait is safe on this screen
and the pulse returns the drive to the nameent class within an iteration.

**The blink-off prompt (the drive's remaining false-positive vector).** The
WOTW Yes/No cursor BLINKS (on/off at ~1 s: the near-white blob measures
0.027-0.040 on the cursor side when on, 0.000-0.002 when off). On a
blink-OFF frame the prompt is a blobless dark-family dialog — and the drive
's save-notice action is A, which on a blink-off create-save prompt (cursor
on No) would SELECT NO, the no-save path. The blink-off frame is still
recognisable by its dense text panel: the measured prompts carry centre-
panel mean 155-164 and text-window edges 0.095-0.134 (10+ retained frames of
both measured prompts, including the Q88 run's screen that ignored A for
100+ s: mid 155, text edges 0.096), where the blobless save notices are a
small panel (centre-panel mean 130-131, text edges 0.039 — one or two sparse
lines). The new clause inside the dark family's dialog branch: `mid.mean >=
145` AND `text.edges >= 0.07` -> `continue` with the side UNKNOWN (the drive
waits for the measured side — it never guesses A onto an unmeasured side);
after 3 consecutive no-side iterations it sends the measured main-stick-left
move (the measured cursor input on both measured prompts; from a Yes cursor
it either stays or wraps back to No, so the next measured side converges on
Yes-A either way). Both clauses carry margin on both sides (155-164 vs 145
vs 130-131; 0.095-0.134 vs 0.07 vs 0.039).

**The measured save-failure flow (the run the drive must survive).** The
2026-10-17 run's retained frames decode the flow with the (raw, currently
not-writable) memory card: title -> START -> the create-save prompt (cursor
on NO, blinking) -> main-stick-left -> cursor YES -> A -> the SAVE-FAILURE
continue-anyway prompt (cursor on YES by default, measured: the ref frame
before any input already shows the left blob) -> A (measured unreliable per
press on this host: 7 misses, the 8th registered) -> a small blobless notice
(mean 73.5, mid 130, `namegrid` edges 0.021, text edges 0.039) -> the name
entry. The A-unreliability is exactly what the linear script could not
represent (its fixed steps burned out on the prompt); the loop represents it
by construction: the same screen prescribes the same action, the retry is
bounded by the iteration cap, and a screen that truly ignores the input
fails the loop detector fast. With a WORKING card the same flow shows the
save-creation notice (no continue-anyway prompt) and the card file is
written (the amendment-2 exit checks assert the non-zero size).

**The opening after naming.** The opening plays (story scroll / cutscenes):
the drive advances text with A ONLY while a text box (a glyph-inked
lower-third band, `boxband` mean >= 100 and edges >= 0.06 — the box-band
clauses WITHOUT the entropy clause, which BoxPresent keeps to reject the
letter scroll; the letter IS a screen whose text A must advance) is on
screen; otherwise it waits. The loop stops at the first `dialogue_box`
class. The name-confirm screen ("is this name OK?") is the same measured
prompt shape as the create-save prompt; a Yes/No prompt seen after the name
entry is labelled `name_confirm` in the transition log and handled by the
prompt action (cursor to Yes via the main stick, then A).

**Evidence contract.** The real test prints the full transition log, one
line per iteration (`step NN: screen:side --(action)--> next | signature`),
and on failure reports the farthest correctly identified screen, the reason
(loop detected / budget exhausted / the measured stop), and the evidence
directory with the per-iteration PNGs and the last frame — the honest-
failure property the linear run's pre-satisfied steps broke.

## Amendment 6 (2026-10-18): the opening legend's narration subtitle class, the drive's success condition, and the ProveInput settled-clause fix

The 2026-10-17 screen-driven re-run reached the opening and then burned
the rest of its 90-iteration budget on the OPENING LEGEND itself: its iters
064-089 (the letterboxed storybook panels carrying the narration subtitles)
were mislabelled `name_entry` / `unknown` / `save_notice` — the classifier
had no class for the letterboxed legend, and the name entry's grid-ink
clause measured the legend frames as name entry (measured `namegrid` edges
0.16-0.22, DOUBLE the keyboard's 0.066-0.079, with the grid window's mean
oscillating 110-147 across the nameent clause's [110, 140] band) — so the
drive sent the name-entry action (A+START+A) to the legend for 26+ iterations
and the run ended on the iteration cap without ever reaching a success
screen. The legend is where the first in-game narrated text lives — the
exact text the recognition pipeline must read — so the classifier needed
its own measured class and the drive its own success condition. (Amendment
5's nameent letterbox FLOOR — the dark-gray name-entry bands at
0.156-0.173 vs the legend's pure-black 0.000 — came out of the same run and
keeps the letterboxed frames out of the name entry; this amendment gives
the legend what that floor only excluded: a class, a wait-only action, and
the success condition.)

**The story_subtitle class (measured).** The opening legend is a
LETTERBOXED storybook panel (PURE-BLACK top/bottom bars, measured lb 0.000)
with the white narration subtitle in the bottom black bar — two measured
lines at y 0.78-0.83 and 0.84-0.88, the bright (>=190) text pixels spanning
x 0.155-0.841 across the 19 text-bearing retained frames (the 2026-10-17
re-run's iters 064-089; union bbox y 0.792-0.875). The `subbar` window
(x full, y 0.76-0.90 — BELOW the panel's measured bottom edge at 0.74 so
the panel's art rows at 0.70-0.74 cannot pollute it, and ABOVE the bottom
margin) measures: WITH text — bright 0.032-0.046, edges 0.078-0.118;
WITHOUT text (a fade, or between subtitles) — bright 0.000, edges <= 0.064.
The clause (checked FIRST in ClassOf — the dark family would otherwise
swallow these frames, which measure mean 65-83 and l3.dark > 0.85):
`letterbox < 0.05` (measured 0.000; every non-letterboxed screen measures
0.156+), `subbar.bright >= 0.02` (0.032-0.046 with text, 0.000 without),
`subbar.edges >= 0.05` (0.078-0.118 with text, <= 0.039 without; the
0.064 fade-frame edge outlier is kept out by the bright clause). The
confusables all measure at the no-text end AND carry dark-gray bands:
the name entry (lb 0.156-0.173, subbar bright 0.000 / edges 0.003), the
file selection (lb 0.156-0.173, 0.000 / 0.019-0.024), the dialog over the
title (0.156-0.173, 0.000 / 0.004). The text fades in and out, so the
class is only claimed on a frame that MEASURES the text; a faded or
between-subtitles frame falls through to the cutscene family (`dark` /
`scene` / `unknown`) and the drive waits on it. The measured negative
check on the retained frames: 0 of the 26 legend-span frames classify as
`nameent`, and the cutscene A-gate (`boxband` mean >= 100 AND edges >= 0.06
WITHOUT the entropy clause) fires on 0 of them — the drive presses nothing
while the legend plays.

**The drive's success condition.** The goal is the first in-game narrated
text, whichever measured success screen the loop reaches FIRST: a
`story_subtitle` frame (only claimed with the measured bottom-bar text)
or the full-frame `dialogue_box` — the first reached keeps the credit, and
the run records its evidence PNG (SuccessPath), the success screen, and
the text-presence flag. On a story-subtitle success the run keeps sampling
WAIT-ONLY (nothing is pressed — the legend advances on its own and a press
may skip the narration) until one FULL subtitle run is measured (a text
run closed by a text-less sample) or 30 post-success story-subtitle
iterations pass (~75 s at the 2 s settle cadence; the measured legend runs
40+ s, so the budget covers multiple subtitles and ends on a closed run in
practice).

**The subtitle region (the crop handoff).** The live-session E2E's
textbox-region configuration is the measured bottom-bar crop, normalised
frame fractions: the union bbox of the retained legend frames' bright text
pixels (x 0.155-0.841, y 0.792-0.875) padded ~0.035 per side for the longer
lines and the fade states — `StorySubCrop = {0.12, 0.75, 0.88, 0.92}`
(x0, y0, x1, y1). The real test logs it and the first success frame's PNG
is the handoff evidence.

**The subtitle timing record.** Each narration subtitle's visibility is
recorded as one SubTiming per measured text run, bracketed by the
surrounding text-less samples at the drive's iteration cadence (2 s settle
+ ~0.2 s screenshot, so a span is a multiple of ~2.2 s and a 0 / Open value
means the run started at the first sample / was still up when the window
closed): fade-in, hold (the fully-visible span), fade-out. Measured on the
2026-10-18 run: subtitle 0 = iterations 20-22, fade-in 2.2 s, hold 4.4 s,
fade-out 2.2 s (closed by the iter-23 text-less sample); subtitle 1 =
iter 24, still up when the observation closed (open).

**The measured flow (2026-10-18 run, 25 iterations, 1 m 2 s to the success
frame; 81 s total including boot).** boot (letterbox cutscene, wait) ->
title (START re-sent each iteration; it registers only on the dim
"press START" phase) -> create-save prompt, cursor NO (dark dialog family,
lb 0.167-0.212) -> MAIN-STICK LEFT 900 ms (the measured cursor move; the
d-pad does not drive it) -> cursor YES (measured: left blob 0.0377 vs
right 0.0000) -> A -> save-creation notice (blobless dialog, mid 130,
namegrid edges 0.021) -> A -> name entry (A+START+A: type the letter under
the cursor, START moves the cursor to END, A confirms — A alone NEVER
confirms, it types) -> the quest-log file selection (A+START together —
A alone and START alone are no-ops, twice) -> the timed PURE-BLACK
transition (wait; the retained frames hold 4-cadence samples of 0.0000
change inside the ~28 s black span, which is why the loop detector ARMS
only on non-wait actions and the success screen disarms it) -> the legend:
cutscene (wait) x1-7 -> `story_subtitle` (WAIT-ONLY, nothing pressed) —
success at iteration 20, text measured (subbar bright 0.033-0.053,
edges 0.078-0.175 on the success frames). With a working Slot A card (the
bridge's explicit raw memcard, amendment 2) the save path shows the
save-creation notice and the card file carries the save (measured 16
MiB, non-zero, verified after the exit rewrite).

**The ProveInput settled-clause fix (the real bridge test).**
`TestRealDolphinBridge` failed on ProveInput: the post-START scene — the
dark memory-card dialog — never satisfied the settled crossing clause. The
clause's band test was `lb > letterboxTitleMin` (0.25, "sky in the bands"),
calibrated on the unit double's BRIGHT full-frame settled scene
(`brightNoiseImage`); the measured real screen is a DARK full-frame dialog
whose frame bands measure 0.166-0.212 (the dialog's dark-gray frame over
the scene — below the title's sky/sea bands at 0.32-0.56, above the
opening movie's black at 0.000). The 2026-10-18 failure trace: START
pressed, the prompt settled at lb 0.166 / ent 0.682 / step 0.016 /
d 0.29 vs the nearest control framing — a true input-caused scene change
the clause rejected. The fix: the settled clause is now the canonical
`isSettledScene` predicate — NOT-letterboxed bands (`lb >= letterboxMovieMax`
0.10: the movie's bands are black, measured 0.000, so this excludes the
movie shape), rendered content (`ent > 0.20`: a black fade has none), and
stable against the previous 1 s sample (`step < crossStepMax` 0.10: the
title's orbit moves 0.10-0.40 per sample and never settles) — and the
same predicate is shared with `hasSettledStretch`, the no-input control
window's guard, so a screen the input window accepts as the press's result
also invalidates the episode when it appears WITHOUT input (a screen change
the press did not cause must never be attributed to it). The attribution
basis is unchanged: the settle case needs no distance clause because the
title never settles, verified on the no-input baseline per episode; the
letterbox movie case still requires two consecutive FAR samples. The
2026-10-18 real run after the fix: `two_consecutive_crossed=true`
(input_max 0.5093, control dispersion 0.1913, threshold 0.2957), accepted
on the settled mode. Unit doubles: `TestProveInputSettledScene` keeps the
bright shape (the sky-band case is a special case of the not-letterboxed
floor); the new `TestIsSettledSceneClauses` (the measured real dialog
settles; the movie / black-fade / orbiting-title confusables do not),
`TestClassifyWindowSettledDarkDialog` (the measured real input window
crosses on the settled mode; a stable letterboxed shape sitting close to
the control does NOT cross — the relaxed floor must not leak into the movie
shape), and `TestProveInputDarkDialogSettled` (the full harness on the
real shape: the dark-dialog double measures lb 0.165 / ent 0.881 / d 0.343
against the orbiting title — the same shape the real screen measured) pin
the fix.

**Load-tolerant timing in the runner unit tests.** The five wall-clock-
bounded `RunScript` tests (the repeat cap, the step timeout, the three
loop-fail-fast tests) flipped under concurrent host load (another worker's
e2e suite: 10 ms poll sleeps inflating ~17x). Their structural assertions
(the attempt COUNT, the wire bytes, the "loop detected" failure) were the
point; the tight margins (a 250 ms deadline with a "under 500 ms" bound)
were load-fragile. The caps are now load-tolerant backstops (1-5 s
deadlines, 1.5-5 s elapsed bounds) and the repeat-cap test polls at 2 ms
so three 8 ms windows keep ~25 ms of nominal sleeps: the repeat cap is
asserted by the attempt count, the timing only guards that the deadline is
not the thing that ran out. (The runner waits out the rest of the step
deadline after the attempt budget is spent, so the repeat-cap elapsed
lands just above its 1 s deadline by construction.)

**Consequences.** The M6 drive-to-first-dialogue now terminates on the
first in-game narrated text (the opening legend's narration subtitle —
measured, cropped, and timed — or the dialogue box), with the full
transition log, the first success frame's PNG, the normalised subtitle
crop, and the per-subtitle fade-in/hold/fade-out record as the handoff to
the live-session E2E's textbox-region configuration. Both real tests pass
on this host (2026-10-18: TestRealDriveToFirstDialogue 81 s,
TestRealDolphinBridge 66 s; evidence: the per-iteration PNGs, the
exit-rewritten configs, and the ProveInput before/control/after/end
frames). Reversal: drop `ClassStorySub` and the `isSettledScene` helper
(back to the `lb > letterboxTitleMin` settled clause) — the drive falls
back to the amendment-5 shape (success only on `dialogue_box`, the legend
mislabelled) and the real bridge test falls back to the measured
ProveInput failure.

## Amendment 7 (2026-10-18): approach D — the hotkeys bind to the keyboard device, the XTest injection, the `StateSaves` dir, and the double-restore round trip

**The consumer contention that killed the two-pipe plan.** The prior
attempts bound the Save/Load chord to the controller pipe. With the chord
byte on the controller FIFO, Dolphin's SI polling thread (~120 Hz, reading
`Pipe/0/pipe1`) and the HotkeyScheduler (reading the same FIFO when a chord
is bound to it) are TWO consumers of the SAME Pipe FIFO: the byte is
consumed by whichever thread reads it first, so the chord fires or is eaten
nondeterministically. A second hotkey FIFO (`Pipes/hotkeys`) does not fix
this — the scheduler still polls a FIFO the SI path is not designed to
share, and the two-pipe layout had its own structural problems (the raw
reorder and the exit-rewrite findings recorded above). **Approach D, which
works and is what this amendment records: bind the hotkeys to the
KEYBOARD device instead of any Pipes FIFO.**

**The measured keyboard device string.** 2609's XInput2 backend resolves
the X11 keyboard device to `XInput2/0/Virtual core pointer` — measured
from the exit-rewritten spike user dirs (`artifacts/spikes/dolphin/user/Config/`):
EVERY keyboard controller (the four default mouse/keyboard controllers)
rewrites its `Device =` line to exactly this string, and a clean exit
KEEPS it (unlike an unresolvable Pipes line, which is dropped). The
opt-in `writeUserConfig` now writes, when `Hotkeys: true`:

```ini
[Hotkeys]
Device = XInput2/0/Virtual core pointer
Save State/Save State Slot 1 = `F1`
Load State/Load State Slot 1 = `F2`
```

with `HotkeysRequireFocus = False` under `[General]` (amendment 2).
F1/F2 are outside the game's input vocabulary (the bridge's inject
vocabulary and the drive's step vocabulary both use keysym names like
`A`, `R`, `Space`, `Prior`) and outside the controller chord, so nothing
else consumes them. (The keys are written with a backtick only when the
key differs from the label — the 2609 format; F1/F2 labels already
differ, so backticks.)

**The XTest injection.** The keys are fired with XTest on the Xvfb
display: xdotool `keydown F1` → 150 ms hold → `keyup F1` (the same
hold duration the chord injector used). `Options.XdotoolBin` defaults to
`/usr/bin/xdotool` (xdotool 4.20260303.1 on this host; it is a dev-host
test dependency — docs/dev-environment.md) and the whole send is behind
the `Options.SendKey` seam (fakeable in unit tests; the NOT-RUN guard
lists it with the other binaries). No focus or window targeting is
needed: a plain XTEST event on the display reaches the hotkey scheduler
regardless of focus (measured — the spike's `xdotool key F1` wrote the
slot file on the unfocused Xvfb display), and `HotkeysRequireFocus =
False` keeps the X11 hotkey handler polling. The controller FIFO keeps a
SINGLE consumer (the SI thread), as it always had on the default path —
the single-pipe drive behavior is untouched.

**The `StateSaves` dir.** 2609 writes slot files to
`<user>/StateSaves/GZLE01.sNN` (measured from the spike dirs and the
live save), NOT `states/`. The first keyboard attempts "failed" the save
poll because `stateSlotPath` globbed `states/*.s01` — the hotkey had
fired all along; the file was one directory over. `stateSlotPath` and
the NOT-RUN evidence listing now use `StateSaves/`.

**The round trip as a double restore.** The drive's success screen is the
animated opening legend (amendment 6). A pixel comparison of the load
against X measures the restored timeline's OWN motion, not the load: the
measured 2026-10-18 run (run-drive3) restored the legend correctly —
the after-load frame classified `storysub` with the SAME panel art
(blobs at the same centroids 0.329/0.673, matching mean/std/entropy) —
yet the 3 s timeline offset alone measured 0.21 RMSE against X (the
subtitle text and the animating panel art differ at T vs T+3 s on the
same restored timeline). A no-op load on an animated scene could
therefore PASS a pixel-vs-X test, and a correct load FAIL it — the
instrument measured the scene, not the save. The round trip is now
proven DETERMINISTICALLY (the blocked.md acceptance's "post-load rmse ≤
crossStepMax" realized at matched offsets):

1. Save slot 1 at the drive's success frame X (fresh `StateSaves/` mtime).
2. Advance (A 500 ms + stick 1500 ms + 2 s) to Y; assert rmse(Y, X) >
   0.05 (a no-op advance would make a no-op load indistinguishable).
3. Load slot 1; settle a FIXED total of 8 s (LoadState's 3 s floor + 5 s);
   screenshot X1 and classify it.
4. Advance again with the SAME input; load slot 1 again; settle 8 s;
   screenshot X2.
5. Assert rmse(X1, X2) ≤ crossStepMax (0.10) AND class(X1) == class(X).

A no-op load stays on the (different-class) Y screen → the class clause
fails. A restore of a different state lands on a different timeline
point → the rmse clause fails. Measured (two consecutive runs, 2026-10-18):
the advance moved the screen by 0.256 / 0.232 (> 0.05), both restores
returned to `storysub` with byte-identical signatures, and
**rmse(X1, X2) = 0.000** — the deterministic restore renders the same
frame (llvmpipe output is byte-identical for the same state and
timeline offset).

**The `--save_state` boot check matches the drive's success scene.**
Phase 3 asserts the fresh boot lands on the scene the drive reached: the
letterboxed legend (the `storysub` class with the measured bottom-bar
text clauses — the bright `BoxPresent` band calibration cannot match the
legend, measured boxband mean 60 < 100) or the dialogue box. Measured in
both runs: the boot lands on `storysub` with the subtitle text present
(`subbar bright=0.041 edges=0.105`) within 30 s.

**The drive iteration cap (90 → 200).** Two of the measured 2026-10-18
drives hit a SLOW-START boot: the title's dim phase does not register
START for ~54-95 iterations (~2.5-4.5 min), past the old 90 cap (which
had been sized to the fast boots). The cap is now 200 — the loop
detector still fails fast on any dead screen (amendments 3/4), so the
cap only bounds a legitimately progressing but slow boot, and the 15 min
deadline is the true limit.

**Evidence (2026-10-18, `artifacts/m6-savestate-20261018/`).**
`TestRealDriveToFirstDialogue` (hotkeys on) passed the round trip twice in
a row (327 s / 344 s): `savestate: saved slot 1 → …/StateSaves/GZLE01.s01
(hotkey key F1 over XInput2/0/Virtual core pointer)`; `advance-Y,
rmse(Y, X) = 0.256` / `0.232`; `after-load-X1, class=storysub` and
`after-load-X2, class=storysub` (identical signatures); `savestate:
double restore, rmse(X1, X2) = 0.000 (control threshold 0.10)`;
`savestate: --save_state boot landed on X (the opening legend (story
subtitle)) — class=storysub lb=0.000 subbar bright=0.041 edges=0.105`;
the exit rewrites matched BOTH device strings (`GCPadNew.ini
device=Pipe/0/pipe1`, `Hotkeys.ini device="XInput2/0/Virtual core
pointer"`). `TestRealDolphinBridge` passed with the hotkey config present
(66 s; `hotkey_device=XInput2/0/Virtual core pointer`; input proven over
the pipe; both exit-rewrite matches). `TestRealAbruptContextKill`
(default single-pipe path) passed with no leftover processes.
`go test -race -count=1 -skip Real ./internal/dolphin/` green (80 s).

**Consequences.** Savestates are now PROVEN and remain opt-in
(`LaunchSpec.Hotkeys=true`): the default launch is the single-pipe
controller-only path with no `Hotkeys.ini` (the unit tests assert the
default file is byte-identical to the pre-hotkey shape), and the default
drive test logs the savestate phases `NOT RUN` on that path. The blocked
register's "Dolphin savestates" row moved to Cleared (2026-10-18).
xdotool is a dev-host test dependency. Reversal: drop the `Hotkeys`
field handling in `writeUserConfig` and the `SaveState`/`LoadState`
methods — the default path and its unit tests are untouched by this
amendment.
