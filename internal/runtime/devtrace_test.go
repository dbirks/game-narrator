package runtime

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"testing"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/vision"
)

// solidFrame builds one deterministic frame: a single colour, a stable hash
// per colour, and a source-clock timestamp.
func solidFrame(col color.Color, tsNS int64) capture.Frame {
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, col)
		}
	}
	return capture.Frame{
		Seq:                tsNS,
		SourceClock:        capture.ClockReplay,
		CaptureTimestampNS: tsNS,
		Image:              img,
		Width:              320,
		Height:             240,
		PixFmt:             capture.PixRGBA,
		Hash:               capture.HashFrame(img),
	}
}

// TestDevTraceQuotaEvictsOldest stores more samples than the quota holds and
// proves the oldest drop out first while the newest stays readable — the
// "per-session byte quota, oldest evicted" rule from the task and docs/spec/13.
func TestDevTraceQuotaEvictsOldest(t *testing.T) {
	ctx := context.Background()
	const quota = 200_000 // small on purpose: a few frames fill it
	r, err := NewDevTraceRecorder(t.TempDir(), quota)
	if err != nil {
		t.Fatalf("NewDevTraceRecorder: %v", err)
	}

	// Different colours give different hashes, so the sampling gate stores
	// every frame. Each 320x240 solid JPEG is a few hundred bytes, so ~300
	// frames overflow the 200 KiB quota.
	var firstSHA, lastSHA string
	for i := 0; i < 300; i++ {
		f := solidFrame(color.RGBA{R: uint8(10 + i%200), G: uint8(i % 250), B: uint8(i % 240)}, int64(i*10_000_000))
		s, err := r.recordFrame(ctx, f, nil)
		if err != nil {
			t.Fatalf("recordFrame %d: %v", i, err)
		}
		if s == nil {
			t.Fatalf("frame %d skipped by the sampling gate, want stored (unique colour)", i)
		}
		if i == 0 {
			firstSHA = s.frame
		}
		lastSHA = s.frame
	}
	if r.totalBytes() > quota {
		t.Fatalf("retained %d bytes, over the %d quota", r.totalBytes(), quota)
	}

	// The newest is still there...
	got, ok := r.lookup(299 * 10_000_000)
	if !ok || got.frame != lastSHA {
		t.Fatalf("lookup(latest) = %v ok=%v, want the newest sample", got, ok)
	}
	// ...the oldest is gone, and its hash no longer resolves to any sample.
	for _, s := range r.samples {
		if s.frame == firstSHA {
			t.Fatalf("oldest sample still retained after eviction")
		}
	}
}

// TestDevTraceSamplingGate rate-limits storage of an unchanged picture: one
// stored sample per devTraceMinSampleInterval window, no matter how many
// identical frames arrive — the time quota docs/spec/13 asks for.
func TestDevTraceSamplingGate(t *testing.T) {
	ctx := context.Background()
	r, err := NewDevTraceRecorder(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewDevTraceRecorder: %v", err)
	}
	f := solidFrame(color.RGBA{R: 200}, 0)
	// Thirty frames of the same picture a few ms apart: at most a handful of
	// stored samples, never one per frame.
	stored := 0
	for i := 0; i < 30; i++ {
		s, err := r.recordFrame(ctx, f, nil)
		if err != nil {
			t.Fatalf("recordFrame %d: %v", i, err)
		}
		if s != nil {
			stored++
		}
		if i == 0 && s == nil {
			t.Fatal("the very first frame must be stored")
		}
	}
	if stored < 1 || stored > 4 {
		t.Fatalf("stored %d samples for 30 identical frames, want at most a few", stored)
	}
}

// TestDevTraceLookupNearestBefore answers the text.observed enrichment: the
// lookup for a timestamp that fell between two stored samples returns the
// nearest kept sample at or before it, and the crops are keyed by region id.
func TestDevTraceLookupNearestBefore(t *testing.T) {
	ctx := context.Background()
	r, err := NewDevTraceRecorder(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewDevTraceRecorder: %v", err)
	}
	regions := []games.Region{{ID: "bottom", RegionType: games.RegionTextbox, RectX: 0.1, RectY: 0.7, RectW: 0.8, RectH: 0.2}}
	// Two distinct frames, 2 s apart on the source clock.
	s1, err := r.recordFrame(ctx, solidFrame(color.RGBA{R: 10}, 1_000_000_000), regions)
	if err != nil || s1 == nil {
		t.Fatalf("first sample: %v %v", s1, err)
	}
	s2, err := r.recordFrame(ctx, solidFrame(color.RGBA{G: 10}, 3_000_000_000), regions)
	if err != nil || s2 == nil {
		t.Fatalf("second sample: %v %v", s2, err)
	}
	if s1.frame == s2.frame {
		t.Fatal("two different frames stored the same hash")
	}
	if s1.crops["bottom"] == "" || s2.crops["bottom"] == "" {
		t.Fatalf("crops not keyed by region: %+v %+v", s1.crops, s2.crops)
	}

	got, ok := r.lookup(2_500_000_000) // between the two
	if !ok || got.frame != s1.frame {
		t.Fatalf("lookup(2.5s) = %v ok=%v, want the 1s sample (nearest before)", got, ok)
	}
	if got.crops["bottom"] != s1.crops["bottom"] {
		t.Fatalf("crop mismatch on the resolved sample")
	}
	// A timestamp before the first sample falls back to the newest kept
	// sample rather than failing: the observation still gets evidence.
	got, ok = r.lookup(100_000_000)
	if !ok || got.frame != s2.frame {
		t.Fatalf("lookup(before first) = %v ok=%v, want fallback to the latest sample", got, ok)
	}
}

