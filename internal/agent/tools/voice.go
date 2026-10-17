package tools

// Voice tools: analyze_character, design_voice
// (docs/spec/03 "Voice design and editorial workflow").
//
// analyze_character produces an editable voice_brief from CITED LINES ONLY —
// the character's own extracted messages, wrapped in the untrusted-data
// delimiters before they reach the model (docs/spec/14). Every creative
// field in the brief is labelled creative: true so a downstream user (or the
// model) can never mistake an invented description for verified lore.
//
// design_voice generates three reference auditions from SYNTHETIC text only
// (never game text: the reference clip must not require rights the user has
// not granted), stores the WAVs as draft assets, and stops at the review
// boundary — voice approval is a human step (docs/spec/03: "User selects
// one"). The approved voice (profiles.preferred_voice_id) is never touched
// by any tool in this package.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/store"
)

// maxCitedLines bounds how many of a character's lines the brief model sees
// (token budget + the brief is a sketch, not an essay).
const maxCitedLines = 12

// voiceBriefFields are the creative fields the brief model must fill
// (docs/spec/03 "structured editable voice_brief").
var voiceBriefFields = []string{
	"vocal_age_impression",
	"timbre",
	"cadence",
	"emotional_baseline",
	"language_notes",
	"pronunciation_notes",
}

// voiceBriefSystemPrompt is configuration-derived (never built from model or
// web output), mirroring the agent core's prompt rule.
const voiceBriefSystemPrompt = `You write voice-design briefs for a text-to-speech voice design checkpoint.
Given a character's name, role and quoted dialogue lines, produce ONE JSON
object named "voice_brief" with exactly these fields: vocal_age_impression,
timbre, cadence, emotional_baseline, language_notes, pronunciation_notes.
Each field is a short string (at most 40 words). vocal_age_impression is an
age IMPRESSION for a voice actor, never a claim about a real person.
Imagination is allowed and expected for the creative fields, but you must not
assert any fact about the game world that is not present in the quoted lines:
creative fields describe a performance, never verified lore.
Answer with JSON only.`

func (d Deps) analyzeCharacter(ctx context.Context, call agent.Call) (agent.Result, error) {
	m, err := parseArgs(call)
	if err != nil {
		return agent.Result{}, err
	}
	profileID, err := require(m, "profile_id")
	if err != nil {
		return agent.Result{}, err
	}
	characterID, err := require(m, "character_id")
	if err != nil {
		return agent.Result{}, err
	}
	if d.Chat == nil {
		return agent.Result{}, fmt.Errorf("no chat role configured for analyze_character (fast_reasoning/agent_reasoning)")
	}

	chars, err := d.Store.ListCharacters(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: analyze_character: list characters: %w", err)
	}
	var ch games.Character
	for _, c := range chars {
		if c.ID == characterID {
			ch = c
		}
	}
	if ch.ID == "" {
		return agent.Result{}, fmt.Errorf("character %s is not on profile %s", characterID, profileID)
	}

	// Cited lines only: the character's own extracted messages.
	lines, _, err := d.Store.ListMessages(ctx, profileID, store.MessageFilter{SpeakerID: characterID}, "", maxCitedLines)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: analyze_character: list lines: %w", err)
	}
	var lineTexts []string
	var cited []map[string]any
	for _, l := range lines {
		text := strings.TrimSpace(l.NormalizedText)
		if text == "" {
			text = strings.TrimSpace(l.OriginalText)
		}
		if text == "" {
			continue
		}
		lineTexts = append(lineTexts, "- "+text)
		cited = append(cited, map[string]any{"message_id": l.ID, "text": clip(text, 400)})
	}
	if len(cited) == 0 {
		return agent.Result{}, fmt.Errorf("character %s has no cited lines on profile %s; import a labelled script first (docs/spec/04: speaker identity must be evidenced, not invented)", ch.ID, profileID)
	}

	// The lines are untrusted data (docs/spec/14): they enter the prompt only
	// inside the delimited block, so a line that looks like an instruction is
	// content, not a command.
	userMsg := fmt.Sprintf("Character name: %s\nRole: %s\n\nCited lines (exactly what the source says; nothing else is known):\n%s",
		ch.Name, roleOrUnknown(ch.Role), agent.QuoteUntrustedText("analyze_character", strings.Join(lineTexts, "\n")))

	resp, err := d.Chat(ctx, openai.ChatRequest{
		Messages: []openai.Message{
			{Role: agent.RoleSystem, Text: voiceBriefSystemPrompt},
			{Role: agent.RoleUser, Text: userMsg},
		},
		ResponseFormatJSON: true,
		// The gateway's 27B rejects response_format json_object (NInfer build
		// 2026-10-17). Opt into the degraded JSON path: retry once without
		// response_format plus a "single JSON object only" instruction and
		// validate the response is one parseable object (docs/spec/06).
		JSONModeFallback: true,
	})
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: analyze_character: model call: %w", err)
	}

	raw := strings.TrimSpace(resp.Text)
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:] // strip any preamble
	}
	if strings.HasSuffix(raw, "```") || strings.Contains(raw, "```") {
		if i := strings.Index(raw, "{"); i >= 0 {
			if j := strings.LastIndex(raw, "}"); j > i {
				raw = raw[i : j+1]
			}
		}
	}
	var brief map[string]any
	if err := json.Unmarshal([]byte(raw), &brief); err != nil {
		return agent.Result{}, fmt.Errorf("tools: analyze_character: model returned no valid JSON brief: %v", err)
	}

	// Every creative field is labelled: the tool re-wraps the model's output
	// so the labelling cannot depend on the model remembering to do it.
	labelled := map[string]any{}
	for _, f := range voiceBriefFields {
		v, ok := brief[f]
		if !ok {
			v = nil
		}
		labelled[f] = map[string]any{"value": v, "creative": true}
	}
	for k := range brief {
		if _, known := labelled[k]; !known {
			labelled[k] = map[string]any{"value": brief[k], "creative": true}
		}
	}

	out := map[string]any{
		"character_id":   ch.ID,
		"character_name": ch.Name,
		"voice_brief":    labelled,
		"cited_lines":    cited,
		"brief_editable": true,
		"note":           "creative fields are labelled creative:true (never verified lore); pass an edited brief as brief_override to design_voice",
	}
	res, err := okResult(out)
	if err != nil {
		return agent.Result{}, err
	}
	return res, nil
}

