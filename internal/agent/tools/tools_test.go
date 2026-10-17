package tools

// Test scaffolding for the onboarding tool set: a real SQLite store and
// asset store in temp dirs, the real generic adapter, and fakes for the
// outside world (search provider, chat model, TTS, web fetcher via
// httptest). The agent loop is the real agent.Run against the real
// executor — only the model and the network are scripted (test category (a)
// synthetic stubs per AGENTS.md; the live run in live_test.go is separate).

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/profileio"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/web"
	"github.com/dbirks/game-narrator/internal/web/search"
)

type env struct {
	t      *testing.T
	ctx    context.Context
	dir    string
	store  *store.Store
	agent  *store.AgentStore
	assets *assets.Local
	deps   Deps
	reg    *agent.Registry
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), dir: t.TempDir()}
	e.open()
	return e
}

func (e *env) open() {
	e.t.Helper()
	db, err := store.Open(e.ctx, filepath.Join(e.dir, store.FileName))
	if err != nil {
		e.t.Fatalf("store.Open: %v", err)
	}
	if _, err := db.Migrate(e.ctx); err != nil {
		e.t.Fatalf("Migrate: %v", err)
	}
	as, err := assets.NewLocal(filepath.Join(e.dir, "assets"))
	if err != nil {
		e.t.Fatalf("assets.NewLocal: %v", err)
	}
	reg := adapter.NewRegistry()
	if err := generic.Register(reg); err != nil {
		e.t.Fatalf("generic.Register: %v", err)
	}
	e.store, e.assets = db, as
	e.agent = store.NewAgentStore(db)
	e.deps = Deps{Store: db, Assets: as, Adapters: reg}
}

func (e *env) close() {
	e.t.Helper()
	if err := e.store.Close(); err != nil {
		e.t.Fatalf("store.Close: %v", err)
	}
}

func (e *env) tools(search search.Provider, fetcher *web.Fetcher) *agent.Registry {
	e.t.Helper()
	e.deps.Search = search
	e.deps.Fetcher = fetcher
	e.deps.Approvals = NewMemoryApprovals()
	if e.deps.Artwork == nil {
		e.deps.Artwork = newArtworkLedger()
	}
	reg, err := DefaultTools(e.deps)
	if err != nil {
		e.t.Fatalf("DefaultTools: %v", err)
	}
	e.reg = reg
	return reg
}

// game/edition/draft build the catalogue rows a profile needs.
func (e *env) game(slug, title string) games.Game {
	e.t.Helper()
	g := games.Game{
		ID: games.NewID(), Slug: slug, Title: title, Platform: "console-like",
		UserTags:  []byte(`["synthetic"]`),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := g.Validate(); err != nil {
		e.t.Fatalf("fixture game invalid: %v", err)
	}
	if err := e.store.CreateGame(e.ctx, g); err != nil {
		e.t.Fatalf("CreateGame: %v", err)
	}
	return g
}

func (e *env) edition(g games.Game, region, language, revision string) games.Edition {
	e.t.Helper()
	aspect := 1.3333
	row := games.Edition{
		ID: games.NewID(), GameID: g.ID, Region: region, Language: language,
		Revision: revision, Locale: language + "_" + region, DisplayAspect: &aspect,
	}
	if err := row.Validate(); err != nil {
		e.t.Fatalf("fixture edition invalid: %v", err)
	}
	if err := e.store.CreateEdition(e.ctx, row); err != nil {
		e.t.Fatalf("CreateEdition: %v", err)
	}
	return row
}

func (e *env) draft(editionID string) games.Profile {
	e.t.Helper()
	p, err := e.store.CreateDraftProfile(e.ctx, editionID)
	if err != nil {
		e.t.Fatalf("CreateDraftProfile: %v", err)
	}
	return p
}

// stageBytes puts content into the asset store and returns its SHA-256.
func (e *env) stageBytes(content []byte) string {
	e.t.Helper()
	staged, _, err := e.assets.PutStaged(e.ctx, bytes.NewReader(content), "")
	if err != nil {
		e.t.Fatalf("PutStaged: %v", err)
	}
	info, err := e.assets.Commit(e.ctx, staged)
	if err != nil {
		e.t.Fatalf("Commit: %v", err)
	}
	return info.SHA256
}

// ---------------------------------------------------------------------------
// Scripted model (same contract as agent.loop_test's fake)
// ---------------------------------------------------------------------------

type scriptedResponse struct {
	text      string
	toolCalls []openai.ToolCall
	err       error
}

type scriptedModel struct {
	mu        sync.Mutex
	responses []scriptedResponse
	requests  []openai.ChatRequest
	// responder answers when the static script is exhausted: tests use it
	// to continue a run with IDs the tools produced (e.g. a source_id that
	// only exists after the import ran).
	responder func(conv []openai.Message) scriptedResponse
}

func (m *scriptedModel) setResponder(fn func(conv []openai.Message) scriptedResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responder = fn
}

func (m *scriptedModel) Chat(ctx context.Context, req openai.ChatRequest) (openai.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	var r scriptedResponse
	if len(m.responses) > 0 {
		r = m.responses[0]
		m.responses = m.responses[1:]
	} else if m.responder != nil {
		r = m.responder(req.Messages)
	} else {
		return openai.ChatResponse{Text: "(script exhausted)", FinishReason: "stop"}, nil
	}
	if r.err != nil {
		return openai.ChatResponse{}, r.err
	}
	fin := "stop"
	if len(r.toolCalls) > 0 {
		fin = "tool_calls"
	}
	return openai.ChatResponse{Text: r.text, ToolCalls: r.toolCalls, FinishReason: fin}, nil
}

// callScripted builds a tool-call scripted response.
func callScripted(id, name string, args any) scriptedResponse {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return scriptedResponse{toolCalls: []openai.ToolCall{{ID: id, Name: name, Arguments: raw}}}
}

// runAgent drives one agent.Run over the env's store.
func (e *env) runAgent(t *testing.T, model *scriptedModel, runID, userRequest string, steps int) agent.Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	out, err := agent.Run(ctx, agent.Options{
		RunID:       runID,
		Model:       model,
		Registry:    e.reg,
		CallerRole:  auth.RoleOperator,
		Store:       e.agent,
		UserRequest: userRequest,
		Budget:      agent.BudgetConfig{MaxSteps: steps, MaxToolCalls: steps * 4},
		ModelLabel:  "scripted-fake",
		Prompt:      agent.PromptContext{UserRole: auth.RoleOperator},
	})
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	return out
}

