// Package mode answers "which mode do we ask the capture card for" — the pure
// half of docs/spec/10's preflight ("Record fps, progressive/interlaced
// status, pixel format"; "negotiate ... e.g. 720p/1080p"). The card's format
// list (devices.ParseFormatsExt from v4l2-ctl --list-formats-ext) and the
// target's policy in; one mode and a human-readable reason out.
//
// The policy is the product decision the ADR (docs/adr/0074) records:
//
//  1. the picture must cover the game's native picture — a card that offers
//     only 320x240 cannot show a 640x480 GameCube canvas, no matter how fast
//     it streams;
//  2. the rate must cover the analysis rate — docs/spec/05 analyses at 5–10
//     Hz, and the *smallest offered* rate at or above it is requested, which
//     is the pre-decode decimation: the card delivers at the analysis rate
//     instead of 60 frames the pipeline drops;
//  3. the format follows the USB link — raw YUYV 1080p60 is ~250 MB/s and
//     only a SuperSpeed link carries it (internal/capture/usb names the
//     fault), while MJPEG is the compressed form that fits a HighSpeed port;
//  4. the mode must fit the CPU decode budget — the A53 appliance's MJPEG
//     decode ceiling is measured on the target (ADR-0076 benchmark), and a
//     mode above it is rejected rather than requested and flapped on.
//
// Select is deliberately pure: no sysfs, no subprocess. The doctor
// (internal/doctor) feeds it real enumeration and the real link speed, and
// the tests feed it the synthetic MS2130/MS2109 fixtures under testdata —
// the same code path either way.
package mode

import (
	"fmt"
	"sort"

	"github.com/dbirks/game-narrator/internal/capture/devices"
	"github.com/dbirks/game-narrator/internal/capture/usb"
)

// Link is the USB link class a mode must be deliverable over.
type Link int

const (
	// LinkUnknown is the conservative default: a link nobody measured is
	// assumed to be at most HighSpeed, because a USB 2 hub is the common
	// failure, not the SuperSpeed one.
	LinkUnknown Link = iota
	// LinkHighSpeed is USB 2: 480 Mb/s.
	LinkHighSpeed
	// LinkSuperSpeed is USB 3.0 or faster.
	LinkSuperSpeed
)

// String names the link for reports.
func (l Link) String() string {
	switch l {
	case LinkHighSpeed:
		return "HighSpeed (USB 2)"
	case LinkSuperSpeed:
		return "SuperSpeed (USB 3)"
	default:
		return "unknown (assumed HighSpeed)"
	}
}

// LinkFromSpeed maps a sysfs speed (Mb/s, internal/capture/usb) to a policy
// link class.
func LinkFromSpeed(speedMBps int) Link {
	switch {
	case speedMBps >= usb.SpeedSuperSpeed:
		return LinkSuperSpeed
	case speedMBps > 0:
		// HighSpeed and the legacy 1.x speeds alike: below the raw-1080p bar.
		return LinkHighSpeed
	default:
		return LinkUnknown
	}
}

// LinkBandwidthMBps is the usable one-way bandwidth a link class carries, in
// MB/s, after protocol overhead (~80% of the nominal rate: the USB 2
// HighSpeed 480 Mb/s ceiling leaves ~48 MB/s of payload — 1080p60 raw YUYV
// at ~250 MB/s does not fit, and that is the number the policy must refuse).
func LinkBandwidthMBps(l Link) float64 {
	switch l {
	case LinkSuperSpeed:
		return 500
	case LinkHighSpeed, LinkUnknown:
		return 48
	default:
		return 48
	}
}

// MaxFPS is the highest rate the policy requests of a card: faster than 60
// fps the pipeline would only drop frames, and a 60 fps ceiling is already
// far above the 5–10 Hz analysis rate docs/spec/05 asks for.
const MaxFPS = 60.0

