package profileio

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
)

// Manifest is the `.gnprofile.json` document (docs/spec/17 "Profile manifest").
//
// Field names follow that sketch where it has names (`sources[].source_id`,
// `messages[].source`, `pages[].index`, `message_edges`, `publication`), and the
// sketch is explicit about being a sketch, so three things were settled while
// formalizing it:
//
//   - `scenes` is a section of its own. The sketch puts a bare `"scene_id"` on a
//     message, but messages.scene_id is a foreign key into `scenes`
//     (0002_content.sql) and the publication gate refuses a revision whose
//     scenes do not resolve, so a manifest that named scenes without describing
//     them could never be re-imported.
//   - `pages[].index` is the 1-based `message_pages.ordinal`, not the 0 the
//     sketch showed, because the schema's CHECK requires >= 1 and renumbering on
//     the way in and out is a place to lose a page.
//   - Sources carry `uri`, `mime_type`, `trust`, `coverage` and `retrieved_at`
//     beside the sketch's four fields. docs/spec/14 requires source_uri, the
//     rights state and importer notes per asset, and a re-import must be able to
//     register the same provenance rather than invent a new one. None of them
//     are copyrighted bytes: the bytes stay in the asset store.
//
// Every id in a manifest is the exporting database's own row id. They are kept
// for cross-checking a re-import against what it came from, and are deliberately
// excluded from manifest_digest, which is a fingerprint of *content* (see
// canonical.go).
type Manifest struct {
	SchemaVersion string          `json:"schema_version"`
	ProfileID     string          `json:"profile_id"`
	Edition       EditionInfo     `json:"edition"`
	Publication   PublicationInfo `json:"publication"`

	Sources           []SourceInfo    `json:"sources"`
	Characters        []CharacterInfo `json:"characters"`
	Scenes            []SceneInfo     `json:"scenes"`
	Regions           []RegionInfo    `json:"regions"`
	Messages          []MessageInfo   `json:"messages"`
	MessageEdges      []EdgeInfo      `json:"message_edges"`
	SpeakerAssertions []AssertionInfo `json:"speaker_assertions"`

	// Digest fingerprints content, not rows: see canonical.go for why a digest
	// that survived nothing but a re-import is the only kind a portable manifest
	// can carry.
	Digest string `json:"manifest_digest"`
}

// EditionInfo is the edition the revision belongs to, in the portability form:
// enough to find or create the matching row elsewhere, and nothing that only
// makes sense inside the exporting database. `slug` is what names the game row
// on re-import; `title` is what a human reads.
type EditionInfo struct {
	Title       string          `json:"title"`
	Slug        string          `json:"slug"`
	Platform    string          `json:"platform"`
	Description string          `json:"description"`
	Tags        json.RawMessage `json:"tags"`
	Region      string          `json:"region"`
	Language    string          `json:"language"`
	Revision    string          `json:"revision"`
	Locale      string          `json:"locale"`
	Notes       string          `json:"notes"`
	Aspect      *float64        `json:"aspect"`
	Identity    *string         `json:"identity"` // edition source_identity: a 64-hex hash or null
}

// PublicationInfo is the lifecycle half of the manifest. It is the part a
// re-import does *not* reproduce — a manifest always becomes a draft — which is
// why Digest ignores this section entirely.
type PublicationInfo struct {
	Status           games.ProfileStatus `json:"status"`
	Revision         int                 `json:"revision"`
	ParentRevision   *int                `json:"parent_revision"`
	ApprovedAt       *string             `json:"approved_at"`
	Author           string              `json:"author"`
	PreferredVoiceID *string             `json:"preferred_voice_id"`
	// StoreDigest is profiles.manifest_digest as the store computed it at
	// publication. It is carried for audit only: it hashes row ids and so
	// cannot survive a re-import, unlike manifest_digest. Naming both would be
	// the dishonest option; naming them differently is the honest one.
	StoreDigest string `json:"store_digest"`
}

// SourceInfo is one provenance row. `sha256` is the content address; the bytes
// it names live in the asset store and travel only inside a .gnpack.
type SourceInfo struct {
	SourceID    string             `json:"source_id"`
	Type        games.SourceType   `json:"type"`
	SHA256      string             `json:"sha256"`
	Rights      games.RightsStatus `json:"rights"`
	Parser      string             `json:"parser"`
	URI         string             `json:"uri"`
	MimeType    string             `json:"mime_type"`
	Trust       float64            `json:"trust"`
	Coverage    string             `json:"coverage"`
	RetrievedAt string             `json:"retrieved_at"`
}

