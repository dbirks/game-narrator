package runtime_test

// LIVE Clef adjudication session test (docs/spec/16 "real decision tests
// when configured"): a replay session of the synthetic clip runs with the
// decision role wired to the deployed System One / Clef-Omni endpoint behind
// the provider router — the same DecisionOptions shape production builds in
// cmd/game-narrator — while the session's speech pipeline resolves the
// committed line through the real TTS and writes real WAV evidence through
// the file sink.
//
// The clip's first line is deliberately ambiguous between two candidates:
// the profile carries "The harbour bell rang twice before anyone looked up."
// (demo-page-1) next to "The harbour bell rang twice before the fog lifted."
// (demo-x01), and the observed line is shared by both — the local policy
// holds the commit for the adjudication (docs/spec/05 "candidate retrieval →
// graph-aware scoring → optional Clef adjudication → commitment policy").
// The clip's tail is padded with the line's last full-line frame: a cold
// gateway call (observed up to 13 s straight after a GPU reset — the
// systemone live budget's reason) must land while the box is still up, or
// the session stops on local evidence alone and the live proof is not a
// proof.
//
// Assertions:
//   - a real Clef call went out: a decision.requested carried the ambiguous
//     pair and a decision.resolved carries the gateway's request id and
//     latency (the correlation the developer UI joins on, docs/spec/13);
//   - the answer drove the commit: a message.committed with reason
//     CLEF_DISAMBIGUATED whose message is Clef's choice from the applied
//     decision.resolved for the same observation;
//   - speech followed with the right voice: the newest Clef commit's
//     generation reaches speech.ended {result:"ended"} (only the newest can
//     play to the end — a newer commit cancels the older line's audio,
//     docs/spec/01 invariant 2), the speech events carry the narrator
//     profile, and the file sink wrote decodable WAV bytes for it.
//
// NOT RUN (skipped, with setup instructions, never failed) when the gateway
// or the direct Base registry is not configured — the provider live-test
// convention (docs/spec/16). The OCR engine stays the pipeline's scripted
// truth seam (this test is about the decision and audio halves, not
// recognition); every decision and TTS byte is real.
//
// Configuration (never printed):
//
//	GAME_NARRATOR_GATEWAY_URL               shared gateway (systemone + speech)
//	GAME_NARRATOR_GATEWAY_TOKEN_ENV         token holder (default GAME_NARRATOR_GATEWAY_TOKEN)
//	GAME_NARRATOR_ROLE_DECISION_MODEL       default clef-omni
//	GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL   default qwen3-tts-voicedesign
//	GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL    default qwen3-tts-base
//	GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL
//	   or GAME_NARRATOR_TTS_BASE_DIRECT_URL  direct Base service (registry)
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// decisionLiveBudget bounds the live adjudication. A warm call is ~0.5 s,
// but a cold gateway straight after a GPU reset has been observed at 13 s:
// the runtime's 1500 ms decision budget (docs/spec/06) is a different, much
// tighter thing, and the live test owns its own, the same way the voices
// live setup stretches the TTS roles' and the systemone live tests stretch
// the endpoint's.
const decisionLiveBudget = 120 * time.Second

// decisionLivePadFrames pads the clip's tail with full-line frames (6 s of
// recorded time: 30 s at the 5× live pace) so a cold adjudication lands
// while the box is still up: the padding re-shows the line, so the box
// reappears as a new lifetime and its READY observation is the reply-time
// reading the fences apply the answer to.
const decisionLivePadFrames = 60

