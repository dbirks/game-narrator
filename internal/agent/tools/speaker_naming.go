package tools

// name_speaker + list_unnamed_speakers (ADR-0187): the agent half of the
// Cast page's "Name speakers with the agent" button. A source import that
// attributes lines to opaque speaker ids (the Wind Waker BMG's per-message
// attribute byte) lands them on proposed placeholder characters whose name
// IS the key (e.g. ww:spk:12) and whose lines are proposed. The curator's
// manual path is the speaker-link API (ADR-0070); this tool is the same
// action, draft-only, driven by the naming run (onboarding run plan task
// "name_speakers"): the model reads each placeholder's lines through
// list_unnamed_speakers and, for every speaker it can identify, records the
// name with name_speaker. What the naming run writes:
//
//   - the placeholder row's name becomes the identified name (its lines
//     already point at that row, so nothing is re-pointed);
//   - the speaker key folds into the row's aliases, so a re-import binds the
//     key straight to the named row (the same alias fold the speaker-link
//     action does, ADR-0070);
//   - the voice class phrase ("Voice class: adult male") and the naming
//     provenance (key, confidence, evidence) go into the personality
//     summary — the cast audit (internal/cast) reads summary+role as its
//     expected-class profile text, so the audit picks the class up;
//   - the row stays in its proposed state: the run ends in the review flow,
//     never published by the agent.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// speakerKeyPattern is the opaque speaker-key shape the imports emit: a
// short namespace, "spk", a number (ww:spk:12). A placeholder is a
// non-retired character whose name matches it — an invented name never
// collides with the pattern.
var speakerKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*:spk:[0-9]+$`)

// maxSampleLines bounds how many lines list_unnamed_speakers carries per
// speaker; maxSampleLineLen truncates each one (the model reasons from a
// sample, not a transcript dump).
const (
	maxSampleLines   = 5
	maxSampleLineLen = 240
	// maxListSpeakers bounds one listing: a pathological revision cannot push
	// an unbounded payload through the loop.
	maxListSpeakers = 100
)

// UnnamedSpeaker is one placeholder the naming run may work on: the key the
// import used, the roster row it sits on, how many lines carry it, and up to
// maxSampleLines lines (truncated) for the model to reason from.
type UnnamedSpeaker struct {
	SpeakerKey  string   `json:"speaker_key"`
	CharacterID string   `json:"character_id"`
	LineCount   int      `json:"line_count"`
	SampleLines []string `json:"sample_lines"`
}

// UnnamedSpeakers is the shared scan behind list_unnamed_speakers: every
// non-retired character named by an opaque speaker key, with its line count
// and sample lines. The api package reuses it to scale the naming run's
// step budget (ADR-0187); it touches only draft-state reads.
func UnnamedSpeakers(ctx context.Context, st *store.Store, profileID string) ([]UnnamedSpeaker, error) {
	chars, err := st.ListCharacters(ctx, profileID)
	if err != nil {
		return nil, fmt.Errorf("tools: list unnamed speakers: list characters: %w", err)
	}
	placeholders := make(map[string]bool, len(chars))
	for _, c := range chars {
		if c.Status != games.CharacterRetired && speakerKeyPattern.MatchString(c.Name) {
			placeholders[c.ID] = true
		}
	}
	if len(placeholders) == 0 {
		return nil, nil
	}
	counts := make(map[string]int, len(placeholders))
	samples := make(map[string][]string, len(placeholders))
	cursor := ""
	for {
		page, next, err := st.ListMessages(ctx, profileID, store.MessageFilter{}, cursor, store.MaxListLimit)
		if err != nil {
			return nil, fmt.Errorf("tools: list unnamed speakers: list messages: %w", err)
		}
		for _, m := range page {
			if !placeholders[m.SpeakerID] {
				continue
			}
			counts[m.SpeakerID]++
			if len(samples[m.SpeakerID]) < maxSampleLines {
				line := m.SpeechText
				if line == "" {
					line = m.NormalizedText
				}
				samples[m.SpeakerID] = append(samples[m.SpeakerID], truncateLine(line, maxSampleLineLen))
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	out := make([]UnnamedSpeaker, 0, len(placeholders))
	for _, c := range chars {
		if !placeholders[c.ID] {
			continue
		}
		out = append(out, UnnamedSpeaker{
			SpeakerKey:  c.Name,
			CharacterID: c.ID,
			LineCount:   counts[c.ID],
			SampleLines: samples[c.ID],
		})
		if len(out) >= maxListSpeakers {
			break
		}
	}
	return out, nil
}

// truncateLine cuts a sample line at the cap, marking the cut: the model
// must be able to see a line was not shown in full.
func truncateLine(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

func (d Deps) listUnnamedSpeakers(ctx context.Context, call agent.Call) (agent.Result, error) {
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
	speakers, err := UnnamedSpeakers(ctx, d.Store, profileID)
	if err != nil {
		return agent.Result{}, err
	}
	out := map[string]any{
		"profile_id":       profileID,
		"unnamed_speakers": speakers,
	}
	if len(speakers) == 0 {
		out["note"] = "no unnamed speaker placeholders remain on this revision"
	} else {
		out["note"] = "name the ones you can identify with name_speaker; leave the ones you cannot (do not guess)"
	}
	return okResult(out)
}

// nameSpeakerClassPhrase is the voice-class sentence name_speaker appends to
// the personality summary. The cast audit's expected-class scan
// (internal/cast.ExpectedClass) reads the summary as profile text, so the
// phrase must carry its keywords: "male"/"man", "female"/"woman", "boy",
// "girl", "creature".
var nameSpeakerClassPhrase = map[string]string{
	"adult_male":   "Voice class: adult male.",
	"adult_female": "Voice class: adult female.",
	"boy":          "Voice class: young boy.",
	"girl":         "Voice class: young girl.",
	"elder_male":   "Voice class: elderly man.",
	"elder_female": "Voice class: elderly woman.",
	"creature":     "Voice class: non-human creature (no gender expectation).",
	"unknown":      "", // no phrase: an honest "unknown" is no class claim
}

// maxNameEvidence bounds the evidence sentence: one run may name dozens of
// speakers and the summary has a 4000-char ceiling.
const maxNameEvidence = 1000

func (d Deps) nameSpeaker(ctx context.Context, call agent.Call) (agent.Result, error) {
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
	key := strings.TrimSpace(strArg(m, "speaker_key"))
	if key == "" || !speakerKeyPattern.MatchString(key) {
		return agent.Result{}, fmt.Errorf("argument %q must be an opaque speaker key like \"ww:spk:12\"", "speaker_key")
	}
	name := strings.TrimSpace(strArg(m, "name"))
	if name == "" {
		return agent.Result{}, fmt.Errorf("argument %q is required and must not be blank", "name")
	}
	if len(name) > 200 {
		return agent.Result{}, fmt.Errorf("argument %q exceeds 200 chars", "name")
	}
	// The key must not look like a real name and the name must not be the key.
	if speakerKeyPattern.MatchString(name) {
		return agent.Result{}, fmt.Errorf("argument %q looks like a speaker key; name_speaker takes the identified name, the key goes in speaker_key", "name")
	}
	genderAge, err := require(m, "gender_age")
	if err != nil {
		return agent.Result{}, err
	}
	classPhrase, ok := nameSpeakerClassPhrase[genderAge]
	if !ok {
		return agent.Result{}, fmt.Errorf("argument %q must be one of adult_male, adult_female, boy, girl, elder_male, elder_female, creature, unknown", "gender_age")
	}
	confRaw, ok := m["confidence"]
	if !ok {
		return agent.Result{}, fmt.Errorf("argument %q is required (a number 0..1)", "confidence")
	}
	confidence, ok := confRaw.(float64)
	if !ok || confidence < 0 || confidence > 1 {
		return agent.Result{}, fmt.Errorf("argument %q must be a number between 0 and 1", "confidence")
	}
	evidence := strings.TrimSpace(strArg(m, "evidence"))
	if len(evidence) > maxNameEvidence {
		evidence = evidence[:maxNameEvidence] + "…"
	}

	profile, _, err := d.Store.GetProfile(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: name_speaker: get profile: %w", err)
	}
	// Draft-only, stated plainly (the store's content write would refuse an
	// immutable revision anyway, but a review revision is frozen for
	// curation and the recovery is the same sentence).
	if profile.Status != games.StatusDraft {
		return agent.Result{}, fmt.Errorf("profile %s is %s; name_speaker may only be recorded on a draft — derive a revision and name there", profileID, profile.Status)
	}

	// Find the placeholder row: the row named by the key itself, or a row
	// the key was folded into by an earlier naming (idempotent retry).
	chars, err := d.Store.ListCharacters(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: name_speaker: list characters: %w", err)
	}
	var ch games.Character
	var foundByAlias bool
	for _, c := range chars {
		if c.Status == games.CharacterRetired {
			continue
		}
		if c.Name == key {
			ch = c
			break
		}
		if aliasList(c.Aliases) != nil && strings.Contains(strings.Join(aliasList(c.Aliases), " "), key) {
			ch = c
			foundByAlias = true
			break
		}
	}
	if ch.ID == "" {
		return agent.Result{}, fmt.Errorf("no speaker placeholder named %s on this revision (the import that created it was for a different revision, or the speaker is already named)", key)
	}
	if foundByAlias && !strings.EqualFold(ch.Name, name) {
		// The key was already named with a different name: re-pointing or
		// overwriting a named row is the curator's speaker-link/curation
		// path, not the naming run's. The run must not second-guess a name
		// that is not its own.
		return agent.Result{}, fmt.Errorf("speaker %s is already named %q; a different name is the curator's to record via the speaker-link action, not name_speaker", key, ch.Name)
	}

	if ch.Status == games.CharacterRetired {
		return agent.Result{}, fmt.Errorf("speaker %s is retired (its identity was linked to another character)", key)
	}

	// Fold the key into the aliases (a re-import's roster match keys on the
	// folded alias, so the key binds to the named row from the next import
	// on) — the same fold the speaker-link action records (ADR-0070).
	aliases := mergeStringList(ch.Aliases, key)

	// The summary carries both the voice-class phrase (the cast audit's
	// expected-class scan) and the provenance (which key, which confidence,
	// what the evidence was). Existing summary text is kept, truncated to
	// fit the 4000-char ceiling.
	var b strings.Builder
	addition := ""
	if classPhrase != "" {
		addition += " " + classPhrase
	}
	addition += fmt.Sprintf(" Named by the agent from speaker key %s (confidence %.2f)", key, confidence)
	if evidence != "" {
		addition += fmt.Sprintf(": %s", evidence)
	}
	keep := ch.PersonalitySummary
	if len(keep) > 0 {
		if !strings.HasSuffix(keep, " ") {
			b.WriteString(" ")
		}
	}
	room := 4000 - len(addition)
	if room < 0 {
		room = 0
	}
	if len(keep)+1 > room {
		keep = keep[:room]
	}
	b.WriteString(keep)
	b.WriteString(addition)
	ch.Name = name
	ch.Aliases = aliases
	ch.PersonalitySummary = b.String()
	if err := ch.Validate(); err != nil {
		return agent.Result{}, fmt.Errorf("tools: name_speaker: named row fails validation: %w", err)
	}
	if err := d.Store.PutCharacters(ctx, profileID, []games.Character{ch}); err != nil {
		return agent.Result{}, fmt.Errorf("tools: name_speaker: store character: %w", err)
	}
	return okResult(map[string]any{
		"character_id": ch.ID,
		"speaker_key":  key,
		"name":         name,
		"gender_age":   genderAge,
		"confidence":   confidence,
		"status":       string(ch.Status),
		"renamed":      !foundByAlias,
		"note":         "recorded on the draft; the naming run ends in review — the curator approves the name, the cast audit reads the voice class from the summary",
	})
}

// aliasList reads a character's aliases JSON array the way the store stores
// them; corrupt or empty input is the empty list.
func aliasList(raw json.RawMessage) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// mergeStringList appends add to a JSON string-array field, skipping exact
// duplicates and keeping the existing order.
func mergeStringList(raw json.RawMessage, add ...string) json.RawMessage {
	cur := aliasList(raw)
	seen := make(map[string]bool, len(cur))
	for _, s := range cur {
		seen[s] = true
	}
	for _, s := range add {
		if s == "" || seen[s] {
			continue
		}
		cur = append(cur, s)
		seen[s] = true
	}
	out, err := json.Marshal(cur)
	if err != nil {
		return json.RawMessage(`[]`)
	}
	return out
}

var listUnnamedSpeakersSchema = json.RawMessage(`{"type":"object","properties":{
	"profile_id":{"type":"string"}
},"required":["profile_id"]}`)

var nameSpeakerSchema = json.RawMessage(`{"type":"object","properties":{
	"profile_id":{"type":"string"},
	"speaker_key":{"type":"string","maxLength":64},
	"name":{"type":"string","maxLength":200},
	"gender_age":{"type":"string","enum":["adult_male","adult_female","boy","girl","elder_male","elder_female","creature","unknown"]},
	"evidence":{"type":"string","maxLength":1000},
	"confidence":{"type":"number","minimum":0,"maximum":1}
},"required":["profile_id","speaker_key","name","gender_age","confidence"]}`)
