package api

// The auto-cast flow over HTTP (ADR-0109, UX-03/UX-03d): the 202 audition job
// created with a profile_id ends approved with the profile-default consent
// attributed to the profile (no click), a failed QA keeps the take pending,
// and the one-step undo restores the previous take. The catalogue, the voice
// service and the durable queue run on the same store, the way the binary
// wires them; the design clips and the ASR read-back are scripted (synthetic
// stubs, AGENTS.md rule 3).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/voices"
)

// apiFakeASR is a scripted ASR read-back: it returns whatever the test queued,
// so the QA gate's pass/fail and the auto-cast's refusal are testable without
// a gateway.
type apiFakeASR struct {
	mu      sync.Mutex
	calls   int
	results []speech.Transcription
	err     error
}

func (f *apiFakeASR) TranscribeText(_ context.Context, _ []byte, _ string) (speech.Transcription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return speech.Transcription{}, f.err
	}
	if len(f.results) == 0 {
		return speech.Transcription{}, errors.New("apiFakeASR: script exhausted")
	}
	r := f.results[0]
	f.results = f.results[1:]
	if r.Model == "" {
		r.Model = "fake-asr"
	}
	if r.RequestID == "" {
		r.RequestID = "fake-asr-" + strconv.Itoa(f.calls)
	}
	return r, nil
}

// passLines returns one transcript per synthetic line, matching it exactly:
// the QA bound is trivially met, so every take the test fills passes.
func passLines(n int) []speech.Transcription {
	out := make([]speech.Transcription, 0, n)
	for _, line := range voices.SyntheticAuditionLines[:n] {
		out = append(out, speech.Transcription{Text: line})
	}
	return out
}

// autoCastEnv is the catalog env with the voice subsystem wired onto the same
// store the catalogue writes to — the binary's wiring, test-scale.
type autoCastEnv struct {
	e     *catalogEnv
	world *voiceTestWorld
	h     http.Handler
}

func newAutoCastEnv(t *testing.T) *autoCastEnv {
	t.Helper()
	e := newCatalogEnv(t)
	world := newVoiceTestWorld(t, e.a.st)
	world.svc.CastProfiles = voices.NewStoreCastProfiles(e.a.st)
	h := e.a.handler(
		WithStore(e.st),
		WithJobs(e.js),
		WithEvents(e.hub),
		WithVoices(world.svc, world.queue),
	)
	return &autoCastEnv{e: e, world: world, h: h}
}

// rosterName returns the first character on the profile's roster: the cast's
// characters come from the roster, and the test needs one real name to use.
func (a *autoCastEnv) rosterName(t *testing.T, profileID string) string {
	t.Helper()
	chars, err := a.e.st.ListCharacters(a.e.ctx, profileID)
	if err != nil || len(chars) == 0 {
		t.Fatalf("the draft's roster is empty (err=%v): the fixture import must create characters", err)
	}
	return chars[0].Name
}

// runQueuedVoiceJob runs the job the API enqueued exactly the way the runner
// would: the registered handler on the enqueued payload, and the decoded
// durable result.
func (a *autoCastEnv) runQueuedVoiceJob(t *testing.T, wantCount int) voices.JobResult {
	t.Helper()
	if len(a.world.queue.params) == 0 {
		t.Fatalf("the queue received no voice job")
	}
	p := a.world.queue.params[len(a.world.queue.params)-1]
	var payload voices.JobPayload
	if err := json.Unmarshal(p.Payload, &payload); err != nil {
		t.Fatalf("job payload %s: %v", p.Payload, err)
	}
	if payload.Count != wantCount {
		t.Fatalf("job payload count = %d, want %d (the enqueue-time read chose it)", payload.Count, wantCount)
	}
	raw, err := voices.JobHandler(a.world.svc)(a.e.ctx,
		jobs.Job{ID: "job-autocast", Kind: voices.JobKind, Payload: p.Payload},
		func(pct float64, msg string) {})
	if err != nil {
		t.Fatalf("job handler: %v", err)
	}
	var result voices.JobResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("job result: %v", err)
	}
	return result
}

