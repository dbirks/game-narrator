//go:build unix

package dolphin

import (
	"context"
	"image"
	"image/color"
	"path/filepath"
	"testing"
	"time"
)

// darkDialogImage is the shape of the REAL post-START memory-card prompt
// (the 2026-10-18 real run's retained evidence, measured on the 640x480
// render window): a full-frame DARK dialog — high-entropy content in the
// centre and DARK-GRAY top/bottom bands (luma ~42, lb ~0.166), between the
// opening movie's black bands (measured 0.000) and the title's sky/sea
// bands (measured 0.32-0.56). The old settled clause's sky-band floor
// (letterboxTitleMin) rejected this shape; isSettledScene's not-letterboxed
// floor (letterboxMovieMax) accepts it.
func darkDialogImage(w, h int) image.Image {
	centre := noiseImage(w, h-60, 42) // high-entropy content
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	gray := color.Gray{Y: 42}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var c color.Color
			if y >= 30 && y < h-30 {
				c = centre.At(x, y-30)
			} else {
				c = gray
			}
			r, g, b, a := c.RGBA()
			i := (y*w + x) * 4
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = byte(r>>8), byte(g>>8), byte(b>>8), byte(a>>8)
		}
	}
	return out
}

// TestIsSettledSceneClauses pins the settled predicate on the REAL measured
// values (the 2026-10-18 run's post-START prompt) and the confusable shapes
// it must keep rejecting.
func TestIsSettledSceneClauses(t *testing.T) {
	// The real post-START screen: the settled dark memory-card dialog
	// (measured on the retained evidence: lb 0.166, ent 0.682, step 0.016,
	// d 0.29 vs the nearest control framing).
	if !isSettledScene(sceneProbe{lb: 0.166, ent: 0.682, step: 0.016}) {
		t.Errorf("the measured real post-START dialog (lb 0.166, ent 0.682, step 0.016) must be settled")
	}
	// The old unit-double shape (the bright full-frame scene) still settles:
	// the sky-band case is a special case of the not-letterboxed floor.
	if !isSettledScene(sceneProbe{lb: 0.40, ent: 0.60, step: 0.02}) {
		t.Errorf("the bright full-frame settled scene (lb 0.40) must stay settled")
	}
	// The letterboxed movie frame: black bands — the movie shape, never
	// settled (the input window's letter mode, with its far clause, owns it).
	if isSettledScene(sceneProbe{lb: 0.000, ent: 0.50, step: 0.01}) {
		t.Errorf("the letterboxed movie shape (black bands, lb 0.000) must not be settled")
	}
	// A black fade: no rendered content (the entropy clause owns it).
	if isSettledScene(sceneProbe{lb: 0.02, ent: 0.05, step: 0.01}) {
		t.Errorf("a black fade (no content, ent 0.05) must not be settled")
	}
	// The orbiting title: the bands are fine, but the orbit moves
	// >= crossStepMax per 1 s sample (measured 0.10-0.40) — it never settles.
	if isSettledScene(sceneProbe{lb: 0.34, ent: 0.60, step: 0.15}) {
		t.Errorf("the orbiting title (step 0.15 >= crossStepMax) must not be settled")
	}
}

