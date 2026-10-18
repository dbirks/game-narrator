package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/windwaker"
	"github.com/dbirks/game-narrator/internal/gcdisc"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/store"
)

// The exit code contract lives here: usage errors are 2, runtime failures are
// 1, success is 0 (AGENTS.md CLI targets).

func TestRunUnknownCommandExitsTwo(t *testing.T) {
	code, err := run([]string{"bogus-command"}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (err %v)", code, err)
	}
	if err == nil {
		t.Fatal("unknown command returned nil error")
	}
}

func TestRunNoArgsExitsTwo(t *testing.T) {
	code, _ := run(nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunServeRejectsBadListenAddress(t *testing.T) {
	code, err := run([]string{"serve", "--listen", "not-an-address"},
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (err %v)", code, err)
	}
	if err == nil {
		t.Fatal("invalid listen address returned nil error")
	}
}

func TestRunServeRejectsUnknownFlag(t *testing.T) {
	code, _ := run([]string{"serve", "--nope"}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// TestRunMigrateEndToEnd exercises the real `game-narrator migrate` path
// against a temp data dir: it must create the database, print one line per
// applied migration plus the schema version, and then be a no-op (docs/spec/02
// "Migration and backup acceptance", docs/spec/16 Milestone 0).
func TestRunMigrateEndToEnd(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "narrator-data")
	dbPath := store.DBPath(dataDir)

	first := captureStdout(t, func() int {
		code, err := run([]string{"migrate", "--data-dir", dataDir}, log)
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		return code
	})
	if first.code != 0 {
		t.Fatalf("exit code = %d, want 0", first.code)
	}
	// Derived from the embedded migrations, never typed: the CLI's job is to apply
	// *this build's* migration set, so pinning "three files, version 3" here would
	// turn the next migration into a failing CLI test rather than a schema change.
	want := ""
	for _, name := range store.MigrationNames() {
		want += "applied " + name + "\n"
	}
	want += fmt.Sprintf("schema version %d\n", store.LatestSchemaVersion())
	if first.output != want {
		t.Fatalf("first run output =\n%q\nwant\n%q", first.output, want)
	}

	// The database really exists at the conventional path and really is at the
	// version this build ships: verify through the store, not by parsing our own
	// stdout.
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open after migrate: %v", err)
	}
	if v, err := st.SchemaVersion(context.Background()); err != nil || v != store.LatestSchemaVersion() {
		t.Fatalf("SchemaVersion after migrate = %d, %v; want %d", v, err, store.LatestSchemaVersion())
	}
	if err := st.IntegrityCheck(context.Background()); err != nil {
		t.Fatalf("IntegrityCheck after migrate: %v", err)
	}
	st.Close()

	second := captureStdout(t, func() int {
		code, err := run([]string{"migrate", "--data-dir", dataDir}, log)
		if err != nil {
			t.Fatalf("second migrate: %v", err)
		}
		return code
	})
	if second.code != 0 {
		t.Fatalf("second exit code = %d, want 0", second.code)
	}
	if want := fmt.Sprintf("schema version %d\n", store.LatestSchemaVersion()); second.output != want {
		t.Fatalf("second run output =\n%q\nwant\n%q", second.output, want)
	}
}

// TestRunMigrateHonoursDataDirPrecedence proves the env var is used when no
// flag is given (and the flag still wins over it).
func TestRunMigrateHonoursDataDirPrecedence(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	envDir := filepath.Join(t.TempDir(), "from-env")
	t.Setenv(bootstrap.EnvDataDir, envDir)

	captureStdout(t, func() int {
		code, err := run([]string{"migrate"}, log)
		if err != nil {
			t.Fatalf("migrate from env: %v", err)
		}
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		return code
	})
	if _, err := os.Stat(store.DBPath(envDir)); err != nil {
		t.Fatalf("GAME_NARRATOR_DATA_DIR was not used: %v", err)
	}

	flagDir := filepath.Join(t.TempDir(), "from-flag")
	captureStdout(t, func() int {
		code, _ := run([]string{"migrate", "--data-dir", flagDir}, log)
		return code
	})
	if _, err := os.Stat(store.DBPath(flagDir)); err != nil {
		t.Fatalf("--data-dir was not used: %v", err)
	}
}

func TestRunMigrateUsageAndValidation(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"migrate", "--nope"}},
		{"unexpected argument", []string{"migrate", "extra"}},
		{"relative data dir", []string{"migrate", "--data-dir", "relative/dir"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := run(tc.args, log); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
		})
	}
}

// withStdin points the CLI's password reader at a fixed input, so a test can
// feed a credential the way an operator pipes one from a secret manager.
func withStdin(t *testing.T, input string) {
	t.Helper()
	prev := stdin
	stdin = strings.NewReader(input)
	t.Cleanup(func() { stdin = prev })
}

// adminTokenForTest creates an admin account and one API token for it through
// the CLI, and returns the token. Going through the CLI rather than
// store.AuthStore directly is the point: it proves the rows `admin create` and
// `admin token` write are the rows the server accepts, which is the first-run
// path docs/spec/14 describes ("local-first, set admin on first run").
func adminTokenForTest(t *testing.T, dataDir string, log *slog.Logger, username, role, name string) string {
	t.Helper()
	withStdin(t, "correct horse battery staple\n")
	created := captureStdout(t, func() int {
		code, err := run([]string{"admin", "create", "--username", username, "--role", role, "--data-dir", dataDir}, log)
		if err != nil {
			t.Fatalf("admin create: %v", err)
		}
		return code
	})
	if created.code != 0 {
		t.Fatalf("admin create exit = %d:\n%s", created.code, created.output)
	}
	minted := captureStdout(t, func() int {
		code, err := run([]string{"admin", "token", "--username", username, "--name", name, "--role", role, "--data-dir", dataDir}, log)
		if err != nil {
			t.Fatalf("admin token: %v", err)
		}
		return code
	})
	if minted.code != 0 {
		t.Fatalf("admin token exit = %d:\n%s", minted.code, minted.output)
	}
	raw := strings.TrimSpace(minted.output)
	if !strings.HasPrefix(raw, "gn_") || len(raw) < 20 {
		t.Fatalf("admin token printed %q, want a gn_... secret", raw)
	}
	if strings.Contains(raw, "created") || strings.Contains(strings.ToLower(minted.output), "token") {
		t.Errorf("the token command mixed prose into the secret line: %q", minted.output)
	}
	return raw
}

// capturedStdout holds what a command wrote to stdout plus its exit code.
type capturedStdout struct {
	output string
	code   int
}

// captureStdout redirects the package-level stdout sink so the CLI output can
// be asserted exactly. It restores the original writer when the test ends.
func captureStdout(t *testing.T, fn func() int) capturedStdout {
	t.Helper()
	prev := stdout
	buf := &bytes.Buffer{}
	stdout = buf
	defer func() { stdout = prev }()

	code := fn()
	return capturedStdout{output: buf.String(), code: code}
}

// --- doctor ---------------------------------------------------------------

// clearGatewayEnv makes the doctor CLI tests independent of whatever gateway
// the developer shell happens to export: with these set to empty the roles are
// unconfigured, which is the first-run state under test.
func clearGatewayEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GAME_NARRATOR_GATEWAY_URL", "")
	t.Setenv("GAME_NARRATOR_GATEWAY_TOKEN", "")
	for _, role := range []string{
		"AGENT_REASONING", "FAST_REASONING", "VISION_OCR_FALLBACK", "DECISION",
		"VOICE_DESIGN", "VOICE_CLONE", "SPEECH_ASR",
	} {
		t.Setenv("GAME_NARRATOR_ROLE_"+role+"_MODEL", "")
	}
}

