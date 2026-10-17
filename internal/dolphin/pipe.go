package dolphin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The controller channel. Upstream Pipes::Init opens every file in
// <user>/Pipes/ read-only; a write to it is a controller command (newline
// terminated, case sensitive — verified against upstream Pipes.cpp and the
// spike's on-disk protocol):
//
//	PRESS|RELEASE {A,B,X,Y,Z,START,L,R,D_UP,D_DOWN,D_LEFT,D_RIGHT}
//	SET {L,R} [0,1]        trigger, 0 = released, 1 = full
//	SET {MAIN,C} [0,1] [0,1]  sticks, 0.5 = centred
//
// Two invariants from the spike are structural here, not comments:
//
//   - a PERSISTENT writer keeps one FIFO end open for the bridge's whole
//     lifetime, because Dolphin's read end must never see EOF (a closing
//     writer makes the device vanish);
//   - every command is a fresh O_WRONLY open + write + close. The open blocks
//     until a reader exists, so a command written before Dolphin bound the
//     device would block the caller forever — which is why writeLine bounds
//     the open with a timeout instead of trusting the OS.
//
// The channel has no acknowledgement back: "Dolphin received my bytes" is not
// observable. That is why every driven scenario must be proved with a scene
// predicate (ProveInput), never with "the write succeeded" (ADR-0007).

// pipeWriteTimeout bounds one command's FIFO open. The spike used 10 s.
const pipeWriteTimeout = 10 * time.Second

// pipeConnectWait bounds how long a readiness check (ProveInput) waits for
// the persistent writer to connect — i.e. for Dolphin to open the FIFO's
// read end during boot. Inject does not use this: its write open is already
// bounded by pipeWriteTimeout, and a 50 ms hold should not sit in a boot
// race.
const pipeConnectWait = 30 * time.Second

// pipeFIFO owns one controller pipe in the isolated user dir.
type pipeFIFO struct {
	path string

	mu        sync.Mutex // serialises command writes
	writer    *os.File   // the persistent end; nil until connected
	connected atomic.Bool
	closed    atomic.Bool
}

// openPipe prepares the FIFO file (idempotent; makeFIFO refuses to replace a
// non-FIFO path) and returns the owner. Nothing is opened yet: connect does
// the blocking open, in a goroutine, because Dolphin is the one that opens
// the other end.
func openPipe(userDir string) (*pipeFIFO, error) {
	p := &pipeFIFO{path: filepath.Join(userDir, "Pipes", pipeFileName)}
	if err := makeFIFO(p.path); err != nil {
		return nil, err
	}
	return p, nil
}

// connect opens the persistent writer end in a background goroutine. The
// open(2) on a FIFO write-end blocks until Dolphin opens the read end, so the
// connect is a fact, not a hope: connected flips only when the blocking open
// returned, which is the spike's /proc fd-proof expressed without /proc.
func (p *pipeFIFO) connect(ctx context.Context) {
	go func() {
		if p.closed.Load() {
			return
		}
		f, err := os.OpenFile(p.path, os.O_WRONLY, 0)
		if err != nil {
			return // closed or the reader side is gone; Inject will say so
		}
		p.mu.Lock()
		if p.closed.Load() {
			p.mu.Unlock()
			f.Close()
			return
		}
		p.writer = f
		p.mu.Unlock()
		p.connected.Store(true)
		_ = ctx
	}()
}

