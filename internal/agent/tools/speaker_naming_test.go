package tools

// Unit tests for the speaker-naming pair (ADR-0187): list_unnamed_speakers
// surfaces an import's opaque speaker placeholders with sample lines, and
// name_speaker renames one placeholder on the draft with the voice class and
// the naming provenance in its summary (the cast audit's expected-class
// input). Invented speakers and lines only (AGENTS.md test category (a));
// no game text anywhere.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/cast"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// namingFixture builds a draft profile with two named characters, three
// speaker placeholders (one of them retired) and the lines that carry them.
// It returns the profile and the placeholder ids keyed by their keys.
func (e *env) namingFixture(t *testing.T) (games.Profile, map[string]string) {
	t.Helper()
	g := e.game("synthetic-cove", "Synthetic Cove")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)
	now := time.Now().UTC().Format(time.RFC3339)
	mk := func(name string, retired bool) games.Character {
		status := games.CharacterProposed
		if retired {
			status = games.CharacterRetired
		}
		return games.Character{ID: games.NewID(), ProfileID: p.ID, Name: name,
			Status: status, CreatedAt: now}
	}
	mar := mk("Marla", false)
	k12 := mk("ww:spk:12", false)
	k7 := mk("ww:spk:7", false)
	k3 := mk("ww:spk:3", true) // retired: its identity was linked away
	for _, c := range []games.Character{mar, k12, k7, k3} {
		if err := c.Validate(); err != nil {
			t.Fatalf("fixture character invalid: %v", err)
		}
	}
	lines := []struct {
		speaker string
		text    string
	}{
		{"ww:spk:12", "The tide chart says we sail at dawn, and I do not miss dawn."},
		{"ww:spk:12", "You crew is late again; the net is soaked through."},
		{"ww:spk:12", "Bring the brass lamp; the channel narrows past the headland."},
		{"ww:spk:12", "Count every rope twice before it leaves my hands."},
		{"ww:spk:12", "One more leak in the hull and I am walking this boat."},
		{"ww:spk:7", "I saw a silver fin at the pier this morning."},
		{"Marla", "Good weather today, captain."},
	}
	ids := map[string]string{"Marla": mar.ID, "ww:spk:12": k12.ID, "ww:spk:7": k7.ID}
	if err := e.store.PutCharacters(e.ctx, p.ID, []games.Character{mar, k12, k7, k3}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}
	msgs := make([]games.Message, 0, len(lines))
	for _, l := range lines {
		msgs = append(msgs, games.Message{
			ID:                    games.NewID(),
			ProfileID:             p.ID,
			OriginalText:          l.text,
			NormalizedText:        l.text,
			SpeechText:            l.text,
			TextKind:              games.TextDialogue,
			SpeakerID:             ids[l.speaker],
			AttributionStatus:     games.AttributionProposed,
			AttributionConfidence: 0.9,
			Revision:              1,
			CreatedAt:             now,
			UpdatedAt:             now,
		})
	}
	if err := e.store.PutMessages(e.ctx, p.ID, msgs, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	return p, map[string]string{"ww:spk:12": k12.ID, "ww:spk:7": k7.ID, "Marla": mar.ID}
}

