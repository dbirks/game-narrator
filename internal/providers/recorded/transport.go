// Package recorded provides the recorded-provider harness required by
// docs/spec/12: deterministic, redacted, replayable model-provider responses
// ("cassettes") implemented as an http.RoundTripper, so every HTTP provider
// adapter — the OpenAI chat client today, System One, vLLM-Omni TTS and ASR
// tomorrow — can be driven with recorded answers without a single line of
// per-adapter test scaffolding.
//
// Three modes, selected by GAME_NARRATOR_PROVIDER_MODE:
//
//	live    (default) no cassette involvement; responses are labelled "live"
//	record  real gateway, every interaction appended to <dir>/<role>.jsonl
//	replay  no network at all; responses come from the cassette or the call
//	        fails with a cassette_miss naming the request key digest
//
// The contract that makes this usable as evidence rather than theatre
// (docs/spec/11 Mode B, docs/spec/12 "No test may silently substitute canned
// model replies"): a replayed response is byte-identical to what was recorded,
// every response carries X-Game-Narrator-Origin so a report can attribute a
// stage to recorded vs live content, and a request with no recorded interaction
// is an error, never an invention.
package recorded

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Environment surface for provider modes. The mode is global on purpose:
// "which providers are real right now" must have one answer per process so a
// report cannot mix live and recorded stages by accident.
const (
	// EnvMode selects live|record|replay. Unset means live.
	EnvMode = "GAME_NARRATOR_PROVIDER_MODE"
	// EnvCassetteDir is where <role>.jsonl files are read from / written to.
	// Required for record and replay.
	EnvCassetteDir = "GAME_NARRATOR_CASSETTE_DIR"
	// HeaderOrigin labels every response with its provenance.
	HeaderOrigin = "X-Game-Narrator-Origin"
)

// Mode is a provider transport mode.
type Mode string

// The three modes.
const (
	ModeLive   Mode = "live"
	ModeRecord Mode = "record"
	ModeReplay Mode = "replay"
)

// ValidMode reports whether m is a known mode.
func ValidMode(m Mode) bool { return m == ModeLive || m == ModeRecord || m == ModeReplay }

// Origin values carried by HeaderOrigin.
const (
	OriginLive     = "live"
	OriginRecorded = "recorded"
	// OriginUnknown means something other than this harness produced the
	// response, which is always a bug in a replay run and never a silent pass.
	OriginUnknown = "unknown"
)

// ModeFromEnv resolves GAME_NARRATOR_PROVIDER_MODE and GAME_NARRATOR_CASSETTE_DIR.
// lookup may be nil, meaning os.LookupEnv (mirrors providers.LoadRoles so tests
// never mutate process state). A misconfigured mode or a missing cassette
// directory is a startup failure, not a fallback to live: an operator who asked
// for replay must never accidentally make real model calls.
func ModeFromEnv(lookup func(string) (string, bool)) (Mode, string, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	raw, _ := lookup(EnvMode)
	mode := Mode(strings.ToLower(strings.TrimSpace(raw)))
	if mode == "" {
		mode = ModeLive
	}
	if !ValidMode(mode) {
		return mode, "", fmt.Errorf("invalid %s=%q: expected one of live, record, replay", EnvMode, strings.TrimSpace(raw))
	}
	dir := strings.TrimSpace(orDefault(lookup, EnvCassetteDir, ""))
	if mode != ModeLive && dir == "" {
		return mode, "", fmt.Errorf("%s is required when %s=%s: point it at a directory of <role>.jsonl cassettes", EnvCassetteDir, EnvMode, mode)
	}
	if mode == ModeLive {
		// A live run with a cassette dir set is legal (recordings coexist);
		// the dir is simply unused.
		return ModeLive, dir, nil
	}
	return mode, dir, nil
}