// CharacterInfo is one roster row. `voice_profile_id` is always null in this
// build: voice definitions are Milestone 4, and the field exists so a manifest
// written today can carry a voice reference when the voices table arrives
// without changing schema_version.
type CharacterInfo struct {
	ID             string                `json:"id"`
	Name           string                `json:"name"`
	Aliases        json.RawMessage       `json:"aliases"`
	Role           string                `json:"role"`
	Summary        string                `json:"summary"`
	Visual         json.RawMessage       `json:"visual"`
	Status         games.CharacterStatus `json:"status"`
	VerifiedRefs   json.RawMessage       `json:"verified_source_refs"`
	Portrait       *string               `json:"portrait"`
	VoiceProfileID *string               `json:"voice_profile_id"`
}

// SceneInfo is one scene. Region fingerprints and visual anchors are omitted:
// they describe captured frames (Milestone 2), not the text a profile carries,
// and a manifest claiming to round-trip them would be claiming data this build
// does not produce.
type SceneInfo struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Aliases json.RawMessage `json:"aliases"`
}

// Rect is the normalized box, in the sketch's spelling.
type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// RegionInfo is one crop with its OCR settings.
type RegionInfo struct {
	ID          string             `json:"id"`
	Label       string             `json:"label"`
	Kind        games.RegionType   `json:"kind"`
	Detect      games.DetectMethod `json:"detect"`
	SceneID     *string            `json:"scene"`
	Rect        Rect               `json:"rect"`
	OCR         json.RawMessage    `json:"ocr"`
	MaskAssets  json.RawMessage    `json:"mask_assets"`
	Reliability *float64           `json:"reliability"`
	MinAspect   *float64           `json:"min_aspect"`
	MaxAspect   *float64           `json:"max_aspect"`
	MinWidthPx  *int               `json:"min_width_px"`
	MinHeightPx *int               `json:"min_height_px"`
}

// MessageInfo is one canonical message. The three text fields are nullable
// because a message whose source's rights are unsettled is exported as structure
// with the text withheld (docs/spec/14); the reason is stated in
// redacted_reason rather than left for a reader to infer.
type MessageInfo struct {
	ID              string          `json:"id"`
	Source          *string         `json:"source"`
	SourceMessageID *string         `json:"source_message_id"`
	Kind            games.TextKind  `json:"kind"`
	Language        string          `json:"language"`
	OriginalText    *string         `json:"original_text"`
	NormalizedText  *string         `json:"normalized_text"`
	SpeechText      *string         `json:"speech_text"`
	SpeakerID       *string         `json:"speaker_id"`
	SceneID         *string         `json:"scene_id"`
	Attribution     AttributionInfo `json:"attribution"`
	Markup          json.RawMessage `json:"markup"`
	Evidence        json.RawMessage `json:"evidence"`
	Flags           json.RawMessage `json:"flags"`
	RedactedReason  *string         `json:"redacted_reason"`
	Pages           []PageInfo      `json:"pages"`
}

// AttributionInfo pairs a speaker with how much to believe it, because
// attribution_status without speaker_id (or the reverse) is half a fact
// (docs/spec/02 "Speaker attribution is not necessarily a speaker-name").
type AttributionInfo struct {
	Status     games.AttributionStatus `json:"status"`
	Confidence float64                 `json:"confidence"`
}

// PageInfo is one display page. Pages are nested inside their message rather
// than a section of their own: (message_id, ordinal) *is* a page's identity, and
// a section keyed by a pair would only invite the pair to be got wrong.
type PageInfo struct {
	Index        int             `json:"index"`
	Text         *string         `json:"text"`
	SourcePageID *string         `json:"source_page_id"`
	Prefixes     json.RawMessage `json:"prefixes"`
	Controls     json.RawMessage `json:"controls"`
	Timing       json.RawMessage `json:"timing"`
}

// EdgeInfo is one dialogue transition. Edges have no id of their own —
// UNIQUE (from_id, to_id, edge_type) is the identity — so they carry endpoints.
type EdgeInfo struct {
	From       string          `json:"from"`
	To         string          `json:"to"`
	Type       games.EdgeType  `json:"type"`
	Condition  string          `json:"condition"`
	Source     *string         `json:"source"`
	Confidence float64         `json:"confidence"`
	Evidence   json.RawMessage `json:"evidence"`
}

// AssertionInfo is one candidate speaker that is not the resolved one.
type AssertionInfo struct {
	ID        string                `json:"id"`
	Message   string                `json:"message"`
	Character string                `json:"character"`
	Method    games.AssertionMethod `json:"method"`
	Posterior float64               `json:"posterior"`
	Reviewer  *string               `json:"reviewer"`
	Evidence  json.RawMessage       `json:"evidence"`
}

//
// ── normalization ───────────────────────────────────────────────────────────
//

