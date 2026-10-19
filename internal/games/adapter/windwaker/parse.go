package windwaker

// parse.go maps a parsed BMG file onto the canonical adapter.ParseResult:
// the message rows, their counts, the speaker sidecar (ADR-0070), the
// flow-graph decision and the rules revision marker.
//
// The mapping is the contract the rest of the system relies on (docs/spec/02):
//
//   - SourceMessageID is the BMG message id as a decimal string: stable
//     across re-imports and the key the unique index
//     messages_source_identity_idx reconciles on;
//   - OriginalText is the lossless escape of the raw pool bytes (escape.go),
//     so re-encoding it yields the exact bytes the source hash names;
//   - MarkupJSON is the token AST with offsets (render.go);
//   - NormalizedText is textnorm of the text runs with tags removed and line
//     breaks as spaces;
//   - SpeechText is the text runs with the rule table applied;
//   - Speakers/SpeakerKeys/SpeakerColumn carry the dump's own line-to-speaker
//     linkage as opaque keys (speakers.go) — the one source of speaker truth
//     this file has, measured in, not guessed from, the user's own disc.

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/bmg"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/gcdisc"
)

// bmgResPath is where the user's disc keeps its message archive (the path
// validated against the real image in ADR-0029's real-file test).
const bmgResPath = "res/Msg/bmgres.arc"

// DiscRegion maps a disc's game id to the edition's region tag. The mapping
// itself is a fact about the GameCube id format (the fourth character is the
// region slot), which the gcdisc package reports; this wrapper is where the
// CLI and the import job read it from the adapter's vocabulary.
func DiscRegion(gameID string) string { return gcdisc.RegionOfGameID(gameID) }

// ExtractBMG reads the first .bmg entry of the disc's message archive.
// Errors are descriptive: which file was missing, not "parse failed".
func ExtractBMG(d *gcdisc.Disc) ([]byte, error) {
	dfs, err := d.FS()
	if err != nil {
		return nil, fmt.Errorf("windwaker: disc filesystem: %w", err)
	}
	arcBytes, err := fs.ReadFile(dfs, bmgResPath)
	if err != nil {
		return nil, fmt.Errorf("windwaker: no message archive at %s: %w", bmgResPath, err)
	}
	arc, err := gcdisc.OpenRARC(arcBytes)
	if err != nil {
		return nil, fmt.Errorf("windwaker: open RARC %s: %w", bmgResPath, err)
	}
	// First .bmg in sorted walk order: deterministic across runs (map
	// iteration is not), and the user's archive holds exactly one.
	var bmgPath string
	err = fs.WalkDir(arc, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".bmg") && bmgPath == "" {
			bmgPath = p
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("windwaker: walk RARC %s: %w", bmgResPath, err)
	}
	if bmgPath == "" {
		return nil, fmt.Errorf("windwaker: no .bmg entry in %s", bmgResPath)
	}
	data, err := fs.ReadFile(arc, bmgPath)
	if err != nil {
		return nil, fmt.Errorf("windwaker: read %s from RARC: %w", path.Base(bmgPath), err)
	}
	return data, nil
}

// parseBMG is the shared core of Parse and ParseDisc.
func (a Adapter) parseBMG(ctx context.Context, data []byte, opts adapter.ParseOptions) (*adapter.ParseResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts, err := opts.Normalize()
	if err != nil {
		return nil, err
	}
	file, err := bmg.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("windwaker: %w", err)
	}
	if int(len(file.Messages)) > opts.MaxRows {
		return nil, fmt.Errorf("windwaker: %w: %d messages exceed the %d-row limit", adapter.ErrTooManyRows, len(file.Messages), opts.MaxRows)
	}
	result, err := a.mapFile(ctx, file, opts)
	if err != nil {
		return nil, err
	}
	if len(result.Messages) == 0 {
		// An archive that parses but yields no message is a parse, not an
		// import; the importer reports ErrNoMessages instead of writing a
		// profile with zero rows (adapter.ErrNoMessages contract).
		return nil, fmt.Errorf("%w: %d entries, %d skipped", adapter.ErrNoMessages, len(file.Messages), result.Stats.Skipped)
	}
	return result, nil
}

