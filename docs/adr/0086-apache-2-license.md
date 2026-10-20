# ADR-0086: License the project under Apache-2.0

Date: 2026-10-20
Status: accepted
Owners: project owner (decision); orchestrator (recorded)
Related: README "License", `LICENSE`, `.goreleaser.yaml` (nfpm `license`, OCI `org.opencontainers.image.licenses`, archive and package contents), `web/package.json`, ADR 0033 (release pipeline left the license unspecified)

## Context

The public repository shipped no license: the README said "Not yet chosen", nfpm packages declared `license: unspecified`, and the OCI image label said `UNLICENSED`. Without a license, nobody else can legally use, modify or redistribute the code, and the release artifacts carry no terms at all.

## Options

- **Apache-2.0**: permissive, includes an explicit patent grant and patent-retaliation clause, and is the norm for Go infrastructure and appliance software.
- **MIT**: permissive and shorter, but has no patent grant.
- **GPL-family copyleft**: would force derivative appliances to publish their source. The owner did not ask for that.

## Decision

Apache License 2.0, as chosen by the project owner on 2026-10-20.
- `LICENSE` holds the unmodified text from https://www.apache.org/licenses/LICENSE-2.0.txt (sha256 `cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30`).
- The release archives include `LICENSE`, and the .deb and Arch packages install it at `/usr/share/doc/game-narrator/LICENSE`.
- nfpm `license` and the OCI `org.opencontainers.image.licenses` label say `Apache-2.0`; `web/package.json` declares it too, and stays `private`.

The license covers project code and documentation only. User-supplied game images, scripts, voices and imports are not distributed and keep their own terms (AGENTS.md rule 5).

## Dependency compatibility (checked 2026-10-20)

- **Go modules:** 20 BSD-style, 3 MIT, 1 Apache-2.0, and 1 MPL-2.0 (`github.com/hashicorp/golang-lru/v2`). MPL-2.0 is file-level copyleft and compatible with distributing an Apache-2.0 work that uses the library unmodified; keep its notice.
- **Web production dependencies** (`pnpm licenses list --prod`): MIT and 0BSD only.
- **External processes** (Dolphin GPL-2.0+, Tesseract Apache-2.0, ffmpeg LGPL/GPL builds) are run as separate programs and are not linked or bundled. Their licenses do not extend to this project, and the packages do not ship them.

## Consequences

- Contributions are accepted under Apache-2.0 (section 5, inbound = outbound). No CLA is required.
- No NOTICE file is required today; add one if a bundled dependency's notice requires it.

## Validation

- `goreleaser check`, and a snapshot release that shows `LICENSE` in the archive and package.
- `scripts/gates/secret-scan.sh` is clean.

## Reversal

Code already released under Apache-2.0 stays available under it. Relicensing future versions needs the agreement of all copyright holders.
