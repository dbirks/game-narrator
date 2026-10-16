# ADR-0001: Go service with embedded React/Vite UI
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: FR-01–FR-15, GN-T076

## Context and constraints
UNO Q Debian arm64 app is primarily IO/orchestration; developer prefers Go for delivery speed, React/Vite familiarity and a single deployed executable. Rust is a viable alternative, but runtime CPU hotspots are OCR/image processing and hosted inference rather than Go request handling.

## Decision
Use Go for backend and React+TypeScript+Vite embedded via `go:embed` after build. Native video/OCR can be supervised FFmpeg/Tesseract subprocesses. No frontend Node runtime in production.

## Alternatives
Rust/Axum with embedded React; Go + templ/htmx. Chosen approach trades slightly larger frontend build for richer developer tool UI and familiar iteration.

## Consequences
Go architecture with typed providers and clean CPU limits. Vite assets must be built before Go; CI tests static deep links. Native tools remain separate OS dependencies.

## Verification and rollback
`make test-ui`, binary startup on amd64 and arm64, embedded SPA deep link check. Revisit language only with performance profile and new ADR.
