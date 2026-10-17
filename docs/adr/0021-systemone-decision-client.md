# ADR-0021: System One decision client — typed Clef-Omni classifier over the gateway's /systemone route
Date: 2026-10-17
Status: accepted
Owners: project
Related requirements/tests: docs/spec/06 "Provider registry" (decision role), "System One / Clef-Omni protocol", "Interface requirements" (`DecisionProvider.Decide`), "Timeout and cancellation defaults" (Clef 1500 ms), "Failure taxonomy"; docs/spec/05 "Clef adjudication and commit policy"; docs/spec/17 "Event envelope" (`decision.resolved`); builds on ADR-0009 (provider roles + failure taxonomy), ADR-0013 (recorded cassettes)

## Context and constraints

- The `decision` role (docs/spec/06) serves the runtime's Clef adjudication
  step (docs/spec/05): one screenshot + context + ranked candidates, answered
  as one batch of typed questions (`is_textbox`, `text_kind`, `speaker`,
  `candidate_message`, `is_complete`, `safe_to_speak`). Until this ADR the
  role was representable but inert: `doctor` reported the configured
  decision role as DEGRADED "client not implemented", and the runtime had
  nothing to call.
- Clef-Omni is not a chat model. docs/spec/06 is explicit: it is a
  joint-schema pooling model spoken to over the System One protocol with a
  `state` object and a `questions` object (choice / noul / score types), and
  a generic top-level `instructions` field is *rejected* by the checkpoint.
  Routing the role through the OpenAI chat adapter (ADR-0009) would therefore
  send a wire shape the model's serving stack does not accept, and would
  force the whole batch into one free-text completion that the runtime had
  to parse and trust — the exact failure mode the typed protocol exists to
  avoid.
- The contract was verified live on 2026-10-17 through the collective
  gateway at `GAME_NARRATOR_GATEWAY_URL` + `/systemone` with a bearer token.
  Facts marked *verified* below were observed against that endpoint; facts
  marked *spec-only* come from docs/spec/06 and must be re-verified before
  they are relied on.
- The runtime's budget is 1500 ms (docs/spec/06 "Timeout and cancellation
  defaults"), after which the runtime proceeds without Clef. A cold gateway
  queue immediately after a GPU reset was observed once at **13 s** — so
  diagnostics need a separate, longer budget, and per-call deadline
  handling is a correctness requirement, not a nicety.

## Decision

- New package `internal/providers/systemone` implements the `decision` role:
  `Client.Decide(ctx, Request) (Decision, error)`.
- **Request.** `model` and `state` are required top-level fields; `questions`
  is an **object keyed by question id**, serialised in the deterministic
  order of `Request.Questions` (a Go map would serialise in arbitrary order);
  `choice` questions carry a `criteria` object (option id → description);
  `noul` questions carry nothing; `score` questions carry an ordered `levels`
  array (*spec-only shape, see Consequences). `images`, when present, are
  base64 data URLs (`data:image/jpeg;base64,…`, `data:image/png;base64,…`),
  max 4 images, 4 MiB each. There is **no top-level `instructions` field** in
  the encoder at all — the prompt text lives per-question in `instructions`.
