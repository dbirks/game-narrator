package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// Runner wires capture → ROI/OCR → session reducer → events, with candidate
// retrieval as a side service. It is single-use: Run owns a fresh reducer, a
// fresh worker pool and fresh per-region state, so a second Run on the same
// Runner would publish two sessions through one set of counters.
type Runner struct {
	cfg Config
	cnt *counters

	// gates is allocated per Runner (not per Run) and is only meaningful while a
	// Run is in flight: it is the retrieval hold described on rankGate.
	gates map[string]*rankGate

	// lastMsg is the id of the last message this run committed ("" before the
	// first commit). The rank worker reads it for the adjudication request's
	// context ("the last committed message, when known", docs/spec/05), so it
	// is guarded by its own lock: the reduce loop writes it, the rank worker
	// reads it from a different goroutine, and a torn read would put one
	// occurrence's id on another occurrence's request.
	lastMu  sync.Mutex
	lastMsg string
}

// lastCommitted reports the id of the last message this run committed, for
// the adjudication request's context. Safe from the rank worker's goroutine.
func (r *Runner) lastCommitted() string {
	r.lastMu.Lock()
	defer r.lastMu.Unlock()
	return r.lastMsg
}

// noteCommit records a committed message id from one Apply result's events,
// so the next adjudication request names the line the session stood behind
// when it asked. The id is read off the message.committed payload the reducer
// owns; an unparseable payload keeps the previous value (context is a hint,
// not a fence, and a missing hint must not lose a decision call).
func (r *Runner) noteCommit(res session.Result) {
	for _, e := range res.Events {
		if e.Name != events.MessageCommitted {
			continue
		}
		var p struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(e.Data, &p); err != nil || p.MessageID == "" {
			continue
		}
		r.lastMu.Lock()
		r.lastMsg = p.MessageID
		r.lastMu.Unlock()
		return
	}
}

// New validates cfg and returns a Runner ready to Run.
func New(cfg Config) (*Runner, error) {
	c := cfg.normalize()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &Runner{cfg: c, cnt: newCounters(c.Trace, c.TraceMax), gates: newGates(c.Regions)}, nil
}

// Config reports the effective configuration, including the clamps normalize
// applied, so a caller can log what actually ran rather than what it asked for.
func (r *Runner) Config() Config { return r.cfg }

// Stats snapshots the counters and latency table. Safe at any time, including
// before Run and after it returns (docs/spec/13 exposes exactly this to a health
// endpoint).
func (r *Runner) Stats() Stats { return r.cnt.snapshot() }

// Trace returns the retained timeline when Config.Trace is on, oldest first.
func (r *Runner) Trace() []TraceEntry { return r.cnt.traceSnapshot() }

// obsIn is one observation travelling to the reducer goroutine. Observations and
// ranking replies reach Apply through one sequence space, assigned there, because
// docs/spec/17 dedupes on "seq <= applied_seq" across producers and two counters
// that could disagree would make a duplicate look like a fresh message.
type obsIn struct {
	region string
	obs    recognition.Observation
	// entered is the wall instant the frame reached the ROI stage. It anchors the
	// latency table; CaptureTimestampNS stays the source clock's business
	// (docs/spec/01 invariant 1) and the two are never interchanged.
	entered time.Time
	ocrText string
	skipped bool
	ocrAt   time.Time
}

type rankIn struct {
	req  rankRequest
	rank matcher.Ranking
	at   time.Time
	// reply is the Decider's answer to this ranking, when the local scoring
	// asked for adjudication (docs/spec/05 "optional Clef/VLM adjudication"): nil
	// Msg commits on the local evidence, a non-nil Msg is applied to the
	// session's reducer before the commit for this observation.
	reply DecisionReply
}

// rankRequest is the ranking worker's whole input plus the fences the reply has
// to carry back: the observation sequence and generation believed active when the
// question was asked (docs/spec/01 invariant 2).
type rankRequest struct {
	region  string
	obsSeq  int64
	text    string
	scene   string
	gen     int64
	entered time.Time
	ocrAt   time.Time
	// frameSeq and captureTSNS stamp the ranking's trace row with the frame
	// that drove the dispatch: the row is the answer to this observation's
	// stable reading, and the harness's display-window readout (ADR-0058
	// amendment 3) correlates the row with the oracle's frames on this stamp.
	frameSeq    int64
	captureTSNS int64
}

type rankJob struct {
	req  rankRequest
	raw  string
	conf float64
}

