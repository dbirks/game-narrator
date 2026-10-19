package bench

// captured.go is the captured-Dolphin holdout of docs/spec/12 (docs/spec/16
// M7, AGENTS.md rule-3 class (b) "captured-frame replay"). It runs real
// emulator-captured frames through the same real pipeline the synthetic
// holdout uses and grades them the same way, so the two holdouts share the
// metrics vocabulary and differ only in what the frames are.
//
// Provenance, stated per AGENTS.md "No simulation theater":
//
//   - frames: the external fixture's PNGs (the dolphin fixture factory,
//     ADR-0054), verified against the manifest's per-frame SHA-256 before any
//     replay;
//   - truth: the fixture's oracle — truth.json's per-frame rows (textbox
//     presence, the Tesseract reading at record time, the line label) and the
//     manifest's measured screen labels;
//   - script: imported at run time from the user's game image
//     (GAME_NARRATOR_GAME_IMAGE_PATH) through the windwaker adapter; it is
//     held in memory for the run and never stored or committed (AGENTS.md
//     rule 5). The image's SHA-256 must match the fixture manifest's, so a
//     fixture recorded against a different image is refused, not re-graded.
//
// What is NOT re-recorded here: the OCR of the pipeline run is the pipeline's
// own (ModeReplay, vision.DefaultSteps — the standard recipe; docs/spec/11
// step 4 forbids fixture-specific branches), not the fixture's record-time
// reading (which used a 4x subtitle-crop recipe). The fixture's text is the
// ground truth and the record of what the recording's engine saw.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/windwaker"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/gcdisc"
	"github.com/dbirks/game-narrator/internal/matcher"
)

// capturedRow is one truth.json row of a dolphin fixture: the drive's oracle
// for one frame (the measured screen decides presence, the record-time
// Tesseract reading decides the text, the line labels the narration line).
type capturedRow struct {
	File    string
	TsNS    int64
	Present bool
	Text    string
	Line    string
	Full    bool
}

// CapturedFixture is one verified fixture with its oracle, before segmenting
// into bench samples.
type CapturedFixture struct {
	ID          string
	Dir         string
	Manifest    dolphin.FixtureManifest
	ManifestSHA string // sha256 of manifest.json (the set's identity hash)
	Rows        []capturedRow
	Region      Region // the fixture profile's textbox ROI
	Resolution  string
}

