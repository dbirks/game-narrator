package dolphin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The isolated user dir layout this bridge creates before launch. Every file
// exists because ADR-0007 proved it on this host; nothing else is touched.
//
//	Config/Dolphin.ini   [Core] GFXBackend (declarative; the CLI --video_backend
//	                     is the binding path on 2609), SlotA = 1 (the
//	                     traditional RAW memory-card device — 2609's
//	                     EXIDeviceType::MemoryCard; the 2609 default
//	                     SlotA = 8 is EXIDeviceType::MemoryCardFolder, the
//	                     GCI-folder device, whose initial-save path was the
//	                     measured save failure on this host) and
//	                     MemcardAPath = <user>/GC/MemoryCardA.USA.raw (the
//	                     card file INSIDE the isolated user dir; the 2609
//	                     keys are [Core] MemcardAPath/MemcardBPath +
//	                     SlotA/SlotB — Source/Core/Core/Config/
//	                     MainSettings.cpp 2609 MAIN_MEMCARD_A_PATH / MAIN_SLOT_A,
//	                     and the /usr/bin/dolphin-emu strings carry the same),
//	                     [Input] BackgroundInput
//	                     = True (MANDATORY: without it the unfocused render
//	                     window makes Dolphin skip polling every controller
//	                     source, the Pipes FIFO included — a silent no-op),
//	                     plus no-fullscreen / no-confirm best-effort keys.
//	                     When the launch opts in to hotkeys (ADR-0043) it
//	                     also carries [General] HotkeysRequireFocus = False;
//	                     the proven single-pipe default (ADR-0007, d8394f3)
//	                     leaves the key untouched.
//	Config/GCPadNew.ini  [GCPad1] bound to the Pipes device of Pipes/pipe1
//	                     ("Pipe/<i>/pipe1", <i> = the file's RAW readdir
//	                     position — 2609's Pipes::PopulateDevices numbers
//	                     devices by unsorted directory-enumeration order, so
//	                     the index is computed from the directory, never
//	                     assumed; with the default single FIFO it resolves
//	                     to exactly Pipe/0/pipe1, the d8394f3 line) with the
//	                     button/axis control names the upstream Pipes backend
//	                     exposes (Button <tok>, Axis <MAIN|C> <X|Y> +/-, Axis
//	                     <L|R> +).
//	Config/Logger.ini    WriteToFile so user/Logs/dolphin.log records the
//	                     booted title and backend — the only place the
//	                     emulator records them (ADR-0007).
//	Pipes/pipe1          the game pad's FIFO — the ONLY FIFO on every path
//	                     (ADR-0043): any file in <user>/Pipes/ becomes device
//	                     "Pipe/<i>/<filename>" (upstream Pipes::Init). It is
//	                     SINGLE-CONSUMER: the only thing that reads it is the
//	                     game pad's input polling. The savestate hotkeys do
//	                     NOT ride it (approach D, ADR-0043 am7).
//	Config/Hotkeys.ini   (opt-in only, LaunchSpec.Hotkeys=true) [Hotkeys]
//	                     Device = XInput2/0/Virtual core pointer (the X11
//	                     device 2609's XInput2 backend resolves keyboard
//	                     input to — the exit-rewritten spike user dir's
//	                     keyboard controllers all carry exactly this string)
//	                     with the slot-1 save/load KEY bindings (save = F1,
//	                     load = F2 — X11 keysym control names, 2609
//	                     hotkey-expression syntax; ADR-0043 am7). The key
//	                     events are injected into the Xvfb display with XTest
//	                     (xdotool), which the hotkey scheduler's own polling
//	                     thread samples — no second reader touches the
//	                     controller FIFO, and no keyboard key is ever in the
//	                     game pad's input alphabet.
//	GC/MemcardA.USA.raw  the Slot A RAW memory card: the PARENT dir (GC/) is
//	                     created by writeUserConfig before launch, the card
//	                     FILE is created by Dolphin on first save — the exit
//	                     check asserts its non-zero size as the use proof
//	                     (ADR-0043 amendment 2).
//
// Savestates ride the keyboard-device hotkeys (SaveState/LoadState,
// ADR-0043 am7) and are OPT-IN; the default launch is the single-pipe path
// ProveInput was proven on, with no Hotkeys.ini and no focus gate.
// SaveSnapshot remains the picture half of any round-trip
// evidence.
const (
	pipeFileName = "pipe1"
	// hotkeyPipeFile is the name the OLD two-pipe layout (ADR-0043 original)
	// used for a second FIFO. It is gone: on every path it must NOT exist
	// (the unit tests assert its absence); the constant survives as that
	// assertion's spelling.
	hotkeyPipeFile = "hotkeys"
)