// DefaultGameW/DefaultGameH are the GameCube's native 4:3 canvas. Profiles
// carry the per-game truth; the policy's default is the class the converter
// chain exists to serve (docs/spec/10).
const (
	DefaultGameW = 640
	DefaultGameH = 480
)

// DefaultAnalysisFPS is docs/spec/05's analysis rate for the live loop.
const DefaultAnalysisFPS = 10.0

// DefaultMJPEGBudgetPPS is the MJPEG decode ceiling in pixels/second that the
// policy asks of the card for the UNO Q's four Cortex-A53 cores. ADR-0076
// measured the target decoding synthetic testsrc2 MJPEG with the appliance's
// real FFmpeg 7.1 (4 cores) at ~140 fps on 1280x720@60 and ~66 fps on
// 1920x1080@30 — about 130 Mpps of sustained decode at roughly one core of
// CPU. The policy asks for far less than the ceiling, because the same four
// cores also run OCR and the speech pipeline: at 8 Mpps the 480p-class
// picture at the 10 Hz analysis rate (3.1 Mpps) fits with wide headroom,
// 720p@10 (9.2 Mpps) is refused, and 1080p@10 (20.7 Mpps) is refused unless
// a higher budget is set from a measured run on the target
// with OCR idling.
const DefaultMJPEGBudgetPPS = 8_000_000.0

// Mode is one concrete capture mode: a size, a rate and a pixel format.
type Mode struct {
	W      int     `json:"w"`
	H      int     `json:"h"`
	FPS    float64 `json:"fps"`
	PixFmt string  `json:"pix_fmt"`
}

// String renders the mode the way a doctor row reads it.
func (m Mode) String() string {
	return fmt.Sprintf("%dx%d %s @ %s fps", m.W, m.H, m.pixFmtName(), fpsString(m.FPS))
}

func (m Mode) pixFmtName() string {
	switch m.PixFmt {
	case "mjpeg":
		return "MJPEG"
	case "yuyv422":
		return "YUYV"
	default:
		return m.PixFmt
	}
}

// fpsString renders a rate without float noise (30 not 30.000001).
func fpsString(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%.3g", f)
}

// Policy is the target's requirements, all with documented defaults so the
// zero value is the documented preflight policy, not a silent mis-selection.
type Policy struct {
	// GameW/GameH is the game's native picture size; 0 uses the GameCube
	// default canvas.
	GameW, GameH int
	// AnalysisFPS is the pipeline's analysis rate; 0 uses DefaultAnalysisFPS.
	AnalysisFPS float64
	// Link is the card's negotiated USB link; LinkUnknown assumes HighSpeed.
	Link Link
	// DecodeBudgetPPS is the MJPEG decode ceiling in pixels/second; 0 uses
	// DefaultMJPEGBudgetPPS. The ADR-0076 benchmark is the number to set it
	// from; raising it without a benchmark is how a mode gets requested that
	// the A53 cannot hold.
	DecodeBudgetPPS float64
}

// DefaultPolicy is the zero-value policy made explicit: GameCube canvas,
// 10 Hz analysis, conservative link, the measured decode budget.
func DefaultPolicy() Policy {
	return Policy{
		GameW:           DefaultGameW,
		GameH:           DefaultGameH,
		AnalysisFPS:     DefaultAnalysisFPS,
		DecodeBudgetPPS: DefaultMJPEGBudgetPPS,
	}
}

func (p Policy) normalize() Policy {
	if p.GameW <= 0 || p.GameH <= 0 {
		p.GameW, p.GameH = DefaultGameW, DefaultGameH
	}
	if p.AnalysisFPS <= 0 {
		p.AnalysisFPS = DefaultAnalysisFPS
	}
	if p.DecodeBudgetPPS <= 0 {
		p.DecodeBudgetPPS = DefaultMJPEGBudgetPPS
	}
	return p
}

