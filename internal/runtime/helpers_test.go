package runtime_test

import (
	"context"
	"fmt"
	"image"
	"image/draw"
	_ "image/png"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// Fixture locations, relative to the repository root (tests run from
// internal/runtime, hence ../../). The synthetic clip and profile are the
// generated docs/spec/11 corpus the pipeline's own replay test replays.
var (
	repoRoot     = filepath.Join("..", "..")
	profilePath  = filepath.Join(repoRoot, "testdata", "synthetic", "profiles", "dialogue-advance-001.json")
	syntheticDir = filepath.Join(repoRoot, "testdata", "synthetic", "replay", "dialogue-advance-001")
)

func corpusPresent(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(syntheticDir, "truth.json")); err != nil {
		t.Skipf("NOT RUN: synthetic replay corpus missing (%v); run scripts/fixtures/gen-synthetic-replay.sh", err)
	}
}

func hasTesseract() bool {
	_, err := exec.LookPath("tesseract")
	return err == nil
}

// env is a runtime Manager wired to a real SQLite store, a real event hub over
// a memory store, and a private dev-trace assets root: the production shape
// with only the OCR engine and frame source swappable.
type env struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	hub    *events.Hub
	mem    *events.MemStore
	mgr    *runtime.Manager
	assets string
	roots  []string
	ocr    ocr.Engine
}

