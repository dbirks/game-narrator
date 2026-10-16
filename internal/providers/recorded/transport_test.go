package recorded

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dbirks/game-narrator/internal/providers"
)

// The gateway host used by these tests. It is a reserved-bad name (RFC 2606)
// and it must never appear in a cassette: only path and query are stored.
const testEndpoint = "https://gateway.invalid/v1/chat/completions"

// stubTransport answers without a network so the record/replay mechanics can be
// unit-tested. It records the body it was handed, which is how the tests prove
// draining the body for redaction did not corrupt the forwarded request.
type stubTransport struct {
	calls   atomic.Int64
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	// next returns the response for request number n (1-based).
	next func(req *http.Request, n int) (*http.Response, error)
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, raw)
	s.headers = append(s.headers, req.Header.Clone())
	n := len(s.bodies)
	s.mu.Unlock()
	s.calls.Add(1)
	if s.next == nil {
		return nil, errors.New("stubTransport: no canned response configured")
	}
	return s.next(req, n)
}

func (s *stubTransport) callBodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.bodies...)
}

// headerAt returns the headers the stub actually received, to prove record mode
// forwards the caller's request unchanged and redacts only the cassette.
func (s *stubTransport) headerAt(i int) http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.headers) {
		return nil
	}
	return s.headers[i]
}

// neverDials is the isolation probe: any outbound call fails the test.
type neverDials struct {
	t     *testing.T
	calls atomic.Int64
}

func (n *neverDials) RoundTrip(req *http.Request) (*http.Response, error) {
	n.calls.Add(1)
	n.t.Errorf("replay mode dialled out to %s %s — cassette replay must never touch the network", req.Method, req.URL.Path)
	return nil, errors.New("replay must not dial")
}

func canned(status int, contentType, body string) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", contentType)
	h.Set("X-Request-Id", "req-canned-1")
	h.Set("Set-Cookie", "session=should-not-be-recorded")
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newPostQuery(query string) *http.Request {
	req := newPost(`{"model":"m","messages":[{"role":"user","text":"paged"}],"stream":false}`)
	req.URL.RawQuery = query
	return req
}

func newPost(body string) *http.Request {
	req, err := http.NewRequest(http.MethodPost, testEndpoint, strings.NewReader(body))
	if err != nil {
		panic(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}

func TestModeFromEnvMatchesTheDocumentedModes(t *testing.T) {
	lookup := func(k string) (string, bool) { return "", false }
	mode, dir, err := ModeFromEnv(lookup)
	if err != nil || mode != ModeLive || dir != "" {
		t.Fatalf("ModeFromEnv(empty) = %q,%q,%v, want live,\"\",nil", mode, dir, err)
	}
	env := func(m, d string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			switch k {
			case EnvMode:
				return m, m != ""
			case EnvCassetteDir:
				return d, d != ""
			}
			return "", false
		}
	}
	for _, tc := range []struct {
		name, mode, dir string
		wantMode        Mode
		wantErr         string
	}{
		{name: "explicit live", mode: "LIVE", wantMode: ModeLive},
		{name: "record needs a dir", mode: "record", wantErr: EnvCassetteDir},
		{name: "replay needs a dir", mode: "replay", wantErr: EnvCassetteDir},
		{name: "record with dir", mode: "record", dir: "/cassettes", wantMode: ModeRecord},
		{name: "replay with dir", mode: " replay ", dir: " /cassettes ", wantMode: ModeReplay},
		{name: "unknown mode", mode: "simulate", wantMode: Mode("simulate"), wantErr: EnvMode},
	} {
		gotMode, gotDir, err := ModeFromEnv(env(tc.mode, tc.dir))
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: ModeFromEnv err = %v, want it to name %s", tc.name, err, tc.wantErr)
				continue
			}
			if tc.wantMode != "" && gotMode != tc.wantMode {
				t.Errorf("%s: mode = %q, want the rejected value %q to be reported back", tc.name, gotMode, tc.wantMode)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: ModeFromEnv err = %v", tc.name, err)
			continue
		}
		if gotMode != tc.wantMode {
			t.Errorf("%s: mode = %q, want %q", tc.name, gotMode, tc.wantMode)
		}
		if tc.wantMode != ModeLive && gotDir != "/cassettes" {
			t.Errorf("%s: dir = %q, want the trimmed cassette dir", tc.name, gotDir)
		}
	}
}