// Run drives the chain until ctx is cancelled or src reaches end of stream. On
// cancellation it returns ctx.Err(); on a clean end of stream it returns the
// reducer's error, if any. Every goroutine it started has exited by the time it
// returns — including any OCR subprocess, whose context is a child of ctx, which
// is what "no tesseract left behind" is a test of rather than a hope.
func (r *Runner) Run(ctx context.Context, src capture.Source, pub session.Publisher) error {
	if src == nil {
		return errors.New("pipeline: a capture.Source is required")
	}
	if pub == nil {
		return errors.New("pipeline: a session.Publisher is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	red, err := session.New(r.cfg.SessionID, r.cfg.ProfileRevision, pub,
		session.WithConfig(r.cfg.Reducer),
		session.WithTrackerConfig(r.cfg.Tracker))
	if err != nil {
		return fmt.Errorf("pipeline: reducer: %w", err)
	}

	frames, err := src.Start(runCtx)
	if err != nil {
		return fmt.Errorf("pipeline: source start: %w", err)
	}

	var wg sync.WaitGroup
	bridge := r.bridge(runCtx, frames, &wg)

	in := make(chan *obsIn, r.cfg.InboundQueue)
	jobs := make(chan *ocrJob, r.cfg.Workers)
	done := make(chan ocrOutcome, r.cfg.MaxPendingOCR*len(r.cfg.Regions)+r.cfg.Workers+4)
	replies := make(chan rankIn, r.cfg.MaxRankingJobs*len(r.cfg.Regions)+2)
	ranks := make(chan rankJob, cap(replies))

	for i := 0; i < r.cfg.Workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.ocrWorker(runCtx, jobs, done) }()
		wg.Add(1)
		go func() { defer wg.Done(); r.rankWorker(runCtx, ranks, replies) }()
	}

	// The ROI stage owns its output channel and closes it when the frame stream
	// ends, which is how the reducer learns "no more observations are coming".
	//
	// The two loops run concurrently, not in series: the reducer must drain the
	// inbound mailbox while the ROI stage is still feeding it, and the ROI stage
	// must keep consuming frames while the reducer applies them. In series, a run
	// whose observation count ever exceeds the mailbox would deadlock — the
	// reducer could not start until roiLoop returned, and roiLoop could not return
	// until the mailbox drained. roiLoop closing `in` is what ends the reducer: it
	// drains the queued tail, waits for the replies it dispatched (bounded), closes
	// `ranks` on the way out so no worker blocks sending a reply nobody will read,
	// and reports through redErrC.
	redErrC := make(chan error, 1)
	go func() {
		redErrC <- r.reduceLoop(runCtx, red, in, replies, ranks, pub)
	}()
	roiErr := r.roiLoop(runCtx, bridge, in, jobs, done)
	redErr := <-redErrC

	cancel()
	_ = src.Close()
	wg.Wait()

	if cerr := runCtx.Err(); cerr != nil && !errors.Is(cerr, context.Canceled) {
		return cerr
	}
	if errors.Is(roiErr, context.Canceled) || errors.Is(redErr, context.Canceled) || errors.Is(runCtx.Err(), context.Canceled) {
		return ctx.Err()
	}
	if roiErr != nil {
		return roiErr
	}
	return redErr
}