// The savestate hotkey layout (approach D, ADR-0043 am7): the hotkeys are
// bound to the KEYBOARD device and fired with XTest key events (xdotool),
// not to the controller pipe. The pipe's FIFO had two readers — Dolphin's
// SI polling thread (~120 Hz) and the HotkeyScheduler both read the same
// file, so a chord byte was consumed by whichever read first and fired
// nondeterministically (the measured approach-B failure). The X11 keyboard
// device is polled by exactly one thread (the hotkey scheduler), and XTest
// fake input lands in the server's key state that thread samples.
//
// hotkeyKeyboardDevice is the device string 2609's XInput2 backend resolves
// keyboard input to on X11: the exit-rewritten spike user dir (this host,
// 2609) carries "XInput2/0/Virtual core pointer" in EVERY keyboard-family
// controller's Device line (GCKeyNew.ini, WiimoteNew.ini, FreeLook...ini —
// the resolved strings, not our input). The clean-exit rewrite of THIS
// bridge's Hotkeys.ini is the standing oracle: if the line does not resolve,
// the exit rewrite drops it and checkExitRewrite fails loudly.
const (
	hotkeyKeyboardDevice = "XInput2/0/Virtual core pointer"

	// saveHotkeyKey / loadHotkeyKey are the X11 keysym control names bound
	// to slot 1's save/load. F1/F2 are outside the game's input alphabet
	// (the drive never types) and outside the drive script's button set;
	// a single key needs no chord timing at all. The exit rewrite of the
	// first real run is the collision oracle: it re-serializes every label
	// including the 2609 defaults, so a default hotkey on either key shows
	// up there (ADR-0043 am7).
	saveHotkeyKey = "F1"
	loadHotkeyKey = "F2"
)

// stateSavesDir is the savestate folder 2609 uses under the user dir:
// "StateSaves" (the exit-rewritten spike user dir on this host carries the
// folder — measured, not assumed; the old "states" spelling was never
// validated because the round trip never completed on this host, and the
// hotkey save's ground truth — a file appearing there — was watched in the
// wrong place). The --save_state boot path takes the returned file path
// verbatim, so no other change is needed.
const stateSavesDir = "StateSaves"

// The Slot A memory card (ADR-0043 amendment 2): the raw card file inside
// the isolated user dir, mirroring Dolphin's own default layout
// (<user>/GC/MemoryCardA.<REGION>.raw; the .USA tag matches the Wind Waker
// image this dev harness drives — 2609's GetMemcardPath substitutes the
// GAME's region into the name for any other region, MainSettings.cpp 2609).
// SlotA = 1 pins the raw-card device (2609 EXIDeviceType::MemoryCard = 1;
// the default 8 = MemoryCardFolder is the GCI-folder device).
const (
	memcardADir      = "GC"
	memcardAFileName = "MemoryCardA.USA.raw"
	memcardSlotA     = "1"
)

// memcardAPath is the Slot A raw card file the generated Dolphin.ini binds.
func memcardAPath(userDir string) string {
	return filepath.Join(userDir, memcardADir, memcardAFileName)
}

