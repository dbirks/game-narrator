package ocr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Pooled is the persistent engine of issue #13: it keeps a small pool of warm
// `tesseract` child processes so the model is loaded across reads.
//
// The mechanism is measured on the target hardware (ADR-0100; the
// re-measurement recipe is in docs/manuals/18), not assumed:
//
//   - Stock tesseract 5.5 reads exactly one image from its input and exits —
//     there is no batch/list mode (two concatenated PNGs on one stdin stream
//     produce one page in the same wall time; --help lists no such flag).
//     The persistent unit is therefore one warm process per read: each slot
//     is `tesseract stdin stdout <engine args>`, spawned early and blocked on
//     its stdin pipe, which it reads only after its fixed startup (process +
//     leptonica + model load, 0.58–0.60 s measured on the A53 — ADR-0096).
//   - Feeding the crop to a warm slot costs the inference only (~0.55 s for
//     the pipeline's preprocessed dialogue crop); the 2-slot pool at the live
//     A53 pace (~0.9 Hz) measured p50 576 ms / p95 593 ms — inside issue
//     #13's 700 ms target.
//   - A model load overlapping an in-flight read thrashes (a warm read while
//     a sibling loads measured 1.30 s; ADR-0096's 17× is the same effect),
//     so the pool serialises OCR execution — at most one read in flight —
//     and spawns a replacement only after the read that consumed a slot has
//     settled, so the load overlaps the read-free gap, never a read.
//
// The embedded Tesseract is not decoration: it is the automatic fallback. A
// read the pool cannot serve (the pool is closed, or degraded after four
// consecutive slot failures — the crash-loop shape) runs the per-process
// path on the remaining deadline, and the pool recovers on the next
// successful slot read (ADR-0100 decision 3). A single crash costs no read:
// the replacement is spawned immediately and the next read finds a warm slot
// again.
//
// Privacy (docs/spec/14) is unchanged from the per-process path: a slot
// receives exactly the crop bytes that path would have written to its 0600
// temp file, and nothing else crosses the pipe.
//
// The slot protocol is a capability, measured at startup, not an assumption
// (ADR-0100, capability probe): NewPooled settles one small real read
// through the slot protocol in a THROWAWAY process (runProbe →
// Tesseract.probeSlotProtocol) — one image on the process's stdin, the TSV
// answer on its stdout, the process's exit. The throwaway shape is deliberate:
// the verdict is a property of the engine, so measuring it must not perturb
// the pool's operational state (no slot consumed, no degrade count, no
// backoff) — the reads find exactly the pool NewPooled built. An engine
// whose settled answer is not the TSV the wrapper parses lacks the protocol
// the pool is built on (a tesseract build without stdin input, a wrapper
// around another engine); the pool then serves every read on the per-process
// fallback (decision 3's shape for a capability the engine lacks, not a
// crash) and stops spending read deadlines on slots that cannot settle. A
// read that races the unsettled probe runs the slot path as normal — worst
// case one read's deadline, one typed failure — after which the verdict is
// final and every read takes the fallback. An engine that merely fails the
// probe (a broken tessdata, a crash, the probe's own budget under load) may
// still speak the protocol; it failed one read, and decision 3's ordinary
// machinery owns a failing engine.
type Pooled struct {
	// Tesseract is the shared engine configuration (Path, Lang, PSM, OEM,
	// Whitelist, Timeout, caps) and the per-process fallback this pool runs
	// when it is closed or degraded. TempDir is unused by the pool: the crop
	// travels over the slot's stdin, not a file.
	Tesseract
	// Slots is the number of warm processes (default DefaultPoolSlots, clamped
	// to [1, MaxPoolSlots]). Bounded memory: one slot is ~29 MB RSS on the A53
	// (ADR-0100), idle in a blocked pipe read with zero CPU.
	Slots int

	mu sync.Mutex
	// slots is the warm set; the slots' fields are guarded by mu.
	slots []*pooledSlot
	// closed is set by Close; reads on a closed pool run the fallback.
	closed bool
	// consecFails counts consecutive slot failures; at DefaultDegradeFails the
	// pool stops spending reads on spawn-and-crash churn and the fallback
	// serves (the crash-loop bound, ADR-0100 decision 3).
	consecFails int
	// backoffUntil gates background replacements after failures (exponential,
	// capped). A read that owns no slot spawns anyway: the read's deadline is
	// its bound, and a broken engine fails fast either way.
	backoffUntil time.Time
	// probeDone is closed once the startup capability probe (runProbe) has
	// settled or given up. A read never parks on it (it checks the verdict,
	// probeUnsupported — see Recognize's comment); it is the liveness handle
	// Close and the tests observe.
	probeDone chan struct{}
	// probeUnsupported is the verdict the probe records (under mu, final
	// once probeDone is closed): the engine's settled probe answer was not
	// the TSV the wrapper parses — the engine lacks the slot protocol. Every
	// read then runs the per-process fallback.
	probeUnsupported bool
	// readGate serialises OCR execution: one in-flight read (ADR-0100
	// decision 2). It is a one-slot semaphore, not a queue — the waiter is the
	// pipeline's own bounded worker, and its deadline is the wait bound
	// (docs/spec/13).
	readGate chan struct{}
	// spawnWg tracks the slots' watcher goroutines so Close reaps every child
	// it started before returning.
	spawnWg sync.WaitGroup
}

