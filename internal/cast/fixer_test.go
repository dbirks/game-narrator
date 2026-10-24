package cast

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// toneDesigner is a scripted voices.DesignClient: every design call returns
// a fixed synthetic tone, so the fixer's retry/idempotence logic is
// exercised without a gateway.
type toneDesigner struct {
	freqs []float64 // one entry per Design call, in order; the last repeats
	model string
	calls int
}

func (d *toneDesigner) Design(ctx context.Context, req tts.DesignRequest) (tts.Audio, error) {
	f := 220.0
	if len(d.freqs) > 0 {
		i := d.calls
		if i >= len(d.freqs) {
			i = len(d.freqs) - 1
		}
		f = d.freqs[i]
	}
	d.calls++
	return tts.Audio{
		WAV:   speech.MakeToneWAV(f, 1.5, 44100),
		Model: d.model, Duration: 0, SampleRate: 44100,
		Origin: "design",
	}, nil
}

func newFixEnv(t *testing.T, design *toneDesigner) (*Fixer, *store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, store.DBPath(dir))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	assetStore, err := assets.NewLocal(filepath.Join(dir, "assets"))
	if err != nil {
		t.Fatalf("open asset store: %v", err)
	}
	svc := &voices.Service{
		Store: st, Assets: assetStore, Design: design,
		Language: "en", InstructionCap: 500, InstanceID: voices.DefaultInstanceID,
	}
	aud := NewAuditor(st, assetStore)
	return &Fixer{Aud: aud, Svc: svc}, st, ctx
}

// TestFixPendingAndApproval: the first take already reads female; the fix
// leaves it PENDING by default and approves it with --approve semantics.
func TestFixPendingAndApproval(t *testing.T) {
	design := &toneDesigner{freqs: []float64{220}}
	f, _, ctx := newFixEnv(t, design)
	res, err := f.FixCharacter(ctx, "pn_test_profile", "Test Knight", ClassFemale, 110, false)
	if err != nil {
		t.Fatalf("FixCharacter: %v", err)
	}
	if res.Verdict != VerdictOK || res.Approved || res.Attempts != 1 {
		t.Fatalf("fix result = %+v, want OK pending one attempt", res)
	}
	rev, err := f.Aud.Store.GetVoiceRevision(ctx, res.NewVoiceID)
	if err != nil {
		t.Fatalf("load new revision: %v", err)
	}
	if rev.State != store.VoiceStateAuditioning {
		t.Fatalf("pending revision state = %q, want %q", rev.State, store.VoiceStateAuditioning)
	}

	// The idempotent re-run (without --approve) re-measures the pending take:
	// no new design call, the same voice comes back pending.
	res1b, err := f.FixCharacter(ctx, "pn_test_profile", "Test Knight", ClassFemale, 110, false)
	if err != nil {
		t.Fatalf("idempotent re-measure: %v", err)
	}
	if res1b.NewVoiceID != res.NewVoiceID || res1b.Approved || design.calls != 1 {
		t.Fatalf("re-measure = %+v design calls=%d, want same pending voice with no new design", res1b, design.calls)
	}

	// The approving re-run re-measures the pending take: no new design call.
	res2, err := f.FixCharacter(ctx, "pn_test_profile", "Test Knight", ClassFemale, 110, true)
	if err != nil {
		t.Fatalf("idempotent fix: %v", err)
	}
	if design.calls != 1 {
		t.Fatalf("approving re-run made %d design calls, want 0 new", design.calls-1)
	}
	if err != nil {
		t.Fatalf("idempotent fix: %v", err)
	}
	if res2.NewVoiceID != res.NewVoiceID || !res2.Approved {
		t.Fatalf("idempotent fix = %+v, want same voice approved", res2)
	}
	rev2, _ := f.Aud.Store.GetVoiceRevision(ctx, res2.NewVoiceID)
	if rev2.State != store.VoiceStateApproved {
		t.Fatalf("approved revision state = %q, want approved", rev2.State)
	}
}

// TestFixRetriesUntilRight: the first two takes read male (110 Hz), the third
// reads female — the retry loop must stop there with the third take.
func TestFixRetriesUntilRight(t *testing.T) {
	f, _, ctx := newFixEnv(t, &toneDesigner{freqs: []float64{110, 110, 220}})
	res, err := f.FixCharacter(ctx, "pn_test_profile", "Test Maiden", ClassFemale, 110, false)
	if err != nil {
		t.Fatalf("FixCharacter: %v", err)
	}
	if res.Verdict != VerdictOK || res.Attempts != 3 {
		t.Fatalf("fix result = %+v, want OK after 3 attempts", res)
	}
}

// TestFixExhausted: every take reads the wrong class; the retries exhaust and
// the last (still pending) take is reported as MISMATCH, not an error.
func TestFixExhausted(t *testing.T) {
	f, _, ctx := newFixEnv(t, &toneDesigner{freqs: []float64{110}})
	res, err := f.FixCharacter(ctx, "pn_test_profile", "Test Maiden", ClassFemale, 110, false)
	if err != nil {
		t.Fatalf("FixCharacter: %v", err)
	}
	if res.Attempts != 1+FixMaxRetries || res.Verdict != VerdictMismatch {
		t.Fatalf("exhausted fix = %+v, want %d attempts MISMATCH", res, 1+FixMaxRetries)
	}
	rev, _ := f.Aud.Store.GetVoiceRevision(ctx, res.NewVoiceID)
	if rev.State != store.VoiceStateAuditioning {
		t.Fatalf("exhausted take state = %q, want pending", rev.State)
	}
}

func TestFixBriefStatesGender(t *testing.T) {
	b := FixBrief("warm, confident delivery", ClassMale)
	if !isFixBrief(b) {
		t.Fatalf("fix brief %q does not carry the marker", b)
	}
	if !strings.Contains(strings.ToUpper(b), "ADULT MAN") {
		t.Fatalf("fix brief must state the expected gender: %q", b)
	}
}
