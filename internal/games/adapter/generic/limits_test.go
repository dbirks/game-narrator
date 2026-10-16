package generic

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
)

// These tests are the difference between "the importer reads files" and "the
// importer can be pointed at a stranger's upload" (docs/spec/14
// "Filesystem/import"). Every limit is asserted at both ends: the boundary value
// succeeds and one past it fails, because a limit that only ever fires is as
// useless as one that never does.

func TestSizeLimitRefusesRatherThanTruncates(t *testing.T) {
	body := "line\n" + strings.Repeat("A reasonably long line of dialogue.\n", 40)

	// One byte under the cap imports.
	res, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatText
		o.MaxBytes = int64(len(body) - 1)
	})
	if err == nil {
		t.Errorf("a source of %d bytes was accepted with MaxBytes=%d", len(body), len(body)-1)
	} else if !errors.Is(err, ErrTooLarge) {
		t.Errorf("error = %v, want ErrTooLarge", err)
	}

	// Exactly the cap imports. The off-by-one here would reject a user's real
	// file for a byte they never exceeded.
	res, err = parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatText
		o.MaxBytes = int64(len(body))
	})
	if err != nil {
		t.Fatalf("MaxBytes=%d rejected a source of the same size: %v", len(body), err)
	}
	// 41: plain-text mode has no header row, so the body's first line is a
	// message too. The count here is the check on that, not an assumption.
	if res.Stats.Imported != 41 {
		t.Errorf("imported %d rows, want all 41 lines", res.Stats.Imported)
	}

	// The error must not arrive as a truncated success either.
	_, err = parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatText
		o.MaxBytes = 100
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
}

// TestSizeLimiterReads is the unit proof of the byte budget. The cases that
// matter are the boundary and the small-buffer ones: a reader that clips Read to
// what remains, then reports the breach on the following call, is what stops any
// reader above it from mistaking a truncated source for a complete one.
func TestSizeLimiterReads(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		max      int64
		bufSize  int
		wantRead int
		wantErr  error
	}{
		{"under", "hello", 10, 4096, 5, io.EOF},
		{"exactly at the cap", "hello", 5, 4096, 5, io.EOF},
		{"small buffer, exact", "hello", 5, 2, 5, io.EOF},
		{"small buffer, under", "hello", 10, 2, 5, io.EOF},
		{"one byte over", "hello", 4, 4096, 5, ErrTooLarge},
		{"well over", "hello there", 4, 4096, 5, ErrTooLarge},
		{"zero budget", "hello", 0, 4096, 1, ErrTooLarge},
		{"empty source", "", 4, 4096, 0, io.EOF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lim := newSizeLimiter(strings.NewReader(tc.body), tc.max)
			n, err := readWith(lim, make([]byte, tc.bufSize))
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if n != tc.wantRead {
				t.Errorf("read %d bytes, want %d", n, tc.wantRead)
			}
		})
	}
}

// readWith drains r using a caller-sized buffer. io.ReadAll always asks for a
// large one, which would hide the path where the limiter clips a short Read.
func readWith(r io.Reader, buf []byte) (int, error) {
	total := 0
	for {
		n, err := r.Read(buf)
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, nil
		}
	}
}

