// Package e2e — helpers for the live catch-up narration E2E (issue #25,
// ADR-0122): the voice workflow, the session settings, the rapid-A drive
// and the assertion-side model of the correlated speech stream. Log-safe
// per AGENTS.md rule 5: log lines and bundle summaries carry counts, ids,
// hashes and timings — never dialogue text, speaker key values or tokens.
// The raw payloads (the event stream, the dev-trace frames and the WAVs)
// ride on the gitignored evidence bundle only.

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/asr"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	rt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
	"github.com/dbirks/game-narrator/internal/voices"
)

// cuSetupVoices builds the voice workflow the session resolves voices
// through (the same wiring cmd/game-narrator builds, the per-character
// test's shape): the real design and Base clients from the roles, the real
// Base registry, the speech cache with its ASR QA gate (wired when the
// speech_asr role is configured — the product shape; a QA-less run is a
// logged deviation, not a skip) and the two registered job kinds the API
// routes enqueue onto.
func cuSetupVoices(t *testing.T, roles map[providers.Role]providers.RoleConfig, runner *jobs.Runner, adapters *adapter.Registry, designCfg, cloneCfg, asrCfg providers.RoleConfig, directURL string) {
	t.Helper()
	designClient, err := tts.NewFromRole(designCfg)
	if err != nil {
		t.Fatalf("voice design client: %v", err)
	}
	cloneClient, err := tts.NewFromRole(cloneCfg)
	if err != nil {
		t.Fatalf("voice clone client: %v", err)
	}
	cuV.registry, err = tts.NewRegistry(directURL, cloneCfg.Token())
	if err != nil {
		t.Fatalf("voice registry: %v", err)
	}
	speechCache := speech.New(cuV.as, store.NewSpeechCacheIndex(cuV.st), 8, speech.WithReferenceChecker(cuV.st))
	cuV.service = &voices.Service{
		Store:         cuV.st,
		Assets:        cuV.as,
		Design:        designClient,
		Base:          cloneClient,
		Registry:      cuV.registry,
		Speech:        speechCache,
		InstanceID:    voices.DefaultInstanceID,
		BaseModelID:   cloneCfg.Model,
		BackendDigest: previewDigest(cloneCfg.Model),
	}
	// The ASR QA gate (FR-10, ADR 0061): on every cache insert the clip is
	// read back by the real ASR endpoint and scored against its line; a
	// failed verdict regenerates once. When the role is unconfigured the
	// QA degrades to not-run (logged; the acceptance here is the catch-up
	// shape and the voices, not the QA verdict).
	if asrCfg.Configured() {
		asrClient, err := asr.NewFromRole(asrCfg)
		if err != nil {
			t.Fatalf("speech_asr client: %v", err)
		}
		cuV.service.ASR = asrClient
		cuV.service.QA = speech.NewQAEngine(speechCache, asrClient)
		t.Logf("catchup e2e: ASR QA wired (model %s): every generated clip is read back before it plays", asrCfg.Model)
	} else {
		asrEnv, _, _, _ := providers.RoleEnvVars(providers.RoleSpeechASR)
		t.Logf("catchup e2e: ASR QA NOT CONFIGURED (set %s): clips play without the read-back gate (a logged deviation, not a skip)", asrEnv)
	}
	if err := voices.New(cuV.service); err != nil {
		t.Fatalf("voice service: %v", err)
	}
	// The voice.auditions and import.file job kinds the API routes enqueue
	// onto (the per-character test's same wiring).
	if err := runner.Register(voices.JobKind, voices.JobHandler(cuV.service), voices.JobHandlerOpts()); err != nil {
		t.Fatalf("register %s: %v", voices.JobKind, err)
	}
	if err := runner.Register(api.JobKindImportFile,
		api.ImportFileJobHandler(cuV.st, cuV.as, adapters), jobs.RegisterOpts{
			Concurrency: 1, Resumable: true, Timeout: 5 * time.Minute,
		}); err != nil {
		t.Fatalf("register %s: %v", api.JobKindImportFile, err)
	}
}

// cuVoiceCleanup removes this run's gn_test_ registry names from the real
// Base registry (ADR 0022): the bounded cleanup the per-character test
// runs, one 60 s budget for the whole set.
func cuVoiceCleanup(t *testing.T, runID string) {
	t.Helper()
	if cuV.registry == nil {
		return
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer ccancel()
	for _, name := range cuV.registered {
		if name == "" {
			continue
		}
		if err := cuV.registry.Remove(cctx, name); err != nil {
			t.Logf("catchup e2e: voice cleanup: %s: %v (voice left on the Base registry)", name, err)
		} else {
			t.Logf("catchup e2e: voice cleanup: %s removed from the Base registry", name)
		}
	}
	_ = runID
}

// cuDesignVoice runs the product's voice workflow end to end for one
// character (the per-character test's pcDesignVoice, tracked into cuV so
// the cleanup removes both voices) and returns the outcome.
func cuDesignVoice(t *testing.T, ctx context.Context, srv http.Handler, token string, st *store.Store, as *assets.Local, reg *tts.Registry, character, brief, designModel, cloneModel string) pcVoiceOutcome {
	t.Helper()
	out := pcDesignVoice(t, ctx, srv, token, st, as, reg, character, brief, designModel, cloneModel)
	cuV.registered = append(cuV.registered, out.Registered)
	return out
}

// cuOCRFallback builds the VLM OCR fallback (ADR-0040) when the role is
// configured; nil otherwise (the session runs the local engine only, the
// per-character test's documented degradation).
func cuOCRFallback(t *testing.T, roles map[providers.Role]providers.RoleConfig) *pipeline.OCRFallback {
	t.Helper()
	cfg := roles[providers.RoleVisionOCRFallback]
	if !cfg.Configured() {
		vlmEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVisionOCRFallback)
		t.Logf("catchup e2e: VLM OCR fallback NOT CONFIGURED (set %s); the session runs the local tesseract engine only", vlmEnv)
		return nil
	}
	client, err := openai.NewFromRole(cfg)
	if err != nil {
		t.Fatalf("vision_ocr_fallback client: %v", err)
	}
	t.Logf("catchup e2e: VLM OCR fallback ready (model %s)", cfg.Model)
	return &pipeline.OCRFallback{Secondary: ocr.NewVLMEngine(ocr.OpenAIClient{Client: client})}
}

// cuDecisionOpts builds the Clef (System One) adjudication wiring
// (ADR-0050): not a skip gate — unconfigured, the session runs local and
// the decision stage is a logged deviation.
func cuDecisionOpts(t *testing.T, roles map[providers.Role]providers.RoleConfig) *rt.DecisionOptions {
	t.Helper()
	cfg := roles[providers.RoleDecision]
	if !cfg.Configured() {
		decEnv, _, _, _ := providers.RoleEnvVars(providers.RoleDecision)
		t.Logf("catchup e2e: decision role NOT CONFIGURED (set %s); the session runs local adjudication", decEnv)
		return nil
	}
	roleReg := providers.NewRegistry(roles)
	chatRouter := router.New(roleReg, router.Options{})
	t.Logf("catchup e2e: Clef decision role configured (model %s, protocol %s)", cfg.Model, cfg.Protocol)
	// Production margin (cmd/game-narrator/main.go runs Margin 0): a confident
	// local match commits on the local evidence and skips Clef entirely — only
	// an abstained or fuzzy-below-bar ranking consults Clef. This is the shape
	// the catch-up scenario asserts against (the multi-box lines are read and
	// committed, not every one held for a Clef round trip). dolphin_live_test.go
	// keeps dolphinClefMargin on purpose: it asserts a Clef request on the
	// opening line.
	return &rt.DecisionOptions{Router: chatRouter, Role: cfg, Timeout: dolphinClefTimeout, Margin: 0}
}

// cuSetSettings applies the session's catch-up policy through the settings
// control (ADR-0122 decision 1) and returns the applied snapshot: the
// partial-body PUT with only the policy word (the other fields keep their
// current values), so the answer's catch_up field is the applied word the
// speech side and the developer UI cannot disagree on.
func cuSetSettings(t *testing.T, srv http.Handler, token, sessionID, policy string) map[string]any {
	t.Helper()
	rec := cuDo(t, srv, token, http.MethodPut, "/api/v1/sessions/"+sessionID+"/settings",
		mustMarshal(t, map[string]any{"catch_up": policy}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session settings (catch_up=%s) = %d: %s", policy, rec.Code, rec.Body.String())
	}
	var snap map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("settings snapshot decode: %v", err)
	}
	return snap
}

// snapshotCatchUp reads the session snapshot's applied catch-up policy
// word (the snapshot field ADR-0122 decision 1 requires).
func snapshotCatchUp(snap map[string]any) string {
	v, _ := snap["catch_up"].(string)
	return v
}

// cuDo is the authenticated in-process request helper (dolphinDo, the
// catch-up test's alias so the helpers read as one set).
func cuDo(t *testing.T, srv http.Handler, token, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return dolphinDo(t, srv, token, method, target, body, headers)
}

// cuScene is the built first-dialogue scene: the published profile (the box
// in BOTH of its measured on-screen positions, the imported script, the
// linked speaker key) and the character the scene's lines resolve to.
type cuScene struct {
	gameID    string
	editionID string
	profileID string
	key       string // the scene's dominant speaker key (counts/ids in logs only)
	charID    string // the character the speaker link targets
}

// The box's two measured on-screen positions (the per-character test's
// proven live configuration: the Wind Waker box tracks the speaker, and
// through the opening conversation it sat HIGH in the 2026-10-20 probe
// while the fixture recorded it LOW — the profile carries both, never
// visible at once, like the box-vs-legend overlap the single-region
// profiles keep apart).
const (
	cuBoxRegionX     = 0.05
	cuBoxRegionY     = 0.62
	cuBoxRegionW     = 0.80
	cuBoxRegionH     = 0.25
	cuBoxHighRegionX = 0.05
	cuBoxHighRegionY = 0.25
	cuBoxHighRegionW = 0.80
	cuBoxHighRegionH = 0.37
)

