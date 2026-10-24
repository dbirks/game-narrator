// The generic kind voices (ADR-0158, issue #51, ADR-0185): the seven roster
// keys `kind:adult_male` … `kind:creature` an unnamed speaker of a confident
// apparent kind speaks with. What used to be a by-hand operator script is
// here: the fixed per-kind briefs, a coverage read (missing / pending /
// approved plus the measured F0 and class per kind), and the one-click
// create-missing run — audition one take per missing kind, measure it with
// this package's audit classifier, retry with stronger pitch wording while
// the measured class is wrong, and leave the passing take PENDING for the
// operator's approve click (voice selection is reviewable, AGENTS.md rule 6).
//
// Every log-safe value is a kind id, a class label and an Hz number; nothing
// here ever carries game text (AGENTS.md rule 5).

package cast

import (
	"bytes"
	"context"
	"fmt"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

// Kinds is the ADR-0158 speaker-kind set in stable display order.
var Kinds = []string{
	"adult_male", "adult_female", "elder_male", "elder_female", "boy", "girl", "creature",
}

// KindCharacter is the roster character key a kind voice is approved under.
func KindCharacter(kind string) string { return "kind:" + kind }

// Briefs is the fixed per-kind design brief the create-missing run audits
// with (attempt one). Each brief states the expected pitch class in words,
// which is exactly what the pitch check below verifies on the take. (The
// scripted TTS's tone is a deterministic function of the brief — the
// Playwright spec in web/e2e computes each brief's take F0 up front, the
// way web/e2e/cast-voice-check.spec.ts does.)
var Briefs = map[string]string{
	"adult_male":   "A mature adult man, a deep resonant voice, unmistakably male and low-pitched, plain and level.",
	"adult_female": "An adult woman, a mid-range voice, clearly female, low-key and unforced.",
	"elder_male":   "An elderly man, a deep baritone, unmistakably male and low-pitched, unhurried and natural.",
	"elder_female": "An elderly woman, a mid-range voice with the weight of age, clearly female, clear and unhurried.",
	"boy":          "A young boy, a small child's voice, high-pitched and bright, steady and warm.",
	"girl":         "A young girl, a mid-range voice, clearly female and youthful, steady and warm.",
	"creature":     "A non-human creature: an alien, inhuman timbre, neither a man's nor a woman's voice.",
}

// retryNotes is the stronger pitch wording the retry attempts append: the
// out-of-class take (issue #51's ~219 Hz "adult male") is re-auditioned with
// the class stated more forcefully.
var retryNotes = map[string]string{
	ClassMale:   " This take must be a DEEP, LOW baritone — unmistakably male, far below any female or child pitch.",
	ClassFemale: " This take must be a clearly adult FEMALE voice in the mid range — clearly higher than any male voice, far below a child's pitch.",
	ClassChild:  " This take must be a small CHILD's voice — high-pitched, far higher than any adult voice.",
}

// BriefFor is the create-missing run's brief for one kind on one attempt
// (attempt starts at 1): the fixed brief, with the stronger pitch wording
// appended from attempt two onward.
func BriefFor(kind string, attempt int) string {
	brief := Briefs[kind]
	if attempt > 1 {
		if note, ok := retryNotes[KindToClass(kind)]; ok {
			brief += note
		}
	}
	return brief
}

// The kind coverage states.
const (
	KindStateMissing  = "missing"
	KindStatePending  = "pending"
	KindStateApproved = "approved"
)

// MaxKindAttempts is the create-missing run's total attempts per kind: one
// audition plus up to 2 retries while the measured class is wrong.
const MaxKindAttempts = 3

// KindRow is one kind's state on the cast page.
type KindRow struct {
	Kind         string  `json:"kind"`
	Character    string  `json:"character"`
	State        string  `json:"state"` // missing | pending | approved
	VoiceID      string  `json:"voice_id,omitempty"`
	Revision     int     `json:"revision,omitempty"`
	MeasuredF0Hz float64 `json:"measured_f0_hz"`
	Measured     string  `json:"measured"` // the class label, or "unmeasured"
	Attempts     int     `json:"attempts,omitempty"`
	Note         string  `json:"note,omitempty"`
}

// KindAuditions is the audition seam: satisfied by *voices.Service, which
// composes the draft revision and the take generation — the create-missing
// run reuses the existing audition path, it does not duplicate it.
type KindAuditions interface {
	CreateAuditions(ctx context.Context, character, brief string, n int) (store.VoiceProfile, []store.VoiceAudition, error)
}

// KindProfiles is the revision-listing seam: satisfied by *store.Store.
type KindProfiles interface {
	ListVoiceProfiles(ctx context.Context, character string) ([]store.VoiceProfile, error)
	ListAuditions(ctx context.Context, voiceProfileID string) ([]store.VoiceAudition, error)
}

// KindManager reads kind coverage and runs the create-missing audition over
// the existing voice workflow.
type KindManager struct {
	Auditions KindAuditions
	Profiles  KindProfiles
	Assets    assets.Store
}

// NewKindManager wires the manager. All three seams are required: a missing
// one would make coverage or the run silently lie about the state.
func NewKindManager(auditions KindAuditions, profiles KindProfiles, a assets.Store) *KindManager {
	return &KindManager{Auditions: auditions, Profiles: profiles, Assets: a}
}

// Coverage reports one row per kind. Approved: the highest approved revision
// carrying a fingerprint and reference (the runtime's selection rule, ADR-0171
// audit). Pending: no approved revision, but a draft/auditioning revision
// carries a take with audio — a voice waiting on the operator's approve
// click. Missing: nothing at all.
func (m *KindManager) Coverage(ctx context.Context) ([]KindRow, error) {
	rows := make([]KindRow, 0, len(Kinds))
	for _, kind := range Kinds {
		character := KindCharacter(kind)
		revs, err := m.Profiles.ListVoiceProfiles(ctx, character)
		if err != nil {
			return nil, fmt.Errorf("cast kinds: list %s: %w", character, err)
		}
		row := KindRow{Kind: kind, Character: character, State: KindStateMissing, Measured: "unmeasured"}
		for i := range revs { // ordered by revision, descending
			rev := revs[i]
			if rev.State == store.VoiceStateApproved && rev.Fingerprint != "" && rev.ReferenceAssetID != "" {
				row.State = KindStateApproved
				row.VoiceID = rev.ID
				row.Revision = rev.Revision
				m.measure(ctx, &row, rev.ReferenceAssetID)
				break
			}
			if row.State == KindStateMissing && (rev.State == store.VoiceStateDraft || rev.State == store.VoiceStateAuditioning) {
				if take, err := m.firstTake(ctx, rev.ID); err != nil {
					return nil, err
				} else if take != "" {
					row.State = KindStatePending
					row.VoiceID = rev.ID
					row.Revision = rev.Revision
					m.measure(ctx, &row, take)
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// CreateMissing runs the one-click create for every kind whose state is
// missing, and returns the full coverage after the run. A kind that already
// has an approved voice or a pending take is left exactly where it is.
func (m *KindManager) CreateMissing(ctx context.Context) ([]KindRow, error) {
	rows, err := m.Coverage(ctx)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].State != KindStateMissing {
			continue
		}
		m.createOne(ctx, &rows[i])
	}
	return rows, nil
}

// createOne audits the missing kind until its take's measured class matches,
// within MaxKindAttempts, and leaves the last take PENDING for the operator.
func (m *KindManager) createOne(ctx context.Context, row *KindRow) {
	class := KindToClass(row.Kind)
	designFailed := false
	for attempt := 1; attempt <= MaxKindAttempts; attempt++ {
		rev, auds, err := m.Auditions.CreateAuditions(ctx, row.Character, BriefFor(row.Kind, attempt), 1)
		if err != nil {
			row.Note = fmt.Sprintf("the audition failed on attempt %d: %v", attempt, err)
			designFailed = true
			break
		}
		take := firstUsableTake(auds)
		if take == "" {
			row.Note = fmt.Sprintf("attempt %d produced no usable take (QA read-back failed)", attempt)
			continue
		}
		wav, err := m.assetBytes(ctx, take)
		if err != nil {
			row.Note = err.Error()
			continue
		}
		pitch, err := speech.MeasurePitch(wav)
		if err != nil {
			row.Note = fmt.Sprintf("the take could not be measured: %v", err)
			continue
		}
		row.State = KindStatePending
		row.VoiceID = rev.ID
		row.Revision = rev.Revision
		row.Attempts = attempt
		row.MeasuredF0Hz = pitch.F0
		row.Measured = speech.VoiceClass(pitch.F0)
		if class == ClassUnknown || Verdict(class, pitch.F0) == VerdictOK {
			row.Note = "the take's pitch matches the kind — pending the approve click"
			return
		}
		// Out-of-class take: flagged, and re-auditioned with the stronger
		// pitch wording while attempts remain.
		if attempt < MaxKindAttempts {
			row.Note = fmt.Sprintf("attempt %d measured %s (%.0f Hz), not %s — re-auditioning",
				attempt, row.Measured, pitch.F0, class)
			continue
		}
		row.Note = fmt.Sprintf("the pitch stayed out of class after %d attempts — flagged for review", attempt)
	}
	if designFailed {
		row.State = KindStateMissing // the row is still the honest state
	}
}

// firstTake returns the asset SHA of the revision's first recorded take, or
// "" when the revision carries no take with audio.
func (m *KindManager) firstTake(ctx context.Context, revisionID string) (string, error) {
	auds, err := m.Profiles.ListAuditions(ctx, revisionID)
	if err != nil {
		return "", fmt.Errorf("cast kinds: list takes of %s: %w", revisionID, err)
	}
	for _, a := range auds {
		if a.AssetSHA256 != "" {
			return a.AssetSHA256, nil
		}
	}
	return "", nil
}

// firstUsableTake picks the first take the run can stand behind: it has audio
// and its QA read-back did not fail. A failed QA is a flag, not a verdict on
// the pitch, so it costs the take (the run re-auditions), while a not-run QA
// (the ASR role absent) is acceptable.
func firstUsableTake(auds []store.VoiceAudition) string {
	for _, a := range auds {
		if a.AssetSHA256 == "" || a.AsrState == store.VoiceAuditionQAFailed {
			continue
		}
		return a.AssetSHA256
	}
	return ""
}

func (m *KindManager) measure(ctx context.Context, row *KindRow, sha string) {
	wav, err := m.assetBytes(ctx, sha)
	if err != nil {
		row.Note = err.Error()
		return
	}
	pitch, err := speech.MeasurePitch(wav)
	if err != nil {
		row.Note = fmt.Sprintf("the take could not be measured: %v", err)
		return
	}
	row.MeasuredF0Hz = pitch.F0
	row.Measured = speech.VoiceClass(pitch.F0)
}

func (m *KindManager) assetBytes(ctx context.Context, sha string) ([]byte, error) {
	r, _, err := m.Assets.Get(ctx, sha)
	if err != nil {
		return nil, fmt.Errorf("cast kinds: read take asset: %v", err)
	}
	defer r.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, fmt.Errorf("cast kinds: read take asset: %v", err)
	}
	return buf.Bytes(), nil
}
