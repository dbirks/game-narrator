# 17 — Example wire contracts, data manifests and invariants

All snippets here are **schema sketches**, not claims of available runtime endpoints, extracted game data or completed code. Coding agent must formalize them in JSON Schema/OpenAPI and create golden tests before depending on them.

## Profile manifest

```json
{
  "schema_version": "1.0",
  "profile_id": "example:game-001",
  "edition": {"title":"Synthetic Adventure","platform":"gamecube-like","region":"US","language":"en","revision":"demo"},
  "sources": [{"source_id":"src-001","type":"user_file","sha256":"<64-hex>","rights":"user_supplied"}],
  "messages": [
    {"id":"demo-001","source":"src-001","original_text":"Press [BTN_A] to begin.","normalized_text":"press a to begin","speech_text":"Press A to begin.","kind":"tutorial","speaker_id":null,"scene_id":"start","pages":[{"index":0,"text":"Press [BTN_A] to begin."}]}
  ],
  "characters": [{"id":"narrator","name":"Narrator","voice_profile_id":"narrator-v1"}],
  "message_edges": [],
  "regions": [{"id":"bottom","kind":"textbox","rect":{"x":0.07,"y":0.72,"w":0.86,"h":0.23},"ocr":{"engine":"tesseract","psm":6,"language":"eng"}}],
  "publication": {"status":"draft","approved_at":null}
}
```

## Event envelope

```json
{
  "schema_version": 1,
  "event_id":"evt-1003",
  "session_id":"session-101",
  "seq":103,
  "trace_id":"trace-a9",
  "causation_id":"evt-1002",
  "occurred_at":"2026-10-16T12:30:10Z",
  "name":"decision.resolved",
  "data":{"observation_id":"obs-8","utterance_generation":12,"provider":"systemone","route_id":"decider-primary","model":"Cloudflare/clef-omni","questions":{"speaker":{"choice":"narrator","probabilities":{"narrator":0.91,"unknown":0.09}}},"accepted":true}
}
```

## Profile voice definition

```json
{
  "voice_profile_id":"voice-narrator-01",
  "revision":1,
  "state":"approved",
  "design_prompt":"Clear, warmly expressive original narrator, measured pace",
  "reference_asset_sha256":"<64-hex>",
  "reference_transcript":"A quiet evening settled over the village.",
  "base_model":"Qwen/Qwen3-TTS-12Hz-1.7B-Base",
  "registry_id":"gn_synthetic_narrator_v1",
  "style_policy":"neutral-with-optional-scene-variant",
  "seed":null,
  "consent_provenance":"original_synthetic_design",
  "registered_instances":[{"instance_id":"tts-base-1","status":"ready"}]
}
```

## Agent tool result

```json
{
  "schema_version":1,
  "tool_call_id":"tool-202",
  "name":"web_search",
  "status":"succeeded",
  "results":[{"title":"Example documentation","url":"https://example.org/docs","retrieved_at":"2026-10-16T12:00:00Z","source_id":"src-search-202","snippet":"Untrusted source content summary","confidence":null}],
  "error":null
}
```

## E2E scenario YAML

```yaml
schema_version: 1
id: synthetic/dialogue-advance-001
origin: generated_synthetic
profile: synthetic-adventure-v1
mode: frame_replay
input:
  frames_dir: tests/fixtures/synthetic/dialogue-advance-001/frames
  timestamps: timestamps.json
inference:
  decision: recorded
  tts: recorded
assertions:
  - type: message_detected
    message_id: demo-001
    max_delay_ms: 1500
  - type: audio_started
    voice_profile: narrator-v1
  - type: no_stale_audio_after_advance
    grace_ms: 200
reports:
  capture_frames: true
  capture_speech: true
  playwright_trace: retain-on-failure
```

## Engine state and idempotency

```text
SessionReducer.Apply(event):
  if event.session_id != active.id: discard
  if event.generation < active.generation: record(stale), discard
  if event.profile_revision != active.profile_revision: discard
  if event.seq <= active.applied_seq: dedupe
  validate transition from current state
  if new committed textbox/page: increment generation; cancel previous audio
  atomically write state+events and enqueue allowed side effects
```

## Confidence combination strategy

Produce a calibrated local score from retrieval and scene evidence; optionally fuse Clef probability through measured calibration rather than multiplying arbitrary raw probabilities. Decision policy might require `p(text_match)` and `p(speaker)` above separately tuned thresholds with a margin against runner-up, but actual constants must come from labeled holdout testing. Persist reason codes `MATCH_EXACT`, `GRAPH_NEIGHBOR`, `CLEF_DISAMBIGUATED`, `PREDICTIVE_PREFIX`, `UNKNOWN_SPEAKER`, `NO_NARRATABLE_TEXT`, `ABSTAINED`.

## Voice registration request (verified server supports this route, config-dependent)

```
POST /v1/audio/voices
Content-Type: multipart/form-data
fields: audio_sample=<approved reference wav>, name=<namespaced immutable ID>,
        consent=<truthful consent/rights record>, ref_text=<exact approved transcript>
```

Then:

```json
{"input":"An original test line.","voice":"gn_synthetic_narrator_v1","response_format":"wav"}
```

Target is a vLLM-Omni Qwen3 Base checkpoint, not a VoiceDesign checkpoint. Use `GET /v1/audio/voices` to verify registration. Do not assume one gateway registration covers all replicas.
