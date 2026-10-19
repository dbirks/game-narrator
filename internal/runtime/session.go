package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/clips"
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
	ID              string `json:"session_id"`
	ProfileID       string `json:"profile_id"`
	ProfileRevision string `json:"profile_revision"`
	Mode            string `json:"mode"`
	SourceKind      string `json:"frame_source"`
	// ClipID is the managed-store clip this run replays (ADR-0073): the id a
	// session was started with, so the UI's source line can say which clip is
	// running rather than only which kind. Empty for dir- and device-sourced
	// sessions.
	ClipID      string         `json:"clip_id,omitempty"`
	DevTrace    bool           `json:"dev_trace"`
	State       string         `json:"state"`
	Reason      string         `json:"reason,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	EndedAt     time.Time      `json:"ended_at,omitempty"`
	EventCursor int64          `json:"event_cursor"`
	Stats       pipeline.Stats `json:"stats"`
	// Audio is the session's speech half — the sink, the live controls, the
	// speech counters and the current utterance — present when the process
	// has a voice workflow wired and the session's sink built.
	Audio *AudioInfo `json:"audio,omitempty"`
	// Decision is the session's Clef adjudication view (docs/spec/05,
	// ADR-0050): whether the decision role is engaged for this session,
	// whether its last engagement degraded it (and why), and the call
	// counts that explain why a line was committed on the local evidence
	// alone. nil when the session runs local (DecisionOptions nil, or the
	// role not configured / not System One).
	Decision *DecisionStatus `json:"decision,omitempty"`
	// Source is the capture source's own self-report (the API view of
	// capture.Stats): whether the source is delivering at all, and if not,
	// why. The pipeline's downstream counters alone cannot answer it — a
	// session whose capture child died at start reads "running" with zero
	// frames delivered and no fault named, and the operator action
	// docs/spec/13 requires is unfindable (the 2026-10-19 live-play run:
	// the dolphin x11grab source delivered nothing for 25 minutes and
	// nothing in the snapshot said why).
	Source SourceInfo `json:"source"`
}

// SourceInfo is the session's capture source self-report: the source's own
// ID and lifecycle state (docs/spec/10 "verify last frame freshness"), the
// delivered count the source itself keeps (independent of the pipeline's
// downstream counter), the supervised-restart count that separates "the card
// was unplugged once" from "capture is flapping", and the last failure text
// (docs/spec/13's actionable operator action).
type SourceInfo struct {
	ID          string    `json:"id"`
	State       string    `json:"state"`
	Delivered   uint64    `json:"delivered"`
	Restarts    uint64    `json:"restarts"`
	FPS         float64   `json:"fps"`
	LastFrameAt time.Time `json:"last_frame_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
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
	speech   *speechWiring   // nil when the process has no speech pipeline
	decision *decisionWiring // nil when the session runs local (no System One decision role)
	stopKind string          // "", "requested", "closed" — who cancelled the run
}

