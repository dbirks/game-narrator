# ADR-0016: Server-side sessions, PBKDF2 passwords, a route→role scope table, and loopback-only first run
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: docs/spec/08 "Conventions" (same-site httpOnly cookie, API token for the CLI, CSRF under cookie auth) + "API version and user authentication" (scopes `viewer…admin`, developer-only data, rate limits, localhost bind / LAN behind an authenticated TLS reverse proxy), docs/spec/14 "Mandatory controls: Authorization" + "Security acceptance tests" ("unauthorized viewer tries dev screenshot, voice registry and profile publish → denied", "stolen old session cookie invalidated", "no raw API key in frontend bundle"), docs/spec/10 "Security defaults for LAN deployment", docs/spec/09 (the SPA must load before anybody signs in), AGENTS.md rules 6, 7, 10, 12; builds on ADR-0008 (SQLite driver, `BEGIN IMMEDIATE`, migration splitter), ADR-0010 (stdlib `ServeMux` router — its pattern vocabulary is what the scope table reuses); `go test -race -count=1 ./internal/auth/ ./internal/store/ ./internal/api/ ./cmd/game-narrator/`

## Context and constraints

- Until this milestone the API trusted its own bind address. That was a real
  control while `--listen 127.0.0.1` was the only shape (ADR-0010), and it is not
  one any more: docs/spec/10 allows LAN exposure by configuration, docs/spec/09
  puts the developer UI on a browser on somebody else's machine, and a LAN caller
  can currently read `GET /api/v1/jobs`, stream `GET /api/v1/events`, cancel a
  job and — once later milestones register them — drive capture preview and TTS.
  docs/spec/14 names all of these: "no unauthenticated screenshot/audio",
  "Prevent unauthenticated capture preview or TTS abuse" (docs/spec/10).
- The scopes are given, not invented: docs/spec/08 lists
  `viewer`, `curator`, `operator`, `developer`, `admin` and says developer mode
  "can display sensitive gameplay text and provider traces; require explicit
  role".
- This is a **local-first single-appliance service**, not a multi-tenant cloud.
  That cuts both ways: there is no need for stateless credential validation
  across replicas, but there *is* a hard requirement that a stolen cookie can be
  killed, because the appliance holds somebody's game library, provider keys and
  voice recordings and it is reachable from a living-room network.
- Two constraints from AGENTS.md that removed the fashionable options: **no new
  Go modules** (so no `golang.org/x/crypto`, therefore no argon2/bcrypt) and Go
  1.27 in `go.mod`, whose standard library has carried `crypto/pbkdf2` since 1.24.
- One writer per database, one process on a 4 GB ARM target (docs/spec/10). Any
  per-request work must be bounded and, for auth, bounded *publicly*: the login
  endpoint is the one route an unauthenticated caller is allowed to make the box
  compute on.
- The embedded SPA must be servable before anybody authenticates (the login form
  *is* part of the SPA), so "auth before routing" applies to `/api/` paths only.

## Decision

`internal/auth` owns the domain (roles, hashing, credentials, CSRF, the scope
table, the limiter — no SQL), `internal/store` owns the rows
(`migrations/0006_auth.sql`, `auth_store.go`), `internal/api/{auth,middleware}.go`
own the HTTP surface, and `cmd/game-narrator` owns the offline administrative
entry point.

**1. Storage — `0006_auth.sql`, four tables, no raw secrets anywhere.**

- `users(id, username UNIQUE COLLATE NOCASE, password_hash, role, disabled,
  created_at, password_changed_at)` with `CHECK (role IN ('viewer','curator',
  'operator','developer','admin'))`. `NOCASE` is what makes `Admin` and `admin`
  the same account at the schema level rather than in application code that a
  later contributor can forget to call. `password_changed_at` is what makes
  "credentials older than the incident" answerable.
- `sessions(id_hash TEXT PRIMARY KEY, user_id … ON DELETE CASCADE, csrf_hash,
  created_at, last_seen_at, expires_at, user_agent, revoked_at)`.
  **`id_hash` is `SHA-256(token)` and the raw token is never persisted.** The
  token is 256 bits of CSPRNG output, so the digest is unguessable and the table
  is not a credential store — a database dump, a backup copy or a leaked
  evidence bundle (docs/spec/16) hands over nothing usable.
