package dolphin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Savestate hotkeys ride the KEYBOARD device (approach D, ADR-0043 am7) and
// are OPT-IN: the launch must set LaunchSpec.Hotkeys=true, or SaveState/
// LoadState refuse with ErrHotkeysDisabled. Hotkeys.ini binds slot 1's
// save/load to two X11 keysyms on the X11 keyboard device
// (hotkeyKeyboardDevice, "XInput2/0/Virtual core pointer" — the device 2609's
// XInput2 backend resolves keyboard input to, measured from the
// exit-rewritten spike user dir), and SaveState/LoadState fire the keys with
// XTest fake input (xdotool keydown/keyup, the Options.SendKey seam), which
// the hotkey scheduler's own polling thread samples.
//
// Why the keyboard, not the pipe (the measured approach-B failure): with the
// chords bound to the controller pipe, the FIFO had TWO readers — Dolphin's
// SI polling thread (~120 Hz) and the HotkeyScheduler both open and read the
// same FIFO, so a chord byte was consumed by whichever thread read first and
// the hotkeys fired nondeterministically. The X11 keyboard device is polled
// by exactly one thread (the hotkey scheduler), and XTest fake input updates
// the X server's key state that thread samples — no shared buffer, no
// contention, and the controller FIFO keeps a single consumer. The drive's
// input alphabet is pipe buttons and axes only; a keyboard key can never
// disturb the game state the round trip measures.
//
// Config: [Hotkeys] Device = <keyboard device> plus the group/label keys
// ("Save State/Save State Slot 1", "Load State/Load State Slot 1" —
// HotkeyManager.cpp s_hotkey_labels, groups "Save State"/"Load State"),
// single-key bindings in 2609's hotkey-expression syntax (backtick-wrapped
// control name; the X11 backend's control names are X11 keysym names, the
// same spelling the exit-rewritten keyboard controller configs carry).
// [General] HotkeysRequireFocus = False is written on the opt-in path
// (Dolphin.ini, config.go) because the 2609 default (True) focus-gates the
// Load/Save State groups and the unfocused Xvfb render window never gets
// focus.

const (
	saveSlotMin = 1
	saveSlotMax = 4

	// hotkeySaveTimeout bounds the wait for the state file to appear
	// (a GameCube save dumps the whole core; generous at llvmpipe speed).
	hotkeySaveTimeout = 30 * time.Second
	// hotkeyLoadSettle is the floor wait after a load hotkey: the core
	// restores the state and re-renders one frame. The perceptual proof is
	// the caller's (it screenshots around the call); this is a floor.
	hotkeyLoadSettle  = 3 * time.Second
	statePollInterval = 250 * time.Millisecond
	// hotkeyHold is the keydown→keyup hold: the hotkey scheduler samples at
	// 5 ms (HotkeyScheduler::ProcessThread), so a single press spans many
	// samples and cannot be missed.
	hotkeyHold = 150 * time.Millisecond
)

// hotkeyKeyFor maps a savestate slot to the X11 keysym bound to it in
// Hotkeys.ini. Only slot 1 is bound (the drive test and the fixture
// recorder use it); slots 2-4 are unbound.
func hotkeyKeyFor(slot int, save bool) (string, error) {
	if slot < saveSlotMin || slot > saveSlotMax {
		return "", fmt.Errorf("dolphin: savestate slot %d out of range %d..%d", slot, saveSlotMin, saveSlotMax)
	}
	if slot != saveSlotMin {
		return "", fmt.Errorf("dolphin: savestate slot %d is not bound in the keyboard layout (only slot %d)", slot, saveSlotMin)
	}
	if save {
		return saveHotkeyKey, nil
	}
	return loadHotkeyKey, nil
}

// hotkeysIni is the <user>/Config/Hotkeys.ini content for the keyboard
// device (approach D, ADR-0043 am7): Device = the X11 keyboard device the
// 2609 XInput2 backend resolves keyboard input to (hotkeyKeyboardDevice —
// measured from the exit-rewritten spike user dir, where every keyboard
// controller's Device line carries exactly this string), plus the slot-1
// save/load KEY bindings. The control names are X11 keysyms (the same
// spelling the exit-rewritten keyboard controller configs use: Keys/A = A,
// Keys/PGUP = Prior — the backtick-wrapped single-key form 2609's hotkey
// parser accepts). An unresolvable Device line is dropped silently by the
// parser, so the exit rewrite of this file (checkExitRewrite at Stop) is
// the structural check that the device string resolved.
func hotkeysIni(dev string) string {
	var b strings.Builder
	b.WriteString("[Hotkeys]\n")
	b.WriteString("Device = " + dev + "\n")
	b.WriteString("Save State/Save State Slot 1 = `" + saveHotkeyKey + "`\n")
	b.WriteString("Load State/Load State Slot 1 = `" + loadHotkeyKey + "`\n")
	return b.String()
}

// stateSlotPath returns the state file for slot (".../StateSaves/
// <GAMEID>.sNN") if present. The glob tolerates the title's GAMEID prefix:
// the bridge does not know the image's ID before boot (2609 MakeStateFilename
// uses the booted game's GameID field). The folder is stateSavesDir —
// measured from 2609's own user dir on this host, not the legacy "states"
// spelling.
func (b *bridge) stateSlotPath(slot int) (string, error) {
	b.mu.Lock()
	userDir := b.st.UserDir
	b.mu.Unlock()
	if userDir == "" {
		return "", ErrNotLaunched
	}
	if slot < saveSlotMin || slot > saveSlotMax {
		return "", fmt.Errorf("dolphin: savestate slot %d out of range %d..%d", slot, saveSlotMin, saveSlotMax)
	}
	matches, err := filepath.Glob(filepath.Join(userDir, stateSavesDir, fmt.Sprintf("*.s%02d", slot)))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", nil
	}
	// At most one title is loaded per core; multiple matches mean stale
	// files from earlier launches — the newest wins.
	path := matches[0]
	best, errBest := os.Stat(path)
	if errBest != nil {
		return path, nil
	}
	for _, m := range matches[1:] {
		st, err := os.Stat(m)
		if err != nil {
			continue
		}
		if st.ModTime().After(best.ModTime()) {
			path, best = m, st
		}
	}
	return path, nil
}

