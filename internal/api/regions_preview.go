// Package api — the region OCR preview (docs/spec/09 "/games/:id/regions":
// "preview OCR variants and textbox events, … test-on-fixture").
//
// The preview is a what-if read, not a write: the editor sends a frame (either
// an uploaded capture or a fixture path under the configured replay roots) and
// the boxes it has on the canvas, and gets back, per box, four OCR variants —
// PSM 6/7 crossed with preprocess on/off, the settings a profile stores in
// regions.ocr_settings (docs/spec/05) — plus the textbox-presence classification
// the dialogue state machine uses (docs/spec/04 "Visual profiles") and the
// timings the page renders.
//
// The image itself never touches disk: it is held in memory for the duration
// of the request and dropped. The one exception is dev trace: a caller that
// asks for `dev_trace` gets a regions.preview event carrying the frame (PNG,
// base64, size-capped) in the durable event log — the same log the developer
// inspector reads — which is where a calibration screenshot belongs.

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

func init() {
	// The dev-trace event name belongs to this handler, so it registers the
	// vocabulary entry itself: an unregistered name fails to publish (the
	// durable log keeps its names permanent, docs/spec/08).
	if err := events.Register("regions.preview"); err != nil {
		panic(err)
	}
}

const (
	// maxPreviewImageBytes caps the upload: a 1080p frame PNG is a few
	// hundred KB, so 4 MB is far beyond any calibration capture and the cap
	// keeps a screenshot dump from holding the OCR pool hostage.
	maxPreviewImageBytes = 4 << 20
	// previewRequestTimeout bounds the whole preview — every box, every
	// variant — so a wedged engine cannot hold the request open past the
	// per-variant deadline.
	previewRequestTimeout = 60 * time.Second
	// previewOCRTimeout bounds one variant; the engine's own 4 s default is
	// for a frame-ordered pipeline, and the preview has no frame to lose.
	previewOCRTimeout = 10 * time.Second
	// devTraceImageCap is the largest frame the dev-trace event carries: the
	// event log is JSON in SQLite, and an unbounded base64 image in it is a
	// database that grows with every preview click.
	devTraceImageCap = 1 << 20
)

// ocrPreviewDeps is what the preview route needs: an engine factory keyed by
// PSM (the preview runs the same engine the runtime does — the point of the
// route is to see what the live pipeline would read) and the replay roots a
// `fixture` preview resolves against.
type ocrPreviewDeps struct {
	Factory     ocrFactoryFunc
	ReplayRoots []string
}

// ocrFactoryFunc builds one engine for one PSM. The default (production)
// factory is spelled in main.go; a test factory is a stub that never spawns a
// process, and the two are indistinguishable to the handler.
type ocrFactoryFunc func(psm int) ocr.Engine

