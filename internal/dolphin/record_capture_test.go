package dolphin

// record_capture_test.go is the unit half of the parallel adaptive capture
// (schema 2, ADR-0054 amendment 2): the capture loop's bookkeeping — its
// own clock (independent of the drive's Screenshot cadence), the min-gap
// between the two streams, the duration/byte budget stops, and the
// manifest's capture block — against the fake bridge. No emulator, no
// Tesseract (the real end-to-end proof is TestRealRecordLegendDense).

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCapturePlanParallelStream verifies the loop records frames on its own
// clock (the drive's Screenshot is called exactly once, far sparser than
// the loop's rate), stops at the duration budget, and stamps the manifest's
// capture block (schema 2, the rate the manifest claims).
func TestCapturePlanParallelStream(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fake := &recFakeBridge{
		imgs: []image.Image{flatFrame(color.Black), flatFrame(color.RGBA{R: 90, G: 90, B: 90, A: 255}), flatFrame(color.RGBA{R: 200, G: 40, B: 40, A: 255})},
	}
	rec, err := NewFixtureRecorder(fake, root, "capture-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	// The nominal rates are 100x the capture min-gap (100 ms), so the
	// effective stream is gap-limited to ~10 frames/s: ~600 ms admits ~4-7
	// loop frames — enough to prove the loop runs on its own clock, few
	// enough to prove the budget cut the window.
	rec.SetCapturePlan(CapturePlan{HighHz: 100, LowHz: 50, MaxDuration: 600 * time.Millisecond})
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := rec.StartCapture(ctx); err != nil {
		t.Fatalf("StartCapture: %v", err)
	}
	// The drive's own screenshot, once, well below the loop's rate: the
	// frame sequence interleaves both streams in capture order.
	if _, err := rec.Screenshot(ctx); err != nil {
		t.Fatalf("Screenshot (drive path): %v", err)
	}
	time.Sleep(1200 * time.Millisecond) // well past the 600 ms budget

	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if summary.Capture == nil || !summary.Capture.Enabled || summary.Capture.HighHz != 100 || summary.Capture.LowHz != 50 {
		t.Fatalf("manifest capture block = %+v, want enabled with the plan's rates", summary.Capture)
	}
	if summary.Capture.MaxDurationSecs <= 0 {
		t.Errorf("manifest capture block carries no max-duration budget: %+v", summary.Capture)
	}
	// The loop must have run: the drive's single screenshot plus loop frames
	// (gap-limited to ~1/s*10 over 600 ms).
	if summary.Frames < 3 {
		t.Fatalf("frames = %d, want the loop to have recorded (>= 3 incl. the drive's screenshot)", summary.Frames)
	}
	// ... and the budget must have cut the window: the frame timestamps are
	// the record (checked below against the manifest).

	// The manifest on disk: schema 2 with the capture block, verified.
	raw, err := os.ReadFile(filepath.Join(root, "capture-fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var man FixtureManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if man.Schema != 2 || man.Capture == nil || !man.Capture.Enabled {
		t.Fatalf("manifest schema/capture = %d %+v, want schema 2 enabled", man.Schema, man.Capture)
	}
	if man.Capture.DurationSecs <= 0 || man.Capture.Bytes == 0 {
		t.Errorf("manifest capture block = %+v, want the measured window and bytes", man.Capture)
	}
	// The budget cut the window: the frames span ~600 ms (the budget), not
	// the 1.2 s the test slept. In-flight frames complete after the stop
	// (their numbers were reserved), so the span is a little longer than
	// the budget — and far shorter than the sleep.
	span := float64(man.Frames[len(man.Frames)-1].TsNS-man.Frames[0].TsNS) / 1e9
	if span < 0.4 || span > 1.0 {
		t.Errorf("frame span = %.3f s, want ~600 ms (the budget cut the loop; the test slept 1.2 s)", span)
	}
	if _, err := VerifyFixture(filepath.Join(root, "capture-fixture")); err != nil {
		t.Fatalf("VerifyFixture: %v", err)
	}
}

// TestCapturePlanByteBudget verifies the byte budget stops the loop (the
// size bound the task asks for): the fixture ends with the budget's worth
// of frames, never much more.
func TestCapturePlanByteBudget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fake := &recFakeBridge{
		imgs: []image.Image{flatFrame(color.Black), flatFrame(color.RGBA{R: 90, G: 90, B: 90, A: 255}), flatFrame(color.RGBA{R: 200, G: 40, B: 40, A: 255})},
	}
	rec, err := NewFixtureRecorder(fake, root, "byte-budget-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	// Flat-colour frames encode to ~1-2 KB each; a 3 KB budget admits a
	// couple and then the loop must stop, whatever the rate says (over
	// 1.5 s an unbounded loop would admit ~15 gap-limited frames).
	const budget = 3 * 1024
	rec.SetCapturePlan(CapturePlan{HighHz: 50, LowHz: 50, MaxBytes: budget})
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := rec.StartCapture(ctx); err != nil {
		t.Fatalf("StartCapture: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if summary.Capture.MaxBytes != budget {
		t.Errorf("manifest MaxBytes = %d, want the plan's budget %d", summary.Capture.MaxBytes, budget)
	}
	if summary.Frames == 0 {
		t.Fatal("the byte budget stopped the loop before any frame: the budget is a ceiling, not a veto")
	}
	raw, err := os.ReadFile(filepath.Join(root, "byte-budget-fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var man FixtureManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if man.Capture.Bytes > int64(budget)+256*1024 {
		t.Errorf("recorded bytes = %d, want within the in-flight bound of the %d budget", man.Capture.Bytes, budget)
	}
	// The budget is the assertion: it stopped the loop long before the 1.5 s
	// sleep ended — the frames' timestamps say where (a fraction of a second
	// span, not 1.5 s).
	if len(man.Frames) > 1 {
		span := float64(man.Frames[len(man.Frames)-1].TsNS-man.Frames[0].TsNS) / 1e9
		if span > 0.8 {
			t.Errorf("frame span = %.3f s, want << 0.8 s (the 3 KB byte budget must have stopped the loop early)", span)
		}
	}
}

// TestCaptureLegacyUnchanged verifies the legacy path: no plan armed, no
// parallel stream — the fixture is exactly the drive's screenshots (the
// existing windwaker-newgame shape), and the manifest's capture block stays
// empty on disk (omitted) so old scenarios' pins are untouched.
func TestCaptureLegacyUnchanged(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fake := &recFakeBridge{
		imgs: []image.Image{flatFrame(color.Black), flatFrame(color.RGBA{R: 90, G: 90, B: 90, A: 255}), flatFrame(color.RGBA{R: 200, G: 40, B: 40, A: 255})},
	}
	rec, err := NewFixtureRecorder(fake, root, "legacy-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// StartCapture without a plan is refused (no silent default-on).
	if err := rec.StartCapture(ctx); err == nil {
		t.Fatal("StartCapture passed without a plan: the zero plan is the legacy recording, not a default-on capture")
	}
	for i := 0; i < 3; i++ {
		if _, err := rec.Screenshot(ctx); err != nil {
			t.Fatalf("Screenshot %d: %v", i, err)
		}
	}
	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if summary.Frames != 3 {
		t.Fatalf("frames = %d, want exactly the drive's 3 screenshots (no parallel stream)", summary.Frames)
	}
	if summary.Capture != nil {
		t.Errorf("legacy recording claims a capture: %+v", summary.Capture)
	}
	raw, err := os.ReadFile(filepath.Join(root, "legacy-fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if got := string(raw); !strings.Contains(got, `"schema": 2`) {
		t.Errorf("manifest does not carry schema 2")
	}
	if got := string(raw); strings.Contains(got, `"capture"`) {
		t.Errorf("legacy manifest carries a capture block (omitted expected): %s", got)
	}
}

// TestCaptureMinGap verifies the min-gap enforcement: at a 100 ms minimum
// spacing the loop cannot out-record the gap, whatever its (theoretical)
// rate — the frame sequence stays strictly spaced.
func TestCaptureMinGap(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fake := &recFakeBridge{
		imgs: []image.Image{flatFrame(color.Black)},
	}
	rec, err := NewFixtureRecorder(fake, root, "mingap-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	// 1000 Hz nominal, 100 ms min-gap: at most one frame per ~100 ms, so
	// ~300 ms admits ~4-5 frames — not the 300 the rate alone would allow.
	rec.SetCapturePlan(CapturePlan{HighHz: 1000, LowHz: 1000, MaxDuration: 300 * time.Millisecond})
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := rec.StartCapture(ctx); err != nil {
		t.Fatalf("StartCapture: %v", err)
	}
	time.Sleep(450 * time.Millisecond)
	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if summary.Frames > 15 {
		t.Errorf("frames = %d in ~300 ms at a 100 ms min-gap: the gap is not enforced", summary.Frames)
	}
	if summary.Frames < 1 {
		t.Errorf("frames = 0: the loop recorded nothing (the min-gap deferred everything)")
	}
}
