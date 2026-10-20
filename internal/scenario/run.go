// run.go is the M7 scenario harness: one scenario YAML runs end-to-end as
// two layers over the synthetic clip.
//
//	Layer A (recognition): the real pipeline in ModeReplay — real frame
//	decoding, presence detection, the region tracker, the matcher and the
//	session reducer — with the OCR engine answered from the clip's truth
//	(docs/spec/11 "Fixture origins"; the OCR-quality gate for this corpus
//	remains the M2 real-Tesseract replay test).
//
//	Layer B (providers): the decision and TTS layers. A sidecar session
//	reducer (the same internal/session component layer A runs, with the same
//	profile revision and tracker settings) is paced on the clip's capture
//	clock and fed the clip's truth, so its commits land at the same capture
//	times as the pipeline's. For every commit the harness issues a real
//	systemone.Decide against the fake (fault-injecting) decision gateway and,
//	when the decision is applied, a real tts.Generate against the fake TTS
//	gateway; the answers come back through the real clients' wire parsing
//	and error taxonomy, and the session's generation fence decides whether
//	a late or duplicated answer is applied.
//
// The two layers publish into one event hub, so the assertions evaluate one
// merged timeline (docs/spec/08). The sidecar's own message.committed events
// are not forwarded: the pipeline's commits are the commitment of record and
// merging the sidecar's would double-count every line (the M2 sidecar made
// the same cut for its speech events).
//
// A scenario that cannot be run honestly carries status: blocked in its YAML
// with a reason; the harness reports it as BLOCKED, never as a pass (ADR-0037
// "blocked ≠ pass").

package scenario

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/replay"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/filesink"
	"github.com/dbirks/game-narrator/internal/vision"
)

// StatusBlocked is the only status value a scenario YAML may carry besides
// the implicit "runnable". A blocked scenario is reported, not passed.
const StatusBlocked = "blocked"

// StatusNotRun is the runner's verdict for a scenario whose recorded input is
// not available on this host (a dolphin fixture that was never recorded, or
// whose manifest hash no longer matches — the fixture was re-recorded). The
// gate convention: NOT RUN with setup instructions, never a pass (ADR-0037
// "blocked ≠ pass"; the same honesty applies to a recording that is absent).
const StatusNotRun = "not_run"

// eventNameFaultInjected is the scenario-timeline event that records a fault
// activation. It is registered here, not in internal/events, because the
// name belongs to the scenario harness that owns the faults: the events
// package documents Register as the extension point for exactly this.
const eventNameFaultInjected = "fault.injected"

func init() {
	if err := events.Register(eventNameFaultInjected); err != nil {
		panic(fmt.Sprintf("scenario: register %s: %v", eventNameFaultInjected, err))
	}
}

// Shared identity for both layers: one merged timeline, one session
// (docs/spec/08 event correlation).
const (
	sessionID       = "synthsession"
	profileRevision = "1"
	decisionModel   = "clef-fake"
	ttsModel        = "tts-fake"
	questionLineID  = "line_confirm"
	choiceConfirmed = "confirmed"
	choiceUnknown   = "unknown"
	// providerTimeout bounds one provider call. A faulted endpoint is
	// expected to be slow; a call that outlives the bound is a timeout and
	// an honest abstain, the way docs/spec/06's decision budget behaves.
	providerTimeout = 10 * time.Second
)

// RunResult is the harness's verdict for one scenario.
type RunResult struct {
	ID      string
	Catalog string // the docs/test-plan/scenario-catalog.md row, when labelled
	Status  string // "passed" | "failed" | "blocked" | "not_run"
	Reason  string // blocked scenarios: the YAML's reason; not_run: the setup instructions
	Report  Report // empty for blocked and not_run scenarios
	Stats   pipeline.Stats
	// SessionStats is the sidecar session's self-report: the fences the
	// decision/TTS layer ran against (stale, duplicated, rejected counts).
	SessionStats session.Stats
	Faults       []Fault
	// Timeline is the merged event timeline, oldest first (report material:
	// the on-disk HTML report correlates its events against the frames and
	// the audio). Nil until the run's events are read.
	Timeline []events.Envelope
	// AudioByOccurrence maps a dialogue occurrence to the synthesized WAV
	// bytes the TTS layer produced for it (empty when the scenario runs
	// without TTS or nothing was spoken).
	AudioByOccurrence map[string][]byte
	// AudioDir is the per-run filesink directory of the harness's real
	// playback half (TTS scenarios): one evidence WAV per started playback,
	// the delivered prefix when a stop landed mid-playback. Empty when the
	// scenario runs without TTS.
	AudioDir string
	// SchedulerStats is the harness scheduler's decision counters (TTS
	// scenarios): drops by reason are the timeline-side proof that a stale
	// or cancelled utterance never reached the sink.
	SchedulerStats speech.Stats
	// Fixture is the provenance for dolphin-origin scenarios: the fixture the
	// frames came from and the slice the scenario replayed (nil otherwise).
	Fixture *DolphinFixtureInfo
}

// Summary is the one-line report the runner test prints per scenario. It
// carries the catalog label and the fault_injected kinds so a report reader
// (or a grep) can trace the line back to the catalog row and the faults
// that fired on the timeline.
func (r RunResult) Summary() string {
	label := r.ID
	if r.Catalog != "" {
		label = fmt.Sprintf("%s [%s]", r.ID, r.Catalog)
	}
	switch r.Status {
	case "blocked":
		return fmt.Sprintf("BLOCKED: %s: %s", label, r.Reason)
	case "not_run":
		return fmt.Sprintf("NOT RUN: %s: %s", label, r.Reason)
	case "passed":
		return fmt.Sprintf("PASS: %s (%d assertions, fault_injected: %s)", label, len(r.Report.Results), faultKinds(r.Faults))
	default:
		return fmt.Sprintf("FAIL: %s: %s", label, failedDetails(r))
	}
}

