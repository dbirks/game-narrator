// Package signal classifies the content of live HDMI capture frames so the
// capture path can tell "the game is playing" from "the card is no longer
// receiving a signal". The MS21xx-class UVC cards the UNO Q capture path
// targets (docs/spec/10) do not report an HDMI-input-loss state through
// v4l2: when the HDMI plug goes out or the source changes mode they either
// keep emitting a generated pattern (black, blue, TV static) or keep
// delivering the last frame they received. The kernel hands the pipeline all
// of those as perfectly good video, so the content itself is the only
// evidence, and docs/spec/12 lists "video loss/freeze" and "repeated
// identical frames" as the perception cases to handle.
//
// Two layers, both pure enough to test on synthetic frames with no card:
//
//   - Classify judges one frame (black / blue / static / frozen / ok) from
//     pixels and the previous frame's hash;
//   - Monitor adds the hysteresis a single-frame judgment needs: a 10 Hz
//     pipeline must not declare the input lost on one black frame (a game
//     fade-to-black lasts two seconds, and nothing is being committed while
//     the screen is legitimately empty) and must not flap back to live on
//     one clean frame after the plug comes back.
//
// The runtime (internal/runtime) wraps the v4l2 source with the monitor:
// frames are held back from the pipeline while the input is lost, which is
// what "pause matching" means concretely — the tracker sees no frames and
// therefore commits nothing, and recovery is automatic because the monitor
// re-admits frames once the content is good again.
package signal

