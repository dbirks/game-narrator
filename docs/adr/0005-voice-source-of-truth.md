# ADR-0005: Application-owned voice references, inference-side registry copies
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: FR-08–FR-11, GN-T015–T020, GN-T045–T047

## Context
Voice IDs reduce request bandwidth; vLLM-Omni persists uploaded voices but multi-machine gateway may route between independent registries.

## Decision
Canonical reference/consent/transcript and version live in application asset store. Register approved immutable voices to Base server(s); reconcile per instance, optionally use inline references/precomputed voice packs. Separate VoiceDesign and Base checkpoints; seed is metadata, not speaker identity.

## Verification
Restart and rotate inference backends; assert voice ID is valid on chosen route, speech recognizable and no wrong builtin fallback. See inference/voice spec parts.
