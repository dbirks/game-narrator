// Package tools implements the concrete onboarding-agent tool set
// (docs/spec/03 "Tool registry and minimum schemas", ADR-0026). Each tool is
// an agent.Tool handler registered into the ADR-0025 core registry: the
// core's executor still enforces allowlist, args schema, role, budget and
// timeout before any handler runs, and every outcome is wrapped in the
// docs/spec/17 envelope with source_ids.
//
// Trust rules carried from ADR-0025 into every handler:
//
//   - web text and user-imported text are untrusted data; tools whose output
//     carries captured text are flagged agent.Tool.UntrustedOutput so the
//     loop renders them only inside the delimited block (docs/spec/14);
//   - handlers write draft revisions only. Publishing happens exclusively
//     through publish_profile, which refuses without a human-issued,
//     single-use approval token (docs/spec/03 "Human-in-the-loop
//     boundaries": publishing requires a clear approval in first
//     deployment);
//   - no tool has shell/exec capability, and none may be given one: the
//     registry refuses the reserved names (docs/spec/03 scenario 3).
//
// The concrete handlers are grouped by concern (research.go, sources.go,
// voice.go, profile.go) and assembled by DefaultTools (registry.go).

package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/web"
	"github.com/dbirks/game-narrator/internal/web/search"
)

// ChatFn is the model boundary analyze_character calls with cited lines to
// produce a voice brief. Production wiring supplies a router call over the
// fast_reasoning (or agent_reasoning) role; tests supply a scripted fake.
type ChatFn func(ctx context.Context, req openai.ChatRequest) (openai.ChatResponse, error)

// VoiceDesigner is the TTS boundary design_voice calls. *tts.Client
// satisfies it; tests supply a fake that returns synthetic WAV bytes.
type VoiceDesigner interface {
	Design(ctx context.Context, req tts.DesignRequest) (tts.Audio, error)
}

// Deps is everything the concrete tools need from the appliance. Store,
// Assets and Adapters are required; the rest may be nil, in which case the
// tools that need them return a failed envelope with a clear reason instead
// of a panic (a misconfigured appliance must not crash the agent loop).
type Deps struct {
	// Store is the SQLite store: catalogue, profile content and Publish.
	Store *store.Store
	// Assets is the content-addressed blob store (staged user uploads,
	// audition WAVs).
	Assets assets.Store
	// Adapters is the import adapter registry (generic@1.1.0 in this build).
	Adapters *adapter.Registry
	// Search is the web search provider (Exa MCP by default, wrapped in
	// search.Budgeted). Nil: web_search and identify_game's web leg are
	// unavailable.
	Search search.Provider
	// Fetcher fetches pages under a security policy (SSRF filtering).
	// Nil: fetch_page is unavailable.
	Fetcher *web.Fetcher
	// Chat drives analyze_character. Nil: analyze_character fails
	// gracefully.
	Chat ChatFn
	// TTS drives design_voice. Nil: design_voice fails gracefully.
	TTS VoiceDesigner
	// Approvals issues and consumes human approval tokens for publish.
	// Nil: publish_profile always fails.
	Approvals ApprovalStore

	// Artwork is the in-process ledger the propose_artwork stub writes to.
	// DefaultTools builds one when nil; tests read it to assert scenario 5.
	Artwork *artworkLedger

	// CrashSim, when non-nil, is invoked by import_user_asset after a
	// successful import. It exists for one test only (docs/spec/03
	// scenario 7): the hook panics to simulate a process crash *between*
	// the tool's writes and the durable outcome row, so the resume test
	// exercises the pending-row worklist against a real, interrupted
	// 3,000-line import. Production wiring leaves it nil.
	CrashSim func()
}

