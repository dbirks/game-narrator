# Purging redacted support bundles

## Purpose

Describe how to collect and purge diagnostic material for a support report.
**There is no `export support bundle` command in this build.** The CLI
provides `serve`, `migrate`, `admin`, `doctor`, `import-disc`, `version` and
the benchmark command `bench` (hidden from the top-level listing; see
`bin/game-narrator --help`) — nothing bundles, scrubs or exports
diagnostics. That capability is required by
docs/spec/13 "Logging/privacy" ("Provide `export support bundle` with
automated scrubber and a preview before export") and is **NOT YET
IMPLEMENTED** (tracking: docs/spec/13; the privacy constraints below are the
interim manual policy).

## What to collect manually today

Everything is either redacted by construction or safe to run locally:

1. **Doctor report** — the primary diagnostic:

   ```sh
   bin/game-narrator doctor --dev --json   # machine-readable; no URLs, tokens or image paths in it
   ```

   Provider lines never print a URL, a token or a game image path
   (docs/spec/13; asserted by the recorded doctor tests, ADR 0010 §7).
2. **System info / health** — `GET /api/v1/system/info`,
   `GET /api/v1/health/ready` (build version/commit, `os`/`arch`,
   readiness state).
3. **Gate evidence** — the per-gate logs and JUnit XML under
   `artifacts/gates/` and the run's evidence bundle
   (`artifacts/<run-id>/`), which is what `make test-full` records.
4. **Event/trace data** for a misbehaving session, via the developer API
   (`/api/v1/dev/events`, `/api/v1/dev/traces/{trace_id}`) or the
   `/developer` page — hashes, counts and selected confidence, not
   full screenshots or waveforms, by default (docs/spec/13 "Logging/privacy").

## Redaction rules for anything you attach

- **Tokens and secrets:** never include `.env`, `GAME_NARRATOR_GATEWAY_TOKEN`
  values, or the gateway hostname (the artifact scanner
  `scripts/release/scan-artifacts.sh` fails builds on exactly these, plus
  `/home/` paths and private file names — docs/release.md "Hard rules").
- **Game text and copyrighted material:** never attach the game image,
  savestates, BMG/source dumps, or screenshots that show copyrighted
  visuals (AGENTS.md rule 5). Diagnostics cite content by hash and count.
- **Audio:** do not attach voice samples or reference audio — voice data is
  user data the package is forbidden from deleting, and it never belongs in
  a support file (docs/spec/14 consent).
- **The database:** do not attach `game-narrator.sqlite` — it contains the
  user-curated catalogue and voice references. If storage state matters,
  describe it (schema version from `migrate`, table counts) instead.

## Purging the collected material

1. Keep only the redacted items above in a scratch dir (e.g.
   `mktemp -d /gn-support.XXXXXX`).
2. Verify before sending:

   ```sh
   grep -rniE 'token|secret|password|/home/|gn_[0-9a-f]{8}' <scratch-dir>   # should match nothing real
   ```

3. Delete the scratch dir and any local copies:

   ```sh
   rm -rf <scratch-dir>
   ```

   `artifacts/` itself is gitignored — but delete any bundle you no longer
   need rather than letting it accumulate.

## What NOT to do

- Do not claim a bundle was "exported with the automated scrubber" — no such
  tool exists yet (no simulation theater, AGENTS.md rule 3).
- Do not attach developer-trace frame crops or audio as a shortcut; the
  default log/trace posture is hashes and counts, and the sample-mode
  storage is quota-bounded and explicitly toggled (docs/spec/13).
- Do not send anything containing the game image path or hash into a
  channel that is not the evidence bundle's audience — even hashes reference
  private user-supplied material (docs/dev-environment.md "Verified gaps" 2).
