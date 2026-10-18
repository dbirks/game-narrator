package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Wire contract, verified live against the deployed checkpoints
// (2026-10-17, recorded in ADR-0022):
//
//   - VoiceDesign: POST /audio/speech {"model","input","instructions",
//     "task_type":"VoiceDesign","response_format":"wav"} → 200 audio/wav.
//     task_type is REQUIRED: without it the server assumes CustomVoice and
//     the VoiceDesign checkpoint answers 400 "does not support
//     task_type='CustomVoice'".
//   - Base: POST /audio/speech {"model","input","voice","response_format":
//     "wav"} → 200 audio/wav for a registered voice. Some deployments make
//     the Base checkpoint require task_type:"Base" explicitly; the client
//     negotiates that from the 400 text and remembers it.
//   - The response is the raw WAV; errors use the OpenAI envelope
//     {"error":{"message",...}}.

const (
	// DefaultUserAgent identifies requests in gateway logs.
	DefaultUserAgent = "game-narrator/0.0 m4-tts"
	// maxResponseBytes caps a single speech response body.
	maxResponseBytes = 32 << 20
	// taskTypeVoiceDesign / taskTypeBase are the wire values the checkpoints
	// understand (docs/spec/06, server openapi "task_type" field).
	taskTypeVoiceDesign = "VoiceDesign"
	taskTypeBase        = "Base"
)

// RequestIDSourceProvider and RequestIDSourceClient name the two origins a
// result's RequestID can have (RecordRequestID): the deployment's own
// request-id header, or the X-Request-Id the client minted and sent.
const (
	RequestIDSourceProvider = "provider"
	RequestIDSourceClient   = "client"
)

// Audio is one validated utterance plus its provenance. WAV is the raw file
// bytes as returned by the server (already validated by ValidateAudio).
// Model, Latency and RequestID carry the provider evidence the cache and the
// developer UI correlate on (docs/spec/07 "Cache", AGENTS.md evidence
// bundles). Origin records exactly which wire contract produced the audio
// (checkpoint, task type, voice), so a replay can never pass off one
// checkpoint's output for another's.
type Audio struct {
	WAV           []byte
	SampleRate    int
	Channels      int
	BitsPerSample int
	Duration      time.Duration
	Model         string
	Latency       time.Duration
	RequestID     string
	// RequestIDSource records which side issued RequestID ("provider" when
	// the deployment returned a request-id header, "client" when the id the
	// client minted and sent as X-Request-Id is all that exists — the
	// vLLM-Omni deployment, ADR-0050). Empty on values that never went
	// through the client's request path.
	RequestIDSource string
	Origin          string
	// Seed is the seed actually sent; it is nil when the caller set none or
	// when the server rejected it and the request was retried without it.
	// Seed is metadata, never speaker identity (ADR-0005).
	Seed *int
	// SeedUnsupported is true when this call's seed was rejected by the
	// server and the retry ran without it (recorded, not silently dropped).
	SeedUnsupported bool
}

// DesignRequest asks the VoiceDesign checkpoint for a reference clip.
// Text is the line to speak; Instructions is the voice description the
// checkpoint conditions on (timbre, cadence, temperament — docs/spec/07
// step 3). Language is the spoken language code (e.g. "English"); empty means
// server default. Seed is optional stochastic control, never identity.
type DesignRequest struct {
	Text         string
	Instructions string
	Language     string
	Seed         *int
}

// GenerateRequest asks the Base checkpoint to speak Text in a previously
// registered voice (or with an inline reference via GenerateWithReference).
// Speed 0 means the server default.
type GenerateRequest struct {
	Text     string
	Voice    string
	Language string
	Speed    float64
}

// Client talks to one deployment (usually a shared gateway) for one model.
// The client owns two negotiated capabilities that are probed lazily from
// the server's own 400 text and then cached: whether the checkpoint rejects
// the default (CustomVoice) task for Base synthesis, and whether an inline
// ref_audio reference is supported at all. Neither is assumed in either
// direction (docs/spec/06: "probe negotiated limits rather than assume
// upstream limits").
type Client struct {
	BaseURL string
	Token   string
	Model   string
	// HTTP is the transport. When nil a per-request context deadline is the
	// only bound (docs/spec/06 "Timeout and cancellation defaults" — the
	// router owns cancellation, not the client).
	HTTP *http.Client

	mu              sync.Mutex
	baseTaskType    bool // checkpoint demanded task_type:"Base" for synthesis
	seedUnsupported bool // server rejected the seed field once
	refAudioProbe   ReferenceProbe
	refAudioProbed  bool
	// Streaming negotiation (ADR-0057): the deployment's answer to the
	// first stream request decides which wire it serves; cached like the
	// task_type and ref_audio probes. streamNote keeps the deployment's own
	// refusal text for diagnostics (one line, truncated).
	streamUnsupported bool
	streamIgnored     bool
	streamServed      bool
	streamNote        string
}

