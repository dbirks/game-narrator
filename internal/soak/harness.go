// harness.go is the soak's two-layer composition over the synthetic clip:
// the same arrangement the scenario harness runs (ADR-0037) —
//
//	Layer A (recognition): the real pipeline in ModeReplay over the looping
//	replay source (real frame decoding, presence detection, the region
//	tracker, the matcher, the session reducer), with the OCR engine answered
//	from the clip's truth.
//
//	Layer B (providers): a sidecar session (the same internal/session
//	component layer A runs) paced on the clip's looped capture clock; for
//	every commit it issues a real systemone.Decide against the chaos
//	decision gateway and, when the decision is applied, a real
//	tts.Generate against the chaos TTS gateway.
//
// The difference from the scenario harness: the clip loops (replay.Loop)
// for the whole duration, the provider faults are seeded chaos instead of
// fixed windows, and every SampleEvery the harness reports its resource
// counters. The sidecar's keep mask and the frame dropper share one
// deterministic drop mask, so both layers see the same frames (the
// scenario's keepMask rule).

package soak

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/replay"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// Shared identity for both layers, mirroring the scenario harness: one
// session, one merged timeline (docs/spec/08 event correlation).
const (
	soakSessionID     = "soaksession"
	soakProfileRev    = "1"
	soakDecisionModel = "clef-fake"
	soakTTSModel      = "tts-fake"
	soakVoiceID       = "synthetic-narrator"
	// providerTimeout bounds one provider call, the way the scenario
	// harness's budget does: a chaos-delayed answer that outlives it is a
	// timeout and an honest abstain.
	providerTimeout = 10 * time.Second
)

// clip is one loaded synthetic clip: the truth rows and the loop geometry
// the sidecar's capture clock runs on (the replay source's loop advances
// each pass by stride = step + last - first).
type clip struct {
	dir    string
	truth  []scenario.Truth
	first  int64 // first row's capture ts (ns)
	stride int64 // per-pass advance (ns)
	// dropMask: the frame indices this pass withholds, shared by the frame
	// dropper (layer A) and the sidecar's keep mask (layer B).
	dropMask map[int]bool
}

func (c *clip) rows() int { return len(c.truth) }

// passOf maps a capture timestamp (ns) to its pass index on the clip's looped
// clock: row ts = truth.TsNS + pass*stride.
func (c *clip) passOf(tsNS int64) int {
	if tsNS < c.first {
		return 0
	}
	return int((tsNS - c.first) / c.stride)
}

// frameTruth is one truth row with its clip: the unit the union engine
// hashes (the clips share the profile, so one engine answers every clip).
type frameTruth struct {
	dir  string
	file string
	text string
	full bool
}

// truthEngine answers the pipeline's OCR reads from the clip's truth, keyed by
// the preprocessed crop hash (the scenario's truthocr.go, copied: the soak
// must run the same OCR arrangement so the load is the load it claims to be —
// the synthetic stub's provenance label, not Tesseract quality, is what this
// run tests).
type truthEngine struct {
	byHash map[string]ocr.Result
}

func newTruthEngine(repoRoot string, profile *scenario.Profile, rows []frameTruth) (*truthEngine, error) {
	regs := profile.ToRegions()
	if len(regs) == 0 {
		return nil, fmt.Errorf("soak truth engine: the profile declares no region")
	}
	e := &truthEngine{byHash: make(map[string]ocr.Result, len(rows))}
	for _, row := range rows {
		img, err := readFrame(filepath.Join(repoRoot, row.dir, row.file))
		if err != nil {
			return nil, fmt.Errorf("soak truth engine: read %s/%s: %w", row.dir, row.file, err)
		}
		crop := vision.CropNormalized(img, vision.RectOf(regs[0]), vision.CropOptions{})
		if crop == nil || crop.Bounds().Empty() {
			continue
		}
		in := vision.Preprocess(crop, vision.DefaultSteps())
		e.byHash[vision.ImageHash(in)] = truthResult(row.text, row.full)
	}
	if len(e.byHash) == 0 {
		return nil, fmt.Errorf("soak truth engine: no frame produced a crop for region %q", regs[0].ID)
	}
	return e, nil
}

func (e *truthEngine) Recognize(_ context.Context, img image.Image) (ocr.Result, error) {
	h := vision.ImageHash(img)
	if res, ok := e.byHash[h]; ok {
		return res, nil
	}
	return ocr.Result{}, fmt.Errorf("soak truth engine: no truth row for crop hash %s (harness defect)", h)
}

func (e *truthEngine) Name() string { return "truth" }

func readFrame(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

// truthResult shapes one truth row the way the scenario's truthOCR does:
// full lines read 0.91, typewriter prefixes 0.85, empty reads 0.
func truthResult(text string, full bool) ocr.Result {
	conf := 0.0
	if text != "" {
		conf = 0.85
		if full {
			conf = 0.91
		}
	}
	res := ocr.Result{Text: text, Engine: "truth", MeanConf: conf}
	if text != "" {
		res.Lines = []ocr.Line{{Text: text, Conf: conf}}
	}
	return res
}

// dropMaskFor picks the per-pass frame indices a chaos run withholds: k
// distinct indices from [1, rows) (never frame 0, the pacing anchor),
// deterministic in the seed. Both layers share the mask so the sidecar's
// commits and the pipeline's frames stay in lockstep.
func dropMaskFor(seed int64, rows, k int) map[int]bool {
	if k <= 0 || rows < 2 {
		return nil
	}
	if k > rows/2 {
		k = rows / 2
	}
	rng := rand.New(rand.NewSource(seed + 1))
	mask := make(map[int]bool, k)
	for len(mask) < k {
		i := rng.Intn(rows-1) + 1
		mask[i] = true
	}
	return mask
}

// frameDropper wraps a replay.Source and withholds the masked frame of each
// pass (the chaos form of the scenario's drop_frames fault, which keeps the
// frame's Seq gap so the loss is visible downstream).
type frameDropper struct {
	inner    *replay.Source
	rows     int
	mask     map[int]bool
	passRows int
	dropped  atomic.Int64
}

func (d *frameDropper) ID() string { return d.inner.ID() + ":chaos-drop" }

func (d *frameDropper) Start(ctx context.Context) (<-chan capture.Frame, error) {
	in, err := d.inner.Start(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan capture.Frame, 32)
	go func() {
		defer close(out)
		for f := range in {
			if d.mask != nil && d.mask[d.passRows%d.rows] {
				d.dropped.Add(1)
				d.passRows++
				continue
			}
			d.passRows++
			select {
			case out <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (d *frameDropper) Stats() capture.Stats {
	st := d.inner.Stats()
	st.DroppedSlot += uint64(d.dropped.Load())
	return st
}

func (d *frameDropper) Close() error { return d.inner.Close() }

func (d *frameDropper) Dropped() int64 { return d.dropped.Load() }