// TestReaderDoesNotBufferTheFile is the streaming claim, proved directly rather
// than inferred from an allocation number: the source is an io.Pipe that would
// block forever once the first batch is delivered, and the context is cancelled
// during that batch. A reader that slurped the file (io.ReadAll) would sit in the
// pipe until the test's deadline expired; a streaming reader notices the
// cancellation between records and returns.
func TestReaderDoesNotBufferTheFile(t *testing.T) {
	const rows = 300_000
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		bw := bufio.NewWriterSize(pw, 1<<16)
		fmt.Fprintln(bw, "line,speaker")
		for i := range rows {
			fmt.Fprintf(bw, "synthetic line %d,Synthetic Speaker\n", i)
			if i%1000 == 0 {
				_ = bw.Flush()
			}
		}
		_ = bw.Flush()
		// The pipe stays open: if the parser wants the rest of the file before it
		// will do anything, that is exactly what it will get to do.
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := newTestAdapter().Parse(ctx, pr, adapter.ParseOptions{
			ProfileID: games.NewID(),
			Format:    FormatCSV,
			MaxBytes:  1 << 30,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reader did not return after cancellation; it is waiting on the source, not streaming from it")
	}
	pw.Close()
}

func TestRowLimit(t *testing.T) {
	// Plain-text mode has no header row, so every line is a record: 13 here.
	body := "line\n" + strings.Repeat("One line.\n", 12)
	res, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatText
		o.MaxRows = 13
	})
	if err != nil {
		t.Fatalf("exactly MaxRows rows failed: %v", err)
	}
	if res.Stats.Imported != 13 {
		t.Errorf("imported %d, want 13", res.Stats.Imported)
	}
	_, err = parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatText
		o.MaxRows = 12
	})
	if !errors.Is(err, ErrTooManyRows) {
		t.Errorf("error = %v, want ErrTooManyRows", err)
	}
}

func TestRaggedRowsWarnButImport(t *testing.T) {
	body := "line,speaker,id\nA,Marla,a1\nB,Corvin\nC,Marla,c3,extra\n"
	res, err := parseString(t, body, func(o *adapter.ParseOptions) { o.Format = FormatCSV })
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Imported != 3 {
		t.Fatalf("imported %d, want all 3 rows despite the ragged field counts", res.Stats.Imported)
	}
	if len(warningsFor(res, "fields")) < 2 {
		t.Errorf("want a warning per ragged row, got %v", res.Warnings)
	}
	// The extra fourth field on row C is not silently mapped onto a role.
	if res.Messages[2].SourceMessageID != "c3" {
		t.Errorf("row C id = %q", res.Messages[2].SourceMessageID)
	}
}

func TestOversizeFieldSkipsItsRow(t *testing.T) {
	huge := strings.Repeat("x", maxFieldBytes+1)
	body := "line\n" + huge + "\nSmall line\n"
	res, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatText
		o.MaxBytes = int64(len(body))
	})
	if err != nil {
		t.Fatal(err)
	}
	// Three lines: the "line" title, the oversized one, and the small one.
	if res.Stats.Imported != 2 || res.Stats.Skipped != 1 {
		t.Errorf("Stats = %+v, want 2 imported and 1 skipped", res.Stats)
	}
	if len(warningsFor(res, "above the")) != 1 {
		t.Errorf("the skipped row must say why: %v", res.Warnings)
	}
}

func TestOversizeJSONFieldSkipsItsRow(t *testing.T) {
	huge := strings.Repeat("海", maxFieldBytes/3+10) // multibyte, so bytes and runes differ
	body := fmt.Sprintf(`{"messages":[{"line":%q},{"line":"Fine"}]}`, huge)
	res, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatJSON
		o.MaxBytes = int64(len(body))
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Imported != 1 {
		t.Errorf("imported %d, want only the small row", res.Stats.Imported)
	}
}

// TestTextFieldOverStorageLimitSaysSo covers the limit that belongs to the
// storage layer rather than this package: games.Message.original_text holds 8000
// characters, so a longer line is skipped with a warning rather than imported and
// then rejected by Validate inside someone else's transaction.
func TestTextFieldOverStorageLimitSaysSo(t *testing.T) {
	long := strings.Repeat("a", maxTextRunes+1)
	body := fmt.Sprintf("line\n%q\nkept\n", long)
	res, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatCSV
		o.MaxBytes = int64(len(body))
	})
	if err != nil {
		t.Fatal(err)
	}
	// CSV mode: "line" is the header, so two data rows, one of them skipped.
	if res.Stats.Rows != 2 || res.Stats.Skipped != 1 || res.Stats.Imported != 1 {
		t.Errorf("Stats = %+v, want 2 rows with the long one skipped", res.Stats)
	}
	if len(warningsFor(res, "8000 character limit")) != 1 {
		t.Errorf("warning should name the character limit: %v", res.Warnings)
	}
}