// cuSetupScene builds the scene profile the per-character test's
// setupScene builds (game → edition → draft → import → character →
// speaker link → the box's two positions → publish), through the API the
// way the product's agent writes them: the script is imported from the
// disc image (the import attaches the BMG's per-line speaker keys, ADR
// 0070), the speaker key is identified from the fixture's matched lines
// mapped onto the parsed script, the curator's speaker link re-points the
// key's lines at the scene character, and the profile publishes (only a
// published revision runs).
func cuSetupScene(t *testing.T, ctx context.Context, srv http.Handler, token string, st *store.Store, bmgBytes []byte, imageSHA, fdDir, runID string) cuScene {
	t.Helper()
	var s cuScene

	code, out := cuDoJSON(t, srv, token, http.MethodPost, "/api/v1/games", map[string]any{
		"slug": "wind-waker-catchup", "title": "The Legend of Zelda: The Wind Waker",
		"platform": "gamecube", "user_tags": json.RawMessage(`[]`),
	}, map[string]string{"Idempotency-Key": runID + "-game"})
	if code != http.StatusCreated {
		t.Fatalf("create game = %d: %v", code, out)
	}
	s.gameID, _ = out["id"].(string)
	code, out = cuDoJSON(t, srv, token, http.MethodPost, "/api/v1/games/"+s.gameID+"/editions", map[string]any{
		"region": "ntsc-u", "language": "en", "revision": "1", "source_identity": imageSHA,
	}, map[string]string{"Idempotency-Key": runID + "-edition"})
	if code != http.StatusCreated {
		t.Fatalf("create edition = %d: %v", code, out)
	}
	s.editionID, _ = out["id"].(string)
	code, out = cuDoJSON(t, srv, token, http.MethodPost, "/api/v1/editions/"+s.editionID+"/profiles", nil,
		map[string]string{"Idempotency-Key": runID + "-profile"})
	if code != http.StatusCreated {
		t.Fatalf("create draft profile = %d: %v", code, out)
	}
	s.profileID, _ = out["id"].(string)
	t.Logf("catchup e2e: catalog ready: game=%s edition=%s profile=%s", s.gameID, s.editionID, s.profileID)

	// The script's speaker sidecar, parsed in-process from the same BMG
	// bytes the import uploads (counts and key ids only, rule 5).
	parsed := cuParseBMG(t, ctx, bmgBytes, s.profileID)

	// Import the script from the disc image through the API.
	rec := dolphinDoMultipart(t, srv, token, "/api/v1/profiles/"+s.profileID+"/import",
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
		Inserted int `json:"messages_inserted"`
	}
	if err := json.Unmarshal(job.Result, &importReport); err != nil {
		t.Fatalf("import job result decode: %v", err)
	}
	if importReport.Inserted < 1000 {
		t.Fatalf("import inserted only %d messages; the real Wind Waker script is a large set (>=1000)", importReport.Inserted)
	}
	t.Logf("catchup e2e: import done: job=%s inserted=%d (speaker keys attached per ADR 0070)", up.JobID, importReport.Inserted)

	// The scene's speaker key, from the fixture's matched lines mapped onto
	// the parsed sidecar (the bench's dominant-key rule, rule 5: counts and
	// the masked key only).
	key, matched, distinct := cuDominantKey(t, fdDir, parsed)
	s.key = key
	t.Logf("catchup e2e: speaker key %s (%d lines matched the dominant key, %d distinct keys)", maskPCKey(key), matched, distinct)

	// The target character (the product's roster write, as the agent's
	// stage_character writes it: a proposed row the curator then links).
	char := games.Character{
		ID: games.NewID(), ProfileID: s.profileID,
		Name: "catchup first-dialogue speaker", Role: "npc",
		Status:    games.CharacterProposed,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := st.PutCharacters(ctx, s.profileID, []games.Character{char}); err != nil {
		t.Fatalf("put character: %v", err)
	}
	s.charID = char.ID

	// The curator's speaker link (draft-only): re-points the key's lines at
	// the character.
	repointed := cuLinkSpeaker(t, srv, token, s.profileID, s.key, char.ID)
	t.Logf("catchup e2e: speaker link recorded: %d lines re-pointed (draft revision)", repointed)

	// The box's two measured positions, through the regions API (the
	// draft-only write carries the profile's row version as If-Match).
	regionBody := map[string]any{"regions": []map[string]any{
		{"label": "dialogue box", "region_type": "textbox",
			"x": cuBoxRegionX, "y": cuBoxRegionY, "w": cuBoxRegionW, "h": cuBoxRegionH},
		{"label": "dialogue box high", "region_type": "textbox",
			"x": cuBoxHighRegionX, "y": cuBoxHighRegionY, "w": cuBoxHighRegionW, "h": cuBoxHighRegionH},
	}}
	etag := cuProfileETag(t, srv, token, s.profileID)
	rec = cuDo(t, srv, token, http.MethodPut, "/api/v1/profiles/"+s.profileID+"/regions",
		mustMarshal(t, regionBody), map[string]string{"If-Match": etag})
	if rec.Code != http.StatusOK {
		t.Fatalf("put region = %d: %s", rec.Code, rec.Body.String())
	}

	// Draft → review → publish (only the published revision runs).
	etag = cuProfileETag(t, srv, token, s.profileID)
	rec = cuDo(t, srv, token, http.MethodPost, "/api/v1/profiles/"+s.profileID+"/status",
		[]byte(`{"to":"review"}`), map[string]string{"If-Match": etag})
	if rec.Code != http.StatusOK {
		t.Fatalf("set status review = %d: %s", rec.Code, rec.Body.String())
	}
	etag = cuProfileETag(t, srv, token, s.profileID)
	rec = cuDo(t, srv, token, http.MethodPost, "/api/v1/profiles/"+s.profileID+"/publish",
		[]byte(`{}`), map[string]string{"If-Match": etag})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish profile = %d: %s", rec.Code, rec.Body.String())
	}
	t.Logf("catchup e2e: profile %s published (the box's two positions, speaker key %s linked to %s)",
		s.profileID, maskPCKey(s.key), s.charID)
	return s
}

// cuDoJSON is cuDo with a JSON body and a decoded response object (nil
// body for GETs; the response decodes to a map for the caller's picks).
func cuDoJSON(t *testing.T, srv http.Handler, token, method, target string, v any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var body []byte
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s %s: %v", method, target, err)
		}
		body = b
	}
	rec := cuDo(t, srv, token, method, target, body, headers)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// cuParseBMG, cuDominantKey, cuLinkSpeaker and cuProfileETag are the
// per-character test's script-side helpers (same package): the parse
// carries the speaker sidecar, the dominant key maps the fixture's matched
// lines onto it with the bench's rule, the speaker link re-points the
// key's lines, and the ETag helper carries the draft write's If-Match
// token. They are aliased here so this file's setup reads as one flow.
var (
	cuParseBMG    = pcParseBMG
	cuDominantKey = pcDominantKey
	cuLinkSpeaker = pcLinkSpeaker
	cuProfileETag = pcProfileETag
)

// cuCommitInfo is one committed, speech-eligible line the driver knows
// about (matched message id; the read-through lines are not box progress —
// they commit on the non-box region's noise and never name a box).
type cuCommitInfo struct {
	Occurrence  string
	MessageID   string
	Generation  int64
	Text        string
	CommittedAt time.Time
	Terminal    bool // a speech terminal event for this occurrence has arrived
}

// cuIsMesaNoise reports whether one observed box-region reading is the
// llvmpipe software renderer's GL info string rather than the game's
// dialogue box. On the dev host (llvmpipe, no GPU) Dolphin's renderer init
// prints its vendor/renderer/version block into the framebuffer for the
// first ~30 s after a savestate load, and the box region's OCR reads it
// (the 2026-10-21 live runs: "Mesa, llvmpipe (LLVM …), 4.6 (Core …)"). It
// is not a dialogue box: it must not drive the driver's box clocks (the
// stability/stall/box-gone signals) or be counted as box progress — it
// clears on its own once the game renders the real box. The check is a
// bounded substring scan over renderer-info tokens (no game content).
func cuIsMesaNoise(text string) bool {
	if len(text) < 8 {
		return false
	}
	low := strings.ToLower(text)
	for _, tok := range []string{"llvmpipe", "mesa", "llvm ", "core profile", "gl_version", "renderer"} {
		if strings.Contains(low, tok) {
			return true
		}
	}
	return false
}

// cuDriver is the rapid-A advance driver: the headless session's stand-in
// for the player, pressing A the way the fixture recordings' input logs
// show it was pressed — one press per box — but FASTER than the speech:
// the press lands as soon as the box's line commits (the commit is the
// evidence the box's text is settled and typed out, the measured 2026-10-20
// run's first line committed 1.8 s into the session), while the previous
// line's TTS round trip, the ASR QA gate and the playback are still in
// the air — the batch the catch-up policy exists to speak. The fallback
// presses keep the conversation moving when a box never commits (the
// settled-but-unmatched box: the stall press) or when a press was lost
// (the bounded retry): a press that leaves the box text unchanged for
// cuLostA is a lost A, retried up to cuMaxPresses per box instance.
type cuDriver struct {
	started      time.Time
	boxRegion    string // the region that commits real lines (learned)
	lastText     string // the newest observed box text
	textChanged  time.Time
	boxEmpty     time.Time       // when the box region last read empty (zero: never)
	instAppeared time.Time       // when the current box instance's text appeared
	commits      []cuCommitInfo  // the ELIGIBLE matched lines (the press accounting's evidence: the lines the session will speak)
	matched      []cuCommitInfo  // every MATCHED commit, eligible or abstained (the rapid-press driver: a matched commit is box progress — the box moved, press to advance while the speech is still busy)
	routed       map[string]bool // every Routed line's occurrence (the settle accounting: the eligible commits plus the read-through lines the wiring routed from the speech.requested's without message_ids — a fenced or still-in-flight line that never terminalled is a lost line, not a settled one)
	terminal     map[string]bool // the routed occurrences that reached a terminal (speech.ended with a result, the wiring's one-settle-terminal rule)
	pressedMsgs  map[string]bool // message ids already pressed for (one press per line)
	instPresses  int             // presses made on the current box instance
	done         bool
}

// cuNewDriver arms the driver at the session's start (the box text is
// "unchanged" from the start: the first observation changes it).
func cuNewDriver(now time.Time) *cuDriver {
	return &cuDriver{
		started: now, textChanged: now, instAppeared: now,
		pressedMsgs: map[string]bool{},
		routed:      map[string]bool{},
		terminal:    map[string]bool{},
	}
}

// cuCommitAt returns the CommittedAt of one committed line (zero when the
// driver never saw it — the reconciled stream read may have dropped the
// event, in which case the caller uses the event's own occurred_at).
func (d *cuDriver) commitAt(occ string) time.Time {
	for _, c := range d.matched {
		if c.Occurrence == occ {
			return c.CommittedAt
		}
	}
	return time.Time{}
}