// do runs one request against the combined handler and records the 2xx
// contract response the way catalogEnv does.
func (a *autoCastEnv) do(t *testing.T, role auth.Role, method, target, body string, kv ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := jsonRequest(method, target, body)
	if body != "" {
		req.Header.Set("Content-Type", ContentTypeJSON)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		req.Header.Set(kv[i], kv[i+1])
	}
	req.Header.Set("Authorization", "Bearer "+a.e.a.bearer(role))
	rec := doReq(t, a.h, req)
	if rec.Code >= 200 && rec.Code < 300 {
		recordContractResponse(t, method, target, rec.Code,
			rec.Header().Get("Content-Type"), rec.Body.Bytes())
	}
	return rec
}

// TestVoiceAutoCastOverHTTP is the product promise on the wire: one take per
// cast character, auto-approved with the profile's default consent, the
// regeneration one step from being undone.
func TestVoiceAutoCastOverHTTP(t *testing.T) {
	a := newAutoCastEnv(t)
	_, _, profileID, _ := newDraft(t, a.e, "autocast-flow", "acflow")
	character := a.rosterName(t, profileID)
	// One take per cast run: each run auditions the first synthetic line, so
	// the script carries one matching read-back per run.
	a.world.svc.ASR = &apiFakeASR{results: append(passLines(1), passLines(1)...)}

	// 1. The cast run: profile_id given, count left unset. The dial is on at
	//    enqueue, so the default count is ONE take, and the response carries
	//    the cast context the UI ties the 202 back to.
	rec := a.do(t, auth.RoleCurator, http.MethodPost, "/api/v1/voices/auditions",
		`{"character":`+strconv.Quote(character)+`,
		 "brief":"a calm, original character voice",
		 "profile_id":`+strconv.Quote(profileID)+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create cast auditions = %d (body %s)", rec.Code, rec.Body.String())
	}
	var created voiceAuditionsCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create response: %v", err)
	}
	if created.CastProfileID != profileID {
		t.Fatalf("cast_profile_id = %q, want the request's profile", created.CastProfileID)
	}
	if created.AutoCast == nil || !*created.AutoCast {
		t.Fatalf("auto_cast = %v, want the dial the API read at enqueue (on)", created.AutoCast)
	}
	result := a.runQueuedVoiceJob(t, 1)
	if !result.AutoApproved || result.State != "approved" {
		t.Fatalf("job result = %+v, want auto-approved and approved", result)
	}
	if result.CastProfileID != profileID {
		t.Fatalf("result cast_profile_id = %q, want the cast context", result.CastProfileID)
	}

	// 2. The take is frozen with the consent the product promised: the
	//    profile's default attributed to the profile, and the flow — not a
	//    click — as the approver.
	rec = a.do(t, auth.RoleCurator, http.MethodGet, "/api/v1/voices/"+created.VoiceID, "")
	var detail voiceDetailDTO
	if rec.Code != http.StatusOK {
		t.Fatalf("detail = %d (body %s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail body: %v", err)
	}
	if detail.State != "approved" || len(detail.Auditions) != 1 {
		t.Fatalf("detail = state %s with %d auditions, want approved with exactly one take", detail.State, len(detail.Auditions))
	}
	if detail.ConsentProvenance != voices.AutoCastConsent(profileID) {
		t.Fatalf("consent_provenance = %q, want %q", detail.ConsentProvenance, voices.AutoCastConsent(profileID))
	}
	if detail.ApprovedBy != voices.AutoCaster {
		t.Fatalf("approved_by = %q, want %q (the flow, not a click)", detail.ApprovedBy, voices.AutoCaster)
	}

	// 3. The list shows exactly one revision of the character: the cast made
	//    one take, and it is done — nothing pending, nothing to review.
	rec = a.do(t, auth.RoleCurator, http.MethodGet, "/api/v1/voices", "")
	var list voiceListDTO
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d (body %s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if len(list.Voices) != 1 || list.Voices[0].State != "approved" {
		t.Fatalf("list = %+v, want exactly one approved revision", list.Voices)
	}
	firstID := list.Voices[0].ID

	// 4. Regeneration is a new revision that auto-casts the same way — and
	//    now there are two takes, the second of them current.
	rec = a.do(t, auth.RoleCurator, http.MethodPost, "/api/v1/voices/auditions",
		`{"character":`+strconv.Quote(character)+`,
		 "brief":"the same character, a different take",
		 "profile_id":`+strconv.Quote(profileID)+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("regenerate = %d (body %s)", rec.Code, rec.Body.String())
	}
	var second voiceAuditionsCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("regenerate response: %v", err)
	}
	result = a.runQueuedVoiceJob(t, 1)
	if !result.AutoApproved {
		t.Fatalf("regenerated run = %+v, want auto-approved too", result)
	}
	rec = a.do(t, auth.RoleCurator, http.MethodGet, "/api/v1/voices", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if len(list.Voices) != 2 || list.Voices[0].ID != second.VoiceID {
		t.Fatalf("list = %+v, want two revisions, the regenerated one first (current)", list.Voices)
	}

	// 5. The one-step undo: keep the old take. The new take retires, the
	//    previous one becomes current again, and nothing is deleted.
	rec = a.do(t, auth.RoleCurator, http.MethodPost, "/api/v1/voices/"+second.VoiceID+"/undo", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("undo = %d (body %s)", rec.Code, rec.Body.String())
	}
	var undone voiceRevisionDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &undone); err != nil {
		t.Fatalf("undo body: %v", err)
	}
	if undone.State != "retired" {
		t.Fatalf("the undone take = %s, want retired", undone.State)
	}
	rec = a.do(t, auth.RoleCurator, http.MethodGet, "/api/v1/voices/"+firstID, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("first take body: %v", err)
	}
	if detail.State != "approved" || detail.ConsentProvenance == "" {
		t.Fatalf("the restored take = state %s consent %q, want approved with its frozen consent", detail.State, detail.ConsentProvenance)
	}
	// The old take's audio is still retrievable: nothing was deleted.
	audio, _, err := a.world.assets.Get(a.e.ctx, detail.ReferenceSHA256)
	if err != nil {
		t.Fatalf("the restored take's reference = %v: undo must not delete the old audio", err)
	}
	audio.Close()

	// 6. The undo is one step: a second undo is refused, and so is undoing a
	//    take with no previous take to restore.
	rec = a.do(t, auth.RoleCurator, http.MethodPost, "/api/v1/voices/"+second.VoiceID+"/undo", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second undo = %d (body %s), want 409 (one step by construction)", rec.Code, rec.Body.String())
	}
	rec = a.do(t, auth.RoleCurator, http.MethodPost, "/api/v1/voices/"+firstID+"/undo", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("undo with no previous take = %d (body %s), want 409", rec.Code, rec.Body.String())
	}
	// An unknown revision is a 404, not a 409: it is not a take at all.
	rec = a.do(t, auth.RoleCurator, http.MethodPost, "/api/v1/voices/01ARZ3NDEKTSW4Y00000000000/undo", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("undo of an unknown revision = %d (body %s), want 404", rec.Code, rec.Body.String())
	}
}

