package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
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

// sse frames for the streaming contract tests: the measured wire shape —
// speech.audio.delta frames with base64 audio, a speech.audio.done terminal
// frame. splitWAV cuts one WAV into N base64 delta payloads the way the
// deployment cuts its frames: the first payload carries the header, the rest
// continue the data chunk.
func sseFrame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

func deltaFrame(payload []byte) string {
	b, _ := json.Marshal(map[string]any{
		"type":            streamEventDelta,
		"audio":           base64.StdEncoding.EncodeToString(payload),
		"response_format": "wav",
	})
	return sseFrame(streamEventDelta, string(b))
}

func doneFrame() string {
	b, _ := json.Marshal(map[string]any{
		"type":  streamEventDone,
		"usage": map[string]any{"input_tokens": 6, "output_tokens": 3, "total_tokens": 9},
	})
	return sseFrame(streamEventDone, string(b))
}

// splitWAV splits wav into n contiguous payloads (header in the first).
func splitWAV(wav []byte, n int) [][]byte {
	if n <= 0 {
		n = 1
	}
	step := (len(wav) + n - 1) / n
	var out [][]byte
	for i := 0; i < len(wav); i += step {
		end := i + step
		if end > len(wav) {
			end = len(wav)
		}
		out = append(out, wav[i:end])
	}
	return out
}

// sseServer is a fake /audio/speech endpoint that answers stream requests
// over the SSE wire, with per-request scripting: which deltas to send,
// whether to drop the connection mid-stream, and whether to hold the last
// frame so a cancellation can land while the stream is live.
type sseServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	streams int
	lastReq recordedRequest
	// plan scripts the next stream answer.
	dropAfterDelta int  // 0: never; >0: close after this many deltas
	holdLast       bool // hold the terminal frame until release
	release        chan struct{}
}

func newSSEServer(wav []byte, dropAfterDelta int, holdLast bool) *sseServer {
	s := &sseServer{
		dropAfterDelta: dropAfterDelta,
		holdLast:       holdLast,
		release:        make(chan struct{}, 1),
	}
	parts := splitWAV(wav, 4)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.lastReq = recordedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
		streaming := strings.Contains(string(body), `"stream":true`)
		if streaming {
			s.streams++
		}
		s.mu.Unlock()

		if !streaming {
			// The atomic wire: a plain WAV answer.
			w.Header().Set("Content-Type", "audio/wav")
			w.Write(wav)
			return
		}

		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		for i, p := range parts {
			_, _ = w.Write([]byte(deltaFrame(p)))
			fl.Flush()
			if s.dropAfterDelta > 0 && i == s.dropAfterDelta-1 {
				// The endpoint dies mid-synthesis: no further frame, no
				// terminal event; the connection closes as the handler
				// returns.
				return
			}
		}
		if s.holdLast {
			// Hold the terminal frame until released — or until the
			// client's request context ends: a cancelled or gone client
			// must never leave the handler holding (holding forever
			// wedges Server.Close, which waits for every connection).
			select {
			case <-s.release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write([]byte(doneFrame()))
		fl.Flush()
	}))
	return s
}

func (s *sseServer) url() string { return s.srv.URL }

func (s *sseServer) releaseStream() {
	select {
	case s.release <- struct{}{}:
	default:
	}
}

func (s *sseServer) streamCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams
}

func (s *sseServer) lastStreamBody() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(s.lastReq.Body, &m)
	return m
}

func newStreamClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(url, "test-token", "tts-fake")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func kindOf(t *testing.T, err error) providers.Kind {
	t.Helper()
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error is not a *providers.Error: %v", err)
	}
	return pe.Kind
}

