package dolphin

import (
	"context"
	"fmt"
	"image"
	"time"
)

// The CLOSE step (issue #12, the measured 2026-10-20 recipe,
// artifacts/runs/dolphin-item-sign-2026-10-20/README.md): after the
// first-dialogue restore, the on-screen dialogue advances to the gameplay
// scene on A presses, and the full-frame gameplay scene is detected by its
// bright frame bands once the dialogue is over. Two measured facts shape the
// policy:
//
//   - the restore's OPENING frame is the saved deck scene WITHOUT the box
//     (the box's typewriter reveal takes 1-2 s): a full-frame bright-band
//     frame that is NOT the gameplay scene, so the detector must not be
//     allowed to fire before the boot guard (30 s) has elapsed (the box
//     itself holds for 10+ minutes, so A presses during the guard advance
//     the dialogue exactly as they do after it — the guard gates DETECTION
//     only);
//   - the dialogue/cutscene frames (the box over the dark deck scene, the
//     letterboxed farewell captions) measure band < 0.15 where the settled
//     gameplay scene measures 0.25-0.29 (the measured evidence:
//     dialogue-mid/farewell-caption lb 0.000; the deck frames 0.255-0.296).
//     The detector needs FULL-FRAME band > 0.15 on three samples taken at
//     the 2 s cadence (2 s apart), so a single bright transition frame
//     cannot fire it.
//
// The policy is split from the live loop so the timing logic is unit
// testable: CloseDetector holds the guard/streak state and CloseAdvance is
// the screen-driven loop that feeds it (screenshot -> classify -> press A
// when the screen is not the full-frame gameplay scene).

const (
	// closeBandMin is the full-frame band floor: 0.15 sits between the
	// dialogue/cutscene measurements (0.000) and the gameplay scene
	// (0.25-0.29) with margin on both sides (letterboxTitleMin is 0.25 —
	// the title's own floor; the gameplay scene measures BELOW the title
	// band, so the close floor is lower and scene-specific).
	closeBandMin = 0.15
	// closeBootGuard is the no-detection window after the first sample:
	// the restore's opening frame is the saved scene before the box's
	// reveal (the measured letterboxed-captions opening the README records
	// as the reason the guard exists). 30 s also bounds how far the
	// detector may sit behind the scene's real arrival (the advance takes
	// ~35 s, so the guard elapses before the dialogue is over — presses
	// are not gated by it, only detection).
	closeBootGuard = 30 * time.Second
	// closeSamples is the stable-window width: three 2 s-cadence samples.
	closeSamples = 3
	// closeSampleInterval is the cadence the detector samples at: 2 s
	// (the drive/fixture cadence). "Samples 2 s apart" = consecutive
	// samples of the cadence.
	closeSampleInterval = 2 * time.Second
	// closePressInterval is the A-press spacing: 2.5 s (the measured
	// recipe; the 500 ms press hold leaves 2 s of the 2.5 s to the
	// game's own advance animation before the next press may land).
	closePressInterval = 2500 * time.Millisecond
	// closeDeadline bounds the whole advance: the measured recipe takes
	// ~40 s; 8 m is a 12x budget for a slow llvmpipe run.
	closeDeadline = 8 * time.Minute
)

// ClosePhase is the detector's current phase.
type ClosePhase int

const (
	// ClosePhaseAdvance: the dialogue/cutscene is still running — the caller
	// keeps pressing A on the press cadence to advance it.
	ClosePhaseAdvance ClosePhase = iota
	// ClosePhaseSettle: the full-frame gameplay scene has appeared — the
	// caller STOPS pressing (pressing A on the deck triggers a letterbox
	// blink, measured 2026-10-20) and samples at the settle cadence until
	// the scene is confirmed stable.
	ClosePhaseSettle
)

// CloseDetector decides when the screen has settled into the full-frame
// gameplay scene from (timestamp, band) samples. It is the pure timing core
// of the CLOSE step:
//
//   - the boot guard: no settle detection before closeBootGuard after the
//     first sample (the restore's opening frame is the saved scene before
//     the box reveal — full-frame, not the deck);
//   - the phase transition: the first full-frame (band > closeBandMin)
//     sample after the guard moves to Settle (presses stop);
//   - the settle streak: three consecutive full-frame samples, each at least
//     closeSampleInterval after the last counted one, confirm the scene is
//     stable; any below-floor sample resets it and returns to Advance.
//
// It is stateless across calls except its own window — one instance per close
// run, fed in order. Observe reports the phase the caller should act in next
// and whether the settle window has completed.
type CloseDetector struct {
	started     time.Time // the first sample's time (zero until the first)
	guardAt     time.Time // started + the boot guard
	phase       ClosePhase
	streak      int       // consecutive band>min samples at the cadence
	lastCounted time.Time // the time of the last sample counted toward the streak
}

