package profileio

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// ImportManifestOptions configures a re-import.
type ImportManifestOptions struct {
	// TargetEditionID names an existing edition to import into. Empty means
	// "create the game and edition this manifest describes", which is what an
	// import onto a fresh appliance does.
	TargetEditionID string

	// Force accepts a language or region mismatch against the target edition.
	// It exists for the deliberate case (importing a JP script's structure to
	// work on it in a US edition), not as a default: the check is what stops an
	// accidental cross-edition mixup (docs/spec/12 "source cross-edition mixup").
	Force bool
}

// ImportManifestReport is what a re-import did.
type ImportManifestReport struct {
	ProfileID string `json:"profile_id"`
	EditionID string `json:"edition_id"`
	GameID    string `json:"game_id"`
	// CreatedCatalog is true when the import had to create the game or edition
	// rather than find them.
	CreatedCatalog bool `json:"created_catalog"`

	MessagesWritten   int `json:"messages_written"`
	MessagesWithheld  int `json:"messages_withheld"`
	SourcesRegistered int `json:"sources_registered"`

	// MissingBlobs lists the content hashes whose bytes are not in the asset
	// store, so the operator knows exactly what to re-attach before the profile
	// can be re-parsed or audited (docs/spec/02: default export "requires
	// reimport or permission-controlled local reference resolution").
	MissingBlobs []string `json:"missing_blobs"`
	// PresentBlobs lists the hashes the local asset store already holds: the
	// bytes came along in a .gnpack, or this machine is where they came from.
	PresentBlobs []string `json:"present_blobs"`

	// Digest is the manifest's content fingerprint, equal to the one the export
	// stamped. It is echoed here because the screen that shows "imported" should
	// be able to say *what* it imported.
	Digest string `json:"digest"`

	// PreferredVoiceID is the voice reference the manifest carried, which this
	// build cannot apply: internal/store exposes no guarded write for
	// profiles.preferred_voice_id (ADR-0015 enumerates the writes that move the
	// row version, and none of them set this column), and issuing a bare UPDATE
	// through Tx.Exec would move catalogue state without moving the revision
	// token — the exact hazard that ADR documents. It is reported here so an
	// operator sees what was dropped instead of having to notice its absence.
	PreferredVoiceID string `json:"preferred_voice_id,omitempty"`
}

// ImportManifest reads a `.gnprofile.json` document (or the manifest entry of a
// .gnpack already unpacked by Unpack) and creates a new draft revision holding
// its content.
//
// What it refuses, in order, is the whole point of the function: a document that
// is not schema-valid; one whose digest does not match its own bytes; one whose
// edition does not match the target edition's language or region. Only then does
// it write, and what it writes is always a *new* draft with *new* ids — never an
// overwrite of an existing revision, because a re-import that could target a
// published revision would be a way to edit what sessions are narrating
// (docs/spec/02 "Transaction workflows", ADR-0015).
//
// References are remapped through one old→new map, exactly as NewRevision does
// inside a database: the property that matters is that no row of the new revision
// points at a row of any other one.
func ImportManifest(ctx context.Context, deps Deps, r io.Reader, opts ImportManifestOptions) (ImportManifestReport, error) {
	var report ImportManifestReport
	if err := deps.validate(); err != nil {
		return report, err
	}
	m, err := DecodeManifest(r)
	if err != nil {
		return report, err
	}
	return importManifest(ctx, deps, m, opts)
}

// ImportManifestDocument is ImportManifest for a document already decoded and
// verified, so a caller that read the file for another reason does not read it
// twice. The digest is re-verified here: this entry point bypasses the schema
// check that DecodeManifest performs, and an unsigned manifest must not become
// database rows because a caller held the wrong handle.
func ImportManifestDocument(ctx context.Context, deps Deps, m Manifest, opts ImportManifestOptions) (ImportManifestReport, error) {
	var report ImportManifestReport
	if err := deps.validate(); err != nil {
		return report, err
	}
	if err := m.Validate(); err != nil {
		return report, err
	}
	if err := Verify(m); err != nil {
		return report, err
	}
	return importManifest(ctx, deps, m, opts)
}