// gcPadPipeSectionBody is the [GCPad1] body (no Device line; gcPadPipeSection
// fills it). Button tokens and axis control names are exactly what upstream
// Pipes.cpp registers (s_button_tokens: A B X Y Z START L R D_UP D_DOWN
// D_LEFT D_RIGHT; axes: "Axis MAIN X +|-", "Axis MAIN Y +|-", "Axis C ...",
// "Axis L +", "Axis R +"). The GCPadNew.ini key spellings ("Main Stick/Up",
// "Triggers/L", ...) are the 2609 ones verified against the spike's isolated
// user dir.
const gcPadPipeSectionBody = `Buttons/A = ` + "`Button A`" + `
Buttons/B = ` + "`Button B`" + `
Buttons/X = ` + "`Button X`" + `
Buttons/Y = ` + "`Button Y`" + `
Buttons/Z = ` + "`Button Z`" + `
Buttons/Start = ` + "`Button START`" + `
Main Stick/Up = ` + "`Axis MAIN Y +`" + `
Main Stick/Down = ` + "`Axis MAIN Y -`" + `
Main Stick/Left = ` + "`Axis MAIN X -`" + `
Main Stick/Right = ` + "`Axis MAIN X +`" + `
Main Stick/Calibration = 100.00 141.42 100.00 141.42 100.00 141.42 100.00 141.42
C-Stick/Up = ` + "`Axis C Y +`" + `
C-Stick/Down = ` + "`Axis C Y -`" + `
C-Stick/Left = ` + "`Axis C X -`" + `
C-Stick/Right = ` + "`Axis C X +`" + `
C-Stick/Calibration = 100.00 141.42 100.00 141.42 100.00 141.42 100.00 141.42
Triggers/L = ` + "`Axis L +`" + `
Triggers/R = ` + "`Axis R +`" + `
D-Pad/Up = ` + "`Button D_UP`" + `
D-Pad/Down = ` + "`Button D_DOWN`" + `
D-Pad/Left = ` + "`Button D_LEFT`" + `
D-Pad/Right = ` + "`Button D_RIGHT`" + `
`

// gcPadPipeSection is the [GCPad1] block with its Device line filled from the
// raw readdir order (2609 assigns "Pipe/<i>/<name>" by enumeration position,
// ADR-0043 — the index is measured, never assumed).
func gcPadPipeSection(dev string) string {
	return "[GCPad1]\nDevice = " + dev + "\n" + gcPadPipeSectionBody
}

// dolphinIni writes the [Core]/[Input] config. BackgroundInput is part of the
// input contract, not a tuning option (ADR-0007): it is always True. The
// Slot A memory card is the raw card file inside the isolated user dir
// (SlotA = 1 + MemcardAPath, ADR-0043 amendment 2): with the 2609 defaults
// (no MemcardAPath, SlotA = 8 = the GCI-folder device) the measured new-game
// save on this host failed with "The game cannot be saved. Would you like
// to continue anyway?" — an explicit, bridge-owned card file makes the save
// target testable (created by Dolphin on first save, asserted non-zero at
// exit). The hotkey focus gate is written ONLY when the launch opts in to
// savestate hotkeys (ADR-0043): the proven single-pipe default leaves
// HotkeysRequireFocus untouched (the d8394f3 config).
func dolphinIni(backend, userDir string, hotkeys bool) string {
	var b strings.Builder
	b.WriteString("[Core]\n")
	b.WriteString("GFXBackend = " + backend + "\n")
	b.WriteString("SlotA = " + memcardSlotA + "\n")
	b.WriteString("MemcardAPath = " + memcardAPath(userDir) + "\n\n")
	b.WriteString("[GPU]\n")
	// 1x internal resolution and no vsync: llvmpipe is a CPU rasteriser
	// (ADR-0007) and these are the two knobs the spike's boot gate used.
	b.WriteString("Settings/InternalResolution = 1\n")
	b.WriteString("Hardware/VSync = False\n\n")
	b.WriteString("[MainWindow]\n")
	// Best-effort declarative keys; --batch is the mechanism that actually
	// suppresses the main UI, and Xvfb has no window manager to fullscreen
	// anything. Unknown keys are ignored and preserved by Dolphin's parser.
	b.WriteString("Fullscreen = False\n")
	b.WriteString("ConfirmOnClose = False\n\n")
	if hotkeys {
		// ADR-0043: without this the hotkey scheduler's input gate demands
		// window focus, which the unfocused Xvfb render window never gets
		// (the 2609 default for [General] HotkeysRequireFocus is True; the
		// Load/Save State groups are focus-gated, HotkeyManager.cpp
		// s_groups_info).
		b.WriteString("[General]\n")
		b.WriteString("HotkeysRequireFocus = False\n\n")
	}
	b.WriteString("[Input]\n")
	b.WriteString("BackgroundInput = True\n")
	return b.String()
}

