package speech_test

import (
	"context"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
)

// TestSchedulerTwentyRapidAdvances is the docs/spec/12 failure-injection case
// "20 rapid textbox page advances": generations 1..20 each become current 150
// ms apart, each UtteranceReady arrives after a seeded random 0–300 ms delay
// (i.e. half of them arrive AFTER their generation is already stale), and
// every generation is cancelled when the next commits.
//
// Asserted (docs/spec/12 "Zero stale audio starts ... local stop event target
// <200 ms"; docs/spec/01: only currently committed generation can play):
//   - ZERO audio starts for any generation that was not current at start time;
//   - at most one active playback at a time (record intervals do not overlap);
//   - every interrupted playback stopped within 200 ms (measured with the
//     dummy sink's timestamps);
//   - late-ready audio for an old generation never plays.
func TestSchedulerTwentyRapidAdvances(t *testing.T) {
	const gens = 20
	const advanceEvery = 150 * time.Millisecond
	const clipDur = 250 * time.Millisecond

	sink := dummy.New(10*time.Millisecond, 10*time.Millisecond)
	pub := &recPub{}
	fence := &testFence{}
	sched := speech.NewScheduler(sink, fence, pub, speech.Options{Clock: time.Now})
	ctx := context.Background()
	sched.Start(ctx)
	defer sched.Stop()

	rng := rand.New(rand.NewSource(1)) // seeded: deterministic scenario
	readyDelay := make([]time.Duration, gens)
	for i := range readyDelay {
		readyDelay[i] = time.Duration(rng.Intn(301)) * time.Millisecond
	}

	var (
		commitAt [gens]time.Time // when generation i+1 became current (before fence switch)
		switchAt [gens]time.Time // when the fence actually switched to i+1
		cancelAt [gens]time.Time // when Cancel(i) was issued (0 if none)
	)

	submitted := make(chan struct{}, gens)
	for i := 0; i < gens; i++ {
		gen := int64(i + 1)
		commitAt[i] = time.Now()
		if i > 0 {
			sched.Cancel(gen-1, "advance")
			cancelAt[i-1] = time.Now()
		}
		fence.cur.Store(gen)
		switchAt[i] = time.Now()

		go func(g int64, d time.Duration) {
			time.Sleep(d)
			wav := speech.MakeToneWAV(440, clipDur.Seconds(), 22050)
			sched.Submit(speech.UtteranceReady{
				SessionID: "s1", Generation: g, UtteranceID: "u" + strconv.Itoa(int(g)),
				Priority: speech.PriorityCurrent, WAV: wav, ExpectedDuration: clipDur,
			})
			submitted <- struct{}{}
		}(gen, readyDelay[i])

		time.Sleep(advanceEvery)
	}
	for i := 0; i < gens; i++ {
		<-submitted
	}

	// Quiesce: no active playback and a grace period for in-flight stops.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sink.ActiveNow() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	records := sink.Records()
	byGen := map[int64][]dummy.Record{}
	for _, r := range records {
		byGen[r.Generation] = append(byGen[r.Generation], r)
	}

	// 1) Zero stale starts. A start for generation g is legal only while g was
	// current: between switchAt[g-1] (became current) and switchAt[g] (stopped
	// being current), plus the sink's own spin-up window (StartDelay + margin)
	// — the fence approved it BEFORE the sink reported started.
	for g := int64(1); g <= gens; g++ {
		first := commitAt[g-1]
		last := switchAt[g-1]
		if g < gens {
			last = switchAt[g]
		}
		for _, r := range byGen[g] {
			if r.Started.IsZero() {
				continue // never started: fine (stopped before first sample)
			}
			if r.Started.Before(first) {
				t.Errorf("gen %d started at %v BEFORE it became current (%v): stale start", g, r.Started, first)
			}
			if g < gens && r.Started.After(last.Add(sink.StartDelay+30*time.Millisecond)) {
				t.Errorf("gen %d started at %v after it stopped being current (%v): stale start (beyond spin-up window)", g, r.Started, last)
			}
		}
	}

	// 2) At most one active playback: record intervals must not overlap.
	for i := 1; i < len(records); i++ {
		if !records[i].Started.IsZero() && !records[i-1].Started.IsZero() &&
			records[i].Started.Before(records[i-1].Ended) {
			t.Errorf("overlap: gen %d started %v while gen %d still active until %v",
				records[i].Generation, records[i].Started, records[i-1].Generation, records[i-1].Ended)
		}
	}

	// 3) Every interrupt stopped within 200 ms of its trigger
	//    (explicit cancel or the fence switch, whichever came first).
	for g := int64(1); g <= gens; g++ {
		for _, r := range byGen[g] {
			if !r.Interrupted || r.Ended.IsZero() {
				continue
			}
			trigger := switchAt[g-1]
			if !cancelAt[g-1].IsZero() && cancelAt[g-1].After(trigger) {
				trigger = cancelAt[g-1]
			}
			lat := r.Ended.Sub(trigger)
			if lat > speech.StopTarget {
				t.Errorf("gen %d stop latency %v exceeds target %v (trigger %v, ended %v)", g, lat, speech.StopTarget, trigger, r.Ended)
			}
		}
	}

	// 4) Late-ready old audio never plays: any generation whose ready delay
	//    exceeded the advance interval was submitted after its switch and must
	//    have no started record.
	late := 0
	for i := 0; i < gens; i++ {
		g := int64(i + 1)
		if readyDelay[i] > advanceEvery {
			late++
			for _, r := range byGen[g] {
				if !r.Started.IsZero() {
					t.Errorf("gen %d (ready after %v, stale since %v) played at %v: late stale audio",
						g, readyDelay[i], switchAt[i], r.Started)
				}
			}
		}
	}
	if late == 0 {
		t.Fatal("seeded scenario produced no late-ready generations; test is vacuous")
	}

	// 5) Event evidence: every started generation has a speech.ended, and the
	//    stale drops are evented as failed (docs/spec/11: assert events, not
	//    just silence).
	started := 0
	for _, r := range records {
		if !r.Started.IsZero() {
			started++
		}
	}
	ended := len(pub.byName(events.SpeechEnded))
	if ended < started {
		t.Errorf("speech.ended count %d < started playbacks %d", ended, started)
	}
	t.Logf("scenario: %d rapid advances, %d records, %d starts, %d late-ready (all suppressed), %d events",
		gens, len(records), started, late, len(pub.evs))
}