// DefaultPoolSlots is the measured minimum (ADR-0100): one slot serves the
// read, the other is warm while the first's replacement loads in the
// read-free gap. One slot measured p95 986 ms at the live pace and degrading;
// two measured p95 593 ms.
const DefaultPoolSlots = 2

// MaxPoolSlots bounds the pool's memory (docs/spec/13): four slots is the
// most a two-region profile plus headroom can want.
const MaxPoolSlots = 4

// DefaultDegradeFails is the consecutive-failure count after which the pool
// stops spending reads on spawn-and-crash churn and the per-process fallback
// serves instead. One or two failures get an immediate replacement; four is
// the crash loop (bad tessdata, broken install), where the fallback gives the
// operator the same typed failure the per-process path always has.
const DefaultDegradeFails = 4

// pooledSlot is one warm tesseract process. Its lifecycle: spawned (blocked
// on its stdin pipe after its fixed startup), consumed by exactly one read
// (which writes the crop to stdin and collects the TSV), exits (tesseract
// reads one image and exits), and replaced in the read-free gap. The
// watcher goroutine is the ONLY caller of cmd.Wait: exec forbids a second
// Wait on the same command, so the read path observes the exit through the
// done channel instead of waiting itself.
type pooledSlot struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *stdoutWriter // cmd.Stdout, owned by Wait's copy goroutine until done
	stderr *tailWriter   // the engine's stderr tail (exec drains it for us)
	born   time.Time
	dead   bool // exited (consumed or died while warm)
	busy   bool // a read owns it
	// consumed is set (under mu) when a read picks the slot. It tells the
	// watcher that the READ owns the slot's outcome bookkeeping (it will mark
	// it dead and spawn the replacement), so the watcher must not also count
	// it as a warm death. A slot that dies while still unconsumed (warm) is
	// the watcher's to account for.
	consumed bool
	// done carries the watcher's Wait error (cap 1: the watcher sends once,
	// the read receives once; an un-consumed idle death just parks there).
	done chan error
	// spawnErr is the typed failure of a spawn that could not start (missing
	// binary): the slot is dead and the read sees the wrapper's typed
	// failure instead of a silent empty read.
	spawnErr error
}

