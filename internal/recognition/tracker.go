package recognition

import (
	"errors"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/games/textnorm"
)

// ErrNotCommittable reports a Commit request the state machine refuses. It is a
// sentinel so the session reducer can tell "not yet" from a bug, and it is
// returned rather than swallowed because a commit that should not have been
// attempted is a defect in the caller's policy, not a condition to retry.
var ErrNotCommittable = errors.New("recognition: not committable")

// entry is one frame's contribution to the temporal vote.
type entry struct {
	Text    string  // output normalization, truncated to maxWindowRunes
	Key     string  // comparison fold of Text — what the vote groups on
	Raw     string  // as the engine read it
	Conf    float64 // [0,1]
	Frame   int64
	TsNS    int64
	Skipped bool // reused from the previous frame because the hash was unchanged
}

// Tracker is the dialogue-observation state machine for one region.
//
// It is deliberately not safe for concurrent use and does not need to be: one
// region belongs to one worker, and docs/spec/01 makes the reducer the only
// place state changes are serialized. There is no mutex, no channel and no
// timer in this file — Step is a pure function of the observation stream onto
// transitions, which is what makes the scripted timelines in the tests the same
// code path as the live runtime.
type Tracker struct {
	regionID string
	cfg      Config
	fold     *textnorm.Folder // comparison fold (FoldOptions)
	norm     *textnorm.Folder // output fold: the text a caller ranks and logs

	state    State
	lifetime uint64
	lastErr  string

	presentRun int

	entries []entry // oldest first, capped at cfg.VoteWindow

	// Reused OCR state (docs/spec/05 "suppress duplicate OCR calls on unchanged
	// region hash"). lastOCR is kept only for its text/confidence, copied into
	// the assembled string, so the capture loop's buffers stay unowned.
	lastHash    string
	haveHash    bool
	lastRaw     string
	lastOCRConf float64
	haveOCR     bool

	// Accepted text and when it last changed, on the observation's own clock.
	// text is what the caller ranks; key is what every comparison here runs on.
	text          string
	key           string
	raw           string
	conf          float64
	changed       int64
	lastTsNS      int64
	lastFrame     int64
	anchor        string // text when the current READY/STABILIZING claim was made
	anchorKey     string // comparison fold of anchor
	contradiction float64

	// readyHold blocks STABILIZING→READY on the observation that caused the
	// stabilization, so the two claims are never the same evidence twice over.
	readyHold       bool
	stabilizedFrame int64

	// Committed line, kept until the box is dismissed or advances.
	committed       string
	committedKey    string
	committedReason string
	predicted       bool
	committedFrame  int64

	stats Stats
}

// NewTracker prepares a tracker for one region. cfg's zero fields take
// DefaultConfig values.
func NewTracker(regionID string, cfg Config) *Tracker {
	return &Tracker{
		regionID: regionID,
		cfg:      cfg.normalize(),
		fold:     textnorm.NewFolder(FoldOptions()),
		norm:     textnorm.NewFolder(textnorm.Options{}),
		state:    StateAbsent,
	}
}

// RegionID reports the region this tracker belongs to.
func (t *Tracker) RegionID() string { return t.regionID }

// Config returns the effective (normalized) configuration, so a dev-mode panel
// can show what thresholds actually decided a transition.
func (t *Tracker) Config() Config { return t.cfg }

// State reports the current state.
func (t *Tracker) State() State { return t.state }

// Text is the accepted normalized text — the string the caller should rank.
// Reading it after Step gives the vote's answer, not the last frame's raw
// output, which is the difference between narrating a line and narrating a
// single bad frame of it. It carries the *output* normalization (case,
// whitespace, Unicode compatibility), not the lossier comparison fold the vote
// runs on, because internal/matcher folds on its own terms and a
// punctuation-stripped, OCR-folded string sent to it would be folded twice.
func (t *Tracker) Text() string { return t.text }

// Key is the comparison fold of Text — the form every equality, prefix and
// edit-distance test in this package actually ran on. Exposed for the developer
// inspector, which must be able to show why two spellings were treated as one
// line (docs/spec/09 "Link everything using trace_id").
func (t *Tracker) Key() string { return t.key }

// Raw is the raw text of the frame that produced the accepted text
// (docs/spec/05 "Preserve raw observed OCR and normalized text separately").
func (t *Tracker) Raw() string { return t.raw }

