package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/vision"
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

	// textCands is the placement-aware textbox region's candidate bands (ADR-0137):
	// the top / profile / bottom rects derived from the region's stored rect, in
	// that order. Empty for a region that is not placement-aware (a non-textbox
	// role, or PlacementDisabled), which crops the single fixed band exactly as
	// the historic path does. Precomputed once when the region is built: the
	// candidates are a function of the stored rect, not of any frame.
	textCands []vision.Rect
	// band is the index, in textCands, of the band the region is currently
	// cropping: the standing crop the hysteresis protects (ADR-0137). Defaults
	// to the profile's own band so frame one reads the calibrated rectangle. It
	// is touched only by the roiLoop goroutine (the same no-lock discipline as
	// the rest of regionState).
	band int
	// bandMem is the placement switch's persistence (ADR-0137 §Robustness): the
	// consecutive-frame run the best box-like band has held, so a crop switch
	// never fires on a single strong frame (the llvmpipe banner's case). Touched
	// only by the roiLoop goroutine, with band.
	bandMem placementMemory

	// lastHash is the crop hash currently being read, or the last one read
	// successfully. Equality with a new frame's crop is the OCR skip
	// (docs/spec/05 "suppress duplicate OCR calls on unchanged region hash"). It is
	// set when a recognition starts, not when it finishes, so frames arriving
	// during a slow read attach to that read instead of racing a duplicate of it.
	lastHash string

	// lastPHash is the perceptual hash (ADR-0115 §3) of the crop currently
	// being read, or the last one read. A new frame's crop within tolerance of
	// it skips OCR the way an identical hash does: the analog feed never
	// delivers two byte-identical crops, but it can keep the box perceptually
	// the same for a page of dialogue, and the tolerant skip is what lets the
	// stable box ride the last read instead of restarting one every frame.
	// Same discipline as lastHash: set when a recognition starts, cleared when
	// it is cancelled or fails.
	lastPHash uint64
	havePHash bool

	// quietRun is the perceptual skip's settle counter (ADR-0115 §3): the
	// consecutive quiet crops since the last change beyond tolerance, the last
	// one included. The skip arms only past PerceptualSkipSettle, because a
	// one-word typewriter growth moves 0–7 bits on the 9×8 grid — inside the
	// tolerance — and only a run of quiet crops distinguishes "the box has been
	// still" from "the line is still typing". It resets when the anchor moves
	// (a new read starts, a read fails, the box vanishes) and when a change
	// beyond tolerance arrives: the quiet run is about the current anchor.
	quietRun int

	// confirmed says the region's current anchor is a CONFIRMING read: a read
	// started because the perceptual skip's settle armed on pixels that were
	// never read — the final typewriter crop that rode a read of the prefix
	// pixels (ADR-0115 §3, amendment 2026-10-23). While set, a crop that is
	// perceptually unchanged but NOT the anchor's pixels skips on the anchor's
	// text instead of starting another confirming read: on the analog feed the
	// crops never repeat exactly, so "the skip armed again on unread pixels"
	// is true every settle period, and re-confirming on each one would put the
	// static box back on the engine's cadence — the per-frame cost §3 removed.
	// The one confirming read per settled box is the bounded fix. Cleared when
	// ANY read starts (a new anchor is not a confirming anchor) and with the
	// rest of the anchor memory when it goes (cancelled/failed read, box gone).
	confirmed bool

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

	// lastPresent is this region's presence verdict for its most recently
	// analysed frame (ADR-0172): the frame-level panel finder runs only
	// while no configured region carries a present box — a region that sees
	// a box owns that frame, and the fixed-region path is untouched.
	// Touched only by the roiLoop goroutine, like the rest of regionState.
	lastPresent bool
}