// NewPooled returns a pool ready to accept reads. The warm spawns start in
// the background immediately: a read arriving within ~0.6 s of construction
// still finds its slot loading (it pays the startup inline — exactly the
// per-process cost, never worse), and later reads find it warm. The startup
// capability probe (runProbe) settles in the background too, in a
// throwaway process that touches none of the pool's slots: a read that
// races it runs the slot path as normal, and once the verdict is settled on
// an unsupported engine every read takes the per-process path.
// Construction never blocks and never fails: a missing binary is the read's
// typed ErrEngineUnavailable, the wrapper's contract (ADR-0012).
func NewPooled(t Tesseract, slots int) *Pooled {
	if slots <= 0 {
		slots = DefaultPoolSlots
	}
	if slots > MaxPoolSlots {
		slots = MaxPoolSlots
	}
	p := &Pooled{
		Tesseract: t,
		Slots:     slots,
		readGate:  make(chan struct{}, 1),
		probeDone: make(chan struct{}),
	}
	p.readGate <- struct{}{} // one in-flight read at a time
	for i := 0; i < slots; i++ {
		p.spawnLocked()
	}
	// The probe runs the slot protocol end to end with one small real read —
	// the measured basis of the pool (ADR-0100) is verified on this engine,
	// not assumed for it.
	p.spawnWg.Add(1)
	go p.runProbe()
	return p
}

// Name implements Namer: the pool's reads are tesseract's reads, and the
// recognition provenance on an observation names the engine that produced the
// hypothesis, not the wrapper around it.
func (p *Pooled) Name() string { return p.Tesseract.Name() }

// Close kills the pool's process groups and reaps them. It is idempotent and
// safe alongside reads still in flight: a read in flight keeps its own slot
// (its deadline killer still reaches the process group), and later reads run
// the per-process fallback. When the owning process exits without calling
// Close, the slots' stdin pipes EOF and each slot exits on the engine's own
// read error — a warm tesseract never outlives the process that owns it.
func (p *Pooled) Close() error {
	p.mu.Lock()
	p.closed = true
	live := make([]*pooledSlot, 0, len(p.slots))
	for _, s := range p.slots {
		if !s.dead {
			s.dead = true
			live = append(live, s)
		}
	}
	p.slots = nil
	p.mu.Unlock()
	for _, s := range live {
		killSlot(s.cmd)
	}
	p.spawnWg.Wait() // every watcher has reaped its child
	return nil
}