// bridge adapts a Source's channel to the queue policy. Two shapes, one
// interface, because docs/spec/01 draws exactly this line: a bounded single-slot
// latest-frame queue for live runtime, *unlike* lossless recording.
func (r *Runner) bridge(ctx context.Context, frames <-chan capture.Frame, wg *sync.WaitGroup) <-chan capture.Frame {
	out := make(chan capture.Frame, 2)
	wg.Add(1)
	go func() {
		defer close(out)
		defer wg.Done()
		if r.cfg.Queue == QueueLossless {
			// Back-pressure instead of deletion: the replayer's channel is
			// unbuffered, so a slow ROI stage slows the replay rather than
			// silently producing a shorter recording than was captured.
			for {
				select {
				case f, ok := <-frames:
					if !ok {
						return
					}
					r.cnt.framesDelivered(1)
					select {
					case out <- f:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}
		slot := capture.NewLatestSlot()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer slot.Close()
			for {
				select {
				case f, ok := <-frames:
					if !ok {
						return
					}
					r.cnt.framesDelivered(1)
					slot.Put(f)
				case <-ctx.Done():
					return
				}
			}
		}()
		for f := range slot.Receive(ctx) {
			select {
			case out <- f:
			case <-ctx.Done():
				r.cnt.framesDropped(slot.Drops())
				return
			}
		}
		// Whatever was overwritten in the slot while a worker was behind. Reported
		// once at the end because LatestSlot's counter is cumulative.
		r.cnt.framesDropped(slot.Drops())
	}()
	return out
}

// ocrWorker runs recognitions and reports every outcome — including cancelled
// and failed ones — so each dispatch receives exactly one answer.
func (r *Runner) ocrWorker(ctx context.Context, jobs <-chan *ocrJob, done chan<- ocrOutcome) {
	for {
		select {
		case job, ok := <-jobs:
			if !ok {
				return
			}
			res, err := r.recognize(job)
			if res.Engine == "" {
				res.Engine = engineName(r.cfg.OCR)
			}
			select {
			case done <- ocrOutcome{job: job, res: res, err: err, done: r.cfg.Now()}:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// recognize runs one job's engine call with the replay-mode retry policy
// (docs/spec/12): under host load a Tesseract call can outlive its deadline, and
// a replay that loses the frame's text is no longer the recording it claims to
// be — so a timed-out or otherwise errored read is retried up to
// ReplayOCRMaxAttempts-1 more times, each attempt on a fresh deadline derived
// from the run context. The attempt budget, not an early kill, is what decides
// a slow frame's fate; what the budget cannot recover reaches applyOutcome as a
// failed read and is named there (Stats.ReplayDegradedFrames).
//
// Two classes of failure are never retried. A cancellation this pipeline issued
// (supersession, a vanished box, shutdown) is not a fault of the engine's: the
// pixels are gone or the run is over, and a retry would read a screen nobody
// wants. And once the run context is done the retries have nothing to serve.
// Live mode retries nothing: a live player has already moved on, and there the
// supersede/cancellation machinery — not a retry — is the policy.
func (r *Runner) recognize(job *ocrJob) (ocr.Result, error) {
	res, err := r.ocrRecognize(job.ctx, job)
	for attempt := 1; r.cfg.Mode == ModeReplay && err != nil && !isCancelledErr(err) &&
		attempt < ReplayOCRMaxAttempts && job.base.Err() == nil; attempt++ {
		retryCtx, cancel := context.WithTimeout(job.base, r.cfg.ReplayOCRTimeout)
		res, err = r.ocrRecognize(retryCtx, job)
		cancel()
	}
	return res, err
}

// ocrRecognize runs the configured OCR engine for one attempt, giving engines
// that implement ocr.RawCropRecognizer both inputs the analyse pass carried:
// the preprocessed crop (the local recipe's reading) and the raw crop (the
// actual pixels a natural-image secondary reads, ADR-0040, the 2026-10-18 amendment). The interface
// assertion — not a concrete-type branch — is the only coupling: an engine
// that does not implement it receives exactly what it received before, and
// the wrap in Config.normalize is untouched.
func (r *Runner) ocrRecognize(ctx context.Context, job *ocrJob) (ocr.Result, error) {
	if raw, ok := r.cfg.OCR.(ocr.RawCropRecognizer); ok {
		return raw.RecognizeWithRawCrop(ctx, job.input, job.raw)
	}
	return r.cfg.OCR.Recognize(ctx, job.input)
}

// rankWorker ranks observations off the reducer goroutine. matcher.Rank is pure
// CPU over a shared immutable Index and never touches session state: the answer
// comes back as a message the reducer is free to refuse (docs/spec/01 "candidate
// lookup is local and deadline constrained").
func (r *Runner) rankWorker(ctx context.Context, ranks <-chan rankJob, replies chan<- rankIn) {
	for {
		select {
		case j, ok := <-ranks:
			if !ok {
				return
			}
			rank := r.cfg.Index.Rank(matcher.Observation{
				Text:         j.raw,
				OCRConf:      j.conf,
				SceneID:      j.req.scene,
				TextKindHint: string(r.cfg.TextKindHint),
			}, r.cfg.RankOptions)
			// The optional Clef adjudication (docs/spec/05 "candidate retrieval →
			// graph-aware scoring → optional Clef/VLM adjudication → commitment
			// policy", ADR-0050): when the local scoring cannot settle the line on
			// its own (session.Config.NeedsAdjudication) and a Decider is wired, ask
			// it before the commit for this observation. The call is synchronous on
			// this dedicated worker — at most one adjudication is in flight per
			// session — and the Decider owns the deadline (the decision role's
			// timeout) and the breaker; a failure is an honest abstain (a nil Msg
			// with its reason), never a guess.
			var reply DecisionReply
			if r.cfg.Decider != nil && r.cfg.Reducer.NeedsAdjudication(rank) {
				r.cnt.decisions()
				reply = r.cfg.Decider.Decide(ctx, DecisionRequest{
					SessionID:       r.cfg.SessionID,
					ProfileRevision: r.cfg.ProfileRevision,
					RegionID:        j.req.region,
					ObservationSeq:  j.req.obsSeq,
					Generation:      j.req.gen,
					SceneID:         j.req.scene,
					Text:            j.req.text,
					RawText:         j.raw,
					Confidence:      j.conf,
					Ranking:         rank,
					Speakers:        r.cfg.Speakers,
					LastMessage:     r.lastCommitted(),
				})
			}
			select {
			case replies <- rankIn{req: j.req, rank: rank, at: r.cfg.Now(), reply: reply}:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// engineName resolves the engine label through ocr.Namer, which the concrete
// engines implement; an anonymous fake has no name and gets "fake" rather than an
// empty provenance field.
func engineName(e ocr.Engine) string {
	if n, ok := e.(ocr.Namer); ok {
		if s := n.Name(); s != "" {
			return s
		}
	}
	return "unnamed"
}
