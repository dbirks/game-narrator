package profileio

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// unixEpoch is the tar mtime every pack entry carries, so an export of the same
// content is byte-identical twice (the same reason assets.Export pins it).
var unixEpoch = time.Unix(0, 0).UTC()

// ExportOptions configures one export.
type ExportOptions struct {
	// IncludeSourceBytes asks for a `.gnpack` — the manifest plus the blobs its
	// sources name. Off by default: docs/spec/02 "Profile portability" and
	// docs/spec/14 both say the default export shares *structured* profiles and
	// not the material they were derived from.
	IncludeSourceBytes bool

	// AcknowledgeCopyright must be true for IncludeSourceBytes. It is a user
	// decision recorded by the caller (docs/spec/14: "private user bundle export
	// optional with explicit acknowledgement"), not a default, not a config
	// file, and not something an agent may set on a user's behalf. When it is
	// given, the manifest carries the text derived from those bytes too: a bundle
	// whose bytes are the user's own but whose text was withheld would be neither
	// shareable nor useful.
	AcknowledgeCopyright bool
}

func (o ExportOptions) validate() error {
	if o.IncludeSourceBytes && !o.AcknowledgeCopyright {
		return fmt.Errorf("%w: IncludeSourceBytes carries the user's own game text and bytes, and Export refuses to decide that is allowed",
			ErrCopyrightAcknowledgement)
	}
	return nil
}

