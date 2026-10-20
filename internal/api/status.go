package api

// The global status bar's backend (docs/design/plum-peach "Global chrome"):
// one typed snapshot of what the box is doing, a canonical short status
// string rendered server-side so every client shows the same text, and an
// SSE stream that pushes it on change at no more than twice a second.
//
// The snapshot aggregates four halves, each read from a seam that is cheap
// by construction — the rule the redesign sets is that the status bar never
// triggers a heavy probe:
//
//   - the active session: the runtime manager's one-active-session
//     snapshot (fps actual vs target, dropped frames, lines spoken, and the
//     frame→voice p50 the session samples per line);
//   - the active setup runs: the agent run log's live rows (state, stage,
//     characters voiced vs total, elapsed) — two indexed COUNTs each;
//   - the health rollup: cached answers only. Storage reads the readiness
//     probe through a TTL cache; the model-backed components read the
//     newest answered diagnostics run (the doctor's own cached report,
//     ADR-0087); capture and OCR read the live session's in-memory
//     counters. A component nobody has probed is "unknown", never a fresh
//     probe smuggled in through the status bar;
//   - the device: version, uptime and the bound listen address.
//
// GET /api/v1/status answers with a fresh assembly on every request (the
// assembly itself is the cheap part). GET /api/v1/status/stream is an SSE
// stream of status.changed frames with Last-Event-ID resume through the
// same hub the event stream uses: the push side is a single engine that
// assembles at 2 Hz and publishes only when the displayed shape changed,
// coalesced to at most one publish per 500 ms.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	rt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
)

const (
	// statusPath is the snapshot read (docs/design/plum-peach "Global
	// chrome"; docs/spec/08 "APIs").
	statusPath = "/api/v1/status"

	// statusStreamPath is the on-change push of the same shape.
	statusStreamPath = "/api/v1/status/stream"

	// statusEventName is the only name the stream carries. It is a real
	// hub event (so Last-Event-ID replay, heartbeats and gap notices come
	// from the same machinery the event stream documents) but it is not a
	// session event: SessionID is empty.
	statusEventName = "status.changed"

	// statusEngineTick is the assembly cadence; statusEngineMinPush is the
	// publish floor (the ≤2/s the redesign sets). Together they bound the
	// stream to one published frame per half second.
	statusEngineTick    = 500 * time.Millisecond
	statusEngineMinPush = 500 * time.Millisecond
)

// ── health vocabulary ──────────────────────────────────────────────────────

// The six component ids of the design's health grid (docs/design/plum-peach
// 2i), in the canonical order the status string and the snapshot use.
var statusComponents = []string{
	"capture",
	"ocr",
	"decision",
	"tts",
	"agent",
	"storage",
}

// The level vocabulary. "unknown" is load-bearing: a component nobody has
// probed is not ok, and reporting it as ok is the lie a "5 ok · 1 slow"
// string would otherwise tell for a box that was never checked.
const (
	statusLevelOK      = "ok"
	statusLevelSlow    = "slow"
	statusLevelFail    = "fail"
	statusLevelUnknown = "unknown"
)

// agentStateReview is the agent run state whose bar word is not its own
// name (docs/spec/03's "review" means "waiting on the curator's answer").
const agentStateReview = "review"

// ── the snapshot ───────────────────────────────────────────────────────────

// StatusSnapshot is GET /api/v1/status and the data of every
// status.changed frame.
type StatusSnapshot struct {
	// UpdatedAt is when this snapshot was assembled (RFC 3339, UTC).
	UpdatedAt string `json:"updated_at"`
	// Status is the canonical short string the bar renders, produced by
	// this server (RenderStatus): every client shows the same text.
	Status string `json:"status"`
	// Session is the active narration session, or absent when the box is
	// not narrating.
	Session *StatusSession `json:"session,omitempty"`
	// Setup is the active onboarding runs (usually zero or one), newest
	// first.
	Setup []StatusSetupRun `json:"setup_runs,omitempty"`
	// Health is the component rollup.
	Health StatusHealth `json:"health"`
	// Device is the box itself.
	Device StatusDevice `json:"device"`
}

