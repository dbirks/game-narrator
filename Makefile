# Game Narrator — development and release targets (AGENTS.md "CLI targets")
#
# A gate prints "NOT RUN: <setup instruction>" when its capability is not
# configured on this host and exits non-zero, so a gate is never green
# without real evidence (AGENTS.md "CLI targets expected in actual
# implementation"; the NOT RUN convention per ADR-0009/0013). The go-test
# gates run through scripts/gates/run-go-gate.sh, which classifies each test
# PASS/FAIL/SKIP/NOT RUN and exits 0 (all ran and passed), 1 (any FAIL, or
# go test itself failed) or 2 (nothing failed but at least one NOT RUN). The
# go gates record that real code in artifacts/gates/<gate>.rc because GNU
# make maps any recipe failure to its own exit status 2 (documented
# behaviour, not configurable); test-full reads those files so FAIL (1) and
# NOT RUN (2) stay distinct.

MODULE  := github.com/dbirks/game-narrator
BIN     := bin/game-narrator
PKG     := $(MODULE)/internal/bootstrap
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X $(PKG).version=$(VERSION) -X $(PKG).commit=$(COMMIT)

# Gate targets that test-full runs, in order.
RUN_GATES := test-unit test-ui diagnostics release-snapshot bench \
             test-replay test-dolphin test-models test-hardware

.PHONY: bootstrap dev build web-build test-unit test-replay test-ui test-dolphin test-models \
        test-hardware test-full lint release-snapshot diagnostics bench soak

## web-build: build the React UI and stage it for go:embed
web-build:
	pnpm --dir web install --frozen-lockfile
	pnpm --dir web build
	@rm -rf internal/webassets/dist
	@mkdir -p internal/webassets/dist
	@cp -a web/dist/. internal/webassets/dist/
	@: > internal/webassets/dist/.keep

## build: compile the game-narrator binary with the UI embedded and version/commit stamped in
build: web-build
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/game-narrator ./cmd/game-narrator

## dev: run the API server from source
dev:
	go run ./cmd/game-narrator serve

## test-unit: static analysis plus race-enabled unit tests
test-unit:
	go vet ./...
	go test -race ./...

## lint: Go static checks plus the frontend lint/format gates
lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...
	pnpm --dir web lint
	pnpm --dir web fmt:check

## bootstrap: reproducible development environment setup
bootstrap:
	@if [ -f scripts/bootstrap-arch.sh ]; then \
		./scripts/bootstrap-arch.sh; \
	else \
		echo "NOT RUN: bootstrap is missing scripts/bootstrap-arch.sh (see docs/spec/16)" >&2; \
		exit 1; \
	fi

## test-full: run every gate in order, continue past failures, then print a
## final table (gate | result | detail). Result PASS comes from exit 0; a
## go gate's recorded exit 2 (artifacts/gates/<gate>.rc) or a legacy gate's
## own "NOT RUN: <gate> ..." output is NOT RUN; anything else is FAIL.
## Exits 1 if any gate FAILed, 2 if nothing failed but at least one gate is
## NOT RUN, and 0 only if every gate passed. Per-gate logs:
## artifacts/gates/full-<gate>.log (gitignored).
test-full:
	@mkdir -p artifacts/gates ; \
	fail=0; notrun=0; table="" ; \
	for target in $(RUN_GATES); do \
		out=artifacts/gates/full-$$target.log ; \
		rm -f artifacts/gates/$$target.rc ; \
		echo "=== GATE $$target ===" ; \
		$(MAKE) $$target >$$out 2>&1 ; \
		rc=$$? ; \
		if [ -f artifacts/gates/$$target.rc ]; then rc=$$(cat artifacts/gates/$$target.rc) ; fi ; \
		if [ $$rc -eq 0 ] ; then res=PASS ; \
		elif [ $$rc -eq 2 ] ; then res="NOT RUN" ; notrun=$$((notrun+1)) ; \
		elif grep -q "^NOT RUN: $$target " $$out 2>/dev/null ; then res="NOT RUN" ; notrun=$$((notrun+1)) ; \
		else res=FAIL ; fail=$$((fail+1)) ; fi ; \
		detail=$$(grep -m1 "^junit:" $$out) ; \
		[ -n "$$detail" ] || detail=$$(grep -m1 "^NOT RUN" $$out) ; \
		[ -n "$$detail" ] || detail=$$(grep -m1 "^FAIL" $$out) ; \
		[ -n "$$detail" ] || detail="exit $$rc" ; \
		detail=$$(printf '%s' "$$detail" | head -c 140) ; \
		table="$$table$$target | $$res | $$detail\n" ; \
	done ; \
	printf '\n=== test-full summary ===\n'; \
	printf 'gate | result | detail\n'; \
	printf '%b' "$$table" ; \
	rc=0 ; \
	if [ $$fail -ne 0 ] ; then rc=1 ; elif [ $$notrun -ne 0 ] ; then rc=2 ; fi ; \
	printf '%s' "$$rc" > artifacts/gates/test-full.rc ; \
	exit $$rc

