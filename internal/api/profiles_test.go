package api

// The profile revision lifecycle on the wire: the optimistic token, the state
// machine, the publication gate and the rows that hang off a revision
// (docs/spec/02 "Transaction workflows", ADR-0015, ADR-0017 requirements 1 and 5).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// newDraft imports a script into a fresh game/edition/draft and returns the
// revision id plus its current token — the state almost every assertion below
// needs a step or two before it.
func newDraft(t *testing.T, e *catalogEnv, slug, key string) (gameID, editionID, profileID, etag string) {
	t.Helper()
	game := e.createGame(auth.RoleCurator, slug, "Draft for "+slug, key)
	gameID = game["id"].(string)
	edition := e.createEdition(auth.RoleCurator, gameID, "us", "en", "rev1", key+"-ed")
	editionID = edition["id"].(string)
	profile, etag := e.createDraft(auth.RoleCurator, editionID)
	profileID = profile["id"].(string)
	if _, job := e.importCSV(auth.RoleCurator, profileID); job["state"] != "succeeded" {
		t.Fatalf("the fixture import did not succeed: %v", job)
	}
	return gameID, editionID, profileID, e.etag(profileID)
}

// TestETagTokenIsTheRowVersion checks the representation on both sides of the
// wire: quoted decimal on the way out (RFC 9110's strong validator), and nothing
// else accepted on the way in.
func TestETagTokenIsTheRowVersion(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "etag", "etag")

	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("ETag = %s, want a quoted string", etag)
	}
	inner := strings.Trim(etag, `"`)
	if _, err := fmt.Sscanf(inner, "%d", new(int)); err != nil || strings.HasPrefix(inner, "0") {
		t.Fatalf("ETag = %s, want a decimal row version with no padding", etag)
	}
	// The token is the row version, so it must be small and it must move with the
	// number of writes — a content hash or a timestamp would be a different
	// contract and would fail this in a way nothing else catches.
	if err := e.st.SetProfileStatus(e.ctx, profileID, games.StatusReview, mustAtoi(inner)); err != nil {
		t.Fatalf("SetProfileStatus at the token the API handed out: %v", err)
	}
	if moved := e.etag(profileID); moved == etag {
		t.Fatalf("a write through the store left the token at %s", moved)
	}
}

