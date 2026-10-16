package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/recorded"
)

// The adapter is transport-agnostic by construction: it holds an *http.Client
// and owns no dialling policy, so the same Chat call can be served by a live
// gateway or by a cassette (internal/providers/recorded, ADR-0013) with no
// branch inside this package. These tests pin that contract, because the value
// of recorded providers as *evidence* (docs/spec/12 "Contract" mode,
// docs/spec/11 Mode B) depends on the replayed path being the same code path —
// not a specially-faked one.
//
// The cassette under test is the offline example committed in
// ../recorded/testdata/fast_reasoning.jsonl. It was recorded from an httptest
// fake gateway, never from a live endpoint.

const (
	cassetteDir   = "../recorded/testdata"
	cassetteRole  = providers.RoleFastReasoning
	cassetteModel = "fake-gateway-model"
	cassetteToken = "fake-gateway-token-do-not-log"
	// cassetteEndpoint is never dialled: replay answers before any socket is
	// opened, and a dial fails the test (see rejectTransport).
	cassetteEndpoint = "http://gateway.invalid/v1"
)

// cassetteRound is one provider call plus the recorded interaction that answers
// it, so the gateway and cassette paths can be compared on identical terms.
type cassetteRound struct {
	name  string
	match func(*recorded.Interaction) bool
	call  func(context.Context, *Client) (string, error)
}

func cassetteRounds(image []byte) []cassetteRound {
	describe := func(r ChatResponse) string {
		return fmt.Sprintf("text=%q reasoning=%q finish=%s model=%s request_id=%s usage=%d/%d/%d",
			r.Text, r.Reasoning, r.FinishReason, r.Model, r.RequestID,
			r.Usage.Prompt, r.Usage.Completion, r.Usage.Reasoning)
	}
	return []cassetteRound{
		{
			name: "text-chat",
			match: func(in *recorded.Interaction) bool {
				return in.Request.Method == "POST" && !strings.Contains(string(in.Request.Body), "image_url")
			},
			call: func(ctx context.Context, c *Client) (string, error) {
				resp, err := c.Chat(ctx, ChatRequest{
					Messages:  []Message{{Role: "user", Text: "Reply with exactly: pong"}},
					MaxTokens: 32,
				})
				if err != nil {
					return "", err
				}
				return describe(resp), nil
			},
		},
		{
			name:  "vision-chat",
			match: func(in *recorded.Interaction) bool { return strings.Contains(string(in.Request.Body), "image_url") },
			call: func(ctx context.Context, c *Client) (string, error) {
				resp, err := c.Chat(ctx, ChatRequest{Messages: []Message{{
					Role:   "user",
					Text:   "What two colors are in this image, left then right? Answer as two lowercase words.",
					Images: [][]byte{image},
				}}, MaxTokens: 64})
				if err != nil {
					return "", err
				}
				return describe(resp), nil
			},
		},
		{
			name:  "list-models",
			match: func(in *recorded.Interaction) bool { return in.Request.Method == "GET" },
			call: func(ctx context.Context, c *Client) (string, error) {
				ids, err := c.ListModels(ctx)
				if err != nil {
					return "", err
				}
				return "models=" + strings.Join(ids, ","), nil
			},
		},
	}
}

// TestGatewayAndCassetteProduceIdenticalResults is the "identical" proof: each
// recorded interaction is replayed twice — once by an httptest gateway serving
// the exact status, allowlisted headers and body that were captured, once by the
// cassette transport — and the normalised ChatResponse must agree in every
// field.
func TestGatewayAndCassetteProduceIdenticalResults(t *testing.T) {
	entries, err := recorded.LoadRole(cassetteDir, cassetteRole)
	if err != nil {
		t.Fatalf("load committed cassette: %v", err)
	}
	image := redBluePNG(t, 64, 64)
	ctx := context.Background()

	for _, round := range cassetteRounds(image) {
		var entry *recorded.Interaction
		for _, in := range entries {
			if round.match(in) {
				entry = in
				break
			}
		}
		if entry == nil {
			t.Errorf("%s: the committed cassette has no matching interaction", round.name)
			continue
		}

		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer "+cassetteToken {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"message":"missing bearer token"}}`)
				return
			}
			for k, v := range entry.Response.Headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(entry.Response.Status)
			fmt.Fprint(w, entry.Response.Body)
		}))

		fromGateway, err := round.call(ctx, &Client{
			BaseURL: gateway.URL + "/v1", Token: cassetteToken, Model: cassetteModel,
			HTTP: &http.Client{},
		})
		gateway.Close()
		if err != nil {
			t.Errorf("%s via gateway: %v", round.name, err)
			continue
		}

		probe := &originSniffer{}
		fromCassette, err := round.call(ctx, &Client{
			BaseURL: cassetteEndpoint, Token: cassetteToken, Model: cassetteModel,
			HTTP: &http.Client{Transport: probe.wrap(t, cassetteReplay(t))},
		})
		if err != nil {
			t.Errorf("%s via cassette: %v", round.name, err)
			continue
		}
		if fromGateway != fromCassette {
			t.Errorf("%s: gateway and cassette disagree\n gateway : %s\n cassette: %s", round.name, fromGateway, fromCassette)
			continue
		}
		for _, origin := range probe.origins {
			if origin != recorded.OriginRecorded {
				t.Errorf("%s: cassette response origin = %q, want recorded", round.name, origin)
			}
		}
		t.Logf("%s: identical through gateway and cassette (%s)", round.name, fromCassette)
	}
}