func TestRunDoctorReportsEveryComponentAsJSON(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "narrator")

	out := captureStdout(t, func() int {
		code, err := run([]string{"doctor", "--data-dir", dataDir, "--json"}, log)
		if err != nil {
			t.Fatalf("doctor: %v", err)
		}
		return code
	})
	if out.code != 0 {
		t.Fatalf("exit code = %d, want 0 on a host with no gateway:\n%s", out.code, out.output)
	}

	var report struct {
		GeneratedAt time.Time `json:"generated_at"`
		Status      string    `json:"status"`
		Totals      struct {
			OK            int `json:"ok"`
			Degraded      int `json:"degraded"`
			Blocked       int `json:"blocked"`
			NotConfigured int `json:"not_configured"`
			Fail          int `json:"fail"`
			Checks        int `json:"checks"`
		} `json:"totals"`
		Checks []struct {
			ID       string `json:"id"`
			Group    string `json:"group"`
			Status   string `json:"status"`
			Summary  string `json:"summary"`
			Duration int64  `json:"duration_ns"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out.output), &report); err != nil {
		t.Fatalf("--json output is not the documented schema: %v\n%s", err, out.output)
	}
	if report.Totals.Checks != len(report.Checks) || len(report.Checks) < 15 {
		t.Fatalf("report has %d checks (totals %d), want at least the 15 core components",
			len(report.Checks), report.Totals.Checks)
	}
	if report.Status == "" || report.GeneratedAt.IsZero() {
		t.Fatalf("report envelope incomplete: %+v", report)
	}

	// The IDs are the contract downstream tooling reads; the states are not
	// asserted because this host may legitimately lack ffmpeg or a DAC.
	want := map[string]bool{
		"storage.data_dir": true, "storage.database": true,
		"tools.ffmpeg": true, "tools.v4l2_ctl": true, "tools.tesseract": true,
		"ocr.controlled_image": true, "capture.devices": true, "audio.sinks": true,
		"providers.agent_reasoning": true, "providers.fast_reasoning": true,
		"providers.vision_ocr_fallback": true, "providers.decision": true,
		"providers.voice_design": true, "providers.voice_clone": true,
		"providers.speech_asr": true,
	}
	seen := map[string]bool{}
	for _, c := range report.Checks {
		if c.Status == "" || c.Summary == "" || c.Group == "" {
			t.Errorf("check %s is incomplete: %+v", c.ID, c)
		}
		if c.ID == "emulator.dolphin" || c.ID == "game.image" {
			t.Errorf("%s appeared without --dev", c.ID)
		}
		seen[c.ID] = true
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("doctor did not report %s (docs/spec/16 M0: configured and missing components must both be listed)", id)
		}
	}
}

func TestRunDoctorDevAddsEmulatorChecks(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	out := captureStdout(t, func() int {
		code, _ := run([]string{"doctor", "--data-dir", filepath.Join(t.TempDir(), "d"), "--dev", "--json"}, log)
		return code
	})
	var report struct {
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out.output), &report); err != nil {
		t.Fatalf("--dev --json output: %v\n%s", err, out.output)
	}
	found := map[string]string{}
	for _, c := range report.Checks {
		found[c.ID] = c.Status
	}
	for _, id := range []string{"emulator.dolphin", "game.image"} {
		if _, ok := found[id]; !ok {
			t.Fatalf("%s missing with --dev: %v", id, found)
		}
	}
}

// TestRunDoctorExitsOneWhenAConfiguredComponentIsBroken is the exit-code
// contract scripts rely on: a broken component is 1, a missing optional one is
// not.
func TestRunDoctorExitsOneWhenAConfiguredComponentIsBroken(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() int {
		code, err := run([]string{"doctor", "--data-dir", filepath.Join(blocker, "nested"), "--json"}, log)
		if err != nil {
			t.Fatalf("doctor must not return an error for a failing check, only exit 1: %v", err)
		}
		return code
	})
	if out.code != 1 {
		t.Fatalf("exit code = %d, want 1:\n%s", out.code, out.output)
	}
	if !strings.Contains(out.output, `"id": "storage.data_dir"`) || !strings.Contains(out.output, `"status": "FAIL"`) {
		t.Fatalf("report does not name the failure:\n%s", out.output)
	}
}

func TestRunDoctorHumanOutputIsATable(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	out := captureStdout(t, func() int {
		code, _ := run([]string{"doctor", "--data-dir", filepath.Join(t.TempDir(), "d")}, log)
		return code
	})
	for _, want := range []string{"storage.data_dir", "providers.fast_reasoning", "Totals:", "OK", "NOT CONFIGURED"} {
		if !strings.Contains(out.output, want) {
			t.Fatalf("human report missing %q:\n%s", want, out.output)
		}
	}
}

func TestRunDoctorUsageErrors(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"doctor", "--nope"}},
		{"unexpected argument", []string{"doctor", "extra"}},
		{"relative data dir", []string{"doctor", "--data-dir", "relative/dir"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := run(tc.args, log); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
		})
	}
}

// --- serve readiness --------------------------------------------------------

// TestStoreReadinessTracksTheLiveDatabase exercises the exact function `serve`
// wires into /api/v1/health/ready: it must answer from the open store and flip
// to fail as soon as the store stops answering, which is what turns the endpoint
// into a 503 (see internal/api readiness tests).
func TestStoreReadinessTracksTheLiveDatabase(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(filepath.Join(t.TempDir(), "data")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	ready := storeReadiness(st, log)
	checks, ok := ready(ctx)
	if !ok {
		t.Fatalf("readiness = not ready with a healthy store: %v", checks)
	}
	database, _ := checks["database"].(map[string]any)
	if want := store.LatestSchemaVersion(); database == nil || database["status"] != "ok" || database["schema_version"] != want {
		t.Fatalf("checks.database = %v, want status ok at schema version %d", checks, want)
	}

	// A closed pool is the cheapest reliable way to make the ping fail; the
	// endpoint must not keep claiming readiness after that.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checks, ok = ready(ctx)
	if ok {
		t.Fatalf("readiness = ready after the store was closed: %v", checks)
	}
	database, _ = checks["database"].(map[string]any)
	if database == nil || database["status"] != "fail" {
		t.Fatalf("checks.database = %v, want a fail entry", checks)
	}
	if path := fmt.Sprint(checks); strings.Contains(path, "game-narrator.sqlite") {
		t.Fatalf("readiness payload leaked the database path: %s", path)
	}
}

// TestStoreReadinessRepairsOnlyADamagedFTSIndex is the readiness-level proof
// of the guard behind the field failure: a database that arrives with
// messages_fts structurally damaged (the content intact) must come up ready
// after the readiness probe rebuilds the derived index — and a database whose
// damage reaches the content must stay down with a fail entry, not get
// rebuilt over.
func TestStoreReadinessRepairsOnlyADamagedFTSIndex(t *testing.T) {
	lc := newLogCapture(t)
	ctx := context.Background()

	seedFTSStore := func(dir string) string {
		t.Helper()
		st, err := store.Open(ctx, store.DBPath(dir))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := st.Migrate(ctx); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		game := games.Game{ID: games.NewID(), Slug: "fts-guard", Title: "FTS Guard",
			Platform: "gamecube", UserTags: []byte(`["synthetic"]`), CreatedAt: nowStampUTC()}
		if err := st.CreateGame(ctx, game); err != nil {
			t.Fatalf("CreateGame: %v", err)
		}
		edition := games.Edition{ID: games.NewID(), GameID: game.ID, Region: "US",
			Language: "en", Revision: "rev0", Locale: "en_US"}
		if err := st.CreateEdition(ctx, edition); err != nil {
			t.Fatalf("CreateEdition: %v", err)
		}
		prof, err := st.CreateDraftProfile(ctx, edition.ID)
		if err != nil {
			t.Fatalf("CreateDraftProfile: %v", err)
		}
		msgs := make([]games.Message, 0, 120)
		for i := 0; i < 120; i++ {
			text := fmt.Sprintf("line %02d of the harbour light keeper log", i%17)
			msgs = append(msgs, games.Message{
				ID: games.NewID(), ProfileID: prof.ID,
				OriginalText: text, NormalizedText: strings.ToLower(text),
				SpeechText: text + "!", Language: "en", TextKind: games.TextDialogue,
				AttributionStatus: games.AttributionUnknown, Revision: 1,
				CreatedAt: nowStampUTC(), UpdatedAt: nowStampUTC(),
			})
		}
		if err := st.PutMessages(ctx, prof.ID, msgs, nil); err != nil {
			t.Fatalf("PutMessages: %v", err)
		}
		// Settle the index into data pages (the shape that gets checkpointed
		// and damaged), then close so the file is self-contained.
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		raw, err := sql.Open("sqlite", store.DBPath(dir)+"?_foreign_keys=1&_journal_mode=WAL&_busy_timeout=5000")
		if err != nil {
			t.Fatalf("raw open: %v", err)
		}
		if _, err := raw.Exec(`INSERT INTO messages_fts(messages_fts) VALUES('merge')`); err != nil {
			t.Fatalf("merge: %v", err)
		}
		if err := raw.Close(); err != nil {
			t.Fatalf("raw close: %v", err)
		}
		return store.DBPath(dir)
	}

	// Case 1: index-only damage — the readiness probe rebuilds and reports ok.
	dbPath := seedFTSStore(t.TempDir())
	if !corruptOneFTSIndexPage(t, dbPath) {
		t.Fatalf("the FTS index corruption was not visible to PRAGMA integrity_check")
	}
	if !messagesTableIntact(t, dbPath) {
		t.Fatalf("the corruption helper damaged the content table: the test is invalid")
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ready := storeReadiness(st, lc.log)
	checks, ok := ready(ctx)
	if !ok {
		t.Fatalf("readiness = not ready after a damaged FTS index: the guard should have rebuilt it: %v", checks)
	}
	if database, _ := checks["database"].(map[string]any); database == nil || database["status"] != "ok" {
		t.Fatalf("checks.database = %v, want status ok", checks)
	}
	if !lc.contains("repaired", "messages_fts") {
		t.Fatalf("the recovery did not reach the operator log: %s", lc.buf.String())
	}

	// Case 2: content damage — the same probe must refuse and stay down.
	dbPath2 := seedFTSStore(t.TempDir())
	if !corruptMessagesRootPage(t, dbPath2) {
		t.Fatalf("the content-page corruption was not visible to PRAGMA integrity_check")
	}
	st2, err := store.Open(ctx, dbPath2)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	ready2 := storeReadiness(st2, lc.log)
	checks, ok = ready2(ctx)
	if ok {
		t.Fatalf("readiness = ready over damaged content: the guard must refuse to repair it: %v", checks)
	}
	if database, _ := checks["database"].(map[string]any); database == nil || database["status"] != "fail" {
		t.Fatalf("checks.database = %v, want a fail entry", checks)
	}
	if !lc.contains("no self-repair applied", "backup") {
		t.Fatalf("the refusal did not reach the operator log with the backup/restore answer: %s", lc.buf.String())
	}
}

// nowStampUTC is the RFC 3339 stamp the schema's strftime CHECK requires.
func nowStampUTC() string { return time.Now().UTC().Format(time.RFC3339) }

// logCapture is a logger the test can assert on: the guard's promises — what
// it repaired, why it refused — are log lines, and a guard that cannot be
// observed cannot be trusted.
type logCapture struct {
	log *slog.Logger
	buf *bytes.Buffer
}

func newLogCapture(t *testing.T) *logCapture {
	t.Helper()
	buf := &bytes.Buffer{}
	return &logCapture{log: slog.New(slog.NewJSONHandler(buf, nil)), buf: buf}
}

// contains reports whether one captured line carries every substring.
func (c *logCapture) contains(subs ...string) bool {
	for _, line := range strings.Split(c.buf.String(), "\n") {
		all := true
		for _, sub := range subs {
			if !strings.Contains(line, sub) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// integrityHasProblems reports whether PRAGMA integrity_check finds anything
// wrong — either a problem line or a check that cannot even complete.
func integrityHasProblems(db *sql.DB) bool {
	rows, err := db.Query("PRAGMA integrity_check")
	if err != nil {
		return true
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return true
		}
		if !strings.EqualFold(strings.TrimSpace(line), "ok") {
			return true
		}
	}
	return false
}

// corruptOneFTSIndexPage damages one FTS index page of a closed database and
// reports whether the damage is visible to PRAGMA integrity_check. Which page
// a zeroed byte lands in is a matter of FTS5 page geometry (segment ids are
// folded into the page numbers, and full pages carry slack a zero byte can
// sit in without breaking a parse), so the helper walks the pages until the
// check fires — the same walk the repair is meant to clear.
func corruptOneFTSIndexPage(t *testing.T, path string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_foreign_keys=1&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	type pageRef struct {
		id    int
		block []byte
	}
	var pages []pageRef
	{
		rows, err := db.Query(`SELECT id, block FROM messages_fts_data ORDER BY id`)
		if err != nil {
			t.Fatalf("read index pages: %v", err)
		}
		for rows.Next() {
			var p pageRef
			if err := rows.Scan(&p.id, &p.block); err != nil {
				t.Fatalf("scan index page: %v", err)
			}
			pages = append(pages, p)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read index pages: %v", err)
		}
		rows.Close()
	}
	if len(pages) < 3 {
		t.Fatalf("the index has %d data pages: the test needs a settled, merged index", len(pages))
	}
	defer db.Close()
	for _, p := range pages {
		if len(p.block) >= 16 {
			b := make([]byte, len(p.block))
			copy(b, p.block)
			b[len(b)/2] = 0
			if _, err := db.Exec(`UPDATE messages_fts_data SET block = ? WHERE id = ?`, b, p.id); err != nil {
				t.Fatalf("corrupt page %d: %v", p.id, err)
			}
			if integrityHasProblems(db) {
				return true
			}
			if _, err := db.Exec(`UPDATE messages_fts_data SET block = ? WHERE id = ?`, p.block, p.id); err != nil {
				t.Fatalf("restore page %d: %v", p.id, err)
			}
		}
	}
	return false
}

// corruptMessagesRootPage rewrites one cell pointer of the messages table's
// root page to point outside the page. The database must be closed: after a
// clean close the WAL is checkpointed away and the file is self-contained.
func corruptMessagesRootPage(t *testing.T, path string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_query_only=1&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open for page layout: %v", err)
	}
	var pageSize, rootPage int
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatalf("page_size: %v", err)
	}
	if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE type='table' AND name='messages'`).Scan(&rootPage); err != nil {
		t.Fatalf("messages rootpage: %v", err)
	}
	db.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// Page N starts at (N-1)*pageSize: the 100-byte database header is the
	// first 100 bytes of page 1, not a preamble before it.
	off := (rootPage - 1) * pageSize
	if off+8 >= len(data) {
		t.Fatalf("page %d at offset %d is outside the %d-byte file", rootPage, off, len(data))
	}
	if pageType := data[off]; pageType != 0x05 && pageType != 0x0D {
		t.Fatalf("messages root page %d has type 0x%02x, want a table btree page", rootPage, pageType)
	}
	cellCount := binary.BigEndian.Uint16(data[off+3 : off+5])
	if cellCount == 0 {
		t.Fatalf("messages root page %d has no cells", rootPage)
	}
	binary.BigEndian.PutUint16(data[off+8:off+10], 0xFFFF)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write corrupt page: %v", err)
	}

	check, err := sql.Open("sqlite", path+"?_foreign_keys=1&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("reopen for verification: %v", err)
	}
	damaged := integrityHasProblems(check)
	check.Close()
	return damaged
}

