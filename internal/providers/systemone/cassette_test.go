package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/recorded"
)

// The cassette harness is the proof that this adapter is transport-agnostic:
// the same Decide call is served by a live gateway, by a recorder, and by a
// replay with the byte-identical recorded answer (internal/providers/recorded,
// ADR-0013). The committed cassette under testdata/decision.jsonl was recorded
// from an httptest fake gateway, never from a live endpoint.

const (
	cassetteDir   = "testdata"
	cassetteFile  = "decision.jsonl"
	cassetteModel = "clef-fake"
	cassetteToken = "fake-gateway-token-do-not-log"
)

// bundleFixture builds the docs/spec/05 adjudication bundle with fixed inputs,
// so the recorded request is reproducible.
func bundleFixture() Request {
	return BuildAdjudication(
		Context{Scene: "outset", OCR: "Would you like to save your progress?", LastMessage: "m-11"},
		[]Candidate{
			{ID: "m-12", Text: "Would you like to save your progress?"},
			{ID: "m-13", Text: "The village is quiet tonight."},
		},
		[]Speaker{
			{ID: "npc_a", Description: "the blacksmith"},
			{ID: "narrator", Description: "world or instruction text"},
		},
	)
}

// answerBundle is the fake model: it answers every question in the request
// with a valid, plausible distribution, so the harness proves the client
// rather than the model.
func answerBundle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Questions map[string]struct {
			Type     string            `json:"type"`
			Criteria map[string]string `json:"criteria"`
			Levels   []string          `json:"levels"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"detail":"bad request"}`, http.StatusUnprocessableEntity)
		return
	}
	answers := map[string]any{}
	for id, q := range req.Questions {
		switch q.Type {
		case "choice":
			opts := make([]string, 0, len(q.Criteria))
			for opt := range q.Criteria {
				opts = append(opts, opt)
			}
			sort.Strings(opts)
			probs := map[string]float64{}
			if len(opts) > 0 {
				probs[opts[0]] = 0.8
				rest := 0.2 / float64(len(opts)-1)
				for _, o := range opts[1:] {
					probs[o] = rest
				}
			}
			answers[id] = map[string]any{"type": "choice", "choice": opts[0], "confidence": 0.8, "probabilities": probs}
		case "noul":
			answers[id] = map[string]any{"type": "noul", "noul": 0.9}
		case "score":
			level := "unknown"
			if len(q.Levels) > 0 {
				level = q.Levels[0]
			}
			answers[id] = map[string]any{"type": "score", "level": level, "confidence": 0.85}
		default:
			http.Error(w, `{"detail":"unknown question type"}`, http.StatusUnprocessableEntity)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", "req-fake-1")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":   cassetteModel,
		"answers": answers,
		"usage":   map[string]int{"input_tokens": 246, "output_tokens": 0},
	})
}

