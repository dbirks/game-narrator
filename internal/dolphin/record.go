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
	"encoding/binary"
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
//
// Versioned (docs/spec discipline: a schema change keeps old fixtures
// readable): schema 1 is the original drive-cadence manifest (frames are the
// drive's own iteration screenshots); schema 2 adds the Capture block — the
// parallel adaptive capture plan and its measured budget consumption, so a
// dense recording (≥10 Hz during dialogue/subtitle scenes) states its rate
// in the manifest (ADR-0054 amendment 2; ADR-0058 amendment 1). VerifyFixture
// accepts both: a schema-1 fixture reads with a zero Capture block.
const FixtureSchema = 2

// schemaV1 is the original manifest schema, still verified (old fixtures are
// never re-recorded just to bump a number).
const schemaV1 = 1

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

// FixtureState is one savestate checkpoint the recording saved (ADR-0054
// amendment 1): the file under the fixture root's states/ dir (gitignored,
// AGENTS.md rule 5 — the state embeds the game's RAM, so it is referenced by
// path + SHA-256 and never committed) and the screen it was taken at. The
// manifest reference is the hash: a replay or a --save_state boot that wants
// the checkpoint verifies the file against the manifest's claim before it
// trusts it.
type FixtureState struct {
	Label  string `json:"label"` // the checkpoint's name ("legend_start", "first_dialogue")
	Slot   int    `json:"slot"`  // the savestate slot it was saved from
	File   string `json:"file"`  // relative to the fixture root ("states/...")
	SHA256 string `json:"sha256"`
	Screen string `json:"screen,omitempty"` // the screen measured when it was saved
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
	Capture         *FixtureCapture  `json:"capture,omitempty"`
	Frames          []FixtureFrame   `json:"frames"`
	Inputs          []FixtureInput   `json:"inputs"`
	Oracle          FixtureOracle    `json:"oracle"`
	OCR             FixtureOCRRecipe `json:"ocr"`
	// OCRBox is the in-game dialogue box's OCR recipe (ADR-0054 amendment
	// 1): present when the recording OCR'd dialogue-box frames (the box-text
	// crop, where OCR names the narration's bottom-bar crop). Zero = the
	// legacy narration-only recording.
	OCRBox *FixtureOCRRecipe `json:"ocr_box,omitempty"`
	// States are the savestate checkpoints the recording saved along the way
	// (ADR-0054 amendment 1): the drive's restart points, referenced by hash
	// so a re-boot from a checkpoint verifies the file before it trusts it.
	States []FixtureState `json:"states,omitempty"`
}

// FixtureCapture records the parallel adaptive capture plan (schema 2) and
// what the run consumed of its budget: the rate the manifest claims, so the
// bench and the ADR numbers state the sampling the frames actually have.
type FixtureCapture struct {
	Enabled         bool    `json:"enabled"`
	HighHz          float64 `json:"high_hz"`       // the dialogue/subtitle-scene rate
	LowHz           float64 `json:"low_hz"`        // the everywhere-else rate
	DurationSecs    float64 `json:"duration_secs"` // measured capture window
	MaxDurationSecs float64 `json:"max_duration_secs,omitempty"`
	MaxBytes        int64   `json:"max_bytes,omitempty"`
	Bytes           int64   `json:"bytes"` // measured encoded frame bytes
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
// enlargement and the normalised subtitle crop the engine saw. When Adaptive
// is set, Region is the BASE region of a per-frame adaptive crop: the engine
// saw the base region with its top replaced by the frame's measured text-ink
// top (boxTextCropAdaptive) — recorded so the recipe is reproducible on the
// fixture's pixels. Replay never re-crops: the profile's region is the base
// region and the truth is this recipe's record-time reading.
type FixtureOCRRecipe struct {
	Engine   string              `json:"engine"`
	PSM      int                 `json:"psm"`
	Scale    int                 `json:"scale"`
	Region   [4]float64          `json:"region"`
	Adaptive *FixtureOCRAdaptive `json:"adaptive,omitempty"`
}

// FixtureOCRAdaptive records the adaptive-crop parameters (the base region's
// top is replaced, per frame, by the first row whose glyph-edge ink reaches
// InkFloor, minus Margin; no strong row = the base region). See
// boxTextCropAdaptive for the measured values.
type FixtureOCRAdaptive struct {
	InkFloor float64 `json:"ink_floor"`
	Margin   float64 `json:"margin"`
	ScanY0   float64 `json:"scan_y0"`
	ScanY1   float64 `json:"scan_y1"`
}

// FixtureSummary is Finalize's hand-back: where the fixture lives and its
// manifest's SHA-256 — the hash a committed scenario references.
type FixtureSummary struct {
	Dir            string
	ID             string
	ManifestPath   string
	ManifestSHA256 string
	Frames         int
	Lines          int             // distinct narration lines in profile.json
	Capture        *FixtureCapture // the manifest's capture block (nil = legacy drive-cadence recording)
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

	// Parallel adaptive capture (schema 2). The capture loop records frames
	// on its own clock (independent of the drive's iteration cadence) at the
	// plan's high rate while the last recorded frame measures a dialogue/
	// subtitle screen and at the low rate elsewhere, until the plan's
	// duration/byte budget is spent. The drive's own Screenshot calls are
	// recorded exactly as before; the two streams interleave in capture
	// order into one frame sequence (names and input markers stay
	// consistent because both append under r.mu).
	plan          CapturePlan
	captureCancel context.CancelFunc
	captureDone   chan struct{}
	captureStart  time.Time
	lastFrameTS   time.Time // the last published frame (either stream)
	captureBytes  int64     // encoded frame bytes published since StartCapture
	captureOn     bool

	// Shared frame bookkeeping of both streams: a frame number is reserved
	// at capture time (the drive synchronously, the loop at the grab), the
	// frame is completed when its file is written (drive: inline; loop: by
	// a worker), and publishLocked appends completed frames in capture
	// order — so r.frames always reads in frame-number (capture) order
	// whichever stream finishes first.
	nextFrameNo int
	pending     map[int]capturePending
	publishNext int
	lastScreen  DriveScreen // the last published frame's class (the loop's clock)
	captureErr  error       // the first frame-write failure (Finalize surfaces it)

	// Savestate checkpoints (ADR-0054 amendment 1): the states the run saved
	// along the way (legend start, first dialogue) land under the fixture
	// root's states/ dir and are referenced by hash in the manifest. AddState
	// is the only writer; Finalize folds the entries into the manifest.
	states []FixtureState
	// lineIDPrefix names the profile line ids ("ww-legend" by default, the
	// opening-legend fixtures): a fixture whose lines are not the legend's
	// (the first in-game dialogue) must not reuse the legend's id space, or
	// the scenarios' message names would claim the wrong line.
	lineIDPrefix string
}

// capturePending is a reserved frame awaiting (or completing into) its
// append into r.frames.
type capturePending struct {
	fr    FixtureFrame
	err   error
	ready bool
	skip  bool // a grabbed frame dropped at cancel: no file, no manifest entry
	bytes int64
}

// CapturePlan configures the recorder's parallel adaptive capture. Zero
// fields take the defaults (10 Hz high / 1 Hz low, unbounded budget). The
// high rate is the task's ≥10 Hz dialogue/subtitle requirement; the low rate
// bounds the fixture's size on the long non-dialogue spans.
type CapturePlan struct {
	HighHz      float64       // frames per second while the last recorded frame is dialogue/subtitle
	LowHz       float64       // frames per second everywhere else
	MaxDuration time.Duration // stop capturing after this (0 = unbounded)
	MaxBytes    int64         // stop capturing once this many encoded bytes were written (0 = unbounded)
}

// captureMinGap is the minimum spacing between any two recorded frames
// (either stream): closer captures would duplicate the emulator's state for
// the bytes' worth (a grab takes ~80 ms; the screen cannot out-run that).
const captureMinGap = 100 * time.Millisecond

// captureFailTolerance is how many consecutive grab failures the capture
// loop survives before it stops (a lost window is honest to stop on; the
// fixture then ends shorter and the manifest says so). The drive's own
// retries are unaffected — this is the parallel stream only.
const captureFailTolerance = 5

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
	return &FixtureRecorder{inner: b, id: id, root: dir, pending: map[int]capturePending{}, lineIDPrefix: "ww-legend"}, nil
}