// Recognize implements Engine. It bounds the whole call — gate wait, slot
// read, everything — by the caller's context and the engine's Timeout, kills
// only this read's slot on deadline (the other slots and any other caller's
// read are untouched), and runs the per-process fallback when the pool cannot
// serve (ADR-0100 decision 3).
func (p *Pooled) Recognize(ctx context.Context, img image.Image) (Result, error) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if img == nil || img.Bounds().Empty() {
		return Result{}, &Failure{Kind: ErrFailed, Engine: p.Name(), Err: errors.New("no pixels to read")}
	}
	deadline := p.deadline()

	// The startup capability verdict (ADR-0100, capability probe): if the
	// probe has already settled on "the engine does not speak the slot
	// protocol", the per-process path is the only one it does (decision 3's
	// shape for a capability the engine lacks, not a crash — the probe's
	// typed verdict, not the crash-loop count, got the pool here). A read
	// that races the unsettled probe runs the slot path as normal: it
	// settles with the probe — worst case one read's deadline, one typed
	// failure — and every read after the verdict takes the fallback. The
	// read never parks on the verdict: the probe holds the one-read gate
	// while settling, and a read holding the gate while waiting for it would
	// deadlock the probe (the read's deadline and the probe's budget would
	// run out together).
	if p.unsupported() {
		fb := p.Tesseract
		fb.Timeout = deadline - time.Since(start)
		return fb.Recognize(ctx, img)
	}

	// The one-read gate (ADR-0100 decision 2): the waiter is the pipeline's
	// own bounded worker; its deadline bounds the wait (docs/spec/13).
	timer := time.NewTimer(deadline)
	acquired := false
	select {
	case <-p.readGate:
		acquired = true
	case <-ctx.Done():
	case <-timer.C:
	}
	timer.Stop()
	if !acquired {
		kind := ErrTimeout
		if ctx.Err() != nil {
			kind = ErrCancelled
		}
		return Result{}, &Failure{Kind: kind, Engine: p.Name(), Err: fmt.Errorf("vision/ocr: pool gate waited %s", deadline)}
	}
	defer func() { p.readGate <- struct{}{} }()

	// The crop as PNG for the slot's stdin. The per-process path would write
	// the same bytes to its 0600 temp file; encoding is the wrapper's
	// measured 1.4 ms on the board.
	pngBytes, err := encodePNG(img)
	if err != nil {
		return Result{}, &Failure{Kind: ErrFailed, Engine: p.Name(), Err: err}
	}

	remain := deadline - time.Since(start)
	if remain <= 0 {
		return Result{}, &Failure{Kind: ErrTimeout, Engine: p.Name(), Err: fmt.Errorf("vision/ocr: pool deadline elapsed before the read started")}
	}

	// Slot selection under mu: the warmest (oldest) free slot, a fresh spawn,
	// or the fallback.
	var (
		slot      *pooledSlot
		fallback  bool
		spawnFail error
	)
	p.mu.Lock()
	if p.closed {
		fallback = true
	} else {
		p.nudgeLocked()
		slot = p.pickSlotLocked()
		if slot == nil {
			// The degraded pool (crash loop) refused a spawn: retry with the
			// backoff so a healed engine recovers on its own (ADR-0100
			// decision 3), then serve the read on the fallback.
			if p.consecFails >= DefaultDegradeFails {
				p.spawnAfterBackoffLocked()
			}
			fallback = true
		} else if slot.spawnErr != nil {
			spawnFail = slot.spawnErr
		}
	}
	p.mu.Unlock()

	if fallback {
		// The per-process path on the remaining deadline: the wrapper's own
		// supervision, argv, typed failures and deadline (ADR-0012).
		fb := p.Tesseract
		fb.Timeout = remain
		return fb.Recognize(ctx, img)
	}
	if spawnFail != nil {
		// A spawn that cannot start (missing binary): the typed failure is
		// the operator's, exactly as the per-process path reports it.
		if f, ok := spawnFail.(*Failure); ok {
			return Result{}, f
		}
		return Result{}, &Failure{Kind: ErrEngineUnavailable, Engine: p.Name(), Err: spawnFail}
	}
	res, ferr := p.runSlotRead(ctx, slot, pngBytes, remain)
	if ferr != nil {
		p.afterSlotFailure(slot)
		return res, ferr
	}
	// Settled: the slot is consumed (tesseract exits after one image). The
	// replacement spawns now, so its load overlaps the read-free gap after
	// the gate releases, never the next read (the measured 0.4 s penalty of
	// the overlap, ADR-0100).
	p.afterSlotSuccess(slot)
	return res, nil
}

// probeUnsupported reports the probe's verdict (under mu). It is final once
// probeDone is closed: the probe sets it and never clears it.
func (p *Pooled) unsupported() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probeUnsupported
}

// probeImage is the probe read's input: a small blank crop. The engine has
// nothing to read (the answer is an empty TSV); the probe verifies the slot
// protocol — spawn, one image on stdin, the TSV answer, the slot's exit —
// not a recognition. The 16×16 shape is the ADR-0100 fixed-startup probe's.
func probeImage() image.Image { return image.NewGray(image.Rect(0, 0, 16, 16)) }

