# ADR-0022: TTS client for the voice_design and voice_clone roles — VoiceDesign/Base over vLLM-Omni speech, voice registry, consent, and WAV invariants
Date: 2026-07-21
Status: accepted
Owners: project
Related requirements/tests: docs/spec/06 "Provider registry" (voice_design, voice_clone), "Failure taxonomy"; docs/spec/07 "Voice and audio pipeline" (VoiceDesign + Base, registry, naming, consent); docs/spec/05 "Audio output" (safe output, 24 kHz→48 kHz); docs/spec/17 "Event envelope" (`voice.registered`, `voice.removed`, `voice.ensure_failed`); docs/spec/14 "Consent ledger"; builds on ADR-0005 (audio pipeline and cache keys), ADR-0009 (provider roles, taxonomy, recorded cassettes), ADR-0021 (per-role adapter style)

## Context and constraints

- The `voice_design` and `voice_clone` roles (docs/spec/06) had adapters
  registered and env plumbing complete, but no client: `doctor` reported
  configured voice roles as DEGRADED "client not implemented yet (M3/M4)",
  and nothing in the runtime could synthesise a line or reconcile the voice
  registry.
- vLLM-Omni speech is **not OpenAI chat**. The contract was verified live on
  2026-07-21:
  - **verified** against the collective gateway (`GAME_NARRATOR_GATEWAY_URL`):
    `POST /audio/speech` with `task_type: "VoiceDesign"` + `instructions`
    returns 200 and a real WAV; without `task_type` the same payload is a 400
    `does not support task_type='CustomVoice'` — the gateway proxies the
    native speech route, it does not normalise it. `seed` was accepted.
    `POST /audio/voices` on the gateway is 404: the registry is not proxied.
  - **verified** against the direct Base service (`GAME_NARRATOR_TTS_BASE_DIRECT_URL`):
    `GET /v1/audio/voices` lists `default` plus registered voices; a
    registered voice synthesises on `POST /v1/audio/speech` **without** any
    `task_type`; the built-in `default` does not — it 400s with
    `task_type='CustomVoice' is not supported` and `Base requires ref_audio
    or speaker_embedding`; sending `ref_audio` + `ref_text` +
    `task_type: "Base"` (inline reference) synthesises 200. So the Base
    checkpoint serves registered-voice TTS, inline-reference TTS, and the
    registry, but not built-in-voice TTS.
  - **spec-only** (docs/spec/07, not yet served by the current checkpoint
    and therefore not relied on by this client): `voice_embedding`
    round-trip, `speaker_embedding` (768-d), and `/v1/audio/voices`
    embedding management for the *registered* voice path.
- The registry (docs/spec/07 "Registry endpoints (Base service)") is on the
  Base service directly, never behind a collective gateway. Reconciliation
  and registration therefore need the direct Base URL, which a production
  deployment exposes via an orchestrator port-forward; live tests use
  `GAME_NARRATOR_TTS_BASE_DIRECT_URL`.
- docs/spec/14 is binding: no voice may be registered without an explicit
  consent flag, no voice may be re-registered under a name whose stored
  consent is refused, and every registration/removal must leave an
  auditable record. docs/spec/17 fixes the event names (`voice.registered`,
  `voice.removed`, `voice.ensure_failed`).
- docs/spec/05 makes the WAV itself a trust boundary: sample rate 8k–96k,
  duration 0<d≤120 s, non-silent (RMS), <1% clipped samples, PCM 16/24/32,
  1–2 channels. A "successful" HTTP response with a corrupt WAV must be
  treated as `audio_invalid`, never played.

## Decision

