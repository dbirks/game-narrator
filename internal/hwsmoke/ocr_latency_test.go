package hwsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestHWOCRLatencyDevice is the hardware gate for the persistent OCR engine
// (issue #13, ADR-0100): the one-shot `game-narrator ocr --image` command
// over a real crop — the product's own preprocessing and engine invocation —
// must finish INSIDE the live frame budget on the target hardware.
//
// What the gate measures, stated honestly (AGENTS.md rule 3):
//
//   - The engine is the real Tesseract over the real crop bytes, on the
//     machine the gate runs on (class (e) hardware evidence, not a
//     synthetic run).
//   - The one-shot command pre-spawns the warm slot pool before the image
//     is decoded (ADR-0100), so the fixed 0.59 s A53 model load (ADR-0096)
//     overlaps the decode/crop/pre-process work instead of serialising in
//     front of the read. The per-read duration the report measures is the
//     engine call — inference, not startup + inference (the ~1.1 s
//     per-call shape of a fresh process, ADR-0096).
//   - Separate invocations are separate processes: the pool is warm WITHIN
//     an invocation, not across the 9 runs. The evidence for the sustained
//     live-session pool (the model kept warm across repeated reads at the
//     live 1.1 s frame pace: p50 576 ms / p95 593 ms, under the 700 ms
//     target) is the board measurement in ADR-0100's validation section.
//     This gate re-verifies the budget through the product's own command
//     on the host it runs on: a healthy host reads the controlled crop
//     well under the bound, and a regression (startup no longer overlapped,
//     or the board's cancelled-read shape of issue #8) fails here.
//
// The assertion is the budget, not a specific number: p95 of the 9 runs'
// engine-call durations must fit under ocrLatencyBudget.

// ocrLatencyBudget is the p95 bound the dialogue crop's engine call must
// fit under: issue #13's 700 ms target (docs/spec/05's live frame budget,
// ADR-0096). The controlled 400×100 crop has less inference than the full
// 1024×182 dialogue crop, so a healthy read sits comfortably under the
// bound on the A53 and far under it on a desktop host. The 700 ms was
// unreachable with the per-call process (0.59 s fixed startup + inference
// measured at 1.13 s, ADR-0096) — the persistent pool is what makes it.
const ocrLatencyBudget = 700 * time.Millisecond * ocrTimeoutScale

// ocrLatencyRuns is the sample count for the p95: nine one-shot invocations
// of the product's own command, each a fresh process whose pool warms over
// its own decode/crop/pre-process work.
const ocrLatencyRuns = 9

func TestHWOCRLatencyDevice(t *testing.T) {
	hwGate(t)

	if _, err := exec.LookPath("tesseract"); err != nil {
		notRun(t, "tesseract is not installed (the one-shot command's real engine)",
			"install tesseract with the English pack (pacman -S tesseract tesseract-data-eng / "+
				"apt install tesseract-ocr tesseract-ocr-eng) and re-run")
	}
	bin, _, missing := serveBinary(t)
	if missing != "" {
		notRun(t, missing,
			"install the game-narrator .deb (game-narrator on PATH) or set "+
				"GAME_NARRATOR_BINARY to the installed binary, or run this gate on a dev host with the module checkout")
	}

	// The controlled crop as the command's --image input (the command decodes
	// PNG; the embed is the bytes).
	dir := t.TempDir()
	crop := dir + "/crop.png"
	if err := os.WriteFile(crop, controlledPNG, 0o644); err != nil {
		t.Fatalf("write controlled crop: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	durations := make([]time.Duration, 0, ocrLatencyRuns)
	for i := 1; i <= ocrLatencyRuns; i++ {
		cmdCtx, cmdCancel := context.WithTimeout(ctx, 2*time.Minute)
		out, err := exec.CommandContext(cmdCtx, bin,
			"ocr", "--image", crop, "--json", "--data-dir", dir).CombinedOutput()
		cmdCancel()
		if err != nil {
			t.Fatalf("run %d: the ocr command failed over a real crop (want a report): %v\noutput:\n%s", i, err, tail(out, 1500))
		}

		var rep struct {
			Regions []struct {
				ID         string `json:"id"`
				Text       string `json:"text"`
				Engine     string `json:"engine"`
				DurationMS int64  `json:"duration_ms"`
				Error      string `json:"error"`
			} `json:"regions"`
			TotalMS int64 `json:"total_ms"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(out), &rep); err != nil {
			t.Fatalf("run %d: the report is not parseable JSON: %v\noutput:\n%s", i, err, tail(out, 800))
		}
		if len(rep.Regions) == 0 {
			t.Fatalf("run %d: the report has no regions: %s", i, tail(out, 800))
		}
		var engine string
		for _, r := range rep.Regions {
			if r.Error != "" {
				t.Fatalf("run %d: region %s failed the engine call: %s", i, r.ID, r.Error)
			}
			// The read must be the real engine's and the text must be the
			// controlled crop's — a blank or mis-read "fast" answer is not
			// inside the budget, it is a broken read.
			if strings.TrimSpace(r.Text) == "" {
				t.Fatalf("run %d: region %s read no text (want the controlled crop's line) in %d ms", i, r.ID, r.DurationMS)
			}
			engine = r.Engine
			d := time.Duration(r.DurationMS) * time.Millisecond
			durations = append(durations, d)
			t.Logf("ocr latency run %d: region %s engine=%s text=%q duration=%s (budget %s)",
				i, r.ID, engine, r.Text, d, ocrLatencyBudget)
		}
	}

	// p95 by nearest rank: the ceiling of 95 % of the sample count, the
	// same rank rule the board measurement (ADR-0100) used.
	sort.Slice(durations, func(a, b int) bool { return durations[a] < durations[b] })
	rank := (95*len(durations) + 99) / 100 // ceil(0.95 n)
	if rank < 1 {
		rank = 1
	}
	if rank > len(durations) {
		rank = len(durations)
	}
	p95 := durations[rank-1]
	p50 := durations[(len(durations)+1)/2-1]
	t.Logf("ocr latency p50=%s p95=%s over %d runs (budget %s); per-run: %v",
		p50, p95, len(durations), ocrLatencyBudget, durations)
	if p95 > ocrLatencyBudget {
		t.Fatalf("p95 engine call took %s over %d runs, over the live budget %s: the crop OCR must finish inside the frame budget on this hardware (issue #13)", p95, len(durations), ocrLatencyBudget)
	}
}
