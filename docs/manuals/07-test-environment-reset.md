# Test environment reset

## Purpose

Reset the local state that tests and gates create — throwaway data
directories, gate logs, browser artefacts, the Dolphin user dirs — without
touching the real durable data directory or committed files.

## Preconditions

- You know which data directory is the **real** one: `--data-dir`,
  `GAME_NARRATOR_DATA_DIR`, else `$XDG_DATA_HOME/game-narrator` (or
  `$HOME/.local/share/game-narrator`); on the Debian service,
  `/var/lib/game-narrator`. Everything below operates on **other** paths.

## Steps

1. **Throwaway databases.** Tests and one-off runs use `--data-dir` with a
   fresh temp dir; the database is created and migrated on demand:

   ```sh
   bin/game-narrator migrate --data-dir "$(mktemp -d)"
   ```

   Delete the temp dir when done (`rm -rf` the `mktemp` path). `go test`
   runs keep their own state in `t.TempDir()` and clean up after themselves.
2. **Gate and evidence output.** `make test-full` and the go-gates write to
   `artifacts/` (per-gate logs, JUnit XML in `artifacts/gates/`, `bench`
   reports in `artifacts/bench/`, evidence bundles under
   `artifacts/<run-id>/`). `artifacts/` is gitignored:

   ```sh
   rm -rf artifacts            # safe: nothing in artifacts/ is tracked
   ```

   Keep `artifacts/<run-id>/` evidence bundles you still need — they are
   the AGENTS.md evidence record, not scratch.
3. **Build output.** `bin/` (the built binary) and `web/dist/` are
   gitignored; `make build` regenerates both, so they can be deleted freely:

   ```sh
   rm -rf bin web/dist test-results playwright-report
   ```

   (`test-results/` and `playwright-report/` are the Playwright outputs.)
4. **Dolphin state.** The dev Dolphin bridge never writes to your real
   Dolphin profile; its isolated user dirs live under
   `$TMPDIR/game-narrator-dolphin` — reset them per
   08-dolphin-profile-reset.md.

## Verification

- `git status --porcelain` is clean — the reset only removed gitignored
  paths, and the `internal/webassets/dist/` tracked `.keep` placeholder is
  still present (a fresh `make build` restores the staged assets).
- A follow-up `make test-unit` / `make test-ui` / `bin/game-narrator
  migrate --data-dir "$(mktemp -d)"` run works from the reset state.

## What NOT to do

- Never reset the **real** data directory (`~/.local/share/game-narrator`,
  `/var/lib/game-narrator`) to make a test pass — that is wiping user state
  (AGENTS.md rule 11). Reset scratch dirs, not user data.
- Do not delete `artifacts/<run-id>/` evidence before the run is reported;
  the bundle (manifest, gate logs, timelines, audio) is the proof of what
  passed (AGENTS.md).
- Do not treat `rm -rf artifacts` as a way to "fix" a failing gate — the
  gate output is the diagnostic; delete it only after reading it.
