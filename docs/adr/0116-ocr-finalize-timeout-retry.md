# ADR-0116: The fixture finalize's OCR pass retries a timed-out engine read (bounded)

Date: 2026-10-20
Status: accepted
Owners: dolphin-gate worker (task dolphin-gate)
Related requirements/tests: every `TestRealRecord*` re-record under
`GN_GATE=test-dolphin` (internal/dolphin); docs/spec/12 (Dolphin emulator
mode); AGENTS.md rule 11 (bounded autonomous troubleshooting: retry only
idempotent operations with max attempts); internal/vision/ocr (the
`ErrTimeout` sentinel's documented semantics)

## Context and constraints

The 2026-10-20 17:23 `TestRealRecordSavestateFixtures` re-record (the
external fixture root, the ADR-0123 first-dialogue anchor validated — the
held box measured steady at boxband 46/0.031 for 3 consecutive 2 s
samples, and phase 3 pressed the first conversation through 4 settled
lines) failed in `Finalize`'s record-time OCR pass:

```
first-dialogue fixture: dolphin: fixture OCR frame frame-00055.png:
  vision/ocr: engine timed out [tesseract] args=--psm 6 -l eng tsv
```

One 4 s tesseract deadline (the engine's `DefaultTimeout`, measured
generous for the 550×110 crop) elapsed under the shared host's load
(other workers' Go builds were at 100% CPU). The finalize propagates the
engine error as-is, so one slow frame aborted a 13-minute real-Dolphin
recording whose replayable frames were already written — and the failure
kind is exactly what the OCR package documents as retryable: `ErrTimeout`
is "Retryable: the region may simply have been expensive, and the next
frame is a new observation." The read is idempotent — the crop's exact
pixels are the input, so a fresh deadline on the same pixels is the retry
(same pixels, same answer).

Constraints: the finalize's OCR is the fixture's truth source (the
record-time reading, the dedupe cache keyed on the crop's exact pixel
hash, ADR-0054 amendment 1) — a retry must not change what is read, only
how often the engine is given its deadline. AGENTS.md rule 11: retry only
idempotent operations, with max attempts, and respect cancellation.

## Options

1. **Raise the engine's `DefaultTimeout`** (e.g. 4 s → 10 s). Affects
   every Tesseract consumer (the live pipeline, the bench replay, the
   scenario replay) to protect one recording path, and a bigger deadline
   does not bound a pathological frame — it only delays the same abort.
2. **Retry the timed-out read, bounded** (this ADR): in the fixture
   finalize's shared OCR core (`ocrImageCrop`, internal/dolphin/record.go),
   a read that fails with `errors.Is(err, ocr.ErrTimeout)` is re-run on
   the same pixels with a fresh per-read deadline, at most
   `ocrTimeoutRetries` (3) attempts, sleeping `ocrTimeoutBackoff`
   (1 s) between attempts, and bailing on `ctx` cancellation. Non-timeout
   errors (`ErrFailed`, `ErrOutputTooLarge`, …) are not retried — they
   are not the load flake the retry targets.
3. **Rerun the whole test when it times out** (operator-level). Works,
   but re-rides 10+ minutes of real Dolphin for a one-frame engine
   flake, and leaves the gate one busy moment away from red.

## Decision

Option 2, in one place: `ocrImageCrop` (the shared core of the story-sub
and the adaptive box-text crops, so every re-record test — newgame,
legend-dense, legend-press, first-dialogue, telescope — gets the bounded
retry). Constants `ocrTimeoutRetries = 3`, `ocrTimeoutBackoff = 1 s`. The
retry changes no reading: identical crop pixels hash to the same cache
key, and the engine's answer for them is what is recorded either way.

## Tradeoffs and validation

- Worst case a genuinely pathological frame costs up to 3×4 s + 2×1 s
  (~14 s) per crop read instead of failing the run — against a 40-minute
  test budget that is noise; best case (the measured case) one retry
  clears the host's load.
- A retry cannot mask a real engine failure: `ErrFailed` and friends are
  not retried, and a frame that times out on all 3 attempts still fails
  the finalize with the engine's own words.
- Validation: `go build ./internal/dolphin/` + `go vet`; the focused
  rerun `TestRealRecordSavestateFixtures` (`GN_GATE=test-dolphin`, the
  external fixture root) after the 17:23 timeout; the full
  `make test-dolphin` gate (every re-record test runs the same finalize
  path).

## Reversal

Delete the retry loop in `ocrImageCrop` (restore the single `Recognize`
call) and the two constants; the finalize returns to fail-fast on the
first timed-out crop read. Nothing else depends on the retry.
