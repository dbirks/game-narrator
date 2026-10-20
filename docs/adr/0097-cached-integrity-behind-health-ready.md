# ADR-0097: Cached database integrity behind /health/ready (startup + schedule)

Date: 2026-10-21
Status: accepted
Owners: agent worker (task issue9-10-board-fixes); owner (review)
Related: issue #9 (readiness always 503 on the UNO Q), ADR-0008
(modernc.org/sqlite driver), `internal/store/readiness.go`
(`Readiness`, the cached verdict), `cmd/game-narrator/main.go`
(`storeReadiness`, `readinessInterval`), `internal/api/api.go`
(`handleReady`, `readinessTimeout`), `internal/api/api_test.go`
(`TestHealthReadyReportsTheCachedVerdictWhileTheCheckIsSlow`),
`internal/store/readiness_test.go`

## Context and constraints

`GET /api/v1/health/ready` ran the full `PRAGMA integrity_check` on every
request, bounded by the handler's hardcoded 2 s probe deadline
(`internal/api/api.go`). On the UNO Q's A53 the modernc.org/sqlite check
takes ~2.83 s against a database the C `sqlite3` CLI verifies in 0.82 s —
so the probe answered 503 `{"database":"integrity check failed"}` on a
healthy file (issue #9), which would also have failed any deployment health
gate on the target hardware.

The process already records, in `cmd/game-narrator/main.go`, that moving the
probe behind a cached interval is an ADR, not a silent downgrade. The
constraints:

- Readiness must still reflect real health: a corrupt file must answer 503,
  not a stale 200.
- The API surface carries no host paths: store errors name the database file
  path, so only fixed strings may reach the payload.
- The one self-repair the store can make (the derived `messages_fts` index,
  entirely rebuildable from `messages`) must keep happening — the question is
  where it runs.
- The M0 database is small; the cost is the check's constant overhead on the
  A53, not its scaling.

## Decision

- The full integrity check (plus the ping and the FTS self-repair) runs in
  `store.Readiness`: once at startup, before the listener opens, and again on
  a fixed schedule in one background goroutine. The per-request path is the
  schema-version ping (a live connection and a readable file) plus the cached
  verdict; `/health/ready` reports the verdict's age in
  `integrity_age_seconds` so a stale "ok" is visible rather than silently
  trusted.
- The schedule is configurable through
  `GAME_NARRATOR_READINESS_INTERVAL` (a Go duration; default
  `store.DefaultReadinessInterval` = 5m; unset or malformed falls back to the
  default rather than refusing to boot). One goroutine owns the ticks, so
  checks never overlap; a tick's context is bounded by the interval, and a
  tick cancelled by shutdown records nothing — the last verdict stands.
- The self-repair decision moves with the check: a scheduled check that finds
  index-only damage rebuilds it in the background, and the next answer is the
  repaired verdict. A startup verdict failure does not stop the boot
  (unchanged from the per-request behaviour): the service answers 503 from
  the cached verdict and the operator path is backup/restore.
- The verdict detail stays one of two fixed strings
  (`store.PingFailedDetail`, `store.IntegrityFailedDetail`); the store's
  errors (which name the file path) stay in the log.
- The 2 s handler deadline (`readinessTimeout`) stays: the per-request path
  now costs a ping, but a wedged dependency must still make the endpoint
  answer "not ready" quickly rather than hold the request open.

## Options considered

1. **Per-request `PRAGMA quick_check`.** A cheaper variant of the same
   check, but it still pays real I/O on every probe on the A53 and its cost
   relative to the full check is driver-version dependent; it also leaves the
   self-repair racing the probe. Rejected: it trades one 503 cause for an
   unbounded per-request cost without fixing the structural mismatch (a slow
   check inside a fast deadline).
2. **Opaque/cached verdict with no per-request ping.** Cheapest possible
   answer, but a database file that stops being readable between checks
   would read as "ok" until the next tick. Rejected: the ping is one small
   query and keeps the answer honest against the dominant failure (the disk
   or the file, not the check's duration).
3. **Longer per-request deadline on the target.** A config knob on
   `readinessTimeout` (e.g. 10 s on the A53) would make the current code pass,
   but every health poll would then cost ~3 s of A53 I/O, a load balancer
   could time out before us, and the probe's contract (answer fast) is the
   one docs/spec/13's "no generic success on an unprobed service" leans on.
   Rejected.

## Positive consequences

- `/health/ready` answers from a cache read plus one small query: well inside
  the 2 s budget on the A53, on every request, forever.
- The answer says how old its health claim is (`integrity_age_seconds`),
  which the per-request design could never express.
- The self-repair runs where it belongs (the check's context), and a repaired
  database returns to 200 at the next answer without any request paying the
  rebuild.

## Negative consequences / risks

- A corruption that lands between two ticks reads as "ok (age N s)" until the
  next scheduled check. Bounded by the interval (5 m default); the age field
  makes the bound visible to operators and load balancers.
- The scheduled check costs ~3 s of A53 I/O per interval. At the default 5 m
  that is negligible; an operator who lowers the interval pays it
  proportionally (the knob is documented in `serve`'s help).
- The startup check delays boot by one check's duration (~3 s on the target,
  milliseconds elsewhere). Accepted: the boot already pays migrations and
  restore application, and a verdict before the listener is the point.

## How to verify (commands and evidence)

- `go test -race -count=1 ./internal/store/ -run Readiness` — the injected
  slow checker never blocks the answer (the ADR's core clause), the failing
  checker records the fixed string, the scheduler re-checks on the interval,
  a cancelled tick keeps the last verdict, and the production probe repairs
  index-only damage at check time (reusing `fts_repair_test.go`'s proven
  corruption).
- `go test -count=1 ./internal/api/ -run TestHealthReady` — the handler
  answers 200 from the cached verdict in well under the 2 s budget while the
  injected 1.5 s check is in flight, with the age in the payload; after the
  check finishes the next answer carries the refreshed verdict.
- On-device: `curl -s localhost:8080/api/v1/health/ready` on the UNO Q
  answers 200 with `integrity_age_seconds` (issue #9's repro, the 503
  `integrity check failed` on a healthy DB, must be gone). NOT RUN until the
  next hardware pass; the A53 timing is the field measurement quoted in the
  context.

## Rollback/migration path

Restore the per-request spelling: delete the `Readiness` wiring in
`main.go` (the startup check, the `go readiness.Run` goroutine) and the
`storeReadiness` cache read; the per-request `IntegrityCheck` body is the
pre-ADR code (kept in git history). No data migration: nothing is persisted,
the cache is process-local. `GAME_NARRATOR_READINESS_INTERVAL` is simply
unset.

## Supersedes / superseded by

Supersedes the per-request integrity probe implied by
`cmd/game-narrator/main.go`'s storeReadiness comment ("M0 databases are small
enough to check on every poll").