// faultKinds reduces the faults to "kind×count" for the summary line; an
// empty list reports as "none".
func faultKinds(faults []Fault) string {
	if len(faults) == 0 {
		return "none"
	}
	counts := map[string]int{}
	var order []string
	for _, f := range faults {
		if counts[f.Type] == 0 {
			order = append(order, f.Type)
		}
		counts[f.Type]++
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%s×%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

func failedDetails(r RunResult) string {
	var bad []string
	for _, res := range r.Report.Results {
		if !res.Passed {
			bad = append(bad, res.Type+": "+res.Detail)
		}
	}
	if len(bad) == 0 {
		return "run error"
	}
	return strings.Join(bad, "; ")
}

// RunScenario runs one scenario end-to-end and evaluates its assertions.
// repoRoot is the repository root: scenario input paths are relative to it.
// The synthetic and captured/hardware shapes read their frames from the
// checkout; dolphin-origin scenarios go through RunDolphinScenario (their
// frames live in the external fixture dir and their profile ships with the
// fixture).
func RunScenario(ctx context.Context, repoRoot string, sc *Scenario) (RunResult, error) {
	if sc.Status == StatusBlocked {
		return RunResult{ID: sc.ID, Catalog: sc.Catalog, Status: "blocked", Reason: sc.Reason, Faults: sc.Faults}, nil
	}
	if sc.Inference.TTS != InferenceNone && sc.Inference.Voice == "" {
		return RunResult{}, fmt.Errorf("scenario %s: inference.tts is %q but inference.voice is empty", sc.ID, sc.Inference.TTS)
	}

	framesDir := filepath.Join(repoRoot, sc.Input.FramesDir)
	profilePath := filepath.Join(repoRoot, "testdata", "synthetic", "profiles", sc.Profile+".json")
	return runScenarioCore(ctx, repoRoot, sc, framesDir, profilePath)
}

// runScenarioCore is the shared execution: profile + truth + matcher from the
// named paths, both harness layers over the frames in framesDir, and the
// assertions against the merged timeline.
func runScenarioCore(ctx context.Context, repoRoot string, sc *Scenario, framesDir, profilePath string) (RunResult, error) {
	profile, err := LoadProfile(profilePath)
	if err != nil {
		return RunResult{}, fmt.Errorf("scenario %s: %w", sc.ID, err)
	}
	truth, err := LoadTruth(framesDir)
	if err != nil {
		return RunResult{}, fmt.Errorf("scenario %s: %w", sc.ID, err)
	}
	idx, err := matcher.Build(profile.ToMessages(), profile.ToEdges(), matcher.IndexOptions{})
	if err != nil {
		return RunResult{}, fmt.Errorf("scenario %s: matcher.Build: %v", sc.ID, err)
	}

	// The keep mask the drop_frames fault applies: both layers must see the
	// same frames, or the sidecar's commits would outrun the pipeline's.
	drop, _ := findFault(sc.Faults, FaultDropFrames)

	run := newRun(sc, repoRoot, framesDir, profile, truth, idx)
	run.kept = keepMask(truth, drop)
	defer run.close()

	if err := run.run(ctx); err != nil {
		return RunResult{}, fmt.Errorf("scenario %s: run: %v", sc.ID, err)
	}

	evs, err := run.mem.After(ctx, 0, events.Filter{}, 8192)
	if err != nil {
		return RunResult{}, fmt.Errorf("scenario %s: read events: %v", sc.ID, err)
	}
	// The harness grades the merged timeline declaring its own OCR engine:
	// layer A reads through the truth stub (truthocr.go), so an assertion
	// scoped to the real engine (engines: tesseract) is a property of a
	// different run and is reported not-applicable, never graded here. The
	// corpus is the profile's own: message_detected anchors on it, engine-
	// free, exactly as the real-engine gate does.
	report := sc.EvaluateWith(evs, run.pipelineStats, run.truth, EvaluateOpts{OCREngine: truthEngineName, Corpus: run.profile.CorpusFold()})
	// The implicit check against the playback half (TTS scenarios): the
	// sink's directory carries exactly one evidence WAV per started
	// utterance and no temporary file. It is appended like
	// replay_not_degraded, not authorable.
	if res, ok := run.audioEvidenceCheck(); ok {
		report.Results = append(report.Results, res)
		if !res.Passed {
			report.Passed = false
		}
	}
	status := "passed"
	if !report.Passed {
		status = "failed"
	}
	return RunResult{
		ID:                sc.ID,
		Catalog:           sc.Catalog,
		Status:            status,
		Report:            report,
		Stats:             run.pipelineStats,
		SessionStats:      run.session.Stats(),
		Faults:            sc.Faults,
		Timeline:          evs,
		AudioByOccurrence: run.audio,
		AudioDir:          run.audioDir,
		SchedulerStats:    run.schedulerStats(),
	}, nil
}

// keepMask reports which truth rows survive the drop_frames window.
func keepMask(truth []Truth, drop Fault) []int {
	out := make([]int, 0, len(truth))
	for i, row := range truth {
		if drop.Type == FaultDropFrames && inFaultWindow(drop.FromMS, drop.ToMS, int(row.TsNS/1e6)) {
			continue
		}
		out = append(out, i)
	}
	return out
}

// findFault returns the first fault of kind and its index, or a zero fault
// and -1.
func findFault(faults []Fault, kind string) (Fault, int) {
	for i, f := range faults {
		if f.Type == kind {
			return f, i
		}
	}
	return Fault{}, -1
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
	tsMS       int64
	// speechEligible is the session's own verdict on the commit (its
	// commitment policy: the narrated-class veto, the abstain, the decision's
	// safe_to_speak veto — whatever the session decided). The harness speaks
	// a commit only when the session said it may, the way the runtime's
	// speech layer reads the same flag: a commit the session marked not
	// eligible is silence with a reason, not a guess.
	speechEligible bool
}

// run is one scenario execution: its resources, its two layers and the
// shared event hub.
type run struct {
	sc        *Scenario
	repoRoot  string
	framesDir string
	profile   *Profile
	truth     []Truth
	kept      []int
	idx       *matcher.Index
	region    string
	voice     string
	// narratedKinds is the scenario's narrated-class setting (docs/spec/09
	// "/play"): the operator's election both session layers run with. The
	// pipeline's commitment policy and the sidecar's veto agree on which text
	// classes the session speaks, so a sign committed in a dialogue-alone
	// session is silent in the timeline exactly as it would be live.
	narratedKinds []games.TextKind
	// kindHint is the primary region's text-kind hint (its on-screen role's
	// class, the runner's fallback when the role names none): the sidecar's
	// local retrieval scores the text-kind component with the same per-region
	// value the pipeline's rank worker uses, so the two layers rank alike.
	kindHint string

	injector *faultInjector
	// fault entry indices (-1 when the scenario carries no such fault)
	idxDrop, idxDown, idxDelay, idxVoice, idxDup int

	// shared event stream
	mem *events.MemStore
	hub *events.Hub

	// layer A
	engine        *truthEngine
	runner        *pipeline.Runner
	src           *replay.Source
	dropSrc       *DropFrameSource
	pipelineStats pipeline.Stats

	// layer B
	session   *session.Reducer
	decClient *systemone.Client
	ttsWrap   *faultedTTS
	gwDec     *decisionGateway
	gwTTS     *ttsGateway
	// the playback half (TTS scenarios): the real scheduler over the
	// filesink under a per-run temp dir — the audio evidence the assertions
	// and report read (one WAV per started playback, the delivered prefix
	// when a stop landed mid-playback).
	sched    *speech.Scheduler
	sink     *filesink.Sink
	audioDir string
	// pipelineErr is the pipeline layer's error, captured from its goroutine.
	pipelineErr error

	// sidecar state (guarded by mu)
	mu       sync.Mutex
	seq      int64
	spoken   map[string]bool   // occurrence already sent to TTS
	started  map[string]int64  // occurrence → generation of the live utterance
	degraded map[string]bool   // role → degraded state last seen on the wire
	audio    map[string][]byte // occurrence → the synthesized WAV (report material)
	decWG    sync.WaitGroup
}

// scenarioNarratedKinds renders the scenario's narrated classes as the
// session stores them: the YAML's names (already validated by Load against
// the content-class vocabulary) as games.TextKind. An empty list is the
// documented default — dialogue alone — which the session's own normalizer
// applies, so the harness never hardcodes the default and drifts from it.
func scenarioNarratedKinds(sc *Scenario) []games.TextKind {
	if len(sc.NarratedKinds) == 0 {
		return nil
	}
	out := make([]games.TextKind, 0, len(sc.NarratedKinds))
	for _, k := range sc.NarratedKinds {
		out = append(out, games.TextKind(k))
	}
	return out
}

// regionKindHint is the primary region's text-kind hint: its on-screen role's
// class when the role names one (sign → sign, item → item), the runner's
// dialogue fallback otherwise — the same rule the pipeline's rank worker
// applies (games.RegionType.TextKindHint, docs/spec/04 "Visual profiles"), so
// the sidecar's local retrieval and the pipeline's retrieval score the
// text-kind component alike.
func regionKindHint(profile *Profile) string {
	if k, ok := games.RegionType(profile.Regions[0].Type).TextKindHint(); ok {
		return string(k)
	}
	return "dialogue"
}

func newRun(sc *Scenario, repoRoot, framesDir string, profile *Profile, truth []Truth, idx *matcher.Index) *run {
	return &run{
		sc:            sc,
		repoRoot:      repoRoot,
		framesDir:     framesDir,
		profile:       profile,
		truth:         truth,
		idx:           idx,
		region:        profile.Regions[0].ID,
		voice:         sc.Inference.Voice,
		narratedKinds: scenarioNarratedKinds(sc),
		kindHint:      regionKindHint(profile),
		idxDrop:       -1,
		idxDown:       -1,
		idxDelay:      -1,
		idxVoice:      -1,
		idxDup:        -1,
		spoken:        map[string]bool{},
		started:       map[string]int64{},
		degraded:      map[string]bool{},
		audio:         map[string][]byte{},
	}
}

// buildEvents sets up the shared hub and the fault injector that publishes
// into it.
func (r *run) buildEvents() {
	r.mem = events.NewMemStore()
	r.hub = events.New(events.WithStore(r.mem))
	r.injector = newFaultInjector(len(r.sc.Faults), r.publishFault)
}

// buildGateways starts the fake provider endpoints with the fault windows
// and the real clients that talk to them.
func (r *run) buildGateways() {
	var decDownFrom, decDownTo, decDelayAfter, decDelay int
	if f, i := findFault(r.sc.Faults, FaultEndpointDown); i >= 0 && f.Role == RoleDecision {
		decDownFrom, decDownTo, r.idxDown = f.FromMS, f.ToMS, i
	}
	if f, i := findFault(r.sc.Faults, FaultDecisionDelay); i >= 0 {
		decDelayAfter, decDelay, r.idxDelay = f.AfterMS, f.DelayMS, i
	}
	r.gwDec = newDecisionGateway(decisionModel, r.injector, decDownFrom, decDownTo, r.idxDown, decDelayAfter, decDelay, r.idxDelay)

	dec, err := systemone.New(r.gwDec.url(), "scenario-token", decisionModel)
	if err != nil {
		panic(fmt.Sprintf("scenario: decision client: %v", err))
	}
	r.decClient = dec

	voiceMissing := ""
	if f, i := findFault(r.sc.Faults, FaultVoiceMissingReplica); i >= 0 {
		voiceMissing = f.Voice
		r.idxVoice = i
	}
	ttsDownFrom, ttsDownTo := 0, 0
	if f, i := findFault(r.sc.Faults, FaultEndpointDown); i >= 0 && f.Role == RoleTTS {
		ttsDownFrom, ttsDownTo = f.FromMS, f.ToMS
	}
	r.gwTTS = newTTSGateway(ttsModel, voiceMissing, r.injector, r.idxVoice, ttsDownFrom, ttsDownTo)
	ttsClient, err := tts.New(r.gwTTS.url(), "scenario-token", ttsModel)
	if err != nil {
		panic(fmt.Sprintf("scenario: tts client: %v", err))
	}
	if f, i := findFault(r.sc.Faults, FaultEndpointDown); i >= 0 && f.Role == RoleTTS {
		r.ttsWrap = &faultedTTS{client: ttsClient, gw: r.gwTTS, fromMS: f.FromMS, toMS: f.ToMS, injector: r.injector, idx: i}
	} else {
		r.ttsWrap = &faultedTTS{client: ttsClient, gw: r.gwTTS, fromMS: 0, toMS: 0, injector: r.injector, idx: -1}
	}
}

// buildAudioPipeline starts the run's playback half (TTS scenarios only):
// the real generation-fenced scheduler over a filesink under a per-run temp
// dir. The filesink writes one evidence WAV per started playback (the
// delivered prefix when a stop lands mid-playback), which is what makes
// "no stale audio starts" and "no temporary file left behind" assertable
// against the sink's own directory rather than the event log alone.
// The scheduler's publisher is a discarding stub: the sidecar session is
// the event recorder of record for the merged timeline, and duplicating
// the scheduler's playback events would double-count every start.
func (r *run) buildAudioPipeline(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "gn-scenario-audio-*")
	if err != nil {
		return fmt.Errorf("audio dir: %w", err)
	}
	r.audioDir = dir
	sink, err := filesink.New(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("filesink: %w", err)
	}
	r.sink = sink
	r.sched = speech.NewScheduler(sink, r, discardingPub{}, speech.Options{})
	r.sched.Start(ctx)
	return nil
}

// IsCurrent is the harness's playback fence: the session's commit
// generation is the only generation the scheduler may start (the same rule
// the runtime's fence applies, docs/spec/01 invariant 2).
func (r *run) IsCurrent(g int64) bool { return r.session.Generation() == g }

// discardingPub swallows the scheduler's playback events: the sidecar
// session is the publisher of record for the merged timeline.
type discardingPub struct{}

func (discardingPub) Publish(context.Context, events.Envelope) (events.Envelope, error) {
	return events.Envelope{}, nil
}

// advanceFence mirrors the runtime's fenceAndCancel at the harness level:
// every commit cancels every older generation still queued or playing, so
// a commit that lands while an older utterance is synthesising or playing
// stops that audio at the source (docs/spec/12 "zero stale audio starts").
func (r *run) advanceFence(generation int64) {
	r.mu.Lock()
	var stale []int64
	for _, gen := range r.started {
		if gen < generation {
			stale = append(stale, gen)
		}
	}
	r.mu.Unlock()
	if r.sched == nil {
		return
	}
	for _, g := range stale {
		r.sched.Cancel(g, "new_commit")
	}
}

// run drives both layers and waits for the provider work to drain.
func (r *run) run(ctx context.Context) error {
	r.buildEvents()
	r.buildGateways()
	if r.sc.Inference.TTS != InferenceNone {
		if err := r.buildAudioPipeline(ctx); err != nil {
			return err
		}
	}

	// --- layer A: the pipeline over the frames -----------------------------
	engine, err := newTruthEngine(r.framesDir, r.profile, r.truth)
	if err != nil {
		return err
	}
	r.engine = engine

	src := &replay.Source{Dir: r.framesDir, Speed: 0}
	r.src = src
	var frameSrc capture.Source = src
	if f, i := findFault(r.sc.Faults, FaultDropFrames); i >= 0 {
		r.idxDrop = i
		r.dropSrc = &DropFrameSource{
			inner:      src,
			FromNS:     int64(f.FromMS) * int64(time.Millisecond),
			ToNS:       int64(f.ToMS) * int64(time.Millisecond),
			FaultIndex: i,
			Injector:   r.injector,
			Detail:     fmt.Sprintf("frames withheld in capture window %d..%d ms", f.FromMS, f.ToMS),
		}
		frameSrc = r.dropSrc
	}

	cfg := pipeline.Config{
		SessionID:       sessionID,
		ProfileRevision: profileRevision,
		Regions:         r.profile.ToRegions(),
		OCR:             engine,
		Index:           r.idx,
		Preprocess:      vision.DefaultSteps(),
		Trace:           true,
		Mode:            pipeline.ModeReplay,
		// The narrated-class setting the scenario declares (docs/spec/09
		// "/play"): the pipeline's commitment policy applies the veto, so a
		// sign or item that commits in a session that did not elect to hear
		// its class is speech-ineligible in the pipeline's own commit events
		// — the record of commitment the report grades.
		Reducer: session.Config{NarratedKinds: r.narratedKinds},
		Tracker: recognition.Config{VoteWindow: 3, StableWindow: 100 * time.Millisecond},
	}
	runner, err := pipeline.New(cfg)
	if err != nil {
		return fmt.Errorf("pipeline.New: %v", err)
	}
	r.runner = runner

	// --- layer B: the provider sidecar -------------------------------------
	// The sidecar's commitment policy runs the scenario's narrated classes
	// (the same set the pipeline layer ran) and names a committed message's
	// content class off the corpus (the matcher index's KindFor, the wiring
	// the pipeline's runner uses): with both, the narrated-class veto is live
	// in the sidecar's commits too, and a sign that the session did not elect
	// to hear carries speech_eligible false — which the harness's speak gate
	// reads, the way the runtime's speech layer reads the same flag.
	pub := &sidecarPub{forward: speechEventNames(), hub: r.hub}
	sess, err := session.New(sessionID, profileRevision, pub,
		session.WithConfig(session.Config{NarratedKinds: r.narratedKinds}),
		session.WithMessageKind(r.idx.KindFor),
		session.WithTrackerConfig(recognition.Config{VoteWindow: 3, StableWindow: 100 * time.Millisecond}))
	if err != nil {
		return fmt.Errorf("session.New (sidecar): %v", err)
	}
	r.session = sess

	var pipeWG sync.WaitGroup
	pipeWG.Add(1)
	go func() {
		defer pipeWG.Done()
		if err := r.runner.Run(ctx, frameSrc, r.hub); err != nil {
			r.pipelineErr = err
		}
	}()
	if err := r.runSidecar(ctx); err != nil {
		pipeWG.Wait()
		return err
	}
	pipeWG.Wait()
	if r.pipelineErr != nil {
		return fmt.Errorf("pipeline: %v", r.pipelineErr)
	}

	// Drain the provider work (delayed decisions, in-flight TTS), then close
	// the utterances that are still live: the session's fences decide which
	// of those ends are real and which are stale.
	drainCtx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.decWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-drainCtx.Done():
		return fmt.Errorf("scenario %s: provider work did not drain within %s", r.sc.ID, providerTimeout)
	}
	r.finishUtterances(ctx)
	r.closeAudio()

	r.pipelineStats = r.runner.Stats()
	return nil
}