func importManifest(ctx context.Context, deps Deps, m Manifest, opts ImportManifestOptions) (ImportManifestReport, error) {
	var report ImportManifestReport
	m = m.normalized()
	report.Digest = m.Digest
	report.PreferredVoiceID = textOrEmpty(m.Publication.PreferredVoiceID)

	gameID, editionID, created, err := resolveCatalog(ctx, deps.Store, m, opts)
	if err != nil {
		return report, err
	}
	report.GameID = gameID
	report.EditionID = editionID
	report.CreatedCatalog = created

	draft, err := deps.Store.CreateDraftProfile(ctx, editionID)
	if err != nil {
		return report, fmt.Errorf("profileio: import manifest: draft: %w", err)
	}
	report.ProfileID = draft.ID

	// One map for every row this manifest defines, remapped to freshly minted
	// ids. Filled before any write so a reference can never observe a partial
	// map (the reason NewRevision mints ids first).
	remap := make(map[string]string,
		len(m.Sources)+len(m.Characters)+len(m.Scenes)+len(m.Regions)+len(m.Messages)+len(m.SpeakerAssertions))
	mint := func(old string) (string, error) {
		if old == "" {
			return "", fmt.Errorf("%w: manifest row with an empty id", ErrInvalid)
		}
		if id, ok := remap[old]; ok {
			return id, nil
		}
		id := games.NewID()
		remap[old] = id
		return id, nil
	}
	for _, s := range m.Sources {
		if _, err := mint(s.SourceID); err != nil {
			return report, err
		}
	}
	for _, c := range m.Characters {
		if _, err := mint(c.ID); err != nil {
			return report, err
		}
	}
	for _, s := range m.Scenes {
		if _, err := mint(s.ID); err != nil {
			return report, err
		}
	}
	for _, r := range m.Regions {
		if _, err := mint(r.ID); err != nil {
			return report, err
		}
	}
	for _, msg := range m.Messages {
		if _, err := mint(msg.ID); err != nil {
			return report, err
		}
	}
	for _, a := range m.SpeakerAssertions {
		if _, err := mint(a.ID); err != nil {
			return report, err
		}
	}
	// Every id reference inside a JSON payload follows the same map, which is
	// what store.remapRefs does for a snapshot: an id left behind inside a
	// markup AST would point at another revision's row.
	rewrite := func(raw json.RawMessage) json.RawMessage {
		return remapRefs(raw, remap)
	}

	ref := func(id string) (string, error) {
		if id == "" {
			return "", nil
		}
		out, ok := remap[id]
		if !ok {
			return "", fmt.Errorf("%w: manifest reference %s is not a row of this document", ErrDanglingRef, id)
		}
		return out, nil
	}
	opt := func(id *string) (string, error) {
		if id == nil {
			return "", nil
		}
		return ref(*id)
	}

	now := nowStamp()
	err = deps.Store.InTx(ctx, func(tx *store.Tx) error {
		work := tx.Context(ctx)

		sources := make([]games.Source, 0, len(m.Sources))
		for _, s := range m.Sources {
			id, err := mint(s.SourceID)
			if err != nil {
				return err
			}
			row := games.Source{
				ID: id, ProfileID: draft.ID,
				SourceType: s.Type, URI: s.URI,
				// retrieved_at is carried over rather than re-stamped: the
				// retrieval happened when the manifest says it happened, and
				// re-importing a file does not retrieve anything again. It is
				// also inside the digest projection, so a "helpful" refresh here
				// would break the round trip this whole path exists to prove.
				RetrievedAt:   s.RetrievedAt,
				ContentSHA256: s.SHA256, MimeType: s.MimeType, Rights: s.Rights,
				Trust: s.Trust, Parser: s.Parser, CoverageNotes: s.Coverage,
			}
			if row.SourceType == "" {
				row.SourceType = games.SourceUserFile
			}
			if row.URI == "" {
				// A provenance row must name *something*; the hash is the only
				// honest address for bytes that arrived without a name.
				row.URI = "asset://" + s.SHA256
			}
			if row.RetrievedAt == "" {
				row.RetrievedAt = now
			}
			if err := row.Validate(); err != nil {
				return err
			}
			sources = append(sources, row)
			switch {
			case row.ContentSHA256 == "":
			case deps.Assets == nil:
				report.MissingBlobs = append(report.MissingBlobs, row.ContentSHA256)
			default:
				if _, err := deps.Assets.Stat(work, row.ContentSHA256); err != nil {
					if errors.Is(err, assets.ErrNotFound) {
						report.MissingBlobs = append(report.MissingBlobs, row.ContentSHA256)
					} else {
						return fmt.Errorf("profileio: import manifest: stat blob %s: %w", row.ContentSHA256, err)
					}
				} else {
					report.PresentBlobs = append(report.PresentBlobs, row.ContentSHA256)
				}
			}
		}
		if len(sources) > 0 {
			if err := deps.Store.PutSources(work, draft.ID, sources); err != nil {
				return fmt.Errorf("profileio: import manifest: sources: %w", err)
			}
			report.SourcesRegistered = len(sources)
		}

		characters := make([]games.Character, 0, len(m.Characters))
		for _, c := range m.Characters {
			id, err := mint(c.ID)
			if err != nil {
				return err
			}
			row := games.Character{
				ID: id, ProfileID: draft.ID, Name: c.Name,
				Aliases:            rewrite(rawOr(c.Aliases, "[]")),
				VisualDescriptors:  rewrite(rawOr(c.Visual, "[]")),
				PersonalitySummary: c.Summary, Role: c.Role,
				VerifiedSourceRefs: rewrite(rawOr(c.VerifiedRefs, "[]")),
				Status:             c.Status, PortraitAssetID: textOrEmpty(c.Portrait),
				CreatedAt: now,
			}
			if row.Status == "" {
				row.Status = games.CharacterProposed
			}
			if err := row.Validate(); err != nil {
				return err
			}
			characters = append(characters, row)
		}
		if len(characters) > 0 {
			if err := deps.Store.PutCharacters(work, draft.ID, characters); err != nil {
				return fmt.Errorf("profileio: import manifest: characters: %w", err)
			}
		}

		scenes := make([]games.Scene, 0, len(m.Scenes))
		for _, s := range m.Scenes {
			id, err := mint(s.ID)
			if err != nil {
				return err
			}
			row := games.Scene{
				ID: id, ProfileID: draft.ID, Name: s.Name,
				Aliases:       rewrite(rawOr(s.Aliases, "[]")),
				VisualAnchors: json.RawMessage("[]"), ProgressJSON: json.RawMessage("{}"),
				CreatedAt: now,
			}
			if err := row.Validate(); err != nil {
				return err
			}
			scenes = append(scenes, row)
		}
		if len(scenes) > 0 {
			if err := deps.Store.PutScenes(work, draft.ID, scenes); err != nil {
				return fmt.Errorf("profileio: import manifest: scenes: %w", err)
			}
		}

		regions := make([]games.Region, 0, len(m.Regions))
		for _, r := range m.Regions {
			id, err := mint(r.ID)
			if err != nil {
				return err
			}
			sceneID, err := opt(r.SceneID)
			if err != nil {
				return err
			}
			row := games.Region{
				ID: id, ProfileID: draft.ID, SceneID: sceneID, Label: r.Label,
				RegionType: r.Kind, DetectMethod: r.Detect,
				RectX: r.Rect.X, RectY: r.Rect.Y, RectW: r.Rect.W, RectH: r.Rect.H,
				MinAspect: r.MinAspect, MaxAspect: r.MaxAspect,
				MinWidthPx: r.MinWidthPx, MinHeightPx: r.MinHeightPx,
				OCRSettings: rawOr(r.OCR, "{}"), MaskAssetIDs: rewrite(rawOr(r.MaskAssets, "[]")),
				Reliability: r.Reliability, Revision: 1, UpdatedAt: now,
			}
			if row.DetectMethod == "" {
				row.DetectMethod = games.DetectImported
			}
			if err := row.Validate(); err != nil {
				return err
			}
			regions = append(regions, row)
		}
		if len(regions) > 0 {
			if err := deps.Store.PutRegions(work, draft.ID, regions); err != nil {
				return fmt.Errorf("profileio: import manifest: regions: %w", err)
			}
		}

		msgs := make([]games.Message, 0, len(m.Messages))
		pages := make([]games.MessagePage, 0, len(m.Messages))
		for _, msg := range m.Messages {
			if msg.RedactedReason != nil {
				// Withheld text is not imported. The alternative — inventing a
				// placeholder string — would put words in the profile that no
				// source ever contained, and the retrieval index would rank them.
				report.MessagesWithheld++
				continue
			}
			id, err := mint(msg.ID)
			if err != nil {
				return err
			}
			sourceID, err := opt(msg.Source)
			if err != nil {
				return err
			}
			sceneID, err := opt(msg.SceneID)
			if err != nil {
				return err
			}
			speakerID, err := opt(msg.SpeakerID)
			if err != nil {
				return err
			}
			row := games.Message{
				ID: id, ProfileID: draft.ID, SourceID: sourceID,
				SourceMessageID: textOrEmpty(msg.SourceMessageID),
				OriginalText:    textOrEmpty(msg.OriginalText),
				MarkupJSON:      rewrite(rawOr(msg.Markup, "[]")),
				NormalizedText:  textOrEmpty(msg.NormalizedText),
				SpeechText:      textOrEmpty(msg.SpeechText),
				Language:        msg.Language, TextKind: msg.Kind,
				SceneID: sceneID, SpeakerID: speakerID,
				AttributionStatus:     msg.Attribution.Status,
				AttributionConfidence: msg.Attribution.Confidence,
				AttributionEvidence:   rewrite(rawOr(msg.Evidence, "[]")),
				Flags:                 rewrite(rawOr(msg.Flags, "{}")),
				Revision:              1, CreatedAt: now, UpdatedAt: now,
			}
			if row.AttributionStatus == "" {
				row.AttributionStatus = games.AttributionUnknown
			}
			if row.TextKind == "" {
				row.TextKind = games.TextOther
			}
			if err := row.Validate(); err != nil {
				return err
			}
			msgs = append(msgs, row)
			for _, p := range msg.Pages {
				if p.Text == nil {
					continue
				}
				page := games.MessagePage{
					MessageID: id, Ordinal: p.Index,
					SourcePageID:    textOrEmpty(p.SourcePageID),
					PageText:        *p.Text,
					VisiblePrefixes: rewrite(rawOr(p.Prefixes, "[]")),
					DisplayControls: rewrite(rawOr(p.Controls, "[]")),
					TimingJSON:      rewrite(rawOr(p.Timing, "{}")),
				}
				if err := page.Validate(); err != nil {
					return err
				}
				pages = append(pages, page)
			}
		}
		for start := 0; start < len(msgs); start += ImportBatchSize {
			end := min(start+ImportBatchSize, len(msgs))
			if err := deps.Store.PutMessages(work, draft.ID, msgs[start:end], pagesFor(msgs[start:end], pages)); err != nil {
				return fmt.Errorf("profileio: import manifest: messages: %w", err)
			}
			report.MessagesWritten += end - start
		}

		edges := make([]games.MessageEdge, 0, len(m.MessageEdges))
		for _, e := range m.MessageEdges {
			from, err := ref(e.From)
			if err != nil {
				return err
			}
			to, err := ref(e.To)
			if err != nil {
				return err
			}
			sourceID, err := opt(e.Source)
			if err != nil {
				return err
			}
			row := games.MessageEdge{
				FromID: from, ToID: to, EdgeType: e.Type, ConditionExpr: e.Condition,
				SourceID: sourceID, Evidence: rewrite(rawOr(e.Evidence, "[]")),
				Confidence: e.Confidence, CreatedAt: now,
			}
			if row.EdgeType == "" {
				row.EdgeType = games.EdgeNext
			}
			if err := row.Validate(); err != nil {
				return err
			}
			edges = append(edges, row)
		}
		if len(edges) > 0 {
			if err := deps.Store.PutEdges(work, draft.ID, edges); err != nil {
				return fmt.Errorf("profileio: import manifest: edges: %w", err)
			}
		}

		assertions := make([]games.SpeakerAssertion, 0, len(m.SpeakerAssertions))
		for _, a := range m.SpeakerAssertions {
			id, err := mint(a.ID)
			if err != nil {
				return err
			}
			messageID, err := ref(a.Message)
			if err != nil {
				return err
			}
			characterID, err := ref(a.Character)
			if err != nil {
				return err
			}
			row := games.SpeakerAssertion{
				ID: id, ProfileID: draft.ID, MessageID: messageID, CharacterID: characterID,
				Method: a.Method, AssertedAt: now, Evidence: rewrite(rawOr(a.Evidence, "[]")),
				Posterior: a.Posterior, Reviewer: textOrEmpty(a.Reviewer),
			}
			if err := row.Validate(); err != nil {
				return err
			}
			assertions = append(assertions, row)
		}
		if len(assertions) > 0 {
			if err := deps.Store.PutSpeakerAssertions(work, draft.ID, assertions); err != nil {
				return fmt.Errorf("profileio: import manifest: assertions: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return ImportManifestReport{}, err
	}
	return report, nil
}

// pagesFor selects the pages whose message is in this batch. The pairing is by
// message id, which is unique, so the filter is exact rather than positional.
func pagesFor(batch []games.Message, pages []games.MessagePage) []games.MessagePage {
	inBatch := make(map[string]bool, len(batch))
	for _, m := range batch {
		inBatch[m.ID] = true
	}
	out := make([]games.MessagePage, 0, len(batch))
	for _, p := range pages {
		if inBatch[p.MessageID] {
			out = append(out, p)
		}
	}
	return out
}

// resolveCatalog finds — or creates, when no target was named — the game and
// edition a manifest belongs to, and enforces the compatibility check.
func resolveCatalog(ctx context.Context, s *store.Store, m Manifest, opts ImportManifestOptions) (gameID, editionID string, created bool, err error) {
	if opts.TargetEditionID != "" {
		edition, err := s.GetEdition(ctx, opts.TargetEditionID)
		if err != nil {
			return "", "", false, fmt.Errorf("profileio: import manifest: target edition: %w", err)
		}
		if problems := editionMismatch(m.Edition, edition); len(problems) > 0 && !opts.Force {
			return "", "", false, fmt.Errorf("%w: %s; re-import with Force to accept the mismatch anyway",
				ErrIncompatibleEdition, strings.Join(problems, "; "))
		}
		return edition.GameID, edition.ID, false, nil
	}

	game, err := findOrCreateGame(ctx, s, m)
	if err != nil {
		return "", "", false, err
	}
	created = game.created
	editions, err := s.ListEditions(ctx, game.id)
	if err != nil {
		return "", "", false, fmt.Errorf("profileio: import manifest: list editions: %w", err)
	}
	for _, e := range editions {
		if e.Region == m.Edition.Region && e.Language == m.Edition.Language && e.Revision == m.Edition.Revision {
			return game.id, e.ID, created, nil
		}
	}
	edition := games.Edition{
		ID: games.NewID(), GameID: game.id,
		Region: m.Edition.Region, Language: m.Edition.Language, Revision: m.Edition.Revision,
		Locale: m.Edition.Locale, DisplayAspect: m.Edition.Aspect,
		SourceIdentity:     textOrEmpty(m.Edition.Identity),
		CompatibilityNotes: m.Edition.Notes,
	}
	if err := edition.Validate(); err != nil {
		return "", "", false, err
	}
	if err := s.CreateEdition(ctx, edition); err != nil {
		return "", "", false, fmt.Errorf("profileio: import manifest: create edition: %w", err)
	}
	return game.id, edition.ID, true, nil
}

type gameOutcome struct {
	id      string
	created bool
}

// findOrCreateGame matches the manifest's game by slug. A slug naming a different
// title is not a collision to overwrite: the import gets a slug derived from its
// own digest instead, because silently merging two games would put one title's
// messages under another title's review history.
func findOrCreateGame(ctx context.Context, s *store.Store, m Manifest) (gameOutcome, error) {
	slug := m.Edition.Slug
	if slug == "" {
		slug = slugify(m.Edition.Title)
	}
	if slug == "" {
		return gameOutcome{}, fmt.Errorf("%w: manifest edition has no title and no slug, so there is no game to import into", ErrInvalid)
	}
	existing, err := s.GetGameBySlug(ctx, slug)
	if errors.Is(err, store.ErrNotFound) {
		return createGameWithSlug(ctx, s, m, slug)
	}
	if err != nil {
		return gameOutcome{}, fmt.Errorf("profileio: import manifest: look up game %q: %w", slug, err)
	}
	if existing.Title == m.Edition.Title {
		return gameOutcome{id: existing.ID}, nil
	}
	// The slug is taken by a different title: derive one from this manifest's own
	// content so two imports of the same manifest still land on the same game.
	return createGameWithSlug(ctx, s, m, fmt.Sprintf("%s-%s", slug, shortHash(m.Digest)))
}

func createGameWithSlug(ctx context.Context, s *store.Store, m Manifest, slug string) (gameOutcome, error) {
	tags := rawOr(m.Edition.Tags, "[]")
	if err := games.RequireJSON("edition.tags", tags); err != nil {
		return gameOutcome{}, err
	}
	game := games.Game{
		ID: games.NewID(), Slug: slug, Title: m.Edition.Title,
		Description: m.Edition.Description, Platform: m.Edition.Platform,
		UserTags: tags, CreatedAt: nowStamp(),
	}
	if err := game.Validate(); err != nil {
		return gameOutcome{}, err
	}
	if err := s.CreateGame(ctx, game); err != nil {
		return gameOutcome{}, fmt.Errorf("profileio: import manifest: create game: %w", err)
	}
	return gameOutcome{id: game.ID, created: true}, nil
}

// editionMismatch reports the ways a manifest's edition disagrees with the
// edition it would be imported into. Region and language are checked because
// both change what the profile means on screen: a region selects the release, a
// language selects the OCR and speech rules. `revision` is not checked — it
// identifies the script dump, and importing a newer dump over an older edition
// row is ordinary maintenance, reported in the report rather than refused.
func editionMismatch(info EditionInfo, edition games.Edition) []string {
	var problems []string
	if !equalFoldTrim(info.Language, edition.Language) {
		problems = append(problems, fmt.Sprintf("language %q does not match the target edition's %q", info.Language, edition.Language))
	}
	if !equalFoldTrim(info.Region, edition.Region) {
		problems = append(problems, fmt.Sprintf("region %q does not match the target edition's %q", info.Region, edition.Region))
	}
	return problems
}

func equalFoldTrim(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// slugify renders a title as the slug games.Validate accepts. It is only used
// when a manifest names no slug at all; exports always carry one.
func slugify(title string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == ' ' || r == '.' || r == '/':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return ""
	}
	if len(out) > 80 {
		out = strings.Trim(out[:80], "-")
	}
	return out
}

// shortHash is a stable disambiguator derived from content, used when a slug is
// already taken by a different title.
func shortHash(digest string) string {
	if len(digest) < 8 {
		return digest
	}
	return digest[:8]
}

// remapRefs rewrites every string inside a JSON payload that names a remapped
// row, mirroring store.remapRefs so an id in a markup AST is not left pointing at
// the revision it came from.
func remapRefs(raw json.RawMessage, remap map[string]string) json.RawMessage {
	if len(raw) == 0 || len(remap) == 0 {
		return raw
	}
	var value any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return raw
	}
	changed := false
	out := remapValue(value, remap, &changed)
	if !changed {
		return raw
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return encoded
}

func remapValue(v any, remap map[string]string, changed *bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = remapValue(val, remap, changed)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = remapValue(val, remap, changed)
		}
		return t
	case string:
		if next, ok := remap[t]; ok {
			*changed = true
			return next
		}
		return t
	default:
		return v
	}
}

//
// ── .gnpack unpacking ───────────────────────────────────────────────────────
//

// UnpackReport describes one unpacked bundle.
type UnpackReport struct {
	Manifest   Manifest `json:"manifest"`
	Blobs      []string `json:"blobs"`
	AlreadyHad []string `json:"already_had"`
}

// Unpack reads a `.gnpack` stream and puts its blobs into the asset store,
// verifying each one against the address its name claims before Commit makes it
// visible (docs/spec/02: "verify on read", docs/spec/14: no implicit trust in an
// imported archive). The manifest entry is returned for ImportManifest.
//
// Only regular entries under blobs/ are written, and their names must be exactly
// 64 lowercase hex characters: a bundle carrying "../" or a symlink entry cannot
// reach outside the asset root, because the asset store's own PutStaged/Commit
// only ever addresses content hashes (docs/spec/14 "Filesystem/import").
func Unpack(ctx context.Context, as assets.Store, r io.Reader) (UnpackReport, error) {
	var report UnpackReport
	if as == nil {
		return report, fmt.Errorf("%w: Unpack requires an asset store", ErrInvalid)
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return report, fmt.Errorf("profileio: read pack: %w", err)
		}
		switch {
		case hdr.Name == PackManifestName:
			body, err := io.ReadAll(io.LimitReader(tr, int64(MaxManifestBytes)+1))
			if err != nil {
				return report, fmt.Errorf("profileio: read pack manifest: %w", err)
			}
			m, err := DecodeManifestBytes(body)
			if err != nil {
				return report, err
			}
			report.Manifest = m
		case strings.HasPrefix(hdr.Name, blobsDir+"/") && hdr.Typeflag == tar.TypeReg:
			name := strings.TrimPrefix(hdr.Name, blobsDir+"/")
			if !validHex64(name) {
				return report, fmt.Errorf("%w: pack entry %q is not a content-addressed blob", ErrInvalid, hdr.Name)
			}
			if _, err := as.Stat(ctx, name); err == nil {
				// Already here. Blobs are content-addressed, so "the same bytes"
				// and "the same blob" are one statement; re-committing would only
				// rewrite what the store has already verified.
				if err := drain(tr, hdr.Size); err != nil {
					return report, fmt.Errorf("profileio: skip existing blob %s: %w", name, err)
				}
				report.AlreadyHad = append(report.AlreadyHad, name)
				continue
			}
			// expectedSHA256 is the entry name, so a bundle whose blob does not
			// hash to the address it claims is refused before anything becomes
			// visible (docs/spec/02 "verify on read").
			id, _, err := as.PutStaged(ctx, tr, name)
			if err != nil {
				return report, fmt.Errorf("profileio: unpack blob %s: %w", name, err)
			}
			if _, err := as.Commit(ctx, id); err != nil {
				return report, fmt.Errorf("profileio: commit unpacked blob %s: %w", name, err)
			}
			report.Blobs = append(report.Blobs, name)
		default:
			// Anything else in the archive is ignored rather than written: a
			// bundle is untrusted input, and the only entries this format means
			// anything for are the manifest and the blobs.
			continue
		}
	}
	if report.Manifest.SchemaVersion == "" {
		return report, fmt.Errorf("%w: pack contained no %s entry", ErrInvalid, PackManifestName)
	}
	return report, nil
}

// drain consumes the rest of a tar entry being skipped, so the reader is
// positioned at the next header.
func drain(r io.Reader, n int64) error {
	_, err := io.Copy(io.Discard, io.LimitReader(r, n))
	return err
}
