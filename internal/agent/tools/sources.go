package tools

// Source tools: import_user_asset, parse_dialogue
// (docs/spec/03 "Tool registry"; docs/spec/14 "Filesystem/import").
//
// Both write draft revisions only, through the idempotent profileio import
// path (reconcile by (source, source_message_id), never duplicate). Neither
// fetches anything from the network: import_user_asset reads a blob the user
// already staged into the asset store, and parse_dialogue re-reads a payload
// a source row points at. Game artwork is explicitly out of scope — no tool
// imports images (task contract, docs/spec/03: "do not auto-publish remote
// art").

import (
	"context"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/profileio"
)

// assetRole is the purpose the user declares for an uploaded file. The
// whitelist doubles as the image veto: an artwork role is refused outright,
// so the onboarding flow can never be steered into importing game images
// (AGENTS.md: "Never fetch or import game images").
var assetRoles = map[string]bool{
	"dialogue_script": true,
	"script":          true,
	"text_dump":       true,
	"metadata":        true,
	"other":           true,
}

var rejectedAssetRoles = map[string]bool{
	"artwork": true, "cover": true, "image": true, "icon": true,
	"portrait": true, "screenshot": true,
}

// safeMediaPrefixes are the media types an import will touch. Everything
// else — executables, archives, images, octet-stream — is refused before a
// parser sees the bytes (docs/spec/14 "safe file handling").
var safeMediaPrefixes = []string{
	"text/",
	"application/json",
	"application/csv",
	"application/x-csv",
	"application/ndjson",
	"application/x-ndjson",
}

// unsafeMagic prefixes identify executable and archive formats the media
// type alone might miss (a .csv that is really a PE header).
var unsafeMagic = []struct {
	prefix []byte
	label  string
}{
	{[]byte{0x4d, 0x5a}, "PE/DOS executable"},
	{[]byte{0x7f, 0x45, 0x4c, 0x46}, "ELF executable"},
	{[]byte{0xca, 0xfe, 0xba, 0xbe}, "Mach-O executable"},
	{[]byte{0xca, 0xfe, 0xd0, 0x0e}, "Mach-O executable"},
	{[]byte{'P', 'K', 0x03, 0x04}, "ZIP archive"},
	{[]byte{0x1f, 0x8b}, "gzip stream"},
}

func mediaAllowed(mediaType string, head []byte) error {
	for _, p := range unsafeMagic {
		if len(head) >= len(p.prefix) && strings.HasPrefix(string(head), string(p.prefix)) {
			return fmt.Errorf("refusing %s content (docs/spec/14: executables and archives are not importable)", p.label)
		}
	}
	for _, p := range safeMediaPrefixes {
		if strings.HasPrefix(strings.ToLower(mediaType), p) {
			return nil
		}
	}
	return fmt.Errorf("media type %q is not an importable text format; executables, archives and images are refused", mediaType)
}

// importUserAsset runs one staged upload through the profileio import path.
func (d Deps) importUserAsset(ctx context.Context, call agent.Call) (agent.Result, error) {
	m, err := parseArgs(call)
	if err != nil {
		return agent.Result{}, err
	}
	profileID, err := require(m, "profile_id")
	if err != nil {
		return agent.Result{}, err
	}
	sha := strArg(m, "staged_asset_sha256")
	if sha == "" {
		return agent.Result{}, fmt.Errorf("argument %q is required", "staged_asset_sha256")
	}
	if !assets.ValidHash(sha) {
		return agent.Result{}, fmt.Errorf("argument %q is not a 64-hex-char SHA-256", "staged_asset_sha256")
	}
	role := strArg(m, "role")
	if rejectedAssetRoles[role] {
		return agent.Result{}, fmt.Errorf("role %q is refused: game artwork is never imported by the onboarding agent (AGENTS.md); propose candidates with propose_artwork instead", role)
	}
	if !assetRoles[role] {
		return agent.Result{}, fmt.Errorf("role %q is not a known asset role (dialogue_script, script, text_dump, metadata, other)", role)
	}
	rights, err := require(m, "rights")
	if err != nil {
		return agent.Result{}, err
	}
	rightsStatus, err := games.ParseRightsStatus(rights)
	if err != nil {
		return agent.Result{}, fmt.Errorf("argument %q: %v", "rights", err)
	}
	if rightsStatus == games.RightsUnknown {
		return agent.Result{}, fmt.Errorf("rights %q is not a position the user can hold for a file they supplied: record user_supplied or licensed (docs/spec/01 invariant 6)", rights)
	}
	fileName := strArg(m, "file_name")
	if fileName == "" {
		fileName = fmt.Sprintf("staged-%s.txt", sha[:12])
	}

	// Read the staged blob. Get opens by content hash; Verify re-hashes so a
	// corrupted store cannot feed a parser bytes that do not match the
	// provenance the source row will record. The media check reads the head
	// off one stream; ImportFile gets a fresh stream from the same hash, so
	// the parser sees the file from byte zero.
	blob, info, err := d.Assets.Get(ctx, sha)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: import_user_asset: %w", err)
	}
	head := make([]byte, 512)
	n, _ := blob.Read(head)
	head = head[:n]
	_ = blob.Close()
	if err := d.Assets.Verify(ctx, sha); err != nil {
		return agent.Result{}, fmt.Errorf("tools: import_user_asset: verify blob: %w", err)
	}
	if err := mediaAllowed(info.MediaType, head); err != nil {
		return agent.Result{}, err
	}
	stream, _, err := d.Assets.Get(ctx, sha)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: import_user_asset: re-open blob: %w", err)
	}
	defer stream.Close()

	report, err := profileio.ImportFile(ctx, profileio.Deps{
		Store: d.Store, Assets: d.Assets, Adapters: d.Adapters,
	}, profileio.ImportRequest{
		ProfileID: profileID,
		FileName:  fileName,
		Reader:    stream,
		Rights:    rightsStatus,
		Trust:     1,
	})
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: import_user_asset: %w", err)
	}

	// Scenario-7 seam: simulate a process crash after the import's writes but
	// before the durable tool-outcome row (see Deps.CrashSim).
	if d.CrashSim != nil {
		d.CrashSim()
	}

	out := map[string]any{
		"profile_id":         profileID,
		"source_id":          report.SourceID,
		"sha256":             report.SHA256,
		"size":               report.Size,
		"media_type":         report.MediaType,
		"adapter":            report.AdapterID,
		"format":             report.Format,
		"role":               role,
		"rights":             rights,
		"stats":              report.Stats,
		"inserted":           report.Inserted,
		"updated":            report.Updated,
		"reused_source":      report.ReusedSource,
		"characters":         report.Characters,
		"speakers":           report.Speakers,
		"warnings":           report.Warnings,
		"warnings_truncated": report.Truncated,
		"coverage_note":      "source is incomplete by default until strong evidence confirms otherwise (docs/spec/03)",
	}
	return okResult(out, report.SourceID)
}

