package pipeline

import (
	"image"
	"sync"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/games"
)

// liveView is the frame-ordered stage's observation for concurrent readers
// (ADR-0110). Two bounded, overwrite-in-place facts the UI asks while the run
// is live:
//
//   - the newest frame still in memory. One slot: Put overwrites, so the
//     pipeline holds exactly one extra reference to the newest frame (an
//     1080p RGBA frame is ~8 MB; the ADR-0082 bridge's LatestSlot already
//     holds one). The preview serves it from memory — never a second device
//     grab, never stored — and the signal route reads its pixels for the
//     active-picture judgment.
//   - the last crop rectangle the ROI stage computed per region, in frame
//     coordinates. One entry per region id (the profile's bounded set). The
//     developer inspector and the signal route draw the same box the runtime
//     crops through the same MapRect, so they cannot disagree.
//
// Both halves are written only on the frame-ordered stage (noteFrame in
// roiLoop, noteCrop in analyze) and read from other goroutines, so the mutex
// is the same cross-goroutine-read guarantee the tracker's Box carries
// (ADR-0082 amendment 1): a reader sees one whole frame or one whole
// rectangle, never a torn pair. Memory is bounded by construction — one
// frame and one rectangle per region, no history, no accumulation — and a
// finished session's runner is unreferenced with the session, which is when
// the held frame's pixels are released.
type liveView struct {
	mu        sync.Mutex
	frame     capture.Frame
	hasFrame  bool
	lastCrops map[string]image.Rectangle
}

// noteFrame records the frame on the frame-ordered stage, once per frame,
// before the regions analyse it. It overwrites the slot: the newest pixels
// are the only ones the preview owes.
func (r *Runner) noteFrame(f capture.Frame) {
	r.liveView.mu.Lock()
	r.liveView.frame = f
	r.liveView.hasFrame = true
	r.liveView.mu.Unlock()
}

// LatestFrame reports the newest frame the frame-ordered stage has seen, for
// a concurrent reader (the API's preview and signal routes). ok is false
// before the first frame — a session that has started but not delivered yet
// answers "no frame in memory", not an empty frame the UI would render.
func (r *Runner) LatestFrame() (f capture.Frame, ok bool) {
	r.liveView.mu.Lock()
	defer r.liveView.mu.Unlock()
	return r.liveView.frame, r.liveView.hasFrame
}

// noteCrop records one region's last crop box (the frame-coordinate box the
// ROI stage's cut used) after the ROI stage computed it. An empty box
// records nothing: a region that crops nothing has no last crop, and the
// answer for it stays "no crop yet", which the caller renders as null
// rather than a whole-frame box the runtime never cut.
func (r *Runner) noteCrop(regionID string, box image.Rectangle) {
	if box.Dx() <= 0 || box.Dy() <= 0 {
		return
	}
	lv := &r.liveView
	lv.mu.Lock()
	if lv.lastCrops == nil {
		lv.lastCrops = make(map[string]image.Rectangle)
	}
	lv.lastCrops[regionID] = box
	lv.mu.Unlock()
}

// LastCrop reports the last crop rectangle the ROI stage computed for the
// named region, in frame coordinates. ok is false before the region's first
// crop (a run that has not processed a frame, or a region that has not
// cropped anything yet).
func (r *Runner) LastCrop(regionID string) (rect image.Rectangle, ok bool) {
	lv := &r.liveView
	lv.mu.Lock()
	defer lv.mu.Unlock()
	rect, ok = lv.lastCrops[regionID]
	return rect, ok
}

// DialogueCrop reports the last crop rectangle of the dialogue region: the
// first region (in profile order) whose text-kind hint is dialogue — the
// textbox when the profile names one, a region the runner's fallback hint
// scores as dialogue otherwise. It is the box the signal route draws as "the
// last dialogue-region crop", and it is null (ok false) while the run has
// not computed one. The kind-hint lookup is the runner's own (newKindHints):
// a sign region's words score as signs, so a sign never answers for the
// dialogue box.
func (r *Runner) DialogueCrop() (rect image.Rectangle, ok bool) {
	for _, reg := range r.cfg.Regions {
		if r.kindHints[reg.ID] != games.TextDialogue {
			continue
		}
		return r.LastCrop(reg.ID)
	}
	return image.Rectangle{}, false
}
