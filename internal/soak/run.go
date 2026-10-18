// run.go is the harness: its resources, its two layers and the counters the
// sampler reads. The sidecar (layer B) owns the pass clock: it paces the
// clip's truth on the capture clock (the scenario's rule) so the provider
// load is one commit per line per pass, not per frame. Layer A (the pipeline)
// processes the frames at the configured replay speed — at Speed 0 that is
// full tilt, which is the "replay load" of docs/spec/12.

package soak

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/capture/replay"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/vision"
)

// faultEvent is one activated chaos fault: what fired, when (wall clock), the
// pipeline's commit count at the moment (the recovery criterion's baseline)
// and the detail the report names.
type faultEvent struct {
	Wall          time.Time `json:"wall"`
	Kind          string    `json:"kind"`
	Role          string    `json:"role"`
	Detail        string    `json:"detail"`
	CommitsAtFire uint64    `json:"commits_at_fire"`
	// Recovered and WaitMS are filled at evaluation: a commit landed after
	// the fault within the bound. Judged is false when the bound's window ran
	// past the run's end (the harness stopped the pipeline at run end, not
	// the fault): the fault is reported, not graded (faultRecoveryVerdict,
	// ADR-0051).
	Recovered bool  `json:"recovered"`
	Judged    bool  `json:"judged"`
	WaitMS    int64 `json:"wait_ms,omitempty"`
	Clip      int   `json:"clip,omitempty"`
	Pass      int   `json:"pass,omitempty"`
}

// passStamp is the sidecar's pass identity for assertion slicing.
type passStamp struct {
	clip int
	pass int
}

// harness is one soak run: the resources, the two layers and the counters.
type harness struct {
	cfg     Config
	chaos   *chaosRNG
	profile *scenario.Profile
	clips   []clip
	// lineIDs: every profile message id (the committed_only name set).
	lineIDs []string

	// gateways and clients (layer B's wire)
	decGW *decisionGW
	ttsGW *ttsGW
	dec   *systemone.Client
	tts   ttsClient

	// layer A
	idx      *matcher.Index
	engine   *truthEngine
	runner   *pipeline.Runner
	src      *cyclingSource
	pipeErr  error
	pipeDone chan struct{}

	// layer B
	sess    *session.Reducer
	pub     *sidecarPub
	wg      sync.WaitGroup // provider work (decisions, TTS)
	passCnt []int          // per clip: next pass the sidecar feeds

	// shared timeline
	mem events.Store // bounded ring (see build): the log must not outgrow the run
	hub *events.Hub

	// counters (the sampler reads them)
	sidecarCommits atomic.Int64
	speechStarted  atomic.Int64
	speechEnded    atomic.Int64
	providerErrs   atomic.Int64
	faultsFired    atomic.Int64
	panics         atomic.Int64
	faultKinds     sync.Map // kind -> *atomic.Int64
	errKinds       sync.Map // provider error kind -> *atomic.Int64
	faultEventsMu  sync.Mutex
	faultEvents    []faultEvent

	// pass timeline (bounded, see passstate.go): the unsettled pass slices —
	// the sidecar's own commit and speech events the scenario assertions
	// grade — plus the occurrence->pass routing a speech event needs to find
	// its slice. A slice is graded and dropped the moment it is settled
	// (every utterance that started in it has a terminal event), so the
	// retained state is bounded by the unsettled horizon (passBound), not
	// the run's pass count. (The 1-hour soak's RSS growth was this slice map
	// kept for every pass: ADR-0045 amendment.)
	passMu         sync.Mutex
	passStates     map[passStamp]*passState
	occPass        map[string]passStamp // occurrence id -> passStamp
	passBound      int
	passFailuresMu sync.Mutex
	passFailures   []string
	passesGraded   int
	// passCommitSlices counts the graded slices that carried a commit:
	// the evidence a sidecar pass actually committed (the report and the
	// pacing test read it).
	passCommitSlices int

	// sidecar dedupe: one ranking per reading per pass (the scenario's
	// lastKey, per pass).
	rankedMu sync.Mutex
	ranked   map[string]bool

	// spoken dedupes a double speak per occurrence (the session's occurrence
	// fence is the backstop; the map only catches what the fence cannot, in
	// the window where a duplicated reply is still in flight). Bounded
	// (spokenCap): on overflow the entries whose pass slice has settled (or
	// already been graded away) are evicted first; a fully unsettled map
	// falls back to clearing, which the fence's backstop makes safe.
	spokenMu sync.Mutex
	spoken   map[string]passStamp

	degraded sync.Map // role -> bool
	seq      atomic.Int64
}

