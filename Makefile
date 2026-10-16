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

# Gate targets that still need implementation (docs/spec/16).
PENDING_GATES := test-replay test-ui test-dolphin test-models test-hardware

.PHONY: bootstrap dev build test-unit test-replay test-ui test-dolphin test-models \
        test-hardware test-full lint release-snapshot diagnostics

## build: compile the game-narrator binary with version/commit stamped in
build:
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/game-narrator ./cmd/game-narrator

## dev: run the API server from source
dev:
	go run ./cmd/game-narrator serve

## test-unit: static analysis plus race-enabled unit tests
test-unit:
	go vet ./...
	go test -race ./...

## lint: gofmt cleanliness plus go vet
lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...

## bootstrap: reproducible development environment setup
bootstrap:
	@if [ -f scripts/bootstrap-arch.sh ]; then \
		./scripts/bootstrap-arch.sh; \
	else \
		echo "NOT RUN: bootstrap is missing scripts/bootstrap-arch.sh (see docs/spec/16)" >&2; \
		exit 1; \
	fi

## test-full: run every gate that is implemented; report the rest as NOT RUN
test-full: test-unit
	@for target in $(PENDING_GATES); do \
		echo "NOT RUN: $$target is not implemented yet (see docs/spec/16)" >&2; \
	done; \
	echo "NOT RUN: test-full is incomplete because the gates above are unimplemented (see docs/spec/16)" >&2; \
	exit 1

## Release and remaining verification gates — intentionally not implemented.
test-replay:
	@echo "NOT RUN: test-replay is not implemented yet (see docs/spec/16)" >&2; exit 1

test-ui:
	@echo "NOT RUN: test-ui is not implemented yet (see docs/spec/16)" >&2; exit 1

test-dolphin:
	@echo "NOT RUN: test-dolphin is not implemented yet (see docs/spec/16)" >&2; exit 1

test-models:
	@echo "NOT RUN: test-models is not implemented yet (see docs/spec/16)" >&2; exit 1

test-hardware:
	@echo "NOT RUN: test-hardware is not implemented yet (see docs/spec/16)" >&2; exit 1

release-snapshot:
	@echo "NOT RUN: release-snapshot is not implemented yet (see docs/spec/16)" >&2; exit 1

diagnostics:
	@echo "NOT RUN: diagnostics is not implemented yet (see docs/spec/16)" >&2; exit 1

## help: list targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
