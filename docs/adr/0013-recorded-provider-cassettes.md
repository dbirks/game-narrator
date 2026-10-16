# ADR-0013: Recorded provider cassettes — transport-level record/replay with redaction and origin labelling
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: docs/spec/12 "Required test modes" (Contract / Frame replay / UI rows use "recorded Clef/VLM", "recorded providers"; "No test may silently substitute canned model replies… Reports explicitly record stage origins"), docs/spec/11 "Headless automation modes" Mode B, docs/spec/16 Milestone 0 ("mocked and replayable providers"); `go test -race ./internal/providers/...`, `go test -race -count=1 -run Live -v ./internal/providers/recorded/`; builds on ADR-0009

## Context and constraints

- docs/spec/12 requires recorded provider responses for three of its blocking
  test modes, and requires that a fixture be a "deterministic provider-response
  sidecar" with a `source_type` label, "valid… frame/image hashes" and human
  review. Nothing in the tree could produce or consume such a sidecar: ADR-0009
  shipped hand-copied response bodies inside `client_test.go`, which proves the
  client's schema handling but cannot be replayed against a *client* end-to-end,
  cannot be regenerated, and records no request.
- docs/spec/11 Mode B ("real rendering/input/OCR, fixed deterministic model
  results") needs the provider half to be swappable per process, not per test
  file, so a Dolphin run can be reproducible without a gateway.
- ADR-0009 already injects an `*http.Client` into every adapter and makes the
  context the only deadline. Any solution that lives below `http.Client` is
  therefore free for the OpenAI chat adapter, and later for System One,
  vLLM-Omni TTS/ASR and web-search fetchers, with no per-adapter code.
- Cassettes are committable artefacts. ADR-0009's rule that credentials and
  private endpoint URLs never reach the tree (AGENTS.md rule 5) applies with
  full force: a recorded request is *where a bearer token and a screenshot
  actually live*, so redaction cannot be a courtesy performed by the caller.
- "Recorded" must never be mistaken for "real". docs/spec/12 forbids silent
  substitution; a report has to state, per stage, whether a model answered.

## Decision

Add `internal/providers/recorded`: an `http.RoundTripper` plus a mode switch.
No provider adapter learns that cassettes exist.

1. **Cassette format.** JSONL, one interaction per line, at
   `<GAME_NARRATOR_CASSETTE_DIR>/<role>.jsonl`:

   ```json
   {"schema_version":1,"role":"fast_reasoning","origin":"recorded",
    "recorded_at":"2026-10-16T12:00:00Z",
    "request":{"method":"POST","path":"/v1/chat/completions","query":"",
      "headers":{"accept":"application/json","content-type":"application/json"},
      "body_digest":"sha256:<hex>",
      "body":{"max_tokens":32,"messages":[{"content":"Reply with exactly: pong","role":"user"}],
              "model":"fake-gateway-model","stream":false}},
    "response":{"status":200,
      "headers":{"content-type":"application/json","x-request-id":"fake-chat-1"},
      "body":"{\"id\":\"chatcmpl-fake-1\",…}"},
    "latency_ms":4}
   ```

   - **The host is never stored.** Only `path` + canonical `query` are, because
     a cassette describes *what a role answered*, not *which machine* answered
     it: endpoints are deployment data (docs/spec/06 configures roles;
     AGENTS.md rule 5 keeps private URLs out of Git). `role` carries the logical
     role name from `providers.Role` instead. `roleFile` rejects a role name
     containing a separator so a role can never address a file outside the
     cassette directory.
   - `request.body` is the *canonical* redacted JSON — keys sorted, compact, no
     HTML escaping, number literals preserved via `json.Number` — so
     re-serialising an equivalent request produces the same bytes and the same
     digest. `body_digest` is the sha256 of those bytes and is re-verified on
     load: a body edited after recording makes the cassette unloadable rather
     than quietly wrong.
   - A reader rejects `schema_version != 1`, `origin != "recorded"`, a
     `recorded_at` that is not RFC 3339, and **unknown fields**
     (`DisallowUnknownFields`). Version drift in a sidecar must fail, because a
     silently-misread cassette is exactly the "synthetic answer presented as
     real" failure docs/spec/12 forbids.

2. **Redaction happens before anything touches disk**, in three independent
   layers, each with its own test:

   | What | Rule |
   | --- | --- |
   | Headers | allowlist only: request `content-type`, `accept`; response `content-type`, `x-request-id`. `Authorization`, cookies, client fingerprints are dropped structurally, not scrubbed after the fact. |
   | Image payloads | every `data:<mime>;base64,<…>` string anywhere in a request body becomes `data:<mime>;sha256,<hex>`, hashing the **decoded** bytes when base64 decodes, so the same image matches regardless of encoding alphabet. Images are matched by digest, never stored. |
   | Final guard | the serialised line is scanned and the write is **refused** (error, no file created) if it contains the active token value, a bare `Bearer <≥12-char token>` run, or any surviving `;base64,` marker. The same guard runs when a cassette is *read*, so a hand-edited fixture cannot smuggle a credential into a run either. |

   A **response** body that carries a base64 payload is refused at record time
   rather than rewritten: replay must return the provider's bytes verbatim, and
   silently redacting a response would make replay serve a fabrication. The
   error names the reason and the limit.

3. **Matching.** Replay key = `method` + `path` + canonical `query` + canonical
   redacted body; the digest of that tuple is the handle used in errors
   (`Request.KeyDigest()`). Key order/whitespace/number formatting in the client's
   serialisation are irrelevant; **semantic differences never silently match**:
   a reordered `content` part array, a changed prompt or a different image
   misses. Multiple interactions with one key replay in recorded order and then
   **miss** — a third identical request is not answered by reusing the last
   canned reply. A miss is a typed `*recorded.MissError` wrapping
   `providers.Error{Kind: unsupported_capability, Retryable: false}` whose
   message begins `cassette_miss:` and names the key digest; `recorded.IsMiss`
   and `recorded.MissDigest` see it through adapter wrapping. Nothing is ever
   invented to satisfy a request.

4. **Modes** via `recorded.ModeFromEnv(lookup)` reading
   `GAME_NARRATOR_PROVIDER_MODE` = `live` (default) | `record` | `replay`, plus
   `GAME_NARRATOR_CASSETTE_DIR`:
   - invalid mode or a missing directory for `record`/`replay` is a startup
     error naming the variable — an operator who asked for replay must never be
     quietly downgraded to making real model calls;
   - `record` wraps the real transport, appends, and is otherwise transparent
     (the request forwarded after the body was drained for redaction is
     byte-identical, which `TestRecordThenReplayRoundTrip` asserts from the
     stub's point of view);
   - `replay` never constructs a socket: the cassette is consulted before `Base`
     is touched, `Stats()` reports `dials`, and the tests pass a `Base` that
     fails the test if called;
   - `recorded.FromEnv(role, lookup)` wires mode, dir and the *resolved* gateway
     token (through `providers.LoadRoles`, so the secret refused on write is the
     same secret the client sends).

5. **Origin labelling.** Every response carries `X-Game-Narrator-Origin`:
   `recorded` from a cassette, `live` from `live`/`record` mode.
   `recorded.Origin(*http.Response)` / `OriginFromHeader(http.Header)` return
   `live`, `recorded` or `unknown` — an unrecognised or absent label is
   `unknown`, never trusted as live content. Callers (developer UI, evidence
   bundle, Playwright traces) put that value in reports, which is what makes
   docs/spec/12's "reports explicitly record stage origins" checkable rather
   than aspirational. The adapter does not need to know: `openai` tests attach a
   one-line sniffer RoundTripper to read the label off real client calls.

6. **Why transport-level.** One implementation covers every HTTP provider
   (OpenAI chat today; System One, VoiceDesign/Base, ASR and web fetchers
   tomorrow) with no adapter-side test hooks, no interface to keep in step, and
   no risk that a role's fake diverges from its real wire shape — the cassette
   *is* the wire shape. It also keeps replay honest at the only layer where
   bytes and headers exist, so redaction, digesting and origin labelling cannot
   be bypassed by a future client that hand-rolls a request.

7. **One narrow adapter change** (ADR-0009 client): `transportError` now
   respects a transport that already classified its failure into the shared
   taxonomy instead of flattening it to `unavailable: transport failure`, and
   keeps the original error as the cause. Without this a cassette miss would
   reach the router as a retryable network error naming nothing.

8. **A committed offline example**: `testdata/fast_reasoning.jsonl`, recorded
   from the **httptest fake gateway** (text chat, vision chat with the
   deterministic 64×64 red/blue PNG, model listing) with a pinned clock and
   pinned latency, so `GN_GENERATE_TESTDATA=1 go test -run
   TestGenerateCommittedCassette ./internal/providers/recorded/` reproduces it
   byte-identically. Live-recorded cassettes are written only to `t.TempDir()`
   and are never committed.

## Alternatives considered

- **Per-adapter fake clients** (a `Chat` stub, a `Synthesize` stub, an ASR
  stub). Rejected: N implementations, each drifting from the real wire shape,
  and none of them can prove the *client* normalises a recorded payload
  correctly — which is the assertion docs/spec/12 cares about.
- **Mocking at the provider interface** (fake `fast_reasoning` returning canned
  `ChatResponse`s). Rejected: skips request construction, header handling,
  status classification and JSON normalisation, so a broken request builder would
  replay happily. Also cannot be shared with UI/frame-replay modes that need
  request-level correlation ids.
- **`httptest.Server` replay** (serve cassettes from a local fake server).
  Rejected: costs a listener and a port per role, adds a second HTTP stack and
  DNS/loopback flakiness, and cannot assert "replay never dials" as directly as
  a transport that refuses to exist.
- **Storing absolute URLs and scrubbing secrets at log time.** Rejected: ADR-0009
  already concluded redaction that depends on every caller remembering it fails;
  and a stored URL is a private endpoint whether or not it is printed.
- **`unsupported_capability` vs a new `cassette_miss` taxonomy kind.** The
  taxonomy lives in `internal/providers/errors.go`, outside this task's
  boundaries, and a new `Kind` would need `Valid()`/`RetryableByDefault()`
  changes plus a router decision. Chosen: reuse `unsupported_capability` (a
  miss really is "this build cannot serve this request right now"), carry the
  literal `cassette_miss` label + key digest in `Msg`, and expose the typed
  `MissError`. A later ADR may add `KindCassetteMiss` if the router needs to
  treat it distinctly from a model-capability gap; supersede this section then.
- **Simulating recorded latency on replay.** Rejected for M0: it makes frame
  replay and UI tests slower without making them truer; timing behaviour is the
  subject of the delay-injection suite, which should inject explicitly. The
  measured `latency_ms` is stored for reports either way.

## Consequences and risks

- Positive: the same `openai.Client` code path answers from a gateway or a
  cassette (`TestGatewayAndCassetteProduceIdenticalResults`), so Mode B, UI and
  frame-replay tests can be built on production request construction.
- Positive: error responses are recordable too (429/503/400 included), and
  retry attempts land in the cassette as separate ordered interactions —
  docs/spec/12's provider-failure cases become reproducible offline from real
  captures instead of hand-written strings.
- Positive: cassettes are diffable, grep-able and committable; the CI example
  needs no network at all.
- Cost: a cassette is role-scoped and append-only. Deleting one interaction means
  regenerating the file, not editing a line (editing is detected by
  `body_digest`).
- **Limit (explicit, fails loudly):** streaming. A request with
  `"stream": true`, or a `text/event-stream`/`ndjson` response, or any binary
  response body (TTS audio, generated image) is **refused at record time** with
  `unsupported_capability` and no partial write. vLLM-Omni TTS/ASR therefore
  cannot be recorded by this mechanism yet; that needs a content-addressed
  binary sidecar (digest-keyed blob store plus a `multipart`/raw-bytes cassette
  entry) and its own ADR before M4.
- **Limit:** one request ↔ one response. Retries, redirects and HTTP/2 push are
  not modelled; a transport that redirects would need the final request recorded.
- **Limit:** the cassette stores only allowlisted headers, so a gateway whose
  only correlation id is a non-allowlisted header loses it on replay
  (`ChatResponse.RequestID` then falls back to the body `id`). Add headers to the
  allowlist deliberately — each one widens what a fixture can carry.
- **Risk:** a stale cassette that still *matches* replays an old answer as if it
  were current. Mitigated by the origin label on every response, the
  `body_digest` self-check, and by the live contract tests in
  `TestLiveRecordThenReplay*` which re-record from the real gateway; not
  mitigated against a gateway that changes behaviour between live runs.
- **Risk:** the token guard only knows the secrets it is handed. `FromEnv` wires
  the resolved gateway token; a caller using a different credential source must
  pass it in `Options.Secrets`.

## Evidence and test plan

Offline (every PR, no network), `internal/providers/recorded`:

- `redact_test.go` — `TestFilterHeadersKeepsOnlyTheAllowlist`,
  `TestRedactRequestBodyReplacesBase64ImageWithDigest`,
  `TestRedactRequestBodyIsCanonicalAndStable` (key order, number literals,
  HTML escaping), `TestRedactRequestBodyRejectsOversizeAndBinary`,
  `TestGuardRefusesCredentialBearerRunAndBase64`,
  `TestInteractionLineRefusesSecretAndWritesNothing` (write refused **and** no
  file created; tampered cassette refused at read),
  `TestAppendInteractionRejectsSchemaDrift`, `TestRoleFileRejectsTraversal`.
- `transport_test.go` — `TestModeFromEnvMatchesTheDocumentedModes`,
  `TestNewValidatesModeAndDir`, `TestRecordThenReplayRoundTrip` (the stub's view
  of the forwarded request is byte-identical including `Authorization`, the 429
  is recorded verbatim, origin flips live→recorded, `dials==0`),
  `TestReplayMatchesIndependentOfKeyOrder`, `TestReplayQueryIsPartOfTheKey`,
  `TestReplayDuplicateKeysReplayInRecordedOrder` (third call misses),
  `TestReplayMissNamesTheKeyDigest`,
  `TestReplayOfMissingCassetteFileNeverFabricates`,
  `TestRecordRefusesWhatItCannotFaithfullyReplay`,
  `TestReplayIsConcurrencySafe`, `TestLiveModePassesThroughAndLabels`,
  `TestOriginHelpers`, `TestFromEnvWiresSecretsAndMode`.
- `record_replay_test.go` —
  `TestRecordAgainstGatewayThenReplayOffline` (httptest fake gateway → record →
  server closed → replay identical `Text`/`Usage`/`Model`/`FinishReason`/
  `RequestID`, origin `recorded`, zero dials), `TestReplayCommittedCassette`
  (the checked-in cassette, offline, plus an unrecorded request missing),
  `TestGenerateCommittedCassette` (skipped unless `GN_GENERATE_TESTDATA=1`).
- `internal/providers/openai/cassette_test.go` —
  `TestGatewayAndCassetteProduceIdenticalResults` (each recorded interaction
  served by an httptest gateway and by the cassette: every normalised field
  equal), `TestCassetteMissSurvivesAdapterClassification`,
  `TestTransportClassifiedErrorIsPreserved`.

Live (conditional, docs/spec/12 "Contract" row),
`internal/providers/recorded/live_test.go`, skipped with
`NOT RUN: set GAME_NARRATOR_GATEWAY_URL …` when the gateway or token is absent:
`TestLiveRecordThenReplayTextChat`, `TestLiveRecordThenReplayVisionChat`.
Verified against the real gateway on 2026-10-16 (endpoint and key supplied by
environment only — deliberately not recorded here):

```
TestLiveRecordThenReplayTextChat    role=fast_reasoning      attempts=1 text="pong"   usage=15/24 reasoning_tokens=21 cassette_bytes=1249
TestLiveRecordThenReplayVisionChat  role=vision_ocr_fallback          text="red blue" usage=94/63 image_sha256=2c0c15025ac074e4558…
```

Both record into `t.TempDir()`, replay offline with a `Base` transport that
fails the test if dialled, and assert the cassette contains no gateway host, no
`http(s)://`, no `Bearer ` run, no `base64,` and no token value, and that every
line is labelled `"origin":"recorded"`. Recording tolerates up to 3 attempts on a
*retryable* failure (AGENTS.md rule 11) — a capacity blip is recorded as its own
interaction and replayed in order, as observed on the first attempt at these
tests (`429 collective_error: backend at capacity`), which is why the recorded
run and the replay both replay the same attempt sequence.

Commands:

```
go vet ./... && go test -race ./...
go test -race -count=1 -run Live -v ./internal/providers/recorded/
grep -rlE 'https?://|Bearer|base64,' internal/providers/recorded/testdata   # no matches
gofmt -l .
```

## Migration / rollback

Purely additive: `internal/providers/recorded/`, one test file and a three-line
guard in `internal/providers/openai`'s `transportError`, this ADR. No schema, no
API, no call-site change; unset `GAME_NARRATOR_PROVIDER_MODE` (the default in
every existing deployment and test) means behaviour is byte-identical to
ADR-0009. Rollback is deleting `internal/providers/recorded/`,
`internal/providers/openai/cassette_test.go` and that guard; the pre-existing
hand-recorded fixtures in `client_test.go` still cover the adapter's schema
handling on their own.

## Follow-ups

- Wire `recorded.Origin` into the decision/`text.observed` event trace and the
  evidence bundle manifest (`model-calls.jsonl.redacted`) so a report line states
  `origin=recorded|live` per stage (docs/spec/12 evidence bundle), and show it in
  the developer inspector.
- Surface provider mode in `internal/doctor` / preflight output: a run against a
  real gateway must be visibly distinguishable from a cassette run in the UI.
- Binary sidecars for TTS/ASR (content-addressed WAV blobs + raw-bytes cassette
  entries) and streaming/SSE support — required before docs/spec/12's
  voices/TTS recorded modes; own ADR, and revisit the taxonomy then.
- Consider a `KindCassetteMiss` (with `Valid()`/`RetryableByDefault()` and router
  policy) if the router must degrade differently for a missing fixture than for a
  model that cannot serve a capability.
- Fixture hygiene tooling: `make` target to regenerate a cassette from a named
  scenario, plus a scanner that fails CI if any committed `.jsonl` contains a
  host, token or base64 payload (the grep above, formalised).
- Frame-replay integration: the same dir layout keyed by role will hold the
  recorded Clef/VLM sidecars for captured-screenshot fixtures (docs/spec/11
  fixture 3, docs/spec/12 "Frame replay").