import (
	"fmt"
	"image"
	"math"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// Kind is the per-frame classification.
type Kind int

const (
	// OK is fresh, normal content (or a streak too young to judge).
	OK Kind = iota
	// Black is a flat near-black picture: the card's no-HDMI frame.
	Black
	// Blue is a flat blue field, the other classic no-signal pattern.
	Blue
	// Static is TV static: per-pixel noise with no structure.
	Static
	// Frozen is the card repeating the last frame it received.
	Frozen
	// SizeChanged is a frame whose dimensions differ from the established
	// picture size: the source changed mode (or is mid-transition).
	SizeChanged
)

// String names the kind for events and doctor output.
func (k Kind) String() string {
	switch k {
	case Black:
		return "black frame (no-signal pattern)"
	case Blue:
		return "blue frame (no-signal pattern)"
	case Static:
		return "static noise (no-signal pattern)"
	case Frozen:
		return "frozen input (card repeating the last frame)"
	case SizeChanged:
		return "picture size changed (source mode change)"
	default:
		return "ok"
	}
}

// Lost reports whether the frame carries no fresh, usable signal.
func (k Kind) Lost() bool { return k != OK }

// sampleStride is the linear sampling factor for the content statistics,
// the same 1/8 rule HashFrame documents: 1/64 of the pixels is enough to
// judge "is this whole picture one flat colour or white noise", and the
// monitor runs on the four A53 cores' most contested budget.
const sampleStride = 8

// Content thresholds. They are tuned for the documented MS21xx behaviour
// (flat generated patterns, not game art) and the tests pin both the
// positive cases (the card's patterns) and the near-miss cases (a game sky,
// a dark scene) so a threshold change is a conscious decision.
const (
	// blackMaxLuma: a no-signal black is darker than any game black, and
	// flat. A dark game scene keeps texture (luma std well above 6).
	blackMaxLuma = 12.0
	blackMaxStd  = 6.0

	// blueMinMean / blueMinGap: a no-signal blue is a flat field dominated by
	// the blue channel by a wide margin (the gap of 60 rejects a pale
	// blue-gray UI panel). A game sky is rejected by bRange (a gradient sky
	// spans the blue channel; the card's flat field does not, text band
	// included) and by fracDev40 (a sky's luma has no dominant bin; the
	// field's text is a thin band far from the field luma).
	blueMinMean = 96.0
	blueMinGap  = 60.0
	// blueMaxBRange: the card's field is flat. Even with a "NO SIGNAL" text
	// band the blue channel spans at most field→white (a 205 field with white
	// text spans 50); a gradient sky spans well above it. A field darker than
	// ~205 with a white band is misread as live content — the safe direction,
	// because matching on "NO SIGNAL" text cannot commit (it is not in any
	// game's corpus).
	blueMaxBRange = 55

	// staticMinLocal is the mean absolute difference between adjacent
	// sampled pixels: white noise averages ~32 (two independent pixels
	// differ by ~128/4 on expectation), a 4:3 game picture upscaled 16:9
	// averages well under 24, and the flatness guard below keeps a noisy
	// *scene* (a TV on screen in the game) from reading as whole-frame
	// static.
	staticMinLocal = 28.0
	staticMaxBin   = 0.12
)

// stats is the sampled picture's summary statistics.
type stats struct {
	n                   int
	lumaMean            float64
	lumaStd             float64
	rMean, gMean, bMean float64
	rStd, gStd, bStd    float64
	local               float64
	maxBinPct           float64
	fracDev40           float64 // share of samples > 40 luma from the dominant luma
	modeLuma            int     // the dominant luma bin
	bRange              int     // b channel spread (max-min over samples)
}

// sample walks img at sampleStride and fills in the picture statistics.
func sample(img image.Image) stats {
	var st stats
	b := img.Bounds()
	var (
		sumR, sumG, sumB   float64
		sumL               float64
		sqR, sqG, sqB, sqL float64
		bins               [256]int
		localSum           float64
		localN             int
		bMin, bMax         int
		bInit              bool
	)
	prevRow := make([][3]int, 0, b.Dx()/sampleStride+1)
	for y := b.Min.Y; y < b.Max.Y; y += sampleStride {
		row := make([][3]int, 0, b.Dx()/sampleStride+1)
		for x := b.Min.X; x < b.Max.X; x += sampleStride {
			r, g, bl, _ := img.At(x, y).RGBA()
			pr, pg, pb := int(r>>8), int(g>>8), int(bl>>8)
			row = append(row, [3]int{pr, pg, pb})
			if !bInit {
				bMin, bMax, bInit = pb, pb, true
			}
			if pb < bMin {
				bMin = pb
			}
			if pb > bMax {
				bMax = pb
			}
			st.n++
			sumR += float64(pr)
			sumG += float64(pg)
			sumB += float64(pb)
			l := 0.299*float64(pr) + 0.587*float64(pg) + 0.114*float64(pb)
			sumL += l
			sqR += float64(pr * pr)
			sqG += float64(pg * pg)
			sqB += float64(pb * pb)
			sqL += l * l
			bins[lumaBin(pr, pg, pb)]++
		}
		if len(prevRow) == len(row) {
			for i := range row {
				for c := 0; c < 3; c++ {
					d := row[i][c] - prevRow[i][c]
					if d < 0 {
						d = -d
					}
					localSum += float64(d)
					localN++
				}
			}
		}
		prevRow = row
	}
	if st.n == 0 {
		return st
	}
	st.lumaMean = sumL / float64(st.n)
	st.lumaStd = stdDev(sqL, sumL, st.n)
	st.rMean = sumR / float64(st.n)
	st.gMean = sumG / float64(st.n)
	st.bMean = sumB / float64(st.n)
	st.rStd = stdDev(sqR, sumR, st.n)
	st.gStd = stdDev(sqG, sumG, st.n)
	st.bStd = stdDev(sqB, sumB, st.n)
	if localN > 0 {
		st.local = localSum / float64(localN)
	}
	maxBin := 0
	mode := 0
	for l := range bins {
		if bins[l] > maxBin {
			maxBin = bins[l]
			mode = l
		}
	}
	st.maxBinPct = float64(maxBin) / float64(st.n)
	// fracDev40 is the share of the picture more than 40 luma away from the
	// dominant luma. A flat no-signal field with a text band keeps it small
	// (the text is a thin band far from the field); a game sky is a gradient
	// with a large deviant share, so it stays a game frame.
	var dev int
	for l := range bins {
		dd := l - mode
		if dd < 0 {
			dd = -dd
		}
		if dd > 40 {
			dev += bins[l]
		}
	}
	st.fracDev40 = float64(dev) / float64(st.n)
	st.modeLuma = mode
	st.bRange = bMax - bMin
	return st
}

// lumaBin buckets a pixel's BT.601 luma into its 0–255 histogram bin.
func lumaBin(r, g, b int) int {
	l := int(0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b) + 0.5)
	if l < 0 {
		return 0
	}
	if l > 255 {
		return 255
	}
	return l
}

