package dolphin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Savestate hotkeys ride the SECOND Pipes device (Pipes/hotkeys,
// ADR-0043) and are OPT-IN: the launch must set LaunchSpec.Hotkeys=true, or
// SaveState/LoadState refuse with ErrHotkeysDisabled (the default
// single-pipe path is the proven one). Slots 1-4 are bound to pipe buttons;
// save and load live in distinct groups so one device carries both. The 2609
// HotkeyManager labels (groups "Save State"/"Load State", keys
// "Save State Slot N", HotkeyManager.cpp s_hotkey_labels) are the schema
// Config/Hotkeys.ini parses; a bare "`Button X`" control is a single-button
// binding (the default GBA hotkeys use the same backtick-wrapped single-control
// form).
const (
	saveSlotMin = 1
	saveSlotMax = 4

	// hotkeyConnectWait bounds the wait for the hotkey FIFO's connect edge
	// (Dolphin opens every file under <user>/Pipes at boot, same as the
	// game pad's pipe; the 30 s bound is pipeConnectWait's).
	hotkeyConnectWait = pipeConnectWait
	// hotkeySaveTimeout bounds the wait for the state file to appear
	// (a GameCube save dumps the whole core; generous at llvmpipe speed).
	hotkeySaveTimeout = 30 * time.Second
	// hotkeyLoadSettle is the floor wait after a load hotkey: the core
	// restores the state and re-renders one frame. The perceptual proof is
	// the caller's (it screenshots around the call); this is a floor.
	hotkeyLoadSettle  = 3 * time.Second
	statePollInterval = 250 * time.Millisecond
	hotkeyHold        = 150 * time.Millisecond // the scheduler samples at 5 ms
)

// hotkeyButtonFor maps a savestate slot to the pipe button bound to it.
// Save slots ride A/B/X/Y, load slots Z/L/R/START. The two pipes are
// separate devices, so the hotkey START does not touch the game pad's
// START binding.
func hotkeyButtonFor(slot int, save bool) (Button, error) {
	if slot < saveSlotMin || slot > saveSlotMax {
		return "", fmt.Errorf("dolphin: savestate slot %d out of range %d..%d", slot, saveSlotMin, saveSlotMax)
	}
	var tokens [saveSlotMax - saveSlotMin + 1]Button
	if save {
		tokens = [saveSlotMax - saveSlotMin + 1]Button{ButtonA, ButtonB, ButtonX, ButtonY}
	} else {
		tokens = [saveSlotMax - saveSlotMin + 1]Button{ButtonZ, ButtonL, ButtonR, ButtonStart}
	}
	return tokens[slot-saveSlotMin], nil
}

// hotkeysIni is the <user>/Config/Hotkeys.ini content for one measured
// device string ("Pipe/<i>/hotkeys" from the raw readdir order — ADR-0043).
// A device name that does not exist is ignored by the parser, so the index
// is measured from the directory rather than assumed; the real test
// re-verifies it against the exit rewrite of this same file.
func hotkeysIni(dev string) string {
	var b strings.Builder
	b.WriteString("[Hotkeys]\n")
	b.WriteString("Device = " + dev + "\n")
	for slot := saveSlotMin; slot <= saveSlotMax; slot++ {
		btn, _ := hotkeyButtonFor(slot, true)
		fmt.Fprintf(&b, "Save State/Save State Slot %d = `Button %s`\n", slot, btn)
	}
	for slot := saveSlotMin; slot <= saveSlotMax; slot++ {
		btn, _ := hotkeyButtonFor(slot, false)
		fmt.Fprintf(&b, "Load State/Load State Slot %d = `Button %s`\n", slot, btn)
	}
	return b.String()
}

// stateSlotPath returns the state file for slot (".../states/<GAMEID>.sNN")
// if present. The glob tolerates the title's GAMEID prefix: the bridge does
// not know the image's ID before boot (2609 MakeStateFilename uses the
// booted game's GameID field).
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
	matches, err := filepath.Glob(filepath.Join(userDir, "states", fmt.Sprintf("*.s%02d", slot)))
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

// hotkeyOK reports the running state + a connected hotkey pipe, reading the
// bridge exactly like Inject does.
func (b *bridge) hotkeyOK() error {
	b.mu.Lock()
	state := b.st.State
	hk := b.hotkeyPipe
	b.mu.Unlock()
	if state != StateRunning {
		return ErrNotRunning
	}
	if hk == nil {
		return fmt.Errorf("dolphin: hotkey pipe not opened: %w", ErrPipeNotConnected)
	}
	if !hk.connected.Load() {
		return fmt.Errorf("dolphin: hotkey pipe %s not connected yet: %w", hotkeyPipeFile, ErrPipeNotConnected)
	}
	return nil
}

// waitHotkeyPipe bounds the wait for the hotkey FIFO's connect edge (the
// same contract as the game pad's pipe; the emulator opens both at boot).
func (b *bridge) waitHotkeyPipe(ctx context.Context) error {
	if err := b.hotkeyOK(); err != nil {
		// A not-connected pipe is the only transient condition worth waiting on.
		if err != ErrPipeNotConnected && !isPipeErr(err) {
			return err
		}
	}
	b.mu.Lock()
	hk := b.hotkeyPipe
	b.mu.Unlock()
	if hk == nil {
		return fmt.Errorf("dolphin: hotkey pipe not opened: %w", ErrPipeNotConnected)
	}
	if !hk.waitConnected(ctx, hotkeyConnectWait) {
		return fmt.Errorf("dolphin: hotkey pipe %s not connected within %s", hotkeyPipeFile, hotkeyConnectWait)
	}
	return nil
}

// isPipeErr reports a wrapped pipe-not-connected error.
func isPipeErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not connected")
}