// cuObserve folds one poll's new events into the driver's view: the box
// region's text (the stability and box-gone signals, the per-character
// test's learned-region rule: once a real commit names the region, only
// that region's readings count — before it, only line-length readings,
// so the non-box noise fragments do not churn the clocks) and the
// committed eligible lines (the box progress).
func (d *cuDriver) observe(evs []devEnvelope) {
	now := time.Now()
	for _, e := range evs {
		var d2 struct {
			Occurrence string   `json:"dialogue_occurrence_id"`
			MessageID  string   `json:"message_id"`
			MessageIDs []string `json:"message_ids"`
			Region     string   `json:"region_id"`
			Generation int64    `json:"generation"`
			Text       string   `json:"text"`
			Eligible   bool     `json:"speech_eligible"`
			Result     string   `json:"result"`
		}
		if len(e.Data) == 0 {
			continue
		}
		if err := json.Unmarshal(e.Data, &d2); err != nil {
			continue
		}
		switch e.Name {
		case "text.observed":
			count := false
			if d.boxRegion != "" {
				count = d2.Region == d.boxRegion
			} else if utf8.RuneCountInString(d2.Text) >= 12 {
				count = true
			}
			if !count {
				continue
			}
			// The llvmpipe renderer's GL info string is not a dialogue box: on
			// the dev host it is drawn over the frame for the first seconds
			// after a savestate load and the box region's OCR reads it (the
			// 2026-10-21 runs' "Mesa, llvmpipe (LLVM …)" readings; the 2026-10-21
			// frame probe places it at the frame's top band, where the
			// placement-aware band selection can crop it). It must not set the
			// box's text or churn the stability clocks: treat it as an empty
			// reading, the way a genuinely empty box read does.
			if d2.Text != "" && cuIsMesaNoise(d2.Text) {
				d2.Text = ""
			}
			if d2.Text == "" {
				if d.boxEmpty.IsZero() {
					d.boxEmpty = now
				}
				continue
			}
			d.boxEmpty = time.Time{} // the box is visible again
			if d2.Text != d.lastText {
				d.lastText = d2.Text
				d.textChanged = now
				d.instAppeared = now
				d.instPresses = 0 // a new box instance
			}
		case "message.committed":
			if d2.MessageID == "" || d2.MessageID == "unknown" {
				continue
			}
			// A MATCHED commit — eligible or abstained — is box progress:
			// the box's line settled, and the rapid press advances the box on
			// it. Eligibility decides only whether the line carries speech
			// (the eligible matched lines are the settle accounting, every one
			// reaching a terminal; an abstained matched line the Clef vetoed
			// speaks nothing, and an unknown-message abstained line is the
			// read-through the driver never presses on).
			if d.boxRegion == "" {
				d.boxRegion = d2.Region // learn the region that commits lines
			}
			at, err := time.Parse(time.RFC3339Nano, e.OccurredAt)
			if err != nil {
				at = now
			}
			c := cuCommitInfo{
				Occurrence: d2.Occurrence, MessageID: d2.MessageID,
				Generation: d2.Generation, Text: d2.Text, CommittedAt: at,
			}
			d.matched = append(d.matched, c)
			if d2.Eligible {
				d.commits = append(d.commits, c)
				d.routed[d2.Occurrence] = true // the eligible commit is a routed line (its speech.requested follows in the same poll or the next)
			}
		case "speech.requested":
			// A line's routed request (no message_ids — the catch-up RUN's
			// request carries them, one entry per covered line, and the run's
			// own occurrence is not a line): the read-through lines the wiring
			// routed from the OCR text are the settle accounting's other half
			// (a read-through line still in its TTS round trip when the
			// conversation ends must reach its terminal before the settle
			// phase exits, like every other line).
			if d2.Occurrence == "" || d2.MessageIDs != nil {
				continue
			}
			d.routed[d2.Occurrence] = true
		case "speech.ended", "speech.cancelled":
			if d2.Occurrence == "" {
				continue
			}
			if e.Name == "speech.ended" && d2.Result == "" {
				continue
			}
			d.terminal[d2.Occurrence] = true
		}
	}
}

// cuNoteTerminal marks one routed line settled (a speech terminal
// arrived for it): the settle phase's exit condition. observe folds the
// stream's terminal events into the same map — this entry point keeps the
// mark idempotent if a caller notes one out of band.
func (d *cuDriver) noteTerminal(occ string) {
	if occ == "" {
		return
	}
	d.terminal[occ] = true
	for i := range d.commits {
		if d.commits[i].Occurrence == occ {
			d.commits[i].Terminal = true
			return
		}
	}
}

// cuDecidePress reports whether the driver presses A now and why (the
// reason lands in the log and the bundle's timeline). The rules, in
// priority order:
//
//  1. Rapid path: a newly committed line no press was made for yet —
//     press at once, while that line's (and the previous lines') audio is
//     still in the air. One press per distinct message: the WOTW
//     conversation's boxes are distinct lines, and the rule makes a late
//     commit of an already-pressed box's line a no-op instead of a second
//     advance (the box would skip its successor).
//  2. Stall fallback: a visible box whose text has been stable for
//     cuStallGap with no commit landed for it (the line the matcher cannot
//     name, the Clef abstaining) — press anyway: the conversation moves
//     and the evidence records the unmatched box (the per-character test's
//     stall-press shape, the one a box that never commits cannot hold
//     against).
//  3. Lost-A retry: a press made for this box instance left the box text
//     unchanged for cuLostA (the A was dropped, the drive tables' measured
//     failure mode) — press again, bounded to cuMaxPresses per box
//     instance.
func (d *cuDriver) decidePress(evs []devEnvelope, now time.Time) (bool, string) {
	if d.done {
		return false, ""
	}
	// The box must be absent this long before the conversation is called
	// over: a between-box transition reads empty for ~1-9 s and a late
	// commit of the just-advanced box can land inside it — 8 s fired
	// mid-conversation on the 2026-10-21 runs' margin.
	if !d.boxEmpty.IsZero() && now.Sub(d.boxEmpty) >= cuConvGone {
		if len(d.commits) >= 1 {
			d.done = true // the conversation is over
			return false, "box gone: the conversation ended"
		}
		return false, "box not yet visible"
	}
	if !d.boxEmpty.IsZero() {
		// The box region last read empty: it is closing, or the next box has
		// not opened yet (the between-box transition, ~1 s in WOTW dialogue)
		// — a press that lands on a closed box advances whatever the A hits
		// next (the next box's line, skipped mid-typewriter). Hold: the box
		// is not on screen, so no rule fires. (The rapid rule's commit is
		// the line the OPEN box holds: by the time it commits the box is
		// visible again, and the empty read is cleared by the first
		// non-empty observation.)
		return false, "box not on screen (transitioning)"
	}
	// The rapid path runs on EVERY matched commit — the abstained ones
	// included: a matched commit is the box's line having settled, and the
	// box advances on the game side regardless of whether the session will
	// speak the line (an abstained line the Clef vetoed speaks nothing; the
	// box still waits for the player's A).
	for _, c := range d.matched {
		if !d.pressedMsgs[c.MessageID] {
			d.pressedMsgs[c.MessageID] = true
			d.instPresses++
			return true, "rapid: line " + shortID(c.MessageID) + " (gen " + itoa(c.Generation) + ") committed — press while the speech is still busy"
		}
	}
	textStable := now.Sub(d.textChanged) >= cuPressStable
	// The fallback presses only fire when the OCR actually sees box text
	// (lastText non-empty): a press into a box the OCR cannot read skips a
	// line the session will never commit (the 2026-10-21 run-3 failure
	// mode: the blind stall/lost-A presses walked the conversation ahead of
	// the OCR, and the committed lines were the conversation's tail). When
	// the OCR is blind there is nothing to advance into — the drive's
	// deadline with the partial stream and the frames is the honest failure.
	if d.lastText == "" {
		return false, ""
	}
	if d.instPresses == 0 && textStable && now.Sub(d.instAppeared) >= cuStallGap {
		d.instPresses++
		return true, "stall: the box's text has been stable without a commit for " + cuStallGap.String() + " — press to move on (the line committed unmatched or never)"
	}
	if d.instPresses > 0 && d.instPresses < cuMaxPresses && textStable && now.Sub(d.textChanged) >= cuLostA {
		d.instPresses++
		return true, "lost-A retry " + itoa(int64(d.instPresses)) + "/" + itoa(int64(cuMaxPresses)) + ": the last press left the box unchanged for " + cuLostA.String()
	}
	return false, ""
}

// shortID renders one id for a log line (rule 5: ids are allowed, values
// are not — a message id is an identifier, not game content).
func shortID(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:7] + "…"
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// cuAllSettled reports whether every routed line — the eligible commits
// and the read-through lines the wiring routed — has a terminal speech
// event (the settle phase's exit: the batch and the runs are done, nothing
// is left in the fence when the session stops). A routed line without a
// terminal is a line the policy lost (or one still in its TTS round trip
// the settle window must outlast).
func (d *cuDriver) allSettled() (int, int) {
	settled := 0
	for occ := range d.routed {
		if d.terminal[occ] {
			settled++
		}
	}
	return settled, len(d.routed)
}

// cuPress is one A press the driver made (the bundle's press timeline: the
// rhythm that made the run faster than the speech, with the rule that
// fired it).
type cuPress struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// cuDriveConversation runs the drive: the rapid-A advance over the first
// in-game conversation, then the settle wait (every committed line
// terminal) — and returns the session's whole dev event stream (the
// polled route's rows; the caller reconciles them against the store) and
// the press timeline (the bundle's evidence of the faster-than-speech
// rhythm). On the deadline it records the PARTIAL stream (the
// commit-spacing-vs-TTS-latency timeline a stuck run needs) and fails, the
// per-character test's same-shape failure evidence.
func cuDriveConversation(t *testing.T, srv http.Handler, token, sessionID string, bridge dolphin.Bridge) ([]devEnvelope, []cuPress) {
	t.Helper()
	drv := cuNewDriver(time.Now())
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	driveCutoff := time.Now().Add(10 * time.Minute) // the drive phase's deadline
	var (
		evs          []devEnvelope
		cursor       int64
		settleArmed  bool
		settleCutoff time.Time
		settledLog   = 0
		presses      []cuPress
	)
	for {
		select {
		case <-poll.C:
		}
		now := time.Now()
		page, next, err := pcPollDevEvents(t, srv, token, sessionID, cursor)
		if err != nil {
			t.Fatalf("dev events: %v", err)
		}
		evs = append(evs, page...)
		if len(page) > 0 {
			cursor = next
		}
		drv.observe(page)
		// The terminal marks (the settle phase counts on them).
		for _, e := range page {
			var p struct {
				Occ    string `json:"dialogue_occurrence_id"`
				Result string `json:"result"`
			}
			if len(e.Data) == 0 {
				continue
			}
			if err := json.Unmarshal(e.Data, &p); err != nil || p.Occ == "" {
				continue
			}
			if (e.Name == "speech.ended" && p.Result != "") || e.Name == "speech.cancelled" {
				drv.noteTerminal(p.Occ)
			}
		}
		if !drv.done {
			if now.After(driveCutoff) {
				cuWritePartialBundle(t, sessionID, evs, "the drive deadline passed: the conversation did not end (the box never cleared or the presses never landed — the partial stream and the press timeline in the bundle are the evidence)")
				t.Fatalf("session %s: the rapid-A drive did not finish the conversation within 10m (the box was still on screen or the presses were lost; the partial stream is in the bundle)", sessionID)
			}
			want, why := drv.decidePress(page, now)
			if want {
				t.Logf("catchup e2e: A press (%s)", why)
				press := cuPress{At: time.Now(), Reason: why}
				actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := bridge.Inject(actx, dolphin.Buttons{A: true}, dolphin.Axes{}, cuPressHold); err != nil {
					t.Logf("catchup e2e: A inject failed: %v (the box stays; the next poll's rules decide)", err)
				} else {
					presses = append(presses, press)
				}
				acancel()
			}
			continue
		}
		// The conversation is over: the settle phase (every committed line
		// terminal) runs its own deadline.
		if !settleArmed {
			settleArmed = true
			settleCutoff = now.Add(cuDriveSettle)
			t.Logf("catchup e2e: the conversation ended (box cleared); settling the last committed lines (window %s)", cuDriveSettle)
		}
		if now.After(settleCutoff) {
			settled, total := drv.allSettled()
			if total > 0 && settled < total {
				cuWritePartialBundle(t, sessionID, evs, fmt.Sprintf("the settle deadline passed with %d of %d committed lines unsettled (the batch or a run was still in its TTS round trip or the fence)", settled, total))
				t.Fatalf("session %s: %d committed line(s) never reached a terminal speech event within %s of the conversation's end (the partial stream is in the bundle)", sessionID, total-settled, cuDriveSettle)
			}
		}
		settled, total := drv.allSettled()
		if total > 0 && settled != settledLog {
			t.Logf("catchup e2e: settling: %d/%d committed lines terminal", settled, total)
			settledLog = settled
		}
		if total > 0 && settled == total {
			t.Logf("catchup e2e: all %d committed lines settled — collecting the rest of the stream", total)
			return evs, presses
		}
	}
}

