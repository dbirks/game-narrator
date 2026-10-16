# ADR-0010: HTTP router — standard-library `net/http` ServeMux with a project-owned fallback
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: FR-12, FR-14; docs/spec/08 (`/api/v1` base, RFC 9457 `problem+json`, request correlation, `viewer`/`curator`/`operator`/`developer`/`admin` scopes), docs/spec/09 "Technology" + "Embedded binary test" (SPA fallback restricted to frontend paths), docs/spec/16 Milestone 0 ("Write ADRs for chosen Go router"); `go test ./internal/api/...`, `TestUnknownAPIPathReturnsProblemJSON`, `TestWrongMethodOnKnownPathReturnsProblemJSON`, `TestHeadOnKnownPathIsNot405`, `TestAPIPathsUnchangedByStaticServing`, `TestStaticRejectsPathTraversal`, `TestStaticDeepLinksFallBackToIndex`, `TestRequestIDHeaderOnEveryResponse`

## Context and constraints

- The service is one Go binary that serves both the versioned JSON API and the
  embedded SPA from a single port (ADR-0001, docs/spec/09 "Technology").
- docs/spec/08 requires every error, **including routing failures**, to be an
  RFC 9457 `application/problem+json` document carrying `request_id`, and
  requires the SPA fallback to be *restricted to frontend paths* — an unknown
  `/api/...` path must never receive `index.html`.
- Milestone 0 keeps the dependency surface at zero besides
  `modernc.org/sqlite` (`go.mod` has exactly one direct requirement); AGENTS.md
  treats dependencies as decisions, not defaults.
- Go 1.27 is in use (`go version go1.27.2-X:nodwarf5 linux/amd64`), so
  `ServeMux` supports method-and-pattern registration (`GET /api/v1/…`) and
  `{wildcard}` segments since Go 1.22.
- Two `ServeMux` behaviours, read from `$GOROOT/src/net/http/server.go` in this
  exact toolchain, conflict with the requirements above:
  1. **Its 404/405 bodies are plain text and cannot be overridden.** `findHandler`
     answers a wrong-method request itself
     (`w.Header().Set("Allow", …); Error(w, StatusText(StatusMethodNotAllowed),
     StatusMethodNotAllowed)`, server.go:2775-2781) and everything else with
     `NotFoundHandler()` (server.go:2782). Registering a `/` handler does not
     change either path, so no `problem+json` shape and no `request_id`.
  2. **It answers an unclean path with a 307 before dispatching anything.**
     `if path != escapedPath { … return RedirectHandler(u.String(),
     StatusTemporaryRedirect), … }` (server.go:2759-2767) runs before the
     `n == nil` (not-found) branch, so a traversal probe such as
     `GET /../etc/passwd` would be *redirected* to `/etc/passwd` instead of
     being rejected — the opposite of docs/spec/09's expectation, and a
     redirect an attacker can use to probe a server-side path.

## Decision

Route with the **standard-library `http.ServeMux`** using Go 1.22+
`METHOD /path` patterns, and own everything the mux cannot express:

1. `routeTable.register(mux, method, path, handler)`
   (`internal/api/router.go`) is the *only* place a route is added. It calls
   `mux.HandleFunc(method+" "+path, h)` and records the pair so the project can
   answer 405 correctly — ServeMux gives `Allow` but not a `problem+json` body.
2. `mux.Handle("/", s.fallback(routes))` catches whatever the mux did not
   dispatch: `routeTable.allowedMethods` produces the `Allow` header and a
   405 problem when the path is known but the method is not (GET routes answer
   HEAD too, mirroring ServeMux), otherwise a 404 problem whose `detail`
   distinguishes `/api/` misses from asset misses.
