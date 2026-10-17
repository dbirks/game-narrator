// Package pipeline is the runtime recognition chain of docs/spec/05
// "Runtime stages": capture → ROI crop → preprocess → OCR (deduped by region
// hash) → dialogue state machine → session reducer ← candidate retrieval, with
// the resulting events published.
//
// It owns no state of its own beyond counters. docs/spec/01 "Parallelism and
// deadline contracts" is the shape of what it wires:
//
//   - one capture loop, feeding a bounded single-slot latest-frame queue
//     (capture.LatestSlot) rather than a lossless backlog, so a worker that is
//     200 ms behind analyses the current frame;
//   - a bounded ROI worker pool (Workers, default 2, max 4) executing OCR, which
//     is the only stage that blocks;
//   - cancellation of an in-flight recognition when a *newer frame of the same
//     region* shows different pixels — the result of a superseded read is
//     dropped and counted, never applied (docs/spec/05 "immutable scene
//     observations");
//   - one serialized reducer goroutine. Candidate ranking runs off it, on its
//     own workers, and returns as a session.RankingMsg stamped with the
//     observation sequence and generation the reducer fenced it against. The
//     matcher never touches reducer state (docs/spec/01 "models never mutate
//     session state").
//
// The bullets above describe Config.Mode == ModeLive, the default. ModeReplay
// turns the same Runner into the lossless fixture replay docs/spec/12 "Frame
// replay" runs in CI, and it changes exactly the two things that would make a
// replay's outcome depend on machine speed:
//
//   - delivery is QueueLossless whatever Config.Queue said — a bounded blocking
//     queue, the producer waits instead of dropping (a replay that drops a
//     frame is a different recording), so FramesDropped stays zero;
//   - a newer frame never cancels an in-flight recognition: each region's ROI
//     and OCR run to completion in frame order (the frame stream pauses for an
//     outstanding read), so OCRCancelled and OCRStale stay zero. The per-call
//     OCR timeout still bounds a wedged engine.
//
// Live dropping and cancellation tests must keep ModeLive on purpose; they are
// what the live bullets describe, and running them in ModeReplay would test a
// different pipeline than the one a player's session runs.
//
// There is no game-specific code path and no fixture-specific branch here
// (docs/spec/11 step 4): every threshold is a documented Config field, and the
// same Runner consumes a replay.Source, an FFmpeg pipe or a Dolphin-dev grab
// because all three deliver capture.Frame.
//
// docs/spec/01 invariant 1 is honoured on the way through: an observation
// carries the frame's own SourceClock and CaptureTimestampNS rather than a
// wall-clock reading taken in a worker, so stability windows measured on a
// replayed clip reproduce the recording's timings.
package pipeline