// Dir is the fixture's directory (<root>/<id>).
func (r *FixtureRecorder) Dir() string { return r.root }

// SetLineIDPrefix sets the profile line ids' prefix (default "ww-legend",
// the opening-legend fixtures). A fixture whose lines are the first in-game
// dialogue (not the legend's narration) uses its own prefix so the id space
// never blends the two line sets (the scenarios name lines by id).
func (r *FixtureRecorder) SetLineIDPrefix(prefix string) {
	if prefix == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lineIDPrefix = prefix
}

// AddState records one savestate checkpoint the run saved (ADR-0054
// amendment 1): file is the state file's path (under the fixture root's
// states/ dir — the caller copied it there after SaveState), label its
// checkpoint name ("legend_start", "first_dialogue"), slot the savestate
// slot it was saved from. The file's SHA-256 is computed now (the manifest
// reference is the hash) and the entry lands in the manifest's states block
// at Finalize. screen is the screen measured at the save (advisory; the
// fixture's own frames are the record).
func (r *FixtureRecorder) AddState(label string, slot int, file, screen string) error {
	abs := file
	if !filepath.IsAbs(file) {
		abs = filepath.Join(filepath.Dir(r.root), file) // the fixture root (states/ is its child, not the fixture's)
	}
	sum, err := sha256File(abs)
	if err != nil {
		return fmt.Errorf("dolphin: fixture state %s: %w", file, err)
	}
	rel, err := filepath.Rel(filepath.Dir(r.root), abs)
	if err != nil {
		return fmt.Errorf("dolphin: fixture state %s: %w", file, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, FixtureState{Label: label, Slot: slot, File: filepath.ToSlash(rel), SHA256: sum, Screen: screen})
	return nil
}

// SetCapturePlan arms the parallel adaptive capture (call before
// StartCapture). It is a no-op after the capture has started: the plan is
// what the manifest records, so it is fixed for the run. The zero plan is
// the legacy drive-cadence recording (no parallel stream).
func (r *FixtureRecorder) SetCapturePlan(p CapturePlan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.captureOn {
		return
	}
	r.plan = p
}

// StartCapture begins the parallel adaptive capture loop on its own clock:
// it records frames at the plan's high rate while the last recorded frame
// measures a dialogue/subtitle screen and at the low rate elsewhere, until
// the budget is spent or StopCapture. The drive's own Screenshot recording
// is unchanged; Finalize always joins a running capture, and a fixture
// recorded without a capture is schema-1-shaped (zero Capture block) exactly
// as before.
func (r *FixtureRecorder) StartCapture(ctx context.Context) error {
	r.mu.Lock()
	if r.captureOn {
		r.mu.Unlock()
		return fmt.Errorf("dolphin: fixture %s capture already started", r.id)
	}
	// The zero plan is the legacy drive-cadence recording: refuse a
	// default-on capture (checked before normalize, which would fill the
	// defaults and make the plan non-zero).
	if !r.plan.enabled() {
		r.mu.Unlock()
		return fmt.Errorf("dolphin: fixture %s has no capture plan (SetCapturePlan first)", r.id)
	}
	r.plan = r.plan.normalize()
	r.captureOn = true
	r.captureErr = nil
	r.captureStart = time.Now()
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.captureCancel = cancel
	r.captureDone = done
	r.mu.Unlock()
	go r.captureLoop(cctx, done)
	return nil
}

// StopCapture joins the capture loop. It is idempotent and safe before
// StartCapture. Finalize calls it, so a capture never writes a frame after
// the manifest is assembled.
func (r *FixtureRecorder) StopCapture() {
	r.mu.Lock()
	if !r.captureOn {
		r.mu.Unlock()
		return
	}
	r.captureOn = false
	cancel := r.captureCancel
	done := r.captureDone
	r.captureCancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// captureWorkers is the parallel worker count of the capture loop (encode +
// write + classify). The grab itself runs on the loop's own goroutine at
// the plan's rate: the workers overlap the CPU-bound processing so the
// effective high rate is not gated on the per-frame processing cost on a
// loaded host (measured on the dev host: ~35 ms of CPU per frame plus the
// X11 grab's spawn/round-trip, ~100-500 ms under contention with the
// drive's and watch's own grabs).
const captureWorkers = 4

// captureInflight is the in-flight channel depth: how many grabbed frames
// may wait for a worker. Bounded (bounded queues / explicit backpressure):
// when the workers lag, the grabber blocks — the rate drops, never the
// order.
const captureInflight = 16

// captureJob is one frame the loop grabbed: its reserved frame number (the
// name is fixed at grab time, so the file, the manifest entry and the truth
// row stay paired no matter how the workers finish) and the pixels captured
// at ts.
type captureJob struct {
	no  int
	ts  time.Time
	img image.Image
}

// captureLoop is the parallel stream. It grabs on the plan's clock (this
// goroutine — the grab is the one step that must keep the clock), hands
// each frame to the captureWorkers parallel workers (encode + write +
// classify — the CPU-bound half, off the clock), and the frame records are
// appended in capture order by the shared reserve/complete/publish
// bookkeeping (captureReserve/captureComplete), which the drive's
// Screenshot shares. It stops on context cancel, the budget, or
// captureFailTolerance consecutive grab failures (a lost display is not
// something a capture loop should spin on).
func (r *FixtureRecorder) captureLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	jobs := make(chan captureJob, captureInflight)
	var wg sync.WaitGroup
	for i := 0; i < captureWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				r.captureWorker(j)
			}
		}()
	}
	// The grabber leaves the loop and then close(jobs) makes the workers
	// drain the channel (every grabbed frame is a reserved frame number and
	// must be completed); wg.Wait returns the loop to StopCapture only when
	// nothing is in flight.
	defer func() { close(jobs); wg.Wait() }()

	interval := r.intervalFor(r.lastScreenLocked())
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		active, fresh := r.captureGate()
		if !active {
			// The budget closed (or the capture was stopped): the recording
			// is done with the frames it has.
			return
		}
		if !fresh {
			interval = 20 * time.Millisecond // someone else just recorded; re-check soon
			continue
		}
		img, err := r.inner.Screenshot(ctx)
		if err != nil {
			fails++
			if fails > captureFailTolerance {
				r.captureFailStop() // stop grabbing; the in-flight frames still complete
				return
			}
			interval = 500 * time.Millisecond
			continue
		}
		fails = 0
		no, active, fresh := r.captureReserve()
		if !active {
			// The capture stopped (budget or cancel) between the gate and
			// the reserve: the grabbed frame is not part of the recording —
			// let it go (no number was reserved, so nothing dangles).
			return
		}
		if !fresh {
			// Someone else captured in the gap since the gate: drop this
			// grab, retry soon.
			interval = 20 * time.Millisecond
			continue
		}
		select {
		case <-ctx.Done():
			r.captureSkip(no) // the grab happened but the recording ended: drop it, keep the sequence whole
			return
		case jobs <- captureJob{no: no, ts: time.Now(), img: img}:
		}
		interval = r.intervalFor(r.lastScreenLocked())
	}
}