// Export reads a revision and renders it as a portable manifest. Any revision
// status may be exported — a draft to move work between machines, a published or
// archived one to hand somebody exactly what shipped.
//
// The manifest is stamped with its digest before it is returned, so what a caller
// holds is always a document that verifies against itself.
func Export(ctx context.Context, s *store.Store, profileID string, opts ExportOptions) (Manifest, error) {
	var m Manifest
	if err := opts.validate(); err != nil {
		return m, err
	}
	if s == nil {
		return m, fmt.Errorf("%w: Export requires a store", ErrInvalid)
	}
	content, err := s.GetProfileContent(ctx, profileID)
	if err != nil {
		return m, fmt.Errorf("profileio: export %s: %w", profileID, err)
	}
	edition, err := s.GetEdition(ctx, content.Profile.EditionID)
	if err != nil {
		return m, fmt.Errorf("profileio: export: edition: %w", err)
	}
	game, err := s.GetGame(ctx, edition.GameID)
	if err != nil {
		return m, fmt.Errorf("profileio: export: game: %w", err)
	}

	// Rights are a property of the source, so the decision is made once per
	// source and applied to the messages that rest on it. A pack that the user
	// acknowledged carries its text; the default export does not.
	textOf := map[string]games.RightsStatus{}
	for _, src := range content.Sources {
		textOf[src.ID] = src.Rights
	}
	withhold := !opts.IncludeSourceBytes

	// The approved voices this revision can actually speak with (ADR-0101):
	// the roster's characters plus the narrator, the set that resolveVoice
	// can resolve. A roster character whose voice was approved for an older
	// revision's row (re-minted ids) is deliberately not here — the voice
	// follows the character row it was approved for, and this revision's
	// narrator fallback is the honest answer until the voice is approved
	// against the current roster.
	roster := make(map[string]bool, len(content.Characters))
	for _, ch := range content.Characters {
		roster[ch.ID] = true
	}
	roster[narratorKey] = true
	allVoices, err := s.ListApprovedVoiceRevisions(ctx)
	if err != nil {
		return m, fmt.Errorf("profileio: export %s: voices: %w", profileID, err)
	}
	var voiceRows []VoiceInfo
	voiceByCharacter := map[string]string{}
	for _, v := range allVoices {
		if !roster[v.Character] || v.Fingerprint == "" || v.ReferenceAssetID == "" {
			continue
		}
		row := VoiceInfo{
			ID:                v.ID,
			Character:         v.Character,
			Revision:          v.Revision,
			State:             v.State,
			ReferenceSHA256:   v.ReferenceAssetID,
			RefTranscript:     v.RefTranscript,
			ModelID:           v.ModelID,
			EmbeddingMode:     v.EmbeddingMode,
			Language:          v.Language,
			Fingerprint:       v.Fingerprint,
			Gain:              v.Gain,
			DesignInstruction: v.DesignInstruction,
			ConsentProvenance: v.ConsentProvenance,
			ApprovedBy:        v.ApprovedBy,
			ApprovedAt:        nilIfEmptyRfc3339(v.ApprovedAt),
		}
		voiceRows = append(voiceRows, row)
		if v.Character != narratorKey {
			voiceByCharacter[v.Character] = v.ID
		}
	}
	if voiceRows == nil {
		voiceRows = []VoiceInfo{}
	}

	m = Manifest{
		SchemaVersion: SchemaVersion,
		ProfileID:     content.Profile.ID,
		Edition: EditionInfo{
			Title:       game.Title,
			Slug:        game.Slug,
			Platform:    game.Platform,
			Description: game.Description,
			Tags:        orEmptyJSON(game.UserTags, "[]"),
			Region:      edition.Region,
			Language:    edition.Language,
			Revision:    edition.Revision,
			Locale:      edition.Locale,
			Notes:       edition.CompatibilityNotes,
			Aspect:      edition.DisplayAspect,
			Identity:    nilIfEmpty(edition.SourceIdentity),
		},
		Publication: PublicationInfo{
			Status:           content.Profile.Status,
			Revision:         content.Profile.RevisionNum,
			ParentRevision:   content.Profile.ParentRevision,
			ApprovedAt:       nilIfEmpty(content.Profile.PublishedAt),
			Author:           content.Profile.PublicationAuthor,
			PreferredVoiceID: nilIfEmpty(content.Profile.PreferredVoiceID),
			NarratorFallback: ptrBool(content.Profile.NarratorFallback),
			AutoCast:         ptrBool(content.Profile.AutoCast),
			StoreDigest:      content.Profile.ManifestDigest,
		},
		Voices: voiceRows,
	}

	for _, src := range content.Sources {
		m.Sources = append(m.Sources, SourceInfo{
			SourceID:    src.ID,
			Type:        src.SourceType,
			SHA256:      src.ContentSHA256,
			Rights:      src.Rights,
			Parser:      src.Parser,
			URI:         src.URI,
			MimeType:    src.MimeType,
			Trust:       src.Trust,
			Coverage:    src.CoverageNotes,
			RetrievedAt: src.RetrievedAt,
		})
	}
	for _, ch := range content.Characters {
		m.Characters = append(m.Characters, CharacterInfo{
			ID:           ch.ID,
			Name:         ch.Name,
			Aliases:      orEmptyJSON(ch.Aliases, "[]"),
			Role:         ch.Role,
			Summary:      ch.PersonalitySummary,
			Visual:       orEmptyJSON(ch.VisualDescriptors, "[]"),
			Status:       ch.Status,
			VerifiedRefs: orEmptyJSON(ch.VerifiedSourceRefs, "[]"),
			Portrait:     nilIfEmpty(ch.PortraitAssetID),
			// The character's approved voice, when this manifest carries it:
			// the cross-reference a reader needs to answer "does this character
			// have a voice" without joining the voices section by character.
			VoiceProfileID: nilIfEmpty(voiceByCharacter[ch.ID]),
		})
	}
	for _, sc := range content.Scenes {
		m.Scenes = append(m.Scenes, SceneInfo{ID: sc.ID, Name: sc.Name, Aliases: orEmptyJSON(sc.Aliases, "[]")})
	}
	for _, rg := range content.Regions {
		m.Regions = append(m.Regions, RegionInfo{
			ID:          rg.ID,
			Label:       rg.Label,
			Kind:        rg.RegionType,
			Detect:      rg.DetectMethod,
			SceneID:     nilIfEmpty(rg.SceneID),
			Rect:        Rect{X: rg.RectX, Y: rg.RectY, W: rg.RectW, H: rg.RectH},
			OCR:         orEmptyJSON(rg.OCRSettings, "{}"),
			MaskAssets:  orEmptyJSON(rg.MaskAssetIDs, "[]"),
			Reliability: rg.Reliability,
			MinAspect:   rg.MinAspect,
			MaxAspect:   rg.MaxAspect,
			MinWidthPx:  rg.MinWidthPx,
			MinHeightPx: rg.MinHeightPx,
		})
	}

	pagesByMessage := map[string][]PageInfo{}
	for _, p := range content.Pages {
		pagesByMessage[p.MessageID] = append(pagesByMessage[p.MessageID], PageInfo{
			Index:        p.Ordinal,
			Text:         strPtr(p.PageText),
			SourcePageID: nilIfEmpty(p.SourcePageID),
			Prefixes:     orEmptyJSON(p.VisiblePrefixes, "[]"),
			Controls:     orEmptyJSON(p.DisplayControls, "[]"),
			Timing:       orEmptyJSON(p.TimingJSON, "{}"),
		})
	}
	for _, msg := range content.Messages {
		info := MessageInfo{
			ID:              msg.ID,
			Source:          nilIfEmpty(msg.SourceID),
			SourceMessageID: nilIfEmpty(msg.SourceMessageID),
			Kind:            msg.TextKind,
			Language:        msg.Language,
			OriginalText:    strPtr(msg.OriginalText),
			NormalizedText:  strPtr(msg.NormalizedText),
			SpeechText:      strPtr(msg.SpeechText),
			SpeakerID:       nilIfEmpty(msg.SpeakerID),
			SceneID:         nilIfEmpty(msg.SceneID),
			Attribution:     AttributionInfo{Status: msg.AttributionStatus, Confidence: msg.AttributionConfidence},
			Markup:          orEmptyJSON(msg.MarkupJSON, "[]"),
			Evidence:        orEmptyJSON(msg.AttributionEvidence, "[]"),
			Flags:           orEmptyJSON(msg.Flags, "{}"),
			Pages:           pagesByMessage[msg.ID],
		}
		if info.Pages == nil {
			info.Pages = []PageInfo{}
		}
		if withhold {
			reason := textWithheldReason(msg.SourceID, textOf)
			if reason != "" {
				redact(&info, reason)
			}
		}
		m.Messages = append(m.Messages, info)
	}
	for _, e := range content.Edges {
		m.MessageEdges = append(m.MessageEdges, EdgeInfo{
			From:       e.FromID,
			To:         e.ToID,
			Type:       e.EdgeType,
			Condition:  e.ConditionExpr,
			Source:     nilIfEmpty(e.SourceID),
			Confidence: e.Confidence,
			Evidence:   orEmptyJSON(e.Evidence, "[]"),
		})
	}
	for _, a := range content.Assertions {
		m.SpeakerAssertions = append(m.SpeakerAssertions, AssertionInfo{
			ID:        a.ID,
			Message:   a.MessageID,
			Character: a.CharacterID,
			Method:    a.Method,
			Posterior: a.Posterior,
			Reviewer:  nilIfEmpty(a.Reviewer),
			Evidence:  orEmptyJSON(a.Evidence, "[]"),
		})
	}

	Stamp(&m)
	if err := m.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("profileio: export %s produced an invalid manifest: %w", profileID, err)
	}
	return m, nil
}