func TestIfMatchAcceptanceAndRefusal(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, current := newDraft(t, e, "ifmatch", "ifmatch")
	target := "/api/v1/profiles/" + profileID + "/status"
	payload := `{"to":"archived"}` // legal from review, which this revision is not yet

	cases := []struct {
		name       string
		header     string
		wantStatus int
		wantCode   string
	}{
		{"absent", "", http.StatusPreconditionRequired, codeRevisionConflict},
		{"empty string", `""`, http.StatusBadRequest, codeInvalidParameter},
		// `*` gets the same code as a mismatch on purpose: the client's recovery is
		// identical (re-read the revision, then decide), and the detail names the rule.
		{"wildcard", "*", http.StatusBadRequest, codeRevisionConflict},
		{"weak validator", `W/` + current, http.StatusBadRequest, codeInvalidParameter},
		{"unquoted", strings.Trim(current, `"`), http.StatusBadRequest, codeInvalidParameter},
		{"not a number", `"one"`, http.StatusBadRequest, codeInvalidParameter},
		{"zero", `"0"`, http.StatusBadRequest, codeInvalidParameter},
		{"negative", `"-3"`, http.StatusBadRequest, codeInvalidParameter},
		{"stale but well formed", `"9001"`, http.StatusPreconditionFailed, codeRevisionConflict},
		{"list with no match", `"11", "9001"`, http.StatusPreconditionFailed, codeRevisionConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kv := []string{}
			if tc.header != "" {
				kv = append(kv, "If-Match", tc.header)
			}
			rec := e.post(auth.RoleCurator, target, payload, kv...)
			requireProblem(t, rec, tc.wantStatus, tc.wantCode)
			if tc.wantStatus == http.StatusPreconditionFailed {
				if got := rec.Header().Get("ETag"); got != current {
					t.Errorf("a 412 must carry the current token so the client can recover, got %q want %s", got, current)
				}
			}
		})
	}

	// A well-formed list that contains the current token satisfies the
	// precondition (RFC 9110 8.8.3.1), even when it names tokens that are stale.
	if rec := e.post(auth.RoleCurator, target, `{"to":"review"}`,
		"If-Match", `"4", `+current); rec.Code != http.StatusOK {
		t.Errorf("a matching validator in a list = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestLifecycleMovesAndTheOnesThatAreNotMoves walks docs/spec/02's state machine
// as the API exposes it. `to: published` is the interesting refusal: it is a
// *legal transition*, and the route still refuses it, because publication is the
// validated, digested, audited act rather than a column write.
func TestLifecycleMovesAndTheOnesThatAreNotMoves(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "lifecycle", "lifecycle")
	target := "/api/v1/profiles/" + profileID + "/status"

	requireProblem(t, e.post(auth.RoleCurator, target, `{"to":"published"}`, "If-Match", etag),
		http.StatusConflict, codeTransitionInvalid)
	requireProblem(t, e.post(auth.RoleCurator, target, `{"to":"archived"}`, "If-Match", etag),
		http.StatusConflict, codeTransitionInvalid) // draft → archived is not an edge
	requireProblem(t, e.post(auth.RoleCurator, target, `{"to":"nonsense"}`, "If-Match", etag),
		http.StatusBadRequest, codeInvalidParameter)
	requireProblem(t, e.post(auth.RoleCurator, target, `{}`, "If-Match", etag),
		http.StatusBadRequest, codeInvalidParameter)

	got := e.requireStatus(e.post(auth.RoleCurator, target, `{"to":"review"}`, "If-Match", etag),
		http.StatusOK, "draft → review")
	if got["status"] != "review" {
		t.Fatalf("body = %v", got)
	}
	if next, _ := got["next_states"].([]any); len(next) == 0 {
		t.Error("the response names no reachable states, so a UI cannot render the allowed moves")
	}
	etag = e.etag(profileID)
	back := e.requireStatus(e.post(auth.RoleCurator, target, `{"to":"draft"}`, "If-Match", etag),
		http.StatusOK, "review → draft")
	if back["status"] != "draft" {
		t.Fatalf("review → draft gave %v", back)
	}
}

// TestPublicationGateListsEveryProblem: the gate's value to a review screen is
// that it reports all of what is wrong in one round trip (docs/spec/03), and the
// two refusal codes are distinct because only one of them is a rights decision a
// curator cannot fix by editing text.
func TestPublicationGateListsEveryProblem(t *testing.T) {
	e := newCatalogEnv(t)

	// (a) a draft with nothing in it: the status finding alone.
	game := e.createGame(auth.RoleCurator, "gate-empty", "Gate Empty", "gate-empty")
	edition := e.createEdition(auth.RoleCurator, game["id"].(string), "us", "en", "rev1", "gate-empty-ed")
	empty, emptyETag := e.createDraft(auth.RoleCurator, edition["id"].(string))
	emptyID := empty["id"].(string)

	rec := e.post(auth.RoleCurator, "/api/v1/profiles/"+emptyID+"/publish", "", "If-Match", emptyETag)
	problem := requireProblem(t, rec, http.StatusUnprocessableEntity, codeValidationFailed)
	if len(problem.Findings) == 0 {
		t.Fatal("a 422 from the publication gate carried no findings")
	}
	if !strings.Contains(strings.Join(problem.Findings, "\n"), "status") {
		t.Errorf("the findings do not name the status problem: %v", problem.Findings)
	}
	// A refusal is recorded even though nothing changed: docs/spec/16's evidence
	// bundle has to answer "was this ever offered, and why not" from the database.
	actions := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+emptyID+"/publish-actions")
	list, _ := actions["publish_actions"].([]any)
	if len(list) == 0 {
		t.Fatal("the refused publication left no audit entry")
	}
	first, _ := list[0].(map[string]any)
	if first["result"] != "rejected" || first["action"] != "rejected" {
		t.Errorf("the audit entry reads %v, want a rejected decision", first)
	}
	if probs, _ := first["problems"].([]any); len(probs) == 0 {
		t.Errorf("the rejected entry carries no problems: %v", first)
	}

	// (b) a revision whose only source has an unresolved rights position. The
	// import route refuses to accept rights=unknown at the door
	// (JOB_REQUIRES_INPUT), so this row arrives the way it would in real life: from
	// a revision assembled elsewhere and shipped to this appliance.
	_, _, profileID, etag := newDraft(t, e, "gate-rights", "gate-rights")
	seedUnlicensedSource(t, e, profileID)
	etag = e.etag(profileID)
	if rec := e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/status", `{"to":"review"}`,
		"If-Match", etag); rec.Code != http.StatusOK {
		t.Fatalf("draft → review = %d (body %s)", rec.Code, rec.Body.String())
	}
	rights := e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/publish", "",
		"If-Match", e.etag(profileID))
	problem = requireProblem(t, rights, http.StatusUnprocessableEntity, codeRightsReview)
	if !strings.Contains(strings.Join(problem.Findings, "\n"), "rights_status") {
		t.Errorf("the rights refusal did not name rights_status: %v", problem.Findings)
	}
	// The gate did not merely refuse: it said so in the audit trail.
	after := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/publish-actions")
	if list, _ := after["publish_actions"].([]any); len(list) == 0 {
		t.Error("the rights refusal left no audit entry")
	}
	// And it becomes publishable once the curator settles the rights position —
	// which is what makes the refusal recoverable rather than a dead end. Editing
	// the *text* cannot get past it, so the fix has to be the provenance decision
	// the code names.
	failedEdit := e.patch(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/messages/"+
		mustList(e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages?kind=dialogue")["messages"])[0].(map[string]any)["id"].(string),
		`{"speech_text":"reworded in the hope of shipping"}`, "If-Match", e.etag(profileID))
	if failedEdit.Code != http.StatusOK {
		t.Fatalf("the curator could not even edit the draft: %d %s", failedEdit.Code, failedEdit.Body.String())
	}
	if still := e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/publish", "",
		"If-Match", e.etag(profileID)); still.Code != http.StatusUnprocessableEntity {
		t.Errorf("rewording got past the rights gate (%d): the gate is not about the text", still.Code)
	} else {
		requireProblem(t, still, http.StatusUnprocessableEntity, codeRightsReview)
	}
	licenseTheLineAgain(t, e, profileID)
	if rec := e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/publish", "",
		"If-Match", e.etag(profileID)); rec.Code != http.StatusOK {
		t.Fatalf("publishing after the rights answer = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestPublishedRevisionIsImmutableButDerivable is the whole point of ADR-0015
// stated as four HTTP assertions.
func TestPublishedRevisionIsImmutableButDerivable(t *testing.T) {
	e := newCatalogEnv(t)
	_, editionID, profileID, etag := newDraft(t, e, "immutable", "immutable")

	// Nothing is published yet, and that is a 404 rather than a null body: a
	// session must refuse to start rather than improvise (docs/spec/13).
	requireProblem(t, e.get(auth.RoleViewer, "/api/v1/editions/"+editionID+"/published"),
		http.StatusNotFound, codeNotFound)

	requireProblem(t, e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/publish", "", "If-Match", etag),
		http.StatusUnprocessableEntity, codeValidationFailed)
	e.requireStatus(e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/status", `{"to":"review"}`,
		"If-Match", etag), http.StatusOK, "draft → review")

	published := e.requireStatus(
		e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/publish", "",
			"If-Match", e.etag(profileID)), http.StatusOK, "publish")
	if published["next_states"] == nil {
		t.Error("a published revision names no next states")
	}
	msgs := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages?limit=1")
	first, _ := msgs["messages"].([]any)
	msg, _ := first[0].(map[string]any)
	mid := msg["id"].(string)

	for _, write := range []struct{ method, target, payload string }{
		{http.MethodPatch, "/api/v1/profiles/" + profileID + "/messages/" + mid, `{"speech_text":"frozen"}`},
		{http.MethodPost, "/api/v1/profiles/" + profileID + "/import", ""},
	} {
		rec := e.send(auth.RoleCurator, write.method, write.target, strings.NewReader(write.payload),
			"Content-Type", ContentTypeJSON, "If-Match", e.etag(profileID))
		if write.method == http.MethodPost {
			// The import route has no If-Match: the immutability is its own check.
			rec = e.post(auth.RoleCurator, write.target, "")
		}
		requireProblem(t, rec, http.StatusConflict, codeImmutable)
	}

	child := e.requireStatus(e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/revisions", ""),
		http.StatusCreated, "derive")
	if child["parent_revision"] != published["revision_num"] {
		t.Errorf("the derived revision names parent %v, want %v", child["parent_revision"], published["revision_num"])
	}
	// Deriving from a *draft* is also legal, and deriving twice from the same
	// published revision is how two curators branch without colliding.
	if rec := e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/revisions", ""); rec.Code != http.StatusCreated {
		t.Errorf("a second derivation = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	requireProblem(t, e.post(auth.RoleCurator, "/api/v1/profiles/"+games.NewID()+"/revisions", ""),
		http.StatusNotFound, codeNotFound)
}

