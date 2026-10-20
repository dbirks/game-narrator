package api

// The developer inspector's per-event actions (ADR-0083): the trace fixture
// recorded into the external fixture root, the stored observation's re-run
// with its diff, the trace's redacted evidence bundle, and the utterance's
// stored audio. The environment is the session env's real shape: a real
// store with the synthetic profile, a real replay session on the synthetic
// clip with the scripted truth OCR, and — where a test needs the utterance's
// audio — the wired voice workflow (approved narrator voice, scripted TTS,
// the programmable sink), so the events, the dev-trace store and the speech
// cache all hold the run's own bytes.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
)

// devEventsOf reads the session's durable events through the developer route,
// the same read the inspector's page performs.
func devEventsOf(t *testing.T, e *sessionEnv, sessionID string) []map[string]any {
	t.Helper()
	got := e.mustOK(auth.RoleDeveloper, "/api/v1/dev/events?session_id="+sessionID+"&limit=1000")
	page, _ := got["events"].([]any)
	out := make([]map[string]any, 0, len(page))
	for _, raw := range page {
		out = append(out, raw.(map[string]any))
	}
	return out
}

// firstCommitTrace returns the first message.committed event's trace id: the
// page-1 occurrence, whose trace holds the observation, the ranking and —
// when the speech workflow is wired — the utterance's audio.
func firstCommitTrace(t *testing.T, e *sessionEnv, sessionID string) (string, map[string]any) {
	t.Helper()
	for _, ev := range devEventsOf(t, e, sessionID) {
		if ev["name"] == "message.committed" {
			traceID, _ := ev["trace_id"].(string)
			if traceID == "" {
				t.Fatalf("a commit carries no trace id: %v", ev)
			}
			return traceID, ev
		}
	}
	t.Fatal("the replay committed nothing; the clip's two pages are documented (ADR-0024)")
	return "", nil
}

