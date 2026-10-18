package dolphin

// record_test.go is the unit half of the fixture recorder: the Bridge
// wrapper's bookkeeping (frames, input log, manifest, hashes, the NOT-RUN
// verification gate) against a fake bridge — no emulator, no image, no
// Tesseract (the real end-to-end proof is TestRealRecordFixture).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// recFakeBridge is the fake Bridge the recorder wraps in unit tests: it
// serves flat frames (the classifier measures whatever they are) and counts
// its inputs.
type recFakeBridge struct {
	imgs          []image.Image
	n             int
	inputs        []string
	screenshotErr error
}

func (f *recFakeBridge) Launch(ctx context.Context, spec LaunchSpec) error { return nil }
func (f *recFakeBridge) Inject(ctx context.Context, b Buttons, a Axes, hold time.Duration) error {
	f.inputs = append(f.inputs, injectWire(b, a))
	return nil
}
func (f *recFakeBridge) Screenshot(ctx context.Context) (image.Image, error) {
	if f.screenshotErr != nil {
		return nil, f.screenshotErr
	}
	img := f.imgs[f.n%len(f.imgs)]
	f.n++
	return img, nil
}
func (f *recFakeBridge) FrameSource(fps int) (capture.Source, error) {
	return nil, errors.New("rec fake: unused")
}
func (f *recFakeBridge) SaveSnapshot(label string) (string, error) { return "", nil }
func (f *recFakeBridge) WaitForGameFrame(ctx context.Context, timeout time.Duration) (Evidence, error) {
	return Evidence{}, nil
}
func (f *recFakeBridge) ProveInput(ctx context.Context) (Evidence, error) { return Evidence{}, nil }
func (f *recFakeBridge) SaveState(ctx context.Context, slot int) (string, error) {
	return "", ErrHotkeysDisabled
}
func (f *recFakeBridge) LoadState(ctx context.Context, slot int) error { return ErrHotkeysDisabled }
func (f *recFakeBridge) State() Status {
	return Status{
		ImageSHA:   "img-sha-123",
		Display:    ":99",
		UserDir:    "/tmp/fake-user-dir",
		DolphinPID: 42,
		Region:     Region{W: 640, H: 480},
	}
}
func (f *recFakeBridge) Stop() {}

