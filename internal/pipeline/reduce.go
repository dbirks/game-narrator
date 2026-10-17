package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/session"
)

// rankGate is the one bit of cross-goroutine state between the reducer and the ROI
// stage: "this region has a ranking outstanding, hold its observations".
//
// The hold is what makes the reducer's own staleness rule usable instead of a coin
// toss. session.Reducer refuses a ranking whose ObservationSeq is older than the
// newest observation it has applied — correctly: that is the out-of-order fence
// docs/spec/12 requires — and a runtime that emits an observation for every frame
// would otherwise race every one of its own rankings into the dust. Stamping the
// reply with whatever the newest sequence happens to be at arrival time would
// silence the fence and hide the race rather than fix it, so the box's evidence
// stream pauses behind one retrieval instead. That costs the ranking worker's
// latency — local retrieval is microseconds against an OCR call's tens of
// milliseconds — and nothing else.
type rankGate struct {
	mu       sync.Mutex
	isClosed bool
	// openCh is non-nil while the gate is closed and is closed when the
	// retrieval lands. A fresh channel per cycle, so a waiter of one retrieval
	// cannot be woken by the next one's release.
	openCh chan struct{}
}

func (g *rankGate) closed() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.isClosed
}

// close claims the slot, reporting false when one is already outstanding.
func (g *rankGate) close() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.isClosed {
		return false
	}
	g.isClosed = true
	g.openCh = make(chan struct{})
	return true
}

func (g *rankGate) open() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if !g.isClosed {
		g.mu.Unlock()
		return
	}
	g.isClosed = false
	ch := g.openCh
	g.openCh = nil
	g.mu.Unlock()
	close(ch)
}

// opened reports the channel that fires when the outstanding retrieval lands;
// nil when the gate is open, for which a select treats the case as never ready.
func (g *rankGate) opened() <-chan struct{} {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.openCh
}

// newGates gives every configured region its own retrieval hold.
func newGates(regions []games.Region) map[string]*rankGate {
	out := make(map[string]*rankGate, len(regions))
	for _, rg := range regions {
		if _, dup := out[rg.ID]; dup {
			continue
		}
		out[rg.ID] = &rankGate{}
	}
	return out
}

// reduceLoop is the serialized session writer: the only caller of Apply, the only
// place message sequences are assigned, and the only place rankings are
// dispatched. It returns when the observation stream has ended and every reply it
// dispatched has been applied (or the grace bound expired), or on cancellation.
//
// outstanding counts the ranking replies in flight: one up for every dispatch,
// one down for every applied reply. That bookkeeping is what lets the loop tell
// "no more replies are coming" from "a reply is still on its way": with it
// missing, a closed stream either returned early and the last line's retrieval
// landed after the session had gone, or spun on the closed channel re-arming a
// grace timer on every iteration and never returned at all.
func (r *Runner) reduceLoop(ctx context.Context, red *session.Reducer, in <-chan *obsIn,
	replies <-chan rankIn, ranks chan<- rankJob) error {
	defer close(ranks)
	gates := r.gates

	seq := int64(0)
	next := func() int64 { seq++; return seq }

	head := &reducerState{
		ranked:   map[string]string{},
		lifetime: map[string]uint64{},
		byRegion: map[string]int64{},
		anchors:  map[string]*anchor{},
	}

	var (
		streamClosed bool
		outstanding  int
		graceC       <-chan time.Time
	)

	for {
		select {
		case v, ok := <-in:
			if !ok {
				if !streamClosed {
					streamClosed = true
					in = nil // the stream is over; stop selecting the closed channel
					if outstanding == 0 {
						return nil
					}
					// Replies already in flight get one bounded grace window to land.
					// One timer, armed exactly once: re-arming per iteration would
					// allocate in a loop that spins until the timer fires.
					graceC = time.NewTimer(r.cfg.OCRTimeout + 500*time.Millisecond).C
				}
				continue
			}
			dispatched, err := r.applyObservation(ctx, red, head, v, ranks, next)
			if err != nil {
				return err
			}
			outstanding += dispatched

		case rk, ok := <-replies:
			if !ok {
				continue
			}
			outstanding--
			err := r.applyRanking(ctx, red, head, rk, next)
			if g := gates[rk.req.region]; g != nil {
				g.open()
			}
			if err != nil {
				return err
			}
			if streamClosed && outstanding == 0 {
				return nil
			}

		case <-ctx.Done():
			return ctx.Err()

		case <-graceC:
			// A reply that has not landed inside the grace bound is not going to
			// land. Ending with the number visible beats a Run that never returns.
			return nil
		}
	}
}