// closeAudio stops the playback half: the scheduler stops any active
// playback (the filesink writes the delivered prefix as the evidence WAV)
// and drains its loop, bounded. The sink itself is closed by close(); the
// directory is kept as the run's audio evidence.
func (r *run) closeAudio() {
	if r.sched != nil {
		_ = r.sched.Stop()
	}
}

// schedulerStats copies the scheduler's decision counters (nil-safe).
func (r *run) schedulerStats() speech.Stats {
	if r.sched == nil {
		return speech.Stats{}
	}
	return r.sched.Stats()
}

// audioEvidenceCheck is the playback-half half of "no stale audio": the
// sink's directory must carry exactly one evidence WAV per started
// utterance — a file for an utterance that never started is stale audio
// that reached the sink, a missing file for a started one is lost evidence
// — and no temporary file may remain (the filesink stages each evidence
// WAV as a .tmp file and renames it on finish; a leftover .tmp is an
// unfinished or abandoned write).
func (r *run) audioEvidenceCheck() (Result, bool) {
	if r.audioDir == "" {
		return Result{}, false
	}
	r.mu.Lock()
	started := make(map[string]int64, len(r.started))
	for occ, gen := range r.started {
		started[occ] = gen
	}
	r.mu.Unlock()

	entries, err := os.ReadDir(r.audioDir)
	if err != nil {
		return Result{Type: "audio_evidence_clean", Passed: false, Detail: fmt.Sprintf("read sink dir: %v", err)}, true
	}
	var offenders []string
	files := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			offenders = append(offenders, fmt.Sprintf("temporary file %s left in the sink dir", name))
			continue
		}
		if !strings.HasSuffix(name, ".wav") {
			continue
		}
		files++
	}
	if len(offenders) == 0 && files != len(started) {
		offenders = append(offenders, fmt.Sprintf("%d evidence WAV(s) in the sink dir for %d started utterance(s): every started utterance must leave exactly one, and no other utterance may leave one", files, len(started)))
	}
	detail := fmt.Sprintf("sink dir %s: %d evidence WAV(s) for %d started utterance(s), no temporary file", r.audioDir, files, len(started))
	if len(offenders) > 0 {
		return Result{Type: "audio_evidence_clean", Passed: false, Detail: strings.Join(offenders, "; ")}, true
	}
	return Result{Type: "audio_evidence_clean", Passed: true, Detail: detail}, true
}

