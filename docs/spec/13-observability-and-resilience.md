# 13 — Observability, diagnostics, performance and resilient operation

## Trace model

Correlate `session_id`, `occurrence_id`, `observation_id`, `generation_id`, `trace_id`, `agent_run_id`, `job_id`, provider `request_id`. A per-dialogue trace is a directed acyclic graph: capture frame → region classification → OCR attempt(s) → local match → Clef request/answer → speech plan → cache/TTS → ASR QA → audio playback → visible developer UI. Trace spans include monotonic timestamps, input/output hashes, model ID, route, latency, error and revision IDs, plus user-selected evidence retention.

## Metrics

Counters: captured/dropped/invalid frames, OCR attempts and fallbacks, speaker unknown, predictions/misfires, duplicate suppression, model calls/failures/retries, voice register reconciliations, ASR QA fails, cached audio hits, stale responses discarded, cancellations, playback underruns. Histograms: stage latency, first audible audio, ROI stability, candidate margin, speaker calibration by confidence bin, ASR WER, PCM queue lengths, RSS and CPU temperature. Gauges: active sessions, job queue, storage utilization, device attached, model provider circuit status. Aggregate by game/profile revisions but avoid high-cardinality trace IDs as metric labels.

## Logging/privacy

JSON structured logs. Default do not log full screenshots, source dialogue or waveforms; log hashes, counts and selected confidence. Developer trace sample mode may store frame crops/audio for debugging under time/size quotas and explicit toggle; redaction/masking of credentials and user identity. External model media requests are opt-in where outside trusted LAN. Provide `export support bundle` with automated scrubber and a preview before export.

## Diagnostics page and CLI

`game-narrator doctor` and `/diagnostics` should verify device connectivity, capture mode, small fresh frame, OCR on a controlled image, audio sink playback (with user consent), SQLite writable/migration status, asset read/write, AI route/model IDs, actual SystemOne typed question test, all three TTS checkpoints only if configured (design/base/custom), ASR transcript, voice registry list and self-reconcile. Report `OK`, `DEGRADED`, `BLOCKED`, `NOT CONFIGURED`; no generic success on unprobed service.

## Resilience matrix

| Fault | Expected app behavior | Operator action |
|---|---|---|
| Capture removed | Mark input offline, pause detection, preserve session metadata and reconnect with backoff | Reinsert card, rerun video test |
| Bad analog converter mode | Show frame/format and unsupported signal detail | Change converter/splitter/capture settings |
| OCR malformed/slow | Drop stale jobs, use cached prior context without repeated voice | Calibrate ROI and preprocessing |
| Decision/VLM down | Use strong local match, narrator/abstain; dev dashboard marks degraded | Repair endpoint or model |
| TTS down | Cached speech only, no blocking gameplay | Repair Base service, retries |
| Voice missing on replica | Reconcile or switch to known ready backend, never wrong voice | Test gateway routes |
| ASR down | Mark new clips unverified according to policy, queue QA later | Repair ASR |
| Asset store offline | Local cache used; new approved assets queued only if safe/durable | Reconnect storage |
| Disk full | Do not corrupt source DB; refuse new assets, evict safe transient cache | Expand/clean storage |
| Power outage | Recover transactions and jobs, stop orphan speech | Check doctor and backup |
| UI disconnected | Session continues headlessly, events buffered with limit | Reconnect |
| Agent source prompt injection | Reject instruction as untrusted data, log security event | Review import/source |

## Performance tuning procedure

Benchmark on Arch with real-model routes and on UNO Q with actual video. Replay a fixed corpus while varying FPS/OCR worker count/JPEG size/ROI settings and graphics capture mode. Inspect CPU/RSS/temp, streaming latency breakdown, p95 event age. Optimize with evidence (frame dedupe, crop sizes, request batching, cache locality), not premature rewrite in Rust. Never hide poor audio latency by shifting completion timestamps.

## Operational manuals required

First boot, safe firmware/dependency updates, storage migration/backup, inference gateway connection, voice registry maintenance, recovery after hardware disconnect, test environment reset, Dolphin profile reset, troubleshooting black frames, diagnosing wrong speaker, purging redacted support bundles, rollback after failed release.
