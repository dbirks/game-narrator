package ocr

import (
	"image"
	"strings"
	"testing"
)

// recordedTSV5 is the literal output of
//
//	tesseract preprocessed-crop.png stdout --psm 6 -l eng tsv
//
// from Tesseract 5.5.3 over the mono-cmd-01 fixture, with the glyph-level rows
// kept as they arrive. It is the "captured engine response" side of docs/spec/12
// "Frame replay": this package's parsing must be testable without the binary.
const recordedTSV5 = "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
	"1\t1\t0\t0\t0\t0\t0\t0\t1100\t220\t-1\t\n" +
	"2\t1\t1\t0\t0\t0\t46\t36\t1054\t46\t-1\t\n" +
	"3\t1\t1\t1\t0\t0\t46\t36\t1054\t46\t-1\t\n" +
	"4\t1\t1\t1\t1\t0\t46\t36\t1054\t46\t-1\t\n" +
	"5\t1\t1\t1\t1\t1\t46\t40\t76\t32\t92.490120\tSet\n" +
	"5\t1\t1\t1\t1\t2\t158\t36\t104\t36\t89.355904\tsail\n" +
	"5\t1\t1\t1\t1\t3\t296\t40\t46\t32\t91.817902\tat\n" +
	"5\t1\t1\t1\t1\t4\t378\t36\t130\t36\t89.898659\tfirst\n" +
	"5\t1\t1\t1\t1\t5\t546\t36\t148\t46\t90.541122\tlight,\n" +
	"5\t1\t1\t1\t1\t6\t736\t36\t76\t36\t93.089615\tand\n" +
	"5\t1\t1\t1\t1\t7\t848\t36\t104\t36\t92.839027\ttake\n" +
	"5\t1\t1\t1\t1\t8\t986\t36\t76\t30\t45.172611\tthe\n"

// recordedTSV3 is the same page as Tesseract 3.x numbered it: no page row, so
// words are level 4. ADR-0012 expects CLI drift between hosts; the parse keyed on
// numbering columns rather than level survives it.
const recordedTSV3 = "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
	"1\t1\t1\t0\t0\t0\t46\t36\t1054\t46\t-1\t\n" +
	"2\t1\t1\t1\t0\t0\t46\t36\t1054\t46\t-1\t\n" +
	"3\t1\t1\t1\t1\t0\t46\t36\t1054\t46\t-1\t\n" +
	"4\t1\t1\t1\t1\t1\t46\t40\t76\t32\t92.490120\tSet\n" +
	"4\t1\t1\t1\t1\t2\t158\t36\t104\t36\t89.355904\tsail\n" +
	"4\t1\t1\t1\t1\t3\t296\t40\t46\t32\t91.817902\tat\n"

func TestParseTSVRecorded(t *testing.T) {
	res, err := ParseTSV([]byte(recordedTSV5))
	if err != nil {
		t.Fatalf("ParseTSV: %v", err)
	}
	if want := "Set sail at first light, and take the"; res.Text != want {
		t.Errorf("Text = %q, want %q", res.Text, want)
	}
	if len(res.Words) != 8 {
		t.Fatalf("%d words, want 8", len(res.Words))
	}
	if len(res.Lines) != 1 {
		t.Fatalf("%d lines, want 1", len(res.Lines))
	}
	w := res.Words[0]
	if w.Text != "Set" || w.Box != image.Rect(46, 40, 122, 72) {
		t.Errorf("first word = %+v, want Set at (46,40)-(122,72)", w)
	}
	if w.Conf < 0.92 || w.Conf > 0.93 {
		t.Errorf("first word conf %.4f, want 0.925 (Tesseract's 92.49 normalized to 0-1)", w.Conf)
	}
	if res.MeanConf < 0.8 || res.MeanConf > 0.9 {
		t.Errorf("MeanConf %.4f is not the mean of the word confidences", res.MeanConf)
	}
	line := res.Lines[0]
	if line.Text != res.Text {
		t.Errorf("single-line Text mismatch: line %q vs result %q", line.Text, res.Text)
	}
	// The line box is the union of its words, so a caller can mask or crop it.
	if line.Box.Min.X != 46 || line.Box.Max.X != 1062 {
		t.Errorf("line box %v, want x 46..1062", line.Box)
	}
	if line.Conf <= 0 || line.Conf > 1 {
		t.Errorf("line conf %.3f out of range", line.Conf)
	}
}

