package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// HTTP-level coverage: the error taxonomy mapping (mirroring the openai
// adapter's rules), deadline and cancellation, and the doubt limiter. The
// gateway is always an httptest fake — these tests prove the client, not the
// model.

const testModel = "clef-fake"

// fakeGateway serves one canned body with one status, records the requests it
// saw, and can delay before answering.
type fakeGateway struct {
	server *httptest.Server
	status int
	body   string
	delay  time.Duration

	mu     sync.Mutex
	bodies []string
	auths  []string
	hits   int
}

// newFakeGateway builds a /systemone stub.
func newFakeGateway(t *testing.T, status int, body string) *fakeGateway {
	t.Helper()
	g := &fakeGateway{status: status, body: body}
	g.server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGateway) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path != "/systemone" {
		http.Error(w, "no such route", http.StatusNotFound)
		return
	}
	g.mu.Lock()
	g.bodies = append(g.bodies, string(body))
	g.auths = append(g.auths, r.Header.Get("Authorization"))
	g.hits++
	g.mu.Unlock()
	if g.delay > 0 {
		select {
		case <-time.After(g.delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", "req-fake-1")
	w.WriteHeader(g.status)
	fmt.Fprint(w, g.body)
}

func (g *fakeGateway) url() string { return g.server.URL }

func (g *fakeGateway) lastBody(t *testing.T) string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) == 0 {
		t.Fatal("the gateway saw no request")
	}
	return g.bodies[len(g.bodies)-1]
}

func (g *fakeGateway) hitsCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

func newTestClient(t *testing.T, g *fakeGateway) *Client {
	t.Helper()
	c, err := New(g.url(), "test-token-must-not-leak", testModel)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func smallRequest() Request {
	return Request{
		State: map[string]any{"ocr": "hello"},
		Questions: []Question{{
			ID:           "probe",
			Type:         TypeChoice,
			Instructions: "pick one",
			Criteria:     []Option{{ID: "ok", Description: "yes"}, {ID: "bad", Description: "no"}},
		}},
	}
}

// validBody renders a valid answer for smallRequest against model m.
func validBody(m string) string {
	return `{"model":"` + m + `","answers":{"probe":{"type":"choice","choice":"ok","confidence":0.9,"probabilities":{"ok":0.9,"bad":0.1}}},"usage":{"input_tokens":7,"output_tokens":0}}`
}

// TestHTTPStatusMapping pins the taxonomy mapping for every status class.
func TestHTTPStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   providers.Kind
		msg    string
	}{
		{http.StatusUnauthorized, `{"detail":"invalid api key"}`, providers.KindAuth, "invalid api key"},
		{http.StatusForbidden, `{"detail":"forbidden"}`, providers.KindAuth, "forbidden"},
		{http.StatusNotFound, `{"detail":"model not found"}`, providers.KindUnsupportedCapability, "model not found"},
		{http.StatusRequestEntityTooLarge, `{"detail":"too large"}`, providers.KindPayloadTooLarge, "too large"},
		{http.StatusTooManyRequests, `{"detail":"slow down"}`, providers.KindRateLimited, "slow down"},
		{http.StatusUnprocessableEntity, `{"detail":"model and state are required"}`, providers.KindBadSchema, "model and state are required"},
		{http.StatusBadRequest, `{"detail":"bad"}`, providers.KindBadSchema, "bad"},
		{http.StatusBadGateway, `{"detail":"upstream"}`, providers.KindUnavailable, "upstream"},
		{http.StatusServiceUnavailable, ``, providers.KindUnavailable, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("http_%d", tc.status), func(t *testing.T) {
			g := newFakeGateway(t, tc.status, tc.body)
			c := newTestClient(t, g)
			_, err := c.Decide(context.Background(), smallRequest())
			var pe *providers.Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v (%T), want *providers.Error", err, err)
			}
			if pe.Kind != tc.kind {
				t.Fatalf("Kind = %q, want %q", pe.Kind, tc.kind)
			}
			if pe.Status != tc.status {
				t.Fatalf("Status = %d, want %d", pe.Status, tc.status)
			}
			if tc.msg != "" && !strings.Contains(pe.Msg, tc.msg) {
				t.Fatalf("Msg = %q, want it to contain the server detail %q", pe.Msg, tc.msg)
			}
		})
	}
}

