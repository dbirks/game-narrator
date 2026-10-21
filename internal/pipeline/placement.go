package pipeline

import (
	"image"

	"github.com/dbirks/game-narrator/internal/vision"
)

// ADR-0137: the textbox region is placement-aware. A dialogue box's width,
// height and horizontal position are calibrated by the profile and stable; only
// its vertical placement varies (a game like Wind Waker draws the box in the
// top, the middle and the bottom of the picture). The ROI stage therefore crops
// one of a small candidate set — top / the profile's own y / bottom — selected
// per frame on a cheap text-presence signal (vision.TextPresence), never on an
// OCR call. Exactly one band is cropped and recognised, through the unchanged
// dedupe/settle/escalation machinery (docs/spec/05 "suppress duplicate OCR
// calls"; AGENTS.md "keep the runtime predictable").
//
// The whole thing is a runtime default: the candidate set is derived from the
// stored rect at run time, so no schema change, no migration, and the existing
// profiles pick the behaviour up without re-publishing.

// bandScores is one candidate band's measurement for one frame.
type bandScores struct {
	rect    vision.Rect
	dark    float64 // panel presence (luma < DarkLuma share)
	text    float64 // bright-on-dark strokes (the letters)
	onPanel float64 // the letters' share that sits in panel rows (the backdrop)
	score   float64 // text × dark × onPanel: text riding the panel
}

