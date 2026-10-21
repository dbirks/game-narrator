package pipeline

import (
	"image"
	"image/color"
	"testing"

	"github.com/dbirks/game-narrator/internal/vision"
)

// syntheticBase is the Wind Waker textbox rect measured on the live-hdmi
// capture (ADR-0137): the profile's "dialogue box high" band, in the 4:3
// frame coordinates the 640×480 capture uses.
var syntheticBase = vision.Rect{X: 0.05, Y: 0.25, W: 0.8, H: 0.37}

// TestTextBandCandidates checks the candidate derivation: top / profile /
// bottom, de-duplicated, with the degenerate cases collapsing to the base.
func TestTextBandCandidates(t *testing.T) {
	// A middle profile: three distinct bands, profile in the middle.
	cands := textBandCandidates(syntheticBase)
	if len(cands) != 3 {
		t.Fatalf("candidates = %d, want 3 (top/profile/bottom)", len(cands))
	}
	if cands[0].Y != 0 {
		t.Errorf("top band y = %v, want 0", cands[0].Y)
	}
	if cands[1].Y != syntheticBase.Y {
		t.Errorf("profile band y = %v, want the base %v", cands[1].Y, syntheticBase.Y)
	}
	if want := 1 - syntheticBase.H; cands[2].Y != want {
		t.Errorf("bottom band y = %v, want %v", cands[2].Y, want)
	}
	for i, c := range cands {
		if c.X != syntheticBase.X || c.W != syntheticBase.W || c.H != syntheticBase.H {
			t.Errorf("band %d keeps the profile's x/w/h: got %+v", i, c)
		}
	}

	// A profile already at the top: top and profile collapse, leaving two.
	top := vision.Rect{X: 0.05, Y: 0, W: 0.8, H: 0.37}
	if cands := textBandCandidates(top); len(cands) != 2 {
		t.Fatalf("top profile: candidates = %d, want 2 (top+bottom collapse to one)", len(cands))
	}
	// A profile already at the bottom: profile and bottom collapse.
	bot := vision.Rect{X: 0.05, Y: 1 - 0.37, W: 0.8, H: 0.37}
	if cands := textBandCandidates(bot); len(cands) != 2 {
		t.Fatalf("bottom profile: candidates = %d, want 2", len(cands))
	}
	// A height that fills the frame: no distinct vertical placement, the base alone.
	full := vision.Rect{X: 0, Y: 0, W: 1, H: 1}
	if cands := textBandCandidates(full); len(cands) != 1 {
		t.Fatalf("full-height profile: candidates = %d, want 1 (the base)", len(cands))
	}
	// An empty base degrades to the base alone (the caller's Validate catches an
	// empty region; the candidate set must not panic on one).
	if cands := textBandCandidates(vision.Rect{}); len(cands) != 1 {
		t.Fatalf("empty base: candidates = %d, want 1", len(cands))
	}
}

func TestProfileBandIndex(t *testing.T) {
	if got := profileBandIndex(textBandCandidates(syntheticBase), syntheticBase); got != 1 {
		t.Errorf("profile band index = %d, want 1 (the middle candidate)", got)
	}
	top := vision.Rect{X: 0.05, Y: 0, W: 0.8, H: 0.37}
	if got := profileBandIndex(textBandCandidates(top), top); got != 0 {
		t.Errorf("top profile band index = %d, want 0", got)
	}
}

