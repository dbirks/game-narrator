package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/providers"
)

// fakeRegistry is an in-memory model of the Base service's /v1/audio/voices
// surface: it answers the listing, the multipart upload and the delete route
// exactly as the openapi contract specifies (ADR-0022), and it enforces the
// immutable registration policy so Ensure's conflict path is exercised for
// real.
type fakeRegistry struct {
	mu       sync.Mutex
	srv      *httptest.Server
	voices   map[string]recordedVoice
	builtIn  []string
	requests []recordedRequest
	// failNextRegisterWith, when non-zero, makes the next POST answer with
	// that status and message (one-shot).
	failNextRegisterWith int
	failNextRegisterMsg  string
}

type recordedVoice struct {
	name        string
	consent     string
	refText     string
	audio       []byte
	contentType string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	fr := &fakeRegistry{
		voices:  make(map[string]recordedVoice),
		builtIn: []string{"default", "home-test-narrator"},
	}
	fr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
		fr.mu.Lock()
		fr.requests = append(fr.requests, rec)
		fr.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/audio/voices":
			fr.mu.Lock()
			names := append([]string{}, fr.builtIn...)
			var up []map[string]any
			for n, v := range fr.voices {
				names = append(names, n)
				up = append(up, map[string]any{
					"name": v.name, "consent": v.consent, "created_at": 1,
					"file_size": len(v.audio), "mime_type": v.contentType,
					"ref_text": v.refText, "embedding_source": "upload",
				})
			}
			fr.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"voices": names, "uploaded_voices": up})
		case r.Method == http.MethodPost && r.URL.Path == "/audio/voices":
			fr.mu.Lock()
			fail := fr.failNextRegisterWith
			failMsg := fr.failNextRegisterMsg
			fr.failNextRegisterWith = 0
			fr.mu.Unlock()
			if fail != 0 {
				writeJSON(w, fail, map[string]any{"error": map[string]any{"message": failMsg}})
				return
			}
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "multipart/form-data" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "expected multipart/form-data"}})
				return
			}
			fields, files, fileCT, perr := parseMultipart(rec.Body, r.Header.Get("Content-Type"))
			if perr != "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": perr}})
				return
			}
			var v recordedVoice
			v.name = fields["name"]
			v.consent = fields["consent"]
			v.refText = fields["ref_text"]
			v.audio = files["audio_sample"]
			v.contentType = fileCT["audio_sample"]
			fr.mu.Lock()
			_, exists := fr.voices[v.name]
			if exists {
				// The immutable registration policy rejects collisions.
				fr.mu.Unlock()
				writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"message": fmt.Sprintf("voice %q already registered", v.name)}})
				return
			}
			fr.voices[v.name] = v
			fr.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "voice": map[string]any{
				"name": v.name, "consent": v.consent, "created_at": 1,
				"file_size": len(v.audio), "mime_type": v.contentType, "ref_text": v.refText,
			}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/audio/voices/"):
			name := strings.TrimPrefix(r.URL.Path, "/audio/voices/")
			fr.mu.Lock()
			_, ok := fr.voices[name]
			delete(fr.voices, name)
			fr.mu.Unlock()
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": fmt.Sprintf("voice %q not found", name)}})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": fmt.Sprintf("voice %q deleted", name)})
		default:
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "unsupported path " + r.URL.Path}})
		}
	}))
	t.Cleanup(fr.srv.Close)
	return fr
}

func (fr *fakeRegistry) url() string { return fr.srv.URL }

func (fr *fakeRegistry) count() int {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return len(fr.requests)
}

func (fr *fakeRegistry) last() recordedRequest {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.requests) == 0 {
		return recordedRequest{}
	}
	return fr.requests[len(fr.requests)-1]
}

