# ADR-0014: Asset store local backend — content-addressed blobs, private modes, hard-link commit
Date: 2026-10-16
Status: accepted
Owners: project
Related requirements/tests: docs/spec/02 "Asset store contract" + "Transaction workflows" + "Crash recovery", docs/spec/14 "Filesystem/import" and "memory/time limits", README invariant 6 ("the *canonical* voice assets live in Game Narrator's configured durable asset storage"), AGENTS.md rules 5 and 7; builds on ADR-0005 (application-owned voice references) and ADR-0008 (what SQLite deliberately does *not* store); `go test -race -count=3 ./internal/assets/`

## Context and constraints

- docs/spec/02 fixes the interface (`PutStaged/Commit/Get/Stat/Verify/Export/DeleteUnreachable`)
  and says blobs live on disk while SQLite stores only hash, size and media type
  ("SQLite schema principles"). ADR-0005 makes that on-disk namespace the canonical
  home of voice reference audio and generated utterances, so the store is on the
  playback path, not a scratch cache.
- The bytes are untrusted twice over: an onboarding agent imports files it did not
  create (docs/spec/14 "Filesystem/import" — path traversal, symlinks, size bombs),
  and a generated WAV may be re-imported from an inference server. Validation has
  to happen before a path is touched, not after.
- Blobs are large and the reader is a real-time audio path. Holding a whole clip in
  memory to hash it, or letting an import fill the disk, are both disqualifying;
  memory must stay bounded by a fixed buffer regardless of blob size.
- Durability requirement from docs/spec/02: "atomic temp+fsync+rename". A blob name
  must never be visible before its bytes are, and a crash must leave a *recoverable*
  state (staging leftovers), not a corrupt-looking blob namespace.
- Same-content writes are routine and concurrent: two synthesis jobs for the same
  utterance produce the same address, and a restart or a second process sharing the
  root can be committing it at the same moment. Deduplication must not depend on a
  lock that only one process can see.
- Modes must stay private (0700/0600 per docs/spec/02) because the same tree holds
  copyrighted user material and voice reference audio.
- Measured environment: the dev host's data directory sits on `btrfs`
  (`stat -f -c %T`, `df -T`) and accepts hard links (a probe `ln` reported
  `2 links`). Target release host is Debian ARM64 on ext4/f2fs — hard links are the
  normal case, not an exotic one.

## Decision

`internal/assets.Local` is the local backend for the docs/spec/02 `Store`
interface. Its guarantees, all of them tested:

1. **Layout.** `<root>/blobs/ab/cd/<sha256>` for committed content and
   `<root>/staging/<id>` for in-flight writes. The two-character fan-out keeps
   directory sizes sane on every filesystem we target while the full hash stays the
   last component, so an address is still the only thing a reader needs. Staging IDs
   are 32 random hex characters (not a counter, not the content hash), so concurrent
   importers cannot collide and a staged blob's address is not guessable before
   `Commit`.
2. **Validation precedes filesystem access.** A hash must be exactly 64 lowercase hex
   characters and a staging ID exactly 32 (`ErrInvalidHash`, `ErrInvalidStagingID`);
   both double as path components, so nothing else can address a file. Hashes are
   never normalised (no trimming, no uppercasing) — two spellings of one address
   would put the catalogue and the filesystem out of step. Media type comes from
   `http.DetectContentType` on the first 512 bytes, never from a filename or a
   caller-supplied header.
3. **Private modes.** Root, `blobs/` and every fan-out directory are created 0700,
   every file 0600, and `NewLocal` re-`chmod`s a directory whose mode drifted. An
   existing file's mode is unaffected by the `O_CREATE` argument, so staging pins
   0600 with an explicit `Chmod` after opening.
4. **Symlinks and non-regular files are refused at every step.** `NewLocal` refuses a
   root that *is* a symlink (ancestors are not inspected — a parent may legitimately
   be a link, everything *under* the root is checked); `checkChain` validates each
   path component before use; `openValidated` does `Lstat` → reject symlink /
   non-regular → `Open` → `fstat` and `os.SameFile`, which closes the
   Lstat-then-open race without `O_NOFOLLOW` (not portable across contributor
   platforms). GC never follows and never *deletes* a link or a special file: under
   the containment rule that is a tampering signal, not garbage.
