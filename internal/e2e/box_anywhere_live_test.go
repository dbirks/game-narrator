package e2e

// ADR-0172 (issue #53): the box-anywhere live scenario. The frame-level panel
// finder must read a dialogue box that no configured region covers.
//
// The scenario runs the REAL pipeline against the REAL emulator: the
// first-dialogue savestate puts the box at the bottom of the screen, but the
// profile's only textbox region is registered at the TOP of the frame — the
// fixed region the profile ships for a different placement — so the
// configured path alone reads nothing. With PanelFind enabled the dynamic
// region ("panel-anywhere") must find the bottom panel, read it with the
// local OCR engine, and commit the line through the usual
// presence → OCR → tracker → matcher path.
//
// Live test (AGENTS.md rule 3 (c)+(d)): the Dolphin emulator with the
// user-supplied image plus the real OCR engine; the private savestate is
// verified by sha. SKIPs as NOT RUN without the prerequisites and without
// GN_BOX_ANYWHERE_E2E=1 — on a fresh checkout nothing here runs and the
// gate says so. Log-safe per rule 5: counts, ids, region ids and timings
// only — never dialogue text, frames or tokens.
//
//	Run with the live env and the shared Dolphin lock:
//
//	bin/dolphin-run.sh env GN_BOX_ANYWHERE_E2E=1 GN_GATE=test-av \
//	  go test -p 1 -count=1 -timeout 25m -v -run TestLiveBoxAnywhere \
//	  ./internal/e2e/

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter/windwaker"
	"github.com/dbirks/game-narrator/internal/gcdisc"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// boxAnywhereShiftedRegion is the configured textbox moved OUT of the
// first-dialogue box's reach: a top-of-frame band. The fixture's box sits at
// the bottom, so the fixed path crops empty scene the whole run — the
// counterfactual the finder exists to break. Invented geometry only (rule 5).
var boxAnywhereShiftedRegion = games.Region{
	ID:         "toponly",
	RegionType: games.RegionTextbox,
	RectX:      0.05,
	RectY:      0.02,
	RectW:      0.80,
	RectH:      0.10,
}

// boxAnywhereRunS is how long the scenario drives the emulator: the box is
// on screen from the savestate, the tracker settles in seconds, and the
// commit is expected well inside this window.
const boxAnywhereRunS = 90 * time.Second