// TestBoxLike exercises the backdrop gate (ADR-0137 §Robustness) in
// isolation: a band is box-like only when it carries a line on a panel whose
// rows the letters actually sit in.
func TestBoxLike(t *testing.T) {
	const presence = 0.30
	const textMin = 0.01
	const onPanelMin = 0.9
	// The box: a line on a panel, the letters in the panel's rows.
	box := bandScores{dark: 0.85, text: 0.036, onPanel: 1.0}
	if !boxLike(box, presence, textMin, onPanelMin) {
		t.Errorf("the box is box-like: got false for %+v", box)
	}
	// The banner: a strong text strip over the scene — its letters sit in
	// scene rows (the measured llvmpipe case: on-panel 0.80). It is NOT box-like.
	banner := bandScores{dark: 0.79, text: 0.027, onPanel: 0.80}
	if boxLike(banner, presence, textMin, onPanelMin) {
		t.Errorf("the banner over the scene is not box-like: got true for %+v", banner)
	}
	// A dark, textless panel (a scene, no line): the text floor keeps it out.
	scene := bandScores{dark: 0.93, text: 0.004, onPanel: 1.0}
	if boxLike(scene, presence, textMin, onPanelMin) {
		t.Errorf("a dark panel with no line is not box-like: got true for %+v", scene)
	}
	// A faint line: the text floor keeps it out (it is the box's own weak read —
	// the selection keeps the standing band for it, the presence path reads it).
	faint := bandScores{dark: 0.96, text: 0.004, onPanel: 1.0}
	if boxLike(faint, presence, textMin, onPanelMin) {
		t.Errorf("a faint line is not box-like: got true for %+v", faint)
	}
	// Text floating in a bright scene: no panel, no box.
	floating := bandScores{dark: 0.10, text: 0.02, onPanel: 1.0}
	if boxLike(floating, presence, textMin, onPanelMin) {
		t.Errorf("text in a bright scene is not box-like: got true for %+v", floating)
	}
}