5. **Streaming with bounded memory.** `PutStaged` copies through a fixed 64 KiB
   buffer while hashing, so a 64 MiB import keeps live heap growth under a 16 MiB
   assertion (docs/spec/14 "memory/time limits"). Nothing in the package buffers a
   whole blob, including `Export` and `Get`.
6. **Size cap.** `WithMaxSize(n)` aborts the *stage* with `ErrTooLarge` the moment the
   stream exceeds `n` and discards the bytes; `n == 0` means unlimited and a negative
   `n` is rejected at construction, so a forgotten cap is visible rather than
   silently permissive.
7. **fsync of file *and* directory.** Staging is fsynced before `PutStaged` returns,
   and `Commit` re-fsyncs the staged inode (that inode *becomes* the blob, so a
   committed name must never be one whose bytes are still only in the page cache)
   and then fsyncs the destination directory — the part of "atomic temp+fsync+rename"
   a file fsync cannot cover, since a blob whose directory entry never reached the
   journal disappears on remount.
8. **`Commit` lands the blob with `link(2)`, not `rename(2)`.** `link` fails with
   `EEXIST` when the destination exists, which is the atomic "create the name, but
   never replace an existing one" the standard library does not expose. Replacing
   matters here because same-content commits are normal: with `rename`, a concurrent
   commit of the same address swaps the inode behind a reader that has already
   resolved the name, and `openValidated`'s `os.SameFile` identity proof then fails
   on bytes that were never wrong — a sound clip rejected as `ErrUnsafePath`.
   Whoever wins the `link` owns the name; everyone else re-stats the winner
   (`dedupeStaged`, which reads `Info` back from the filesystem instead of trusting
   the caller's) and discards its own staging copy. `Commit` also re-hashes the
   staged file rather than trusting `PutStaged`'s bookkeeping, so a restart, a
   concurrent GC, or on-disk tampering between the two calls cannot smuggle
   mismatched bytes into the blob namespace (`ErrCorrupt`).
9. **`RecoverStaging`** walks `staging/` and discards every leftover, returning the
   count. Interrupted staged writes are *discarded, never resumed* (docs/spec/02
   "Crash recovery"): a half-written blob has no resume token, and re-importing is
   cheaper than reconciling an unknown byte count. Startup calls it before serving
   reads, so staging never accumulates garbage across restarts.
10. **GC is reachability-based and confined to `blobs/`.** `DeleteUnreachable`
    (docs/spec/02 "reference counts are advisory, GC via reachability") takes a
    predicate, refuses a nil one (that would mean "delete everything"), supports
    `dryRun`, and walks only `<root>/blobs` — staging is `RecoverStaging`'s business
    and the rest of the filesystem is nobody's business here. An unreadable entry
    aborts the walk rather than letting GC act on an incomplete picture; emptied
    `ab/cd` directories are pruned deepest-first, and `blobs/` itself is never
    removed so the store stays usable.
11. **Interface, not implementation, is the boundary.** An S3-compatible backend can
    be added behind the same `Store` interface (docs/spec/02 "Optional
    S3-compatible/network provider") without touching callers.

## Alternatives considered

- **`rename(2)` to land a blob** (the original `Commit` implementation). Rejected
  for the reason in Decision 8 — rename replaces, and the resulting race was
  reproduced in tests before the fix (see the "Add the content-addressed local
  asset store" commit message). A check-then-rename with a lock was
  considered and rejected because an in-process mutex says nothing about a second
  service, a restore job, or a `doctor` run sharing the root, and a TOCTOU window in
  front of `rename` is exactly the window `link(2)` closes for free.
- **Stream directly into the blob path under `O_EXCL`.** Rejected: readers would see
  an incomplete blob for the duration of the write, and a crash would leave a partial
  file *inside* `blobs/`, where GC then has to distinguish garbage from a live blob —
  the staging namespace exists so that orphan detection is a directory listing.
- **Flat `blobs/<sha256>`, or three-level fan-out.** Flat is one directory per hash
  prefix collision-space (millions of entries on a large voice + cache tree); a
  third level adds depth to every walk (including GC and `Export`) for no benefit at
  our sizes. Two levels (`ab/cd`) is the middle that every target filesystem handles.
- **Refuse `Commit` when `link(2)` is unavailable.** Rejected: it would make a
  no-hard-link filesystem (FAT/exFAT — realistic for an externally mounted stick) a
  total failure for the whole product. `commitByRename` handles that case with a
  stat-then-rename, which is *correct* on a filesystem where a second concurrent
  committer is a corner case rather than a design assumption; it is documented as
  not atomic against a concurrent creator of the same address, which is exactly why
  it is the fallback and not the primary path.
- **SQLite BLOBs for assets.** Rejected by docs/spec/02 outright (export/portability,
  streaming reads, cache eviction, and a DB that would grow with every WAV), and
  ADR-0008 already fixed the driver boundary.
- **Content hash as the only integrity story, checked on read.** Kept as the
  mechanism (`Verify` re-hashes and returns `ErrCorrupt`) but not treated as a
  license or approval check — docs/spec/02 is explicit that a hash is not a
  substitute for those.

## Consequences and risks

- **The asset root must sit on a filesystem that supports hard links** (ext4, btrfs,
  xfs, f2fs: yes; FAT, exFAT, many NFS/9p and FUSE mounts, and any split where
  `staging/` and `blobs/` land on different mounts: no). Where hard links are
  unavailable the store still works through `commitByRename`, but loses the
  never-replace guarantee under concurrency — a degraded mode that must be *seen*,
  not silently accepted. **`doctor` should probe this** (attempt a throwaway
  `link`+`unlink` under the root and report `DEGRADED` rather than `OK`) — recorded
  as a follow-up, not as present behaviour: today `doctor` has no asset-store check
  at all.
- Extra cost per commit: one directory fsync and one full re-hash of the staged
  bytes. On the audio path this is off the playback deadline (it happens at import
  and at synthesis, not at speak time), and it buys a name that is either absent or
  complete.
- `link(2)` means a committed blob has a second name until the staging name is
  removed; if that removal fails the bytes are already committed and readable, and
  `RecoverStaging` collects the orphan — the failure is a leaked name, never a lost
  blob.
- Staging bookkeeping (`Local.staged`) is process-local, as documented: after a
  restart, IDs from the previous process are meaningless and the files are orphans
  for `RecoverStaging`. Two processes never share staging state.
- Reachability GC is only as honest as its predicate: a catalogue bug that reports a
  needed blob unreachable will delete it, which is why `dryRun` exists as the
  operator's first move and why GC refuses to act on an unreadable tree.
- Nothing here encrypts anything. 0700/0600 keeps other *users* out; it does not
  protect against a host compromise, and voice material carries consent obligations
  (docs/spec/14) that remain a policy layer above the store.

## Evidence and test plan

Implementation: `internal/assets/assets.go` (interface + validation), `local.go`
(layout, modes, staging, `link(2)` commit), `gc.go` (`RecoverStaging`,
`DeleteUnreachable`), `export.go` (tar + manifest).

Properties → tests (all in `internal/assets`):

| Guarantee | Test |
|---|---|
| round trip, idempotent commit of identical content | `TestPutCommitGetRoundTrip`, `TestCommitIsIdempotentForDuplicateContent` |
| modes are private (0700 dirs / 0600 files) | `TestModesArePrivate` |
| symlink refusal (root, blob, fan-out dir), non-regular refusal | `TestNewLocalRefusesSymlinkedRoot`, `TestGetRefusesSymlinkedBlob`, `TestCheckChainRefusesSymlinkedFanoutDir`, `TestBlobReplacedByDirectoryIsRefused` |
| hash/staging-ID validation before filesystem access | `TestInvalidHashRejectedWithoutFilesystemAccess`, `TestInvalidStagingIDRejected`, `TestValidHashAcceptsExactlyLowercase64` |
| expected-hash mismatch keeps nothing; tampering detected | `TestExpectedHashMismatchKeepsNothing`, `TestVerifyDetectsFlippedByte`, `TestCommitRejectsStagingContentThatChanged`, `TestTamperedBlobIsDetectableBeforePlayback` |
| bounded memory on a 64 MiB blob | `TestStreamingLargeBlobKeepsMemoryBounded`, `TestExportLargeBlobStreams`, `TestGetStreamsBodyWithoutBuffering` |
| size cap | `TestMaxSizeAbortsStage`, `TestMaxSizeUnlimitedByDefault`, `TestNewLocalRejectsNegativeMaxSize` |
| staging recovery across restart | `TestRecoverStagingDiscardsLeftovers`, `TestReopenRootServesExistingBlobsAndRecoversStaging`, `TestAbortDiscardsStaging` |
| GC limited to `blobs/`, keeps staging + foreign files, ignores symlinks, prunes fan-out | `TestDeleteUnreachableKeepsStagingAndForeignFiles`, `TestDeleteUnreachableIgnoresSymlinks`, `TestDeleteUnreachableEverythingAndPruneTree`, `TestDeleteUnreachableDryRunThenDelete`, `TestDeleteUnreachableRequiresPredicate`, `TestDeleteUnreachableHonoursCancellation` |
| concurrent identical commits dedupe to one blob (the `link(2)` case) | `TestConcurrentImportersDedupe` (16 goroutines, one expected blob, staging empty) |
| a committed name does not change inode under another handle | `TestCommitMakesBlobVisibleAtomicallyToAnotherHandle` |
| cancellation propagates mid-write | `TestCancellationDuringStageDiscards`, `TestReaderErrorPropagates` |
| export is exact, byte-stable, all-or-nothing | `TestExportContainsExactlyTheRequestedBlobs`, `TestExportIsByteStable`, `TestExportMissingRefWritesNothing`, `TestExportBodiesSurviveVerification` |
| interface conformance | `TestStoreInterfaceIsSatisfiedByLocal` |

Commands and observed output:

```
$ go test -race -count=3 ./internal/assets/
ok  	github.com/dbirks/game-narrator/internal/assets	4.866s

$ stat -f -c %T "${XDG_DATA_HOME:-$HOME/.local/share}"   # asset root filesystem
btrfs
$ ln probe probe2 && stat -c '%h links' probe            # hard links available here
2 links
```

Not yet covered (open verification, tracked below): behaviour on a no-hard-link
filesystem (the `commitByRename` fallback has no FAT/exFAT fixture) and a `doctor`
check that reports the degraded mode.

## Migration / rollback

The store is new: no existing deployment data to migrate, and no schema change (the
blob namespace is files on disk; SQLite still holds only hash/size/media type). To
roll back, delete nothing and switch callers off the `Store` interface — blobs remain
readable by content hash from any other tool, since the layout is the plain
`blobs/ab/cd/<sha256>` convention.

Moving an existing root between filesystems (e.g. off an exFAT stick onto ext4) is a
copy, not a rename: `Export` to a tar on the destination and re-`PutStaged`/`Commit`
from it, which re-establishes hard-link commit semantics and re-verifies every hash
on the way in. Verify with `Verify` per ref and `Export`/`Import` byte-stability
(`TestExportIsByteStable`, `TestExportBodiesSurviveVerification`). Re-importing a
blob that is already present is a no-op by address, so a half-finished move can be
re-run.

## Follow-ups

- `doctor`: probe hard-link support in the configured asset root (`link`+`unlink` on
  a throwaway file) and report `DEGRADED` when it is absent, so the no-hard-link
  fallback is visible in a report rather than inferred from a mount. Also add the
  asset root itself as a storage check alongside `storage.data_dir`.
- Wire the store into the service: import jobs and the voice/utterance cache call
  `RecoverStaging` at startup before serving reads (currently a tested method with
  no caller in the runtime path — M1/M4 work).
- Retention classes from docs/spec/02 (ephemeral frame cache, sampled observations,
  pinned voices, LRU utterance cache) need a policy layer above
  `DeleteUnreachable`; the GC primitive deliberately knows nothing about them.
- S3-compatible backend behind the same interface (docs/spec/02), which is where the
  `commitByRename` fallback should become a real conditional-write instead.
- Measure commit latency (double hash + 2 fsyncs) on the UNO Q's storage, where
  fsync cost is the unknown; record it in the M2/M4 performance baseline.
- Review alongside ADR-0005 (voice source of truth) once real voice registration
  exists, to confirm the store is the only writer of canonical audio.