- New package `internal/providers/tts` implements both roles with one
  client, `tts.Client`:
  - `Design(ctx, DesignRequest{Text, Instructions, Language, Seed})` →
    `POST /audio/speech` with `task_type: "VoiceDesign"`; `instructions`
    only when non-empty; `seed` only when ≥ 0. The `Seed` field is
    **documented as metadata only**: docs/spec/07 says "record the seed as
    metadata", and the verified contract shows the server accepting it
    without any reproducibility guarantee — the field exists so the seed
    can be *recorded* into `voice.registered`, not to promise
    determinism.
  - `Generate(ctx, GenerateRequest{Text, Voice, Language, Speed})` →
    `POST /audio/speech` with `voice` and optional `language`/`speed`; **no**
    `task_type` (verified: registered voices must not send one — it 400s).
  - `Audio{WAV, SampleRate, Channels, BitsPerSample, Duration, Model,
    Latency, RequestID, Origin}`. `Origin` distinguishes the recorded
    fixture origin from a live server; `RequestID` is derived from
    `x-request-id` when present, else SHA-256 of the WAV (docs/spec/05 "all
    events carry … request IDs").
- **WAV validation** (`ValidateAudio`) after every synthesis: RIFF/WAVE
  magic, `fmt ` chunk with PCM (code 1), 1–2 channels, 16/24/32 bits, 8k–96k
  Hz, `data` chunk size consistent with sample rate and 0<d≤120 s, RMS
  above a −90 dBFS floor (not all-silent), and <1% samples within 0.5% of
  full scale (not clipped). Every failure is `audio_invalid`. The
  committed cassette WAV (a deterministic 24 kHz sine) is validated by the
  same code the live path uses, and the recorded tests assert the
  parser+validator accept it while a corruption battery rejects each
  invariant individually.
- **Error mapping** (docs/spec/06 taxonomy): 401/403 → `auth`, 429/503 →
  `rate_limited`, 4xx → `bad_schema` (with the server's message), 5xx →
  `provider_error`, transport/timeout → `timeout`, empty body after 200 →
  `audio_invalid`. Error strings carry the taxonomy kind and the server
  message only — never the bearer token or the base URL.
- **Retry with negotiation.** Both calls retry exactly once with the
  offending field dropped when the 400 names that field: `task_type='Base'
  is not supported` → retry without `task_type: Base` (and stop sending it
  thereafter, sticky); `unknown field: seed`/`does not support 'seed'` →
  retry without `seed` and record `seed_unsupported` on the result
  metadata. Negotiated facts are process-lifetime (the endpoint does not
  change), per the ADR-0021 precedent for server-shaped contracts.
- **Voice registry** (`tts.Registry`, docs/spec/07, on the direct Base
  service): `List`, `Register(ctx, Voice{Name, RefAudio, RefText, Consent})`
  as multipart `audio_sample` + `name` + `consent` (+ optional
  `ref_text`), `Remove(name)`, and `Ensure(ctx, []Voice)` — the
  docs/spec/07 reconcile (missing → register, conflicting name with a
  different digest → explicit `ErrVoiceConflict`, never re-register or
  overwrite). Identity is the 8-hex-char `Digest8` of the reference
  payload; the name carries it, so a name collision with different audio is
  detectable from the name alone.
- **Consent** (docs/spec/14): `Register` requires a non-empty `Consent`
  (sentinel `ErrConsentRequired`); a name whose listed consent is refused
  may not be registered (sentinel `ErrVoiceConflict`). Consent strings pass
  through to the server unchanged and are what `voice.registered` carries.
- **Naming** (docs/spec/07): the helper `MakeName(profile8, character,
  rev, digest)` produces `gn_<profile8>_<character>_v<rev>_<digest8>` with
  `rev ≥ 1`; `ValidateName` enforces the shape plus `profile8` and
  `digest8` as hex. The test namespace `gn_test_<random8>` is what the live
  tests register and what the registry's guards accept; the client refuses
  to `Remove` anything not in the configured namespace (default
  `gn_test_`) as a hard guard against deleting user voices.
- **Missing voice**: `Generate` with a 400 that names the voice as
  unknown/missing, or the server's "requires ref_audio" for a built-in
  voice, maps to `voice_missing` (docs/spec/06 kind) — the runtime's
  documented fallback (docs/spec/07 "Missing voice … → voice_missing,
  continue with the built-in default").
- **Reference probe**: `GenerateWithReference` sends inline `ref_audio` +
  `ref_text` + `task_type: "Base"` and returns `unsupported_capability`
  unless the server accepts it; `ProbeReferenceSupport` runs one minimal
  probe and records `supported|unsupported|unprobed`. The verified current
  contract supports inline reference (200), so the probe records
  `supported`; on deployments where it 400s the probe records
  `unsupported` and the capability is reported honestly — never assumed.
- **Doctor** (`internal/doctor/tts.go`, checks `providers.voice_design` /
  `providers.voice_clone`): the real client against the real role route
  under the check's deadline. voice_design sends a 3-word Design probe and
  reports duration/latency/sample rate; voice_clone lists the registry
  (registry URL from `GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL`, falling
  back to `GAME_NARRATOR_TTS_BASE_DIRECT_URL`; when neither is set it
  synthesises with `default` and notes that the registry was not probed),
  synthesises with the first `gn_*` voice or `default`, and runs the
  reference probe (best-effort: a probe failure is reported, not fatal).
  Real, not DEGRADED. ADR-0010 §7 redaction is enforced: summaries, details
  and remedies carry models, durations and kinds — never the gateway URL or
  token (asserted by the recorded doctor tests).
- **Live tests** (`Live*`, skipped as NOT RUN without the env vars, exactly
  the ADR-0013 house rule): voice_design exercises Design→validate with the
  real gateway; voice_clone exercises the full registry cycle on the direct
  Base URL — register `gn_test_<random8>` with a real consent string,
  verify it lists, synthesise with it (no `task_type`), assert the WAV
  invariants, and `Remove` in `t.Cleanup` so the test always cleans up. The
  live tests never touch voices that do not start `gn_test_` (the registry
  guard makes this a compile-level property, not a convention). WAVs are
  written to `t.TempDir()` and never committed.
- **Recorded tests** (httptest + committed cassette `testdata/ref-24k.wav`):
  Design with/without `instructions`/`seed`; the `task_type` 400→retry
  negotiation; the seed 400→retry + `seed_unsupported`; each taxonomy
  branch; the full WAV-corruption battery; the registry cycle including the
  consent-refused and digest-conflict guards; and the reference probe
  supported/unsupported outcomes.

## Alternatives

1. Route voice synthesis through the OpenAI chat adapter (ADR-0009) with a
   "TTS-like" voice field — rejected: the verified contract shows the
   gateway rejects that shape (400 `does not support task_type`), and chat
   endpoints return text, not audio.
2. Make the registry part of the gateway — rejected: the gateway returns
   404 for `/v1/audio/voices` and a collective gateway's role is proxying
   inference, not persistent state; the spec places the registry on the
   Base service.
3. Trust the server's WAV and skip validation — rejected: docs/spec/05
   names the WAV invariants as part of the safe-output contract, and a
   corrupt audio cache would poison playback for every game that uses it.
4. Send `task_type: "Base"` on every Generate — rejected: verified to 400
   for registered voices; lazy negotiation covers both server shapes.
5. Use a fake TTS in `doctor` — rejected: ADR-0010's purpose is that
   `doctor` proves the route with a real request; a fake probe would be the
   simulation theatre docs/spec/12 forbids.

## Tradeoffs

- The client negotiates `task_type` and `seed` from 400 text, matching the
  ADR-0021 precedent; the cost is parsing a couple of known error strings
  and recording the result, which is how a server-shaped (non-spec-first)
  contract is handled honestly in this codebase.
- `Digest8` is 8 hex chars of SHA-256: collision probability is negligible
  for the registry's size (a handful of voices per deployment), and the
  tradeoff is names that fit in voice fields and event payloads.
- The doctor's voice_clone check synthesises a short line on the role's
  route; on a GPU-starred box that costs a few seconds of decode. It is the
  same cost the runtime would pay for its first line, and docs/spec/06 sets
  the TTS budget at 30–120 s, so the check stays inside the documented
  budget instead of inventing a cheaper fake.
- `Ensure` never overwrites: a conflicting digest is an error for a human
  (or the next milestone's review UI) to resolve, which matches
  docs/spec/07 "never re-register or overwrite" and keeps consent revocable.

## Validation

- `go vet`, `gofmt -l` clean on both packages.
- Recorded suite: `go test -count=1 ./internal/providers/tts/ ./internal/doctor/`
  with the race detector — all taxonomy, WAV-invariant, registry, consent
  and naming assertions pass, including the doctor's redaction assertions.
- Live suite (this machine, 2026-07-21, gateway + direct Base service):
  - `go test -count=1 -v -run Live ./internal/providers/tts/` →
    `TestLiveDesignViaGateway` OK (3-word design, 24 kHz, ~1 s audio);
    `TestLiveGenerateViaGateway` OK (generate with a listed voice, WAV
    invariants hold); `TestLiveRegistryCycleDirect` OK (design a clip →
    register `gn_test_<8>` with a real consent string → verify it lists →
    synthesise with it → `Remove` in `t.Cleanup`).
  - `game-narrator doctor` with the voice roles configured →
    `providers.voice_design` OK and `providers.voice_clone` OK with
    measured duration/latency; token and URL absent from the output.
- Registry safety: the live tests registered only `gn_test_*` voices and
  removed them (in `t.Cleanup`); the direct service's voice list was
  restored to its pre-test state.

## Consequences

- The runtime now has a working, validated synthesis path and a
  consent-aware, reconcile-only voice registry for the M5 character pipeline
  and the M7 playback loop; `voice.ensure_failed` / `voice.registered` /
  `voice.removed` event emission is the runtime's job and lands with it.
- `doctor` is one check closer to "OK means the game can speak": both voice
  roles are probed with real requests.
- Deferred (documented, not silently assumed): `voice_embedding`
  round-trip, `speaker_embedding`, registry embedding management, and
  built-in-voice TTS on the Base checkpoint — all spec-only against the
  current server; the probe and the `unsupported_capability` result keep
  the system honest while they are absent.
- The live tests depend on two env vars being set to reachable services;
  without them they report NOT RUN (the ADR-0013 rule), never green.
