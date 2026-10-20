package zzpwprobe

import (
	"fmt"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
)

func msgs() []games.Message {
	texts := []string{
		"The harbour bell rang twice before anyone looked up.",
		"Nobody in the village had seen that boat before.",
		"The harbour bell rang twice before anyone looked up again.",
		"The harbour bell was missing from the wreck.",
		"Nobody in the village had seen that boat before long.",
		"The keeper left the lamp burning all night.",
		"Someone had moved the crates before tide turn.",
		"The waterline was higher than the last chart said.",
		"A rowboat sat untied beneath the broken jetty.",
		"Nobody mentioned the light that went out at dusk.",
		"The harbourmaster kept his ledger in the upper drawer.",
		"Something had been dragged across the slipway.",
		"The tide would not turn for another hour.",
		"Every window on the quay faced the same direction.",
	}
	out := make([]games.Message, len(texts))
	for i, t := range texts {
		id := fmt.Sprintf("demo-%03d", i)
		out[i] = games.Message{
			ID: id, OriginalText: t, NormalizedText: t, SpeechText: t,
			Language: "en", TextKind: games.TextDialogue,
		}
	}
	return out
}

func TestZZProbe(t *testing.T) {
	idx, err := matcher.Build(msgs(), []games.MessageEdge{
		{FromID: "demo-000", ToID: "demo-001", EdgeType: games.EdgeNext, Confidence: 0.9},
	}, matcher.IndexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = idx
	texts := []string{
		"The harbour bell rang twice before anyone looked up.",
		"the harbour bell rang twlce before anyone looked up",
		"Nobody in the village had seen that boat before.",
		"nobody ln the vlllage had seen that boat before",
	}
	for _, text := range texts {
		rk := idx.Rank(matcher.Observation{Text: text}, matcher.DefaultRankOptions())
		fmt.Printf("text=%q\n  reason=%s abstain=%v margin=%.4f n=%d\n", text, rk.Reason, rk.Abstain, rk.Margin, len(rk.Candidates))
		for i, c := range rk.Candidates {
			if i >= 4 {
				break
			}
			fmt.Printf("  [%d] %s prob=%.4f text=%.3f\n", i, c.MessageID, c.Probability, c.Components[matcher.ComponentText])
		}
	}
}
