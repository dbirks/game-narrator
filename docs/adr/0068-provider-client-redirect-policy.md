# ADR-0068: Provider-client redirect policy — refuse every hop off the configured host

Date: 2026-10-19
Status: accepted
Owners: security-audit worker (provider redirect hardening)
Related requirements/tests: docs/spec/14 ("Mandatory controls": the
model-provider redirect and wrong-TLS certificate are *rejected*, trust zone E
is "authenticated but potentially changing"), docs/spec/11 (no simulation
theatre; live tests keep the request proof), AGENTS.md rule 6 (allowlisted
tools; the inference gateway is operator configuration, not a download source),
AGENTS.md rule 7 (bounded, predictable runtime), ADR-0040 (the shared dev
gateway serves every role), `internal/providers` (redirect.go,
redirect_test.go, redirect_audit_test.go), the `openai`, `systemone`, `tts`
client constructors, `internal/providers/recorded` (evidence harness)

## Context and constraints

`docs/spec/14` accepts the product only when a model-provider redirect and a
wrong TLS certificate are rejected, not followed. The Go default `http.Client`
follows up to 10 redirects. The provider clients (openai, systemone, tts, asr)
dial a gateway the operator configured, so the *host* of that URL is trusted
configuration — but the gateway itself sits in trust zone E: authenticated,
yet mutable (a misconfiguration, a compromised box, or a MITM upstream of it
can answer with a `Location` header). A followed off-host redirect is a full
SSRF primitive: the request body (prompt, game state, uploaded audio) and,
for same-host hops, the bearer token flow to an endpoint nobody configured
(loopback services, the cloud metadata address, an internal LAN host), and the
target's response re-enters the model context as if it were the model's.

The TLS half of the spec-14 line needed no code: no client sets
`InsecureSkipVerify`, and the per-client tests pin that default.

## Options considered

1. **No guard, rely on Go's cross-host Authorization stripping.** Rejected:
   Go strips only the `Authorization` header on cross-host hops; the request
   body (prompt / game state / audio) still lands at the redirect target, and
   the response still flows into the model context. The spec-14 line is not
   met.
2. **Refuse all redirects.** Rejected: a legitimate same-host hop (load
   balancer, path move) would become a hard failure, and same-host redirects
   keep the token in the operator's own trust zone.
3. **Pre-bound host guard per client + generic origin-bound guard for the
   evidence harness.** Chosen. The client knows its configured base at
   construction, so it binds the guard there; the recorded cassette harness
   (a generic `RoundTripper` wrapper used by gates and tests) cannot know the
   base up front, so it compares each hop against the original request's host
   (`via[0]`, which `net/http` documents as "requests made already, oldest
   first").

## Decision

`providers.SafeRedirect(baseURL)` installs a `CheckRedirect` with this per-hop
policy, in one place:

- **budget**: more than `MaxRedirects` (3) hops is refused, so a same-host
  loop is a classified error, not a deadline race;
- **scheme**: only `http`/`https` targets are acceptable at all (`file:`,
  `gopher:` and friends are refused outright);
- **host+port**: the target host (default port 80/443 normalised away) must
  equal the configured host — "gateway.lan" and "gateway.lan:443" are the
  same destination, "gateway.lan:8443" is not;
- **upgrade-only**: an `https` base may be redirected to `https` (never
  downgraded to `http`, where the token would cross plaintext); an `http`
  base may upgrade to `https`.

A client built without a valid base URL refuses *every* redirect: a client
with no known configured host must not gain the ability to follow redirects.
`providers.RedirectConfinedToOrigin` applies the identical policy with the
configured host taken from `via[0]`; the recorded harness's `Client()` uses
it so live/record-mode evidence capture carries the same control.

Every provider client installs the guard in `New`, `NewFromRole` (the
timeout clone keeps `CheckRedirect` by struct copy), and `httpClient()` (the
hand-built-client fallback). A refused hop surfaces as a `providers.Error`
wrapping `providers.ErrRedirectRefused` with `Retryable = false`: the next
attempt would follow the same redirect, so retrying is pointless (docs/spec/06
retry rule). `internal/providers/asr` is in flight with another worker and
carries a deferred `t.Skip("SECURITY-FINDING GN-SEC-002")` test naming the
same wiring (docs/status/security-review.md).

## Tradeoffs

- The 3-hop budget is conservative; a legitimate multi-hop LB chain is
  unusual and, if ever needed, is operator configuration we can raise in one
  place with a test.
- Binding the host at construction means a client whose base URL changes at
  runtime must be rebuilt — true today (clients are immutable per role).
- The origin-bound guard trusts `via[0]`; a caller that hands the harness a
  request already addressed to an attacker host gets an attacker host. That
  caller is test/gate code in this repo, not a network boundary.

## Validation

- `internal/providers/redirect_test.go`: policy unit tests (off-host,
  same-host, loop budget, downgrade, other port, default-port normalisation,
  `file:` scheme, unparseable base, origin-bound variant).
- `internal/providers/redirect_audit_test.go`: acceptance shape per client —
  gateway A 302s to gateway B; the call fails with `ErrRedirectRefused` and B
  is provably never contacted. Fail-first proven: at HEAD the openai,
  systemone and tts clients all followed the hop (B's empty 200 body surfaced
  as a decode error), i.e. the test fails before the fix and passes after.
- `go test -race -count=1 ./internal/providers/...` green.

## Consequences

Any future provider client must take the guard from its constructor; the
`httpClient()` fallbacks keep hand-built clients safe. `make test-models`
evidence (recorded harness) is now redirect-safe. The spec-14 acceptance line
"model-provider redirect rejected" is demonstrably tested for the live
runtime paths (asr pending GN-SEC-002).

## Reversal plan

Remove the `CheckRedirect` assignments and the guard files; clients revert to
the Go default 10-hop policy. The spec-14 acceptance line then fails, so a
reversal must land with a spec change and the ADR marked superseded.