// cuWritePartialBundle writes the pre-failure stream to the evidence
// bundle (the per-character test's same-shape partial evidence): the
// commit-spacing-vs-TTS-latency timeline a stuck run needs to be
// diagnosed, before the t.TempDir store dies with the test.
func cuWritePartialBundle(t *testing.T, sessionID string, evs []devEnvelope, why string) {
	t.Helper()
	runID := "partial-" + time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(pcRepoRoot(t), "artifacts", cuEvidenceSub, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("catchup e2e: partial bundle mkdir %s: %v", dir, err)
		return
	}
	writeJSONFile(t, filepath.Join(dir, "dev_events.json"), evs)
	writeJSONFile(t, filepath.Join(dir, "partial_reason.json"), map[string]string{"session_id": sessionID, "reason": why})
	t.Logf("catchup e2e: partial evidence written: %s (%s)", dir, why)
}

// cuReconcileStream merges the durable store's session rows into the
// polled stream (the per-character test's gap check, the same rule: the
// store read is the authority, the route gap is logged) and orders the
// merged stream by seq (the polled route's rows arrive in seq order, but
// the merged-in rows may land at the end: the assertion-side model reads
// "last terminal wins", so the stream must be in the store's truth order).
func cuReconcileStream(t *testing.T, sessionID string, evs []devEnvelope, direct []events.Envelope) []devEnvelope {
	t.Helper()
	have := map[int64]bool{}
	for _, e := range evs {
		have[e.Seq] = true
	}
	missing := 0
	for _, e := range direct {
		if !have[e.Seq] {
			evs = append(evs, devEnvelope{
				Seq: e.Seq, EventID: e.EventID, SessionID: e.SessionID, TraceID: e.TraceID,
				CausationID: e.CausationID, Name: e.Name,
				OccurredAt: e.OccurredAt.Format("2006-01-02T15:04:05.000Z"), Data: e.Data,
			})
			missing++
		}
	}
	if missing > 0 {
		t.Logf("catchup e2e: the polled dev-events route returned %d of the store's %d session rows; %d merged in from the direct store read (the assertions run on the store)",
			len(evs)-missing, len(direct), missing)
		if debugLog != nil {
			debugLog.Printf("catchup %s: reconciled %d store rows into the %d-row polled stream (route gap %d)", sessionID, missing, len(evs)-missing, missing)
		}
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
	return evs
}

// cuLine is one committed, speech-routed line in the assertion-side model:
// the commit (the message, the generation, the wall clock the line
// committed) and the terminal the policy gave it — spoken (result ended,
// by its own playback or a merged run) or fenced (result skipped, the
// catch-up reason: the window drop, the max-age drop, the rescue's batch
// hand-off). A read-through line (message_id "unknown", the unmatched box
// the session's policy speaks from its OCR text) is a line like any other:
// unattributed, the narrator's by the resolveVoice rule.
type cuLine struct {
	Occurrence  string
	MessageID   string // "unknown" for the read-through lines
	RegionID    string
	Generation  int64
	Text        string // the committed text (the script line; the read-through's cleaned OCR)
	CharacterID string // the resolved speaker (the speech events' character_id; "narrator" when unattributed)
	CommittedAt time.Time
	RequestedAt time.Time
	ReadyAt     time.Time
	StartedAt   time.Time
	// The line's settled terminal: the last speech.ended/speech.cancelled
	// for its occurrence (the one-terminal discipline: one source settles
	// a line, the claim rules keep the rest silent — the scheduler's own
	// stale-generation drop on a fenced line's in-flight audio is the
	// utterance's lifecycle terminal, the line's fate is the settle
	// terminal it replaced).
	TerminalResult string
	TerminalReason string
	TerminalAt     time.Time // the stream instant of that terminal (the line→run join's tie-break)
	// SpokenBy: the playback that spoke the line — the line's own
	// occurrence (the standalone path) or the merged run's occurrence
	// (the run's settled-line event names the run's outcome for the line).
	SpokenBy string
	// VoiceID: the voice the line's own speech events carried (the
	// correlation's voice_id; "" when the line terminalled before any
	// voice: a skip with no approved voice, a fence drop).
	VoiceID string
	// WAVs: the file sink's played files for the line's playbacks
	// (generation + utterance id the sink names them by).
	WAVs []string
}

// cuRun is one catch-up merged run: the utterance the flush built from the
// batch's maximal consecutive same-speaker group (ADR-0122 decision 3) —
// one TTS request for the run's lines, one playback, the run's speaker's
// voice.
type cuRun struct {
	Occurrence     string
	MessageIDs     []string // the covered lines' message ids, commit order (the versioned merged id's segments)
	CharacterID    string   // the run's speaker (the requested payload's character_id)
	Generation     int64
	Trigger        string // the flush trigger (playback_ended, the window, the box change, the policy switch)
	TextLen        int
	ReadyAt        time.Time
	StartedAt      time.Time
	TerminalResult string
	TerminalReason string
	TerminalAt     time.Time // the run's playback terminal instant (the line→run join's tie-break)
	VoiceID        string
	WAVs           []string
}

// cuStart is one playback that actually began (the sink's speech.started,
// the scheduler's own event: one start per started playback, the
// single-active-playback rule's evidence).
type cuStart struct {
	Occurrence string
	Generation int64
	At         time.Time // the event's occurred_at
	StartedAt  time.Time // the sink's started_at (zero when the payload lacks it)
	VoiceID    string
}

// cuSpeechModel is the assertion-side view of one session's correlated
// speech stream: the lines (the committed eligible lines plus the
// read-through lines the wiring routed), the merged runs and the starts,
// joined on the occurrence and message ids the wiring correlates. The
// events stay in the bundle's dev_events.json (the raw payloads); the
// model is what the assertions and the manifest read from.
type cuSpeechModel struct {
	Lines   map[string]*cuLine
	lineSeq []string // commit order (the stream order the events arrived in)
	Runs    map[string]*cuRun
	Started []cuStart
	// readyByOccurrence: every speech.ready's instant, keyed by the
	// occurrence it named (a standalone line's, a run's): the no-backlog
	// bound measures from the ready instant (ADR-0122 decision 5: the max
	// age is the audio-ready instant, the queue behind a running playback
	// is the back-to-back shape, not a backlog).
	readyByOccurrence map[string]time.Time
}

// cuParseSpeechModel walks the session's dev event stream (store-truth
// order) and builds the model. The join rules are the wiring's own
// correlation (ADR 0122: the dialogue_occurrence_id every speech event
// carries, the run's message_ids the merged request names, the
// settled-line terminal's run outcome): every committed eligible line
// appears exactly once, covered by the playback that spoke it or fenced
// it.
func cuParseSpeechModel(evs []devEnvelope, fileSinkDir, sessionID string) *cuSpeechModel {
	m := &cuSpeechModel{
		Lines: map[string]*cuLine{}, Runs: map[string]*cuRun{},
		readyByOccurrence: map[string]time.Time{},
	}
	// currentRun is the run whose terminal just landed: a run's settled-line
	// terminals (reason catch_up_merged) are published by the wiring right
	// after the run's own playback terminal (the scheduler event that
	// triggers settleRunLines), in the store's seq order — so the run a
	// merged-line terminal names is the run that just ended. The message-id
	// heuristic (cuRunCovering) is only the fallback for a stream that lost
	// the run's terminal.
	var currentRun string
	for _, e := range evs {
		var d struct {
			Occurrence  string   `json:"dialogue_occurrence_id"`
			UtteranceID string   `json:"utterance_id"`
			MessageID   string   `json:"message_id"`
			MessageIDs  []string `json:"message_ids"`
			RegionID    string   `json:"region_id"`
			Generation  int64    `json:"generation"`
			Text        string   `json:"text"`
			Eligible    bool     `json:"speech_eligible"`
			Reason      string   `json:"reason"`
			Result      string   `json:"result"`
			VoiceID     string   `json:"voice_id"`
			CharacterID string   `json:"character_id"`
			Merged      bool     `json:"merged"`
			Trigger     string   `json:"trigger"`
			StartedAt   string   `json:"started_at"`
		}
		if len(e.Data) == 0 {
			continue
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			continue
		}
		if d.Occurrence == "" {
			d.Occurrence = d.UtteranceID
		}
		at, _ := time.Parse(time.RFC3339Nano, e.OccurredAt)
		switch e.Name {
		case "message.committed":
			if !d.Eligible || d.MessageID == "" || d.MessageID == "unknown" || d.Occurrence == "" {
				continue
			}
			if _, ok := m.Lines[d.Occurrence]; ok {
				continue // the occurrence committed once (the reducer dedupes)
			}
			m.Lines[d.Occurrence] = &cuLine{
				Occurrence: d.Occurrence, MessageID: d.MessageID, RegionID: d.RegionID,
				Generation: d.Generation, Text: d.Text, CommittedAt: at,
			}
			m.lineSeq = append(m.lineSeq, d.Occurrence)
		case "speech.requested":
			if d.Occurrence == "" {
				continue
			}
			// A catch-up RUN's TTS request (runCatchUpBatch's speech.requested):
			// it carries message_ids — one entry per covered line — where the
			// line's own routed request (requestedPayload) never does. The
			// discriminator is the field's presence, not the merged flag: a
			// single-line run (the batch's one-line flush, merged=false) is
			// still a run — registering it as a line (the old shape) doubled
			// the line's count and orphaned the run's voice on a phantom line.
			if d.MessageIDs != nil {
				if _, ok := m.Runs[d.Occurrence]; ok {
					continue
				}
				m.Runs[d.Occurrence] = &cuRun{
					Occurrence: d.Occurrence, MessageIDs: d.MessageIDs,
					CharacterID: d.CharacterID, Generation: d.Generation,
					Trigger: d.Trigger, TextLen: len(d.Text),
				}
				continue
			}
			l, ok := m.Lines[d.Occurrence]
			if !ok {
				// A read-through line: the commit carried message_id
				// "unknown" (not a box line) and the wiring routed it from
				// the OCR text — the model learns it here.
				l = &cuLine{Occurrence: d.Occurrence, MessageID: d.MessageID, RegionID: d.RegionID, Generation: d.Generation, Text: d.Text, CommittedAt: at}
				m.Lines[d.Occurrence] = l
				m.lineSeq = append(m.lineSeq, d.Occurrence)
			}
			l.RequestedAt = at
			if d.CharacterID != "" {
				l.CharacterID = d.CharacterID
			}
			if l.MessageID == "" {
				l.MessageID = d.MessageID
			}
		case "speech.ready":
			if d.Occurrence == "" {
				continue
			}
			m.readyByOccurrence[d.Occurrence] = at
			if r, ok := m.Runs[d.Occurrence]; ok {
				r.ReadyAt = at
				if d.VoiceID != "" {
					r.VoiceID = d.VoiceID
				}
				continue
			}
			if l, ok := m.Lines[d.Occurrence]; ok {
				l.ReadyAt = at
				if d.VoiceID != "" {
					l.VoiceID = d.VoiceID
				}
			}
		case "speech.started":
			if d.Occurrence == "" {
				continue
			}
			sa := at
			if d.StartedAt != "" {
				sa, _ = time.Parse(time.RFC3339Nano, d.StartedAt)
			}
			m.Started = append(m.Started, cuStart{Occurrence: d.Occurrence, Generation: d.Generation, At: at, StartedAt: sa, VoiceID: d.VoiceID})
			if r, ok := m.Runs[d.Occurrence]; ok {
				r.StartedAt = sa
				continue
			}
			if l, ok := m.Lines[d.Occurrence]; ok {
				l.StartedAt = sa
				l.SpokenBy = d.Occurrence
				if d.VoiceID != "" {
					l.VoiceID = d.VoiceID
				}
			}
		case "speech.ended":
			if d.Occurrence == "" || d.Result == "" {
				continue
			}
			if r, ok := m.Runs[d.Occurrence]; ok {
				// The run's own playback terminal: the run's outcome. The
				// settled-line terminals (reason catch_up_merged) land as the
				// covered lines' own ended events — in the stream they may
				// precede the run's terminal (the 2026-10-21 run: same
				// millisecond, the line's first), so the line→run join also
				// runs post-walk on the run's message_ids.
				r.TerminalResult = d.Result
				r.TerminalReason = d.Reason
				r.TerminalAt = at
				if d.Result == "ended" {
					currentRun = d.Occurrence
				}
				continue
			}
			l, ok := m.Lines[d.Occurrence]
			if !ok {
				continue
			}
			l.TerminalResult = d.Result
			l.TerminalReason = d.Reason
			l.TerminalAt = at
			if d.VoiceID != "" {
				l.VoiceID = d.VoiceID
			}
			if d.Result == "ended" && d.Reason == "catch_up_merged" {
				// The settled-line event names the run's outcome for the
				// line: the run is the one that just terminalled (its
				// covered lines settle under it, in stream order).
				l.SpokenBy = currentRun
				if l.SpokenBy == "" {
					l.SpokenBy = cuRunCovering(m, l)
				}
			}
		case "speech.cancelled":
			if d.Occurrence == "" {
				continue
			}
			if r, ok := m.Runs[d.Occurrence]; ok {
				if r.TerminalResult == "" {
					r.TerminalResult = "cancelled"
					r.TerminalReason = d.Reason
					r.TerminalAt = at
				}
				continue
			}
			if l, ok := m.Lines[d.Occurrence]; ok && l.TerminalResult == "" {
				l.TerminalResult = "cancelled"
				l.TerminalReason = d.Reason
				l.TerminalAt = at
			}
		}
	}
	// The line→run join, post-walk: a settled-line terminal (ended,
	// catch_up_merged) that landed BEFORE its run's playback terminal took
	// the in-stream currentRun — the run that was playing at the instant
	// the line's settled event arrived, not the run that covered the line
	// (the covering run was still queued behind that playback: its own
	// playback terminal postdates the line's). For a matched line the
	// authoritative link is the run's message_ids covering the line's
	// message id (cuRunCovering): when the in-stream join's run does not
	// cover the line, re-join to the covering run. The read-through lines
	// (the unknown-message placeholder id matches nothing) and the already
	// covering joins keep the in-stream value.
	for _, occ := range m.lineSeq {
		l := m.Lines[occ]
		if l.TerminalResult != "ended" || l.TerminalReason != "catch_up_merged" {
			continue
		}
		if l.MessageID == "" || l.MessageID == "unknown" {
			// The read-through lines carry no message id to match: the
			// covering run is the same-generation run whose playback
			// terminal is closest to the line's settled terminal (the line
			// settles under its own run's playback — the gen-17 run's
			// playback ends one millisecond after the line's settled event;
			// the in-stream currentRun at that instant is the previous run,
			// whose voice the assertion must not be checked against). When
			// no same-generation run's terminal is near, the in-stream join
			// (or the occurrence fallback) stands.
			if l.SpokenBy == "" {
				l.SpokenBy = cuRunCovering(m, l)
			}
			var cover string
			var bestGap time.Duration
			for runOcc, r := range m.Runs {
				if r.Generation != l.Generation || r.TerminalResult != "ended" || r.TerminalAt.IsZero() || l.TerminalAt.IsZero() {
					continue
				}
				gap := r.TerminalAt.Sub(l.TerminalAt)
				if gap < 0 {
					gap = -gap
				}
				if cover == "" || gap < bestGap {
					cover, bestGap = runOcc, gap
				}
			}
			if cover != "" && cover != l.SpokenBy {
				l.SpokenBy = cover
			}
			continue
		}
		covers := func(runOcc string) bool {
			r, ok := m.Runs[runOcc]
			if !ok {
				return false
			}
			for _, id := range r.MessageIDs {
				if id == l.MessageID {
					return true
				}
			}
			return false
		}
		if !covers(l.SpokenBy) {
			// The covering run: the one(s) whose message_ids carry the
			// line's id — normally exactly one. When two runs carry it
			// (the same message committed twice, both lines batched), the
			// line's own is the run whose playback terminal is closest to
			// the line's settled terminal (the line settles under its own
			// run's playback).
			var cover string
			var bestGap time.Duration
			for runOcc, r := range m.Runs {
				if !covers(runOcc) || runOcc == l.SpokenBy {
					continue
				}
				gap := time.Duration(0)
				if !r.TerminalAt.IsZero() && !l.TerminalAt.IsZero() {
					gap = r.TerminalAt.Sub(l.TerminalAt)
					if gap < 0 {
						gap = -gap
					}
				}
				if cover == "" || (gap < bestGap && !r.TerminalAt.IsZero()) {
					cover, bestGap = runOcc, gap
				}
			}
			if cover != "" {
				l.SpokenBy = cover
			}
		}
	}
	// The WAVs (the file sink's evidence files, the per-character test's
	// glob: generation-utteranceId-*.wav under the session's sink dir;
	// the sink's SafeName keeps the utterance id usable as a filename
	// component, so the merged run's versioned id loses its punctuation
	// in the file name — the glob uses the same reduction).
	wavDir := filepath.Join(fileSinkDir, sessionID)
	for _, occ := range m.lineSeq {
		l := m.Lines[occ]
		ids := []string{l.Occurrence}
		if l.SpokenBy != "" && l.SpokenBy != l.Occurrence {
			ids = append(ids, l.SpokenBy)
		}
		for _, id := range ids {
			gen := l.Generation
			if id != l.Occurrence {
				if r, ok := m.Runs[id]; ok {
					gen = r.Generation
				}
			}
			matches, _ := filepath.Glob(filepath.Join(wavDir, fmt.Sprintf("gen%d-%s-*.wav", gen, cuSafeName(id))))
			l.WAVs = append(l.WAVs, matches...)
		}
	}
	for occ, r := range m.Runs {
		matches, _ := filepath.Glob(filepath.Join(wavDir, fmt.Sprintf("gen%d-%s-*.wav", r.Generation, cuSafeName(occ))))
		r.WAVs = append(r.WAVs, matches...)
	}
	return m
}

// cuSafeName mirrors the file sink's SafeName (internal/speech/filesink):
// the utterance id's filename-safe reduction (alphanumerics, '-', '_' and
// the first 48 bytes only) — the WAV glob must spell the id the way the
// sink named the file.
func cuSafeName(s string) string {
	if s == "" {
		return "u"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s) && i < 48; i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "u"
	}
	return string(out)
}

