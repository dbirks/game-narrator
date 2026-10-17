# ADR-0041: Storage resilience evidence — disk full, power outage, asset store offline (M7 storage rows)

Date: 2026-10-17 (amended 2026-10-17: "Reference check before delete" section below)
Status: accepted
Records decisions made in this change (`internal/store/repo_tx.go`, `internal/assets/assets.go`, `internal/assets/local.go`, `internal/speech/cache.go`, plus the resilience tests `internal/store/resilience_test.go`, `internal/assets/resilience_test.go`, `internal/jobs/resilience_test.go`, `internal/speech/resilience_test.go`).
Owners: project
Related requirements/tests: docs/spec/13 "Resilience matrix" rows Disk full / Power outage / Asset store offline; AGENTS.md rule 10 (version and migration discipline — backup/restore tests), "Restart tests demonstrate persistent games/voices/cache and resumed or safely failed jobs"; ADR-0008 (SQLite driver), ADR-0014 (asset store local backend), ADR-0023 (speech cache and audio scheduler)

## Context and constraints

The resilience matrix (docs/spec/13) names three storage faults with required behaviour, and AGENTS.md bars claiming a row until it is tested:

| Matrix row | Required behaviour |
|---|---|
| Disk full | Do not corrupt source DB; refuse new assets, evict safe transient cache |
| Power outage | Recover transactions and jobs, stop orphan speech |
| Asset store offline | Local cache used; new approved assets queued only if safe/durable |

The storage layer (`internal/store`, `internal/assets`, `internal/jobs`, `internal/speech/cache.go`) had no tests exercising these faults, and two product gaps made the "Disk full" row unimplementable by callers: a SQLite capacity failure surfaced as an opaque driver error (callers cannot distinguish "disk full, evict and retry" from "server fault"), and the asset store had no primitive to reclaim a single blob, so a transient cache could not be evicted safely.

## Decision

Prove the three rows with fault-injected tests, and add the minimal product surface the tests exposed:

1. **Typed capacity error.** `store.ErrNoSpace` (`internal/store/repo_tx.go`): SQLite's `SQLITE_FULL` (primary result code 13) is mapped in `mapWriteError`/`mapReadError` to a sentinel callers switch on with `errors.Is`. A full disk is a capacity condition with a defined recovery, not a data fault (409-style) or a server fault (500).
2. **Single-blob reclamation.** `assets.Store.Delete(ctx, hash)` (`internal/assets`): removes exactly one committed blob through the same safety chain as every read (hash normalization, chain check, validated open, base-name identity), fsyncs the directory, refuses symlinks/non-regular files. Reachability stays the caller's contract — the same principle as `DeleteUnreachable`'s predicate.
3. **Transient-cache eviction.** `speech.Cache.EvictTransient(ctx, maxEntries)` (`internal/speech/cache.go`): evicts up to `maxEntries` (≤0 = all) of the cache's own index entries, least-recently used first (recency is process-local; entries loaded from a previous run are seeded at load time, so untouched entries are evicted first). Per entry: index entry removed, then its blob deleted. The safety argument: the index holds only generated TTS utterances, every key is re-synthesizable, and nothing outside the index names those blobs — approved voice references and imported source data live in the catalogue and are unreachable from this method by construction. The quota is the caller's (runtime/operator policy on a capacity error); the method is the mechanism, not the policy.

Test techniques:

- **Disk full / SQLite**: `PRAGMA max_page_count` pinned at the database's current page count on a pool-pinned single connection — the VFS-free way to make the next page-growing write fail with a genuine `SQLITE_FULL`.
- **Disk full / assets**: a real `ENOSPC` from a genuinely full filesystem — a ~20 MiB `tmpfs` mounted by the test parent inside user+mount namespaces (`unshare -Urm`), no root needed; if the environment blocks namespaces, the same scenario runs against a fault-injecting reader that fails with `ENOSPC` after N bytes (identical store code path: stream → error → staging cleanup).
- **Disk full / speech cache**: a capacity-accounting `assets.Store` wrapper whose stream fails with `ENOSPC` mid-write once committed bytes would exceed the quota (real partial staging writes and real cleanup in the wrapped `Local`).
- **Power outage**: re-exec of the test binary; the child opens the store, enqueues+claims a job (lease live, attempt in flight), stages an uncommitted asset, announces `READY`, and is SIGKILLed by the parent. No cleanup runs — the on-disk shape of a power cut.
- **Asset store offline**: an `assets.Store` wrapper that refuses every operation with a typed sentinel while "offline".

## Alternatives considered

