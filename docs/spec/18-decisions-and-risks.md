# 18 — Decision register, research claims and risk ledger

## Chosen architecture (user-confirmed)

- Go service, React/Vite embedded via `go:embed`; SQLite, source-aware assets and durable jobs.
- Arduino UNO Q 4 GB Debian/Linux on real hardware with UVC HDMI capture over powered USB-C hub; existing RCA-to-HDMI converter and powered splitter, separate TV branch; USB audio baseline.
- Linux Arch development host with passwordless sudo for authorized agent; Dolphin installed and user-provided original game file; full reproducible emulator E2E is a primary success criterion.
- Self-hosted inference gateway with role-based model routing, separately hosted Qwen3-TTS VoiceDesign/Base and Qwen3-ASR; Clef-Omni as typed decision provider.
- Game onboarding agent may research, prepare and ask when genuinely stuck; approved source material imported by user, no redistribution; human review at publication initially.
- Character dialogue plus intentional sign/item narration; fast/predictive when high-confidence, player remains in control; narrator voice/abstain on uncertainty.
- GoReleaser for arm64/amd64 binaries, Linux packages and OCI images. Keep ADRs under `docs/adr`.

## Unverified technical assumptions to test rather than assert

- Capture dongle behaves as UVC on UNO Q kernel, accepts your RCA converter's output and operates within hub bandwidth/power budget.
- Dolphin with chosen pinned Arch build can render screenshots under selected isolated headless/display setup; input pipe vs virtual controller usable in that build.
- Actual inference gateway forwards `/v1/systemone`, `/v1/audio/speech` and voice registration/list routes with necessary payload types, auth and instance affinity; if not, dedicated per-role services must be configured.
- Qwen3-TTS Base registration persists/reconciles in your deployment; sampling seed available via chosen serving mode (not guaranteed for HTTP endpoint).
- Local UNO Q OCR on actual captured GameCube composite image meets useful accuracy/latency. Need empirical calibration.
- ASR suitable for validating pronunciation/non-omission of selected generated dialogue; cannot certify actor identity/emotional delivery on its own.
- License/consent status of each user-supplied or web-imported source verified individually.

## Risks and mitigations

| ID | Risk | Severity | Mitigation | Test proof |
|---|---|---|---|---|
| R-01 | OCR fails on analog/composite video | High | choose scaling/masking/alternate captures, VLM fallback, ROI calibration | device sample CER and visual fixture |
| R-02 | Wrong speaker due to repeated phrases | High | graph+scene+Clef+unknown; calibrate speaking precision | repeated-phrase holdout cases |
| R-03 | Player skips before speech starts | High | generation fence, interrupt, cache/prefetch | rapid-advance audio timeline |
| R-04 | Registry voice ID missing on GPU replica | High | instance affinity, reconcile, inline reference fallback | routed multi-instance reset test |
| R-05 | Dolphin headless display/input brittle | High | empirical driver spike; virtual display alternatives; replay fallback | actual frame and controlled input proof |
| R-06 | Agent imports wrong text/edition | High | edition hash+source evidence, diff and approval | conflicting source challenge |
| R-07 | GPU contention makes live latency poor | Medium | model QoS, dedicated roles, prefetch and cache | concurrent load test |
| R-08 | UNO Q storage/power/thermal limits | High | remote assets, buffer caps, PD power and soak | 1h capture/hotplug/telemetry |
| R-09 | Copyright and voice rights | High | user-owned imports, no bundled assets, consent and export filters | artifact scan and approval logs |
| R-10 | Agent prompt injection/SSRF | High | safe tools, fetcher allowlists, review scopes | adversarial suite |
| R-11 | Model output nondeterministic | Medium | recorded providers for regression, threshold live tests | replay vs live run comparison |
| R-12 | Product too tailored to Zelda | High | generic adapter contract, synthetic second-game test | no game-specific core conditional |
| R-13 | Long WAV caching exhausts disk | Medium | content hashes, quotas, LRU, remote store | cache quota/eviction tests |
| R-14 | Untrusted game parser binary execution | High | no arbitrary plugin execution, sandbox parser | malicious archive suite |
| R-15 | GoReleaser OCI/lib dependencies mismatch | Medium | docker v2, native app/package smoke | arm64/amd64 clean installs |

## ADR rule

An unverified assumption becoming a design choice **requires an ADR** with validation evidence, chosen version and fallback. Initial ADRs are seeded but can be superseded; keep permanent history.

## Success criteria mapping

Use parts 00 and 16 as the top-level requirement/acceptance index. Track risk mitigation tests in the quality dashboard; R-01 through R-05, R-09, R-10 and R-12 must be explicitly addressed before a hardware release.
