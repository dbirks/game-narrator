// Package runtime: the live session's Clef (System One) adjudication wiring
// (docs/spec/05 "candidate retrieval → graph-aware scoring → optional Clef/VLM
// adjudication → commitment policy → speech", ADR-0050).
//
// The runtime owns the decision role's provider client, exactly the way it
// owns the VLM OCR fallback's (ADR-0040): the pipeline never imports a
// provider adapter. What the pipeline gets is its Decider seam — one call per
// locally ambiguous ranking, between the local scoring and the commit for the
// same observation — and this file is the implementation: it renders the
// bundled System One questions (systemone.BuildAdjudication), sends them
// through the model router (breaker, priority slot, the role's per-attempt
// timeout), maps the answers back to message ids (systemone.Interpret) and
// hands the pipeline the session's DecisionMsg. The session's fences do the
// rest: a reply that lands after a newer commit is discarded on generation, a
// redelivered one on sequence, and the commit that waited for the answer
// proceeds with the narrowed candidate (CLEF_DISAMBIGUATED) or, when the role
// could not answer, on the local evidence alone.

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	ses "github.com/dbirks/game-narrator/internal/session"
)

// DecisionOptions wires the optional Clef adjudication into live sessions.
// nil (the default) keeps sessions local: the matcher's commitment stands
// without a decision gate and no decision events are emitted, the documented
// "NOT CONFIGURED" shape the role's preflight reports.
type DecisionOptions struct {
	// Router is the model router the decision calls flow through: the
	// breaker, the priority slot and the role's per-attempt timeout all
	// live there (docs/spec/06 "Routing, reliability and replicas").
	// Required when the options are non-nil.
	Router *router.Router
	// Role is the decision role's resolved configuration: its model, token,
	// protocol and timeout. A role that is not configured (or that does not
	// speak System One) keeps sessions local: the degraded mode is a nil
	// DecisionOptions, not an error.
	Role providers.RoleConfig
	// Timeout bounds the whole adjudication — every retry the router makes
	// — for one observation. Zero (the default) takes the decision role's
	// own timeout, the runtime's 1500 ms decision budget (docs/spec/06
	// "Timeout and cancellation defaults"): the loop keeps the budget and
	// skips Clef on timeout, the way the doctor's probe keeps its own.
	Timeout time.Duration
	// Margin is the commitment policy's ambiguity threshold: a ranking whose
	// margin over the runner-up falls below it (or that abstains locally)
	// holds its commit for the adjudication (session.Config.Adjudicate).
	// Zero takes the documented default (the matcher's own acceptance floor:
	// consulting Clef for a margin the local policy already accepts would
	// spend a GPU forward pass to confirm the local evidence).
	Margin float64
	// SpeakerOptions caps the speaker question's declared-speaker options
	// (the System One per-choice option cap is MaxOptions; the cap leaves
	// room for the "unknown" option the question always carries).
	SpeakerOptions int
}

// DecisionStatus is the session's view of the decision role (docs/spec/13
// "Decision/VLM down … dev dashboard marks degraded"): whether the role is
// configured, whether the last engagement degraded it, and the call counts
// the developer UI needs to explain why a line was committed locally.
type DecisionStatus struct {
	Configured bool      `json:"configured"`
	Degraded   bool      `json:"degraded"`
	Reason     string    `json:"reason,omitempty"`
	Calls      int64     `json:"calls"`
	Answered   int64     `json:"answered"`
	DegradedAt time.Time `json:"degraded_at,omitempty"`
}

// decisionWiring is one session's adjudication: the provider call (through
// the shared router) plus the per-session degraded bookkeeping. It is built
// per session in Manager.Start (like the speech wiring) and handed to the
// pipeline as its Decider. The pipeline owns the ordering — the call sits
// between the candidates and the commit for the same observation — and the
// decision.resolved event; the wiring owns the request, the deadline, the
// breaker and the decision.requested event. The pipeline's dedicated
// decision call path is the bounded concurrency: at most one adjudication
// is in flight per session (ADR-0050).
type decisionWiring struct {
	m     *Manager
	id    string // the session id the events are stamped with
	opts  DecisionOptions
	texts map[string]string // message id → the text the model sees (display text)
	// roleOK is the session's degraded mode up front: the decision workflow
	// is wired in this process, but the role behind it is not configured
	// (or does not speak System One). Sessions run local — every
	// adjudication abstains with "not_configured" — and the session status
	// says so (Configured false), which is what docs/spec/13's
	// "NOT CONFIGURED" shape is on a live session.
	roleOK bool

	mu       sync.Mutex
	degraded bool
	reason   string
	calls    int64
	answered int64
	at       time.Time
}