// StatusSession is the active session's bar view.
type StatusSession struct {
	SessionID string `json:"session_id"`
	Game      string `json:"game,omitempty"`
	Mode      string `json:"mode"`
	State     string `json:"state"`
	Paused    bool   `json:"paused"`
	Source    string `json:"source"`
	// CaptureFPS is the source's measured rate (0 when it has not
	// delivered frames yet).
	CaptureFPS float64 `json:"capture_fps"`
	// CaptureFPSTarget is the rate the session asked of its device; 0 for
	// source kinds with no named rate.
	CaptureFPSTarget float64 `json:"capture_fps_target,omitempty"`
	// FramesDropped is the pipeline's dropped-frame counter.
	FramesDropped uint64 `json:"frames_dropped"`
	// LinesSpoken is the audio counter of playbacks the sink started —
	// the lines the box actually said, not the lines it committed.
	LinesSpoken int64 `json:"lines_spoken"`
	// FrameToVoiceP50MS is the status line's latency number, see
	// frameToVoiceTotal: the frame→committed p50 (the pipeline's per-line
	// histogram) plus the commit→voice p50 (the session's last-N window),
	// each half contributing when it has samples. 0 when no half has any.
	FrameToVoiceP50MS int64 `json:"frame_to_voice_p50_ms"`
	// FrameToVoiceSamples is how many lines went into the commit→voice
	// half (0 = that half has no samples yet and the number is the
	// pipeline's frame→committed half alone).
	FrameToVoiceSamples int `json:"frame_to_voice_samples"`
	// OCRCalls and OCRErrors are the pipeline's OCR counters: the evidence
	// half of the OCR health tile, and the "why" behind a slow tile.
	OCRCalls  uint64 `json:"ocr_calls"`
	OCRErrors uint64 `json:"ocr_errors"`
	// ElapsedMS is the run's age in whole milliseconds.
	ElapsedMS int64 `json:"elapsed_ms"`
	// CreatedAt is the run's start (RFC 3339, UTC).
	CreatedAt string `json:"created_at"`
}

// StatusSetupRun is one active onboarding run's bar view.
type StatusSetupRun struct {
	RunID string `json:"run_id"`
	// Game is the run's game title: the profile's game when the run has a
	// profile, the plan's title before that, "" when neither names one.
	Game    string `json:"game,omitempty"`
	State   string `json:"state"`
	Stage   string `json:"stage,omitempty"`
	Voiced  int    `json:"voiced"`
	Total   int    `json:"characters"`
	Elapsed int64  `json:"elapsed_ms"`
}

