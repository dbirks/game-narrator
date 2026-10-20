//go:build unix

package dolphin

// telescope_test.go is the G8 item recording (docs/status/blocked.md, row
// "Dolphin sign savestate"; ADR-0089's item content class on a real capture):
// from the mid-game deck checkpoint — the proven first-dialogue state (the
// M6 fixture's own savestate) advanced to the gameplay scene by the
// screen-driven CLOSE step, the same route TestRealSavestateRoundTripGameplay
// proved round-trips — the drive walks to Aryll, speaks with her, and
// records the conversation that ends in the item-get box (the item popup,
// the content-kind mark's "item" line) as the fixture
// windwaker-aryll-telescope.
//
// The fixture carries (AGENTS.md rule 5 — all of it gitignored, referenced
// by hash): the approach frames (walk/camera steps at the 1 Hz low rate),
// the conversation + item box at the dense 10 Hz rate, the record-time
// Tesseract truth of every box line (the adaptive box-text crop), the
// savestate saved RIGHT BEFORE the item box (the pre-press save of the
// advance that produced it — the checkpoint a replay or a --save_state boot
// verifies against the manifest), and the content-kind mark (kind "item",
// frame = the first recorded frame the item box was on screen) that makes
// the profile emit the box region as an item region and the content line's
// per-message text_kind (ADR-0089, the committed scenario loader's read).
//
// The "see the screenshot after every step" half of the drive: every frame
// is measured structurally (the screen class, the olive talk-arrow's
// window stats, the A-button label's Tesseract reading), and the judgments
// the classifier cannot make (the item box's distinct layout, the "Speak"
// label, a stuck camera) go to the user-configured vision model — the same
// role the product's OCR fallback uses — one downscaled frame + one word
// back (vision_probe.go). The drive degrades honestly (structural-only,
// logged) when the gateway is not configured.
//
// Golden-file semantics (ADR-0053): a verified fixture whose referenced
// states hash is NOT re-recorded. NOT RUN when the image or the host tools
// are missing — and never labelled as unit coverage.