// newDecisionWiring returns the session's adjudication wiring, or nil when
// the process has no decision workflow wired at all (DecisionOptions nil):
// then Info.Decision stays nil and the session is simply local, the shape
// before this role existed. texts maps one published message id to the text
// the candidate options are rendered with (the canonical original text,
// falling back to the speech text); an empty map renders the stable
// placeholder instead, which must never happen for a published message.
func (m *Manager) newDecisionWiring(id string, texts map[string]string) *decisionWiring {
	opts := m.opts.Decision
	if opts == nil || opts.Router == nil {
		return nil
	}
	w := &decisionWiring{
		m:     m,
		id:    id,
		opts:  *opts,
		texts: texts,
	}
	w.roleOK = w.opts.Role.Configured() && w.opts.Role.Protocol == providers.ProtocolSystemOne
	if !w.roleOK {
		w.reason = "not_configured"
	}
	if w.opts.SpeakerOptions <= 0 {
		w.opts.SpeakerOptions = systemone.MaxOptions - 1
	}
	return w
}

// status snapshots the wiring's DecisionStatus for the session Info.
func (w *decisionWiring) status() DecisionStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return DecisionStatus{
		Configured: true,
		Degraded:   w.degraded,
		Reason:     w.reason,
		Calls:      w.calls,
		Answered:   w.answered,
		DegradedAt: w.at,
	}
}

