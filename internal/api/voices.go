package api

// /api/v1/voices — the voice setup workflow (docs/spec/08 "APIs": voice list,
// audition creation (202 job), audition audio, approve, register, preview,
// registrations; docs/spec/07 voice setup; docs/spec/14 consent).
//
// Roles come from the scope table (internal/auth/policy.go), not from here:
// reads of which voices exist are viewer; curation (auditions, approve,
// preview) is curator; touching the provider registry is operator. The
// contract test asserts each operation's x-required-role matches the table, so
// the two cannot drift.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// voiceDeps wires the voice subsystem into the server. The queue is what the
// audition-creation job enqueues onto; a nil queue answers 503 on that one
// route (no other voice route needs it) so a build without the durable queue
// still serves the rest of the workflow.
type voiceDeps struct {
	Service *voices.Service
	Queue   JobQueue
}

// WithVoices wires the voice workflow (docs/spec/08 "APIs").
func WithVoices(svc *voices.Service, queue JobQueue) Option {
	return func(s *Server) {
		s.voices = &voiceDeps{Service: svc, Queue: queue}
	}
}

// voiceRequired answers 503 when the voice subsystem is not wired: an endpoint
// that returned an empty voice list from a process without the workflow would
// read as "no voices configured" rather than "this build does not do voices".
func (s *Server) voiceRequired(w http.ResponseWriter, r *http.Request) bool {
	if s.voices == nil || s.voices.Service == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "voices-unavailable", "SUBSYSTEM_UNAVAILABLE",
			"Service Unavailable", "the voice workflow is not configured in this process")
		return false
	}
	return true
}

// ── DTOs ────────────────────────────────────────────────────────────────────

// voiceRevisionDTO is the wire shape of one voice revision. Content fields that
// exist only once approved (reference, fingerprint, consent) are omitted while
// draft: an absent member means "not frozen yet", which is the honest state.
type voiceRevisionDTO struct {
	ID                string                 `json:"id"`
	Character         string                 `json:"character"`
	Revision          int                    `json:"revision"`
	State             string                 `json:"state"`
	DesignInstruction string                 `json:"design_instruction"`
	Language          string                 `json:"language,omitempty"`
	RefTranscript     string                 `json:"ref_transcript,omitempty"`
	ReferenceSHA256   string                 `json:"reference_sha256,omitempty"`
	Fingerprint       string                 `json:"fingerprint,omitempty"`
	ConsentProvenance string                 `json:"consent_provenance,omitempty"`
	ApprovedBy        string                 `json:"approved_by,omitempty"`
	ApprovedAt        string                 `json:"approved_at,omitempty"`
	AuditionIDs       []string               `json:"audition_ids"`
	CreatedAt         string                 `json:"created_at"`
	UpdatedAt         string                 `json:"updated_at"`
	Registrations     []voiceRegistrationDTO `json:"registrations,omitempty"`
}

// voiceDetailDTO is GET /api/v1/voices/{id}: the revision plus its audition
// rows — the full state the voice page renders.
type voiceDetailDTO struct {
	voiceRevisionDTO
	Auditions []voiceAuditionDTO `json:"auditions"`
}

