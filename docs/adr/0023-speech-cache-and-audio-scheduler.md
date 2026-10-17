# ADR-0023: Speech cache key, singleflight TTS cache, generation-fenced audio scheduler, and subprocess PipeWire sinks

Date: 2026-07-21
Status: accepted
Owners: project
Related requirements/tests: docs/spec/07 "Cache", "Audio scheduler", "Failure behavior and test coverage"; docs/spec/01 "Parallelism and deadline contracts" (only the currently committed generation can play; prefetch lower priority), "Key invariants" 2 + 4; docs/spec/00 200 ms local stop target; docs/spec/11 "Avoiding false positives" (a WAV is not proof it was audible); docs/spec/12 failure-injection cases; builds on ADR-0005 (audio pipeline and cache keys), ADR-0022 (TTS client, WAV invariants)

## Context and constraints

- The session loop must never wait on TTS or audio I/O (docs/spec/07: "Audio
  sink must never block session event loop"; docs/spec/01: bounded queues,
  explicit backpressure, deadline/cancellation propagation).
- Only the currently committed generation may play; prefetch is lower
  priority; a clip synthesized for a stale generation must never play
  (docs/spec/01 invariant 2; docs/spec/12 "zero stale audio starts",
  "TTS partly streams before cancellation", "20 rapid advances").
- docs/spec/11 is binding for test design: a WAV file existing (or a
  provider call succeeding) is **not** proof the audio was delivered to a
  sink; tests must assert sink samples and playback events.
- The host (Debian ARM64, this TTY session) has a running PipeWire user
  session with only a virtual null sink (`auto_null`); no hardware audio is
  reachable. Evidence here is therefore **virtual-sink evidence**, and the
  acoustic path is NOT VERIFIED until a hardware smoke test with a mic.
- docs/spec/00 target: a cancel must stop local playback within 200 ms
  (device buffering excluded and reported separately, docs/spec/12).

## Decision

### Cache key

`internal/speech.CacheKey` is the SHA-256 (64 lowercase hex) of the
canonical JSON of exactly these fields (docs/spec/07 "Cache" + docs/spec/01
invariant 4), guarded by `version: 1` in the same JSON so a future field
change yields a disjoint key space instead of a silent collision:

1. `utterance_text` — canonical, TTS-normalized speech text (required
   non-empty; whitespace is identity).
2. `original_message_id` — the source dialogue message.
3. `speech_rules_revision` — rules revision that derived the text.
4. `language` — spoken language code.
5. `voice_revision_fingerprint` — immutable VoiceProfileRevision fingerprint
   (never a mutable voice name; docs/spec/07 voice lifecycle step 5).
6. `tts_model_id` — "model+version" exactly as routed.
7. `backend_config_digest` — digest of the synthesis backend configuration
   (endpoint policy, sampling parameters, output settings).
8. `style_variant` — pre-approved delivery style (empty = neutral).
9. `seed` — `*int`; **nil (not sent) is distinct from 0**.
10. `format` — stored output format (e.g. `wav/pcm16`).

Deliberate interpretation: the spec's `supported_seed+sampling` pair is
encoded as `seed` (where the backend honors one) plus `seed`'s absence
encoded distinctly; sampling parameters are not a separate key field — they
are backend synthesis settings and therefore covered by
`backend_config_digest`. A backend that changes sampling without changing
the digest is a bug in the digest owner (ADR-0022 owns the digest inputs),
not a key hole. Field-set changes are a key-space change and must bump
`version`. Test: `TestCacheKeyFieldSensitivity` (one mutation per field,
including seed nil↔0) must pass for the no-collision contract to hold.

### Singleflight, priorities, bounded generation

`internal/speech.Cache` fronts the asset store:

- **Singleflight per key**: N concurrent `GetOrGenerate` callers for one
  key produce exactly one provider call; the rest share the leader's result
  (test: 50 concurrent → exactly 1 generation, identical SHA256).
