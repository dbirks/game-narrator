# 07 — Voice lifecycle, TTS caching, ASR quality and audio output

## Voice lifecycle

1. Extract representative character dialogue and context (verified source references, language and role).
2. Generate editable `VoiceBrief` with base temperament, vocal timbre, cadence and articulation, plus separate optional delivery styles.
3. Call Qwen3-TTS **VoiceDesign** checkpoint for candidate reference WAVs; log prompts, model revision and optional seed/sampling where supported.
4. Store exact reference transcript and reference WAV in Game Narrator's canonical asset store; audition in UI with user feedback.
5. Approve an immutable `VoiceProfileRevision`. Attach `consent_provenance` and original-design status.
6. Reconcile that revision to vLLM-Omni **Base** checkpoint as registered voice (or supply inline reference when unavailable); use versioned registry ID.
7. Generate utterances for known dialogue on demand, optionally prewarm successor nodes; ASR-check expected text; record quality state and cache by immutable key.
8. On revision change, old cache remains attributed to old voice; new work uses new revision. Admin may retire and GC unreferenced assets after retention window.

**Qwen3-TTS details verified:** vLLM-Omni supports VoiceDesign, Base and CustomVoice as separate checkpoints. The `/v1/audio/voices` upload uses multipart sample, name, optional exact transcript, consent. Uploaded voices persist to files; `VLLM_OMNI_SPEAKER_REGISTRATION_POLICY=immutable` rejects re-upload collisions. Upload/delete may require one API frontend. Voice registry is not per-user protected by default. See sources. The inference route must enforce rights and tenant namespace. An uploaded voice is a Base voice, not CustomVoice.

## Identity vs emotion vs random seed

`Identity` = approved reference clip / voice prompt / model checkpoint; `delivery` = optional per-scene/line emotion style; `seed` = stochastic generation control where supported. Persist all with result metadata, but **seed does not define or guarantee a voice**. Check backend whether HTTP `/v1/audio/speech` actually accepts and honors a seed; documented seed support in some WebSocket/session configurations does not automatically establish support for all HTTP modes. If unsupported, mark not reproducible and use immutable WAVs for reproducible playback.

Clone/VoiceDesign speaker conditioning is not identical to CustomVoice's instruction following. Do not assume arbitrary per-line instruction is honored by Base. Achieve style variation through selection of pre-approved delivery variants, limited punctuation/speech-text edits, or separate auditioned references if necessary. Never accidentally put previous scene context into the `input` field to be spoken aloud.

## Canonical speech rendering

- Input: canonical message AST + actual visible text + variable substitutions + locale, resulting in `SpeechText` and `SpeechPlan`.
- Nonspoken tokens: portrait tags, colors, debug markup and UI animations removed (with provenance).
- Button icons: translate according to edition/device mapping (e.g. “press A”), avoid repetitions and wrong buttons. Pronunciations: phoneme/spelling overrides in app domain; no unexplained output from TTS.
- Treat scene context as optional delivery metadata only; selected speech text is the only material passed as utterance input.
- Segment at semantically complete sentence boundaries; split long lines with punctuation and max token length; preserving listener comprehension matters more than optimizing token count. Do not pre-speak unrevealed branches.

## Cache

Cache key: SHA256 of canonical JSON `(utterance_text, original_message_id, speech_rules_revision, language, voice_revision_fingerprint, tts_model_id+version, backend_config, style_variant, supported_seed+sampling, format)`. The cache holds WAV/PCM with duration, sample rate, QA and failure state. Avoid duplicate synthesis via singleflight per cache key, bounded concurrent generation and atomic asset commits. Skip QC-failed cache entries by default unless explicit user override.

Prioritize `current committed` > `likely next` > `batch background` and obey inference server load. Prewarm top weighted successor messages but cap by scenes/byte quota, graph entropy and resource budget. Export/import voice cache manifests only with permitted assets.

## ASR loop

For generated audio: Qwen3-ASR transcription + normalization against requested speech. Metrics: CER, WER, missing names, number/button pronunciation, repeated segments, unexpected extra text, duration outliers and silent audio. Thresholds initially advisory and language dependent; tune on approved samples and real game clips. For high-risk failures trigger regenerate with alternative seed/settings and bounded attempts; present unresolved issues for review rather than infinite retries. ASR cannot fully verify emotional delivery, voice identity or naturalness; retain human audition and sample-based timbre consistency checks.

## Audio scheduler

Input `UtteranceReady` with session/generation token, cancel deadline, expected text duration, voice ID, priority. Output `start`, `progress`, `end`, `interrupted`, `error`. Only event reducer can approve playback token; if new dialogue or session disconnect occurs, fade and drop queued chunks. Use USB ALSA/PipeWire sink by stable device identity; audio sink abstraction also includes dummy/file/capture for CI and tests.

Prefer buffered WAV for cached utterances; support chunked playback for valid streaming TTS while fencing each chunk. When a source is fully committed, deliver samples in order with bounded buffer and underrun stats. Apply volume, short fade, optional delay calibration and output format conversion. Audio sink must never block session event loop. Bluetooth is optional, not baseline.

## Audio coexistence and acoustics

Physical TV carries original game audio; voice plays through separate USB speakers. Later audio mixing via external extractor/mixer is optional. Account for TV delay, USB DAC buffer and room acoustics. Include a test tone, calibration page, per-voice gain, ducking only if compatible audio source can actually be controlled (don't promise software ducking of analog TV output).

## Failure behavior and test coverage

- Inference offline: use approved cached clips; no indefinite spinner or blocking gameplay.
- Voice ID unknown on replica: reconcile/fallback within deadline; don't silently change to a wrong built-in voice.
- VoiceDesign returns malformed/noisy audio: quarantine audition, do not approve.
- ASR transcript diverges: retry bounded times, retain diagnostic evidence, mark review.
- Player advances mid-stream: audible playback stops/fades even if TTS backend ignores cancellation; late audio never plays.
- Character speaker uncertain: configurable narrator or silence. Confirmed signs/items use narrator.
- Back-to-back same text events with new occurrence ID can be spoken twice when appropriate; repeated OCR of one occurrence should not duplicate.
