// The create-missing orchestration (issue #51, ADR-0185) with a fake voice
// service and synthetic tones (110 / 220 / 320 Hz — male, female and child
// range): the fixed briefs, the retry on a wrong measured class, the pending
// hand-off to the operator, and the coverage read. No live model, no game
// text (rule 5).

package cast

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

// queuedTake is one canned audition: the take's asset SHA and its QA state.
type queuedTake struct {
	sha string
	qa  store.VoiceAuditionQAState
}

// fakeKindService is the create-missing seam with canned takes: each call
// spends the next take queued for that character (tone + QA state) or the
// next queued error for it.
type fakeKindService struct {
	takes map[string][]queuedTake
	errs  map[string][]error
	// briefs records the brief of every attempt, in call order.
	briefs []string
	calls  int
	// revs maps character -> its (single, highest) revision.
	revs map[string]store.VoiceProfile
	// auds maps revision id -> its takes.
	auds map[string][]store.VoiceAudition
}

func newFakeKindService() *fakeKindService {
	return &fakeKindService{
		takes: map[string][]queuedTake{},
		errs:  map[string][]error{},
		revs:  map[string]store.VoiceProfile{},
		auds:  map[string][]store.VoiceAudition{},
	}
}

func (f *fakeKindService) queueTone(kind, sha string, qa store.VoiceAuditionQAState) {
	f.takes[KindCharacter(kind)] = append(f.takes[KindCharacter(kind)], queuedTake{sha: sha, qa: qa})
}

func (f *fakeKindService) queueError(kind string, err error) {
	f.errs[KindCharacter(kind)] = append(f.errs[KindCharacter(kind)], err)
}

func (f *fakeKindService) CreateAuditions(_ context.Context, character, brief string, _ int) (store.VoiceProfile, []store.VoiceAudition, error) {
	f.calls++
	f.briefs = append(f.briefs, brief)
	if len(f.errs[character]) > 0 {
		err := f.errs[character][0]
		f.errs[character] = f.errs[character][1:]
		return store.VoiceProfile{}, nil, err
	}
	if len(f.takes[character]) == 0 {
		return store.VoiceProfile{}, nil, fmt.Errorf("fake: no take queued for %s (call %d)", character, f.calls)
	}
	queued := f.takes[character][0]
	f.takes[character] = f.takes[character][1:]
	sha := queued.sha
	rev := store.VoiceProfile{
		ID:        "rev-" + character + "-" + fmt.Sprint(f.calls),
		Character: character,
		Revision:  1,
		State:     store.VoiceStateDraft,
	}
	f.revs[character] = rev
	aud := store.VoiceAudition{
		ID:             "aud-" + sha,
		VoiceProfileID: rev.ID,
		AssetSHA256:    sha,
		AsrState:       queued.qa,
	}
	f.auds[rev.ID] = []store.VoiceAudition{aud}
	return rev, f.auds[rev.ID], nil
}

// fakeKindProfiles is the revision-listing seam over the fake's state
// (one revision per character, the store's highest-first order).
type fakeKindProfiles struct{ svc *fakeKindService }

func (f *fakeKindProfiles) ListVoiceProfiles(_ context.Context, character string) ([]store.VoiceProfile, error) {
	p, ok := f.svc.revs[character]
	if !ok {
		return nil, nil
	}
	return []store.VoiceProfile{p}, nil
}

func (f *fakeKindProfiles) ListAuditions(_ context.Context, voiceProfileID string) ([]store.VoiceAudition, error) {
	return f.svc.auds[voiceProfileID], nil
}

func newKindEnv(t *testing.T, svc *fakeKindService) *KindManager {
	t.Helper()
	as, err := assets.NewLocal(filepath.Join(t.TempDir(), "assets"))
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}
	return &KindManager{Auditions: svc, Profiles: &fakeKindProfiles{svc: svc}, Assets: as}
}