// Confidence is the mean OCR confidence of the frames agreeing with Text.
func (t *Tracker) Confidence() float64 { return t.conf }

// Lifetime counts box lifetimes: it increases every time the region goes from
// absent-or-dismissed to present. It is how the session reducer knows that text
// identical to the previous occurrence is nevertheless a *new* occurrence
// (docs/spec/12 "repeat NPC after menu"), without this package pretending to
// own occurrence identity.
func (t *Tracker) Lifetime() uint64 { return t.lifetime }

// PredictiveEligible reports whether a predictive commit is currently legal on
// the tracker's own evidence: revealing, predictive mode on, a prefix long
// enough to be more than a guess, and confidence above the floor. The caller
// still has to prove the *matching* half (one unique strong candidate with a
// margin) — splitting the two checks is what stops a prefix from committing on
// text evidence alone (docs/spec/05 "Predict playback policy").
func (t *Tracker) PredictiveEligible() bool {
	if t.state != StateRevealing || !t.cfg.PredictiveMode {
		return false
	}
	return runeLen(t.key) >= t.cfg.MinPredictLen && t.conf >= t.cfg.MinConfidence
}

// Stats returns a copy of the counters.
func (t *Tracker) Stats() Stats { return t.stats }

// Snapshot copies the readable state.
func (t *Tracker) Snapshot() Snapshot {
	return Snapshot{
		RegionID:        t.regionID,
		State:           t.state,
		Text:            t.text,
		Key:             t.key,
		Raw:             t.raw,
		Confidence:      t.conf,
		Lifetime:        t.lifetime,
		Committed:       t.committed,
		CommittedReason: t.committedReason,
		LastFrameSeq:    t.lastFrame,
		LastTimestampNS: t.lastTsNS,
		Predicted:       t.predicted,
		LastError:       t.lastErr,
		Window:          len(t.entries),
		SinceChange:     Duration(time.Duration(maxInt64(t.lastTsNS-t.changed, 0))),
		Contradictions:  t.contradiction,
	}
}

// Reset returns the tracker to StateAbsent, discarding the vote window and the
// committed line, without emitting transitions. It is for a boundary the state
// machine cannot observe — a profile revision change, a stop — where the
// previous box's history is about a different set of canonical texts
// (docs/spec/05 "Belief decays or resets on … pause").
func (t *Tracker) Reset() {
	t.state = StateAbsent
	t.presentRun = 0
	t.entries = nil
	t.lastHash = ""
	t.haveHash = false
	t.lastRaw = ""
	t.lastOCRConf = 0
	t.haveOCR = false
	t.text, t.raw, t.conf, t.key = "", "", 0, ""
	t.changed = 0
	t.anchor, t.anchorKey = "", ""
	t.committed, t.committedReason, t.predicted = "", "", false
	t.committedKey = ""
	t.readyHold = false
	t.stabilizedFrame = 0
	t.lastErr = ""
	t.contradiction = 0
}

// CommitRequest asks the tracker to take the READY→COMMITTED (or, in predictive
// mode, REVEALING→COMMITTED) step. The session reducer owns it: the tracker can
// say a line is *stable* but only the ranking can say it is *known*, and
// docs/spec/05 puts those two facts in different stages.
type CommitRequest struct {
	// FrameSeq / CaptureTimestampNS locate the commit in the capture timeline.
	FrameSeq           int64
	CaptureTimestampNS int64
	// Text is the normalized text being committed. If non-empty it must match
	// the tracker's accepted text: a commit for a text the tracker does not hold
	// means the caller acted on a superseded observation, which is precisely the
	// stale-work class docs/spec/01 invariant 2 exists to fence.
	Text string
	// Reason carries the matcher's reason code for the commit, and must be
	// ReasonPredictivePrefix for a commit out of StateRevealing.
	Reason string
}