// New returns a client for a base URL (the gateway or a dedicated endpoint,
// including its /v1 path). baseURL is validated, not silently repaired.
func New(baseURL, token, model string) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, providers.NewError(providers.KindUnsupportedCapability, 0, "tts: BaseURL is empty for model "+model)
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("tts: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("tts: base URL scheme %q is not supported, use http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("tts: base URL has no host")
	}
	if u.User != nil {
		return nil, errors.New("tts: credentials in the base URL are not allowed, use the token field")
	}
	return &Client{BaseURL: strings.TrimRight(u.String(), "/"), Token: token, Model: model}, nil
}

// NewFromRole builds a client for the voice_design or voice_clone role,
// rejecting roles that are not configured or do not speak the vLLM-Omni
// speech protocol.
func NewFromRole(cfg providers.RoleConfig) (*Client, error) {
	if cfg.Role != providers.RoleVoiceDesign && cfg.Role != providers.RoleVoiceClone {
		err := providers.NewError(providers.KindUnsupportedCapability, 0,
			"tts: role "+string(cfg.Role)+" is not a TTS role")
		err.Role = cfg.Role
		return nil, err
	}
	if !cfg.Configured() {
		err := providers.NewError(providers.KindUnsupportedCapability, 0, "role "+string(cfg.Role)+" is NOT CONFIGURED")
		err.Role = cfg.Role
		return nil, err
	}
	if cfg.Protocol != providers.ProtocolVLLMOmniSpeech {
		err := providers.NewError(providers.KindUnsupportedCapability, 0,
			fmt.Sprintf("role %s uses protocol %q, which has no TTS adapter in this build", cfg.Role, cfg.Protocol))
		err.Role = cfg.Role
		err.Model = cfg.Model
		return nil, err
	}
	c, err := New(cfg.BaseURL, cfg.Token(), cfg.Model)
	if err != nil {
		if pe := asProvidersError(err); pe != nil {
			pe.Role = cfg.Role
			pe.Model = cfg.Model
		}
		return nil, err
	}
	if cfg.Timeout > 0 {
		base := c.HTTP
		if base == nil {
			base = &http.Client{}
		}
		cloned := *base
		cloned.Timeout = cfg.Timeout
		c.HTTP = &cloned
	}
	return c, nil
}

// String renders the client identity for logs. The token is always masked and
// the base URL is hidden, because a gateway address is treated like a
// credential by AGENTS.md rule 5 and the task's logging contract.
func (c *Client) String() string {
	return fmt.Sprintf("tts.Client{BaseURL: <set>, Model: %q, Token: %q}", c.Model, mask(c.Token))
}

// LogValue keeps the same guarantee for structured logging.
func (c *Client) LogValue() any {
	return map[string]any{
		"base_url": "<set>",
		"model":    c.Model,
		"token":    mask(c.Token),
	}
}

func mask(secret string) string {
	if secret == "" {
		return ""
	}
	return providers.MaskValue
}

func asProvidersError(err error) *providers.Error {
	var pe *providers.Error
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{}
}

func (c *Client) endpoint(path string) string {
	return strings.TrimRight(c.BaseURL, "/") + path
}

// do issues one JSON request to /audio/speech and returns the raw response
// body for 2xx. Every failure mode is classified as *providers.Error; the
// raw body of error responses is discarded after the message is extracted
// (it can echo the request, including the token).
// do issues one JSON request to /audio/speech and returns the raw response
// body for 2xx, with the effective request id and its source. Every request
// carries a client-minted ULID as X-Request-Id: deployments that return no
// request-id header (the vLLM-Omni one, ADR-0050) must still be correlatable
// from provider-side logs by the id we sent. Every failure mode is
// classified as *providers.Error and carries the same effective id; the raw
// body of error responses is discarded after the message is extracted (it
// can echo the request, including the token).
func (c *Client) do(ctx context.Context, body map[string]any) ([]byte, time.Duration, string, string, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, 0, "", "", &providers.Error{Kind: providers.KindBadSchema, Msg: "tts: cannot encode request", Err: err, Model: c.Model}
	}
	clientID := newRequestID()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/audio/speech"), bytes.NewReader(buf))
	if err != nil {
		return nil, time.Since(start), "", "", c.transportError(err, 0, clientID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/wav, application/json")
	req.Header.Set("User-Agent", DefaultUserAgent)
	req.Header.Set("X-Request-Id", clientID)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, time.Since(start), "", "", c.transportError(err, 0, clientID)
	}
	defer func() { io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) }()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	latency := time.Since(start)
	reqID, reqIDSource := RecordRequestID(requestID(resp.Header), clientID)
	if readErr != nil {
		return nil, latency, reqID, reqIDSource, c.transportError(readErr, resp.StatusCode, reqID)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, latency, reqID, reqIDSource, c.httpError(resp.StatusCode, respBody, reqID)
	}
	return respBody, latency, reqID, reqIDSource, nil
}

// requestID reads the provider request id from the response headers so the
// developer UI can correlate the audio with provider-side logs.
func requestID(h http.Header) string {
	for _, k := range []string{"X-Request-Id", "Request-Id", "X-Amzn-Requestid", "X-Oneapi-Request-Id"} {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			return v
		}
	}
	return ""
}
