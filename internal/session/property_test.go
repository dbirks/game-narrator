package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/recognition"
)

// propertyInterleavings is docs/spec/12's property-test requirement ("event reducer
// never accepts stale generation") at a count, not as an aspiration. Two thousand
// interleavings of five message classes over a forty-step timeline is roughly the
// point at which a schedule-dependent bug stops hiding: each run reorders
// observations, out-of-order ranking replies, late decisions, late speech stages
// and control events against whatever generation fence happens to be standing.
//
// It is a fixed count rather than a testing.Short()-skipped fuzz because the
// acceptance gate is "the property holds", and a gate that disappears on the flag a
// busy CI job passes is not a gate.
const propertyInterleavings = 2000

// propTexts are deliberately *materially* different from one another, plus the two
// shapes the state machine has to tell apart from a page change: a typewriter
// extension of a line already seen, and a one-glyph misread of it. Without those
// two the property test would only ever exercise the commit path.
var propTexts = []string{
	"THE DOOR OPENS",
	"A GUARD BARS IT",
	"SWORDS CROSS ABOVE",
	"THE STAIRS DESCEND",
	"WATER RUNS BELOW",
	"A LIGHT FLICKERS",
	"SOMETHING BREATHES",
	"THE WALLS ARE WARM",
	"A VOICE SAYS TURN BACK",
	"NO DOOR BEHIND YOU",
}

// propJitter is propTexts[0] with one OCR-confusable glyph and one dropped space:
// the same line as far as the vote is concerned.
const propJitter = "THE DOOR 0PENS"

// step is one message in a generated schedule, with every parameter fixed *before*
// execution. Resolving a relative observation reference at apply time would make
// the two replay runs of the same seed different schedules rather than the same
// schedule applied twice, which is the only thing the determinism half of the
// property is testing.
type step struct {
	kind     Kind
	text     string
	genDelta int // added to the active generation when the message is stamped
	obsAgo   int // ranking/decision target: 0 is the newest observation
	abstain  bool
	margin   float64
	prefix   bool // answer with the matcher's PREDICTIVE_PREFIX reason
	speech   SpeechKind
	occKind  int // 0 current occurrence, 1 an occurrence from earlier in the run, 2 unknown
	ctl      ControlKind
	dismiss  bool
	region   string
}

func TestPropertyStaleGenerationNeverApplied(t *testing.T) {
	ctx := context.Background()
	for iteration := 0; iteration < propertyInterleavings; iteration++ {
		rng := rand.New(rand.NewSource(int64(iteration) + 1))
		steps := generateSteps(rng, 40)

		h := newHarness(t, DefaultConfig())
		stream := runSteps(t, h, ctx, steps, iteration)

		// Determinism: the same schedule into a fresh reducer must reproduce the
		// same event stream byte for byte. docs/spec/12 requires it of ranking, and
		// the reducer is where the events a fixture replays are actually made, so
		// the requirement belongs here too — a reducer whose output depended on Go's
		// map iteration order could not be diffed against a recorded trace at all.
		again := newHarness(t, DefaultConfig())
		stream2 := runBare(again, ctx, steps, iteration)
		if stream != stream2 {
			t.Fatalf("iteration %d: the same schedule produced different event streams:\n%s\n---\n%s",
				iteration, stream, stream2)
		}
	}
}