// validate checks the mandatory deps. Optional deps are checked by the tools
// that use them, so one missing provider degrades one tool, not the set.
func (d Deps) validate() error {
	var missing []string
	if d.Store == nil {
		missing = append(missing, "Store")
	}
	if d.Assets == nil {
		missing = append(missing, "Assets")
	}
	if d.Adapters == nil || d.Adapters.Len() == 0 {
		missing = append(missing, "Adapters")
	}
	if len(missing) > 0 {
		return fmt.Errorf("tools: Deps missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// nowStamp is the RFC 3339 form the schema timestamp CHECKs accept.
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

// parseArgs unmarshals a call's arguments into a string-keyed map. The
// executor already validated the args against the tool's input schema; this
// is the handler-side convenience view.
func parseArgs(call agent.Call) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(call.Arguments, &m); err != nil {
		return nil, fmt.Errorf("tools: %s: bad arguments: %w", call.Name, err)
	}
	return m, nil
}

// strArg reads a string argument, returning "" when absent or not a string.
func strArg(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// intArg reads a numeric argument as an int, returning fallback when absent
// or not a number (JSON numbers unmarshal as float64).
func intArg(m map[string]any, key string, fallback int) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return fallback
}

// boolArg reads a bool argument, returning fallback when absent.
func boolArg(m map[string]any, key string, fallback bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return fallback
}

// okResult wraps a JSON-able value as a successful tool result. The
// executor fills the envelope fields (schema version, call id, status).
func okResult(v any, sourceIDs ...string) (agent.Result, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: marshal result: %w", err)
	}
	ids := append([]string{}, sourceIDs...)
	return agent.Result{Results: raw, SourceIDs: ids}, nil
}

// require validates a non-empty required string argument.
func require(m map[string]any, key string) (string, error) {
	v := strArg(m, key)
	if v == "" {
		return "", fmt.Errorf("argument %q is required", key)
	}
	return v, nil
}

// webSourceTrust is the trust a web-sourced provenance row earns
// (docs/spec/14: retrieved material is untrusted input; the onboarding
// user may raise trust by re-supplying the bytes themselves).
const webSourceTrust = 0.4

// webSourceRights is the rights position a web-sourced row carries until a
// human says otherwise (docs/spec/03: "unknown source rights" is a review
// boundary; unknown is the honest stored value for fetched material).
const webSourceRights = games.RightsUnknown

// registerWebSource records one fetched/searched URL as a provenance row on
// the profile and returns the source id. An existing row with the same
// content hash (or, when the page body is unknown, the same URI) is reused:
// provenance is write-once (docs/spec/01 invariant 6), and re-fetching a
// stable page must not accumulate rows.
func registerWebSource(ctx context.Context, d Deps, profileID string, typ games.SourceType, uri, sha256 string) (string, error) {
	if typ != games.SourceWebPage {
		return "", fmt.Errorf("tools: unsupported web source type %q", typ)
	}
	sources, err := d.Store.ListSources(ctx, profileID)
	if err != nil {
		return "", fmt.Errorf("tools: list sources: %w", err)
	}
	for _, s := range sources {
		if (sha256 != "" && s.ContentSHA256 == sha256) || (sha256 == "" && s.URI == uri) {
			return s.ID, nil
		}
	}
	src := games.Source{
		ID:            games.NewID(),
		ProfileID:     profileID,
		SourceType:    games.SourceWebPage,
		URI:           uri,
		RetrievedAt:   nowStamp(),
		ContentSHA256: sha256,
		Rights:        webSourceRights,
		Trust:         webSourceTrust,
	}
	if err := src.Validate(); err != nil {
		return "", fmt.Errorf("tools: invalid web source: %w", err)
	}
	if err := d.Store.PutSources(ctx, profileID, []games.Source{src}); err != nil {
		return "", fmt.Errorf("tools: register web source: %w", err)
	}
	return src.ID, nil
}

// listMessages pages through every message of one revision. Tools that
// need source-scoped statistics filter in memory: MessageFilter has no
// source_id predicate, and the working set is one game's text (docs/spec/02
// bounds a revision to ~10 000 messages).
func (d Deps) listMessages(ctx context.Context, profileID string) ([]games.Message, error) {
	var out []games.Message
	cursor := ""
	for {
		page, next, err := d.Store.ListMessages(ctx, profileID, store.MessageFilter{}, cursor, store.MaxListLimit)
		if err != nil {
			return nil, fmt.Errorf("tools: list messages: %w", err)
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// ---------------------------------------------------------------------------
// Human approval tokens (docs/spec/03 "publishing requires a clear approval")
// ---------------------------------------------------------------------------

// Approval is one human grant of publish authority for one profile. Token
// carries the raw secret only on the Issue side: it is shown to the human
// once and never stored — the stores keep its SHA-256.
type Approval struct {
	ID        string
	ProfileID string
	GrantedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	// Token is the raw token, set only by Issue; never persisted.
	Token string
}

// ApprovalTTL bounds how long an unspent token remains valid.
const ApprovalTTL = 24 * time.Hour

// ApprovalStore issues and consumes human approval tokens. The interface is
// the agent's boundary: the agent can consume (publish_profile) but has no
// tool that issues. Implementations: MemoryApprovals (tests, ephemeral) and
// SQLiteApprovals (production; migration 0008, single-use enforced by the
// consumed_at column).
type ApprovalStore interface {
	// Issue mints a token for profileID. grantedBy names the human
	// (username or "ui") for the audit trail.
	Issue(ctx context.Context, profileID, grantedBy string) (Approval, error)
	// Consume validates a raw token and burns it. It returns the approved
	// profile; unknown, spent and expired tokens all fail.
	Consume(ctx context.Context, token string) (Approval, error)
}

// mintToken produces "gnapp_" + 64 hex chars (256 bits of randomness).
func mintToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("tools: approval token: no system randomness: %w", err)
	}
	return "gnapp_" + hex.EncodeToString(buf[:]), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// MemoryApprovals is an in-process ApprovalStore for tests and dev mode.
// Single-use and expiry semantics match the SQLite implementation.
type MemoryApprovals struct {
	mu     sync.Mutex
	byHash map[string]memApproval
	now    func() time.Time
}

type memApproval struct {
	approval Approval
	spent    bool
}

// NewMemoryApprovals returns an empty in-memory approval store.
func NewMemoryApprovals() *MemoryApprovals {
	return &MemoryApprovals{byHash: map[string]memApproval{}, now: func() time.Time { return time.Now().UTC() }}
}

// Issue implements ApprovalStore.
func (m *MemoryApprovals) Issue(_ context.Context, profileID, grantedBy string) (Approval, error) {
	if !games.ValidID(profileID) {
		return Approval{}, fmt.Errorf("tools: approval: profile id %q is not a ULID", profileID)
	}
	if strings.TrimSpace(grantedBy) == "" {
		return Approval{}, fmt.Errorf("tools: approval: granted_by is required")
	}
	token, err := mintToken()
	if err != nil {
		return Approval{}, err
	}
	now := m.now()
	a := Approval{
		ID:        games.NewID(),
		ProfileID: profileID,
		GrantedBy: strings.TrimSpace(grantedBy),
		CreatedAt: now,
		ExpiresAt: now.Add(ApprovalTTL),
		Token:     token,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byHash[tokenHash(token)] = memApproval{approval: a}
	return a, nil
}

// Consume implements ApprovalStore.
func (m *MemoryApprovals) Consume(_ context.Context, token string) (Approval, error) {
	h := tokenHash(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	ma, ok := m.byHash[h]
	if !ok {
		return Approval{}, fmt.Errorf("tools: approval: unknown token")
	}
	if ma.spent {
		return Approval{}, fmt.Errorf("tools: approval: token already used")
	}
	if m.now().After(ma.approval.ExpiresAt) {
		return Approval{}, fmt.Errorf("tools: approval: token expired")
	}
	ma.spent = true
	m.byHash[h] = ma
	return ma.approval, nil
}
