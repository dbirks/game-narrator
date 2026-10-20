// Package e2e — helpers for the per-character live E2E (task
// per-character-live): live play speaks each character in their own linked
// voice. Log-safe per AGENTS.md rule 5: log lines and returned summaries
// carry counts, ids, hashes and timings — never dialogue text, game content
// or tokens. Fixture texts and the imported script never leave the
// gitignored evidence bundle.

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/windwaker"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"runtime"

	rt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// percharEvidenceSub is this test's gitignored evidence subdir under the
// REPO ROOT's artifacts/ (.gitignore: artifacts/) — AGENTS.md's evidence
// bundle location. It resolves against the repo root (pcRepoRoot), not the
// package dir: go test runs in internal/e2e, and a package-relative path
// would write the bundle under internal/e2e/artifacts/ (the 2026-10-20
// failure's second defect).
const percharEvidenceSub = "per-character-live"

// pcRepoRoot is the repository root: the test runs from internal/e2e, so
// the root is two levels up, verified by its go.mod the same way the soak
// test's resolver does.
func pcRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s does not look like the repository: %v", root, err)
	}
	return root
}

// debugLog mirrors the polling client's own reads (seqs only, never
// payloads) into the GN_E2E_DEBUG_LOG file: a stream the developer route
// fails to return in full must be tellable apart from a client-side drop
// before either side is blamed.
var debugLog *log.Logger

// The two recorded checkpoints the test boots (ADR 0054: the savestates are
// referenced by the fixture manifests' SHA-256; the files live under the
// fixture root's gitignored states/ dir, AGENTS.md rule 5).
const (
	pcFixtureFirstDialogue = "windwaker-first-dialogue" // the first in-game NPC dialogue
	pcFixtureLegendPress   = "windwaker-legend-press"   // the opening legend from legend_start
	pcStateFirstDialogue   = "states/windwaker-first-dialogue.state"
	pcStateLegendStart     = "states/windwaker-legend-start.state"
)

// pcSceneSpec is one live scene's configuration: its checkpoint (the
// fixture + savestate), the single region the scene's profile carries (the
// proven live configuration observes ONE region per profile — the dialogue
// box and the legend subtitle band overlap on screen, so one profile with
// both regions would make the recognizer see two contradictory textboxes at
// once), and the advance target (how many fully-played lines count as the
// scene's evidence).
type pcSceneSpec struct {
	label       string
	fixtureDir  string
	state       string
	regionLabel string
	regionType  string
	x, y, w, h  float64
	// extraRegions: the other textbox positions the profile carries beyond
	// the primary measured region above (the Wind Waker dialogue box
	// tracks the speaker on screen, so the opening scene's profile also
	// carries the box's high position — one low region alone saw nothing
	// in the 2026-10-20 probe).
	extraRegions []pcRegionSpec
	minEnded     int
	charName     string
	charRole     string
	brief        string
	// voiceCharacter: the character id the voice is designed under. Empty
	// means the scene's own character (the default). The narrator scene sets
	// it to "narrator": its lines' speaker keys are not linked to a roster
	// character (the game's story narration is unattributed), so the
	// runtime's resolveVoice falls back to the "narrator" voice (internal/
	// runtime/speech.go). Designing the voice under "narrator" is what makes
	// that fallback resolve — the product's intended narration path, not a
	// test shim.
	voiceCharacter string
}

// pcRegionSpec is one profile region: the measured box position and its
// label/type (the regions API's own fields).
type pcRegionSpec struct {
	label      string
	regionType string
	x, y, w, h float64
}

// pcSceneOutcome is one scene's built profile + linked character + designed
// voice: the per-scene half of the run.
type pcSceneOutcome struct {
	spec       pcSceneSpec
	gameID     string
	editionID  string
	profileID  string
	key        string // the scene's dominant speaker key (counts/ids only)
	keyMatched int    // the fixture lines that mapped onto the parsed script
	repointed  int    // the speaker-link's re-pointed line count
	charID     string
	voice      pcVoiceOutcome
}

// pcFixtureRoot resolves the external fixture root: the env override, else
// the gitignored default tried from the package dir and the working dir (go
// test runs in the former). ok is false when neither exists — the caller
// reports NOT RUN, never a pass.
func pcFixtureRoot(t *testing.T) (string, bool) {
	t.Helper()
	var candidates []string
	if root := os.Getenv("GAME_NARRATOR_FIXTURE_DIR"); root != "" {
		candidates = append(candidates, root)
	} else {
		candidates = append(candidates, filepath.Join("..", "..", "artifacts", "fixtures"), "artifacts/fixtures")
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, pcFixtureFirstDialogue)); err == nil && st.IsDir() {
			if _, err := os.Stat(filepath.Join(c, pcFixtureLegendPress)); err == nil {
				return c, true
			}
		}
	}
	return "", false
}