// TestPickTextBand exercises the selection rule in isolation (ADR-0137
// §Robustness): the box-likeness gate, the sticky margin, and the switch
// persistence — the selection without a frame.
func TestPickTextBand(t *testing.T) {
	const presence = 0.30
	const margin = 1.25
	const textMin = 0.01
	const onPanelMin = 0.9
	const switchFrames = 3
	var mem placementMemory
	// Three bands: top carries the line (the live-hdmi rev3 top-box frame,
	// letters fully on the panel), the others are the line's bleed onto
	// neighbouring bands.
	cands := func() []bandScores {
		return []bandScores{
			{rect: vision.Rect{Y: 0}, dark: 0.85, text: 0.036, onPanel: 1.0, score: 0.036 * 0.85 * 1.0},
			{rect: vision.Rect{Y: 0.25}, dark: 0.63, text: 0.018, onPanel: 1.0, score: 0.018 * 0.63 * 1.0},
			{rect: vision.Rect{Y: 0.63}, dark: 0.36, text: 0.010, onPanel: 1.0, score: 0.010 * 0.36 * 1.0},
		}
	}

	// The box appears at the top while the crop is on the profile band (mid,
	// index 1): the competitor is box-like, but the crop does not move on the
	// first strong frame — it takes the top band only once the run is complete
	// (the anti-phantom rule: one strong frame never moves the crop).
	for i := 1; i < switchFrames; i++ {
		if got := pickTextBand(cands(), 1, presence, margin, textMin, onPanelMin, &mem, switchFrames); got != 1 {
			t.Fatalf("frame %d of the box's appearance: got band %d, want 1 (the standing band holds until the run completes)", i, got)
		}
	}
	if got := pickTextBand(cands(), 1, presence, margin, textMin, onPanelMin, &mem, switchFrames); got != 0 {
		t.Fatalf("run complete: got band %d, want 0 (the crop takes the line's band)", got)
	}
	// The crop is on top now: it stays while the box persists there.
	if got := pickTextBand(cands(), 0, presence, margin, textMin, onPanelMin, &mem, switchFrames); got != 0 {
		t.Fatalf("standing on the line: got band %d, want 0 (stay top)", got)
	}

	// A dark scene: every band is dark (panel presence high) but none carries
	// text past the floor. No band is box-like, the crop does not drift — dark
	// alone is never a box.
	scene := []bandScores{
		{rect: vision.Rect{Y: 0}, dark: 0.93, text: 0.006, onPanel: 1.0},
		{rect: vision.Rect{Y: 0.25}, dark: 0.82, text: 0.005, onPanel: 1.0},
		{rect: vision.Rect{Y: 0.63}, dark: 0.42, text: 0.004, onPanel: 1.0},
	}
	if got := pickTextBand(scene, 1, presence, margin, textMin, onPanelMin, &mem, switchFrames); got != 1 {
		t.Fatalf("dark scene keeps the standing crop: got %d, want 1", got)
	}

	// The exact regression the backdrop gate exists for: the llvmpipe banner.
	// A strong text strip at the top of the frame (text past the floor) whose
	// letters sit in scene rows (on-panel 0.80 < 0.9), while the standing
	// band — the profile's own — carries the box's faint read (text under the
	// floor, letters in the panel's rows). No band is box-like, so the crop
	// never leaves the calibrated band, however long the banner persists:
	// frame after frame, the answer is the standing band.
	bannerScene := []bandScores{
		{rect: vision.Rect{Y: 0}, dark: 0.79, text: 0.027, onPanel: 0.80},
		{rect: vision.Rect{Y: 0.62}, dark: 0.96, text: 0.004, onPanel: 1.0},
		{rect: vision.Rect{Y: 0.75}, dark: 0.98, text: 0.002, onPanel: 1.0},
	}
	for i := 0; i < 40; i++ {
		if got := pickTextBand(bannerScene, 1, presence, margin, textMin, onPanelMin, &mem, switchFrames); got != 1 {
			t.Fatalf("banner frame %d: got band %d, want 1 (the banner is not a box; the crop stays on the calibrated band)", i, got)
		}
	}

	// The banner fades and no band is box-like: still the standing band, and
	// the persistence run is dead (a box that reappears elsewhere counts from
	// one, and takes the crop only once ITS run is complete).
	faded := []bandScores{
		{rect: vision.Rect{Y: 0}, dark: 0.54, text: 0.001, onPanel: 0.0},
		{rect: vision.Rect{Y: 0.62}, dark: 0.96, text: 0.004, onPanel: 1.0},
		{rect: vision.Rect{Y: 0.75}, dark: 0.98, text: 0.002, onPanel: 1.0},
	}
	if got := pickTextBand(faded, 1, presence, margin, textMin, onPanelMin, &mem, switchFrames); got != 1 {
		t.Fatalf("faded banner keeps the standing band: got %d, want 1", got)
	}
	// A new box appears at the bottom (box-like): the crop follows it, but only
	// once the bottom's run is complete — never on its first strong frame.
	bottomBox := []bandScores{
		{rect: vision.Rect{Y: 0}, dark: 0.54, text: 0.001, onPanel: 0.0},
		{rect: vision.Rect{Y: 0.62}, dark: 0.96, text: 0.004, onPanel: 1.0},
		{rect: vision.Rect{Y: 0.75}, dark: 0.98, text: 0.02, onPanel: 1.0, score: 0.02 * 0.98},
	}
	for i := 1; i < switchFrames; i++ {
		if got := pickTextBand(bottomBox, 1, presence, margin, textMin, onPanelMin, &mem, switchFrames); got != 1 {
			t.Fatalf("new bottom box, frame %d: got %d, want 1 (the standing band holds until the run completes)", i, got)
		}
	}
	if got := pickTextBand(bottomBox, 1, presence, margin, textMin, onPanelMin, &mem, switchFrames); got != 2 {
		t.Fatalf("bottom box's run complete: got %d, want 2 (follow the box)", got)
	}

	// The margin: two box-like bands (the line bleeds across both). The
	// standing band keeps the crop unless the best beats it by the margin.
	bleed := []bandScores{
		{rect: vision.Rect{Y: 0}, dark: 0.85, text: 0.02, onPanel: 1.0, score: 0.02 * 0.85},
		{rect: vision.Rect{Y: 0.25}, dark: 0.85, text: 0.03, onPanel: 1.0, score: 0.03 * 0.85},
		{rect: vision.Rect{Y: 0.63}, dark: 0.40, text: 0.0, onPanel: 1.0},
	}
	// Fresh memory: mid is standing and box-like, top's score (0.017) is under
	// the margin (1.25 × 0.0255) — the crop stays on mid.
	mem2 := placementMemory{}
	if got := pickTextBand(bleed, 1, presence, margin, textMin, onPanelMin, &mem2, switchFrames); got != 1 {
		t.Fatalf("within margin the standing box band holds: got %d, want 1", got)
	}
	// Past the margin the best takes over (after its run completes).
	bleed[0].text, bleed[0].score = 0.04, 0.04*0.85
	for i := 0; ; i++ {
		if i >= switchFrames {
			break
		}
		got := pickTextBand(bleed, 1, presence, margin, textMin, onPanelMin, &mem2, switchFrames)
		if i == switchFrames-1 && got != 0 {
			t.Fatalf("past the margin: got %d, want 0 (the crop follows the stronger box)", got)
		}
	}

	// Out-of-range remembered band: take the best box-like band outright (there
	// is no standing crop to protect).
	c := cands()
	if got := pickTextBand(c, 9, presence, margin, textMin, onPanelMin, &placementMemory{}, switchFrames); got != 0 {
		t.Fatalf("bad remembered band falls to the best: got %d, want 0", got)
	}
}

