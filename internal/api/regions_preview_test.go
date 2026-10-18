package api

// The region OCR preview (docs/spec/09 "preview OCR variants and textbox
// events, … test-on-fixture"): the frame admission rules (upload cap, fixture
// allowlist, traversal refusal), the variant matrix, the presence
// classification, and the dev-trace event — plus one read of the real engine
// over a synthetic frame, so the stub plumbing and the tesseract path are
// both exercised (AGENTS.md "No simulation theater": a stub is a stub, and the
// real read is labelled as such).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// stubEngine is the preview's test engine: it never spawns a process and
// answers the same text for every variant, so a test asserting the plumbing
// can count variants without racing tesseract.
type stubEngine struct {
	text string
}

func (s stubEngine) Recognize(ctx context.Context, img image.Image) (ocr.Result, error) {
	return ocr.Result{Text: s.text, MeanConf: 0.9, Engine: "stub"}, nil
}

func (s stubEngine) Name() string { return "stub" }

// previewEnv is a catalogEnv whose handler additionally wires the OCR preview:
// the stub engine (or the real tesseract where a test names it) and the
// replay roots the fixture admits.
func previewEnv(t *testing.T, e *catalogEnv, factory ocrFactoryFunc, roots []string) {
	t.Helper()
	e.h = e.a.handler(
		WithStore(e.st), WithJobs(e.js), WithEvents(e.hub),
		WithImport(e.as, e.reg, e.runner),
		WithOCRPreview(factory, roots),
	)
}

// blackPNG is a small frame whose textbox crop is dark enough to trip the
// presence detector (share ≥ 0.30 below luma 90).
func blackPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png: %v", err)
	}
	return buf.Bytes()
}

