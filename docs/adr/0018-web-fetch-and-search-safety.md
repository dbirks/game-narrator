# ADR-0018: Web fetch and search safety — SSRF-safe fetcher, content quoting, search providers
Date: 2026-10-17
Status: accepted
Owners: project
Related requirements/tests: docs/spec/14 "Trust zones" (D external web = hostile data), "Mandatory controls" (prompt injection, SSRF, abuse/resource), "Security acceptance tests" (source-page prompt override; web URL redirects to 127.0.0.1); docs/spec/03 tool registry (`web_search`, `fetch_page` guardrails: "SSRF/IP filtering, byte limits, redirects", "bounded queries"), "Human-in-the-loop boundaries" ("agent cannot … contact arbitrary private IPs"); docs/spec/06 "Timeout and cancellation defaults" (15–30 s per search/fetch), "Failure taxonomy"; docs/spec/12 "Agent and sources" (online search disabled; HTML scraper hitting redirect to private IP); examples/config `agent:` block (`search_provider`, `max_searches_per_run`, `max_fetch_bytes`, `web_private_ip_access`)

## Context and constraints

- The onboarding agent researches games on the web (docs/spec/03: `web_search`,
  `fetch_page`). The result URL set is *chosen by untrusted third parties* —
  search engines and fetched pages can both steer the agent. Any fetcher that
  dials whatever a hostname resolves to is an SSRF primitive: the agent would
  gain the ability to "contact arbitrary private IPs", which docs/spec/03
  explicitly forbids and docs/spec/14 lists as a mandatory control (reject
  loopback, RFC1918, link-local incl. the `169.254.169.254` metadata range,
  private DNS rebindings; follow redirects with revalidation; timeouts,
  response size and MIME limits).
- Web content is trust zone D (hostile). docs/spec/14 requires that HTML and
  web snippets "enter tools as quoted data. No privileged instructions or
  dynamic tool schemas are built from retrieved data." Acceptance test: a
  source page that attempts a prompt override must be blocked.