// runSidecar is layer B: the paced truth feed plus the decision/TTS work it
// triggers. The feed paces the capture clock so the commit wall times mirror
// the clip: a decision delayed by 900 ms on the wall clock is a decision
// delayed by 900 ms on the recording.
func (r *run) runSidecar(ctx context.Context) error {
	base := r.truth[r.kept[0]].TsNS
	start := time.Now()
	lastKey := ""
	for _, i := range r.kept {
		row := r.truth[i]
		target := start.Add(time.Duration((row.TsNS-base)/int64(time.Millisecond)) * time.Millisecond)
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

		res, err := func() (session.Result, error) {
			o := truthOCR(row.Text, row.Full)
			return r.session.Apply(ctx, session.ObservationMsg{
				Header:   r.msgHeader(r.session.Generation()),
				RegionID: r.region,
				Observation: recognition.Observation{
					FrameSeq:           int64(i + 1),
					CaptureTimestampNS: row.TsNS,
					SourceClock:        "replay",
					RegionID:           r.region,
					RegionPresent:      row.Present,
					RegionHash:         fmt.Sprintf("truth-%03d", i+1),
					OCR:                &o,
				},
			})
		}()
		if err != nil {
			return fmt.Errorf("sidecar observation %d: %v", i+1, err)
		}
		st := r.session.State()
		if len(st.Regions) == 0 || st.Regions[0].TextState != recognition.StateReady || st.Regions[0].Text == "" {
			continue
		}
		rs := st.Regions[0]
		if rs.NormalizedKey == lastKey {
			continue // this exact reading was already answered
		}
		lastKey = rs.NormalizedKey
		obsSeq := rs.ObservationSeq
		rank := r.idx.Rank(matcher.Observation{Text: rs.RawText, OCRConf: rs.Confidence, TextKindHint: r.kindHint}, matcher.RankOptions{})
		res, err = r.session.Apply(ctx, session.RankingMsg{
			Header:         r.msgHeader(r.session.Generation()),
			RegionID:       r.region,
			ObservationSeq: obsSeq,
			Text:           rs.Text,
			Ranking:        rank,
		})
		if err != nil {
			return fmt.Errorf("sidecar ranking %d: %v", i+1, err)
		}
		for _, e := range res.Events {
			if e.Name != events.MessageCommitted {
				continue
			}
			c, ok := r.parseCommit(e, obsSeq)
			if !ok {
				continue
			}
			r.decWG.Add(1)
			go r.handleCommit(ctx, c)
		}
	}
	return nil
}

