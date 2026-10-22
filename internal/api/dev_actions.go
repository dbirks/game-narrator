package api

// The developer inspector's per-event actions (ADR-0083, docs/spec/09
// "Developer dashboard layout"): recording a trace as a regression fixture in
// the external fixture root, re-running the recorded observation's local
// decision with the current build, exporting the trace's redacted evidence
// bundle, and serving one utterance's stored audio from the speech cache.
//
// Four routes, all under the developer-only /api/v1/dev grant:
//
//   - POST /api/v1/dev/fixtures                  fixture recording (docs/spec/08
//     "fixture recording and browsing"; the browsing half is GET
//     /api/v1/dev/fixtures, ADR-0052)
//   - POST /api/v1/dev/traces/{trace_id}/rerun   the re-run with its diff
//   - GET  /api/v1/dev/traces/{trace_id}/evidence  the redacted export bundle
//   - GET  /api/v1/dev/speech/audio?cache_key=…  the utterance's WAV
//
// What none of these do: re-invoke the model provider (the re-run recomputes
// the local decision path only and says so — ADR-0083 decision 3), apply a
// fix (the correction labels ride POST /api/v1/corrections and are
// record-only, ADR-0052), or write any fixture byte inside the repository
// (AGENTS.md rule 5; the root is $GAME_NARRATOR_FIXTURE_DIR, ADR-0054).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/support"
)

// ── shared trace reading ────────────────────────────────────────────────────

// traceScanPages caps the store pages one trace lookup may consume: a trace
// that spans more than this of the store's history is broken, not slow, and
// the answer says so instead of holding the appliance. The store predicate is
// strict (seq > cursor), so the continuation cursor is the LAST scanned seq —
// +1 would skip the row appended at exactly last+1 between two scan pages
// (the same off-by-one the events page had; dev.go's trace route documents it
// at the source).
const traceScanPages = 50

// loadTraceEvents returns every persisted envelope that carries the named
// trace, in seq order, paging the durable store (which filters by session and
// name, not by trace). It is the action half of GET /api/v1/dev/traces —
// the same scan, kept here so the fixture, re-run and evidence routes read
// one truth and dev.go's page handler stays as-is.
func (s *Server) loadTraceEvents(ctx context.Context, traceID string) ([]events.Envelope, bool, error) {
	if s.devEvents == nil {
		return nil, false, nil
	}
	var matched []events.Envelope
	scanSeq := int64(0)
	for i := 0; i < traceScanPages; i++ {
		page, err := s.devEvents.After(ctx, scanSeq, events.Filter{}, 1000)
		if err != nil {
			return nil, false, err
		}
		for _, e := range page {
			if e.TraceID == traceID {
				matched = append(matched, e)
			}
		}
		if len(page) < 1000 {
			return matched, true, nil
		}
		scanSeq = page[len(page)-1].Seq
	}
	return matched, false, nil
}

// lastEventOf finds the newest envelope of one name in a trace's events and
// decodes its payload (nil when the trace holds none or the payload is not a
// JSON object — a payload it cannot read is passed through as absent, the
// same rule the speech correlation applies).
func lastEventOf(trace []events.Envelope, name string) (events.Envelope, map[string]any, bool) {
	for i := len(trace) - 1; i >= 0; i-- {
		e := trace[i]
		if e.Name != name {
			continue
		}
		var data map[string]any
		if len(e.Data) > 0 {
			if err := json.Unmarshal(e.Data, &data); err != nil {
				return e, nil, false
			}
		}
		return e, data, true
	}
	return events.Envelope{}, nil, false
}

func payloadStr(data map[string]any, key string) string {
	v, _ := data[key].(string)
	return v
}

func payloadNum(data map[string]any, key string) (float64, bool) {
	v, ok := data[key].(float64)
	return v, ok
}

// ── POST /api/v1/dev/fixtures — the trace fixture ───────────────────────────

// devFixtureRecordRequest is the fixture recording's body: the trace whose
// observation, expectation, audio and labels become one replayable unit.
type devFixtureRecordRequest struct {
	TraceID string `json:"trace_id"`
}

// devFixtureEntry is one file of a written fixture: its path inside the
// fixture directory, the content's SHA-256 and its size. The manifest is the
// integrity claim (ADR-0054's shape; a manifest cannot carry its own hash).
type devFixtureEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// devFixtureResponse is the 201 body: the fixture's identity and where it
// landed, so the operator sees exactly where the evidence left the machine.
type devFixtureResponse struct {
	Fixture struct {
		ID              string            `json:"id"`
		Kind            string            `json:"kind"`
		Dir             string            `json:"dir"`
		TraceID         string            `json:"trace_id"`
		SessionID       string            `json:"session_id"`
		ProfileID       string            `json:"profile_id"`
		ProfileRevision int               `json:"profile_revision"`
		Entries         []devFixtureEntry `json:"entries"`
	} `json:"fixture"`
}

