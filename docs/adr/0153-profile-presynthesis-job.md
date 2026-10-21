# ADR 0153: Pre-synthesize a profile's lines into the TTS cache

- Status: accepted
- Date: 2026-10-21
- Issue: #41

## Context

Live narration on the UNO Q has to start speech within about 500 ms of a settled dialogue box (the ADR-0143 gate: settled→audio p50 ≤ 500 ms, p95 ≤ 800 ms). A TTS cache miss costs 0.5–1 s, plus network time, on the shared Qwen3-TTS backend. The collective gateway also has outages several times a day; on 2026-10-21 there were 404/502/400 bursts at roughly 16:52, 17:01 and 17:13. A missed line spoken during an outage is silent or in the narrator voice.

Two mechanisms existed before this ADR:

- the per-session successor prewarm (ADR 0095), which synthesizes the dialogue graph's likely next lines during play;
- the A/V harness's own fixture pre-synthesis (test code).

Nothing let an operator fill the cache for a whole profile before play. The Wind Waker profile has about 4,300 messages and 29 voiced characters.

## Decision

1. `(*runtime.Manager).PresynthesizeProfile(ctx, profileID, opts, progress)` (`internal/runtime/presynth.go`):
   - It walks the profile's messages in profile order.
   - For each line it resolves the speaker's approved voice exactly as a live session does. The speaker, alias and canonical-text maps come from one helper, `profileSpeechMaps`, which `session.go` now uses too.
   - It synthesizes into the shared cache at `PriorityBackground`.
2. **One key identity.** `speechWiring.lineCacheKey` is the only function that computes a matched line's cache key. The live commit path, the successor prewarm and the pre-synthesis all call it, so a pre-synthesized line is a cache hit for the commit that speaks it. `TestPresynthOneThenCommitHitsTheCache` pins this. Merged catch-up runs keep their own `m1:` key (ADR 0122).
3. **Bounded and honest:**
   - concurrency defaults to 1 (max 4), because the TTS is shared with live play;
   - the run stops after 5 consecutive provider failures (a gateway outage), with the error kind;
   - a disk guard (`FreeBytes`; production uses `FreeBytesAt(dataDir)` via `statfs`) projects the remaining bytes from the first 20 syntheses and refuses to eat into a 500 MB reserve;
   - progress reports total, done, cache hits, synthesized, failed, skipped (no approved voice) and bytes.
4. **Idempotent, so resumable.** A cached line costs one lookup, so a restarted run continues where it stopped.
5. **Entry point.** `POST /api/v1/profiles/{id}/presynthesize` (curator, through the `/api/v1/profiles` wildcard grant; body `{limit?, concurrency? 0..4}`) queues one `speech.presynth_profile` job per profile. A second request while one is queued or running gets 409 naming that job. The handler (`internal/api/presynth.go`) reports progress as done/total and returns the final count as the job result. A gateway-outage stop is retryable (the rerun resumes from the cache), while a disk-budget refusal, an unknown profile or a bad payload is permanent. `cmd/game-narrator` registers the kind after the runtime exists and before the runner starts. Live sessions pause the job queue, as they do for every job. Start it with `curl -X POST -H 'Authorization: Bearer gn_…' http://<host>:8080/api/v1/profiles/<published id>/presynthesize -d '{}'`, then follow `/api/v1/jobs/<id>`.

## Options considered

- **Prewarm only (status quo).** It covers the graph's next lines, not a player who jumps between NPCs. It also needs the gateway at play time.
- **Synthesize at publish time, automatically.** This couples publishing to a long, gateway-dependent job, and spends TTS on profiles never played. The operator-started job gives the same result on demand.
- **Store compressed audio.** The cache stores WAV (pcm16). About 4,300 lines at roughly 100–150 KB each is 0.4–0.65 GB, which fits the board's free space. The disk guard covers smaller volumes. Compression is a separate cache-format decision.

## Tradeoffs

- A full Wind Waker pass is about 4,300 syntheses: roughly 40–70 minutes at concurrency 1 on the shared backend.
- Voices re-approved after a run change the voice fingerprint, so those lines need another pass. The idempotent rerun only synthesizes the changed ones.

## Validation

- `TestPresynthOneThenCommitHitsTheCache`: the first pass synthesizes, the second makes no provider call, and a live commit of the line is `from_cache: true` with no new provider request.
- `TestPresynthOneSkipsAVoicelessSpeaker`.
- `TestPresynthesizeRouteQueuesOnePerProfile` (202 with the payload, 409 on a second request, 404, 403 for a viewer, 400 for concurrency 9) and `TestPresynthJobHandlerOutcomes` (progress percentage and result; disk budget, unknown profile and bad payload are permanent; a gateway outage is retryable).
- The whole `internal/runtime` suite passes under `-race`, which includes the commit and prewarm paths that now share `lineCacheKey`.
- Live: run the job on the board's published profile, then confirm the TTS cache-hit rate in the A/V harness and on Traces.

## Reversal

Remove `presynth.go` and the job route. `lineCacheKey` and `profileSpeechMaps` are behaviour-preserving refactors and can stay.
