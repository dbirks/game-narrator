// Package e2e — the live multimodal Dolphin A/V harness (ADR-0143):
//
// Dolphin (the real emulator, the real disc image, the recorded
// first-dialogue savestate) → the shared-clock FFmpeg capture (one .mp4 +
// one 16 kHz mono .wav from the same wall clock, T0) of the Xvfb display
// AND the narrator's isolated PipeWire null sink → a live session in
// PREDICTIVE mode driven through the first in-game conversation → the dev
// event stream correlated onto that shared clock into a per-line timeline
// (box appear → text settled → commit → Clef → TTS ready → audio start →
// audio end, with the acoustic RMS onset measured in the recorded track) →
// per-line multimodal clips → ASR WER/CER per clip.
//
// The gate asserts (avAssertReport): >= 2 committed lines, no stale start
// (the ADR-0122 backlog at the timeline level), no duplicate commit, and —
// unless GN_AV_NO_LATENCY_GATE=1 — the settled → audio p50/p95 within the
// warm-run bounds (p50<=500 ms, p95<=800 ms). The fixture's lines are
// pre-synthesised into the speech cache (both voices) so the latency gate
// measures the session's own stages, not a TTS round trip.
//
// A live test (AGENTS.md rule 3 (c)+(d)+(e): the Dolphin emulator plus the
// real model providers; the null sink + shared-clock capture stand in for
// the hardware speaker). SKIPs as NOT RUN without the prerequisites (gate,
// image, emulator tools, the verified first-dialogue fixture, the gateway,
// the voice roles). Log-safe per rule 5: counts, ids, hashes, digests and
// timings only — never dialogue text, speaker keys, frames or tokens.
//
//	make test-av
//
// or directly (the shared gateway env provides the OCR VLM fallback, the
// `decision` role, the TTS roles and the `speech_asr` role):
//
//	GN_GATE=test-av go test -p 1 -count=1 -timeout 45m -v \
//	  -run TestLiveDolphinAVHarness ./internal/e2e/

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games/adapter/windwaker"
	"github.com/dbirks/game-narrator/internal/gcdisc"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
)

// avEvidenceSub is this harness's gitignored evidence subdir under the
// REPO ROOT's artifacts/ (.gitignore: artifacts/). go test runs in
// internal/e2e, so the bundle resolves against the repo root (pcRepoRoot).
const avEvidenceSub = "av-harness"

// avTotalBudget is the whole run (voice design + import + the drive).
const avTotalBudget = 45 * time.Minute