// handleDevFixtureRecord answers POST /api/v1/dev/fixtures (docs/spec/08
// "fixture recording"): it reads the trace's persisted envelopes and writes
// one trace fixture under the external fixture root — never into the
// repository (AGENTS.md rule 5). The fixture is the per-occurrence regression
// unit ADR-0083 defines: the stored observation, the recorded expectation,
// the utterance's audio and the frame/crop samples when the stores still
// hold them, plus the labels linked to the trace at save time.
func (s *Server) handleDevFixtureRecord(w http.ResponseWriter, r *http.Request) {
	if s.devEvents == nil || s.sessions == nil || s.store == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "dev-inspection-unavailable",
			"Dev Inspection Unavailable",
			"no event store, session runtime or catalogue is wired in this process; a fixture needs all three")
		return
	}
	var req devFixtureRecordRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	traceID := strings.TrimSpace(req.TraceID)
	if traceID == "" {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-trace-id",
			"Bad Request", "trace_id names the trace to record and is required")
		return
	}
	ctx := r.Context()
	trace, _, err := s.loadTraceEvents(ctx, traceID)
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "internal-error",
			"Internal Error", "reading the event store failed; see the server log for this request_id")
		return
	}
	if len(trace) == 0 {
		s.writeProblem(w, r, http.StatusNotFound, "trace-not-found",
			"Trace Not Found", fmt.Sprintf("no persisted event carries trace id %q in this process", traceID))
		return
	}
	obsEvent, obs, ok := lastEventOf(trace, events.TextObserved)
	if !ok {
		s.writeProblem(w, r, http.StatusUnprocessableEntity, "trace-has-no-observation",
			"Unprocessable Entity", "the trace holds no stored text.observed to record; a fixture is an observation plus its expectation")
		return
	}
	if info, exists := s.sessions.Get(obsEvent.SessionID); !exists {
		s.writeProblem(w, r, http.StatusNotFound, "session-not-found",
			"Session Not Found", fmt.Sprintf(
				"the trace's session %q is no longer in this process, so its profile revision cannot be resolved; the fixture is the run's own record, and it must be saved before the process that ran it restarts",
				obsEvent.SessionID))
		return
	} else {
		content, err := s.store.GetProfileContent(ctx, info.ProfileID)
		if err != nil {
			s.storeProblem(w, r, err, "resolve fixture profile", "profile")
			return
		}
		// The trace's events in seq order, and the expectation's three halves.
		seqs := make([]int64, 0, len(trace))
		for _, e := range trace {
			seqs = append(seqs, e.Seq)
		}
		candEvent, cand, hasCand := lastEventOf(trace, "message.candidates")
		commitEvent, commit, hasCommit := lastEventOf(trace, "message.committed")
		readyEvent, ready, hasReady := lastEventOf(trace, events.SpeechReady)
		endedEvent, ended, hasEnded := lastEventOf(trace, events.SpeechEnded)

		fixtureID := games.NewID()
		dir := filepath.Join(dolphin.FixtureDir(), "trace-fixtures", fixtureID)
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			s.writeProblem(w, r, http.StatusInternalServerError, "fixture-write-failed",
				"Internal Error", "creating the fixture directory failed; check the fixture root is writable")
			return
		}

		// observation.json — the stored observation the re-run replays.
		observation := map[string]any{
			"event_id":               obsEvent.EventID,
			"seq":                    obsEvent.Seq,
			"raw_text":               payloadStr(obs, "raw_text"),
			"text":                   payloadStr(obs, "text"),
			"normalized_key":         payloadStr(obs, "normalized_key"),
			"confidence":             obs["confidence"],
			"text_state":             payloadStr(obs, "text_state"),
			"region_id":              payloadStr(obs, "region_id"),
			"generation":             obs["generation"],
			"frame_seq":              obs["frame_seq"],
			"frame_asset":            payloadStr(obs, "frame_asset"),
			"crop_asset":             payloadStr(obs, "crop_asset"),
			"dialogue_occurrence_id": payloadStr(obs, "dialogue_occurrence_id"),
		}
		// expected.json — what the recorded run produced.
		expected := map[string]any{"candidates": nil, "commit": nil, "decision": nil}
		if hasCand {
			expected["candidates"] = map[string]any{
				"event_id": candEvent.EventID, "seq": candEvent.Seq,
				"reason": payloadStr(cand, "reason"), "margin": cand["margin"],
				"abstain": cand["abstain"], "scene_id": payloadStr(cand, "scene_id"),
				"candidates": cand["candidates"],
			}
		}
		if hasCommit {
			expected["commit"] = map[string]any{
				"event_id": commitEvent.EventID, "seq": commitEvent.Seq,
				"message_id":      payloadStr(commit, "message_id"),
				"speech_eligible": commit["speech_eligible"],
				"reason":          payloadStr(commit, "reason"),
				"speaker_id":      payloadStr(commit, "speaker_id"),
				"speaker_known":   commit["speaker_known"],
				"predictive":      commit["predictive"],
			}
		}
		if decEvent, dec, hasDec := lastEventOf(trace, "decision.resolved"); hasDec {
			expected["decision"] = map[string]any{
				"event_id": decEvent.EventID, "seq": decEvent.Seq,
				"provider": payloadStr(dec, "provider"), "model": payloadStr(dec, "model"),
				"request_id":        payloadStr(dec, "request_id"),
				"request_id_source": payloadStr(dec, "request_id_source"),
				"accepted":          dec["accepted"], "reason": payloadStr(dec, "reason"),
				"latency_ms": dec["latency_ms"],
			}
		}

		files := map[string][]byte{
			"observation.json": marshalIndent(observation),
			"expected.json":    marshalIndent(expected),
			"corrections.json": marshalIndent(s.traceCorrections(ctx, info.ProfileID, traceID)),
		}

		// The dev-trace samples: the full frame and the ROI crop, when the
		// store still holds them (an evicted sample degrades the fixture —
		// the manifest simply does not name it, the ADR's documented mode).
		for name, key := range map[string]string{"frame": "frame_asset", "crop": "crop_asset"} {
			sha := payloadStr(obs, key)
			if sha == "" {
				continue
			}
			rc, info2, err := s.sessions.OpenDevAsset(ctx, sha)
			if err != nil {
				continue
			}
			b, rerr := io.ReadAll(rc)
			rc.Close()
			if rerr != nil {
				continue
			}
			files[filepath.Join("frames", name+assetExtension(info2.MediaType))] = b
		}

		// The utterance's audio, when the trace's speech.ready carries the
		// cache key and the cache still holds the entry the session played.
		if hasReady {
			speechJSON := map[string]any{
				"event_id": readyEvent.EventID, "seq": readyEvent.Seq,
				"cache_key":      payloadStr(ready, "cache_key"),
				"voice_id":       payloadStr(ready, "voice_id"),
				"voice_revision": ready["voice_revision"],
				"from_cache":     ready["from_cache"],
				"voice_route":    payloadStr(ready, "voice_route"),
			}
			if hasEnded {
				speechJSON["ended_event_id"] = endedEvent.EventID
				speechJSON["result"] = payloadStr(ended, "result")
				speechJSON["reason"] = payloadStr(ended, "reason")
			}
			files["speech.json"] = marshalIndent(speechJSON)
			if wav, ok := s.speechAudioBytes(ctx, payloadStr(ready, "cache_key")); ok {
				files[filepath.Join("speech", "utterance.wav")] = wav
			}
		}

		if err := writeFixtureFiles(dir, files); err != nil {
			s.writeProblem(w, r, http.StatusInternalServerError, "fixture-write-failed",
				"Internal Error", "writing the fixture files failed: "+err.Error())
			return
		}
		entries, err := manifestFixtureEntries(dir, files)
		if err != nil {
			s.writeProblem(w, r, http.StatusInternalServerError, "fixture-manifest-failed",
				"Internal Error", "computing the fixture manifest failed: "+err.Error())
			return
		}
		manifest := map[string]any{
			"schema":     1,
			"kind":       "trace-fixture",
			"id":         fixtureID,
			"created_at": time.Now().UTC().Format(time.RFC3339),
			"source": map[string]any{
				"trace_id":               traceID,
				"session_id":             obsEvent.SessionID,
				"profile_id":             info.ProfileID,
				"profile_revision":       content.Profile.RevisionNum,
				"dialogue_occurrence_id": payloadStr(obs, "dialogue_occurrence_id"),
				"event_seqs":             seqs,
			},
			"entries": entries,
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), marshalIndent(manifest), 0o600); err != nil {
			s.writeProblem(w, r, http.StatusInternalServerError, "fixture-write-failed",
				"Internal Error", "writing the fixture manifest failed: "+err.Error())
			return
		}

		var resp devFixtureResponse
		resp.Fixture.ID = fixtureID
		resp.Fixture.Kind = "trace-fixture"
		resp.Fixture.Dir = dir
		resp.Fixture.TraceID = traceID
		resp.Fixture.SessionID = obsEvent.SessionID
		resp.Fixture.ProfileID = info.ProfileID
		resp.Fixture.ProfileRevision = content.Profile.RevisionNum
		resp.Fixture.Entries = entries
		s.auditCatalogWrite(r, "dev.fixture.recorded", map[string]any{
			"fixture_id": fixtureID, "trace_id": traceID, "session_id": obsEvent.SessionID,
		})
		writeJSON(w, http.StatusCreated, resp)
	}
}