// TestVoiceAutoCastRefusesForeignContexts: the cast's consent can never be
// attributed to a profile that does not exist or that does not own the
// character, and a dial that is off at job end leaves the take pending review
// (the honest "toggled after the run" outcome).
func TestVoiceAutoCastRefusesForeignContexts(t *testing.T) {
	a := newAutoCastEnv(t)
	_, _, profileID, _ := newDraft(t, a.e, "autocast-foreign", "acforeign")
	character := a.rosterName(t, profileID)
	a.world.svc.ASR = &apiFakeASR{results: passLines(len(voices.SyntheticAuditionLines))}

	// A profile that does not exist: the run cannot be tied to it.
	rec := a.do(t, auth.RoleCurator, http.MethodPost, "/api/v1/voices/auditions",
		`{"character":`+strconv.Quote(character)+`,"brief":"b","profile_id":"01ARZ3NDEKTSW4Y00000000000"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown profile = %d (body %s), want 404", rec.Code, rec.Body.String())
	}

	// A character the profile does not carry: a voice name is a roster name,
	// and a consent attributed to a profile that never had the character is
	// exactly the fabrication docs/spec/14 exists to prevent.
	rec = a.do(t, auth.RoleCurator, http.MethodPost, "/api/v1/voices/auditions",
		`{"character":"a name this profile does not carry","brief":"b","profile_id":`+strconv.Quote(profileID)+`}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("character not on the roster = %d (body %s), want 404", rec.Code, rec.Body.String())
	}
	// Neither refusal leaked a draft revision: the checks run before any
	// voice row exists.
	rec = a.do(t, auth.RoleCurator, http.MethodGet, "/api/v1/voices", "")
	var list voiceListDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Voices) != 0 {
		t.Fatalf("list after the refusals = %+v (err=%v), want no voice rows", list.Voices, err)
	}

	// Dial off: the enqueue-time read only chose the count (an explicit count
	// is honoured), and the job's re-read finds the dial off — the take stays
	// pending review, with the reason on the job result.
	if err := a.e.st.SetProfileAutoCast(a.e.ctx, profileID, false); err != nil {
		t.Fatalf("turn the dial off: %v", err)
	}
	rec = a.do(t, auth.RoleCurator, http.MethodPost, "/api/v1/voices/auditions",
		`{"character":`+strconv.Quote(character)+`,"brief":"b","count":2,"profile_id":`+strconv.Quote(profileID)+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create with the dial off = %d (body %s)", rec.Code, rec.Body.String())
	}
	var created voiceAuditionsCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create response: %v", err)
	}
	if created.AutoCast == nil || *created.AutoCast {
		t.Fatalf("auto_cast = %v, want the off dial as the API read it", created.AutoCast)
	}
	result := a.runQueuedVoiceJob(t, 2)
	if result.AutoApproved || result.State != "auditioning" {
		t.Fatalf("dial off at job end: result = %+v, want pending (auditioning)", result)
	}
	if result.AutoApprovalNote == "" {
		t.Fatalf("the pending take carries no reason: the UI would show a stuck run")
	}
}

