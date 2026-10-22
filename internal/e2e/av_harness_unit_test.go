// Package e2e — the A/V harness's unit half (ADR 0143): the RMS-onset
// detector on a synthetic tone-in-silence WAV, and the timeline
// correlation on a synthetic dev event stream. It runs in the ordinary
// go test (no GN_GATE, no emulator, no providers): the metrics the live
// harness gates on are proven deterministic here.
package e2e

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/speech"
)

// avToneWAV renders the unit test's reference track: a 2 s 16 kHz 16-bit
// mono WAV, silent from 0..400 ms, a 440 Hz tone (0.25 amplitude, the
// sink tests' reference level) from 400..1400 ms, silent again.
func avToneWAV(t *testing.T) []byte {
	t.Helper()
	const rate = 16000
	data := make([]byte, 2*rate*2)
	for i := 0; i < 2*rate; i++ {
		ms := int64(i) * 1000 / rate
		if ms < 400 || ms >= 1400 {
			continue
		}
		v := int16(0.25 * 32767 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		binary.LittleEndian.PutUint16(data[i*2:], uint16(v))
	}
	return speech.EncodeWAV(data, rate, 1, 16)
}

// avEv builds one synthetic dev envelope: the store's fixed-width UTC
// occurred_at (the same shape the reducer stamps), a compact JSON payload.
func avEv(seq int64, name, at string, payload map[string]any) devEnvelope {
	t, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return devEnvelope{Seq: seq, EventID: "ev" + fmt.Sprint(seq), SessionID: "sess-av", TraceID: "trace" + fmt.Sprint(seq), Name: name, OccurredAt: at, Data: t}
}

func TestAVHarnessMetricsAndRMSOnset(t *testing.T) {
	wav := avToneWAV(t)

	// (1) The RMS onset: the 440 Hz tone starts at 400 ms, so the first
	// 20 ms window at/over -40 dBFS must open at 400 ms (window
	// quantization allows the answer to land in 380..440).
	onset, ok := avDetectRMSOnsetMS(wav, 0, 2000, -40.0)
	if !ok {
		t.Fatalf("avDetectRMSOnsetMS: no onset in the tone span (0..2000 ms, -40 dBFS)")
	}
	if onset < 380 || onset > 440 {
		t.Fatalf("avDetectRMSOnsetMS onset = %d ms; want within [380, 440] (the tone starts at 400 ms)", onset)
	}
	if _, ok := avDetectRMSOnsetMS(wav, 0, 300, -40.0); ok {
		t.Fatalf("avDetectRMSOnsetMS: onset in the 0..300 ms silence (want none)")
	}
	if _, ok := avDetectRMSOnsetMS(wav, 1500, 2000, -40.0); ok {
		t.Fatalf("avDetectRMSOnsetMS: onset in the 1500..2000 ms silence (want none)")
	}

	// (2) The timeline (the Amendment 1 shape): six committed lines on the
	// shared clock T0 = 10:00:00Z — line 2's audio starts at 6200 ms,
	// after line 3 committed at 6000 (the ADR-0122 backlog shape the
	// StaleStarts counter names), and lines 5-6 are merged into one
	// catch-up run (one audio, two lines: the run's started must NOT be
	// phantom) — plus one repeat commit, one read-through commit with its
	// audio (phantom #1) and one bare started no commit explains
	// (phantom #2).
	recStart, _ := time.Parse(time.RFC3339Nano, "2026-10-21T10:00:00.000Z")
	stamp := func(ms int) string {
		return recStart.Add(time.Duration(ms) * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	obs := func(seq int64, ms int, region string, lifetime uint64, state string) devEnvelope {
		return avEv(seq, "text.observed", stamp(ms), map[string]any{
			"region_id": region, "lifetime": lifetime, "text": "line " + fmt.Sprint(lifetime), "text_state": state,
		})
	}
	// commit is one committed line; speaker "" is the unattributed line
	// (the narrator's, the resolveVoice fallback).
	commit := func(seq int64, ms int, occ, msg, region string, lifetime uint64, speaker string, gen, obsSeq int64) devEnvelope {
		return avEv(seq, "message.committed", stamp(ms), map[string]any{
			"dialogue_occurrence_id": occ, "message_id": msg, "region_id": region,
			"lifetime": lifetime, "speaker_id": speaker, "speaker_known": speaker != "",
			"generation": gen, "observation_seq": obsSeq,
		})
	}
	// commitRT is the read-through commit (ADR 0136): the abstained line's
	// shape — message_id "unknown", speech_eligible false.
	commitRT := func(seq int64, ms int, occ, region string, lifetime uint64, gen int64) devEnvelope {
		return avEv(seq, "message.committed", stamp(ms), map[string]any{
			"dialogue_occurrence_id": occ, "message_id": "unknown", "region_id": region,
			"lifetime": lifetime, "speech_eligible": false,
			"generation": gen, "observation_seq": 0,
		})
	}
	dec := func(seq int64, ms int, msg string, gen, obsSeq int64) devEnvelope {
		return avEv(seq, "decision.resolved", stamp(ms), map[string]any{
			"region_id": "r1", "message_id": msg, "generation": gen, "observation_seq": obsSeq,
		})
	}
	ready := func(seq int64, ms int, occ string, cache bool, voice string) devEnvelope {
		return avEv(seq, "speech.ready", stamp(ms), map[string]any{
			"dialogue_occurrence_id": occ, "from_cache": cache, "voice_id": voice,
		})
	}
	// requestedRun is the merged run's speech.requested (the run's own
	// occurrence plus the covered lines' message ids).
	requestedRun := func(seq int64, ms int, runOcc string, msgIDs []string, voice string) devEnvelope {
		return avEv(seq, "speech.requested", stamp(ms), map[string]any{
			"dialogue_occurrence_id": runOcc, "message_ids": msgIDs, "voice_id": voice,
		})
	}
	started := func(seq int64, ms int, occ, voice string) devEnvelope {
		return avEv(seq, "speech.started", stamp(ms), map[string]any{
			"dialogue_occurrence_id": occ, "started_at": stamp(ms), "voice_id": voice,
		})
	}
	ended := func(seq int64, ms int, occ string) devEnvelope {
		return avEv(seq, "speech.ended", stamp(ms), map[string]any{
			"dialogue_occurrence_id": occ, "result": "ended", "ended_at": stamp(ms),
		})
	}
	evs := []devEnvelope{
		// Line 1: the full chain, cached, attributed (the expected voice is
		// the character's).
		obs(1, 1000, "r1", 1, "REVEALING"), obs(2, 1500, "r1", 1, "STABILIZING"), obs(3, 2000, "r1", 1, "READY"),
		dec(4, 2400, "m-1", 1, 10), commit(5, 2500, "occ-1", "m-1", "r1", 1, "char-av", 1, 10),
		ready(6, 2700, "occ-1", true, "av-voice-char"), started(7, 2800, "occ-1", "av-voice-char"),
		ended(8, 4800, "occ-1"),
		// Line 2: unattributed (the expected voice is the narrator's),
		// spoken in a third voice (VoiceMatch false), the stale start.
		obs(9, 5000, "r1", 2, "REVEALING"), obs(10, 5200, "r1", 2, "READY"),
		dec(11, 5400, "m-2", 2, 11), commit(12, 5500, "occ-2", "m-2", "r1", 2, "", 2, 11),
		ready(13, 5600, "occ-2", false, "av-voice-other"), started(14, 6200, "occ-2", "av-voice-other"),
		ended(15, 8200, "occ-2"),
		// Line 3: no box readings, no Clef row — the deltas off those are 0.
		commit(16, 6000, "occ-3", "m-3", "r2", 3, "char-av", 3, 12),
		ready(17, 6100, "occ-3", false, "av-voice-char"), started(18, 7000, "occ-3", "av-voice-char"),
		ended(19, 8000, "occ-3"),
		// Line 4: the bottom box, the third idle latency sample.
		obs(20, 9000, "r3", 4, "READY"),
		commit(21, 9200, "occ-4", "m-4", "r3", 4, "char-av", 4, 13),
		ready(22, 9300, "occ-4", true, "av-voice-char"), started(23, 9400, "occ-4", "av-voice-char"),
		ended(24, 10400, "occ-4"),
		// Lines 5-6: the catch-up run (one audio, two lines).
		commit(25, 13000, "occ-5", "m-5", "r1", 6, "char-av", 5, 14),
		commit(26, 13500, "occ-6", "m-6", "r1", 7, "char-av", 6, 15),
		requestedRun(27, 13600, "occ-run-1", []string{"m-5", "m-6"}, "av-voice-char"),
		ready(28, 13700, "occ-run-1", true, "av-voice-char"), started(29, 13800, "occ-run-1", "av-voice-char"),
		ended(30, 15500, "occ-run-1"),
		// The repeat commit: the reducer's dedupe (counted, not a line).
		commit(31, 12500, "occ-1", "m-1", "r1", 1, "char-av", 1, 10),
		// Phantom #1: the read-through commit and the audio it routed.
		commitRT(32, 11000, "occ-rt", "r1", 8, 7),
		started(33, 11200, "occ-rt", "av-voice-char"),
		// Phantom #2: the bare started no commit explains.
		started(34, 12000, "occ-ghost", "av-voice-char"),
	}
	// The region rows' centre Y over the frame height: r1 the middle band,
	// r2 the top band, r3 the bottom band (the ADR-0137 placement shape).
	regionGeos := map[string]float64{"r1": 0.50, "r2": 0.20, "r3": 0.80}
	expectedVoices := avExpectedVoices{Character: "av-voice-char", Narrator: "av-voice-narr"}
	rep := avBuildTimeline("run-av-unit", "sess-av", recStart, evs, wav, expectedVoices, regionGeos, nil)

	if len(rep.Lines) != 6 {
		t.Fatalf("timeline: %d lines; want 6 (repeat and read-through commits excluded): %+v", len(rep.Lines), rep)
	}
	l1, l2, l3 := &rep.Lines[0], &rep.Lines[1], &rep.Lines[2]
	l4, l5, l6 := &rep.Lines[3], &rep.Lines[4], &rep.Lines[5]
	// Line 1: the full chain, cached, the expected voice, the middle box.
	if l1.OccurrenceID != "occ-1" || l1.TBoxAppearMS != 1000 || l1.TTextSettledMS != 2000 || l1.TCommitMS != 2500 ||
		l1.TClefDoneMS != 2400 || l1.TTSReadyMS != 2700 || l1.TAudioStartEventMS != 2800 || l1.TAudioEndMS != 4800 ||
		!l1.FromCache || !l1.VoiceMatch || l1.ExpectedVoiceID != "av-voice-char" || l1.BoxPosition != "middle" {
		t.Fatalf("line 1 stage instants wrong: %+v", l1)
	}
	if l1.SettledToAudioMS != 800 || l1.AppearToAudioMS != 1800 || l1.CommitToAudioMS != 300 {
		t.Fatalf("line 1 deltas wrong: settled=%d appear=%d commit=%d (want 800/1800/300)", l1.SettledToAudioMS, l1.AppearToAudioMS, l1.CommitToAudioMS)
	}
	// Line 2: unattributed — the expected voice is the narrator's, the line
	// spoke in a third (VoiceMatch false), the stale start, the middle box.
	if l2.OccurrenceID != "occ-2" || l2.TBoxAppearMS != 5000 || l2.TTextSettledMS != 5200 || l2.TCommitMS != 5500 ||
		l2.TAudioStartEventMS != 6200 || l2.TAudioEndMS != 8200 || l2.VoiceMatch || l2.VoiceID != "av-voice-other" ||
		l2.ExpectedVoiceID != "av-voice-narr" || l2.BoxPosition != "middle" {
		t.Fatalf("line 2 stage instants wrong: %+v", l2)
	}
	if l2.SettledToAudioMS != 1000 || l2.AppearToAudioMS != 1200 {
		t.Fatalf("line 2 deltas wrong: settled=%d appear=%d (want 1000/1200)", l2.SettledToAudioMS, l2.AppearToAudioMS)
	}
	// Line 3: no box readings, no Clef row — the deltas off those are 0;
	// the top box (region r2).
	if l3.OccurrenceID != "occ-3" || l3.TBoxAppearMS != 0 || l3.TTextSettledMS != 0 || l3.TCommitMS != 6000 ||
		l3.TAudioStartEventMS != 7000 || l3.SettledToAudioMS != 0 || l3.AppearToAudioMS != 0 || l3.CommitToAudioMS != 1000 ||
		!l3.VoiceMatch || l3.BoxPosition != "top" {
		t.Fatalf("line 3 stage instants wrong: %+v", l3)
	}
	// Line 4: the bottom box (region r3), the third idle sample.
	if l4.OccurrenceID != "occ-4" || l4.TBoxAppearMS != 9000 || l4.TTextSettledMS != 9000 || l4.TCommitMS != 9200 ||
		l4.TAudioStartEventMS != 9400 || l4.BoxPosition != "bottom" || !l4.VoiceMatch || !l4.FromCache {
		t.Fatalf("line 4 stage instants wrong: %+v", l4)
	}
	if l4.SettledToAudioMS != 400 || l4.AppearToAudioMS != 400 || l4.CommitToAudioMS != 200 {
		t.Fatalf("line 4 deltas wrong: settled=%d appear=%d commit=%d (want 400/400/200)", l4.SettledToAudioMS, l4.AppearToAudioMS, l4.CommitToAudioMS)
	}
	// Lines 5-6: the merged run — both lines carry the run's audio span and
	// their run position; the first line is the run's idle sample.
	if !l5.Batched || l5.RunPosition != 0 || l5.RunOccurrenceID != "occ-run-1" ||
		!l6.Batched || l6.RunPosition != 1 || l6.RunOccurrenceID != "occ-run-1" {
		t.Fatalf("run batch marking wrong: line 5 %+v, line 6 %+v", l5, l6)
	}
	if l5.TAudioStartEventMS != 13800 || l5.TAudioEndMS != 15500 || l6.TAudioStartEventMS != 13800 || l6.TAudioEndMS != 15500 {
		t.Fatalf("run audio span wrong: line 5 start/end=%d/%d, line 6 start/end=%d/%d (want 13800/15500 each)",
			l5.TAudioStartEventMS, l5.TAudioEndMS, l6.TAudioStartEventMS, l6.TAudioEndMS)
	}
	// The gate's percentile inputs (nearest rank over the idle samples only:
	// lines 3 and 5 have no box readings, the run's second line (6) is the
	// run's audio, not its own — settled is over lines 1, 2, 4; appear adds
	// nothing new, so p50 is the median 1200 over [400, 1200, 1800]; commit
	// is over all five idle lines, [200, 300, 700, 800, 1000]).
	if rep.IdleSamples != 3 {
		t.Fatalf("idle samples = %d; want 3 (lines 1, 2, 4)", rep.IdleSamples)
	}
	if rep.SettledToAudioP50MS != 800 || rep.SettledToAudioP95MS != 1000 {
		t.Fatalf("settled p50/p95 = %d/%d; want 800/1000", rep.SettledToAudioP50MS, rep.SettledToAudioP95MS)
	}
	if rep.AppearToAudioP50MS != 1200 || rep.AppearToAudioP95MS != 1800 {
		t.Fatalf("appear p50/p95 = %d/%d; want 1200/1800", rep.AppearToAudioP50MS, rep.AppearToAudioP95MS)
	}
	if rep.CommitToAudioP50MS != 700 || rep.CommitToAudioP95MS != 1000 {
		t.Fatalf("commit p50/p95 = %d/%d; want 700/1000", rep.CommitToAudioP50MS, rep.CommitToAudioP95MS)
	}
	// The consistency counters: one stale start (line 2 after line 3's
	// commit), one duplicate commit, four cache hits (lines 1 and 4 plus the
	// run's cached ready, attributed to both lines it covers), nothing
	// missed — and the Amendment 1 phantoms: the read-through's audio and
	// the bare started, the run's started explained by its covered lines.
	if rep.StaleStarts != 1 || rep.DuplicateCommits != 1 || rep.CacheHits != 4 || rep.MissedLines != 0 {
		t.Fatalf("counters stale=%d dup=%d cache=%d missed=%d; want 1/1/4/0", rep.StaleStarts, rep.DuplicateCommits, rep.CacheHits, rep.MissedLines)
	}
	if rep.PhantomUtterances != 4 || rep.PhantomReadThroughs != 1 {
		t.Fatalf("phantoms = %d (read-through %d); want 4 (1)", rep.PhantomUtterances, rep.PhantomReadThroughs)
	}
	if len(rep.PhantomRows) != 4 {
		t.Fatalf("phantom rows = %d; want 4: %+v", len(rep.PhantomRows), rep.PhantomRows)
	}
}