// newFakeSystemOneGateway serves the /systemone route.
func newFakeSystemOneGateway(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/systemone" {
			http.Error(w, `{"detail":"no such route"}`, http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+cassetteToken {
			http.Error(w, `{"detail":"invalid api key"}`, http.StatusUnauthorized)
			return
		}
		answerBundle(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type rejectTransport struct{ t *testing.T }

func (rt *rejectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.t.Errorf("replay dialled out to %s %s — cassette replay must never touch the network", req.Method, req.URL.Path)
	return nil, errors.New("replay must not dial")
}

// decisionDescribe renders a Decision without latency so the live and
// recorded paths can be compared on identical terms.
func decisionDescribe(d Decision) string {
	keys := make([]string, 0, len(d.Answers))
	for k := range d.Answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		a := d.Answers[k]
		pkeys := make([]string, 0, len(a.Probabilities))
		for pk := range a.Probabilities {
			pkeys = append(pkeys, pk)
		}
		sort.Strings(pkeys)
		var probs []string
		for _, pk := range pkeys {
			probs = append(probs, fmt.Sprintf("%s=%.6f", pk, a.Probabilities[pk]))
		}
		parts = append(parts, fmt.Sprintf("%s{type=%s choice=%s conf=%.6f noul=%.6f score=%s probs=[%s]}",
			k, a.Type, a.Choice, a.Confidence, a.Noul, a.Score, strings.Join(probs, " ")))
	}
	return fmt.Sprintf("model=%s request_id=%s usage=%d/%d answers=[%s]",
		d.Model, d.RequestID, d.Usage.Input, d.Usage.Output, strings.Join(parts, " "))
}

// TestRecordedCassetteRoundTrip is the "identical" proof: the same Decide call
// through the live fake gateway, through the recorder, and through replay
// must agree in every field, and the replayed response must be labelled
// "recorded" while the live one is labelled "live".
func TestRecordedCassetteRoundTrip(t *testing.T) {
	gw := newFakeSystemOneGateway(t)
	req := bundleFixture()
	ctx := context.Background()

	// 1. Live path.
	live, err := New(gw.URL, cassetteToken, cassetteModel)
	if err != nil {
		t.Fatalf("New (live): %v", err)
	}
	dLive, err := live.Decide(ctx, req)
	if err != nil {
		t.Fatalf("live Decide: %v", err)
	}

	// 2. Record path: the same call through the recording transport writes
	// the committed cassette format into a temp dir.
	recDir := t.TempDir()
	rec, err := recorded.New(recorded.Options{
		Role:      providers.RoleDecision,
		Mode:      recorded.ModeRecord,
		Dir:       recDir,
		Base:      http.DefaultTransport,
		Secrets:   []string{cassetteToken},
		Now:       func() time.Time { return time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC) },
		LatencyMS: func(time.Duration) int64 { return 1234 },
	})
	if err != nil {
		t.Fatalf("recorded.New (record): %v", err)
	}
	recClient, err := New(gw.URL, cassetteToken, cassetteModel)
	if err != nil {
		t.Fatalf("New (record client): %v", err)
	}
	recClient.HTTP = rec.Client()
	dRec, err := recClient.Decide(ctx, req)
	if err != nil {
		t.Fatalf("record Decide: %v", err)
	}
	wrote, err := os.ReadFile(filepath.Join(recDir, cassetteFile))
	if err != nil {
		t.Fatalf("the recording transport wrote no cassette: %v", err)
	}
	committed, err := os.ReadFile(filepath.Join(cassetteDir, cassetteFile))
	if err != nil {
		if os.Getenv("UPDATE_CASSETTE") == "" {
			t.Fatalf("read committed cassette: %v (set UPDATE_CASSETTE=1 once to regenerate)", err)
		}
		committed = nil
	}
	recBytes := strings.TrimSpace(string(wrote))
	committedBytes := strings.TrimSpace(string(committed))
	if recBytes != committedBytes {
		if os.Getenv("UPDATE_CASSETTE") != "" {
			if err := os.WriteFile(filepath.Join(cassetteDir, cassetteFile), wrote, 0o644); err != nil {
				t.Fatalf("write committed cassette: %v", err)
			}
			t.Fatalf("committed cassette regenerated; inspect and re-run without UPDATE_CASSETTE")
		}
		t.Fatalf("recorded cassette diverged from the committed one:\n recorded : %s\n committed: %s", recBytes, committedBytes)
	}

	// 3. Replay path: no network, the cassette answers.
	probe := &originSniffer{}
	replay, err := recorded.New(recorded.Options{
		Role:    providers.RoleDecision,
		Mode:    recorded.ModeReplay,
		Dir:     cassetteDir,
		Base:    &rejectTransport{t: t},
		Secrets: []string{cassetteToken},
	})
	if err != nil {
		t.Fatalf("recorded.New (replay): %v", err)
	}
	replayClient, err := New("http://gateway.invalid", cassetteToken, cassetteModel)
	if err != nil {
		t.Fatalf("New (replay client): %v", err)
	}
	replayClient.HTTP = &http.Client{Transport: probe.wrap(replay)}
	dReplay, err := replayClient.Decide(ctx, req)
	if err != nil {
		t.Fatalf("replay Decide: %v", err)
	}
	if hits, misses, dials := replay.Stats(); dials != 0 {
		t.Fatalf("replay dialled %d times (hits=%d misses=%d)", dials, hits, misses)
	}

	// Identical in every field except the measured wall-clock latency.
	if decisionDescribe(dLive) != decisionDescribe(dReplay) {
		t.Fatalf("live and replay disagree:\n live  : %s\n replay: %s", decisionDescribe(dLive), decisionDescribe(dReplay))
	}
	if decisionDescribe(dRec) != decisionDescribe(dLive) {
		t.Fatalf("record and live disagree:\n record: %s\n live : %s", decisionDescribe(dRec), decisionDescribe(dLive))
	}
	if dLive.Origin != "live" || dRec.Origin != "live" || dReplay.Origin != "recorded" {
		t.Fatalf("origins = live:%q record:%q replay:%q, want live/live/recorded",
			dLive.Origin, dRec.Origin, dReplay.Origin)
	}
	for _, origin := range probe.origins {
		if origin != recorded.OriginRecorded {
			t.Fatalf("replayed response labelled %q, want recorded", origin)
		}
	}
	// The recorded path must not leak the token into the cassette.
	for _, line := range strings.Split(string(wrote), "\n") {
		if strings.Contains(line, cassetteToken) {
			t.Fatalf("the cassette contains the bearer token: %s", line)
		}
	}
	t.Logf("bundle round trip: live=%s", decisionDescribe(dLive))
}

// TestCassetteMissFailsLoudly: a request with no recorded interaction is an
// error naming the key digest, never an invented answer.
func TestCassetteMissFailsLoudly(t *testing.T) {
	replay, err := recorded.New(recorded.Options{
		Role:    providers.RoleDecision,
		Mode:    recorded.ModeReplay,
		Dir:     cassetteDir,
		Base:    &rejectTransport{t: t},
		Secrets: []string{cassetteToken},
	})
	if err != nil {
		t.Fatalf("recorded.New: %v", err)
	}
	c, err := New("http://gateway.invalid", cassetteToken, cassetteModel)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.HTTP = &http.Client{Transport: replay}
	_, err = c.Decide(context.Background(), Request{
		State:     map[string]any{"ocr": "never recorded"},
		Questions: []Question{{ID: "probe", Type: TypeNoul, Instructions: "was this ever asked?"}},
	})
	if err == nil {
		t.Fatal("an unrecorded request returned a response")
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *providers.Error", err, err)
	}
	if pe.Kind != providers.KindUnsupportedCapability || pe.Retryable {
		t.Fatalf("Kind=%q Retryable=%v, want unsupported_capability and non-retryable (%s)", pe.Kind, pe.Retryable, pe)
	}
	if !recorded.IsMiss(err) {
		t.Error("recorded.IsMiss did not see through the adapter")
	}
	if digest, ok := recorded.MissDigest(err); !ok || !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("MissDigest = %q,%v, want the cassette key digest", digest, ok)
	}
}

// originSniffer records the provenance label of every response.
type originSniffer struct{ origins []string }

func (s *originSniffer) wrap(next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := next.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		s.origins = append(s.origins, recorded.Origin(resp))
		return resp, nil
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