// messagesTableIntact is the sanity check that the index corruption helper
// really left the content alone: the right number of rows and a known line.
func messagesTableIntact(t *testing.T, path string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_query_only=1&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open for content check: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM messages`).Scan(&n); err != nil || n != 120 {
		t.Fatalf("content check: n=%d err=%v, want 120", n, err)
	}
	var norm string
	if err := db.QueryRow(`SELECT normalized_text FROM messages ORDER BY rowid LIMIT 1`).Scan(&norm); err != nil {
		t.Fatalf("content check: %v", err)
	}
	return strings.Contains(norm, "harbour light keeper")
}

func TestServeMigratesBeforeListening(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "serve-data")
	dbPath := store.DBPath(dataDir)

	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the database should not exist yet: %v", err)
	}

	addr := freeListenAddress(t)
	serveDone := make(chan int, 1)
	go func() {
		code, _ := run([]string{"serve", "--listen", addr, "--data-dir", dataDir}, log)
		serveDone <- code
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/api/v1/health/ready")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				last = string(body)
				break
			}
			last = fmt.Sprintf("HTTP %d %s", resp.StatusCode, body)
		} else {
			last = err.Error()
		}
		time.Sleep(25 * time.Millisecond)
	}
	if want := fmt.Sprintf(`"schema_version":%d`, store.LatestSchemaVersion()); !strings.Contains(last, want) {
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		t.Fatalf("readiness never reported the migrated database: %s", last)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("serve did not create the database before listening: %v", err)
	}

	// Drain the way systemd would. The signal is delivered to this process and
	// taken by serve's signal.NotifyContext, so the handler under test is the
	// real startup and shutdown path, not a reimplementation of it.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signalling serve: %v", err)
	}
	select {
	case code := <-serveDone:
		if code != 0 {
			t.Fatalf("serve exit code = %d, want 0 after a clean drain", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve never shut down after SIGTERM — the store would not be closed either")
	}

	// The store was closed on shutdown, so a reopening read still works but the
	// process that held it is gone: proves defer ordering in runServe.
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopening after shutdown: %v", err)
	}
	defer st.Close()
	if v, err := st.SchemaVersion(context.Background()); err != nil || v != store.LatestSchemaVersion() {
		t.Fatalf("schema version after restart = %d (%v), want %d", v, err, store.LatestSchemaVersion())
	}
}

// freeListenAddress reserves a port, releases it and returns it. The tiny race
// that leaves is preferable to a fixed port that collides with a developer's
// running service.
func freeListenAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	return addr
}

// --- serve: durable jobs and the event stream ------------------------------

// TestServeRunsEnqueuedJobsAndStreamsThem is the M1 acceptance path through
// the real CLI wiring (not a reassembled copy of it): `serve` starts the job
// runner over the SQLite queue; a system.noop job enqueued on the same
// database is claimed, executed and recorded; and a browser-grade SSE client
// reconnecting with Last-Event-ID: 0 replays that job's lifecycle from the
// durable event log (docs/spec/08 "Event stream reliability", docs/spec/12
// "SSE reconnect and Last-Event-ID"). The binary's own shutdown then exits 0
// with the store still readable.
func TestServeRunsEnqueuedJobsAndStreamsThem(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "jobs-serve")
	dbPath := store.DBPath(dataDir)
	addr := freeListenAddress(t)

	// The database is prepared with the *same commands an operator runs* —
	// migrate, admin create, admin token — so this asserts the CLI and the server
	// agree on the credential tables, not just that each works alone.
	if code, err := run([]string{"migrate", "--data-dir", dataDir}, log); err != nil || code != 0 {
		t.Fatalf("migrate: code=%d err=%v", code, err)
	}
	token := adminTokenForTest(t, dataDir, log, "admin", "admin", "ci-probe")

	serveDone := make(chan int, 1)
	go func() {
		code, err := run([]string{"serve", "--listen", addr, "--data-dir", dataDir}, log)
		if err != nil {
			t.Errorf("serve: %v", err)
		}
		serveDone <- code
	}()
	defer func() {
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case code := <-serveDone:
			if code != 0 {
				t.Errorf("serve exit code = %d, want 0 after a clean drain", code)
			}
		case <-time.After(20 * time.Second):
			t.Error("serve did not exit after SIGTERM")
		}
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	waitReady(t, client, addr)

	// Before the token is presented anywhere: the queue is not readable by
	// whoever finds the port (docs/spec/14 "no unauthenticated … audio/screenshot"
	// generalises to every route that is not health or system info).
	if resp, err := client.Get("http://" + addr + "/api/v1/jobs"); err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous GET /api/v1/jobs = %d, want 401 (%s)", resp.StatusCode, trimForLog(string(body)))
		}
	}

	// Enqueue through a second connection to the same database — exactly
	// what a later import subsystem does in-process, and honest about the
	// queue being the table, not a goroutine channel.
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("opening the live database: %v", err)
	}
	defer st.Close()
	js := store.NewJobsStore(st)
	job, created, err := js.Enqueue(ctx, store.EnqueueParams{
		Kind:        "system.noop",
		Payload:     json.RawMessage(`{"probe":"m1"}`),
		MaxAttempts: 3,
	})
	if err != nil || !created {
		t.Fatalf("enqueue: created=%v err=%v", created, err)
	}

	// The runner must pick it up and land it as `succeeded`, visible through
	// the API — the same view a UI gets.
	deadline := time.Now().Add(15 * time.Second)
	var state string
	for time.Now().Before(deadline) && state != "succeeded" {
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/v1/jobs/"+job.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err == nil {
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				resp.Body.Close()
				t.Fatalf("job detail is not JSON: %v", err)
			}
			resp.Body.Close()
			state, _ = body["state"].(string)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if state != "succeeded" {
		t.Fatalf("job %s never reached succeeded through the API, last state %q", job.ID, state)
	}

	// The SSE stream, replayed from the beginning with a Last-Event-ID an
	// ordinary client would have carried across a reconnect: the job's own
	// events must be on it, correlated to the job id.
	streamCtx, cancelStream := context.WithTimeout(ctx, 5*time.Second)
	defer cancelStream()
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet,
		"http://"+addr+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "0")
	// The event stream is no longer an anonymous channel: it carries game text
	// (docs/spec/08 "Partial data redacted when not in developer mode").
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("opening the event stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event stream status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}

	var collected strings.Builder
	buf := make([]byte, 4096)
	needleStart, needleDone := "event: job.started", "event: job.succeeded"
	for {
		n, err := resp.Body.Read(buf)
		collected.Write(buf[:n])
		text := collected.String()
		if strings.Contains(text, needleStart) && strings.Contains(text, needleDone) &&
			strings.Contains(text, `"job_id":"`+job.ID+`"`) {
			break
		}
		if err != nil { // the 5s stream deadline ends the read; decide below
			break
		}
	}
	text := collected.String()
	for _, want := range []string{needleStart, needleDone, `"job_id":"` + job.ID + `"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("event stream missing %q; got %d bytes:\n%s", want, len(text), trimForLog(text))
		}
	}
	// Every frame carries a numeric id — the cursor a reconnect would send.
	if !strings.Contains(text, "id: ") {
		t.Fatalf("SSE frames missing id: cursor field:\n%s", trimForLog(text))
	}
}

