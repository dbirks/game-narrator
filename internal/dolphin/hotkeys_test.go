//go:build unix

package dolphin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHotkeysIni: the [Hotkeys] section content the bridge writes — the
// keyboard device line (approach D, ADR-0043 am7: the X11 keyboard device
// 2609's XInput2 backend resolves keyboard input to, measured from the
// exit-rewritten spike user dir) and the slot-1 save/load KEY bindings in
// 2609's single-key hotkey-expression syntax (backtick-wrapped X11 keysym
// names — the same spelling the exit-rewritten keyboard controller configs
// carry). Save and load are DISJOINT single keys: no chord, no timing race,
// and neither key is in the game pad's button alphabet.
func TestHotkeysIni(t *testing.T) {
	ini := hotkeysIni(hotkeyKeyboardDevice)
	if !strings.HasPrefix(ini, "[Hotkeys]\n") {
		t.Fatalf("Hotkeys.ini must open with the [Hotkeys] section:\n%s", ini)
	}
	if !strings.Contains(ini, "Device = "+hotkeyKeyboardDevice+"\n") {
		t.Errorf("missing the keyboard device binding:\n%s", ini)
	}
	want := map[string]string{
		"Save State/Save State Slot 1": "`" + saveHotkeyKey + "`\n",
		"Load State/Load State Slot 1": "`" + loadHotkeyKey + "`\n",
	}
	for k, v := range want {
		line := k + " = " + v
		if !strings.Contains(ini, line) {
			t.Errorf("missing binding %q:\n%s", line, ini)
		}
	}
	// Single keys, not chords: no chord-expression syntax in the bindings.
	if strings.Contains(ini, "@(") {
		t.Errorf("the keyboard layout binds single keys, not chords:\n%s", ini)
	}
	// The save and load keys are disjoint (a shared key would fire both).
	if saveHotkeyKey == loadHotkeyKey {
		t.Errorf("the save and load keys must differ: %q", saveHotkeyKey)
	}
	// Slot 1 is the only bound slot: no other slot lines.
	if strings.Contains(ini, "Slot 2") || strings.Contains(ini, "Slot 3") || strings.Contains(ini, "Slot 4") {
		t.Errorf("slots 2-4 must stay unbound in the keyboard layout:\n%s", ini)
	}
}

// TestHotkeyKeyFor: the slot→key mapping (slot 1 only; the others refuse),
// and the save/load keys come from the same constants Hotkeys.ini is
// written with (the bridge and the config can never disagree).
func TestHotkeyKeyFor(t *testing.T) {
	save, err := hotkeyKeyFor(1, true)
	if err != nil || save != saveHotkeyKey {
		t.Errorf("hotkeyKeyFor(1, save) = %q, %v; want %q", save, err, saveHotkeyKey)
	}
	load, err := hotkeyKeyFor(1, false)
	if err != nil || load != loadHotkeyKey {
		t.Errorf("hotkeyKeyFor(1, load) = %q, %v; want %q", load, err, loadHotkeyKey)
	}
	if save == load {
		t.Errorf("save and load must not share a key: %q", save)
	}
	for _, slot := range []int{0, 2, 3, 4, 5} {
		if _, err := hotkeyKeyFor(slot, true); err == nil {
			t.Errorf("slot %d must be refused (only slot 1 is bound)", slot)
		}
	}
}

// TestDolphinIniHotkeyGate: the [General] HotkeysRequireFocus=False line is
// written ONLY when the launch opts in to hotkeys — without it the unfocused
// Xvfb window can never take hotkeys (the 2609 default is True; ADR-0043).
// The proven single-pipe default leaves the key untouched (d8394f3).
func TestDolphinIniHotkeyGate(t *testing.T) {
	ini := dolphinIni("OGL", "/tmp/dolphin-user", true)
	if !strings.Contains(ini, "[General]\nHotkeysRequireFocus = False\n") {
		t.Errorf("Dolphin.ini (hotkeys on) missing the hotkey focus gate:\n%s", ini)
	}
	if !strings.Contains(ini, "BackgroundInput = True\n") {
		t.Errorf("Dolphin.ini (hotkeys on) lost the BackgroundInput line:\n%s", ini)
	}
	// The default (hotkeys off) writes NO [General] section at all.
	def := dolphinIni("OGL", "/tmp/dolphin-user", false)
	if strings.Contains(def, "HotkeysRequireFocus") {
		t.Errorf("Dolphin.ini (hotkeys off) must leave HotkeysRequireFocus untouched:\n%s", def)
	}
	if !strings.Contains(def, "BackgroundInput = True\n") {
		t.Errorf("Dolphin.ini (hotkeys off) lost the BackgroundInput line:\n%s", def)
	}
}