// Observe feeds one sample and reports (the phase to act in next, whether the
// settled window has completed). A below-floor sample is "not full-frame": it
// resets the streak and holds/returns to Advance. The caller's screenshot
// error path should skip Observe entirely (a lost sample is not a scene
// change).
func (d *CloseDetector) Observe(now time.Time, band float64) (ClosePhase, bool) {
	if d.started.IsZero() {
		d.started = now
		d.guardAt = now.Add(closeBootGuard)
		return ClosePhaseAdvance, false
	}
	if now.Before(d.guardAt) {
		// Inside the boot guard: the opening frame is the saved scene before
		// the box reveal. Advance only — never settle.
		return ClosePhaseAdvance, false
	}
	fullFrame := band > closeBandMin
	if !fullFrame {
		// The dialogue/cutscene is still running (or that was a passing
		// full-frame glimpse): advance, and any settle streak is void.
		d.phase = ClosePhaseAdvance
		d.streak = 0
		d.lastCounted = now
		return d.phase, false
	}
	// Cadence floor: count a sample only if it is at least closeSampleInterval
	// after the last counted one (a burst of fast samples cannot fake a
	// window).
	if !d.lastCounted.IsZero() && now.Before(d.lastCounted.Add(closeSampleInterval)) {
		return d.phase, d.phase == ClosePhaseSettle && d.streak >= closeSamples
	}
	d.lastCounted = now
	if d.phase == ClosePhaseSettle {
		d.streak++
		return d.phase, d.streak >= closeSamples
	}
	// The first full-frame sample after the guard: the gameplay scene has
	// arrived. Stop pressing (pressing A on the deck triggers the letterbox
	// blink) and begin the settle window.
	d.phase = ClosePhaseSettle
	d.streak = 1
	return d.phase, d.streak >= closeSamples
}

// Detail renders the detector's state for the per-sample trace.
func (d *CloseDetector) Detail(band float64) string {
	guard := "pending"
	if !d.started.IsZero() {
		if time.Now().Before(d.guardAt) {
			guard = fmt.Sprintf("%s left", d.guardAt.Sub(time.Now()).Round(time.Second))
		} else {
			guard = "elapsed"
		}
	}
	phase := "advance"
	if d.phase == ClosePhaseSettle {
		phase = "settle"
	}
	return fmt.Sprintf("lb=%.3f want > %.2f; phase=%s streak=%d/%d (guard %s)",
		band, closeBandMin, phase, d.streak, closeSamples, guard)
}

// CloseAdvance is the screen-driven CLOSE loop (the issue #12 step 1):
// from the first-dialogue restore, press A on the 2.5 s cadence while the
// screen is not the settled full-frame gameplay scene (band > closeBandMin
// on three 2 s samples, after the boot guard), and stop with the saved
// scene's reference frame when it is. The reference frame is the last
// accepted sample's screenshot — the frame the caller saves the state on.
// The ctx deadline bounds the run; the returned error names the last
// measured sample on a budget failure.
func CloseAdvance(ctx context.Context, b Bridge, evOut string) (image.Image, error) {
	var det CloseDetector
	deadline := time.Now().Add(closeDeadline)
	var lastDetail string
	frames := 0
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("close: context done before the gameplay scene: %w (last: %s)", ctx.Err(), lastDetail)
		default:
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("close: no settled full-frame scene within %s (last: %s)", closeDeadline, lastDetail)
		}
		shot, err := b.Screenshot(ctx)
		if err != nil {
			// A lost sample is not a scene change: wait the cadence and
			// re-sample (the detector is not fed).
			if err := ctxErr(ctx); err != nil {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(closeSampleInterval):
			}
			continue
		}
		frames++
		sig := ScreenSigOf(shot)
		c, _ := sig.ClassOf()
		phase, done := det.Observe(time.Now(), sig.Letterbox)
		lastDetail = fmt.Sprintf("class=%s | %s", c, det.Detail(sig.Letterbox))
		if evOut != "" {
			storeEvidence(evOut, fmt.Sprintf("close-iter-%02d.png", frames), shot)
		}
		if done {
			return shot, nil
		}
		switch phase {
		case ClosePhaseSettle:
			// Presses are stopped: the scene is settling (measured stable
			// within ~2 s of the presses stopping). Sample at the settle
			// cadence — no input, so the deck cannot blink.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(closeSampleInterval):
			}
		default:
			// Advance: A on the 2.5 s cadence while the screen is not the
			// settled gameplay scene (the dialogue and the cutscene both
			// advance on A).
			if err := b.Inject(ctx, Buttons{A: true}, Axes{}, 500*time.Millisecond); err != nil {
				return nil, fmt.Errorf("close: press A: %w", err)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(closePressInterval):
			}
		}
	}
}

func ctxErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