// generateSteps builds one schedule: mostly observations, with ranking replies that
// may be addressed to an older frame, decisions that may arrive late, speech stages
// that may name a stale generation or a superseded occurrence, and occasional
// controls.
func generateSteps(rng *rand.Rand, n int) []step {
	var steps []step
	for i := 0; i < n; i++ {
		switch roll := rng.Intn(100); {
		case roll < 45: // observation
			steps = append(steps, step{kind: KindObservation, text: propTexts[rng.Intn(len(propTexts))], region: "bottom"})
		case roll < 52: // a flicker frame, or a slow typewriter's next character
			if rng.Intn(2) == 0 {
				steps = append(steps, step{kind: KindObservation, text: propJitter, region: "bottom"})
			} else {
				steps = append(steps, step{kind: KindObservation, text: "THE DOOR", region: "bottom"})
			}
		case roll < 68: // ranking reply
			steps = append(steps, step{
				kind: KindRanking, obsAgo: rng.Intn(3), genDelta: 0,
				abstain: rng.Intn(6) == 0, margin: float64(rng.Intn(70)) / 100,
				prefix: rng.Intn(3) == 0, region: "bottom",
			})
		case roll < 76: // decision, sometimes late
			steps = append(steps, step{
				kind: KindDecision, obsAgo: rng.Intn(4),
				genDelta: func() int {
					if rng.Intn(3) == 0 {
						return -2 // an answer that took longer than the page did
					}
					return 0
				}(),
				region: "bottom",
			})
		case roll < 88: // speech stage
			kinds := []SpeechKind{SpeechReady, SpeechStarted, SpeechEnded, SpeechFailed}
			occ := 0
			genDelta := 0
			if rng.Intn(4) == 0 {
				occ = 1 // a clip belonging to an occurrence this box no longer stands behind
				genDelta = -2
			}
			if rng.Intn(12) == 0 {
				occ = 2 // a clip for something this session never committed
			}
			steps = append(steps, step{kind: KindSpeech, speech: kinds[rng.Intn(len(kinds))], occKind: occ, genDelta: genDelta, region: "bottom"})
		case roll < 93: // dismissal: the box vanishes
			steps = append(steps, step{kind: KindObservation, dismiss: true, region: "bottom"})
		default: // control
			ctls := []ControlKind{ControlPause, ControlResume, ControlStop, ControlProfileChanged}
			ctl := ctls[rng.Intn(len(ctls))]
			if ctl == ControlStop && rng.Intn(4) != 0 {
				ctl = ControlResume // a stop would end the timeline early and stop exercising the rest
			}
			steps = append(steps, step{kind: KindControl, ctl: ctl, region: "bottom"})
		}
	}
	return steps
}

// runSteps applies a schedule, asserts the invariants and returns the serialized
// event stream. With assert off it performs the same work and returns the same
// stream, which is what lets the determinism half of the property compare two runs
// of one schedule (a second t.Fatalf on a replay would report the same failure
// twice and hide whether the streams differed).
func runSteps(t *testing.T, h *harness, ctx context.Context, steps []step, iteration int) string {
	t.Helper()
	return run(t, h, ctx, steps, iteration, true)
}

func runBare(h *harness, ctx context.Context, steps []step, iteration int) string {
	return run(nil, h, ctx, steps, iteration, false)
}

