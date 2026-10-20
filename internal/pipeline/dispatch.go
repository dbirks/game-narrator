package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// regionState is the ROI stage's per-region memory. It is touched only by the
// roiLoop goroutine — hence no locks — which is also why one goroutine owns the
// event loop: per-region FIFO order toward the reducer is what makes the tracker's
// stability window mean anything, and a lock would buy the illusion of a shared
// state, not the ordering.
type regionState struct {
	region games.Region
	gate   *rankGate

	// lastHash is the crop hash currently being read, or the last one read
	// successfully. Equality with a new frame's crop is the OCR skip
	// (docs/spec/05 "suppress duplicate OCR calls on unchanged region hash"). It is
	// set when a recognition starts, not when it finishes, so frames arriving
	// during a slow read attach to that read instead of racing a duplicate of it.
	lastHash string

	// inflight is the one recognition this region has outstanding. One, not a
	// list: two concurrent reads of one box return two answers about a screen the
	// player has already changed, and the reducer can only use the newest.
	// In live mode the frames also RIDE it: while a read is outstanding a
	// changed-hash frame attaches to it instead of superseding it, so a read
	// slower than the frame interval still settles (see accept).
	inflight *ocrJob

	pending []*pending
	heldAt  time.Time
	nextID  int64

	// lastOK is the most recent successful read of this region and the hash it
	// belongs to. It exists for one case: a pending observation holding the only
	// copy of a result can be evicted by the queue bound, and without this the
	// pixels would stay "already read" (lastHash says so) while the tracker never
	// received the text — a box frozen at a typewriter prefix. deliveredOK says
	// whether the reducer has been handed it, which is what distinguishes a genuine
	// reuse observation (nil OCR, per docs/spec/05) from a lost result.
	lastOK      *ocr.Result
	lastOKHash  string
	deliveredOK bool
}

// roiLoop is the ROI stage: one goroutine, one select over frame arrivals, OCR
// outcomes, the probe tick and cancellation. It closes in when the frames end so
// the reducer can tell "the recording ended" from "I am still waiting".
func (r *Runner) roiLoop(ctx context.Context, frames <-chan capture.Frame, in chan<- *obsIn, jobs chan<- *ocrJob, done <-chan ocrOutcome) error {
	defer close(in)
	regions := make([]*regionState, 0, len(r.cfg.Regions))
	for _, reg := range r.cfg.Regions {
		regions = append(regions, &regionState{region: reg, gate: r.gates[reg.ID]})
	}
	ticker := time.NewTicker(time.Second / time.Duration(r.cfg.FrameProbeHz))
	defer ticker.Stop()

	for {
		// Completed work is drained before new work is taken. That ordering is not
		// politeness: the select below would otherwise choose between a finished
		// recognition and a fresh frame at random, which lets frames whose results
		// are ready pile up behind the queue bound and get evicted. Preferring the
		// results is what makes a speed-0 replay deterministic — the same clip
		// produces the same observation stream every run — and it is why
		// TestDeterminism can assert equality instead of tolerance.
		drained := true
		for drained {
			drained = false
			select {
			case o := <-done:
				r.applyOutcome(regions, o)
				if err := r.emitAll(regions, ctx, in); err != nil {
					r.abort(regions)
					return err
				}
				drained = true
			default:
			}
		}
		select {
		case f, ok := <-frames:
			if !ok {
				r.drainAtEOF(ctx, regions, in, done)
				return nil
			}
			r.cnt.framesProcessed()
			// The active-picture tracker (ADR-0082) is fed once per frame, on this
			// frame-ordered stage, before any region crops the frame: every region
			// then maps through the same hysteresised window (CropOptions.Tracker
			// reads Box, it never mutates it), and K regions cannot advance the
			// hysteresis count faster than the frame rate. A nil tracker keeps the
			// historic stateless crop behaviour, and a borderless source (Dolphin)
			// never settles, so its crops map the whole frame as before.
			if tr := r.cfg.Crop.Tracker; tr != nil && f.Image != nil {
				tr.Update(f.Image)
			}
			for _, rs := range regions {
				if r.cfg.Mode == ModeReplay {
					// Replay determinism (docs/spec/12): this region's outstanding
					// recognition must land before its next frame is analysed.
					if err := r.waitInflight(rs, regions, ctx, in, done); err != nil {
						r.abort(regions)
						return err
					}
				}
				if err := rs.accept(r, ctx, f, in, jobs); err != nil {
					r.abort(regions)
					return err
				}
			}
		case o := <-done:
			r.applyOutcome(regions, o)
			if err := r.emitAll(regions, ctx, in); err != nil {
				r.abort(regions)
				return err
			}
		case <-ticker.C:
			// The 2–5 Hz probe docs/spec/05 asks for. It serves one purpose here: an
			// observation whose engine has stopped answering ages out on a bounded
			// deadline instead of holding the queue head forever. The engine's own
			// timeout is the primary bound; this is the belt.
			r.ageOut(regions)
			if err := r.emitAll(regions, ctx, in); err != nil {
				r.abort(regions)
				return err
			}
		case <-ctx.Done():
			r.abort(regions)
			return ctx.Err()
		}
	}
}

