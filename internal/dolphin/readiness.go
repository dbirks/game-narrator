package dolphin

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// The readiness predicates (docs/spec/11 "Avoiding false positives": "Do not
// declare test passed because Dolphin process existed"). Both are ports of
// the spike's measured gates, with the thresholds the spike calibrated:
//
//	WaitForGameFrame — boot.sh's predicate, spike-faithful: a full-root shot
//	  whose (a) entropy on the render-window crop (the spike's CROP; the full
//	  screen is what dilutes it) is > 0.1 (a blank or crashed window
//	  measures ~0), (b) it differs from the pre-Dolphin baseline over the
//	  full screen (the "bare GUI" boot.sh compared against, AE > 1000 px),
//	  and (c) it differs from the previous shot, which boot.sh also made on
//	  the window crop of BOTH shots when a window exists (AE > 0). ALL THREE
//	  must hold, and the first sample is never accepted (it has no
//	  predecessor): a frozen frame that merely differs from the GUI cannot
//	  pass.
//
//	ProveInput — input.sh's gate: a scene change attributed to a controller
//	  event, proven against a no-input control window taken immediately before
//	  the press inside the same screen visit (the attract loop confounder:
//	  left alone, the game starts its opening movie by itself ~50-90 s after
//	  the title — the very transition the input test looks for). The spike
//	  classified scenes by nearest-neighbour RMSE against 12 retained title
//	  framings; this port makes the control window the reference set, which
//	  needs no retained evidence and costs the same per-sample image math.
//	  The letterbox-band test (title 0.33-0.60, movie 0.000) is kept verbatim
//  because it is the camera-motion-immune scene test.

// Predicate thresholds — the spike's measured values (ADR-0007). The
// readiness conjunction is what makes each clause do work: the t=6 s false
// positive measured entropy 0.138 / vs_baseline 0.0612 / vs_prev 0.0000 and
// is rejected by the motion clause, while a frozen-but-different scene
// (entropy and baseline fine) is rejected by the same clause — the emulated
// GPU must be producing motion, not a frozen window.
const (
	entropyMin    = 0.1  // a rendered scene, not black/blank (boot.sh's ENTROPY_MIN; spike: GUI 0.169, game 0.277)
	vsBaselineMin = 0.01 // "differs from the bare GUI": boot.sh's AE > 1000 px as a screen
	// fraction, debiased by the luma tolerance. A true first frame
	// differs by a large fraction of the screen; a boot-logo frame over the
	// same background the bare GUI shows does not.
	vsPrevMin = 0.001 // "two successive frames differ": boot.sh's AE > 0 px as a
	// fraction, debiased by the luma tolerance. A frozen window
	// measures 0.0000 (the observed false positive); a live 1 s-cadence
	// render moves orders of magnitude more than 0.1% of sampled pixels.
	probePeriod    = 1 * time.Second
	controlSamples = 15 // the no-input control window: the spike's 15 samples at the true 1 s
	// cadence (~15 s), inside the title-only span (the attract
	// movie starts ~50-90 s after the title). The fast grab
	// (ImageMagick import, ~0.1 s) is what keeps the true 1 s cadence;
	// at the 6 s cadence a slow grab forces, the orbit dispersion
	// rises to ~0.31 and the scene threshold to ~0.36, which blunts
	// the crossing the press causes (the bug 4 failure measured
	// exactly that: dispersion 0.3139, threshold 0.3570).
	controlCadence = 1 * time.Second
	// pressBudget bounds how long after the title was FOUND the START press
	// may land. The attract movie starts on its own ~50-90 s after the
	// title — the very transition the input test looks for — so the press
	// and its observation window must finish inside the title-only span:
	// a scene change seen in the input window is then attributable to the
	// press, not to the attract loop. With the 15 s control window plus the
	// pre-press probe the press lands ~17-20 s after the find and the
	// 15 s input window ends ~35 s after — 30 s keeps the margin. A visit
	// older than the budget is discarded, not pressed into: the attract
	// loop brings the title back and the next episode gets a fresh timer.
	pressBudget       = 30 * time.Second
	letterboxTitleMin = 0.25 // title shows sky/sea in the bands (measured 0.32-0.37)
	letterboxMovieMax = 0.10 // the letterboxed opening movie is black there (measured 0.000)
	sceneEntropyMin   = 0.20 // a rendered scene inside the input window
	movieRefDist      = 0.40 // title-vs-movie reference distance (calibrated 0.426); the threshold's far anchor
	crossStepMax      = 0.10 // a settled full-frame screen is stable between 1 s samples (RMSE < 0.10 — the spike's exact value); the title camera orbit moves 0.10-0.40
	maxTitleEpisodes  = 3
	normW, normH      = 320, 240 // the classifier's working size (the spike's 320x240 normalise)
)

