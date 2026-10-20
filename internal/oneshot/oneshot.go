// Package oneshot runs the product's single-frame recognition path over one
// image: the profile's stored region crops (or the whole active picture when
// no game is named), the pipeline's preprocessing recipe, the OCR engine,
// the matcher's text fold and the top-k candidate ranking.
//
// It exists so the operator can ask "what does the device read right now?"
// (issue #1) through the product's own code paths instead of grabbing a
// frame with FFmpeg and running Tesseract by hand: the same
// vision.CropNormalized / vision.Preprocess calls the pipeline's ROI stage
// makes (internal/pipeline/roi.go), the same panel-presence walk
// (vision.DarkShare), the same engine the runtime wires (ocr.Tesseract by
// default) and the same matcher index and fold a live session ranks with.
// Nothing here reimplements the pipeline; a session's machinery (reducer,
// tracker, queues, retries) is deliberately absent — one frame needs none
// of it — and the crop options mirror the production crop configuration:
// LetterboxDetect plus one tracker that settles the active-picture window
// from the frame itself (Tracker.Settle, ADR-0082 amendment 1), so a one-shot
// read of a frame the session saw cuts the same box the session would cut.
package oneshot

import (
	"context"
	"image"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// FallbackRegionID is the identity the no-game mode's synthetic region takes
// in the report, standing in for a stored regions row id.
const FallbackRegionID = "frame"

// FallbackRegionLabel is the human label of the no-game region.
const FallbackRegionLabel = "whole active picture"

// Options configures one Run. Every zero field takes the same default the
// live pipeline takes, so a one-shot read and a session read of the same
// frame go through one recipe (pipeline.Config.normalize is the reference).
type Options struct {
	// Source labels where the image came from ("image:<path>" or
	// "device:<id> mode:<mode>") — the report's provenance line. The
	// image cannot say it, so the caller stamps it.
	Source string
	// Engine is the recognition engine; nil takes ocr.Tesseract{} — the
	// production default the runtime uses when no engine is configured.
	Engine ocr.Engine
	// Regions are the profile's stored regions; nil or empty runs the whole
	// active picture (letterbox-detected) — the no-game diagnostic.
	Regions []games.Region
	// Index ranks the reads when non-nil (matcher.Build over the published
	// content); nil skips the matcher entirely.
	Index *matcher.Index
	// RankOptions tunes the matcher; zero fields take
	// matcher.DefaultRankOptions.
	RankOptions matcher.RankOptions
	// Preprocess is the vision recipe; zero takes vision.DefaultSteps() —
	// the pipeline's own default.
	Preprocess vision.Steps
	// Presence is the panel detector's parameters; zero takes
	// pipeline.DefaultPresence().
	Presence pipeline.PresenceConfig
	// Timeout bounds one engine call; zero takes ocr.DefaultTimeout.
	Timeout time.Duration
	// TextKindHint seeds the matcher observation; empty takes
	// games.TextDialogue — the pipeline's default for a textbox region.
	TextKindHint games.TextKind
	// Debug, when non-nil, receives each region's raw crop and the
	// preprocessed input before the engine call — the --save-debug seam.
	// The one-shot path keeps no images; the caller (the CLI) writes them.
	Debug func(regionID string, rawCrop, preprocessed image.Image)
}

// normalize fills the defaults exactly as pipeline.Config.normalize does for
// the fields a single frame reads.
func (o Options) normalize() Options {
	if o.Engine == nil {
		o.Engine = ocr.Tesseract{}
	}
	if o.Preprocess.Scale == 0 {
		o.Preprocess = vision.DefaultSteps()
	}
	// The pipeline's PresenceConfig.normalize is unexported; mirror it here
	// against the exported default so the zero value reads the same way.
	if o.Presence.DarkLuma == 0 {
		o.Presence.DarkLuma = pipeline.DefaultPresence().DarkLuma
	}
	if !(o.Presence.MinDarkShare > 0) { // unset, NaN or negative
		o.Presence.MinDarkShare = pipeline.DefaultPresence().MinDarkShare
	}
	if o.Presence.MinDarkShare > 1 {
		o.Presence.MinDarkShare = 1
	}
	if o.Timeout <= 0 {
		o.Timeout = ocr.DefaultTimeout
	}
	if o.TextKindHint == "" {
		o.TextKindHint = games.TextDialogue
	}
	return o
}

// RegionReport is one region's look at the frame, in the same order the
// pipeline's ROI stage produces its facts: crop, presence, read.
type RegionReport struct {
	// ID is the stored region id, or FallbackRegionID in the no-game mode.
	ID string
	// Label is the stored region label; empty for the fallback region.
	Label string
	// Rect is the region as stored, in fractions of the active picture —
	// 0,0,1,1 for the fallback region.
	RectX, RectY, RectW, RectH float64
	// CropX/Y/W/H is the crop's pixel bounds in the input image.
	CropX, CropY, CropW, CropH int
	// Present is the pipeline's panel detector's answer on the raw crop
	// (the dark share at Presence's thresholds); the live pipeline's
	// presence fact, so a "no text" reading can be told apart from a
	// "no box" one.
	Present bool
	// Empty records that the rect covered nothing at this resolution — a
	// different fact from "a box is there and it is blank".
	Empty bool
	// Text is the engine's raw reading, un-normalized (docs/spec/05 keeps
	// raw and normalized separate).
	Text string
	// Normalized is the matcher fold of Text — the key the ranking was
	// computed over; empty when no index was supplied.
	Normalized string
	// MeanConf is the engine's word-confidence mean, [0,1]; 0 when nothing
	// was read.
	MeanConf float64
	// Engine names the producer (ocr.Namer), or the type name when the
	// engine does not implement it.
	Engine string
	// Duration is the engine call's wall time.
	Duration time.Duration
	// OCRError names the engine failure when the read failed; empty on a
	// read (including a read of nothing).
	OCRError string
	// Rank is the matcher's answer for this reading; nil when no index was
	// supplied or the read produced no text.
	Rank *Ranking
}

// Ranking is the matcher's answer, the part of matcher.Ranking the
// operator acts on: the shortlist with scores and the abstain verdict.
type Ranking struct {
	// Reason is the matcher's Reason* code for the answer.
	Reason string
	// Abstain is the matcher's local-evidence verdict (docs/spec/05).
	Abstain bool
	// Candidates is ordered best-first, length ≤ TopK.
	Candidates []Candidate
}

// Candidate is one ranked message: the id the operator can look up and the
// two score spellings the matcher reports (raw log-linear sum, softmax
// probability).
type Candidate struct {
	MessageID   string
	Score       float64
	Probability float64
}

// Report is the one-shot answer over one image.
type Report struct {
	// Source is the provenance line the caller stamped (Options.Source).
	Source string
	// Width and Height are the input image's dimensions.
	Width, Height int
	// Regions is one entry per region read, in profile order (a single
	// fallback region in the no-game mode).
	Regions []RegionReport
	// Total is the whole Run's wall time.
	Total time.Duration
}

// Run reads img exactly the way a live session would read one of its frames
// for these regions: crop with the production crop options, measure
// presence on the raw crop, preprocess with the recipe, run the engine on
// the preprocessed input (handing the raw crop along to engines that read
// both), fold the text and rank it when an index is supplied.
//
// It never panics on a degenerate image or rect: a crop that covers nothing
// is reported as an empty read, and an engine failure is reported per
// region, never returned as an error that drops the other regions'
// evidence. Run itself returns no error — the caller's context deadline is
// honoured between regions and inside the engine call, and the report is
// the answer (including per-region OCRError) either way.
func Run(ctx context.Context, img image.Image, opts Options) Report {
	o := opts.normalize()
	start := time.Now()

	rep := Report{Source: o.Source}
	if img != nil {
		b := img.Bounds()
		rep.Width, rep.Height = b.Dx(), b.Dy()
	}

	// The regions to read: the profile's, or one whole-active-picture
	// region in the no-game mode.
	regions := opts.Regions
	fallback := len(regions) == 0
	if fallback {
		regions = []games.Region{{ID: FallbackRegionID, Label: FallbackRegionLabel}}
	}

	// The production crop configuration (pipelineConfig: LetterboxDetect plus
	// one tracker per session), settled from the frame itself: a single frame
	// has no stream to hysteresise over, so the window the tracker would have
	// adopted is the frame's own stateless scan (Tracker.Settle, ADR-0082
	// amendment 1). Every region — the profile's stored regions and the
	// no-game fallback alike — maps through it, which is the same mapping the
	// session applies to its regions once the window has settled. A frame
	// that reports no window (borderless, or a failed scan) settles nothing
	// and degrades to the whole frame: the LetterboxNone mapping.
	tracker := vision.NewTracker(vision.TrackerConfig{})
	tracker.Settle(img)
	crop := vision.CropOptions{Letterbox: vision.LetterboxDetect, Tracker: tracker}

	// One engine context for every region read, so a deadline reached during
	// region N reports the remaining regions promptly rather than blocking
	// on each of them.
	callCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	for _, reg := range regions {
		var rp vision.Rect
		if fallback {
			rp = vision.Rect{W: 1, H: 1}
		} else {
			rp = vision.RectOf(reg)
		}
		rep.Regions = append(rep.Regions, runRegion(callCtx, img, rp, crop, reg, o))
		if ctx.Err() != nil {
			// The caller is done; the remaining regions are left unrun and
			// the run stops rather than spending the rest of the budget on
			// nobody.
			break
		}
	}
	rep.Total = time.Since(start)
	return rep
}

// activeWindow is the window a one-shot crop maps onto, in the same order
// CropNormalized applies it for the one-shot's crop options (LetterboxDetect
// plus a tracker settled from the frame, ADR-0082 amendment 1): the
// tracker's accepted window when one settled, the stateless ActiveArea scan
// otherwise, the whole frame when the scan reports nothing. Keeping the
// report's box and the crop's pixels in agreement is the point: the one-shot
// reader cuts no box the session would not cut.
func activeWindow(img image.Image, opts vision.CropOptions) image.Rectangle {
	if img == nil {
		return image.Rectangle{}
	}
	b := img.Bounds()
	if opts.Tracker != nil {
		if a, ok := opts.Tracker.Box(); ok {
			return a
		}
	}
	if opts.Letterbox == vision.LetterboxDetect {
		if a, ok := vision.ActiveArea(img); ok {
			return a
		}
	}
	return b
}

// runRegion is one region's read: the pipeline's ROI-stage sequence (crop,
// presence, preprocess, recognize) with the session machinery — dedupe,
// cancellation, queues, retries — removed because one frame has no newer
// frame to supersede it.
func runRegion(ctx context.Context, img image.Image, rect vision.Rect, crop vision.CropOptions, reg games.Region, o Options) RegionReport {
	rep := RegionReport{ID: reg.ID, Label: reg.Label}
	r := rect
	rep.RectX, rep.RectY, rep.RectW, rep.RectH = r.X, r.Y, r.W, r.H

	// Crop — the exact call the pipeline's analyze makes (roi.go). A nil or
	// empty image and a degenerate rect both come back empty, which is the
	// "read no text" degrade, not a fault.
	cropImg := vision.CropNormalized(img, rect, crop)
	if cropImg == nil {
		cropImg = image.NewRGBA(image.Rectangle{})
	}
	b := cropImg.Bounds()
	if b.Empty() {
		rep.Empty = true
		return rep
	}
	// The crop is a copy with a (0,0) origin, so the report names the box it
	// covers in the input image with the same projection CropNormalized
	// applies — MapRect on the same active window (activeWindow is the
	// one-shot's window: the tracker settled from the frame, degrading to the
	// stateless scan then the whole frame, ADR-0082 amendment 1).
	box := vision.MapRect(activeWindow(img, crop), rect)
	rep.CropX, rep.CropY, rep.CropW, rep.CropH = box.Min.X, box.Min.Y, box.Dx(), box.Dy()

	// Presence is measured on the raw grayscale crop, not the preprocessed
	// one (AutoInvert and Otsu destroy the light/dark relationship the
	// detector asks about) — the same fact the pipeline's analyze records.
	rep.Present = vision.DarkShare(vision.ToGray(cropImg), o.Presence.DarkLuma) >= o.Presence.MinDarkShare

	// Preprocess — the pipeline's recipe on the crop.
	input := vision.Preprocess(cropImg, o.Preprocess)
	if o.Debug != nil {
		o.Debug(reg.ID, cropImg, input)
	}

	// Read — the same engine-call shape as the pipeline's ocrRecognize: an
	// engine that reads both (ocr.RawCropRecognizer, ADR-0040) gets the
	// preprocessed crop and the raw crop, everything else the preprocessed
	// input alone.
	var res ocr.Result
	var err error
	if raw, ok := o.Engine.(ocr.RawCropRecognizer); ok {
		res, err = raw.RecognizeWithRawCrop(ctx, input, cropImg)
	} else {
		res, err = o.Engine.Recognize(ctx, input)
	}
	if namer, ok := o.Engine.(ocr.Namer); ok {
		rep.Engine = namer.Name()
	}
	rep.MeanConf = res.MeanConf
	rep.Duration = res.Duration
	if err != nil {
		rep.OCRError = err.Error()
		return rep
	}
	rep.Text = res.Text
	if res.Engine != "" {
		rep.Engine = res.Engine
	}

	// Rank — only when an index is supplied and the read produced text;
	// the observation carries the raw text (the matcher folds it — the
	// session's rankWorker does exactly this) and the engine's mean
	// confidence, which feeds the visibility component only.
	if o.Index != nil && res.Text != "" {
		rank := o.Index.Rank(matcher.Observation{
			Text:         res.Text,
			OCRConf:      res.MeanConf,
			TextKindHint: string(o.TextKindHint),
		}, o.RankOptions)
		rep.Normalized = o.Index.Fold(res.Text)
		cands := make([]Candidate, 0, len(rank.Candidates))
		for _, c := range rank.Candidates {
			cands = append(cands, Candidate{MessageID: c.MessageID, Score: c.Score, Probability: c.Probability})
		}
		rep.Rank = &Ranking{Reason: rank.Reason, Abstain: rank.Abstain, Candidates: cands}
	}
	return rep
}