// runProbe settles one small real read through the engine's slot protocol and
// records the capability verdict (the type comment above): does the engine
// take one image on its stdin stream and answer with the TSV the wrapper
// parses?
//
// The check runs in a THROWAWAY process, not a pool slot (ADR-0100, capability
// probe): the verdict is a property of the engine, and measuring it must not
// perturb the pool's operational state — no slot consumed, no degrade count,
// no backoff. The pool the reads find is the pool NewPooled built, healthy or
// not. The probe still holds the one-read gate while it runs: it runs the
// model, and a model load overlapping a read is the measured hazard
// (ADR-0100 decision 2), so the probe serialises with reads exactly as a read
// does. Its bound is the engine's own deadline (p.deadline): the probe settles
// with any read, and a probe the deadline outruns has the engine merely slow,
// which the ordinary machinery owns.
//
// The verdict:
//
//   - a settled, parseable TSV: the protocol holds; nothing is recorded and
//     every read runs the slot path as normal.
//   - a settled, unparseable answer (ErrTSVUnparseable): the engine lacks the
//     protocol; the probeUnsupported flag is set (under mu, final once
//     probeDone closes) and every read runs the per-process fallback. No
//     failure count follows — the engine is not crashing, it is answering in
//     a language the wrapper does not speak, and the crash-loop machinery
//     must not also own it.
//   - everything else (a non-zero exit, a crash, the deadline under load, an
//     unstartable binary): the engine may still speak the protocol; it failed
//     this read, and the verdict simply is not "the protocol is missing".
//
// probeDone closes on every path — including a pool closed before the probe
// settled — so a reader observing the verdict is never parked forever.
func (p *Pooled) runProbe() {
	defer p.spawnWg.Done()
	defer close(p.probeDone)

	budget := p.deadline()
	runCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	// Serialise with reads (ADR-0100 decision 2): the probe runs the model,
	// and a model load overlapping a read degrades the read. The probe's own
	// deadline bounds the gate wait, so the probe never parks a reader past
	// the engine's deadline.
	select {
	case <-p.readGate:
	case <-runCtx.Done():
		// The budget ran out on the gate (a reader held it for its whole
		// deadline): no verdict, and the readers that wanted one run the slot
		// path on their own deadlines. The probe is done; nothing is retried
		// (the first real read retries the engine on its own deadline anyway).
		return
	}
	defer func() { p.readGate <- struct{}{} }()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	pngBytes, err := encodePNG(probeImage())
	if err != nil {
		return
	}
	err = p.Tesseract.probeSlotProtocol(runCtx, pngBytes, budget)
	var f *Failure
	if err != nil && errors.As(err, &f) && errors.Is(f.Err, ErrTSVUnparseable) {
		p.mu.Lock()
		p.probeUnsupported = true
		p.mu.Unlock()
	}
}

// pickSlotLocked returns a slot for one read, or nil when the fallback must
// serve. Callers hold p.mu.
func (p *Pooled) pickSlotLocked() *pooledSlot {
	var warm *pooledSlot
	for _, s := range p.slots {
		if s.dead || s.busy || s.spawnErr != nil {
			continue
		}
		if warm == nil || s.born.Before(warm.born) {
			warm = s
		}
	}
	if warm != nil {
		warm.busy = true
		warm.consumed = true
		return warm
	}
	// No free slot: the degraded pool (crash loop) stops spending reads on
	// spawn-and-crash churn — the fallback serves; the caller retries the
	// spawn on its backoff (spawnAfterBackoffLocked).
	if p.consecFails >= DefaultDegradeFails {
		return nil
	}
	// One (or a few) failure(s) cost no read: spawn now, at the read's own
	// deadline's mercy. The backoff gates background replacements only.
	s := p.spawnLocked()
	if s == nil || s.spawnErr != nil {
		return nil
	}
	s.busy = true
	s.consumed = true
	return s
}

