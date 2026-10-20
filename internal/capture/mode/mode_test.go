package mode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/capture/devices"
)

// loadFixture parses a recorded/synthetic --list-formats-ext fixture through
// the real parser, so the selection code is tested on the exact shape v4l2-ctl
// produces, not on hand-built structs.
func loadFixture(t *testing.T, name string) []devices.Format {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	f := devices.ParseFormatsExt(string(b))
	if len(f) == 0 {
		t.Fatalf("fixture %s parsed no formats", name)
	}
	return f
}

func TestFixturesParse(t *testing.T) {
	ms2130 := loadFixture(t, "ms2130-usb3-formats.txt")
	if len(ms2130) != 2 {
		t.Fatalf("ms2130 fixture: %d formats, want 2", len(ms2130))
	}
	if ms2130[0].PixFmt != "yuyv422" || len(ms2130[0].Sizes) != 3 {
		t.Errorf("ms2130 yuyv: %+v", ms2130[0])
	}
	if !ms2130[0].Sizes[1].Stepwise || ms2130[0].Sizes[1].Min != 30 || ms2130[0].Sizes[1].Max != 60 {
		t.Errorf("ms2130 720p stepwise not parsed: %+v", ms2130[0].Sizes[1])
	}
	ms2109 := loadFixture(t, "ms2109-usb2-formats.txt")
	if len(ms2109) != 1 || ms2109[0].PixFmt != "mjpeg" || len(ms2109[0].Sizes) != 3 {
		t.Fatalf("ms2109 fixture: %+v", ms2109)
	}
}

// TestSelectPolicyTable is the policy's contract: every row is a real preflight
// situation the doctor will face, and the chosen mode plus the reason the
// operator is shown.
func TestSelectPolicyTable(t *testing.T) {
	ms2130 := loadFixture(t, "ms2130-usb3-formats.txt")
	ms2109 := loadFixture(t, "ms2109-usb2-formats.txt")
	pol := Policy{GameW: 640, GameH: 480, AnalysisFPS: 10}

	for _, tc := range []struct {
		name    string
		formats []devices.Format
		pol     Policy
		wantW   int
		wantH   int
		wantFPS float64
		wantFmt string
		wantErr bool
		wantSub []string
	}{
		{"ms2130 superspeed picks the native canvas in raw", ms2130, Policy{GameW: 640, GameH: 480, AnalysisFPS: 10, Link: LinkSuperSpeed}, 640, 480, 30, "yuyv422", false,
			[]string{"640x480", "YUYV", "SuperSpeed"}},
		{"ms2130 superspeed 720p game prefers yuyv over mjpeg", ms2130, Policy{GameW: 1280, GameH: 720, AnalysisFPS: 10, Link: LinkSuperSpeed, DecodeBudgetPPS: 15_000_000}, 1280, 720, 30, "yuyv422", false,
			[]string{"YUYV preferred"}},
		{"ms2130 highspeed 720p game has no feasible mode", ms2130, Policy{GameW: 1280, GameH: 720, AnalysisFPS: 10, Link: LinkHighSpeed}, 0, 0, 0, "", true, nil},
		{"ms2109 highspeed picks 480p mjpeg at the analysis rate", ms2109, pol, 640, 480, 10, "mjpeg", false,
			[]string{"stepwise", "decode budget"}},
		{"ms2109 superspeed still takes its only format", ms2109, Policy{GameW: 640, GameH: 480, AnalysisFPS: 10, Link: LinkSuperSpeed}, 640, 480, 10, "mjpeg", false, nil},
		{"ms2109 highspeed 720p rejected by budget until the budget is raised", ms2109, Policy{GameW: 1280, GameH: 720, AnalysisFPS: 10, DecodeBudgetPPS: 10_000_000}, 1280, 720, 10, "mjpeg", false, nil},
		{"ms2130 superspeed 1080p game runs raw at the link ceiling", ms2130, Policy{GameW: 1920, GameH: 1080, AnalysisFPS: 10, Link: LinkSuperSpeed}, 1920, 1080, 30, "yuyv422", false, nil},
		{"ms2130 highspeed 1080p game has no feasible mode at the default budget", ms2130, Policy{GameW: 1920, GameH: 1080, AnalysisFPS: 10, Link: LinkHighSpeed}, 0, 0, 0, "", true, nil},
		{"ms2130 highspeed 1080p game admits 1080p10 mjpeg once the budget is measured up", ms2130, Policy{GameW: 1920, GameH: 1080, AnalysisFPS: 10, Link: LinkHighSpeed, DecodeBudgetPPS: 25_000_000}, 1920, 1080, 10, "mjpeg", false, nil},
		{"fifteen hertz analysis picks fifteen inside the stepwise interval", ms2109, Policy{GameW: 640, GameH: 480, AnalysisFPS: 15}, 640, 480, 15, "mjpeg", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := Select(tc.formats, tc.pol)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Select = %v, want an error", sel)
				}
				if !strings.Contains(err.Error(), "no offered mode") && len(tc.formats) > 0 {
					t.Errorf("error %q does not name the empty candidate set", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if got := sel.Mode; got.W != tc.wantW || got.H != tc.wantH || got.FPS != tc.wantFPS || got.PixFmt != tc.wantFmt {
				t.Errorf("mode = %v, want %dx%d %s @ %g", got, tc.wantW, tc.wantH, tc.wantFmt, tc.wantFPS)
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(sel.Reason, sub) {
					t.Errorf("reason %q lacks %q", sel.Reason, sub)
				}
			}
		})
	}
}

func TestSelectRejectsEmptyFormats(t *testing.T) {
	if _, err := Select(nil, DefaultPolicy()); err == nil {
		t.Fatal("an empty format list must error with the preflight guidance")
	}
}

func TestLinkFromSpeed(t *testing.T) {
	for speed, want := range map[int]Link{
		0:     LinkUnknown,
		12:    LinkHighSpeed,
		480:   LinkHighSpeed,
		5000:  LinkSuperSpeed,
		10000: LinkSuperSpeed,
		20000: LinkSuperSpeed,
	} {
		if got := LinkFromSpeed(speed); got != want {
			t.Errorf("LinkFromSpeed(%d) = %v, want %v", speed, got, want)
		}
	}
}

func TestModeString(t *testing.T) {
	m := Mode{W: 1280, H: 720, FPS: 30, PixFmt: "mjpeg"}
	if got := m.String(); got != "1280x720 MJPEG @ 30 fps" {
		t.Errorf("String = %q", got)
	}
}
