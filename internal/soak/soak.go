// Package soak is the long-run soak harness of docs/spec/12: "No unhandled
// panics/OOM during at least 1h replay load on UNO Q profile and 2h simulated
// network/provider chaos", with the docs/spec/10 resource budgets as its
// pass criteria.
//
// The soak loops the synthetic clip(s) through the real pipeline + session
// for the whole duration — the same two-layer composition the scenario
// harness runs (ADR-0037): layer A is the real pipeline in ModeReplay over
// the cycling replay source (the "replay load"), and layer B is a sidecar
// session paced on the capture clock that drives the real provider clients
// at the fake (chaos-injecting) gateways. Every SampleEvery the soak records
// heap/RSS, the goroutine count, the open-FD count and the run's counters
// (queue drops, commits, speech events, provider errors, recovered panics),
// and at the end it asserts the pass criteria and writes a JSON + markdown
// report under artifacts/soak/<run-id>/.
//
// Provenance: the frames and their labels are generated_synthetic
// (docs/spec/11 "Fixture origins") — the soak tests the runtime's endurance
// under load and chaos, not OCR quality (that gate is the M2 real-Tesseract
// replay test) or real-game behaviour (that is Dolphin/hardware mode).

package soak

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/session"
)

// Config is one soak run.
type Config struct {
	// Duration is the whole-run budget. The Makefile soak target runs
	// 1h replay then 2h chaos as two Runs.
	Duration time.Duration
	// Clips are the clip directories (relative to RepoRoot) the harness
	// cycles; each pass replays one clip.
	Clips []string
	// Speed is the replay speed: 0 (the default) drains as fast as the
	// pipeline takes — the sustained load; 1.0 paces the frames at the
	// recording's own rate.
	Speed float64
	// Chaos enables and tunes the fault injection; the zero value with
	// Enabled false is a clean replay run.
	Chaos ChaosConfig
	// SampleEvery is the resource sampling period (default 5 s).
	SampleEvery time.Duration
	// RepoRoot is the repository root the fixture paths resolve against.
	RepoRoot string
	// Profile is the profile name under testdata/synthetic/profiles.
	Profile string
	// OutDir is the report directory; empty means <RepoRoot>/artifacts/soak.
	OutDir string
	// Voice is the voice every speech event must name (the
	// no_voice_substitution assertion); empty uses the soak default.
	Voice string
}

// ChaosConfig tunes the fault injection. Zero fields take the defaults in
// fillDefaults; the seed makes a chaos run reproducible (0 picks one, and the
// report carries it).
type ChaosConfig struct {
	// Enabled switches the chaos on.
	Enabled bool
	// Seed is the RNG seed (0 = pick one, recorded in the report).
	Seed int64
	// Rate is the probability a provider request draws a fault (default 0.15).
	Rate float64
	// BurstMin/BurstMax bound a 5xx burst's length in requests (2..6).
	BurstMin, BurstMax int
	// LatencySpikeMinMS/LatencySpikeMaxMS bound the latency-spike hold (300..1500).
	LatencySpikeMinMS, LatencySpikeMaxMS int
	// SlowBodyGapMS is the between-chunk gap of a slow body (300).
	SlowBodyGapMS int
	// DecisionDelayMS is the scenario-shape decision hold (900, the
	// gn-t034 value).
	DecisionDelayMS int
	// DropFramesPerPass is the frames the chaos withholds per clip pass,
	// from both layers (2).
	DropFramesPerPass int
	// DuplicateRate is the probability an applied decision answer is
	// delivered a second time (the scenario's decision_duplicate shape; 0.1).
	DuplicateRate float64
}

// Pass-criteria bounds (documented; the ADR records the rationale):
const (
	// maxRSSGrowthPct is the maximum RSS growth from the 10% mark to the end.
	maxRSSGrowthPct = 20.0
	// maxRSSGrowthBytes is the absolute growth bound (docs/spec/10: 4 GB box).
	maxRSSGrowthBytes = 1 << 30
	// The rss_slope bound is not a fixed rate: it is the growth rate that would
	// consume the maxRSSGrowthBytes budget over the whole soak window
	// (maxRSSGrowthBytes / window, see rssSlopeBound). A short CI soak therefore
	// leans on the rss_growth level gate, and the 1h/2h soaks gain a slope bound
	// calibrated to their own horizon (1 GiB / 2h = 8.5 MiB/min). This keeps the
	// detector honest across scales: it flags a monotonic growth only when that
	// growth, sustained over the window, would actually breach the 1 GiB budget.
	// The Go runtime's VmRSS arena ratcheting (one-time steps plus a saturating
	// drift, observed well under these bounds) is not a leak and must not trip
	// it; see the ADR.
	// goroutineFDSlop is the steady-state allowance over the pre-run baseline:
	// the pipeline workers, the gateway listeners and the client's idle
	// connections are the only steady additions.
	goroutineFDSlop = 16
	// faultRecoveryBound is how long after a fault a commit must land. The
	// local chain commits a line per ~1.2 s of pass; the bound is 25x that
	// plus the provider timeout margin, so a single slow decision (10 s)
	// cannot hold the count flat across the window.
	faultRecoveryBound = 30 * time.Second
)