type voiceAuditionDTO struct {
	ID         string `json:"id"`
	Index      int    `json:"index"`
	Transcript string `json:"transcript"`
	ModelID    string `json:"model_id"`
	SHA256     string `json:"sha256"`
	DurationMS int64  `json:"duration_ms"`
	LatencyMS  int64  `json:"latency_ms,omitempty"`
	Origin     string `json:"origin,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	CreatedAt  string `json:"created_at"`
	// ASR QA verdict (FR-10, ADR 0061). qa_state is "unknown" (not run — the
	// role is unconfigured or the endpoint was down), "passed", or "failed"
	// (flagged: the UI shows the badge with qa_wer and qa_transcript as the
	// review evidence, and approval is refused until a human reviews).
	QAState      string  `json:"qa_state"`
	QAWER        float64 `json:"qa_wer,omitempty"`
	QACER        float64 `json:"qa_cer,omitempty"`
	QATranscript string  `json:"qa_transcript,omitempty"`
	QAModel      string  `json:"qa_model,omitempty"`
	QARequestID  string  `json:"qa_request_id,omitempty"`
	QACheckedAt  string  `json:"qa_checked_at,omitempty"`
}

type voiceRegistrationDTO struct {
	InstanceID     string `json:"instance_id"`
	RegisteredName string `json:"registered_name"`
	RemoteDigest   string `json:"remote_digest,omitempty"`
	State          string `json:"state"`
	Err            string `json:"err,omitempty"`
	LastVerifiedAt string `json:"last_verified_at,omitempty"`
}

type voiceRegistrationsDTO struct {
	VoiceID       string                 `json:"voice_id"`
	Registrations []voiceRegistrationDTO `json:"registrations"`
}

type voiceRegistrationResponse struct {
	InstanceID     string `json:"instance_id"`
	RegisteredName string `json:"registered_name"`
	State          string `json:"state"`
	Err            string `json:"err,omitempty"`
}

type voiceListDTO struct {
	Voices []voiceRevisionDTO `json:"voices"`
}

type voiceAuditionsCreateRequest struct {
	Character string `json:"character"`
	Brief     string `json:"brief"`
	Count     int    `json:"count,omitempty"`
}

type voiceAuditionsCreateResponse struct {
	JobID     string `json:"job_id"`
	VoiceID   string `json:"voice_id"`
	Character string `json:"character"`
	Revision  int    `json:"revision"`
	State     string `json:"state"`
}

type voiceApproveRequest struct {
	AuditionID string `json:"audition_id"`
	Consent    string `json:"consent"`
}

type voicePreviewRequest struct {
	Text string `json:"text"`
}

// ── converters ──────────────────────────────────────────────────────────────

func toVoiceRevisionDTO(p store.VoiceProfile, regs []store.VoiceRegistration) voiceRevisionDTO {
	ids := p.AuditionIDs
	if ids == nil {
		ids = []string{}
	}
	dto := voiceRevisionDTO{
		ID: p.ID, Character: p.Character, Revision: p.Revision, State: p.State,
		DesignInstruction: p.DesignInstruction, Language: p.Language,
		RefTranscript: p.RefTranscript, ReferenceSHA256: p.ReferenceAssetID,
		Fingerprint: p.Fingerprint, ConsentProvenance: p.ConsentProvenance,
		ApprovedBy:  p.ApprovedBy,
		AuditionIDs: ids,
		CreatedAt:   fmtTime(p.CreatedAt), UpdatedAt: fmtTime(p.UpdatedAt),
	}
	if p.ApprovedAt != nil {
		dto.ApprovedAt = fmtTime(*p.ApprovedAt)
	}
	if len(regs) > 0 {
		dto.Registrations = toVoiceRegistrationDTOs(regs)
	}
	return dto
}

func toVoiceAuditionDTOs(auds []store.VoiceAudition) []voiceAuditionDTO {
	out := make([]voiceAuditionDTO, 0, len(auds))
	for _, a := range auds {
		dto := voiceAuditionDTO{
			ID: a.ID, Index: a.Index, Transcript: a.Transcript,
			ModelID: a.ModelID, SHA256: a.AssetSHA256,
			DurationMS: a.DurationMS, LatencyMS: a.LatencyMS,
			Origin: a.Origin, RequestID: a.RequestID, CreatedAt: fmtTime(a.CreatedAt),
			QAState: auditionQAStateName(a.AsrState),
			QAWER:   a.AsrWER, QACER: a.AsrCER,
			QATranscript: a.AsrTranscript, QAModel: a.AsrModel,
			QARequestID: a.AsrRequestID,
		}
		if a.AsrCheckedAt != nil {
			dto.QACheckedAt = fmtTime(*a.AsrCheckedAt)
		}
		out = append(out, dto)
	}
	return out
}

// auditionQAStateName renders the audition's QA state for the API; "unknown"
// is the not-run state (ADR 0061's degraded mode).
func auditionQAStateName(s store.VoiceAuditionQAState) string {
	switch s {
	case store.VoiceAuditionQAPassed:
		return "passed"
	case store.VoiceAuditionQAFailed:
		return "failed"
	default:
		return "unknown"
	}
}

func toVoiceRegistrationDTOs(regs []store.VoiceRegistration) []voiceRegistrationDTO {
	out := make([]voiceRegistrationDTO, 0, len(regs))
	for _, r := range regs {
		last := ""
		if r.LastVerifiedAt != nil {
			last = fmtTime(*r.LastVerifiedAt)
		}
		out = append(out, voiceRegistrationDTO{
			InstanceID: r.InstanceID, RegisteredName: r.RegisteredName,
			RemoteDigest: r.RemoteDigest, State: r.State, Err: r.Err,
			LastVerifiedAt: last,
		})
	}
	return out
}

// ── GET /api/v1/voices ──────────────────────────────────────────────────────

// handleVoiceList serves GET /api/v1/voices?character=. The voice page of a
// game lists the revisions for each of its characters; without the parameter
// every revision is returned (a voice setup on a fresh install must not read
// as an error).
func (s *Server) handleVoiceList(w http.ResponseWriter, r *http.Request) {
	if !s.voiceRequired(w, r) {
		return
	}
	character := strings.TrimSpace(r.URL.Query().Get("character"))
	profiles, err := s.voices.Service.Store.ListVoiceProfiles(r.Context(), character)
	if err != nil {
		s.storeProblem(w, r, err, "list voices", "voice")
		return
	}
	out := make([]voiceRevisionDTO, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, toVoiceRevisionDTO(p, nil))
	}
	writeJSON(w, http.StatusOK, voiceListDTO{Voices: out})
}

// ── GET /api/v1/voices/{id} ─────────────────────────────────────────────────

// handleVoiceGet serves GET /api/v1/voices/{id}: one revision with its
// auditions and its per-instance registrations.
func (s *Server) handleVoiceGet(w http.ResponseWriter, r *http.Request) {
	if !s.voiceRequired(w, r) {
		return
	}
	id, ok := s.idFromPath(w, r, "id")
	if !ok {
		return
	}
	rev, err := s.voices.Service.Store.GetVoiceRevision(r.Context(), id)
	if err != nil {
		s.storeProblem(w, r, err, "get voice", "voice")
		return
	}
	auditions, err := s.voices.Service.Store.ListAuditions(r.Context(), id)
	if err != nil {
		s.storeProblem(w, r, err, "list voice auditions", "audition")
		return
	}
	regs, err := s.voices.Service.Store.ListVoiceRegistrations(r.Context(), id)
	if err != nil {
		s.storeProblem(w, r, err, "list voice registrations", "registration")
		return
	}
	out := voiceDetailDTO{voiceRevisionDTO: toVoiceRevisionDTO(rev, regs), Auditions: toVoiceAuditionDTOs(auditions)}
	writeJSON(w, http.StatusOK, out)
}

// ── GET /api/v1/voices/{id}/auditions/{aid} ─────────────────────────────────

// handleVoiceAuditionAudio serves the audition clip as audio/wav. The clip's
// SHA-256 is its content identity, so it doubles as the ETag.
func (s *Server) handleVoiceAuditionAudio(w http.ResponseWriter, r *http.Request) {
	if !s.voiceRequired(w, r) {
		return
	}
	if _, ok := s.idFromPath(w, r, "id"); !ok {
		return
	}
	if _, ok := s.idFromPath(w, r, "aid"); !ok {
		return
	}
	au, err := s.voices.Service.Store.GetAudition(r.Context(), r.PathValue("aid"))
	if err != nil {
		s.storeProblem(w, r, err, "get audition", "audition")
		return
	}
	if au.VoiceProfileID != r.PathValue("id") {
		s.writeCodeProblem(w, r, http.StatusNotFound, "not-found", codeNotFound,
			"Not Found", "no such audition on this voice revision")
		return
	}
	s.streamVoiceAsset(w, r, au.AssetSHA256)
}

// streamVoiceAsset serves one asset-store blob as audio/wav with the content
// identity as the ETag. A missing blob is a 410: the row outlived its bytes,
// which the UI must distinguish from a plain 404.
func (s *Server) streamVoiceAsset(w http.ResponseWriter, r *http.Request, sha string) {
	rdr, info, err := s.voices.Service.Assets.Get(r.Context(), sha)
	if err != nil {
		if errors.Is(err, assets.ErrNotFound) {
			s.writeCodeProblem(w, r, http.StatusGone, "asset-gone", codeNotFound,
				"Gone", "the audio for this voice is no longer present in the asset store")
			return
		}
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "asset-unreadable", "INTERNAL",
			"Internal Server Error", "the voice audio could not be read")
		return
	}
	defer rdr.Close()
	h := w.Header()
	h.Set("Content-Type", "audio/wav")
	h.Set("ETag", `"`+sha+`"`)
	h.Set("Cache-Control", "private, max-age=3600")
	if info.Size > 0 {
		h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rdr)
}

// ── POST /api/v1/voices/auditions ───────────────────────────────────────────

// handleVoiceAuditionsCreate serves POST /api/v1/voices/auditions. The draft
// revision row is created synchronously (so it is immediately addressable and
// the UI can show "designing…"), and the slow part — the VoiceDesign calls —
// is the durable job the response's job_id tracks (docs/spec/08: audition
// creation is queue-backed; a gateway stall must not hold an HTTP request open
// for minutes).
func (s *Server) handleVoiceAuditionsCreate(w http.ResponseWriter, r *http.Request) {
	if !s.voiceRequired(w, r) {
		return
	}
	var req voiceAuditionsCreateRequest
	if !s.decodeJSONBody(w, r, &req, 1<<16) {
		return
	}
	if req.Count <= 0 {
		req.Count = len(voices.SyntheticAuditionLines)
	}
	rev, err := s.voices.Service.CreateDraft(r.Context(), strings.TrimSpace(req.Character), strings.TrimSpace(req.Brief), req.Count)
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-parameter", codeInvalidParameter,
			"Bad Request", err.Error())
		return
	}
	if s.voices.Queue == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "queue-unavailable", "SUBSYSTEM_UNAVAILABLE",
			"Service Unavailable", "the job queue is not configured; audition generation is unavailable")
		return
	}
	payload, err := json.Marshal(voices.JobPayload{ProfileID: rev.ID})
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "encode-fault", "INTERNAL",
			"Internal Server Error", "the audition job could not be prepared")
		return
	}
	job, _, err := s.voices.Queue.Enqueue(r.Context(), store.EnqueueParams{
		Kind:    voices.JobKind,
		Payload: payload,
		TraceID: rev.ID,
	})
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "queue-unavailable", "SUBSYSTEM_UNAVAILABLE",
			"Service Unavailable", "the job queue could not accept the audition job")
		return
	}
	s.auditCatalogWrite(r, "voice.auditions.create", map[string]any{
		"voice_id": rev.ID, "character": rev.Character, "job_id": job.ID, "count": req.Count,
	})
	writeJSON(w, http.StatusAccepted, voiceAuditionsCreateResponse{
		JobID: job.ID, VoiceID: rev.ID, Character: rev.Character,
		Revision: rev.Revision, State: rev.State,
	})
}

// ── POST /api/v1/voices/{id}/approve ────────────────────────────────────────

// handleVoiceApprove serves POST /api/v1/voices/{id}/approve. Approving is the
// consent-gated freeze (docs/spec/14, ADR-0005): the request must carry a
// meaningful consent record, and the identity that approves is recorded on the
// frozen row.
func (s *Server) handleVoiceApprove(w http.ResponseWriter, r *http.Request) {
	if !s.voiceRequired(w, r) {
		return
	}
	id, ok := s.idFromPath(w, r, "id")
	if !ok {
		return
	}
	var req voiceApproveRequest
	if !s.decodeJSONBody(w, r, &req, 1<<14) {
		return
	}
	actor := IdentityFromContext(r.Context()).Username()
	rev, err := s.voices.Service.Approve(r.Context(), id, strings.TrimSpace(req.AuditionID), strings.TrimSpace(req.Consent), actor)
	if err != nil {
		s.voiceProblem(w, r, err)
		return
	}
	s.auditCatalogWrite(r, "voice.approve", map[string]any{
		"voice_id": id, "audition_id": req.AuditionID, "actor": actor,
	})
	writeJSON(w, http.StatusOK, toVoiceRevisionDTO(rev, nil))
}

// ── POST /api/v1/voices/{id}/register ───────────────────────────────────────

// handleVoiceRegister serves POST /api/v1/voices/{id}/register: run one
// reconciliation pass for this revision on this instance (docs/spec/06
// VoiceRegistryManager). It is idempotent — re-registering an already-present
// voice reports present and changes nothing.
func (s *Server) handleVoiceRegister(w http.ResponseWriter, r *http.Request) {
	if !s.voiceRequired(w, r) {
		return
	}
	id, ok := s.idFromPath(w, r, "id")
	if !ok {
		return
	}
	reg, err := s.voices.Service.Register(r.Context(), id)
	if err != nil {
		s.voiceProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, voiceRegistrationResponse{
		InstanceID: reg.InstanceID, RegisteredName: reg.RegisteredName,
		State: reg.State, Err: reg.Err,
	})
}

// ── POST /api/v1/voices/{id}/preview ────────────────────────────────────────

// handleVoicePreview serves POST /api/v1/voices/{id}/preview. The response is
// the preview audio (audio/wav); the cache decision rides X-Cache so the UI can
// show "cache hit" without a second call (docs/spec/07 step 7: the second
// identical preview is served from the speech cache, not a new TTS call).
func (s *Server) handleVoicePreview(w http.ResponseWriter, r *http.Request) {
	if !s.voiceRequired(w, r) {
		return
	}
	id, ok := s.idFromPath(w, r, "id")
	if !ok {
		return
	}
	var req voicePreviewRequest
	if !s.decodeJSONBody(w, r, &req, 1<<14) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-parameter", codeInvalidParameter,
			"Bad Request", "the preview line is empty")
		return
	}
	res, err := s.voices.Service.Preview(r.Context(), id, req.Text)
	if err != nil {
		s.voiceProblem(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "audio/wav")
	if res.FromCache {
		h.Set("X-Cache", "HIT")
	} else {
		h.Set("X-Cache", "MISS")
	}
	h.Set("ETag", `"`+res.SHA256+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.WAV)
}

