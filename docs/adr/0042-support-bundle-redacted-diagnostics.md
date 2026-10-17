# ADR-0042: `support-bundle` — the redacted diagnostics archive

Date: 2026-10-17
Status: accepted
Owners: david (support-bundle)
Related requirements/tests: docs/spec/13 ("Logging/privacy": "Provide `export
support bundle` with automated scrubber and a preview before export";
"Diagnostics page and CLI"), docs/spec/14 (tokens stay server-side; redaction
of credentials and identity), docs/release.md "Hard rules" (artifact scanner
fails builds on tokens, gateway hostnames, `/home/` paths), AGENTS.md rules 3
(no simulation theater) and 5 (never commit private material),
docs/manuals/11-purging-support-bundles.md (was NOT YET IMPLEMENTED)

## Context and constraints

A support report needs the build identity, the doctor report, the schema/migration
state, the table sizes and the effective model-role configuration — and it must
not carry a token, a gateway hostname, a private IP, a home path, an email
address, a database row, game text, a frame, a crop, a waveform or dialogue.
The manual (`docs/manuals/11`) previously said no such command existed and gave
a manual procedure. The app writes **no log file** under the data dir
(structured JSON goes to stderr — the serve process journal), so "recent
structured logs" has no file to ship; the manifest records that instead.
The bundle builder may not edit `internal/store`, `internal/doctor`,
`internal/bootstrap` or `internal/providers` (other workers own them); it
consumes their public APIs. The doctor's storage probe opens **and migrates**
the store by design (`doctor`'s documented contract), which a diagnostics
command must not do silently.

## Decision

1. **Command.** `game-narrator support-bundle --data-dir DIR [--out FILE]
   [--since D] [--include-dev-trace] [--probe]` (one new case in the
   `main.go` switch; implementation in `cmd/game-narrator/support.go`,
   logic in a new package `internal/support`). `--out` defaults to the
   current directory (`./game-narrator-support-<UTC stamp>.tar.gz`): the
   bundle is a transient artifact, not durable state, and the app keeps no
   copies. Exit 0/1/2 matches the other commands; a missing database is an
   actionable exit-1 ("run migrate first") — enforced **before** the doctor
   section runs, so the build never creates or migrates the store (the
   doctor's probe would).
2. **Archive contents** (one tar.gz, flat JSON entries only, verified by a
   sha256 manifest of every other entry — the manifest cannot carry its own
   hash, and the preview prints the list before export, per docs/spec/13):
   - `manifest.json`: version/commit/Go/OS/arch, `created_at`, the data-dir
     **basename only** (no host path), a logging note (stderr-only; no log
     file under the data dir), the doctor mode, the entry hashes.
   - `doctor.json`: `doctor.Run` output wrapped with `mode` + `note`.
     **Offline by default**: every provider probe interface
     (`ProviderProbe`, `DecisionProbe`, `VoiceProbe`) is replaced by one
     that answers with a `providers.KindUnavailable` "probe skipped" error
     and never dials — network-free by construction, not by convention.
     Local checks (storage, tools, OCR image, capture, audio) run for real.
     `--probe` drops the injections and exercises the gateways like
     `doctor`.
   - `store.json`: schema version, applied migrations and per-table row
     **counts** through a `?_query_only=1` connection (the same
     read-only DSN style as `store.Restore`). The database file is never
     copied; counts are the spec's "describe it, don't attach it".
   - `config.redacted.json`: `providers.LoadRoles` (the same source of
     truth the runtime uses) per role: model, protocol, timeout, effort,
     state; every URL reduced to `scheme://<redacted-host>`; tokens are
     presence-only (`token_set` bools); environment variable **names** and
     set/unset are kept (the name of the variable holding a secret is what
     the recipient re-checks — the doctor follows the same rule).
   - `dev-trace.json` (only with `--include-dev-trace`): per session under
     `<DataDir>/dev-trace/<session>/blobs/**` — the session id (basename),
     and per blob sha256/size/mtime. `--since` windows by mtime. No blob
     is opened; the hashes correlate with `/api/v1/dev/traces/{id}`.
3. **Redaction layer** (`internal/support/redact.go`), ordered: URLs first
   (an absolute URL swallows host, port, path, query and userinfo in one
   step — a hostname with a token in the query cannot leak through a
   partial rule), then `Authorization`/`Bearer` headers, then bare keys
   (`sk-`/`sk_`, `gh[pousr]_`, `github_pat_`, the app's own `gn_` prefix),
   then private IPv4 (RFC 1918, validated with `net.ParseIP`) and IPv6
   (ULA `fc00::/7`, link-local `fe80::/10`) — public and loopback addresses
   are kept, then `/home/<user>[/rest]` → `~[/rest]`, then email. Replacements
   are fixed sentinels (`<redacted>`, `<redacted-host>`), never truncated
   originals.
4. **Content guard** (the test that makes the claim): a bundle built from a
   data dir seeded with a synthetic profile containing distinctive dialogue
   (via the store's public API), a dev-trace JPEG-magic blob, asset-store
   PNG- and WAV-magic blobs and a filesink WAV must contain **none** of those
   strings and **no** WAV/PNG/JPEG magic bytes anywhere in the archive; the
   archive holds exactly the documented entries; the manifest hashes verify;
   the store stays byte-identical after the build.

## Alternatives considered

- **Ship the database (or a backup of it).** Rejected: docs/spec/13 and the
  manual both ban it — the catalogue is user-curated content; counts and
  schema version are the required substitute.
- **Let the doctor probe live by default.** Rejected: a support bundle must
  be buildable offline (tests included); live probes go behind `--probe`,
  matching how `doctor` is run deliberately.
- **Report skipped provider probes as NOT CONFIGURED / OK.** Rejected:
  both would be false (the role *is* configured and was *not* shown to
  work); the doctor's standing rule is "never report an unprobed service as
  working", so the injected probe surfaces as `unavailable` with an
  explicit skip note, and `mode` + `note` label the section.
- **A blanket recursive string scrub over the config tree as the only
  redaction.** Rejected: a key-name rule ("token_env" matches a credential
  key) would redact the env var *names* the spec requires to survive; the
  config section redacts per field (URL rule, presence bools, names kept)
  and the tree scrubber exists as a tested general utility, not as the
  config path.
- **Copy recent files from the data dir into the bundle.** Rejected: the
  data dir holds the catalogue, assets, audio and dev-trace samples — all
  user data; the bundle's contract is metadata + hashes only.

## Consequences and risks

- A bundle from a machine with no database fails with an actionable message
  instead of silently migrating one (the doctor's migrate-on-open contract
  is preserved for `doctor` and `serve`, not extended to diagnostics).
- Offline provider rows read as `unavailable`/FAIL in `doctor.json`: honest,
  but a recipient must read `mode` + `note` (documented in the manual); a
  future `SKIPPED` doctor status would be a cleaner fit but would mean
  editing `internal/doctor`, which is outside this task's boundary.
- FTS5 shadow tables (`messages_fts_*`) appear in `table_counts`: real
  tables, counted honestly; filtering them by name would guess.
- The redaction layer is regex-based and table-tested, not a guarantee: the
  content guard test plus the release scanner remain the backstops, and the
  manual's pre-share `grep` is the operator's final check.

## Evidence and verification

- `go test ./internal/support/...` (table-driven redaction: URLs, bearer,
  `sk-`/`gh*`/`gn_` keys, `/home/` → `~`, private v4/v6, email; content
  guard; read-only store; `--since` window) — offline, no network.
- `go test ./cmd/...` end-to-end: builds the real binary, `migrate` a fresh
  data dir, runs the real `support-bundle`, inspects the tar.gz (entries,
  manifest hashes, preview lines, default `--out` in the cwd, usage/exit
  codes).
- Live smoke: bundle built on this dev machine with real gateway env set —
  `config.redacted.json` shows `https://<redacted-host>` and no hostname,
  token or `/home/` path anywhere in the archive.

## Migration / rollback

No schema, protocol or data change; `internal/{store,doctor,bootstrap,providers}`
untouched. Revert: delete `internal/support`, `cmd/game-narrator/support{,_test}.go`,
the one `case` + two usage lines in `main.go`, restore the previous
`docs/manuals/11` and this ADR; nothing else references the package.

## Follow-ups

- If the doctor ever grows a `SKIPPED` state, replace the injected-probe
  offline mode with it (revisit ADR-0042 §4 here).
- A developer-UI "Download support bundle" button would call the same
  `support.Build` server-side (docs/spec/13 "Diagnostics page") — out of
  scope here; the CLI is the first-class path.
