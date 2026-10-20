# ADR-0106: Telemetry surface — bounded metrics and the /metrics scrape (gap G17)

Date: 2026-11-05
Status: accepted
Owners: gap G17 (metrics and telemetry surface)
Related requirements/tests: docs/spec/13 ("Metrics": "Aggregate by game/profile
revisions but avoid high-cardinality trace IDs as metric labels", "no generic
success on an unprobed service"), docs/spec/01 (system architecture: the
`internal/telemetry` package beside the pipeline and the API), docs/spec/14
(Authorization: the scrape is an unauthenticated read of appliance internals if
ungated), ADR-0074 (audio underruns and output latency: the definitions the
playback counters use), ADR-0100 (the warm OCR pool: the OCR stage whose calls,
cancels and fallbacks this surface counts), ADR-0102 (the status snapshot: the
separate, lower-cardinality surface this ADR does not replace). Tests
`TestGoldenExpositionSmall`, `TestProductCatalogComplete`,
`TestCardinalityBound`, `TestLabelSanitization`, `TestNilSafeHooks`,
`TestNegativeDuration`, `TestConcurrency` (internal/telemetry, `-race`);
`TestMetrics*` (internal/api: the auth gate and the two scrape endpoints).

## Context and constraints

docs/spec/13 requires a metrics surface: the hot-path counters and stage
latencies the operator and the diagnostics/health pages need, exposed for a
scraper and for the appliance's own UI. The same spec names the cardinality
rule that makes a metrics surface safe on the 4-core, 4 GB UNO Q it targets:
aggregate by game and profile revision, and never a per-message, per-utterance
or per-trace label. A label that carries a message id would grow a series for
every line of dialogue the box narrates — the exact unbounded-growth failure a
bounded-appliance product must not ship.

The pipeline already keeps per-run counters and a bounded latency table
(`internal/pipeline/stats.go`, mutex-guarded, the same shape the session's
commit→voice sampler uses). Those are per-run and in-process: they answer "what
did this run do" for the session's terminal event and the bottlenecks page, and
they are discarded with the run. What is missing is a *process* surface — one
registry the whole process books into, that outlives any single run, that a
monitor can scrape in Prometheus text format, and that the diagnostics and
health pages can read as a structured snapshot.

Constraints:

- **No new heavy dependency.** The release build is `CGO_ENABLED=0` pure Go and
  the appliance has no package manager for a metrics client library. The
  Prometheus text exposition this product needs is `HELP`/`TYPE` lines plus
  labelled samples; a hand-rolled writer is a few hundred lines against a
  dependency (and its transitive closure) the 4 GB box would carry for a format
  it renders once per scrape.
- **Bounded by construction.** The surface must not be able to grow past a
  fixed memory cost no matter what a caller passes, because the label values
  (a game id, a profile revision) are not the process's own to bound at the
  call site.
- **Off by default, and gated.** A raw metrics stream on a LAN is an
  unauthenticated read of the appliance's internals (docs/spec/14). The scrape
  is admin-tier, and the surface is inert until a process explicitly wires a
  catalog.
- **Nil-safe hooks.** The pipeline, the speech wiring and the job store are all
  built without a catalog in tests and in processes that do not wire one; the
  documented "off" shape is "the same call site, no booking", so wiring a
  catalog is a pure addition that cannot change an unwired process's behaviour.

## Decision

1. **A new `internal/telemetry` package owns the process registry and the two
   renderings.** A `Registry` holds declared metrics in declaration order; each
   metric is a counter, a histogram or a gauge. `WritePrometheus` renders the
   Prometheus text format (a hand-rolled, deterministic writer: metrics in
   declaration order, series sorted by label values, histogram buckets
   cumulative with a `le="+Inf"` total, `_sum`/`_count` rows); `Snapshot`
   renders the same data as a JSON structure for the internal consumers. The
   two surfaces read the one registry, so they cannot drift. Stdlib only.

2. **Cardinality is bounded by construction, on three axes.** (a) A metric
   declares its label dimensions at declaration time; the product catalog
   declares exactly two — `game` and `profile_revision` — and nothing else
   exists to label a series. (b) Every label value is sanitised to a safe
   charset (`[A-Za-z0-9._-]`, anything else `_`) and cut at 64 bytes, so an
   untrusted value can widen a label but never break the exposition's syntax or
   grow a series identity. (c) Each metric keeps at most `maxSeries` (64)
   distinct series; a label set arriving past the bound folds into one reserved
   `__overflow__` series instead of being dropped, so the count is never lost
   and the table never grows past its bound. The fold is what makes the bound
   safe rather than lossy: a late or hostile game still books, into the
   overflow series.

