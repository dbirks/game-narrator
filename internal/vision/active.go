package vision

import (
	"image"
	"sync"
)

// ActiveArea is the stateless one-frame answer to "where is the picture in
// this frame?" (roi.go). It fails on exactly the frames a live source emits
// between scenes: a fade, a black frame, a frame whose bars have not settled.
// On such a frame it returns the whole canvas, and a region mapped against the
// canvas for one frame is a crop that jumps by the bar width, hashes to
// nothing the OCR cache knows, and reads scene as text.
//
// Tracker is the stateful runtime answer: "where is the picture on this
// source?" It carries the accepted window forward across frames where
// detection is unreliable (fades, blacks, degenerate detections) and accepts
// a genuinely different window only after it repeats itself for several
// consecutive frames. The hysteresis is asymmetric on purpose: noise of a
// couple of pixels at a bar edge never moves the crop, while a real change
// (the converter re-centres, a cutscene letterbox appears or clears) is
// adopted, a few frames late — late is the correct side of the error, because
// a crop that moves on a one-frame glitch reads the glitch as dialogue.
//
// One Tracker belongs to one frame source (one pipeline run, one session).
// Call Update once per frame on the frame-ordered stage; every region's crop
// for that frame then reads the same window through Box, so K regions never
// observe K different windows.
//
// Update is the frame-ordered stage's call; Box may be read from another
// goroutine (the runtime's dev-trace recorder snapshots the window when it
// samples a frame, ADR-0082 amendment 1), so the state is mutex-guarded. The
// hysteresis is still a pure sequence over Update calls: the mutex keeps
// cross-goroutine Box reads whole, it does not change the update rule.
type Tracker struct {
	cfg  trackerConfig
	mu   sync.Mutex
	acc  image.Rectangle // accepted window, valid when have
	have bool
	// pend is the candidate window waiting to settle and its consecutive count.
	// A candidate must be identical on every update while counted; one
	// different observation restarts the count.
	pend      image.Rectangle
	pendCount int
}

// defaultSettle is how many consecutive identical candidates must be observed
// before a new window is adopted. Three frames at a 2–5 Hz recognition rate is
// 0.6–1.5 s: a cutscene letterbox is up (and stays up for seconds) by then,
// while a single dropped or corrupted frame cannot repeat itself.
const defaultSettle = 3

// TrackerConfig tunes the hysteresis. Zero values take the defaults, so a
// profile-stored zero config is a valid config, not a misconfiguration.
type TrackerConfig struct {
	// Settle is the consecutive-identical-candidate count required to adopt a
	// new window (default 3). Bounded to a sane range: below 2 the tracker
	// would move on a single observed frame, above 60 it would lag a real
	// window change by minutes.
	Settle int
	// Tol is the maximum per-edge movement, in pixels, between a candidate
	// and the accepted window that still counts as "the same window" (the
	// crop stays put). Zero (default) derives the tolerance from the frame
	// size: max(2, min(width, height) / 250) — about 4 px at 1080p, 2 px at
	// 720p. A real re-centring or letterbox change moves an edge by dozens of
	// pixels; a bar-edge flicker moves it by one or two.
	Tol int
}

type trackerConfig struct {
	settle, tol int
}

func (c TrackerConfig) normalized() trackerConfig {
	s := c.Settle
	if s < 2 {
		s = defaultSettle
	}
	if s > 60 {
		s = 60
	}
	return trackerConfig{settle: s, tol: c.Tol}
}

// NewTracker returns a tracker with no accepted window: the first frames are
// mapped against the whole canvas (exactly the LetterboxNone behaviour), and
// the window is adopted after Settle stable observations. That start-up
// behaviour is deliberate — a session that begins on a black frame must not
// invent a window from noise.
func NewTracker(cfg TrackerConfig) *Tracker {
	return &Tracker{cfg: cfg.normalized()}
}

// Reset clears all state, returning the tracker to its fresh-start behaviour.
// A session that switches frame sources mid-run (a replay clip spliced after
// a live capture) must not carry the live source's window into the clip.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.acc, t.have = image.Rectangle{}, false
	t.pend, t.pendCount = image.Rectangle{}, 0
}

