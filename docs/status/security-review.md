# Security review — HTTP surface, fetcher, file handling (security-audit task)

Date: 2026-10-19 (continuation 05:45 relaunch) · Reviewer: security-audit worker
Scope: `internal/api`, `internal/auth`, `internal/web` (fetcher), `internal/assets`,
`internal/profileio`, `internal/support`, `internal/providers` (all provider clients),
redirect/TLS acceptance line of docs/spec/14, AGENTS.md rules 5 and 6.
Method: attacker's-eye review with a failing test proven before every fix
(throwaway worktree at HEAD: the pre-fix clients demonstrably *followed* the
off-host redirect — the redirect target's response came back into the client as
a decode error, i.e. the body and context flow the spec-14 line forbids).

## Findings

| ID | Finding | Severity | Proof / test | Status |
|----|---------|----------|--------------|--------|
| GN-SEC-001 | `openai`, `systemone`, `tts` clients used bare `http.Client`s: a gateway redirect to any other host was followed (Go's 10-hop default), moving the prompt/game-state/audio body and the bearer token to an unconfigured endpoint; the target's response re-entered the model context (full SSRF primitive against the trust-zone-E gateway) | High | `redirect_audit_test.go` per client (fail-first proven at HEAD: all three followed the hop); fixed wiring in `New`/`NewFromRole`/`httpClient`; policy unit tests in `redirect_test.go` | Fixed here (ADR-0068); race-green |
| GN-SEC-002 | `asr.Client` has no redirect guard (`httpClient()` returns a bare `http.Client{}`) — same SSRF surface for the audio upload path | High | `TestASRClientRefusesOffHostRedirect` written and failing pre-fix | **Deferred**: `t.Skip("SECURITY-FINDING GN-SEC-002: …")` — `internal/providers/asr` is in flight with another worker; apply the same SafeRedirect wiring (ADR-0068) and remove the skip |
| GN-SEC-003 | Recorded evidence-harness `Client()` (live/record mode dials real gateways in `make test-models`) followed off-host redirects | Medium | `TestRedirectConfinedToOrigin` (budget, off-host, `file:`, empty-via); origin bound to `via[0]` per net/http "oldest first" | Fixed here (ADR-0068): `providers.RedirectConfinedToOrigin` |
| GN-SEC-004 | `openai.TestNewValidatesBaseURL` pinned the pre-fix contract "HTTP must stay nil", which would have blocked any future guard; the spec-14 line could not be satisfied without breaking a test | Low (contract rot) | Same test, updated | Fixed here: test now pins `CheckRedirect != nil` and `Timeout == 0` (context stays the only deadline) |

TLS half of the spec-14 line: no client sets `InsecureSkipVerify`; per-client
tests pin that default (no code change needed).

## Areas reviewed and found clean

- **API authz matrix (`internal/api` + `internal/auth`).** Single scope table
  (`auth.DefaultPolicy`) in table order, first-match, **fail-closed default =
  admin for any unmatched method+path**; `TestEveryRegisteredRouteIsCoveredByTheScopeTable`
  drift-detects any route missing from the table; the matrix test walks every
  route at every role (viewer < curator < operator < developer < admin).
  Developer payload routes (raw gameplay text, provider traces, sampled frames)
  sit under the developer-only `/api/v1/dev` prefix grant; settings/config/
  secrets/users are admin; voice-registry reads are curator-and-above per the
  spec-14 denial line. No route found reachable by a lower role than the table
  names.
- **CSRF / session (`internal/api/auth.go`, `middleware.go`).** `gn_session` is
  HttpOnly with `SameSite=Strict`; the CSRF token is a separate non-HttpOnly
  cookie echoed back only after its digest verifies against the session row,
  and is checked in a header with an Origin check (defense in depth on top of
  SameSite). Logout revokes the session server-side and clears the cookies;
  a bearer token gets a 409 pointing at token revocation instead of a fake
  200. Login is dual-bucket rate-limited (per address **and** per username)
  with uniform unknown-user/wrong-password answers and a dummy verify, so no
  user enumeration and no account-grinding across hosts. Password change
  revokes every session **and** API token of the account (including the
  caller's), clearing cookies.
- **Path traversal / file handling.** Asset store is content-addressed
  (64-hex blob names only) with explicit `filepath.Rel` escape guards
  (`..`/absolute refused) and deliberately *not* `EvalSymlinks`, so a
  symlinked path cannot be walked out of the root (`assets/local.go`,
  `safety_test.go`). `profileio.Unpack` writes only regular `blobs/` entries
  whose names are exactly 64 lowercase hex chars, verifies each blob against
  the hash its name claims before commit, size-limits the manifest, and
  ignores everything else — `../` or symlink entries cannot reach outside the
  asset root.
- **Fetcher/SSRF (`internal/web`).** Pre-I/O policy (scheme allowlist, port
  rules, blocked names, numeric-host and private-address refusal), a
  **connect-time re-resolve and per-IP re-validation** (DNS-rebinding
  defeated: the resolved address is re-checked against the policy in the
  dialer, not only at check time), explicit `PrivateAllow` for dev endpoints
  that authorizes *private* resolutions only, a redirect budget, and a total
  client deadline covering the whole redirect chain.
- **Secrets hygiene.** OpenAPI contract carries no literal tokens (structural
  references only). The support bundle redacts bearer/authorization headers,
  config values and addresses before anything is bundled. Provider clients
  mask the token and hide the base URL in `String()`/`LogValue`, and
  `transportError` keeps the raw transport string out of surfaced errors
  because it may name the endpoint. SSE events and problem+json bodies carry
  no credential material in the swept handlers.

## Residual / deferred for the orchestrator

- **GN-SEC-002 (high)** — asr redirect guard; skip marker in
  `internal/providers/redirect_audit_test.go`, one-line wiring per ADR-0068.
- Not re-verified in this pass (other workers in flight): the Dolphin bridge
  handlers' request validation and the agent tool-allowlist/prompt-injection
  surface in `internal/agent` — both listed as in-flight; recommend a follow-up
  sweep once those land. This pass's API review covered the authz table, CSRF,
  and secret-bearing response shapes, not per-field input validation of every
  handler.

## Evidence

- Fail-first: worktree at HEAD + guard files → `go test ./internal/providers/`
  shows all three live clients following the off-host redirect (failures quote
  the redirect target's empty response).
- Fixed: `go test -race -count=1 ./internal/providers/...` all packages ok;
  `go test -race -count=1 ./internal/api/... ./internal/web/...
  ./internal/assets/... ./internal/profileio/... ./internal/auth/...
  ./internal/support/...` all ok; `go vet ./internal/...` clean; gofmt clean
  for the swept packages (one gofmt fix applied in `internal/profileio`).
- ADR-0068 records the policy, options, tradeoffs, and reversal plan.