// Selection is the policy's answer: the mode, why it was chosen, and how many
// candidates the policy rejected — the count the doctor shows when the
// selection is surprising, so "why 720p and not 1080p" is answerable.
type Selection struct {
	Mode     Mode
	Reason   string
	Rejected int
}

type candidate struct {
	m        Mode
	unsized  bool // the card did not enumerate rates at this size
	rejected bool
	reason   string
}

// Select picks the capture mode from the device's format list. formats is the
// output of devices.ParseFormatsExt; an empty list errors with the guidance
// the doctor needs (the node enumerated no modes — permission or metadata
// node, not a card fault).
func Select(formats []devices.Format, p Policy) (Selection, error) {
	if len(formats) == 0 {
		return Selection{}, fmt.Errorf("mode: the device enumerates no formats (check /dev permissions; docs/spec/10)")
	}
	p = p.normalize()
	var (
		cands []candidate
		rej   []string
	)
	addReject := func(c candidate) {
		if len(rej) < 3 {
			rej = append(rej, fmt.Sprintf("%d %d @ %s fps: %s", c.m.W, c.m.H, c.m.pixFmtName(), c.reason))
		}
		cands = append(cands, c)
	}

	bandwidth := LinkBandwidthMBps(p.Link)
	for _, f := range formats {
		switch f.PixFmt {
		case "mjpeg", "yuyv422":
		default:
			addReject(candidate{m: Mode{PixFmt: f.PixFmt}, rejected: true, reason: "unsupported pixel format"})
			continue
		}
		for _, sz := range f.Sizes {
			if sz.W < p.GameW || sz.H < p.GameH {
				addReject(candidate{m: Mode{W: sz.W, H: sz.H, PixFmt: f.PixFmt}, rejected: true, reason: fmt.Sprintf("smaller than the game's %dx%d picture", p.GameW, p.GameH)})
				continue
			}
			rate, ok, ranged := pickRate(sz, p.AnalysisFPS)
			if !ok {
				addReject(candidate{m: Mode{W: sz.W, H: sz.H, PixFmt: f.PixFmt}, rejected: true,
					reason: fmt.Sprintf("offers no rate in %s–%d fps", fpsString(p.AnalysisFPS), int(MaxFPS))})
				continue
			}
			c := candidate{m: Mode{W: sz.W, H: sz.H, FPS: rate, PixFmt: f.PixFmt}}
			var feas string
			switch f.PixFmt {
			case "yuyv422":
				wire := float64(sz.W*sz.H*2) * rate / 1e6
				if wire > bandwidth {
					addReject(candidate{m: c.m, rejected: true,
						reason: fmt.Sprintf("%s MB/s exceeds the %s link's ~%d MB/s", trim0(wire), p.Link, int64(bandwidth))})
					continue
				}
				feas = fmt.Sprintf("raw %s fits the %s link (%s MB/s ≤ %d MB/s)", c.m.pixFmtName(), p.Link, trim0(wire), int64(bandwidth))
			case "mjpeg":
				pps := float64(sz.W*sz.H) * rate
				if pps > p.DecodeBudgetPPS {
					addReject(candidate{m: c.m, rejected: true,
						reason: fmt.Sprintf("%s Mpps exceeds the decode budget %s Mpps", trim0(pps/1e6), trim0(p.DecodeBudgetPPS/1e6))})
					continue
				}
				feas = fmt.Sprintf("within the MJPEG decode budget (%s ≤ %s Mpps)", trim0(pps/1e6), trim0(p.DecodeBudgetPPS/1e6))
			}
			c.reason = feas
			if ranged {
				c.reason = fmt.Sprintf("%s fps chosen inside the stepwise %s-%s interval the driver advertises; %s",
					fpsString(rate), fpsString(sz.Min), fpsString(sz.Max), c.reason)
			}
			if !ranged && len(sz.FPS) == 0 {
				c.unsized = true
				c.reason += "; rate not enumerated by the driver"
			}
			cands = append(cands, c)
		}
	}
	var ok []candidate
	for _, c := range cands {
		if !c.rejected {
			ok = append(ok, c)
		}
	}
	if len(ok) == 0 {
		detail := "no offered mode fits the policy"
		if len(rej) > 0 {
			detail = fmt.Sprintf("%s (examples: %s)", detail, joinN(rej, "; "))
		}
		return Selection{}, fmt.Errorf("mode: %s", detail)
	}
	sort.Slice(ok, func(i, j int) bool { return rankLess(ok[i], ok[j], p.Link, p.AnalysisFPS) })
	best := ok[0]
	why := fmt.Sprintf("%s: smallest offered size ≥ %dx%d with a rate ≥ %s fps; %s; %s",
		best.m, p.GameW, p.GameH, fpsString(p.AnalysisFPS), fmtPref(p.Link), best.reason)
	return Selection{Mode: best.m, Reason: why, Rejected: len(cands) - len(ok)}, nil
}