- **Validation (client side, before the network):** ids match
  `[a-z0-9_]{1,32}` and are unique; choice has 2..16 options (the 16 cap is
  the `MaxOptions` default from docs/spec/05 "limit candidate options to
  supported request cap"); images are non-empty, within the byte cap and
  JPEG/PNG by magic bytes; `state` is non-empty. All violations are reported
  in one `bad_schema` error (over-cap images are `payload_too_large`), so a
  caller fixes the whole request in one round trip instead of serially.
- **Validation (response side, docs/spec/06 "Response parser validates…"):**
  every asked question id is answered and no extra ids appear; answer type
  matches the question type; a choice is inside the declared options;
  probabilities are finite, in [0,1], cover exactly the declared options and
  sum to 1 ± 0.02; confidence equals the chosen option's probability
  ± 1e-3; noul is in [0,1]. Every violation is collected into one
  `bad_schema` error listing all of them. A model echo that differs from the
  requested model is `wrong_checkpoint` (a different checkpoint answered;
  the answers are not trustworthy), not `bad_schema`.
- **Error classification** mirrors `internal/providers/openai`: 401/403 →
  `auth`, 404 → `unsupported_capability`, 413 → `payload_too_large`, 422 →
  `bad_schema` carrying the gateway's `detail`, 429 → `rate_limited`,
  5xx/408/transport → `unavailable`, expired context deadline → `timeout`,
  caller cancel → `cancelled`. The transport error cause chain is preserved,
  so a recorded-cassette miss (ADR-0013) survives the adapter and stays
  non-retryable.
- **Deadline policy.** The context is the single deadline owner (the client
  transport has no timeout of its own unless the role config supplies one,
  matching ADR-0009). The runtime sets 1500 ms and proceeds without Clef on
  timeout; `doctor` sets a **20 s** probe budget because the 13 s cold-start
  observation makes the runtime budget diagnostic-hostile.
- **Adjudication glue.** `BuildAdjudication(obs, candidates, speakers)`
  renders the docs/spec/05 bundle with fixed question ids and deterministic
  candidate option ids `c1..cN` (plus `none`); `Interpret(decision,
  candidates)` maps `cN` back to the caller's message ids and returns raw
  floats — **no calibration here**, per docs/spec/06 the probabilities are
  signals for a per-game commit policy, not guarantees.
- **Rate limiting.** `WithDoubtLimit(limit, uncertain)` installs a per-key
  (session) token bucket for consecutive doubtful decisions (docs/spec/06
  "Rate-limit consecutive doubtful decisions"): each doubtful answer spends a
  token, a confident answer refills, and an empty bucket makes `Decide` fail
  locally with `rate_limited` before any request is sent. Default classifier:
  a choice picked `none`/`unknown`, or a noul < 0.5.
- **Doctor.** `providers.decision` is now a real probe: one tiny choice
  question (content-agnostic — any schema-valid answer proves the route),
  20 s budget, OK with the measured latency when the endpoint answers
  validly, FAIL with the taxonomy kind otherwise, NOT CONFIGURED when the
  role has no model.
- `.env.example` now documents `GAME_NARRATOR_ROLE_DECISION_MODEL=clef-omni`
  (placeholder, commented out) with `systemone` protocol and an optional
  per-role `_URL`.

## Alternatives considered

1. **Route the decision role through the OpenAI chat adapter** (prompt the
   model for JSON, parse it). Rejected: the checkpoint rejects top-level
   instructions and the serving protocol is typed by design; a free-text
   answer would put the runtime's commit policy behind a parser and a model
   prior rather than validated, schema-bounded signals (docs/spec/06).
2. **Wait for the runtime milestone and ship no client (M4).** Rejected: the
   role stays unverifiable in `doctor`, and docs/spec/16 expects preflight
   to probe every configured role; the protocol facts needed a live contract
   test anyway, which lands with the client.
3. **A bespoke HTTP client with no shared taxonomy.** Rejected: every other
   adapter classifies into the shared `providers.Error` vocabulary, and the
   router's retry/circuit policy switches on that vocabulary (docs/spec/06
   "Failure taxonomy": "Retries must distinguish transient network failure
   vs permanent schema/policy failure").

## Consequences and risks

- **Verified live (2026-10-17):** request/response envelope shape; `model`
  and `state` required (422 `{"detail":"model and state are required"}`);
  422 is the bad-request status; bearer auth; a warm call at ~0.5 s and a
  13 s cold-start tail; image transport accepted (an 83-byte PNG round-tripped
  in the live test, `text_kind=none` on a blank frame); `usage.input_tokens`
  is populated while `output_tokens` is 0 (the forward pass is one pool, not
  a generation — expect prompt-side accounting only).
- **Spec-only, unverified live:** the `score` question type. docs/spec/06
  documents it ("score is ordered levels") but no live observation exists;
  the client supports a symmetric shape (request `levels` array; response
  `level` + optional `confidence`) and validates the level against the
  declared set. Until a live score round-trip is recorded, the runtime must
  not rely on it.
- **Calibration stays out of scope.** `Interpret` returns raw signals; the
  commit-policy thresholds are the runtime's, benchmarked per game/scene
  (docs/spec/05). A future calibration layer must not be baked into this
  client.
- **Gateway limits are negotiated, not assumed.** The local caps (4 images,
  4 MiB, 16 options) are fail-fast defaults; a stricter gateway still gets a
  4xx that maps to the taxonomy, and preflight should record the negotiated
  limits (docs/spec/06 "probe negotiated limits rather than assume upstream
  limits").
- **New failure surface:** a misconfigured `GAME_NARRATOR_ROLE_DECISION_URL`
  pointing at a gateway that lacks the `/systemone` route yields
  `unsupported_capability` (404) from `doctor` — an operator-visible,
  actionable FAIL rather than a silent DEGRADED.
- **Secrets:** the client renders without the token (`String`/`LogValue`
  mask it), errors never carry the base URL, and the cassette layer (ADR-0013)
  refuses to write a bearer token into a fixture. The committed cassette
  `internal/providers/systemone/testdata/decision.jsonl` was recorded from an
  httptest fake gateway, never from a live endpoint.

## Evidence and test plan

All in-repo and reproducible; live tests SKIP with `NOT RUN: …` offline.

- `go vet ./internal/providers/systemone/... ./internal/doctor/...`
- `go test -race -count=1 ./internal/providers/systemone/... ./internal/doctor/...`
  - golden wire serialization (deterministic key order, data-URL images, no
    top-level `instructions`): `TestGoldenRequestSerialization`,
    `TestRequestEncodingIsDeterministic`, `TestRequestEncodingKeyOrder`,
    `TestRequestEncodingHasNoTopLevelInstructions`;
  - every request/response validation rule: `TestRequestValidation`,
    `TestResponseValidation`, `TestProbabilityValidationRejectsNonFinite`,
    `TestResponseValidationListsEveryProblem`;
  - HTTP error mapping, deadline → `timeout`, cancel → `cancelled`,
    unreachable → `unavailable`: `TestHTTPStatusMapping`,
    `TestDeadlineExceededIsTimeout`, `TestCallerCancellationIsCancelled`,
    `TestUnreachableEndpointIsUnavailable`;
  - doubt budget: `TestDoubtLimitRefusesAfterConsecutiveDoubts`,
    `TestDoubtLimitResetsOnConfidentAnswer`, `TestDoubtLimitDisabledByDefault`;
  - cassette round trip through `internal/providers/recorded` (recorded
    from an httptest fake, replayed offline, live/cassette byte-comparison,
    origin labelling, miss handling): `TestRecordedCassetteRoundTrip`,
    `TestCassetteMissFailsLoudly`;
  - doctor decision check: `TestDecisionRoleOKAgainstFakeSystemOne`,
    `TestDecisionRoleFailsWithAuthKind`, `TestDecisionRoleFailsWithBadSchema`,
    `TestDecisionRoleTimeoutBoundsTheProbe`, `TestDecisionRoleNotConfiguredWithoutModel`,
    `TestDecisionProbeInjection`.
- Live contract tests (`-run Live`, gateway + token from the environment,
  model from `GAME_NARRATOR_ROLE_DECISION_MODEL`, default `clef-omni`):
  `TestLiveDecisionBundleAdjudication` (bundle on
  "Would you like to save your progress?", verified answers
  `text_kind=dialogue 0.551`, `candidate_message=c1 0.950`,
  `speaker=system 0.945`, `is_textbox 0.633`, `is_complete 0.981`,
  `safe_to_speak 0.299`, latency 406 ms, usage 621/0),
  `TestLiveDecisionAcceptsImage` (83-byte PNG accepted, latency 283 ms,
  usage 233/0), `TestLiveDecisionMissingStateIsBadSchema` (422 →
  `bad_schema`).
- `game-narrator doctor` reports `providers.decision` with the measured
  latency on a live gateway and NOT CONFIGURED / FAIL with the taxonomy kind
  otherwise.

## Migration / rollback

- No persistence or data migration: the client is stateless, the doubt
  budget is in-process, and the cassette lives under the package's
  `testdata/`.
- Rollback: remove `internal/providers/systemone` and the doctor dispatch in
  `internal/doctor/providers.go` (`checkDecisionRole`), restore the
  DEGRADED placeholder, and revert `.env.example`. The role configuration
  format (`GAME_NARRATOR_ROLE_DECISION_*`) is unchanged, so no operator
  configuration breaks in either direction.
- Reversal plan if the live protocol diverges from this ADR (e.g. a future
  gateway move to `/v1/systemone` or a renamed `score` field): the wire
  shapes live in `types.go`/`client.go` and the golden + cassette fixtures
  fail loudly; the contract tests define the fix.

## Follow-ups

- Record a live `score` round-trip (or drop the type) and close the
  spec-only gap; update this ADR with the observed wire shape.
- Probe and record the gateway's negotiated limits (max images/options/payload)
  in preflight instead of assuming the local caps.
- Wire the runtime's 1500 ms decision deadline, skip-on-timeout policy and
  the doubt budget into the recognition loop (docs/spec/05) and start the
  per-game calibration of the raw signals.
- Consider a `stale_result` check: the router must drop a Clef answer that
  lands after its observation has been superseded (docs/spec/06 failure
  taxonomy) — the client records the provenance fields for it.
