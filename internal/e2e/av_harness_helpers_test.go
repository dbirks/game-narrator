// Package e2e — the A/V harness timeline and metric helpers (ADR 0143):
// the shared-clock report types, the RMS-onset detector, the nearest-rank
// percentile, and the join of the session's dev event stream onto the
// recording's wall clock. Log-safe per AGENTS.md rule 5: counts, ids and
// timings only — never dialogue text.
package e2e

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/speech"
)

// avLineRecord is one committed dialogue line on the shared A/V clock: the
// stage instants (ms from the recording's wall-clock start, T0) plus the
// acoustic onset measured in the recorded audio track itself.
type avLineRecord struct {
	Index              int     `json:"index"`
	OccurrenceID       string  `json:"occurrence_id"`
	TraceID            string  `json:"trace_id"`
	MessageID          string  `json:"message_id"`
	RegionID           string  `json:"region_id"`
	TBoxAppearMS       int64   `json:"t_box_appear_ms"`
	TTextSettledMS     int64   `json:"t_text_settled_ms"`
	TCommitMS          int64   `json:"t_commit_ms"`
	TClefDoneMS        int64   `json:"t_clef_done_ms,omitempty"`
	TTSReadyMS         int64   `json:"t_tts_ready_ms"`
	TAudioStartEventMS int64   `json:"t_audio_start_event_ms"`
	TAudioStartRMSMS   int64   `json:"t_audio_start_rms_ms"`
	TAudioEndMS        int64   `json:"t_audio_end_ms"`
	SettledToAudioMS   int64   `json:"settled_to_audio_ms"`
	AppearToAudioMS    int64   `json:"appear_to_audio_ms"`
	CommitToAudioMS    int64   `json:"commit_to_audio_ms"`
	FromCache          bool    `json:"from_cache"`
	VoiceID            string  `json:"voice_id"`
	ExpectedVoiceID    string  `json:"expected_voice_id"`
	VoiceMatch         bool    `json:"voice_match"`
	ASRWER             float64 `json:"asr_wer"`
	ASRCER             float64 `json:"asr_cer"`
	// ASRWERvsScript is the ASR transcript's WER against the line's script
	// text (the corpus ground truth avASRScore scores against, Amendment 1): the
	// gate's spoken-line fidelity input; zero where the line was never
	// scored (an unscored line is "no evidence", not a perfect reading).
	ASRWERvsScript float64 `json:"asr_wer_vs_script"`
	// BoxPosition is the line's box placement (Amendment 1, from the
	// profile's placement-aware region rows, ADR 0137): "top", "middle",
	// "bottom" — or "" when the line's region has no row to classify.
	BoxPosition string `json:"box_position,omitempty"`
	ClipMP4     string `json:"clip_mp4,omitempty"`
	ClipWAV     string `json:"clip_wav,omitempty"`
	// Batched: spoken inside a merged catch-up run (ADR-0122); RunPosition
	// is its place in the run (0 = the run's first, audible-at-start line).
	Batched         bool   `json:"batched,omitempty"`
	RunPosition     int    `json:"run_position,omitempty"`
	RunOccurrenceID string `json:"run_occurrence_id,omitempty"`
	// EndResult is the line's terminal result (completed, interrupted,
	// skipped, cancelled, …).
	EndResult string `json:"end_result,omitempty"`
	// QueuedBehindSpeech: committed while another line was audible, so its
	// commit→audio is queueing time, kept out of the idle latency gate.
	QueuedBehindSpeech bool `json:"queued_behind_speech,omitempty"`
	clipAudioStartMS   int64
	asrScored          bool
}