func run(t *testing.T, h *harness, ctx context.Context, steps []step, iteration int, assert bool) string {
	var obsSeqs []int64
	var occurrences []string
	committed := map[string]bool{}
	// cancelledOcc tracks occurrences whose utterance was stopped, so a start for a
	// cancelled utterance can be caught against the log rather than trusted to the
	// phase table that is supposed to prevent it.
	cancelledOcc := map[string]bool{}
	var lastCommitGen int64
	var sb strings.Builder
	fatalf := func(format string, args ...any) {
		if assert {
			t.Fatalf("iteration %d: "+format, append([]any{iteration}, args...)...)
		}
		panic(fmt.Sprintf(format, args...))
	}

	for i, st := range steps {
		if h.r.Stopped() {
			break
		}
		active := h.r.Generation()
		var (
			res Result
			err error
		)
		h.seq++
		h.ts += int64(200 * time.Millisecond)
		stamped := active + int64(st.genDelta)
		if stamped < 0 {
			stamped = 0
		}
		head := Header{SessionID: h.r.st.SessionID, Seq: h.seq, Generation: stamped, ProfileRevision: h.profile}

		switch st.kind {
		case KindObservation:
			obs := recognition.Observation{
				FrameSeq: h.seq, CaptureTimestampNS: h.ts, SourceClock: "monotonic",
				RegionID: st.region, RegionPresent: !st.dismiss,
			}
			if !st.dismiss {
				obs.RegionHash = fmt.Sprintf("h-%d", h.seq)
				obs.OCR = ocrResult(st.text, 0.9)
			}
			res, err = h.r.Apply(ctx, ObservationMsg{Header: head, RegionID: st.region, Observation: obs})
			if !st.dismiss {
				obsSeqs = append(obsSeqs, h.seq)
				if len(obsSeqs) > 8 {
					obsSeqs = obsSeqs[len(obsSeqs)-8:]
				}
			}
		case KindRanking:
			target := newest(obsSeqs, st.obsAgo)
			if target == 0 {
				continue
			}
			rk := matcher.Ranking{Margin: st.margin, Reason: matcher.ReasonExact, Abstain: st.abstain}
			if st.prefix {
				rk.Reason = matcher.ReasonPredictivePrefix
			}
			if !st.abstain {
				rk.Candidates = []matcher.Candidate{
					{MessageID: fmt.Sprintf("msg-%d", target), Probability: 0.8},
					{MessageID: fmt.Sprintf("msg-%d-x", target), Probability: 0.8 - st.margin},
				}
			}
			res, err = h.r.Apply(ctx, RankingMsg{
				Header: head, RegionID: st.region, ObservationSeq: target,
				Text: h.text(st.region), Ranking: rk,
			})
		case KindDecision:
			target := newest(obsSeqs, st.obsAgo)
			if target == 0 {
				continue
			}
			res, err = h.r.Apply(ctx, DecisionMsg{
				Header: head, RegionID: st.region, ObservationSeq: target,
				MessageID: "", SpeakerID: "narrator", SpeakerKnown: true,
				SpeechEligible: true, Accepted: true, Provider: "fake",
			})
		case KindSpeech:
			occ := ""
			switch st.occKind {
			case 0:
				occ = h.occurrence(st.region)
			case 1:
				occ = older(occurrences, 2)
			case 2:
				occ = "occ-unknown-999"
			}
			if occ == "" {
				continue
			}
			res, err = h.r.Apply(ctx, SpeechMsg{Header: head, RegionID: st.region, OccurrenceID: occ, Kind: st.speech})
		case KindControl:
			res, err = h.r.Apply(ctx, ControlMsg{Header: head, Kind: st.ctl, NewProfileRevision: fmt.Sprintf("rev-%d", i)})
		}
		if err != nil {
			fatalf("step %d (%s) returned an error: %v", i, st.kind, err)
			continue
		}

		// --- the properties ----------------------------------------------------
		// The fence is stamped-vs-active, not the intended delta: a delta that would
		// go below zero is clamped, and a message stamped 0 into a session that has
		// not committed anything is simply current.
		if stamped < active {
			if res.Applied {
				fatalf("step %d (%s) was applied although it was stamped generation %d while %d was active",
					i, st.kind, stamped, active)
			}
			if len(res.Events) > 0 {
				fatalf("step %d (%s) emitted %v although it was stamped generation %d below the active %d",
					i, st.kind, namesOf(res.Events), stamped, active)
			}
		}
		for _, ev := range res.Events {
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				fatalf("step %d: %s carries a payload that is not an object: %v", i, ev.Name, err)
			}
			if g, ok := data["generation"]; ok {
				gf, isNum := g.(float64)
				if !isNum {
					fatalf("step %d: event %s carries a non-numeric generation %v", i, ev.Name, g)
				}
				// The reducer's central promise: nothing it publishes names a
				// generation older than the one that was active when it decided to
				// publish it. A commit's own event may name the generation it just
				// created, and nothing may name one it did not create.
				if int64(gf) < active {
					fatalf("step %d (%s) published %s naming generation %d while %d was active: %s",
						i, st.kind, ev.Name, int64(gf), active, string(ev.Data))
				}
				if int64(gf) > active+1 {
					fatalf("step %d (%s) published %s naming generation %d, more than one ahead of %d",
						i, st.kind, ev.Name, int64(gf), active)
				}
			}
			if ev.Name == events.MessageCommitted {
				content, _ := data["message_id"].(string)
				if content == UnknownMessage {
					content, _ = data["normalized_text"].(string)
				}
				key := fmt.Sprintf("%s|%v|%s", data["region_id"], data["lifetime"], content)
				if committed[key] {
					fatalf("step %d committed the same occurrence content twice: %s", i, key)
				}
				committed[key] = true
				gen := int64(0)
				if g, ok := data["generation"]; ok {
					gen = int64(g.(float64))
				}
				if gen <= lastCommitGen {
					fatalf("step %d: commit generations are not strictly increasing (%d then %d)", i, lastCommitGen, gen)
				}
				lastCommitGen = gen
				if occ, ok := data["dialogue_occurrence_id"].(string); ok && occ != "" {
					occurrences = append(occurrences, occ)
				}
			}
			if ev.Name == events.SpeechStarted {
				// A start may never name an occurrence that has been cancelled: this
				// is docs/spec/12's "no stale audio starts after a new committed
				// dialogue event", asserted against the log rather than trusted to a
				// phase table.
				occ, _ := data["dialogue_occurrence_id"].(string)
				if cancelledOcc[occ] {
					fatalf("step %d started audio for cancelled occurrence %s", i, occ)
				}
			}
			if ev.Name == events.SpeechCancelled {
				if occ, ok := data["dialogue_occurrence_id"].(string); ok {
					cancelledOcc[occ] = true
				}
			}
			fmt.Fprintf(&sb, "%s|%s|%s|%d|%s\n", ev.Name, ev.TraceID, ev.CausationID, ev.Seq, string(ev.Data))
		}
		if now := h.r.Generation(); now < active {
			fatalf("step %d: the generation counter went backwards (%d then %d)", i, active, now)
		}
	}
	// Every emitted event was well-formed: the fake publisher validates each one
	// against the envelope contract, so a payload this package invented that the API
	// layer could not store or a browser could not decode fails here rather than in
	// review.
	if got := h.r.Stats().PublishErrors; got != 0 {
		fatalf("%d publish errors, last: %s", got, h.r.Stats().PublishErrorsLast)
	}
	return sb.String()
}