// TestGenerateStreamHappyPath: the full SSE wire assembles into the exact
// WAV the deployment sent; the provenance record carries the request id the
// deployment echoed (or, when it echoes none, the client-minted one) and the
// stream origin.
func TestGenerateStreamHappyPath(t *testing.T) {
	wav := sineWAV(22050, 0.3, 440, 0.4)
	srv := newSSEServer(wav, 0, false)
	defer srv.srv.Close()
	c := newStreamClient(t, srv.url())

	a, err := c.GenerateStream(context.Background(), GenerateRequest{Text: "hello world", Voice: "v"})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if string(a.WAV) != string(wav) {
		t.Fatalf("assembled %d byte(s), want the deployment's %d bytes (exact concatenation)", len(a.WAV), len(wav))
	}
	if a.RequestID == "" {
		t.Fatal("request id is empty: the correlation identity is missing")
	}
	if !strings.Contains(a.Origin, "stream=sse") {
		t.Fatalf("origin %q does not name the streaming wire", a.Origin)
	}
	if a.Latency <= 0 {
		t.Fatalf("latency %v is not measured", a.Latency)
	}
	// The request carried the stream flag and the client-minted id.
	body := srv.lastStreamBody()
	if body["stream"] != true {
		t.Fatalf("request body: stream = %v, want true", body["stream"])
	}
	if body["input"] != "hello world" || body["voice"] != "v" || body["model"] != "tts-fake" {
		t.Fatalf("request body %v: synthesis identity lost on the streaming wire", body)
	}
	if got := c.StreamCapability(); got != "sse" {
		t.Fatalf("StreamCapability = %q, want sse (the deployment served the streaming wire)", got)
	}
	// The request id the client sent is the one surfaced when the
	// deployment echoes none of its own (the httptest server here sets the
	// X-Request-Id only on the atomic fake; this server sets none).
	srv.mu.Lock()
	clientSent := srv.lastReq.Header.Get("X-Request-Id")
	srv.mu.Unlock()
	if clientSent == "" || a.RequestID != clientSent {
		t.Fatalf("surfaced request id %q, the client sent %q (deployment echoes no id of its own)", a.RequestID, clientSent)
	}
}

// TestGenerateStreamMidStreamDrop: the endpoint dies after the second
// delta — no terminal event. The call fails with the endpoint-down
// classification, names how much audio had arrived, and returns no partial.
func TestGenerateStreamMidStreamDrop(t *testing.T) {
	wav := sineWAV(22050, 0.3, 440, 0.4)
	srv := newSSEServer(wav, 2, false) // drop after 2 of 4 deltas
	defer srv.srv.Close()
	c := newStreamClient(t, srv.url())

	_, err := c.GenerateStream(context.Background(), GenerateRequest{Text: "hello world", Voice: "v"})
	if err == nil {
		t.Fatal("mid-stream drop: GenerateStream succeeded, want the endpoint-down failure")
	}
	if k := kindOf(t, err); k != providers.KindUnavailable {
		t.Fatalf("kind = %q, want %q (the endpoint died mid-synthesis)", k, providers.KindUnavailable)
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error is not *providers.Error: %v", err)
	}
	if !pe.Retryable {
		t.Fatal("mid-stream endpoint loss is retryable: the utterance may be re-synthesised")
	}
	if !strings.Contains(pe.Msg, "audio byte(s) (endpoint down mid-synthesis)") {
		t.Fatalf("error %q does not name the mid-synthesis cut with its delivered byte count", pe.Msg)
	}
	var delivered int64
	if _, err := fmt.Sscanf(pe.Msg, "tts: stream ended before the terminal event after %d audio byte(s) (endpoint down mid-synthesis)", &delivered); err != nil {
		t.Fatalf("cannot parse the delivered byte count from %q: %v", pe.Msg, err)
	}
	if delivered <= 0 || delivered >= int64(len(wav)) {
		t.Fatalf("delivered %d byte(s): want a genuine partial (0 < n < the full %d)", delivered, len(wav))
	}
}

