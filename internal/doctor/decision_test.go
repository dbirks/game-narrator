package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
)

// The decision check probes System One for real (ADR-0021): these tests run
// the default probe against an httptest gateway so the adapter, the deadline
// and the redaction are all covered, plus a fake DecisionProbe for the
// answer-shape matrix.

const decisionModel = "clef-fake"

// validDecisionBody is a valid answer for decisionProbeRequest.
const validDecisionBody = `{"model":"` + decisionModel + `","answers":{"probe":{"type":"choice","choice":"ok","confidence":0.9,"probabilities":{"ok":0.9,"bad":0.1}}},"usage":{"input_tokens":5,"output_tokens":0}}`

// systemOneStub is a minimal /systemone gateway: one status, one body, an
// optional delay, and a record of what it saw.
type systemOneStub struct {
	status int
	body   string
	delay  time.Duration
	server *httptest.Server

	mu     sync.Mutex
	bodies []string
	auths  []string
	hits   int
}

func newSystemOneStub(t *testing.T, status int, body string) *systemOneStub {
	t.Helper()
	s := &systemOneStub{status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/systemone" {
			http.Error(w, `{"detail":"no such route"}`, http.StatusNotFound)
			return
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.hits++
		s.mu.Unlock()
		if s.delay > 0 {
			select {
			case <-time.After(s.delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		fmt.Fprint(w, s.body)
	}))
	t.Cleanup(srv.Close)
	s.server = srv
	return s
}

func (s *systemOneStub) url() string { return s.server.URL }

// TestDecisionRoleOKAgainstFakeSystemOne runs the default probe against a
// real HTTP /systemone stub and checks the OK row.
func TestDecisionRoleOKAgainstFakeSystemOne(t *testing.T) {
	const token = "super-secret-token-value"
	stub := newSystemOneStub(t, http.StatusOK, validDecisionBody)
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", stub.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", token)
	h.withEnv("GAME_NARRATOR_ROLE_DECISION_MODEL", decisionModel)

	c := h.check(h.run(), "providers.decision")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, decisionModel) {
		t.Fatalf("summary = %q, want the model id", c.Summary)
	}
	if !strings.Contains(c.Summary, "answered the decision probe in ") {
		t.Fatalf("summary = %q, want the measured latency", c.Summary)
	}
	if !strings.Contains(c.Detail, "probabilities are raw signals") {
		t.Fatalf("detail = %q, want the no-calibration note", c.Detail)
	}
	// The gateway must have seen a real bearer-authenticated request.
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.hits != 1 {
		t.Fatalf("the gateway saw %d requests, want exactly 1", stub.hits)
	}
	if stub.auths[0] != "Bearer "+token {
		t.Fatalf("Authorization = %q, want the bearer token", stub.auths[0])
	}
	// The wire body must be a System One request, not a chat request.
	var wire struct {
		Model        string         `json:"model"`
		State        map[string]any `json:"state"`
		Questions    map[string]any `json:"questions"`
		Instructions any            `json:"instructions"`
		Messages     any            `json:"messages"`
	}
	if err := json.Unmarshal([]byte(stub.bodies[0]), &wire); err != nil {
		t.Fatalf("wire body is not an object: %v", err)
	}
	if wire.Model != decisionModel {
		t.Fatalf("model = %q, want %q", wire.Model, decisionModel)
	}
	if len(wire.State) == 0 || len(wire.Questions) == 0 {
		t.Fatalf("state or questions missing from the wire body: %s", stub.bodies[0])
	}
	if wire.Instructions != nil || wire.Messages != nil {
		t.Fatalf("the probe sent a chat-shaped or instructions-bearing body: %s", stub.bodies[0])
	}
	// No URL or token may leak into the row.
	if strings.Contains(c.Summary+c.Detail, stub.url()) || strings.Contains(c.Summary+c.Detail, token) {
		t.Fatalf("check leaked a URL or token: %+v", c)
	}
}

