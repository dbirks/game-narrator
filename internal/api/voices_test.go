package api

// The voice workflow over HTTP (docs/spec/08 "APIs"): the 202 audition job,
// the audition audio, the consent-gated approve, the idempotent register, the
// preview miss-then-hit (X-Cache), and the honest 503 when the subsystem or
// the queue is not wired. Role denials are the matrix's business
// (TestRoleMatrixOverEveryRoute); here the gate is open and the handlers'
// behaviour is under test.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// ── fakes ───────────────────────────────────────────────────────────────────

// apiFakeTTS answers Design and Base with deterministic, valid PCM16 WAVs and
// counts the calls: a preview hit that made a provider call is a test failure.
type apiFakeTTS struct {
	mu          sync.Mutex
	designCalls int
	baseCalls   int
}

func (f *apiFakeTTS) Design(_ context.Context, req tts.DesignRequest) (tts.Audio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.designCalls++
	return tts.Audio{
		WAV:        toneWAV(180+float64(f.designCalls)*40, 24000, 48000),
		SampleRate: 24000, Channels: 1, BitsPerSample: 16,
		Duration: 2 * time.Second, Model: "fake-voicedesign", Latency: 7 * time.Millisecond,
		Origin: "voice_design model=fake-voicedesign task_type=VoiceDesign",
	}, nil
}

func (f *apiFakeTTS) Generate(_ context.Context, req tts.GenerateRequest) (tts.Audio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.baseCalls++
	return tts.Audio{
		WAV:        toneWAV(300, 24000, 48000),
		SampleRate: 24000, Channels: 1, BitsPerSample: 16,
		Duration: 2 * time.Second, Model: "fake-base", Latency: 7 * time.Millisecond,
		Origin: "voice_clone model=fake-base voice=" + req.Voice,
	}, nil
}

func (f *apiFakeTTS) GenerateWithReference(_ context.Context, req tts.GenerateRequest, refAudio []byte, _ string) (tts.Audio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.baseCalls++
	return tts.Audio{
		WAV:        toneWAV(600, 24000, 48000),
		SampleRate: 24000, Channels: 1, BitsPerSample: 16,
		Duration: 2 * time.Second, Model: "fake-base", Latency: 7 * time.Millisecond,
		Origin: "voice_clone model=fake-base voice=" + req.Voice + " inline_ref_audio=true",
	}, nil
}

// toneWAV renders a valid, non-silent, unclipped PCM16 mono WAV of the given
// sample count.
func toneWAV(freqHz float64, rate, n int) []byte {
	data := make([]byte, n*2)
	for i := 0; i < n; i++ {
		s := int16(9000 * math.Sin(2*math.Pi*freqHz*float64(i)/float64(rate)))
		data[2*i] = byte(s)
		data[2*i+1] = byte(s >> 8)
	}
	out := []byte("RIFF")
	out = appendU32(out, 36+uint32(len(data)))
	out = append(out, "WAVEfmt "...)
	out = appendU32(out, 16)
	out = appendU16(out, 1)
	out = appendU16(out, 1)
	out = appendU32(out, uint32(rate))
	out = appendU32(out, uint32(rate*2))
	out = appendU16(out, 2)
	out = appendU16(out, 16)
	out = append(out, "data"...)
	out = appendU32(out, uint32(len(data)))
	return append(out, data...)
}

func appendU16(b []byte, v uint16) []byte { return append(b, byte(v), byte(v>>8)) }
func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// apiFakeRegistry is an in-memory registry with the identity rule: a name's
// digest8 must equal the reference's digest8, and present names are not
// re-uploaded.
type apiFakeRegistry struct {
	mu     sync.Mutex
	voices map[string][]byte
}

func newAPIFakeRegistry() *apiFakeRegistry {
	return &apiFakeRegistry{voices: map[string][]byte{}}
}

func (r *apiFakeRegistry) List(_ context.Context) (tts.ListResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for n, b := range r.voices {
		if len(b) > 0 {
			names = append(names, n)
		}
	}
	return tts.ListResult{Voices: names}, nil
}

func (r *apiFakeRegistry) Ensure(_ context.Context, required []tts.Voice) (tts.EnsureReport, error) {
	var rep tts.EnsureReport
	listed, _ := r.List(context.Background())
	have := map[string]bool{}
	for _, n := range listed.Voices {
		have[n] = true
	}
	for _, v := range required {
		if len(v.Name) < 9 || v.Name[len(v.Name)-8:] != tts.Digest8(v.RefAudio) {
			rep.Conflicts = append(rep.Conflicts, tts.Conflict{Name: v.Name, Reason: "name digest mismatch"})
			continue
		}
		if have[v.Name] {
			rep.Present = append(rep.Present, v.Name)
			continue
		}
		r.mu.Lock()
		r.voices[v.Name] = v.RefAudio
		r.mu.Unlock()
		rep.Registered = append(rep.Registered, v.Name)
	}
	return rep, nil
}

// captureQueue records the job and answers with a row, standing in for the
// durable queue the API enqueues onto.
type captureQueue struct {
	mu     sync.Mutex
	params []store.EnqueueParams
}