// newEnv builds the environment. With no options the OCR engine is Tesseract
// (the production engine); WithFakeOCR swaps in the scripted engine keyed on
// the clip's truth, so a test can exercise the whole session without a binary.
func newEnv(t *testing.T, opts ...func(*env)) *env {
	t.Helper()
	e := &env{
		t:     t,
		ctx:   context.Background(),
		ocr:   ocr.Tesseract{},
		roots: []string{absPath(t, syntheticDir)},
	}
	for _, opt := range opts {
		opt(e)
	}
	var err error
	e.st, err = store.Open(e.ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := e.st.Migrate(e.ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e.mem = events.NewMemStore()
	e.hub = events.New(events.WithStore(e.mem))
	e.assets = t.TempDir()
	e.mgr, err = runtime.New(runtime.Options{
		Store:       e.st,
		Hub:         e.hub,
		AssetsRoot:  e.assets,
		ReplayRoots: e.roots,
		OCR:         e.ocr,
		Log:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	// Cleanups run LIFO: the manager (which stops live sessions and waits for
	// their terminal events) before the hub, the hub before the store.
	t.Cleanup(func() { _ = e.st.Close() })
	t.Cleanup(func() { _ = e.hub.Close() })
	t.Cleanup(e.mgr.Close)
	return e
}

func (e *env) start(req runtime.StartRequest) (runtime.Info, error) {
	return e.mgr.Start(e.ctx, req)
}

// withFakeOCR swaps the OCR engine for the scripted truth engine; delay slows
// every recognition so a test can hold a session open while asserting
// concurrency rules against it.
func withFakeOCR(script map[string]string, delay time.Duration) func(*env) {
	return func(e *env) {
		e.ocr = &fakeOCR{script: script, delay: delay}
	}
}

// replayStart is the session request for the synthetic clip with the given
// toggles; devTrace off plus fake OCR makes a full session take milliseconds.
func (e *env) replayStart(profileID string, devTrace bool) runtime.StartRequest {
	return runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: syntheticClipDir()},
		Mode:        runtime.ModeConservative,
		DevTrace:    devTrace,
	}
}

func syntheticClipDir() string {
	abs, _ := filepath.Abs(syntheticDir)
	return abs
}

func absPath(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

// seedSyntheticProfile imports the synthetic profile through the store's own
// write path — the same loader the pipeline test uses — and publishes it. The
// store requires 26-character ULID row ids (games.Validate), while the fixture
// speaks friendly ids (demo-page-1); the import maps friendly → ULID and keeps
// the original in source_message_id, exactly the production import does. It
// returns the published profile id and the friendly → ULID message map the
// assertions use to name the commits.
func seedSyntheticProfile(t *testing.T, st *store.Store, publish bool) (string, map[string]string) {
	t.Helper()
	profile, err := scenario.LoadProfile(profilePath)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	game := games.Game{
		ID: games.NewID(), Slug: "synthetic-demo", Title: "Synthetic Demo",
		Platform: "gamecube", UserTags: []byte(`["synthetic"]`), CreatedAt: now,
	}
	if err := st.CreateGame(ctx, game); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	edition := games.Edition{
		ID: games.NewID(), GameID: game.ID, Region: "US", Language: "en",
		Revision: "rev0", Locale: "en_US",
	}
	if err := st.CreateEdition(ctx, edition); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	draft, err := st.CreateDraftProfile(ctx, edition.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}

	byFriendly := map[string]string{}
	msgs := make([]games.Message, 0, len(profile.Messages))
	for _, m := range profile.ToMessages() {
		id := games.NewID()
		byFriendly[m.ID] = id
		row := m
		row.ID = id
		row.ProfileID = draft.ID
		row.SourceMessageID = m.ID
		row.AttributionStatus = games.AttributionUnknown
		row.Revision = 1
		row.CreatedAt = now
		row.UpdatedAt = now
		msgs = append(msgs, row)
	}
	if err := st.PutMessages(ctx, draft.ID, msgs, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	regions := make([]games.Region, 0, len(profile.Regions))
	for _, r := range profile.ToRegions() {
		r.ID = games.NewID()
		r.ProfileID = draft.ID
		r.DetectMethod = games.DetectManual
		r.Revision = 1
		r.UpdatedAt = now
		regions = append(regions, r)
	}
	if err := st.PutRegions(ctx, draft.ID, regions); err != nil {
		t.Fatalf("PutRegions: %v", err)
	}
	edges := profile.ToEdges()
	for i := range edges {
		edges[i].FromID = byFriendly[edges[i].FromID]
		edges[i].ToID = byFriendly[edges[i].ToID]
		edges[i].CreatedAt = now
	}
	if err := st.PutEdges(ctx, draft.ID, edges); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}

	_, rev, err := st.GetProfile(ctx, draft.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if !publish {
		return draft.ID, byFriendly
	}
	if err := st.SetProfileStatus(ctx, draft.ID, games.StatusReview, rev); err != nil {
		t.Fatalf("SetProfileStatus: %v", err)
	}
	// SetProfileStatus moves the row version; the publish token is the fresh one.
	_, rev, err = st.GetProfile(ctx, draft.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	published, err := st.Publish(ctx, draft.ID, rev, "test-author@local")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return published.ID, byFriendly
}

// fakeOCR is the scripted engine from the pipeline's own harness: the pipeline
// hands the engine the *preprocessed* crop, so the key is the hash of the
// image as received — exactly the identity the ROI stage computed (a
// re-preprocess here would hash already-preprocessed pixels and miss).
type fakeOCR struct {
	script map[string]string
	delay  time.Duration
	calls  atomic.Int64
}

func (f *fakeOCR) Name() string { return "fake" }

func (f *fakeOCR) Recognize(ctx context.Context, img image.Image) (ocr.Result, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ocr.Result{}, ctx.Err()
		}
	}
	text, ok := f.script[vision.ImageHash(img)]
	if !ok {
		return ocr.Result{}, fmt.Errorf("fake: no script for this crop")
	}
	return ocr.Result{Text: text, MeanConf: 0.95, Engine: "fake"}, nil
}

// truthScript builds the hash → text map the way the pipeline harness does:
// decode each frame, crop the textbox rect, preprocess with the pipeline's
// recipe, hash it. A recipe drift between the pipeline and this script would
// miss every key and commit nothing, which is exactly the failure the harness
// comments call out.
func truthScript(t *testing.T) map[string]string {
	t.Helper()
	corpusPresent(t)
	truth, err := scenario.LoadTruth(syntheticClipDir())
	if err != nil {
		t.Fatalf("load truth: %v", err)
	}
	out := map[string]string{}
	profile, err := scenario.LoadProfile(profilePath)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	if len(profile.Regions) != 1 {
		t.Fatalf("fixture has %d regions, want 1", len(profile.Regions))
	}
	r := profile.Regions[0]
	for _, row := range truth {
		f, err := os.Open(filepath.Join(syntheticClipDir(), row.File))
		if err != nil {
			t.Fatalf("open frame %s: %v", row.File, err)
		}
		img, _, err := image.Decode(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("decode frame %s: %v", row.File, err)
		}
		// Normalise to RGBA exactly as the pipeline's source does (draw.Draw onto
		// a fresh RGBA): the fixture frames are paletted PNGs, and the crop hash
		// only agrees when both sides hash the same concrete pixel type.
		rgba := image.NewRGBA(img.Bounds())
		draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
		crop := vision.CropNormalized(rgba, vision.Rect{X: r.X, Y: r.Y, W: r.W, H: r.H}, vision.CropOptions{})
		if crop == nil || crop.Bounds().Empty() {
			t.Fatalf("%s: crop is empty", row.File)
		}
		out[vision.ImageHash(vision.Preprocess(crop, vision.DefaultSteps()))] = row.Text
	}
	return out
}

// waitState polls the session until it reaches a terminal state or the deadline.
func waitState(t *testing.T, m *runtime.Manager, id string, deadline time.Duration) runtime.Info {
	t.Helper()
	by := time.Now().Add(deadline)
	for {
		info, ok := m.Get(id)
		if ok {
			switch info.State {
			case runtime.StateStopped, runtime.StateFailed:
				return info
			}
		}
		if time.Now().After(by) {
			t.Fatalf("session %s did not reach a terminal state within %s (last: %+v)", id, deadline, info)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitRunning polls until the session leaves "starting", so a test can assert
// the one-session rule while a run is provably live.
func waitRunning(t *testing.T, m *runtime.Manager, id string) runtime.Info {
	t.Helper()
	by := time.Now().Add(30 * time.Second)
	for {
		info, ok := m.Get(id)
		if ok && info.State == runtime.StateRunning {
			return info
		}
		if time.Now().After(by) {
			t.Fatalf("session %s never reached running (last: %+v)", id, info)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func eventsFrom(t *testing.T, e *env, sessionID string) []events.Envelope {
	t.Helper()
	evs, err := e.mem.After(e.ctx, 0, events.Filter{SessionID: sessionID}, 4096)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return evs
}
