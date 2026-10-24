package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/recognition"
)

// Apply runs docs/spec/17 "Engine state and idempotency" literally, in the order
// that document gives it, and returns what happened:
//
//	discard if the message belongs to another session
//	discard and record(stale) if its generation is below the active one
//	discard if the profile revision differs
//	dedupe by seq
//	validate the transition from the current state
//	on a new committed textbox/page: increment generation, cancel previous audio
//	write state and emit events together, in that order
//
// The order is not stylistic. A message that failed a fence must not have
// already mutated state or emitted an event, so the fences run before the switch;
// the generation increment must be visible to the events of the same step, so the
// commit event carries the *new* generation; and the cancellation of the previous
// utterance must be emitted before the new occurrence exists, because
// docs/spec/12's stale-audio assertion is about the interval between a page
// turning and audio stopping, and an event log that put the cancellation after the
// new commit could not demonstrate the gap.
//
// The reason field of Result is one of the discard classes, never a free-form
// error string, so a test asserts on the class of refusal rather than on
// wording.
func (r *Reducer) Apply(ctx context.Context, m Msg) (Result, error) {
	if m == nil {
		return Result{Reason: "nil_message"}, nil
	}
	kind := m.MsgKind()
	res := Result{Kind: kind}
	if err := m.Validate(); err != nil {
		r.applyMu.Lock()
		r.stats.Invalid++
		r.applyMu.Unlock()
		res.Reason = "invalid:" + err.Error()
		return res, nil
	}
	h := m.MsgHeader()

	r.applyMu.Lock()
	defer r.applyMu.Unlock()

	if err := ctx.Err(); err != nil {
		res.Reason = "cancelled"
		return res, err
	}

	// --- the four fences of docs/spec/17 ---------------------------------
	if h.SessionID != r.st.SessionID {
		r.stats.OtherSession++
		res.Reason = "other_session"
		return res, nil
	}
	if r.st.Stopped && kind != KindControl {
		// A stopped session has no active generation to fence against; accepting
		// work would schedule audio that can never be heard, which is the
		// "unexplained silence" class docs/spec/13 asks the runtime to explain.
		r.stats.DroppedAfterStop++
		res.Reason = "stopped"
		return res, nil
	}
	if h.ProfileRevision != r.st.ProfileRevision {
		// Message ids are only meaningful inside one published revision
		// (docs/spec/01 "Only published profile revisions are used for runtime").
		r.stats.ProfileMismatch++
		res.Reason = "profile_revision_mismatch"
		return res, nil
	}
	if h.Generation < r.st.Generation {
		r.recordStaleLocked(m, "generation below active")
		r.stats.Stale++
		res.Reason = "stale_generation"
		return res, nil
	}
	if h.Generation > r.st.Generation {
		// The mirror of the stale rule, and just as load-bearing: a generation is
		// created only by a commit inside this reducer, so a message naming one the
		// session has not reached names a fence that does not exist. Accepting it
		// would let a producer that mis-stamped its work — or a replay of a log from
		// ahead of this session — act as though a commitment that never happened had
		// authorized it. docs/spec/17 writes only the `<` case because its pseudo-code
		// assumes a single producer; the runtime has five of them.
		r.recordStaleLocked(m, "generation above active")
		r.stats.FutureGeneration++
		res.Reason = "future_generation"
		return res, nil
	}
	if h.Seq > 0 && h.Seq <= r.st.AppliedSeq {
		// Duplicate delivery, not an out-of-order one: a lower seq arriving late
		// has already been passed by the messages that followed it, and applying
		// it again would either re-commit an occurrence or replay a cancellation.
		r.stats.Duplicated++
		res.Reason = "duplicate_seq"
		return res, nil
	}

	switch v := m.(type) {
	case ObservationMsg:
		err := r.applyObservation(ctx, v, &res)
		r.finish(&res, h.Seq, err)
		return res, err
	case RankingMsg:
		err := r.applyRanking(ctx, v, &res)
		r.finish(&res, h.Seq, err)
		return res, err
	case DecisionMsg:
		err := r.applyDecision(ctx, v, &res)
		r.finish(&res, h.Seq, err)
		return res, err
	case SpeechMsg:
		err := r.applySpeech(ctx, v, &res)
		r.finish(&res, h.Seq, err)
		return res, err
	case ControlMsg:
		err := r.applyControl(ctx, v, &res)
		r.finish(&res, h.Seq, err)
		return res, err
	default:
		r.stats.Invalid++
		res.Reason = fmt.Sprintf("unsupported_message_kind_%d", kind)
		return res, nil
	}
}

// finish books the applied counters and turns an internal reason into a Result.
// A message that was fenced is not "applied", and a message that was applied but
// changed nothing (an observation of a stable box) still consumed its seq — that
// distinction is what makes the dedupe rule testable.
func (r *Reducer) finish(res *Result, seq int64, err error) {
	if err != nil {
		if res.Reason == "" {
			res.Reason = "error"
		}
		return
	}
	if res.Reason == "" {
		res.Reason = "applied"
		res.Applied = true
		r.stats.Applied++
		if seq > r.st.AppliedSeq {
			r.st.AppliedSeq = seq
		}
	}
}

func (r *Reducer) recordStaleLocked(m Msg, why string) {
	h := m.MsgHeader()
	n := StaleNotice{
		Kind:       m.MsgKind(),
		Generation: h.Generation,
		Active:     r.st.Generation,
		Seq:        h.Seq,
		At:         r.clock.Now(),
		Why:        why,
	}
	if om, ok := m.(ObservationMsg); ok {
		n.RegionID = om.RegionID
	}
	switch v := m.(type) {
	case RankingMsg:
		n.RegionID = v.RegionID
	case DecisionMsg:
		n.RegionID = v.RegionID
	case SpeechMsg:
		n.RegionID = v.RegionID
	}
	if len(r.st.Stale) >= maxStaleNotices {
		copy(r.st.Stale, r.st.Stale[1:])
		r.st.Stale = r.st.Stale[:len(r.st.Stale)-1]
	}
	r.st.Stale = append(r.st.Stale, n)
}

