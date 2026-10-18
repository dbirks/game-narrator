package tools

// Voice tools: analyze_character, design_voice
// (docs/spec/03 "Voice design and editorial workflow").
//
// analyze_character produces an editable voice_brief grounded in the
// character's evidence (docs/spec/03 "Voice design and editorial workflow",
// ADR-0046): the character's own extracted messages (cited lines) when they
// exist, wrapped in the untrusted-data delimiters before they reach the model
// (docs/spec/14); or, for a web-researched character with no lines on the
// profile, the researched character facts plus the metadata of the source
// rows stage_character recorded — never page content. Every creative field
// in the brief is labelled creative: true so a downstream user (or the model)
// can never mistake an invented description for verified lore.
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

// voiceBriefSystemPromptResearched is the companion contract for the
// research-grounded mode (ADR-0046): no dialogue lines exist on the profile,
// the evidence is the researched character facts plus the metadata of the
// sources that evidence them, and the same "creative, never verified lore"
// rule applies to those facts.
const voiceBriefSystemPromptResearched = `You write voice-design briefs for a text-to-speech voice design checkpoint.
Given a character's name, role and researched character facts plus the metadata of the sources that evidence them, produce ONE JSON object named "voice_brief" with exactly these fields: vocal_age_impression, timbre, cadence, emotional_baseline, language_notes, pronunciation_notes.
Each field is a short string (at most 40 words). vocal_age_impression is an age IMPRESSION for a voice actor, never a claim about a real person.
The character facts are the ONLY evidence about the game world available to you; no dialogue lines exist. Imagination is allowed and expected for the creative fields, but you must not assert any fact about the game world that is not present in the character facts: creative fields describe a performance, never verified lore, and never a real person.
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

	// Verified source grounding (ADR-0046): stage_character records the source
	// rows a researched character was built from. Only rows that exist on this
	// profile count; the character's refs must survive that filter to qualify
	// for the research-grounded brief.
	var refs []string
	var refSet map[string]bool
	if len(cited) == 0 && len(ch.VerifiedSourceRefs) > 0 {
		var rawRefs []string
		if err := json.Unmarshal(ch.VerifiedSourceRefs, &rawRefs); err == nil {
			seen := map[string]bool{}
			for _, id := range rawRefs {
				if strings.TrimSpace(id) != "" && !seen[id] {
					seen[id] = true
					refs = append(refs, id)
				}
			}
			refSet = seen
		}
	}
	var sources []games.Source
	if len(refs) > 0 {
		all, err := d.Store.ListSources(ctx, profileID)
		if err != nil {
			return agent.Result{}, fmt.Errorf("tools: analyze_character: list sources: %w", err)
		}
		for _, s := range all {
			if refSet[s.ID] {
				sources = append(sources, s)
			}
		}
		if len(sources) == 0 {
			return agent.Result{}, fmt.Errorf("character %s cites no resolvable sources on profile %s; stage it with stage_character or import a labelled script first", ch.ID, profileID)
		}
		// Keep only the staged refs that resolved to a source row, in the
		// staged order: a ref with no row is not evidence.
		kept := map[string]bool{}
		for _, s := range sources {
			kept[s.ID] = true
		}
		validRefs := refs[:0]
		for _, id := range refs {
			if kept[id] {
				validRefs = append(validRefs, id)
			}
		}
		refs = validRefs
	}
	if len(cited) == 0 && len(refs) == 0 {
		return agent.Result{}, fmt.Errorf("character %s has no cited lines on profile %s; import a labelled script first (docs/spec/04: speaker identity must be evidenced, not invented)", ch.ID, profileID)
	}

	// The evidence that grounds the brief (ADR-0046): the character's own
	// cited lines when they exist, otherwise the source rows the staging
	// recorded. Source rows contribute METADATA only (id, URI, rights,
	// retrieval time) — never page content: fetched text is untrusted
	// (docs/spec/14) and must not be re-quoted into the prompt.
	var (
		systemPrompt string
		userMsg      string
		grounding    = map[string]any{
			"mode":        "cited_lines",
			"cited_lines": len(cited),
			"source_ids":  []string{},
		}
	)
	if len(cited) > 0 {
		// The lines are untrusted data (docs/spec/14): they enter the prompt
		// only inside the delimited block, so a line that looks like an
		// instruction is content, not a command.
		systemPrompt = voiceBriefSystemPrompt
		userMsg = fmt.Sprintf("Character name: %s\nRole: %s\n\nCited lines (exactly what the source says; nothing else is known):\n%s",
			ch.Name, roleOrUnknown(ch.Role), agent.QuoteUntrustedText("analyze_character", strings.Join(lineTexts, "\n")))
	} else {
		// Researched-character mode: grounded in the staged sources and the
		// character facts, with the evidence listed by id (never by content).
		var evidence []string
		for _, src := range sources { // already filtered to the character's refs
			evidence = append(evidence, fmt.Sprintf("- %s %s (rights %s, retrieved %s)", src.ID, src.URI, src.Rights, src.RetrievedAt))
		}
		grounding = map[string]any{
			"mode":        "researched_character",
			"cited_lines": 0,
			"source_ids":  refs,
			"note":        "no dialogue lines on this profile; the brief is grounded in the researched character facts and the sources named (ADR-0046)",
		}
		systemPrompt = voiceBriefSystemPromptResearched
		facts := "(no researched summary recorded)"
		if ch.PersonalitySummary != "" {
			facts = "Research character facts from this run's web research (the only evidence available):\n" + ch.PersonalitySummary
		}
		userMsg = fmt.Sprintf("Character name: %s\nRole: %s\n%s\n\nEvidence sources (metadata only; page content was not quoted to you):\n%s",
			ch.Name, roleOrUnknown(ch.Role), facts, strings.Join(evidence, "\n"))
	}

	resp, err := d.Chat(ctx, openai.ChatRequest{
		Messages: []openai.Message{
			{Role: agent.RoleSystem, Text: systemPrompt},
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

	// The model reads "one JSON object named voice_brief" two ways: the
	// fields at the top level, or wrapped in a {"voice_brief": {...}}
	// object — which mirrors this tool's own result shape, so the
	// interpretation is reasonable. Accept both; the normalised labelled
	// output is the same either way.
	if _, hasField := brief[voiceBriefFields[0]]; !hasField {
		if nested, ok := brief["voice_brief"].(map[string]any); ok {
			brief = nested
		}
	}

	// The brief drives the VoiceDesign audition (docs/spec/03, docs/spec/07):
	// every creative field must carry content, so the audition instruction
	// is complete instead of silently degraded. A field the model left
	// empty (or did not write as a string) is a prompt violation, not a
	// valid output: the failed result goes back to the agent, which re-runs
	// the call with the error in context (the same loop that repairs a
	// refused stage_character argument).
	var incomplete []string
	for _, f := range voiceBriefFields {
		s, _ := brief[f].(string)
		if strings.TrimSpace(s) == "" {
			incomplete = append(incomplete, f)
		}
	}
	if len(incomplete) > 0 {
		return agent.Result{}, fmt.Errorf("tools: analyze_character: the model left the brief field(s) %s empty; every voice_brief field must be a short non-empty string", strings.Join(incomplete, ", "))
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

	if cited == nil {
		cited = []map[string]any{} // JSON: an empty array, not null
	}
	out := map[string]any{
		"character_id":   ch.ID,
		"character_name": ch.Name,
		"voice_brief":    labelled,
		"cited_lines":    cited,
		"grounding":      grounding,
		"brief_editable": true,
		"note":           "creative fields are labelled creative:true (never verified lore); pass an edited brief as brief_override to design_voice",
	}
	var sourceIDs []string
	for _, id := range grounding["source_ids"].([]string) {
		sourceIDs = append(sourceIDs, id)
	}
	res, err := okResult(out, sourceIDs...)
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