// spawnLocked starts one warm slot and appends it to the pool. Callers hold
// p.mu. A spawn that cannot start records spawnErr on the (dead) slot: the
// pool keeps its bounded shape and the next read sees the typed failure.
func (p *Pooled) spawnLocked() *pooledSlot {
	bin, err := p.Tesseract.binary()
	if err != nil {
		var kind error = ErrFailed
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
			kind = ErrEngineUnavailable
		}
		s := &pooledSlot{dead: true, done: make(chan error, 1), spawnErr: &Failure{Kind: kind, Engine: p.Name(), Err: err}}
		p.slots = append(p.slots, s)
		return s
	}
	tail := p.StderrTail
	if tail <= 0 {
		tail = DefaultStderrTail
	}
	args := append([]string{"stdin", "stdout"}, p.Tesseract.args()...)
	cmd := exec.Command(bin, args...)
	superviseSlot(cmd)
	cmd.Stderr = &tailWriter{limit: tail} // exec drains the engine's stderr into the bounded tail
	// The TSV travels through a Wait-owned buffer, not a StdoutPipe the read
	// drains: Wait closes a StdoutPipe's read end "after seeing the command
	// exit", so a drain racing that close can observe a clean exit with the
	// answer still unread in the pipe buffer — an empty-output failure on a
	// child that answered correctly (ADR-0100 correction). The buffer's
	// single owner is Wait's copy goroutine (the wrapper's and
	// probeSlotProtocol's proven pattern), and its cap is the wrapper's
	// StdoutCap, which bounds a misbehaving child's heap growth (docs/spec/13).
	limit := p.StdoutCap
	if limit <= 0 {
		limit = DefaultStdoutCap
	}
	var stdoutBuf bytes.Buffer
	stdoutBuf.Grow(1 << 14)
	cmd.Stdout = &stdoutWriter{buf: &stdoutBuf, cap: uint64(limit)}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return p.spawnFailureLocked(err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		var kind error = ErrFailed
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
			kind = ErrEngineUnavailable
		}
		s := &pooledSlot{dead: true, done: make(chan error, 1), spawnErr: &Failure{Kind: kind, Engine: p.Name(), Err: err}}
		p.slots = append(p.slots, s)
		return s
	}
	s := &pooledSlot{
		cmd:    cmd,
		stdin:  stdin,
		stdout: cmd.Stdout.(*stdoutWriter),
		stderr: cmd.Stderr.(*tailWriter),
		born:   time.Now(),
		done:   make(chan error, 1),
	}
	p.slots = append(p.slots, s)
	// The watcher: the ONLY caller of cmd.Wait. A slot consumed by a read is
	// reaped here too — the read observes the exit through s.done, so no
	// second Wait is ever issued on the same command. A slot that dies while
	// warm (OOM, a tessdata error that surfaces after spawn) is marked dead
	// here and replaced by the next read-free moment.
	p.spawnWg.Add(1)
	go func() {
		defer p.spawnWg.Done()
		err := cmd.Wait()
		select {
		case s.done <- err:
		default:
		}
		p.mu.Lock()
		if !s.dead && !s.consumed {
			// The slot died while warm (OOM, a tessdata error that surfaces
			// after spawn): count it against the pool — a warm death is a
			// failure the next read would have hit, and the crash-loop bound
			// must see it (ADR-0100 decision 3). A consumed slot's outcome is
			// the read's to book (afterSlotFailure/Success), never the
			// watcher's: counting both would double the backoff and double the
			// replacement.
			s.dead = true
			s.spawnErr = errors.New("vision/ocr: warm slot exited before a read consumed it")
			p.consecFails++
			p.backoffUntil = time.Now().Add(backoffFor(p.consecFails))
			p.dropDeadLocked()
			p.spawnReplacementLocked()
		}
		p.mu.Unlock()
	}()
	return s
}

// spawnFailureLocked records an unstartable slot (bounded shape, typed
// failure). Callers hold p.mu.
func (p *Pooled) spawnFailureLocked(err error) *pooledSlot {
	s := &pooledSlot{dead: true, done: make(chan error, 1), spawnErr: &Failure{Kind: ErrFailed, Engine: p.Name(), Err: err}}
	p.slots = append(p.slots, s)
	return s
}

