package main

// Tests for the one-shot `ocr` command (issue #1): the --image path against
// the repo's synthetic controlled asset with the real engine, the JSON
// schema, --save-debug output, the usage contract, and the --game path
// against a seeded store (the synthetic corpus plus one line that matches
// the controlled image's render, so the matcher's answer is assertable end
// to end). No real game frames or game text: the controlled asset is the
// same synthetic, copyright-free render internal/doctor and the hardware
// gate use (AGENTS.md rule 5).

import (
	"context"
	"encoding/json"
	"image/png"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

const (
	// ocrControlledImage is the synthetic controlled OCR asset (a "NARRATOR 42"
	// render, generated with ImageMagick; no game content).
	ocrControlledImage = "../../internal/doctor/testdata/ocr-check.png"
	// ocrProfilePath is the synthetic corpus the seeded profile ranks against.
	ocrProfilePath = "../../testdata/synthetic/profiles/dialogue-advance-001.json"
)

func ocrLog() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func ocrHasTesseract() bool {
	_, err := exec.LookPath("tesseract")
	return err == nil
}

// scaleOCRTestBudget scales the one-shot command's engine budget for the
// real-Tesseract tests: the 5× (or 1×) comes from the package's
// race_on_test.go / race_off_test.go — the unscaled 4 s default timed out
// a region read on the 2-core -race runner (CI run 39424864849, this
// package's profile test; the pipeline and vision/ocr packages' 39090252213
// precedent). Call it before any run in the test.
func scaleOCRTestBudget() {
	ocrEngineBudget = ocr.DefaultTimeout * time.Duration(ocrEngineBudgetScale)
}

// TestRunOCRUsage pins the command's contract: exactly one of --device /
// --image, the flag bounds, and exit 2 on violations (the CLI targets in
// AGENTS.md: usage errors are 2).
func TestRunOCRUsage(t *testing.T) {
	log := ocrLog()
	cases := []struct {
		name string
		args []string
	}{
		{"no input", []string{"--data-dir", t.TempDir()}},
		{"both inputs", []string{"--data-dir", t.TempDir(), "--image", ocrControlledImage, "--device", "/dev/video0"}},
		{"top below the bound", []string{"--data-dir", t.TempDir(), "--top", "0", "--image", ocrControlledImage}},
		{"top above the matcher bound", []string{"--data-dir", t.TempDir(), "--top", "51", "--image", ocrControlledImage}},
		{"unknown flag", []string{"--data-dir", t.TempDir(), "--image", ocrControlledImage, "--nope"}},
		{"positional argument", []string{"--data-dir", t.TempDir(), "--image", ocrControlledImage, "extra"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, err := run(append([]string{"ocr"}, c.args...), log, nil)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (err %v)", code, err)
			}
			if err == nil {
				t.Fatal("usage violation returned a nil error")
			}
		})
	}
}

