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
	"strings"
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

	// ADR-0170 (the conversation invariants): the drive mode — "rapid" the
	// default (make test-av, the fast-advancing player), "slow" the
	// slow-reader (A only after the page's speech ended + 500 ms; the
	// queue-don't-cut rule measures this drive's page turns). The
	// synthesized-duration tap keeps the pipewire sink (the observable
	// audio) and records each utterance's full WAV duration on the way.
	driveMode := strings.TrimSpace(os.Getenv("GN_AV_DRIVE"))
	if driveMode == "" {
		driveMode = "rapid"
	}
	if driveMode != "rapid" && driveMode != "slow" {
		t.Fatalf("GN_AV_DRIVE must be rapid or slow (got %q)", os.Getenv("GN_AV_DRIVE"))
	}
	slowDrive := driveMode == "slow"
	synthDurs := &avSynthDurTracker{durs: map[string]int64{}}

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
		Adapters:   adapters,
		Store:      st,
		Hub:        hub,
		Jobs:       runner,
		AssetsRoot: filepath.Join(dataDir, "dev-trace"),
		Speech: &runtime.SpeechOptions{
			Voices:      cuV.service,
			Assets:      as,
			SinkFactory: synthDurs.factory(sinkName),
			FileSinkDir: filepath.Join(dataDir, "audio"),
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
	prewarm, prewarmIDs := avPreSynthesize(t, ctx, st, cuV.service, s.profileID, pc.fdDir, cuV.char, cuV.narr)
	avRuntimePresynth(t, ctx, sessions, s.profileID, prewarmIDs)
	// ADR-0170 evidence: the session profile's page rows the speech wiring
	// reads at commit time — per page, the ordinal, the word count and a
	// short hash (never the game text itself, AGENTS.md rule 5). Where the
	// game drew two boxes for one message and the rows disagree with the
	// boxes, the page the wiring spoke on box two shows up against these.
	if pcContent, cerr := st.GetProfileContent(ctx, s.profileID); cerr == nil {
		for _, msg := range pcContent.Messages {
			var rows []string
			for _, p := range pcContent.Pages {
				if p.MessageID == msg.ID {
					rows = append(rows, fmt.Sprintf("p%d w%d sha%x", p.Ordinal, len(strings.Fields(p.PageText)), pconvPageHash(p.PageText)))
				}
			}
			if len(rows) > 1 {
				t.Logf("av e2e: profile pages ..%s: canonical w%d %s", shortID(msg.ID), len(strings.Fields(msg.SpeechText)), rows)
			}
		}
	}

	// ── the live session: bridge from the route's savestate (the ADR-0162
	//    play loop) or the first-dialogue fixture state ─────────────────────
	launchCtx, launchCancel := context.WithTimeout(context.Background(), cuLaunchTimeout)
	defer launchCancel()
	var route *dolphin.Route
	routePath := os.Getenv("GN_AV_ROUTE")
	routeSaveState := pc.fdState
	if routePath != "" {
		rb, rerr := os.ReadFile(routePath)
		if rerr != nil {
			t.Fatalf("GN_AV_ROUTE: read the route file: %v", rerr)
		}
		parsed, rerr := dolphin.ParseRoute(rb)
		if rerr != nil {
			t.Fatalf("GN_AV_ROUTE: %s: %v", routePath, rerr)
		}
		route = &parsed
		routeSaveState = filepath.Join(pc.fdDir, "..", route.SaveState)
	}
	bridge = pcLaunchBridge(t, launchCtx, dataDir, avBridgeLabel(route), routeSaveState)

	// The shared-clock capture: the display + the sink's monitor, from T0.
	mediaDir := filepath.Join(bundle, "media")
	stopRec, recStart := avStartRecorder(t, bridge.State().Display, sinkName+".monitor",
		filepath.Join(mediaDir, "session.mp4"), filepath.Join(mediaDir, "session.wav"))
	t.Cleanup(func() { _ = stopRec() })

	sessionID := avStartPredictiveSession(t, srv, token, s.profileID)

	// Drive the conversation (the cuDriver's proven A-press advance loop),
	// then let the last committed lines settle — or, in route mode (the
	// ADR-0162 play loop), run the route's scripted steps and settle.
	var evs []devEnvelope
	var presses []cuPress
	var marks []avMark
	if route != nil {
		evs, presses, marks = avRunRoute(t, ctx, srv, token, sessionID, bridge, route)
	} else if slowDrive {
		evs, presses = avDriveSlow(t, srv, token, sessionID, bridge)
	} else {
		evs, presses = cuDriveConversation(t, srv, token, sessionID, bridge)
	}

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
	// The expected voices are the scene's two (the per-line resolveVoice
	// rule): a commit attributed to a speaker resolves to the character's
	// voice, every unattributed line to the narrator's (the catch-up
	// test's same expectation). The region rows give the box positions.
	expectedVoices := avExpectedVoices{Character: cuV.char.VoiceID, Narrator: cuV.narr.VoiceID,
		CharacterID: s.charID, SpeakerByMessage: avSpeakerByMessage(t, ctx, st, s.profileID)}
	regionGeos := avRegionGeos(t, ctx, st, s.profileID)
	rep := avBuildTimeline(runID, sessionID, recStart, evs, sessionWAV, expectedVoices, regionGeos, marks)
	rep.SessionMP4 = filepath.Join(mediaDir, "session.mp4")
	rep.SessionWAV = filepath.Join(mediaDir, "session.wav")

	// ADR-0170 (the conversation invariants): the synthesized durations the
	// tap recorded (each line's own occurrence; a merged run's occurrence —
	// the batched lines' RunOccurrenceID) are written onto the lines and the
	// invariant findings run AFTER the ASR scoring (below): the duplicate
	// and read-ahead rules read the transcript side, and the page ground
	// truth is the corpus's expected text the scoring writes onto each line
	// — the box the player SAW, not the OCR's noisy reading (an OCR drop
	// must not read as read-ahead). The conversation run only: the route
	// mode's scripted play loop does not drive the conversation page by
	// page, so it runs no invariants.
	clipsDir := filepath.Join(bundle, "clips")
	avSliceClips(t, filepath.Join(mediaDir, "session.mp4"), filepath.Join(mediaDir, "session.wav"), clipsDir, &rep.Lines)
	expected := avCorpusTexts(t, ctx, st, s.profileID)
	avASRScore(t, ctx, pc.asrCfg, &rep.Lines, expected)

	if route == nil {
		durs := synthDurs.snapshot()
		for i := range rep.Lines {
			l := &rep.Lines[i]
			id := l.OccurrenceID
			if l.Batched && l.RunOccurrenceID != "" {
				id = l.RunOccurrenceID
			}
			if d, ok := durs[id]; ok {
				l.SynthDurMS = d
			}
		}
		rep.ConvDrive = driveMode
		rep.ConvGateIssues, rep.ConvReportIssues = convCheckConversation(rep.Lines, avConvPages(rep.Lines), slowDrive)
	}

	// The evidence lands before the assertions: a failed gate still leaves
	// the timeline + the report + the media to diagnose against.
	writeJSONFile(t, filepath.Join(bundle, "timeline.json"), rep)
	// The raw session event stream the timeline was built from: a phantom
	// or a late line is diagnosed from these rows, not re-derived (the
	// bundle is outside git; events carry OCR text, AGENTS.md rule 5).
	writeJSONFile(t, filepath.Join(bundle, "events.json"), evs)
	avWriteReport(t, filepath.Join(bundle, "report.md"), rep)
	avWriteManifest(t, bundle, runID, pc, s, sessionID, sinkName, prewarm, len(presses), rep, pc.asrCfg.Configured(), startAll)

	avAssertReport(t, rep)

	t.Logf("LIVE A/V HARNESS (Dolphin + real providers): PASS — %d committed lines (%d idle samples), %d A presses, settled→audio p50/p95 = %d/%d ms, appear→audio p50/p95 = %d/%d ms, %d cache hits, %d phantom utterances, %d stale starts, %d duplicate commits, conversation invariants: %d gate issues, %d report-only (%s drive); bundle %s (%s)",
		len(rep.Lines), rep.IdleSamples, len(presses), rep.SettledToAudioP50MS, rep.SettledToAudioP95MS,
		rep.AppearToAudioP50MS, rep.AppearToAudioP95MS, rep.CacheHits, rep.PhantomUtterances, rep.StaleStarts, rep.DuplicateCommits,
		len(rep.ConvGateIssues), len(rep.ConvReportIssues), rep.ConvDrive,
		bundle, time.Since(startAll).Round(time.Second))
}