// waitReady polls /api/v1/health/ready until it answers 200.
func waitReady(t *testing.T, client *http.Client, addr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/api/v1/health/ready")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			_ = body
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("serve never became ready")
}

func trimForLog(s string) string {
	if len(s) > 1500 {
		return s[:1500] + "…"
	}
	return s
}

// --- admin: accounts and API tokens (docs/spec/08 "API version and user
// authentication", docs/spec/14 "local-first, set admin on first run") -------

// TestAdminCreateReadsThePasswordFromStdin is the credential-hygiene contract:
// the password is piped, never a flag (argv is world-readable in `ps`), never
// echoed, and never written to the log.
func TestAdminCreateReadsThePasswordFromStdin(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "admin-create")

	out := captureStdout(t, func() int {
		withStdin(t, "correct horse battery staple\n")
		code, err := run([]string{"admin", "create", "--username", "admin", "--role", "admin", "--data-dir", dataDir}, log)
		if err != nil {
			t.Fatalf("admin create: %v", err)
		}
		return code
	})
	if out.code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", out.code, out.output)
	}
	if !strings.HasPrefix(out.output, "created admin with role admin (01") {
		t.Fatalf("unexpected output:\n%s", out.output)
	}
	if strings.Contains(out.output, "correct horse battery staple") {
		t.Fatalf("the CLI echoed the password back: %s", out.output)
	}

	// The row is real, and it holds a PBKDF2 envelope rather than a password.
	st, err := store.Open(context.Background(), store.DBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	repo := store.NewAuthStore(st)
	u, err := repo.UserByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatalf("the created account is not readable: %v", err)
	}
	if u.Role != store.RoleAdmin || u.Disabled {
		t.Fatalf("account = %+v", u)
	}
	if !strings.HasPrefix(u.PasswordHash, "pbkdf2-sha256$600000$") {
		t.Fatalf("stored hash = %.24s…, want the default-cost pbkdf2-sha256 envelope", u.PasswordHash)
	}
	if strings.Contains(u.PasswordHash, "correct horse battery staple") {
		t.Fatal("the password was stored")
	}

	// A duplicate name is refused (case-insensitively), and a weak password is
	// refused with the rule quoted.
	dup := captureStdout(t, func() int {
		withStdin(t, "another fine passphrase\n")
		code, _ := run([]string{"admin", "create", "--username", "ADMIN", "--role", "viewer", "--data-dir", dataDir}, log)
		return code
	})
	if dup.code != 1 {
		t.Fatalf("duplicate username exit = %d, want 1:\n%s", dup.code, dup.output)
	}
	weak := captureStdout(t, func() int {
		withStdin(t, "short\n")
		code, err := run([]string{"admin", "create", "--username", "weakling", "--data-dir", dataDir}, log)
		if !errors.Is(err, errUsage) && err == nil {
			// A policy failure is a runtime failure (1), not a usage error (2).
			t.Errorf("weak password returned err=%v, want a non-usage error", err)
		}
		return code
	})
	if weak.code != 1 {
		t.Fatalf("weak password exit = %d, want 1 (a rejected account must not look like a success)", weak.code)
	}
	// No stdin at all is a usage error with an actionable message, not a hang.
	nostdin := captureStdout(t, func() int {
		withStdin(t, "")
		code, _ := run([]string{"admin", "create", "--username", "third", "--data-dir", dataDir}, log)
		return code
	})
	if nostdin.code != 2 {
		t.Fatalf("empty stdin exit = %d, want 2", nostdin.code)
	}
	// An unknown role is usage (2), because the operator can fix the command line.
	badrole := captureStdout(t, func() int {
		withStdin(t, "a perfectly fine passphrase\n")
		code, _ := run([]string{"admin", "create", "--username", "boss", "--role", "supreme", "--data-dir", dataDir}, log)
		return code
	})
	if badrole.code != 2 {
		t.Fatalf("unknown role exit = %d, want 2", badrole.code)
	}
	// And only the good account exists: a rejected create wrote nothing.
	users, err := repo.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Username != "admin" {
		t.Fatalf("accounts after the rejected creates = %+v", users)
	}
}

// createAccount runs `admin create` with the password piped on stdin and fails
// the test if the CLI did not take it.
func createAccount(t *testing.T, dataDir string, log *slog.Logger, username, role, password string) {
	t.Helper()
	withStdin(t, password+"\n")
	out := captureStdout(t, func() int {
		code, err := run([]string{"admin", "create", "--username", username, "--role", role, "--data-dir", dataDir}, log)
		if err != nil {
			t.Fatalf("admin create %s: %v", username, err)
		}
		return code
	})
	if out.code != 0 {
		t.Fatalf("admin create %s exit = %d:\n%s", username, out.code, out.output)
	}
}

