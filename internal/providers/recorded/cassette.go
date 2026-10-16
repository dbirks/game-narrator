package recorded

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// SchemaVersion is the cassette line format this package writes and accepts.
// A reader that meets a different version must fail rather than guess:
// silently replaying an incompatible sidecar is exactly the "synthetic answers
// presented as real" failure docs/spec/12 forbids.
const SchemaVersion = 1

// maxRecordedRequestBytes and maxRecordedResponseBytes bound what a cassette
// may hold. They are deliberately close to (never below) the limits the OpenAI
// adapter enforces, so a request the runtime would send is always recordable,
// and a runaway response is refused instead of ballooning a test fixture.
const (
	maxRecordedRequestBytes  = 16 << 20
	maxRecordedResponseBytes = 8 << 20
)

// Interaction is one recorded request/response pair: one JSONL line in
// <cassette-dir>/<role>.jsonl.
//
// The endpoint host is *never* stored. A cassette says which logical role
// served an interaction, not which machine answered it (docs/spec/06 configures
// roles, and AGENTS.md rule 5 keeps private endpoint URLs out of the tree).
type Interaction struct {
	SchemaVersion int       `json:"schema_version"`
	Role          string    `json:"role"`
	Origin        string    `json:"origin"`
	RecordedAt    time.Time `json:"recorded_at"`
	Request       Request   `json:"request"`
	Response      Response  `json:"response"`
	LatencyMS     int64     `json:"latency_ms"`
}

// Request is the redacted request half of an interaction.
type Request struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Query is the canonical (sorted) form of the URL query, kept separately
	// from Path so a cassette line never encodes an absolute URL.
	Query string `json:"query,omitempty"`
	// Headers carries only the allowlist in redact.go.
	Headers map[string]string `json:"headers,omitempty"`
	// BodyDigest is `sha256:<hex>` over the canonical redacted Body. It is
	// stored so a reader can prove the body was not edited after recording.
	BodyDigest string `json:"body_digest"`
	// Body is the canonicalised (sorted-key, compact, HTML-escaping off),
	// redacted request JSON — `null` when the request had no body.
	Body json.RawMessage `json:"body"`
}

// Response is the replayable response half of an interaction.
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body"`
}

// roleFile maps a role to its cassette path, rejecting anything that could
// escape the cassette directory.
func roleFile(dir string, role providers.Role) (string, error) {
	name := strings.TrimSpace(string(role))
	if name == "" {
		return "", errors.New("recorded: a role is required to name a cassette file")
	}
	if !validRoleName(name) {
		return "", fmt.Errorf("recorded: role %q may only contain letters, digits, '.', '_' or '-'", name)
	}
	return filepath.Join(dir, name+".jsonl"), nil
}

var roleNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validRoleName(name string) bool {
	// Reject "." / ".." / anything with a separator explicitly: the regexp
	// alone would happily accept "../evil".
	if name == "." || name == ".." {
		return false
	}
	return roleNamePattern.MatchString(name) && !strings.ContainsAny(name, `/\`)
}

// canonicalJSON re-encodes a decoded JSON value deterministically: object keys
// sorted (encoding/json's contract for maps), compact, no HTML escaping, and
// numbers kept as their original literals via json.Number. Canonicalising both
// at record time and at replay time is what makes matching independent of the
// key order or escaping a particular client happened to emit.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("recorded: cannot canonicalise JSON: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// decodeJSON parses with number literals preserved so re-encoding is lossless.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// Reject trailing content: a cassette body must be one JSON document.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("recorded: request body has trailing data after the JSON document")
	}
	return v, nil
}

// newRequest builds the storable request record from an outbound request.
func newRequest(req *http.Request, rawBody []byte) (Request, error) {
	if req.URL == nil {
		return Request{}, errors.New("recorded: request has no URL")
	}
	query := canonicalQuery(req.URL)
	body, err := redactRequestBody(rawBody)
	if err != nil {
		return Request{}, err
	}
	return Request{
		Method:     strings.ToUpper(req.Method),
		Path:       req.URL.Path,
		Query:      query,
		Headers:    filterRequestHeaders(req.Header),
		BodyDigest: digest(body),
		Body:       body,
	}, nil
}

// canonicalQuery renders the URL query in sorted-parameter form ("" when
// there is none), so an equivalent request written in a different parameter
// order still matches the cassette.
func canonicalQuery(u *url.URL) string {
	if u == nil {
		return ""
	}
	q := u.RawQuery
	if strings.TrimSpace(q) == "" {
		return ""
	}
	values, err := url.ParseQuery(q)
	if err != nil {
		// An unparsable query is kept verbatim: it must not silently vanish
		// from the matching key.
		return q
	}
	return values.Encode() // url.Values.Encode sorts by key
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// KeyDigest identifies the interaction for replay matching: method, path,
// canonical query and canonical redacted body. It is the only handle a replay
// miss can name, so it is exported through the error message and here.
func (r Request) KeyDigest() string {
	var b bytes.Buffer
	b.WriteString(r.Method)
	b.WriteByte(' ')
	b.WriteString(r.Path)
	if r.Query != "" {
		b.WriteByte('?')
		b.WriteString(r.Query)
	}
	b.WriteByte('\n')
	body := r.Body
	if len(body) == 0 {
		body = json.RawMessage("null")
	}
	b.Write(canonicalForDigest(body))
	return digest(b.Bytes())
}

// canonicalForDigest re-canonicalises stored bytes when possible so a
// hand-formatted cassette matches what a live request produces.
func canonicalForDigest(raw []byte) []byte {
	if v, err := decodeJSON(raw); err == nil {
		if out, err := canonicalJSON(v); err == nil {
			return out
		}
	}
	return bytes.TrimSpace(raw)
}

// validate checks a loaded or about-to-be-written interaction.
func (in *Interaction) validate() error {
	if in == nil {
		return errors.New("recorded: nil interaction")
	}
	if in.SchemaVersion != SchemaVersion {
		return fmt.Errorf("recorded: unsupported cassette schema_version %d, this build writes %d", in.SchemaVersion, SchemaVersion)
	}
	if in.Origin != OriginRecorded {
		return fmt.Errorf("recorded: cassette entry has origin %q, want %q; a cassette is by definition recorded content", in.Origin, OriginRecorded)
	}
	if !validRoleName(strings.TrimSpace(in.Role)) {
		return fmt.Errorf("recorded: cassette entry has invalid role %q", in.Role)
	}
	if in.Request.Method == "" || in.Request.Path == "" {
		return errors.New("recorded: cassette entry is missing request.method or request.path")
	}
	if len(in.Request.Body) == 0 {
		in.Request.Body = json.RawMessage("null")
	}
	if in.Request.BodyDigest == "" {
		in.Request.BodyDigest = digest(canonicalForDigest(in.Request.Body))
	} else if got := digest(canonicalForDigest(in.Request.Body)); got != in.Request.BodyDigest {
		return fmt.Errorf("recorded: cassette entry for %s %s has body_digest %s but its stored body hashes to %s",
			in.Request.Method, in.Request.Path, in.Request.BodyDigest, got)
	}
	if in.Response.Status == 0 {
		return errors.New("recorded: cassette entry is missing response.status")
	}
	return nil
}

// line serialises the interaction and runs the final credential guard. It is
// the only way a cassette line reaches disk, so no writer can skip the guard.
func (in *Interaction) line(secrets []string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(in); err != nil {
		return nil, fmt.Errorf("recorded: cannot serialise cassette entry: %w", err)
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	if err := guard(out, secrets); err != nil {
		return nil, err
	}
	return out, nil
}

// readCassette loads every interaction from a JSONL file. A missing file is not
// an error at load time — the replay attempt then misses, which is the only way
// to guarantee no fabricated response is served for an unrecorded request.
func readCassette(path string, secrets []string) ([]*Interaction, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recorded: cannot open cassette %s: %w", path, err)
	}
	defer f.Close()
	var out []*Interaction
	sc := newLineScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := guard(line, secrets); err != nil {
			return nil, fmt.Errorf("recorded: %s:%d: %w", filepath.Base(path), lineNo, err)
		}
		in, err := parseInteraction(line)
		if err != nil {
			return nil, fmt.Errorf("recorded: %s:%d: %w", filepath.Base(path), lineNo, err)
		}
		out = append(out, in)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("recorded: cannot read cassette %s: %w", path, err)
	}
	return out, nil
}

// parseInteraction decodes one line without letting encoding/json flatten the
// request body: Body is kept as raw canonical bytes for exact matching.
func parseInteraction(line []byte) (*Interaction, error) {
	var raw struct {
		SchemaVersion int        `json:"schema_version"`
		Role          string     `json:"role"`
		Origin        string     `json:"origin"`
		RecordedAt    string     `json:"recorded_at"`
		Request       requestRaw `json:"request"`
		Response      Response   `json:"response"`
		LatencyMS     int64      `json:"latency_ms"`
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("malformed cassette line: %w", err)
	}
	when, err := time.Parse(time.RFC3339Nano, raw.RecordedAt)
	if err != nil {
		return nil, fmt.Errorf("recorded_at %q is not RFC3339: %w", raw.RecordedAt, err)
	}
	body := json.RawMessage(raw.Request.Body)
	if len(bytes.TrimSpace(body)) == 0 {
		body = json.RawMessage("null")
	}
	in := &Interaction{
		SchemaVersion: raw.SchemaVersion,
		Role:          raw.Role,
		Origin:        raw.Origin,
		RecordedAt:    when.UTC(),
		Request: Request{
			Method:     raw.Request.Method,
			Path:       raw.Request.Path,
			Query:      raw.Request.Query,
			Headers:    raw.Request.Headers,
			BodyDigest: raw.Request.BodyDigest,
			Body:       body,
		},
		Response:  raw.Response,
		LatencyMS: raw.LatencyMS,
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	return in, nil
}

type requestRaw struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Query      string            `json:"query"`
	Headers    map[string]string `json:"headers"`
	BodyDigest string            `json:"body_digest"`
	Body       json.RawMessage   `json:"body"`
}

// appendInteraction writes one line, creating the cassette file and directory
// on demand. Appends are serialised by the Transport mutex, and O_APPEND keeps
// concurrent processes from overwriting each other's lines.
func appendInteraction(path string, in *Interaction, secrets []string) error {
	line, err := in.line(secrets)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("recorded: cannot create cassette directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("recorded: cannot open cassette %s: %w", path, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("recorded: cannot write cassette %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("recorded: cannot finish cassette %s: %w", path, err)
	}
	return nil
}

// httpResponse rebuilds a replayable response. ContentLength and the body
// bytes are exactly what was recorded, so a client cannot tell the difference
// except through the origin header.
func (in *Interaction) httpResponse(req *http.Request) *http.Response {
	header := http.Header{}
	for k, v := range in.Response.Headers {
		header.Set(k, v)
	}
	header.Set(HeaderOrigin, OriginRecorded)
	body := []byte(in.Response.Body)
	return &http.Response{
		Status:        http.StatusText(in.Response.Status),
		StatusCode:    in.Response.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// Load reads a cassette file for reporting: a test report or the developer UI
// can enumerate what a recorded run actually contains — role, path, request
// digest, status, latency — without replaying it (docs/spec/12: reports record
// stage origins). A missing file is an error here, unlike in replay, where an
// absent cassette legitimately produces misses.
func Load(path string) ([]*Interaction, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("recorded: no cassette at %s: %w", path, err)
	}
	entries, err := readCassette(path, nil)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// LoadRole loads <dir>/<role>.jsonl.
func LoadRole(dir string, role providers.Role) ([]*Interaction, error) {
	path, err := roleFile(dir, role)
	if err != nil {
		return nil, err
	}
	return Load(path)
}
