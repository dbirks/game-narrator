package profileio

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// TestSchemaAcceptsEveryExportedManifest is the "formalize them in JSON Schema
// and create golden tests" obligation of docs/spec/17 discharged for profiles:
// every document shape Export can produce — full, redacted, empty, bundled — is
// checked against the schema that ships in this binary.
func TestSchemaAcceptsEveryExportedManifest(t *testing.T) {
	// A revision with content, and the same revision with its text withheld.
	e := newEnv(t)
	g := e.game("synthetic-tide-schema", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	decorate(t, e, p.ID, mustContent(t, e, p.ID))
	published := publish(t, e, p.ID, "curator@local")

	cases := map[string]Manifest{
		"draft with content":  profileContentFor(t, e, published.ID),
		"acknowledged bundle": mustExport(t, e, published.ID, ExportOptions{IncludeSourceBytes: true, AcknowledgeCopyright: true}),
		"empty revision":      mustExport(t, e, emptyDraft(t, e, g.Slug), ExportOptions{}),
		"redacted revision":   profileContentFor(t, e, redactedRevision(t, e)),
	}
	for name, m := range cases {
		raw, err := Encode(m)
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		if err := ValidateDocument(append(raw, '\n')); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		// And it reads back through the same door an importer uses.
		again, err := DecodeManifest(bytes.NewReader(append(raw, '\n')))
		if err != nil {
			t.Errorf("%s: DecodeManifest: %v", name, err)
			continue
		}
		if again.Digest != m.Digest {
			t.Errorf("%s: digest drifted through decode: %s vs %s", name, again.Digest, m.Digest)
		}
	}
}

// TestSchemaRejectsInvalidDocuments exercises the validator on documents a
// hostile or careless importer could hand over, and requires each failure to name
// the JSON pointer of the thing that is wrong — the difference between an error
// an onboarding screen can act on and one it can only print.
func TestSchemaRejectsInvalidDocuments(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-schema-invalid", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	decorate(t, e, p.ID, mustContent(t, e, p.ID))
	good := mustManifestBytes(t, profileContentFor(t, e, p.ID))

	for _, tc := range []struct {
		name     string
		mutate   func(doc map[string]any)
		pointer  string
		contains string
	}{
		{
			name: "unknown top-level property",
			mutate: func(doc map[string]any) {
				doc["profile_notes"] = "added by hand"
			},
			pointer:  "#/profile_notes",
			contains: "unknown property",
		},
		{
			name: "wrong type inside a message",
			mutate: func(doc map[string]any) {
				doc["messages"].([]any)[0].(map[string]any)["kind"] = 42
			},
			pointer:  "#/messages/0/kind",
			contains: "expected string",
		},
		{
			name: "value outside the content-class vocabulary",
			mutate: func(doc map[string]any) {
				doc["messages"].([]any)[0].(map[string]any)["kind"] = "cutscene"
			},
			pointer:  "#/messages/0/kind",
			contains: "not one of",
		},
		{
			name: "malformed content hash",
			mutate: func(doc map[string]any) {
				doc["sources"].([]any)[0].(map[string]any)["sha256"] = "deadbeef"
			},
			pointer:  "#/sources/0/sha256",
			contains: "does not match",
		},
		{
			name: "identifier that is not a ULID",
			mutate: func(doc map[string]any) {
				doc["characters"].([]any)[0].(map[string]any)["id"] = "the-narrator"
			},
			pointer:  "#/characters/0/id",
			contains: "does not match",
		},
		{
			name: "missing required section",
			mutate: func(doc map[string]any) {
				delete(doc, "regions")
			},
			pointer:  "#/",
			contains: `missing required property "regions"`,
		},
		{
			name: "null where an array is required",
			mutate: func(doc map[string]any) {
				doc["scenes"] = nil
			},
			pointer:  "#/scenes",
			contains: "expected array",
		},
		{
			name: "page ordinal below the first page",
			mutate: func(doc map[string]any) {
				doc["messages"].([]any)[0].(map[string]any)["pages"] = []any{map[string]any{
					"index": float64(0), "text": "first page", "source_page_id": nil,
					"prefixes": []any{}, "controls": []any{}, "timing": map[string]any{},
				}}
			},
			// The ordinal bound is a two-column rule (ordinal >= 1), which this
			// validator subset cannot state; Manifest.Validate and
			// games.MessagePage.Validate both do, and ImportManifest runs them
			// before writing anything.
			pointer:  "ordinals start at 1",
			contains: "index 0",
		},
		{
			name: "unknown property in a nested object",
			mutate: func(doc map[string]any) {
				doc["edition"].(map[string]any)["platfrom"] = "gamecube"
			},
			pointer:  "#/edition/platfrom",
			contains: "unknown property",
		},
		{
			name: "digest of the wrong shape",
			mutate: func(doc map[string]any) {
				doc["manifest_digest"] = "0123"
			},
			pointer:  "#/manifest_digest",
			contains: "does not match",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeDocument(t, good)
			tc.mutate(doc)
			raw := encodeDocument(t, doc)

			err := ValidateDocument(raw)
			schemaOnly := strings.HasPrefix(tc.pointer, "ordinals")
			if schemaOnly {
				// This one is the structural pass's job, not the schema's.
				_, err = DecodeManifestBytes(raw)
			}
			if err == nil {
				t.Fatalf("the document was accepted")
			}
			if !errors.Is(err, ErrSchemaValidation) && !schemaOnly {
				t.Errorf("err = %v, want it to wrap ErrSchemaValidation", err)
			}
			if !strings.Contains(err.Error(), tc.pointer) {
				t.Errorf("error does not name %s: %v", tc.pointer, err)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error does not say %q: %v", tc.contains, err)
			}
			// The same document must be refused on the way in, not just by a
			// standalone check nobody calls.
			fresh := newIn(t)
			if _, importErr := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(raw), ImportManifestOptions{}); importErr == nil {
				t.Error("ImportManifest accepted a document the schema rejects")
			}
		})
	}

	// Trailing content is not "the first document is fine".
	trailing := append(bytes.TrimSuffix(good, []byte("\n")), []byte("\n{}\n")...)
	if err := ValidateDocument(trailing); err == nil || !strings.Contains(err.Error(), "more than one JSON value") {
		t.Errorf("two documents in one file: err = %v", err)
	}
	if err := ValidateDocument([]byte("not json")); err == nil || !errors.Is(err, ErrSchemaValidation) {
		t.Errorf("non-JSON document: err = %v", err)
	}
}

