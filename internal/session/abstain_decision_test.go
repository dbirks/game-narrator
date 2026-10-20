package session

// The local-abstain + adjudication contract (docs/spec/05 "Clef adjudication
// and commit policy", ADR-0055's production margin zero): NeedsAdjudication
// sends exactly the rankings the local scoring abstained on to the decision
// role, so the commitment policy must be able to stand on that answer. An
// abstain with a shortlist is an identity question the model is asked to
// settle; an answer that narrows to a retrieved candidate commits, with the
// model's safe-to-speak as the eligibility evidence. Nothing here may invent
// a line retrieval never offered.

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/matcher"
)

// abstainRank applies a ranking that abstained while still carrying the
// shortlist the matcher documents on abstain (the decision layer's input).
// The harness's own rank helper models an abstain with no candidates, so the
// message goes through the reducer directly, the way the "narrowing" test does.
func (h *harness) abstainRankWithCandidates(obsSeq int64, candidates []matcher.Candidate) Result {
	h.gen = h.r.Generation()
	h.seq++
	res, err := h.r.Apply(h.ctx, RankingMsg{
		Header:         h.header(),
		RegionID:       h.region,
		ObservationSeq: obsSeq,
		Text:           h.text(h.region),
		Ranking: matcher.Ranking{
			Candidates: candidates,
			Margin:     0.05,
			Reason:     matcher.ReasonAbstained,
			Abstain:    true,
		},
	})
	if err != nil {
		h.t.Fatalf("Apply(abstain ranking): %v", err)
	}
	return res
}

func TestDecisionOnLocalAbstain(t *testing.T) {
	t.Run("narrowing makes the abstained line commit and speak", func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		h.settle("TWO ADJACENT GRAPH CANDIDATES", 0.9)
		obs := h.lastObs
		h.gen = 0
		h.seq++
		if res, err := h.r.Apply(h.ctx, DecisionMsg{
			Header:   Header{SessionID: h.r.st.SessionID, Seq: h.seq, Generation: 0, ProfileRevision: h.profile},
			RegionID: "bottom", ObservationSeq: obs, MessageID: "demo-second", SpeakerID: "narrator",
			SpeakerKnown: true, SpeechEligible: true, Accepted: true, Provider: "systemone",
		}); err != nil || res.Reason != "applied" {
			t.Fatalf("decision delivery: %+v %v", res, err)
		}
		res := h.abstainRankWithCandidates(obs, []matcher.Candidate{
			{MessageID: "demo-first", Probability: 0.55},
			{MessageID: "demo-second", Probability: 0.45},
		})
		ev := res.Events[len(res.Events)-1]
		if got := fieldStr(t, ev, "message_id"); got != "demo-second" {
			t.Fatalf("message_id = %q, want the candidate the model chose", got)
		}
		if !fieldBool(t, ev, "speech_eligible") {
			t.Fatal("the model said safe_to_speak and the local abstain was the identity question: the line must speak")
		}
		if got := fieldStr(t, ev, "reason"); got != ReasonClefDisambiguated {
			t.Fatalf("reason = %q, want %q", got, ReasonClefDisambiguated)
		}
	})

	t.Run("a vetoed answer commits the line silent", func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		h.settle("TWO ADJACENT GRAPH CANDIDATES", 0.9)
		obs := h.lastObs
		h.decide(obs, 0, "demo-second", "narrator", false, true)
		res := h.abstainRankWithCandidates(obs, []matcher.Candidate{
			{MessageID: "demo-first", Probability: 0.55},
			{MessageID: "demo-second", Probability: 0.45},
		})
		ev := res.Events[len(res.Events)-1]
		if got := fieldStr(t, ev, "message_id"); got != "demo-second" {
			t.Fatalf("message_id = %q: a veto silences the utterance, it does not erase the match", got)
		}
		if fieldBool(t, ev, "speech_eligible") {
			t.Fatal("safe_to_speak = no must keep the line silent")
		}
	})

	t.Run("a rejected answer leaves the abstain as silence", func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		h.settle("TWO ADJACENT GRAPH CANDIDATES", 0.9)
		obs := h.lastObs
		h.decide(obs, 0, "demo-second", "narrator", true, false)
		res := h.abstainRankWithCandidates(obs, []matcher.Candidate{
			{MessageID: "demo-first", Probability: 0.55},
			{MessageID: "demo-second", Probability: 0.45},
		})
		ev := res.Events[len(res.Events)-1]
		if got := fieldStr(t, ev, "message_id"); got != UnknownMessage {
			t.Fatalf("message_id = %q, want %q: a rejected answer is not an answer", got, UnknownMessage)
		}
		if fieldBool(t, ev, "speech_eligible") {
			t.Fatal("no accepted answer, no audio")
		}
	})

	t.Run("an invention is refused even on an abstain", func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		h.settle("TWO ADJACENT GRAPH CANDIDATES", 0.9)
		obs := h.lastObs
		h.decide(obs, 0, "msg-hallucinated", "narrator", true, true)
		res := h.abstainRankWithCandidates(obs, []matcher.Candidate{
			{MessageID: "demo-first", Probability: 0.55},
			{MessageID: "demo-second", Probability: 0.45},
		})
		ev := res.Events[len(res.Events)-1]
		if got := fieldStr(t, ev, "message_id"); got != UnknownMessage {
			t.Fatalf("message_id = %q, want %q: the model's choice was not a candidate", got, UnknownMessage)
		}
	})

	t.Run("an abstain with no shortlist and no answer is silence", func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		h.settle("TWO ADJACENT GRAPH CANDIDATES", 0.9)
		res := h.rank("", 0.0, true) // the harness abstain carries no candidates
		ev := res.Events[len(res.Events)-1]
		if got := fieldStr(t, ev, "message_id"); got != UnknownMessage {
			t.Fatalf("message_id = %q, want %q", got, UnknownMessage)
		}
		if fieldBool(t, ev, "speech_eligible") {
			t.Fatal("nothing retrieved, nothing to speak")
		}
	})
}
