# 14 — Threat model, privacy, source rights and voice safety

## Trust zones

A) browser and LAN user (authenticated by role); B) Go core/runtime (trusted application); C) game bytes/frames and imported scripts (untrusted data); D) external web pages/search results (hostile data); E) user-configured inference gateway and remote model instances (authenticated but potentially changing); F) Dolphin/dev-only input controller (high local privileges); G) local asset store/SQLite (restricted filesystem). No agent-provided instructions cross from C/D into privileged B/F.

## Mandatory controls

- **Prompt injection:** HTML, text dumps, character text, source code comments and web snippets enter tools as quoted data. No privileged instructions or dynamic tool schemas are built from retrieved data. Content can suggest analysis targets, but not change permissions. Tool executor verifies allowlist, args schema, role and cost before action.
- **SSRF:** URL fetcher rejects loopback, RFC1918, link-local, metadata endpoints and private DNS rebindings by default; follow redirects with revalidation; supports explicit allowlisted private dev endpoints outside agent browse tools. Timeouts, response size and MIME limits.
- **Filesystem/import:** relative-path traversal blocking, no symlink-follow for untrusted ZIP, max decompressed size/ratio/file count, memory/time limits, validate image decoders, hash and quarantine unknown executables; no automatic execution of sourced binaries.
- **Authorization:** local-first, set admin on first run, signed sessions, CSRF, role ACL, rate limits, secure secrets store, HTTPS for LAN. Developer-mode routes and emulator controller endpoints privileged; no unauthenticated screenshot/audio.
- **Inference gateway:** API tokens server-side only, audit endpoint configuration changes, domain allowlist or user approval for media upload outside LAN, voice registry secured at proxy because upstream can be global and mutable.
- **Privacy:** user sets frame/audio log retention; developer diagnostics can reveal game content, voice samples and filenames. Export support bundle only after redaction preview and explicit user action.
- **Supply chain:** pin dependency versions, vuln scan/SBOM/container base digest, reproducible builds where feasible, signed and checksummed artifacts, CI least privilege, segregate publishing tokens.

## Source material / intellectual-property policy

No packaged games or copyrighted dialogue/art in release. Users can upload own game image or text and keep it local. Web agent can discover sources and summarize availability, but acquiring/importing any protected full text must comply with rights, terms and user policy; never circumvent DRM/access controls. Default export shares structured profiles without proprietary media/text; private user bundle export optional with explicit acknowledgement. Title/character facts, generic metadata and original synthesized voices are separated from copyrighted assets. Include `source_uri`, license/rights state and importer notes per asset.

## Voice consent

Default to original, newly designed synthetic character voices. If a user imports audio of a real person or recognizable performer, require consent/rights confirmation, ownership/authorized-use declaration, limited access and deletion path. Avoid generating false endorsements or passing generated dialogue as genuine voice recordings. TTS endpoints' required `consent` metadata must be passed meaningfully; don't fabricate third-party approval. Deletion should remove local reference and schedule removal of registry copies where authorized.

## Abuse and resource controls

Bound tool budget/search calls, remote inference/concurrency, upload sizes, TTS text length, generated audio quota and retention. Agent cannot update admin/gateway/secret config through natural language without role-protected explicit action. Prevent execution of HTML/script from user-imported source previews, CSP and sanitizer. Treat OCR text as hostile string for React escaping and log encoding.

## Security acceptance tests

Source page attempts prompt override → blocked; web URL redirects to 127.0.0.1 → blocked; ZIP symlink/zip-slip and decompression bomb → quarantined; unauthorized viewer tries dev screenshot, voice registry and profile publish → denied; stolen old session cookie invalidated; no raw API key in frontend bundle, JS error or reports; unapproved voice upload refused; cross-profile voice name collision impossible; model provider redirect/wrong TLS certificate rejected; profile export honors content rights flags.
