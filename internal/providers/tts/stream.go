package tts

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Streaming synthesis wire (measured 2026-10-18 against the qwen3-tts-base
// deployment on the shared gateway; ADR-0057). The request is the atomic
// synthesis payload plus "stream": true; the 200 answer is
// Content-Type: text/event-stream with no Content-Length. Frames:
//
//	event: speech.audio.delta
//	data: {"type":"speech.audio.delta","audio":"<base64 wav bytes>","response_format":"wav"}
//	<blank line>
//
// The first delta carries the RIFF header and fmt chunk (plus data as far as
// the deployment cut the frame); later deltas continue the data chunk, so
// the base64-decoded deltas concatenate into one complete WAV. The terminal
// frame is event: speech.audio.done carrying the usage accounting. The
// measured answer carries no request-id header, so the client-minted
// X-Request-Id remains the correlation identity (the ADR-0050 rule).
const (
	streamEventDelta = "speech.audio.delta"
	streamEventDone  = "speech.audio.done"
)

// ErrStreamUnsupported is the negotiated-capability marker: the deployment
// answered the stream request with a 400 about the stream flag, so
// GenerateStream falls back to the atomic Generate instead of retrying a
// request the server has said it cannot serve. The capability is cached on
// the client, like the Base task_type negotiation.
var ErrStreamUnsupported = errors.New("tts: deployment does not accept the stream flag")

// Stream is one in-flight streaming synthesis. Next delivers the
// base64-decoded audio deltas in wire order until the terminal
// speech.audio.done event (io.EOF). A failure mid-stream — the endpoint
// dropping the connection, a malformed frame, or the caller's context being
// cancelled — ends the stream with a classified error, and whatever partial
// bytes arrived are discarded by every caller contract: the atomicity the
// product owes is "a complete validated WAV or an error", and only the wire
// is streaming.
type Stream struct {
	client   *Client
	reqID    string
	reqIDSrc string
	// ctx is the caller's request context; Next checks it before every
	// delivery, not only when the next wire read fails, so a cancelled
	// synthesis is terminal even with bytes already buffered.
	ctx context.Context

	resp *http.Response
	r    *bufio.Reader
	// single is the one-shot body when the deployment ignored the stream
	// flag and answered the atomic wire (200 audio/wav): Next returns it
	// whole once, then io.EOF.
	single       []byte
	singleServed atomic.Bool
	done         bool
	bytes        atomic.Int64 // decoded audio bytes delivered via Next

	mu     sync.Mutex
	closed bool
	err    error // sticky terminal error
}

// OpenStream starts one streaming synthesis request and returns the in
// flight stream on a 200. It never returns partial audio: a non-200 is
// classified exactly as the atomic path classifies it, and a 400 about the
// stream flag is recorded as the negotiated capability (ErrStreamUnsupported
// wrapped in the classified error) so the caller can fall back to the
// atomic path.
func (c *Client) OpenStream(ctx context.Context, req GenerateRequest) (*Stream, error) {
	if strings.TrimSpace(req.Text) == "" {
		return nil, &providers.Error{Kind: providers.KindBadSchema, Msg: "tts: OpenStream.Text is empty", Model: c.Model}
	}
	if strings.TrimSpace(req.Voice) == "" {
		return nil, &providers.Error{Kind: providers.KindBadSchema, Msg: "tts: OpenStream.Voice is empty", Model: c.Model}
	}
	payload := c.synthesisPayload(req, c.baseTaskTypeCached())
	payload["stream"] = true
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &providers.Error{Kind: providers.KindBadSchema, Msg: "tts: cannot encode stream request", Err: err, Model: c.Model}
	}
	clientID := newRequestID()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/audio/speech"), bytes.NewReader(body))
	if err != nil {
		return nil, c.transportError(err, 0, clientID)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream, audio/wav, application/json")
	httpReq.Header.Set("User-Agent", DefaultUserAgent)
	httpReq.Header.Set("X-Request-Id", clientID)
	if c.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, c.transportError(err, 0, clientID)
	}
	reqID, reqIDSrc := RecordRequestID(requestID(resp.Header), clientID)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		e := c.httpError(resp.StatusCode, respBody, reqID)
		// The negotiation: a 400 naming the stream flag means this
		// deployment serves synthesis without streaming. Record it once
		// (the ADR-0022 probe pattern) and mark the error so GenerateStream
		// retries the atomic path instead of re-asking.
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Msg), "stream") {
			c.markStreamUnsupported(e.Msg)
			out := *e
			out.Kind = providers.KindUnsupportedCapability
			out.Retryable = false
			out.Err = errors.Join(out.Err, ErrStreamUnsupported)
			return nil, &out
		}
		return nil, e
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(ct), "text/event-stream") {
		c.markStreamServed()
		return &Stream{
			client:   c,
			reqID:    reqID,
			reqIDSrc: reqIDSrc,
			ctx:      ctx,
			resp:     resp,
			r:        bufio.NewReader(resp.Body),
		}, nil
	}
	// The deployment ignored the flag and answered the atomic wire: wrap
	// the complete body as a one-shot stream. The deployment's actual
	// behaviour is the negotiation, never an assumption.
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, c.transportError(readErr, resp.StatusCode, reqID)
	}
	c.markStreamIgnored()
	return &Stream{
		client:   c,
		reqID:    reqID,
		reqIDSrc: reqIDSrc,
		single:   respBody,
	}, nil
}

