package speech_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/filesink"
)

// TestStopJoinsActivePlaybackTerminal pins the shutdown join: Stop() must not
// return while the active playback's final delivery is in flight. The
// filesink writes its evidence WAV only at completion (staged as .tmp,
// renamed on finish), so the directory holding exactly one WAV and no .tmp
// after Stop is the join proof — before the join, Stop raced the sink's
// fade sleep + write + rename and could observe an empty (or .tmp-only)
// directory. The terminal speech.ended event must be published too: a
// playback that is stopped at shutdown still ends, it does not vanish.
func TestStopJoinsActivePlaybackTerminal(t *testing.T) {
	dir := t.TempDir()
	sink, err := filesink.New(dir)
	if err != nil {
		t.Fatalf("filesink: %v", err)
	}
	pub := &recPub{}
	fence := &testFence{}
	fence.cur.Store(1)

	sched := speech.NewScheduler(sink, fence, pub, speech.Options{StopFade: 50 * time.Millisecond})
	sched.Start(context.Background())

	// A 300 ms clip: long enough that Stop lands mid-playback.
	sr, ch, bits := 22050, 1, 16
	clip := time.Duration(float64(sr) * 0.3)
	pcm := make([]byte, int(clip)*sr*ch*bits/1000)
	wav := speech.EncodeWAV(pcm, sr, ch, bits)

	if !sched.Submit(speech.UtteranceReady{UtteranceID: "u1", Generation: 1, WAV: wav}) {
		t.Fatal("Submit: shed before the test began")
	}
	// Let the playback become active (first 10 ms slice delivered).
	time.Sleep(60 * time.Millisecond)

	if err := sched.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read sink dir: %v", err)
	}
	var wavs, tmps int
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".tmp"):
			tmps++
		case strings.HasSuffix(e.Name(), ".wav"):
			wavs++
		}
	}
	if tmps != 0 {
		t.Errorf("temporary file(s) left in the sink dir after Stop: %d", tmps)
	}
	if wavs != 1 {
		t.Errorf("evidence WAV(s) after Stop = %d, want 1 (the stopped playback's delivered prefix)", wavs)
	}
	// The stopped playback got its terminal event, not silence.
	if evs := pub.byName(events.SpeechEnded); len(evs) != 1 {
		t.Fatalf("speech.ended events after Stop = %d, want 1", len(evs))
	}
}
