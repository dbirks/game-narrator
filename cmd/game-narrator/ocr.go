package main

// The one-shot `game-narrator ocr` command (issue #1): the product's own
// recognition over a single frame from a V4L2 device or a single image file —
// the answer to "what does the device read right now?" without a running
// service, and without skipping the pipeline's crop, presence, preprocess,
// OCR configuration, normalization and matcher (the issue's evidence: the
// manual FFmpeg + tesseract detour proved the hardware, not the pipeline).
//
// It reuses the product's code paths by construction: the capture resolution
// and source are the session runtime's own (stack.Resolve + the ADR-0076
// mode, the same wiring serve builds), and the recognition is
// internal/oneshot — the pipeline's ROI-stage sequence (crop, presence,
// preprocess, recognize, fold, rank) with the session machinery removed.
// Nothing here reimplements any of it.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/oneshot"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

const ocrUsageText = `usage: game-narrator ocr [--game <profile-id>] (--device <node-or-id> | --image <file>) [--json] [--save-debug DIR] [--top N] [--data-dir DIR]

Run the product's own recognition on one frame — the pipeline's crop,
textbox-presence, preprocessing, OCR and matcher (docs/spec/05) — over a
single live V4L2 frame or a single image file, and print what it reads.
This is the "what does the device read right now?" diagnostic: the same
code paths a live session runs (the session's capture resolution and
mode, the pipeline's vision recipe and OCR engine, the matcher's index
and fold), without starting a session.

Exactly one of --device / --image selects the input:
  --device /dev/videoN (or the device's stable id; empty = the capture
     stack's own resolution) grabs one live frame through the capture
     stack — the same format negotiation a v4l2 session uses
     (stack.Resolve + the ADR-0076 policy). It works while serve is
     stopped; if the service (or any other program) holds the device,
     the command fails clearly and says so.
  --image frame.png reads one image file (PNG/JPEG/GIF).

With --game <profile-id> the published profile's regions are read (one
line per region) and the matcher ranks each read against the installed
game's published messages (top-N ids with scores, default --top 10,
capped at the matcher's own bound of 50). Without it, the whole active
picture (letterbox-detected) is read.

--json prints the same report as structured JSON. --save-debug DIR writes
the input image and each region's raw crop and preprocessed input into
DIR for troubleshooting (local only: the files contain game frames and
never go into a support bundle).

exit status: 0 on a report (including a read of nothing), 1 on failure
(no frame, device busy, unknown game, engine failed on every region),
2 on bad usage.
`

// ocrGrabTimeout bounds the first-frame wait on a live device: a healthy
// UVC card delivers a frame in well under a second after FFmpeg opens the
// node, so 15 s is a wedged source or a held device, not a slow one.
const ocrGrabTimeout = 15 * time.Second

// ocrTotalTimeout bounds the whole command: grab (≤ ocrGrabTimeout) plus
// one engine call per region (≤ ocr.DefaultTimeout each) has real slack.
const ocrTotalTimeout = 2 * time.Minute

// deviceBusyHints are the FFmpeg/v4l2 error tails a held device produces.
// The UVC bus is single-consumer (ADR-0078): a running session holds the
// node, and the open fails with EBUSY/EAGAIN rather than delivering.
var deviceBusyHints = []string{
	"device or resource busy",
	"resource temporarily unavailable",
	"cannot allocate resource",
	"failed to open device",
}