- **Priority admission, not preemption**: provider calls are bounded by a
  semaphore (default 1, configurable) whose wait queue admits
  `Current > Next > Background`, FIFO within a priority. A fresh
  high-priority arrival beats strictly lower-priority waiters but does not
  cut in front of an equal-or-higher-priority waiter already queued, and
  never re-orders an in-flight call (the provider contract does not allow
  mid-flight reordering). Test: with semaphore 1, a background holder
  releases to `current` before `next`.
- **Validated, atomic commits**: only `tts.ValidateAudio`-passing audio is
  committed (stage → commit via the asset store; a failed generation is
  retried, never served). Silent or clipped audio is rejected.
- **QA gating**: entries marked `QAFailed` are skipped by default
  (`ErrQAFailed`) and playable only with an explicit override that still
  reports the failed state (reviewable, not silent). The ASR loop owns
  bounded regeneration with alternative seeds → new keys.
- **Restart durability**: the index is a pluggable `Index` hook
  (in-memory now); a restart test feeds the previous snapshot to a fresh
  cache and asserts zero re-generation.

### Audio sinks

`AudioSink.Play(ctx, PlayRequest{UtteranceID, Generation, WAV, Gain,
FadeOutMs}) → PlayHandle{Stop(fade), Done()}` where `PlayResult{Started,
Ended, Interrupted, Err}` is delivered exactly once. Sinks:

- **`pipewire.Sink{Target, Binary}`** — subprocess `pw-play --target=<name>
  --latency=50ms -` (default) or `paplay --device=<name> -`: argv only, no
  shell; the validated WAV is streamed into stdin in ~100 ms chunks (the
  pipe is the bounded buffer; the sink never blocks the caller for the
  utterance duration). `Target` is a required stable sink name (device
  identity rule, docs/spec/07). Stop = stop writing (close stdin) +
  SIGTERM, SIGKILL after 150 ms grace.
- **`filesink.Sink{Dir}`** — writes, in real time, exactly the samples that
  would have been delivered, one WAV per utterance, written only on
  completion/stop (no half-valid files). A stopped utterance writes a
  strictly shorter file whose bytes are the exact source prefix (test
  asserts byte equality, ~159 ms delivered of a 600 ms tone stopped at
  150 ms). This is the docs/spec/11 "assert samples" evidence for CI.
- **`dummy.Sink`** — real-time paced with programmable start/stop delays and
  recorded `Record`s (start/end timestamps, interruption) for scheduler
  timing assertions.

Measured stop latency (local, stop signal → sink terminal result; device
buffering excluded per docs/spec/12 wording):

| sink | measured | note |
| --- | --- | --- |
| pipewire (SIGTERM, virtual null sink) | **3–4 ms** | 3 s tone stopped at 500 ms; `pw-play --latency=50ms` device buffer is discarded, not faded |
| dummy (stop delay 5 ms) | ≤ ~10 ms | pacing-limited, test evidence |
| filesink (fade 0) | ~29 µs | in-process pacing, test evidence |

All far inside the 200 ms local target. On hardware the number to track is
the same measurement on the USB speaker sink; the device-side buffer adds a
bounded, reported constant that must be excluded from the stop-latency
claim (docs/spec/12).

### Scheduler and fencing

`internal/speech.Scheduler` runs its own goroutine with a bounded inbox
(16); `Submit` never blocks the caller (full inbox → newest item shed +
`speech.ended{result:"failed"}` event — explicit backpressure,
docs/spec/01). It owns at most one active playback. Fencing points
(docs/spec/01 invariant 2; docs/spec/12 "zero stale audio starts"):

1. **Before starting** — the popped item is checked against the `Fence`
   (`IsCurrent(generation)` — equality, not "≤") and against a
   pending-cancel map; stale items are dropped + evented, never played.