// cuRunCovering finds, for a line settled by a catch-up run, the run that
// covered it: the ended run whose message_ids include the line's message
// id (the settled-line terminal's run outcome, the ADR-0122 decision 3
// coverage the run's requested event names). The tie-break is the
// generation: a run's generation is its newest line's, so a generation
// match identifies the single-line run exactly — the read-through lines'
// shared "unknown" id otherwise covers every ended run that speaks noise
// (the 2026-10-21 run's two narrator read-through runs both carried
// ["unknown"]) — then the larger covered span (the more specific run).
func cuRunCovering(m *cuSpeechModel, l *cuLine) string {
	best, bestScore := "", -1
	for occ, r := range m.Runs {
		if r.TerminalResult != "ended" {
			continue
		}
		covers := false
		for _, id := range r.MessageIDs {
			if id == l.MessageID {
				covers = true
				break
			}
		}
		if !covers {
			continue
		}
		score := len(r.MessageIDs)
		if r.Generation == l.Generation {
			score += 1 << 32 // a generation match outranks any span
		}
		if score > bestScore {
			best, bestScore = occ, score
		}
	}
	return best
}

// cuLineCount / cuStartCount / cuRunCount / cuLargestRun are the manifest
// and summary log's counters.
func (m *cuSpeechModel) LineCount() int  { return len(m.Lines) }
func (m *cuSpeechModel) StartCount() int { return len(m.Started) }
func (m *cuSpeechModel) RunCount() int   { return len(m.Runs) }
func (m *cuSpeechModel) LargestRun() int {
	n := 0
	for _, r := range m.Runs {
		if len(r.MessageIDs) > n {
			n = len(r.MessageIDs)
		}
	}
	return n
}

// cuAssertConversation is assertion (0) the whole rest stands on: the
// drive actually walked a multi-box conversation — at least
// cuMinDialogueBox distinct committed dialogue lines (the brief's
// "multi-box conversation"), every one of them a MATCHED line (a message
// id the corpus names: the read-through lines are counted separately),
// and the session's committed lines all carried the scene's linked
// speaker (the lines' message ids map to the character the speaker link
// targets — the session's resolveVoice input the voice assertions check
// against).
func cuAssertConversation(t *testing.T, s cuScene, m *cuSpeechModel) {
	t.Helper()
	matched := 0
	for _, occ := range m.lineSeq {
		l := m.Lines[occ]
		if l.MessageID != "" && l.MessageID != "unknown" {
			matched++
		}
	}
	if matched < cuMinDialogueBox {
		t.Fatalf("the conversation produced only %d committed dialogue lines (want >= %d: the brief's multi-box conversation); the box's lines did not match the script live — the evidence bundle's stream is the diagnosis", matched, cuMinDialogueBox)
	}
	t.Logf("catchup e2e: conversation: %d committed dialogue lines (matched) + %d read-through lines, %d playbacks, %d catch-up runs", matched, m.LineCount()-matched, m.StartCount(), m.RunCount())
}

// cuMergeEvidence is the merge's measured shape for the assertion log and
// the manifest: the batch events (the lines that settled through the
// catch-up batch — the window drop, the rescue's hand-off, the run's
// outcome), the multi-line runs the window admitted, and the live commit
// cadence the window measured against (the smallest gap between two
// successive routed-line commits, in the stream's commit order).
type cuMergeEvidence struct {
	BatchEvents      int           // the routed lines with a catch-up terminal (stale drop, batched hand-off, merged outcome)
	MultiLineRuns    int           // the catch-up runs covering two or more lines
	MultiLineCovered int           // the lines those runs cover
	MinCommitGap     time.Duration // the smallest successive-commit gap (zero: fewer than two commits)
	Window           time.Duration // the batch window the cadence measured against (the product default; the settings API has no per-session knob)
}

// cuMergeEvidenceOf walks the model's routed lines and runs once (the
// assertion and the manifest read the same numbers).
func cuMergeEvidenceOf(m *cuSpeechModel) cuMergeEvidence {
	window := speech.DefaultCatchUpBatchWindow
	ev := cuMergeEvidence{Window: window}
	for _, occ := range m.lineSeq {
		l := m.Lines[occ]
		switch {
		case l.TerminalResult == "skipped" && (l.TerminalReason == "catch_up_stale" || l.TerminalReason == "catch_up_max_age"),
			l.TerminalResult == "cancelled" && l.TerminalReason == "catch_up_batched",
			l.TerminalResult == "ended" && l.TerminalReason == "catch_up_merged":
			ev.BatchEvents++
		}
	}
	for _, r := range m.Runs {
		if len(r.MessageIDs) >= 2 {
			ev.MultiLineRuns++
			ev.MultiLineCovered += len(r.MessageIDs)
		}
	}
	for i := 1; i < len(m.lineSeq); i++ {
		a, b := m.Lines[m.lineSeq[i-1]].CommittedAt, m.Lines[m.lineSeq[i]].CommittedAt
		if a.IsZero() || b.IsZero() || !b.After(a) {
			continue
		}
		if gap := b.Sub(a); ev.MinCommitGap == 0 || gap < ev.MinCommitGap {
			ev.MinCommitGap = gap
		}
	}
	return ev
}