// decide implements pipeline.Decider. It is called by the pipeline for one
// locally ambiguous ranking; the context carries the run's cancellation and
// the call bounds itself with the decision role's timeout. Every outcome —
// an answer, a fenced abstain, a provider failure — is reported on the
// reply, because the pipeline's decision.resolved event must be able to
// explain why a line was committed the way it was.
func (w *decisionWiring) Decide(ctx context.Context, req pipeline.DecisionRequest) pipeline.DecisionReply {
	reply := pipeline.DecisionReply{Provider: "systemone"}
	if !w.roleOK {
		// The workflow is wired but the role is not configured (or does not
		// speak System One): a local decision, and the session's status says
		// why (Configured false, reason "not_configured"). The pipeline's
		// decision.resolved carries the same reason, so a developer reading
		// the event log sees the local commit explained.
		reply.Reason = "not_configured"
		return reply
	}
	// The candidate options the model sees: the local shortlist, in the
	// ranking's order, capped to the request's option allowance (the
	// candidate question always carries the "none" option, so the cap
	// leaves one slot — the same cap BuildAdjudication applies).
	cands := req.Ranking.Candidates
	if len(cands) > systemone.MaxOptions-1 {
		cands = cands[:systemone.MaxOptions-1]
	}
	scands := make([]systemone.Candidate, 0, len(cands))
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		scands = append(scands, systemone.Candidate{ID: c.MessageID, Text: w.textFor(c.MessageID)})
		ids = append(ids, c.MessageID)
	}
	reply.Candidates = ids

	speakers := make([]systemone.Speaker, 0, len(req.Speakers))
	for _, s := range req.Speakers {
		if s.ID == "" {
			continue
		}
		if len(speakers) >= w.opts.SpeakerOptions {
			break
		}
		speakers = append(speakers, systemone.Speaker{ID: s.ID, Description: s.Description})
	}

	w.mu.Lock()
	w.calls++
	w.mu.Unlock()
	w.emitRequested(req)

	// The whole adjudication — retries included — stays inside the decision
	// budget (docs/spec/06 "Timeout and cancellation defaults"): the router
	// gives each attempt the role's per-attempt timeout, and this context
	// bounds the attempts collectively.
	deadline := w.opts.Timeout
	if deadline <= 0 {
		deadline = w.opts.Role.Timeout
	}
	if deadline <= 0 {
		deadline = 1500 * time.Millisecond
	}
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	obs := systemone.Context{
		Scene:       req.SceneID,
		OCR:         req.Text,
		LastMessage: req.LastMessage,
	}
	bundle := systemone.BuildAdjudication(obs, scands, speakers)
	// The screenshot (ADR-0154): the model reads the whole frame, not only the
	// OCR'd text, so a misplaced crop or a misread box is still decided from
	// the picture. No frame (a source that delivered none yet) keeps the
	// historic text-only request.
	if jpg := encodeDecisionFrame(req.Frame); jpg != nil {
		bundle.Images = [][]byte{jpg}
	}
	if len(speakers) == 0 {
		// The profile declares no speakers: the bundle's speaker question
		// would carry a single "unknown" option, which the protocol's choice
		// rule (at least two options) rejects before anything leaves the
		// process — and a profile with no declared actors has no attribution
		// to establish anyway. The question is dropped; Interpret leaves the
		// speaker unknown, the documented shape for an undeclared cast.
		qs := bundle.Questions[:0]
		for _, q := range bundle.Questions {
			if q.ID != systemone.QSpeaker {
				qs = append(qs, q)
			}
		}
		bundle.Questions = qs
	}

	var (
		dec     systemone.Decision
		callErr error
	)
	_, callErr = router.Do(dctx, w.opts.Router, providers.RoleDecision, router.CallOptions{
		Idempotent: true,
	}, func(ctx context.Context, target router.Target) (struct{}, error) {
		client, err := systemone.New(target.BaseURL, w.opts.Role.Token(), target.Model)
		if err != nil {
			return struct{}{}, err
		}
		dec, err = client.Decide(ctx, bundle)
		return struct{}{}, err
	})
	if callErr != nil && errors.Is(ctx.Err(), context.Canceled) {
		// The caller withdrew the question (a newer generation superseded
		// the read, or the session stopped): our own cancellation, not the
		// provider failing. Marking the role degraded here flashed
		// "decision degraded" on every fast advance and failed the live
		// play test with a decision answered a second earlier.
		reply.Reason = "cancelled"
		reply.Model = w.opts.Role.Model
		return reply
	}
	if callErr != nil {
		if dctx.Err() == context.DeadlineExceeded {
			callErr = budgetTimeoutError(callErr)
		}
		w.noteFailure(callErr)
		reply.Reason = "provider_error"
		reply.Model = w.opts.Role.Model
		var pe *providers.Error
		if errors.As(callErr, &pe) {
			reply.RequestID = pe.RequestID
			reply.Reason = "provider_error:" + string(pe.Kind)
			if pe.Model != "" {
				reply.Model = pe.Model
			}
		}
		return reply
	}

	adj := systemone.Interpret(dec, scands, speakers)
	w.mu.Lock()
	w.answered++
	recovered := w.degraded
	w.degraded = false
	if recovered {
		w.reason = "recovered"
	}
	w.mu.Unlock()
	if recovered {
		w.emitDegraded(false, "recovered")
	}

	choice := ""
	if a, ok := dec.Answers[systemone.QCandidate]; ok {
		choice = a.Choice
	}
	reply.RequestID = dec.RequestID
	reply.RequestIDSource = dec.RequestIDSource
	reply.Latency = dec.Latency
	reply.Model = dec.Model
	reply.Accepted = true
	reply.Reason = "answered"
	reply.Choice = choice
	reply.ChosenID = adj.Candidate
	reply.Probabilities = adj.CandidateProbabilities
	reply.Speaker = adj.Speaker
	reply.SpeakerConfidence = adj.SpeakerConfidence
	reply.SpeakerKnown = adj.Speaker != "" && adj.Speaker != systemone.SpeakerUnknown
	reply.SpeakerKind = adj.SpeakerKind
	reply.SpeakerKindConfidence = adj.SpeakerKindConfidence
	if adj.SafeToSpeak != nil {
		// The provider's safe_to_speak is a 0..1 confidence; the session's
		// speech_eligible is a flag, so the conversion is a half: a model
		// less than half sure a line should be spoken must not trigger an
		// utterance (docs/spec/05 "If unclear whether the text should be
		// spoken → silence").
		v := *adj.SafeToSpeak >= 0.5
		reply.SafeToSpeak = &v
	}

	// The session's fences stamp the message: the generation it was asked
	// under (a reply that lands after a newer commit is stale there) and the
	// observation it answers (a reply for a superseded reading is stale
	// there). The seq is assigned by the pipeline's single writer.
	msgID := adj.Candidate
	if msgID == "" {
		msgID = ses.UnknownMessage
	}
	speakerID := ""
	if adj.Speaker != "" && adj.Speaker != systemone.SpeakerUnknown {
		speakerID = adj.Speaker
	}
	eligible := false
	if adj.SafeToSpeak != nil {
		eligible = *adj.SafeToSpeak >= 0.5
	}
	reply.Msg = &ses.DecisionMsg{
		Header: ses.Header{
			SessionID:       req.SessionID,
			ProfileRevision: req.ProfileRevision,
			Generation:      req.Generation,
		},
		RegionID:              req.RegionID,
		ObservationSeq:        req.ObservationSeq,
		MessageID:             msgID,
		SpeakerID:             speakerID,
		SpeakerConfidence:     adj.SpeakerConfidence,
		SpeakerKnown:          reply.SpeakerKnown,
		SpeakerKind:           adj.SpeakerKind,
		SpeakerKindConfidence: adj.SpeakerKindConfidence,
		SpeechEligible:        eligible,
		Accepted:              true,
		Provider:              "systemone",
		Model:                 dec.Model,
	}
	return reply
}