// lastScreenLocked is the screen class the interval is based on (the last
// frame appended by either stream). Callers hold no lock; it takes r.mu.
func (r *FixtureRecorder) lastScreenLocked() DriveScreen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastScreen
}

// captureReserve is the loop's gate and reservation in one lock: it reports
// whether the capture is still active (budget and cancel), whether a frame
// may be captured now (the captureMinGap since the last reservation of
// either stream — the gap is enforced on reservations, the capture instant,
// so in-flight frames cannot burst past it while a previous frame is still
// in a worker), and, when both hold, hands out the reserved frame number.
// (no, active, fresh): no>0 means the number is reserved and the frame
// must be completed (or skipped at cancel); active=false means the capture
// has stopped for good; fresh=false means retry soon.
func (r *FixtureRecorder) captureReserve() (int, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.captureOn {
		return 0, false, false
	}
	if r.plan.MaxDuration > 0 && time.Since(r.captureStart) >= r.plan.MaxDuration {
		r.captureOn = false // the budget is the stop; the manifest records the window
		return 0, false, false
	}
	if r.plan.MaxBytes > 0 && r.captureBytes >= r.plan.MaxBytes {
		r.captureOn = false
		return 0, false, false
	}
	fresh := r.lastFrameTS.IsZero() || time.Since(r.lastFrameTS) >= captureMinGap
	if !fresh {
		return 0, true, false
	}
	no := r.nextFrameNo
	r.nextFrameNo++
	r.lastFrameTS = time.Now()
	return no, true, true
}

// reserveFrameNoLocked reserves the next frame number unconditionally —
// the drive's synchronous Screenshot path (which records whether or not a
// capture plan is on). The drive always records (it is the fixture's
// contract), so it stamps the capture gap unconditionally. Callers hold
// r.mu.
func (r *FixtureRecorder) reserveFrameNoLocked() int {
	no := r.nextFrameNo
	r.nextFrameNo++
	r.lastFrameTS = time.Now()
	return no
}

// captureWorker is the CPU-bound half of one captured frame: PNG encode,
// write, the drive's own screen measurement, and the manifest entry — then
// it completes the frame so the shared bookkeeping can publish it in
// capture order.
func (r *FixtureRecorder) captureWorker(j captureJob) {
	buf := new(bytes.Buffer)
	if err := png.Encode(buf, j.img); err != nil {
		r.captureComplete(j.no, FixtureFrame{}, fmt.Errorf("dolphin: fixture encode frame: %w", err), 0)
		return
	}
	sig := ScreenSigOf(j.img)
	screen, side := DriveScreenOf(sig)
	sum := sha256.Sum256(buf.Bytes())
	name := fmt.Sprintf("frame-%05d.png", j.no+1)
	path := filepath.Join(r.root, "frames", name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		r.captureComplete(j.no, FixtureFrame{}, fmt.Errorf("dolphin: fixture write frame %d: %w", j.no+1, err), 0)
		return
	}
	fr := FixtureFrame{
		File:   name,
		TsNS:   j.ts.UnixNano(),
		SHA256: hex.EncodeToString(sum[:]),
		Screen: string(screen),
		Side:   string(side),
		Sig:    sig.Key(),
	}
	r.captureComplete(j.no, fr, nil, int64(buf.Len()))
}

