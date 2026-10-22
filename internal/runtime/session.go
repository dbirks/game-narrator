package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/clips"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/recognition"
	ses "github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision"
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
	// Paused is the operator's pause half (docs/spec/08 "…/pause, …/resume"):
	// true while the session has stopped committing and speaking, false
	// otherwise. It moves on the pause/resume routes and the snapshot carries
	// it, so the UI's pause button shows the state the server holds, not the
	// click the operator made.
	Paused bool `json:"paused"`
	// NarratedClasses is the operator's narrated-class setting (docs/spec/09
	// "/play"): the text classes the commitment policy narrates, normalized —
	// dialogue first, always present. The default (dialogue alone) is what a
	// session started without a settings call carries.
	NarratedClasses []games.TextKind `json:"narrated_classes"`
	// CatchUp is the operator's catch-up narration policy (issue #25,
	// ADR-0122): the speech side's handling of the lines committed while an
	// utterance is playing. The default (catch_up) batches and merges them
	// per speaker; latest_only cancels the older audio and speaks the newest
	// line at once. The snapshot carries the applied word so the UI's toggle
	// reads the server's state, not the form's last click.
	CatchUp string `json:"catch_up"`
	// UnmatchedReadThrough is the operator's unmatched read-through policy
	// (ADR-0136): when ON (the default), a box the matcher cannot name is
	// spoken from its OCR text (cleaned, bounded, the speaker's voice when
	// the attribution is known) instead of staying silent; when OFF the
	// abstained box keeps the historic silence. The snapshot carries the
	// applied state so the Play toggle reads the server's value.
	UnmatchedReadThrough bool   `json:"unmatched_read_through"`
	SourceKind           string `json:"frame_source"`
	// ClipID is the managed-store clip this run replays (ADR-0073): the id a
	// session was started with, so the UI's source line can say which clip is
	// running rather than only which kind. Empty for dir- and device-sourced
	// sessions.
	ClipID string `json:"clip_id,omitempty"`
	// CaptureDevice is the UVC HDMI capture device's stable id this run uses
	// (kind v4l2, ADR-0078): the device the resolution settled on — the
	// requested id, the persisted selection, or the first usable device — so
	// a fallback is visible in the snapshot, not guessed from the request.
	// Empty for every other source kind.
	CaptureDevice string `json:"capture_device,omitempty"`
	// CaptureMode is the mode the session asked of that device (e.g. "640x480
	// MJPEG @ 10 fps"); empty for every other source kind.
	CaptureMode string `json:"capture_mode,omitempty"`
	// CaptureModeSource names the mode's provenance: "saved" (the operator's
	// pin) or "policy" (the ADR-0076 selection); empty for every other kind.
	CaptureModeSource string         `json:"capture_mode_source,omitempty"`
	DevTrace          bool           `json:"dev_trace"`
	State             string         `json:"state"`
	Reason            string         `json:"reason,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	EndedAt           time.Time      `json:"ended_at,omitempty"`
	EventCursor       int64          `json:"event_cursor"`
	Stats             pipeline.Stats `json:"stats"`
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
	// CaptureFPSTarget is the fps of the capture mode this session asked of
	// its device (the target half of the status bar's "capture 30fps" line,
	// against the measured Source.FPS). 0 when the source kind has no named
	// rate (replay, dolphin, manual, test pattern's rate is carried here
	// when the source is the test pattern).
	CaptureFPSTarget float64 `json:"capture_fps_target,omitempty"`
	// CommitToVoice is the commit→voice latency window the status bar reads
	// (status.go): per committed line, the wall clock of the commit to the
	// wall clock of the line's audio starting on the sink, kept for the last
	// N lines. 0/0 until a line completes the window.
	CommitToVoice CommitToVoiceSummary `json:"commit_to_voice"`
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
	m      *Manager
	id     string
	cancel context.CancelFunc
	done   chan struct{}
	runner *pipeline.Runner
	src    capture.Source
	// controls is the live-control channel (pause/resume/settings) the manager
	// sends on and the pipeline's single writer drains: the runtime owns the
	// channel because it owns the policy, and the pipeline owns the apply
	// because it owns the writer. Buffered (16) so a burst of operator clicks
	// never blocks the caller; the depth is a bound, not a queue to fill — a
	// session that let 16 controls pile unapplied has a dead writer, and the
	// next control's send failure is how that surfaces.
	controls chan ses.Msg
	// ctrlClosed marks the control channel closed: run() closes it the moment
	// the writer is done, so a control that lands after the run has ended is
	// refused (sendControl answers false) instead of buffered into a dead
	// session and answered as applied — a phantom success the operator's UI
	// would render as "paused" on a session that is already over. Guarded by mu.
	ctrlClosed bool
	mu         sync.Mutex
	info       Info
	rec        *devTraceRecorder
	speech     *speechWiring   // nil when the process has no speech pipeline
	decision   *decisionWiring // nil when the session runs local (no System One decision role)
	// voiceLat samples the line's commit→voice interval (status.go) for the
	// status bar; the commit watcher and the speech scheduler's publisher
	// are its two writers. Always non-nil for a started session.
	voiceLat *frameVoiceSampler
	stopKind string // "", "requested", "closed" — who cancelled the run
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
	if s.voiceLat != nil {
		if sv := s.voiceLat.snapshot(); sv.Count > 0 {
			info.CommitToVoice = CommitToVoiceSummary{Count: sv.Count, P50MS: sv.P50.Milliseconds()}
		}
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
	// Library lifecycle (ADR-0107, gap G14): a soft-deleted revision is 404
	// for Play — the mark is the delete, and starting a session on it would
	// resurrect what the operator removed (the row stays readable by the
	// store's own repositories until the purge removes it). A disabled title
	// family is a 409: the game exists and its profiles stay editable, but a
	// session must not start on a game the operator took off Play.
	if prof.DeletedAt != "" {
		return Info{}, fmt.Errorf("%w: profile %s was deleted (purge_after %s)", ErrProfileDeleted, req.ProfileID, prof.PurgeAfter)
	}
	edition, err := m.opts.Store.GetEdition(ctx, prof.EditionID)
	if err != nil {
		return Info{}, err
	}
	game, err := m.opts.Store.GetGame(ctx, edition.GameID)
	if err != nil {
		return Info{}, err
	}
	if game.Disabled {
		return Info{}, fmt.Errorf("%w: game %s is disabled", ErrGameDisabled, game.Slug)
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
	// Speaker-link resolution (ADR-0070's link, ADR-0101): a line whose speaker
	// character is identified by another live character — the placeholder's name
	// (the INF1 key the speaker-link folded into the target's aliases) — resolves
	// to the target, so the line speaks with the target's voice. It is computed
	// once, here, so the speech pipeline and the decision wiring (whose speaker
	// options are the same distinct ids) agree on what a speaker is.
	resolveSpeaker := speakerLinkResolver(content.Characters)
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
	srcB, err := buildFrameSource(ctx, kind, dir, m.effectiveReplayRoots(), m.opts.ReplaySpeed, m.opts.DevDolphin, m.opts.ManualSource, m.opts.Capture, nil)
	if err != nil {
		return Info{}, err
	}
	if kind == "" {
		// The request named no frame source and the resolution settled one
		// (ADR-0119, issue #28): the resolved kind is what the snapshot, the
		// session.started event and the status bar name — v4l2 with the
		// device it settled on, or the developer-mode test pattern — never
		// the absence the request carried.
		kind = srcB.kind
	}
	src := srcB.src

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
	// The live-control channel (pause/resume/settings, docs/spec/08): the
	// runtime owns it (it owns the policy) and the pipeline's single writer
	// drains it (it owns the apply). Buffered so a burst of operator clicks
	// never blocks the caller that pressed them.
	controls := make(chan ses.Msg, 16)
	cfg := m.pipelineConfig(req, published, id, content.Regions, idx, kind, mode, controls)
	cfg.RevealDetector = m.revealDetector(content.Sources)

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
			if msg.SpeakerID != "" {
				// The option is the resolved speaker: a placeholder the curator
				// linked is offered under the character it was identified as.
				if sp := resolveSpeaker(msg.SpeakerID); sp != "" && !speakerSeen[sp] {
					speakerSeen[sp] = true
					speakers = append(speakers, pipeline.SpeakerOption{ID: sp})
				}
			}
		}
		cfg.Reducer.Adjudicate = true
		cfg.Reducer.AdjudicateMargin = m.opts.Decision.Margin
		cfg.Speakers = speakers
		if w := m.newDecisionWiring(id, texts); w != nil {
			cfg.Decider = w
		}
	}

	// The speculative TTS prewarm seam (docs/spec/07, the overlap of the
	// Clef round trip with the synthesis): the pipeline's OnAdjudicating hook
	// fires on the rank worker just before the adjudication goes out, when the
	// top local candidate is already known. It prewarms that line's synthesis
	// in the background so the commit resolves from the singleflighted cache.
	// The speech wiring is not built until after the runner below, so the hook
	// reads it through an atomic pointer the wiring stores once it exists; the
	// run goroutine starts only after the store, so the hook never sees a
	// half-built wiring. nil (no speech in this process) is a no-op.
	specSpeech := new(atomic.Pointer[speechWiring])
	cfg.OnAdjudicating = func(messageID string) {
		if sw := specSpeech.Load(); sw != nil {
			sw.speculativePrewarm(messageID)
		}
	}

	runner, err := pipeline.New(cfg)
	if err != nil {
		cancel()
		_ = src.Close()
		return Info{}, fmt.Errorf("runtime: pipeline setup: %w", err)
	}

	sess := &session{
		m:        m,
		id:       id,
		cancel:   cancel,
		done:     make(chan struct{}),
		runner:   runner,
		src:      src,
		controls: controls,
		// The sampler exists even when the speech wiring is absent: a
		// session that never speaks reports 0/0 from it, which is the same
		// honest shape the snapshot carries from an unwired sink.
		voiceLat: newFrameVoiceSampler(),
	}
	sess.info = Info{
		ID:              id,
		ProfileID:       published.ID,
		ProfileRevision: strconv.Itoa(published.RevisionNum),
		Mode:            mode,
		// The narrated classes start at the documented default (dialogue
		// alone); a mid-session settings call is what moves them, and the
		// snapshot carries the current set so the UI's toggles read the
		// server's state, not the form's last click. CatchUp starts at the
		// documented default (catch_up, issue #25): the same snapshot
		// discipline — the settings call is what moves it. UnmatchedReadThrough
		// starts at its documented default (ON, ADR-0136): an unmatched box
		// speaks from its OCR text unless the operator turns the read-through
		// off — the user's own words: "just read it, even if we don't have the
		// dialogue correct".
		NarratedClasses:      []games.TextKind{games.TextDialogue},
		CatchUp:              ses.CatchUpPolicyCatchUp,
		UnmatchedReadThrough: true,
		SourceKind:           kind,
		ClipID:               req.FrameSource.ClipID,
		DevTrace:             req.DevTrace,
		CaptureDevice:        srcB.device,
		CaptureMode:          srcB.mode,
		CaptureModeSource:    srcB.modeSource,
		CaptureFPSTarget:     srcB.modeFPS,
		State:                StateStarting,
		CreatedAt:            time.Now().UTC(),
	}
	sess.rec = rec

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()
	// The session gauge of docs/spec/13 "Metrics": the count of sessions the
	// manager is running right now. Booked at registration (the session is
	// live from here) and dropped in unregister, so the gauge reads the same
	// number the manager's own map holds. nil-safe: an off process books
	// nothing.
	m.metrics.SessionStarted()

	// session.started is the cursor clients poll from: everything the session
	// publishes afterwards has a higher seq (docs/spec/08 SSE semantics).
	started, err := m.opts.Hub.Publish(ctx, events.Envelope{
		Name:      EventSessionStarted,
		SessionID: id,
		Data: mustJSON(map[string]any{
			"profile_id":       published.ID,
			"profile_revision": published.RevisionNum,
			"mode":             mode,
			"frame_source":     frameSourceEvent(kind, req.FrameSource.ClipID, srcB.device, srcB.mode, srcB.modeSource),
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

	// The no-signal wrapper (internal/capture/signal, docs/spec/12): on the
	// live v4l2 input it holds frames back from the pipeline while the card
	// reports no HDMI signal and publishes the capture.signal transitions;
	// the fixture sources pass through unchanged.
	src = m.newSignalSource(src, id)

	if rec != nil {
		// The dev-trace evidence must describe the same crop basis the pipeline
		// reads: the recorder crops its samples through the session's own crop
		// options and stamps the active window it sampled on the text.observed
		// events (ADR-0082 amendment 1). It is set before the source starts, so
		// no sample can race the assignment.
		rec.SetCropOptions(cfg.Crop)
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
		speakers, speakerAliasByID, prewarmTexts, prewarmPages := profileSpeechMaps(content, resolveSpeaker)
		if w := m.newSpeechWiring(id, speakers, published.NarratorFallback, runCtx, runner, idx, prewarmTexts, prewarmPages, sess.voiceLat); w != nil {
			w.speakerAliasByID = speakerAliasByID
			sess.speech = w
			// The OnAdjudicating hook above reads the wiring through this
			// pointer; storing it here (before the run starts) is what lets a
			// Clef in flight find the prewarm to trigger.
			specSpeech.Store(w)
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
// (docs/spec/14 "Filesystem"). A v4l2 run additionally names the capture
// device's stable id and the mode it asked (ADR-0078): the correlation the
// dongle's physical identity and the run's pixels need.
func frameSourceEvent(kind, clipID, capDevice, capMode, capModeSource string) map[string]any {
	out := map[string]any{"kind": kind}
	if clipID != "" {
		out["clip_id"] = clipID
	}
	if capDevice != "" {
		out["device"] = capDevice
		out["mode"] = capMode
		out["mode_source"] = capModeSource
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
	// The default frame source (ADR-0119, issue #28): a request may name no
	// kind at all — buildFrameSource then resolves the usable capture device
	// (or the developer-mode test pattern). A dir or a clip id without a kind
	// is ambiguous (a replay dir and a device id both live in frame_source.dir),
	// so the answer is the 400 that names the rule, never a guess.
	if req.FrameSource.Kind == "" && (req.FrameSource.Dir != "" || req.FrameSource.ClipID != "") {
		return fmt.Errorf("%w: frame_source.dir or frame_source.clip_id names an input but the kind that reads it is missing", ErrIncompleteFrameSource)
	}
	// A clip id with a named kind is a replay selection (ADR-0073); the id is
	// validated here, where the request's shape is the only input.
	if req.FrameSource.ClipID != "" && req.FrameSource.Kind != "" {
		if req.FrameSource.Kind != SourceKindReplay {
			return fmt.Errorf("%w: clip_id selects a replay clip and is not used with frame source kind %q", ErrInvalidClipID, req.FrameSource.Kind)
		}
		if !clips.ValidID(req.FrameSource.ClipID) {
			return fmt.Errorf("%w: %q (a clip id is a lowercase slug of at most %d characters — letters, digits, dot, dash, underscore — and never a path)",
				ErrInvalidClipID, req.FrameSource.ClipID, clips.MaxIDLen)
		}
	}
	switch req.FrameSource.Kind {
	case "", SourceKindReplay, SourceKindTestPattern, SourceKindV4L2, SourceKindDolphin, SourceKindManual:
	default:
		return fmt.Errorf("%w: %q", ErrUnknownSourceKind, req.FrameSource.Kind)
	}
	return nil
}

// pipelineConfig assembles the pipeline from the published content and the
// session's settings. Replay runs in ModeReplay (lossless, the recording it
// claims to be); every other source runs live.
// pipelineConfig builds the pipeline's settings for one run. decisionMode is
// the normalized decision mode (the Mode* constants): it seeds the two halves
// of the commitment policy that carry it — the session reducer's
// PredictiveMode and the tracker's — so a predictive session commits the way
// it was started, and the mid-session settings control is what moves them
// after. controls is the live-control channel (nil only if the caller does
// not wire one; the runtime always does).
func (m *Manager) pipelineConfig(req StartRequest, published games.Profile, sessionID string, regions []games.Region, idx *matcher.Index, kind, decisionMode string, controls chan ses.Msg) pipeline.Config {
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
	predictive := decisionMode == ModePredictive
	// The session's engine: the configured engine (a test seam) as given, else
	// the process-shared warm pool (issue #13, ADR-0100) — the same engine all
	// of the pipeline's regions share, so the pool's one-read-at-a-time
	// serialisation (ADR-0100 decision 2) is the session's OCR pacing.
	engine := m.ocrEngine()
	// One active-picture tracker per session (ADR-0082): the region crops map
	// through the tracked window once it has settled and the whole frame until
	// it has; a borderless source (Dolphin) never settles, so its crops keep
	// mapping the whole frame. The base recipe (letterbox detection plus the
	// ADR-0115 §2 inset that cuts the textbox's decorative border and speaker
	// icon out of the OCR input) is DefaultCropOptions — one source of the crop
	// recipe the pipeline and the test harness's truth script both cut with —
	// and the tracker is the session-state half added here.
	baseCrop := DefaultCropOptions()
	baseCrop.Tracker = vision.NewTracker(vision.TrackerConfig{})
	cfg := pipeline.Config{
		Mode:            mode,
		SessionID:       sessionID,
		ProfileRevision: strconv.Itoa(published.RevisionNum),
		Regions:         regions,
		OCR:             engine,
		Index:           idx,
		Reducer:         ses.Config{PredictiveMode: predictive, CatchUp: ses.CatchUpPolicyCatchUp},
		Tracker:         m.trackerConfig(kind, predictive),
		Controls:        controls,
		Preprocess:      vision.DefaultSteps(),
		Crop:            baseCrop,
		// The fallback text-kind hint for regions whose on-screen role names no
		// class of its own: the dialogue box is the documented default. A region
		// whose role does name a class (sign, item, menu, tutorial) ranks its
		// readings with that class — the pipeline derives the per-region hint
		// from games.RegionType.TextKindHint (docs/spec/04 "Visual profiles").
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
		// The confidence threshold precedence (ADR-0115 §4): a region's
		// ocr_settings value wins (the profile's call about its own font),
		// then the process env default newOCRFallbackWiring merged into the
		// policy, then a per-source default — the analog HDMI capture (v4l2)
		// takes the measured floor of its correct crop reads, so a correct read
		// at 0.85+ no longer spends a VLM call, while a clean emulator crop
		// keeps the ocr package's 0.6 bar.
		if thr := fallbackConfThresholdIn(regions); thr > 0 {
			policy.ConfidenceThreshold = thr
		} else if policy.ConfidenceThreshold <= 0 && kind == SourceKindV4L2 {
			policy.ConfidenceThreshold = v4l2FallbackConfThreshold
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
	// The telemetry surface (docs/spec/13 "Metrics", internal/telemetry): the
	// pipeline's stages book into it when the process wired a catalog. The
	// label pair is the bounded game/revision the spec names — the game is
	// the profile's edition's game, resolved once at session start (a store
	// read, not the hot path), and an empty edition id or an error degrades
	// to the "none" label value rather than erroring the session. m.metrics
	// nil (the default) leaves cfg.Metrics nil and the run unwired: every
	// hook is nil-safe, so an off process behaves exactly as before.
	cfg.Metrics = m.metrics
	if m.metrics != nil && m.opts.Store != nil && published.EditionID != "" {
		if ed, err := m.opts.Store.GetEdition(context.Background(), published.EditionID); err == nil {
			cfg.MetricsGame = ed.GameID
		}
	}
	return cfg
}

// cropInsetPx is the per-side crop inset the runtime sets on its textbox
// regions (ADR-0115 §2): the textbox's decorative border and speaker icon sit
// inside the profile-calibrated rect on a captured feed, and the OCR engine
// reads them as noise glyphs ("4 ", "$o?") that poison the match. Two pixels is
// the border width measured on the board feed. It is read through
// DefaultCropOptions, the one source of the crop recipe: a drift between the
// pipeline's crop and the test harness's truth script is a miss on every crop
// hash (the recipe-drift failure helpers_test.go's truthScript comment names),
// so the number and the recipe it sits in live in one place and both sides read
// it.
const cropInsetPx = 2

// DefaultCropOptions is the runtime's base crop recipe for a session's textbox
// regions: letterbox detection (a 4:3 picture inside a 16:9 capture maps its
// rect onto the picture, not the bars) and the per-side inset (ADR-0115 §2)
// that cuts the textbox's decorative border out of the OCR input. The
// per-session tracker (the active-picture window, ADR-0082) is session state
// the caller adds; it is not part of the recipe. This is the single source of
// the crop recipe: the pipeline's crop and the test harness's truth script
// must cut with the same options or the crop hashes drift and every read
// misses.
func DefaultCropOptions() vision.CropOptions {
	return vision.CropOptions{
		Letterbox: vision.LetterboxDetect,
		Inset:     cropInsetPx,
	}
}

// v4l2FallbackConfThreshold is the per-source OCR fallback confidence default
// for the analog HDMI capture (ADR-0115 §4): the measured floor of correct crop
// reads on the board feed (Tesseract 0.82–0.95 on the box). A primary read at
// or above it is resolved locally and the VLM is not called; below it the read
// is genuinely uncertain and the secondary is worth its call. Other source
// kinds (the clean emulator crop, a replay clip) keep the ocr package's 0.6
// default, which is the right bar for pixels that do not carry analog noise.
const v4l2FallbackConfThreshold = 0.85

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

// fallbackConfThresholdIn reports the confidence threshold a region names in
// its ocr_settings — the profile-level override ADR-0115 §4 stores as the
// {"fallback_confidence_threshold": <float>} member of the regions.ocr_settings
// JSON object, the same home as ADR-0040's difficult_font. It wins over the
// process env default and the per-source default. The first region that names a
// value in (0,1] is returned; malformed ocr_settings reads as "no value" (0),
// the same discipline difficultFontIn applies, so the recognition path must not
// fail a session over a store-shape surprise it did not cause.
func fallbackConfThresholdIn(regions []games.Region) float64 {
	for _, r := range regions {
		if len(r.OCRSettings) == 0 {
			continue
		}
		var s struct {
			FallbackConfidenceThreshold float64 `json:"fallback_confidence_threshold"`
		}
		if err := json.Unmarshal(r.OCRSettings, &s); err != nil {
			continue
		}
		if s.FallbackConfidenceThreshold > 0 && s.FallbackConfidenceThreshold <= 1 {
			return s.FallbackConfidenceThreshold
		}
	}
	return 0
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
func (m *Manager) trackerConfig(kind string, predictive bool) recognition.Config {
	if m.opts.Tracker != (recognition.Config{}) {
		cfg := m.opts.Tracker
		// An explicitly supplied tracker config carries its own thresholds, but
		// the decision mode is the session's, not the caller's: the test seam
		// exists to override the cadence knobs, and a predictive session must
		// still reveal its prefixes (the text machine's half of the mode) even
		// when the tracker's rest came from Options.
		cfg.PredictiveMode = predictive
		return cfg
	}
	cfg := recognition.DefaultConfig()
	cfg.PredictiveMode = predictive
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
	// The startup log reads info fields that Manager.SetSettings rewrites
	// under the session lock (mode, and the settings block around it): a
	// settings call that lands while this log is formatting its fields would
	// race the unlocked reads (issue #40). Read the log's fields under the
	// lock, so the line can never describe a half-updated settings struct.
	s.mu.Lock()
	profileID := s.info.ProfileID
	revision := s.info.ProfileRevision
	mode := s.info.Mode
	devTrace := s.info.DevTrace
	s.mu.Unlock()
	s.m.log.Info("session running",
		"session_id", s.id, "profile_id", profileID,
		"revision", revision, "mode", mode,
		"source", src.ID(), "dev_trace", devTrace)

	err := s.runner.Run(runCtx, src, pub)

	// The writer is done, so the live-control channel closes with it: a pause,
	// resume or settings that lands after the run ended is a refusal (the
	// 409 the operator deserves), not a message buffered into a session that
	// will never read it and reported as applied. Safe to close here — Run
	// returns only after the writer has exited, so nothing reads it anymore
	// (the writer treats a closed channel as "the operator stopped steering").
	s.mu.Lock()
	if !s.ctrlClosed {
		close(s.controls)
		s.ctrlClosed = true
	}
	s.mu.Unlock()

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

// Active reports the session that is starting, running or stopping, with
// its fresh snapshot — the "active session" the status bar and watchdogs
// read. The manager's one-active-session invariant (docs/spec/01) means at
// most one answer exists; the scan is over the bounded finished-sessions
// map the developer inspector keeps, like ActiveCaptureDevice. A finished
// session is not active: its final counters are its final counters, and a
// status bar that narrated them would describe a run that is over.
func (m *Manager) Active() (Info, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		snap := s.Snapshot()
		switch snap.State {
		case StateStarting, StateRunning, StateStopping:
			return snap, true
		}
	}
	return Info{}, false
}

// ActiveCaptureDevice reports the v4l2 session that holds a capture device
// (ADR-0078): the device's stable id and the session that is running it, for
// the capture preview route's busy check. The UVC bus is single-consumer — a
// preview and a session on the same device cannot both open it — so the
// API answers a same-device preview 503 capture-busy (with the session id
// named) instead of a raw FFmpeg "device or resource busy". A session in a
// terminal state releases the device, so it is not reported. The one-active-
// session invariant means at most one such session exists; the scan is over
// the manager's map, which holds the bounded set of finished sessions the
// developer inspector keeps.
func (m *Manager) ActiveCaptureDevice() (deviceID, sessionID string, active bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		snap := s.Snapshot()
		switch snap.State {
		case StateStarting, StateRunning, StateStopping:
		default:
			continue
		}
		if snap.SourceKind == SourceKindV4L2 && snap.CaptureDevice != "" {
			return snap.CaptureDevice, id, true
		}
	}
	return "", "", false
}

// CapturePictureView is the active-picture window the signal route reports
// (ADR-0110): the tracker's accepted window once it has settled one, the
// whole frame plus Settled false until then (the ADR-0082 fresh-start
// behaviour — a session that begins on a black frame invents no window).
// X/Y are the window's origin in frame coordinates; W/H its size.
type CapturePictureView struct {
	X       int  `json:"x"`
	Y       int  `json:"y"`
	W       int  `json:"w"`
	H       int  `json:"h"`
	Settled bool `json:"settled"`
}

// CaptureCropView is one crop rectangle in frame coordinates (the last
// dialogue-region cut the signal route draws; ADR-0110).
type CaptureCropView struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// CaptureLive is the active capture session's live view for the UX-11 surface
// (ADR-0110): the session id and the negotiated format and target fps, the
// source's self-report (the measured fps and the last-frame time), the
// active-picture tracker's window and settle state, the last dialogue-region
// crop and the newest frame still in memory (the preview serves it without a
// device grab). ok is false when no v4l2 capture session is starting, running
// or stopping: a finished session's final counters are not a live signal, and
// a non-capture session (replay, dolphin, manual) holds no device the signal
// question is about.
//
// The reads are concurrency-safe by construction: the session scan runs under
// the manager's lock (the same walk ActiveCaptureDevice takes), the snapshot
// and the source's self-report take their own locks, and the tracker's Box and
// the pipeline's live-view reads are mutex-guarded for exactly this
// cross-goroutine reader (ADR-0082 amendment 1, ADR-0110 §3).
func (m *Manager) CaptureLive() (CaptureLive, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out CaptureLive
	for _, s := range m.sessions {
		snap := s.Snapshot()
		switch snap.State {
		case StateStarting, StateRunning, StateStopping:
		default:
			continue
		}
		if snap.SourceKind != SourceKindV4L2 || snap.CaptureDevice == "" {
			continue
		}
		out = CaptureLive{
			SessionID: s.id,
			Device:    snap.CaptureDevice,
			Format:    snap.CaptureMode,
			FPSTarget: snap.CaptureFPSTarget,
			Source:    snap.Source,
		}
		if s.runner != nil {
			cfg := s.runner.Config()
			var f capture.Frame
			haveFrame := false
			if f, haveFrame = s.runner.LatestFrame(); haveFrame && f.Image == nil {
				haveFrame = false
			}
			w, h := f.Bounds()
			// The window: the tracker's accepted one once settled, the whole
			// frame plus Settled false until then (the ADR-0082 fresh-start
			// behaviour — a session that begins on a black frame invents no
			// window). A nil tracker (a wiring the runtime does not produce
			// for v4l2 sessions) degrades to the same whole-frame answer.
			if tr := cfg.Crop.Tracker; tr != nil {
				if b, ok := tr.Box(); ok {
					out.Picture = CapturePictureView{
						X: b.Min.X, Y: b.Min.Y, W: b.Dx(), H: b.Dy(),
						Settled: true,
					}
				} else {
					out.Picture = CapturePictureView{W: w, H: h}
				}
			} else {
				out.Picture = CapturePictureView{W: w, H: h}
			}
			if rect, ok := s.runner.DialogueCrop(); ok {
				out.Dialogue = &CaptureCropView{X: rect.Min.X, Y: rect.Min.Y, W: rect.Dx(), H: rect.Dy()}
			}
			if haveFrame {
				out.Latest = &f
			}
		}
		return out, true
	}
	return CaptureLive{}, false
}

// CaptureLive is the view CaptureLive returns: the pieces the API's
// GET /api/v1/capture/signal and the in-memory preview read of a running
// v4l2 session (ADR-0110).
type CaptureLive struct {
	SessionID string
	// Device is the capture device's stable id the session holds.
	Device string
	// Format is the session's capture_mode (e.g. "640x480 MJPEG @ 10 fps"):
	// the negotiated format the signal reports.
	Format string
	// FPSTarget is the fps of that mode (the target half of "fps actual vs
	// target"); 0 when the source kind has no named rate.
	FPSTarget float64
	// Source is the source's self-report (the measured fps, the last-frame
	// time, the lifecycle state, the last failure text).
	Source SourceInfo
	// Picture is the active-picture window: the tracker's accepted window once
	// it has settled one, the whole frame plus Settled false until then.
	Picture CapturePictureView
	// Dialogue is the last dialogue-region crop, nil until the run has
	// computed one (or the profile has no dialogue region).
	Dialogue *CaptureCropView
	// Latest is the newest frame in memory, nil until the first delivery;
	// the preview serves it and the signal's active-picture judgment reads it.
	Latest *capture.Frame
}

// Pause stops a running session's commitment and speech
// (docs/spec/08 "…/pause": "pause stops new speech and fences pending
// utterances"). It is one control message to the session's single writer:
// the reducer stops committing (the commit gate reads the paused flag) and
// cancels the in-flight recognitions and rankings, and the session's speech
// wiring — subscribed to the same event stream on the same chain — fences
// the audio still queued or playing. The answer is the session's fresh
// snapshot (the operator's UI reads paused from it, so the button shows the
// server's state). Pressing pause on an already-paused session is a no-op
// that returns the same snapshot: a duplicate is not an error the operator
// needs.
func (m *Manager) Pause(id string) (Info, error) {
	return m.controlSession(id, ses.ControlMsg{
		Kind:   ses.ControlPause,
		Reason: "operator",
	}, func(i Info) (bool, error) {
		if i.Paused {
			return true, nil // idempotent: already where the pause wanted to be
		}
		return false, nil
	})
}

// Resume continues a paused session from the next commit
// (docs/spec/08 "…/resume"). The session's text machines kept advancing
// through the pause — the observations were real — so the resume changes no
// belief: the commit policy picks up where the evidence stands, and the next
// box that is ready with a valid ranking commits exactly once. A resume can
// therefore never burst a backlog of the paused window's lines; the backlog
// was never held, it was observed and decided, and the decisions are
// idempotent. Symmetric with Pause, resuming an unpaused session is a no-op.
func (m *Manager) Resume(id string) (Info, error) {
	return m.controlSession(id, ses.ControlMsg{
		Kind:   ses.ControlResume,
		Reason: "operator",
	}, func(i Info) (bool, error) {
		if !i.Paused {
			return true, nil
		}
		return false, nil
	})
}

// SetSettings applies the operator's mid-session policy (docs/spec/08
// "…/settings", docs/spec/09 "/play"): the decision mode, the narrated
// classes and the speech side's catch-up narration policy (issue #25).
// The body is partial (a nil field keeps its current value); the merged
// request is validated against the documented vocabulary (mode is the
// Mode* constants, the classes are games.TextKind words, catch_up is the
// session policy's word), and a request that names the state the session
// already has is a no-op — no control message, no event — because a
// settings message is an operator-visible change, and one that changed
// nothing would be a lie in the event log. The answer is the session's
// fresh snapshot, which carries the applied mode, classes and policy.
func (m *Manager) SetSettings(id string, body SessionSettings) (Info, error) {
	sess, err := m.runningSession(id)
	if err != nil {
		return Info{}, err
	}
	sess.mu.Lock()
	cur := sess.info
	sess.mu.Unlock()

	mode := cur.Mode
	if body.Mode != nil && *body.Mode != "" {
		mode = *body.Mode
	}
	if mode != ModeConservative && mode != ModePredictive {
		return Info{}, fmt.Errorf("%w: mode must be %q or %q, got %q",
			ErrInvalidSessionSettings, ModeConservative, ModePredictive, mode)
	}

	kinds := cur.NarratedClasses
	if body.NarratedClasses != nil {
		kinds = make([]games.TextKind, 0, len(*body.NarratedClasses))
		for _, w := range *body.NarratedClasses {
			k := games.TextKind(w)
			if !validTextKind(k) {
				return Info{}, fmt.Errorf("%w: unknown text kind %q", ErrInvalidSessionSettings, w)
			}
			kinds = append(kinds, k)
		}
	}

	catchUp := cur.CatchUp
	if body.CatchUp != nil && *body.CatchUp != "" {
		if _, err := ses.ValidateCatchUpPolicy(*body.CatchUp); err != nil {
			return Info{}, fmt.Errorf("%w: %v", ErrInvalidSessionSettings, err)
		}
		catchUp = *body.CatchUp
	}

	// The unmatched read-through (ADR-0136): a plain bool, no vocabulary to
	// validate — the documented values are true and false. A nil field keeps
	// the session's current value (the partial-body rule), and the value is
	// the snapshot's, so a body that names what the session already has is a
	// no-op below.
	readThrough := cur.UnmatchedReadThrough
	if body.UnmatchedReadThrough != nil {
		readThrough = *body.UnmatchedReadThrough
	}

	norm := normalizeKinds(kinds)
	sameMode := mode == cur.Mode
	sameKinds := sameKinds(norm, cur.NarratedClasses)
	sameCatchUp := catchUp == cur.CatchUp
	sameReadThrough := readThrough == cur.UnmatchedReadThrough
	if sameMode && sameKinds && sameCatchUp && sameReadThrough {
		return sess.Snapshot(), nil // the session already has this policy
	}

	predictive := mode == ModePredictive
	if !sess.sendControl(ses.ControlMsg{
		Kind:   ses.ControlSettings,
		Reason: "operator",
		Settings: ses.Settings{
			Predictive:    predictive,
			NarratedKinds: norm,
			CatchUp:       catchUp,
		},
	}) {
		return Info{}, fmt.Errorf("%w: the session's writer is not accepting controls", ErrSessionNotRunning)
	}
	// The read-through's speech half (ADR-0136): the policy lives on the
	// speech wiring, not the session reducer — the reducer's commit contract
	// (speech_eligible false for an unmatched box) is untouched, and the
	// wiring reads the flag at the next commit. A session without a speech
	// wiring (no voice workflow in this process) runs silent whatever the
	// policy, so the flag has nothing to move.
	if sess.speech != nil {
		sess.speech.setUnmatchedReadThrough(readThrough)
	}
	sess.mu.Lock()
	if sess.info.State == StateRunning || sess.info.State == StateStarting {
		sess.info.Mode = mode
		sess.info.NarratedClasses = norm
		sess.info.CatchUp = catchUp
		sess.info.UnmatchedReadThrough = readThrough
	}
	sess.mu.Unlock()
	return sess.Snapshot(), nil
}

// validTextKind reports whether a games.TextKind word is one of the documented
// classes. The vocabulary is the spec's; a word outside it is a 400, not a
// guess — a filter that cannot name a class must not invent one.
func validTextKind(k games.TextKind) bool {
	switch k {
	case games.TextDialogue, games.TextSign, games.TextItem,
		games.TextMenu, games.TextTutorial, games.TextSystem, games.TextOther:
		return true
	default:
		return false
	}
}

// normalizeKinds is the session reducer's narrated-class normalization for the
// API's sake: dedupe, force dialogue first (the product's default), keep the
// request's order for the rest. It is the runtime's copy of the rule because
// the runtime is what validates the request — the reducer would refuse the
// message anyway, and a 500 is a worse answer than a 400.
func normalizeKinds(kinds []games.TextKind) []games.TextKind {
	seen := map[games.TextKind]bool{}
	out := []games.TextKind{}
	if len(kinds) == 0 {
		return []games.TextKind{games.TextDialogue}
	}
	// dialogue first when it is requested; when it is not, it is still first,
	// because the commitment policy force-includes it (the session reducer's
	// normalize does the same, and the snapshot must show what is applied).
	if !seen[games.TextDialogue] {
		out = append(out, games.TextDialogue)
		seen[games.TextDialogue] = true
	}
	for _, k := range kinds {
		if k == games.TextDialogue || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// sameKinds compares two normalized narrated-class lists element-wise. Both
// sides are normalizeKinds' output (dialogue first, deduped), so value
// equality is the right test — the order is part of the shape.
func sameKinds(a, b []games.TextKind) bool {
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

// controlSession is the shared path of the binary live controls (pause,
// resume): look the session up, confirm it can still be steered (a terminal
// session is 409, not a dropped message), apply the idempotence check the
// caller supplies against the current snapshot, and — when the control is
// new — hand it to the session's single writer and update the snapshot's
// operator state. The snapshot the answer carries is taken after the send,
// so it shows the state the control intended, not the state the click made.
func (m *Manager) controlSession(id string, ctl ses.ControlMsg, skip func(Info) (bool, error)) (Info, error) {
	sess, err := m.runningSession(id)
	if err != nil {
		return Info{}, err
	}
	if skip != nil {
		if do, err := skip(sess.Snapshot()); err != nil {
			return Info{}, err
		} else if do {
			return sess.Snapshot(), nil
		}
	}
	if !sess.sendControl(ctl) {
		return Info{}, fmt.Errorf("%w: the session's writer is not accepting controls", ErrSessionNotRunning)
	}
	sess.mu.Lock()
	if sess.info.State == StateRunning || sess.info.State == StateStarting {
		switch ctl.Kind {
		case ses.ControlPause:
			sess.info.Paused = true
		case ses.ControlResume:
			sess.info.Paused = false
		}
	}
	sess.mu.Unlock()
	return sess.Snapshot(), nil
}

// runningSession looks a session up and answers ErrSessionNotRunning when it
// has reached a terminal state — the live-control rule: a control is refused,
// with the 409 the spec names, rather than sent into a run that is already
// over. A missing session is the distinct ErrSessionNotFound (a 404), because
// "no such session" and "the session is over" are different answers to the
// operator.
func (m *Manager) runningSession(id string) (*session, error) {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, ErrManagerClosed
	}
	sess, ok := m.sessionByID(id)
	if !ok {
		return nil, ErrSessionNotFound
	}
	sess.mu.Lock()
	state := sess.info.State
	sess.mu.Unlock()
	if state == StateStopped || state == StateFailed {
		return nil, ErrSessionNotRunning
	}
	return sess, nil
}

// sendControl hands one live control to the session's single writer. The send
// is non-blocking into the buffer: the buffer exists so a burst of operator
// clicks never blocks the caller, and a full buffer means the writer is not
// draining — a wedged session, which the caller answers as not-running rather
// than pretending the control landed. A closed channel (the run is over) is
// the same answer; the close flag is checked under the same lock that run()
// takes to close, so a control is never sent into a channel that is closing
// or closed — a send on a closed channel would panic, and a panic in the
// operator's click handler is a worse answer than the refusal.
func (s *session) sendControl(m ses.Msg) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctrlClosed {
		return false
	}
	select {
	case s.controls <- m:
		return true
	default:
		return false
	}
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
	// The session gauge of docs/spec/13 "Metrics" (booked at registration
	// in Start): the session is gone from the manager's map, so the count
	// drops by one. nil-safe: an off process books nothing.
	m.metrics.SessionStopped()
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

	// The process-shared OCR pool (issue #13, ADR-0100) lives for the
	// process; the server's shutdown is its only exit. Clearing it under the
	// lock (and out of the way before Close's kill-and-reap) makes a
	// concurrent WarmOCR's pool — or a second Close — find nothing: the pool
	// is closed exactly once, and a warm tesseract never outlives the
	// server. Close is idempotent and reaps in-flight spawns, so it is safe
	// to run against a pool whose warm slots are still settling.
	m.mu.Lock()
	pool := m.ocrPool
	m.ocrPool = nil
	m.mu.Unlock()
	if pool != nil {
		_ = pool.Close()
	}
}

// speakerLinkResolver is the roster's speaker resolution (ADR-0070's link,
// ADR-0101): a line whose speaker character is identified by another live
// character — the placeholder's name, the opaque speaker key the
// speaker-link folded into the target's aliases — resolves to the target, so
// the line speaks with the character the curator settled the identity to,
// and with the voice that character carries.
//
// The rule is deliberately narrow. It follows the link the speaker-link
// action leaves behind (placeholder retired, its name in the target's
// aliases, the lines re-pointed), it resolves at most one hop (a chain of
// aliases is a roster defect, not a resolution path), and it leaves
// everything else alone: a speaker that matches no character resolves to
// itself, an unattributed line stays unattributed, and a retired speaker
// whose name no live character carries stays retired — nothing is invented,
// the docs/spec/04 rule for the whole attribution pipeline.
func speakerLinkResolver(characters []games.Character) func(string) string {
	names := make(map[string]string, len(characters))
	byName := map[string]string{}
	for _, c := range characters {
		names[c.ID] = c.Name
		if c.Status == games.CharacterRetired {
			// A retired row identifies nobody: its identity is the target's.
			continue
		}
		var aliases []string
		if len(c.Aliases) > 0 {
			_ = json.Unmarshal(c.Aliases, &aliases)
		}
		for _, a := range aliases {
			if a == "" {
				continue
			}
			if _, taken := byName[a]; !taken {
				// Two live characters claiming one spelling is a roster
				// contradiction the import would have reported; the first
				// stands, and the line keeps its own speaker rather than
				// guessing between the two.
				byName[a] = c.ID
			}
		}
	}
	return func(speakerID string) string {
		if speakerID == "" {
			return speakerID
		}
		name, known := names[speakerID]
		if !known || name == "" {
			return speakerID
		}
		if target, ok := byName[name]; ok && target != speakerID {
			return target
		}
		return speakerID
	}
}

// revealDetector is the profile's game reveal-complete marker (ADR-0152):
// the first source whose parser ("<adapter id>@<version>") names a
// registered adapter offering the capability. Nil when none does.
func (m *Manager) revealDetector(sources []games.Source) pipeline.RevealCompleteDetector {
	if m.opts.Adapters == nil {
		return nil
	}
	for _, src := range sources {
		id, _, _ := strings.Cut(src.Parser, "@")
		if id == "" {
			continue
		}
		a, ok := m.opts.Adapters.Get(id)
		if !ok {
			continue
		}
		if d, ok := a.(adapter.RevealCompleteDetector); ok {
			return d
		}
	}
	return nil
}