func TestLiveBoxAnywhereDynamicRegion(t *testing.T) {
	if os.Getenv("GN_BOX_ANYWHERE_E2E") != "1" {
		t.Skip("NOT RUN: GN_BOX_ANYWHERE_E2E is not 1 (live scenario: private fixtures + emulator + OCR engine; run it through the shared Dolphin lock)")
	}
	pc := boxAnywherePreconds(t)
	t.Setenv(dolphin.EnvDevDolphin, "1")

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)

	// Durable state, jobs, auth (the av harness's core, minus the voice
	// workflow this scenario does not exercise).
	core := avDurableCore(t, ctx)
	// The import.file job kind the API's import route enqueues onto
	// (registered before Start: the runner refuses new kinds once up).
	if err := core.runner.Register(api.JobKindImportFile,
		api.ImportFileJobHandler(core.st, core.as, core.adapters), jobs.RegisterOpts{
			Concurrency: 1, Resumable: true, Timeout: 5 * time.Minute,
		}); err != nil {
		t.Fatalf("register import job kind: %v", err)
	}
	if err := core.runner.Start(ctx); err != nil {
		t.Fatalf("jobs runner start: %v", err)
	}
	srv := api.New(bootstrap.BuildInfo{Version: "e2e-ba-live"}, time.Now(), core.srvLog,
		api.WithAuth(core.authSvc, store.NewAuthStore(core.st)),
		api.WithStore(core.st),
		api.WithJobs(core.js),
		api.WithEvents(core.hub),
		api.WithImport(core.as, core.adapters, core.runner),
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

	// game → edition → draft profile → import → the SHIFTED region →
	// review → publish (the catch-up test's catalog flow).
	runID := "ba-" + time.Now().UTC().Format("20060102T150405Z")
	code, out := cuDoJSON(t, srv, core.token, "POST", "/api/v1/games",
		map[string]any{"slug": "wind-waker-boxanywhere", "title": "The Legend of Zelda: The Wind Waker",
			"platform": "gamecube", "user_tags": json.RawMessage(`[]`)},
		map[string]string{"Idempotency-Key": runID + "-game"})
	if code != 201 {
		t.Fatalf("create game = %d: %v", code, out)
	}
	gameID, _ := out["id"].(string)
	code, out = cuDoJSON(t, srv, core.token, "POST", "/api/v1/games/"+gameID+"/editions",
		map[string]any{"region": "ntsc-u", "language": "en", "revision": "1", "source_identity": pc.imageSHA},
		map[string]string{"Idempotency-Key": runID + "-edition"})
	if code != 201 {
		t.Fatalf("create edition = %d: %v", code, out)
	}
	editionID, _ := out["id"].(string)
	code, out = cuDoJSON(t, srv, core.token, "POST", "/api/v1/editions/"+editionID+"/profiles", nil,
		map[string]string{"Idempotency-Key": runID + "-profile"})
	if code != 201 {
		t.Fatalf("create draft profile = %d: %v", code, out)
	}
	profileID, _ := out["id"].(string)

	rec := dolphinDoMultipart(t, srv, core.token, "/api/v1/profiles/"+profileID+"/import",
		"windwaker.bmg", bmgBytes, map[string]string{
			"format": "bmg", "rights": "user_supplied", "language": "en",
		}, map[string]string{"Idempotency-Key": runID + "-import"})
	if rec.Code != 202 {
		t.Fatalf("import upload = %d: %s", rec.Code, rec.Body.String())
	}
	var up struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil || up.JobID == "" {
		t.Fatalf("import upload response: %v: %s", err, rec.Body.String())
	}
	job := dolphinWaitJob(t, srv, core.token, up.JobID, 5*time.Minute)
	if job.State != "succeeded" {
		t.Fatalf("import job %s ended %s: %s", up.JobID, job.State, string(job.Result))
	}
	var importReport struct {
		Inserted int `json:"messages_inserted"`
	}
	if err := json.Unmarshal(job.Result, &importReport); err != nil || importReport.Inserted < 1000 {
		t.Fatalf("import job %s: inserted=%d; want >= 1000 (the real script)", up.JobID, importReport.Inserted)
	}
	t.Logf("box-anywhere e2e: import done: %d messages", importReport.Inserted)

	// The one configured region, shifted out of the box's reach.
	regionBody := map[string]any{"regions": []map[string]any{{
		"label": boxAnywhereShiftedRegion.ID, "region_type": "textbox",
		"x": boxAnywhereShiftedRegion.RectX, "y": boxAnywhereShiftedRegion.RectY,
		"w": boxAnywhereShiftedRegion.RectW, "h": boxAnywhereShiftedRegion.RectH,
	}}}
	etag := cuProfileETag(t, srv, core.token, profileID)
	rec = cuDo(t, srv, core.token, "PUT", "/api/v1/profiles/"+profileID+"/regions",
		mustMarshal(t, regionBody), map[string]string{"If-Match": etag})
	if rec.Code != 200 {
		t.Fatalf("put region = %d: %s", rec.Code, rec.Body.String())
	}
	etag = cuProfileETag(t, srv, core.token, profileID)
	rec = cuDo(t, srv, core.token, "POST", "/api/v1/profiles/"+profileID+"/status",
		[]byte(`{"to":"review"}`), map[string]string{"If-Match": etag})
	if rec.Code != 200 {
		t.Fatalf("set status review = %d: %s", rec.Code, rec.Body.String())
	}
	etag = cuProfileETag(t, srv, core.token, profileID)
	rec = cuDo(t, srv, core.token, "POST", "/api/v1/profiles/"+profileID+"/publish",
		[]byte(`{}`), map[string]string{"If-Match": etag})
	if rec.Code != 200 {
		t.Fatalf("publish profile = %d: %s", rec.Code, rec.Body.String())
	}
	t.Logf("box-anywhere e2e: profile %s published with the single SHIFTED (top) region", profileID)

	// The matcher's corpus: the profile's own messages, straight from the
	// store (no runtime session in this scenario — the pipeline is the
	// subject).
	published, _, err := core.st.PublishedProfile(ctx, editionID)
	if err != nil {
		t.Fatalf("PublishedProfile: %v", err)
	}
	content, err := core.st.GetProfileContent(ctx, published.ID)
	if err != nil {
		t.Fatalf("GetProfileContent: %v", err)
	}
	idx, err := matcher.Build(content.Messages, content.Edges, matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("matcher.Build: %v", err)
	}

	// The emulator from the verified first-dialogue savestate.
	launchCtx, launchCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer launchCancel()
	bridge := pcLaunchBridge(t, launchCtx, core.dataDir, "boxanywhere", pc.fdState)

	// The pipeline: the shifted region plus the frame-level panel finder.
	// Tesseract is the production primary engine (the runtime's OCR pool).
	probeCtx, probeCancel := context.WithTimeout(ctx, 20*time.Second)
	defer probeCancel()
	if dbg, derr := bridge.FrameSource(10); derr == nil {
		ch, serr := dbg.Start(probeCtx)
		if serr == nil {
			pf := pipeline.DefaultPanelFind()
			for i := 0; i < 15; i++ {
				select {
				case f, ok := <-ch:
					if !ok || f.Image == nil {
						i = 100 // break
						continue
					}
					b := f.Image.Bounds()
					got := pipeline.FindPanelsImage(f.Image, pf)
					rects := "none"
					if len(got) > 0 {
						rects = fmt.Sprintf("%+v", got[0])
					}
					t.Logf("box-anywhere e2e: probe frame %d %dx%d finder: %s", i, b.Dx(), b.Dy(), rects)
					if len(got) > 0 {
						crop := vision.CropNormalized(f.Image, vision.Rect{X: got[0].X, Y: got[0].Y, W: got[0].W, H: got[0].H}, vision.CropOptions{})
						if crop != nil {
							pp := vision.PanelPresenceOf(crop, 90, vision.DefaultTextLuma, vision.DefaultSatFloor)
							t.Logf("box-anywhere e2e: probe frame %d finder-crop presence: dark=%.3f text=%.4f satDark=%.3f topDark=%.3f", i, pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark)
						}
					}
				case <-probeCtx.Done():
					i = 100
				}
				if i >= 100 {
					break
				}
			}
		}
		_ = dbg.Close()
	}
	cfg := pipeline.Config{
		SessionID:       "boxanywhere-live",
		ProfileRevision: "1",
		Regions:         []games.Region{boxAnywhereShiftedRegion},
		OCR:             ocr.NewPooled(ocr.Tesseract{}, ocr.DefaultPoolSlots),
		Index:           idx,
		Preprocess:      vision.DefaultSteps(),
		OCRTimeout:      5 * time.Second,
		Trace:           true,
		PanelFind:       pipeline.DefaultPanelFind(),
	}
	cfg.PanelFind.Enabled = true
	r, err := pipeline.New(cfg)
	if err != nil {
		bridge.Stop()
		t.Fatalf("pipeline.New: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, boxAnywhereRunS)
	t.Cleanup(runCancel)
	start := time.Now()
	src, err := bridge.FrameSource(10)
	if err != nil {
		bridge.Stop()
		t.Fatalf("bridge frame source: %v", err)
	}
	err = r.Run(runCtx, src, core.hub)
	bridge.Stop()
	if err != nil && runCtx.Err() != nil {
		t.Logf("box-anywhere e2e: run ended with the drive budget: %v (expected on a live source)", err)
		err = nil
	}
	if err != nil {
		t.Fatalf("pipeline run: %v", err)
	}
	t.Logf("box-anywhere e2e: drive complete in %s", time.Since(start).Round(time.Millisecond))

	// Assertion 1: the dynamic region saw a present box and delivered OCR
	// text (region ids only — the text itself stays out of the log, rule 5).
	traces := r.Trace()
	dynPresent, dynRead, fixedPresent := 0, 0, 0
	for _, tr := range traces {
		switch {
		case tr.RegionID == "panel-anywhere" && tr.RegionPresent:
			dynPresent++
			if tr.OCRText != "" {
				dynRead++
			}
		case tr.RegionID == boxAnywhereShiftedRegion.ID && tr.RegionPresent:
			fixedPresent++
		}
	}
	t.Logf("box-anywhere e2e: dynamic-region present observations=%d, with OCR text=%d; shifted region present observations=%d (of %d trace rows)",
		dynPresent, dynRead, fixedPresent, len(traces))
	if dynPresent == 0 {
		t.Fatalf("the dynamic region never reported a present box: the finder did not place the bottom panel")
	}
	if dynRead == 0 {
		t.Fatalf("the dynamic region never delivered OCR text: the read path failed")
	}
	if fixedPresent > 0 {
		t.Logf("note: the shifted top region reported presence %d times (scene noise); the box is the dynamic region's", fixedPresent)
	}

	// Assertion 2: the line committed through the session event stream —
	// the dynamic region's read is what committed it, because the shifted
	// region crops empty scene.
	evs, err := store.NewEventStore(core.st).After(ctx, 0, events.Filter{SessionID: "boxanywhere-live"}, 4096)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	committed, committedIDs := 0, map[string]int{}
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		var d struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("message.committed payload: %v", err)
		}
		committed++
		committedIDs[d.MessageID]++
	}
	t.Logf("box-anywhere e2e: committed lines=%d (distinct message ids=%d; ids stay out of the log, rule 5)", committed, len(committedIDs))
	if committed == 0 {
		t.Fatalf("no line committed with the dynamic region: the finder's read never reached the matcher")
	}
}

