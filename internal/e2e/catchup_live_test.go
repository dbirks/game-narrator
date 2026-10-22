// Package e2e — the live catch-up narration E2E (issue #25, ADR-0122):
//
// Dolphin (real emulator, real disc image, the recorded first-dialogue
// savestate) → live frame capture → OCR → commit against the script
// imported from the same disc image → the session's catch-up speech policy
// (the product default: catch_up, the 900 ms batch window, the 3 s
// per-utterance max age) — driven by the test pressing A through the first
// in-game conversation FASTER than the speech runs (a press lands as soon
// as the box's line commits, while the previous line's TTS round trip and
// playback are still in the air), so the lines commit into a busy speech
// and the policy's live shape is what is under test.
//
// The assertions (the brief in issue #25, the live half of ADR-0122's
// validation):
//
//  1. The session stays current — no backlog: no utterance starts more
//     than the per-utterance max age (plus a bounded scheduler margin)
//     after the newest line it covers committed; a line left more than
//     the batch window after a newer commit settles skipped/catch_up_stale
//     and never starts; and every committed eligible line settles exactly
//     once (spoken by one utterance, or fenced with a catch-up reason).
//  2. The rapid same-speaker boxes merge into fewer utterances (the
//     catch_up batching events, the ADR-0122 live-validation's batch
//     half): at least one line commits while the speech is busy and
//     settles through the catch-up batch (the window drop, the rescue's
//     hand-off or the run's outcome — the immediate path never emits a
//     catch-up terminal), every catch-up run is one speaker (the merge's
//     same-speaker rule), the playback count never exceeds the line count
//     (a multi-line run is one playback for its lines), and the manifest
//     records the live commit cadence against the 900 ms batch window —
//     the window's own multi-line admission (two lines committing inside
//     the window) is the controlled-timing suites' evidence (the batcher
//     unit tests, the four-rapid-boxes white-box walk, the
//     synthetic/gn-t025-catch-up replay); the ADR-0122 live validation
//     asserts the three properties — every box commits, the catch-up
//     speech covers every committed message, nothing stale starts.
//  3. Each spoken utterance uses the speaker's voice: every utterance
//     carries the voice of the character its lines' speaker key was linked
//     to; unattributed lines (the unmatched read-through) carry the
//     narrator voice — the narrator is never a substitute for a character
//     that has an approved voice.
//  4. The evidence bundle (dev-trace frames and crops, the correlated dev
//     event stream, the spoken-line records and the played WAVs) lands
//     under the REPO ROOT's gitignored artifacts/catch-up-live/<run-id>/
//     (AGENTS.md evidence bundle, rule 5: the bundle carries the raw
//     payloads, the logs never do).
//
// This is a live test (AGENTS.md rule 3 category (c)+(d)+(e): the Dolphin
// emulator plus the real model providers; the file sink stands in for the
// hardware speaker, the same class the live per-character E2E uses). It
// SKIPs as NOT RUN unless the prerequisites are present (gate, image,
// emulator tools, the first-dialogue fixture + savestate, the gateway,
// the voice roles); the Clef/OCR-fallback roles are not skip gates — an
// unproven stage is a failure with evidence, not a silent skip.
//
// Log-safe per AGENTS.md rule 5: log lines carry counts, ids, hashes and
// timings — never dialogue text, speaker key values or tokens.
//
// Run (the shared gateway env provides the OCR VLM fallback, the
// `decision` role, the TTS roles and the `speech_asr` role):
//
//	GN_GATE=test-dolphin go test -p 1 -count=1 -timeout 45m -v \
//	  -run TestLiveCatchUpRapidDialogue ./internal/e2e/
//
// In the gate it runs in the test-dolphin real-Dolphin phase (Makefile:
// the phase's -run pattern names it alongside the internal/dolphin Real
// tests; -p 1 keeps one Dolphin at a time).

package e2e

