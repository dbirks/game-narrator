// Package e2e — the A/V harness's live support (ADR 0143): the gate +
// prerequisite checks, the predictive-mode session start, the corpus
// expected-text lookup, the per-clip ASR scoring, the evidence manifest and
// the gate's assertions. The test itself (av_harness_test.go) reads as one
// flow; these helpers keep each stage's rules in one place, the way the
// catch-up live test splits its helpers. Log-safe per AGENTS.md rule 5.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/games/adapter/windwaker"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/asr"
	"github.com/dbirks/game-narrator/internal/providers/router"
	rt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

// avPreconds is everything the A/V harness needs before it boots: the
// gate-checked environment, the verified fixture and the provider roles.
type avPreconds struct {
	img       string
	imageSHA  string
	fdDir     string
	fdState   string
	roles     map[providers.Role]providers.RoleConfig
	designCfg providers.RoleConfig
	cloneCfg  providers.RoleConfig
	asrCfg    providers.RoleConfig
	directURL string
}

// avPreconditions runs the A/V harness's gate and prerequisite checks and
// reports NOT RUN (a skip, never a pass) when any is absent. It belongs to
// the test-av gate (this harness) and the test-dolphin gate (the phase that
// runs the real-Dolphin suites); everything else is the catch-up live test's
// same dev-host prerequisites (the real image, the emulator tools, the
// verified first-dialogue fixture, the gateway, the voice roles).
func avPreconditions(t *testing.T) avPreconds {
	t.Helper()
	gate := os.Getenv(dolphin.EnvGate)
	if gate != "test-av" && gate != "test-dolphin" {
		t.Skipf("NOT RUN: %s is %q (want test-av or test-dolphin) — run via make test-av", dolphin.EnvGate, gate)
	}
	img := os.Getenv(dolphin.EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set — point it at the user-supplied Wind Waker image (dev-only, the image is never logged)", dolphin.EnvGameImage)
	}
	if _, err := os.Stat(img); err != nil {
		t.Skipf("NOT RUN: disc image not found at the %s path (dev-only)", dolphin.EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo", "tesseract", "pactl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH (dev host dependencies: dolphin-emu, xorg-server-xvfb, ffmpeg, x11-utils, tesseract, pipewire/pulseaudio)", bin)
		}
	}
	if err := strayDolphinProcs(); err != nil {
		t.Skipf("NOT RUN: %v", err)
	}
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
		t.Skipf("NOT RUN: set %s and the token env named by %s (default %s)", providers.EnvGatewayURL, providers.EnvGatewayTokenEnv, providers.EnvGatewayToken)
	}
	designCfg, cloneCfg := roles[providers.RoleVoiceDesign], roles[providers.RoleVoiceClone]
	if strings.TrimSpace(designCfg.Model) == "" || strings.TrimSpace(cloneCfg.Model) == "" {
		t.Skipf("NOT RUN: set the voice design (%s) and Base synthesis (%s) model envs", providers.RoleVoiceDesign, providers.RoleVoiceClone)
	}
	directURL := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"))
	if directURL == "" {
		directURL = strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	}
	if directURL == "" {
		t.Skipf("NOT RUN: set GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL (or GAME_NARRATOR_TTS_BASE_DIRECT_URL) to the Base voice registry")
	}
	// The ASR role scores each line's clip (WER/CER); it is NOT a skip gate —
	// an unconfigured run records zero WER/CER and logs the deviation.
	return avPreconds{
		img: img, imageSHA: dolphinImageSHA(t, img),
		fdDir: fdDir, fdState: fdState, roles: roles,
		designCfg: designCfg, cloneCfg: cloneCfg, asrCfg: roles[providers.RoleSpeechASR],
		directURL: directURL,
	}
}