- The search backend must not be hardcoded to one vendor (docs/spec/03:
  "local search provider interface … rather than hardcoding any one
  vendor"). The operator requires web search to work out of the box; the
  operator's pi agent already uses Exa's keyless public MCP endpoint
  (`https://mcp.exa.ai/mcp`, streamable-HTTP MCP, JSON-RPC 2.0), so Exa MCP
  is the natural default. Self-hosted SearXNG must be supported for private
  deployments, and "disabled" must remain a first-class setting
  (docs/spec/12: "online search disabled").
- No new Go modules: `internal/web` is standard library only. `go 1.27`.
- docs/spec/06 bounds agent browsing at 15–30 s per search/fetch and gives a
  stable failure taxonomy; examples/config gives `max_fetch_bytes: 4000000`
  and `max_searches_per_run: 40`.

## Decision

### 1. `internal/web`: `Fetcher` + `Policy`

- `web.Fetcher{Policy}` with `Fetch(ctx, rawURL) (*Document, error)`.
  `Document` carries `URL` (final), `Status`, `MediaType`, `RetrievedAt`,
  `SHA256` (of the raw body), `Title`, `Text`, `Links`, and `Untrusted: true`.
  The raw body is hashed, not retained.
- **SSRF: two-layer connect-time enforcement.**
  1. *URL level* (`Policy.ValidURL`), applied to the initial URL and to every
     redirect hop (`CheckRedirect` re-validates each hop, max 5 redirects):
     schemes http/https only; ports 80/443 only unless allowlisted; blocked
     names (`localhost`, `*.local`, `*.internal` and v6-localhost aliases);
     IP literals checked directly; and *numeric-only hosts that are not
     valid IP literals* (`2130706433`, `0177.0.0.1`, `0x7f.1`, …) rejected —
     these are the decimal/octal/hex encodings browsers would interpret as
     IPs and Go's `net` stack would not, so we refuse instead of delegating.
  2. *Connect level* (the primary anti-rebinding control): the transport
     `DialContext` resolves the host via `Policy.Resolver` (injectable for
     tests; `net.DefaultResolver` in production), re-validates **every**
     candidate IP against `Policy.AddressAllowed` (deny loopback, RFC1918,
     CGNAT `100.64/10`, link-local incl. metadata, ULA `fc00::/7`,
     multicast, `0.0.0.0/8`, reserved `240/4`, unspecified; and
     IPv4-mapped IPv6 forms), and dials only an allowed address. A
     `net.Dialer.Control` backstop re-checks the literal address handed to
     the kernel as defense in depth. A public-looking name resolving to a
     private IP is refused, whether directly or after a redirect.
     Go 1.27's resolver internals are not externally injectable, so the
     *resolution itself* is the injection seam (this also means the check
     runs on the exact IPs we dial, with no resolver state we do not control).
- **Limits:** total timeout 20 s (inside the 15–30 s spec band) via context
  and `http.Client.Timeout`; raw body capped at `MaxBytes` (default 4 MiB,
  from config `max_fetch_bytes`) read through `io.LimitReader(n+1)` — an
  oversize body is a hard error (`body_too_large`), never silent
  truncation; decompressed bytes capped at 16 MiB (gzip/deflate; anything
  else refused as `unsupported_encoding`) so compressed bombs are rejected;
  MIME allowlist `text/html, text/plain, application/json,
  application/xml, application/xhtml+xml` (empty Content-Type is treated as
  `text/plain` — documented posture); HTTP ≥ 400 refused.
- `User-Agent: GameNarrator/<version> (+research)` on every request.
- **Explicit private dev endpoints.** `Policy.AllowPrivate` +
  `PrivateAllow` (exact `host[:port]`, IP, or CIDR) authorizes private
  addresses for *operator-configured* dev endpoints only (e.g. a local dev
  proxy); hostname entries authorize private IPs only, so an allowlisted
  name resolving publicly is refused. This is never enabled for agent
  browse (config `web_private_ip_access: false` is the default and the only
  mode the agent tools use); it exists so a developer can point non-agent
  tooling at a LAN fixture server without disabling the fetcher.
- **Extraction without x/net.** No `golang.org/x/net/html` (no new modules):
  a minimal streaming tag stripper (standard library only) drops
  `<script>/<style>/<noscript>/<template>/<iframe>` blocks, comments,
  doctypes and PIs; captures `<title>` and `<a href>`; decodes entities via
  `html.UnescapeString`; normalizes whitespace (incl. `&nbsp;`); caps text at
  200 KB with an explicit truncation marker.
- **Prompt-injection posture.** The fetcher never interprets content.
  `web.QuoteUntrusted(doc)` wraps the extracted text in explicit
  `[UNTRUSTED WEB CONTENT …]` / `[END UNTRUSTED WEB CONTENT]` delimiters with
  a header stating the block is third-party data containing *no instructions
  for the agent*; zero-width and bidi-control characters are stripped before
  quoting (count logged at debug). Tests assert a page whose content is
  "Ignore previous instructions…" comes back verbatim *inside* the wrapper,
  exactly once, with the header preceding the content.

### 2. `internal/web/search`: one interface, three providers

- `search.Provider { Name() string; Search(ctx, query string, opts Options)
  ([]Result{Title, URL, Snippet, Source, RetrievedAt}, error) }`;
  `Options.Max` (default 5) bounds results per query.
- **`Disabled`** returns `ErrSearchDisabled` — the explicit
  "online search disabled" state (docs/spec/12); callers treat it as
  configuration, not a retryable failure.
- **`SearXNG{BaseURL}`**: `GET {BaseURL}/search?q=…&format=json`, 1 MiB
  response cap, defensive filters (non-http(s) and URL-less hits dropped).
  For private self-hosted deployments.
- **`ExaMCP{Endpoint}`** (default `https://mcp.exa.ai/mcp`): streamable-HTTP
  MCP per call — `initialize` → `notifications/initialized` → `tools/list`
  → `tools/call`. Both `application/json` and `text/event-stream` response
  encodings are handled (SSE data lines parsed per spec); `Mcp-Session-Id`
  is carried across the request sequence. The search tool and its argument
  schema are **discovered at runtime** from `tools/list` (preference:
  configured name → default `web_search_exa` → `*web_search*` → `*search*`
  → first tool); `numResults` is only sent when the discovered schema
  declares it. The tool's text content is parsed defensively (direct JSON
  array, then JSON-array substring, then the raw text kept as a single
  fallback snippet, capped at 2000 chars).
- **`Budgeted{Provider, MaxCalls, MinInterval}`** wraps any provider:
  per-run call budget (default 40 = `max_searches_per_run`; a failed call
  still consumes the budget) and a minimum interval between calls;
  cancellation during the rate-limit wait surfaces as `rate_limited`.
  Queries and result *counts* are logged at debug — never full page text.
- **Result URLs are hostile data.** Providers keep only absolute http(s)
  URLs as a first line of defense; the authoritative check is the fetcher
  policy — `web.Policy.ValidURL` must re-validate every result URL before
  `Fetch` (enforced in tests; the live test validates all live result URLs).

### 3. Live tests

`GAME_NARRATOR_LIVE_WEB=1`-gated tests (marked NOT RUN otherwise, per
docs/spec/16 test discipline): fetch `https://example.com/` and assert the
"Example Domain" title through the default-policy fetcher; ExaMCP search for
"GameCube" must return ≥ 1 result with an https URL, all result URLs must
pass the fetcher policy, and one result URL must fetch successfully.
Provider, latency and counts are logged.

## Alternatives considered

- **Block only at the `net.Dialer.Control` layer, trusting Go's resolver.**
  Rejected: Go 1.27's resolver wire protocol is internal
  (`internal/nettrace`), so a rebinding host cannot be injected for tests,
  and we cannot guarantee which resolver state produced the address.
  Ours: the fetcher controls resolution itself, checks every candidate IP
  before dialing, and keeps `Control` as a backstop.
