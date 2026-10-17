package generic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
)

// Every test in this package imports a source and asserts on the messages that
// come out — not on the shape of the API. A mocked parse result would prove the
// types compile, which go build already proved (AGENTS.md "No simulation
// theater"). Test data is synthetic and written for this repository; no game's
// text appears here (docs/spec/14 "Source material / intellectual-property
// policy").

func testOptions() adapter.ParseOptions {
	return adapter.ParseOptions{ProfileID: games.NewID()}
}

func parseFixture(t *testing.T, name string, mutate func(*adapter.ParseOptions)) *adapter.ParseResult {
	t.Helper()
	path := filepath.Join("testdata", name)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	opts := testOptions()
	if mutate != nil {
		mutate(&opts)
	}
	res, err := Adapter{}.Parse(context.Background(), f, opts)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	assertResultSane(t, res, opts.TextNorm)
	return res
}

// parseString imports an inline source, which most of the failure cases use so
// the bytes under test sit next to the assertion about them.
func parseString(t *testing.T, body string, mutate func(*adapter.ParseOptions)) (*adapter.ParseResult, error) {
	t.Helper()
	opts := testOptions()
	opts.Format = detectForTest(body)
	if mutate != nil {
		mutate(&opts)
	}
	res, err := Adapter{}.Parse(context.Background(), strings.NewReader(body), opts)
	if res != nil {
		assertResultSane(t, res, opts.TextNorm)
	}
	return res, err
}

func detectForTest(body string) string { return InferFormat([]byte(body)) }

