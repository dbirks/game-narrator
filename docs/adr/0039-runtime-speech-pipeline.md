# ADR-0039: Runtime speech pipeline — commit-driven composition, voice-resolution policy, sink selection, live audio controls

Date: 2026-10-17
Status: accepted
Owners: agent (m4-speech-runtime)
Related requirements/tests: docs/spec/07 "Audio scheduler", "Caching", "Playback", docs/spec/01
invariant 2 (commit_id/utterance_generation fence stale async work) and the high-level event list,
docs/spec/12 "Zero stale audio starts", docs/spec/13 degraded modes ("did it speak, and why not"),
docs/spec/16 live-test categories; ADR-0022 (TTS client + registry), ADR-0023 (speech cache key +
generation-fenced scheduler), ADR-0030 (voice workflow: approved revisions, per-instance
registrations). Tests: `TestSpeech*` (internal/runtime, deterministic fake TTS),
`TestLiveSpeechSession` (internal/runtime, real TTS, NOT RUN without the gateway env),
`TestSessionAudio*` (internal/api), `web/e2e` play-page audio controls.

## Context and constraints

Before this change a runtime session recognised and committed lines but never spoke: the speech
package (cache, scheduler, fence, sinks) was only exercised by the scenario harness's sidecar.
The runtime now owns the per-session audio lifecycle end to end.

Constraints that shaped the decision:

- The session reducer (internal/session) owns the text lifecycle and the commit generation; the
  scheduler (internal/speech) is the single owner of device-level playback events (ADR-0023).
  The runtime must compose, not reimplement, both.
- docs/spec/01 invariant 2: stale asynchronous audio work is fenced by the commit identity —
  a newer committed generation, or the textbox advance that produced it, cancels/blocks older
  audio. "Zero stale audio starts" (docs/spec/12) is enforced in the session reducer's phase
  table and the scheduler's fence, not in a UI filter.
- The voice workflow (ADR-0030) already resolves the approved revision and the per-instance
  registration; the runtime must consume that state, never design or register voices itself.
- docs/spec/13: a missing voice or sink must be reported as degraded status (503 /
  `result:"skipped"`), never papered over with a substitute.
- The frame-replay gate (docs/spec/12) requires the *frame* decision to stay a pure function of
  the frames and the engine — replay pacing at Speed 0 (as fast as the consumer takes).

## Decision

### Composition: the publisher chain watches its own commits

`Manager.Options.Speech` wires one voice pipeline per process (the voice service, the asset
store for reference fallback, the sink selection). At session start, when the option is set,
the session's publisher chain becomes `commitWatcher` → `tracePublisher`: the watcher intercepts
each `message.committed` envelope on the session's own scope and hands it to the session's
`speechWiring`, which then owns the audio lifecycle. A nil option runs silent sessions and the
audio API answers 503 `audio_unavailable` — the documented degraded mode, not an error at start.

Per committed line the wiring: resolves the voice (below), builds the cache key
(`speech.CacheKeyOf` over `KeyInput`: utterance text, message id, speech-rules revision,
language, voice-revision fingerprint, TTS model id, backend digest, format — the key already
carries the text version, the voice version and the rendering-rules revision), runs
`Cache.GetOrGenerate` on the voice service's shared TTS cache over the real `voice_clone`
client (one 120 s deadline per utterance, the role's documented floor), applies the operator's
fenced delay and mute state, and submits the WAV to the session's
generation-fenced `Scheduler`, whose publisher is the wiring's `speechEventPublisher` (it
correlates the scheduler's minimal payloads with the committing frame's trace id and the
message/occurrence/region/voice ids before they hit the hub).

The fence advances on **every** commit — an ineligible box still means the player moved on —
via `fenceAndCancel`, which also asks the scheduler to stop the superseded generation. A sink
that cannot be built degrades the session to silent (logged); a voice that cannot be resolved
ends the utterance `result:"skipped"` with the reason. Sessions end with their audio:
`run` stops the wiring before the terminal events go out, and the scheduler's stop is bounded.

### Voice resolution: approved voice, never a substitute

The speaker is the line's assigned character; an unattributed line is the narrator's. The
speaker character's approved revision (with its ready registration, or the approved reference
carried inline when the voice is not registered on this instance, docs/spec/06) is the only
legitimate voice. A character without an approved voice falls back to the narrator voice when
one is configured — never to a different character's voice, never to a builtin. A specific
fault on the primary voice (missing approved reference) is reported, not papered over by the
fallback. The TTS request carries the registration's name (or the inline reference +
transcript), so the wire identity is exactly the approved one.

### Sink selection: config-driven, preflight-checked

