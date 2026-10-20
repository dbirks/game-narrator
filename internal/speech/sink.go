package speech

import (
	"context"
	"time"
)

// PlayRequest asks one sink to play one utterance. WAV must be a validated
// RIFF/WAVE (tts.ValidateAudio) so sinks may assume PCM; a sink must not be
// handed audio that failed validation (quarantine, docs/spec/07 "Failure
// behavior").
type PlayRequest struct {
	// UtteranceID is the occurrence-level identity that event logs correlate
	// on (docs/spec/09 "Link everything").
	UtteranceID string
	// Generation is the utterance generation token the fence approved. It is
	// redundant with the fence for playback itself but carried so sink-level
	// diagnostics (and filesink names) are self-describing.
	Generation int64
	// WAV is the complete utterance WAV.
	WAV []byte
	// Gain is a linear volume factor (1.0 = unity). Sinks that cannot apply
	// gain (a subprocess they do not control) report it in diagnostics rather
	// than pretend.
	Gain float64
	// FadeOutMs is the fade applied at the natural end of the utterance.
	// Stop() applies its own (possibly different) fade on interruption.
	FadeOutMs int
}

// PlayResult is the terminal outcome of one playback.
type PlayResult struct {
	// Started is when the sink reported/observed the first sample leaving the
	// sink API (not necessarily the speaker; device latency is excluded and
	// reported separately — docs/spec/12 stop-latency wording).
	Started time.Time
	// FirstSampleAt is when the sink first handed samples to the device
	// buffer (the pipewire sink stamps its first successful write to the
	// player process; zero when the sink does not report it). The interval
	// Started → FirstSampleAt is the output latency docs/spec/13 asks to
	// count: how long the device side took before audio could come out.
	FirstSampleAt time.Time
	// Ended is when the sink finished (natural end, stop, or failure).
	Ended time.Time
	// Interrupted is true when Stop() ended the playback early.
	Interrupted bool
	// Err is non-nil for a sink-level failure (spawn, I/O, decode).
	Err error
}

// PlayHandle controls one in-flight playback.
//
// Stop is idempotent and non-blocking: it signals the sink to end playback
// after at most the given fade and returns immediately; the terminal outcome
// arrives via Done(). A sink must honour Stop well inside the 200 ms stop
// target of docs/spec/00 even if it cannot apply a true sample-accurate fade
// (document the measured stop latency, ADR-0023).
type PlayHandle interface {
	// Stop ends the playback after the given fade (0 = as fast as possible).
	Stop(fade time.Duration)
	// Done closes with the terminal result exactly once.
	Done() <-chan PlayResult
}

// AudioSink is the device boundary (docs/spec/07 "Audio scheduler": "audio
// sink abstraction also includes dummy/file/capture for CI and tests").
// Implementations: pipewire (subprocess pw-play/paplay to a named sink),
// filesink (records what would have played), dummy (programmable test sink).
//
// Contract:
//   - Play must not block the caller beyond process spawn / first write; the
//     sink does its own pacing. "Audio sink must never block session event
//     loop" (docs/spec/07) is enforced by the scheduler, which calls Play
//     from its own goroutine, and by this interface's non-blocking Stop.
//   - Stop may be called before, during, or after Done; after Done it is a
//     no-op.
type AudioSink interface {
	// Play starts playback and returns a handle as soon as the sink has
	// accepted the request. Err is for pre-flight failures (bad WAV, spawn
	// failure).
	Play(ctx context.Context, req PlayRequest) (PlayHandle, error)
	// Close releases sink-level resources (temp files, processes).
	Close() error
}

// Chunk is one sample block from a streaming source.
type Chunk struct {
	// PCM is raw interleaved PCM, already in the source's format
	// (SampleRate/Channels/Bits). No WAV framing here: framing is the
	// player's job once the stream is complete or fenced.
	PCM []byte
	// Err ends the stream with a reason (TTS aborted, 429, decode error).
	// When Err is non-nil, PCM is empty and no further chunks arrive.
	Err error
}

// ChunkSource is a streaming TTS source: one that delivers PCM as it is
// synthesized (docs/spec/07 "support chunked playback for valid streaming
// TTS while fencing each chunk"). The scheduler fences at every chunk
// boundary: if the generation is no longer current, it stops reading and the
// partial audio never plays (docs/spec/12 "TTS partly streams before
// cancellation"; "late audio never plays").
//
// Next blocks until the next chunk is ready or ctx is done.
type ChunkSource interface {
	// Next delivers the next chunk. On end-of-stream it returns
	// Chunk{Err: io.EOF} — or, equivalently, io.EOF as the error; either
	// form terminates the stream cleanly and no more chunks are delivered.
	// The source must respect ctx: after ctx is done, Next returns
	// immediately with a ctx-derived Err and no more chunks are delivered
	// (the failure-injection case "TTS partly streams before cancellation"
	// asserts exactly this).
	Next(ctx context.Context) (Chunk, error)
	// SampleRate is the PCM sample rate (Hz).
	SampleRate() int
	// Channels is the channel count.
	Channels() int
	// Bits is the bits per sample (16 or 32).
	Bits() int
}

// Fence is the generation-approval boundary. The session reducer implements
// it (only the event reducer can approve a playback token, docs/spec/07
// "Audio scheduler"). The scheduler asks the fence BEFORE starting and AT
// EVERY chunk boundary; a "no" at any point means: do not start / stop /
// drop, never play stale audio.
type Fence interface {
	// IsCurrent reports whether generation is the currently committed
	// generation. Equality, not "≤": a clip for generation 11 that finishes
	// after generation 12 committed is stale (docs/spec/01 invariant 2:
	// commit_id and utterance_generation fence stale asynchronous work).
	IsCurrent(generation int64) bool
}