// runOCR implements the one-shot recognition command.
func runOCR(args []string, log *slog.Logger) (int, error) {
	fs := flag.NewFlagSet("ocr", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, ocrUsageText) }
	game := fs.String("game", "", "profile id whose published regions and messages are read and ranked")
	device := fs.String("device", "", "V4L2 device node or stable id; empty = the capture stack's own resolution")
	imagePath := fs.String("image", "", "image file (PNG/JPEG/GIF) to read instead of a live frame")
	asJSON := fs.Bool("json", false, "print the report as JSON instead of text")
	saveDebug := fs.String("save-debug", "", "write the input image and each region's crop and preprocessed input into this directory")
	top := fs.Int("top", 10, "matcher candidates to report per region (1..50)")
	dataDir := fs.String("data-dir", "", "durable data directory; overrides "+bootstrap.EnvDataDir)
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "game-narrator ocr: unexpected argument %q\n", fs.Arg(0))
		return 2, fmt.Errorf("%w: unexpected argument", errUsage)
	}
	hasDevice := strings.TrimSpace(*device) != ""
	hasImage := strings.TrimSpace(*imagePath) != ""
	if hasDevice == hasImage {
		fmt.Fprint(os.Stderr, ocrUsageText)
		return 2, fmt.Errorf("%w: exactly one of --device or --image is required", errUsage)
	}
	if *top < 1 || *top > 50 {
		return 2, fmt.Errorf("%w: --top must be in 1..50 (the matcher's bound)", errUsage)
	}

	cfg, err := bootstrap.LoadConfig("", *dataDir, os.LookupEnv)
	if err != nil {
		return 2, err
	}
	// Ctrl-C must not abandon an engine call that is still in flight.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, ocrTotalTimeout)
	defer cancel()

	// Persistent OCR engine (issue #13, ADR-0100): the warm pool is spawned
	// here, before the image is decoded, so its fixed startup (the 0.59 s
	// A53 model load, ADR-0096) overlaps the store open / decode / crop /
	// pre-process work instead of serialising in front of the read. The
	// per-read measurement below then costs the inference, not startup +
	// inference (~1.1 s on the A53 per ADR-0096). The per-process
	// Tesseract the pool embeds is still the fallback for a broken slot.
	pool := ocr.NewPooled(ocr.Tesseract{}, ocr.DefaultPoolSlots)
	defer pool.Close()

	st, err := openDataStore(ctx, cfg.DataDir, log)
	if err != nil {
		return 1, err
	}
	defer closeStore(st, log)

	// The input image: one live frame through the capture stack (the
	// session's own resolution and source, so the format negotiation is
	// the session's), or a file.
	var img image.Image
	var source string
	if hasImage {
		img, err = readImageFile(*imagePath)
		if err != nil {
			return 1, err
		}
		source = "image:" + *imagePath
	} else {
		var stats capture.Stats
		img, source, stats, err = grabOneFrame(ctx, st, *device, log)
		if err != nil {
			return 1, fmt.Errorf("game-narrator ocr: %w", err)
		}
		log.Debug("ocr grab", "source", source, "state", stats.State, "fps", stats.FPS)
	}

	// The debug seam (AGENTS.md: the crop and the intermediate images for
	// troubleshooting — local only, never in a support bundle): the input
	// image is written here, each region's crop and preprocessed input by
	// the oneshot Debug callback below.
	if strings.TrimSpace(*saveDebug) != "" {
		if err := os.MkdirAll(*saveDebug, 0o755); err != nil {
			log.Error("ocr: cannot create the debug directory", "dir", *saveDebug, "error", err)
		} else {
			_ = writeDebugImage(filepath.Join(*saveDebug, "source.png"), img)
		}
	}

	// The game's published content and candidate index, when named: the
	// same load the session runtime applies (only the published revision
	// runs, docs/spec/01 "Key invariants" 2).
	var (
		regions []games.Region
		index   *matcher.Index
	)
	if *game != "" {
		var content store.ProfileContent
		regions, index, content, err = loadGameContent(ctx, st, *game)
		if err != nil {
			return 1, err
		}
		log.Debug("ocr game loaded", "profile", content.Profile.ID, "revision", content.Profile.RevisionNum, "regions", len(regions), "messages", len(content.Messages))
	}

	rep := oneshot.Run(ctx, img, oneshot.Options{
		Source:      source,
		Regions:     regions,
		Index:       index,
		Engine:      pool,
		RankOptions: matcher.RankOptions{TopK: *top, Weights: matcher.DefaultWeights()},
		// Every other field zero: the one-shot path then takes exactly the
		// defaults the live pipeline takes (its normalize mirrors
		// pipeline.Config.normalize), so this read is the session's read.
		Debug: debugSink(*saveDebug),
	})

	if *asJSON {
		if err := writeOCRJSON(stdout, rep); err != nil {
			return 1, fmt.Errorf("writing the ocr report: %w", err)
		}
	} else {
		writeOCRText(stdout, rep)
	}

	// A report is success even when the read is empty (a frame with no box
	// is an answer). Failure is when no region produced a read at all: the
	// engine failed everywhere, or the frame had nothing the regions cover.
	if !anyRegionRead(rep.Regions) {
		return 1, fmt.Errorf("no region produced a read (per-region errors above)")
	}
	return 0, nil
}

// readImageFile decodes one image file. The stdlib decoders (PNG, JPEG, GIF)
// are what a capture frame or a screenshot is; an unknown container is the
// operator's to convert, not this command's to guess at.
func readImageFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening the image: %w", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w (PNG, JPEG and GIF are read in place)", path, err)
	}
	return img, nil
}

