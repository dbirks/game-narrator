package api

// The developer-only Dolphin bridge routes (docs/spec/11, dev opt-in). The
// bridge is faked at the interface seam: these tests assert the HTTP
// contract — the opt-in gate, the unwired gate, the role, the request
// validation, the response shapes (validated against the OpenAPI document
// like every other 2xx in this package) and the screenshot's media type —
// not the emulator, which the internal/dolphin tests own.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/dolphin"
)

// fakeDolphinBridge is the scripted Bridge for the route tests.
type fakeDolphinBridge struct {
	mu       sync.Mutex
	state    dolphin.State
	lastErr  string
	launched int
	stopped  int
	injected []string
}

func newFakeDolphinBridge() *fakeDolphinBridge {
	return &fakeDolphinBridge{state: dolphin.StateStopped}
}

func (f *fakeDolphinBridge) setState(s dolphin.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

func (f *fakeDolphinBridge) Launch(ctx context.Context, spec dolphin.LaunchSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == dolphin.StateRunning || f.state == dolphin.StateLaunching {
		return dolphin.ErrAlreadyLaunched
	}
	f.launched++
	f.state = dolphin.StateRunning
	return nil
}

func (f *fakeDolphinBridge) Inject(ctx context.Context, buttons dolphin.Buttons, axes dolphin.Axes, hold time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != dolphin.StateRunning {
		return dolphin.ErrNotRunning
	}
	var out []string
	if buttons.Start {
		out = append(out, "start")
	}
	if buttons.A {
		out = append(out, "a")
	}
	f.injected = append(f.injected, out...)
	return nil
}

func (f *fakeDolphinBridge) Screenshot(ctx context.Context) (image.Image, error) {
	if f.state != dolphin.StateRunning {
		return nil, dolphin.ErrNotRunning
	}
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 200, G: 10, B: 10, A: 0xff})
	return img, nil
}

var errDolphinFrameSourceNotInRouteTests = errors.New("dolphin route tests do not build a frame source")

func (f *fakeDolphinBridge) FrameSource(fps int) (capture.Source, error) {
	return nil, errDolphinFrameSourceNotInRouteTests
}

func (f *fakeDolphinBridge) SaveSnapshot(label string) (string, error) {
	return "snapshots/" + label + ".png", nil
}

func (f *fakeDolphinBridge) WaitForGameFrame(ctx context.Context, timeout time.Duration) (dolphin.Evidence, error) {
	return dolphin.Evidence{}, nil
}

func (f *fakeDolphinBridge) ProveInput(ctx context.Context) (dolphin.Evidence, error) {
	return dolphin.Evidence{}, nil
}

func (f *fakeDolphinBridge) SaveState(ctx context.Context, slot int) (string, error) {
	return "savestates/fake-slot-" + fmt.Sprint(slot) + ".png", nil
}

func (f *fakeDolphinBridge) LoadState(ctx context.Context, slot int) error {
	return nil
}

func (f *fakeDolphinBridge) State() dolphin.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return dolphin.Status{
		State:     f.state,
		PipeConn:  f.state == dolphin.StateRunning,
		ImageSHA:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageSize: 1 << 30,
		LastError: f.lastErr,
	}
}

func (f *fakeDolphinBridge) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == dolphin.StateStopped {
		return
	}
	f.stopped++
	f.state = dolphin.StateStopped
}

// dolphinEnv is the route-test harness: an auth service, the handler with
// the bridge wired (or not), and a send helper that validates every 2xx
// against the OpenAPI document like the other suites.
type dolphinEnv struct {
	t *testing.T
	a *testAuth
	h http.Handler
}

func newDolphinEnv(t *testing.T, optIn string, bridge dolphin.Bridge) (e *dolphinEnv) {
	t.Helper()
	t.Setenv(dolphin.EnvDevDolphin, optIn)
	e = &dolphinEnv{t: t}
	e.a = newTestAuth(t)
	if bridge != nil {
		e.h = e.a.handler(WithDolphinBridge(bridge))
	} else {
		e.h = e.a.handler()
	}
	return e
}

// send performs one request as a role and contract-validates any 2xx body.
func (e *dolphinEnv) send(role auth.Role, method, target, payload string) *httptest.ResponseRecorder {
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

// decodeBody parses a JSON response body (2xx only; the send helper already
// contract-validated the shape).
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	return decodeJSON(t, rec)
}

var dolphinPaths = []struct {
	method, path string
}{
	{http.MethodGet, "/api/v1/dev/dolphin"},
	{http.MethodPost, "/api/v1/dev/dolphin/launch"},
	{http.MethodPost, "/api/v1/dev/dolphin/input"},
	{http.MethodPost, "/api/v1/dev/dolphin/screenshot"},
	{http.MethodPost, "/api/v1/dev/dolphin/stop"},
}

