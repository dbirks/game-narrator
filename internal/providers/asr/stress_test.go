package asr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
)

// LIVE WER-stress harness (FR-10 acceptance gap 7, ADR 0061 amendment): the
// evidence that the QA bound is set from measurement, not assumption.
//
// It designs five voices (varied age/pitch/accent) with the real
// qwen3-tts-voicedesign checkpoint, registers each under a gn_test_<random8>
// name on the real Base service (removed in cleanup), synthesizes 64 invented
// lines with qwen3-tts-base, transcribes every clip with qwen3-asr-1-7b, and
// scores BOTH sides with the product's own normalizer (speech.Evaluate):
//
//   - the GENUINE set: each transcript against its own line;
//   - the WRONG-TEXT control: each transcript against the PARTNER line of the
//     same pair (audio of line A scored against text B) — no extra ASR spend,
//     and the pairs are written topic-adjacent so the control is the hardest
//     realistic case (a line that shares scene words with the spoken one).
//
// The distributions decide the bound: every genuine WER must sit at or under
// the bound and every control WER strictly above it. p50/p95/p99/max of the
// genuine set and min/p1 of the control set are recorded with the evidence
// JSON under artifacts/asr-stress/ (gitignored).
//
// Runs only under the test-models gate with real endpoints; SKIPs "NOT RUN"
// otherwise, so `go test ./...` stays green offline. Configuration (never
// printed, never logged — AGENTS.md rule 5):
//
//	GAME_NARRATOR_GATEWAY_URL           shared gateway (TTS + ASR)
//	GAME_NARRATOR_GATEWAY_TOKEN_ENV     names the variable holding the token
//	GAME_NARRATOR_TTS_BASE_DIRECT_URL   direct Base service (voice registry)
//
// Log lines carry model ids, metrics and timings — never a URL or a token.

const stressEvidenceDir = "artifacts/asr-stress"

// stressRoot resolves the repository root from the test's working directory
// (go test runs in the package dir), so the evidence always lands under the
// gitignored artifacts/asr-stress/ at the root, not in a package-local tree.
func stressRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err == nil && !fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate the repository root (AGENTS.md) above %s", dir)
		}
		dir = parent
	}
}

var stressDesignLine = "The lantern swung slowly as the tide came in over the old stone quay."

type stressVoiceDef struct {
	tag  string // short label for logs/evidence
	desc string // the voice-design instructions
}

// stressVoices is the design set: five voices with deliberately varied age,
// pitch and accent descriptions (the task requires >= 4).
var stressVoices = []stressVoiceDef{
	{tag: "low-male-brit", desc: "A calm 34-year-old British man with a low, steady baritone, a measured pace, and a faint West Country accent."},
	{tag: "warm-female-us", desc: "A warm 62-year-old American woman with a rich alto voice, a slow unhurried delivery, and a light Mid-Atlantic accent."},
	{tag: "bright-female-brit", desc: "A bright 21-year-old British woman with a high, light voice, quick energetic delivery, and a northern English accent."},
	{tag: "gravel-male-us", desc: "A deep 48-year-old American man with a gravelly bass voice, a slow weighty pace, and a southern accent."},
	{tag: "soft-elder-brit", desc: "A soft 75-year-old British man with a thin wheezy tenor, a very slow pace with long pauses, and a gentle Scottish accent."},
}

