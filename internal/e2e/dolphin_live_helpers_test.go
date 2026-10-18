// Package e2e — helpers for the live Dolphin E2E (task m6-live-e2e).
//
// The helpers here are test-only and log-safe per AGENTS.md rule 5: log lines
// and returned summaries carry counts, ids, hashes and timings — never
// dialogue text, page content or tokens. The evidence bundle written under
// the gitignored artifacts/ directory is the place that may carry raw
// payloads; the test's own log and report do not.

package e2e

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
)

// dolphinEvidenceDir is the gitignored evidence root for this test
// (.gitignore: artifacts/). It resolves under the package directory, the
// same way the onboarding test's evidence dir does.
const dolphinEvidenceDir = "artifacts/m6-live"

// devEventsPage is the GET /api/v1/dev/events response shape (the durable
// event log page the developer UI polls when the SSE stream is down).
type devEventsPage struct {
	Events    []devEnvelope `json:"events"`
	NextSeq   int64         `json:"next_seq"`
	Truncated bool          `json:"truncated"`
}

// devEnvelope is one persisted event as the developer API returns it.
type devEnvelope struct {
	Seq         int64           `json:"seq"`
	EventID     string          `json:"event_id"`
	SessionID   string          `json:"session_id"`
	TraceID     string          `json:"trace_id"`
	CausationID string          `json:"causation_id"`
	Name        string          `json:"name"`
	OccurredAt  string          `json:"occurred_at"`
	Data        json.RawMessage `json:"data"`
}

// dolphinDo is the authenticated request helper for the in-process API
// server (the same handler the production binary serves).
func dolphinDo(t *testing.T, srv http.Handler, token string, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}

// dolphinDoMultipart uploads one file part named "file" plus the named form
// fields, the shape POST /api/v1/profiles/{id}/import accepts.
func dolphinDoMultipart(t *testing.T, srv http.Handler, token, target, fileName string, fileBytes []byte, fields map[string]string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("multipart field %s: %v", k, err)
		}
	}
	fw, err := w.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("multipart file part: %v", err)
	}
	if _, err := fw.Write(fileBytes); err != nil {
		t.Fatalf("multipart file bytes: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, target, &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}

// jobSnapshot is the /api/v1/jobs/{id} fields the poller reads.
type jobSnapshot struct {
	ID     string          `json:"id"`
	State  string          `json:"state"`
	Result json.RawMessage `json:"result"`
}

// dolphinWaitJob polls the job route until a terminal state (succeeded or a
// terminal failure) or the deadline, returning the last snapshot.
func dolphinWaitJob(t *testing.T, srv http.Handler, token, jobID string, deadline time.Duration) jobSnapshot {
	t.Helper()
	var last jobSnapshot
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(deadline)
	defer timeout.Stop()
	for {
		select {
		case <-timeout.C:
			t.Fatalf("job %s not terminal after %s (last state %q)", jobID, deadline, last.State)
		case <-tick.C:
		}
		rec := dolphinDo(t, srv, token, http.MethodGet, "/api/v1/jobs/"+jobID, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("job %s GET = %d: %s", jobID, rec.Code, rec.Body.String())
		}
		var j jobSnapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
			t.Fatalf("job %s decode: %v", jobID, err)
		}
		last = j
		switch j.State {
		case "succeeded", "failed", "cancelled":
			return last
		}
	}
}

// levenshtein is the classic DP edit distance over runes.
func dolphinLevenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// dolphinCER is the character error rate of ocr against ref: the edit
// distance over letter-like runes only (case folded, punctuation and
// whitespace stripped), normalised by the reference length. Both texts are
// the product's own readings, kept out of the log by the caller.
func dolphinCER(ocr, ref string) float64 {
	keep := func(s string) []rune {
		var out []rune
		for _, r := range strings.ToLower(strings.TrimSpace(s)) {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				out = append(out, r)
			}
		}
		return out
	}
	o, r := keep(ocr), keep(ref)
	if len(r) == 0 {
		if len(o) == 0 {
			return 0
		}
		return 1
	}
	return float64(dolphinLevenshtein(o, r)) / float64(len(r))
}

// strayDolphinProcs scans /proc for another dolphin-emu or Xvfb process
// (the no-parallel rule the dolphin real tests enforce): two emulators on
// one machine fight over the render-window lookup.
func strayDolphinProcs() error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil // no /proc (non-linux host); the launch itself will say so
	}
	for _, e := range entries {
		if !isPIDDir(e.Name()) {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if name == "dolphin-emu" || name == "Xvfb" {
			return fmt.Errorf("another %s process is already running (pid %s); the no-parallel rule forbids two bridges at once", name, e.Name())
		}
	}
	return nil
}