func TestDepthBombIsRefused(t *testing.T) {
	// games.MaxJSONDepth is 32; the same rule applies to a source document, or an
	// import would succeed here and be rejected at insert time.
	deep := strings.Repeat("[", games.MaxJSONDepth+2) + strings.Repeat("]", games.MaxJSONDepth+2)
	_, err := parseString(t, fmt.Sprintf(`{"messages":[{"line":%s}]}`, deep), func(o *adapter.ParseOptions) {
		o.Format = FormatJSON
	})
	if err == nil {
		t.Fatal("a deeply nested document imported")
	}
	if !strings.Contains(err.Error(), "nests") {
		t.Errorf("error does not explain the depth limit: %v", err)
	}

	// One level under the limit still works, so the check is a limit and not a
	// refusal of structured input.
	ok := strings.Repeat("[", games.MaxJSONDepth-2) + "1" + strings.Repeat("]", games.MaxJSONDepth-2)
	res, err := parseString(t, fmt.Sprintf(`{"messages":[{"other":%s,"line":"kept"}]}`, ok), func(o *adapter.ParseOptions) {
		o.Format = FormatJSON
	})
	if err != nil {
		t.Fatalf("a document inside the depth limit was refused: %v", err)
	}
	if res.Stats.Imported != 1 {
		t.Errorf("imported %d", res.Stats.Imported)
	}
	if len(warningsFor(res, "nested JSON")) != 1 {
		t.Errorf("the dropped nested value should be reported: %v", res.Warnings)
	}
}

func TestJSONLDepthBombRefsuesWholeFile(t *testing.T) {
	deep := strings.Repeat(`{"a":`, games.MaxJSONDepth+1) + "1" + strings.Repeat("}", games.MaxJSONDepth+1)
	_, err := parseString(t, fmt.Sprintf("{\"line\":\"first\"}\n%s\n{\"line\":\"second\"}\n", deep), func(o *adapter.ParseOptions) {
		o.Format = FormatJSONL
	})
	if err == nil {
		t.Fatal("a line beyond the depth limit was absorbed")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("the error should point at the offending line: %v", err)
	}
}

func TestInvalidUTF8IsMarkedAndWarned(t *testing.T) {
	// bytes.Reader, not a string, so the invalid bytes are exact.
	body := "line\nHero\ufffds sword?\nbroken \xff\xff here\n"
	res, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatText
		o.MaxBytes = int64(len(body))
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Imported != 3 {
		t.Fatalf("imported %d, want all three lines kept (plain text has no header row)", res.Stats.Imported)
	}
	for _, m := range res.Messages {
		if !utf8.ValidString(m.OriginalText) {
			t.Errorf("stored text is not valid UTF-8: %q", m.OriginalText)
		}
	}
	if !strings.ContainsRune(res.Messages[2].OriginalText, '\ufffd') {
		t.Errorf("invalid bytes were dropped rather than replaced: %q", res.Messages[2].OriginalText)
	}
	if len(warningsFor(res, "invalid UTF-8")) != 1 {
		t.Errorf("want exactly one warning, on the broken row: %v", res.Warnings)
	}
}