// waitConnected polls until the persistent writer has connected (the reader
// opened the FIFO) or the context expires. ProveInput uses it to ride out
// the boot race between Launch and Dolphin binding the controller device.
func (p *pipeFIFO) waitConnected(ctx context.Context, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for {
		if p.connected.Load() {
			return true
		}
		if p.closed.Load() {
			return false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		wait := 200 * time.Millisecond
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
	}
}

// writeLine sends one command. A fresh O_WRONLY open per command mirrors the
// spike's `printf ... > pipe` exactly (one writer per line, closed after),
// so the bytes on the wire are identical to the proven run.
func (p *pipeFIFO) writeLine(ctx context.Context, line string) error {
	if p.closed.Load() {
		return fmt.Errorf("dolphin: the controller pipe is closed: %w", ErrNotLaunched)
	}
	if !p.connected.Load() {
		return fmt.Errorf("dolphin: controller pipe not connected yet: %w", ErrPipeNotConnected)
	}
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(p.path, os.O_WRONLY, 0)
		if err != nil {
			done <- fmt.Errorf("dolphin: open controller pipe for %q: %w: %v", line, ErrPipeNotConnected, err)
			return
		}
		if _, err := f.WriteString(line + "\n"); err != nil {
			f.Close()
			done <- fmt.Errorf("dolphin: write %q to controller pipe: %w", line, err)
			return
		}
		f.Close()
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("dolphin: timed out writing %q to the controller pipe: %w", line, ctx.Err())
	case <-time.After(pipeWriteTimeout):
		return fmt.Errorf("dolphin: timed out after %s writing %q to the controller pipe (Dolphin has not opened it?): %w",
			pipeWriteTimeout, line, ErrPipeNotConnected)
	}
}

// close shuts the persistent end and removes the FIFO. Idempotent. It runs
// after Dolphin is stopped in Stop's ordering, so no writer is left holding
// a device the next run wants to recreate.
func (p *pipeFIFO) close() {
	p.closed.Store(true)
	p.connected.Store(false)
	p.mu.Lock()
	if p.writer != nil {
		_ = p.writer.Close()
		p.writer = nil
	}
	p.mu.Unlock()
	_ = os.Remove(p.path)
}

// buttonOrder is the fixed PRESS/RELEASE order so the wire bytes are
// deterministic for a given Inject call (tests assert the exact sequence).
var buttonOrder = []struct {
	field func(Buttons) bool
	token string
}{
	{func(b Buttons) bool { return b.A }, "A"},
	{func(b Buttons) bool { return b.B }, "B"},
	{func(b Buttons) bool { return b.X }, "X"},
	{func(b Buttons) bool { return b.Y }, "Y"},
	{func(b Buttons) bool { return b.Z }, "Z"},
	{func(b Buttons) bool { return b.Start }, "START"},
	{func(b Buttons) bool { return b.L }, "L"},
	{func(b Buttons) bool { return b.R }, "R"},
	{func(b Buttons) bool { return b.Up }, "D_UP"},
	{func(b Buttons) bool { return b.Down }, "D_DOWN"},
	{func(b Buttons) bool { return b.Left }, "D_LEFT"},
	{func(b Buttons) bool { return b.Right }, "D_RIGHT"},
}

// pressedTokens lists the buttons to act on, in buttonOrder.
func pressedTokens(b Buttons) []string {
	var out []string
	for _, e := range buttonOrder {
		if e.field(b) {
			out = append(out, e.token)
		}
	}
	return out
}

// clamp01 clamps a float into [0,1].
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// f3 renders the pipe-protocol value with three decimals (the protocol
// strtod's; finer precision is noise at llvmpipe cadence).
func f3(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
}

// axisCommands renders the SET commands for non-neutral axes. Stick values
// arrive -1..1 and are mapped to the protocol's 0..1 (0.5 = centred);
// triggers are already 0..1 (0 = released).
func axisCommands(a Axes) []string {
	var out []string
	if a.MainX != 0 || a.MainY != 0 {
		out = append(out, "SET MAIN "+f3(clamp01((a.MainX+1)/2))+" "+f3(clamp01((a.MainY+1)/2)))
	}
	if a.StickX != 0 || a.StickY != 0 {
		out = append(out, "SET C "+f3(clamp01((a.StickX+1)/2))+" "+f3(clamp01((a.StickY+1)/2)))
	}
	if a.L != 0 {
		out = append(out, "SET L "+f3(clamp01(a.L)))
	}
	if a.R != 0 {
		out = append(out, "SET R "+f3(clamp01(a.R)))
	}
	return out
}

// axisResetCommands returns the axes to neutral, so a held Inject does not
// leave the stick off-centre after it finishes.
func axisResetCommands() []string {
	return []string{"SET MAIN 0.5 0.5", "SET C 0.5 0.5", "SET L 0", "SET R 0"}
}
