package recognition

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// driver advances one tracker on a scripted timeline with fake timestamps. It
// exists so every test in this file states a *timeline* rather than a call
// sequence, and so no test ever depends on how long the machine running it
// takes: the tracker reads time only from CaptureTimestampNS (docs/spec/01
// invariant 1), which is what makes a 200 ms typewriter reproducible in CI.
type driver struct {
	t   *testing.T
	tr  *Tracker
	seq int64
	ts  int64
	// FrameInterval is the capture gap between steps.
	FrameInterval time.Duration
	// Clock names the source clock under test.
	Clock string
}

func newDriver(t *testing.T, cfg Config) *driver {
	return &driver{
		t:             t,
		tr:            NewTracker("bottom", cfg),
		ts:            time.Hour.Nanoseconds(), // a clock that is not "the epoch"
		FrameInterval: 200 * time.Millisecond,
		Clock:         "monotonic",
	}
}

// frame feeds one text read with a fresh region hash (i.e. a real OCR pass).
func (d *driver) frame(text string, conf float64) []Transition {
	d.seq++
	d.ts += int64(d.FrameInterval)
	return d.tr.Step(Observation{
		FrameSeq:           d.seq,
		CaptureTimestampNS: d.ts,
		SourceClock:        d.Clock,
		RegionID:           "bottom",
		RegionPresent:      true,
		RegionHash:         fmt.Sprintf("h-%d", d.seq),
		OCR:                ocrResult(text, conf),
	})
}

// reuse feeds a frame whose region hash is unchanged, so no OCR ran — the
// caller passed nil and the hash proves why.
func (d *driver) reuse(hash string) []Transition {
	d.seq++
	d.ts += int64(d.FrameInterval)
	return d.tr.Step(Observation{
		FrameSeq:           d.seq,
		CaptureTimestampNS: d.ts,
		SourceClock:        d.Clock,
		RegionID:           "bottom",
		RegionPresent:      true,
		RegionHash:         hash,
	})
}

// gap feeds a frame with no new OCR evidence at a new hash (an OCR failure the
// caller chose not to report as text).
func (d *driver) emptyRead() []Transition {
	d.seq++
	d.ts += int64(d.FrameInterval)
	return d.tr.Step(Observation{
		FrameSeq:           d.seq,
		CaptureTimestampNS: d.ts,
		SourceClock:        d.Clock,
		RegionID:           "bottom",
		RegionPresent:      true,
		RegionHash:         fmt.Sprintf("h-%d", d.seq),
		OCR:                ocrResult("", 0),
	})
}

// absent feeds a frame where the detector no longer sees a box.
func (d *driver) absent() []Transition {
	d.seq++
	d.ts += int64(d.FrameInterval)
	return d.tr.Step(Observation{
		FrameSeq:           d.seq,
		CaptureTimestampNS: d.ts,
		SourceClock:        d.Clock,
		RegionID:           "bottom",
		RegionPresent:      false,
	})
}

// appear drives the minimum number of frames this config needs to leave ABSENT,
// then stops. A test about commitment policy should not have to restate the
// detection bookkeeping, but it must not be able to skip it either: with
// AppearFrames = 2 the box is DETECTED on frame 2 and REVEALING from that frame
// on, and docs/spec/05 forbids reading frame 1 as a box at all.
func (d *driver) appear(text string, conf float64) []Transition {
	var trs []Transition
	for i := 0; i < d.tr.Config().AppearFrames; i++ {
		trs = d.frame(text, conf)
	}
	return trs
}

// raw feeds a fully specified observation, for the tests that must break the
// input contract on purpose.
func (d *driver) raw(o Observation) []Transition {
	d.seq++
	if o.FrameSeq == 0 {
		o.FrameSeq = d.seq
	}
	if o.CaptureTimestampNS == 0 {
		d.ts += int64(d.FrameInterval)
		o.CaptureTimestampNS = d.ts
	}
	return d.tr.Step(o)
}

func ocrResult(text string, conf float64) *ocr.Result {
	res := &ocr.Result{Text: text, MeanConf: conf, Engine: "tesseract"}
	if text == "" {
		return res
	}
	for _, word := range strings.Fields(text) {
		res.Words = append(res.Words, ocr.Word{Text: word, Conf: conf})
	}
	return res
}