// runReplaySession starts the synthetic clip's replay through the API and
// waits for its terminal snapshot. dev_trace requires the developer role at
// the start route, so the start rides that role (the operator's 403 is the
// documented refusal, not a bug the test papers over).
func runReplaySession(t *testing.T, e *sessionEnv, devTrace bool) string {
	t.Helper()
	payload := startPayload(e.profileID, "replay", mustAbs(t, sessSyntheticDir), "conservative", devTrace)
	rec := e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/sessions", payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	id := decodeJSON(t, rec)["session_id"].(string)
	fin := e.waitTerminal(id)
	if fin["state"] != "stopped" {
		t.Fatalf("terminal state = %v, want stopped (reason %v)", fin["state"], fin["reason"])
	}
	return id
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

// TestDevFixtureRecord is the fixture recording half of docs/spec/08: the
// trace's observation, expectation, audio and samples land as one directory
// under the external fixture root, with the manifest hashing every entry.
func TestDevFixtureRecord(t *testing.T) {
	fixtureRoot := t.TempDir()
	t.Setenv(dolphin.EnvFixtureDir, fixtureRoot)
	e := newSessionEnvWired(t, true)
	sessionID := runReplaySession(t, e, true)
	traceID, commit := firstCommitTrace(t, e, sessionID)

	// The route is developer-only: the operator's click is refused at the gate.
	rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/dev/fixtures", `{"trace_id":"`+traceID+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	rec = e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/fixtures", `{"trace_id":"`+traceID+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("record = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	fixture := body["fixture"].(map[string]any)
	dir, _ := fixture["dir"].(string)
	if !strings.HasPrefix(dir, fixtureRoot) {
		t.Fatalf("fixture dir %q is not under the configured fixture root %q", dir, fixtureRoot)
	}
	if fixture["kind"] != "trace-fixture" || fixture["trace_id"] != traceID || fixture["session_id"] != sessionID {
		t.Fatalf("the fixture identity is wrong: %v", fixture)
	}
	entries := fixture["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("the fixture names no entries")
	}

	// manifest.json on disk: the integrity claim. Its rows hash the bytes as
	// written, and the manifest cannot carry its own hash.
	manifestRaw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatalf("the manifest is not JSON: %v", err)
	}
	if manifest["kind"] != "trace-fixture" || manifest["id"] != fixture["id"] {
		t.Fatalf("the manifest names a different fixture: %v", manifest)
	}
	source := manifest["source"].(map[string]any)
	if source["trace_id"] != traceID || source["session_id"] != sessionID {
		t.Fatalf("the manifest's correlation block is wrong: %v", source)
	}
	manifestEntries := manifest["entries"].([]any)
	if len(manifestEntries) != len(entries) {
		t.Fatalf("the manifest has %d entries, the response names %d", len(manifestEntries), len(entries))
	}
	byPath := map[string]bool{}
	for _, raw := range manifestEntries {
		row := raw.(map[string]any)
		path, _ := row["path"].(string)
		sha, _ := row["sha256"].(string)
		size := int64(row["size"].(float64))
		byPath[path] = true
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatalf("the manifest names a missing file %s: %v", path, err)
		}
		if int64(len(data)) != size {
			t.Fatalf("%s: size %d, want %d", path, len(data), size)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != sha {
			t.Fatalf("%s: the manifest's hash does not match the file", path)
		}
	}
	if byPath["manifest.json"] {
		t.Fatal("the manifest may not carry its own hash")
	}

	// observation.json: the stored observation the re-run replays.
	obsRaw, err := os.ReadFile(filepath.Join(dir, "observation.json"))
	if err != nil {
		t.Fatalf("read observation: %v", err)
	}
	var observation map[string]any
	if err := json.Unmarshal(obsRaw, &observation); err != nil {
		t.Fatalf("the observation is not JSON: %v", err)
	}
	if observation["raw_text"] == nil || observation["raw_text"] == "" {
		t.Fatalf("the observation carries no reading: %v", observation)
	}

	// expected.json: the recorded run's candidates, commit and (wired) decision.
	expectedRaw, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}
	var expected map[string]any
	if err := json.Unmarshal(expectedRaw, &expected); err != nil {
		t.Fatalf("the expectation is not JSON: %v", err)
	}
	if expected["candidates"] == nil {
		t.Fatal("the expectation holds no recorded ranking")
	}
	commitRow, _ := expected["commit"].(map[string]any)
	commitData, _ := commit["data"].(map[string]any)
	if commitRow == nil || commitRow["message_id"] != commitData["message_id"] {
		t.Fatalf("the expectation's commit is wrong: %v (the run committed %v)", expected["commit"], commitData["message_id"])
	}

	// The dev-trace samples: the run stored them (dev trace on), so the
	// fixture carries the frame and the crop.
	if !byPath["frames/frame.jpg"] || !byPath["frames/crop.png"] {
		t.Fatalf("the fixture holds no frame or crop sample: %v", byPath)
	}
	// The utterance's audio: the wired voice speaks the committed line, so
	// the cache holds the WAV the session played and the fixture carries it.
	if !byPath["speech/utterance.wav"] {
		t.Fatalf("the fixture holds no utterance audio: %v", byPath)
	}
	wav, err := os.ReadFile(filepath.Join(dir, "speech", "utterance.wav"))
	if err != nil {
		t.Fatalf("read the utterance wav: %v", err)
	}
	if !bytes.HasPrefix(wav, []byte("RIFF")) {
		t.Fatal("the utterance audio is not a WAV")
	}
	// corrections.json is the (possibly empty) label list, JSON either way.
	if _, err := os.ReadFile(filepath.Join(dir, "corrections.json")); err != nil {
		t.Fatalf("read corrections: %v", err)
	}

	// The failure shapes: an unknown trace is a 404, and a trace with no
	// stored observation is a 422 (the global events carry no observation).
	unknown := e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/fixtures", "{\"trace_id\":\"0123456789ABCDEFGHIJKLMNOP\"}")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown trace = %d, want 404 (body %s)", unknown.Code, unknown.Body.String())
	}
	emptyTrace := games.NewID()
	if _, err := e.mem.Append(context.Background(), events.Envelope{
		SchemaVersion: 1,
		EventID:       games.NewID(),
		TraceID:       emptyTrace,
		Name:          events.SessionStopped,
		Data:          json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("append a bare trace event: %v", err)
	}
	bare := e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/fixtures", `{"trace_id":"`+emptyTrace+`"}`)
	if bare.Code != http.StatusUnprocessableEntity {
		t.Fatalf("observation-less trace = %d, want 422 (body %s)", bare.Code, bare.Body.String())
	}
}