func (s *session) Snapshot() Info {
	// The pipeline's live counters are read outside the session lock, the same
	// way the source's self-report is: Stats() is concurrency-safe (it takes
	// the pipeline's own lock and returns a consistent copy), and the runner is
	// set at construction and never reassigned. This matters because the stop
	// path is NOT the only writer of s.info.Stats anymore — a running session's
	// snapshot must carry the LIVE frames/ocr/commits counters, not the zeros
	// the struct was initialised with. Before this, the Play page's Frames/OCR/
	// Commits (and any watchdog reading them) showed 0 for the whole run and
	// only jumped to the final totals at stop — the 2026-10-19 live-play
	// "0 frames delivered" page snapshot was this staleness, not a dead source.
	var live pipeline.Stats
	if s.runner != nil {
		live = s.runner.Stats()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.info
	info.Stats = live
	if s.speech != nil {
		info.Audio = s.speech.audioInfo()
	}
	if s.decision != nil {
		d := s.decision.status()
		info.Decision = &d
	}
	// The source's self-report is read outside the session lock on purpose:
	// every capture.Source implements Stats concurrency-safe (the interface
	// contract), and the source's own lock is where its state moves.
	st := s.src.Stats()
	info.Source = SourceInfo{
		ID:          s.src.ID(),
		State:       string(st.State),
		Delivered:   st.Delivered,
		Restarts:    st.Restarts,
		FPS:         st.FPS,
		LastFrameAt: st.LastFrameAt,
		LastError:   st.LastError,
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
	// A clip id outranks dir: the id is the product path (ADR-0073), and a
	// request that names both keeps the id — the directory it would resolve
	// to is exactly <clips store>/<id>, which is what the id already says.
	dir := req.FrameSource.Dir
	if kind == SourceKindReplay && req.FrameSource.ClipID != "" {
		var err2 error
		dir, err2 = m.clipSourceDir(req.FrameSource.ClipID)
		if err2 != nil {
			return Info{}, err2
		}
	}
	src, err := buildFrameSource(kind, dir, m.effectiveReplayRoots(), m.opts.ReplaySpeed, m.opts.DevDolphin, m.opts.ManualSource, nil)
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
	cfg := m.pipelineConfig(req, published, id, content.Regions, idx, kind)

	// The Clef adjudication (docs/spec/05, ADR-0050): when the process has a
	// configured System One decision role, the session's commitment policy
	// runs with the adjudication on — the pipeline's Decider is this
	// session's wiring, and the reducer's policy asks it for every locally
	// ambiguous ranking. The texts the candidate options are rendered with
	// come from the published content (the canonical original text, falling
	// back to the speech text); the speaker options are the profile's
	// distinct declared speakers. A role that is not configured keeps the
	// pipeline local (Decider nil) — the documented degraded shape, not an
	// error.
	if m.opts.Decision != nil {
		texts := make(map[string]string, len(content.Messages))
		speakerSeen := make(map[string]bool, len(content.Messages))
		var speakers []pipeline.SpeakerOption
		for _, msg := range content.Messages {
			t := msg.OriginalText
			if t == "" {
				t = msg.NormalizedText
			}
			if t == "" {
				t = msg.SpeechText
			}
			if t != "" {
				texts[msg.ID] = t
			}
			if msg.SpeakerID != "" && !speakerSeen[msg.SpeakerID] {
				speakerSeen[msg.SpeakerID] = true
				speakers = append(speakers, pipeline.SpeakerOption{ID: msg.SpeakerID})
			}
		}
		cfg.Reducer.Adjudicate = true
		cfg.Reducer.AdjudicateMargin = m.opts.Decision.Margin
		cfg.Speakers = speakers
		if w := m.newDecisionWiring(id, texts); w != nil {
			cfg.Decider = w
		}
	}

	runner, err := pipeline.New(cfg)
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
		ClipID:          req.FrameSource.ClipID,
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
			"frame_source":     frameSourceEvent(kind, req.FrameSource.ClipID),
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

	// The decision wiring lives on the session (for the Snapshot's Decision
	// view) once the pipeline has been handed it: cfg.Decider was built
	// before the runner above.
	if w, ok := cfg.Decider.(*decisionWiring); ok && w != nil {
		sess.decision = w
	}

	go sess.run(runCtx, obs, pub)

	snap := sess.Snapshot()
	return snap, nil
}

// checkRequest validates the request before anything is touched, so a bad
// body costs nothing on the box.
// effectiveReplayRoots is the frame source's allowlist (ADR-0073): the
// configured roots plus, when this process has a managed clip store, the
// store itself. An uploaded clip is product content on this appliance — the
// whole point of the clip route is that it stops needing an operator edit of
// the service environment — so it is a root by construction, not by config.
// Duplicates are dropped; the allowlist test is a prefix walk and repeats
// change nothing but read as if they meant something.
func (m *Manager) effectiveReplayRoots() []string {
	roots := make([]string, 0, len(m.opts.ReplayRoots)+1)
	for _, r := range m.opts.ReplayRoots {
		if r == "" {
			continue
		}
		dup := false
		for _, kept := range roots {
			if kept == r {
				dup = true
				break
			}
		}
		if !dup {
			roots = append(roots, r)
		}
	}
	if d := m.opts.ClipsDir; d != "" {
		dup := false
		for _, kept := range roots {
			if kept == d {
				dup = true
				break
			}
		}
		if !dup {
			roots = append(roots, d)
		}
	}
	return roots
}

// clipSourceDir resolves a clip id against the managed store (ADR-0073). The
// id contract (internal/clips) is the security boundary — no separators means
// no traversal — and the resolved directory still has to pass the replay-root
// allowlist in buildFrameSource, so the id and the root check are the two
// independent layers docs/spec/14 "Filesystem" asks for. The directory's
// existence is the allowlist test's job: a clip deleted between the list and
// the start answers the documented "source missing", not a path error.
func (m *Manager) clipSourceDir(clipID string) (string, error) {
	if m.opts.ClipsDir == "" {
		return "", ErrClipsUnavailable
	}
	if !clips.ValidID(clipID) {
		return "", fmt.Errorf("%w: %q", ErrInvalidClipID, clipID)
	}
	return filepath.Join(m.opts.ClipsDir, clipID), nil
}

// frameSourceEvent is the session.started event's frame_source object. The
// kind and the clip id (a slug, never a path) are evidence the developer UI
// correlates the run to its input with; a dir-shaped source names no path —
// the event stream is developer-readable and a host path is not event data
// (docs/spec/14 "Filesystem").
func frameSourceEvent(kind, clipID string) map[string]any {
	out := map[string]any{"kind": kind}
	if clipID != "" {
		out["clip_id"] = clipID
	}
	return out
}

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
	// Clip ids select a managed-store clip rather than a path (ADR-0073);
	// the id is validated here, where the request's shape is the only input.
	if req.FrameSource.ClipID != "" {
		if req.FrameSource.Kind != SourceKindReplay {
			return fmt.Errorf("%w: clip_id selects a replay clip and is not used with frame source kind %q", ErrInvalidClipID, req.FrameSource.Kind)
		}
		if !clips.ValidID(req.FrameSource.ClipID) {
			return fmt.Errorf("%w: %q (a clip id is a lowercase slug of at most %d characters — letters, digits, dot, dash, underscore — and never a path)",
				ErrInvalidClipID, req.FrameSource.ClipID, clips.MaxIDLen)
		}
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