// WaitForGameFrame implements Bridge.WaitForGameFrame: it polls the boot
// predicate on a 1 s cadence until it holds or the deadline passes. The
// evidence returned is the measured predicate, not "the process is running":
// frame hashes, the two distances the predicate used, and the wall time.
func (b *bridge) WaitForGameFrame(ctx context.Context, timeout time.Duration) (Evidence, error) {
	b.mu.Lock()
	display := b.st.Display
	baseline := b.baseline
	baselineHash := b.baselineHash
	if display == "" || baseline == nil {
		b.mu.Unlock()
		return Evidence{}, fmt.Errorf("dolphin: WaitForGameFrame needs a launched bridge: %w", ErrNotLaunched)
	}
	b.mu.Unlock()

	deadline := time.Now().Add(timeout)
	started := time.Now()
	var (
		prev    image.Image // the previous FULL shot (the spike keeps shots and crops at compare time)
		lastEnt = -1.0
		lastVsB = -1.0
		lastVsP = -1.0
	)
	for {
		if time.Now().After(deadline) {
			return Evidence{}, fmt.Errorf(
				"dolphin: no game frame within %s (last measured: entropy=%.3f want >%.2f, vs_baseline=%.4f want >%.3f, vs_prev=%.4f want >%.3f) — see the bridge-emu.log in the user dir",
				timeout, lastEnt, entropyMin, lastVsB, vsBaselineMin, lastVsP, vsPrevMin)
		}
		select {
		case <-ctx.Done():
			return Evidence{}, ctx.Err()
		default:
		}

		// The spike's predicate shape (boot.sh): a shot of the WHOLE root —
		// the full-screen comparison is what boot.sh made against the bare-GUI
		// baseline (AE > 1000 px) and the previous shot (AE > 0) — with the
		// entropy measured on the render-window crop when a window resolves
		// (boot.sh's CROP), because a full-screen grab dilutes it. A frame is
		// a game frame only when all three hold on the same shot, and only
		// from the second shot on: the spike never accepts the first sample
		// (it has no predecessor to differ from), so a static frame that
		// merely differs from the GUI can never pass.
		full, err := b.rootGrab(display)
		if err != nil {
			// A failed probe is retried on the next tick: one slow or failed
			// grab is not a lost display.
			select {
			case <-ctx.Done():
				return Evidence{}, ctx.Err()
			case <-time.After(probePeriod):
			}
			continue
		}

		var region Region
		regionOK := false
		if r, rerr := b.currentRegion(display); rerr == nil && r.W > 0 {
			region, regionOK = r, true
		}
		entImg := full
		if regionOK {
			entImg = cropRegion(full, region)
		}
		ent := entropy01(entImg)
		vsB := diffFraction(baseline, full)
		var vsP float64
		if prev != nil {
			// The spike cropped BOTH shots (PREV and CUR) when a window existed,
			// so a window appearing mid-sequence never compares a crop against
			// a full screen.
			a, bimg := prev, full
			if regionOK {
				a, bimg = cropRegion(prev, region), cropRegion(full, region)
			}
			vsP = diffFraction(a, bimg)
		}
		lastEnt, lastVsB, lastVsP = ent, vsB, vsP
		if prev != nil && ent > entropyMin && vsB > vsBaselineMin && vsP > vsPrevMin {
			hash := capture.HashFrame(full)
			ev := Evidence{
				Hashes:    []uint64{hashHead(baselineHash), hashHead(hash)},
				Distances: []float64{vsB, lastVsP},
				At:        time.Now(),
			}
			b.mu.Lock()
			b.st.FirstFrame = ev.At
			b.mu.Unlock()
			b.log.Info("dolphin: first game frame",
				"first_frame_t_seconds", int(time.Since(started).Seconds()),
				"entropy", fmt.Sprintf("%.3f", ent),
				"vs_baseline", fmt.Sprintf("%.4f", vsB),
				"vs_prev", fmt.Sprintf("%.4f", lastVsP),
				"frame_hash", hash)
			return ev, nil
		}
		prev = full
		select {
		case <-ctx.Done():
			return Evidence{}, ctx.Err()
		case <-time.After(probePeriod):
		}
	}
}