// stressPairs is the invented line set: 32 pairs, 64 lines. The pairs cover
// the shapes the bound must survive — short/long, numbers (words, digits,
// thousands), invented names, punctuation-heavy lines, questions and
// exclamations, contractions, and British/American spelling variants — and
// each pair shares scene vocabulary without repeating sentences, so the
// wrong-text control is the hardest realistic mismatch, not a random one.
var stressPairs = [][2]string{
	// Short declaratives.
	{"The gate was open.", "The door stood open."},
	{"A lamp glowed in the hall.", "A light burned in the room."},
	{"She stopped at the bridge.", "He halted at the ford."},
	{"The bell rang once.", "The bell rang twice."},
	{"We are almost there.", "They are already here."},
	{"The path ends at the wall.", "The road runs along the wall."},
	// Long declaratives.
	{"The caravan moved south after midnight, and the leader kept his eyes on the far ridge all the way across the valley floor.",
		"The caravan turned west before dawn, and the guide watched the low hills until they vanished into the pale morning mist."},
	{"Beyond the flooded field stood a row of black pines, and the wind pressed through their branches with a sound like distant water.",
		"Across the dry meadow rose a line of grey beeches, and the rain hammered their leaves with a sound like scattered stone."},
	{"She carried the message in her pocket, folded twice, and read it again by the firelight before she dared to speak a single word of it.",
		"He kept the letter in his coat, folded twice, and read it twice by the candle before he dared to show it to anyone."},
	{"The market square was empty at noon, its stalls shuttered and its stones wet, as if the traders had gone home expecting rain that had already passed.",
		"The market lane was quiet at dusk, its carts gone and its cobbles glistening, as if the traders had left before the first snow began to fall."},
	{"Every window on the upper floor was dark, and the only sound came from the water running down the old stone stair behind the shutters.",
		"Every lamp on the lower floor was out, and the only noise came from the wind circling the tower and tapping the iron shutters."},
	{"They planted the markers at the crossroads, painted them red, and agreed that the first traveler who found them would follow the northern road without turning back.",
		"They drove the stakes into the heath, painted them white, and agreed that the first rider who found them would keep to the southern lane at all costs."},
	// Numbers: spelled-out, digits, thousands.
	{"Three ships waited at the quay, each crew of forty men, and the tide would turn in two hours.",
		"Five barges sat in the harbour, each crew of thirty men, and the tide would turn in one hour."},
	{"The order was for twelve crates of iron nails and a dozen barrels of salted fish, delivered before the ninth day.",
		"The order was for eight crates of copper wire and a dozen barrels of smoked fish, delivered by the seventh day."},
	{"Room 214 was booked for six guests, and the spare key was kept in drawer seven of the front desk.",
		"Room 302 was booked for four guests, and the spare key was kept in drawer five of the back office."},
	{"The recipe called for two cups of flour, a pinch of salt, and the juice of one lemon, stirred for ten minutes.",
		"The recipe called for three cups of rice, a pinch of sugar, and the rind of two lemons, stirred for five minutes."},
	{"Mile twelve was where the road forked, and the sign pointed four kilometres to the old ferry crossing.",
		"Mile eighteen was where the track split, and the post pointed seven miles to the new bridge."},
	{"The survey counted 1240 heads of cattle, 340 sheep, and a herd of 60 horses grazing on the eastern slope.",
		"The census counted 875 heads of cattle, 610 sheep, and a herd of 90 horses grazing on the western slope."},
	// Invented names.
	{"Eleanor whispered to Marcus at the gate, and Marcus answered before the last word had left her lips.",
		"Rowan whispered to Elias at the window, and Elias answered before the last word had left her lips."},
	{"The captain, a woman named Odette, took the helm herself and would not name a time for the return.",
		"The pilot, a man named Caspar, took the wheel himself and would not name a day for the return."},
	{"When the bells of Saint Aldric's tower rang at midnight, the whole street woke and called it an omen.",
		"When the bells of Saint Maren's church rang at midnight, the whole lane woke and called it a warning."},
	{"Professor Hale kept his lectures short: five minutes on the theory, then the work, then the door.",
		"Professor Vance kept his lectures long: an hour on the theory, then the drill, then the exam."},
	// Punctuation-heavy: dashes, semicolons, quoted speech, interjections.
	{"What do you mean — a locked room, and no key, and no window either?!",
		"What is she saying — an empty hall, and no door, and no stair either?!"},
	{"'Stay here,' she said; 'do not follow the lantern, whatever it promises.'",
		"'Wait outside,' he said; 'do not open the chest, whatever it contains.'"},
	{"Watch out! Watch out! The rope is frayed, the plank is loose, and nobody down there is anchoring it.",
		"Be careful! Be careful! The chain is rusted, the beam is cracked, and nobody up there is holding it."},
	{"'The bridge is out, the ford is flooded, and the ferryman will not move,' she said, 'so we wait.'",
		"'The road is blocked, the tunnel is dark, and the guide will not lead,' he said, 'so we rest.'"},
	{"No, no — not the north gate; the east gate, the one with the iron studs, and mind the steps.",
		"No, no — not the south gate; the west gate, the one with the bronze rings, and mind the ladder."},
	{"Did you see it? A second light over the water, then a third! Did you see it, or was it the fog?",
		"Did you hear it? A second bell past the ridge, then a third! Did you hear it, or was it the wind?"},
	// British/American spelling (mapped and unmapped variants) — the two lines
	// are different lines, so the control stays a genuine wrong-line case.
	{"The harbour master, a strict man, kept his grey favourite cat in the old office and never let it out of sight.",
		"The harbor keeper, a lazy man, kept his gray favorite dog in the new shed and rarely walked it at all."},
	{"She recognised the organisation's colours, remembered the centre of the square, and bought a litre of ink for the ledger.",
		"He noticed the company's logo, forgot the center of the plaza, and sold a liter of paint at the counter."},
	// Contractions, questions, exclamations — different content, shared shape.
	{"I don't know what the keeper wants, but if he comes tonight through the low gate, we won't be ready, will we?",
		"I don't know what the warden wants, but if she comes tomorrow across the field, we won't be ready, will we?"},
	{"'The fog is back,' the guide said, and the mules refused the narrow track, because we'd all learned that the fog does not wait.",
		"'The rain is back,' the scout said, and the horses spooked at the open meadow, because we'd all learned that the rain does not wait."},
}