- **FUSE overlay or root-only size-limited filesystem for the disk-full test.** Needs root (the target dev environment is a plain user) or adds a kernel-module dependency to the test suite. The user-namespace tmpfs is rootless and produces a kernel-originated `ENOSPC`; the fault-reader fallback keeps the test honest where namespaces are blocked.
- **Queue new approved assets in memory when the store is offline.** The speech cache has no durable queue; an in-memory queue would silently lose work on the next crash, which is the exact failure the "Power outage" row forbids. The matrix allows queuing *only if safe/durable*, so the product refuses with a typed error and documents that (this ADR + `EvictTransient`/offline test), instead of pretending durability.
- **Evicting via `DeleteUnreachable` with a speech-side reachability predicate.** The predicate would have to encode catalogue knowledge (voice references, source blobs) that the speech package does not own; a wrong predicate deletes approved content. `Delete` on the exact evicted hash, bounded by the cache's own index, makes the safe set visible in the code.
- **Adding a durable queue table for offline assets (schema migration).** Out of scope for the storage-rows milestone: the runtime's import job already persists work in the durable job queue, and the speech cache is regenerable by definition. Revisit if a non-regenerable asset path appears.

## Consequences and risks

- Callers of `store` can now branch on `ErrNoSpace` (refuse → evict → retry); the API layer should map it to a 507-style capacity response when it surfaces (follow-up, `internal/api` is out of this change's boundary).
- `assets.Store` implementations must implement `Delete`; only `Local` exists today, so the interface change is compile-checked and closed.
- `EvictTransient` LRU state is process-local: after a restart, order is re-learned from `LoadAll` + accesses. Eviction is a capacity escape hatch, not a correctness mechanism, so this is acceptable; a persistent recency column is a follow-up if the appliance's cache grows large enough that cold-start eviction order matters.
- `max_page_count` is per-connection; the test pins the pool to one connection and asserts the pool really serves the capped connection before relying on it.
- The user-namespace test is skipped-to-fallback, not skipped, when namespaces are unavailable — the contract is still asserted, with the fault writer in place of the kernel.

## Evidence and test plan

Proven by these tests (each logs one observable line, `resilience: …`):

| Row | Test | What is proven |
|---|---|---|
| Disk full | `internal/store` `TestDiskFullSQLiteFullIsTypedAndLeavesNoCorruption` | `SQLITE_FULL` maps to typed `ErrNoSpace`; `PRAGMA integrity_check` = ok after the failure; committed job intact; close+reopen recovers the WAL (committed data still there) |
| Disk full | `internal/assets` `TestDiskFullAssetsRefusesWritesWithoutCorruption` | real `ENOSPC` (20 MiB tmpfs) is typed via `errors.Is`; a partial asset never appears under its final name; failed staging is discarded; committed blobs verify; `Delete`-based reclamation recovers new writes (namespaces blocked → fault-writer fallback asserts the same) |
| Disk full | `internal/speech` `TestDiskFullCacheRefusesThenEvictionRecovers`, `TestEvictTransientNeverTouchesForeignBlobs` | a refused generation leaves no index entry and no blob; LRU eviction removes the oldest transient entries first, reports freed bytes, leaves survivors servable; the previously refused write then succeeds; a foreign (voice-reference-shaped) blob survives full eviction |
| Power outage | `internal/jobs` `TestPowerLossKilledMidWorkRecoversJobsAndAssets` | SIGKILL mid-job+mid-asset: integrity ok (WAL recovered); the job row is intact and `running` with its lease; the startup sweep discards exactly the one orphan staging file and its content is not visible as a committed asset nor committable; `RecoverOnStartup` marks the job recoverable with the attempt recorded `crashed` ("process died while the attempt was running") and requeues it (resumable kind, budget remains); a fresh worker claims it — resumed, not failed |
| Asset store offline | `internal/speech` `TestAssetsOfflineDegradesAndRecovers` | cached reads degrade with a typed error (no panic, no stale serve); new writes are refused, not queued (no durable queue exists — refusal is the documented safe behaviour); recovery is transparent (cache hit again, new writes commit) |

Run: `go test -count=1 -v -run 'Resilience|DiskFull|PowerLoss|Offline' ./internal/store/... ./internal/assets/... ./internal/jobs/... ./internal/speech/...` (all pass; `-race` green on the four packages).

## What is still unproven

- **Real disk full on the UNO Q** (ext4 on the appliance's eMMC, with the WAL sidecars): the `max_page_count` and tmpfs faults exercise the same SQLite/kernel errors, but the hardware row still needs a UNO Q smoke (AGENTS.md rule 12: report as unverified, never as passed).
- **Real power loss** (capacitor drain, mid-checkpoint): SIGKILL covers the process view of the same file state; the hardware row is unverified.
- **Asset store offline as a network failure** (S3-compatible backend): the offline wrapper proves the client-side contract; the remote backend itself does not exist yet (ADR-0014 keeps the interface open for it).
- **Runtime-owned eviction policy**: who calls `EvictTransient` when a write fails with a capacity error, and at what quota, is a runtime concern (out of boundary here).
- **Orphan speech stop on power loss**: "stop orphan speech" — the player-side half — belongs to the audio scheduler row; this change proves the storage/jobs half (in-flight work recovers and staged bytes are swept).

## Migration / rollback

No schema change, no data migration. Reversal: drop `ErrNoSpace` and the `SQLITE_FULL` mapping (capacity errors become opaque again), delete `assets.Store.Delete` and `Local.Delete`, delete `Cache.EvictTransient` and the LRU bookkeeping, delete the four `resilience_test.go` files. Nothing else references the new surface; the tests delete with it.

## Follow-ups

- Map `store.ErrNoSpace` to a capacity HTTP status in `internal/api` when the write path surfaces it (507 Storage Full or a documented 503 + `retry-after`).
- Runtime eviction policy: on `ErrNoSpace`/`ENOSPC` from a commit, call `EvictTransient` with a quota derived from the failed write's size, then retry once (bounded-retry rule).
- Persistent LRU recency if cold-start eviction order becomes measurable (APM: eviction count, freed bytes).
- UNO Q hardware smoke: real disk-full (fill the eMMC) and real power pull, per the "still unproven" list.

## Amendment: Reference check before delete (2026-10-17)

Review of the eviction contract (commit daaeaa0) found the safety argument in decision 3 — "nothing outside this index names these blobs" — true of the **entries** but false of the **blobs**: assets are content-addressed and *shared*, so the blob hash of an evicted cache entry can be the very hash an approved voice reference, an audition clip, a character portrait or an imported source names. Deleting that blob destroys catalogue bytes under the guise of cache reclamation; the previous `TestEvictTransientNeverTouchesForeignBlobs` only covered a foreign blob with *different* bytes.

Decision (amends decision 3, `EvictTransient`):

1. **Catalogue-side check.** `store.Store.AssetReferenced(ctx, sha256) (bool, error)` (`internal/store/asset_ref.go`): one UNION over all five columns that name asset-store blobs outside the speech cache index — `source_blobs.sha256` (PK), `sources.content_sha256`, `characters.portrait_asset_id`, `voice_profiles.canonical_reference_asset_id`, `voice_auditions.asset_sha256`. Every branch is an indexed point lookup: two were indexed at creation (0001), migration `0010_asset_reference_indexes.sql` adds the other three (indexes only — no column, no default, no data change; existing databases upgrade on the next `migrate`). Columns whose hashes live only inside JSON (`characters.visual_descriptors`, `scenes.visual_anchors`, `regions.mask_asset_ids`, `messages.attribution_evidence`) are evidence pointers, not ownership references, and are deliberately out of scope. A malformed hash is an error, never "unreferenced" — answering no there would authorise deleting an unaddressable blob.
2. **Cache-side gate.** `assets.ReferenceChecker` interface (`internal/assets/reference.go`); `speech.Cache` takes it as an OPTIONAL `Option` (`speech.New(…, WithReferenceChecker(store))`), so existing construction sites keep compiling. Per evicted entry: checker says "referenced" → the index entry is dropped (still counted as `evicted`) but the blob is kept, so its size does not enter `freed`; "unreferenced" → the blob is deleted; checker error OR no checker configured → the blob is kept and counted in `Stats().RefusedDeletes`. **A cache without a checker never deletes a blob at all** (index-only eviction, logged once per process): safe by default, because deleting a blob without proof that it is unshared is exactly how approved content is lost.

Tests (each labels what is stubbed, AGENTS.md rule 3): `internal/store/asset_ref_test.go` — the query sees one row in each referencing table, is index-backed across the whole UNION (`EXPLAIN QUERY PLAN` asserts no table scan), and rejects malformed hashes; `internal/speech/evict_reference_test.go` — shared hash (a voice-reference row naming the entry's exact blob) → entry evicted, blob still verifies, `freed` = 0, refusal counted; no checker → index-only with both blobs intact; checker error → blob kept. The pre-existing resilience eviction tests now wire an explicit "unshared" stub checker (the real query is proven in `internal/store`).

Wiring: the production call sites (`cmd/game-narrator/main.go`, the runtime speech cache) still construct the cache without a checker, so eviction is index-only there — the safe default, and a visible one (`RefusedDeletes` > 0 after any eviction). Those packages are owned by other workers at the time of this amendment; wiring `WithReferenceChecker(store)` at those call sites is a **follow-up**, not a done item.

Reversal addendum: additionally drop `AssetReferenced` + its tests, `assets.ReferenceChecker`, the cache option, `RefusedDeletes` and the `0010` indexes (index-only; `DROP INDEX` is trivially reversible), and restore the old unconditional `Delete` in `EvictTransient`. Nothing else changes.