// testFrame builds a 640×480 RGBA frame: a mid-grey scene with one dark panel
// carrying bright text strokes at the named band (0=top, 1=profile, 2=bottom).
// The text rows are sparse (a line is a few words, ~15% of the row bright —
// the measured box's rows), so the text's rows stay panel rows: the letters
// ride the panel, the way the real box's letters do.
func testFrame(band int) *image.RGBA {
	const w, h = 640, 480
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Scene: a mid-grey fill (luma ~100) — neither panel-dark nor text-bright,
	// so a band with no box scores low on both shares.
	scene := color.RGBA{R: 100, G: 100, B: 100, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, scene)
		}
	}
	// The box band's pixel span (the synthetic base rect mapped to 640×480).
	y0s := []int{0, 120, 302} // top / profile / bottom, matching the base rect
	y0, y1 := y0s[band], y0s[band]+178
	// The dark panel (luma ~40) inside the band.
	panel := color.RGBA{R: 40, G: 40, B: 40, A: 255}
	for y := y0 + 10; y < y1-10; y++ {
		for x := 40; x < 440; x++ {
			img.Set(x, y, panel)
		}
	}
	// Bright text strokes (luma ~230) riding the panel: sparse rows (the line's
	// words — under half the row bright, so the rows stay panel rows), the
	// measured box's row density, enough of them to clear the text floor.
	white := color.RGBA{R: 230, G: 230, B: 230, A: 255}
	for line := 0; line < 10; line++ {
		ly := y0 + 20 + line*15
		for _, seg := range [][2]int{{60, 100}, {140, 180}, {220, 260}} {
			for x := seg[0]; x < seg[1]; x++ {
				img.Set(x, ly, white)
				img.Set(x, ly+1, white)
			}
		}
	}
	return img
}

// testBannerFrame builds the 2026-10-21 Dolphin probe's failure shape as a
// synthetic frame (ADR-0137 §Robustness, the regression the backdrop gate
// exists for): a dark scene with a dense text strip at the TOP of the frame —
// the llvmpipe GL-info banner, its letters in scene rows — while the real
// dialogue box sits in the BOTTOM band, its line riding a panel that fills the
// band. The box must win the crop, every frame, for as long as the banner
// persists.
func testBannerFrame() *image.RGBA {
	const w, h = 640, 480
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// The scene: a dark ground (the game behind the box). Dark, the way the
	// measured probe's scene was: the banner's panel presence rides the
	// scene's darkness, which is exactly why the dark share alone cannot
	// reject it.
	scene := color.RGBA{R: 45, G: 45, B: 45, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, scene)
		}
	}
	// The bottom box: a panel (luma ~30) filling the bottom band, with a short
	// line of text (luma ~230) riding it — sparse rows on the panel, the
	// letters in the panel's rows.
	panel := color.RGBA{R: 30, G: 30, B: 30, A: 255}
	for y := 310; y < 470; y++ {
		for x := 30; x < 610; x++ {
			img.Set(x, y, panel)
		}
	}
	white := color.RGBA{R: 230, G: 230, B: 230, A: 255}
	for _, ly := range []int{340, 360, 380, 400, 420, 440} {
		for _, seg := range [][2]int{{100, 200}, {240, 330}} {
			for x := seg[0]; x < seg[1]; x++ {
				img.Set(x, ly, white)
				img.Set(x, ly+1, white)
			}
		}
	}
	// The top banner: a dense strip of text (luma ~230) directly on the scene —
	// no panel behind it. Several near-full rows, the way the GL-info block
	// reads: its text share beats the box's on a raw pixel count, which is why
	// the selection must reject it on the backdrop, not the count. Its rows
	// are mostly bright (the text, not a panel), so they are scene rows, not
	// panel rows: on-panel ~0.
	for _, ly := range []int{22, 24, 26, 62, 64, 66, 102, 104} {
		for x := 20; x < 560; x++ {
			img.Set(x, ly, white)
		}
	}
	return img
}