func cassetteReplay(t *testing.T) *recorded.Transport {
	t.Helper()
	tr, err := recorded.New(recorded.Options{
		Role:    cassetteRole,
		Mode:    recorded.ModeReplay,
		Dir:     cassetteDir,
		Base:    &rejectTransport{t: t},
		Secrets: []string{cassetteToken},
	})
	if err != nil {
		t.Fatalf("recorded.New: %v", err)
	}
	return tr
}

// originSniffer records the provenance label of every response, which is how a
// caller labels a stage in a report without the adapter knowing about cassettes.
type originSniffer struct{ origins []string }

func (s *originSniffer) wrap(t *testing.T, next http.RoundTripper) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := next.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		s.origins = append(s.origins, recorded.Origin(resp))
		return resp, nil
	})
}

// rejectTransport fails the test if a replay ever tries to leave the process.
type rejectTransport struct{ t *testing.T }

func (rt *rejectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.t.Errorf("replay dialled out to %s %s — cassette replay must never touch the network", req.Method, req.URL.Path)
	return nil, errors.New("replay must not dial")
}

// TestCassetteMissSurvivesAdapterClassification proves the rule in
// transportError: a transport that already classifies a failure is not flattened
// into a retryable "transport failure", which would tell the router to retry
// something that can never succeed.
func TestCassetteMissSurvivesAdapterClassification(t *testing.T) {
	c := &Client{BaseURL: cassetteEndpoint, Model: cassetteModel, HTTP: &http.Client{Transport: cassetteReplay(t)}}
	_, err := c.Chat(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Text: "This was never recorded against the fake gateway."}},
		MaxTokens: 8,
	})
	if err == nil {
		t.Fatal("an unrecorded request returned a response")
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *providers.Error", err, err)
	}
	if pe.Kind == providers.KindUnavailable {
		t.Errorf("the cassette miss was flattened into a retryable transport failure: %s", pe)
	}
	if pe.Kind != providers.KindUnsupportedCapability || pe.Retryable {
		t.Errorf("Kind=%q Retryable=%v, want unsupported_capability and non-retryable (%s)", pe.Kind, pe.Retryable, pe)
	}
	if !recorded.IsMiss(err) {
		t.Error("recorded.IsMiss did not see through the adapter")
	}
	if digest, ok := recorded.MissDigest(err); !ok || !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("MissDigest = %q,%v, want the cassette key digest", digest, ok)
	}
}

// TestTransportClassifiedErrorIsPreserved covers the same rule for a plain
// taxonomy error from any other transport (a router, a proxy, a future adapter).
func TestTransportClassifiedErrorIsPreserved(t *testing.T) {
	stub := roundTripFunc(func(*http.Request) (*http.Response, error) {
		e := providers.NewError(providers.KindRateLimited, http.StatusTooManyRequests, "gateway says slow down")
		e.RequestID = "stub-1"
		return nil, e
	})
	c := &Client{BaseURL: "http://gateway.invalid/v1", Token: recordedToken, Model: recordedModel,
		HTTP: &http.Client{Transport: stub}}
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Text: "pong?"}}})
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if pe.Kind != providers.KindRateLimited || pe.Status != http.StatusTooManyRequests || pe.RequestID != "stub-1" {
		t.Errorf("classification lost: %+v", pe)
	}
	if pe.Model != recordedModel {
		t.Errorf("Model = %q, want the client's model filled in for provenance", pe.Model)
	}
	if strings.Contains(pe.Error(), recordedToken) {
		t.Errorf("error text leaked the token: %s", pe)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
