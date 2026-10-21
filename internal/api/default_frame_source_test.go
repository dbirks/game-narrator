package api

// The API surface of the default frame source (ADR-0119, issue #28) and of
// the visible job-queue foreground pause (issue #27): POST /api/v1/sessions
// accepts a request that names no frame_source and answers the resolution's
// outcomes (the 201 whose session runs the ready device, the 409 that names
// the operator action, the 400 for the ambiguous shape), while the status
// snapshot and the job list carry the {paused_by, since, queued_count}
// object for as long as the pause holds.

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/devices"
	"github.com/dbirks/game-narrator/internal/capture/mode"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
)

// idleCaptureSource is the API test's capture source: it opens, delivers
// nothing (the test asserts on the start answer, not on frames) and closes
// with the session.
type idleCaptureSource struct{ ch chan capture.Frame }

func newIdleCaptureSource() *idleCaptureSource {
	return &idleCaptureSource{ch: make(chan capture.Frame, 1)}
}

func (s *idleCaptureSource) ID() string { return "v4l2:" + stack.FakePath }

func (s *idleCaptureSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return s.ch, nil
}

func (s *idleCaptureSource) Stats() capture.Stats { return capture.Stats{} }

func (s *idleCaptureSource) Close() error {
	select {
	case <-s.ch:
	default:
	}
	return nil
}

// apiDefaultResolve is the default-resolution half of the test enumeration:
// an empty request (the default's request) settles on the one ready device,
// the way the production stack's "persisted selection, then the first usable
// device" does.
func apiDefaultResolve() func(context.Context, string) (stack.Resolved, error) {
	return func(ctx context.Context, requestedID string) (stack.Resolved, error) {
		if requestedID != "" && requestedID != stack.FakeID {
			return stack.Resolved{}, stack.ErrDeviceUnknown
		}
		return stack.Resolved{
			Device: devices.Device{
				Path: stack.FakePath, Name: "Synthetic HDMI",
				StableID: stack.FakeID, Kind: "Video Capture",
			},
			Mode:       mode.Mode{W: 640, H: 480, FPS: 10, PixFmt: "mjpeg"},
			ModeSource: stack.ModeSourcePolicy,
			Reason:     "test wiring",
		}, nil
	}
}

// newDefaultSourceEnv is the session test environment with the capture wiring
// handed to the runtime the way main.go hands the production stack's
// closures: the manager owns the resolution, and the API only maps its
// answers.
func newDefaultSourceEnv(t *testing.T, resolve func(context.Context, string) (stack.Resolved, error)) *sessionEnv {
	t.Helper()
	a := newTestAuth(t)
	e := &sessionEnv{t: t, a: a, st: a.st}
	e.profileID = seedSyntheticProfileForAPI(t, a.st, true)
	clipDir, _ := filepath.Abs(sessSyntheticDir)

	mem := events.NewMemStore()
	e.mem = mem
	hub := events.New(events.WithStore(mem))
	src := newIdleCaptureSource()
	mgr, err := runtime.New(runtime.Options{
		Store:       a.st,
		Hub:         hub,
		AssetsRoot:  t.TempDir(),
		ReplayRoots: []string{clipDir},
		OCR:         newTruthOCR(t),
		Capture: &runtime.CaptureOptions{
			Resolve: resolve,
			Source:  func(ctx context.Context, res stack.Resolved) (capture.Source, error) { return src, nil },
		},
		Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = a.st.Close() })
	t.Cleanup(func() { _ = hub.Close() })
	t.Cleanup(mgr.Close)
	e.mgr = mgr
	e.h = a.handler(WithStore(a.st), WithEvents(hub), WithDevEvents(mem), WithSessions(mgr))
	return e
}

// TestStartSessionDefaultFrameSource covers the resolution's API outcomes:
// the ready device wins the default (201, the session's frame_source is the
// resolved kind and names the device), the ambiguous shape is the 400 that
// names the rule, and with no device the developer-mode fallback runs while
// the appliance gets the 409 that names the operator action.
func TestStartSessionDefaultFrameSource(t *testing.T) {
	e := newDefaultSourceEnv(t, apiDefaultResolve())

	t.Run("ready device wins the default", func(t *testing.T) {
		rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions",
			`{"profile_id":"`+e.profileID+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /sessions (no frame source, device ready) = %d, want 201 (body %s)", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		if body["frame_source"] != runtime.SourceKindV4L2 {
			t.Fatalf("session frame_source = %v, want %q (the resolved default)", body["frame_source"], runtime.SourceKindV4L2)
		}
		if body["capture_device"] != stack.FakeID {
			t.Fatalf("session capture_device = %v, want %q", body["capture_device"], stack.FakeID)
		}
		// Release the one-active-session invariant for the next subtest: the
		// source is idle, so the stop is immediate.
		if _, err := e.mgr.Stop(body["session_id"].(string)); err != nil {
			t.Fatalf("stop the default-source session: %v", err)
		}
	})

	t.Run("a dir or clip id without a kind is the 400 that names the rule", func(t *testing.T) {
		rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions",
			`{"profile_id":"`+e.profileID+`","frame_source":{"dir":"some/dir"}}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST /sessions (dir without kind) = %d, want 400 (body %s)", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		if body["type"] != "urn:game-narrator:error:incomplete-frame-source" {
			t.Fatalf("problem type = %v, want the incomplete-frame-source type", body["type"])
		}
	})
}

// TestStartSessionDefaultFrameSourceAbsent is the card-less box: the default
// resolves the developer-mode test pattern when GAME_NARRATOR_DEV is set,
// and the 409 NO_FRAME_SOURCE that names the operator action when it is not
// — a silent synthetic session is the failure the issue describes.
func TestStartSessionDefaultFrameSourceAbsent(t *testing.T) {
	e := newDefaultSourceEnv(t, func(ctx context.Context, requestedID string) (stack.Resolved, error) {
		return stack.Resolved{}, stack.ErrDeviceAbsent
	})

	t.Run("the appliance answers the 409 that names the action", func(t *testing.T) {
		rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions",
			`{"profile_id":"`+e.profileID+`"}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("POST /sessions (no device, no developer mode) = %d, want 409 (body %s)", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		if body["type"] != "urn:game-narrator:error:no-frame-source" {
			t.Fatalf("problem type = %v, want the no-frame-source type", body["type"])
		}
		if body["code"] != "NO_FRAME_SOURCE" {
			t.Fatalf("problem code = %v, want NO_FRAME_SOURCE", body["code"])
		}
		if !strings.Contains(body["detail"].(string), "connect your capture card") {
			t.Fatalf("problem detail = %v, want the operator action named", body["detail"])
		}
	})

	t.Run("developer mode takes the synthetic fallback", func(t *testing.T) {
		t.Setenv(runtime.EnvDeveloperMode, "1")
		rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions",
			`{"profile_id":"`+e.profileID+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /sessions (no device, developer mode) = %d, want 201 (body %s)", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		if body["frame_source"] != runtime.SourceKindTestPattern {
			t.Fatalf("session frame_source = %v, want %q (the developer-mode fallback)", body["frame_source"], runtime.SourceKindTestPattern)
		}
	})
}