// textBandCandidates derives the candidate placements for a textbox region's
// base rect: the top (y=0), the profile's own y (the calibrated band — the
// middle placement in practice) and the bottom (y=1-h), de-duplicated. The
// horizontal position and the width/height are the profile's; only y moves.
// A degenerate base (empty, or a height that fills the frame) yields the base
// rect alone, which is the historic single-band path.
func textBandCandidates(base vision.Rect) []vision.Rect {
	if base.Empty() || base.H >= 1 {
		return []vision.Rect{base}
	}
	// top, profile, bottom — in that order, so the profile's own band is the
	// middle candidate and the remembered band defaults to it.
	anchors := []float64{0, base.Y, 1 - base.H}
	out := make([]vision.Rect, 0, len(anchors))
	for _, y := range anchors {
		// Keep the box inside the unit square: a clamped base already sits in
		// [0,1], so y is in [0, 1-h]; clamp anyway so a hand-built rect (a
		// test, a malformed profile) cannot push a candidate off the frame.
		ny := y
		if ny < 0 {
			ny = 0
		}
		if ny > 1-base.H {
			ny = 1 - base.H
		}
		r := vision.Rect{X: base.X, Y: ny, W: base.W, H: base.H}
		dup := false
		for _, o := range out {
			if o.Y == r.Y && o.X == r.X && o.W == r.W && o.H == r.H {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		out = append(out, base)
	}
	return out
}

// profileBandIndex is the index of the profile's own band (base.Y) in the
// candidate set: the remembered band's default, so a crop that has never moved
// reads the band the operator calibrated — the historic behaviour on frame one.
func profileBandIndex(cands []vision.Rect, base vision.Rect) int {
	for i, c := range cands {
		if c.Y == base.Y {
			return i
		}
	}
	// The profile anchor was dropped (degenerate base): fall back to the middle
	// candidate, the band closest to the calibrated position.
	if len(cands) > 0 {
		return len(cands) / 2
	}
	return 0
}

// boxLike reports whether one band reads as a dialogue box: it carries a line
// (its text share reaches textMin), the line rides a panel (its dark share
// reaches the presence floor) and the letters sit IN the panel (its on-panel
// share reaches textOnPanel). The on-panel term is the backdrop gate
// (ADR-0137 §Robustness): a text strip floating over the scene — the llvmpipe
// GL-info banner Dolphin draws into the top of the frame after a savestate
// load, a HUD caption — has its letters in scene rows and fails it, while the
// box's letters sit in the panel's rows and pass it. Measured: banner band
// 0.80, box band 1.00 (both the 2026-10-21 Dolphin probe and the live-hdmi
// capture); the gate sits at 0.9 between them.
func boxLike(c bandScores, presenceFloor, textMin, textOnPanel float64) bool {
	return c.text >= textMin && c.dark >= presenceFloor && c.onPanel >= textOnPanel
}

// placementMemory is the per-region switch persistence (ADR-0137 §Robustness).
// It is the anti-phantom rule: a band that looks box-like on ONE frame is not
// a box move — the llvmpipe banner scores a strong text strip for its whole
// lifetime, and a single strong frame at the top of the frame must never drag
// the crop off the band the operator calibrated. A competitor band only takes
// the crop after it has been THE best box-like band for switchFrames
// consecutive scored frames; any frame where it is not (a blip, the banner
// flickering, the box already gone) resets the run.
type placementMemory struct {
	cand int // the candidate the run is counting toward, -1 when none
	run  int // consecutive frames cand has been the best box-like band
}

// advance moves the persistence memory one frame: it counts cand while cand
// keeps being the best box-like band, and resets on any other outcome — so a
// switch only ever fires on a run that is unbroken.
func (m *placementMemory) advance(cand int) {
	if cand == m.cand && cand >= 0 {
		m.run++
		return
	}
	m.cand = cand
	m.run = 1
}

// pickTextBand chooses the band to crop, given this frame's scored candidates
// and the remembered band, applying ADR-0137's box-likeness gate, the sticky
// margin and the switch persistence (all pure: the selection rule isolated
// from the crop, so the tests exercise it without a frame).
//
// The rule:
//
//  1. A band is a box candidate only when it is box-like (boxLike): text on a
//     panel, the letters inside the panel. Dark alone is never a box — a dark
//     scene puts a panel on every band, so the dark share is the region's
//     presence gate (the caller's job), never the selector — and text alone
//     is never a box, because the text is what a HUD strip carries.
//
//  2. No candidate is box-like: no box is on screen. The crop does not move —
//     it stays on the remembered band, the region reads as present-or-absent
//     on the band it is already on (exactly the historic single-band
//     behaviour), the perceptual dedupe keeps suppressing the repeated empty
//     OCR, and re-acquisition is immediate the frame the box returns. This is
//     also the banner's endgame: once the GL-info strip fades, no band is
//     box-like, and a crop that never left the calibrated band is still there,
//     reading the box the whole time.
//
//  3. Otherwise the best box-like candidate (highest text×dark×onPanel score,
//     ties to the lower index) is the one the box is on. If it is the standing
//     band, the crop stays. If the standing band is itself box-like it keeps
//     the crop unless the best beats it by the relative margin
//     (switchMargin× its score) — the anti-flicker: a one-frame blip of a few
//     percent must not move the crop and reset the perceptual dedupe.
//
//  4. The standing band is not box-like (the box is elsewhere, or was never
//     there): the crop takes the best band only after the persistence run
//     reaches switchFrames — never on a single strong frame, and never while
//     the competitor's run is broken by one non-best frame.
func pickTextBand(cands []bandScores, cur int, presenceFloor, switchMargin, textMin, textOnPanel float64, mem *placementMemory, switchFrames int) int {
	if len(cands) == 0 {
		return 0
	}
	if switchFrames < 1 {
		switchFrames = 1
	}
	// The box-like candidates and their best.
	best := -1
	for i := range cands {
		if !boxLike(cands[i], presenceFloor, textMin, textOnPanel) {
			continue
		}
		if best < 0 || cands[i].score > cands[best].score {
			best = i
		}
	}
	if best < 0 {
		// No box anywhere: keep the remembered band, and the persistence run
		// dies with the box (a competitor that reappears counts from one).
		if mem != nil {
			mem.advance(-1)
		}
		if cur < 0 || cur >= len(cands) {
			return 0
		}
		return cur
	}
	if mem != nil {
		mem.advance(best)
	}
	if cur < 0 || cur >= len(cands) {
		return best
	}
	if best == cur {
		// The standing band is where the box is. The run counts toward itself,
		// so the margin below can never fire on it.
		return cur
	}
	if boxLike(cands[cur], presenceFloor, textMin, textOnPanel) {
		// The standing band carries a box too (the line bleeds across two
		// bands): it keeps the crop unless the best beats it by the margin.
		if cands[best].score < switchMargin*cands[cur].score {
			return cur
		}
	}
	// The standing band is not box-like: the competitor takes the crop only
	// once its run of best frames is unbroken and complete.
	if mem != nil && mem.run >= switchFrames {
		return best
	}
	return cur
}

// scoreTextBands measures each candidate band on one frame: crop the band
// through the tracker's active window (vision.CropNormalized shares the cut's
// own window and MapRect spelling), a luma pass (ToGray) and the cheap
// TextPresence walk. It is the whole per-frame selection cost — three luma
// passes against the ~600 ms OCR it steers — and it derives the box score
// (text × dark × onPanel): a line riding the panel beats a line floating over
// the scene, the backdrop preference the selection ranks on.
func scoreTextBands(img image.Image, cands []vision.Rect, crop vision.CropOptions, darkLuma uint8) []bandScores {
	out := make([]bandScores, len(cands))
	for i, r := range cands {
		c := vision.CropNormalized(img, r, crop)
		var dark, text, onPanel float64
		if c != nil && !c.Bounds().Empty() {
			dark, text, onPanel = vision.TextPresence(vision.ToGray(c), darkLuma, vision.DefaultTextLuma)
		}
		s := bandScores{rect: r, dark: dark, text: text, onPanel: onPanel}
		// The backdrop-weighted box score: text on a panel, the letters in the
		// panel's rows. The dark factor is the panel's contrast (a box is text
		// on a panel darker than the scene, not text on the scene); the
		// on-panel factor is its extent of influence (the panel the letters
		// actually sit in).
		s.score = text * dark * onPanel
		out[i] = s
	}
	return out
}