// ── Evidence shapes (written under artifacts/asr-stress/, gitignored) ─────

type stressClip struct {
	Idx          int     `json:"idx"`
	Pair         int     `json:"pair"`
	Voice        string  `json:"voice"`
	Kind         string  `json:"kind"` // "genuine" | "wrong_text"
	Expected     string  `json:"expected"`
	Transcript   string  `json:"transcript,omitempty"`
	WER          float64 `json:"wer"`
	CER          float64 `json:"cer"`
	WithinBound  bool    `json:"within_bound"`
	WAV          string  `json:"wav,omitempty"`
	ASRRequestID string  `json:"asr_request_id,omitempty"`
	ASRLatencyMS int64   `json:"asr_latency_ms,omitempty"`
}

type stressEvidence struct {
	GeneratedAt string        `json:"generated_at"`
	Purpose     string        `json:"purpose"`
	ASRModel    string        `json:"asr_model"`
	TTSModel    string        `json:"tts_model"`
	DesignModel string        `json:"design_model"`
	WERBound    float64       `json:"wer_bound_in_effect"`
	Verdict     string        `json:"verdict"`
	Recommended float64       `json:"recommended_bound,omitempty"`
	Statistics  stressStats   `json:"statistics"`
	Designs     []stressVoice `json:"designs"`
	Clips       []stressClip  `json:"clips"`
}

type stressVoice struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	RegisteredAs string `json:"registered_as"`
	Clips        int    `json:"clips"`
	Errors       int    `json:"errors"`
}

type stressStats struct {
	GenuineN   int     `json:"genuine_n"`
	GenuineP50 float64 `json:"genuine_p50"`
	GenuineP95 float64 `json:"genuine_p95"`
	GenuineP99 float64 `json:"genuine_p99"`
	GenuineMax float64 `json:"genuine_max"`
	WrongN     int     `json:"wrong_n"`
	WrongMin   float64 `json:"wrong_min"`
	WrongP1    float64 `json:"wrong_p1"`
	WrongP50   float64 `json:"wrong_p50"`
	WrongMax   float64 `json:"wrong_max"`
}