// stdDev is the population standard deviation from running sums.
func stdDev(sq, sum float64, n int) float64 {
	if n == 0 {
		return 0
	}
	mean := sum / float64(n)
	v := sq/float64(n) - mean*mean
	if v < 0 {
		v = 0
	}
	return math.Sqrt(v)
}

// Classify judges one frame. hash and prevHash are the frame's and the
// previous frame's capture.HashFrame values ("" when unknown): an identical
// hash is the frozen case, and it is checked first because it is the
// cheapest test and the card's "stop delivering" mode. A nil image is the
// decode-failure case, which the source counts as a DecodeError; it is not a
// signal judgment.
func Classify(img image.Image, hash, prevHash string) Kind {
	if img == nil {
		return OK
	}
	if hash != "" && hash == prevHash {
		return Frozen
	}
	st := sample(img)
	switch {
	case st.lumaMean < blackMaxLuma && st.lumaStd < blackMaxStd:
		return Black
	case st.bMean >= blueMinMean &&
		st.bMean-st.rMean >= blueMinGap &&
		st.bMean-st.gMean >= blueMinGap &&
		st.bRange <= blueMaxBRange &&
		st.fracDev40 <= 0.25:
		return Blue
	case st.local >= staticMinLocal && st.maxBinPct < staticMaxBin:
		return Static
	default:
		return OK
	}
}

// Event is one signal-state transition the monitor observed.
type Event struct {
	// Lost is the state the monitor moved to.
	Lost bool
	// Kind is the classification that caused the transition (OK on a
	// recovery).
	Kind Kind
	// Detail is the human sentence the event payload carries.
	Detail string
	// AtNS and Seq come from the frame that tipped the decision, so the
	// session event correlates with the frame evidence the dev trace kept.
	AtNS int64
	Seq  int64
}

// Monitor is the hysteresis layer over Classify. It is single-consumer: one
// Monitor per source, fed from the source's own frame loop, because the
// hysteresis state is an ordered view of one frame stream.
type Monitor struct {
	// LostAfter is the consecutive lost-classified frames required before
	// the monitor declares the input lost. Three at a 10 Hz analysis rate is
	// 300 ms: a game fade-to-black is longer and is handled (the screen is
	// empty, so nothing commits while it is held), a single corrupt frame is
	// shorter.
	LostAfter int
	// OKAfter is the consecutive good frames required to clear a declared
	// loss. The plug-back transient on an MS21xx (a flash of the old size,
	// then the new picture settling) is why recovery is not one frame.
	OKAfter int
	// SizeSettle is the consecutive frames at one new size required before
	// a size change is a mode change rather than a single-frame glitch.
	SizeSettle int
	// FreezeWindow is how long the identical-hash streak must run before it
	// is judged Frozen; zero counts immediate repetition. The window exists
	// because a delivery stall shorter than it is queue behaviour, not input
	// loss.
	FreezeWindow time.Duration

	refW, refH    int
	altW, altH    int
	altStreak     int
	prevHash      string
	prevNS        int64
	frozenSinceNS int64
	lostStreak    int
	okStreak      int
	lost          bool
	hasRef        bool
}

// NewMonitor returns the documented defaults: the 300 ms lost gate, the
// two-frame recovery gate, the three-frame size gate and the 3 s freeze
// window. All four are overridable for tests and for a card whose transient
// behaviour is measured differently.
func NewMonitor() *Monitor {
	return &Monitor{
		LostAfter:    3,
		OKAfter:      2,
		SizeSettle:   3,
		FreezeWindow: 3 * time.Second,
	}
}