// toneAsset commits one synthetic tone and returns its SHA, the way the
// real workflow commits a take's bytes.
func toneAsset(t *testing.T, as assets.Store, ctx context.Context, freq float64) string {
	t.Helper()
	wav := speech.MakeToneWAV(freq, 1.5, 44100)
	stg, _, err := as.PutStaged(ctx, bytes.NewReader(wav), "")
	if err != nil {
		t.Fatalf("PutStaged: %v", err)
	}
	info, err := as.Commit(ctx, stg)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return info.SHA256
}

// sealAllBut plants an approved voice on every kind but open, so the
// single-kind tests' run only ever has the one kind under test to create.
func sealAllBut(t *testing.T, m *KindManager, svc *fakeKindService, open string) {
	t.Helper()
	ctx := context.Background()
	for _, kind := range Kinds {
		if kind == open {
			continue
		}
		sha := toneAsset(t, m.Assets, ctx, 110)
		svc.revs[KindCharacter(kind)] = store.VoiceProfile{
			ID: "sealed-" + kind, Character: KindCharacter(kind), Revision: 1,
			State: store.VoiceStateApproved, Fingerprint: strings.Repeat("f", 64),
			ReferenceAssetID: sha,
		}
	}
}

func TestBriefForUsesStrongerPitchWordingOnRetries(t *testing.T) {
	if !strings.Contains(BriefFor("adult_male", 2), "DEEP, LOW") {
		t.Errorf("the retry brief must restate the pitch: %q", BriefFor("adult_male", 2))
	}
	if BriefFor("adult_male", 1) == BriefFor("adult_male", 2) {
		t.Error("attempt two must differ from attempt one")
	}
	// The creature has no class to restate: the retry is the same brief.
	if BriefFor("creature", 1) != BriefFor("creature", 3) {
		t.Errorf("the creature's brief must not gain pitch wording: %q", BriefFor("creature", 3))
	}
}

func TestKindToClassCoversTheSevenKinds(t *testing.T) {
	want := map[string]string{
		"adult_male": ClassMale, "elder_male": ClassMale,
		"adult_female": ClassFemale, "elder_female": ClassFemale, "girl": ClassFemale,
		"boy": ClassChild, "creature": ClassUnknown,
	}
	if len(Kinds) != len(want) {
		t.Fatalf("Kinds has %d entries, want %d", len(Kinds), len(want))
	}
	for _, k := range Kinds {
		if got := KindToClass(KindCharacter(k)); got != want[k] {
			t.Errorf("KindToClass(%q) = %q, want %q", k, got, want[k])
		}
	}
}

func TestCreateMissingAuditionsEachMissingKindOnce(t *testing.T) {
	ctx := context.Background()
	svc := newFakeKindService()
	m := newKindEnv(t, svc)
	// Every kind lands in class on its first take.
	freqs := map[string]float64{
		"adult_male": 110, "elder_male": 110,
		"adult_female": 220, "elder_female": 220, "girl": 220,
		"boy": 320, "creature": 220,
	}
	for kind, freq := range freqs {
		svc.queueTone(kind, toneAsset(t, m.Assets, ctx, freq), store.VoiceAuditionQAPassed)
	}
	rows, err := m.CreateMissing(ctx)
	if err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}
	if len(rows) != len(Kinds) {
		t.Fatalf("coverage has %d rows, want %d", len(rows), len(Kinds))
	}
	byKind := map[string]KindRow{}
	for _, r := range rows {
		byKind[r.Kind] = r
		if r.State != KindStatePending {
			t.Errorf("%s state = %q, want pending (the approve click is the operator's)", r.Kind, r.State)
		}
		if r.Attempts != 1 {
			t.Errorf("%s attempts = %d, want 1 (the first take was in class)", r.Kind, r.Attempts)
		}
		if r.VoiceID == "" {
			t.Errorf("%s: no voice id on the pending row", r.Kind)
		}
	}
	if got := byKind["adult_male"]; got.MeasuredF0Hz <= 0 || got.Measured != ClassMale {
		t.Errorf("adult_male measured %s (%.0f Hz), want the male class", got.Measured, got.MeasuredF0Hz)
	}
	if got := byKind["boy"]; got.Measured != ClassChild {
		t.Errorf("boy measured %s, want the child class", got.Measured)
	}
	if got := byKind["creature"]; got.Measured == "unmeasured" {
		t.Error("creature's take measured nothing")
	}
	if svc.calls != len(Kinds) {
		t.Errorf("auditions created = %d, want one per kind (%d)", svc.calls, len(Kinds))
	}
}