// pickRate chooses the rate to request at one size: the smallest offered rate
// at or above the analysis rate and at or below MaxFPS, so the card delivers
// at the analysis rate instead of a higher one the pipeline would only drop
// (the pre-decode decimation). A stepwise size accepts any rate inside its
// advertised interval, so the analysis rate itself is requested when it lies
// in the interval; a discrete size falls back to its enumerated rates; a size
// that enumerates no interval at all is accepted at the analysis rate exactly
// (flagged false/false so the caller marks the rate unverified).
func pickRate(sz devices.Size, analysis float64) (rate float64, ok, ranged bool) {
	if sz.Stepwise && sz.Max >= analysis && sz.Max > 0 {
		lo := sz.Min
		if lo <= 0 {
			lo = 0
		}
		if analysis < lo {
			rate = lo
		} else {
			rate = analysis
		}
		if rate > MaxFPS {
			return 0, false, false
		}
		return rate, true, true
	}
	best := 0.0
	found := false
	for _, r := range sz.FPS {
		if r < analysis || r > MaxFPS {
			continue
		}
		if !found || r < best {
			best, found = r, true
		}
	}
	if found {
		return best, true, false
	}
	if len(sz.FPS) == 0 {
		return analysis, true, false
	}
	return 0, false, false
}

// rankLess orders candidates: picture area first (the "lowest resolution that
// covers the game" rule), then format by link (SuperSpeed prefers YUYV — raw
// is cheaper to decode; HighSpeed/unknown prefer MJPEG — raw will not fit),
// then closeness to the analysis rate (a 30 fps pick beats 60 when both fit).
func rankLess(a, b candidate, link Link, analysis float64) bool {
	aa, ba := a.m.W*a.m.H, b.m.W*b.m.H
	if aa != ba {
		return aa < ba
	}
	if a.m.PixFmt != b.m.PixFmt {
		return formatPrefers(a.m.PixFmt, b.m.PixFmt, link)
	}
	if a.m.FPS != b.m.FPS {
		return dist(a.m.FPS, analysis) < dist(b.m.FPS, analysis)
	}
	return a.m.PixFmt < b.m.PixFmt
}

func formatPrefers(x, y string, link Link) bool {
	prefer := "mjpeg"
	if link == LinkSuperSpeed {
		prefer = "yuyv422"
	}
	if (x == prefer) != (y == prefer) {
		return x == prefer
	}
	return x < y
}

func dist(f, analysis float64) float64 { return f - analysis }

func fmtPref(link Link) string {
	if link == LinkSuperSpeed {
		return "YUYV preferred on a SuperSpeed link (raw is near-free to decode and the link carries it)"
	}
	return "MJPEG preferred (a HighSpeed or unknown link cannot be trusted with raw at these rates)"
}

func trim0(f float64) string {
	return fpsString(f)
}

func joinN(s []string, sep string) string {
	out := s[0]
	for i := 1; i < len(s); i++ {
		out += sep + s[i]
	}
	return out
}
