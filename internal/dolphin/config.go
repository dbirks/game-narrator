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
//	Pipes/pipe1          the game pad's FIFO (always the ONLY FIFO on the
//	                     default path; any file in <user>/Pipes/ becomes
//	                     device "Pipe/<i>/<filename>", upstream Pipes::Init).
//	Config/Hotkeys.ini   (opt-in only, LaunchSpec.Hotkeys=true) [Hotkeys]
//	                     Device = Pipe/<i>/hotkeys with the slot 1-4
//	                     save/load bindings (ADR-0043).
//	Pipes/hotkeys        (opt-in only) the second FIFO: savestate hotkeys
//	                     (ADR-0043).
//	GC/MemcardA.USA.raw  the Slot A RAW memory card: the PARENT dir (GC/) is
//	                     created by writeUserConfig before launch, the card
//	                     FILE is created by Dolphin on first save — the exit
//	                     check asserts its non-zero size as the use proof
//	                     (ADR-0043 amendment 2).
//
// Savestates ride the hotkey pipe (SaveState/LoadState, ADR-0043) and are
// OPT-IN and unproven; the default launch is the single-pipe path ProveInput
// was proven on. SaveSnapshot remains the picture half of any round-trip
// evidence.
const (
	pipeFileName   = "pipe1"
	hotkeyPipeFile = "hotkeys" // the second FIFO: savestate hotkeys (opt-in, ADR-0043)
)

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

// writeUserConfig lays the whole isolated config down before launch. It is
// idempotent: an existing file is patched in place (the [GCPad1] section is
// replaced wholesale, BackgroundInput forced on, other sections preserved),
// which keeps a re-run against an existing user dir honest instead of
// silently diverging from what Dolphin itself would write. hotkeys=false (the
// default, ADR-0043) writes EXACTLY the d8394f3 single-pipe layout: one FIFO
// (Pipes/pipe1), no Hotkeys.ini, no [General] HotkeysRequireFocus. The raw-
// directory-order index resolution is kept for both paths (correct per the
// measured raw order pipe1,hotkeys) but on the default path the directory
// holds pipe1 alone, so gcpadDev resolves to Pipe/0/pipe1.
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
	// The FIFOs exist before the device strings are computed: the strings are
	// measured FROM the directory's raw readdir order (the same enumeration
	// 2609's Pipes::PopulateDevices indexes with ScanDirectoryTree's
	// unsorted children), so every file must be in place when it is read.
	pipePath := filepath.Join(userDir, "Pipes", pipeFileName)
	if err := makeFIFO(pipePath); err != nil {
		return "", "", fmt.Errorf("dolphin: create controller FIFO %s: %w", pipeFileName, err)
	}
	if hotkeys {
		// The opt-in second FIFO (ADR-0043): savestate hotkeys. Not created
		// on the default path — the proven single-pipe layout carries no
		// second device, and no Hotkeys.ini is written (untouched).
		if err := makeFIFO(filepath.Join(userDir, "Pipes", hotkeyPipeFile)); err != nil {
			return "", "", fmt.Errorf("dolphin: create hotkey FIFO %s: %w", hotkeyPipeFile, err)
		}
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
	hotkeyDev, err = pipesDeviceAt(order, hotkeyPipeFile)
	if err != nil {
		return "", "", err
	}
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
