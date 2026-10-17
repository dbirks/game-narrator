# Game Narrator — development and release targets (AGENTS.md "CLI targets")
#
# Unimplemented gates print "NOT RUN: ..." to stderr and their recipe exits 1,
# so `make` fails. They are never allowed to print success; see AGENTS.md "CLI
# targets expected in actual implementation". Note that GNU make maps any
# recipe failure to its own exit status 2 (documented behaviour, not
# configurable), so `make test-replay` returns 2 while the recipe returns 1.

MODULE  := github.com/dbirks/game-narrator
BIN     := bin/game-narrator
PKG     := $(MODULE)/internal/bootstrap
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X $(PKG).version=$(VERSION) -X $(PKG).commit=$(COMMIT)

# Gate targets that test-full runs for real.
RUN_GATES := test-unit test-ui diagnostics release-snapshot
# Gate targets that still need implementation (docs/spec/16).
PENDING_GATES := test-replay test-dolphin test-models test-hardware

.PHONY: bootstrap dev build web-build test-unit test-replay test-ui test-dolphin test-models \
        test-hardware test-full lint release-snapshot diagnostics

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

## test-full: run every gate that is implemented; report the rest as NOT RUN
test-full: $(RUN_GATES)
	@for target in $(RUN_GATES); do \
		echo "RUN: $$target passed"; \
	done; \
	for target in $(PENDING_GATES); do \
		echo "NOT RUN: $$target is not implemented yet (see docs/spec/16)" >&2; \
	done; \
	echo "NOT RUN: test-full is incomplete because the gates above are unimplemented (see docs/spec/16)" >&2; \
	exit 1

## Release and remaining verification gates — intentionally not implemented.
test-replay:
	@echo "NOT RUN: test-replay is not implemented yet (see docs/spec/16)" >&2; exit 1

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

test-dolphin:
	@echo "NOT RUN: test-dolphin is not implemented yet (see docs/spec/16)" >&2; exit 1

test-models:
	@echo "NOT RUN: test-models is not implemented yet (see docs/spec/16)" >&2; exit 1

test-hardware:
	@echo "NOT RUN: test-hardware is not implemented yet (see docs/spec/16)" >&2; exit 1

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

## help: list targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