func TestListUnnamedSpeakersListsPlaceholders(t *testing.T) {
	e := newEnv(t)
	defer e.close()
	p, ids := e.namingFixture(t)
	e.tools(&fakeSearch{}, nil)

	res := agent.NewExecutor(e.reg, auth.RoleOperator, 16).Execute(e.ctx, agent.Call{
		ID:        "ls-1",
		Name:      "list_unnamed_speakers",
		Arguments: json.RawMessage(fmt.Sprintf(`{"profile_id":%q}`, p.ID)),
	})
	if res.Status != agent.StatusSucceeded {
		t.Fatalf("list_unnamed_speakers = %s: %s", res.Status, res.Error)
	}
	var out struct {
		Unnamed []struct {
			SpeakerKey  string   `json:"speaker_key"`
			CharacterID string   `json:"character_id"`
			LineCount   int      `json:"line_count"`
			SampleLines []string `json:"sample_lines"`
		} `json:"unnamed_speakers"`
	}
	if err := json.Unmarshal(res.Results, &out); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(out.Unnamed) != 2 {
		t.Fatalf("listed %d placeholders, want 2 (the retired and the named rows are out): %+v", len(out.Unnamed), out.Unnamed)
	}
	byKey := map[string]int{}
	for _, s := range out.Unnamed {
		byKey[s.SpeakerKey] = s.LineCount
		if got := s.CharacterID; got != ids[s.SpeakerKey] {
			t.Errorf("placeholder %s: character_id = %s, want %s", s.SpeakerKey, got, ids[s.SpeakerKey])
		}
		if len(s.SampleLines) > maxSampleLines {
			t.Errorf("placeholder %s: %d sample lines, cap is %d", s.SpeakerKey, len(s.SampleLines), maxSampleLines)
		}
	}
	if byKey["ww:spk:12"] != 5 {
		t.Errorf("ww:spk:12 line count = %d, want 5", byKey["ww:spk:12"])
	}
	if byKey["ww:spk:7"] != 1 {
		t.Errorf("ww:spk:7 line count = %d, want 1", byKey["ww:spk:7"])
	}
}

func TestNameSpeakerRenamesPlaceholderOnDraft(t *testing.T) {
	e := newEnv(t)
	defer e.close()
	p, ids := e.namingFixture(t)
	e.tools(&fakeSearch{}, nil)

	call := agent.Call{
		ID:   "ns-1",
		Name: "name_speaker",
		Arguments: json.RawMessage(fmt.Sprintf(
			`{"profile_id":%q,"speaker_key":"ww:spk:12","name":"Tetra","gender_age":"adult_female","evidence":"issues standing orders and watches the tide chart; crew address her as captain","confidence":0.9}`,
			p.ID)),
	}
	res := agent.NewExecutor(e.reg, auth.RoleOperator, 16).Execute(e.ctx, call)
	if res.Status != agent.StatusSucceeded {
		t.Fatalf("name_speaker = %s: %s", res.Status, res.Error)
	}
	chars, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListCharacters: %v", err)
	}
	var named games.Character
	for _, c := range chars {
		if c.ID == ids["ww:spk:12"] {
			named = c
		}
	}
	if named.Name != "Tetra" {
		t.Fatalf("placeholder row name = %q, want Tetra", named.Name)
	}
	if !strings.Contains(string(named.Aliases), "ww:spk:12") {
		t.Errorf("key not folded into aliases: %s", named.Aliases)
	}
	if !strings.Contains(named.PersonalitySummary, "ww:spk:12") ||
		!strings.Contains(named.PersonalitySummary, "0.90") {
		t.Errorf("provenance missing from summary: %q", named.PersonalitySummary)
	}
	// The cast audit reads summary+role as its expected-class text; the
	// naming run's gender_age must be what the audit classifies on.
	if got := cast.ExpectedClass(named.PersonalitySummary + " " + named.Role); got != cast.ClassFemale {
		t.Errorf("cast audit expected class = %q, want %q (summary: %q)", got, cast.ClassFemale, named.PersonalitySummary)
	}
	// The lines still point at the row: nothing was re-pointed.
	msgs, _, err := e.store.ListMessages(e.ctx, p.ID, store.MessageFilter{SpeakerID: ids["ww:spk:12"]}, "", 10)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 5 {
		t.Errorf("placeholder lines = %d, want 5 (rename must keep them)", len(msgs))
	}
	// Draft only: the profile did not move.
	fresh, _, err := e.store.GetProfile(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if fresh.Status != games.StatusDraft {
		t.Fatalf("profile status = %s, want draft (name_speaker is a draft write)", fresh.Status)
	}
}

