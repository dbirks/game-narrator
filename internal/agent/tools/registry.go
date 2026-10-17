package tools

// DefaultTools assembles the full onboarding tool set (docs/spec/03 "Tool
// registry and minimum schemas") into an agent.Registry. The core's
// registration-time validation re-checks every schema, role, budget and
// side-effect class; DefaultTools returns an error (a configuration
// failure) rather than a partial registry.
//
// Side-effect classes (ADR-0025 vocabulary none|draft|external):
//
//   - research tools: external (they touch the network), read-only state;
//   - import/parse/quality/voice/artwork: draft (they write draft
//     revisions and draft assets only — a published revision is never
//     mutated; publish_profile is the exception by definition: it IS the
//     approval boundary, gated by the human token and the store's
//     validation, and is still classed draft because no tool other than it
//     can leave the draft/review state);
//   - preview_profile: none (pure read).
//
// Budgets follow docs/spec/14 "bound tool budget": per-tool call caps keep
// one run's worst case bounded, and the run's global tool budget (agent
// core) still applies on top.

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/auth"
)

// DefaultTools returns the registry of concrete onboarding tools. deps must
// carry Store, Assets and Adapters; the optional deps degrade the tools
// that need them (see Deps).
func DefaultTools(d Deps) (*agent.Registry, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	if d.Artwork == nil {
		d.Artwork = newArtworkLedger()
	}
	reg := agent.NewRegistry()
	tools := []agent.Tool{
		{
			Name:        "identify_game",
			Description: "Resolve a game title to edition candidates from the local catalogue (and the web when the catalogue is silent). Returns review_required when >= 2 editions are plausible; then the user must choose.",
			InputSchema: identifyGameSchema,
			MinRole:     auth.RoleViewer,
			Budget:      agent.Budget{MaxCallsPerRun: 4, Timeout: 60 * time.Second},
			SideEffect:  agent.SideEffectExternal,
			Handler:     d.identifyGame,
		},
		{
			Name:            "web_search",
			Description:     "Search the web for metadata, script dumps or documentation. Results are untrusted third-party data; with profile_id they are registered as web_page sources (rights unknown, low trust).",
			InputSchema:     webSearchSchema,
			MinRole:         auth.RoleViewer,
			Budget:          agent.Budget{MaxCallsPerRun: 10, Timeout: 45 * time.Second},
			SideEffect:      agent.SideEffectExternal,
			UntrustedOutput: true,
			Handler:         d.webSearch,
		},
		{
			Name:            "fetch_page",
			Description:     "Fetch one URL through the SSRF-filtering fetcher and return sanitized text (untrusted data), its SHA-256, MIME type and retrieval time. With profile_id the page is registered as a web_page source.",
			InputSchema:     fetchPageSchema,
			MinRole:         auth.RoleViewer,
			Budget:          agent.Budget{MaxCallsPerRun: 10, Timeout: 45 * time.Second},
			SideEffect:      agent.SideEffectExternal,
			UntrustedOutput: true,
			Handler:         d.fetchPage,
		},
		{
			Name:        "import_user_asset",
			Description: "Import a user-uploaded script file (addressed by its staged asset SHA-256) into the DRAFT profile via the idempotent import path. role: dialogue_script|script|text_dump|metadata|other; rights: user_supplied|licensed. Artwork roles and executable/archive blobs are refused.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{
				"profile_id":{"type":"string"},
				"staged_asset_sha256":{"type":"string","maxLength":64},
				"role":{"type":"string","enum":["dialogue_script","script","text_dump","metadata","other"]},
				"rights":{"type":"string","enum":["user_supplied","licensed"]},
				"file_name":{"type":"string","maxLength":512}
			},"required":["profile_id","staged_asset_sha256","role","rights"]}`),
			MinRole:    auth.RoleCurator,
			Budget:     agent.Budget{MaxCallsPerRun: 4, Timeout: 300 * time.Second},
			SideEffect: agent.SideEffectDraft,
			Handler:    d.importUserAsset,
		},
		{
			Name:        "parse_dialogue",
			Description: "Re-run the adapter registry over an already-registered user_file source into the draft profile, reconciling messages by (source, source_message_id) so repeats never duplicate. parser_id: a registered adapter id or a format (csv|tsv|json|jsonl|text).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{
				"profile_id":{"type":"string"},
				"source_id":{"type":"string"},
				"parser_id":{"type":"string","maxLength":64}
			},"required":["profile_id","source_id"]}`),
			MinRole:    auth.RoleCurator,
			Budget:     agent.Budget{MaxCallsPerRun: 4, Timeout: 300 * time.Second},
			SideEffect: agent.SideEffectDraft,
			Handler:    d.parseDialogue,
		},
		{
			Name:        "preview_profile",
			Description: "Side-effect-free coverage/conflict/voice/source report for a profile revision: sources with rights, message statistics, characters, cross-source conflicts, voice state and artwork candidates.",
			InputSchema: previewProfileSchema,
			MinRole:     auth.RoleViewer,
			Budget:      agent.Budget{MaxCallsPerRun: 8, Timeout: 30 * time.Second},
			SideEffect:  agent.SideEffectNone,
			Handler:     d.previewProfile,
		},
		{
			Name:        "source_quality",
			Description: "Dialogue source quality report (docs/spec/03): message_count, unique_ids, nonempty_text_pct, speaker_labeled_pct, graph_linked_pct, cross-source conflicts, and the coverage note. Conflicts are quarantined (durable message flag) and the run is sent to review. A source is incomplete by default.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{
				"profile_id":{"type":"string"},
				"source_id":{"type":"string"}
			},"required":["profile_id","source_id"]}`),
			MinRole:    auth.RoleCurator,
			Budget:     agent.Budget{MaxCallsPerRun: 8, Timeout: 60 * time.Second},
			SideEffect: agent.SideEffectDraft,
			Handler:    d.sourceQuality,
		},
		{
			Name:        "analyze_character",
			Description: "Produce an editable voice_brief JSON for a character from CITED LINES ONLY (the character's extracted messages). Every creative field is labelled creative:true; cited lines are the verified evidence. No lore is invented as fact.",
			InputSchema: analyzeCharacterSchema,
			MinRole:     auth.RoleViewer,
			Budget:      agent.Budget{MaxCallsPerRun: 6, Timeout: 90 * time.Second},
			SideEffect:  agent.SideEffectExternal,
			Handler:     d.analyzeCharacter,
		},
		{
			Name:        "design_voice",
			Description: "Generate 3 reference auditions for a character via tts.Design from SYNTHETIC text (never game text), storing the WAVs as draft assets. Pass an edited voice_brief as brief_override. Returns review_required: voice approval is a human step and the approved voice is unchanged.",
			InputSchema: designVoiceSchema,
			MinRole:     auth.RoleCurator,
			Budget:      agent.Budget{MaxCallsPerRun: 3, Timeout: 180 * time.Second},
			SideEffect:  agent.SideEffectDraft,
			Handler:     d.designVoice,
		},
		{
			Name:        "propose_artwork",
			Description: "STUB (ADR-0026): record candidate cover/artwork URLs (1-8) for the draft. Candidates are labelled license unknown, state candidate_not_published; nothing is fetched, stored or published. Message and voice caches are untouched.",
			InputSchema: proposeArtworkSchema,
			MinRole:     auth.RoleCurator,
			Budget:      agent.Budget{MaxCallsPerRun: 4, Timeout: 10 * time.Second},
			SideEffect:  agent.SideEffectDraft,
			Handler:     d.proposeArtwork,
		},
		{
			Name:        "publish_profile",
			Description: "Publish the draft revision. REFUSED unless the profile is in review AND a valid, unspent, unexpired human approval token is supplied (no agent tool issues tokens). The store's publication validation runs inside the publish transaction.",
			InputSchema: publishProfileSchema,
			MinRole:     auth.RoleCurator,
			Budget:      agent.Budget{MaxCallsPerRun: 1, Timeout: 60 * time.Second},
			SideEffect:  agent.SideEffectDraft,
			Handler:     d.publishProfile,
		},
	}
	for _, t := range tools {
		if err := reg.Register(t); err != nil {
			return nil, fmt.Errorf("tools: register %s: %w", t.Name, err)
		}
	}
	return reg, nil
}