// TestDecisionRoleFailsWithAuthKind: a 401 surfaces as FAIL with the auth
// kind, not as a generic error.
func TestDecisionRoleFailsWithAuthKind(t *testing.T) {
	stub := newSystemOneStub(t, http.StatusUnauthorized, `{"detail":"invalid api key"}`)
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", stub.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", "wrong")
	h.withEnv("GAME_NARRATOR_ROLE_DECISION_MODEL", decisionModel)

	c := h.check(h.run(), "providers.decision")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, string(providers.KindAuth)) {
		t.Fatalf("summary = %q, want the auth kind", c.Summary)
	}
}

// TestDecisionRoleFailsWithBadSchema: a 200 that does not answer the declared
// question is a schema failure.
func TestDecisionRoleFailsWithBadSchema(t *testing.T) {
	stub := newSystemOneStub(t, http.StatusOK, `{"model":"`+decisionModel+`","answers":{"not_asked":{"type":"noul","noul":0.5}}}`)
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", stub.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", "t")
	h.withEnv("GAME_NARRATOR_ROLE_DECISION_MODEL", decisionModel)

	c := h.check(h.run(), "providers.decision")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, string(providers.KindBadSchema)) {
		t.Fatalf("summary = %q, want the bad_schema kind", c.Summary)
	}
}

// TestDecisionRoleTimeoutBoundsTheProbe: a slow gateway must not hold the
// report past the check budget; the row says timeout.
func TestDecisionRoleTimeoutBoundsTheProbe(t *testing.T) {
	stub := newSystemOneStub(t, http.StatusOK, validDecisionBody)
	stub.delay = 400 * time.Millisecond
	h := newFakeHost(t)
	h.opts.ModelTimeout = 150 * time.Millisecond
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", stub.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", "t")
	h.withEnv("GAME_NARRATOR_ROLE_DECISION_MODEL", decisionModel)

	c := h.check(h.run(), "providers.decision")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, string(providers.KindTimeout)) {
		t.Fatalf("summary = %q, want a deadline-driven timeout", c.Summary)
	}
	if c.Duration > 2*time.Second {
		t.Fatalf("duration = %s, the decision budget did not bound the probe", c.Duration)
	}
}

// TestDecisionRoleNotConfiguredWithoutModel: no model means NOT CONFIGURED
// with a remedy naming the env var, and no request is sent.
func TestDecisionRoleNotConfiguredWithoutModel(t *testing.T) {
	h := newFakeHost(t)
	c := h.check(h.run(), "providers.decision")
	mustStatus(t, c, StatusNotConfigured)
	if !strings.Contains(c.Remedy, "GAME_NARRATOR_ROLE_DECISION_MODEL") {
		t.Fatalf("remedy = %q, want it to name the model env var", c.Remedy)
	}
}

// --- injected probe --------------------------------------------------------

type fakeDecisionProbe struct {
	decision systemone.Decision
	err      error
	seenReq  systemone.Request
}

func (p *fakeDecisionProbe) Decide(_ context.Context, _ providers.RoleConfig, req systemone.Request) (systemone.Decision, error) {
	p.seenReq = req
	return p.decision, p.err
}

// TestDecisionProbeInjection pins the seam: an injected probe's decision
// becomes the OK row and its error becomes the FAIL row.
func TestDecisionProbeInjection(t *testing.T) {
	h := newFakeHost(t)
	h.opts.DecisionProbe = &fakeDecisionProbe{decision: systemone.Decision{
		Model:   "injected-model",
		Answers: map[string]systemone.Answer{"probe": {Type: systemone.TypeChoice, Choice: "ok"}},
		Usage:   systemone.Usage{Input: 3, Output: 0},
		Latency: 42 * time.Millisecond,
	}}
	h.withEnv("GAME_NARRATOR_ROLE_DECISION_MODEL", decisionModel)
	c := h.check(h.run(), "providers.decision")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "injected-model") {
		t.Fatalf("summary = %q, want the injected model", c.Summary)
	}

	h2 := newFakeHost(t)
	h2.opts.DecisionProbe = &fakeDecisionProbe{err: providers.NewError(providers.KindUnavailable, 0, "endpoint unreachable")}
	h2.withEnv("GAME_NARRATOR_ROLE_DECISION_MODEL", decisionModel)
	c = h2.check(h2.run(), "providers.decision")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, string(providers.KindUnavailable)) {
		t.Fatalf("summary = %q, want the unavailable kind", c.Summary)
	}
}