// TestProfileAndEditionReads covers the list-shaped reads whose contract is that
// they are always arrays, never null: a generated client typed as []Profile must
// not receive null (docs/spec/08's "one contract per shape").
func TestProfileAndEditionReads(t *testing.T) {
	e := newCatalogEnv(t)
	game := e.createGame(auth.RoleCurator, "reads", "Reads", "reads")
	gameID := game["id"].(string)
	edition := e.createEdition(auth.RoleCurator, gameID, "us", "en", "rev1", "reads-ed")
	editionID := edition["id"].(string)

	empty := e.mustOK(auth.RoleViewer, "/api/v1/editions/"+editionID+"/profiles")
	if profiles, ok := empty["profiles"].([]any); !ok || len(profiles) != 0 {
		t.Fatalf("an edition with no revisions read as %v", empty)
	}
	first, _ := e.createDraft(auth.RoleCurator, editionID)
	second := e.requireStatus(e.post(auth.RoleCurator,
		"/api/v1/profiles/"+first["id"].(string)+"/revisions", ""), http.StatusCreated, "derive")

	listed := e.mustOK(auth.RoleViewer, "/api/v1/editions/"+editionID+"/profiles")
	profiles, _ := listed["profiles"].([]any)
	if len(profiles) != 2 {
		t.Fatalf("the edition lists %v revisions, want 2", listed["profiles"])
	}
	if a, _ := profiles[0].(map[string]any); a["revision_num"] != float64(1) {
		t.Errorf("revisions are not ordered lowest-first: %v", profiles)
	}
	if b, _ := profiles[1].(map[string]any); b["id"] != second["id"] {
		t.Errorf("the second entry is %v, want the derived revision", profiles[1])
	}

	// A profile's characters and regions are arrays too, and the DTO renders a
	// JSON column that arrived empty as [] / {} rather than null.
	chars := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+first["id"].(string)+"/characters")
	if got, ok := chars["characters"].([]any); !ok || len(got) != 0 {
		t.Fatalf("characters read as %v", chars)
	}
	regions := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+first["id"].(string)+"/regions")
	if got, ok := regions["regions"].([]any); !ok || len(got) != 0 {
		t.Fatalf("regions read as %v", regions)
	}

	// Seed one of each through the repository — the route the agent milestone
	// writes them by — and check the wire shape of a populated row, including the
	// nullable numbers the generated client has to type as number | null.
	seedCharacterAndRegion(t, e, first["id"].(string))
	filled := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+first["id"].(string)+"/characters")
	one, _ := filled["characters"].([]any)
	if len(one) != 1 {
		t.Fatalf("the seeded character did not appear: %v", filled["characters"])
	}
	c, _ := one[0].(map[string]any)
	for _, member := range []string{"aliases", "visual_descriptors", "verified_source_refs"} {
		if _, ok := c[member].([]any); !ok {
			t.Errorf("character.%s = %v, want an array", member, c[member])
		}
	}
	if c["name"] != "Marla" || c["status"] != "confirmed" {
		t.Errorf("the character DTO lost a field: %v", c)
	}
	filledRegions := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+first["id"].(string)+"/regions")
	r, _ := filledRegions["regions"].([]any)
	if len(r) != 1 {
		t.Fatalf("the seeded region did not appear: %v", filledRegions["regions"])
	}
	rg, _ := r[0].(map[string]any)
	if rg["region_type"] != "textbox" || rg["detect_method"] != "imported" {
		t.Errorf("the region DTO lost an enum: %v", rg)
	}
	if _, ok := rg["ocr_settings"].(map[string]any); !ok {
		t.Errorf("region.ocr_settings = %v, want an object", rg["ocr_settings"])
	}
	if minAspect, ok := rg["min_aspect"].(float64); !ok || minAspect != 2.0 {
		t.Errorf("region.min_aspect = %v, want 2", rg["min_aspect"])
	}
	if maxAspect := rg["max_aspect"]; maxAspect != nil {
		t.Errorf("region.max_aspect = %v, want JSON null for an unset bound", maxAspect)
	}

	// Reading a revision that does not exist is a 404 on every route under it.
	for _, suffix := range []string{"", "/messages", "/messages/search?q=x", "/characters", "/regions", "/publish-actions", "/export"} {
		requireProblem(t, e.get(auth.RoleViewer, "/api/v1/profiles/"+games.NewID()+suffix),
			http.StatusNotFound, codeNotFound)
	}
}

