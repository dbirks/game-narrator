package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// harness is the scripted timeline the tests drive: a capture worker feeding
// frames, a ranking worker answering, a speech worker reporting back, and a
// publisher that behaves like events.Hub (numbers, validates, remembers).
//
// Nothing here sleeps. Every timestamp the reducer's logic could consult arrives
// in an Observation, so a test of a typewriter reveal, a 20-page flip or a reply
// that landed three frames late is the same code path in CI as at 3 a.m. on a
// UNO Q — which is the property docs/spec/12's "Unit … state reducer … fakes" row
// exists to protect, and the reason AGENTS.md forbids calling a mocked workflow
// end-to-end: this is the reducer's real logic on fake *input*, which is what a
// unit test is allowed to be.
type harness struct {
	t      testing.TB
	r      *Reducer
	pub    *fakePublisher
	clock  *fakeClock
	ids    *idGen
	region string
	// seq is the harness's own message sequence, assigned the way a replay tool
	// assigns it, so ObservationMsg.Seq can be named by a later RankingMsg.
	seq int64
	ts  int64
	// gen is the generation the workers last saw, which is what a real worker
	// stamps. Stale-generation tests set h.gen deliberately.
	gen int64
	// lastObs is the seq of the most recent observation, for ranking replies.
	lastObs int64
	profile string
	ctx     context.Context
}

