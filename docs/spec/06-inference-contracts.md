# 06 — Inference provider contracts and routing

## Provider registry

Configure model roles, not scattered raw URLs:

```
agent_reasoning       → OpenAI-compatible chat tool/function calls
fast_reasoning        → OpenAI-compatible chat or responses, tested capability
vision_ocr_fallback   → image-capable multimodal chat, tested payload constraints
decision              → System One `/v1/systemone` (Clef-Omni)
voice_design          → vLLM-Omni VoiceDesign checkpoint
voice_clone           → vLLM-Omni Base checkpoint
speech_asr            → Qwen3-ASR compatible transcription endpoint
```

A gateway may route to several machines. Declare named logical deployment IDs and physical instance IDs where necessary; provider discovery can query gateway administrative API if available, otherwise configure per-capability dedicated endpoints. Do not assume one OpenAI base URL supports all modalities, all checkpoints, or that API feature names match. Preflight probes actual deployed behavior, timeouts, outputs and auth.

## Interface requirements

`DecisionProvider.Decide(ctx, evidence, questions) -> decisions` with probabilities/latency/model and request provenance.  
`VisionProvider.Extract/Describe(ctx, crops, context, schema) -> structured hypothesis`.  
`AgentProvider.Next(ctx, messages, allowedTools, responseSchema) -> toolCall|answer`.  
`TTSProvider.Design/Clone/Register/List/Remove/Generate/Stream`.  
`ASRProvider.Transcribe(ctx, audio, language) -> transcript + timestamps/confidence when supported`.  
`ModelRouter.Resolve(role, affinity, capabilities) -> target, route_id, health_state`.

## System One / Clef-Omni protocol

As of research date, vLLM documents `/v1/systemone` with `state`, `questions` object; choice questions map option IDs to descriptions; `noul` is yes/no, `score` ordered levels. Clef-Omni is a joint-schema pooling model, not chat text generation. It may accept `images`, `audio`, `videos` and return answers in a single model forward pass. Do not pass generic top-level `instructions` to Clef-Omni; vLLM rejects it for this checkpoint. Its scoring outputs are signals requiring policy calibration, not reliability guarantees. vLLM limits can differ from third-party gateways; probe negotiated limits rather than assume upstream limits.

Example shape (illustrative, needs real endpoint contract test):

```json
{
  "model": "Cloudflare/clef-omni",
  "state": {"scene": "outset", "ocr": "Who's there?", "last_message": "m-11"},
  "images": ["data:image/jpeg;base64,REDACTED"],
  "questions": {
    "kind": {"type":"choice","instructions":"Classify visible text","criteria":{"dialogue":"character speaks","sign":"interacted sign","none":"no narratable text"}},
    "speaker": {"type":"choice","instructions":"Who is speaking?","criteria":{"char_a":"visible character A","narrator":"world/instruction text","unknown":"cannot establish"}}
  }
}
```

Response parser validates declared IDs, finite probability [0,1], complete choice sets and schema version; rejects unknown answer types and model mismatch. Capture normalized candidate texts (not the entire game corpus). Rate-limit consecutive doubtful decisions to avoid hammering GPU due to one unreadable textbox. Batch compatible questions per observation; never ask endlessly until the model chooses a desired answer.

## Vision fallback

Inputs: ROI crop first, full scene screenshot if scene inference needs it, OCR alternatives, edition/language, top 5 candidates and recent scene context. Outputs with JSON schema: `visible_text`, `text_complete`, `bounding_boxes`, `speaker_candidates`, `scene_hint`, `reason`, `uncertainty`, `model_version`. Validate UTF-8, length and boxed coordinate ranges. Do not let the VLM replace known canonical game script based on its own prior knowledge. Data retention default off for remote third parties; URL/IP validation and explicit user consent if a non-LAN model is chosen.

## Model-serving version and health negotiation

Maintain `GET /health` or documented model-status endpoints plus capability probe suite for:

- chat/tool calling correctness and JSON-schema output;
- decision endpoint accepts text plus one image and returns valid choices;
- VoiceDesign endpoint synthesizes WAV, sample rate and duration valid;
- Base registry lists, registers and reuses an approved synthetic voice (with consent token); restart behavior or reconciliation tested;
- ASR returns a transcript with language identification or specified language;
- streaming supported vs buffered modes; seed field support negotiated (never rely on it for timbre identity).

Persist `model_id`, backend name/version, base URL logical label, real instance ID if exposed, request digest, latency and sampling settings. Redact credentials and media by default. Record a capability fingerprint for cache/version invalidation.

## Routing, reliability and replicas

- Role-based route table and circuit breakers. Retry idempotent inference calls with bounded exponential backoff only if event still current. Do not blindly retry voice registration without idempotency and fingerprint check.
- Persistent voice source of truth resides in app-owned asset store. `VoiceRegistryManager` compares provider instance's `GET /v1/audio/voices` against required voice revisions, registers missing profiles, verifies sample identity where possible. If gateway hides replica identity, route cloned TTS through a **sticky, dedicated Base endpoint** or deploy precomputed profiles across all replicas; test routing through every instance. A global gateway alias without instance control is insufficient for correctness.
- `VLLM_OMNI_SPEAKER_REGISTRATION_POLICY=immutable` is recommended on inference replicas if supported; never rely on friendly voice names as unique across tenants. Use namespaces based on account/profile/revision digest.
- If registration is unavailable (`--api-server-count > 1` or gateway strips multipart endpoints), fall back to explicit `ref_audio` or server-start loaded `custom_voice_dir`. This fallback is negotiated and reported in preflight.
- Background generation receives lower priority than runtime. Apply per-role concurrency, max payload bytes and bandwidth caps, and cache common outputs.
- OpenAI-compatible endpoints are only adapters for supported capabilities; Qwen3-ASR exact routes must be probed/configurable rather than assumed.

## Timeout and cancellation defaults (to calibrate)

- Local OCR: target 150–400 ms; abandon if superseded.
- Clef decision: timeout 1500 ms initially (LAN), may be calibrated lower; it can be skipped rather than delay speech if strong local match.
- VLM fallback: 3–10 s; late responses annotate next observation only if still applicable.
- On-demand TTS: request timeout 30–120 s, but playback generation fenced/cancellable; enqueue bounded.
- Agent browsing/tool tasks: 15–30 s per search/fetch, bounded batch budgets and resumable jobs.

## Failure taxonomy

`timeout`, `unavailable`, `rate_limited`, `bad_schema`, `auth`, `payload_too_large`, `unsupported_capability`, `wrong_checkpoint`, `voice_missing`, `replica_mismatch`, `decode_error`, `audio_invalid`, `content_refused`, `cancelled`, `stale_result`.

Retries must distinguish transient network failure vs permanent schema/policy failure. Surfaced in preflight UI with command/action to repair. Service can run cached-only while models down.
