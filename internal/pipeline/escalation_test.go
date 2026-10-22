package pipeline_test

import (
	"context"
	"encoding/json"
	"image"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/replay"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// The escalation tests (ADR-0115 §1) run the live pipeline on a corpus of
// near-twin lines — the live shape, where the same box shows similar lines —
// and a stable OCR reading that is a garble between several of them. The
// shared harness corpus is deliberately *distinct*: its garbles rank a winner
// the calibrated softmax drives to ~1.0, so a fuzzy-below-bar or an abstained
// shortlist (the two escalation entry points) are unreachable on it. The twin
// corpus is where they live.

const (
	escTrueLine = "The old lamp at the pier went dark at nine."
	escTrueID   = "synthmsg-lamp-true"
	// The stable reading: an OCR garble between the true line and its "noon"
	// twin, ranked MATCH_FUZZY with a calibrated top probability below the
	// escalation bar (measured through the pipeline: top ≈ 0.51, runner-up
	// ≈ 0.38, gate margin ≈ 0.003) — the live line's shape, where one stable
	// read could not settle the line and only the later stable reads and Clef
	// could.
	escGarble = "The old lamp at the pier went dark at noonine."
	// The calibrated commit bar the tests run under: above the garble's
	// measured top probability (≈0.51 through the pipeline's own confidence
	// plumbing) and far below certainty, the way the operator's bar sits. The
	// zero-value default (0.5) is a hair under this corpus's measured top, so
	// the tests name the bar explicitly rather than fighting the calibration
	// to four decimals; the clause is the point, not its default constant.
	escFuzzyBar = 0.6
)

// escTwinLines are the invented near-twin corpus lines (the true line plus the
// eight twins that share its first words the way one box's lines do).
var escTwinLines = []string{
	escTrueLine,
	"The old lamp at the pier went dark at noon.",
	"The old lamp at the pier was lit at nine.",
	"The new lamp at the pier went dark at nine.",
	"The old lamp at the quay went dark at nine.",
	"The old lantern at the pier went dark at nine.",
	"The old lamp at the pier burned dark at nine.",
	"The old lamp at the pier stood dark at nine.",
	"The new lantern at the quay went dark at noon.",
}

func escTwinID(i int) string { return "synthmsg-lamp-tw" + string(rune('a'+i)) }

// escIndex builds the twin corpus: the true line, its near twins, and one far
// distractor so retrieval has somewhere to be wrong.
func escIndex(t *testing.T) *matcher.Index {
	t.Helper()
	msgs := make([]games.Message, 0, len(escTwinLines)+1)
	for i, text := range escTwinLines {
		id := escTwinID(i)
		if i == 0 {
			id = escTrueID
		}
		msgs = append(msgs, games.Message{ID: id, ProfileID: "synth-profile", OriginalText: text, SpeechText: text, TextKind: games.TextDialogue})
	}
	msgs = append(msgs, games.Message{ID: "synthmsg-lamp-far", ProfileID: "synth-profile",
		OriginalText: "The kettle was full of rain by morning.", SpeechText: "The kettle was full of rain by morning.",
		TextKind: games.TextDialogue})
	idx, err := matcher.Build(msgs, nil, matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("matcher.Build: %v", err)
	}
	return idx
}

// garbleOCR is a scripted ocr.Engine that reads every crop as the one stable
// garble — the board's stable-but-uncertain line — with the fake's uniform
// 15/16 word confidence (the same exact-binary value the shared harness uses,
// so the tracker's vote sees the same shape of evidence).
type garbleOCR struct {
	calls atomic.Int64
	text  string
}

func (g *garbleOCR) Name() string { return "garble" }

func (g *garbleOCR) Recognize(ctx context.Context, img image.Image) (ocr.Result, error) {
	_ = ctx
	if img == nil || img.Bounds().Empty() {
		return ocr.Result{}, &ocr.Failure{Kind: ocr.ErrFailed, Engine: "garble", Err: context.DeadlineExceeded}
	}
	g.calls.Add(1)
	const conf = 0.9375
	res := ocr.Result{Text: g.text, Engine: "garble", MeanConf: conf}
	if g.text != "" {
		res.Lines = []ocr.Line{{Text: g.text, Conf: conf, Box: image.Rect(0, 0, 1000, 100)}}
		x := 0
		for _, w := range strings.Fields(g.text) {
			res.Words = append(res.Words, ocr.Word{Text: w, Conf: conf, Box: image.Rect(x, 20, x+30, 80)})
			x += 50
		}
	}
	return res, nil
}

// scriptedDecider records every DecisionRequest and answers per attempt: the
// scripted Clef. capture is what the assertions read — the question the
// pipeline offered (the read text and the capped shortlist) and its answers.
type scriptedDecider struct {
	mu       sync.Mutex
	seen     []pipeline.DecisionRequest
	answer   func(attempt int, req pipeline.DecisionRequest) pipeline.DecisionReply
	attemptN int
}

func (d *scriptedDecider) Decide(ctx context.Context, req pipeline.DecisionRequest) pipeline.DecisionReply {
	_ = ctx
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, req)
	d.attemptN++
	return d.answer(d.attemptN, req)
}

