# ADR-0077: Rights-gated web-source import — a disc-less onboarding path that can publish

Date: 2026-10-19
Status: accepted
Owners: agent worker (g4-research-only)
Related requirements/tests: docs/spec/00 (curator journey "imports owned game data or approved web material"; "Generic game import … agent discovery, text dump, script archive, manual entry"), docs/spec/03 ("Search playbook"; "Human-in-the-loop boundaries" — unknown source rights require human choice; "Tool registry"), docs/spec/06/07 (sources, rights, approvals), docs/spec/14 (trust zone D "hostile data"; "Source material / intellectual-property policy": acquiring protected full text must comply with rights, terms and user policy; "Security acceptance tests"), docs/spec/01 (key invariant 6: import provenance is immutable after registration; corrections are overlay patches), ADR-0018 (web fetch and search safety), ADR-0026 (approval tokens, single-use), ADR-0028 (onboarding API/UI), ADR-0029 (message parser), ADR-0066 (disc import API/UI), AGENTS.md rules 3/5/6. Tests: `internal/agent/tools/websource_test.go` (scenario tests: approved web dump → publishable; malicious dump page → refused/flagged; unapproved source → never imported; content drift → refused), `internal/store/repo_rights_test.go` (rights assertion: draft-only, provenance untouched, published refused), `internal/store/repo_publish_test.go` (gate message names the fixes), `internal/api/onboarding_webimport_test.go` (approve issues the single-use token, state view redacts it, second approval refused), `internal/agent/tools/live_webimport_test.go` (live proposal check, counts only), `web/e2e/onboarding.spec.ts` (Playwright walk of the approval step on the packaged binary, fake model/fetcher, labelled synthetic).

## Context and constraints

Gap audit G4 (docs/status/gap-audit-2026-10-19.md): a user who owns a game but has no disc dump cannot finish onboarding. `parse_dialogue` refuses every source that is not a user file (`internal/agent/tools/sources.go`: "fetched pages are not re-parsed as text dumps"), `import_user_asset` only reads a blob the user already uploaded, and the store's publish gate then refuses the researched-but-empty revision ("the revision contains no messages, so there is nothing to narrate"). The profile is a publish dead-end: characters, voice briefs and auditions exist, but nothing can be narrated or published.

docs/spec/14 already frames the answer: the web agent may *discover* sources, but acquiring protected full text "must comply with rights, terms and user policy". docs/spec/03 puts "unknown source rights" at a human review boundary. So the missing piece is exactly one: a rights-gated import of a specific web text source, where the human's approval of that specific source is what records the user's rights assertion.

Constraints:

- The approval boundary already exists and is single-use (ADR-0026/0028): `POST /api/v1/onboarding/runs/{id}/approve` issues a `gnapp_` token the agent can consume but never issue; the run stops at review when a tool returns `review_required`.
- The fetcher (ADR-0018) hashes the raw body but retains no payload, so the bytes of a fetched page do not exist on the appliance. Staging them *before* approval is exactly what the task forbids ("the user approves … before its bytes are staged").
- Provenance rows are write-once (docs/spec/01 invariant 6; `putSource` answers `ErrImmutable` to a changed repeat). The rights question, however, is a field the schema models as open (`unknown` until the user decides) and the publish rights gate is designed to be fixable — an unfixable refusal "is a lock, not a finding".
- Downloads stay untrusted: policy-gated fetch only (SSRF, byte caps, media caps), no instructions from page text, no new persistence before approval, no copyrighted bytes in the repo or logs (AGENTS.md rule 5).

## Decision

A two-tool flow over the existing approval boundary, plus one store operation and one gate-message fix.

### Tools (`internal/agent/tools/websource.go`)