// LoadCapturedFixture loads and verifies one fixture: the manifest's schema
// and every frame's SHA-256 (dolphin.VerifyFixture is the replay gate), then
// truth.json and the fixture profile, cross-checked against the manifest's
// frame list (the oracle and the frames must name the same frames in the
// same order, or the grading would be a lie about its own recording).
func LoadCapturedFixture(dir string) (*CapturedFixture, error) {
	man, err := dolphin.VerifyFixture(dir)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		return nil, fmt.Errorf("bench: read fixture truth: %v", err)
	}
	var rows []struct {
		File    string `json:"file"`
		TsNS    int64  `json:"ts_ns"`
		Present bool   `json:"box_present"`
		Text    string `json:"text"`
		Line    string `json:"line"`
		Full    bool   `json:"text_is_full_line"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("bench: parse %s/truth.json: %v", dir, err)
	}
	if len(rows) != len(man.Frames) {
		return nil, fmt.Errorf("bench: fixture %s: truth.json has %d rows, the manifest names %d frames", man.ID, len(rows), len(man.Frames))
	}
	fx := &CapturedFixture{ID: man.ID, Dir: dir, Manifest: man, Rows: make([]capturedRow, len(rows))}
	for i, r := range rows {
		if r.File != man.Frames[i].File || r.TsNS != man.Frames[i].TsNS {
			return nil, fmt.Errorf("bench: fixture %s: truth row %d does not match manifest frame %s (oracle out of sync with the recording)", man.ID, i+1, man.Frames[i].File)
		}
		fx.Rows[i] = capturedRow{File: r.File, TsNS: r.TsNS, Present: r.Present, Text: r.Text, Line: r.Line, Full: r.Full}
	}
	var prof struct {
		ID      string `json:"id"`
		Origin  string `json:"origin"`
		Regions []struct {
			ID   string  `json:"id"`
			Type string  `json:"type"`
			X    float64 `json:"x"`
			Y    float64 `json:"y"`
			W    float64 `json:"w"`
			H    float64 `json:"h"`
		} `json:"regions"`
		Resolution struct {
			W int `json:"w"`
			H int `json:"h"`
		} `json:"resolution"`
	}
	if profraw, err := os.ReadFile(filepath.Join(dir, "profile.json")); err != nil {
		return nil, fmt.Errorf("bench: read fixture profile: %v", err)
	} else if err := json.Unmarshal(profraw, &prof); err != nil {
		return nil, fmt.Errorf("bench: parse %s/profile.json: %v", dir, err)
	}
	if prof.Origin != "dolphin" {
		return nil, fmt.Errorf("bench: fixture %s: profile origin must be dolphin (got %q)", man.ID, prof.Origin)
	}
	if len(prof.Regions) == 0 {
		return nil, fmt.Errorf("bench: fixture %s: profile names no region", man.ID)
	}
	r := prof.Regions[0]
	fx.Region = Region{ID: r.ID, Type: r.Type, X: r.X, Y: r.Y, W: r.W, H: r.H}
	fx.Resolution = fmt.Sprintf("%dx%d", prof.Resolution.W, prof.Resolution.H)
	sha, err := dolphin.ManifestSHA256(dir)
	if err != nil {
		return nil, fmt.Errorf("bench: fixture %s: %v", man.ID, err)
	}
	fx.ManifestSHA = sha
	return fx, nil
}

// isLineRow reports whether the row shows a full narration line: the oracle's
// full-line flag (the record-time engine read the whole line, not a
// typewriter prefix).
func (r capturedRow) isLineRow() bool { return r.Full && r.Text != "" }

// capturedSpan is one oracle span: the frame range (in the fixture's truth
// rows) of one sample — a leading empty run or one narration-line occurrence
// with its trailing frames. line is the oracle's label of the narration line
// (empty for the leading empty span); it selects the sample's display window
// (the rows the oracle says show the line, ADR-0058 amendment 3).
type capturedSpan struct {
	kind  string
	text  string
	line  string
	start int
	end   int // exclusive
}

// Segments cuts the capture into bench samples the way the synthetic set is
// structured: one leading "empty" sample for the frames before the first
// narration line (the precision fence — the pipeline must commit nothing on
// cutscene/title/prompt frames), then one "dialogue" sample per narration
// line occurrence. An occurrence is a consecutive run of the same line
// (same label and text): its first text frame up to the frame just before
// the next occurrence's first text frame — the trailing frames play the
// synthetic set's advance frame and exercise the duplicate/stale-speech
// paths. The box being dismissed and the same line reappearing is a new
// occurrence (a new appearance is a new narration, as in the synthetic set
// where each sample is one standalone run).
// resolve maps a line's truth text to the imported script's message id
// ("" when the line is not in the script: a retrieval miss the metrics
// grade honestly).
func (f *CapturedFixture) Segments(resolve func(text string) string) []Sample {
	var (
		lineStarts  []int
		prevWasLine bool
		prevLine    string
		prevText    string
	)
	for i, r := range f.Rows {
		if !r.isLineRow() {
			prevWasLine = false
			continue
		}
		if !prevWasLine || r.Line != prevLine || r.Text != prevText {
			lineStarts = append(lineStarts, i)
			prevLine, prevText = r.Line, r.Text
		}
		prevWasLine = true
	}
	var spans []capturedSpan
	if len(lineStarts) == 0 {
		spans = append(spans, capturedSpan{kind: "empty", start: 0, end: len(f.Rows)})
		return f.buildSamples(spans, resolve)
	}
	if lineStarts[0] > 0 {
		spans = append(spans, capturedSpan{kind: "empty", start: 0, end: lineStarts[0]})
	}
	for j, s := range lineStarts {
		end := len(f.Rows)
		if j+1 < len(lineStarts) {
			end = lineStarts[j+1]
		}
		spans = append(spans, capturedSpan{kind: "dialogue", text: f.Rows[s].Text, line: f.Rows[s].Line, start: s, end: end})
	}
	return f.buildSamples(spans, resolve)
}

// buildSamples turns the oracle's spans into bench samples.
func (f *CapturedFixture) buildSamples(spans []capturedSpan, resolve func(text string) string) []Sample {
	var out []Sample
	n := 0
	for _, sp := range spans {
		if sp.end <= sp.start {
			continue
		}
		n++
		frames := make([]Frame, 0, sp.end-sp.start)
		// The display window (ADR-0058 amendment 3): the frames the oracle says
		// the line is on screen — a present row whose line label is the span's
		// line. Within the span's range the segmenting rule guarantees every
		// line row is the span's line, so this is the line's frames exactly:
		// the trailing frames (the next occurrence's lead-in) carry a
		// different label or no box and fall outside the window.
		dw := make([]bool, sp.end-sp.start)
		for i, r := range f.Rows[sp.start:sp.end] {
			frames = append(frames, Frame{File: r.File, TsNS: r.TsNS, Absent: !r.Present})
			if sp.kind == "dialogue" {
				dw[i] = r.Present && r.Line == sp.line
			}
		}
		s := Sample{
			ID:             fmt.Sprintf("cx%04d", n),
			Frames:         frames,
			SourceText:     sp.text,
			Speaker:        SpeakerUnknown,
			TextKind:       sp.kind,
			Scene:          f.ID,
			IntendedSpeech: sp.kind == "dialogue",
			Split:          SplitHoldout, // a captured frame is graded, never tuned on
			Variant:        Variant{Resolution: f.Resolution},
			DisplayWindow:  dw,
		}
		if id := resolve(sp.text); id != "" {
			id := id
			s.MessageID = &id
		}
		out = append(out, s)
	}
	return out
}

// ImportScript imports the game's canonical messages at run time through the
// windwaker adapter (the user's image, opened read-only via gcdisc). Nothing
// is written: the messages live for the run and the report, and the image
// path is never recorded in any output (AGENTS.md rule 5). The adapter's own
// game-id check refuses a mislabelled image before any parse.
func ImportScript(ctx context.Context, imagePath string) ([]games.Message, error) {
	d, err := gcdisc.Open(imagePath)
	if err != nil {
		return nil, fmt.Errorf("bench: open game image: %v", err)
	}
	defer d.Close()
	res, err := windwaker.New().(windwaker.Adapter).ParseDisc(ctx, d, adapter.ParseOptions{
		ProfileID: games.NewID(),
		Language:  "en",
	})
	if err != nil {
		return nil, fmt.Errorf("bench: import script: %v", err)
	}
	return res.Messages, nil
}

// ImageSHA256 is the fixture-replay gate's image identity check: the fixture
// manifest records the SHA-256 of the image it was recorded against, and a
// bench run on a different image would grade the wrong recording.
func ImageSHA256(imagePath string) (string, error) {
	f, err := os.Open(imagePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// scriptKeyOf is the retrieval key one imported message lands on in the
// bench's matcher index: the adapter's normalized text (default textnorm
// options) folded exactly as matcher.Build folds it (the bench's matchOpts).
// Re-deriving it here — instead of trusting the stored column — keeps the
// truth-to-script mapping on the same key the index will rank with.
func scriptKeyOf(m games.Message) string {
	norm := textnorm.Normalize(m.OriginalText, textnorm.Options{})
	return textnorm.Normalize(norm, matchOpts)
}

// truthKeyOf is the same fold applied to the fixture's record-time reading,
// so truth text and script keys are compared in one space.
func truthKeyOf(text string) string {
	return textnorm.Normalize(text, matchOpts)
}

// resolveTruth maps a narration line's truth text to the imported script's
// message id: an exact folded-key match first, then the nearest key within a
// bounded edit distance (the record-time Tesseract reading may differ from
// the BMG text by a glyph or two). No match returns "" — the line is then
// graded as not in the script, never guessed onto the closest one.
func resolveTruth(msgs []games.Message) func(text string) string {
	exact := make(map[string]string, len(msgs))
	keys := make([]string, len(msgs))
	ids := make([]string, len(msgs))
	for _, m := range msgs {
		k := scriptKeyOf(m)
		if k == "" {
			continue
		}
		keys = append(keys, k)
		ids = append(ids, m.ID)
		if _, ok := exact[k]; !ok {
			exact[k] = m.ID
		}
	}
	return func(text string) string {
		k := truthKeyOf(text)
		if id, ok := exact[k]; ok {
			return id
		}
		// Nearest key within the tolerance: the reading and the script are
		// two renderings of the same line, so a small edit distance is the
		// expected gap. The bound keeps a 5%-off reading attached and a
		// different line off.
		tol := len(k) / 10
		if tol < 3 {
			tol = 3
		}
		best, bestD := "", -1
		for i, kk := range keys {
			if len(kk) < len(k)-tol || len(kk) > len(k)+tol {
				continue
			}
			d := levenshtein(k, kk)
			if d <= tol && (bestD < 0 || d < bestD) {
				best, bestD = ids[i], d
			}
		}
		return best
	}
}

// levenshtein is the plain edit distance on two already-folded keys.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			m := prev[j] + 1
			if v := cur[j-1] + 1; v < m {
				m = v
			}
			if v := prev[j-1] + cost; v < m {
				m = v
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

// BuildCapturedIndex compiles the imported script into the matcher index the
// pipeline and the sidecar share. Speakers: the BMG carries no speaker
// labels (the adapter reports none), so every line is unattributed — the
// named-speaker metric is n/a on this holdout by construction, which is the
// honest reading (unknown cases don't receive a forced speaker).
func BuildCapturedIndex(msgs []games.Message) (*matcher.Index, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("bench: the imported script has no messages")
	}
	return matcher.Build(msgs, nil, matcher.IndexOptions{})
}

// CapturedProfile is the bench script the captured run's sidecar answers
// from: the imported lines, unattributed. It is built in memory, never
// written (AGENTS.md rule 5).
func CapturedProfile(fx *CapturedFixture, msgs []games.Message) *ScriptProfile {
	p := &ScriptProfile{
		SchemaVersion: SchemaVersion,
		ID:            "captured-" + fx.ID,
		Origin:        "dolphin",
		Description:   "Captured-Dolphin holdout script: the imported game messages (in-memory, never stored).",
		Scene:         fx.ID,
		Regions:       []Region{fx.Region},
	}
	for _, m := range msgs {
		p.Messages = append(p.Messages, ScriptLine{ID: m.ID, Text: m.OriginalText, Speaker: SpeakerUnknown})
	}
	return p
}

// RunCapturedBench runs the captured-Dolphin holdout end-to-end: verify the
// fixture, import the script from the user's image (refusing an image whose
// SHA-256 does not match the fixture's), replay every segment through the
// real pipeline (ModeReplay + Tesseract, the fixture's own frames and
// capture-clock timestamps), compute the docs/spec/12 metrics, and write
// report.json / report.md / manifest.json into outDir. It is class (b)
// captured-frame replay: real frames, real engine, real matcher, external
// truth — and it is always the holdout split (a captured frame is graded,
// never tuned on).
func RunCapturedBench(ctx context.Context, fixtureDir, imagePath, outDir string) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fx, err := LoadCapturedFixture(fixtureDir)
	if err != nil {
		return nil, err
	}
	if imagePath == "" {
		return nil, fmt.Errorf("NOT RUN: bench captured holdout needs the game image (set GAME_NARRATOR_GAME_IMAGE_PATH to the user's Wind Waker image, or pass --image)")
	}
	sha, err := ImageSHA256(imagePath)
	if err != nil {
		return nil, fmt.Errorf("NOT RUN: bench captured holdout cannot read the game image %s: %v", filepath.Base(imagePath), err)
	}
	if fx.Manifest.GameImageSHA256 != "" && sha != fx.Manifest.GameImageSHA256 {
		return nil, fmt.Errorf("NOT RUN: bench captured holdout: the image's SHA-256 does not match fixture %s's manifest (the fixture was recorded against a different image; re-record it or point GAME_NARRATOR_GAME_IMAGE_PATH at the recorded image)", fx.ID)
	}

	msgs, err := ImportScript(ctx, imagePath)
	if err != nil {
		return nil, err
	}
	idx, err := BuildCapturedIndex(msgs)
	if err != nil {
		return nil, fmt.Errorf("bench: captured matcher index: %v", err)
	}
	prof := CapturedProfile(fx, msgs)
	benchProfile = prof
	regions := prof.BuildRegions()

	resolve := resolveTruth(msgs)
	samples := fx.Segments(resolve)
	if len(samples) == 0 {
		return nil, fmt.Errorf("bench: fixture %s segments into no samples", fx.ID)
	}

	framesDir := filepath.Join(fx.Dir, "frames")
	work, err := os.MkdirTemp("", "bench-captured-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	pinReplayEngineThreads()
	start := time.Now()
	cpuBefore := procCPUTime()
	// Bounded parallelism (ADR-0064): each sample's hot path is one Tesseract
	// call at a time (ModeReplay is sequential per region) plus light Go work,
	// so a handful of concurrent samples uses the box without oversubscribing
	// it. The worker count is a scheduler detail: per-frame OCR is
	// deterministic, so results do not depend on it (the M2 three-identical-
	// runs property holds).
	workers := replayWorkers(len(samples))
	jobs := make(chan int)
	var (
		mu       sync.Mutex
		notRun   []NotRunSample
		outcomes = make([]sampleOutcomes, len(samples))
		wg       sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				s := samples[i]
				// One per-segment timestamps manifest: the replay source
				// reads frames from the fixture's frames/ dir (read-only —
				// the fixture is never rewritten) and paces on the
				// recording's own capture clock.
				tsPath := filepath.Join(work, fmt.Sprintf("ts-%s.json", s.ID))
				if err := writeManifest(tsPath, s.Frames); err != nil {
					fmt.Fprintf(os.Stderr, "bench: captured %s NOT RUN: %v\n", s.ID, err)
					mu.Lock()
					notRun = append(notRun, NotRunSample{SampleID: s.ID, Reason: err.Error()})
					mu.Unlock()
					continue
				}
				out, err := runSample(ctx, framesDir, tsPath, s, idx, regions)
				if err != nil {
					fmt.Fprintf(os.Stderr, "bench: captured %s NOT RUN: %v\n", s.ID, err)
					mu.Lock()
					notRun = append(notRun, NotRunSample{SampleID: s.ID, Reason: err.Error()})
					mu.Unlock()
					continue
				}
				mu.Lock()
				outcomes[i] = out
				mu.Unlock()
			}
		}()
	}
	for i := range samples {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	var ok []sampleOutcomes
	for _, o := range outcomes {
		if o.sample.id != "" {
			ok = append(ok, o)
		}
	}
	if len(ok) == 0 {
		return nil, fmt.Errorf("bench: no captured sample completed (tesseract required; see the NOT RUN lines above)")
	}

	// A dialogue sample whose line did not fold-match the imported script is
	// graded as not in the script (any commit on it is a false positive); the
	// manifest records how many lines that was.
	missed := 0
	for _, o := range ok {
		if o.sample.textKind == "dialogue" && o.sample.msgID == "" {
			missed++
		}
	}

	rep := &Report{
		SchemaVersion: SchemaVersion,
		RunID:         time.Now().UTC().Format("20060102T150405Z"),
		Set:           fx.ID,
		Split:         SplitHoldout,
		Origin:        "dolphin",
		Class:         ClassCaptured,
		Samples:       len(ok),
		Metrics:       RunMetrics(ok),
		Diagnostics: Diagnostics{
			GoVersion:          runtime.Version(),
			Tesseract:          tesseractVersion(),
			OS:                 runtime.GOOS,
			SetHash:            fx.ManifestSHA,
			SampleIDs:          sampleIDs(ok),
			CPUTimeSecs:        procCPUTime() - cpuBefore,
			PeakRSSMB:          procPeakRSSMB(),
			DurationSecs:       time.Since(start).Seconds(),
			MatcherCalibration: matcherCalibrationDigest(),
			NotRun:             notRun,
			Top1Misses:         top1Misses(ok),
		},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(outDir, "report.json"), rep); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.md"), []byte(RenderMarkdown(rep, ok)), 0o644); err != nil {
		return nil, err
	}
	man := manifestOf(rep)
	man["fixture_manifest_sha"] = fx.ManifestSHA
	man["game_image_sha256"] = sha
	man["script_messages"] = len(msgs)
	man["truth_lines_not_in_script"] = missed
	man["script_source"] = "imported at run time via the windwaker adapter (never stored; AGENTS.md rule 5)"
	if err := writeJSON(filepath.Join(outDir, "manifest.json"), man); err != nil {
		return nil, err
	}
	return rep, nil
}

// CapturedFrameCount verifies a fixture and returns its frame count, so the
// run's caller can size the run-level wall bound (ReplayRunBound) before the
// replay starts. It runs the full VerifyFixture gate (every frame's SHA-256),
// so a fixture that cannot be verified is refused here, before any sample
// runs — the same refusal the run itself would give.
func CapturedFrameCount(fixtureDir string) (int, error) {
	fx, err := LoadCapturedFixture(fixtureDir)
	if err != nil {
		return 0, err
	}
	return len(fx.Manifest.Frames), nil
}

// replayRunFloor is the run-level wall bound's floor: it covers a fixture of
// any size on a normal host (the dense fixture, 1085 frames, runs in under
// five minutes wall on the 8-core bench host) and keeps the bound honest for
// the small fixtures (newgame: 25 frames) where the per-frame term alone
// would be seconds.
const replayRunFloor = 30 * time.Minute

// replayRunPerFrame is the per-frame term of the run-level wall bound
// (ADR-0064). It is the harness's patience bound, not a load-timing
// mechanism: a captured replay's verdict must not depend on host load, so the
// bound must be far above any realistic per-frame cost while still finite.
// Measured per-frame wall cost on the dense fixture is ~0.14s (the one
// 680-frame sample is ~0.14s/frame of Tesseract; the rest are
// decode/analyze-only). The pipeline's own per-OCR-call budget is
// ReplayOCRMaxAttempts x DefaultReplayOCRTimeout = 90s; 10s/frame is 1/9 of
// that worst case and ~70x the measured cost, so no realistic load drives a
// healthy run into it and a genuinely wedged run is still bounded.
const replayRunPerFrame = 10 * time.Second

// ReplayRunBound is the run-level wall bound one captured-replay phase
// (bench captured, or one calibrate source) gets. The per-sample deadlines
// are gone (ADR-0064: a load-sensitive deadline turns a slow sample into a
// NOT RUN and changes the verdict), so this is the harness's only patience
// bound: a 30-minute floor plus 10s per fixture frame. The caller creates a
// context with this deadline and passes it to the run.
func ReplayRunBound(totalFrames int) time.Duration {
	if totalFrames <= 0 {
		return replayRunFloor
	}
	if b := time.Duration(totalFrames) * replayRunPerFrame; b > replayRunFloor {
		return b
	}
	return replayRunFloor
}
