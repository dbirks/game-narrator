package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/recognition"
	ses "github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// Info is the API view of a session: a consistent snapshot of its lifecycle
// state and the pipeline counters that prove what the run did. It is a value
// copy taken under the session lock, so a reader never sees a half-updated
// row the way a direct struct read would.
type Info struct {
	ID              string         `json:"session_id"`
	ProfileID       string         `json:"profile_id"`
	ProfileRevision string         `json:"profile_revision"`
	Mode            string         `json:"mode"`
	SourceKind      string         `json:"frame_source"`
	DevTrace        bool           `json:"dev_trace"`
	State           string         `json:"state"`
	Reason          string         `json:"reason,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	EndedAt         time.Time      `json:"ended_at,omitempty"`
	EventCursor     int64          `json:"event_cursor"`
	Stats           pipeline.Stats `json:"stats"`
	// Audio is the session's speech half — the sink, the live controls, the
	// speech counters and the current utterance — present when the process
	// has a voice workflow wired and the session's sink built.
	Audio *AudioInfo `json:"audio,omitempty"`
}

// session is one pipeline run owned by the Manager. Finished sessions are
// kept (not deleted) so the developer inspector can still read the run's
// dev-trace evidence after it ends; the manager's Close is what releases
// them, which is when the per-session dev-trace stores stop being served.
type session struct {
	m        *Manager
	id       string
	cancel   context.CancelFunc
	done     chan struct{}
	runner   *pipeline.Runner
	src      capture.Source
	mu       sync.Mutex
	info     Info
	rec      *devTraceRecorder
	speech   *speechWiring // nil when the process has no speech pipeline
	stopKind string        // "", "requested", "closed" — who cancelled the run
}

func (s *session) Snapshot() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.info
	if s.speech != nil {
		info.Audio = s.speech.audioInfo()
	}
	return info
}

// setState records a terminal or transitional state; reason "" keeps the
// previous one, which is how stopping holds "requested" across the
// stopping→stopped hop.
func (s *session) setState(state, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info.State = state
	if reason != "" {
		s.info.Reason = reason
	}
	if s.info.EndedAt.IsZero() && (state == StateStopped || state == StateFailed) {
		s.info.EndedAt = time.Now().UTC()
	}
}

// Start runs the documented session workflow (docs/spec/08 "Example session
// workflow"): validate, load the published revision, build the corpus index,
// open the frame source, publish session.started, and hand the pipeline to a
// goroutine that owns the run from then on. Exactly one session may be
// starting, running or stopping at a time (docs/spec/01 "Key invariants").
//
// The returned Info carries state "starting": the goroutine flips it to
// "running" the moment the run loop begins, so the 201 response is never
// ahead of reality.
func (m *Manager) Start(ctx context.Context, req StartRequest) (Info, error) {
	if err := m.checkRequest(ctx, req); err != nil {
		return Info{}, err
	}
	mode := req.Mode
	if mode == "" {
		mode = ModeConservative
	}
	kind := req.FrameSource.Kind

	// Only the published revision runs (docs/spec/01 "Key invariants" 2): a
	// draft edit must not change what an active session's text was decided
	// against, so the corpus is always built from the published content rows.
	prof, _, err := m.opts.Store.GetProfile(ctx, req.ProfileID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Info{}, fmt.Errorf("%w: %s", ErrProfileNotFound, req.ProfileID)
		}
		return Info{}, err
	}
	published, _, err := m.opts.Store.PublishedProfile(ctx, prof.EditionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Info{}, fmt.Errorf("%w: no revision published for edition %s", ErrProfileNotPublished, prof.EditionID)
		}
		return Info{}, err
	}
	if published.ID != prof.ID {
		return Info{}, fmt.Errorf("%w: %s is revision %d but the published revision is %s",
			ErrProfileNotPublished, req.ProfileID, prof.RevisionNum, published.ID)
	}
	content, err := m.opts.Store.GetProfileContent(ctx, published.ID)
	if err != nil {
		return Info{}, err
	}
	if len(content.Regions) == 0 {
		return Info{}, fmt.Errorf("%w: %s", ErrProfileNotRunnable, req.ProfileID)
	}
	idx, err := matcher.Build(content.Messages, content.Edges, matcher.IndexOptions{})
	if err != nil {
		return Info{}, fmt.Errorf("runtime: build candidate index: %w", err)
	}
	src, err := buildFrameSource(kind, req.FrameSource.Dir, m.opts.ReplayRoots, m.opts.ReplaySpeed, m.opts.DevDolphin, m.opts.ManualSource, nil)
	if err != nil {
		return Info{}, err
	}

	id := games.NewID()
	var rec *devTraceRecorder
	if req.DevTrace {
		if m.opts.AssetsRoot == "" {
			return Info{}, ErrDevTraceUnavailable
		}
		rec, err = NewDevTraceRecorder(devTraceDir(m.opts.AssetsRoot, id), m.quota)
		if err != nil {
			return Info{}, err
		}
	}

	runCtx, cancel := context.WithCancel(context.Background()) // the run outlives the request
	runner, err := pipeline.New(m.pipelineConfig(req, published, id, content.Regions, idx, kind))
	if err != nil {
		cancel()
		_ = src.Close()
		return Info{}, fmt.Errorf("runtime: pipeline setup: %w", err)
	}

	sess := &session{
		m:      m,
		id:     id,
		cancel: cancel,
		done:   make(chan struct{}),
		runner: runner,
		src:    src,
	}
	sess.info = Info{
		ID:              id,
		ProfileID:       published.ID,
		ProfileRevision: strconv.Itoa(published.RevisionNum),
		Mode:            mode,
		SourceKind:      kind,
		DevTrace:        req.DevTrace,
		State:           StateStarting,
		CreatedAt:       time.Now().UTC(),
	}
	sess.rec = rec

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()

	// session.started is the cursor clients poll from: everything the session
	// publishes afterwards has a higher seq (docs/spec/08 SSE semantics).
	started, err := m.opts.Hub.Publish(ctx, events.Envelope{
		Name:      EventSessionStarted,
		SessionID: id,
		Data: mustJSON(map[string]any{
			"profile_id":       published.ID,
			"profile_revision": published.RevisionNum,
			"mode":             mode,
			"frame_source":     map[string]any{"kind": kind},
			"dev_trace":        req.DevTrace,
			"regions":          len(content.Regions),
		}),
	})
	if err != nil {
		m.unregister(sess)
		cancel()
		_ = src.Close()
		return Info{}, fmt.Errorf("runtime: publish session.started: %w", err)
	}
	sess.info.EventCursor = started.Seq

	if m.opts.Jobs != nil {
		// Background work pauses for the lifetime of the session
		// (docs/spec/01 "Key invariants" 1: the foreground session wins the
		// CPU/audio budget), cleared again when the run ends.
		m.opts.Jobs.SetForegroundBusy(true)
	}

	obs := &observedSource{inner: src, rec: rec, regions: content.Regions, log: m.log}
	tracePub := &tracePublisher{hub: m.opts.Hub, rec: rec, sessionID: id}
	var pub ses.Publisher = tracePub

	// The voice pipeline (docs/spec/07, ADR-0039): when the process has a
	// voice workflow wired, the session's publisher chain watches its own
	// commits — the watcher intercepts message.committed and the fenced
	// scheduler owns the playback events. A sink that cannot be built degrades
	// to a silent session (logged), never a failed run: audio output
	// misconfiguration must not take the capture down with it.
	if m.opts.Speech != nil {
		speakers := make(map[string]string, len(content.Messages))
		for _, msg := range content.Messages {
			if msg.SpeakerID != "" {
				speakers[msg.ID] = msg.SpeakerID
			}
		}
		if w := m.newSpeechWiring(id, speakers, runCtx); w != nil {
			sess.speech = w
			pub = &commitWatcher{w: w, inner: tracePub}
		}
	}

	go sess.run(runCtx, obs, pub)

	snap := sess.Snapshot()
	return snap, nil
}

// checkRequest validates the request before anything is touched, so a bad
// body costs nothing on the box.
func (m *Manager) checkRequest(ctx context.Context, req StartRequest) error {
	m.mu.Lock()
	closed := m.closed
	var blocker *session
	for _, s := range m.sessions {
		if st := s.Snapshot().State; st == StateStarting || st == StateRunning || st == StateStopping {
			blocker = s
			break
		}
	}
	m.mu.Unlock()
	if closed {
		return ErrManagerClosed
	}
	if blocker != nil {
		return fmt.Errorf("%w: %s is %s", ErrAnotherSessionRunning, blocker.id, blocker.Snapshot().State)
	}
	switch req.Mode {
	case "", ModeConservative, ModePredictive:
	default:
		return fmt.Errorf("%w: %q", ErrInvalidMode, req.Mode)
	}
	switch req.FrameSource.Kind {
	case SourceKindReplay, SourceKindTestPattern, SourceKindV4L2, SourceKindDolphin, SourceKindManual:
	default:
		return fmt.Errorf("%w: %q", ErrUnknownSourceKind, req.FrameSource.Kind)
	}
	return nil
}

// pipelineConfig assembles the pipeline from the published content and the
// session's settings. Replay runs in ModeReplay (lossless, the recording it
// claims to be); every other source runs live.
func (m *Manager) pipelineConfig(req StartRequest, published games.Profile, sessionID string, regions []games.Region, idx *matcher.Index, kind string) pipeline.Config {
	mode := pipeline.ModeLive
	// The manual source (the test seam) hands the pipeline a recording it
	// claims to be — the clip's own frames on the clip's own source clock,
	// pushed by hand — so it runs the lossless replay semantics too:
	// every pushed frame is processed in order (no latest-slot drops), and
	// the run's outcome is a pure function of the frames, which is exactly
	// the determinism the push-wait-push protocol of the speech tests needs
	// (docs/spec/12 "deterministic delay-injection suite").
	if kind == SourceKindReplay || kind == SourceKindManual {
		mode = pipeline.ModeReplay
	}
	engine := m.opts.OCR
	if engine == nil {
		engine = ocr.Tesseract{}
	}
	cfg := pipeline.Config{
		Mode:            mode,
		SessionID:       sessionID,
		ProfileRevision: strconv.Itoa(published.RevisionNum),
		Regions:         regions,
		OCR:             engine,
		Index:           idx,
		Reducer:         ses.Config{},
		Tracker:         m.trackerConfig(kind),
		Preprocess:      vision.DefaultSteps(),
		Crop:            vision.CropOptions{},
		// A textbox region is dialogue (the synthetic corpus and the documented
		// default are both dialogue boxes); per-region text-kind hints land with
		// profile-level OCR settings, not one session's guess.
		TextKindHint: games.TextDialogue,
	}
	if fb := m.opts.OCRFallback; fb != nil && fb.Secondary != nil {
		// One FallbackEngine per session (its rate limiter and crop cache are
		// per-run state, ADR-0040 §2): the Secondary engine is shared across
		// sessions (stateless) and the Policy is the process default with the
		// session's regions' difficult_font flag merged in — the documented
		// knob for a font the local engine is known to defeat.
		policy := fb.Policy
		if difficultFontIn(regions) {
			policy.DifficultFont = true
		}
		cfg.OCRFallback = &pipeline.OCRFallback{Secondary: fb.Secondary, Policy: policy}
		// The ocr.fallback field is recorded in the pipeline's bounded
		// timeline and the session's terminal event carries that timeline, so
		// a fallback session keeps the timeline (512 rows, in-memory, no
		// durable write). The trace stays off where the fallback is off — the
		// event log remains the durable record and the timeline's cost is the
		// cost of the feature, not of every session.
		cfg.Trace = true
	}
	return cfg
}

// difficultFontIn reports whether one of the session's regions flags
// difficult_font in its ocr_settings — the profile-level flag ADR-0040
// names ("the profile's DifficultFont flag"), stored as the
// {"difficult_font": true} member of the regions.ocr_settings JSON object.
// The session's OCR engine is one engine shared by all of the pipeline's
// regions (pipeline.Config.OCR), so the session-level policy takes the
// conservative union: a single hard-font region runs the secondary on every
// frame of every region, and the rate limit and crop cache bound the cost
// (one call per distinct crop, at most one per second).
//
// Malformed ocr_settings (a row the profile validator should never admit)
// reads as "no flag", not an error: the recognition path must not fail a
// session over a store-shape surprise it did not cause.
func difficultFontIn(regions []games.Region) bool {
	for _, r := range regions {
		if len(r.OCRSettings) == 0 {
			continue
		}
		var s struct {
			DifficultFont bool `json:"difficult_font"`
		}
		if err := json.Unmarshal(r.OCRSettings, &s); err != nil {
			continue
		}
		if s.DifficultFont {
			return true
		}
	}
	return false
}

// trackerConfig takes the caller's explicit settings when given, otherwise
// the documented defaults — sized to the source's cadence. A replay session
// is paced at the recording's own 10 Hz clip cadence, where page dwell is
// 400 ms; the 250 ms default stability window (sized for 60 fps live play)
// would commit page one and miss page two, so replay sessions take the
// 100 ms window and the 3-frame vote window the pipeline's own frame-replay
// test documents ("tracker tuned to the clip's 10 Hz cadence — documented
// settings, not fixture branches"). Live sources keep the documented
// defaults.
func (m *Manager) trackerConfig(kind string) recognition.Config {
	if m.opts.Tracker != (recognition.Config{}) {
		return m.opts.Tracker
	}
	cfg := recognition.DefaultConfig()
	// The manual source speaks the recording's recorded clock (its frames
	// carry the clip's timestamps), so the tracker is sized the same way a
	// replay session's is: the 10 fps cadence is what the knobs assume.
	if kind == SourceKindReplay || kind == SourceKindManual {
		cfg.StableWindow = 100 * time.Millisecond
		cfg.VoteWindow = 3
	}
	return cfg
}

// run owns a session from start to its terminal event. It is the only place
// the foreground-busy flag is released and the terminal events are published,
// so a crash mid-run cannot leave the box looking busy or the session
// missing its session.stopped.
func (s *session) run(runCtx context.Context, src *observedSource, pub ses.Publisher) {
	defer close(s.done)
	s.setState(StateRunning, "")
	s.m.log.Info("session running",
		"session_id", s.id, "profile_id", s.info.ProfileID,
		"revision", s.info.ProfileRevision, "mode", s.info.Mode,
		"source", src.ID(), "dev_trace", s.info.DevTrace)

	err := s.runner.Run(runCtx, src, pub)

	// The session's audio ends with the run: pending synthesis is cancelled,
	// active playback stops within the scheduler's bound, the sink releases
	// its resources — all before the terminal events go out, so the run's
	// final state is the session's final state.
	s.stopSpeech()

	st := s.runner.Stats()
	// The run's bounded timeline (frame→state→text→candidate→event, capped at
	// the pipeline's 512 rows) travels with the terminal events: it is the
	// ocr.fallback trace field's home in the session's event stream
	// (docs/spec/05 "include both and the discrepancy", ADR-0040) and the
	// evidence-bundle half of the run that the event log keeps.
	trace := s.runner.Trace()
	if trace == nil {
		trace = []pipeline.TraceEntry{}
	}

	s.mu.Lock()
	kind := s.stopKind
	s.mu.Unlock()

	var state, reason string
	switch {
	case kind == "closed":
		state, reason = StateStopped, StopReasonClosed
	case kind == "requested":
		state, reason = StateStopped, StopReasonRequested
	case err == nil:
		state, reason = StateStopped, StopReasonSourceEnd
	default:
		state, reason = StateFailed, StopReasonError
	}

	// The run context is done by now; the terminal events get a fresh one so
	// the inspector's "did it end?" question is always answerable.
	stopCtx := context.Background()
	if state == StateFailed {
		if _, perr := s.m.opts.Hub.Publish(stopCtx, events.Envelope{
			Name:      EventSessionFailed,
			SessionID: s.id,
			Data:      mustJSON(map[string]any{"error": truncateError(err), "stats": st, "trace": trace}),
		}); perr != nil {
			s.m.log.Warn("publish session.failed", "session_id", s.id, "error", perr)
		}
	}
	if _, perr := s.m.opts.Hub.Publish(stopCtx, events.Envelope{
		Name:      events.SessionStopped,
		SessionID: s.id,
		Data:      mustJSON(map[string]any{"reason": reason, "stats": st, "trace": trace}),
	}); perr != nil {
		s.m.log.Warn("publish session.stopped", "session_id", s.id, "error", perr)
	}

	s.mu.Lock()
	s.info.Stats = st
	s.mu.Unlock()
	s.setState(state, reason)

	if s.m.opts.Jobs != nil {
		s.m.opts.Jobs.SetForegroundBusy(false)
	}
	if s.rec != nil {
		s.m.log.Info("dev trace retained",
			"session_id", s.id, "samples", s.rec.sampleCount(), "bytes", s.rec.totalBytes())
	}
	s.m.log.Info("session ended",
		"session_id", s.id, "state", state, "reason", reason,
		"frames", st.FramesDelivered, "commits", st.Commits, "ocr_calls", st.OCRCalls)
}

// Stop is idempotent: stopping a stopping/stopped/failed session returns its
// current state without touching anything. For a live session it cancels the
// run and waits for finalization so the returned snapshot is terminal when
// the run has actually ended.
func (m *Manager) Stop(id string) (Info, error) {
	s, ok := m.sessionByID(id)
	if !ok {
		return Info{}, ErrSessionNotFound
	}
	s.mu.Lock()
	switch s.info.State {
	case StateStarting, StateRunning:
		s.info.State = StateStopping
		s.stopKind = "requested"
	case StateStopping, StateStopped, StateFailed:
		s.mu.Unlock()
		return s.Snapshot(), nil
	}
	cancel := s.cancel
	s.mu.Unlock()
	cancel()
	select {
	case <-s.done:
	case <-time.After(stopSettleTimeout):
		m.log.Warn("stop settle timed out; run is finalizing in the background", "session_id", id)
	}
	return s.Snapshot(), nil
}

// Get returns a session's current snapshot, active or finished.
func (m *Manager) Get(id string) (Info, bool) {
	s, ok := m.sessionByID(id)
	if !ok {
		return Info{}, false
	}
	return s.Snapshot(), true
}

// SetAudio applies the operator's live audio controls to one session
// (docs/spec/07 "Apply volume, short fade, optional delay calibration"): the
// change is effective at the next fence-checked decision — the next start
// for mute and delay, the next sample for volume. The body is partial: a nil
// field keeps its current value. The answer is the session's fresh snapshot,
// so the caller sees the applied state, not just an acknowledgement.
func (m *Manager) SetAudio(id string, s AudioSettings) (Info, error) {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return Info{}, ErrManagerClosed
	}
	sess, ok := m.sessionByID(id)
	if !ok {
		return Info{}, ErrSessionNotFound
	}
	if sess.speech == nil {
		return Info{}, ErrAudioUnavailable
	}
	if s.Volume != nil && (*s.Volume < 0 || *s.Volume > 1) {
		return Info{}, fmt.Errorf("%w: volume must be in 0..1, got %v", ErrInvalidAudioSettings, *s.Volume)
	}
	if s.SpeechDelayMS != nil && (*s.SpeechDelayMS < 0 || *s.SpeechDelayMS > speechDelayMaxMS) {
		return Info{}, fmt.Errorf("%w: speech_delay_ms must be in 0..%d, got %d", ErrInvalidAudioSettings, speechDelayMaxMS, *s.SpeechDelayMS)
	}
	sess.speech.setAudio(s)
	return sess.Snapshot(), nil
}

// stopSpeech ends the session's audio pipeline (nil when unwired). It is
// bounded by the scheduler's own stop bound, so a wedged sink cannot hold
// the run's finalisation open.
func (s *session) stopSpeech() {
	if s.speech != nil {
		s.speech.stop()
	}
}

// sessionByID looks a session up; finished sessions stay readable until Close.
func (m *Manager) sessionByID(id string) (*session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *Manager) unregister(s *session) {
	m.mu.Lock()
	delete(m.sessions, s.id)
	m.mu.Unlock()
}

// OpenDevAsset serves one dev-trace blob by content hash from the store of any
// session this manager has run. Content addressing means the same hash is the
// same bytes in any session's store, so the answer does not depend on which
// store holds it.
func (m *Manager) OpenDevAsset(ctx context.Context, hash string) (io.ReadCloser, assets.Info, error) {
	if !assets.ValidHash(hash) {
		return nil, assets.Info{}, fmt.Errorf("runtime: %w", assets.ErrInvalidHash)
	}
	m.mu.Lock()
	recs := make([]*devTraceRecorder, 0, len(m.sessions))
	for _, s := range m.sessions {
		s.mu.Lock()
		rec := s.rec
		s.mu.Unlock()
		if rec != nil {
			recs = append(recs, rec)
		}
	}
	m.mu.Unlock()

	for _, rec := range recs {
		rc, info, err := rec.asset(ctx, hash)
		if err == nil {
			return rc, info, nil
		}
		if !errors.Is(err, assets.ErrNotFound) {
			return nil, assets.Info{}, err
		}
	}
	return nil, assets.Info{}, fmt.Errorf("runtime: dev-trace asset %s…: %w", hash[:8], assets.ErrNotFound)
}

// Close stops every live session (reason "closed") and waits, bounded, for
// them to finalize. It is the server's shutdown hook: after it returns no
// session exists and no dev-trace store is served.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	var live []*session
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.Unlock()

	for _, s := range live {
		s.mu.Lock()
		if s.info.State == StateStarting || s.info.State == StateRunning {
			s.stopKind = "closed"
			s.mu.Unlock()
			s.cancel()
		} else {
			s.mu.Unlock()
		}
	}
	deadline := time.After(closeSettleTimeout)
	for _, s := range live {
		select {
		case <-s.done:
		case <-deadline:
			m.log.Warn("manager close did not wait out the run", "session_id", s.id)
		}
	}
}