// TestDeadlineExceededIsTimeout proves the client honours the context deadline
// and classifies it as timeout (the runtime's 1500 ms Clef budget relies on
// this: on timeout the runtime proceeds without Clef, docs/spec/06).
func TestDeadlineExceededIsTimeout(t *testing.T) {
	g := newFakeGateway(t, http.StatusOK, validBody(testModel))
	g.delay = 2 * time.Second
	c := newTestClient(t, g)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Decide(ctx, smallRequest())
	elapsed := time.Since(start)
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *providers.Error", err, err)
	}
	if pe.Kind != providers.KindTimeout {
		t.Fatalf("Kind = %q, want timeout", pe.Kind)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the context error must stay reachable through errors.Is")
	}
	if !pe.Retryable {
		t.Fatal("a timeout is transient and must be marked retryable")
	}
	if elapsed > time.Second {
		t.Fatalf("Decide took %s; the deadline was not honoured", elapsed)
	}
}

// TestCallerCancellationIsCancelled distinguishes a caller cancel from an
// expired deadline.
func TestCallerCancellationIsCancelled(t *testing.T) {
	g := newFakeGateway(t, http.StatusOK, validBody(testModel))
	g.delay = 2 * time.Second
	c := newTestClient(t, g)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := c.Decide(ctx, smallRequest())
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *providers.Error", err, err)
	}
	if pe.Kind != providers.KindCancelled {
		t.Fatalf("Kind = %q, want cancelled", pe.Kind)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("the context error must stay reachable through errors.Is")
	}
}

// TestUnreachableEndpointIsUnavailable covers the no-server case.
func TestUnreachableEndpointIsUnavailable(t *testing.T) {
	g := newFakeGateway(t, http.StatusOK, validBody(testModel))
	addr := g.url()
	g.server.Close()
	c, err := New(addr, "", testModel)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Decide(context.Background(), smallRequest())
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *providers.Error", err, err)
	}
	if pe.Kind != providers.KindUnavailable {
		t.Fatalf("Kind = %q, want unavailable", pe.Kind)
	}
	if strings.Contains(pe.Msg, "127.0.0.1") || strings.Contains(pe.Msg, "192.168") {
		t.Fatalf("the error leaks an address: %q", pe.Msg)
	}
}

// TestNonJSONResponseIsBadSchema guards against a gateway that answers with
// HTML or prose.
func TestNonJSONResponseIsBadSchema(t *testing.T) {
	g := newFakeGateway(t, http.StatusOK, "<html>502</html>")
	c := newTestClient(t, g)
	_, err := c.Decide(context.Background(), smallRequest())
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *providers.Error", err, err)
	}
	if pe.Kind != providers.KindBadSchema {
		t.Fatalf("Kind = %q, want bad_schema", pe.Kind)
	}
}

// TestRequestValidationSendsNothing proves client-side validation fails before
// the network: a malformed request must not consume the deadline.
func TestRequestValidationSendsNothing(t *testing.T) {
	g := newFakeGateway(t, http.StatusOK, validBody(testModel))
	c := newTestClient(t, g)
	_, err := c.Decide(context.Background(), Request{State: map[string]any{"ocr": "x"}})
	if err == nil {
		t.Fatal("a request without questions was accepted")
	}
	var pe *providers.Error
	if !errors.As(err, &pe) || pe.Kind != providers.KindBadSchema {
		t.Fatalf("err = %v, want a local bad_schema", err)
	}
	if g.hitsCount() != 0 {
		t.Fatalf("the gateway saw %d requests; validation must fail before the network", g.hitsCount())
	}
}