- **x/net/html tokenizer for extraction.** Rejected: would add a module
  (task forbids new Go modules) and a large dependency surface for a task —
  stripping hostile markup to text — that a bounded standard-library
  stripper covers. The stripper is deliberately not a parser: it degrades
  gracefully on malformed HTML (worst case: some markup survives as text,
  which is safe because it is quoted as untrusted data).
- **Hardcode Exa's tool name/arguments.** Rejected: tool names and schemas
  can change server-side; runtime discovery with a documented preference
  (and schema-gated arguments) is the MCP contract.
- **SearXNG as default.** Rejected for the default (no out-of-the-box
  backend on a fresh host); kept as the self-hosted/private option.
- **`robots.txt` enforcement in the fetcher.** Deferred (see Follow-ups):
  the spec's robots requirement applies to *imported* content acquisition
  (docs/spec/03 "Do not bypass … robots/terms enforcement"); fetch_page
  for research reads public pages, and a first-cut fetcher stays minimal.

## Consequences and risks

- The agent cannot reach private networks via search results, fetched
  links, redirects, or rebinding DNS; the SSRF acceptance tests (redirect
  to `127.0.0.1`, rebinding host, encoded IPs) are covered by
  `TestSSRFPolicyURLValidation`, `TestSSRFDNSRebindingBlocked`,
  `TestRedirectToPrivateIPBlocked`, `TestRedirectToRebindHostBlocked`.
- Residual risk, accepted: a name allowed at connect time can re-resolve to
  a different IP on a *later* connection (keep-alive reuse). Each new
  connection re-resolves and re-checks; the window is one connection
  lifetime. (TOCTOU between resolve and connect within one dial is inherent
  to any resolver-based check; the `Control` backstop narrows it.)
- Exa MCP is keyless and **sends queries to a third party** (Exa). Queries
  here are public-knowledge game research (no user secrets — docs/spec/03
  guardrail "bounded queries, no user secrets"), but this is a privacy
  property operators must accept; SearXNG or Disabled are the alternatives.
- The MCP handshake is 3 POSTs per search (initialize → tools/list →
  tools/call). Bounded by the rate limit and budget; session reuse across
  searches is a possible optimization (see Follow-ups).
- HTML extraction is approximate by design: titles/links may be missing on
  malformed pages; the truncation marker and SHA-256 of the raw body let
  the review UI show what was kept.
- Failure codes map onto docs/spec/06 taxonomy (`timeout`,
  `payload_too_large` via body caps, `unavailable`, `rate_limited`,
  `bad_schema` for MCP/tool parse failures).

## Evidence and test plan

- `go vet ./internal/web/...`; `go test -race -count=1 ./internal/web/...`
  (SSRF table incl. decimal/octal/hex IPs, IPv6 loopback/mapped, localhost,
  injected-resolver rebinding, redirect-to-private via httptest + dial-check
  hook, ports, oversize body, gzip bomb, disallowed MIME, redirect loop,
  timeout; extraction, sanitization, quoting; allowlist semantics; SearXNG
  and Exa MCP against recorded fixtures in JSON *and* SSE encodings; budget
  and rate limits).
- Live (`GAME_NARRATOR_LIVE_WEB=1`): `TestLiveFetchExampleDomain`,
  `TestLiveExaMCPGameCube` (recorded-response fixtures in
  `internal/web/search/testdata/` are reconstructed from the documented
  response shapes, not live-captured bytes — per AGENTS.md §3 they are
  *synthetic stubs / captured-frame replay*, not live evidence).
- Acceptance mapping: docs/spec/14 "web URL redirects to 127.0.0.1 →
  blocked" → `TestRedirectToPrivateIPBlocked`; "source page attempts prompt
  override → blocked" → `TestPromptInjectionQuoting`; docs/spec/12 "online
  search disabled" → `TestDisabled` + `ErrSearchDisabled`.

## Migration / rollback

- New package only; no schema, no API route, no persisted state. Revert by
  deleting `internal/web/` and this ADR; no data migration needed. The
  onboarding agent wiring (later milestone) consumes the documented
  interface, so rollback is a compile-time change only.

## Follow-ups

- **robots.txt / terms policy** for fetched sources (spec-driven, before
  any bulk import path is built).
- **Fetch cache** keyed by (URL, policy revision, content hash) to avoid
  re-fetching stable pages within a run; audio/voice caches already carry
  version keys (AGENTS.md §10).
- **MCP session reuse** across searches within a run (3 POSTs → 1) once the
  handshake cost matters.
- **Redirect allowlist for the dev proxy** when the developer-mode
  pipeline needs `AllowPrivate` (currently unused by agent tools).
- Wire into the agent tool registry (`web_search`, `fetch_page`) in the
  onboarding milestone, including `source_id` attribution and redacted tool
  traces (docs/spec/03 tool schemas).
