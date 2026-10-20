# ADR-0087: First-run wizard (G9) and the diagnostics API surface (G10)

Date: 2026-10-20
Status: accepted
Owners: g9-g10-setup-diagnostics worker
Related requirements/tests: docs/spec/09 ("`/setup` first-run wizard: detect device, configure auth/storage/gateway roles, run test audio, review data/privacy settings"; "`/diagnostics`: video input modes, power/environment warnings, CPU/RAM/storage, network and each model role, voice registry replication, speaker test, system logs (redacted)"), docs/spec/13 ("Diagnostics page and CLI"), gap audit rows G9/G10 (`docs/status/gap-audit-2026-10-19.md`), ADR-0074 (audio device selection, test tone, the labelled-synthetic audio seam), ADR-0078 (capture device API, the labelled-synthetic capture seam), ADR-0082 (active-picture window: per-frame, developer-trace only), ADR-0042 (support-bundle redaction — the redactor this ADR reuses for logs), AGENTS.md rules 3/5/7/9.

## Context and constraints

The gap audit (G9, G10) found the wizard shipping only the admin-claim step and the diagnostics page rendering a slice: "model-role health and the voice registry are not exposed by any endpoint in this build" (the page's own words). The build since then gained the capture device API (ADR-0078), the audio device/test-tone surface (ADR-0074) and the doctor's per-role provider probes, so every wizard step and nearly every diagnostics section now has a real backing endpoint — except three things no endpoint answers at all:

1. **Host vitals** — CPU load, RAM, storage totals and thermal state exist nowhere in the API (temperature only in the doctor CLI's capture check and hwsmoke).
2. **Model-role reachability with latency** — the doctor's role probes (the content probe, the model-list check, the measured answer latency) run only in the `game-narrator doctor` CLI process. The scope table already reserved `POST /api/v1/diagnostics/run` (operator) and `GET /api/v1/diagnostics/{id}` (viewer) for a later milestone; the routes were promised, not built.
3. **Recent system logs** — the process logs JSON to stderr (the journald), with no in-process read path.

Constraints: the diagnostics page is public (ADR-0016 §3) — it must render before any account exists, and every section the API keeps behind a role must say so instead of failing (the jobs section already does this). The wizard is admin-only (its backing endpoints — `/settings`, the audio/capture writes — are admin/operator tier), and once an admin exists `/setup` must keep answering signed-out visitors with the claim redirect (route-coverage spec, step 4). A diagnostics page or wizard that invents a value nobody probed is the defect docs/spec/13 writes about; every section therefore carries an honest UNAVAILABLE/NOT CONFIGURED state. Run state must be bounded (AGENTS.md rule 7): the role probes talk to real gateways on budgets of up to five minutes each, so a run store that grows per-click or leaks per-run goroutines is a resource leak on a 4 GB box.

## Decision

### 1. The host vitals endpoint (public)

`GET /api/v1/diagnostics/host` — a system-info-class read (docs/spec/08: public routes carry "no game content, no paths and no secrets"): CPU cores and the load averages, RAM total/available, the data directory's free and total bytes (base name only, the `settingsStorage` rule) and asset-store byte total, the thermal zones read from `/sys/class/thermal` where present, and the x86 CPU thermal-throttle counters summed where present. A bounded warning list derives from fixed thresholds (free space < 1 GiB, 1-minute load above the core count, a zone at or above 85 °C, available RAM below 10 %). Every unmeasurable part answers null with the fact stated (`available: false`, `known: false`): a CI host with no thermal zones renders "no thermal zones exposed by this host", not an empty table that reads as "all cool". A new `public` grant in the scope table sits with the other system-info-class reads.

### 2. The diagnostics run: the doctor's checks, not a copy

`POST /api/v1/diagnostics/run` (operator — the reserved grant) accepts `{"roles": ["fast_reasoning"]}` (zero to seven valid role names; empty means the host snapshot only) and answers 202 with a `run_id`. The run executes out-of-band under `context.Background` with the doctor's own per-check deadlines and a 30-minute overall cap: the host snapshot of item 1, then each requested role's probe. `GET /api/v1/diagnostics/{id}` (viewer — the other reserved grant) answers the run's current shape: `running` with whatever has landed, `done` with the whole, 404 for unknown or expired ids.

The per-role probe is `doctor.ProbeRole`, a new export on the doctor package that runs exactly the CLI's `providers.<role>` check for one role — same role-config loading, same probe-budget rule (the decision role's 20 s clamp, the vision role's longer budget), same failure taxonomy and redaction (`roleFailure`, `safeText`). The API owns only the HTTP concerns: role-name validation, the run store and the view mapping. A second client in `internal/api` that answered "pong in 412 ms" would have drifted from the doctor's content probe the way `RoleConfig.Status()` drifted from the adapters (the `settingsRoleState` comment names the incident); one probe spelling is the rule the capture and audio surfaces already follow.

Run state is an in-memory, mutex-guarded store bounded to the 32 newest runs with a one-hour post-completion TTL, evicted lazily on create and read — no goroutine, no disk: the result of a probe nobody polls is not appliance state, and the durable-job queue is for work that must survive a restart (a probe does not; the operator re-clicks). A role row carries the doctor's status (OK / FAIL / NOT CONFIGURED / DEGRADED), its summary, detail and remedy, its measured `duration_ms` and the role's `configured` flag — the latency the spec names, as the probe's own measured duration, never a guess.

### 3. Redacted recent logs (developer)

`GET /api/v1/diagnostics/logs?limit=` (developer — granted before the `{id}` grant, since a concrete path must beat the template): the last `limit` (default 200, capped 1000) records of a new bounded in-process log ring (`internal/logbuf`, capacity 500, AGENTS.md rule 7). `cmd/game-narrator` composes the existing stderr JSON handler and the ring with a two-handler fanout, so journald keeps every line and the ring keeps the tail. Every message and attribute value passes through the support bundle's redactor (`support.RedactText`: URLs, tokens, private IPs) before it answers — the same redaction ADR-0042 applies to the bundle, because a log line that names a gateway host or a private IP is an operational detail, and AGENTS.md rule 5 is absolute. The section is developer-tier like the dev-trace evidence route: system logs can carry provider request detail, and docs/spec/14 keeps that class behind the explicit role.

### 4. The diagnostics page (G10)

`/diagnostics` keeps its build/readiness/job-queue sections and gains one section per spec item, each backed by the endpoint that answers it, each with its anonymous state:

- **Video input** — `GET /api/v1/capture/devices` (ADR-0078): the negotiated mode (w × h, fps, pixel format), its provenance (saved vs policy) and the device state are the "current negotiated format/fps, signal status". The ADR-0082 active-picture window is named exactly where it lives: per-frame in the developer trace of a running session — the section says so when no session is running, instead of drawing a number out of the air.
- **Power/environment** — the host endpoint's thermal zones, throttle counters and warnings; the USB link state and downgrade flag come from the capture section's device rows (the dongle's negotiated link, ADR-0076).
- **CPU / RAM / storage** — the host endpoint's cores, load, RAM and storage blocks.
- **Network and model roles** — the diagnostics run: the signed-in operator sees a per-role "probe" control and the last run's status, latency and remedy per role; the anonymous visitor sees the section's sign-in state, the same rule as the jobs section.
- **Voice registry replication** — `GET /api/v1/voices` (viewer): the revision counts by state and, per voice, the per-instance registration rows. The note states the build's fact: one appliance, no cross-node replication; a "registration" is a voice's row on a TTS Base instance, and the counts are that state.
- **Speaker test** — `POST /api/v1/audio/test-tone` (ADR-0074): a consent-gated "play the test tone" control and its answer (sink, device, duration; the filesink's evidence file when there is no audible output).
- **System logs** — the logs endpoint behind the developer role; the other roles see the state, not an error.

### 5. The wizard (G9)

Once an admin exists, `/setup` serves the wizard to a signed-in admin (the claim form keeps its existing contract for unclaimed appliances; the signed-out redirect to `/login` keeps its route-coverage contract). Six steps, each reading live state from the endpoints above — nothing is cached or believed:

1. **Devices** — the capture device list (selection persisted through ADR-0078's PUT when the operator chooses) and the audio output list (selection persisted through ADR-0074), each with its NOT CONFIGURED / no-device state; the synthetic seams label themselves, as in the Settings page.
2. **Storage** — the settings storage section (data directory base name, free space, asset store) with the host endpoint's storage warning.
3. **Models** — the settings gateway presence and role table, each row with a **Test** button that runs the diagnostics run for that one role and reports the doctor's answer and its measured duration.
4. **Audio** — the selected output and the consent-gated test tone (the "run test audio" step), with the filesink evidence note.
5. **Data & privacy** — the review: local-only appliance, no telemetry and no auto-update (the settings update section's fact), where the data lives, the backup path, and the voice-consent model — presented for review, with a confirmation checkbox.
6. **Done** — the per-step states (CONFIGURED / NOT CONFIGURED / skipped) and the finish button.

Resumability is the URL (`/setup?step=models`): every step re-reads live state on entry, so a refresh or a re-visit lands on the step with fresh facts, and no step's "done" is ever a stored claim — the finish button leaves the appliance, the wizard stays open at `/setup` whenever an admin wants it again. Skippable steps are honest: a step whose backing answer is NOT CONFIGURED or empty renders that state with the operator action and remains skippable; nothing is presented as configured because the user moved on.

## Alternatives considered

1. **A synchronous role probe in the request path** — rejected: the vision role's budget is three minutes, the agent_reasoning role five; a request that holds that long is a wedged connection in every client, and the reserved `run`/`{id}` pair is the shape the scope table already promised for exactly this.
2. **A second, API-owned model probe client** — rejected: it would be the second spelling of the doctor's content probe (drift risk the settings page already paid for), and AGENTS.md rule 3 counts an API that answers "model fine" from a different probe than the CLI's as a claim nobody can audit against `doctor`.
3. **Journal/tail-based logs** — rejected: the journald is host-owned and varies (container hosts, dev checkouts without a journal); the in-process ring is the one spelling that exists on every deployment, and the support-bundle redactor already defines what "redacted" means here.
4. **A persisted wizard-completion flag (an `app_settings` row)** — rejected for this milestone: the wizard's facts are live and re-derivable on every entry, and a stored "done" is a claim the appliance cannot re-verify (a dongle unplugged after the wizard would leave the flag lying). Resumability via the step query parameter gives the spec'd behaviour without the new state; a completion flag can still land if the product needs one.
5. **A new voice-registry summary endpoint** — rejected: `GET /api/v1/voices` already carries the registration rows the section renders; a summary endpoint would be a second reader of the same rows with one less grant to review.

## Consequences and risks

- The scope table's two reserved `/api/v1/diagnostics` grants go live as documented, plus one public host read and one developer logs read; `TestEveryRegisteredRouteIsCoveredByTheScopeTable`, the scope-table↔contract test and the route coverage keep the three from drifting.
- A diagnostics run that probes every role can take the sum of the role budgets (minutes); the page and the wizard say "running" and poll — the run is never faked into finishing, and the 32-run bound plus lazy TTL eviction keeps the box's memory flat under click-happy operators.
- The log ring is process-local: a restart clears it, and the ring never replaces the journald (the fanout keeps both). A 500-record cap is a tail, not an archive — the support bundle remains the durable redacted log path.
- The host endpoint exposes load/RAM/temperature to an unauthenticated caller; the exposure class is the same as `system/info` (host capacity, no content, no paths, no secrets), and the warnings are fixed-threshold text, never derived from game data.
- The wizard is admin-only by the endpoints it touches (settings, audio, capture, probes); a non-admin signed into `/setup` sees the admin-required state, and an operator who is not an admin gets the same — the page checks the role the API enforces, so the two cannot disagree.

## Evidence and test plan

- Go: `internal/api/diagnostics_test.go` (host report shape on a fabricated sysfs/proc tree via the package's seam fields; the run lifecycle 202 → running → done with a scripted doctor probe; role-name validation 400; unknown run id 404; the log ring's redaction and cap, and the 503 unwired-ring path), `internal/doctor` probe test (ProbeRole answers the same Check shape the CLI's providers row does, NOT CONFIGURED for an unset role, the decision clamp), `internal/logbuf` ring test (cap, concurrency, Tail ordering). `GN_GATE=test-unit go test -race` on the touched packages; `gofmt`/`vet` clean.
- Contract: the four operations in `openapi.json` (host, run, `{id}`, logs) with `x-required-role` matching the scope table; `openapi_contract_test.go` green; `schema.d.ts` regenerated (`pnpm gen:api`) and typecheck green.
- Playwright: `diagnostics.spec.ts` extended on the public dev-server project (every new section renders with its honest state; host numbers assert against the live host), a new `setup-wizard.spec.ts` on a new project whose server carries the ADR-0074/0078 synthetic seams plus a scripted model gateway (claim → login → every wizard step's visible state: the labelled-synthetic device lists, the storage fact, the per-role Test answer from the scripted gateway, the test tone's filesink evidence, the privacy review, the done summary), and the a11y spec's route matrix gains the signed-in wizard state (AGENTS.md rule 9). Real-dongle and real-gateway runs remain NOT RUN per the gap audit's evidence classes.

## Migration / rollback

All additive. Revert: remove the four routes from `Handler()` and their contract entries, delete `internal/api/diagnostics.go` and its tests, drop the two scope-table grants and the `WithLogRing` option (the stderr handler stands alone again), delete `internal/logbuf`, the `doctor.ProbeRole` export, the wizard step in `Setup.tsx` (the claim form is the pre-ADR state) and the new page sections. No schema, store or event change; no `app_settings` key is written by this ADR.

## Follow-ups

- G17 (metrics surface) is the natural next step for a durable, queryable time series of the counters this page reports as instantaneous; the host endpoint is the template for what that surface renders.
- A wizard-completion flag and a "hardware verified" badge are candidates once the dongle runs land (G1/G2 NOT RUN rows), so the done step can distinguish "reviewed" from "verified on this hardware".
- The active-picture window section can upgrade from "named gap" to live data if the dev-trace asset route ever surfaces the last settled window outside a running session; until then the ADR-0082 note stands.
