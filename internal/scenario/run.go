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
)

// providerTimeout bounds one provider call. A faulted endpoint is
// expected to be slow; a call that outlives the bound is a timeout and
// an honest abstain, the way docs/spec/06's decision budget behaves.
// It is a var, not a const, so the race-instrumented test build can widen
// it (race_on_test.go): under -race on a saturated host the same sidecar
// work takes several times longer on the wall clock (issue #22), and a
// bound that times out a healthy call turns a correctness test into a
// load flake.
var providerTimeout = 10 * time.Second

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
	reclaimOurTempDirs() // harness start: reclaim a killed run's temp dirs (issue #14)
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
	// free, exactly as the real-engine gate does. The catch-up policy is the
	// scenario's declared one: a no_stale_speech verdict under catch_up must
	// not read a completed non-last line as stale (ADR-0122 decision 2: the
	// line that is playing keeps playing to the end), while latest_only keeps
	// the historical cut-on-advance reading.
	report := sc.EvaluateWith(evs, run.pipelineStats, run.truth, EvaluateOpts{OCREngine: truthEngineName, Corpus: run.profile.CorpusFold(), CatchUp: sc.CatchUp})
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

	// Catch-up narration (issue #25, ADR-0122): the sidecar's batcher, the
	// scenario's catch_up policy running the shared decision core
	// (internal/speech: the window drop, the speaker-run split, the
	// sentence-spacing join, the versioned merged id). cuMu guards the batch
	// bookkeeping; it is a separate lock from mu because the flush workers
	// and the scheduler's playback terminal both touch it from their own
	// goroutines while mu guards the shared sidecar tables.
	catchUpPolicy string // the scenario's policy (harness default: latest_only)
	speakerTable  map[string]string
	cuMu          sync.Mutex
	cuPending     []speech.CatchUpLine // the batch: committed lines awaiting the flush
	cuTimer       *time.Timer          // the window flush timer (nil while no batch is pending)
	cuSynth       int                  // utterances in their TTS round trip
	cuPlaying     int                  // utterances submitted, not yet terminal
	cuPlayingGen  int64                // the generation the scheduler plays now (0 = idle)
	cuApproved    []int64              // catch-up generations the fence spares (cap 16)
	cuDropped     int                  // lines the window or the max age fenced
	cuMergedRuns  int                  // merged runs the flushes built
	cuWG          sync.WaitGroup       // the flush workers in flight
	// cuActive is the set of catch-up generations submitted to the scheduler
	// and not yet terminal (cuMu): the immediate line and each merged run's
	// generation. advanceFence cancels only the stale generations it still
	// holds — a generation that has already reached its terminal (the
	// playback ended, the run finished) is no longer queued or playing, so a
	// later commit must not cancel it a second time (the spurious re-cancel
	// that cut the first line's evidence WAV before the sink wrote it).
	cuActive map[int64]bool
	// cuMergedOcc is the merged run's occurrences (cuMu): the scheduler
	// plays each one through the filesink, so the audio evidence check
	// counts their WAVs the way it counts the immediate path's.
	cuMergedOcc map[string]bool

	// Per-character voices (issue #36, gap G3, ADR-0132): the scenario's
	// approved roster (speaker label to voice name, copied at run start so
	// a shared scenario struct is never mutated) and the voice each started
	// occurrence was synthesised with. The latter lets the terminals
	// (finishUtterances, applySpeech) name the occurrence's OWN voice rather
	// than the scenario's default, so the timeline's voice_id agrees with
	// the request every record of the utterance carries.
	voices     map[string]string
	voiceByOcc map[string]string // occurrence → the voice it was synthesised with (mu)
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
	voices := make(map[string]string, len(sc.Voices))
	for k, v := range sc.Voices {
		voices[k] = v
	}
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
		cuMergedOcc:   map[string]bool{},
		cuActive:      map[int64]bool{},
		voices:        voices,
		voiceByOcc:    map[string]string{},
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
	// The scenario's tone (issue #25, ADR-0122): the catch-up scenario names
	// the overlap it models — a tone still playing when the next line commits
	// — instead of leaving it to the scheduler's coin flip.
	if r.sc.Inference.TTSToneMS > 0 {
		r.gwTTS.setToneMS(r.sc.Inference.TTSToneMS)
	}
	r.catchUpPolicy = r.sc.CatchUp
	r.speakerTable = r.sc.Speakers
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
// The dir carries a .gn-owner marker so a SIGKILLed run's sink is
// reclaimable by the next harness start (issue #14).
// The scheduler's publisher is the catch-up observer: the sidecar session
// is the event recorder of record for the merged timeline, so the observer
// swallows the playback events (duplicating them would double-count every
// start) and reads them only for the catch-up policy's bookkeeping (issue
// #25, ADR-0122): the playing generation the fence spares, and the
// playback terminal that flushes the pending batch the way the runtime's
// wiring flushes on the scheduler's end.
func (r *run) buildAudioPipeline(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "gn-scenario-audio-*")
	if err != nil {
		return fmt.Errorf("audio dir: %w", err)
	}
	writeTempOwnerMarker(dir) // reclaimable if this run is SIGKILLed (issue #14)
	r.audioDir = dir
	sink, err := filesink.New(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("filesink: %w", err)
	}
	r.sink = sink
	r.sched = speech.NewScheduler(sink, r, &catchUpPub{run: r}, speech.Options{})
	r.sched.Start(ctx)
	return nil
}

