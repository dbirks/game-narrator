package api

// Handler tests for the session lifecycle and developer inspection routes
// (ADR-0024). The environment is the production shape: a real SQLite store
// with the synthetic profile imported through the store's own write path, a
// real event hub over the memory store, and a real runtime.Manager whose only
// test seam is the OCR engine (the pipeline's own harness script, keyed on
// the clip's truth). No session state is faked: the 201 comes from a pipeline
// that actually ran the replay, and the dev-trace asset served here is the
// JPEG the recorder actually stored.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// Fixture locations, relative to the repository root (tests run from
// internal/api, hence ../../).
const (
	sessSyntheticDir = "../../testdata/synthetic/replay/dialogue-advance-001"
	sessProfilePath  = "../../testdata/synthetic/profiles/dialogue-advance-001.json"
)

type sessionEnv struct {
	t         *testing.T
	a         *testAuth
	st        *store.Store
	mgr       *runtime.Manager
	h         http.Handler
	profileID string
}

// newSessionEnv wires the full session surface: real store, hub, runtime and
// the API server with every session option set. The synthetic profile is
// imported and published through the store's write path, as the runtime tests
// do; its id is on the env for the tests that start sessions against it.
func newSessionEnv(t *testing.T) *sessionEnv {
	t.Helper()
	a := newTestAuth(t)
	e := &sessionEnv{t: t, a: a, st: a.st}
	e.profileID = seedSyntheticProfileForAPI(t, a.st, true)
	clipDir, _ := filepath.Abs(sessSyntheticDir)

	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	mgr, err := runtime.New(runtime.Options{
		Store:       a.st,
		Hub:         hub,
		AssetsRoot:  t.TempDir(),
		ReplayRoots: []string{clipDir},
		OCR:         newTruthOCR(t),
		Log:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	// Cleanups run LIFO: manager (stops live sessions, waits for terminal
	// events) before hub, hub before store.
	t.Cleanup(func() { _ = a.st.Close() })
	t.Cleanup(func() { _ = hub.Close() })
	t.Cleanup(mgr.Close)

	e.mgr = mgr
	e.h = a.handler(
		WithStore(a.st),
		WithEvents(hub),
		WithDevEvents(mem),
		WithSessions(mgr),
	)
	return e
}

// send performs one request as a role. For every 2xx it validates the body
// against the OpenAPI operation documenting the route (contract clause c),
// the same guarantee the catalog flows get: a handler that starts returning a
// differently-shaped body fails here, not only in a schema test.
func (e *sessionEnv) send(role auth.Role, method, target, payload string) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	if payload != "" {
		body = strings.NewReader(payload)
	}
	r := httptest.NewRequest(method, target, body)
	if body != nil {
		r.Header.Set("Content-Type", ContentTypeJSON)
	}
	if role != auth.RoleNone {
		r.Header.Set("Authorization", "Bearer "+e.a.bearer(role))
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code >= 200 && rec.Code < 300 {
		recordContractResponse(e.t, method, target, rec.Code,
			rec.Header().Get("Content-Type"), rec.Body.Bytes())
	}
	return rec
}

func (e *sessionEnv) mustOK(role auth.Role, target string) map[string]any {
	e.t.Helper()
	rec := e.send(role, http.MethodGet, target, "")
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET %s = %d, want 200 (body %s)", target, rec.Code, rec.Body.String())
	}
	return decodeJSON(e.t, rec)
}

// seedSyntheticProfileForAPI imports the synthetic profile through the store's
// own write path (friendly ids mapped to ULIDs, the production import's
// behaviour). A compact copy of the runtime tests' seeder: this package may
// not import the runtime test helpers, and the seeding rules are a store
// contract both must respect.
func seedSyntheticProfileForAPI(t *testing.T, st *store.Store, publish bool) string {
	t.Helper()
	profile, err := scenario.LoadProfile(sessProfilePath)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	// A unique slug per seed: the same store can be seeded more than once in a
	// test (a published profile for the happy path plus an unpublished one for
	// the refusal case), and games.slug is UNIQUE.
	seedSlugSeq.Add(1)
	game := games.Game{ID: games.NewID(), Slug: fmt.Sprintf("synthetic-demo-%d", seedSlugSeq.Load()),
		Title: "Synthetic Demo", Platform: "gamecube",
		UserTags: []byte(`["synthetic"]`), CreatedAt: now}
	if err := st.CreateGame(ctx, game); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	edition := games.Edition{ID: games.NewID(), GameID: game.ID, Region: "US",
		Language: "en", Revision: "rev0", Locale: "en_US"}
	if err := st.CreateEdition(ctx, edition); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	draft, err := st.CreateDraftProfile(ctx, edition.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	byFriendly := map[string]string{}
	msgs := make([]games.Message, 0, len(profile.Messages))
	for _, m := range profile.ToMessages() {
		id := games.NewID()
		byFriendly[m.ID] = id
		row := m
		row.ID, row.ProfileID, row.SourceMessageID = id, draft.ID, m.ID
		row.AttributionStatus = games.AttributionUnknown
		row.Revision = 1
		row.CreatedAt, row.UpdatedAt = now, now
		msgs = append(msgs, row)
	}
	if err := st.PutMessages(ctx, draft.ID, msgs, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	regions := make([]games.Region, 0, len(profile.Regions))
	for _, r := range profile.ToRegions() {
		r.ID, r.ProfileID = games.NewID(), draft.ID
		r.DetectMethod = games.DetectManual
		r.Revision = 1
		r.UpdatedAt = now
		regions = append(regions, r)
	}
	if err := st.PutRegions(ctx, draft.ID, regions); err != nil {
		t.Fatalf("PutRegions: %v", err)
	}
	edges := profile.ToEdges()
	for i := range edges {
		edges[i].FromID = byFriendly[edges[i].FromID]
		edges[i].ToID = byFriendly[edges[i].ToID]
		edges[i].CreatedAt = now
	}
	if err := st.PutEdges(ctx, draft.ID, edges); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}
	_, rev, err := st.GetProfile(ctx, draft.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if !publish {
		return draft.ID
	}
	if err := st.SetProfileStatus(ctx, draft.ID, games.StatusReview, rev); err != nil {
		t.Fatalf("SetProfileStatus: %v", err)
	}
	_, rev, err = st.GetProfile(ctx, draft.ID) // SetProfileStatus moves the row version
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	published, err := st.Publish(ctx, draft.ID, rev, "test-author@local")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return published.ID
}

// truthOCR is the scripted engine from the pipeline's own harness: the
// pipeline hands it the preprocessed crop, so the key is the hash of the
// image as received.
type truthOCR struct {
	script map[string]string
	calls  atomic.Int64
}

func newTruthOCR(t *testing.T) *truthOCR {
	t.Helper()
	truth, err := scenario.LoadTruth(sessSyntheticDir)
	if err != nil {
		t.Fatalf("load truth: %v", err)
	}
	profile, err := scenario.LoadProfile(sessProfilePath)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	out := map[string]string{}
	for _, row := range truth {
		f, err := os.Open(filepath.Join(sessSyntheticDir, row.File))
		if err != nil {
			t.Fatalf("open frame %s: %v", row.File, err)
		}
		img, _, err := image.Decode(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("decode frame %s: %v", row.File, err)
		}
		rgba := image.NewRGBA(img.Bounds())
		draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
		for _, reg := range profile.Regions {
			crop := vision.CropNormalized(rgba, vision.Rect{X: reg.X, Y: reg.Y, W: reg.W, H: reg.H}, vision.CropOptions{})
			if crop != nil && crop.Bounds().Dx() > 0 {
				out[vision.ImageHash(vision.Preprocess(crop, vision.DefaultSteps()))] = row.Text
			}
		}
	}
	return &truthOCR{script: out}
}

func (f *truthOCR) Name() string { return "truth" }

func (f *truthOCR) Recognize(ctx context.Context, img image.Image) (ocr.Result, error) {
	f.calls.Add(1)
	text, ok := f.script[vision.ImageHash(img)]
	if !ok {
		return ocr.Result{}, nil
	}
	return ocr.Result{Text: text}, nil
}

// ── tests ───────────────────────────────────────────────────────────────────

func startPayload(profileID, kind, dir, mode string, devTrace bool) string {
	dt := "false"
	if devTrace {
		dt = "true"
	}
	return `{"profile_id":"` + profileID + `","frame_source":{"kind":"` + kind + `","dir":"` + dir +
		`"},"mode":"` + mode + `","dev_trace":` + dt + `}`
}

// waitTerminal polls the session snapshot until it reaches a terminal state.
func (e *sessionEnv) waitTerminal(id string) map[string]any {
	e.t.Helper()
	by := time.Now().Add(30 * time.Second)
	for {
		got := e.mustOK(auth.RoleOperator, "/api/v1/sessions/"+id)
		if st := got["state"].(string); st == runtime.StateStopped || st == runtime.StateFailed {
			return got
		}
		if time.Now().After(by) {
			e.t.Fatalf("session never reached a terminal state; last %v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSessionStartValidation(t *testing.T) {
	e := newSessionEnv(t)
	clipDir, _ := filepath.Abs(sessSyntheticDir)
	// The allowlist test needs a directory that exists but is outside the
	// roots: existence is checked before the allowlist, so a missing path
	// would read as source-missing rather than source-not-allowed.
	absOutside := filepath.Join(t.TempDir(), "clip")
	if err := os.MkdirAll(absOutside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	role := auth.RoleOperator
	cases := []struct {
		name, payload string
		want          int
		code          string
	}{
		{"unknown profile", startPayload("01HAAAAAAAAAAAAAAAAAAAAAAA", "replay", clipDir, "conservative", false),
			http.StatusNotFound, "profile-not-found"},
		{"invalid mode", startPayload(e.profileID, "replay", clipDir, "turbo", false),
			http.StatusBadRequest, "invalid-mode"},
		{"unknown source kind", startPayload(e.profileID, "webrtc", clipDir, "conservative", false),
			http.StatusBadRequest, "unknown-source-kind"},
		{"v4l2 unsupported", startPayload(e.profileID, "v4l2", "", "conservative", false),
			http.StatusNotImplemented, "unsupported-capability"},
		{"dolphin unsupported", startPayload(e.profileID, "dolphin", "", "conservative", false),
			http.StatusNotImplemented, "unsupported-capability"},
		{"replay dir outside allowlist", startPayload(e.profileID, "replay", absOutside, "conservative", false),
			http.StatusBadRequest, "source-not-allowed"},
		{"replay dir missing", startPayload(e.profileID, "replay", filepath.Join(clipDir, "nope"), "conservative", false),
			http.StatusBadRequest, "source-missing"},
		{"unpublished profile refused", startPayload(seedSyntheticProfileForAPI(t, e.st, false), "replay", clipDir, "conservative", false),
			http.StatusConflict, "profile-not-published"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := e.send(role, http.MethodPost, "/api/v1/sessions", c.payload)
			assertSessionProblem(t, rec, c.want, c.code)
		})
	}

	// dev_trace is the developer toggle on an operator route: an operator
	// without developer gets a 403, not a session with silent frame storage.
	rec := e.send(role, http.MethodPost, "/api/v1/sessions",
		startPayload(e.profileID, "replay", clipDir, "conservative", true))
	assertSessionProblem(t, rec, http.StatusForbidden, "dev-trace-requires-developer")

	// The session surface itself requires a credential.
	rec = e.send(auth.RoleNone, http.MethodPost, "/api/v1/sessions",
		startPayload(e.profileID, "replay", clipDir, "conservative", false))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated start = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestSessionLifecycleAndViewerRead(t *testing.T) {
	e := newSessionEnv(t)
	clipDir, _ := filepath.Abs(sessSyntheticDir)

	rec := e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/sessions",
		startPayload(e.profileID, "replay", clipDir, "conservative", true))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	started := decodeJSON(t, rec)
	id := started["session_id"].(string)
	if started["event_cursor"].(float64) <= 0 {
		t.Fatalf("event_cursor = %v, want the session.started seq", started["event_cursor"])
	}

	// A viewer may read the session's state (the ACL split ADR-0024 records):
	// the snapshot is profile, mode and counters, not gameplay text.
	if got := e.mustOK(auth.RoleViewer, "/api/v1/sessions/"+id); got["session_id"] != id {
		t.Fatalf("viewer read session_id = %v, want %s", got["session_id"], id)
	}

	// The replay ends on its own; the terminal snapshot names its reason.
	fin := e.waitTerminal(id)
	if fin["state"] != runtime.StateStopped {
		t.Fatalf("terminal state = %q, want stopped (body %s)", fin["state"], fin)
	}
	if fin["reason"] != runtime.StopReasonSourceEnd {
		t.Fatalf("reason = %v, want source_end", fin["reason"])
	}

	// Stop is idempotent: the terminal session answers 200 with its own state.
	rec = e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/stop", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stop = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if stop := decodeJSON(t, rec); stop["state"] != runtime.StateStopped {
		t.Fatalf("stop response state = %v, want stopped", stop["state"])
	}

	// Unknown ids read 404, not 500.
	rec = e.send(auth.RoleViewer, http.MethodGet, "/api/v1/sessions/01HAAAAAAAAAAAAAAAAAAAAAAA", "")
	assertSessionProblem(t, rec, http.StatusNotFound, "session-not-found")
}

func TestDevInspectionRoutes(t *testing.T) {
	e := newSessionEnv(t)
	clipDir, _ := filepath.Abs(sessSyntheticDir)

	rec := e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/sessions",
		startPayload(e.profileID, "replay", clipDir, "conservative", true))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	id := decodeJSON(t, rec)["session_id"].(string)
	e.waitTerminal(id)

	// The developer page carries the session's events: lifecycle at the ends
	// and at least one text.observed carrying the dev-trace references.
	page := e.mustOK(auth.RoleDeveloper, "/api/v1/dev/events?session_id="+id+"&limit=1000")
	evs := page["events"].([]any)
	if len(evs) == 0 {
		t.Fatal("dev/events returned no events for the finished session")
	}
	var sawStarted, sawStopped, sawObserved bool
	var frameAsset, cropAsset, traceID string
	for _, raw := range evs {
		ev := raw.(map[string]any)
		switch ev["name"].(string) {
		case "session.started":
			sawStarted = true
		case "session.stopped":
			sawStopped = true
		case "text.observed":
			sawObserved = true
			if data, ok := ev["data"].(map[string]any); ok {
				if s, ok := data["frame_asset"].(string); ok && s != "" {
					frameAsset = s
					cropAsset, _ = data["crop_asset"].(string)
					traceID, _ = ev["trace_id"].(string)
				}
			}
		}
	}
	if !sawStarted || !sawStopped {
		t.Fatalf("dev/events missing lifecycle events: started=%v stopped=%v", sawStarted, sawStopped)
	}
	if !sawObserved {
		t.Fatal("dev/events has no text.observed for a replay that observed text")
	}
	if len(frameAsset) != 64 {
		t.Fatalf("frame_asset %q is not a sha256", frameAsset)
	}

	// The developer gate: a viewer never sees the raw event payloads.
	if rec := e.send(auth.RoleViewer, http.MethodGet, "/api/v1/dev/events?session_id="+id, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer dev/events = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	// The trace page groups one observation's events under its trace id.
	if traceID != "" {
		trace := e.mustOK(auth.RoleDeveloper, "/api/v1/dev/traces/"+traceID)
		tEvs := trace["events"].([]any)
		if len(tEvs) == 0 {
			t.Fatal("dev/traces returned an empty page for a live trace id")
		}
		for _, raw := range tEvs {
			if ev := raw.(map[string]any); ev["trace_id"].(string) != traceID {
				t.Fatalf("trace page carries an event with trace_id %v, want %s", ev["trace_id"], traceID)
			}
		}
	}

	// The asset route serves the stored JPEG with its content type, and an
	// unknown hash is a 404 problem, not bytes.
	rec = e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/dev/assets/"+frameAsset, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dev/assets = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("asset content type = %q, want image/jpeg", ct)
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), jpegMagic) {
		t.Fatal("asset body is not JPEG bytes: the inspector would show garbage")
	}
	if cropAsset != "" {
		rec = e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/dev/assets/"+cropAsset, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("crop asset = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Fatalf("crop content type = %q, want image/png", ct)
		}
	}
	rec = e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/dev/assets/"+strings.Repeat("0", 64), "")
	assertSessionProblem(t, rec, http.StatusNotFound, "asset-not-found")
}

func TestSessionsUnwiredIsA503(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st)) // no WithSessions, no WithDevEvents
	r := func(role auth.Role, method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader("{}"))
		req.Header.Set("Content-Type", ContentTypeJSON)
		if role != auth.RoleNone {
			req.Header.Set("Authorization", "Bearer "+a.bearer(role))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := r(auth.RoleOperator, http.MethodPost, "/api/v1/sessions"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired POST /sessions = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := r(auth.RoleDeveloper, http.MethodGet, "/api/v1/dev/events"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired dev/events = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}

// assertSessionProblem checks the RFC 9457 shape on one response and that the
// stable urn slug is the documented one: writeProblem carries the slug in the
// type member (the generic problem has no separate code member), so the type
// is the machine-readable identity a client branches on.
func assertSessionProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	requireContentType(t, rec, ContentTypeProblem)
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, wantStatus, rec.Body.String())
	}
	var doc struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if doc.Type != "urn:game-narrator:error:"+wantCode {
		t.Fatalf("problem.type = %q, want urn:game-narrator:error:%s", doc.Type, wantCode)
	}
}

var jpegMagic = []byte{0xff, 0xd8, 0xff}

var seedSlugSeq atomic.Int64
