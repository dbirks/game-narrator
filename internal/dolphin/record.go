package dolphin

// record.go is the M6 fixture recorder (docs/spec/11 "Fixture specification",
// docs/spec/12 "Frame replay"): while a screen-driven drive runs, it records a
// replayable fixture into an EXTERNAL, user-configured directory —
// $GAME_NARRATOR_FIXTURE_DIR/<scenario-id>/ (default: the gitignored
// artifacts/fixtures/, AGENTS.md rule 5). A captured fixture of a commercial
// game is never committed: the committed scenario files reference it by id +
// manifest hash only, and replaying a missing or altered fixture is NOT RUN
// with setup instructions, never a pass.
//
// What one fixture carries (per-frame, at the drive's iteration cadence):
//
//   - the frame itself (PNG, lossless) and its capture timestamp;
//   - the screen-class label the drive's own classifier measured on it
//     (ScreenSigOf -> DriveScreenOf — the drive believes exactly this);
//   - the input log (every Inject with its wire description and time);
//   - the oracle: the drive's full transition log (screen, side, action) and
//     subtitle-timing record (which scene/state the drive believed it was in,
//     iteration by iteration).
//
// plus the replay side the scenario harness needs: truth.json (per-frame text
// as the real Tesseract engine read it at record time on the subtitle crop)
// and profile.json (the distinct narration lines as messages). The narration
// text is commercial game text: it lives ONLY in this gitignored fixture,
// never in a tracked file, test source, log or report (AGENTS.md rule 5).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// EnvFixtureDir names the external fixture root; the default keeps fixtures
// under the gitignored artifacts/ tree so a fresh checkout stays clean.
const EnvFixtureDir = "GAME_NARRATOR_FIXTURE_DIR"

// DefaultFixtureRoot is the fixture root when the env var is unset, relative
// to the working directory (the repository root under make and go test).
const DefaultFixtureRoot = "artifacts/fixtures"

// FixtureSchema is the fixture manifest schema version this recorder writes.
const FixtureSchema = 1

// FixtureDir resolves the external fixture root for this process.
func FixtureDir() string {
	if v := os.Getenv(EnvFixtureDir); v != "" {
		return v
	}
	return DefaultFixtureRoot
}

// FixtureFrame is one recorded frame: its capture time, the frame's own
// SHA-256 (the manifest's integrity claim), the drive's measured screen class
// and the signature key (the classifier's measured detail).
type FixtureFrame struct {
	File   string `json:"file"`
	TsNS   int64  `json:"ts_ns"`
	SHA256 string `json:"sha256"`
	Screen string `json:"screen"`
	Side   string `json:"side,omitempty"`
	Sig    string `json:"sig"`
}

// FixtureInput is one recorded Inject: the wire description the drive sent,
// stamped with the capture clock and the index of the last frame recorded
// before it (-1 when the input preceded the first frame).
type FixtureInput struct {
	TsNS  int64  `json:"ts_ns"`
	Frame int    `json:"frame"`
	Wire  string `json:"wire"`
}