func (d *scriptedDecider) snapshot() []pipeline.DecisionRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]pipeline.DecisionRequest, len(d.seen))
	copy(out, d.seen)
	return out
}

// pickReply answers with the shortlist's runner-up (the model narrows away
// from the local top-1: the disambiguation the escalation exists for) and
// marks the line safe to speak.
func pickReply(req pipeline.DecisionRequest) pipeline.DecisionReply {
	if len(req.Ranking.Candidates) < 2 {
		return pipeline.DecisionReply{Provider: "scripted", Reason: "abstained"}
	}
	pick := req.Ranking.Candidates[1].MessageID
	return pipeline.DecisionReply{
		Msg: &session.DecisionMsg{
			RegionID:       req.RegionID,
			MessageID:      pick,
			SpeechEligible: true,
			Accepted:       true,
			Provider:       "scripted",
			Model:          "scripted-decider",
			Reason:         "answered",
		},
		Provider:   "scripted",
		RequestID:  "scripted-req",
		Model:      "scripted-decider",
		Accepted:   true,
		Reason:     "answered",
		ChosenID:   pick,
		Candidates: candidateIDs(req.Ranking),
	}
}

func abstainReply() pipeline.DecisionReply {
	// A nil Msg is the decider's abstain: the commit proceeds on the local
	// scoring alone and the reply's reason is what the resolved event carries.
	return pipeline.DecisionReply{Provider: "scripted", Reason: "abstained"}
}

func candidateIDs(r matcher.Ranking) []string {
	out := make([]string, 0, len(r.Candidates))
	for _, c := range r.Candidates {
		out = append(out, c.MessageID)
	}
	return out
}

// clockSource wraps the replay source and publishes each frame's source
// timestamp so the pipeline's Now can run on source time: the escalation
// cadence (EscalationRetryAfter) is then measured in the same clock the
// recording uses, and the run is paced by the source, not by the host.
type clockSource struct {
	inner *replay.Source
	last  atomic.Int64
}

func (c *clockSource) ID() string { return c.inner.ID() }

func (c *clockSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	ch, err := c.inner.Start(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan capture.Frame, 8)
	go func() {
		defer close(out)
		for f := range ch {
			c.last.Store(f.CaptureTimestampNS)
			out <- f
		}
	}()
	return out, nil
}

func (c *clockSource) Stats() capture.Stats { return c.inner.Stats() }
func (c *clockSource) Close() error         { return c.inner.Close() }