func reasons(trs []Transition) string {
	out := make([]string, len(trs))
	for i, tr := range trs {
		out[i] = tr.String()
	}
	return strings.Join(out, " | ")
}

func lastReason(trs []Transition) string {
	if len(trs) == 0 {
		return ""
	}
	return trs[len(trs)-1].Reason
}

func containsReason(trs []Transition, reason string) bool {
	for _, tr := range trs {
		if tr.Reason == reason {
			return true
		}
	}
	return false
}

func containsPair(trs []Transition, from, to State) bool {
	for _, tr := range trs {
		if tr.From == from && tr.To == to {
			return true
		}
	}
	return false
}

// committedConfig is the conservative default with the timing spread out enough
// that a test can distinguish "still typing" from "finished" frame by frame.
func committedConfig() Config {
	return Config{
		AppearFrames:     2,
		StableWindow:     400 * time.Millisecond,
		MinConfidence:    0.5,
		VoteWindow:       5,
		AdvanceRunFrames: 2,
	}
}

// TestAppearsRevealsStabilizesReadyCommitOnce is the happy path of docs/spec/05
// "Dialogue observation state machine", and the docs/spec/16 M2 gate "text state
// machine": a box appears, types out over six frames, stabilizes, reaches READY,
// and a commit takes it to COMMITTED exactly once.
func TestAppearsRevealsStabilizesReadyCommitOnce(t *testing.T) {
	d := newDriver(t, committedConfig())

	trs := d.frame("THE SHIP", 0.9) // frame 1: AppearFrames=2, so still absent
	if len(trs) != 0 {
		t.Fatalf("first present frame with AppearFrames=2 emitted %s, want nothing", reasons(trs))
	}
	if got := d.tr.State(); got != StateAbsent {
		t.Fatalf("state after one frame = %s, want absent", got)
	}

	reveal := []string{"THE SHIP", "THE SHIP S", "THE SHIP SAI", "THE SHIP SAILS", "THE SHIP SAILS", "THE SHIP SAILS"}
	sawRevealing, sawStabilizing, sawReady := false, false, false
	for _, text := range reveal {
		trs := d.frame(text, 0.9)
		if len(trs) == 0 {
			continue
		}
		if d.tr.State() == StateRevealing {
			sawRevealing = true
		}
		if d.tr.State() == StateStabilizing {
			sawStabilizing = true
		}
		if d.tr.State() == StateReady {
			sawReady = true
		}
	}
	if !sawRevealing || !sawStabilizing || !sawReady {
		t.Fatalf("state chain reached %s (revealing=%v stabilizing=%v ready=%v)", d.tr.State(), sawRevealing, sawStabilizing, sawReady)
	}
	if got, want := d.tr.Text(), "the ship sails"; got != want {
		t.Fatalf("accepted text = %q, want %q", got, want)
	}
	if d.tr.State() != StateReady {
		t.Fatalf("final state = %s, want ready", d.tr.State())
	}

	trs, err := d.tr.Commit(CommitRequest{FrameSeq: d.seq, CaptureTimestampNS: d.ts, Text: d.tr.Text(), Reason: ReasonCommitted})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !containsPair(trs, StateReady, StateCommitted) {
		t.Fatalf("commit transitions = %s, want ready->committed", reasons(trs))
	}

	// 50 more frames of the same OCR must not move the state machine at all:
	// docs/spec/16 M2 "repeat observation doesn't create duplicate speech".
	for i := 0; i < 50; i++ {
		if trs := d.frame("THE SHIP SAILS", 0.9); len(trs) != 0 {
			t.Fatalf("repeat frame %d emitted %s, want no transition", i, reasons(trs))
		}
	}
	if got := d.tr.Stats().Commits; got != 1 {
		t.Fatalf("commits = %d, want 1", got)
	}
	// A second commit is refused by the state machine rather than producing a
	// second occurrence.
	if _, err := d.tr.Commit(CommitRequest{FrameSeq: d.seq, Text: d.tr.Text()}); err == nil {
		t.Fatal("second commit succeeded, want ErrNotCommittable")
	}
}