// Commit applies a commit decision and returns the transition it caused.
func (t *Tracker) Commit(req CommitRequest) ([]Transition, error) {
	text := req.Text
	if text == "" {
		text = t.text
	}
	if req.Text != "" && req.Text != t.text && t.normKey(req.Text) != t.key {
		// A commit for a text this tracker does not hold means the caller acted
		// on a superseded observation — the stale-work class docs/spec/01
		// invariant 2 exists to fence.
		t.stats.CommitRejected++
		return nil, fmt.Errorf("%w: committing %q but the tracker holds %q", ErrNotCommittable, truncate(req.Text), truncate(t.text))
	}
	switch t.state {
	case StateReady:
		// The normal path: full-line readiness (docs/spec/05 "Default
		// conservative full-line readiness").
	case StateRevealing:
		if !t.cfg.PredictiveMode {
			t.stats.CommitRejected++
			return nil, fmt.Errorf("%w: predictive commit requested while predictive mode is off", ErrNotCommittable)
		}
		if req.Reason != ReasonPredictivePrefix {
			t.stats.CommitRejected++
			return nil, fmt.Errorf("%w: commit while revealing requires reason %s, got %q", ErrNotCommittable, ReasonPredictivePrefix, req.Reason)
		}
		if runeLen(t.key) < t.cfg.MinPredictLen {
			t.stats.CommitRejected++
			return nil, fmt.Errorf("%w: prefix of %d runes is shorter than MinPredictLen %d", ErrNotCommittable, runeLen(t.key), t.cfg.MinPredictLen)
		}
		if t.conf < t.cfg.MinConfidence {
			t.stats.CommitRejected++
			return nil, fmt.Errorf("%w: prefix confidence %.3f is below MinConfidence %.3f", ErrNotCommittable, t.conf, t.cfg.MinConfidence)
		}
	default:
		t.stats.CommitRejected++
		return nil, fmt.Errorf("%w: state %s cannot commit", ErrNotCommittable, t.state)
	}
	t.committed = text
	t.committedKey = t.key
	t.committedReason = req.Reason
	t.predicted = t.state == StateRevealing
	t.committedFrame = req.FrameSeq
	t.stats.Commits++
	return []Transition{t.moveTo(StateCommitted, req.Reason, req.FrameSeq, req.CaptureTimestampNS)}, nil
}