// ── GET /api/v1/voices/{id}/registrations ───────────────────────────────────

// handleVoiceRegistrations serves GET /api/v1/voices/{id}/registrations: the
// per-instance registration rows (docs/spec/14: the voice registry is
// curator-readable, not viewer — replica state names upstream endpoints).
func (s *Server) handleVoiceRegistrations(w http.ResponseWriter, r *http.Request) {
	if !s.voiceRequired(w, r) {
		return
	}
	id, ok := s.idFromPath(w, r, "id")
	if !ok {
		return
	}
	regs, err := s.voices.Service.Store.ListVoiceRegistrations(r.Context(), id)
	if err != nil {
		s.storeProblem(w, r, err, "list voice registrations", "registration")
		return
	}
	writeJSON(w, http.StatusOK, voiceRegistrationsDTO{VoiceID: id, Registrations: toVoiceRegistrationDTOs(regs)})
}

// ── error mapping ───────────────────────────────────────────────────────────

// voiceProblem maps a voices.Service error onto the docs/spec/08 error
// contract. Each sentinel has exactly one HTTP meaning so a client can branch
// on `code` rather than prose.
func (s *Server) voiceProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, voices.ErrConsentRequired):
		s.writeCodeProblem(w, r, http.StatusBadRequest, "consent-required", "CONSENT_REQUIRED",
			"Bad Request", "approval needs a meaningful consent record (docs/spec/14); an empty consent is refused")
	case errors.Is(err, voices.ErrInvalidState):
		s.writeCodeProblem(w, r, http.StatusConflict, "voice-state", codeConflict,
			"Conflict", "the voice is not in the state this operation requires")
	case errors.Is(err, voices.ErrNotApproved):
		s.writeCodeProblem(w, r, http.StatusConflict, "voice-not-approved", codeConflict,
			"Conflict", "the voice must be approved before this operation")
	case errors.Is(err, voices.ErrAuditionNotFound), errors.Is(err, voices.ErrReferenceMissing):
		s.writeCodeProblem(w, r, http.StatusNotFound, "not-found", codeNotFound,
			"Not Found", "the named voice resource is not present")
	case errors.Is(err, voices.ErrAuditionFlagged):
		// FR-10 / ADR 0061: a flagged audition (ASR transcript beyond the
		// bound) is refused until a human reviews the evidence on the row.
		s.writeCodeProblem(w, r, http.StatusConflict, "audition-flagged", "AUDITION_FLAGGED",
			"Conflict", err.Error())
	default:
		s.storeProblem(w, r, err, "voice operation", "voice")
	}
}
