# Purging redacted support bundles

## Purpose

Collect and purge the diagnostic material for a support report.
`game-narrator support-bundle` builds one redacted `tar.gz` archive that is
safe to send to a third party: no tokens, no gateway hostnames, no private
IPs, no home paths, no database rows, no game text, no frames, crops, audio
or dialogue (docs/spec/13 "Logging/privacy", AGENTS.md rule 5).

```sh
bin/game-narrator support-bundle --data-dir DIR \
    [--out FILE.tar.gz] [--since 24h] [--include-dev-trace] [--probe]
```

Flags:

- `--data-dir DIR` — the durable data directory to bundle
  (default `GAME_NARRATOR_DATA_DIR`, else the XDG default).
- `--out FILE.tar.gz` — where the archive is written. **Default: the current
  directory** (`./game-narrator-support-<UTC stamp>.tar.gz`). The app keeps
  no copies: the bundle lives exactly where `--out` puts it and nothing
  under the data dir is touched.
- `--since 24h` — limit the dev-trace metadata to the last `24h`. The app
  writes no log file (structured JSON goes to stderr, see below), so the
  window applies to the dev-trace section only.
- `--include-dev-trace` — include dev-trace *metadata* (session ids, blob
  hashes, sizes, mtimes). Never include frames, crops, audio or dialogue
  text; without the flag the section is absent.
- `--probe` — let the doctor section exercise the configured provider
  endpoints. Off by default: the bundle is built **offline-safe**, with no
  network access, and configured roles report as not probed rather than as
  working.

Exit status: 0 on success, 1 on failure (e.g. no database in the data dir —
run `bin/game-narrator migrate --data-dir DIR` first), 2 on bad usage.

## What is inside

Every entry is a small JSON document; the archive contains nothing else
(never the database file, assets, audio or dev-trace blobs):

- `manifest.json` — version, commit, Go version, OS/arch, `created_at`, the
  data-dir **basename only** (no host path), the sha256 and size of every
  other entry, and the logging note: the service logs structured JSON to
  stderr only, so the bundle carries a note instead of log content (the
  journal to look at is the process journal of `game-narrator serve`).
- `doctor.json` — the doctor report. Local checks (storage, tools, OCR
  image, capture devices, audio sinks) ran for real; network provider
  probes are skipped unless `--probe` (the `mode` field says which).
- `store.json` — schema version, applied migrations and per-table row
  **counts only**, never content. The database itself is never copied into
  a bundle.
- `config.redacted.json` — the effective model-role configuration: every
  URL reduced to `scheme://<redacted-host>`, every token/secret value
  replaced by `<redacted>`, environment variable *names* and set/unset
  kept (so a recipient can re-check the configuration without seeing
  secrets).
- `dev-trace.json` (only with `--include-dev-trace`) — per session: the
  session id and each stored sample's sha256, size and mtime. The hashes
  let you correlate the trace with the developer API
  (`/api/v1/dev/traces/{trace_id}`) without shipping the samples.

## Inspect before sharing

The command prints a preview (path, entry list, sha256 per entry) before
you move the file. Verify the archive itself, then re-check for anything
the redaction rules could not have covered:

```sh
# what is inside (entry names only)
tar -tzf bundle.tar.gz

# identity, entry hashes and the logging note
tar -xOf bundle.tar.gz manifest.json | jq .

# store metadata: schema version + table counts (no content)
tar -xOf bundle.tar.gz store.json | jq '.schema_version, .table_counts'

# redacted config: no hostnames, no token values
tar -xOf bundle.tar.gz config.redacted.json | jq .

# scan for anything that should not be present (expect no real matches)
mkdir -p /tmp/gn-check && tar -xzf bundle.tar.gz -C /tmp/gn-check
grep -rniE 'token|secret|password|/home/|gn_[0-9a-f]{8}' /tmp/gn-check
rm -rf /tmp/gn-check
```

## Purging

The bundle is a transient artifact: it lives where `--out` puts it (the
current directory by default), and the app keeps no copies — there is
nothing to purge inside `game-narrator` after you are done with it.

1. Verify it with the commands above before sending it.
2. Delete it once the support exchange is done:

   ```sh
   rm bundle.tar.gz        # or the path you gave to --out
   rm -rf /tmp/gn-check    # and any inspection scratch dir
   ```

   Do not let bundles accumulate in scratch dirs or home directories; each
   one is a redacted snapshot of a machine, and keeping old ones is
   keeping old, possibly more-identifying, snapshots.

## What NOT to do

- Do not attach the database, game image, savestates, BMG/source dumps,
  voice samples or any screenshot that shows copyrighted visuals
  (AGENTS.md rule 5). Diagnostics cite content by hash and count.
- Do not attach developer-trace frame crops or audio as a shortcut;
  `--include-dev-trace` ships hashes, sizes and mtimes only.
- Do not run `--probe` from a machine that should stay offline; it sends
  real requests to the configured gateways.
- Do not send anything containing the game image path into a channel that
  is not the evidence bundle's audience (docs/dev-environment.md
  "Verified gaps" 2). The bundle does not contain it; do not add it.
