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

## Capability fallback (2026-10-17 amendment)

The NInfer gateway build serving qwen3-8-27b rejects two request features with
an HTTP 400 it cannot guarantee: `response_format {"type":"json_object"}` and
`tool_choice:"required"` (`tool_choice:"auto"` and image input are accepted).
A gateway that cannot guarantee a feature is a **capability finding, not a
schema accident**, so the adapter now degrades honestly instead of failing:

1. **Typed capability errors.** A 400 that names `response_format` or
   `tool_choice` maps to `unsupported_capability` (the existing classification,
   kept) and is tagged with a matchable sentinel: `ErrUnsupportedResponseFormat`
   or `ErrUnsupportedToolChoice` (`errors.Is`), so callers and diagnostics name
   the exact gap rather than parse prose.
2. **Opt-in, retry-once fallback.** `ChatRequest.JSONModeFallback` retries once
   without `response_format` plus a "single JSON object only" system instruction,
   then validates the response is one parseable object (extracted outermost
   `{…}`; a non-object answer is a typed `bad_schema`, no further retries).
   `ChatRequest.ToolChoiceFallback` retries `required` once as `auto` plus a
   call-a-tool instruction and reports `ErrToolCallMissing` if no call arrives.
   Without the opt-in flag a rejection still fails loudly. This is a bounded
   retry (one), inside the adapter, and is not the model router's retry policy.
3. **Cached per (base URL, model).** A "rejected" finding is cached for the
   client's lifetime (a `Client` is one base URL + model), so later calls skip
   the doomed first attempt. `Client.Capabilities()` exposes the snapshot
   (`json_mode`, `tool_choice_required`) for diagnostics; `doctor` reports both
   per role. `analyze_character` opts into `JSONModeFallback` and keeps
   validating the brief's fields.
4. **Honest live tests.** The tool-calling live test uses `auto` (the app's real
   usage); a separate `required` probe and the JSON-mode test record a
   CAPABILITY FINDING (pass with a logged finding) instead of asserting a
   capability the deployment does not have (AGENTS.md rule 3, no simulation
   theatre). `TestLiveJSONModeFallbackReturnsObject` proves the degraded path
   returns a parseable object against the real gateway.

**Tradeoff / reversal.** The fallback hides a genuine deployment limitation
behind a working call; the `Capabilities()` snapshot and the live findings keep
that visibility. If the gateway grows to guarantee structured output or forced
tool calls, the finding flips to `supported` with no code change, and the
fallback simply stops firing.

## Reasoning effort (2026-10-17 amendment)

Live evidence (orchestrator, `qwen3-8-27b` via the shared gateway, 2026-10-17):
the gateway honours the `reasoning_effort` chat-completions field —
`"none"` → 0 reasoning tokens, `"low"` → ~25 on the tool-calling prompt —
while an omitted field lets the model spend its default budget: with
`max_tokens=48` a counting prompt consumed the entire budget in
`reasoning_content` and returned `finish_reason=length` with empty content
(usage `completion_tokens=48`, `completion_tokens_details.reasoning_tokens=48`).
Per-request thinking budgets (`thinking_budget`, `thinking_token_budget`,
`max_thinking_tokens`) are silently ignored by the serving stack, so the
supported lever is the effort level. This made
`TestLiveToolCallingExecutesOneTool` fail ~1/5 of runs with
`finish=length text=""` at `MaxTokens=256`, and left every app role with the
same exposure (the adapter never sent the field).

1. **Per-role configuration.** `RoleConfig` gains `ReasoningEffort` and
   `LoadRoles` resolves `GAME_NARRATOR_ROLE_<ROLE>_REASONING_EFFORT`
   (helper `RoleReasoningEffortEnv`, listed by `EnvKeyList()`). Accepted
   values are `none | low | medium | high` (case-insensitive); an unknown
   value is a startup config error naming the role and the variable. The
   env var overrides the role default; an empty value keeps the default.
   Defaults are latency-driven (docs/spec/05 timings):
   `fast_reasoning` and `vision_ocr_fallback` (10 s ceiling, runtime hot
   paths) → `none`; `agent_reasoning` → `low`; the undeployed roles → empty
   (the field is omitted and the model default applies). Documented here
   rather than in `examples/` (out of scope for this change).
