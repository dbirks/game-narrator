package api

// The generic kind voices over HTTP (issue #51, ADR-0185): the coverage read
// and the one-click create-missing run, on the wired voice subsystem with a
// scripted TTS whose takes are synthetic tones (110 / 220 / 320 Hz — male,
// female, child range). The pitch retry is exercised end to end: an out-of
// class first take costs the kind one retry with the stronger brief.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/cast"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// kindFakeTTS answers Design with the queued tone for the request's
// instruction (the derived brief — plain prose under the cap, so the brief
// itself), one queue entry per call. The Base checkpoint methods come from
// the embedded fixed-tone fake (the kind run never calls them).
type kindFakeTTS struct {
	apiFakeTTS
	byInstruction map[string][]float64
}

func (f *kindFakeTTS) Design(_ context.Context, req tts.DesignRequest) (tts.Audio, error) {
	queue, ok := f.byInstruction[req.Instructions]
	if !ok || len(queue) == 0 {
		return tts.Audio{}, errNoTake
	}
	freq := queue[0]
	f.byInstruction[req.Instructions] = queue[1:]
	return tts.Audio{
		WAV:        toneWAV(freq, 24000, 48000),
		SampleRate: 24000, Channels: 1, BitsPerSample: 16,
		Duration: 2 * time.Second, Model: "fake-voicedesign", Latency: 7 * time.Millisecond,
		Origin: "voice_design model=fake-voicedesign task_type=VoiceDesign",
	}, nil
}

var errNoTake = errNoTakeSentinel{}

type errNoTakeSentinel struct{}

func (errNoTakeSentinel) Error() string { return "no take queued for this instruction" }

// kindWorld wires the voice subsystem the way newVoiceTestWorld does, with
// the queued TTS in place of the fixed-tone fake.
func kindWorld(t *testing.T, st *store.Store, ttsClient *kindFakeTTS) *voices.Service {
	t.Helper()
	assetStore, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	svc := &voices.Service{
		Store: st, Assets: assetStore, Design: ttsClient, Base: ttsClient,
		Registry:      newAPIFakeRegistry(),
		Speech:        speech.New(assetStore, speech.NewMemIndex(), 4),
		BaseModelID:   "fake-base",
		BackendDigest: "test-preview-v1",
		Language:      "English",
	}
	if err := voices.New(svc); err != nil {
		t.Fatalf("voices.New: %v", err)
	}
	return svc
}

// queueToneFor queues one design answer (the synthetic tone's frequency) for
// a kind's fixed brief on one attempt: the instruction the voice service
// derives from the brief is the queue key.
func queueToneFor(t *testing.T, ttsClient *kindFakeTTS, kind string, attempt int, freq float64) {
	t.Helper()
	key := voices.DesignInstruction(cast.BriefFor(kind, attempt), 0)
	ttsClient.byInstruction[key] = append(ttsClient.byInstruction[key], freq)
}