func TestNameSpeakerIdempotentRetry(t *testing.T) {
	e := newEnv(t)
	defer e.close()
	p, _ := e.namingFixture(t)
	e.tools(&fakeSearch{}, nil)
	exec := agent.NewExecutor(e.reg, auth.RoleOperator, 16)
	args := json.RawMessage(fmt.Sprintf(
		`{"profile_id":%q,"speaker_key":"ww:spk:7","name":"Pim","gender_age":"boy","evidence":"talks about fish and hides at sea","confidence":0.8}`,
		p.ID))
	for i := 0; i < 2; i++ {
		res := exec.Execute(e.ctx, agent.Call{ID: fmt.Sprintf("retry-%d", i), Name: "name_speaker", Arguments: args})
		if res.Status != agent.StatusSucceeded {
			t.Fatalf("retry %d: name_speaker = %s: %s", i, res.Status, res.Error)
		}
	}
	chars, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListCharacters: %v", err)
	}
	named := 0
	for _, c := range chars {
		if c.Name == "Pim" {
			named++
			if strings.Count(string(c.Aliases), "ww:spk:7") != 1 {
				t.Errorf("retry duplicated the alias fold: %s", c.Aliases)
			}
		}
	}
	if named != 1 {
		t.Fatalf("named rows = %d, want 1 (retries must not fork the roster)", named)
	}
}