// TestClassifyWindowSettledDarkDialog: the input window the real run
// produced — one title sample, the fade, then the STABLE dark dialog — must
// cross on the settled mode (two consecutive stable dialog samples), even
// though the dialog's bands are the dark-gray dialog frame, not the title's
// sky. The confusable — a letterboxed movie shape that stays CLOSE to the
// control (the far clause cannot see it) — must NOT cross: the relaxed band
// floor must not leak into the movie shape.
func TestClassifyWindowSettledDarkDialog(t *testing.T) {
	dlg := darkDialogImage(640, 480)
	dlgLuma := normalise(dlg, normW, normH)
	titleLuma := normalise(brightNoiseImage(640, 480, 7), normW, normH)
	t.Logf("measured synthetic shape: dialog lb=%.3f ent=%.3f d-vs-title=%.3f",
		letterBand01(dlg), entropy01(dlg), rmseSlice(dlgLuma, titleLuma))
	ctrl := []sceneProbe{{luma: titleLuma, lb: 0.40, ent: 0.60, step: 0.30}}
	// The window: title sample (orbit step), fade (no content), then the
	// settled dialog — the measured real sequence (step 0.016-0.02 once the
	// dialog is up, lb 0.166-0.233, ent 0.64-0.68).
	win := []sceneProbe{
		{luma: titleLuma, lb: 0.40, ent: 0.60, step: 0.25},
		{luma: dlgLuma, lb: 0.153, ent: 0.68, step: 0.25}, // the fade-in sample
		{luma: dlgLuma, lb: 0.191, ent: 0.65, step: 0.22},
		{luma: dlgLuma, lb: 0.191, ent: 0.65, step: 0.02},
		{luma: dlgLuma, lb: 0.173, ent: 0.68, step: 0.02},
		{luma: dlgLuma, lb: 0.166, ent: 0.68, step: 0.02},
	}
	crossed, maxD := classifyWindow(win, ctrl, 0.29)
	t.Logf("dialog window: crossed=%v maxDist=%.3f (sceneTH 0.29)", crossed, maxD)
	if !crossed {
		t.Fatalf("the measured real post-START window must cross (the settled mode)")
	}

	// The confusable: the SAME stability, but the movie shape (black bands)
	// sitting CLOSE to the control — the far clause cannot see it, and the
	// settled mode's band floor must keep it out.
	movieWin := []sceneProbe{
		{luma: titleLuma, lb: 0.40, ent: 0.60, step: 0.25},
		{luma: titleLuma, lb: 0.000, ent: 0.50, step: 0.01}, // stable, black bands
		{luma: titleLuma, lb: 0.000, ent: 0.50, step: 0.01},
		{luma: titleLuma, lb: 0.000, ent: 0.50, step: 0.01},
	}
	if crossed, _ := classifyWindow(movieWin, ctrl, 0.29); crossed {
		t.Fatalf("a stable letterboxed shape close to the control must not cross (movie false positive)")
	}
}

// TestProveInputDarkDialogSettled: the full harness on the REAL shape — the
// orbiting title, then the stable dark dialog after the press (the
// 2026-10-18 measured post-START screen). Acceptance must come from the
// settled mode: the dialog sits close in RMSE to the title framings, so the
// letterbox far-branch cannot see it (the TestProveInputSettledScene double
// used the bright full-frame shape, which is why the sky-band floor's
// mismatch with the measured screen survived until the real run).
func TestProveInputDarkDialogSettled(t *testing.T) {
	const pressAt = 10
	env := newTestBridge(t, func(call int, r Region) image.Image {
		if call == 1 {
			return solidImage(r.W, r.H, 0) // baseline
		}
		if call < pressAt {
			return brightNoiseImage(r.W, r.H, call+7) // the orbiting title
		}
		// The new screen: the STABLE dark dialog (the same frame every
		// sample — step 0), dark-gray bands, rendered content.
		return darkDialogImage(r.W, r.H)
	}, func(o *Options) {
		o.ProveInputSamples = 4
		o.ProveInputCadence = 100 * time.Millisecond
	})
	mustLaunch(t, env, LaunchSpec{})
	defer env.b.Stop()
	_ = pipeReader(t, filepath.Join(env.b.State().UserDir, "Pipes", "pipe1"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ev, err := env.b.ProveInput(ctx)
	if err != nil {
		t.Fatalf("ProveInput: %v", err)
	}
	if len(ev.Distances) != 2 {
		t.Fatalf("evidence distances = %v, want 2 (control dispersion, input max)", ev.Distances)
	}
	t.Logf("dark-dialog input: control_dispersion=%.4f input_max=%.4f", ev.Distances[0], ev.Distances[1])
	if _, ok := ev.Files["after"]; !ok {
		t.Errorf("evidence missing the after PNG (the crossed sample)")
	}
}
