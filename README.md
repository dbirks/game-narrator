# Game Narrator — implementation specification

**Status:** Proposed implementation contract · **Version:** 1.0-draft · **Prepared:** 2026-10-16

Game Narrator is a self-hosted, game-agnostic AI dubbing and narration appliance. A Go service on an Arduino UNO Q (4 GB) inspects **a copy** of a game's video signal, recognizes contextual text, finds the correct character, and plays consistent generated speech. A React/Vite UI embeds into the Go executable. The development stack runs on an Arch Linux host with Dolphin, Playwright, deterministic frame replay, and real-model inference tests. An autonomous, tool-using setup agent creates portable profiles for **user-provided games/content**.

**This repository is a specification and planning kit, not a working implementation.** No emulator, physical capture, endpoint integration, or end-to-end tests have been executed as part of its creation. Example artifacts are deliberately synthetic and must not be represented as validated game data.

## Reading order

| Part | File | Deliverable |
|---|---|---|
| 0 | [Product charter](docs/spec/00-product-charter.md) | Scope, UX, release criteria |
| 1 | [Architecture](docs/spec/01-system-architecture.md) | Components, interfaces, trust boundaries |
| 2 | [Canonical data model](docs/spec/02-data-and-storage.md) | SQLite, asset hashes, versioned game profiles |
| 3 | [Onboarding agent](docs/spec/03-onboarding-agent.md) | Autonomous tools, prompts, approval, provenance |
| 4 | [Game adapters](docs/spec/04-game-adapters.md) | Multi-game capture/text/speaker portability |
| 5 | [Runtime recognition](docs/spec/05-runtime-recognition.md) | Detection, OCR, graph matching, prediction |
| 6 | [Inference adapters](docs/spec/06-inference-contracts.md) | Clef, vision, ASR and gateway protocol |
| 7 | [Voice and audio](docs/spec/07-voice-audio.md) | VoiceDesign, Base, registry, ASR QA, playback |
| 8 | [Service API](docs/spec/08-api-and-events.md) | HTTP contracts, job and event stream |
| 9 | [Frontend](docs/spec/09-ui-and-workflows.md) | React pages, developer mode and accessibility |
| 10 | [Infrastructure](docs/spec/10-hardware-and-deployment.md) | Arch + UNO Q + capture + secure runtime |
| 11 | [Emulator and E2E](docs/spec/11-dolphin-automation.md) | Live control, DTM, fixtures, headless preflight |
| 12 | [Verification](docs/spec/12-test-strategy.md) | Test pyramid, scenario matrix, confidence gates |
| 13 | [Operability](docs/spec/13-observability-and-resilience.md) | Tracing, diagnostics, recovery, budgets |
| 14 | [Security](docs/spec/14-security-and-content.md) | Prompt injection, SSRF, copyright, voice consent |
| 15 | [Publishing](docs/spec/15-release-and-goreleaser.md) | GoReleaser, Debian/Arch/container artifacts |
| 16 | [Execution plan](docs/spec/16-implementation-and-acceptance.md) | Complete work breakdown and exit requirements |
| 17 | [Contracts and examples](docs/spec/17-example-contracts.md) | Example JSON, YAML, policies and golden events |
| 18 | [Decision register](docs/spec/18-decisions-and-risks.md) | Locked decisions, risks and unresolved experiments |

Supporting materials:
- [`AGENTS.md`](AGENTS.md): mandatory agent operating agreement and definition of done.
- [`docs/agents/`](docs/agents/): agent system prompt, troubleshooting, fixture authoring and acceptance rubric.
- [`docs/adr/`](docs/adr/): ADR policy and initial decisions; document every substantial subsequent decision.
- [`docs/reference/sources.md`](docs/reference/sources.md): verified primary-source technical links, limitations, research date.
- [`docs/test-plan/scenario-catalog.md`](docs/test-plan/scenario-catalog.md): extensive cross-game, visual, audio, failure and hardware test catalog.
- [`examples/`](examples/): nonproduction example configurations and illustrative scenario schema.

## Non-negotiable invariants

1. The original gameplay signal **never** traverses the inference path; the TV sees the hardware splitter's other output.
2. The UNO Q does not need HDMI input or the game to be emulated on the UNO Q; capture arrives through Linux UVC/V4L2.
3. No copyrighted game image, scripts, proprietary assets, or non-consensual cloned voice samples are bundled in distributed releases.
4. Production uses real recognition/audio code; mocks are permitted only for isolated tests and recorded-model fixtures, clearly labeled.
5. Each speech task and observation is session-scoped, ordered, cancellable, traceable, and idempotent.
6. The *canonical* voice assets live in Game Narrator's configured durable asset storage, not solely on inference servers.
7. An inference gateway must not cause voice registrations to silently disappear on replica rerouting.
8. The onboarding agent can research automatically but cannot silently trust external instructions or publish unverified assets.
9. The agent implementing this project must create, run, inspect, and report reproducible end-to-end tests — including **real Dolphin** paths when the authorized test host is available.
10. Release completion requires the **full scope and acceptance matrix**, not merely one demo vertical slice.

## Spec-to-repo instructions

Use this package as initial repository documentation; keep path structure. Implementation should create `/cmd`, `/internal`, `/web`, `/migrations`, `/testdata` and deployment files in the repository root as described. Keep speculative values clearly labeled; convert to measured thresholds during benchmark calibration. No credentials or third-party game bytes go into Git.