import (
	"context"
	"fmt"
	"log"
	"log/slog"
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
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// The catch-up live test's pacing and budget. The press rhythm is what
// makes the run "faster than speech": the driver presses A as soon as the
// box's line commits (the commit is the evidence the box's text is
// settled), and the previous line's utterance — TTS round trip, the ASR
// QA gate, the playback — outlives the box's typewriter by a wide margin
// (measured on the 2026-10-20 live runs: the first line's commit→playback
// was ~2-4 s of audio against a 154 s box hold the per-character run
// waited through; here the next box commits while that audio is still
// playing, which is the batch the policy exists to speak).
const (
	cuSessionWait    = 14 * time.Minute // the drive+settle window for the conversation
	cuDriveSettle    = 4 * time.Minute  // after the conversation ends: let the last runs settle
	cuTotalBudget    = 45 * time.Minute // the whole test (voice design included)
	cuLaunchTimeout  = 180 * time.Second
	cuPressHold      = 500 * time.Millisecond // the proven drive-table hold (ADR 0043's press shape)
	cuConvGone       = 30 * time.Second       // box-absent readings this long: the conversation is over (the between-box transition reads empty ~1-9 s — the per-character run's box 1→2 gap measured 9 s; 30 s of no box is the conversation's end, not a transition)
	cuPressStable    = 3 * time.Second        // box text unchanged before a press may land
	cuStallGap       = 45 * time.Second       // a settled box that never commits: press anyway (evidence-bound) — the bound must exceed one line's box→OCR→Clef→commit path WITH the VLM fallback in the loop (the per-character test's proven 45 s advance-stall guard; 30 s fired mid-pipeline on the 2026-10-21 runs)
	cuLostA          = 12 * time.Second       // a press that leaves the box text unchanged this long: lost, retry
	cuMaxPresses     = 5                      // press attempts per box instance (1 + bounded lost-A retries)
	cuMinDialogueBox = 3                      // the brief's "multi-box conversation"
)

// cuEvidenceSub is this test's gitignored evidence subdir under the REPO
// ROOT's artifacts/ (.gitignore: artifacts/) — go test runs in
// internal/e2e, so the bundle must resolve against the repo root
// (pcRepoRoot), not the package dir.
const cuEvidenceSub = "catch-up-live"

// cuV is this test's voice-workflow state, late-bound into the cleanup:
// whatever registered by test end gets removed from the real Base registry,
// even if a later step fails after an earlier one already registered (the
// per-character test's same-shape discipline; ADR 0022: live tests touch
// only gn_test_ names, and always remove them).
type cuVoiceState struct {
	st         *store.Store
	as         *assets.Local
	registry   *tts.Registry
	service    *voices.Service
	char       pcVoiceOutcome // the scene speaker's designed voice
	narr       pcVoiceOutcome // the narrator's designed voice (unattributed lines)
	registered []string       // the gn_test_ names removed on cleanup
}

var cuV cuVoiceState

func TestLiveCatchUpRapidDialogue(t *testing.T) {
	// The live catch-up e2e belongs to the test-dolphin gate (ADR-0065):
	// it boots the real emulator AND calls the real gateway.
	if os.Getenv(dolphin.EnvGate) != "test-dolphin" {
		t.Skipf("live catch-up e2e outside the test-dolphin gate: run via make test-dolphin (it sets %s=test-dolphin)", dolphin.EnvGate)
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

	// The recorded first-dialogue checkpoint: fixture root + fixture +
	// savestate, verified against the manifest's SHA-256 claim (ADR 0054).
	root, ok := pcFixtureRoot(t)
	if !ok {
		t.Skipf("NOT RUN: the fixtures are absent (external, gitignored; record them with the dolphin fixture gate, ADR 0054)")
	}
	fdDir := filepath.Join(root, pcFixtureFirstDialogue)
	if _, err := os.Stat(filepath.Join(fdDir, "profile.json")); err != nil {
		t.Skipf("NOT RUN: fixture %s is incomplete (%v)", pcFixtureFirstDialogue, err)
	}
	fdState := pcVerifyState(t, fdDir, pcStateFirstDialogue)

	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	gatewayCfg := roles[providers.RoleAgentReasoning]
	if strings.TrimSpace(gatewayCfg.BaseURL) == "" || strings.TrimSpace(gatewayCfg.Token()) == "" {
		t.Skipf("NOT RUN: set %s and the token env named by %s (default %s) to run the live catch-up e2e",
			providers.EnvGatewayURL, providers.EnvGatewayTokenEnv, providers.EnvGatewayToken)
	}
	designCfg, cloneCfg := roles[providers.RoleVoiceDesign], roles[providers.RoleVoiceClone]
	designEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceDesign)
	cloneEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceClone)
	if strings.TrimSpace(designCfg.Model) == "" || strings.TrimSpace(cloneCfg.Model) == "" {
		t.Skipf("NOT RUN: set %s (character voice design) and %s (Base synthesis) to run the live catch-up e2e", designEnv, cloneEnv)
	}
	directURL := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"))
	if directURL == "" {
		directURL = strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	}
	if directURL == "" {
		t.Skipf("NOT RUN: set GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL (or GAME_NARRATOR_TTS_BASE_DIRECT_URL) to the Base voice registry")
	}
	// The ASR QA role: wired when configured (the QA gate runs on every
	// cache insert, the product shape); NOT a skip gate — the acceptance
	// here is the catch-up shape and the per-utterance voices, and a QA-less
	// run still proves both (it is logged as a deviation, like the per-
	// character test's decision role).
	asrCfg := roles[providers.RoleSpeechASR]

	// The dev-only Dolphin seam the runtime consults for kind:dolphin.
	t.Setenv(dolphin.EnvDevDolphin, "1")

	ctx, cancel := context.WithTimeout(context.Background(), cuTotalBudget)
	t.Cleanup(cancel)
	startAll := time.Now()

	runID := "catchup-" + time.Now().UTC().Format("20060102T150405Z")
	bundle := filepath.Join(pcRepoRoot(t), "artifacts", cuEvidenceSub, runID)
	t.Logf("catchup e2e: run %s; evidence bundle %s (class: real Dolphin + real providers; savestate verified by manifest sha)", runID, bundle)

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
	// a failed live run's server-side error is debuggable.
	srvLog := disc
	if p := os.Getenv("GN_E2E_DEBUG_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			t.Cleanup(func() { _ = f.Close() })
			srvLog = slog.New(slog.NewTextHandler(f, nil))
			debugLog = log.New(f, "catchup-client: ", log.Ltime|log.Lmicroseconds)
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
	if _, err := authSvc.Setup(ctx, "cu-admin", e2ePassword, "127.0.0.1:1", true); err != nil {
		t.Fatalf("auth setup: %v", err)
	}
	dev, err := authSvc.CreateUser(ctx, "cu-dev", e2ePassword, auth.RoleDeveloper, nil, "catchup-live")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	token, _, err := authSvc.CreateToken(ctx, nil, auth.CreateTokenParams{
		UserID: dev.ID, Name: "e2e-catchup", Role: auth.RoleDeveloper,
	}, "127.0.0.1:1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// ── voice workflow (the same wiring cmd/game-narrator builds) ─────────
	cuV = cuVoiceState{st: st, as: as}
	t.Cleanup(func() { cuVoiceCleanup(t, runID) })
	cuSetupVoices(t, roles, runner, adapters, designCfg, cloneCfg, asrCfg, directURL)
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}

	// ── session runtime: speech, VLM OCR fallback, Clef, Dolphin seam ─────
	var bridge dolphin.Bridge
	devDolphin := func() (capture.Source, error) {
		if bridge == nil {
			return nil, fmt.Errorf("catchup e2e: the Dolphin bridge is not launched yet")
		}
		src, err := bridge.FrameSource(10)
		if err != nil {
			t.Logf("catchup e2e: dev-dolphin frame source failed: %v (bridge state: %+v)", err, bridge.State())
		}
		return src, err
	}

	sessions, err := runtime.New(runtime.Options{
		Adapters:    adapters,
		Store:       st,
		Hub:         hub,
		Jobs:        runner,
		AssetsRoot:  filepath.Join(dataDir, "dev-trace"),
		Speech:      &runtime.SpeechOptions{Voices: cuV.service, Assets: as, Sink: runtime.SinkFile, FileSinkDir: filepath.Join(dataDir, "audio")},
		OCRFallback: cuOCRFallback(t, roles),
		Decision:    cuDecisionOpts(t, roles),
		DevDolphin:  devDolphin,
		Log:         srvLog,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}

	srv := api.New(bootstrap.BuildInfo{Version: "e2e-catchup-live"}, time.Now(), srvLog,
		api.WithAuth(authSvc, store.NewAuthStore(st)),
		api.WithStore(st),
		api.WithJobs(js),
		api.WithEvents(hub),
		api.WithImport(as, adapters, runner),
		api.WithSessions(sessions),
		api.WithDevEvents(store.NewEventStore(st)),
		api.WithVoices(cuV.service, runner),
	).Handler()

	// The disc's script, extracted once (counts only, rule 5).
	discFile, err := gcdisc.Open(img)
	if err != nil {
		t.Fatalf("open disc image: %v", err)
	}
	bmgBytes, err := windwaker.ExtractBMG(discFile)
	_ = discFile.Close()
	if err != nil {
		t.Fatalf("extract BMG from disc image: %v", err)
	}

	// ── the scene profile (game → edition → draft → import → character →
	// speaker link → the box's two measured positions → publish) ──────────
	cuScene := cuSetupScene(t, ctx, srv, token, st, bmgBytes, imageSHA, fdDir, runID)

	// The two voices the session resolves: the scene speaker's own voice
	// (the character the speaker link targets) and the narrator's (the
	// unattributed lines — the unmatched read-through — resolve to it).
	cuV.char = cuDesignVoice(t, ctx, srv, token, st, as, cuV.registry,
		cuScene.charID, "A young woman's bright, curious voice — the sea-raised heroine of the opening island scene. Lively, a little mischievous, clear enunciation.", designCfg.Model, cloneCfg.Model)
	cuV.narr = cuDesignVoice(t, ctx, srv, token, st, as, cuV.registry,
		"narrator", "An older storyteller's warm, unhurried voice — the legend narrator of an old kingdom's tale. Grave but kind, slow cadence.", designCfg.Model, cloneCfg.Model)
	t.Logf("catchup e2e: voices ready: character %s (%s), narrator %s (%s)",
		cuScene.charID, cuV.char.Registered, "narrator", cuV.narr.Registered)

	// Warm the TTS cache the way production does (the profile presynth,
	// ADR-0153) and the A/V harness does: without it the catch-up timing
	// measured the TTS backend's raw synthesis (2-3 s per uncached line on
	// 2026-10-22), not the session's catch-up pacing the assertions are about.
	warmed, warmedIDs := avPreSynthesize(t, ctx, st, cuV.service, cuScene.profileID, fdDir, cuV.char, cuV.narr)
	avRuntimePresynth(t, ctx, sessions, cuScene.profileID, warmedIDs)
	t.Logf("catchup e2e: prewarm: %d (voice, line-text) cache entries", warmed)

	// ── the live session: bridge from the first-dialogue savestate ───────
	launchCtx, launchCancel := context.WithTimeout(context.Background(), cuLaunchTimeout)
	defer launchCancel()
	bridge = pcLaunchBridge(t, launchCtx, dataDir, "catchup", fdState)
	fileSinkDir := filepath.Join(dataDir, "audio")

	sessionID := pcStartSession(t, srv, token, cuScene.profileID)
	// The session's catch-up policy, applied through the settings control
	// (ADR-0122 decision 1): the request names the documented default
	// word, so the snapshot's applied word is the evidence the speech side
	// and the developer UI agree on the policy.
	snap := cuSetSettings(t, srv, token, sessionID, "catch_up")
	if cu := snapshotCatchUp(snap); cu != "catch_up" {
		t.Fatalf("session %s snapshot catch_up = %q, want catch_up (the ADR-0122 default policy; the settings control applies it)", sessionID, cu)
	}
	t.Logf("catchup e2e: session %s running (profile %s, catch_up policy applied, snapshot verified)", sessionID, cuScene.profileID)

	// Drive the conversation: rapid A presses (faster than the speech),
	// then let the last committed lines settle.
	evs, presses := cuDriveConversation(t, srv, token, sessionID, bridge)

	// Reconcile the polled stream against the durable store read directly
	// (the per-character test's gap check: the assertions run on the
	// store's truth; a non-zero route gap is logged).
	if direct, err := store.NewEventStore(st).After(ctx, 0, events.Filter{SessionID: sessionID}, 10000); err == nil {
		evs = cuReconcileStream(t, sessionID, evs, direct)
	}

	snapshot := pcStopSession(t, srv, token, sessionID)
	bridge.Stop()

	// ── the evidence bundle (artifacts/ is gitignored; private) ───────────
	// The raw evidence lands BEFORE the assertions: a failed assertion still
	// leaves the bundle (the stream, the frames and the WAVs) to diagnose
	// against; the manifest and the model's records are written after the
	// assertions pass (the failure's diagnosis is the stream, not the
	// model's view of it).
	cuWriteEvidence(t, bundle, sessionID, snapshot, evs, dataDir)

	// ── the assertions (against the store-truth stream) ───────────────────
	model := cuParseSpeechModel(evs, fileSinkDir, sessionID)
	cuAssertConversation(t, cuScene, model)
	cuAssertMerge(t, model)
	cuAssertNoBacklog(t, model)
	cuAssertVoices(t, cuScene, model)

	cuWriteManifest(t, bundle, runID, imageSHA, fdState, cuScene, sessionID,
		model, presses, startAll)

	ev := cuMergeEvidenceOf(model)
	t.Logf("LIVE CATCH-UP E2E (Dolphin + real providers): PASS — %d committed lines over the rapid-A drive, %d playbacks, %d catch-up runs (largest %d lines, %d batch events), every line settled exactly once, no playback started past its max age, every run one speaker, every utterance in its speaker's voice (narrator for the unattributed); live commit cadence %s against the %s batch window; bundle %s (%s)",
		model.LineCount(), model.StartCount(), model.RunCount(), model.LargestRun(), ev.BatchEvents,
		cuMergeEvidenceGapNote(ev.MinCommitGap), ev.Window.String(), bundle,
		time.Since(startAll).Round(time.Second))
}