func TestNameSpeakerRefusesNonDraftAndBadArgs(t *testing.T) {
	e := newEnv(t)
	defer e.close()
	p, _ := e.namingFixture(t)
	e.tools(&fakeSearch{}, nil)
	exec := agent.NewExecutor(e.reg, auth.RoleOperator, 16)
	run := func(args string) agent.Result {
		return exec.Execute(e.ctx, agent.Call{ID: "x", Name: "name_speaker", Arguments: json.RawMessage(args)})
	}
	// A review (curation-frozen) revision is refused: draft-only.
	_, rowVersion, err := e.store.GetProfile(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if err := e.store.SetProfileStatus(e.ctx, p.ID, games.StatusReview, rowVersion); err != nil {
		t.Fatalf("SetProfileStatus: %v", err)
	}
	res := run(fmt.Sprintf(`{"profile_id":%q,"speaker_key":"ww:spk:12","name":"Tetra","gender_age":"adult_female","confidence":0.9}`, p.ID))
	if res.Status != agent.StatusFailed {
		t.Fatalf("review profile: name_speaker = %s, want refused (%s)", res.Status, res.Error)
	}
	if !strings.Contains(strings.ToLower(res.Error), "draft") {
		t.Errorf("refusal should name the draft-only rule: %s", res.Error)
	}
	// Back on a draft: the bad-arg matrix.
	if err := e.store.SetProfileStatus(e.ctx, p.ID, games.StatusDraft, rowVersion+1); err != nil {
		t.Fatalf("SetProfileStatus back to draft: %v", err)
	}
	bad := map[string]string{
		"unknown key":        fmt.Sprintf(`{"profile_id":%q,"speaker_key":"nope","name":"Tetra","gender_age":"adult_female","confidence":0.9}`, p.ID),
		"key as name":        fmt.Sprintf(`{"profile_id":%q,"speaker_key":"ww:spk:12","name":"ww:spk:99","gender_age":"adult_female","confidence":0.9}`, p.ID),
		"bad gender":         fmt.Sprintf(`{"profile_id":%q,"speaker_key":"ww:spk:12","name":"Tetra","gender_age":"alien","confidence":0.9}`, p.ID),
		"confidence > 1":     fmt.Sprintf(`{"profile_id":%q,"speaker_key":"ww:spk:12","name":"Tetra","gender_age":"adult_female","confidence":1.4}`, p.ID),
		"missing confidence": fmt.Sprintf(`{"profile_id":%q,"speaker_key":"ww:spk:12","name":"Tetra","gender_age":"adult_female"}`, p.ID),
		"no such key":        fmt.Sprintf(`{"profile_id":%q,"speaker_key":"ww:spk:99","name":"Tetra","gender_age":"adult_female","confidence":0.9}`, p.ID),
	}
	for what, args := range bad {
		res := run(args)
		// refused is the executor's schema-level answer (bad enum, missing
		// arg); failed is the handler's own validation. Either is the answer.
		if res.Status == agent.StatusSucceeded {
			t.Errorf("%s: expected a refusal, got succeeded", what)
		}
	}
}

func TestNameSpeakerCannotSecondGuessANamedRow(t *testing.T) {
	e := newEnv(t)
	defer e.close()
	p, _ := e.namingFixture(t)
	e.tools(&fakeSearch{}, nil)
	exec := agent.NewExecutor(e.reg, auth.RoleOperator, 16)
	first := exec.Execute(e.ctx, agent.Call{
		ID:   "n1",
		Name: "name_speaker",
		Arguments: json.RawMessage(fmt.Sprintf(
			`{"profile_id":%q,"speaker_key":"ww:spk:7","name":"Pim","gender_age":"boy","confidence":0.8}`, p.ID)),
	})
	if first.Status != agent.StatusSucceeded {
		t.Fatalf("first naming = %s: %s", first.Status, first.Error)
	}
	second := exec.Execute(e.ctx, agent.Call{
		ID:   "n2",
		Name: "name_speaker",
		Arguments: json.RawMessage(fmt.Sprintf(
			`{"profile_id":%q,"speaker_key":"ww:spk:7","name":"Gondo","gender_age":"adult_male","confidence":0.7}`, p.ID)),
	})
	if second.Status != agent.StatusFailed {
		t.Fatalf("a different name for the same key must be refused, got %s: %s", second.Status, second.Error)
	}
}

// TestNamingRunLoopIsDraftOnly drives the full naming run shape: the
// scripted model lists the placeholders, names two of them, leaves the rest
// and stops. The run must end succeeded on a draft revision with the
// provenance stored — the review/approve flow is where the names land.
func TestNamingRunLoopIsDraftOnly(t *testing.T) {
	e := newEnv(t)
	defer e.close()
	p, _ := e.namingFixture(t)
	e.tools(&fakeSearch{}, nil)

	model := &scriptedModel{responses: []scriptedResponse{
		callScripted("c1", "list_unnamed_speakers", map[string]any{"profile_id": p.ID}),
		callScripted("c2", "name_speaker", map[string]any{
			"profile_id": p.ID, "speaker_key": "ww:spk:12", "name": "Tetra",
			"gender_age": "adult_female",
			"evidence":   "issues standing orders; the crew address her as captain", "confidence": 0.9,
		}),
		callScripted("c3", "name_speaker", map[string]any{
			"profile_id": p.ID, "speaker_key": "ww:spk:7", "name": "Pim",
			"gender_age": "boy",
			"evidence":   "tall talk about silver fins; hides at sea", "confidence": 0.8,
		}),
		{text: "Two of the three placeholders are named with evidence; the third (Marla's companion) cannot be identified from the sample lines and is left for the curator. The draft is ready for review."},
	}}
	runID := games.NewID()
	out := e.runAgent(t, model, runID, "Name the unnamed speakers on the draft profile "+p.ID+".", 6)
	if out.State != agent.StateSucceeded {
		t.Fatalf("naming run state = %s (stop %s), want succeeded", out.State, out.StopReason)
	}
	chars, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListCharacters: %v", err)
	}
	named := map[string]bool{}
	for _, c := range chars {
		switch c.Name {
		case "Tetra", "Pim":
			named[c.Name] = true
			if !strings.Contains(c.PersonalitySummary, "agent") || !strings.Contains(c.PersonalitySummary, "ww:spk") {
				t.Errorf("%s: provenance missing from summary: %q", c.Name, c.PersonalitySummary)
			}
		}
	}
	if !named["Tetra"] || !named["Pim"] {
		t.Fatalf("naming run left no named rows: %v", named)
	}
	fresh, _, err := e.store.GetProfile(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if fresh.Status != games.StatusDraft {
		t.Fatalf("naming run must end on a draft (review flow approves), status = %s", fresh.Status)
	}
	// Both tool calls are on the record.
	calls := e.toolCallResults(t, runID)
	if len(calls) != 3 {
		t.Errorf("tool-call rows = %d, want 3 (list + two namings)", len(calls))
	}
}