// TestAdminTokenPrintsTheSecretOnce covers the integration-credential path of
// docs/spec/08, including the ceiling rule that keeps a token from outgrowing
// its owner.
func TestAdminTokenPrintsTheSecretOnce(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "admin-token")
	if code, err := run([]string{"migrate", "--data-dir", dataDir}, log); err != nil || code != 0 {
		t.Fatalf("migrate: code=%d err=%v", code, err)
	}
	// Each create gets its own stdin: the password is read once per invocation,
	// and a second invocation must not inherit an exhausted pipe.
	createAccount(t, dataDir, log, "admin", "admin", "correct horse battery staple")
	createAccount(t, dataDir, log, "reader", "viewer", "a second fine passphrase")

	out := captureStdout(t, func() int {
		code, err := run([]string{"admin", "token", "--username", "admin", "--name", "ci-harness", "--role", "operator", "--data-dir", dataDir}, log)
		if err != nil {
			t.Fatalf("admin token: %v", err)
		}
		return code
	})
	if out.code != 0 {
		t.Fatalf("exit = %d:\n%s", out.code, out.output)
	}
	raw := strings.TrimSpace(out.output)
	if strings.Count(out.output, "\n") != 1 {
		t.Fatalf("stdout must carry the secret alone, got:\n%s", out.output)
	}
	if !strings.HasPrefix(raw, "gn_") {
		t.Fatalf("token = %q, want the documented gn_ prefix", raw)
	}

	// It authenticates against the live server, which is the only proof that the
	// digest the CLI wrote is the digest the API reads.
	st, err := store.Open(context.Background(), store.DBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.NewService(store.NewAuthStore(st))
	id, err := svc.VerifyToken(context.Background(), raw)
	if err != nil {
		t.Fatalf("the printed token does not authenticate: %v", err)
	}
	if id.Role != auth.RoleOperator {
		t.Fatalf("verified role = %s, want operator", id.Role)
	}
	// And the database cannot give the secret back.
	toks, err := store.NewAuthStore(st).ListAPITokens(context.Background(), id.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 1 {
		t.Fatalf("tokens = %+v", toks)
	}
	if toks[0].TokenHash != "" && toks[0].TokenHash != auth.DigestSecret(raw) {
		t.Errorf("the list projection carried a digest that is not ours: %q", toks[0].TokenHash)
	}
	st.Close()

	// Ceiling: a viewer may not mint an operator token.
	tooHigh := captureStdout(t, func() int {
		code, _ := run([]string{"admin", "token", "--username", "reader", "--name", "escalation", "--role", "admin", "--data-dir", dataDir}, log)
		return code
	})
	if tooHigh.code != 1 {
		t.Fatalf("over-ceiling token exit = %d, want 1:\n%s", tooHigh.code, tooHigh.output)
	}
	if strings.Contains(tooHigh.output, "gn_") {
		t.Fatal("a refused token mint still printed a secret")
	}
	// Omitting --role inherits the owner's role (admin here), which is what an
	// operator typing `admin token --username admin` expects.
	inherited := captureStdout(t, func() int {
		code, _ := run([]string{"admin", "token", "--username", "admin", "--name", "defaulted", "--data-dir", dataDir}, log)
		return code
	})
	if inherited.code != 0 {
		t.Fatalf("defaulted token exit = %d:\n%s", inherited.code, inherited.output)
	}
	st2, err := store.Open(context.Background(), store.DBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	// An unknown account is actionable, not a stack trace.
	unknown := captureStdout(t, func() int {
		code, _ := run([]string{"admin", "token", "--username", "ghost", "--name", "x", "--data-dir", dataDir}, log)
		return code
	})
	if unknown.code != 1 {
		t.Fatalf("unknown account exit = %d, want 1:\n%s", unknown.code, unknown.output)
	}
	// A negative lifetime is usage.
	neg := captureStdout(t, func() int {
		code, _ := run([]string{"admin", "token", "--username", "admin", "--name", "x", "--expires-days", "-3", "--data-dir", dataDir}, log)
		return code
	})
	if neg.code != 2 {
		t.Fatalf("--expires-days -3 exit = %d, want 2", neg.code)
	}
}

func TestAdminListNeverCarriesASecret(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "admin-list")
	if code, _ := run([]string{"migrate", "--data-dir", dataDir}, log); code != 0 {
		t.Fatal("migrate failed")
	}
	before := captureStdout(t, func() int {
		code, _ := run([]string{"admin", "list", "--data-dir", dataDir}, log)
		return code
	})
	if before.code != 0 || !strings.Contains(before.output, "no accounts") {
		t.Fatalf("an empty appliance should say so plainly, got %d:\n%s", before.code, before.output)
	}
	createAccount(t, dataDir, log, "operator1", "operator", "correct horse battery staple")
	after := captureStdout(t, func() int {
		code, _ := run([]string{"admin", "list", "--data-dir", dataDir}, log)
		return code
	})
	if after.code != 0 || !strings.Contains(after.output, "operator1\toperator") {
		t.Fatalf("list output:\n%s", after.output)
	}
	for _, secret := range []string{"pbkdf2", "correct horse battery staple", "gn_"} {
		if strings.Contains(after.output, secret) {
			t.Errorf("admin list leaked %q", secret)
		}
	}
}

func TestAdminUsageErrors(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	for name, args := range map[string][]string{
		"no subcommand":  {"admin"},
		"unknown":        {"admin", "delete-everything"},
		"unknown flag":   {"admin", "create", "--nope"},
		"extra argument": {"admin", "create", "stray"},
		"missing data":   {"admin", "create", "--username", "x", "--data-dir", "relative/dir"},
	} {
		if code, _ := run(args, log); code != 2 {
			t.Errorf("%s: exit = %d, want 2", name, code)
		}
	}
}

// TestServeRequiresAuthenticationAndCompletesTheFirstRunCycle is docs/spec/14's
// security acceptance sequence against the real binary: an anonymous caller gets
// nothing, setup claims the appliance from loopback, login yields a session, the
// CSRF header is required for a state-changing call, and logout invalidates the
// cookie that was just in use.
func TestServeRequiresAuthenticationAndCompletesTheFirstRunCycle(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "auth-serve")
	addr := freeListenAddress(t)

	serveDone := make(chan int, 1)
	go func() {
		code, err := run([]string{"serve", "--listen", addr, "--data-dir", dataDir}, log)
		if err != nil {
			t.Errorf("serve: %v", err)
		}
		serveDone <- code
	}()
	defer func() {
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-serveDone:
		case <-time.After(20 * time.Second):
			t.Error("serve did not exit after SIGTERM")
		}
	}()
	client := &http.Client{Timeout: 5 * time.Second, Jar: newJar()}
	base := "http://" + addr + "/api/v1"
	waitReady(t, client, addr)

	type step struct {
		name   string
		method string
		path   string
		body   string
		csrf   bool
		want   int
	}
	var csrfToken string
	var cookie string
	pas := []step{
		{"status before setup", http.MethodGet, "/auth/status", "", false, http.StatusOK},
		{"setup from loopback", http.MethodPost, "/auth/setup", `{"username":"admin","password":"correct horse battery staple"}`, false, http.StatusCreated},
		{"setup again", http.MethodPost, "/auth/setup", `{"username":"two","password":"correct horse battery staple"}`, false, http.StatusConflict},
		{"me before login", http.MethodGet, "/auth/me", "", false, http.StatusUnauthorized},
		{"jobs before login", http.MethodGet, "/jobs", "", false, http.StatusUnauthorized},
		{"login", http.MethodPost, "/auth/login", `{"username":"admin","password":"correct horse battery staple"}`, false, http.StatusOK},
		{"me", http.MethodGet, "/auth/me", "", false, http.StatusOK},
		{"cancel without csrf", http.MethodPost, "/jobs/does-not-exist/cancel", "", false, http.StatusForbidden},
		{"cancel with csrf", http.MethodPost, "/jobs/does-not-exist/cancel", "", true, http.StatusNotFound},
		{"logout with csrf", http.MethodPost, "/auth/logout", "", true, http.StatusNoContent},
		{"me after logout", http.MethodGet, "/auth/me", "", false, http.StatusUnauthorized},
	}
	for _, st := range pas {
		req, err := http.NewRequest(st.method, base+st.path, strings.NewReader(st.body))
		if err != nil {
			t.Fatal(err)
		}
		if st.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		if st.csrf && csrfToken != "" {
			req.Header.Set("X-CSRF-Token", csrfToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if cs := resp.Cookies(); len(cs) > 0 {
			parts := make([]string, 0, len(cs))
			for _, c := range cs {
				if c.Value != "" {
					parts = append(parts, c.Name+"="+c.Value)
				}
			}
			if len(parts) > 0 {
				cookie = strings.Join(parts, "; ")
			}
		}
		if st.name == "login" {
			var parsed struct {
				CSRFToken string `json:"csrf_token"`
			}
			if err := json.Unmarshal(body, &parsed); err != nil {
				t.Fatalf("login body: %v (%s)", err, body)
			}
			csrfToken = parsed.CSRFToken
			if csrfToken == "" {
				t.Fatal("login returned no csrf_token")
			}
		}
		if resp.StatusCode != st.want {
			t.Fatalf("%s: %d, want %d (%s)", st.name, resp.StatusCode, st.want, trimForLog(string(body)))
		}
	}
}

// TestServeCatalogAndImportOverTheWire is the binary-level proof of Milestone 1's
// acceptance criterion: a real process, a real data directory, the CLI's own
// migrate/admin commands, and then the catalog API driven end to end — create a
// title idempotently, add an edition, open a draft, upload the synthetic script as
// a browser would, watch the durable job run to succeeded, edit a line under its
// ETag and download the result. Nothing here is mocked: the import runs in the
// serve command's own job runner against the asset store it opened, which is what
// makes "the wiring is present" (rather than "the handler works") a tested claim
// (AGENTS.md "no simulation theatre").
func TestServeCatalogAndImportOverTheWire(t *testing.T) {
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "catalog-serve")
	addr := freeListenAddress(t)
	if code, err := run([]string{"migrate", "--data-dir", dataDir}, log); err != nil || code != 0 {
		t.Fatalf("migrate: code=%d err=%v", code, err)
	}
	token := adminTokenForTest(t, dataDir, log, "admin", "admin", "catalog-probe")

	serveDone := make(chan int, 1)
	go func() {
		code, err := run([]string{"serve", "--listen", addr, "--data-dir", dataDir}, log)
		if err != nil {
			t.Errorf("serve: %v", err)
		}
		serveDone <- code
	}()
	defer func() {
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case code := <-serveDone:
			if code != 0 {
				t.Errorf("serve exit code = %d, want 0 after a clean drain", code)
			}
		case <-time.After(20 * time.Second):
			t.Error("serve did not exit after SIGTERM")
		}
	}()

	client := &http.Client{Timeout: 10 * time.Second}
	base := "http://" + addr + "/api/v1"
	waitReady(t, client, addr)

	do := func(method, path string, body []byte, hdr map[string]string) (int, map[string]any, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var parsed map[string]any
		if len(raw) > 0 && json.Valid(raw) {
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("%s %s: body is not JSON: %v (%s)", method, path, err, trimForLog(string(raw)))
			}
		}
		return resp.StatusCode, parsed, resp.Header, raw
	}
	must := func(want int, what string, code int, _ map[string]any, _ http.Header, raw []byte) {
		t.Helper()
		if code != want {
			t.Fatalf("%s = %d, want %d (%s)", what, code, want, trimForLog(string(raw)))
		}
	}

	// The contract is public and is the build's own.
	code, doc, _, _ := do(http.MethodGet, "/openapi.json", nil, nil)
	must(http.StatusOK, "GET /openapi.json", code, doc, nil, nil)
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("the served contract declares %v", doc["openapi"])
	}
	// An anonymous caller gets nothing else. `do` always presents the token, so
	// this one request is made without it on purpose.
	anon, err := http.Get(base + "/games")
	if err != nil {
		t.Fatalf("anonymous GET /games: %v", err)
	}
	anonRaw, _ := io.ReadAll(io.LimitReader(anon.Body, 4096))
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /games without a credential = %d, want 401 (%s)", anon.StatusCode, trimForLog(string(anonRaw)))
	}

	jsonType := map[string]string{"Content-Type": "application/json"}
	code, game, _, raw := do(http.MethodPost, "/games",
		[]byte(`{"slug":"synthetic-adventure","title":"Synthetic Adventure","platform":"gamecube-like"}`),
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": "serve-1"})
	must(http.StatusCreated, "POST /games", code, game, nil, raw)
	gameID, _ := game["id"].(string)
	if gameID == "" {
		t.Fatalf("no game id in %s", trimForLog(string(raw)))
	}
	code, replay, replayHdr, _ := do(http.MethodPost, "/games",
		[]byte(`{"slug":"synthetic-adventure","title":"Synthetic Adventure","platform":"gamecube-like"}`),
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": "serve-1"})
	must(http.StatusCreated, "the replayed POST /games", code, replay, nil, nil)
	if replay["id"] != gameID {
		t.Fatalf("the replay created a second game: %v vs %v", replay["id"], gameID)
	}
	if replayHdr.Get("Idempotency-Replayed") != "true" {
		t.Errorf("the replay did not say so: %v", replayHdr)
	}

	code, edition, _, raw := do(http.MethodPost, "/games/"+gameID+"/editions",
		[]byte(`{"region":"us","language":"en","revision":"rev1"}`),
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": "serve-2"})
	must(http.StatusCreated, "POST /games/{id}/editions", code, edition, nil, raw)
	editionID, _ := edition["id"].(string)

	code, draft, draftHdr, raw := do(http.MethodPost, "/editions/"+editionID+"/profiles", nil, jsonType)
	must(http.StatusCreated, "POST /editions/{id}/profiles", code, draft, nil, raw)
	profileID, _ := draft["id"].(string)
	etag := draftHdr.Get("ETag")
	if etag != `"1"` {
		t.Errorf("a fresh draft carries ETag %s, want \"1\"", etag)
	}

	// The upload, as a browser sends it.
	csvBytes, err := os.ReadFile(filepath.Join("..", "..", "internal", "games", "adapter", "generic",
		"testdata", "synthetic-adventure.csv"))
	if err != nil {
		t.Fatalf("read the synthetic script: %v", err)
	}
	var multipartBody bytes.Buffer
	mw := multipart.NewWriter(&multipartBody)
	part, err := mw.CreateFormFile("file", "synthetic-adventure.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(csvBytes); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("rights", "user_supplied"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	code, accepted, _, raw := do(http.MethodPost, "/profiles/"+profileID+"/import", multipartBody.Bytes(),
		map[string]string{"Content-Type": mw.FormDataContentType()})
	must(http.StatusAccepted, "POST /profiles/{id}/import", code, accepted, nil, raw)
	jobID, _ := accepted["job_id"].(string)
	if jobID == "" {
		t.Fatalf("the 202 named no job: %s", trimForLog(string(raw)))
	}

	deadline := time.Now().Add(60 * time.Second)
	var state string
	for time.Now().Before(deadline) && state != "succeeded" {
		code, job, _, _ := do(http.MethodGet, "/jobs/"+jobID, nil, nil)
		must(http.StatusOK, "GET /jobs/{id}", code, job, nil, nil)
		state, _ = job["state"].(string)
		if state == "failed" || state == "cancelled" {
			t.Fatalf("the import job ended in %q: %s", state, trimForLog(fmt.Sprint(job)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if state != "succeeded" {
		t.Fatalf("the import job never succeeded, last state %q", state)
	}

	// The content is now readable, searchable and editable through the same API.
	code, page, _, raw := do(http.MethodGet, "/profiles/"+profileID+"/messages?limit=10", nil, nil)
	must(http.StatusOK, "GET /profiles/{id}/messages", code, page, nil, raw)
	msgs, _ := page["messages"].([]any)
	if len(msgs) != 10 {
		t.Fatalf("limit=10 returned %d messages", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	msgID, _ := first["id"].(string)
	if moved, _ := page["profile_etag"].(string); moved == "" {
		t.Error("the message page carries no revision token for the editor to write with")
	}

	code, search, _, raw := do(http.MethodGet, "/profiles/"+profileID+"/messages/search?q=harbourmaster", nil, nil)
	must(http.StatusOK, "GET …/messages/search", code, search, nil, raw)
	if hits, _ := search["hits"].([]any); len(hits) == 0 {
		t.Fatalf("the full-text index found nothing for a word in the script: %s", trimForLog(string(raw)))
	}

	// The write. The import moved the row version, so the token the draft was
	// handed at creation is now correctly stale: a binary that still accepted it
	// would be accepting a write from an editor who had lost the race.
	staleCode, _, staleHdr, staleRaw := do(http.MethodPatch, "/profiles/"+profileID+"/messages/"+msgID,
		[]byte(`{"speech_text":"lost the race"}`),
		map[string]string{"Content-Type": "application/json", "If-Match": etag})
	if staleCode != http.StatusPreconditionFailed {
		t.Fatalf("a write under a superseded token = %d, want 412 (%s)", staleCode, trimForLog(string(staleRaw)))
	}
	if !strings.Contains(string(staleRaw), "PROFILE_REVISION_CONFLICT") {
		t.Errorf("the stale-token refusal is not the documented conflict: %s", trimForLog(string(staleRaw)))
	}
	current := staleHdr.Get("ETag")
	if current == "" || current == etag {
		t.Fatalf("the 412 named the current token as %q, and the draft's was %s", current, etag)
	}
	code, edited, editedHdr, raw := do(http.MethodPatch, "/profiles/"+profileID+"/messages/"+msgID,
		[]byte(`{"speech_text":"The harbourmaster is counting the crates again."}`),
		map[string]string{"Content-Type": "application/json", "If-Match": current})
	must(http.StatusOK, "PATCH with the current token", code, edited, nil, raw)
	msg, _ := edited["message"].(map[string]any)
	if msg["speech_text"] != "The harbourmaster is counting the crates again." {
		t.Errorf("speech_text did not take: %v", msg["speech_text"])
	}
	if msg["original_text"] != first["original_text"] {
		t.Errorf("the edit changed original_text: %v → %v", first["original_text"], msg["original_text"])
	}
	if editedHdr.Get("ETag") == current {
		t.Error("the write did not move the revision token")
	}

	// And the revision can leave the appliance as the manifest that documents it.
	code, _, exportHdr, exportRaw := do(http.MethodGet, "/profiles/"+profileID+"/export", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET …/export = %d (%s)", code, trimForLog(string(exportRaw)))
	}
	if cd := exportHdr.Get("Content-Disposition"); !strings.Contains(cd, ".gnprofile.json") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	var manifest map[string]any
	if err := json.Unmarshal(exportRaw, &manifest); err != nil {
		t.Fatalf("the export is not a manifest: %v", err)
	}
	if _, ok := manifest["messages"].([]any); !ok {
		t.Fatalf("the manifest carries no messages: %v", sortedKeysOf(manifest))
	}

	// The durable state the run produced is on disk where serve said it would be:
	// one data directory, database plus content-addressed blobs (ADR-0017).
	if _, err := os.Stat(store.DBPath(dataDir)); err != nil {
		t.Errorf("the database is not under the data directory: %v", err)
	}
	if entries, err := filepath.Glob(filepath.Join(AssetDir(dataDir), "blobs", "*", "*", "*")); err != nil || len(entries) == 0 {
		t.Errorf("no blob landed in the asset store under %s (%v)", AssetDir(dataDir), err)
	}
}

// doWithHeader issues one request with a single header pair and returns what
// `do` returns, for the retry an editor performs after a 412.
func doWithHeader(t *testing.T, client *http.Client, base, token, method, path, payload, ifMatch string) (int, map[string]any, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", ifMatch)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed map[string]any
	if len(raw) > 0 && json.Valid(raw) {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("body is not JSON: %v (%s)", err, trimForLog(string(raw)))
		}
	}
	return resp.StatusCode, parsed, resp.Header, raw
}

func sortedKeysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// newJar keeps session cookies across the cycle's steps, the way a browser
// would; http.Client ignores a jar-less cookie header otherwise.
func newJar() http.CookieJar {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	return jar
}

// --- serve: the session runtime and the developer inspector ----------------

// TestServeWiresTheSessionRuntime is the M2 acceptance path through the real
// CLI wiring (not a reassembled copy): `serve` starts the session runtime
// over the live database, a replay session with developer tracing runs the
// real pipeline with the production Tesseract engine, and the inspector
// reads back the evidence the run actually stored. The profile is imported
// through the store's own write path before serve starts, the same way an
// operator prepares the database with the CLI.
func TestServeWiresTheSessionRuntime(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("NOT RUN: tesseract is not installed; the session E2E needs the production OCR engine")
	}
	clearGatewayEnv(t)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := filepath.Join(t.TempDir(), "sessions-serve")
	addr := freeListenAddress(t)
	clipDir, err := filepath.Abs(filepath.Join("..", "..", "testdata", "synthetic", "replay", "dialogue-advance-001"))
	if err != nil {
		t.Fatal(err)
	}
	// The serve process inherits the test's environment: point the replay
	// allowlist at the synthetic clip explicitly, the documented production
	// knob (GAME_NARRATOR_REPLAY_ROOTS) rather than the dev-checkout default.
	t.Setenv("GAME_NARRATOR_REPLAY_ROOTS", clipDir)

	if code, err := run([]string{"migrate", "--data-dir", dataDir}, log); err != nil || code != 0 {
		t.Fatalf("migrate: code=%d err=%v", code, err)
	}
	devToken := adminTokenForTest(t, dataDir, log, "dev-user", "developer", "dev-probe")
	opToken := adminTokenForTest(t, dataDir, log, "op-user", "operator", "op-probe")
	viewToken := adminTokenForTest(t, dataDir, log, "view-user", "viewer", "view-probe")

	// The database is prepared before the listener starts, exactly the way
	// an operator prepares it: a published synthetic profile the session
	// can run.
	st, err := store.Open(context.Background(), store.DBPath(dataDir))
	if err != nil {
		t.Fatalf("opening the prepared database: %v", err)
	}
	profileID := seedSyntheticProfileForServe(t, st)
	if err := st.Close(); err != nil {
		t.Fatalf("closing the prepared database: %v", err)
	}

	serveDone := make(chan int, 1)
	go func() {
		code, err := run([]string{"serve", "--listen", addr, "--data-dir", dataDir}, log)
		if err != nil {
			t.Errorf("serve: %v", err)
		}
		serveDone <- code
	}()
	defer func() {
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case code := <-serveDone:
			if code != 0 {
				t.Errorf("serve exit code = %d, want 0 after a clean drain", code)
			}
		case <-time.After(30 * time.Second):
			t.Error("serve did not exit after SIGTERM")
		}
	}()

	client := &http.Client{Timeout: 15 * time.Second}
	base := "http://" + addr + "/api/v1"
	waitReady(t, client, addr)

	call := func(token, method, path string, body []byte) (int, map[string]any, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, base+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var parsed map[string]any
		if json.Valid(raw) {
			_ = json.Unmarshal(raw, &parsed)
		}
		return resp.StatusCode, parsed, raw
	}

	// The session surface requires a credential, whatever its role.
	if code, _, raw := call("", http.MethodPost, "/sessions", []byte(`{}`)); code != http.StatusUnauthorized {
		t.Fatalf("anonymous POST /sessions = %d, want 401 (%s)", code, trimForLog(string(raw)))
	}

	// dev_trace is the developer toggle on an operator route: the operator
	// asks for it and is refused before a session exists.
	startBody := []byte(`{"profile_id":"` + profileID + `","frame_source":{"kind":"replay","dir":"` + clipDir +
		`"},"mode":"conservative","dev_trace":true}`)
	if code, _, raw := call(opToken, http.MethodPost, "/sessions", startBody); code != http.StatusForbidden {
		t.Fatalf("operator dev_trace start = %d, want 403 (%s)", code, trimForLog(string(raw)))
	}

	// The developer starts the replay; the pipeline runs on Tesseract.
	code, started, raw := call(devToken, http.MethodPost, "/sessions", startBody)
	if code != http.StatusCreated {
		t.Fatalf("developer start = %d, want 201 (%s)", code, trimForLog(string(raw)))
	}
	sessID, _ := started["session_id"].(string)
	if sessID == "" {
		t.Fatalf("no session_id in %s", trimForLog(string(raw)))
	}

	// The viewer may read the session's state while it runs.
	if code, _, raw := call(viewToken, http.MethodGet, "/sessions/"+sessID, nil); code != http.StatusOK {
		t.Fatalf("viewer GET /sessions/{id} = %d, want 200 (%s)", code, trimForLog(string(raw)))
	}

	// The replay ends on its own; poll through the API to the terminal state.
	var state string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && state != "stopped" && state != "failed" {
		_, body, raw := call(devToken, http.MethodGet, "/sessions/"+sessID, nil)
		state, _ = body["state"].(string)
		if state == "" {
			t.Fatalf("poll lost the session: %s", trimForLog(string(raw)))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if state != "stopped" {
		t.Fatalf("terminal state = %q, want stopped", state)
	}

	// Stop stays idempotent on the finished session.
	if code, stopped, raw := call(devToken, http.MethodPost, "/sessions/"+sessID+"/stop", []byte(`{}`)); code != http.StatusOK {
		t.Fatalf("stop = %d, want 200 (%s)", code, trimForLog(string(raw)))
	} else if stopped["state"] != "stopped" {
		t.Fatalf("stop response state = %v, want stopped", stopped["state"])
	}

	// The inspector: the developer page carries the session's events, the
	// trace of one observation under its trace id, and the stored frame.
	code, page, raw := call(devToken, http.MethodGet, "/dev/events?session_id="+sessID+"&limit=1000", nil)
	if code != http.StatusOK {
		t.Fatalf("dev/events = %d, want 200 (%s)", code, trimForLog(string(raw)))
	}
	evs, _ := page["events"].([]any)
	var sawStarted, frameAsset string
	for _, rawEv := range evs {
		ev, _ := rawEv.(map[string]any)
		if ev == nil {
			continue
		}
		if ev["name"] == "session.started" {
			sawStarted = "1"
		}
		if ev["name"] == "text.observed" {
			data, _ := ev["data"].(map[string]any)
			if s, ok := data["frame_asset"].(string); ok && s != "" {
				frameAsset = s
			}
		}
	}
	if sawStarted == "" {
		t.Fatalf("dev/events has no session.started for %s (%d events)", sessID, len(evs))
	}
	if frameAsset == "" {
		t.Fatalf("no text.observed carried a frame_asset reference (%d events)", len(evs))
	}
	if code, _, raw := call(viewToken, http.MethodGet, "/dev/events?session_id="+sessID, nil); code != http.StatusForbidden {
		t.Fatalf("viewer dev/events = %d, want 403 (%s)", code, trimForLog(string(raw)))
	}
	code, _, raw = call(devToken, http.MethodGet, "/dev/assets/"+frameAsset, nil)
	if code != http.StatusOK {
		t.Fatalf("dev/assets = %d, want 200 (%s)", code, trimForLog(string(raw)))
	}
}

// seedSyntheticProfileForServe imports the synthetic profile through the
// store's own write path (friendly ids mapped to ULIDs, the production
// import's behaviour) and publishes it. The cmd tests must not link internal
// test helpers, so this is the store-write copy of the seeder the runtime
// and api suites keep for themselves; the seeding rules are a store contract
// all three must respect.
func seedSyntheticProfileForServe(t *testing.T, st *store.Store) string {
	t.Helper()
	profile, err := scenario.LoadProfile(filepath.Join("..", "..", "testdata", "synthetic", "profiles", "dialogue-advance-001.json"))
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	game := games.Game{ID: games.NewID(), Slug: "synthetic-serve-demo", Title: "Synthetic Demo",
		Platform: "gamecube", UserTags: []byte(`["synthetic"]`), CreatedAt: now}
	if err := st.CreateGame(ctx, game); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	edition := games.Edition{ID: games.NewID(), GameID: game.ID, Region: "US",
		Language: "en", Revision: "rev0", Locale: "en_US"}
	if err := st.CreateEdition(ctx, edition); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	draft, err := st.CreateDraftProfile(ctx, edition.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	byFriendly := map[string]string{}
	msgs := make([]games.Message, 0, len(profile.Messages))
	for _, m := range profile.ToMessages() {
		id := games.NewID()
		byFriendly[m.ID] = id
		row := m
		row.ID, row.ProfileID, row.SourceMessageID = id, draft.ID, m.ID
		row.AttributionStatus = games.AttributionUnknown
		row.Revision = 1
		row.CreatedAt, row.UpdatedAt = now, now
		msgs = append(msgs, row)
	}
	if err := st.PutMessages(ctx, draft.ID, msgs, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	regions := make([]games.Region, 0, len(profile.Regions))
	for _, r := range profile.ToRegions() {
		r.ID, r.ProfileID = games.NewID(), draft.ID
		r.DetectMethod = games.DetectManual
		r.Revision = 1
		r.UpdatedAt = now
		regions = append(regions, r)
	}
	if err := st.PutRegions(ctx, draft.ID, regions); err != nil {
		t.Fatalf("PutRegions: %v", err)
	}
	edges := profile.ToEdges()
	for i := range edges {
		edges[i].FromID = byFriendly[edges[i].FromID]
		edges[i].ToID = byFriendly[edges[i].ToID]
		edges[i].CreatedAt = now
	}
	if err := st.PutEdges(ctx, draft.ID, edges); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}
	_, rev, err := st.GetProfile(ctx, draft.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if err := st.SetProfileStatus(ctx, draft.ID, games.StatusReview, rev); err != nil {
		t.Fatalf("SetProfileStatus: %v", err)
	}
	_, rev, err = st.GetProfile(ctx, draft.ID) // SetProfileStatus moves the row version
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	published, err := st.Publish(ctx, draft.ID, rev, "serve-test@local")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return published.ID
}

// --- import-disc ----------------------------------------------------------
//
// The import-disc tests split the same way the windwaker package does: usage
// errors run everywhere, and the end-to-end import runs only when the
// operator's own disc image is present (env-gated, NOT RUN otherwise). The
// end-to-end test asserts counts through the store — never message text.

func TestRunImportDiscUsageErrors(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// No image anywhere: a clean env, an explicit failure with exit 2.
	t.Setenv(EnvGameImage, "")
	code, err := run([]string{"import-disc", "--data-dir", t.TempDir()}, log)
	if code != 2 {
		t.Fatalf("no image: exit code = %d, want 2 (err %v)", code, err)
	}
	if err == nil {
		t.Fatal("no image: expected a usage error")
	}
	// An unreadable image path is a runtime failure (exit 1), not usage.
	code, _ = run([]string{"import-disc", "--data-dir", t.TempDir(), "--image", "/nonexistent/dump.ciso"}, log)
	if code != 1 {
		t.Fatalf("unreadable image: exit code = %d, want 1", code)
	}
	// An unknown flag is usage (exit 2).
	code, _ = run([]string{"import-disc", "--nope"}, log)
	if code != 2 {
		t.Fatalf("unknown flag: exit code = %d, want 2", code)
	}
}

// TestRunImportDiscEndToEnd imports the operator's real disc into a temp data
// dir and asserts the store holds exactly the messages the adapter parsed —
// counts only, no text (docs/spec/14, AGENTS.md rule 5). It also asserts the
// idempotent re-run reports without duplicating.
func TestRunImportDiscEndToEnd(t *testing.T) {
	imagePath := os.Getenv(EnvGameImage)
	if imagePath == "" {
		t.Skip("NOT RUN: " + EnvGameImage + " is not set (import-disc end-to-end needs the operator's disc)")
	}
	if _, err := os.Stat(imagePath); err != nil {
		t.Skip("NOT RUN: disc image not found at " + imagePath)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dataDir := t.TempDir()

	// The expected count comes from parsing the disc through the same
	// adapter the command uses: what the adapter yields is what the store
	// must hold.
	d, err := gcdisc.Open(imagePath)
	if err != nil {
		t.Fatalf("open disc: %v", err)
	}
	defer d.Close()
	ww := windwaker.New().(windwaker.Adapter)
	parsed, err := ww.ParseDisc(context.Background(), d, adapter.ParseOptions{
		ProfileID: "01ARZ3NDEKTSV4RRFFQ69C5BYW",
		Language:  "en",
	})
	if err != nil {
		t.Fatalf("parse the disc for the expected count: %v", err)
	}
	want := len(parsed.Messages)
	if want == 0 {
		t.Fatal("the adapter parsed no messages; the disc is not a readable Wind Waker image")
	}

	first := captureStdout(t, func() int {
		code, err := run([]string{"import-disc", "--data-dir", dataDir, "--image", imagePath}, log)
		if err != nil {
			t.Fatalf("import-disc: %v", err)
		}
		return code
	})
	if first.code != 0 {
		t.Fatalf("import-disc exit code = %d, want 0 (output %q)", first.code, first.output)
	}

	st, err := store.Open(context.Background(), store.DBPath(dataDir))
	if err != nil {
		t.Fatalf("open the import data dir: %v", err)
	}
	defer st.Close()

	game, err := st.GetGameBySlug(context.Background(), "wind-waker")
	if err != nil {
		t.Fatalf("game %q not found after import: %v", "wind-waker", err)
	}
	got, err := countMessagesForTest(context.Background(), st, game)
	if err != nil {
		t.Fatalf("counting messages: %v", err)
	}
	if got != want {
		t.Fatalf("store holds %d messages, the adapter parsed %d", got, want)
	}

	// The source row records the adapter identity and the content hash; the
	// asset store holds the BMG bytes under that hash.
	sources, err := sourcesForTest(context.Background(), st, game)
	if err != nil {
		t.Fatalf("listing sources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("store holds %d source rows, want 1", len(sources))
	}
	src := sources[0]
	if src.SourceType != games.SourceGameExtract || src.Rights != games.RightsUserSupplied {
		t.Fatalf("source row type/rights = %s/%s, want game_extract/user_supplied", src.SourceType, src.Rights)
	}
	if src.Parser != windwaker.ID+"@"+windwaker.Version {
		t.Fatalf("source parser = %q, want %q", src.Parser, windwaker.ID+"@"+windwaker.Version)
	}

	// Idempotency: a second run must report, not duplicate.
	second := captureStdout(t, func() int {
		code, err := run([]string{"import-disc", "--data-dir", dataDir, "--image", imagePath}, log)
		if err != nil {
			t.Fatalf("second import-disc: %v", err)
		}
		return code
	})
	if second.code != 0 {
		t.Fatalf("second import-disc exit code = %d, want 0 (output %q)", second.code, second.output)
	}
	if !strings.Contains(second.output, "already imported") {
		t.Fatalf("second run did not report idempotency: %q", second.output)
	}
	got, err = countMessagesForTest(context.Background(), st, game)
	if err != nil {
		t.Fatalf("recounting messages: %v", err)
	}
	if got != want {
		t.Fatalf("after the second run the store holds %d messages, want %d (re-run duplicated rows)", got, want)
	}
}

// countMessagesForTest counts a game's messages across all of its profiles.
func countMessagesForTest(ctx context.Context, st *store.Store, game games.Game) (int, error) {
	editions, err := st.ListEditions(ctx, game.ID)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, e := range editions {
		profiles, err := st.ListProfiles(ctx, e.ID)
		if err != nil {
			return 0, err
		}
		for _, p := range profiles {
			n, err := countProfileMessages(ctx, st, p.ID)
			if err != nil {
				return 0, err
			}
			total += n
		}
	}
	return total, nil
}

// sourcesForTest lists a game's source rows across all of its profiles.
func sourcesForTest(ctx context.Context, st *store.Store, game games.Game) ([]games.Source, error) {
	editions, err := st.ListEditions(ctx, game.ID)
	if err != nil {
		return nil, err
	}
	var out []games.Source
	for _, e := range editions {
		profiles, err := st.ListProfiles(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range profiles {
			s, err := st.ListSources(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, s...)
		}
	}
	return out, nil
}
