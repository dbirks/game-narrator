// Package e2e — assertion stages (d)–(e) of the live Dolphin E2E
// (task m6-live-e2e) plus the evidence bundle's manifest. Log-safe per
// AGENTS.md rule 5: counts, ids, hashes, timings — never dialogue text.

package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

// assertDolphinSpeech is stage (d): TTS produced audio bytes (a WAV longer
// than 0.5 s in the session's filesink) and the playback was observable
// (speech.started/ended events on the commit's trace, the counters on the
// session snapshot).
func assertDolphinSpeech(t *testing.T, a *dolphinAsserts, ch *dolphinChain) {
	t.Helper()
	if a.session.Audio == nil || a.session.Audio.Sink != "filesink" {
		t.Fatalf("e2e: (d) FAIL: the session has no filesink audio (audio=%+v); the playback is unobservable; bundle %s",
			a.session.Audio, a.bundle)
	}
	sc := a.session.Audio.Speech
	if sc.Requested < 1 || sc.Started < 1 || sc.Ended < 1 {
		t.Fatalf("e2e: (d) FAIL: speech counters do not show a played utterance (requested=%d started=%d ended=%d); bundle %s",
			sc.Requested, sc.Started, sc.Ended, a.bundle)
	}
	if ch.speechStart == nil || ch.speechEnd == nil {
		t.Fatalf("e2e: (d) FAIL: speech.started/ended missing on trace %s (started=%v ended=%v); bundle %s",
			ch.trace, ch.speechStart != nil, ch.speechEnd != nil, a.bundle)
	}
	if ch.speechEndData.Result != "" && ch.speechEndData.Result != "ended" {
		t.Fatalf("e2e: (d) FAIL: the utterance ended with result %q (not a clean playback); voice=%s; bundle %s",
			ch.speechEndData.Result, ch.speechEndData.VoiceID, a.bundle)
	}
	wavs := listWavs(t, a.fileSinkDir, a.sessionID)
	if len(wavs) == 0 {
		t.Fatalf("e2e: (d) FAIL: no WAV written under %s/%s (the filesink is empty); bundle %s",
			a.fileSinkDir, a.sessionID, a.bundle)
	}
	best, bestDur := "", 0.0
	for _, w := range wavs {
		dur, bytesN, err := wavDurationSeconds(w)
		if err != nil {
			t.Fatalf("e2e: (d) FAIL: parse %s: %v", w, err)
		}
		if dur > bestDur {
			best, bestDur = w, dur
		}
		t.Logf("e2e: (d): wav %s bytes=%d duration=%.2fs", filepath.Base(w), bytesN, dur)
	}
	if bestDur <= 0.5 {
		t.Fatalf("e2e: (d) FAIL: longest utterance WAV is %.2fs (<= 0.5s); files under %s/%s; bundle %s",
			bestDur, a.fileSinkDir, a.sessionID, a.bundle)
	}
	if err := copyEvidence(best, filepath.Join(a.bundle, "audio", filepath.Base(best))); err != nil {
		t.Fatalf("e2e: (d): copy %s into the bundle: %v", best, err)
	}
	t.Logf("e2e: (d) PASS: wav=%s duration=%.2fs sink=filesink requested=%d started=%d ended=%d; trace %s",
		filepath.Base(best), bestDur, sc.Requested, sc.Started, sc.Ended, ch.trace)
}