// toolCallResults loads the persisted tool-call rows for a run.
func (e *env) toolCallResults(t *testing.T, runID string) map[string]string {
	t.Helper()
	rows, err := e.agent.ListAgentToolCalls(e.ctx, runID)
	if err != nil {
		t.Fatalf("ListAgentToolCalls: %v", err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Name+"#"+r.ID] = string(r.ResultJSON)
	}
	return out
}

// ---------------------------------------------------------------------------
// Fakes for the outside world
// ---------------------------------------------------------------------------

// fakeSearch records queries and returns canned results.
type fakeSearch struct {
	mu       sync.Mutex
	queries  []string
	results  []search.Result
	failNext bool
}

func (f *fakeSearch) Name() string { return "fake-search" }

func (f *fakeSearch) Search(ctx context.Context, query string, opts search.Options) ([]search.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	if f.failNext {
		f.failNext = false
		return nil, fmt.Errorf("fake search: boom")
	}
	if f.results != nil {
		return f.results, nil
	}
	return []search.Result{{
		Title: "Synthetic Adventure — modding wiki", URL: "https://wiki.example/synthetic-adventure",
		Snippet: "Script format documentation", Source: "fake-search",
		RetrievedAt: time.Now().UTC(),
	}}, nil
}

func (f *fakeSearch) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

// newFakeWeb stands up an httptest page and a REAL web.Fetcher that is
// allowed to reach exactly that server: a name-based PrivateAllow entry
// (the test host), the server's port, an injected resolver pinning the
// host to loopback, and a DialCheck that whitelists the server's exact
// (ip, port). Every other private address stays refused — the same
// pattern as internal/web's own tests.
func newFakeWeb(t *testing.T, h http.HandlerFunc) (fetchURL string, fetcher *web.Fetcher) {
	t.Helper()
	const host = "synthetic.example"
	s := httptestServer(t, h)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}
	srvPort := mustPort(u.Port())
	p := web.Policy{
		AllowPrivate: true,
		PrivateAllow: []string{host},
		AllowPorts:   []int{srvPort},
		Resolver: func(ctx context.Context, name string) ([]net.IP, error) {
			if name == host {
				return []net.IP{net.ParseIP("127.0.0.1")}, nil
			}
			return nil, fmt.Errorf("no resolution for %s", name)
		},
	}
	f := web.NewFetcher(p)
	f.DialCheck = func(ip net.IP, dialHost string, port int) error {
		if ip.Equal(net.ParseIP("127.0.0.1")) && port == srvPort {
			return nil
		}
		return fmt.Errorf("dial check: refusing %s:%d", dialHost, port)
	}
	t.Cleanup(f.Client.CloseIdleConnections)
	return fmt.Sprintf("http://%s:%d", host, srvPort), f
}

