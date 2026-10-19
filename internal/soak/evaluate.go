// evaluate.go is the soak's pass criteria, asserted at the end of the run:
// no panics, the RSS growth bound and the monotonic-growth (slope) detector,
// the goroutine/FD return-to-baseline checks, fault recovery (a commit lands
// after every fault within the bound), and the scenario assertions over the
// per-pass timeline slices (no stale speech and friends — the scenario's
// evaluators, reused: the sidecar's own session events are that layer's
// commitment of record, sliced on the pass grid the sidecar's clock owns).

package soak

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	gnrt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/scenario"
)

// atomicInt64 is the counter type the per-key maps hold.
type atomicInt64 = atomic.Int64

// numGoroutines is the runtime's own count (the sampler's row).
func numGoroutines() int { return runtime.NumGoroutine() }

// waitTimeout waits for the channel to close or the bound to elapse.
func waitTimeout(done <-chan struct{}, bound time.Duration) error {
	select {
	case <-done:
		return nil
	case <-time.After(bound):
		return fmt.Errorf("timed out after %s", bound)
	}
}

// takeSample is the sampler's function: one row of the resource table plus
// the run's cumulative counters at the moment.
func (h *harness) takeSample() sample {
	st := h.runner.Stats()
	c := h.counters()
	return sample{
		T:               time.Now(),
		RSSKB:           rssKB(),
		Goroutines:      numGoroutines(),
		FDs:             fdCount(),
		Commits:         int64(st.Commits),
		SidecarCommits:  c.SidecarCommits,
		FramesDelivered: st.FramesDelivered,
		FramesDropped:   st.FramesDropped,
		SpeechStarted:   c.SpeechStarted,
		SpeechEnded:     c.SpeechEnded,
		ProviderErrors:  c.ProviderErrors,
		FaultsFired:     c.FaultsFired,
		RecoveredPanics: c.Panics,
	}
}

// collect fills the Result's run evidence from the finished harness.
func (r *Result) collect(h *harness) {
	r.Seed = h.cfg.Chaos.Seed
	r.Config = h.cfg
	r.Pipeline = h.runner.Stats()
	// The run's own per-stage latency summary (docs/spec/16 M7): the same
	// shared function the session API, the support bundle and the developer
	// page use, over the run's own stage stats. Wall-clock diagnostic, not a
	// pass criterion (ADR-0064's class of number).
	r.Bottlenecks = gnrt.Bottlenecks(r.Pipeline.Latency)
	r.Session = h.sess.Stats()
	r.Faults = h.faultKindCounts()
	r.ProviderErrors = h.errKindCounts()
	h.faultEventsMu.Lock()
	r.FaultEvents = append([]faultEvent(nil), h.faultEvents...)
	h.faultEventsMu.Unlock()
	r.SourceDrops = int64(h.src.Stats().DroppedSlot)
	// The panic count covers the harness, the pipeline and both fault
	// injectors (a panic inside an injector is recovered and counted there).
	r.Panics = h.panics.Load()
	if h.decGW != nil {
		r.Panics += int64(h.decGW.panicCount())
	}
	if h.ttsGW != nil {
		r.Panics += int64(h.ttsGW.panicCount())
	}
	n, failures := r.evaluatePasses(h)
	r.PassesEvaluated = n
	r.PassFailures = failures
}

// faultKindCounts renders the per-kind fault counters.
func (h *harness) faultKindCounts() map[string]int64 {
	out := map[string]int64{}
	h.faultKinds.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomicInt64).Load()
		return true
	})
	return out
}

// errKindCounts renders the per-kind provider error counters.
func (h *harness) errKindCounts() map[string]int64 {
	out := map[string]int64{}
	h.errKinds.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomicInt64).Load()
		return true
	})
	return out
}