func TestLiveDecision(t *testing.T) {
	designCfg, cloneCfg, directURL, ok := liveSpeechEnv(t)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)

	// --- the decision role, the way production loads it (cmd/game-narrator's
	// newDecisionWiring): the model id from the environment or the role's
	// documented default, the System One protocol, and the live budget in
	// place of the role's 1500 ms production floor.
	decisionModelEnv, _, _, _ := providers.RoleEnvVars(providers.RoleDecision)
	decisionModel := strings.TrimSpace(os.Getenv(decisionModelEnv))
	if decisionModel == "" {
		decisionModel = "clef-omni"
	}
	t.Setenv(decisionModelEnv, decisionModel)
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	decisionCfg := roles[providers.RoleDecision]
	if !decisionCfg.Configured() || decisionCfg.Protocol != providers.ProtocolSystemOne {
		t.Fatalf("the decision role is not usable: %s", decisionCfg.Status())
	}
	decisionCfg.Timeout = decisionLiveBudget
	decisionOpts := &runtime.DecisionOptions{
		Router: router.New(providers.NewRegistry(roles), router.Options{}),
		Role:   decisionCfg,
		// The test's lever on the commitment policy, the unit tests' own
		// choice: production leaves the margin at zero (Clef is asked only
		// for a ranking the local scoring abstained on), while 1.0 asks the
		// model about every non-abstained ranking, so the clip's ambiguous
		// line — the one the test exists to prove — reaches the gateway.
		Margin: 1.0,
	}
	t.Logf("decision role: model=%s protocol=%s live budget=%s (the 1500 ms production budget is the role's, not this test's)",
		decisionModel, decisionCfg.Protocol, decisionLiveBudget)

	// --- the real voice, the speech live test's flow: one real VoiceDesign
	// audition, one gn_test_ registration (removed on the way out).
	designClient, err := tts.NewFromRole(designCfg)
	if err != nil {
		t.Fatalf("design client: %v", err)
	}
	cloneClient, err := tts.NewFromRole(cloneCfg)
	if err != nil {
		t.Fatalf("clone client: %v", err)
	}
	registry, err := tts.NewRegistry(directURL, cloneCfg.Token())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	const brief = "A calm narrator voice, warm and unhurried, for an original synthetic character."
	line := voices.SyntheticAuditionLines[0]
	start := time.Now()
	reference, err := designClient.Design(ctx, tts.DesignRequest{Text: line, Instructions: brief, Language: "English"})
	if err != nil {
		t.Fatalf("designing the narrator reference: %v", err)
	}
	t.Logf("designed reference: model=%s latency=%s (wall %s)", reference.Model, reference.Latency, time.Since(start).Round(time.Millisecond))
	sum := sha256.Sum256(reference.WAV)
	refSHA := hex.EncodeToString(sum[:])
	fingerprint := voices.Fingerprint(refSHA, line, reference.Model)
	var rand8 [4]byte
	if _, err := rand.Read(rand8[:]); err != nil {
		t.Fatalf("random name: %v", err)
	}
	name := "gn_test_" + hex.EncodeToString(rand8[:])
	if err := tts.ValidateName(name); err != nil {
		t.Fatalf("test name fails the grammar: %v", err)
	}
	start = time.Now()
	if _, err := registry.Register(ctx, tts.Voice{
		Name: name, RefAudio: reference.WAV,
		RefText: line, Consent: liveConsentRecord,
	}); err != nil {
		t.Fatalf("registering %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := registry.Remove(context.Background(), name); err != nil {
			t.Logf("cleanup remove %s: %v", name, err)
		}
	})
	t.Logf("registered %s (wall %s)", name, time.Since(start).Round(time.Millisecond))

	// --- the session environment: real store, real assets, the shared TTS
	// cache over the real clone client, the file sink as evidence. The clip
	// is the synthetic fixture re-stamped at 5× and padded (pacedDecisionClip).
	corpusPresent(t)
	replayRoot := t.TempDir()
	clip := pacedDecisionClip(t, replayRoot)
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	assetRoot := t.TempDir()
	assetStore, err := assets.NewLocal(assetRoot)
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	cache := speech.New(assetStore, speech.NewMemIndex(), 4)
	voiceService := &voices.Service{
		Store: st, Assets: assetStore,
		Design: designClient, Base: cloneClient, Registry: registry,
		Speech:        cache,
		InstanceID:    "primary",
		BaseModelID:   cloneCfg.Model,
		BackendDigest: "live-decision-v1",
	}
	if err := voices.New(voiceService); err != nil {
		t.Fatalf("voices.New: %v", err)
	}
	profileID := seedLiveNarratorVoice(t, st, assetStore, reference.WAV, line, refSHA, fingerprint, cloneCfg.Model, name)
	t.Cleanup(func() { _ = st.Close() })
	t.Cleanup(func() { _ = hub.Close() })

	sinkDir := t.TempDir()
	mgr, err := runtime.New(runtime.Options{
		Store:      st,
		Hub:        hub,
		AssetsRoot: t.TempDir(),
		// Paced at real time from the clip's re-stamped deltas (ADR-0039):
		// the decision's live budget (up to 120 s, cold) must fit inside the
		// padded source lifetime, and the committed line its dwell.
		ReplaySpeed: 1.0,
		ReplayRoots: []string{replayRoot},
		OCR:         &fakeOCR{script: truthScript(t), delay: 10 * time.Millisecond},
		Speech: &runtime.SpeechOptions{
			Voices:      voiceService,
			Assets:      assetStore,
			Sink:        runtime.SinkFile,
			FileSinkDir: sinkDir,
		},
		Decision: decisionOpts,
		Log:      slogDiscard(),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(mgr.Close)

	// --- run the replay clip end to end.
	prof, msgMap := seedSyntheticProfile(t, st, true)
	info, err := mgr.Start(ctx, runtime.StartRequest{
		ProfileID:   prof,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: clip},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	deadline := time.Now().Add(13 * time.Minute)
	var final runtime.Info
	for {
		final, _ = mgr.Get(info.ID)
		if final.State == runtime.StateStopped || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if final.State != runtime.StateStopped {
		t.Fatalf("session did not stop: state %s", final.State)
	}
	evs, err := mem.After(ctx, 0, events.Filter{SessionID: info.ID}, 4096)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	// --- 1. A real Clef call went out, and it carried the ambiguous pair. ---
	var requested, resolved int
	type decisionPayload struct {
		ObservationSeq int64    `json:"observation_seq"`
		RequestID      string   `json:"request_id"`
		Candidates     []string `json:"candidates"`
		Choice         string   `json:"choice"`
		MessageID      string   `json:"message_id"`
		Applied        bool     `json:"applied"`
		Accepted       bool     `json:"accepted"`
		Reason         string   `json:"reason"`
		LatencyMS      int64    `json:"latency_ms"`
		Model          string   `json:"model"`
	}
	var applied []decisionPayload
	for _, ev := range evs {
		switch ev.Name {
		case events.DecisionRequested:
			requested++
			var p struct {
				Candidates []struct {
					MessageID string `json:"message_id"`
				} `json:"candidates"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				continue
			}
			if requested == 1 {
				ids := make([]string, 0, len(p.Candidates))
				for _, c := range p.Candidates {
					ids = append(ids, c.MessageID)
				}
				t.Logf("first decision.requested: text=%q candidates=%d (the clip's line is shared by demo-page-1 and demo-x01)", p.Text, len(p.Candidates))
				if len(ids) < 2 {
					t.Fatalf("the first decision carried %d candidates, want ≥2 (the ambiguous pair)", len(ids))
				}
				hasPage, hasVariant := false, false
				for _, id := range ids {
					if id == msgMap["demo-page-1"] {
						hasPage = true
					}
					if id == msgMap["demo-x01"] {
						hasVariant = true
					}
				}
				if !hasPage || !hasVariant {
					t.Fatalf("the first decision's candidates miss the ambiguous pair (demo-page-1=%v, demo-x01=%v)", hasPage, hasVariant)
				}
			}
		case events.DecisionResolved:
			resolved++
			var p decisionPayload
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				continue
			}
			t.Logf("decision.resolved: request_id=%s model=%s latency=%dms applied=%v choice=%s message=%s",
				p.RequestID, p.Model, p.LatencyMS, p.Applied, p.Choice, p.MessageID)
			if p.RequestID == "" {
				t.Fatalf("decision.resolved carries no request id: the call did not come back from the gateway")
			}
			if p.Applied {
				applied = append(applied, p)
			}
		}
	}
	if requested < 1 || resolved < 1 {
		t.Fatalf("decision events: requested=%d resolved=%d, want ≥1 of each (the policy asked and the gateway answered)", requested, resolved)
	}
	if len(applied) < 1 {
		t.Fatal("no applied decision.resolved: the answer never drove a commit")
	}
	t.Logf("real Clef request ids (applied): %s", applied[0].RequestID)

	// --- 2. The answer drove the commit. ---
	//
	// Every CLEF_DISAMBIGUATED commit must match an applied decision on the
	// observation and the choice. The audio assertions follow the NEWEST
	// CLEF commit: a newer commit cancels the older line's audio
	// (fenceAndCancel, docs/spec/01 invariant 2), so only the newest
	// generation can play to the end and reach speech.ended{result:"ended"}.
	// The padded tail keeps the box up until the source ends, so the newest
	// commit's line plays out before source_end.
	var clefCommits []commitPayload
	seen := 0
	for _, ev := range evs {
		if ev.Name != events.MessageCommitted {
			continue
		}
		seen++
		var c commitPayload
		if err := json.Unmarshal(ev.Data, &c); err != nil {
			continue
		}
		if c.Reason == "CLEF_DISAMBIGUATED" {
			clefCommits = append(clefCommits, c)
		}
	}
	if len(clefCommits) < 1 {
		t.Fatalf("no message.committed with reason CLEF_DISAMBIGUATED among %d commits: the answer drove nothing", seen)
	}
	var clefCommit commitPayload
	for _, c := range clefCommits {
		var match *decisionPayload
		for i := range applied {
			d := &applied[i]
			if d.ObservationSeq == c.ObsSeq && d.MessageID == c.MessageID {
				match = d
				break
			}
		}
		if match == nil {
			t.Fatalf("commit %s (observation %d, generation %d) matches no applied decision.resolved: the commit did not come from the model's answer",
				c.MessageID, c.ObsSeq, c.Generation)
		}
		t.Logf("clef commit: generation=%d message=%s observation=%d request_id=%s", c.Generation, c.MessageID, c.ObsSeq, match.RequestID)
		if c.Generation > clefCommit.Generation {
			clefCommit = c
		}
	}
	if !clefCommit.SpeechOK {
		t.Fatal("the newest Clef-committed line is not speech-eligible although the model said safe_to_speak")
	}

	// --- 3. Speech followed, with the right voice. ---
	// The newest CLEF commit's generation is the one the session's audio can
	// finish: nothing commits after it (it is the newest), the padded box
	// stays up, and source_end comes long after its dwell. An older commit's
	// audio is legitimately cancelled by the fence (docs/spec/01 invariant
	// 2), so "ended" is only owed to the newest.
	var ended, rightVoice bool
	for _, ev := range evs {
		if ev.Name != events.SpeechStarted && ev.Name != events.SpeechEnded {
			continue
		}
		var p struct {
			Generation int64  `json:"generation"`
			Result     string `json:"result"`
			VoiceID    string `json:"voice_id"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			continue
		}
		if p.Generation != clefCommit.Generation {
			continue
		}
		if ev.Name == events.SpeechStarted {
			t.Logf("playback started: generation=%d voice=%s", p.Generation, name)
		}
		if ev.Name == events.SpeechEnded && p.Result == "ended" {
			ended = true
			t.Logf("clef-committed line ended: generation=%d result=%q", p.Generation, p.Result)
		}
		if p.VoiceID == profileID {
			rightVoice = true
		}
	}
	if !ended {
		t.Fatalf("the Clef-committed line (generation %d) never reached speech.ended {result:\"ended\"}", clefCommit.Generation)
	}
	if !rightVoice {
		t.Fatalf("no speech event for the committed line carries the narrator profile %s: a substitute voice spoke", profileID)
	}
	var wavPath string
	var wavErr error
	by := time.Now().Add(10 * time.Second)
	for {
		wavPath, wavErr = genWAV(sinkDir, clefCommit.Generation)
		if wavErr == nil || time.Now().After(by) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if wavErr != nil {
		t.Fatalf("no WAV evidence for the committed line's generation %d: %v", clefCommit.Generation, wavErr)
	}
	b, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatalf("reading the evidence WAV: %v", err)
	}
	pcm, err := speech.DecodeWAV(b)
	if err != nil {
		t.Fatalf("decoding the evidence WAV: %v", err)
	}
	if pcm.Frames() < 160 {
		t.Fatalf("evidence WAV has %d frames at %d Hz; want delivered samples", pcm.Frames(), pcm.SampleRate)
	}
	delivered := time.Duration(pcm.Frames()) * time.Second / time.Duration(pcm.SampleRate)
	t.Logf("evidence WAV: %s bytes=%d sample_rate=%d frames=%d (~%s delivered) — real TTS audio, not a stub",
		filepath.Base(wavPath), len(b), pcm.SampleRate, pcm.Frames(), delivered.Round(time.Millisecond))
}

// pacedDecisionClip derives the live session's clip from the synthetic
// fixture the same way the speech live test does (liveClipPace× the recorded
// cadence), and pads the tail with decisionLivePadFrames copies of the clip's
// last full-line frame (line a, before the box dismisses): the padding
// re-shows the ambiguous line, so a cold adjudication (up to the live budget)
// lands while the box is still up — the box reappears as a new lifetime and
// its READY observation is the reply-time reading the fences apply the
// answer to. Without the padding the clip's box dismisses at 12 s of live
// time, and a cold reply would be fenced stale_text before it could commit.
func pacedDecisionClip(t *testing.T, root string) string {
	t.Helper()
	src := syntheticClipDir()
	dst := filepath.Join(root, "live-decision-clip")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatalf("clip dir: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(src, "timestamps.json"))
	if err != nil {
		t.Fatalf("timestamps: %v", err)
	}
	var ts []map[string]any
	if err := json.Unmarshal(raw, &ts); err != nil {
		t.Fatalf("timestamps parse: %v", err)
	}
	if len(ts) < 18 {
		t.Fatalf("synthetic clip has %d entries; want ≥18 (its line-a full frames)", len(ts))
	}
	lineName, _ := ts[17]["file"].(string) // clip frame 18: line a's full line, its last visible frame
	lineFrame, err := os.ReadFile(filepath.Join(src, lineName))
	if err != nil {
		t.Fatalf("line frame: %v", err)
	}
	for _, e := range ts {
		f, _ := e["file"].(string)
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatalf("copying %s: %v", f, err)
		}
		if err := os.WriteFile(filepath.Join(dst, f), b, 0o600); err != nil {
			t.Fatalf("copying %s: %v", f, err)
		}
		if n, ok := e["ts_ns"].(float64); ok {
			e["ts_ns"] = int64(n) * liveClipPace
		}
	}
	var last int64
	if n, ok := ts[len(ts)-1]["ts_ns"].(int64); ok {
		last = n
	}
	for i := 1; i <= decisionLivePadFrames; i++ {
		name := fmt.Sprintf("frame-%03d.png", len(ts)+i)
		if err := os.WriteFile(filepath.Join(dst, name), lineFrame, 0o600); err != nil {
			t.Fatalf("padding frame %d: %v", i, err)
		}
		last += 100 * 1000 * 1000 * liveClipPace
		ts = append(ts, map[string]any{"file": name, "ts_ns": last})
	}
	b, err := json.MarshalIndent(ts, "", " ")
	if err != nil {
		t.Fatalf("timestamps marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "timestamps.json"), b, 0o600); err != nil {
		t.Fatalf("timestamps write: %v", err)
	}
	return dst
}
