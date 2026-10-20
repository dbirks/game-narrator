package pipeline_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
)

// TestSignItemRegionsCommitTheirClass is the sign/item runtime path
// (docs/spec/04 "Visual profiles", docs/spec/12 release gate "at least 5
// scenarios with actual live model requests including one sign/item"): the
// region's on-screen role is the text-kind hint its reads score with — a sign
// region's read ranks against the corpus's sign messages, an item region's
// against the item messages — and the commit carries the corpus's class for
// the developer UI's display and the session's narrated-class veto.
//
// The frames are the invented 24-frame clip (scripts/fixtures/
// gen-synthetic-replay.sh): page one shows lineA, page two shows lineB. The
// corpus stores page one's line under the class under test (sign, item, or
// the dialogue control) and page two's line as dialogue, so each page commits
// and the first commit's class is the region's. The matcher's own text_kind
// component (the candidates event's per-candidate components) is the
// discriminator the test asserts on directly: it is 1 when the region's hint
// names the corpus's class and 0 when the region is a sign but the corpus
// says dialogue — a sign region must rank its reads as signs without
// rewriting what the corpus says a line is.
func TestSignItemRegionsCommitTheirClass(t *testing.T) {
	const (
		msgA = "synthmsg-class-a" // the clip's page one (lineA), under the class under test
		msgB = "synthmsg-class-b" // the clip's page two (lineB), always dialogue
	)
	type run struct {
		region       games.RegionType
		aKind        games.TextKind // the corpus's class for page one's line
		wantKindComp float64        // the text_kind component the page-one ranking must carry
	}
	cases := []run{
		{games.RegionSign, games.TextSign, 1.0},
		{games.RegionItem, games.TextItem, 1.0},
		{games.RegionTextbox, games.TextDialogue, 1.0},
		// The negative control: a sign region whose corpus holds the line as
		// dialogue. The region's hint must not rewrite the corpus's class —
		// the commit stays dialogue, and the kind component reads 0
		// (a mismatch), not 0.5 (no hint at all): the sign hint was applied,
		// and it scored the corpus's class against it.
		{games.RegionSign, games.TextDialogue, 0.0},
	}
	rr := regionRect(t)
	region := games.Region{ID: "bottom", RectX: rr.X, RectY: rr.Y, RectW: rr.W, RectH: rr.H}

	for _, c := range cases {
		t.Run(string(c.region)+"-"+string(c.aKind), func(t *testing.T) {
			region.RegionType = c.region
			msgs := []games.Message{
				{ID: msgA, ProfileID: "synth-profile", OriginalText: lineA, SpeechText: lineA, TextKind: c.aKind},
				{ID: msgB, ProfileID: "synth-profile", OriginalText: lineB, SpeechText: lineB, TextKind: games.TextDialogue},
				// Invented distractors, dialogue: a half-revealed prefix must be a
				// real decision rather than a lookup (docs/spec/05).
				{ID: "synthmsg-x1", ProfileID: "synth-profile", OriginalText: "The keeper left the lamp burning all night.", SpeechText: "The keeper left the lamp burning all night.", TextKind: games.TextDialogue},
				{ID: "synthmsg-x2", ProfileID: "synth-profile", OriginalText: "Someone had moved the crates before tide turn.", SpeechText: "Someone had moved the crates before tide turn.", TextKind: games.TextDialogue},
			}
			idx, err := matcher.Build(msgs, nil, matcher.IndexOptions{})
			if err != nil {
				t.Fatalf("matcher.Build: %v", err)
			}
			eng := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
			h := newHarness(t, eng, func(cfg *pipeline.Config) {
				cfg.Mode = pipeline.ModeReplay
				cfg.Index = idx
				cfg.Regions = []games.Region{region}
			})
			st, evs, err := h.run(t, h.source(0, false))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if eng.misses.Load() != 0 {
				t.Errorf("fake OCR was handed %d crops that are not in the script: the ROI recipe and the harness recipe disagree", eng.misses.Load())
			}
			// Both pages commit, in page order: the region's class changes what
			// page one commits as, not whether it commits.
			if ids := committed(t, evs); len(ids) != 2 || ids[0] != msgA || ids[1] != msgB {
				t.Fatalf("committed = %v, want [%s %s]", ids, msgA, msgB)
			}
			if st.Commits != 2 {
				t.Errorf("Stats.Commits = %d, want 2", st.Commits)
			}
			// The commit carries the corpus's class (the session's commitKind,
			// read off the candidate index): page one under the class under
			// test, page two as dialogue.
			if got, ok := commitKinds(t, evs); !ok {
				t.Fatalf("no message.committed payloads in the run")
			} else if got[0] != string(c.aKind) || got[1] != string(games.TextDialogue) {
				t.Errorf("commit text_kind = %v, want [%s %s]: the corpus's class is what the UI displays and the narrated-class veto reads", got, c.aKind, games.TextDialogue)
			}
			// The matcher's own text_kind component: the region's hint scored
			// against the corpus's stored class (1 = the hint named it, 0 = it
			// did not). A pipeline that still ranked every region with the
			// dialogue fallback would read 0 here on the sign and item runs.
			if comp, ok := firstTopKindComponent(evs); !ok {
				t.Fatal("no message.candidates event in the run")
			} else if math.Abs(comp-c.wantKindComp) > 1e-9 {
				t.Errorf("page-one top candidate text_kind component = %v, want %v: the %s region's reads must score with the %s hint", comp, c.wantKindComp, c.region, hintLabel(c.region))
			}
			timeline(t, h.runner)
		})
	}
}

// commitKinds reads the text_kind field off every message.committed payload,
// oldest first ("" when the corpus could not name the line).
func commitKinds(t *testing.T, evs []events.Envelope) ([]string, bool) {
	t.Helper()
	var out []string
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		var p struct {
			MessageID string `json:"message_id"`
			TextKind  string `json:"text_kind"`
		}
		if err := json.Unmarshal(e.Data, &p); err != nil {
			t.Fatalf("message.committed payload: %v", err)
		}
		out = append(out, p.TextKind)
	}
	return out, len(out) > 0
}

// firstTopKindComponent reads the text_kind component off the top candidate of
// the run's first message.candidates event (the page-one ranking).
func firstTopKindComponent(evs []events.Envelope) (float64, bool) {
	for _, e := range evs {
		if e.Name != events.MessageCandidates {
			continue
		}
		var p struct {
			Candidates []struct {
				MessageID  string                 `json:"message_id"`
				Components map[string]interface{} `json:"components"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(e.Data, &p); err != nil || len(p.Candidates) == 0 {
			return 0, false
		}
		v, ok := p.Candidates[0].Components["text_kind"]
		f, isNum := v.(float64)
		return f, ok && isNum
	}
	return 0, false
}

// hintLabel names the region's expected hint for an error message.
func hintLabel(rt games.RegionType) string {
	if k, ok := rt.TextKindHint(); ok {
		return string(k)
	}
	return "fallback"
}
