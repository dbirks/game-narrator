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
func (r *Runner) analyze(f capture.Frame, reg games.Region) analysis {
	crop := vision.CropNormalized(f.Image, vision.RectOf(reg), r.cfg.Crop)
	a := analysis{crop: crop}
	if crop == nil || crop.Bounds().Empty() {
		a.empty = true
		return a
	}
	// Presence is measured on the raw grayscale crop, not on the preprocessed
	// one: AutoInvert and Otsu deliberately destroy the light/dark relationship
	// this detector asks about.
	a.present = darkShare(vision.ToGray(crop), r.cfg.Presence.DarkLuma) >= r.cfg.Presence.MinDarkShare
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

// darkShare is the fraction of pixels below luma. It walks Pix directly: a
// 1048×168 crop is 176 k pixels and At() would make that a method call each.
func darkShare(g *image.Gray, luma uint8) float64 {
	if g == nil || len(g.Pix) == 0 {
		return 0
	}
	dark := 0
	for _, v := range g.Pix {
		if v < luma {
			dark++
		}
	}
	return float64(dark) / float64(len(g.Pix))
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
	// frameSeq is the capture frame this read was started for. The replay
	// degradation counter needs it: a frame whose read fails after the retry
	// policy is named in the report by its sequence, not by a wall-clock guess.
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
