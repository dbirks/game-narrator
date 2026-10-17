# ADR-0028: Onboarding agent API, durable agent.run jobs and the /games/new UI

Date: 2026-10-17
Status: accepted
Owners: agent (m3-agent-api-ui)
Related requirements/tests: FR onboarding (docs/spec/03 "onboarding agent", docs/spec/08
"Event envelope", docs/spec/09 "Routes and page requirements", docs/spec/13 "degraded
modes", docs/spec/14 "untrusted data"); tests `TestOnboarding*` (internal/api),
`TestLiveOnboardingAPI`, `web/e2e/onboarding.spec.ts`.

(Numbering follows the milestone plan; 0027 is not used.)

## Context and constraints

The onboarding agent core (ADR-0025) and its tool registry (ADR-0026) exist as a library:
`internal/agent` runs a conversation loop over a tool registry, with durable run/tool-call/
message logs, a resume worklist, and an event stream. Nothing exposed it to a user. This
milestone adds:

1. an API surface (`/api/v1/onboarding/...`) that starts, observes, steers and approves a
   run, executed as durable jobs so an attempt survives process restarts;
2. a `/games/new` page in the embedded SPA: conversational panel, tool timeline, source
   cards, quality report, draft preview, human review items and a chat box;
3. degradation behaviour when the `agent_reasoning` provider is unavailable;
4. Playwright evidence against the packaged binary with a scripted model gateway.

Hard constraints: no new Go modules or npm dependencies (React/TypeScript/Vite, the
existing REST client, react-query and the SSE event stream are the stack); publication
only through the human-approval path (ADR-0026: a `gnapp_` token, single-use, issued only
by the approval endpoint); no gateway URL/token in logs; web sources are untrusted and
rendered as text.

## Decision

### Durable execution: the run is a job

`POST /api/v1/onboarding/runs` (202) pre-creates the run row (`queued`), reuses or creates
the game/edition/draft profile, freezes a `plan` (title, platform, use_web, staged asset
sha, profile id) in the run row and enqueues an `agent.run` job (`internal/agent/job.go`,
`JobKind = "agent.run"`, payload `{run_id, profile_id, user_request, plan}`). The job
handler runs the agent loop with `CallerRole = curator`; the loop's durable state machine
is the source of truth. `GET /api/v1/onboarding/runs/{id}` is a pure snapshot assembled
from durable rows (`internal/api/onboarding_state.go`): state, steps, tool calls with
status/duration/source ids, sources (web pages flagged `untrusted`), the structured
`source_quality` reports, the draft profile view, pending review items and the redacted
conversation. SSE (`agent.step`, `tool.started`, `tool.completed`, `review.required` on
the existing `/api/v1/events`) carries the live tail; the snapshot is what a page that
missed the stream rebuilds from. A run whose latest job is queued/running rejects
messages/approvals with 409 — one attempt at a time.

Bookkeeping errors (enqueue faults, store faults) fail the job and retry it; run outcomes
(succeeded/blocked/review/failed) are *results*, never job failures — `JobResult`/
`ResultOf` in `internal/agent/job.go`.

### Lifecycle

- `POST .../messages`: appends a user turn (durable) and enqueues a resume attempt.
- `POST .../pause` / `.../cancel`: cancel the in-flight job (no-op 200 when idle); the
  run stays resumable. `/cancel` is granted to curator: it is run lifecycle, not queue
  control (the job queue stays operator-gated at `/api/v1/jobs`).
- `POST .../resume`: enqueues an attempt for a blocked/review/cancelled/succeeded run
  (409 for failed or while a job is in flight). Succeeded runs are deliberately
  resumable — the approval flow needs it.
- `POST .../approve {review_item_id}`: the publish boundary. `"publish"` issues the
  single-use token (ADR-0026), moves the draft `draft → review`, appends a user message
  carrying the token and the instruction to call `publish_profile`, and enqueues a resume.
  Tool-review items (a `review_required` tool call) are approved the same way. No other
  path can publish: `publish_profile` refuses without a profile in review *and* an
  unspent token (ADR-0026), and the UI's only publish action is this endpoint.
- Uploads (`POST /api/v1/onboarding/uploads`) reuse the catalog staging path
  (`stageAndCommit`, ADR-0014): raw bytes or multipart `file`; the response carries the
  content sha the run request names, and a run naming an un-staged hash is refused (404).

### Provider fault degradation (docs/spec/13)

The model boundary is the provider router's `agent_reasoning` role. When the role is
unconfigured or its gateway is down, the attempt lands the run in `blocked` with a clear
stop reason (`provider_unavailable`, `agent.StopReasonProviderUnavailable`) instead of
`failed`: the run is resumable and the UI's banner says so. The loop treats
`providers.ErrProviderUnavailable` (gateway 5xx after retries, breaker open) and an
unconfigured role (`KindUnsupportedCapability` with status 0) as availability faults;
other model errors (a 404 capability gap, a malformed response) remain `failed` /
`model_error` — those are permanent for the attempt, not transient. Verified by
`TestOnboardingProviderFaultDegradesAndRecovers` against a real router pointed at a
local gateway that fails with 503 and then recovers.

### Web UI (`/games/new`)

`web/src/pages/NewGame.tsx` + `web/src/api/onboarding.ts` on the existing stack. The page
holds a setup form (title, platform, web-research toggle defaulting on, optional script
upload) and, once a run exists, a two-column panel: conversation + chat on the left;
tool timeline (name/status/duration/source ids), source cards (type, uri as text, rights,
trust, and an `untrusted` badge for web pages), the whole quality report, the draft
preview and the review items (Approve/Reject) on the right. State comes from a 2 s
react-query poll plus the SSE stream (named events per `event:` field, filtered to the
run's `run_id` client-side; `Last-Event-ID` resume is the browser's native behaviour).
Rejecting a review item is recorded as a human decision in the conversation (a steer
message), not a server mutation. Every run-produced string is rendered as text children —
never `dangerouslySetInnerHTML`, never parsed (docs/spec/14). OpenAPI documents all eight
endpoints (`x-required-role: curator`); the contract test covers the wire shape.

### Evidence

`web/e2e/onboarding.spec.ts` runs on the packaged binary (`make build`) with
`agent_reasoning` pointed at `web/e2e/fake-model.mjs` — a scripted OpenAI-compatible
gateway (plus the malicious fixture page). Test (a): claim the appliance, sign in, start
a run with the staged synthetic CSV and web off, assert the import and `source_quality`
land in the timeline with the import's source id, approve the publish review item, and
find the published revision in the library. Test (b): the scripted model fetches the
local allowlisted page that instructs the model to call a non-existent tool
(`drop_database`) and to use a fabricated approval token; the spec asserts the source is
shown untrusted, no such tool call exists, nothing was published and the console stayed
clean. The private-endpoint allowlist (`GAME_NARRATOR_WEB_ALLOW_PRIVATE` +
`GAME_NARRATOR_WEB_PRIVATE_ALLOW`, dev-mode opt-out of docs/spec/14) admits exactly the
fixture host:port and is set only for the e2e server. `TestLiveOnboardingAPI`
(`GAME_NARRATOR_LIVE_MODEL`) repeats the happy path with a real model, web on, asserting
the CSV imported and no publish without approval.

## Alternatives considered

- **In-process run execution (no job)** — rejected: an attempt would die with the process
  and "resume" could not survive a restart; the queue is the appliance's only durable
  execution primitive (ADR-0003/0008).
- **A dedicated onboarding SSE endpoint per run** — rejected: `/api/v1/events` already
  filters by session and event names; a second stream would duplicate resume semantics
  (Last-Event-ID) and the hub's fan-out.
- **Approving a tool review by re-running the tool server-side** — rejected: the model
  must observe the human decision in the conversation; a silent retry would bypass the
  "reviewable voice/style selection" principle and hide the approval in the timeline.
- **Publish directly from the approve endpoint (no model round-trip)** — rejected:
  publication would then have two code paths; the token + `publish_profile` keeps
  ADR-0026's single boundary and makes the approval visible in the run's tool timeline.

## Consequences and risks

- One attempt at a time per run (409) simplifies the snapshot and the approval race at
  the cost of a brief pause between user turns; the queue's 250 ms poll bounds it.
- `blocked` is resumable by design; a permanently misconfigured role therefore parks
  runs (visible in the UI banner and the job row) rather than silently dropping them.
- The state assembler scans the `agent.run` queue for the run's latest job (the queue is
  small and bounded); this is O(queue) per GET — acceptable at appliance scale, and it
  degrades to "no job" on store faults instead of failing the page.