// TestRunOCRImageJSON is the --image path end to end against the controlled
// asset: the real crop/preprocess/Tesseract path, the report's JSON schema
// and the read itself.
func TestRunOCRImageJSON(t *testing.T) {
	if !ocrHasTesseract() {
		t.Skip("NOT RUN: tesseract is not installed on this host; the --image path needs the real engine (install it and re-run, docs/spec/10)")
	}
	scaleOCRTestBudget()
	log := ocrLog()
	dataDir := t.TempDir()
	captured := captureStdout(t, func() int {
		code, err := run([]string{"ocr", "--data-dir", dataDir, "--image", ocrControlledImage, "--json"}, log, nil)
		if err != nil {
			t.Fatalf("ocr: %v", err)
		}
		return code
	})
	if captured.code != 0 {
		t.Fatalf("exit code = %d, want 0 (output:\n%s)", captured.code, captured.output)
	}

	var rep struct {
		Command string `json:"command"`
		Source  string `json:"source"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
		Regions []struct {
			ID         string                       `json:"id"`
			Label      string                       `json:"label"`
			Rect       struct{ X, Y, W, H float64 } `json:"rect"`
			Crop       struct{ X, Y, W, H int }     `json:"crop"`
			Present    bool                         `json:"present"`
			Empty      bool                         `json:"empty"`
			Text       string                       `json:"text"`
			Normalized string                       `json:"normalized"`
			MeanConf   float64                      `json:"mean_conf"`
			Engine     string                       `json:"engine"`
			DurationMS int64                        `json:"duration_ms"`
			Error      string                       `json:"error"`
		} `json:"regions"`
		TotalMS int64 `json:"total_ms"`
	}
	if err := json.Unmarshal([]byte(captured.output), &rep); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, captured.output)
	}
	if rep.Command != "ocr" || rep.Width != 400 || rep.Height != 100 {
		t.Fatalf("report header = %+v, want command ocr and the controlled asset's 400x100 frame", rep)
	}
	if rep.TotalMS <= 0 {
		t.Fatalf("total_ms = %d, want a measured wall time", rep.TotalMS)
	}
	if len(rep.Regions) != 1 {
		t.Fatalf("regions = %d, want the single fallback region (no game named)", len(rep.Regions))
	}
	r := rep.Regions[0]
	if r.ID != "frame" || r.Error != "" {
		t.Fatalf("region = %+v, want the fallback region without an error", r)
	}
	// The whole active picture: the controlled asset carries no black bars,
	// so the crop is the whole 400x100 frame.
	if r.Crop.W != 400 || r.Crop.H != 100 || r.Rect.W != 1 || r.Rect.H != 1 {
		t.Fatalf("crop/rect = %+v/%+v, want the whole frame", r.Crop, r.Rect)
	}
	if r.Engine != "tesseract" {
		t.Fatalf("engine = %q, want tesseract (the production engine)", r.Engine)
	}
	// The controlled read: "NARRATOR 42" — the same ground truth the doctor
	// and the hardware gate bound, so this asserts the pipeline path, not
	// the engine alone (the engine is the same struct the pipeline runs).
	got := strings.Join(strings.Fields(r.Text), " ")
	if cr := cer(got, "NARRATOR 42"); cr > 0.05 {
		t.Fatalf("controlled read = %q (CER %.1f%%), want the asset's render within 5%%", got, cr*100)
	}
	if r.MeanConf <= 0 || r.MeanConf > 1 {
		t.Fatalf("mean_conf = %v, want in (0,1]", r.MeanConf)
	}
	if r.DurationMS < 0 {
		t.Fatalf("duration_ms = %d, want non-negative", r.DurationMS)
	}
}

// TestRunOCRImageSaveDebug writes the debug images and checks they are the
// right files: the input image, the raw crop and the preprocessed input,
// each a decodable PNG.
func TestRunOCRImageSaveDebug(t *testing.T) {
	if !ocrHasTesseract() {
		t.Skip("NOT RUN: tesseract is not installed on this host (install it and re-run, docs/spec/10)")
	}
	scaleOCRTestBudget()
	log := ocrLog()
	dataDir := t.TempDir()
	debugDir := filepath.Join(t.TempDir(), "debug")
	captured := captureStdout(t, func() int {
		code, err := run([]string{"ocr", "--data-dir", dataDir, "--image", ocrControlledImage, "--save-debug", debugDir}, log, nil)
		if err != nil {
			t.Fatalf("ocr: %v", err)
		}
		return code
	})
	if captured.code != 0 {
		t.Fatalf("exit code = %d, want 0 (output:\n%s)", captured.code, captured.output)
	}
	for _, name := range []string{"source.png", "frame-crop.png", "frame-preprocess.png"} {
		path := filepath.Join(debugDir, name)
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("debug image %s missing: %v", name, err)
		}
		if st.Size() == 0 {
			t.Fatalf("debug image %s is empty", name)
		}
		if !pngDecodable(t, path) {
			t.Fatalf("debug image %s is not a decodable PNG", name)
		}
	}
	// The human report must still be the answer on stdout.
	if !strings.Contains(captured.output, "read:") {
		t.Fatalf("human report has no read line:\n%s", captured.output)
	}
}

func pngDecodable(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = png.Decode(f)
	return err == nil
}

// TestRunOCRUnknownGame is the --game error path: an id the store does not
// hold must fail clearly (exit 1, a message that names the id), not guess.
func TestRunOCRUnknownGame(t *testing.T) {
	log := ocrLog()
	dataDir := t.TempDir()
	// Migrate the store so the command opens it as production would.
	ctx := context.Background()
	st, err := openDataStore(ctx, dataDir, log)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	st.Close()

	captured := captureStdout(t, func() int {
		code, err := run([]string{"ocr", "--data-dir", dataDir, "--image", ocrControlledImage, "--game", "gn_no_such_profile"}, log, nil)
		if err == nil {
			t.Fatalf("unknown game returned a nil error (code %d)", code)
		}
		return code
	})
	if captured.code != 1 {
		t.Fatalf("exit code = %d, want 1 (output:\n%s)", captured.code, captured.output)
	}
}

// seedOCRTestProfile seeds the store exactly the way the session runtime's
// tests seed a synthetic published profile (internal/runtime/helpers_test.go):
// the fixture corpus plus one synthetic line that matches the controlled
// image's render, and the fixture's region widened to the whole frame so the
// render is inside the crop. It returns the published profile id and the
// matching line's id.
func seedOCRTestProfile(t *testing.T, st *store.Store) (string, string) {
	t.Helper()
	profile, err := scenario.LoadProfile(ocrProfilePath)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	game := games.Game{ID: games.NewID(), Slug: "ocr-cmd-synthetic", Title: "OCR Command Synthetic",
		Platform: "gamecube", UserTags: []byte(`["synthetic"]`), CreatedAt: now}
	if err := st.CreateGame(ctx, game); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	edition := games.Edition{ID: games.NewID(), GameID: game.ID, Region: "US",
		Language: "en", Revision: "rev0", Locale: "en_US"}
	if err := st.CreateEdition(ctx, edition); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	draft, err := st.CreateDraftProfile(ctx, edition.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}

	byFriendly := map[string]string{}
	msgs := make([]games.Message, 0, len(profile.Messages)+1)
	for _, m := range profile.ToMessages() {
		id := games.NewID()
		byFriendly[m.ID] = id
		row := m
		row.ID, row.ProfileID, row.SourceMessageID = id, draft.ID, m.ID
		row.AttributionStatus = games.AttributionUnknown
		row.Revision = 1
		row.CreatedAt, row.UpdatedAt = now, now
		msgs = append(msgs, row)
	}
	// The synthetic controlled read: a corpus line that matches the
	// controlled asset's render, so the matcher's top-1 is assertable
	// without any game text in the repo.
	matchID := games.NewID()
	msgs = append(msgs, games.Message{
		ID: matchID, ProfileID: draft.ID, OriginalText: "NARRATOR 42",
		NormalizedText: "narrator 42", TextKind: games.TextDialogue,
		AttributionStatus: games.AttributionUnknown,
		Revision:          1, CreatedAt: now, UpdatedAt: now,
	})
	if err := st.PutMessages(ctx, draft.ID, msgs, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}

	regions := make([]games.Region, 0, len(profile.Regions))
	for i, r := range profile.ToRegions() {
		r.ID, r.ProfileID = games.NewID(), draft.ID
		r.DetectMethod = games.DetectManual
		r.Revision = 1
		r.UpdatedAt = now
		if i == 0 {
			// Widen the first region to the whole frame: the controlled
			// asset's render is centred, and the test must not depend on
			// the fixture's textbox geometry.
			r.RectX, r.RectY, r.RectW, r.RectH = 0, 0, 1, 1
			r.Label = "full frame"
		}
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
	if err := st.SetProfileStatus(ctx, draft.ID, games.StatusReview, rev); err != nil {
		t.Fatalf("SetProfileStatus: %v", err)
	}
	_, rev, err = st.GetProfile(ctx, draft.ID) // the status write moved the row version
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	published, err := st.Publish(ctx, draft.ID, rev, "ocr-test@local")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return published.ID, matchID
}

// TestRunOCRGameRanksAgainstThePublishedProfile is the --game path end to
// end: the command loads the published revision, reads the frame through the
// pipeline and ranks the read against the corpus — the controlled read
// must rank the matching corpus line top-1.
func TestRunOCRGameRanksAgainstThePublishedProfile(t *testing.T) {
	if !ocrHasTesseract() {
		t.Skip("NOT RUN: tesseract is not installed on this host (install it and re-run, docs/spec/10)")
	}
	scaleOCRTestBudget()
	log := ocrLog()
	dataDir := t.TempDir()

	ctx := context.Background()
	st, err := openDataStore(ctx, dataDir, log)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	profileID, matchID := seedOCRTestProfile(t, st)
	st.Close()

	captured := captureStdout(t, func() int {
		code, err := run([]string{"ocr", "--data-dir", dataDir, "--image", ocrControlledImage,
			"--game", profileID, "--json", "--top", "5"}, log, nil)
		if err != nil {
			t.Fatalf("ocr: %v", err)
		}
		return code
	})
	if captured.code != 0 {
		t.Fatalf("exit code = %d, want 0 (output:\n%s)", captured.code, captured.output)
	}

	var rep struct {
		Regions []struct {
			Text       string `json:"text"`
			Normalized string `json:"normalized"`
			Rank       *struct {
				Reason     string `json:"reason"`
				Candidates []struct {
					MessageID   string  `json:"message_id"`
					Score       float64 `json:"score"`
					Probability float64 `json:"probability"`
				} `json:"candidates"`
			} `json:"rank"`
		} `json:"regions"`
	}
	if err := json.Unmarshal([]byte(captured.output), &rep); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, captured.output)
	}
	// The widened whole-frame region is the one that reads the render; find
	// the region whose read matches the controlled text.
	found := false
	for _, r := range rep.Regions {
		if cer(strings.Join(strings.Fields(r.Text), " "), "NARRATOR 42") > 0.05 {
			continue
		}
		if r.Rank == nil || len(r.Rank.Candidates) == 0 {
			t.Fatalf("the controlled read's region has no rank: %+v", r)
		}
		if len(r.Rank.Candidates) > 5 {
			t.Fatalf("candidates = %d, want ≤ 5 (--top 5)", len(r.Rank.Candidates))
		}
		if top := r.Rank.Candidates[0]; top.MessageID != matchID {
			t.Fatalf("top candidate = %q, want the corpus line matching the controlled read (%q)", top.MessageID, matchID)
		}
		if top := r.Rank.Candidates[0]; top.Probability <= 0 || top.Probability > 1 {
			t.Fatalf("top probability = %v, want in (0,1]", top.Probability)
		}
		if r.Normalized == "" {
			t.Fatalf("normalized = empty: the matcher fold of the read must be reported")
		}
		found = true
	}
	if !found {
		t.Fatalf("no region read the controlled text:\n%s", captured.output)
	}
}

// cer is the character error rate the doctor and the hardware gate use for
// the controlled asset, so all three bound the same string the same way.
func cer(got, want string) float64 {
	gotR := []rune(got)
	wantR := []rune(want)
	cost := make([][]int, len(gotR)+1)
	for i := range cost {
		cost[i] = make([]int, len(wantR)+1)
		cost[i][0] = i
	}
	for j := range wantR {
		cost[0][j] = j
	}
	for i, g := range gotR {
		for j, w := range wantR {
			c := 1
			if g == w {
				c = 0
			}
			m := cost[i+1][j] + 1
			if v := cost[i][j+1] + 1; v < m {
				m = v
			}
			if v := cost[i][j] + c; v < m {
				m = v
			}
			cost[i+1][j+1] = m
		}
	}
	if len(wantR) == 0 {
		return 0
	}
	return float64(cost[len(gotR)][len(wantR)]) / float64(len(wantR))
}
