package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	topID        string   // top candidate of the last ranking trace row
	topProb      float64  // its probability
	ocrText      string   // last non-empty OCR reading of the run
	stats        pipeline.Stats

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
}

// runSample replays one sample's frames through the real pipeline (ModeReplay,
// real Tesseract, real matcher) and then drives the speech sidecar over the
// pipeline's own per-frame readings. Deterministic: same frames, same engine,
// same answer — the property the M2 replay test asserts across three runs.
func runSample(ctx context.Context, framesDir string, s Sample, idx *matcher.Index, regions []games.Region) (sampleOutcomes, error) {
	cfg := pipeline.Config{
		SessionID:       benchSessionID,
		ProfileRevision: benchRevision,
		Regions:         regions,
		OCR:             ocr.Tesseract{},
		Index:           idx,
		Preprocess:      vision.DefaultSteps(),
		Trace:           true,
		Mode:            pipeline.ModeReplay,
		OCRTimeout:      ocr.DefaultTimeout,
		Tracker:         benchTracker,
	}
	runner, err := pipeline.New(cfg)
	if err != nil {
		return sampleOutcomes{}, fmt.Errorf("pipeline.New: %v", err)
	}
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	src := &replay.Source{Dir: framesDir, Speed: 0}
	rctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := runner.Run(rctx, src, hub); err != nil {
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
		for _, tr := range p.Transitions {
			if tr.To == "committed" {
				out.commitTSNS = append(out.commitTSNS, tr.CaptureTimestampNS)
			}
		}
	}
	trace := runner.Trace()
	for i := range trace { // last ranking row wins: it stands behind the commit
		if trace[i].TopCandidate != "" {
			out.topID, out.topProb = trace[i].TopCandidate, trace[i].TopScore
		}
		if trace[i].OCRText != "" {
			out.ocrText = trace[i].OCRText
		}
	}
	out.stats = runner.Stats()

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
	for i, f := range s.Frames {
		present := f.File != ""
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
	}

	var speech []speechEvent
	var firstSpeaker string
	firstKnown := false
	for _, e := range pub.byName(events.MessageCommitted) {
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
			return nil, 0, "", false, fmt.Errorf("sidecar decision: %v", err)
		}
		if !eligible {
			continue // not in the script: silence, never a forced speaker
		}
		if _, err := sess.Apply(ctx, session.SpeechMsg{
			Header: header(p.Generation), RegionID: p.RegionID,
			OccurrenceID: p.Occurrence, Kind: session.SpeechReady, VoiceID: benchVoice,
		}); err != nil {
			return nil, 0, "", false, fmt.Errorf("sidecar ready: %v", err)
		}
		res, err := sess.Apply(ctx, session.SpeechMsg{
			Header: header(p.Generation), RegionID: p.RegionID,
			OccurrenceID: p.Occurrence, Kind: session.SpeechStarted, VoiceID: benchVoice,
		})
		if err != nil {
			return nil, 0, "", false, fmt.Errorf("sidecar started: %v", err)
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
	var first int64
	for _, f := range s.Frames {
		if f.File == "text.png" {
			first = f.TsNS
			break
		}
	}
	return truthSample{
		id: s.ID, text: s.SourceText, msgID: s.MessageInScript(),
		speaker: s.Speaker, intended: s.IntendedSpeech, textKind: s.TextKind,
		variant: s.Variant, firstTextNS: first,
	}
}
