# ADR-0100: Persistent OCR engine — warm Tesseract process pool (issue #13)

Date: 2026-10-20
Status: accepted
Owners: issue13-ocr-engine worker
Related requirements/tests: GitHub issue #13 (persistent OCR engine, no per-read
Tesseract startup), issue #8 (live OCR within the frame budget), docs/spec/05
(OCR pipeline, 4 s deadline, "timings and benchmarks"), docs/spec/13
(backpressure and memory ceilings), docs/spec/14 (privacy: crop only), ADR-0012
(Tesseract wrapper contract: argv-only, process-group kill, bounded pipes, typed
failures), ADR-0096 (live OCR latency: the 0.59 s fixed startup measured, the
p95 < 700 ms target recorded as the persistent-engine follow-up),
ADR-0085 (one-shot `ocr` command); tests
`TestTesseractRunsRealImageThroughInputFile`,
`TestPooled*` (internal/vision/ocr: protocol, crash recovery, deadline,
cancellation isolation), `TestHWOCRLatencyDevice` (internal/hwsmoke,
`GN_GATE=test-hardware`)

## Context and constraints

ADR-0096 measured the live A53 read at 1.13 s: 0.59 s of fixed process
startup (spawn, leptonica init, 4.1 MB "fast" `eng` model load, LSTM init)
plus ~0.54 s of inference. The p95 < 700 ms target (issue #8, carried into
issue #13) is unreachable with a per-call engine process — ADR-0096's
follow-up named the only path: a persistent engine that amortises the 0.59 s
startup. Issue #13 additionally lists the shape options: a long-lived
Tesseract over a pipe (batch/list mode), a small persistent helper process,
or a C API binding.

Constraints: the release build stays `CGO_ENABLED=0` (pure Go, no
libtesseract in the Go binary — ADR-0012's reason); Python is not on the
release appliance; the supervision contract of ADR-0012 §2 holds per engine
call (argv-only exec, process-group kill on cancellation, bounded pipes,
typed failures); docs/spec/13's memory ceiling (the UNO Q keeps 4 GB stable);
cancellation of one caller's read must not touch another caller's in-flight
read.

## What the board measurement says (measured for this ADR, 2026-10-20/21,
UNO Q A53, tesseract 5.5.0 / leptonica 1.84.1, the 4.1 MB "fast" `eng`, the
pipeline's preprocessed 2064×376 dialogue crop)

| probe | wall time |
|---|---|
| fresh process per read (file operand) | 1102–1164 ms (in-product `ocr --image`: 1116–1121 ms, matching ADR-0096) |
| **batch/list mode from one stdin stream** | **does not exist**: two concatenated PNGs piped to `tesseract stdin stdout` produce one page in the same wall time (1416 ms single vs 1422 ms two); `tesseract --help` lists no batch/list/persistent option |
| fixed startup (16×16 input, fresh) | 569–578 ms |
| **prewarmed process (spawned, stdin held open, image fed after 1.5 s idle)** | **567–584 ms for the preprocessed dialogue crop** — the entire fixed cost is paid at spawn, before the first image is read |
| RSS of one warm (blocked) process | **29 MB** (1 thread, state S) |
| a model load overlapping an in-flight read | warm read degrades to 1301 ms (vs 890 ms with an idle sibling); ADR-0096's 17× for two concurrent OCRs is the same effect — a load must never overlap a read |
| **2-slot warm pool, reads at the live A53 pace (~0.9 Hz, 1.1 s cadence), 14 reads** | **p50 576 ms, p95 593 ms, max 593 ms** |
| 2-slot pool, back-to-back reads (reads faster than the engine) | p95 1084 ms — the replacement's load overlaps a read; the live pipeline does not run this shape (ADR-0096's cap + coalescing) |
| 1-slot pool at the live pace | p95 986 ms and degrading — the 0.59 s load does not fit the 0.52 s read-free gap, so two slots are the minimum |
| SIGKILL of a warm process mid-read | detected in 8 ms (exit -9, 0 B stdout); the per-process fallback path reads the crop in 1481 ms |

Reading the table: the fixed cost is paid once per warm process, at spawn,
before the first image; a warm read costs inference only (~0.57 s measured);
and the only hazard is overlapping a model load with an in-flight read.

## Decision

1. **A new `ocr.Pooled` engine keeps a small pool of warm `tesseract` child
   processes.** Each slot is `tesseract stdin stdout <engine args>` (the
   wrapper's ADR-0012 argv shape with `stdin` as the input operand): the
   process spawns, pays its fixed startup while blocked on reading the first
   image from the open stdin pipe, and stays alive until an image is fed.
   `Pooled.Recognize` encodes the preprocessed crop as PNG (the wrapper's
   existing recipe: lossless, NoCompression), hands the bytes to one warm
   slot's stdin, collects the TSV from its stdout, and parses it with the
   wrapper's `ParseTSV` — the same typed failures, caps and Result the
   per-process path produces. One slot serves exactly one read: stock
   tesseract reads one image and exits (measured: no batch mode), so the
   pool respawns the replacement immediately after the read settles.
2. **Two slots by default (configurable, clamped 1..4), and reads are
   serialised: at most one OCR in flight.** The measurement above is why:
   a replacement's model load overlapping a read costs 0.4 s extra (1301 ms
   vs 890 ms), and two concurrent OCRs cost the ADR-0096 17×. The second slot
   exists so the next read has a warm process while the previous slot's
   replacement loads in the read-free gap (measured p95 593 ms at the live
   pace). Serialising OCR inside the engine is the live-mode cap
   (ADR-0096 decision 1) extended to every mode: the pipeline's
   `MaxConcurrentOCR` remains the outer bound, and the engine never runs two
   OCRs at once on the A53's scheduler. The cost is bounded: a waiting
   reader is the pipeline's own bounded worker (its 4 s deadline is the wait
   bound), and live sessions cap at one read at a time anyway.