// normalized returns m with every JSON column that arrived absent, null or empty
// replaced by the default its table carries. internal/games' rule is that a JSON
// column is never nil when valid — the schema defaults these to [] or {} — and a
// manifest written by hand (or by an older writer that emitted null) must not be
// the reason a row fails a CHECK constraint halfway through a transaction.
// Normalizing before hashing also means `null`, `[]` and an absent key all
// produce the same digest, which is the right answer: they all mean "none".
func (m Manifest) normalized() Manifest {
	out := m
	out.Edition.Tags = jsonJSONArray(m.Edition.Tags)
	// Sources have no JSON columns; an empty uri or a missing retrieval timestamp
	// is filled at row-build time, where the content hash can stand in for an
	// address (see importManifest), rather than being invented here.
	for i := range out.Characters {
		c := &out.Characters[i]
		c.Aliases = jsonJSONArray(c.Aliases)
		c.Visual = jsonJSONArray(c.Visual)
		c.VerifiedRefs = jsonJSONArray(c.VerifiedRefs)
	}
	for i := range out.Scenes {
		out.Scenes[i].Aliases = jsonJSONArray(out.Scenes[i].Aliases)
	}
	for i := range out.Regions {
		r := &out.Regions[i]
		r.OCR = jsonJSONObject(r.OCR)
		r.MaskAssets = jsonJSONArray(r.MaskAssets)
	}
	for i := range out.Messages {
		msg := &out.Messages[i]
		msg.Markup = jsonJSONArray(msg.Markup)
		msg.Evidence = jsonJSONArray(msg.Evidence)
		msg.Flags = jsonJSONObject(msg.Flags)
		for j := range msg.Pages {
			p := &msg.Pages[j]
			p.Prefixes = jsonJSONArray(p.Prefixes)
			p.Controls = jsonJSONArray(p.Controls)
			p.Timing = jsonJSONObject(p.Timing)
		}
	}
	for i := range out.MessageEdges {
		out.MessageEdges[i].Evidence = jsonJSONArray(out.MessageEdges[i].Evidence)
	}
	for i := range out.SpeakerAssertions {
		out.SpeakerAssertions[i].Evidence = jsonJSONArray(out.SpeakerAssertions[i].Evidence)
	}
	for i := range out.Messages {
		if out.Messages[i].Pages == nil {
			out.Messages[i].Pages = []PageInfo{}
		}
	}
	return out
}

func jsonJSONArray(raw json.RawMessage) json.RawMessage  { return jsonDefaultJSON(raw, "[]") }
func jsonJSONObject(raw json.RawMessage) json.RawMessage { return jsonDefaultJSON(raw, "{}") }

func jsonDefaultJSON(raw json.RawMessage, fallback string) json.RawMessage {
	switch strings.TrimSpace(string(raw)) {
	case "", "null":
		return json.RawMessage(fallback)
	}
	return raw
}

//
// ── structural validation ───────────────────────────────────────────────────
//

