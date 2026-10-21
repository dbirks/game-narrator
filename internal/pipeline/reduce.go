package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	replies <-chan rankIn, ranks chan<- rankJob, controls <-chan session.Msg, pub session.Publisher) error {
	defer close(ranks)
	gates := r.gates

	seq := int64(0)
	next := func() int64 { seq++; return seq }

	head := &reducerState{
		ranked:     map[string]string{},
		lifetime:   map[string]uint64{},
		byRegion:   map[string]int64{},
		anchors:    map[string]*anchor{},
		escalation: map[string]*escalationState{},
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
					// Replies already in flight get one bounded grace window to
					// land. One timer, armed exactly once: re-arming per iteration
					// would allocate in a loop that spins until the timer fires.
					// The deterministic replay (ADR-0064 amendment 2) arms no wall
					// grace at all: a reply in flight at the end of the recording
					// is the recording's own tail, and ending the run over it would
					// drop the last line on a slow host. The reply always lands (the
					// rank worker is local CPU, or the Decider's own bounded
					// deadline) or the run's context cancels; either way the run
					// returns.
					if !r.cfg.Deterministic {
						graceC = time.NewTimer(r.cfg.OCRTimeout + 500*time.Millisecond).C
					}
				}
				continue
			}
			obsRes, dispatched, err := r.applyObservation(ctx, red, head, v, ranks, next)
			if err != nil {
				return err
			}
			r.noteCommit(obsRes)
			outstanding += dispatched

		case rk, ok := <-replies:
			if !ok {
				continue
			}
			outstanding--
			// The adjudication answer, when the local scoring asked for one,
			// goes through the session's fences before the commit for this
			// observation reads it (docs/spec/05: the decision sits between the
			// scoring and the commitment policy). A reply that landed after a
			// newer commit names a generation the session has passed and is
			// discarded there — counted, not applied — and the commit proceeds
			// on the local evidence alone.
			//
			// The observation seq both are stamped with is the region's current
			// one at reply time, not the ask-time one the job carried (the
			// scenario harness's own rule, ADR-0050): a stable box keeps
			// advancing its observation seq frame after frame, and an ask-time
			// seq would make a healthy reply race every later observation of
			// the box — with the decision call in flight for up to the role's
			// timeout, the ranking would lose that race every time and the
			// commit for the line would be fenced as stale_observation. The
			// staleness the fences still owe (the box advanced while the call
			// was out) is content's and generation's: the ranking carries the
			// ask-time text (stale_text fences a changed box) and the ask-time
			// generation (stale_generation fences a dialogue that committed
			// ahead), and the decision's answer may only narrow to a candidate
			// the retrieval for the commit actually offered (the session's own
			// rule: a model may refuse but never supply).
			obsSeq := rk.req.obsSeq
			if cur := replyObsSeq(red, rk.req.region); cur > 0 {
				obsSeq = cur
			}
			decRes := r.applyDecision(ctx, red, rk, obsSeq, next)
			rankRes, err := r.applyRanking(ctx, red, head, rk, obsSeq, next)
			if g := gates[rk.req.region]; g != nil {
				g.open()
			}
			if err != nil {
				return err
			}
			r.noteCommit(rankRes)
			// The resolved event goes out for every adjudication the policy
			// asked — an applied answer, a fenced one, and a provider that
			// could not be asked (the decision.requested the wiring emitted
			// at call start needs its counterpart so the developer UI's
			// frame → OCR → candidates → Clef request → commit chain never
			// dangles): the payload says which of the three it was.
			if rk.reply.Reason != "" || rk.reply.Msg != nil {
				r.emitDecisionResolved(ctx, pub, rk, obsSeq, decRes, rankRes)
			}
			if streamClosed && outstanding == 0 {
				return nil
			}

		case ctl, ok := <-controls:
			if !ok {
				controls = nil // the operator stopped steering; stop selecting the closed channel
				continue
			}
			// This channel carries the operator's live controls and nothing else
			// (the runtime's pause/resume/settings are its only senders); a
			// different message shape on it is a wiring fault, and it fails the run
			// rather than being applied through a door it was never meant for.
			cm, isControl := ctl.(session.ControlMsg)
			if !isControl {
				return fmt.Errorf("pipeline: non-control message %T on the live-control channel", ctl)
			}
			// The control rides the same single writer as the observations, so it
			// gets the same identity stamp at the same place: the header describes
			// the world the message is sent *from*, and a live control is sent from
			// the writer itself — its own session, its current revision, and the
			// generation standing at the moment of the apply (the writer is the only
			// applier, so nothing can commit between the read and the Apply). A
			// headerless control is sent from nowhere: the session fence reads that
			// as other_session and drops it without an event — the operator's pause
			// dies in the log, and the runtime's snapshot lies "paused" on top of a
			// session that kept speaking (the 2026-10-19 G7 controls: every live
			// control was fenced out this way).
			cm.Header = session.Header{
				SessionID:       r.cfg.SessionID,
				Generation:      red.Generation(),
				ProfileRevision: r.cfg.ProfileRevision,
			}
			// The operator's live control (pause/resume/settings) lands on the
			// same single writer as every other state change: it goes through
			// Apply, so it is fenced by the same session/revision/generation
			// rules as an observation and it emits its own event
			// (session.paused, session.resumed, session.settings_changed) in the
			// stream's order. A refused control is not a run failure: a control
			// that arrives after the session has stopped is dropped (the stop
			// already owns the terminal state), and one the reducer declined as
			// a duplicate was applied by an earlier, identical message — the
			// runtime's own bookkeeping holds the applied state either way.
			if res, err := red.Apply(ctx, cm); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, session.ErrStopped) {
					continue
				}
				return err
			} else if !res.Applied && res.Reason != "" {
				_ = res.Reason
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
	// escalation is the per-region retry state for uncertain rankings
	// (ADR-0115 §1): the key being escalated, the occurrence's box lifetime,
	// how many rankings have gone out for it, and when the previous attempt's
	// reply landed (the retry's cadence is measured from there).
	escalation map[string]*escalationState
}

