package tools

// stage_character (ADR-0046): the missing link between web research and the
// profile's character roster (docs/spec/03 chain "search_sources → … →
// infer characters+roles", implemented here as the research-grounded half).
//
// Before this tool, characters entered a profile only through parse_dialogue
// (speaker labels in an imported script), so a web-research-only onboarding
// run — no user script — could never produce a character list, and
// analyze_character had no character (let alone cited lines) to brief. The
// tool stages a researched character into the DRAFT roster with its
// supporting source ids as verified_source_refs, which is the grounding
// analyze_character's research mode (ADR-0046) reads.
//
// Guardrails:
//   - every source_id must be a provenance row already registered on the
//     profile (by web_search/fetch_page with profile_id, or an import) — a
//     character cannot cite a source the run never retrieved;
//   - the name is the roster identity: re-staging the same name updates the
//     existing row (idempotent across retries), it never duplicates;
//   - the summary and role are the agent's researched facts about the
//     character; they enter analyze_character's prompt labelled as facts
//     from this run's web research, never as verified lore.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/games"
)

// maxStageSources bounds how many source rows one character may cite.
const maxStageSources = 8

func (d Deps) stageCharacter(ctx context.Context, call agent.Call) (agent.Result, error) {
	m, err := parseArgs(call)
	if err != nil {
		return agent.Result{}, err
	}
	profileID, err := require(m, "profile_id")
	if err != nil {
		return agent.Result{}, err
	}
	if !games.ValidID(profileID) {
		return agent.Result{}, fmt.Errorf("argument %q is not a ULID", "profile_id")
	}
	name := strings.TrimSpace(strArg(m, "name"))
	if name == "" {
		return agent.Result{}, fmt.Errorf("argument %q is required and must not be blank", "name")
	}
	role := strings.TrimSpace(strArg(m, "role"))
	summary := strings.TrimSpace(strArg(m, "personality_summary"))
	if len(summary) > 4000 {
		return agent.Result{}, fmt.Errorf("argument %q exceeds 4000 chars", "personality_summary")
	}

	// source_ids: required, and every one of them must be a provenance row on
	// this profile — the character is grounded in sources the run actually
	// retrieved (docs/spec/03: "every factual claim about game content must
	// attach one or more source_id references").
	rawIDs, err := stringListArg(m, "source_ids", 1, maxStageSources)
	if err != nil {
		return agent.Result{}, err
	}
	sources, err := d.Store.ListSources(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: stage_character: list sources: %w", err)
	}
	known := make(map[string]bool, len(sources))
	for _, s := range sources {
		known[s.ID] = true
	}
	seen := map[string]bool{}
	sourceIDs := make([]string, 0, len(rawIDs))
	for _, id := range rawIDs {
		if !known[id] {
			return agent.Result{}, fmt.Errorf("source %s is not registered on profile %s; stage_character only grounds characters in sources this run retrieved or imported", id, profileID)
		}
		if !seen[id] {
			seen[id] = true
			sourceIDs = append(sourceIDs, id)
		}
	}
	refs, err := json.Marshal(sourceIDs)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: stage_character: encode source refs: %w", err)
	}

	// Name is the roster identity: an existing row with the same name is
	// updated in place (a retry or a second research pass must not fork the
	// roster), a new name mints a row in the proposed state.
	chars, err := d.Store.ListCharacters(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: stage_character: list characters: %w", err)
	}
	var ch games.Character
	updated := false
	for _, c := range chars {
		if strings.EqualFold(c.Name, name) {
			ch = c
			updated = true
			break
		}
	}
	if ch.ID == "" {
		ch = games.Character{
			ID: games.NewID(), ProfileID: profileID, Status: games.CharacterProposed,
			CreatedAt: nowStamp(),
		}
	}
	ch.Name = name
	ch.Role = role
	ch.PersonalitySummary = summary
	ch.VerifiedSourceRefs = refs
	if ch.Status == "" {
		ch.Status = games.CharacterProposed
	}

	// The write goes through the same draft-revision gate every content tool
	// uses: a non-mutable revision refuses the row, not the tool.
	if err := d.Store.PutCharacters(ctx, profileID, []games.Character{ch}); err != nil {
		return agent.Result{}, fmt.Errorf("tools: stage_character: store character: %w", err)
	}
	out := map[string]any{
		"character_id": ch.ID,
		"name":         name,
		"role":         role,
		"status":       string(ch.Status),
		"updated":      updated,
		"source_ids":   sourceIDs,
		"note":         "the character is grounded in the sources named; with no dialogue lines yet, analyze_character works from this grounding (ADR-0046)",
	}
	return okResult(out, sourceIDs...)
}

// stringListArg reads a de-duplicated-eligible JSON string array from the
// call's arguments, enforcing min/max counts and per-item length.
func stringListArg(m map[string]any, key string, min, max int) ([]string, error) {
	raw, ok := m[key]
	if !ok {
		return nil, fmt.Errorf("argument %q is required", key)
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("argument %q must be an array of strings", key)
	}
	if len(arr) < min || len(arr) > max {
		return nil, fmt.Errorf("argument %q needs %d-%d entries, got %d", key, min, max, len(arr))
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("argument %q must be an array of non-empty strings", key)
		}
		if len(s) > 64 {
			return nil, fmt.Errorf("argument %q: entries exceed 64 chars", key)
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, nil
}

var stageCharacterSchema = json.RawMessage(`{"type":"object","properties":{
	"profile_id":{"type":"string"},
	"name":{"type":"string","maxLength":200},
	"role":{"type":"string","maxLength":100},
	"personality_summary":{"type":"string","maxLength":4000},
	"source_ids":{"type":"array","items":{"type":"string","maxLength":64},"minItems":1,"maxItems":8}
},"required":["profile_id","name","source_ids"]}`)