// Step consumes one observation and reports the transitions it caused.
//
// The order of work is load-bearing: invalidate, then dismiss, then presence
// bookkeeping, then OCR reuse, then the vote, then state progression. Running
// the vote before the OCR reuse would read a frame that deliberately supplied no
// new evidence as an empty line, and the state progression before the vote would
// compare the previous frame's text against the previous frame's thresholds.
func (t *Tracker) Step(o Observation) []Transition {
	t.stats.Observations++
	var out []Transition

	if err := o.Validate(); err != nil {
		t.stats.Invalid++
		if t.state != StateError {
			out = append(out, t.moveTo(StateError, ReasonInvalidObservation, o.FrameSeq, o.CaptureTimestampNS))
		}
		t.lastErr = err.Error()
		return out
	}
	if o.RegionID != t.regionID {
		// Not a corrupted frame, just addressed to the wrong box: two trackers
		// wired backwards would otherwise vote a menu's text and a subtitle's
		// text into one line and call the mixture stable.
		t.stats.Invalid++
		msg := fmt.Sprintf("observation is for region %q, tracker is for %q", o.RegionID, t.regionID)
		if t.state != StateAbsent && t.state != StateError {
			out = append(out, t.moveTo(StateError, ReasonInvalidObservation, o.FrameSeq, o.CaptureTimestampNS))
		}
		t.lastErr = msg
		return out
	}

	if t.state == StateError {
		out = append(out, t.moveTo(StateAbsent, ReasonRecovered, o.FrameSeq, o.CaptureTimestampNS))
		t.lastErr = ""
	}

	if !o.RegionPresent {
		// ANY→DISMISSED. The vote window and the accepted text are dropped: the
		// next box is a new occurrence even if it says the same words
		// (docs/spec/05 "ANY→DISMISSED: region vanishes; policy fade/cancel;
		// preserve recent graph context" — the *graph* context survives, the
		// pixel history does not).
		t.presentRun = 0
		if t.state != StateAbsent && t.state != StateDismissed {
			out = append(out, t.moveTo(StateDismissed, ReasonRegionVanished, o.FrameSeq, o.CaptureTimestampNS))
		}
		t.clearBox()
		return out
	}

	// --- presence -------------------------------------------------------
	if t.state == StateAbsent || t.state == StateDismissed {
		if t.presentRun == 0 {
			// First present frame of a new box lifetime. The counter moves once per
			// lifetime, not once per frame spent waiting out AppearFrames, because
			// the session uses it to decide that text identical to the previous
			// occurrence is nevertheless a *new* occurrence (docs/spec/12 "repeat
			// NPC after menu").
			t.lifetime++
			t.clearBox()
			t.readyHold = false
			t.stabilizedFrame = 0
		}
		t.presentRun++
		if t.presentRun >= t.cfg.AppearFrames {
			out = append(out, t.moveTo(StateAppearing, ReasonRegionDetected, o.FrameSeq, o.CaptureTimestampNS))
		}
	} else {
		t.presentRun++
	}

	// --- OCR reuse ------------------------------------------------------
	newEvidence := o.OCR != nil
	if newEvidence {
		if o.RegionHash != "" && t.haveHash && o.RegionHash == t.lastHash {
			// The caller spent an OCR call the hash would have saved. Counting
			// it is the only way the waste is visible in a benchmark.
			t.stats.OCRRedundantSameHash++
		}
		raw, conf := rawText(o.OCR, t.cfg.MinWordConfidence)
		t.lastRaw, t.lastOCRConf, t.haveOCR = raw, conf, true
		if raw == "" {
			t.stats.EmptyReads++
		}
	} else if o.RegionHash != "" && t.haveHash && o.RegionHash == t.lastHash && t.haveOCR {
		// Nothing in the region changed, so the previous read is still the
		// current text — reuse it and record the saved call.
		t.stats.OCRSkippedSameHash++
	}
	if o.RegionHash != "" {
		t.lastHash, t.haveHash = o.RegionHash, true
	}
	if t.haveOCR {
		t.appendEntry(o, t.lastRaw, t.lastOCRConf, !newEvidence)
	}

	t.lastFrame, t.lastTsNS = o.FrameSeq, o.CaptureTimestampNS

	// --- vote -----------------------------------------------------------
	if t.changed == 0 {
		// Nothing has been accepted yet in this lifetime, so the stability clock
		// starts at this frame rather than at the epoch: an empty box is "stable
		// and empty", not "stable for an hour".
		t.changed = o.CaptureTimestampNS
	}
	v := vote(t.entries)
	accepted, acceptedKey, conf := v.Text, v.Key, v.Confidence
	// A trailing run that has held for AdvanceRunFrames frames overrides the
	// window majority. Without this, a page advance is only seen once the older
	// page's text has aged out of the window, and at 20 rapid advances
	// (docs/spec/12) every commit would lag a page behind. With it, one noisy
	// frame still cannot beat the majority, which is the flicker test. A
	// committed *predictive* line may also be overridden by a bare extension:
	// the commit claimed the message was complete, so more text on screen
	// falsifies that claim (docs/spec/05 "if prefix turns out wrong").
	if tailKey, run := tailRun(t.entries); run >= t.cfg.AdvanceRunFrames && tailKey != acceptedKey {
		overrides := acceptedKey == "" ||
			isMaterial(acceptedKey, tailKey, t.cfg.ChangeThreshold) ||
			(t.state == StateCommitted && t.predicted && isExtension(acceptedKey, tailKey))
		if overrides {
			accepted, acceptedKey = newestForKey(t.entries, tailKey), tailKey
			conf = confidenceOf(t.entries, tailKey)
		}
	}
	if acceptedKey != t.key {
		t.key = acceptedKey
		t.text = accepted
		t.changed = o.CaptureTimestampNS
		t.stats.TextChanges++
	}
	t.raw = t.textRaw(t.key)
	t.conf = conf
	t.contradiction = t.contradictionRatio(t.key)

	// --- state progression ---------------------------------------------
	out = append(out, t.progress(o)...)
	return out
}