// pcFixtureProfile is the fixture's profile.json: the matched lines (their
// record-time readings) and the measured regions. Only the fields the test
// needs are decoded; the texts themselves are matched in-process and never
// logged.
type pcFixtureProfile struct {
	ID       string `json:"id"`
	Messages []struct {
		ID           string `json:"id"`
		OriginalText string `json:"original_text"`
	} `json:"messages"`
	Regions []struct {
		ID   string  `json:"id"`
		Type string  `json:"type"`
		X    float64 `json:"x"`
		Y    float64 `json:"y"`
		W    float64 `json:"w"`
		H    float64 `json:"h"`
	} `json:"regions"`
}

// pcLoadFixtureProfile reads one fixture's profile.json (the matched lines
// and the measured regions the recording used).
func pcLoadFixtureProfile(t *testing.T, dir string) pcFixtureProfile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "profile.json"))
	if err != nil {
		t.Fatalf("fixture profile %s: %v", dir, err)
	}
	var p pcFixtureProfile
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("fixture profile decode %s: %v", dir, err)
	}
	if len(p.Messages) == 0 {
		t.Fatalf("fixture %s: no matched lines in profile.json", dir)
	}
	return p
}

// pcVerifyState checks the fixture manifest's savestate reference against
// the file on disk (SHA-256, the manifest's own claim) and returns the
// file's path. A missing or corrupt state is NOT RUN material: the
// checkpoint is external, gitignored user state (AGENTS.md rule 5).
func pcVerifyState(t *testing.T, fixtureDir, relState string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.json"))
	if err != nil {
		t.Fatalf("fixture manifest %s: %v", fixtureDir, err)
	}
	var man struct {
		States []struct {
			File   string `json:"file"`
			SHA256 string `json:"sha256"`
			Screen string `json:"screen"`
			Slot   int    `json:"slot"`
			Label  string `json:"label"`
		} `json:"states"`
	}
	if err := json.Unmarshal(b, &man); err != nil {
		t.Fatalf("fixture manifest decode %s: %v", fixtureDir, err)
	}
	var want string
	for _, s := range man.States {
		if s.File == relState {
			want = s.SHA256
			break
		}
	}
	if want == "" {
		t.Fatalf("fixture %s: manifest references no savestate %s", filepath.Base(fixtureDir), relState)
	}
	path := filepath.Join(fixtureDir, "..", relState)
	got := sha256File(t, path)
	if got != want {
		t.Fatalf("savestate %s: sha256 %s does not match the fixture manifest's %s (stale or corrupt checkpoint; re-record with the savestate gate)",
			relState, got[:16], want[:16])
	}
	return path
}

