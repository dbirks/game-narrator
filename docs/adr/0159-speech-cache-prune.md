# ADR 0159: Prune the speech cache's unreachable entries

- Status: accepted
- Date: 2026-10-22
- Issue: #49

## Context and constraints

The speech cache (ADR-0023, ADR-0039) is the shared TTS cache a live session's
commits, the successor prewarm (ADR-0095) and the profile pre-synthesis
(ADR-0153) all draw from. Before ADR-0159 the only eviction was `EvictTransient`
— a disk-full quota mechanism, not an "unreachable" notion.

The key changes of ADR-0153/0155/0156 after the cache was last sized changed the
cache keys the live path can produce:

- ADR-0153 replaced the pre-synthesis's whole-message keys with the live
  commit's key identity, so pre-synthesized lines now live under the same key a
  commit computes;
- ADR-0155 moved the utterance identity to the page-scoped `<message id>#p<n>`
  key for multi-box messages, so an earlier pre-synthesis pass (and a
  whole-message-era entry) can no longer be hit;
- ADR-0156 moved the voice per line from the raw slot to the adjudicator's
  confident per-occurrence speaker, else the narrator fallback, so slot-linked
  keys (ADR-0101's slot→voice mapping) can no longer be hit either.

The result: entries the cache still holds whose keys no current published
profile revision can produce are dead weight that no play, prewarm or pre-synthesis
will ever read, and each new full pre-synthesis pass adds roughly a gigabyte of
it (the ~3.1 GB of speech assets with ~half dead after the key change, on a
partition with ~2.7 GB free).

Nothing in the product exposed a way to reclaim that space: the only eviction
path was the disk-full quota, which answers "how many entries may I keep" and
never "which entries can never be reached".

## Decision

1. **One reachable-key set.** The runtime computes, for every edition's currently
   published profile revision, the cache keys the live path can produce — one
   per display page under the page's resolved voice (ADR-0155), the approved
   voice of the speaker's character else the narrator voice when the profile's
   narrator fallback is on (ADR-0101/ADR-0156) — through the same key computation
   the commit, prewarm and pre-synthesis already share (ADR-0153's one-key
   identity), refactored into shared package-level helpers so the prune cannot
   drift from the live path. A line whose speaker resolves to no voice produces no
   key (the live path skips it).
2. **`Cache.PruneUnreachable(ctx, keep, dryRun)`.** The cache grows a prune entry
   point next to `EvictTransient`: it snapshots the victims (LRU order), applies
   a keep predicate (the reachable set) and deletes through the existing
   reference-checked deletion (ADR-0041) — a blob still named by the catalogue
   (approved voice references, audition clips, character portraits, imported
   sources) keeps its bytes while its entry is dropped, and a cache without a
   checker evicts index entries only, safely. `dryRun` reports the would-be
   entries and bytes without touching anything.
3. **The job.** `speech.cache_prune` (`internal/api/speech_cache_prune.go`): the
   prune runs as a background job like the pre-synthesis (the live-session queue
   pause applies — the job runs between play sessions), queued by
   `POST /api/v1/speech/cache/prune` with body `{"dry_run":bool}`. The route
   answers 202 with the job, the handler maps the runtime's would-be answer onto
   the queue's retry rules (retryable on store faults, permanent on bad input).
   Result carries `entries` and `bytes`.
4. **The operator command.** `game-narrator cache prune [--dry-run] --data-dir DIR`
   runs the same `runtime.PruneSpeechCache` against a data directory — the same
   code the API job drives — so the operator command and the job answer from one
   path.
5. **The doctor row.** `speech.cache_disk` warns when the free space on the data
   partition drops under the 2 GB reserve the prune needs, or when the cache's
   unreachable share would exceed the 30% warn threshold — the share measured
   through the prune's own dry-run walk.

## Options considered

- **Delete unreachable entries eagerly at publish time.** Rejected: it couples
   publishing to a long gateway-dependent pass and would spend TTS on profiles
   never played; the operator-started prune is the same answer on demand, minus
   the coupling.
- **A full re-synthesis at prune time.** Rejected: the prune is a key-set
   computation plus eviction, not a synthesis; a full pass re-adds the very
   gigabytes it aims to remove.
- **Keep unreachable entries for forensics.** Rejected: the entries are content-
   addressed and the prune is the documented reclaim path; the catalogue rows
   (and their reference checks) are the source of truth for what is still live.

## Tradeoffs

- The keep set is a superset-safe answer: entries the live path can still hit are
  kept, so the prune never deletes a line that would have been spoken.
- The unreachable share is measured, not guessed: the doctor and the prune share
  the same dry-run walk, so the row cannot drift from the command that acts on
  it.

## Validation

- `internal/speech` (`Cache.PruneUnreachable`): reachable entries survive, the
  unreachable go through the reference-checked deletion, dry run deletes nothing,
  and a blob still referenced by a catalogue row survives its entry's drop.
- `internal/runtime` (`SpeechReachableKeys`, `PruneSpeechCache`): the reachable
  set covers published revisions including narrator-voiced keys and page-scoped
  utterances; a prune against invented data removes only the unreachable.
- `internal/api` (`POST /api/v1/speech/cache/prune`, `speech.cache_prune` job):
  202 with the accepted body, job payload round-trip, handler outcomes, and the
  OpenAPI contract.
- `internal/doctor` (`speech.cache_disk`): low free space degrades, empty cache
  answers OK.
- `cmd/game-narrator` (`cache prune`): the operator command runs the shared code.
- `make lint` (gofmt, vet, web api check, ADR numbers, secret scan).

## Reversal plan

Remove the prune entry point, the job route, the CLI command and the doctor row;
the reachable-key walk stays as the keep-set computation and can be reused by any
future cache-maintenance surface.

## Links / references

- ADR-0023 (speech cache), ADR-0039 (runtime speech pipeline), ADR-0070, ADR-0095,
  ADR-0101, ADR-0153, ADR-0155, ADR-0156, ADR-0041 (reference check).

## Amendment 2026-10-22: shared-slot lines keep every voice they can speak in

A shared-slot line (ADR-0156) gets its voice per occurrence. It can be the narrator, the linked character when Clef names it, or a generic kind voice (ADR-0158). All of these keys are reachable, so the prune keeps them. Key computation is cheap and no synthesis is implied.

Test: `TestReachableKeysCoverSharedSlotVoices`.