// TestTypewriterPrefixIsNotANewUtterance is docs/spec/05 "A typewriter prefix is
// not a new distinct utterance": every intermediate read must be an extension,
// never a material change, so a revealing box never looks like an advance.
func TestTypewriterPrefixIsNotANewUtterance(t *testing.T) {
	d := newDriver(t, committedConfig())
	full := "THE STORM IS COMING"
	for i := 4; i <= len(full); i++ {
		trs := d.frame(full[:i], 0.85)
		if containsReason(trs, ReasonMaterialChange) || containsPair(trs, StateCommitted, StateAdvancing) {
			t.Fatalf("prefix %q caused %s; a typewriter extension is not a new utterance", full[:i], reasons(trs))
		}
	}
	if got, want := d.tr.Text(), strings.ToLower("the storm is coming"); got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

// TestFlickerInsideStableBoxDoesNotAdvance: one noisy OCR frame in a committed
// box must not cancel speech (docs/spec/12 failure injection, "Dialogue and
// graph"). The temporal vote, and the requirement that a competing text hold for
// AdvanceRunFrames frames before it overrides, are what make that true.
func TestFlickerInsideStableBoxDoesNotAdvance(t *testing.T) {
	d := newDriver(t, committedConfig())
	for i := 0; i < 8; i++ {
		d.frame("THE SHIP SAILS SOUTH", 0.9)
	}
	if _, err := d.tr.Commit(CommitRequest{FrameSeq: d.seq, Text: d.tr.Text()}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	trs := d.frame("A WHOLE DIFFERENT LINE", 0.9) // the flicker
	if containsPair(trs, StateCommitted, StateAdvancing) {
		t.Fatalf("single noisy frame advanced the box: %s", reasons(trs))
	}
	if got, want := d.tr.Text(), "the ship sails south"; got != want {
		t.Fatalf("flicker changed the accepted text to %q, want %q", got, want)
	}
	for i := 0; i < 3; i++ {
		if trs := d.frame("THE SHIP SAILS SOUTH", 0.9); containsPair(trs, StateCommitted, StateAdvancing) {
			t.Fatalf("post-flicker frame advanced the box: %s", reasons(trs))
		}
	}
	if st := d.tr.Stats(); st.Commits != 1 {
		t.Fatalf("commits after flicker = %d, want 1", st.Commits)
	}
}

// TestMaterialChangeAdvances is COMMITTED→ADVANCING: the player advanced the
// page, so the box's text changes to something that is not an extension.
func TestMaterialChangeAdvances(t *testing.T) {
	d := newDriver(t, committedConfig())
	for i := 0; i < 8; i++ {
		d.frame("PAGE ONE TEXT", 0.9)
	}
	if _, err := d.tr.Commit(CommitRequest{FrameSeq: d.seq, Text: d.tr.Text()}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	trs := d.frame("PAGE TWO ENTIRELY", 0.9)
	if len(trs) != 0 {
		t.Fatalf("first frame of a new page emitted %s, want confirmation first", reasons(trs))
	}
	trs = d.frame("PAGE TWO ENTIRELY", 0.9)
	if !containsPair(trs, StateCommitted, StateAdvancing) || !containsPair(trs, StateAdvancing, StateRevealing) {
		t.Fatalf("advance transitions = %s, want committed->advancing->revealing", reasons(trs))
	}
	if !containsReason(trs, ReasonMaterialChange) {
		t.Fatalf("advance reason = %s, want material_text_change", reasons(trs))
	}
	if got, want := d.tr.Text(), "page two entirely"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

// TestUnchangedHashReusesPreviousOCR covers docs/spec/05 "suppress duplicate OCR
// calls on unchanged region hash" from the tracker's side: frames that carry no
// OCR result keep the accepted text alive and are counted, and an OCR result
// delivered for a hash already read is counted as wasted work.
func TestUnchangedHashReusesPreviousOCR(t *testing.T) {
	d := newDriver(t, committedConfig())
	d.frame("STATIC LINE", 0.9)
	d.frame("STATIC LINE", 0.9)
	for i := 0; i < 6; i++ {
		trs := d.reuse("h-2")
		if len(trs) != 0 && !containsPair(trs, StateRevealing, StateStabilizing) && !containsPair(trs, StateStabilizing, StateReady) {
			t.Fatalf("reused frame emitted unexpected %s", reasons(trs))
		}
	}
	if got := d.tr.Stats().OCRSkippedSameHash; got != 6 {
		t.Fatalf("ocr_skipped_same_hash = %d, want 6", got)
	}
	if got, want := d.tr.Text(), "static line"; got != want {
		t.Fatalf("text after reuse = %q, want %q (the previous read must survive a no-OCR frame)", got, want)
	}

	// The same hash arriving *with* an OCR result is redundant work upstream.
	d.seq++
	d.ts += int64(d.FrameInterval)
	d.tr.Step(Observation{
		FrameSeq: d.seq, CaptureTimestampNS: d.ts, SourceClock: d.Clock,
		RegionID: "bottom", RegionPresent: true, RegionHash: "h-2", OCR: ocrResult("STATIC LINE", 0.9),
	})
	if got := d.tr.Stats().OCRRedundantSameHash; got != 1 {
		t.Fatalf("ocr_redundant_same_hash = %d, want 1", got)
	}
}

// TestPredictiveCommitIsOptIn is docs/spec/05 "REVEALING→COMMITTED: only
// predictive mode" plus the MinPredictLen floor.
func TestPredictiveCommitIsOptIn(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		d := newDriver(t, committedConfig())
		d.appear("THE SHIP SAILS", 0.95)
		if d.tr.State() != StateRevealing && d.tr.State() != StateAppearing {
			t.Fatalf("state = %s, want still revealing", d.tr.State())
		}
		_, err := d.tr.Commit(CommitRequest{FrameSeq: d.seq, Text: d.tr.Text(), Reason: ReasonPredictivePrefix})
		if err == nil {
			t.Fatal("predictive commit succeeded with PredictiveMode off")
		}
		if got := d.tr.Stats().CommitRejected; got != 1 {
			t.Fatalf("commit_rejected = %d, want 1", got)
		}
	})

	t.Run("on_but_short", func(t *testing.T) {
		cfg := committedConfig()
		cfg.PredictiveMode = true
		cfg.MinPredictLen = 12
		d := newDriver(t, cfg)
		d.appear("THE SHIP", 0.95)
		if d.tr.State() != StateRevealing {
			t.Fatalf("state = %s, want revealing", d.tr.State())
		}
		if _, err := d.tr.Commit(CommitRequest{FrameSeq: d.seq, Text: d.tr.Text(), Reason: ReasonPredictivePrefix}); err == nil {
			t.Fatal("commit succeeded on a prefix shorter than MinPredictLen")
		}
	})

	t.Run("on_and_long_enough", func(t *testing.T) {
		cfg := committedConfig()
		cfg.PredictiveMode = true
		cfg.MinPredictLen = 10
		d := newDriver(t, cfg)
		d.appear("THE SHIP SAILS", 0.95)
		if !d.tr.PredictiveEligible() {
			t.Fatalf("PredictiveEligible false in state %s with text %q", d.tr.State(), d.tr.Text())
		}
		trs, err := d.tr.Commit(CommitRequest{FrameSeq: d.seq, Text: d.tr.Text(), Reason: ReasonPredictivePrefix})
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if !containsPair(trs, StateRevealing, StateCommitted) {
			t.Fatalf("transitions = %s, want revealing->committed", reasons(trs))
		}
		if !d.tr.Snapshot().Predicted {
			t.Fatal("snapshot does not mark the commit as predictive")
		}
	})

	t.Run("on_wrong_reason", func(t *testing.T) {
		cfg := committedConfig()
		cfg.PredictiveMode = true
		cfg.MinPredictLen = 4
		d := newDriver(t, cfg)
		d.appear("THE SHIP SAILS", 0.95)
		if _, err := d.tr.Commit(CommitRequest{FrameSeq: d.seq, Text: d.tr.Text(), Reason: ReasonCommitted}); err == nil {
			t.Fatal("commit while revealing succeeded without the PREDICTIVE_PREFIX reason")
		}
	})
}

// TestPredictionMisfireIsLabelled: when a committed prefix turns out to be wrong,
// the transition says so, because docs/spec/05 requires the fade to be marked
// and a fixture to be stored — which the session cannot do unless it is told.
func TestPredictionMisfireIsLabelled(t *testing.T) {
	cfg := committedConfig()
	cfg.PredictiveMode = true
	cfg.MinPredictLen = 8
	d := newDriver(t, cfg)
	d.appear("THE SHIP", 0.95)
	if _, err := d.tr.Commit(CommitRequest{FrameSeq: d.seq, Text: d.tr.Text(), Reason: ReasonPredictivePrefix}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	d.frame("THE SHIP SANK", 0.95)
	trs := d.frame("THE SHIP SANK", 0.95)
	if !containsReason(trs, ReasonPredictionMisfire) {
		t.Fatalf("transitions = %s, want prediction_misfire", reasons(trs))
	}
}

// TestDismissThenReappearIsANewLifetime: the same words in a box that vanished and
// came back are a new occurrence (docs/spec/12 "repeat NPC after menu"). The
// tracker's job is only to say the lifetime changed; the occurrence id is the
// session's.
func TestDismissThenReappearIsANewLifetime(t *testing.T) {
	d := newDriver(t, committedConfig())
	for i := 0; i < 8; i++ {
		d.frame("HELDO THE BRAVE", 0.9)
	}
	first := d.tr.Lifetime()
	trs := d.absent()
	if !containsPair(trs, StateCommitted, StateDismissed) && !containsPair(trs, StateReady, StateDismissed) {
		t.Fatalf("dismiss transitions = %s, want ...->dismissed", reasons(trs))
	}
	if got := d.tr.Text(); got != "" {
		t.Fatalf("text after dismissal = %q, want empty", got)
	}
	for i := 0; i < 8; i++ {
		d.frame("HELDO THE BRAVE", 0.9)
	}
	if d.tr.Lifetime() != first+1 {
		t.Fatalf("lifetime = %d, want %d (a reappearing box is a new lifetime)", d.tr.Lifetime(), first+1)
	}
	if got := d.tr.Stats().Commits; got != 0 {
		t.Fatalf("the tracker committed %d times on its own; only Commit may do that", got)
	}
}

// TestInvalidObservationEntersErrorAndRecovers: the invariant-1 fields are not
// advisory, and a tracker that received a malformed frame must say so rather than
// quietly treat it as an empty box.
func TestInvalidObservationEntersErrorAndRecovers(t *testing.T) {
	d := newDriver(t, committedConfig())
	d.frame("SOME TEXT", 0.9)

	trs := d.raw(Observation{RegionID: "bottom", RegionPresent: true, SourceClock: "monotonic"})
	if !containsPair(trs, StateAppearing, StateError) && !containsReason(trs, ReasonInvalidObservation) {
		t.Fatalf("malformed observation gave %s, want ...->error", reasons(trs))
	}
	if got := d.tr.Stats().Invalid; got != 1 {
		t.Fatalf("invalid = %d, want 1", got)
	}
	if d.tr.Snapshot().LastError == "" {
		t.Fatal("snapshot carries no explanation for the error state")
	}

	trs = d.frame("SOME TEXT", 0.9)
	if !containsReason(trs, ReasonRecovered) {
		t.Fatalf("recovery transitions = %s, want error->absent %s", reasons(trs), ReasonRecovered)
	}
	if d.tr.State() == StateError {
		t.Fatal("still in error after a well-formed frame")
	}

	// A nil OCR with no hash is the other uninterpretable case: it could be
	// "unchanged" or "the engine died", and guessing wrong narrates silence.
	if err := (Observation{RegionID: "bottom", RegionPresent: true, SourceClock: "m", CaptureTimestampNS: 1, OCR: nil, RegionHash: ""}).Validate(); err == nil {
		t.Fatal("nil OCR with no region hash validated as sound")
	}
}

// TestWrongRegionIsRefused keeps two boxes from voting into one line.
func TestWrongRegionIsRefused(t *testing.T) {
	d := newDriver(t, committedConfig())
	d.raw(Observation{
		FrameSeq: 1, CaptureTimestampNS: d.ts + 1, SourceClock: d.Clock,
		RegionID: "choices", RegionPresent: true, RegionHash: "x", OCR: ocrResult("A MENU", 0.9),
	})
	if got := d.tr.Stats().Invalid; got != 1 {
		t.Fatalf("invalid = %d, want 1", got)
	}
	if got := d.tr.Text(); got != "" {
		t.Fatalf("tracker adopted another region's text %q", got)
	}
}

// TestEmptyReadDoesNotAdvance: a frame that reads nothing is missing evidence,
// not an empty line, and must never be mistaken for the player advancing.
func TestEmptyReadDoesNotAdvance(t *testing.T) {
	d := newDriver(t, committedConfig())
	for i := 0; i < 8; i++ {
		d.frame("A STABLE LINE", 0.9)
	}
	if _, err := d.tr.Commit(CommitRequest{FrameSeq: d.seq, Text: d.tr.Text()}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for i := 0; i < 3; i++ {
		trs := d.emptyRead()
		if containsPair(trs, StateCommitted, StateAdvancing) {
			t.Fatalf("empty read %d advanced the box: %s", i, reasons(trs))
		}
	}
}

// TestStepIsDeterministicOnReplay runs the same timeline through two trackers and
// requires identical transcripts, the property docs/spec/12 demands of ranking and
// which the whole fixture-replay strategy depends on.
func TestStepIsDeterministicOnReplay(t *testing.T) {
	timeline := []struct {
		text string
		conf float64
	}{
		{"ONE", 0.6}, {"ONE T", 0.6}, {"ONE TWO", 0.7}, {"ONE TWO THREE", 0.8},
		{"ONE TWO THREE", 0.55}, {"ONE TWO THREE", 0.9}, {"", 0}, {"ONE TWO THREE", 0.9},
		{"ONE TWO FOUR", 0.9}, {"ONE TWO FOUR", 0.9}, {"ONE TWO FOUR MORE", 0.9},
		{"ONE TWO FOUR MORE", 0.9},
	}
	run := func() []string {
		d := newDriver(t, committedConfig())
		var log []string
		for _, f := range timeline {
			for _, tr := range d.frame(f.text, f.conf) {
				log = append(log, tr.String())
			}
			log = append(log, fmt.Sprintf("state=%s text=%s conf=%.3f contra=%.3f", d.tr.State(), d.tr.Text(), d.tr.Confidence(), d.tr.Snapshot().Contradictions))
		}
		return log
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("replay lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("replay diverged at step %d:\n  %s\n  %s", i, a[i], b[i])
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	for s := StateAbsent; s <= StateError; s++ {
		txt, err := s.MarshalText()
		if err != nil {
			t.Fatalf("%d: MarshalText: %v", s, err)
		}
		var back State
		if err := back.UnmarshalText(txt); err != nil {
			t.Fatalf("%q: UnmarshalText: %v", txt, err)
		}
		if back != s {
			t.Fatalf("%s round-tripped to %s", s, back)
		}
	}
	var bad State
	if err := bad.UnmarshalText([]byte("not-a-state")); err == nil {
		t.Fatal("unknown state name accepted")
	}
	if got := State(200).String(); !strings.HasPrefix(got, "state(") {
		t.Fatalf("out-of-range state renders %q", got)
	}
}

func TestConfigNormalizeRejectsNonsense(t *testing.T) {
	got := Config{
		AppearFrames:          -3,
		VoteWindow:            9999,
		MinConfidence:         -1,
		ChangeThreshold:       4,
		MaxContradictionRatio: -0.5,
		MinWordConfidence:     2,
		AdvanceRunFrames:      500,
		MinPredictLen:         0,
		StableWindow:          -time.Second,
	}.normalize()
	d := DefaultConfig()
	if got.AppearFrames != d.AppearFrames || got.MinConfidence != d.MinConfidence || got.ChangeThreshold != d.ChangeThreshold {
		t.Fatalf("negative/out-of-range fields survived: %+v", got)
	}
	if got.VoteWindow != 32 || got.AdvanceRunFrames > got.VoteWindow {
		t.Fatalf("window/run bounds not enforced: vote=%d run=%d", got.VoteWindow, got.AdvanceRunFrames)
	}
	if got.MinPredictLen != d.MinPredictLen || got.StableWindow != d.StableWindow {
		t.Fatalf("zero fields did not take defaults: %+v", got)
	}
	// NaN must not survive: a NaN threshold makes every comparison false, which
	// reads as "the text never changes" — the silent failure mode docs/spec/12
	// fuzzes for.
	nan := Config{MinConfidence: nanFloat(), ChangeThreshold: nanFloat()}.normalize()
	if nan.MinConfidence != d.MinConfidence || nan.ChangeThreshold != d.ChangeThreshold {
		t.Fatalf("NaN survived normalization: %+v", nan)
	}
	// A single-frame stability window is legal and meaningful (a box that is
	// never animated), so a caller may set it; so must MinConfidence 0.55 vs 0.4.
	tight := Config{StableWindow: time.Millisecond, MinConfidence: 0.4}.normalize()
	if tight.StableWindow != time.Millisecond || tight.MinConfidence != 0.4 {
		t.Fatalf("explicit legal values were overwritten: %+v", tight)
	}
}

func nanFloat() float64 {
	z := 0.0
	return z / z
}

func TestEditRatioAndClassification(t *testing.T) {
	cases := []struct {
		a, b string
		want changeKind
	}{
		{"the ship sails", "the ship sails", changeSame},
		{"the ship", "the ship sails", changeExtension},
		{"the ship sails", "", changeDropout},
		{"the ship sails", "the ship saile", changeJitter},
		{"the ship sails", "a different line", changeMaterial},
		{"the ship sails", "the ship", changeTruncation},
		{"", "anything", changeExtension},
	}
	for _, c := range cases {
		got, ratio := classify(c.a, c.b, 0.34)
		if got != c.want {
			t.Errorf("classify(%q, %q) = %s (ratio %.3f), want %s", c.a, c.b, got, ratio, c.want)
		}
	}
	if r := editRatio("kitten", "sitting"); r < 0.1 || r > 0.5 {
		t.Errorf("editRatio(kitten, sitting) = %.3f, want ~0.29", r)
	}
	// Bounded: a huge input must not make the distance quadratic in its length.
	// The bound is on the *distance*, not on the answer — and it must not be
	// inflated the other way either. A difference past the comparison window may
	// not be read as "maximally different", because that would cancel the
	// player's speech for a repeated observation of one long page, which is the
	// stale/duplicate cancellation docs/spec/16 M2 forbids. What editRatio
	// guarantees is a *lower bound*: the length difference always shows, the
	// invisible tail never does.
	huge := strings.Repeat("a", 5000)
	grown := huge + strings.Repeat("b", 50)
	// The rate is measured over the sampled span (2*maxLevRunes), not over the
	// full length: 50 changed runes out of a 384-rune sample, which is below
	// ChangeThreshold and so not a material change.
	if r := editRatio(huge, grown); r < 50.0/384.0-1e-9 || r > 0.34 {
		t.Errorf("ratio past the window = %.4f, want the sampled divergence rate ~%.4f and below the material threshold", r, 50.0/384.0)
	}
	if k, _ := classify(huge, grown, 0.34); k != changeExtension {
		t.Errorf("classify(5 KB page, 5 KB page + tail) = %s, want extension: growth past the sample must never read as a new line", k)
	}
	if r := editRatio(huge, huge); r != 0 {
		t.Errorf("identical 5 KB texts gave ratio %.3f, want 0", r)
	}
	if got := len(clampRunes(huge, maxLevRunes)); got != maxLevRunes {
		t.Fatalf("comparison window not bounded: %d runes, want %d", got, maxLevRunes)
	}
	// A change inside the window is still seen at full strength.
	if !isMaterial(huge, strings.Repeat("b", 5000), 0.34) {
		t.Error("two wholly different 5 KB texts were not material")
	}
}

func TestConsistentAndPrefixRules(t *testing.T) {
	if !consistent("the ship", "the ship sails") {
		t.Error("a prefix must be consistent with its extension")
	}
	if !consistent("", "anything") {
		t.Error("an empty read is compatible with everything: a blank frame is missing evidence, not a contradiction")
	}
	if !consistent("the ahip sails", "the ship sails") {
		t.Error("one glyph of OCR noise must not count as a contradiction of the line")
	}
	if consistent("the ship sails", "open the chest") {
		t.Error("a different line must register as a contradiction")
	}
}

func TestRawTextDropsLowConfidenceArtifacts(t *testing.T) {
	res := &ocr.Result{Text: "HELLO >", Words: []ocr.Word{
		{Text: "HELLO", Conf: 0.9},
		{Text: ">", Conf: 0.1},
	}}
	text, conf := rawText(res, 0.3)
	if text != "HELLO" {
		t.Fatalf("text = %q, want the edge cursor dropped", text)
	}
	if conf < 0.85 || conf > 0.95 {
		t.Fatalf("confidence = %.3f, want the kept word's confidence only", conf)
	}
	// Every word below the floor is a blank box, not a box containing the noise.
	if text, _ := rawText(&ocr.Result{Words: []ocr.Word{{Text: "|", Conf: 0.05}}}, 0.3); text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
	// Engines that return no boxes still contribute.
	if text, _ := rawText(&ocr.Result{Text: "LINES ONLY", MeanConf: 0.7}, 0.3); text != "LINES ONLY" {
		t.Fatalf("text = %q, want the engine's own text", text)
	}
	if text, conf := rawText(nil, 0.3); text != "" || conf != 0 {
		t.Fatalf("nil result gave (%q, %.2f)", text, conf)
	}
}

// TestReadyClaimCostsAStabilizingFrame pins the one progression step that is
// never chained inside a frame. STABILIZING says "the text stopped changing";
// READY says "and nothing in the window contradicts it, at a confidence worth
// speaking". If one observation could assert both, the contradiction check would
// be the stability check read twice and the vote window would never have had a
// chance to disagree — which is how a box gets narrated mid-word. The frame
// boundary is also where the session reducer gets to rank the stabilizing text
// (docs/spec/01 "Candidate lookup is local and deadline constrained").
func TestReadyClaimCostsAStabilizingFrame(t *testing.T) {
	d := newDriver(t, committedConfig())
	var states []State
	for i := 0; i < 6; i++ {
		d.frame("THE SHIP SAILS", 0.9)
		states = append(states, d.tr.State())
	}
	sawStabilizing, sawReady, readyAfterStabilizing := false, false, false
	for _, s := range states {
		switch s {
		case StateStabilizing:
			sawStabilizing = true
		case StateReady:
			sawReady = true
			readyAfterStabilizing = true
		}
		if readyAfterStabilizing && s == StateReady {
			break
		}
	}
	if !sawStabilizing || !sawReady {
		t.Fatalf("states seen = %v, want a stabilizing frame and then a ready frame", states)
	}
	for _, s := range states[:1] {
		if s == StateReady {
			t.Fatal("READY was claimed on the first frame")
		}
	}
	// No Step may produce both transitions at once.
	d2 := newDriver(t, committedConfig())
	for i := 0; i < 6; i++ {
		trs := d2.frame("THE SHIP SAILS", 0.9)
		stab, ready := false, false
		for _, tr := range trs {
			if tr.To == StateStabilizing {
				stab = true
			}
			if tr.To == StateReady {
				ready = true
			}
		}
		if stab && ready {
			t.Fatalf("one observation claimed both stabilizing and ready: %s", reasons(trs))
		}
	}
}

// TestAcceptedTextIsTheRankedFormNotTheComparisonFold guards the two-fold split.
// OCR reads "STATIC LINE"; the comparison fold is "statlc llne" (I→l), and the
// matcher folds on its own terms (internal/matcher IndexOptions). Reporting the
// lossy fold as the accepted text would both corrupt what is ranked and put a
// mangled sentence on screen and in the event log
// (docs/spec/05 "Preserve raw observed OCR and normalized text separately").
func TestAcceptedTextIsTheRankedFormNotTheComparisonFold(t *testing.T) {
	d := newDriver(t, committedConfig())
	for i := 0; i < 4; i++ {
		d.frame("STATIC LINE 100%", 0.9)
	}
	if got, want := d.tr.Text(), "static line 100%"; got != want {
		t.Fatalf("Text() = %q, want %q (the output fold keeps punctuation and does not guess at glyph confusions)", got, want)
	}
	key := d.tr.Key()
	if key == d.tr.Text() {
		t.Fatalf("Key() = Text() = %q; the comparison fold must be lossier than the ranked text", key)
	}
	if !strings.Contains(key, "l") || strings.Contains(key, "%") {
		t.Fatalf("Key() = %q, want the OCR fold (1→l) with punctuation stripped", key)
	}
	if got := d.tr.Raw(); got != "STATIC LINE 100%" {
		t.Fatalf("Raw() = %q, want the engine's own text", got)
	}
}
