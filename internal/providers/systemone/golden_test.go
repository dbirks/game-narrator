package systemone

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden test pins the wire contract: model + state required, questions
// as an id-keyed object in deterministic (slice) order, images as base64 data
// URLs, and — critically — no top-level "instructions" field, which the
// gateway rejects for this checkpoint (docs/spec/06 "System One / Clef-Omni
// protocol").

// goldenJPEG is a minimal byte sequence with the JPEG magic number (SOI +
// APP0 + EOI); the content never has to decode, only be recognisable.
var goldenJPEG = []byte{
	0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00,
	0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x08, 0x00, 0x00,
	0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xD9,
}

// goldenPNG renders a fixed 2x2 image so the encoding is reproducible.
func goldenPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return buf.Bytes()
}

func goldenRequest(t *testing.T) Request {
	t.Helper()
	// The state map literal is intentionally NOT in sorted key order; the
	// encoder must sort it so logical equality implies byte equality.
	return Request{
		Model: "clef-omni",
		State: map[string]any{
			"scene":        "outset",
			"last_message": "m-11",
			"ocr":          "Would you like to save your progress?",
		},
		Images: [][]byte{goldenPNG(t), goldenJPEG},
		Questions: []Question{
			{
				ID:           "text_kind",
				Type:         TypeChoice,
				Instructions: "What kind of text is visible?",
				Criteria: []Option{
					{ID: "dialogue", Description: "a character speaks"},
					{ID: "sign", Description: "an interacted sign"},
					{ID: "none", Description: "no narratable text"},
				},
			},
			{ID: "is_textbox", Type: TypeNoul, Instructions: "Is the visible UI region a textbox?"},
			{ID: "risk_level", Type: TypeScore, Instructions: "Rate the risk of speaking now", Levels: []string{"low", "medium", "high"}},
		},
	}
}

// TestGoldenRequestSerialization pins the exact bytes the client sends.
func TestGoldenRequestSerialization(t *testing.T) {
	req := goldenRequest(t)
	got, err := req.encodeRequest()
	if err != nil {
		t.Fatalf("encodeRequest: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "golden-request.json"))
	if err != nil {
		t.Fatalf("read golden: %v (regenerate with make test-unit-update or `go test -run TestGolden ./internal/providers/systemone/` after a protocol change)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("wire bytes diverged from the golden:\n got: %s\nwant: %s", got, want)
	}
}

// TestRequestEncodingIsDeterministic proves the same logical request always
// encodes to the same bytes, regardless of the map construction order of its
// state or the slice ordering the caller chose.
func TestRequestEncodingIsDeterministic(t *testing.T) {
	base := goldenRequest(t)
	a, err := base.encodeRequest()
	if err != nil {
		t.Fatalf("encode #1: %v", err)
	}
	// Rebuild the same logical request with the state map declared in the
	// opposite order and the questions slice in the same order.
	rebuilt := base
	rebuilt.State = map[string]any{
		"ocr":          base.State["ocr"],
		"last_message": base.State["last_message"],
		"scene":        base.State["scene"],
	}
	b, err := rebuilt.encodeRequest()
	if err != nil {
		t.Fatalf("encode #2: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("same logical request encoded differently:\n a: %s\n b: %s", a, b)
	}
}

// TestRequestEncodingKeyOrder pins the two ordering rules: question keys
// follow the slice order (a map would not), and the criteria keys inside one
// question are sorted (a Go map).
func TestRequestEncodingKeyOrder(t *testing.T) {
	req := goldenRequest(t)
	raw, err := req.encodeRequest()
	if err != nil {
		t.Fatalf("encodeRequest: %v", err)
	}
	s := string(raw)

	// Question keys must appear in slice order: text_kind, is_textbox, risk_level.
	ikind := strings.Index(s, `"text_kind":`)
	itextbox := strings.Index(s, `"is_textbox":`)
	irisk := strings.Index(s, `"risk_level":`)
	if ikind < 0 || itextbox < 0 || irisk < 0 || !(ikind < itextbox && itextbox < irisk) {
		t.Fatalf("question keys are not in slice order: kind=%d textbox=%d risk=%d", ikind, itextbox, irisk)
	}
	// Criteria keys inside text_kind must be sorted: dialogue < none < sign.
	idlg := strings.Index(s, `"dialogue":`)
	inone := strings.Index(s, `"none":`)
	ismg := strings.Index(s, `"sign":`)
	if idlg < 0 || inone < 0 || ismg < 0 || !(idlg < inone && inone < ismg) {
		t.Fatalf("criteria keys are not sorted: dialogue=%d none=%d sign=%d", idlg, inone, ismg)
	}
	// State keys are sorted by encoding/json: last_message < ocr < scene.
	ilm := strings.Index(s, `"last_message":`)
	iocr := strings.Index(s, `"ocr":`)
	isce := strings.Index(s, `"scene":`)
	if ilm < 0 || iocr < 0 || isce < 0 || !(ilm < iocr && iocr < isce) {
		t.Fatalf("state keys are not sorted: last_message=%d ocr=%d scene=%d", ilm, iocr, isce)
	}
}

// TestRequestEncodingHasNoTopLevelInstructions enforces the contract the
// gateway actually enforces (verified 422: "model and state are required" and
// the checkpoint rejects top-level instructions, docs/spec/06).
func TestRequestEncodingHasNoTopLevelInstructions(t *testing.T) {
	req := goldenRequest(t)
	raw, err := req.encodeRequest()
	if err != nil {
		t.Fatalf("encodeRequest: %v", err)
	}
	var top struct {
		Model        string          `json:"model"`
		State        map[string]any  `json:"state"`
		Images       []string        `json:"images"`
		Questions    map[string]any  `json:"questions"`
		Instructions json.RawMessage `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("encoded body is not an object: %v", err)
	}
	if len(top.Instructions) != 0 {
		t.Fatalf("top-level instructions present: %s", top.Instructions)
	}
	if top.Model != "clef-omni" || len(top.State) == 0 || len(top.Questions) == 0 {
		t.Fatalf("required envelope fields missing: %+v", top)
	}
	// Images must be base64 data URLs with honest media types.
	if len(top.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(top.Images))
	}
	pngURL, jpegURL := top.Images[0], top.Images[1]
	if !strings.HasPrefix(pngURL, "data:image/png;base64,") {
		t.Fatalf("png data URL = %q...", pngURL[:min(30, len(pngURL))])
	}
	if !strings.HasPrefix(jpegURL, "data:image/jpeg;base64,") {
		t.Fatalf("jpeg data URL = %q...", jpegURL[:min(30, len(jpegURL))])
	}
	if _, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(pngURL, "data:image/png;base64,")); err != nil {
		t.Fatalf("png payload is not base64: %v", err)
	}
}

// TestRegenerateGolden is a maintenance seam: with UPDATE_GOLDEN=1 it rewrites
// testdata/golden-request.json from the current encoder. Run it deliberately
// after a protocol change and inspect the diff before committing.
func TestRegenerateGolden(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") == "" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate testdata/golden-request.json")
	}
	req := goldenRequest(t)
	raw, err := req.encodeRequest()
	if err != nil {
		t.Fatalf("encodeRequest: %v", err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "golden-request.json"), raw, 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
	t.Logf("regenerated golden request (%d bytes)", len(raw))
}