// rootGrab grabs the whole root window on display — the full-screen image the
// readiness comparisons are made on (the spike's `shot`).
func (b *bridge) rootGrab(display string) (image.Image, error) {
	b.mu.Lock()
	state := b.st.State
	b.mu.Unlock()
	if state == StateStopped || state == StateFailed {
		return nil, ErrNotLaunched
	}
	w, h, err := b.opts.ScreenSize(display)
	if err != nil || w <= 0 || h <= 0 {
		return nil, fmt.Errorf("dolphin: root size on %s: %w", display, err)
	}
	return b.opts.Grab(display, Region{X: 0, Y: 0, W: w, H: h})
}

// hashHead folds the 64-char frame hash to 8 bytes for Evidence.Hashes.
func hashHead(h string) uint64 {
	if len(h) < 16 {
		return 0
	}
	v, err := strconv.ParseUint(h[:16], 16, 64)
	if err != nil {
		return 0
	}
	return v
}

// sceneProbe is one measured sample of the scene classifier.
type sceneProbe struct {
	t       time.Time
	image   image.Image // the window crop as grabbed (evidence)
	luma    []uint8     // the crop normalised to normW x normH
	lb      float64     // letterbox band
	ent     float64     // entropy
	hash    string
	dist    float64 // RMSE to the nearest reference (filled in phase 3)
	step    float64 // RMSE against the previous probe (the spike's `step`, same 320x240 metric as dist)
	crossed bool
}