// GenerateStream synthesises one utterance over the streaming wire and
// returns the same provenance record the atomic Generate does: one complete
// validated WAV, or a classified error. The partial is never returned.
//
// Cancellation: the caller's context bounds the whole call; cancelling it
// mid-stream stops the read (the response body is bound to the request
// context) and Next surfaces a KindCancelled error after whatever bytes had
// already arrived. A fence that moves while the utterance is still
// synthesising is the caller's cancellation — the pipeline's mid-synthesis
// cancel is real when this path is in use (ADR-0057).
//
// Capability: when the deployment has negotiated itself as non-streaming
// (ErrStreamUnsupported), GenerateStream serves the call with the atomic
// Generate without re-asking; a deployment that ignores the flag is
// transparent (its 200 audio/wav answer is wrapped as a one-shot stream).
func (c *Client) GenerateStream(ctx context.Context, req GenerateRequest) (Audio, error) {
	if c.streamUnsupportedCached() {
		return c.Generate(ctx, req)
	}
	s, err := c.OpenStream(ctx, req)
	if err != nil {
		if errors.Is(err, ErrStreamUnsupported) {
			// Negotiated on this call: serve atomically.
			return c.Generate(ctx, req)
		}
		return Audio{}, c.classifyGenerateError(req, err)
	}
	defer s.Close()

	start := time.Now()
	var buf bytes.Buffer
	for {
		chunk, err := s.Next()
		if len(chunk) > 0 {
			buf.Write(chunk)
		}
		if err != nil {
			if IsCleanEnd(err) {
				break
			}
			// Partial discarded: the contract is the complete validated
			// WAV or an error, and this is the error.
			return Audio{}, err
		}
	}
	if buf.Len() == 0 {
		e := providers.NewError(providers.KindAudioInvalid, 0, "tts: stream completed with no audio")
		e.Model = c.Model
		e.RequestID = s.RequestID()
		return Audio{}, e
	}
	origin := fmt.Sprintf("voice_clone model=%s voice=%s stream=sse", c.Model, req.Voice)
	if c.streamIgnoredCached() {
		origin = fmt.Sprintf("voice_clone model=%s voice=%s stream=ignored", c.Model, req.Voice)
	}
	if c.baseTaskTypeCached() {
		origin += " task_type=" + taskTypeBase
	}
	return c.finishAudio(normalizeAssembledWAV(buf.Bytes()), time.Since(start), s.RequestID(), s.reqIDSrc, origin, nil, false)
}

