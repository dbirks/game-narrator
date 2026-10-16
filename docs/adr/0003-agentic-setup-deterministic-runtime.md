# ADR-0003: Autonomous onboarding, deterministic runtime policies
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: FR-03–FR-10, GN-T053–T070

## Context
Different games need source discovery, script parsing and speaker/voice inference. Gameplay needs bounded, auditable response times and predictable playback.

## Decision
Single allowlisted tool-using game setup orchestrator manages durable deterministic importer/index/voice jobs; profile publication approved. Runtime relies on OCR/candidate matching/dialogue graph and policy with Clef/VLM as targeted classifiers; never prompts a general LLM every frame.

## Consequences
Excellent inspectability and costs but more domain/review plumbing. Separate model adapters and systematic calibration required. User-content prompt injection threat is handled at tool boundary.

## Verification
Agent injection/resume tests; frame replay, Clef schema tests and live audio cancellation. Revisit only through benchmarked alternatives and ADR.