// runSlotRead feeds one crop to one slot and collects its TSV. remain bounds
// the read; the slot's process group is killed when the bound elapses
// (ADR-0012 §2) — and only this slot's group.
func (p *Pooled) runSlotRead(ctx context.Context, slot *pooledSlot, pngBytes []byte, remain time.Duration) (Result, error) {
	start := time.Now()
	runCtx, cancel := context.WithTimeoutCause(ctx, remain, ErrTimeout)
	defer cancel()

	// The deadline killer: SIGKILL the slot's process group when runCtx is
	// done. The read path closes settled when it returns naturally first, so
	// a kill lands only on a read that did not settle in time; the kill on a
	// group that already exited is ESRCH, which is success.
	killed := make(chan struct{})
	settled := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			killSlot(slot.cmd)
			close(killed)
		case <-settled:
		}
	}()
	defer close(settled)

	if _, err := slot.stdin.Write(pngBytes); err != nil {
		// EPIPE before the first byte: the slot died between selection and
		// the write. The typed failure carries what its stderr holds.
		_ = slot.stdin.Close()
		return Result{}, &Failure{
			Kind:   ErrFailed,
			Engine: p.Name(),
			Args:   p.Tesseract.args(),
			Stderr: slot.stderr.String(),
			Err:    fmt.Errorf("vision/ocr: writing the crop to the engine slot: %w", err),
		}
	}
	// EOF on the slot's stdin: the image is handed over; the slot processes,
	// answers and exits.
	if err := slot.stdin.Close(); err != nil {
		_ = slot.stdin.Close()
	}

	// The watcher's Wait owns the TSV: it copies the slot's stdout into
	// slot.stdout (the wrapper's cap-carrying writer) and — with no
	// StdoutPipe left for it to close — cannot race the read's view of the
	// buffer. done is the happens-after edge: by the time it fires, the copy
	// has completed (or was abandoned at the 250 ms WaitDelay, in which case
	// the exit status below says so). The buffer's cap is the wrapper's
	// StdoutCap, so a misbehaving child cannot grow the heap unboundedly
	// (docs/spec/13); the over flag is checked after done, under the same
	// happens-after.
	var waitErr error
	select {
	case waitErr = <-slot.done:
	case <-killed:
		waitErr = errors.New("vision/ocr: the engine slot was killed at its deadline")
	}

	elapsed := time.Since(start)
	if cause := context.Cause(runCtx); cause != nil {
		kind := ErrCancelled
		if errors.Is(cause, ErrTimeout) {
			kind = ErrTimeout
		}
		return Result{}, &Failure{
			Kind:   kind,
			Engine: p.Name(),
			Args:   p.Tesseract.args(),
			Stderr: slot.stderr.String(),
			Err:    cause,
		}
	}
	if slot.stdout.over {
		return Result{}, &Failure{
			Kind:   ErrOutputTooLarge,
			Engine: p.Name(),
			Args:   p.Tesseract.args(),
			Stderr: slot.stderr.String(),
			Err:    fmt.Errorf("stdout exceeded %d bytes", slot.stdout.cap),
		}
	}
	if waitErr != nil {
		return Result{}, &Failure{
			Kind:   ErrFailed,
			Engine: p.Name(),
			Args:   p.Tesseract.args(),
			Stderr: slot.stderr.String(),
			Err:    waitErr,
		}
	}
	res, perr := ParseTSV(slot.stdout.buf.Bytes())
	if perr != nil {
		return Result{}, &Failure{
			Kind:   ErrFailed,
			Engine: p.Name(),
			Args:   p.Tesseract.args(),
			Stderr: slot.stderr.String(),
			Err:    perr,
		}
	}
	res.Duration = elapsed
	res.Engine = p.Name()
	return res, nil
}

