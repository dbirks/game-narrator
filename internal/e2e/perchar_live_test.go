// Package e2e — the per-character live E2E (task per-character-live):
//
// Dolphin (real emulator, real disc image, the recorded savestate
// checkpoints) → live frame capture → OCR → candidate match against the
// script imported from the same disc image (the import attaches the BMG's
// per-line speaker keys, ADR 0070) → the curator's speaker-link identifies
// each scene's speaker key as a character → the session's speech resolves
// each committed line to THAT character's own designed voice — two
// characters, two distinct VoiceDesign designs, both registered on the
// real Base registry under gn_test_ names (the automation namespace,
// ADR 0022) and always removed — and the line's ASR QA read-back passes
// against the line (the speech_asr role, ADR 0061).
//
// The run covers both recorded checkpoints, one bridge launch each
// (--save_state boot, ADR 0043/0054): the opening legend (narrated lines,
// the narrator/legend speaker) and the first in-game NPC dialogue (the
// first gameplay dialogue's speaker). Each scene runs under its OWN
// published profile carrying the scene's measured region(s). The dialogue
// scene carries the box's two on-screen positions (low, measured, and
// high — the Wind Waker box tracks the speaker and sat HIGH through the
// whole opening conversation in the 2026-10-20 probe, where the single
// low region saw nothing); the two positions are never visible at once,
// unlike the box-vs-legend-band overlap that kept the first attempt to a
// single region per profile.
//
// The assertions prove: every committed line's speech events carry the
// voice of the character its speaker key was linked to; both characters'
// voices are actually heard (two distinct voice ids across the run); the
// two characters' WAVs differ; and every clip's ASR QA verdict is passed
// against its line. The headless session's dialogue-box advance is the
// test pressing A the way the fixture recordings did (their manifests'
// input logs).
//
// This is a live test (AGENTS.md test category (d)+(e): real model
// providers and the Dolphin emulator). It SKIPs as NOT RUN unless the
// prerequisites are present (image, emulator tools, fixtures + states,
// gateway, voice roles, the speech_asr role); the Clef/OCR-fallback roles
// are not skip gates — an unproven stage is a failure with evidence, not a
// silent skip (the spec's "never label a mocked workflow as real" rule).
//
// Log-safe per AGENTS.md rule 5: log lines carry counts, ids, hashes and
// timings — never dialogue text, speaker key values or tokens. The
// evidence bundle (artifacts/per-character-live/<run-id>/, gitignored)
// carries the raw payloads.
//
// Run (the shared gateway env provides the OCR VLM fallback, the
// `decision` role, the TTS roles and the `speech_asr` role):
//
//	GN_GATE=test-dolphin go test -p 1 -count=1 -timeout 45m -v \
//	  -run TestLivePerCharacterVoices ./internal/e2e/

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/games/adapter/windwaker"
	"github.com/dbirks/game-narrator/internal/gcdisc"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/asr"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
	"github.com/dbirks/game-narrator/internal/voices"
)

// The session's speech budget: a line whose ASR read-back fails regenerates
// once (ADR 0061); the per-session wall bound below stays generous on top.
const (
	pcSessionWait     = 12 * time.Minute // per session: enough spoken lines or this bound
	pcLaunchTimeout   = 180 * time.Second
	pcTotalBudget     = 40 * time.Minute
	pcNarratorRegionX = 0.12 // legend subtitle band, the fixture's measured region
	pcNarratorRegionY = 0.75
	pcNarratorRegionW = 0.76
	pcNarratorRegionH = 0.17
	pcBoxRegionX      = 0.05 // dialogue box, the fixture's measured region
	pcBoxRegionY      = 0.62
	pcBoxRegionW      = 0.80
	pcBoxRegionH      = 0.25

	// The dialogue box's HIGH position (the 2026-10-20 screen probe: the
	// Wind Waker box tracks the speaker's on-screen position, and through
	// the whole opening conversation it sits at y~.25-.60 — ABOVE the
	// measured low region, which saw 6822 frames and zero text and the
	// scene starved to its deadline). The profile carries BOTH positions
	// (the pipeline runs multi-region profiles: the legend fixture's own
	// recording used two); the box is in one position at a time and the
	// two regions do not overlap (high ends where low begins).
	pcBoxHighRegionX = 0.05
	pcBoxHighRegionY = 0.25
	pcBoxHighRegionW = 0.80
	pcBoxHighRegionH = 0.37
)