3. **The per-process path stays, and is the automatic fallback.** A read that
   cannot use the pool (the pool is closed, or degraded — four consecutive
   slot failures, the crash-loop shape) runs `Tesseract.Recognize` (the
   existing per-call process) on the remaining deadline. Degradation is the
   only fallback trigger: a single crash gets an immediate replacement
   spawn (crash recovery, no per-read penalty — measured: a fresh-process
   read costs the same 1.1 s a cold slot would), and the pool recovers on
   the next successful slot read. The fallback is what keeps a broken
   engine from burning the pool's backoff churn inside the frame budget:
   the read gets the same typed failure the per-process path has always
   given (ADR-0012 §4), and the operator's `doctor`/stderr tail is
   unchanged.
4. **Supervision, deadlines and cancellation follow ADR-0012 §2 per slot,
   scoped to the caller's read.** Each slot is its own process group
   (`Setpgid`); the read's context (`caller ctx` ∪ `Tesseract.Timeout`,
   default 4 s) is the slot's `exec` context, so a deadline or a caller
   cancel SIGKILLs only that slot's group — the other slots and any other
   caller's read are untouched. Stdout is bounded by the wrapper's cap
   (8 MiB) and stderr by its tail (2 KiB) per read; the slot's stdout/stderr
   pipes are drained to EOF before the read returns, so a wedged child
   cannot outlive the read (the wrapper's `WaitDelay` grace covers the
   inherited-fd edge). Cancellation of one caller's read never cancels
   another's: the contexts are per read, not per pool.
5. **Memory is bounded by the slot count.** Two warm slots measure 29 MB RSS
   each on the A53 (~58 MB total, 1 thread each, blocked in a pipe read —
   zero CPU while idle). The pool adds no queues beyond the one-read gate
   (docs/spec/13: the reader waiting on the gate is the pipeline worker,
   already bounded by `MaxConcurrentOCR` and its deadline).