func newHarness(t testing.TB, cfg Config, opts ...Option) *harness {
	t.Helper()
	pub := newFakePublisher()
	clock := &fakeClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	ids := &idGen{}
	opts = append([]Option{WithConfig(cfg), WithClock(clock), WithIDs(ids.Next)}, opts...)
	profile := "profile-rev-1"
	r, err := New("session-harness", profile, pub, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := &harness{
		t: t, r: r, pub: pub, clock: clock, ids: ids,
		region: "bottom", ts: time.Hour.Nanoseconds(), profile: profile,
		ctx: context.Background(),
	}
	return h
}

// nextSeq assigns a message sequence. Real live workers leave Seq 0 and let Send
// stamp it; the tests stamp their own because a ranking reply has to name the
// observation it answers, and a replay tool feeding a durable log must keep the
// sequences it recorded (docs/spec/17's dedupe rule is only meaningful for a
// sequence the producer owns).
func (h *harness) nextSeq() int64 { h.seq++; return h.seq }

// frame feeds one observation of text at conf.
func (h *harness) frame(text string, conf float64) Result {
	return h.frameRegion(h.region, text, conf)
}

func (h *harness) frameRegion(region, text string, conf float64) Result {
	h.ts += int64(200 * time.Millisecond)
	h.gen = h.r.Generation()
	h.seq++
	h.lastObs = h.seq
	res, err := h.r.Apply(h.ctx, ObservationMsg{
		Header:   h.header(),
		RegionID: region,
		Observation: recognition.Observation{
			FrameSeq:           h.seq,
			CaptureTimestampNS: h.ts,
			SourceClock:        "monotonic",
			RegionID:           region,
			RegionPresent:      true,
			RegionHash:         fmt.Sprintf("hash-%d-%s", h.seq, region),
			OCR:                ocrResult(text, conf),
		},
	})
	if err != nil {
		h.t.Fatalf("Apply(observation): %v", err)
	}
	return res
}

// hold feeds the same text n more times, i.e. what the box looks like while it is
// stable on screen.
func (h *harness) hold(text string, conf float64, n int) []Result {
	var out []Result
	for i := 0; i < n; i++ {
		out = append(out, h.frame(text, conf))
	}
	return out
}

// settle feeds frames of one text until the region is READY *and holds that text*.
// The second condition is what makes it usable for a page advance: a box that is
// COMMITTED on the previous page is in a perfectly good state, and returning on
// state alone would report the old page as settled.
func (h *harness) settle(text string, conf float64) (int, recognition.State) {
	want := normText(text)
	for i := 1; i <= 16; i++ {
		h.frame(text, conf)
		st := h.regionState(h.region)
		if st == recognition.StateReady && h.text(h.region) == want {
			return i, st
		}
		if st == recognition.StateCommitted && h.text(h.region) == want {
			return i, st
		}
	}
	h.t.Fatalf("region never settled on %q (last state %s, text %q)", want, h.regionState(h.region), h.text(h.region))
	return 0, 0
}

// normText is the output normalization the tracker reports for the strings the
// tests use: no punctuation, single spaces, lower case. Keeping it here rather
// than importing textnorm again means the harness states its expectations in the
// same form the reducer reports.
func normText(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// blank feeds a frame where the detector no longer sees a box.
func (h *harness) dismiss() Result {
	h.ts += int64(200 * time.Millisecond)
	h.gen = h.r.Generation()
	h.seq++
	res, err := h.r.Apply(h.ctx, ObservationMsg{
		Header:   h.header(),
		RegionID: h.region,
		Observation: recognition.Observation{
			FrameSeq:           h.seq,
			CaptureTimestampNS: h.ts,
			SourceClock:        "monotonic",
			RegionID:           h.region,
			RegionPresent:      false,
		},
	})
	if err != nil {
		h.t.Fatalf("Apply(dismiss): %v", err)
	}
	return res
}

// reuse feeds a frame whose region hash is unchanged, so the caller ran no OCR for
// it — the input the tracker is required to answer by reusing the previous read.
func (h *harness) reuse(hash string) Result {
	h.ts += int64(200 * time.Millisecond)
	h.gen = h.r.Generation()
	h.seq++
	h.lastObs = h.seq
	res, err := h.r.Apply(h.ctx, ObservationMsg{
		Header:   h.header(),
		RegionID: h.region,
		Observation: recognition.Observation{
			FrameSeq:           h.seq,
			CaptureTimestampNS: h.ts,
			SourceClock:        "monotonic",
			RegionID:           h.region,
			RegionPresent:      true,
			RegionHash:         hash,
		},
	})
	if err != nil {
		h.t.Fatalf("Apply(reuse): %v", err)
	}
	return res
}

// decide delivers an advisory decision from the decision layer. gen is explicit
// because the late-answer tests must be able to name a generation the session has
// already left behind.
func (h *harness) decide(obsSeq int64, gen int64, messageID, speaker string, eligible, accepted bool) Result {
	return h.decideKind(obsSeq, gen, messageID, speaker, "", 0, eligible, accepted)
}

// decideKind delivers an advisory decision that also names the speaker's
// apparent kind (ADR-0158): the kind the frame established for the speaker,
// with its own confidence. The kind is optional — an empty kind and a zero
// confidence read as no kind, which the speech side's kind gate treats as
// unnamed.
func (h *harness) decideKind(obsSeq int64, gen int64, messageID, speaker string, kind string, kindConfidence float64, eligible, accepted bool) Result {
	h.seq++
	res, err := h.r.Apply(h.ctx, DecisionMsg{
		Header:                Header{SessionID: h.r.st.SessionID, Seq: h.seq, Generation: gen, ProfileRevision: h.profile},
		RegionID:              h.region,
		ObservationSeq:        obsSeq,
		MessageID:             messageID,
		SpeakerID:             speaker,
		SpeakerKnown:          speaker != "",
		SpeakerKind:           kind,
		SpeakerKindConfidence: kindConfidence,
		SpeechEligible:        eligible,
		Accepted:              accepted,
		Provider:              "systemone",
		Model:                 "clef-omni",
	})
	if err != nil {
		h.t.Fatalf("Apply(decision): %v", err)
	}
	return res
}

// speechAt delivers a speech stage stamped with an explicit generation, for the
// "clip from an older generation arrives late" tests.
func (h *harness) speechAt(gen int64, occurrence, region string, kind SpeechKind) Result {
	h.seq++
	res, err := h.r.Apply(h.ctx, SpeechMsg{
		Header:       Header{SessionID: h.r.st.SessionID, Seq: h.seq, Generation: gen, ProfileRevision: h.profile},
		RegionID:     region,
		OccurrenceID: occurrence,
		Kind:         kind,
	})
	if err != nil {
		h.t.Fatalf("Apply(speech): %v", err)
	}
	return res
}

func (h *harness) header() Header {
	return Header{SessionID: h.r.st.SessionID, Seq: h.seq, Generation: h.gen, ProfileRevision: h.profile}
}

// rank answers the most recent (or a named) observation.
func (h *harness) rank(messageID string, margin float64, abstain bool) Result {
	return h.rankAt(h.lastObs, h.region, "", messageID, margin, abstain, "")
}

func (h *harness) rankAt(obsSeq int64, region, text, messageID string, margin float64, abstain bool, scene string) Result {
	h.gen = h.r.Generation()
	h.seq++
	if text == "" {
		text = h.text(region)
	} else {
		text = normText(text)
	}
	rk := matcher.Ranking{Margin: margin, Reason: matcher.ReasonExact, Abstain: abstain}
	if !abstain && messageID != "" {
		rk.Candidates = []matcher.Candidate{{MessageID: messageID, Score: 1.0, Probability: 0.9 - margin/2}}
		rk.Candidates = append(rk.Candidates, matcher.Candidate{MessageID: messageID + "-runner", Probability: 0.9 - margin/2 - margin})
	}
	if abstain {
		rk.Reason = matcher.ReasonAbstained
	}
	res, err := h.r.Apply(h.ctx, RankingMsg{
		Header:         h.header(),
		RegionID:       region,
		ObservationSeq: obsSeq,
		Text:           text,
		SceneID:        scene,
		Ranking:        rk,
	})
	if err != nil {
		h.t.Fatalf("Apply(ranking): %v", err)
	}
	return res
}

// predictiveRank answers with the matcher's prefix reason, which is the only
// reason that can make a commit out of a still-revealing box.
func (h *harness) predictiveRank(obsSeq int64, region, text, messageID string, margin float64) Result {
	h.gen = h.r.Generation()
	h.seq++
	res, err := h.r.Apply(h.ctx, RankingMsg{
		Header:         h.header(),
		RegionID:       region,
		ObservationSeq: obsSeq,
		Text:           text,
		Ranking: matcher.Ranking{
			Candidates: []matcher.Candidate{{MessageID: messageID, Probability: 0.8}, {MessageID: "other", Probability: 0.8 - margin}},
			Margin:     margin,
			Reason:     matcher.ReasonPredictivePrefix,
		},
	})
	if err != nil {
		h.t.Fatalf("Apply(predictive ranking): %v", err)
	}
	return res
}

func (h *harness) speech(obsID string, kind SpeechKind, region string) Result {
	if obsID == "" {
		obsID = h.occurrence(region)
	}
	h.seq++
	res, err := h.r.Apply(h.ctx, SpeechMsg{
		Header:        Header{SessionID: h.r.st.SessionID, Seq: h.seq, Generation: h.gen, ProfileRevision: h.profile},
		RegionID:      region,
		OccurrenceID:  obsID,
		Kind:          kind,
		CacheKey:      "cache-" + obsID,
		VoiceID:       "narrator-v1",
		VoiceRevision: 3,
	})
	if err != nil {
		h.t.Fatalf("Apply(speech): %v", err)
	}
	return res
}

func (h *harness) control(kind ControlKind, newRev string) Result {
	h.seq++
	res, err := h.r.Apply(h.ctx, ControlMsg{
		Header:             Header{SessionID: h.r.st.SessionID, Seq: h.seq, Generation: h.r.Generation(), ProfileRevision: h.profile},
		Kind:               kind,
		NewProfileRevision: newRev,
	})
	if err != nil {
		h.t.Fatalf("Apply(control %s): %v", kind, err)
	}
	if kind == ControlProfileChanged && newRev != "" {
		h.profile = newRev
	}
	return res
}

// controlSettings sends the mid-session operator policy (docs/spec/09
// "/play"): the complete target state, as the runtime merges it — the
// harness plays the runtime's half here.
func (h *harness) controlSettings(predictive bool, kinds ...games.TextKind) Result {
	h.seq++
	res, err := h.r.Apply(h.ctx, ControlMsg{
		Header: Header{SessionID: h.r.st.SessionID, Seq: h.seq, Generation: h.r.Generation(), ProfileRevision: h.profile},
		Kind:   ControlSettings,
		Settings: Settings{
			Predictive:    predictive,
			NarratedKinds: kinds,
		},
	})
	if err != nil {
		h.t.Fatalf("Apply(control settings): %v", err)
	}
	return res
}

// --- reads ---------------------------------------------------------------

func (h *harness) regionState(id string) recognition.State {
	for _, reg := range h.r.State().Regions {
		if reg.RegionID == id {
			return reg.TextState
		}
	}
	return recognition.StateAbsent
}

func (h *harness) text(id string) string {
	for _, reg := range h.r.State().Regions {
		if reg.RegionID == id {
			return reg.Text
		}
	}
	return ""
}

func (h *harness) occurrence(id string) string {
	for _, reg := range h.r.State().Regions {
		if reg.RegionID == id {
			return reg.DialogueOccurrenceID
		}
	}
	return ""
}

func (h *harness) trace(id string) string {
	for _, reg := range h.r.State().Regions {
		if reg.RegionID == id {
			return reg.TraceID
		}
	}
	return ""
}

func (h *harness) regionSpeechPhase(id string) SpeechPhase {
	for _, reg := range h.r.State().Regions {
		if reg.RegionID == id {
			return reg.Speech
		}
	}
	return PhaseNone
}

// matcherRank builds the ranking a retrieval worker returns for a confident single
// answer, so the tests do not spell out candidate internals they do not assert on.
func matcherRank(messageID string) matcher.Ranking {
	return matcher.Ranking{
		Candidates: []matcher.Candidate{
			{MessageID: messageID, Score: 2.5, Probability: 0.8},
			{MessageID: messageID + "-runner", Probability: 0.3},
		},
		Margin: 0.5,
		Reason: matcher.ReasonExact,
	}
}

// timeoutAfter bounds the tests that wait on a goroutine. It is generous (seconds)
// because they assert that something *does* happen, and a CI box under -race is
// slow; the property they protect is that the reducer's loop is not wedged.
func timeoutAfter() <-chan time.Time { return time.After(5 * time.Second) }

// --- fakes ---------------------------------------------------------------

// fakePublisher stands in for *events.Hub: it assigns a ULID and a seq, and — the
// part that matters — it *validates*, so an event this package would be proud of
// but a browser could not decode fails the test that emitted it.
type fakePublisher struct {
	published []events.Envelope
	seq       int64
	failFor   map[string]int // name → remaining failures injected
	err       error
}

func newFakePublisher() *fakePublisher {
	return &fakePublisher{failFor: map[string]int{}}
}

func (p *fakePublisher) Publish(_ context.Context, e events.Envelope) (events.Envelope, error) {
	if p.err != nil {
		return events.Envelope{}, p.err
	}
	if n := p.failFor[e.Name]; n > 0 {
		p.failFor[e.Name] = n - 1
		return events.Envelope{}, fmt.Errorf("injected publish failure for %s", e.Name)
	}
	e.SchemaVersion = events.SchemaVersion
	if e.EventID == "" {
		e.EventID = p.idFor(p.seq + 1)
	}
	if e.OccurredAt.IsZero() {
		return events.Envelope{}, fmt.Errorf("occurred_at must be set")
	}
	if len(e.Data) == 0 {
		e.Data = json.RawMessage("{}")
	}
	p.seq++
	e.Seq = p.seq
	if err := e.Validate(); err != nil {
		return events.Envelope{}, err
	}
	p.published = append(p.published, e)
	return e, nil
}

// idFor mints a valid, deterministic event id. The envelope contract requires a
// 26-character Crockford base32 ULID, and games.NewID would satisfy it with
// entropy from the system RNG; a counter satisfies it just as well and is what
// makes an event stream byte-comparable across two runs of the same message list,
// which is the determinism property docs/spec/12 asks for.
func (p *fakePublisher) idFor(n int64) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	out := make([]byte, games.IDLength)
	out[0] = '0' // the timestamp field's top bits, kept inside what ValidID accepts
	v := uint64(n) + 1
	for i := len(out) - 1; i >= 1; i-- {
		out[i] = alphabet[v&31]
		v >>= 5
	}
	return string(out)
}

func (p *fakePublisher) names() []string {
	out := make([]string, 0, len(p.published))
	for _, e := range p.published {
		out = append(out, e.Name)
	}
	return out
}

func (p *fakePublisher) byName(name string) []events.Envelope {
	var out []events.Envelope
	for _, e := range p.published {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

func (p *fakePublisher) reset() { p.published = nil }

// fakeClock advances a millisecond per call, so event timestamps are deterministic
// and two events from one message are still strictly ordered (docs/spec/17's
// envelope carries occurred_at, and a log where two records share a timestamp
// cannot be replayed by eye).
type fakeClock struct{ at time.Time }

func (c *fakeClock) Now() time.Time {
	out := c.at
	c.at = c.at.Add(time.Millisecond)
	return out
}

// idGen hands out sortable, obviously-distinct ids. The reducer only requires that
// its ids are unique and, for events, ULID-shaped — the envelope validator checks
// event ids, and occurrence ids are opaque to every consumer that stores them.
type idGen struct{ n int }

func (g *idGen) Next() string {
	g.n++
	return fmt.Sprintf("occ-%06d", g.n)
}

// --- helpers -------------------------------------------------------------

// ocrResult builds an OCR result the way the engine reports one: words with
// per-word confidence, plus the concatenated text and mean for engines that do
// not return boxes.
func ocrResult(text string, conf float64) *ocr.Result {
	res := &ocr.Result{Text: text, MeanConf: conf, Engine: "tesseract"}
	if text == "" {
		return res
	}
	for _, w := range strings.Fields(text) {
		res.Words = append(res.Words, ocr.Word{Text: w, Conf: conf})
	}
	return res
}

func dataOf(t *testing.T, e events.Envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatalf("data of %s is not an object: %v", e.Name, err)
	}
	return m
}

func fieldStr(t *testing.T, e events.Envelope, key string) string {
	t.Helper()
	v, ok := dataOf(t, e)[key]
	if !ok {
		t.Fatalf("%s has no %q field: %s", e.Name, key, string(e.Data))
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s.%s = %T, want string", e.Name, key, v)
	}
	return s
}

func fieldFloat(t *testing.T, e events.Envelope, key string) float64 {
	t.Helper()
	v, ok := dataOf(t, e)[key]
	if !ok {
		t.Fatalf("%s has no %q field: %s", e.Name, key, string(e.Data))
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s.%s = %T, want number", e.Name, key, v)
	}
	return f
}

func fieldBool(t *testing.T, e events.Envelope, key string) bool {
	t.Helper()
	v, ok := dataOf(t, e)[key]
	if !ok {
		t.Fatalf("%s has no %q field: %s", e.Name, key, string(e.Data))
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("%s.%s = %T, want bool", e.Name, key, v)
	}
	return b
}

func committedReasons(t *testing.T, evs []events.Envelope) []string {
	t.Helper()
	var out []string
	for _, e := range evs {
		if e.Name == events.MessageCommitted {
			out = append(out, fmt.Sprintf("%s/%s", fieldStr(t, e, "message_id"), fieldStr(t, e, "reason")))
		}
	}
	return out
}

func hasName(evs []events.Envelope, name string) bool {
	for _, e := range evs {
		if e.Name == name {
			return true
		}
	}
	return false
}

func countName(evs []events.Envelope, name string) int {
	n := 0
	for _, e := range evs {
		if e.Name == name {
			n++
		}
	}
	return n
}

func joinNames(evs []events.Envelope) string { return strings.Join(namesOf(evs), " ") }

func namesOf(evs []events.Envelope) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Name)
	}
	return out
}