// escalationState is one occurrence's (box lifetime + normalized text) bounded
// retry budget (ADR-0115 §1): the first ranking is attempt one, and while the
// occurrence's ranking keeps coming back uncertain — the session policy's
// NeedsAdjudication — later stable observations re-rank it once per
// EscalationRetryAfter of source time until the attempts run out or the box's
// lifetime or text changes.
type escalationState struct {
	key      string
	lifetime uint64
	attempts int
	lastAt   time.Time
}

// applyObservation applies one observation and, when the box's state says the
// session could use candidates, dispatches at most one retrieval. It reports the
// applied result (the caller records any commit it carried) and the dispatches
// so the caller can keep the in-flight reply count honest.
func (r *Runner) applyObservation(ctx context.Context, red *session.Reducer, head *reducerState,
	v *obsIn, ranks chan<- rankJob, next func() int64) (session.Result, int, error) {
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
		return res, 0, err
	}
	return res, r.maybeRank(ctx, red, head, v, gen, ranks), nil
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
		// The predictive gate reads the session's *live* decision mode from the
		// reducer's state, not the pipeline's frozen config copy: the mode is
		// switchable mid-session (a settings control), and the config copy would
		// hold the start-time value forever — a gate that cannot see the switch
		// would keep dispatching prefix rankings after the operator chose
		// conservative (or stop before one who chose predictive). The session's
		// PredictiveMode is the single source the commitment policy and the
		// evidence half agree on; the tracker's per-region PredictiveMode is set
		// from it by the same control, so one read is the whole gate.
		waitingPredictive := rs.TextState == recognition.StateRevealing &&
			st.PredictiveMode
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
			head.escalation[rs.RegionID] = nil
		}
		if head.ranked[rs.RegionID] == rs.NormalizedKey {
			// This exact reading has already been answered — unless its answer was
			// uncertain and the escalation budget still has an attempt (ADR-0115
			// §1): the retry is the same key re-dispatched on the retry's own
			// cadence, bounded in attempts.
			esc := head.escalation[rs.RegionID]
			if esc == nil || esc.key != rs.NormalizedKey ||
				esc.attempts >= r.cfg.EscalationMaxAttempts ||
				r.cfg.Now().Sub(esc.lastAt) < r.cfg.EscalationRetryAfter {
				continue
			}
		}
		if !gate.close() {
			continue
		}
		head.ranked[rs.RegionID] = rs.NormalizedKey
		// The attempt bookkeeping (ADR-0115 §1): a re-dispatch of the same key
		// under the same lifetime consumes the next attempt; anything else — a
		// fresh occurrence, a changed key after a fenced reply — starts a new
		// budget at attempt one.
		if esc := head.escalation[rs.RegionID]; esc != nil && esc.key == rs.NormalizedKey && esc.lifetime == rs.Lifetime {
			esc.attempts++
		} else {
			head.escalation[rs.RegionID] = &escalationState{key: rs.NormalizedKey, lifetime: rs.Lifetime, attempts: 1}
		}

		req := rankRequest{
			region:      rs.RegionID,
			obsSeq:      head.byRegion[rs.RegionID],
			text:        rs.Text,
			scene:       r.cfg.SceneID,
			gen:         gen,
			entered:     v.entered,
			ocrAt:       v.ocrAt,
			frameSeq:    v.obs.FrameSeq,
			captureTSNS: v.obs.CaptureTimestampNS,
			kindHint:    r.kindHints[rs.RegionID],
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
	rk rankIn, obsSeq int64, next func() int64) (session.Result, error) {
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
		ObservationSeq: obsSeq,
		Text:           rk.req.text,
		SceneID:        rk.req.scene,
		Ranking:        rk.rank,
	})
	if err != nil {
		return res, err
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
			r.metrics.RankingStale(r.mLabels)
			// The reading was never usefully answered, so it becomes re-askable.
			// Without this the pipeline would refuse to re-rank a box whose one
			// retrieval was refused, and would go silent for the rest of that line.
			head.ranked[rk.req.region] = ""
			// The escalation budget goes with the reset: the fenced reply's
			// occurrence is re-asked as a fresh one, on a fresh budget.
			head.escalation[rk.req.region] = nil
		}
		return res, r.bookRanking(rk, res)
	}
	r.cnt.ranking()
	r.metrics.Ranking(r.mLabels)
	if rk.rank.Abstain {
		r.cnt.rankingAbstained()
		r.metrics.RankingAbstained(r.mLabels)
	}
	// The escalation bookkeeping (ADR-0115 §1), at the reply — the one place
	// the attempt's fate is known: a commit settles the occurrence; a
	// confident ranking settles it the same way the key dedupe always has;
	// only an uncertain ranking without a commit keeps its budget, stamped
	// with the reply time so the next attempt's cadence is measured from
	// here.
	switch {
	case countCommits(res.Events) > 0 || !r.cfg.Reducer.NeedsAdjudication(rk.rank):
		head.escalation[rk.req.region] = nil
	default:
		if esc := head.escalation[rk.req.region]; esc != nil {
			esc.lastAt = r.cfg.Now()
		}
	}
	if !rk.req.ocrAt.IsZero() {
		r.cnt.ocrRank.observe(rk.at.Sub(rk.req.ocrAt))
	}
	if a := head.anchors[rk.req.region]; a != nil && !a.entered.IsZero() {
		r.cnt.frameComm.observe(r.cfg.Now().Sub(a.entered))
		// The frame→voice anchor (metrics.go): this ranking applied and it
		// committed — book the frame that entered the pipeline for the line,
		// so the speech wiring's audio-start event can book the interval on
		// the telemetry surface (docs/spec/13 "first audible audio").
		if countCommits(res.Events) > 0 {
			r.noteFrameVoiceAnchor(res.Events, a.entered)
		}
	}
	return res, r.bookRanking(rk, res)
}

