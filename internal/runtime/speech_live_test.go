package runtime_test

// LIVE speech session test (docs/spec/16: "real TTS tests when configured"):
// a replay session of the synthetic clip runs against the deployed
// checkpoints — one real VoiceDesign audition for the narrator, one
// gn_test_ registration on the direct Base registry (removed on the way
// out, AGENTS.md rule 5), and the session's speech pipeline resolving the
// committed lines through the real TTS and writing real WAV evidence
// through the file sink. It SKIPs as "NOT RUN" unless the gateway and the
// direct registry URL are configured, so `go test ./...` stays green
// offline. The environment reuses the internal/voices live setup: the same
// role configuration, the same fixed synthetic audition line and consent
// record, the same gn_test_ name grammar.
//
// The OCR engine stays the pipeline's scripted truth seam (this test is
// about the audio half, not recognition); every TTS byte is real. The clip
// is re-stamped at 5× the recorded source cadence and replayed at Speed 1.0
// (real time): the measured real-TTS round trip (~1 s) must fit inside a
// textbox's dwell before the fence moves or the session ends (ADR-0039).
//
// Configuration (never printed):
//
//	GAME_NARRATOR_GATEWAY_URL              shared gateway (speech route)
//	GAME_NARRATOR_GATEWAY_TOKEN_ENV        token holder (default GAME_NARRATOR_GATEWAY_TOKEN)
//	GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL  default qwen3-tts-voicedesign
//	GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL   default qwen3-tts-base
//	GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL
//	   or GAME_NARRATOR_TTS_BASE_DIRECT_URL  direct Base service (registry)

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// liveConsentRecord is the consent record this test's registration carries
// (docs/spec/14): the reference is an original synthetic design made for
// this test, and the record says so. It mirrors the voices package's own
// live harness constant.
const liveConsentRecord = "original_synthetic_design (game-narrator automated test)"

// liveSpeechEnv is the runtime-side copy of the voices live harness's
// configuration check: both the speech gateway and a direct Base registry
// URL must be configured for the audio half to be real.
func liveSpeechEnv(t *testing.T) (providers.RoleConfig, providers.RoleConfig, string, bool) {
	t.Helper()
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	designEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceDesign)
	cloneEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceClone)
	design, clone := roles[providers.RoleVoiceDesign], roles[providers.RoleVoiceClone]
	if m := strings.TrimSpace(os.Getenv(designEnv)); m != "" {
		design.Model = m
	} else {
		design.Model = "qwen3-tts-voicedesign"
	}
	if m := strings.TrimSpace(os.Getenv(cloneEnv)); m != "" {
		clone.Model = m
	} else {
		clone.Model = "qwen3-tts-base"
	}
	design.Timeout, clone.Timeout = 180*time.Second, 180*time.Second

	direct := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"))
	if direct == "" {
		direct = strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	}
	if strings.TrimSpace(design.BaseURL) == "" || strings.TrimSpace(design.Token()) == "" {
		t.Skip("NOT RUN: set GAME_NARRATOR_GATEWAY_URL and the token env named by " +
			providers.EnvGatewayTokenEnv + " to run the live speech session test")
	}
	if direct == "" {
		t.Skip("NOT RUN: set GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL (or GAME_NARRATOR_TTS_BASE_DIRECT_URL) to the direct Base service to run the live speech session test")
	}
	return design, clone, direct, true
}