// msgHeader stamps a sidecar message with the shared sequence and the
// generation the answer was asked under. The decision and speech messages
// carry the commit's generation, not the session's current one: a reply that
// lands after a newer commit must name the older generation so the session's
// fence can see it as stale and discard it.
func (r *run) msgHeader(gen int64) session.Header {
	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.mu.Unlock()
	return session.Header{
		SessionID:       sessionID,
		Seq:             seq,
		Generation:      gen,
		ProfileRevision: profileRevision,
	}
}

// parseCommit reduces a message.committed payload to the provider layer's
// view: the line, the occurrence, the generation believed active and the
// capture time of the commit (the transition into committed names the frame).
func (r *run) parseCommit(e events.Envelope, obsSeq int64) (commitInfo, bool) {
	var p committedPayload
	if err := json.Unmarshal(e.Data, &p); err != nil {
		return commitInfo{}, false
	}
	tsNS, ok := commitTimeNS(p)
	if !ok {
		return commitInfo{}, false
	}
	return commitInfo{
		messageID:      p.MessageID,
		text:           p.Text,
		occurrence:     p.DialogueOccurrenceID,
		generation:     p.Generation,
		region:         p.RegionID,
		obsSeq:         obsSeq,
		tsNS:           tsNS,
		tsMS:           tsNS / 1e6,
		speechEligible: p.SpeechEligible,
	}, true
}