// TestVoiceAuditionsWithoutProfileIdStayManual: the manual flow is unchanged
// — no cast context on the job, the default count is the full synthetic set,
// and the take waits for a human approval.
func TestVoiceAuditionsWithoutProfileIdStayManual(t *testing.T) {
	a := newAutoCastEnv(t)
	_, _, profileID, _ := newDraft(t, a.e, "autocast-manual", "acmanual")
	character := a.rosterName(t, profileID)
	a.world.svc.ASR = &apiFakeASR{results: passLines(len(voices.SyntheticAuditionLines))}

	rec := a.do(t, auth.RoleCurator, http.MethodPost, "/api/v1/voices/auditions",
		`{"character":`+strconv.Quote(character)+`,"brief":"a manual run"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create manual auditions = %d (body %s)", rec.Code, rec.Body.String())
	}
	var created voiceAuditionsCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create response: %v", err)
	}
	if created.CastProfileID != "" || created.AutoCast != nil {
		t.Fatalf("manual run carries a cast context (%+v): the manual flow must be unchanged", created)
	}
	result := a.runQueuedVoiceJob(t, len(voices.SyntheticAuditionLines))
	if result.CastProfileID != "" || result.AutoApproved {
		t.Fatalf("manual run's result = %+v, want no cast context and no auto-approval", result)
	}
	if result.State != "auditioning" {
		t.Fatalf("manual run = %s, want auditioning (a human approves)", result.State)
	}
	_ = profileID
}