// TestScoreTextBandsOnSyntheticBox checks the cheap signal end to end on a
// synthetic frame: the band that carries the line scores clearly higher than
// the empty bands, on all three placements.
func TestScoreTextBandsOnSyntheticBox(t *testing.T) {
	cands := textBandCandidates(syntheticBase)
	crop := vision.CropOptions{} // LetterboxNone: map onto the whole frame
	const darkLuma = uint8(90)
	for band, want := range map[int]string{0: "top", 1: "middle", 2: "bottom"} {
		img := testFrame(band)
		sc := scoreTextBands(img, cands, crop, darkLuma)
		best := 0
		for i := range sc {
			if sc[i].score > sc[best].score {
				best = i
			}
		}
		if best != band {
			t.Errorf("%s box: best band = %d (scores %v), want %d", want, best, bandScoresScores(sc), band)
		}
		// The box band must actually clear the selection floor: the signal is not
		// just relatively higher, it is high enough to move the crop.
		if sc[band].text < DefaultPlacementTextMin {
			t.Errorf("%s box: box-band text share %v < floor %v (the crop would never move there)",
				want, sc[band].text, DefaultPlacementTextMin)
		}
		// And its letters must sit in the panel's rows (the backdrop gate): the
		// synthetic box is the gate's positive case.
		if sc[band].onPanel < DefaultPlacementTextOnPanel {
			t.Errorf("%s box: box-band on-panel share %v < gate %v (the crop would reject the box itself)",
				want, sc[band].onPanel, DefaultPlacementTextOnPanel)
		}
	}
}

// TestPlacementBannerVsBox is the synthetic regression for the 2026-10-21
// Dolphin run (ADR-0137 §Robustness): a text banner at the top of the frame
// and the box in the bottom band. The box must win — the crop lands on the
// bottom band and stays there for as long as the banner persists, and it never
// visits the top band's phantom text.
func TestPlacementBannerVsBox(t *testing.T) {
	cands := textBandCandidates(syntheticBase)
	crop := vision.CropOptions{}
	const darkLuma = uint8(90)
	const presence, margin, textMin = 0.30, DefaultPlacementSwitchMargin, DefaultPlacementTextMin
	const onPanelMin, switchFrames = DefaultPlacementTextOnPanel, DefaultPlacementSwitchFrames

	band := profileBandIndex(cands, syntheticBase) // start on the profile band (middle)
	var mem placementMemory
	img := testBannerFrame()

	// Measure the frame's signals so the test fails loudly (with the numbers)
	// if the synthetic ever stops modelling the measured case.
	s0 := scoreTextBands(img, cands, crop, darkLuma)
	if s0[0].text <= s0[2].text {
		t.Fatalf("synthetic does not model the probe: the banner's text share (%v) must beat the box's (%v) — the failure is a count the backdrop gate must reject",
			s0[0].text, s0[2].text)
	}
	if s0[2].onPanel < DefaultPlacementTextOnPanel || s0[0].onPanel >= DefaultPlacementTextOnPanel {
		t.Fatalf("synthetic does not model the probe: the box's letters must sit in the panel's rows (%v >= %v) and the banner's must not (%v < %v)",
			s0[2].onPanel, DefaultPlacementTextOnPanel, s0[0].onPanel, DefaultPlacementTextOnPanel)
	}

	// The banner persists for a long run (the measured case: seconds of the
	// GL-info block). The crop never visits the top band: the banner is not a
	// box, so no band is box-like until the box's own band clears the floor —
	// and the box's band (bottom) is where the line is, so once its run
	// completes the crop takes it and stays.
	everTop := false
	for frame := 0; frame < 30; frame++ {
		scores := scoreTextBands(img, cands, crop, darkLuma)
		band = pickTextBand(scores, band, presence, margin, textMin, onPanelMin, &mem, switchFrames)
		if band == 0 {
			everTop = true
			t.Fatalf("frame %d: the crop visited the top band (the banner's phantom text) — the backdrop gate must keep it out", frame)
		}
	}
	if everTop {
		t.Fatalf("the crop visited the banner band")
	}
	// Where it landed: the box's band. The box's line is strong enough to clear
	// the floor on the synthetic (unlike the probe's faint read), so the bottom
	// band is box-like and, once its run completes, takes the crop from the
	// middle.
	if band != 2 {
		t.Fatalf("after %d frames of banner + box: standing band = %d, want 2 (the box's band)", 30, band)
	}

	// The banner fades (a fresh frame with only the box): the crop holds the
	// box's band, and a new top box would take over only after ITS run.
	boxOnly := testFrame(2)
	for frame := 0; frame < 10; frame++ {
		scores := scoreTextBands(boxOnly, cands, crop, darkLuma)
		band = pickTextBand(scores, band, presence, margin, textMin, onPanelMin, &mem, switchFrames)
		if band != 2 {
			t.Fatalf("box-only frame %d: band = %d, want 2 (hold the box's band)", frame, band)
		}
	}
}