// ── seeding through the repositories ───────────────────────────────────────
//
// These write content rows the way internal/store does, because no route in
// Milestone 1 creates a character, a region or a source whose rights position is
// unresolved: that work belongs to the agent and review-queue milestones. A test
// that faked the rows in memory instead would not exercise the CHECK constraints
// or the DTO mapping, which is exactly what is being asserted here.

func seedCharacterAndRegion(t *testing.T, e *catalogEnv, profileID string) {
	t.Helper()
	ctx := e.ctx
	character := games.Character{
		ID: games.NewID(), ProfileID: profileID, Name: "Marla",
		// An array of objects, which is what docs/spec/04's visual profile says a
		// descriptor is; the store only demands json_valid, and the contract is what
		// keeps the two appliances' UIs from disagreeing about the shape.
		Aliases:            json.RawMessage(`["harbourmaster"]`),
		VisualDescriptors:  json.RawMessage(`[{"trait":"tall"},{"trait":"oilskin coat"}]`),
		PersonalitySummary: "Counts crates.", Role: "harbourmaster",
		VerifiedSourceRefs: json.RawMessage(`[]`), Status: games.CharacterConfirmed, CreatedAt: nowStamp(),
	}
	if err := e.st.PutCharacters(ctx, profileID, []games.Character{character}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}
	region := games.Region{
		ID: games.NewID(), ProfileID: profileID, Label: "bottom textbox",
		RegionType: games.RegionTextbox, DetectMethod: games.DetectImported,
		RectX: 0.05, RectY: 0.72, RectW: 0.9, RectH: 0.2,
		MinAspect: ptr(2.0), OCRSettings: json.RawMessage(`{"threshold":0.6}`),
		MaskAssetIDs: json.RawMessage(`[]`), Revision: 1, UpdatedAt: nowStamp(),
	}
	if err := e.st.PutRegions(ctx, profileID, []games.Region{region}); err != nil {
		t.Fatalf("PutRegions: %v", err)
	}
}