// progress applies the transition rules of docs/spec/05 in order, allowing a
// chain within one frame (a box that appears fully rendered really is revealed
// on the frame it appears, and a page advance is ADVANCING→REVEALING at once).
//
// One step is deliberately not chained: STABILIZING→READY. The stabilization
// claim is "the text has stopped changing"; the ready claim is "and nothing in
// the window contradicts it, at a confidence worth speaking". If one frame could
// assert both, the contradiction check would be the stability check read twice
// and the vote window would never have had a chance to disagree. READY
// therefore costs at least one frame of STABILIZING — which is also the frame
// boundary at which the session reducer ranks the stabilizing text and decides
// whether to speak it (docs/spec/01 "Candidate lookup is local and deadline
// constrained", docs/spec/05 "STABILIZING→READY: … no active contradictory
// observations").
func (t *Tracker) progress(o Observation) []Transition {
	var out []Transition
	elapsed := time.Duration(o.CaptureTimestampNS - t.changed)
	if elapsed < 0 {
		// The source clock went backwards. Not on a monotonic clock, but
		// "dolphin-emulator" and a rewound VM clock are real, and the safe
		// reading is "we have no idea how long this text has been stable".
		elapsed = 0
		t.changed = o.CaptureTimestampNS
	}
	stable := elapsed >= t.cfg.StableWindow
	if t.readyHold && o.FrameSeq != t.stabilizedFrame {
		t.readyHold = false
	}

	// A material change to a line already committed is the player advancing the
	// page: cancel the old speech and start revealing the new one.
	if t.state == StateCommitted {
		kind, _ := classify(t.committedKey, t.key, t.cfg.ChangeThreshold)
		switch {
		case kind == changeMaterial || kind == changeTruncation:
			reason := ReasonMaterialChange
			if t.predicted {
				// The committed text turned out wrong: docs/spec/05 "Predictive
				// playback policy" calls for marking the misfire, and the
				// session turns that into a fade plus a fixture.
				reason = ReasonPredictionMisfire
			}
			out = append(out, t.moveTo(StateAdvancing, reason, o.FrameSeq, o.CaptureTimestampNS))
			out = append(out, t.moveTo(StateRevealing, ReasonNewTextRevealing, o.FrameSeq, o.CaptureTimestampNS))
			t.setAnchor()
		case t.predicted && kind == changeExtension:
			// A predictive commit claims the prefix was a *complete* message, so
			// more text on screen falsifies it — even though for a normally
			// committed line an extension is explicitly not a new utterance
			// (docs/spec/05 "A typewriter prefix is not a new distinct
			// utterance"). The distinction is which claim was made, not which
			// pixels moved.
			out = append(out, t.moveTo(StateAdvancing, ReasonPredictionMisfire, o.FrameSeq, o.CaptureTimestampNS))
			out = append(out, t.moveTo(StateRevealing, ReasonNewTextRevealing, o.FrameSeq, o.CaptureTimestampNS))
			t.setAnchor()
		case kind == changeJitter:
			// The screen text drifted by less than a material change. The
			// occurrence is already committed — the event was emitted and audio
			// may be playing — so the tracker does not rewrite history. It counts
			// the drift instead, which is the visible trace of a StableWindow
			// shorter than a typewriter's pause.
			t.stats.CommittedDrift++
		}
		return out
	}

	// A material change to text that was stable but not yet committed means it
	// was not final; fall back to revealing rather than committing a line that
	// is still moving.
	if t.state == StateReady || t.state == StateStabilizing {
		if isMaterial(t.anchorKey, t.key, t.cfg.ChangeThreshold) {
			out = append(out, t.moveTo(StateRevealing, ReasonContradictoryText, o.FrameSeq, o.CaptureTimestampNS))
			t.setAnchor()
			return out
		}
		if t.state == StateReady && t.key != t.anchorKey {
			// The line grew by an extension while it waited to be committed: same
			// utterance, longer text. Re-anchor so the session commits what is on
			// screen rather than what was on screen three frames ago.
			t.setAnchor()
		}
	}

	if t.state == StateAppearing && t.key != "" {
		out = append(out, t.moveTo(StateRevealing, ReasonTextRevealing, o.FrameSeq, o.CaptureTimestampNS))
		t.setAnchor()
	}

	if t.state == StateRevealing && t.key != "" && stable {
		out = append(out, t.moveTo(StateStabilizing, ReasonStableWindow, o.FrameSeq, o.CaptureTimestampNS))
		t.setAnchor()
		t.readyHold = true
		t.stabilizedFrame = o.FrameSeq
	}

	if t.state == StateStabilizing {
		if t.contradiction > t.cfg.MaxContradictionRatio {
			// Recent frames disagree with the accepted text: the line is still
			// moving or the video is noisy, and a confidence check passed on top
			// of a contradiction would be a confident wrong narration.
			out = append(out, t.moveTo(StateRevealing, ReasonContradictoryText, o.FrameSeq, o.CaptureTimestampNS))
			t.setAnchor()
		} else if !t.readyHold && t.conf >= t.cfg.MinConfidence && stable {
			out = append(out, t.moveTo(StateReady, ReasonEvidenceSufficient, o.FrameSeq, o.CaptureTimestampNS))
			t.setAnchor()
		}
	}
	return out
}

