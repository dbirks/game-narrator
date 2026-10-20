# AGENTS.md — mandatory working agreement for coding agents

> Applies to coding agents and human contributors. Read `README.md` and all relevant `docs/spec/*` sections before implementing a subsystem. The full product, not a minimal demo, is the target.

## Operating principles

1. **Own outcomes, not just diffs.** Inspect the environment, identify prerequisites, run validation, debug failures, and do not claim success without evidence. You may make locally reversible decisions without repeated permission. Ask only for credentials, inaccessible private game files, destructive operations, irreversible publication, or truly blocked information.
2. **Maintain ADRs.** Any decision impacting component boundaries, persistence, protocol, testing, dependency, performance, security, or release gets a numbered `docs/adr/NNNN-slug.md` prior to or alongside the change. Write context, options, decision, tradeoffs, validation, consequences, and reversal plan. Update superseded ADRs rather than rewriting history.
3. **No simulation theater.** Tests must distinguish (a) synthetic stubs, (b) captured-frame replay, (c) Dolphin emulator, (d) real model providers, (e) UNO Q hardware. Never label a mocked workflow as real end-to-end. Preserve provider-request proof for live tests.
4. **First reproduce, then repair.** Capture failure with event traces, correlated screenshots, API exchanges (with secrets redacted), audio artifact, and a failing automated assertion. Apply the smallest fix; rerun the focused test and adjacent regressions.
5. **Never commit third-party ROMs, game-script dumps, copyrighted art, voice samples without rights, credentials, personal data, or private endpoint URLs.** Use externally configured fixture locations and hashes. All imports are provenance-aware.
6. **Respect intended agency.** Research is autonomous but tools are allowlisted; downloads are untrusted; voice/style selection is reviewable; game publication requires configured policy. No arbitrary shell execution by setup agent.
7. **Keep runtime predictable.** Deadline/cancellation propagation, immutable scene observations, monotonic event sequence, bounded queues, explicit backpressure, safe audio output, graceful degraded modes.
8. **Use typed interfaces and compatibility tests** for model providers; do not assume all APIs follow OpenAI chat semantics. Verify actual installed versions, capabilities, and image/input size limits.
9. **Web frontend is a first-class product.** Exercise real pages through Playwright; inspect browser errors; assert event correlation; capture traces/screenshots and accessibility results.
10. **Version and migration discipline.** Every schema change has forward migration and backup/restore test; keep data backward compatibility within policy; audio cache keys include versions.
11. **Bounded autonomous troubleshooting.** Retry only idempotent operations with max attempts. Prefer local logs/documentation, then upstream project docs. Do not wipe user state to make tests pass. Escalate blockers with evidence and an unblock plan.
12. **No release until gates pass.** Gate list in `docs/spec/16-implementation-and-acceptance.md`, including hardware smoke where hardware is available. If unavailable, report explicitly as unverified and do not present final hardware acceptance as passed.

## Expected work loop

`inspect → decide/ADR → implement → unit tests → fixture replay → Playwright → Dolphin real E2E → provider integration → performance → documentation → report`

For each milestone create an evidence bundle (`artifacts/<run-id>/`) with:
- manifest (git SHA, dependency versions, OS, model IDs, emulator build, game image hash *only*, timestamp);
- JUnit/Go test summaries and Playwright HTML trace/report;
- timeline of frames/OCR/ranked candidates/Clef decisions/playback and linked redacted raw requests;
- resulting audio WAV and ASR evaluation where test data permissions permit;
- pass/fail and deviation from benchmark; known flakiness and reproduction commands.

## CLI targets expected in actual implementation

`make bootstrap`, `make dev`, `make test-unit`, `make test-replay`, `make test-ui`, `make test-dolphin`, `make test-models`, `make test-hardware`, `make test-full`, `make lint`, `make release-snapshot`, `make diagnostics`.

Implement targets; **do not** mark pending targets green by printing a success message. `make test-full` should run all environment-available gates and clearly mark unavailable hardware/private dependencies as `NOT RUN` with actionable setup instructions.

## Minimum quality policy

- `go test ./...`, race testing of concurrent packages, TypeScript typecheck, lint, migrations and API contract tests pass.
- Browser tests assert visible state, not merely HTTP 200.
- Live E2E asserts that real frame input was recognized, a genuine decision-provider request took place, TTS produced audio bytes, playback was observable, and the developer UI showed matching correlation IDs.
- Restart tests demonstrate persistent games/voices/cache and resumed or safely failed jobs.
- Failure-injection tests cover dropped frames, delayed/duplicated model responses, endpoint loss, voice missing on a replica, and rapid textbox advances.

## File improvements as GitHub issues

When you notice an improvement that is outside the current task — a missing one-shot CLI command, a diagnostic that would have saved time, an awkward manual step, a tool that lies on real hardware, a doc gap — do not let it vanish into a chat log or a report. File a GitHub issue (`gh issue create --label enhancement` or `--label bug`) with: what you were doing, what was missing or wrong, the evidence (commands and output, redacted), and a suggested shape for the fix. Search open issues first (`gh issue list --search`) to avoid duplicates. The repository is public: issues follow rule 5 — no hostnames, IPs, tokens, private endpoint URLs, game text, or frames. Fix it in the current change instead when it is small and in scope.

**Codify one-offs.** When you get something working by hand on a real device (pairing a speaker, enabling a capture mode, a workaround for an OS quirk), the manual steps are the first draft of a feature, not the end. Record the exact commands that worked, the gotchas and the verification output. Then codify them: a setup guide section in `docs/manuals/`, a read-only preflight/doctor check, and — when an operator would reasonably want to do it again — a flow in the device web UI. Do it in the same change when it is small; otherwise file an issue containing the working recipe so the implementer starts from proven steps.

## Engineering choices not to reopen casually

Go + React/TypeScript/Vite; SQLite; embedded SPA assets; provider adapters; Arch development + Dolphin; Debian ARM64 UNO Q release target; external UVC capture and USB audio; user-supplied content; GoReleaser; developer mode; ADRs; testing first-class. Revisit only with quantified evidence and an ADR.
