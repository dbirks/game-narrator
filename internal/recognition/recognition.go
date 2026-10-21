// Package recognition decides *what text a dialogue box currently shows and
// whether it is finished changing* — nothing else.
//
// It is the stage docs/spec/05-runtime-recognition.md calls the "Dialogue
// observation state machine", sitting between OCR and the commitment policy:
// frames arrive with a cropped region and (maybe) an OCR result, and the
// package reports state transitions with reasons. The commitment decision
// itself lives one level up (internal/session), because it depends on the
// candidate ranking, which this package must not know about: docs/spec/01
// "Candidate lookup is local and deadline constrained" makes retrieval a
// separate worker, and a tracker that reached into the index would serialize
// OCR-rate work onto a corpus scan.
//
// Deliberate properties, each of which exists because a named failure mode in
// the spec names it:
//
//   - Deterministic and pure. Tracker.Step takes an immutable Observation and
//     returns []Transition; it starts no goroutine, reads no clock, opens no
//     file. Time enters only through CaptureTimestampNS, so a test can replay a
//     typewriter at 200 ms per frame and get byte-identical results (docs/spec/12
//     "frames skipped at varied fps", "two OCR futures finish out of order", and
//     AGENTS.md "no simulation theater" — a mocked timeline must still be the
//     real algorithm, which is only possible if the algorithm has no clock).
//   - One frame, one source of truth about time. Observation is rejected if it
//     lacks source_clock or capture_timestamp_ns (docs/spec/01 invariant 1).
//     A tracker that silently accepted wall-clock-only observations would
//     reintroduce the ambiguity the invariant exists to remove.
//   - Duplicate suppression is structural, not advisory. An unchanged region
//     hash means no OCR call (counted in Stats.OCRSkippedSameHash,
//     docs/spec/05 "suppress duplicate OCR calls on unchanged region hash"),
//     and a typewriter prefix is never a new utterance ("A typewriter prefix is
//     not a new distinct utterance"), which is why prefix-extension growth is
//     classified separately from a material change.
//
// There is no game-specific logic here, and there must never be
// (docs/spec/04). Every threshold is a documented Config field with a
// conservative default, not a constant tuned on one game's screenshots.
package recognition