// regionLocked returns the region's state, creating it on first sight. A session
// learns its regions from the observations it is fed rather than from a config
// list, because the profile's region list is the *capture* worker's business and
// a reducer that demanded the two agree would refuse to run whenever a profile
// declared a region the detector never fired on.
func (r *Reducer) regionLocked(id string) *region {
	if reg, ok := r.regions[id]; ok {
		return reg
	}
	reg := &region{id: id, tr: recognition.NewTracker(id, r.trackerCfg), settling: make(map[string]settlingLine)}
	r.regions[id] = reg
	return reg
}

// --- observation -----------------------------------------------------------

func (r *Reducer) applyObservation(ctx context.Context, m ObservationMsg, res *Result) error {
	reg := r.regionLocked(m.RegionID)
	before := reg.tr.State()
	trs := reg.tr.Step(m.Observation)

	// "validate transition from current state": the chain the tracker reports must
	// start where the reducer believes the box was. It normally cannot fail — the
	// reducer owns the tracker — but a tracker rebuilt underneath a session (a
	// config option applied late, a future Reset path) would otherwise rewrite
	// history silently, and the developer inspector would show a state machine
	// whose transitions do not line up.
	if len(trs) > 0 && trs[0].From != before {
		r.stats.RejectedState++
		res.Reason = "invalid_transition_chain"
		return nil
	}

	if lifetime := reg.tr.Lifetime(); lifetime != reg.lifetime {
		// A new box lifetime ends the previous occurrence for good. The trace id
		// changes with it — one trace per dialogue occurrence (docs/spec/01
		// "High-level events", docs/spec/09 "Link everything using trace_id"). A
		// page turn *inside* one lifetime is the other half of that rule: the
		// ADVANCING transition below mints its own trace, so a trace never holds
		// events from two occurrences. The committed content stays in
		// r.committedKeys only for *this* lifetime, so the same words coming back
		// after a dismissal are a new occurrence (docs/spec/12 "repeat NPC after
		// menu").
		reg.lifetime = lifetime
		reg.traceID = r.ids()
		reg.emittedKey = ""
		reg.occurrenceID = ""
		reg.messageID = ""
		reg.phase = PhaseNone
		reg.generation = 0
		reg.predictive = false
		reg.rankings = reg.rankings[:0]
		reg.decisions = reg.decisions[:0]
	}

	reg.lastObsSeq = m.Header.Seq
	reg.text = reg.tr.Text()
	reg.key = reg.tr.Key()
	if reg.key != reg.lastKey {
		reg.lastKey = reg.key
		reg.keySinceSeq = m.Header.Seq
	}

	// Transitions that mean something to the session.
	for _, tr := range trs {
		switch tr.To {
		case recognition.StateAdvancing:
			// docs/spec/05 "COMMITTED→ADVANCING: … cancel previous speech
			// generation". The cancellation fires *here*, on the frame that shows
			// the page turned, rather than waiting for the next line to be
			// recognized and committed: the acceptance target is a local stop
			// inside 200 ms of the advance (docs/spec/12), and the next line's OCR
			// is not coming that fast. The generation counter still moves with the
			// commit, so exactly one generation exists per committed occurrence.
			//
			// Under the catch_up policy (issue #25, ADR-0122 decision 2) the
			// playing line is left to run out — the tap batches the new line, it
			// does not cut the audio the player is hearing. The speech side owns
			// that line's terminal (it ends when its playback does), so the
			// session does not cancel it here: it would cut the very line the
			// policy keeps playing, and the line would reach no terminal of its
			// own. latest_only keeps the historical immediate cut.
			reason := CancelDialogueAdvanced
			if tr.Reason == recognition.ReasonPredictionMisfire {
				reason = CancelPredictionWrong
			}
			if !(r.cfg.CatchUp == CatchUpPolicyCatchUp && reg.phase.active()) {
				if err := r.cancelLocked(ctx, reg, reason, res); err != nil {
					return err
				}
			}
			// The page has turned: a new dialogue occurrence has started inside the
			// same box lifetime, and docs/spec/01 gives each occurrence its own
			// trace_id. The cancellation above was emitted under the old trace
			// because it stops the *previous* occurrence's audio, and the next
			// event's causation_id still names it — causation deliberately crosses
			// the trace boundary, so a reviewer walks across the turn by causation
			// and filters one occurrence by trace. Without this mint the new
			// page's text.observed, message.candidates and message.committed would
			// ride the previous page's trace, and the inspector would show one
			// trace holding both pages' commitments.
			reg.traceID = r.ids()
		case recognition.StateDismissed:
			if err := r.cancelLocked(ctx, reg, CancelRegionDismissed, res); err != nil {
				return err
			}
			reg.occurrenceID = ""
			reg.messageID = ""
			reg.phase = PhaseNone
		case recognition.StateAbsent:
			// ERROR→ABSENT (recovery). Nothing audible changes, but the box's
			// claim about the text is gone, so the pending evidence goes with it.
			reg.rankings = reg.rankings[:0]
			reg.decisions = reg.decisions[:0]
		}
	}

	// text.observed fires on a state change or on new accepted text, not on every
	// frame: docs/spec/01's backpressure rule exists to keep redundant perception
	// traffic from reaching the UI, and a 5 Hz stream of identical text would do
	// exactly that.
	if len(trs) > 0 || reg.key != reg.emittedKey {
		reg.emittedKey = reg.key
		if _, err := r.emitFor(ctx, res, reg, events.TextObserved, r.textData(reg, m, trs)); err != nil {
			return err
		}
	}

	return r.maybeCommit(ctx, reg, res)
}

// causeOf returns the event this one answers. Keeping the chain per region means
// a developer can walk frame → candidates → commitment → cancellation for one
// occurrence without joining on a trace id by hand (docs/spec/13).
func (r *Reducer) causeOf(reg *region) string { return reg.lastEventID }

// --- ranking ---------------------------------------------------------------

func (r *Reducer) applyRanking(ctx context.Context, m RankingMsg, res *Result) error {
	reg := r.regionLocked(m.RegionID)

	// An answer to a question the screen has already changed. This is the
	// out-of-order case docs/spec/12 names ("two OCR futures finish out of order"):
	// ranking #1 landing after ranking #2 is not wrong of the provider, it is
	// simply no longer about the box, and applying it would commit a line the
	// player has already passed.
	if m.ObservationSeq < reg.lastObsSeq {
		r.recordStaleLocked(m, fmt.Sprintf("observation_seq %d superseded by %d", m.ObservationSeq, reg.lastObsSeq))
		r.stats.Stale++
		res.Reason = "stale_observation"
		return nil
	}
	if m.Text != "" && m.Text != reg.text {
		r.recordStaleLocked(m, fmt.Sprintf("ranked %q but the box reads %q", m.Text, reg.text))
		r.stats.Stale++
		res.Reason = "stale_text"
		return nil
	}
	// A duplicate answer to the same question (docs/spec/12 "duplicate delivery")
	// is dropped rather than queued twice: the second copy would emit a second
	// message.candidates for one observation and make the causal chain branch.
	for i := range reg.rankings {
		if reg.rankings[i].obsSeq == m.ObservationSeq {
			r.stats.Duplicated++
			res.Reason = "duplicate_ranking"
			return nil
		}
	}

	if len(reg.rankings) >= maxRankingsPerRegion {
		reg.rankings = append(reg.rankings[1:], rankedObservation{
			obsSeq: m.ObservationSeq, text: m.Text, scene: m.SceneID, rank: m.Ranking,
		})
	} else {
		reg.rankings = append(reg.rankings, rankedObservation{
			obsSeq: m.ObservationSeq, text: m.Text, scene: m.SceneID, rank: m.Ranking,
		})
	}

	env, err := r.emitFor(ctx, res, reg, events.MessageCandidates, r.candidatesData(reg, m))
	if err != nil {
		return err
	}
	// The commitment's causation_id is this event, so a reviewer reading
	// message.committed can jump straight to the candidate list that produced it
	// (docs/spec/11 "verify screenshot, OCR, candidates, … all show the same
	// occurrence/trace IDs").
	reg.lastEventID = env.EventID

	return r.maybeCommit(ctx, reg, res)
}

// --- decision --------------------------------------------------------------

func (r *Reducer) applyDecision(ctx context.Context, m DecisionMsg, res *Result) error {
	reg := r.regionLocked(m.RegionID)
	if m.ObservationSeq < reg.lastObsSeq {
		r.recordStaleLocked(m, "observation superseded")
		r.stats.Stale++
		res.Reason = "stale_observation"
		return nil
	}
	if len(reg.decisions) >= maxRankingsPerRegion {
		reg.decisions = reg.decisions[1:]
	}
	reg.decisions = append(reg.decisions, answeredObservation{
		obsSeq: m.ObservationSeq, msgID: m.MessageID, speaker: m.SpeakerID,
		known: m.SpeakerKnown, speakerConfidence: m.SpeakerConfidence,
		speakerKind: m.SpeakerKind, speakerKindConfidence: m.SpeakerKindConfidence,
		eligible: m.SpeechEligible, accepted: m.Accepted,
		provider: m.Provider, model: m.Model,
	})
	return nil
}

// --- speech ----------------------------------------------------------------

func (r *Reducer) applySpeech(ctx context.Context, m SpeechMsg, res *Result) error {
	reg := r.regionLocked(m.RegionID)
	// The clip's occurrence must be one this box committed and still stands
	// behind: the current occurrence, or — under the catch-up policy (issue
	// #25, ADR-0122 decision 2/8) — a settling occurrence whose audio outlived
	// the successor that committed on the region. A settling line keeps its own
	// phase (the region's single phase is the current occurrence's), so its
	// terminal is applied here, not refused as an unknown occurrence.
	settling := false
	if m.OccurrenceID != "" && reg.occurrenceID == m.OccurrenceID {
		settling = false
	} else if _, ok := reg.settling[m.OccurrenceID]; ok {
		settling = true
	} else {
		// The clip claims to belong to an occurrence this box never committed, or
		// no longer stands behind. Either way there is no state to update, and
		// emitting the event would put an audio lifecycle on the wire that no
		// commitment backs — precisely what docs/spec/09 tells a reviewer to
		// distrust.
		r.stats.RejectedOccurrence++
		res.Reason = "unknown_occurrence"
		return nil
	}
	var cur SpeechPhase
	var emitOcc, emitMsg string
	if settling {
		cur = reg.settling[m.OccurrenceID].phase
		emitOcc, emitMsg = m.OccurrenceID, reg.settling[m.OccurrenceID].messageID
	} else {
		cur = reg.phase
		emitOcc, emitMsg = reg.occurrenceID, reg.messageID
	}
	next, ok := speechAdvance(cur, m.Kind)
	if !ok {
		// The guard behind docs/spec/12's "no stale audio starts": a started for a
		// cancelled or finished utterance is refused here, so a TTS worker that
		// ignores the cancellation cannot make the session claim it heard the clip.
		r.stats.RejectedState++
		res.Reason = "invalid_speech_phase"
		return nil
	}
	if settling {
		line := reg.settling[m.OccurrenceID]
		line.phase = next
		reg.settling[m.OccurrenceID] = line
		// A terminal closes the line: it leaves the settling set, the way the
		// runtime's fence drops a settled generation (ADR-0122 decision 4).
		if next == PhaseEnded || next == PhaseCancelled || next == PhaseFailed {
			delete(reg.settling, m.OccurrenceID)
		}
	} else {
		reg.phase = next
	}
	// Event names come from docs/spec/01's list, which is the single source of
	// truth: it has no speech.failed, so a failure is published as speech.ended
	// carrying result:"failed" and the error, rather than as a new name invented
	// here that no consumer would know to subscribe to.
	name := events.SpeechReady
	switch m.Kind {
	case SpeechStarted:
		name = events.SpeechStarted
	case SpeechEnded, SpeechFailed:
		name = events.SpeechEnded
	}
	data := r.speechData(reg, m)
	// A settling line's terminal names the line still being heard, not the
	// successor that committed on the region: the region's own id belongs to the
	// successor, so the payload carries the settling occurrence's own identity.
	data["dialogue_occurrence_id"] = emitOcc
	data["message_id"] = emitMsg
	_, err := r.emitFor(ctx, res, reg, name, data)
	return err
}

