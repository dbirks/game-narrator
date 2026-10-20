package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/dbirks/game-narrator/internal/capture/replay"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// The bench session identity and tracker settings. The tracker values match
// internal/pipeline's synthetic replay test (VoteWindow 3, StableWindow 100ms
// on the 10 Hz capture clock); the reducer policy is the session default. Both
// the pipeline and the speech sidecar use exactly these, so the sidecar is a
// faithful shadow of the pipeline's session, not a different system.
const (
	benchSessionID = "benchsession"
	benchRevision  = "1"
	benchVoice     = "synthetic-narrator"
	// sidecarConf is the script-side confidence the M2 sidecars use for
	// replayed readings; the bench's tracker runs with the default
	// MinConfidence and the sidecar's job is the speech layer, not a second
	// opinion on OCR quality.
	sidecarConf = 0.9
)

var benchTracker = recognition.Config{VoteWindow: 3, StableWindow: 100 * time.Millisecond}

// writeManifest writes the replay source's timestamps.json for one sample.
func writeManifest(path string, frames []Frame) error {
	type entry struct {
		File string `json:"file"`
		TsNS int64  `json:"ts_ns"`
	}
	rows := make([]entry, 0, len(frames))
	for _, f := range frames {
		rows = append(rows, entry{File: f.File, TsNS: f.TsNS})
	}
	raw, err := json.MarshalIndent(rows, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// speechEvent is one speech.started the sidecar's session emitted.
type speechEvent struct {
	occurrenceID string
	messageID    string
	generation   int64
}

// sampleOutcomes is everything one sample's run contributes to the metrics:
// the pipeline's own events and trace, and the sidecar's speech events.
type sampleOutcomes struct {
	sample truthSample

	// Pipeline side.
	commitMsgIDs []string // message.committed payload message_id, in order
	commitTSNS   []int64  // capture ts of each transition into committed
	commitRaw    []string // the committed reading (payload raw_text)
	commitStable []string // the committed stable text (payload text), in commit order
	commitText   string   // last ranking trace row's accepted (tracker-stable) text
	topID        string   // top candidate of the last ranking trace row
	topProb      float64  // its probability
	ocrText      string   // last non-empty OCR reading of the run
	stats        pipeline.Stats

	// Display-window grading (ADR-0058 amendment 3): the top-1 of the line's
	// display window — the frames the oracle says the truth line is on screen.
	// windowRanked is true when a ranking row fell inside the window (the
	// reading below is then the graded one); windowTop* is the top candidate
	// of the window's committed/stable reading when the sample committed,
	// else of the window's last ranking row.
	windowTopID   string
	windowTopProb float64
	windowRanked  bool

	// Speech side (sidecar).
	speechStarted   []speechEvent
	speechCancelled int
	speakerID       string // scripted decision speaker for the first commit
	speakerKnown    bool
}

// truthSample is the sample's grading facts, flattened for the metrics.
type truthSample struct {
	id          string
	text        string
	msgID       string // "" when not in the script
	speaker     string
	intended    bool
	textKind    string
	variant     Variant
	firstTextNS int64 // capture ts of the first text frame
	frameStepNS int64 // max consecutive-frame gap of the capture grid (replay clock)
}

// runSample replays one sample's frames through the real pipeline (ModeReplay,
// real Tesseract, real matcher) and then drives the speech sidecar over the
// pipeline's own per-frame readings. Deterministic: same frames, same engine,
// same answer — the property the M2 replay test asserts across three runs.
// The pipeline runs in Deterministic mode (issue #17, ADR-0064 amendment 2):
// the replay's wall-clock belts (age-out, retrieval-hold budget, end-of-stream
// grace) are removed, so the recognition outcome is independent of host load
// while the per-OCR-call deadline and retry budget stay the wedge guard. The
// measured stage latencies remain wall-clock and are reported separately (the
// report's latency section and the recorded-bounds flag), never graded on.
// timestampsFile is the replay source's manifest override ("" = the default
// <dir>/timestamps.json); the captured holdout points it at a per-sample
// segment of the fixture's frame manifest without touching the fixture.
func runSample(ctx context.Context, framesDir, timestampsFile string, s Sample, idx *matcher.Index, regions []games.Region) (sampleOutcomes, error) {
	cfg := pipeline.Config{
		SessionID:       benchSessionID,
		ProfileRevision: benchRevision,
		Regions:         regions,
		OCR:             ocr.Tesseract{},
		Index:           idx,
		Preprocess:      vision.DefaultSteps(),
		Trace:           true,
		TraceMax:        benchTraceMax(len(s.Frames)),
		Mode:            pipeline.ModeReplay,
		Deterministic:   true,
		OCRTimeout:      ocr.DefaultTimeout,
		Tracker:         benchTracker,
	}
	runner, err := pipeline.New(cfg)
	if err != nil {
		return sampleOutcomes{}, fmt.Errorf("pipeline.New: %v", err)
	}
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	src := &replay.Source{Dir: framesDir, Speed: 0, TimestampsFile: timestampsFile}
	// No per-sample wall budget (ADR-0064): a replay verdict must not depend
	// on host load, and any per-sample wall deadline is load-dependent by
	// construction — under CPU contention a sample whose OCR calls slow down
	// outlives its budget and is lost to a NOT RUN, which changes the sample
	// count and every metric with it (the class (b) regression of ADR-0058
	// amendment 1, measured on the dense fixture). The bounds that remain are
	// the load-independent ones: the pipeline's own per-OCR-call deadline and
	// retry budget (ModeReplay; a wedged engine is killed per call, so a
	// sample cannot hang), and the caller's run-level timeout (ReplayRunBound),
	// which is the harness's patience, scaled to the fixture.
	if err := runner.Run(ctx, src, hub); err != nil {
		_ = src.Close()
		_ = hub.Close()
		return sampleOutcomes{}, fmt.Errorf("pipeline run: %v", err)
	}
	_ = src.Close()
	evs, err := mem.After(context.Background(), 0, events.Filter{}, 4096)
	if err != nil {
		_ = hub.Close()
		return sampleOutcomes{}, fmt.Errorf("read events: %v", err)
	}
	_ = hub.Close()

	out := sampleOutcomes{sample: truthSampleFrom(s)}
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		var p struct {
			MessageID   string `json:"message_id"`
			RawText     string `json:"raw_text"`
			Text        string `json:"text"`
			Transitions []struct {
				To                 string `json:"to"`
				CaptureTimestampNS int64  `json:"capture_timestamp_ns"`
			} `json:"transitions"`
		}
		if err := json.Unmarshal(e.Data, &p); err != nil {
			continue
		}
		out.commitMsgIDs = append(out.commitMsgIDs, p.MessageID)
		out.commitRaw = append(out.commitRaw, p.RawText)
		out.commitStable = append(out.commitStable, p.Text)
		for _, tr := range p.Transitions {
			if tr.To == "committed" {
				out.commitTSNS = append(out.commitTSNS, tr.CaptureTimestampNS)
			}
		}
	}
	out.stats = runner.Stats()
	// Overflow fence (ADR-0058 amendment 3): the harness must lose no trace
	// rows — the grading readout runs on them. The ring is sized to the sample
	// (benchTraceMax), so an eviction is a defect, not a shed row: fail the
	// sample loudly instead of grading on an incomplete timeline.
	if d := out.stats.TraceDropped; d > 0 {
		return sampleOutcomes{}, fmt.Errorf("bench: sample %s: trace ring overflow — %d rows evicted before the readout (the ring was sized %d rows for %d frames; the harness must lose no trace rows, ADR-0058 amendment 3)",
			s.ID, d, benchTraceMax(len(s.Frames)), len(s.Frames))
	}
	trace := runner.Trace()
	for i := range trace { // last ranking row wins: the legacy (last-row) reading
		if trace[i].TopCandidate != "" {
			out.topID, out.topProb = trace[i].TopCandidate, trace[i].TopScore
		}
		if trace[i].AcceptedText != "" {
			out.commitText = trace[i].AcceptedText
		}
		if trace[i].OCRText != "" {
			out.ocrText = trace[i].OCRText
		}
	}
	gradeDisplayWindow(&out, s, trace)

	speech, cancelled, speaker, known, err := runSidecar(ctx, idx, regions[0].ID, s)
	if err != nil {
		return sampleOutcomes{}, err
	}
	out.speechStarted = speech
	out.speechCancelled = cancelled
	out.speakerID = speaker
	out.speakerKnown = known
	// The named-speaker metric attributes the pipeline's actual commit, not
	// the sidecar's: a wrong recognition must lose its speaker too.
	if len(out.commitMsgIDs) > 0 {
		out.speakerID, out.speakerKnown, _ = scriptSpeaker(out.commitMsgIDs[0])
	}
	return out, nil
}

// benchTraceMax sizes the pipeline's trace ring to one sample's replay so no
// row can be evicted (ADR-0058 amendment 3): a frame contributes at most three
// rows (one dispatch row, one observation row, one ranking row), and the +64
// margin covers the run's non-frame rows. This is the harness's ring option
// (Config.TraceMax); the pipeline's runtime default is untouched.
func benchTraceMax(frames int) int {
	return frames*4 + 64
}

// displayWindowFrames is the sample's display window: the frames the oracle
// says the truth line is on screen (ADR-0058 amendment 3). The captured holdout
// carries the fixture's per-frame oracle labels in s.DisplayWindow (a frame is
// in the window when the oracle marks the box present and its line label is
// the line's label); the synthetic sets leave it nil, and the window then is
// the sample's box-present frames (its text.png frames — a synthetic sample's
// trailing frames are boxless, so there the window and the ranked span
// coincide and both holdouts are graded one way).
func displayWindowFrames(s Sample) []Frame {
	var out []Frame
	for i, f := range s.Frames {
		present := f.File != "" && f.File != "absent.png" && !f.Absent
		if s.DisplayWindow != nil {
			present = s.DisplayWindow[i]
		}
		if present {
			out = append(out, f)
		}
	}
	return out
}

// gradeDisplayWindow computes the sample's display-window top-1 reading
// (ADR-0058 amendment 3): over the ranking trace rows whose driving frame
// falls in the window's capture-ts range, the graded top-1 is the top
// candidate of the line's committed/stable reading — the window row whose
// accepted text is the first commit's stable text, the reading the session's
// own commit rule (session/apply.go decide: the commit is the applied
// ranking's top candidate) committed — or, when the sample did not commit or
// no such row exists, of the window's last ranking row. A sample with no
// ranking row in the window is unranked (windowRanked stays false) and the
// metrics fall back to the last-row reading for it.
func gradeDisplayWindow(out *sampleOutcomes, s Sample, trace []pipeline.TraceEntry) {
	win := displayWindowFrames(s)
	if len(win) == 0 {
		return
	}
	w0, w1 := win[0].TsNS, win[len(win)-1].TsNS
	committed := ""
	if len(out.commitStable) > 0 {
		committed = out.commitStable[0]
	}
	var committedTop string
	var committedProb float64
	for i := range trace {
		row := &trace[i]
		if row.TopCandidate == "" {
			continue
		}
		if row.CaptureTimestampNS < w0 || row.CaptureTimestampNS > w1 {
			continue
		}
		// The window's last ranking row is the fallback reading.
		out.windowTopID, out.windowTopProb, out.windowRanked = row.TopCandidate, row.TopScore, true
		if committed != "" && row.AcceptedText == committed {
			// The committed reading's row; the last such row wins.
			committedTop, committedProb = row.TopCandidate, row.TopScore
		}
	}
	if committedTop != "" {
		out.windowTopID, out.windowTopProb = committedTop, committedProb
	}
}

// pinReplayEngineThreads pins the OCR engine's OpenMP thread pool to one
// thread per process. Tesseract 5 otherwise spawns a thread pool of the whole
// core set per call, so the bench's parallel samples oversubscribe the host
// (four single-threaded Tesseract processes plus the Go runtime fit on an
// 8-core box with headroom; four default Tesseract calls run eight-plus
// threads and push per-call latency into the retry budget under any
// concurrent load). Single-threaded is also the deterministic configuration:
// same crop, same thread count, same answer — the property the replay's
// determinism contract (ADR-0037/0054) grades. It is process-global; the
// bench is a leaf tool and its children (Tesseract) inherit the environment.
func pinReplayEngineThreads() {
	_ = os.Setenv("OMP_NUM_THREADS", "1")
}

// replayWorkers is the bench's parallel sample count: bounded so the
// concurrent Tesseract processes cannot starve each other's per-call deadline
// (the pipeline's ModeReplay retry budget is the load shield, but a bench
// that saturates its own host manufactures the load it exists to tolerate).
// Four on an 8-core box, two on a 4-core one, one on a small box; never more
// samples than exist. Results never depend on the count: per-frame OCR is
// deterministic and ModeReplay is sequential per region (ADR-0064).
func replayWorkers(n int) int {
	w := 4
	if h := runtime.NumCPU() / 2; h < w {
		w = h
	}
	if w < 1 {
		w = 1
	}
	if n < w {
		w = n
	}
	return w
}

// recPub is the sidecar's events.Publisher: it keeps the envelopes so the
// speech lifecycle can be read after the replay.
type recPub struct {
	seq int64
	out []events.Envelope
}

func (p *recPub) Publish(_ context.Context, e events.Envelope) (events.Envelope, error) {
	p.seq++
	if e.EventID == "" {
		e.EventID = games.NewID()
	}
	e.Seq = p.seq
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = events.SchemaVersion
	}
	if err := e.Validate(); err != nil {
		return e, err
	}
	p.out = append(p.out, e)
	return e, nil
}