// loggerIni is the spike's file-logging config so user/Logs/dolphin.log proves
// the booted title and backend (docs/spec/11: "record emulator build +
// config + shaders/video backend").
const loggerIni = `[Logs]
BOOT = True
CORE = True
GP = True
Video = True
Host GPU = True
DVD = True
EXI = True
SI = True
DSPHLE = True
JIT = True
COMMON = True
MASTER = True
[Options]
WriteToConsole = False
WriteToFile = True
WriteToWindow = False
`

// gfxIni turns Dolphin's on-screen-display messages off. With them on, the
// llvmpipe renderer's "Video Info: … Mesa, llvmpipe (LLVM …) … Core Profile
// Mesa" toast is drawn over the game frame after boot and every savestate
// load; the narrator's OCR reads it as dialogue (the 2026-10-21 A/V harness
// run transcribed it in the spoken audio through the unmatched read-through).
// A real GameCube through a capture card has no such overlay, so the dev
// emulator must not draw one either (#41).
const gfxIni = `[Settings]
OnScreenDisplayMessages = False
ShowFPS = False
`

// writeUserConfig lays the whole isolated config down before launch. It is
// idempotent: an existing file is patched in place (the [GCPad1] section is
// replaced wholesale, BackgroundInput forced on, other sections preserved),
// which keeps a re-run against an existing user dir honest instead of
// silently diverging from what Dolphin itself would write. hotkeys=false (the
// default, ADR-0043) writes EXACTLY the d8394f3 single-pipe layout: one FIFO
// (Pipes/pipe1), no Hotkeys.ini, no [General] HotkeysRequireFocus. The
// hotkey path (ADR-0043 am3, approach B) writes the SAME single-pipe layout
// plus Hotkeys.ini: no second FIFO is created, and the hotkey device is the
// game pad's measured device string (Pipe/0/pipe1 with pipe1 the only FIFO),
// so the raw-directory-order index resolution runs once and both configs
// agree on the one device the emulator binds both controllers to.
func writeUserConfig(userDir, backend string, hotkeys bool) (gcpadDev, hotkeyDev string, err error) {
	cfg := filepath.Join(userDir, "Config")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		return "", "", fmt.Errorf("dolphin: create Config dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "Dolphin.ini"), []byte(dolphinIni(backend, userDir, hotkeys)), 0o644); err != nil {
		return "", "", fmt.Errorf("dolphin: write Dolphin.ini: %w", err)
	}
	// The memory card's PARENT dir exists before launch (Dolphin creates the
	// card file itself on first save; the parent is this bridge's
	// responsibility — ADR-0043 amendment 2).
	if err := os.MkdirAll(filepath.Join(userDir, memcardADir), 0o755); err != nil {
		return "", "", fmt.Errorf("dolphin: create memory-card dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Join(userDir, "Config"), "Logger.ini"), []byte(loggerIni), 0o644); err != nil {
		return "", "", fmt.Errorf("dolphin: write Logger.ini: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "GFX.ini"), []byte(gfxIni), 0o644); err != nil {
		return "", "", fmt.Errorf("dolphin: write GFX.ini: %w", err)
	}
	// The FIFO exists before the device strings are computed: the strings
	// are measured FROM the directory's raw readdir order (the same
	// enumeration 2609's Pipes::PopulateDevices indexes with
	// ScanDirectoryTree's unsorted children), so the file must be in place
	// when the order is read. On EVERY path there is exactly this one FIFO,
	// and it stays single-consumer (approach D, ADR-0043 am7): the savestate
	// hotkeys do not ride it.
	pipePath := filepath.Join(userDir, "Pipes", pipeFileName)
	if err := makeFIFO(pipePath); err != nil {
		return "", "", fmt.Errorf("dolphin: create controller FIFO %s: %w", pipeFileName, err)
	}
	order, err := rawReaddir(filepath.Join(userDir, "Pipes"))
	if err != nil {
		return "", "", fmt.Errorf("dolphin: read the Pipes device order: %w", err)
	}
	gcpadDev, err = pipesDeviceAt(order, pipeFileName)
	if err != nil {
		return "", "", err
	}
	if err := writeGCPadNew(userDir, gcpadDev); err != nil {
		return "", "", err
	}
	if !hotkeys {
		// The default single-pipe path: no Hotkeys.ini, no hotkey device
		// (SaveState/LoadState refuse with ErrHotkeysDisabled, ADR-0043).
		return gcpadDev, "", nil
	}
	// The opt-in hotkey path (approach D): NO second FIFO, and the hotkeys
	// do NOT ride the pipe. The hotkey device is the X11 keyboard device
	// (hotkeyKeyboardDevice) — 2609's hotkey scheduler polls it on its own
	// thread, and the key events are injected with XTest (xdotool), so the
	// controller FIFO keeps exactly one reader (the game pad's input
	// polling). The device string is the measured one (the exit-rewritten
	// spike user dir's keyboard controllers all resolve to it); the exit
	// rewrite of this file is the standing re-verification.
	hotkeyDev = hotkeyKeyboardDevice
	if err := os.WriteFile(filepath.Join(cfg, "Hotkeys.ini"), []byte(hotkeysIni(hotkeyDev)), 0o644); err != nil {
		return "", "", fmt.Errorf("dolphin: write Hotkeys.ini: %w", err)
	}
	return gcpadDev, hotkeyDev, nil
}

// pipesDeviceAt renders the 2609 device string for one file of the raw
// readdir order: "Pipe/<i>/<name>", <i> = the file's enumeration position.
func pipesDeviceAt(order []string, name string) (string, error) {
	for i, n := range order {
		if n == name {
			return fmt.Sprintf("Pipe/%d/%s", i, name), nil
		}
	}
	return "", fmt.Errorf("dolphin: FIFO %s missing from the Pipes enumeration %v", name, order)
}

// writeGCPadNew patches [GCPad1] to the Pipes mapping. When the file does not
// exist (a fresh isolated dir) only [GCPad1] is written: Dolphin fills the
// remaining pads with its defaults on first run, and the mapping lines are
// proven to survive a clean exit unchanged (the spike's exit-rewrite check).
func writeGCPadNew(userDir, dev string) error {
	path := filepath.Join(userDir, "Config", "GCPadNew.ini")
	existing := ""
	if b, err := os.ReadFile(path); err == nil {
		existing = string(b)
	}

	var out string
	if existing == "" {
		out = gcPadPipeSection(dev)
	} else {
		// Split into sections, replace [GCPad1] wholesale (a leftover
		// "Device = Keyboard" line is exactly the bug the spike's patcher
		// deleted), keep every other section verbatim.
		blocks := splitSections(existing)
		seen := false
		for i, blk := range blocks {
			if strings.HasPrefix(strings.TrimSpace(blk), "[GCPad1]") {
				blocks[i] = gcPadPipeSection(dev)
				seen = true
				break
			}
		}
		if !seen {
			blocks = append(blocks, gcPadPipeSection(dev))
		}
		out = strings.Join(blocks, "")
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return fmt.Errorf("dolphin: write GCPadNew.ini: %w", err)
	}
	return nil
}

// splitSections breaks an ini file into per-[Section] blocks. A line that is
// not inside a section (ini preamble) sticks to the first block. Manual
// rather than regex: Go's RE2 has no lookahead, and ini structure is
// line-oriented anyway.
func splitSections(ini string) []string {
	var blocks []string
	cur := ""
	for _, line := range strings.Split(ini, "\n") {
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			if cur != "" {
				blocks = append(blocks, cur)
			}
			cur = line + "\n"
			continue
		}
		cur += line + "\n"
	}
	if cur != "" {
		blocks = append(blocks, cur)
	}
	return blocks
}

// makeFIFO creates the FIFO if it is not already one. A regular file at the
// path is an error, not a silent replacement: overwriting it would change the
// device Dolphin binds to, and a FIFO at the path from a previous run is
// exactly what a re-run wants.
func makeFIFO(path string) error {
	fi, err := os.Lstat(path)
	if err == nil {
		if fi.Mode()&os.ModeNamedPipe != 0 {
			return nil
		}
		return fmt.Errorf("dolphin: %s exists and is not a FIFO", filepath.Base(path))
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := mkFIFO(path, 0o644); err != nil {
		return err
	}
	return nil
}