// traceCorrections is the profile's corrections whose evidence names the
// trace (the ADR-0083 correlation member: the "trace:<id>" evidence entry,
// the string-array column ADR-0052 shapes for "frame/observation ids"),
// bounded by the list bound and newest-first from the store. A read failure
// degrades to no labels rather than failing the save: the fixture is the
// observation's record, and the labels are a convenience that rides along.
func (s *Server) traceCorrections(ctx context.Context, profileID, traceID string) []map[string]any {
	rows, err := s.store.ListCorrections(ctx, store.CorrectionFilter{ProfileID: profileID, Limit: correctionsListLimit})
	if err != nil {
		return []map[string]any{}
	}
	needle := []byte(`"trace:` + traceID + `"`)
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		if !bytes.Contains(c.Evidence, needle) {
			continue
		}
		var evidence []any
		if len(c.Evidence) > 0 {
			_ = json.Unmarshal(c.Evidence, &evidence)
		}
		out = append(out, map[string]any{
			"id": c.ID, "kind": c.Kind.String(), "target_type": c.TargetType.String(),
			"target_id": c.TargetID, "message_id": c.MessageID,
			"before_value": c.BeforeValue, "after_value": c.AfterValue,
			"reason": c.Reason, "evidence": evidence, "actor": c.Actor,
			"status": c.Status.String(), "created_at": c.CreatedAt,
		})
	}
	return out
}