func (r *Runner) emitAll(regions []*regionState, ctx context.Context, in chan<- *obsIn) error {
	for _, rs := range regions {
		if err := rs.emitReady(r, ctx, in); err != nil {
			return err
		}
	}
	return nil
}

// waitInflight is the Replay-mode half of the frame-order contract: while this
// region has an outstanding recognition, the frame stream pauses for it. Each
// region's ROI/OCR is therefore strictly sequential in frame order — frame N+1
// is never analysed while frame N's recognition is still running — so a newer
// frame can never supersede an in-flight read and no supersede-cancel is ever
// issued (Stats.OCRCancelled and OCRStale stay zero on a clean replay). That is
// what makes a replay's outcome independent of how fast the CPU happens to be:
// the live path instead races frames against OCR and its drop counts depend on
// timing, which is exactly the machine-speed dependence docs/spec/12 forbids in
// the replay gate.
//
// The per-call timeout still applies: a wedged engine answers through its own
// context, the outcome arrives as an error, and that is what OCRErrors counts
// rather than a cancellation. Every outcome is applied through the same
// applyOutcome path the live select uses, so a multi-region replay keeps the
// per-region FIFO the tracker's stability window needs.
func (r *Runner) waitInflight(rs *regionState, regions []*regionState, ctx context.Context, in chan<- *obsIn, done <-chan ocrOutcome) error {
	for rs.inflight != nil {
		select {
		case o := <-done:
			r.applyOutcome(regions, o)
			if err := r.emitAll(regions, ctx, in); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// applyOutcome books a recognition result against the job that asked for it.
//
// A result for a job that was cancelled or superseded is dropped and counted.
// This is docs/spec/12's "two OCR futures finish out of order": the late answer is
// not wrong, it is about a screen that no longer exists, and applying it would let
// a truncated prefix reach the reducer after the full line already had.
func (r *Runner) applyOutcome(regions []*regionState, o ocrOutcome) {
	rs := regionFor(regions, o.job)
	if o.job == nil || o.job.dead {
		if o.err == nil {
			// A completed read of pixels nobody wants any more.
			r.cnt.ocrStale()
		}
		if rs != nil {
			rs.dropPendingFor(o.job)
			if rs.lastHash == o.job.hash {
				// The read that was supposed to describe these pixels was cancelled, so
				// "unchanged hash ⇒ already read" would be a claim about a result that
				// does not exist. The next identical frame re-reads.
				rs.lastHash = ""
			}
		}
		return
	}
	if o.err != nil {
		if !isCancelledErr(o.err) {
			// A genuine engine fault (timeout, non-zero exit, unavailable). A
			// cancellation this pipeline itself issued is not a fault: it was
			// counted where it was decided, and the engine's "I was cancelled"
			// answer only confirms it, so counting it again would make one
			// supersession look like two problems.
			r.cnt.ocrError()
			if r.cfg.Mode == ModeReplay {
				// The replay retry policy (ocrWorker) spent its budget and the
				// frame's text is still missing: the run is no longer the recording
				// it claims to be (docs/spec/12 "Frame replay"), so the frame is
				// named for the report instead of absorbed into OCR errors. Live
				// mode keeps its current semantics: a faulted frame there is simply
				// OCRErrors, and the next frame is the truth again.
				r.cnt.replayDegraded(o.job.frameSeq)
			}
		}
		if rs != nil {
			// A failed read is not evidence. The observation still goes out — the
			// region is present and the tracker should see the frame — but with no
			// OCR payload, and lastHash is cleared so the next frame retries rather
			// than reusing a result that never arrived.
			rs.lastHash = ""
			rs.inflight = nil
			rs.lastOK = nil
			rs.lastOKHash = ""
			rs.markFailed(o.job, o)
		}
		return
	}
	if rs != nil {
		if fb := o.res.Fallback; fb != nil {
			// The VLM fallback path ran (docs/spec/05, ADR-0039): count the
			// frame, and count the remote call only when one actually went
			// out — a cache hit or a rate-limited frame did not bill the
			// gateway, and the report must say so.
			r.cnt.ocrFallbackFrames()
			if fb.Reason != "secondary_cached" && fb.Reason != "secondary_rate_limited" {
				r.cnt.ocrFallbackSecondary()
			}
		}
		keep := o.res
		rs.lastOK = &keep
		rs.lastOKHash = o.job.hash
		rs.deliveredOK = false
		rs.inflight = nil
		rs.fillPending(o.job, o)
	}
}

// isCancelledErr reports whether a recognition failure is the engine confirming a
// cancellation this pipeline issued (supersession or shutdown) rather than a fault
// of its own. A timeout is deliberately not a cancellation here: the engine was
// still running when its hard deadline expired, which is what OCRErrors exists to
// report.
func isCancelledErr(err error) bool {
	return errors.Is(err, ocr.ErrCancelled) || errors.Is(err, context.Canceled)
}

// regionFor finds the region a job belongs to. Region ids are stable and few, so a
// scan beats a map at this size and keeps the state copyable.
func regionFor(regions []*regionState, job *ocrJob) *regionState {
	if job == nil {
		return nil
	}
	for _, rs := range regions {
		if rs.region.ID == job.region {
			return rs
		}
	}
	return nil
}

// drainAtEOF runs at end of stream: outstanding recognitions get one bounded
// window to land, because the last useful text in a clip is often still in flight
// when its final frame is delivered. Anything unresolved afterwards is dropped and
// counted, so a truncated tail is a number rather than an assumption.
//
// The only thing this waits on is OCR: ranking replies go straight from the rank
// worker to the reducer, which is running concurrently and waits for them itself
// (reduceLoop's outstanding count). Holding this loop on a retrieval would just
// add a second, slower fence over the same reply.
func (r *Runner) drainAtEOF(ctx context.Context, regions []*regionState, in chan<- *obsIn, done <-chan ocrOutcome) {
	budget := r.cfg.OCRTimeout + 200*time.Millisecond
	if r.cfg.Mode == ModeReplay {
		// A replay read is allowed its whole retry budget before EOF gives up
		// on it; the live bound would interrupt a second retry attempt.
		budget = ReplayOCRMaxAttempts*r.cfg.ReplayOCRTimeout + 200*time.Millisecond
	}
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	for {
		if err := r.emitAll(regions, ctx, in); err != nil {
			return
		}
		unresolved := false
		for _, rs := range regions {
			for _, p := range rs.pending {
				if !p.resolved() {
					unresolved = true
					break
				}
			}
			if unresolved {
				break
			}
		}
		if !unresolved {
			return
		}
		select {
		case o, ok := <-done:
			if !ok {
				return
			}
			r.applyOutcome(regions, o)
		case <-deadline.C:
			r.abort(regions)
			return
		case <-ctx.Done():
			r.abort(regions)
			return
		}
	}
}

// abort cancels everything outstanding and counts what was lost.
func (r *Runner) abort(regions []*regionState) {
	for _, rs := range regions {
		for _, p := range rs.pending {
			if p.job != nil && !p.resolved() && !p.job.dead {
				p.job.kill()
				r.cnt.ocrCancelled()
			}
		}
		rs.pending = nil
		rs.inflight = nil
	}
}

// ageOut drops observations whose recognition has outrun its deadline.
//
// It drops the observation, and kills the job only when that job is not the
// region's shared in-flight read: in live mode the newer pendings ride on that
// read (accept), so killing it here would cancel a read that is still the
// newest evidence the region has — the "cancel every in-progress read" failure
// issue #8 reports. A dropped head does not strand the riders: they keep their
// job, and when the read settles fillPending resolves them; a read that settles
// with no riders left still books lastOK, so the dedupe remembers the pixels.
func (r *Runner) ageOut(regions []*regionState) {
	limit := r.cfg.OCRTimeout + 250*time.Millisecond
	if r.cfg.Mode == ModeReplay {
		// The retry policy legitimately spends several per-call budgets on one
		// frame; aging out mid-retry would turn the load spike the retries
		// exist to ride out into a counted drop — the exact failure ModeReplay
		// exists to prevent.
		limit = ReplayOCRMaxAttempts*r.cfg.ReplayOCRTimeout + 250*time.Millisecond
	}
	for _, rs := range regions {
		for len(rs.pending) > 0 {
			p := rs.pending[0]
			if p.resolved() || r.cfg.Now().Sub(p.at) < limit {
				break
			}
			rs.pending = rs.pending[1:]
			if p.job != nil && p.job != rs.inflight {
				// A job nobody is riding on any more: cancel it and count it.
				p.job.kill()
				r.cnt.ocrCancelled()
			}
			r.cnt.ocrStale()
		}
	}
}

// --- one region ------------------------------------------------------------

// accept takes one frame's look at one region: crop, presence, hash, dedupe, and
// either an OCR dispatch or the reuse observation docs/spec/05 requires.
func (rs *regionState) accept(r *Runner, ctx context.Context, f capture.Frame, in chan<- *obsIn, jobs chan<- *ocrJob) error {
	now := r.cfg.Now()
	a := r.analyze(f, rs.region)
	p := &pending{frame: f, reg: rs.region, a: a, at: now}

	switch {
	case !a.present:
		// The box is gone, or was never in this rect. No recognition is worth
		// running, and the dedupe memory is cleared: reusing the last line for a box
		// that reappeared is exactly the "same words after a menu count as one
		// occurrence" bug docs/spec/12 tests for.
		rs.discardInflight(r)
		rs.lastHash = ""
	case a.hash == rs.lastHash && rs.lastHash != "":
		// Identical pixels: no OCR. If a read of these very pixels is in flight the
		// observation attaches to it rather than starting a duplicate.
		p.job = rs.inflight
		r.cnt.ocrSkipped()
	case a.hash != "":
		if rs.inflight != nil {
			// A read of a slightly older box is already in flight. Killing it on
			// every changed-hash frame is how a live region livelocks: when the
			// engine's latency exceeds the frame interval — a box over an animated
			// scene changes its crop every frame, so this branch runs every frame —
			// no read ever settles and the region never perceives a character. The
			// shape measured on the real host: a 12-minute Dolphin session, a
			// perfectly readable box on screen, 3861 OCR calls, 3862 cancellations,
			// zero rankings, zero commits.
			//
			// Ride the in-flight read instead: the frame's observation resolves
			// with that read's result and the read settles at the engine's own
			// pace, the docs/spec/05 2–5 Hz OCR cadence the engine actually
			// sustains. A box that advanced while the read ran is refused by the
			// reducer's generation fence, and the frame after the read settles
			// starts the next one. Replay never gets here: waitInflight
			// serializes, so inflight is nil at accept time and a changed hash
			// starts a fresh read, exactly as before.
			p.job = rs.inflight
			r.cnt.ocrDeferred()
		} else {
			p.job = rs.startJob(r, ctx, f.Seq, a, jobs)
		}
	}

	rs.pending = append(rs.pending, p)
	// Bounded queue with explicit backpressure: the *oldest* unresolved read is what
	// goes, because it is the least current answer in the system. The shared
	// in-flight read is never killed here: in live mode the newer pendings ride
	// on it, so killing it on overflow would punish the frames it was serving and
	// restart the supersede-to-death loop through the eviction door.
	for len(rs.pending) > r.cfg.MaxPendingOCR {
		head := rs.pending[0]
		rs.pending = rs.pending[1:]
		if !head.resolved() {
			if head.job != nil && head.job != rs.inflight {
				head.job.kill()
				r.cnt.ocrCancelled()
			}
			r.cnt.ocrStale()
		}
	}
	return rs.emitReady(r, ctx, in)
}

// startJob dispatches one recognition. The per-job context is a child of the run
// context carrying the configured timeout, so killing it reaches the engine's own
// subprocess: internal/vision/ocr supervises a process group. In ModeReplay the
// budget is ReplayOCRTimeout rather than OCRTimeout: nothing supersedes a replay
// read, so its deadline exists to bound a wedged engine, and the retry policy in
// ocrWorker — not an early kill — is what decides a slow frame's fate.
func (rs *regionState) startJob(r *Runner, ctx context.Context, frameSeq int64, a analysis, jobs chan<- *ocrJob) *ocrJob {
	timeout := r.cfg.OCRTimeout
	if r.cfg.Mode == ModeReplay {
		timeout = r.cfg.ReplayOCRTimeout
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	rs.nextID++
	job := &ocrJob{
		id:       rs.nextID,
		region:   rs.region.ID,
		hash:     a.hash,
		frameSeq: frameSeq,
		input:    a.input,
		raw:      a.crop,
		ctx:      jctx,
		cancel:   cancel,
		base:     ctx,
		started:  r.cfg.Now(),
	}
	rs.inflight = job
	rs.lastHash = a.hash
	select {
	case jobs <- job:
		// The engine is actually being called about these pixels from here on;
		// this is the number docs/spec/05's "suppress duplicate OCR calls" asserts
		// against, so it is booked at dispatch, not at completion.
		r.cnt.ocrCall()
	case <-ctx.Done():
		job.kill()
	}
	return job
}

// discardInflight kills an outstanding read because the pixels it described are no
// longer on screen, and retires the observations waiting on it in the same breath.
//
// Dropping them here rather than keeping them unresolved is the difference between
// a bounded pipeline and a stalled one: a result that will be refused at arrival
// can still hold the head of the queue, and the queue's bound is per-region, so one
// slow read would otherwise evict *newer* observations to make room for an answer
// nobody wants. The observation is counted as cancelled and stale where it is
// dropped, which is the honest ledger — those frames are not in the reducer, and
// Stats says so.
func (rs *regionState) discardInflight(r *Runner) {
	job := rs.inflight
	if job == nil {
		return
	}
	job.kill()
	rs.inflight = nil
	dropped := rs.dropPendingFor(job)
	r.cnt.ocrCancelled()
	for i := 0; i < dropped; i++ {
		r.cnt.ocrStale()
	}
	if rs.lastHash == job.hash {
		// "Same hash means it was already read" cannot be claimed about a read that
		// was cancelled, so the next identical frame re-reads.
		rs.lastHash = ""
	}
}

// emitReady hands the reducer every observation at the head of this region's queue
// that is ready, in frame order.
//
// While a retrieval is outstanding for this region the queue is held *behind the
// retrieval*: the session reducer refuses a ranking whose observation it has
// already been superseded by, so the box's evidence stream pauses until the reply
// lands and the next observation goes out only after it (see rankGate). The hold
// is bounded: held past OCRTimeout the observations go out anyway, and a
// retrieval that then lands is refused by the reducer's own sequence fence and
// counted. A stalled retrieval must be able to cost a frame, not cost the run.
func (rs *regionState) emitReady(r *Runner, ctx context.Context, in chan<- *obsIn) error {
	for len(rs.pending) > 0 && rs.pending[0].resolved() {
		if err := rs.holdIfGateClosed(r, ctx); err != nil {
			return err
		}
		p := rs.pending[0]
		rs.pending = rs.pending[1:]
		if err := rs.emit(r, ctx, in, p); err != nil {
			return err
		}
	}
	return nil
}

// holdIfGateClosed blocks the ROI stage while this region's retrieval is
// outstanding, so the reply is applied before any newer observation of the region
// can supersede it. Blocking — rather than deferring the hold to the next call —
// is what makes the hold effective at all: at replay speed the ROI would otherwise
// out-run the reducer and hand every observation to the mailbox before the first
// retrieval could close the gate.
//
// The hold is bounded in total per hold period: heldAt is only cleared when the
// gate actually opens, so once the OCRTimeout budget is spent the queue flows even
// if the retrieval is still missing, and a reply that lands late is then refused
// and counted by the reducer instead of held hostage.
func (rs *regionState) holdIfGateClosed(r *Runner, ctx context.Context) error {
	if !rs.gate.closed() {
		rs.heldAt = time.Time{}
		return nil
	}
	now := r.cfg.Now()
	if rs.heldAt.IsZero() {
		rs.heldAt = now
	}
	remaining := r.cfg.OCRTimeout - now.Sub(rs.heldAt)
	if remaining <= 0 {
		// Budget spent: let the stream flow; the reducer's fence handles the reply
		// that lands late.
		return nil
	}
	select {
	case <-rs.gate.opened():
		rs.heldAt = time.Time{}
		return nil
	case <-time.After(remaining):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (rs *regionState) emit(r *Runner, ctx context.Context, in chan<- *obsIn, p *pending) error {
	o := recognition.Observation{
		FrameSeq:           p.frame.Seq,
		CaptureTimestampNS: p.frame.CaptureTimestampNS,
		SourceClock:        p.frame.SourceClock,
		RegionID:           rs.region.ID,
		RegionPresent:      p.a.present,
		RegionHash:         p.a.hash,
		OCR:                p.ready,
	}
	if err := o.Validate(); err != nil {
		// An unusable observation is not fed to the tracker — recognition would put
		// the region in StateError and a pipeline defect would be reported as a
		// dialogue-box problem. It is counted as a stale drop, which is where an
		// operator looks for a frame that silently never arrived.
		r.cnt.ocrStale()
		return nil
	}
	if o.OCR == nil && !rs.deliveredOK && rs.lastOK != nil && p.a.hash != "" && p.a.hash == rs.lastOKHash {
		// The pixels have been read but the answer never reached the reducer (its
		// observation was evicted). Deliver it now rather than let the region's
		// accepted text stay one prefix behind for the rest of the box's life.
		o.OCR = rs.lastOK
	}
	if o.OCR != nil && p.a.hash == rs.lastOKHash {
		rs.deliveredOK = true
	}
	txt := ""
	if o.OCR != nil {
		txt = o.OCR.Text
	}
	var fb *OCRFallbackTrace
	if p.ready != nil && p.ready.Fallback != nil {
		f := p.ready.Fallback
		fb = &OCRFallbackTrace{PrimaryConf: f.PrimaryConf, Discrepancy: f.Discrepancy, Chosen: f.Chosen, Reason: f.Reason, SecondaryText: f.SecondaryText, SecondaryEngine: f.SecondaryEngine}
	}
	r.cnt.observations(1)
	r.cnt.recordTrace(TraceEntry{
		FrameSeq:           o.FrameSeq,
		CaptureTimestampNS: o.CaptureTimestampNS,
		RegionID:           o.RegionID,
		RegionPresent:      o.RegionPresent,
		RegionHash:         short(p.a.hash),
		OCRSkipped:         p.job == nil,
		OCRText:            txt,
		OCRFallback:        fb,
	})
	if p.job != nil {
		r.cnt.frameOCR.observe(p.readyAt.Sub(p.at))
	}
	select {
	case in <- &obsIn{region: rs.region.ID, obs: o, entered: p.at, ocrText: txt, skipped: p.job == nil, ocrAt: p.readyAt}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// fillPending attaches a completed recognition to every pending observation that
// waited on that job — which is how eight frames of one stable box produce one OCR
// call and eight observations.
func (rs *regionState) fillPending(job *ocrJob, o ocrOutcome) {
	for _, p := range rs.pending {
		if p.job == job {
			ready := o.res
			p.ready = &ready
			p.readyAt = o.done
		}
	}
	if rs.inflight == job {
		rs.inflight = nil
	}
}

// markFailed resolves a job with no result: the observations go out with a nil OCR
// so the region's timeline keeps moving.
func (rs *regionState) markFailed(job *ocrJob, o ocrOutcome) {
	for _, p := range rs.pending {
		if p.job == job {
			p.err = o.err
			p.ready = nil
			p.readyAt = o.done
		}
	}
}

// dropPendingFor removes observations whose job was superseded and reports how many.
func (rs *regionState) dropPendingFor(job *ocrJob) int {
	return rs.dropPendingForN(job)
}

func (rs *regionState) dropPendingForN(job *ocrJob) int {
	before := len(rs.pending)
	kept := rs.pending[:0]
	for _, p := range rs.pending {
		if p.job != job {
			kept = append(kept, p)
		}
	}
	rs.pending = kept
	if rs.inflight == job {
		rs.inflight = nil
	}
	return len(rs.pending) - before
}

// kill cancels a job exactly once and marks it dead, so a result arriving
// afterwards is recognisable as the stale one it is.
func (j *ocrJob) kill() {
	if j == nil {
		return
	}
	if j.cancel != nil {
		j.cancel()
	}
	j.dead = true
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