// injectHotkey presses (and releases) one button on the hotkey pipe. The
// hold (150 ms) spans many of the scheduler's 5 ms samples (2609
// HotkeyScheduler::ProcessThread), so a single press is not missed.
func (b *bridge) injectHotkey(ctx context.Context, btn Button, hold time.Duration) error {
	if err := b.hotkeyOK(); err != nil {
		return err
	}
	b.mu.Lock()
	hk := b.hotkeyPipe
	b.mu.Unlock()
	if err := hk.writeLine(ctx, "PRESS "+string(btn)); err != nil {
		return fmt.Errorf("dolphin: hotkey press %s: %w", btn, err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(hold):
	}
	if err := hk.writeLine(ctx, "RELEASE "+string(btn)); err != nil {
		return fmt.Errorf("dolphin: hotkey release %s: %w", btn, err)
	}
	return nil
}

// hotkeyEnabled reads whether the running launch opted in to the savestate
// hotkey FIFO (ADR-0043: the default single-pipe launch has none).
func (b *bridge) hotkeyEnabled() bool {
	b.mu.Lock()
	enabled := b.spec.Hotkeys
	b.mu.Unlock()
	return enabled
}

// SaveState triggers a savestate save of slot over the hotkey pipe and
// waits for the state file on disk, returning its path (the value a
// --save_state boot takes). Freshness is verified against the mtime seen
// before the hotkey — that is the "the save actually happened" half of the
// round trip; the perceptual half is the caller's (LoadState + frames).
// Refuses with ErrHotkeysDisabled when the launch did not opt in.
func (b *bridge) SaveState(ctx context.Context, slot int) (string, error) {
	if !b.hotkeyEnabled() {
		return "", ErrHotkeysDisabled
	}
	if err := b.waitHotkeyPipe(ctx); err != nil {
		return "", err
	}
	btn, err := hotkeyButtonFor(slot, true)
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
	if err := b.injectHotkey(ctx, btn, hotkeyHold); err != nil {
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
			// Evidence for the trace: what the states dir holds, if anything.
			b.mu.Lock()
			userDir := b.st.UserDir
			b.mu.Unlock()
			entries, _ := filepath.Glob(filepath.Join(userDir, "states", "*"))
			return "", fmt.Errorf("dolphin: savestate slot %d not written within %s (states dir: %v)",
				slot, hotkeySaveTimeout, entries)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(statePollInterval):
		}
	}
}

// LoadState triggers a savestate load of slot over the hotkey pipe and
// waits the settle floor. The round-trip proof (screen X before, advance to
// Y, load, screen X again) is assembled by callers out of SaveState,
// Inject and Screenshot — this method only fires the hotkey and paces it.
// Refuses with ErrHotkeysDisabled when the launch did not opt in.
func (b *bridge) LoadState(ctx context.Context, slot int) error {
	if !b.hotkeyEnabled() {
		return ErrHotkeysDisabled
	}
	if err := b.waitHotkeyPipe(ctx); err != nil {
		return err
	}
	btn, err := hotkeyButtonFor(slot, false)
	if err != nil {
		return err
	}
	if err := b.injectHotkey(ctx, btn, hotkeyHold); err != nil {
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