// nearestRank is the nearest-rank percentile (p in (0, 100]) of a non-empty
// slice, computed on the values themselves (no sorting side effect).
func nearestRank(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := int((p/100)*float64(len(sorted)) + 0.999999) // ceil
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// retryable is the bounded-retry predicate for the harness: only idempotent
// operations (a TTS synthesis / ASR transcription of fixed bytes) are ever
// retried, and only on the provider-classified retryable kinds (docs/spec/13).
func retryable(err error) bool {
	var pe *providers.Error
	if !errors.As(err, &pe) {
		return false
	}
	return pe.Kind.RetryableByDefault()
}

// TestLiveWERStress is the bound's evidence run. It is deliberately NOT part
// of the unit gate: it only runs under the test-models gate with real
// endpoints and SKIPs "NOT RUN" otherwise (AGENTS.md rule 3: this is a real
// provider measurement, and it must be labelled as one).
func TestLiveWERStress(t *testing.T) {
	if os.Getenv(providers.EnvGate) != "test-models" {
		t.Skipf("live WER stress outside the test-models gate: run via make test-models (it sets %s=test-models)", providers.EnvGate)
	}
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	cfgOf := func(role providers.Role, modelDefault string) providers.RoleConfig {
		t.Helper()
		c := roles[role]
		modelEnv, _, _, _ := providers.RoleEnvVars(role)
		if m := strings.TrimSpace(os.Getenv(modelEnv)); m != "" {
			c.Model = m
		} else {
			c.Model = modelDefault
		}
		c.Timeout = 120 * time.Second
		return c
	}
	asrCfg := cfgOf(providers.RoleSpeechASR, liveASRModel)
	designCfg := cfgOf(providers.RoleVoiceDesign, "qwen3-tts-voicedesign")
	baseCfg := cfgOf(providers.RoleVoiceClone, "qwen3-tts-base")
	direct := strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	for _, c := range []providers.RoleConfig{asrCfg, designCfg, baseCfg} {
		if strings.TrimSpace(c.BaseURL) == "" || strings.TrimSpace(c.Token()) == "" {
			t.Skip("NOT RUN: set GAME_NARRATOR_GATEWAY_URL and the token env named by " +
				providers.EnvGatewayTokenEnv + " (default " + providers.EnvGatewayToken + ") to run the live WER stress")
		}
	}
	if direct == "" {
		t.Skip("NOT RUN: set GAME_NARRATOR_TTS_BASE_DIRECT_URL to the direct Base service to run the live WER stress (the voice registry route is not on the shared gateway)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	root := stressRoot(t)
	bound := speech.WERBound()
	t.Logf("WER stress: asr=%s tts=%s design=%s bound=%.3f voices=%d lines=%d",
		asrCfg.Model, baseCfg.Model, designCfg.Model, bound, len(stressVoices), len(stressPairs)*2)

	designClient, err := tts.NewFromRole(designCfg)
	if err != nil {
		t.Fatalf("tts.NewFromRole(design): %v", err)
	}
	baseClient, err := tts.NewFromRole(baseCfg)
	if err != nil {
		t.Fatalf("tts.NewFromRole(base): %v", err)
	}
	asrClient, err := NewFromRole(asrCfg)
	if err != nil {
		t.Fatalf("asr.NewFromRole: %v", err)
	}
	reg, err := tts.NewRegistry(direct, baseCfg.Token())
	if err != nil {
		t.Fatalf("tts.NewRegistry: %v", err)
	}

	// ── Design and register the five voices (gn_test_ only; cleanup removes) ──
	type designed struct {
		tag  string
		name string
		desc string
	}
	var (
		voices    []designed
		clipCount = map[string]int{}
		errCount  = map[string]int{}
	)
	for _, v := range stressVoices {
		tag, desc := v.tag, v.desc
		da, err := designClient.Design(ctx, tts.DesignRequest{Text: stressDesignLine, Instructions: desc, Language: "English"})
		if err != nil {
			if retryable(err) {
				da, err = designClient.Design(ctx, tts.DesignRequest{Text: stressDesignLine, Instructions: desc, Language: "English"})
			}
		}
		if err != nil {
			t.Fatalf("design %s: %v", tag, err)
		}
		var nb [4]byte
		if _, err := rand.Read(nb[:]); err != nil {
			t.Fatalf("voice name: %v", err)
		}
		name := "gn_test_" + hex.EncodeToString(nb[:])
		voice := tts.Voice{Name: name, RefAudio: da.WAV, RefText: stressDesignLine,
			Consent: "original_synthetic_design (game-narrator automated test)"}
		if _, err := reg.Register(ctx, voice); err != nil {
			t.Fatalf("register %s: %v", tag, err)
		}
		registered := name
		voices = append(voices, designed{tag: tag, name: name, desc: desc})
		clipCount[tag] = 0
		errCount[tag] = 0
		t.Logf("WER stress design: voice=%s registered=%s design_latency=%s", tag, registered, da.Latency.Round(time.Millisecond))
	}
	removed := map[string]bool{}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		for _, v := range voices {
			if removed[v.name] {
				continue
			}
			if err := reg.Remove(cctx, v.name); err != nil {
				t.Logf("cleanup Remove(%s): %v", v.name, err)
			}
		}
	})

	// ── Synthesize + transcribe all 64 lines ─────────────────────────────────
	type lineResult struct {
		line       string
		wav        []byte
		wavRel     string
		transcript string
		reqID      string
		reqSrc     string
		latencyMS  int64
	}
	words := make([]lineResult, 0, len(stressPairs)*2)
	for pi, pair := range stressPairs {
		for j, line := range pair {
			idx := pi*2 + j
			vi := idx % len(stressVoices)
			tag := stressVoices[vi].tag
			voice := voices[vi].name
			a, err := baseClient.Generate(ctx, tts.GenerateRequest{Text: line, Voice: voice})
			if err != nil && retryable(err) {
				a, err = baseClient.Generate(ctx, tts.GenerateRequest{Text: line, Voice: voice})
			}
			if err != nil {
				errCount[tag]++
				t.Fatalf("generate line %d (voice %s): %v", idx, tag, err)
			}
			rel := fmt.Sprintf("wav/line-%02d-%s.wav", idx, tag)
			abs := filepath.Join(root, stressEvidenceDir, rel)
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatalf("mkdir wav dir: %v", err)
			}
			if err := os.WriteFile(abs, a.WAV, 0o600); err != nil {
				t.Fatalf("write wav %s: %v", rel, err)
			}
			res, err := asrClient.Transcribe(ctx, TranscribeRequest{WAV: a.WAV, Language: "en"})
			if err != nil && retryable(err) {
				res, err = asrClient.Transcribe(ctx, TranscribeRequest{WAV: a.WAV, Language: "en"})
			}
			if err != nil {
				errCount[tag]++
				t.Fatalf("transcribe line %d: %v", idx, err)
			}
			clipCount[tag]++
			words = append(words, lineResult{
				line: line, wav: a.WAV, wavRel: rel,
				transcript: res.Text, reqID: res.RequestID, reqSrc: res.RequestIDSource,
				latencyMS: res.Latency.Milliseconds(),
			})
			genuine := speech.Evaluate(line, res.Text)
			t.Logf("WER stress line=%02d voice=%s transcript=%q wer=%.3f cer=%.3f request_id=%s",
				idx, tag, res.Text, genuine.WER, genuine.CER, res.RequestID)
		}
	}

	// ── Score both sets with the product's own normalizer ───────────────────
	var (
		clips       []stressClip
		genuineWERs []float64
		wrongWERs   []float64
	)
	for idx, w := range words {
		wavPath := filepath.Join(root, stressEvidenceDir, w.wavRel)
		_, partnerIdx := idx/2, idx^1
		tag := stressVoices[idx%len(stressVoices)].tag
		g := speech.Evaluate(w.line, w.transcript)
		clips = append(clips, stressClip{
			Idx: idx, Pair: idx / 2, Voice: tag,
			Kind: "genuine", Expected: w.line, Transcript: w.transcript,
			WER: g.WER, CER: g.CER, WithinBound: g.WER <= bound,
			WAV: wavPath, ASRRequestID: w.reqID, ASRLatencyMS: w.latencyMS,
		})
		genuineWERs = append(genuineWERs, g.WER)
		wrong := speech.Evaluate(words[partnerIdx].line, w.transcript)
		clips = append(clips, stressClip{
			Idx: idx, Pair: idx / 2, Voice: tag,
			Kind: "wrong_text", Expected: words[partnerIdx].line, Transcript: w.transcript,
			WER: wrong.WER, CER: wrong.CER, WithinBound: wrong.WER <= bound,
			WAV: wavPath, ASRRequestID: w.reqID, ASRLatencyMS: w.latencyMS,
		})
		wrongWERs = append(wrongWERs, wrong.WER)
	}

	// ── Statistics, verdict, evidence JSON ──────────────────────────────────
	stats := stressStats{
		GenuineN:   len(genuineWERs),
		GenuineP50: nearestRank(genuineWERs, 50),
		GenuineP95: nearestRank(genuineWERs, 95),
		GenuineP99: nearestRank(genuineWERs, 99),
		GenuineMax: nearestRank(genuineWERs, 100),
		WrongN:     len(wrongWERs),
		WrongMin:   nearestRank(wrongWERs, 1),
		WrongP1:    nearestRank(wrongWERs, 1),
		WrongP50:   nearestRank(wrongWERs, 50),
		WrongMax:   nearestRank(wrongWERs, 100),
	}
	sort.Float64s(genuineWERs)
	sort.Float64s(wrongWERs)
	maxGenuine, minWrong := genuineWERs[len(genuineWERs)-1], wrongWERs[0]
	midpoint := (maxGenuine + minWrong) / 2

	var verdict string
	switch {
	case maxGenuine <= bound && bound < minWrong:
		verdict = fmt.Sprintf("bound %.3f is evidence-based: all %d genuine clips at or under it (max %.3f) and all %d wrong-text controls strictly above it (min %.3f)",
			bound, stats.GenuineN, maxGenuine, stats.WrongN, minWrong)
	case bound < maxGenuine:
		verdict = fmt.Sprintf("bound %.3f is TOO LOW: %d genuine clip(s) sit above it (max %.3f vs min wrong-text %.3f); midpoint of the gap is %.3f",
			bound, countAbove(genuineWERs, bound), maxGenuine, minWrong, midpoint)
	default:
		verdict = fmt.Sprintf("bound %.3f is TOO HIGH: %d wrong-text control(s) sit at or under it (min %.3f vs max genuine %.3f); midpoint of the gap is %.3f",
			bound, countAtOrBelow(wrongWERs, bound), minWrong, maxGenuine, midpoint)
	}
	recommended := 0.0
	if maxGenuine > bound || bound >= minWrong {
		recommended = midpoint
	}

	evidence := stressEvidence{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Purpose:     "FR-10 acceptance gap 7: evidence for the ASR QA WER bound (ADR 0061 amendment); invented lines only",
		ASRModel:    asrCfg.Model,
		TTSModel:    baseCfg.Model,
		DesignModel: designCfg.Model,
		WERBound:    bound,
		Verdict:     verdict,
		Recommended: recommended,
		Statistics:  stats,
	}
	for _, v := range voices {
		evidence.Designs = append(evidence.Designs, stressVoice{
			Name: v.tag, Description: v.desc, RegisteredAs: v.name,
			Clips: clipCount[v.tag], Errors: errCount[v.tag],
		})
	}
	evidence.Clips = clips
	if err := os.MkdirAll(filepath.Join(root, stressEvidenceDir), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", stressEvidenceDir, err)
	}
	evPath := filepath.Join(root, stressEvidenceDir, fmt.Sprintf("stress-%d.json", time.Now().Unix()))
	if err := os.WriteFile(evPath, mustMarshalEvidence(t, evidence), 0o600); err != nil {
		t.Fatalf("write evidence %s: %v", evPath, err)
	}

	t.Logf("WER stress stats: genuine n=%d p50=%.3f p95=%.3f p99=%.3f max=%.3f | wrong n=%d min=%.3f p1=%.3f p50=%.3f max=%.3f",
		stats.GenuineN, stats.GenuineP50, stats.GenuineP95, stats.GenuineP99, stats.GenuineMax,
		stats.WrongN, stats.WrongMin, stats.WrongP1, stats.WrongP50, stats.WrongMax)
	t.Logf("WER stress verdict: %s", verdict)
	t.Logf("WER stress evidence: %s (%d clips scored, %d WAVs under %s/wav/)", evPath, len(clips), len(words), stressEvidenceDir)

	// ── The assertion: the in-effect bound must separate the two sets ──────
	for _, c := range clips {
		if c.Kind == "genuine" && c.WER > bound {
			t.Errorf("genuine line %d exceeds the bound: wer=%.3f bound=%.3f transcript=%q expected=%q",
				c.Idx, c.WER, bound, c.Transcript, c.Expected)
		}
		if c.Kind == "wrong_text" && c.WER <= bound {
			t.Errorf("wrong-text control line %d would PASS QA: wer=%.3f bound=%.3f (the bound does not separate)",
				c.Idx, c.WER, bound)
		}
	}
	if maxGenuine >= minWrong {
		t.Errorf("no separation in the data: max genuine %.3f >= min wrong-text %.3f; the bound cannot be set from these distributions", maxGenuine, minWrong)
	}
}

func countAbove(values []float64, v float64) int {
	n := 0
	for _, x := range values {
		if x > v {
			n++
		}
	}
	return n
}

func countAtOrBelow(values []float64, v float64) int {
	n := 0
	for _, x := range values {
		if x <= v {
			n++
		}
	}
	return n
}
