# ADR-0002: Physical video bypass and pluggable frame sources
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: FR-01, GN-T037–T040, GN-T072

## Context
The game must remain playable without AI inference latency. UNO Q has no dedicated HDMI video input; HDMI can be captured through a UVC device.

## Decision
Use powered HDMI splitter to TV and independent UVC capture path on UNO Q. `FrameSource` abstracts V4L2, Dolphin, and recorded frames. Emulator input automation is dev-only. Direct display branch must never depend on Go service.

## Alternatives
Pass-through HDMI capture, video processing overlay, analog direct capture. Alternatives have more latency/compatibility risk.

## Validation
Measure connected splitter/capture card formats and frames on board; verify game remains visible when UNO Q powered down. `make test-hardware` when available; zero model-dependent display latency expected by architecture.