// writeFixtureFiles writes one fixture's files under dir, creating the
// subdirectories they name. Files land 0600 and directories 0700: the
// evidence tier is private by default, the way the dev-trace store is.
func writeFixtureFiles(dir string, files map[string][]byte) error {
	for path, data := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// manifestFixtureEntries hashes every fixture entry on disk (the bytes as
// written, read back — the manifest claims what is there, not what was meant)
// and returns the rows sorted by path, so a fixture is byte-identical for
// identical inputs.
func manifestFixtureEntries(dir string, files map[string][]byte) ([]devFixtureEntry, error) {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]devFixtureEntry, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			return nil, fmt.Errorf("re-read %s: %w", path, err)
		}
		sum := sha256.Sum256(data)
		out = append(out, devFixtureEntry{
			Path:   path,
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(data)),
		})
	}
	return out, nil
}

// assetExtension maps a dev-trace sample's content type to a filename
// extension the fixture's manifest can name.
func assetExtension(mediaType string) string {
	switch mediaType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	default:
		return ".bin"
	}
}

// ── POST /api/v1/dev/traces/{trace_id}/rerun — the re-run ───────────────────

// devRerunCandidate is one re-ranked message: the same decomposition the
// recorded candidates carry (score, probability, components) so the diff the
// page renders compares like with like.
type devRerunCandidate struct {
	MessageID   string             `json:"message_id"`
	Score       float64            `json:"score"`
	Probability float64            `json:"probability"`
	Components  map[string]float64 `json:"components,omitempty"`
}

// devRerunRanking is one side of the re-run: the candidate table, the
// winner's margin, the matcher's reason code and the abstain flag.
type devRerunRanking struct {
	Candidates []devRerunCandidate `json:"candidates"`
	Margin     float64             `json:"margin"`
	Reason     string              `json:"reason"`
	Abstain    bool                `json:"abstain"`
}

// devRerunDiffRow is one candidate's movement between the two rankings: a
// nil rank or probability is the candidate's absence on that side.
type devRerunDiffRow struct {
	MessageID           string   `json:"message_id"`
	PreviousRank        *int     `json:"previous_rank"`
	CurrentRank         *int     `json:"current_rank"`
	PreviousProbability *float64 `json:"previous_probability"`
	CurrentProbability  *float64 `json:"current_probability"`
	Delta               *float64 `json:"delta"`
}

// devRerunDecision is the re-run's honesty note (ADR-0083 decision 3):
// whether the recorded trace involved a provider adjudication, and that this
// route recomputes the local decision path only — the model is not
// re-invoked, because the session's decision wiring is fenced per session.
type devRerunDecision struct {
	ProviderInvolved bool   `json:"provider_involved"`
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	RequestID        string `json:"request_id,omitempty"`
	Note             string `json:"note"`
}

// devRerunResponse is the 200 body: the stored observation it re-ran, the
// recorded ranking, the re-ranked one, the diff and the decision note.
type devRerunResponse struct {
	TraceID     string `json:"trace_id"`
	SessionID   string `json:"session_id"`
	ProfileID   string `json:"profile_id"`
	Observation struct {
		EventSeq      int64   `json:"event_seq"`
		RawText       string  `json:"raw_text"`
		NormalizedKey string  `json:"normalized_key"`
		TextState     string  `json:"text_state"`
		Confidence    float64 `json:"confidence"`
		RegionID      string  `json:"region_id"`
		SceneID       string  `json:"scene_id"`
		Generation    float64 `json:"generation"`
	} `json:"observation"`
	Previous *devRerunRanking  `json:"previous,omitempty"`
	Current  devRerunRanking   `json:"current"`
	Changed  bool              `json:"changed"`
	Diff     []devRerunDiffRow `json:"diff"`
	Decision devRerunDecision  `json:"decision"`
}