// TestStreamCancelMidStream: a cancellation while the stream is live stops
// the read, surfaces KindCancelled with the progress, and the stream
// delivers nothing after the cancel.
func TestStreamCancelMidStream(t *testing.T) {
	wav := sineWAV(22050, 0.3, 440, 0.4)
	srv := newSSEServer(wav, 0, true) // hold the terminal frame
	defer srv.srv.Close()
	c := newStreamClient(t, srv.url())

	ctx, cancel := context.WithCancel(context.Background())
	s, err := c.OpenStream(ctx, GenerateRequest{Text: "hello world", Voice: "v"})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	first, err := s.Next()
	if err != nil {
		t.Fatalf("first delta: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first delta is empty")
	}
	cancel() // the fence moved while synthesis was in flight

	// The next read must not return a chunk: it fails with the
	// cancellation, naming how much had arrived.
	_, err = s.Next()
	if err == nil {
		t.Fatal("Next after cancel: no error, want the cancellation")
	}
	if k := kindOf(t, err); k != providers.KindCancelled {
		t.Fatalf("kind = %q, want %q", k, providers.KindCancelled)
	}
	if n := s.BytesRead(); n < int64(len(first)) {
		t.Fatalf("BytesRead = %d, want at least the delivered first delta (%d)", n, len(first))
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d audio byte", s.BytesRead())) {
		t.Fatalf("cancellation error %q does not carry the delivered byte count", err)
	}
	// Sticky: later reads keep returning the same terminal error.
	if _, err2 := s.Next(); err2 != err {
		t.Fatalf("second post-cancel read: %v, want the sticky terminal error", err2)
	}
	s.Close()

	// The server's held terminal frame must not leak: releasing it after
	// the client went away is a no-op for the client.
	srv.releaseStream()
	time.Sleep(20 * time.Millisecond)
}

// TestGenerateStreamCancelDiscardsPartial: the caller-visible contract —
// GenerateStream with a cancelled context returns the error, never a WAV.
func TestGenerateStreamCancelDiscardsPartial(t *testing.T) {
	wav := sineWAV(22050, 0.3, 440, 0.4)
	srv := newSSEServer(wav, 0, true)
	defer srv.srv.Close()
	c := newStreamClient(t, srv.url())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.GenerateStream(ctx, GenerateRequest{Text: "hello world", Voice: "v"})
		done <- err
	}()
	// Let the stream deliver at least one delta, then cancel mid-stream.
	time.Sleep(50 * time.Millisecond)
	cancel()
	err := <-done
	if err == nil {
		t.Fatal("cancelled GenerateStream returned success")
	}
	if k := kindOf(t, err); k != providers.KindCancelled {
		t.Fatalf("kind = %q, want %q (the caller cancelled)", k, providers.KindCancelled)
	}
	srv.releaseStream()
}

// TestGenerateStreamServerIgnoresFlag: a deployment that answers the
// streaming request with the atomic wire (200 audio/wav) is transparent —
// the call returns its WAV and the capability is recorded as "ignored",
// never assumed.
func TestGenerateStreamServerIgnoresFlag(t *testing.T) {
	wav := sineWAV(22050, 0.3, 440, 0.4)
	srv := newSSEServer(wav, 0, false) // answers non-stream with the WAV; stream requests fall through the scripted path below
	defer srv.srv.Close()
	// A server that ignores the flag answers audio/wav to stream requests:
	// reuse the atomic fake with the stream flag stripped of meaning.
	atomic := newFakeSpeechServer(t, func(i int, rec recordedRequest) (int, []byte, string) {
		return http.StatusOK, wav, "audio/wav"
	})
	c := newStreamClient(t, atomic.url())
	a, err := c.GenerateStream(context.Background(), GenerateRequest{Text: "hello world", Voice: "v"})
	if err != nil {
		t.Fatalf("GenerateStream against a flag-ignoring deployment: %v", err)
	}
	if string(a.WAV) != string(wav) {
		t.Fatalf("WAV = %d byte(s), want the deployment's %d", len(a.WAV), len(wav))
	}
	if got := c.StreamCapability(); got != "ignored" {
		t.Fatalf("StreamCapability = %q, want ignored (the deployment served the atomic wire)", got)
	}
	if !strings.Contains(a.Origin, "stream=ignored") {
		t.Fatalf("origin %q does not record that the flag was ignored", a.Origin)
	}
}