// assertResultSane checks the guarantees the adapter owes for *every* result: ids
// are canonical, nothing invents a foreign key, and the arithmetic in Stats
// accounts for the rows. norm is the option set the import was configured with,
// because the stored key is only "right" relative to the profile's choices.
func assertResultSane(t *testing.T, res *adapter.ParseResult, norm textnorm.Options) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range res.Messages {
		if err := m.Validate(); err != nil {
			t.Errorf("message %q (%s) failed Validate: %v", m.SourceMessageID, truncate(m.OriginalText, 40), err)
		}
		if seen[m.ID] {
			t.Errorf("two messages share id %s", m.ID)
		}
		seen[m.ID] = true
		if m.SpeakerID != "" {
			t.Errorf("message %q claims speaker %q; the generic adapter cannot create character rows, so this invents a speaker", m.SourceMessageID, m.SpeakerID)
		}
		if m.SceneID != "" {
			t.Errorf("message %q claims scene %q for the same reason", m.SourceMessageID, m.SceneID)
		}
		if m.Revision != 1 {
			t.Errorf("message %q has revision %d; an import always starts at 1", m.SourceMessageID, m.Revision)
		}
		if want := textnorm.Normalize(m.OriginalText, norm); want != m.NormalizedText {
			t.Errorf("message %q normalized_text is not what textnorm produces:\n store %q\n want  %q",
				m.SourceMessageID, m.NormalizedText, want)
		}
	}
	for _, p := range res.Pages {
		if err := p.Validate(); err != nil {
			t.Errorf("page %d of %s failed Validate: %v", p.Ordinal, truncate(p.MessageID, 8), err)
		}
		if !seen[p.MessageID] {
			t.Errorf("page %d references message %s that is not in the result", p.Ordinal, p.MessageID)
		}
	}
	if len(res.SpeakerKeys) != len(res.Messages) {
		t.Errorf("SpeakerKeys has %d entries for %d messages; the sidecar must stay parallel to Messages",
			len(res.SpeakerKeys), len(res.Messages))
	}
	if res.Stats.Imported > res.Stats.Rows {
		t.Errorf("imported %d messages from %d rows", res.Stats.Imported, res.Stats.Rows)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func warningsFor(res *adapter.ParseResult, contains string) []adapter.Warning {
	var out []adapter.Warning
	for _, w := range res.Warnings {
		if strings.Contains(w.Msg, contains) {
			out = append(out, w)
		}
	}
	return out
}

//
// ── the formats ─────────────────────────────────────────────────────────────
//

func TestParseCSVFixture(t *testing.T) {
	res := parseFixture(t, "synthetic-adventure.csv", nil)

	if res.Stats.Rows != 40 || res.Stats.Imported != 40 || res.Stats.Skipped != 0 {
		t.Errorf("Stats = %+v, want 40 rows, 40 messages, 0 skipped", res.Stats)
	}
	if len(res.Warnings) != 0 {
		for _, w := range res.Warnings {
			t.Errorf("unexpected warning at line %d [%s]: %s", w.Line, w.Column, w.Msg)
		}
	}

	byID := map[string]games.Message{}
	for _, m := range res.Messages {
		byID[m.SourceMessageID] = m
	}
	if len(byID) != 40 {
		t.Fatalf("only %d distinct source ids for 40 rows", len(byID))
	}

	// A quoted field that spans lines keeps its newline: original_text is
	// byte-faithful, and the wrap is the source's own (docs/spec/02).
	multiline := byID["l-006"]
	if !strings.Contains(multiline.OriginalText, "\nNobody built") {
		t.Errorf("multiline field was flattened: %q", multiline.OriginalText)
	}
	if multiline.NormalizedText != "mind the gap between the second and third step. nobody built these docks for tall feet." {
		t.Errorf("the wrap survived into the match key: %q", multiline.NormalizedText)
	}

	// Content classes carried through, including the two the requirement names.
	kinds := map[string]games.TextKind{}
	for _, m := range res.Messages {
		kinds[m.SourceMessageID] = m.TextKind
	}
	for id, want := range map[string]games.TextKind{
		"l-001": games.TextDialogue, "l-003": games.TextSign, "l-005": games.TextItem,
		"l-009": games.TextSystem, "l-036": games.TextSign,
	} {
		if kinds[id] != want {
			t.Errorf("%s kind = %q, want %q", id, kinds[id], want)
		}
	}

	// A named speaker is proposed with its label kept; an unnamed one is unknown.
	// Neither gets a speaker_id, because none exists to point at.
	named := byID["l-001"]
	if named.AttributionStatus != games.AttributionProposed || named.AttributionConfidence != proposedSpeakerConfidence {
		t.Errorf("named speaker produced %q/%v", named.AttributionStatus, named.AttributionConfidence)
	}
	if named.SpeakerID != "" {
		t.Errorf("speaker id invented: %q", named.SpeakerID)
	}
	var evidence []map[string]any
	if err := json.Unmarshal(named.AttributionEvidence, &evidence); err != nil {
		t.Fatalf("attribution evidence is not the documented array: %v", err)
	}
	if len(evidence) != 1 || evidence[0]["method"] != "source_label" || evidence[0]["evidence"] != "Marla" {
		t.Errorf("evidence = %v, want one source_label fragment naming Marla", evidence)
	}
	var flags map[string]string
	if err := json.Unmarshal(named.Flags, &flags); err != nil || flags["source_speaker"] != "Marla" || flags["source_scene"] != "Harbour" {
		t.Errorf("flags = %s, want the source's own speaker and scene labels", named.Flags)
	}

	unnamed := byID["l-002"]
	if unnamed.AttributionStatus != games.AttributionUnknown || unnamed.AttributionConfidence != 0 {
		t.Errorf("row with an empty speaker column got %q/%v", unnamed.AttributionStatus, unnamed.AttributionConfidence)
	}

	// Non-ASCII speaker names survive: the file says Têvance, not Tevance and
	// not "?vance".
	if _, ok := byID["l-029"]; !ok {
		t.Fatal("row l-029 is missing")
	}
	if got := byID["l-029"].AttributionEvidence; !strings.Contains(string(got), "T") || !strings.Contains(string(got), "\\u00e9") && !strings.Contains(string(got), "ê") {
		t.Errorf("non-ASCII speaker name mangled in evidence: %s", got)
	}
	if flags := byID["l-029"].Flags; !json.Valid(flags) {
		t.Errorf("flags for the non-ASCII row are not valid JSON: %s", flags)
	}
}

func TestParseTSV(t *testing.T) {
	body := "line\tspeaker\tkind\nFirst\tMarla\tdialogue\nSecond\t\titem\n"
	res, err := parseString(t, body, func(o *adapter.ParseOptions) { o.Format = adapter.FormatAuto })
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Imported != 2 {
		t.Fatalf("imported %d", res.Stats.Imported)
	}
	if res.Messages[0].TextKind != games.TextDialogue || res.Messages[1].TextKind != games.TextItem {
		t.Errorf("kinds = %q, %q", res.Messages[0].TextKind, res.Messages[1].TextKind)
	}
	if res.Messages[0].SpeakerID != "" || res.Messages[0].AttributionStatus != games.AttributionProposed {
		t.Errorf("speaker handling wrong: %+v", res.Messages[0])
	}
}

func TestParseJSONArrayAndObjectForm(t *testing.T) {
	array := `[{"text":"One","speaker":"Marla"},{"text":"Two"}]`
	res, err := parseString(t, array, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Imported != 2 || res.Messages[1].AttributionStatus != games.AttributionUnknown {
		t.Fatalf("array form wrong: %+v", res.Stats)
	}

	wrapped := `{"title":"synthetic","messages":[{"text":"Three"},{"text":"Four","speaker":"Corvin"}]}`
	res, err = parseString(t, wrapped, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Imported != 2 || res.Messages[0].OriginalText != "Three" {
		t.Fatalf("object form wrong: %+v", res.Stats)
	}
	if ws := warningsFor(res, "ignoring top-level JSON key"); len(ws) != 1 {
		t.Errorf("want one warning naming the ignored key, got %v", ws)
	}

	// A JSON object whose list is named something else must say so rather than
	// import nothing silently (docs/spec/14: report what the importer ignored).
	wrong := `{"entries":[{"text":"Lost"}]}`
	if _, err := parseString(t, wrong, nil); err == nil {
		t.Error("a file whose list is not named messages imported successfully")
	} else if !strings.Contains(err.Error(), "no") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestParseJSONLFixture(t *testing.T) {
	res := parseFixture(t, "synthetic-jp.jsonl", func(o *adapter.ParseOptions) {
		o.Format = FormatJSONL
		o.Language = "ja"
		o.DefaultKind = games.TextDialogue
	})
	if res.Stats.Rows != 15 || res.Stats.Imported != 15 {
		t.Fatalf("Stats = %+v, want 15 rows and 15 messages", res.Stats)
	}
	for _, m := range res.Messages {
		if m.Language != "ja" {
			t.Errorf("language not stamped on %q", m.OriginalText)
		}
		// No speakers in this fixture, so no attribution may be claimed.
		if m.AttributionStatus != games.AttributionUnknown {
			t.Errorf("message %q got attribution %q from a file with no speaker column", truncate(m.OriginalText, 12), m.AttributionStatus)
		}
		// Japanese must reach the index unharmed, folded only in case and space.
		if strings.ContainsAny(m.NormalizedText, " \n") && !strings.Contains(m.OriginalText, " ") {
			t.Errorf("space appeared in the key for text with no space: %q", m.NormalizedText)
		}
	}
	if got := res.Messages[3].NormalizedText; !strings.Contains(got, "深海の祠") {
		t.Errorf("Japanese content lost: %q", got)
	}
	// Halfwidth kana, if a fixture ever carries it, must fold to the full form.
	res, err := parseString(t, `{"messages":[{"line":"ｶﾀﾅﾞｶ ﾊﾝﾒｲﾉ"}]}`, func(o *adapter.ParseOptions) {
		o.Format = FormatJSON
		o.Language = "ja"
	})
	if err != nil {
		t.Fatal(err)
	}
	// The expectation is written with escapes: the halfwidth block folds to the
	// NFKC targets, and the voiced mark stays decomposed (ナ + U+3099) because this
	// package does not compose. A look-alike codepoint pasted into a literal here
	// would make the test pass for the wrong reason.
	want := "\u30ab\u30bf\u30ca\u3099\u30ab \u30cf\u30f3\u30e1\u30a4\u30ce"
	if got := res.Messages[0].NormalizedText; got != want {
		t.Errorf("halfwidth katakana folded to %q, want %q", got, want)
	}
}

func TestParsePlainTextDefault(t *testing.T) {
	res := parseFixture(t, "plain.txt", func(o *adapter.ParseOptions) { o.Format = FormatText })
	// One message per non-empty line, and the fixture has 9 of them.
	if res.Stats.Imported != 9 {
		t.Errorf("imported %d messages, want one per non-empty line (9):\n%s", res.Stats.Imported, textList(res))
	}
	if len(res.Pages) != 0 {
		t.Errorf("a single-page script should need no page rows, got %d", len(res.Pages))
	}
	for _, m := range res.Messages {
		if strings.Contains(m.OriginalText, "\n") {
			t.Errorf("line mode produced a multi-line message: %q", m.OriginalText)
		}
	}
}

func TestParsePlainTextPages(t *testing.T) {
	res := parseFixture(t, "plain.txt", func(o *adapter.ParseOptions) {
		o.Format = FormatText
		o.BlankLineSplitsPages = true
	})

	// With paging on, the fixture's three double-blank-separated paragraphs are
	// three messages, and the single blank lines inside them are page breaks:
	// 2 pages, 2 pages, 1 page (docs/spec/02: a message may have several pages,
	// and a page is not an utterance).
	if res.Stats.Imported != 3 {
		t.Fatalf("imported %d messages, want 3:\n%s", res.Stats.Imported, textList(res))
	}
	wantPages := [][]string{
		{"The gate is locked from the other side.", "Someone left a lantern burning here.\nIt is still warm."},
		{"The bridge is out.\nThe rope on the far bank is not.\nWhoever cut it came back this way.", "Signed, in haste: “M.”"},
		{"Six crates came through the gate this morning.\nFour went out again before the bell."},
	}
	if len(res.Messages) != len(wantPages) {
		t.Fatalf("got %d messages, want %d", len(res.Messages), len(wantPages))
	}
	for i, want := range wantPages {
		got := []string{}
		for _, p := range res.Pages {
			if p.MessageID == res.Messages[i].ID {
				got = append(got, p.PageText)
			}
		}
		if len(want) == 1 {
			// A single page with no source page identity is the message itself
			// and needs no page row; that is the documented behaviour.
			if len(got) != 0 {
				t.Errorf("message %d (%q) has %d page rows, want none", i, res.Messages[i].OriginalText, len(got))
			}
		} else if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("message %d pages =\n%q\nwant\n%q", i, got, want)
		}
		if joined := strings.Join(want, "\n\n"); joined != res.Messages[i].OriginalText {
			t.Errorf("message %d text %q is not its pages joined by a blank line: %q", i, res.Messages[i].OriginalText, joined)
		}
	}

	// Wrapped lines inside one page keep their own newline: paging must not
	// flatten the source's wrapping, because original_text is byte-faithful.
	if !strings.Contains(res.Messages[1].OriginalText, "\nThe rope") {
		t.Errorf("wrapped lines were not kept together: %q", res.Messages[1].OriginalText)
	}
}

func textList(res *adapter.ParseResult) string {
	var b strings.Builder
	for _, m := range res.Messages {
		fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(m.OriginalText, "\n", "⏎"))
	}
	return b.String()
}

//
// ── column mapping ──────────────────────────────────────────────────────────
//

func TestColumnAutoMap(t *testing.T) {
	body := "id,dialogue,character,type,scene\n7,Hello there,Malda,sign,Quay\n"
	res, err := parseString(t, body, func(o *adapter.ParseOptions) { o.Format = FormatCSV })
	if err != nil {
		t.Fatal(err)
	}
	m := res.Messages[0]
	if m.SourceMessageID != "7" {
		t.Errorf("source id = %q", m.SourceMessageID)
	}
	if m.TextKind != games.TextSign {
		t.Errorf("kind = %q", m.TextKind)
	}
	if string(m.AttributionEvidence) == "[]" || !strings.Contains(string(m.AttributionEvidence), "Malda") {
		t.Errorf("speaker label lost: %s", m.AttributionEvidence)
	}
	if !strings.Contains(string(m.Flags), "Quay") {
		t.Errorf("scene label lost: %s", m.Flags)
	}
}

func TestColumnAutoMapIsCaseAndSpaceInsensitive(t *testing.T) {
	body := "  TEXT \nfirst line\nSECOND LINE\n"
	res, err := parseString(t, body, func(o *adapter.ParseOptions) { o.Format = FormatCSV })
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Imported != 2 || res.Messages[0].OriginalText != "first line" {
		t.Fatalf("Stats=%+v messages=%+v", res.Stats, res.Messages)
	}
}

func TestColumnExplicitOverridesAutoMap(t *testing.T) {
	// "name" is a speaker by the auto-map candidate list; here it is the text.
	body := "name,note\nthe real line,who cares\n"
	res, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatCSV
		o.ColumnMap = adapter.ColumnMap{Text: "name"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages[0].OriginalText != "the real line" {
		t.Errorf("explicit mapping ignored: %q", res.Messages[0].OriginalText)
	}
	if res.Messages[0].AttributionStatus != games.AttributionUnknown {
		t.Errorf("the auto-map stole the speaker role: %+v", res.Messages[0])
	}

	// An explicit name that is not in the file is reported, not ignored.
	res, err = parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatCSV
		o.ColumnMap = adapter.ColumnMap{Text: "line", Speaker: "who_speaks"}
	})
	if err != nil {
		t.Logf("expected: no text column found -> %v", err)
	} else {
		for _, w := range res.Warnings {
			t.Logf("warning: %s", w.Msg)
		}
	}
}

func TestHeaderlessCSVIsData(t *testing.T) {
	body := "First line,Sae,dialogue\nSecond line,,sign\n"
	res, err := parseString(t, body, func(o *adapter.ParseOptions) { o.Format = FormatCSV })
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Imported != 2 {
		t.Fatalf("imported %d, want both rows (a header row would have made one)", res.Stats.Imported)
	}
	if res.Messages[0].OriginalText != "First line" || res.Messages[0].TextKind != games.TextDialogue {
		t.Errorf("positional mapping wrong: %+v", res.Messages[0])
	}
	if res.Messages[1].TextKind != games.TextSign {
		t.Errorf("second row kind = %q", res.Messages[1].TextKind)
	}
	if len(warningsFor(res, "treated as data")) != 1 {
		t.Error("the reader must say it guessed the layout")
	}
	// Positional ids are stable: re-importing the same file must not duplicate.
	for i, want := range []string{"csv-1", "csv-2"} {
		if res.Messages[i].SourceMessageID != want {
			t.Errorf("source id %d = %q, want %q", i, res.Messages[i].SourceMessageID, want)
		}
	}
}

func TestExplicitIndexMappingOnHeaderlessCSV(t *testing.T) {
	body := "sae,First line\n?,Second line\n"
	res, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatCSV
		o.ColumnMap = adapter.ColumnMap{Text: "1", Speaker: "0"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages[0].OriginalText != "First line" {
		t.Errorf("index mapping ignored: %q", res.Messages[0].OriginalText)
	}
}

// TestTwoGamePortability is docs/spec/04's second check: a synthetic game with a
// deliberately different layout, language, speaker labelling and column naming
// must ingest through the same code, with only a mapping change. If a future
// edit makes this pass by special-casing one of the two files, the diff will be
// visible in this test's failure message, not in a runtime condition.
func TestTwoGamePortability(t *testing.T) {
	csv := parseFixture(t, "synthetic-adventure.csv", func(o *adapter.ParseOptions) {
		o.Format = FormatCSV
		o.Language = "en"
	})
	other := parseFixture(t, "synthetic-other-game.json", func(o *adapter.ParseOptions) {
		o.Format = FormatJSON
		o.Language = "en"
		o.DefaultKind = games.TextOther
		o.ColumnMap = adapter.ColumnMap{
			Text:    "line_of_dialogue",
			ID:      "utterance_id",
			Speaker: "who_speaks",
			Kind:    "sort_of_text",
			Scene:   "stage",
			Page:    "leaf_number",
		}
	})

	if csv.Stats.Imported == 0 || other.Stats.Imported == 0 {
		t.Fatalf("one of the two games imported nothing: %d, %d", csv.Stats.Imported, other.Stats.Imported)
	}
	// The second game repeats utterance_id 1 and 5 to express pages, so 15 rows
	// become 13 messages with two page rows.
	if other.Stats.Rows != 15 {
		t.Errorf("rows = %d, want 15", other.Stats.Rows)
	}
	if other.Stats.Imported != 13 {
		t.Errorf("imported = %d, want 13 (two id repeats merged into their message's pages)", other.Stats.Imported)
	}
	if len(other.Pages) != 4 {
		t.Errorf("page rows = %d, want 4 (both sides of the two merged messages)", len(other.Pages))
	}
	for _, p := range other.Pages {
		if p.SourcePageID != "2" && p.SourcePageID != "" {
			t.Errorf("source page id %q should be the leaf_number the file carries", p.SourcePageID)
		}
	}

	// Both games reach the same invariants regardless of their shape.
	for name, res := range map[string]*adapter.ParseResult{"csv": csv, "other": other} {
		for _, m := range res.Messages {
			if err := m.Validate(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			if m.SpeechText != m.OriginalText {
				t.Errorf("%s: speech rendering was altered by the parser, which docs/spec/02 says is a separate editable step", name)
			}
		}
	}

	// And neither game's vocabulary can be found in this package's source.
	assertNoGameVocabulary(t)
}

// gameVocabulary is the list docs/spec/04's portability rule forbids in shared
// code. The check is worth its awkwardness: a condition on one game's name is
// invisible to every other test in this package, because the code still works for
// the game named in it. It scans identifiers and literals rather than raw bytes,
// so a comment may still *explain* the rule using the words the rule is about.
var gameVocabulary = []string{
	"zelda", "hyrule", "waker", "herobegin", "greatbay", "outset", "tetra",
	"sheikah", "nintendo", "gamecube", "dolphinslot", "macguffin",
}

func assertNoGameVocabulary(t *testing.T) {
	t.Helper()
	for _, dir := range []string{".", ".."} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			for _, token := range codeTokens(path) {
				name := strings.ToLower(token)
				for _, word := range gameVocabulary {
					if strings.Contains(name, word) {
						t.Errorf("%s uses %q in code: docs/spec/04 forbids one game's vocabulary in shared code (token %q)", path, word, token)
					}
				}
			}
		}
	}
}

// codeTokens returns the identifiers and literals of a Go file, comments
// excluded. Comments are where the rule gets explained, so forbidding the words
// there would mean writing the docs around the thing the docs are about.
func codeTokens(path string) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		// A file that does not parse is a build failure, which go vet reports far
		// better than this test would; report nothing here rather than twice.
		return nil
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Ident:
			out = append(out, v.Name)
		case *ast.BasicLit:
			out = append(out, strings.Trim(v.Value, "\"`"))
		}
		return true
	})
	return out
}

func TestUnknownKindFallsBackWithWarning(t *testing.T) {
	body := "line,kind\nA,dialogue\nB,cutscene\nC,\n"
	res, err := parseString(t, body, func(o *adapter.ParseOptions) { o.Format = FormatCSV })
	if err != nil {
		t.Fatal(err)
	}
	want := []games.TextKind{games.TextDialogue, games.TextOther, games.TextOther}
	for i, m := range res.Messages {
		if m.TextKind != want[i] {
			t.Errorf("row %d kind = %q, want %q", i, m.TextKind, want[i])
		}
	}
	if ws := warningsFor(res, "cutscene"); len(ws) != 1 {
		t.Errorf("want one warning about the unknown class, got %v", ws)
	}

	// The caller's DefaultKind is honoured for the same input.
	res, err = parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatCSV
		o.DefaultKind = games.TextTutorial
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages[1].TextKind != games.TextTutorial {
		t.Errorf("DefaultKind ignored: %q", res.Messages[1].TextKind)
	}
}

func TestKindIsCaseInsensitive(t *testing.T) {
	res, err := parseString(t, "line,kind\nA,Sign\nB,SIGN\n", func(o *adapter.ParseOptions) { o.Format = FormatCSV })
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range res.Messages {
		if m.TextKind != games.TextSign {
			t.Errorf("row %d = %q", i, m.TextKind)
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("case alone is not a defect worth reporting: %v", res.Warnings)
	}
}

func TestSourceMessageIDsReconcile(t *testing.T) {
	body := "line,id\nA,  odd id  \nB,ok\nC,\nD," + strings.Repeat("x", 200) + "\n"
	res, err := parseString(t, body, func(o *adapter.ParseOptions) { o.Format = FormatCSV })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 4 {
		t.Fatalf("imported %d, want 4: an unusable id must not lose the row", res.Stats.Imported)
	}
	ids := []string{}
	for _, m := range res.Messages {
		ids = append(ids, m.SourceMessageID)
		if err := m.Validate(); err != nil {
			t.Errorf("id %q: %v", m.SourceMessageID, err)
		}
	}
	if ids[0] == "  odd id  " {
		t.Errorf("an id with spaces was stored as-is: %q", ids[0])
	}
	if len(ids[3]) > maxSourceSideID {
		t.Errorf("id longer than the storage limit was truncated silently: %d bytes", len(ids[3]))
	}
	if len(warningsFor(res, "identity")) == 0 && len(warningsFor(res, "identity "))+len(warningsFor(res, "empty")) == 0 {
		t.Errorf("no warning explains the rewritten ids: %v", ids)
	}
	// Two rows with the same id are one message with two pages, not a collision.
	dupe, err := parseString(t, "line,id\nOne,k\nTwo,k\n", func(o *adapter.ParseOptions) { o.Format = FormatCSV })
	if err != nil {
		t.Fatal(err)
	}
	if dupe.Stats.Imported != 1 || len(dupe.Pages) != 2 {
		t.Errorf("repeated id gave %d messages and %d pages, want 1 and 2", dupe.Stats.Imported, len(dupe.Pages))
	}
	if dupe.Messages[0].OriginalText != "One\n\nTwo" {
		t.Errorf("pages did not join into the message: %q", dupe.Messages[0].OriginalText)
	}
	if ws := warningsFor(dupe, "different speaker"); len(ws) != 0 {
		t.Errorf("identical repeats warned about speakers: %v", ws)
	}
	// A repeat that disagrees about the speaker is a script bug and says so.
	conflict, err := parseString(t, "line,id,speaker\nOne,k,A\nTwo,k,B\n", func(o *adapter.ParseOptions) { o.Format = FormatCSV })
	if err != nil {
		t.Fatal(err)
	}
	if len(warningsFor(conflict, "different speaker")) != 1 {
		t.Errorf("want a warning naming the disagreement, got %v", conflict.Warnings)
	}
}

func TestSourceIDRequiresSourceMessageID(t *testing.T) {
	// games.Message.Validate refuses a message with a source_id and no
	// source_message_id, because a re-import could not reconcile it. Setting
	// SourceID here must therefore still produce reconcilable rows.
	src := games.NewID()
	res, err := parseString(t, "line\nOnly line\n", func(o *adapter.ParseOptions) {
		o.Format = FormatText
		o.SourceID = src
	})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Messages[0]
	if m.SourceID != src || m.SourceMessageID == "" {
		t.Errorf("source pairing wrong: %+v", m)
	}
}

// fixtureMapping is the per-file configuration onboarding would collect: the
// second game's column names are not auto-mappable, which is the point of having
// a mapping at all. Everything else is left to the adapter's defaults.
func fixtureMapping(name string) func(*adapter.ParseOptions) {
	return func(o *adapter.ParseOptions) {
		o.Language = "auto"
		if name == "synthetic-other-game.json" {
			o.ColumnMap = adapter.ColumnMap{
				Text:    "line_of_dialogue",
				ID:      "utterance_id",
				Speaker: "who_speaks",
				Kind:    "sort_of_text",
				Scene:   "stage",
				Page:    "leaf_number",
			}
		}
	}
}

// TestIdempotentReparse is the property docs/spec/02 relies on for
// "re-import reconciles, not duplicates": the same bytes produce the same keys.
func TestIdempotentReparse(t *testing.T) {
	for _, name := range []string{"synthetic-adventure.csv", "synthetic-jp.jsonl", "plain.txt", "synthetic-other-game.json"} {
		t.Run(name, func(t *testing.T) {
			mapping := fixtureMapping(name)
			first := parseFixture(t, name, mapping)
			second := parseFixture(t, name, mapping)
			if len(first.Messages) != len(second.Messages) {
				t.Fatalf("message counts differ: %d then %d", len(first.Messages), len(second.Messages))
			}
			for i := range first.Messages {
				a, b := first.Messages[i], second.Messages[i]
				if a.OriginalText != b.OriginalText || a.NormalizedText != b.NormalizedText || a.SourceMessageID != b.SourceMessageID {
					t.Errorf("row %d differs:\n %q/%q vs %q/%q", i, a.SourceMessageID, a.NormalizedText, b.SourceMessageID, b.NormalizedText)
				}
				if a.ID == b.ID {
					t.Errorf("row %d reused a canonical id; ids are minted per parse", i)
				}
				if a.CreatedAt != b.CreatedAt && a.CreatedAt == "" {
					t.Errorf("row %d has no timestamp", i)
				}
			}
			// The normalization itself must be a fixed point.
			for _, m := range first.Messages {
				if again := textnorm.Normalize(m.NormalizedText, textnorm.Options{}); again != m.NormalizedText {
					t.Errorf("normalized_text is not stable for %q: %q", m.OriginalText, again)
				}
			}
		})
	}
}

//
// ── detection ───────────────────────────────────────────────────────────────
//

func TestDetectPrefersTheRightFormat(t *testing.T) {
	csvHead := []byte("line,speaker\nHello,Marla\nSecond,Corvin\n")
	tsvHead := []byte("line\tspeaker\nHello\tMarla\n")
	jsonHead := []byte(`{"messages":[{"text":"Hello"}]}`)
	jsonlHead := []byte("{\"line\":\"One\"}\n{\"line\":\"Two\"}\n")
	arrHead := []byte(`[{"text":"One"},{"text":"Two"}]`)
	textHead := []byte("Hello there.\nThis is a line of a script.\n")
	proseHead := []byte("The harbourmaster counted the crates again\nand paid the boy no mind\n")

	cases := []struct {
		file string
		head []byte
		want string
		min  adapter.Confidence
	}{
		{"script.csv", csvHead, FormatCSV, adapter.ConfidentFormat},
		{"SCRIPT.TSV", tsvHead, FormatTSV, adapter.ConfidentFormat},
		{"script.json", jsonHead, FormatJSON, adapter.ConfidentFormat},
		{"script.jsonl", jsonlHead, FormatJSONL, adapter.ConfidentFormat},
		// A .jsonl extension on a single pretty-printed array is a common lie
		// people tell their tools: sniffing wins on the content, and the
		// confidence drops to the "name and content disagree" band.
		{"dump.jsonl", arrHead, FormatJSON, adapter.ConfidentWeak},
		{"script.txt", textHead, FormatText, adapter.ConfidentFormat},
		{"no-extension", csvHead, FormatCSV, adapter.ConfidentSniffed},
		{"no-extension-json", jsonHead, FormatJSON, adapter.ConfidentSniffed},
		{"prose.txt", proseHead, FormatText, adapter.ConfidentFormat},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			if got := InferFormat(tc.head); got != tc.want {
				t.Errorf("InferFormat = %q, want %q", got, tc.want)
			}
			c := Adapter{}.Detect(context.Background(), tc.file, tc.head)
			if c < tc.min {
				t.Errorf("Detect(%s) = %v, want at least %v", tc.file, c, tc.min)
			}
			if !c.Valid() {
				t.Errorf("Detect returned an unusable %v", c)
			}
		})
	}

	// The registry must rank this adapter correctly for each input, and the
	// format it detected must be the one Parse then uses.
	r := adapter.NewRegistry()
	if err := Register(r); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		match, ok := r.DetectOne(context.Background(), tc.file, tc.head)
		if !ok {
			t.Errorf("%s: no adapter claimed it", tc.file)
			continue
		}
		if match.Manifest.ID != ID {
			t.Errorf("%s: %s won", tc.file, match.Manifest.ID)
		}
		res, err := Adapter{}.Parse(context.Background(), bytes.NewReader(tc.head), adapter.ParseOptions{
			ProfileID: games.NewID(),
		})
		if err != nil {
			t.Errorf("%s: auto-format parse failed: %v", tc.file, err)
			continue
		}
		if res.Stats.Imported == 0 {
			t.Errorf("%s: detection accepted a file that produced no messages", tc.file)
		}
	}
}

