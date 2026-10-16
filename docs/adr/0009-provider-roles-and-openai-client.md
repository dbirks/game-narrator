# ADR-0009: Model-role configuration via environment and the OpenAI-compatible chat adapter
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: docs/spec/06 "Provider registry", "Interface requirements", "Model-serving version and health negotiation", "Failure taxonomy", "Timeout and cancellation defaults"; docs/spec/12 "Contract" mode (provider schema, recorded every PR / live conditional); docs/spec/16 Milestone 0; `go test -race ./internal/providers/...`, `go test -race -run Live -v ./internal/providers/...`

## Context and constraints

- docs/spec/06 requires the runtime to configure **roles**, not scattered raw
  URLs, and to state per role which protocol and endpoint serve it, with a
  timeout, so preflight can answer "which roles are usable right now" and the
  router can classify failures.
- Three of the seven roles (`agent_reasoning`, `fast_reasoning`,
  `vision_ocr_fallback`) are served today by OpenAI-compatible chat endpoints.
  Four (`decision`, `voice_design`, `voice_clone`, `speech_asr`) are **not
  deployed** and have no adapter in this build; they must still be
  representable in configuration and report `NOT CONFIGURED` rather than be
  absent or look usable.
- M0 has no YAML dependency (AGENTS.md "bounded dependencies"; `go.mod` carries
  only `modernc.org/sqlite`). `examples/config/config.example.yaml` documents
  the eventual `providers.roles` shape but is explicitly "not yet supported by
  an implemented binary".
- `internal/bootstrap` already established the precedence rule *flag > env >
  default* for the settings it owns, so role config follows the same mechanism.
- Credentials must never reach logs, UI, errors, fixtures or commits
  (AGENTS.md rule 5). Operators frequently already export a provider key under
  a different name and should not have to duplicate the secret.
- A live OpenAI-compatible gateway was reachable for contract verification
  during M0 (address and key supplied only through the environment; neither is
  recorded in this repository, including this ADR).

## Decision

Add `internal/providers` (configuration + shared failure taxonomy) and
`internal/providers/openai` (one narrow adapter):

1. **Role registry.** `providers.Role` covers all seven roles.
   `providers.RoleConfig{Role, Protocol, BaseURL, Model, TokenEnv, Timeout}`
   plus an unexported `token` field, and a `providers.Registry` view with
   `Ready()` / `Missing()` for preflight.
2. **Env-var surface** resolved by `providers.LoadRoles(lookup)`:
   - `GAME_NARRATOR_GATEWAY_URL` — shared gateway base URL.
   - `GAME_NARRATOR_GATEWAY_TOKEN_ENV` — **names the variable that holds the
     bearer token** (default `GAME_NARRATOR_GATEWAY_TOKEN`), so an existing
     secret variable can be referenced without copying the secret.
   - `GAME_NARRATOR_ROLE_<ROLE>_MODEL` (enables the role), `_URL`, `_PROTOCOL`,
     `_TIMEOUT` overrides.
   - Defaults follow docs/spec/06: protocols from the role table
     (`openai_chat`, `openai_chat_vision`, `systemone`, `vllm_omni_speech`,
     `probe_required`) and timeouts from "Timeout and cancellation defaults"
     (agent 30s, fast 10s, vision 10s, decision 1500ms, TTS 120s, ASR 30s).
   - A role without a model is `Configured() == false` and `Status()` renders
     `NOT CONFIGURED: … (set GAME_NARRATOR_ROLE_<ROLE>_MODEL)`.
   - URL validation rejects anything but `http`/`https`, an absent host, and
     URL userinfo, naming the offending variable and never echoing the token.
   - A role-specific `_URL` deliberately **drops** the shared gateway
     credential: a dedicated endpoint must be given its own credential path in
     the ADR that implements sticky/per-instance routing (docs/spec/06
     "Routing, reliability and replicas").
3. **Redaction is structural, not polite.** `RoleConfig.String()`,
   `RoleConfig.LogValue()` and `openai.Client.String()`/`LogValue()` render the
   credential as `****`; `providers.Redact` scrubs a secret and any
   `Bearer …` run from provider error text and caps it at 400 bytes. Error
   messages carry kind, status, model, request id and a short provider message
   — never the token or the request body.
4. **Adapter.** `openai.Client{BaseURL, Token, Model, HTTP *http.Client}` with
   `ListModels`, `Chat`, `Health`. `HTTP` stays nil by default so the
   **context deadline is the only timeout** (docs/spec/13 cancellation);
   `NewFromRole` sets the per-role timeout on the transport instead. No retries:
   retry/failover policy belongs to `ModelRouter`, which knows whether the
   originating observation is still current.
