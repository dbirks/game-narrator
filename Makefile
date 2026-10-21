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
             test-replay test-dolphin test-models test-hardware secret-scan

.PHONY: bootstrap dev build web-build test-unit test-replay test-ui test-dolphin test-models \
        test-hardware test-full lint release-snapshot verify-apt-pins diagnostics bench soak secret-scan

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

## test-unit: static analysis plus race-enabled unit tests. GN_GATE=test-unit
## marks the run so every live/real test family (the real Dolphin drives and
## recorders, the internal/e2e Live tests, the -run Live provider/voices/
## runtime tests) skips itself: the unit gate must never depend on host env
## (the image path, the gateway URL) and must never boot the emulator or call
## the gateway — the owning gates (make test-dolphin / make test-models) set
## their own GN_GATE value and are the only runs those tests execute under
## (AGENTS.md rule 3: label classes; ADR-0065). hwsmoke is already excluded:
## it self-skips without GAME_NARRATOR_HW_SMOKE, which only the test-hardware
## recipe sets. -skip keeps TestDolphinScenarios out of the unit gate the way
## the test-replay recipe does: it belongs to the test-dolphin gate (the
## recorded Dolphin fixture replay, 30-minute budget) and its 10+ minute run
## would time out the unit gate's default test timeout.
##
## -timeout 25m makes the per-package deadline explicit instead of relying on
## Go's 10-minute default (issue #15): internal/api's race-enabled suite runs
## in the several-minute band, and under parallel gate load one run already
## hit the implicit 10-minute panic inside a migration. 25m is a deliberate
## ceiling — a package that needs it is a regression to fix, not to absorb.
test-unit:
	go vet ./...
	GN_GATE=test-unit go test -race -timeout 25m -skip TestDolphinScenarios ./...

## lint: Go static checks, the frontend lint/format gates and the repo-wide
## secret scan in tree mode (the history-including run is the dedicated
## `secret-scan` gate target below and the CI step).
lint:
	@unformatted="$$(git ls-files -z --cached --others --exclude-standard -- '*.go' | xargs -0 -r gofmt -l)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...
	pnpm --dir web check:api
	pnpm --dir web lint
	pnpm --dir web fmt:check
	scripts/gates/adr-numbers.sh
	scripts/gates/secret-scan.sh

## secret-scan: the "no secrets / private endpoints / game data in Git" gate
## (AGENTS.md rule 5) — scripts/gates/secret-scan.sh scans every tracked
## file and, with --history, the added lines of every commit, for API
## keys/tokens, private endpoint URLs, /home/<user>/ paths, game-data
## magic/extension and unallowlisted large binaries. Matched values are
## never printed (masked; length only); documented false positives are
## suppressed via scripts/gates/secret-scan.allow and listed with their
## reason. Like the go gates it records its exit code in
## artifacts/gates/secret-scan.rc: 0 clean, 1 findings, 2 NOT RUN.
## --history scans the added lines of every commit in --all; on a shallow
## clone it covers the available depth only (CI runs it with fetch-depth 0).
secret-scan:
	@mkdir -p artifacts/gates ; \
	scripts/gates/secret-scan.sh --history ; \
	rc=$$?; echo $$rc > artifacts/gates/secret-scan.rc ; exit $$rc

## bootstrap: reproducible development environment setup
bootstrap:
	@if [ -f scripts/bootstrap-arch.sh ]; then \
		./scripts/bootstrap-arch.sh; \
	else \
		echo "NOT RUN: bootstrap is missing scripts/bootstrap-arch.sh (see docs/spec/16)" >&2; \
		exit 1; \
	fi

