# ADR-0006: GoReleaser v2 release pipeline and multiarch OCI
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: FR-15, GN-T077–T080

## Decision
Build linux/arm64 Debian UNO Q binary and linux/amd64 Arch dev binary, `.deb` (and tested Arch package), archives and checksums, multiarch OCI image with current `dockers_v2`, signed/SBOM-backed releases when configured. Do not include copyrighted game bytes or secrets.

## Verification
`goreleaser check`, snapshot, clean package install and OCI manifest tests on each target. GoReleaser syntax must be verified against pinned actual binary rather than copied blindly. Publish only after explicit user repository/registry setup and quality gates.