5. **Normalisation of the observed gateway shape** into `ChatResponse`:
   `Text` is `TrimSpace`-ed, `Reasoning` keeps `reasoning_content` **separate**
   (it must never be narrated), `Usage.Reasoning` reads
   `usage.reasoning_tokens`, `RequestID` prefers the `x-request-id` response
   header over the body `id`, and `Latency` measures the full request/response.
6. **Client-side capability gates**: the ChatML `developer` role is rejected
   before any request (`unsupported_capability`); each image is capped at 4 MiB
   (`payload_too_large`) and sent as a `data:image/<type>;base64,` part whose
   media type is sniffed from the bytes.
7. **Failure taxonomy mapping** (`*providers.Error{Kind, Status, Retryable,
   Msg, Err, RequestID, Role, Model}`), kinds exactly as listed in docs/spec/06:
   | Condition | Kind | Retryable |
   | --- | --- | --- |
   | 401 / 403 | `auth` | no |
   | 404, or 400 whose message names an unsupported parameter/role/model | `unsupported_capability` | no |
   | 408, context deadline, network timeout | `timeout` | yes |
   | caller cancellation | `cancelled` | no |
   | 413, oversized local image | `payload_too_large` | no |
   | 429 (including `insufficient_quota`) | `rate_limited` | yes |
   | 5xx, connection refused/DNS/reset, TLS failure | `unavailable` | yes |
   | malformed JSON, missing/null content without reasoning, zero or multiple choices | `bad_schema` | no |
   The `Kind` type is the shared vocabulary for later System One / TTS / ASR
   adapters; `Is` allows `errors.Is(err, &providers.Error{Kind: …})`.

## Alternatives considered

- **YAML role config now** (`examples/config/config.example.yaml`). Rejected for
  M0: it would add the repository's first parsing dependency, a schema version
  and a migration story before any consumer exists, and it duplicates the
  secret-in-file problem the token-env indirection exists to avoid. The
  `RoleConfig` shape is already YAML-shaped, so a later `LoadRolesYAML` is
  additive; revisit when the onboarding wizard writes config (docs/spec/03).
- **Scattered per-provider env vars** (`OPENAI_API_KEY`, `VISION_URL`, …).
  Rejected: docs/spec/06 requires role-level routing, health and provenance,
  which is unrepresentable when two roles share one variable name.
- **Read the token into `RoleConfig.String()` and redact at log time.**
  Rejected: redaction that depends on every caller remembering it fails the
  first time someone logs a struct with `%v`. Masking lives on the type.
- **Automatic retry with backoff inside the adapter.** Rejected now (AGENTS.md
  rule 7 / docs/spec/06: retries only while the event is still current) and the
  router needs the `Retryable` flag to make that judgement.
- **Send text-only `content` plus a separate `images` array** (the System One
  shape). Rejected for chat: the compatible gateway answers image questions only
  with `image_url` content parts, verified live.

## Consequences and risks

- Positive: role state is answerable without touching the network
  (`Status()`), so preflight can show `NOT CONFIGURED` for undeployed roles and
  the service can run cached-only; adding an adapter later changes only
  `roleSpecs[...].deployed`, not configuration.
- Positive: one taxonomy from the start means the router, UI and later TTS/ASR
  adapters classify identically.
- Risk: env-only config is untyped at the edges (typos in a role name are
  invisible). Mitigated by `EnvKeyList()` + `.env.example` and by validation
  that fails startup naming the variable.
- Risk: **a reasoning-capable gateway can approach the 10s `fast_reasoning`
  default.** Live evidence: the one-token "pong" answer took 8.8s on the first
  call of a cold gateway (34 reasoning tokens emitted first) and 0.57s warm on
  the identical prompt. The router must bound reasoning tokens and track p95 per
  role; the documented 10s default stays until calibrated on real runtime
  traffic.
- Risk: a gateway can hide replica identity behind one base URL, which is
  insufficient for cloned-voice routing (docs/spec/06). Out of scope here;
  recorded so the voice ADR cannot assume this client is instance-aware.
- Risk: `bad_schema` on a gateway that legitimately returns several choices is
  stricter than upstream; deliberate, because narrating an arbitrary choice of
  several would be nondeterministic.

## Evidence and test plan