// replyObsSeq reports the region's current observation seq from the reducer's
// own state, for the reply-time stamp. It is read on the single writer's
// goroutine between applies — the reducer's lock is free then, and no
// observation can land between the read and the apply that uses it. Zero when
// the region is not yet known to the reducer (the ranking was dispatched from
// an applied observation, so this is a defensive path only: the caller falls
// back to the ask-time seq, and the fences decide).
func replyObsSeq(red *session.Reducer, region string) int64 {
	for _, rs := range red.State().Regions {
		if rs.RegionID == region {
			return rs.ObservationSeq
		}
	}
	return 0
}

// applyDecision applies the Decider's answer to the session's reducer before
// the commit for the same observation reads it (docs/spec/05: the adjudication
// sits between the scoring and the commitment policy, ADR-0050). The message
// the Decider returned is stamped here — the session-wide seq and the session
// and revision it belongs to — and it goes through the session's fences with
// the generation it was asked under: a reply that landed after a newer commit
// is discarded there (stale_generation), a redelivered one on its seq
// (duplicate_seq), and the commit proceeds on the local evidence alone. It
// reports the applied result (nil when the reply carried no message: the
// role abstained and there is nothing to fence), so the decision.resolved
// event can say how the session treated the answer.
func (r *Runner) applyDecision(ctx context.Context, red *session.Reducer, rk rankIn, obsSeq int64, next func() int64) *session.Result {
	if rk.reply.Msg == nil {
		return nil
	}
	dm := *rk.reply.Msg
	dm.Header.Seq = next()
	dm.Header.SessionID = r.cfg.SessionID
	dm.Header.ProfileRevision = r.cfg.ProfileRevision
	dm.Header.Generation = rk.req.gen
	// Reply-time observation seq (see the caller's note): the stamp the
	// decision is stored under, and the one the commit for the same ranking
	// reads it back with.
	dm.ObservationSeq = obsSeq
	res, err := red.Apply(ctx, dm)
	if err != nil {
		// A decision that cannot be applied is no reason to fail the run the
		// way a failed observation would: the commit for the observation still
		// has the local evidence, and the failure goes on the decision's own
		// event below rather than ending a run over an advisory answer.
		if res.Reason == "" {
			res.Reason = err.Error()
		}
		r.cnt.decisionFenced()
		r.metrics.DecisionFenced(r.mLabels)
		return &res
	}
	if res.Applied {
		r.cnt.decisionApplied()
		r.metrics.DecisionApplied(r.mLabels)
	} else {
		// A fenced answer (stale generation, duplicate seq, a superseded
		// observation) is the session's discard working as designed:
		// counted, and named on the event so the developer UI can show
		// "Clef said X, it arrived after the page had turned, the commit
		// stands on the local evidence".
		r.cnt.decisionFenced()
		r.metrics.DecisionFenced(r.mLabels)
	}
	return &res
}