// ProveInput implements Bridge.ProveInput: the title-screen START test,
// self-contained (see the package comment for the spike mapping). It returns
// the two distances the report asks for — controlDispersion and
// inputMaxDistance — plus before/control/after PNGs under
// <userDir>/evidence (never the repo).
func (b *bridge) ProveInput(ctx context.Context) (Evidence, error) {
	b.mu.Lock()
	state := b.st.State
	display := b.st.Display
	userDir := b.st.UserDir
	pipe := b.pipe
	b.mu.Unlock()
	if state != StateRunning {
		return Evidence{}, ErrNotRunning
	}
	if pipe == nil {
		return Evidence{}, fmt.Errorf("dolphin: the controller pipe is not connected; cannot prove input: %w", ErrPipeNotConnected)
	}
	// The reader (Dolphin) opens the FIFO during boot, so a ProveInput right
	// after Launch may race the connection: this is a long readiness check,
	// so wait for the connection instead of failing instantly (Inject keeps
	// the instant check — fast feedback for a 50 ms hold).
	if !pipe.waitConnected(ctx, pipeConnectWait) {
		return Evidence{}, fmt.Errorf("dolphin: the controller pipe is not connected (Dolphin has not opened it within %s); cannot prove input: %w", pipeConnectWait, ErrPipeNotConnected)
	}

	// The sample windows: the spike's 15 at a true 1 s cadence, unless the
	// caller tuned them (tests shorten the windows; the cadence rule is what
	// keeps the window honest).
	samples := b.opts.ProveInputSamples
	if samples <= 0 {
		samples = controlSamples
	}
	cadence := b.opts.ProveInputCadence
	if cadence <= 0 {
		cadence = controlCadence
	}

	started := time.Now()

	for episode := 0; episode < maxTitleEpisodes; episode++ {
		// phase 1: seek the full-frame scene (no input is sent while seeking).
		before, beforeHash, err := b.seekScene(ctx, display, cadence)
		if err != nil {
			return Evidence{}, err
		}
		foundAt := time.Now()
		b.log.Info("dolphin: prove input: title scene found",
			"episode", episode+1, "t_seconds", int(time.Since(started).Seconds()))

		// phase 2: the no-input control window. It stops early on the first
		// letterboxed sample (the movie crossing invalidates the episode and
		// shortens the retry cycle).
		ctrl, err := b.sampleWindow(ctx, display, samples, cadence, func(p sceneProbe) bool { return p.lb < letterboxMovieMax })
		if err != nil {
			return Evidence{}, err
		}
		if anyLetterboxed(ctrl) {
			b.log.Warn("dolphin: prove input: control window hit the attract movie; next title visit",
				"episode", episode+1)
			continue
		}
		// The settled crossing rule relies on the title orbit NEVER settling:
		// if the no-input baseline itself shows a stable full-frame scene, a
		// stable post-press scene cannot be attributed to the press. Discard
		// the episode (a rendering stall or an anomalous title) — never
		// accept on it.
		if hasSettledStretch(ctrl) {
			b.log.Warn("dolphin: prove input: the no-input control window itself settled (the title is not orbiting); a stable scene cannot be attributed to the press; next title visit",
				"episode", episode+1)
			continue
		}
		// The spike's CONTROL_MAX: the largest per-sample distance to the
		// CLOSEST other control sample (input.sh measured each no-input sample
		// against the closest known title framing; with the control window as
		// the reference set, the closest other sample is that port). This is
		// the threshold anchor, NOT the max pairwise distance: sample 1 vs
		// sample 15 straddles the whole orbit arc the window spans and is a
		// systematically larger number (measured 0.35 where the spike's
		// per-sample metric measured 0.25) — anchoring the threshold on it
		// blunts the crossing the press causes (the settled save-file prompt
		// sits at ~0.35, below a pairwise-anchored threshold of ~0.38).
		controlDisp := controlMaxDistance(ctrl)
		if controlDisp >= movieRefDist {
			return Evidence{}, fmt.Errorf(
				"dolphin: prove input: the no-input control window already shows a scene change (per-sample max distance %.3f >= %.2f, the title-vs-movie reference); input cannot be attributed",
				controlDisp, movieRefDist)
		}
		// Recompute the threshold from the measured control, midpoint between
		// what the orbit actually did and the title-vs-movie reference: the
		// camera orbit can never fake a crossing (input.sh's rule).
		sceneTH := (controlDisp + movieRefDist) / 2
		if sceneTH <= controlDisp {
			sceneTH = controlDisp + 0.05
		}
		b.log.Info("dolphin: prove input: control window clean",
			"control_dispersion", fmt.Sprintf("%.4f", controlDisp),
			"scene_threshold", fmt.Sprintf("%.4f", sceneTH))

		// phase 3: the press and its observation window. The press must land
		// young enough that the attract movie (earliest ~50 s after the
		// title) cannot start before the input window ends; a visit that is
		// older is discarded — the attract loop brings the title back.
		if since := time.Since(foundAt); since > pressBudget {
			b.log.Warn("dolphin: prove input: title visit too old for a press",
				"episode", episode+1, "since_found", int(since.Seconds()), "budget", int(pressBudget.Seconds()))
			continue
		}
		pre, err := b.probeOne(ctx, display)
		if err != nil {
			return Evidence{}, err
		}
		if preDist := nearestDist(pre, ctrl); preDist >= sceneTH || pre.lb < letterboxTitleMin {
			// The attract movie can start right here — it is the very transition
			// the input test looks for. Do NOT press into it (a change with no
			// input would invalidate the attribution); wait for the title to
			// come back instead.
			b.log.Warn("dolphin: prove input: the game left the title screen before the press; next title visit",
				"episode", episode+1, "dist", fmt.Sprintf("%.4f", preDist), "letterband", fmt.Sprintf("%.3f", pre.lb))
			continue
		}
		if err := b.Inject(ctx, Buttons{Start: true}, Axes{}, 500*time.Millisecond); err != nil {
			return Evidence{}, err
		}
		b.log.Info("dolphin: prove input: pressed START over the Pipes device", "hold_ms", 500)

		in, err := b.sampleWindow(ctx, display, samples, cadence, nil)
		if err != nil {
			return Evidence{}, err
		}
		crossed, inputMax := classifyWindow(in, ctrl, sceneTH)
		var after image.Image
		var afterHash string
		for _, p := range in {
			if p.crossed {
				after, afterHash = p.image, p.hash
				break
			}
		}
		var end image.Image // what the screen actually showed when no crossing was proven
		if len(in) > 0 {
			end = in[len(in)-1].image
		}
		ev := b.storeProveEvidence(userDir, before, ctrl[0].image, after, end)
		ev.Distances = []float64{controlDisp, inputMax}
		ev.Hashes = []uint64{hashHead(beforeHash), hashHead(afterHash)}
		ev.At = time.Now()
		b.log.Info("dolphin: prove input: result",
			"input_max_distance", fmt.Sprintf("%.4f", inputMax),
			"control_dispersion", fmt.Sprintf("%.4f", controlDisp),
			"scene_threshold", fmt.Sprintf("%.4f", sceneTH),
			"two_consecutive_crossed", crossed,
			"t_seconds", int(time.Since(started).Seconds()))
		if !crossed {
			// The failure needs the event trace: which sub-condition (far,
			// letterboxed, settled, adjacency) each sample satisfied, plus the
			// "end" PNG of what the screen actually showed.
			b.log.Warn("dolphin: prove input: input window trace",
				"samples", inputWindowTrace(in, sceneTH))
			return ev, fmt.Errorf(
				"dolphin: prove input: START did not change the scene (max distance %.4f, threshold %.4f; control dispersion %.4f) — input NOT proven accepted",
				inputMax, sceneTH, controlDisp)
		}
		return ev, nil
	}
	return Evidence{}, fmt.Errorf("dolphin: prove input: no usable title visit in %d episodes (the attract loop crossed each control window)", maxTitleEpisodes)
}

