package runtime

import (
	"context"
	"image"
	"image/color"
	"testing"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/games"
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