import (
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// State is one of the nine dialogue-observation states in docs/spec/05
// "Dialogue observation state machine". The zero value is StateAbsent: a
// tracker for a region that has never been seen is genuinely absent, so a
// freshly constructed Tracker needs no initialization call to be correct.
type State uint8

const (
	StateAbsent State = iota
	StateAppearing
	StateRevealing
	StateStabilizing
	StateReady
	StateCommitted
	StateAdvancing
	StateDismissed
	StateError
)

var stateNames = [...]string{
	"absent", "appearing", "revealing", "stabilizing", "ready",
	"committed", "advancing", "dismissed", "error",
}

// String names the state for event payloads and the developer inspector
// (docs/spec/09 "Link everything using trace_id": a trace full of integers is
// not inspectable).
func (s State) String() string {
	if int(s) >= len(stateNames) {
		return fmt.Sprintf("state(%d)", uint8(s))
	}
	return stateNames[s]
}

// JSONValue lets State marshal as its name rather than its number, so an
// event's `text_state` field reads the same in the SSE stream, the events
// table and a fixture.
func (s State) JSONValue() any { return s.String() }

// MarshalText implements encoding.TextMarshaler so encoding/json writes the
// name.
func (s State) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText accepts the name written by MarshalText.
func (s *State) UnmarshalText(b []byte) error {
	for i, n := range stateNames {
		if string(b) == n {
			*s = State(i)
			return nil
		}
	}
	return fmt.Errorf("recognition: unknown state %q", string(b))
}

// Reasons are the machine-readable causes attached to every Transition. They
// are lower-case snake tokens because they are the text of a transition, not a
// matcher verdict — the matcher's own codes (MATCH_EXACT, ABSTAINED, …,
// documented in docs/spec/17 "Confidence combination strategy") travel in the
// events that internal/session emits, and conflating the two vocabularies is
// how a stability heuristic ends up looking like a match decision.
const (
	// ReasonRegionDetected: ABSENT/DISMISSED→APPEARING after AppearFrames
	// consecutive present frames (docs/spec/05 "region detector sees a plausible
	// textbox for minimum consecutive frames").
	ReasonRegionDetected = "region_detected"
	// ReasonTextRevealing: APPEARING→REVEALING, non-empty text observed.
	ReasonTextRevealing = "text_content_increasing"
	// ReasonStableWindow: REVEALING→STABILIZING, the prefix has not changed for
	// Config.StableWindow.
	ReasonStableWindow = "prefix_stable_window"
	// ReasonEvidenceSufficient: STABILIZING→READY, OCR confidence above
	// Config.MinConfidence with no contradictory observation in the vote window.
	ReasonEvidenceSufficient = "evidence_sufficient"
	// ReasonLowConfidence: STABILIZING→READY refused; the caller (session) reads
	// this as "wait", never as "guess" (docs/spec/05 "Never invent dialogue to
	// fill uncertainty").
	ReasonLowConfidence = "insufficient_confidence"
	// ReasonMaterialChange: COMMITTED→ADVANCING, the text changed by more than a
	// typewriter extension.
	ReasonMaterialChange = "material_text_change"
	// ReasonPredictionMisfire: the committed text turned out to be wrong before
	// the next page, per docs/spec/05 "if already playing, fade and mark
	// prediction_misfire". The audio-level bookkeeping is the session's.
	ReasonPredictionMisfire = "prediction_misfire"
	// ReasonNewTextRevealing: ADVANCING→REVEALING for the incoming page.
	ReasonNewTextRevealing = "new_text_revealing"
	// ReasonContradictoryText: a READY/STABILIZING text changed materially
	// before it was committed, so it was not final after all.
	ReasonContradictoryText = "contradictory_text"
	// ReasonRegionVanished: ANY→DISMISSED.
	ReasonRegionVanished = "region_vanished"
	// ReasonNewLifetime: DISMISSED/ABSENT→APPEARING for a box that is a new
	// occurrence rather than a continuation (docs/spec/12 "repeat NPC after
	// menu": the same words in a new box lifetime must be a new occurrence).
	ReasonNewLifetime = "new_box_lifetime"
	// ReasonInvalidObservation: ANY→ERROR, the observation violated the input
	// contract (docs/spec/01 invariant 1) and cannot be interpreted.
	ReasonInvalidObservation = "invalid_observation"
	// ReasonRecovered: ERROR→ABSENT on the first well-formed observation.
	ReasonRecovered = "recovered"
	// ReasonCommitted: READY→COMMITTED, requested by the session reducer.
	ReasonCommitted = "message_committed"
	// ReasonPredictivePrefix is the matcher's PREDICTIVE_PREFIX code, reused
	// here as the reason a REVEALING→COMMITTED transition is legal at all
	// (docs/spec/05 "REVEALING→COMMITTED: only predictive mode"). It is the one
	// reason drawn from the matcher vocabulary because it *is* the matcher's
	// evidence claim; see CommitRequest.Reason.
	ReasonPredictivePrefix = "PREDICTIVE_PREFIX"
)

// Observation is one immutable look at one region in one frame. It is a value
// passed by the capture/ROI worker into the tracker, and the tracker never
// retains a pointer into the frame pipeline: the OCR result is read for its
// words and confidence and then not needed, which is what lets the capture
// loop reuse its buffers (docs/spec/01 "ROI worker pool … cancellation on
// newer observation").
type Observation struct {
	// FrameSeq is the capture loop's monotonic frame number. Transitions carry
	// it so a trace can be lined up against frame.observed.
	FrameSeq int64
	// CaptureTimestampNS is the frame's capture time in nanoseconds on
	// SourceClock. Required: docs/spec/01 invariant 1 makes every frame carry
	// an explicit source clock and capture timestamp rather than a wall-clock
	// label, because the stability window has to be measured on a clock that
	// does not jump when the NTP client corrects the system time.
	CaptureTimestampNS int64
	// SourceClock names the clock CaptureTimestampNS belongs to ("monotonic",
	// "v4l2", "dolphin-emulator", …). Required, same invariant.
	SourceClock string
	// RegionID is the ROI this observation covers ("bottom", "choices"…). A
	// Tracker belongs to exactly one region, so that typewriter progress in one
	// box is never compared against text from another.
	RegionID string
	// RegionPresent reports whether the detector still believes a textbox is
	// on screen at this ROI. ANY→DISMISSED follows its loss (docs/spec/05).
	RegionPresent bool
	// RegionHash identifies the cropped ROI's pixels. When it is unchanged from
	// the previous frame, OCR is skipped and the prior result reused; an empty
	// hash means "no hash computed for this frame", which is not the same as
	// "unchanged" and never counts as a skip.
	RegionHash string
	// OCR is this frame's recognition result, nil when the region hash was
	// unchanged and the previous result is being reused, or when OCR failed and
	// the caller chose not to report a result (which the tracker reads as "no
	// new evidence", not as "the text disappeared").
	OCR *ocr.Result
	// RevealDetector reports that the region's game draws a reveal-complete
	// marker and the pipeline looked for it on this frame (ADR-0152);
	// RevealComplete is the marker's verdict. Games without a marker leave
	// both false and the tracker settles on the stability window alone.
	RevealDetector bool
	RevealComplete bool
}

// Validate enforces the input contract. It returns a descriptive error rather
// than a bool because the error text becomes the Reason of the ERROR
// transition and shows up in the developer inspector, where "invalid" alone
// would be a dead end.
func (o Observation) Validate() error {
	switch {
	case o.RegionID == "":
		return fmt.Errorf("region_id is required")
	case o.SourceClock == "":
		return fmt.Errorf("source_clock is required (docs/spec/01 invariant 1)")
	case o.CaptureTimestampNS <= 0:
		return fmt.Errorf("capture_timestamp_ns must be positive on clock %q (docs/spec/01 invariant 1)", o.SourceClock)
	case o.FrameSeq < 0:
		return fmt.Errorf("frame_seq %d is negative", o.FrameSeq)
	case o.RegionPresent && o.OCR == nil && o.RegionHash == "":
		// A nil OCR with no hash cannot be attributed: it is either "skipped
		// because unchanged" (which requires a hash) or "the engine failed".
		// Guessing either way would make an OCR outage look like a cleared
		// textbox, so the observation is refused and the tracker says why.
		return fmt.Errorf("nil OCR requires a region_hash proving the region is unchanged")
	}
	return nil
}

// Transition is one state change with its cause. Step returns a slice because a
// single frame can legitimately chain transitions — a box that appears already
// fully rendered goes APPEARING→REVEALING→STABILIZING→READY on one frame, and
// a page advance is COMMITTED→ADVANCING→REVEALING. Collapsing those into the
// last state would lose exactly the evidence docs/spec/05 "Timings and
// benchmarks" wants to reconstruct.
type Transition struct {
	From   State  `json:"from"`
	To     State  `json:"to"`
	Reason string `json:"reason"`
	// Text is the tracker's accepted normalized text *after* the transition, so
	// a reader can see what the state was about.
	Text string `json:"text,omitempty"`
	// FrameSeq / CaptureTimestampNS locate the transition in the capture
	// timeline.
	FrameSeq           int64 `json:"frame_seq"`
	CaptureTimestampNS int64 `json:"capture_timestamp_ns"`
}

// String renders "revealing->stabilizing prefix_stable_window" for log lines
// and the compact transition list in a text.observed payload.
func (tr Transition) String() string {
	return fmt.Sprintf("%s->%s %s", tr.From, tr.To, tr.Reason)
}

// Config tunes the state machine. Zero fields mean "use the default", so
// Config{MinConfidence: 0.8} is a valid way to ask for one stricter threshold,
// and no field can be set to a value the machine cannot cope with — normalize
// clamps. A profile overrides these per region (docs/spec/02 gives regions
// their own ocr settings); nothing here is game knowledge.
type Config struct {
	// AppearFrames is the minimum number of consecutive present frames before
	// ABSENT→APPEARING, which is what stops a single noisy detector frame from
	// starting an occurrence.
	AppearFrames int
	// StableWindow is how long the prefix must be unchanged before
	// REVEALING→STABILIZING, i.e. how long a typewriter may pause without being
	// declared finished. Measured on SourceClock, not wall time.
	StableWindow time.Duration
	// MinConfidence is the mean OCR confidence (in [0,1]) required for
	// STABILIZING→READY. Below it the tracker stays put: docs/spec/05 answers
	// low confidence with silence or a second opinion, not with a guess.
	MinConfidence float64
	// VoteWindow is N — the number of recent OCR texts kept for temporal voting
	// (docs/spec/05 "Use temporal voting between several adjacent frames").
	VoteWindow int
	// PredictiveMode enables the only path out of REVEALING straight to
	// COMMITTED. Off by default per docs/spec/05 "Default conservative
	// full-line readiness, enable predictive mode explicitly".
	PredictiveMode bool
	// MinPredictLen is the minimum normalized rune count a prefix must reach
	// before a predictive commit is allowed — "very small agreeing set" plus
	// "nontrivial length" in docs/spec/05 "Predictive playback policy". A
	// 3-character fragment is how "Yes." gets spoken as "Yes, we are under
	// attack."
	MinPredictLen int
	// ChangeThreshold is the normalized edit-distance ratio above which a text
	// change is material rather than OCR jitter.
	ChangeThreshold float64
	// AdvanceRunFrames is how many consecutive frames must agree on a materially
	// different text before that text overrides the vote. This is the flicker
	// gate docs/spec/12 "Dialogue and graph" tests for: one bad frame inside a
	// stable box must not cancel the player's speech.
	AdvanceRunFrames int
	// MaxContradictionRatio bounds how much vote weight may be incompatible with
	// the accepted text while still allowing READY. A box whose recent frames
	// disagree this much is not stable, however good the mean confidence is.
	MaxContradictionRatio float64
	// MinWordConfidence drops OCR words below this confidence before the text is
	// assembled, for the low-confidence artifacts docs/spec/05 "Text processing"
	// says to ignore (an edge cursor read as "l" or ">").
	MinWordConfidence float64
}

// DefaultConfig is the conservative starting point from docs/spec/05: 2–5 Hz
// OCR means a full line reveals in a few frames, so a quarter-second stability
// window and a five-frame vote are the shape of the timing budget, not tuned
// constants. Values are advisory until the labeled holdout sets in
// docs/spec/12 exist; changing them then is expected, and this table is where
// the change is made.
func DefaultConfig() Config {
	return Config{
		AppearFrames:          2,
		StableWindow:          250 * time.Millisecond,
		MinConfidence:         0.55,
		VoteWindow:            5,
		PredictiveMode:        false,
		MinPredictLen:         8,
		ChangeThreshold:       0.34,
		AdvanceRunFrames:      2,
		MaxContradictionRatio: 0.25,
		MinWordConfidence:     0.3,
	}
}

// normalize clamps c into a usable configuration, filling unset fields from
// DefaultConfig. A NaN confidence or a negative window would otherwise make a
// comparison never or always true — the docs/spec/12 fuzz target for the
// matcher applies to this package for the same reason, so nothing trusts a
// caller-supplied number.
func (c Config) normalize() Config {
	d := DefaultConfig()
	c.AppearFrames = clampInt(c.AppearFrames, d.AppearFrames, 1, 64)
	c.VoteWindow = clampInt(c.VoteWindow, d.VoteWindow, 1, 32)
	c.MinPredictLen = clampInt(c.MinPredictLen, d.MinPredictLen, 1, 256)
	c.AdvanceRunFrames = clampInt(c.AdvanceRunFrames, d.AdvanceRunFrames, 1, c.VoteWindow)
	if c.StableWindow <= 0 {
		c.StableWindow = d.StableWindow
	}
	if c.StableWindow > time.Hour {
		c.StableWindow = time.Hour
	}
	c.MinConfidence = pickFloat(c.MinConfidence, d.MinConfidence, 0, 1)
	c.ChangeThreshold = pickFloat(c.ChangeThreshold, d.ChangeThreshold, 0.01, 1)
	c.MaxContradictionRatio = pickFloat(c.MaxContradictionRatio, d.MaxContradictionRatio, 0, 1)
	c.MinWordConfidence = pickFloat(c.MinWordConfidence, d.MinWordConfidence, 0, 1)
	// A run gate wider than the window could never fire, which would silently
	// disable flicker suppression.
	if c.AdvanceRunFrames > c.VoteWindow {
		c.AdvanceRunFrames = c.VoteWindow
	}
	return c
}

// FoldOptions describes the normalization used for *comparison*. Comparison is
// deliberately lossier than the matcher's key folding: the tracker's job is to
// tell "the same text again" from "different text", and OCR jitter between two
// frames of one static box (a period read as a colon, an O as a 0) must land on
// the same side of that test. Raw text is preserved separately and is what
// goes into events, per docs/spec/05 "Preserve raw observed OCR and normalized
// text separately".
func FoldOptions() textnorm.Options {
	return textnorm.Options{StripPunctuation: true, OCRFold: true}
}

// clampInt resolves one integer field. Unset (or non-positive) takes the default;
// an out-of-range value is clamped to the bound rather than discarded, because a
// caller that asks for a 9,999-frame vote window means "as large as the machine
// allows", and silently receiving the default would change flicker behaviour with
// nothing in the log saying so. Config() reports the effective values, so the
// clamp is observable to a developer panel.
func clampInt(v, def, lo, hi int) int {
	if v <= 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func pickFloat(v, def, lo, hi float64) float64 {
	if v == 0 || !(v >= lo && v <= hi) { // also catches NaN
		return def
	}
	return v
}

// Stats are counters, not events: a caller that needs to *know* something
// happened reads an event, and a caller that needs to know how often reads
// these (docs/spec/13 exposes them as metrics). They exist mainly so the tests
// can prove a suppression fired instead of merely failing to notice it didn't.
type Stats struct {
	// Observations counts Step calls, valid or not.
	Observations int64 `json:"observations"`
	// Invalid counts observations refused by Validate (which put the tracker in
	// StateError).
	Invalid int64 `json:"invalid"`
	// OCRSkippedSameHash counts frames where the region hash was unchanged and
	// the previous OCR result was reused — the duplicate-call suppression
	// docs/spec/05 "OCR pipeline" requires, and the number that decides whether
	// OCR cost is per-frame or per-change.
	OCRSkippedSameHash int64 `json:"ocr_skipped_same_hash"`
	// OCRRedundantSameHash counts the opposite mistake: an OCR result delivered
	// for a region hash the tracker had already read. Nothing is corrupted —
	// the vote simply sees the same text twice — but it means a caller is
	// burning CPU the hash was supposed to save.
	OCRRedundantSameHash int64 `json:"ocr_redundant_same_hash"`
	// EmptyReads counts present-region observations that produced no usable
	// text (a blank box, or every word below MinWordConfidence).
	EmptyReads int64 `json:"empty_reads"`
	// TextChanges counts accepted-text changes, i.e. how much typing the vote
	// window absorbed.
	TextChanges int64 `json:"text_changes"`
	// CommittedDrift counts frames after a commit on which the on-screen text
	// moved by less than a material change — the trace of a line committed before
	// it had finished typing (a StableWindow shorter than the typewriter's pause).
	// The committed occurrence is never rewritten, so this counter is the only
	// place that truncation stays visible (docs/spec/12 scores duplicate and
	// missed narration from exactly this kind of counter).
	CommittedDrift int64 `json:"committed_drift"`
	// RevealGrowthFrames counts frames on which a fresh OCR read strictly
	// extended the previous read — a typewriter revealing more of the same
	// line. Each such frame restarts the stability window, so STABILIZING and
	// READY are only reachable after the raw text has stopped growing for the
	// whole StableWindow.
	RevealGrowthFrames int64 `json:"reveal_growth_frames"`
	// Transitions counts emitted transitions.
	Transitions int64 `json:"transitions"`
	// Commits counts READY→COMMITTED / predictive REVEALING→COMMITTED.
	Commits int64 `json:"commits"`
	// CommitRejected counts Commit calls refused by the state machine (a commit
	// requested while revealing without predictive mode, or for text the tracker
	// does not hold).
	CommitRejected int64 `json:"commit_rejected"`
}

// Snapshot is the tracker's readable state for the developer inspector
// (docs/spec/16 M2 "Developer inspector displays frame, crop, normalized OCR
// and candidates"). It is a copy; holding one does not pin the tracker.
type Snapshot struct {
	RegionID        string    `json:"region_id"`
	State           State     `json:"state"`
	Text            string    `json:"text"`
	Key             string    `json:"key"`
	Raw             string    `json:"raw_text"`
	Confidence      float64   `json:"confidence"`
	Lifetime        uint64    `json:"lifetime"`
	Committed       string    `json:"committed_text,omitempty"`
	CommittedReason string    `json:"committed_reason,omitempty"`
	LastFrameSeq    int64     `json:"last_frame_seq"`
	LastTimestampNS int64     `json:"last_capture_timestamp_ns"`
	Predicted       bool      `json:"predicted"`
	LastError       string    `json:"last_error,omitempty"`
	Window          int       `json:"window"`
	SinceChange     Duration  `json:"since_change"`
	Contradictions  float64   `json:"contradiction_ratio"`
	Now             time.Time `json:"-"`
}

// Duration is time.Duration in JSON without nanosecond noise.
type Duration time.Duration

// MarshalText renders a millisecond duration.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }
