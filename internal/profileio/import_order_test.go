package profileio

import (
	"bytes"
	"testing"

	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/store"
)

// TestImportBatchPreservesScriptOrder is the regression for the speaker-link
// Playwright flake (ADR-0080): a batch of lines minted in the same
// millisecond at import used to sort by random suffix, so any consumer that
// pages by id could shuffle them. The import below writes n small CSV rows
// whose parse loop mints one ULID per row — well inside a single millisecond
// — and then asserts that both consumers of the id order the UI relies on
// keep the script order:
//
//   - the editor/API listing: internal/api/messages.go pages through
//     exactly this ListMessages call and the web list renders the rows in
//     the order returned, so a shuffled page is what the operator saw;
//   - the export: the manifest renders GetProfileContent's message order,
//     so a profile moved to another machine carried the shuffle with it.
//
// The same-millisecond condition itself is proven deterministically by
// internal/games (TestNewIDsStrictlyAscendingInTightLoop and the concurrent
// variant); this test is the end-to-end half: real file, real adapter,
// real store, order asserted. With the pre-ADR-0080 generator, n lines
// minted in one millisecond came back in one of n! random orders, so for
// n = 16 a pass of the old code would be a 1-in-20,922,789,888,000 accident.
func TestImportBatchPreservesScriptOrder(t *testing.T) {
	const n = 16
	want := lineIDs(n)

	e := newEnv(t)
	g := e.game("synthetic-order", "Synthetic Order")
	p := e.draft(e.edition(g, "US", "en").ID)

	var buf bytes.Buffer
	for i := 0; i < n; i++ {
		writeCSVRow(&buf, i)
	}
	req := ImportRequest{
		ProfileID: p.ID, FileName: "order.csv", Reader: &buf,
		Format: generic.FormatCSV,
		Parse: adapter.ParseOptions{
			Language: "en", Format: generic.FormatCSV,
			// No header row: position addressing, the same path the
			// 1200-row streaming import in importfile_test.go exercises.
			ColumnMap: adapter.ColumnMap{ID: "0", Text: "2", Kind: "3", Scene: "4"},
		},
	}
	report, err := ImportFile(e.ctx, e.deps, req)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if report.Stats.Imported != n {
		t.Fatalf("imported %d rows, want %d", report.Stats.Imported, n)
	}

	// ── the editor/API listing ───────────────────────────────────────────
	var (
		got    []string
		cursor string
		lastID string
	)
	for {
		page, next, err := e.store.ListMessages(e.ctx, p.ID, store.MessageFilter{}, cursor, store.MaxListLimit)
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		for _, m := range page {
			if m.ID <= lastID {
				t.Fatalf("listing is not id-ascending: %q after %q", m.ID, lastID)
			}
			lastID = m.ID
			got = append(got, m.SourceMessageID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	assertScriptOrder(t, "listing", got, want)

	// ── the export ───────────────────────────────────────────────────────
	m, err := Export(e.ctx, e.store, p.ID, ExportOptions{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	exported := make([]string, 0, len(m.Messages))
	for _, msg := range m.Messages {
		if msg.SourceMessageID == nil {
			t.Fatalf("export message %s carries no source_message_id", msg.ID)
		}
		exported = append(exported, *msg.SourceMessageID)
	}
	assertScriptOrder(t, "export", exported, want)
}

// lineIDs is the script order of the rows writeCSVRow appends: line-0,
// line-1, ... (the ids the ColumnMap binds to the source_message_id column).
func lineIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "line-" + itoa(i)
	}
	return out
}

func assertScriptOrder(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s has %d messages, want %d", what, len(got), len(want))
	}
	for i, id := range want {
		if got[i] != id {
			t.Fatalf("%s position %d = %s, want %s: a same-millisecond import batch came back out of script order (ADR-0080)",
				what, i, got[i], id)
		}
	}
}
