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
	ClipMP4            string  `json:"clip_mp4,omitempty"`
	ClipWAV            string  `json:"clip_wav,omitempty"`
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
// ordered; p50/p95 are the warm-run gate's inputs.
func avBuildTimeline(runID, sessionID string, recStart time.Time, evs []devEnvelope, sessionWAV []byte, expectedVoice string) avTimelineReport {
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
	}
	obsGroups := map[string][]obsRec{}
	obsByTrace := map[string][]obsRec{}
	var coms []comRec
	var decs []decRec
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
				Occ        string `json:"dialogue_occurrence_id"`
				Msg        string `json:"message_id"`
				Region     string `json:"region_id"`
				Lifetime   uint64 `json:"lifetime"`
				Generation int64  `json:"generation"`
				ObsSeq     int64  `json:"observation_seq"`
				Eligible   *bool  `json:"speech_eligible"`
			}
			if len(e.Data) == 0 || json.Unmarshal(e.Data, &d) != nil || d.Occ == "" || d.Msg == "" || d.Msg == "unknown" || (d.Eligible != nil && !*d.Eligible) {
				continue
			}
			coms = append(coms, comRec{at: at, trace: e.TraceID, occ: d.Occ, msg: d.Msg, region: d.Region, lifetime: d.Lifetime, gen: d.Generation, seq: d.ObsSeq})
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
		line := avLineRecord{
			OccurrenceID: c.occ, TraceID: c.trace, MessageID: c.msg,
			RegionID: c.region, TCommitMS: avMS(recStart, c.at),
			ExpectedVoiceID: expectedVoice,
		}
		// The box's readings at/before the commit: in a live session each page
		// turn mints its own trace_id on StateAdvancing (internal/session/apply.go),
		// so obsByTrace isolates in-place page turns that share lifetime=1;
		// synthetic unit streams fall back to the region+lifetime group.
		obsList := obsByTrace[c.trace]
		if len(obsList) == 0 {
			obsList = obsGroups[avObsKey(c.region, c.lifetime)]
		}
		var appear, settled, lastPre time.Time
		for _, o := range obsList {
			if o.at.After(c.at) {
				continue
			}
			if appear.IsZero() {
				appear = o.at
			}
			lastPre = o.at
			if settled.IsZero() && strings.EqualFold(o.state, "ready") {
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

	// The speech side, joined on the occurrence the wiring correlates every
	// speech event with (a merged run's events carry the run's own
	// occurrence, so a run-covered line keeps its own events' stamps).
	byOcc := make(map[string]int, len(rep.Lines))
	for i := range rep.Lines {
		byOcc[rep.Lines[i].OccurrenceID] = i
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
		i, ok := byOcc[d.Occ]
		if !ok {
			if len(d.MessageIDs) == 1 {
				for j := range rep.Lines {
					if rep.Lines[j].MessageID == d.MessageIDs[0] {
						switch e.Name {
						case "speech.ready":
							if rep.Lines[j].TTSReadyMS == 0 {
								if at, ok := avParseAt(e.OccurredAt); ok {
									rep.Lines[j].TTSReadyMS = avMS(recStart, at)
								}
							}
							rep.Lines[j].FromCache = d.FromCache
						case "speech.started":
							at, _ := avParseAt(e.OccurredAt)
							if d.StartedAt != "" {
								if sa, ok := avParseAt(d.StartedAt); ok {
									at = sa
								}
							}
							if rep.Lines[j].clipAudioStartMS == 0 {
								rep.Lines[j].clipAudioStartMS = avMS(recStart, at)
							}
						}
						if rep.Lines[j].VoiceID == "" {
							rep.Lines[j].VoiceID = d.VoiceID
						}
					}
				}
			}
			continue
		}
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
			terminal[d.Occ] = true
			// The end instant the clip slicer's window needs (an interrupted
			// line still has its ended_at; the result names the outcome).
			if line.TAudioEndMS == 0 {
				at, _ := avParseAt(e.OccurredAt)
				if d.EndedAt != "" {
					if ea, ok := avParseAt(d.EndedAt); ok {
						at = ea
					}
				}
				line.TAudioEndMS = avMS(recStart, at)
			}
		case "speech.cancelled":
			terminal[d.Occ] = true
		}
		if line.VoiceID == "" {
			line.VoiceID = d.VoiceID
		}
	}

	// The acoustic half: the onset in the recorded track (the fallback is
	// the event stamp), the stage-to-stage deltas and the counters.
	var settled, appear, commit []int64
	for i := range rep.Lines {
		line := &rep.Lines[i]
		line.Index = i
		if line.TAudioStartEventMS > 0 {
			if on, ok := avDetectRMSOnsetMS(sessionWAV, line.TAudioStartEventMS, line.TAudioEndMS+500, -40.0); ok {
				line.TAudioStartRMSMS = on
			} else {
				line.TAudioStartRMSMS = line.TAudioStartEventMS // silent window: the event stamp
			}
		}
		line.VoiceMatch = line.VoiceID != "" && line.VoiceID == line.ExpectedVoiceID
		if line.TAudioStartEventMS > 0 {
			if line.TTextSettledMS > 0 {
				line.SettledToAudioMS = line.TAudioStartEventMS - line.TTextSettledMS
			}
			if line.TBoxAppearMS > 0 {
				line.AppearToAudioMS = line.TAudioStartEventMS - line.TBoxAppearMS
			}
			line.CommitToAudioMS = line.TAudioStartEventMS - line.TCommitMS
			if line.SettledToAudioMS > 0 {
				settled = append(settled, line.SettledToAudioMS)
			}
			if line.AppearToAudioMS > 0 { // 0 = the box's appear was never observed
				appear = append(appear, line.AppearToAudioMS)
			}
			commit = append(commit, line.CommitToAudioMS)
		}
		if line.FromCache {
			rep.CacheHits++
		}
		if !terminal[line.OccurrenceID] && line.TAudioStartEventMS == 0 && line.TTSReadyMS == 0 {
			rep.MissedLines++ // committed, never routed, never terminal
		}
	}
	rep.SettledToAudioP50MS = avPercentileMS(settled, 50)
	rep.SettledToAudioP95MS = avPercentileMS(settled, 95)
	rep.AppearToAudioP50MS = avPercentileMS(appear, 50)
	rep.AppearToAudioP95MS = avPercentileMS(appear, 95)
	rep.CommitToAudioP50MS = avPercentileMS(commit, 50)
	rep.CommitToAudioP95MS = avPercentileMS(commit, 95)
	// A stale start is the ADR-0122 backlog at the timeline level: this
	// line's playback began after a NEWER line had already committed (the
	// player moved on before this audio became audible).
	for i := range rep.Lines {
		l := &rep.Lines[i]
		if l.TAudioStartEventMS == 0 || l.TCommitMS == 0 {
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