// The 110 Hz "adult male" take is in class; a 220 Hz take is not — the run
// must retry with the stronger brief and stop when the class lands (issue
// #51's 219 Hz failure, in miniature).
func TestCreateMissingRetriesAnOutOfClassTake(t *testing.T) {
	ctx := context.Background()
	svc := newFakeKindService()
	m := newKindEnv(t, svc)
	sealAllBut(t, m, svc, "adult_male")

	// Take one: 220 Hz (female range) — out of class. Take two: 110 Hz —
	// in class, so the run stops on attempt two.
	svc.queueTone("adult_male", toneAsset(t, m.Assets, ctx, 220), store.VoiceAuditionQAPassed)
	svc.queueTone("adult_male", toneAsset(t, m.Assets, ctx, 110), store.VoiceAuditionQAPassed)

	rows, err := m.CreateMissing(ctx)
	if err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}
	var row KindRow
	for _, r := range rows {
		if r.Kind == "adult_male" {
			row = r
		}
	}
	if row.State != KindStatePending {
		t.Fatalf("adult_male state = %q, want pending", row.State)
	}
	if row.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (one retry after the out-of-class take)", row.Attempts)
	}
	if row.Measured != ClassMale {
		t.Errorf("measured = %q, want %q", row.Measured, ClassMale)
	}
	if len(svc.briefs) != 2 {
		t.Fatalf("briefs seen = %d, want 2", len(svc.briefs))
	}
	if svc.briefs[0] != BriefFor("adult_male", 1) || svc.briefs[1] != BriefFor("adult_male", 2) {
		t.Errorf("briefs = %q, want the fixed brief then the stronger one", svc.briefs)
	}
	if !strings.Contains(svc.briefs[1], "DEEP, LOW") {
		t.Error("the retry brief must carry the stronger pitch wording")
	}
}

func TestCreateMissingGivesUpAfterTwoRetriesButKeepsTheTake(t *testing.T) {
	ctx := context.Background()
	svc := newFakeKindService()
	m := newKindEnv(t, svc)
	sealAllBut(t, m, svc, "adult_male")
	// Three takes, all 220 Hz (out of class for adult_male).
	for i := 0; i < MaxKindAttempts; i++ {
		svc.queueTone("adult_male", toneAsset(t, m.Assets, ctx, 220), store.VoiceAuditionQAPassed)
	}
	rows, err := m.CreateMissing(ctx)
	if err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}
	var row KindRow
	for _, r := range rows {
		if r.Kind == "adult_male" {
			row = r
		}
	}
	if svc.calls != MaxKindAttempts {
		t.Fatalf("auditions = %d, want %d (the initial plus 2 retries)", svc.calls, MaxKindAttempts)
	}
	// The take stays pending — flagged, never auto-approved (rule 6).
	if row.State != KindStatePending || row.Attempts != MaxKindAttempts {
		t.Errorf("row = %+v, want the flagged pending take after %d attempts", row, MaxKindAttempts)
	}
	if !strings.Contains(row.Note, "out of class") {
		t.Errorf("note = %q, want the out-of-class flag", row.Note)
	}
}