func (fr *fakeRegistry) countPosts() int {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	var n int
	for _, rec := range fr.requests {
		if rec.Method == "POST" {
			n++
		}
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

// parseMultipart parses a recorded multipart body the way the Base service
// will. It goes through http.Request.ParseMultipartForm deliberately: that
// is the path a real HTTP server takes to parse the upload, and it enforces
// the 1 MiB size limit exactly as the server would.
func parseMultipart(body []byte, contentType string) (fields map[string]string, files map[string][]byte, fileCT map[string]string, errText string) {
	fields, files, fileCT = map[string]string{}, map[string][]byte{}, map[string]string{}
	req, err := http.NewRequest(http.MethodPost, "http://registry.invalid/audio/voices", bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil, err.Error()
	}
	req.Header.Set("Content-Type", contentType)
	if err := req.ParseMultipartForm(1 << 20); err != nil {
		return nil, nil, nil, err.Error()
	}
	for k, vs := range req.MultipartForm.Value {
		if len(vs) > 0 {
			fields[k] = vs[0]
		}
	}
	for k, ps := range req.MultipartForm.File {
		if len(ps) == 0 {
			continue
		}
		f, err := ps[0].Open()
		if err != nil {
			return nil, nil, nil, err.Error()
		}
		b, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, nil, nil, err.Error()
		}
		files[k] = b
		fileCT[k] = ps[0].Header.Get("Content-Type")
	}
	return fields, files, fileCT, ""
}

func testRegistry(t *testing.T) (*Registry, *fakeRegistry) {
	t.Helper()
	fr := newFakeRegistry(t)
	r, err := NewRegistry(fr.url(), "reg-token")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r, fr
}

func testVoice(name string, ref []byte) Voice {
	return Voice{
		Name:     name,
		RefAudio: ref,
		RefText:  "The keeper counts three lanterns.",
		Consent:  "original_synthetic_design (game-narrator automated test)",
	}
}

// TestRegisterRefusesWithoutConsent pins docs/spec/14: the consent record is
// required and passed truthfully; an empty one is refused client-side, before
// any request reaches the server.
func TestRegisterRefusesWithoutConsent(t *testing.T) {
	r, fr := testRegistry(t)
	v := testVoice("gn_test_0123abcd", fixtureWAV(t))
	v.Consent = "   "
	_, err := r.Register(context.Background(), v)
	if !errors.Is(err, ErrConsentRequired) {
		t.Fatalf("error = %v, want ErrConsentRequired", err)
	}
	if fr.count() != 0 {
		t.Errorf("requests = %d, want 0 (refusal must be client-side)", fr.count())
	}
}

// TestRegisterSendsTheMultipartContract pins the verified wire shape:
// audio_sample file + name + consent + ref_text fields.
func TestRegisterSendsTheMultipartContract(t *testing.T) {
	r, fr := testRegistry(t)
	up, err := r.Register(context.Background(), testVoice("gn_test_0123abcd", fixtureWAV(t)))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if up.Name != "gn_test_0123abcd" {
		t.Errorf("echoed name = %q", up.Name)
	}
	rec := fr.last()
	if rec.Method != "POST" || rec.Path != "/audio/voices" {
		t.Fatalf("request = %s %s", rec.Method, rec.Path)
	}
	if got := rec.Header.Get("Authorization"); got != "Bearer reg-token" {
		t.Errorf("Authorization = %q", got)
	}
	mt, _, err := mime.ParseMediaType(rec.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		t.Fatalf("Content-Type = %q", rec.Header.Get("Content-Type"))
	}
	fields, files, fileCT, perr := parseMultipart(rec.Body, rec.Header.Get("Content-Type"))
	if perr != "" {
		t.Fatalf("parse multipart: %s", perr)
	}
	audio := files["audio_sample"]
	audioCT := fileCT["audio_sample"]
	if fields["name"] != "gn_test_0123abcd" {
		t.Errorf("name field = %q", fields["name"])
	}
	if fields["consent"] == "" {
		t.Error("consent field missing")
	}
	if fields["ref_text"] != "The keeper counts three lanterns." {
		t.Errorf("ref_text field = %q", fields["ref_text"])
	}
	if string(audio) != string(fixtureWAV(t)) {
		t.Error("audio_sample differs from the reference WAV")
	}
	if audioCT != "audio/wav" {
		t.Errorf("audio_sample Content-Type = %q", audioCT)
	}
}

// TestRegisterConflictIsExplicit: a collision under the immutable policy is
// ErrVoiceConflict, not a retryable transport failure.
func TestRegisterConflictIsExplicit(t *testing.T) {
	r, _ := testRegistry(t)
	v := testVoice("gn_test_0123abcd", fixtureWAV(t))
	if _, err := r.Register(context.Background(), v); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// The fake registry enforces the immutable policy: a second upload of the
	// same name is a 409.
	_, err := r.Register(context.Background(), v)
	if !errors.Is(err, ErrVoiceConflict) {
		t.Fatalf("second Register error = %v, want ErrVoiceConflict", err)
	}
	var pe *providers.Error
	if errors.As(err, &pe) && pe.Retryable {
		t.Error("a collision must not be marked retryable")
	}
}

// TestRemoveGuardsTheNamespace: the client refuses to touch names outside the
// gn_ namespace, and maps a missing gn_ voice to voice_missing.
func TestRemoveGuardsTheNamespace(t *testing.T) {
	r, fr := testRegistry(t)
	if err := r.Remove(context.Background(), "default"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("Remove(default) = %v, want ErrInvalidName", err)
	}
	if err := r.Remove(context.Background(), "home-test-narrator"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("Remove(home-test-narrator) = %v, want ErrInvalidName", err)
	}
	if fr.count() != 0 {
		t.Errorf("requests = %d, want 0 (guard is client-side)", fr.count())
	}
	v := testVoice("gn_test_0123abcd", fixtureWAV(t))
	if _, err := r.Register(context.Background(), v); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Remove(context.Background(), v.Name); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	err := r.Remove(context.Background(), v.Name)
	if err == nil {
		t.Fatal("removing an absent voice must fail")
	}
	var pe *providers.Error
	if !errors.As(err, &pe) || pe.Kind != providers.KindVoiceMissing {
		t.Errorf("second Remove error = %v, want kind %s", err, providers.KindVoiceMissing)
	}
}

// TestEnsureReconcilesWithoutTouchingPresentVoices pins the reconcile
// algorithm: one listing, registers only what is missing, reports what is
// present, and never re-registers a name whose content contradicts the
// digest embedded in the name.
func TestEnsureReconcilesWithoutTouchingPresentVoices(t *testing.T) {
	r, fr := testRegistry(t)
	refA := sineWAV(24000, 1, 220, 0.4)
	refB := sineWAV(24000, 1, 330, 0.4)

	nameA, err := MakeName("ab12cd34", "narrator", 1, refA)
	if err != nil {
		t.Fatalf("MakeName A: %v", err)
	}
	nameB, err := MakeName("ab12cd34", "narrator", 2, refB)
	if err != nil {
		t.Fatalf("MakeName B: %v", err)
	}
	// Pre-register A on the fake instance so Ensure must report it present.
	if _, err := r.Register(context.Background(), testVoice(nameA, refA)); err != nil {
		t.Fatalf("pre-register A: %v", err)
	}
	baselinePosts := fr.countPosts()

	// A conflicting voice: the name carries refB's digest but the payload is
	// refA — the digest mismatch must be reported, not registered.
	nameC, err := MakeName("ab12cd34", "narrator", 3, refB)
	if err != nil {
		t.Fatalf("MakeName C: %v", err)
	}
	conflict := testVoice(nameC, refA)

	report, err := r.Ensure(context.Background(), []Voice{testVoice(nameA, refA), testVoice(nameB, refB), conflict})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(report.Present) != 1 || report.Present[0] != nameA {
		t.Errorf("present = %v, want [%s]", report.Present, nameA)
	}
	if len(report.Registered) != 1 || report.Registered[0] != nameB {
		t.Errorf("registered = %v, want [%s]", report.Registered, nameB)
	}
	if len(report.Conflicts) != 1 || report.Conflicts[0].Name != nameC {
		t.Errorf("conflicts = %v, want one for %s", report.Conflicts, nameC)
	}
	if got := fr.countPosts() - baselinePosts; got != 1 {
		t.Errorf("new POSTs = %d, want 1 (present and conflicting voices must not be re-registered)", got)
	}
	// Idempotence: a second Ensure must register nothing.
	report2, err := r.Ensure(context.Background(), []Voice{testVoice(nameA, refA), testVoice(nameB, refB)})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if len(report2.Registered) != 0 || len(report2.Present) != 2 {
		t.Errorf("second Ensure = %+v, want all present, none registered", report2)
	}
}

// TestEnsureFailsFastOnInvalidInput: a bad name or missing consent anywhere
// in the required set is rejected before any mutation.
func TestEnsureFailsFastOnInvalidInput(t *testing.T) {
	r, fr := testRegistry(t)
	ref := fixtureWAV(t)
	bad := testVoice("friendly-name", ref)
	if _, err := r.Ensure(context.Background(), []Voice{bad}); !errors.Is(err, ErrInvalidName) {
		t.Errorf("Ensure = %v, want ErrInvalidName", err)
	}
	consentless := testVoice("gn_test_0123abcd", ref)
	consentless.Consent = ""
	if _, err := r.Ensure(context.Background(), []Voice{consentless}); !errors.Is(err, ErrConsentRequired) {
		t.Errorf("Ensure = %v, want ErrConsentRequired", err)
	}
	if fr.count() != 0 {
		t.Errorf("requests = %d, want 0 (fail-fast is before any I/O)", fr.count())
	}
}

// TestMakeNameAndValidateName pin the immutable naming scheme: same content
// always names identically, different content never shares a name, and
// anything outside the gn_ namespace is refused.
func TestMakeNameAndValidateName(t *testing.T) {
	refA := sineWAV(24000, 1, 220, 0.4)
	refB := sineWAV(24000, 1, 330, 0.4)

	n1, err := MakeName("ab12cd34", "narrator", 1, refA)
	if err != nil {
		t.Fatalf("MakeName: %v", err)
	}
	want := "gn_ab12cd34_narrator_v1_" + Digest8(refA)
	if n1 != want {
		t.Errorf("name = %q, want %q", n1, want)
	}
	// Same content, same name — the idempotence key of Ensure.
	n1b, _ := MakeName("ab12cd34", "narrator", 1, refA)
	if n1b != n1 {
		t.Errorf("same content renamed: %q vs %q", n1b, n1)
	}
	// Different content, different name — a revision change is visible.
	n2, _ := MakeName("ab12cd34", "narrator", 1, refB)
	if n2 == n1 {
		t.Error("different content produced the same name")
	}
	if !strings.HasSuffix(n2, "_"+Digest8(refB)) {
		t.Errorf("name %q does not carry the content digest", n2)
	}

	if err := ValidateName(n1); err != nil {
		t.Errorf("ValidateName(%q) = %v", n1, err)
	}
	if err := ValidateName("gn_test_0123abcd"); err != nil {
		t.Errorf("ValidateName(test name) = %v", err)
	}
	for _, bad := range []string{
		"", "default", "home-test-narrator", "gn_synthetic_narrator_v1", // spec-era example without the digest suffix
		"gn_x", "gn_ab12cd34_narrator_v1_0123abcde", // 7 digest chars
		"GN_ab12cd34_narrator_v1_0123abcd", // uppercase
		"gn_ab12cd34_narrator_v0_0123abcd", // v0
		"gn_ab12cd34_NARRATOR_v1_0123abcd",
		"/etc/passwd", "gn_test_0123abcde",
	} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) accepted a name outside the scheme", bad)
		}
	}
	for _, bad := range []struct {
		profile, char string
		rev           int
	}{
		{"xyz", "narrator", 1},
		{"ab12cd34", "Narrator", 1},
		{"ab12cd34", "n", 0},
	} {
		if _, err := MakeName(bad.profile, bad.char, bad.rev, refA); err == nil {
			t.Errorf("MakeName(%q,%q,%d) accepted invalid input", bad.profile, bad.char, bad.rev)
		}
	}
	if _, err := MakeName("ab12cd34", "narrator", 1, nil); err == nil {
		t.Error("MakeName accepted empty reference audio")
	}
}

// TestListParsesUploadedVoices pins the listing shape the Base service
// returns (openapi verified): built-in and uploaded names in "voices", the
// metadata array in "uploaded_voices".
func TestListParsesUploadedVoices(t *testing.T) {
	r, _ := testRegistry(t)
	if _, err := r.Register(context.Background(), testVoice("gn_test_0123abcd", fixtureWAV(t))); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, n := range got.Voices {
		if n == "gn_test_0123abcd" {
			found = true
		}
	}
	if !found {
		t.Errorf("List.Voices = %v, want the registered name", got.Voices)
	}
	if len(got.Uploaded) != 1 || got.Uploaded[0].Name != "gn_test_0123abcd" {
		t.Errorf("Uploaded = %+v", got.Uploaded)
	}
	if got.Uploaded[0].RefText != "The keeper counts three lanterns." {
		t.Errorf("ref_text = %q", got.Uploaded[0].RefText)
	}
}