// grabOneFrame opens the resolved device's live source — the same
// construction the session runtime uses for a v4l2 frame source
// (stack.Resolve: the requested id, the persisted selection, the ADR-0076
// mode) — and reads one frame. The source is a throwaway: one frame is
// grabbed and the source closed, the way the API's preview snapshot does.
func grabOneFrame(ctx context.Context, st *store.Store, device string, log *slog.Logger) (image.Image, string, capture.Stats, error) {
	// The process's one capture layer, wired exactly as serve wires it:
	// the same enumeration, mode policy and persisted-selection seam.
	capStack := stack.DefaultOptions(os.LookupEnv, captureSelectionStore{st: st}, log)
	res, err := capStack.Resolve(ctx, device)
	if err != nil {
		switch {
		case errors.Is(err, stack.ErrDeviceAbsent):
			return nil, "", capture.Stats{}, fmt.Errorf("no usable capture device is attached: %w (attach the UVC HDMI capture dongle, docs/spec/10, and re-run)", err)
		case errors.Is(err, stack.ErrDeviceUnknown):
			return nil, "", capture.Stats{}, fmt.Errorf("the requested device is not in the current enumeration: %w (the device list is the allowlist; see the capture device API for the ids the session may name)", err)
		case errors.Is(err, stack.ErrModeUnavailable):
			return nil, "", capture.Stats{}, fmt.Errorf("no offered capture mode fits the selection policy: %w", err)
		}
		return nil, "", capture.Stats{}, fmt.Errorf("resolving the capture device: %w", err)
	}
	src, err := capStack.Source(ctx, res.NodePath(), res.Mode)
	if err != nil {
		return nil, "", capture.Stats{}, fmt.Errorf("opening the capture device %s: %w", res.ID(), err)
	}
	defer src.Close()
	frames, err := src.Start(ctx)
	if err != nil {
		stt := src.Stats()
		return nil, "", stt, busyOrDeviceError(stt, err)
	}
	grabCtx, cancel := context.WithTimeout(ctx, ocrGrabTimeout)
	defer cancel()
	select {
	case f, ok := <-frames:
		if !ok {
			stt := src.Stats()
			return nil, "", stt, busyOrDeviceError(stt, fmt.Errorf("the capture source ended before a frame (state %s: %s)", stt.State, orEmptyStr(stt.LastError)))
		}
		if f.Image == nil {
			stt := src.Stats()
			return nil, "", stt, fmt.Errorf("the capture device delivered an empty frame (state %s: %s)", stt.State, orEmptyStr(stt.LastError))
		}
		stt := src.Stats()
		return f.Image, deviceSourceLine(res), stt, nil
	case <-grabCtx.Done():
		stt := src.Stats()
		return nil, "", stt, busyOrDeviceError(stt, fmt.Errorf("no frame within %s (state %s: %s)", ocrGrabTimeout, stt.State, orEmptyStr(stt.LastError)))
	}
}

// deviceSourceLine is the provenance line the report carries for a live
// grab: the device's API identity (the device list's spelling), the
// negotiated mode and the mode's provenance — a fallback is visible, not
// guessed at (the session snapshot's CaptureDevice/CaptureMode contract).
func deviceSourceLine(res stack.Resolved) string {
	return fmt.Sprintf("device:%s mode:%s source:%s", res.ID(), res.Mode.String(), res.ModeSource)
}

// busyOrDeviceError turns a dead or silent capture source into the operator
// message. A held device (a running session owns the UVC bus, ADR-0078, or
// another program has the node open) reads as EBUSY/EAGAIN in FFmpeg's tail;
// that is named as such, with the session API as the alternative. Anything
// else is the source's own fault text, which docs/spec/13 requires be
// actionable.
func busyOrDeviceError(st capture.Stats, err error) error {
	msg := strings.ToLower(err.Error())
	if st.LastError != "" {
		msg += " " + strings.ToLower(st.LastError)
	}
	for _, hint := range deviceBusyHints {
		if strings.Contains(msg, hint) {
			return fmt.Errorf("the capture device is busy: a running game-narrator serve session (or another program, e.g. an open FFmpeg or ffplay) holds the node — the UVC bus is single-consumer. Stop the session, or use the running service's session API instead of this one-shot reader, and retry")
		}
	}
	return fmt.Errorf("the capture device is not delivering frames: %w", err)
}