// listWavs reads the session's filesink dir (created lazily by the sink).
func listWavs(t *testing.T, root, sessionID string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("e2e: (d): read filesink dir %s/%s: %v", root, sessionID, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, filepath.Join(root, sessionID, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func copyEvidence(src, dest string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeEvidenceFile(dest, b)
}

// assertDolphinCorrelation is stage (e): the developer API shows the
// correlated chain frame → OCR → candidates → Clef → commit → speech. The
// trace-level links all share the commit's trace id; the Clef request is
// joined on observation_seq + generation (the product's documented join);
// and the scripted check re-reads the chain through the developer traces
// route the inspector uses.
func assertDolphinCorrelation(t *testing.T, a *dolphinAsserts, ch *dolphinChain) {
	t.Helper()
	var missing []string
	for _, link := range []struct {
		name string
		e    *devEnvelope
	}{
		{"text.observed", ch.text},
		{"message.candidates", ch.candidates},
		{"decision.resolved", ch.decResolved},
		{"speech.requested", ch.speechReq},
		{"speech.started", ch.speechStart},
		{"speech.ended", ch.speechEnd},
		{"decision.requested(joined)", ch.decRequested},
	} {
		if link.e == nil {
			missing = append(missing, link.name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("e2e: (e) FAIL: chain on trace %s is missing %v; bundle %s", ch.trace, missing, a.bundle)
	}
	for name, e := range map[string]*devEnvelope{
		"text.observed": ch.text, "message.candidates": ch.candidates,
		"decision.resolved": ch.decResolved, "speech.requested": ch.speechReq,
		"speech.started": ch.speechStart, "speech.ended": ch.speechEnd,
	} {
		if e.TraceID != ch.trace {
			t.Fatalf("e2e: (e) FAIL: %s carries trace %s, not the commit trace %s; bundle %s",
				name, e.TraceID, ch.trace, a.bundle)
		}
	}
	// The occurrence the audio was spoken for must be the commit's:
	// observation-time and commit-time occurrences differ by design (the
	// commit mints the occurrence it speaks), and the trace is what links
	// them — the speech events riding the commit's occurrence is the
	// "this audio is this line" correlation.
	if ch.speechEndData.Occurrence != "" && ch.commitData.OccurrenceID != "" &&
		ch.speechEndData.Occurrence != ch.commitData.OccurrenceID {
		t.Fatalf("e2e: (e) FAIL: speech.ended's dialogue occurrence %s != the commit's %s (the audio is not the committed line); bundle %s",
			ch.speechEndData.Occurrence, ch.commitData.OccurrenceID, a.bundle)
	}
	// The scripted developer-API check: the traces route (the same surface
	// the developer page reads) must show the chain on the commit's trace.
	rec := dolphinDo(t, a.srv, a.token, http.MethodGet,
		"/api/v1/dev/traces/"+ch.trace+"?limit=1000", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("e2e: (e) FAIL: dev traces for %s = %d: %s", ch.trace, rec.Code, rec.Body.String())
	}
	var page devEventsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("e2e: (e): dev traces decode: %v", err)
	}
	have := map[string]bool{}
	for _, e := range page.Events {
		have[e.Name] = true
	}
	for _, want := range []string{"text.observed", "message.candidates", "decision.resolved",
		"message.committed", "speech.requested", "speech.started", "speech.ended"} {
		if !have[want] {
			t.Fatalf("e2e: (e) FAIL: the developer traces route shows %d events on trace %s but %s is absent; bundle %s",
				len(page.Events), ch.trace, want, a.bundle)
		}
	}
	t.Logf("e2e: (e) PASS: correlated chain complete on trace %s (dev traces route: %d events; clef join obs_seq=%d gen=%d)",
		ch.trace, len(page.Events), ch.candData.ObservationSeq, ch.candData.Generation)
}

// dolphinManifest writes the bundle's manifest.json per AGENTS.md: git SHA,
// model ids, emulator build, the game-image hash (the bridge's own
// computation — the image path is never written), timings and the chain's
// ids. It never writes dialogue text.
func dolphinManifest(t *testing.T, a *dolphinAsserts, ch *dolphinChain) {
	t.Helper()
	bin, _ := exec.LookPath("dolphin-emu")
	dolphinVersion, dolphinSHA := "", ""
	if bin != "" {
		dolphinVersion, dolphinSHA = dolphinBinaryInfo(bin)
	}
	bridgeSt := a.bridge.State()
	writeEvidenceJSON(t, filepath.Join(a.bundle, "manifest.json"), map[string]any{
		"run_id":               a.runID,
		"git_sha":              repoGitSHA(),
		"os":                   runtime.GOOS,
		"arch":                 runtime.GOARCH,
		"started_at":           a.startAll.UTC().Format(time.RFC3339),
		"finished_at":          time.Now().UTC().Format(time.RFC3339),
		"duration_seconds":     int64(time.Since(a.startAll).Seconds()),
		"models":               a.models,
		"dolphin_binary":       bin,
		"dolphin_version":      dolphinVersion,
		"dolphin_binary_sha16": dolphinSHA,
		"dolphin_display":      bridgeSt.Display,
		"dolphin_pid":          bridgeSt.DolphinPID,
		"game_image_sha256":    bridgeSt.ImageSHA,
		"game_image_size":      bridgeSt.ImageSize,
		"profile_id":           a.profileID,
		"session_id":           a.sessionID,
		"voice_id":             a.voice,
		"trace_id":             ch.trace,
		"commit_event_id":      ch.commit.EventID,
		"decision_request_id":  ch.decResData.RequestID,
		"drive_reason":         a.drive.Reason,
		"drive_success_screen": string(a.drive.SuccessScreen),
		"session_state":        a.session.State,
		"session_stats":        a.session.Stats,
		"audio":                a.session.Audio,
		"decision_status":      a.session.Decision,
		"timeline_json":        "timeline.json",
		"subtitle_frames":      "frames/",
		"audio_wavs":           "audio/",
	})
	t.Logf("e2e: evidence bundle complete: %s (manifest, timeline, frames, audio)", a.bundle)
}