// seekScene walks 1 s probes until two consecutive probes show a full-frame
// rendered scene (the title). The letterbox band is what distinguishes the
// title from the opening movie on this game, and the band is camera-motion
// immune — the retained-title-framing classifier of the spike, minus the
// retained frames, which a fresh bridge must not require.
func (b *bridge) seekScene(ctx context.Context, display string, cadence time.Duration) (image.Image, string, error) {
	var prev *sceneProbe
	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		default:
		}
		p, err := b.probeOne(ctx, display)
		if err != nil {
			return nil, "", err
		}
		full := p.lb > letterboxTitleMin && p.ent > sceneEntropyMin
		if full && prev != nil && prev.lb > letterboxTitleMin && prev.ent > sceneEntropyMin {
			img, err := b.currentGrab(display)
			if err != nil {
				return nil, "", err
			}
			return img, p.hash, nil
		}
		prev = &p
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(cadence):
		}
	}
}

// sampleWindow takes n probes at a true cadence: sleep only the remainder of
// each period (the spike's cadence rule — a naive per-sample sleep plus the
// probe's own time would blow the window). stopEarly (nil for the full
// window) stops the window as soon as a probe satisfies it: the control
// window passes the letterbox check, because a movie crossing invalidates the
// episode and nothing is to be learned from the remaining samples — stopping
// early shortens the discarded-episode cycle, which the attract loop's wait
// dominates.
func (b *bridge) sampleWindow(ctx context.Context, display string, n int, cadence time.Duration, stopEarly func(sceneProbe) bool) ([]sceneProbe, error) {
	var out []sceneProbe
	for i := 0; i < n; i++ {
		tick := time.Now()
		p, err := b.probeOne(ctx, display)
		if err != nil {
			return nil, err
		}
		if len(out) > 0 {
			// The spike's `step`: normalised luma RMSE against the previous
			// sample at the working size (crossStepMax was calibrated on
			// exactly this metric, not a pixel-differing fraction).
			p.step = rmseSlice(out[len(out)-1].luma, p.luma)
		}
		out = append(out, p)
		if stopEarly != nil && stopEarly(p) {
			return out, nil
		}
		remain := cadence - time.Since(tick)
		if remain > 20*time.Millisecond {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(remain):
			}
		}
	}
	return out, nil
}