Recorded (every PR, no network): `internal/providers/openai/client_test.go` —
`TestChatRecordedNormalizesLeadingNewlinesAndReasoning`,
`…SendsBearerAuthorizationAndRequestBodyShape`,
`…VisionRequestCarriesDataURLImageParts`, `…JSONModeSetsResponseFormat`,
`…ErrorClassification` (401/403/404/400/408/413/429/500/503),
`TestChatRecordedMalformedJSONIsBadSchema`, timeout via a short context,
cancellation, connection-refused, developer-role rejection, oversize-image
rejection, `TestRoleConfigStringMasksToken`. Response bodies are recorded
gateway payloads with model ids replaced by `recorded-model`; no endpoint or
credential appears in any fixture (AGENTS.md rule 5).

Live (conditional, docs/spec/12 "Contract" row): `live_test.go`, skipped with
`NOT RUN: set GAME_NARRATOR_GATEWAY_URL …` when the gateway or token is absent.
Verified against the real gateway on 2026-10-16 (model `qwen3-8-flash-next`,
endpoint and key supplied by environment only — deliberately not recorded):

```
TestLiveListModelsReportsTheConfiguredModel  model=qwen3-8-flash-next present=true count=3 latency=274ms
TestLiveChatTextAnswersPong                  finish=stop latency=8.823s usage=15/37 reasoning_tokens=34 reasoning_chars=144
TestLiveChatVisionNamesBothColors            bytes=144 finish=stop latency=767ms usage=94/63 text="red blue"
TestLiveChatJSONModeReturnsParsableObject    supported=true latency=560ms usage=34/41
TestLiveChatRejectsUnknownModelLive          kind=unsupported_capability status=404 retryable=false
```

A second `-count=1` run minutes later reproduced every assertion with warmer
latency (`pong` in 571ms, `usage=15/36 reasoning_tokens=33`; vision 878ms,
`text="red blue"`; JSON 473ms; 404 mapping unchanged), which separates the
cold-start outlier from steady state while keeping the slower observation on
record.

Gateway behaviours captured by those runs and encoded in the client:

1. `GET {base}/models` answers `{"data":[{"id":…}]}` and lists 3 ids.
2. `choices[0].message.content` is prefixed with blank lines (`"\n\npong"`) —
   trim before matching or narrating.
3. Reasoning arrives in a **separate** `reasoning_content` field and
   `usage.reasoning_tokens`; `prompt_tokens_details` was `null`.
4. The `developer` role is not accepted → rejected client-side as
   `unsupported_capability` (use `system`/`user`).
5. Images work as `{"type":"image_url","image_url":{"url":"data:image/png;base64,…"}}`
   content parts; a 64×64 red/blue PNG was answered correctly in 767ms.
6. `response_format: {"type":"json_object"}` **is** supported on this gateway:
   `{"ok":true}` parsed as JSON, so JSON mode is a usable capability here and is
   not silently assumed elsewhere (the live test still records a rejection as a
   capability finding instead of failing the suite).
7. An unknown model id returns 404 → `unsupported_capability`, non-retryable.

Commands: `go vet ./...`, `go test -race ./...` (live tests SKIP without env),
`go test -race -count=1 -run Live -v ./internal/providers/...` with the gateway
variables exported, and
`grep -rnE 'https?://[a-z0-9.-]+' internal/providers .env.example docs/adr/0009-*`
restricted to `127.0.0.1` / `example.invalid` placeholders.

## Migration / rollback

Purely additive: `internal/providers`, `internal/providers/openai`,
`.env.example`, this ADR. No schema, no API, no call-site change. Rollback is
deleting those paths; the role/timeout/protocol defaults it encodes live again
in docs/spec/06 until something else reads them. No restart or data migration is
involved, and no credential is stored anywhere by this decision.

## Follow-ups

- `ModelRouter`: role-based routes, bounded exponential backoff using
  `Retryable`, circuit breakers, sticky per-instance routing for cloned voices.
- Capability preflight in the API/UI: call `ListModels` + `Health` + a probe
  matrix and persist the capability fingerprint (docs/spec/06 "Model-serving
  version and health negotiation").
- System One, VoiceDesign/Base and ASR adapters with their own ADRs; flip
  `roleSpecs[*].deployed` as each lands and report `NOT AVAILABLE` until then.
- Calibrate the 10s `fast_reasoning` default against real runtime traffic and
  reasoning-token bounds (revise this ADR with measurements).
- YAML role config with schema versioning once the onboarding wizard persists
  settings; env stays the override layer above it.
- Per-role concurrency and max-payload caps, plus `usage` recording in the
  decision/event log.