// normalizeAssembledWAV repairs the streaming placeholder header the
// deployment ships on the measured wire (ADR-0057): the first delta is the
// WAV header whose RIFF size and data chunk length carry the 0xFFFFFFFF
// streaming markers (the RIFF convention for a file that arrives in chunks),
// and the following deltas are raw PCM. The assembled file is not valid until
// the lengths name the bytes actually received — a reader that refuses to
// trust a 4-gigabyte data chunk (this validator included) rejects it as sent.
// Bytes whose lengths already name the file are returned unchanged, so a
// deployment that ships final lengths (or a one-shot body wrapped as a
// single chunk) passes through untouched.
func normalizeAssembledWAV(b []byte) []byte {
	if len(b) < 12+16+8 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return b
	}
	offset := 12
	for offset+8 <= len(b) {
		size := int(binary.LittleEndian.Uint32(b[offset+4 : offset+8]))
		if string(b[offset:offset+4]) == "data" {
			actual := len(b) - (offset + 8)
			switch {
			case size == 0xFFFFFFFF:
				// The streaming marker: the data chunk runs to the end of the
				// file by definition.
			case offset+8+size > len(b):
				return b // malformed header: the validator names it
			case offset+8+size < len(b):
				return b // a chunk follows the data chunk: not this wire shape
			}
			if size == actual && int(binary.LittleEndian.Uint32(b[4:8])) == len(b)-8 {
				return b // lengths already name the assembled file
			}
			out := append([]byte(nil), b...)
			binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
			binary.LittleEndian.PutUint32(out[offset+4:offset+8], uint32(actual))
			return out
		}
		// Advance past a normal chunk; a placeholder or an overrun names a
		// header we cannot trust — leave it for the validator.
		if size == 0xFFFFFFFF || offset+8+size > len(b) {
			return b
		}
		offset += 8 + size
		if size%2 == 1 {
			offset++
		}
	}
	return b
}

// Next delivers the next decoded audio delta, or io.EOF at the terminal
// event. Cancellation and deadline expiry are terminal before the next
// delivery, even with deltas already buffered: the caller ended the
// synthesis, so a late chunk is not a delivery. A mid-stream failure is
// sticky: every later Next returns the same classified error.
func (s *Stream) Next() ([]byte, error) {
	if s.single != nil {
		if s.singleServed.Swap(true) {
			return nil, io.EOF
		}
		n := int64(len(s.single))
		s.bytes.Add(n)
		return s.single, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if s.done {
		return nil, io.EOF
	}
	// Cancellation is terminal even with bytes still buffered: deliver the
	// classified cancellation, never a late chunk (the buffered bytes count
	// toward the progress the error names).
	if err := s.ctx.Err(); err != nil {
		s.failLocked(s.client.streamReadError(err, s.BytesRead()))
		return nil, s.err
	}

	var eventName string
	var dataLines []string
	for {
		line, rerr := s.r.ReadString('\n')
		if rerr != nil {
			// The wire ended. A clean end is the blank line after the done
			// frame, handled by the dispatch below; getting here means the
			// connection died before the terminal event, or the caller
			// cancelled while the read was in flight.
			s.failLocked(s.client.streamReadError(rerr, s.BytesRead()))
			return nil, s.err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(line[len("data:"):]))
		case line == "":
			if eventName == "" && len(dataLines) == 0 {
				continue // stray blank line between frames
			}
			switch eventName {
			case streamEventDelta:
				raw, derr := decodeDeltaFrame(dataLines)
				if derr != nil {
					s.failLocked(derr)
					return nil, s.err
				}
				s.bytes.Add(int64(len(raw)))
				return raw, nil
			case streamEventDone:
				s.done = true
				return nil, io.EOF
			default:
				// An event the client does not act on (a ping, or a newer
				// protocol's extra frame): the frame boundary still
				// applies; ignore the frame, keep reading.
			}
			eventName, dataLines = "", nil
		default:
			// SSE comments (": ...") and unknown fields: ignored.
		}
	}
}

// decodeDeltaFrame parses one speech.audio.delta frame's data (the SSE spec
// joins multiple data lines with a newline) and base64-decodes the audio.
func decodeDeltaFrame(lines []string) ([]byte, error) {
	if len(lines) == 0 {
		return nil, providers.NewError(providers.KindDecodeError, 0, "tts: delta frame carries no data")
	}
	var d struct {
		Type  string `json:"type"`
		Audio string `json:"audio"`
	}
	if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &d); err != nil {
		return nil, providers.NewError(providers.KindDecodeError, 0, fmt.Sprintf("tts: delta frame is not JSON: %v", err))
	}
	raw, err := base64.StdEncoding.DecodeString(d.Audio)
	if err != nil {
		return nil, providers.NewError(providers.KindDecodeError, 0, fmt.Sprintf("tts: delta audio is not base64: %v", err))
	}
	if len(raw) == 0 {
		return nil, providers.NewError(providers.KindDecodeError, 0, "tts: delta frame carries no audio bytes")
	}
	return raw, nil
}