// textWithheldReason reports why a message's text may not be exported, or ""
// when it may. A message with no registered source has no rights record to read
// at all, and docs/spec/14's rule is about *unresolved* rights, so the answer for
// "never asked" is the same as for "asked and unresolved": withhold.
func textWithheldReason(sourceID string, rights map[string]games.RightsStatus) string {
	if sourceID == "" {
		return noSourceReason
	}
	status, ok := rights[sourceID]
	if !ok {
		return noSourceReason
	}
	if !textFieldsPublishable(status) {
		return rightsWithheld
	}
	return ""
}

// noSourceReason is the second redacted_reason code: the message names no source,
// so nothing in the profile says whose text it is.
const noSourceReason = "no_registered_source"

// redact removes every text-bearing field of a message and says why. Structure is
// kept on purpose — a reviewer of a withheld profile still needs to see that the
// line exists, its class, where it sits in the graph and how many pages it has,
// which is what docs/spec/14 means by "shares structured profiles without
// proprietary media/text".
func redact(info *MessageInfo, reason string) {
	null := (*string)(nil)
	info.OriginalText, info.NormalizedText, info.SpeechText = null, null, null
	info.Markup = json.RawMessage("[]")
	for i := range info.Pages {
		info.Pages[i].Text = null
	}
	info.RedactedReason = strPtr(reason)
}