// TestDevTraceRerun is the re-run half: the stored observation is re-ranked
// by the current build's matcher over the run's own immutable revision, and
// the answer carries the recorded side, the re-ranked side and the diff.
func TestDevTraceRerun(t *testing.T) {
	e := newSessionEnv(t)
	sessionID := runReplaySession(t, e, false)
	traceID, _ := firstCommitTrace(t, e, sessionID)

	rec := e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/traces/"+traceID+"/rerun", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rerun = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["trace_id"] != traceID || body["session_id"] != sessionID {
		t.Fatalf("the re-run names the wrong trace or session: %v", body)
	}
	observation := body["observation"].(map[string]any)
	if observation["raw_text"] == nil || observation["raw_text"] == "" {
		t.Fatalf("the re-run names no stored observation: %v", observation)
	}
	current := body["current"].(map[string]any)
	candidates := current["candidates"].([]any)
	if len(candidates) == 0 {
		t.Fatal("the re-run returned no candidates")
	}
	// The truth OCR is deterministic and the revision is immutable, so the
	// re-run of the same observation over the same corpus is the same
	// ranking: the winner holds and the diff is empty of movement.
	previous, hasPrevious := body["previous"].(map[string]any)
	if !hasPrevious {
		t.Fatal("the recorded side is null; the trace's ranking is persisted")
	}
	prevCands := previous["candidates"].([]any)
	top, _ := candidates[0].(map[string]any)
	prevTop, _ := prevCands[0].(map[string]any)
	if top["message_id"] != prevTop["message_id"] {
		t.Fatalf("the re-run's winner %v moved from the recorded %v", top["message_id"], prevTop["message_id"])
	}
	if body["changed"] != false {
		t.Fatalf("an unchanged re-run reports changed = %v", body["changed"])
	}
	diff := body["diff"].([]any)
	if len(diff) == 0 {
		t.Fatal("the re-run reports no diff rows")
	}
	for _, raw := range diff {
		row := raw.(map[string]any)
		if row["delta"] != nil {
			if d, _ := row["delta"].(float64); d > 1e-9 || d < -1e-9 {
				t.Fatalf("the unchanged re-run moved a probability: %v", row)
			}
		}
	}
	decision := body["decision"].(map[string]any)
	if decision["provider_involved"] != false || decision["note"] == "" || decision["note"] == nil {
		t.Fatalf("the decision note is missing or wrong: %v", decision)
	}

	// The failure shapes: an unknown trace is a 404; the operator is refused.
	unknown := e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/dev/traces/"+"0123456789ABCDEFGHIJKLMNOPQRST"+"/rerun", "")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown trace = %d, want 404 (body %s)", unknown.Code, unknown.Body.String())
	}
	forbidden := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/dev/traces/"+traceID+"/rerun", "")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("operator = %d, want 403 (body %s)", forbidden.Code, forbidden.Body.String())
	}
}