// Observe consumes one delivered frame and returns the monitor's state
// after this frame (lost), whether the state changed (changed) and, when it
// did, the transition event. The wrapper forwards the frame to the pipeline
// only while lost is false — that hold-back is the paused matching.
func (m *Monitor) Observe(f capture.Frame) (lost, changed bool, ev Event) {
	if !m.hasRef {
		m.refW, m.refH = f.Width, f.Height
		m.hasRef = true
	}
	if f.Width != m.refW || f.Height != m.refH {
		if f.Width == m.altW && f.Height == m.altH {
			m.altStreak++
		} else {
			m.altW, m.altH = f.Width, f.Height
			m.altStreak = 1
		}
		if m.altStreak < m.sizeSettle() {
			// Mid-transition: the frame is not trusted, and the size has not
			// settled. Nothing else is judged on it.
			return m.decide(SizeChanged, f)
		}
		m.refW, m.refH = m.altW, m.altH
		m.altW, m.altH = 0, 0
		m.altStreak = 0
		m.prevHash = ""
		return m.decide(SizeChanged, f)
	}
	m.altStreak = 0

	kind := Classify(f.Image, f.Hash, m.prevHash)
	if kind == Frozen {
		if m.prevNS == f.CaptureTimestampNS {
			// No clock advance between two identical frames: a live feed
			// cannot hand out the same frame at one instant, so a stream
			// that does is, by definition, not delivering fresh video. Judge
			// the repeat as frozen without waiting on the window, which is
			// the case the synthetic and replayed streams fall into.
			kind = Frozen
		} else if m.FreezeWindow > 0 {
			if m.frozenSinceNS == 0 || f.CaptureTimestampNS < m.frozenSinceNS {
				m.frozenSinceNS = f.CaptureTimestampNS
			}
			if f.CaptureTimestampNS-m.frozenSinceNS < int64(m.FreezeWindow) {
				// The streak is younger than the window: a queue stall, not a
				// dead input. Judged ok for the hysteresis; the streak keeps
				// running and is judged frozen on the frame that outlives the
				// window.
				kind = OK
			}
		}
		// A zero FreezeWindow leaves the immediate Frozen judgment in place.
	} else {
		m.frozenSinceNS = 0
	}
	m.prevHash = f.Hash
	m.prevNS = f.CaptureTimestampNS
	return m.decide(kind, f)
}

func (m *Monitor) sizeSettle() int {
	if m.SizeSettle <= 0 {
		return 3
	}
	return m.SizeSettle
}

// decide applies the hysteresis counters to one classified frame and reports
// the transition it causes, if any. A frame that is lost but the monitor is
// not yet lost (or vice versa) moves the streaks without an event; the event
// fires on the frame that crosses the gate, and only then.
func (m *Monitor) decide(kind Kind, f capture.Frame) (bool, bool, Event) {
	if kind.Lost() {
		m.lostStreak++
		m.okStreak = 0
	} else {
		m.okStreak++
		m.lostStreak = 0
	}
	switch {
	case !m.lost && m.lostStreak >= m.lostAfter():
		m.lost = true
		m.lostStreak = 0
		return m.report(true, kind, f)
	case m.lost && m.okStreak >= m.okAfter():
		m.lost = false
		m.okStreak = 0
		return m.report(false, OK, f)
	default:
		return m.lost, false, Event{}
	}
}

func (m *Monitor) lostAfter() int {
	if m.LostAfter <= 0 {
		return 3
	}
	return m.LostAfter
}

func (m *Monitor) okAfter() int {
	if m.OKAfter <= 0 {
		return 2
	}
	return m.OKAfter
}

// report builds the transition event and its detail sentence.
func (m *Monitor) report(lost bool, kind Kind, f capture.Frame) (bool, bool, Event) {
	var detail string
	if lost {
		detail = fmt.Sprintf("input lost after %d consecutive frame(s): %s; frames held back, matching paused",
			m.lostAfter(), kind)
	} else {
		detail = fmt.Sprintf("input recovered after %d good frame(s); frames flow to the pipeline again",
			m.okAfter())
	}
	return lost, true, Event{Lost: lost, Kind: kind, Detail: detail, AtNS: f.CaptureTimestampNS, Seq: f.Seq}
}

// State reports the monitor's current judgment without consuming a frame.
func (m *Monitor) State() (lost bool) { return m.lost }