// parseDialogue re-runs the adapter registry over an already-registered
// source, reconciling the parse into the draft profile. It is the tool for
// "the file registered but the parser choice was wrong" — and it is safe to
// repeat: the same bytes reconcile to the same rows (docs/spec/02
// "reconcile, not duplicate").
func (d Deps) parseDialogue(ctx context.Context, call agent.Call) (agent.Result, error) {
	m, err := parseArgs(call)
	if err != nil {
		return agent.Result{}, err
	}
	profileID, err := require(m, "profile_id")
	if err != nil {
		return agent.Result{}, err
	}
	sourceID, err := require(m, "source_id")
	if err != nil {
		return agent.Result{}, err
	}
	parserID := strArg(m, "parser_id")

	sources, err := d.Store.ListSources(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: parse_dialogue: list sources: %w", err)
	}
	var src games.Source
	for _, s := range sources {
		if s.ID == sourceID {
			src = s
			break
		}
	}
	if src.ID == "" {
		return agent.Result{}, fmt.Errorf("source %s is not registered on profile %s", sourceID, profileID)
	}
	if src.SourceType != games.SourceUserFile {
		return agent.Result{}, fmt.Errorf("source %s is %s: this appliance keeps no payload for %s sources; import the bytes with import_user_asset (docs/spec/14: fetched pages are not re-parsed as text dumps)", src.ID, src.SourceType, src.SourceType)
	}
	if src.ContentSHA256 == "" {
		return agent.Result{}, fmt.Errorf("source %s has no content hash; there is nothing to parse", src.ID)
	}

	blob, _, err := d.Assets.Get(ctx, src.ContentSHA256)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: parse_dialogue: read blob: %w", err)
	}
	defer blob.Close()
	head := make([]byte, 512)
	n, _ := blob.Read(head)
	head = head[:n]

	// parser_id selects either a registered adapter (verified against
	// detection BEFORE any write) or a source format to force.
	format := ""
	if parserID != "" {
		if _, ok := d.Adapters.Get(parserID); ok {
			match, found := d.Adapters.DetectOne(ctx, "source", head)
			if !found {
				return agent.Result{}, fmt.Errorf("adapter %s does not claim these bytes (nothing registered matched)", parserID)
			}
			if match.Adapter.Manifest().ID != parserID {
				return agent.Result{}, fmt.Errorf("adapter %s does not claim these bytes (detection picked %q)", parserID, match.Adapter.Manifest().ID)
			}
		} else {
			switch parserID {
			case "csv", "tsv", "json", "jsonl", "text":
				format = parserID
			default:
				return agent.Result{}, fmt.Errorf("parser_id %q is neither a registered adapter nor a known format (csv, tsv, json, jsonl, text)", parserID)
			}
		}
	}

	// Re-read from the start: the detection peek consumed the head bytes.
	blob2, _, err := d.Assets.Get(ctx, src.ContentSHA256)
	if err != nil {
		return agent.Result{}, err
	}
	defer blob2.Close()

	report, err := profileio.ImportFile(ctx, profileio.Deps{
		Store: d.Store, Assets: d.Assets, Adapters: d.Adapters,
	}, profileio.ImportRequest{
		ProfileID: profileID,
		FileName:  fileNameFromURI(src.URI),
		Reader:    blob2,
		Format:    format,
		Rights:    src.Rights,
		Trust:     src.Trust,
	})
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: parse_dialogue: %w", err)
	}
	out := map[string]any{
		"profile_id":    profileID,
		"source_id":     report.SourceID,
		"parser":        report.AdapterID,
		"format":        report.Format,
		"stats":         report.Stats,
		"inserted":      report.Inserted,
		"updated":       report.Updated,
		"reused_source": report.ReusedSource,
		"warnings":      report.Warnings,
		"note":          "re-import reconciles by (source, source_message_id): no duplicates on repeat",
	}
	return okResult(out, report.SourceID)
}

// fileNameFromURI recovers the file name a provenance URI recorded.
func fileNameFromURI(uri string) string {
	base := strings.TrimPrefix(uri, "file://")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if base == "" || base == "." || base == "/" {
		base = "source"
	}
	return base
}
