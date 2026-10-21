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

	// (2) The timeline: two full lines plus one bare commit and one repeat
	// commit, on the shared clock T0 = 10:00:00Z. Line 2's audio starts at
	// 6200 ms, after line 3 committed at 6000 — the ADR-0122 backlog shape
	// the StaleStarts counter names.
	recStart, _ := time.Parse(time.RFC3339Nano, "2026-10-21T10:00:00.000Z")
	stamp := func(ms int) string {
		return recStart.Add(time.Duration(ms) * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	obs := func(seq int64, ms int, lifetime uint64, state string) devEnvelope {
		return avEv(seq, "text.observed", stamp(ms), map[string]any{
			"region_id": "r1", "lifetime": lifetime, "text": "line " + fmt.Sprint(lifetime), "text_state": state,
		})
	}
	commit := func(seq int64, ms int, occ, msg string, lifetime uint64, gen, obsSeq int64) devEnvelope {
		return avEv(seq, "message.committed", stamp(ms), map[string]any{
			"dialogue_occurrence_id": occ, "message_id": msg, "region_id": "r1",
			"lifetime": lifetime, "generation": gen, "observation_seq": obsSeq,
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
		obs(1, 1000, 1, "REVEALING"), obs(2, 1500, 1, "STABILIZING"), obs(3, 2000, 1, "READY"),
		dec(4, 2400, "m-1", 1, 10), commit(5, 2500, "occ-1", "m-1", 1, 1, 10),
		ready(6, 2700, "occ-1", true, "av-voice-char"), started(7, 2800, "occ-1", "av-voice-char"),
		ended(8, 4800, "occ-1"),
		obs(9, 5000, 2, "REVEALING"), obs(10, 5200, 2, "READY"),
		dec(11, 5400, "m-2", 2, 11), commit(12, 5500, "occ-2", "m-2", 2, 2, 11),
		ready(13, 5600, "occ-2", false, "av-voice-other"), started(14, 6200, "occ-2", "av-voice-other"),
		ended(15, 8200, "occ-2"),
		commit(16, 6000, "occ-3", "m-3", 3, 3, 12),
		ready(17, 6100, "occ-3", false, "av-voice-char"), started(18, 7000, "occ-3", "av-voice-char"),
		ended(19, 8000, "occ-3"),
		commit(20, 6100, "occ-1", "m-1", 1, 1, 10), // repeat: the reducer's dedupe
	}
	rep := avBuildTimeline("run-av-unit", "sess-av", recStart, evs, wav, "av-voice-char")

	if len(rep.Lines) != 3 {
		t.Fatalf("timeline: %d lines; want 3 (repeat commit excluded): %+v", len(rep.Lines), rep)
	}
	l1, l2, l3 := &rep.Lines[0], &rep.Lines[1], &rep.Lines[2]
	// Line 1: the full chain, cached, the expected voice.
	if l1.OccurrenceID != "occ-1" || l1.TBoxAppearMS != 1000 || l1.TTextSettledMS != 2000 || l1.TCommitMS != 2500 ||
		l1.TClefDoneMS != 2400 || l1.TTSReadyMS != 2700 || l1.TAudioStartEventMS != 2800 || l1.TAudioEndMS != 4800 ||
		!l1.FromCache || !l1.VoiceMatch {
		t.Fatalf("line 1 stage instants wrong: %+v", l1)
	}
	if l1.SettledToAudioMS != 800 || l1.AppearToAudioMS != 1800 || l1.CommitToAudioMS != 300 {
		t.Fatalf("line 1 deltas wrong: settled=%d appear=%d commit=%d (want 800/1800/300)", l1.SettledToAudioMS, l1.AppearToAudioMS, l1.CommitToAudioMS)
	}
	// Line 2: a different voice (VoiceMatch false) and the stale start.
	if l2.OccurrenceID != "occ-2" || l2.TBoxAppearMS != 5000 || l2.TTextSettledMS != 5200 || l2.TCommitMS != 5500 ||
		l2.TAudioStartEventMS != 6200 || l2.TAudioEndMS != 8200 || l2.VoiceMatch || l2.VoiceID != "av-voice-other" {
		t.Fatalf("line 2 stage instants wrong: %+v", l2)
	}
	if l2.SettledToAudioMS != 1000 || l2.AppearToAudioMS != 1200 {
		t.Fatalf("line 2 deltas wrong: settled=%d appear=%d (want 1000/1200)", l2.SettledToAudioMS, l2.AppearToAudioMS)
	}
	// Line 3: no box readings, no Clef row — the deltas off those are 0.
	if l3.OccurrenceID != "occ-3" || l3.TBoxAppearMS != 0 || l3.TTextSettledMS != 0 || l3.TCommitMS != 6000 ||
		l3.TAudioStartEventMS != 7000 || l3.SettledToAudioMS != 0 || l3.AppearToAudioMS != 0 || l3.CommitToAudioMS != 1000 || !l3.VoiceMatch {
		t.Fatalf("line 3 stage instants wrong: %+v", l3)
	}
	// The gate's percentile inputs (nearest rank over the positive samples
	// only: line 3 has no box readings, so it contributes appear/settled
	// samples only where it observed them — settled absent, commit present).
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
	// commit), one duplicate commit, one cache hit, nothing missed.
	if rep.StaleStarts != 1 || rep.DuplicateCommits != 1 || rep.CacheHits != 1 || rep.MissedLines != 0 {
		t.Fatalf("counters stale=%d dup=%d cache=%d missed=%d; want 1/1/1/0", rep.StaleStarts, rep.DuplicateCommits, rep.CacheHits, rep.MissedLines)
	}
}