// hotkeyOK reports the running state required to fire a hotkey key: the
// launch is running (the scheduler polls only while the core is up), on the
// opt-in path (Hotkeys.ini exists), on a display this bridge can address,
// with the SendKey seam wired (real xdotool or a test fake).
func (b *bridge) hotkeyOK() error {
	b.mu.Lock()
	state := b.st.State
	display := b.st.Display
	sendKey := b.opts.SendKey
	b.mu.Unlock()
	if state != StateRunning {
		return ErrNotRunning
	}
	if sendKey == nil {
		return fmt.Errorf("dolphin: no SendKey seam wired (xdotool?) for the savestate hotkeys: %w", ErrHotkeysDisabled)
	}
	if display == "" {
		return fmt.Errorf("dolphin: no display recorded; cannot inject the hotkey key: %w", ErrNotRunning)
	}
	return nil
}

// injectHotkeyKey presses (and releases) one X11 key into the display with
// XTest fake input (Options.SendKey — real xdotool in production). The hold
// (hotkeyHold) spans many of the scheduler's 5 ms samples, so a single press
// is not missed. The controller FIFO is untouched: the key event lands in
// the X server's key state, which the hotkey scheduler's own thread polls
// (approach D, ADR-0043 am7 — the single-consumer controller pipe is
// preserved by construction).
func (b *bridge) injectHotkeyKey(ctx context.Context, key string) error {
	if err := b.hotkeyOK(); err != nil {
		return err
	}
	b.mu.Lock()
	display := b.st.Display
	sendKey := b.opts.SendKey
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	b.log.Info("dolphin: injecting savestate hotkey key", "key", key, "display", display, "hold", hotkeyHold)
	if err := sendKey(display, key, hotkeyHold); err != nil {
		return fmt.Errorf("dolphin: hotkey key %s: %w", key, err)
	}
	return nil
}

// hotkeyEnabled reads whether the running launch opted in to the savestate
// hotkeys (ADR-0043): only an opt-in launch writes Hotkeys.ini and binds the
// keyboard keys; the default launch's keys are unbound and SaveState/
// LoadState refuse with ErrHotkeysDisabled.
func (b *bridge) hotkeyEnabled() bool {
	b.mu.Lock()
	enabled := b.spec.Hotkeys
	b.mu.Unlock()
	return enabled
}

// SaveState triggers a savestate save of slot over the keyboard hotkey (F1,
// XTest-injected) and waits for the state file on disk, returning its path
// (the value a --save_state boot takes). Freshness is verified against the
// mtime seen before the hotkey — that is the "the save actually happened"
// half of the round trip; the perceptual half is the caller's (LoadState +
// frames). Refuses with ErrHotkeysDisabled when the launch did not opt in.
func (b *bridge) SaveState(ctx context.Context, slot int) (string, error) {
	if !b.hotkeyEnabled() {
		return "", ErrHotkeysDisabled
	}
	key, err := hotkeyKeyFor(slot, true)
	if err != nil {
		return "", err
	}
	pre, err := b.stateSlotPath(slot)
	var preMtime time.Time
	if err == nil && pre != "" {
		if st, statErr := os.Stat(pre); statErr == nil {
			preMtime = st.ModTime()
		}
	}
	if err := b.injectHotkeyKey(ctx, key); err != nil {
		return "", err
	}
	deadline := time.Now().Add(hotkeySaveTimeout)
	for {
		path, err := b.stateSlotPath(slot)
		if err == nil && path != "" {
			if st, statErr := os.Stat(path); statErr == nil && st.ModTime().After(preMtime) {
				b.log.Info("dolphin: savestate saved", "slot", slot, "path", path)
				return path, nil
			}
		}
		if time.Now().After(deadline) {
			// Evidence for the trace: what the StateSaves dir holds, if anything.
			b.mu.Lock()
			userDir := b.st.UserDir
			b.mu.Unlock()
			entries, _ := filepath.Glob(filepath.Join(userDir, stateSavesDir, "*"))
			return "", fmt.Errorf("dolphin: savestate slot %d not written within %s (StateSaves dir: %v)",
				slot, hotkeySaveTimeout, entries)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(statePollInterval):
		}
	}
}

// LoadState triggers a savestate load of slot over the keyboard hotkey (F2,
// XTest-injected) and waits the settle floor. The round-trip proof (screen X
// before, advance to Y, load, screen X again) is assembled by callers out of
// SaveState, Inject and Screenshot — this method only fires the hotkey and
// paces it. Refuses with ErrHotkeysDisabled when the launch did not opt in.
func (b *bridge) LoadState(ctx context.Context, slot int) error {
	if !b.hotkeyEnabled() {
		return ErrHotkeysDisabled
	}
	key, err := hotkeyKeyFor(slot, false)
	if err != nil {
		return err
	}
	if err := b.injectHotkeyKey(ctx, key); err != nil {
		return err
	}
	b.log.Info("dolphin: savestate load triggered", "slot", slot)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(hotkeyLoadSettle):
	}
	return nil
}