// TestPlacementEndToEnd drives the sticky selection across a sequence: the box
// appears at the top, the crop moves there (once the box's run completes) and
// stays, then the box moves to the bottom and the crop follows — and a dark
// frame in between does not flicker it.
func TestPlacementEndToEnd(t *testing.T) {
	cands := textBandCandidates(syntheticBase)
	crop := vision.CropOptions{}
	const darkLuma = uint8(90)
	const presence, margin, textMin = 0.30, DefaultPlacementSwitchMargin, DefaultPlacementTextMin
	const onPanelMin, switchFrames = DefaultPlacementTextOnPanel, DefaultPlacementSwitchFrames

	band := profileBandIndex(cands, syntheticBase) // start on the profile band (middle)
	var mem placementMemory

	// A dark scene first: the crop stays on the profile band (no box anywhere).
	scores := scoreTextBands(testFrameScene(), cands, crop, darkLuma)
	band = pickTextBand(scores, band, presence, margin, textMin, onPanelMin, &mem, switchFrames)
	if band != 1 {
		t.Fatalf("dark scene: band = %d, want 1 (the standing profile band)", band)
	}

	// The box appears at the top: the crop holds for the run, then moves.
	for i := 0; i < switchFrames; i++ {
		scores = scoreTextBands(testFrame(0), cands, crop, darkLuma)
		band = pickTextBand(scores, band, presence, margin, textMin, onPanelMin, &mem, switchFrames)
		if i < switchFrames-1 && band != 1 {
			t.Fatalf("top box, frame %d of the run: band = %d, want 1 (the standing band holds)", i, band)
		}
	}
	if band != 0 {
		t.Fatalf("top box run complete: band = %d, want 0 (the crop takes the line's band)", band)
	}

	// The top box persists across frames (slight scene noise): the crop stays.
	for i := 0; i < 3; i++ {
		scores = scoreTextBands(testFrame(0), cands, crop, darkLuma)
		band = pickTextBand(scores, band, presence, margin, textMin, onPanelMin, &mem, switchFrames)
		if band != 0 {
			t.Fatalf("persisting top box, frame %d: band = %d, want 0 (stay top)", i, band)
		}
	}

	// The box moves to the bottom: the crop holds until the bottom's run
	// completes, then follows.
	for i := 0; i < switchFrames; i++ {
		scores = scoreTextBands(testFrame(2), cands, crop, darkLuma)
		band = pickTextBand(scores, band, presence, margin, textMin, onPanelMin, &mem, switchFrames)
		if i == switchFrames-1 && band != 2 {
			t.Fatalf("bottom box run complete: band = %d, want 2 (follow the box)", band)
		}
	}
	if band != 2 {
		t.Fatalf("bottom box: band = %d, want 2", band)
	}
}

// testFrameScene is a 640×480 frame with a dark scene and no box: every band is
// dark (panel presence high) but none carries text.
func testFrameScene() *image.RGBA {
	const w, h = 640, 480
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	dark := color.RGBA{R: 30, G: 30, B: 30, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, dark)
		}
	}
	return img
}

func bandScoresScores(s []bandScores) []float64 {
	out := make([]float64, len(s))
	for i := range s {
		out[i] = s[i].score
	}
	return out
}