func (q *captureQueue) Enqueue(_ context.Context, p store.EnqueueParams) (*store.Job, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.params = append(q.params, p)
	return &store.Job{ID: fmt.Sprintf("job-%d", len(q.params))}, true, nil
}

// voiceTestWorld wires the voice subsystem the way cmd/game-narrator does:
// real store and asset store, scripted providers, in-memory speech index.
type voiceTestWorld struct {
	svc    *voices.Service
	queue  *captureQueue
	tts    *apiFakeTTS
	reg    *apiFakeRegistry
	assets *assets.Local
}

func newVoiceTestWorld(t *testing.T, st *store.Store) *voiceTestWorld {
	t.Helper()
	assetStore, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	fake := &apiFakeTTS{}
	reg := newAPIFakeRegistry()
	svc := &voices.Service{
		Store: st, Assets: assetStore, Design: fake, Base: fake, Registry: reg,
		Speech:        speech.New(assetStore, speech.NewMemIndex(), 4),
		BaseModelID:   "fake-base",
		BackendDigest: "test-preview-v1",
		Language:      "English",
	}
	if err := voices.New(svc); err != nil {
		t.Fatalf("voices.New: %v", err)
	}
	return &voiceTestWorld{svc: svc, reg: reg, tts: fake, assets: assetStore, queue: &captureQueue{}}
}

// ── the flow ────────────────────────────────────────────────────────────────

