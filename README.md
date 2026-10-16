# Game Narrator

Game Narrator watches a copy of a game's video signal, reads the dialogue that
appears on screen, works out which character is speaking, and plays the line
back in a consistent generated voice. For a GameCube that means an analog-to-HDMI
converter into a powered 1×2 HDMI splitter: one leg goes straight to the TV, the
other into a UVC HDMI capture dongle plugged into an Arduino UNO Q running Linux,
with speech out to a USB sound device — the picture on your TV never passes
through this app. The same pipeline also runs against Dolphin on an Arch Linux
development host, with no UNO Q involved.

## Status

Early development — Milestone 0 (environment/discovery and executable
infrastructure) is in progress and nothing after it has started. What runs today
is a Go service with an embedded React UI, SQLite migrations, model-role
configuration and a Playwright-checked diagnostics page; treat everything else
as designed but unbuilt.

| Milestone | Scope (see [docs/spec/16](docs/spec/16-implementation-and-acceptance.md)) | Status      |
| --------- | ------------------------------------------------------------------------- | ----------- |
| M0        | Environment/discovery and executable infrastructure                       | in progress |
| M1        | Full foundation and portability                                           | not started |
| M2        | Video + recognition engine                                                | not started |
| M3        | Model integration and knowledge agent                                     | not started |
| M4        | Complete voice and playback pipeline                                      | not started |
| M5        | Complete React product                                                    | not started |
| M6        | Dolphin live driver and fixture factory                                   | not started |
| M7        | Full live-model and adverse-condition validation                          | not started |
| M8        | UNO Q and distributable release                                           | not started |

Unimplemented gates (`test-replay`, `test-dolphin`, `test-models`,
`test-hardware`, `release-snapshot`) print `NOT RUN: ...` and fail — they can
never report success before the work exists.

## How it works

Every runtime path shares this pipeline. It is the design from
[`docs/spec/05`](docs/spec/05-runtime-recognition.md) and
[`docs/spec/07`](docs/spec/07-voice-audio.md), built in M2–M4 — none of it runs
yet.

1. Capture a frame: a UVC HDMI dongle on the UNO Q, Dolphin on a dev host, or a
   recorded fixture in CI.
2. Detect the dialogue text box in a configured region of that frame.
3. Run OCR over the region (local Tesseract, with a vision-model fallback).
4. Match the recognized text against the imported game script to rank candidate
   messages.
5. Decide who is speaking, and what kind of text this is — that is the job of
   the `decision` role: a typed classifier (Clef-Omni over the System One
   protocol, [`docs/spec/06`](docs/spec/06-inference-contracts.md)) combined
   with the character/scene graph and the local match scores, with explicit
   abstention when it is not confident. The general `fast_reasoning` model is
   not the speaker decider; it is only consulted for unresolved OCR or unusual
   layout.
6. Synthesize the line in that character's approved voice, or reuse a cached
   clip keyed on voice and text versions.
7. Play it to the configured audio output, and cancel playback the moment the
   player advances the text box.

Recognition and audio work happen on the device; model inference (vision
fallback, speaker decision, speech synthesis, ASR quality checks) runs on a
separate GPU gateway reachable over HTTP, not on the UNO Q.

## Quickstart (Arch Linux dev host)

Arch Linux with `sudo` is enough to start: the bootstrap script installs Go,
Node/pnpm, make and the rest.

```sh
scripts/bootstrap-arch.sh --dry-run                    # preview pacman commands, install nothing
scripts/bootstrap-arch.sh                              # install dev packages (idempotent, uses sudo)
pnpm --dir web exec playwright install chromium        # browser used by make test-ui

make build                                             # Vite build -> embedded into bin/game-narrator
bin/game-narrator migrate                              # apply pending SQLite migrations
bin/game-narrator serve                                # http://127.0.0.1:8080/diagnostics
bin/game-narrator doctor --dev                         # same as: make diagnostics
```

Then open <http://127.0.0.1:8080/diagnostics> — it shows build version/commit,
Go version, `os`/`arch` and readiness read live from the API.

`bin/game-narrator doctor` (`make diagnostics`, which builds the binary first)
probes storage, native tools, OCR, capture, audio and every model role, and
reports each one as `OK` / `DEGRADED` / `BLOCKED` / `NOT CONFIGURED` / `FAIL` —
exiting 1 only when a configured component is actually broken.

For development, `make dev` runs **only** the Go server from source
(`go run ./cmd/game-narrator serve`); it serves the last `make build` UI, so it
picks up Go changes, not `web/` edits. For UI hot reload, run the Vite dev
server in a second terminal:

```sh
make dev                                               # go run ./cmd/game-narrator serve
pnpm --dir web dev                                     # Vite on http://127.0.0.1:5173
```

Vite listens on `127.0.0.1:5173` and proxies `/api` to `127.0.0.1:8080`, so you
open <http://127.0.0.1:5173/diagnostics> while the Go server keeps running on
8080 (set `GAME_NARRATOR_API` if the API is on another port).

Verification gates that exist today:

```sh
make test-unit    # go vet + go test -race ./...
make test-ui      # frontend checks, then Playwright against a real embedded binary
make lint         # gofmt/vet + oxlint + oxfmt --check
make diagnostics  # build, then bin/game-narrator doctor --dev
make test-full    # runs the gates above, exits non-zero while others are NOT RUN
```

`go test ./...` stays green with no network access: live provider tests skip
with `NOT RUN` unless a gateway is configured.

Useful knobs for `serve` and `migrate`:

- `GAME_NARRATOR_LISTEN` (or `--listen`) — TCP address, default `127.0.0.1:8080`.
- `GAME_NARRATOR_DATA_DIR` (or `--data-dir`) — durable data directory holding
  `game-narrator.sqlite`; defaults to `$XDG_DATA_HOME/game-narrator`, else
  `$HOME/.local/share/game-narrator`. `bin/game-narrator migrate --data-dir "$(mktemp -d)"`
  is handy for a throwaway database.
- `serve` never migrates on boot; run `migrate` explicitly so a running service
  cannot be upgraded out from under itself.

## Configuration

Copy `.env.example` and fill it in — every value there is a placeholder on a
`.invalid` host, so a forgotten default fails loudly instead of pointing at
somebody else's server. Model **roles** are configured by environment variables
(`GAME_NARRATOR_ROLE_*`), not raw URLs in a config file; a role is enabled only
when its model is set, and preflight reports the rest as `NOT CONFIGURED` while
the service keeps running. In this build only the chat-style roles have adapters
(`agent_reasoning`, `fast_reasoning`, `vision_ocr_fallback`); `decision`,
`voice_design`, `voice_clone` and `speech_asr` are representable in configuration
but not implemented. `examples/config/` holds the illustrative YAML/env samples
from the spec kit. Never commit `.env` — it is gitignored, and so is any real
token.

## Game content and copyright

This repository ships no games, no game scripts, no art and no voices. You
supply your own legally obtained game image through `GAME_NARRATOR_GAME_IMAGE_PATH`
(and it is referenced read-only — never copied into the repo, the data directory
or an evidence bundle). Disc images, savestates and input movies are gitignored;
do not commit them, and do not commit screenshots that show copyrighted visuals.

## Repository layout

```
cmd/game-narrator/      CLI entry point (serve | migrate | doctor | version)
internal/api/           HTTP handlers, middleware, static SPA serving
internal/bootstrap/     config, server lifecycle, build/version info
internal/providers/     model-role registry + OpenAI-compatible chat adapter
internal/store/         SQLite access, embedded migrations (store/migrations/)
internal/webassets/     go:embed mount point for the built web assets
web/                    React + TypeScript + Vite UI, Vitest tests, Playwright e2e/
scripts/                bootstrap-arch.sh; spikes/dolphin/ capability probe
docs/spec/              parts 00-18: the implementation contract
docs/adr/               architecture decision records
docs/spikes/            measured capability findings (Dolphin headless)
docs/agents/            onboarding-agent prompts and workflow docs
docs/reference/         primary-source links and limitations
docs/test-plan/         scenario catalog
examples/               nonproduction config and scenario samples
bin/ , artifacts/       build output and evidence bundles (both gitignored)
```

Decision records: the substantial choices behind the layout above are ADRs
0001–0014 in [`docs/adr/`](docs/adr/) — read the relevant one before reopening
a boundary, a persistence format or a provider protocol.

## Specification

The documents below are the implementation contract; code follows them, and
substantial departures get an ADR.

### Reading order

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

### Non-negotiable invariants

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

## Contributing

Read [`AGENTS.md`](AGENTS.md) first (it is also available as `CLAUDE.md`) — it is
the working agreement: inspect before deciding, keep milestones honest, and never
present a mocked workflow as real end-to-end. Any decision that changes
component boundaries, persistence, protocol, dependencies, security or the
release process needs a numbered ADR in `docs/adr/` before or alongside the
change. Tests must prove real behaviour: browser tests assert visible state, not
HTTP 200, and a gate that is not implemented reports `NOT RUN` rather than
passing. `make help` lists the targets.

## License

Not yet chosen.