// Validate checks what the JSON Schema cannot: that the document's own
// references resolve, that ids are unique, and that each row would satisfy the
// domain type it becomes. The schema check (schema.go) runs first on raw
// structure and catches type, enum and shape problems by JSON pointer; this
// catches the ones that need to see two parts of the document at once.
func (m Manifest) Validate() error {
	m = m.normalized()
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schema_version %q, this build reads %q", ErrInvalid, m.SchemaVersion, SchemaVersion)
	}
	if m.Edition.Title == "" {
		return fmt.Errorf("%w: edition.title is required", ErrInvalid)
	}
	seen := map[string]string{} // id -> what claimed it, for the error message
	check := func(kind, id string) error {
		if id == "" {
			return fmt.Errorf("%w: %s entry with an empty id", ErrInvalid, kind)
		}
		if prev, dup := seen[id]; dup {
			return fmt.Errorf("%w: id %s is used by both %s and %s", ErrInvalid, id, prev, kind)
		}
		seen[id] = kind
		return nil
	}

	for _, s := range m.Sources {
		if err := check("source", s.SourceID); err != nil {
			return err
		}
		if s.SHA256 != "" && !validHex64(s.SHA256) {
			return fmt.Errorf("%w: source %s sha256 %q is not 64 lowercase hex characters", ErrInvalid, s.SourceID, s.SHA256)
		}
		if !s.Type.Valid() {
			return fmt.Errorf("%w: source %s type %q", ErrInvalid, s.SourceID, string(s.Type))
		}
		if !s.Rights.Valid() {
			return fmt.Errorf("%w: source %s rights %q", ErrInvalid, s.SourceID, string(s.Rights))
		}
	}
	for _, c := range m.Characters {
		if err := check("character", c.ID); err != nil {
			return err
		}
		if c.Name == "" {
			return fmt.Errorf("%w: character %s has no name", ErrInvalid, c.ID)
		}
	}
	for _, s := range m.Scenes {
		if err := check("scene", s.ID); err != nil {
			return err
		}
	}
	for _, r := range m.Regions {
		if err := check("region", r.ID); err != nil {
			return err
		}
	}
	// The two-pass check above only sees ids defined so far, and a message may
	// name a scene or a character defined after it. A JSON document has no
	// "before", so resolve references in a second pass over the whole id set.
	for _, msg := range m.Messages {
		if err := check("message", msg.ID); err != nil {
			return err
		}
	}
	for _, a := range m.SpeakerAssertions {
		if err := check("assertion", a.ID); err != nil {
			return err
		}
	}

	// Second pass: every optional foreign key must name a row of the right kind.
	want := func(from, what, id, expect string) error {
		if id == "" {
			return nil
		}
		kind, ok := seen[id]
		if !ok {
			return fmt.Errorf("%w: %s: %s reference to %s, which this manifest does not define", ErrDanglingRef, from, what, id)
		}
		if kind != expect {
			return fmt.Errorf("%w: %s: %s reference to %s, which is a %s", ErrDanglingRef, from, what, id, kind)
		}
		return nil
	}
	for _, msg := range m.Messages {
		if msg.Source != nil {
			if err := want("message "+msg.ID, "source", *msg.Source, "source"); err != nil {
				return err
			}
		}
		if msg.SceneID != nil {
			if err := want("message "+msg.ID, "scene", *msg.SceneID, "scene"); err != nil {
				return err
			}
		}
		if msg.SpeakerID != nil {
			if err := want("message "+msg.ID, "speaker", *msg.SpeakerID, "character"); err != nil {
				return err
			}
		}
		if !msg.Kind.Valid() {
			return fmt.Errorf("%w: message %s kind %q", ErrInvalid, msg.ID, string(msg.Kind))
		}
		if msg.Attribution.Status != "" && !msg.Attribution.Status.Valid() {
			return fmt.Errorf("%w: message %s attribution status %q", ErrInvalid, msg.ID, string(msg.Attribution.Status))
		}
		if msg.RedactedReason != nil {
			if msg.OriginalText != nil || msg.NormalizedText != nil {
				return fmt.Errorf("%w: message %s is redacted but still carries text", ErrInvalid, msg.ID)
			}
		} else if msg.OriginalText == nil {
			return fmt.Errorf("%w: message %s has no original_text and no redacted_reason", ErrInvalid, msg.ID)
		}
		for _, p := range msg.Pages {
			if p.Index < 1 {
				return fmt.Errorf("%w: message %s has a page with index %d; page ordinals start at 1", ErrInvalid, msg.ID, p.Index)
			}
			if p.Text == nil && msg.RedactedReason == nil {
				return fmt.Errorf("%w: message %s has a page with no text and no redaction reason", ErrInvalid, msg.ID)
			}
		}
	}
	for _, r := range m.Regions {
		if r.SceneID != nil {
			if err := want("region "+r.ID, "scene", *r.SceneID, "scene"); err != nil {
				return err
			}
		}
	}
	for _, e := range m.MessageEdges {
		if err := want("edge "+e.From+"->"+e.To+" ("+string(e.Type)+")", "from", e.From, "message"); err != nil {
			return err
		}
		if err := want("edge "+e.From+"->"+e.To+" ("+string(e.Type)+")", "to", e.To, "message"); err != nil {
			return err
		}
		if e.Source != nil {
			if err := want("edge "+e.From+"->"+e.To, "source", *e.Source, "source"); err != nil {
				return err
			}
		}
		if !e.Type.Valid() {
			return fmt.Errorf("%w: edge %s->%s has type %q", ErrInvalid, e.From, e.To, string(e.Type))
		}
	}
	for _, a := range m.SpeakerAssertions {
		if err := want("assertion "+a.ID, "message", a.Message, "message"); err != nil {
			return err
		}
		if err := want("assertion "+a.ID, "character", a.Character, "character"); err != nil {
			return err
		}
		if !a.Method.Valid() {
			return fmt.Errorf("%w: assertion %s method %q", ErrInvalid, a.ID, string(a.Method))
		}
	}
	return nil
}

// validHex64 reports whether s is exactly 64 lowercase hex characters, the shape
// every content address in this schema has. games.requireHash enforces the same
// rule on rows but is unexported, and a manifest is checked before any row
// exists; the schema states it again as a pattern.
func validHex64(s string) bool {
	if len(s) != games.MaxSHA256Hex {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// textOrEmpty dereferences an optional text field for storage, where an absent
// text means "withheld" and no column accepts null.
func textOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func strPtr(s string) *string { return &s }

// rawOr returns raw when it holds a JSON value, and the fallback otherwise, so a
// hand-written manifest that omits a JSON column still produces a valid row
// (the schema defaults these to [] and {}).
func rawOr(raw json.RawMessage, fallback string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(fallback)
	}
	return raw
}