// evaluatePasses finishes the pass grading the settle path started
// incrementally (settle.go): every slice settled during the run was already
// graded and dropped, so what remains is the run-end residue — the last
// passBound or so passes, whose utterances finishUtterances just closed.
// The run-level stale-generation check runs here on the final pipeline
// stats: it is a cumulative counter, so the old per-slice form (every slice
// graded against the same global number) is replaced by the one check it
// always meant. Returns the number of slices graded (across the whole run)
// and the failing details.
func (r *Result) evaluatePasses(h *harness) (int, []string) {
	_, failures := h.gradeRemaining()
	// The run-level stale-generation check (the scenario's evaluator, on the
	// final stats): a message applied despite naming a stale generation is a
	// fence failure the run must name, once, against the run — not once per
	// slice.
	sc := &scenario.Scenario{ID: "soak-run", Assertions: []scenario.Assertion{{Type: "no_stale_generation_applied"}}}
	rep := sc.Evaluate(nil, r.Pipeline, nil)
	for _, a := range rep.Results {
		if !a.Passed {
			failures = append(failures, a.Type+": "+a.Detail)
		}
	}
	h.passFailuresMu.Lock()
	failures = append(failures, h.passFailures...)
	h.passFailuresMu.Unlock()
	return h.passesGraded, failures
}

// gradeRemaining grades the pass slices still pending at run end (the residue
// the settle path left) and releases them. Returns the slices graded and
// their failures. The slices are graded as-is: a stuck utterance the residue
// still carries is exactly the voice-contract failure it owes.
func (h *harness) gradeRemaining() (int, []string) {
	var failures []string
	var stamps []passStamp
	h.passMu.Lock()
	for stamp, ps := range h.passStates {
		if len(ps.events) > 0 {
			stamps = append(stamps, stamp)
		}
	}
	h.passMu.Unlock()
	sort.Slice(stamps, func(i, j int) bool { return stampLess(stamps[i], stamps[j]) })
	n := 0
	for _, stamp := range stamps {
		h.passMu.Lock()
		ps := h.passStates[stamp]
		h.passMu.Unlock()
		if ps == nil || len(ps.events) == 0 {
			continue
		}
		failures = append(failures, h.gradeSlice(stamp, ps.events)...)
		h.passMu.Lock()
		h.passesGraded++
		if passHasCommit(ps.events) {
			h.passCommitSlices++
		}
		h.releaseSliceLocked(ps)
		h.passMu.Unlock()
		n++
	}
	return n, failures
}

// voiceContractOffenders is the loop-aware form of the no_voice_substitution
// contract for one pass slice. Every utterance that started in the pass (a
// speech.ready or speech.started event) must reach a terminal outcome in the
// same slice — speech.ended (result "ended", or "failed" carrying the voice
// and the reason in error) or speech.cancelled (carrying the fence's reason),
// the same terminal contract the runtime speech scheduler got in ADR-0039 —
// and no speech event may name a voice other than the configured one (the
// substitution check itself, unchanged). The terminal is judged per
// occurrence, never as "some good end exists": on a looped pass the clip's
// own box dismissal at the end of the pass (dialogue-advance-001 rows
// 22-23) legitimately cancels the last line's utterance, and a chaos-
// delayed decision lost to the next advance commits with no voice events at
// all (an abstain, docs/spec/06) — both healthy, both terminal.
func voiceContractOffenders(evs []events.Envelope, voice string) []string {
	started, terminal := map[string]bool{}, map[string]bool{}
	var out []string
	for _, e := range evs {
		switch e.Name {
		case events.SpeechReady, events.SpeechStarted, events.SpeechEnded, events.SpeechCancelled:
		default:
			continue
		}
		var p struct {
			Occurrence string `json:"dialogue_occurrence_id"`
			VoiceID    string `json:"voice_id"`
		}
		if err := json.Unmarshal(e.Data, &p); err != nil {
			continue
		}
		if p.VoiceID != "" && p.VoiceID != voice {
			out = append(out, fmt.Sprintf("%s names voice %q (configured %q)", e.Name, p.VoiceID, voice))
		}
		switch e.Name {
		case events.SpeechReady, events.SpeechStarted:
			if p.Occurrence != "" {
				started[p.Occurrence] = true
			}
		case events.SpeechEnded, events.SpeechCancelled:
			if p.Occurrence != "" {
				terminal[p.Occurrence] = true
			}
		}
	}
	for occ := range started {
		if !terminal[occ] {
			out = append(out, fmt.Sprintf("utterance %s started but reached no terminal in this pass (want speech.ended or speech.cancelled)", occ))
		}
	}
	sort.Strings(out)
	return out
}