// sha256File streams a file's SHA-256 (4 MiB chunks; the state files are
// ~25 MiB and are never loaded whole).
func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v (external gitignored checkpoint missing — NOT RUN material, re-record the fixtures)", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 4<<20)); err != nil {
		t.Fatalf("hash %s: %v", filepath.Base(path), err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// maskPCKey renders one opaque speaker key for the log: the stable prefix
// plus a short digest of the value. The key ids are identifiers the test
// may name (the task's counts/ids allowance), but the ADR 0070 test
// practice keeps even key VALUES out of logs; the full key rides on the
// API calls and the evidence bundle only.
func maskPCKey(key string) string {
	if key == "" {
		return "<none>"
	}
	sum := sha256.Sum256([]byte(key))
	prefix := key
	if i := 9; i < len(key) {
		prefix = key[:i] + ":"
	}
	return prefix + hex.EncodeToString(sum[:])[:8]
}

// pcScriptKey folds one imported line exactly as the bench's matcher index
// does (ADR 0070's cross-check path): the adapter's normalized text, then
// the retrieval fold (punctuation stripped, OCR-confusable forms folded).
// Re-deriving the key instead of trusting a stored column keeps the
// truth-to-script mapping on the same key the live ranking will use.
func pcScriptKey(text string) string {
	norm := textnorm.Normalize(text, textnorm.Options{})
	return textnorm.Normalize(norm, textnorm.Options{StripPunctuation: true, OCRFold: true})
}

// pcDominantKey maps a fixture's matched lines onto the parsed script's
// speaker keys with the bench's own rule: an exact folded-key match first,
// then the nearest key within a bounded edit distance (the record-time
// reading and the script differ by a glyph or two), never a guess. It
// reports the matched line count, the distinct keys the matched lines
// carry, and the dominant key (the speaker of the scene). Counts and key
// ids only — the texts themselves stay in the bundle (AGENTS.md rule 5).
func pcDominantKey(t *testing.T, fixtureDir string, parsed *adapter.ParseResult) (dominant string, matched, distinct int) {
	t.Helper()
	prof := pcLoadFixtureProfile(t, fixtureDir)

	// The parsed side: folded key → speaker sidecar key, lockstep (the
	// adapter fills SpeakerKeys parallel to Messages, ADR 0070).
	type scriptLine struct{ key, speaker string }
	lines := make([]scriptLine, 0, len(parsed.Messages))
	exact := make(map[string]string, len(parsed.Messages))
	for i, m := range parsed.Messages {
		k := pcScriptKey(m.OriginalText)
		if k == "" || i >= len(parsed.SpeakerKeys) {
			continue
		}
		lines = append(lines, scriptLine{key: k, speaker: parsed.SpeakerKeys[i]})
		if _, ok := exact[k]; !ok {
			exact[k] = parsed.SpeakerKeys[i]
		}
	}
	var counts map[string]int
	for _, msg := range prof.Messages {
		k := pcScriptKey(msg.OriginalText)
		speaker := ""
		if s, ok := exact[k]; ok {
			speaker = s
		} else {
			// Nearest key within the tolerance (the bench's bound: the
			// reading and the script are two renderings of the same line;
			// the bound keeps a 5%-off reading attached and a different
			// line off).
			tol := len(k) / 10
			if tol < 3 {
				tol = 3
			}
			best, bestD := -1, 0
			for i, l := range lines {
				if len(l.key) < len(k)-tol || len(l.key) > len(k)+tol {
					continue
				}
				d := dolphinLevenshtein([]rune(k), []rune(l.key))
				if d <= tol && (bestD < 0 || d < bestD) {
					best, bestD = i, d
				}
			}
			if best < 0 {
				continue // not in the script: never guessed onto the closest line
			}
			speaker = lines[best].speaker
		}
		if speaker == "" {
			continue
		}
		matched++
		if counts == nil {
			counts = map[string]int{}
		}
		counts[speaker]++
	}
	distinct = len(counts)
	var max int
	for k, n := range counts {
		if n > max {
			dominant, max = k, n
		}
	}
	if matched == 0 {
		t.Fatalf("fixture %s: none of the %d matched lines mapped onto the parsed script (the recording and the disc import must share the lines; check the image hash)",
			filepath.Base(fixtureDir), len(prof.Messages))
	}
	t.Logf("e2e: fixture %s: %d/%d matched lines mapped, %d distinct speaker keys (dominant %s)",
		filepath.Base(fixtureDir), matched, len(prof.Messages), distinct, maskPCKey(dominant))
	return dominant, matched, distinct
}

// pcParseBMG parses the disc's message script in-process through the
// windwaker adapter (the same parse the import job runs through the API,
// ADR 0070): it needs the speaker sidecar (per-line keys) to map the
// fixtures' matched lines onto speakers. The parse result is test-local:
// nothing from it is logged (counts and key ids only).
func pcParseBMG(t *testing.T, ctx context.Context, bmgBytes []byte, profileID string) *adapter.ParseResult {
	t.Helper()
	opts := adapter.ParseOptions{ProfileID: profileID, MaxBytes: adapter.DefaultMaxBytes}
	norm, err := opts.Normalize()
	if err != nil {
		t.Fatalf("parse options: %v", err)
	}
	manifest := windwaker.New().Manifest()
	res, err := windwaker.New().Parse(ctx, bytes.NewReader(bmgBytes), norm)
	if err != nil {
		t.Fatalf("parse BMG (adapter %s@%s): %v", manifest.ID, manifest.Version, err)
	}
	if len(res.Messages) < 1000 || len(res.SpeakerKeys) != len(res.Messages) {
		t.Fatalf("parse: %d messages, %d speaker keys (want the full script with a lockstep sidecar)", len(res.Messages), len(res.SpeakerKeys))
	}
	t.Logf("e2e: BMG parsed in-process: %d messages, %d distinct speakers (column %s), %d bytes",
		len(res.Messages), len(res.Speakers), res.SpeakerColumn, len(bmgBytes))
	return res
}

// pcGnTestName mints one automated-test registry name: the gn_test_
// namespace the Base registry reserves for live tests (ADR 0022: the only
// names the live tests may register or delete), eight random hex chars.
func pcGnTestName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("mint test name: %v", err)
	}
	return "gn_test_" + hex.EncodeToString(b)
}

// pcVoiceOutcome is one designed, approved, registered voice: the
// revision row, the automation-test registry name it was registered under,
// and the QA verdict the design audition earned (the approval gate's
// evidence, ADR 0061).
type pcVoiceOutcome struct {
	VoiceID     string
	Character   string
	Registered  string
	AuditionID  string
	QAState     string
	QAWER       float64
	PreviewWAV  []byte
	DesignModel string
	CloneModel  string
}