// TestDolphinRoutesWithoutOptIn: without GAME_NARRATOR_DEV_DOLPHIN the routes
// answer 501 no matter how a developer-role caller phrases the request
// (docs/spec/01 invariant 7: production never executes controller actions).
func TestDolphinRoutesWithoutOptIn(t *testing.T) {
	e := newDolphinEnv(t, "0", newFakeDolphinBridge())
	for _, p := range dolphinPaths {
		rec := e.send(auth.RoleDeveloper, p.method, p.path, "")
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s without opt-in = %d, want 501 (body %s)", p.method, p.path, rec.Code, rec.Body.String())
		}
	}
}

// TestDolphinRoutesUnwired: with the opt-in but no bridge wired into the
// process the routes answer 503 — the route existing is a fact, the emulator
// being reachable is not.
func TestDolphinRoutesUnwired(t *testing.T) {
	e := newDolphinEnv(t, "1", nil)
	for _, p := range dolphinPaths {
		rec := e.send(auth.RoleDeveloper, p.method, p.path, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s unwired = %d, want 503 (body %s)", p.method, p.path, rec.Code, rec.Body.String())
		}
	}
}

// TestDolphinRoutesFull: the happy path and every branchable failure.
func TestDolphinRoutesFull(t *testing.T) {
	fake := newFakeDolphinBridge()
	e := newDolphinEnv(t, "1", fake)

	// Role gate: the /api/v1/dev prefix is developer-only.
	rec := e.send(auth.RoleViewer, http.MethodGet, "/api/v1/dev/dolphin", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer GET status = %d, want 403", rec.Code)
	}

	// Status: 200 with the self-report (contract-validated by send).
	rec = e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/dev/dolphin", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["state"] != "stopped" {
		t.Fatalf("state = %v, want stopped", body["state"])
	}

	// Input before launch: the condition is branchable (409), not an error.
	rec = e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/dolphin/input", `{"buttons":{"start":true}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("input while stopped = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}

	// Launch validation: negative width is a 400.
	rec = e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/dolphin/launch", `{"width":-1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("launch with negative width = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}

	// Launch: 202 (the launch is async; the body is the status at dispatch).
	rec = e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/dolphin/launch", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("launch = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	// The fake launch is fast; wait for the goroutine to have landed it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fake.State().State != dolphin.StateRunning {
		time.Sleep(5 * time.Millisecond)
	}
	if fake.launchedCount() != 1 {
		t.Fatalf("bridge launched %d times, want 1", fake.launchedCount())
	}

	// A second launch while running is a 409, not a second emulator.
	rec = e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/dolphin/launch", `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second launch while running = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}

	// Closed vocabulary (while running): an unknown button is a 400, not a
	// silent drop; an out-of-range axis is a 400 (a clamped value would hide
	// the operator's typo).
	rec = e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/dolphin/input", `{"buttons":{"star":true}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("input with unknown button = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	rec = e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/dolphin/input", `{"axes":{"main_x":2.5}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("input with out-of-range axis = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}

	// Input: the closed set, a real hold, accepted (200, status body).
	rec = e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/dolphin/input",
		`{"buttons":{"start":true,"a":true},"hold_ms":25}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("input while running = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got := fake.injectedNames()
	if len(got) != 2 || !contains(got, "start") || !contains(got, "a") {
		t.Fatalf("injected buttons = %v, want start and a", got)
	}

	// Screenshot: a fresh PNG with the media type, decodable, not stored.
	rec = e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/dolphin/screenshot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("screenshot = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("screenshot Content-Type = %q, want image/png", ct)
	}
	if _, err := png.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil {
		t.Fatalf("screenshot body is not a decodable PNG: %v", err)
	}

	// Stop: 200 (state stopped), and idempotent (a second 200, one kill).
	rec = e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/dolphin/stop", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stop = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body = decodeBody(t, rec)
	if body["state"] != "stopped" {
		t.Fatalf("state after stop = %v, want stopped", body["state"])
	}
	rec = e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/dolphin/stop", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second stop = %d, want 200 (idempotent)", rec.Code)
	}
	if fake.stoppedCount() != 1 {
		t.Fatalf("bridge stopped %d times, want 1 (idempotent Stop)", fake.stoppedCount())
	}
}

// launchedCount / stoppedCount / injectedNames are the test-side readers.
func (f *fakeDolphinBridge) launchedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launched
}

func (f *fakeDolphinBridge) stoppedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func (f *fakeDolphinBridge) injectedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.injected...)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