func TestVoiceWorkflowOverHTTP(t *testing.T) {
	a := newTestAuth(t)
	world := newVoiceTestWorld(t, a.st)
	h := a.handler(WithVoices(world.svc, world.queue))

	curator := a.bearer(auth.RoleCurator)
	operator := a.bearer(auth.RoleOperator)

	// 1. A fresh appliance lists no voices (not an error).
	rec := doAuthedJSON(t, h, http.MethodGet, "/api/v1/voices", curator, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list voiceListDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Voices) != 0 {
		t.Fatalf("empty list: %v %s", err, rec.Body.String())
	}

	// 2. Create auditions: 202, a job id, and the draft revision.
	reqBody := map[string]any{"character": "hero", "brief": "a warm, unhurried narrator", "count": 3}
	rec = doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/auditions", curator, reqBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create auditions: %d %s", rec.Code, rec.Body.String())
	}
	var created voiceAuditionsCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create response: %v", err)
	}
	if created.JobID == "" || created.VoiceID == "" || created.State != store.VoiceStateDraft {
		t.Fatalf("create response incomplete: %+v", created)
	}
	if len(world.queue.params) != 1 || world.queue.params[0].Kind != voices.JobKind || world.queue.params[0].TraceID != created.VoiceID {
		t.Fatalf("queue received %+v, want one voice.auditions job traced to the revision", world.queue.params)
	}
	var payload voices.JobPayload
	if err := json.Unmarshal(world.queue.params[0].Payload, &payload); err != nil || payload.ProfileID != created.VoiceID {
		t.Fatalf("job payload %s: %v", world.queue.params[0].Payload, err)
	}

	// The job runs (simulated here by calling the handler's fill directly:
	// the queue is what schedules it in production).
	if _, err := world.svc.CreateAuditionsOnDraft(context.Background(), created.VoiceID); err != nil {
		t.Fatalf("fill draft: %v", err)
	}

	// 3. The detail shows the auditioning revision with three clips.
	rec = doAuthedJSON(t, h, http.MethodGet, "/api/v1/voices/"+created.VoiceID, curator, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail voiceDetailDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail body: %v", err)
	}
	if detail.State != store.VoiceStateAuditioning || len(detail.Auditions) != 3 {
		t.Fatalf("detail = state %s with %d auditions, want auditioning/3", detail.State, len(detail.Auditions))
	}
	for i, au := range detail.Auditions {
		if au.Transcript == "" || au.SHA256 == "" {
			t.Fatalf("audition %d incomplete: %+v", i, au)
		}
	}

	// 4. The audition audio is the clip's bytes, with the identity as ETag.
	au0 := detail.Auditions[0]
	areq := jsonRequest(http.MethodGet, "/api/v1/voices/"+created.VoiceID+"/auditions/"+au0.ID, "")
	areq.Header.Set("Authorization", "Bearer "+curator)
	arec := doReq(t, h, areq)
	if arec.Code != http.StatusOK || arec.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("audition audio: %d %q", arec.Code, arec.Header().Get("Content-Type"))
	}
	if arec.Header().Get("ETag") != `"`+au0.SHA256+`"` {
		t.Fatalf("audition ETag = %q, want the clip's sha256", arec.Header().Get("ETag"))
	}
	wav, _ := io.ReadAll(arec.Body)
	if len(wav) == 0 {
		t.Fatal("audition audio is empty")
	}
	if _, err := tts.ValidateAudio(wav); err != nil {
		t.Fatalf("audition audio is not a valid WAV: %v", err)
	}

	// 5. Approve: empty consent is a 400 CONSENT_REQUIRED; with consent it
	// freezes the revision.
	bad := doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/"+created.VoiceID+"/approve", curator,
		map[string]any{"audition_id": au0.ID, "consent": ""})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("approve empty consent: %d %s, want 400", bad.Code, bad.Body.String())
	}
	var badDoc codedProblem
	_ = json.Unmarshal(bad.Body.Bytes(), &badDoc)
	if badDoc.Code != "CONSENT_REQUIRED" {
		t.Fatalf("approve empty consent code = %q, want CONSENT_REQUIRED", badDoc.Code)
	}

	rec = doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/"+created.VoiceID+"/approve", curator,
		map[string]any{"audition_id": au0.ID, "consent": "original_synthetic_design (api test)"})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	var approved voiceRevisionDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &approved); err != nil {
		t.Fatalf("approve body: %v", err)
	}
	if approved.State != store.VoiceStateApproved || approved.Fingerprint == "" ||
		approved.ReferenceSHA256 == "" || approved.ConsentProvenance == "" || approved.ApprovedBy == "" {
		t.Fatalf("approved revision incomplete: %+v", approved)
	}

	// A second approve is a 409: the state machine does not move back.
	again := doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/"+created.VoiceID+"/approve", curator,
		map[string]any{"audition_id": au0.ID, "consent": "again"})
	if again.Code != http.StatusConflict {
		t.Fatalf("re-approve: %d %s, want 409", again.Code, again.Body.String())
	}

	// 6. Register (operator): the voice lands on the instance, ready.
	rec = doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/"+created.VoiceID+"/register", operator, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	var regRow voiceRegistrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &regRow); err != nil {
		t.Fatalf("register body: %v", err)
	}
	if regRow.State != store.VoiceRegReady || regRow.RegisteredName == "" {
		t.Fatalf("register row = %+v, want ready with a name", regRow)
	}
	// Idempotent: a second pass reports present and changes nothing.
	rec = doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/"+created.VoiceID+"/register", operator, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-register: %d %s", rec.Code, rec.Body.String())
	}
	var regAgain voiceRegistrationResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &regAgain)
	if regAgain.State != store.VoiceRegReady || regAgain.RegisteredName != regRow.RegisteredName {
		t.Fatalf("re-register changed state: %+v", regAgain)
	}

	// The registrations list agrees.
	rec = doAuthedJSON(t, h, http.MethodGet, "/api/v1/voices/"+created.VoiceID+"/registrations", curator, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("registrations: %d %s", rec.Code, rec.Body.String())
	}
	var regs voiceRegistrationsDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &regs); err != nil || len(regs.Registrations) != 1 ||
		regs.Registrations[0].State != store.VoiceRegReady {
		t.Fatalf("registrations = %+v, %v", regs, err)
	}

	// 7. Preview: miss, then hit — the hit makes no provider call.
	line := "Preview line for the workflow test."
	rec = doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/"+created.VoiceID+"/preview", curator, map[string]any{"text": line})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("preview content-type = %q", ct)
	}
	if got := rec.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("first preview X-Cache = %q, want MISS", got)
	}
	missWAV, _ := io.ReadAll(rec.Body)
	if _, err := tts.ValidateAudio(missWAV); err != nil {
		t.Fatalf("preview audio invalid: %v", err)
	}
	if world.tts.baseCalls != 1 {
		t.Fatalf("base calls after miss = %d, want 1", world.tts.baseCalls)
	}

	rec = doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/"+created.VoiceID+"/preview", curator, map[string]any{"text": line})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview 2: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("second preview X-Cache = %q, want HIT (docs/spec/07 step 7)", got)
	}
	if world.tts.baseCalls != 1 {
		t.Fatalf("base calls after hit = %d, want still 1 (a hit makes no provider call)", world.tts.baseCalls)
	}
	hitWAV, _ := io.ReadAll(rec.Body)
	if len(hitWAV) != len(missWAV) || string(hitWAV) != string(missWAV) {
		t.Fatal("hit returned different bytes than the miss committed")
	}

	// A different line is a fresh miss.
	rec = doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/"+created.VoiceID+"/preview", curator, map[string]any{"text": "A different line."})
	if rec.Code != http.StatusOK || rec.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("new line preview: %d %q, want 200 MISS", rec.Code, rec.Header().Get("X-Cache"))
	}
	if world.tts.baseCalls != 2 {
		t.Fatalf("base calls after a new line = %d, want 2", world.tts.baseCalls)
	}
}

func TestVoiceUnwiredAnswers503(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler() // no WithVoices
	rec := doAuthedJSON(t, h, http.MethodGet, "/api/v1/voices", a.bearer(auth.RoleCurator), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired voices: %d %s, want 503 (an empty list would claim no voices exist)", rec.Code, rec.Body.String())
	}
	rec = doAuthedJSON(t, h, http.MethodPost, "/api/v1/voices/auditions", a.bearer(auth.RoleCurator),
		map[string]any{"character": "x", "brief": "y"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired audition create: %d, want 503", rec.Code)
	}
}

// doAuthedJSON runs one JSON request with a bearer token and returns the
// recorder; nil body is no body (GETs and no-body POSTs).
func doAuthedJSON(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == nil {
		req = jsonRequest(method, path, "")
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		req = jsonRequest(method, path, string(raw))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return doReq(t, h, req)
}