func kindRowsFrom(t *testing.T, rec *httptest.ResponseRecorder) []cast.KindRow {
	t.Helper()
	var body struct {
		Kinds []cast.KindRow `json:"kinds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("kinds body: %v (%s)", err, rec.Body.String())
	}
	return body.Kinds
}

func TestKindVoicesCoverageAndCreateMissing(t *testing.T) {
	a := newTestAuth(t)
	ttsClient := &kindFakeTTS{byInstruction: map[string][]float64{}}
	svc := kindWorld(t, a.st, ttsClient)
	h := a.handler(WithVoices(svc, &captureQueue{}))

	// 1. A fresh appliance: every kind is missing, nothing measured.
	rec := doAuthedJSON(t, h, http.MethodGet, "/api/v1/voices/kinds", a.bearer(auth.RoleViewer), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("coverage: %d %s", rec.Code, rec.Body.String())
	}
	recordContractResponse(t, http.MethodGet, "/api/v1/voices/kinds", rec.Code,
		rec.Header().Get("Content-Type"), rec.Body.Bytes())
	rows := kindRowsFrom(t, rec)
	if len(rows) != len(cast.Kinds) {
		t.Fatalf("coverage has %d rows, want %d", len(rows), len(cast.Kinds))
	}
	for _, r := range rows {
		if r.State != cast.KindStateMissing || r.Measured != "unmeasured" {
			t.Errorf("%s = state %q measured %q, want missing/unmeasured", r.Kind, r.State, r.Measured)
		}
	}

	// 2. The one-click run: every kind lands in class on its first take.
	queueToneFor(t, ttsClient, "adult_male", 1, 110)
	queueToneFor(t, ttsClient, "adult_female", 1, 220)
	queueToneFor(t, ttsClient, "elder_male", 1, 110)
	queueToneFor(t, ttsClient, "elder_female", 1, 220)
	queueToneFor(t, ttsClient, "boy", 1, 320)
	queueToneFor(t, ttsClient, "girl", 1, 220)
	queueToneFor(t, ttsClient, "creature", 1, 220)

	rec = doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/kinds/create-missing", a.bearer(auth.RoleCurator), map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("create-missing: %d %s", rec.Code, rec.Body.String())
	}
	recordContractResponse(t, http.MethodPost, "/api/v1/voices/kinds/create-missing", rec.Code,
		rec.Header().Get("Content-Type"), rec.Body.Bytes())
	rows = kindRowsFrom(t, rec)
	byKind := map[string]cast.KindRow{}
	for _, r := range rows {
		byKind[r.Kind] = r
	}
	wantClass := map[string]string{
		"adult_male": cast.ClassMale, "elder_male": cast.ClassMale,
		"adult_female": cast.ClassFemale, "elder_female": cast.ClassFemale,
		"girl": cast.ClassFemale, "boy": cast.ClassChild,
	}
	for kind, class := range wantClass {
		got := byKind[kind]
		if got.State != cast.KindStatePending || got.Attempts != 1 {
			t.Errorf("%s = state %q attempts %d, want pending on the first take", kind, got.State, got.Attempts)
		}
		if got.Measured != class {
			t.Errorf("%s measured %q, want %q", kind, got.Measured, class)
		}
		if got.VoiceID == "" {
			t.Errorf("%s: the pending take has no voice id", kind)
		}
	}
	if got := byKind["creature"]; got.State != cast.KindStatePending {
		t.Errorf("creature = state %q, want pending (no class to fail)", got.State)
	}

	// 3. The coverage now reads pending, with the measured class per kind.
	rec = doAuthedJSON(t, h, http.MethodGet, "/api/v1/voices/kinds", a.bearer(auth.RoleViewer), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("coverage after run: %d %s", rec.Code, rec.Body.String())
	}
	for _, r := range kindRowsFrom(t, rec) {
		if r.State != cast.KindStatePending {
			t.Errorf("%s coverage state = %q, want pending", r.Kind, r.State)
		}
	}
}

func TestKindVoicesCreateMissingRetriesOutOfClass(t *testing.T) {
	a := newTestAuth(t)
	ttsClient := &kindFakeTTS{byInstruction: map[string][]float64{}}
	svc := kindWorld(t, a.st, ttsClient)
	h := a.handler(WithVoices(svc, &captureQueue{}))

	// adult_male's first take is 220 Hz (out of class); the retry lands at
	// 110 Hz. The other six kinds each need their in-class take, or the run
	// spends its three attempts on them too.
	queueToneFor(t, ttsClient, "adult_male", 1, 220)
	queueToneFor(t, ttsClient, "adult_male", 2, 110)
	queueToneFor(t, ttsClient, "adult_female", 1, 220)
	queueToneFor(t, ttsClient, "elder_male", 1, 110)
	queueToneFor(t, ttsClient, "elder_female", 1, 220)
	queueToneFor(t, ttsClient, "boy", 1, 320)
	queueToneFor(t, ttsClient, "girl", 1, 220)
	queueToneFor(t, ttsClient, "creature", 1, 220)

	rec := doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/kinds/create-missing", a.bearer(auth.RoleCurator), map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("create-missing: %d %s", rec.Code, rec.Body.String())
	}
	byKind := map[string]cast.KindRow{}
	for _, r := range kindRowsFrom(t, rec) {
		byKind[r.Kind] = r
	}
	male := byKind["adult_male"]
	if male.State != cast.KindStatePending || male.Attempts != 2 {
		t.Fatalf("adult_male = state %q attempts %d, want pending after one retry", male.State, male.Attempts)
	}
	if male.Measured != cast.ClassMale {
		t.Errorf("adult_male measured %q, want %q", male.Measured, cast.ClassMale)
	}
}

func TestKindVoicesCreateMissingRequiresCurator(t *testing.T) {
	a := newTestAuth(t)
	ttsClient := &kindFakeTTS{byInstruction: map[string][]float64{}}
	svc := kindWorld(t, a.st, ttsClient)
	h := a.handler(WithVoices(svc, &captureQueue{}))

	rec := doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/kinds/create-missing", a.bearer(auth.RoleViewer), map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer create-missing: %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

func TestKindVoicesWithoutSubsystem(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler() // no WithVoices

	for _, tc := range []struct {
		method, path string
		role         auth.Role
	}{
		{http.MethodGet, "/api/v1/voices/kinds", auth.RoleViewer},
		{http.MethodPost, "/api/v1/voices/kinds/create-missing", auth.RoleCurator},
	} {
		rec := doAuthedJSON(t, h, tc.method, tc.path, a.bearer(tc.role), nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: %d, want 503 (%s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