// flatFrame is one 640×480 flat frame of the given colour: enough pixels for
// the classifier's measurements, no content the test must reason about.
func flatFrame(c color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestFixtureRecorderUnit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	base := color.RGBA{R: 40, G: 40, B: 40, A: 255}
	fake := &recFakeBridge{
		imgs: []image.Image{
			flatFrame(base),
			flatFrame(color.RGBA{R: 120, G: 60, B: 30, A: 255}),
			flatFrame(color.RGBA{R: 20, G: 200, B: 90, A: 255}),
		},
	}
	rec, err := NewFixtureRecorder(fake, root, "unit-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := rec.Screenshot(ctx); err != nil {
			t.Fatalf("Screenshot %d: %v", i, err)
		}
	}
	if err := rec.Inject(ctx, Buttons{A: true, Start: true}, Axes{}, 200*time.Millisecond); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := rec.Inject(ctx, Buttons{}, Axes{MainX: -1}, 900*time.Millisecond); err != nil {
		t.Fatalf("Inject (stick): %v", err)
	}

	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if summary.ID != "unit-fixture" || summary.Frames != 3 || summary.Lines != 0 {
		t.Fatalf("summary = %+v, want id unit-fixture, 3 frames, 0 lines", summary)
	}
	if summary.ManifestSHA256 == "" {
		t.Fatal("summary carries no manifest sha256")
	}

	// The layout: frames/, manifest.json, truth.json, profile.json.
	for _, rel := range []string{"frames/frame-00001.png", "frames/frame-00002.png", "frames/frame-00003.png", "manifest.json", "truth.json", "profile.json"} {
		if _, err := os.Stat(filepath.Join(root, "unit-fixture", rel)); err != nil {
			t.Fatalf("missing fixture file %s: %v", rel, err)
		}
	}

	// The manifest: the hashes, the input log, the image SHA (and only the
	// image SHA — the path itself must never be written).
	raw, err := os.ReadFile(filepath.Join(root, "unit-fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	// The image identity is the SHA only: the user-supplied image path never
	// lands in the manifest (AGENTS.md rule 5).
	if strings.Contains(string(raw), EnvGameImage) {
		t.Fatalf("manifest names the image env/path: %s", raw)
	}
	var man FixtureManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if man.ID != "unit-fixture" || man.Origin != "dolphin" || man.Schema != FixtureSchema {
		t.Fatalf("manifest identity = %+v", man)
	}
	if man.GameImageSHA256 != "img-sha-123" {
		t.Fatalf("manifest image sha = %q, want the recorded img-sha-123", man.GameImageSHA256)
	}
	if len(man.Frames) != 3 {
		t.Fatalf("manifest frames = %d, want 3", len(man.Frames))
	}
	for i, fr := range man.Frames {
		wantName := fmt.Sprintf("frame-%05d.png", i+1)
		if fr.File != wantName {
			t.Fatalf("frame %d: manifest names %q, want %q (the recorder's frame naming is the report's link)", i+1, fr.File, wantName)
		}
		sha, err := sha256File(filepath.Join(root, "unit-fixture", "frames", fr.File))
		if err != nil {
			t.Fatalf("frame %s: %v", fr.File, err)
		}
		if sha != fr.SHA256 {
			t.Fatalf("frame %s: manifest hash %s, actual %s", fr.File, fr.SHA256, sha)
		}
		if fr.TsNS <= 0 {
			t.Fatalf("frame %s: no capture timestamp", fr.File)
		}
	}
	if len(man.Inputs) != 2 || man.Inputs[0].Wire != "A+START" || man.Inputs[1].Wire != "STICK(x=-1.0 y=0.0)" {
		t.Fatalf("input log = %+v, want the two recorded injects with their wire descriptions", man.Inputs)
	}
	if man.Inputs[0].Frame != 2 || man.Inputs[1].Frame != 2 {
		t.Fatalf("input frame markers = [%d %d], want 2 (both landed after the 3rd frame)", man.Inputs[0].Frame, man.Inputs[1].Frame)
	}
	if man.Oracle.Reason != "" || len(man.Oracle.Transitions) != 0 {
		t.Fatalf("oracle = %+v, want empty (the unit run drove no trace)", man.Oracle)
	}

	// The truth: three rows, all absent (no frame measured the subtitle bar).
	truthRaw, err := os.ReadFile(filepath.Join(root, "unit-fixture", "truth.json"))
	if err != nil {
		t.Fatalf("truth: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(truthRaw, &rows); err != nil {
		t.Fatalf("truth parse: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("truth rows = %d, want 3", len(rows))
	}
	for i, row := range rows {
		if row["box_present"] != false || row["text"] != "" {
			t.Fatalf("truth row %d = %+v, want absent and textless", i, row)
		}
	}

	// The profile: zero messages (the unit frames measured no subtitle text).
	profRaw, err := os.ReadFile(filepath.Join(root, "unit-fixture", "profile.json"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	var prof map[string]any
	if err := json.Unmarshal(profRaw, &prof); err != nil {
		t.Fatalf("profile parse: %v", err)
	}
	if msgs, _ := prof["messages"].([]any); len(msgs) != 0 {
		t.Fatalf("profile messages = %v, want none", msgs)
	}

	// The manifest's sha256: the summary's value is the file's real hash.
	sha, err := sha256File(filepath.Join(root, "unit-fixture", "manifest.json"))
	if err != nil {
		t.Fatalf("manifest sha: %v", err)
	}
	if sha != summary.ManifestSHA256 {
		t.Fatalf("manifest sha %s != summary %s", sha, summary.ManifestSHA256)
	}

	// VerifyFixture passes on the fresh fixture and fails on a tampered frame.
	if _, err := VerifyFixture(filepath.Join(root, "unit-fixture")); err != nil {
		t.Fatalf("VerifyFixture (fresh): %v", err)
	}
	framePath := filepath.Join(root, "unit-fixture", "frames", "frame-00001.png")
	rawFrame, err := os.ReadFile(framePath)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if err := os.WriteFile(framePath, append(rawFrame, 'x'), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := VerifyFixture(filepath.Join(root, "unit-fixture")); err == nil {
		t.Fatal("VerifyFixture passed on a tampered frame: the hash gate is open")
	} else if !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tamper error = %v, want the hash mismatch named", err)
	}
}

func TestVerifyFixtureMissingDir(t *testing.T) {
	if _, err := VerifyFixture(filepath.Join(t.TempDir(), "no-such-fixture")); err == nil {
		t.Fatal("VerifyFixture passed on a missing dir: the NOT RUN gate is open")
	}
}

func TestNewFixtureRecorderRejectsBadID(t *testing.T) {
	for _, id := range []string{"", "a/b", "..", "x\x00y"} {
		if _, err := NewFixtureRecorder(&recFakeBridge{imgs: []image.Image{flatFrame(color.White)}}, t.TempDir(), id); err == nil {
			t.Fatalf("NewFixtureRecorder accepted fixture id %q", id)
		}
	}
}

func TestFixtureRecorderScreenshotFailure(t *testing.T) {
	fake := &recFakeBridge{
		imgs:          []image.Image{flatFrame(color.White)},
		screenshotErr: errors.New("grab failed"),
	}
	rec, err := NewFixtureRecorder(fake, t.TempDir(), "fail-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	if _, err := rec.Screenshot(context.Background()); err == nil {
		t.Fatal("Screenshot returned no error: the failure must propagate (a fixture missing a frame is a lie)")
	}
	if _, err := rec.Finalize(context.Background(), DriveTrace{}); err == nil {
		t.Fatal("Finalize passed with zero frames: an empty fixture must not be handed back")
	}
}

func TestInjectWire(t *testing.T) {
	cases := []struct {
		b    Buttons
		a    Axes
		want string
	}{
		{Buttons{A: true}, Axes{}, "A"},
		{Buttons{A: true, Start: true, Right: true}, Axes{}, "A+START+D_RIGHT"},
		{Buttons{}, Axes{MainX: -1, MainY: 0}, "STICK(x=-1.0 y=0.0)"},
		{Buttons{Start: true}, Axes{MainX: 1}, "START+STICK(x=1.0 y=0.0)"},
		{Buttons{}, Axes{}, ""},
	}
	for i, c := range cases {
		if got := injectWire(c.b, c.a); got != c.want {
			t.Fatalf("case %d: injectWire = %q, want %q", i, got, c.want)
		}
	}
}

// TestFixtureRecorderPNGRoundTrip pins that the recorded frames are lossless
// PNG: the manifest's hash covers exactly the bytes on disk.
func TestFixtureRecorderPNGRoundTrip(t *testing.T) {
	img := flatFrame(color.NRGBA{R: 10, G: 200, B: 30, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	if got := hex.EncodeToString(sum[:]); len(got) != 64 {
		t.Fatalf("sha256 hex = %d chars, want 64", len(got))
	}
}