// pcDesignVoice runs the product's voice workflow end to end for one
// character: auditions through the API (real VoiceDesign provider, one
// clip), the ASR QA gate on the audition, approval with consent, and the
// Base registration under a gn_test_ name (the test namespaces its own
// voices; the registration itself is the real registry wire). The
// registration row is written with the test name so the session's
// resolveVoice speaks the registered checkpoint name, not an inline
// reference. Cleanup removes the name from the registry.
func pcDesignVoice(t *testing.T, ctx context.Context, srv http.Handler, token string, st *store.Store, as *assets.Local, reg *tts.Registry, character, brief, designModel, cloneModel string) pcVoiceOutcome {
	t.Helper()
	out := pcVoiceOutcome{Character: character, DesignModel: designModel, CloneModel: cloneModel}

	// 1. auditions: the API enqueues the voice.auditions job (the design
	// provider + the ASR QA read-back run in the job, with the service's
	// ASR client wired the way cmd/game-narrator wires it).
	code, body := pcDoJSON(t, srv, token, http.MethodPost, "/api/v1/voices/auditions", map[string]any{
		"character": character,
		"brief":     brief,
		"count":     1,
	}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("voice auditions create (character %s) = %d: %v", character, code, body)
	}
	out.VoiceID, _ = body["voice_id"].(string)
	jobID, _ := body["job_id"].(string)
	if out.VoiceID == "" || jobID == "" {
		t.Fatalf("voice auditions response: %v", body)
	}
	job := dolphinWaitJob(t, srv, token, jobID, 10*time.Minute)
	if job.State != "succeeded" {
		t.Fatalf("voice auditions job %s ended %s: %s", jobID, job.State, string(job.Result))
	}

	// 2. the audition's own QA verdict (the approval gate's evidence):
	// with the speech_asr role wired, the designed clip was transcribed by
	// the real ASR endpoint and scored against its line.
	code, body = pcDoJSON(t, srv, token, http.MethodGet, "/api/v1/voices/"+out.VoiceID, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("get voice %s = %d: %v", out.VoiceID, code, body)
	}
	var detail struct {
		State     string `json:"state"`
		Auditions []struct {
			ID      string  `json:"id"`
			QAState string  `json:"qa_state"`
			QAWER   float64 `json:"qa_wer"`
			Model   string  `json:"model_id"`
		} `json:"auditions"`
	}
	if err := json.Unmarshal(mustMarshal(t, body), &detail); err != nil {
		t.Fatalf("voice detail decode: %v", err)
	}
	if detail.State != "auditioning" || len(detail.Auditions) != 1 {
		t.Fatalf("voice %s state=%s auditions=%d; want auditioning with one clip", out.VoiceID, detail.State, len(detail.Auditions))
	}
	out.AuditionID, out.QAState, out.QAWER = detail.Auditions[0].ID, detail.Auditions[0].QAState, detail.Auditions[0].QAWER
	if out.QAState != "passed" {
		t.Fatalf("voice %s audition %s QA state=%s wer=%.3f; the speech_asr role is wired, so the designed clip must pass its own read-back before it may be approved",
			out.VoiceID, out.AuditionID, out.QAState, out.QAWER)
	}
	t.Logf("e2e: voice %s (character %s): audition %s QA passed (wer=%.3f, asr via the real speech_asr role)", out.VoiceID, character, out.AuditionID, out.QAWER)

	// 3. approval (the human gate, with the consent the registry requires).
	code, body = pcDoJSON(t, srv, token, http.MethodPost, "/api/v1/voices/"+out.VoiceID+"/approve", map[string]any{
		"audition_id": out.AuditionID,
		"consent":     "original_synthetic_design (game-narrator per-character live e2e)",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("voice approve %s = %d: %v", out.VoiceID, code, body)
	}
	rev, err := st.GetVoiceRevision(ctx, out.VoiceID)
	if err != nil {
		t.Fatalf("voice revision %s: %v", out.VoiceID, err)
	}
	if rev.ReferenceAssetID == "" || rev.RefTranscript == "" || rev.ConsentProvenance == "" {
		t.Fatalf("voice %s: approved row missing reference/transcript/consent", out.VoiceID)
	}
	r, _, err := as.Get(ctx, rev.ReferenceAssetID)
	if err != nil {
		t.Fatalf("read approved reference: %v", err)
	}
	ref, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("read approved reference bytes: %v", err)
	}

	// 4. register on the Base registry under the test namespace: the real
	// registry wire with the approved reference and consent; the test
	// namespaces the name gn_test_* so the shared registry sees only
	// automation voices (ADR 0022) and the cleanup below always removes it.
	name := pcGnTestName(t)
	inst := voices.DefaultInstanceID
	if err := st.UpsertVoiceRegistration(ctx, store.VoiceRegistration{
		VoiceProfileID: out.VoiceID, InstanceID: inst, RegisteredName: name, State: store.VoiceRegPending,
	}); err != nil {
		t.Fatalf("record pending registration: %v", err)
	}
	// The registry sits behind a kubectl port-forward: the upstream pod can
	// drop a connection mid-flight (EOF), which the client classifies as a
	// transient unavailability. Bounded retry (AGENTS.md: retry only
	// idempotent operations, max attempts) — re-registering the same name
	// with the same revision is idempotent.
	var up tts.UploadedVoice
	var regErr error
	for attempt := 1; attempt <= 3; attempt++ {
		up, regErr = reg.Register(ctx, tts.Voice{Name: name, RefAudio: ref, RefText: rev.RefTranscript, Consent: rev.ConsentProvenance})
		if regErr == nil {
			break
		}
		var perr *providers.Error
		if !errors.As(regErr, &perr) || perr.Kind != providers.KindUnavailable || attempt == 3 {
			break
		}
		t.Logf("e2e: Base registry register %s: attempt %d/3 unavailable (kind=%s); retrying in 5s (bounded retry of a transient drop)", name, attempt, perr.Kind)
		time.Sleep(5 * time.Second)
	}
	if regErr != nil {
		t.Fatalf("Base registry register %s (real wire): %v", name, regErr)
	}
	out.Registered = name
	now := time.Now().UTC()
	if err := st.UpsertVoiceRegistration(ctx, store.VoiceRegistration{
		VoiceProfileID: out.VoiceID, InstanceID: inst, RegisteredName: up.Name, State: store.VoiceRegReady, LastVerifiedAt: &now,
	}); err != nil {
		t.Fatalf("record ready registration: %v", err)
	}
	t.Logf("e2e: voice %s registered on the Base registry as %s (upload %d bytes, real wire)", out.VoiceID, up.Name, up.FileSize)

	// 5. one preview through the voices service: proves the registered voice
	// synthesises end-to-end before the session's own utterance does.
	rec := pcDo(t, srv, token, http.MethodPost, "/api/v1/voices/"+out.VoiceID+"/preview", mustMarshal(t, map[string]any{
		"text": voices.SyntheticAuditionLines[0],
	}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("voice preview %s = %d", out.VoiceID, rec.Code)
	}
	out.PreviewWAV = rec.Body.Bytes()
	if len(out.PreviewWAV) < 64 || string(out.PreviewWAV[:4]) != "RIFF" {
		t.Fatalf("voice preview %s: %d bytes, not a WAV", out.VoiceID, len(out.PreviewWAV))
	}
	t.Logf("e2e: voice %s preview: %d bytes (X-Cache %s)", out.VoiceID, len(out.PreviewWAV), rec.Header().Get("X-Cache"))
	return out
}

// pcDo is the authenticated request helper for the in-process API server
// (dolphinDo with the JSON body convenience the do-local helpers in the
// other live tests build for themselves); headers may be nil.
func pcDo(t *testing.T, srv http.Handler, token, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return dolphinDo(t, srv, token, method, target, body, headers)
}

// pcDoJSON is pcDo with a JSON body and a decoded response object (nil body
// for GETs; the response decodes to a map for the caller's field picks).
func pcDoJSON(t *testing.T, srv http.Handler, token, method, target string, v any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var body []byte
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s %s: %v", method, target, err)
		}
		body = b
	}
	rec := pcDo(t, srv, token, method, target, body, headers)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// pcSpokenLine is one committed line's correlated audio evidence: the