// boxAnywherePreconds gates the scenario: the same hard prerequisites as the
// av harness, minus the voice/ASR roles the scenario does not exercise.
func boxAnywherePreconds(t *testing.T) avPreconds {
	t.Helper()
	gate := os.Getenv(dolphin.EnvGate)
	if gate != "test-av" && gate != "test-dolphin" {
		t.Skipf("NOT RUN: %s is %q (want test-av or test-dolphin)", dolphin.EnvGate, gate)
	}
	img := os.Getenv(dolphin.EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set (the user-supplied Wind Waker image, dev-only)", dolphin.EnvGameImage)
	}
	if _, err := os.Stat(img); err != nil {
		t.Skipf("NOT RUN: disc image not found at the %s path (dev-only)", dolphin.EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo", "tesseract"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH", bin)
		}
	}
	if err := strayDolphinProcs(); err != nil {
		t.Skipf("NOT RUN: %v", err)
	}
	root, ok := pcFixtureRoot(t)
	if !ok {
		t.Skipf("NOT RUN: the fixtures are absent (external, gitignored)")
	}
	fdDir := filepath.Join(root, pcFixtureFirstDialogue)
	if _, err := os.Stat(filepath.Join(fdDir, "profile.json")); err != nil {
		t.Skipf("NOT RUN: fixture %s is incomplete (%v)", pcFixtureFirstDialogue, err)
	}
	fdState := pcVerifyState(t, fdDir, pcStateFirstDialogue)
	return avPreconds{
		img: img, imageSHA: dolphinImageSHA(t, img),
		fdDir: fdDir, fdState: fdState,
	}
}