func TestLivePerCharacterVoices(t *testing.T) {
	// The live per-character e2e belongs to the test-dolphin gate
	// (ADR-0065): it boots the real emulator AND calls the real gateway.
	if os.Getenv(dolphin.EnvGate) != "test-dolphin" {
		t.Skipf("live per-character e2e outside the test-dolphin gate: run via make test-dolphin (it sets %s=test-dolphin)", dolphin.EnvGate)
	}
	img := os.Getenv(dolphin.EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set — point it at the user-supplied Wind Waker image (dev-only, the image is never logged)", dolphin.EnvGameImage)
	}
	if _, err := os.Stat(img); err != nil {
		t.Skipf("NOT RUN: disc image not found at the %s path (dev-only)", dolphin.EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo", "tesseract"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH (dev host dependencies: dolphin-emu, xorg-server-xvfb, ffmpeg, x11-utils, tesseract)", bin)
		}
	}
	if err := strayDolphinProcs(); err != nil {
		t.Skipf("NOT RUN: %v", err)
	}

	// The recorded checkpoints: fixture root + both fixtures + both
	// savestates, each verified against its manifest's SHA-256 claim.
	root, ok := pcFixtureRoot(t)
	if !ok {
		t.Skipf("NOT RUN: the fixtures are absent (external, gitignored; record them with the dolphin fixture gate, ADR 0054)")
	}
	fdDir, lgDir := filepath.Join(root, pcFixtureFirstDialogue), filepath.Join(root, pcFixtureLegendPress)
	if _, err := os.Stat(filepath.Join(lgDir, "profile.json")); err != nil {
		t.Skipf("NOT RUN: fixture %s is incomplete (%v)", pcFixtureLegendPress, err)
	}
	fdState := pcVerifyState(t, fdDir, pcStateFirstDialogue)
	lgState := pcVerifyState(t, lgDir, pcStateLegendStart)

	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	gatewayCfg := roles[providers.RoleAgentReasoning] // the shared gateway's URL/token live on every role
	if strings.TrimSpace(gatewayCfg.BaseURL) == "" || strings.TrimSpace(gatewayCfg.Token()) == "" {
		t.Skipf("NOT RUN: set %s and the token env named by %s (default %s) to run the live per-character e2e",
			providers.EnvGatewayURL, providers.EnvGatewayTokenEnv, providers.EnvGatewayToken)
	}
	designCfg, cloneCfg := roles[providers.RoleVoiceDesign], roles[providers.RoleVoiceClone]
	designEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceDesign)
	cloneEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceClone)
	if strings.TrimSpace(designCfg.Model) == "" || strings.TrimSpace(cloneCfg.Model) == "" {
		t.Skipf("NOT RUN: set %s (character voice design) and %s (Base synthesis) to run the live per-character e2e", designEnv, cloneEnv)
	}
	directURL := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"))
	if directURL == "" {
		directURL = strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	}
	if directURL == "" {
		t.Skipf("NOT RUN: set GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL (or GAME_NARRATOR_TTS_BASE_DIRECT_URL) to the Base voice registry")
	}
	// The ASR QA role: a skip gate here because the task's acceptance is the
	// per-clip ASR verdict — without the endpoint the QA evidence cannot
	// exist, and a QA-less run must not read as a per-character pass.
	asrCfg := roles[providers.RoleSpeechASR]
	if !asrCfg.Configured() {
		asrEnv, _, _, _ := providers.RoleEnvVars(providers.RoleSpeechASR)
		t.Skipf("NOT RUN: the speech_asr role is not configured (set %s; the per-clip ASR QA verdict is the acceptance evidence)", asrEnv)
	}

	// The dev-only Dolphin seam the runtime consults for kind:dolphin.
	t.Setenv(dolphin.EnvDevDolphin, "1")

	ctx, cancel := context.WithTimeout(context.Background(), pcTotalBudget)
	t.Cleanup(cancel)
	startAll := time.Now()

	runID := "perchar-" + time.Now().UTC().Format("20060102T150405Z")
	// The bundle goes under the REPO ROOT's gitignored artifacts/ (AGENTS.md
	// evidence bundle), not the package dir: go test runs in internal/e2e.
	bundle := filepath.Join(pcRepoRoot(t), "artifacts", percharEvidenceSub, runID)
	t.Logf("e2e: run %s; evidence bundle %s (class: real Dolphin + real providers; savestates verified by manifest sha)", runID, bundle)

	// The image's SHA-256 is the edition's source identity (the UI and the
	// bridge use the hash + size, never the path). It is streamed in-process
	// and never logged in full (rule 5) — the bundle manifest carries it.
	imageSHA := dolphinImageSHA(t, img)

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
	// GN_E2E_DEBUG_LOG names a file: the runtime and API logs land there so
	// a failed live run's server-side error is debuggable (the in-process
	// API's 500 body points at the server log, which is discarded by default).
	srvLog := disc
	if p := os.Getenv("GN_E2E_DEBUG_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			t.Cleanup(func() { _ = f.Close() })
			srvLog = slog.New(slog.NewTextHandler(f, nil))
			// The polling client's own page reads (seqs only) go to the same
			// file, tagged, so a route-vs-client stream gap is attributable.
			debugLog = log.New(f, "e2e-client: ", log.Ltime|log.Lmicroseconds)
		} else {
			t.Fatalf("open debug log %s: %v", p, err)
		}
	}
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
	dev, err := authSvc.CreateUser(ctx, "e2e-perchar-dev", e2ePassword, auth.RoleDeveloper, nil, "e2e-live")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	token, _, err := authSvc.CreateToken(ctx, nil, auth.CreateTokenParams{
		UserID: dev.ID, Name: "e2e-perchar", Role: auth.RoleDeveloper,
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
	speechCache := speech.New(as, store.NewSpeechCacheIndex(st), 8, speech.WithReferenceChecker(st))
	voiceSvc := &voices.Service{
		Store:         st,
		Assets:        as,
		Design:        designClient,
		Base:          cloneClient,
		Registry:      registry,
		Speech:        speechCache,
		InstanceID:    voices.DefaultInstanceID,
		BaseModelID:   cloneCfg.Model,
		BackendDigest: previewDigest(cloneCfg.Model),
	}
	// ASR QA (FR-10, ADR 0061): the speech_asr role runs the audition
	// read-back and the generated-line QA on cache insert; unconfigured it
	// degrades to not-run, but above the role is a skip gate.
	asrClient, err := asr.NewFromRole(asrCfg)
	if err != nil {
		t.Fatalf("speech_asr client: %v", err)
	}
	voiceSvc.ASR = asrClient
	voiceSvc.QA = speech.NewQAEngine(speechCache, asrClient)
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
	var voiceCloser func() // the gn_test_ registrations' remote cleanup
	voiceCloser = func() {}

	speechOpts := &runtime.SpeechOptions{
		Voices:      voiceSvc,
		Assets:      as,
		Sink:        runtime.SinkFile,
		FileSinkDir: filepath.Join(dataDir, "audio"),
	}

	// The VLM OCR fallback (ADR-0040): wired when the role is configured;
	// nil otherwise the session runs the local engine only (documented).
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

	// The Clef (System One) adjudication wiring (ADR-0050): not a skip gate
	// — when unconfigured the session runs local and the decision assertion
	// must fail loudly rather than fake the stage.
	var decisionOpts *runtime.DecisionOptions
	if cfg := roles[providers.RoleDecision]; cfg.Configured() {
		roleReg := providers.NewRegistry(roles)
		chatRouter := router.New(roleReg, router.Options{})
		decisionOpts = &runtime.DecisionOptions{
			Router:  chatRouter,
			Role:    cfg,
			Timeout: dolphinClefTimeout,
			// Production margin (cmd/game-narrator/main.go runs Margin 0): a
			// confident local match commits on the local evidence and skips
			// Clef; only an abstained or fuzzy-below-bar ranking consults it.
			// The per-character run asserts voices, not the decision stage, so
			// the multi-box lines are read and committed without holding every
			// one for a Clef round trip. (dolphin_live_test.go keeps
			// dolphinClefMargin on purpose: it asserts a Clef request on the
			// opening line.)
			Margin: 0,
		}
		t.Logf("e2e: Clef decision role configured (model %s, protocol %s)", cfg.Model, cfg.Protocol)
	} else {
		decEnv, _, _, _ := providers.RoleEnvVars(providers.RoleDecision)
		t.Logf("e2e: decision role NOT CONFIGURED (set %s); the session runs local adjudication (this run's acceptance is per-character voices, not the decision stage)", decEnv)
	}

	// The dev-only Dolphin frame source seam (one bridge at a time; the
	// session's frame source is the running bridge's live X11 capture).
	var bridge dolphin.Bridge
	devDolphin := func() (capture.Source, error) {
		if bridge == nil {
			return nil, fmt.Errorf("e2e: the Dolphin bridge is not launched yet")
		}
		src, err := bridge.FrameSource(10)
		if err != nil {
			t.Logf("e2e: dev-dolphin frame source failed: %v (bridge state: %+v)", err, bridge.State())
		}
		return src, err
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
		Log:         srvLog,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}

	srv := api.New(bootstrap.BuildInfo{Version: "e2e-perchar-live"}, time.Now(), srvLog,
		api.WithAuth(authSvc, store.NewAuthStore(st)),
		api.WithStore(st),
		api.WithJobs(js),
		api.WithEvents(hub),
		api.WithImport(as, adapters, runner),
		api.WithSessions(sessions),
		api.WithDevEvents(store.NewEventStore(st)),
		api.WithVoices(voiceSvc, runner),
	).Handler()

	// The disc's script, extracted once: both scene profiles import the SAME
	// bytes (one sidecar, two profiles), so their speaker keys come from the
	// same parse.
	discFile, err := gcdisc.Open(img)
	if err != nil {
		t.Fatalf("open disc image: %v", err)
	}
	bmgBytes, err := windwaker.ExtractBMG(discFile)
	_ = discFile.Close()
	if err != nil {
		t.Fatalf("extract BMG from disc image: %v", err)
	}

	// ── 1. the two scene profiles (game → edition → draft → import →
	// character → speaker link → single region → publish → voice) ────────
	// One published profile per scene, each carrying only that scene's
	// measured region. Both profiles import the SAME script from the disc
	// image through the API (the import attaches the BMG speaker keys,
	// ADR 0070), so the two scenes' speaker keys come from the same
	// sidecar and are distinct. registeredNames is late-bound into the
	// cleanup below: whatever registered by test end gets removed, even if
	// a later scene fails after an earlier one already registered.
	var registeredNames []string
	setupScene := func(spec pcSceneSpec) pcSceneOutcome {
		var o pcSceneOutcome
		o.spec = spec

		code, out := pcDoJSON(t, srv, token, http.MethodPost, "/api/v1/games", map[string]any{
			"slug": "wind-waker-" + spec.label, "title": "The Legend of Zelda: The Wind Waker",
			"platform": "gamecube", "user_tags": json.RawMessage(`[]`),
		}, map[string]string{"Idempotency-Key": runID + "-game-" + spec.label})
		if code != http.StatusCreated {
			t.Fatalf("%s: create game = %d: %v", spec.label, code, out)
		}
		o.gameID, _ = out["id"].(string)
		code, out = pcDoJSON(t, srv, token, http.MethodPost, "/api/v1/games/"+o.gameID+"/editions", map[string]any{
			"region": "ntsc-u", "language": "en", "revision": "1", "source_identity": imageSHA,
		}, map[string]string{"Idempotency-Key": runID + "-edition-" + spec.label})
		if code != http.StatusCreated {
			t.Fatalf("%s: create edition = %d: %v", spec.label, code, out)
		}
		o.editionID, _ = out["id"].(string)
		code, out = pcDoJSON(t, srv, token, http.MethodPost, "/api/v1/editions/"+o.editionID+"/profiles", nil,
			map[string]string{"Idempotency-Key": runID + "-profile-" + spec.label})
		if code != http.StatusCreated {
			t.Fatalf("%s: create draft profile = %d: %v", spec.label, code, out)
		}
		o.profileID, _ = out["id"].(string)
		t.Logf("e2e: %s: catalog ready: game=%s edition=%s profile=%s", spec.label, o.gameID, o.editionID, o.profileID)

		// The script's speaker sidecar, parsed in-process from the same BMG
		// bytes the import uploads (counts and key ids only).
		parsed := pcParseBMG(t, ctx, bmgBytes, o.profileID)

		// Import the script from the disc image through the API.
		rec := dolphinDoMultipart(t, srv, token, "/api/v1/profiles/"+o.profileID+"/import",
			"windwaker.bmg", bmgBytes, map[string]string{
				"format": "bmg", "rights": "user_supplied", "language": "en",
			}, map[string]string{"Idempotency-Key": runID + "-import-" + spec.label})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: import upload = %d: %s", spec.label, rec.Code, rec.Body.String())
		}
		var up struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil || up.JobID == "" {
			t.Fatalf("%s: import upload response: %v: %s", spec.label, err, rec.Body.String())
		}
		job := dolphinWaitJob(t, srv, token, up.JobID, 5*time.Minute)
		if job.State != "succeeded" {
			t.Fatalf("%s: import job %s ended %s: %s", spec.label, up.JobID, job.State, string(job.Result))
		}
		var importReport struct {
			Inserted int `json:"messages_inserted"`
		}
		if err := json.Unmarshal(job.Result, &importReport); err != nil {
			t.Fatalf("%s: import job result decode: %v", spec.label, err)
		}
		if importReport.Inserted < 1000 {
			t.Fatalf("%s: import inserted only %d messages; the real Wind Waker script is a large set (>=1000)", spec.label, importReport.Inserted)
		}
		t.Logf("e2e: %s: import done: job=%s inserted=%d (speaker keys attached per ADR 0070)", spec.label, up.JobID, importReport.Inserted)

		// The scene's speaker key, from the fixture's matched lines mapped
		// onto the parsed sidecar.
		key, matched, distinct := pcDominantKey(t, spec.fixtureDir, parsed)
		o.key, o.keyMatched = key, matched
		t.Logf("e2e: %s: speaker key %s (dominant of %d/%d matched lines, %d distinct keys)", spec.label, maskPCKey(key), matched, distinct, distinct)

		// The target character (the product's roster write, as the agent's
		// stage_character writes it: a proposed row the curator then links).
		char := games.Character{
			ID: games.NewID(), ProfileID: o.profileID,
			Name: spec.charName, Role: spec.charRole,
			Status:    games.CharacterProposed,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		if err := st.PutCharacters(ctx, o.profileID, []games.Character{char}); err != nil {
			t.Fatalf("%s: put character: %v", spec.label, err)
		}
		o.charID = char.ID

		// The curator's speaker link (draft-only): re-points the key's lines
		// at the character.
		o.repointed = pcLinkSpeaker(t, srv, token, o.profileID, o.key, char.ID)
		t.Logf("e2e: %s: speaker link recorded: %d lines re-pointed (draft revision)", spec.label, o.repointed)

		// The scene's measured region(s), through the regions API: the
		// primary region plus any extra textbox positions (the dialogue box
		// moves with the speaker, so its profile carries both of its
		// on-screen positions).
		regionList := []map[string]any{{
			"label": spec.regionLabel, "region_type": spec.regionType,
			"x": spec.x, "y": spec.y, "w": spec.w, "h": spec.h,
		}}
		for _, r := range spec.extraRegions {
			regionList = append(regionList, map[string]any{
				"label": r.label, "region_type": r.regionType,
				"x": r.x, "y": r.y, "w": r.w, "h": r.h,
			})
		}
		regionBody := map[string]any{"regions": regionList}
		etag := pcProfileETag(t, srv, token, o.profileID)
		rec = pcDo(t, srv, token, http.MethodPut, "/api/v1/profiles/"+o.profileID+"/regions",
			mustMarshal(t, regionBody), map[string]string{"If-Match": etag})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: put region = %d: %s", spec.label, rec.Code, rec.Body.String())
		}

		// Draft → review → publish (only the published revision runs).
		etag = pcProfileETag(t, srv, token, o.profileID)
		rec = pcDo(t, srv, token, http.MethodPost, "/api/v1/profiles/"+o.profileID+"/status",
			[]byte(`{"to":"review"}`), map[string]string{"If-Match": etag})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: set status review = %d: %s", spec.label, rec.Code, rec.Body.String())
		}
		etag = pcProfileETag(t, srv, token, o.profileID)
		rec = pcDo(t, srv, token, http.MethodPost, "/api/v1/profiles/"+o.profileID+"/publish",
			[]byte(`{}`), map[string]string{"If-Match": etag})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: publish profile = %d: %s", spec.label, rec.Code, rec.Body.String())
		}
		t.Logf("e2e: %s: profile %s published (region %s + %d extra, speaker key %s linked)", spec.label, o.profileID, spec.regionLabel, len(spec.extraRegions), maskPCKey(o.key))

		// The character's designed voice (VoiceDesign → QA → approve →
		// register under a gn_test_ name, removed on cleanup). Registered
		// names are tracked incrementally so a later scene's failure still
		// cleans up this voice. The narrator scene designs under "narrator"
		// (its unattributed lines resolve there); the others under the
		// scene's own character.
		voiceChar := char.ID
		if spec.voiceCharacter != "" {
			voiceChar = spec.voiceCharacter
		}
		o.voice = pcDesignVoice(t, ctx, srv, token, st, as, registry, voiceChar,
			spec.brief, designCfg.Model, cloneCfg.Model)
		registeredNames = append(registeredNames, o.voice.Registered)
		return o
	}

	npcScene := setupScene(pcSceneSpec{
		label: "first-dialogue", fixtureDir: fdDir, state: fdState,
		regionLabel: "dialogue box", regionType: "textbox",
		x: pcBoxRegionX, y: pcBoxRegionY, w: pcBoxRegionW, h: pcBoxRegionH,
		extraRegions: []pcRegionSpec{{
			label: "dialogue box high", regionType: "textbox",
			x: pcBoxHighRegionX, y: pcBoxHighRegionY, w: pcBoxHighRegionW, h: pcBoxHighRegionH,
		}},
		minEnded: 2, charName: "e2e first-dialogue speaker", charRole: "npc",
		brief: "A young woman's bright, curious voice — the sea-raised heroine of the opening island scene. Lively, a little mischievous, clear enunciation.",
	})
	legendScene := setupScene(pcSceneSpec{
		label: "legend", fixtureDir: lgDir, state: lgState,
		regionLabel: "legend subtitle", regionType: "subtitle",
		x: pcNarratorRegionX, y: pcNarratorRegionY, w: pcNarratorRegionW, h: pcNarratorRegionH,
		minEnded: 3, charName: "e2e legend narrator", charRole: "narrator",
		voiceCharacter: "narrator",
		brief:          "An older storyteller's warm, unhurried voice — the legend narrator of an old kingdom's tale. Grave but kind, slow cadence.",
	})

	// The two scenes' speaker keys must be DISTINCT: one shared speaker
	// could not prove two characters speaking in two voices.
	if npcScene.key == legendScene.key {
		t.Fatalf("the first-dialogue fixture and the legend fixture share one speaker key (%s); one shared speaker cannot prove two distinct voices — re-check the checkpoints",
			maskPCKey(npcScene.key))
	}
	t.Logf("e2e: scene speakers distinct: first-dialogue %s (matched %d lines, linked %d), legend %s (matched %d lines, linked %d)",
		maskPCKey(npcScene.key), npcScene.keyMatched, npcScene.repointed,
		maskPCKey(legendScene.key), legendScene.keyMatched, legendScene.repointed)

	// The registered names are automation voices: the cleanup removes them
	// from the real Base registry (ADR 0022: live tests touch only
	// gn_test_ names, and always remove them).
	voiceCloser = func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		for _, name := range registeredNames {
			if name == "" {
				continue
			}
			if err := registry.Remove(cctx, name); err != nil {
				t.Logf("e2e: voice cleanup: %s: %v (voice left on the Base registry)", name, err)
			} else {
				t.Logf("e2e: voice cleanup: %s removed from the Base registry", name)
			}
		}
	}
	t.Cleanup(func() { voiceCloser() })

	// The expected voice per character (the session's resolveVoice resolves
	// each line's speaker to exactly this profile id).
	expectedVoice := map[string]string{
		npcScene.charID:    npcScene.voice.VoiceID,
		legendScene.charID: legendScene.voice.VoiceID,
	}

	// ── 2. the two live sessions (one bridge at a time) ──────────────────
	// Each session boots its own real Dolphin bridge from its scene's
	// savestate and runs until enough of its checkpoint's lines are fully
	// played. The sessions run SEQUENTIALLY: the no-parallel-Dolphins rule.
	fileSinkDir := filepath.Join(dataDir, "audio")

	// pcSessionEvidence is one session's captured evidence (no game text).
	type pcSessionEvidence struct {
		label        string
		profileID    string
		sessionID    string
		charID       string // the scene's character id (the WAV maps key by scene, not by the line's speaker: narrator-fallback lines carry a speaker key, not the character id)
		sceneVoiceID string // the voice this scene's lines carry (its character's voice, or the narrator voice for the narrator scene)
		snapshot     map[string]any
		events       []devEnvelope
		spoken       []pcSpokenLine
	}
	var evidence []pcSessionEvidence

	runSession := func(sc pcSceneOutcome) pcSessionEvidence {
		label, state := sc.spec.label, sc.spec.state
		regionLabel, minEnded := sc.spec.regionLabel, sc.spec.minEnded
		profileID := sc.profileID
		launchCtx, launchCancel := context.WithTimeout(context.Background(), pcLaunchTimeout)
		defer launchCancel()
		bridge = pcLaunchBridge(t, launchCtx, dataDir, label, state) // the shared var: the runtime's devDolphin seam reads it
		defer bridge.Stop()

		sessionID := pcStartSession(t, srv, token, profileID)

		// The advance driver: the fixture recordings pressed A per line
		// (their manifests' input logs: 4 presses for the first-dialogue
		// scene, 23 for the legend) — the headless session has no player, so
		// the test presses A the same way the proven drive tables do: one
		// hold per newly fully-played line, plus one stall-press when the
		// scene stops making progress (an OCR-unmatched line would otherwise
		// stall the scene forever).
		//
		// Pacing (learned from the 2026-10-19/2026-10-20 runs): a press may
		// only land when the box text has been STABLE for pcBoxStable (a
		// transitioning box's text churns through empty/partial readings) and
		// no real box commit (eligible or abstained) has arrived for
		// pcAdvanceStall — a real commit means the box moved, so the stall
		// clock resets, and a press into a just-committed line would fence
		// its own pipeline (TTS + QA outlast a blind press cadence). A press
		// into an empty box is never made: the box is gone (the dialogue
		// ended or the scene moved on) and there is nothing to advance.
		//
		// Progress and text stability are scoped to the DIALOGUE BOX region
		// (learned from the region that commits real messages): the profile
		// carries the box in BOTH positions (plus the narrator band in the
		// legend scene), and the non-box region's OCR noise (1-3 char
		// fragments off the animated water) commits constantly. Counting
		// that noise as progress starves the stall guard — the 2026-10-20
		// run: the box stalled on a line the model vetoed (safe_to_speak
		// false → COMMITTED_ABSTAINED, never spoken), 48 noise commits kept
		// resetting the 45 s stall clock and 1366 noise readings kept
		// resetting the 10 s stability check, so the stall-press never fired
		// and the session died at its 12-min deadline with one fully-played
		// line.
		var (
			droveEnded   int
			lastProgress = time.Now()
			committed    = map[string]bool{} // eligible committed occurrences
			terminal     = map[string]bool{} // occurrences with a terminal speech event
			endedOcc     = map[string]bool{} // occurrences fully played (speech.ended, result "ended")
			boxRegion    string              // the region that commits real lines (learned)
			lastText     string              // the newest observed box text (stable-text check)
			textChanged  = time.Now()        // when the box text last changed
		)
		advance := func(evs []devEnvelope) {
			for _, e := range evs {
				var d struct {
					Occurrence string `json:"dialogue_occurrence_id"`
					MessageID  string `json:"message_id"`
					Result     string `json:"result"`
					Eligible   bool   `json:"speech_eligible"`
					Text       string `json:"text"`
					Region     string `json:"region_id"`
				}
				if len(e.Data) == 0 {
					continue
				}
				if err := json.Unmarshal(e.Data, &d); err != nil {
					continue
				}
				switch e.Name {
				case "text.observed":
					// The box region's readings drive the stability check once
					// the box is learned (empty readings included: they clear
					// the box and arm the empty-box guard). Before it is,
					// only line-length readings count — the non-box region's
					// 1-3 char noise fragments must not churn the clock during
					// the opening guard.
					count := false
					if boxRegion != "" {
						count = d.Region == boxRegion
					} else if utf8.RuneCountInString(d.Text) >= 12 {
						count = true
					}
					if count && d.Text != lastText {
						lastText, textChanged = d.Text, time.Now()
					}
				case "message.committed":
					if d.Occurrence == "" {
						continue
					}
					// A REAL commit (a matched message, eligible or abstained)
					// is box progress: the box moved to a new line, so the
					// stall clock resets — a press into a just-committed line
					// would fence its pipeline. A non-dialogue noise commit
					// (message_id "unknown") is NOT box progress: the box has
					// not moved, and counting it lets the non-box region's
					// constant fragments starve the stall-press (the 2026-10-20
					// failure mode).
					if d.MessageID != "" && d.MessageID != "unknown" {
						boxRegion = d.Region
						lastProgress = time.Now()
					}
					if d.Eligible {
						committed[d.Occurrence] = true
					}
				case "speech.ended":
					if d.Occurrence == "" {
						continue
					}
					terminal[d.Occurrence] = true
					if d.Result == "ended" {
						endedOcc[d.Occurrence] = true
					}
				case "speech.cancelled":
					if d.Occurrence == "" {
						continue
					}
					terminal[d.Occurrence] = true
				}
			}
			// Press for every fully-played line not pressed for yet: the press
			// lands AFTER the line's speech.ended (and its commit), so the next
			// commit cannot fence this line's playback (the box is held until A).
			if len(endedOcc) > droveEnded {
				for i := droveEnded; i < len(endedOcc); i++ {
					pcPressAdvance(t, bridge, label)
				}
				droveEnded = len(endedOcc)
				lastProgress = time.Now()
				textChanged = time.Now() // the press starts the next box's transition
				return
			}
			inFlight := 0
			for occ := range committed {
				if !terminal[occ] {
					inFlight++
				}
			}
			// The stall guard: the box must be NON-EMPTY (there is something to
			// advance), its text must have been STABLE (not mid-transition),
			// no eligible line may be in flight, and no commit may have landed
			// for the gap. The FIRST press carries a long guard: the scene's
			// opening line is never skipped while its OCR/Clef/TTS cycle is
			// plausibly still in flight.
			stallGap := pcAdvanceStall
			if droveEnded == 0 {
				stallGap = pcOpeningStall
			}
			if inFlight == 0 && lastText != "" &&
				time.Since(lastProgress) > stallGap && time.Since(textChanged) > pcBoxStable {
				pcPressAdvance(t, bridge, label) // stall: the stable box is not progressing; move it
				lastProgress = time.Now()
				textChanged = time.Now()
			}
		}

		t.Logf("e2e: %s session %s running (region %s)", label, sessionID, regionLabel)
		evs, ok := pcWaitSpokenLines(t, srv, token, sessionID, fileSinkDir, minEnded, pcSessionWait, advance)
		// Reconcile the polled stream against the durable store read
		// DIRECTLY (the same rows, no route in between): if the developer
		// route's pages dropped rows under live load, the store read is the
		// authority — merge the rows the polling missed (dedup on seq) so
		// the assertions run on the store's truth, and record how many the
		// route did not return (the developer UI polls this same route, so a
		// non-zero gap is inspector-visible, not just a test artifact).
		if direct, err := store.NewEventStore(st).After(ctx, 0, events.Filter{SessionID: sessionID}, 10000); err == nil {
			have := map[int64]bool{}
			for _, e := range evs {
				have[e.Seq] = true
			}
			missing := 0
			for _, e := range direct {
				if !have[e.Seq] {
					evs = append(evs, devEnvelope{
						Seq: e.Seq, EventID: e.EventID, SessionID: e.SessionID, TraceID: e.TraceID,
						CausationID: e.CausationID, Name: e.Name, OccurredAt: e.OccurredAt.Format("2006-01-02T15:04:05.000Z"), Data: e.Data,
					})
					missing++
				}
			}
			if missing > 0 {
				t.Logf("e2e: %s: the polled dev-events stream returned %d of the store's %d session rows; %d merged in from the direct store read (the route gap is logged, the assertions run on the store)",
					label, len(evs)-missing, len(direct), missing)
				if debugLog != nil {
					debugLog.Printf("e2e %s: reconciled %d store rows into the %d-row polled stream (route gap %d)", label, missing, len(evs)-missing, missing)
				}
			}
		}
		spoken := pcCollectSpokenLines(evs, fileSinkDir, sessionID)
		snapshot := pcStopSession(t, srv, token, sessionID) // stop once the evidence is collected
		ev := pcSessionEvidence{label: label, profileID: profileID, sessionID: sessionID, charID: sc.charID, sceneVoiceID: sc.voice.VoiceID, snapshot: snapshot, events: evs, spoken: spoken}
		if !ok {
			// A deadline means the scene stalled (an unmatched box, a slow
			// provider, a stuck drive): record this session's PARTIAL stream
			// in the bundle NOW so the commit-spacing-vs-TTS-latency timeline
			// survives the failure (the store lives in t.TempDir and dies
			// with the test).
			dir := filepath.Join(bundle, fmt.Sprintf("session-%d-%s", len(evidence), label))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Logf("e2e: %s: partial bundle mkdir %s: %v", label, dir, err)
			} else {
				writeJSONFile(t, filepath.Join(dir, "dev_events.json"), evs)
				writeJSONFile(t, filepath.Join(dir, "spoken.json"), spoken)
				writeJSONFile(t, filepath.Join(dir, "snapshot.json"), snapshot)
				// The global store rows (no session filter): the names and
				// session ids of every persisted event this process held, so
				// a row the session-filtered page drops is identifiable (which
				// session_id it carried, or whether it is absent at all).
				if g, err := store.NewEventStore(st).After(ctx, 0, events.Filter{}, 1000); err == nil {
					writeJSONFile(t, filepath.Join(dir, "global_events.json"), g)
				}
				t.Logf("e2e: %s: partial evidence (the pre-failure stream) written: %s", label, dir)
			}
			t.Fatalf("%s: the session stalled before %d fully-played lines (deadline %s; the partial stream is in %s — check the commit spacing vs TTS/QA latency there)", label, minEnded, pcSessionWait, dir)
		}
		return ev
	}
	evidence = append(evidence, runSession(npcScene))
	evidence = append(evidence, runSession(legendScene))
	// ── 11. the assertions ─────────────────────────────────────────────────
	// (a) every committed line's speech used the voice of the character its
	// speaker key was linked to: the line's message row carries the linked
	// speaker (the import + link did the attribution), the session's
	// resolveVoice must have resolved THAT character's voice profile, and
	// the speech events that carry a voice id (speech.ready's chosen voice,
	// echoed onto the scheduler's terminal events by the wiring's
	// correlation) must carry it. A line that terminalled before any
	// playback ("skipped": backpressure or no resolvable voice; "failed":
	// the synthesis itself failed) carries no voice id on its events and is
	// checked to be exactly that — never a silently substituted voice.
	// (b) both characters' voices were used (two distinct voice ids across
	// the run's voice-bearing lines: the chosen voice of every line that
	// reached a voice, plus the fully-played ones; the ended lines are the
	// audio proof and are asserted separately in (c)+(d)).
	// (c) the two characters' WAVs differ.
	// (d) every fully-played clip's ASR QA verdict is passed against its
	// line (the speech_asr role, ADR 0061).
	var (
		linesChecked   int
		linesEnded     int
		linesSkipped   int
		qaPassed       int
		voiceUsage     = map[string]int{}    // voice id -> line count that CARRIED that voice (chosen or played)
		endedByVoice   = map[string]int{}    // voice id -> fully-played line count
		wavByChar      = map[string]string{} // character -> one ended line's WAV sha
		wavCountByChar = map[string]int{}
	)
	endedByChar := map[string]string{} // character -> one ended line's WAV path (for the differing-WAV check)

	for _, ev := range evidence {
		for _, l := range ev.spoken {
			msg, _, err := st.GetMessage(ctx, ev.profileID, l.MessageID)
			if err != nil {
				t.Fatalf("message %s (session %s): %v", l.MessageID, ev.sessionID, err)
			}
			charID := msg.SpeakerID
			want, ok := expectedVoice[charID]
			if !ok {
				// A speaker not linked to a roster character: the runtime
				// resolves these to the session's narrator voice (internal/
				// runtime/speech.go's resolveVoice falls back to "narrator").
				// The line must still carry THIS session's scene voice — never
				// another character's — so the cross-substitution check holds
				// for narrator-fallback lines too.
				want = ev.sceneVoiceID
			}
			if l.VoiceID == "" {
				// A line that terminalled before any playback can carry no
				// voice id on its events: "skipped" (backpressure, or no
				// resolvable voice — the reason says which) and "failed"
				// (the synthesis itself failed; the wiring's terminal payload
				// carries the result, not the voice). Both are checked to be
				// exactly that — never a silently substituted voice. A line
				// that chose a voice (ready/started/cancelled/ended) must
				// carry it.
				if l.Result != "skipped" && l.Result != "failed" {
					t.Fatalf("line %s (session %s) terminalled %q (reason %q) without any voice id on its speech events; every line that reached a voice choice must carry it", l.MessageID, ev.sessionID, l.Result, l.Reason)
				}
				linesSkipped++ // terminal without a voice event: no voice was evidenced, none may be implied
				continue
			}
			if l.VoiceID != want {
				t.Fatalf("line %s (session %s) spoke (result %q) with voice %s; speaker %s resolves to voice %s — the session must resolve the line's own character/narrator voice, never another character's",
					l.MessageID, ev.sessionID, l.Result, l.VoiceID, charID, want)
			}
			linesChecked++
			voiceUsage[l.VoiceID]++

			if l.Result != "ended" {
				// Terminal but not fully played (interrupted, cancelled, failed):
				// the voice choice is still asserted above; the WAV + QA
				// assertions need a complete clip.
				continue
			}
			linesEnded++
			endedByVoice[l.VoiceID]++

			// The line's WAV from the file sink (the utterance the run
			// played, identified by utterance id + generation; a QA retry
			// re-synthesises into a new sequence of the same utterance).
			wavDir := filepath.Join(fileSinkDir, ev.sessionID)
			pattern := fmt.Sprintf("gen%d-%s-*.wav", l.Generation, l.UtteranceID)
			matches, err := filepath.Glob(filepath.Join(wavDir, pattern))
			if err != nil {
				t.Fatalf("glob %s: %v", pattern, err)
			}
			if len(matches) == 0 {
				t.Fatalf("line %s (session %s) played to the end but its WAV is absent from the file sink (%s/%s; the sink must keep the played audio)",
					l.MessageID, ev.sessionID, wavDir, pattern)
			}
			wav := matches[len(matches)-1]
			if _, seen := wavByChar[ev.charID]; !seen {
				endedByChar[ev.charID] = wav
				wavByChar[ev.charID] = sha256FileLabel(t, wav)
			}
			wavCountByChar[ev.charID]++

			// (d) the clip's ASR QA verdict against the line.
			key := speech.CacheKey(l.CacheKey)
			entry, found := store.NewSpeechCacheIndex(st).Lookup(key)
			if !found {
				t.Fatalf("line %s (session %s): no speech-cache entry for its played key (the cache must hold the played clip)", l.MessageID, ev.sessionID)
			}
			if entry.QA != speech.QAPassed {
				t.Fatalf("line %s (session %s): the played clip's ASR QA verdict is %s (model %s, wer=%.3f cer=%.3f); the acceptance is a passed verdict against the line (ADR 0061)",
					l.MessageID, ev.sessionID, entry.QA, entry.QAModel, entry.QAWER, entry.QACER)
			}
			qaPassed++
		}
	}

	// (b) both characters' voices were used: two distinct voice ids across
	// the run, each character's own among them (the minEnded pacing makes
	// the ended lines the primary proof; the voice-bearing cancelled lines
	// are the fallback proof a fenced or session-stopped line still carries
	// its chosen voice).
	if len(voiceUsage) < 2 {
		t.Fatalf("only %d distinct voice id(s) across the run's %d voice-bearing lines (usage: %v); the run needs BOTH characters' voices (the task's distinct-voice proof; the legend+NPC fallback sessions were already run)",
			len(voiceUsage), linesChecked, voiceUsage)
	}
	if voiceUsage[npcScene.voice.VoiceID] < 1 || voiceUsage[legendScene.voice.VoiceID] < 1 {
		t.Fatalf("voice usage %v: each character's own voice must be used at least once (npc=%s, legend=%s)", voiceUsage, npcScene.voice.VoiceID, legendScene.voice.VoiceID)
	}
	t.Logf("e2e: assertions: %d lines carried the linked character's voice (%d fully-played, %d skipped without a voice), %d ASR QA passed; usage by voice: npc=%d (ended %d) legend=%d (ended %d)",
		linesChecked, linesEnded, linesSkipped, qaPassed,
		voiceUsage[npcScene.voice.VoiceID], endedByVoice[npcScene.voice.VoiceID],
		voiceUsage[legendScene.voice.VoiceID], endedByVoice[legendScene.voice.VoiceID])

	// (c) the two characters' WAVs differ (distinct designed voices must
	// produce distinct audio, not just distinct ids).
	if endedByChar[npcScene.charID] == "" || endedByChar[legendScene.charID] == "" {
		t.Fatalf("no fully-played line for one or both characters (npc wav=%v legend wav=%v); the WAV-difference proof needs a complete clip per character",
			endedByChar[npcScene.charID], endedByChar[legendScene.charID])
	}
	shaNPC, shaLegend := wavByChar[npcScene.charID], wavByChar[legendScene.charID]
	if shaNPC == shaLegend {
		t.Fatalf("the two characters' WAVs are byte-identical (%s); the distinct designs must produce distinct audio", shaNPC)
	}
	t.Logf("e2e: wav check: npc clip sha256=%s… legend clip sha256=%s… (differ; %d fully-played clips: npc=%d legend=%d)",
		shaNPC[:12], shaLegend[:12], linesEnded, wavCountByChar[npcScene.charID], wavCountByChar[legendScene.charID])

	// ── 12. the evidence bundle (artifacts/ is gitignored; private) ───────
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", bundle, err)
	}
	manifest := map[string]any{
		"run_id":                  runID,
		"timestamp_utc":           time.Now().UTC().Format(time.RFC3339),
		"gate":                    "test-dolphin",
		"test_class":              "live: real Dolphin emulator + real model providers (ADR-0065 category d+e); savestates recorded, verified by fixture manifest sha",
		"os":                      osLabel(),
		"go_version":              goVersionLabel(),
		"disc_image_sha256":       imageSHA, // the hash only (rule 5: the path is never recorded)
		"edition_source_identity": imageSHA,
		"savestates": map[string]any{
			pcFixtureFirstDialogue: map[string]string{"file": filepath.Base(fdState), "sha256": stateSHA(t, fdState)},
			pcFixtureLegendPress:   map[string]string{"file": filepath.Base(lgState), "sha256": stateSHA(t, lgState)},
		},
		"roles": map[string]string{
			"voice_design": designCfg.Model,
			"voice_clone":  cloneCfg.Model,
			"speech_asr":   asrCfg.Model,
			"decision":     decisionModelLabel(decisionOpts),
			"ocr_fallback": ocrFallbackModel,
		},
		"characters": map[string]string{
			"first_dialogue_speaker": npcScene.charID,
			"legend_narrator":        legendScene.charID,
		},
		"speaker_keys": map[string]string{
			"first_dialogue": npcScene.key,
			"legend":         legendScene.key,
		},
		"voices": map[string]any{
			"npc":    map[string]string{"profile": npcScene.voice.VoiceID, "registered": npcScene.voice.Registered, "design_model": npcScene.voice.DesignModel},
			"legend": map[string]string{"profile": legendScene.voice.VoiceID, "registered": legendScene.voice.Registered, "design_model": legendScene.voice.DesignModel},
		},

		"profiles": map[string]any{
			"first_dialogue": map[string]string{"game": npcScene.gameID, "edition": npcScene.editionID, "profile": npcScene.profileID},
			"legend":         map[string]string{"game": legendScene.gameID, "edition": legendScene.editionID, "profile": legendScene.profileID},
		},

		"speaker_link_repointed": map[string]int{
			"first_dialogue": npcScene.repointed,
			"legend":         legendScene.repointed,
		},
		"assertions": map[string]any{
			"lines_carried_linked_voice": linesChecked,
			"lines_fully_played":         linesEnded,
			"lines_skipped_no_voice":     linesSkipped,
			"asr_qa_passed":              qaPassed,
			"voice_usage":                voiceUsage,
			"ended_by_voice":             endedByVoice,
			"wav_sha256":                 wavByChar,
			"wavs_differ":                shaNPC != shaLegend,
		},
		"sessions": map[string]string{
			"first_dialogue": evidence[0].sessionID,
			"legend":         evidence[1].sessionID,
		},
	}
	writeJSONFile(t, filepath.Join(bundle, "manifest.json"), manifest)

	for i, ev := range evidence {
		dir := filepath.Join(bundle, fmt.Sprintf("session-%d-%s", i, ev.label))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		writeJSONFile(t, filepath.Join(dir, "snapshot.json"), ev.snapshot)
		writeJSONFile(t, filepath.Join(dir, "dev_events.json"), ev.events) // private evidence: the raw correlated stream
		writeJSONFile(t, filepath.Join(dir, "spoken.json"), ev.spoken)
	}
	t.Logf("e2e: evidence bundle written: %s (manifest + %d sessions' snapshots, raw events and spoken-line records)", bundle, len(evidence))

	// ── 13. the run's summary (the no-simulation-theater label) ───────────
	t.Logf("LIVE PER-CHARACTER E2E (Dolphin + real providers): PASS — %d lines carried the voice of their linked character (%d fully played, %d skipped without a voice), %d ASR QA verdicts passed, two distinct voices used (npc %d/legend %d voice-bearing lines, ended %d/%d), the characters' WAVs differ; voices %s/%s cleaned from the Base registry",
		linesChecked, linesEnded, linesSkipped, qaPassed,
		voiceUsage[npcScene.voice.VoiceID], voiceUsage[legendScene.voice.VoiceID],
		endedByVoice[npcScene.voice.VoiceID], endedByVoice[legendScene.voice.VoiceID],
		npcScene.voice.Registered, legendScene.voice.Registered)
	_ = startAll
}