// previewBox is one canvas box: normalized [0,1] geometry, and the id of the
// stored region it edits (empty for a box not saved yet).
type previewBox struct {
	ID string  `json:"id,omitempty"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	W  float64 `json:"w"`
	H  float64 `json:"h"`
}

// previewJSONRequest is the `fixture` form of the preview body.
type previewJSONRequest struct {
	Fixture  string       `json:"fixture"`
	Regions  []previewBox `json:"regions"`
	DevTrace bool         `json:"dev_trace"`
}

type previewResponse struct {
	Source         string                `json:"source"`
	FixturePath    string                `json:"fixture_path,omitempty"`
	Width          int                   `json:"width"`
	Height         int                   `json:"height"`
	Engine         string                `json:"engine"`
	DurationMS     int64                 `json:"duration_ms"`
	Regions        []previewRegionResult `json:"regions"`
	TextboxVerdict string                `json:"textbox_verdict"`
	DevTrace       bool                  `json:"dev_trace,omitempty"`
}

type previewRegionResult struct {
	ID       string           `json:"id,omitempty"`
	Box      previewBox       `json:"box"`
	Empty    bool             `json:"empty,omitempty"`
	Textbox  previewPresence  `json:"textbox"`
	Variants []previewVariant `json:"variants"`
}

type previewPresence struct {
	Present   bool    `json:"present"`
	DarkShare float64 `json:"dark_share"`
}

type previewVariant struct {
	PSM            int     `json:"psm"`
	Preprocess     bool    `json:"preprocess"`
	Text           string  `json:"text"`
	MeanConfidence float64 `json:"mean_confidence"`
	DurationMS     int64   `json:"duration_ms"`
	Error          string  `json:"error,omitempty"`
}

// handleProfileRegionsPreview serves POST /api/v1/profiles/{id}/regions/preview.
// Two content types: multipart with an `image` part (≤ 4 MB) plus optional
// `regions`/`dev_trace` parts, or JSON naming a `fixture` under the replay
// roots. Boxes default to the profile's stored regions when the request names
// none, so "test on fixture" against the saved set is one call.
func (s *Server) handleProfileRegionsPreview(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	if s.ocrPreview.Factory == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "ocr-unavailable", "SUBSYSTEM_UNAVAILABLE",
			"Service Unavailable",
			"no OCR engine is wired in this process; the preview answers nothing rather than an empty read that reads as a blank textbox")
		return
	}
	p, _, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), previewRequestTimeout)
	defer cancel()

	var (
		img         image.Image
		source      string
		fixturePath string
		boxes       []previewBox
		devTrace    bool
	)
	ctype, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ctype {
	case "multipart/form-data":
		var perr *previewError
		img, boxes, devTrace, perr = s.decodePreviewMultipart(r, p.ID)
		if perr != nil {
			code := codeInvalidParameter
			if perr.status == http.StatusRequestEntityTooLarge {
				code = codeTooLarge // the 413 body names the boundary that fired
			}
			s.writeCodeProblem(w, r, perr.status, perr.slug, code, perr.title, perr.detail)
			return
		}
		source = "upload"
	case ContentTypeJSON:
		var req previewJSONRequest
		if !s.decodeJSONBody(w, r, &req, maxPreviewImageBytes) {
			return
		}
		if req.Fixture == "" {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "missing-fixture", codeInvalidParameter,
				"Bad Request", "the JSON preview body requires fixture (a frame path under the replay roots)")
			return
		}
		if err := validatePreviewBoxes(req.Regions); err != nil {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "region-geometry", codeInvalidParameter,
				"Bad Request", err.Error())
			return
		}
		var ferr error
		img, fixturePath, ferr = s.readPreviewFixture(req.Fixture)
		if ferr != nil {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "fixture-unavailable", codeInvalidParameter,
				"Bad Request", ferr.Error())
			return
		}
		boxes, devTrace = req.Regions, req.DevTrace
		source = "fixture"
	default:
		s.writeCodeProblem(w, r, http.StatusUnsupportedMediaType, "unsupported-media", "UNSUPPORTED_MEDIA_TYPE",
			"Unsupported Media Type", "send multipart/form-data with an image part, or application/json with a fixture path")
		return
	}

	if len(boxes) == 0 {
		boxes = s.storedPreviewBoxes(ctx, p.ID)
		if len(boxes) == 0 {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "no-regions", codeInvalidParameter,
				"Bad Request", "no boxes to preview: the request named none and the revision has no stored regions")
			return
		}
	}
	if img == nil {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "unreadable-image", codeInvalidParameter,
			"Bad Request", "the image could not be decoded")
		return
	}

	out, devTraceErr := s.runPreview(ctx, img, boxes)
	out.Source = source
	out.FixturePath = fixturePath
	if devTrace {
		if s.hub == nil {
			s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "dev-trace-unavailable", "SUBSYSTEM_UNAVAILABLE",
				"Service Unavailable", "dev trace was requested but no event hub is wired in this process")
			return
		}
		data := map[string]any{
			"profile_id": p.ID, "source": source, "width": out.Width, "height": out.Height,
			"regions": out.Regions, "duration_ms": out.DurationMS,
		}
		if fixturePath != "" {
			data["fixture_path"] = fixturePath
		}
		if source == "upload" {
			if b, err := pngEncode(img); err == nil && len(b) <= devTraceImageCap {
				data["image_b64"] = base64.StdEncoding.EncodeToString(b)
			}
		}
		raw, err := json.Marshal(data)
		if err != nil {
			s.writeCodeProblem(w, r, http.StatusInternalServerError, "dev-trace-encode", "INTERNAL_ERROR",
				"Internal Error", "the dev-trace payload could not be encoded")
			return
		}
		e := events.Envelope{Name: "regions.preview", TraceID: games.NewID(), Data: raw}
		pctx, pcancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer pcancel()
		if _, err := s.hub.Publish(pctx, e); err != nil && !errors.Is(err, events.ErrClosed) {
			devTraceErr = err.Error() // the preview itself succeeded; report the trace drop
		} else if err == nil {
			out.DevTrace = true
		}
	}
	if devTraceErr != "" {
		s.writeCodeProblem(w, r, http.StatusBadGateway, "dev-trace-failed", "DEV_TRACE_FAILED",
			"Bad Gateway", "the preview ran but the dev-trace event could not be stored: "+devTraceErr)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// previewError is a typed 4xx the multipart/fixture decoders answer.
type previewError struct {
	status int
	slug   string
	title  string
	detail string
}

func (e *previewError) Error() string { return e.title + ": " + e.detail }

// decodePreviewMultipart reads the form parts: image (file, ≤ cap), regions
// (JSON array) and dev_trace (bool). The image is bounded while read, so an
// oversized upload is a 413 at the boundary, not a heap that grows into one.
func (s *Server) decodePreviewMultipart(r *http.Request, profileID string) (image.Image, []previewBox, bool, *previewError) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, false, &previewError{http.StatusBadRequest, "multipart-invalid", "Bad Request",
			"the multipart body is malformed: " + err.Error()}
	}
	var (
		img      image.Image
		boxes    []previewBox
		devTrace bool
		seen     = map[string]bool{}
	)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, false, &previewError{http.StatusBadRequest, "multipart-invalid", "Bad Request",
				"the multipart body is malformed: " + err.Error()}
		}
		name := part.FormName()
		switch name {
		case "image":
			if seen[name] {
				return nil, nil, false, &previewError{http.StatusBadRequest, "duplicate-part", "Bad Request",
					"the image part may appear exactly once"}
			}
			raw, err := io.ReadAll(io.LimitReader(part, maxPreviewImageBytes+1))
			if err != nil {
				return nil, nil, false, &previewError{http.StatusBadRequest, "multipart-read", "Bad Request",
					"reading the image part failed: " + err.Error()}
			}
			if len(raw) > maxPreviewImageBytes {
				return nil, nil, false, &previewError{http.StatusRequestEntityTooLarge, "image-too-large",
					"Request Entity Too Large",
					fmt.Sprintf("the image part must be at most %d bytes (4 MB)", maxPreviewImageBytes)}
			}
			dec, _, err := image.Decode(bytes.NewReader(raw))
			if err != nil {
				return nil, nil, false, &previewError{http.StatusBadRequest, "image-unreadable", "Bad Request",
					"the image part is not a decodable image: " + err.Error()}
			}
			img = dec
		case "regions":
			var list []previewBox
			if err := json.NewDecoder(io.LimitReader(part, 1<<20)).Decode(&list); err != nil {
				return nil, nil, false, &previewError{http.StatusBadRequest, "regions-invalid", "Bad Request",
					"the regions part must be a JSON array of {id,x,y,w,h}: " + err.Error()}
			}
			boxes = list
		case "dev_trace":
			b, _ := io.ReadAll(io.LimitReader(part, 16))
			devTrace = b[0] == 't' || b[0] == 'T' || b[0] == '1'
		}
		seen[name] = true
	}
	if img == nil {
		return nil, nil, false, &previewError{http.StatusBadRequest, "missing-image", "Bad Request",
			"the multipart body requires an image part"}
	}
	for i, b := range boxes {
		if !validPreviewBox(b) {
			return nil, nil, false, &previewError{http.StatusBadRequest, "region-geometry", "Bad Request",
				fmt.Sprintf("box %d is not a normalized [0,1] box inside the frame", i)}
		}
	}
	return img, boxes, devTrace, nil
}

// readPreviewFixture resolves a fixture path under the replay roots and decodes
// it. The allowlist is the same discipline the replay frame source applies
// (ADR-0024): a relative path, no traversal, resolved inside a configured root
// — so the preview cannot be a path probe for the host filesystem.
func (s *Server) readPreviewFixture(fixture string) (image.Image, string, error) {
	f := strings.TrimSpace(fixture)
	if f == "" {
		return nil, "", fmt.Errorf("fixture is empty")
	}
	if path.IsAbs(f) || path.Clean(f) != f {
		return nil, "", fmt.Errorf("fixture must be a clean relative path, e.g. synthetic/textbox/plain-dejavu-01.png")
	}
	for _, comp := range strings.Split(f, "/") {
		if comp == ".." || comp == "." {
			return nil, "", fmt.Errorf("fixture must not contain . or .. components")
		}
	}
	var lastErr error
	for _, root := range s.ocrPreview.ReplayRoots {
		candidate := root + "/" + f
		raw, err := os.ReadFile(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		if int64(len(raw)) > maxPreviewImageBytes {
			return nil, "", fmt.Errorf("fixture %s exceeds the 4 MB preview cap", f)
		}
		dec, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, "", fmt.Errorf("fixture %s is not a decodable image: %v", f, err)
		}
		return dec, f, nil
	}
	if len(s.ocrPreview.ReplayRoots) == 0 {
		return nil, "", fmt.Errorf("no replay roots are configured in this process, so no fixture preview is possible")
	}
	return nil, "", fmt.Errorf("fixture %s is not under the configured replay roots: %v", f, lastErr)
}

// storedPreviewBoxes is the fallback box list: the revision's stored regions,
// so a fixture test against the calibrated set needs no body at all.
func (s *Server) storedPreviewBoxes(ctx context.Context, profileID string) []previewBox {
	rows, err := s.store.ListRegions(ctx, profileID)
	if err != nil {
		return nil
	}
	out := make([]previewBox, 0, len(rows))
	for _, rg := range rows {
		out = append(out, previewBox{ID: rg.ID, X: rg.RectX, Y: rg.RectY, W: rg.RectW, H: rg.RectH})
	}
	return out
}

// runPreview crops, classifies and OCRs every box. Per-variant failures are
// carried in the variant, not thrown: the page's whole job is to show which
// setting reads the box and which does not, and one wedged variant must not
// blank the other three.
func (s *Server) runPreview(ctx context.Context, img image.Image, boxes []previewBox) (*previewResponse, string) {
	bounds := img.Bounds()
	out := &previewResponse{
		Width:   bounds.Dx(),
		Height:  bounds.Dy(),
		Regions: make([]previewRegionResult, 0, len(boxes)),
	}
	if n, ok := s.ocrPreview.Factory(ocr.DefaultPSM).(ocr.Namer); ok {
		out.Engine = n.Name()
	}
	start := time.Now()
	for _, b := range boxes {
		rr := previewRegionResult{ID: b.ID, Box: b, Variants: make([]previewVariant, 0, 4)}
		crop := vision.CropNormalized(img, vision.Rect{X: b.X, Y: b.Y, W: b.W, H: b.H}, vision.CropOptions{})
		if crop == nil || crop.Bounds().Empty() {
			rr.Empty = true
			out.Regions = append(out.Regions, rr)
			continue
		}
		share := darkShare(vision.ToGray(crop))
		rr.Textbox = previewPresence{Present: share >= presenceMinDarkShare, DarkShare: share}
		for _, psm := range []int{ocr.DefaultPSM, ocr.DefaultPSMLine} {
			for _, prep := range []bool{true, false} {
				input := image.Image(crop)
				if prep {
					input = vision.Preprocess(crop, vision.DefaultSteps())
				}
				v := s.runVariant(ctx, psm, prep, input)
				rr.Variants = append(rr.Variants, v)
			}
		}
		if rr.Textbox.Present {
			out.TextboxVerdict = "present"
		}
		out.Regions = append(out.Regions, rr)
	}
	if out.TextboxVerdict == "" {
		out.TextboxVerdict = "absent"
	}
	out.DurationMS = time.Since(start).Milliseconds()
	return out, ""
}

// runVariant is one (PSM, preprocess) cell of the preview matrix.
func (s *Server) runVariant(ctx context.Context, psm int, prep bool, input image.Image) previewVariant {
	v := previewVariant{PSM: psm, Preprocess: prep}
	vctx, cancel := context.WithTimeout(ctx, previewOCRTimeout)
	defer cancel()
	start := time.Now()
	res, err := s.ocrPreview.Factory(psm).Recognize(vctx, input)
	v.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		switch {
		case errors.Is(err, ocr.ErrEngineUnavailable):
			v.Error = "ocr engine unavailable: tesseract is not installed or not on PATH"
		case errors.Is(err, ocr.ErrTimeout):
			v.Error = "ocr timed out"
		case errors.Is(err, ocr.ErrCancelled):
			v.Error = "preview cancelled"
		default:
			v.Error = "ocr failed: " + err.Error()
		}
		return v
	}
	v.Text = strings.TrimSpace(res.Text)
	v.MeanConfidence = res.MeanConf
	return v
}

// presenceMinDarkShare and presenceDarkLuma mirror the runtime's panel
// detector defaults (internal/pipeline PresenceConfig.DefaultPresence,
// measured on the synthetic corpus): the preview must classify the way the
// live pipeline will, or the editor calibrates against a different rule than
// the one that decides at play time.
const (
	presenceDarkLuma     = 90
	presenceMinDarkShare = 0.30
)

// darkShare is the fraction of a gray crop's pixels below presenceDarkLuma —
// the runtime's textbox-presence measurement, spelled here over vision.ToGray
// because the preview and the pipeline must agree on the number they compare.
func darkShare(g *image.Gray) float64 {
	if g == nil || len(g.Pix) == 0 {
		return 0
	}
	dark := 0
	for _, v := range g.Pix {
		if v < presenceDarkLuma {
			dark++
		}
	}
	return float64(dark) / float64(len(g.Pix))
}

// validPreviewBox is the geometry guard for request-supplied boxes: every
// coordinate finite, in [0,1], and the box inside the unit square — the same
// rule games.Region.Validate enforces on the stored rows, applied here so a
// nonsense box is a 400 rather than a crop that reads nothing.
func validPreviewBox(b previewBox) bool {
	for _, f := range []float64{b.X, b.Y, b.W, b.H} {
		if f != f || f < 0 || f > 1 { // NaN check, then bounds
			return false
		}
	}
	return b.X+b.W <= 1 && b.Y+b.H <= 1
}

// validatePreviewBoxes is the per-box guard over a whole request: the first
// bad box is reported with its index, which is what the editor needs to find
// the box on the canvas.
func validatePreviewBoxes(boxes []previewBox) error {
	for i, b := range boxes {
		if !validPreviewBox(b) {
			return fmt.Errorf("box %d is not a normalized [0,1] box inside the frame", i)
		}
	}
	return nil
}

func pngEncode(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// The standard errors.Is is used at the switch sites above; no wrapper.