// WriteManifest writes a manifest as canonical JSON plus a newline. It stamps a
// missing digest so a caller cannot accidentally publish an unsigned document,
// and refuses a document whose stamp disagrees, which is the difference between
// "this file was exported" and "this file was edited after it was exported".
func WriteManifest(w io.Writer, m Manifest) error {
	if m.Digest == "" {
		Stamp(&m)
	} else if err := Verify(m); err != nil {
		return err
	}
	raw, err := Encode(m)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = w.Write(raw)
	return err
}

// ExportBundle writes a `.gnpack`: a tar holding the manifest as `manifest.json`
// and each source blob as `blobs/<sha256>`. It requires the copyright
// acknowledgement twice over — in the options, and in the fact that the caller had
// to name the asset store the bytes come from.
//
// The entry naming matches assets.Export's layout (`blobs/<hash>`, `manifest.json`)
// so a pack is readable by the same tooling either way. It is written here rather
// than by calling assets.Export because that method owns the tar writer it creates
// (internal/assets/export.go closes it, and appending entries after a tar's
// end-of-archive marker would hide them), and the pack needs a second entry that
// assets knows nothing about. Blobs still stream through assets.Get with a bounded
// buffer, which is the part that matters for memory.
func ExportBundle(ctx context.Context, deps Deps, profileID string, opts ExportOptions, w io.Writer) (Manifest, error) {
	if deps.Assets == nil {
		return Manifest{}, fmt.Errorf("%w: ExportBundle requires an asset store to read blobs from", ErrInvalid)
	}
	if w == nil {
		return Manifest{}, fmt.Errorf("%w: ExportBundle requires a writer", ErrInvalid)
	}
	opts.IncludeSourceBytes = true
	m, err := Export(ctx, deps.Store, profileID, opts)
	if err != nil {
		return Manifest{}, err
	}

	refs := PackBlobRefs(m)
	// Resolve every ref before the first byte, exactly as assets.Export does: a
	// pack that fails halfway is a corrupt file on the other end of the pipe.
	for _, ref := range refs {
		if _, err := deps.Assets.Stat(ctx, ref); err != nil {
			return Manifest{}, fmt.Errorf("profileio: pack for %s: blob %s: %w", profileID, ref, err)
		}
	}

	tw := tar.NewWriter(w)
	body, err := Encode(m)
	if err != nil {
		tw.Close()
		return Manifest{}, err
	}
	body = append(body, '\n')
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     PackManifestName,
		Size:     int64(len(body)),
		Mode:     0o600,
		// Fixed epoch: the same content must produce the same bytes, so a pack can
		// be hashed and compared the way store-side digests are.
		ModTime: unixEpoch,
		Format:  tar.FormatUSTAR,
	}); err != nil {
		tw.Close()
		return Manifest{}, err
	}
	if _, err := tw.Write(body); err != nil {
		tw.Close()
		return Manifest{}, err
	}
	for _, ref := range refs {
		if err := writeBlob(ctx, deps.Assets, tw, ref); err != nil {
			tw.Close()
			return Manifest{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return Manifest{}, fmt.Errorf("profileio: pack for %s: close tar: %w", profileID, err)
	}
	return m, nil
}

// PackManifestName is the tar entry carrying the profile manifest in a .gnpack.
const PackManifestName = "manifest.json"

// blobsDir mirrors the asset store's own layout so a pack and an
// assets.Export stream are interchangeable at the entry level.
const blobsDir = "blobs"

// blobPath is the tar name of one blob.
func blobPath(sha string) string { return blobsDir + "/" + sha }

// SourceBlobRefs lists the content hashes a manifest points at, deduplicated and
// sorted, skipping manual sources that carry no payload.
func SourceBlobRefs(m Manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, src := range m.Sources {
		if src.SHA256 == "" || seen[src.SHA256] {
			continue
		}
		seen[src.SHA256] = true
		out = append(out, src.SHA256)
	}
	return out
}

// VoiceBlobRefs lists the approved reference WAVs the manifest's voices name,
// deduplicated and sorted. A default export lists them as pointers and withholds
// them; the pack commits them, which is the only way the importing appliance's
// Reconcile pass can register the voices from bytes (ADR-0101).
func VoiceBlobRefs(m Manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range m.Voices {
		if v.ReferenceSHA256 == "" || seen[v.ReferenceSHA256] {
			continue
		}
		seen[v.ReferenceSHA256] = true
		out = append(out, v.ReferenceSHA256)
	}
	return out
}

// PackBlobRefs is the union of the pack's two blob families: the source payloads
// and the voice references. One entry per content hash — a hash the manifest
// names as both a source and a reference travels once.
func PackBlobRefs(m Manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range append(append([]string{}, SourceBlobRefs(m)...), VoiceBlobRefs(m)...) {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// writeBlob streams one committed blob into the pack. Nothing is buffered: the
// copy uses io.Copy over the store's own reader, so a 200 MB dump does not become
// 200 MB of heap.
func writeBlob(ctx context.Context, as assets.Store, tw *tar.Writer, sha string) error {
	r, info, err := as.Get(ctx, sha)
	if err != nil {
		return fmt.Errorf("profileio: pack: blob %s: %w", sha, err)
	}
	defer r.Close()
	if info.SHA256 != sha {
		return fmt.Errorf("profileio: pack: blob %s: %w: store answered for %s", sha, ErrCorruptBlob, info.SHA256)
	}
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     blobPath(sha),
		Size:     info.Size,
		Mode:     0o600,
		ModTime:  unixEpoch,
		Format:   tar.FormatUSTAR,
	}); err != nil {
		return fmt.Errorf("profileio: pack: header %s: %w", sha, err)
	}
	n, err := io.Copy(tw, r)
	if err != nil {
		return fmt.Errorf("profileio: pack: body %s: %w", sha, err)
	}
	if n != info.Size {
		return fmt.Errorf("profileio: pack: %s: %w: copied %d of %d bytes", sha, ErrCorruptBlob, n, info.Size)
	}
	return nil
}

// ErrCorruptBlob reports a blob whose bytes and address disagree mid-read, the
// asset store's Verify failure surfacing at export time.
var ErrCorruptBlob = assets.ErrCorrupt

// DecodeManifest reads a `.gnprofile.json` document: schema validation first, so
// a failure names JSON pointers rather than Go field names, then the structural
// and reference checks, then the digest.
func DecodeManifest(r io.Reader) (Manifest, error) {
	raw, err := io.ReadAll(io.LimitReader(r, int64(MaxManifestBytes)+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("profileio: read manifest: %w", err)
	}
	return DecodeManifestBytes(raw)
}

// DecodeManifestBytes is DecodeManifest for bytes already in hand.
func DecodeManifestBytes(raw []byte) (Manifest, error) {
	if len(raw) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrInvalid, MaxManifestBytes)
	}
	if err := ValidateDocument(raw); err != nil {
		return Manifest{}, err
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	m = m.normalized()
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	if err := Verify(m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

//
// ── small conversion helpers ────────────────────────────────────────────────
//

// nilIfEmpty renders a nullable column as JSON null rather than "", so a
// reviewer can tell "absent" from "deliberately empty" — the distinction the
// schema keeps for the same reason.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return strPtr(s)
}

// nilIfEmptyRfc3339 renders a nullable timestamp column: nil when unset, the
// RFC-3339 instant otherwise (voice approval times travel as written).
func nilIfEmptyRfc3339(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return strPtr(s)
}

// ptrBool renders a bool as a pointer so a manifest field can distinguish an
// explicit value from an absent one (publication.narrator_fallback).
func ptrBool(b bool) *bool { return &b }

// orEmptyJSON keeps a JSON column a JSON value: nil would marshal as null, and
// 0002_content.sql defaults these to [] or {} instead.
func orEmptyJSON(raw json.RawMessage, fallback string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(fallback)
	}
	return raw
}