## bench: the docs/spec/12 benchmark gate — the holdout split of the committed
## synthetic set, replayed through the real pipeline (real Tesseract, real
## matcher), graded against the generated ground truth. Prints the holdout
## report; artifacts land in artifacts/bench/ (gitignored). Holds under the
## five-minute test-full budget (measured ~3 min, 2026-10).
bench:
	@command -v tesseract >/dev/null 2>&1 || { echo "NOT RUN: bench (tesseract not installed; see docs/spec/16)" >&2; exit 1; }
	@command -v magick >/dev/null 2>&1 || { echo "NOT RUN: bench (ImageMagick not installed; see docs/spec/16)" >&2; exit 1; }
	@mkdir -p artifacts
	go run ./cmd/game-narrator bench --set testdata/bench/bench-synthetic-001 --split holdout --out artifacts/bench
	@cat artifacts/bench/report.md

## test-replay: the docs/spec/12 replay gate — the synthetic scenario suite
## (fault injection; BLOCKED scenarios are reported, never passed, ADR-0037)
## plus the pipeline replay tests (real Tesseract, deterministic
## three-identical-runs), race-enabled. BLOCKED and NOT RUN (e.g. tesseract
## missing) results are reported per test and never counted as a pass.
test-replay:
	@scripts/gates/run-go-gate.sh test-replay -race -count=1 ./internal/scenario/... ./internal/pipeline/... ; \
	rc=$$?; echo $$rc > artifacts/gates/test-replay.rc ; exit $$rc

## test-ui: the real UI gate (docs/spec/09, docs/spec/12 "UI") — frontend
## checks, then an embedded-binary build, then Playwright against that binary
## with no Vite dev-server dependency for the embedded project.
test-ui:
	pnpm --dir web typecheck
	pnpm --dir web lint
	pnpm --dir web fmt:check
	pnpm --dir web test
	$(MAKE) build
	pnpm --dir web test:e2e

## test-dolphin: the docs/spec/12 "Dolphin emulator mode" gate — the real
## Dolphin tests (real_test.go, drive_test.go) boot a real Dolphin against
## the user-supplied game image; never labelled as unit coverage. Requires
## GAME_NARRATOR_GAME_IMAGE_PATH plus dolphin-emu, Xvfb, ffmpeg and
## x11-utils on PATH; without them the tests report NOT RUN (exit 2).
## -p 1: one Dolphin at a time; the boot-to-first-dialogue drive takes
## minutes, hence the 20-minute timeout.
test-dolphin:
	@scripts/gates/run-go-gate.sh test-dolphin -count=1 -p 1 -timeout 20m -run Real ./internal/dolphin/... ; \
	rc=$$?; echo $$rc > artifacts/gates/test-dolphin.rc ; exit $$rc

## test-models: the live provider contract gate (docs/spec/12 "provider
## schema — recorded every PR, live conditional") — the -run Live tests in
## the openai, recorded, systemone and tts providers, the voices workflow and
## the runtime speech session, all against the real gateway. Sequential (-p
## 1) so gateway calls never overlap. Requires GAME_NARRATOR_GATEWAY_URL
## plus the token env — the vision tests run by default (no opt-in; the
## GAME_NARRATOR_LIVE_VISION switch was removed with the VLM OCR fallback,
## ADR 0040) and the TTS registry tests need
## GAME_NARRATOR_TTS_BASE_DIRECT_URL. Each unset capability reports NOT RUN
## (exit 2), never a pass.
test-models:
	@scripts/gates/run-go-gate.sh test-models -count=1 -p 1 -timeout 30m -run Live \
		./internal/providers/openai/... ./internal/providers/recorded/... \
		./internal/providers/systemone/... ./internal/providers/tts/... \
		./internal/voices/... ./internal/runtime/... ; \
	rc=$$?; echo $$rc > artifacts/gates/test-models.rc ; exit $$rc

