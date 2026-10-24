# Operational manuals

The twelve runbooks required by [docs/spec/13](../spec/13-observability-and-resilience.md),
"Operational manuals required", plus the UNO Q hardware-smoke runbook (M8,
docs/spec/16) and the workflow manuals for the corrections/Tests page,
the Dolphin fixture recorder and savestates, verifying published
artifacts, and the UNO Q install runbook. Each file states its purpose,
preconditions,
steps (real commands), verification, and what NOT to do. Anything that depends
on a feature that is not committed yet is marked `NOT YET IMPLEMENTED` with the
tracking item, and hardware-dependent claims are marked `NOT VERIFIED` per
AGENTS.md rule 12.

| Manual | File |
| --- | --- |
| First boot | [01-first-boot.md](01-first-boot.md) |
| Safe dependency updates | [02-safe-dependency-updates.md](02-safe-dependency-updates.md) |
| Storage migration / backup / restore | [03-storage-migration-backup-restore.md](03-storage-migration-backup-restore.md) |
| Inference gateway connection | [04-inference-gateway-connection.md](04-inference-gateway-connection.md) |
| Voice registry maintenance | [05-voice-registry-maintenance.md](05-voice-registry-maintenance.md) |
| Recovery after hardware disconnect | [06-recovery-after-hardware-disconnect.md](06-recovery-after-hardware-disconnect.md) |
| Test environment reset | [07-test-environment-reset.md](07-test-environment-reset.md) |
| Dolphin profile reset | [08-dolphin-profile-reset.md](08-dolphin-profile-reset.md) |
| Troubleshooting black frames | [09-troubleshooting-black-frames.md](09-troubleshooting-black-frames.md) |
| Diagnosing wrong speaker | [10-diagnosing-wrong-speaker.md](10-diagnosing-wrong-speaker.md) |
| Purging redacted support bundles | [11-purging-support-bundles.md](11-purging-support-bundles.md) |
| Rollback after failed release | [12-rollback-after-failed-release.md](12-rollback-after-failed-release.md) |
| UNO Q hardware smoke (M8) | [13-uno-q-hardware-smoke.md](13-uno-q-hardware-smoke.md) |
| Corrections and the Tests page (operator) | [14-corrections-and-tests-page.md](14-corrections-and-tests-page.md) |
| Dolphin fixture recorder and the test-dolphin gate (developer) | [15-dolphin-fixture-recorder.md](15-dolphin-fixture-recorder.md) |
| Dolphin savestates and their dev dependencies (developer) | [16-dolphin-savestates.md](16-dolphin-savestates.md) |
| Verifying published artifacts (release) | [17-verifying-published-artifacts.md](17-verifying-published-artifacts.md) |
| HDMI capture bring-up on the UNO Q (dongle + hub) | [18-hdmi-capture-on-uno-q.md](18-hdmi-capture-on-uno-q.md) |
| Install and run on an Arduino UNO Q | [19-install-on-uno-q.md](19-install-on-uno-q.md) |
| Cast voice-gender audit (find/fix mismatched voices) | [20-cast-voice-gender-audit.md](20-cast-voice-gender-audit.md) |

Blocked items these manuals point at are tracked in
[docs/status/blocked.md](../status/blocked.md); the release runbook is
[docs/release.md](../release.md).