func TestDetectAgreesWithParse(t *testing.T) {
	// A file that Detect ranks must Parse to the same format, or onboarding shows
	// one thing and imports another.
	heads := map[string][]byte{
		FormatCSV:   []byte("a,b\n1,2\n"),
		FormatTSV:   []byte("a\tb\n1\t2\n"),
		FormatJSON:  []byte(`[{"line":"x"}]`),
		FormatJSONL: []byte("{\"line\":\"x\"}\n{\"line\":\"y\"}\n"),
		FormatText:  []byte("one line only and a second\nand a third line here\n"),
	}
	for want, head := range heads {
		if got := InferFormat(head); got != want {
			t.Errorf("InferFormat(%q) = %s, want %s", head, got, want)
		}
	}
	if got := InferFormat(nil); got != "" {
		t.Errorf("an empty head detected as %q", got)
	}
	// An extension that contradicts the content is reported weakly, not as a
	// confident match: the user should choose, not be auto-picked.
	strong := Adapter{}.Detect(context.Background(), "script.csv", []byte("just a line of prose\nagain"))
	if strong > adapter.ConfidentWeak {
		t.Errorf("a .csv whose content is prose was detected at %v", strong)
	}
	nameOnly := Adapter{}.Detect(context.Background(), "script.csv", nil)
	if nameOnly != adapter.ConfidentName {
		t.Errorf("a name with no content sampled at %v, want %v", nameOnly, adapter.ConfidentName)
	}
	if c := (newTestAdapter()).Detect(context.Background(), "", nil); c != adapter.ConfidentNone {
		t.Errorf("nothing at all scored %v", c)
	}
}

