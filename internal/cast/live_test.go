package cast

// LIVE cast-audit test (ADR-0171, gate pattern of ADR-0065): a seeded
// mismatch — a character the profile describes as a young woman whose
// approved voice's reference measures male-class (~110 Hz synthetic tone) —
// is detected by the real CLI binary's `cast audit`, then redesigned through
// `cast audit --fix --apply` against the live voice-design checkpoint. The
// fixed take is measured again and must read the expected class; it stays
// PENDING approval (no registry name is ever created, no game text appears
// anywhere — every string here is invented). It SKIPs as "NOT RUN" unless
// the test-models gate and the speech gateway are configured, so
// `go test ./...` stays green offline.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

// liveAuditEnv reports the skip reason; ok is true only under the
// test-models gate with the design gateway configured.
func liveAuditEnv(t *testing.T) bool {
	t.Helper()
	if os.Getenv(providers.EnvGate) != "test-models" {
		t.Skipf("live cast-audit test outside the test-models gate: run via make test-models (it sets %s=test-models)", providers.EnvGate)
	}
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	design := roles[providers.RoleVoiceDesign]
	if m := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL")); m != "" {
		design.Model = m
	}
	if strings.TrimSpace(design.BaseURL) == "" || strings.TrimSpace(design.Token()) == "" {
		t.Skip("NOT RUN: set GAME_NARRATOR_GATEWAY_URL and the token env named by " +
			providers.EnvGatewayTokenEnv + " to run the live cast-audit test")
	}
	direct := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"))
	if direct == "" {
		direct = strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	}
	if direct == "" {
		t.Skip("NOT RUN: set GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL (or GAME_NARRATOR_TTS_BASE_DIRECT_URL) to the direct Base service to run the live cast-audit test")
	}
	return true
}

type liveAuditJSON struct {
	Profile string `json:"profile"`
	Rows    []struct {
		Character    string  `json:"character"`
		VoiceID      string  `json:"voice_id"`
		Expected     string  `json:"expected"`
		MeasuredF0Hz float64 `json:"measured_f0_hz"`
		Measured     string  `json:"measured"`
		Verdict      string  `json:"verdict"`
	} `json:"rows"`
}