// Verdict is one pass criterion's outcome.
type Verdict struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// resourceSample is the (goroutine, FD) pair the baseline and the final
// sample compare.
type resourceSample struct {
	T          time.Time `json:"t"`
	RSSKB      int64     `json:"rss_kb"`
	Goroutines int       `json:"goroutines"`
	FDs        int       `json:"fds"`
}

// Result is the soak's verdict for one run.
type Result struct {
	RunID           string `json:"run_id"`
	Mode            string `json:"mode"` // "replay" | "chaos"
	Seed            int64  `json:"seed,omitempty"`
	Config          Config `json:"config"`
	Started         time.Time
	DurationWall    time.Duration    `json:"duration_wall"`
	Baseline        resourceSample   `json:"baseline"`
	Final           resourceSample   `json:"final"`
	Samples         []sample         `json:"samples"`
	Faults          map[string]int64 `json:"faults"`
	FaultEvents     []faultEvent     `json:"fault_events"`
	ProviderErrors  map[string]int64 `json:"provider_errors"`
	Pipeline        pipeline.Stats   `json:"pipeline"`
	Session         session.Stats    `json:"session"`
	SourceDrops     int64            `json:"source_drops"`
	Panics          int64            `json:"panics"`
	PassesEvaluated int              `json:"passes_evaluated"`
	PassFailures    []string         `json:"pass_failures"`
	Verdicts        []Verdict        `json:"verdicts"`
	ReportDir       string           `json:"report_dir"`
	ReportErr       string           `json:"report_err,omitempty"`
	RunErr          string           `json:"run_err,omitempty"`
}

// Passed reports the run's overall verdict: every pass criterion held and the
// run itself completed (no pipeline error, no panics).
func (r *Result) Passed() bool {
	if r.RunErr != "" {
		return false
	}
	for _, v := range r.Verdicts {
		if !v.Passed {
			return false
		}
	}
	return true
}

// SummaryLine is the one-line report the test prints: verdict, run id, mode,
// the RSS growth and slope, the goroutine/FD deltas, the fault and commit
// totals, and the first failing criterion when one failed.
func (r *Result) SummaryLine() string {
	verdict := "PASS"
	if !r.Passed() {
		verdict = "FAIL"
	}
	line := fmt.Sprintf("soak: %s run=%s mode=%s wall=%s rss_growth=%s slope=%.2fMiB/min goroutines %d->%d fds %d->%d commits=%d faults=%d provider_errors=%d panics=%d passes_evaluated=%d report=%s",
		verdict, r.RunID, r.Mode, r.DurationWall.Round(time.Second),
		r.rssGrowth(), r.rssSlopeMiBPerMin(),
		r.Baseline.Goroutines, r.Final.Goroutines,
		r.Baseline.FDs, r.Final.FDs,
		r.Pipeline.Commits, r.FaultsTotal(), r.ProviderErrorsTotal(),
		r.panicsTotal(), r.PassesEvaluated, r.ReportDir)
	if verdict == "FAIL" {
		line += " first_fail=" + r.firstFailure()
	}
	return line
}

