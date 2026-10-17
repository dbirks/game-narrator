package api

// The documented enum vocabularies are compared against the ones the service
// actually accepts — `games.WalkEnum` for the domain tables, `auth.Ordered` for the
// role ladder — rather than against somebody's memory of them. This is the check
// that keeps a hand-written contract from containing a tidy-looking vocabulary the
// domain has never heard of, which is the failure mode that makes a generated
// client accept what the appliance refuses and reject what it stores.
//
// It caught four such inventions while this document was being written: `narration`,
// `quest`, `hint` and `error` in text_kind; `probable` and `contested` in
// attribution_status; a whole invented detect_method list (`fixed_roi`, `edge`,
// `ocr_anchor`, `adaptive`); and `rejected` for a character, which is `retired`.

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/profileio"
)

// domainEnums maps a documented schema (or a "Schema.properties.member" path) onto
// the Go type name whose values it must contain exactly.
var domainEnums = map[string]string{
	"ProfileStatus":                   "ProfileStatus",
	"RightsStatus":                    "RightsStatus",
	"TextKind":                        "TextKind",
	"AttributionStatus":               "AttributionStatus",
	"JobState":                        "JobState",
	"Region.properties.region_type":   "RegionType",
	"Region.properties.detect_method": "DetectMethod",
	"Character.properties.status":     "CharacterStatus",
	"JobAttempt.properties.outcome":   "AttemptOutcome",
	"Role":                            "", // the ladder lives in internal/auth, checked below
}

// literalEnums pins the vocabularies that some other package writes without a
// validated table, each with where the values come from, so they are asserted
// rather than waved through.
var literalEnums = map[string][]string{
	// internal/store/repo_publish.go writes exactly these action values and these
	// two results.
	"PublishAction.properties.action": {"publish", "archive", "rejected"},
	"PublishAction.properties.result": {"succeeded", "rejected"},
	// internal/api/api.go's handleReady answers one of two words.
	"HealthStatus.properties.status": {"ok", "fail"},
	// The 202 of the one route that enqueues this kind.
	"ImportAccepted.properties.job_kind": {JobKindImportFile},
	// internal/profileio writes the manifest schema version it understands.
	"ProfileManifest.properties.schema_version": {profileio.SchemaVersion},
}

func TestOpenAPIEnumVocabulariesMatchTheDomainEnums(t *testing.T) {
	doc := openapiFixture(t)
	if _, ok := pathValue(doc, "components", "schemas", "Game").(map[string]any); !ok {
		t.Fatal("the document has no component schemas, so this check would pass vacuously")
	}

	seen := map[string][]string{}
	collect := func(sch map[string]any, where string) {
		if !strings.HasPrefix(where, "#/components/schemas/") {
			return
		}
		if values, ok := sch["enum"].([]any); ok {
			out := make([]string, 0, len(values))
			for _, v := range values {
				s, _ := v.(string)
				out = append(out, s)
			}
			seen[strings.TrimPrefix(where, "#/components/schemas/")] = out
		}
	}
	walkSchemas(doc, collect)
	if len(seen) == 0 {
		t.Fatal("no enum vocabulary was found in the component schemas")
	}

	for path, documented := range seen {
		if len(documented) == 0 {
			t.Errorf("%s documents an empty enum", path)
			continue
		}
		typeName, isDomain := domainEnums[path]
		if isDomain && typeName == "" {
			// Role: the ladder is ordered and lives in internal/auth.
			var ladder []string
			for _, role := range auth.Ordered() {
				ladder = append(ladder, role.String())
			}
			if !sameStrings(documented, ladder) {
				t.Errorf("Role documents %v, auth.Ordered is %v", documented, ladder)
			}
			continue
		}
		if isDomain {
			var actual []string
			if err := games.WalkEnum(typeName, func(v string) { actual = append(actual, v) }); err != nil {
				t.Fatalf("%s maps to games.%s: %v", path, typeName, err)
			}
			// Same members, in any order. A missing value makes a generated client
			// reject what the service stores; an extra one makes it accept what the
			// service refuses. Both are contract lies.
			if !sameStrings(documented, actual) {
				t.Errorf("%s documents %v, but games.%s is %v", path, documented, typeName, actual)
			}
			continue
		}
		want, pinned := literalEnums[path]
		if !pinned {
			t.Errorf("%s declares an enum vocabulary this test accounts for nowhere: %v\n"+
				"add it to domainEnums (the games type name) or literalEnums (naming where the values are written)",
				path, documented)
			continue
		}
		if !sameStrings(documented, want) {
			t.Errorf("%s documents %v, the writing code produces %v", path, documented, want)
		}
	}

	// Visibility, not a claim: which of the domain's fifteen vocabularies this
	// surface describes. Most belong to rows Milestone 1 does not expose (corrections,
	// edges, assertions, source types), so nothing is asserted about the direction —
	// but the list is printed where a reviewer will see it.
	t.Logf("documented vocabularies: %d; domain vocabularies: %d (%s)",
		len(seen), len(games.EnumNames()), strings.Join(games.EnumNames(), ","))
}