// avTimelineReport is the run's synchronized A/V timeline (ADR 0143): the
// per-occurrence stage instants, the latency percentiles the gate asserts
// on, and the consistency counters.
type avTimelineReport struct {
	RunID               string         `json:"run_id"`
	SessionID           string         `json:"session_id"`
	RecordingStartedAt  time.Time      `json:"recording_started_at"`
	SessionMP4          string         `json:"session_mp4"`
	SessionWAV          string         `json:"session_wav"`
	Lines               []avLineRecord `json:"lines"`
	SettledToAudioP50MS int64          `json:"settled_to_audio_p50_ms"`
	SettledToAudioP95MS int64          `json:"settled_to_audio_p95_ms"`
	AppearToAudioP50MS  int64          `json:"appear_to_audio_p50_ms"`
	AppearToAudioP95MS  int64          `json:"appear_to_audio_p95_ms"`
	CommitToAudioP50MS  int64          `json:"commit_to_audio_p50_ms"`
	CommitToAudioP95MS  int64          `json:"commit_to_audio_p95_ms"`
	MissedLines         int            `json:"missed_lines"`
	DuplicateCommits    int            `json:"duplicate_commits"`
	StaleStarts         int            `json:"stale_starts"`
	CacheHits           int            `json:"cache_hits"`
	// PhantomUtterances (Amendment 1) counts the speech.started events the
	// timeline cannot explain: the occurrence — or the merged run's
	// occurrence the started names — maps to no committed line, or to a
	// read-through line. This is the "phantom speech" counter: on the board
	// the narrator spoke OCR text with no dialogue box on screen, and the
	// timeline had no field that named it.
	PhantomUtterances int `json:"phantom_utterances"`
	// PhantomReadThroughs is the read-through half of the phantom count: the
	// started events whose occurrence is a commit with the read-through
	// marker (message_id "unknown", ADR 0136) — spoken where the corpus
	// cannot name the line.
	PhantomReadThroughs int `json:"phantom_read_throughs"`
	// SkippedLines counts lines whose terminal was skipped (stale catch-up,
	// duplicate region, backpressure). IdleSamples is the number of lines in
	// the settled→audio gate; the Queued* percentiles are the lines that
	// waited behind another line's audio.
	SkippedLines             int   `json:"skipped_lines"`
	IdleSamples              int   `json:"idle_samples"`
	QueuedCommitToAudioP50MS int64 `json:"queued_commit_to_audio_p50_ms"`
	QueuedCommitToAudioP95MS int64 `json:"queued_commit_to_audio_p95_ms"`
}

// avDetectRMSOnsetMS scans a 16-bit PCM WAV in 20 ms windows from
// searchFromMS to searchToMS (ms from the audio track's start) and returns
// the start offset, in ms, of the first window whose RMS level —
// 20*log10(rms) over the normalized [-1.0, 1.0] samples — reaches
// thresholdDBFS (the ADR-0143 onset gate, e.g. -40.0), or (0, false).
func avDetectRMSOnsetMS(wavBytes []byte, searchFromMS, searchToMS int64, thresholdDBFS float64) (int64, bool) {
	pcm, err := speech.DecodeWAV(wavBytes)
	if err != nil || pcm.SampleRate <= 0 || pcm.BitsPerSample != 16 {
		return 0, false
	}
	ch, frames := int64(pcm.Channels), int64(pcm.Frames())
	if totalMS := frames * 1000 / int64(pcm.SampleRate); searchToMS > totalMS {
		searchToMS = totalMS
	}
	if searchFromMS < 0 {
		searchFromMS = 0
	}
	for off := searchFromMS; off+20 <= searchToMS; off += 20 {
		start, end := int(off*int64(pcm.SampleRate)/1000), int((off+20)*int64(pcm.SampleRate)/1000)
		if end > int(frames) {
			end = int(frames)
		}
		if start >= end {
			break // past the end of the track
		}
		var sum, sumSq float64
		n := 0
		for s := start; s < end; s++ {
			fb := s * int(ch) * 2
			if fb+2 > len(pcm.Data) {
				break
			}
			x := float64(int16(binary.LittleEndian.Uint16(pcm.Data[fb:fb+2]))) / 32768.0
			sum, sumSq, n = sum+x, sumSq+x*x, n+1
		}
		if n == 0 {
			break
		}
		mean := sum / float64(n)
		if rms := math.Sqrt(math.Max(0, sumSq/float64(n)-mean*mean)); rms > 0 && 20*math.Log10(rms) >= thresholdDBFS {
			return off, true
		}
	}
	return 0, false
}