// handleDevTraceRerun answers POST /api/v1/dev/traces/{trace_id}/rerun: it
// rebuilds the matcher index from the session's profile revision content
// (the published revision is immutable, so the corpus is the run's own,
// scored by the current build's matcher — the honest meaning of "current
// model" for the local decision path) and re-ranks the trace's last stored
// observation. It reports the recorded provider adjudication as recorded and
// does not re-invoke it (ADR-0083 decision 3).
func (s *Server) handleDevTraceRerun(w http.ResponseWriter, r *http.Request) {
	if s.devEvents == nil || s.sessions == nil || s.store == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "dev-inspection-unavailable",
			"Dev Inspection Unavailable",
			"no event store, session runtime or catalogue is wired in this process; the re-run has nothing to read or score against")
		return
	}
	traceID := strings.TrimSpace(r.PathValue("trace_id"))
	if traceID == "" {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-trace-id",
			"Bad Request", "the trace id in the path is empty")
		return
	}
	ctx := r.Context()
	trace, _, err := s.loadTraceEvents(ctx, traceID)
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "internal-error",
			"Internal Error", "reading the event store failed; see the server log for this request_id")
		return
	}
	if len(trace) == 0 {
		s.writeProblem(w, r, http.StatusNotFound, "trace-not-found",
			"Trace Not Found", fmt.Sprintf("no persisted event carries trace id %q in this process", traceID))
		return
	}
	obsEvent, obs, ok := lastEventOf(trace, events.TextObserved)
	if !ok {
		s.writeProblem(w, r, http.StatusUnprocessableEntity, "trace-has-no-observation",
			"Unprocessable Entity", "the trace holds no stored text.observed to re-run")
		return
	}
	info, exists := s.sessions.Get(obsEvent.SessionID)
	if !exists {
		s.writeProblem(w, r, http.StatusNotFound, "session-not-found",
			"Session Not Found", fmt.Sprintf(
				"the trace's session %q is no longer in this process, so its profile cannot be re-scored; start a new session on the current revision and re-run there",
				obsEvent.SessionID))
		return
	}
	content, err := s.store.GetProfileContent(ctx, info.ProfileID)
	if err != nil {
		s.storeProblem(w, r, err, "read re-run profile", "profile")
		return
	}
	idx, err := matcher.Build(content.Messages, content.Edges, matcher.IndexOptions{})
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "index-build-failed",
			"Internal Error", "building the candidate index for the re-run failed: "+err.Error())
		return
	}

	// The observation the run recorded, as the matcher takes it: the raw OCR
	// text (the pipeline ranks raw, not the normalised key), the engine's
	// confidence, the scene the trace's ranking believed (text.observed
	// carries no scene; the candidates payload does) and the region role's
	// kind hint (the pipeline's per-region hint, same source).
	conf, _ := payloadNum(obs, "confidence")
	sceneID := ""
	if _, cand, hasCand := lastEventOf(trace, "message.candidates"); hasCand {
		sceneID = payloadStr(cand, "scene_id")
	}
	kindHint := ""
	regionID := payloadStr(obs, "region_id")
	for i := range content.Regions {
		if content.Regions[i].ID == regionID {
			if k, ok := content.Regions[i].RegionType.TextKindHint(); ok {
				kindHint = string(k)
			}
			break
		}
	}
	ranking := idx.Rank(matcher.Observation{
		Text:         payloadStr(obs, "raw_text"),
		OCRConf:      conf,
		SceneID:      sceneID,
		TextKindHint: kindHint,
	}, matcher.RankOptions{})

	current := devRerunRanking{
		Margin:  ranking.Margin,
		Reason:  ranking.Reason,
		Abstain: ranking.Abstain,
	}
	current.Candidates = make([]devRerunCandidate, 0, len(ranking.Candidates))
	for _, c := range ranking.Candidates {
		comp := make(map[string]float64, len(c.Components))
		for k, v := range c.Components {
			comp[k] = v
		}
		current.Candidates = append(current.Candidates, devRerunCandidate{
			MessageID: c.MessageID, Score: c.Score, Probability: c.Probability, Components: comp,
		})
	}

	// The previous side: the stored message.candidates of the same trace,
	// or null when the trace never ranked (a commit on local evidence alone
	// without a persisted ranking is the shape the null documents).
	var previous *devRerunRanking
	if _, cand, hasCand := lastEventOf(trace, "message.candidates"); hasCand {
		prev := &devRerunRanking{
			Reason:  payloadStr(cand, "reason"),
			Abstain: cand["abstain"] == true,
		}
		if m, ok := payloadNum(cand, "margin"); ok {
			prev.Margin = m
		}
		cands, _ := cand["candidates"].([]any)
		prev.Candidates = make([]devRerunCandidate, 0, len(cands))
		for _, raw := range cands {
			rec, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			cc := devRerunCandidate{MessageID: payloadStr(rec, "message_id")}
			if v, ok := payloadNum(rec, "probability"); ok {
				cc.Probability = v
			} else if v, ok := payloadNum(rec, "score"); ok {
				cc.Score = v
			}
			if comps, ok := rec["components"].(map[string]any); ok {
				cc.Components = make(map[string]float64, len(comps))
				for k, v := range comps {
					if f, ok := v.(float64); ok {
						cc.Components[k] = f
					}
				}
			}
			prev.Candidates = append(prev.Candidates, cc)
		}
		previous = prev
	}

	// The diff: the union of both sides' message ids, previous's order first
	// then current's additions, each with its ranks, probabilities and the
	// probability delta where both sides have one.
	rankOf := func(rs []devRerunCandidate) map[string]int {
		m := make(map[string]int, len(rs))
		for i, c := range rs {
			if _, seen := m[c.MessageID]; !seen {
				m[c.MessageID] = i + 1
			}
		}
		return m
	}
	prevRanks, curRanks := rankOf(previousOrEmpty(previous)), rankOf(current.Candidates)
	diff := make([]devRerunDiffRow, 0, len(prevRanks)+len(curRanks))
	seen := make(map[string]bool, len(prevRanks)+len(curRanks))
	addRow := func(id string, prev, cur *devRerunCandidate) {
		if seen[id] {
			return
		}
		seen[id] = true
		row := devRerunDiffRow{MessageID: id}
		if prev != nil {
			p := int(prevRanks[id])
			row.PreviousRank = &p
			pp := prev.Probability
			row.PreviousProbability = &pp
		}
		if cur != nil {
			c := int(curRanks[id])
			row.CurrentRank = &c
			cp := cur.Probability
			row.CurrentProbability = &cp
			if row.PreviousProbability != nil {
				d := cp - *row.PreviousProbability
				row.Delta = &d
			}
		}
		diff = append(diff, row)
	}
	for _, pc := range previousOrEmpty(previous) {
		var cc *devRerunCandidate
		for j := range current.Candidates {
			if current.Candidates[j].MessageID == pc.MessageID {
				cc = &current.Candidates[j]
				break
			}
		}
		addRow(pc.MessageID, &pc, cc)
	}
	for _, cc := range current.Candidates {
		var pc *devRerunCandidate
		if previous != nil {
			for j := range previous.Candidates {
				if previous.Candidates[j].MessageID == cc.MessageID {
					pc = &previous.Candidates[j]
					break
				}
			}
		}
		addRow(cc.MessageID, pc, &cc)
	}

	// The decision note: the recorded adjudication, as recorded.
	decision := devRerunDecision{Note: rerunDecisionNote}
	if _, dec, hasDec := lastEventOf(trace, "decision.resolved"); hasDec {
		decision.ProviderInvolved = payloadStr(dec, "request_id") != ""
		decision.Provider = payloadStr(dec, "provider")
		decision.Model = payloadStr(dec, "model")
		decision.RequestID = payloadStr(dec, "request_id")
	}

	prevTop, curTop := "", ""
	if previous != nil && len(previous.Candidates) > 0 {
		prevTop = previous.Candidates[0].MessageID
	}
	if len(current.Candidates) > 0 {
		curTop = current.Candidates[0].MessageID
	}
	// changed is the decision-level flip: a different winner, or the abstain
	// flag moving, between the recorded and the re-ranked evidence. A trace
	// with no stored ranking has no baseline to change, and its diff rows say
	// so (every previous_* null) instead of claiming a change.
	changed := previous != nil && (prevTop != curTop || previous.Abstain != current.Abstain)

	var resp devRerunResponse
	resp.TraceID = traceID
	resp.SessionID = obsEvent.SessionID
	resp.ProfileID = info.ProfileID
	resp.Observation.EventSeq = obsEvent.Seq
	resp.Observation.RawText = payloadStr(obs, "raw_text")
	resp.Observation.NormalizedKey = payloadStr(obs, "normalized_key")
	resp.Observation.TextState = payloadStr(obs, "text_state")
	resp.Observation.Confidence = conf
	resp.Observation.RegionID = regionID
	resp.Observation.SceneID = sceneID
	if g, ok := payloadNum(obs, "generation"); ok {
		resp.Observation.Generation = g
	}
	resp.Previous = previous
	resp.Current = current
	resp.Changed = changed
	resp.Diff = diff
	resp.Decision = decision
	writeJSON(w, http.StatusOK, resp)
}