// TestDolphinIniMemcardSlotA: the generated Dolphin.ini binds Slot A to the
// traditional RAW memory card (2609 EXIDeviceType::MemoryCard = 1) and to a
// card file INSIDE the isolated user dir (ADR-0043 amendment 2 — the 2609
// defaults, no MemcardAPath + SlotA = 8 = the GCI-folder device, were the
// measured new-game save failure on this host). The keys are the 2609 ones
// ([Core] SlotA / MemcardAPath; MainSettings.cpp MAIN_SLOT_A /
// MAIN_MEMCARD_A_PATH), and the path is the bridge's <user>/GC/
// MemoryCardA.USA.raw.
func TestDolphinIniMemcardSlotA(t *testing.T) {
	user := "/tmp/dolphin-user"
	ini := dolphinIni("OGL", user, false)
	for _, want := range []string{
		"[Core]\nGFXBackend = OGL\nSlotA = 1\n",
		"MemcardAPath = " + filepath.Join(user, "GC", "MemoryCardA.USA.raw") + "\n",
	} {
		if !strings.Contains(ini, want) {
			t.Errorf("Dolphin.ini missing %q:\n%s", want, ini)
		}
	}
	// The default launch does not touch the hotkey gate (the memcard keys are
	// unconditional — they are part of the launch contract, ADR-0043 am2).
	iniOn := dolphinIni("OGL", user, true)
	if !strings.Contains(iniOn, "SlotA = 1\n") || !strings.Contains(iniOn, "MemcardAPath = "+filepath.Join(user, "GC", "MemoryCardA.USA.raw")+"\n") {
		t.Errorf("Dolphin.ini (hotkeys on) lost the memcard keys:\n%s", iniOn)
	}
}

