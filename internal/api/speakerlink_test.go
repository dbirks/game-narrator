package api

// The curator's speaker-link action (ADR-0070): the identification of an
// import's speaker placeholder with a real character. The happy path, the
// retired-placeholder refusal, the unknown-target 404 and the draft-only
// 409 are the four outcomes the ADR names; the assertions read the rows back
// through the API and the store, so the wire and the database cannot disagree.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
)

// seedSpeakerPlaceholder plants exactly what an opaque-speaker import leaves
// behind: a proposed placeholder named by the source's own opaque key, a
// confirmed character to link to, and three lines pointing at the placeholder
// with the parser's proposed source-field evidence. The texts are invented
// (AGENTS.md rule 5): nothing here is game material.
func seedSpeakerPlaceholder(t *testing.T, e *catalogEnv, profileID string) (placeholderID, targetID string) {
	t.Helper()
	placeholderID = games.NewID()
	targetID = games.NewID()
	placeholder := games.Character{
		ID:                 placeholderID,
		ProfileID:          profileID,
		Name:               "ww:spk:7",
		Aliases:            json.RawMessage(`["ww:spk:7"]`),
		VerifiedSourceRefs: json.RawMessage(`[]`),
		Status:             games.CharacterProposed,
		CreatedAt:          nowStamp(),
	}
	target := games.Character{
		ID:                 targetID,
		ProfileID:          profileID,
		Name:               "Marla",
		Aliases:            json.RawMessage(`["harbourmaster"]`),
		VerifiedSourceRefs: json.RawMessage(`[]`),
		Status:             games.CharacterConfirmed,
		CreatedAt:          nowStamp(),
	}
	if err := e.st.PutCharacters(e.ctx, profileID, []games.Character{placeholder, target}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}
	evidence := json.RawMessage(`[{"method":"source_field","field":"inf1:attribute_0","key":"ww:spk:7","posterior":1.0}]`)
	msgs := make([]games.Message, 0, 3)
	for _, line := range []string{
		"invented line one of the placeholder",
		"invented line two of the placeholder",
		"invented line three of the placeholder",
	} {
		msgs = append(msgs, games.Message{
			ID:                    games.NewID(),
			ProfileID:             profileID,
			OriginalText:          line,
			MarkupJSON:            json.RawMessage(`[]`),
			NormalizedText:        line,
			SpeechText:            line,
			Language:              "en",
			TextKind:              games.TextDialogue,
			SpeakerID:             placeholderID,
			AttributionStatus:     games.AttributionProposed,
			AttributionConfidence: 1.0,
			AttributionEvidence:   evidence,
			Flags:                 json.RawMessage(`{}`),
			Revision:              1,
			CreatedAt:             nowStamp(),
			UpdatedAt:             nowStamp(),
		})
	}
	if err := e.st.PutMessages(e.ctx, profileID, msgs, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	return placeholderID, targetID
}

// postSpeakerLink is the curator's action on the wire.
func postSpeakerLink(t *testing.T, e *catalogEnv, profileID, placeholderID, targetID, etag string) (map[string]any, string) {
	t.Helper()
	target := fmt.Sprintf("/api/v1/profiles/%s/characters/%s/speaker-link", profileID, placeholderID)
	body := fmt.Sprintf(`{"target_character_id":%q}`, targetID)
	rec := e.post(auth.RoleCurator, target, body, "If-Match", etag)
	out := e.requireStatus(rec, http.StatusOK, "POST …/characters/{cid}/speaker-link")
	return out, rec.Header().Get("ETag")
}

func TestSpeakerLinkHappyPath(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "speaker-link", "sl")
	placeholderID, targetID := seedSpeakerPlaceholder(t, e, profileID)
	etag := e.etag(profileID)

	out, etagHeader := postSpeakerLink(t, e, profileID, placeholderID, targetID, etag)
	if out["profile_id"] != profileID || out["source_character_id"] != placeholderID ||
		out["target_character_id"] != targetID {
		t.Errorf("response ids = %v / %v / %v, want the revision and the two characters",
			out["profile_id"], out["source_character_id"], out["target_character_id"])
	}
	if n, ok := out["repointed_messages"].(float64); !ok || int(n) != 3 {
		t.Errorf("repointed_messages = %v, want the three planted lines", out["repointed_messages"])
	}
	if etagHeader == "" || etagHeader == etag {
		t.Errorf("ETag = %q, want the revision's moved token (the write moved the row)", etagHeader)
	}

	// The lines: every one now points at the target, confirmed at 1.0, and the
	// source-field evidence survives with the curator added as a reviewer entry.
	got := e.mustOK(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/messages?speaker="+targetID)
	msgs := mustList(got["messages"])
	if len(msgs) != 3 {
		t.Fatalf("the target has %d lines, want the three the link moved", len(msgs))
	}
	for _, raw := range msgs {
		m := raw.(map[string]any)
		if m["speaker_id"] != targetID {
			t.Errorf("speaker_id = %v, want the target", m["speaker_id"])
		}
		if m["attribution_status"] != "confirmed" {
			t.Errorf("attribution_status = %v, want confirmed", m["attribution_status"])
		}
		if conf, ok := m["attribution_confidence"].(float64); !ok || conf != 1.0 {
			t.Errorf("attribution_confidence = %v, want 1.0", m["attribution_confidence"])
		}
		var evidence []map[string]any
		if ev, ok := m["attribution_evidence"]; ok {
			_ = json.Unmarshal(jsonField(ev), &evidence)
		}
		var sourceField, reviewer bool
		for _, en := range evidence {
			if en["method"] == "source_field" {
				sourceField = true
			}
			if en["method"] == "manual" && en["reviewer"] == "curator" {
				reviewer = true
			}
		}
		if !sourceField || !reviewer {
			t.Errorf("evidence = %v, want the source_field entry kept and a manual entry naming the curator", evidence)
		}
	}
	// Nothing points at the retired placeholder any more.
	after := e.mustOK(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/messages?speaker="+placeholderID)
	if n := len(mustList(after["messages"])); n != 0 {
		t.Errorf("%d lines still point at the placeholder, want none", n)
	}

	// The roster: the placeholder is retired, the target carries the key as an
	// alias so a re-import binds the key straight to the target.
	chars := mustList(e.mustOK(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/characters")["characters"])
	var placeholder, target map[string]any
	for _, raw := range chars {
		c := raw.(map[string]any)
		switch c["id"] {
		case placeholderID:
			placeholder = c
		case targetID:
			target = c
		}
	}
	if placeholder == nil || target == nil {
		t.Fatalf("the roster lost a character: %v", chars)
	}
	if placeholder["status"] != "retired" {
		t.Errorf("placeholder status = %v, want retired", placeholder["status"])
	}
	var aliases []string
	_ = json.Unmarshal(jsonField(target["aliases"]), &aliases)
	found := false
	for _, a := range aliases {
		if a == "ww:spk:7" {
			found = true
		}
	}
	if !found {
		t.Errorf("target aliases = %v, want the placeholder's key folded in", aliases)
	}

	// The audit trail: one manual assertion per moved line, the curator named.
	assertions, err := e.st.ListSpeakerAssertions(e.ctx, profileID)
	if err != nil {
		t.Fatalf("ListSpeakerAssertions: %v", err)
	}
	manual := 0
	for _, a := range assertions {
		if a.Method == games.AssertionManual && a.CharacterID == targetID && a.Reviewer == "curator" {
			manual++
		}
	}
	if manual != 3 {
		t.Errorf("%d manual assertions for the target, want one per moved line", manual)
	}
}

// jsonField renders a decoded JSON-array field the way the DTOs carry it:
// the RawMessage columns come back as []byte in a map[string]any, and
// marshalling the rest keeps one path for both shapes.
func jsonField(v any) []byte {
	switch t := v.(type) {
	case []byte:
		return t
	case string:
		return []byte(t)
	}
	b, _ := json.Marshal(v)
	return b
}

func TestSpeakerLinkRefusals(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "speaker-link-refuse", "slr")
	placeholderID, targetID := seedSpeakerPlaceholder(t, e, profileID)
	etag := e.etag(profileID)
	target := fmt.Sprintf("/api/v1/profiles/%s/characters/%s/speaker-link", profileID, placeholderID)

	// A retired placeholder has already been identified: the second link is a
	// conflict, not a no-op.
	postSpeakerLink(t, e, profileID, placeholderID, targetID, etag)
	etag = e.etag(profileID)
	rec := e.post(auth.RoleCurator, target, fmt.Sprintf(`{"target_character_id":%q}`, targetID), "If-Match", etag)
	e.requireStatus(rec, http.StatusConflict, "POST link on a retired placeholder")
	requireProblem(t, rec, http.StatusConflict, codeConflict)

	// A target the revision does not have is a 404, the same shape the reads
	// answer with.
	etag = e.etag(profileID)
	rec = e.post(auth.RoleCurator, target, fmt.Sprintf(`{"target_character_id":%q}`, games.NewID()), "If-Match", etag)
	e.requireStatus(rec, http.StatusNotFound, "POST link to an unknown target")

	// A self-link and a missing target are 400s: malformed calls, not state.
	etag = e.etag(profileID)
	rec = e.post(auth.RoleCurator, target, fmt.Sprintf(`{"target_character_id":%q}`, placeholderID), "If-Match", etag)
	e.requireStatus(rec, http.StatusBadRequest, "POST a self-link")
	etag = e.etag(profileID)
	rec = e.post(auth.RoleCurator, target, `{}`, "If-Match", etag)
	e.requireStatus(rec, http.StatusBadRequest, "POST a missing target")

	// Draft-only: a review revision is frozen for curation, and the refusal
	// says the recovery.
	etag = e.etag(profileID)
	if err := e.st.SetProfileStatus(e.ctx, profileID, games.StatusReview, mustAtoi(strings.Trim(etag, `"`))); err != nil {
		t.Fatalf("SetProfileStatus: %v", err)
	}
	etag = e.etag(profileID)
	rec = e.post(auth.RoleCurator, target, fmt.Sprintf(`{"target_character_id":%q}`, targetID), "If-Match", etag)
	e.requireStatus(rec, http.StatusConflict, "POST a link on a review revision")
	requireProblem(t, rec, http.StatusConflict, codeConflict)
}