// Run runs one soak for cfg.Duration (or until ctx is done) and returns its
// Result. The report is always written, even when the run fails: a failed
// soak's numbers are the evidence (AGENTS.md "first reproduce").
func Run(ctx context.Context, cfg Config) (Result, error) {
	if err := cfg.fillDefaults(); err != nil {
		return Result{}, err
	}
	res := Result{Mode: modeName(cfg)}
	res.RunID = newRunID(res.Mode)

	h, err := newHarness(cfg)
	if err != nil {
		return res, fmt.Errorf("soak: build harness: %v", err)
	}
	if err := h.build(); err != nil {
		return res, fmt.Errorf("soak: wire harness: %v", err)
	}

	// The baseline is the pre-run steady state: goroutines and FDs before any
	// soak resource exists, the reference "between loops" means the run
	// returns to.
	res.Baseline = resourceSample{T: time.Now(), RSSKB: rssKB(), Goroutines: numGoroutines(), FDs: fdCount()}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.NewTimer(cfg.Duration)
	defer timer.Stop()

	if err := h.start(runCtx); err != nil {
		return res, fmt.Errorf("soak: start: %v", err)
	}
	sidecarDone := make(chan struct{})
	go func() {
		defer close(sidecarDone)
		_ = h.sidecar(runCtx)
	}()

	// The sampler: one sample immediately, then every SampleEvery. A dedicated
	// collector drains the channel for the whole run: without a live reader the
	// bounded channel would fill and back-pressure the sampler, so a long soak
	// would record only the first few samples (the whole point of the resource
	// table would be lost).
	sampleCtx, sampleCancel := context.WithCancel(ctx)
	samples := make(chan sample)
	go sampler(sampleCtx, cfg.SampleEvery, func() sample { return h.takeSample() }, samples)
	collectorDone := make(chan struct{})
	var samplesMu sync.Mutex
	go func() {
		defer close(collectorDone)
		for s := range samples {
			samplesMu.Lock()
			res.Samples = append(res.Samples, s)
			samplesMu.Unlock()
		}
	}()

	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	res.DurationWall = time.Since(res.Baseline.T)

	// Stop the load: cancel the run, end the frame stream, wait for the
	// pipeline, drain the provider work, close the live utterances, release
	// the gateways. Each wait is bounded: a run that cannot stop is a
	// failure the report names, not a hang.
	cancel()
	if err := h.src.Close(); err != nil {
		res.RunErr = fmt.Sprintf("source close: %v", err)
	}
	if err := waitTimeout(h.pipeDone, 30*time.Second); err != nil {
		res.RunErr = appendErr(res.RunErr, fmt.Sprintf("pipeline did not stop: %v", err))
	}
	if err := waitTimeout(waitGroupDone(&h.wg), 2*providerTimeout); err != nil {
		res.RunErr = appendErr(res.RunErr, fmt.Sprintf("provider work did not drain: %v", err))
	}
	h.finishUtterances(context.Background())
	h.close()
	sampleCancel()
	<-collectorDone
	res.Final = resourceSample{T: time.Now(), RSSKB: rssKB(), Goroutines: numGoroutines(), FDs: fdCount()}

	if h.pipeErr != nil && res.RunErr == "" {
		res.RunErr = h.pipeErr.Error()
	}

	res.collect(h)
	res.evaluate()
	res.ReportDir = writeReport(&res)
	if res.ReportErr != "" && res.RunErr == "" {
		res.RunErr = res.ReportErr
	}
	return res, nil
}

// modeName names the report's mode.
func modeName(cfg Config) string {
	if cfg.Chaos.Enabled {
		return "chaos"
	}
	return "replay"
}

// fillDefaults validates the config and fills zero fields with the soak's
// documented defaults.
func (c *Config) fillDefaults() error {
	if c.Duration <= 0 {
		return fmt.Errorf("soak: Duration must be > 0 (got %v)", c.Duration)
	}
	if len(c.Clips) == 0 {
		return fmt.Errorf("soak: at least one clip is required")
	}
	if c.Profile == "" {
		c.Profile = "dialogue-advance-001"
	}
	if c.SampleEvery <= 0 {
		c.SampleEvery = 5 * time.Second
	}
	if c.RepoRoot == "" {
		// The test runs from internal/soak; the package root is the default.
		c.RepoRoot, _ = os.Getwd()
	}
	if c.Voice == "" {
		c.Voice = soakVoiceID
	}
	chaos := &c.Chaos
	if chaos.Rate <= 0 {
		chaos.Rate = 0.15
	}
	if chaos.Rate > 1 {
		return fmt.Errorf("soak: Chaos.Rate must be in (0,1] (got %v)", chaos.Rate)
	}
	if chaos.BurstMin <= 0 {
		chaos.BurstMin = 2
	}
	if chaos.BurstMax <= 0 || chaos.BurstMax < chaos.BurstMin {
		chaos.BurstMax = 6
	}
	if chaos.LatencySpikeMinMS <= 0 {
		chaos.LatencySpikeMinMS = 300
	}
	if chaos.LatencySpikeMaxMS <= 0 || chaos.LatencySpikeMaxMS < chaos.LatencySpikeMinMS {
		chaos.LatencySpikeMaxMS = 1500
	}
	if chaos.SlowBodyGapMS <= 0 {
		chaos.SlowBodyGapMS = 300
	}
	if chaos.DecisionDelayMS <= 0 {
		chaos.DecisionDelayMS = 900
	}
	if chaos.DropFramesPerPass < 0 {
		return fmt.Errorf("soak: Chaos.DropFramesPerPass must be >= 0")
	}
	if chaos.DuplicateRate < 0 || chaos.DuplicateRate > 1 {
		return fmt.Errorf("soak: Chaos.DuplicateRate must be in [0,1]")
	}
	return nil
}

// waitGroupDone returns a channel closed when the group drains.
func waitGroupDone(wg *sync.WaitGroup) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// appendErr joins a non-empty detail onto an existing run error.
func appendErr(existing, detail string) string {
	if existing == "" {
		return detail
	}
	return existing + "; " + detail
}
