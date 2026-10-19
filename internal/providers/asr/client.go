// Package asr is the typed client for the speech_asr role: the
// OpenAI-compatible POST /v1/audio/transcriptions endpoint (docs/spec/06
// "Provider registry", ADR 0061). It is the read-back half of the voice
// loop: the TTS side produces audio, and this client proves "the audio
// actually heard was the intended line" by transcribing it and reporting the
// divergence (docs/spec/07 "ASR loop", FR-10).
//
// The deployment's wire (verified against the collective gateway, 2026-10-18):
// multipart form with fields model, file (WAV bytes), response_format
// ("json" -> {"text"}; "verbose_json" -> {"task","language","duration","text"}),
// and optional language. It answers 200 with JSON and no request-id header,
// so the client mints a ULID and records its source exactly like the TTS
// client (internal/providers/tts/requestid.go).
package asr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// ResponseFormat is the transcription response shape the deployment serves.
type ResponseFormat string

const (
	// FormatJSON answers {"text"}.
	FormatJSON ResponseFormat = "json"
	// FormatVerboseJSON answers {"task","language","duration","text"}.
	FormatVerboseJSON ResponseFormat = "verbose_json"
)

// DefaultUserAgent identifies requests in gateway logs.
const DefaultUserAgent = "game-narrator/0.0 m4-asr"

// maxResponseBytes caps a single transcription response body.
const maxResponseBytes = 4 << 20

// TranscribeRequest is one utterance to transcribe.
type TranscribeRequest struct {
	// WAV is the audio bytes (RIFF/WAVE). Empty is a client-side refusal.
	WAV []byte
	// Language hints the language (e.g. "en"); empty lets the model detect.
	Language string
	// ResponseFormat selects the answer shape; empty defaults to
	// FormatVerboseJSON (it carries the duration QA reports).
	ResponseFormat ResponseFormat
	// Filename is the multipart file part's name (diagnostic only; the
	// endpoint keys on the bytes). Empty uses "audio.wav".
	Filename string
}

// TranscribeResult is one completed transcription.
type TranscribeResult struct {
	// Text is the recognised transcript, verbatim from the endpoint.
	Text string
	// Language is the endpoint's language identification (verbose_json).
	Language string
	// DurationS is the endpoint's measured audio duration in seconds
	// (verbose_json); 0 when the shape does not carry it.
	DurationS float64
	// Model is the model the request named (the client's own).
	Model string
	// RequestID is the effective correlation id and its source, as
	// resolved by RecordRequestID.
	RequestID       string
	RequestIDSource string
	// Latency is the round trip.
	Latency time.Duration
}

// Client speaks the OpenAI-compatible transcription wire to one base URL.
type Client struct {
	BaseURL string
	Token   string
	Model   string
	// HTTP bounds a single request; nil uses a bare client. The caller's
	// context still bounds the work (the router owns cancellation, not the
	// client).
	HTTP *http.Client
}

// New returns a client for a base URL (the gateway or a dedicated endpoint,
// including its /v1 path). baseURL is validated, not silently repaired.
func New(baseURL, token, model string) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, providers.NewError(providers.KindUnsupportedCapability, 0, "asr: BaseURL is empty for model "+model)
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("asr: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("asr: base URL scheme %q is not supported, use http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("asr: base URL has no host")
	}
	if u.User != nil {
		return nil, errors.New("asr: credentials in the base URL are not allowed, use the token field")
	}
	c := &Client{BaseURL: strings.TrimRight(u.String(), "/"), Token: token, Model: model}
	// Redirect guard (spec-14, ADR-0068): a mutable gateway must not be able
	// to move an audio upload (and its bearer token) to another host.
	c.HTTP = &http.Client{CheckRedirect: providers.SafeRedirect(u.String())}
	return c, nil
}

