package runtime_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
)

// friendlyByID inverts the import's friendly → ULID map through
// source_message_id, the way the production import keeps the original ids.
func friendlyByID(t *testing.T, byFriendly map[string]string, ulid string) string {
	t.Helper()
	for friendly, id := range byFriendly {
		if id == ulid {
			return friendly
		}
	}
	t.Fatalf("committed message %s is not one of the seeded messages", ulid)
	return ""
}

// committedMessageIDs pulls the committed message ids (in commit order) out of
// a session's event stream.
func committedMessageIDs(t *testing.T, evs []events.Envelope) []string {
	t.Helper()
	var ids []string
	for _, ev := range evs {
		if ev.Name != events.MessageCommitted {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("message.committed data: %v", err)
		}
		id, _ := data["message_id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// TestSyntheticReplaySessionRealTesseract is the session-layer proof of the
// pipeline's frame-replay gate: real recorded frames, real Tesseract, real
// matcher, real pipeline, real store and real event log — plus the dev-trace
// evidence layer on top (stored frame/crop, referenced from text.observed).
// "Synthetic" is load-bearing: every frame and sentence is generated.
func TestSyntheticReplaySessionRealTesseract(t *testing.T) {
	if !hasTesseract() {
		t.Skip("NOT RUN: tesseract not installed")
	}
	corpusPresent(t)
	e := newEnv(t)
	profileID, byFriendly := seedSyntheticProfile(t, e.st, true)

	info, err := e.start(e.replayStart(profileID, true))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if info.ID == "" || (info.State != runtime.StateStarting && info.State != runtime.StateRunning) {
		t.Fatalf("start info = %+v, want a live session", info)
	}
	final := waitState(t, e.mgr, info.ID, 180*time.Second)
	if final.State != runtime.StateStopped {
		t.Fatalf("state = %q (reason %q, stats %+v), want stopped", final.State, final.Reason, final.Stats)
	}
	if final.Reason != runtime.StopReasonSourceEnd {
		t.Fatalf("reason = %q, want %q", final.Reason, runtime.StopReasonSourceEnd)
	}
	if final.Stats.FramesDelivered != 24 {
		t.Fatalf("frames_delivered = %d, want 24 (the whole clip)", final.Stats.FramesDelivered)
	}
	if final.Stats.FramesDropped != 0 || final.Stats.OCRCancelled != 0 {
		t.Fatalf("clean replay left drops: dropped=%d cancelled=%d", final.Stats.FramesDropped, final.Stats.OCRCancelled)
	}
	if final.Stats.Commits != 2 {
		t.Fatalf("commits = %d (stats %+v), want 2", final.Stats.Commits, final.Stats)
	}

	evs := eventsFrom(t, e, info.ID)
	if evs[0].Name != runtime.EventSessionStarted {
		t.Fatalf("first event = %q, want %s", evs[0].Name, runtime.EventSessionStarted)
	}
	if final.EventCursor != evs[0].Seq {
		t.Fatalf("event_cursor = %d, want the session.started seq %d", final.EventCursor, evs[0].Seq)
	}
	if evs[len(evs)-1].Name != events.SessionStopped {
		t.Fatalf("last event = %q, want session.stopped", evs[len(evs)-1].Name)
	}

	ids := committedMessageIDs(t, evs)
	if len(ids) != 2 {
		t.Fatalf("committed %d messages, want 2: %v", len(ids), ids)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[friendlyByID(t, byFriendly, id)] = true
	}
	if !got["demo-page-1"] || !got["demo-page-2"] {
		t.Fatalf("committed %v, want demo-page-1 and demo-page-2", got)
	}

	// Dev-trace evidence: at least one text.observed carries the stored frame
	// and crop, and both are fetchable with the right media type.
	var frameSHA, cropSHA string
	for _, ev := range evs {
		if ev.Name != events.TextObserved {
			continue
		}
		var data map[string]any
		_ = json.Unmarshal(ev.Data, &data)
		if f, _ := data["frame_asset"].(string); f != "" {
			frameSHA = f
		}
		if c, _ := data["crop_asset"].(string); c != "" {
			cropSHA = c
		}
	}
	if frameSHA == "" || cropSHA == "" {
		t.Fatalf("no text.observed carried frame_asset/crop_asset (dev_trace was on)")
	}
	frameBytes, err := fetchAsset(t, e, frameSHA, "image/jpeg")
	if err != nil {
		t.Fatalf("frame asset: %v", err)
	}
	if len(frameBytes) < 1000 {
		t.Fatalf("frame asset is %d bytes, suspiciously small for a 640px JPEG", len(frameBytes))
	}
	cropBytes, err := fetchAsset(t, e, cropSHA, "image/png")
	if err != nil {
		t.Fatalf("crop asset: %v", err)
	}
	if !bytes.HasPrefix(cropBytes, pngMagic) {
		t.Fatalf("crop asset is not a PNG (first bytes %x)", cropBytes[:4])
	}
	// The events name exactly the content the store served: same hash, both of
	// them, which is what "evidence references the stored bytes" means.
	if frameSHA != shaHex(frameBytes) || cropSHA != shaHex(cropBytes) {
		t.Fatal("asset bytes do not hash to the sha the events referenced")
	}

	// The trace of a commit is readable end to end: same trace_id across the
	// observations that produced it (the pipeline stamps them).
	var traceID string
	for _, ev := range evs {
		if ev.Name == events.MessageCommitted && ev.TraceID != "" {
			traceID = ev.TraceID
			break
		}
	}
	if traceID == "" {
		t.Fatal("message.committed carries no trace_id")
	}
	var inTrace int
	for _, ev := range evs {
		if ev.TraceID == traceID {
			inTrace++
		}
	}
	if inTrace < 2 {
		t.Fatalf("trace %s holds %d events, want at least the commit and its evidence", traceID, inTrace)
	}
}

// TestReplaySessionFakeOCRDevTraceOn proves the same session end to end with
// the scripted engine (no binary dependency): the fake contributes
// recognition only; the pipeline, store, events and dev-trace storage are all
// the real thing.
func TestReplaySessionFakeOCRDevTraceOn(t *testing.T) {
	corpusPresent(t)
	script := truthScript(t)
	e := newEnv(t, withFakeOCR(script, 0))
	profileID, byFriendly := seedSyntheticProfile(t, e.st, true)

	info, err := e.start(e.replayStart(profileID, true))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	final := waitState(t, e.mgr, info.ID, 60*time.Second)
	if final.State != runtime.StateStopped || final.Reason != runtime.StopReasonSourceEnd {
		t.Fatalf("final = %+v, want stopped/source_end", final)
	}
	if final.Stats.Commits != 2 {
		t.Fatalf("commits = %d, want 2", final.Stats.Commits)
	}
	evs := eventsFrom(t, e, info.ID)
	got := map[string]bool{}
	for _, id := range committedMessageIDs(t, evs) {
		got[friendlyByID(t, byFriendly, id)] = true
	}
	if !got["demo-page-1"] || !got["demo-page-2"] {
		t.Fatalf("committed %v, want both demo pages", got)
	}
	// dev trace on: assets exist and are referenced.
	if frameSHA, cropSHA := assetRefs(t, evs); frameSHA == "" || cropSHA == "" {
		t.Fatal("dev_trace on but no text.observed carries frame/crop assets")
	} else {
		if _, err := fetchAsset(t, e, frameSHA, "image/jpeg"); err != nil {
			t.Fatalf("frame asset: %v", err)
		}
		if _, err := fetchAsset(t, e, cropSHA, "image/png"); err != nil {
			t.Fatalf("crop asset: %v", err)
		}
	}
}

// TestDevTraceOffStoresNothing is the privacy rule in concrete form: without
// the developer toggle the pipeline stores no images at all, and the events
// carry no references to any.
func TestDevTraceOffStoresNothing(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t, withFakeOCR(truthScript(t), 0))
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	info, err := e.start(e.replayStart(profileID, false))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	final := waitState(t, e.mgr, info.ID, 60*time.Second)
	if final.State != runtime.StateStopped {
		t.Fatalf("state = %q, want stopped", final.State)
	}
	if final.Stats.Commits != 2 {
		t.Fatalf("commits = %d, want 2", final.Stats.Commits)
	}
	for _, ev := range eventsFrom(t, e, info.ID) {
		if ev.Name != events.TextObserved {
			continue
		}
		var data map[string]any
		_ = json.Unmarshal(ev.Data, &data)
		if _, hasFrame := data["frame_asset"]; hasFrame {
			t.Fatal("text.observed carries frame_asset with dev_trace off")
		}
		if _, hasCrop := data["crop_asset"]; hasCrop {
			t.Fatal("text.observed carries crop_asset with dev_trace off")
		}
	}
	entries, err := os.ReadDir(e.assets)
	if err != nil {
		t.Fatalf("read assets root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("assets root holds %d entries with dev_trace off: %v", len(entries), names(entries))
	}
}

// TestOneSessionAtATime covers the foreground invariant and the idempotent
// stop: a second start while one is live is refused, and after the first is
// stopped (twice, because stop is a no-op after the first) the box accepts a
// new session.
func TestOneSessionAtATime(t *testing.T) {
	corpusPresent(t)
	// A slow fake OCR keeps the session provably live for the assertions.
	e := newEnv(t, withFakeOCR(truthScript(t), 250*time.Millisecond))
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	first, err := e.start(e.replayStart(profileID, false))
	if err != nil {
		t.Fatalf("start first: %v", err)
	}
	waitRunning(t, e.mgr, first.ID)

	_, err = e.start(e.replayStart(profileID, false))
	if !errors.Is(err, runtime.ErrAnotherSessionRunning) {
		t.Fatalf("second start = %v, want ErrAnotherSessionRunning", err)
	}

	stopped, err := e.mgr.Stop(first.ID)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.State != runtime.StateStopped || stopped.Reason != runtime.StopReasonRequested {
		t.Fatalf("stop = %+v, want stopped/requested", stopped)
	}
	again, err := e.mgr.Stop(first.ID)
	if err != nil {
		t.Fatalf("idempotent stop: %v", err)
	}
	if again.State != runtime.StateStopped {
		t.Fatalf("second stop = %+v, want still stopped", again)
	}

	second, err := e.start(e.replayStart(profileID, false))
	if err != nil {
		t.Fatalf("start after stop: %v", err)
	}
	final := waitState(t, e.mgr, second.ID, 60*time.Second)
	if final.State != runtime.StateStopped {
		t.Fatalf("second session = %+v, want stopped", final)
	}

	if _, err := e.mgr.Stop("01H0000000000000000000000X"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("stop unknown = %v, want ErrSessionNotFound", err)
	}
}

// TestForegroundBusyPausesJobs is docs/spec/01's "background work pauses
// between jobs while [a live session] is set", observed through a real jobs
// runner: a job enqueued mid-session stays queued until the session ends.
// The env pieces are built here, not through newEnv, because the jobs runner
// must exist before the manager is constructed and the store before both.
func TestForegroundBusyPausesJobs(t *testing.T) {
	corpusPresent(t)
	script := truthScript(t)
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hub := events.New(events.WithStore(events.NewMemStore()))
	t.Cleanup(func() { _ = hub.Close() })

	js := store.NewJobsStore(st)
	runner, err := jobs.NewRunner(js, hub, slog.New(slog.DiscardHandler),
		jobs.WithPollInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if err := runner.Register("system.noop",
		func(ctx context.Context, job jobs.Job, _ jobs.ProgressFn) (json.RawMessage, error) {
			return nil, nil
		}, jobs.RegisterOpts{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}
	t.Cleanup(func() { _ = runner.Stop(ctx) })

	mgr, err := runtime.New(runtime.Options{
		Store:       st,
		Hub:         hub,
		Jobs:        runner,
		AssetsRoot:  t.TempDir(),
		ReplayRoots: []string{syntheticClipDir()},
		OCR:         &fakeOCR{script: script, delay: 250 * time.Millisecond},
		Log:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(mgr.Close)

	profileID, _ := seedSyntheticProfile(t, st, true)
	info, err := mgr.Start(ctx, runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: syntheticClipDir()},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRunning(t, mgr, info.ID)

	job, created, err := js.Enqueue(ctx, store.EnqueueParams{Kind: "system.noop"})
	if err != nil || !created {
		t.Fatalf("enqueue: %v created=%v", err, created)
	}
	time.Sleep(700 * time.Millisecond)
	got, err := js.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.State != store.JobQueued {
		t.Fatalf("job state = %q while a session is live; foreground-busy must pause background work", got.State)
	}

	if _, err := mgr.Stop(info.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	by := time.Now().Add(15 * time.Second)
	for {
		got, err := js.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if got.State == store.JobSucceeded {
			break
		}
		if time.Now().After(by) {
			t.Fatalf("job state = %q after the session ended; it should have been claimed", got.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// assetRefs returns the frame/crop shas named by the first text.observed that
// carries both, and reports them.
func assetRefs(t *testing.T, evs []events.Envelope) (string, string) {
	t.Helper()
	for _, ev := range evs {
		if ev.Name != events.TextObserved {
			continue
		}
		var data map[string]any
		_ = json.Unmarshal(ev.Data, &data)
		frame, _ := data["frame_asset"].(string)
		crop, _ := data["crop_asset"].(string)
		if frame != "" && crop != "" {
			return frame, crop
		}
	}
	return "", ""
}

func fetchAsset(t *testing.T, e *env, hash, wantType string) ([]byte, error) {
	t.Helper()
	rc, info, err := e.mgr.OpenDevAsset(e.ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("open asset %s: %w", hash[:12], err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read asset: %w", err)
	}
	if info.MediaType != wantType {
		return b, fmt.Errorf("asset %s media type = %q, want %q", hash[:12], info.MediaType, wantType)
	}
	return b, nil
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, en := range entries {
		out = append(out, en.Name())
	}
	return out
}

var pngMagic = []byte{0x89, 'P', 'N', 'G'}

func shaHex(b []byte) string {
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}
