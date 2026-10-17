package scenario

import (
	"strings"
	"testing"
)

// The documents below are the shape docs/spec/17 fixes: block maps, a nested
// map, a list of mappings and a list of scalars. They also carry the comment
// styles and scalar typings the loader must survive or reject with a line
// number.

const validDoc = `
# full-line comment
schema_version: 1   # trailing comment
id: synthetic/dialogue-advance-001
origin: generated_synthetic
profile: dialogue-advance-001
mode: frame_replay
input:
  frames_dir: testdata/synthetic/replay/dialogue-advance-001
  timestamps: timestamps.json
inference:
  decision: none
  tts: none
assertions:
  - type: message_detected
    message_id: demo-page-1
    max_delay_ms: 1500
  - type: events_order
    names:
      - text.observed
      - message.committed
`

func TestParseYAMLValidShape(t *testing.T) {
	root, err := parseYAML(validDoc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v, line, ok := root.scalarInt("schema_version"); !ok || v != 1 {
		t.Fatalf("schema_version = %v at line %d, want 1", v, line)
	}
	if v, _, ok := root.scalarStr("origin"); !ok || v != "generated_synthetic" {
		t.Fatalf("origin = %q, want generated_synthetic", v)
	}
	input, ok := root.child("input")
	if !ok || input.kind != ymap {
		t.Fatalf("input is not a mapping: kind=%v", input)
	}
	if v, _, ok := input.scalarStr("frames_dir"); !ok || v != "testdata/synthetic/replay/dialogue-advance-001" {
		t.Fatalf("input.frames_dir = %q", v)
	}
	inf, ok := root.child("inference")
	if !ok {
		t.Fatal("inference missing")
	}
	if v, _, ok := inf.scalarStr("tts"); !ok || v != "none" {
		t.Fatalf("inference.tts = %q", v)
	}
	items, line, ok := root.listAt("assertions")
	if !ok || len(items) != 2 {
		t.Fatalf("assertions: got %d items (line %d), want 2", len(items), line)
	}
	first := items[0]
	if v, _, ok := first.scalarStr("type"); !ok || v != "message_detected" {
		t.Fatalf("assertions[0].type = %q", v)
	}
	if v, l, ok := first.scalarInt("max_delay_ms"); !ok || v != 1500 {
		t.Fatalf("assertions[0].max_delay_ms = %d at line %d, want 1500", v, l)
	}
	if v, _, ok := items[1].scalarStr("type"); !ok || v != "events_order" {
		t.Fatalf("assertions[1].type = %q", v)
	}
	names, ok := items[1].child("names")
	if !ok || names.kind != ylist || len(names.items) != 2 {
		t.Fatalf("assertions[1].names: kind=%v len=%d", names.kind, len(names.items))
	}
	if names.items[0].str != "text.observed" || names.items[1].str != "message.committed" {
		t.Fatalf("names = %q, %q", names.items[0].str, names.items[1].str)
	}
}

func TestParseYAMLCommentsAndCRLF(t *testing.T) {
	doc := "# header\r\na: 1  # tail comment\r\nb: \"x # not a comment\"\r\n"
	root, err := parseYAML(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v, _, ok := root.scalarStr("b"); !ok || v != "x # not a comment" {
		t.Fatalf("b = %q, want the quoted value with its '#'", v)
	}
}

func TestParseYAMLScalarTyping(t *testing.T) {
	doc := "a: 12\nb: 0.25\nc: true\nd: \"1500\"\ne: 1_0\nf: null\ng: some text\n"
	root, err := parseYAML(doc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v, _, ok := root.scalarInt("a"); !ok || v != 12 {
		t.Fatalf("a = %d (ok=%v), want int 12", v, ok)
	}
	b, ok := root.child("b")
	if !ok || !b.hasF || b.fnum != 0.25 {
		t.Fatalf("b is not the float 0.25")
	}
	if v, _, ok := root.scalarBool("c"); !ok || !v {
		t.Fatalf("c = %v (ok=%v), want true", v, ok)
	}
	// A quoted number stays a string: the author typed text, not a number.
	if v, _, ok := root.scalarInt("d"); ok || v != 0 {
		t.Fatalf("d must not parse as an int (got %d, ok=%v)", v, ok)
	}
	// Underscores are not numeric syntax in this subset.
	if v, _, ok := root.scalarInt("e"); ok || v != 0 {
		t.Fatalf("e must not parse as an int (got %d, ok=%v)", v, ok)
	}
	// null is an empty scalar, not the string "null".
	if v, _, ok := root.scalarStr("f"); !ok || v != "" {
		t.Fatalf("f: null is an empty scalar (got %q, ok=%v)", v, ok)
	}
	if v, _, ok := root.scalarStr("g"); !ok || v != "some text" {
		t.Fatalf("g = %q", v)
	}
}

// errDocs pairs a document with the line number its error must name.
func TestParseYAMLErrors(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		// wantLine, when non-zero, must appear in the error text.
		wantLine int
	}{
		{"bad indentation at key level", "a: 1\n   b: 2\n", 2},
		{"indented top-level key", " a: 1\n", 1},
		{"tab indentation", "a: 1\n\tb: 2\n", 2},
		{"missing colon", "a 1\nb: 2\n", 1},
		{"key with spaces", "bad key: 1\n", 1},
		{"duplicate key", "a: 1\na: 2\n", 2},
		{"key without value", "a:\nb: 1\n", 1},
		{"key without value at eof", "a:\n", 1},
		{"list items misaligned", "a:\n  - type: x\n   message_id: y\n", 3},
		{"list dash at wrong indent", "a:\n  - type: x\n   - type: y\n", 3},
		{"unterminated quote", `a: "x\n`, 1},
		{"empty document", "# only a comment\n", 0},
		{"top level not a map", "- a\n- b\n", 1},
		{"list item where key expected", "a: 1\n- b: 2\n", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseYAML(c.doc)
			if err == nil {
				t.Fatalf("want error, got none")
			}
			if c.wantLine != 0 {
				want := "line " + itoa(c.wantLine) + ":"
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %s", err, want)
				}
			}
			if c.wantLine == 0 && !strings.Contains(err.Error(), "line ") && c.name != "empty document" {
				t.Fatalf("error has no line number: %v", err)
			}
		})
	}
}

func TestParseYAMLScalarListItemRejectedForKeys(t *testing.T) {
	// A dash line whose rest looks like neither 'key: value' nor a scalar
	// (empty after the dash with no block) is an error, not a silent skip.
	_, err := parseYAML("a:\n  -\n")
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want a line-2 error for an empty list item, got %v", err)
	}
}

// itoa keeps the test free of a strconv import; the parser owns that job.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