// rerunDecisionNote is the re-run's fixed honesty statement (ADR-0083
// decision 3): the route recomputes the local decision path with the current
// build's matcher; the model adjudication is not re-invoked, and re-running
// the session's replay is the model's own re-run.
const rerunDecisionNote = "re-run recomputes the local decision path (the current build's matcher over the run's immutable revision); " +
	"the model adjudication is not re-invoked by this route — the session's decision wiring is fenced per session — re-run the session's replay for a model re-run"

func previousOrEmpty(p *devRerunRanking) []devRerunCandidate {
	if p == nil {
		return nil
	}
	return p.Candidates
}

// ── GET /api/v1/dev/traces/{trace_id}/evidence — the export bundle ─────────

// redactJSONValue walks one decoded JSON value and scrubs every string with
// the support bundle's redaction rules (ADR-0042): URLs and their hosts,
// bearer/authorization material, bare API keys, private IPs, home paths and
// email addresses. Structure, numbers and the gameplay text the developer
// role may already see on the page pass through unchanged (ADR-0083
// decision 4).
func redactJSONValue(v any) any {
	switch t := v.(type) {
	case string:
		return support.RedactText(t)
	case map[string]any:
		for k, val := range t {
			t[k] = redactJSONValue(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = redactJSONValue(t[i])
		}
		return t
	default:
		return v
	}
}

// handleDevTraceEvidence answers GET /api/v1/dev/traces/{trace_id}/evidence:
// one gzip-compressed tar of the trace's export — the redacted event log,
// the stored frame and crop samples when the dev-trace store holds them, the
// utterance's audio when the speech cache holds it, the linked labels and a
// sha256 manifest of every other entry. It is the developer's explicit export
// of the tier the role may already see on the page (docs/spec/08), redacted
// of credentials and network identity; unlike the ADR-0042 support bundle it
// keeps the gameplay text and the frames, because they are the evidence.
func (s *Server) handleDevTraceEvidence(w http.ResponseWriter, r *http.Request) {
	if s.devEvents == nil || s.sessions == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "dev-inspection-unavailable",
			"Dev Inspection Unavailable",
			"no event store or session runtime is wired in this process; the evidence export has nothing to read")
		return
	}
	traceID := strings.TrimSpace(r.PathValue("trace_id"))
	if traceID == "" {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-trace-id",
			"Bad Request", "the trace id in the path is empty")
		return
	}
	ctx := r.Context()
	trace, _, err := s.loadTraceEvents(ctx, traceID)
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "internal-error",
			"Internal Error", "reading the event store failed; see the server log for this request_id")
		return
	}
	if len(trace) == 0 {
		s.writeProblem(w, r, http.StatusNotFound, "trace-not-found",
			"Trace Not Found", fmt.Sprintf("no persisted event carries trace id %q in this process", traceID))
		return
	}
	obsEvent, obs, _ := lastEventOf(trace, events.TextObserved)

	// events.json: every envelope of the trace, string values redacted.
	eventsJSON := make([]any, 0, len(trace))
	for _, e := range trace {
		redacted := map[string]any{
			"schema_version": e.SchemaVersion,
			"event_id":       e.EventID,
			"session_id":     e.SessionID,
			"seq":            e.Seq,
			"name":           e.Name,
			"occurred_at":    e.OccurredAt.UTC().Format(time.RFC3339Nano),
		}
		if e.TraceID != "" {
			redacted["trace_id"] = e.TraceID
		}
		if e.CausationID != "" {
			redacted["causation_id"] = e.CausationID
		}
		var data any = any(map[string]any{})
		if len(e.Data) > 0 {
			var decoded map[string]any
			if err := json.Unmarshal(e.Data, &decoded); err == nil {
				data = redactJSONValue(decoded)
			} else {
				data = redactJSONValue(string(e.Data))
			}
		}
		redacted["data"] = data
		eventsJSON = append(eventsJSON, redacted)
	}

	profileID := ""
	if info, exists := s.sessions.Get(obsEvent.SessionID); exists {
		profileID = info.ProfileID
	}

	entries := map[string][]byte{
		"events.json": marshalIndent(map[string]any{"events": eventsJSON}),
	}
	// The labels linked to the trace at export time (the fixture's same
	// read, degraded to an empty list when the catalogue is unwired).
	if profileID != "" && s.store != nil {
		entries["corrections.json"] = marshalIndent(s.traceCorrections(ctx, profileID, traceID))
	}
	// The stored samples, when present (same degradation as the fixture: the
	// bundle names what is there).
	if obs != nil {
		for name, key := range map[string]string{"frame": "frame_asset", "crop": "crop_asset"} {
			sha := payloadStr(obs, key)
			if sha == "" {
				continue
			}
			rc, info2, err := s.sessions.OpenDevAsset(ctx, sha)
			if err != nil {
				continue
			}
			b, rerr := io.ReadAll(rc)
			rc.Close()
			if rerr != nil {
				continue
			}
			entries[filepath.Join("assets", name+assetExtension(info2.MediaType))] = b
		}
		if readyEvent, ready, hasReady := lastEventOf(trace, events.SpeechReady); hasReady {
			_ = readyEvent
			speechJSON := map[string]any{
				"cache_key":   payloadStr(ready, "cache_key"),
				"voice_id":    payloadStr(ready, "voice_id"),
				"voice_route": payloadStr(ready, "voice_route"),
			}
			entries["speech.json"] = marshalIndent(speechJSON)
			if wav, ok := s.speechAudioBytes(ctx, payloadStr(ready, "cache_key")); ok {
				entries[filepath.Join("speech", "utterance.wav")] = wav
			}
		}
	}

	// The manifest: the correlation block, the redaction note and the hashes
	// of every other entry (it cannot carry its own hash).
	manifestEntries := make([]devFixtureEntry, 0, len(entries))
	for path := range entries {
		sum := sha256.Sum256(entries[path])
		manifestEntries = append(manifestEntries, devFixtureEntry{
			Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(entries[path])),
		})
	}
	sort.Slice(manifestEntries, func(i, j int) bool {
		return manifestEntries[i].Path < manifestEntries[j].Path
	})
	manifest := map[string]any{
		"schema":     1,
		"kind":       "trace-evidence",
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"source":     map[string]any{"trace_id": traceID, "session_id": obsEvent.SessionID, "profile_id": profileID},
		"redaction":  "string values pass the support-bundle redaction rules (urls, credentials, private ips, home paths, emails); gameplay text and the stored samples are kept: this is the developer's export of the tier that role may already view (adr-0083)",
		"entries":    manifestEntries,
	}
	entries["manifest.json"] = marshalIndent(manifest)

	// One tar, gzip-compressed, entries in sorted path order.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		data := entries[path]
		hdr := &tar.Header{Name: path, Mode: 0o600, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			gz.Close()
			s.writeProblem(w, r, http.StatusInternalServerError, "evidence-bundle-failed",
				"Internal Error", "writing the evidence archive failed: "+err.Error())
			return
		}
		if _, err := tw.Write(data); err != nil {
			gz.Close()
			s.writeProblem(w, r, http.StatusInternalServerError, "evidence-bundle-failed",
				"Internal Error", "writing the evidence archive failed: "+err.Error())
			return
		}
	}
	if err := tw.Close(); err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "evidence-bundle-failed",
			"Internal Error", "closing the evidence archive failed: "+err.Error())
		return
	}
	if err := gz.Close(); err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "evidence-bundle-failed",
			"Internal Error", "compressing the evidence archive failed: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="trace-evidence-`+traceID+`.tar.gz"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
	s.auditCatalogWrite(r, "dev.evidence.exported", map[string]any{
		"trace_id": traceID, "session_id": obsEvent.SessionID, "entries": len(entries),
	})
}