// TestOpenAPIRequestSchemasMatchTheHandlers is the input half of the same promise:
// where the document says a body member is required or forbidden, the handler must
// behave that way. The check is the HTTP answer, not a reflection over structs,
// because the document's promise is about behaviour.
func TestOpenAPIRequestSchemasMatchTheHandlers(t *testing.T) {
	e := newCatalogEnv(t)
	doc := openapiFixture(t)
	paths, _ := pathValue(doc, "paths").(map[string]any)

	// The complete create works, and each documented-required member dropped alone
	// does not.
	if rec := e.post(auth.RoleCurator, "/api/v1/games",
		`{"slug":"contract-full","title":"Contract Full"}`, "Idempotency-Key", "req-full"); rec.Code != http.StatusCreated {
		t.Fatalf("the complete create = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	for field, payload := range map[string]string{
		"slug":  `{"title":"Contract No Slug"}`,
		"title": `{"slug":"contract-no-title"}`,
	} {
		rec := e.post(auth.RoleCurator, "/api/v1/games", payload, "Idempotency-Key", "req-"+field)
		if rec.Code == http.StatusCreated {
			t.Errorf("POST /api/v1/games succeeded with the documented-required member %q absent: %s",
				field, rec.Body.String())
		}
	}

	// The required list names exactly the two fields the handler treats as
	// mandatory: an over-long list would promise a refusal the handler does not
	// raise, an under-long one would let a title-less row through.
	schema, _ := pathValue(paths, "/api/v1/games", "post", "requestBody", "content",
		"application/json", "schema").(map[string]any)
	var required []string
	for _, raw := range schema["required"].([]any) {
		required = append(required, raw.(string))
	}
	if !sameStrings(required, []string{"slug", "title"}) {
		t.Errorf("POST /api/v1/games documents required = %v, want [slug title]", required)
	}

	_, _, profileID, etag := newDraft(t, e, "contract-fields", "contract-fields")
	msgs := mustList(e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages?limit=1")["messages"])
	if len(msgs) == 0 {
		t.Fatal("the fixture draft has no messages")
	}
	mid := msgs[0].(map[string]any)["id"].(string)
	// A member the contract does not list is refused by name — the same DisallowUnknownFields
	// rule the document states as additionalProperties: false.
	rec := e.patch(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/messages/"+mid,
		`{"speech_text":"ok","flags":{}}`, "If-Match", etag)
	requireProblem(t, rec, http.StatusBadRequest, codeInvalidParameter)
	if !strings.Contains(rec.Body.String(), "flags") {
		t.Errorf("the refusal did not name the undocumented member: %s", rec.Body.String())
	}

	// And the documented PATCH schema really says what the handler does: one
	// writable property, no open tail.
	patchMedia, _ := pathValue(paths, "/api/v1/profiles/{id}/messages/{mid}", "patch",
		"requestBody", "content", "application/json").(map[string]any)
	patchSchema, _ := patchMedia["schema"].(map[string]any)
	props, _ := patchSchema["properties"].(map[string]any)
	if len(props) != 1 {
		t.Errorf("the PATCH message request documents %d properties (%s), want the one writable field",
			len(props), strings.Join(sortedKeysOfMap(props), ","))
	}
	if _, ok := props["speech_text"]; !ok {
		t.Errorf("the PATCH message request does not document speech_text: %s", strings.Join(sortedKeysOfMap(props), ","))
	}
	if strict, isBool := patchSchema["additionalProperties"].(bool); !isBool || strict {
		t.Errorf("the PATCH message request must document additionalProperties: false, because DisallowUnknownFields enforces exactly that: %v",
			patchSchema["additionalProperties"])
	}
	if req, _ := patchSchema["required"].([]any); len(req) != 1 || req[0] != "speech_text" {
		t.Errorf("the PATCH message request documents required = %v, want [speech_text]", req)
	}
}

func sortedKeysOfMap(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
