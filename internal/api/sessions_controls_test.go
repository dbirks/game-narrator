package api

// The player's live controls at the route (docs/spec/08 "…/pause, …/resume,
// …/settings", ADR-0075): pause/resume are idempotent operator writes that
// answer with the session's snapshot and one event each, the settings route
// applies the mid-session policy (partial body, normalized answer, one event
// per change, nothing for a no-op), and the documented refusals — 400
// out-of-vocabulary, 404 unknown session, 409 terminal session, 503 unwired
// runtime — carry the stable problem codes the client branches on.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
)

// controlPayload decodes one event's data into a map.
func controlPayload(t *testing.T, ev events.Envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("decode %s payload: %v", ev.Name, err)
	}
	return m
}

// countControlEvents counts the session events of the name in the log.
func countControlEvents(evs []events.Envelope, name string) int {
	n := 0
	for _, ev := range evs {
		if ev.Name == name {
			n++
		}
	}
	return n
}

// waitControlEvent polls the session's event log until the named event has
// appeared exactly want times: the control lands on the writer's chain, so
// the event is the async half of the answer (the snapshot is the sync one).
func waitControlEvent(t *testing.T, e *sessionEnv, id, name string, want int, deadline time.Duration) []events.Envelope {
	t.Helper()
	by := time.Now().Add(deadline)
	for {
		evs := e.sessionEvents(id)
		if n := countControlEvents(evs, name); n == want {
			return evs
		}
		if time.Now().After(by) {
			t.Fatalf("want %d %s event(s), have %d (have %d events)",
				want, name, countControlEvents(evs, name), len(evs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSessionControlRoutes walks the three spec-08 control routes against a
// real session. The session runs the test_pattern source (FFmpeg testsrc2
// through the same supervised subprocess live capture uses — synthetic
// frames through the production consumer, labelled as such per AGENTS.md rule
// 3): the replay recording ends after 24 frames, and the control calls would
// race the replay's EOF; the pattern ends only on stop, so every control in
// this test lands on a provably live session.
func TestSessionControlRoutes(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skipf("NOT RUN: ffmpeg is not installed (%v); the test_pattern source needs it", err)
	}
	e := newSessionEnv(t)

	rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions",
		startPayload(e.profileID, "test_pattern", "", "conservative", false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	id := decodeJSON(t, rec)["session_id"].(string)

	// The snapshot the operator's UI reads: not paused, the documented
	// default policy (conservative, dialogue alone).
	got := e.mustOK(auth.RoleViewer, "/api/v1/sessions/"+id)
	if p := got["paused"].(bool); p {
		t.Fatalf("fresh session reports paused = %v, want false", p)
	}
	if m := got["mode"].(string); m != "conservative" {
		t.Fatalf("fresh session mode = %q, want conservative", m)
	}
	if classes := stringsOf(t, got["narrated_classes"]); len(classes) != 1 || classes[0] != "dialogue" {
		t.Fatalf("fresh session narrated_classes = %v, want [dialogue]", classes)
	}

	// The pause: 200, the snapshot says paused, the event is on the chain
	// with the operator's reason.
	rec = e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/pause", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pause = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if p := decodeJSON(t, rec)["paused"].(bool); !p {
		t.Fatalf("pause answer paused = %v, want true (the button reads the server's state)", p)
	}
	evs := waitControlEvent(t, e, id, events.SessionPaused, 1, 10*time.Second)
	p := controlPayload(t, evs[len(evs)-1])
	if p["reason"] != "operator" {
		t.Fatalf("session.paused reason = %v, want operator", p["reason"])
	}

	// Idempotent: a second pause is the same snapshot and no second event.
	rec = e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/pause", "")
	if rec.Code != http.StatusOK || decodeJSON(t, rec)["paused"].(bool) != true {
		t.Fatalf("second pause = %d / %s, want the same paused snapshot", rec.Code, rec.Body.String())
	}
	if n := countControlEvents(e.sessionEvents(id), events.SessionPaused); n != 1 {
		t.Fatalf("session.paused events = %d, want 1 (a duplicate is not an event)", n)
	}

	// The resume: 200, the snapshot flips back, one event.
	rec = e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/resume", "")
	if rec.Code != http.StatusOK || decodeJSON(t, rec)["paused"].(bool) != false {
		t.Fatalf("resume = %d / %s, want 200 with paused=false", rec.Code, rec.Body.String())
	}
	waitControlEvent(t, e, id, events.SessionResumed, 1, 10*time.Second)
	rec = e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/resume", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second resume = %d, want 200 (idempotent)", rec.Code)
	}
	if n := countControlEvents(e.sessionEvents(id), events.SessionResumed); n != 1 {
		t.Fatalf("session.resumed events = %d, want 1", n)
	}

	// A settings body that names the state the session already has is a
	// no-op: 200, the same snapshot, and no event — a change that changed
	// nothing is not an event.
	rec = e.send(auth.RoleOperator, http.MethodPut, "/api/v1/sessions/"+id+"/settings", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op settings = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if n := countControlEvents(e.sessionEvents(id), events.SessionSettings); n != 0 {
		t.Fatalf("the no-op settings left %d events, want 0", n)
	}

	// A real change: predictive plus the items class. The answer is the
	// applied, normalized policy — dialogue first, always present — and the
	// event carries the same state.
	rec = e.send(auth.RoleOperator, http.MethodPut, "/api/v1/sessions/"+id+"/settings",
		`{"mode":"predictive","narrated_classes":["item","dialogue"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	after := decodeJSON(t, rec)
	if after["mode"] != "predictive" {
		t.Fatalf("applied mode = %v, want predictive", after["mode"])
	}
	if classes := stringsOf(t, after["narrated_classes"]); len(classes) != 2 || classes[0] != "dialogue" || classes[1] != "item" {
		t.Fatalf("applied classes = %v, want the normalized [dialogue item]", classes)
	}
	evs = waitControlEvent(t, e, id, events.SessionSettings, 1, 10*time.Second)
	p = controlPayload(t, evs[len(evs)-1])
	if p["mode"] != "predictive" {
		t.Fatalf("event mode = %v, want predictive", p["mode"])
	}
	if classes := stringsOf(t, p["narrated_classes"]); len(classes) != 2 || classes[0] != "dialogue" || classes[1] != "item" {
		t.Fatalf("event classes = %v, want the normalized [dialogue item]", classes)
	}

	// A partial body moves only its own field: the mode returns to
	// conservative and the classes the last change set survive.
	rec = e.send(auth.RoleOperator, http.MethodPut, "/api/v1/sessions/"+id+"/settings", `{"mode":"conservative"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial settings = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	after = decodeJSON(t, rec)
	if after["mode"] != "conservative" {
		t.Fatalf("mode = %v, want conservative", after["mode"])
	}
	if classes := stringsOf(t, after["narrated_classes"]); len(classes) != 2 || classes[1] != "item" {
		t.Fatalf("classes = %v, want the earlier [dialogue item] to survive the mode-only body", classes)
	}
	waitControlEvent(t, e, id, events.SessionSettings, 2, 10*time.Second)

	// Out-of-vocabulary values are refused with the stable code, and the
	// session's policy is unchanged by the refusal.
	rec = e.send(auth.RoleOperator, http.MethodPut, "/api/v1/sessions/"+id+"/settings", `{"mode":"turbo"}`)
	assertSessionProblem(t, rec, http.StatusBadRequest, "invalid-session-settings")
	rec = e.send(auth.RoleOperator, http.MethodPut, "/api/v1/sessions/"+id+"/settings",
		`{"narrated_classes":["graffiti"]}`)
	assertSessionProblem(t, rec, http.StatusBadRequest, "invalid-session-settings")
	rec = e.send(auth.RoleOperator, http.MethodPut, "/api/v1/sessions/"+id+"/settings", `not json`)
	assertSessionProblem(t, rec, http.StatusBadRequest, "invalid-session-settings")
	got = e.mustOK(auth.RoleOperator, "/api/v1/sessions/"+id)
	if got["mode"] != "conservative" {
		t.Fatalf("the refused requests moved the mode to %v", got["mode"])
	}

	// An unknown session id is a 404, not a 409: "no such session" and
	// "the session is over" are different answers to the operator.
	unknown := games.NewID()
	rec = e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+unknown+"/pause", "")
	assertSessionProblem(t, rec, http.StatusNotFound, "session-not-found")
	rec = e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+unknown+"/resume", "")
	assertSessionProblem(t, rec, http.StatusNotFound, "session-not-found")
	rec = e.send(auth.RoleOperator, http.MethodPut, "/api/v1/sessions/"+unknown+"/settings", `{}`)
	assertSessionProblem(t, rec, http.StatusNotFound, "session-not-found")

	// The terminal session refuses every control: the refusal is the
	// documented answer, not a dropped click.
	e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/stop", "")
	fin := e.waitTerminal(id)
	if fin["state"] == "running" || fin["state"] == "starting" {
		t.Fatalf("session never stopped: %v", fin["state"])
	}
	rec = e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/pause", "")
	assertSessionProblem(t, rec, http.StatusConflict, "session-not-running")
	rec = e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/resume", "")
	assertSessionProblem(t, rec, http.StatusConflict, "session-not-running")
	rec = e.send(auth.RoleOperator, http.MethodPut, "/api/v1/sessions/"+id+"/settings", `{}`)
	assertSessionProblem(t, rec, http.StatusConflict, "session-not-running")
}

// stringsOf reads a JSON string array out of a decoded snapshot or payload.
func stringsOf(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("not a string array: %v", v)
	}
	out := make([]string, 0, len(raw))
	for _, w := range raw {
		s, ok := w.(string)
		if !ok {
			t.Fatalf("array element is not a string: %v", w)
		}
		out = append(out, s)
	}
	return out
}

// TestSessionControlUnwiredIsA503 is the degraded answer for the control
// routes: a process without the session runtime (no WithSessions) says so
// with the stable 503 rather than pretending a control was applied.
func TestSessionControlUnwiredIsA503(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st)) // no WithSessions
	id := games.NewID()
	r := func(role auth.Role, method, target, body string) *httptest.ResponseRecorder {
		return doReq(t, h, a.as(role, jsonRequest(method, target, body)))
	}
	if rec := r(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/pause", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired pause = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := r(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/resume", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired resume = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := r(auth.RoleOperator, http.MethodPut, "/api/v1/sessions/"+id+"/settings", "{}"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired settings = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}