// ── GET /api/v1/dev/speech/audio — the utterance's stored WAV ───────────────

// handleDevSpeechAudio answers GET /api/v1/dev/speech/audio?cache_key=…: the
// utterance's stored audio, read from the same speech.Cache instance the
// session's playback used (the voice service the API wires for
// /api/v1/voices). The cache key rides the trace's speech.ready event. The
// bytes are served with the entry's SHA-256 as the ETag and no-store, the
// voice audition audio's pattern; 404 covers evicted and never-stored keys
// alike, and 503 the unwired voice service.
func (s *Server) handleDevSpeechAudio(w http.ResponseWriter, r *http.Request) {
	if s.voices == nil || s.voices.Service == nil || s.voices.Service.Speech == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "voices-unavailable",
			"Voices Unavailable",
			"the voice workflow is not configured in this process; the utterance audio store is not reachable")
		return
	}
	key := speech.CacheKey(strings.TrimSpace(r.URL.Query().Get("cache_key")))
	if len(key) == 0 {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-cache-key",
			"Bad Request", "cache_key names the utterance's audio cache entry and is required")
		return
	}
	if err := key.Validate(); err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-cache-key",
			"Bad Request", "cache_key is not a valid speech cache key")
		return
	}
	res, ok, err := s.voices.Service.Speech.Get(r.Context(), key, true)
	if err != nil {
		s.writeProblem(w, r, http.StatusNotFound, "speech-audio-gone",
			"Speech Audio Gone", "the utterance's audio entry is no longer readable from the store; it may have been evicted")
		return
	}
	if !ok {
		s.writeProblem(w, r, http.StatusNotFound, "speech-audio-not-found",
			"Speech Audio Not Found", "no utterance audio is stored under this cache key in this process")
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("ETag", `"`+res.SHA256+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(res.WAV)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.WAV)
}

// speechAudioBytes reads one utterance's stored WAV by cache key (the
// dev-store half of the fixture and evidence bundles). A missing voice
// service, an unreadable key or an evicted entry all answer "not present":
// the bundle degrades to the reason, it does not fail.
func (s *Server) speechAudioBytes(ctx context.Context, cacheKey string) ([]byte, bool) {
	if cacheKey == "" || s.voices == nil || s.voices.Service == nil || s.voices.Service.Speech == nil {
		return nil, false
	}
	key := speech.CacheKey(cacheKey)
	if err := key.Validate(); err != nil {
		return nil, false
	}
	res, ok, err := s.voices.Service.Speech.Get(ctx, key, true)
	if err != nil || !ok {
		return nil, false
	}
	return res.WAV, true
}

// marshalIndent renders one bundle/fixture file with the stable two-space
// indent every fixture file carries (the ADR-0054 golden-file shape).
func marshalIndent(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// The values here are all built from decoded JSON or plain maps: a
		// marshal failure is a build-time bug, and the honest bytes are the
		// type name, which the caller's manifest will hash as written.
		return []byte(fmt.Sprintf("%v", v))
	}
	return append(b, '\n')
}
