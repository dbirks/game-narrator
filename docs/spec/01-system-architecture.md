# 01 — System architecture, ownership, lifecycle

## Topology

```text
GameCube -> analog-to-HDMI -> powered HDMI splitter ───> TV (direct path)
                                                └──────> UVC USB capture card
                                                          |
                                         Powered USB-C hub / UNO Q Debian ARM64
                                                          |
       browser <── LAN HTTPS ──> Go API + embedded React SPA + job workers + SQLite
                                                          |
   frame adapter -> text-state detector -> ROI preprocessing -> OCR -> candidate matcher
                                                          |                 |
                                                    dialogue graph       profile DB
                                                          |
                                      adjudicator/policy -> cache -> speech scheduler
                                          |      |           |         |
                                          |      |     content store   USB ALSA/PipeWire
                                  Clef/SystemOne  VLM fallback
                                          |                 
                                gateway / named-capability model routing
                                          |
                          VoiceDesign | Base TTS | Qwen3 ASR | fast LLM
```

One program can serve API, jobs and capture worker on UNO Q, but each component must have a testable interface. GPU inference remains remote. A server-side asset backend can use an attached disk, network volume or object store; UNO Q has a bounded local spool/cache. Avoid assuming the network is present at boot.

## Process domains and proposed Go packages

```
cmd/game-narrator/{main.go}
internal/bootstrap    # config validation, startup, graceful shutdown
internal/api          # auth, REST, SSE, browser static SPA
internal/session      # playback lifecycle, state machine, event log
internal/capture      # V4L2, ffmpeg, image replayer, Dolphin-dev capture
internal/vision       # ROI, change detection, crop transforms, OCR
internal/matcher      # normalized text candidates, graph, confidence
internal/decision     # typed choice bundle, abstain policy
internal/providers    # capability registry, gateway clients, retries
internal/speech       # voices, ASR, synthesis, cache, audio output
internal/onboarding   # agent loop, allowlisted tools, approvals
internal/games        # manifests, imports, adapters, revisions
internal/jobs         # durable workflow queue and checkpoints
internal/assets       # SHA256 blobs, staged writes, GC, exports
internal/store        # SQLite migrations, transactional repositories
internal/devtools     # frame/event/artifact inspection, fixture recording
internal/telemetry    # structured events, metrics, tracing
web/                  # React / TypeScript / Vite source and assets
internal/webassets/   # generated Vite dist copied for go:embed
```

Use Go interface contracts for `FrameSource`, `TextExtractor`, `CandidateRetriever`, `DecisionClient`, `SpeechSynthesizer`, `ASRClient`, `AudioSink`, `AssetStore`, `SearchProvider`, `GameImporter`, `GameStateOracle` (dev-only). No circular imports; services receive dependencies in constructors; error taxonomy explicit.

## High-level events (single source of truth)

- `frame.observed`: session sequence, source timestamp, monotonic capture time, dimensions, frame hash, preview reference.
- `region.changed`: ROI ID, classification, bounding box, change magnitude, stable time.
- `text.observed`: extraction hypotheses, raw glyph tokens/word boxes, crop hash, OCR confidence, page state.
- `message.candidates`: ranked matches with component scores and source IDs.
- `decision.requested` / `decision.resolved`: provider route, context digest, question IDs, results, latency, abstention status.
- `message.committed`: detected dialogue occurrence ID, canonical message ID (or unknown), speech eligibility, speaker outcome, text variant.
- `speech.requested` / `speech.ready` / `speech.started` / `speech.cancelled` / `speech.ended`: utterance, voice revision, cache key, timings, result.
- `session.scene_changed`, `provider.degraded`, `agent.job.progress`, `agent.review_required`, `system.health_changed`.

Every event has `event_id`, `session_id`, monotonic `seq`, `trace_id`, `timestamp`, `origin` and `schema_version`; derivations refer to `causation_id`. Persist bounded metadata and sampled image/audio references according to privacy configuration.

## Parallelism and deadline contracts

- One capture loop: bounded single-slot latest-frame queue for live runtime, unlike lossless recording.
- ROI worker pool (one or two on UNO Q, configurable); OCR task dedupe by image/region hash; cancellation on newer observation.
- One session event reducer serializes state changes and owns active utterance generation counter. Models never mutate session state directly.
- Candidate lookup is local and deadline constrained; decision/VLM calls have independent timeout, circuit breaker and returned-generation check.
- Speech generation producer may run asynchronously; only currently committed generation can play. Prefetch tasks lower priority than urgent current speech.
- SSE fanout ring buffer is bounded; reconnect via Last-Event-ID or persisted cursor; slow browsers cannot stall capture.
- SQLite in WAL mode with transaction limits and busy timeout; no long network operations inside database transactions.

## Runtime and setup isolation

The interactive runtime has a CPU/network/storage resource reservation; background research and bulk pre-generation are rate limited or paused while a live session needs resources. Agent and web-scrape content has lower trust than profile/corrected user data. Only published profile revisions are used for runtime; setup drafts can be edited without affecting an active session. Atomic publish emits `profile.published` and transitions sessions only at safe boundary or user confirmation.

## Local / remote ownership

- **UNO Q**: capture, local text state/OCR, metadata DB and ephemeral live session, basic cache, web application, USB audio.
- **Inference gateway**: routes capabilities to GPU services; opaque endpoint details; no assumption of sticky routing.
- **Canonical asset store**: game profiles, voice reference clips, approved/generated utterances, original import manifests, snapshots; may be local, remote or mirrored.
- **Arch developer machine**: emulator driver, fixtures, full CI-equivalent suite, temporary graphical display, inspection of generated reports.

## Key invariants for code review

1. Every frame has explicit `source_clock` and `capture_timestamp_ns`, not a wall-clock-only label.
2. `commit_id` and `utterance_generation` fence stale asynchronous work.
3. A screenshot is never sent to an unconfigured external host implicitly. Local-only is default.
4. Cache identity includes: message text normalized for TTS, game/profile revision, voice immutable revision, TTS engine/model digest, synthesis settings, language, optional style variant and policy version.
5. An agent may suggest an edit but only a domain service performs a validated transaction.
6. Import provenance is immutable after source registration; corrections are overlay patches.
7. Production never executes Dolphin controller actions; developer harness is a separate executable/module with explicit opt-in and test permissions.