// cancelled tracks occurrences whose utterance was stopped. Declared alongside run
// so the property text reads as one rule per block.
func newest(seqs []int64, ago int) int64 {
	if len(seqs) == 0 {
		return 0
	}
	idx := len(seqs) - 1 - ago
	if idx < 0 {
		idx = 0
	}
	return seqs[idx]
}

func older(items []string, back int) string {
	if len(items) == 0 {
		return ""
	}
	idx := len(items) - back
	if idx < 0 {
		return items[0]
	}
	return items[idx]
}

// TestConcurrentSendersRaceFree runs the contract in docs/spec/01 against the race
// detector: the capture loop hands work to the reducer from its own goroutine, the
// ranking worker from another, and none of them may block or corrupt the state the
// reducer owns.
//
// Six producers each drive one region (six regions is the shape a multi-box profile
// has: dialogue, choices, subtitles), repeating one line so each box settles. A
// single ranking goroutine reads the observation each region last reported — which
// is how a real ranking worker learns an observation_seq, from the text.observed
// event — and answers it. That interleaving is the point: the answer frequently
// arrives after the box has moved on, and it must be fenced rather than applied.
func TestConcurrentSendersRaceFree(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- h.r.Run(ctx) }()

		const producers, each = 6, 150
		var wg sync.WaitGroup
		for p := 0; p < producers; p++ {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				region := fmt.Sprintf("region-%d", p)
				text := propTexts[p%len(propTexts)]
				base := time.Now().UnixNano()
				for i := 0; i < each; i++ {
					// Seq is left 0 so Send assigns it from the reducer's own counter:
					// that is the live path, and it is the only way several producers can
					// share one dedupe cursor without stepping on each other.
					h.r.Send(ObservationMsg{
						Header:   Header{SessionID: h.r.st.SessionID, ProfileRevision: h.profile},
						RegionID: region,
						Observation: recognition.Observation{
							FrameSeq: int64(i + 1), CaptureTimestampNS: base + int64(i)*int64(200*time.Millisecond),
							SourceClock: "monotonic", RegionID: region, RegionPresent: true,
							RegionHash: fmt.Sprintf("race-%d-%d", p, i), OCR: ocrResult(text, 0.9),
						},
					})
				}
			}(p)
		}

		// The ranking side, driven from the test goroutine so it can read the reducer's
		// reported observation seq the way a worker reads the event stream.
		var rankWG sync.WaitGroup
		rankWG.Add(1)
		rankErr := make(chan string, 1)
		go func() {
			defer rankWG.Done()
			for n := 0; n < 4000; n++ {
				st := h.r.State()
				if st.Stopped {
					return
				}
				for _, reg := range st.Regions {
					if reg.ObservationSeq == 0 || reg.Text == "" {
						continue
					}
					// Seq 0: the reducer numbers it. A producer that stamps its own
					// sequence is a replay tool replaying a recorded log, and mixing the
					// two would make half of these look like duplicates.
					res, err := h.r.Apply(ctx, RankingMsg{
						Header:         Header{SessionID: h.r.st.SessionID, Generation: st.Generation, ProfileRevision: h.profile},
						RegionID:       reg.RegionID,
						ObservationSeq: reg.ObservationSeq,
						Text:           reg.Text,
						Ranking: matcher.Ranking{Reason: matcher.ReasonExact, Margin: 0.4,
							Candidates: []matcher.Candidate{{MessageID: "msg-" + reg.RegionID, Probability: 0.9}}},
					})
					if err != nil {
						rankErr <- fmt.Sprintf("ranking apply: %v", err)
						return
					}
					_ = res
				}
				time.Sleep(time.Millisecond)
			}
		}()

		wg.Wait()
		// Let the queue drain and the rankings land before stopping.
		deadline := time.Now().Add(5 * time.Second)
		for h.r.InboxDepth() > 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		for i := 0; i < 20; i++ {
			st := h.r.State()
			for _, reg := range st.Regions {
				if reg.ObservationSeq == 0 || reg.Text == "" {
					continue
				}
				if _, err := h.r.Apply(ctx, RankingMsg{
					Header:   Header{SessionID: h.r.st.SessionID, Generation: st.Generation, ProfileRevision: h.profile},
					RegionID: reg.RegionID, ObservationSeq: reg.ObservationSeq, Text: reg.Text,
					Ranking: matcher.Ranking{Reason: matcher.ReasonExact, Margin: 0.4,
						Candidates: []matcher.Candidate{{MessageID: "msg-" + reg.RegionID, Probability: 0.9}}},
				}); err != nil {
					t.Fatalf("ranking apply: %v", err)
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		select {
		case msg := <-rankErr:
			t.Fatal(msg)
		default:
		}
		cancel()
		rankWG.Wait()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v", err)
		}

		st := h.r.Stats()
		if st.Commits == 0 {
			t.Fatalf("900 observations and hundreds of rankings committed nothing; stats %+v", st)
		}
		seen := map[string]bool{}
		for _, e := range h.pub.byName(events.MessageCommitted) {
			d := dataOf(t, e)
			key := fmt.Sprintf("%v|%v|%v", d["region_id"], d["lifetime"], d["message_id"])
			if seen[key] {
				t.Fatalf("concurrent producers produced a duplicate commitment of %s", key)
			}
			seen[key] = true
		}
		if got := h.r.Generation(); got != int64(len(seen)) {
			t.Fatalf("generation %d does not equal the %d distinct committed occurrences", got, len(seen))
		}
		// The inbox is bounded and said so: whatever was shed under load is counted per
		// class, which is what tells an operator a worker died instead of hiding it.
		if st.DroppedInboxObservations+st.DroppedInboxRankings == 0 {
			t.Log("nothing was dropped under load (the host kept up)")
		}
	})
}