func isPIDDir(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// repoGitSHA reads the repository's HEAD commit by reading .git/HEAD
// directly (no git invocation: the test never mutates the repo). "" when
// the working tree is not a checkout (e.g. a release archive).
func repoGitSHA() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		head, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
		if err == nil {
			return resolveGitRef(dir, string(head))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func resolveGitRef(dir, head string) string {
	head = strings.TrimSpace(head)
	if len(head) == 40 && isHex(head) {
		return head // detached
	}
	if strings.HasPrefix(head, "ref: ") {
		ref := strings.TrimPrefix(head, "ref: ")
		b, err := os.ReadFile(filepath.Join(dir, ".git", ref))
		if err == nil {
			return strings.TrimSpace(string(b))
		}
		pack, err := os.ReadFile(filepath.Join(dir, ".git", "packed-refs"))
		if err == nil {
			sc := bufio.NewScanner(strings.NewReader(string(pack)))
			for sc.Scan() {
				fields := strings.Fields(sc.Text())
				if len(fields) == 2 && fields[1] == ref {
					return fields[0]
				}
			}
		}
	}
	return head
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// dolphinBinaryVersion extracts the version string from the emulator binary
// (Dolphin embeds "Dolphin <yyww>"), and dolphinBinarySHA16 is the binary's
// content identity for the manifest.
var dolphinVersionRe = regexp.MustCompile(`Dolphin \d{3,6}`)

func dolphinBinaryInfo(bin string) (version, sha16 string) {
	b, err := os.ReadFile(bin)
	if err != nil {
		return "", ""
	}
	if m := dolphinVersionRe.Find(b); m != nil {
		version = string(m)
	}
	return version, sha16Hex(b)
}

func sha16Hex(b []byte) string {
	sum := sha256Sum(b)
	return hexEncode(sum)[:16]
}

// dolphinImageSHA streams the SHA-256 of the user-supplied disc image
// (4 MiB chunks, the bridge's hashFile contract: the image is >1 GiB and is
// never loaded whole). The caller must not log the value — AGENTS.md rule 5:
// it is only passed in-process into the API (the edition's source identity)
// and compared against the bridge's own manifest computation.
func dolphinImageSHA(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open disc image for hashing: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 4<<20)); err != nil {
		t.Fatalf("hash disc image: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// wavDurationSeconds parses a WAV's data chunk length (no external deps).
func wavDurationSeconds(path string) (float64, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return 0, 0, fmt.Errorf("wav header: %w", err)
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return 0, 0, fmt.Errorf("not a RIFF/WAVE file: %s", filepath.Base(path))
	}
	var (
		sampleRate int
		channels   int
		bits       int
		dataBytes  int64
	)
	for {
		chHdr := make([]byte, 8)
		if _, err := io.ReadFull(f, chHdr); err != nil {
			break
		}
		size := int64(uint32(chHdr[4]) | uint32(chHdr[5])<<8 | uint32(chHdr[6])<<16 | uint32(chHdr[7])<<24)
		if size < 0 || size > 1<<24 {
			break
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(f, body); err != nil {
			break
		}
		switch string(chHdr[0:4]) {
		case "fmt ":
			channels = int(binary.LittleEndian.Uint16(body[2:4]))
			sampleRate = int(binary.LittleEndian.Uint32(body[4:8]))
			bits = int(binary.LittleEndian.Uint16(body[14:16]))
		case "data":
			dataBytes = size
		}
		if size%2 == 1 {
			if _, err := f.Seek(1, io.SeekCurrent); err != nil {
				break
			}
		}
	}
	if sampleRate <= 0 || bits <= 0 {
		return 0, dataBytes, fmt.Errorf("wav fmt chunk missing: %s", filepath.Base(path))
	}
	return float64(dataBytes) / (float64(sampleRate) * float64(bits/8) * float64(channels)), dataBytes, nil
}

// writeEvidenceFile writes one bundle file, creating directories.
func writeEvidenceFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeEvidenceJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("evidence %s encode: %v", filepath.Base(path), err)
	}
	if err := writeEvidenceFile(path, b); err != nil {
		t.Fatalf("evidence %s write: %v", filepath.Base(path), err)
	}
}
