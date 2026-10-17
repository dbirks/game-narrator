// Package ocr runs the local OCR engine as a supervised subprocess.
//
// ADR-0012 is the reason this is a process and not a library: the release stays
// CGO_ENABLED=0, a segfault inside leptonica degrades one capability instead of
// taking down the process that owns the database handle and the audio device,
// and the tool's version is a per-host fact that `game-narrator doctor` probes
// rather than a build-time assumption.
//
// The supervision contract that ADR-0012 §2 imposes on this package:
//
//   - argv only, never a shell string: no `sh -c`, so a filename with spaces or
//     a semicolon is one argument, not a command.
//   - a hard deadline, and cancellation kills the *process group*. An OCR child
//     that ignores SIGTERM must not survive its scene observation
//     (docs/spec/05 "immutable scene observations", "Deadline/cancellation
//     propagation").
//   - bounded pipes: stdout and stderr are drained even after the cap is reached,
//     so a chatty child cannot deadlock on a full pipe while the parent stops
//     reading, and a 10 MB stderr dump cannot grow the Go heap.
//   - typed failures: unavailable, timeout, cancelled and failed are distinct,
//     because the caller's retry policy and the UI's "OCR unavailable" message
//     differ per kind (ADR-0009's provider-style failure taxonomy).
//
// Engine is the seam docs/spec/05 needs: a VLM fallback ("compare lightweight OCR
// against optional VLM fallback for difficult fonts") implements the same
// interface over the same Result, so the recognition stage never branches on
// which engine produced a hypothesis.
package ocr

import (
	"context"
	"errors"
	"image"
	"time"
)

// Engine recognizes text in one image. Implementations must honor ctx: the
// runtime cancels a recognition whose scene observation has been superseded by a
// newer frame, and an engine that returns late would let a stale observation
// reach the reducer (docs/spec/05 "model responses are advisory messages
// accepted only when event epoch … match").
type Engine interface {
	Recognize(ctx context.Context, img image.Image) (Result, error)
}

// Name reports the engine's stable identifier for logs and for the OCR
// provenance field on an observation. It must not be derived from user input:
// the recognition log is part of an evidence bundle (AGENTS.md "manifest").
type Namer interface {
	Name() string
}

// Result is one recognition pass over one image.
//
// docs/spec/05 requires more than concatenated text: "Return OCR word boxes and
// confidence, normalized tokens and alternative glyph hypotheses, not just
// concatenated text." Word boxes are what make a later stage able to ignore a
// low-confidence textbox-edge cursor or to tell a two-line page from a wrapped
// one; confidence is what the stability threshold and the VLM-fallback decision
// are computed from.
type Result struct {
	// Text is the hypotheses joined by line, in reading order, exactly as the
	// engine returned them — raw. docs/spec/05 "Preserve raw observed OCR and
	// normalized text separately" makes normalization the caller's step
	// (internal/games/textnorm), never a silent edit here.
	Text string
	// Lines preserves line breaks and per-line geometry.
	Lines []Line
	// Words is the flat word stream, in reading order.
	Words []Word
	// MeanConf averages word confidences in [0,1]; 0 when no word was read.
	MeanConf float64
	// Duration is wall-clock time inside Recognize, including the process spawn
	// — the number the p50/p95 OCR latency benchmark reports (docs/spec/05
	// "Timings and benchmarks").
	Duration time.Duration
	// Engine names the producer ("tesseract"), for discrepancy reporting when a
	// VLM and the local engine disagree.
	Engine string
	// Fallback is the docs/spec/05 "include both and the discrepancy" audit row,
	// populated only by FallbackEngine: both readings, their normalised
	// discrepancy, which one won and why. nil means the read came from a single
	// engine and nothing was compared.
	Fallback *FallbackOutcome
}

// Line is one recognized line.
type Line struct {
	Text string
	// Conf is the line's mean word confidence, [0,1].
	Conf float64
	Box  image.Rectangle
}

// Word is one recognized word.
type Word struct {
	Text string
	// Conf is the engine's word confidence normalized to [0,1]. Tesseract's own
	// scale is 0–100, and its structural rows report -1; -1 becomes 0 here so a
	// caller filtering "conf > 0.6" cannot be confused by a missing value.
	Conf float64
	// Box is the word's pixel rectangle in the coordinate space of the image
	// passed to Recognize — so a caller that upscaled 2× before OCR sees boxes
	// in the upscaled space and must divide back.
	Box image.Rectangle
}

// Sentinel errors for the failure kinds a caller must distinguish. They are
// wrapped, never returned bare, so errors.Is works while the message still names
// the binary and the stderr tail.
var (
	// ErrEngineUnavailable means the binary could not be found or could not be
	// started. Retryable only after an operator installs the package — which is
	// what makes it different from ErrFailed (docs/spec/10 "Require … Tesseract
	// +language packs").
	ErrEngineUnavailable = errors.New("vision/ocr: engine unavailable")
	// ErrTimeout means the deadline in Tesseract.Timeout elapsed. Retryable:
	// the region may simply have been expensive, and the next frame is a new
	// observation.
	ErrTimeout = errors.New("vision/ocr: engine timed out")
	// ErrCancelled means the caller's context was cancelled first, i.e. the
	// runtime no longer wants this observation. It is not a fault and must not
	// be counted against the engine.
	ErrCancelled = errors.New("vision/ocr: recognition cancelled")
	// ErrFailed means the engine ran and reported a non-zero exit or unreadable
	// output; Stderr carries the truncated tail.
	ErrFailed = errors.New("vision/ocr: engine failed")
	// ErrOutputTooLarge means stdout exceeded the configured cap; the result is
	// untrustworthy, so it is an error rather than a truncation.
	ErrOutputTooLarge = errors.New("vision/ocr: engine output exceeded the cap")
)

// Failure carries the engine's own words alongside the kind. Stderr is truncated
// to StderrTail bytes: enough for an operator to act on, bounded so a tool that
// dumps 10 MB on start-up cannot grow the appliance's heap.
type Failure struct {
	Kind   error
	Engine string
	Args   []string
	Stderr string
	Err    error
}

func (f *Failure) Error() string {
	msg := f.Kind.Error()
	if f.Engine != "" {
		msg += " [" + f.Engine + "]"
	}
	if len(f.Args) > 0 {
		msg += " args=" + argvBrief(f.Args)
	}
	if f.Stderr != "" {
		msg += " stderr=" + f.Stderr
	}
	if f.Err != nil {
		msg += ": " + f.Err.Error()
	}
	return msg
}

func (f *Failure) Unwrap() []error {
	if f.Err == nil {
		return []error{f.Kind}
	}
	return []error{f.Kind, f.Err}
}

// Timeout satisfies the net.Error-shaped Timeout() check the jobs layer uses for
// backoff decisions.
func (f *Failure) Timeout() bool { return errors.Is(f.Kind, ErrTimeout) }

// argvBrief renders the argv for a log line without pretending to redact it: the
// OCR path takes no secrets, and a truncated-looking argument list that hides the
// real cause is worse than a verbose one.
func argvBrief(args []string) string {
	const cap = 160
	s := ""
	for _, a := range args {
		if len(s)+len(a)+1 > cap {
			return s + "…"
		}
		if s != "" {
			s += " "
		}
		s += a
	}
	return s
}