- `api_tokens(id, user_id … CASCADE, name, token_hash UNIQUE, role, created_at,
  last_used_at, expires_at, revoked_at)`. The role is validated at creation to be
  ≤ the owner's role, so a token cannot outlive its principal's authority — and
  at verification the *effective* role is `min(owner role, token role)`, so an
  owner demoted after the fact demotes the token with it.
- `auth_audit(id, occurred_at, user_id NULL, event, remote_addr, detail_json)`.
  `user_id NULL` is the shape of a rejection at the door: no principal was
  established. `detail_json` is built from usernames, ids, roles and counts by
  the call sites — there is no code path that hands it a password or a token, and
  `TestAuthStoreAuditShapeAndSecretHygiene` fails if one is added.

**2. Passwords — PBKDF2-HMAC-SHA256, 600,000 iterations, upgradeable envelope.**
`pbkdf2sha256$<iter>$<b64salt>$<b64key>` — `crypto/pbkdf2` with a 16-byte random
salt and a 32-byte key; verification re-derives with the *stored* iteration count
and compares with `subtle.ConstantTimeCompare`. The envelope carries its own
parameters, so the work factor is a policy value (`PBKDF2Iterations`, or a
deployment's `WithPasswordCost`) and not a schema value: `NeedsUpgradeTo`
compares a stored hash against the service's current cost and `Login` re-derives
and rotates the row on a successful match (`password.upgraded` in the audit log).
A password file written at 100k iterations is therefore repaired by ordinary use,
with no migration and no forced logout. Minimum length 12, maximum 256 — the
floor is OWASP's, the ceiling exists because the cost of a KDF is a function of
the input size and an unbounded password is an unauthenticated way to buy CPU at
login time.
A stored hash may only ask for iterations inside `[1_000, 5_000_000]`
(`minStoredIterations`/`maxStoredIterations`): the envelope is attacker-visible
data for a user who has written one, and a hostile or corrupted row must not be
able to request 10¹⁰ iterations from the login path.
`dummyVerifyPassword` runs the KDF against a fixed dummy hash when the username
does not exist, so a wrong username and a wrong password cost the same and take
the same time — `TestLoginFailuresAreUniformAndCannotEnumerateAccounts` asserts
both the identical body and the absence of any user-conditional fast path.

**3. Roles and the scope table — one table, fail-closed default.**
`auth.Role` is an ordered integer (`viewer < curator < operator < developer <
admin`, plus `RoleNone` = "no principal"), so an authorization check is
`role.AtLeast(min)` with no nil case and no per-route closure. `DefaultPolicy()`
is a declarative `[]Grant{Method, Pattern, Access}` in `internal/auth/policy.go`
using `net/http`'s own pattern vocabulary (ADR-0010), so the grant language and
the routing language cannot drift apart. The table is written specific-first,
prefix-last, and `Policy.Require` returns the first match.

| Route family | Minimum role | Why |
|---|---|---|
| `GET /api/v1/health/*`, `GET /api/v1/system/info` | public | readiness and `doctor` must work before there is an account; build info only, no paths, no secrets |
| `GET /api/v1/auth/status`, `POST /api/v1/auth/login`, `POST /api/v1/auth/setup` | public | they cannot be guarded by the credential they produce; setup is additionally loopback-only *in its handler* |
| `GET /api/v1/auth/me`, `logout`, `password`, `/api/v1/jobs`, `/api/v1/jobs/{id}`, `/api/v1/events`, `GET /api/v1/games*`, `GET /api/v1/voices{,/{id}}` | viewer | the read-only library and the operational reads a viewer page paints |
| writes under `/api/v1/games`, `/api/v1/profiles`, `/api/v1/sources`, `/api/v1/scenes`, `POST /api/v1/corrections`, `/api/v1/import/*`, `/api/v1/onboarding/*`, most `/api/v1/voices/*` (approve, auditions, registrations) | curator | docs/spec/14 makes publication a deliberate, rights-reviewed, role-gated act |
| `/api/v1/sessions*`, `/api/v1/capture/*`, `/api/v1/audio/*`, `/api/v1/frame-sources/*`, `POST /api/v1/jobs/{id}/cancel|retry`, `POST /api/v1/voices/{id}/register`, `POST /api/v1/diagnostics/run` | operator | control of the physical appliance; registering a cloned voice names an upstream endpoint, which is not library data |
| `/api/v1/dev/*` (any verb), raw provider traces, developer-only events | developer | docs/spec/08's "require explicit role"; docs/spec/10's "never ship them active on UNO Q without auth" |
| `/api/v1/users*`, `/api/v1/config*`, `/api/v1/secrets/*`, `/api/v1/tokens/*` | admin | accounts, endpoint configuration and sealed keys |
| **anything else under `/api/v1/`** | **admin** | fail closed |

The default is a property of `Policy.Require`, not of the router: a path that
matches no grant requires admin **whether or not a handler exists for it**. An
unauthenticated probe of a nonexistent path gets 401, never a 404 that confirms
the path is real — route discovery is step one of attacking any service.
`TestRoleMatrixOverEveryRoute` walks ~40 routes × 5 roles,
`TestEveryRegisteredRouteIsCoveredByTheScopeTable` fails if `Handler()` ever
registers a route with no grant, and `TestUnknownRouteFailsClosedToAdmin` pins
the default on its own.

**4. Sessions — server-side, cookie-carried, two deadlines.**
32-byte token in `gn_session`; `HttpOnly`, `SameSite=Strict`, `Path=/`,
`Max-Age` = remaining absolute lifetime. `Secure` is set when `r.TLS != nil`
**or** an `X-Forwarded-Proto: https` arrived from a peer this deployment named as
a proxy in `api.WithTrustedProxy` / `GAME_NARRATOR_TRUSTED_PROXY_CIDRS`; only
`X-Forwarded-Proto` is consulted, not RFC 7239 `Forwarded`, because it is the one
hint the decision needs and the one nginx and Caddy both write.
Idle timeout 12 h, absolute 7 d, both enforced at
verification time from `last_seen_at`/`created_at` and checked *before* any
handler runs; `TouchInterval` (1 min) throttles the `last_seen_at` write so a
reconnecting SSE client does not become the busiest writer on the appliance.
Every 401 that names a dead cookie also *clears* it, so a browser stops resending
a credential the server has already revoked.
The CSRF token is minted at login, delivered in the response body **and** in a
second cookie `gn_csrf` that is deliberately *not* HttpOnly, and stored only as
`csrf_hash`. A page reads it and echoes it in `X-CSRF-Token`; the server compares
digests in constant time. `GET /api/v1/auth/me` re-echoes it (after verifying it
against the session's stored digest) so a reload is not indistinguishable from a
logout.
Changing a password or disabling an account calls one store function
(`RotatePassword` / `SetUserDisabled`) that revokes **every** session and token of
that user in the same write — that is docs/spec/14's "stolen old session cookie
invalidated", implemented as a cascade rather than as an eviction policy.

**5. CSRF — header match *and* origin match, only where cookies are the credential.**
`authorize` applies the rule when the principal came from a cookie and
`UnsafeMethod(r.Method)` (anything but `GET`, `HEAD`, `OPTIONS`). It requires
`X-CSRF-Token` to digest-match the session's stored token **and**, when `Origin`
or `Referer` is present, to equal the request host. Bearer requests are exempt:
a cross-site page cannot set `Authorization` without a preflight that the server
never authorizes, so the header *is* the CSRF token, and demanding a second one
would only push CLI and CI users into copying two secrets. API tokens are minted
for exactly that population.
`TestCSRFMatrix` covers missing / wrong / correct and the `Origin` mismatch;
`TestBearerRequestsAreExemptFromCSRF` pins the exemption.

**6. API tokens — `gn_<base32>`, printed once.** 20 base32 characters (160 bits)
after a `gn_` prefix. The prefix is a security feature, not decoration: a secret
that is recognisable at a glance gets noticed in a screenshot or a CI log, and
secret scanners can key on it (docs/spec/14's "no raw API key" covers our own
credentials too). 160 bits rather than 256 because this value is typed by humans
and pasted into config; its digest is the only thing stored, `LooksLikeAPIToken`
is a shape pre-filter so a foreign `Authorization` header is never run through a
lookup, and losing the value means minting a new one — the intended operator
experience for something that may live in a CI secret store. A token carries its
own role and expiry and can be revoked independently of its owner.

**7. First run — loopback transport rule, once.** While no admin exists,
`GET /api/v1/auth/status` reports `{"setup_required":true,"loopback_setup":<can
this caller run setup>}` and `POST /api/v1/auth/setup {username,password}`
creates the first admin — but only when `IsLoopbackRemote(r.RemoteAddr)`
(`127.0.0.0/8`, `::1`). The check reads the **transport** peer address and never
`X-Forwarded-For`: the same header that would let an attacker claim to be a
different client would let them claim to be *this machine*, which is the one
claim docs/spec/10 ("Bind localhost on initial boot") exists to make impossible.
Once an admin exists, setup returns 409 with `already-initialized`; a request
from a non-loopback address gets 403 `setup-not-local` and a
`doctor`/CLI pointer. Setup creates an account but mints **no session**: the
wizard must log in like anybody else, which proves the password is known before
any privileged route is reachable. `TestSetupFlowClaimsTheApplianceOnce`,
`TestSetupRefusalOrder` (spoofed `X-Forwarded-For` from a non-loopback peer is
refused) and `TestServeRequiresAuthenticationAndCompletesTheFirstRunCycle`
cover the ordering.

**8. Rate limits — token bucket, two keys, in memory.** Login and setup spend
from a 10-token bucket that refills over 15 min, keyed *both* by remote address
and by username, so one host cannot spray an account and one account cannot be
used to burn the whole LAN's allowance. A token bucket rather than a fixed
window: a fixed window permits 10 guesses at 14:59 and 10 more at 15:01.
Overflow answers 429 problem+json with `Retry-After`. The key map is capped
(4096): when full, expired buckets are reclaimed, and if none are reclaimable the
*new* key is refused — an evicted attacker is rate-limited, not un-rate-limited.
State is in memory because it is *abuse telemetry*: worthless across a restart,
and putting it in SQLite would make the DoS endpoint the busiest writer on the
box. The failed attempt itself is audited in SQLite either way, without the
attempted password.

**9. Event-stream confidentiality.** `GET /api/v1/events` requires viewer; on
top of that, `developerScopedEvents` filters SSE frames so a principal below
developer never receives `decision.*` or `provider.trace.*` — in replayed history
as well as live. It is a prefix **deny**-list applied at the frame boundary rather
than an allow-list compiled at subscribe time, because the event registry is
open: `internal/events.Register` is called by whichever subsystem needs a name,
and an allow-list would both silently starve a viewer of new operational events
and break when the 65th name exceeded `internal/events`' filter-list bound.
Adding a developer-only *family* is therefore a visible edit to a security table.

**10. Enforcement point and CLI.** One middleware (`Server.authorize`) answers
authorization **before** routing for every `/api/` path; non-API paths pass
through so the SPA can serve its own login form. `internal/api` never asks a
handler to check a role. `game-narrator admin create --username U --role R`
reads the password from stdin (never argv, where it would sit in `ps` and shell
history) and `game-narrator admin token --username U --name N --role R` prints a
new token once; `serve` warns at startup, and readiness reports it, when no
admin account exists.

## Alternatives considered

- **JWT / stateless bearer tokens.** Rejected. They cannot satisfy docs/spec/14's
  "stolen old session cookie invalidated" without a server-side revocation list,
  at which point they are sessions with extra steps and a larger blast radius (a
  leaked JWT is self-validating until `exp`, and `exp` is 7 days here). They also
  put the role — our authorization input — inside a value the client can read,
  cache and send to a third-party analytics script. A single-appliance service
  with one writer to one SQLite file has no horizontal-scaling argument to trade
  against losing immediate revocation.
- **argon2id / bcrypt / scrypt.** Rejected on the constraint, not the merit: all
  three live in `golang.org/x/crypto` and AGENTS.md forbids new modules.
  PBKDF2-HMAC-SHA256 at 600k iterations is OWASP's own current floor (it lists
  it alongside argon2id and bcrypt as acceptable, with different parameters), so
  the fallback is a sanctioned choice rather than a stopgap. Its real weakness is
  GPU/ASIC friendliness (no memory hardness); the mitigations that matter here are
  the length-12 floor, the 10-per-15-min limiter, and the fact that the password
  file stores only a PBKDF2 output an attacker must still brute-force offline.
  bcrypt would additionally cap input at 72 bytes, which is a worse user-facing
  lie than a length error. Revisit via ADR if `x/crypto` becomes available
  (follow-ups).
- **Storing raw session ids / API tokens.** Rejected: backups, `sqlite3 .dump`
  diagnostics and evidence bundles (docs/spec/16) would then carry live
  credentials. SHA-256 of a 256-bit CSPRNG value is unguessable, so hashing buys
  the leak-resistance with nothing lost — unlike passwords, there is no dictionary
  to search, so no slow KDF is needed at this layer.
- **Double-submit cookie CSRF only (no session-bound token).** Rejected: an
  attacker who can plant a cookie can usually plant the matching value, and it
  requires the token to be *accepted from a cookie* as well as a header, which
  reopens the BREACH-style compression and subdomain-coefficient holes. The
  synchronizer token stored hashed, plus an `Origin`/`Referer` match when present,
  gives two independent checks with no client-side cryptography.
- **Origin/Referer alone.** Rejected as the only rule: non-browser clients and
  privacy-stripped requests send neither header, and "no header" would then have
  to mean "no request" — breaking the CLI and the Playwright harness that drive
  the API without a browser context.
- **Per-route handler checks (`if !id.Role.AtLeast(...)`) inside each handler.**
  Rejected: the failure mode is a handler someone forgets to guard, and it is
  invisible in review. A central table fails a test (`TestEveryRegisteredRoute…`)
  instead of failing closed in production only by accident.
- **Middleware asking the router which routes exist to decide the grant.**
  Rejected: it makes the safe answer depend on registration order, so a route
  added by a later milestone would be unauthenticated until its handler was
  wired. `Policy.Require` decides from the table alone; the route list exists only
  so a *test* can detect the drift.
- **Loopback check honouring `X-Forwarded-For`.** Rejected outright (see §7). The
  one header an attacker fully controls cannot gate the credential that gates
  everything else.
- **`Set-Cookie: …; Secure` always.** Rejected: over plain HTTP on localhost the
  browser drops the cookie and the default first-run experience becomes unusable,
  which teaches operators to disable the flag rather than to terminate TLS. The
  flag is therefore *derived* from the real scheme, with `WithTrustedProxy` as the
  explicit opt-in for the docs/spec/10 reverse-proxy deployment.
- **Sessions in a separate KV store / Redis.** Rejected: another daemon, another
  secret, and another persistence model to migrate, for a service whose entire
  state is one SQLite file (ADR-0008) — and one writer already holds the lock.
- **Lockout stored per account in SQL with a `failed_attempts` column.** Rejected:
  it converts unauthenticated traffic into write traffic on the hot catalogue row
  and creates a trivial account-disablement attack (burn somebody's counter, they
  cannot log in).

## Consequences and risks

Positive:
- docs/spec/14's three auth acceptance tests are now executable statements, not
  intentions: whole-surface role matrix, revoked-cookie invalidation, and no
  secret material in any DTO, cookie or log line.
- Adding a route can no longer be an unauthenticated act by omission: an
  ungranted path requires admin and a registered-but-ungranted path fails a test.
- Credential material is leak-resistant at every layer that can be dumped:
  database, backup, evidence bundle, log, audit row, HTTP DTO of a *second* read.
- Password cost is policy, so the 600k → higher migration happens by use.
- Tests are cheap: `auth.WithPasswordCost(1000)` keeps the suite inside seconds
  while `TestPasswordCostDefaultIsOWASP2023Floor` pins that the *production*
  default is still 600k and that `hashPassword` uses it.

Costs and new failure modes (all deliberate, all worth naming):
- Sessions are server state: every authenticated request is a SQLite read. On one
  appliance at LAN scale that is fine, and the `last_seen_at` write is throttled
  so it is one write per session-minute rather than per request.
- No session cache exists. If a future milestone makes `/api/v1/events` fan out to
  hundreds of subscribers per second of *new* requests, that read is the first
  thing to cache — and it must be invalidated by the same revocation writes.
- PBKDF2 at 600k iterations costs real CPU per login on a 4 GB ARM target
  (single-digit seconds there, hundreds of ms here). Login is rare; the limiter
  bounds the worst case. A deployment may lower it with `WithPasswordCost` — at
  the cost of a weaker file, which is why it refuses < 1000 outright.
- An in-memory limiter resets on restart. Accepted: it is telemetry, and the
  durable audit rows are the evidence.
- Fail-closed-to-admin will occasionally surprise a milestone that adds a route
  and gets 403 in its own test — the intended outcome being that it adds a grant
  and a matrix row rather than an exception.
- `developerScopedEvents` filters at the frame boundary; it is correct only for
  frames `encodeSSEFrame` emits (one record per `Write`, `\n\n`-terminated). A
  handler that wrote partial frames would let a record straddle two `Write` calls.
  `maxSSEFrameBuffer` bounds the pending buffer and `drain` runs on the way out;
  a future non-SSE payload on that path must not reuse the wrapper.
- The role ceiling is `min(owner, token)`: demoting an owner demotes their tokens
  but does *not* retroactively shrink an already-created session (§4 revokes on
  password change and disable, not on role change). Rotating a role therefore
  should be paired with a revocation until an admin UI exposes "sign the user out".

## Evidence and test plan

Schema and store (`internal/store/auth_store_test.go`):
`TestMigration0006RegistersTheAuthTables` (0006 applied on top of the 0001–0005
sequence in one database, dense versioning, `PRAGMA integrity_check` afterwards,
and no `sessions` column that could name a secret — AGENTS.md rule 10, with
`TestMigrateFromBlankDBIsRepeatable` covering the blank-and-rerun path), `TestAuthStoreUserConstraints` (role CHECK, NOCASE uniqueness),
`TestAuthStoreSessionLifecycle`, `TestAuthStorePasswordRotationRevokesEverythingInOneWrite`,
`TestAuthStoreDisableWithdrawsAndEnableDoesNotRestore`, `TestAuthStoreAPITokens`,
`TestAuthStorePruneBoundsTheOperationalTables`,
`TestAuthStoreAuditShapeAndSecretHygiene`, `TestAuthStoreCascadesSessionsButNotAudit`,
`TestHashSecretIsTheOnlyFormStored`.

Domain (`internal/auth/`):
`TestPasswordCostDefaultIsOWASP2023Floor`, `TestHashPasswordEnvelopeShape`,
`TestVerifyPasswordRoundTripAndFailure`, `TestVerifyPasswordRejectsTamperedEnvelopes`,
`TestNeedsUpgradeFollowsTheConfiguredCost`, `TestValidatePasswordLength`,
`TestHashPasswordDoesNotTrimTheCredential`, `TestDummyVerifyActuallyRunsTheKDF`,
`TestSetupCreatesTheFirstAdminFromLoopbackOnly`, `TestLoginMintsSessionAndVerifiesIt`,
`TestSessionIdleAndAbsoluteExpiry` (injected clock), `TestPasswordChangeRevokesEverything`,
`TestDisabledUserLosesEveryCredential`, `TestAPITokenRoleCeilingAndExpiry`,
`TestCSRFCheck`, `TestLoginRateLimitsAndAuditsFailuresWithoutThePassword`,
`TestPasswordHashIsUpgradedOnLogin`, `TestRoleOrdering`,
`TestDefaultPolicyGrantsDocsSpec08Routes`, `TestUnknownRouteRequiresAdmin`,
`TestIsLoopbackRemote`, `TestOriginAllowed`, `TestDeveloperOnlyEvents`,
`TestNewAPITokenFormat`, `TestLoginLimiterRefillsAndCaps`,
`TestLimiterRefusesNewKeysWhenFull`.

HTTP surface (`internal/api/auth*_test.go`):
`TestPublicRoutesAnswerWithoutCredentials`, `TestNonPublicRoutesRefuseWhenNoCredentialStoreIsWired`,
`TestSetupFlowClaimsTheApplianceOnce`, `TestSetupRefusalOrder`,
`TestLoginMeLogoutRoundTrip`, `TestLoginFailuresAreUniformAndCannotEnumerateAccounts`,
`TestSessionCookieSecureFlagFollowsTheRealScheme`, `TestCSRFMatrix`,
`TestBearerRequestsAreExemptFromCSRF`, `TestBearerTokenRespectsItsOwnRole`,
`TestUnknownAndForeignTokensAreRejected`,
`TestPasswordChangeOverHTTPInvalidatesTheOldCookie`, `TestIdleAndAbsoluteSessionExpiry`,
`TestLoginRateLimitAnswers429WithRetryAfter`, `TestRoleMatrixOverEveryRoute`,
`TestEveryRegisteredRouteIsCoveredByTheScopeTable`, `TestUnknownRouteFailsClosedToAdmin`,
`TestAdminUserRoutesAreAdminOnlyAndCarryNoHashes`,
`TestAdminCreatesAccountMintsTokenOnceAndWithdraws`, `TestAdminCannotDisableItselfThroughTheAPI`,
`TestEventStreamRedactsDeveloperEvents`.

CLI: `TestAdminCreateReadsThePasswordFromStdin`,
`TestAdminTokenPrintsTheSecretOnce`, `TestAdminListNeverCarriesASecret`,
`TestServeRequiresAuthenticationAndCompletesTheFirstRunCycle`.

Commands:
```
go vet ./... && go test -race -count=1 ./internal/auth/ ./internal/store/ ./internal/api/ ./cmd/game-narrator/
```
plus the live walk-through against a real `serve` on 127.0.0.1 recorded in the
m1-auth evidence bundle (`artifacts/<run-id>/`): status → setup → login → me →
unauthenticated read → CSRF-enforced POST → authorized read → logout → dead
cookie. Not yet exercised (later gates, honestly labelled): a browser-driven
login flow through Playwright (docs/spec/16 requires it once `web/` ships the
form), TLS termination on a LAN deployment, and real-hardware smoke with auth on.

## Migration / rollback

- Forward: `0006_auth.sql` is additive — four new tables, no rewrite of existing
  rows, no CHECK relaxation, so it applies to a blank database and to a database
  already carrying games, profiles, jobs and events alike (verified in
  `TestMigration0006RegistersTheAuthTables`). Existing jobs/health routes keep
  their handlers; only their *reachability* narrows.
- Operational consequence of a fresh upgrade: an upgraded appliance has no admin,
  therefore `setup_required:true` and every privileged route 401s until somebody
  runs setup on the machine itself. `serve` logs the unclaimed state at startup
  (and `game-narrator doctor` reports it) rather than letting an appliance look
  healthy while refusing every credential.
- Breaking-change surface: any pre-existing localhost client that called
  `/api/v1/jobs` or `/api/v1/events` without a credential now needs one. The
  migration path is a CLI/API token (`game-narrator admin token …`), which is the
  integration credential docs/spec/08 already prescribes.
- Rollback: revert the middleware wiring (`api.WithAuth` is an option; with it
  absent the server answers 503 `auth-unavailable` rather than silently serving
  unauthenticated, so a partial revert cannot open the API), then `DROP TABLE
  auth_audit, api_tokens, sessions, users`. The tables are self-contained;
  nothing else references them, so no other migration needs reverting and no
  catalogue data is touched. SQLite has no `DROP COLUMN` problem here because no
  pre-existing table was altered.

## Follow-ups

- **TLS termination guidance for LAN** (docs/spec/10 "HTTPS/Tailscale/reverse
  proxy with authentication"): document a working nginx/Caddy snippet with
  `X-Forwarded-Proto` plus the matching `GAME_NARRATOR_TRUSTED_PROXY_CIDRS` allowlist, and assert the
  Secure flag through it. Until then the forwarded-https path is exercised only by
`TestSessionCookieSecureFlagFollowsTheRealScheme`, not by a real proxy.
- **Playwright coverage of the login flow** once `web/` ships it (a later task):
  the SPA must read `gn_csrf`, send `X-CSRF-Token`, and treat a 401
  `session-*` as "go back to the login form", with a test for each.
- **argon2id via `golang.org/x/crypto`** if the dependency ban is lifted; the
  envelope is already parameterized (`pbkdf2-sha256$…` names its own algorithm),
  so the migration is "accept a second prefix, rehash on login" with no schema
  change. Same for a cost bump above 600k once the ARM target is measurable.
- **Role change should revoke sessions** (or the admin UI should offer it) — see
  the risk above.
- **WebAuthn/passkeys** as a second factor for `admin`, and hardware-key binding
  for the UNO Q release target: the appliance is physically reachable, which is
  the case a password alone answers badly.
- **Sealed provider credential rotation** (`/api/v1/secrets/*` is granted to
  admin here so the route cannot be born open; the handler is docs/spec/14's
  "secure secrets store" work in a later milestone).
- **Audit export** — `auth_audit` is written and pruned; `make diagnostics`
  should surface recent auth events (with `detail_json` redaction preserved).
- **Login UI rate-limit affordance**: surface `Retry-After` rather than a spinner.
- Review date: at the first LAN deployment (docs/spec/16 release snapshot), and
  whenever `internal/events` grows a name that carries gameplay text.