func TestNewValidatesModeAndDir(t *testing.T) {
	if _, err := New(Options{Role: providers.RoleFastReasoning, Mode: Mode("simulate")}); err == nil {
		t.Error("New accepted an unknown mode")
	}
	if _, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeReplay}); err == nil {
		t.Error("New accepted replay mode without a cassette directory")
	}
	if _, err := New(Options{Role: providers.Role("../etc"), Mode: ModeRecord, Dir: t.TempDir()}); err == nil {
		t.Error("New accepted a role that escapes the cassette directory")
	}
	tr, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeLive})
	if err != nil {
		t.Fatalf("live mode with no dir: %v", err)
	}
	if tr.Path() != "" {
		t.Errorf("live transport reports cassette path %q, want none", tr.Path())
	}
}

// TestRecordThenReplayRoundTrip is the core proof: one transport records
// against a stubbed endpoint, a second transport replays the same two
// interactions from the file with the endpoint removed entirely.
func TestRecordThenReplayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stub := &stubTransport{next: func(req *http.Request, n int) (*http.Response, error) {
		switch n {
		case 1:
			return canned(http.StatusOK, "application/json", `{"choices":[{"message":{"content":"pong"}}]}`), nil
		default:
			return canned(http.StatusTooManyRequests, "application/json", `{"error":{"message":"slow down"}}`), nil
		}
	}}
	recorder, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeRecord, Dir: dir, Base: stub, Secrets: []string{testToken}})
	if err != nil {
		t.Fatalf("New(record): %v", err)
	}
	client := recorder.Client()

	firstBody := `{"model":"m","messages":[{"role":"user","text":"pong"}],"stream":false}`
	secondBody := `{"model":"m","messages":[{"role":"user","text":"again"}],"stream":false}`
	first := newPost(firstBody)
	resp, err := client.Do(first)
	if err != nil {
		t.Fatalf("record request 1: %v", err)
	}
	if got := Origin(resp); got != OriginLive {
		t.Errorf("Origin during record = %q, want %q", got, OriginLive)
	}
	drainAndClose(resp)

	second := newPost(secondBody)
	resp, err = client.Do(second)
	if err != nil {
		t.Fatalf("record request 2: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("recorded status = %d, want the stub's 429 to reach the caller", resp.StatusCode)
	}
	drainAndClose(resp)

	// Draining and redacting the body for the cassette may not alter what
	// actually goes out: the stub must see the caller's bytes and headers.
	bodies := stub.callBodies()
	if len(bodies) != 2 {
		t.Fatalf("stub received %d bodies, want 2", len(bodies))
	}
	if string(bodies[0]) != firstBody || string(bodies[1]) != secondBody {
		t.Errorf("record mode altered the forwarded request:\n got 0 = %s\n got 1 = %s", bodies[0], bodies[1])
	}
	if got := stub.headerAt(0); got == nil || got.Get("Authorization") != "Bearer "+testToken {
		t.Error("record mode must forward the caller's headers unchanged; only the cassette drops Authorization")
	}

	cassette := recorderCassette(t, dir)
	if lines := strings.Count(cassette, "\n"); lines != 2 {
		t.Fatalf("cassette has %d lines, want 2:\n%s", lines, cassette)
	}
	assertNoLeakage(t, cassette)

	replayBase := &neverDials{t: t}
	replayer, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeReplay, Dir: dir, Base: replayBase, Secrets: []string{testToken}})
	if err != nil {
		t.Fatalf("New(replay): %v", err)
	}
	replayClient := replayer.Client()

	for _, tc := range []struct {
		body       string
		wantStatus int
		wantBody   string
	}{
		{`{"stream":false,"messages":[{"role":"user","text":"pong"}],"model":"m"}`, http.StatusOK, `{"choices":[{"message":{"content":"pong"}}]}`},
		{`{"model":"m","messages":[{"role":"user","text":"again"}],"stream":false}`, http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`},
	} {
		resp, err := replayClient.Do(newPost(tc.body))
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		got := Origin(resp)
		gotBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if got != OriginRecorded {
			t.Errorf("Origin in replay = %q, want %q", got, OriginRecorded)
		}
		if resp.StatusCode != tc.wantStatus || string(gotBody) != tc.wantBody {
			t.Errorf("replay = %d %q, want %d %q", resp.StatusCode, gotBody, tc.wantStatus, tc.wantBody)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("replayed Content-Type = %q, want the recorded value", ct)
		}
		if rid := resp.Header.Get("X-Request-Id"); rid != "req-canned-1" {
			t.Errorf("replayed x-request-id = %q, want the recorded correlation id", rid)
		}
	}
	if _, misses, dials := replayer.Stats(); misses != 0 || dials != 0 || replayBase.calls.Load() != 0 {
		t.Errorf("replay stats hits/misses/dials = %d/%d, probe calls %d, want no dials", 2, misses, replayBase.calls.Load())
	}
}

// TestReplayMatchesIndependentOfKeyOrderAndImageDigest shows the matching key
// is method+path+canonical redacted body: a re-serialised request with the same
// logical content, and a re-sent identical image, both hit.
func TestReplayMatchesIndependentOfKeyOrder(t *testing.T) {
	dir := t.TempDir()
	img := testImage(t)
	wire := visionBody(t, "m", "colours?", img)
	stub := &stubTransport{next: func(*http.Request, int) (*http.Response, error) {
		return canned(http.StatusOK, "application/json", `{"choices":[{"message":{"content":"red blue"}}]}`), nil
	}}
	recorder, err := New(Options{Role: providers.RoleVisionOCRFallback, Mode: ModeRecord, Dir: dir, Base: stub, Secrets: []string{testToken}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := recorder.Client().Do(newPost(string(wire))); err != nil {
		t.Fatalf("record: %v", err)
	}

	cassette := recorderCassette(t, dir)
	if strings.Contains(cassette, "base64") || strings.Contains(cassette, "gateway.invalid") || strings.Contains(cassette, "://") {
		t.Errorf("cassette leaked an image payload or the endpoint:\n%s", cassette)
	}
	if !strings.Contains(cassette, sha256Hex(img)) {
		t.Error("cassette does not carry the image digest used for matching")
	}

	replayer, err := New(Options{Role: providers.RoleVisionOCRFallback, Mode: ModeReplay, Dir: dir, Base: &neverDials{t: t}})
	if err != nil {
		t.Fatalf("New(replay): %v", err)
	}
	// Same logical request, different serialisation order and whitespace.
	shuffled := `{ "stream" : false, "messages" : [ { "content" : [ {"image_url":{"url":"data:image/png;base64,` +
		base64.StdEncoding.EncodeToString(img) + `"},"type":"image_url"} , {"text":"colours?","type":"text"} ], "role": "user" } ], "model" : "m" } }`
	// The shuffled form differs structurally (part order), so it must miss;
	// re-serialising the *same* structure in a different key order must hit.
	if _, err := replayer.Client().Do(newPost(shuffled)); err == nil {
		t.Error("reordered content parts matched a recording they do not equal")
	} else if !IsMiss(err) {
		t.Errorf("err = %v, want a cassette miss", err)
	}
	reordered := `{"stream":false,"messages":[{"role":"user","content":[{"text":"colours?","type":"text"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
		base64.StdEncoding.EncodeToString(img) + `"}}]}],"model":"m"}`
	resp, err := replayer.Client().Do(newPost(reordered))
	if err != nil {
		t.Fatalf("replay with re-serialised body: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "red blue") {
		t.Errorf("replayed body = %q", body)
	}
}

// TestReplayQueryIsPartOfTheKey records the deliberate strengthening of the
// matching rule: the canonical query joins method+path+body in the key, because
// answering a differently-scoped request from the same path would be a
// fabrication. Equivalent parameter orders still match.
func TestReplayQueryIsPartOfTheKey(t *testing.T) {
	dir := t.TempDir()
	stub := &stubTransport{next: func(*http.Request, int) (*http.Response, error) {
		return canned(http.StatusOK, "application/json", `{"page":"two"}`), nil
	}}
	recorder, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeRecord, Dir: dir, Base: stub})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := recorder.Client().Do(newPostQuery("b=2&a=1")); err != nil {
		t.Fatalf("record: %v", err)
	}

	replayer, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeReplay, Dir: dir, Base: &neverDials{t: t}})
	if err != nil {
		t.Fatalf("New(replay): %v", err)
	}
	resp, err := replayer.Client().Do(newPostQuery("a=1&b=2"))
	if err != nil {
		t.Errorf("a canonically equivalent query missed: %v", err)
	} else {
		drainAndClose(resp)
	}
	if _, err := replayer.Client().Do(newPostQuery("a=1")); err == nil || !IsMiss(err) {
		t.Errorf("a narrower query err = %v, want a cassette miss", err)
	}
}

func TestReplayDuplicateKeysReplayInRecordedOrder(t *testing.T) {
	dir := t.TempDir()
	body := `{"model":"m","messages":[{"role":"user","text":"advance"}],"stream":false}`
	answers := []string{"first", "second"}
	i := 0
	stub := &stubTransport{next: func(*http.Request, int) (*http.Response, error) {
		answer := answers[i]
		i++
		return canned(http.StatusOK, "application/json", `{"content":"`+answer+`"}`), nil
	}}
	recorder, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeRecord, Dir: dir, Base: stub})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range answers {
		resp, err := recorder.Client().Do(newPost(body))
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		drainAndClose(resp)
	}
	replayer, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeReplay, Dir: dir, Base: &neverDials{t: t}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, want := range answers {
		resp, err := replayer.Client().Do(newPost(body))
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(got), want) {
			t.Errorf("replayed %q, want the recorded %q answer (recorded order must be preserved)", got, want)
		}
	}
	// A third identical request has no recording behind it: it must miss rather
	// than repeat the last answer.
	if _, err := replayer.Client().Do(newPost(body)); err == nil || !IsMiss(err) {
		t.Errorf("third identical request err = %v, want a cassette miss", err)
	}
}

func TestReplayMissNamesTheKeyDigest(t *testing.T) {
	dir := t.TempDir()
	replayer, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeReplay, Dir: dir, Base: &neverDials{t: t}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := newPost(`{"model":"m","messages":[{"role":"user","text":"unrecorded"}],"stream":false}`)
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	parsed, err := newRequest(req, raw)
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	wantKey := parsed.KeyDigest()

	_, err = replayer.Client().Do(newPost(string(raw)))
	if err == nil {
		t.Fatal("replay with no cassette at all returned a response")
	}
	if !IsMiss(err) {
		t.Fatalf("err = %v (%T), want *recorded.MissError", err, err)
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err does not unwrap to *providers.Error: %v", err)
	}
	if pe.Kind != providers.KindUnsupportedCapability {
		t.Errorf("Kind = %q, want unsupported_capability", pe.Kind)
	}
	if pe.Retryable {
		t.Error("a cassette miss must not be retried")
	}
	if !strings.Contains(pe.Msg, wantKey) {
		t.Errorf("error does not name the key digest %s: %s", wantKey, pe)
	}
	if !strings.Contains(pe.Msg, "cassette_miss") {
		t.Errorf("error = %q, want the cassette_miss label", pe.Msg)
	}
	if got, ok := MissDigest(err); !ok || got != wantKey {
		t.Errorf("MissDigest = %q,%v, want %q", got, ok, wantKey)
	}
	if !errors.Is(err, &providers.Error{Kind: providers.KindUnsupportedCapability}) {
		t.Error("errors.Is with a taxonomy kind must match a cassette miss")
	}
	if hits, missCount, dials := replayer.Stats(); hits != 0 || missCount != 1 || dials != 0 {
		t.Errorf("replay stats = hits %d misses %d dials %d, want 0/1/0", hits, missCount, dials)
	}
	// The same error survives an adapter wrapping it.
	wrapped := fmt.Errorf("openai: while calling fast_reasoning: %w", err)
	if !IsMiss(wrapped) {
		t.Error("IsMiss lost the classification through wrapping")
	}
}

// TestReplayOfMissingCassetteFileNeverFabricates is the "no silent substitution"
// rule: an absent cassette yields misses for everything, never a stub answer.
func TestReplayOfMissingCassetteFileNeverFabricates(t *testing.T) {
	dir := t.TempDir()
	probe := &neverDials{t: t}
	replayer, err := New(Options{Role: providers.RoleDecision, Mode: ModeReplay, Dir: dir, Base: probe})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://gateway.invalid/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := replayer.Client().Do(req)
	if err == nil {
		drainAndClose(resp)
		t.Fatal("a GET against a missing cassette succeeded; nothing was recorded")
	}
	if !IsMiss(err) || !strings.Contains(err.Error(), filepath.Join(dir, "decision.jsonl")) {
		t.Errorf("err = %v, want it to name the cassette file it looked for", err)
	}
	if probe.calls.Load() != 0 {
		t.Errorf("probe dialed %d times in replay mode", probe.calls.Load())
	}
}

func TestLiveModePassesThroughAndLabels(t *testing.T) {
	stub := &stubTransport{next: func(*http.Request, int) (*http.Response, error) {
		return canned(http.StatusOK, "application/json", `{"ok":true}`), nil
	}}
	dir := t.TempDir()
	live, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeLive, Dir: dir, Base: stub})
	if err != nil {
		t.Fatalf("New(live): %v", err)
	}
	resp, err := live.Client().Do(newPost(`{"model":"m","messages":[],"stream":false}`))
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	if Origin(resp) != OriginLive {
		t.Errorf("Origin = %q, want live", Origin(resp))
	}
	drainAndClose(resp)
	if _, err := os.Stat(filepath.Join(dir, "fast_reasoning.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Error("live mode wrote a cassette")
	}
	if _, misses, dials := live.Stats(); dials != 1 || misses != 0 {
		t.Errorf("live stats = misses %d dials %d, want one dial", misses, dials)
	}
}