// perPassAssertions is the assertion set one pass slice is graded against:
// the loop-safe half of the scenario vocabulary. no_voice_substitution is
// not reused per pass: its terminal requirement (a good speech.ended must
// exist in the slice) is unsound on a looped pass (see
// voiceContractOffenders), and its substitution half is kept there instead.
// no_stale_generation_applied is not reused per pass either: it reads the
// cumulative pipeline counter, a run-level fact the run-level check in
// evaluatePasses grades once.
func perPassAssertions(lineIDs []string, evs []events.Envelope) []scenario.Assertion {
	as := []scenario.Assertion{
		{Type: "no_duplicate_commit"},
		{Type: "committed_only", Names: lineIDs},
	}
	for _, id := range lineIDs {
		as = append(as, scenario.Assertion{Type: "no_duplicate_speech", MessageID: id})
	}
	return as
}

// passHasSpeech reports whether the slice carries any speech lifecycle event.
func passHasSpeech(evs []events.Envelope) bool {
	for _, e := range evs {
		switch e.Name {
		case events.SpeechReady, events.SpeechStarted, events.SpeechCancelled, events.SpeechEnded:
			return true
		}
	}
	return false
}

// staleSpeechOffenders is the loop-aware form of docs/spec/12's "zero stale
// audio starts after a new committed dialogue event": a speech.started whose
// most recent preceding message.committed (wall order, on the events'
// OccurredAt) names a different line is an offender — the dialogue is on
// another line and this audio is starting anyway. The session's generation
// fence is what keeps this list empty: a reply that lands after the advance
// is stale and refused, never applied. The slice is re-sorted on OccurredAt
// first because the slice is built in append order, and a chaos-delayed
// decision can file a started event after the next commit's.
func staleSpeechOffenders(evs []events.Envelope) []string {
	ordered := make([]events.Envelope, len(evs))
	copy(ordered, evs)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].OccurredAt.Before(ordered[j].OccurredAt) })
	var out []string
	lastCommit := ""
	for _, e := range ordered {
		switch e.Name {
		case events.MessageCommitted:
			var p committedPayload
			if err := json.Unmarshal(e.Data, &p); err == nil {
				lastCommit = p.MessageID
			}
		case events.SpeechStarted:
			var p struct {
				MessageID string `json:"message_id"`
			}
			if err := json.Unmarshal(e.Data, &p); err != nil {
				continue
			}
			if p.MessageID != "" && lastCommit != "" && p.MessageID != lastCommit {
				out = append(out, fmt.Sprintf("speech.started for %q after %q committed", p.MessageID, lastCommit))
			}
		}
	}
	return out
}

// evaluate asserts the run-level pass criteria.
func (r *Result) evaluate() {
	r.Verdicts = append(r.Verdicts, noPanicsVerdict(r))
	r.Verdicts = append(r.Verdicts, rssGrowthVerdict(r))
	r.Verdicts = append(r.Verdicts, rssSlopeVerdict(r))
	r.Verdicts = append(r.Verdicts, goroutineReturnVerdict(r))
	r.Verdicts = append(r.Verdicts, fdReturnVerdict(r))
	r.Verdicts = append(r.Verdicts, faultRecoveryVerdict(r))
	r.Verdicts = append(r.Verdicts, passAssertionVerdict(r))
	if r.RunErr != "" {
		r.Verdicts = append(r.Verdicts, Verdict{Name: "run_completed", Passed: false, Detail: r.RunErr})
	}
}

// noPanicsVerdict: not one recovered panic anywhere in the harness.
func noPanicsVerdict(r *Result) Verdict {
	n := r.panicsTotal()
	if n == 0 {
		return Verdict{Name: "no_panics", Passed: true, Detail: "no recovered panics (harness, gateways, pipeline)"}
	}
	return Verdict{Name: "no_panics", Passed: false, Detail: fmt.Sprintf("%d recovered panic(s)", n)}
}