// TestBearerTokenAndWireShape checks what the gateway actually receives.
func TestBearerTokenAndWireShape(t *testing.T) {
	g := newFakeGateway(t, http.StatusOK, validBody(testModel))
	c := newTestClient(t, g)
	d, err := c.Decide(context.Background(), smallRequest())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	g.mu.Lock()
	lastAuth := g.auths[len(g.auths)-1]
	g.mu.Unlock()
	if lastAuth != "Bearer test-token-must-not-leak" {
		t.Fatalf("Authorization = %q, want the bearer token", lastAuth)
	}
	body := g.lastBody(t)
	var wire struct {
		Model        string         `json:"model"`
		State        map[string]any `json:"state"`
		Questions    map[string]any `json:"questions"`
		Instructions any            `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatalf("wire body is not an object: %v", err)
	}
	if wire.Model != testModel {
		t.Fatalf("model = %q, want %q (the client's model fills an empty Request.Model)", wire.Model, testModel)
	}
	if wire.Instructions != nil {
		t.Fatalf("top-level instructions leaked onto the wire: %v", wire.Instructions)
	}
	if d.Origin != "live" {
		t.Fatalf("Origin = %q, want live", d.Origin)
	}
	if d.RequestID != "req-fake-1" {
		t.Fatalf("RequestID = %q, want the gateway's correlation handle", d.RequestID)
	}
	if d.Model != testModel {
		t.Fatalf("Model = %q, want %q", d.Model, testModel)
	}
	if d.Answers["probe"].Choice != "ok" || d.Answers["probe"].Confidence != 0.9 {
		t.Fatalf("answers not normalised: %+v", d.Answers["probe"])
	}
	if d.Usage.Input != 7 || d.Usage.Output != 0 {
		t.Fatalf("usage not carried: %+v", d.Usage)
	}
	if d.Latency <= 0 {
		t.Fatal("latency was not measured")
	}
	// The token must not leak into any client-facing rendering.
	for _, s := range []string{c.String(), fmt.Sprint(c.LogValue())} {
		if strings.Contains(s, "test-token-must-not-leak") {
			t.Fatalf("client rendering leaked the token: %s", s)
		}
	}
}

// TestNewFromRoleRejectsWrongProtocolAndUnconfigured pins the role seam.
func TestNewFromRoleRejectsWrongProtocolAndUnconfigured(t *testing.T) {
	unconfigured := providers.RoleConfig{Role: providers.RoleDecision, Protocol: providers.ProtocolSystemOne, Timeout: time.Second}
	if _, err := NewFromRole(unconfigured); err == nil {
		t.Fatal("an unconfigured role must not build a client")
	}
	wrong := providers.RoleConfig{
		Role: providers.RoleDecision, Protocol: providers.ProtocolOpenAIChat,
		BaseURL: "http://gateway.invalid", Model: "x", Timeout: time.Second,
	}
	_, err := NewFromRole(wrong)
	var pe *providers.Error
	if !errors.As(err, &pe) || pe.Kind != providers.KindUnsupportedCapability {
		t.Fatalf("err = %v, want unsupported_capability for a non-systemone protocol", err)
	}
	ok := providers.RoleConfig{
		Role: providers.RoleDecision, Protocol: providers.ProtocolSystemOne,
		BaseURL: "http://gateway.invalid", Model: "clef-omni", Timeout: 1500 * time.Millisecond,
	}
	c, err := NewFromRole(ok)
	if err != nil {
		t.Fatalf("NewFromRole: %v", err)
	}
	if c.HTTP == nil || c.HTTP.Timeout != 1500*time.Millisecond {
		t.Fatalf("the role's timeout budget was not applied to the transport: %+v", c.HTTP)
	}
}

// --- doubt limiter ----------------------------------------------------------

// TestDoubtLimitRefusesAfterConsecutiveDoubts pins the rate-limiting rule:
// after `limit` consecutive doubtful decisions per key, Decide refuses
// locally with rate_limited before sending another request.
func TestDoubtLimitRefusesAfterConsecutiveDoubts(t *testing.T) {
	// Every answer picks "bad"; the test's classifier counts that as doubtful.
	g := newFakeGateway(t, http.StatusOK, `{"model":"`+testModel+`","answers":{"probe":{"type":"choice","choice":"bad","confidence":0.1,"probabilities":{"ok":0.9,"bad":0.1}}},"usage":{"input_tokens":1,"output_tokens":0}}`)
	c, err := New(g.url(), "", testModel, WithDoubtLimit(2, func(d Decision) bool {
		return d.Answers["probe"].Choice == "bad"
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := smallRequest()
	req.Key = "session-1"
	for i := 1; i <= 2; i++ {
		if _, err := c.Decide(context.Background(), req); err != nil {
			t.Fatalf("call %d within the budget: %v", i, err)
		}
	}
	_, err = c.Decide(context.Background(), req)
	if err == nil {
		t.Fatal("the third consecutive doubtful decision was sent; the budget must refuse locally")
	}
	var pe *providers.Error
	if !errors.As(err, &pe) || pe.Kind != providers.KindRateLimited {
		t.Fatalf("err = %v, want rate_limited", err)
	}
	if pe.Retryable {
		t.Fatal("a budget refusal must not be marked retryable")
	}
	if got := g.hitsCount(); got != 2 {
		t.Fatalf("gateway saw %d requests, want exactly 2 (the third must be refused locally)", got)
	}
}

// TestDoubtLimitResetsOnConfidentAnswer proves a confident answer refills the
// bucket, and that keys are independent.
func TestDoubtLimitResetsOnConfidentAnswer(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Call 3 is confident (refills), the rest are doubtful.
		choice := "bad"
		if n == 2 {
			choice = "ok"
		}
		fmt.Fprint(w, `{"model":"`+testModel+`","answers":{"probe":{"type":"choice","choice":"`+choice+`","confidence":0.5,"probabilities":{"ok":0.5,"bad":0.5}}},"usage":{}}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "", testModel, WithDoubtLimit(2, func(d Decision) bool {
		return d.Answers["probe"].Choice == "bad"
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := smallRequest()
	req.Key = "a"
	// 1 spends the budget; 2 is confident and refills; 3,4 spend again; 5 is
	// refused before it reaches the wire.
	for i := 1; i <= 4; i++ {
		if _, err := c.Decide(context.Background(), req); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := c.Decide(context.Background(), req); err == nil {
		t.Fatal("call 5 was sent although the budget was exhausted")
	}
	// A fresh key has its own full budget: two more go out, the third refuses.
	other := smallRequest()
	other.Key = "b"
	for i := 1; i <= 2; i++ {
		if _, err := c.Decide(context.Background(), other); err != nil {
			t.Fatalf("call %d on key b: %v", i, err)
		}
	}
	if _, err := c.Decide(context.Background(), other); err == nil {
		t.Fatal("key b exceeded its own budget without refusal")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 6 {
		t.Fatalf("the gateway served %d requests, want 6 (4 for a, 2 for b)", calls)
	}
}

// TestDoubtLimitDisabledByDefault: without the option, nothing is limited.
func TestDoubtLimitDisabledByDefault(t *testing.T) {
	g := newFakeGateway(t, http.StatusOK, `{"model":"`+testModel+`","answers":{"probe":{"type":"choice","choice":"bad","confidence":0.1,"probabilities":{"ok":0.9,"bad":0.1}}},"usage":{}}`)
	c := newTestClient(t, g)
	req := smallRequest()
	for i := 0; i < 5; i++ {
		if _, err := c.Decide(context.Background(), req); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if got := g.hitsCount(); got != 5 {
		t.Fatalf("gateway saw %d requests, want 5 (no budget configured)", got)
	}
}

// TestDefaultUncertain pins the default classifier's shape.
func TestDefaultUncertain(t *testing.T) {
	mk := func(choice string, noul float64) Decision {
		d := Decision{Answers: map[string]Answer{}}
		if choice != "" {
			d.Answers["q"] = Answer{Type: TypeChoice, Choice: choice}
		}
		if noul >= 0 {
			d.Answers["n"] = Answer{Type: TypeNoul, Noul: noul}
		}
		return d
	}
	if !DefaultUncertain(mk("none", 0.9)) {
		t.Error("choice none must be doubtful")
	}
	if !DefaultUncertain(mk("unknown", 0.9)) {
		t.Error("choice unknown must be doubtful")
	}
	if !DefaultUncertain(mk("", 0.2)) {
		t.Error("noul below 0.5 must be doubtful")
	}
	if DefaultUncertain(mk("dialogue", 0.9)) {
		t.Error("a confident, non-fallback answer must not be doubtful")
	}
	if DefaultUncertain(Decision{}) {
		t.Error("an empty decision must not be doubtful")
	}
}