// cuMergeEvidenceGapNote renders the cadence's smallest gap for the log
// lines (the zero-gap case — fewer than two commits — has no duration to
// round).
func cuMergeEvidenceGapNote(gap time.Duration) string {
	if gap <= 0 {
		return "n/a (fewer than two commits)"
	}
	return gap.Round(time.Millisecond).String()
}

// cuAssertMerge is assertion 2 (the brief's "rapid same-speaker boxes are
// merged into fewer utterances (catch_up batching events)"): the rapid
// presses committed lines into a busy speech, and the catch-up batch is
// what handled them — the ADR-0122 live-validation's batch half, with the
// merge's shape the batch produced.
//
// The gates (all live-observable; the ADR-0122's live validation asserts
// the three properties — every box commits, the catch-up speech covers
// every committed message, nothing stale starts — the merge's controlled
// shape at the window's own 900 ms admission is the controlled-timing
// suites' evidence, the batcher unit tests, the runtime's four-rapid-
// boxes white-box walk and the synthetic/gn-t025-catch-up replay):
//
//  1. Batch events: at least one routed line settled through the catch-up
//     batch — skipped catch_up_stale (the window drop: a line the player
//     left more than the batch window past a newer commit), cancelled
//     catch_up_batched (the rescue's hand-off: the line's in-flight
//     synthesis taken over by the batch), or ended catch_up_merged (the
//     line's terminal is its run's outcome). The immediate path never
//     emits a catch-up terminal, so a batch event is the stream's proof a
//     line committed while the speech was busy — the faster-than-speech
//     drive the test exists to exercise.
//  2. Per-speaker runs: every catch-up run is one speaker — the lines it
//     covers all resolve to the run's speaker (the merge's same-speaker
//     rule, ADR-0122 decision 3: maximal consecutive same-speaker groups,
//     a speaker change splits into separate runs; a line whose speaker is
//     unresolved is exempt — the narrator fallback resolves at synthesis,
//     and the voice assertion checks its voice).
//  3. The fewer-utterances shape: the playback count is no greater than
//     the routed line count (a fenced or dropped line never plays; a
//     multi-line run is one playback for its lines) — and every multi-
//     line run's request carries the versioned merged id form (ADR-0122
//     decision 6: the message_ids' segments are the covered lines' ids).
//
// A multi-line merged run (one TTS request covering two or more lines)
// is the merge's budget win when the window admits it: two lines commit
// within the 900 ms window while the speech is busy. The assertion
// RECORDS the live cadence against the window (the smallest successive
// commit gap, the manifest's merge evidence) — when the scene's commits
// outrun the window (the 2026-10-21 run-6 shape: the 0.9-1.7 s
// partial→settled re-commits and the 5-10 s box-to-box spacing against
// the then-600 ms default, which the settings API does not expose per
// session), every run is the single-line form and the batch's evidence
// is gate 1's events. The gate-1 failure is the rapid drive not running
// faster than the speech (nothing committed into a busy speech) — the
// shape the brief's faster-than-speech premise rules out.
func cuAssertMerge(t *testing.T, m *cuSpeechModel) {
	t.Helper()
	ev := cuMergeEvidenceOf(m)
	// Gate 1: the batch events — a line committed into a busy speech.
	if ev.BatchEvents == 0 {
		t.Fatalf("no routed line settled through the catch-up batch (no skipped/catch_up_stale, cancelled/catch_up_batched or ended/catch_up_merged terminal): the rapid A-presses did not commit into a busy speech — the drive did not run faster than the speech (the stream's commit-to-playback spacing in the bundle is the evidence); want the ADR-0122 batch's events")
	}
	// Gate 2: the per-speaker runs — a run's lines are one speaker.
	for occ, r := range m.Runs {
		if len(r.MessageIDs) < 2 {
			continue // a single-line run is one speaker by construction (the voice assertion still checks its voice)
		}
		for _, id := range r.MessageIDs {
			for _, l := range m.Lines {
				if l.MessageID != id {
					continue
				}
				if id == "unknown" && l.Generation != r.Generation {
					continue // the placeholder id is not a shared identity: a read-through run covers its own generation's line only
				}
				if l.CharacterID != "" && l.CharacterID != r.CharacterID {
					t.Fatalf("catch-up run %s (speaker %s) covers line %s (msg %s, speaker %s): the run's lines are one speaker (the merge's same-speaker rule, the ADR-0122 decision 3's speaker-change split) — a crossed run would speak a character's line in another speaker's voice (the run's request and the line's events are in the bundle)",
						shortID(occ), cuMaskChar(r.CharacterID), shortID(l.Occurrence), shortID(id), cuMaskChar(l.CharacterID))
				}
			}
		}
	}
	// Gate 3: the fewer-utterances shape — no line plays twice; a multi-
	// line run is one playback for its lines.
	if m.StartCount() > m.LineCount() {
		t.Fatalf("%d playbacks for %d routed lines: a playback count above the line count plays a line twice (a fenced or dropped line never plays, and a multi-line run is one playback for its lines) — the merge's fewer-utterances shape is violated (the starts and the lines' terminals are in the bundle)", m.StartCount(), m.LineCount())
	}
	for occ, r := range m.Runs {
		if len(r.MessageIDs) < 2 {
			continue
		}
		t.Logf("catchup e2e: merged run %s: %d lines, speaker %s, trigger %s (one TTS request for the run's lines)", shortID(occ), len(r.MessageIDs), cuMaskChar(r.CharacterID), r.Trigger)
	}
	gapNote := cuMergeEvidenceGapNote(ev.MinCommitGap)
	t.Logf("catchup e2e: merge: %d batch events, %d runs (%d multi-line covering %d lines), %d playbacks for %d routed lines; the live commit cadence's smallest gap %s against the %s batch window (the window's multi-line admission is the controlled-timing suites' evidence)",
		ev.BatchEvents, len(m.Runs), ev.MultiLineRuns, ev.MultiLineCovered, m.StartCount(), m.LineCount(), gapNote, ev.Window.String())
}

// cuAssertNoBacklog is assertion 1 (the brief's "stay current, never build
// a backlog"): nothing starts late.
//
//  1. Max-age (ADR-0122 decision 5, the per-utterance bound): every
//     started playback became ready no more than the per-utterance max
//     age (the core's documented 3 s) plus a bounded margin after the
//     newest line it covers committed — the drop rule the wiring enforces
//     at the ready instant (a ready past the bound never submits, the
//     lines settle skipped/catch_up_stale or catch_up_max_age instead).
//     The bound is ready-based by the ADR's own definition: the queue
//     behind a running playback is the back-to-back shape (decision 2:
//     the playing line is left to run out), not a backlog.
//  2. Window (ADR-0122 decision 3, the no-stale terminal): a line the
//     player left more than the batch window (the core's documented 900
//     ms) after a newer commit settles skipped/catch_up_stale and never
//     starts — the stream's stale-dropped lines are asserted to carry no
//     playback of their own (no started event, no WAV), and every line
//     settles EXACTLY ONCE (one terminal: the one-terminal discipline the
//     claim rules enforce — the "every box commits and is covered" half
//     of the brief).
//  3. No orphan audio: every playback started belongs to a committed line
//     or a run the model knows (an audio with no committed origin would be
//     the backlog the issue forbids).
func cuAssertNoBacklog(t *testing.T, m *cuSpeechModel) {
	t.Helper()
	maxAge := speech.DefaultCatchUpMaxUtteranceAge
	window := speech.DefaultCatchUpBatchWindow
	// The stamp-skew margin on the window bound: every instant below is the
	// same process clock (the pipeline's commit stamp, the sink's start
	// stamp), so the skew is event-publish latency, not clock drift — a
	// few hundred ms is the generous bound; a real violation is the run the
	// fence should have cancelled at the commit that out-ran the window.
	windowMargin := 300 * time.Millisecond
	staleDrop := 0
	// The committed lines in stream (commit) order, for the start-based
	// newest-commit scan.
	type commitPt struct {
		at   time.Time
		line *cuLine
	}
	var commits []commitPt
	for _, occ := range m.lineSeq {
		l := m.Lines[occ]
		commits = append(commits, commitPt{at: l.CommittedAt, line: l})
	}
	backlogStarts := 0
	var maxLag time.Duration
	for _, st := range m.Started {
		latest, latestMatched := latestCoveredCommit(m, st.Occurrence)
		if latest.IsZero() {
			t.Fatalf("playback %s (gen %d) has no committed line it covers: the audio has no committed origin (an orphan playback — the backlog shape)", shortID(st.Occurrence), st.Generation)
		}
		start := st.StartedAt
		if start.IsZero() {
			start = st.At
		}
		if lag := start.Sub(latest); lag > maxLag {
			maxLag = lag
		}
		// The no-backlog bound (issue #25: "speech for an older box never
		// starts after a newer box has committed beyond the batch window"): at
		// the playback's start, no MATCHED dialogue line committed before the
		// start is more than the batch window newer than the playback's
		// newest covered matched line. A commit that lands more than the
		// window past a flushed batch's newest line cancels the batch at the
		// source (ADR-0122 decision 4), so a playback that starts with the
		// player already further along is speaking a box the player left more
		// than the window ago — the backlog the issue forbids. The bound is
		// start-based because the ADR-0122 max-age drop is enforced at the
		// FLUSH instant ("the ready instant is the flush instant — the TTS
		// latency lands on top of it"): a start whose newest covered commit
		// is older than the max age is legal when the provider is merely
		// slow inside the window (the ADR-0122 decision 4 "the player has
		// not left it, only the provider is slow"). Read-through lines
		// (message "unknown", the non-box region's noise) are not boxes:
		// the bound is the player's box progress, so a playback that covers
		// no matched line is exempt (a box playback covering a matched line
		// is not: its covered matched line is its box progress).
		if latestMatched.IsZero() {
			continue
		}
		newestBeforeStart := time.Time{}
		for _, c := range commits {
			if c.line.MessageID == "" || c.line.MessageID == "unknown" {
				continue
			}
			if !c.at.After(start) {
				newestBeforeStart = c.at // commits are stream-ordered: the last match at or before the start is the newest
			}
		}
		if newestBeforeStart.IsZero() {
			continue // no matched box line committed before the start: nothing to be stale against
		}
		if gap := newestBeforeStart.Sub(latestMatched); gap > window+windowMargin {
			backlogStarts++
			t.Errorf("playback %s (gen %d) started with the player %v past its newest covered box line (the batch window is %s + %s margin): a commit that far ahead fences the batch at the source (ADR-0122 decision 4) — the playback speaking a box the player left beyond the window is the backlog issue #25 forbids (the commit/start spacing is in the bundle's stream)",
				shortID(st.Occurrence), st.Generation, gap.Round(time.Millisecond), window, windowMargin)
		}
	}
	if backlogStarts > 0 {
		t.Fatalf("%d playback(s) started past the batch window: the session fell behind the player — the no-backlog policy was violated live", backlogStarts)
	}
	t.Logf("catchup e2e: no-backlog: every started playback began with the player within the batch window (%s) of its newest covered box line (max start lag %v past its newest covered commit; the max-age drop %s is enforced at the flush instant, the TTS latency lands on top of it)", window, maxLag.Round(time.Millisecond), maxAge)
	settled := 0
	for _, occ := range m.lineSeq {
		l := m.Lines[occ]
		if l.TerminalResult == "" {
			t.Fatalf("committed line %s (msg %s) never reached a terminal speech event: every committed line settles exactly once (the claim rule), an unsettled line is a fence that lost the line (the stream is in the bundle)", shortID(occ), shortID(l.MessageID))
		}
		if l.TerminalResult == "skipped" && (l.TerminalReason == "catch_up_stale" || l.TerminalReason == "catch_up_max_age") {
			staleDrop++
			if !l.StartedAt.IsZero() {
				t.Fatalf("line %s (msg %s) settled %s/%s but has a started playback: a stale-dropped line never starts — the drop is its terminal (ADR-0122 decisions 3/5)", shortID(occ), shortID(l.MessageID), l.TerminalResult, l.TerminalReason)
			}
			if len(l.WAVs) > 0 {
				t.Fatalf("line %s (msg %s) settled %s/%s but left %d WAV(s) in the sink: the stale drop plays nothing (the file sink keeps only played audio)", shortID(occ), shortID(l.MessageID), l.TerminalResult, l.TerminalReason, len(l.WAVs))
			}
		}
		settled++
	}
	// Every line's terminal is a spoken terminal (result ended — by its own
	// playback or a merged run), a documented catch-up fence (skipped, the
	// stale/max-age drop, the rescue's batch hand-off) or a named failure
	// (failed: the synthesis or the sink died for that line — the line's
	// fate is still named, the ADR-0122 "every line settles exactly once"
	// discipline: a failed line is a logged deviation, not a lost line).
	unaccounted, failedLines := 0, 0
	for _, occ := range m.lineSeq {
		l := m.Lines[occ]
		switch {
		case l.TerminalResult == "ended":
		case l.TerminalResult == "skipped" && (l.TerminalReason == "catch_up_stale" || l.TerminalReason == "catch_up_max_age" || l.TerminalReason == "catch_up_batched"):
			// catch_up_batched: the rescue's own terminal on the line's
			// STANDALONE utterance when the line re-entered the batch — the
			// line's covering terminal (ended, the run's outcome) lands
			// later in the stream and overwrote this one; a line whose LAST
			// terminal is catch_up_batched never got the run's covering
			// terminal (the run died before settling it) — accounted, the
			// batch's fate is the line's fate, but it is logged: the line's
			// content was dropped, not spoken.
		case l.TerminalResult == "skipped" && l.TerminalReason == "duplicate_region" && cuSpokenElsewhere(m, occ, l.MessageID):
			// The same box committed from a second, overlapping region within
			// the duplicate window: spoken once by its sibling occurrence, the
			// copy skipped by design. Accounted only when the sibling really
			// was spoken — a duplicate skip with no spoken sibling would hide
			// a lost line. (Visible to this model since commits precede their
			// speech events in the stream.)
		case l.TerminalResult == "failed":
			failedLines++
			t.Logf("catchup e2e: line %s (msg %s) terminalled failed/%s — a named failure (the line's fate is recorded; the deviation is counted below)", shortID(occ), shortID(l.MessageID), l.TerminalReason)
		default:
			unaccounted++
			t.Errorf("line %s (msg %s) settled %q/%q: every committed line ends spoken (ended), fenced with a documented catch-up reason (the stale/max-age drop, the rescue's batch hand-off) or failed with a named reason — anything else is a line the policy lost without saying why (the stream is in the bundle)",
				shortID(occ), shortID(l.MessageID), l.TerminalResult, l.TerminalReason)
		}
	}
	if unaccounted > 0 {
		t.Fatalf("%d committed line(s) settled without a documented terminal", unaccounted)
	}
	t.Logf("catchup e2e: no-backlog: %d lines settled (exactly once each; %d ended failed with a named reason), %d stale-dropped (the window/max-age fence), %d playbacks, every start within the batch window of the player's box progress (the queue behind a running playback is the back-to-back shape, not a backlog)",
		settled, failedLines, staleDrop, m.StartCount())
}

