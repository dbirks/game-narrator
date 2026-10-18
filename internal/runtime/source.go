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
	"github.com/dbirks/game-narrator/internal/games"
)

// Frame source kinds (docs/spec/08 "Example session workflow": the session
// names its frame source). v4l2 and dolphin are the live hardware paths that
// later milestones implement; this build answers them 501 rather than faking
// them (AGENTS.md "No simulation theater").
const (
	SourceKindReplay      = "replay"
	SourceKindTestPattern = "test_pattern"
	SourceKindV4L2        = "v4l2"
	SourceKindDolphin     = "dolphin"
	EnvReplayRoots        = "GAME_NARRATOR_REPLAY_ROOTS"
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

// buildFrameSource constructs the capture source for a session. replay is the
// fixture-replay source behind docs/spec/12's "Frame replay" gate;
// test_pattern is FFmpeg's testsrc2 through the same supervised subprocess
// the live capture uses (ADR-0012 part 1), so a dev host without a capture
// card still gets real frames through the production consumer.
//
// dolphin is the dev-only live emulator source (docs/spec/11, ADR-0007): it
// is admitted only when the operator opted in via EnvDevDolphin AND a bridge
// closure was wired into the manager. Either half missing answers 501
// (ErrSourceUnsupported) — the runtime never fakes the emulator and never
// launches one itself; the closure is the one place the dev bridge hands a
// ready frame source to the pipeline.
func buildFrameSource(kind, dir string, roots []string, speed float64, devDolphin func() (capture.Source, error), lookupEnv func(string) (string, bool)) (capture.Source, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	switch kind {
	case SourceKindReplay:
		dir, err := resolveReplayDir(dir, roots)
		if err != nil {
			return nil, err
		}
		return &replay.Source{Dir: dir, Speed: speed}, nil
	case SourceKindTestPattern:
		// 640x480 at the clip's 10 Hz cadence: the tracker's documented knobs
		// are sized to a 10 Hz recording, and the pattern needs no device.
		return ffmpegpipe.TestSource(640, 480, 10), nil
	case SourceKindDolphin:
		if v, _ := lookupEnv(EnvDevDolphin); v != "1" && v != "true" {
			return nil, fmt.Errorf("runtime: frame source kind %q is not enabled in this build (set %s=1 on a dev host with the bridge wired): %w", kind, EnvDevDolphin, ErrSourceUnsupported)
		}
		if devDolphin == nil {
			return nil, fmt.Errorf("runtime: frame source kind %q is enabled (%s=1) but no Dolphin bridge closure is wired into this process: %w", kind, EnvDevDolphin, ErrSourceUnsupported)
		}
		return devDolphin()
	case SourceKindV4L2:
		return nil, fmt.Errorf("runtime: frame source kind %q is not implemented in this build: %w", kind, ErrSourceUnsupported)
	default:
		return nil, fmt.Errorf("runtime: unknown frame source kind %q: %w", kind, ErrUnknownSourceKind)
	}
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