// postWarmupSamples trims the run's warm-up: the samples at or after the
// warm-up mark (10% of the run, 30 s floor — see warmUpMark), plus the
// final sample (the growth window's end point).
func (r *Result) postWarmupSamples() []sample {
	if len(r.Samples) == 0 {
		return nil
	}
	cut := r.Baseline.T.Add(warmUpMark(r.Config))
	var out []sample
	for _, s := range r.Samples {
		if !s.T.Before(cut) {
			out = append(out, s)
		}
	}
	return out
}

// rssGrowthVerdict: growth from the 10% mark to the end, bounded both
// relatively (20%) and absolutely (1 GiB, the docs/spec/10 4 GB box).
func rssGrowthVerdict(r *Result) Verdict {
	pts := r.postWarmupSamples()
	if len(pts) < 2 || pts[0].RSSKB == 0 {
		return Verdict{Name: "rss_growth", Passed: false, Detail: "RSS not measurable (no /proc/self/status, or < 2 post-warm-up samples)"}
	}
	first, last := pts[0].RSSKB, pts[len(pts)-1].RSSKB
	delta := last - first
	growthPct := float64(delta) / float64(first) * 100
	if delta < 0 {
		delta = 0 // a GC shrink is not a violation
	}
	ok := growthPct < maxRSSGrowthPct && int64(delta)*1024 < maxRSSGrowthBytes
	detail := fmt.Sprintf("RSS %d -> %d KiB over %s (%.1f%% growth, bound %.0f%% and %d MiB)",
		first, last, lastSampleGap(pts).Round(time.Second), growthPct, maxRSSGrowthPct, maxRSSGrowthBytes>>20)
	if !ok {
		return Verdict{Name: "rss_growth", Passed: false, Detail: detail}
	}
	return Verdict{Name: "rss_growth", Passed: true, Detail: detail}
}

// lastSampleGap is the warm-up window's span for the detail line.
func lastSampleGap(pts []sample) time.Duration {
	return pts[len(pts)-1].T.Sub(pts[0].T)
}

// rssSlopeVerdict: the monotonic-growth detector — the Theil–Sen slope (median
// of the pairwise slopes) of the post-warm-up VmRSS, bounded at the growth
// rate that would consume the whole 1 GiB budget over the window
// (rssSlopeBound). A steady leak lifts every sample together, so its median
// pairwise slope lands at the leak rate; zero-drift GC jitter oscillates
// symmetrically, so its median pairwise slope is ≈ 0. A one-time arena step is
// a level shift, not a sustained rate: its contribution to the median decays
// as the window lengthens, which is exactly why the bound is calibrated to the
// window rather than held at a fixed short-run rate.
func rssSlopeVerdict(r *Result) Verdict {
	pts := r.postWarmupSamples()
	snaps := make([]snap, 0, len(pts))
	for _, p := range pts {
		snaps = append(snaps, snap{t: p.T, rss: p.RSSKB})
	}
	bound := rssSlopeBound(lastSampleGap(pts))
	slope := leakSlopeMiBPerMin(snaps)
	detail := fmt.Sprintf("RSS slope %.2f MiB/min over %d post-warm-up samples (bound %.1f = 1 GiB / window)",
		slope, len(pts), bound)
	if len(pts) < 3 {
		return Verdict{Name: "rss_slope", Passed: true, Detail: detail + " (fit not run: too few samples)"}
	}
	if lastSampleGap(pts) < slopeMinWindow {
		// The fit is reported (summary line, report) but not graded: over a
		// span this short a one-time Go-arena step or a GC sawtooth of tens of
		// MiB reads as 30+ MiB/min — at or past the bound — while a real leak
		// at the bound rate has moved only a few tens of MiB. The post-warm-up
		// tail of a 100 s run also still carries warm-up under a loaded
		// machine. The rss_growth level bound (20% / 1 GiB over the same
		// window) still grades this run; the slope detector grades the windows
		// it can resolve (the 1h/2h gates: 54/105 min post-warm-up).
		return Verdict{Name: "rss_slope", Passed: true, Detail: detail + fmt.Sprintf(
			" (fit not graded: post-warm-up window %s < %s minimum — at this resolution a one-time arena step or GC sawtooth is indistinguishable from a leak; the rss_growth level bound still applies)",
			lastSampleGap(pts).Round(time.Second), slopeMinWindow)}
	}
	ok := slope < bound
	if !ok {
		return Verdict{Name: "rss_slope", Passed: false, Detail: detail}
	}
	return Verdict{Name: "rss_slope", Passed: true, Detail: detail}
}