// setAnchor records the text the current stability claim is about.
func (t *Tracker) setAnchor() { t.anchor, t.anchorKey = t.text, t.key }

// contradictionRatio measures the vote weight that disagrees with the accepted
// text, recomputed against the accepted text rather than the vote's own winner
// so that a trailing-run override is reported honestly: if the tracker accepted
// a new page early, the frames still describing the old page *are*
// contradictions of what it now claims, and STABILIZING must not wave that
// through.
func (t *Tracker) contradictionRatio(acceptedKey string) float64 {
	if len(t.entries) == 0 {
		return 0
	}
	total, contra := 0.0, 0.0
	for _, e := range t.entries {
		w := 0.5 + unit(e.Conf)
		total += w
		if e.Key != acceptedKey && !consistent(acceptedKey, e.Key) {
			contra += w
		}
	}
	if total == 0 {
		return 0
	}
	return contra / total
}

// textRaw returns the raw text associated with an accepted comparison key — the
// latest frame that read it, so events can show what the engine actually saw
// rather than the folded comparison form.
func (t *Tracker) textRaw(acceptedKey string) string {
	for i := len(t.entries) - 1; i >= 0; i-- {
		if t.entries[i].Key == acceptedKey {
			return t.entries[i].Raw
		}
	}
	if acceptedKey == "" {
		return ""
	}
	return acceptedKey
}

func (t *Tracker) appendEntry(o Observation, raw string, conf float64, reused bool) {
	// Two folds of one read: Text is what the caller ranks and logs, Key is what
	// the vote compares. Keeping both is what lets the tracker ride out "the ship
	// sails" vs "the ship saile" without ever *reporting* the mangled form as the
	// line (docs/spec/05 "Preserve raw observed OCR and normalized text
	// separately").
	norm := boundedText(normalizeText(t.norm, raw))
	key := boundedText(normalizeText(t.fold, raw))
	t.entries = append(t.entries, entry{
		Text: norm, Key: key, Raw: raw, Conf: unit(conf), Frame: o.FrameSeq, TsNS: o.CaptureTimestampNS, Skipped: reused,
	})
	if max := t.cfg.VoteWindow; len(t.entries) > max {
		// Slide rather than reallocate every frame: this slice is ≤ 32 entries
		// and this runs on every frame of a 4-core appliance.
		trimmed := make([]entry, max)
		copy(trimmed, t.entries[len(t.entries)-max:])
		t.entries = trimmed
	}
}

// normKey folds a caller-supplied text into this tracker's comparison form, so a
// Commit whose text differs from the accepted one only by punctuation or an
// OCR-confusable glyph is not refused as a stale claim.
func (t *Tracker) normKey(s string) string {
	return boundedText(normalizeText(t.fold, s))
}

// boundedText caps the stored form so a pathological OCR result cannot make the
// vote window's memory proportional to its input.
func boundedText(s string) string {
	if n := runeLen(s); n > maxWindowRunes {
		return takeRunes(s, maxWindowRunes)
	}
	return s
}

func (t *Tracker) clearBox() {
	t.entries = nil
	t.text, t.raw, t.conf, t.key = "", "", 0, ""
	t.changed = 0
	t.anchor, t.anchorKey = "", ""
	t.committed, t.committedKey, t.committedReason, t.predicted = "", "", "", false
	t.contradiction = 0
}

func (t *Tracker) moveTo(to State, reason string, frame, ts int64) Transition {
	tr := Transition{From: t.state, To: to, Reason: reason, FrameSeq: frame, CaptureTimestampNS: ts, Text: t.textAfter(to)}
	t.state = to
	t.stats.Transitions++
	return tr
}

// textAfter reports the text a transition should carry. For the transitions
// that leave a committed line behind, the caller's question is "what is on
// screen now", which is the accepted text, not the one that was spoken.
func (t *Tracker) textAfter(State) string { return t.text }

func truncate(s string) string {
	if runeLen(s) <= 48 {
		return s
	}
	return takeRunes(s, 48) + "…"
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