// speechAdvance is the utterance lifecycle. A cancelled phase may only be
// re-entered by cancellation (idempotent), never by a start: docs/spec/12 requires
// "Zero stale audio starts after a new committed dialogue event", and this table
// is where that is enforced rather than in a UI filter downstream.
func speechAdvance(cur SpeechPhase, kind SpeechKind) (SpeechPhase, bool) {
	switch kind {
	case SpeechReady:
		switch cur {
		case PhaseNone, PhaseReady:
			return PhaseReady, true
		default:
			return cur, false
		}
	case SpeechStarted:
		switch cur {
		case PhaseReady, PhaseStarted:
			return PhaseStarted, true
		default:
			return cur, false
		}
	case SpeechEnded:
		switch cur {
		case PhaseReady, PhaseStarted, PhaseEnded:
			return PhaseEnded, true
		default:
			return cur, false
		}
	case SpeechFailed:
		switch cur {
		case PhaseNone, PhaseReady, PhaseStarted, PhaseFailed:
			return PhaseFailed, true
		default:
			return cur, false
		}
	default:
		return cur, false
	}
}

// --- control ---------------------------------------------------------------

func (r *Reducer) applyControl(ctx context.Context, m ControlMsg, res *Result) error {
	switch m.Kind {
	case ControlPause:
		if r.st.Paused {
			res.Reason = "duplicate_control"
			return nil
		}
		r.st.Paused = true
		if err := r.cancelAllLocked(ctx, CancelPaused, res); err != nil {
			return err
		}
		// The session-level half of the pause is done; the event is the
		// operator's state transition (docs/spec/08: a change the UI and the
		// event log must both see) and it rides the single writer's chain,
		// after the cancellations it caused, so the order is the truth.
		// The runtime's speech wiring watches this name on the same chain
		// to stop the audio that is still queued or playing.
		if _, err := r.emitFor(ctx, res, nil, events.SessionPaused, map[string]any{
			"generation": r.st.Generation, "reason": reasonOr(m.Reason, "operator"),
		}); err != nil {
			return err
		}
	case ControlResume:
		if !r.st.Paused {
			res.Reason = "duplicate_control"
			return nil
		}
		r.st.Paused = false
		// The resume changes no belief: the text machines kept advancing
		// through the pause (the observations were real), and the commit
		// policy picks up where the evidence stands — the next box that is
		// READY with a valid ranking commits exactly once, which is why a
		// resume can never burst a backlog of the paused window's lines.
		if _, err := r.emitFor(ctx, res, nil, events.SessionResumed, map[string]any{
			"generation": r.st.Generation, "reason": reasonOr(m.Reason, "operator"),
		}); err != nil {
			return err
		}
	case ControlSettings:
		// The mid-session operator policy (docs/spec/09 "/play": the
		// conservative/predictive toggle, the narrated classes and the speech
		// side's catch-up policy, issue #25). The payload is the complete
		// target state — the runtime merged the request with the session's
		// current policy before sending — so a message that names the state
		// the session already has is a duplicate, not a change.
		kinds := normalizeNarratedKinds(m.Settings.NarratedKinds)
		catchUp, err := ValidateCatchUpPolicy(m.Settings.CatchUp)
		if err != nil {
			return err
		}
		if m.Settings.Predictive == r.cfg.PredictiveMode && kindsEqual(kinds, r.cfg.NarratedKinds) && catchUp == r.cfg.CatchUp {
			res.Reason = "duplicate_control"
			return nil
		}
		r.cfg.PredictiveMode = m.Settings.Predictive
		r.cfg.NarratedKinds = kinds
		r.cfg.CatchUp = catchUp
		for _, reg := range r.regions {
			// The text machine's half of the mode switch: a tracker left on
			// the old side would hold a box REVEALING (or keep revealing a
			// prefix the commitment policy now refuses), so both halves of
			// the policy move in one step under the one lock.
			reg.tr.SetPredictiveMode(m.Settings.Predictive)
		}
		mode := "conservative"
		if m.Settings.Predictive {
			mode = "predictive"
		}
		if _, err := r.emitFor(ctx, res, nil, events.SessionSettings, map[string]any{
			"generation":       r.st.Generation,
			"mode":             mode,
			"narrated_classes": kinds,
			"catch_up":         catchUp,
			"reason":           reasonOr(m.Reason, "operator"),
		}); err != nil {
			return err
		}
	case ControlStop:
		if r.st.Stopped {
			res.Reason = "duplicate_control"
			return nil
		}
		r.st.Stopped = true
		if err := r.cancelAllLocked(ctx, CancelStopped, res); err != nil {
			return err
		}
		if _, err := r.emitFor(ctx, res, nil, events.SessionStopped, map[string]any{
			"generation": r.st.Generation, "reason": reasonOr(m.Reason, "requested"),
		}); err != nil {
			return err
		}
	case ControlProfileChanged:
		// An atomic publish mid-session is docs/spec/12's "new profile publication
		// while session active". The revision changes, so message ids now mean
		// different things: every piece of in-flight evidence was gathered against
		// the old one and must not be applied to the new, and the occurrence
		// identities were deduped against old content keys.
		if m.NewProfileRevision == r.st.ProfileRevision {
			res.Reason = "duplicate_control"
			return nil
		}
		r.st.ProfileRevision = m.NewProfileRevision
		if err := r.cancelAllLocked(ctx, CancelProfileChanged, res); err != nil {
			return err
		}
		for _, reg := range r.regions {
			// Reset rather than continue: the vote window holds text whose meaning
			// was defined by the previous revision (docs/spec/05 "belief decays or
			// resets on … pause" is the same reasoning for a weaker event).
			reg.tr.Reset()
			reg.lifetime = 0
			reg.traceID = ""
			reg.occurrenceID = ""
			reg.messageID = ""
			reg.text, reg.key, reg.lastKey, reg.emittedKey = "", "", "", ""
			reg.rankings = reg.rankings[:0]
			reg.decisions = reg.decisions[:0]
			reg.phase = PhaseNone
			reg.generation = 0
		}
		r.committedKeys = nil
		r.st.Recent = nil
	}
	return nil
}

func reasonOr(reason, fallback string) string {
	if reason == "" {
		return fallback
	}
	return reason
}