// newEscalationHarness wires a live-mode Runner to the twin corpus and the
// replay source, the way the shared harness does but with the test's own index
// (the shared corpus cannot express a fuzzy-below-bar reading).
func newEscalationHarness(t *testing.T, eng *garbleOCR, dec *scriptedDecider, controls chan session.Msg,
	mut func(*pipeline.Config)) (*pipeline.Runner, *events.MemStore, *events.Hub, *clockSource) {
	t.Helper()
	// Speed 1: Clef answers in wall time while the cadence runs on source
	// time; at 4× a slow race-instrumented runner ran out of stable frames
	// before the third retry (#50).
	src := &replay.Source{Dir: corpusDir(t), Speed: 1}
	cs := &clockSource{inner: src}
	cfg := pipeline.Config{
		SessionID:       "synthsession",
		ProfileRevision: "1",
		Regions:         []games.Region{corpusRegion(t)},
		OCR:             eng,
		Index:           escIndex(t),
		Preprocess:      steps(),
		Trace:           true,
		Queue:           pipeline.QueueLossless,
		OCRTimeout:      2 * time.Second,
		// The corpus samples at 100 ms per frame: the tracker is tuned to that
		// cadence, the same way the shared harness does it.
		Tracker: recognition.Config{
			VoteWindow:   3,
			StableWindow: 100 * time.Millisecond,
		},
		// The near-twin corpus is where the escalation lives: the acceptance
		// gate is tuned to it (a gate margin of ~0.003 between near twins is
		// the corpus's honest evidence, not a defect), the rest of the policy
		// is the documented default.
		RankOptions: matcher.RankOptions{MinMargin: 0.001, MinProbability: 0.1},
		// The session's uncertainty policy (ADR-0115 §1): adjudication on, the
		// margin clause off (0) so the fuzzy-below-bar clause alone drives the
		// fuzzy case, and the calibrated bar at the corpus's test value (the
		// garble ranks ≈0.51 top through the pipeline; see escFuzzyBar).
		Reducer: session.Config{Adjudicate: true, AdjudicateFuzzyBelow: escFuzzyBar},
		Decider: dec,
		// The escalation budget (ADR-0115 §1): three attempts, 200 ms of source
		// time between them (two corpus frames), the shortlist capped at five.
		EscalationRetryAfter:  200 * time.Millisecond,
		EscalationMaxAttempts: 3,
		DecisionCandidates:    5,
		// Source time is the run's clock: the cadence and the latency stamps
		// are measured in the recording's own seconds.
		Now:      func() time.Time { return time.Unix(0, cs.last.Load()) },
		Controls: controls,
	}
	if mut != nil {
		mut(&cfg)
	}
	r, err := pipeline.New(cfg)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	return r, mem, hub, cs
}

func runEscalation(t *testing.T, r *pipeline.Runner, hub *events.Hub, store *events.MemStore, src capture.Source) (pipeline.Stats, []events.Envelope) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = src.Close()
		hub.Close()
	})
	if err := r.Run(ctx, src, hub); err != nil {
		t.Fatalf("run: %v", err)
	}
	st := r.Stats()
	evs, err := store.After(context.Background(), 0, events.Filter{}, 4096)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return st, evs
}

type escCommit struct {
	MessageID      string `json:"message_id"`
	Reason         string `json:"reason"`
	MatcherReason  string `json:"matcher_reason"`
	SpeechEligible bool   `json:"speech_eligible"`
	Text           string `json:"text"`
	RawText        string `json:"raw_text"`
}

// committedEsc extracts the message.committed payloads, oldest first.
func committedEsc(t *testing.T, evs []events.Envelope) []escCommit {
	t.Helper()
	var out []escCommit
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		var d escCommit
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("message.committed payload: %v", err)
		}
		out = append(out, d)
	}
	return out
}

