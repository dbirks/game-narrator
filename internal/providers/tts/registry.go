package tts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"

	"github.com/dbirks/game-narrator/internal/providers"
)

// The registry lives on the Base service itself: GET/POST/DELETE
// /v1/audio/voices (routes verified against the deployment's openapi.json,
// ADR-0022). A collective gateway does not proxy those routes (it answers
// 404 "unsupported path"), so the Registry takes the *direct* Base service
// URL, not the shared gateway. The app-owned asset store remains the
// source of truth for reference audio, transcript and consent (ADR-0005);
// this is the per-instance copy the Base checkpoint needs to resolve a
// voice name to an embedding (docs/spec/06 "Routing, reliability and
// replicas").

// Voice is one canonical voice revision the registry must hold. RefAudio is
// the approved reference WAV from the asset store; RefText is the exact
// approved transcript of that WAV; Consent is the truthful rights/consent
// record (docs/spec/14 "Voice consent" — it must be passed meaningfully,
// never fabricated, and registration without it is refused).
type Voice struct {
	Name     string
	RefAudio []byte
	RefText  string
	Consent  string
}

// UploadedVoice is one registry entry as the Base service reports it.
type UploadedVoice struct {
	Name               string `json:"name"`
	Consent            string `json:"consent"`
	CreatedAt          int64  `json:"created_at"`
	FileSize           int64  `json:"file_size"`
	MimeType           string `json:"mime_type"`
	RefText            string `json:"ref_text"`
	SpeakerDescription string `json:"speaker_description"`
	EmbeddingSource    string `json:"embedding_source"`
	EmbeddingDim       int    `json:"embedding_dim"`
}

// ListResult is the GET /v1/audio/voices answer: Voices lists every name the
// checkpoint can speak (built-ins plus uploads); Uploaded carries the
// registry metadata for the uploaded ones.
type ListResult struct {
	Voices   []string
	Uploaded []UploadedVoice
}

// EnsureReport is the outcome of one reconciliation pass.
type EnsureReport struct {
	// Present already held on this instance with a matching content digest.
	Present []string
	// Registered were uploaded by this pass (or by a racing instance with
	// identical content, detected by the post-conflict re-list).
	Registered []string
	// Conflicts are names that exist here with different content, or whose
	// requested content contradicts the name's digest. They are NEVER
	// re-registered: names are immutable (docs/spec/06) and a collision means
	// a higher revision or a tenant namespace problem that a human must see.
	Conflicts []Conflict
}

// Conflict names the voice and why reconciliation refused it.
type Conflict struct {
	Name   string
	Reason string
}

// Sentinel errors the registry returns for policy violations. They are
// distinct from *providers.Error transport failures so callers can branch on
// them with errors.Is; a conflict additionally carries the server's HTTP
// status as its cause.
var (
	// ErrConsentRequired: registration refused because the consent record is
	// empty (docs/spec/14).
	ErrConsentRequired = errors.New("tts: consent is required for voice registration and was empty")
	// ErrInvalidName: the name is not a legal namespaced immutable ID.
	ErrInvalidName = errors.New("tts: voice name is not a valid namespaced immutable ID")
	// ErrVoiceConflict: the name already exists with different content (or
	// the server rejected the upload as a collision under the immutable
	// registration policy). The caller must pick a new revision, not retry.
	ErrVoiceConflict = errors.New("tts: voice name is already registered with different content")
)

// Registry talks to one Base service instance's voice registry.
type Registry struct {
	BaseURL string
	Token   string
	// HTTP is the transport; nil means context-deadline-only, as in Client.
	HTTP *http.Client
}

// New returns a registry for the direct Base service base URL (e.g. a
// port-forwarded /v1 endpoint). Validation mirrors Client.New.
func NewRegistry(baseURL, token string) (*Registry, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, providers.NewError(providers.KindUnsupportedCapability, 0, "tts: registry BaseURL is empty")
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("tts: invalid registry base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("tts: registry base URL scheme %q is not supported, use http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("tts: registry base URL has no host")
	}
	if u.User != nil {
		return nil, errors.New("tts: credentials in the registry base URL are not allowed, use the token field")
	}
	return &Registry{BaseURL: strings.TrimRight(u.String(), "/"), Token: token}, nil
}