## test-full: run every gate in order, continue past failures, then print a
## final table (gate | result | detail) and write the machine-readable
## evidence manifest (scripts/gates/test-full-manifest.sh: the per-gate rows
## plus the bench second set's report, ADR-0079, for the second game's
## holdout leg — the AGENTS.md evidence bundle's machine half). Result PASS
## comes from exit 0; a go gate's recorded exit 2 (artifacts/gates/<gate>.rc)
## or a legacy gate's own "NOT RUN: <gate> ..." output is NOT RUN; anything
## else is FAIL.
## Exits 1 if any gate FAILed, 2 if nothing failed but at least one gate is
## NOT RUN, and 0 only if every gate passed. Per-gate logs:
## artifacts/gates/full-<gate>.log (gitignored); the manifest:
## artifacts/gates/test-full-manifest.json (gitignored, like the rest of
## artifacts/).
test-full:
	@mkdir -p artifacts/gates ; \
	tsv=artifacts/gates/test-full-gates.tsv ; \
	rm -f "$$tsv" ; \
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
		printf '%s\t%s\t%s\n' "$$target" "$$res" "$$detail" >>"$$tsv" ; \
	done ; \
	printf '\n=== test-full summary ===\n'; \
	printf 'gate | result | detail\n'; \
	printf '%b' "$$table" ; \
	if bash scripts/gates/test-full-manifest.sh artifacts/gates/test-full-manifest.json ; then \
		printf 'evidence manifest: artifacts/gates/test-full-manifest.json\n' ; \
	else \
		printf 'test-full: WARNING: the evidence manifest was not written (the gate results above stand)\n' >&2 ; \
	fi ; \
	rc=0 ; \
	if [ $$fail -ne 0 ] ; then rc=1 ; elif [ $$notrun -ne 0 ] ; then rc=2 ; fi ; \
	printf '%s' "$$rc" > artifacts/gates/test-full.rc ; \
	exit $$rc

## bench: the docs/spec/12 benchmark gate — the holdout split of the committed
## synthetic set, replayed through the real pipeline (real Tesseract, real
## matcher), graded against the generated ground truth. Prints the holdout
## report; artifacts land in artifacts/bench/ (gitignored). The synthetic
## holdout holds under the five-minute test-full budget (measured ~3 min,
## 2026-10); the captured part (when the fixtures are configured) runs the
## dense fixture's ~1085 frames through the same replay and adds several
## minutes (ADR-0064). Tesseract runs single-threaded (OMP_NUM_THREADS=1):
## the replay's per-frame OCR must be deterministic and the bench's bounded
## workers must not oversubscribe the host with the engine's OpenMP pool.
## The captured-Dolphin holdouts (class (b) captured-frame replay, M7) run
## the external fixtures' frames (windwaker-newgame and, once recorded,
## windwaker-legend-dense — the dense ≥10 Hz legend recording, ADR-0058
## amendment 1) against the script imported at run time from
## GAME_NARRATOR_GAME_IMAGE_PATH, each reported separately; then the
## calibration fit runs over the synthetic tuning split plus the fixtures'
## real-frame observations AND their deterministic degraded-render variants
## (class b′, generated at bench time, never stored, never reported as
## clean captures). A NOT FITTED refusal is a verdict (ADR-0035), never a
## gate failure. The captured part is NOT RUN with setup steps when the
## fixture dirs or the image are absent (never a silent pass, never a
## failure of the synthetic part).
bench:
	@command -v tesseract >/dev/null 2>&1 || { echo "NOT RUN: bench (tesseract not installed; see docs/spec/16)" >&2; exit 1; }
	@command -v magick >/dev/null 2>&1 || { echo "NOT RUN: bench (ImageMagick not installed; see docs/spec/16)" >&2; exit 1; }
	@mkdir -p artifacts
	# One build, then direct binary runs: `go run` swallows the child's exit
	# code (it always exits 1 on a non-zero child, printing "exit status N"),
	# so the NOT FITTED verdict (exit 4, a refusal the gate must accept,
	# ADR-0035) would read as a gate failure through go run. The binary lands
	# under the gitignored artifacts/ tree (not game-derived content, ADR-0058
	# amendment 1).
	go build -o $(CURDIR)/artifacts/bench-bin/game-narrator ./cmd/game-narrator
	OMP_NUM_THREADS=1 $(CURDIR)/artifacts/bench-bin/game-narrator bench --set testdata/bench/bench-synthetic-001 --split holdout --out artifacts/bench
	# The second game's holdout (ADR-0079, gap audit G12) runs in the same
	# gate: if either set's holdout misses the bar the recipe fails (the bench
	# command exits non-zero on a NOT FIT verdict).
	OMP_NUM_THREADS=1 $(CURDIR)/artifacts/bench-bin/game-narrator bench --set testdata/bench/bench-synthetic-002 --split holdout --out artifacts/bench-second
	@cat artifacts/bench/report.md
	@echo "--- second game (bench-synthetic-002) holdout ---"
	@cat artifacts/bench-second/report.md
	@if [ -n "$$GAME_NARRATOR_FIXTURE_DIR" ] && [ -n "$$GAME_NARRATOR_GAME_IMAGE_PATH" ]; then \
		fixtures=""; \
		for fx in windwaker-newgame windwaker-legend-dense; do \
			[ -d "$$GAME_NARRATOR_FIXTURE_DIR/$$fx" ] && fixtures="$$fixtures $$fx"; \
		done; \
		if [ -n "$$fixtures" ]; then \
			rc=0; \
			for fx in $$fixtures; do \
				echo; \
				OMP_NUM_THREADS=1 $(CURDIR)/artifacts/bench-bin/game-narrator bench captured --fixture "$$GAME_NARRATOR_FIXTURE_DIR/$$fx" --image "$$GAME_NARRATOR_GAME_IMAGE_PATH" --out "artifacts/bench-captured/$$fx" || rc=$$?; \
				cat "artifacts/bench-captured/$$fx/report.md"; \
			done; \
			echo; \
			cfix=""; for fx in $$fixtures; do cfix="$$cfix --fixture $$GAME_NARRATOR_FIXTURE_DIR/$$fx"; done; \
			OMP_NUM_THREADS=1 $(CURDIR)/artifacts/bench-bin/game-narrator bench calibrate --set testdata/bench/bench-synthetic-001 $$cfix --image "$$GAME_NARRATOR_GAME_IMAGE_PATH" --out artifacts/bench-calibrate; \
			calrc=$$?; \
			if [ $$calrc -eq 4 ]; then echo "(calibration: NOT FITTED — the refusal is a verdict, ADR-0035; no calibration.json written)"; fi; \
			if [ $$rc -ne 0 ] || { [ $$calrc -ne 0 ] && [ $$calrc -ne 4 ]; }; then exit 1; fi; \
		else \
			echo ""; \
			echo "NOT RUN: bench captured holdout (class b): set GAME_NARRATOR_FIXTURE_DIR (fixture root containing windwaker-newgame and/or windwaker-legend-dense, recorded via the dolphin fixture factory, ADR-0054) and GAME_NARRATOR_GAME_IMAGE_PATH (the user's Wind Waker image) — see docs/spec/16 M7" >&2; \
		fi; \
	else \
		echo ""; \
		echo "NOT RUN: bench captured holdout (class b): set GAME_NARRATOR_FIXTURE_DIR (fixture root containing windwaker-newgame and/or windwaker-legend-dense, recorded via the dolphin fixture factory, ADR-0054) and GAME_NARRATOR_GAME_IMAGE_PATH (the user's Wind Waker image) — see docs/spec/16 M7" >&2; \
	fi