2. **Per-request override.** `openai.ChatRequest.ReasoningEffort` (empty =
   omit) is sent as `reasoning_effort` and wins over the role default;
   `NewFromRole` carries the role default into the client, so plain `Chat`
   calls from role clients are bounded by default. The field is omitted when
   both are empty, keeping pre-tool request shapes byte-identical.
3. **Capability degradation, cached.** A 400 naming `reasoning_effort`
   (matched by `IsReasoningEffortRejection`; the name is also a
   `kindForStatus` capability hint) degrades once by omitting the field and
   caches the finding (`Capabilities().ReasoningEffort`) per (base URL,
   model) exactly like the JSON-mode and tool_choice findings — later calls
   skip the doomed first attempt. Verified against this gateway the field is
   accepted, so the degradation is insurance, not the expected path.
4. **New failure kind `budget_exhausted` + `ErrOutputBudgetExhausted`.** A
   completion that ends `finish_reason=length` with neither text nor a tool
   call is the model having spent the whole output budget (usually
   reasoning) — now a typed, non-retryable `*providers.Error` of kind
   `budget_exhausted` carrying the gateway usage numbers
   (`prompt/completion/reasoning`), with `errors.Is(err,
   ErrOutputBudgetExhausted)`. This extends the docs/spec/06 kind list by one
   member (the spec's 15 kinds had no "budget consumed, no answer" case);
   `ValidKind`/taxonomy tests cover it. A reasoning-only answer on
   `finish_reason=stop` remains a normal response, and a tool call on a
   `length` finish is a complete answer.
5. **Combined capability degradation fixed.** When the JSON *and*
   tool_choice degradations apply, each degraded retry re-derives the request
   from the current flag set through one composed instruction (both the
   "single JSON object only" and the call-a-tool parts), so the second
   rebuild no longer discards the first's instruction. The retry is a bounded
   loop — each capability degrades at most once, so at most three attempts
   per call. Unit tests cover the in-call sequence (400 JSON → 400 tool →
   200, final request carries both instructions) and the cached-both case
   (single request, composed instruction).

**New tests.** Recorded/synthetic (no network): `TestChatRecordedLengthFinishWithoutAnswerIsBudgetExhausted`,
`TestChatRecordedLengthFinishWithToolCallIsNotAnError` (both finish-length
edge cases), `TestChatRecordedNullContentWithReasoningIsNotAnError` (re-scoped
to `finish=stop`; a reasoning-only answer on `stop` stays a normal response),
`TestReasoningEffortRejectionRetriesWithoutTheField`,
`TestReasoningEffortRejectedCacheSkipsDoomedFirstAttempt`,
`TestReasoningEffortEmptySendsNoField`,
`TestCombinedJSONAndToolChoiceDegradationKeepsBothInstructions`,
`TestCombinedJSONAndToolChoiceDegradationFromCacheIsOneRequest`, and
`TestLoadRolesReasoningEffortDefaultsOverrideAndValidation`. The recorded
request-shape test additionally asserts `reasoning_effort` is omitted when
unrequested.

**Live verification (2026-10-17, qwen3-8-27b):**

```
TestLiveToolCallingExecutesOneTool  -count=5, all PASS (reasoning_effort=low, max_tokens=1024)
  finish=tool_calls tool=echo_tool latency=1.653s usage=321/53 reasoning_tokens=25
  finish=tool_calls tool=echo_tool latency=2.702s usage=321/54 reasoning_tokens=26
  finish=tool_calls tool=echo_tool latency=2.82s  usage=321/53 reasoning_tokens=25
  finish=tool_calls tool=echo_tool latency=4.179s usage=321/55 reasoning_tokens=27
  finish=tool_calls tool=echo_tool latency=1.503s usage=321/49 reasoning_tokens=21
TestLiveReasoningEffortNoneYieldsZeroReasoningTokens  PASS
  finish=stop latency=449ms usage=17/2 reasoning_tokens=0
```

The flaky ~1/5 `finish=length text=""` failure at `MaxTokens=256` with the
model's default effort is gone at the pinned effort + 1024 budget.

**Tradeoff / reversal.** `none` on the runtime roles trades model reasoning
quality for the 10 s ceiling; a role can be raised via its env var without
code changes. The `budget_exhausted` kind is additive; removing it (and the
sentinel) restores the old "bad_schema on empty message" behaviour if a
future gateway makes the case unreachable. The composed instruction merges
two system hints into one message; if a future deployment misbehaves with
combined instructions, the flags still degrade independently per capability.
