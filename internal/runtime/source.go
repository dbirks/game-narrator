package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/ffmpegpipe"
	"github.com/dbirks/game-narrator/internal/capture/replay"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	"github.com/dbirks/game-narrator/internal/games"
)

// Frame source kinds (docs/spec/08 "Example session workflow": the session
// names its frame source). v4l2 (the UVC HDMI capture dongle) and dolphin are
// the live hardware paths: v4l2 is wired (ADR-0078) — the device is resolved
// against the live enumeration by stable id and the mode comes from the
// operator's pin or the ADR-0076 policy; dolphin stays the dev-only bridge
// (ADR-0007), answered 501 rather than faked when its gate is not met.
const (
	SourceKindReplay      = "replay"
	SourceKindTestPattern = "test_pattern"
	SourceKindV4L2        = "v4l2"
	SourceKindDolphin     = "dolphin"
	// SourceKindManual is the explicitly-driven frame source: the caller
	// pushes frames at its own pace (the runtime's Options.ManualSource
	// seam). It is a test seam for docs/spec/12's "deterministic
	// delay-injection suite" — a session test that must assert an ordering
	// between a frame and the events it causes pushes the frame, waits on
	// the specific events, then pushes the next one, so the assertion holds
	// on the event stream rather than on a wall-clock margin. Production
	// never wires the closure, so the kind answers ErrSourceUnsupported.
	SourceKindManual = "manual"
	EnvReplayRoots   = "GAME_NARRATOR_REPLAY_ROOTS"
	// EnvDeveloperMode is the process-level developer mode (ADR-0119,
	// issue #28): the family of GAME_NARRATOR_DEV_DOLPHIN and the ADR-0074
	// fake flags — a dev-host opt-in read at the layer that acts on it, unset
	// on the appliance. The frame-source default uses it as its fallback
	// gate: with no usable capture device a developer host still gets the
	// synthetic test pattern, while the appliance gets the 409 that names the
	// operator action (connect the card), never a silent synthetic session.
	EnvDeveloperMode = "GAME_NARRATOR_DEV"
	// EnvDevDolphin mirrors internal/dolphin.EnvDevDolphin: the runtime admits
	// the dolphin frame source only on an explicit developer opt-in. The name
	// is duplicated rather than imported so the core package keeps no
	// dependency on the dev-only hardware package (the closure below is the
	// only seam between them); a runtime test asserts the two stay equal.
	EnvDevDolphin = "GAME_NARRATOR_DEV_DOLPHIN"
)

// The pacing of a replayed session comes from Options.ReplaySpeed. Speed 0
// (the default) drains the recording as fast as the pipeline consumes it:
// the tracker decides on the recording's own source clock, so the decision is
// a pure function of the frames and the engine — docs/spec/12's "a replay
// must not depend on machine speed". Pacing at real time (1.0) would
// interleave wall-clock OCR/ranking latency with the source clock and make
// the outcome a function of machine speed, which is exactly the failure the
// Frame-replay gate exists to forbid — so the default stays 0, and only a
// caller that deliberately wants real-time pacing (the live session test,
// whose evidence is the audio half, not the frame-replay gate) sets 1.0.

// DefaultReplayRoots answers the allowlist default: an explicit
// GAME_NARRATOR_REPLAY_ROOTS (comma-separated, absolute) wins; otherwise the
// repo-relative testdata/ is admitted when the process runs out of a
// directory that has one (a dev context), and nothing in production, where no
// such directory exists. An explicitly set empty value means "no replay roots
// at all".
func DefaultReplayRoots(lookupEnv func(string) (string, bool)) []string {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if raw, ok := lookupEnv(EnvReplayRoots); ok {
		var out []string
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	candidate := filepath.Join(cwd, "testdata")
	if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
		return []string{candidate}
	}
	return nil
}

