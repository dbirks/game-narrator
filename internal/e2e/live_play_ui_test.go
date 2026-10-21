// Package e2e — the live play path through the real browser UI (task
// browser-live-play, AGENTS.md rule 3: real Dolphin + real providers through
// the real UI, class (d)+(e)).
//
// TestLiveDolphin proves Dolphin frames → OCR → match → Clef → narrator TTS
// through the HTTP API in-process. This test is the same chain driven the way
// a developer actually uses it: the Playwright `live-play` project (web/e2e)
// signs in, starts the session from /play with the Dolphin frame source,
// watches the committed line and the speech counters, opens the developer
// inspector for that session and reads the correlated trace, then stops the
// session from the Play page. The Go side owns what the browser cannot:
//
//   - the server itself. The production binary never wires the dev Dolphin
//     bridge into the process (internal/api/dolphin.go answers 503 for an
//     unwired bridge), so the in-process handler here is the only process
//     that can serve a live kind:dolphin session — exactly the wiring
//     TestLiveDolphin proves. It is served on a real TCP port (18120–18139,
//     the band this test owns inside the 18090–18139 e2e port space) so the
//     browser, not a test recorder, is the client.
//   - the seeding: the script is imported from the disc image through the
//     same API the test helper uses (BMG extracted from the image, uploaded
//     through the profile's import route — no game text in this repo), the
//     story-subtitle region is set and the profile published.
//   - the narrator voice: auditions → approve → register, in the registry's
//     gn_ namespaced name, removed from the Base registry when the run ends.
//   - the screen-driven drive to the opening legend (the same drive table
//     TestLiveDolphin runs), so the legend the browser watches is real.
//
// The Playwright walk runs under a separate minimal runner config
// (web/e2e/live-play.playwright.config.ts, no webServer entries: the suite's
// other servers own ports 18090–18111 that a concurrent gate may hold).
//
// Log-safe per AGENTS.md rule 5: log lines carry counts, ids, hashes and
// timings — never dialogue text, frame content or tokens. Evidence (trace,
// screenshots, WAVs, timeline, manifest) lands in the gitignored bundle
// artifacts/live-play-ui/<run-id>/.
//
// Run (the shared gateway env provides the OCR VLM fallback, the `decision`
// role and the narrator TTS; the gate variable is set by make test-dolphin):
//
//	GN_GATE=test-dolphin go test -p 1 -count=1 -timeout 60m -v \
//	  -run 'TestLivePlayUI$' ./internal/e2e/

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/games/adapter/windwaker"
	"github.com/dbirks/game-narrator/internal/gcdisc"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
	"github.com/dbirks/game-narrator/internal/voices"
	"github.com/dbirks/game-narrator/internal/webassets"
)

// livePlayEvidenceDir is this test's gitignored evidence root (.gitignore:
// artifacts/), beside m6-live.
const livePlayEvidenceDir = "artifacts/live-play-ui"

// livePlayPortCandidates is the port band this test owns for its server:
// inside the 18090–18139 e2e space, above the Playwright suite's 18090–18111
// (web/e2e/support/ports.ts) so a concurrent make test-ui run never owns one.
// A candidate already held by another process is skipped, never killed:
// the no-parallel rule extends to ports as well as emulators.
var livePlayPortCandidates = []int{18120, 18121, 18122, 18123, 18124, 18125, 18126, 18127, 18128, 18129, 18130, 18131, 18132, 18133, 18134, 18135, 18136, 18137, 18138, 18139}