// FixtureTransition is one row of the oracle: the drive's belief at one
// iteration (the measured screen, the cursor side, the action sent).
type FixtureTransition struct {
	Index  int    `json:"index"`
	Screen string `json:"screen"`
	Side   string `json:"side,omitempty"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

// FixtureSubTiming mirrors SubTiming for the manifest record.
type FixtureSubTiming struct {
	FirstIndex int   `json:"first_index"`
	LastIndex  int   `json:"last_index"`
	FadeInNS   int64 `json:"fade_in_ns"`
	HoldNS     int64 `json:"hold_ns"`
	FadeOutNS  int64 `json:"fade_out_ns"`
	Open       bool  `json:"open"`
}

// FixtureManifest is the fixture's manifest.json. It carries the SHA-256 of
// every frame and the game-image SHA only (the image path itself is never
// written: the image is a user-supplied secret, AGENTS.md rule 5).
type FixtureManifest struct {
	Schema          int              `json:"schema"`
	ID              string           `json:"id"`
	Origin          string           `json:"origin"`
	RecordedAt      string           `json:"recorded_at"`
	GameImageSHA256 string           `json:"game_image_sha256"`
	Dolphin         FixtureDolphin   `json:"dolphin"`
	Resolution      FixtureSize      `json:"resolution"`
	Frames          []FixtureFrame   `json:"frames"`
	Inputs          []FixtureInput   `json:"inputs"`
	Oracle          FixtureOracle    `json:"oracle"`
	OCR             FixtureOCRRecipe `json:"ocr"`
}

type FixtureDolphin struct {
	Display string `json:"display"`
	UserDir string `json:"user_dir"`
	PID     int    `json:"dolphin_pid"`
}

type FixtureSize struct {
	W int `json:"w"`
	H int `json:"h"`
}

// FixtureOracle is the record of which scene/state the drive believed it was
// in, iteration by iteration: the run's outcome and its full transition log.
type FixtureOracle struct {
	OK          bool                `json:"ok"`
	Reason      string              `json:"reason"`
	Transitions []FixtureTransition `json:"transitions"`
	SubTimings  []FixtureSubTiming  `json:"sub_timings"`
	SuccessText bool                `json:"success_text,omitempty"`
}

// FixtureOCRRecipe records how the fixture's truth text was produced, so a
// replay can state what "the OCR said" means: the engine, the page mode, the
// enlargement and the normalised subtitle crop the engine saw.
type FixtureOCRRecipe struct {
	Engine string     `json:"engine"`
	PSM    int        `json:"psm"`
	Scale  int        `json:"scale"`
	Region [4]float64 `json:"region"`
}

// FixtureSummary is Finalize's hand-back: where the fixture lives and its
// manifest's SHA-256 — the hash a committed scenario references.
type FixtureSummary struct {
	Dir            string
	ID             string
	ManifestPath   string
	ManifestSHA256 string
	Frames         int
	Lines          int // distinct narration lines in profile.json
}

// FixtureRecorder is the Bridge wrapper that records a fixture while a drive
// runs. Wrap the real bridge BEFORE the drive sees it:
//
//	rec := NewFixtureRecorder(b, id)
//	rec.Launch(ctx, spec)
//	drv, _ := RunDrive(ctx, rec, opts, table)
//	summary, _ := rec.Finalize(ctx, drv)
//
// Every Screenshot is recorded (frame file + capture timestamp + the
// classifier's screen label); every Inject is logged with its wire
// description. The classifier runs the drive's OWN measurement path
// (ScreenSigOf -> DriveScreenOf), so the recorded labels are exactly the
// screen the drive believed was on screen — no second opinion.
type FixtureRecorder struct {
	inner Bridge
	id    string
	root  string // the fixture's own dir: <fixture root>/<id>

	mu       sync.Mutex
	frames   []FixtureFrame
	inputs   []FixtureInput
	st       Status // the launch's status (image SHA, user dir, display, pid)
	launched bool
}

// NewFixtureRecorder starts recording into <fixture root>/<id>. The dir tree
// is created here so a recording failure is visible before any frame is
// silently lost.
func NewFixtureRecorder(b Bridge, root, id string) (*FixtureRecorder, error) {
	if id == "" {
		return nil, fmt.Errorf("dolphin: fixture id is empty")
	}
	if strings.ContainsAny(id, `/\`+"\x00") {
		return nil, fmt.Errorf("dolphin: fixture id %q is not a safe directory name", id)
	}
	if id == "." || id == ".." {
		// No separator in the id, so these are the only two that could
		// escape the fixture root (".." to its parent, "." the root itself).
		return nil, fmt.Errorf("dolphin: fixture id %q is not allowed", id)
	}
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "frames"), 0o755); err != nil {
		return nil, fmt.Errorf("dolphin: fixture dir %s: %w", dir, err)
	}
	return &FixtureRecorder{inner: b, id: id, root: dir}, nil
}

// Dir is the fixture's directory (<root>/<id>).
func (r *FixtureRecorder) Dir() string { return r.root }

// --- the Bridge surface the drive talks through ---------------------------

// Launch delegates to the inner bridge and remembers the launch's status
// (the image SHA the manifest carries is taken from there, never from the
// path the caller passed).
func (r *FixtureRecorder) Launch(ctx context.Context, spec LaunchSpec) error {
	if err := r.inner.Launch(ctx, spec); err != nil {
		return err
	}
	r.mu.Lock()
	r.st = r.inner.State()
	r.launched = true
	r.mu.Unlock()
	return nil
}

// Screenshot records the frame (file, capture timestamp, measured screen
// class) and hands the same pixels to the drive. A write failure is fatal to
// the run: a fixture missing a frame is a lie about the recording.
func (r *FixtureRecorder) Screenshot(ctx context.Context) (image.Image, error) {
	img, err := r.inner.Screenshot(ctx)
	if err != nil {
		return nil, err
	}
	ts := time.Now()
	r.mu.Lock()
	seq := len(r.frames)
	r.mu.Unlock()

	name := fmt.Sprintf("frame-%05d.png", seq+1)
	path := filepath.Join(r.root, "frames", name)
	buf := new(bytes.Buffer)
	if err := png.Encode(buf, img); err != nil {
		return nil, fmt.Errorf("dolphin: fixture encode frame %d: %w", seq+1, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return nil, fmt.Errorf("dolphin: fixture write frame %d: %w", seq+1, err)
	}
	sum := sha256.Sum256(buf.Bytes())

	sig := ScreenSigOf(img)
	screen, side := DriveScreenOf(sig)
	fr := FixtureFrame{
		File:   name,
		TsNS:   ts.UnixNano(),
		SHA256: hex.EncodeToString(sum[:]),
		Screen: string(screen),
		Side:   string(side),
		Sig:    sig.Key(),
	}
	r.mu.Lock()
	r.frames = append(r.frames, fr)
	r.mu.Unlock()
	return img, nil
}

// Inject logs the input (wire description + capture timestamp + the last
// recorded frame) before delegating, so the log reads as "this input landed
// after this frame".
func (r *FixtureRecorder) Inject(ctx context.Context, buttons Buttons, axes Axes, hold time.Duration) error {
	r.mu.Lock()
	fr := -1
	if len(r.frames) > 0 {
		fr = len(r.frames) - 1
	}
	in := FixtureInput{TsNS: time.Now().UnixNano(), Frame: fr, Wire: injectWire(buttons, axes)}
	r.mu.Unlock()
	if err := r.inner.Inject(ctx, buttons, axes, hold); err != nil {
		return err
	}
	r.mu.Lock()
	r.inputs = append(r.inputs, in)
	r.mu.Unlock()
	return nil
}

// The rest of the Bridge surface passes through untouched.
func (r *FixtureRecorder) FrameSource(fps int) (capture.Source, error) {
	return r.inner.FrameSource(fps)
}
func (r *FixtureRecorder) SaveSnapshot(label string) (string, error) {
	return r.inner.SaveSnapshot(label)
}
func (r *FixtureRecorder) WaitForGameFrame(ctx context.Context, timeout time.Duration) (Evidence, error) {
	return r.inner.WaitForGameFrame(ctx, timeout)
}
func (r *FixtureRecorder) ProveInput(ctx context.Context) (Evidence, error) {
	return r.inner.ProveInput(ctx)
}
func (r *FixtureRecorder) SaveState(ctx context.Context, slot int) (string, error) {
	return r.inner.SaveState(ctx, slot)
}
func (r *FixtureRecorder) LoadState(ctx context.Context, slot int) error {
	return r.inner.LoadState(ctx, slot)
}
func (r *FixtureRecorder) State() Status { return r.inner.State() }
func (r *FixtureRecorder) Stop()         { r.inner.Stop() }

// --- Finalize: manifest, truth, profile ------------------------------------

// OCRConfFloor is the Tesseract confidence floor a subtitle reading must clear
// to become truth text; below it the frame's text is "" (the reading was
// noise, and recording it would invent a line).
const OCRConfFloor = 0.4

// Finalize closes the recording: it writes manifest.json (frames + hashes +
// inputs + the drive's oracle), truth.json and profile.json (the narration
// lines as Tesseract read them on the subtitle crop), verifies every frame's
// hash, and returns the summary with the manifest's SHA-256.
//
// The OCR step runs the real Tesseract on every frame the drive measured as
// the story-subtitle class (the drive only claims that class on a frame that
// MEASURES the bottom-bar text). It is the fixture's provenance: the truth is
// what the engine said at record time, on the real frames — the same
// "recorded OCR answer" convention the synthetic corpus uses
// (docs/spec/11 "Fixture origins").
func (r *FixtureRecorder) Finalize(ctx context.Context, drv DriveTrace) (FixtureSummary, error) {
	r.mu.Lock()
	frames := append([]FixtureFrame(nil), r.frames...)
	inputs := append([]FixtureInput(nil), r.inputs...)
	st := r.st
	launched := r.launched
	r.mu.Unlock()

	if len(frames) == 0 {
		return FixtureSummary{}, fmt.Errorf("dolphin: fixture %s recorded no frames", r.id)
	}
	imageSHA := ""
	if launched {
		imageSHA = st.ImageSHA
	}
	if imageSHA == "" {
		// The drive can run against a pre-launched bridge; take the SHA from
		// the live status. Either way the path never lands in the manifest.
		imageSHA = r.inner.State().ImageSHA
	}

	// The truth text, per frame. Non-subtitle frames carry no narration text
	// in the region (the presence/absence of the bottom bar is the point of
	// those scenarios), so only the subtitle frames are OCR'd.
	texts := make([]string, len(frames))
	canonical := []string{} // distinct lines, in first-seen order
	seen := map[string]int{}
	resolution := FixtureSize{W: st.Region.W, H: st.Region.H}
	recipe := FixtureOCRRecipe{Engine: "tesseract", PSM: ocr.DefaultPSM, Scale: ocrSubtitleScale, Region: StorySubCrop}

	for i, fr := range frames {
		if fr.Screen != string(DriveScreenStorySubtitle) {
			continue
		}
		text, err := r.ocrFrame(ctx, fr.File)
		if err != nil {
			return FixtureSummary{}, fmt.Errorf("dolphin: fixture OCR frame %s: %w", fr.File, err)
		}
		texts[i] = text
		if text == "" {
			continue
		}
		key := ocrFoldKey(text)
		if idx, ok := seen[key]; !ok {
			seen[key] = len(canonical)
			canonical = append(canonical, text)
			continue
		} else if canonical[idx] == "" {
			canonical[idx] = text
		}
	}

	manifest := r.buildManifest(frames, inputs, st, drv, imageSHA, resolution, recipe)
	summary := FixtureSummary{Dir: r.root, ID: r.id, Frames: len(frames)}

	if err := writeJSON(filepath.Join(r.root, "manifest.json"), manifest); err != nil {
		return summary, err
	}
	if err := r.writeTruth(frames, texts, canonical); err != nil {
		return summary, err
	}
	lines, err := r.writeProfile(canonical, resolution)
	if err != nil {
		return summary, err
	}
	summary.Lines = lines

	// Self-verify before handing the fixture back: every frame file must
	// exist and hash to what the manifest claims (the same check the replay
	// side runs, so a corrupt recording fails here, not in a scenario).
	if _, err := VerifyFixture(r.root); err != nil {
		return summary, err
	}

	manPath := filepath.Join(r.root, "manifest.json")
	sha, err := sha256File(manPath)
	if err != nil {
		return summary, err
	}
	summary.ManifestPath = manPath
	summary.ManifestSHA256 = sha
	return summary, nil
}

// ocrFrame runs the real Tesseract on one frame's subtitle crop. The crop is
// the drive's measured StorySubCrop, enlarged ocrSubtitleScale× (Catmull-Rom)
// before the engine sees it — a 480p letterbox line is too small for Tesseract
// at native size, and the enlargement is part of the recorded recipe.
func (r *FixtureRecorder) ocrFrame(ctx context.Context, file string) (string, error) {
	img, err := readPNGFile(filepath.Join(r.root, "frames", file))
	if err != nil {
		return "", err
	}
	crop := vision.CropNormalized(img, vision.RectOf(games.Region{
		RectX: StorySubCrop[0], RectY: StorySubCrop[1],
		RectW: StorySubCrop[2] - StorySubCrop[0], RectH: StorySubCrop[3] - StorySubCrop[1],
	}), vision.CropOptions{})
	if crop == nil || crop.Bounds().Empty() {
		return "", nil
	}
	big := enlarge(crop, ocrSubtitleScale)
	res, err := ocr.Tesseract{PSM: ocr.DefaultPSM}.Recognize(ctx, big)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(res.Text)
	if text == "" || res.MeanConf < OCRConfFloor {
		return "", nil
	}
	return text, nil
}

// writeTruth writes truth.json in the fixture's ground-truth shape: one row
// per frame with the textbox presence in the region and the OCR text. Present
// is true exactly where the drive measured the narration's bottom bar
// (story_subtitle); elsewhere the region is not a textbox even if pixels are.
func (r *FixtureRecorder) writeTruth(frames []FixtureFrame, texts []string, canonical []string) error {
	type row struct {
		File    string `json:"file"`
		TsNS    int64  `json:"ts_ns"`
		Present bool   `json:"box_present"`
		Text    string `json:"text"`
		Line    string `json:"line"`
		Full    bool   `json:"text_is_full_line"`
	}
	canonLine := map[string]string{}
	for i, c := range canonical {
		canonLine[ocrFoldKey(c)] = string(rune('a' + i))
	}
	rows := make([]row, 0, len(frames))
	for i, fr := range frames {
		sub := fr.Screen == string(DriveScreenStorySubtitle)
		text := ""
		if i < len(texts) {
			text = texts[i]
		}
		ln := ""
		full := false
		if text != "" {
			ln = canonLine[ocrFoldKey(text)]
			full = ln != ""
		}
		rows = append(rows, row{File: fr.File, TsNS: fr.TsNS, Present: sub, Text: text, Line: ln, Full: full})
	}
	return writeJSON(filepath.Join(r.root, "truth.json"), rows)
}

// writeProfile writes profile.json: the distinct narration lines as messages
// (ids carry no game text — "ww-legend-NN" — so the committed scenario YAML
// can name them freely), consecutive lines linked by next edges, and the
// measured bottom-bar region.
func (r *FixtureRecorder) writeProfile(canonical []string, resolution FixtureSize) (int, error) {
	type msg struct {
		ID           string `json:"id"`
		OriginalText string `json:"original_text"`
		SceneID      string `json:"scene_id"`
	}
	type edge struct {
		From       string  `json:"from"`
		To         string  `json:"to"`
		Type       string  `json:"type"`
		Confidence float64 `json:"confidence"`
	}
	messages := make([]msg, 0, len(canonical))
	for i, c := range canonical {
		messages = append(messages, msg{ID: fmt.Sprintf("ww-legend-%02d", i+1), OriginalText: c})
	}
	edges := make([]edge, 0, max(len(canonical)-1, 0))
	for i := 1; i < len(canonical); i++ {
		edges = append(edges, edge{
			From: fmt.Sprintf("ww-legend-%02d", i), To: fmt.Sprintf("ww-legend-%02d", i+1),
			Type: "next", Confidence: 0.9,
		})
	}
	p := map[string]any{
		"id":     r.id,
		"origin": "dolphin",
		"description": "Recorded Wind Waker opening-legend fixture (docs/spec/11 'Fixture origins'): " +
			"the narration lines are the real Tesseract readings of the drive's subtitle frames, " +
			"recorded at capture time; the profile, truth and frames live in this gitignored fixture only " +
			"(AGENTS.md rule 5). No game text appears in any tracked file.",
		"text_kind": "dialogue",
		"messages":  messages,
		"edges":     edges,
		"regions": []map[string]any{{
			"id":   "bottom",
			"type": "textbox",
			"x":    StorySubCrop[0], "y": StorySubCrop[1],
			"w": StorySubCrop[2] - StorySubCrop[0], "h": StorySubCrop[3] - StorySubCrop[1],
		}},
		"resolution": resolution,
	}
	return len(canonical), writeJSON(filepath.Join(r.root, "profile.json"), p)
}

// buildManifest assembles the manifest from the recorded frames, the input
// log, the launch status and the drive's oracle (its transition log is the
// record of which scene/state the drive believed it was in).
func (r *FixtureRecorder) buildManifest(frames []FixtureFrame, inputs []FixtureInput, st Status, drv DriveTrace, imageSHA string, resolution FixtureSize, recipe FixtureOCRRecipe) FixtureManifest {
	oracle := FixtureOracle{OK: drv.OK, Reason: drv.Reason, SuccessText: drv.SuccessText}
	for _, tr := range drv.Transitions {
		oracle.Transitions = append(oracle.Transitions, FixtureTransition{
			Index:  tr.Index,
			Screen: string(tr.Screen),
			Side:   string(tr.Side),
			Action: tr.Action,
			Detail: tr.Detail,
		})
	}
	for _, st2 := range drv.SubTimings {
		oracle.SubTimings = append(oracle.SubTimings, FixtureSubTiming{
			FirstIndex: st2.FirstIndex, LastIndex: st2.LastIndex,
			FadeInNS: int64(st2.FadeIn), HoldNS: int64(st2.Hold), FadeOutNS: int64(st2.FadeOut),
			Open: st2.Open,
		})
	}
	return FixtureManifest{
		Schema:          FixtureSchema,
		ID:              r.id,
		Origin:          "dolphin",
		RecordedAt:      time.Now().UTC().Format(time.RFC3339),
		GameImageSHA256: imageSHA,
		Dolphin: FixtureDolphin{
			Display: st.Display, UserDir: st.UserDir, PID: st.DolphinPID,
		},
		Resolution: resolution,
		Frames:     frames,
		Inputs:     inputs,
		Oracle:     oracle,
		OCR:        recipe,
	}
}

// --- verification (the replay side's integrity check) -----------------------

// VerifyFixture reads <dir>/manifest.json and checks that every frame file
// exists and hashes to the manifest's claim. It is the replay gate: a
// scenario that references a fixture by id + manifest hash must refuse to run
// (NOT RUN) when this check fails or the dir is absent.
func VerifyFixture(dir string) (FixtureManifest, error) {
	manPath := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(manPath)
	if err != nil {
		return FixtureManifest{}, fmt.Errorf("fixture %s: %w", dir, err)
	}
	var m FixtureManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return FixtureManifest{}, fmt.Errorf("fixture %s: parse manifest: %w", dir, err)
	}
	if m.Schema != FixtureSchema {
		return FixtureManifest{}, fmt.Errorf("fixture %s: unsupported manifest schema %d (want %d)", dir, m.Schema, FixtureSchema)
	}
	if len(m.Frames) == 0 {
		return FixtureManifest{}, fmt.Errorf("fixture %s: manifest names no frames", dir)
	}
	for _, fr := range m.Frames {
		sum, err := sha256File(filepath.Join(dir, "frames", fr.File))
		if err != nil {
			return FixtureManifest{}, fmt.Errorf("fixture %s: frame %s: %w", dir, fr.File, err)
		}
		if sum != fr.SHA256 {
			return FixtureManifest{}, fmt.Errorf("fixture %s: frame %s hash mismatch (want %s, got %s) — the fixture is not the recording it claims; re-record it", dir, fr.File, fr.SHA256, sum)
		}
	}
	return m, nil
}

// ManifestSHA256 is the SHA-256 of one fixture's manifest.json — the value a
// committed scenario references so a replayed fixture is provably the
// recording the scenario was authored against.
func ManifestSHA256(dir string) (string, error) {
	return sha256File(filepath.Join(dir, "manifest.json"))
}

// --- small helpers ------------------------------------------------------------

// ocrSubtitleScale enlarges the subtitle crop before Tesseract sees it
// (480p letterbox text is ~14px tall at native size — far below Tesseract's
// useful input). The factor is recorded in the fixture's OCR recipe.
const ocrSubtitleScale = 4

// ocrFoldKey is the retrieval fold the profile deduplicates lines under
// (punctuation stripped, OCR glyph confusions folded — the same fold the
// matcher uses, internal/matcher).
func ocrFoldKey(s string) string {
	return textnorm.Normalize(s, textnorm.Options{StripPunctuation: true, OCRFold: true})
}

// injectWire renders one Inject the way the drive's action table names its
// own inputs (the pipe.go wire tokens), so the fixture's input log reads the
// same as the drive's transition log.
func injectWire(b Buttons, a Axes) string {
	var parts []string
	if b.A {
		parts = append(parts, "A")
	}
	if b.B {
		parts = append(parts, "B")
	}
	if b.X {
		parts = append(parts, "X")
	}
	if b.Y {
		parts = append(parts, "Y")
	}
	if b.Z {
		parts = append(parts, "Z")
	}
	if b.Start {
		parts = append(parts, "START")
	}
	if b.L {
		parts = append(parts, "L")
	}
	if b.R {
		parts = append(parts, "R")
	}
	if b.Up {
		parts = append(parts, "D_UP")
	}
	if b.Down {
		parts = append(parts, "D_DOWN")
	}
	if b.Left {
		parts = append(parts, "D_LEFT")
	}
	if b.Right {
		parts = append(parts, "D_RIGHT")
	}
	if a.MainX != 0 || a.MainY != 0 {
		parts = append(parts, fmt.Sprintf("STICK(x=%.1f y=%.1f)", a.MainX, a.MainY))
	}
	return strings.Join(parts, "+")
}

// enlarge returns img scaled up by s×, nearest-neighbour (the fixture's OCR
// recipe: a 480p letterbox line is ~14px tall at native size, far below
// Tesseract's useful input; the nearest filter keeps the white-on-black
// glyph edges hard, which is what the engine segments on). s <= 1 returns a
// copy.
func enlarge(img image.Image, s int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if s <= 1 {
		out := image.NewNRGBA(b)
		draw.Draw(out, b, img, b.Min, draw.Over)
		return out
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w*s, h*s))
	for y := 0; y < h*s; y++ {
		for x := 0; x < w*s; x++ {
			dst.Set(x, y, img.At(x/s+b.Min.X, y/s+b.Min.Y))
		}
	}
	return dst
}

func readPNGFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("fixture write %s: %w", path, err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("fixture write %s: %w", path, err)
	}
	return nil
}

func sha256File(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// SortFrames is a test/report helper: the frame files in manifest order.
func SortFrames(m FixtureManifest) []string {
	out := make([]string, 0, len(m.Frames))
	for _, fr := range m.Frames {
		out = append(out, fr.File)
	}
	sort.Strings(out)
	return out
}