## test-hardware: the UNO Q hardware gate (docs/spec/16, M8) — the
## docs/spec/10 preflight as executable checks (internal/hwsmoke): the
## V4L2 capture grab through the app's FFmpeg path, the consented audio
## sink tone, the controlled-image OCR, the RAM/CPU/temperature/storage
## preflight plus a 60 s serve-session RSS budget, and the platform report.
## Each absent capability is NOT RUN (exit 2) with the exact attach or
## configure instruction — never a vacuous pass — and the budget RSS
## assertion only runs on the UNO Q profile (arm64, < 5 GB RAM); on other
## hosts the numbers are logged and the assertion is NOT RUN-explained.
## Audible playback requires GAME_NARRATOR_HW_AUDIO_CONSENT=1; the capture
## node and audio sink are chosen by GAME_NARRATOR_CAPTURE_DEVICE /
## GAME_NARRATOR_AUDIO_DEVICE when set. See docs/manuals/13-uno-q-hardware-smoke.md.
test-hardware:
	@scripts/gates/run-go-gate.sh test-hardware -count=1 -timeout 15m ./internal/hwsmoke/... ; \
	rc=$$?; echo $$rc > artifacts/gates/test-hardware.rc ; exit $$rc

## release-snapshot: local snapshot pipeline (docs/spec/15, ADR 0033).
## Validates the build, archives, SBOMs, .debs and the OCI image WITHOUT
## publishing: --snapshot skips all publishing and dockers_v2 snapshot builds
## never push (verified against goreleaser 2.14 internals). The GHCR repo is
## not configured by default (GN_DOCKER_REPO unset => docker section disabled);
## the Makefile sets a local-only repo name so the snapshot image is built and
## scanned locally. Tagged publishing needs explicit user approval
## (docs/release.md).
release-snapshot:
	@command -v goreleaser >/dev/null 2>&1 || { echo "NOT RUN: release-snapshot (goreleaser not installed; see docs/spec/16)" >&2; exit 1; }
	@set -e; \
	goreleaser check; \
	if docker info >/dev/null 2>&1; then \
		echo "release-snapshot: docker available in this session"; \
		GN_DOCKER_REPO=game-narrator goreleaser release --snapshot --clean; \
	elif command -v sg >/dev/null 2>&1 && sg docker -c 'docker info' >/dev/null 2>&1; then \
		echo "release-snapshot: session lacks the docker group — running goreleaser via sg docker"; \
		sg docker -c "GN_DOCKER_REPO=game-narrator exec goreleaser release --snapshot --clean"; \
	else \
		echo "release-snapshot: docker not usable — dockers_v2 stays disabled, no image build"; \
		goreleaser release --snapshot --clean; \
	fi; \
	scripts/release/smoke.sh

## diagnostics: probe the host honestly — storage, native tools, OCR, capture,
## audio, every model role, and with --dev the Dolphin/Xvfb and game image
## checks. Exits 1 only when a configured component is actually broken.
diagnostics: build
	$(BIN) doctor --dev

## soak: the docs/spec/12 long soak pair — 1h of replay load, then 2h of
## simulated provider/network chaos (the TestSoakLong budgets; ADR-0045).
## Each run gets an explicit -timeout that outlives the run plus the bounded
## stop phase. Hours-long by design, so it is not part of test-full; the
## race-enabled coverage of the same composition is TestSoakShort in
## test-unit. Reports land under artifacts/soak/<run-id>/.
soak:
	@GAME_NARRATOR_SOAK_DURATION=1h go test -count=1 -timeout 1h15m ./internal/soak/ -run TestSoakLong -v
	@GAME_NARRATOR_SOAK_DURATION=2h GAME_NARRATOR_SOAK_CHAOS=1 go test -count=1 -timeout 2h15m ./internal/soak/ -run TestSoakLong -v

## help: list targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