func newHarness(cfg Config) (*harness, error) {
	profile, err := scenario.LoadProfile(filepath.Join(cfg.RepoRoot, "testdata", "synthetic", "profiles", cfg.Profile+".json"))
	if err != nil {
		return nil, fmt.Errorf("soak: profile: %v", err)
	}
	clips, err := loadClips(cfg)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(profile.Messages))
	for _, m := range profile.Messages {
		ids = append(ids, m.ID)
	}
	seed := cfg.Chaos.Seed
	if seed == 0 && cfg.Chaos.Enabled {
		seed = time.Now().UnixNano()
	}
	h := &harness{
		cfg:        cfg,
		chaos:      newChaosRNG(seed),
		profile:    profile,
		clips:      clips,
		lineIDs:    ids,
		spoken:     map[string]passStamp{},
		ranked:     map[string]bool{},
		passCnt:    make([]int, len(clips)),
		passStates: make(map[passStamp]*passState),
		occPass:    make(map[string]passStamp),
		passBound:  pendingPassBound(minClipSpanS(clips)),
		pipeDone:   make(chan struct{}),
	}
	return h, nil
}

// loadClips loads every clip's truth and its drop mask (chaos only).
func loadClips(cfg Config) ([]clip, error) {
	out := make([]clip, 0, len(cfg.Clips))
	for _, dir := range cfg.Clips {
		full := filepath.Join(cfg.RepoRoot, dir)
		truth, err := scenario.LoadTruth(full)
		if err != nil {
			return nil, fmt.Errorf("soak: clip %s: %v", dir, err)
		}
		c := clip{dir: dir, truth: truth, first: truth[0].TsNS}
		step := int64(100_000_000)
		if len(truth) >= 2 && truth[1].TsNS > truth[0].TsNS {
			step = truth[1].TsNS - truth[0].TsNS
		}
		c.stride = step + truth[len(truth)-1].TsNS - truth[0].TsNS
		if cfg.Chaos.Enabled && cfg.Chaos.DropFramesPerPass > 0 {
			c.dropMask = dropMaskFor(cfg.Chaos.Seed, len(truth), cfg.Chaos.DropFramesPerPass)
		}
		out = append(out, c)
	}
	return out, nil
}

// build wires the gateways, the clients, the pipeline and the sidecar.
// The hub's log is a bounded ring (not the unbounded MemStore) so the soak's
// own event log cannot be the OOM the run is meant to rule out.
func (h *harness) build() error {
	h.mem = newRingStore(8192)
	h.hub = events.New(events.WithStore(h.mem))
	rep := chaosDialer{rng: h.chaos, report: h.recordFault}
	h.decGW = newDecisionGW(soakDecisionModel, &h.cfg.Chaos, rep)
	h.ttsGW = newTTSGW(soakTTSModel, soakVoiceID, &h.cfg.Chaos, rep)

	dec, err := systemone.New(h.decGW.url(), "soak-token", soakDecisionModel)
	if err != nil {
		return fmt.Errorf("soak: decision client: %v", err)
	}
	h.dec = dec
	ttsClient, err := tts.New(h.ttsGW.url(), "soak-token", soakTTSModel)
	if err != nil {
		return fmt.Errorf("soak: tts client: %v", err)
	}
	h.tts = ttsClient

	idx, err := matcher.Build(h.profile.ToMessages(), h.profile.ToEdges(), matcher.IndexOptions{})
	if err != nil {
		return fmt.Errorf("soak: matcher.Build: %v", err)
	}
	h.idx = idx

	// One truth engine answers every read: the clips share the profile, so the
	// union of their truth rows is the hash->text map.
	rows := make([]frameTruth, 0, 64)
	for _, c := range h.clips {
		for _, t := range c.truth {
			rows = append(rows, frameTruth{dir: c.dir, file: t.File, text: t.Text, full: t.Full})
		}
	}
	eng, err := newTruthEngine(h.cfg.RepoRoot, h.profile, rows)
	if err != nil {
		return err
	}
	h.engine = eng

	cfg := pipeline.Config{
		SessionID:       soakSessionID,
		ProfileRevision: soakProfileRev,
		Regions:         h.profile.ToRegions(),
		OCR:             h.engine,
		Index:           idx,
		Preprocess:      vision.DefaultSteps(),
		Trace:           false,
		Mode:            pipeline.ModeReplay,
		Tracker:         recognition.Config{VoteWindow: 3, StableWindow: 100 * time.Millisecond},
	}
	runner, err := pipeline.New(cfg)
	if err != nil {
		return fmt.Errorf("soak: pipeline.New: %v", err)
	}
	h.runner = runner
	h.src = newCyclingSource(h.cfg.RepoRoot, h.clips, h.cfg.Speed)

	pub := &sidecarPub{hub: h.hub, h: h}
	sess, err := session.New(soakSessionID, soakProfileRev, pub,
		session.WithTrackerConfig(recognition.Config{VoteWindow: 3, StableWindow: 100 * time.Millisecond}))
	if err != nil {
		return fmt.Errorf("soak: sidecar session: %v", err)
	}
	h.sess = sess
	h.pub = pub
	return nil
}