// String renders the registry identity for logs without the URL or token.
func (r *Registry) String() string {
	return fmt.Sprintf("tts.Registry{BaseURL: <set>, Token: %q}", mask(r.Token))
}

// List returns every voice the instance can speak plus the uploaded-voice
// metadata (docs/spec/07 step 6: reconcile against this listing).
func (r *Registry) List(ctx context.Context) (ListResult, error) {
	var out struct {
		Voices   []string        `json:"voices"`
		Uploaded []UploadedVoice `json:"uploaded_voices"`
	}
	if err := r.rawDo(ctx, http.MethodGet, "/audio/voices", nil, "", &out); err != nil {
		return ListResult{}, err
	}
	return ListResult{Voices: out.Voices, Uploaded: out.Uploaded}, nil
}

// formPartHeader builds the header for the audio_sample multipart part with
// an explicit audio Content-Type (the multipart default, application/
// octet-stream, is not what the Base service expects).
func formPartHeader(name, filename string) textproto.MIMEHeader {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, name, filename))
	h.Set("Content-Type", "audio/wav")
	return h
}

// Register uploads one voice revision. It refuses to run without a
// non-empty consent record or a name outside the immutable scheme, and it
// refuses to persist a reference WAV that fails ValidateAudio (a broken
// reference belongs in quarantine, never in a registry).
func (r *Registry) Register(ctx context.Context, v Voice) (UploadedVoice, error) {
	if strings.TrimSpace(v.Consent) == "" {
		return UploadedVoice{}, fmt.Errorf("registering %q: %w", safeName(v.Name), ErrConsentRequired)
	}
	if err := ValidateName(v.Name); err != nil {
		return UploadedVoice{}, err
	}
	if len(v.RefAudio) == 0 {
		return UploadedVoice{}, fmt.Errorf("registering %q: reference audio is empty", safeName(v.Name))
	}
	if _, err := ValidateAudio(v.RefAudio); err != nil {
		return UploadedVoice{}, fmt.Errorf("registering %q: reference audio rejected: %s", safeName(v.Name), err)
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreatePart(formPartHeader("audio_sample", "reference.wav"))
	if err != nil {
		return UploadedVoice{}, err
	}
	if _, err := part.Write(v.RefAudio); err != nil {
		return UploadedVoice{}, err
	}
	w.WriteField("name", v.Name)
	w.WriteField("consent", v.Consent)
	if strings.TrimSpace(v.RefText) != "" {
		// The exact approved transcript: the checkpoint uses it for embedding
		// alignment and it is the replay evidence (docs/spec/07 step 4).
		w.WriteField("ref_text", v.RefText)
	}
	if err := w.Close(); err != nil {
		return UploadedVoice{}, err
	}

	var resp struct {
		Success bool          `json:"success"`
		Voice   UploadedVoice `json:"voice"`
		Message string        `json:"message"`
	}
	err = r.rawDo(ctx, http.MethodPost, "/audio/voices", &buf, w.FormDataContentType(), &resp)
	if err != nil {
		if isRegistrationConflict(err) {
			return UploadedVoice{}, &voiceConflict{cause: err}
		}
		return UploadedVoice{}, err
	}
	if !resp.Success {
		return UploadedVoice{}, fmt.Errorf("registering %q: server reported failure: %s",
			safeName(v.Name), oneLine(resp.Message, 200))
	}
	return resp.Voice, nil
}

// Remove deletes one voice by name. Only names inside our own gn_ namespace
// are deletable through this client: the registry is shared across tenants
// on the server by default (docs/spec/07 "Voice registry is not per-user
// protected"), so the client side refuses to reach outside the namespace.
func (r *Registry) Remove(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	var out map[string]any
	err := r.rawDo(ctx, http.MethodDelete, "/audio/voices/"+url.PathEscape(name), nil, "", &out)
	var pe *providers.Error
	if errors.As(err, &pe) && pe.Kind == providers.KindUnsupportedCapability && pe.Status == http.StatusNotFound {
		// The instance does not know the voice: report it as voice_missing so
		// the routing layer can reconcile (docs/spec/06 failure taxonomy).
		pe.Kind = providers.KindVoiceMissing
	}
	return err
}

// Ensure reconciles this instance against the required voice set
// (docs/spec/06 "VoiceRegistryManager", ADR-0005): list once, register what
// is missing, report what is present, and never re-register a name whose
// content contradicts its stored identity. Identity is the digest8 embedded
// in the immutable name itself (docs/spec/06 "never rely on friendly voice
// names"), which makes the check work even on instances whose listing does
// not expose the reference bytes.
func (r *Registry) Ensure(ctx context.Context, required []Voice) (EnsureReport, error) {
	var report EnsureReport
	if len(required) == 0 {
		return report, nil
	}
	// Fail fast on invalid input before any mutation: Ensure must not leave a
	// partially registered state behind an operator mistake.
	for _, v := range required {
		if err := ValidateName(v.Name); err != nil {
			return report, err
		}
		if strings.TrimSpace(v.Consent) == "" {
			return report, fmt.Errorf("ensuring %q: %w", safeName(v.Name), ErrConsentRequired)
		}
	}

	listed, err := r.List(ctx)
	if err != nil {
		return report, err
	}
	have := make(map[string]bool, len(listed.Voices))
	for _, n := range listed.Voices {
		have[n] = true
	}

	for _, v := range required {
		wantDigest := Digest8(v.RefAudio)
		if !strings.HasSuffix(v.Name, "_"+wantDigest) {
			report.Conflicts = append(report.Conflicts, Conflict{
				Name:   v.Name,
				Reason: "name digest does not match the reference audio content",
			})
			continue
		}
		if have[v.Name] {
			report.Present = append(report.Present, v.Name)
			continue
		}
		if _, err := r.Register(ctx, v); err != nil {
			if errors.Is(err, ErrVoiceConflict) {
				// A racing instance (or an earlier pass) may have registered the
				// same content meanwhile: re-list to tell that from a true
				// collision. Only idempotent success is accepted.
				if again, listErr := r.List(ctx); listErr == nil {
					for _, n := range again.Voices {
						if n == v.Name {
							report.Present = append(report.Present, v.Name)
							err = nil
							break
						}
					}
				}
				if err != nil {
					report.Conflicts = append(report.Conflicts, Conflict{Name: v.Name, Reason: err.Error()})
					continue
				}
			} else {
				return report, err
			}
			continue
		}
		report.Registered = append(report.Registered, v.Name)
	}
	return report, nil
}

// rawDo issues one registry request. GET and DELETE take a JSON response;
// POST sends the pre-encoded multipart body. contentType is "" for the
// JSON methods. Error classification reuses the Client's mapping so the
// gateway's 404 "unsupported path" and the taxonomy stay consistent.
func (r *Registry) rawDo(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	client := &Client{BaseURL: r.BaseURL, Token: r.Token, HTTP: r.HTTP}
	req, err := http.NewRequestWithContext(ctx, method, r.BaseURL+path, body)
	if err != nil {
		return client.transportError(err, 0)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", DefaultUserAgent)
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	httpc := client.httpClient()
	resp, err := httpc.Do(req)
	if err != nil {
		return client.transportError(err, 0)
	}
	defer func() { io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) }()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return client.transportError(readErr, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return client.httpError(resp.StatusCode, respBody, requestID(resp.Header))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			e := providers.NewError(providers.KindBadSchema, resp.StatusCode, "tts: registry response is not valid JSON")
			e.Err = err
			return e
		}
	}
	return nil
}