// rssSlopeBound is the leak-detector slope bound for one soak window: the
// growth rate that would consume the entire maxRSSGrowthBytes budget over the
// window (1 GiB / window). A window under a minute is clamped to one minute so
// the division stays meaningful and a short soak's bound is not absurdly high.
func rssSlopeBound(window time.Duration) float64 {
	if window < time.Minute {
		window = time.Minute
	}
	return float64(maxRSSGrowthBytes>>20) / window.Minutes() // MiB / min
}

// goroutineReturnVerdict: the goroutine count returns to the pre-run
// baseline plus the steady-state slop after the full teardown.
func goroutineReturnVerdict(r *Result) Verdict {
	delta := r.Final.Goroutines - r.Baseline.Goroutines
	ok := delta <= goroutineFDSlop
	return Verdict{
		Name:   "goroutines_return",
		Passed: ok,
		Detail: fmt.Sprintf("goroutines %d -> %d (delta %d, slop %d)", r.Baseline.Goroutines, r.Final.Goroutines, delta, goroutineFDSlop),
	}
}

// fdReturnVerdict: the open-FD count returns to baseline plus slop — a
// per-loop FD leak would show as a delta far above the listener and idle
// connection allowance.
func fdReturnVerdict(r *Result) Verdict {
	delta := r.Final.FDs - r.Baseline.FDs
	ok := delta <= goroutineFDSlop
	return Verdict{
		Name:   "fds_return",
		Passed: ok,
		Detail: fmt.Sprintf("fds %d -> %d (delta %d, slop %d)", r.Baseline.FDs, r.Final.FDs, delta, goroutineFDSlop),
	}
}

// faultRecoveryVerdict: every injected fault whose recovery window closed
// inside the run is followed by recovery — a commit lands after it within
// the bound. The bound is measured on the pipeline's commit counter (the
// local chain's commitment), which the provider faults are not allowed to
// stop.
//
// Resolution-aware: a fault fired within faultRecoveryBound of the run's end
// has an OPEN window. The harness stops the pipeline at run end — the run's
// timer, not the fault — and the commit counter freezes with it, so "no
// commit in the remainder" is the run's stop, not evidence of a missing
// recovery; under CPU contention the pipeline's commit rate is also slower,
// so the tail window is even less informative. The old form clamped the
// window to the run's end and graded the tail anyway, which failed the
// 100 s CI soak on faults the run itself outran. Those faults are reported
// (Judged=false; the fault table says "open window") and excluded from the
// verdict; every fault whose window closed inside the run is graded exactly
// as before — same bound, same evidence.
func faultRecoveryVerdict(r *Result) Verdict {
	if len(r.FaultEvents) == 0 {
		return Verdict{Name: "fault_recovery", Passed: true, Detail: "no faults injected"}
	}
	var bad []string
	var judged, open int
	for i := range r.FaultEvents {
		ev := &r.FaultEvents[i]
		windowClose := ev.Wall.Add(faultRecoveryBound)
		if windowClose.After(r.Final.T) {
			// Open window: the run ended inside the recovery bound. The
			// pipeline was stopped by the run, not the fault.
			ev.Judged = false
			ev.Recovered = false
			open++
			continue
		}
		ev.Judged = true
		judged++
		c := commitsAtOrAfter(r, windowClose)
		ev.Recovered = c > ev.CommitsAtFire
		ev.WaitMS = waitForCommits(r, ev, c)
		if !ev.Recovered {
			bad = append(bad, fmt.Sprintf("%s@%s: %d commits at fault, %d by +%.0fs",
				ev.Kind, ev.Wall.Format("15:04:05"), ev.CommitsAtFire, c, faultRecoveryBound.Seconds()))
		}
	}
	detail := fmt.Sprintf("%d fault(s) graded (window closed inside the run), every one followed by a commit within %s", judged, faultRecoveryBound)
	if open > 0 {
		detail += fmt.Sprintf("; %d fault(s) not graded: recovery window open at run end (the run's stop freezes the commit counter; see the fault table)", open)
	}
	if len(bad) > 0 {
		return Verdict{Name: "fault_recovery", Passed: false, Detail: fmt.Sprintf("%d/%d graded faults without recovery: %s",
			len(bad), judged, join(bad, 4))}
	}
	return Verdict{Name: "fault_recovery", Passed: true, Detail: detail}
}