func (p *recPub) byName(name string) []events.Envelope {
	var out []events.Envelope
	for _, e := range p.out {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// runSidecar replays the sample's truth frames through a fresh session reducer
// with the bench's tracker and the shared matcher index. This is the M2 speech
// sidecar pattern: what is real here is the session state machine, the matcher
// ranking of the sample's line, and the sample's timing; what is scripted is
// the TTS worker's readiness (we assert on speech scheduling, not audio).
// Perception quality (recall/CER/top-1/calibration) is measured separately on
// the pipeline's real OCR; the sidecar isolates the speech layer. It answers
// each commit with the scripted decision (speaker from the script — the
// synthetic stand-in for Clef) and an immediate TTS-ready. It returns the
// speech.started events, the speech.cancelled count, and the first commit's
// scripted speaker answer.
func runSidecar(ctx context.Context, idx *matcher.Index, regionID string, s Sample) ([]speechEvent, int, string, bool, error) {
	pub := &recPub{}
	sess, err := session.New(benchSessionID, benchRevision, pub,
		session.WithTrackerConfig(benchTracker))
	if err != nil {
		return nil, 0, "", false, fmt.Errorf("sidecar session: %v", err)
	}
	next := int64(0)
	header := func(gen int64) session.Header {
		next++
		return session.Header{SessionID: benchSessionID, Seq: next, Generation: gen, ProfileRevision: benchRevision}
	}
	lastKey := ""
	var speech []speechEvent
	var firstSpeaker string
	firstKnown := false
	processed := 0 // commits already answered (index into the publisher's list)
	// answer walks every commit the session has emitted that has not been
	// answered yet and applies the scripted decision + immediate TTS-ready +
	// start — at the moment the commit fires, while the region still stands
	// behind the occurrence. The session's speech lifecycle rejects a start
	// whose occurrence no longer stands (a dismissed box), so answering after
	// the replay would leave a captured sample that shows its advance frame
	// with a committed line unable to speak at all (its region is dismissed
	// before the batched answer lands). In the product the decision provider
	// and the TTS worker run concurrently with the capture; this is that,
	// scripted.
	answer := func() error {
		commits := pub.byName(events.MessageCommitted)
		for _, e := range commits[processed:] {
			var p struct {
				MessageID  string `json:"message_id"`
				Occurrence string `json:"dialogue_occurrence_id"`
				Generation int64  `json:"generation"`
				RegionID   string `json:"region_id"`
			}
			if err := json.Unmarshal(e.Data, &p); err != nil {
				continue
			}
			spk, known, eligible := scriptSpeaker(p.MessageID)
			if firstSpeaker == "" {
				firstSpeaker, firstKnown = spk, known
			}
			obsSeq := int64(0)
			if st := sess.State(); len(st.Regions) > 0 {
				obsSeq = st.Regions[0].ObservationSeq
			}
			dm := session.DecisionMsg{
				Header:         header(p.Generation),
				RegionID:       p.RegionID,
				ObservationSeq: obsSeq,
				MessageID:      p.MessageID,
				SpeakerID:      spk,
				SpeakerKnown:   known,
				SpeechEligible: eligible,
				Accepted:       true,
				Provider:       "scripted-synthetic",
				Model:          "scripted-synthetic",
			}
			if _, err := sess.Apply(ctx, dm); err != nil {
				return fmt.Errorf("sidecar decision: %v", err)
			}
			if !eligible {
				continue // not in the script: silence, never a forced speaker
			}
			if _, err := sess.Apply(ctx, session.SpeechMsg{
				Header: header(p.Generation), RegionID: p.RegionID,
				OccurrenceID: p.Occurrence, Kind: session.SpeechReady, VoiceID: benchVoice,
			}); err != nil {
				return fmt.Errorf("sidecar ready: %v", err)
			}
			res, err := sess.Apply(ctx, session.SpeechMsg{
				Header: header(p.Generation), RegionID: p.RegionID,
				OccurrenceID: p.Occurrence, Kind: session.SpeechStarted, VoiceID: benchVoice,
			})
			if err != nil {
				return fmt.Errorf("sidecar started: %v", err)
			}
			for _, ev := range res.Events {
				if ev.Name != events.SpeechStarted {
					continue
				}
				var sp struct {
					Occurrence string `json:"dialogue_occurrence_id"`
					MessageID  string `json:"message_id"`
					Generation int64  `json:"generation"`
				}
				_ = json.Unmarshal(ev.Data, &sp)
				speech = append(speech, speechEvent{occurrenceID: sp.Occurrence, messageID: sp.MessageID, generation: sp.Generation})
			}
		}
		processed = len(commits)
		return nil
	}
	for i, f := range s.Frames {
		present := f.File != "" && !f.Absent
		text := ""
		if present && s.TextKind == "dialogue" {
			text = s.SourceText
		}
		obs := recognition.Observation{
			FrameSeq:           int64(i + 1),
			CaptureTimestampNS: f.TsNS,
			SourceClock:        "replay",
			RegionID:           regionID,
			RegionPresent:      present,
		}
		if text != "" {
			obs.OCR = sidecarOCR(text)
		}
		if _, err := sess.Apply(ctx, session.ObservationMsg{
			Header:      header(sess.Generation()),
			RegionID:    regionID,
			Observation: obs,
		}); err != nil {
			return nil, 0, "", false, fmt.Errorf("sidecar obs %d: %v", i, err)
		}
		st := sess.State()
		if len(st.Regions) == 0 || st.Regions[0].TextState != recognition.StateReady || st.Regions[0].Text == "" {
			continue
		}
		rs := st.Regions[0]
		if rs.NormalizedKey == lastKey {
			continue // this exact reading was already answered
		}
		lastKey = rs.NormalizedKey
		rank := idx.Rank(matcher.Observation{Text: rs.RawText, OCRConf: rs.Confidence, TextKindHint: "dialogue"}, matcher.RankOptions{})
		if _, err := sess.Apply(ctx, session.RankingMsg{
			Header:         header(sess.Generation()),
			RegionID:       regionID,
			ObservationSeq: rs.ObservationSeq,
			Text:           rs.Text,
			Ranking:        rank,
		}); err != nil {
			return nil, 0, "", false, fmt.Errorf("sidecar rank %d: %v", i, err)
		}
		// The ranking is what commits: answer every new commit now, while the
		// region still stands behind its occurrence.
		if err := answer(); err != nil {
			return nil, 0, "", false, err
		}
	}
	_ = answer() // a commit fired on a final observation is answered before the return

	return speech, len(pub.byName(events.SpeechCancelled)), firstSpeaker, firstKnown, nil
}

// scriptSpeaker is the scripted decision provider: it answers from the script
// (the synthetic stand-in for Clef, which in the product answers from game
// context). In-script messages are eligible to be spoken; a named speaker is
// known only when the script attributes one — a line the script leaves
// unattributed must never receive a forced named speaker.
var scriptSpeaker = func(msgID string) (speaker string, known, eligible bool) {
	spk, inScript := benchProfile.SpeakerOf(msgID)
	return spk, inScript && spk != SpeakerUnknown, inScript
}

// benchProfile is the active set's script; runBench installs it before any
// sample runs. It is a var (not a parameter threaded through every call)
// because the sidecar's decision answer is the one place the script is
// consulted, and the ADR records that real sets will replace it with the
// reviewed profile rows.
var benchProfile *ScriptProfile

// sidecarOCR shapes one replayed reading the way the real engine's result
// arrives: text and a script-side mean confidence.
func sidecarOCR(text string) *ocr.Result {
	res := ocr.Result{Text: text, Engine: "sidecar", MeanConf: sidecarConf}
	res.Lines = []ocr.Line{{Text: text, Conf: sidecarConf}}
	return &res
}

func truthSampleFrom(s Sample) truthSample {
	// The sample's first text frame: the synthetic set names it "text.png";
	// a captured segment is the first frame the oracle does not mark absent.
	// It is the latency origin (the frame the box's text first appears on).
	var first int64
	for _, f := range s.Frames {
		if f.File == "text.png" || (f.File != "" && f.File != "absent.png" && !f.Absent) {
			first = f.TsNS
			break
		}
	}
	return truthSample{
		id: s.ID, text: s.SourceText, msgID: s.MessageInScript(),
		speaker: s.Speaker, intended: s.IntendedSpeech, textKind: s.TextKind,
		variant: s.Variant, firstTextNS: first, frameStepNS: frameStepOf(s.Frames),
	}
}

// frameStepOf is the largest positive consecutive-frame gap of the sample's
// capture grid (the replay clock's frame interval; on a uniform schedule every
// gap equals it). It bounds how late a commit decided by one frame can be:
// the frame is processed after its capture, before the next gap elapses
// (ADR-0038, latency resolution note).
func frameStepOf(frames []Frame) int64 {
	var max int64
	for i := 1; i < len(frames); i++ {
		if d := frames[i].TsNS - frames[i-1].TsNS; d > max {
			max = d
		}
	}
	return max
}