// loadGameContent loads the named profile's published revision and builds
// the candidate index over its content — the session runtime's exact load
// (runtime.Start): only the published revision runs, a draft edit must not
// change what the text was decided against.
func loadGameContent(ctx context.Context, st *store.Store, gameID string) ([]games.Region, *matcher.Index, store.ProfileContent, error) {
	prof, _, err := st.GetProfile(ctx, gameID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, store.ProfileContent{}, fmt.Errorf("no profile with id %q in this data dir (the id is the profile revision's, as the API and the session report it)", gameID)
		}
		return nil, nil, store.ProfileContent{}, fmt.Errorf("looking up profile %s: %w", gameID, err)
	}
	published, _, err := st.PublishedProfile(ctx, prof.EditionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, store.ProfileContent{}, fmt.Errorf("no revision is published for %s's edition %s: publish a revision (the API or the import flow) before it can be read", gameID, prof.EditionID)
		}
		return nil, nil, store.ProfileContent{}, fmt.Errorf("reading the published revision of %s's edition: %w", gameID, err)
	}
	if published.ID != prof.ID {
		return nil, nil, store.ProfileContent{}, fmt.Errorf("profile %s is revision %d, but the published revision of that edition is %s: only the published revision runs (docs/spec/01)", gameID, prof.RevisionNum, published.ID)
	}
	content, err := st.GetProfileContent(ctx, published.ID)
	if err != nil {
		return nil, nil, store.ProfileContent{}, fmt.Errorf("reading the published content of %s: %w", gameID, err)
	}
	if len(content.Regions) == 0 {
		return nil, nil, store.ProfileContent{}, fmt.Errorf("the published revision of %s has no regions: a profile needs at least one region to read (the region editor or the import flow stores them)", gameID)
	}
	idx, err := matcher.Build(content.Messages, content.Edges, matcher.IndexOptions{})
	if err != nil {
		return nil, nil, store.ProfileContent{}, fmt.Errorf("building the candidate index for %s: %w", gameID, err)
	}
	return content.Regions, idx, content, nil
}

// anyRegionRead reports whether at least one region produced a read — a
// text result (possibly empty) or an error-free observation. A failed
// engine call is not a read.
func anyRegionRead(regions []oneshot.RegionReport) bool {
	for _, r := range regions {
		if r.OCRError == "" && !r.Empty {
			return true
		}
	}
	return false
}

// ── report output ────────────────────────────────────────────────────────────

// The JSON schema the --json flag emits. It is the oneshot report's faithful
// spelling: one entry per region, the raw and normalized text kept separate
// (docs/spec/05), the scores as the matcher reports them.
type ocrJSON struct {
	Command string          `json:"command"`
	Source  string          `json:"source"`
	Width   int             `json:"width"`
	Height  int             `json:"height"`
	Regions []ocrJSONRegion `json:"regions"`
	TotalMS int64           `json:"total_ms"`
}

// ocrJSONRect is a region's stored rect in fractions of the active picture
// (the regions.rect_x … rect_y spelling, docs/spec/02).
type ocrJSONRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// ocrJSONPixelRect is a crop's box in the input image's pixels.
type ocrJSONPixelRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type ocrJSONRegion struct {
	ID         string           `json:"id"`
	Label      string           `json:"label,omitempty"`
	Rect       ocrJSONRect      `json:"rect"`
	Crop       ocrJSONPixelRect `json:"crop"`
	Present    bool             `json:"present"`
	Empty      bool             `json:"empty"`
	Text       string           `json:"text"`
	Normalized string           `json:"normalized,omitempty"`
	MeanConf   float64          `json:"mean_conf"`
	Engine     string           `json:"engine,omitempty"`
	DurationMS int64            `json:"duration_ms"`
	Error      string           `json:"error,omitempty"`
	Rank       *ocrJSONRank     `json:"rank,omitempty"`
}

type ocrJSONRank struct {
	Reason     string        `json:"reason"`
	Abstain    bool          `json:"abstain"`
	Candidates []ocrJSONCand `json:"candidates"`
}

type ocrJSONCand struct {
	MessageID   string  `json:"message_id"`
	Score       float64 `json:"score"`
	Probability float64 `json:"probability"`
}

