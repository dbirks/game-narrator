package ocr

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeEngine writes a fake tesseract into a fresh dir and returns its path.
// It mimics the real engine's shape — the thing ADR-0100's mechanism depends
// on and the tests must exercise rather than assert:
//
//  1. at SPAWN: record argv, then (if FAKE_LOAD_S is set) sleep — the
//     simulated model load, which the real engine pays before the first image
//     arrives;
//  2. block on stdin for the image (cat to EOF), exactly as tesseract blocks
//     after its fixed startup;
//  3. on the image: if the FAKE_CRASHES counter is armed, self-SIGKILL (the
//     measured mid-read crash shape); else if FAKE_HOLDS is armed, block
//     (the deadline-ignoring engine); otherwise print the TSV and exit.
//
// One process serves exactly one image, like the real engine: the pool's
// replacement is what the next read finds.
func fakeEngine(t *testing.T) string {
	t.Helper()
	body := "#!/bin/sh\n"
	body += "if [ -n \"$FAKE_ARGV\" ]; then printf '%s\\n' \"$@\" >> \"$FAKE_ARGV\"; fi\n"
	body += "if [ -n \"$FAKE_PIDS\" ]; then echo $$ >> \"$FAKE_PIDS\"; fi\n"
	body += "if [ -n \"$FAKE_LOAD_S\" ]; then sleep \"$FAKE_LOAD_S\"; fi\n"
	body += "cat > /dev/null\n"
	body += "if [ -n \"$FAKE_CRASHES\" ] && [ -f \"$FAKE_CRASHES\" ]; then\n"
	body += "  n=$(cat \"$FAKE_CRASHES\" 2>/dev/null || echo 0)\n"
	body += "  if [ \"$n\" -gt 0 ]; then echo $((n-1)) > \"$FAKE_CRASHES\"; kill -KILL $$; exit 1; fi\n"
	body += "fi\n"
	body += "if [ -n \"$FAKE_HOLDS\" ] && [ -f \"$FAKE_HOLDS\" ]; then\n"
	body += "  n=$(cat \"$FAKE_HOLDS\" 2>/dev/null || echo 0)\n"
	body += "  if [ \"$n\" -gt 0 ]; then echo $((n-1)) > \"$FAKE_HOLDS\"; sleep 30; fi\n"
	body += "fi\n"
	body += "printf 'level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n5\t1\t1\t1\t1\t1\t1\t1\t4\t4\t99\thello\n'"
	stub := filepath.Join(t.TempDir(), "fake-tesseract")
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return stub
}

// poolTinyImage is any non-empty frame; the stubs do not read pixels.
func poolTinyImage() image.Image {
	return image.NewGray(image.Rect(0, 0, 8, 8))
}

// setCounter arms a counter file for the stub's crash/hold behaviour.
func setCounter(t *testing.T, name string, n int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strconv.Itoa(n)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(name, path)
}

// processGone reports whether pid has exited — including as a zombie (killed,
// not yet reaped): a SIGKILLed process lingers as a zombie until the pool's
// watcher reaps it, and syscall.Kill(pid, 0) succeeds on a zombie, so the
// liveness probe alone cannot tell "killed, pending reap" from "alive".
// It is Linux-shaped (/proc); on other hosts it degrades to the probe.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return true // ESRCH: reaped (or never ran)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // gone between the probe and the read
	}
	// /proc/<pid>/stat: "pid (comm) state ..." — comm may contain spaces and
	// parentheses, so the state is the first field after the last ')'.
	fields := strings.Split(string(data), ") ")
	if len(fields) < 2 {
		return false
	}
	return strings.HasPrefix(fields[1], "Z")
}