// avPercentileMS is the nearest-rank percentile (rank = ceil(pct/100*n),
// 1-based) of a slice of int64 values; empty input reports 0 (a missing
// sample is "no evidence", not a fabricated instant).
func avPercentileMS(vals []int64, pct float64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(pct / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// avMS converts one instant to ms from the recording's start (T0); an
// instant before T0 clamps to 0 (the clock starts with the recorder).
func avMS(recStart, at time.Time) int64 {
	if at.IsZero() || recStart.IsZero() {
		return 0
	}
	d := at.Sub(recStart)
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

// avParseAt parses one envelope's occurred_at (the store's fixed-width UTC
// shape; RFC3339Nano also accepts the payload's started_at/ended_at).
func avParseAt(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// avObsKey is the box-instance group key: the region plus the tracker's box
// lifetime (a lifetime change is a new box instance, the reducer's rule).
func avObsKey(region string, lifetime uint64) string {
	return region + "\x00" + strconv.FormatUint(lifetime, 10)
}

// avSpeechData is the payload the join reads off every speech.* event (the
// wiring correlates every one with the occurrence; the scheduler's raw
// events fall back to the utterance id).
type avSpeechData struct {
	Occ        string   `json:"dialogue_occurrence_id"`
	Utterance  string   `json:"utterance_id"`
	MessageIDs []string `json:"message_ids"`
	FromCache  bool     `json:"from_cache"`
	VoiceID    string   `json:"voice_id"`
	Result     string   `json:"result"`
	StartedAt  string   `json:"started_at"`
	EndedAt    string   `json:"ended_at"`
}

// avExpectedVoices are the voices the session resolves lines to (the
// ADR-0143 dual-voice scene): the character's voice for a line the commit
// attributes to a speaker, the narrator's for every other line (the
// runtime's speakerFor contract: the assigned character, else the
// narrator). Either may be "" — a line whose slot is empty has no
// expected voice, and the gate's voice rule skips it.
type avExpectedVoices struct {
	Character string
	Narrator  string
	// CharacterID and SpeakerByMessage are the profile's own attribution
	// (message id → speaker character id, after the speaker link): the map
	// the runtime's speakerFor resolves voices from. When set, a line whose
	// message the profile gives to CharacterID expects the character's
	// voice and every other line the narrator's. The commit's speaker_known
	// is the decision step's attribution, not the profile's: on the
	// 2026-10-21 run it was false for lines the profile links to the
	// character, so every correctly voiced line read as a mismatch. Nil
	// falls back to the commit's attribution (the synthetic unit tests).
	CharacterID      string
	SpeakerByMessage map[string]string
}

// avBuildTimeline correlates the session's dev event stream onto the
// recording's wall clock (ADR 0143), per committed occurrence: the box's
// region+lifetime group's first non-empty non-Mesa reading (t_box_appear)
// and first READY state (t_text_settled) from text.observed — the event
// carries no occurrence id before the commit mints it, so the join is the
// box instance — the commit, the Clef resolution (joined on the commit's
// region+generation+message+observation_seq), speech.ready (t_tts_ready +
// from_cache), speech.started (event + voice) and speech.ended
// (t_audio_end). t_audio_start_rms is the first 20 ms window at/after the
// audio start whose RMS in the recorded track crosses -40 dBFS, falling
// back to the event stamp when the window is silent. Lines are stream-
// ordered; p50/p95 are the warm-run gate's inputs. Amendment 1 adds the
// per-line expected voice from the commit's speaker attribution (the
// resolveVoice rule), the box position from regionGeos (region id → the
// region row's centre Y over the frame height) and the phantom-utterance
// scan over the speech.started events the timeline's lines cannot explain.
func avBuildTimeline(runID, sessionID string, recStart time.Time, evs []devEnvelope, sessionWAV []byte, expectedVoices avExpectedVoices, regionGeos map[string]float64) avTimelineReport {
	rep := avTimelineReport{RunID: runID, SessionID: sessionID, RecordingStartedAt: recStart, Lines: []avLineRecord{}}
	type obsRec struct {
		at    time.Time
		state string
	}
	type decRec struct {
		at     time.Time
		region string
		gen    int64
		msg    string
		seq    int64
	}
	type comRec struct {
		at                      time.Time
		trace, occ, msg, region string
		lifetime                uint64
		gen, seq                int64
		speaker                 string
		speakerKnown            bool
	}
	obsGroups := map[string][]obsRec{}
	obsByTrace := map[string][]obsRec{}
	var coms []comRec
	var decs []decRec
	// The read-through commits (Amendment 1): the occurrence ids of the
	// abstained lines (the commit payload's read-through marker, message_id
	// "unknown", ADR 0136). They are NOT lines of the timeline — the gate
	// counts their audio as phantom instead — but the scan needs the ids to
	// name which phantoms were read-throughs.
	readThroughOcc := map[string]bool{}
	for _, e := range evs {
		at, err := time.Parse(time.RFC3339Nano, e.OccurredAt)
		if err != nil {
			continue
		}
		switch e.Name {
		case "text.observed":
			var d struct {
				RegionID string `json:"region_id"`
				Lifetime uint64 `json:"lifetime"`
				Text     string `json:"text"`
				State    string `json:"text_state"`
			}
			if len(e.Data) == 0 || json.Unmarshal(e.Data, &d) != nil || d.Text == "" || cuIsMesaNoise(d.Text) {
				continue
			}
			k := avObsKey(d.RegionID, d.Lifetime)
			obsGroups[k] = append(obsGroups[k], obsRec{at: at, state: d.State})
			if e.TraceID != "" {
				obsByTrace[e.TraceID] = append(obsByTrace[e.TraceID], obsRec{at: at, state: d.State})
			}
		case "message.committed":
			var d struct {
				Occ          string `json:"dialogue_occurrence_id"`
				Msg          string `json:"message_id"`
				Region       string `json:"region_id"`
				Lifetime     uint64 `json:"lifetime"`
				Generation   int64  `json:"generation"`
				ObsSeq       int64  `json:"observation_seq"`
				Eligible     *bool  `json:"speech_eligible"`
				SpeakerID    string `json:"speaker_id"`
				SpeakerKnown bool   `json:"speaker_known"`
			}
			if len(e.Data) == 0 || json.Unmarshal(e.Data, &d) != nil || d.Occ == "" || d.Msg == "" {
				continue
			}
			if d.Msg == "unknown" {
				readThroughOcc[d.Occ] = true
				continue // the read-through is not a timeline line (the gate counts its audio as phantom)
			}
			if d.Eligible != nil && !*d.Eligible {
				continue
			}
			coms = append(coms, comRec{at: at, trace: e.TraceID, occ: d.Occ, msg: d.Msg, region: d.Region, lifetime: d.Lifetime, gen: d.Generation, seq: d.ObsSeq, speaker: d.SpeakerID, speakerKnown: d.SpeakerKnown})
		case "decision.resolved":
			var d struct {
				Region     string `json:"region_id"`
				Msg        string `json:"message_id"`
				Generation int64  `json:"generation"`
				ObsSeq     int64  `json:"observation_seq"`
			}
			if len(e.Data) == 0 || json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			decs = append(decs, decRec{at: at, region: d.Region, gen: d.Generation, msg: d.Msg, seq: d.ObsSeq})
		}
	}

	// One line per committed occurrence, stream order; a repeat commit of
	// the same occurrence is the reducer's dedupe in evidence (counted).
	seen := map[string]bool{}
	for _, c := range coms {
		if seen[c.occ] {
			rep.DuplicateCommits++
			continue
		}
		seen[c.occ] = true
		// The line's expected voice: the resolveVoice rule (the catch-up
		// test's same per-speaker expectation) — a commit attributed to a
		// speaker resolves to the character's voice, every other line (the
		// unattributed) to the narrator's. The attribution is the commit's
		// own (speaker_id + speaker_known): a known speaker is a character,
		// an absent one is the narrator's fallback.
		expected := expectedVoices.Narrator
		if expectedVoices.SpeakerByMessage != nil {
			if sp := expectedVoices.SpeakerByMessage[c.msg]; sp != "" && sp == expectedVoices.CharacterID {
				expected = expectedVoices.Character
			}
		} else if c.speakerKnown && c.speaker != "" {
			expected = expectedVoices.Character
		}
		centreY, geoKnown := regionGeos[c.region]
		line := avLineRecord{
			OccurrenceID: c.occ, TraceID: c.trace, MessageID: c.msg,
			RegionID: c.region, TCommitMS: avMS(recStart, c.at),
			ExpectedVoiceID: expected,
			BoxPosition:     avBoxPosition(centreY, geoKnown),
		}
		// The box's readings at/before the commit: in a live session each page
		// turn mints its own trace_id on StateAdvancing (internal/session/apply.go),
		// so obsByTrace isolates in-place page turns that share lifetime=1;
		// synthetic unit streams fall back to the region+lifetime group.
		obsList := obsByTrace[c.trace]
		if len(obsList) == 0 {
			obsList = obsGroups[avObsKey(c.region, c.lifetime)]
		}
		// The settle is the LAST ready at/before the commit, and the appear is
		// the first reading after the box's last dismissal: a region whose
		// lifetime spans a scene transition (junk reads that went "ready",
		// dismissed, then the real box) would otherwise date the line from
		// the junk — run av-20261022T023133Z reported a 9.3 s settled→audio
		// for a line that committed 7 ms after its real settle.
		var appear, settled, lastPre time.Time
		for _, o := range obsList {
			if o.at.After(c.at) {
				continue
			}
			if strings.EqualFold(o.state, "dismissed") || strings.EqualFold(o.state, "absent") {
				appear = time.Time{}
				settled = time.Time{}
				continue
			}
			if appear.IsZero() {
				appear = o.at
			}
			lastPre = o.at
			if strings.EqualFold(o.state, "ready") {
				settled = o.at
			}
		}
		if settled.IsZero() && !lastPre.IsZero() {
			settled = lastPre
		}
		line.TBoxAppearMS = avMS(recStart, appear)
		line.TTextSettledMS = avMS(recStart, settled)
		// The Clef's resolution on this commit's identity, at/before the
		// commit (a late one still names itself).
		var pre, late decRec
		for _, r := range decs {
			if r.region != c.region || r.gen != c.gen || r.msg != c.msg || (c.seq > 0 && r.seq > 0 && r.seq != c.seq) {
				continue
			}
			late = r
			if !r.at.After(c.at) {
				pre = r
			}
		}
		if !pre.at.IsZero() {
			line.TClefDoneMS = avMS(recStart, pre.at)
		} else if !late.at.IsZero() {
			line.TClefDoneMS = avMS(recStart, late.at)
		}
		rep.Lines = append(rep.Lines, line)
	}

	// The speech side. A line spoken by its own utterance carries its own
	// occurrence on every speech event. A line the catch-up policy batched
	// (ADR-0122) is spoken inside a merged RUN whose events carry the run's
	// own occurrence plus message_ids (the covered lines in commit order):
	// the run's requested/ready rows map the run occurrence onto its lines,
	// and the run's started/ended stamps are the covered lines' playback
	// span. Only the run's FIRST line becomes audible at the run's start; the
	// later lines follow it inside the same audio, so they are marked
	// Batched with their RunPosition and kept out of the idle latency gate.
	byOcc := make(map[string]int, len(rep.Lines))
	for i := range rep.Lines {
		byOcc[rep.Lines[i].OccurrenceID] = i
	}
	runLines := map[string][]int{} // run occurrence → covered line indices
	// A cross-region duplicate (a box seen through two profile regions) is
	// never routed: its one terminal is skipped/duplicate_region. It must not
	// take the run's audio from the copy that was routed (run
	// av-20261021T210125Z pinned a run on the duplicate, leaving the spoken
	// copy with no audio).
	dupOcc := map[string]bool{}
	for _, e := range evs {
		if e.Name != "speech.ended" {
			continue
		}
		var d struct {
			Occ    string `json:"dialogue_occurrence_id"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(e.Data, &d) == nil && d.Reason == "duplicate_region" {
			dupOcc[d.Occ] = true
		}
	}
	for _, e := range evs {
		if e.Name != "speech.requested" && e.Name != "speech.ready" {
			continue
		}
		var d avSpeechData
		if len(e.Data) == 0 || json.Unmarshal(e.Data, &d) != nil || d.Occ == "" || len(d.MessageIDs) == 0 {
			continue
		}
		if _, own := byOcc[d.Occ]; own {
			continue
		}
		if _, seenRun := runLines[d.Occ]; seenRun {
			continue
		}
		used := map[int]bool{}
		var idx []int
		for _, mid := range d.MessageIDs {
			// The covered line: the latest committed occurrence of that
			// message at/before the run (a repeat line elsewhere in the
			// conversation is a different occurrence).
			best := -1
			for j := range rep.Lines {
				if rep.Lines[j].MessageID == mid && !used[j] && !rep.Lines[j].Batched && !dupOcc[rep.Lines[j].OccurrenceID] {
					best = j
				}
			}
			if best >= 0 {
				used[best] = true
				idx = append(idx, best)
			}
		}
		for pos, j := range idx {
			rep.Lines[j].Batched = true
			rep.Lines[j].RunPosition = pos
			rep.Lines[j].RunOccurrenceID = d.Occ
		}
		runLines[d.Occ] = idx
	}
	terminal := map[string]bool{}
	for _, e := range evs {
		var d avSpeechData
		if len(e.Data) == 0 || json.Unmarshal(e.Data, &d) != nil || (d.Occ == "" && d.Utterance == "") {
			continue
		}
		if d.Occ == "" {
			d.Occ = d.Utterance
		}
		var targets []int
		if i, ok := byOcc[d.Occ]; ok {
			targets = []int{i}
		} else if idx, ok := runLines[d.Occ]; ok {
			targets = idx
		} else {
			continue
		}
		for _, i := range targets {
			line := &rep.Lines[i]
			switch e.Name {
			case "speech.ready":
				if line.TTSReadyMS == 0 {
					if at, ok := avParseAt(e.OccurredAt); ok {
						line.TTSReadyMS = avMS(recStart, at)
					}
				}
				line.FromCache = d.FromCache
			case "speech.started":
				at, _ := avParseAt(e.OccurredAt)
				if d.StartedAt != "" {
					if sa, ok := avParseAt(d.StartedAt); ok {
						at = sa
					}
				}
				if line.TAudioStartEventMS == 0 {
					line.TAudioStartEventMS = avMS(recStart, at)
				}
				if line.clipAudioStartMS == 0 {
					line.clipAudioStartMS = avMS(recStart, at)
				}
			case "speech.ended":
				if d.Result == "" {
					continue // an ended without a result is not a terminal
				}
				terminal[line.OccurrenceID] = true
				if line.EndResult == "" {
					line.EndResult = d.Result
				}
				// The end instant the clip slicer's window needs (an
				// interrupted line still has its ended_at).
				if line.TAudioEndMS == 0 && d.Result != "skipped" {
					at, _ := avParseAt(e.OccurredAt)
					if d.EndedAt != "" {
						if ea, ok := avParseAt(d.EndedAt); ok {
							at = ea
						}
					}
					line.TAudioEndMS = avMS(recStart, at)
				}
			case "speech.cancelled":
				terminal[line.OccurrenceID] = true
				if line.EndResult == "" {
					line.EndResult = "cancelled"
				}
			}
			if line.VoiceID == "" {
				line.VoiceID = d.VoiceID
			}
		}
	}

	// The phantom scan (Amendment 1): every speech.started the timeline's
	// lines do not explain. A started names the occurrence of the utterance
	// it begins (the wiring's correlation; the scheduler's raw event falls
	// back to the utterance id, the same occurrence). It is explained when
	// the occurrence is a committed line's, or a merged run's that covers
	// at least one committed line (the run's message_ids joined it onto
	// them above). Everything else — no committed line at all, or a line
	// the timeline does not hold (the read-throughs) — is phantom: audio
	// with no line to account for it (the board's "speech with no box" is
	// exactly this class).
	for _, e := range evs {
		if e.Name != "speech.started" {
			continue
		}
		var d avSpeechData
		if len(e.Data) == 0 || json.Unmarshal(e.Data, &d) != nil {
			continue
		}
		occ := d.Occ
		if occ == "" {
			occ = d.Utterance
		}
		if occ == "" {
			continue
		}
		if _, ok := byOcc[occ]; ok {
			continue
		}
		if idx, ok := runLines[occ]; ok && len(idx) > 0 {
			continue
		}
		rep.PhantomUtterances++
		if readThroughOcc[occ] {
			rep.PhantomReadThroughs++
		}
	}
	// The acoustic half: the audible onset in the recorded track (the
	// fallback is the event stamp when the window is silent).
	for i := range rep.Lines {
		line := &rep.Lines[i]
		line.Index = i
		if line.TAudioStartEventMS > 0 {
			end := line.TAudioEndMS + 500
			if line.TAudioEndMS == 0 {
				end = line.TAudioStartEventMS + 3000
			}
			if on, ok := avDetectRMSOnsetMS(sessionWAV, line.TAudioStartEventMS, end, -40.0); ok {
				line.TAudioStartRMSMS = on
			} else {
				line.TAudioStartRMSMS = line.TAudioStartEventMS // silent window: the event stamp
			}
		}
	}
	// Queued behind speech: the line committed while ANOTHER line's audio
	// was audible (the player advanced during playback). Its audio cannot
	// start until the playing line ends (the catch-up policy keeps the
	// playing line), so its settled→audio is queueing time, not session
	// latency — it is reported separately and excluded from the idle gate.
	for i := range rep.Lines {
		l := &rep.Lines[i]
		for j := range rep.Lines {
			o := &rep.Lines[j]
			if i == j || o.TAudioStartRMSMS == 0 || o.TAudioEndMS == 0 {
				continue
			}
			if o.RunOccurrenceID != "" && o.RunOccurrenceID == l.RunOccurrenceID {
				continue // the same run: one audio, not a queue
			}
			if l.TCommitMS >= o.TAudioStartRMSMS && l.TCommitMS < o.TAudioEndMS {
				l.QueuedBehindSpeech = true
				break
			}
		}
	}
	var settled, appear, commit, queued []int64
	for i := range rep.Lines {
		line := &rep.Lines[i]
		line.VoiceMatch = line.VoiceID != "" && line.VoiceID == line.ExpectedVoiceID
		audible := line.TAudioStartRMSMS
		if audible > 0 && (!line.Batched || line.RunPosition == 0) {
			if line.TTextSettledMS > 0 {
				line.SettledToAudioMS = audible - line.TTextSettledMS
			}
			if line.TBoxAppearMS > 0 {
				line.AppearToAudioMS = audible - line.TBoxAppearMS
			}
			line.CommitToAudioMS = audible - line.TCommitMS
			if line.QueuedBehindSpeech {
				queued = append(queued, line.CommitToAudioMS)
			} else {
				if line.TTextSettledMS > 0 {
					settled = append(settled, line.SettledToAudioMS)
				}
				if line.AppearToAudioMS > 0 { // 0 = the box's appear was never observed
					appear = append(appear, line.AppearToAudioMS)
				}
				commit = append(commit, line.CommitToAudioMS)
			}
		}
		if line.FromCache {
			rep.CacheHits++
		}
		if !terminal[line.OccurrenceID] && line.TAudioStartEventMS == 0 && line.TTSReadyMS == 0 {
			rep.MissedLines++ // committed, never routed, never terminal
		}
		if line.EndResult == "skipped" {
			rep.SkippedLines++
		}
	}
	rep.IdleSamples = len(settled)
	rep.SettledToAudioP50MS = avPercentileMS(settled, 50)
	rep.SettledToAudioP95MS = avPercentileMS(settled, 95)
	rep.AppearToAudioP50MS = avPercentileMS(appear, 50)
	rep.AppearToAudioP95MS = avPercentileMS(appear, 95)
	rep.CommitToAudioP50MS = avPercentileMS(commit, 50)
	rep.CommitToAudioP95MS = avPercentileMS(commit, 95)
	rep.QueuedCommitToAudioP50MS = avPercentileMS(queued, 50)
	rep.QueuedCommitToAudioP95MS = avPercentileMS(queued, 95)
	// A stale start is the ADR-0122 backlog at the timeline level: a line's
	// OWN utterance began after a NEWER line had already committed (the
	// player moved on before this audio became audible). A batched line is
	// the catch-up policy's by design (its max age fences a stale run).
	for i := range rep.Lines {
		l := &rep.Lines[i]
		if l.Batched || l.TAudioStartEventMS == 0 || l.TCommitMS == 0 {
			continue
		}
		for j := range rep.Lines {
			if i != j && rep.Lines[j].TCommitMS > l.TCommitMS && l.TAudioStartEventMS > rep.Lines[j].TCommitMS {
				rep.StaleStarts++
				break
			}
		}
	}
	return rep
}