- The dev private-allowlist env vars must never be set on a production appliance; the
  fetcher denies private addresses without them (ADR-0018), and the e2e is the only
  place they appear.
- Bug fixed along the way (evidence: `TestAllowPrivateDevEndpoints`): `Policy.effectivePort`
  returned the Sscanf item count (1) instead of the explicit port, so a
  host:port `PrivateAllow` entry never matched an IP-literal URL at the pre-check.
  `internal/web` is now port-correct for explicit ports.

## Evidence and test plan

- `go test -race ./internal/api/ ./internal/agent/... ./internal/web/ ./cmd/...` — includes
  `TestOnboardingHappyPath`, `TestOnboardingUploadStagesBytes` (raw + multipart),
  `TestOnboardingProviderFaultDegradesAndRecovers`, `TestOnboardingLifecycleGuards`,
  `TestOpenAPI*` (contract, enums, validation).
- `pnpm --dir web check:api && typecheck && lint && fmt:check && test` — schema.d.ts is
  generated from the OpenAPI doc (gen:api) and checked to match.
- `make test-ui` → `web/e2e/onboarding.spec.ts` (two tests above) on the packaged binary.
- `GAME_NARRATOR_LIVE_MODEL=<model> go test -run LiveOnboardingAPI ./internal/api/` —
  NOT RUN unless the environment exports the model; a skip is reported, never green-washed.

## Migration / rollback

No schema change (reuses migrations 0007/0008). Rollback: drop the route registrations in
`internal/api/api.go` and the `WithOnboarding` wiring in `cmd/game-narrator/main.go`; the
`agent.run` job kind stops being enqueued and existing run rows become inert data. The
UI route and OpenAPI paths are removed in the same change; generated `schema.d.ts` is
regenerated. The loop changes (blocked-on-provider-fault, succeeded-runs-resumable) are
behaviour-preserving for the other callers (none exist yet besides the job handler) and
revert with the same commit set.

## Follow-ups

- Voice and artwork review items (ADR-0026 `design_voice` / `propose_artwork`) plug into
  the same review-item view; the UI already renders arbitrary `kind: tool` items.
- `make test-hardware`/Dolphin evidence for onboarding with game-capture sources is a
  later milestone; until then onboarding evidence is catalogue/script/web-only.
- Consider a per-run `job_id` cursor on the queue so the latest-job scan is an index
  lookup (measure first; the queue is bounded and small).