func mustPort(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

// fakeChat returns a canned JSON voice brief.
func fakeChat(brief string) ChatFn {
	return func(ctx context.Context, req openai.ChatRequest) (openai.ChatResponse, error) {
		return openai.ChatResponse{Text: brief, FinishReason: "stop"}, nil
	}
}

// fakeTTS returns a distinct small WAV per call.
type fakeTTS struct {
	mu      sync.Mutex
	calls   int
	failAll bool
}

func (f *fakeTTS) Design(ctx context.Context, req tts.DesignRequest) (tts.Audio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll {
		return tts.Audio{}, fmt.Errorf("fake tts: provider down")
	}
	f.calls++
	return tts.Audio{
		WAV:           makeWAV(f.calls),
		SampleRate:    8000,
		Channels:      1,
		BitsPerSample: 16,
		Duration:      900 * time.Millisecond,
		Model:         "fake-voice-design",
		Origin:        "fake_design task_type=VoiceDesign",
	}, nil
}

// makeWAV builds a minimal valid 16-bit PCM mono WAV (RIFF header + a short
// sine-like payload); the tool treats the bytes opaquely, but the header
// keeps the blob honest if an eyeball lands on it.
func makeWAV(seed int) []byte {
	const sampleRate = 8000
	samples := make([]int16, 400)
	for i := range samples {
		samples[i] = int16(8000 * math.Sin((float64(i)*2*3.14159*440)/float64(sampleRate)+float64(seed)))
	}
	dataSize := len(samples) * 2
	buf := bytes.NewBuffer(nil)
	binary.Write(buf, binary.LittleEndian, []byte("RIFF"))
	binary.Write(buf, binary.LittleEndian, uint32(36+dataSize))
	binary.Write(buf, binary.LittleEndian, []byte("WAVE"))
	binary.Write(buf, binary.LittleEndian, []byte("fmt "))
	binary.Write(buf, binary.LittleEndian, uint32(16))
	binary.Write(buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(buf, binary.LittleEndian, uint16(1)) // mono
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(buf, binary.LittleEndian, uint16(2))
	binary.Write(buf, binary.LittleEndian, uint16(16))
	binary.Write(buf, binary.LittleEndian, []byte("data"))
	binary.Write(buf, binary.LittleEndian, uint32(dataSize))
	for _, s := range samples {
		binary.Write(buf, binary.LittleEndian, s)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// CSV fixtures
// ---------------------------------------------------------------------------

// smallCSV is the docs/spec/03 scenario-2 "partial script": a handful of
// lines from an invented vocabulary, none of which is game text.
const smallCSV = `message_id,speaker,line,kind,area
m-001,Marla,"Welcome to the test harbour.",dialogue,Harbour
m-002,,"The crates are counted twice.",dialogue,Harbour
m-003,Corvin,"Take the long rope.",dialogue,Harbour
m-004,,"A gull lands and waits.",system,Harbour
m-005,Marla,"The tide is turning.",dialogue,Wreck
m-006,Corvin,"Ladder. Now.",dialogue,Wreck
`

// dupCSV is the scenario-8 fixture: the same line text at two locations.
const dupCSV = `message_id,speaker,line,kind,area
d-001,Marla,"The same words, in two places.",dialogue,Harbour
d-002,Corvin,"Different line, different place.",dialogue,Harbour
d-003,Marla,"The same words, in two places.",dialogue,Wreck
`

// genCSV builds n rows of invented dialogue.
func genCSV(n int) string {
	var b strings.Builder
	b.WriteString("message_id,speaker,line,kind,area\n")
	speakers := []string{"Marla", "Corvin", "Boy"}
	for i := 1; i <= n; i++ {
		b.WriteString(fmt.Sprintf("l-%04d,%s,\"Invented line number %d of %d.\",dialogue,Area%d\n",
			i, speakers[i%3], i, n, i%4))
	}
	return b.String()
}

// conflictCSVv1/v2 are the scenario-4/10 fixtures: same message ids, v2
// changes one line's text (a "source version mismatch").
const conflictCSVv1 = `message_id,speaker,line,kind,area
m-001,Marla,"The original reading stands.",dialogue,Harbour
m-002,Corvin,"Shared line, both versions agree.",dialogue,Harbour
`

const conflictCSVv2 = `message_id,speaker,line,kind,area
m-001,Marla,"A different reading from another source.",dialogue,Harbour
m-002,Corvin,"Shared line, both versions agree.",dialogue,Harbour
`

// profileioDeps aliases profileio's Deps so the test setup helper
// importDirect reads plainly.
type profileioDeps = profileio.Deps

// profileIODeps adapts the env's deps to profileio's own Deps type for the
// test setup step importDirect (not used by the tools under test).
func (d Deps) profileIODeps() profileioDeps {
	return profileioDeps{Store: d.Store, Assets: d.Assets, Adapters: d.Adapters}
}

// profileSnapshot is what "the rest of the profile" means in scenario 5 and
// 6: message/source/character counts, the row version and the approved
// voice. Anything here changing is a scenario failure.
type profileSnapshot struct {
	msgCount       int
	srcCount       int
	charCount      int
	rowVersion     int64
	preferredVoice string
}

func (e *env) profileSnapshot(t *testing.T, profileID string) profileSnapshot {
	t.Helper()
	content, err := e.store.GetProfileContent(e.ctx, profileID)
	if err != nil {
		t.Fatalf("GetProfileContent: %v", err)
	}
	return profileSnapshot{
		msgCount:       len(content.Messages),
		srcCount:       len(content.Sources),
		charCount:      len(content.Characters),
		rowVersion:     content.RowVersion,
		preferredVoice: content.Profile.PreferredVoiceID,
	}
}

// designVoiceAuditions returns the audition SHA-256s a run's design_voice
// call stored.
func (e *env) designVoiceAuditions(t *testing.T, runID string) []string {
	t.Helper()
	rows, err := e.agent.ListAgentToolCalls(e.ctx, runID)
	if err != nil {
		t.Fatalf("ListAgentToolCalls: %v", err)
	}
	var out []string
	for _, r := range rows {
		if r.Name != "design_voice" {
			continue
		}
		var res agent.Result
		if err := json.Unmarshal(r.ResultJSON, &res); err != nil {
			t.Fatalf("unmarshal design_voice result: %v", err)
		}
		var body struct {
			Auditions []struct {
				SHA256 string `json:"sha256"`
			} `json:"auditions"`
		}
		if err := json.Unmarshal(res.Results, &body); err != nil {
			t.Fatalf("unmarshal design_voice auditions: %v", err)
		}
		for _, a := range body.Auditions {
			out = append(out, a.SHA256)
		}
	}
	return out
}

// stringsReader wraps a string as an io.Reader (test setup convenience).
func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

// blobCount counts committed blobs (files under blobs/) in the asset store.
func blobCount(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(filepath.Join(root, "blobs"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("blobCount: %v", err)
	}
	return n
}

// toolFieldByCall returns the JSON value of key in the tool message
// answering callID, rendered as a string (strings pass through; arrays yield
// the first element; anything else is left empty). The tool result payload is
// compact JSON on its own line inside the [TOOL RESULT] block.
func toolFieldByCall(conv []openai.Message, callID, key string) string {
	for _, m := range conv {
		if m.Role != "tool" || m.ToolCallID != callID {
			continue
		}
		if v, ok := toolPayloadField(m, key); ok {
			return v
		}
	}
	return ""
}

// toolPayloadField extracts key from the JSON payload line of one tool
// message. Strings pass through; arrays yield the first element; objects
// fall back to their compact form. (ok, "") when the key is absent.
func toolPayloadField(m openai.Message, key string) (string, bool) {
	for _, line := range strings.Split(m.Text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		raw, ok := obj[key]
		if !ok {
			return "", false
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s, true
		}
		var arr []string
		if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
			return arr[0], true
		}
		return string(raw), true
	}
	return "", false
}

// hasToolMsg reports whether conv carries the tool result for callID.
func hasToolMsg(conv []openai.Message, callID string) bool {
	for _, m := range conv {
		if m.Role == "tool" && m.ToolCallID == callID {
			return true
		}
	}
	return false
}

// extractToolField walks the conversation backwards and returns the first
// JSON value of a tool message carrying key (tool results are rendered as
// JSON inside the [TOOL RESULT] block). Empty when absent.
func extractToolField(conv []openai.Message, key string) string {
	for i := len(conv) - 1; i >= 0; i-- {
		if conv[i].Role != "tool" {
			continue
		}
		if v, ok := toolPayloadField(conv[i], key); ok {
			return v
		}
	}
	return ""
}

// httptestServer returns a server for fetch tests.
func httptestServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}