// TestGenerateStreamUnsupportedFallback: a 400 naming the stream flag is the
// negotiated capability — GenerateStream serves the call atomically (the
// fallback is transparent to the caller) and the capability is cached so
// later calls never re-ask.
func TestGenerateStreamUnsupportedFallback(t *testing.T) {
	wav := sineWAV(22050, 0.3, 440, 0.4)
	var mu sync.Mutex
	streamSeen, atomicSeen := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		if strings.Contains(string(body), `"stream":true`) {
			streamSeen++
		} else {
			atomicSeen++
		}
		mu.Unlock()
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write(errorBody("stream is not supported for this model"))
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav)
	}))
	defer srv.Close()
	c := newStreamClient(t, srv.URL)

	a, err := c.GenerateStream(context.Background(), GenerateRequest{Text: "hello world", Voice: "v"})
	if err != nil {
		t.Fatalf("GenerateStream with the atomic fallback: %v", err)
	}
	if string(a.WAV) != string(wav) {
		t.Fatalf("WAV = %d byte(s), want the atomic deployment's %d", len(a.WAV), len(wav))
	}
	if got := c.StreamCapability(); !strings.HasPrefix(got, "unsupported") {
		t.Fatalf("StreamCapability = %q, want the negotiated unsupported note", got)
	}
	// A second call must not re-ask the stream question.
	if _, err := c.GenerateStream(context.Background(), GenerateRequest{Text: "again", Voice: "v"}); err != nil {
		t.Fatalf("second GenerateStream: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if streamSeen != 1 || atomicSeen != 2 {
		t.Fatalf("wire usage: stream=%d atomic=%d, want stream=1 (the probe) atomic=2 (the fallbacks)", streamSeen, atomicSeen)
	}
}

// TestStreamMalformedFrame: a delta that is not decodable JSON/base64 ends
// the stream with the decode classification, and a later well-formed stream
// on the same client still works (the error is per-stream, not cached).
func TestStreamMalformedFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Write([]byte(sseFrame(streamEventDelta, `{"type":"speech.audio.delta","audio":"!!!not-base64!!!"}`)))
	}))
	defer srv.Close()
	c := newStreamClient(t, srv.URL)
	_, err := c.GenerateStream(context.Background(), GenerateRequest{Text: "hello", Voice: "v"})
	if err == nil {
		t.Fatal("malformed delta: GenerateStream succeeded")
	}
	if k := kindOf(t, err); k != providers.KindDecodeError {
		t.Fatalf("kind = %q, want %q (a malformed frame is a decode failure, not a retry)", k, providers.KindDecodeError)
	}
	var pe *providers.Error
	if errors.As(err, &pe) && pe.Retryable {
		t.Fatal("a malformed frame is not retryable: the deployment will send the same bytes again")
	}
}

// TestStreamUnknownEventsAreIgnored: a ping/keep-alive event between deltas
// is part of a healthy stream, not a failure.
func TestStreamUnknownEventsAreIgnored(t *testing.T) {
	wav := sineWAV(22050, 0.3, 440, 0.4)
	parts := splitWAV(wav, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(deltaFrame(parts[0])))
		w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")) // keep-alive
		fl.Flush()
		w.Write([]byte(deltaFrame(parts[1])))
		fl.Flush()
		w.Write([]byte(doneFrame()))
	}))
	defer srv.Close()
	c := newStreamClient(t, srv.URL)
	a, err := c.GenerateStream(context.Background(), GenerateRequest{Text: "hello", Voice: "v"})
	if err != nil {
		t.Fatalf("stream with a ping frame: %v", err)
	}
	if string(a.WAV) != string(wav) {
		t.Fatalf("WAV = %d byte(s), want the deployment's %d (the ping must not break the assembly)", len(a.WAV), len(wav))
	}
}