// latestCoveredCommit is the newest committed instant among the lines a
// playback covers (the line's own commit for the standalone playback, the
// run's covered lines' commits for the merged run) and — separately — the
// newest of those that is a MATCHED dialogue line (message id not
// "unknown"): the no-backlog bound is the player's box progress, so the
// read-through noise lines do not set the origin.
func latestCoveredCommit(m *cuSpeechModel, occ string) (time.Time, time.Time) {
	var latest, latestMatched time.Time
	if r, ok := m.Runs[occ]; ok {
		for _, id := range r.MessageIDs {
			for _, l := range m.Lines {
				if l.MessageID == id && l.CommittedAt.After(latest) {
					latest = l.CommittedAt
				}
				if l.MessageID != "" && l.MessageID != "unknown" && l.CommittedAt.After(latestMatched) {
					latestMatched = l.CommittedAt
				}
			}
		}
		return latest, latestMatched
	}
	if l, ok := m.Lines[occ]; ok {
		matched := l.CommittedAt
		if l.MessageID == "" || l.MessageID == "unknown" {
			matched = time.Time{}
		}
		return l.CommittedAt, matched
	}
	return time.Time{}, time.Time{}
}

// cuMaskChar renders one resolved speaker for a log line (rule 5): a
// character id is an identifier (allowed), the narrator word is the
// product's own — both are loggable; this only shortens long ids.
func cuMaskChar(id string) string { return shortID(id) }

// cuAssertVoices is assertion 3 (the brief's "each spoken utterance uses
// the speaker's voice, the narrator only for unattributed lines"): for
// every line that reached a voice (its events carry a voice id) and every
// merged run (its ready event carries the run's speaker's voice), the
// voice is the one the speaker resolves to — the scene character's own
// designed voice for the character's lines (the speaker link's target),
// the narrator's voice for every line the speaker does not attribute to
// that character (the unattributed and the no-designed-voice fallbacks).
// A line that terminalled before any voice (a skip with no approved
// voice, a fence drop) carries no voice id and is checked to be exactly
// that — never a silently substituted one.
func cuAssertVoices(t *testing.T, s cuScene, m *cuSpeechModel) {
	t.Helper()
	charVoice := cuV.char.VoiceID
	narrVoice := cuV.narr.VoiceID
	if charVoice == "" || narrVoice == "" {
		t.Fatalf("the scene's voices are not resolved (character %q, narrator %q): the voice design step must register both before the assertions", charVoice, narrVoice)
	}
	// expectedVoice is the resolveVoice rule for this scene (two designed
	// voices exist: the scene character's and the narrator's): a line
	// attributed to the scene character resolves to the character's voice,
	// every other line (unattributed, or a character without a designed
	// voice: the narrator fallback, resolveVoice's documented rule) to the
	// narrator's — never a different character's voice.
	expectedVoice := func(characterID string) string {
		if characterID == s.charID {
			return charVoice
		}
		return narrVoice
	}
	checked, skippedNoVoice, narratorLines, charLines := 0, 0, 0, 0
	for _, occ := range m.lineSeq {
		l := m.Lines[occ]
		if l.VoiceID == "" {
			if l.TerminalResult == "ended" && l.SpokenBy != "" {
				// The line was spoken by a merged run (its own events never
				// carried a voice: the settled-line terminal has no voice_id
				// and the line's standalone path was rescued into the batch)
				// — the line's voice is the RUN's voice (one voice per run:
				// the run's speaker), checked against the line's speaker.
				r, ok := m.Runs[l.SpokenBy]
				if !ok || r.VoiceID == "" {
					t.Fatalf("line %s (msg %s) terminalled ended via merged run %s, but the run carries no voice id on its speech events: a spoken line must be in a voice (the run's ready/started events are in the bundle)", shortID(occ), shortID(l.MessageID), shortID(l.SpokenBy))
				}
				want := expectedVoice(l.CharacterID)
				if r.VoiceID != want {
					t.Fatalf("line %s (msg %s, speaker %s) was spoken by run %s in voice %s; the speaker resolves to voice %s — a line is spoken in its speaker's voice (the run's events are in the bundle)",
						shortID(occ), shortID(l.MessageID), cuMaskChar(l.CharacterID), shortID(l.SpokenBy), shortID(r.VoiceID), shortID(want))
				}
				checked++
				if l.CharacterID == "narrator" {
					narratorLines++
				} else if l.CharacterID == s.charID {
					charLines++
				}
				continue
			}
			if l.TerminalResult != "skipped" && l.TerminalResult != "cancelled" && l.TerminalResult != "failed" {
				t.Fatalf("line %s (msg %s) terminalled %q without any voice id on its speech events; every line that reached a voice choice must carry it", shortID(occ), shortID(l.MessageID), l.TerminalResult)
			}
			skippedNoVoice++
			continue
		}
		want := expectedVoice(l.CharacterID)
		if l.VoiceID != want {
			t.Fatalf("line %s (msg %s, speaker %s) spoke with voice %s; the speaker resolves to voice %s (the %s) — a line is spoken in its speaker's voice, and the narrator is the fallback for the unattributed, never a substitute for a character that has a voice (the events are in the bundle)",
				shortID(occ), shortID(l.MessageID), cuMaskChar(l.CharacterID), shortID(l.VoiceID), shortID(want), cuMaskChar(l.CharacterID))
		}
		checked++
		if l.CharacterID == "narrator" {
			narratorLines++
		} else if l.CharacterID == s.charID {
			charLines++
		}
	}
	// The merged runs: the run's ready voice is the run's SPEAKER's voice
	// (the run is the maximal consecutive same-speaker group, so one voice
	// per run; the run's character_id names that speaker).
	runChecked, runNarrator := 0, 0
	for occ, r := range m.Runs {
		if r.VoiceID == "" {
			continue // the run never reached a voice (fenced before ready): its lines' terminals account it
		}
		want := expectedVoice(r.CharacterID)
		if r.VoiceID != want {
			t.Fatalf("merged run %s (speaker %s) synthesized with voice %s; the run's speaker resolves to voice %s — the run is the speaker's voice for its whole span (the merged request's text and voice are in the bundle)",
				shortID(occ), cuMaskChar(r.CharacterID), shortID(r.VoiceID), shortID(want))
		}
		runChecked++
		if r.CharacterID == "narrator" {
			runNarrator++
		}
		// The run's lines all resolve to the run's speaker (the merge's
		// same-speaker rule, decision 3: maximal consecutive same-speaker
		// groups) — a line inside the run that resolves elsewhere is a
		// speaker split the merge crossed.
		for _, id := range r.MessageIDs {
			for _, l := range m.Lines {
				if l.MessageID != id {
					continue
				}
				if id == "unknown" && l.Generation != r.Generation {
					continue // the placeholder id is not a shared identity: the run covers its own generation's line only (the read-through lines' voices are the per-line check below)
				}
				if l.CharacterID != "" && l.CharacterID != r.CharacterID {
					t.Fatalf("merged run %s (speaker %s) covers line %s (msg %s, speaker %s): the run's lines are one speaker (the merge's same-speaker rule) — a crossed run would speak a character's line in another speaker's voice", shortID(occ), cuMaskChar(r.CharacterID), shortID(l.Occurrence), shortID(id), cuMaskChar(l.CharacterID))
				}
			}
		}
	}
	t.Logf("catchup e2e: voices: %d lines carried a voice (%d the character's, %d the narrator's for unattributed lines, %d skipped without a voice), %d merged runs checked (%d narrator runs)",
		checked, charLines, narratorLines, skippedNoVoice, runChecked, runNarrator)
}