// TestSchemaVocabulariesMatchTheDomainEnums keeps the shipped schema and the Go
// enums from drifting apart. The same idea as internal/games' test that its
// enums match the migrations' CHECK lists: two spellings of one vocabulary have
// to be compared automatically or they will disagree eventually.
func TestSchemaVocabulariesMatchTheDomainEnums(t *testing.T) {
	var schema struct {
		Defs map[string]struct {
			Enum []json.RawMessage `json:"enum"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(SchemaJSON(), &schema); err != nil {
		t.Fatalf("the shipped schema does not parse: %v", err)
	}
	checked := 0
	for _, name := range games.EnumNames() {
		def, ok := schema.Defs[name]
		if !ok {
			continue
		}
		enum, ok := games.LookupEnum(name)
		if !ok {
			t.Errorf("schema $defs names %q, which is not a registered vocabulary", name)
			continue
		}
		if strings.Join(unquoted(def.Enum), "|") != strings.Join(enum.Values, "|") {
			t.Errorf("%s: schema allows [%s], internal/games allows [%s]",
				name, strings.Join(unquoted(def.Enum), ", "), strings.Join(enum.Values, ", "))
		}
		checked++
	}
	if checked < 7 {
		t.Errorf("only %d vocabularies were compared; the schema has drifted away from the domain", checked)
	}
	// The redaction codes are this package's own vocabulary: exactly the two the
	// exporter can write, plus null, which means "not redacted" and is the value
	// every cleared message carries.
	reasons := unquoted(schema.Defs["RedactedReason"].Enum)
	if len(reasons) != 3 || reasons[0] != rightsWithheld || reasons[1] != noSourceReason || reasons[2] != "null" {
		t.Errorf("schema redacted_reason enum = %v, want [%q, %q, null]", reasons, rightsWithheld, noSourceReason)
	}
	// Every $ref the document uses resolves: compileSchema would have failed
	// otherwise, but say which one is broken when it does.
	if _, err := compileSchema(SchemaJSON()); err != nil {
		t.Fatalf("the shipped schema does not compile: %v", err)
	}
}

// TestValidatorRefusesUnknownKeywords is the failure direction that matters: a
// schema extended with a keyword this validator does not implement must not
// quietly become more permissive.
func TestValidatorRefusesUnknownKeywords(t *testing.T) {
	for _, doc := range []string{
		`{"type":"object","minLength":1}`,
		`{"type":"array","prefixItems":[{"type":"string"}]}`,
		`{"oneOf":[{"type":"string"},{"type":"null"}]}`,
		`{"not":{"type":"string"}}`,
		`{"properties":{"a":{"$ref":"#/definitions/x"}}}`,
		`{"properties":{"a":{"$ref":"#/$defs/missing"}}}`,
		`{"properties":{"a":{"$ref":"https://example.invalid/schema.json"}}}`,
		`{"properties":{"a":{"type":"string","$ref":"#/$defs/sha256"}}}`,
		`{"pattern":"^(unclosed"}`,
	} {
		if _, err := compileSchema([]byte(doc)); err == nil {
			t.Errorf("a schema this validator cannot honour compiled anyway: %s", doc)
		}
	}
	// Recursion is refused rather than followed until the stack gives out.
	recursive := `{"$defs":{"loop":{"$ref":"#/$defs/loop"}},"properties":{"a":{"$ref":"#/$defs/loop"}}}`
	if _, err := compileSchema([]byte(recursive)); err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Errorf("recursive schema: err = %v", err)
	}
}

//
// ── fixture helpers ─────────────────────────────────────────────────────────
//

// unquoted renders an enum list for comparison, leaving JSON null as "null".
func unquoted(list []json.RawMessage) []string {
	out := make([]string, 0, len(list))
	for _, raw := range list {
		var s string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			out = append(out, "null")
			continue
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			out = append(out, string(raw))
			continue
		}
		out = append(out, s)
	}
	return out
}

func mustExport(t *testing.T, e *env, profileID string, opts ExportOptions) Manifest {
	t.Helper()
	m, err := Export(e.ctx, e.store, profileID, opts)
	if err != nil {
		t.Fatalf("Export(%s): %v", profileID, err)
	}
	return m
}

// emptyDraft starts a second revision lineage on the same game, holding nothing:
// the shape of a profile that has been created but not yet populated, which is a
// real import target and must still be a valid manifest.
func emptyDraft(t *testing.T, e *env, gameSlug string) string {
	t.Helper()
	g, err := e.store.GetGameBySlug(e.ctx, gameSlug)
	if err != nil {
		t.Fatalf("GetGameBySlug(%s): %v", gameSlug, err)
	}
	editions, err := e.store.ListEditions(e.ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(editions) == 0 {
		t.Fatal("the fixture game has no editions")
	}
	other := games.Edition{
		ID: games.NewID(), GameID: g.ID, Region: "JPC", Language: "en", Revision: "empty",
	}
	if err := e.store.CreateEdition(e.ctx, other); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	p, err := e.store.CreateDraftProfile(e.ctx, other.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	return p.ID
}

// redactedRevision builds a revision whose only content rests on unsettled
// rights, so its export is the withheld shape.
func redactedRevision(t *testing.T, e *env) string {
	t.Helper()
	g := e.game("synthetic-tide-schema-redacted", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	source := games.Source{
		ID: games.NewID(), ProfileID: p.ID, SourceType: games.SourceRepository,
		URI: "https://example.invalid/mirror", RetrievedAt: nowStamp(),
		ContentSHA256: sha256Bytes([]byte("invented repository dump")),
		MimeType:      "text/plain", Rights: games.RightsUnknown, Trust: 0.2,
		Parser: "profileio.test@1.0.0",
	}
	msg := games.Message{
		ID: games.NewID(), ProfileID: p.ID, SourceID: source.ID, SourceMessageID: "dump-1",
		OriginalText:   "Text nobody has cleared for sharing.",
		NormalizedText: "text nobody has cleared for sharing",
		SpeechText:     "Text nobody has cleared for sharing.", Language: "en",
		TextKind: games.TextDialogue, AttributionStatus: games.AttributionUnknown,
		Revision: 1, CreatedAt: nowStamp(), UpdatedAt: nowStamp(),
	}
	if err := e.store.PutSources(e.ctx, p.ID, []games.Source{source}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutMessages(e.ctx, p.ID, []games.Message{msg}, []games.MessagePage{
		{MessageID: msg.ID, Ordinal: 1, PageText: msg.OriginalText},
	}); err != nil {
		t.Fatal(err)
	}
	return p.ID
}