// --- names ---------------------------------------------------------------

// Digest8 is the content identity embedded in a voice name: the first 8 hex
// chars of SHA-256 over the reference WAV bytes.
func Digest8(refAudio []byte) string {
	sum := sha256.Sum256(refAudio)
	return hex.EncodeToString(sum[:])[:8]
}

// MakeName builds the canonical immutable voice name (docs/spec/06
// "namespaces based on account/profile/revision digest"):
//
//	gn_<profile8>_<character>_v<rev>_<digest8>
//
// profile8 is the 8-hex-char profile identity, character the stable
// character slug, rev the VoiceProfileRevision number (ADR-0005), digest8
// the content digest of refAudio. Two revisions with different audio can
// never share a name; the same audio re-registered is the same name, which
// is what makes Ensure idempotent.
func MakeName(profile8, character string, rev int, refAudio []byte) (string, error) {
	if !nameProfileRe.MatchString(profile8) {
		return "", fmt.Errorf("%w: profile must be 8 lowercase hex chars, got %q", ErrInvalidName, profile8)
	}
	if !nameCharacterRe.MatchString(character) {
		return "", fmt.Errorf("%w: character slug must be [a-z0-9][a-z0-9_-]{0,31}, got %q", ErrInvalidName, character)
	}
	if rev < 1 {
		return "", fmt.Errorf("%w: revision must be >= 1, got %d", ErrInvalidName, rev)
	}
	if len(refAudio) == 0 {
		return "", fmt.Errorf("%w: reference audio is empty", ErrInvalidName)
	}
	return fmt.Sprintf("gn_%s_%s_v%d_%s", profile8, character, rev, Digest8(refAudio)), nil
}