// avStartPredictiveSession starts one live Dolphin session in PREDICTIVE
// mode (the ADR-0143 session shape: the matcher commits on its predictive
// signal, so the box → commit → audio path is as short as the product
// allows) and verifies the applied mode from the settings control's
// snapshot (the catch-up test's same snapshot-verified rule).
func avStartPredictiveSession(t *testing.T, srv http.Handler, token, profileID string) string {
	t.Helper()
	code, out := pcDoJSON(t, srv, token, http.MethodPost, "/api/v1/sessions", map[string]any{
		"profile_id":   profileID,
		"frame_source": map[string]any{"kind": rt.SourceKindDolphin},
		"mode":         "predictive",
		"dev_trace":    true,
	}, nil)
	if code != http.StatusCreated {
		t.Fatalf("session start (predictive) = %d: %v (the runtime answers the dolphin kind 501 without %s=1 or the bridge seam)",
			code, out, dolphin.EnvDevDolphin)
	}
	sessionID, _ := out["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("session start response has no id: %v", out)
	}
	// Verify the applied mode from the settings snapshot (the control that
	// reports what the session actually runs, not what was requested).
	snap := avSetMode(t, srv, token, sessionID, "predictive")
	if m, _ := snap["mode"].(string); m != "predictive" {
		t.Fatalf("session %s snapshot mode = %q, want predictive (the ADR-0143 session shape)", sessionID, m)
	}
	t.Logf("av e2e: session %s running (profile %s, predictive mode applied and snapshot-verified, dev_trace on)", sessionID, profileID)
	return sessionID
}

// avSetMode applies one session mode through the settings control and
// returns the applied snapshot (the ADR-0143 session's mode half; the
// catch-up test's cuSetSettings carries the catch_up half).
func avSetMode(t *testing.T, srv http.Handler, token, sessionID, mode string) map[string]any {
	t.Helper()
	rec := cuDo(t, srv, token, http.MethodPut, "/api/v1/sessions/"+sessionID+"/settings",
		mustMarshal(t, map[string]any{"mode": mode}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session settings (mode=%s) = %d: %s", mode, rec.Code, rec.Body.String())
	}
	var snap map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("settings snapshot decode: %v", err)
	}
	return snap
}

// avDecisionOpts builds the session's decision options for the A/V harness:
// the Clef decision role when configured, but with Margin 0 — the
// matcher's own acceptance floor, so a locally-accepted margin is not held
// for a Clef adjudication. That keeps the box → commit → audio path short
// (the latency gate's stage), which is the ADR-0143 harness's whole point;
// the catch-up live test instead uses its dolphinClefMargin to exercise the
// adjudication. Unconfigured, the session runs local adjudication (nil).
func avDecisionOpts(t *testing.T, roles map[providers.Role]providers.RoleConfig) *rt.DecisionOptions {
	t.Helper()
	cfg := roles[providers.RoleDecision]
	if !cfg.Configured() {
		decEnv, _, _, _ := providers.RoleEnvVars(providers.RoleDecision)
		t.Logf("av e2e: decision role NOT CONFIGURED (set %s); the session runs local adjudication (Margin 0)", decEnv)
		return nil
	}
	roleReg := providers.NewRegistry(roles)
	chatRouter := router.New(roleReg, router.Options{})
	t.Logf("av e2e: Clef decision role configured (model %s, protocol %s; Margin 0 — local evidence commits)", cfg.Model, cfg.Protocol)
	return &rt.DecisionOptions{Router: chatRouter, Role: cfg, Timeout: dolphinClefTimeout, Margin: 0}
}

// avCore is the harness's durable core: the store + assets + adapters, the
// jobs store + event hub + runner, the auth service + a developer token, and
// the server log. avDurableCore builds it (the catch-up live test's same
// wiring) and registers the cleanups (store close, runner stop, hub close).
type avCore struct {
	dataDir  string
	st       *store.Store
	as       *assets.Local
	adapters *adapter.Registry
	js       *store.JobsStore
	hub      *events.Hub
	runner   *jobs.Runner
	authSvc  *auth.Service
	token    string
	srvLog   *slog.Logger
}