// mapFile walks every BMG entry and builds the parse result. The order is
// the INF1 table order (the pool's own numbering), which is what re-import
// reconciliation and the UI listing both assume.
func (a Adapter) mapFile(ctx context.Context, file *bmg.File, opts adapter.ParseOptions) (*adapter.ParseResult, error) {
	rules := a.rulesOrDefault()
	result := &adapter.ParseResult{}
	result.Warnings = append(result.Warnings, a.rulesWarning())

	// Graph decision: no edges unless the flow sections decode, which as of
	// this revision they do not (graph.go). The note is recorded so the
	// coverage column answers "is there a graph, and why not".
	if _, decoded := flowEdges(file); !decoded {
		result.Warnings = append(result.Warnings, adapter.Warning{
			Line:   0,
			Column: "graph",
			Msg:    GraphNote,
		})
	}

	// One stamp for every row of this parse: the generic adapter does the
	// same, and a parse that minted its rows at different instants would
	// give the revision a created_at scatter that no clock could explain.
	stamp := time.Now().UTC().Format(time.RFC3339)

	result.Stats.Rows = len(file.Messages)
	// Speaker sidecar (ADR-0070): the distinct opaque ids in first-occurrence
	// order, and the per-row key parallel to Messages. The evidence is shared
	// per key so every row of a speaker carries the same provenance.
	speakerIdx := map[string]int{}
	speakerEvidenceCache := map[string]json.RawMessage{}
	for _, m := range file.Messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		original, normalized, speech, markup, err := messageTexts(m, rules, opts.TextNorm)
		if err != nil {
			return nil, fmt.Errorf("windwaker: message %d: %w", m.ID, err)
		}
		if normalized == "" {
			// Empty or system-looking entry: skipped with a count, never a
			// row (games.Message.Validate forbids the empty normalizedText
			// such a row would carry). A speaker whose only row was skipped
			// must not become an unresolvable roster entry, so it is not
			// registered either.
			result.Stats.Skipped++
			continue
		}
		if len([]rune(original)) > maxOriginalLen || len([]rune(normalized)) > maxOriginalLen {
			// The message exceeds the stored-column bound: skip with a
			// warning rather than failing the whole import (this disc has
			// none; a hostile or different BMG must not wedge the import).
			result.Warnings = append(result.Warnings, adapter.Warning{
				Line:   0,
				Column: fmt.Sprintf("message %d", m.ID),
				Msg:    fmt.Sprintf("skipped: original text exceeds %d characters", maxOriginalLen),
			})
			result.Stats.Skipped++
			continue
		}
		// The speaker slot (ADR-0070): the INF1 attribute byte is the dump's
		// own line-to-speaker linkage. It is an opaque identifier, not a name
		// the source vouches for, so the row leaves the parse proposed — the
		// importer binds it to a placeholder character and records the dump's
		// claim as a dump_metadata assertion; a curator's link is what
		// confirms the identity. An entry without the attribute byte (this
		// layout's bound) degrades to "no speaker", never a guess. The key is
		// folded the way message text is (the interface's Key contract), which
		// keeps a re-import's roster match exact under the importer's own
		// textnorm options.
		key := speakerKeyOf(m)
		if key != "" {
			key = textnorm.Normalize(key, opts.TextNorm)
		}
		row := games.Message{
			ID:                games.NewID(),
			ProfileID:         opts.ProfileID,
			SourceID:          opts.SourceID,
			SourceMessageID:   fmt.Sprintf("%d", m.ID),
			OriginalText:      original,
			MarkupJSON:        markup,
			NormalizedText:    normalized,
			SpeechText:        speech,
			Language:          opts.Language,
			TextKind:          textKind(),
			AttributionStatus: games.AttributionUnknown,
			Revision:          1,
			CreatedAt:         stamp,
			UpdatedAt:         stamp,
		}
		if key != "" {
			evidence, ok := speakerEvidenceCache[key]
			if !ok {
				evidence = speakerEvidence(opts.SourceID, key)
				speakerEvidenceCache[key] = evidence
			}
			row.AttributionStatus = games.AttributionProposed
			row.AttributionConfidence = 1.0
			row.AttributionEvidence = evidence
		}
		if err := row.Validate(); err != nil {
			// A row the adapter minted that still fails validation is a bug
			// in the mapping, not a property of the source: fail loudly so
			// the bug is found here, not as a constraint error mid-import.
			return nil, fmt.Errorf("windwaker: message %d failed validation: %w", m.ID, err)
		}
		result.Messages = append(result.Messages, row)
		result.SpeakerKeys = append(result.SpeakerKeys, key)
		if key != "" {
			if _, seen := speakerIdx[key]; !seen {
				speakerIdx[key] = len(result.Speakers)
				result.Speakers = append(result.Speakers, adapter.SpeakerRef{
					Key:         key,
					DisplayName: key, // the game data names no speaker; the id is the label
					SourceNamed: false,
				})
			}
		}
		result.Stats.Imported++
	}
	if len(result.Speakers) > 0 {
		result.SpeakerColumn = SpeakerColumn
		// A deterministic provenance marker (the rules-revision pattern):
		// counts only, so the import report can answer "how many distinct
		// speakers, and where did the attribution come from" without a single
		// line of game text (AGENTS.md rule 5).
		result.Warnings = append(result.Warnings, adapter.Warning{
			Line:   0,
			Column: "speakers",
			Msg: fmt.Sprintf("speaker attribution from %s: %d distinct opaque ids across %d imported rows; the source names no speaker, so each id becomes a placeholder a curator links (ADR-0070)",
				SpeakerColumn, len(result.Speakers), result.Stats.Imported),
		})
	}
	return result, nil
}

// tagCoverage reports how many of the file's tag occurrences have an explicit
// rule entry, as (covered, total). The real-file test asserts the share is at
// least 0.95 and logs it: the table is the evidence, and a drop in coverage
// means new (group,type) pairs appeared that this revision has not classified.
func tagCoverage(file *bmg.File, rules *Rules) (covered, total int) {
	for _, m := range file.Messages {
		for _, t := range m.Tokens {
			if t.Kind == bmg.TokText {
				continue
			}
			total++
			if _, ok := rules.entries[[2]int{t.Group, t.Type}]; ok {
				covered++
			}
		}
	}
	return covered, total
}

// tagPairs is the sorted distinct (group,type) pairs of a file, for reports
// that must be stable across runs.
func tagPairs(file *bmg.File) [][2]int {
	seen := map[[2]int]bool{}
	var out [][2]int
	for _, m := range file.Messages {
		for _, t := range m.Tokens {
			if t.Kind == bmg.TokText {
				continue
			}
			p := [2]int{t.Group, t.Type}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}