// commit, the resolved voice (the speech events carry voice_id) and the
// terminal playback result. The WAV path is the file sink's evidence file.
type pcSpokenLine struct {
	MessageID   string
	TraceID     string
	VoiceID     string
	CacheKey    string
	Result      string
	Reason      string
	UtteranceID string
	Generation  int64
	WAVPath     string
}

// pcCollectSpokenLines scans one session's event log for every committed,
// speech-eligible line and its terminal speech event, joined on the
// occurrence the wiring correlates (dialogue_occurrence_id). Lines whose
// speech never reached a terminal event are not "spoken" (still in the
// fence when the session stopped) and are left out.
func pcCollectSpokenLines(evs []devEnvelope, fileSinkDir, sessionID string) []pcSpokenLine {
	byOcc := map[string]*pcSpokenLine{}
	var order []string
	add := func(id string) {
		if _, ok := byOcc[id]; !ok {
			byOcc[id] = &pcSpokenLine{MessageID: "", TraceID: "", VoiceID: "", CacheKey: "", Result: "", Reason: ""}
			order = append(order, id)
		}
	}
	for _, e := range evs {
		var d struct {
			Occurrence  string `json:"dialogue_occurrence_id"`
			MessageID   string `json:"message_id"`
			TraceID     string `json:"trace_id"`
			Eligible    bool   `json:"speech_eligible"`
			VoiceID     string `json:"voice_id"`
			CacheKey    string `json:"cache_key"`
			Result      string `json:"result"`
			Reason      string `json:"reason"`
			UtteranceID string `json:"utterance_id"`
			Generation  int64  `json:"generation"`
		}
		if len(e.Data) == 0 {
			continue
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			continue
		}
		switch e.Name {
		case "message.committed":
			if d.Occurrence == "" || d.MessageID == "" || d.MessageID == "unknown" || !d.Eligible {
				continue
			}
			add(d.Occurrence)
			byOcc[d.Occurrence].MessageID = d.MessageID
			if byOcc[d.Occurrence].TraceID == "" {
				byOcc[d.Occurrence].TraceID = e.TraceID
			}
		case "speech.ready":
			if d.Occurrence == "" {
				continue
			}
			add(d.Occurrence)
			if byOcc[d.Occurrence].VoiceID == "" {
				byOcc[d.Occurrence].VoiceID = d.VoiceID
			}
			if d.CacheKey != "" {
				byOcc[d.Occurrence].CacheKey = d.CacheKey
			}
		case "speech.started":
			// The utterance that actually began playing (a QA-failed
			// generation never starts; a later regeneration supersedes the
			// earlier record).
			if d.Occurrence == "" {
				continue
			}
			add(d.Occurrence)
			l := byOcc[d.Occurrence]
			if l.VoiceID == "" {
				l.VoiceID = d.VoiceID
			}
			if d.UtteranceID != "" {
				l.UtteranceID = d.UtteranceID
				l.Generation = d.Generation
			}
		case "speech.ended":
			if d.Occurrence == "" || d.Result == "" {
				continue
			}
			add(d.Occurrence)
			l := byOcc[d.Occurrence]
			l.Result = d.Result
			l.Reason = d.Reason
			if l.VoiceID == "" {
				l.VoiceID = d.VoiceID
			}
			if d.UtteranceID != "" {
				l.UtteranceID = d.UtteranceID
				l.Generation = d.Generation
			}
		case "speech.cancelled":
			// The scheduler's queued-drop terminal (session end before start):
			// it carries no result field, so mark it distinctly — the line is
			// terminal without having played.
			if d.Occurrence == "" {
				continue
			}
			add(d.Occurrence)
			l := byOcc[d.Occurrence]
			if l.Result == "" {
				l.Result = "cancelled"
			}
			l.Reason = d.Reason
			if l.VoiceID == "" {
				l.VoiceID = d.VoiceID
			}
		}
	}
	var out []pcSpokenLine
	for _, id := range order {
		l := byOcc[id]
		if l.Result == "" {
			continue // no terminal speech event: still in the fence when the session stopped
		}
		if l.MessageID == "" {
			// A fence record for a commit that never became a message: the
			// non-dialogue OCR noise commits (message_id "unknown", e.g. the
			// 1-3 char fragments the non-box region reads off the animated
			// water) still get a queued utterance slot, and every new commit
			// fences the older slots (speech.cancelled, reason new_commit).
			// The join leaves their ids empty — they are not spoken LINES,
			// the line assertions join on the message id, and the raw fence
			// events stay in the bundle's dev_events.json for diagnosis.
			continue
		}
		l.WAVPath = filepath.Join(fileSinkDir, sessionID)
		out = append(out, *l)
	}
	return out
}