// TestEscalationFuzzyBelowBarCommitsViaClef is the ADR-0115 §1 core: a stable
// line whose stable read ranks MATCH_FUZZY below the calibrated commit bar is
// offered to Clef with the read text and the top-5 shortlist, and a Clef
// verdict that selects a retrieved candidate commits it as speakable
// (CLEF_DISAMBIGUATED). The local top-1 is *not* what gets said: the model
// narrowed to the runner-up.
func TestEscalationFuzzyBelowBarCommitsViaClef(t *testing.T) {
	eng := &garbleOCR{text: escGarble}
	dec := &scriptedDecider{answer: func(_ int, req pipeline.DecisionRequest) pipeline.DecisionReply {
		return pickReply(req)
	}}
	r, mem, hub, src := newEscalationHarness(t, eng, dec, nil, nil)
	st, evs := runEscalation(t, r, hub, mem, src)

	commits := committedEsc(t, evs)
	if len(commits) != 1 {
		t.Fatalf("commits = %d (%+v), want exactly one (the escalation settles the line once)", len(commits), commits)
	}
	c := commits[0]
	if c.Reason != "CLEF_DISAMBIGUATED" {
		t.Fatalf("commit reason = %q, want CLEF_DISAMBIGUATED (the Clef verdict, not the local fuzzy top-1)", c.Reason)
	}
	if c.MessageID == escTrueID {
		t.Fatalf("commit kept the local top-1 %s: the scripted verdict picked the runner-up, so the commit must be the model's pick", c.MessageID)
	}
	if c.MessageID == "" {
		t.Fatal("commit has no message_id: a Clef verdict on a retrieved candidate is a commit, not a silence")
	}
	if !c.SpeechEligible {
		t.Fatalf("speech_eligible = false, want true: the verdict marked the line safe to speak, and a CLEF_DISAMBIGUATED commit that cannot speak is not the fix (the live line sat unspoken)")
	}
	if c.MatcherReason != "MATCH_FUZZY" {
		t.Fatalf("matcher_reason = %q, want MATCH_FUZZY (the local read was the fuzzy one below the bar)", c.MatcherReason)
	}
	// The commit carries the reading on both scales: the raw read (case kept)
	// is the text the box showed and the model adjudicated, and the folded key
	// is the text machine's identity for it.
	if c.RawText != escGarble {
		t.Fatalf("commit raw_text = %q, want the stable read %q", c.RawText, escGarble)
	}
	if c.Text == "" {
		t.Fatal("commit text is empty: the folded key is the occurrence's identity")
	}

	// The question the pipeline offered Clef: the read text and the capped
	// shortlist, not the whole retrieval pool. RawText is the engine's raw
	// read (case kept); Text is the session's accepted text (the folded key).
	seen := dec.snapshot()
	if len(seen) != 1 {
		t.Fatalf("decisions asked = %d, want 1 (a committed reply ends the escalation; nothing re-asks after the commit)", len(seen))
	}
	ask := seen[0]
	if ask.RawText != escGarble {
		t.Fatalf("decision request raw_text = %q, want the read text %q (the model adjudicates the reading, not an id)", ask.RawText, escGarble)
	}
	if ask.Text == "" {
		t.Fatal("decision request text is empty: the session's accepted text must ride the question too")
	}
	if got := len(ask.Ranking.Candidates); got != 5 {
		t.Fatalf("decision shortlist = %d candidates, want the top-5 cap (DecisionCandidates)", got)
	}
	if ask.Ranking.Reason != "MATCH_FUZZY" {
		t.Fatalf("decision ranking reason = %q, want MATCH_FUZZY", ask.Ranking.Reason)
	}
	if ask.Ranking.Candidates[0].Probability >= escFuzzyBar {
		t.Fatalf("top candidate calibrated p = %.3f, want below the %.2f escalation bar (the clause that sent this read to Clef)", ask.Ranking.Candidates[0].Probability, escFuzzyBar)
	}

	// The counters the report reads: one adjudication asked and applied, one
	// commit, and the invariant-2 counter untouched.
	if st.Decisions != 1 || st.DecisionApplied != 1 {
		t.Fatalf("decisions/applied = %d/%d, want 1/1 (the verdict landed through the fences)", st.Decisions, st.DecisionApplied)
	}
	if st.Commits != 1 {
		t.Fatalf("commits = %d, want 1", st.Commits)
	}
	if st.StaleApplied != 0 {
		t.Fatalf("stale_generation_applied = %d, want 0", st.StaleApplied)
	}
}