// failLocked records the terminal error; later Next calls keep returning it.
func (s *Stream) failLocked(err error) {
	if s.err == nil {
		s.err = err
		s.done = true // nothing more arrives; Next returns the error, not io.EOF
	}
}

// streamReadError classifies a failed read of the stream body: a caller
// cancellation is KindCancelled, a deadline is KindTimeout, and a
// connection that ends before the terminal event is the endpoint dying
// mid-synthesis (KindUnavailable, retryable) — the fault the failure
// injection "TTS partly streams before cancellation" exists to prove.
func (c *Client) streamReadError(err error, delivered int64) error {
	var e *providers.Error
	switch {
	case errors.Is(err, context.Canceled):
		e = providers.NewError(providers.KindCancelled, 0,
			fmt.Sprintf("tts: synthesis cancelled mid-stream after %d audio byte(s)", delivered))
	case errors.Is(err, context.DeadlineExceeded):
		e = providers.NewError(providers.KindTimeout, 0,
			fmt.Sprintf("tts: synthesis deadline exceeded mid-stream after %d audio byte(s)", delivered))
	default:
		e = providers.NewError(providers.KindUnavailable, 0,
			fmt.Sprintf("tts: stream ended before the terminal event after %d audio byte(s) (endpoint down mid-synthesis)", delivered))
		e.Retryable = true
	}
	e.Model = c.Model
	e.Err = err
	return e
}

// IsCleanEnd reports whether err is the stream's clean terminal — the
// speech.audio.done event reached. A mid-stream failure wraps the read
// error (which is io.EOF when the connection simply closed) through its
// cause, so callers must not test failures with errors.Is(err, io.EOF):
// use this check to decide between "the stream completed" and "the stream
// died".
func IsCleanEnd(err error) bool {
	var pe *providers.Error
	if errors.As(err, &pe) {
		return false
	}
	return errors.Is(err, io.EOF)
}

// RequestID is the effective request id: the deployment's answer header
// when it carries one, else the client-minted X-Request-Id (the measured
// deployment carries none).
func (s *Stream) RequestID() string { return s.reqID }

// RequestIDSource names where the effective id came from.
func (s *Stream) RequestIDSource() string { return s.reqIDSrc }

// BytesRead is the decoded audio byte count Next has delivered so far:
// the progress a cancellation or a mid-stream failure cut off.
func (s *Stream) BytesRead() int64 { return s.bytes.Load() }

// Close releases the response body. Safe to call more than once; a caller
// that abandons a stream without reading it to the end must call Close so
// the connection returns to the pool.
func (s *Stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.resp != nil {
		io.Copy(io.Discard, io.LimitReader(s.resp.Body, 1<<16))
		return s.resp.Body.Close()
	}
	return nil
}

// ── negotiated capability (probe once, never assume) ───────────────────────
//
// The deployment's own answer decides which wire it serves: an SSE answer
// proves streaming, a 200 audio/wav answer proves the flag is ignored, and
// a 400 naming the stream flag proves the deployment does not serve it.
// Like the Base task_type and ref_audio probes, the result is cached on the
// client after the first answer.

func (c *Client) markStreamUnsupported(note string) {
	c.mu.Lock()
	c.streamUnsupported = true
	if c.streamNote == "" {
		c.streamNote = oneLine(note, 200)
	}
	c.mu.Unlock()
}

func (c *Client) markStreamIgnored() {
	c.mu.Lock()
	c.streamIgnored = true
	c.mu.Unlock()
}

func (c *Client) markStreamServed() {
	c.mu.Lock()
	c.streamServed = true
	c.mu.Unlock()
}

func (c *Client) streamUnsupportedCached() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streamUnsupported
}

func (c *Client) streamIgnoredCached() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streamIgnored
}

// StreamCapability reports the negotiated streaming state for diagnostics:
// "unprobed" until the first stream request, then "sse" (the deployment
// served the streaming wire), "ignored" (it served the atomic wire), or
// "unsupported: <note>" (it refused the flag).
func (c *Client) StreamCapability() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.streamUnsupported:
		note := c.streamNote
		if note == "" {
			note = "flag rejected"
		}
		return "unsupported: " + note
	case c.streamIgnored:
		return "ignored"
	case c.streamServed:
		return "sse"
	default:
		return "unprobed"
	}
}