## test-replay: the docs/spec/12 replay gate — the synthetic scenario suite
## (fault injection; BLOCKED scenarios are reported, never passed, ADR-0037)
## plus the pipeline replay tests (real Tesseract, deterministic
## three-identical-runs), race-enabled. BLOCKED and NOT RUN (e.g. tesseract
## missing) results are reported per test and never counted as a pass.
## TestDolphinScenarios lives in internal/scenario but belongs to the
## test-dolphin gate (it needs the recorded Dolphin fixture under
## $GAME_NARRATOR_FIXTURE_DIR and is NOT RUN without it); -skip keeps it out
## of this gate so a fixture-less host (CI) sees a clean 0 instead of a
## NOT RUN exit 2.
test-replay:
	@scripts/gates/run-go-gate.sh test-replay -race -count=1 -skip TestDolphinScenarios ./internal/scenario/... ./internal/pipeline/... ; \
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
## Dolphin tests (real_test.go, drive_test.go, record_real_test.go) boot a
## real Dolphin against the user-supplied game image; the recorder test
## (TestRealRecordFixture) drives the new-game flow through the fixture
## recorder and lands the replayable fixture under $GAME_NARRATOR_FIXTURE_DIR
## (default artifacts/fixtures/, gitignored — AGENTS.md rule 5). The second
## phase replays the committed dolphin scenarios (testdata/scenarios/dolphin)
## through the real recognition/runtime path and writes the HTML report to
## artifacts/scenarios/<run-id>/index.html. Never labelled as unit coverage.
## Requires GAME_NARRATOR_GAME_IMAGE_PATH plus dolphin-emu, Xvfb, ffmpeg,
## x11-utils and tesseract on PATH; without them the tests report NOT RUN
## (exit 2). -p 1: one Dolphin at a time; the boot-to-first-dialogue drive
## and its re-record take minutes, hence the 45-minute timeout.
## GN_GATE=test-dolphin is the explicit gate variable the TestReal* tests
## require (ADR-0065): with it absent they skip, so no other go-test run
## (make test-unit included) can boot a second emulator. -skip keeps the
## re-OCR recipe-revision step (TestRealReOCRFixture, ADR-0054) out of the
## gate: it is NOT RUN unless GN_REOCR_FIXTURE names the fixture id, which
## would make this gate rc 2 forever — run it on demand instead:
##   GN_REOCR_FIXTURE=<fixture id> go test -count=1 -timeout 30m -run 'TestRealReOCRFixture$' ./internal/dolphin/
## The third phase (task browser-live-play) is the live play path through
## the REAL UI (AGENTS.md rule 3 class (d)+(e)): internal/e2e TestLivePlayUI
## serves the in-process API + embedded SPA on a real port (18120–18139 band)
## with the dev Dolphin bridge wired (the production binary never does),
## imports the script from the disc image, seeds the region, designs the
## narrator voice, launches the bridge and runs the Playwright live-play
## project (web/e2e/live-play.spec.ts, via e2e/live-play.playwright.config.ts)
## over the real pages: /play start with the Dolphin source, the committed
## line + speech counters, the /developer?session=… inspector's trace row
## chain, the Play page's Stop — then asserts the docs/spec/12 live-E2E set
## (frames, script match, Clef request, TTS bytes, correlation) and writes
## the evidence bundle (Playwright trace, screenshots, WAVs, timeline,
## manifest) to artifacts/live-play-ui/<run-id>/ (gitignored). It is NOT RUN
## (rc 2, with setup lines) without the image, the dev host binaries,
## pnpm + a Playwright chromium, the built web UI and the live gateway/TTS
## env — so a host without any of them never turns this gate red. It needs
## GN_GATE=test-dolphin like the first phase (the test enforces it) and runs
## AFTER the real-drive phase: one Dolphin at a time, and this phase boots
## the only emulator of its phase. The phase now runs a second live e2e in
## the same package (issue #25, ADR-0122): TestLiveCatchUpRapidDialogue —
## the catch-up narration E2E: the same first-dialogue savestate, the
## session's catch_up policy, the rapid-A drive (faster than the speech) and
## the no-backlog/merge/voice assertions against the correlated stream, the
## bundle under artifacts/catch-up-live/<run-id>/. It boots its own Dolphin
## sequentially in the phase (one emulator at a time: it runs before
## TestLivePlayUI, in file order) and NOT RUNs on the same missing env. The
## phase budget is 115 m: 45 m for the catch-up test's internal budget (the
## voice design, the drive and the settle) plus TestLivePlayUI's 60 m (the
## voice auditions and the screen-driven walk to the opening legend take
## minutes each).
## The liveui phase needs the packaged SPA go:embedded into the test
## binary (webassets.Built()); the recipe builds it between the scenario
## and liveui phases (make web-build) so the phase RUNS on a host with pnpm
## instead of reporting NOT RUN. Without pnpm the build is skipped and the
## phase stays NOT RUN (a host without it never turns this gate red); a
## failed build removes the staged dist so the phase reports NOT RUN
## instead of walking a half-built UI. NOTE: no comment lines inside the
## recipe body — a `#` line mid-recipe splits the recipe into separate shell
## invocations (measured, GNU Make 4.4.1), so the rc1/rc2/rc3 assignments
## would not reach the final rc aggregation line (2026-10-20, dolphin-gate).
test-dolphin:
	@GN_GATE=test-dolphin scripts/gates/run-go-gate.sh test-dolphin -count=1 -p 1 -timeout 45m -run Real -skip 'TestRealReOCRFixture$$' ./internal/dolphin/... ; \
	rc1=$$?; \
	GN_GATE=test-dolphin scripts/gates/run-go-gate.sh test-dolphin-scenarios -count=1 -timeout 30m -run TestDolphinScenarios ./internal/scenario/... ; \
	rc2=$$?; \
	if command -v pnpm >/dev/null 2>&1; then \
		$(MAKE) --no-print-directory web-build || { echo "web-build failed: the live play UI phase will report NOT RUN (the staged SPA was removed)" >&2; rm -rf internal/webassets/dist; } ; \
	else \
		echo "(web-build skipped: pnpm not on PATH - the live play UI phase reports NOT RUN)" >&2; \
	fi ; \
	GN_GATE=test-dolphin scripts/gates/run-go-gate.sh test-dolphin-liveui -count=1 -p 1 -timeout 115m -run 'TestLivePlayUI$$|TestLiveCatchUpRapidDialogue$$' ./internal/e2e/... ; \
	rc3=$$?; \
	if [ $$rc1 -eq 1 ] || [ $$rc2 -eq 1 ] || [ $$rc3 -eq 1 ]; then rc=1; elif [ $$rc1 -eq 2 ] || [ $$rc2 -eq 2 ] || [ $$rc3 -eq 2 ]; then rc=2; else rc=0; fi; \
	echo $$rc > artifacts/gates/test-dolphin.rc ; exit $$rc