func orDefault(lookup func(string) (string, bool), key, fallback string) string {
	if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

// Origin reports the provenance of an HTTP response: "recorded" when it was
// served from a cassette, "live" when it came from a real endpoint, "unknown"
// when nothing labelled it. Callers put this value in event traces and test
// reports so a stage is never ambiguous (docs/spec/12: "Reports explicitly
// record stage origins").
func Origin(resp *http.Response) string {
	if resp == nil {
		return OriginUnknown
	}
	return OriginFromHeader(resp.Header)
}

// OriginFromHeader is Origin for callers that only hold the header set, such as
// an adapter that has already drained and closed the response.
func OriginFromHeader(h http.Header) string {
	if h == nil {
		return OriginUnknown
	}
	v := strings.ToLower(strings.TrimSpace(h.Get(HeaderOrigin)))
	switch v {
	case OriginLive, OriginRecorded:
		return v
	default:
		return OriginUnknown
	}
}

// Options configures a Transport.
type Options struct {
	// Role names the cassette file and is written into every interaction. It
	// is the logical role, never an endpoint.
	Role providers.Role
	Mode Mode
	// Dir is the cassette directory; required for record and replay.
	Dir string
	// Base is the real transport used by live and record modes. Nil means
	// http.DefaultTransport. Replay never touches Base.
	Base http.RoundTripper
	// Secrets are active credentials (bearer tokens) that must not appear in a
	// cassette. Each serialised line is scanned for them and refused on match.
	Secrets []string
	// Now overrides the clock used for recorded_at; nil means time.Now. It
	// never affects latency measurement, which is always wall-clock.
	Now func() time.Time
	// LatencyMS transforms the measured elapsed time before it is stored; nil
	// stores it unchanged. The committed testdata cassette pins it so that
	// regenerating the fixture is byte-identical.
	LatencyMS func(elapsed time.Duration) int64
}

// Transport is an http.RoundTripper that records or replays interactions for
// one role. It is safe for concurrent use: appends are serialised and the
// replay table is loaded once under a mutex.
type Transport struct {
	Role    providers.Role
	Mode    Mode
	Dir     string
	Base    http.RoundTripper
	Secrets []string

	path      string
	now       func() time.Time
	latencyMS func(elapsed time.Duration) int64

	mu      sync.Mutex
	entries []*Interaction
	byKey   map[string][]*Interaction // request key digest -> interactions, recorded order
	cursors map[string]int            // request key digest -> next unconsumed entry
	loaded  bool

	hits   int64
	misses int64
	dials  int64
}

// New validates Options and returns the transport for that mode.
func New(opts Options) (*Transport, error) {
	mode := opts.Mode
	if mode == "" {
		mode = ModeLive
	}
	if !ValidMode(mode) {
		return nil, fmt.Errorf("recorded: unknown mode %q: expected live, record or replay", string(mode))
	}
	t := &Transport{
		Role:    opts.Role,
		Mode:    mode,
		Dir:     strings.TrimSpace(opts.Dir),
		Base:    opts.Base,
		Secrets: nonEmpty(opts.Secrets),
		now:     opts.Now,
	}
	if t.now == nil {
		t.now = time.Now
	}
	t.latencyMS = opts.LatencyMS
	if t.latencyMS == nil {
		t.latencyMS = func(d time.Duration) int64 { return d.Milliseconds() }
	}
	if t.Base == nil {
		t.Base = http.DefaultTransport
	}
	if mode != ModeLive {
		if t.Dir == "" {
			return nil, fmt.Errorf("recorded: %s is required in %s mode", EnvCassetteDir, mode)
		}
		path, err := roleFile(t.Dir, opts.Role)
		if err != nil {
			return nil, err
		}
		t.path = path
	}
	return t, nil
}

// FromEnv builds the transport for a role from GAME_NARRATOR_PROVIDER_MODE,
// GAME_NARRATOR_CASSETTE_DIR and the gateway credential resolved through the
// normal role configuration path, so the token that must never be written is
// the same one the client actually sends.
func FromEnv(role providers.Role, lookup func(string) (string, bool)) (*Transport, error) {
	mode, dir, err := ModeFromEnv(lookup)
	if err != nil {
		return nil, err
	}
	opts := Options{Role: role, Mode: mode, Dir: dir}
	if roles, err := providers.LoadRoles(lookup); err == nil {
		if cfg, ok := roles[role]; ok && cfg.Token() != "" {
			opts.Secrets = append(opts.Secrets, cfg.Token())
		}
	}
	return New(opts)
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Path is the cassette file this transport reads or writes ("" in live mode).
func (t *Transport) Path() string { return t.path }

// Stats reports hit/miss/dial counters for diagnostics and test assertions:
// a replay run that dials is a broken isolation, and Dials()==0 is how a test
// proves it.
func (t *Transport) Stats() (hits, misses, dials int64) {
	return atomic.LoadInt64(&t.hits), atomic.LoadInt64(&t.misses), atomic.LoadInt64(&t.dials)
}

// Client returns an *http.Client that uses this transport with no overall
// timeout, which is the shape provider adapters expect: the request context owns
// the deadline (docs/spec/06 "Timeout and cancellation defaults"). The
// redirect guard confines every hop to the original request's host (ADR-0068):
// in live and record mode this client dials real gateways, and a mutable
// gateway must not be able to move an evidence-capture request to another
// host.
func (t *Transport) Client() *http.Client {
	return &http.Client{Transport: t, CheckRedirect: providers.RedirectConfinedToOrigin}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("recorded: nil request")
	}
	// Live mode touches nothing: no body buffering, no cassette limits, just
	// the provenance header. Recording is opt-in per process, never a cost paid
	// by a normal run.
	if t.Mode == ModeLive {
		return t.live(req)
	}
	rawBody, err := drainBody(req)
	if err != nil {
		return nil, err
	}
	recorded, err := newRequest(req, rawBody)
	if err != nil {
		return nil, fmt.Errorf("recorded: cannot record request: %w", err)
	}
	if t.Mode == ModeRecord {
		return t.record(req, recorded, rawBody)
	}
	return t.replay(req, recorded)
}

// drainBody consumes and closes the request body (the RoundTripper contract)
// and returns its bytes.
func drainBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	defer req.Body.Close()
	if req.ContentLength > int64(maxRecordedRequestBytes) {
		return nil, fmt.Errorf("recorded: request body is %d bytes, over the %d byte cassette limit",
			req.ContentLength, maxRecordedRequestBytes)
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, int64(maxRecordedRequestBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("recorded: cannot read request body: %w", err)
	}
	if len(body) > maxRecordedRequestBytes {
		return nil, fmt.Errorf("recorded: request body exceeds the %d byte cassette limit", maxRecordedRequestBytes)
	}
	return body, nil
}

func (t *Transport) live(req *http.Request) (*http.Response, error) {
	atomic.AddInt64(&t.dials, 1)
	resp, err := t.Base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	label(resp, OriginLive)
	return resp, nil
}

func (t *Transport) record(req *http.Request, recorded Request, rawBody []byte) (*http.Response, error) {
	recordedAt := t.now().UTC()
	start := time.Now()
	atomic.AddInt64(&t.dials, 1)
	resp, err := t.Base.RoundTrip(withBody(req, rawBody))
	if err != nil {
		return nil, err
	}
	defer func() {
		if resp.Body != nil {
			resp.Body.Close()
		}
	}()
	var readErr error
	var body []byte
	if resp.Body != nil {
		body, readErr = io.ReadAll(io.LimitReader(resp.Body, maxRecordedResponseBytes+1))
	}
	latency := time.Since(start)
	if readErr != nil {
		return nil, fmt.Errorf("recorded: cannot read response body to record it: %w", readErr)
	}
	if len(body) > maxRecordedResponseBytes {
		return nil, fmt.Errorf("recorded: response body exceeds the %d byte cassette limit; the interaction is not recordable", maxRecordedResponseBytes)
	}
	if err := t.recordingSupported(recorded, resp, body); err != nil {
		return nil, err
	}
	label(resp, OriginLive)
	in := &Interaction{
		SchemaVersion: SchemaVersion,
		Role:          string(t.Role),
		Origin:        OriginRecorded,
		RecordedAt:    recordedAt,
		Request:       recorded,
		Response: Response{
			Status:  resp.StatusCode,
			Headers: filterResponseHeaders(resp.Header),
			Body:    string(body),
		},
		LatencyMS: t.latencyMS(latency),
	}
	if err := t.append(in); err != nil {
		return nil, err
	}
	// Hand the caller an equivalent response with the buffered body: the
	// original body is already consumed.
	rebuilt := in.httpResponse(req)
	rebuilt.Header.Set(HeaderOrigin, OriginLive)
	rebuilt.Status = resp.Status
	rebuilt.TLS = resp.TLS
	return rebuilt, nil
}

func (t *Transport) replay(req *http.Request, recorded Request) (*http.Response, error) {
	if err := t.load(); err != nil {
		return nil, err
	}
	key := recorded.KeyDigest()
	t.mu.Lock()
	var match *Interaction
	if queue := t.byKey[key]; queue != nil {
		if cursor := t.cursors[key]; cursor < len(queue) {
			match = queue[cursor]
			t.cursors[key] = cursor + 1
		}
	}
	t.mu.Unlock()
	if match == nil {
		atomic.AddInt64(&t.misses, 1)
		return nil, newMissError(t.Role, t.path, recorded)
	}
	if match.Role != string(t.Role) {
		// A cassette loaded from the wrong role file cannot answer for this
		// role; serving it anyway would attribute a response to the wrong
		// model.
		atomic.AddInt64(&t.misses, 1)
		return nil, newMissError(t.Role, t.path, recorded)
	}
	atomic.AddInt64(&t.hits, 1)
	return match.httpResponse(req), nil
}

// load reads the cassette once, in order, and prepares FIFO cursors. Multiple
// recorded interactions with the same key replay in recorded order and then
// miss, so a test cannot silently reuse a canned answer for a third call.
func (t *Transport) load() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded {
		return nil
	}
	entries, err := readCassette(t.path, t.Secrets)
	if err != nil {
		return err
	}
	t.entries = entries
	t.byKey = make(map[string][]*Interaction, len(entries))
	for _, in := range entries {
		t.byKey[in.Request.KeyDigest()] = append(t.byKey[in.Request.KeyDigest()], in)
	}
	t.cursors = make(map[string]int, len(entries))
	t.loaded = true
	return nil
}