// Box reports the currently accepted window. ok is false until the first
// window has settled; callers map regions against the whole frame then.
// Safe to read from another goroutine while the frame stage runs Update
// (ADR-0082 amendment 1: the dev-trace recorder's cross-goroutine read).
func (t *Tracker) Box() (area image.Rectangle, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.acc, t.have
}

// Update observes one frame and advances the tracker's state. It returns the
// window to use for this frame's region mapping: the accepted window once one
// has settled, the whole frame otherwise. The return value is always safe to
// hand to MapRect.
//
// The update rule:
//
//   - No bars detected (ok=false from ActiveArea): nothing moves. A black
//     fade frame, a pure-black frame and a 4:3 frame all fail the scan, and
//     all of them must keep the last window rather than fall back to the
//     canvas.
//   - Candidate within Tol of the accepted window: nothing moves. The
//     accepted window is kept exactly — not snapped to the candidate —
//     because a bar edge that flickers a pixel per frame would otherwise
//     move the crop and change every region hash every frame.
//   - Candidate outside Tol: it is a candidate for a new window and must
//     repeat itself, identical, Settle times in a row before the crop moves.
//     Any other observation restarts the count.
func (t *Tracker) Update(img image.Image) image.Rectangle {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return t.current(b)
	}
	cand, ok := ActiveArea(img)
	if !ok {
		// Unreliable detection: the count dies with the frame, but the
		// accepted window (if any) survives.
		t.pend, t.pendCount = image.Rectangle{}, 0
		return t.current(b)
	}
	if !t.have {
		// No accepted window yet: settle one.
		if cand == t.pend {
			t.pendCount++
		} else {
			t.pend, t.pendCount = cand, 1
		}
		if t.pendCount >= t.cfg.settle {
			t.acc, t.have = cand, true
			t.pend, t.pendCount = image.Rectangle{}, 0
		}
		return t.current(b)
	}
	if t.sameWindow(cand) {
		// Within hysteresis: stay exactly where the crop is.
		t.pend, t.pendCount = image.Rectangle{}, 0
		return t.acc
	}
	// A genuinely different window: require the settle count.
	if cand == t.pend {
		t.pendCount++
	} else {
		t.pend, t.pendCount = cand, 1
	}
	if t.pendCount >= t.cfg.settle {
		t.acc = cand
		t.pend, t.pendCount = image.Rectangle{}, 0
	}
	return t.acc
}

// Settle adopts the window this one frame reports from a single observation,
// skipping the settle count. It is the single-frame caller's seam (the one-shot
// CLI, ADR-0082 amendment 1): a one-frame input has no stream to hysteresise
// over, so the settle count it would have to wait for can never elapse, and
// the stateless scan of the one frame the caller was given is the whole
// evidence there is. A frame that reports no window (borderless, fade or
// degenerate) leaves the tracker in its fresh-start state: Box stays unset and
// callers map against the whole frame. The frame loop must keep using Update —
// Settle would let one frame adopt a window the settle count exists to withhold.
func (t *Tracker) Settle(img image.Image) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if img == nil {
		return
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return
	}
	cand, ok := ActiveArea(img)
	if !ok {
		return
	}
	t.acc, t.have = cand, true
	t.pend, t.pendCount = image.Rectangle{}, 0
}

// current is the window in force: the accepted one, or the whole frame. The
// caller holds the mutex (Update and Settle run it); it is the read half of
// the window, not a public entry point.
func (t *Tracker) current(b image.Rectangle) image.Rectangle {
	if t.have {
		return t.acc
	}
	return b
}

// sameWindow reports whether a candidate is the accepted window within the
// per-edge tolerance.
func (t *Tracker) sameWindow(cand image.Rectangle) bool {
	tol := t.cfg.tol
	if tol <= 0 {
		tol = defaultTol(t.acc.Dx(), t.acc.Dy())
	}
	return edgeDist(t.acc.Min, cand.Min) <= tol && edgeDist(t.acc.Max, cand.Max) <= tol
}

func defaultTol(w, h int) int {
	m := w
	if h < m {
		m = h
	}
	tol := m / 250
	if tol < 2 {
		tol = 2
	}
	return tol
}

func edgeDist(a, b image.Point) int {
	dx, dy := b.X-a.X, b.Y-a.Y
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	if dx > dy {
		return dx
	}
	return dy
}