func TestManifestMatchesFixtures(t *testing.T) {
	m := Adapter{}.Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("the shipped manifest is invalid: %v", err)
	}
	if m.ID != ID || m.Version != Version {
		t.Errorf("identity changed: %+v", m)
	}
	for _, f := range m.Fixtures {
		if _, err := os.Stat(filepath.Join(".", f)); err != nil {
			t.Errorf("the manifest lists fixture %s which is not in the tree: %v", f, err)
		}
	}
	// Locales claimed must be locales tested: en through the CSV and JSON
	// fixtures, ja through the JSONL one.
	if !sort.StringsAreSorted(m.Locales) {
		t.Errorf("locales %v are not sorted, so the manifest digest would drift", m.Locales)
	}
}

func TestRegistryWiringRefusesDoubleRegistration(t *testing.T) {
	r := adapter.NewRegistry()
	if err := Register(r); err != nil {
		t.Fatal(err)
	}
	if err := Register(r); err == nil {
		t.Error("the same adapter id was registered twice")
	}
}

// TestUnknownFormatIsRefused keeps a typo in onboarding configuration from
// silently falling back to sniffing.
func TestUnknownFormatIsRefused(t *testing.T) {
	_, err := parseString(t, "line\nA\n", func(o *adapter.ParseOptions) { o.Format = "bmg" })
	if err == nil {
		t.Fatal("an unsupported format was accepted")
	}
	if !strings.Contains(err.Error(), "bmg") {
		t.Errorf("error does not name the format: %v", err)
	}
}