// afterSlotSuccess clears the failure bookkeeping and spawns the replacement
// in the read-free gap. The caller still holds the read gate when this runs,
// so the replacement's load overlaps the gap after the gate releases, never
// an in-flight read.
func (p *Pooled) afterSlotSuccess(slot *pooledSlot) {
	p.mu.Lock()
	p.consecFails = 0
	p.backoffUntil = time.Time{}
	p.markDeadLocked(slot)
	p.dropDeadLocked()
	p.spawnReplacementLocked()
	p.mu.Unlock()
}

// afterSlotFailure records the failure: the backoff grows, the replacement is
// scheduled in the read-free gap (it may wait out the backoff — the read
// already has its typed failure), and at the degrade count the pool stops
// spending reads on churn.
func (p *Pooled) afterSlotFailure(slot *pooledSlot) {
	p.mu.Lock()
	p.consecFails++
	p.backoffUntil = time.Now().Add(backoffFor(p.consecFails))
	p.markDeadLocked(slot)
	p.dropDeadLocked()
	if p.consecFails < DefaultDegradeFails {
		p.spawnReplacementLocked()
	}
	p.mu.Unlock()
}

// spawnReplacementLocked starts one replacement slot when the pool wants more
// than it has and the backoff has elapsed; inside the backoff window it
// defers to the next read-free moment. Callers hold p.mu.
func (p *Pooled) spawnReplacementLocked() {
	if p.closed || len(p.slots) >= p.wantedSlots() {
		return
	}
	p.spawnAfterBackoffLocked()
}

// spawnAfterBackoffLocked spawns one slot once the backoff has elapsed, and
// re-arms the backoff so a still-broken engine retries at the capped pace
// rather than per-read. Callers hold p.mu. It is the degraded pool's
// recovery path: a healed engine (the operator fixes the tessdata) warms its
// replacement slot, the next read uses it, and the success clears the
// degradation (ADR-0100 decision 3).
func (p *Pooled) spawnAfterBackoffLocked() {
	if p.closed || len(p.slots) >= p.wantedSlots() {
		return
	}
	if p.consecFails > 0 {
		if !p.backoffUntil.IsZero() && time.Now().Before(p.backoffUntil) {
			return
		}
		p.backoffUntil = time.Now().Add(backoffFor(p.consecFails))
	}
	p.spawnLocked()
}

// markDeadLocked frees a consumed slot. Callers hold p.mu.
func (p *Pooled) markDeadLocked(slot *pooledSlot) {
	slot.dead = true
	slot.busy = false
}

// nudgeLocked retries a deferred background replacement without waiting:
// the backoff defers the SPAWN, not the retry — a read-free moment (this
// call) re-arms the check, so a still-elapsed backoff spawns immediately and
// an un-elapsed one stays deferred. Callers hold p.mu.
func (p *Pooled) nudgeLocked() {
	if p.closed || p.consecFails == 0 {
		return
	}
	if !p.backoffUntil.IsZero() && time.Now().Before(p.backoffUntil) {
		return
	}
	p.spawnReplacementLocked()
}

// dropDeadLocked compacts the slice so it never grows one entry per read.
// Callers hold p.mu.
func (p *Pooled) dropDeadLocked() {
	kept := p.slots[:0]
	for _, s := range p.slots {
		if !s.dead {
			kept = append(kept, s)
		}
	}
	p.slots = kept
}

// wantedSlots is the clamped slot count.
func (p *Pooled) wantedSlots() int {
	n := p.Slots
	if n <= 0 {
		n = DefaultPoolSlots
	}
	if n > MaxPoolSlots {
		n = MaxPoolSlots
	}
	return n
}

// backoffFor is the crash-restart backoff: 200 ms doubling per consecutive
// failure, capped at 2 s (docs/spec/13: bounded, idempotent retries only).
func backoffFor(fails int) time.Duration {
	b := 200 * time.Millisecond
	for i := 1; i < fails; i++ {
		b *= 2
	}
	if b > 2*time.Second {
		return 2 * time.Second
	}
	return b
}