// classifyWindow marks the crossed samples of the input window. A sample
// crosses in one of the spike's two modes:
//   - the letterbox transition (the opening movie, or the fade with
//     rendered content): two CONSECUTIVE samples far from every control
//     sample AND letterboxed with rendered pixels — a single transition
//     frame cannot fake it (input.sh's acceptance rule);
//   - a different SETTLED full-frame screen: two consecutive samples that
//     are a rendered full-frame screen (non-letterboxed bands + content,
//     isSettledScene) stable against their predecessor. The title's camera
//     orbit never settles (it moves >= crossStepMax per sample — the
//     measured 0.10-0.40), so a settled stretch after the press is a new
//     scene even when it sits close in RMSE to the title: the save-file
//     prompt is displayed over the same sky and measures
//     ~0.20 against the control window's nearest framing (which ends 1 s
//     before the press), while the spike's retained-framing reference
//     measured 0.345 from a different orbit phase. The settle case does
//     not need the distance clause for that reason — and the control
//     window is checked per episode to contain no settled stretch of its
//     own (hasSettledStretch), so "the title never settles" is verified
//     on the no-input baseline, not assumed.
func classifyWindow(win, ctrl []sceneProbe, sceneTH float64) (bool, float64) {
	refs := make([][]uint8, 0, len(ctrl))
	for _, c := range ctrl {
		refs = append(refs, c.luma)
	}
	var (
		maxDist     float64
		bothCrossed bool
		prevFar     bool
		prevSettled bool
	)
	for i := range win {
		d := rmseSlice(win[i].luma, nearestSlice(win[i].luma, refs))
		win[i].dist = d
		if d > maxDist {
			maxDist = d
		}
		far := d > sceneTH
		letter := win[i].lb < letterboxMovieMax && win[i].ent > sceneEntropyMin
		settled := isSettledScene(win[i])
		win[i].crossed = (prevFar && far && letter) || (prevSettled && settled)
		prevFar = far
		prevSettled = settled
		if win[i].crossed {
			bothCrossed = true
		}
	}
	return bothCrossed, maxDist
}

// isSettledScene reports whether a sample is a SETTLED full-frame rendered
// screen — a new scene the press produced: rendered content (a pure-black
// fade has none), NON-LETTERBOXED frame bands (the opening movie's bands
// are black, measured 0.000), and stable against the previous 1 s sample
// (the title's camera orbit moves >= crossStepMax per sample — measured
// 0.10-0.40 — and never settles; the no-input control window verifies that
// per episode with hasSettledStretch, which uses this same predicate).
//
// The band clause excludes the MOVIE shape; it is NOT a sky test. The
// post-START screen on this image is the dark memory-card dialog, whose
// frame bands measure 0.166-0.212 (the dialog's dark-gray frame over the
// scene) — below the title's sky/sea bands (0.32-0.56). The original
// sky-band floor (letterboxTitleMin) was calibrated on the unit double's
// bright full-frame scene and rejected the measured real screen: the
// 2026-10-18 real run pressed START, the prompt settled at lb 0.166 /
// d 0.29 / step 0.02, and the input was not proven.
func isSettledScene(p sceneProbe) bool {
	return p.lb >= letterboxMovieMax && p.ent > sceneEntropyMin && p.step < crossStepMax
}

// hasSettledStretch reports whether the window contains two consecutive
// samples that are a stable rendered full-frame screen — the shape of a NEW
// scene, which the title's orbit must never produce while no input is sent
// (the no-input baseline the settled crossing rule relies on). The SAME
// predicate as classifyWindow's settled mode (isSettledScene): a screen the
// input window would accept as the press's result must also invalidate the
// episode when it appears without input.
func hasSettledStretch(win []sceneProbe) bool {
	prev := false
	for i := range win {
		s := isSettledScene(win[i])
		if prev && s {
			return true
		}
		prev = s
	}
	return false
}

func anyLetterboxed(win []sceneProbe) bool {
	for _, p := range win {
		if p.lb < letterboxMovieMax {
			return true
		}
	}
	return false
}

// controlMaxDistance is the spike's CONTROL_MAX: the largest, over the
// control samples, of the distance to the CLOSEST other sample. The spike
// measured every no-input sample against the closest known title framing;
// with the control window as the reference set (no retained evidence), the
// closest other sample is the port of a known framing. A control window is
// "clean" when this stays below the title-vs-movie reference (movieRefDist).
func controlMaxDistance(win []sceneProbe) float64 {
	var max float64
	for i := range win {
		var best float64
		first := true
		for j := range win {
			if i == j {
				continue
			}
			if d := rmseSlice(win[i].luma, win[j].luma); first || d < best {
				best, first = d, false
			}
		}
		if !first && best > max {
			max = best
		}
	}
	return max
}

func nearestDist(p sceneProbe, ctrl []sceneProbe) float64 {
	var best float64
	first := true
	for _, c := range ctrl {
		d := rmseSlice(p.luma, c.luma)
		if first || d < best {
			best = d
			first = false
		}
	}
	return best
}