func TestLivePlayUI(t *testing.T) {
	if os.Getenv(dolphin.EnvGate) != "test-dolphin" {
		t.Skipf("live play UI e2e outside the test-dolphin gate: run via make test-dolphin (it sets %s=test-dolphin)", dolphin.EnvGate)
	}
	img := os.Getenv(dolphin.EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set — point it at the user-supplied Wind Waker image (dev-only, the image is never logged)", dolphin.EnvGameImage)
	}
	if _, err := os.Stat(img); err != nil {
		t.Skipf("NOT RUN: disc image not found at the %s path (dev-only)", dolphin.EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo", "pnpm"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH (dev host dependencies: dolphin-emu, xorg-server-xvfb, ffmpeg, x11-utils, pnpm)", bin)
		}
	}
	if !webassets.Built() {
		t.Skipf("NOT RUN: the web UI is not built into the test binary — the Playwright walk needs the packaged SPA the handler serves; run `make web-build` (or `make build`) and re-run")
	}
	home, err := os.UserHomeDir()
	if err == nil {
		chromes, _ := filepath.Glob(filepath.Join(home, ".cache", "ms-playwright", "chromium-*", "chrome-linux*", "chrome"))
		if len(chromes) == 0 {
			t.Skipf("NOT RUN: no Playwright chromium under %s/.cache/ms-playwright — install it with `cd web && pnpm exec playwright install chromium`", home)
		}
	}
	if err := strayDolphinProcs(); err != nil {
		t.Skipf("NOT RUN: %v", err)
	}

	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	gatewayCfg := roles[providers.RoleAgentReasoning] // the shared gateway's URL/token live on every role
	if strings.TrimSpace(gatewayCfg.BaseURL) == "" || strings.TrimSpace(gatewayCfg.Token()) == "" {
		t.Skipf("NOT RUN: set %s and the token env named by %s (default %s) to run the live play UI e2e",
			providers.EnvGatewayURL, providers.EnvGatewayTokenEnv, providers.EnvGatewayToken)
	}
	designCfg, cloneCfg := roles[providers.RoleVoiceDesign], roles[providers.RoleVoiceClone]
	designEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceDesign)
	cloneEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceClone)
	if strings.TrimSpace(designCfg.Model) == "" || strings.TrimSpace(cloneCfg.Model) == "" {
		t.Skipf("NOT RUN: set %s (narrator TTS, e.g. qwen3-tts-voicedesign) and %s (e.g. qwen3-tts-base) to run the live play UI e2e", designEnv, cloneEnv)
	}
	directURL := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"))
	if directURL == "" {
		directURL = strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	}
	if directURL == "" {
		t.Skipf("NOT RUN: set GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL (or GAME_NARRATOR_TTS_BASE_DIRECT_URL) to the Base voice registry for the narrator voice")
	}
	t.Setenv(dolphin.EnvDevDolphin, "1")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	t.Cleanup(cancel)
	startAll := time.Now()

	runID := "liveui-" + time.Now().UTC().Format("20060102T150405Z")
	bundle := filepath.Join(livePlayEvidenceDir, runID)
	t.Logf("live-play-ui: run %s; evidence bundle %s", runID, bundle)
	// ── durable state, jobs, auth ──────────────────────────────────────────
	dataDir := t.TempDir()
	st, err := store.Open(ctx, store.DBPath(dataDir))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	as, err := assets.NewLocal(filepath.Join(dataDir, "assets"))
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}
	adapters := adapter.NewRegistry()
	if err := generic.Register(adapters); err != nil {
		t.Fatalf("generic.Register: %v", err)
	}
	if err := windwaker.Register(adapters); err != nil {
		t.Fatalf("windwaker.Register: %v", err)
	}

	disc := slog.New(slog.DiscardHandler)
	js := store.NewJobsStore(st)
	hub := events.New(events.WithStore(store.NewEventStore(st)), events.WithLogger(disc))
	runner, err := jobs.NewRunner(js, hub, disc, jobs.WithPollInterval(50*time.Millisecond))
	if err != nil {
		t.Fatalf("jobs.NewRunner: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = runner.Stop(stopCtx)
		_ = hub.Close()
	})

	authSvc := auth.NewService(store.NewAuthStore(st), auth.WithPasswordCost(1000), auth.WithLogger(disc))
	// One admin for both halves: the browser signs in with the password (a
	// random value passed to the spec through the environment, never logged),
	// the Go side seeds through a bearer token of the same account. Admin
	// holds the operator role the Play page requires and the developer role
	// its dev-trace flag and the inspector require.
	adminUser := "live-play-admin"
	adminPass, err := randomHex(16)
	if err != nil {
		t.Fatalf("admin password: %v", err)
	}
	adminID, err := func() (string, error) {
		u, err := authSvc.Setup(ctx, adminUser, adminPass, "127.0.0.1:1", true)
		if err != nil {
			return "", fmt.Errorf("auth setup: %w", err)
		}
		return u.ID, nil
	}()
	if err != nil {
		t.Fatalf("%v", err)
	}
	token, _, err := authSvc.CreateToken(ctx, nil, auth.CreateTokenParams{
		UserID: adminID, Name: "live-play-ui", Role: auth.RoleAdmin,
	}, "127.0.0.1:1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// ── voice workflow (the same wiring cmd/game-narrator builds) ─────────
	designClient, err := tts.NewFromRole(designCfg)
	if err != nil {
		t.Fatalf("voice design client: %v", err)
	}
	cloneClient, err := tts.NewFromRole(cloneCfg)
	if err != nil {
		t.Fatalf("voice clone client: %v", err)
	}
	registry, err := tts.NewRegistry(directURL, cloneCfg.Token())
	if err != nil {
		t.Fatalf("voice registry: %v", err)
	}
	voiceSvc := &voices.Service{
		Store:         st,
		Assets:        as,
		Design:        designClient,
		Base:          cloneClient,
		Registry:      registry,
		Speech:        speech.New(as, store.NewSpeechCacheIndex(st), 8, speech.WithReferenceChecker(st)),
		InstanceID:    voices.DefaultInstanceID,
		BaseModelID:   cloneCfg.Model,
		BackendDigest: previewDigest(cloneCfg.Model),
	}
	if err := voices.New(voiceSvc); err != nil {
		t.Fatalf("voice service: %v", err)
	}
	if err := runner.Register(voices.JobKind, voices.JobHandler(voiceSvc), voices.JobHandlerOpts()); err != nil {
		t.Fatalf("register %s: %v", voices.JobKind, err)
	}
	if err := runner.Register(api.JobKindImportFile,
		api.ImportFileJobHandler(st, as, adapters), jobs.RegisterOpts{
			Concurrency: 1, Resumable: true, Timeout: 5 * time.Minute,
		}); err != nil {
		t.Fatalf("register %s: %v", api.JobKindImportFile, err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}

	// ── session runtime: speech, VLM OCR fallback, Clef, Dolphin seam ─────
	var voiceCloser func() // the narrator voice's remote registration cleanup
	voiceCloser = func() {}

	speechOpts := &runtime.SpeechOptions{
		Voices:      voiceSvc,
		Assets:      as,
		Sink:        runtime.SinkFile,
		FileSinkDir: filepath.Join(dataDir, "audio"),
	}
	var ocrFallback *pipeline.OCRFallback
	var ocrFallbackModel string
	if cfg := roles[providers.RoleVisionOCRFallback]; cfg.Configured() {
		client, err := openai.NewFromRole(cfg)
		if err != nil {
			t.Fatalf("vision_ocr_fallback client: %v", err)
		}
		ocrFallback = &pipeline.OCRFallback{Secondary: ocr.NewVLMEngine(ocr.OpenAIClient{Client: client})}
		ocrFallbackModel = cfg.Model
		t.Logf("live-play-ui: VLM OCR fallback ready (model %s)", cfg.Model)
	} else {
		vlmEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVisionOCRFallback)
		t.Logf("live-play-ui: VLM OCR fallback NOT CONFIGURED (set %s); the session runs the local tesseract engine only", vlmEnv)
	}
	var decisionOpts *runtime.DecisionOptions
	if cfg := roles[providers.RoleDecision]; cfg.Configured() {
		roleReg := providers.NewRegistry(roles)
		chatRouter := router.New(roleReg, router.Options{})
		decisionOpts = &runtime.DecisionOptions{
			Router:  chatRouter,
			Role:    cfg,
			Timeout: dolphinClefTimeout,
			Margin:  dolphinClefMargin,
		}
		t.Logf("live-play-ui: Clef decision role configured (model %s, protocol %s)", cfg.Model, cfg.Protocol)
	} else {
		decEnv, _, _, _ := providers.RoleEnvVars(providers.RoleDecision)
		t.Logf("live-play-ui: decision role NOT CONFIGURED (set %s, e.g. clef-omni); the Clef assertions must fail loudly", decEnv)
	}

	// The dev-only Dolphin bridge (the object the runtime's DevDolphin seam
	// and the dev API routes share): created here, wired into both, launched
	// once the seed is in and the Playwright walk is about to start.
	// The bridge's own log goes to the test log (the 2026-10-19 run's blind
	// spot was a discard handler: a bridge that lost its render window or
	// its Xvfb left no trail on either side of the failure it caused).
	bridgeLog := slog.New(slog.NewTextHandler(os.Stdout, nil))
	bridge, err := dolphin.New(dolphin.Options{
		DataDir:       filepath.Join(dataDir, "dolphin"),
		LaunchTimeout: 180 * time.Second,
		Log:           bridgeLog,
	})
	if err != nil {
		t.Fatalf("dolphin.New: %v", err)
	}
	var bridgeLaunched bool
	t.Cleanup(func() {
		if bridgeLaunched {
			bridge.Stop()
		}
	})
	devDolphin := func() (capture.Source, error) {
		return bridge.FrameSource(10)
	}

	sessions, err := runtime.New(runtime.Options{
		Adapters:    adapters,
		Store:       st,
		Hub:         hub,
		Jobs:        runner,
		AssetsRoot:  filepath.Join(dataDir, "dev-trace"),
		Speech:      speechOpts,
		OCRFallback: ocrFallback,
		Decision:    decisionOpts,
		DevDolphin:  devDolphin,
		Log:         disc,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}

	srv := api.New(bootstrap.BuildInfo{Version: "e2e-live-play-ui"}, time.Now(), disc,
		api.WithAuth(authSvc, store.NewAuthStore(st)),
		api.WithStore(st),
		api.WithJobs(js),
		api.WithEvents(hub),
		api.WithImport(as, adapters, runner),
		api.WithSessions(sessions),
		api.WithDevEvents(store.NewEventStore(st)),
		api.WithVoices(voiceSvc, runner),
		api.WithDolphinBridge(bridge),
	).Handler()
	// ── the real TCP listener: the browser's server ────────────────────────
	// One candidate port at a time; a port another process holds is skipped,
	// never killed (the no-parallel rule), and the band exhausted is an
	// honest NOT RUN with the setup line.
	var (
		listener net.Listener
		port     int
	)
	for _, p := range livePlayPortCandidates {
		addr := fmt.Sprintf("127.0.0.1:%d", p)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		listener, port = l, p
		break
	}
	if listener == nil {
		t.Skipf("NOT RUN: every port %d–%d is in use — free one (check `ss -ltnp | grep '127.0.0.1:181[23]'` and kill only a PID this run started) or run on another host", livePlayPortCandidates[0], livePlayPortCandidates[len(livePlayPortCandidates)-1])
	}
	apiURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	apiSrv := &http.Server{Handler: srv}
	t.Cleanup(func() { _ = apiSrv.Close() })
	go func() { _ = apiSrv.Serve(listener) }()
	// Prove the TCP path before the browser trusts it: health through the
	// real socket, not the in-process handler.
	health, err := http.Get(apiURL + "/api/v1/health/live")
	if err != nil {
		t.Fatalf("health over %s: %v", apiURL, err)
	}
	body, _ := io.ReadAll(health.Body)
	_ = health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health over %s = %d: %s", apiURL, health.StatusCode, string(body))
	}
	t.Logf("live-play-ui: server on %s (in-process API + embedded SPA; browser is the client)", apiURL)

	do := func(method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
		return dolphinDo(t, srv, token, method, target, body, headers)
	}
	doJSON := func(method, target string, v any, headers map[string]string) (int, map[string]any) {
		t.Helper()
		var body []byte
		if v != nil {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal %s %s: %v", method, target, err)
			}
			body = b
		}
		rec := do(method, target, body, headers)
		var out map[string]any
		if rec.Body.Len() > 0 {
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
		}
		return rec.Code, out
	}

	// ── 1. catalog: game → edition → draft profile (the UI's create path) ─
	code, out := doJSON(http.MethodPost, "/api/v1/games", map[string]any{
		"slug": "wind-waker", "title": "The Legend of Zelda: The Wind Waker",
		"platform": "gamecube", "user_tags": json.RawMessage(`[]`),
	}, map[string]string{"Idempotency-Key": runID + "-game"})
	if code != http.StatusCreated {
		t.Fatalf("create game = %d: %v", code, out)
	}
	gameID, _ := out["id"].(string)
	imageSHA := dolphinImageSHA(t, img)
	code, out = doJSON(http.MethodPost, "/api/v1/games/"+gameID+"/editions", map[string]any{
		"region": "ntsc-u", "language": "en", "revision": "1", "source_identity": imageSHA,
	}, map[string]string{"Idempotency-Key": runID + "-edition"})
	if code != http.StatusCreated {
		t.Fatalf("create edition = %d: %v", code, out)
	}
	editionID, _ := out["id"].(string)
	code, out = doJSON(http.MethodPost, "/api/v1/editions/"+editionID+"/profiles", nil,
		map[string]string{"Idempotency-Key": runID + "-profile"})
	if code != http.StatusCreated {
		t.Fatalf("create draft profile = %d: %v", code, out)
	}
	profileID, _ := out["id"].(string)
	t.Logf("live-play-ui: catalog ready: game=%s edition=%s profile=%s", gameID, editionID, profileID)

	// ── 2. import the script from the disc image through the API ──────────
	discFile, err := gcdisc.Open(img)
	if err != nil {
		t.Fatalf("open disc image: %v", err)
	}
	bmgBytes, err := windwaker.ExtractBMG(discFile)
	_ = discFile.Close()
	if err != nil {
		t.Fatalf("extract BMG from disc image: %v", err)
	}
	t.Logf("live-play-ui: BMG extracted from the disc image: %d bytes (hash on the import job result, not logged)", len(bmgBytes))
	rec := dolphinDoMultipart(t, srv, token, "/api/v1/profiles/"+profileID+"/import",
		"windwaker.bmg", bmgBytes, map[string]string{
			"format": "bmg", "rights": "user_supplied", "language": "en",
		}, map[string]string{"Idempotency-Key": runID + "-import"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("import upload = %d: %s", rec.Code, rec.Body.String())
	}
	var up struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil || up.JobID == "" {
		t.Fatalf("import upload response: %v: %s", err, rec.Body.String())
	}
	job := dolphinWaitJob(t, srv, token, up.JobID, 5*time.Minute)
	if job.State != "succeeded" {
		t.Fatalf("import job %s ended %s: %s", up.JobID, job.State, string(job.Result))
	}
	var importReport struct {
		Adapter  string `json:"adapter"`
		Format   string `json:"format"`
		Inserted int    `json:"messages_inserted"`
		Updated  int    `json:"messages_updated"`
	}
	if err := json.Unmarshal(job.Result, &importReport); err != nil {
		t.Fatalf("import job result decode: %v", err)
	}
	if importReport.Inserted < 1000 {
		t.Fatalf("import inserted only %d messages; the real Wind Waker script is a large set (>=1000)", importReport.Inserted)
	}
	t.Logf("live-play-ui: import done: job=%s adapter=%s format=%s inserted=%d updated=%d", up.JobID, importReport.Adapter, importReport.Format, importReport.Inserted, importReport.Updated)

	// ── 3. the story-subtitle region, through the regions API ────────────
	regionBody := map[string]any{
		"regions": []map[string]any{{
			"label":       "opening legend subtitle",
			"region_type": "subtitle",
			"x":           dolphinRegionX0,
			"y":           dolphinRegionY0,
			"w":           dolphinRegionX1 - dolphinRegionX0,
			"h":           dolphinRegionY1 - dolphinRegionY0,
		}},
	}
	var profileEtag string
	{
		rec := do(http.MethodGet, "/api/v1/profiles/"+profileID, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("get profile = %d: %s", rec.Code, rec.Body.String())
		}
		profileEtag = rec.Header().Get("ETag")
		if profileEtag == "" {
			t.Fatalf("GET profile carries no ETag (the If-Match token for the region write)")
		}
	}
	rec = do(http.MethodPut, "/api/v1/profiles/"+profileID+"/regions",
		mustMarshal(t, regionBody), map[string]string{"If-Match": profileEtag})
	if rec.Code != http.StatusOK {
		t.Fatalf("put regions = %d: %s", rec.Code, rec.Body.String())
	}
	profileEtag = rec.Header().Get("ETag")

	// ── 4. draft → review → publish (only the published revision runs) ────
	rec = do(http.MethodPost, "/api/v1/profiles/"+profileID+"/status",
		[]byte(`{"to":"review"}`), map[string]string{"If-Match": profileEtag})
	if rec.Code != http.StatusOK {
		t.Fatalf("set profile status review = %d: %s", rec.Code, rec.Body.String())
	}
	profileEtag = rec.Header().Get("ETag")
	rec = do(http.MethodPost, "/api/v1/profiles/"+profileID+"/publish",
		[]byte(`{}`), map[string]string{"If-Match": profileEtag})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish profile = %d: %s", rec.Code, rec.Body.String())
	}
	t.Logf("live-play-ui: profile %s published with the story-subtitle region", profileID)

	// ── 5. the narrator voice: auditions → approve → register ────────────
	code, out = doJSON(http.MethodPost, "/api/v1/voices/auditions", map[string]any{
		"character": dolphinVoiceCharacter,
		"brief":     "A warm, calm narrator voice for storybook legend narration. Clear enunciation, gentle pace.",
		"count":     2,
	}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("voice auditions create = %d: %v", code, out)
	}
	voiceID, _ := out["voice_id"].(string)
	job = dolphinWaitJob(t, srv, token, out["job_id"].(string), 10*time.Minute)
	if job.State != "succeeded" {
		t.Fatalf("voice auditions job ended %s: %s", job.State, string(job.Result))
	}
	var voiceDetail struct {
		State     string `json:"state"`
		Auditions []struct {
			ID string `json:"id"`
		} `json:"auditions"`
	}
	{
		rec := do(http.MethodGet, "/api/v1/voices/"+voiceID, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("get voice = %d: %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &voiceDetail); err != nil {
			t.Fatalf("voice detail decode: %v", err)
		}
	}
	if voiceDetail.State != "auditioning" || len(voiceDetail.Auditions) == 0 {
		t.Fatalf("voice %s state=%s auditions=%d; want auditioning with clips", voiceID, voiceDetail.State, len(voiceDetail.Auditions))
	}
	auditionID := voiceDetail.Auditions[0].ID
	rec = do(http.MethodPost, "/api/v1/voices/"+voiceID+"/approve",
		mustMarshal(t, map[string]any{
			"audition_id": auditionID,
			"consent":     "original_synthetic_design (game-narrator e2e live play UI run)",
		}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("voice approve = %d: %s", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodPost, "/api/v1/voices/"+voiceID+"/register", []byte(`{}`), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("voice register = %d: %s", rec.Code, rec.Body.String())
	}
	var reg struct {
		State          string `json:"state"`
		RegisteredName string `json:"registered_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil {
		t.Fatalf("voice register response: %v", err)
	}
	t.Logf("live-play-ui: narrator voice %s: auditions=%d registered=%s name=%s", voiceID, len(voiceDetail.Auditions), reg.State, reg.RegisteredName)
	// The Base registry keeps gn_-named voices; this one belongs to this run,
	// so its removal is part of the run, not a leak.
	if reg.RegisteredName != "" {
		voiceCloser = func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer ccancel()
			if err := registry.Remove(cctx, reg.RegisteredName); err != nil {
				t.Logf("live-play-ui: voice cleanup: %v (voice %s left on the Base registry)", err, reg.RegisteredName)
			} else {
				t.Logf("live-play-ui: voice cleanup: registered name %s removed", reg.RegisteredName)
			}
		}
	}
	t.Cleanup(voiceCloser)
	// ── 6. launch the real Dolphin bridge ─────────────────────────────────
	launchCtx, launchCancel := context.WithCancel(context.Background())
	t.Cleanup(launchCancel)
	if err := bridge.Launch(launchCtx, dolphin.LaunchSpec{}); err != nil {
		t.Fatalf("dolphin launch: %v", err)
	}
	bridgeLaunched = true
	st2 := bridge.State()
	if st2.State != dolphin.StateRunning || st2.ImageSHA == "" {
		t.Fatalf("bridge state=%s after launch (want running with the image hash recorded)", st2.State)
	}
	t.Logf("live-play-ui: dolphin running: display=%s pid=%d (image hash recorded by the bridge; the path is never logged)", st2.Display, st2.DolphinPID)

	// ── 7. the screen-driven drive to the opening legend ──────────────────
	// The drive writes one evidence PNG per iteration into the bundle's drive
	// dir; it runs while the Playwright walk is in flight, because the
	// browser starts the session that turns the legend into a commit.
	if err := os.MkdirAll(filepath.Join(bundle, "drive"), 0o755); err != nil {
		t.Fatalf("create drive evidence dir: %v", err)
	}
	type driveResult struct {
		drive dolphin.DriveTrace
		err   error
	}
	driveCh := make(chan driveResult, 1)
	driveWall := time.Now()
	go func() {
		d, derr := dolphin.RunDrive(ctx, bridge, dolphin.DriveOptions{
			Deadline:    10 * time.Minute,
			EvidenceDir: filepath.Join(bundle, "drive"),
		}, dolphin.WindWakerNewGameDrive())
		// The full transition log carries what the 2026-10-19 post-mortem
		// needed and the one-line-per-screen log did not: the measured side
		// and the classifier's numbers on every screen (the name_confirm
		// flake was invisible without them).
		t.Logf("live-play-ui: drive transitions:\n%s", d.TransitionLog())
		t.Logf("live-play-ui: drive: ok=%v reason=%q wall=%s success_screen=%s", d.OK, d.Reason, time.Since(driveWall).Round(time.Second), d.SuccessScreen)
		driveCh <- driveResult{drive: d, err: derr}
	}()

	// ── 8. the Playwright walk: the browser half of the chain ────────────
	webDir := filepath.Join("..", "..")
	webDir = filepath.Join(webDir, "web")
	// The runner writes its per-test artifacts (the trace) into the suite's
	// gitignored output root, namespaced for this run; clear a previous run's
	// leftovers so the bundle's trace copy carries only this run's artifacts.
	if err := os.RemoveAll(filepath.Join(webDir, "test-results", "live-play")); err != nil {
		t.Fatalf("clear live-play output dir: %v", err)
	}
	plCmd := exec.Command("pnpm", "exec", "playwright", "test", "--config", "e2e/live-play.playwright.config.ts")
	plCmd.Dir = webDir
	plCmd.Env = append(os.Environ(),
		"GN_LIVE_UI_URL="+apiURL,
		"GN_LIVE_UI_ADMIN="+adminUser,
		"GN_LIVE_UI_PASS="+adminPass,
		"GN_LIVE_UI_BUNDLE="+absPath(t, bundle),
	)
	var plOut bytes.Buffer
	plCmd.Stdout = &plOut
	plCmd.Stderr = &plOut
	t.Logf("live-play-ui: running the Playwright live-play walk against %s", apiURL)
	plStart := time.Now()
	if err := plCmd.Start(); err != nil {
		t.Fatalf("playwright start: %v", err)
	}
	plDone := make(chan error, 1)
	go func() { plDone <- plCmd.Wait() }()

	// ── 8b. the Go-side watchdog: fail fast on the faults the walk cannot name ──
	// The 2026-10-19 run burned its whole 25 m locator wait while both
	// halves were already dead: the drive had stopped at minute 4 (the
	// name_confirm flake) and the browser session's capture source never
	// delivered a frame. The walk can only report "no line appeared"; the
	// Go side can name which half died and why — the drive result and the
	// session snapshot with its capture-source self-report. The watchdog
	// watches both and fails the run at the first named fault, with the
	// diagnostics that say why, instead of after 25 silent minutes.
	var (
		monMu   sync.Mutex
		monID   string
		monInfo runtime.Info
		monHave bool
	)
	monFault := make(chan string, 1)
	monStop := make(chan struct{})
	defer close(monStop)
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		var (
			id    string
			first time.Time
		)
		for {
			select {
			case <-monStop:
				return
			case <-tick.C:
			}
			if id == "" {
				id, _ = liveSessionID(t, srv, token)
				if id == "" {
					continue
				}
				first = time.Now()
				t.Logf("live-play-ui: watchdog: browser session %s observed", id)
			}
			info, ok := sessions.Get(id)
			if !ok {
				continue
			}
			monMu.Lock()
			monID, monInfo, monHave = id, info, true
			monMu.Unlock()
			t.Logf("live-play-ui: watchdog: %s state=%s delivered=%d processed=%d ocr=%d commits=%d source=%s/%s restarts=%d",
				id, info.State, info.Stats.FramesDelivered, info.Stats.FramesProcessed,
				info.Stats.OCRCalls, info.Stats.Commits, info.Source.ID, info.Source.State, info.Source.Restarts)
			if info.Source.LastError != "" {
				t.Logf("live-play-ui: watchdog: source fault: %s", info.Source.LastError)
			}
			var fault string
			switch {
			case info.State == runtime.StateRunning && info.Stats.FramesDelivered == 0 && info.Source.LastError != "":
				fault = fmt.Sprintf("the session's capture source reports a fault and has delivered zero frames (source %s state=%s restarts=%d last_error=%q)", info.Source.ID, info.Source.State, info.Source.Restarts, info.Source.LastError)
			case info.State == runtime.StateRunning && info.Stats.FramesDelivered == 0 && time.Since(first) > 5*time.Minute:
				fault = fmt.Sprintf("the session has delivered zero frames for 5 minutes (source %s state=%s restarts=%d last_error=%q)", info.Source.ID, info.Source.State, info.Source.Restarts, info.Source.LastError)
			}
			if fault != "" {
				select {
				case monFault <- fault:
				default:
				}
			}
		}
	}()
	// Wait for the walk's outcome, a named watchdog fault, or the drive's
	// result (a drive that cannot reach the legend can never produce the
	// commit the walk waits for — its result is the first half of the
	// chain, so it is the first thing to fail fast).
	var (
		plErr    error
		walkDone bool
		driveGot bool
		driveRes driveResult
	)
	for !walkDone {
		select {
		case plErr = <-plDone:
			walkDone = true
		case fault := <-monFault:
			_ = plCmd.Process.Kill() // this test's own child: killing it is cleanup, not an escape
			// The drive's result may not be in yet; the zero trace then is the
			// truth (the drive is still running — the session's numbers are
			// the evidence that matters).
			t.Fatalf("live-play-ui aborted before the walk could name its failure: %s\n--- diagnostics ---\n%s", fault, livePlayDiagnostics(bridge, monID, monInfo, monHave, dataDir, &driveRes.drive))
		case res := <-driveCh:
			driveGot = true
			driveRes = res
			if res.err != nil || !res.drive.OK {
				_ = plCmd.Process.Kill()
				t.Fatalf("the drive to the opening legend stopped, so the walk's target commit can never happen: %s (last screen %s)\n--- diagnostics ---\n%s", res.drive.Reason, res.drive.LastKey, livePlayDiagnostics(bridge, monID, monInfo, monHave, dataDir, &res.drive))
			}
		}
	}
	if !driveGot {
		// The walk finished; the drive's buffered result must exist by now
		// (a commit on the page is proof the legend was reached, which is
		// proof the drive returned).
		driveRes = <-driveCh
		if driveRes.err != nil || !driveRes.drive.OK {
			_ = plCmd.Process.Kill()
			t.Fatalf("the drive to the opening legend stopped: %s (last screen %s)\n--- diagnostics ---\n%s", driveRes.drive.Reason, driveRes.drive.LastKey, livePlayDiagnostics(bridge, monID, monInfo, monHave, dataDir, &driveRes.drive))
		}
	}
	plWall := time.Since(plStart).Round(time.Second)
	if plErr != nil {
		tail := lastLines(plOut.String(), 40)
		t.Fatalf("Playwright live-play walk failed (%v, wall=%s): the emulator keeps running under the bridge until the cleanup stops it.\n--- playwright output (tail) ---\n%s\n--- diagnostics ---\n%s", plErr, plWall, tail, livePlayDiagnostics(bridge, monID, monInfo, monHave, dataDir, &driveRes.drive))
	}
	t.Logf("live-play-ui: Playwright walk passed (wall=%s)", plWall)
	// ── 9. the correlated chain, read back through the developer API ──────
	// The browser already asserted the chain in the inspector; this is the
	// durable half of the same proof (the developer API the UI polls), so
	// the bundle carries the events the UI rendered, independent of the
	// browser run.
	var (
		commit     devEnvelope
		chainReady bool
	)
	sessionID, err := liveSessionID(t, srv, token)
	if err != nil {
		t.Fatalf("dev events: %v", err)
	}
	if sessionID == "" {
		t.Fatalf("the browser's session left no events on the dev log; bundle %s", bundle)
	}
	t.Logf("live-play-ui: session %s (created by the browser, resolved through the dev event log)", sessionID)
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	wait := time.NewTimer(10 * time.Minute)
	defer wait.Stop()
	var evs []devEnvelope
	for {
		select {
		case <-wait.C:
			t.Fatalf("no committed-and-spoken chain within 10m after the UI walk (bundle %s carries the events so far)", bundle)
		case <-poll.C:
		}
		evs, err = dolphinDevEvents(t, srv, token, sessionID)
		if err != nil {
			t.Fatalf("dev events: %v", err)
		}
		writeEvidenceJSON(t, filepath.Join(bundle, "timeline.json"), evs)
		chainReady = findDolphinChain(t, evs, &commit)
		if chainReady {
			break
		}
		// Fail fast on a terminal session (issue #39): the runtime publishes
		// session.stopped last — after the session's audio has ended — so a
		// terminal event on the page means no further event of the session
		// will ever arrive, and the 10-minute budget is for running sessions.
		// The failure names the terminal state and the last stats the rerun
		// needs; the 2026-10-20 run instead idled the emulator for 10 m on a
		// session the walk had already stopped.
		if term, terminal := sessionTerminal(evs, sessionID); terminal {
			t.Fatalf("no committed-and-spoken chain: the session is terminal (state=%s reason=%s; last stats: frames_delivered=%d frames_processed=%d ocr_calls=%d rankings=%d commits=%d), so no further events will ever arrive (bundle %s carries the events so far)",
				term.state, term.reason, term.stats.FramesDelivered, term.stats.FramesProcessed,
				term.stats.OCRCalls, term.stats.Rankings, term.stats.Commits, bundle)
		}
	}
	if commit.TraceID == "" {
		t.Fatalf("chain found but the commit event carries no trace id: %s", commit.EventID)
	}
	t.Logf("live-play-ui: correlated chain complete on trace %s (commit %s)", commit.TraceID, commit.EventID)

	// ── 10. stop-proof: the Play page's Stop took the session terminal ────
	var final struct {
		State  string         `json:"state"`
		Reason string         `json:"reason"`
		Stats  pipeline.Stats `json:"stats"`
	}
	{
		rec := do(http.MethodGet, "/api/v1/sessions/"+sessionID, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("session get = %d: %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &final); err != nil {
			t.Fatalf("session snapshot decode: %v", err)
		}
	}
	if final.State != "stopped" {
		t.Fatalf("session %s state=%s after the Play page's Stop (want stopped); bundle %s", sessionID, final.State, bundle)
	}
	t.Logf("live-play-ui: session %s stopped by the UI: state=%s reason=%s frames_processed=%d ocr_calls=%d rankings=%d", sessionID, final.State, final.Reason, final.Stats.FramesProcessed, final.Stats.OCRCalls, final.Stats.Rankings)

	// The drive's result was already consumed and checked by the step-8
	// wait (a failed drive aborts the run there, before the walk's 25 m
	// locator wait can burn the budget).
	copyPlaywrightTrace(t, webDir, filepath.Join(bundle, "playwright"))

	// ── 11. the docs/spec/12 live-E2E assertion set (the same contract as
	//      TestLiveDolphin: real frames, script match, Clef request, TTS
	//      bytes + playback, the correlation chain, the bundle manifest) ──
	var snapshot struct {
		Audio *struct {
			Sink   string      `json:"sink"`
			Speech speechCount `json:"speech"`
		} `json:"audio"`
		Decision *struct {
			Configured bool   `json:"configured"`
			Degraded   bool   `json:"degraded"`
			Reason     string `json:"reason"`
			Calls      int64  `json:"calls"`
			Answered   int64  `json:"answered"`
		} `json:"decision"`
	}
	{
		rec := do(http.MethodGet, "/api/v1/sessions/"+sessionID, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("session get (full) = %d: %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
			t.Fatalf("session snapshot decode: %v", err)
		}
	}
	assertDolphinLive(t, &dolphinAsserts{
		bundle:    bundle,
		srv:       srv,
		token:     token,
		profileID: profileID,
		session: struct {
			State  string         `json:"state"`
			Reason string         `json:"reason"`
			Stats  pipeline.Stats `json:"stats"`
			Audio  *struct {
				Sink   string      `json:"sink"`
				Speech speechCount `json:"speech"`
			} `json:"audio"`
			Decision *struct {
				Configured bool   `json:"configured"`
				Degraded   bool   `json:"degraded"`
				Reason     string `json:"reason"`
				Calls      int64  `json:"calls"`
				Answered   int64  `json:"answered"`
			} `json:"decision"`
		}{
			State:    final.State,
			Reason:   final.Reason,
			Stats:    final.Stats,
			Audio:    snapshot.Audio,
			Decision: snapshot.Decision,
		},
		sessionID:      sessionID,
		commit:         commit,
		bridge:         bridge,
		evidenceEvents: evs,
		drive:          driveRes.drive,
		voice:          voiceID,
		fileSinkDir:    speechOpts.FileSinkDir,
		devTraceDir:    filepath.Join(dataDir, "dev-trace", sessionID),
		models: map[string]string{
			"decision":            roles[providers.RoleDecision].Model,
			"vision_ocr_fallback": ocrFallbackModel,
			"voice_design":        designCfg.Model,
			"voice_clone":         cloneCfg.Model,
			"ocr_engine":          "tesseract",
		},
		runID:    runID,
		startAll: startAll,
	})
	t.Logf("live-play-ui: PASS in %s — the live play path ran through the real UI: %s (bundle %s)", time.Since(startAll).Round(time.Second), apiURL, bundle)
}

// LINES-HELPERS

// liveSessionID resolves the browser-created session's id through the dev
// event log (this server runs exactly one session; the session API has no
// list route, so the log — the developer UI's own surface — is the resolver).
func liveSessionID(t *testing.T, srv http.Handler, token string) (string, error) {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		rec := dolphinDo(t, srv, token, http.MethodGet, "/api/v1/dev/events?since_seq=0&limit=100", nil, nil)
		if rec.Code != http.StatusOK {
			return "", fmt.Errorf("dev events page = %d: %s", rec.Code, rec.Body.String())
		}
		var p devEventsPage
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			return "", fmt.Errorf("dev events decode: %w", err)
		}
		for _, e := range p.Events {
			if e.SessionID != "" {
				return e.SessionID, nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", nil
}

// copyPlaywrightTrace copies the runner's trace artifacts (web/test-results/
// live-play, gitignored) into the evidence bundle so the bundle is
// self-contained. The orchestrator clears that dir before the run, so what
// lands here is this run's trace.
func copyPlaywrightTrace(t *testing.T, webDir, dest string) {
	t.Helper()
	root := filepath.Join(webDir, "test-results", "live-play")
	walk := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a vanished run dir is not a trace-copy failure
		}
		if d.IsDir() || d.Name() != "trace.zip" {
			return nil
		}
		if err := copyEvidence(path, filepath.Join(dest, d.Name())); err != nil {
			t.Logf("live-play-ui: trace copy %s: %v", path, err)
		}
		return nil
	})
	if err := walk; err != nil {
		t.Logf("live-play-ui: trace walk: %v", err)
	}
}

// absPath resolves the (package-relative) bundle dir so the Playwright
// child — which runs from web/ — can write its screenshots into it.
func absPath(t *testing.T, rel string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("absPath: %v", err)
	}
	return filepath.Join(wd, rel)
}

// lastLines returns the last n lines of s for a failure report (the full
// output is already on disk only in the runner's test-results dir; the tail
// is what an operator needs in the test log).
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// randomHex is n random bytes as a hex string (the browser login password;
// passed to the spec through the environment, never logged).
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// devTraceSamples counts the frame evidence the runtime's dev-trace store
// retained for one session (one hash-addressed blob per retained sample under
// the session's private store dir). A zero count is itself evidence: the
// pipeline saw no frame to record.
func devTraceSamples(dataDir, sessionID string) int {
	root := filepath.Join(dataDir, "dev-trace", sessionID)
	count := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			count++
		}
		return nil
	})
	return count
}

// livePlayDiagnostics assembles the "why did the live run die" bundle for
// every Fatalf on the live-play-ui failure path: the bridge's own state
// (display, PIDs, its last error), the session snapshot (state, the
// pipeline's counters, the capture source's self-report — the field the
// 2026-10-19 run lacked), the dev-trace sample count, and the drive's
// transition log with its measured sides. No game text: ids, counters and
// class labels only (AGENTS.md rule 5).
func livePlayDiagnostics(bridge dolphin.Bridge, sessionID string, snap runtime.Info, haveSnap bool, dataDir string, drive *dolphin.DriveTrace) string {
	st := bridge.State()
	var b strings.Builder
	fmt.Fprintf(&b, "bridge: state=%s display=%s dolphin_pid=%d xvfb_pid=%d last_error=%q\n",
		st.State, st.Display, st.DolphinPID, st.XvfbPID, st.LastError)
	if haveSnap {
		fmt.Fprintf(&b, "session %s: state=%s reason=%q delivered=%d processed=%d dropped=%d ocr=%d rankings=%d commits=%d\n",
			snap.ID, snap.State, snap.Reason, snap.Stats.FramesDelivered, snap.Stats.FramesProcessed,
			snap.Stats.FramesDropped, snap.Stats.OCRCalls, snap.Stats.Rankings, snap.Stats.Commits)
		fmt.Fprintf(&b, "source: id=%s state=%s delivered=%d restarts=%d fps=%.1f last_error=%q\n",
			snap.Source.ID, snap.Source.State, snap.Source.Delivered, snap.Source.Restarts, snap.Source.FPS, snap.Source.LastError)
		fmt.Fprintf(&b, "dev-trace: %d samples retained\n", devTraceSamples(dataDir, snap.ID))
	} else {
		b.WriteString("session: none observed by the watchdog (the browser may not have created one yet)\n")
	}
	if drive != nil {
		fmt.Fprintf(&b, "drive: ok=%v reason=%q last_key=%q iterations=%d\ntransitions:\n%s\n",
			drive.OK, drive.Reason, drive.LastKey, len(drive.Transitions), drive.TransitionLog())
	}
	return b.String()
}

// sessionTerminalReport is the step-9 poll's fail-fast decision (issue #39):
// the session's terminal event is already on the dev log, and the report
// names the terminal state, its reason and the last stats the terminal
// event carried — the diagnosis a rerun needs instead of the 10-minute
// silence.
type sessionTerminalReport struct {
	state  string // runtime.StateFailed or runtime.StateStopped
	reason string // the terminal event's reason: requested, source_end, error, closed
	stats  pipeline.Stats
}

// sessionTerminal decides, from one session's decoded event page, whether the
// session is already terminal — and if so which terminal state. It is pure
// over the decoded envelopes, so it unit-tests without an emulator, server
// or provider.
//
// Soundness: the runtime publishes the terminal events last
// (internal/runtime's session run: stopSpeech — pending synthesis cancelled,
// active playback stopped — runs before session.failed / session.stopped go
// out, and the publisher suppresses the reducer's duplicate session.stopped).
// Once session.stopped is on the durable log, no further event of that
// session can arrive — in particular no speech.ended — so a terminal session
// whose chain is incomplete can never complete it, and the poll must not
// burn the 10-minute budget idling the emulator under the bridge. The
// budget still covers sessions that are still running.
//
// A terminal event only counts for sessionID (the caller's session): the
// poll's own page is already filtered to it (dolphinDevEvents), and the
// match keeps the decision honest on a page that is not.
func sessionTerminal(evs []devEnvelope, sessionID string) (sessionTerminalReport, bool) {
	var rep sessionTerminalReport
	terminal := false
	for _, e := range evs {
		if e.SessionID != sessionID {
			continue
		}
		switch e.Name {
		case runtime.EventSessionFailed:
			if rep.state == "" {
				rep.state = runtime.StateFailed
			}
		case events.SessionStopped:
			terminal = true
			if rep.state == "" {
				rep.state = runtime.StateStopped
			}
			var d struct {
				Reason string         `json:"reason"`
				Stats  pipeline.Stats `json:"stats"`
			}
			if err := json.Unmarshal(e.Data, &d); err == nil {
				rep.reason = d.Reason
				rep.stats = d.Stats
			}
		}
	}
	return rep, terminal
}