// kindsEqual compares two normalized narrated-class lists. Both sides are the
// output of normalizeNarratedKinds (dialogue first, deduped, in input order),
// so value equality is the right test: the order is part of the shape, and a
// list that holds the same classes in a different order is the same policy
// either way — the comparison is element-wise over the normalized form, which
// is why a settings message re-sending the current state in another order is
// still a duplicate, not a change.
func kindsEqual(a, b []games.TextKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// cancelAllLocked cancels every region's live utterance. A pause or a stop is not
// one box's business: audio for any region is still audible (docs/spec/07 "User
// progression always wins").
func (r *Reducer) cancelAllLocked(ctx context.Context, reason string, res *Result) error {
	for _, id := range sortedRegionIDs(r.regions) {
		if err := r.cancelLocked(ctx, r.regions[id], reason, res); err != nil {
			return err
		}
	}
	return nil
}

func sortedRegionIDs(m map[string]*region) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// cancelLocked emits speech.cancelled if — and only if — that region's utterance
// could still be heard or was pending. The "only if" is what makes the count
// meaningful: docs/spec/12 counts cancellations per advance, and a reducer that
// announced cancellations of audio that had already finished would report a
// stop it never had to make.
func (r *Reducer) cancelLocked(ctx context.Context, reg *region, reason string, res *Result) error {
	if reg.occurrenceID == "" || !reg.phase.active() {
		return nil
	}
	data := map[string]any{
		"generation":             r.st.Generation,
		"cancelled_generation":   r.st.Generation,
		"reason":                 reason,
		"region_id":              reg.id,
		"dialogue_occurrence_id": reg.occurrenceID,
		"message_id":             reg.messageID,
		"speech_phase":           string(reg.phase),
		"predicted":              reg.predictive,
	}
	reg.phase = PhaseCancelled
	r.stats.Cancellations++
	_, err := r.emitFor(ctx, res, reg, events.SpeechCancelled, data)
	return err
}

// --- commitment ------------------------------------------------------------

// maybeCommit is the commitment policy: READY plus a non-abstaining ranking, or,
// in predictive mode only, a revealing box whose ranking carries
// PREDICTIVE_PREFIX with a margin. It commits *once* per occurrence, and a
// suppressed duplicate still moves the text machine forward, because a box that
// stayed READY would re-attempt the commit on every frame forever.
func (r *Reducer) maybeCommit(ctx context.Context, reg *region, res *Result) error {
	if reg.key == "" {
		return nil
	}
	if r.st.Paused && !r.cfg.AllowCommitWhilePaused {
		// The message is still applied — the observation or ranking was real and is
		// now part of the box's history — only the commitment waits. Overwriting
		// Result.Reason here would report the message itself as refused, which is a
		// different fact and would be a lie about the queue.
		r.stats.CommitBlocked++
		return nil
	}
	state := reg.tr.State()
	rk, ok := reg.pendingRanking(reg.text, reg.keySinceSeq)
	if !ok {
		// No evidence yet. Not an error and not a stat worth incrementing: this is
		// the ordinary state of a box between "stable" and "ranked".
		return nil
	}
	predictive := false
	switch state {
	case recognition.StateReady:
	case recognition.StateCommitted:
		return nil
	case recognition.StateRevealing:
		if !r.cfg.PredictiveMode || !reg.tr.PredictiveEligible() {
			r.stats.CommitBlocked++
			return nil
		}
		// docs/spec/05 "REVEALING→COMMITTED: only predictive mode with unique
		// strong prefix candidate, scene match and explicit risk policy", and
		// docs/spec/05 "Predict playback policy" for what "strong" means: one
		// canonical message, nontrivial length, margin over the runner-up.
		if rk.rank.Abstain || rk.rank.Reason != matcher.ReasonPredictivePrefix {
			r.stats.CommitBlocked++
			return nil
		}
		if rk.rank.Margin < r.cfg.MinPredictiveMargin {
			r.stats.CommitBlocked++
			return nil
		}
		predictive = true
	default:
		return nil
	}

	messageID, eligible, reason := r.decide(reg, rk)
	content := messageID
	if content == UnknownMessage {
		content = "text:" + reg.key
	}
	identity := committedIdentity{regionID: reg.id, lifetime: reg.lifetime, content: content}
	duplicate := r.seenCommitted(identity)

	// The tracker is asked either way. It owns the text machine's view of "this
	// line has been committed", and refusing to tell it so would leave the box
	// READY while the session believed otherwise.
	trs, err := reg.tr.Commit(recognition.CommitRequest{
		FrameSeq:           reg.tr.Snapshot().LastFrameSeq,
		CaptureTimestampNS: reg.tr.Snapshot().LastTimestampNS,
		Text:               reg.text,
		Reason:             commitReason(predictive),
		Abstained:          messageID == UnknownMessage,
	})
	if err != nil {
		// A refused commit is a policy mismatch between the two state machines,
		// which is a defect in whoever wired them (docs/spec/05 keeps the text
		// claim and the knowledge claim separate so each can be checked).
		r.stats.CommitRejected++
		return nil
	}
	if duplicate {
		// Same box, same lifetime, same content, already committed: suppress the
		// event entirely rather than emit a second commitment of one occurrence and
		// let the speech layer dedupe it (docs/spec/16 M2 "repeat observation
		// doesn't create duplicate speech" is a *commitment* property, and this is
		// where it is decided).
		reg.consumeRanking(rk.obsSeq)
		r.stats.DuplicateContent++
		return nil
	}
	r.rememberCommitted(identity)

	// A new committed textbox/page: increment the generation and cancel the
	// previous audio (docs/spec/17). The cancel has usually already fired at
	// ADVANCING; cancelLocked is idempotent, so this only covers the paths that
	// commit without an intervening advance — an instant second box, or a box that
	// reappeared with different text before the audio was audible.
	if err := r.cancelOtherLocked(ctx, reg, res); err != nil {
		return err
	}
	r.st.Generation++
	reg.generation = r.st.Generation
	reg.predictive = predictive
	// Catch-up (issue #25, ADR-0122 decision 2): the line the player was just
	// hearing is not cut by this commit — it settles, keeping its own phase so
	// its terminal (when its playback ends) can still be applied. latest_only
	// already cancelled it at the source (ADVANCING / cancelOtherLocked), so
	// its phase is not active here and nothing settles.
	if r.cfg.CatchUp == CatchUpPolicyCatchUp && reg.occurrenceID != "" && reg.phase.active() {
		if reg.settling == nil {
			reg.settling = make(map[string]settlingLine)
		}
		if _, ok := reg.settling[reg.occurrenceID]; !ok && len(reg.settling) < maxSettlingLines {
			reg.settling[reg.occurrenceID] = settlingLine{phase: reg.phase, messageID: reg.messageID}
		}
	}
	reg.phase = PhaseNone
	if reg.traceID == "" {
		reg.traceID = r.ids()
	}
	reg.occurrenceID = r.ids()
	reg.messageID = messageID

	data := r.committedData(reg, rk, messageID, eligible, reason, predictive, trs)
	env, err := r.emitFor(ctx, res, reg, events.MessageCommitted, data)
	if err != nil {
		return err
	}
	reg.consumeRanking(rk.obsSeq)
	r.stats.Commits++

	if messageID != UnknownMessage {
		r.pushRecent(messageID)
	}

	return r.maybeSceneChange(ctx, reg, rk, env.EventID, res)
}

// commitReason names the tracker's transition. A predictive commit must carry the
// matcher's own PREDICTIVE_PREFIX code (recognition.Tracker refuses any other
// reason while revealing), and an ordinary one carries the commitment reason so
// the text machine's log says which policy moved it.
func commitReason(predictive bool) string {
	if predictive {
		return recognition.ReasonPredictivePrefix
	}
	return recognition.ReasonCommitted
}

// NeedsAdjudication reports whether the commitment policy asks for Clef's
// adjudication before committing rank (docs/spec/05 "Clef adjudication and
// commit policy", ADR-0050): a ranking the local scoring abstained on, or one
// whose margin over the runner-up falls below the configured threshold — the
// ambiguous candidates the local evidence cannot settle — or, since ADR-0115 §1,
// a non-abstaining MATCH_FUZZY ranking whose top candidate's calibrated
// probability sits below AdjudicateFuzzyBelow: the live case where a real line
// reads correctly but scores fuzzy at a p the commitment policy would not
// speak, and the bounded re-ask is the mechanism the "same text keeps being
// observed" evidence needs. A ranking the local policy committed on
// confidently never spends a model call. This is the single source of truth
// for the signal: the pipeline consults it to decide whether to dispatch the
// decision at all, so "the policy asked" and "a call went out" can never
// disagree. With Adjudicate off (the default) it is always false and the
// matcher's commitment is the commitment.
func (c Config) NeedsAdjudication(rank matcher.Ranking) bool {
	if !c.Adjudicate {
		return false
	}
	if rank.Abstain {
		return true
	}
	if c.AdjudicateMargin > 0 && rank.Margin < c.AdjudicateMargin {
		return true
	}
	// The fuzzy-below-bar clause (ADR-0115 §1): a fuzzy read the local policy
	// accepted but the calibrated probability does not back. The scale is the
	// operator-visible calibrated one (Candidate.Probability), deliberately not
	// the revision-1 scale the matcher's acceptance gate runs on (ADR-0035
	// "Acceptance gate unchanged"). A disabled clause (negative) never fires;
	// zero is read as the documented default, the way normalize reads it, so a
	// config that never went through normalize still behaves as documented.
	fuzzyBar := c.AdjudicateFuzzyBelow
	if fuzzyBar == 0 {
		fuzzyBar = 0.5
	}
	if fuzzyBar > 0 && fuzzyBar < 1 && rank.Reason == matcher.ReasonFuzzy && len(rank.Candidates) > 0 &&
		fuzzyBar > rank.Candidates[0].Probability {
		return true
	}
	return false
}

// decide turns a ranking plus any advisory decision into (message id, speech
// eligibility, reason). The rules are docs/spec/05 "Clef adjudication and commit
// policy": abstention is an answer, a model may refuse but never supply a line
// the retrieval did not find, and speaking as a named actor requires the
// evidence for it.
func (r *Reducer) decide(reg *region, rk rankedObservation) (messageID string, eligible bool, reason string) {
	if rk.rank.Abstain || len(rk.rank.Candidates) == 0 {
		// "confirmed sign/item → narrator; … unclear whether text should be spoken
		// → silence. Never invent dialogue to fill uncertainty." The occurrence
		// still commits, with message_id unknown and speech_eligible false, so the
		// developer UI can show the box that was not narratable.
		//
		// The exception is the call the design exists for: at the production
		// decision margin (zero), NeedsAdjudication sends *exactly* these local
		// abstains to Clef — the shortlist is the question (the Ranking's
		// abstain shortlist is documented as the decision layer's input), and an
		// answer that narrows to a retrieved candidate is a narrowing, not an
		// invention (candidateHas). The model's safe-to-speak is the eligibility
		// evidence for the line it resolved — there is no local "speakable" verdict
		// to narrow, because the local policy's verdict was the abstention itself
		// (docs/spec/05 "speech eligibility … required before speaking"). A
		// rejected, fenced or absent answer leaves the abstain as silence.
		if ans, ok := reg.decisionFor(rk.obsSeq); ok &&
			ans.accepted && ans.msgID != "" && ans.msgID != UnknownMessage &&
			candidateHas(rk.rank, ans.msgID) {
			messageID = ans.msgID
			eligible = ans.eligible
			reason = ReasonClefDisambiguated
			if !eligible {
				reason = ReasonCommittedAbstain
			}
		} else {
			return UnknownMessage, false, ReasonCommittedAbstain
		}
	} else {
		messageID = rk.rank.Candidates[0].MessageID
		if messageID == "" {
			return UnknownMessage, false, ReasonCommittedAbstain
		}
		eligible = true
		reason = rk.rank.Reason
		if reason == "" {
			reason = ReasonCommittedKnown
		}
		if ans, ok := reg.decisionFor(rk.obsSeq); ok {
			// A decision may *narrow* the answer, never widen it: the model's choice is
			// honoured only if retrieval offered it, because the alternative lets a
			// hallucinated option outrank the local evidence (docs/spec/12 "hallucinated
			// VLM speaker", "high score on wrong option").
			if ans.accepted && ans.msgID != "" && ans.msgID != UnknownMessage && candidateHas(rk.rank, ans.msgID) {
				messageID = ans.msgID
				reason = ReasonClefDisambiguated
			}
			if ans.accepted && !ans.eligible {
				// safe_to_speak = no vetoes the utterance. The reverse is not a
				// licence: eligibility comes from the local policy, and one high
				// probability is not why audio starts.
				eligible = false
				reason = ReasonCommittedAbstain
			}
		}
	}
	if eligible && messageID != UnknownMessage && r.kindOf != nil {
		if k, ok := r.kindOf(messageID); ok && !kindIn(r.cfg.NarratedKinds, k) {
			// The narrated-class veto (docs/spec/09 "/play": the operator hears
			// dialogue, +signs, +items — whatever the setting names). The line
			// is a *known* message that simply is not one the session elected
			// to hear: it commits — the box advances and the text machine
			// knows — and it is marked not eligible, which is the speech
			// layer's "no audio" answer. A class the corpus cannot name (the
			// lookup's not-found) is not a veto: a filter that cannot say what
			// a line is must not say that it is silent.
			eligible = false
			reason = ReasonCommittedNotNarrated
		}
	}
	return messageID, eligible, reason
}

// kindIn reports whether a normalized narrated-class list holds a class.
// The list is short (the vocabulary has seven classes and dialogue is the
// default), so a linear scan is the whole implementation — a map for this
// would be ceremony.
func kindIn(kinds []games.TextKind, k games.TextKind) bool {
	for _, have := range kinds {
		if have == k {
			return true
		}
	}
	return false
}

func candidateHas(rank matcher.Ranking, id string) bool {
	for _, c := range rank.Candidates {
		if c.MessageID == id {
			return true
		}
	}
	return false
}

// cancelOtherLocked cancels any *other* region's live utterance before a new
// commitment: two boxes on screen (dialogue plus a choice list) must not both
// stand behind audio, and docs/spec/07's speech scheduler owns exactly one active
// utterance per session.
func (r *Reducer) cancelOtherLocked(ctx context.Context, next *region, res *Result) error {
	for _, id := range sortedRegionIDs(r.regions) {
		reg := r.regions[id]
		if reg == next {
			continue
		}
		if err := r.cancelLocked(ctx, reg, CancelSuperseded, res); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reducer) seenCommitted(id committedIdentity) bool {
	for _, have := range r.committedKeys {
		if have == id {
			return true
		}
	}
	return false
}

// rememberCommitted keeps the identity, bounded FIFO. The bound matters: an
// unbounded dedupe list is a per-session memory leak in the one runtime that is
// expected to run for hours of replay without restart (docs/spec/12's 1 h load
// gate), and an identity older than the last few boxes can no longer be repeated
// by a live observation anyway.
func (r *Reducer) rememberCommitted(id committedIdentity) {
	if n := len(r.committedKeys); n >= maxCommittedIdentities {
		copy(r.committedKeys, r.committedKeys[1:])
		r.committedKeys = r.committedKeys[:n-1]
	}
	r.committedKeys = append(r.committedKeys, id)
}

const maxCommittedIdentities = 128

func (r *Reducer) pushRecent(messageID string) {
	max := r.cfg.RecentMessages
	if max <= 0 {
		return
	}
	// Newest first, with duplicates collapsed in place: the list feeds a ranking
	// worker's graph context, and the same id twice would look like two hops.
	for i, id := range r.st.Recent {
		if id == messageID {
			r.st.Recent = append(r.st.Recent[:i], r.st.Recent[i+1:]...)
			break
		}
	}
	r.st.Recent = append([]string{messageID}, r.st.Recent...)
	if len(r.st.Recent) > max {
		r.st.Recent = r.st.Recent[:max]
	}
}

// maybeSceneChange emits session.scene_changed when the matcher reports a scene
// other than the last one the session believed. The first scene is recorded
// silently: there is nothing it changed *from*, and an event claiming a transition
// from nowhere would make the developer UI show a jump the player never saw.
func (r *Reducer) maybeSceneChange(ctx context.Context, reg *region, rk rankedObservation, cause string, res *Result) error {
	if rk.scene == "" {
		return nil
	}
	if r.st.SceneID == "" {
		r.st.SceneID = rk.scene
		return nil
	}
	if r.st.SceneID == rk.scene {
		return nil
	}
	from := r.st.SceneID
	r.st.SceneID = rk.scene
	r.stats.SceneChanges++
	_, err := r.emit(ctx, res, events.SessionSceneChanged, reg.traceID, cause, map[string]any{
		"from_scene":             from,
		"to_scene":               rk.scene,
		"region_id":              reg.id,
		"message_id":             reg.messageID,
		"observation_seq":        rk.obsSeq,
		"generation":             r.st.Generation,
		"dialogue_occurrence_id": reg.occurrenceID,
	})
	return err
}

// --- payload construction --------------------------------------------------

func (r *Reducer) textData(reg *region, m ObservationMsg, trs []recognition.Transition) map[string]any {
	snap := reg.tr.Snapshot()
	return map[string]any{
		"observation_seq":        m.Header.Seq,
		"region_id":              reg.id,
		"text_state":             snap.State.String(),
		"lifetime":               reg.lifetime,
		"text":                   reg.text,
		"normalized_key":         reg.key,
		"raw_text":               snap.Raw,
		"confidence":             snap.Confidence,
		"crop_hash":              m.Observation.RegionHash,
		"source_clock":           m.Observation.SourceClock,
		"capture_timestamp_ns":   m.Observation.CaptureTimestampNS,
		"transitions":            trs,
		"generation":             r.st.Generation,
		"frame_seq":              m.Observation.FrameSeq,
		"region_present":         m.Observation.RegionPresent,
		"ocr_skipped_same_hash":  reg.tr.Stats().OCRSkippedSameHash,
		"dialogue_occurrence_id": reg.occurrenceID,
	}
}

func (r *Reducer) candidatesData(reg *region, m RankingMsg) map[string]any {
	limit := r.cfg.MaxCandidates
	cands := make([]map[string]any, 0, len(m.Ranking.Candidates))
	for i, c := range m.Ranking.Candidates {
		if i >= limit {
			break
		}
		cands = append(cands, map[string]any{
			"message_id":  c.MessageID,
			"score":       c.Score,
			"probability": c.Probability,
			"components":  c.Components,
		})
	}
	return map[string]any{
		"observation_seq":        m.ObservationSeq,
		"region_id":              reg.id,
		"text":                   reg.text,
		"reason":                 m.Ranking.Reason,
		"margin":                 m.Ranking.Margin,
		"abstain":                m.Ranking.Abstain,
		"candidates":             cands,
		"candidate_total":        len(m.Ranking.Candidates),
		"scene_id":               m.SceneID,
		"generation":             r.st.Generation,
		"dialogue_occurrence_id": reg.occurrenceID,
	}
}

func (r *Reducer) committedData(reg *region, rk rankedObservation, messageID string, eligible bool, reason string, predictive bool, trs []recognition.Transition) map[string]any {
	speaker := ""
	known := false
	speakerConfidence := 0.0
	speakerKind := ""
	speakerKindConfidence := 0.0
	if ans, ok := reg.decisionFor(rk.obsSeq); ok {
		speaker, known = ans.speaker, ans.known
		speakerConfidence = ans.speakerConfidence
		speakerKind = ans.speakerKind
		speakerKindConfidence = ans.speakerKindConfidence
	}
	// The display page the matcher scored for the committed message (ADR-0184):
	// the speech wiring speaks exactly that page rather than re-deriving it
	// from the OCR read. 0 means no page was recorded (page-less message or an
	// "unknown" commit) and the historical page choice stands.
	page := 0
	for _, c := range rk.rank.Candidates {
		if c.MessageID == messageID {
			page = c.Page
			break
		}
	}
	return map[string]any{
		"dialogue_occurrence_id":  reg.occurrenceID,
		"message_id":              messageID,
		"speech_eligible":         eligible,
		"generation":              r.st.Generation,
		"reason":                  reason,
		"matcher_reason":          rk.rank.Reason,
		"margin":                  rk.rank.Margin,
		"region_id":               reg.id,
		"lifetime":                reg.lifetime,
		"observation_seq":         rk.obsSeq,
		"text":                    reg.text,
		"raw_text":                reg.tr.Snapshot().Raw,
		"normalized_text":         reg.key,
		"text_state":              reg.tr.State().String(),
		"text_variant":            reg.text,
		"speaker_id":              speaker,
		"speaker_known":           known,
		"speaker_confidence":      speakerConfidence,
		"speaker_kind":            speakerKind,
		"speaker_kind_confidence": speakerKindConfidence,
		"predictive":              predictive,
		"prediction_misfire":      false,
		"scene_id":                rk.scene,
		"transitions":             trs,
		"confidence":              reg.tr.Confidence(),
		"text_kind":               r.commitKind(messageID),
		"page":                    page,
	}
}

// commitKind is the committed message's text class for the event payload, so
// the developer UI (and the narrated-class veto's own record) can show what
// kind of line just committed. It is "" when the corpus cannot name the
// message — the honest answer, not a guess: an abstained line (the "unknown"
// sentinel) and a class the index never saw both read as "not named", and a
// payload that invented a class would be a lie about the veto's own input.
func (r *Reducer) commitKind(messageID string) string {
	if messageID == UnknownMessage || r.kindOf == nil {
		return ""
	}
	if k, ok := r.kindOf(messageID); ok {
		return string(k)
	}
	return ""
}

func (r *Reducer) speechData(reg *region, m SpeechMsg) map[string]any {
	return map[string]any{
		"dialogue_occurrence_id": reg.occurrenceID,
		"message_id":             reg.messageID,
		"generation":             m.Generation,
		"region_id":              reg.id,
		"cache_key":              m.CacheKey,
		"voice_id":               m.VoiceID,
		"voice_revision":         m.VoiceRevision,
		"latency_ms":             m.LatencyMS,
		"error":                  m.Error,
		"speech_phase":           string(reg.phase),
		"result":                 speechResult(m.Kind),
	}
}

// speechResult names the outcome in the payload, so a report can group failures
// by type without inventing an event name for them (docs/spec/05 "Derive p50/p95
// distribution by game and failure type").
func speechResult(k SpeechKind) string {
	if k == SpeechFailed {
		return "failed"
	}
	return string(k)
}

// emitFor publishes an event belonging to one region (nil region: to the session
// as a whole), wiring the trace and causation ids from that region's chain.
func (r *Reducer) emitFor(ctx context.Context, res *Result, reg *region, name string, data any) (events.Envelope, error) {
	trace, cause := "", r.lastSessionEventID
	if reg != nil {
		trace, cause = reg.traceID, reg.lastEventID
	}
	out, err := r.emit(ctx, res, name, trace, cause, data)
	if err != nil {
		return out, err
	}
	if reg != nil {
		reg.lastEventID = out.EventID
	} else {
		r.lastSessionEventID = out.EventID
	}
	return out, nil
}

// emit publishes one event. Failures are counted and returned, never swallowed: a
// session that cannot publish is degraded (docs/spec/13), and a caller that keeps
// applying state as though the log were healthy would report a run that no browser
// ever saw.
func (r *Reducer) emit(ctx context.Context, res *Result, name, traceID, causationID string, data any) (events.Envelope, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		r.stats.PublishErrors++
		r.stats.PublishErrorsLast = err.Error()
		return events.Envelope{}, fmt.Errorf("session: marshal %s: %w", name, err)
	}
	e := events.Envelope{
		SessionID:   r.st.SessionID,
		TraceID:     traceID,
		CausationID: causationID,
		Name:        name,
		OccurredAt:  r.clock.Now(),
		Data:        payload,
	}
	out, err := r.pub.Publish(ctx, e)
	if err != nil {
		r.stats.PublishErrors++
		r.stats.PublishErrorsLast = err.Error()
		return e, fmt.Errorf("session: publish %s: %w", name, err)
	}
	if out.EventID == "" {
		// The chain is only inspectable if the publisher identified the event it
		// accepted (docs/spec/01 "Every event has event_id …").
		r.stats.BrokenCausation++
	}
	r.stats.Events++
	if r.log != nil {
		r.log.Append(out)
	}
	res.Events = append(res.Events, out)
	return out, nil
}
