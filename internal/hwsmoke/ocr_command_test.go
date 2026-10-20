package hwsmoke

// TestHWOCRCommandDevice is the hardware half of issue #1: the one-shot
// `game-narrator ocr --device` command over a real UVC capture node — the
// product's own pipeline (the capture stack's format negotiation, the
// pipeline's crop and preprocessing, the real Tesseract, and the report)
// over one live frame.
//
// This is class (e) real-hardware evidence, not a synthetic run: the frame
// comes from the UVC card through the same stack.Resolve + source construction
// a v4l2 session uses, and the read is the real Tesseract engine (no stubs).
// The binary is the installed one or a build of this module (serveBinary),
// so the test exercises exactly what the operator runs.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture/devices"
)

// ocrCommandBudget bounds the whole command (grab + one engine call per
// region): the command's own 2-minute deadline sits inside it.
const ocrCommandBudget = 4 * time.Minute

func TestHWOCRCommandDevice(t *testing.T) {
	hwGate(t)

	if _, err := exec.LookPath("tesseract"); err != nil {
		notRun(t, "tesseract is not installed (the one-shot command's real engine)",
			"install tesseract with the English pack (pacman -S tesseract tesseract-data-eng / "+
				"apt install tesseract-ocr tesseract-ocr-eng) and re-run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), ocrCommandBudget)
	defer cancel()

	// The device: the same selection rule as the capture smoke (docs/spec/10) —
	// GAME_NARRATOR_CAPTURE_DEVICE when set, else the first capture-capable node.
	devs, err := devices.List(ctx)
	if err != nil {
		if errors.Is(err, devices.ErrToolUnavailable) {
			notRun(t, "v4l2-ctl is not installed",
				"install v4l-utils (pacman -S v4l-utils / apt install v4l-utils) and re-run")
		}
		t.Fatalf("v4l2 enumeration failed: %v", err)
	}
	if len(devs) == 0 {
		notRun(t, "no V4L2 capture device found",
			"attach the UVC HDMI capture dongle through the USB hub (docs/spec/10 physical "+
				"connection) and re-run make test-hardware")
	}
	dev, why, missing := selectDevice(devs)
	if missing != "" {
		notRun(t, missing, howToFixCapture(missing))
	}
	t.Logf("ocr command: selected device %s (selection: %s)", dev.Path, why)

	// The binary the operator runs: the installed one, or a build of this
	// module (the same resolution the budget session uses).
	bin, _, missingBin := serveBinary(t)
	if missingBin != "" {
		notRun(t, missingBin,
			"install the game-narrator .deb (game-narrator on PATH) or set "+
				"GAME_NARRATOR_BINARY to the installed binary, or run this gate on a dev host with the module checkout")
	}
	t.Logf("ocr command: binary %s", bin)

	dataDir := t.TempDir()
	cmdCtx, cmdCancel := context.WithTimeout(ctx, ocrCommandBudget)
	defer cmdCancel()
	out, err := exec.CommandContext(cmdCtx, bin,
		"ocr", "--device", dev.Path, "--json", "--data-dir", dataDir).CombinedOutput()
	if err != nil {
		t.Fatalf("the ocr command failed (want a report over one live frame): %v\noutput:\n%s", err, tail(out, 1500))
	}

	// The report: the provenance line names the device, the frame is real
	// (non-zero dimensions from the card) and at least one region was read
	// by the real engine without an error.
	var rep struct {
		Source  string `json:"source"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
		Regions []struct {
			ID     string `json:"id"`
			Empty  bool   `json:"empty"`
			Text   string `json:"text"`
			Engine string `json:"engine"`
			Error  string `json:"error"`
		} `json:"regions"`
		TotalMS int64 `json:"total_ms"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &rep); err != nil {
		t.Fatalf("the ocr command's --json output is not valid JSON: %v\n%s", err, tail(out, 1500))
	}
	if !strings.HasPrefix(rep.Source, "device:") {
		t.Fatalf("source = %q, want the device provenance line", rep.Source)
	}
	if rep.Width <= 0 || rep.Height <= 0 {
		t.Fatalf("frame = %dx%d, want real dimensions from the capture card", rep.Width, rep.Height)
	}
	if len(rep.Regions) == 0 {
		t.Fatalf("regions = 0, want at least the fallback whole-frame region")
	}
	read := 0
	for _, r := range rep.Regions {
		if r.Error != "" {
			t.Fatalf("region %s errored: %s", r.ID, r.Error)
		}
		if r.Empty {
			continue
		}
		if r.Engine != "tesseract" {
			t.Fatalf("region %s engine = %q, want the real tesseract engine", r.ID, r.Engine)
		}
		read++
		t.Logf("region %s read %q (engine %s)", r.ID, r.Text, r.Engine)
	}
	if read == 0 {
		t.Fatal("no region produced a read over the live frame")
	}
	t.Logf("ocr command: %s — %dx%d frame, %d region(s) read in %d ms",
		rep.Source, rep.Width, rep.Height, read, rep.TotalMS)
}