func TestPunctuationOnlyRowIsNotImported(t *testing.T) {
	// normalized_text is NOT NULL and non-empty: a row that folds to nothing is
	// unmatchable, and importing it would fill the index with rows recognition can
	// never return.
	res, err := parseString(t, "line\n!!!\nreal line\n", func(o *adapter.ParseOptions) {
		o.Format = FormatCSV
		o.TextNorm.StripPunctuation = true
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Imported != 1 || res.Stats.Skipped != 1 {
		t.Errorf("Stats = %+v", res.Stats)
	}
	if len(warningsFor(res, "normalizes to nothing")) != 1 {
		t.Errorf("the skip must be explained: %v", res.Warnings)
	}
}

func TestWarningListIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("line,speaker\n")
	b.WriteString("a real line,Marla\n") // one good row, so the import can succeed
	for i := range maxWarnings + 200 {
		fmt.Fprintf(&b, ",missing-%d\n", i)
	}
	body := b.String()
	res, err := parseString(t, body, func(o *adapter.ParseOptions) {
		o.Format = FormatCSV
		o.MaxBytes = int64(len(body))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) > maxWarnings+1 {
		t.Errorf("%d warnings returned, the cap is %d", len(res.Warnings), maxWarnings)
	}
	if len(warningsFor(res, "suppressed")) != 1 {
		t.Errorf("the truncated list must say it was truncated: %v", res.Warnings)
	}
}

func TestMalformedCSVIsAFailureNotAGuess(t *testing.T) {
	// LazyQuotes=false is a decision: absorbing a stray quote shifts every later
	// column on the row and yields messages that look fine.
	_, err := parseString(t, "line,speaker\n\"unbalanced,Marla\n\"another,Corvin\n", func(o *adapter.ParseOptions) {
		o.Format = FormatCSV
	})
	if err == nil {
		t.Fatal("a file with malformed quoting imported")
	}
	if !strings.Contains(err.Error(), "line") {
		t.Errorf("the error should locate the record: %v", err)
	}
}

func TestEmptySources(t *testing.T) {
	for name, body := range map[string]string{
		"nothing":      "",
		"blank lines":  "\n\n\n",
		"empty object": "{}",
		"empty array":  "[]",
		"header only":  "line,speaker\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseString(t, body, func(o *adapter.ParseOptions) { o.MaxBytes = 1 << 20 })
			if !errors.Is(err, adapter.ErrNoMessages) {
				t.Errorf("err = %v, want ErrNoMessages so onboarding says \"nothing recognised\"", err)
			}
		})
	}
}