// TestDevTraceEvidence is the export half: one tar.gz with the redacted event
// log, the samples, the audio and the hash manifest.
func TestDevTraceEvidence(t *testing.T) {
	e := newSessionEnvWired(t, true)
	sessionID := runReplaySession(t, e, true)
	traceID, _ := firstCommitTrace(t, e, sessionID)

	rec := e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/dev/traces/"+traceID+"/evidence", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("evidence = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("content type = %q, want application/gzip", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, traceID) {
		t.Fatalf("content disposition = %q, want an attachment named for the trace", cd)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache control = %q, want no-store", cc)
	}

	gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("open the gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next tar entry: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		files[hdr.Name] = data
	}
	gz.Close()

	// The bundle's shape: the manifest, the event log, the labels, the
	// samples and the audio (the run stored them: dev trace on, wired voice
	// speaks). The samples ride the assets/ tree the bundle documents.
	for _, want := range []string{"manifest.json", "events.json", "corrections.json", "assets/frame.jpg", "assets/crop.png", "speech.json", "speech/utterance.wav"} {
		if _, ok := files[want]; !ok {
			t.Errorf("the bundle holds no %s (entries: %v)", want, keys(files))
		}
	}
	// events.json: every envelope of the trace, and the redaction applied —
	// the request ids and the gameplay text survive, the rules' targets do
	// not (the walker is unit-tested; this is the bundle carrying it).
	var eventsDoc struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(files["events.json"], &eventsDoc); err != nil {
		t.Fatalf("events.json is not JSON: %v", err)
	}
	if len(eventsDoc.Events) == 0 {
		t.Fatal("the bundle's event log is empty")
	}
	var foundCommit, foundCandidates bool
	for _, ev := range eventsDoc.Events {
		switch ev["name"] {
		case "message.committed":
			foundCommit = true
		case "message.candidates":
			foundCandidates = true
		}
	}
	if !foundCommit || !foundCandidates {
		t.Fatalf("the bundle's event log holds no commit or ranking: %v", eventsDoc.Events)
	}
	// The manifest's hashes match the bundle's bytes.
	var manifest map[string]any
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatalf("the manifest is not JSON: %v", err)
	}
	if src, _ := manifest["source"].(map[string]any); src["trace_id"] != traceID {
		t.Fatalf("the manifest names a different trace: %v", manifest["source"])
	}
	for _, raw := range manifest["entries"].([]any) {
		row := raw.(map[string]any)
		path, _ := row["path"].(string)
		sha, _ := row["sha256"].(string)
		data, ok := files[path]
		if !ok {
			t.Fatalf("the manifest names a missing bundle entry %s", path)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != sha {
			t.Fatalf("the manifest's hash for %s does not match the bundle", path)
		}
	}

	// The failure shapes: an unknown trace is a 404, the operator is refused.
	unknown := e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/dev/traces/"+"0123456789ABCDEFGHIJKLMNOPQRST"+"/evidence", "")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown trace = %d, want 404 (body %s)", unknown.Code, unknown.Body.String())
	}
	forbidden := e.send(auth.RoleOperator, http.MethodGet, "/api/v1/dev/traces/"+traceID+"/evidence", "")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("operator = %d, want 403 (body %s)", forbidden.Code, forbidden.Body.String())
	}
}

