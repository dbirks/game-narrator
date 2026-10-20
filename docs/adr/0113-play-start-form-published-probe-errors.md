# ADR-0113: The /play start form's published probe — 404 is a state, every other probe error is a fault

Date: 2026-10-20
Status: accepted
Owners: redesign-b3 (Health/Settings merge integration)
Related requirements/tests: docs/spec/09 (visible-state assertions), AGENTS.md rules 4 and 9, `web/src/pages/Play.tsx` (`fetchRunOptions`), `web/e2e/ksearch.spec.ts` (the ⌘K walk's /play stop), `web/e2e/disc-import-walk.spec.ts` (the /play stop on the shared disc appliance)
Links/references: ADR-0094 (per-run e2e isolation), ADR-0110 §1 (the "machine's answer" convention for honest non-ok responses), ADR-0112 (the same post-merge `--workers=2` suite pass that surfaced this)
Supersedes / superseded by: —

## Context

`fetchRunOptions` builds the /play start form's list by probing every edition's published revision: list the games, list each game's editions, then `GET /api/v1/editions/{id}/published` per edition. The route answers **404** for an edition with no published revision — the documented, normal state (an unpublished shelf entry), and the page's own comment said *"a 404 here means 'nothing published', not a fault"*.

The probe's error handling was a bare `catch { continue; }`: **every** failure of the probe — a 404, a transient 5xx, an aborted in-flight read, a dropped connection — was swallowed and the edition skipped. A single transient probe failure under load therefore turned a shelf with a live published profile into a permanent false *"Nothing is published"* empty state: the query *succeeded* with an empty list, so react-query cached it, no retry ran, and the page offered no recovery. Observed in the post-merge `--workers=2` suite pass (2026-10-20): the ksearch ⌘K walk's /play stop timed out its 20 s `play-start-form` settle, and the failure snapshot showed the false empty state on an appliance whose seeded revision had just been asserted published by the walk's previous test — while the same page's status bar rendered normally.

Two adjacent consequences belong to the same decision:

- The same probe makes the browser log a 404 whenever /play mounts on an appliance that holds any unpublished edition. On the disc project's shared appliance (the disc-import walk and the speaker-link spec share one server by design — their games are disjoint), the sibling walk's unpublished game makes the /play stop's probe 404 in exactly the order the two specs happen to run. That 404 is the machine's honest answer about the sibling game, not a fault of the walk observing it.
- The 404 answer stays correct product behaviour: the start form's job is to list *runnable* profiles, and an unpublished edition has none.

## Options

1. **Retry the whole options list on any probe failure (a page-level retry loop).** Rejected: it re-implements what the query layer already does for genuine errors (react-query retries the query function), hides persistent faults behind a spinner, and keeps conflating the 404 state with failures inside the one hot loop.
2. **Add a per-edition published-state field to the editions list response, dropping the probe.** Rejected: a protocol/persistence change (new response shape, migration of the list route's contract) to fix an error-handling wart; the probe is the route of record for the published revision, and the start form needs that exact resource.
3. **Narrow the catch: skip only on 404, rethrow everything else.** Accepted: it is the smallest change that restores the page's honest behaviour — 404 renders the documented per-edition state, and any other probe failure fails the query so the page's existing `ErrorState` ("Cannot list published profiles", with its retry) renders instead of a false empty state. The query layer's default retries give transient failures their recovery within the page's own budget.

## Decision

`fetchRunOptions` in `web/src/pages/Play.tsx` catches the per-edition probe and distinguishes the two cases by the error's HTTP status (`apiStatus` from `src/api/client`):

```ts
} catch (error) {
  // Only a 404 means "nothing published for this edition" …
  if (apiStatus(error) !== 404) throw error;
  continue;
}
```

- A **404** continues to be skipped: the edition simply has no runnable profile, and the empty list (or the other editions' profiles) is the truth.
- **Any other error** (5xx, abort, network) rethrows out of `fetchRunOptions`: the query enters its error state after the query layer's retries, and the page renders the existing retryable `ErrorState` instead of caching a false empty shelf.
- The **disc-import walk's** console-cleanliness allowlist gains **404** next to its existing 401, with a comment naming the cause: its /play stop probes the shared appliance, whose sibling walk (speaker-link) legitimately holds an unpublished game; the browser logs the probe's 404 as a failed resource. That is the machine's own answer about the sibling game (ADR-0110 §1's "machine's answer" convention, the same family as the 503/409 allowlists in the ksearch, agent-dock and setup-board walks), not a fault of the walk.

No protocol, persistence or provider change: the route, its 404 problem answer, and the page's states are unchanged; only which errors a state is built from is corrected.

## Consequences

- A /play page whose probe transiently fails now shows the retryable error state (and recovers on the query layer's retries) instead of a permanent false "Nothing is published". The false-empty-state failure class the ksearch walk hit is gone for every consumer of the start form.
- Pages that mount /play on an appliance with any unpublished edition will always log a 404 probe; specs asserting console cleanliness on such an appliance must allowlist it (the disc walk does; a future spec that shares a /play-mounting appliance with an unpublished shelf entry needs the same line).
- The 404 probe remains on the wire for every /play mount (one request per edition). If the shelf grows large enough to matter, the editions list should carry the published state — that is the rejected option 2, revisited on evidence, not preemptively.

## Validation

- The ksearch ⌘K walk's /play stop — the reproduction (the post-merge `--workers=2` run: the settle timed out on the false empty state of a published appliance) — is green in the rebuilt full-suite `--workers=2` pass (this branch's Phase 1 gate), as is the disc-import walk with its 404 allowlist.
- `web` typecheck, lint and format gates pass; the change is two lines of error handling plus an import, reviewed against `src/api/client`'s rejection semantics (every non-ok response rejects with an `ApiError` carrying `status`).

## Reversal plan

Restore the bare `catch { continue; }` in `fetchRunOptions` and drop the 404 from the disc walk's allowlist. The page returns to swallowing probe failures into the empty state; no data, protocol or other consumer is affected.
