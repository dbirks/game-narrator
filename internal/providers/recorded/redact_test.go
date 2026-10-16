package recorded

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// testToken stands in for a live gateway credential. Everything in this file
// proves that a credential, an Authorization header or an image payload cannot
// reach disk through this package (AGENTS.md rule 5).
const testToken = "gn-secret-token-0123456789abcdef"

// timeNow is a fixed clock so recorded_at is reproducible in failure output.
func timeNow() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func testImage(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if x < 4 {
				img.Set(x, y, color.RGBA{R: 255, A: 255})
			} else {
				img.Set(x, y, color.RGBA{B: 255, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

// visionBody is the wire shape the OpenAI adapter produces for an image
// question, in the exact ordering its own encoder emits.
func visionBody(t *testing.T, model, prompt string, image []byte) []byte {
	payload := map[string]any{
		"model": model,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": prompt},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(image),
				}},
			},
		}},
		"stream": false,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return raw
}

func TestFilterHeadersKeepsOnlyTheAllowlist(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Cookie", "session=leak-me")
	h.Set("X-Internal-Trace", "trace-me")
	h.Set("X-Request-Id", "req-42")

	got := filterRequestHeaders(h)
	if len(got) != 2 || got["content-type"] != "application/json" || got["accept"] != "application/json" {
		t.Errorf("request headers = %v, want content-type and accept only", got)
	}
	for name := range got {
		if name == "authorization" {
			t.Fatal("Authorization must never be recorded")
		}
	}

	resp := filterResponseHeaders(h)
	if len(resp) != 2 || resp["x-request-id"] != "req-42" || resp["content-type"] != "application/json" {
		t.Errorf("response headers = %v, want content-type and x-request-id only", resp)
	}
	if _, ok := resp["cookie"]; ok {
		t.Error("unlisted response header was recorded")
	}
}

func TestRedactRequestBodyReplacesBase64ImageWithDigest(t *testing.T) {
	img := testImage(t)
	raw := visionBody(t, "model-a", "name the two colours", img)

	canonical, err := redactRequestBody(raw)
	if err != nil {
		t.Fatalf("redactRequestBody: %v", err)
	}
	text := string(canonical)
	if strings.Contains(text, "base64") {
		t.Errorf("redacted body still mentions base64:\n%s", text)
	}
	if !strings.Contains(text, "data:image/png;sha256,") {
		t.Errorf("redacted body has no digest image reference:\n%s", text)
	}
	want := sha256Hex(img)
	if !strings.Contains(text, want) {
		t.Errorf("redacted body does not carry the sha256 of the image bytes (%s):\n%s", want, text)
	}
	if strings.Contains(text, base64.StdEncoding.EncodeToString(img)[:24]) {
		t.Error("part of the image payload survived redaction")
	}
	// The rest of the request must still be intact: images are digested, the
	// prompt is not.
	if !strings.Contains(text, "name the two colours") {
		t.Errorf("prompt was lost during redaction:\n%s", text)
	}
}

func TestRedactRequestBodyIsCanonicalAndStable(t *testing.T) {
	first, err := redactRequestBody([]byte(`{"b":1,"a":{"z":[1,2,3],"m":null},"t":"x"}`))
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	second, err := redactRequestBody([]byte(`{ "a" : { "m": null, "z": [ 1, 2, 3 ] }, "t":"x", "b": 1 }`))
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("canonical forms differ:\n %s\n %s", first, second)
	}
	// Numbers keep their literal form: 1.0 must not become 1, or a client that
	// re-serialises sampling parameters would never match its own recording.
	dec, err := redactRequestBody([]byte(`{"temperature":1.0,"n":10.50}`))
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	if !strings.Contains(string(dec), `1.0`) || !strings.Contains(string(dec), `10.50`) {
		t.Errorf("number literals were rewritten: %s", dec)
	}
	// HTML escaping must not leak into the canonical form either.
	esc, err := redactRequestBody([]byte(`{"q":"a<b>&c"}`))
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	if strings.Contains(string(esc), `\u003c`) || strings.Contains(string(esc), `\u0026`) {
		t.Errorf("canonical form is HTML-escaped: %s", esc)
	}
	// An empty body is a documented value, not an absent field.
	empty, err := redactRequestBody(nil)
	if err != nil || string(empty) != "null" {
		t.Errorf("redactRequestBody(nil) = %s, %v, want null", empty, err)
	}
}

func TestRedactRequestBodyRejectsOversizeAndBinary(t *testing.T) {
	big := make([]byte, maxRecordedRequestBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := redactRequestBody(big); err == nil {
		t.Error("an oversized request body must be refused, not truncated")
	}
	if _, err := redactRequestBody([]byte{0x00, 0x01, 0x02, 0xff, 0xfe}); err == nil {
		t.Error("a binary non-JSON request body must be refused")
	}
}

func TestGuardRefusesCredentialBearerRunAndBase64(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"active token", `{"role":"fast_reasoning","note":"key is ` + testToken + `"}`},
		{"bearer run", `{"headers":{"proxy-authorization":"Bearer abcdef0123456789abcdef"}}`},
		{"leftover image", `{"body":"data:image/png;base64,iVBORw0KGgo="}`},
		{"leftover octet stream", `{"body":"data:application/octet-stream;base64,AAECAwQ="}`},
	} {
		if err := guard([]byte(tc.line), []string{testToken}); err == nil {
			t.Errorf("%s: guard accepted a line that must never be written", tc.name)
		}
	}
	// Benign content must pass, including a provider that complains about the
	// absence of a bearer token in words.
	for _, line := range []string{
		`{"error":{"message":"Bearer token is required"}}`,
		`{"choices":[{"message":{"content":"red blue"}}]}`,
	} {
		if err := guard([]byte(line), []string{testToken}); err != nil {
			t.Errorf("guard rejected a harmless line %s: %v", line, err)
		}
	}
}