// TestEscalationRetriesBoundedAndCommitsOnResume is the retry half of ADR-0115
// §1: the line's ranking keeps coming back uncertain — the same stable read
// ranks MATCH_FUZZY below the calibrated bar every time, and Clef abstains
// twice — so the later stable reads re-rank the same reading on the retry's
// cadence until the bounded attempts run out: no fourth ask, though the box
// stays READY and stable for the rest of the clip. The answer the budget
// finally earns (attempt three) commits on the resume: the retry exists so the
// line is finally said, not just finally known.
func TestEscalationRetriesBoundedAndCommitsOnResume(t *testing.T) {
	eng := &garbleOCR{text: escGarble}
	dec := &scriptedDecider{answer: func(n int, req pipeline.DecisionRequest) pipeline.DecisionReply {
		if n < 3 {
			// The first two attempts Clef abstains: an honest "not confident
			// enough" that must not consume the line — the retry is the fix.
			return abstainReply()
		}
		return pickReply(req)
	}}
	controls := make(chan session.Msg, 8)
	// The operator pauses before the line settles: the box's commit is blocked
	// while paused, so every uncertain reply keeps its escalation budget
	// instead of settling the occurrence — the retry's reachable world.
	controls <- session.ControlMsg{Kind: session.ControlPause}

	r, mem, hub, src := newEscalationHarness(t, eng, dec, controls, nil)

	// Release the run once the budget is spent: the resume is what lets the
	// third attempt's applied decision drive the commit, and a poll on the
	// run's own counter keeps the release inside the run rather than on a
	// wall-clock guess.
	go func() {
		for r.Stats().Rankings < 3 {
			time.Sleep(5 * time.Millisecond)
		}
		controls <- session.ControlMsg{Kind: session.ControlResume}
	}()

	st, evs := runEscalation(t, r, hub, mem, src)

	// The bounded budget: three rankings for the one stable reading — the
	// first plus two retries on the cadence — and not a fourth, though the
	// box stayed READY and stable for the rest of the clip. The matcher does
	// not abstain on this corpus (the tuned gate margin passes): every attempt
	// is the same uncertain MATCH_FUZZY the fuzzy-below-bar clause escalates.
	if st.Rankings != 3 {
		t.Fatalf("rankings = %d, want 3 (the first attempt plus EscalationMaxAttempts-bounded retries; the box was stable and READY for the rest of the clip)", st.Rankings)
	}
	if st.RankingsAbstained != 0 {
		t.Fatalf("rankings_abstained = %d, want 0 (the tuned gate passes this corpus; the uncertainty is the calibrated bar, not the matcher's abstain)", st.RankingsAbstained)
	}
	if st.Decisions != 3 {
		t.Fatalf("decisions = %d, want 3 (every uncertain attempt offers the line to Clef with its read text and shortlist)", st.Decisions)
	}

	commits := committedEsc(t, evs)
	if len(commits) != 1 {
		t.Fatalf("commits = %d (%+v), want exactly one (the third attempt's verdict settles the line on the resume)", len(commits), commits)
	}
	c := commits[0]
	if c.Reason != "CLEF_DISAMBIGUATED" {
		t.Fatalf("commit reason = %q, want CLEF_DISAMBIGUATED (the attempt-three verdict)", c.Reason)
	}
	if c.MessageID == "" || !c.SpeechEligible {
		t.Fatalf("commit = %+v, want a speakable message (the retry exists so the line is finally said, not just finally known)", c)
	}

	seen := dec.snapshot()
	if len(seen) != 3 {
		t.Fatalf("decider asked %d times, want 3 (one per attempt, capped by the budget)", len(seen))
	}
	if got := len(seen[2].Ranking.Candidates); got != 5 {
		t.Fatalf("attempt-three shortlist = %d candidates, want the top-5 cap", got)
	}
	if st.DecisionApplied != 1 || st.DecisionFenced != 0 {
		t.Fatalf("decision applied/fenced = %d/%d, want 1/0 (two abstains apply nothing; the third verdict lands through the fences)", st.DecisionApplied, st.DecisionFenced)
	}
	if st.StaleApplied != 0 {
		t.Fatalf("stale_generation_applied = %d, want 0", st.StaleApplied)
	}
}