// cuWriteEvidence lands the RAW evidence in the bundle BEFORE the
// assertions run (AGENTS.md's milestone bundle, rule 5: private,
// gitignored, outside git): the correlated dev event stream (the raw
// payloads: the frames' dev-trace references, the commit/ready/start/end
// chain, the merged requests' texts), the session snapshot, the dev-trace
// frames and crops (the pixels the OCR read) and the session's played WAVs
// (the file sink's audio). A failed assertion still leaves this bundle to
// diagnose against — the manifest and the model's records are the
// success path's cuWriteManifest.
func cuWriteEvidence(t *testing.T, bundle, sessionID string, snapshot map[string]any, evs []devEnvelope, dataDir string) {
	t.Helper()
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("mkdir bundle %s: %v", bundle, err)
	}
	writeJSONFile(t, filepath.Join(bundle, "dev_events.json"), evs) // the raw correlated stream (private evidence)
	writeJSONFile(t, filepath.Join(bundle, "snapshot.json"), snapshot)

	// The dev-trace frames and crops (the pixels the OCR read: the
	// text.observed events' frame_asset/crop_asset references address
	// these) and the session's played WAVs (the file sink's files).
	if src, err := filepath.Abs(filepath.Join(dataDir, "dev-trace")); err == nil {
		cuCopyDir(t, src, filepath.Join(bundle, "frames"))
	}
	wavDir := filepath.Join(dataDir, "audio", sessionID)
	if entries, err := os.ReadDir(wavDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".wav") {
				continue
			}
			cuCopyFile(t, filepath.Join(wavDir, e.Name()), filepath.Join(bundle, "audio", e.Name()))
		}
	}
	t.Logf("catchup e2e: raw evidence written: %s (the stream, the snapshot, the frames and the WAVs)", bundle)
}

// cuWriteManifest writes the bundle's summary half AFTER the assertions
// pass: the manifest (counts, ids, hashes and timings — never the game
// text or the tokens) and the model's line/run records.
func cuWriteManifest(t *testing.T, bundle, runID, imageSHA, fdState string, s cuScene, sessionID string,
	m *cuSpeechModel, presses []cuPress, startAll time.Time) {
	t.Helper()
	staleDrops, spoken, noVoice := 0, 0, 0
	for _, occ := range m.lineSeq {
		l := m.Lines[occ]
		switch {
		case l.TerminalResult == "ended":
			spoken++
		case l.TerminalResult == "skipped" && (l.TerminalReason == "catch_up_stale" || l.TerminalReason == "catch_up_max_age"):
			staleDrops++
		case l.VoiceID == "":
			noVoice++
		}
	}
	wavCount := 0
	for _, occ := range m.lineSeq {
		wavCount += len(m.Lines[occ].WAVs)
	}
	manifest := map[string]any{
		"run_id":            runID,
		"timestamp_utc":     time.Now().UTC().Format(time.RFC3339),
		"gate":              "test-dolphin",
		"test_class":        "live: real Dolphin emulator + real model providers (ADR-0065 category c+d+e); the first-dialogue savestate, verified by the fixture manifest sha; the file sink stands in for the hardware speaker (the per-character live E2E's same class)",
		"os":                osLabel(),
		"go_version":        goVersionLabel(),
		"disc_image_sha256": imageSHA, // the hash only (rule 5: the path is never recorded)
		"savestate": map[string]string{
			"file":   filepath.Base(fdState),
			"sha256": stateSHA(t, fdState),
		},
		"catch_up_policy": map[string]any{
			"policy":            "catch_up (the documented default; applied through the settings control, the snapshot verified)",
			"batch_window":      speech.DefaultCatchUpBatchWindow.String(),
			"max_utterance_age": speech.DefaultCatchUpMaxUtteranceAge.String(),
		},
		"profile": map[string]string{
			"game": s.gameID, "edition": s.editionID, "profile": s.profileID,
		},
		"speaker_key": s.key, // the bundle is private evidence: the key value is allowed here, the log masks it
		"character":   s.charID,
		"voices": map[string]any{
			"character": map[string]string{"profile": cuV.char.VoiceID, "registered": cuV.char.Registered, "design_model": cuV.char.DesignModel},
			"narrator":  map[string]string{"profile": cuV.narr.VoiceID, "registered": cuV.narr.Registered, "design_model": cuV.narr.DesignModel},
		},
		"session":  sessionID,
		"duration": time.Since(startAll).Round(time.Second).String(),
		"drive": map[string]any{
			"presses":        len(presses),
			"press_timeline": presses,
			"press_pace":     "rapid: one press per box as soon as the box's line commits (faster than the speech), the per-character test's same proven 500ms A hold",
		},
		"assertions": map[string]any{
			"committed_lines":     m.LineCount(),
			"lines_spoken":        spoken,
			"lines_stale_dropped": staleDrops,
			"lines_no_voice":      noVoice,
			"playbacks":           m.StartCount(),
			"catch_up_runs":       m.RunCount(),
			"largest_run_lines":   m.LargestRun(),
			"wav_files":           wavCount,
		},
		"merge_evidence": map[string]any{
			"batch_events":      cuMergeEvidenceOf(m).BatchEvents,
			"multiline_runs":    cuMergeEvidenceOf(m).MultiLineRuns,
			"multiline_covered": cuMergeEvidenceOf(m).MultiLineCovered,
			"min_commit_gap":    cuMergeEvidenceOf(m).MinCommitGap.String(),
			"batch_window":      speech.DefaultCatchUpBatchWindow.String(),
			"note":              "the batch window is the product default (ADR-0122): a multi-line merged run needs two lines committing inside the window while the speech is busy — the controlled-timing suites (the batcher unit tests, the four-rapid-boxes white-box walk, the synthetic/gn-t025-catch-up replay) prove that admission; the live scene's measured cadence (min_commit_gap) is the evidence for what the window admitted here",
		},
	}
	writeJSONFile(t, filepath.Join(bundle, "manifest.json"), manifest)
	type lineSummary struct {
		Occurrence     string   `json:"occurrence"`
		MessageID      string   `json:"message_id"`
		Generation     int64    `json:"generation"`
		CharacterID    string   `json:"character_id"`
		CommittedAt    string   `json:"committed_at"`
		ReadyAt        string   `json:"ready_at,omitempty"`
		StartedAt      string   `json:"started_at,omitempty"`
		TerminalResult string   `json:"terminal_result"`
		TerminalReason string   `json:"terminal_reason"`
		SpokenBy       string   `json:"spoken_by,omitempty"`
		VoiceID        string   `json:"voice_id,omitempty"`
		WAVs           []string `json:"wavs,omitempty"`
	}
	lines := make([]lineSummary, 0, len(m.lineSeq))
	for _, occ := range m.lineSeq {
		l := m.Lines[occ]
		ls := lineSummary{
			Occurrence: l.Occurrence, MessageID: l.MessageID, Generation: l.Generation,
			CharacterID: l.CharacterID, TerminalResult: l.TerminalResult, TerminalReason: l.TerminalReason,
			SpokenBy: l.SpokenBy, VoiceID: l.VoiceID, WAVs: l.WAVs,
		}
		if !l.CommittedAt.IsZero() {
			ls.CommittedAt = l.CommittedAt.UTC().Format(time.RFC3339Nano)
		}
		if !l.ReadyAt.IsZero() {
			ls.ReadyAt = l.ReadyAt.UTC().Format(time.RFC3339Nano)
		}
		if !l.StartedAt.IsZero() {
			ls.StartedAt = l.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		lines = append(lines, ls)
	}
	writeJSONFile(t, filepath.Join(bundle, "lines.json"), lines)
	type runSummary struct {
		Occurrence     string   `json:"occurrence"`
		MessageIDs     []string `json:"message_ids"`
		CharacterID    string   `json:"character_id"`
		Generation     int64    `json:"generation"`
		Trigger        string   `json:"trigger"`
		TextLen        int      `json:"text_len"`
		ReadyAt        string   `json:"ready_at,omitempty"`
		StartedAt      string   `json:"started_at,omitempty"`
		TerminalResult string   `json:"terminal_result"`
		TerminalReason string   `json:"terminal_reason"`
		VoiceID        string   `json:"voice_id,omitempty"`
		WAVs           []string `json:"wavs,omitempty"`
	}
	runs := make([]runSummary, 0, len(m.Runs))
	for _, r := range m.Runs {
		rs := runSummary{
			Occurrence: r.Occurrence, MessageIDs: r.MessageIDs, CharacterID: r.CharacterID,
			Generation: r.Generation, Trigger: r.Trigger, TextLen: r.TextLen,
			TerminalResult: r.TerminalResult, TerminalReason: r.TerminalReason, VoiceID: r.VoiceID, WAVs: r.WAVs,
		}
		if !r.ReadyAt.IsZero() {
			rs.ReadyAt = r.ReadyAt.UTC().Format(time.RFC3339Nano)
		}
		if !r.StartedAt.IsZero() {
			rs.StartedAt = r.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		runs = append(runs, rs)
	}
	writeJSONFile(t, filepath.Join(bundle, "runs.json"), runs)
	t.Logf("catchup e2e: evidence bundle complete: %s (manifest, the line/run records)", bundle)
}

// cuCopyDir copies one directory tree (the dev-trace's frames and crops)
// into the bundle, preserving the tree's relative layout: the dev-trace
// store is content-addressed (assets.NewLocal's blobs/<xx>/<yy>/<sha>), so
// a top-level-only copy lands nothing. Bounded: the dev-trace recorder's
// own quota caps the tree, and a copy failure is a logged deviation (the
// stream's asset references still address the originals in the data dir
// while the test lives).
func cuCopyDir(t *testing.T, src, dst string) {
	t.Helper()
	n := 0
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return nil
		}
		if cuCopyFile(t, path, filepath.Join(dst, rel)) {
			n++
		}
		return nil
	})
	if err != nil {
		t.Logf("catchup e2e: dev-trace frames absent (%v): the bundle's stream carries the asset references only", err)
		return
	}
	t.Logf("catchup e2e: %d dev-trace assets copied to the bundle (frames + crops, content-addressed tree)", n)
}

// cuCopyFile copies one file into the bundle; reports true when it landed.
func cuCopyFile(t *testing.T, src, dst string) bool {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Logf("catchup e2e: bundle mkdir %s: %v", filepath.Dir(dst), err)
		return false
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Logf("catchup e2e: bundle copy %s: %v", filepath.Base(dst), err)
		return false
	}
	return true
}

// cuSpokenElsewhere reports whether another committed occurrence of the same
// message ended spoken (a duplicate-region skip's sibling).
func cuSpokenElsewhere(m *cuSpeechModel, occ, messageID string) bool {
	for _, other := range m.lineSeq {
		if other == occ {
			continue
		}
		if o := m.Lines[other]; o.MessageID == messageID && o.TerminalResult == "ended" {
			return true
		}
	}
	return false
}