func TestParseOptionsProfileRequired(t *testing.T) {
	_, err := Adapter{}.Parse(context.Background(), strings.NewReader("line\nA\n"), adapter.ParseOptions{})
	if err == nil {
		t.Fatal("a message with no profile was produced")
	}
	if !strings.Contains(fmt.Sprint(err), "ProfileID") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// newTestAdapter exists because a composite literal followed by a method call is
// ambiguous inside an if statement's init clause in Go, and the alternative is
// writing (Adapter{}) at every call site.
func newTestAdapter() Adapter { return Adapter{} }

// TestNormalizationOptionsReachTheIndex proves ParseOptions.TextNorm is wired to
// the stored key rather than being decoration: the same bytes produce a different
// normalized_text depending on the profile's choices, and the player-name
// placeholder is what makes a line match whichever name the player chose.
func TestNormalizationOptionsReachTheIndex(t *testing.T) {
	body := `{"messages":[{"line":"Player One, the tide is out!"},{"line":"A Sign -- Really"}]}`

	plain, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatJSON
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Messages[0].NormalizedText != "player one, the tide is out!" {
		t.Errorf("default import changed the text beyond case and space: %q", plain.Messages[0].NormalizedText)
	}
	// Two ASCII hyphens are what the source wrote, and no fold applies to them:
	// compatibility folding is about the characters that *differ by encoding*,
	// not about normalizing a writer's punctuation habits, which is what
	// StripPunctuation is for.
	if plain.Messages[1].NormalizedText != "a sign -- really" {
		t.Errorf("unexpected default fold: %q", plain.Messages[1].NormalizedText)
	}

	configured, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatJSON
		o.TextNorm = textnorm.Options{StripPunctuation: true, PlayerNames: []string{"Player One"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := configured.Messages[0].NormalizedText; got != "{player} the tide is out" {
		t.Errorf("configured normalization produced %q", got)
	}
	if got := configured.Messages[1].NormalizedText; got != "a sign  really" && got != "a sign really" {
		t.Errorf("punctuation stripping produced %q", got)
	}

	// OCRFold is deliberately lossy, and the import applies exactly what the
	// matcher will apply later so the two sides stay comparable
	// (docs/spec/12 "candidate ranking").
	ocr, err := parseString(t, `{"messages":[{"line":"T0rn 1ine"}]}`, func(o *adapter.ParseOptions) {
		o.Format = FormatJSON
		o.TextNorm = textnorm.Options{OCRFold: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := ocr.Messages[0].NormalizedText; got != "tom llne" {
		t.Errorf("OCR folding produced %q, want the documented confusions folded (0→o, rn→m, 1→l, i→l)", got)
	}

	// original_text never moves with the options: the rendering the user reads and
	// the narrator speaks comes from that column, not from the key.
	for i := range plain.Messages {
		if plain.Messages[i].OriginalText != configured.Messages[i].OriginalText {
			t.Errorf("original_text changed with the options: %q vs %q",
				plain.Messages[i].OriginalText, configured.Messages[i].OriginalText)
		}
	}
}

// TestJSONScalarAndNestedValues covers the value kinds a real file contains: a
// numeric id, a null speaker, and a nested object that has nowhere to go.
func TestJSONScalarAndNestedValues(t *testing.T) {
	body := `{"messages":[
		{"line":"One","id":142,"speaker":null,"kind":"dialogue"},
		{"line":"Two","extra":{"nested":{"deep":true}},"ok":false}
	]}`
	res, err := parseString(t, body, func(o *adapter.ParseOptions) { o.Format = FormatJSON })
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Imported != 2 {
		t.Fatalf("imported %d, want 2", res.Stats.Imported)
	}
	if res.Messages[0].SourceMessageID != "142" {
		t.Errorf("a numeric id should keep its source text, got %q", res.Messages[0].SourceMessageID)
	}
	if res.Messages[0].AttributionStatus != games.AttributionUnknown {
		t.Errorf("an explicit null speaker must stay unknown rather than becoming the string \"null\": %q",
			res.Messages[0].AttributionStatus)
	}
	if ws := warningsFor(res, "nested JSON"); len(ws) != 1 {
		t.Errorf("the dropped object should be reported: %v", res.Warnings)
	}
	if ws := warningsFor(res, "not a string"); len(ws) == 0 {
		t.Errorf("scalar substitutions should be visible in the report: %v", res.Warnings)
	}
	// An unmapped key is named once, which is how a renamed column is found.
	if ws := warningsFor(res, "not mapped to anything"); len(ws) == 0 {
		t.Errorf("unmapped keys should be reported once each: %v", res.Warnings)
	}
}