1. **`propose_web_import`** (pure read, `review_required`): the agent proposes one already-registered `web_page` source (from `fetch_page`/`web_search` provenance) with its full provenance: URL, raw-body SHA-256, retrieval time, rights position (`unknown`). The tool returns `review_required`, so the run lands in state `review` at the existing human boundary — the UI's review item renders the provenance (a new optional `provenance` field on the onboarding review-item view).
2. **`import_web_source`** (draft side-effect, curator): arguments `profile_id`, `source_id`, `approval_token`. The tool:
   - consumes the single-use token through the existing `ApprovalStore` (unknown/spent/expired tokens refuse; the token's profile must match) — the human approval is the gate, and it names the approver for the audit field;
   - re-reads the source row (must be `web_page` with a URL and the recorded SHA-256);
   - re-fetches the URL through the same policy-gated fetcher (new `Fetcher.FetchRaw`, the same request path as `fetch_page` with the decompressed body retained in memory) — so the download happens only after approval, under the same SSRF/size/media limits;
   - verifies the re-fetched raw-body SHA-256 equals the recorded one: the user approved *these* bytes. A page that changed since the proposal is refused (the agent must re-fetch and re-propose), never silently imported;
   - applies the existing safe-media checks (text media types only; executable/archive magic refused) to the decompressed body;
   - runs the existing `profileio.ImportFile` path with `SourceKind: web_page`, the page URL as `SourceURI`, rights = the user's assertion from the approval (`user_supplied`), trust 0.4 (third-party data stays low-trust even after a rights assertion), and a coverage note recording the assertion (approver, time) — the existing parsers and the existing `source_quality` report then run exactly as for a user upload.

The agent prompt gains the flow: fetch → propose (stop for review) → on approval, import with the token → quality report. No web source is ever imported without a consumed token; a token inside page text is content, never an approval (the same rule the malicious-page test already proves for `publish_profile`).

### The rights assertion and the write-once row

`profileio.ImportFile` deduplicates by content hash. For an uncompressed page the decompressed body *is* the raw body, so the import's content hash equals the fetch row's recorded SHA-256 and the import would reuse the fetch row — whose rights are `unknown`. Re-import cannot rewrite provenance, so the assertion would be unrecordable and the publish rights gate (sources the content rests on must be `user_supplied`/`licensed`) would be a dead end.

So the store gains one narrow operation, `SetSourceRights(profileID, sourceID, rights, note)`: it records the user's rights assertion on an existing row of a draft revision (draft-only, row-version bump like every other content write), appending the assertion (approver, time) to `coverage_notes`. This is the one field invariant 6 deliberately leaves open — provenance (URI, bytes hash, retrieval time) is untouched, and the operation is reachable only through the token-gated import tool, which means the assertion always has a consumed human approval behind it. When the hashes differ (compressed page), no update is needed: the import registers a fresh row with the asserted rights and the fetch row remains unreferenced provenance.

### Publish gate message (`internal/store/repo_publish.go`)

The no-messages refusal now names exactly what is missing and the three ways to add dialogue: import the disc (disc import), stage a script file the user has (setup upload / `import_user_asset`), or approve and import a web text source (`propose_web_import`, then `import_web_source` after approval); manual entry of lines is the remaining route.

### UI

The onboarding review item for a `propose_web_import` renders the provenance block (URL, SHA-256 prefix, size/media when available, rights `unknown`) above the existing Approve/Reject buttons; the Approve button's act is the rights assertion. No new page, no new route.

## Alternatives considered

1. **Stage the raw bytes at fetch time; approval releases them** — rejected: the task and docs/spec/14 make the approval precede staging, and keeping hostile payloads on disk before the user has decided is the exact posture the rights gate exists to prevent.
2. **A new source type `user_approved_web`** — rejected: the enum is a wire contract (validated in `games/enums.go` and the OpenAPI contract); the distinction the product needs (asserted rights) is a rights-status fact, not a source-type fact, and the provenance URI already says it is a web page.
3. **Let the agent choose the rights value** — rejected: the rights position is a legal assertion a human makes. The approval *is* the assertion (`user_supplied`); giving the model a legal vocabulary would make the boundary theater.
4. **Update the fetch row in place via `PutSources`** — rejected: `putSource` is write-once by design (invariant 6); the change would need a carve-out inside the very function whose error message is the invariant. A narrow, audited, token-gated operation is the honest boundary.

## Consequences and risks

- A disc-less user reaches a publishable profile: research → fetch → proposal → human approval (the rights assertion) → import → quality → publish. The research-only dead end is gone.
- The re-fetch after approval means two downloads of the page; the hash check turns that cost into a feature (content drift is refused, not absorbed).
- `SetSourceRights` is the first deliberate mutation of a provenance row. It mutates only the rights field plus the coverage note, only on draft revisions, only behind a consumed human token. The audit trail is the coverage note plus the approval row's `granted_by`.
- A malicious dump page is refused or inert: non-text/executable content fails the media checks before any write; a text page's instructions are data (the loop renders untrusted tool output in a delimited block; no tool schema is built from it); a fabricated token in the text is consumed only if a human issued it — `ConsumeApproval` on an unknown hash refuses.
- OpenAPI contract change: `OnboardingReviewItem` gains an optional `provenance` object (additive; `pnpm gen:api` regenerates the TS schema).

## Evidence and test plan

- `go test ./internal/agent/... ./internal/store/... ./internal/api/...` (unit gate):
  - scenario "no disc, approved web dump → publishable profile": a synthetic fixture site (invented text) is fetched with a policy-gated fetcher, proposed, approved (token issued as the human approval step does), imported, and the store's publication validation passes a revision that has messages and publishable rights;
  - "malicious dump page → refused/flagged": an HTML page carrying injection text is imported only as inert data (the text is stored, nothing is obeyed); a page whose body is executable/archive content is refused at the media check;
  - "unapproved source → never imported": `import_web_source` with no token, a spent token, or a token for another profile refuses, and leaves no new asset, source or message;
  - content drift: the fixture site changes between proposal and approval → refused with a re-fetch/re-propose message.
- `SetSourceRights` store tests: draft-only, unknown → asserted, note appended, published target refused, unknown source refused.
- Publish-gate test asserts the refusal names the disc-import, approved-web-source and manual-entry routes.
- API test: approving a `propose_web_import` review item on a run in `review` issues a token (response carries `approval_id`, the token is consumable, the state view redacts it).
- Live onboarding check (`test-models` gate only, NOT RUN elsewhere): the real agent proposes a `propose_web_import` for a real public title's script source; the test reports counts (searches, fetches, proposals) and no copyrighted text, and the profile is discarded (temp appliance, nothing written to the repo).
- Playwright on the packaged binary (fake model + local labelled-synthetic dump page through the dev private allowlist, the test-only deviation ADR-0028 already documents): start a run with no disc, watch the proposal stop the run in review, read the provenance block, approve, watch `import_web_source` and `source_quality` succeed and the draft publish.

## Migration / rollback

No schema change (`SetSourceRights` reuses the existing `sources` columns and the draft row-version discipline). Rollback: drop the two tool registrations and the `provenance` field; the store operation is inert without callers. Existing rows are untouched.

## Follow-ups

- The agent currently cannot *find* dump pages better than a generic search; a source-kind hint in `web_search` (kind: script_dump) would sharpen discovery — follow-up, not this ADR.
- Per-source license metadata (beyond the user assertion) if the rights vocabulary grows.