// TestRecordRefusesWhatItCannotFaithfullyReplay covers the documented limits:
// streaming and binary responses fail clearly and leave no cassette behind.
func TestRecordRefusesWhatItCannotFaithfullyReplay(t *testing.T) {
	cases := []struct {
		name    string
		request string
		resp    *http.Response
		want    string
	}{
		{
			name:    "streaming request",
			request: `{"model":"m","messages":[{"role":"user","text":"hi"}],"stream":true}`,
			resp:    canned(http.StatusOK, "application/json", `{"ok":true}`),
			want:    "streaming request",
		},
		{
			name:    "event-stream response",
			request: `{"model":"m","messages":[{"role":"user","text":"hi"}],"stream":false}`,
			resp:    canned(http.StatusOK, "text/event-stream", "data: {}\n\n"),
			want:    "streaming response",
		},
		{
			name:    "binary response",
			request: `{"model":"m","messages":[{"role":"user","text":"hi"}],"stream":false}`,
			resp:    canned(http.StatusOK, "application/octet-stream", "\x00\x01\x02\xff\xfe binary audio"),
			want:    "binary response body",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := &stubTransport{next: func(*http.Request, int) (*http.Response, error) {
				return canned(tc.resp.StatusCode, tc.resp.Header.Get("Content-Type"), readCannedBody(t, tc.resp)), nil
			}}
			recorder, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeRecord, Dir: dir, Base: stub})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = recorder.Client().Do(newPost(tc.request))
			if err == nil {
				t.Fatal("an unrecordable interaction was accepted")
			}
			var pe *providers.Error
			if !errors.As(err, &pe) || pe.Kind != providers.KindUnsupportedCapability {
				t.Fatalf("err = %v (%T), want *providers.Error/unsupported_capability", err, err)
			}
			if !strings.Contains(pe.Msg, tc.want) {
				t.Errorf("err = %q, want it to explain %q", pe.Msg, tc.want)
			}
			if entries := recorderCassette(t, dir); entries != "" {
				t.Errorf("a refused interaction still wrote cassette content:\n%s", entries)
			}
		})
	}
}