// start launches layer A (the pipeline over the cycling frames) and returns.
// The pipeline's Run starts the source itself: starting it here too would trip
// the source's once-only Start contract.
func (h *harness) start(ctx context.Context) error {
	go func() {
		defer close(h.pipeDone)
		defer func() {
			if p := recover(); p != nil {
				h.panics.Add(1)
				h.pipeErr = fmt.Errorf("pipeline panic: %v", p)
			}
		}()
		if err := h.runner.Run(ctx, h.src, h.hub); err != nil && ctx.Err() == nil {
			h.pipeErr = err
		}
	}()
	return nil
}

// sidecar is layer B: the paced truth feed and the provider work it
// triggers, for the whole duration.
func (h *harness) sidecar(ctx context.Context) error {
	defer func() {
		if p := recover(); p != nil {
			h.panics.Add(1)
		}
	}()
	for {
		if ctx.Err() != nil {
			return nil
		}
		for ci, c := range h.clips {
			if ctx.Err() != nil {
				return nil
			}
			pass := h.passCnt[ci]
			// Each pass is a fresh replay of the clip: the first row is
			// due now, the rest on the clip's capture clock (the scenario
			// paces a run the same way).
			if err := h.sidecarPass(ctx, ci, c, pass, time.Now()); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			h.passCnt[ci] = pass + 1
			// Pass boundary: the pass stops being fed (its slice is now
			// gradeable) — grade and drop every settled slice and hold the
			// pending set to its bound (settlePasses). Every commit event of
			// the pass was filed while it was being fed, so a settled slice is
			// complete when it settles.
			h.passMu.Lock()
			h.closePassLocked(passStamp{clip: ci, pass: pass})
			h.passMu.Unlock()
			h.settlePasses()
			h.rankedMu.Lock()
			h.ranked = map[string]bool{}
			h.rankedMu.Unlock()
		}
	}
}

// sidecarPass feeds one pass of one clip on the capture clock.
func (h *harness) sidecarPass(ctx context.Context, ci int, c clip, pass int, anchor time.Time) error {
	stamp := passStamp{clip: ci, pass: pass}
	for i, t := range c.truth {
		if c.dropMask != nil && c.dropMask[i] {
			continue
		}
		// The capture timestamp carries the loop offset (the monotonic source
		// clock the session and the pipeline see across passes). The wall
		// target paces the in-clip offset only — the anchor resets per pass, so
		// a pass*stride term in the target would be dead sleep and the pass
		// wall time would grow pass by pass (the sidecar would do ~21 passes of
		// a 2.3 s clip in 10 minutes instead of ~260: ADR-0045).
		ts := t.TsNS + int64(pass)*c.stride
		offset := t.TsNS - c.first
		target := anchor.Add(time.Duration(offset/int64(time.Millisecond)) * time.Millisecond)
		if d := time.Until(target); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := h.feedRow(ctx, c, ts, t, stamp); err != nil {
			return fmt.Errorf("soak: clip %d pass %d row %d: %v", ci, pass, i, err)
		}
	}
	return nil
}

// feedRow applies one truth row through the sidecar session and, when the
// region is ready with a new reading, the ranking and the commit work.
func (h *harness) feedRow(ctx context.Context, c clip, tsNS int64, t scenario.Truth, stamp passStamp) error {
	o := truthResult(t.Text, t.Full)
	if _, err := h.sess.Apply(ctx, session.ObservationMsg{
		Header:   h.header(h.sess.Generation()),
		RegionID: h.profile.Regions[0].ID,
		Observation: recognition.Observation{
			FrameSeq:           tsNS,
			CaptureTimestampNS: tsNS,
			SourceClock:        "replay",
			RegionID:           h.profile.Regions[0].ID,
			RegionPresent:      t.Present,
			RegionHash:         fmt.Sprintf("soak-%d", tsNS),
			OCR:                &o,
		},
	}); err != nil {
		return fmt.Errorf("observation: %w", err)
	}
	st := h.sess.State()
	if len(st.Regions) == 0 || st.Regions[0].TextState != recognition.StateReady || st.Regions[0].Text == "" {
		return nil
	}
	rs := st.Regions[0]
	key := fmt.Sprintf("%d:%s", stamp.clip, rs.NormalizedKey)
	h.rankedMu.Lock()
	if h.ranked[key] {
		h.rankedMu.Unlock()
		return nil // this reading was already answered this pass
	}
	h.ranked[key] = true
	h.rankedMu.Unlock()

	obsSeq := rs.ObservationSeq
	rank := h.idx.Rank(matcher.Observation{Text: rs.RawText, OCRConf: rs.Confidence, TextKindHint: "dialogue"}, matcher.RankOptions{})
	res, err := h.sess.Apply(ctx, session.RankingMsg{
		Header:         h.header(h.sess.Generation()),
		RegionID:       h.profile.Regions[0].ID,
		ObservationSeq: obsSeq,
		Text:           rs.Text,
		Ranking:        rank,
	})
	if err != nil {
		return fmt.Errorf("ranking: %w", err)
	}
	for _, e := range res.Events {
		if e.Name != events.MessageCommitted {
			continue
		}
		info, ok := h.parseCommit(e, obsSeq)
		if !ok {
			continue
		}
		h.sidecarCommits.Add(1)
		h.passMu.Lock()
		h.occPass[info.occurrence] = stamp
		h.passMu.Unlock()
		h.pub.addPassEvent(stamp, e)
		h.wg.Add(1)
		go h.handleCommit(ctx, info, stamp)
	}
	return nil
}