// TestInteractionLineRefusesSecretAndWritesNothing is the "refuses to write"
// half of the redaction contract: the guard runs inside the only serialiser, so
// a leak is a hard error and no partial file survives.
func TestInteractionLineRefusesSecretAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fast_reasoning.jsonl")
	in := &Interaction{
		SchemaVersion: SchemaVersion,
		Role:          string(providers.RoleFastReasoning),
		Origin:        OriginRecorded,
		RecordedAt:    timeNow(),
		Request: Request{
			Method: "POST", Path: "/v1/chat/completions",
			BodyDigest: digest(json.RawMessage(`{}`)), Body: json.RawMessage(`{}`),
		},
		Response: Response{Status: 500, Body: `{"error":{"message":"invalid key ` + testToken + `"}}`},
	}
	err := appendInteraction(path, in, []string{testToken})
	if err == nil {
		t.Fatal("appendInteraction accepted an interaction containing the active token")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("error = %v, want it to name the credential problem", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the refused write still created %s (stat err=%v)", path, statErr)
	}
	// And the same cassette, hand-edited to add a token, is refused at read time
	// too — reading is not a privileged path.
	good := &Interaction{
		SchemaVersion: SchemaVersion,
		Role:          string(providers.RoleFastReasoning),
		Origin:        OriginRecorded,
		RecordedAt:    timeNow(),
		Request:       Request{Method: "POST", Path: "/v1/chat/completions", Body: json.RawMessage(`{}`)},
		Response:      Response{Status: 200, Body: `{}`},
	}
	good.Request.BodyDigest = digest(canonicalForDigest(good.Request.Body))
	if err := appendInteraction(path, good, nil); err != nil {
		t.Fatalf("clean interaction rejected: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cassette: %v", err)
	}
	tampered := strings.Replace(string(raw), `"body":"{}"`, `"body":"token=`+testToken+`"`+`,"x":"{}"`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatalf("write tampered cassette: %v", err)
	}
	if _, err := readCassette(path, []string{testToken}); err == nil {
		t.Error("readCassette accepted a cassette line containing the active token")
	}
}

// TestAppendInteractionRejectsSchemaDrift protects the sidecar contract: an
// unknown schema version or a non-recorded origin never loads.
func TestAppendInteractionRejectsSchemaDrift(t *testing.T) {
	line := `{"schema_version":99,"role":"fast_reasoning","origin":"recorded","recorded_at":"2026-01-01T00:00:00Z",` +
		`"request":{"method":"POST","path":"/v1/chat/completions","body_digest":"sha256:x","body":{}},` +
		`"response":{"status":200,"body":"{}"},"latency_ms":1}`
	if _, err := parseInteraction([]byte(line)); err == nil {
		t.Error("schema_version 99 parsed without error")
	} else if !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("error = %v, want it to name schema_version", err)
	}
	live := strings.Replace(line, `"schema_version":99`, `"schema_version":1`, 1)
	live = strings.Replace(live, `"origin":"recorded"`, `"origin":"live"`, 1)
	if _, err := parseInteraction([]byte(live)); err == nil {
		t.Error(`origin "live" loaded as cassette content`)
	}
	// A body edited after recording is detected via body_digest.
	drifted := `{"schema_version":1,"role":"fast_reasoning","origin":"recorded","recorded_at":"2026-01-01T00:00:00Z",` +
		`"request":{"method":"POST","path":"/v1/chat/completions","body_digest":"sha256:` + strings.Repeat("0", 64) + `","body":{"model":"m"}},` +
		`"response":{"status":200,"body":"{}"},"latency_ms":1}`
	if _, err := parseInteraction([]byte(drifted)); err == nil || !strings.Contains(err.Error(), "body_digest") {
		t.Errorf("a body that does not hash to its stored digest was accepted: %v", err)
	}
	// Unknown fields are a format disagreement, not something to ignore.
	unknown := `{"schema_version":1,"role":"fast_reasoning","origin":"recorded","recorded_at":"2026-01-01T00:00:00Z",` +
		`"request":{"method":"POST","path":"/v1/chat/completions","body_digest":"sha256:` + digest(json.RawMessage(`{}`))[7:] + `","body":{},"totp":"x"},` +
		`"response":{"status":200,"body":"{}"},"latency_ms":1}`
	if _, err := parseInteraction([]byte(unknown)); err == nil {
		t.Error("an unknown cassette field was accepted silently")
	}
}

func TestRoleFileRejectsTraversal(t *testing.T) {
	if _, err := roleFile(t.TempDir(), providers.Role("../evil")); err == nil {
		t.Error("role \"../evil\" produced a cassette path")
	}
	if _, err := roleFile(t.TempDir(), providers.Role("")); err == nil {
		t.Error("an empty role produced a cassette path")
	}
	p, err := roleFile("/cassettes", providers.RoleFastReasoning)
	if err != nil {
		t.Fatalf("valid role rejected: %v", err)
	}
	if filepath.Base(p) != "fast_reasoning.jsonl" || filepath.Dir(p) != "/cassettes" {
		t.Errorf("path = %q", p)
	}
}

// assertNoLeakage is the committable-cassette contract: no endpoint, no
// credential, no image payload, in any form.
func assertNoLeakage(t *testing.T, cassette string) {
	t.Helper()
	for _, banned := range []string{"gateway.invalid", "http://", "https://", testToken, "Bearer ", "base64,"} {
		if strings.Contains(cassette, banned) {
			t.Errorf("cassette contains %q; it is not committable", banned)
		}
	}
}