func (t *Transport) append(in *Interaction) error {
	if err := in.validate(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return appendInteraction(t.path, in, t.Secrets)
}

func withBody(req *http.Request, raw []byte) *http.Request {
	fwd := req.Clone(req.Context())
	if raw == nil {
		fwd.Body = nil
		fwd.ContentLength = 0
		return fwd
	}
	fwd.Body = io.NopCloser(bytes.NewReader(raw))
	fwd.ContentLength = int64(len(raw))
	return fwd
}

func label(resp *http.Response, origin string) {
	if resp == nil {
		return
	}
	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	resp.Header.Set(HeaderOrigin, origin)
}

// recordingSupported rejects interactions a cassette cannot faithfully replay.
// Streaming is the known M0 limit and must fail loudly here rather than record
// a partial body that replays as a wrong answer (see ADR-0013 "Limits").
func (t *Transport) recordingSupported(req Request, resp *http.Response, body []byte) error {
	if streamingRequested(req.Body) {
		return t.notYetSupported(`a streaming request ("stream": true)`)
	}
	if resp != nil {
		if ct := strings.ToLower(resp.Header.Get("Content-Type")); strings.Contains(ct, "event-stream") || strings.Contains(ct, "ndjson") {
			return t.notYetSupported("a streaming response (" + ct + ")")
		}
	}
	if !isProbablyText(body) {
		return t.notYetSupported("a binary response body (audio or image content)")
	}
	return ensureNoDataURLs("response", body)
}

func streamingRequested(body []byte) bool {
	if len(body) == 0 || !json.Valid(body) {
		return false
	}
	var probe struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream != nil && *probe.Stream
}

func (t *Transport) notYetSupported(what string) error {
	err := providers.NewError(providers.KindUnsupportedCapability, 0,
		fmt.Sprintf("recorded: cassettes cannot store %s yet (recorded providers are buffered text/JSON only in M0)", what))
	err.Role = t.Role
	return err
}

// MissError is returned when a replay has no recorded interaction for a
// request. It wraps a *providers.Error classified as unsupported_capability —
// retrying a cassette miss cannot succeed — and names the request key digest so
// the exact interaction to record is recoverable from the failure message.
type MissError struct {
	Role      providers.Role
	Cassette  string
	Method    string
	Path      string
	KeyDigest string
	Inner     *providers.Error
}

func (e *MissError) Error() string { return e.Inner.Error() }

// Unwrap exposes the taxonomy error so errors.As(*providers.Error) and
// errors.Is(err, &providers.Error{Kind: providers.KindUnsupportedCapability})
// both match.
func (e *MissError) Unwrap() error { return e.Inner }

func newMissError(role providers.Role, path string, req Request) *MissError {
	key := req.KeyDigest()
	msg := fmt.Sprintf("cassette_miss: no recorded interaction for key %s (%s %s) in %s; "+
		"replay never invents a response — re-record the interaction or run with %s=live",
		key, strings.ToUpper(req.Method), req.Path, cassetteLabel(path), EnvMode)
	inner := providers.NewError(providers.KindUnsupportedCapability, 0, msg)
	inner.Role = role
	inner.Retryable = false
	return &MissError{Role: role, Cassette: path, Method: strings.ToUpper(req.Method), Path: req.Path, KeyDigest: key, Inner: inner}
}

func cassetteLabel(path string) string {
	if path == "" {
		return "(no cassette file configured)"
	}
	return path
}

// IsMiss reports whether an error is a cassette miss (including when it has
// been wrapped by a provider adapter).
func IsMiss(err error) bool {
	var miss *MissError
	return errors.As(err, &miss)
}

// MissDigest returns the request key digest a miss named, so a report can say
// which interaction is missing without reprinting (possibly sensitive) request
// contents.
func MissDigest(err error) (string, bool) {
	var miss *MissError
	if errors.As(err, &miss) && miss.KeyDigest != "" {
		return miss.KeyDigest, true
	}
	return "", false
}