// anchor carries the wall-clock origins of one latency measurement through the
// asynchronous stages, because by the time a commit exists the observation that
// caused it is several goroutines in the past.
type anchor struct {
	entered time.Time // frame reached the ROI stage
	ocrAt   time.Time // its recognition finished (zero when OCR was skipped)
}

type reducerState struct {
	ranked   map[string]string // region → accepted key a ranking was dispatched for
	lifetime map[string]uint64
	byRegion map[string]int64 // region → last applied ObservationMsg.Seq
	anchors  map[string]*anchor
}

// applyObservation applies one observation and, when the box's state says the
// session could use candidates, dispatches at most one retrieval. It reports the
// dispatches so the caller can keep the in-flight reply count honest.
func (r *Runner) applyObservation(ctx context.Context, red *session.Reducer, head *reducerState,
	v *obsIn, ranks chan<- rankJob, next func() int64) (int, error) {
	gen := red.Generation()
	n := next()
	res, err := red.Apply(ctx, session.ObservationMsg{
		Header: session.Header{
			SessionID:       r.cfg.SessionID,
			Seq:             n,
			Generation:      gen,
			ProfileRevision: r.cfg.ProfileRevision,
		},
		RegionID:    v.region,
		Observation: v.obs,
	})
	head.byRegion[v.region] = n
	head.anchors[v.region] = &anchor{entered: v.entered, ocrAt: v.ocrAt}
	r.bookObservation(v, res)
	if err != nil {
		return 0, err
	}
	return r.maybeRank(ctx, red, head, v, gen, ranks), nil
}

// maybeRank dispatches a ranking for the region this observation belongs to, when
// the box's state means candidates could change what gets said.
//
// "Needs candidates" is READY (the commitment policy is blocked on evidence) or
// REVEALING in predictive mode (where it is blocked on a prefix candidate
// specifically). Outside those two, retrieving candidates would spend the
// appliance's CPU on a box the session cannot commit: an appearing region with no
// text yet, a dismissed one, or a typewriter in a runtime that has not opted into
// speaking prefixes (docs/spec/05 "Default conservative full-line readiness,
// enable predictive mode explicitly").
func (r *Runner) maybeRank(ctx context.Context, red *session.Reducer, head *reducerState,
	v *obsIn, gen int64, ranks chan<- rankJob) (dispatched int) {
	st := red.State()
	for _, rs := range st.Regions {
		if rs.RegionID != v.region {
			continue
		}
		ready := rs.TextState == recognition.StateReady
		waitingPredictive := rs.TextState == recognition.StateRevealing &&
			r.cfg.Reducer.PredictiveMode && r.cfg.Tracker.PredictiveMode
		if (!ready && !waitingPredictive) || rs.Text == "" {
			continue
		}
		gate := r.gates[rs.RegionID]
		if gate.closed() {
			continue // one outstanding retrieval per box
		}
		if head.lifetime[rs.RegionID] != rs.Lifetime {
			// A new box lifetime is a new occurrence even when the words are the
			// same (docs/spec/12 "repeat NPC after menu"), so the key dedupe resets.
			head.lifetime[rs.RegionID] = rs.Lifetime
			head.ranked[rs.RegionID] = ""
		}
		if head.ranked[rs.RegionID] == rs.NormalizedKey {
			continue // this exact reading has already been answered
		}
		if !gate.close() {
			continue
		}
		head.ranked[rs.RegionID] = rs.NormalizedKey

		req := rankRequest{
			region:  rs.RegionID,
			obsSeq:  head.byRegion[rs.RegionID],
			text:    rs.Text,
			scene:   r.cfg.SceneID,
			gen:     gen,
			entered: v.entered,
			ocrAt:   v.ocrAt,
		}
		if a := head.anchors[rs.RegionID]; a != nil {
			req.entered, req.ocrAt = a.entered, a.ocrAt
		}
		job := rankJob{req: req, raw: rs.RawText, conf: rs.Confidence}
		select {
		case ranks <- job:
			return 1
		case <-ctx.Done():
			gate.open()
			return 0
		}
	}
	return 0
}