## test-models: the live provider contract gate (docs/spec/12 "provider
## schema — recorded every PR, live conditional") — the -run Live tests in
## the openai, recorded, systemone, tts and asr providers, the voices workflow
## and the runtime speech session, all against the real gateway. Sequential
## (-p 1) so gateway calls never overlap. Requires GAME_NARRATOR_GATEWAY_URL
## plus the token env — the vision tests run by default (no opt-in; the
## GAME_NARRATOR_LIVE_VISION switch was removed with the VLM OCR fallback,
## ADR 0040) and the TTS registry tests need
## GAME_NARRATOR_TTS_BASE_DIRECT_URL. Each unset capability reports NOT RUN
## (exit 2), never a pass. The agent live smoke (two-tool registry,
## synthetic-adventure onboarding) and the VLM OCR-fallback live test join
## this gate (ADR-0065): they are live gateway tests and no longer ride the
## test-unit ./... glob. The 30-minute full-chain TestLiveOnboardingAPI
## (internal/api) stays MANUAL (GN_GATE=test-models + the package is not in
## this recipe) — it would eat the gate's whole budget by itself.
## GN_GATE=test-models is the gate variable the live provider/voices/runtime
## tests require (ADR-0065): without it they skip, so make test-unit never
## calls the real gateway.
test-models:
	@GN_GATE=test-models scripts/gates/run-go-gate.sh test-models -count=1 -p 1 -timeout 30m -run Live \
		./internal/providers/openai/... ./internal/providers/recorded/... \
		./internal/providers/systemone/... ./internal/providers/tts/... \
		./internal/providers/asr/... ./internal/voices/... ./internal/runtime/... \
		./internal/agent/... ./internal/vision/... ; \
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
## GAME_NARRATOR_HW_SMOKE=1 is the gate that lets the TestHW* tests run at
## all: `go test ./...` (and CI's go test -race ./...) must NOT drive the
## hardware smoke, so this target — and scripts/hw/run-on-device.sh — are
## the only two places that set it.
test-hardware:
	@GAME_NARRATOR_HW_SMOKE=1 scripts/gates/run-go-gate.sh test-hardware -count=1 -timeout 15m ./internal/hwsmoke/... ; \
	rc=$$?; echo $$rc > artifacts/gates/test-hardware.rc ; exit $$rc

## verify-apt-pins: drift guard for the OCI image's apt version pins (ADR 0131).
## packaging/Dockerfile pins exact package versions against immutable
## snapshot.debian.org timestamps; this verifies the pins resolve (fast index
## check, no docker needed) and, when docker is usable, builds the
## `apt-pins` stage for linux/amd64 (+ linux/arm64 under binfmt emulation).
## Wired into release-snapshot and publish.yml so a Debian version update
## fails early with an actionable message instead of at image build time.
verify-apt-pins:
	@scripts/release/verify-apt-pins.sh

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
	scripts/release/verify-apt-pins.sh; \
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