// StatusHealthComponent is one health tile's row.
type StatusHealthComponent struct {
	ID     string `json:"id"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

// StatusHealth is the rollup the bar renders as "5 ok · 1 slow".
type StatusHealth struct {
	Components []StatusHealthComponent `json:"components"`
	OK         int                     `json:"ok"`
	Slow       int                     `json:"slow"`
	Fail       int                     `json:"fail"`
	Unknown    int                     `json:"unknown"`
	// Detail is the rendered rollup (the idle bar's whole string).
	Detail string `json:"detail"`
}

// StatusDevice is the box's own half.
type StatusDevice struct {
	Version    string `json:"version"`
	UptimeMS   int64  `json:"uptime_ms"`
	ListenAddr string `json:"listen_addr,omitempty"`
}

// sessionFacts is the assembly's internal view of the active session: the
// runtime's Info plus the decision tile's live input (the degraded flag and
// its reason). The wire shape (StatusSession) is the bar's fields only; the
// tile's inputs stay here.
type sessionFacts struct {
	info      rt.Info
	decDeg    bool
	decDetail string
}

// ── assembly ───────────────────────────────────────────────────────────────

// statusReadinessTTL bounds how often the readiness probe is actually run
// for the storage tile: the probe is the cache, the TTL is the staleness
// the bar accepts. 30 s is long enough that a poll at 2 Hz costs the probe
// at most twice a minute, short enough that a database that dies while the
// box narrates turns the tile red within half a minute.
const statusReadinessTTL = 30 * time.Second

// statusReadinessCache is the readiness half of the health rollup: the
// probe runs at most once per TTL, and a failing probe is the answer
// (fail + the probe's own error text), never a panic.
type statusReadinessCache struct {
	mu        sync.Mutex
	lastProbe time.Time
	ok        bool
	detail    string
}

func (c *statusReadinessCache) get(ctx context.Context, probe func(context.Context) (bool, string), now func() time.Time) (level, detail string) {
	c.mu.Lock()
	if c.lastProbe.IsZero() || now().Sub(c.lastProbe) >= statusReadinessTTL {
		c.lastProbe = now()
		c.mu.Unlock()
	} else {
		ok, detail := c.ok, c.detail
		c.mu.Unlock()
		if ok {
			return statusLevelOK, detail
		}
		return statusLevelFail, detail
	}
	ok, detail := false, "readiness probe unavailable"
	if probe != nil {
		ok, detail = probe(ctx)
	}
	c.mu.Lock()
	c.ok = ok
	c.detail = detail
	c.mu.Unlock()
	if ok {
		return statusLevelOK, detail
	}
	return statusLevelFail, detail
}

// gameTitleCache keeps the profile→game title join out of the hot path: a
// title does not change under a profile id (content rows are per-revision,
// the id is per-revision), so one lookup per profile id is the whole cost
// of the session's and the run's game name. A failed lookup is not cached
// (a transient store error must not pin an empty name for the process).
type gameTitleCache struct {
	mu     sync.Mutex
	titles map[string]string
}

func newGameTitleCache() *gameTitleCache { return &gameTitleCache{titles: make(map[string]string, 8)} }

func (c *gameTitleCache) get(ctx context.Context, st *store.Store, profileID string) string {
	if st == nil || profileID == "" {
		return ""
	}
	c.mu.Lock()
	if t, ok := c.titles[profileID]; ok {
		c.mu.Unlock()
		return t
	}
	c.mu.Unlock()
	t, err := st.GameTitleForProfile(ctx, profileID)
	if err != nil || t == "" {
		return ""
	}
	c.mu.Lock()
	c.titles[profileID] = t
	c.mu.Unlock()
	return t
}

// assembleStatus is the snapshot assembly. It runs at request time (GET)
// and at the engine's 2 Hz (the stream), so every seam it touches must be
// in-memory or a bounded indexed read — the seam set here is the whole
// set: no doctor probe, no OCR call, no provider call.
func (s *Server) assembleStatus(ctx context.Context, now time.Time) StatusSnapshot {
	snap := StatusSnapshot{UpdatedAt: now.UTC().Format(time.RFC3339)}

	var facts *sessionFacts
	// (a) the active session. The nil guard is the pattern the other
	// session seams use (a process without a manager wired answers the
	// session's shape as "no active session", never a 500): the bar on such
	// a box is the idle rollup, which is the honest thing to show.
	if s.sessions != nil {
		if info, ok := s.sessions.Active(); ok {
			facts = &sessionFacts{info: info}
			if info.Decision != nil && info.Decision.Degraded {
				facts.decDeg = true
				facts.decDetail = "decision degraded"
				if info.Decision.Reason != "" {
					facts.decDetail = "decision degraded: " + info.Decision.Reason
				}
			}
			snap.Session = s.sessionView(ctx, now, facts)
		}
	}

	// (b) the active setup runs.
	snap.Setup = s.assembleSetupRuns(ctx, now)

	// (c) the health rollup, and (d) the device.
	snap.Health = s.assembleHealth(ctx, now, snap.Session, facts)
	snap.Device = StatusDevice{
		Version:    s.build.Version,
		UptimeMS:   elapsedMS(now, s.startedAt),
		ListenAddr: s.listenAddr,
	}

	snap.Status = RenderStatus(snap)
	return snap
}

// sessionView shapes the runtime's Info into the wire shape and computes
// the combined latency number.
func (s *Server) sessionView(ctx context.Context, now time.Time, f *sessionFacts) *StatusSession {
	ss := &StatusSession{
		SessionID:        f.info.ID,
		Game:             s.gameTitle.get(ctx, s.store, f.info.ProfileID),
		Mode:             f.info.Mode,
		State:            f.info.State,
		Paused:           f.info.Paused,
		Source:           f.info.SourceKind,
		CaptureFPS:       f.info.Source.FPS,
		CaptureFPSTarget: f.info.CaptureFPSTarget,
		FramesDropped:    f.info.Stats.FramesDropped,
		OCRCalls:         f.info.Stats.OCRCalls,
		OCRErrors:        f.info.Stats.OCRErrors,
		ElapsedMS:        elapsedMS(now, f.info.CreatedAt),
		CreatedAt:        f.info.CreatedAt.UTC().Format(time.RFC3339),
	}
	if f.info.Audio != nil {
		ss.LinesSpoken = f.info.Audio.Speech.Started
	}
	ss.FrameToVoiceP50MS, ss.FrameToVoiceSamples = frameToVoiceTotal(f.info)
	return ss
}

// frameToVoiceTotal combines the two measured halves into the bar's number.
// The pipeline's frame→committed histogram is whole-run (its p50 is stable
// but cold-start-tinted); the session's commit→voice window is last-N
// (current but blind to the frame half). The sum is the estimate the status
// line documents: each half measured per line, neither half invented.
func frameToVoiceTotal(info rt.Info) (int64, int) {
	var ms int64
	samples := info.CommitToVoice.Count
	if samples > 0 {
		ms += info.CommitToVoice.P50MS
	}
	if fc := info.Stats.Latency.FrameToCommitted; fc.Count > 0 {
		ms += fc.P50.Milliseconds()
	}
	return ms, samples
}

// assembleSetupRuns reads the live agent runs and their counts. A process
// without the agent log wired (s.onb nil, or a test server) answers an
// empty list: "no setup run" is true there.
func (s *Server) assembleSetupRuns(ctx context.Context, now time.Time) []StatusSetupRun {
	if s.onb == nil || s.onb.AgentStore == nil {
		return nil
	}
	runs, err := s.onb.AgentStore.ListActiveAgentRuns(ctx)
	if err != nil {
		s.log.Warn("status: listing active agent runs", "error", err)
		return nil
	}
	out := make([]StatusSetupRun, 0, len(runs))
	for _, run := range runs {
		view := StatusSetupRun{
			RunID:   run.ID,
			State:   run.State,
			Elapsed: elapsedMS(now, run.CreatedAt),
		}
		if run.ProfileID != "" {
			view.Game = s.gameTitle.get(ctx, s.store, run.ProfileID)
			if s.store != nil {
				total, voiced, err := s.store.ProfileCharacterVoiceCounts(ctx, run.ProfileID)
				if err == nil {
					view.Total, view.Voiced = total, voiced
				}
			}
		} else if title := store.AgentPlanTitle(run.PlanJSON); title != "" {
			view.Game = title
		}
		// The stage is the newest tool call — the step the agent is on:
		// its name while in flight, its settled name once done. A run
		// with no tool calls yet is at the queued edge and names none.
		if tc, err := s.onb.AgentStore.LatestAgentToolCall(ctx, run.ID); err == nil && tc.Name != "" {
			view.Stage = tc.Name
		}
		out = append(out, view)
	}
	return out
}

// assembleHealth rolls the six tiles from their cached sources. Order is
// the canonical component order, so a rendered "tts slow · ocr fail" is the
// same order on every client.
func (s *Server) assembleHealth(ctx context.Context, now time.Time, session *StatusSession, facts *sessionFacts) StatusHealth {
	views := make([]statusComponentView, len(statusComponents))
	if session != nil {
		views[0] = captureHealthFromSession(session)
		views[1] = ocrHealthFromSession(session)
	} else {
		views[0] = statusComponentView{Level: statusLevelUnknown, Detail: "no active capture"}
		views[1] = statusComponentView{Level: statusLevelUnknown, Detail: "not active"}
	}
	// The model-backed halves: the session's live degradation flag beats
	// the last doctor run (it is newer), the doctor run beats nothing.
	views[2] = statusComponentView{Level: statusLevelUnknown, Detail: "not probed"}
	views[3] = statusComponentView{Level: statusLevelUnknown, Detail: "not probed"}
	views[4] = statusComponentView{Level: statusLevelUnknown, Detail: "not probed"}
	if run := s.latestDiagnosticsRun(); run != nil {
		if v, ok := doctorRoleView(run, "decision"); ok {
			views[2] = v
		}
		if v, ok := doctorRoleView(run, "voice_design"); ok {
			views[3] = v
		}
		if v, ok := doctorRoleView(run, "agent_reasoning"); ok {
			views[4] = v
		}
	}
	if session != nil && facts != nil && facts.decDeg {
		views[2] = statusComponentView{Level: statusLevelSlow, Detail: facts.decDetail}
	}
	views[5] = statusComponentView{Level: statusLevelUnknown, Detail: "no readiness probe wired"}
	// The probe is the cache and the TTL is its staleness. A process with
	// no readiness seam wired does not get a fail tile (a fail says the
	// database is broken; the honest word is that nobody checked): the
	// tile stays unknown, which the rollup renders as a count.
	if s.statusReadiness != nil && s.readiness != nil {
		fixedNow := now
		level, detail := s.statusReadiness.get(ctx, s.runReadiness, func() time.Time { return fixedNow })
		views[5] = statusComponentView{Level: level, Detail: detail}
	}

	health := StatusHealth{Components: make([]StatusHealthComponent, 0, len(statusComponents))}
	for i, comp := range statusComponents {
		c := StatusHealthComponent{ID: comp, Level: views[i].Level, Detail: views[i].Detail}
		health.Components = append(health.Components, c)
		switch c.Level {
		case statusLevelOK:
			health.OK++
		case statusLevelSlow:
			health.Slow++
		case statusLevelFail:
			health.Fail++
		case statusLevelUnknown:
			health.Unknown++
		}
	}
	health.Detail = renderHealthRollup(health)
	return health
}

// runReadiness is the readiness probe wrapped to the cache's shape: the
// server's own readiness seam (the same one /health/ready answers from), so
// the storage tile and the readiness endpoint never disagree. The probe is
// bounded the same way the endpoint bounds it: a stuck dependency makes the
// tile "fail: timed out", not the whole status request.
func (s *Server) runReadiness(ctx context.Context) (bool, string) {
	if s.readiness == nil {
		return false, "no readiness probe wired"
	}
	cctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	body, ok := safeReadiness(cctx, s.readiness, s.log)
	if !ok {
		return false, readinessFailureDetail(body)
	}
	return true, "database ready"
}

// readinessFailureDetail names the failed check from the readiness body
// (the storeReadiness shape: {"database": {"status": "fail", "error": …}}).
// A body it does not recognise is the honest "a dependency is not ready".
func readinessFailureDetail(body map[string]any) string {
	for name, raw := range body {
		check, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if status, _ := check["status"].(string); status == "fail" || status == "degraded" {
			detail := ""
			if e, ok := check["error"].(string); ok {
				detail = e
			}
			if detail == "" {
				detail = "not ready"
			}
			return fmt.Sprintf("%s: %s", name, detail)
		}
	}
	return "a dependency is not ready"
}

// latestDiagnosticsRun is the cached-doctor half of the health rollup: the
// newest run with at least one answered role, preferring a finished one.
// It is a read of the in-memory run store (ADR-0087) — the probe already
// happened when the run did, which is the whole point of the seam.
func (s *Server) latestDiagnosticsRun() *diagnosticsRun {
	s.diagRuns.mu.Lock()
	defer s.diagRuns.mu.Unlock()
	var fallback *diagnosticsRun
	for _, id := range s.diagRuns.order {
		run := s.diagRuns.runs[id]
		if len(run.roleViewsSnapshot()) == 0 {
			continue
		}
		if run.isDone() {
			return run
		}
		if fallback == nil {
			fallback = run
		}
	}
	return fallback
}

// doctorRoleView maps one answered role's doctor status onto a component
// view: OK → ok, DEGRADED → slow, BLOCKED and FAIL → fail, NOT CONFIGURED
// → unknown with its own word (a box with no decision model configured is
// not failing, it is running local — the tile says so).
func doctorRoleView(run *diagnosticsRun, role string) (statusComponentView, bool) {
	for _, v := range run.roleViewsSnapshot() {
		if v.Role != role {
			continue
		}
		detail := v.Summary
		if detail == "" {
			detail = v.Detail
		}
		if detail == "" {
			detail = v.Status
		}
		switch v.Status {
		case "OK":
			return statusComponentView{Level: statusLevelOK, Detail: detail}, true
		case "DEGRADED":
			return statusComponentView{Level: statusLevelSlow, Detail: detail}, true
		case "BLOCKED", "FAIL":
			return statusComponentView{Level: statusLevelFail, Detail: detail}, true
		case "NOT CONFIGURED":
			return statusComponentView{Level: statusLevelUnknown, Detail: "not configured"}, true
		}
		return statusComponentView{Level: statusLevelUnknown, Detail: detail}, true
	}
	return statusComponentView{}, false
}

// statusComponentView is the internal (level, detail) pair before it is
// laid onto the canonical component order.
type statusComponentView struct {
	Level  string
	Detail string
}

// captureHealthFromSession is the capture tile from the live session's own
// counters: the source is the authority on whether the card is delivering,
// and the rate is the authority on whether it is delivering fast enough.
func captureHealthFromSession(ss *StatusSession) statusComponentView {
	if ss.CaptureFPS > 0 {
		if ss.CaptureFPSTarget > 0 && ss.CaptureFPS < ss.CaptureFPSTarget/2 {
			return statusComponentView{Level: statusLevelSlow, Detail: fmt.Sprintf("%.0f fps against a %.0f fps target", ss.CaptureFPS, ss.CaptureFPSTarget)}
		}
		return statusComponentView{Level: statusLevelOK, Detail: fmt.Sprintf("%.0f fps", ss.CaptureFPS)}
	}
	if ss.FramesDropped > 0 {
		return statusComponentView{Level: statusLevelSlow, Detail: fmt.Sprintf("%d frames dropped", ss.FramesDropped)}
	}
	if ss.State == "starting" {
		return statusComponentView{Level: statusLevelOK, Detail: "starting"}
	}
	return statusComponentView{Level: statusLevelOK, Detail: "active session"}
}

// ocrHealthFromSession is the OCR tile from the live pipeline counters:
// no errors in the run's reads is ok, a few is slow, a failing engine is
// fail. The counters are the same ones the session snapshot already
// carries, so the tile costs nothing the bar does not pay for anyway.
func ocrHealthFromSession(ss *StatusSession) statusComponentView {
	switch {
	case ss.OCRErrors > 0 && ss.OCRErrors >= ss.OCRCalls:
		return statusComponentView{Level: statusLevelFail, Detail: fmt.Sprintf("%d OCR errors", ss.OCRErrors)}
	case ss.OCRErrors > 0:
		return statusComponentView{Level: statusLevelSlow, Detail: fmt.Sprintf("%d OCR errors of %d reads", ss.OCRErrors, ss.OCRCalls)}
	case ss.OCRCalls > 0:
		return statusComponentView{Level: statusLevelOK, Detail: fmt.Sprintf("%d reads", ss.OCRCalls)}
	default:
		return statusComponentView{Level: statusLevelOK, Detail: "no OCR reads yet"}
	}
}

// ── the canonical string ──────────────────────────────────────────────────

// RenderStatus renders the bar's text from a snapshot (docs/design/plum-
// peach "Global chrome": "● agent running · 9 of 12 voiced · 18m 04s",
// "● live · capture 30fps · 412ms frame→voice", "5 ok · 1 slow"). It is a
// pure function of the snapshot — every client renders the same string
// from the same bytes — and it is the server's, so a client that renders
// its own variant is a client bug the contract test can name.
//
// Priority: a live session beats a setup run (narration is the box's
// foreground, docs/spec/01 invariant 1); non-ok health appends in both
// cases, because a non-ok tile "should also surface" in the bar; with
// neither the health rollup is the whole string.
func RenderStatus(snap StatusSnapshot) string {
	var head string
	switch {
	case snap.Session != nil:
		head = renderSessionHead(*snap.Session)
	case len(snap.Setup) > 0:
		head = renderSetupHead(snap.Setup[0])
	default:
		head = snap.Health.Detail
	}
	tail := nonOKHealth(snap.Health)
	if tail == "" {
		return head
	}
	return head + " · " + tail
}

func renderSessionHead(ss StatusSession) string {
	verb := "● live"
	switch {
	case ss.Paused:
		verb = "● paused"
	case ss.State != "running":
		verb = "● " + ss.State
	}
	var parts []string
	parts = append(parts, verb)
	if fps := barFPS(ss.CaptureFPS, ss.CaptureFPSTarget); fps > 0 {
		parts = append(parts, fmt.Sprintf("capture %dfps", int(fps+0.5)))
	}
	if ss.FrameToVoiceP50MS > 0 {
		parts = append(parts, fmt.Sprintf("%dms frame→voice", ss.FrameToVoiceP50MS))
	}
	if ss.LinesSpoken > 0 {
		parts = append(parts, fmt.Sprintf("%d lines", ss.LinesSpoken))
	}
	return strings.Join(parts, " · ")
}

func renderSetupHead(run StatusSetupRun) string {
	verb := "● agent " + run.State
	if run.State == agentStateReview {
		verb = "● agent needs review"
	}
	var parts []string
	parts = append(parts, verb)
	if run.Total > 0 || run.Voiced > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d voiced", run.Voiced, run.Total))
	}
	if run.Elapsed > 0 {
		parts = append(parts, formatElapsed(time.Duration(run.Elapsed)*time.Millisecond))
	}
	return strings.Join(parts, " · ")
}

// barFPS picks the number the bar shows: the measured rate when it has one,
// the target when the card has not delivered yet (a starting session is
// not "0 fps", it is "not measuring yet", and the target is the truer
// number).
func barFPS(actual, target float64) float64 {
	if actual > 0 {
		return actual
	}
	return target
}

// nonOKHealth is the bar's health tail: the slow and fail tiles in
// canonical order. "unknown" appends nothing — a tile nobody has probed is
// not a fault to shout about, and the rollup's "1 unknown" says the rest.
func nonOKHealth(h StatusHealth) string {
	var parts []string
	for _, c := range h.Components {
		switch c.Level {
		case statusLevelSlow, statusLevelFail:
			parts = append(parts, c.ID+" "+c.Level)
		}
	}
	return strings.Join(parts, " · ")
}

// renderHealthRollup is the idle bar: "5 ok · 1 slow", the non-zero groups
// in the fixed order ok → slow → fail → unknown. A box with all six ok
// reads "6 ok" — the absence of a problem is still a number, because the
// bar's job is to say what the box is doing and an empty bar is "nothing".
func renderHealthRollup(h StatusHealth) string {
	var parts []string
	if h.OK > 0 {
		parts = append(parts, fmt.Sprintf("%d ok", h.OK))
	}
	if h.Slow > 0 {
		parts = append(parts, fmt.Sprintf("%d slow", h.Slow))
	}
	if h.Fail > 0 {
		parts = append(parts, fmt.Sprintf("%d fail", h.Fail))
	}
	if h.Unknown > 0 {
		parts = append(parts, fmt.Sprintf("%d unknown", h.Unknown))
	}
	if len(parts) == 0 {
		return "no health data"
	}
	return strings.Join(parts, " · ")
}

// formatElapsed is the bar's duration shape: 42s, 18m 04s, 1h 02m, 2d 3h.
// Minutes always carry a two-digit second field (the design's "18m 04s"),
// which keeps the string's width stable across a bar's updates.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func elapsedMS(now, since time.Time) int64 {
	ms := now.Sub(since).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

// ── the push engine ───────────────────────────────────────────────────────

// statusEngine is the process's single status push loop. One loop per
// process (not one per stream client): K stream clients each get the same
// published frames from the hub, and the hub's replay is what makes a
// reconnecting client whole. It assembles at statusEngineTick and publishes
// only when the displayed shape changed, coalesced to at most one publish
// per statusEngineMinPush — the ≤2/s the redesign sets, and the bound a
// chatty session (a line committed every 200 ms) must not beat.
type statusEngine struct {
	s *Server

	mu         sync.Mutex
	lastKey    string
	lastPushAt time.Time
}

// statusEngineRef returns the process's engine, built lazily on first use.
// It is internal state like the diagnostics run store: the route degrades
// on the hub seam (the stream answers 503 when no hub is wired), not on
// the engine.
func (s *Server) statusEngineRef() *statusEngine {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.statusEng == nil {
		s.statusEng = &statusEngine{s: s}
	}
	return s.statusEng
}

// start runs the loop at most once per process. It is called from the
// stream handler, because the stream is the only consumer of the pushed
// frames: a process whose clients all read the snapshot by GET pays no
// push at all.
func (e *statusEngine) start() {
	e.s.statusEngineOnce.Do(func() {
		go e.run()
	})
}

// run is the loop: a baseline publish, then one tick per statusEngineTick.
// The loop is process-lived (it does not track subscribers: the hub owns
// subscriber state, and a loop that stopped when the bar's tab closed
// would leave the next tab waiting a whole tick for a frame the log never
// got). The loop exits only if the hub stops accepting publishes — the
// publish error is the shutdown signal, and after it the next stream
// request answers 503 through the hub's own rule.
func (e *statusEngine) run() {
	e.tick(context.Background())
	ticker := time.NewTicker(statusEngineTick)
	defer ticker.Stop()
	for {
		<-ticker.C
		if !e.tick(context.Background()) {
			return
		}
	}
}

// tick assembles once and publishes when the displayed shape changed. It
// reports whether the engine is still able to publish (false only on a
// hub that refuses, i.e. a shutdown — the loop's exit, not an error worth
// a warning per tick).
func (e *statusEngine) tick(ctx context.Context) bool {
	snap := e.s.assembleStatus(ctx, time.Now())
	key := statusCompareKey(snap)

	e.mu.Lock()
	now := time.Now()
	due := now.Sub(e.lastPushAt) >= statusEngineMinPush
	changed := key != e.lastKey
	if changed && due {
		e.lastPushAt = now
	}
	e.mu.Unlock()
	if !changed || !due {
		// A change that is not due is coalesced: the next tick re-reads
		// it (lastKey is untouched) and publishes when the floor clears.
		return true
	}

	data, err := json.Marshal(snap)
	if err != nil {
		// A snapshot that cannot be marshalled is a build-time bug in the
		// assembly; report it and keep serving GET (which answers the same
		// shape through its own writer).
		e.s.log.Error("status: cannot marshal snapshot", "error", err)
		return true
	}
	if _, err := e.s.hub.Publish(ctx, events.Envelope{
		Name: statusEventName,
		Data: data,
	}); err != nil {
		if errors.Is(err, events.ErrClosed) {
			e.s.log.Debug("status: hub closed; the push loop ends")
			return false
		}
		e.s.log.Warn("status: publish failed", "error", err)
		return true
	}
	e.mu.Lock()
	e.lastKey = key
	e.mu.Unlock()
	return true
}

// statusCompareKey is the coarse projection the push decision compares:
// the displayed numbers, not the measured ones. The rendered string is
// already coarse (integer fps, integer ms, bucketed elapsed), and the
// counters plus the health levels are the rest of what a client displays.
// Uptime, updated_at and the health details are excluded on purpose:
// publishing a new updated_at for a box that did nothing is the exact
// churn the "push on change" rule exists to stop.
func statusCompareKey(snap StatusSnapshot) string {
	var cmp struct {
		Status  string
		Version string
		Session *struct {
			State   string
			Mode    string
			Paused  bool
			Fps     int
			P50MS   int64
			Samples int
			Lines   int64
			Dropped uint64
		}
		Setup []struct {
			State   string
			Stage   string
			Voiced  int
			Total   int
			Elapsed int64 // bucketed to 5 s: the bar's text moves in 5-s steps
		}
		Health []struct {
			ID    string
			Level string
		}
	}
	cmp.Status = snap.Status
	cmp.Version = snap.Device.Version
	if snap.Session != nil {
		ss := snap.Session
		cmp.Session = &struct {
			State   string
			Mode    string
			Paused  bool
			Fps     int
			P50MS   int64
			Samples int
			Lines   int64
			Dropped uint64
		}{
			State:   ss.State,
			Mode:    ss.Mode,
			Paused:  ss.Paused,
			Fps:     int(barFPS(ss.CaptureFPS, ss.CaptureFPSTarget) + 0.5),
			P50MS:   ss.FrameToVoiceP50MS,
			Samples: ss.FrameToVoiceSamples,
			Lines:   ss.LinesSpoken,
			Dropped: ss.FramesDropped,
		}
	}
	for _, run := range snap.Setup {
		cmp.Setup = append(cmp.Setup, struct {
			State   string
			Stage   string
			Voiced  int
			Total   int
			Elapsed int64
		}{State: run.State, Stage: run.Stage, Voiced: run.Voiced, Total: run.Total, Elapsed: run.Elapsed / 5000})
	}
	for _, c := range snap.Health.Components {
		cmp.Health = append(cmp.Health, struct {
			ID    string
			Level string
		}{ID: c.ID, Level: c.Level})
	}
	b, err := json.Marshal(cmp)
	if err != nil {
		return snap.Status // the projection is display data; a marshal
		// failure degrades to the string alone, which is still a key.
	}
	return string(b)
}

// ── the routes ────────────────────────────────────────────────────────────

// handleStatus answers GET /api/v1/status with a fresh assembly. The
// assembly is the cheap half of the design (in-memory counters, two
// indexed COUNTs per setup run, a TTL'd probe for storage), so a fresh
// answer per request is what a viewer polling the bar should get: the
// same number the push side publishes, minus the wait for the next frame.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		s.writeProblem(w, r, http.StatusMethodNotAllowed, "method-not-allowed",
			"Method Not Allowed",
			fmt.Sprintf("method %s is not allowed on %s; the status snapshot is GET only", r.Method, statusPath))
		return
	}
	snap := s.assembleStatus(r.Context(), time.Now())
	writeJSON(w, http.StatusOK, snap)
}

// handleStatusStream answers GET /api/v1/status/stream: the SSE stream of
// status.changed frames, filtered from the hub the way the event stream
// filters its names — so Last-Event-ID resume, heartbeats and the
// resync-required gap notice all come from the documented machinery
// (docs/spec/08 "Event stream reliability"), not a second implementation.
// The stream is the snapshot's push half: a client that opens it and holds
// on sees the same string the GET answers, move for move.
func (s *Server) handleStatusStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		s.writeProblem(w, r, http.StatusMethodNotAllowed, "method-not-allowed",
			"Method Not Allowed",
			fmt.Sprintf("method %s is not allowed on %s; the status stream is GET only", r.Method, statusStreamPath))
		return
	}
	if s.hub == nil {
		// The same honest 503 as the event stream: a stream that opened
		// 200 with no publisher behind it would be a bar that silently
		// froze, which is worse than a bar that cannot open.
		s.writeProblem(w, r, http.StatusServiceUnavailable, "events-unavailable",
			"Event Stream Unavailable",
			"no event hub is configured in this process; the status stream has nothing to push")
		return
	}

	cursor, err := eventCursor(r)
	if err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-last-event-id",
			"Bad Request", err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "streaming-unsupported",
			"Streaming Unsupported",
			"the response writer does not support flushing; the status stream requires it")
		return
	}

	// The push loop exists from now on (idempotent per process): the
	// client that opens the stream is the reason the box has something to
	// push, and the first frame is due immediately, not after a tick.
	s.statusEngineRef().start()

	ctx := r.Context()
	sub, err := s.hub.Subscribe(ctx, events.Filter{Names: []string{statusEventName}}, cursor)
	if err != nil {
		s.streamFailure(w, r, "subscribe", err)
		return
	}
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", ContentTypeEventStream)
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	s.log.Debug("status stream opened",
		"request_id", RequestIDFromContext(ctx),
		"subscriber_id", sub.ID(),
		"after_seq", cursor,
	)
	// The event stream's pump is the pump: one subscriber queue, one
	// heartbeat interval, the same frame encoding and the same clean-end
	// rules. Reusing it is the "patterns, not copies" half of the task.
	if err := s.streamEvents(ctx, w, flusher, sub, s.hub.Heartbeat()); err != nil && !isStreamEnd(ctx, err) {
		s.log.Warn("status stream ended with an error",
			"request_id", RequestIDFromContext(ctx),
			"subscriber_id", sub.ID(),
			"error", err,
		)
	}
}