// TestDevTraceStoreRejectsOversized is the per-blob bound: a single sample
// larger than the blob cap is refused before it can eat the session quota.
func TestDevTraceStoreRejectsOversized(t *testing.T) {
	big := make([]byte, devTraceMaxBlobBytes+1)
	r, err := NewDevTraceRecorder(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewDevTraceRecorder: %v", err)
	}
	if _, err := r.storeBytes(context.Background(), big, "image/jpeg"); err == nil {
		t.Fatal("oversized sample accepted, want a refusal")
	}
}

// borderedDevFrame is a 16:9 frame whose active window is the flat-mid-grey
// rect (20,20)-(300,160) inside pure-black borders — the synthetic shape
// vision.ActiveArea settles on (AGENTS.md rule 5: generated pixels only).
func borderedDevFrame(tsNS int64) capture.Frame {
	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			var c color.Color = color.Black
			if x >= 20 && x < 300 && y >= 20 && y < 160 {
				c = color.Gray{Y: 128}
			}
			img.Set(x, y, c)
		}
	}
	return capture.Frame{
		Seq:                tsNS,
		SourceClock:        capture.ClockReplay,
		CaptureTimestampNS: tsNS,
		Image:              img,
		Width:              320,
		Height:             180,
		PixFmt:             capture.PixRGBA,
		Hash:               capture.HashFrame(img),
	}
}

// TestDevTraceSampleCarriesTheCropBasis pins the ADR-0082 amendment 1 wiring:
// with the session's crop options (a settled tracker), the sample names the
// window it was cut through and the text.observed enrichment carries it, so
// the developer inspector can show the crop basis.
func TestDevTraceSampleCarriesTheCropBasis(t *testing.T) {
	ctx := context.Background()
	r, err := NewDevTraceRecorder(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewDevTraceRecorder: %v", err)
	}
	f := borderedDevFrame(1_000_000_000)
	tr := vision.NewTracker(vision.TrackerConfig{})
	tr.Settle(f.Image) // the one-shot-style settle; the window is the bordered rect
	r.SetCropOptions(vision.CropOptions{Letterbox: vision.LetterboxDetect, Tracker: tr})
	regions := []games.Region{{ID: "box", RectX: 0.1, RectY: 0.1, RectW: 0.8, RectH: 0.8}}
	s, err := r.recordFrame(ctx, f, regions)
	if err != nil {
		t.Fatalf("recordFrame: %v", err)
	}
	if s == nil {
		t.Fatal("no sample stored")
	}
	if s.basis == nil || !s.basis.Settled {
		t.Fatalf("basis = %+v, want the settled window", s.basis)
	}
	// Fractions of the frame: (20/320, 20/180, 280/320, 140/180).
	if s.basis.X != 20/320.0 || s.basis.Y != 20/180.0 || s.basis.W != 280/320.0 || s.basis.H != 140/180.0 {
		t.Fatalf("basis fractions = %.4f,%.4f,%.4f,%.4f, want %.4f,%.4f,%.4f,%.4f",
			s.basis.X, s.basis.Y, s.basis.W, s.basis.H, 20/320.0, 20/180.0, 280/320.0, 140/180.0)
	}
	// The enrichment carries the window and the evidence references.
	out := attachDevTraceData(mustJSON(map[string]any{"region_id": "box"}), s, "box")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("enriched payload is not JSON: %v", err)
	}
	win, ok := m["active_window"].(map[string]any)
	if !ok {
		t.Fatalf("enriched payload has no active_window: %v", m)
	}
	if win["x"] != 20/320.0 || win["y"] != 20/180.0 || win["w"] != 280/320.0 || win["h"] != 140/180.0 {
		t.Fatalf("active_window = %v, want the settled fractions", win)
	}
	if win["settled"] != true {
		t.Fatalf("active_window.settled = %v, want true", win["settled"])
	}
	if m["frame_asset"] == nil || m["crop_asset"] == nil {
		t.Fatalf("enriched payload lost the evidence references: %v", m)
	}
}

// TestDevTraceSampleBasisDegradesToWholeFrame pins the other side: a tracker
// that never settled (a borderless source) and a session without a tracker
// both name the whole frame, so the inspector can say the crop basis is the
// full picture.
func TestDevTraceSampleBasisDegradesToWholeFrame(t *testing.T) {
	ctx := context.Background()
	f := solidFrame(color.RGBA{R: 90}, 2_000_000_000) // 4:3, no bars
	regions := []games.Region{{ID: "box", RectX: 0.1, RectY: 0.1, RectW: 0.8, RectH: 0.8}}

	// A tracker that never settled.
	r1, err := NewDevTraceRecorder(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewDevTraceRecorder: %v", err)
	}
	tr := vision.NewTracker(vision.TrackerConfig{})
	r1.SetCropOptions(vision.CropOptions{Letterbox: vision.LetterboxDetect, Tracker: tr})
	s1, err := r1.recordFrame(ctx, f, regions)
	if err != nil || s1 == nil {
		t.Fatalf("recordFrame: %v %v", s1, err)
	}
	if s1.basis.Settled || s1.basis.X != 0 || s1.basis.Y != 0 || s1.basis.W != 1 || s1.basis.H != 1 {
		t.Fatalf("unsettled tracker basis = %+v, want the whole frame", s1.basis)
	}

	// No tracker at all.
	r2, err := NewDevTraceRecorder(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewDevTraceRecorder: %v", err)
	}
	s2, err := r2.recordFrame(ctx, solidFrame(color.RGBA{G: 90}, 3_000_000_000), regions)
	if err != nil || s2 == nil {
		t.Fatalf("recordFrame: %v %v", s2, err)
	}
	if s2.basis.Settled || s2.basis.W != 1 || s2.basis.H != 1 {
		t.Fatalf("no-tracker basis = %+v, want the whole frame", s2.basis)
	}
}