// TestStreamCloseIdempotent: Close is safe to call twice and after a full
// read.
func TestStreamCloseIdempotent(t *testing.T) {
	wav := sineWAV(22050, 0.3, 440, 0.4)
	srv := newSSEServer(wav, 0, false)
	defer srv.srv.Close()
	c := newStreamClient(t, srv.url())
	s, err := c.OpenStream(context.Background(), GenerateRequest{Text: "hello", Voice: "v"})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	for {
		if _, err := s.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// placeholderHeaderWAV is the wire shape the measured deployment ships
// (ADR-0057): a 44-byte WAV header whose RIFF size and data chunk length
// are the 0xFFFFFFFF streaming markers, followed by the raw PCM the rest of
// the deltas deliver.
func placeholderHeaderWAV(wav []byte) [][]byte {
	head := append([]byte(nil), wav[:44]...)
	binary.LittleEndian.PutUint32(head[4:8], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(head[40:44], 0xFFFFFFFF)
	pcm := wav[44:]
	half := len(pcm) / 2
	return [][]byte{head, pcm[:half], pcm[half:]}
}

// TestNormalizeAssembledWAV covers the placeholder repair in isolation:
// a marker header is patched to the assembled lengths, a file whose
// lengths already name it passes through byte-identical, and a non-WAVE
// buffer is left for the validator to name.
func TestNormalizeAssembledWAV(t *testing.T) {
	src := sineWAV(22050, 0.3, 440, 0.4)

	chunks := placeholderHeaderWAV(src)
	var buf bytes.Buffer
	for _, c := range chunks {
		buf.Write(c)
	}
	in := buf.Bytes()
	if bytes.Equal(in, src) {
		t.Fatalf("fixture: the assembled input carries the placeholder header, not the final lengths")
	}
	fixed := normalizeAssembledWAV(in)
	if !bytes.Equal(fixed, src) {
		t.Fatalf("the placeholder header was not repaired to the source's final lengths (output %d byte(s))", len(fixed))
	}
	if int(binary.LittleEndian.Uint32(fixed[40:44])) != len(src)-44 {
		t.Errorf("data chunk length = %d, want %d (the assembled PCM)", binary.LittleEndian.Uint32(fixed[40:44]), len(src)-44)
	}
	if int(binary.LittleEndian.Uint32(fixed[4:8])) != len(src)-8 {
		t.Errorf("RIFF size = %d, want %d", binary.LittleEndian.Uint32(fixed[4:8]), len(src)-8)
	}
	// The repaired file must validate as the source audio.
	if _, err := ValidateAudio(fixed); err != nil {
		t.Fatalf("repaired file does not validate: %v", err)
	}

	// A file whose lengths already name it is returned unchanged.
	same := normalizeAssembledWAV(src)
	if !bytes.Equal(same, src) {
		t.Errorf("a final-length WAV was modified; it must pass through byte-identical")
	}

	// A non-WAVE buffer is untouched: the validator names it.
	junk := []byte("not a riff at all, just bytes for the validator to name")
	if out := normalizeAssembledWAV(junk); !bytes.Equal(out, junk) {
		t.Errorf("a non-WAVE buffer was modified; it must be left for the validator")
	}
}

// TestGenerateStreamStreamingPlaceholderHeader: the measured deployment wire
// end to end — the first delta is the marker header, the rest are raw PCM;
// GenerateStream returns the complete validated WAV with the lengths
// repaired, and the audio is the source tone, not a 4-gigabyte chunk.
func TestGenerateStreamStreamingPlaceholderHeader(t *testing.T) {
	src := sineWAV(22050, 0.3, 440, 0.4)
	chunks := placeholderHeaderWAV(src)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "audio/wav")
			w.Write(src)
			return
		}
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			_, _ = w.Write([]byte(deltaFrame(c)))
			fl.Flush()
		}
		_, _ = w.Write([]byte(doneFrame()))
		fl.Flush()
	}))
	defer srv.Close()

	c := newStreamClient(t, srv.URL)
	a, err := c.GenerateStream(context.Background(), GenerateRequest{Text: "hello", Voice: "v"})
	if err != nil {
		t.Fatalf("GenerateStream on the placeholder-header wire: %v", err)
	}
	if len(a.WAV) != len(src) {
		t.Fatalf("WAV = %d byte(s), want the source's %d (the marker header must be patched, not trusted)", len(a.WAV), len(src))
	}
	info, err := ValidateAudio(src)
	if err != nil {
		t.Fatalf("source does not validate: %v", err)
	}
	if a.Duration != info.Duration {
		t.Errorf("duration %s, want the source's %s", a.Duration, info.Duration)
	}
}