// commitsAtOrAfter is the pipeline's commit count at the first sample at or
// after t, or the final pipeline count when t is past the last sample.
func commitsAtOrAfter(r *Result, t time.Time) uint64 {
	for _, s := range r.Samples {
		if !s.T.Before(t) {
			return uint64(s.Commits)
		}
	}
	return r.Pipeline.Commits
}

// waitForCommits estimates how long after the fault the recovery commit was
// first observed (from the sample grid; the report's evidence for the bound).
func waitForCommits(r *Result, ev *faultEvent, target uint64) int64 {
	if target <= ev.CommitsAtFire {
		return 0
	}
	for _, s := range r.Samples {
		if s.T.Before(ev.Wall) {
			continue
		}
		if uint64(s.Commits) > ev.CommitsAtFire {
			return s.T.Sub(ev.Wall).Milliseconds()
		}
	}
	return faultRecoveryBound.Milliseconds()
}

// passAssertionVerdict folds the per-pass scenario assertions into one
// verdict: the stale-speech and duplicate checks hold on every pass.
func passAssertionVerdict(r *Result) Verdict {
	if r.PassesEvaluated == 0 {
		return Verdict{Name: "pass_assertions", Passed: true, Detail: "no complete pass (run shorter than one pass)"}
	}
	if len(r.PassFailures) > 0 {
		return Verdict{Name: "pass_assertions", Passed: false,
			Detail: fmt.Sprintf("%d failing assertion(s) over %d passes: %s", len(r.PassFailures), r.PassesEvaluated, join(r.PassFailures, 3))}
	}
	return Verdict{Name: "pass_assertions", Passed: true,
		Detail: fmt.Sprintf("%d pass slice(s) clean (no stale speech, no duplicate commit/speech, committed only, voice contract: no substitution, every started utterance terminates)", r.PassesEvaluated)}
}

// panicsTotal is the recovered-panic count (harness, gateways) as recorded
// in the Result.
func (r *Result) panicsTotal() int64 {
	return r.Panics
}

// FaultsTotal is the total fault activations, all kinds.
func (r *Result) FaultsTotal() int64 {
	var n int64
	for _, v := range r.Faults {
		n += v
	}
	return n
}

// ProviderErrorsTotal is the total provider errors, all kinds.
func (r *Result) ProviderErrorsTotal() int64 {
	var n int64
	for _, v := range r.ProviderErrors {
		n += v
	}
	return n
}

// firstFailure names the first failing verdict for the summary line.
func (r *Result) firstFailure() string {
	for _, v := range r.Verdicts {
		if !v.Passed {
			return v.Name
		}
	}
	return "run_error"
}

// FirstFailure is the exported view of the summary's failure name.
func (r *Result) FirstFailure() string { return r.firstFailure() }

// join takes at most n items of the slice for a bounded detail line.
func join(items []string, n int) string {
	if len(items) > n {
		items = items[:n]
	}
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	if len(items) > n {
		out += fmt.Sprintf(" (+%d more)", len(items)-n)
	}
	return out
}

// itoa is the small int->string the pass labels need.
func itoa(i int) string { return fmt.Sprintf("%d", i) }