// captureComplete records one frame's outcome (the manifest entry and/or an
// error, plus the encoded byte count for the budget) and publishes every
// contiguously completed frame into r.frames — the shared append path of
// both streams, so the frame list always stays in capture order (frame
// number order), whatever order the workers finish in.
func (r *FixtureRecorder) captureComplete(no int, fr FixtureFrame, err error, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil && r.captureErr == nil {
		r.captureErr = err // Finalize surfaces it (a fixture missing a frame is a lie)
	}
	r.pending[no] = capturePending{fr: fr, err: err, ready: true, bytes: n}
	r.publishLocked()
}

// publishLocked appends every contiguously completed frame to r.frames and
// drops its pending slot. Callers hold r.mu. A gap (a lower number still
// in a worker) parks the higher frames until the gap completes — order is
// the invariant, completion is not.
func (r *FixtureRecorder) publishLocked() {
	for {
		pp, ok := r.pending[r.publishNext]
		if !ok || !pp.ready {
			return
		}
		if !pp.skip && pp.err == nil {
			r.frames = append(r.frames, pp.fr)
			r.lastScreen = DriveScreen(pp.fr.Screen)
			r.captureBytes += pp.bytes
		}
		delete(r.pending, r.publishNext)
		r.publishNext++
	}
}

// captureSkip drops a frame that was reserved (grabbed) but must not be
// recorded: the capture stopped (cancel) between the reserve and the job
// hand-off. The number is completed as a skip so the publish sequence
// advances past it — the frame's file is never written and the manifest
// never lists it (the numbering may then have a gap: the manifest is the
// source of truth, and a cancelled grab is outside the recording).
func (r *FixtureRecorder) captureSkip(no int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending[no] = capturePending{skip: true, ready: true}
	r.publishLocked()
}

// captureGate takes the loop's decision under one lock: whether the capture
// is still on and within budget (active) and whether a frame may be grabbed
// now (fresh — nothing was captured within captureMinGap by either
// stream).
func (r *FixtureRecorder) captureGate() (bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.captureOn {
		return false, false
	}
	if r.plan.MaxDuration > 0 && time.Since(r.captureStart) >= r.plan.MaxDuration {
		r.captureOn = false // the budget is the stop; the manifest records the window
		return false, false
	}
	if r.plan.MaxBytes > 0 && r.captureBytes >= r.plan.MaxBytes {
		r.captureOn = false
		return false, false
	}
	fresh := r.lastFrameTS.IsZero() || time.Since(r.lastFrameTS) >= captureMinGap
	return true, fresh
}