// textFor renders one candidate option's description: the canonical original
// text the model was calibrated against, the speech text when the profile has
// no original (a synthetic line), and a stable placeholder when neither is
// stored (the option must never be blank — a blank description would make
// two candidates indistinguishable in the question).
func (w *decisionWiring) textFor(id string) string {
	if t := w.texts[id]; t != "" {
		return t
	}
	return "candidate " + id
}

// emitRequested publishes the decision.requested event: the request context
// (observation, region, generation, the text being adjudicated, the
// candidates the model is offered and the margin that made the policy ask)
// that the developer UI joins to the candidates and the commit on
// observation_seq and generation (docs/spec/13 "Correlate … trace_id").
// A failed publish degrades the diagnostics, not the run.
// budgetTimeoutError names a budget deadline as a timeout: the per-attempt
// context is a child of the adjudication's budget context, and when the
// budget runs out Go reports the child's context.Canceled — the taxonomy's
// cancellation, which is a caller's choice (the session ended). What ran out
// here is the decision's own time (docs/spec/06 "Failure taxonomy"): a
// timeout, retryable in general and named as one on the session's degraded
// view and the decision.resolved event. The original error stays the cause
// for errors.Is.
func budgetTimeoutError(cause error) error {
	var pe *providers.Error
	if !errors.As(cause, &pe) {
		return cause
	}
	out := providers.NewError(providers.KindTimeout, pe.Status, "systemone: adjudication budget exceeded")
	out.Model = pe.Model
	out.RequestID = pe.RequestID
	out.Err = cause
	return out
}

func (w *decisionWiring) emitRequested(req pipeline.DecisionRequest) {
	candData := make([]map[string]any, 0, len(req.Ranking.Candidates))
	for i, c := range req.Ranking.Candidates {
		if i >= systemone.MaxOptions-1 {
			break
		}
		candData = append(candData, map[string]any{
			"candidate_id": fmt.Sprintf("c%d", i+1),
			"message_id":   c.MessageID,
			"probability":  c.Probability,
		})
	}
	data, err := json.Marshal(map[string]any{
		"provider":        "systemone",
		"model":           w.opts.Role.Model,
		"region_id":       req.RegionID,
		"observation_seq": req.ObservationSeq,
		"generation":      req.Generation,
		"text":            req.Text,
		"scene_id":        req.SceneID,
		"margin":          req.Ranking.Margin,
		"abstain":         req.Ranking.Abstain,
		"candidates":      candData,
	})
	if err != nil {
		return
	}
	_, _ = w.m.opts.Hub.Publish(context.Background(), events.Envelope{
		SessionID:  w.id,
		Name:       events.DecisionRequested,
		OccurredAt: time.Now().UTC(),
		Data:       data,
	})
}

// noteFailure marks the session's decision role degraded (docs/spec/13
// "Decision/VLM down"): the first failure publishes provider.degraded, the
// first success after it the recovery. The state lands on the session Info
// (DecisionStatus) as well, so a snapshot poll — the degraded path the
// developer page uses — names the same condition the event log does.
func (w *decisionWiring) noteFailure(err error) {
	kind := "provider_error"
	var pe *providers.Error
	if errors.As(err, &pe) && pe.Kind != "" {
		kind = string(pe.Kind)
	}
	w.mu.Lock()
	transition := !w.degraded
	w.degraded = true
	w.reason = kind
	if transition {
		w.at = time.Now().UTC()
	}
	w.mu.Unlock()
	// Once on the first failure, silent on every repeated one: the event log
	// is a state-transition record, and the call count on the session Info
	// is where the repetition shows.
	if transition {
		w.emitDegraded(true, kind)
	}
}

// emitDegraded publishes the provider.degraded state transition (the
// scenario harness's own shape): once on the first failure, once again on
// the recovery, never on every repeated failure.
func (w *decisionWiring) emitDegraded(degraded bool, reason string) {
	data, err := json.Marshal(map[string]any{
		"role":     "decision",
		"degraded": degraded,
		"reason":   reason,
	})
	if err != nil {
		return
	}
	_, _ = w.m.opts.Hub.Publish(context.Background(), events.Envelope{
		SessionID:  w.id,
		Name:       events.ProviderDegraded,
		OccurredAt: time.Now().UTC(),
		Data:       data,
	})
}
