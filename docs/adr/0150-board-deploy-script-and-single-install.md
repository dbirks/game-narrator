# ADR-0150: one board deploy script (two install modes), and a doctor check that names the port collision

Date: 2026-10-21
Status: accepted
Owners: board-ops worker
Related requirements/tests: docs/spec/13 (diagnostics: a service that has not been probed is never reported as working), docs/spec/16 (release gates; hardware smoke where hardware is available), docs/manuals/19 (install on UNO Q; env file, the two partitions, the `.deb` lifecycle), ADR-0126 (data-dir source reporting — the "two databases on one host" warning this ADR's incident is the sibling of), `internal/doctor/net_test.go`, `scripts/board/deploy.sh`

## Context and constraints

On 2026-10-21 a real UNO Q board (hostnames redacted) carried **two** game-narrator installs at once: the packaged systemd unit (`game-narrator.service`, `User=game-narrator`, `EnvironmentFile=/etc/game-narrator/env`, data in `/var/lib/game-narrator`) and a hand-run user-level process (`~/gn-live`, env file `~/gn-live/env` with `GAME_NARRATOR_DATA_DIR=~/gn-live/data`, started with `setsid nohup /usr/bin/game-narrator serve >> serve.log 2>&1 < /dev/null &`). A deploy installed a new binary and restarted the systemd unit **while the user-level process still held the listen port**. The consequences on a CPU-bound appliance:

1. The unit crash-looped (503 restarts) with `listen tcp <host>:8080: bind: address already in use`, burning CPU for the whole incident.
2. The new build never served.
3. The unit pointed at a different, empty data dir, so even a manual stop+start would have "succeeded" on the wrong data.

The same incident produced two more operational lessons:

4. A `set -e` deploy script aborted at the data backup because `tar czf` exits 1 when a file changes while being read (`data/dev-trace/.../blobs: file changed as we read it` — the dev-trace store is live while the service runs). The working recipe is `tar czf data-backup-$ts.tar.gz --exclude=data/dev-trace data`, treating tar rc=1 as a warning.
5. The old process must be stopped from `pgrep -x game-narrator` by exact PID. `pkill -f "game-narrator serve"` matches the ssh shell that runs the deploy itself.

The cross-build recipe that worked from the dev host (web assets must already be built into `internal/webassets/dist`):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
  -ldflags "-X github.com/dbirks/game-narrator/internal/bootstrap.version=0.1.0-$SHA \
            -X github.com/dbirks/game-narrator/internal/bootstrap.commit=$SHA" \
  -o <out> ./cmd/game-narrator
```

Verified on the board with `game-narrator version` (there is no `--version` flag).

Constraints: the board is a Debian arm64 appliance with passwordless sudo and key-based ssh (manual 19 preconditions); the deploy must never prompt (it runs unattended from a dev host); the target host may only come from operator-supplied configuration (no board address in tracked files, AGENTS.md rule 5); and a diagnostic must never report a component as working that it has not probed (docs/spec/13).

## Options

1. **Document the recipe only (manual section).** Rejected alone: the two-install collision is exactly the class of failure a checklist does not catch — a deploy that believes it is targeting "the" install. The refusal has to be code that sees the board's actual process and unit state.
2. **A deploy script that kills everything named game-narrator and starts one install.** Rejected: `pkill -f`-style matching kills the deploy's own ssh shell, and blanket killing hides the second install instead of naming it — the operator loses the signal that two installs coexist.
3. **A mode-explicit deploy script with an active-other-install refusal, plus a doctor row that probes the port (chosen).** The script commits to one of the two layouts (`--user` or `--systemd`), refuses to continue while the *other* install is active, and prints the one command that disables it. The doctor gets a `network.listen` row that performs a real bind of the configured address, so the collision is a `FAIL` before the first crash loop, not after the 503rd.

## Decision

1. **`scripts/board/deploy.sh`** (bash, shellcheck-clean). The target host comes only from `GN_BOARD_SSH` (`user@host`) and is never defaulted. Exactly one mode is required:
   - `--user` deploys the `~/gn-live` layout (env file `~/gn-live/env`, data in `~/gn-live/data`, started with the proven `setsid nohup` recipe); it creates the layout and a placeholder env file (data dir + listen default + commented gateway keys, mode 0600) if absent.
   - `--systemd` deploys the packaged unit (`game-narrator.service`), data in `/var/lib/game-narrator`.
   - Both modes: cross-build the arm64 binary with the version/commit stamp (the command above; web assets staged via `make web-build` are a hard precondition), `scp` it to the board, back up the data directory with the tolerant recipe (tar rc=1 → warning, rc>1 → stop; `dev-trace` excluded), keep the previous binary at `/usr/bin/game-narrator.prev` for rollback, stop the old process(es) by exact PID from `pgrep -x` (SIGTERM, 15 s grace, SIGKILL fallback — the script waits on the PIDs it sent signals to, and never uses `pkill -f`), install with `sudo -n install`, start, wait up to 30 s for `GET /api/v1/health/live` on the listen address the mode's own env file configures (loopback only), and print `game-narrator version`.
   - **The refusal:** deploying `--user` while `systemctl is-active game-narrator` is refused with `sudo systemctl disable --now game-narrator` as the printed remedy; deploying `--systemd` while a user-level process runs as the ssh login user (`pgrep -x -u $USER game-narrator`) is refused with `kill $(pgrep -x -u $USER game-narrator)`. Refusing before touching anything is the incident fix: the port holder is named, not killed blindly.
   - `--dry-run` prints every step (local commands and the exact remote scripts) and touches nothing.
2. **The `network.listen` doctor check** (`internal/doctor/net.go`, check id `network.listen`, group `Network`). It performs a real `net.Listen` on the configured address (the CLI passes the same resolution serve uses: `--listen` flag > `GAME_NARRATOR_LISTEN` > `127.0.0.1:8080`; the doctor subcommand gains a `--listen` flag). Outcomes:
   - bind succeeds → `OK` (the listener is closed again; the probe only proves the port is free).
   - bind fails with `EADDRINUSE` → the holder is disambiguated (amendment below): it answers as a game-narrator and there is no second-install evidence → `OK` ("held by a running game-narrator <version>"); a holder that does not answer as a game-narrator, or a game-narrator holder with second-install evidence, → `FAIL` (the former with "another program holds <addr>", the latter with the two-install summary `another process holds <addr>; is a second game-narrator install running? (systemctl status game-narrator / pgrep -x game-narrator)` and a remedy naming both stop commands, with the `pkill -f` warning).
   - invalid address, permission, or other bind error → `FAIL` with the specific cause.
   - no address configured → `NOT CONFIGURED` (the row cannot probe what it cannot name; the CLI always resolves an address, so this state only appears on seams that do not supply one).
   The in-serve `/diagnostics` surface is unaffected: it runs the doctor's single-role probes, not the full report, so a serve process never probes a port it holds itself. The support bundle's doctor section (`internal/support`) passes the same resolved address, because the bundle is precisely the artifact sent when a deploy misbehaves and the port collision is the incident to explain.
3. **The manual section** (manual 19, "One install only" and "Deploying a dev build") records both layouts, the refusal, the tolerant-backup gotcha, the `pgrep -x`/`pkill -f` warning, and the cross-build recipe, so the knowledge lives in a reviewed document, not only in the script's comments.

## Amendment — 2026-10-21 (2nd run): a held address is disambiguated, not failed

**Trigger.** Review of the first-run diff found the row wrong for its main operator flow: operators run `game-narrator doctor` **while the narrator is serving**, so the configured address is normally held by the healthy, intended install — and the row reported `FAIL` every time.

**Change** (in `internal/doctor/net.go`, same check id):

- On `EADDRINUSE` the holder is asked who it is: a `GET /api/v1/system/info` to the loopback holder with a 2 s budget (`holderProbeBudget`). Loopback addresses only — a configured LAN or tailnet address is a remote destination and stays unprobed; this is a local disambiguation, not a network test. A 200 JSON answer with a non-empty `version` is the "answers as a game-narrator" signature. `/api/v1/health/live` is deliberately not an identifying signal: a bare liveness 200 is what any service answers.
- game-narrator holder, no second-install evidence → `OK` with the summary `held by a running game-narrator <version>` (the version read from system/info).
- Holder does not answer as a game-narrator → `FAIL` "another program holds <addr>: the holder does not answer as a game-narrator", with the `ss -ltnp` holder-finding remedy.
- game-narrator holder plus second-install evidence → `FAIL` with the unchanged two-install summary and the specific evidence in the detail. The doctor binary is itself named `game-narrator`, so `pgrep -x` always counts it; the evidence is the self-excluded count — at least two *other* processes (the holder plus a second install), or at least one other process while the unit shows `NRestarts > 0` and is not stably active (`SubState != active`): its process then never holds the port, so the game-narrator holder is a non-unit process — the exact 2026-10-21 collision. An *active* unit's `NRestarts > 0` is restart history, not a loop: its process is the stable holder and the row stays `OK`.
- The process and systemd probes sit behind the `InstallEvidence` interface (`GameNarratorProcesses`, `UnitRestartState`); the production implementation runs `pgrep -x` / `systemctl show` through the runner's `Exec` seam, and unit tests inject fakes, so the suite never shells out.
- The probe is bounded by the 2 s budget even for a wedged holder (AGENTS.md rule 7: the report must not hang on one unresponsive local process).

**Not changed.** The free-address, invalid-address, permission and no-address outcomes, the two-install summary/remedy wording, the `--listen` resolution, and the support-bundle wiring.

**Validation.** New tests in `internal/doctor/net_test.go`: `TestListenCheckOKWhenTheHolderIsTheNarrator` (a fake HTTP server answers system/info like the narrator → `OK` naming the version), `TestListenCheckFailWhenAnotherProgramHoldsTheAddress` (a plain listener → the foreign-holder `FAIL`), `TestListenCheckFailWhenTheHolderIsAForeignHTTPService` (a 404-style foreign service), `TestListenCheckFailWhenTwoNarratorProcessesAreRunning` (self-excluded pgrep count of 2 → the two-install `FAIL`), `TestListenCheckFailWhenTheUnitRestartLoopsAndANonUnitProcessHoldsThePort` (`NRestarts=503`, not stably active → the two-install `FAIL`), `TestListenCheckProbeIsBoundedWhenTheHolderNeverAnswers` (the 2 s budget), `TestListenRowIsOKInAFullRunWhenTheNarratorHoldsThePort`. `go test -count=1 -timeout 10m ./internal/doctor/...` → `ok`; all 12 `-run 'Listen'` tests PASS.

## Tradeoffs

- **One binary path, two installs.** Both installs use `/usr/bin/game-narrator` (the evidence layout), so a deploy of either mode replaces the binary both use. The `game-narrator.prev` rollback target is shared accordingly. A per-install binary split is a packaging decision this ADR does not make; the refusal makes the coexistence visible, which is the incident's actual cost.
- **The refusal is a hard stop, not a warning.** An operator who really wants both installs (different ports, different data dirs) must stop one deliberately first — the script prints exactly which command. That deliberate step is the point: two installs are a configuration the operator must own, not a state a deploy may discover mid-run.
- **The doctor row binds the port for a few microseconds.** A real bind is the only probe that distinguishes "free" from "held by somebody" without parsing `ss`/`netstat` output, and docs/spec/13 forbids reporting a component as working without exercising it. The window is a local syscall; there is no race worth designing around, and a miss is a serve-time error the operator already sees in the log.
- **`pgrep -x -u $USER` assumes the unit runs as a different user.** On the appliance the unit is `User=game-narrator` and the ssh login user is different, so the filter is exact. A box where the login user *is* the service user would need the refusal read as "any game-narrator process that is not the unit's" — noted here rather than encoded, since the appliance layout is the one this ADR binds to.

## Verification

- **Unit** (`internal/doctor/net_test.go`):
  - `TestListenCheckFailWhenAnotherProgramHoldsTheAddress` / `TestListenCheckFailWhenTheHolderIsAForeignHTTPService` — a port held by a program that does not answer as a game-narrator reports the foreign-holder `FAIL` with a non-empty remedy.
  - the amendment's disambiguation tests (holder answers as game-narrator → `OK`; two-install evidence → `FAIL`; the probe bounded by its 2 s budget) are listed in the amendment section above.
  - `TestListenCheckOKWhenTheAddressIsFree` — a released port reads `OK` with the address named.
  - `TestListenCheckNotConfiguredWithoutAnAddress` / `TestListenCheckUnparseableAddressIsFail` — the two non-bind states.
  - `TestListenRowIsNotConfiguredInAPlainRun` / `TestListenRowFailsAFullRunWhenThePortIsHeld` — the row is present in the full report and a held port makes `HasFail()` true (the exit-code-1 contract deploy tooling reads).
  - `internal/doctor/doctor_test.go` `TestRunReportsEveryComponentExactlyOnce` extended with the new id.
- **CLI** (`cmd/game-narrator`): the doctor subcommand resolves the address like serve and the `clearGatewayEnv` helper pins `GAME_NARRATOR_LISTEN` to a free port, so the exit-code assertions do not depend on whatever a developer box runs on the default address.
- **Script**: `bash -n scripts/board/deploy.sh` (syntax) and `shellcheck scripts/board/deploy.sh` (clean; one documented `SC2016` disable where `$HOME` must expand on the board, not locally) both pass. `--dry-run` for both modes prints the full step sequence and exits 0 without building, copying or contacting any host. **Not run against real hardware in this change** — the refusal/backup/stop paths were derived from the recorded incident evidence, and a live-board rehearsal is the follow-up (manual 19, the hardware gates in docs/spec/16).

## Consequences and reversal

- **Consequences.** A board with two installs cannot silently starve a deploy: the deploy refuses and names the holder, and `game-narrator doctor` flags the second install on `network.listen` before the crash loop starts (a port held by the single healthy install reads `OK` — the amendment — so the row is useful in the operator flow of running doctor while the narrator serves). The operator has one reviewed script for both layouts, with rollback (`game-narrator.prev`) and a data backup taken on every deploy.
- **Reversal.** Delete `scripts/board/deploy.sh`, the `network.listen` row (`internal/doctor/net.go`, its registration in `internal/doctor/doctor.go`, the `--listen` flag in `cmd/game-narrator`, and the test files); the report shape reverts to the pre-change check set and the manual sections can be removed. No schema, protocol or state change is involved.

## Links

- docs/manuals/19-install-on-uno-q.md — "One install only (systemd unit vs user-level run)" and "Deploying a dev build".
- ADR-0126 — the sibling warning (stale default data dir) for the "two installs, wrong data dir" half of the incident.
- docs/spec/13 — the diagnostic state vocabulary this row uses (OK / DEGRADED / BLOCKED / NOT CONFIGURED / FAIL).

## Amendment (2026-10-22): the user-level install starts at boot

A board power-cycle left nothing narrating, because the user-level install was a `setsid` process. `packaging/systemd/user/game-narrator.service` runs it in the user's systemd instance: it shares the PipeWire session the hub-HDMI sink lives in, starts at boot with lingering, and restarts on failure. That covers a tailnet listen address that is not up yet. `deploy.sh --user` stops and starts through the unit when it is enabled. Without the unit it falls back to `setsid` and says the process will not survive a reboot. Manual 19 has a "Starting at boot" section. Verified on the board: the unit is enabled with `Linger=yes`, the narrator runs under it, and a live session started on it.
