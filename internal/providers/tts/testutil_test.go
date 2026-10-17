package tts

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Shared fixtures and fakes for the tts package tests. The fake speech server
// records every request (method, path, headers, body) so tests can assert the
// wire contract — task_type presence, voice, bearer token — the way the
// recorded provider tests assert their cassettes (ADR-0013 pattern).

// fixtureWAV is the committed canonical reference cassette
// (testdata/ref-24k.wav): 1.0 s, 24 kHz, 16-bit mono, ~40 % of full scale.
func fixtureWAV(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ref-24k.wav"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// recordedRequest is one captured request.
type recordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// fakeSpeechServer is an httptest server that answers /audio/speech with a
// scripted sequence and records everything.
type fakeSpeechServer struct {
	t        *testing.T
	srv      *httptest.Server
	requests []recordedRequest
	// handler answers request i (0-based) with the given status/body.
	handler func(i int, rec recordedRequest) (int, []byte, string)
}

func newFakeSpeechServer(t *testing.T, handler func(i int, rec recordedRequest) (int, []byte, string)) *fakeSpeechServer {
	t.Helper()
	f := &fakeSpeechServer{t: t, handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   body,
		}
		f.requests = append(f.requests, rec)
		status, respBody, contentType := f.handler(len(f.requests)-1, rec)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Header().Set("X-Request-Id", "req-"+fmt.Sprint(len(f.requests)))
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSpeechServer) url() string { return f.srv.URL }

func (f *fakeSpeechServer) last() recordedRequest {
	if len(f.requests) == 0 {
		f.t.Fatalf("no request was recorded")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeSpeechServer) nth(i int) recordedRequest {
	if i >= len(f.requests) {
		f.t.Fatalf("request %d was not recorded (%d total)", i, len(f.requests))
	}
	return f.requests[i]
}

// errorBody renders the OpenAI-style error envelope the checkpoints use.
func errorBody(message string) []byte {
	return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"BadRequestError","param":null,"code":400}}`, message))
}

// --- WAV construction -----------------------------------------------------

// wavFormat renders a minimal RIFF/WAVE container around raw PCM data.
// format 1 = PCM; anything else is used for the malformed-fixture cases.
func wavFormat(format, channels, sampleRate, bits int, data []byte) []byte {
	byteRate := sampleRate * channels * bits / 8
	out := make([]byte, 0, 44+len(data))
	out = append(out, "RIFF"...)
	out = appendU32(out, 36+uint32(len(data)))
	out = append(out, "WAVE"...)
	out = append(out, "fmt "...)
	out = appendU32(out, 16)
	out = appendU16(out, uint16(format))
	out = appendU16(out, uint16(channels))
	out = appendU32(out, uint32(sampleRate))
	out = appendU32(out, uint32(byteRate))
	out = appendU16(out, uint16(channels*bits/8))
	out = appendU16(out, uint16(bits))
	out = append(out, "data"...)
	out = appendU32(out, uint32(len(data)))
	out = append(out, data...)
	return out
}

// sineWAV renders a 16-bit mono sine at the given amplitude (0..1 of full
// scale) for the given duration; the canonical test audio.
func sineWAV(sampleRate int, seconds float64, freqHz, amplitude float64) []byte {
	n := int(seconds * float64(sampleRate))
	data := make([]byte, n*2)
	for i := 0; i < n; i++ {
		s := int16(float64(int16Max) * amplitude * math.Sin(2*math.Pi*freqHz*float64(i)/float64(sampleRate)))
		binary.LittleEndian.PutUint16(data[2*i:], uint16(s))
	}
	return wavFormat(1, 1, sampleRate, 16, data)
}

// silenceWAV renders true digital silence — the "all-silent" fixture. 8-bit
// PCM is unsigned, so its zero level is 128, not 0 (0 is full negative
// scale, which the clipping check would rightly flag).
func silenceWAV(sampleRate int, seconds float64, bits int) []byte {
	n := int(seconds * float64(sampleRate))
	data := make([]byte, n*bits/8)
	if bits == 8 {
		for i := range data {
			data[i] = 128
		}
	}
	return wavFormat(1, 1, sampleRate, bits, data)
}

// clippedWAV renders samples pinned to ±full scale — the "clipped" fixture.
func clippedWAV(sampleRate int, seconds float64) []byte {
	n := int(seconds * float64(sampleRate))
	data := make([]byte, n*2)
	for i := 0; i < n; i++ {
		var s int16
		if i%2 == 0 {
			s = 32767
		} else {
			s = -32768
		}
		binary.LittleEndian.PutUint16(data[2*i:], uint16(s))
	}
	return wavFormat(1, 1, sampleRate, 16, data)
}

const int16Max = 32767

// wavDataOnly strips the RIFF header, leaving the raw PCM payload.
func wavDataOnly(wav []byte) []byte {
	if len(wav) < 44 {
		return nil
	}
	return wav[44:]
}

// setChunkSize rewrites the data-chunk size field at offset 40 so the
// declared length no longer matches the actual payload.
func setChunkSize(wav []byte, size int) []byte {
	out := append([]byte(nil), wav...)
	binary.LittleEndian.PutUint32(out[40:], uint32(size))
	return out
}

// mustBytes keeps helper call sites readable.
func mustBytes(b []byte) []byte { return b }