func TestParseTSVVersionTolerance(t *testing.T) {
	new_, err := ParseTSV([]byte(recordedTSV5))
	if err != nil {
		t.Fatal(err)
	}
	old, err := ParseTSV([]byte(recordedTSV3))
	if err != nil {
		t.Fatal(err)
	}
	if want := "Set sail at"; old.Text != want {
		t.Errorf("legacy-level TSV read as %q, want %q", old.Text, want)
	}
	// Same words, same confidences, from differently numbered rows: the level
	// column is not load-bearing.
	for i := range old.Words {
		if old.Words[i].Text != new_.Words[i].Text || old.Words[i].Conf != new_.Words[i].Conf {
			t.Errorf("word %d differs between engine versions: %+v vs %+v", i, old.Words[i], new_.Words[i])
		}
	}
}

// TestParseTSVQuotedWord is the regression for reading Tesseract's tab-separated
// output with CSV semantics: an unquoted `"` mid-field is ordinary text, and a CSV
// parser treats it as the start of a quoted field, swallowing the rest of the line
// — including tabs — and reporting a corrupted hypothesis as a clean reading.
func TestParseTSVQuotedWord(t *testing.T) {
	in := "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"5\t1\t1\t1\t1\t1\t10\t10\t20\t20\t95\t\"Wait\"\n" +
		"5\t1\t1\t1\t1\t2\t40\t10\t20\t20\t90\ttonight!\n"
	res, err := ParseTSV([]byte(in))
	if err != nil {
		t.Fatalf("ParseTSV: %v", err)
	}
	if len(res.Words) != 2 {
		t.Fatalf("%d words, want 2: %+v", len(res.Words), res.Words)
	}
	if res.Words[0].Text != `"Wait"` {
		t.Errorf("word = %q, want the quotes preserved verbatim", res.Words[0].Text)
	}
	if res.Words[1].Text != "tonight!" {
		t.Errorf("second word = %q, want tonight! — a swallowed tab merges lines", res.Words[1].Text)
	}
}

func TestParseTSVTwoLines(t *testing.T) {
	in := "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"5\t1\t1\t1\t1\t1\t10\t10\t20\t20\t95\tone\n" +
		"5\t1\t1\t1\t2\t1\t10\t50\t20\t20\t95\ttwo\n" +
		"5\t1\t1\t1\t2\t2\t40\t50\t20\t20\t85\tthree\n"
	res, err := ParseTSV([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 2 {
		t.Fatalf("%d lines, want 2", len(res.Lines))
	}
	if res.Text != "one\ntwo three" {
		t.Errorf("Text = %q, want a line break between paragraphs of text", res.Text)
	}
	if got := res.Lines[1].Conf; got < 0.89 || got > 0.91 {
		t.Errorf("second line conf %.3f, want the mean of 0.95 and 0.85", got)
	}
}

func TestParseTSVEdgeCases(t *testing.T) {
	if _, err := ParseTSV(nil); err == nil {
		t.Error("empty output parsed without error; a killed engine can produce this")
	}
	if _, err := ParseTSV([]byte("foo\tbar\n1\t2\n")); err == nil {
		t.Error("a stream with no level column parsed without error")
	}
	// A truncated final row is tolerated: it is what a killed or timed-out child
	// leaves behind, and the rows before it were fully written.
	res, err := ParseTSV([]byte("level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"5\t1\t1\t1\t1\t1\t1\t1\t2\t2\t80\thello\n" +
		"5\t1\t1\t1\t1\t2\t1\t1\t2"))
	if err != nil {
		t.Fatalf("truncated stream: %v", err)
	}
	if len(res.Words) != 1 || res.Words[0].Text != "hello" {
		t.Errorf("truncated stream yielded %+v, want just the complete word", res.Words)
	}
	// conf -1 on a word row (it happens on rejected alternatives) must not become
	// a negative confidence, which would sail past any `conf > 0.6` filter.
	res, err = ParseTSV([]byte("level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"5\t1\t1\t1\t1\t1\t1\t1\t2\t2\t-1\tnull\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Words[0].Conf != 0 {
		t.Errorf("conf -1 became %.2f, want 0", res.Words[0].Conf)
	}
	// A zero-size box (Tesseract reports one for whitespace-only words) is "no
	// geometry", not a rectangle at the origin.
	res, err = ParseTSV([]byte("level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"5\t1\t1\t1\t1\t1\t0\t0\t0\t0\t90\tspaced\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Words[0].Box.Empty() {
		t.Errorf("zero-size box became %v", res.Words[0].Box)
	}
}

func TestResultEmptyIsZeroValue(t *testing.T) {
	// An empty page is a normal outcome, not an error, and its MeanConf must be 0
	// rather than NaN — a NaN that would then poison a stability threshold.
	res, err := ParseTSV([]byte("level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "" || len(res.Words) != 0 || res.MeanConf != 0 {
		t.Errorf("empty page = %+v", res)
	}
	if strings.Contains(res.Text, " ") {
		t.Error("empty page has text")
	}
}