// writeOCRJSON emits the report as JSON on the command's stdout.
func writeOCRJSON(w io.Writer, rep oneshot.Report) error {
	out := ocrJSON{
		Command: "ocr",
		Source:  rep.Source,
		Width:   rep.Width,
		Height:  rep.Height,
		TotalMS: rep.Total.Milliseconds(),
	}
	for _, r := range rep.Regions {
		jr := ocrJSONRegion{
			ID:         r.ID,
			Label:      r.Label,
			Rect:       ocrJSONRect{X: r.RectX, Y: r.RectY, W: r.RectW, H: r.RectH},
			Crop:       ocrJSONPixelRect{X: r.CropX, Y: r.CropY, W: r.CropW, H: r.CropH},
			Present:    r.Present,
			Empty:      r.Empty,
			Text:       r.Text,
			Normalized: r.Normalized,
			MeanConf:   r.MeanConf,
			Engine:     r.Engine,
			DurationMS: r.Duration.Milliseconds(),
			Error:      r.OCRError,
		}
		if r.Rank != nil {
			jr.Rank = &ocrJSONRank{Reason: r.Rank.Reason, Abstain: r.Rank.Abstain}
			for _, c := range r.Rank.Candidates {
				jr.Rank.Candidates = append(jr.Rank.Candidates, ocrJSONCand{
					MessageID:   c.MessageID,
					Score:       c.Score,
					Probability: c.Probability,
				})
			}
		}
		out.Regions = append(out.Regions, jr)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// writeOCRText emits the human report: one line per region with the read,
// the evidence and, when a game is named, the ranked shortlist.
func writeOCRText(w io.Writer, rep oneshot.Report) {
	fmt.Fprintf(w, "source: %s\n", rep.Source)
	fmt.Fprintf(w, "frame:  %dx%d\n", rep.Width, rep.Height)
	for _, r := range rep.Regions {
		label := r.ID
		if r.Label != "" {
			label = fmt.Sprintf("%s (%s)", r.ID, r.Label)
		}
		fmt.Fprintf(w, "region %s rect=(%.2f,%.2f,%.2f,%.2f) crop=%dx%d@(%d,%d) present=%s empty=%v\n",
			label, r.RectX, r.RectY, r.RectW, r.RectH, r.CropW, r.CropH, r.CropX, r.CropY,
			boolWord(r.Present), r.Empty)
		switch {
		case r.Empty:
			fmt.Fprintln(w, "  read:   (the region covers nothing at this resolution)")
		case r.OCRError != "":
			fmt.Fprintf(w, "  read:   FAILED: %s\n", r.OCRError)
		default:
			fmt.Fprintf(w, "  read:   %q (conf %.2f, engine %s, %s)\n",
				r.Text, r.MeanConf, orDashStr(r.Engine), r.Duration.Round(time.Millisecond))
			if r.Normalized != "" && r.Normalized != r.Text {
				fmt.Fprintf(w, "  folded: %q\n", r.Normalized)
			}
			if r.Rank != nil {
				verdict := "abstain"
				if !r.Rank.Abstain {
					verdict = "local-evidence ok"
				}
				fmt.Fprintf(w, "  rank:   %s (%s)\n", r.Rank.Reason, verdict)
				for i, c := range r.Rank.Candidates {
					fmt.Fprintf(w, "   #%d %s p=%.3f score=%.3f\n", i+1, c.MessageID, c.Probability, c.Score)
				}
			}
		}
	}
	fmt.Fprintf(w, "total:  %s\n", rep.Total.Round(time.Millisecond))
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// orDashStr renders a blank engine name as a dash rather than a dangling
// label.
func orDashStr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// orEmptyStr renders a blank last-error as "(none)", so the state line reads
// as a fact rather than a dangling colon.
func orEmptyStr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// ── --save-debug ─────────────────────────────────────────────────────────────

var debugFileSafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// debugSink builds the oneshot debug callback: it writes each region's raw
// crop and preprocessed input into dir. The directory is created on demand
// (the first region's callback), and a write failure is logged, never
// fatal — the diagnostic report is the answer, the debug images are the
// evidence (and they contain game frames, so they stay local, never in a
// support bundle).
func debugSink(dir string) func(regionID string, raw, preprocessed image.Image) {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	created := false
	return func(regionID string, raw, preprocessed image.Image) {
		if !created {
			created = true
			if err := os.MkdirAll(dir, 0o755); err != nil {
				slog.Error("ocr: creating the debug directory", "dir", dir, "error", err)
				return
			}
		}
		name := debugFileSafe.ReplaceAllString(regionID, "_")
		_ = writeDebugImage(filepath.Join(dir, name+"-crop.png"), raw)
		_ = writeDebugImage(filepath.Join(dir, name+"-preprocess.png"), preprocessed)
	}
}

// writeDebugImage encodes one intermediate image as PNG; the caller decides
// a failure is not worth more than the log line above.
func writeDebugImage(path string, img image.Image) error {
	if img == nil || img.Bounds().Empty() {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