func TestLiveSpeechSession(t *testing.T) {
	designCfg, cloneCfg, directURL, ok := liveSpeechEnv(t)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)

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

	// 1. Design one real audition from the fixed synthetic line — the
	// narrator's reference.
	const brief = "A calm narrator voice, warm and unhurried, for an original synthetic character."
	line := voices.SyntheticAuditionLines[0]
	start := time.Now()
	reference, err := designClient.Design(ctx, tts.DesignRequest{Text: line, Instructions: brief, Language: "English"})
	if err != nil {
		t.Fatalf("designing the narrator reference: %v", err)
	}
	t.Logf("designed reference: model=%s duration=%s latency=%s (wall %s)",
		reference.Model, reference.Duration, reference.Latency, time.Since(start).Round(time.Millisecond))

	// 2. Approve it (the canonical reference with the real consent record)
	// and register exactly one gn_test_ name, removed on the way out.
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

	// 3. The session environment: real store, real assets, the shared TTS
	// cache over the real clone client, the file sink as evidence. The clip
	// is the synthetic fixture re-stamped at 5× its recorded source cadence
	// (pacedLiveClip): at Speed 1.0 each textbox then stays up 5× its
	// recorded dwell, so the measured real-TTS round trip (~1 s) fits before
	// the fence moves or the session ends.
	corpusPresent(t)
	replayRoot := t.TempDir()
	liveClip := pacedLiveClip(t, replayRoot)
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
	// *tts.Client satisfies the design and base seams and *tts.Registry the
	// registry seam, so the real clients are the service's collaborators.
	voiceService := &voices.Service{
		Store: st, Assets: assetStore,
		Design: designClient, Base: cloneClient, Registry: registry,
		Speech:        cache,
		InstanceID:    "primary",
		BaseModelID:   cloneCfg.Model,
		BackendDigest: "live-session-v1",
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
		// the Speed-0 drain would outrun the real TTS and fence every
		// utterance; at 1.0 the first committed line's audio has its
		// (5× recorded) dwell to land before the fence moves.
		ReplaySpeed: 1.0,
		ReplayRoots: []string{replayRoot},
		OCR:         &fakeOCR{script: truthScript(t), delay: 10 * time.Millisecond},
		Speech: &runtime.SpeechOptions{
			Voices:      voiceService,
			Assets:      assetStore,
			Sink:        runtime.SinkFile,
			FileSinkDir: sinkDir,
		},
		Log: slogDiscard(),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(mgr.Close)

	// 4. Run the replay clip end to end: the committed lines must resolve
	// the real registered voice and write real WAV evidence.
	prof, _ := seedSyntheticProfile(t, st, true)
	info, err := mgr.Start(ctx, runtime.StartRequest{
		ProfileID:   prof,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: liveClip},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	deadline := time.Now().Add(9 * time.Minute)
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

	// 5. The evidence and the assertions. The replay paces real time from
	// the clip's 5× re-stamped deltas (ReplaySpeed 1.0, pacedLiveClip), so
	// the first committed line has a dwell — 5× the recorded one — for the
	// real TTS round trip and a playback to land before the fence moves or
	// the session ends. The dwell-vs-round-trip comparison is logged below
	// for each committed line. The floor asserted is: at least the first
	// committed line reaches speech.ended {result:"ended"} (a stop at
	// session end may interrupt the playing clip, reason "interrupted"; the
	// fence still forbids a stale start) and the file sink wrote the
	// delivered samples.
	evs, err := mem.After(ctx, 0, events.Filter{SessionID: info.ID}, 4096)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	facts := liveTimeline(t, evs, lineDwell(liveClip))
	if len(facts) == 0 {
		t.Fatal("no committed line: the session committed nothing")
	}
	for _, f := range facts {
		note := ""
		if f.dwellMS > 0 && f.roundTrip > 0 && int64(f.roundTrip/time.Millisecond) > f.dwellMS {
			note = " — the recorded dwell is shorter than the real TTS round trip: the fence decides whether this playback survives"
		}
		t.Logf("line %q: dwell %d ms (clip paced %d× the recorded cadence); committed at %s; real TTS round trip %s%s",
			f.line, f.dwellMS, liveClipPace, f.commitAt.Format("15:04:05.000"), f.roundTrip.Round(time.Millisecond), note)
	}
	first := facts[0]
	var firstEnded, firstVoice, firstStarted bool
	for _, ev := range evs {
		if ev.Name != events.SpeechEnded && ev.Name != events.SpeechStarted {
			continue
		}
		var p map[string]any
		if err := jsonUnmarshal(ev.Data, &p); err != nil {
			continue
		}
		if gen, _ := p["generation"].(float64); int64(gen) != first.gen {
			continue
		}
		if ev.Name == events.SpeechStarted {
			firstStarted = true
			t.Logf("playback started: generation=%d trace_id=%s (real TTS, voice %s)", first.gen, ev.TraceID, name)
		}
		if p["result"] == "ended" {
			firstEnded = true
			t.Logf("first line ended: generation=%d result=%q reason=%v", first.gen, p["result"], p["reason"])
		}
		if p["voice_id"] == profileID {
			firstVoice = true
		}
	}
	if !firstStarted {
		t.Logf("note: no speech.started for the first line — its TTS round trip outlived the fence or the session")
	}
	if !firstEnded {
		t.Fatalf("the first committed line (generation %d, %q) never reached speech.ended {result:\"ended\"}", first.gen, first.line)
	}
	if !firstVoice {
		t.Fatalf("no speech event for the first line carries the narrator profile %s: a substitute voice spoke", profileID)
	}
	// The file sink writes the delivered prefix when the playback ends; the
	// scheduler's stop is bounded, so poll for the evidence with a grace.
	var wavPath string
	var wavErr error
	by := time.Now().Add(10 * time.Second)
	for {
		wavPath, wavErr = genWAV(sinkDir, first.gen)
		if wavErr == nil || time.Now().After(by) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if wavErr != nil {
		t.Fatalf("no WAV evidence for the first line's generation %d: %v", first.gen, wavErr)
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
		t.Fatalf("evidence WAV has %d frames at %d Hz; want delivered samples (a started playback delivers ≥ one 10 ms slice)", pcm.Frames(), pcm.SampleRate)
	}
	delivered := time.Duration(pcm.Frames()) * time.Second / time.Duration(pcm.SampleRate)
	t.Logf("evidence WAV: %s bytes=%d sample_rate=%d frames=%d (~%s delivered) — real TTS audio, not a stub",
		filepath.Base(wavPath), len(b), pcm.SampleRate, pcm.Frames(), delivered.Round(time.Millisecond))
}

// seedLiveNarratorVoice leaves exactly the rows the approve-then-register
// flow leaves: the reference bytes committed to this process's asset store,
// one approved narrator revision (fingerprint and frozen reference — the
// speech pipeline resolves it), and one ready registration on this
// instance carrying the real gn_test_ name the registry now holds.
func seedLiveNarratorVoice(t *testing.T, st *store.Store, a assets.Store, refWAV []byte, transcript, refSHA, fingerprint, model, name string) string {
	t.Helper()
	ctx := context.Background()
	staged, _, err := a.PutStaged(ctx, bytes.NewReader(refWAV), refSHA)
	if err != nil {
		t.Fatalf("staging the reference: %v", err)
	}
	committed, err := a.Commit(ctx, staged)
	if err != nil {
		t.Fatalf("committing the reference: %v", err)
	}
	if committed.SHA256 != refSHA {
		t.Fatalf("reference sha %s, want %s", committed.SHA256, refSHA)
	}
	now := time.Now().UTC()
	id := games.NewID()
	profile := store.VoiceProfile{
		ID: id, Character: "narrator", Revision: 1,
		State:             store.VoiceStateApproved,
		DesignInstruction: "A calm narrator voice, warm and unhurried, for an original synthetic character.",
		ReferenceAssetID:  refSHA,
		RefTranscript:     transcript,
		ModelID:           model,
		Language:          "English",
		ConsentProvenance: liveConsentRecord,
		ApprovedBy:        "live-speech-test",
		ApprovedAt:        &now,
		Fingerprint:       fingerprint,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := st.CreateVoiceRevision(ctx, profile); err != nil {
		t.Fatalf("creating the narrator revision: %v", err)
	}
	if err := st.UpsertVoiceRegistration(ctx, store.VoiceRegistration{
		VoiceProfileID: id, InstanceID: "primary",
		RegisteredName: name, State: store.VoiceRegReady, LastVerifiedAt: &now,
	}); err != nil {
		t.Fatalf("recording the registration: %v", err)
	}
	return id
}

// slogDiscard is a discard logger for the session under test: the events
// store is the evidence, not the process log.
func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func jsonUnmarshal(data []byte, out any) error { return json.Unmarshal(data, out) }

// pacedLiveClip derives the live session's clip from the synthetic fixture:
// the same frames and the same lines, re-stamped at liveClipPace× the
// recorded source cadence. At Speed 1.0 the replay then paces real time from
// the re-stamped deltas, and each textbox stays up liveClipPace× its
// recorded dwell (line "a" 3.0 s, line "b" 2.0 s) — long enough for the
// measured real-TTS round trip (~1 s) to land before the fence moves or the
// session ends. The frame-replay gate keeps running the original 100 ms clip
// at Speed 0 in the unit tests: that run is a pure function of the frames
// and the engine, never of the wall clock (docs/spec/12); this live run is
// about the audio half, and its OCR stays the scripted truth seam.
const liveClipPace = 5

func pacedLiveClip(t *testing.T, root string) string {
	t.Helper()
	src := syntheticClipDir()
	dst := filepath.Join(root, "live-speech-clip")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatalf("clip dir: %v", err)
	}
	entries, err := os.ReadFile(filepath.Join(src, "timestamps.json"))
	if err != nil {
		t.Fatalf("timestamps: %v", err)
	}
	var ts []map[string]any
	if err := json.Unmarshal(entries, &ts); err != nil {
		t.Fatalf("timestamps parse: %v", err)
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
	if b, err := json.MarshalIndent(ts, "", " "); err == nil {
		os.WriteFile(filepath.Join(dst, "timestamps.json"), b, 0o600)
	}
	truthRaw, err := os.ReadFile(filepath.Join(src, "truth.json"))
	if err != nil {
		t.Fatalf("truth: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(truthRaw, &rows); err != nil {
		t.Fatalf("truth parse: %v", err)
	}
	for _, r := range rows {
		if n, ok := r["ts_ns"].(float64); ok {
			r["ts_ns"] = int64(n) * liveClipPace
		}
	}
	if b, err := json.MarshalIndent(rows, "", " "); err == nil {
		os.WriteFile(filepath.Join(dst, "truth.json"), b, 0o600)
	}
	return dst
}

// liveLineDwell is one line's recorded textbox window on the clip's source
// clock: from its first full-line frame until the box leaves it (the next
// line appears, or the box is dismissed).
type liveLineDwell struct {
	line    string
	startNS int64
	endNS   int64
}

func (d liveLineDwell) ms() int64 { return (d.endNS - d.startNS) / int64(time.Millisecond) }

// lineDwell reads the clip's ground truth and reports each line's recorded
// dwell in clip order.
func lineDwell(dir string) []liveLineDwell {
	truth, err := scenario.LoadTruth(dir)
	if err != nil {
		return nil
	}
	var out []liveLineDwell
	var cur liveLineDwell
	for _, row := range truth {
		if row.Full && row.Line != "" {
			if row.Line != cur.line {
				if cur.line != "" {
					cur.endNS = row.TsNS
					out = append(out, cur)
				}
				cur = liveLineDwell{line: row.Line, startNS: row.TsNS}
			}
		} else if cur.line != "" {
			cur.endNS = row.TsNS
			out = append(out, cur)
			cur = liveLineDwell{}
		}
	}
	if cur.line != "" {
		cur.endNS = truth[len(truth)-1].TsNS
		out = append(out, cur)
	}
	return out
}

// liveLineFact is one committed line's measured timeline: the recorded
// dwell, the commit's wall time, and the real TTS round trip between the
// session's own speech.requested and speech.ready.
type liveLineFact struct {
	line      string
	gen       int64
	dwellMS   int64
	commitAt  time.Time
	roundTrip time.Duration
}

// liveTimeline joins the session's event log with the clip's recorded line
// order (commit N ↔ line N).
func liveTimeline(t *testing.T, evs []events.Envelope, dwells []liveLineDwell) []liveLineFact {
	t.Helper()
	var order []int64
	commitAt := map[int64]time.Time{}
	reqAt, readyAt := map[int64]time.Time{}, map[int64]time.Time{}
	for _, ev := range evs {
		var p map[string]any
		if err := jsonUnmarshal(ev.Data, &p); err != nil {
			continue
		}
		gen, _ := p["generation"].(float64)
		g := int64(gen)
		switch ev.Name {
		case events.MessageCommitted:
			order = append(order, g)
			commitAt[g] = ev.OccurredAt
		case events.SpeechRequested:
			if _, ok := reqAt[g]; !ok {
				reqAt[g] = ev.OccurredAt
			}
		case events.SpeechReady:
			if _, ok := readyAt[g]; !ok {
				readyAt[g] = ev.OccurredAt
			}
		}
	}
	out := make([]liveLineFact, 0, len(order))
	for i, g := range order {
		f := liveLineFact{line: fmt.Sprintf("line#%d", i+1), gen: g, commitAt: commitAt[g]}
		if i < len(dwells) {
			f.line = dwells[i].line
			f.dwellMS = dwells[i].ms()
		}
		if r, ok := reqAt[g]; ok {
			if y, ok := readyAt[g]; ok {
				f.roundTrip = y.Sub(r)
			}
		}
		out = append(out, f)
	}
	return out
}

// genWAV finds the file-sink WAV written for one generation (the sink names
// its files gen<N>-…).
func genWAV(root string, gen int64) (string, error) {
	var found string
	prefix := fmt.Sprintf("gen%d-", gen)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || !strings.HasSuffix(p, ".wav") {
			return err
		}
		if strings.HasPrefix(filepath.Base(p), prefix) {
			found = p
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("no WAV for generation %d under %s", gen, root)
	}
	return found, nil
}
