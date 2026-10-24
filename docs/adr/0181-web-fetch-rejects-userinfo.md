# ADR 0181: The web fetch policy rejects user:password URL components

- Status: accepted (implemented)
- Date: 2026-10-23
- Task: hygiene-audit (security pass over the SSRF path)

## Context and constraints

`web.Policy.ValidURL` is the single choke point for every URL the agent's
web fetcher touches: the initial URL of `fetch_page`/`fetch_raw` and every
redirect hop (the redirect revalidation in `internal/web/fetcher.go` calls
the same `ValidURL`). It already enforces scheme, port, private/reserved
address (including DNS rebinding), blocked names and numeric-host
encodings.

During the security audit the gap: `url.Parse` accepts `u.User != nil`
(`http://user:secret@host/`) and `ValidURL` never looked at it. Two
consequences:

1. **Credentials on the wire.** `net/http` turns a userinfo component into
   an `Authorization: Basic …` header. The fetcher's target is arbitrary
   (agent-chosen URLs, redirects under attacker control), so a URL — or a
   redirect hop — carrying credentials would send them to that host.
2. **Credentials in the log.** The fetcher's debug log records the raw URL
   (`"web fetch", "url", rawURL`), so any password in a userinfo component
   lands in the system log, which the developer diagnostics route serves
   back over HTTP.

## Options

- **A. Reject userinfo in `ValidURL`** (chosen). One check, applied to the
  initial URL and every redirect hop for free. The legitimate public web
  never needs embedded credentials; the product has no authenticated-fetch
  feature.
- **B. Strip the userinfo before dialing.** Hides the credential from the
  wire but leaves it in the debug log, and the "we fetched your page but
  dropped your credentials" surprise is worse than a clear refusal.
- **C. Redact only the log.** Leaves the wire path open (a redirect can
  still exfiltrate the Basic header) and treats the symptom.

## Decision

`ValidURL` returns a new sentinel `ErrUserinfo` (code `userinfo`) whenever
`u.User != nil`, before any port/IP checks. The error surfaces through the
existing typed-error taxonomy exactly like the other SSRF rejections.

## Validation

- `TestValidURLRejectsUserinfo` (internal/web): `user:secret@`, `user@`,
  `@host`, and malformed `a:b:c@` all return `ErrUserinfo`; a credential-free
  public URL still passes.
- `TestFetchRejectsRedirectThatAddsUserinfo` (internal/web): an origin that
  302s to a URL gaining `user:pw@` is refused with `ErrUserinfo` and the
  redirect target is never contacted.
- `go test ./internal/web/` passes; `make lint` clean.

## Consequences

- Any URL with embedded credentials is now a refusal, not a silent fetch.
  No product feature depends on it; the fetcher is untrusted-content
  browsing.
- The error code `userinfo` joins the stable tool error taxonomy
  (docs/spec/03); agent-facing surfaces that map web errors should surface
  it like the other SSRF codes.

## Reversal

Remove the `u.User` check and the `ErrUserinfo` sentinel; the two tests
above record the intended behavior and fail on reversal.