// pcProfileETag reads the profile's row version token (the If-Match value
// the draft-only content writes require, ADR 0015).
func pcProfileETag(t *testing.T, srv http.Handler, token, profileID string) string {
	t.Helper()
	rec := pcDo(t, srv, token, http.MethodGet, "/api/v1/profiles/"+profileID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get profile %s = %d: %s", profileID, rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("GET profile carries no ETag (the If-Match token for the draft write)")
	}
	return etag
}

// pcCharacterIDByName finds a roster row by its exact name through the
// characters API (the placeholders the import creates are named by their
// opaque speaker keys, ADR 0070).
func pcCharacterIDByName(t *testing.T, srv http.Handler, token, profileID, name string) string {
	t.Helper()
	code, body := pcDoJSON(t, srv, token, http.MethodGet, "/api/v1/profiles/"+profileID+"/characters", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("list characters = %d: %v", code, body)
	}
	var list struct {
		Characters []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"characters"`
	}
	if err := json.Unmarshal(mustMarshal(t, body), &list); err != nil {
		t.Fatalf("characters decode: %v", err)
	}
	for _, c := range list.Characters {
		if c.Name == name {
			return c.ID
		}
	}
	t.Fatalf("no character named %s on profile %s (the import's placeholder roster must carry it; roster: %d rows)", name, profileID, len(list.Characters))
	return ""
}

// pcLinkSpeaker records the curator's speaker-link (ADR 0070): the
// source-field speaker identified by the placeholder named key is the
// target character. It is a draft-only write carrying the profile's row
// version as If-Match, and it must re-point at least one line. It returns
// the number of re-pointed lines (counts only, for the bundle).
func pcLinkSpeaker(t *testing.T, srv http.Handler, token, profileID, key, targetID string) int {
	t.Helper()
	placeholder := pcCharacterIDByName(t, srv, token, profileID, key)
	if placeholder == targetID {
		t.Fatalf("the placeholder for %s is the target row itself", maskPCKey(key))
	}
	etag := pcProfileETag(t, srv, token, profileID)
	code, body := pcDoJSON(t, srv, token, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/characters/"+placeholder+"/speaker-link",
		map[string]any{"target_character_id": targetID}, map[string]string{"If-Match": etag})
	if code != http.StatusOK {
		t.Fatalf("speaker-link %s -> character %s = %d: %v", maskPCKey(key), targetID, code, body)
	}
	repointed, _ := body["repointed_messages"].(float64)
	if repointed < 1 {
		t.Fatalf("speaker-link for %s re-pointed %d lines; the scene's speaker must carry lines (the link is the attribution the session resolves)", maskPCKey(key), int(repointed))
	}
	t.Logf("e2e: speaker-link: placeholder %s -> character %s: %d lines re-pointed (confirmed, manual assertion recorded)",
		maskPCKey(key), targetID, int(repointed))
	return int(repointed)
}

// pcLaunchBridge boots one real Dolphin bridge from a savestate
// (--save_state boot, the proven ADR 0043/0054 path) and keeps it alive for
// the test's lifetime. One bridge at a time: the caller stops the previous
// bridge before launching the next (the no-parallel rule).
func pcLaunchBridge(t *testing.T, launchCtx context.Context, dataDir, label, state string) dolphin.Bridge {
	t.Helper()
	disc := slog.New(slog.DiscardHandler)
	b, err := dolphin.New(dolphin.Options{
		DataDir:       filepath.Join(dataDir, "dolphin-"+label),
		LaunchTimeout: pcLaunchTimeout,
		Log:           disc,
	})
	if err != nil {
		t.Fatalf("dolphin.New (%s): %v", label, err)
	}
	if err := b.Launch(launchCtx, dolphin.LaunchSpec{
		SaveState: state,
		// A nil Lifetime would make the 3-minute launch deadline the bridge's
		// lifetime (the bridge self-stops when it cancels): the session
		// outlives the launch, so the lifetime is the process's and the
		// explicit Stop calls (runSession's defer + the cleanup below, plus
		// Pdeathsig on the spawned children) end it.
		Lifetime: context.Background(),
	}); err != nil {
		b.Stop()
		t.Fatalf("dolphin launch (%s, savestate %s): %v", label, filepath.Base(state), err)
	}
	t.Cleanup(func() { b.Stop() })
	st := b.State()
	if st.State != dolphin.StateRunning || st.ImageSHA == "" {
		t.Fatalf("bridge state=%s after launch (%s); want running with the image hash recorded", st.State, label)
	}
	t.Logf("e2e: dolphin %s running from the savestate: display=%s pid=%d (the bridge records the image hash and size; the path is never logged)",
		label, st.Display, st.DolphinPID)
	return b
}

// pcStartSession starts one live Dolphin session against the published
// profile (dev_trace on: the developer evidence the bundle carries).
func pcStartSession(t *testing.T, srv http.Handler, token, profileID string) string {
	t.Helper()
	code, out := pcDoJSON(t, srv, token, http.MethodPost, "/api/v1/sessions", map[string]any{
		"profile_id":   profileID,
		"frame_source": map[string]any{"kind": rt.SourceKindDolphin},
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
	return sessionID
}

// pcStopSession stops the session and reads its final snapshot (the same
// counters the live E2E assertion set reads).
func pcStopSession(t *testing.T, srv http.Handler, token, sessionID string) map[string]any {
	t.Helper()
	rec := pcDo(t, srv, token, http.MethodPost, "/api/v1/sessions/"+sessionID+"/stop", []byte(`{}`), nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("session stop = %d: %s", rec.Code, rec.Body.String())
	}
	rec = pcDo(t, srv, token, http.MethodGet, "/api/v1/sessions/"+sessionID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session get = %d: %s", rec.Code, rec.Body.String())
	}
	var snap map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("session snapshot decode: %v", err)
	}
	return snap
}

// pcWaitSpokenLines polls the durable event log (the developer UI's own
// polling route) until at least minEnded committed lines reached a fully
// played terminal speech event (speech.ended result "ended"). It returns
// the session's WHOLE durable stream so far (the assertion phase reads the
// correlated chains from it) and true. On the deadline it returns the
// stream and false — the caller records the partial stream in the evidence
// bundle (the commit-spacing-vs-TTS-latency timeline a stuck run needs to
// be diagnosed) and then fails. The read cursor advances after the last
// seen seq: a fixed latest-page read drops old events once the store
// outgrows the page, and an advance driver that lost track of its
// in-flight lines would press A into the middle of a playback and fence
// the run's own utterances (speech.cancelled, reason new_commit).
// advance, when non-nil, is called after every poll with the events seen
// SINCE ITS LAST CALL: the headless session has no player, so the test
// presses A for the dialogue box the way the fixture recordings did (their
// manifests' input logs), and its pacing state stays incremental.
func pcWaitSpokenLines(t *testing.T, srv http.Handler, token, sessionID, fileSinkDir string, minEnded int, deadline time.Duration, advance func(newEvs []devEnvelope)) ([]devEnvelope, bool) {
	t.Helper()
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	wait := time.NewTimer(deadline)
	defer wait.Stop()
	var (
		evs    []devEnvelope
		cursor int64 // the last seen seq; the next poll resumes after it
	)
	for {
		select {
		case <-wait.C:
			spoken := pcCollectSpokenLines(evs, fileSinkDir, sessionID)
			t.Logf("e2e: session %s: deadline after %s with %d fully-played of %d needed lines (%d terminal lines so far; the partial stream goes to the evidence bundle)", sessionID, deadline, countSpokenEnded(spoken), minEnded, len(spoken))
			return evs, false
		case <-poll.C:
		}
		page, next, err := pcPollDevEvents(t, srv, token, sessionID, cursor)
		if err != nil {
			t.Fatalf("dev events: %v", err)
		}
		evs = append(evs, page...)
		if len(page) > 0 {
			cursor = next
		}
		if advance != nil {
			advance(page) // the delta: the driver's committed/terminal maps accumulate
		}
		spoken := pcCollectSpokenLines(evs, fileSinkDir, sessionID)
		ended := countSpokenEnded(spoken)
		if ended >= minEnded {
			t.Logf("e2e: session %s: %d fully-played lines (of %d terminal) — collecting the rest of the run", sessionID, ended, len(spoken))
			return evs, true
		}
	}
}

// countSpokenEnded counts a collected set's fully-played lines.
func countSpokenEnded(spoken []pcSpokenLine) int {
	n := 0
	for _, l := range spoken {
		if l.Result == "ended" {
			n++
		}
	}
	return n
}

// pcPollDevEvents reads the session's durable dev event stream after
// sinceSeq through the developer route (the developer UI's own polling
// route, ADR 0053) and follows the page's truncated flag (bounded) so the
// caller can accumulate the whole stream without dropping old events. The
// route's since_seq cursor is strict (seq > since), so the returned
// next_seq is the cursor for the next call. Each page is mirrored to the
// debug log (seqs only, no payloads) so a stream the route fails to return
// in full can be told apart from a client-side drop.
func pcPollDevEvents(t *testing.T, srv http.Handler, token, sessionID string, sinceSeq int64) ([]devEnvelope, int64, error) {
	t.Helper()
	const maxPages = 50 // 50k events: far past one session's whole stream
	var (
		out    []devEnvelope
		cursor = sinceSeq
	)
	for i := 0; i < maxPages; i++ {
		rec := pcDo(t, srv, token, http.MethodGet,
			fmt.Sprintf("/api/v1/dev/events?session_id=%s&since_seq=%d&limit=1000", sessionID, cursor), nil, nil)
		if rec.Code != http.StatusOK {
			return out, cursor, fmt.Errorf("GET dev events = %d: %s", rec.Code, rec.Body.String())
		}
		var page devEventsPage
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			return out, cursor, err
		}
		if debugLog != nil {
			seqs := make([]int64, 0, len(page.Events))
			for _, e := range page.Events {
				seqs = append(seqs, e.Seq)
			}
			debugLog.Printf("dev-events page: since=%d rows=%d truncated=%v next=%d seqs=%v", cursor, len(page.Events), page.Truncated, page.NextSeq, seqs)
		}
		out = append(out, page.Events...)
		if !page.Truncated {
			return out, page.NextSeq, nil
		}
		if page.NextSeq <= cursor {
			return out, cursor, fmt.Errorf("dev events pagination made no progress (since=%d next=%d)", cursor, page.NextSeq)
		}
		cursor = page.NextSeq
	}
	return out, cursor, fmt.Errorf("dev events pagination exceeded %d pages", maxPages)
}

// stateSHA returns a savestate file's SHA-256 for the bundle's manifest
// (the fixture manifest's claim was already verified at skip time).
func stateSHA(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open savestate %s: %v", filepath.Base(path), err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash savestate: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sha256FileLabels returns a WAV file's SHA-256 (for the differing-WAV
// check and the manifest).
func sha256FileLabel(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open wav %s: %v", filepath.Base(path), err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash wav: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func goVersionLabel() string {
	return runtime.Version()
}

// pcAdvanceStall is how long the advance driver waits without a new commit
// (eligible or abstained) before it may stall-press A, so an unrecognised
// or abstained line cannot hold the scene for the whole session deadline.
// It must outlast the box→OCR→Clef→commit pipeline for one line (including
// OCR-jitter re-observations, ~10-20 s in the 2026-10-20 run) — a shorter
// gap lands the press mid-pipeline and fences the line's own commit.
const pcAdvanceStall = 45 * time.Second

// pcBoxStable is how long the observed box text must be UNCHANGED before a
// stall-press is allowed: a box mid-transition churns its reading through
// empty/partial fragments, and a press during that window samples the
// transition (the 2026-10-20 run's fragments — "still halt-aslecp, aren't
// you?" — made Clef answer "none" and the lines committed without speech).
const pcBoxStable = 10 * time.Second

// pcOpeningStall is the advance driver's guard before its FIRST stall-press:
// the scene's opening line gets a long window so a slow first OCR/Clef/TTS
// cycle is never skipped in flight (once the scene has moved, pcAdvanceStall
// applies).
const pcOpeningStall = 90 * time.Second

// pcPressAdvance presses A on the bridge (the same Inject call the proven
// drive tables use, 500 ms hold): the headless session's stand-in for the
// player's dialogue-box advance, the way the fixture recordings' input
// logs show it was pressed at record time.
func pcPressAdvance(t *testing.T, bridge dolphin.Bridge, label string) {
	t.Helper()
	actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer acancel()
	if err := bridge.Inject(actx, dolphin.Buttons{A: true}, dolphin.Axes{}, 500*time.Millisecond); err != nil {
		t.Logf("e2e: %s: advance inject failed: %v", label, err)
	} else {
		t.Logf("e2e: %s: A pressed (advance the dialogue box)", label)
	}
}

func osLabel() string {
	return fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
}

// decisionModelLabel names the configured decision role's model for the
// manifest (or "unconfigured — local only" when absent).
func decisionModelLabel(opts *rt.DecisionOptions) string {
	if opts == nil || opts.Role.BaseURL == "" {
		return "unconfigured — local only"
	}
	return opts.Role.Model
}

// writeJSONFile writes one bundle file (the bundle is gitignored private
// evidence, never a tracked artifact).
func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal bundle file %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write bundle file %s: %v", path, err)
	}
}