// handleCommit is the decision/TTS work one commit triggers: ask the
// decision layer, apply the answer through the session's fences, and when
// the answer is applied, request the voice.
func (r *run) handleCommit(ctx context.Context, c commitInfo) {
	defer r.decWG.Done()
	// The fence advances on EVERY commit, the way the runtime's commit
	// watcher does: an older utterance still queued or playing is cancelled
	// at the source, not after it has started.
	r.advanceFence(c.generation)
	if r.sc.Inference.Decision == InferenceNone {
		// Local-chain-only mode: the matcher's commitment is the commitment,
		// and the voice is requested without a decision gate (the M2 shape) —
		// unless the session's commitment policy vetoed the utterance (the
		// line's class is not one the session elected to hear): a commit that
		// is not speech-eligible stays silent here, the way the runtime's
		// speech layer reads the same flag.
		if c.speechEligible {
			r.speak(ctx, c)
		}
		return
	}

	r.emit(ctx, events.DecisionRequested, map[string]any{
		"message_id":      c.messageID,
		"occurrence_id":   c.occurrence,
		"generation":      c.generation,
		"region_id":       c.region,
		"capture_ts_ns":   c.tsNS,
		"provider":        "systemone",
		"provider_origin": "recorded",
	})

	pctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	req := systemone.Request{
		Model: decisionModel,
		State: map[string]any{
			"capture_ts_ms":  c.tsMS,
			"confirm_choice": choiceConfirmed,
			"text":           c.text,
		},
		Questions: []systemone.Question{{
			ID:           questionLineID,
			Type:         systemone.TypeChoice,
			Instructions: "Confirm which profile line the textbox shows; choose exactly one.",
			Criteria: []systemone.Option{
				{ID: choiceConfirmed, Description: c.text},
				{ID: choiceUnknown, Description: "the text matches no profile line"},
			},
		}},
	}
	dec, err := r.decClient.Decide(pctx, req)
	if err != nil {
		kind := providerErrorKind(err)
		r.emit(ctx, events.DecisionResolved, r.decisionResolved(c, decisionOutcome{
			accepted: false,
			applied:  false,
			reason:   "provider_error",
			errKind:  kind,
		}))
		r.setDegraded(ctx, RoleDecision, true, kind)
		// Abstain: docs/spec/06 — on provider error the runtime proceeds
		// without Clef. The matcher's commitment stands, but a named voice
		// without the decision's evidence is silence, not a guess.
		return
	}
	r.setDegraded(ctx, RoleDecision, false, "recovered")

	choice := dec.Answers[questionLineID].Choice
	// The model may confirm a line, but the session's own verdict stands over
	// it: a commit the commitment policy marked not eligible (the narrated-
	// class veto, an abstain) is not spoken no matter what the model said.
	eligible := choice != choiceUnknown && c.speechEligible
	msgID := c.messageID
	if choice == choiceUnknown {
		msgID = session.UnknownMessage
	}

	// The answer goes through the session's fences with the generation it
	// was asked under: a reply that lands after a newer commit names a
	// generation the session has passed and is discarded, not applied.
	//
	// The observation seq is stamped at reply time, not at ask time: the
	// sidecar feeds one observation per frame, so a stable box keeps
	// advancing the region's seq, and stamping the ask-time seq would make a
	// healthy immediate reply race the sidecar's next observation for the
	// reducer's lock — a scheduling coin flip, and failure tests must not be
	// flaky (AGENTS.md). The staleness this fence owes the delayed-reply
	// scenario is the dialogue's, and the generation fence carries it:
	// between the ask and the reply, the commit that advanced the dialogue
	// bumped the generation.
	obsNow := c.obsSeq
	if st := r.session.State(); len(st.Regions) > 0 {
		obsNow = st.Regions[0].ObservationSeq
	}
	dm := session.DecisionMsg{
		Header:         r.msgHeader(c.generation),
		RegionID:       c.region,
		ObservationSeq: obsNow,
		MessageID:      msgID,
		SpeechEligible: eligible,
		Accepted:       true,
		Provider:       "systemone",
		Model:          dec.Model,
	}
	res, err := r.session.Apply(ctx, dm)
	outcome := decisionOutcome{
		accepted: true,
		applied:  err == nil && res.Applied,
		reason:   res.Reason,
		model:    dec.Model,
	}
	if err != nil {
		outcome.applied = false
		outcome.reason = err.Error()
	}

	// decision_duplicate: the same reply, same seq, delivered twice.
	if outcome.applied {
		if dup, i := findFault(r.sc.Faults, FaultDecisionDuplicate); i >= 0 && dup.Message == c.messageID {
			r.injector.fire(i, Fault{Type: FaultDecisionDuplicate, Message: c.messageID}, c.tsMS,
				fmt.Sprintf("decision reply for %s delivered twice (same seq)", c.messageID))
			res2, err2 := r.session.Apply(ctx, dm)
			r.emit(ctx, events.DecisionResolved, r.decisionResolved(c, decisionOutcome{
				accepted: true,
				applied:  err2 == nil && res2.Applied,
				reason:   res2.Reason,
				model:    dec.Model,
				delivery: 2,
			}))
		}
	}

	r.emit(ctx, events.DecisionResolved, r.decisionResolved(c, outcome))
	if eligible && outcome.applied {
		r.speak(ctx, c)
	}
}