// avDurableCore builds the harness's durable core: a fresh store (migrated),
// the local asset store, the adapter registry (generic + windwaker), the
// jobs store + event hub + runner (50 ms poll), the auth service (a dev
// user + token) and the server log (GN_E2E_DEBUG_LOG names a file so a
// failed live run's server-side error is debuggable). It registers the
// cleanups so a failed run still closes the store, stops the runner and
// closes the hub.
func avDurableCore(t *testing.T, ctx context.Context) avCore {
	t.Helper()
	c := avCore{}
	c.dataDir = t.TempDir()
	st, err := store.Open(ctx, store.DBPath(c.dataDir))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c.st = st

	as, err := assets.NewLocal(filepath.Join(c.dataDir, "assets"))
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}
	c.as = as

	adapters := adapter.NewRegistry()
	if err := generic.Register(adapters); err != nil {
		t.Fatalf("generic.Register: %v", err)
	}
	if err := windwaker.Register(adapters); err != nil {
		t.Fatalf("windwaker.Register: %v", err)
	}
	c.adapters = adapters

	disc := slog.New(slog.DiscardHandler)
	srvLog := disc
	if p := os.Getenv("GN_E2E_DEBUG_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			t.Cleanup(func() { _ = f.Close() })
			srvLog = slog.New(slog.NewTextHandler(f, nil))
		}
	}
	c.srvLog = srvLog

	c.js = store.NewJobsStore(st)
	c.hub = events.New(events.WithStore(store.NewEventStore(st)), events.WithLogger(disc))
	runner, err := jobs.NewRunner(c.js, c.hub, disc, jobs.WithPollInterval(50*time.Millisecond))
	if err != nil {
		t.Fatalf("jobs.NewRunner: %v", err)
	}
	c.runner = runner
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = runner.Stop(stopCtx)
		_ = c.hub.Close()
	})

	authSvc := auth.NewService(store.NewAuthStore(st), auth.WithPasswordCost(1000), auth.WithLogger(disc))
	if _, err := authSvc.Setup(ctx, "av-admin", e2ePassword, "127.0.0.1:1", true); err != nil {
		t.Fatalf("auth setup: %v", err)
	}
	dev, err := authSvc.CreateUser(ctx, "av-dev", e2ePassword, auth.RoleDeveloper, nil, "av-harness")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	c.authSvc = authSvc
	token, _, err := authSvc.CreateToken(ctx, nil, auth.CreateTokenParams{
		UserID: dev.ID, Name: "e2e-av", Role: auth.RoleDeveloper,
	}, "127.0.0.1:1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	c.token = token
	return c
}