6. **Wiring.** The runtime uses one process-shared `Pooled` (default two
   slots over the production `Tesseract` settings) wherever the session
   runtime today falls back to `ocr.Tesseract{}` — i.e. whenever
   `runtime.Options.OCR` is nil (the production shape); a configured
   `Options.OCR` (the test seams) is used as given, so scripted/fake engines
   are never wrapped. The one-shot `ocr` command (ADR-0085) constructs the
   pool before the image decode so the warm spawn overlaps the decode,
   crop and preprocess and the engine call costs inference only. The
   region-editor OCR preview (main.go's `WithOCRPreview`) stays per-request
   per-process: one read behind a 15 s budget, where the 0.59 s startup is
   invisible and a per-request pool would leak slots the API never closes.
   The bench keeps per-sample `Tesseract{}`: its contract is determinism
   over repeated samples (ADR-0064) and it runs on the dev host, where the
   per-call cost is tens of milliseconds; the persistent bench engine is a
   follow-up if frame-to-voice in the bench becomes the bottleneck.
7. **The hardware gate asserts the issue's target.** `TestHWOCRLatencyDevice`
   now runs the one-shot `ocr --image` command over the controlled crop nine
   times and asserts p95 ≤ 700 ms for the engine call (the issue #13 target,
   previously 1100 ms — ADR-0096's documented per-process floor — because
   the one-shot command now runs the pooled engine). The controlled 400×100
   crop is the gate's self-contained input (game crops cannot be committed);
   the dialogue-crop p50/p95 evidence is the board measurement above.
8. **The slot protocol is a capability the pool detects at startup, not an
   assumption (capability probe).** The pool's slots run the engine's
   `stdin` operand — one image on the slot's stdin, the TSV answer on its
   stdout, the slot's exit — a protocol measured on the engine ADR-0100
   ships with (stock tesseract, ADR-0012 §1), not a contract every
   `Engine`-shaped binary carries. `NewPooled` therefore settles one small
   real read through the slot protocol in a **throwaway process** at
   construction (`Tesseract.probeSlotProtocol`, `Pooled.runProbe`), bounded
   by the engine's own deadline and serialised on the pool's one-read gate
   (the probe runs the model; a model load overlapping a read is the
   decision-2 hazard). The verdict:

   - a settled answer that parses as the wrapper's TSV (empty is fine):
     the protocol holds; nothing is recorded.
   - a settled answer that is **not** the TSV the wrapper's argv asks for
     (`ErrTSVUnparseable`, a clean exit with an unparseable stream): the
     engine lacks the protocol. `Pooled` records the verdict and serves
     **every** read on the per-process fallback — decision 3's shape for a
     capability the engine lacks, triggered by the probe's typed verdict
     rather than the crash-loop count. An engine that cannot read `stdin`
     would otherwise fail every slot read with a typed error while the pool
     churned spawn/crash/backoff for a protocol the engine never had.
   - anything else (non-zero exit, crash, the deadline, an unstartable
     binary): the engine may still speak the protocol; it failed this read,
     and decision 3's ordinary machinery owns a failing engine.

   The throwaway shape is deliberate: the verdict is a property of the
   engine, and measuring it must not perturb the pool's operational state —
   no slot consumed, no degrade count, no backoff — so the reads find
   exactly the pool `NewPooled` built. A read that races the unsettled probe
   runs the slot path as normal (worst case: one read's deadline, one typed
   failure); once the verdict is settled on an unsupported engine, every
   read takes the fallback. The probe's image is the ADR's fixed-startup
   16×16 probe: the probe verifies the protocol (spawn, one image on stdin,
   the TSV answer, the exit), not a recognition.

## Alternatives considered

- **Tesseract batch/list mode over one persistent process (issue #13's
  option 1, the task's first preference).** Measured and rejected: stock
  5.5 reads exactly one image from a stdin stream (two concatenated PNGs →
  one page, same wall time) and exposes no batch/list/persistent flag. The
  persistent unit is therefore one warm process per read, which is the pool.
- **A persistent helper that links libtesseract (C or cgo).** The only way
  to keep one process serving unbounded reads. Rejected: cgo breaks the
  `CGO_ENABLED=0` release rule (ADR-0012's core reason for the subprocess);
  a C helper is a second native build artifact the GoReleaser release does
  not produce (per-arch cross-compilation, a libtesseract runtime
  dependency, a second supervision surface) — the release-artifact class of
  problem the rule exists to avoid. If the board ever shows the pool's 0.57 s
   inference floor still matters, this is the decision to reopen, with a
   release-pipeline ADR.
- **A pool of per-call processes without prewarm (spawn at read time, reuse
   nothing).** Measured: identical to today (1.1 s) — the startup is the
  cost; only pre-spawned-and-idle processes amortise it (the 0.59 s gap
  between fresh 1102 ms and prewarmed 567 ms).
- **One-slot pool.** Measured p95 986 ms at the live pace and degrading: the
  0.59 s load does not fit the 0.52 s gap between reads, so the second read
  waits on the load. Two slots is the measured minimum.
- **Concurrent warm slots (no read serialisation).** Measured: the
  replacement's load overlaps a read (1301 ms) and two OCRs cost the
  ADR-0096 17× on the A53. Serialising is strictly better on the target
  hardware and invisible on the dev host (reads there are tens of ms).
- **Core-pinning the slots (`taskset`).** ADR-0096 already rejected it
  (CPU knowledge the product should not own); the serialisation makes the
  contention that pinning worked around not happen.

## Consequences and risks

- Live A53 reads settle in ~0.57–0.59 s measured (p95 593 ms at the live
  pace) — inside issue #13's 700 ms target and the 4 s hard deadline with
  6.8× headroom; the one-shot `ocr` command's engine call drops from the
  1.12 s per-process floor to the inference-only cost (the gate now asserts
  the 700 ms target).
- ~58 MB of steady RSS for the two warm slots on a 3.6 GB appliance (the
  serve process's documented budget is 512 MiB, docs/manuals/13); the slots
  hold no frame content (privacy: they receive the same crop bytes the
  per-process path already receives, docs/spec/14), and they are children of
  the narrator — when the narrator exits, the stdin pipes EOF and the slots
  exit with the engine's own error, so no warm tesseract can outlive the
  process that owns it.
- A crash-looping engine (bad tessdata, broken install) degrades the pool
  after four failures: reads then run the per-process path (the same typed
  failures as today) while the pool retries its replacement in the
  background; one successful slot read clears the degradation (automatic
  recovery). The operator-visible behaviour of a broken engine is exactly
  what ADR-0012 already documents.
- Reads wait behind one in-flight OCR (the engine's serialisation). Live
  sessions already cap at one read at a time (ADR-0096); replay-mode reads
  that previously ran two at once now run one at a time — the replay
  determinism suite is unaffected (the readings are the same; only the
  wall-clock overlap changes), and the A53's 17× concurrent-OCR cost is
  avoided in every mode.
- `Pooled` is a new exported engine type; `Options.OCR` callers that pass a
  real `Tesseract` (not the default) get no pooling — opt-in by wrapping,
  which is what the runtime and the one-shot command do.
- The 700 ms gate budget assumes the pooled one-shot command: a host running
  the gate against an older binary (per-process) will fail the gate with the
  documented floor of ~1.1 s — the gate asserts the shipped behaviour, as
  ADR-0096's 1100 ms budget asserted its floor.

## Evidence and test plan

- Board measurements above (2026-10-20/21, UNO Q A53, tesseract 5.5.0):
  the batch-mode probe, the prewarm gap, the RSS, the load-overlap penalty,
  the 2-slot p50/p95 at the live pace, and the mid-read SIGKILL recovery.
  Re-measurement recipe appended to docs/manuals/18.
- `TestPooledReadsThroughTheSlotProtocol` (internal/vision/ocr): a fake
  engine process (shell stub, the package's existing test convention)
  proves the protocol end to end — the crop arrives on the slot's stdin,
  the TSV on its stdout is parsed, the argv is `stdin stdout <engine args>`,
  and a second read on the pool runs a replacement slot (the model is kept
  loaded across reads: both reads settle, the stub records two invocations).
- `TestPooledFallsBackToPerProcessWhenSlotProtocolUnsupported` (decision 8,
  the capability probe): a fake engine that lacks the slot protocol — its
  `stdin` operand answers in a shape the wrapper does not parse (a clean
  exit, not the TSV) while its file-operand path speaks TSV. The startup
  probe records the verdict, and every read after it runs the per-process
  path: the reads settle with the fallback's text, and the argv log proves
  no read after the verdict ever used the slot protocol.
- `TestPooledCrashRecoversWithBackoff` / `TestPooledDegradedPoolFallsBackToPerProcess`:
  a stub that dies on its first N reads (SIGKILL itself) — the next read is
  served by a replacement (recovery), and after four consecutive failures
  the pool runs the per-process fallback while a background replacement
  heals it; the fallback's typed failure is the wrapper's, not the pool's.
- `TestPooledDeadlineKillsOnlyTheCallerSlot`: a stub that ignores its input
  (blocks) is killed at `Timeout` with `ErrTimeout`, and the pool survives
  with a replacement (the read cost its deadline; the pool does not wedge).
- `TestPooledCancelledReadKillsOnlyItsSlot`: caller A's read holds a slot;
  A cancels — A returns `ErrCancelled`, A's slot process is SIGKILLed (the
  recorded pids prove the process died), and caller B's read settles with
  its text on the other slot: cancellation of only the caller's read.
- `TestPooledHealsAfterDegradation`: a degraded pool keeps probing the
  engine on its backoff; when the engine heals, the probe's slot read
  settles and the degradation clears without an operator restart
  (automatic recovery, ADR-0100 decision 3).
- `TestPooledClosedPoolFallsBack` / `TestPooledEmptyImageFailsTyped` /
  `TestPooledSlotCountIsClamped`: the closed-pool fallback, the wrapper's
  input contract, and the slot-count clamp (the memory bound).
- `TestHWOCRLatencyDevice` (internal/hwsmoke, `GN_GATE=test-hardware`):
  nine one-shot `ocr --image` runs over the controlled crop; the engine-call
  p95 must be ≤ 700 ms (the issue #13 target) and every run must read the
  controlled text.
- Full regression: `go test -race ./internal/vision/... ./internal/pipeline/...
  ./internal/runtime/... ./cmd/...` (replay determinism suite included).

## Migration / rollback

No data, protocol or schema change; the engine's wire shape to the pipeline
(`ocr.Engine`) is unchanged. Rollback: revert the wiring (runtime's
`pipelineConfig`, the one-shot command's pool construction) and the gate
budget — the per-process path is untouched and serves every read exactly as
ADR-0096 documented. `Pooled` itself is additive and inert unless
constructed. No migration needed in either direction; the on-device binary
is replaced by the next release.

## Follow-ups

- **The bench's persistent engine** (issue #13's "frame-to-voice in the
  bench"): per-sample `Tesseract{}` keeps the bench deterministic and
  dev-host-shaped; a shared pool across bench samples is a small change if
  the bench ever runs on the A53 at scale.
- **Re-measure the live session on the board after release**: `ocr_calls`
  with high settle ratio, `ocr_cancelled` near zero, and frame-to-voice in
  the bench should now reflect the ~0.57 s OCR floor (ADR-0096's follow-up
  re-measurement, now with the persistent engine in the loop).
- **A libtesseract-linked helper** remains the only path below the ~0.57 s
  inference floor; it needs a release-pipeline ADR (second native artifact)
  and is out of scope while the inference-only read beats the 700 ms target.

## Correction (2026-10-20): the slot's TSV is Wait-owned, not reader-drained

Decision 4's "stdout pipes are drained to EOF before the read returns"
shipped with a race: the slot's stdout was a `StdoutPipe` the read drained
with `io.Copy`, while the slot's watcher — the only legal `cmd.Wait` caller
— runs `Wait` concurrently. `Wait` closes a StdoutPipe's read end "after
seeing the command exit" (os/exec), so a drain descheduled between the
child's exit and its pipe-buffer read loses the TSV: the read then parses
empty output on a clean exit — `empty tsv output` on a child that answered
correctly. Observed once under full parallel `-race` load
(`TestPooledReadsThroughTheSlotProtocol`), never in isolation: it needs the
reader descheduled at the exact exit moment.

Fixed the way `Tesseract.Recognize` and `probeSlotProtocol` already do it:
the slot's stdout is a `Wait`-owned, `StdoutCap`-bounded buffer
(`stdoutWriter`), and the read waits on the watcher's `done` — the
happens-after edge that makes the buffer's contents readable — instead of
draining a pipe it does not own. This also closes the one gap the decision's
cap claim had: the drain path's buffer was unbounded, so a misbehaving
child's heap growth was not actually bounded by the wrapper's 8 MiB cap;
the Wait-owned buffer is. Decision 4's drain sentence is superseded by this
correction; the rest of the decision stands.