`SpeechOptions.Sink` selects `dummy` (test default), `filesink` (one WAV per started playback
under `<FileSinkDir>/<session id>`, named by generation — the live test's evidence) or
`pipewire` (only when configured and the PipeWire device is present; the doctor preflight
reports the device, the sink's `Play` error degrades that utterance to `result:"failed"`).
Volume is applied once, in the `gainSink` wrapper between the scheduler (which always requests
unity gain) and the device sink, so no sink can double-apply it.

### Audio controls: partial, live, fenced

`POST /api/v1/sessions/{id}/audio` takes a partial body (`muted?`, `volume?` 0..1,
`speech_delay_ms?` 0..2000); a field left out keeps its value. The manager validates the
ranges (400 `invalid_audio_settings`), applies the change to the session's wiring, and answers
with the fresh session snapshot (`audio` carries the sink, the applied controls, the speech
counters requested/ready/started/cancelled/ended/cache_hits and the current utterance). Mute
skips playback but still resolves the voice and fills the cache; the utterance ends
`result:"muted"`. The delay waits before start and is still fence-checked: a commit during the
wait moves the fence and the scheduler's before-start check drops the waiting utterance. Volume
scales the delivered samples at the sink boundary. A session without a voice pipeline answers
503 — the control was not applied to nothing.

### Pacing seam: `Options.ReplaySpeed` stays 0 by default

`buildFrameSource` now takes the manager's `ReplaySpeed` (default 0: drain as fast as the
consumer takes — the documented Frame-replay-gate behaviour, which the unit tests keep using
with the fake TTS). Only the live session test sets 1.0, because its evidence is the audio
half, which the wall clock owns: at Speed 0 the clip drains in milliseconds and every real TTS
response (~0.7–0.9 s) arrives after the fence has moved, so no playback can start. The live
test additionally re-stamps its copy of the clip at 5× the recorded source cadence so a
textbox's dwell (3.0 s / 2.0 s) exceeds the measured round trip; the frame-replay gate is
unaffected — it runs the original clip at Speed 0, where the decision is a pure function of the
frames.

### Payload naming: `generation` stays

docs/spec/08 names the speech events but not their payload fields; the system-wide payloads
(`message.committed`, `decision.resolved`, `session.stopped`, the speech events) all carry the
commit generation as `generation`, and the speech payloads additionally carry `voice_revision`
and `dialogue_occurrence_id`, which removes the only ambiguity a bare `generation` could have
there (the value is the commit generation that fences the utterance — docs/spec/01's
`utterance_generation` concept, already identical in value). Renaming one emitter in this
package while the other four live in read-only packages (internal/session, internal/speech)
would fork the wire shape; the field stays `generation`.

## Options considered

1. **Sidecar in the runtime (rejected):** a speech goroutine polling the event log for commits
   would add a second reader on the session's events and race the pipeline's own publication;
   the publisher chain is in-band and ordered with the commits it reacts to.
2. **Voice resolution in the reducer (rejected):** the reducer is text-scoped and read-only for
   this milestone; it already emits the eligibility answer, which the wiring consumes.
3. **Prewarming the TTS cache for likely successors (out of scope):** docs/spec/07's prewarm
   targets *likely successor* lines — a separate feature that must not mask the current line's
   real latency (the live test measures it on purpose).
4. **Per-utterance `utterance_generation` rename (rejected):** see the payload-naming decision
   above.

## Positive consequences

- One active session speaks with the speaker's approved voice; every speech event carries the
  session id, the committing generation and the committing frame's trace id (frame → commit →
  speech correlation in the developer UI).
- Stale audio is fenced at the source (reducer phase table + scheduler fence), verified with
  real timing: the live test's second line interrupts the first mid-playback and the filesink
  evidence carries the delivered prefix.
- "Did it speak, and why not" is answerable from the API snapshot (counters + current
  utterance) and the event log.

## Negative consequences / risks

- The session's publisher chain gains one interceptor; a misbehaving wiring would delay
  publication. Mitigation: the watcher never blocks (it copies the envelope and returns the
  inner publisher's answer), and the wiring's work runs on bounded worker slots.
- Live pacing (Speed 1.0) makes the live test wall-clock-dependent by design; its assertions
  are floors (first line ends + real WAV bytes), not exact-timing claims, and the test logs the
  dwell-vs-round-trip comparison it measured.

## How to verify (commands and evidence)

```
go test -race -count=1 ./internal/runtime/... ./internal/api/...   # deterministic unit tests
go test -count=1 -v -run Live ./internal/runtime/                  # real TTS (NOT RUN without env)
```

Evidence from the live run (2026-10-17, two runs): line "a" dwell 3000 ms (clip paced
5× the recorded cadence), real TTS round trip 859–895 ms, `speech.started` then
`speech.ended {result:"ended", reason:"interrupted"}` (line b's commit interrupted the
playback); filesink WAV `gen1-…-1.wav`, 128 204–128 684 bytes, 24 kHz, ~2.7 s delivered, of
real TTS audio; line "b" dwell 2000 ms, round trip 658–776 ms.

## Rollback/migration path

Drop `Options.Speech` (silent sessions) and the `/audio` route; the event payload is unchanged,
so no migration. The `ReplaySpeed` field defaults to 0 and can be deleted with the live test.
No schema change.

## Links/references

- internal/runtime/speech.go (wiring, controls, gain sink), internal/runtime/manager.go
  (Options.Speech, ReplaySpeed, SetAudio), internal/api/sessions.go (audio route + problem
  mapping), internal/speech (cache/scheduler/sinks, read-only), internal/voices (read-only),
  docs/spec/07, docs/spec/12, docs/spec/13, docs/spec/16.