func roleOrUnknown(role string) string {
	if strings.TrimSpace(role) == "" {
		return "(unspecified)"
	}
	return role
}

// syntheticAuditionLines are the fixed synthetic texts the auditions speak.
// They are deliberately NOT game text: a reference clip derived from game
// dialogue would carry rights questions the user never approved
// (docs/spec/03: auditions come from synthetic test text or APPROVED game
// text; synthetic always qualifies).
var syntheticAuditionLines = []string{
	"Hello there. Let me tell you a little about today.",
	"The morning was calm, and the road ahead was long.",
	"I would like to ask you a small question, if that is all right.",
}

func (d Deps) designVoice(ctx context.Context, call agent.Call) (agent.Result, error) {
	m, err := parseArgs(call)
	if err != nil {
		return agent.Result{}, err
	}
	profileID, err := require(m, "profile_id")
	if err != nil {
		return agent.Result{}, err
	}
	characterID, err := require(m, "character_id")
	if err != nil {
		return agent.Result{}, err
	}
	if d.TTS == nil {
		return agent.Result{}, fmt.Errorf("no voice design provider configured (tts.Design)")
	}

	chars, err := d.Store.ListCharacters(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: design_voice: list characters: %w", err)
	}
	var ch games.Character
	for _, c := range chars {
		if c.ID == characterID {
			ch = c
		}
	}
	if ch.ID == "" {
		return agent.Result{}, fmt.Errorf("character %s is not on profile %s", characterID, profileID)
	}

	briefJSON := strings.TrimSpace(strArg(m, "brief_override"))
	if briefJSON == "" {
		briefJSON = `{"note":"no voice brief provided; defaulting to a neutral, warm reading voice"}`
	} else if !json.Valid([]byte(briefJSON)) {
		return agent.Result{}, fmt.Errorf("argument %q is not a JSON object", "brief_override")
	}
	if len(briefJSON) > 4000 {
		briefJSON = clip(briefJSON, 4000)
	}
	instructions := fmt.Sprintf("Voice for the character %s. Design instructions (JSON, may be partly creative): %s", ch.Name, briefJSON)

	sum := sha256.Sum256([]byte(briefJSON))
	type auditionOut struct {
		Index      int    `json:"index"`
		SHA256     string `json:"sha256"`
		SizeBytes  int64  `json:"size_bytes"`
		DurationMS int64  `json:"duration_ms"`
		Transcript string `json:"transcript"`
		Origin     string `json:"origin"`
	}
	auditions := make([]auditionOut, 0, len(syntheticAuditionLines))
	for i, line := range syntheticAuditionLines {
		audio, err := d.TTS.Design(ctx, tts.DesignRequest{
			Text:         line,
			Instructions: instructions,
			Language:     "English",
		})
		if err != nil {
			return agent.Result{}, fmt.Errorf("tools: design_voice: audition %d: %w", i+1, err)
		}
		staged, _, err := d.Assets.PutStaged(ctx, strings.NewReader(string(audio.WAV)), "")
		if err != nil {
			return agent.Result{}, fmt.Errorf("tools: design_voice: stage audition %d: %w", i+1, err)
		}
		committed, err := d.Assets.Commit(ctx, staged)
		if err != nil {
			_ = d.Assets.Abort(context.WithoutCancel(ctx), staged)
			return agent.Result{}, fmt.Errorf("tools: design_voice: commit audition %d: %w", i+1, err)
		}
		auditions = append(auditions, auditionOut{
			Index: i + 1, SHA256: committed.SHA256, SizeBytes: committed.Size,
			DurationMS: int64(audio.Duration.Milliseconds()),
			Transcript: line, Origin: audio.Origin,
		})
	}
	out := map[string]any{
		"character_id":   ch.ID,
		"character_name": ch.Name,
		"brief_digest":   hex.EncodeToString(sum[:]),
		"auditions":      auditions,
		"text_source":    "synthetic test text (never game text; no rights required)",
		"note":           "voice approval is a human step: pick one audition and register it outside the agent (docs/spec/03); the approved voice is unchanged by this tool",
	}
	res, err := okResult(out)
	if err != nil {
		return agent.Result{}, err
	}
	// Voice approval is a human step (docs/spec/03 scenario 6: the approved
	// voice remains while the draft brief/auditions change): the run stops
	// at the review boundary.
	res.ReviewRequired = true
	return res, nil
}

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

var (
	analyzeCharacterSchema = json.RawMessage(`{"type":"object","properties":{
		"profile_id":{"type":"string"},
		"character_id":{"type":"string"}
	},"required":["profile_id","character_id"]}`)

	designVoiceSchema = json.RawMessage(`{"type":"object","properties":{
		"profile_id":{"type":"string"},
		"character_id":{"type":"string"},
		"brief_override":{"type":"string","maxLength":4000}
	},"required":["profile_id","character_id"]}`)
)