// resolveReplayDir admits a replay directory only when it is at or below one
// of the configured roots after full symlink resolution. The API never accepts
// an arbitrary filesystem path: a "replay" that pointed at /etc would be the
// box reading files it was never meant to read (docs/spec/14 "Filesystem").
//
// Both sides are resolved with EvalSymlinks before the prefix test, so a
// symlink inside an allowed root that points outside it fails the test, and
// relative paths are anchored to the process's working directory — the same
// directory a developer runs the binary from.
func resolveReplayDir(dir string, roots []string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("runtime: a replay frame source requires a dir")
	}
	if len(roots) == 0 {
		return "", ErrSourceNotAllowed
	}
	target, err := absResolved(dir)
	if err != nil {
		return "", ErrSourceMissing
	}
	fi, err := os.Stat(target)
	if err != nil || !fi.IsDir() {
		return "", ErrSourceMissing
	}
	for _, root := range roots {
		resolvedRoot, err := absResolved(root)
		if err != nil {
			continue // a root that cannot be resolved admits nothing
		}
		if isUnder(resolvedRoot, target) {
			return target, nil
		}
	}
	return "", ErrSourceNotAllowed
}

// absResolved absolutises p and resolves every symlink it can. A fully
// existing path resolves completely. A path whose tail is missing (a replay
// clip not created yet, a typo) resolves the longest existing prefix and
// re-attaches the missing tail: the tail is not followed because it does not
// exist, which is also exactly where a sneaky symlink could not hide yet.
// A tail that does exist but is itself a symlink resolves through it, so a
// symlink that points outside the allowlist fails the prefix test.
func absResolved(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	dir := filepath.Dir(abs)
	var walkErr error
	for {
		resolvedDir, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(resolvedDir, filepath.Base(abs)), nil
		}
		walkErr = err
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", walkErr
		}
		dir = parent
	}
}