// probeOne grabs, normalises and classifies one sample. The stored image is
// the render-window crop the evidence report asks humans to look at —
// numbers alone are not accepted (ADR-0007).
func (b *bridge) probeOne(ctx context.Context, display string) (sceneProbe, error) {
	if ctx.Err() != nil {
		return sceneProbe{}, ctx.Err()
	}
	img, err := b.currentGrab(display)
	if err != nil {
		return sceneProbe{}, err
	}
	return sceneProbe{
		t:     time.Now(),
		image: img,
		luma:  normalise(img, normW, normH),
		lb:    letterBand01(img),
		ent:   entropy01(img),
		hash:  capture.HashFrame(img),
	}, nil
}

// currentGrab is Screenshot minus the state check: window region when it
// resolves, full screen before the window exists.
func (b *bridge) currentGrab(display string) (image.Image, error) {
	b.mu.Lock()
	state := b.st.State
	b.mu.Unlock()
	if state == StateStopped || state == StateFailed {
		return nil, ErrNotLaunched
	}
	region, err := b.currentRegion(display)
	if err != nil {
		w, h, serr := b.opts.ScreenSize(display)
		if serr != nil || w <= 0 {
			return nil, err
		}
		region = Region{X: 0, Y: 0, W: w, H: h}
	}
	return b.opts.Grab(display, region)
}

// normalise resizes an image to w x h luma (nearest neighbour).
func normalise(img image.Image, w, h int) []uint8 {
	_, _, px := luma(img)
	if px == nil {
		return nil
	}
	r := img.Bounds()
	if r.Dx() <= 0 || r.Dy() <= 0 {
		return nil
	}
	out := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		sy := y * r.Dy() / h
		for x := 0; x < w; x++ {
			sx := x * r.Dx() / w
			out[y*w+x] = px[sy*r.Dx()+sx]
		}
	}
	return out
}

// rmseSlice is the normalised (÷255) luma RMSE of two equal-size buffers —
// the spike's "%[RMSE]" at its working size, so the calibrated thresholds
// (title orbit 0.10-0.40, title-vs-movie 0.426) stay directly comparable.
func rmseSlice(a, b []uint8) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var sum float64
	for i := range a {
		d := int(a[i]) - int(b[i])
		sum += float64(d * d)
	}
	return math.Sqrt(sum/float64(len(a))) / 255.0
}

func nearestSlice(x []uint8, refs [][]uint8) []uint8 {
	var best []uint8
	var bestD float64
	first := true
	for _, r := range refs {
		d := rmseSlice(x, r)
		if first || d < bestD {
			best, bestD, first = r, d, false
		}
	}
	if best == nil {
		return x
	}
	return best
}

// inputWindowTrace renders the classifier's per-sample decision for the
// input window (the event trace a NOT PROVEN result needs): per sample the
// distance to the closest control sample, the letterbox band, the entropy,
// the step against its predecessor, and which sub-conditions held.
func inputWindowTrace(win []sceneProbe, sceneTH float64) string {
	parts := make([]string, len(win))
	for i := range win {
		far := win[i].dist > sceneTH
		letter := win[i].lb < letterboxMovieMax && win[i].ent > sceneEntropyMin
		settled := win[i].lb > letterboxTitleMin && win[i].step < crossStepMax
		parts[i] = fmt.Sprintf("%d[d=%.3f lb=%.3f ent=%.2f step=%.3f far=%v letter=%v settled=%v cross=%v]",
			i+1, win[i].dist, win[i].lb, win[i].ent, win[i].step, far, letter, settled, win[i].crossed)
	}
	return strings.Join(parts, " ")
}

// storeProveEvidence writes the before/control/after PNGs (and, when no
// crossing was proven, the "end" frame the screen actually showed) under the
// user dir's evidence tree (never the repo — the screenshots contain
// copyrighted gameplay and the task boundary forbids them in the tree).
func (b *bridge) storeProveEvidence(userDir string, before, control, after, end image.Image) Evidence {
	ev := Evidence{Files: map[string]string{}}
	if userDir == "" || before == nil {
		return ev
	}
	dir := filepath.Join(userDir, "evidence", "proveinput-"+time.Now().UTC().Format("20060102T150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ev
	}
	for label, img := range map[string]image.Image{"before": before, "control": control, "after": after, "end": end} {
		if img == nil {
			continue
		}
		f, err := os.Create(filepath.Join(dir, label+".png"))
		if err != nil {
			continue
		}
		err = png.Encode(f, img)
		_ = f.Close()
		if err == nil {
			ev.Files[label] = f.Name()
		}
	}
	return ev
}