// avCorpusTexts reads the profile's imported messages and returns
// message_id → the expected spoken text (the normalized reading — the
// tracker's committed text for a clean box, and therefore the line's TTS
// input). It is the ASR scorer's expected side (the WER/CER ground truth).
func avCorpusTexts(t *testing.T, ctx context.Context, st *store.Store, profileID string) map[string]string {
	t.Helper()
	out := map[string]string{}
	cursor := ""
	for {
		page, next, err := st.ListMessages(ctx, profileID, store.MessageFilter{}, cursor, 500)
		if err != nil {
			t.Fatalf("corpus texts: ListMessages: %v", err)
		}
		for _, m := range page {
			v := strings.TrimSpace(m.SpeechText)
			if v == "" {
				v = strings.TrimSpace(m.NormalizedText)
			}
			if v != "" {
				out[m.ID] = v
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(out) == 0 {
		t.Fatalf("corpus texts: the profile %s has no readable normalized text", profileID)
	}
	return out
}

// avASRScore transcribes each line's clip WAV through the real ASR endpoint
// and scores it against the line's expected text (the speech.Evaluate WER /
// CER the QA gate uses). It fills the timeline's per-line ASRWER / ASRCER
// and — the scored side being the corpus's script text — the line's
// ASRWERvsScript (Amendment 1, the gate's spoken-line fidelity input).
// A read-through line (message_id "unknown") has no corpus ground truth and
// is left unscored (zero). The ASR role unconfigured leaves all lines zero
// and the caller logs the deviation. It never logs transcript text (rule 5).
func avASRScore(t *testing.T, ctx context.Context, asrCfg providers.RoleConfig, lines *[]avLineRecord, expected map[string]string) {
	t.Helper()
	if !asrCfg.Configured() {
		return // the caller logs the ASR-not-configured deviation
	}
	client, err := asr.NewFromRole(asrCfg)
	if err != nil {
		t.Fatalf("av asr: NewFromRole: %v", err)
	}
	scored := 0
	for i := range *lines {
		l := &(*lines)[i]
		if l.MessageID == "" || l.MessageID == "unknown" || l.ClipWAV == "" {
			continue
		}
		exp, ok := expected[l.MessageID]
		if !ok || exp == "" {
			continue // no ground truth (a read-through or an unmapped line)
		}
		wav, err := os.ReadFile(l.ClipWAV)
		if err != nil {
			t.Logf("av asr: line %d clip unreadable (%v): left unscored", l.Index, err)
			continue
		}
		cctx, ccancel := context.WithTimeout(ctx, 60*time.Second)
		tr, err := client.TranscribeText(cctx, wav, "en")
		ccancel()
		if err != nil {
			t.Logf("av asr: line %d transcription failed (%v): left unscored", l.Index, err)
			continue
		}
		rec := speech.Evaluate(exp, tr.Text)
		l.ASRWER, l.ASRCER = rec.WER, rec.CER
		// The scored side IS the script text (the corpus lookup), so the
		// transcript's WER against the expected reading is the vs-script WER
		// the gate's spoken-line fidelity rule reads (Amendment 1).
		l.ASRWERvsScript = rec.WER
		l.asrScored = true
		scored++
	}
	t.Logf("av asr: scored %d line clips (model %s)", scored, asrCfg.Model)
}

// avRegionGeos reads the profile's region rows and returns region id →
// the row's centre Y over the frame height (the normalised RectY plus half
// the height, ADR 0137's placement-aware rows): the box-position
// classifier's input (Amendment 1). A profile without rows answers the
// empty map — the report classifies no positions, honestly.
func avRegionGeos(t *testing.T, ctx context.Context, st *store.Store, profileID string) map[string]float64 {
	t.Helper()
	rows, err := st.ListRegions(ctx, profileID)
	if err != nil {
		t.Fatalf("region geos: ListRegions: %v", err)
	}
	geos := map[string]float64{}
	for _, r := range rows {
		geos[r.ID] = r.RectY + r.RectH/2.0
	}
	return geos
}

// avSpeakerByMessage is the profile's message id → speaker character id map
// (the speaker link already applied by the import): the attribution the
// runtime resolves each line's voice from.
func avSpeakerByMessage(t *testing.T, ctx context.Context, st *store.Store, profileID string) map[string]string {
	t.Helper()
	content, err := st.GetProfileContent(ctx, profileID)
	if err != nil {
		t.Fatalf("speaker map: GetProfileContent: %v", err)
	}
	m := make(map[string]string, len(content.Messages))
	for _, msg := range content.Messages {
		if msg.SpeakerID != "" {
			m[msg.ID] = msg.SpeakerID
		}
	}
	return m
}

// avWriteManifest writes the run's evidence manifest (AGENTS.md evidence
// bundle; rule 5: the manifest carries ids, hashes, digests, counts and
// timings — never dialogue text, speaker key values, frames or tokens).
func avWriteManifest(t *testing.T, bundle, runID string, pc avPreconds, s cuScene, sessionID, sinkName string,
	prewarm, presses int, rep avTimelineReport, asrConfigured bool, startAll time.Time) {
	t.Helper()
	werSum, cerSum, scored := 0.0, 0.0, 0
	for _, l := range rep.Lines {
		if l.asrScored {
			werSum, cerSum, scored = werSum+l.ASRWER, cerSum+l.ASRCER, scored+1
		}
	}
	manifest := map[string]any{
		"run_id":            runID,
		"timestamp_utc":     time.Now().UTC().Format(time.RFC3339),
		"gate":              os.Getenv(dolphin.EnvGate),
		"test_class":        "live: real Dolphin emulator + real model providers (ADR-0065 category c+d+e); the first-dialogue savestate verified by the fixture manifest sha; a PipeWire null sink isolated from the user's devices, and the shared-clock FFmpeg capture (one .mp4 + one 16 kHz mono .wav from the same wall clock) stands in for the hardware speaker",
		"os":                osLabel(),
		"go_version":        goVersionLabel(),
		"git_sha":           repoGitSHA(),
		"disc_image_sha256": pc.imageSHA, // the hash only (rule 5: the path is never recorded)
		"savestate": map[string]string{
			"file":   filepath.Base(pc.fdState),
			"sha256": stateSHA(t, pc.fdState),
		},
		"profile":   map[string]string{"game": s.gameID, "edition": s.editionID, "profile": s.profileID},
		"character": s.charID,
		"voices": map[string]any{
			"character": map[string]string{"profile": cuV.char.VoiceID, "registered": cuV.char.Registered, "design_model": cuV.char.DesignModel, "clone_model": cuV.char.CloneModel},
			"narrator":  map[string]string{"profile": cuV.narr.VoiceID, "registered": cuV.narr.Registered, "design_model": cuV.narr.DesignModel, "clone_model": cuV.narr.CloneModel},
		},
		"audio": map[string]any{
			"sink":      "pipewire null sink (module-null-sink, isolated)",
			"sink_name": sinkName,
			"monitor":   sinkName + ".monitor",
			"master":    "the narrator streams to the null sink; the harness records the sink's monitor",
		},
		"session": sessionID,
		"mode":    "predictive",
		"prewarm": map[string]any{
			"entries": prewarm,
			"note":    "the fixture's first-dialogue lines pre-synthesised under both voices into the speech cache (the ADR-0143 warm-cache design): the latency gate measures the session's stages, not the TTS round trip",
		},
		"drive":    map[string]any{"presses": presses, "driver": "cuDriver (the catch-up live test's proven A-press advance loop)"},
		"asr":      map[string]any{"configured": asrConfigured, "scored_clips": scored},
		"duration": time.Since(startAll).Round(time.Second).String(),
		"latency_gate": map[string]any{
			"settled_to_audio_p50_ms": rep.SettledToAudioP50MS,
			"settled_to_audio_p95_ms": rep.SettledToAudioP95MS,
			"appear_to_audio_p50_ms":  rep.AppearToAudioP50MS,
			"appear_to_audio_p95_ms":  rep.AppearToAudioP95MS,
			"commit_to_audio_p50_ms":  rep.CommitToAudioP50MS,
			"commit_to_audio_p95_ms":  rep.CommitToAudioP95MS,
			"idle_samples":            rep.IdleSamples,
			"bounds":                  "settled p50<=500ms, p95<=800ms on >= 3 idle lines (the ADR-0143 Amendment 1 gate; GN_AV_NO_LATENCY_GATE=1 records the bounds without asserting them)",
		},
		"consistency": map[string]any{
			"lines":                 len(rep.Lines),
			"missed":                rep.MissedLines,
			"duplicate_commits":     rep.DuplicateCommits,
			"stale_starts":          rep.StaleStarts,
			"cache_hits":            rep.CacheHits,
			"phantom_utterances":    rep.PhantomUtterances,
			"phantom_read_throughs": rep.PhantomReadThroughs,
			"skipped":               rep.SkippedLines,
		},
	}
	if scored > 0 {
		manifest["asr_mean_wer"] = werSum / float64(scored)
		manifest["asr_mean_cer"] = cerSum / float64(scored)
	}
	writeJSONFile(t, filepath.Join(bundle, "manifest.json"), manifest)
}

// avGateFailures is the ADR-0143 Amendment 1 gate as data: the report's
// failures, one named message per violated rule (an empty slice is a
// pass). Pure over the report — no env, no testing — so the unit tests
// pin each rule on synthetic timelines. The rules (Amendment 1 makes the
// gate strict: a warm run that cannot name its own audio now fails):
//
//   - >= 2 committed lines (the multi-box conversation);
//   - >= 3 idle samples (the settled→audio gate's floor: two samples are
//     not a distribution, the 2/2 ms percentile that fooled the first
//     board run — 12 of 14 lines had no audio at all);
//   - the settled→audio p50/p95 within the warm-run bounds (the original
//     ADR-0143 latency rule);
//   - zero missed lines (a committed line that never got a terminal);
//   - zero phantom utterances (a speech.started the timeline's lines do
//     not explain — the board's "speech with no box");
//   - zero stale starts and zero duplicate commits (the original
//     ADR-0143 consistency rules);
//   - every line with a known expected voice speaks in it (the resolveVoice
//     rule, per line — a character line in the narrator's voice, or
//     unattributed line in the character's, is a miswired speaker);
//   - every spoken line's ASR WER vs script at or under 0.35 (a spoken
//     line that does not track the script is a bad read or a wrong line).
func avGateFailures(rep avTimelineReport) []string {
	var fails []string
	if len(rep.Lines) < 2 {
		fails = append(fails, fmt.Sprintf("%d committed lines; want >= 2 (the multi-box conversation)", len(rep.Lines)))
	}
	if rep.IdleSamples < 3 {
		fails = append(fails, fmt.Sprintf("idle samples = %d; want >= 3 (too few idle lines to measure: the settled→audio gate needs a distribution, not one or two samples)", rep.IdleSamples))
	}
	if rep.MissedLines > 0 {
		fails = append(fails, fmt.Sprintf("%d missed line(s) (committed, never routed, never terminal); the timeline rows are the evidence", rep.MissedLines))
	}
	if rep.PhantomUtterances > 0 {
		fails = append(fails, fmt.Sprintf("%d phantom utterance(s) (%d read-through): speech.started events no committed line explains — the board's \"speech with no box\"; the timeline rows are the evidence", rep.PhantomUtterances, rep.PhantomReadThroughs))
	}
	if rep.StaleStarts != 0 {
		fails = append(fails, fmt.Sprintf("%d stale start(s) (a line's audio began after a newer line had committed — the ADR-0122 backlog the session must not show); the timeline row is the evidence", rep.StaleStarts))
	}
	if rep.DuplicateCommits != 0 {
		fails = append(fails, fmt.Sprintf("%d duplicate commit(s) of the same occurrence (the reducer's dedupe must hold); the timeline row is the evidence", rep.DuplicateCommits))
	}
	for i := range rep.Lines {
		l := &rep.Lines[i]
		if l.ExpectedVoiceID != "" && l.TAudioStartEventMS > 0 && !l.VoiceMatch { // only a line that played has a voice to judge
			fails = append(fails, fmt.Sprintf("line %d: spoken in voice %q; the line's expected voice is %q (a line with a known expected voice must speak in it); the timeline row is the evidence", l.Index, l.VoiceID, l.ExpectedVoiceID))
		}
	}
	for i := range rep.Lines {
		l := &rep.Lines[i]
		if l.TAudioStartEventMS == 0 {
			continue // not a spoken line: nothing was audible to misread
		}
		if l.ASRWERvsScript > 0.35 {
			fails = append(fails, fmt.Sprintf("line %d: ASR WER vs script = %.3f > 0.35 (a spoken line must track the script; the clip and the timeline row are the evidence)", l.Index, l.ASRWERvsScript))
		}
	}
	// The latency bounds: the ADR-0143 warm-run rule, only meaningful over
	// the gate's idle-sample floor (a p50 over 0..2 samples is no estimate).
	if rep.IdleSamples >= 3 {
		if rep.SettledToAudioP50MS > 500 {
			fails = append(fails, fmt.Sprintf("settled→audio p50 = %d ms > 500 ms (the ADR-0143 warm-run bound, %d idle samples); the per-line rows are the evidence", rep.SettledToAudioP50MS, rep.IdleSamples))
		}
		if rep.SettledToAudioP95MS > 800 {
			fails = append(fails, fmt.Sprintf("settled→audio p95 = %d ms > 800 ms (the ADR-0143 warm-run bound, %d idle samples); the per-line rows are the evidence", rep.SettledToAudioP95MS, rep.IdleSamples))
		}
	}
	return fails
}

// avAssertReport runs the ADR-0143 Amendment 1 gate's assertions against
// the timeline: every avGateFailures rule, and — unless the run disabled
// the latency gate (GN_AV_NO_LATENCY_GATE=1) — the settled→audio p50/p95
// within the warm-run bounds. The env escape covers the latency bounds
// only (an acoustic measurement the environment's audio path can degrade);
// the strictness rules — the idle floor, the missed, the phantoms, the
// voices, the WER — always assert: they are the session's correctness, not
// the audio path's. The report.md and the timeline rows are the evidence a
// failure names.
func avAssertReport(t *testing.T, rep avTimelineReport) {
	t.Helper()
	fails := avGateFailures(rep)
	if os.Getenv("GN_AV_NO_LATENCY_GATE") == "1" {
		var kept []string
		for _, f := range fails {
			if !strings.HasPrefix(f, "settled→audio") {
				kept = append(kept, f)
			}
		}
		t.Logf("av gate: latency bounds DISABLED (GN_AV_NO_LATENCY_GATE=1) — recorded settled p50/p95 = %d/%d ms over %d idle samples without asserting the 500/800 ms bounds", rep.SettledToAudioP50MS, rep.SettledToAudioP95MS, rep.IdleSamples)
		fails = kept
	}
	for _, f := range fails {
		t.Errorf("av gate: %s", f)
	}
}