// isUnder reports whether p is root or below it, lexically, on resolved paths.
func isUnder(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// builtSource is buildFrameSource's answer: the source the session runs,
// and, for kind v4l2, the capture device and mode the resolution settled on
// (ADR-0078) — the session snapshot and the session.started event carry both,
// so a fallback to the persisted selection or the first usable device is
// visible in the run's own evidence, not guessed from the request.
type builtSource struct {
	src capture.Source
	// kind is the kind the source runs under when buildFrameSource resolved
	// one (ADR-0119, issue #28): a request that named no frame source gets
	// the resolved kind here — v4l2 when a usable capture device won the
	// default, test_pattern when developer mode took the fallback — so the
	// session snapshot, the session.started event and the status bar name the
	// source that actually runs, not the absence the request carried.
	// Empty for every request that named its own kind.
	kind string
	// device is the v4l2 device's stable id actually used; "" for every
	// other kind.
	device string
	// mode is the v4l2 mode asked of the device (mode.Mode.String(), e.g.
	// "640x480 MJPEG @ 10 fps"); "" for every other kind.
	mode string
	// modeFPS is the structured rate of that mode (the target half of the
	// status bar's capture line); 0 when the source kind has no named rate.
	modeFPS float64
	// modeSource names the v4l2 mode's provenance (the operator's pin or the
	// policy); "" for every other kind.
	modeSource string
}

// buildFrameSource constructs the capture source for a session. replay is the
// fixture-replay source behind docs/spec/12's "Frame replay" gate;
// test_pattern is FFmpeg's testsrc2 through the same supervised subprocess
// the live capture uses (ADR-0012 part 1), so a dev host without a capture
// card still gets real frames through the production consumer.
//
// v4l2 is the UVC HDMI capture dongle (ADR-0078): the device is resolved by
// stable id against the live enumeration (the enumeration is the allowlist —
// a client-named path is refused) and the source is the ffmpegpipe V4L2
// constructor at the resolved mode. The resolution and the construction are
// the CaptureOptions wiring's closures (nil wiring answers
// ErrCaptureUnavailable: an unwired process refuses rather than fakes a
// device).
//
// dolphin is the dev-only live emulator source (docs/spec/11, ADR-0007): it
// is admitted only when the operator opted in via EnvDevDolphin AND a bridge
// closure was wired into the manager. Either half missing answers 501
// (ErrSourceUnsupported) — the runtime never fakes the emulator and never
// launches one itself; the closure is the one place the dev bridge hands a
// ready frame source to the pipeline.
//
// manual is the test seam (Options.ManualSource): a nil closure means the
// process has no manual source to hand out, so the kind is refused rather
// than faked — the same no-simulation-theater rule the dolphin gate applies
// to the emulator.
func buildFrameSource(ctx context.Context, kind, dir string, roots []string, speed float64, devDolphin func() (capture.Source, error), manual func() (capture.Source, error), capOpts *CaptureOptions, lookupEnv func(string) (string, bool)) (builtSource, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	switch kind {
	case SourceKindReplay:
		dir, err := resolveReplayDir(dir, roots)
		if err != nil {
			return builtSource{}, err
		}
		return builtSource{src: &replay.Source{Dir: dir, Speed: speed}}, nil
	case SourceKindTestPattern:
		// 640x480 at the clip's 10 Hz cadence: the tracker's documented knobs
		// are sized to a 10 Hz recording, and the pattern needs no device.
		return builtSource{src: ffmpegpipe.TestSource(640, 480, 10), modeFPS: 10}, nil
	case SourceKindDolphin:
		if v, _ := lookupEnv(EnvDevDolphin); v != "1" && v != "true" {
			return builtSource{}, fmt.Errorf("runtime: frame source kind %q is not enabled in this build (set %s=1 on a dev host with the bridge wired): %w", kind, EnvDevDolphin, ErrSourceUnsupported)
		}
		if devDolphin == nil {
			return builtSource{}, fmt.Errorf("runtime: frame source kind %q is enabled (%s=1) but no Dolphin bridge closure is wired into this process: %w", kind, EnvDevDolphin, ErrSourceUnsupported)
		}
		src, err := devDolphin()
		if err != nil {
			return builtSource{}, err
		}
		return builtSource{src: src}, nil
	case SourceKindManual:
		if manual == nil {
			return builtSource{}, fmt.Errorf("runtime: frame source kind %q is a test seam: no manual source closure is wired into this process: %w", kind, ErrSourceUnsupported)
		}
		src, err := manual()
		if err != nil {
			return builtSource{}, err
		}
		return builtSource{src: src}, nil
	case SourceKindV4L2:
		bs, absent, err := buildV4L2Source(ctx, dir, capOpts)
		if err != nil {
			return builtSource{}, err
		}
		if absent {
			// No dongle attached: the documented 503 answer — the physical
			// connection is the operator's action (docs/spec/08's
			// DEVICE_UNAVAILABLE class).
			return builtSource{}, fmt.Errorf("runtime: %w: no usable capture device is attached: connect the UVC HDMI capture dongle and retry", ErrCaptureDeviceAbsent)
		}
		return bs, nil
	case "":
		// The default frame source (ADR-0119, issue #28): a request that
		// named no frame_source runs the usable capture device — the ADR-0078
		// chain through the process's capture wiring (the persisted selection,
		// then the first usable device) — and the synthetic test pattern is a
		// developer-mode fallback, never the appliance default. The resolved
		// kind rides back in the answer so the session snapshot, the
		// session.started event and the status bar name the source that
		// actually runs, not the absence the request carried.
		if capOpts != nil && capOpts.Resolve != nil && capOpts.Source != nil {
			bs, absent, err := buildV4L2Source(ctx, "", capOpts)
			switch {
			case err != nil:
				// A device that is present but offers no policy-fitting mode
				// keeps its 503 through the default path too: a broken card
				// is never hidden behind a silent synthetic session.
				return builtSource{}, err
			case !absent:
				bs.kind = SourceKindV4L2
				return bs, nil
			}
		}
		// No usable device (or no capture wiring): the developer-mode
		// fallback, or the 409 that names the operator's action.
		if v, ok := lookupEnv(EnvDeveloperMode); ok && (v == "1" || v == "true") {
			return builtSource{kind: SourceKindTestPattern, src: ffmpegpipe.TestSource(640, 480, 10), modeFPS: 10}, nil
		}
		return builtSource{}, fmt.Errorf("runtime: the request named no frame source and no usable capture device is attached: %w — connect your capture card and start again, or name the frame source explicitly (a v4l2 device, a replay dir or clip, or, in developer mode, the test pattern)", ErrNoFrameSource)
	default:
		return builtSource{}, fmt.Errorf("runtime: unknown frame source kind %q: %w", kind, ErrUnknownSourceKind)
	}
}

// buildV4L2Source is the v4l2 (UVC HDMI capture dongle) half of
// buildFrameSource (ADR-0078): it resolves the device and mode (the stable-id
// allowlist, the persisted selection or the operator's pinned mode, the
// ADR-0076 policy) and builds the live source at the resolved mode —
// ffmpegpipe.V4L2 with the "v4l2:<node>" ID the no-signal wrapper keys on in
// production. deviceAbsent reports the no-usable-device refusal
// (stack.ErrDeviceAbsent) separately so the two callers answer it their own
// way: the explicit kind v4l2 with the 503 the operator knows, the default
// resolution with its developer-mode fallback and the 409 (ADR-0119).
func buildV4L2Source(ctx context.Context, requestedID string, capOpts *CaptureOptions) (builtSource, bool, error) {
	if capOpts == nil || capOpts.Resolve == nil || capOpts.Source == nil {
		return builtSource{}, false, fmt.Errorf("runtime: frame source kind %q is not wired in this process: %w", SourceKindV4L2, ErrCaptureUnavailable)
	}
	res, err := capOpts.Resolve(ctx, requestedID)
	if err != nil {
		switch {
		case errors.Is(err, stack.ErrDeviceAbsent):
			return builtSource{}, true, nil
		case errors.Is(err, stack.ErrDeviceUnknown):
			return builtSource{}, false, fmt.Errorf("runtime: %w: %v", ErrCaptureDeviceUnknown, err)
		case errors.Is(err, stack.ErrModeUnavailable):
			return builtSource{}, false, fmt.Errorf("runtime: %w: %v", ErrCaptureModeUnavailable, err)
		}
		return builtSource{}, false, fmt.Errorf("runtime: resolving the capture device: %w", err)
	}
	src, err := capOpts.Source(ctx, res)
	if err != nil {
		return builtSource{}, false, fmt.Errorf("runtime: opening the capture device %s: %w", res.ID(), err)
	}
	return builtSource{src: src, device: res.ID(), mode: res.Mode.String(), modeFPS: res.Mode.FPS, modeSource: res.ModeSource}, false, nil
}

// observedSource wraps a capture source with the developer-trace observer.
// Every frame the pipeline will see passes through the recorder before it is
// forwarded, so the evidence and the recognition describe the same pixels;
// the wrapper forwards untouched when tracing is off, which is the
// "no images stored or referenced without dev mode" rule made structural.
type observedSource struct {
	inner   capture.Source
	rec     *devTraceRecorder
	regions []games.Region
	log     *slog.Logger

	out  chan capture.Frame
	done chan struct{}
}

func (o *observedSource) ID() string { return o.inner.ID() }

func (o *observedSource) Stats() capture.Stats { return o.inner.Stats() }

func (o *observedSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	frames, err := o.inner.Start(ctx)
	if err != nil {
		return nil, err
	}
	o.out = make(chan capture.Frame, 2)
	o.done = make(chan struct{})
	go o.pump(ctx, frames)
	return o.out, nil
}

func (o *observedSource) Close() error {
	err := o.inner.Close()
	<-o.done
	return err
}

// pump samples frames for the dev trace and forwards them. A sample failure is
// logged and skipped, never fatal: the debug feature must not stall capture
// (docs/spec/01 "slow browsers cannot stall capture" applies to observers too).
func (o *observedSource) pump(ctx context.Context, frames <-chan capture.Frame) {
	defer close(o.out)
	defer close(o.done)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return
			}
			if o.rec != nil {
				if _, err := o.rec.recordFrame(ctx, f, o.regions); err != nil {
					o.log.Warn("dev trace sample not stored; continuing untraced for this frame",
						"frame_seq", f.Seq, "error", err)
				}
			}
			select {
			case o.out <- f:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