3. **The product catalog is the spec's named counter list, no more.**
   `NewProduct` declares the full set at process start so the exposition is
   complete from the first scrape: frames delivered/dropped; OCR calls,
   duration, cancels, errors and fallbacks; matcher rankings, stale and
   abstained; decisions total, by verdict (answered / provider_error /
   not_configured), applied, fenced and with latency; TTS cache hits/misses and
   duration; playback underruns; prewarm hits; frame-to-voice; and the two
   process-wide gauges, `job_queue_depth` and `active_sessions`. What is
   deliberately *not* here is a per-message, per-utterance or per-trace label:
   that cardinality is the failure mode docs/spec/13 names, and the developer
   UI gets those detail rows from the event log and the dev traces.

4. **The hooks are nil-safe and the "off" shape is a nil catalog.** Every hot
   path calls a method on a `*telemetry.Product`; a nil product is the
   documented off shape, so a harness or a process that wires no catalog keeps
   the same call sites and pays nothing. The pipeline books frames, OCR,
   matcher and decision samples under the run's label pair (the game the
   profile's edition belongs to, resolved once at session start from the
   store, and the profile revision); the speech wiring books TTS, playback
   underruns and prewarm hits under the same pair; the job queue depth is read
   from the durable queue at scrape time (the database is the authority on job
   state, and crash-recovery handoff moves jobs between states outside the
   enqueue/complete paths, so an in-process counter would drift); the active
   session count is moved by the runtime manager at registration and
   deregistration. The pipeline's own per-run stats and the session's
   commit→voice sampler are untouched: they answer the per-run question this
   surface does not.