// TestReplayIsConcurrencySafeAndConsumesInOrder protects the shared cursor under
// -race: two workers replaying the same key get the two recorded answers, and a
// third worker misses.
func TestReplayIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	body := `{"model":"m","messages":[{"role":"user","text":"tick"}],"stream":false}`
	answers := []string{"a", "b", "c", "d", "e", "f"}
	i := 0
	stub := &stubTransport{next: func(*http.Request, int) (*http.Response, error) {
		answer := answers[i]
		i++
		return canned(http.StatusOK, "application/json", `{"n":"`+answer+`"}`), nil
	}}
	recorder, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeRecord, Dir: dir, Base: stub})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range answers {
		resp, err := recorder.Client().Do(newPost(body))
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		drainAndClose(resp)
	}
	replayer, err := New(Options{Role: providers.RoleFastReasoning, Mode: ModeReplay, Dir: dir, Base: &neverDials{t: t}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var missCount int
	var wg sync.WaitGroup
	for range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := replayer.Client().Do(newPost(body))
			if err != nil {
				mu.Lock()
				missCount++
				mu.Unlock()
				return
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			mu.Lock()
			seen[string(got)]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if missCount != 0 {
		t.Fatalf("%d concurrent replays missed", missCount)
	}
	if len(seen) != len(answers) {
		t.Errorf("got %d distinct answers for %d recordings: %v", len(seen), len(answers), seen)
	}
	for answer, count := range seen {
		if count != 1 {
			t.Errorf("answer %q served %d times; each recording must be served once", answer, count)
		}
	}
}

func TestOriginHelpers(t *testing.T) {
	if got := Origin(nil); got != OriginUnknown {
		t.Errorf("Origin(nil) = %q, want %q", got, OriginUnknown)
	}
	if got := OriginFromHeader(nil); got != OriginUnknown {
		t.Errorf("OriginFromHeader(nil) = %q", got)
	}
	h := http.Header{}
	if got := OriginFromHeader(h); got != OriginUnknown {
		t.Errorf("OriginFromHeader(empty) = %q", got)
	}
	h.Set(HeaderOrigin, "RECORDED")
	if got := OriginFromHeader(h); got != OriginRecorded {
		t.Errorf("OriginFromHeader = %q, want recorded (case-insensitive)", got)
	}
	h.Set(HeaderOrigin, "provider-invented")
	if got := OriginFromHeader(h); got != OriginUnknown {
		t.Errorf("an unrecognised origin label was trusted: %q", got)
	}
}

func TestFromEnvWiresSecretsAndMode(t *testing.T) {
	dir := t.TempDir()
	lookup := func(k string) (string, bool) {
		switch k {
		case EnvMode:
			return "replay", true
		case EnvCassetteDir:
			return dir, true
		case providers.EnvGatewayURL:
			return "https://gateway.invalid/v1", true
		case providers.EnvGatewayToken:
			return testToken, true
		}
		return "", false
	}
	tr, err := FromEnv(providers.RoleFastReasoning, lookup)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if tr.Mode != ModeReplay || tr.Dir != dir || tr.Path() != filepath.Join(dir, "fast_reasoning.jsonl") {
		t.Errorf("transport mode=%q dir=%q path=%q, want replay against %s", tr.Mode, tr.Dir, tr.Path(), filepath.Join(dir, "fast_reasoning.jsonl"))
	}
	if len(tr.Secrets) != 1 || tr.Secrets[0] != testToken {
		t.Error("FromEnv did not carry the resolved gateway token into the redaction guard")
	}
	bad := func(k string) (string, bool) {
		if k == EnvMode {
			return "recompute", true
		}
		return "", false
	}
	if _, err := FromEnv(providers.RoleFastReasoning, bad); err == nil || !strings.Contains(err.Error(), EnvMode) {
		t.Errorf("FromEnv with a bad mode = %v, want an error naming %s", err, EnvMode)
	}
}

// recorderCassette returns the cassette file contents in dir, or "" when no
// interaction has been written yet.
func recorderCassette(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cassette dir: %v", err)
	}
	var out strings.Builder
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out.WriteString(string(raw))
	}
	return out.String()
}

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func readCannedBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read canned body: %v", err)
	}
	resp.Body.Close()
	return string(raw)
}