// TestWriteUserConfigMemcardDir: writeUserConfig creates the memory card's
// PARENT dir (<user>/GC) before launch (the card file itself is created by
// Dolphin on first save — the exit check asserts its non-zero size, ADR-0043
// amendment 2), and Dolphin.ini binds it.
func TestWriteUserConfigMemcardDir(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := writeUserConfig(dir, "OGL", false); err != nil {
		t.Fatalf("writeUserConfig: %v", err)
	}
	// The parent dir exists (the file does not yet — Dolphin creates it).
	gc := filepath.Join(dir, "GC")
	fi, err := os.Stat(gc)
	if err != nil || !fi.IsDir() {
		t.Fatalf("the GC dir was not created before launch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gc, "MemoryCardA.USA.raw")); !os.IsNotExist(err) {
		t.Errorf("the card file must not be pre-created (Dolphin creates it on first save): %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "Config", "Dolphin.ini")); err != nil {
		t.Fatalf("read Dolphin.ini: %v", err)
	} else if !strings.Contains(string(got), "MemcardAPath = "+memcardAPath(dir)+"\n") ||
		!strings.Contains(string(got), "SlotA = 1\n") {
		t.Errorf("Dolphin.ini missing the memcard binding:\n%s", got)
	}
}

// TestWriteUserConfigHotkeyKeys: a fresh user dir with the opt-in flag
// gets the SAME single-pipe layout as the default plus Hotkeys.ini (the
// approach-D unblock, ADR-0043 am7): exactly one FIFO (Pipes/pipe1, the
// single-consumer controller path), NO second FIFO, and Hotkeys.ini bound
// to the X11 KEYBOARD device (hotkeyKeyboardDevice — not the game pad's
// pipe device, whose FIFO must stay single-consumer) with the slot-1
// key bindings present.
func TestWriteUserConfigHotkeyKeys(t *testing.T) {
	dir := t.TempDir()
	gcpadDev, hotkeyDev, err := writeUserConfig(dir, "OGL", true)
	if err != nil {
		t.Fatalf("writeUserConfig: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dir, "Pipes", pipeFileName))
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("Pipes/pipe1 is not a FIFO: %v", err)
	}
	// NO second FIFO on the keyboard path (it never existed since the
	// original two-pipe attempt was abandoned).
	if _, err := os.Lstat(filepath.Join(dir, "Pipes", hotkeyPipeFile)); !os.IsNotExist(err) {
		t.Errorf("Pipes/%s must not exist on the keyboard path (err %v)", hotkeyPipeFile, err)
	}
	// The hotkey device is the KEYBOARD device, distinct from the game pad's
	// pipe device (one reader per input source: the scheduler polls the
	// keyboard, the game pad's input polling reads the pipe).
	if hotkeyDev != hotkeyKeyboardDevice {
		t.Errorf("hotkey device = %q, want the keyboard device %q", hotkeyDev, hotkeyKeyboardDevice)
	}
	if hotkeyDev == gcpadDev {
		t.Errorf("the hotkey device must not ride the game pad's pipe (single-consumer FIFO): %q", hotkeyDev)
	}
	if gcpadDev != "Pipe/0/pipe1" {
		t.Errorf("gcpad device = %q, want Pipe/0/pipe1 (pipe1 is the only FIFO)", gcpadDev)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "Config", "Hotkeys.ini")); err != nil {
		t.Errorf("read Hotkeys.ini: %v", err)
	} else if string(got) != hotkeysIni(hotkeyKeyboardDevice) {
		t.Errorf("Hotkeys.ini on disk differs from hotkeysIni(%s):\n%s", hotkeyKeyboardDevice, got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "Config", "GCPadNew.ini")); err != nil {
		t.Errorf("read GCPadNew.ini: %v", err)
	} else if !strings.Contains(string(got), "Device = "+gcpadDev+"\n") {
		t.Errorf("GCPadNew.ini missing the measured device line %q:\n%s", gcpadDev, got)
	}
}

// TestWriteUserConfigDefaultSinglePipe: the default launch (hotkeys off,
// ADR-0043) writes EXACTLY the proven d8394f3 single-pipe layout — one FIFO
// (Pipes/pipe1), no second FIFO, no Hotkeys.ini, no [General]
// HotkeysRequireFocus, and GCPadNew.ini carrying Device = Pipe/0/pipe1.
func TestWriteUserConfigDefaultSinglePipe(t *testing.T) {
	dir := t.TempDir()
	gcpadDev, hotkeyDev, err := writeUserConfig(dir, "OGL", false)
	if err != nil {
		t.Fatalf("writeUserConfig: %v", err)
	}
	if hotkeyDev != "" {
		t.Errorf("default path: hotkey device = %q, want empty (hotkeys disabled)", hotkeyDev)
	}
	// pipe1 exists and is a FIFO; the hotkeys FIFO does NOT exist.
	fi, err := os.Lstat(filepath.Join(dir, "Pipes", pipeFileName))
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("Pipes/pipe1 is not a FIFO: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "Pipes", hotkeyPipeFile)); !os.IsNotExist(err) {
		t.Errorf("Pipes/%s must not exist on the default path (err %v)", hotkeyPipeFile, err)
	}
	// No Hotkeys.ini at all (untouched, as in d8394f3).
	if _, err := os.Stat(filepath.Join(dir, "Config", "Hotkeys.ini")); !os.IsNotExist(err) {
		t.Errorf("Config/Hotkeys.ini must not exist on the default path (err %v)", err)
	}
	// The proven device line: with pipe1 the only FIFO, the measured raw
	// order resolves to exactly the d8394f3 string.
	if gcpadDev != "Pipe/0/pipe1" {
		t.Errorf("gcpad device = %q, want Pipe/0/pipe1 (the proven d8394f3 line)", gcpadDev)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "Config", "GCPadNew.ini")); err != nil {
		t.Errorf("read GCPadNew.ini: %v", err)
	} else if !strings.Contains(string(got), "Device = Pipe/0/pipe1\n") {
		t.Errorf("GCPadNew.ini missing the proven device line:\n%s", got)
	}
	// Dolphin.ini keeps the proven [Core]/[Input] keys and no [General] gate,
	// and binds the Slot A raw card inside the isolated user dir (ADR-0043
	// amendment 2).
	if got, err := os.ReadFile(filepath.Join(dir, "Config", "Dolphin.ini")); err != nil {
		t.Errorf("read Dolphin.ini: %v", err)
	} else {
		if strings.Contains(string(got), "HotkeysRequireFocus") {
			t.Errorf("Dolphin.ini must not carry the hotkey focus gate on the default path:\n%s", got)
		}
		for _, want := range []string{"GFXBackend = OGL", "BackgroundInput = True", "SlotA = 1", "MemcardAPath = " + memcardAPath(dir)} {
			if !strings.Contains(string(got), want) {
				t.Errorf("Dolphin.ini missing %q:\n%s", want, got)
			}
		}
	}
	// The memory card's parent dir is created before launch (ADR-0043 am2).
	if fi, err := os.Stat(filepath.Join(dir, "GC")); err != nil || !fi.IsDir() {
		t.Errorf("the GC (memory-card) dir was not created: %v", err)
	}
}

// TestPipesDeviceOrder: rawReaddir is the enumeration 2609's
// PopulateDevices indexes with — pipesDeviceAt renders "Pipe/<i>/<name>"
// with <i> the file's position in THAT order (unsorted; os.ReadDir's sorted
// order would not do), and the order is stable across reads of the same
// directory.
func TestPipesDeviceOrder(t *testing.T) {
	pipes := filepath.Join(t.TempDir(), "Pipes")
	if err := os.MkdirAll(pipes, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{pipeFileName, hotkeyPipeFile} {
		if err := makeFIFO(filepath.Join(pipes, n)); err != nil {
			t.Fatal(err)
		}
	}
	order, err := rawReaddir(pipes)
	if err != nil {
		t.Fatalf("rawReaddir: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("order = %v, want both FIFOs", order)
	}
	gcpadDev, err := pipesDeviceAt(order, pipeFileName)
	if err != nil {
		t.Fatalf("pipesDeviceAt(pipe1): %v", err)
	}
	hotkeyDev, err := pipesDeviceAt(order, hotkeyPipeFile)
	if err != nil {
		t.Fatalf("pipesDeviceAt(hotkeys): %v", err)
	}
	if gcpadDev == hotkeyDev {
		t.Errorf("the two FIFOs must not share a device index: %q", gcpadDev)
	}
	// The index is the position in the measured order (not the sorted one).
	for i, n := range order {
		want := fmt.Sprintf("Pipe/%d/%s", i, n)
		if n == pipeFileName && gcpadDev != want {
			t.Errorf("gcpad device = %q, want %q (order %v)", gcpadDev, want, order)
		}
		if n == hotkeyPipeFile && hotkeyDev != want {
			t.Errorf("hotkey device = %q, want %q (order %v)", hotkeyDev, want, order)
		}
	}
	order2, err := rawReaddir(pipes)
	if err != nil || strings.Join(order2, ",") != strings.Join(order, ",") {
		t.Errorf("readdir order is not stable: %v vs %v", order, order2)
	}
	if _, err := pipesDeviceAt(order, "missing"); err == nil {
		t.Error("a missing FIFO must be an error")
	}
}

// TestSaveLoadStateKeyboardKeys: the SaveState/LoadState contract over the
// fake SendKey seam (test category (a): a synthetic stub — no emulator, no
// X server; the stub stands in for both xdotool and Dolphin's reaction). A
// default launch refuses with ErrHotkeysDisabled and injects NOTHING; an
// opt-in launch injects exactly the save key (F1) into the launch's display
// with the scheduler-spanning hold, the stub's "emulator" writes the state
// file on that press, and SaveState returns its path with a fresh mtime;
// LoadState injects exactly the load key (F2). The controller FIFO is never
// touched by either call (the single-consumer invariant, approach D).
func TestSaveLoadStateKeyboardKeys(t *testing.T) {
	type keyPress struct {
		display, key string
		hold         time.Duration
	}
	var mu sync.Mutex
	var presses []keyPress

	// Default launch: refused, nothing injected.
	def := newTestBridge(t, nil)
	mustLaunch(t, def, LaunchSpec{})
	if _, err := def.b.SaveState(context.Background(), 1); !errors.Is(err, ErrHotkeysDisabled) {
		t.Errorf("default path SaveState = %v, want ErrHotkeysDisabled", err)
	}
	if err := def.b.LoadState(context.Background(), 1); !errors.Is(err, ErrHotkeysDisabled) {
		t.Errorf("default path LoadState = %v, want ErrHotkeysDisabled", err)
	}
	def.b.Stop()

	// Opt-in launch with the stubbed SendKey seam (declared before the
	// closure so the stub can reach the bridge it records).
	var env *testEnv
	env = newTestBridge(t, nil, func(o *Options) {
		o.SendKey = func(display, key string, hold time.Duration) error {
			mu.Lock()
			presses = append(presses, keyPress{display: display, key: key, hold: hold})
			mu.Unlock()
			if key == saveHotkeyKey {
				// The stub emulator: the save key press writes the state file
				// where 2609 puts savestates (the StateSaves folder).
				st := env.b.State()
				dir := filepath.Join(st.UserDir, stateSavesDir)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Errorf("stub emulator mkdir states: %v", err)
					return nil
				}
				if err := os.WriteFile(filepath.Join(dir, "FAKE01.s01"), []byte("state"), 0o644); err != nil {
					t.Errorf("stub emulator write state file: %v", err)
				}
			}
			return nil
		}
	})
	mustLaunch(t, env, LaunchSpec{Hotkeys: true})
	ctx := context.Background()

	// Save: one keydown/keyup of the SAVE key into the launch's display, and
	// the state file the stub wrote comes back with a fresh mtime.
	path, err := env.b.SaveState(ctx, 1)
	if err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() || fi.Size() == 0 {
		t.Fatalf("the save path %s is not a written file: %v", path, err)
	}
	mu.Lock()
	var savePresses []keyPress
	for _, p := range presses {
		if p.key == saveHotkeyKey {
			savePresses = append(savePresses, p)
		}
	}
	mu.Unlock()
	if len(savePresses) != 1 {
		t.Fatalf("the save hotkey was injected %d times, want exactly 1 (presses %v)", len(savePresses), presses)
	}
	if want := env.b.State().Display; savePresses[0].display != want || savePresses[0].hold != hotkeyHold {
		t.Errorf("save press = %+v, want the launch display %q with the %s hold", savePresses[0], want, hotkeyHold)
	}

	// Unbound slots refuse before any injection.
	if _, err := env.b.SaveState(ctx, 2); err == nil {
		t.Error("slot 2 must be refused (only slot 1 is bound)")
	}

	// Load: one keydown/keyup of the LOAD key, then the settle floor.
	if err := env.b.LoadState(ctx, 1); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	mu.Lock()
	var loadPresses []keyPress
	for _, p := range presses {
		if p.key == loadHotkeyKey {
			loadPresses = append(loadPresses, p)
		}
	}
	mu.Unlock()
	if len(loadPresses) != 1 {
		t.Fatalf("the load hotkey was injected %d times, want exactly 1 (presses %v)", len(loadPresses), presses)
	}
	env.b.Stop()
}

// TestStateSlotPath: the state-file glob (slot 1 → *.s01 under <user>/
// StateSaves — the 2609 folder, not the legacy "states" spelling) finds the
// file the hotkey save produces, and the newest wins when a stale one
// lingers.
func TestStateSlotPath(t *testing.T) {
	dir := t.TempDir()
	states := filepath.Join(dir, stateSavesDir)
	if err := os.MkdirAll(states, 0o755); err != nil {
		t.Fatal(err)
	}
	b := &bridge{}
	b.st.UserDir = dir
	// Nothing saved yet: no path, no error.
	if p, err := b.stateSlotPath(1); err != nil || p != "" {
		t.Fatalf("empty states dir: got %q, %v", p, err)
	}
	fresh := filepath.Join(states, "GZLE01.s01")
	if err := os.WriteFile(fresh, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := b.stateSlotPath(1)
	if err != nil || p != fresh {
		t.Fatalf("stateSlotPath(1) = %q, %v; want %s", p, err, fresh)
	}
	// A stale file for the same slot: the newest mtime wins.
	stale := filepath.Join(states, "OLDAAA.s01")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(stale, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	p, err = b.stateSlotPath(1)
	if err != nil || p != fresh {
		t.Fatalf("newest-wins: got %q, %v; want %s", p, err, fresh)
	}
	// Out-of-range slots are refused, and slot 2 does not match slot 1's file.
	if _, err := b.stateSlotPath(0); err == nil {
		t.Error("slot 0 must be refused")
	}
	if _, err := b.stateSlotPath(5); err == nil {
		t.Error("slot 5 must be refused")
	}
	if p, _ := b.stateSlotPath(2); p != "" {
		t.Errorf("slot 2 must not match the slot-1 file: %q", p)
	}
}
