// Package e2e — the first real live E2E (task m6-live-e2e):
//
// Dolphin (real emulator, real disc image) → live frame capture → OCR
// (Tesseract + the configured VLM fallback, ADR-0040) → candidate match
// against the script imported from the same disc image through the API's
// import path → the Clef (System One `decision` role) adjudication → commit
// → narrator TTS (qwen3-tts family) played through the file sink — with the
// whole chain correlated by trace id through the developer event API the
// developer UI polls (GET /api/v1/dev/events?session_id=…).
//
// This is a live test (AGENTS.md test category (d)+(e): real model providers
// and the Dolphin emulator). It SKIPs as NOT RUN unless the prerequisites
// are present (game image, emulator host tools, gateway, TTS roles); the
// Clef and OCR-fallback roles are NOT skip gates — when they are missing the
// run still exercises every other stage and the corresponding assertion
// fails loudly (the spec's "never label a mocked workflow as real" rule
// applies in reverse: a stage the live run could not prove is a failure with
// evidence, not a silent skip).
//
// Log-safe per AGENTS.md rule 5: the test's log lines carry counts, ids,
// hashes and timings — never dialogue text or tokens. The evidence bundle
// (artifacts/m6-live/<run-id>/, gitignored) is where raw payloads, frames
// and audio live.
//
// Run (the shared gateway env provides the OCR VLM fallback, the
// `decision` role = clef-omni and the narrator TTS):
//
//	GAME_NARRATOR_GAME_IMAGE_PATH=… go test -p 1 -count=1 -timeout 25m -v \
//	  -run 'TestLiveDolphin' ./internal/e2e/

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
)

// The story-subtitle region the drive measured (normalised frame fractions,
// ADR-0043 amendment 6): the opening legend's letterbox panel, white
// narration text in the bottom band.
const (
	dolphinRegionX0 = 0.120
	dolphinRegionY0 = 0.750
	dolphinRegionX1 = 0.880
	dolphinRegionY1 = 0.920

	dolphinVoiceCharacter = "narrator"
	// The commitment policy's adjudication threshold: a margin this high
	// means every locally-accepted ranking is worth one Clef call (the
	// documented knob — DecisionOptions.Margin). Zero would consult Clef
	// only on local abstention, which a clear match would not trigger.
	dolphinClefMargin = 0.99
	// The whole adjudication's bound (retries included): the role's 1.5 s
	// default is a hot-path budget; a cold gateway forward pass can take
	// longer, and the docs/spec/12 live gate wants the request proven, not
	// raced.
	dolphinClefTimeout = 20 * time.Second
	// CER of the OCR reading against the matched imported line
	// (docs/spec/12 "real frame input was recognised").
	dolphinMaxCER = 0.15
)

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }
func hexEncode(s [32]byte) string { return hex.EncodeToString(s[:]) }