// header stamps one sidecar message (the scenario's msgHeader shape).
func (h *harness) header(gen int64) session.Header {
	return session.Header{
		SessionID:       soakSessionID,
		Seq:             h.seq.Add(1),
		Generation:      gen,
		ProfileRevision: soakProfileRev,
	}
}

// commitInfo is one sidecar commit reduced to what the provider layer needs.
type commitInfo struct {
	messageID  string
	text       string
	occurrence string
	generation int64
	region     string
	obsSeq     int64
	tsNS       int64
}

// committedPayload is the message.committed payload the sidecar reads (the
// scenario's committedPayload shape).
type committedPayload struct {
	MessageID            string              `json:"message_id"`
	Text                 string              `json:"text"`
	NormalizedText       string              `json:"normalized_text"`
	RegionID             string              `json:"region_id"`
	Generation           int64               `json:"generation"`
	DialogueOccurrenceID string              `json:"dialogue_occurrence_id"`
	Transitions          []transitionPayload `json:"transitions"`
}

type transitionPayload struct {
	To                 string `json:"to"`
	CaptureTimestampNS int64  `json:"capture_timestamp_ns"`
}

func (h *harness) parseCommit(e events.Envelope, obsSeq int64) (commitInfo, bool) {
	var p committedPayload
	if err := json.Unmarshal(e.Data, &p); err != nil {
		return commitInfo{}, false
	}
	var tsNS int64
	for _, tr := range p.Transitions {
		if tr.To == "committed" {
			tsNS = tr.CaptureTimestampNS
			break
		}
	}
	return commitInfo{
		messageID:  p.MessageID,
		text:       p.Text,
		occurrence: p.DialogueOccurrenceID,
		generation: p.Generation,
		region:     p.RegionID,
		obsSeq:     obsSeq,
		tsNS:       tsNS,
	}, true
}

// recordFault is the chaos reporters' sink: one faultEvent per activation,
// stamped with the wall time and the pipeline's commit count at the moment.
func (h *harness) recordFault(kind, role, detail string) {
	h.faultsFired.Add(1)
	counterInc(&h.faultKinds, kind)
	ev := faultEvent{Wall: time.Now(), Kind: kind, Role: role, Detail: detail}
	if h.runner != nil {
		ev.CommitsAtFire = h.runner.Stats().Commits
	}
	h.faultEventsMu.Lock()
	h.faultEvents = append(h.faultEvents, ev)
	h.faultEventsMu.Unlock()
}

// counterInc bumps the per-key atomic counter under the sync.Map.
func counterInc(m *sync.Map, key string) {
	v, _ := m.LoadOrStore(key, new(atomicInt64))
	v.(*atomicInt64).Add(1)
}

// counters is the sampler's view of the harness.
func (h *harness) counters() counters {
	var c counters
	c.SidecarCommits = h.sidecarCommits.Load()
	c.SpeechStarted = h.speechStarted.Load()
	c.SpeechEnded = h.speechEnded.Load()
	c.ProviderErrors = h.providerErrs.Load()
	c.FaultsFired = h.faultsFired.Load()
	c.Panics = h.panics.Load()
	return c
}

// counters is the harness's cumulative counters at one moment.
type counters struct {
	SidecarCommits int64 `json:"sidecar_commits"`
	SpeechStarted  int64 `json:"speech_started"`
	SpeechEnded    int64 `json:"speech_ended"`
	ProviderErrors int64 `json:"provider_errors"`
	FaultsFired    int64 `json:"faults_fired"`
	Panics         int64 `json:"recovered_panics"`
}

// close releases every resource the harness holds.
func (h *harness) close() {
	if h.decGW != nil {
		h.decGW.close()
	}
	if h.ttsGW != nil {
		h.ttsGW.close()
	}
	if h.src != nil {
		_ = h.src.Close()
	}
	if h.hub != nil {
		_ = h.hub.Close()
	}
}

var _ = replay.Source{} // the cycling source wraps replay sources