// fakeJobPause is the WithJobRunner seam's test fake: the API's view of the
// runner's foreground pause without a runner (the seam exists so the API
// reads a shape, not a worker pool).
type fakeJobPause struct{ p jobs.ForegroundPause }

func (f fakeJobPause) ForegroundPause(ctx context.Context) jobs.ForegroundPause { return f.p }

// pauseSince is the fixture's fixed pause start (RFC 3339, UTC): the tests
// assert the API echoes the runner's stamp byte for byte, so a fixed value
// is what makes the echo visible.
func pauseSince() (time.Time, string) {
	ts := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	return ts, ts.Format(time.RFC3339)
}

// TestStatusAndJobsSurfacesForegroundPause is issue #27's acceptance at the
// API: the pause object {paused_by, since, queued_count} is on the status
// snapshot (and in the bar's string) and on the job list while it holds, and
// absent when the box is not pausing anything.
func TestStatusAndJobsSurfacesForegroundPause(t *testing.T) {
	js := store.NewJobsStore(openTemplateStore(t))
	sinceT, since := pauseSince()

	t.Run("paused: the object rides both surfaces", func(t *testing.T) {
		h := newHandler(t, WithJobs(js), WithJobRunner(fakeJobPause{p: jobs.ForegroundPause{
			Paused: true, Since: sinceT, Queued: 3,
		}}))
		rec := do(t, h, http.MethodGet, "/api/v1/status")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		status := decodeJSON(t, rec)
		jobsObj, ok := status["jobs"].(map[string]any)
		if !ok {
			t.Fatalf("status snapshot has no jobs object while the pause is active: %s", rec.Body.String())
		}
		if jobsObj["paused_by"] != jobPauseByLiveSession || jobsObj["queued_count"] != float64(3) || jobsObj["since"] != since {
			t.Fatalf("status jobs = %v, want {live_session, %s, 3}", jobsObj, since)
		}
		if !strings.Contains(status["status"].(string), "jobs paused") {
			t.Fatalf("status string = %q, want the jobs-paused marker", status["status"])
		}

		jrec := do(t, h, http.MethodGet, "/api/v1/jobs")
		if jrec.Code != http.StatusOK {
			t.Fatalf("GET /jobs = %d, want 200 (body %s)", jrec.Code, jrec.Body.String())
		}
		list := decodeJSON(t, jrec)
		listObj, ok := list["jobs_paused"].(map[string]any)
		if !ok {
			t.Fatalf("job list has no jobs_paused object while the pause is active: %s", jrec.Body.String())
		}
		if listObj["paused_by"] != jobPauseByLiveSession || listObj["queued_count"] != float64(3) || listObj["since"] != since {
			t.Fatalf("jobs list jobs_paused = %v, want the same {live_session, %s, 3} object", listObj, since)
		}
	})

	t.Run("idle: the object is absent, never zeroed", func(t *testing.T) {
		h := newHandler(t, WithJobs(js), WithJobRunner(fakeJobPause{}))
		rec := do(t, h, http.MethodGet, "/api/v1/status")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		status := decodeJSON(t, rec)
		if _, present := status["jobs"]; present {
			t.Fatalf("idle status snapshot carries a jobs object: %s", status)
		}
		if strings.Contains(status["status"].(string), "jobs paused") {
			t.Fatalf("idle status string = %q, want no jobs-paused marker", status["status"])
		}
		jrec := do(t, h, http.MethodGet, "/api/v1/jobs")
		if jrec.Code != http.StatusOK {
			t.Fatalf("GET /jobs = %d, want 200 (body %s)", jrec.Code, jrec.Body.String())
		}
		list := decodeJSON(t, jrec)
		if _, present := list["jobs_paused"]; present {
			t.Fatalf("idle job list carries a jobs_paused object: %s", list)
		}
	})
}