func TestLiveCastAuditFix(t *testing.T) {
	liveAuditEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	// ── local test store: the seeded mismatch ──────────────────────────
	dataDir := t.TempDir()
	st, err := store.Open(ctx, store.DBPath(dataDir))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	assetStore, err := assets.NewLocal(filepath.Join(dataDir, "assets"))
	if err != nil {
		t.Fatalf("assets: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	g := games.Game{ID: games.NewID(), Slug: "cast-audit-live", Title: "Cast Audit Live", Platform: "wii", CreatedAt: now}
	if err := g.Validate(); err != nil {
		t.Fatalf("game: %v", err)
	}
	if err := st.CreateGame(ctx, g); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	ed := games.Edition{ID: games.NewID(), GameID: g.ID, Region: "us", Language: "en", Revision: "v1"}
	if err := ed.Validate(); err != nil {
		t.Fatalf("edition: %v", err)
	}
	if err := st.CreateEdition(ctx, ed); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	p, err := st.CreateDraftProfile(ctx, ed.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	const character = "Maribel"
	if err := st.PutCharacters(ctx, p.ID, []games.Character{
		{ID: games.NewID(), ProfileID: p.ID, Name: character,
			PersonalitySummary: "a young woman, quick and bright", Status: games.CharacterConfirmed, CreatedAt: now},
	}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}

	// Seed the wrong voice: an approved revision whose reference is a 3 s
	// 110 Hz tone (male class). The brief carries no gender words.
	wav := speech.MakeToneWAV(110, 3.0, 24000)
	sum := sha256.Sum256(wav)
	refSHA := hex.EncodeToString(sum[:])
	sid, _, err := assetStore.PutStaged(ctx, bytes.NewReader(wav), refSHA)
	if err != nil {
		t.Fatalf("PutStaged: %v", err)
	}
	if _, err := assetStore.Commit(ctx, sid); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	seededAt := time.Now().UTC()
	if err := st.CreateVoiceRevision(ctx, store.VoiceProfile{
		ID: games.NewID(), Character: character, Revision: 1,
		State: store.VoiceStateApproved, DesignInstruction: "A calm, steady voice.",
		ReferenceAssetID: refSHA, Fingerprint: refSHA, Language: "English",
		ApprovedAt: &seededAt, CreatedAt: seededAt, UpdatedAt: seededAt,
	}); err != nil {
		t.Fatalf("seed approved voice: %v", err)
	}

	// ── build the CLI binary the operator would run ────────────────────
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "game-narrator")
	b := exec.Command("go", "build", "-o", bin, "github.com/dbirks/game-narrator/cmd/game-narrator")
	b.Dir = root
	if out, err := b.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	run := func(args ...string) (string, int) {
		c := exec.Command(bin, args...)
		c.Dir = root
		var buf bytes.Buffer
		c.Stdout = &buf
		c.Stderr = &buf
		err := c.Run()
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				t.Fatalf("run %v: %v", args, err)
			}
		}
		return buf.String(), code
	}

	// 1. Audit: the seeded mismatch must be detected, with the measured F0.
	out, code := run("cast", "audit", "--profile", p.ID, "--json", "--data-dir", dataDir)
	if code != 1 {
		t.Fatalf("audit exit = %d, want 1 (mismatch)\n%s", code, out)
	}
	var rep liveAuditJSON
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("audit json: %v\n%s", err, out)
	}
	var row *struct {
		Character    string  `json:"character"`
		VoiceID      string  `json:"voice_id"`
		Expected     string  `json:"expected"`
		MeasuredF0Hz float64 `json:"measured_f0_hz"`
		Measured     string  `json:"measured"`
		Verdict      string  `json:"verdict"`
	}
	for i := range rep.Rows {
		if rep.Rows[i].Character == character {
			row = &rep.Rows[i]
		}
	}
	if row == nil {
		t.Fatalf("no row for %s in: %s", character, out)
	}
	if row.Verdict != VerdictMismatch || row.Expected != ClassFemale {
		t.Fatalf("seeded row = %+v, want expected %q verdict MISMATCH", row, ClassFemale)
	}
	if row.MeasuredF0Hz < 90 || row.MeasuredF0Hz > 140 {
		t.Fatalf("seeded measured F0 = %.1f Hz, want ~110", row.MeasuredF0Hz)
	}
	t.Logf("audit: %s expected %s measured %.1f Hz (%s) verdict %s",
		row.Character, row.Expected, row.MeasuredF0Hz, row.Measured, row.Verdict)

	// 2. Fix: redesign against the live voice-design checkpoint. The new
	// take must read the expected class and stay PENDING review.
	fixStart := time.Now()
	fout, fcode := run("cast", "audit", "--profile", p.ID, "--fix", "--apply", "--data-dir", dataDir)
	if fcode != 1 {
		t.Fatalf("fix exit = %d, want 1 (the fix stays pending, so the old mismatched voice still speaks)\n%s", fcode, fout)
	}
	if !strings.Contains(fout, "pending review") || !strings.Contains(fout, "->") {
		t.Fatalf("fix output lacks the before->after pending-review report:\n%s", fout)
	}
	t.Logf("fix (%s):\n%s", time.Since(fixStart).Round(time.Millisecond), strings.TrimSpace(fout))

	// 3. Verify the store: the new revision carries the fix brief, is
	// pending, and its take's measured F0 reads the expected class.
	revs, err := st.ListVoiceProfiles(ctx, character)
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	var fixed *store.VoiceProfile
	for i := range revs {
		if strings.Contains(revs[i].DesignInstruction, fixMarker) &&
			(revs[i].State == store.VoiceStateAuditioning || revs[i].State == store.VoiceStateDraft) {
			fixed = &revs[i]
		}
	}
	if fixed == nil {
		t.Fatalf("no pending fix revision after --fix --apply (revisions: %+v)", revs)
	}
	auds, err := st.ListAuditions(ctx, fixed.ID)
	if err != nil || len(auds) == 0 {
		t.Fatalf("fix revision has no auditions: %v", err)
	}
	r, _, err := assetStore.Get(ctx, auds[0].AssetSHA256)
	if err != nil {
		t.Fatalf("read fixed take: %v", err)
	}
	var take bytes.Buffer
	if _, err := take.ReadFrom(r); err != nil {
		t.Fatalf("read fixed take: %v", err)
	}
	_ = r.Close()
	p2, err := speech.MeasurePitch(take.Bytes())
	if err != nil {
		t.Fatalf("measure fixed take: %v", err)
	}
	if p2.F0 <= 0 {
		t.Fatalf("fixed take has no voiced frames")
	}
	if got := ClassOfF0(p2.F0); got != ClassFemale {
		t.Fatalf("fixed take F0 %.1f Hz reads %q, want %q", p2.F0, got, ClassFemale)
	}
	t.Logf("verified: take %s (revision %d) measured %.1f Hz (%s); state=%s; pending approval",
		fixed.ID, fixed.Revision, p2.F0, ClassOfF0(p2.F0), fixed.State)

	// 4. Idempotence: re-running the fix re-measures the pending take and
	// opens no further draft.
	beforeN := len(revs)
	rout, _ := run("cast", "audit", "--profile", p.ID, "--fix", "--apply", "--data-dir", dataDir)
	revs2, err := st.ListVoiceProfiles(ctx, character)
	if err != nil {
		t.Fatalf("re-list revisions: %v", err)
	}
	if len(revs2) != beforeN {
		t.Fatalf("idempotent re-run opened %d extra revision(s), want 0\n%s", len(revs2)-beforeN, rout)
	}
	if !strings.Contains(rout, "pending review") {
		t.Fatalf("re-run did not report the pending fix take:\n%s", rout)
	}
	t.Logf("idempotent re-run: revision count stable at %d, reported %q", len(revs2), strings.Split(strings.TrimSpace(rout), "\n")[0])
}