func (r *Runner) applyRanking(ctx context.Context, red *session.Reducer, head *reducerState,
	rk rankIn, next func() int64) error {
	active := red.Generation()
	n := next()
	res, err := red.Apply(ctx, session.RankingMsg{
		Header: session.Header{
			SessionID:       r.cfg.SessionID,
			Seq:             n,
			Generation:      rk.req.gen,
			ProfileRevision: r.cfg.ProfileRevision,
		},
		RegionID:       rk.req.region,
		ObservationSeq: rk.req.obsSeq,
		Text:           rk.req.text,
		SceneID:        rk.req.scene,
		Ranking:        rk.rank,
	})
	if err != nil {
		return err
	}
	if res.Applied && rk.req.gen < active {
		// The invariant-2 violation this counter exists to catch: a message naming a
		// superseded generation must never change state. If it ever does, this is the
		// line that says so.
		r.cnt.staleApplied()
	}
	if !res.Applied {
		switch res.Reason {
		case "stale_observation", "stale_text", "stale_generation", "future_generation", "duplicate_ranking":
			r.cnt.rankingStale()
			// The reading was never usefully answered, so it becomes re-askable.
			// Without this the pipeline would refuse to re-rank a box whose one
			// retrieval was refused, and would go silent for the rest of that line.
			head.ranked[rk.req.region] = ""
		}
		return r.bookRanking(rk, res)
	}
	r.cnt.ranking()
	if rk.rank.Abstain {
		r.cnt.rankingAbstained()
	}
	if !rk.req.ocrAt.IsZero() {
		r.cnt.ocrRank.observe(rk.at.Sub(rk.req.ocrAt))
	}
	if a := head.anchors[rk.req.region]; a != nil && !a.entered.IsZero() {
		r.cnt.frameComm.observe(r.cfg.Now().Sub(a.entered))
	}
	return r.bookRanking(rk, res)
}

// bookRanking records a reply's counters and its trace row.
func (r *Runner) bookRanking(rk rankIn, res session.Result) error {
	r.cnt.events(uint64(len(res.Events)))
	r.cnt.commits(countCommits(res.Events))
	top, score := "", 0.0
	if len(rk.rank.Candidates) > 0 {
		top = rk.rank.Candidates[0].MessageID
		score = rk.rank.Candidates[0].Probability
	}
	r.cnt.recordTrace(TraceEntry{
		RegionID:     rk.req.region,
		AcceptedText: rk.req.text,
		TopCandidate: top,
		TopScore:     score,
		Reason:       rk.rank.Reason,
		Event:        eventNames(res.Events),
	})
	return nil
}

// bookObservation records an applied observation's event count and its trace row.
func (r *Runner) bookObservation(v *obsIn, res session.Result) {
	r.cnt.events(uint64(len(res.Events)))
	r.cnt.commits(countCommits(res.Events))
	if state := textStateOf(res.Events); state != "" {
		r.cnt.recordTrace(TraceEntry{
			FrameSeq:           v.obs.FrameSeq,
			CaptureTimestampNS: v.obs.CaptureTimestampNS,
			RegionID:           v.region,
			AcceptedText:       v.ocrText,
			State:              state,
			Event:              eventNames(res.Events),
		})
	}
}

func countCommits(evs []events.Envelope) uint64 {
	var n uint64
	for _, e := range evs {
		if e.Name == events.MessageCommitted {
			n++
		}
	}
	return n
}

func eventNames(evs []events.Envelope) string {
	parts := make([]string, 0, len(evs))
	for _, e := range evs {
		parts = append(parts, e.Name)
	}
	return strings.Join(parts, ",")
}

// textStateOf pulls text_state out of a text.observed payload. The reducer owns
// that JSON; reading one field of it is cheaper and steadier than re-reading
// session state, and it is the same string the developer UI displays.
func textStateOf(evs []events.Envelope) string {
	for _, e := range evs {
		if e.Name != events.TextObserved {
			continue
		}
		var payload struct {
			TextState string `json:"text_state"`
		}
		if err := json.Unmarshal(e.Data, &payload); err == nil {
			return payload.TextState
		}
	}
	return ""
}