// roiLoop is the ROI stage: one goroutine, one select over frame arrivals, OCR
// outcomes, the probe tick and cancellation. It closes in when the frames end so
// the reducer can tell "the recording ended" from "I am still waiting".
func (r *Runner) roiLoop(ctx context.Context, frames <-chan capture.Frame, in chan<- *obsIn, jobs chan<- *ocrJob, done <-chan ocrOutcome) error {
	defer close(in)
	regions := make([]*regionState, 0, len(r.cfg.Regions))
	for _, reg := range r.cfg.Regions {
		rs := &regionState{region: reg, gate: r.gates[reg.ID]}
		// Placement-aware textbox candidates (ADR-0137): derived at run time from
		// the stored rect, so the behaviour reaches an existing profile without a
		// schema change or a re-publish. A non-textbox role, or a profile that
		// disabled placement, keeps textCands empty and crops the single fixed
		// band the historic path does.
		if !r.cfg.PlacementDisabled && reg.RegionType == games.RegionTextbox {
			rs.textCands = textBandCandidates(vision.RectOf(reg))
			rs.band = profileBandIndex(rs.textCands, vision.RectOf(reg))
		}
		regions = append(regions, rs)
	}
	// ADR-0172 (issue #53): the dynamic panel region. When the frame-level
	// panel finder sees a dialogue panel no configured region covers, the
	// panel becomes this region's rect and rides the SAME
	// presence → OCR → tracker → matcher path as a configured one (same
	// ids/events contract; its observations carry the fixed id
	// dynamicPanelRegionID, which is the "dynamic placement" mark the
	// reducer and dev UI read). It is born empty (an empty rect reads as
	// absent) and stays a fixed-band region — its rect is what panelFindTick
	// moves. Appended last: every consumer below (emitAll, ageOut, the
	// frame loop) then sees it without any further change, and a finder that
	// never fires leaves it permanently absent — the configured path is
	// bit-for-bit what it was.
	var dyn *regionState
	if r.cfg.PanelFind.Enabled {
		dyn = &regionState{region: games.Region{ID: dynamicPanelRegionID, RegionType: games.RegionTextbox}, gate: r.gates[dynamicPanelRegionID]}
		regions = append(regions, dyn)
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
			// The live view's newest frame (ADR-0110): one overwrite per frame,
			// on the frame-ordered stage, before the regions analyse it — the
			// preview's "newest pixels in memory" and the signal route's
			// active-picture judgment read this slot from their own goroutines.
			r.noteFrame(f)
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
			// ADR-0172: scan for a panel no configured region covers. Runs
			// only on its cadence and only while the configured regions see
			// no box — the fixed-region path pays nothing when it works.
			if dyn != nil {
				r.panelFindTick(regions, dyn, f)
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

// ADR-0172 (issue #53) — the frame-level panel finder's hook. The scan rule
// itself is the pure FindPanels in panelfind.go; this is its scheduler on the
// frame-ordered stage.

// dynamicPanelRegionID is the dynamic panel region's id (ADR-0172): fixed so
// the reducer's per-region tracker and the dev UI's region listing read its
// observations as a finder-placed box, not a profile region. It is the
// "dynamic placement" mark the task contract asks observations to carry.
const dynamicPanelRegionID = "panel-anywhere"

// panelFindTick runs after every configured region has looked at the frame
// (roiLoop's frame case). It scans at most every PanelFind.EveryNFrames and
// only while no configured region reports a present box — the gate that
// keeps the fixed-region path bit-for-bit unchanged when it works (ADR-0172
// §cost). A panel no configured region covers becomes the dynamic region's
// rect; when the scan finds none, the dynamic region keeps its last rect (or
// stays empty) and the ordinary presence path over that rect reports absent,
// so a box the finder loses degrades to "no evidence", never to "wrong
// text". Touched only by the roiLoop goroutine (no locks, same discipline as
// the rest of the region state).
func (r *Runner) panelFindTick(all []*regionState, dyn *regionState, f capture.Frame) {
	if f.Image == nil {
		return
	}
	n := int64(r.cfg.PanelFind.EveryNFrames)
	if n < 1 {
		n = 1
	}
	if f.Seq%n != 0 {
		return
	}
	for _, rs := range all {
		if rs != dyn && rs.lastPresent {
			// A configured region carries a box: the fixed path owns the
			// frame, and a panel near its bands is its domain anyway.
			return
		}
	}
	luma := panelLuma(f.Image, r.cfg.PanelFind.SampleW, r.cfg.PanelFind.SampleH)
	cands := FindPanels(luma, r.cfg.PanelFind.SampleW, r.cfg.PanelFind.SampleH, r.cfg.PanelFind)
	for _, p := range cands {
		if panelCoveredBy(p, all, dyn) {
			continue
		}
		dyn.region.RectX, dyn.region.RectY, dyn.region.RectW, dyn.region.RectH = p.X, p.Y, p.W, p.H
		return
	}
}

// panelCoveredBy reports whether the candidate sits where a configured region
// (its stored band, or any of its placement-aware candidate bands) can crop:
// such a panel is the region's domain, and the dynamic region stays out of it.
func panelCoveredBy(p PanelRect, all []*regionState, dyn *regionState) bool {
	for _, rs := range all {
		if rs == dyn {
			continue
		}
		if rectsOverlap(p, vision.RectOf(rs.region)) {
			return true
		}
		for _, b := range rs.textCands {
			if rectsOverlap(p, b) {
				return true
			}
		}
	}
	return false
}

// rectsOverlap is the unit-rect overlap the coverage check above needs.
func rectsOverlap(p PanelRect, r vision.Rect) bool {
	return p.X < r.X+r.W && r.X < p.X+p.W && p.Y < r.Y+r.H && r.Y < p.Y+p.H
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
				// does not exist. The next identical frame re-reads. The perceptual
				// memory is the same claim on a tolerant key: it goes too, with the
				// quiet run the skip's settle is measured on.
				rs.lastHash = ""
				rs.lastPHash = 0
				rs.havePHash = false
				rs.quietRun = 0
				rs.confirmed = false
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

			r.metrics.OCRError(r.mLabels)
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
			// than reusing a result that never arrived. The perceptual memory and its
			// quiet run go with it: the skip must not arm on a crop nobody read.
			rs.lastHash = ""
			rs.lastPHash = 0
			rs.havePHash = false
			rs.quietRun = 0
			rs.confirmed = false
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

			r.metrics.OCRFallback(r.mLabels)
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
	var deadline <-chan time.Time
	if r.cfg.Deterministic {
		// The deterministic replay (ADR-0064 amendment 2) has no wall budget
		// at end of stream: the outstanding reads are the recording's own
		// tail, and a read that outlives a timer is waited for, not dropped.
		// The per-call deadline and retry budget still bound a wedged engine;
		// the run's context bounds the rest.
	} else {
		budget := r.cfg.OCRTimeout + 200*time.Millisecond
		if r.cfg.Mode == ModeReplay {
			// A replay read is allowed its whole retry budget before EOF gives
			// up on it; the live bound would interrupt a second retry attempt.
			budget = ReplayOCRMaxAttempts*r.cfg.ReplayOCRTimeout + 200*time.Millisecond
		}
		timer := time.NewTimer(budget)
		defer timer.Stop()
		deadline = timer.C
	}
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
		case <-deadline:
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

				r.metrics.OCRCancelled(r.mLabels)
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
	if r.cfg.Deterministic {
		// The bench's deterministic replay (ADR-0064 amendment 2): a
		// wall-clock belt is exactly the load dependency the mode exists to
		// remove. The per-call deadline and the retry budget already bound a
		// wedged engine, and an observation whose read is still running is
		// waited for (waitInflight), not dropped — a frame lost here on a slow
		// host is a recognition miss the bench's verdicts would absorb.
		return
	}
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

				r.metrics.OCRCancelled(r.mLabels)
			}
			r.cnt.ocrStale()
		}
	}
}

