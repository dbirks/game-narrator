//go:build unix

package dolphin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHotkeysIni: the [Hotkeys] section content the bridge writes — the
// device line (the measured second-FIFO Pipes path) and the slot bindings
// for both the save and load groups.
func TestHotkeysIni(t *testing.T) {
	ini := hotkeysIni("Pipe/1/hotkeys")
	if !strings.HasPrefix(ini, "[Hotkeys]\n") {
		t.Fatalf("Hotkeys.ini must open with the [Hotkeys] section:\n%s", ini)
	}
	if !strings.Contains(ini, "Device = Pipe/1/hotkeys\n") {
		t.Errorf("missing the device binding:\n%s", ini)
	}
	// Every save and load slot bound to a distinct pipe button.
	want := map[string]string{
		"Save State/Save State Slot 1": "Button A",
		"Save State/Save State Slot 2": "Button B",
		"Save State/Save State Slot 3": "Button X",
		"Save State/Save State Slot 4": "Button Y",
		"Load State/Load State Slot 1": "Button Z",
		"Load State/Load State Slot 2": "Button L",
		"Load State/Load State Slot 3": "Button R",
		"Load State/Load State Slot 4": "Button START",
	}
	for k, v := range want {
		line := k + " = `" + v + "`\n"
		if !strings.Contains(ini, line) {
			t.Errorf("missing binding %q:\n%s", line, ini)
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

// TestWriteUserConfigHotkeyFIFO: a fresh user dir with the opt-in flag gets
// BOTH Pipes FIFOs and the Hotkeys.ini; both device strings are computed
// from the directory's raw readdir order (2609 numbers devices by
// enumeration position, so the index is measured, never assumed — ADR-0043),
// and both ini files carry the measured strings.
func TestWriteUserConfigHotkeyFIFO(t *testing.T) {
	dir := t.TempDir()
	gcpadDev, hotkeyDev, err := writeUserConfig(dir, "OGL", true)
	if err != nil {
		t.Fatalf("writeUserConfig: %v", err)
	}
	for _, p := range []string{
		filepath.Join(dir, "Pipes", pipeFileName),
		filepath.Join(dir, "Pipes", hotkeyPipeFile),
		filepath.Join(dir, "Config", "Hotkeys.ini"),
	} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("missing %s: %v", p, err)
			continue
		}
		if strings.Contains(p, "Pipes") && fi.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("%s is not a FIFO (mode %v)", p, fi.Mode())
		}
	}
	if !strings.HasSuffix(hotkeyDev, "/hotkeys") || !strings.HasSuffix(gcpadDev, "/pipe1") {
		t.Errorf("device strings do not name their FIFOs: gcpad=%q hotkey=%q", gcpadDev, hotkeyDev)
	}
	if gcpadDev == hotkeyDev {
		t.Errorf("the two FIFOs must not share a device: %q", gcpadDev)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "Config", "Hotkeys.ini")); err != nil {
		t.Errorf("read Hotkeys.ini: %v", err)
	} else if string(got) != hotkeysIni(hotkeyDev) {
		t.Errorf("Hotkeys.ini on disk differs from hotkeysIni(%s):\n%s", hotkeyDev, got)
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

// TestStateSlotPath: the state-file glob (slot 1 → *.s01 under <user>/states)
// finds the file the hotkey save produces, and the newest wins when a stale
// one lingers.
func TestStateSlotPath(t *testing.T) {
	dir := t.TempDir()
	states := filepath.Join(dir, "states")
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
