// Package voices implements the voice workflow domain (docs/spec/07
// "Voice lifecycle", docs/spec/02 voice_profiles/voice_registrations,
// ADR-0005/0022/0023): auditioning, approving, registering and previewing
// cloned voices per character.
//
// It is the application-owned layer above the provider adapters
// (internal/providers/tts) and the speech cache (internal/speech). The asset
// store plus the voice_profiles row are the source of truth for a voice: the
// provider registry (voice_registrations) is the per-instance copy that
// Reconcile keeps in sync, so a lost registry or restarted provider never
// loses the canonical reference.
//
// Model
//
//	A voice profile is one per character (or the narrator). Each design
//	attempt is one immutable revision: it starts draft, carries N audition
//	clips generated from fixed synthetic lines (never game text,
//	docs/spec/03), and can be approved with an explicit consent string.
//	Approval freezes the chosen clip and its exact transcript as the
//	canonical reference, computes the revision fingerprint used in every TTS
//	cache key for that revision, and pre-registers the immutable, namespaced
//	voice name. A new approval is a new revision; the previous one stays
//	approved so old cache entries remain attributable (docs/spec/07 step 8).
//
//	Key invariants:
//	  - Approve requires a non-empty consent record (docs/spec/14).
//	  - After approval the reference (clip bytes + transcript + model) is
//		immutable; the store refuses any other move (ADR-0005).
//	  - Fingerprint = SHA-256 over (reference digest, transcript, model);
//		stable across restarts and embedded in the speech cache key.
//	  - Registry names are namespaced and content-addressed
//		(tts.MakeName); this package only ever registers/removes names it
//		derived itself (gn_ production scheme, gn_test_ for tests).
//	  - Reconcile re-registers approved voices whose registry entry went
//		missing (docs/spec/06 "voice missing on replica"), and reports
//		conflicts without ever overwriting a name with different content.
package voices

// Revision states (docs/spec/02 voice_profiles.state).
const (
	StateDraft       = "draft"
	StateAuditioning = "auditioning"
	StateApproved    = "approved"
	StateRetired     = "retired"
)

// Registration states (docs/spec/02 voice_registrations.state).
const (
	RegPending = "pending"
	RegReady   = "ready"
	RegMissing = "missing"
	RegInvalid = "invalid"
)

// DefaultInstanceID is the provider instance id for the single Base service
// this build talks to; a multi-instance deployment sets a different id per
// Registry wiring (docs/spec/06 "Routing, reliability and replicas").
const DefaultInstanceID = "primary"

// SyntheticAuditionLines are the fixed texts the auditions speak
// (docs/spec/03: "auditions come from synthetic test text or APPROVED game
// text"). They are deliberately not game text: a reference clip derived from
// game dialogue would carry rights questions the user never approved. The
// agent's design_voice tool speaks the same three lines.
var SyntheticAuditionLines = []string{
	"Hello there. Let me tell you a little about today.",
	"The morning was calm, and the road ahead was long.",
	"I would like to ask you a small question, if that is all right.",
}