func seedUnlicensedSource(t *testing.T, e *catalogEnv, profileID string) {
	t.Helper()
	ctx := e.ctx
	// An external scrape whose rights position was never resolved — the shape the
	// publication gate exists to stop. (games.Source.Validate refuses
	// rights=unknown for a file:// the user handed over, so a local file cannot
	// honestly carry an unresolved answer.)
	source := games.Source{
		ID: games.NewID(), ProfileID: profileID, SourceType: games.SourceWebPage,
		URI: "https://scripts.invalid/raw-dump", RetrievedAt: nowStamp(),
		ContentSHA256: strings.Repeat("1", 64), MimeType: "text/plain",
		Rights: games.RightsUnknown, Trust: 0.4, Parser: "generic@1",
		CoverageNotes: "planted by a test to exercise the rights gate",
	}
	if err := e.st.PutSources(ctx, profileID, []games.Source{source}); err != nil {
		t.Fatalf("PutSources: %v", err)
	}
	msg := games.Message{
		ID: games.NewID(), ProfileID: profileID, SourceID: source.ID,
		SourceMessageID: "scraper-row-1", OriginalText: "Scraped line of unknown provenance.",
		MarkupJSON: json.RawMessage(`[]`), NormalizedText: "scraped line of unknown provenance",
		SpeechText: "Scraped line of unknown provenance.", Language: "en",
		TextKind: games.TextDialogue, AttributionStatus: games.AttributionUnknown,
		AttributionEvidence: json.RawMessage(`[]`), Flags: json.RawMessage(`{}`),
		Revision: 1, CreatedAt: nowStamp(), UpdatedAt: nowStamp(),
	}
	if err := e.st.PutMessages(ctx, profileID, []games.Message{msg}, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
}

// licenseTheLineAgain is the curator's decision, written the only way this
// storage allows: provenance rows are immutable, so the correction is a *new*
// source with a settled rights position and the line re-pointed at it — exactly
// what profileio's own re-import warning tells the reviewer to do ("correct it by
// registering a new source"). The old, unresolved row stays in the revision as the
// record that it once existed, and stops gating publication only because no
// message still hangs off it.
func licenseTheLineAgain(t *testing.T, e *catalogEnv, profileID string) {
	t.Helper()
	sources, err := e.st.ListSources(e.ctx, profileID)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	var unresolved *games.Source
	for i := range sources {
		if sources[i].Rights == games.RightsUnknown {
			unresolved = &sources[i]
		}
	}
	if unresolved == nil {
		t.Fatal("there is no unresolved source to correct: the fixture stopped testing what it claims to")
	}
	replacement := games.Source{
		ID: games.NewID(), ProfileID: profileID, SourceType: games.SourceUserFile,
		URI: "file:///invented/licensed-script.csv", RetrievedAt: nowStamp(),
		ContentSHA256: strings.Repeat("2", 64), MimeType: "text/csv",
		Rights: games.RightsLicensed, Trust: 1, Parser: "generic@1",
		CoverageNotes: "the curator's second look at the same lines",
	}
	if err := e.st.PutSources(e.ctx, profileID, []games.Source{replacement}); err != nil {
		t.Fatalf("registering the corrected source: %v", err)
	}
	msgs, _, err := e.st.ListMessages(e.ctx, profileID, store.MessageFilter{}, "", store.MaxListLimit)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var moved []games.Message
	for _, m := range msgs {
		if m.SourceID == unresolved.ID {
			m.SourceID = replacement.ID
			moved = append(moved, m)
		}
	}
	if len(moved) == 0 {
		t.Fatal("no message hangs off the unresolved source: the fixture stopped testing what it claims to")
	}
	if err := e.st.PutMessages(e.ctx, profileID, moved, nil); err != nil {
		t.Fatalf("re-pointing the line: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func mustAtoi(s string) int64 {
	var v int64
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		v = v*10 + int64(r-'0')
	}
	return v
}

// TestEveryCatalogOperationWasExercised makes clause (c) a coverage claim rather
// than a hope: each operation this milestone added must have had a real success
// body validated at least once. A route that no test reached fails here even if
// the document describes it perfectly.
func TestEveryCatalogOperationWasExercised(t *testing.T) {
	// The flows live in catalog_test.go and importexport_test.go; running only
	// one of those files with -run would report the others as missing, so the
	// dependency is stated rather than hidden.
	seen := map[string]bool{}
	contractMu.Lock()
	for _, hit := range contractHits {
		seen[hit.op] = true
	}
	contractMu.Unlock()
	if len(seen) == 0 {
		t.Skip("no catalog flow ran in this invocation (go test -run selects a subset)")
	}

	a := newTestAuth(t)
	s := New(testBuild, testStarted, discardLogger, WithWebAssets(uiBuild()), WithAuth(a.svc, a.repo))
	s.Handler()

	for _, rt := range s.routes {
		if !milestoneCatalogRoute(rt.method, rt.path) {
			continue
		}
		key := rt.method + " " + rt.path
		if !seen[key] {
			t.Errorf("operation %s produced no validated success body in these tests: the contract covers what the handlers did, and nothing exercised it", key)
		}
	}
}

// milestoneCatalogRoute selects the operations ADR-0017 added.
func milestoneCatalogRoute(method, path string) bool {
	switch {
	case strings.HasPrefix(path, "/api/v1/games"),
		strings.HasPrefix(path, "/api/v1/editions"),
		strings.HasPrefix(path, "/api/v1/profiles"),
		path == openAPIPath:
		return true
	}
	_ = method
	return false
}