// decisionOutcome is the payload of one decision.resolved event.
type decisionOutcome struct {
	accepted bool
	applied  bool
	reason   string
	errKind  string
	model    string
	delivery int
}

func (r *run) decisionResolved(c commitInfo, o decisionOutcome) map[string]any {
	payload := map[string]any{
		"message_id":    c.messageID,
		"occurrence_id": c.occurrence,
		"generation":    c.generation,
		"capture_ts_ns": c.tsNS,
		"accepted":      o.accepted,
		"applied":       o.applied,
		"reason":        o.reason,
		"provider":      "systemone",
	}
	if o.errKind != "" {
		payload["error"] = o.errKind
	}
	if o.model != "" {
		payload["model"] = o.model
	}
	if o.delivery > 1 {
		payload["delivery"] = o.delivery
	}
	return payload
}

// speak requests the voice for one committed occurrence, at most once per
// occurrence: the duplicate-delivery fault must not produce a second
// utterance, and the session's occurrence fence is the backstop.
func (r *run) speak(ctx context.Context, c commitInfo) {
	r.mu.Lock()
	if r.spoken[c.occurrence] {
		r.mu.Unlock()
		return
	}
	r.spoken[c.occurrence] = true
	r.mu.Unlock()

	if r.sc.Inference.TTS == InferenceNone {
		// No voice layer in this scenario: the M2 sidecar shape applies the
		// lifecycle directly, with the scenario's voice named.
		r.applySpeech(ctx, c, session.SpeechReady, "")
		return
	}

	pctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	r.ttsWrap.stampMS = c.tsMS
	audio, err := r.ttsWrap.GenerateStream(pctx, tts.GenerateRequest{Text: c.text, Voice: r.voice})
	if err != nil {
		// Silence with a reason: the failure carries the voice that was
		// requested, never a substitute. A mid-synthesis endpoint death is
		// the same terminal step: the utterance ends failed, and the phase
		// fence keeps it failed — a started is refused after a failed.
		r.applySpeech(ctx, c, session.SpeechFailed, providerErrorKind(err))
		return
	}
	// Report material: the actual synthesized bytes, per occurrence.
	r.mu.Lock()
	r.audio[c.occurrence] = audio.WAV
	r.mu.Unlock()
	key := fmt.Sprintf("text=%s|voice=%s", fold(c.text), r.voice)
	latency := int64(audio.Latency / time.Millisecond)
	// One fresh header per lifecycle step: the messages share the occurrence
	// but not the sequence — reusing the Ready header on the Started would
	// trip the duplicate-seq fence and the utterance would never start.
	m := session.SpeechMsg{
		Header:        r.msgHeader(c.generation),
		RegionID:      c.region,
		OccurrenceID:  c.occurrence,
		Kind:          session.SpeechReady,
		VoiceID:       r.voice,
		VoiceRevision: 1,
		CacheKey:      key,
		LatencyMS:     latency,
	}
	if _, err := r.session.Apply(ctx, m); err != nil {
		return
	}
	m.Kind = session.SpeechStarted
	m.Header = r.msgHeader(c.generation)
	if _, err := r.session.Apply(ctx, m); err != nil {
		return
	}
	r.mu.Lock()
	r.started[c.occurrence] = c.generation
	r.mu.Unlock()
	// The playback half: the real scheduler, fenced on the session's commit
	// generation, plays the utterance through the filesink. A generation the
	// fence no longer approves is dropped before it can start ("zero stale
	// audio starts"), and a cancelled generation is dropped with the reason.
	if r.sched != nil {
		r.sched.Submit(speech.UtteranceReady{
			SessionID:        sessionID,
			Generation:       c.generation,
			UtteranceID:      c.occurrence,
			Priority:         speech.PriorityCurrent,
			WAV:              audio.WAV,
			ExpectedDuration: audio.Duration,
		})
	}
}