// IsCurrent is the harness's playback fence: the session's commit
// generation is the only generation the scheduler may start (the same rule
// the runtime's fence applies, docs/spec/01 invariant 2) — plus, under the
// catch-up policy, the generations a flush approved before its submits
// (ADR-0122 decision 4: the fence approves every merged run BEFORE the
// first submit, so a commit that lands mid-flush cannot fence a run the
// fence already let speak).
func (r *run) IsCurrent(g int64) bool {
	r.cuMu.Lock()
	for _, a := range r.cuApproved {
		if a == g {
			r.cuMu.Unlock()
			return true
		}
	}
	r.cuMu.Unlock()
	return r.session.Generation() == g
}

// catchUpPub swallows the scheduler's playback events (the sidecar session
// is the publisher of record) and observes them for the catch-up policy's
// bookkeeping (issue #25, ADR-0122): the start moves the playing
// generation the fence spares, and the playing generation's terminal
// unapproves it and flushes the pending batch on the playback end — the
// runtime's noteSchedulerEvent flush, the batcher's own trigger.
type catchUpPub struct {
	run *run
}

type schedPlaybackEvent struct {
	UtteranceID string `json:"utterance_id"`
	Generation  int64  `json:"generation"`
	Result      string `json:"result"`
}

func (p *catchUpPub) Publish(_ context.Context, e events.Envelope) (events.Envelope, error) {
	var ev schedPlaybackEvent
	if json.Unmarshal(e.Data, &ev) != nil || ev.Generation == 0 {
		return events.Envelope{}, nil
	}
	switch e.Name {
	case events.SpeechStarted:
		p.run.notePlaybackStarted(ev.Generation)
	case events.SpeechEnded, events.SpeechCancelled:
		p.run.notePlaybackTerminal(ev.Generation)
	}
	return events.Envelope{}, nil
}