func (r *FixtureRecorder) captureFailStop() {
	r.mu.Lock()
	r.captureOn = false
	cancel := r.captureCancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// intervalFor is the plan's clock for a screen class: the high rate on the
// dialogue/subtitle screens (the scenes where a 2 s grid would sample a line
// in one frame), the low rate everywhere else.
func (r *FixtureRecorder) intervalFor(screen DriveScreen) time.Duration {
	hz := r.plan.LowHz
	switch screen {
	case DriveScreenStorySubtitle, DriveScreenDialogueBox:
		hz = r.plan.HighHz
	}
	if hz <= 0 {
		hz = 1
	}
	return time.Duration(float64(time.Second) / hz)
}

// recordFrame records one frame (file, capture timestamp, the drive's own
// measured screen class) and is the single write path of both streams. It
// returns the measured screen so the capture loop can pick its next
// interval. The whole record (name reservation, encode, write, append) is
// one r.mu critical section: the frame NUMBER and the frame's bytes/hash
// must pair atomically, or two streams interleaving between the number
// reservation and the append could reserve the same name and one file's
// bytes would carry another frame's hash (the verification gate would fail
// on a healthy recording).
// recordFrame records one frame on the drive's synchronous path: the
// number is reserved immediately (the pixels are already in hand — reserve
// order is capture order), the frame is encoded, written, classified and
// completed through the shared bookkeeping, which appends it in capture
// order (a frame can briefly wait behind a still-in-flight loop frame;
// the manifest and the truth rows are built from the published order).
func (r *FixtureRecorder) recordFrame(img image.Image, ts time.Time) (DriveScreen, error) {
	r.mu.Lock()
	no := r.reserveFrameNoLocked()
	r.mu.Unlock()
	buf := new(bytes.Buffer)
	if err := png.Encode(buf, img); err != nil {
		r.captureComplete(no, FixtureFrame{}, fmt.Errorf("dolphin: fixture encode frame: %w", err), 0)
		return DriveScreenUnknown, fmt.Errorf("dolphin: fixture encode frame: %w", err)
	}
	sig := ScreenSigOf(img)
	screen, side := DriveScreenOf(sig)
	sum := sha256.Sum256(buf.Bytes())
	name := fmt.Sprintf("frame-%05d.png", no+1)
	path := filepath.Join(r.root, "frames", name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		r.captureComplete(no, FixtureFrame{}, fmt.Errorf("dolphin: fixture write frame %d: %w", no+1, err), 0)
		return DriveScreenUnknown, fmt.Errorf("dolphin: fixture write frame %d: %w", no+1, err)
	}
	fr := FixtureFrame{
		File:   name,
		TsNS:   ts.UnixNano(),
		SHA256: hex.EncodeToString(sum[:]),
		Screen: string(screen),
		Side:   string(side),
		Sig:    sig.Key(),
	}
	r.captureComplete(no, fr, nil, int64(buf.Len()))
	return screen, nil
}

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
// the run: a fixture missing a frame is a lie about the recording. It shares
// the capture loop's frame sequence (one mutex, capture order) and updates
// the loop's min-gap clock, so the two streams never record the same instant
// twice.
func (r *FixtureRecorder) Screenshot(ctx context.Context) (image.Image, error) {
	img, err := r.inner.Screenshot(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := r.recordFrame(img, time.Now()); err != nil {
		return nil, err
	}
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
	// Join any running parallel capture first: the manifest is assembled
	// from the frame list as of this point, so no frame may land after it.
	r.StopCapture()
	r.mu.Lock()
	frames := append([]FixtureFrame(nil), r.frames...)
	inputs := append([]FixtureInput(nil), r.inputs...)
	st := r.st
	launched := r.launched
	plan := r.plan
	started := !r.captureStart.IsZero()
	start := r.captureStart
	bytes := r.captureBytes
	r.mu.Unlock()
	captureOn := plan.enabled() && started

	r.mu.Lock()
	capErr := r.captureErr
	r.mu.Unlock()
	if capErr != nil {
		// A frame's file could not be written: the fixture would be missing
		// frames its manifest truth implies — that is a lie, so the finalize
		// fails and the caller must not ship this fixture.
		return FixtureSummary{}, capErr
	}

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

	// The truth text, per frame (ocrFrames: the shared record-time OCR +
	// canonicalisation step, also the re-OCR path for a recipe revision).
	texts, subRuns, boxRuns, subFrames, boxFrames, err := r.ocrFrames(ctx, frames)
	if err != nil {
		return FixtureSummary{}, err
	}
	resolution := FixtureSize{W: st.Region.W, H: st.Region.H}
	recipe := FixtureOCRRecipe{Engine: "tesseract", PSM: ocr.DefaultPSM, Scale: ocrSubtitleScale, Region: StorySubCrop}

	var capture *FixtureCapture
	if captureOn {
		norm := plan.normalize()
		capture = &FixtureCapture{
			Enabled:         true,
			HighHz:          norm.HighHz,
			LowHz:           norm.LowHz,
			DurationSecs:    time.Since(start).Seconds(),
			MaxDurationSecs: plan.MaxDuration.Seconds(),
			MaxBytes:        plan.MaxBytes,
			Bytes:           bytes,
		}
	}

	var ocrBox *FixtureOCRRecipe
	if boxFrames > 0 {
		// The box recipe records the ADAPTIVE crop: the base boxTextCrop with
		// the per-frame text-ink top (boxTextCropAdaptive). The parameters are
		// recorded so the recipe is reproducible on the fixture's pixels.
		ocrBox = &FixtureOCRRecipe{
			Engine: "tesseract", PSM: ocr.DefaultPSM, Scale: ocrSubtitleScale,
			Region:   boxTextCrop,
			Adaptive: &FixtureOCRAdaptive{InkFloor: boxInkFloor, Margin: boxCropMargin, ScanY0: boxInkScanY0, ScanY1: boxInkScanY1},
		}
	}

	r.mu.Lock()
	states := append([]FixtureState(nil), r.states...)
	prefix := r.lineIDPrefix
	r.mu.Unlock()

	manifest := r.buildManifest(frames, inputs, st, drv, imageSHA, resolution, recipe, capture, ocrBox, states)
	summary := FixtureSummary{Dir: r.root, ID: r.id, Frames: len(frames), Capture: capture}

	if err := writeJSON(filepath.Join(r.root, "manifest.json"), manifest); err != nil {
		return summary, err
	}
	if err := r.writeTruth(frames, texts, subRuns, boxRuns); err != nil {
		return summary, err
	}
	lines, err := r.writeProfile(subRuns, boxRuns, subFrames, boxFrames, resolution, prefix)
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

// ocrCropCached is ocrFrameCached generalised to a crop window: the crop's
// pixel hash is computed first and, when the cache already holds an answer
// for exactly these pixels, the engine is not re-run. cache is Finalize's
// per-run map (shared across both crops — the pixel hash is the key, so a
// subtitle crop and a box crop with identical pixels share the reading).
// boxInk* are the measured parameters of the adaptive box-text crop
// (boxTextCropAdaptive), read from the retained first-dialogue fixture's
// frames (2026-10-18): the dialogue box's first text line sits where the box
// has ONE line (measured y 0.74-0.84) or, for a multi-line box, at its first
// line (measured y 0.64-0.70). A fixed 0.62 top (boxTextCrop, kept for
// presence and the profile region) therefore drags the box's top border and
// empty interior into every single-line box's reading; on the 2026-10-18
// first-dialogue recording that garbage read conf 0.34-0.67 and never
// settled, while the same box read conf 0.81-0.85 with the text-anchored top
// and the multi-line box kept its three lines at conf 0.89-0.92.
const (
	// boxInkScanY0/Y1 bound the row scan for the text-ink top (above the box
	// bottom, below the scene).
	boxInkScanY0 = 0.60
	boxInkScanY1 = 0.90
	// boxInkFloor is the row edge-ink fraction (fraction of the row's
	// horizontal |dluma|>=boxEdgeDelta pixels) that counts as text ink. A
	// text line measures 0.11-0.43; the box's border and its flat interior
	// measure <= 0.06; the floor sits between the two.
	boxInkFloor = 0.08
	// boxCropMargin is how far above the first ink row the adaptive top sits
	// (the glyph's ascenders and the row-resolution of the ink scan).
	boxCropMargin = 0.02
	// boxCropTopMin keeps the adaptive top inside the box: a spurious ink row
	// high in the scene must not drag the crop up into the scene art.
	boxCropTopMin = 0.55
	// boxEdgeDelta is the luma step (0..255) that counts as a glyph-stroke
	// edge — the same delta the screen signature uses (sigEdgeDelta).
	boxEdgeDelta = 30
)

// adaptiveBoxTextCrop returns the record-time reading crop for one
// dialogue-box frame: the base boxTextCrop with its top replaced by the
// frame's measured text-ink top minus boxCropMargin. The bottom, left and
// right edges are the base region's (the text never runs below the measured
// 0.82 and the base x band is the reading band). When no row reaches
// boxInkFloor (the box is empty or mid-fade) the base region is returned:
// the reading is "" anyway (below OCRConfFloor), so the fallback cannot
// invent a line. The crop is a deterministic function of the frame's
// pixels, so re-running it on the fixture's frames reproduces the truth.
func adaptiveBoxTextCrop(img image.Image) [4]float64 {
	crop := boxTextCrop
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 4 || h < 4 {
		return crop
	}
	x0, x1 := int(0.05*float64(w)), int(0.85*float64(w))
	y0, y1 := int(boxInkScanY0*float64(h)), int(boxInkScanY1*float64(h))
	lum := func(x, y int) uint32 {
		r, g, bl, _ := img.At(x, y).RGBA()
		return (r + g + bl) / (3 * 256)
	}
	for y := y0; y < y1; y++ {
		var edges int
		for x := x0 + 1; x < x1; x++ {
			d := int(lum(x, y)) - int(lum(x-1, y))
			if d < 0 {
				d = -d
			}
			if d >= boxEdgeDelta {
				edges++
			}
		}
		if float64(edges)/float64(x1-x0-1) < boxInkFloor {
			continue
		}
		top := float64(y)/float64(h) - boxCropMargin
		if top < boxCropTopMin {
			top = boxCropTopMin
		}
		crop[1] = top
		break
	}
	return crop
}

// ocrFrames is the record-time OCR + canonicalisation step, shared by the
// live Finalize and the recipe-revision re-OCR (re-finalise): two text-
// bearing screen classes are OCR'd (ADR-0054 amendment 1) — the opening
// legend's narration subtitles (story_subtitle, the bottom-bar crop) and the
// in-game dialogue box (dialogue_box, the adaptive box-text crop).
// Presence/absence of the box in the region is the point of the other frames,
// so they carry no text.
//
// The crop dedupe (dense recordings, schema 2): a high-rate capture holds a
// static subtitle for hundreds of near-identical frames; the OCR answer is a
// function of the crop's pixels, so a crop whose pixels are identical to a
// crop already read reuses that reading instead of re-running the engine
// (identical input, identical answer — the recipe is unchanged and the truth
// is still the engine's record-time reading, only not re-run on pixels the
// engine has already seen). The cache is the exact pixel hash, so any pixel
// difference re-runs the engine.
func (r *FixtureRecorder) ocrFrames(ctx context.Context, frames []FixtureFrame) (texts []string, sub, box lineRuns, subFrames, boxFrames int, err error) {
	texts = make([]string, len(frames))
	cropCache := map[string]string{} // crop pixel hash -> the engine's reading
	for i, fr := range frames {
		switch fr.Screen {
		case string(DriveScreenStorySubtitle):
			subFrames++
			text, err := r.ocrCropCached(ctx, fr.File, StorySubCrop, cropCache)
			if err != nil {
				return nil, sub, box, subFrames, boxFrames, fmt.Errorf("dolphin: fixture OCR frame %s: %w", fr.File, err)
			}
			texts[i] = text
		case string(DriveScreenDialogueBox):
			boxFrames++
			text, err := r.ocrBoxFrame(ctx, fr.File, cropCache)
			if err != nil {
				return nil, sub, box, subFrames, boxFrames, fmt.Errorf("dolphin: fixture OCR frame %s: %w", fr.File, err)
			}
			texts[i] = text
		}
	}
	// Canonical lines per reveal run (canonicalizeRuns): the profile's message
	// set is the settled lines, not the readings. The first-seen scheme
	// (amendment 3) was right that Tesseract's record-time reading is the
	// truth, wrong about WHICH reading names the line: it pinned the
	// typewriter's first prefix as a line and made the profile teach the
	// matcher its own prefixes (the 2026-10-18 17:14
	// windwaker-first-dialogue re-record: 30 profile lines for 4 spoken).
	sub = canonicalizeRuns(frames, texts, string(DriveScreenStorySubtitle))
	box = canonicalizeRuns(frames, texts, string(DriveScreenDialogueBox))
	return texts, sub, box, subFrames, boxFrames, nil
}

// ocrCropCached OCRs one frame's crop, reusing the reading of an identical
// crop (the crop's exact pixels hash to the cache key — the dense capture
// holds static text for hundreds of frames).
func (r *FixtureRecorder) ocrCropCached(ctx context.Context, file string, crop [4]float64, cache map[string]string) (string, error) {
	img, err := readPNGFile(filepath.Join(r.root, "frames", file))
	if err != nil {
		return "", err
	}
	return r.ocrImageCrop(ctx, img, crop, cache)
}

// ocrBoxFrame OCRs one dialogue-box frame with the ADAPTIVE box-text crop
// (boxTextCropAdaptive): the top is anchored to the frame's measured text
// ink, so a single-line box is not read through the box's empty interior and
// border. The base region stays the profile's and the presence region's
// crop; only the record-time reading crop moves.
func (r *FixtureRecorder) ocrBoxFrame(ctx context.Context, file string, cache map[string]string) (string, error) {
	img, err := readPNGFile(filepath.Join(r.root, "frames", file))
	if err != nil {
		return "", err
	}
	return r.ocrImageCrop(ctx, img, adaptiveBoxTextCrop(img), cache)
}

// ocrImageCrop runs the shared OCR core over one already-decoded crop.
func (r *FixtureRecorder) ocrImageCrop(ctx context.Context, img image.Image, crop [4]float64, cache map[string]string) (string, error) {
	c := vision.CropNormalized(img, vision.RectOf(games.Region{
		RectX: crop[0], RectY: crop[1],
		RectW: crop[2] - crop[0], RectH: crop[3] - crop[1],
	}), vision.CropOptions{})
	if c == nil || c.Bounds().Empty() {
		return "", nil
	}
	sum := cropPixelHash(c)
	if text, ok := cache[sum]; ok {
		return text, nil
	}
	big := enlarge(c, ocrSubtitleScale)
	res, err := ocr.Tesseract{PSM: ocr.DefaultPSM}.Recognize(ctx, big)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(res.Text)
	if text == "" || res.MeanConf < OCRConfFloor {
		text = ""
	}
	cache[sum] = text
	return text, nil
}

// cropPixelHash is the SHA-256 of the crop's raw RGBA pixels in row-major
// order — the exact-input fingerprint the dedupe caches under (any pixel
// difference is a different key; identical pixels reuse the reading).
func cropPixelHash(img image.Image) string {
	h := sha256.New()
	b := img.Bounds()
	var px [16]byte
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			cr, cg, cb, ca := img.At(x, y).RGBA()
			binary.LittleEndian.PutUint32(px[0:4], uint32(cr))
			binary.LittleEndian.PutUint32(px[4:8], uint32(cg))
			binary.LittleEndian.PutUint32(px[8:12], uint32(cb))
			binary.LittleEndian.PutUint32(px[12:16], uint32(ca))
			h.Write(px[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeTruth writes truth.json in the fixture's ground-truth shape: one row
// per frame with the textbox presence in the region and the OCR text. Present
// is true exactly where the drive measured a text-bearing screen — the
// narration's bottom bar (story_subtitle) or the in-game dialogue box
// (dialogue_box, ADR-0054 amendment 1); elsewhere the region is not a textbox
// even if pixels are. The line labels run over the combined line set (the
// narration lines first, then the box lines) so a label always names one
// profile message; every frame of a run carries its RUN's line label (a
// typewriter prefix belongs to the line it is revealing), and Full marks
// exactly the frames whose reading IS the run's canonical line.
func (r *FixtureRecorder) writeTruth(frames []FixtureFrame, texts []string, sub, box lineRuns) error {
	type row struct {
		File    string `json:"file"`
		TsNS    int64  `json:"ts_ns"`
		Present bool   `json:"box_present"`
		Text    string `json:"text"`
		Line    string `json:"line"`
		Full    bool   `json:"text_is_full_line"`
	}
	offset := len(sub.Canonical)
	labelOf := func(fam lineRuns, idx int, off int) string {
		if idx < 0 {
			return ""
		}
		return string(rune('a' + idx + off))
	}
	rows := make([]row, 0, len(frames))
	for i, fr := range frames {
		present := fr.Screen == string(DriveScreenStorySubtitle) || fr.Screen == string(DriveScreenDialogueBox)
		text := ""
		if i < len(texts) {
			text = texts[i]
		}
		ln := ""
		full := false
		switch fr.Screen {
		case string(DriveScreenStorySubtitle):
			ln = labelOf(sub, sub.lineOf[i], 0)
			if text != "" {
				full = ocrFoldKey(text) == sub.runKey[i]
			}
		case string(DriveScreenDialogueBox):
			ln = labelOf(box, box.lineOf[i], offset)
			if text != "" {
				full = ocrFoldKey(text) == box.runKey[i]
			}
		}
		rows = append(rows, row{File: fr.File, TsNS: fr.TsNS, Present: present, Text: text, Line: ln, Full: full})
	}
	return writeJSON(filepath.Join(r.root, "truth.json"), rows)
}

// writeProfile writes profile.json: the distinct narration lines (first) and
// in-game dialogue-box lines (ADR-0054 amendment 1) as messages — ids carry
// no game text ("<prefix>-NN", the prefix set per fixture so the two line
// sets never blend) — so the committed scenario YAML can name them freely —
// consecutive lines linked by next edges within each family, and the
// measured text regions (the box's region first when the fixture carries box
// lines: the harness reads Regions[0] as the region it replays against).
func (r *FixtureRecorder) writeProfile(sub, box lineRuns, subFrames, boxFrames int, resolution FixtureSize, prefix string) (int, error) {
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
	subCanonical, boxCanonical := sub.Canonical, box.Canonical
	total := len(subCanonical) + len(boxCanonical)
	messages := make([]msg, 0, total)
	idOf := func(i int) string { return fmt.Sprintf("%s-%02d", prefix, i+1) }
	for i, c := range subCanonical {
		messages = append(messages, msg{ID: idOf(i), OriginalText: c})
	}
	offset := len(subCanonical)
	for i, c := range boxCanonical {
		messages = append(messages, msg{ID: idOf(offset + i), OriginalText: c})
	}
	edges := make([]edge, 0, max(total-1, 0))
	addEdges := func(n, base int) {
		for i := 1; i < n; i++ {
			edges = append(edges, edge{
				From: idOf(base + i - 1), To: idOf(base + i),
				Type: "next", Confidence: 0.9,
			})
		}
	}
	addEdges(len(subCanonical), 0)
	addEdges(len(boxCanonical), offset)
	// The region order is by text-bearing frame count (the primary text
	// region first): the harness replays against regions[0], so a
	// subtitle-centric fixture (the press-paced legend: 541 bottom-bar frames
	// against 8 stray box frames) must present the bottom bar first, and a
	// box-centric fixture (the first dialogue) the box. A tie keeps the box
	// first (the in-game box is the stronger text region).
	putBoxFirst := boxFrames > 0 && boxFrames >= subFrames
	regions := []map[string]any{}
	boxRegion := func() map[string]any {
		return map[string]any{
			"id": "box", "type": "textbox",
			"x": boxTextCrop[0], "y": boxTextCrop[1],
			"w": boxTextCrop[2] - boxTextCrop[0], "h": boxTextCrop[3] - boxTextCrop[1],
		}
	}
	bottomRegion := func() map[string]any {
		return map[string]any{
			"id": "bottom", "type": "textbox",
			"x": StorySubCrop[0], "y": StorySubCrop[1],
			"w": StorySubCrop[2] - StorySubCrop[0], "h": StorySubCrop[3] - StorySubCrop[1],
		}
	}
	if len(box.Canonical) > 0 {
		if putBoxFirst {
			regions = append(regions, boxRegion())
		}
		if len(sub.Canonical) > 0 {
			regions = append(regions, bottomRegion())
		}
		if !putBoxFirst {
			// The box lines exist but the bottom bar is the primary text
			// region: the box region rides along (a two-region fixture the
			// harness reads against regions[0]).
			regions = append(regions, boxRegion())
		}
	} else if len(sub.Canonical) > 0 {
		regions = append(regions, bottomRegion())
	}
	if len(regions) == 0 {
		regions = append(regions, map[string]any{
			"id": "bottom", "type": "textbox",
			"x": StorySubCrop[0], "y": StorySubCrop[1],
			"w": StorySubCrop[2] - StorySubCrop[0], "h": StorySubCrop[3] - StorySubCrop[1],
		})
	}
	p := map[string]any{
		"id":     r.id,
		"origin": "dolphin",
		"description": "Recorded Wind Waker fixture (docs/spec/11 'Fixture origins'): " +
			"the lines are the real Tesseract readings of the recording's subtitle and dialogue-box " +
			"frames, recorded at capture time; the profile, truth and frames live in this gitignored " +
			"fixture only (AGENTS.md rule 5). No game text appears in any tracked file.",
		"text_kind":  "dialogue",
		"messages":   messages,
		"edges":      edges,
		"regions":    regions,
		"resolution": resolution,
	}
	return total, writeJSON(filepath.Join(r.root, "profile.json"), p)
}

// buildManifest assembles the manifest from the recorded frames, the input
// log, the launch status and the drive's oracle (its transition log is the
// record of which scene/state the drive believed it was in).
func (r *FixtureRecorder) buildManifest(frames []FixtureFrame, inputs []FixtureInput, st Status, drv DriveTrace, imageSHA string, resolution FixtureSize, recipe FixtureOCRRecipe, capture *FixtureCapture, ocrBox *FixtureOCRRecipe, states []FixtureState) FixtureManifest {
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
		Capture:    capture,
		Frames:     frames,
		Inputs:     inputs,
		Oracle:     oracle,
		OCR:        recipe,
		OCRBox:     ocrBox,
		States:     states,
	}
}

// normalize fills the plan's zero fields with the defaults (10 Hz high /
// 1 Hz low, unbounded budget).
func (p CapturePlan) normalize() CapturePlan {
	if p.HighHz <= 0 {
		p.HighHz = 10
	}
	if p.LowHz <= 0 {
		p.LowHz = 1
	}
	return p
}

// enabled reports whether the plan arms a parallel capture at all (the zero
// plan — SetCapturePlan never called — is the legacy drive-cadence
// recording).
func (p CapturePlan) enabled() bool {
	return p.HighHz > 0 || p.LowHz > 0
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
	// Versioned gate: the current schema and the original one (the existing
	// windwaker-newgame fixture and any other schema-1 recording stay
	// readable; the Capture block is simply zero on them).
	if m.Schema != FixtureSchema && m.Schema != schemaV1 {
		return FixtureManifest{}, fmt.Errorf("fixture %s: unsupported manifest schema %d (want %d or %d)", dir, m.Schema, FixtureSchema, schemaV1)
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

// lineRuns is one screen family's reveal runs with their canonical lines:
// the profile's message set (Canonical, the distinct settled lines in
// first-seen run order) plus the per-frame membership the truth writer uses
// for text_is_full_line — a frame is FULL exactly when its own reading
// fold-matches its run's canonical line (the settled frames of a typewriter
// reveal, never its prefixes, never a noise variant).
type lineRuns struct {
	Canonical []string // distinct settled lines, first-seen run order
	lineOf    []int    // per frame: Canonical index of the frame's run's line, -1 outside the family
	runKey    []string // per frame: fold key of the frame's run's canonical line ("" outside the family)
}

// canonicalizeRuns groups a screen family's frames into reveal runs —
// maximal consecutive stretches of the family's screen class — and picks
// each run's canonical reading: the MODE fold key among the run's non-empty
// readings. A typewriter reveal holds its settled line for the bulk of the
// run, and a static line's OCR noise stays within a few variants, so the
// mode is the line the run displays; ties go to the longer raw text, then
// the last occurrence (deterministic: the fold keys are walked in sorted
// order, map order never leaks into the choice). A run whose canonical key
// an earlier run established reuses that line: the same dialogue line twice
// in a scene is one message (the first-seen scheme was right about dedupe,
// never about which reading represents the line).
func canonicalizeRuns(frames []FixtureFrame, texts []string, screen string) lineRuns {
	runs := lineRuns{lineOf: make([]int, len(frames)), runKey: make([]string, len(frames))}
	for i := range runs.lineOf {
		runs.lineOf[i] = -1
	}
	keyToLine := map[string]int{}
	i := 0
	for i < len(frames) {
		if frames[i].Screen != screen {
			i++
			continue
		}
		j := i
		for j < len(frames) && frames[j].Screen == screen {
			j++
		}
		line := -1
		var canKey string
		count := map[string]int{}
		lastIdx := map[string]int{}
		for k := i; k < j; k++ {
			if texts[k] == "" {
				continue
			}
			key := ocrFoldKey(texts[k])
			count[key]++
			lastIdx[key] = k
		}
		if len(count) > 0 {
			var best string
			bestN, bestLen, bestLast := 0, -1, -1
			keys := make([]string, 0, len(count))
			for k := range count {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				n, l, last := count[k], len(texts[lastIdx[k]]), lastIdx[k]
				switch {
				case n > bestN ||
					n == bestN && l > bestLen ||
					n == bestN && l == bestLen && last > bestLast:
					best, bestN, bestLen, bestLast = k, n, l, last
				}
			}
			canKey = best
			if l, ok := keyToLine[canKey]; ok {
				line = l
			} else {
				line = len(runs.Canonical)
				keyToLine[canKey] = line
				runs.Canonical = append(runs.Canonical, texts[lastIdx[canKey]])
			}
		}
		for k := i; k < j; k++ {
			runs.lineOf[k] = line
			if line >= 0 {
				runs.runKey[k] = canKey
			}
		}
		i = j
	}
	return runs
}

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