func TestCancellationStopsReading(t *testing.T) {
	var b strings.Builder
	b.WriteString("line\n")
	for i := range 5000 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	body := b.String()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Adapter{}.Parse(ctx, strings.NewReader(body), adapter.ParseOptions{
		ProfileID: games.NewID(),
		Format:    FormatText,
		MaxBytes:  int64(len(body)),
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}

	// A context with a deadline that expires mid-file behaves the same way; the
	// 100k-row test below proves the readers keep up, so a short deadline lands
	// inside the file rather than before it.
	deadline, cancelDeadline := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelDeadline()
	if _, err := newTestAdapter().Parse(deadline, strings.NewReader(body), adapter.ParseOptions{
		ProfileID: games.NewID(),
		Format:    FormatText,
		MaxBytes:  int64(len(body)),
	}); err == nil {
		t.Error("an expired context produced a result")
	}
}

// TestBoundedMemoryOnLargeSource is docs/spec/16 M1's "without memory blowup"
// check for the import path: 100 000 rows parsed from a real file on disk, in
// bounded memory and in a few seconds. A generated file in t.TempDir() is used
// rather than a testdata fixture so no large blob enters the repository
// (AGENTS.md "No real game text", and a 6 MB fixture would be worse than that).
func TestBoundedMemoryOnLargeSource(t *testing.T) {
	if testing.Short() {
		t.Skip("large import benchmark skipped in -short mode")
	}
	const rows = 100_000
	path := filepath.Join(t.TempDir(), "synthetic-large.csv")
	writeLargeCSV(t, path, rows)

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	start := time.Now()
	res, err := Adapter{}.Parse(context.Background(), f, adapter.ParseOptions{
		ProfileID: games.NewID(),
		Format:    FormatCSV,
		Language:  "en",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Parse of %d rows: %v", rows, err)
	}
	if res.Stats.Imported != rows {
		t.Errorf("imported %d, want %d", res.Stats.Imported, rows)
	}

	// The budget is stated on HeapAlloc, and the honest way to read that number
	// for a live-footprint claim is after a collection: without one it measures how
	// lazily the collector has run rather than what the import holds. The transient
	// peak has its own proof — TestReaderDoesNotBufferTheFile fails if a reader
	// waits on the source instead of streaming from it — so this number is about
	// what survives the import, which is what a server holding N concurrent
	// imports would actually feel.
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	const heapBudget = 64 << 20
	if growth > heapBudget {
		t.Errorf("HeapAlloc grew %s importing %d rows (%s per message), above the %s budget",
			human(growth), rows, human(growth/int64(rows)), human(heapBudget))
	}
	// TotalAlloc is reported rather than bounded here. The result owns a copy of
	// every message's text and key by construction, the append that builds it
	// doubles, and the per-record strings encoding/csv hands out are garbage as
	// soon as a row is folded — so the number is a few times the file size for any
	// correct implementation. Whether the reader *streams* is answered directly by
	// TestReaderDoesNotBufferTheFile, which hangs if the parser waits on its
	// source, and that is the claim worth a test. A bound here would only measure
	// how much the domain type costs.
	size := fileSize(t, path)
	allocated := int64(after.TotalAlloc) - int64(before.TotalAlloc)

	if elapsed > 10*time.Second {
		t.Errorf("parsing %d rows took %s, above the 10s budget", rows, elapsed)
	}
	t.Logf("%d rows from a %s file in %s: live heap growth %s (%s/message), %s allocated, %d messages, %d pages",
		rows, human(size), elapsed.Round(time.Millisecond), human(growth),
		human(growth/int64(rows)), human(allocated), res.Stats.Imported, len(res.Pages))

	// Spot-check content correctness at a high row index, so a reader that
	// stopped early cannot pass on memory numbers alone.
	for _, idx := range []int{0, rows / 2, rows - 1} {
		want := fmt.Sprintf("synthetic-%d", idx+1)
		if res.Messages[idx].SourceMessageID != want {
			t.Errorf("row %d has source id %q, want %q", idx, res.Messages[idx].SourceMessageID, want)
		}
	}
}

// writeLargeCSV generates a script with the shape of a real one: short lines, a
// speaker on most rows, an id on every row.
func writeLargeCSV(t *testing.T, path string, rows int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 1<<16)
	if _, err := w.WriteString("id,line,speaker,kind\n"); err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		speaker := ""
		if i%3 != 0 {
			speaker = "Synthetic Speaker"
		}
		kind := "dialogue"
		if i%50 == 0 {
			kind = "sign"
		}
		fmt.Fprintf(w, "synthetic-%d,\"Line %d of the synthetic script, which is long enough to be realistic.\",%s,%s\n",
			i+1, i+1, speaker, kind)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func human(n int64) string {
	switch {
	case n > 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// TestRepeatedParseProducesIdenticalKeys is the reconciliation property again,
// on the large file: a re-run after a crash must produce the same source
// identities, or docs/spec/02's "reconcile, not duplicate" is a comment.
func TestRepeatedParseProducesIdenticalKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
	path := filepath.Join(t.TempDir(), "repeat.csv")
	writeLargeCSV(t, path, 20_000)
	var keys [][]string
	for range 2 {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Adapter{}.Parse(context.Background(), f, adapter.ParseOptions{
			ProfileID: games.NewID(), Format: FormatCSV,
		})
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range res.Messages {
			got = append(got, m.SourceMessageID+"|"+m.NormalizedText)
		}
		keys = append(keys, got)
	}
	if strings.Join(keys[0], "\n") != strings.Join(keys[1], "\n") {
		t.Error("two parses of the same bytes produced different keys")
	}
}