func TestCreateMissingSkipsKindsThatAreNotMissing(t *testing.T) {
	ctx := context.Background()
	svc := newFakeKindService()
	m := newKindEnv(t, svc)
	sealAllBut(t, m, svc, "adult_male")

	// boy already has an approved voice, so the run must leave it alone.
	boySHA := toneAsset(t, m.Assets, ctx, 320)
	svc.revs[KindCharacter("boy")] = store.VoiceProfile{
		ID: "approved-boy", Character: KindCharacter("boy"), Revision: 1,
		State: store.VoiceStateApproved, Fingerprint: strings.Repeat("f", 64),
		ReferenceAssetID: boySHA,
	}
	// adult_male is missing and lands in class on the first take.
	svc.queueTone("adult_male", toneAsset(t, m.Assets, ctx, 110), store.VoiceAuditionQAPassed)

	rows, err := m.CreateMissing(ctx)
	if err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}
	byKind := map[string]KindRow{}
	for _, r := range rows {
		byKind[r.Kind] = r
	}
	if got := byKind["boy"]; got.State != KindStateApproved || got.VoiceID != "approved-boy" {
		t.Errorf("boy = %+v, want the approved voice untouched", got)
	}
	if got := byKind["adult_male"]; got.State != KindStatePending {
		t.Errorf("adult_male = %+v, want the new pending take", got)
	}
	if svc.calls != 1 {
		t.Fatalf("auditions = %d, want exactly 1 (only the missing kind)", svc.calls)
	}
}

func TestCreateMissingReportsDesignFailure(t *testing.T) {
	ctx := context.Background()
	svc := newFakeKindService()
	m := newKindEnv(t, svc)
	sealAllBut(t, m, svc, "adult_male")
	svc.queueError("adult_male", fmt.Errorf("the design checkpoint refused the brief"))
	rows, err := m.CreateMissing(ctx)
	if err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}
	var row KindRow
	for _, r := range rows {
		if r.Kind == "adult_male" {
			row = r
		}
	}
	if row.State != KindStateMissing {
		t.Errorf("state = %q, want missing (the run failed before a take)", row.State)
	}
	if row.Note == "" {
		t.Error("the design failure must be reported on the row")
	}
}

func TestCoverageReadsMissingPendingApproved(t *testing.T) {
	ctx := context.Background()
	svc := newFakeKindService()
	m := newKindEnv(t, svc)

	// adult_male: approved (110 Hz — male class).
	approvedSHA := toneAsset(t, m.Assets, ctx, 110)
	svc.revs[KindCharacter("adult_male")] = store.VoiceProfile{
		ID: "v-approved", Character: KindCharacter("adult_male"), Revision: 1,
		State: store.VoiceStateApproved, Fingerprint: strings.Repeat("f", 64),
		ReferenceAssetID: approvedSHA,
	}
	// adult_female: pending — a draft revision with a 220 Hz take.
	pendingSHA := toneAsset(t, m.Assets, ctx, 220)
	pendingRev := store.VoiceProfile{
		ID: "v-pending", Character: KindCharacter("adult_female"), Revision: 1,
		State: store.VoiceStateDraft,
	}
	svc.revs[KindCharacter("adult_female")] = pendingRev
	svc.auds[pendingRev.ID] = []store.VoiceAudition{{
		ID: "a1", VoiceProfileID: pendingRev.ID, AssetSHA256: pendingSHA,
	}}
	// boy: missing — nothing at all.

	rows, err := m.Coverage(ctx)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	byKind := map[string]KindRow{}
	for _, r := range rows {
		byKind[r.Kind] = r
	}
	male := byKind["adult_male"]
	if male.State != KindStateApproved || male.Measured != ClassMale {
		t.Errorf("adult_male = %+v, want approved with the male class", male)
	}
	female := byKind["adult_female"]
	if female.State != KindStatePending || female.Measured != ClassFemale {
		t.Errorf("adult_female = %+v, want pending with the female class", female)
	}
	if byKind["boy"].State != KindStateMissing {
		t.Errorf("boy state = %q, want missing", byKind["boy"].State)
	}
}

// The kind id with and without the roster prefix maps to the same class.
func TestKindToClassIgnoresTheRosterPrefix(t *testing.T) {
	for _, k := range Kinds {
		if KindToClass(k) != KindToClass("kind:"+k) {
			t.Errorf("KindToClass(%q) != KindToClass(kind:%q)", k, k)
		}
	}
}