// emitDecisionResolved publishes the decision.resolved event: the provider's
// verdict on the ambiguous ranking, its request id and latency (the
// correlation the developer UI joins frame → OCR → candidates → Clef request
// → commit → audio on, docs/spec/13), and how the session's fences treated
// the answer. It rides the session's publisher chain after the ranking's own
// events, causally on the candidates event the decision answers, so the
// envelope order is the wire order. obsSeq is the reply-time stamp the
// decision was applied under (the candidates event's observation_seq), so
// the developer UI's join keys agree.
func (r *Runner) emitDecisionResolved(ctx context.Context, pub session.Publisher, rk rankIn, obsSeq int64, decRes *session.Result, rankRes session.Result) {
	// The decision answers the candidates event this ranking just emitted:
	// causation and trace come from that envelope. A ranking the session
	// fenced carried no candidates event; then the decision is about an
	// observation the session no longer stands behind and there is nothing
	// to causally attach it to — the payload still goes out, unattached.
	var trace, cause string
	for _, e := range rankRes.Events {
		if e.Name == events.MessageCandidates {
			trace, cause = e.TraceID, e.EventID
			break
		}
	}
	// The fence outcome, when a message reached the fences at all; when the
	// provider could not be asked there is no fence outcome and the reply's
	// own reason is the explanation (the event is the run's record of why
	// the commit ran on the local evidence).
	applied := false
	reason := rk.reply.Reason
	if decRes != nil {
		applied = decRes.Applied
		reason = decRes.Reason
	}
	payload := map[string]any{
		"provider":        rk.reply.Provider,
		"region_id":       rk.req.region,
		"observation_seq": obsSeq,
		"generation":      rk.req.gen,
		"text":            rk.req.text,
		"accepted":        rk.reply.Accepted,
		"applied":         applied,
		"reason":          reason,
		"request_id":      rk.reply.RequestID,
		"latency_ms":      int64(rk.reply.Latency / time.Millisecond),
		"candidates":      rk.reply.Candidates,
		"choice":          rk.reply.Choice,
		"message_id":      rk.reply.ChosenID,
		"probabilities":   rk.reply.Probabilities,
		"speaker":         rk.reply.Speaker,
	}
	if rk.reply.Model != "" {
		payload["model"] = rk.reply.Model
	}
	if rk.reply.RequestIDSource != "" {
		payload["request_id_source"] = rk.reply.RequestIDSource
	}
	if rk.reply.SafeToSpeak != nil {
		payload["safe_to_speak"] = *rk.reply.SafeToSpeak
	}
	if rk.reply.Reason != "" {
		payload["decision_reason"] = rk.reply.Reason
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return // the event is diagnostics; the run reports on its own counters
	}
	env, err := pub.Publish(ctx, events.Envelope{
		SessionID:   r.cfg.SessionID,
		TraceID:     trace,
		CausationID: cause,
		Name:        events.DecisionResolved,
		OccurredAt:  r.cfg.Now(),
		Data:        data,
	})
	if err != nil {
		// The event is diagnostics, not state: a publish failure is counted on
		// the reducer's own counter if the next ranking event hits it, and the
		// run's decision counters (applied/fenced) are the record that stands.
		return
	}
	r.cnt.events(uint64(len([]events.Envelope{env})))
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
		FrameSeq:           rk.req.frameSeq,
		CaptureTimestampNS: rk.req.captureTSNS,
		RegionID:           rk.req.region,
		AcceptedText:       rk.req.text,
		TopCandidate:       top,
		TopScore:           score,
		Reason:             rk.rank.Reason,
		Event:              eventNames(res.Events),
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