3. **The static/SPA dispatcher sits ahead of the mux**, not inside it
   (`internal/api/api.go` `Handler`): a request whose `r.URL.Path` does not
   begin with `/api/` and that no registered route claims is served by
   `serveStatic`. This is forced by constraint 2 above — a `/…` unclean UI path
   would otherwise be 307-redirected by ServeMux before any handler ran — and it
   keeps `/api/` inside the problem+json rules (docs/spec/09 "SPA fallback
   restricted to frontend paths"; `TestAPIPathsUnchangedByStaticServing`).
4. `withRequestID` wraps the whole dispatch so correlation, including on
   fallback responses, is unconditional (docs/spec/08).
5. Middleware stays plain `http.Handler` decoration. Per-route auth scopes are
   added by wrapping the handler at `routes.register` call sites, not by a
   framework middleware stack.

## Alternatives considered

- **`github.com/go-chi/chi/v5` v5.5.0** (published 2026-10-14, actively
  maintained) — closest fit: `Router` is an `http.Handler`, idiomatic
  `With`-style middleware, route groups, per-route middleware, no handlers
  re-written. Rejected for now because the *only* capability above method+path
  patterns that we would buy is the middleware chain, which is ~30 lines of
  `http.Handler` decoration, and because chi — like every router that owns the
  not-found path — still needs a custom `NotFound`/`MethodNotAllowed` handler
  and a dispatcher in front to dodge the same 307 canonicalization for SPA
  paths. Adding it later is a local change at the two files listed above.
- **`github.com/gorilla/mux` v1.8.1** — last published **2023-10-18**
  (`proxy.golang.org` `@latest`), i.e. effectively maintenance-only, and its
  regexp-based route matching predates (and is slower to reason about than)
  stdlib patterns. Rejected on maintenance risk and on capability overlap.
- **`github.com/labstack/echo/v4` v4.16.0** (2026-09-27) — a full web framework:
  its own context type, binder, renderer and error model. That model would have
  to be subverted to emit docs/spec/08's exact `problem+json` fields and it
  disagrees with ADR-0001's "one narrow Go service, no framework surface".
  Rejected.
- **Hand-rolled route table with no `ServeMux` at all** — maximally explicit,
  but we would reimplement path cleaning, wildcards, `Allow`, HEAD handling and
  the radix matching that the standard library already ships and tests. Rejected:
  strictly more code than option 3 for no gain.

## Evidence and test plan

Current surface: three registered routes, one `HandleFunc` call site — the
`routeTable.register` helper — so there is no route-framework need today.

```
$ go test -count=1 ./internal/api/...
ok  	github.com/dbirks/game-narrator/internal/api	0.004s

$ go test -count=1 -v ./internal/api/... | grep -c '^--- PASS'
25

$ go test -race -count=1 ./internal/api/...
ok  	github.com/dbirks/game-narrator/internal/api	1.022s

$ grep -c HandleFunc internal/api/*.go
internal/api/api.go:0
internal/api/api_test.go:0
internal/api/middleware.go:0
internal/api/middleware_test.go:0
internal/api/router.go:1
internal/api/static.go:0
internal/api/static_test.go:0

$ grep -c "routes.register" internal/api/api.go
3
```

Behaviour that the fallback exists to provide (all assertions live in
`internal/api/*_test.go`, run by `make test-unit`):

| Requirement | Test | Observed |
| --- | --- | --- |
| Unknown API path is a 404 `problem+json` with `request_id` | `TestUnknownAPIPathReturnsProblemJSON`, `TestUnknownNestedAPIPathReturnsProblemJSON` | 404 + `application/problem+json` |
| Wrong method on a known path is a 405 with `Allow` | `TestWrongMethodOnKnownPathReturnsProblemJSON` | `Allow: GET, HEAD` |
| GET routes also answer HEAD (not a spurious 405) | `TestHeadOnKnownPathIsNot405` | 200 |
| SPA fallback never fires inside `/api/` | `TestAPIPathsUnchangedByStaticServing` (`GET /api/index.html` → 404) | 404 problem |
| `/%2e%2e/etc/passwd`, `/../etc/passwd`, `/assets/../../etc/passwd` and 5 more are **404, not 307** | `TestStaticRejectsPathTraversal` | 404 + fixed detail |
| Extensionless UI deep links serve `index.html`, real missing files do not | `TestStaticDeepLinksFallBackToIndex`, `TestStaticMissingFileWithExtensionIsProblem404` | 200 / 404 |
| Every response, including fallbacks, correlates | `TestRequestIDHeaderOnEveryResponse`, `TestStaticRequestIDCorrelates` | header present |
| No hostname/env leak from a route handler | `TestSystemInfoLeaksNoHostOrEnv` | asserted |

Sources for the two `ServeMux` behaviours quoted under "Context":
`$(go env GOROOT)/src/net/http/server.go` lines 2743-2782 in go1.27.2, and
`$(go env GOROOT)/src/net/http/serve_test.go:512` `TestMuxRedirectLeadingSlashes`,
which asserts `StatusTemporaryRedirect` for an unclean request path.

## Consequences and risks

Positive: zero new modules, so `CGO_ENABLED=0` static cross-compiles (ADR-0008,
ADR-0012) stay a single `go build`; routing semantics are standard-library
documented behaviour for the next maintainer; `problem+json` 404/405 and the
restricted SPA fallback are test-asserted rather than hoped for; the
`routeTable` duplication is 60 lines.

Costs and risks:

- **Unclean `/api/...` paths still get ServeMux's 307.** The dispatcher only
  pre-empts non-`/api/` paths, so e.g. `GET /api/v1/health/../live` is
  canonicalized by the standard library rather than answered with a problem
  document. The redirect target is computed by Go from the cleaned path, so it
  cannot escape the API root or reach the filesystem, and no route matches a
  traversal path after cleaning. Accepted for M0; if a later security review
  demands problem+json for these too, the fix is to move the clean-path check
  into the dispatcher (compare `r.URL.EscapedPath()` and answer 404 directly).
- `routeTable` must stay in sync with the mux. It does so by construction — the
  only registration path appends to both — but a handler registered with a bare
  `mux.HandleFunc` would silently lose 405 detection. Enforced by convention and
  the `grep -c HandleFunc` evidence line above; a future lint or test that
  counts routes in both structures would make it mechanical.
- ServeMux patterns cannot express regex constraints or host routing; the
  milestones that need those (docs/spec/08 rate limiting per route group)
  implement them as handler decoration.
- 307-vs-308 and trailing-slash semantics belong to the standard library, so a
  Go upgrade can change them. Covered by the table above, which runs on every PR.

## Migration / rollback

Rollback of the router choice is contained in `internal/api`: replace
`http.NewServeMux()` with `chi.NewRouter()` (or echo), keep `routeTable`, keep
`fallback`, and move the `serveStatic` dispatcher to a `Use`/global middleware
that runs *before* route matching. No public contract changes: status codes,
`Allow`, `Content-Type: application/problem+json`, `X-Request-Id` and the deep
link behaviour are what `internal/api` tests and `web/e2e` assert, so the swap is
verified by re-running `go test -race ./internal/api/...` and `make test-ui`
unchanged. Rolling *back* to ServeMux's own 404/405 is not offered: it fails
docs/spec/08, which is why the fallback exists.

## Follow-ups

- Revisit trigger, stated in docs/spec/16 terms: when the route count grows past
  the handful of M1-M5 groups (session, game, voice, onboarding, job, dev) or
  when per-route auth scopes (`viewer`/`curator`/`operator`/`developer`/`admin`)
  and per-route rate limits make hand-decorated middleware painful at each
  `routes.register` call site, adopt chi in a superseding ADR with the same
  evidence table.
- Add a test that asserts `routeTable` and the mux agree (e.g. reflect over a
  registration helper or table-drive both) so a bare `mux.HandleFunc` cannot slip
  past 405 detection.
- Decide problem+json-vs-307 for unclean `/api/` paths during the security
  milestone (docs/spec/14) and record the outcome here.
- Register the SSE and long-poll routes (docs/spec/08 "Event streaming") under
  the same table so 405 semantics stay uniform.
- Review when Milestone 1's auth middleware lands.