// NewFromRole builds a client for the speech_asr role, rejecting roles that
// are not configured or do not speak the transcription protocol.
func NewFromRole(cfg providers.RoleConfig) (*Client, error) {
	if cfg.Role != providers.RoleSpeechASR {
		err := providers.NewError(providers.KindUnsupportedCapability, 0,
			"asr: role "+string(cfg.Role)+" is not the speech_asr role")
		err.Role = cfg.Role
		return nil, err
	}
	if !cfg.Configured() {
		err := providers.NewError(providers.KindUnsupportedCapability, 0, "role "+string(cfg.Role)+" is NOT CONFIGURED")
		err.Role = cfg.Role
		return nil, err
	}
	if cfg.Protocol != providers.ProtocolOpenAITranscriptions {
		err := providers.NewError(providers.KindUnsupportedCapability, 0,
			fmt.Sprintf("role %s uses protocol %q, which has no ASR adapter in this build", cfg.Role, cfg.Protocol))
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

// Transcribe issues one transcription request and returns the recognised
// text. Every failure mode is classified as *providers.Error and carries the
// effective request id (AGENTS.md rule 3: the evidence bundle needs the id on
// every clip). The request is idempotent (a pure function of the bytes), so
// callers MAY retry it on the retryable kinds; this client itself never
// retries — the bounded retry policy belongs to the caller
// (docs/spec/13 "bounded autonomous troubleshooting").
func (c *Client) Transcribe(ctx context.Context, req TranscribeRequest) (TranscribeResult, error) {
	if len(req.WAV) == 0 {
		return TranscribeResult{}, providers.NewError(providers.KindBadSchema, 0, "asr: empty audio")
	}
	format := req.ResponseFormat
	if format == "" {
		format = FormatVerboseJSON
	}
	fname := req.Filename
	if fname == "" {
		fname = "audio.wav"
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := writeForm(mw, "model", c.Model); err != nil {
		return TranscribeResult{}, err
	}
	if err := writeForm(mw, "response_format", string(format)); err != nil {
		return TranscribeResult{}, err
	}
	if lang := strings.TrimSpace(req.Language); lang != "" {
		if err := writeForm(mw, "language", lang); err != nil {
			return TranscribeResult{}, err
		}
	}
	fw, err := mw.CreateFormFile("file", fname)
	if err != nil {
		return TranscribeResult{}, providers.NewError(providers.KindBadSchema, 0, "asr: building multipart: "+err.Error())
	}
	if _, err := fw.Write(req.WAV); err != nil {
		return TranscribeResult{}, providers.NewError(providers.KindBadSchema, 0, "asr: writing audio: "+err.Error())
	}
	if err := mw.Close(); err != nil {
		return TranscribeResult{}, providers.NewError(providers.KindBadSchema, 0, "asr: closing multipart: "+err.Error())
	}

	clientID := newRequestID()
	start := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/audio/transcriptions"), &body)
	if err != nil {
		return TranscribeResult{}, c.transportError(err, 0, clientID)
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", DefaultUserAgent)
	httpReq.Header.Set("X-Request-Id", clientID)
	if c.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return TranscribeResult{}, c.transportError(err, 0, clientID)
	}
	defer func() { io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) }()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	latency := time.Since(start)
	reqID, reqIDSource := RecordRequestID(requestID(resp.Header), clientID)
	if readErr != nil {
		return TranscribeResult{}, c.transportError(readErr, resp.StatusCode, reqID)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return TranscribeResult{}, c.httpError(resp.StatusCode, respBody, reqID)
	}

	var parsed struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
		// Task is a pointer so "present but empty" and "absent" are distinct:
		// the deployed verbose_json response carries `task: "transcribe"` in
		// both the speech and no-speech cases (measured 2026-10-18), so a 200
		// whose body omits it is schema drift (a different service answering),
		// not a valid empty transcript — it must fail as a decode error.
		// The plain `json` shape (FormatJSON) legitimately omits it, so the
		// check applies only to the verbose_json the client requests by
		// default.
		Task *string `json:"task"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		e := providers.NewError(providers.KindDecodeError, resp.StatusCode, "asr: decoding transcription: "+err.Error())
		e.Model = c.Model
		e.RequestID = reqID
		return TranscribeResult{}, e
	}
	if parsed.Task == nil && format == FormatVerboseJSON {
		e := providers.NewError(providers.KindDecodeError, resp.StatusCode,
			"asr: response is not a transcription object (no `task` field)")
		e.Model = c.Model
		e.RequestID = reqID
		return TranscribeResult{}, e
	}
	// An empty `text` is a VALID result — the endpoint saying "no speech in
	// this audio" — not a wire failure (the deployed qwen3-asr-1-7b
	// transcribes the doctor's probe tone to exactly this, measured
	// 2026-10-18). The client stays a faithful wire adapter; the POLICY for
	// an empty transcript belongs to the caller: the QA loop scores it WER
	// 1.0 (a line expected to carry speech that comes back silent regenerates
	// once and is then flagged with the evidence — docs/spec/07: silent
	// audio is a QA signal), and the doctor probe treats it as a verified
	// route. A 200 with a non-transcription body still fails as a decode
	// error above.
	return TranscribeResult{
		Text:            parsed.Text,
		Language:        parsed.Language,
		DurationS:       parsed.Duration,
		Model:           c.Model,
		RequestID:       reqID,
		RequestIDSource: reqIDSource,
		Latency:         latency,
	}, nil
}

// endpoint joins the base URL and route.
func (c *Client) endpoint(path string) string {
	return strings.TrimRight(c.BaseURL, "/") + path
}

// httpClient returns the request client, honouring the role's timeout.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// Hand-built clients (tests, tooling) still get the redirect guard.
	return &http.Client{CheckRedirect: providers.SafeRedirect(c.BaseURL)}
}

// writeForm writes one text field to the multipart writer.
func writeForm(mw *multipart.Writer, key, value string) error {
	f, err := mw.CreateFormField(key)
	if err != nil {
		return providers.NewError(providers.KindBadSchema, 0, "asr: building multipart: "+err.Error())
	}
	if _, err := f.Write([]byte(value)); err != nil {
		return providers.NewError(providers.KindBadSchema, 0, "asr: writing field: "+err.Error())
	}
	return nil
}

// requestID reads the provider request id from the response headers so the
// developer UI can correlate the clip with provider-side logs.
func requestID(h http.Header) string {
	for _, k := range []string{"X-Request-Id", "Request-Id", "X-Amzn-Requestid", "X-Oneapi-Request-Id"} {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

// asProvidersError unwraps a *providers.Error if present.
func asProvidersError(err error) *providers.Error {
	var pe *providers.Error
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

// String renders the client identity for logs. The token is always masked and
// the base URL is hidden (AGENTS.md rule 5).
func (c *Client) String() string {
	return fmt.Sprintf("asr.Client{BaseURL: <set>, Model: %q, Token: %q}", c.Model, mask(c.Token))
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
