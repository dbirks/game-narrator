# Technical research references and verification notes

**Research date:** 2026-10-16. These are upstream documentation or closely relevant implementation references. Links may change. Coding agent must pin actual installed versions, run capability probes, and update this file and ADRs if behavior differs. This file is **not** an endorsement of scraping, distributing games, or performing actions without user-supplied access.

## Primary sources

1. [Arduino UNO Q user manual](https://docs.arduino.cc/tutorials/uno-q/user-manual/) — Debian Linux MPU, dedicated STM32 microcontroller, 2GB/4GB models, PC/SBC modes, USB-C multiport adapter.
2. [Arduino UNO Q power specification](https://docs.arduino.cc/tutorials/uno-q/power-specification) — USB-C 5V/3A PD contract and alternate power paths. Hub/device power requires validation.
3. [vLLM-Omni TTS API](https://docs.vllm.ai/projects/vllm-omni/en/stable/serving/speech_api/) — Qwen3-TTS VoiceDesign/Base/CustomVoice checkpoint split, `/v1/audio/speech`, `GET/POST/DELETE /v1/audio/voices`, multipart `audio_sample`, `ref_text`, `consent`, persisted `.safetensors`, `SPEAKER_SAMPLES_DIR`, immutable registration policy, multi-frontend restrictions, possible precomputed voice loading and streaming modes. **Caveat:** model-specific `seed` support must be probed in actual HTTP serving mode.
4. [vLLM-Omni Qwen3-TTS recipe](https://github.com/vllm-project/vllm-omni/blob/main/recipes/Qwen/Qwen3-TTS.md) — sample server commands for all three Qwen3 checkpoints, model/task distinctions, streaming examples. Community-maintained; verify chosen version.
5. [vLLM Structured Decisions / System One](https://docs.vllm.ai/en/latest/serving/online_serving/structured_decisions/) — `/v1/systemone` body, question types/criteria, structured answer probabilities, Clef-Omni pooling behavior, multimedia inputs, restrictions on `instructions`, model/server-specific limits. Example request shape is copied *structurally*, not with specific fixture content.
6. [Cloudflare Clef-Omni release](https://developers.cloudflare.com/changelog/post/2026-10-09-clef-omni-workers-ai/) — October 9, 2026 release and audio/video/text/image input capabilities. It is new, so account for model/backend changes.
7. [Dolphin emulator command-line reference](https://github.com/dolphin-emu/dolphin/blob/master/Readme.md) — `--user`, `--movie`, `--exec`, `--save_state`, `--batch`, `--video_backend`. Crucially, `--batch` means no conventional UI, not a guaranteed fully headless renderer.
8. [Dolphin controller configuration](https://dolphin-emu.org/docs/guides/configuring-controllers/) — emulated GameCube controller mapping. Version-specific scripted input methods require empirical spike; no blanket guarantee.
9. [CloudModding generic BMG](https://wiki.cloudmodding.com/zgcn/BMG) — message-format sections and Zelda/Mario-related variants.
10. [CloudModding Wind Waker BMG](https://wiki.cloudmodding.com/zgcn/WW:BMG) — game-specific message attributes, message ID, possible next message ID, textbox style/typewriter flag. Many fields partially documented; preserve unknowns.
11. [Tesseract improve OCR quality](https://tesseract-ocr.github.io/tessdoc/ImproveQuality.html) — preprocessing, small-region segmentation modes, PSM 6/7.
12. [Go `embed` package](https://pkg.go.dev/embed) — embed static React/Vite assets into Go executable.
13. [GoReleaser Docker v2](https://www.goreleaser.com/customization/package/dockers_v2/) — preferred `dockers_v2` multiarch Buildx path, warning about provisional name until v3.
14. [GoReleaser v2.16 release](https://goreleaser.com/blog/goreleaser-v2.16/) — Docker v2 nonexperimental/production ready as of 2026.
15. [GoReleaser getting started](https://www.goreleaser.com/getting-started/quick-start/) — `goreleaser check`, snapshots, single-target build and publishing prerequisites.
16. [GoReleaser nFPM integration](https://www.goreleaser.com/customization/package/nfpm/) — `.deb`, Arch and other Linux packages.
17. [Playwright HTML reporter](https://playwright.dev/docs/test-reporters) — browser-test HTML evidence.
18. [Playwright screenshots/video/trace](https://playwright.dev/docs/test-use-options) — failure trace and browser artifacts.
19. [TheGamesDB API OpenAPI definition](https://github.com/TheGamesDB/TheGamesDBv2/blob/master/API/spec.yaml) — candidate metadata/cover-art lookup requiring own API credentials and rights review.

## Integration caveats to revisit during implementation

- **Dolphin input pipes & MemoryWatcher:** third-party examples exist but can be obsolete; verify against installed Dolphin before promising programmatic controls. DTM+savestates are verified CLI abilities; live controller injection is an implementation spike, with virtual gamepad fallback.
- **Qwen3-ASR:** deployment route and output schema depend on chosen serving implementation. Do not hardcode OpenAI-compatible routes unless verified. Store the capability contract after actual probe.
- **Inference gateway:** multiple replicas may each have separate uploaded voice registries. A global alias is not sufficient to prove persistent voice IDs; direct/affinity routing or voice synchronization is essential.
- **GoReleaser config:** exact YAML option syntax can change. Generate with installed `goreleaser init` / validate with `goreleaser check`; don't treat examples as production-tested.
- **Game content:** BMG and fan-maintained dumps are technical reference formats/data sources, not licensed text for redistribution. No game-specific copyrighted material included here.
- **CLEF confidence:** probabilities are choice scores; calibrate with held-out gameplay labels and do not claim guaranteed truth.
- **UNO Q capability:** hardware manuals establish Linux/USB support but cannot guarantee your specific powered hub/capture card/audio sink function together. Physical preflight is mandatory.

## Documentation update policy

Every externally dependent API protocol has a recorded version and current verification date in `docs/reference/sources.md`. If a provider/backend changes, update compatibility contract tests and cite a source/commit. Maintain a `docs/adr/` entry for consequential changes.