// applySpeech pushes one lifecycle step through the session's speech fence.
func (r *run) applySpeech(ctx context.Context, c commitInfo, kind session.SpeechKind, failKind string) {
	m := session.SpeechMsg{
		Header:       r.msgHeader(c.generation),
		RegionID:     c.region,
		OccurrenceID: c.occurrence,
		Kind:         kind,
		VoiceID:      r.voice,
	}
	if failKind != "" {
		m.Error = failKind + ": " + r.voice
	}
	if _, err := r.session.Apply(ctx, m); err != nil {
		// A refused speech step is the fence working (invalid phase, unknown
		// occurrence): it is recorded in the session stats, and no event is
		// emitted for a state change that did not happen.
	}
}

// finishUtterances closes the utterances still live at the end of the clip:
// the last line's speech ends cleanly; a cancelled one is refused by the
// session's phase fence (a cancelled phase cannot re-enter by an end), which
// is exactly "no stale audio starts".
func (r *run) finishUtterances(ctx context.Context) {
	r.mu.Lock()
	live := make(map[string]int64, len(r.started))
	for occ, gen := range r.started {
		live[occ] = gen
	}
	r.mu.Unlock()
	for occ, gen := range live {
		m := session.SpeechMsg{
			Header:       r.msgHeader(gen),
			RegionID:     r.region,
			OccurrenceID: occ,
			Kind:         session.SpeechEnded,
			VoiceID:      r.voice,
		}
		_, _ = r.session.Apply(ctx, m)
	}
}

// setDegraded tracks the provider.degraded state per role: the first failure
// publishes the degradation, the first success after it the recovery.
func (r *run) setDegraded(ctx context.Context, role string, degraded bool, reason string) {
	r.mu.Lock()
	cur := r.degraded[role]
	if cur == degraded {
		r.mu.Unlock()
		return
	}
	r.degraded[role] = degraded
	r.mu.Unlock()
	r.emit(ctx, events.ProviderDegraded, map[string]any{
		"role":     role,
		"degraded": degraded,
		"reason":   reason,
	})
}

// emit publishes one harness event onto the shared timeline.
func (r *run) emit(ctx context.Context, name string, data map[string]any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	_, _ = r.hub.Publish(ctx, events.Envelope{
		SessionID: sessionID,
		Name:      name,
		Data:      payload,
	})
}

// publishFault is the faultInjector's report sink: one fault.injected event
// per fault entry, on the shared timeline.
func (r *run) publishFault(index int, kind string, timeMS int64, role string, detail string) {
	if r.hub == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"fault_index": index,
		"kind":        kind,
		"time_ms":     timeMS,
		"role":        role,
		"detail":      detail,
	})
	_, _ = r.hub.Publish(context.Background(), events.Envelope{
		SessionID: sessionID,
		Name:      eventNameFaultInjected,
		Data:      payload,
	})
}

// close releases every resource the run holds.
func (r *run) close() {
	if r.sched != nil {
		_ = r.sched.Stop()
	}
	if r.sink != nil {
		_ = r.sink.Close()
	}
	if r.gwDec != nil {
		r.gwDec.Close()
	}
	if r.gwTTS != nil {
		r.gwTTS.Close()
	}
	if r.src != nil {
		_ = r.src.Close()
	}
	if r.hub != nil {
		_ = r.hub.Close()
	}
}

// sidecarPub is the sidecar session's publisher: the speech lifecycle events
// are forwarded onto the shared timeline (they are the provider layer's
// evidence); everything else stays local, numbered the way the hub would, so
// the sidecar's causation chain stays inspectable without touching the
// merged seq space.
type sidecarPub struct {
	forward map[string]bool
	hub     *events.Hub

	mu    sync.Mutex
	seq   int64
	local []events.Envelope
}

// speechEventNames are the session event names the merged timeline carries
// from the sidecar.
func speechEventNames() map[string]bool {
	return map[string]bool{
		events.SpeechReady:     true,
		events.SpeechStarted:   true,
		events.SpeechCancelled: true,
		events.SpeechEnded:     true,
	}
}

// Publish implements session.Publisher.
func (p *sidecarPub) Publish(ctx context.Context, e events.Envelope) (events.Envelope, error) {
	if !p.forward[e.Name] {
		p.mu.Lock()
		p.seq++
		e.Seq = p.seq
		if e.EventID == "" {
			e.EventID = games.NewID()
		}
		if e.OccurredAt.IsZero() {
			e.OccurredAt = time.Now().UTC()
		}
		if e.SchemaVersion == 0 {
			e.SchemaVersion = events.SchemaVersion
		}
		p.local = append(p.local, e)
		p.mu.Unlock()
		return e, nil
	}
	return p.hub.Publish(ctx, e)
}

// localEvents keeps the sidecar's non-forwarded events for diagnostics.
func (p *sidecarPub) localEvents() []events.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]events.Envelope, len(p.local))
	copy(out, p.local)
	return out
}

// providerErrorKind reduces one provider failure to its taxonomy kind for
// the timeline (docs/spec/06 error classification).
func providerErrorKind(err error) string {
	var pe *providers.Error
	if stderrors.As(err, &pe) {
		return string(pe.Kind)
	}
	return "unknown"
}