// TestPooledReadsThroughTheSlotProtocol proves the warm-pool protocol with a
// fake engine (ADR-0100): the argv is `stdin stdout <engine args>`, the crop
// arrives on the slot's stdin (the stub drains it exactly as tesseract does),
// the TSV on stdout is parsed, and — the issue's point — the model is loaded
// across reads: both reads settle in well under the stub's simulated model
// load, i.e. they ran on processes spawned and loading BEFORE the read
// arrived. A cold (per-process) read would pay the load inline.
func TestPooledReadsThroughTheSlotProtocol(t *testing.T) {
	argPath := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_ARGV", argPath)
	t.Setenv("FAKE_LOAD_S", "0.5") // 500 ms simulated model load at spawn
	stub := fakeEngine(t)

	pool := NewPooled(Tesseract{Path: stub, Timeout: 5 * time.Second}, 2)
	defer pool.Close()

	// Let both slots finish their simulated model load: the point of the pool
	// is that the read finds the model already loaded.
	time.Sleep(800 * time.Millisecond)

	// Read 1: on a warm slot, so its cost must be under the 500 ms load.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := pool.Recognize(ctx, poolTinyImage())
	if err != nil {
		t.Fatalf("read 1: %v", err)
	}
	if res.Text != "hello" {
		t.Errorf("read 1 text = %q, want hello", res.Text)
	}
	if res.Engine != "tesseract" {
		t.Errorf("read 1 engine = %q, want tesseract (the provenance)", res.Engine)
	}
	if res.Duration >= 500*time.Millisecond {
		t.Errorf("read 1 took %s: the slot was not warm (want < the 500 ms simulated load)", res.Duration)
	}

	// The replacement for read 1's consumed slot loads in the read-free gap;
	// give it its 500 ms load, then read 2 must also run warm.
	time.Sleep(800 * time.Millisecond)
	res2, err := pool.Recognize(ctx, poolTinyImage())
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if res2.Text != "hello" {
		t.Errorf("read 2 text = %q, want hello", res2.Text)
	}
	if res2.Duration >= 500*time.Millisecond {
		t.Errorf("read 2 took %s: the replacement slot was not warm (want < the 500 ms simulated load)", res2.Duration)
	}

	// The argv contract: stdin as the input operand, stdout as the output,
	// then the engine flags — ADR-0012's shape with the pool's input route.
	raw, err := os.ReadFile(argPath)
	if err != nil {
		t.Fatalf("stub did not record argv: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	joined := strings.Join(lines, " ")
	for _, want := range []string{"stdin", "stdout", "--psm 6", "-l eng", "tsv"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", joined, want)
		}
	}
	for _, a := range lines {
		// argv-only, no shell (ADR-0012 §1), asserted on the actual argv.
		if a == "sh" || a == "bash" || a == "-c" {
			t.Errorf("argv contains a shell token: %q", a)
		}
	}
}

// TestPooledCrashRecoversWithBackoff proves the crash path (ADR-0100
// decision 3): a slot that dies mid-read (SIGKILL, the measured board shape)
// yields a typed ErrFailed, the replacement serves the next read (a single
// crash costs no read), and after DefaultDegradeFails consecutive failures
// the pool stops spending reads on churn and the per-process fallback serves.
func TestPooledCrashRecoversWithBackoff(t *testing.T) {
	t.Setenv("FAKE_ARGV", filepath.Join(t.TempDir(), "argv"))
	setCounter(t, "FAKE_CRASHES", 3)
	stub := fakeEngine(t)

	pool := NewPooled(Tesseract{Path: stub, Timeout: 5 * time.Second}, 1)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Reads 1-3: the stub self-SIGKILLs (3 armed failures). Each failure gets
	// a replacement (consecFails < 4); the sleeps let the backoff elapse and
	// the next read's nudge spawn the replacement.
	for i := 1; i <= 3; i++ {
		_, err := pool.Recognize(ctx, poolTinyImage())
		var f *Failure
		if !errors.As(err, &f) {
			t.Fatalf("read %d: want *Failure, got %T (%v)", i, err, err)
		}
		if !errors.Is(f.Kind, ErrFailed) {
			t.Errorf("read %d: failure kind = %v, want ErrFailed (the mid-read crash)", i, f.Kind)
		}
		time.Sleep(300 * time.Millisecond)
	}
	// Read 4: the counter is spent — the replacement serves with the TSV.
	res, err := pool.Recognize(ctx, poolTinyImage())
	if err != nil {
		t.Fatalf("read 4 (recovery after 3 crashes): %v", err)
	}
	if res.Text != "hello" {
		t.Errorf("read 4 text = %q, want hello", res.Text)
	}
}

// TestPooledDegradedPoolFallsBackToPerProcess proves the degrade + fallback
// + recovery cycle: four consecutive crashes degrade the pool (no more
// spawn-and-crash churn per read), the per-process fallback then serves the
// reads (the wrapper's typed failures, not the pool's), and once the backoff
// elapses the pool warms a replacement and a slot read clears the
// degradation.
func TestPooledDegradedPoolFallsBackToPerProcess(t *testing.T) {
	t.Setenv("FAKE_ARGV", filepath.Join(t.TempDir(), "argv"))
	stub := fakeEngine(t)

	// Arm 5 crashes BEFORE the pool spawns its warm slots: the stubs inherit
	// the environment at spawn, so the counter must be visible then (the
	// check itself happens at image time, after the crop arrives). Four of
	// them degrade the pool; the fifth (and any later) read must come from
	// the per-process fallback, which consumes the last one.
	setCounter(t, "FAKE_CRASHES", 5)

	pool := NewPooled(Tesseract{Path: stub, Timeout: 5 * time.Second}, 1)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Reads 1-4: consecutive slot failures degrade the pool. The settle
	// sleeps let each replacement spawn and its slot warm, so the next read
	// is the pool's and not a fallback detour.
	for i := 1; i <= DefaultDegradeFails; i++ {
		_, err := pool.Recognize(ctx, poolTinyImage())
		var f *Failure
		if !errors.As(err, &f) {
			t.Fatalf("degrade read %d: want *Failure, got %T (%v)", i, err, err)
		}
		if !errors.Is(f.Kind, ErrFailed) {
			t.Errorf("degrade read %d: failure kind = %v, want ErrFailed", i, f.Kind)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Degraded: the next read is the per-process fallback. The counter is
	// still armed (5-4=1): the fallback's stub crashes too — the typed
	// failure is the wrapper's per-process one, which is the point: a broken
	// engine fails the way ADR-0012 always made it fail, and the pool is not
	// burning the read on spawn churn.
	_, err := pool.Recognize(ctx, poolTinyImage())
	var f *Failure
	if !errors.As(err, &f) || !errors.Is(f.Kind, ErrFailed) {
		t.Fatalf("degraded read: want the per-process ErrFailed, got %v", err)
	}
	// Wait out the 4th failure's backoff (1.6 s), then the pool's nudge
	// warms a replacement and the counter is spent: a SLOT read settles and
	// clears the degradation (automatic recovery, ADR-0100 decision 3). A
	// slot read is the only path that clears consecFails — the fallback does
	// not — so the final assertion below proves the pool, not the fallback,
	// recovered. Retry briefly: if the settle timing left one crash pending
	// in the counter, the retry spends it and the next settles.
	time.Sleep(1700 * time.Millisecond)
	var res Result
	for i := 0; ; i++ {
		res, err = pool.Recognize(ctx, poolTinyImage())
		if err == nil {
			break
		}
		if i >= 5 {
			t.Fatalf("post-degrade recovery did not settle after %d reads: last error %v", i+1, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if res.Text != "hello" {
		t.Errorf("post-degrade text = %q, want hello", res.Text)
	}
	pool.mu.Lock()
	healed := pool.consecFails == 0
	pool.mu.Unlock()
	if !healed {
		t.Error("the pool is still degraded after the recovery read: a slot read must clear the degradation")
	}
	// The pool is healed: the next read is warm and the counter is spent.
	res, err = pool.Recognize(ctx, poolTinyImage())
	if err != nil || res.Text != "hello" {
		t.Fatalf("healed pool read: %v (text %q)", err, res.Text)
	}
}

// TestPooledHealsAfterDegradation proves the other half of the degradation
// contract (ADR-0100 decision 3): a pool that degraded on a crash loop
// keeps PROBING the engine on its backoff — every backoff window a nudge
// warms a replacement slot, and the next read consumes it (a warm slot is
// always preferred, even by a degraded pool: only a slot read can clear the
// degradation) — so when the engine heals (fixed tessdata, the operator's
// fix), the probe read settles, consecFails clears, and the pool is fully
// pool again without an operator restart. The reads between the probes are
// served by the per-process fallback, which is how the operator gets the
// typed failure a broken engine always had.
func TestPooledHealsAfterDegradation(t *testing.T) {
	setCounter(t, "FAKE_CRASHES", 6)
	stub := fakeEngine(t)

	pool := NewPooled(Tesseract{Path: stub, Timeout: 5 * time.Second}, 1)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Degrade the pool: four consecutive slot failures (the counter drops
	// 6→2). The settle sleeps let each replacement spawn and warm.
	for i := 1; i <= DefaultDegradeFails; i++ {
		if _, err := pool.Recognize(ctx, poolTinyImage()); err == nil {
			t.Fatalf("degrade read %d: want failure, got success", i)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Two armed crashes remain, and the backoff re-arms on every probe
	// failure: wait out each 1.6 s window, probe with the pool's own read
	// (the nudge warms the probe slot inside it), and watch the counter
	// spend the last two crashes. The engine heals after the second probe.
	for probe := 1; probe <= 2; probe++ {
		time.Sleep(1700 * time.Millisecond)
		res, err := pool.Recognize(ctx, poolTinyImage())
		var f *Failure
		if !errors.As(err, &f) || !errors.Is(f.Kind, ErrFailed) {
			t.Fatalf("probe %d: want the crash-loop ErrFailed, got %v (text %q)", probe, err, res.Text)
		}
		pool.mu.Lock()
		cf := pool.consecFails
		pool.mu.Unlock()
		if cf < DefaultDegradeFails {
			t.Fatalf("probe %d: consecFails = %d, want ≥ %d (the crash loop still counts)", probe, cf, DefaultDegradeFails)
		}
	}

	// The engine is healed: the next probe (another backoff window later) is
	// a SLOT read — the warm slot the nudge warmed settles, and the slot
	// success clears the degradation. Only a slot read can do that: the
	// fallback's success never touches the pool's bookkeeping.
	time.Sleep(1700 * time.Millisecond)
	res, err := pool.Recognize(ctx, poolTinyImage())
	if err != nil || res.Text != "hello" {
		t.Fatalf("heal probe: want a warm slot read of the healed engine, got %v (text %q)", err, res.Text)
	}
	pool.mu.Lock()
	healed := pool.consecFails == 0
	pool.mu.Unlock()
	if !healed {
		t.Error("the pool is still degraded after the healed engine's probe: the slot success must clear the degradation")
	}
	// And it is pool again: the next read is warm.
	res, err = pool.Recognize(ctx, poolTinyImage())
	if err != nil || res.Text != "hello" {
		t.Fatalf("post-heal read: %v (text %q)", err, res.Text)
	}
}

// TestPooledDeadlineKillsOnlyTheCallerSlot proves the deadline contract
// (ADR-0012 §2 via the pool, ADR-0100 decision 4): a slot whose read ignores
// its deadline is SIGKILLed at Timeout, the failure is typed ErrTimeout, and
// the pool survives with a replacement.
func TestPooledDeadlineKillsOnlyTheCallerSlot(t *testing.T) {
	setCounter(t, "FAKE_HOLDS", 99) // the stub holds after every image
	stub := fakeEngine(t)

	pool := NewPooled(Tesseract{Path: stub, Timeout: 400 * time.Millisecond}, 1)
	defer pool.Close()
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err := pool.Recognize(ctx, poolTinyImage())
	elapsed := time.Since(start)
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("want *Failure, got %T (%v)", err, err)
	}
	if !errors.Is(f.Kind, ErrTimeout) {
		t.Errorf("failure kind = %v, want ErrTimeout", f.Kind)
	}
	if elapsed > 2*time.Second {
		t.Errorf("the read took %s: the deadline kill did not bound it (want ~400 ms)", elapsed)
	}
	// The pool survives: a replacement is spawned (backoff nudge), and the
	// next read runs on it (timing out again — the churn bound is the point,
	// not a panic or a wedged process).
	time.Sleep(300 * time.Millisecond)
	_, err = pool.Recognize(ctx, poolTinyImage())
	var f2 *Failure
	if !errors.As(err, &f2) || !errors.Is(f2.Kind, ErrTimeout) {
		t.Errorf("second read: want ErrTimeout on the replacement slot, got %v", err)
	}
}

// TestPooledCancelledReadKillsOnlyItsSlot proves cancellation of ONLY the
// caller's read (ADR-0100 decision 4): caller A's read holds a slot (the stub
// blocks after the image); the caller cancels — A returns ErrCancelled
// promptly, its slot's process is SIGKILLed, and caller B's read settles with
// its text on the other slot: B's read was untouched by A's cancel.
func TestPooledCancelledReadKillsOnlyItsSlot(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "pids")
	t.Setenv("FAKE_PIDS", pidPath)
	stub := fakeEngine(t)

	// One hold: caller A's read blocks in its slot (the stub sleeps 30 s
	// after the image); caller B's read (counter spent) answers.
	setCounter(t, "FAKE_HOLDS", 1)

	pool := NewPooled(Tesseract{Path: stub, Timeout: 30 * time.Second}, 2)
	defer pool.Close()
	time.Sleep(200 * time.Millisecond) // both slots warm (no load in this stub)

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelB()

	var (
		errA  error
		doneA = make(chan struct{})
	)
	// Caller A: reaches its slot and holds (the stub sleeps).
	go func() {
		_, errA = pool.Recognize(ctxA, poolTinyImage())
		close(doneA)
	}()
	// Let A reach its slot and hold.
	time.Sleep(400 * time.Millisecond)
	// Caller B: queued on the read gate behind A (the pool serialises OCR).
	var (
		resB  Result
		errB  error
		doneB = make(chan struct{})
	)
	go func() {
		resB, errB = pool.Recognize(ctxB, poolTinyImage())
		close(doneB)
	}()
	// Cancel caller A only. A's deadline killer SIGKILLs A's slot group.
	cancelA()
	select {
	case <-doneA:
	case <-time.After(3 * time.Second):
		t.Fatal("caller A's read did not return after cancellation")
	}
	if errA == nil {
		t.Fatal("caller A: want the cancellation failure, got nil")
	}
	var f *Failure
	if !errors.As(errA, &f) || !errors.Is(f.Kind, ErrCancelled) {
		t.Errorf("caller A: failure = %v, want ErrCancelled", errA)
	}
	// Caller A's slot process is dead (SIGKILLed, not left sleeping). The
	// stubs record their pids at spawn; a replacement slot may also have
	// spawned by now, so the ordering is not guaranteed — assert that at
	// least one recorded slot process is gone, which is the kill that
	// matters (A's slot), paired with B settling below.
	pids, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("stubs did not record pids: %v", err)
	}
	fields := strings.Fields(string(pids))
	if len(fields) < 2 {
		t.Fatalf("want ≥ 2 stub pids (two slots), got %v", fields)
	}
	// Let the killed slot's watcher reap it (a SIGKILLed process lingers as a
	// zombie until reaped, and a zombie still answers the liveness probe).
	time.Sleep(100 * time.Millisecond)
	dead := 0
	for _, p := range fields {
		n, aerr := strconv.Atoi(p)
		if aerr != nil {
			continue
		}
		if processGone(n) {
			dead++
		}
	}
	if dead == 0 {
		t.Errorf("no stub process died: the cancel must SIGKILL caller A's slot group (pids %v)", fields)
	}
	// Caller B settles with its text: B's read was untouched by A's cancel.
	select {
	case <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("caller B's read did not settle after A's cancel")
	}
	if errB != nil {
		t.Fatalf("caller B's read: %v", errB)
	}
	if resB.Text != "hello" {
		t.Errorf("caller B text = %q, want hello", resB.Text)
	}
}

// TestPooledClosedPoolFallsBack proves the closed-pool fallback (ADR-0100
// decision 3): after Close, reads still work through the per-process path.
func TestPooledClosedPoolFallsBack(t *testing.T) {
	stub := fakeEngine(t)
	pool := NewPooled(Tesseract{Path: stub, Timeout: 5 * time.Second}, 1)
	pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := pool.Recognize(ctx, poolTinyImage())
	if err != nil {
		t.Fatalf("read on a closed pool: want the per-process fallback, got %v", err)
	}
	if res.Text != "hello" {
		t.Errorf("text = %q, want hello", res.Text)
	}
}

// TestPooledEmptyImageFailsTyped mirrors the wrapper's input contract: no
// pixels is a typed failure before any engine is touched.
func TestPooledEmptyImageFailsTyped(t *testing.T) {
	stub := fakeEngine(t)
	pool := NewPooled(Tesseract{Path: stub, Timeout: 5 * time.Second}, 1)
	defer pool.Close()
	_, err := pool.Recognize(context.Background(), nil)
	var f *Failure
	if !errors.As(err, &f) || !errors.Is(f.Kind, ErrFailed) {
		t.Errorf("empty image: want ErrFailed, got %v", err)
	}
}

// TestPooledSlotCountIsClamped proves the memory bound (docs/spec/13): the
// slot count is clamped to [1, MaxPoolSlots].
func TestPooledSlotCountIsClamped(t *testing.T) {
	stub := fakeEngine(t)
	if got := NewPooled(Tesseract{Path: stub}, 99).Slots; got != MaxPoolSlots {
		t.Errorf("Slots(99) = %d, want %d (the clamp)", got, MaxPoolSlots)
	}
	if got := NewPooled(Tesseract{Path: stub}, 0).Slots; got != DefaultPoolSlots {
		t.Errorf("Slots(0) = %d, want %d (the default)", got, DefaultPoolSlots)
	}
	pool := NewPooled(Tesseract{Path: stub}, 1)
	defer pool.Close()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.slots) != 1 {
		t.Errorf("pool has %d slots, want 1", len(pool.slots))
	}
}
