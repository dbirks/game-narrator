package pipeline

// Cost measurement (ADR-0172, issue #53): the panel finder's per-frame
// cost on the host that ships it. The scan is the whole per-frame work the
// dynamic region adds when it runs: FindPanelsImage is panelLuma (the
// downsample to the fixed scan size) plus the pure FindPanels row scan.
//
// The UNO Q is the target appliance (CPU-bound, ~7.5 fps). The bench below
// measures the FULL frame path on the dev host; the REPORT scales it by the
// task's ~4× CPU factor to project the UNO Q cost. The benchmark runs at the
// finder's default scan cadence (every N frames), not every frame, so the
// amortised per-frame cost is even lower.

import (
	"image"
	"image/color"
	"testing"
)

// busyFrame builds a 640×480 RGBA with a mid-gray scene and a dark bottom
// dialogue panel carrying bright text — the shape the finder exists to
// catch, at the resolution the emulator frame source delivers.
func busyFrame() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			c := color.Gray{128}
			if y >= 350 && y < 430 && x >= 32 && x < 608 {
				c = color.Gray{20}
				if (x-32)%8 >= 5 && (x-32)%8 < 7 {
					c = color.Gray{240}
				}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// busyFrameNoPanel is the same scene with no panel — the frame the finder
// runs on most often, and the one whose cost bounds the amortised path.
func busyFrameNoPanel() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.Gray{128})
		}
	}
	return img
}

func benchFrame(b *testing.B, img image.Image, cfg PanelFindConfig) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FindPanelsImage(img, cfg)
	}
}

// BenchmarkFindPanelsFrameWithPanel is the full downsample+scan cost on a
// 640×480 frame that contains a panel.
func BenchmarkFindPanelsFrameWithPanel(b *testing.B) {
	img := busyFrame()
	cfg := DefaultPanelFind()
	cfg.Enabled = true
	benchFrame(b, img, cfg)
}

// BenchmarkFindPanelsFrameNoPanel is the same cost on a panel-less frame —
// the common case where the configured region owns the box.
func BenchmarkFindPanelsFrameNoPanel(b *testing.B) {
	img := busyFrameNoPanel()
	cfg := DefaultPanelFind()
	cfg.Enabled = true
	benchFrame(b, img, cfg)
}