// TestDevSpeechAudio is the utterance audio half: the cache key from the
// trace's speech.ready answers with the stored WAV, its SHA-256 as the ETag.
func TestDevSpeechAudio(t *testing.T) {
	e := newSessionEnvWired(t, true)
	sessionID := runReplaySession(t, e, false)
	traceID, _ := firstCommitTrace(t, e, sessionID)

	cacheKey := ""
	for _, ev := range devEventsOf(t, e, sessionID) {
		if ev["name"] != "speech.ready" {
			continue
		}
		if tid, _ := ev["trace_id"].(string); tid != traceID {
			continue
		}
		data, _ := ev["data"].(map[string]any)
		cacheKey, _ = data["cache_key"].(string)
		break
	}
	if cacheKey == "" {
		t.Fatal("the utterance's speech.ready carries no cache key")
	}

	rec := e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/dev/speech/audio?cache_key="+cacheKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audio = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("content type = %q, want audio/wav", ct)
	}
	wav := rec.Body.Bytes()
	if !bytes.HasPrefix(wav, []byte("RIFF")) {
		t.Fatal("the audio is not a WAV")
	}
	sum := sha256.Sum256(wav)
	if rec.Header().Get("ETag") != `"`+hex.EncodeToString(sum[:])+`"` {
		t.Fatalf("etag = %q, want the WAV's SHA-256 quoted", rec.Header().Get("ETag"))
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache control = %q, want no-store", cc)
	}

	// The failure shapes: a valid key the cache does not hold is a 404, and a
	// key that is not a cache key is a 400. A key is 64 lowercase hex chars with
	// no checksum, so flipping the last char names a different, well-formed
	// entry the cache holds no bytes for.
	mutated := []byte(cacheKey)
	if mutated[len(mutated)-1] == '0' {
		mutated[len(mutated)-1] = '1'
	} else {
		mutated[len(mutated)-1] = '0'
	}
	missing := e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/dev/speech/audio?cache_key="+string(mutated), "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing key = %d, want 404 (body %s)", missing.Code, missing.Body.String())
	}
	bad := e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/dev/speech/audio?cache_key=not-a-key", "")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid key = %d, want 400 (body %s)", bad.Code, bad.Body.String())
	}
	// The operator is refused at the gate.
	forbidden := e.send(auth.RoleOperator, http.MethodGet, "/api/v1/dev/speech/audio?cache_key="+cacheKey, "")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("operator = %d, want 403 (body %s)", forbidden.Code, forbidden.Body.String())
	}
}

// TestRedactJSONValue is the evidence bundle's redaction rule in isolation:
// credentials, URLs and hosts, private IPs, home paths and emails are
// scrubbed; the gameplay text and the structure pass through unchanged.
func TestRedactJSONValue(t *testing.T) {
	in := map[string]any{
		"game_text":   "The harbour bell rang twice, and no one looked up.",
		"request_id":  "0123456789ABCDEFGHIJKLMNOPQRST",
		"endpoint":    "https://gateway.example.internal:8443/v1?token=sk_live_secret_value",
		"auth":        "Authorization: Bearer sk_live_secret_value",
		"ip":          "the gateway answered from 10.20.0.7",
		"home":        "log file at /home/david/data/session.json",
		"mail":        "ask marina@example.com",
		"nested":      []any{"Bearer sk_test_abcdefgh1234", map[string]any{"url": "http://192.168.1.5/x"}},
		"count":       3,
		"probability": 0.97,
	}
	out := redactJSONValue(in).(map[string]any)
	if out["game_text"] != "The harbour bell rang twice, and no one looked up." {
		t.Errorf("the gameplay text must survive: %v", out["game_text"])
	}
	if out["request_id"] != "0123456789ABCDEFGHIJKLMNOPQRST" {
		t.Errorf("the correlation id must survive: %v", out["request_id"])
	}
	if out["count"] != 3 || out["probability"] != 0.97 {
		t.Errorf("numbers pass through: %v / %v", out["count"], out["probability"])
	}
	bundle := string(marshalIndent(out))
	for _, leak := range []string{"sk_live_secret_value", "sk_test_abcdefgh1234", "gateway.example.internal", "10.20.0.7", "/home/david", "marina@example.com", "192.168.1.5"} {
		if strings.Contains(bundle, leak) {
			t.Errorf("the redaction leaked %q into: %s", leak, bundle)
		}
	}
	// JSON's marshal escapes the marker's angle brackets; the on-disk bytes
	// carry the escaped form, which is what the assertion must match.
	for _, redacted := range []string{`\u003credacted\u003e`, `\u003credacted-host\u003e`} {
		if !strings.Contains(bundle, redacted) {
			t.Errorf("the redaction marker %q is absent from: %s", redacted, bundle)
		}
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