// advanceFence mirrors the runtime's fenceAndCancel at the harness level:
// every commit cancels every older generation still queued or playing, so
// a commit that lands while an older utterance is synthesising or playing
// stops that audio at the source (docs/spec/12 "zero stale audio starts").
// Under the catch-up policy the line that is playing keeps being heard —
// a new commit batches the new line instead of cutting the playback (the
// behaviour issue #25 asked for, ADR-0122 decision 2): the fence spares
// the playing generation the way the runtime's fence spares it.
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
	// The catch-up policy's spare (ADR-0122 decision 2): the line that is
	// playing keeps playing to the end — the tap batches, it does not cut.
	var playing int64
	if r.catchUpPolicy == session.CatchUpPolicyCatchUp {
		r.cuMu.Lock()
		playing = r.cuPlayingGen
		r.cuMu.Unlock()
	}
	for _, g := range stale {
		if g == playing {
			continue
		}
		// Under the catch-up policy a generation that has already reached its
		// terminal is no longer queued or playing: the first line's playback
		// ended (its evidence WAV is written) long before a later commit
		// lands, so the commit must not cancel it a second time and cut the
		// sink's write (issue #25, ADR-0122 decision 2). Only a generation
		// still active (submitted, not yet terminal) is cancelled at the
		// source. latest_only keeps the historical shape: every older started
		// generation is cancelled on the advance.
		if r.catchUpPolicy == session.CatchUpPolicyCatchUp {
			r.cuMu.Lock()
			active := r.cuActive[g]
			r.cuMu.Unlock()
			if !active {
				continue
			}
		}
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
		// The perceptual skip runs at its product defaults here (ADR-0115 §3,
		// issue #26): the settle gate keeps the typewriter's word-sized steps —
		// inside the tolerance, one frame each at the clip's 10 fps — reaching
		// OCR one frame at a time, so the graded capture-clock contract holds
		// with the same path the live feed uses. Byte-identical frames still
		// take the exact-hash half, which is the "repeat observation" the spec's
		// no-duplicate-commit assertion grades.
		// The narrated-class setting the scenario declares (docs/spec/09
		// "/play"): the pipeline's commitment policy applies the veto, so a
		// sign or item that commits in a session that did not elect to hear
		// its class is speech-ineligible in the pipeline's own commit events
		// — the record of commitment the report grades. CatchUp: the session's
		// catch-up policy the scenario names (issue #25, ADR-0122) — the same
		// word the sidecar's speech sidecar runs, so the session state the
		// timeline records matches the audio behaviour the run exhibits.
		Reducer: session.Config{NarratedKinds: r.narratedKinds, CatchUp: r.sc.CatchUp},
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
		session.WithConfig(session.Config{NarratedKinds: r.narratedKinds, CatchUp: r.sc.CatchUp}),
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

// closeAudio stops the playback half: the catch-up workers drain (bounded),
// the batch the run's end meets still pending settles with the reason, and
// the scheduler stops any active playback (the filesink writes the
// delivered prefix as the evidence WAV) and drains its loop, bounded. The
// sink itself is closed by close(); the directory is kept as the run's
// audio evidence.
func (r *run) closeAudio() {
	// The catch-up playback chain drains before the scheduler stops
	// (issue #25, ADR-0122): each run's end flushes the next batch, so a
	// flush worker finishes after SUBMITTING its runs, not after the runs
	// play. Stopping the scheduler on the flush workers' drain alone would
	// cut the in-flight run and drop the queued one (their evidence WAVs are
	// written on the playback's end, so the scheduler must not stop before
	// the runs end). waitCatchUpSettled keeps the runs playing to their
	// terminal; a wedged provider or sink must not hold the run's
	// finalisation open, so the wait is bounded.
	r.waitCatchUpSettled()
	// The batch the run's end meets still pending is skipped with the
	// reason before the scheduler stops (docs/spec/13 on the way out).
	r.settleCatchUpBatch()
	if r.sched != nil {
		_ = r.sched.Stop()
	}
}

// waitCatchUpSettled waits for the catch-up playback chain to settle: no
// batch pending, no synthesis in flight, and no submitted utterance still
// active. The chain's trigger is a playback's terminal, which spawns the
// next flush worker after this method's WaitGroup drain, so the loop
// re-waits on the group each pass and only returns once nothing is pending,
// synthesising or playing. Under latest_only the batcher never runs (the
// counters stay zero), so the first pass settles immediately. Bounded:
// the per-utterance TTS is itself bounded by providerTimeout, so a wedged
// provider or sink cannot hold the run's finalisation open.
func (r *run) waitCatchUpSettled() {
	deadline := time.Now().Add(providerTimeout)
	for {
		r.cuWG.Wait()
		r.cuMu.Lock()
		settled := len(r.cuPending) == 0 && r.cuSynth == 0 && r.cuPlaying == 0
		r.cuMu.Unlock()
		if settled {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
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
// unfinished or abandoned write). Per utterance (issue #36, ADR-0132): every
// started occurrence — the immediate path's and the catch-up runs' — must
// have its OWN evidence WAV, named after its own utterance id (the sink's
// file is gen<generation>-<SafeName(utterance)>-<seq>.wav): the aggregate
// count alone cannot see a run that wrote one utterance's audio under
// another utterance's name, and "the evidence WAV exists per utterance" is
// what the two-voice scenario asserts.
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
	r.cuMu.Lock()
	merged := len(r.cuMergedOcc)
	wantParts := make(map[string]bool, len(started)+merged)
	for occ := range started {
		wantParts["-"+filesink.SafeName(occ)+"-"] = true
	}
	for occ := range r.cuMergedOcc {
		wantParts["-"+filesink.SafeName(occ)+"-"] = true
	}
	r.cuMu.Unlock()

	entries, err := os.ReadDir(r.audioDir)
	if err != nil {
		return Result{Type: "audio_evidence_clean", Passed: false, Detail: fmt.Sprintf("read sink dir: %v", err)}, true
	}
	var offenders []string
	files := 0
	havePart := make(map[string]bool, len(wantParts))
	for part := range wantParts {
		havePart[part] = false
	}
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
		for part := range wantParts {
			if strings.Contains(name, part) {
				havePart[part] = true
			}
		}
	}
	// The expected count is the immediate path's started utterances plus the
	// catch-up runs (issue #25, ADR-0122): each flush's run plays through
	// the sink and leaves its own evidence WAV, the way a started one does.
	want := len(started) + merged
	if len(offenders) == 0 && files != want {
		offenders = append(offenders, fmt.Sprintf("%d evidence WAV(s) in the sink dir for %d started utterance(s) and %d catch-up run(s): every started utterance and every run must leave exactly one, and no other utterance may leave one", files, len(started), merged))
	}
	for part, ok := range havePart {
		if !ok {
			offenders = append(offenders, fmt.Sprintf("no evidence WAV named after utterance %q in the sink dir", strings.Trim(part, "-")))
		}
	}
	detail := fmt.Sprintf("sink dir %s: %d evidence WAV(s) for %d started utterance(s) and %d catch-up run(s), one per utterance, no temporary file", r.audioDir, files, len(started), merged)
	if len(offenders) > 0 {
		return Result{Type: "audio_evidence_clean", Passed: false, Detail: strings.Join(offenders, "; ")}, true
	}
	return Result{Type: "audio_evidence_clean", Passed: true, Detail: detail}, true
}

// runSidecar is layer B: the paced truth feed plus the decision/TTS work it
// triggers. The feed paces the capture clock so the commit wall times mirror
// the clip: a decision delayed by 900 ms on the wall clock is a decision
// delayed by 900 ms on the recording. The pacing is a simulation of the
// dialogue's own cadence, not a test budget: the TTS interleaving the
// scenario assertions grade — an utterance starting while the previous line
// is still playing, a dialogue_advanced cancel mid-playback, the evidence
// WAV the sink writes over the utterance's duration — needs the wall gap
// between commits a real session has while the previous line is spoken
// (issue #22: the load sensitivity was the suite's shared 3-minute context
// outrun by these paced runs on a saturated race host, fixed by the
// race-scaled suite bound in scenarios_test.go — not by removing the
// pacing, which the playback interleaving depends on).
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
			r.routeSpeech(ctx, c)
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
		r.routeSpeech(ctx, c)
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

	// The per-character voice (issue #36, gap G3, ADR-0132): the line's
	// speaker's approved voice when the scenario declares one, else the
	// narrator's. It rides the TTS request, the session's lifecycle events
	// and the cache key — every record of the utterance names the voice the
	// request was made with.
	speaker := r.speakerFor(c.messageID)
	voice := r.voiceFor(speaker)
	if voice != r.voice || speaker != "narrator" {
		r.mu.Lock()
		r.voiceByOcc[c.occurrence] = voice
		r.mu.Unlock()
	}

	if r.sc.Inference.TTS == InferenceNone {
		// No voice layer in this scenario: the M2 sidecar shape applies the
		// lifecycle directly, with the resolved voice named.
		r.applySpeech(ctx, c, session.SpeechReady, "", voice)
		return
	}

	// The catch-up policy's busy bookkeeping (issue #25, ADR-0122): while
	// this utterance is in its TTS round trip a newer commit is not idle —
	// it batches instead of racing a second synthesis. latest_only runs
	// carry no batcher, so the counter stays at zero and the policy's
	// decisions never run.
	catchUp := r.catchUpPolicy == session.CatchUpPolicyCatchUp
	if catchUp {
		r.cuMu.Lock()
		r.cuSynth++
		r.cuMu.Unlock()
		defer func() {
			r.cuMu.Lock()
			r.cuSynth--
			r.cuMu.Unlock()
		}()
	}

	pctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	r.ttsWrap.stampMS = c.tsMS
	audio, err := r.ttsWrap.GenerateStream(pctx, tts.GenerateRequest{Text: c.text, Voice: voice})
	if err != nil {
		// Silence with a reason: the failure carries the voice that was
		// requested, never a substitute. A mid-synthesis endpoint death is
		// the same terminal step: the utterance ends failed, and the phase
		// fence keeps it failed — a started is refused after a failed.
		r.applySpeech(ctx, c, session.SpeechFailed, providerErrorKind(err), voice)
		return
	}
	// Report material: the actual synthesized bytes, per occurrence.
	r.mu.Lock()
	r.audio[c.occurrence] = audio.WAV
	r.mu.Unlock()
	key := fmt.Sprintf("text=%s|voice=%s", fold(c.text), voice)
	latency := int64(audio.Latency / time.Millisecond)
	// One fresh header per lifecycle step: the messages share the occurrence
	// but not the sequence — reusing the Ready header on the Started would
	// trip the duplicate-seq fence and the utterance would never start.
	m := session.SpeechMsg{
		Header:        r.msgHeader(c.generation),
		RegionID:      c.region,
		OccurrenceID:  c.occurrence,
		Kind:          session.SpeechReady,
		VoiceID:       voice,
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
		if catchUp {
			// The utterance is now the batcher's "playing or queued": a
			// commit that lands before the scheduler drops it is busy, and
			// the fence spares it while it plays (ADR-0122 decision 2). The
			// immediate line is the catch-up policy's playing line, so the
			// fence approves its generation before the submit — the fence's
			// own rule that a generation it already let speak cannot be
			// fenced by a commit that lands mid-submit — and the commit
			// watcher's fence spare leaves it uncancelled while it runs out.
			// Approving before Submit is what keeps the first line's evidence
			// WAV off the drop path: without it the rapid commits advance the
			// session past the line's generation and the fence drops it as
			// stale before it ever plays (the line is speech.started but the
			// sink never hears it, so audio_evidence_clean counts a WAV that
			// was never written).
			r.cuMu.Lock()
			r.cuPlaying++
			r.cuPlayingGen = c.generation
			r.cuActive[c.generation] = true
			if indexInt64(r.cuApproved, c.generation) < 0 {
				r.cuApproved = append(r.cuApproved, c.generation)
				if len(r.cuApproved) > speech.DefaultCatchUpApprovedCap {
					r.cuApproved = r.cuApproved[1:]
				}
			}
			r.cuMu.Unlock()
		}
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
// voice is the occurrence's resolved voice (the immediate path's, issue #36
// ADR-0132): the event must name the voice the request was made with, not
// the scenario's default.
func (r *run) applySpeech(ctx context.Context, c commitInfo, kind session.SpeechKind, failKind, voice string) {
	m := session.SpeechMsg{
		Header:       r.msgHeader(c.generation),
		RegionID:     c.region,
		OccurrenceID: c.occurrence,
		Kind:         kind,
		VoiceID:      voice,
	}
	if failKind != "" {
		m.Error = failKind + ": " + voice
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
	live := make([]string, 0, len(r.started))
	voiceOf := make(map[string]string, len(r.started))
	for occ := range r.started {
		live = append(live, occ)
		voiceOf[occ] = r.voiceByOcc[occ]
	}
	r.mu.Unlock()
	for _, occ := range live {
		// A settling line's terminal is a new observation (playback ended now),
		// so the header carries the current generation; the occurrence id, not
		// the generation, ties the terminal to the line still being heard
		// (issue #25, ADR-0122 decision 8). The terminal names the
		// occurrence's own voice — the one its request was made with
		// (issue #36, ADR-0132), the scenario's default when the occurrence
		// recorded none.
		voice := voiceOf[occ]
		if voice == "" {
			voice = r.voice
		}
		m := session.SpeechMsg{
			Header:       r.msgHeader(r.session.Generation()),
			RegionID:     r.region,
			OccurrenceID: occ,
			Kind:         session.SpeechEnded,
			VoiceID:      voice,
		}
		_, _ = r.session.Apply(ctx, m)
	}
}

// ── Catch-up narration (issue #25, ADR-0122) --------------------------------
//
// The sidecar's catch-up policy, the runtime's wiring shape at harness
// scale: the shared decision core (internal/speech) makes the decisions —
// the window drop, the speaker-run split, the sentence-spacing join, the
// versioned merged id — and this side owns the TTS calls, the scheduler
// submits and the events, the way the runtime's wiring does. latest_only
// runs never reach this code: routeSpeech falls straight through to speak.

// speakerFor is the harness's speaker resolution for one committed message:
// the scenario's declared attribution, the narrator when the scenario named
// none — the same fallback the runtime's resolution applies to an
// unattributed line (the synthetic profiles carry no character rows, so
// this table is the only speaker source a synthetic run has).
func (r *run) speakerFor(messageID string) string {
	if sp, ok := r.speakerTable[messageID]; ok && sp != "" {
		return sp
	}
	return "narrator"
}

// voiceFor is the harness's voice resolution for one speaker (issue #36,
// gap G3, ADR-0132): the scenario's approved voice for that speaker when
// one is declared, else the narrator's voice (r.voice, the scenario's
// inference.voice) — the runtime's resolution at harness scale: the
// character's voice, the narrator's when the line is unattributed or the
// speaker has none (ADR-0101), never a different character's voice and
// never a builtin. Under latest_only, or a scenario without a voices block,
// every speaker resolves to the narrator's voice: the historical
// single-voice behaviour, so no pre-existing scenario changes.
func (r *run) voiceFor(speaker string) string {
	if v, ok := r.voices[speaker]; ok && v != "" {
		return v
	}
	return r.voice
}

// routeSpeech is the commit→speech hand-off under the session's catch-up
// policy (issue #25, ADR-0122): latest_only speaks the line at once (the
// historical path, byte for byte the behaviour the pre-catch-up scenarios
// grade); catch_up speaks at once when speech is idle — the fast path keeps
// the first line's commit→voice latency — and BATCHES the line when an
// utterance is synthesising or playing or a batch is already pending (the
// overlap the issue's rapid A-taps produce).
func (r *run) routeSpeech(ctx context.Context, c commitInfo) {
	if r.catchUpPolicy != session.CatchUpPolicyCatchUp {
		r.speak(ctx, c)
		return
	}
	line := speech.CatchUpLine{
		Generation:  c.generation,
		Occurrence:  c.occurrence,
		MessageID:   c.messageID,
		RegionID:    c.region,
		Text:        c.text,
		Speaker:     r.speakerFor(c.messageID),
		CommittedAt: time.Now(),
	}
	var dropped []speech.CatchUpLine
	r.cuMu.Lock()
	busy := r.cuSynth > 0 || r.cuPlaying > 0 || len(r.cuPending) > 0
	if !busy {
		r.cuMu.Unlock()
		r.speak(ctx, c)
		return
	}
	// The playing line is the session's active playback, and the commit must
	// not cut it (issue #25's point: a tap that lands while the player is
	// listening keeps the line playing to the end; ADR-0122 decision 2). The
	// scheduler re-checks its fence during playback, so the approval is what
	// the "leave it to run out" is implemented with — the runtime's
	// fence.approve(playingGen), the same shape.
	if r.cuPlayingGen != 0 && indexInt64(r.cuApproved, r.cuPlayingGen) < 0 {
		r.cuApproved = append(r.cuApproved, r.cuPlayingGen)
		if len(r.cuApproved) > speech.DefaultCatchUpApprovedCap {
			r.cuApproved = r.cuApproved[1:]
		}
	}
	// The window fences the pending prefix the new line outlives (ADR-0122:
	// "Fencing (ADR 0092) still applies to anything older than the batch
	// window") — the batcher's own prefix rule, oldest first.
	if n := speech.BatchWindowDrop(r.cuPending, line.CommittedAt, speech.DefaultCatchUpBatchWindow); n > 0 {
		dropped = append([]speech.CatchUpLine(nil), r.cuPending[:n]...)
		r.cuPending = r.cuPending[n:]
		r.cuDropped += n
	}
	r.cuPending = append(r.cuPending, line)
	if r.cuTimer == nil {
		// The window elapsing since the batch's FIRST line flushes (the
		// runtime's arm-once rule): a later line extends the batch's content,
		// not its deadline.
		r.cuTimer = time.AfterFunc(speech.DefaultCatchUpBatchWindow, func() { r.flushCatchUp("catch_up_window") })
	}
	r.cuMu.Unlock()
	// The batched line's commit is still a request (the event says the line
	// was asked for and where it went); its one terminal is the batch's
	// outcome — the merged run's, the window's fence, or the run's end.
	r.emit(ctx, events.SpeechRequested, map[string]any{
		"dialogue_occurrence_id": line.Occurrence,
		"message_id":             line.MessageID,
		"generation":             line.Generation,
		"region_id":              line.RegionID,
		"text":                   line.Text,
		"character_id":           line.Speaker,
		"catch_up":               "batched",
	})
	for _, l := range dropped {
		r.emit(ctx, events.SpeechEnded, map[string]any{
			"dialogue_occurrence_id": l.Occurrence,
			"message_id":             l.MessageID,
			"generation":             l.Generation,
			"region_id":              l.RegionID,
			"result":                 "skipped",
			"reason":                 "catch_up_stale",
			"character_id":           l.Speaker,
		})
	}
}

// flushCatchUp takes the pending batch (if any) off the batcher and hands
// it to a flush worker. The window timer, the playback terminal and the run
// end all call it; an empty batch is a no-op, so the three triggers may
// race freely.
func (r *run) flushCatchUp(trigger string) {
	r.cuMu.Lock()
	if r.cuTimer != nil {
		t := r.cuTimer
		r.cuTimer = nil
		t.Stop()
	}
	batch := r.cuPending
	r.cuPending = nil
	r.cuMu.Unlock()
	if len(batch) == 0 {
		return
	}
	r.cuWG.Add(1)
	go func() {
		defer r.cuWG.Done()
		r.runCatchUpBatch(batch, trigger)
	}()
}

// runCatchUpBatch is the flush (issue #25, ADR-0122): the batcher's own
// decisions on the shared core (the max-age drop, the speaker-run split),
// then one TTS request per speaker run, in commit order. The runs share the
// batch's newest committed generation as their bookkeeping base — run i
// books base+i — and the fence approves every one of them BEFORE the first
// submit, the way the runtime's flush does (a commit that lands mid-flush
// cannot fence a run the fence already let speak: it batches its own line
// instead).
func (r *run) runCatchUpBatch(batch []speech.CatchUpLine, trigger string) {
	if len(batch) == 0 {
		return
	}
	now := time.Now()
	var batchBase int64
	for _, l := range batch {
		if l.Generation > batchBase {
			batchBase = l.Generation
		}
	}
	var dropped []speech.CatchUpLine
	var runs []speech.CatchUpRun
	for _, run := range speech.SplitRuns(batch) {
		// The per-utterance max age (issue #25 item 1): a run whose audio
		// would not be ready until after the newest line it covers committed
		// plus the max age is dropped before it starts — the dismiss-
		// without-successor backstop ADR-0092 deferred (the box is gone, no
		// commit moved the fence, and only the age bound keeps the late audio
		// off the sink).
		if now.Sub(run.LatestCommittedAt()) > speech.DefaultCatchUpMaxUtteranceAge {
			dropped = append(dropped, run.Lines...)
			continue
		}
		runs = append(runs, run)
	}
	for _, l := range dropped {
		r.cuMu.Lock()
		r.cuDropped++
		r.cuMu.Unlock()
		r.emit(ctxOf(r), events.SpeechEnded, map[string]any{
			"dialogue_occurrence_id": l.Occurrence,
			"message_id":             l.MessageID,
			"generation":             l.Generation,
			"region_id":              l.RegionID,
			"result":                 "skipped",
			"reason":                 "catch_up_stale",
			"character_id":           l.Speaker,
		})
	}
	if len(runs) == 0 {
		return
	}
	// Approve the fence before the first submit (ADR-0122 decision 4), and
	// book the runs' occurrences for the audio evidence check.
	r.cuMu.Lock()
	for i, run := range runs {
		gen := batchBase + int64(i)
		if indexInt64(r.cuApproved, gen) < 0 {
			r.cuApproved = append(r.cuApproved, gen)
			if len(r.cuApproved) > speech.DefaultCatchUpApprovedCap {
				r.cuApproved = r.cuApproved[1:]
			}
		}
		r.cuActive[gen] = true
		r.cuMergedOcc["cu-"+speech.MergedOriginalID(run.MessageIDs())] = true
		r.cuMergedRuns++
	}
	r.cuMu.Unlock()
	for i, run := range runs {
		gen := batchBase + int64(i)
		mergedID := speech.MergedOriginalID(run.MessageIDs())
		occ := "cu-" + mergedID
		// The merged run's request: the run's ids (the merged cache key's id
		// form, ADR-0122 decision 6) ride the event the report grades on.
		r.emit(ctxOf(r), events.SpeechRequested, map[string]any{
			"dialogue_occurrence_id": occ,
			"message_id":             mergedID,
			"message_ids":            run.MessageIDs(),
			"generation":             gen,
			"region_id":              run.Lines[0].RegionID,
			"text":                   run.Text,
			"character_id":           run.Speaker,
			"merged":                 len(run.Lines) > 1,
			"trigger":                trigger,
			"catch_up":               "merged",
		})
		r.speakMerged(ctxOf(r), occ, gen, run)
	}
}

// speakMerged is one merged run's TTS worker: one request for the run's
// merged text (the merge is the budget win, ADR-0122 decision 3), the
// evidence WAV under the run's occurrence, the scheduler's submit under
// the run's approved generation. The session's region lifecycle is the
// committed box's, not the run's — the run is a harness-level utterance
// (its events above are its record), the way the runtime's merged runs are
// wiring-level utterances the session's per-region lifecycle does not own.
func (r *run) speakMerged(ctx context.Context, occ string, gen int64, run speech.CatchUpRun) {
	if r.sched == nil {
		return
	}
	r.cuMu.Lock()
	r.cuSynth++
	r.cuMu.Unlock()
	defer func() {
		r.cuMu.Lock()
		r.cuSynth--
		r.cuMu.Unlock()
	}()
	pctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	// The run's voice resolves from the run's speaker (issue #36, gap G3,
	// ADR-0132): the merged id is not in the speaker table, so the speaker —
	// the decision's input, not a lookup — carries the roster, the same rule
	// the runtime's merged runs follow (resolveVoiceForCharacter on the run's
	// speaker). An unattributed run's speaker is the narrator's, whose voice
	// is the scenario's inference.voice.
	voice := r.voiceFor(run.Speaker)
	audio, err := r.ttsWrap.GenerateStream(pctx, tts.GenerateRequest{Text: run.Text, Voice: voice})
	if err != nil {
		// The run's one terminal: the lines it covered end failed with the
		// provider's reason (docs/spec/13 "did it speak, and why not").
		for _, l := range run.Lines {
			r.emit(ctx, events.SpeechEnded, map[string]any{
				"dialogue_occurrence_id": l.Occurrence,
				"message_id":             l.MessageID,
				"generation":             l.Generation,
				"region_id":              l.RegionID,
				"result":                 "failed",
				"reason":                 providerErrorKind(err),
				"character_id":           l.Speaker,
			})
		}
		return
	}
	r.mu.Lock()
	r.audio[occ] = audio.WAV
	r.voiceByOcc[occ] = voice
	r.mu.Unlock()
	// The merged run's started: the line's "spoken" evidence (the report's
	// speech_spoken_count reads the started's covered ids), the run's ids
	// and speaker riding the way the session's started rides its line's.
	// Emitted only when the synthesis succeeded: a run whose TTS failed has
	// no started, so its lines count as never spoken, not as the request
	// that preceded the failure.
	r.emit(ctx, events.SpeechStarted, map[string]any{
		"dialogue_occurrence_id": occ,
		"message_id":             speech.MergedOriginalID(run.MessageIDs()),
		"message_ids":            run.MessageIDs(),
		"generation":             gen,
		"voice_id":               voice,
		"character_id":           run.Speaker,
		"catch_up":               "merged",
	})
	r.cuMu.Lock()
	r.cuPlaying++
	r.cuMu.Unlock()
	r.sched.Submit(speech.UtteranceReady{
		SessionID:        sessionID,
		Generation:       gen,
		UtteranceID:      occ,
		Priority:         speech.PriorityCurrent,
		WAV:              audio.WAV,
		ExpectedDuration: audio.Duration,
	})
}

// notePlaybackStarted is the scheduler's start, read by the catch-up
// observer: the playing generation is the fence's spare (ADR-0122
// decision 2).
func (r *run) notePlaybackStarted(gen int64) {
	r.cuMu.Lock()
	r.cuPlayingGen = gen
	r.cuMu.Unlock()
}

// notePlaybackTerminal is the scheduler's end/cancel for one generation:
// it unapproves the generation, and when the PLAYING generation ends it
// moves on and flushes the pending batch on the playback end — the
// runtime's flush trigger the batcher owes (a line that committed while
// the previous line was still playing speaks back-to-back, not at the
// window's end when the end came first).
func (r *run) notePlaybackTerminal(gen int64) {
	r.cuMu.Lock()
	if i := indexInt64(r.cuApproved, gen); i >= 0 {
		r.cuApproved = append(r.cuApproved[:i], r.cuApproved[i+1:]...)
	}
	// The generation reached its terminal: it is no longer queued or playing,
	// so a later commit must not cancel it a second time (the spurious
	// re-cancel that cut the first line's evidence WAV before the sink wrote
	// it, issue #25 ADR-0122).
	delete(r.cuActive, gen)
	wasPlaying := r.cuPlayingGen == gen
	if wasPlaying {
		r.cuPlayingGen = 0
	}
	r.cuPlaying--
	if r.cuPlaying < 0 {
		r.cuPlaying = 0
	}
	r.cuMu.Unlock()
	if wasPlaying {
		r.flushCatchUp("playback_ended")
	}
}

// ctxOf is the flush worker's publish context: the run's context is
// already drained by the time the workers finish, and the hub's publish
// must not fail on the run's end (the runtime's endCtx rule, scaled to the
// harness's single context).
func ctxOf(r *run) context.Context {
	return context.Background()
}

// settleCatchUpBatch takes the run down: the batch the run's end meets
// still pending is skipped with the reason (docs/spec/13 still holds on
// the way out), and the approvals empty — a batch the run is not speaking
// keeps nothing speakable.
func (r *run) settleCatchUpBatch() {
	r.cuMu.Lock()
	if r.cuTimer != nil {
		t := r.cuTimer
		r.cuTimer = nil
		t.Stop()
	}
	pending := r.cuPending
	r.cuPending = nil
	r.cuApproved = nil
	r.cuPlayingGen = 0
	r.cuMu.Unlock()
	for _, l := range pending {
		r.emit(ctxOf(r), events.SpeechEnded, map[string]any{
			"dialogue_occurrence_id": l.Occurrence,
			"message_id":             l.MessageID,
			"generation":             l.Generation,
			"region_id":              l.RegionID,
			"result":                 "skipped",
			"reason":                 "session_ended",
			"character_id":           l.Speaker,
		})
	}
}

// indexInt64 is the approved set's linear find (the set is capped at 16;
// a linear scan is the bound the runtime's fence relies on).
func indexInt64(set []int64, v int64) int {
	for i, g := range set {
		if g == v {
			return i
		}
	}
	return -1
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