import (
	"context"
	"fmt"
	"image"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

const (
	// FixtureIDTelescope is the G8 item fixture id (the scenario
	// dolphin/item-telescope's input).
	FixtureIDTelescope = "windwaker-aryll-telescope"
	// StateRelDeck is the stable deck checkpoint (the CLOSE scene: Aryll a
	// few steps from Link on the lookout deck) under the fixture root's
	// states/ dir — the checkpoint this recording starts from, referenced
	// by the fixture's manifest by hash.
	StateRelDeck = "states/windwaker-deck-aryll.state"
	// StateRelPreItem is the savestate saved RIGHT BEFORE the item box:
	// the pre-press save of the advance that produced the item popup (the
	// conversation is at its last dialogue line).
	StateRelPreItem = "states/windwaker-pre-item-box.state"
)

// The drive's vision questions (one word back; vision_probe normalizes).
const (
	// qItemBox asks the item judgment: the item-get screen (the distinct
	// layout with the item icon) vs the plain dialogue box. The answer is
	// the drive's item detection — the "detect it visually" step.
	qItemBox = "This is a frame from the GameCube game The Legend of Zelda: The Wind Waker. " +
		"Is this the ITEM-GET screen (an item is being obtained: an item icon and item name are " +
		"present, a large item presentation, or a message box announcing that an item was obtained) " +
		"rather than a plain NPC dialogue box? Answer with exactly one word: YES or NO."
	// qSpeak asks the A-button label judgment (the label's Tesseract reading
	// is the fast signal; the model confirms before the pressing A).
	qSpeak = "In this game frame, is a button action-prompt label visible that reads SPEAK " +
		"(the prompt shown when facing a character you can talk to)? Answer with exactly one word: YES or NO."
	// qWalkDir breaks a stuck approach: which screen direction is the olive
	// talk arrow in, relative to the character.
	qWalkDir = "In this third-person game frame, where is the floating yellow exclamation-mark talk arrow " +
		"relative to the main character? AHEAD (straight ahead, above the character), LEFT (screen left), " +
		"RIGHT (screen right), BEHIND (the character faces away from it), or NONE (no such arrow is visible). " +
		"Answer with exactly one word."
)

// The approach policy's measured constants (the deck is a small wooden
// platform: a few steps at most; the budget is the fail-fast bound).
const (
	// arrowMinPx is the talk-arrow's densest-window pixel floor (640x480):
	// the arrow measures ~400 dense olive px in the 32 px window on the
	// restored-deck evidence (the 24 px proximity clustering measured 445 px
	// in a 33x25 bbox at (0.60, 0.34)), while the only other olive object on
	// the deck — the bottom-right "A SPEAK" prompt UI (115 px at (0.86, 0.90))
	// — is excluded by the arrow's cy <= 0.66 clause. Scene greens and the
	// deck's pure-yellow sunlit patches (measured rgb 250,235,16: r8 >= 225)
	// do not clear the olive predicate, so a qualifying window is the arrow.
	arrowMinPx = 150
	// arrowHighPx is the HIGH-CONFIDENCE arrow floor (runApproach's
	// movement ladder, step 2): the measured 2026-10-20 11:28 run's
	// sunlit-deck false positives run 150-324 px, and a cluster at or above
	// this floor is the arrow (the walk may aim at it) — the floor sits in
	// the measured gap (324 < 380 < 445, the restored-deck calibration's
	// arrow). NOTE the arrow's size varies with camera distance and its own
	// pulse (the 11:28 run's LANDING arrow measured 194 px — below the
	// floor), so size alone is not the movement signal: a SINGLE qualifying
	// cluster (ladder step 1) is trusted at any size, and below both the
	// vision walk-dir leads (step 3).
	arrowHighPx = 380
	// walkHold is one walk step (the character's ~1.5 deck planks; the
	// bridge re-centers the stick after the hold).
	walkHold = 600 * time.Millisecond
	// approachBudget bounds the walk-to-Aryll phase (measured expectation:
	// a dozen or so 2 s iterations).
	approachBudget = 8 * time.Minute
	// convBudget bounds the conversation phase (a handful of box lines +
	// letterboxed story-subtitle interludes + the item box; the dense
	// recording's clock runs in parallel).
	convBudget = 12 * time.Minute
	// interludeBudget bounds ONE letterboxed interlude (the story subtitles
	// auto-advance; a long one plus the fade is a few seconds, the generous
	// bound is the "stuck in an unexpected cutscene" fail-fast).
	interludeBudget = 45 * time.Second
	// boxGoneBudget bounds a NON-letterboxed box absence after the box was
	// seen (the box closed without an interlude and without the item box —
	// the conversation ended early, or the screen changed unexpectedly).
	boxGoneBudget = 5 * time.Second
	// visionStuckCap is how many vision tiebreakers one stuck approach may
	// spend (a camera that keeps sweeping and never finds the arrow is
	// failing with evidence, not looping).
	visionStuckCap = 8
	// speakAskCap bounds the approach's vision label reads (the OCR band is
	// the primary gate; the model is the fallback for the small UI text).
	speakAskCap = 12
	// visionCallCap bounds the whole run's vision asks (evidence: the
	// budget number the log line reports). The conversation's item check
	// gates on the layout hash (one ask per new layout, the 2 s reconfirm
	// doubles it), so a dozen-or-so box lines plus the interludes fit.
	visionCallCap = 24
	// itemLayoutDrift is the aHash64 Hamming floor that counts as "a new
	// layout": the item box (the darkened scene + the icon layout) flips
	// most of the 64 bits against a dialogue box; a typewriter reveal
	// inside one box moves few.
	itemLayoutDrift = 10
)

// TestRealRecordTelescopeFixture is the G8 item recording gate (real
// Dolphin only; NOT RUN without the image/tools).
func TestRealRecordTelescopeFixture(t *testing.T) {
	skipUnlessTestDolphinGate(t)
	img := os.Getenv(EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set — point it at the user-supplied game image (dev-only, the image is never logged)", EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo", "xdotool", "tesseract"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH (dev host dependencies: dolphin-emu, xorg-server-xvfb, ffmpeg, x11-utils, xdotool, tesseract): %v", bin, err)
		}
	}
	if err := strayDolphinProcesses(); err != nil {
		t.Skipf("NOT RUN: %v", err)
	}

	root := fixtureRootForTest(t)
	fixtureState := filepath.Join(root, StateRelFirstDialogue)
	if _, err := os.Stat(fixtureState); err != nil {
		t.Skipf("NOT RUN: the %s fixture state is missing (%v) — record it first via TestRealRecordSavestateFixtures (GN_GATE=test-dolphin)", StateRelFirstDialogue, err)
	}
	// Golden gate (ADR-0053): the verified fixture + its referenced states
	// (both the deck checkpoint and the pre-item-box save hash) is a stable
	// artifact — NOT re-recorded.
	if ok, why := verifiedFixtureAndStates(t, root, FixtureIDTelescope); ok {
		t.Logf("fixture %s present and verified (states: %s) — NOT re-recorded", FixtureIDTelescope, why)
		return
	}

	evOut := filepath.Join("artifacts", "runs", "dolphin-telescope-"+time.Now().UTC().Format("2006-01-02-150405"))
	if err := os.MkdirAll(evOut, 0o755); err != nil {
		t.Fatalf("evidence dir %s: %v", evOut, err)
	}
	t.Logf("evidence: %s", evOut)

	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, LaunchTimeout: 180 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	defer b.Stop()

	// The vision probe (the drive's eyes): configured-or-degraded.
	vision, verr := newVisionProbe()
	if verr != nil {
		t.Logf("vision probe: %v — the drive runs structural-only (the item box is then detected by its layout change and the Tesseract reading's confidence; the run's evidence frames carry the record for a human check)", verr)
	} else {
		t.Logf("vision probe: model=%s (the item-box / Speak-label / camera judgments)", vision.Model())
	}

	// One session, hotkeys on (the deck save + the pre-item save ride the
	// F1 slot): boot from the proven first-dialogue state, the CLOSE step to
	// the deck, the deck-state save, then the approach + conversation under
	// the fixture recorder.
	if err := b.Launch(ctx, LaunchSpec{SaveState: fixtureState, Hotkeys: true}); err != nil {
		t.Fatalf("launch (save_state=first-dialogue, hotkeys): %v", err)
	}
	st := b.State()
	t.Logf("launched: display=%s dolphin_pid=%d user_dir=%s hotkey_device=%s image_sha256=%s",
		st.Display, st.DolphinPID, st.UserDir, st.HotkeyDevice, st.ImageSHA)

	// Phase 1: the first dialogue box (the state's own screen).
	boxDeadline := time.Now().Add(10 * time.Minute)
	boxSeen := false
	for !boxSeen {
		if time.Now().After(boxDeadline) {
			t.Fatalf("the first dialogue box never appeared within 10 m of the %s restore", StateRelFirstDialogue)
		}
		shot, err := b.Screenshot(ctx)
		if err == nil && ScreenSigOf(shot).RealBoxForm() {
			boxSeen = true
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context done: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	t.Logf("the first dialogue box is on screen — running the CLOSE step")

	// Phase 2: the CLOSE advance to the gameplay (deck) scene.
	xRef, err := CloseAdvance(ctx, b, evOut)
	if err != nil {
		t.Fatalf("CLOSE: %v", err)
	}
	t.Logf("gameplay scene (the deck X): %s", ScreenSigOf(xRef).SigLine())

	// Phase 3: the deck checkpoint (the stable state this recording starts
	// from; the manifest references it by hash).
	deckSlot, err := b.SaveState(ctx, stateSaveSlot)
	if err != nil {
		t.Fatalf("deck save: %v", err)
	}
	if _, err := ParseStateHeader(deckSlot, "GZLE01"); err != nil {
		t.Fatalf("deck state header: %v", err)
	}
	deckState := filepath.Join(root, StateRelDeck)
	if err := copyFileVerified(deckSlot, deckState); err != nil {
		t.Fatalf("copy deck state: %v", err)
	}
	if sum, err := sha256File(deckState); err == nil {
		t.Logf("deck checkpoint saved (slot 1): %s sha256=%s", StateRelDeck, sum)
	}

	// Phase 4: the recorder (the approach at 1 Hz, the conversation + item
	// box at 10 Hz) and the drive.
	rec, err := NewFixtureRecorder(b, root, FixtureIDTelescope)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	rec.SetCapturePlan(denseCapturePlan)
	rec.SetLineIDPrefix("ww-aryll")
	if err := rec.SetContentKind("item"); err != nil {
		t.Fatalf("content kind: %v", err)
	}
	if err := rec.StartCapture(ctx); err != nil {
		t.Fatalf("start capture: %v", err)
	}

	approachOK := runApproach(ctx, t, rec, vision, evOut)
	if !approachOK {
		rec.StopCapture()
		t.Fatalf("approach: the Speak label never measured on screen within %s (the drive's evidence frames are at %s)", approachBudget, evOut)
	}

	// The speaking A (the conversation starts).
	if err := rec.Inject(ctx, Buttons{A: true}, Axes{}, 500*time.Millisecond); err != nil {
		rec.StopCapture()
		t.Fatalf("speak A: %v", err)
	}
	t.Logf("speak A sent — the conversation is on screen")

	itemOK, itemFirstIdx, preItemState, err := runConversation(ctx, t, rec, vision, evOut)
	if err != nil {
		rec.StopCapture()
		t.Fatalf("conversation: %v", err)
	}
	if !itemOK {
		rec.StopCapture()
		t.Fatalf("the item box never measured on screen within %s (vision calls: %d; the evidence frames are at %s)", convBudget, visionCalls(vision), evOut)
	}

	// The content mark: the first recorded frame the item box was on
	// screen (the frame the item run's reveal started with — the index the
	// manifest's Content block names).
	if err := rec.MarkContentFrame(itemFirstIdx); err != nil {
		rec.StopCapture()
		t.Fatalf("content mark: %v", err)
	}

	// The pre-item-box savestate reference (the state the conversation
	// saved right before the advance that produced the item box).
	if preItemState != "" {
		if err := rec.AddState("pre-item-box", stateSaveSlot, StateRelPreItem, string(DriveScreenDialogueBox)); err != nil {
			rec.StopCapture()
			t.Fatalf("pre-item-box state reference: %v", err)
		}
	}
	// The deck checkpoint reference (this recording's start).
	if err := rec.AddState("deck", stateSaveSlot, StateRelDeck, string(DriveScreenDialogueBox)); err != nil {
		rec.StopCapture()
		t.Fatalf("deck state reference: %v", err)
	}

	summary, err := rec.Finalize(ctx, DriveTrace{OK: true, Reason: "the item box was measured on screen (the G8 item recording)"})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	t.Logf("fixture: %s frames=%d lines=%d manifest_sha256=%s — update testdata/scenarios/dolphin/item-telescope.yaml's input.manifest_sha256",
		summary.ID, summary.Frames, summary.Lines, summary.ManifestSHA256)
}

// verifiedFixtureAndStates is the golden gate for this recording: the
// fixture verifies (every frame hashes) AND every savestate it references
// exists and hashes to the manifest's claim (the deck checkpoint and the
// pre-item-box save — a fixture missing either state cannot be replayed
// from its checkpoint, so the pair is not "present").
func verifiedFixtureAndStates(t *testing.T, root, id string) (bool, string) {
	t.Helper()
	dir := filepath.Join(root, id)
	man, err := VerifyFixture(dir)
	if err != nil {
		return false, ""
	}
	if len(man.States) == 0 {
		return false, ""
	}
	var details []string
	for _, st := range man.States {
		p := filepath.Join(filepath.Dir(dir), st.File)
		sum, err := sha256File(p)
		if err != nil {
			return false, ""
		}
		if sum != st.SHA256 {
			return false, ""
		}
		details = append(details, st.Label+"@"+st.File)
	}
	return true, joinStrings(details, ", ")
}

// --- the approach: walk to Aryll until the A label reads "Speak" -----------

// isOliveArrowPixel is the talk arrow's measured color predicate. The
// restored-deck evidence (artifacts/runs/dolphin-savestate-2026-10-20-
// 085709/restored-deck.png) measured the arrow at (0.60, 0.34) as a 445 px
// cluster, 33x25 bbox, densest rgb (162-166, 177-179, 81-84): an
// OLIVE-GOLD fill with green at or above red — not the saturated green the
// first drive calibrated on (that predicate found no qualifying window on
// the deck, which is why the first runs never saw the arrow). The clauses:
// g >= r-10 (g at or above r), g >= 140 (the arrow is bright), g-b >= 50
// (the yellow family — b far below g), 130 <= r < 225 (the deck's
// pure-yellow sunlit patches measure rgb 250,235,16 — r >= 225 keeps them
// out; scene greens and the sea are darker or less yellow). Calibrated by
// densest-blob clustering over the measured frame: the arrow is the only
// qualifying object above the count floor; its sole competitor is the
// bottom-right prompt UI (115 px at (0.86, 0.90)), which the window's
// cy <= 0.66 clause excludes.
func isOliveArrowPixel(r8, g8, b8 int) bool {
	return g8 >= r8-10 && g8 >= 140 && g8-b8 >= 50 && r8 >= 130 && r8 < 225
}

// arrowStats is one frame's olive talk-arrow measurement (the arrow is the
// only qualifying olive object on the deck scene; the sunlit patches and
// the sea do not clear the predicate).
type arrowStats struct {
	count int     // the densest 32 px window's olive pixels
	cx    float64 // centroid (frame fraction, 0..1)
	cy    float64
	ok    bool // the densest window met the arrow's density/compactness/fill/upper-mid tests
}

// oliveArrowStats finds the interaction arrow: the densest 32 px window of
// olive (isOliveArrowPixel — the arrow is a solid fill; the deck's sunlit
// yellow is r >= 225 and the scene greens are not the yellow family, so no
// window of them reaches the arrow's density). A candidate window is the
// arrow when it is dense (>=150 px), compact (the window's olive bounding
// box <=40 px on a side), solid (the count fills at least 30% of its
// bounding box — the arrow is a fill, scattered sunlit pixels are not),
// and upper-mid (the arrow floats over Aryll's head at cy ~0.34 — the only
// other qualifying blob on the deck, the bottom-right "A SPEAK" prompt UI
// at (0.86, 0.90), sits below the cy <= 0.66 clause). Calibrated on the
// captured restored-deck evidence (the arrow is ~400 dense olive px at
// (0.60, 0.34); the deck itself clears no qualifying window).
func oliveArrowStats(img image.Image) arrowStats {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	win, step := 32, 8
	var best arrowStats
	for wy := 0; wy+win <= h; wy += step {
		for wx := 0; wx+win <= w; wx += step {
			n, sx, sy := 0, 0, 0
			minx, miny, maxx, maxy := 1<<30, 1<<30, -1, -1
			for y := wy; y < wy+win; y++ {
				for x := wx; x < wx+win; x++ {
					r, g, bl, _ := img.At(x, y).RGBA()
					g8, r8, b8 := int(g>>8), int(r>>8), int(bl>>8)
					if isOliveArrowPixel(r8, g8, b8) {
						n++
						sx += x
						sy += y
						if x < minx {
							minx = x
						}
						if x > maxx {
							maxx = x
						}
						if y < miny {
							miny = y
						}
						if y > maxy {
							maxy = y
						}
					}
				}
			}
			if n <= 0 {
				continue
			}
			bw, bh := maxx-minx+1, maxy-miny+1
			cy := float64(sy) / float64(n) / float64(h)
			if n >= arrowMinPx && bw <= 40 && bh <= 40 && float64(n) >= 0.3*float64(bw*bh) && cy <= 0.66 {
				c := arrowStats{count: n, cx: float64(sx) / float64(n) / float64(w), cy: cy, ok: true}
				if c.count > best.count {
					best = c
				}
			}
		}
	}
	return best
}

// oliveArrowClusters returns every qualifying cluster (the same criteria
// as oliveArrowStats) deduplicated by proximity (windows overlapping the
// same blob are one cluster), densest first. The drive tracks one cluster
// across samples (continuity): the deck can carry more than one qualifying
// blob (the arrow plus a sunlit prop), and the densest per frame would flip
// between them while the arrow itself is the steady one.
func oliveArrowClusters(img image.Image) []arrowStats {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	win, step := 32, 8
	type cand struct {
		n, sx, sy              int
		minx, miny, maxx, maxy int
	}
	var cands []cand
	for wy := 0; wy+win <= h; wy += step {
		for wx := 0; wx+win <= w; wx += step {
			c := cand{minx: 1 << 30, miny: 1 << 30, maxx: -1, maxy: -1}
			for y := wy; y < wy+win; y++ {
				for x := wx; x < wx+win; x++ {
					r, g, bl, _ := img.At(x, y).RGBA()
					g8, r8, b8 := int(g>>8), int(r>>8), int(bl>>8)
					if isOliveArrowPixel(r8, g8, b8) {
						c.n++
						c.sx += x
						c.sy += y
						if x < c.minx {
							c.minx = x
						}
						if x > c.maxx {
							c.maxx = x
						}
						if y < c.miny {
							c.miny = y
						}
						if y > c.maxy {
							c.maxy = y
						}
					}
				}
			}
			if c.n <= 0 {
				continue
			}
			bw, bh := c.maxx-c.minx+1, c.maxy-c.miny+1
			cy := float64(c.sy) / float64(c.n) / float64(h)
			if c.n >= arrowMinPx && bw <= 40 && bh <= 40 && float64(c.n) >= 0.3*float64(bw*bh) && cy <= 0.66 {
				cands = append(cands, c)
			}
		}
	}
	// Deduplicate: a window overlapping an already-kept cluster's centre is
	// the same blob.
	var out []arrowStats
	for _, c := range cands {
		cx, cy := float64(c.sx)/float64(c.n)/float64(w), float64(c.sy)/float64(c.n)/float64(h)
		dupe := false
		for _, k := range out {
			if math.Abs(k.cx-cx)+math.Abs(k.cy-cy) < 0.08 {
				dupe = true
				break
			}
		}
		if !dupe {
			out = append(out, arrowStats{count: c.n, cx: cx, cy: cy, ok: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].count > out[j].count })
	return out
}

// speakLabelCrop is the A-button prompt's reading band: the bottom-right
// corner (the prompt UI sits at ~(0.86, 0.90) on the restored-deck
// evidence — the button glyph just left of the word). The crop is OCR'd at
// 4x (the prompt text is small at 640x480). NOTE: the measured OCR readings
// of this band are low-confidence (the small stylized font), so the OCR is
// the fast signal only — the vision model's reading of the same band is the
// drive's primary "Speak" judgment (runApproach stops on the model's two
// consecutive YESes).
var speakLabelCrop = [4]float64{0.60, 0.82, 0.99, 0.99}

// speakLabelFold OCRs the prompt band and returns the reading folded the
// matcher's way (lowercased, punctuation stripped, OCR-folded) — the
// comparison is a substring check ("speak"), so the button glyph's own
// OCR noise does not defeat it.
func speakLabelFold(ctx context.Context, img image.Image) (string, error) {
	c := vision.CropNormalized(img, vision.RectOf(games.Region{
		RectX: speakLabelCrop[0], RectY: speakLabelCrop[1],
		RectW: speakLabelCrop[2] - speakLabelCrop[0], RectH: speakLabelCrop[3] - speakLabelCrop[1],
	}), vision.CropOptions{})
	if c == nil || c.Bounds().Empty() {
		return "", nil
	}
	res, err := ocr.Tesseract{PSM: ocr.DefaultPSM}.Recognize(ctx, enlarge(c, 4))
	if err != nil {
		return "", err
	}
	return textnorm.Normalize(res.Text, textnorm.Options{StripPunctuation: true, OCRFold: true}), nil
}

// runApproach drives the approach: 2 s cadence. It stops on the "Speak"
// label measured by the VISION MODEL (the measured 2026-10-20 run: the
// model is stable on the qSpeak read — yes on the landing frame, no on
// every frame after the prompt was lost — while the label's Tesseract
// reading on this small font is low-confidence and stays logged as the fast
// signal only).
//
// The landing state is MEASURED (the restore lands facing Aryll with the
// prompt up: the qSpeak read is yes on the first sample), so the FIRST yes
// drops the drive into the CONFIRM phase: no movement, the question
// re-asked at the 4 s gate, two consecutive yeses the stop. This prevents
// the 2026-10-20 11:28 failure mode: the first yes walked (the tracked
// cluster was a sunlit-deck false positive at (0.32, ...), not the arrow at
// (0.60, 0.34)), Link crossed the deck out of range, and the prompt was
// gone for the rest of the budget.
//
// A LOST prompt is hunted with the vision model's WALK DIRECTION as the
// primary signal (it sees the quest marker the structural detector cannot
// isolate from the deck's sunlit patches — the measured false positives run
// 150-324 px, the landing arrow 445 px): the structural walk toward a
// cluster runs only on a HIGH-CONFIDENCE cluster (arrowHighPx), and the
// camera sweep is the fallback when the model says none.
func runApproach(ctx context.Context, t *testing.T, rec *FixtureRecorder, vision *visionProbe, evOut string) bool {
	t.Helper()
	deadline := time.Now().Add(approachBudget)
	const askGate = 4 * time.Second
	var speakOCRStreak, visionYesStreak, confirmNoStreak int
	var lastAskAt, lastWalkDirAt time.Time
	askCount, walkDirUsed := 0, 0
	confirming := false
	for {
		if time.Now().After(deadline) {
			return false
		}
		img, err := rec.Screenshot(ctx)
		if err != nil {
			t.Logf("approach: screenshot failed: %v", err)
			select {
			case <-ctx.Done():
				return false
			case <-time.After(2 * time.Second):
			}
			continue
		}
		sig := ScreenSigOf(img)
		c, _ := sig.ClassOf()
		clusters := oliveArrowClusters(img)
		clusterLine := "none"
		if len(clusters) > 0 {
			parts := make([]string, 0, len(clusters))
			for i, k := range clusters {
				parts = append(parts, fmt.Sprintf("%d:%dpx@(%0.2f,%0.2f)", i, k.count, k.cx, k.cy))
			}
			clusterLine = strings.Join(parts, " ")
		}
		// The high-confidence arrow: the only clusters the structural
		// walk trusts (the measured sunlit-deck false positives run
		// 150-324 px; the landing arrow measures 445 px).
		highConf := false
		var ar arrowStats
		for _, k := range clusters {
			if k.count >= arrowHighPx && k.count > ar.count {
				ar, highConf = k, true
			}
		}
		label, labelErr := speakLabelFold(ctx, img)
		labelNote := ""
		if labelErr != nil {
			labelNote = " (label OCR: " + labelErr.Error() + ")"
		}
		t.Logf("approach: class=%s clusters=[%s] highConf=%v label=%q confirm=%v yesStreak=%d%s", c, clusterLine, highConf, label, confirming, visionYesStreak, labelNote)

		// The OCR fast signal (three consecutive readings of the band; the
		// vision read is the gate — the measured label OCR on this font is
		// low-confidence).
		if strings.Contains(label, "speak") {
			speakOCRStreak++
			if speakOCRStreak >= 3 {
				storeEvidence(evOut, "approach-speak-ocr.png", img)
				t.Logf("approach: the A label reads SPEAK (3 consecutive OCR readings) — Link is facing Aryll")
				return true
			}
		} else {
			speakOCRStreak = 0
		}

		// The vision read of the label (the primary gate): at the 4 s gate,
		// one word back.
		if vision != nil && askCount < speakAskCap && time.Since(lastAskAt) >= askGate {
			askCount++
			lastAskAt = time.Now()
			ans, sha, verr := vision.Ask(ctx, img, qSpeak)
			if verr != nil {
				t.Logf("approach: vision label read unavailable: %v", verr)
			} else {
				t.Logf("approach: vision label read: %s (frame sha256 %s)", ans, shortSHA(sha))
				storeEvidence(evOut, fmt.Sprintf("approach-label-%02d.png", askCount), img)
				switch {
				case ans == "yes":
					visionYesStreak++
					confirmNoStreak = 0
					confirming = true
					if visionYesStreak >= 2 {
						storeEvidence(evOut, "approach-speak-vision.png", img)
						t.Logf("approach: the vision model reads the A label as SPEAK twice in a row (no motion between) — Link is facing Aryll")
						return true
					}
				case confirming:
					// The prompt was up a moment ago and is gone now: it
					// drops out as the camera settles or the step crosses
					// the range edge; two lost reads drop back to the hunt
					// before any more motion.
					visionYesStreak = 0
					confirmNoStreak++
					if confirmNoStreak >= 2 {
						confirming = false
						t.Logf("approach: the prompt is gone (2 lost reads) — back to the hunt")
					}
				default:
					visionYesStreak = 0
				}
			}
		}

		// The movement phase: NO motion while confirming (the confirm IS the
		// measurement; moving during it is how the 2026-10-20 11:28 run
		// lost the prompt). The movement signal ladder (the measured 11:28
		// run: the arrow itself measured 194 px on the landing frame — the
		// sunlit-deck false positives run 150-324 px — so SIZE ALONE cannot
		// tell them apart; uniqueness and the model can):
		//
		//   1. a SINGLE qualifying cluster is unambiguous (the measured
		//      landing frame: the only cluster, at the arrow's position) —
		//      aim-first at it;
		//   2. otherwise a HIGH-CONFIDENCE cluster (>=380 px, above the
		//      measured 324 px false-positive ceiling) — aim-first at it;
		//   3. otherwise the vision model's walk direction (it sees the
		//      marker the structural detector cannot isolate);
		//   4. otherwise the camera sweep.
		if !confirming {
			trust := len(clusters) == 1 || highConf
			if trust {
				if len(clusters) == 1 {
					ar = clusters[0]
				}
				// Aim-first: off-centre more than a quarter frame is a
				// camera nudge (walking while the camera is sideways
				// circles), centred is a forward walk.
				if math.Abs(ar.cx-0.5) > 0.25 {
					side := 1.0
					if ar.cx < 0.5 {
						side = -1.0
					}
					nudge := 0.7 * side
					t.Logf("approach: the arrow (single/high-confidence, %dpx) is off-centre (cx=%.2f) — camera nudge (StickX=%.2f)", ar.count, ar.cx, nudge)
					if err := rec.Inject(ctx, Buttons{}, Axes{StickX: nudge, StickY: 0}, 400*time.Millisecond); err != nil {
						t.Logf("approach: nudge inject: %v", err)
					}
				} else {
					sx, sy := walkVector(ar)
					t.Logf("approach: the arrow (single/high-confidence, %dpx) is centred — walking (stick x=%.2f y=%.2f)", ar.count, sx, sy)
					storeEvidence(evOut, fmt.Sprintf("approach-walk-%03d.png", walkDirUsed), img)
					if err := rec.Inject(ctx, Buttons{}, Axes{MainX: sx, MainY: sy}, walkHold); err != nil {
						t.Logf("approach: walk inject: %v", err)
					}
				}
			} else if vision != nil && walkDirUsed < visionStuckCap && time.Since(lastWalkDirAt) >= 6*time.Second {
				walkDirUsed++
				lastWalkDirAt = time.Now()
				ans, sha, verr := vision.Ask(ctx, img, qWalkDir)
				if verr != nil {
					t.Logf("approach: vision walk-dir unavailable: %v", verr)
				} else {
					t.Logf("approach: vision walk-dir: %s (frame sha256 %s)", ans, shortSHA(sha))
					storeEvidence(evOut, fmt.Sprintf("approach-stuck-%03d.png", walkDirUsed), img)
					switch ans {
					case "ahead":
						walkCtx(ctx, rec, 0, -1)
					case "left":
						walkCtx(ctx, rec, -1, -0.3)
					case "right":
						walkCtx(ctx, rec, 1, -0.3)
					case "behind":
						turnCamera(ctx, rec, 180)
					default: // "none": the marker is off-frame — sweep the camera
						turnCamera(ctx, rec, 90)
					}
				}
			} else {
				// Ambiguous clusters and no vision signal (unavailable or
				// budget spent): the camera sweep is the only honest motion
				// (the marker is a world-space object; a quarter turn finds
				// it or rules out this facing).
				t.Logf("approach: ambiguous clusters, no vision walk-dir — camera sweep (C-stick)")
				if err := rec.Inject(ctx, Buttons{}, Axes{StickX: 0.9, StickY: 0}, 300*time.Millisecond); err != nil {
					t.Logf("approach: sweep inject: %v", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
}

// walkVector maps the arrow's centroid to the main-stick vector. FORWARD
// dominates: the first drive attempt's lateral-heavy vector (x≈0.9, y≈0.3)
// put Link on a circle — the camera orbits with a circling character, and
// the arrow's screen position whips around the frame. A full-stick forward
// step with a mild lateral trim (the arrow's off-centre x, halved) closes
// the gap without circling; the camera nudge (runApproach's stall path)
// owns the horizontal alignment.
func walkVector(ar arrowStats) (sx, sy float64) {
	sx = clampStick((ar.cx - 0.5) * 1.2) // the lateral trim: at most 0.6, forward stays full
	sy = -1
	return sx, sy
}

// walkCtx is one walk step (the bridge re-centers the stick after the hold).
func walkCtx(ctx context.Context, rec *FixtureRecorder, sx, sy float64) {
	_ = rec.Inject(ctx, Buttons{}, Axes{MainX: sx, MainY: sy}, 700*time.Millisecond)
}

// turnCamera sweeps the C-stick: deg ≈ 90 (a quarter turn) or 180.
func turnCamera(ctx context.Context, rec *FixtureRecorder, deg int) {
	hold := 300 * time.Millisecond
	if deg >= 180 {
		hold = 600 * time.Millisecond
	}
	_ = rec.Inject(ctx, Buttons{}, Axes{StickX: 0.9, StickY: 0}, hold)
}

// clampStick bounds a stick axis to the pipe's -1..1.
func clampStick(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// --- the conversation: advance to the item box, save it before ------------

// runConversation rides the conversation under the recorder: the box's
// typewriter settle gates the advance, and at EVERY settle — before the
// advancing A — the item-box judgment runs: the vision model's double
// check on the settled layout (the item-get screen's layout flips the
// perceptual hash against the dialogue box, so the ask lands exactly on
// the new layout; a typewriter reveal inside one box stays within the
// drift floor). A savestate is saved at entry and BEFORE every advance it
// precedes — so the last save of the run is the pre-item-box state, the
// checkpoint the fixture references (the conversation's last dialogue line
// settled, one advance from the item box).
//
// The conversation carries LETTERBOXED beats (the story subtitles that
// auto-advance, ClassStorySub with text; the fades between them, the
// ClassDark/ClassScene cutscene family). They are not the box closing:
// the loop waits them out (the story-subtitle budget is the fail-fast for
// an unexpected long cutscene, the fade budget for a non-letterboxed
// absence) and never presses during one (a press mid-subtitle would skip
// a beat). On a SETTLED non-box layout the item judgment still runs (the
// item-get screen, if it does not classify as the box family, is detected
// visually and reported as an actionable calibration failure with its
// evidence frames — the fixture's item line must resolve to a box line).
//
// The settle-window reset keys on the box-RUN TRANSITION (the first box
// sample after an absence), the way the proven first-dialogue drive does:
// a reset keyed on the box being up every sample empties the window every
// sample and the settle check never accumulates its two samples (the
// measured 2026-10-18 17:00 run: box on screen ~2 m, zero presses).
//
// It returns (itemOK, the first recorded frame index the item box was on
// screen, the pre-item state file path, err).
func runConversation(ctx context.Context, t *testing.T, rec *FixtureRecorder, vision *visionProbe, evOut string) (bool, int, string, error) {
	t.Helper()
	deadline := time.Now().Add(convBudget)
	pace := &pressPace{cooldown: 1500 * time.Millisecond}
	noResp := &noRespWatch{}
	boxSeen := false
	var boxGoneSince time.Time
	var prevWasBox bool
	var lastAskedHash uint64
	var lastAskedAt time.Time
	visionAsked := 0
	nonBoxRunFirst := -1 // the first recorded frame of the current non-box run
	itemFirstIdx := 0
	preItem := ""

	// savePreItem is the conversation checkpoint (slot 1 → the stable
	// pre-item file): called at entry and before every advance, so the
	// last save of the run is the pre-item-box state.
	savePreItem := func() error {
		statePath, serr := rec.SaveState(ctx, stateSaveSlot)
		if serr != nil {
			return fmt.Errorf("pre-press save: %w", serr)
		}
		stable := filepath.Join(filepath.Dir(rec.Dir()), StateRelPreItem)
		if cerr := copyFileVerified(statePath, stable); cerr != nil {
			return fmt.Errorf("copy pre-item state: %w", cerr)
		}
		preItem = stable
		return nil
	}
	if err := savePreItem(); err != nil {
		return false, itemFirstIdx, preItem, err
	}
	t.Logf("conv: entry checkpoint saved (the fallback pre-item state): %s", StateRelPreItem)

	// itemBoxCheck is the judgment at one settled layout: the vision ask +
	// the 2 s reconfirm (the same layout must still read as the item box —
	// a fade or a box close in between fails it). It runs before the
	// advancing A, so the item box is never pressed away.
	itemBoxCheck := func(img image.Image) (bool, error) {
		if vision == nil {
			return false, nil // structural-only degradation: no visual judgment (logged by the caller)
		}
		if visionAsked >= visionCallCap {
			return false, nil
		}
		h := aHash64(img)
		if lastAskedHash != 0 && hamming(lastAskedHash, h) <= itemLayoutDrift {
			return false, nil // the same layout as the last ask: already judged
		}
		if time.Since(lastAskedAt) < 4*time.Second {
			return false, nil
		}
		visionAsked++
		ans, sha, verr := vision.Ask(ctx, img, qItemBox)
		if verr != nil {
			t.Logf("conv: vision item-check unavailable: %v", verr)
			return false, nil
		}
		lastAskedHash, lastAskedAt = h, time.Now()
		t.Logf("conv: vision item-check: %s (frame sha256 %s)", ans, shortSHA(sha))
		storeEvidence(evOut, fmt.Sprintf("conv-itemcheck-%02d.png", visionAsked), img)
		if ans != "yes" {
			return false, nil
		}
		// Reconfirm 2 s later: the same layout must still read as the item
		// box.
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		img2, err := rec.Screenshot(ctx)
		if err != nil {
			return false, fmt.Errorf("reconfirm screenshot: %w", err)
		}
		ans2, sha2, verr := vision.Ask(ctx, img2, qItemBox)
		if verr != nil {
			return false, fmt.Errorf("reconfirm ask: %w", verr)
		}
		t.Logf("conv: vision item recheck: %s (frame sha256 %s)", ans2, shortSHA(sha2))
		storeEvidence(evOut, "item-box.png", img2)
		if ans2 != "yes" {
			return false, nil
		}
		// The content mark: the last published frame at the double-confirm
		// is an item-box frame (the box persists until pressed), so the
		// mark resolves to the item line directly (its frame's settled
		// text), or — if that frame's text is still unrevealed — snaps
		// forward within the 30-frame bound, which stays inside the item
		// box (nothing else lands between the confirm and the return).
		itemFirstIdx = rec.FramesPublished() - 1
		return true, nil
	}

	for {
		if time.Now().After(deadline) {
			return false, itemFirstIdx, preItem, fmt.Errorf("the item box never appeared within %s (vision calls: %d; boxSeen=%v presses=%d)", convBudget, visionAsked, boxSeen, pace.presses)
		}
		img, err := rec.Screenshot(ctx)
		if err != nil {
			return false, itemFirstIdx, preItem, fmt.Errorf("screenshot: %w", err)
		}
		sig := ScreenSigOf(img)
		c, _ := sig.ClassOf()
		boxInk := sig.Regions["boxband"].edges
		t.Logf("conv: class=%s boxInk=%.4f boxSeen=%v presses=%d", c, boxInk, boxSeen, pace.presses)

		if c != ClassBox {
			if boxGoneSince.IsZero() {
				boxGoneSince = time.Now()
				pace.onFade()
				nonBoxRunFirst = rec.FramesPublished() - 1
			}
			if boxSeen {
				// Letterboxed beats auto-advance at their own pace; a
				// non-letterboxed absence (the box closed, the gameplay
				// scene) is the conversation ending early.
				limit := boxGoneBudget
				if c == ClassStorySub {
					limit = interludeBudget
				}
				if time.Since(boxGoneSince) >= limit {
					if c != ClassStorySub {
						// The settled non-box layout: the item judgment
						// before the fail (the item-get screen, if it does
						// not classify box, is detected visually and
						// reported with its evidence).
						if ok, cerr := itemBoxCheck(img); cerr != nil {
							return false, itemFirstIdx, preItem, cerr
						} else if ok {
							storeEvidence(evOut, "item-box-nonclass.png", img)
							return false, nonBoxRunFirst, preItem, fmt.Errorf("the item screen double-confirmed on a NON-box screen (class=%s, %s): the fixture's item line must resolve to a box line — the classifier needs the item screen's measured shape (evidence: item-box-nonclass.png)", c, sig.SigLine())
						}
						return false, itemFirstIdx, preItem, fmt.Errorf("the box was absent (class=%s) for %s after the last advance without the item box double-confirming (presses: %d; vision calls: %d)", c, limit, pace.presses, visionAsked)
					}
					return false, itemFirstIdx, preItem, fmt.Errorf("the letterboxed interlude (class=%s) persisted %s without the item box (presses: %d; vision calls: %d) — an unexpected cutscene or a stuck subtitle", c, limit, pace.presses, visionAsked)
				}
			}
			prevWasBox = false
			select {
			case <-ctx.Done():
				return false, itemFirstIdx, preItem, ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}

		// The box: a new RUN (the first appearance, or a reappearance after
		// an absence) restarts the settle window. The reset keys on the
		// transition (the proven first-dialogue pattern): reset on every box
		// sample and the settle check never accumulates its two samples.
		// (The content mark does not ride the run start: the box can stay
		// open across line advances, so the mark is the double-confirm
		// frame itself — see itemBoxCheck.)
		if !prevWasBox {
			pace.inkWindow = nil
			pace.lastInk = 0
		}
		boxSeen = true
		boxGoneSince = time.Time{}
		prevWasBox = true
		if noResp.observe(img) {
			return false, itemFirstIdx, preItem, fmt.Errorf("the box stopped responding to presses after %d presses without the item box double-confirming (vision calls: %d) — the conversation ended early", pace.presses, visionAsked)
		}
		if noResp.waiting() || !pace.onInk(boxInk) {
			select {
			case <-ctx.Done():
				return false, itemFirstIdx, preItem, ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}

		// The typewriter has settled: the advance decision point.
		noResp.anchorForPress(img)
		// The item judgment FIRST (before the advancing A presses the box
		// away): the settled layout is fully formed, and the item box's
		// double check is the stop.
		if ok, cerr := itemBoxCheck(img); cerr != nil {
			return false, itemFirstIdx, preItem, cerr
		} else if ok {
			t.Logf("conv: the ITEM BOX is on screen (double-confirmed) — the conversation is at the item-get")
			return true, itemFirstIdx, preItem, nil
		}
		// Not the item box (or the judgment is unavailable): save the
		// pre-advance state (the last save of the run is the pre-item-box
		// state) and press the advance.
		if serr := savePreItem(); serr != nil {
			return false, itemFirstIdx, preItem, serr
		}
		t.Logf("conv: pre-press state saved (press %d): %s", pace.presses+1, StateRelPreItem)
		if err := rec.Inject(ctx, Buttons{A: true}, Axes{}, 400*time.Millisecond); err != nil {
			return false, itemFirstIdx, preItem, fmt.Errorf("advance A: %w", err)
		}

		select {
		case <-ctx.Done():
			return false, itemFirstIdx, preItem, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// shortSHA trims a sha256 to its 12-hex prefix (the log's correlation key
// into the retained evidence PNGs — the full hash is the manifest's).
func shortSHA(sha string) string {
	if len(sha) >= 12 {
		return sha[:12]
	}
	return sha
}

// visionCalls is the probe's call count (0 for a nil probe — the
// structural-only degradation).
func visionCalls(v *visionProbe) int {
	if v == nil {
		return 0
	}
	return v.Calls()
}
