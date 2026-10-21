package pipeline

import (
	"context"
	"image"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// analysis is one region's look at one frame, before anyone decides whether it is
// worth an OCR call.
type analysis struct {
	present bool
	hash    string
	crop    image.Image
	// input is what the engine would be handed (preprocessed, padded, scaled).
	input image.Image
	// empty records that the rect covered nothing at this resolution, which is a
	// different fact from "a box is there and it is blank".
	empty bool
	// pHash is the crop's 64-bit dHash (ADR-0115 §3): the tolerant half of the
	// unchanged-hash dedupe. havePHash is false for an empty crop, which must
	// not read as "perceptually equal to" the previous crop's hash.
	pHash     uint64
	havePHash bool
	// revealDetector/revealComplete: the game's reveal-complete marker was
	// looked for on this frame and its verdict (ADR-0152).
	revealDetector bool
	revealComplete bool
}

// analyze crops, decides textbox presence, preprocesses and hashes. It is pure
// CPU with no I/O and no shared state, and it runs on the frame-ordered stage
// rather than on the OCR pool because its output — hash equality — *is* the
// cancellation decision: a worker that hashed its own frame could not notice,
// while a recognition was running, that the screen behind it had changed. The
// cost is one copy plus one luma pass over the crop (a 640×480 frame's textbox
// region: ~0.1 ms), against tens of milliseconds for the OCR that follows it, so
// bounding the pool by OCR work — what docs/spec/01's "ROI worker pool" is for —
// is unaffected.
//
// rect is the band to crop, chosen by the caller: the profile's fixed band for
// a region that is not placement-aware, or the selected candidate band for a
// placement-aware textbox region (ADR-0137). analyze is agnostic to which it
// is — presence, the perceptual hash and the preprocessed hash all run on the
// band it is handed, which is what keeps the tracker and the dedupe on one
// stable stream.
func (r *Runner) analyze(f capture.Frame, reg games.Region, rect vision.Rect) analysis {
	crop := vision.CropNormalized(f.Image, rect, r.cfg.Crop)
	a := analysis{crop: crop}
	// The live view's last-crop record (ADR-0110): the box the cut actually
	// used, in frame coordinates (vision.CropRect shares the cut's own
	// active-window and MapRect spelling — the crop image is rebased to its
	// origin, so its bounds alone cannot carry the position). The developer
	// inspector and the signal route draw this box; noteCrop records nothing
	// for an empty box, so a region that crops nothing keeps answering "no
	// crop yet".
	cropRect := vision.CropRect(f.Image, rect, r.cfg.Crop)
	r.noteCrop(reg.ID, cropRect)
	// The game's reveal-complete marker (ADR-0152) sits relative to the band
	// on the full frame (below the box's edge), so it is read here, the one
	// stage that holds the frame and the band together.
	if d := r.cfg.RevealDetector; d != nil && f.Image != nil && !cropRect.Empty() {
		a.revealDetector = true
		a.revealComplete = d.RevealComplete(f.Image, cropRect)
	}
	if crop == nil || crop.Bounds().Empty() {
		a.empty = true
		return a
	}
	// Presence is measured on the raw colour crop, not on the preprocessed
	// one: AutoInvert and Otsu deliberately destroy the light/dark relationship
	// and the panel's colour this detector asks about. The panel read is the
	// triple gate of vision.PanelPresenceOf (ADR-0137, Amendment: the sea false
	// positive): dark enough to be a panel, carrying a line (the text share —
	// the letters a box exists to show; the old dark-only read called the open
	// sea a box and OCR'd it), and not a saturated scene wearing through the
	// band (the sea's dark is saturated blue, the panel's is its own
	// desaturated tint).
	pp := vision.PanelPresenceOf(crop, r.cfg.Presence.DarkLuma, vision.DefaultTextLuma, vision.DefaultSatFloor)
	a.present = r.cfg.Presence.Present(pp)
	// The perceptual dedupe key (ADR-0115 §3) is on the raw crop for the same
	// reason presence is: the question is "did the box the camera sees change",
	// and the preprocess recipe's binarisation would move the answer with the
	// recipe rather than with the picture.
	a.pHash, a.havePHash = vision.PerceptualHash(crop)
	// The dedupe key is a lossless luma hash of the preprocessed crop. Preprocessed
	// rather than raw for two reasons: the thing being suppressed is the OCR cost,
	// so the key must describe exactly what the engine would re-read; and a profile
	// that changes its recipe gets a fresh key namespace for free rather than a
	// cache of stale hypotheses (vision.Steps.Hash is what names that recipe in a
	// log).
	in := vision.Preprocess(crop, r.cfg.Preprocess)
	a.input = in
	a.hash = vision.ImageHash(in)
	return a
}

// ocrJob is one dispatched recognition.
//
// Its context is cancelled by the frame loop the moment a newer frame of the same
// region shows a different crop, and it carries a buffered result channel so the
// worker that ran it can never block on a loop that has already moved on: a
// result arriving after cancellation is what Stats.OCRStale counts, and the
// alternative — a worker parked forever on an unread channel — is a goroutine
// leak with an OCR subprocess behind it (docs/spec/05 "cancel on newer
// observation", AGENTS.md "deadline/cancellation propagation").
type ocrJob struct {
	id     int64
	region string
	hash   string
	// pHash is the crop's perceptual hash at dispatch (ADR-0115 §3): the
	// region's lastPHash is cleared when this read is cancelled or fails, the
	// same discipline as hash, so "perceptually unchanged" is never claimed
	// about a read that does not exist.
	pHash    uint64
	frameSeq int64
	ctx      context.Context
	cancel   context.CancelFunc
	// base is the run context the per-call deadline is a child of. The retry
	// policy (ModeReplay only) derives a fresh deadline from it after the job's
	// own one has expired, so a wedged-but-recoverable engine cannot burn the
	// whole retry budget on the first attempt's timer.
	base context.Context
	// input is the preprocessed crop (the recipe's output) the OCR engine's
	// local path reads. raw is the crop's actual pixels: engines that
	// implement ocr.RawCropRecognizer (the fallback engine does, ADR-0040
	// §6) run a natural-image secondary on it instead of on the recipe's
	// output.
	input   image.Image
	raw     image.Image
	started time.Time

	// dead is set by the frame loop when it stops wanting this result. Only the
	// ROI goroutine writes it, and only the ROI goroutine reads it, so the flag
	// needs no atomics: the worker never consults it, it just notices its context
	// was cancelled and returns whatever it has.
	dead bool
}

type ocrOutcome struct {
	job  *ocrJob
	res  ocr.Result
	err  error
	done time.Time
}

// pending is one observation waiting its turn at the reducer.
//
// Per-region FIFO order is what lets the dialogue state machine measure a
// stability window at all: it computes "unchanged for StableWindow" from the
// capture timestamps it is handed, and an observation stream that arrives out of
// order would report the gap backwards. A recognition whose result is cancelled
// or superseded therefore removes its pending observation rather than delivering
// it late — which is why "two OCR futures finish out of order" (docs/spec/12) is
// a counted drop in this package and a counted discard in the reducer.
type pending struct {
	frame capture.Frame
	reg   games.Region
	a     analysis
	job   *ocrJob // nil when the observation needs no recognition
	// phashSkip marks the perceptual-dedupe skip (ADR-0115 §3): the crop is
	// within tolerance of the crop the region last OCRed, so no OCR ran and the
	// observation rides the last successful read's text (see emit). The exact
	// hash-skip path does not need the flag: its pixels are identical, so the
	// lastOK-reuse condition in emit matches on the hash alone.
	phashSkip bool
	// ready holds the recognition result once the job resolves. It is a copy of
	// the engine's Result because recognition.Observation carries a pointer that
	// the reducer reads later, and the worker's stack frame is not a place to
	// point at.
	ready *ocr.Result
	err   error
	at    time.Time // when the frame entered this stage, for frame→ocr latency
	// readyAt is when the recognition landed; zero for a skipped one.
	readyAt time.Time
}

// resolved reports whether the observation can be handed to the reducer.
func (p *pending) resolved() bool { return p.job == nil || p.ready != nil || p.err != nil }