// dolphinLivePrereq is one NOT RUN condition with its setup line.
func TestLiveDolphin(t *testing.T) {
	// The live Dolphin e2e belongs to the test-dolphin gate (ADR-0065): it
	// boots the real emulator AND calls the real gateway, so it must never
	// run from make test-unit on a host that happens to have both configured.
	if os.Getenv(dolphin.EnvGate) != "test-dolphin" {
		t.Skipf("live Dolphin e2e outside the test-dolphin gate: run via make test-dolphin (it sets %s=test-dolphin)", dolphin.EnvGate)
	}
	img := os.Getenv(dolphin.EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set — point it at the user-supplied Wind Waker image (dev-only, the image is never logged)", dolphin.EnvGameImage)
	}
	if _, err := os.Stat(img); err != nil {
		t.Skipf("NOT RUN: disc image not found at the %s path (dev-only)", dolphin.EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH (dev host dependencies: dolphin-emu, xorg-server-xvfb, ffmpeg, x11-utils)", bin)
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
		t.Skipf("NOT RUN: set %s and the token env named by %s (default %s) to run the live Dolphin e2e",
			providers.EnvGatewayURL, providers.EnvGatewayTokenEnv, providers.EnvGatewayToken)
	}
	designCfg, cloneCfg := roles[providers.RoleVoiceDesign], roles[providers.RoleVoiceClone]
	designEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceDesign)
	cloneEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceClone)
	if strings.TrimSpace(designCfg.Model) == "" || strings.TrimSpace(cloneCfg.Model) == "" {
		t.Skipf("NOT RUN: set %s (narrator TTS, e.g. qwen3-tts-voicedesign) and %s (e.g. qwen3-tts-base) to run the live Dolphin e2e", designEnv, cloneEnv)
	}
	directURL := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"))
	if directURL == "" {
		directURL = strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	}
	if directURL == "" {
		t.Skipf("NOT RUN: set GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL (or GAME_NARRATOR_TTS_BASE_DIRECT_URL) to the Base voice registry for the narrator voice")
	}

	// The dev-only Dolphin seam the runtime consults for kind:dolphin.
	t.Setenv(dolphin.EnvDevDolphin, "1")

	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Minute)
	t.Cleanup(cancel)
	startAll := time.Now()

	runID := "m6live-" + time.Now().UTC().Format("20060102T150405Z")
	bundle := filepath.Join(dolphinEvidenceDir, runID)
	t.Logf("e2e: run %s; evidence bundle %s", runID, bundle)

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
	if _, err := authSvc.Setup(ctx, "e2e-admin", e2ePassword, "127.0.0.1:1", true); err != nil {
		t.Fatalf("auth setup: %v", err)
	}
	dev, err := authSvc.CreateUser(ctx, "e2e-dolphin-dev", e2ePassword, auth.RoleDeveloper, nil, "e2e-live")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	token, _, err := authSvc.CreateToken(ctx, nil, auth.CreateTokenParams{
		UserID: dev.ID, Name: "e2e-dolphin", Role: auth.RoleDeveloper,
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
	// The voice.auditions job kind the API route enqueues onto.
	if err := runner.Register(voices.JobKind, voices.JobHandler(voiceSvc), voices.JobHandlerOpts()); err != nil {
		t.Fatalf("register %s: %v", voices.JobKind, err)
	}
	// The import.file job kind the API's import route enqueues onto.
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

	// The VLM OCR fallback (ADR-0040): wired when the vision_ocr_fallback
	// role is configured — the documented shape when it is not is nil, and
	// the session then runs the local engine only.
	var ocrFallback *pipeline.OCRFallback
	var ocrFallbackModel string
	if cfg := roles[providers.RoleVisionOCRFallback]; cfg.Configured() {
		client, err := openai.NewFromRole(cfg)
		if err != nil {
			t.Fatalf("vision_ocr_fallback client: %v", err)
		}
		ocrFallback = &pipeline.OCRFallback{Secondary: ocr.NewVLMEngine(ocr.OpenAIClient{Client: client})}
		ocrFallbackModel = cfg.Model
		t.Logf("e2e: VLM OCR fallback ready (model %s)", cfg.Model)
	} else {
		vlmEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVisionOCRFallback)
		t.Logf("e2e: VLM OCR fallback NOT CONFIGURED (set %s); the session runs the local tesseract engine only", vlmEnv)
	}

	// The Clef (System One) adjudication wiring (ADR-0050). A decision role
	// that is not configured keeps the sessions local — and then the
	// assertion below fails loudly with "clef not consulted by runtime"
	// rather than skipping or faking the stage.
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
		t.Logf("e2e: Clef decision role configured (model %s, protocol %s)", cfg.Model, cfg.Protocol)
	} else {
		decEnv, _, _, _ := providers.RoleEnvVars(providers.RoleDecision)
		t.Logf("e2e: decision role NOT CONFIGURED (set %s, e.g. clef-omni); the session will run local and the Clef assertion must fail", decEnv)
	}

	// The dev-only Dolphin frame source seam: the running bridge hands its
	// live X11 capture to the session exactly the way the production dev
	// host wires it.
	var bridge dolphin.Bridge
	devDolphin := func() (capture.Source, error) {
		if bridge == nil {
			return nil, fmt.Errorf("e2e: the Dolphin bridge is not launched yet")
		}
		return bridge.FrameSource(10)
	}

	sessions, err := runtime.New(runtime.Options{
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

	srv := api.New(bootstrap.BuildInfo{Version: "e2e-dolphin-live"}, time.Now(), disc,
		api.WithAuth(authSvc, store.NewAuthStore(st)),
		api.WithStore(st),
		api.WithJobs(js),
		api.WithEvents(hub),
		api.WithImport(as, adapters, runner),
		api.WithSessions(sessions),
		api.WithDevEvents(store.NewEventStore(st)),
		api.WithVoices(voiceSvc, runner),
	).Handler()
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
	// The edition is tied to the disc by its SHA-256 (the identity scheme the
	// UI and the bridge use: the image by hash and size, never by path). The
	// hash is streamed in-process and never logged (rule 5): it goes into the
	// API body and the bundle manifest (via the bridge's own computation).
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
	t.Logf("e2e: catalog ready: game=%s edition=%s profile=%s", gameID, editionID, profileID)

	// ── 2. import the script from the disc image through the API ──────────
	// The disc image is the user-supplied source (GAME_NARRATOR_GAME_IMAGE_PATH,
	// never copied or logged). The UI/API import path takes the message script
	// the disc carries: the BMG is extracted from the image and uploaded
	// through POST /profiles/{id}/import (the route the UI posts to), where the
	// windwaker adapter parses it and profileio writes the messages.
	discFile, err := gcdisc.Open(img)
	if err != nil {
		t.Fatalf("open disc image: %v", err)
	}
	bmgBytes, err := windwaker.ExtractBMG(discFile)
	_ = discFile.Close()
	if err != nil {
		t.Fatalf("extract BMG from disc image: %v", err)
	}
	// The script content's hash rides on the import job's result (the
	// product's own digest) — the test log carries the byte count only.
	t.Logf("e2e: BMG extracted from the disc image: %d bytes (hash recorded on the import job result, not logged)", len(bmgBytes))

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
		Stats    struct {
			Rows     int `json:"rows"`
			Imported int `json:"imported"`
			Skipped  int `json:"skipped"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(job.Result, &importReport); err != nil {
		t.Fatalf("import job result decode: %v", err)
	}
	if importReport.Inserted < 1000 {
		t.Fatalf("import inserted only %d messages; the real Wind Waker script is a large set (>=1000)", importReport.Inserted)
	}
	t.Logf("e2e: import done: job=%s adapter=%s format=%s inserted=%d updated=%d rows=%d skipped=%d",
		up.JobID, importReport.Adapter, importReport.Format, importReport.Inserted, importReport.Updated,
		importReport.Stats.Rows, importReport.Stats.Skipped)

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
	t.Logf("e2e: profile %s published with the story-subtitle region", profileID)

	// ── 5. the narrator voice: auditions → approve → register → preview ──
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
			"consent":     "original_synthetic_design (game-narrator e2e live dolphin run)",
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
	// One preview through the voices service: proves the registered voice
	// synthesises end-to-end before the session's own utterance does.
	rec = do(http.MethodPost, "/api/v1/voices/"+voiceID+"/preview",
		mustMarshal(t, map[string]any{"text": voices.SyntheticAuditionLines[0]}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("voice preview = %d: %s", rec.Code, rec.Body.String())
	}
	t.Logf("e2e: narrator voice %s: auditions=%d approved=%s registered=%s name=%s preview=%d bytes",
		voiceID, len(voiceDetail.Auditions), voiceDetail.State, reg.State, reg.RegisteredName, rec.Body.Len())
	// Remote-registration cleanup (the Base registry keeps gn_-named voices;
	// this one belongs to this run).
	if reg.RegisteredName != "" {
		voiceCloser = func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer ccancel()
			if err := registry.Remove(cctx, reg.RegisteredName); err != nil {
				t.Logf("e2e: voice cleanup: %v (voice %s left on the Base registry)", err, reg.RegisteredName)
			} else {
				t.Logf("e2e: voice cleanup: registered name %s removed", reg.RegisteredName)
			}
		}
	}
	t.Cleanup(voiceCloser)

	// ── 6. launch the real Dolphin bridge ─────────────────────────────────
	bridge, err = dolphin.New(dolphin.Options{
		DataDir:       filepath.Join(dataDir, "dolphin"),
		LaunchTimeout: 180 * time.Second,
		Log:           disc,
	})
	if err != nil {
		t.Fatalf("dolphin.New: %v", err)
	}
	launchCtx, launchCancel := context.WithCancel(context.Background())
	t.Cleanup(launchCancel)
	if err := bridge.Launch(launchCtx, dolphin.LaunchSpec{}); err != nil {
		bridge.Stop()
		t.Fatalf("dolphin launch: %v", err)
	}
	t.Cleanup(func() { bridge.Stop() })
	st2 := bridge.State()
	if st2.State != dolphin.StateRunning || st2.ImageSHA == "" {
		t.Fatalf("bridge state=%s after launch (want running with the image hash recorded)", st2.State)
	}
	t.Logf("e2e: dolphin running: display=%s pid=%d image recorded by the bridge (hash and size; the path is never logged)",
		st2.Display, st2.DolphinPID)

	// ── 7. the session: live Dolphin frames through the HTTP API ──────────
	code, out = doJSON(http.MethodPost, "/api/v1/sessions", map[string]any{
		"profile_id":   profileID,
		"frame_source": map[string]any{"kind": runtime.SourceKindDolphin},
		"mode":         "conservative",
		"dev_trace":    true,
	}, nil)
	if code != http.StatusCreated {
		t.Fatalf("session start = %d: %v (the runtime answers the dolphin kind 501 without %s=1 or the bridge seam)",
			code, out, dolphin.EnvDevDolphin)
	}
	sessionID, _ := out["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("session start response has no id: %v", out)
	}
	t.Logf("e2e: session %s started (profile %s, source dolphin, dev_trace on)", sessionID, profileID)

	// ── 8. the screen-driven drive to the opening legend narration ────────
	// The drive writes one evidence PNG per iteration into EvidenceDir and
	// drops them silently if the dir is missing (storeEvidence's os.Create),
	// so the bundle subdir is created before the run.
	if err := os.MkdirAll(filepath.Join(bundle, "drive"), 0o755); err != nil {
		t.Fatalf("create drive evidence dir: %v", err)
	}
	driveStart := time.Now()
	drv, derr := dolphin.RunDrive(ctx, bridge, dolphin.DriveOptions{
		Deadline:    10 * time.Minute,
		EvidenceDir: filepath.Join(bundle, "drive"),
	}, dolphin.WindWakerNewGameDrive())
	for _, tr := range drv.Transitions {
		next := "<end>"
		if i := tr.Index + 1; i < len(drv.Transitions) {
			next = string(drv.Transitions[i].Screen)
		}
		t.Logf("e2e: drive %02d: %s --(%s)--> %s", tr.Index, tr.Screen, tr.Action, next)
	}
	t.Logf("e2e: drive: ok=%v reason=%q elapsed=%s wall=%s success_screen=%s",
		drv.OK, drv.Reason, drv.Finished.Sub(drv.Started).Round(time.Second), time.Since(driveStart).Round(time.Second), drv.SuccessScreen)
	if derr != nil || !drv.OK {
		// The honest failure contract (ADR-0043): the full transition log is
		// above and the drive's PNGs are in the bundle; name the cause and
		// stop rather than working around it.
		t.Fatalf("drive to the first narrated text stopped: %s (last screen %s; evidence: %s)",
			drv.Reason, drv.LastKey, bundle)
	}

	// ── 9. the correlated chain, read back through the developer API ──────
	// The developer UI's polling route (web/src/api/sessions.ts): GET
	// /api/v1/dev/events?session_id=… The chain to find on one trace:
	// text.observed → message.candidates → decision.requested/resolved →
	// message.committed → speech.requested → speech.started → speech.ended.
	var (
		commit     devEnvelope
		chainReady bool
	)
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	wait := time.NewTimer(12 * time.Minute)
	defer wait.Stop()
	for {
		select {
		case <-wait.C:
			t.Fatalf("no committed-and-spoken chain within 12m (bundle %s carries the events so far)", bundle)
		case <-poll.C:
		}
		evs, err := dolphinDevEvents(t, srv, token, sessionID)
		if err != nil {
			t.Fatalf("dev events: %v", err)
		}
		writeEvidenceJSON(t, filepath.Join(bundle, "timeline.json"), evs)
		chainReady = findDolphinChain(t, evs, &commit)
		if chainReady {
			break
		}
	}
	if commit.TraceID == "" {
		t.Fatalf("chain found but the commit event carries no trace id: %s", commit.EventID)
	}
	t.Logf("e2e: correlated chain complete on trace %s (commit %s)", commit.TraceID, commit.EventID)

	// ── 10. stop the session and read its final counters ──────────────────
	rec = do(http.MethodPost, "/api/v1/sessions/"+sessionID+"/stop", []byte(`{}`), nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("session stop = %d: %s", rec.Code, rec.Body.String())
	}
	var final struct {
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
	t.Logf("e2e: session %s stopped: state=%s reason=%s frames_processed=%d ocr_calls=%d fallback_calls=%d rankings=%d",
		sessionID, final.State, final.Reason, final.Stats.FramesProcessed, final.Stats.OCRCalls,
		final.Stats.OCRFallbackSecondaryCalls, final.Stats.Rankings)

	// ── 11. assertions (docs/spec/12 "Live E2E") ──────────────────────────
	assertDolphinLive(t, &dolphinAsserts{
		bundle:         bundle,
		srv:            srv,
		token:          token,
		profileID:      profileID,
		session:        final,
		sessionID:      sessionID,
		commit:         commit,
		bridge:         bridge,
		evidenceEvents: evsAll(t, srv, token, sessionID),
		drive:          drv,
		voice:          voiceID,
		fileSinkDir:    speechOpts.FileSinkDir,
		devTraceDir:    filepath.Join(dataDir, "dev-trace", sessionID), // NewDevTraceRecorder(devTraceDir(AssetsRoot, id), …)
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
}

// speechCount mirrors runtime.SpeechCounters for the snapshot decode.
type speechCount struct {
	Requested int64 `json:"requested"`
	Ready     int64 `json:"ready"`
	Started   int64 `json:"started"`
	Cancelled int64 `json:"cancelled"`
	Ended     int64 `json:"ended"`
	CacheHits int64 `json:"cache_hits"`
}

// mustMarshal is the test-local JSON marshal that fails the test on the
// (impossible) error instead of returning a half body.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// previewDigest is the preview cache key's backend digest (ADR-0023), the
// same construction cmd/game-narrator uses.
func previewDigest(model string) string {
	sum := sha256.Sum256([]byte("voice-preview-v1|" + model))
	return hex.EncodeToString(sum[:])[:8]
}

// dolphinDevEvents pages the durable event log for one session (the same
// route and filters the developer UI's polling fallback uses).
func dolphinDevEvents(t *testing.T, srv http.Handler, token, sessionID string) ([]devEnvelope, error) {
	t.Helper()
	var out []devEnvelope
	since := int64(0)
	for page := 0; page < 50; page++ {
		rec := dolphinDo(t, srv, token, http.MethodGet,
			fmt.Sprintf("/api/v1/dev/events?session_id=%s&since_seq=%d&limit=1000", sessionID, since), nil, nil)
		if rec.Code != http.StatusOK {
			return nil, fmt.Errorf("dev events page = %d: %s", rec.Code, rec.Body.String())
		}
		var p devEventsPage
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			return nil, fmt.Errorf("dev events decode: %w", err)
		}
		out = append(out, p.Events...)
		if !p.Truncated {
			return out, nil
		}
		since = p.NextSeq
	}
	return out, fmt.Errorf("dev events: still truncated after 50 pages")
}

// evsAll is dolphinDevEvents with the test failure attached (assertion phase
// only; the poll loop above tolerates transient reads).
func evsAll(t *testing.T, srv http.Handler, token, sessionID string) []devEnvelope {
	evs, err := dolphinDevEvents(t, srv, token, sessionID)
	if err != nil {
		t.Fatalf("dev events: %v", err)
	}
	return evs
}

// findDolphinChain reports whether the event page carries a complete
// correlated chain: a speech-eligible commit of a real message whose
// occurrence's trace also shows OCR (text.observed with text), candidates,
// the Clef verdict (decision.resolved) and the speech events. The Clef
// REQUEST (decision.requested) rides no trace — the product joins it on
// observation_seq + generation (internal/runtime/decision.go) — so the poll
// loop waits for the verdict on the trace and the assertion phase proves the
// request's join. It points commit at the best commit found so far for the
// report.
func findDolphinChain(t *testing.T, evs []devEnvelope, commit *devEnvelope) bool {
	t.Helper()
	for _, e := range evs {
		if e.Name != "message.committed" {
			continue
		}
		var d struct {
			MessageID string `json:"message_id"`
			Eligible  bool   `json:"speech_eligible"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil || d.MessageID == "" || d.MessageID == "unknown" || !d.Eligible {
			continue
		}
		*commit = e
		trace := e.TraceID
		if trace == "" {
			return false
		}
		have := map[string]bool{}
		for _, f := range evs {
			if f.TraceID != trace {
				continue
			}
			have[f.Name] = true
		}
		return have["text.observed"] && have["message.candidates"] &&
			have["decision.resolved"] && have["speech.requested"] &&
			have["speech.started"] && have["speech.ended"]
	}
	return false
}

// dolphinAsserts carries the assertion phase's inputs.
type dolphinAsserts struct {
	bundle    string
	srv       http.Handler
	token     string
	profileID string
	sessionID string
	commit    devEnvelope
	session   struct {
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
	}
	evidenceEvents []devEnvelope
	drive          dolphin.DriveTrace
	voice          string
	fileSinkDir    string // the session's filesink root (<data>/audio)
	devTraceDir    string // the session's dev-trace asset store (<data>/dev-trace)
	models         map[string]string
	runID          string
	startAll       time.Time
	bridge         dolphin.Bridge
}

// assertDolphinLive is the docs/spec/12 live-E2E assertion set, in the
// task's lettering: (a) real frames + detected subtitle, (b) OCR matched an
// imported line (rank 1, CER), (c) a genuine Clef request, (d) TTS audio
// bytes + observable playback, (e) the correlation chain. Every failure
// names the bundle so the evidence is one path away.
func assertDolphinLive(t *testing.T, a *dolphinAsserts) {
	t.Helper()
	chain := dolphinChainEvidence(t, a)
	assertDolphinFrames(t, a, chain)      // (a) real frames + detected subtitle
	assertDolphinMatch(t, a, chain)       // (b) OCR matched an imported line
	assertDolphinClef(t, a, chain)        // (c) a genuine Clef request
	assertDolphinSpeech(t, a, chain)      // (d) TTS audio bytes + observable playback
	assertDolphinCorrelation(t, a, chain) // (e) the correlation chain
	dolphinManifest(t, a, chain)          // the evidence bundle's manifest.json
}