// previewDoc is the response shape the assertions read.
type previewDoc struct {
	Source      string `json:"source"`
	FixturePath string `json:"fixture_path"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Engine      string `json:"engine"`
	DurationMS  int64  `json:"duration_ms"`
	Regions     []struct {
		ID      string `json:"id"`
		Empty   bool   `json:"empty"`
		Textbox struct {
			Present   bool    `json:"present"`
			DarkShare float64 `json:"dark_share"`
		} `json:"textbox"`
		Variants []struct {
			PSM            int     `json:"psm"`
			Preprocess     bool    `json:"preprocess"`
			Text           string  `json:"text"`
			MeanConfidence float64 `json:"mean_confidence"`
			DurationMS     int64   `json:"duration_ms"`
			Error          string  `json:"error"`
		} `json:"variants"`
	} `json:"regions"`
	TextboxVerdict string `json:"textbox_verdict"`
	DevTrace       bool   `json:"dev_trace"`
}

func (e *catalogEnv) preview(t *testing.T, role auth.Role, target string, body []byte, contentType string) previewDoc {
	t.Helper()
	rec := e.send(role, http.MethodPost, target, bytes.NewReader(body), "Content-Type", contentType)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("POST %s = %d, want 200 (body %s)", target, rec.Code, rec.Body.String())
	}
	var doc previewDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		e.t.Fatalf("decode preview: %v (%s)", err, rec.Body.String())
	}
	return doc
}

func TestPreviewFixtureRunsWithTheEngineVariants(t *testing.T) {
	e := newCatalogEnv(t)
	factory := func(psm int) ocr.Engine { return stubEngine{text: fmt.Sprintf("stub psm%d", psm)} }
	roots := []string{t.TempDir()}
	// The fixture frame: written under the root the preview admits.
	if err := os.WriteFile(roots[0]+"/frame.png", blackPNG(t, 640, 480), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	previewEnv(t, e, factory, roots)
	_, _, profileID, _ := newDraft(t, e, "preview-fixture", "pf")

	doc := e.preview(t, auth.RoleCurator, "/api/v1/profiles/"+profileID+"/regions/preview",
		[]byte(`{"fixture":"frame.png","regions":[{"x":0.07,"y":0.72,"w":0.86,"h":0.23}]}`),
		ContentTypeJSON)
	if doc.Source != "fixture" || doc.FixturePath != "frame.png" {
		t.Errorf("source/path = %s/%s, want fixture/frame.png", doc.Source, doc.FixturePath)
	}
	if doc.Width != 640 || doc.Height != 480 {
		t.Errorf("frame = %dx%d, want 640x480", doc.Width, doc.Height)
	}
	if doc.Engine != "stub" {
		t.Errorf("engine = %q, want the stub the test wired", doc.Engine)
	}
	if len(doc.Regions) != 1 {
		t.Fatalf("regions = %d, want 1", len(doc.Regions))
	}
	region := doc.Regions[0]
	if region.Empty {
		t.Errorf("the box covered a quarter of the frame and read empty")
	}
	if len(region.Variants) != 4 {
		t.Fatalf("variants = %d, want 4 (PSM 6/7 × preprocess on/off)", len(region.Variants))
	}
	seen := map[int]int{}
	for _, v := range region.Variants {
		seen[v.PSM]++
		if v.Error != "" {
			t.Errorf("variant psm%d/preprocess=%v failed: %s", v.PSM, v.Preprocess, v.Error)
		}
		if v.Text == "" {
			t.Errorf("variant psm%d/preprocess=%v read no text", v.PSM, v.Preprocess)
		}
	}
	if seen[6] != 2 || seen[7] != 2 {
		t.Errorf("variant spread = %v, want two PSM 6 and two PSM 7", seen)
	}
	// The crop is a black panel: the presence detector must call it present.
	if !region.Textbox.Present || region.Textbox.DarkShare < 0.9 {
		t.Errorf("textbox = %+v, want present with a high dark share", region.Textbox)
	}
	if doc.TextboxVerdict != "present" {
		t.Errorf("verdict = %q, want present", doc.TextboxVerdict)
	}
}

func TestPreviewMultipartUploadAndDevTrace(t *testing.T) {
	e := newCatalogEnv(t)
	previewEnv(t, e, func(psm int) ocr.Engine { return stubEngine{text: "stub line"} }, []string{t.TempDir()})
	_, _, profileID, _ := newDraft(t, e, "preview-upload", "pu")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("image", "capture.png")
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	if _, err := part.Write(blackPNG(t, 320, 240)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.WriteField("regions", `[{"x":0.05,"y":0.6,"w":0.9,"h":0.3}]`); err != nil {
		t.Fatalf("regions part: %v", err)
	}
	if err := mw.WriteField("dev_trace", "true"); err != nil {
		t.Fatalf("dev_trace part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rec := e.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/regions/preview", &buf, "Content-Type", mw.FormDataContentType())
	if rec.Code != http.StatusOK {
		e.t.Fatalf("POST preview = %d (body %s)", rec.Code, rec.Body.String())
	}
	var doc previewDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		e.t.Fatalf("decode: %v", err)
	}
	if doc.Source != "upload" || !doc.DevTrace {
		t.Errorf("source/dev_trace = %s/%v, want upload/true", doc.Source, doc.DevTrace)
	}
	if doc.Width != 320 || doc.Height != 240 {
		t.Errorf("frame = %dx%d, want 320x240", doc.Width, doc.Height)
	}

	// The dev trace is the durable event the developer inspector reads: the
	// regions.preview envelope, stored by the hub's store, with the frame in
	// its payload (the one place the image may be persisted).
	evStore := store.NewEventStore(e.st)
	page, err := evStore.After(e.ctx, 0, events.Filter{Names: []string{"regions.preview"}}, 100)
	if err != nil {
		t.Fatalf("event store After: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("stored %d regions.preview events, want 1", len(page))
	}
	if !strings.Contains(string(page[0].Data), "image_b64") {
		t.Errorf("the dev-trace event carries no image payload: %s", string(page[0].Data))
	}
	if !strings.Contains(string(page[0].Data), profileID) {
		t.Errorf("the dev-trace event does not name the profile it previewed")
	}
}

func TestPreviewUploadIsCappedAtFourMB(t *testing.T) {
	e := newCatalogEnv(t)
	previewEnv(t, e, func(psm int) ocr.Engine { return stubEngine{} }, nil)
	_, _, profileID, _ := newDraft(t, e, "preview-cap", "pc")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("image", "dump.png")
	// 4 MB + 1 of payload: the cap is a boundary rule, and the part's bytes
	// are read with a limiter, so the content does not matter.
	if _, err := part.Write(make([]byte, maxPreviewImageBytes+1)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	_ = mw.Close()

	rec := e.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/regions/preview", &buf, "Content-Type", mw.FormDataContentType())
	requireProblem(t, rec, http.StatusRequestEntityTooLarge, codeTooLarge)
}

func TestPreviewRefusesBadGeometryAndContentTypes(t *testing.T) {
	e := newCatalogEnv(t)
	roots := []string{t.TempDir()}
	previewEnv(t, e, func(psm int) ocr.Engine { return stubEngine{} }, roots)
	_, _, profileID, _ := newDraft(t, e, "preview-bad", "pb")

	// A box past the frame is a 400, not a crop that reads nothing.
	rec := e.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/regions/preview",
		bytes.NewReader([]byte(`{"fixture":"frame.png","regions":[{"x":0.5,"y":0.5,"w":0.7,"h":0.7}]}`)),
		"Content-Type", ContentTypeJSON)
	requireProblem(t, rec, http.StatusBadRequest, codeInvalidParameter)

	// A body that is neither multipart nor JSON is a 415 naming both.
	rec = e.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/regions/preview",
		bytes.NewReader([]byte("nope")), "Content-Type", "text/plain")
	if rec.Code != http.StatusUnsupportedMediaType {
		e.t.Fatalf("content type = %d, want 415", rec.Code)
	}

	// A traversal fixture is refused: the preview is not a path probe.
	rec = e.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/regions/preview",
		bytes.NewReader([]byte(`{"fixture":"../../etc/passwd"}`)),
		"Content-Type", ContentTypeJSON)
	requireProblem(t, rec, http.StatusBadRequest, codeInvalidParameter)
}

func TestPreviewWithoutEngineIsA503(t *testing.T) {
	e := newCatalogEnv(t) // no WithOCRPreview: the honest degradation
	_, _, profileID, _ := newDraft(t, e, "preview-noengine", "pn")
	rec := e.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/regions/preview",
		bytes.NewReader([]byte(`{"fixture":"frame.png"}`)),
		"Content-Type", ContentTypeJSON)
	if rec.Code != http.StatusServiceUnavailable {
		e.t.Fatalf("preview without an engine = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestPreviewRealEngineReadsTheSyntheticFrame is the non-stub half of the
// preview tests: the real tesseract binary over the repo's synthetic textbox
// frame, asserting the line the fixture's truth file records. It skips (and
// says so) when the binary is absent, the same discipline the doctor check
// applies — a skipped real-engine test is reported, never faked.
func TestPreviewRealEngineReadsTheSyntheticFrame(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("tesseract is not installed; the real-engine preview is unverified on this host")
	}
	e := newCatalogEnv(t)
	factory := func(psm int) ocr.Engine {
		return ocr.Tesseract{PSM: psm, Timeout: 15 * time.Second}
	}
	roots := []string{"../../testdata"}
	previewEnv(t, e, factory, roots)
	_, _, profileID, _ := newDraft(t, e, "preview-real", "pr")

	// The box the generator recorded for the line (testdata/synthetic/textbox/
	// truth.json, plain-dejavu-01.png: "The old lighthouse keeps no flame
	// these days.").
	doc := e.preview(t, auth.RoleCurator, "/api/v1/profiles/"+profileID+"/regions/preview",
		[]byte(`{"fixture":"synthetic/textbox/plain-dejavu-01.png","regions":[{"x":0.07,"y":0.72,"w":0.86,"h":0.23}]}`),
		ContentTypeJSON)
	if doc.Engine != "tesseract" {
		t.Fatalf("engine = %q, want tesseract", doc.Engine)
	}
	if len(doc.Regions) != 1 || doc.Regions[0].Empty {
		t.Fatalf("regions = %+v, want one non-empty box", doc.Regions)
	}
	read := false
	for _, v := range doc.Regions[0].Variants {
		if v.Error != "" {
			t.Logf("variant psm%d/preprocess=%v errored (tolerated in the matrix): %s", v.PSM, v.Preprocess, v.Error)
			continue
		}
		if strings.Contains(strings.ToLower(v.Text), "old lighthouse") {
			read = true
		}
	}
	if !read {
		t.Errorf("no variant read the synthetic line; variants: %+v", doc.Regions[0].Variants)
	}
}