// --- one region ------------------------------------------------------------

// placeBand reports the rect to crop for one frame (ADR-0137). For a region that
// is not placement-aware it is the profile's fixed band, exactly the historic
// crop. For a placement-aware textbox region it scores the candidate bands on
// this frame's crops (a luma pass each, no OCR) and advances rs.band through the
// sticky hysteresis, returning the selected band's rect. The caller crops that
// rect and runs the unchanged presence/dedupe/OCR machinery on it, so the
// tracker, the settle and the escalation see one stable stream the way they
// always have.
func (rs *regionState) placeBand(r *Runner, f capture.Frame) vision.Rect {
	base := vision.RectOf(rs.region)
	if len(rs.textCands) == 0 || f.Image == nil {
		return base
	}
	scores := scoreTextBands(f.Image, rs.textCands, r.cfg.Crop, r.cfg.Presence.DarkLuma)
	rs.band = pickTextBand(scores, rs.band,
		r.cfg.Presence.MinDarkShare, r.cfg.PlacementSwitchMargin, r.cfg.PlacementTextMin,
		r.cfg.PlacementTextOnPanel, &rs.bandMem, r.cfg.PlacementSwitchFrames)
	return scores[rs.band].rect
}

// accept takes one frame's look at one region: crop, presence, hash, dedupe, and
// either an OCR dispatch or the reuse observation docs/spec/05 requires.
func (rs *regionState) accept(r *Runner, ctx context.Context, f capture.Frame, in chan<- *obsIn, jobs chan<- *ocrJob) error {
	now := r.cfg.Now()
	rect := rs.placeBand(r, f)
	a := r.analyze(f, rs.region, rect)
	// ADR-0172: the frame-level panel finder's run gate (lastPresent).
	rs.lastPresent = a.present
	p := &pending{frame: f, reg: rs.region, a: a, at: now}

	switch {
	case !a.present:
		// The box is gone, or was never in this rect. No recognition is worth
		// running, and the dedupe memory is cleared: reusing the last line for a box
		// that reappeared is exactly the "same words after a menu count as one
		// occurrence" bug docs/spec/12 tests for. The perceptual memory is the
		// same claim on a tolerant key: it goes too, with the quiet run that the
		// skip's settle is measured on.
		rs.discardInflight(r)
		rs.lastHash = ""
		rs.lastPHash = 0
		rs.havePHash = false
		rs.quietRun = 0
		rs.confirmed = false
	case a.hash == rs.lastHash && rs.lastHash != "":
		// Identical pixels: no OCR. If a read of these very pixels is in flight the
		// observation attaches to it rather than starting a duplicate. The crop is
		// the quietest quiet, so the settle counter runs on it too.
		p.job = rs.inflight
		rs.quietRun++
		r.cnt.ocrSkipped()
	case rs.havePHash && a.havePHash && r.cfg.PerceptualSkipTolerance >= 0 && vision.PerceptualDistance(a.pHash, rs.lastPHash) <= r.cfg.PerceptualSkipTolerance:
		// Perceptually unchanged (ADR-0115 §3): the exact hash says the crop
		// changed — on an analog feed it changes every frame — but the picture
		// the box shows did not. The skip is settle-gated (quietRun past
		// PerceptualSkipSettle): a one-word typewriter growth moves 0–7 bits, inside
		// the tolerance, so a quiet crop inside a run of changes is a word being
		// typed, not noise, and it gets the read — the settle is what keeps the
		// tolerant skip from freezing a line at a prefix. Armed, no OCR: the
		// observation rides the in-flight read when one is outstanding (and gets
		// that read's text, which is about the same picture) or, with nothing in
		// flight, goes out on the last successful read's text (the reuse in emit).
		// Counted with the exact-hash skip: for the operator it is the same fact —
		// "OCR was skipped because the box had not changed" — and the counter is
		// where docs/spec/05's suppression is asserted.
		//
		// The confirming read (ADR-0115 §3, amendment 2026-10-23): this case only
		// runs when the crop's exact hash is NOT the anchor read's (an identical
		// crop is the case above), so "perceptually unchanged" here means the
		// picture did not change but the pixels never were the read's — the final
		// typewriter crop that rode a read of the PREFIX pixels. A settle that
		// arms on pixels nobody read may not skip, or the box freezes on the
		// anchor's text for the rest of the page: it starts ONE confirming read
		// of this crop, which re-anchors the dedupe memory on the pixels actually
		// read (startJob) and resets the quiet run so the skip re-arms on the
		// new anchor. On the analog feed the crops differ every frame, so the
		// settle arms again on unread pixels every settle period; the
		// confirming-read flag keeps that to one read per settled box — after
		// it, the re-armed skip rides the anchor's text, exactly as §3 measured.
		// If a read is in flight the crop rides it instead, and the next quiet
		// crop after it settles does the confirming read.
		rs.quietRun++
		if rs.quietRun > r.cfg.PerceptualSkipSettle {
			if rs.inflight != nil {
				// The read in flight may be of these pixels (a confirming read) or
				// of an older anchor's: ride it either way, and the next quiet
				// crop after it settles answers whether these pixels are read.
				p.job = rs.inflight
				p.lagging = true
				r.cnt.ocrDeferred()
			} else if !rs.confirmed {
				// These pixels were never read: one confirming read, re-anchoring
				// the dedupe memory on them (startJob also resets the quiet run
				// and books the anchor confirmed).
				p.job = rs.startJob(r, ctx, f.Seq, a, jobs, true)
			} else {
				// The anchor was confirmed already: the picture the box shows is
				// the one the anchor read, however the noise moved the pixels —
				// skip on its text.
				p.job = rs.inflight
				p.phashSkip = true
				r.cnt.ocrSkipped()
			}
		} else if rs.inflight != nil {
			// Quiet but the settle is not armed: the read already in flight is
			// paying for this crop's pixels, so ride it rather than start a second —
			// and the next frame re-anchors (the read settles, the crop after it
			// re-reads), which is how a slow growth still reaches OCR one frame a
			// time.
			p.job = rs.inflight
			p.lagging = true
			r.cnt.ocrDeferred()
		} else {
			// Quiet but the settle is not armed and nothing is in flight: this is
			// the word-sized change the tolerance cannot see past, so read it. The
			// new anchor follows the read (startJob) and the quiet run starts over.
			p.job = rs.startJob(r, ctx, f.Seq, a, jobs, false)
		}
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
			// starts a fresh read, exactly as before. The quiet run breaks with
			// the change: whatever settles next re-anchors it.
			rs.quietRun = 0
			p.job = rs.inflight
			p.lagging = true
			r.cnt.ocrDeferred()
		} else {
			p.job = rs.startJob(r, ctx, f.Seq, a, jobs, false)
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

				r.metrics.OCRCancelled(r.mLabels)
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
func (rs *regionState) startJob(r *Runner, ctx context.Context, frameSeq int64, a analysis, jobs chan<- *ocrJob, confirmed bool) *ocrJob {
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
		pHash:    a.pHash,
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
	// Every read re-anchors the perceptual memory on its own pixels; only the
	// one started by the armed settle's confirming read is a CONFIRMING anchor
	// (confirmed), and the skip's "the anchor was already confirmed" ride on
	// unread pixels is what keeps a settled box to one extra read (see the
	// field). A plain read — first sight of a crop, a word that just landed —
	// clears the flag: its anchor has not been confirmed, and the settle that
	// arms on its quiet crops may owe one confirming read.
	rs.confirmed = confirmed
	// The perceptual memory is set at read start, like lastHash: frames that
	// arrive during this read attach to it instead of racing a duplicate of it,
	// and a crop that is perceptually identical to the one being read must not
	// look "new" to the next frame's accept. The quiet run starts with the new
	// anchor: the skip's settle is measured on crops quiet against THIS read.
	if a.havePHash {
		rs.lastPHash = a.pHash
		rs.havePHash = true
	}
	rs.quietRun = 0
	select {
	case jobs <- job:
		// The engine is actually being called about these pixels from here on;
		// this is the number docs/spec/05's "suppress duplicate OCR calls" asserts
		// against, so it is booked at dispatch, not at completion.
		r.cnt.ocrCall()

		r.metrics.OCRCall(r.mLabels)
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

	r.metrics.OCRCancelled(r.mLabels)
	for i := 0; i < dropped; i++ {
		r.cnt.ocrStale()
	}
	if rs.lastHash == job.hash {
		// "Same hash means it was already read" cannot be claimed about a read that
		// was cancelled, so the next identical frame re-reads. The perceptual
		// memory is the same claim on a tolerant key: it goes too, with the quiet
		// run the skip's settle is measured on.
		rs.lastHash = ""
		rs.lastPHash = 0
		rs.havePHash = false
		rs.quietRun = 0
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
	if r.cfg.Deterministic {
		// The deterministic replay's hold has no wall budget (ADR-0064
		// amendment 2): the reply must land before the region's next
		// observation, so the hold is the run's context, not a timer. A reply
		// that a budget would have expired is applied in time instead, and the
		// outcome no longer depends on how fast the rank worker ran. The rank
		// worker is local CPU (or the Decider's own bounded deadline), and the
		// run's context bounds the hold either way.
		select {
		case <-rs.gate.opened():
			rs.heldAt = time.Time{}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
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
		RevealDetector:     p.a.revealDetector,
		RevealComplete:     p.a.revealComplete,
		OCRLagging:         p.lagging,
	}
	if err := o.Validate(); err != nil {
		// An unusable observation is not fed to the tracker — recognition would put
		// the region in StateError and a pipeline defect would be reported as a
		// dialogue-box problem. It is counted as a stale drop, which is where an
		// operator looks for a frame that silently never arrived.
		r.cnt.ocrStale()
		return nil
	}
	if o.OCR == nil && !rs.deliveredOK && rs.lastOK != nil && (p.phashSkip || (p.a.hash != "" && p.a.hash == rs.lastOKHash)) {
		// The pixels have been read but the answer never reached the reducer (its
		// observation was evicted), or the crop is perceptually unchanged from the
		// last read (ADR-0115 §3) and no OCR ran: deliver the last successful
		// text rather than let the region's accepted text stay one prefix behind
		// for the rest of the box's life.
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