2. **At every chunk boundary** — streaming `ChunkSource`s are read one
   chunk at a time; before each pull the fence and cancel state are checked,
   and the stream context is cancelled on a stale verdict so the source
   stops receiving `Next` calls (test: 4/20 chunks delivered, source frozen
   after cancel; partial audio never plays; drop is evented as failed).
3. **On a 100 ms ticker** during any buffered playback — a fence flip
   mid-playback stops the playback within the same 200 ms budget as an
   explicit `Cancel(generation, reason)` (which also drops queued items of
   that generation). Every stop emits `speech.cancelled{reason}`; outcomes
   emit `speech.started` / `speech.ended{result: ended|failed}` through the
   `Publisher`.

The `Fence` is implemented by the session reducer (only the event reducer
can approve a playback token, docs/spec/07) via
`speech.NewSessionFence(*session.Reducer)`.

Test evidence (all under `-race`): 20 rapid advances (generations 1..20,
each ready after seeded random 0–300 ms, 150 ms apart) → zero stale starts,
no overlapping playbacks, every interrupt stopped within 200 ms (dummy-sink
timestamps), 8 of 20 late-ready generations suppressed, and every started
playback evented with `speech.ended`.

### Virtual-sink evidence, acoustic NOT VERIFIED

`TestPipeWireNullSinkRoundTrip` (real PipeWire on this host):
`pactl load-module module-null-sink sink_name=gn_test_sink` (unloaded in
`t.Cleanup`; leftover modules swept on entry), a 1 s 440 Hz tone played
through `pipewire.Sink` while `pw-record --target=<monitor serial>
--rate=48000 --channels=1 --format=s16 -n 96000` records the sink's monitor
source; the recording is asserted non-silent (RMS 5792) and near 440 Hz via
a zero-crossing estimate (**measured 440 Hz**). This proves samples reached
the sink — the docs/spec/11 bar — and it is labeled **virtual sink
evidence**: no acoustic, hardware, or latency-to-ear claim. The acoustic
path (USB speaker, TV coexistence, calibration) is **NOT VERIFIED** until
the hardware smoke test (make test-hardware) with a microphone.

Environment quirks recorded for reproducibility:

- `pw-record --target` wants the monitor source **serial**, not its name
  (a name fails with "no target node available"); the test resolves the
  serial from `pactl list short sources`.
- This `pw-record` build exits 1 when it stops at `--sample-count` while
  still writing a complete, finalized WAV; the test therefore validates the
  file contents (frames + tone) rather than the exit status and logs it.
- `parecord` (pacat 17.0) in this build uses long options only
  (`--format=`, `--rate=`, `--channels=`) and has no `--length`.

## Consequences

- The scheduler is the single owner of device-level `speech.*` events; the
  session reducer's own TTS lifecycle events (`speech.requested`,
  `speech.ready`) must not be re-emitted at device scope when both are
  wired (wiring task, M5+).
- `ChunkSource.Next` may signal end-of-stream via `Chunk{Err: io.EOF}` **or**
  `io.EOF` as the error; the scheduler honors both (the former was the
  documented contract but the loop only checked the error — fixed here; a
  clean stream used to busy-loop the scheduler).
- Key-space evolution must bump `keyVersion`; the field set in the canonical
  JSON is part of the contract.
- Stop on a subprocess sink discards the device buffer rather than fading
  it; a sample-accurate fade-on-stop would require an in-process sink
  (PipeWire client library) — out of scope unless measured latency on
  hardware shows the click matters.

## Reversal plan

- Revert the package; the cache key is versioned, so old entries remain
  valid under `version: 1` and a v2 key space is additive.
- Swap sinks by implementing `AudioSink` (in-process PipeWire, ALSA direct,
  or capture) — the scheduler, fence and cache are sink-agnostic.
- The null-sink test is skipped (`NOT RUN: no pipewire sink`) wherever
  `pactl info` fails, so headless CI is unaffected; no module is left
  loaded (sweep on entry + unload in cleanup + DoD check
  `pactl list short modules | grep -c gn_test_sink` == 0).