5. **The frame→voice interval is booked across the two packages by an anchor,
   not a shared clock domain.** The pipeline, on a commit it applied, records
   the committing line's frame-entry time keyed by the dialogue occurrence id
   in a bounded table (its own lock, reset rather than grown at its bound — the
   same honest shape the session's sampler takes). The speech wiring's
   audio-start event (which carries the same occurrence id) completes the
   interval on the telemetry surface. The anchor is keyed by occurrence id
   because that is the identity the two halves already share; a line whose
   commit carried no anchor (telemetry unwired, an unparseable payload, or a
   table reset) books nothing, which is the honest "not measured".

6. **Two endpoints, one data source.** `GET /metrics` serves the Prometheus
   text format for a monitor. It sits at the root, outside `/api/v1`, so it is
   not in the route table the scope table and the OpenAPI contract track, and
   the standard authorisation middleware skips it (non-`/api/` paths pass
   through); its own gate is enforced inside the handler: an admin principal
   (the usual session cookie or API token, resolved by the same `identify` the
   `/api/` routes use) or a static bearer token when one is configured
   (constant-time compare, for a monitor that does not want to mint a user
   token). It is off by default: a server without a wired catalog answers 404,
   so the absence of a scrape target is visible to the monitor instead of the
   SPA answering for it. `GET /api/v1/dev/metrics` serves the JSON snapshot for
   the diagnostics and health surfaces; it sits under `/api/v1/dev`, so the
   standard middleware enforces the scope table's developer grant, and it is
   the lower route for the same data inside the normal ACL. Both refresh the
   job-queue gauge from the durable queue just before rendering.

7. **Wiring is one option per process.** `api.WithMetrics(product, token)`
   wires the scrape endpoints; `runtime.Options.Metrics` wires the pipeline and
   voice hooks; the process (cmd/game-narrator) builds one `NewProduct()` and
   passes it to both. A process that wires nothing behaves exactly as before:
   the hooks are nil no-ops and the endpoints answer 404/503.

## Alternatives considered

- **The official Prometheus client library (`prometheus/client_golang`).**
  The maintained path with a battle-tested exposition. Rejected: it pulls a
  non-trivial transitive closure (procfs, common, model, protobuf for the
  remote-write and pushgateway paths this product does not use) onto a
  `CGO_ENABLED=0` pure-Go release for a 4 GB appliance, for a format this
  product renders once per scrape and only in two shapes (text for a monitor,
  JSON for its own UI). If the product later needs push-gateway, remote write or
  the library's collector ecosystem, this is the decision to reopen, with the
  dependency ADR that rule requires. The hand-rolled writer is the same data
  the snapshot uses, so the two surfaces cannot drift on a format change.
- **A per-message or per-utterance label set (the "rich" metrics a developer
  wants).** The exact cardinality failure docs/spec/13 forbids: a series per
  line of dialogue is unbounded on a long session and the memory a per-message
  label spends is the 4 GB ceiling's business to refuse. The bounded pair
  (game, profile revision) is the spec's named aggregate; the per-line detail
  lives in the event log and the dev traces, which are the surfaces built for
  it.
- **Keeping the job-queue depth as an in-process counter.** Drifts from the
  table on crash-recovery handoff (jobs move between states outside the
  enqueue/complete paths) and reads a false zero after a restart. The database
  is the authority on job state (internal/jobs' first rule), so the gauge is a
  read of it at scrape time; a scrape that cannot count keeps the last reading
  rather than reporting zero.
- **Making /metrics a public route (like /health).** A raw metrics stream
  names the game being played, the profile revision in use and the appliance's
  load — an unauthenticated read of appliance state docs/spec/14 keeps
  authenticated. It is admin-or-token, and off by default.
- **Folding this surface into the status snapshot (ADR-0102).** The status bar
  is a kiosk-visible, low-cardinality rollup on every screen; the metrics
  surface is a developer/monitor diagnostic with per-stage histograms and a
  bounded per-(game, revision) label set. Different readers, different
  cardinality, different lifetime. They share the underlying counters where
  they can but are separate surfaces.

## Consequences and risks

- The process carries one bounded registry: the catalog's fixed metric set plus
  at most `maxSeries` series per labelled metric (the overflow fold caps the
  table). Memory is a small, fixed cost independent of how long the box runs or
  how many lines it narrates.
- The two scrape endpoints are live only when a process wires a catalog; a
  default build answers 404 at `/metrics` and 503 at the developer snapshot. A
  monitor that expects the scrape must therefore wire it, and the "not enabled"
  404 is the honest signal.
- The frame→voice sample books only when the pipeline and the speech wiring are
  both wired to the same catalog and the commit's occurrence id round-trips
  through the audio-start event; a line that misses any of that books nothing
  (the summary reports fewer samples, which is the honest shape for a stage
  that was not measured).
- The job-queue gauge adds one bounded `COUNT(*)` to the durable queue at each
  scrape; a scrape interval (not a per-frame cadence) makes this negligible,
  and an unreadable queue degrades to the last known reading rather than a
  false zero.
- `/metrics` is outside the OpenAPI/scope-table contract by design (it is a
  monitor endpoint, not a versioned API route); its gate is the handler's own
  and is covered by its own test rather than the scope-table matrix.

## Evidence and test plan

- `TestGoldenExpositionSmall` (internal/telemetry): pins the Prometheus text
  format on a small registry — the HELP/TYPE pairs, the label order, the
  cumulative histogram buckets ending at `le="+Inf"`, the `_sum`/`_count` rows
  and the declaration-order metric layout.
- `TestProductCatalogComplete`: the catalog is exactly the spec's named set,
  with the right kind for each and the histogram catalog on the shared bucket
  set; the samples land where the hooks say they do.
- `TestCardinalityBound`: 200 distinct label sets fold into the reserved
  overflow series — the table never exceeds `maxSeries`, the overflow series is
  present, and the folded counts still sum to every sample booked (no count
  lost); a label set arriving after the bound is still accepted.
- `TestLabelSanitization`: a hostile label (quotes, braces, a newline, a
  100-rune value) lands in the output as a safe, bounded label, the exposition
  stays well-formed and the JSON snapshot stays valid.
- `TestNilSafeHooks`: a nil `*Product` and a nil handle are the off shape —
  every hook returns without panicking.
- `TestNegativeDuration`: a backwards clock books at zero, not dropped.
- `TestConcurrency` (`-race`): one catalog hammered by 16 writer goroutines
  while 8 readers snapshot and render the exposition; the totals are exact and
  no reader sees a torn document.
- `TestMetrics*` (internal/api): the `/metrics` gate (unauthenticated and a
  below-admin role are refused; an admin token and the configured token are
  admitted; an unwired catalog answers 404) and the developer snapshot
  (developer admitted, a lower role refused, an unwired catalog answers 503).
- Full regression: `go test -race ./internal/telemetry/... ./internal/api/...`
  and `go build ./...`.

## Migration / rollback

No data, protocol or schema change. The hooks are additive and nil-safe, so a
build that does not wire a catalog behaves exactly as before. Rollback: stop
building and passing the catalog (the process wiring and the two option calls);
the endpoints revert to 404/503 and the hooks to no-ops. No migration in either
direction; the on-device binary is replaced by the next release.

## Follow-ups

- **A Prometheus scrape target on the appliance** (a `prometheus.yml` job and
  the metrics token in the operator's environment) is a deployment concern
  outside this change; the surface is ready for it.
- **Push vs pull**: the surface is pull-only (a scraper reads `/metrics`). If a
  deployment needs the box to push (no inbound scrape), that is a separate
  decision with its own ADR and, if it means the client library, the
  dependency ADR this one defers.
- **Per-game historical retention**: the registry is in-process and resets on
  restart. Durable, queryable history (the "how has frame-to-voice trended
  across sessions" question) is the event log / dev-trace domain, not this
  surface; it is out of scope here.
