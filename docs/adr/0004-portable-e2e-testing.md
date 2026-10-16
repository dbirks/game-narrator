# ADR-0004: Deterministic replay plus live Dolphin and hardware evidence
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: FR-13, GN-T001–T080

## Context
Coding agents must independently verify real outcomes, not merely compile or run mocks. Dolphin is available on Arch after user provisioning. DTM/savestate compatibility and headless graphics may vary.

## Decision
Separate recorded frame/mocked-model tests from real Dolphin rendering/input, live inference and physical UNO Q tests. Pin emulator/config/game hash, keep fixtures private by default, require actual frame/audio/model traces and Playwright correlation. HTML/JSON report emitted for every run.

## Consequences
CI remains fast and reliable; real-model/nightly/hardware suites require more setup. Do not call a DTM or `--batch` launch successful without observed rendered checkpoints.

## Verification
Complete suite in part 12 and actual E2E commands; reproducible labeled screenshots and UI traces. Rollback/fallback is frame replay with explicit `real_emulator_not_run` status, not a passing real test.