// ValidateName accepts exactly two shapes: the production scheme and the
// automated-test scheme gn_test_<random8> (the only names the live tests
// may register or delete; AGENTS.md rule 5). Everything else — friendly
// names, another tenant's namespace, a path — is refused.
func ValidateName(name string) error {
	if name == "" || len(name) > 128 {
		return fmt.Errorf("%w: %q", ErrInvalidName, oneLine(name, 40))
	}
	if nameProductionRe.MatchString(name) || nameTestRe.MatchString(name) {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrInvalidName, oneLine(name, 40))
}

var (
	nameProfileRe    = regexp.MustCompile(`^[0-9a-f]{8}$`)
	nameCharacterRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	nameProductionRe = regexp.MustCompile(`^gn_[0-9a-f]{8}_[a-z0-9][a-z0-9_-]{0,31}_v[1-9][0-9]*_[0-9a-f]{8}$`)
	nameTestRe       = regexp.MustCompile(`^gn_test_[0-9a-f]{8}$`)
)

// isRegistrationConflict reports whether the registration failure is a name
// collision the server's immutable policy rejected (409, or a 400 whose text
// names an existing/conflicting voice).
func isRegistrationConflict(err error) bool {
	pe := asProvidersError(err)
	if pe == nil {
		return false
	}
	msg := strings.ToLower(pe.Msg)
	switch pe.Status {
	case http.StatusConflict:
		return true
	case http.StatusBadRequest:
		return strings.Contains(msg, "already exists") || strings.Contains(msg, "conflict") ||
			strings.Contains(msg, "immutable") || strings.Contains(msg, "collision")
	default:
		return false
	}
}

// voiceConflict wraps ErrVoiceConflict and keeps the server failure as the
// cause (status, request id) for diagnostics.
type voiceConflict struct{ cause error }

func (e *voiceConflict) Error() string { return ErrVoiceConflict.Error() }
func (e *voiceConflict) Unwrap() error { return errors.Join(ErrVoiceConflict, e.cause) }

// safeName keeps a voice name out of an error message when it is not a
// legal name yet (an attacker-controlled or mistyped name must not reach a
// log unmodified).
func safeName(name string) string {
	if err := ValidateName(name); err == nil {
		return name
	}
	return oneLine(name, 40)
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