func TestLiveDolphinAVHarness(t *testing.T) {
	pc := avPreconditions(t)

	// The dev-only Dolphin seam the runtime consults for kind:dolphin.
	t.Setenv(dolphin.EnvDevDolphin, "1")

	ctx, cancel := context.WithTimeout(context.Background(), avTotalBudget)
	t.Cleanup(cancel)
	startAll := time.Now()

	runID := "av-" + time.Now().UTC().Format("20060102T150405Z")
	bundle := filepath.Join(pcRepoRoot(t), "artifacts", avEvidenceSub, runID)
	t.Logf("av e2e: run %s; evidence bundle %s (class: real Dolphin + real providers; savestate verified by manifest sha)", runID, bundle)

	// ── durable state, jobs, auth (avDurableCore) ──────────────────────────────
	core := avDurableCore(t, ctx)
	st, as, dataDir := core.st, core.as, core.dataDir
	adapters, js, hub, runner := core.adapters, core.js, core.hub, core.runner
	authSvc, token, srvLog := core.authSvc, core.token, core.srvLog

	// ── voice workflow (the wiring cmd/game-narrator builds) ─────────────
	cuV = cuVoiceState{st: st, as: as}
	t.Cleanup(func() { cuVoiceCleanup(t, runID) })
	cuSetupVoices(t, pc.roles, runner, adapters, pc.designCfg, pc.cloneCfg, pc.asrCfg, pc.directURL)
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}

	// ── the isolated null sink (before the runtime, for the speech target) ─
	sinkName := avCreateNullSink(t, runID)

	// ── session runtime: pipewire sink, VLM OCR fallback, Clef, Dolphin ────
	var bridge dolphin.Bridge
	devDolphin := func() (capture.Source, error) {
		if bridge == nil {
			return nil, fmt.Errorf("av e2e: the Dolphin bridge is not launched yet")
		}
		src, err := bridge.FrameSource(10)
		if err != nil {
			t.Logf("av e2e: dev-dolphin frame source failed: %v (bridge state: %+v)", err, bridge.State())
		}
		return src, err
	}

	sessions, err := runtime.New(runtime.Options{
		Store:      st,
		Hub:        hub,
		Jobs:       runner,
		AssetsRoot: filepath.Join(dataDir, "dev-trace"),
		Speech: &runtime.SpeechOptions{
			Voices:         cuV.service,
			Assets:         as,
			Sink:           runtime.SinkPipeWire,
			PipeWireTarget: sinkName,
			FileSinkDir:    filepath.Join(dataDir, "audio"),
		},
		OCRFallback: cuOCRFallback(t, pc.roles),
		Decision:    avDecisionOpts(t, pc.roles),
		DevDolphin:  devDolphin,
		Log:         srvLog,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { sessions.Close() })
	sessions.WarmOCR()

	srv := api.New(bootstrap.BuildInfo{Version: "e2e-av-live"}, time.Now(), srvLog,
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
	discFile, err := gcdisc.Open(pc.img)
	if err != nil {
		t.Fatalf("open disc image: %v", err)
	}
	bmgBytes, err := windwaker.ExtractBMG(discFile)
	_ = discFile.Close()
	if err != nil {
		t.Fatalf("extract BMG from disc image: %v", err)
	}

	// ── the scene profile (game → edition → import → character → link →
	// the box's two positions → publish) ────────────────────────────────────
	s := cuSetupScene(t, ctx, srv, token, st, bmgBytes, pc.imageSHA, pc.fdDir, runID)

	// The two voices the session resolves: the scene speaker's and the
	// narrator's (the unattributed lines).
	cuV.char = cuDesignVoice(t, ctx, srv, token, st, as, cuV.registry,
		s.charID, "A young woman's bright, curious voice — the sea-raised heroine of the opening island scene. Lively, a little mischievous, clear enunciation.", pc.designCfg.Model, pc.cloneCfg.Model)
	cuV.narr = cuDesignVoice(t, ctx, srv, token, st, as, cuV.registry,
		"narrator", "An older storyteller's warm, unhurried voice — the legend narrator of an old kingdom's tale. Grave but kind, slow cadence.", pc.designCfg.Model, pc.cloneCfg.Model)
	t.Logf("av e2e: voices ready: character %s (%s), narrator %s (%s)",
		s.charID, cuV.char.Registered, "narrator", cuV.narr.Registered)

	// ── warm the TTS cache (the ADR-0143 design) ──────────────────────────
	prewarm := avPreSynthesize(t, ctx, st, cuV.service, s.profileID, pc.fdDir, cuV.char, cuV.narr)

	// ── the live session: bridge from the first-dialogue savestate ────────
	launchCtx, launchCancel := context.WithTimeout(context.Background(), cuLaunchTimeout)
	defer launchCancel()
	bridge = pcLaunchBridge(t, launchCtx, dataDir, "av", pc.fdState)

	// The shared-clock capture: the display + the sink's monitor, from T0.
	mediaDir := filepath.Join(bundle, "media")
	stopRec, recStart := avStartRecorder(t, bridge.State().Display, sinkName+".monitor",
		filepath.Join(mediaDir, "session.mp4"), filepath.Join(mediaDir, "session.wav"))
	t.Cleanup(func() { _ = stopRec() })

	sessionID := avStartPredictiveSession(t, srv, token, s.profileID)

	// Drive the conversation (the cuDriver's proven A-press advance loop),
	// then let the last committed lines settle.
	evs, presses := cuDriveConversation(t, srv, token, sessionID, bridge)

	// Stop the capture (SIGINT → trailer finalize) and read the track.
	if err := stopRec(); err != nil {
		t.Logf("av e2e: recorder stop: %v (the media is written; the trailer may be partial)", err)
	}
	sessionWAV, err := os.ReadFile(filepath.Join(mediaDir, "session.wav"))
	if err != nil {
		t.Fatalf("read the session track (the RMS onset's source): %v", err)
	}

	// Reconcile the polled stream against the durable store read directly.
	if direct, err := store.NewEventStore(st).After(ctx, 0, events.Filter{SessionID: sessionID}, 10000); err == nil {
		evs = cuReconcileStream(t, sessionID, evs, direct)
	}
	_ = pcStopSession(t, srv, token, sessionID)
	bridge.Stop()

	// ── the shared-clock timeline + clips + ASR ───────────────────────────
	// The expected voice is the scene speaker's: the matched lines' speaker
	// resolves to the character (the narrator carries only the unattributed).
	rep := avBuildTimeline(runID, sessionID, recStart, evs, sessionWAV, cuV.char.VoiceID)
	rep.SessionMP4 = filepath.Join(mediaDir, "session.mp4")
	rep.SessionWAV = filepath.Join(mediaDir, "session.wav")
	clipsDir := filepath.Join(bundle, "clips")
	avSliceClips(t, filepath.Join(mediaDir, "session.mp4"), filepath.Join(mediaDir, "session.wav"), clipsDir, &rep.Lines)
	expected := avCorpusTexts(t, ctx, st, s.profileID)
	avASRScore(t, ctx, pc.asrCfg, &rep.Lines, expected)

	// The evidence lands before the assertions: a failed gate still leaves
	// the timeline + media to diagnose against.
	writeJSONFile(t, filepath.Join(bundle, "timeline.json"), rep)
	avWriteManifest(t, bundle, runID, pc, s, sessionID, sinkName, prewarm, len(presses), rep, pc.asrCfg.Configured(), startAll)

	avAssertReport(t, rep)

	t.Logf("LIVE A/V HARNESS (Dolphin + real providers): PASS — %d committed lines, %d A presses, settled→audio p50/p95 = %d/%d ms, appear→audio p50/p95 = %d/%d ms, %d cache hits, %d stale starts, %d duplicate commits; bundle %s (%s)",
		len(rep.Lines), len(presses), rep.SettledToAudioP50MS, rep.SettledToAudioP95MS,
		rep.AppearToAudioP50MS, rep.AppearToAudioP95MS, rep.CacheHits, rep.StaleStarts, rep.DuplicateCommits,
		bundle, time.Since(startAll).Round(time.Second))
}
