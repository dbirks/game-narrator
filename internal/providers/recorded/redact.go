package recorded

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Header allowlists. Anything not listed never reaches a cassette: a
// Authorization header, a cookie, a client fingerprint or a proxy trace header
// is not needed to replay a response, and storing one would turn a test
// fixture into a credential (AGENTS.md rule 5).
var (
	requestHeaderAllowlist  = []string{"Accept", "Content-Type"}
	responseHeaderAllowlist = []string{"Content-Type", "X-Request-Id"}
)

func filterRequestHeaders(h http.Header) map[string]string {
	return filterHeaders(h, requestHeaderAllowlist)
}

func filterResponseHeaders(h http.Header) map[string]string {
	return filterHeaders(h, responseHeaderAllowlist)
}

// filterHeaders keeps allowlisted headers only, keyed lower-case so a cassette
// does not depend on the casing a server happened to use.
func filterHeaders(h http.Header, allow []string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(allow))
	for _, name := range allow {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		out[strings.ToLower(name)] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// dataURLPattern matches a base64 data URL inside a string value. The payload
// class is the RFC 4648 alphabet plus padding; anything else ends the match,
// and the trailing guard below catches shapes this pattern failed to redact.
var dataURLPattern = regexp.MustCompile(`(?is)data:([^;,]*)\s*;\s*base64\s*,([A-Za-z0-9+/\s]+={0,2})`)

// bearerRunPattern finds a bare "Bearer <token>" run in serialized content. A
// mention such as "Bearer token is required" is too short to match, so ordinary
// provider error text still records; an actual credential does not.
var bearerRunPattern = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`)

// base64MarkerPattern is the post-redaction tripwire. If any of these survive,
// something was not redacted and the line must not be written.
var base64MarkerPattern = regexp.MustCompile(`(?i)(;\s*base64\s*,|data:[^,\n]{0,120}base64[;/])`)

// redactRequestBody turns raw request bytes into the canonical, redacted JSON
// stored in a cassette. Binary or non-JSON bodies are stored as a JSON string
// with the same redaction applied; images are replaced by a digest, never
// stored (docs/spec/12 requires the sidecar to be committable).
func redactRequestBody(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	if len(raw) > maxRecordedRequestBytes {
		return nil, fmt.Errorf("recorded: request body is %d bytes, over the %d byte cassette limit; the interaction is not recordable",
			len(raw), maxRecordedRequestBytes)
	}
	if !json.Valid(raw) {
		if !isProbablyText(raw) {
			return nil, errors.New("recorded: cannot store a non-text, non-JSON request body; send JSON or record the interaction with a text payload")
		}
		text, _, err := redactString(string(raw))
		if err != nil {
			return nil, err
		}
		return canonicalJSON(text)
	}
	value, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("recorded: cannot decode request body: %w", err)
	}
	redacted, _, err := redactValue(value)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(redacted)
}

// redactValue walks a decoded JSON document and replaces every base64 data URL.
func redactValue(v any) (any, int, error) {
	switch t := v.(type) {
	case string:
		out, n, err := redactString(t)
		return out, n, err
	case map[string]any:
		count := 0
		for k, item := range t {
			out, n, err := redactValue(item)
			if err != nil {
				return nil, 0, fmt.Errorf("recorded: field %q: %w", k, err)
			}
			t[k] = out
			count += n
		}
		return t, count, nil
	case []any:
		count := 0
		for i, item := range t {
			out, n, err := redactValue(item)
			if err != nil {
				return nil, 0, fmt.Errorf("recorded: element %d: %w", i, err)
			}
			t[i] = out
			count += n
		}
		return t, count, nil
	default:
		return v, 0, nil
	}
}

// redactString replaces `data:<mime>;base64,<payload>` with
// `data:<mime>;sha256,<hex>`, hashing the decoded bytes when the payload is
// valid base64 so the same image matches even if it was re-encoded with a
// different alphabet.
func redactString(s string) (string, int, error) {
	if !strings.Contains(strings.ToLower(s), "base64") {
		return s, 0, nil
	}
	count := 0
	out := dataURLPattern.ReplaceAllStringFunc(s, func(match string) string {
		groups := dataURLPattern.FindStringSubmatch(match)
		if len(groups) < 3 {
			return match
		}
		count++
		mime := strings.TrimSpace(groups[1])
		if mime == "" {
			mime = "application/octet-stream"
		}
		return "data:" + mime + ";sha256," + sha256Hex(payloadBytes(groups[2]))
	})
	if count > 0 && base64MarkerPattern.MatchString(out) {
		return "", 0, errors.New("a base64 payload survived redaction; refusing to record this body")
	}
	return out, count, nil
}

// payloadBytes decodes a base64 payload so the digest identifies the image
// bytes, not one particular encoding of them; an undecodable payload is hashed
// verbatim rather than dropped, so matching stays deterministic.
func payloadBytes(payload string) []byte {
	compact := strings.Join(strings.Fields(payload), "")
	if decoded, err := base64.StdEncoding.DecodeString(compact); err == nil {
		return decoded
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(compact); err == nil {
		return decoded
	}
	return []byte(payload)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ensureNoDataURLs is the response-side rule: a recorded response must be
// byte-identical to what the provider sent, so a response that carries a base64
// payload (generated image, inline audio) is refused outright instead of being
// quietly rewritten into something replay would serve wrongly.
func ensureNoDataURLs(kind string, body []byte) error {
	if base64MarkerPattern.Match(body) {
		return fmt.Errorf("recorded: %s body carries a base64 payload; cassettes store text/JSON responses only, so this interaction cannot be recorded (binary responses and streaming are not supported yet)", kind)
	}
	return nil
}

// guard is the last line of defence before a line touches disk (and the same
// check when a cassette is read back, so a hand-edited fixture cannot smuggle a
// credential into a test run either).
func guard(line []byte, secrets []string) error {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if bytes.Contains(line, []byte(secret)) {
			return errors.New("refusing to write: the active credential appears in the serialised interaction")
		}
	}
	if bearerRunPattern.Match(line) {
		return errors.New("refusing to write: a bare \"Bearer <token>\" run appears in the serialised interaction")
	}
	if base64MarkerPattern.Match(line) {
		return errors.New("refusing to write: a base64 payload appears in the serialised interaction")
	}
	return nil
}

func isProbablyText(b []byte) bool {
	limit := len(b)
	if limit > 512 {
		limit = 512
	}
	for _, r := range string(b[:limit]) {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	// A recorded vision request with a digest-sized body plus a long prompt can
	// exceed the 64 KiB default; allow one full interaction per line.
	const maxLine = maxRecordedRequestBytes + (1 << 20)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	return sc
}
