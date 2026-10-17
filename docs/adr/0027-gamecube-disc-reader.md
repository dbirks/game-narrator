# ADR-0027: GameCube disc reader (internal/gcdisc)
Date: 2026-10-17
Status: accepted
Owners: david
Related requirements/tests: GN-T004 (disc access), `internal/gcdisc` unit tests (`TestCISO`, `TestCISOErrors`, `TestYaz0*`, `TestRARC*`, `TestFST*`), `FuzzYaz0`, `FuzzRARC`, real-image test `TestRARCRealImage` (env-gated)

## Context and constraints
The runtime must read user-supplied GameCube disc images (WotL-style single-disc images) to locate game text (RARC archives of BMG files) without a mounted disc drive, on both Arch (development) and the Debian ARM64 UNO Q target. Disc images are untrusted user input: hostile or corrupt containers (inflated sizes, cycles, path tricks, decompression bombs) must be rejected or bounded, never crash the process, and never leak copyrighted content. The reader must stay read-only and cheap enough to run on the UNO Q's modest memory budget.

## Decision
`internal/gcdisc` is a read-only, layered disc reader with exactly these layers, each bounded independently:

- **CISO container** (`ciso.go`): verified layout — `"CISO"` magic at 0x0000, little-endian block size at 0x0004, **block presence map at offset 8** (0x7FF8 bytes, one byte per logical block, only 0 or 1), present blocks stored contiguously from 0x8000. Logical→physical mapping uses a prefix sum of present blocks; absent blocks read as zeros; a few hundred bytes of trailer after the last block are ignored. The GCM boot header lives at logical offset 0 (inside the first stored block for CISO).
- **GCM boot header** (`ciso.go`): game ID/maker, name (probed at 0x10 and 0x20), disc number/total, and the FST offset/size at 0x424/0x428 (big-endian).
- **FST** (`fst.go`): 12-byte big-endian entries — `type(1) | name_offset(3) | u32 a | u32 b`. Verified: file entries carry **absolute logical disc offsets** (a=offset, b=size); directory entries carry parent index and subtree-end. Exposed as a lazy `fs.FS` (`fs.ReadDirFS`, `fs.StatFS`); only the FST table itself is read up front.
- **RARC** (`rarc.go`): big-endian RARC header/info/node/entry tables, with the string table and data section located by table-relative offsets; subdirectories resolved through node indices; Yaz0-flagged entries decompressed on read.
- **Yaz0** (`yaz0.go`): `"Yaz0"` magic, big-endian declared size, 8 reserved bytes, 8-symbol groups (control bit MSB first: set = literal, clear = 2-byte back-reference; N=0 extends length with a third byte, L = B3+0x12).

**Safety bounds.** Every declared size, count, and offset is validated against the container/disk before use:
- Sizes: CISO block size must be a power of two ≤ 64 MiB and the file must hold all present blocks; FST file ranges must lie within disc size; RARC data/node/entry/string sections and per-file offsets must lie within the archive; Yaz0 declared size is capped at `Yaz0MaxOut` = 64 MiB (decompression-bomb guard) and may be further capped by a caller `maxOut`.
- Counts: FST entry count ≤ 200,000; RARC node and entry counts ≤ 65,536 each.
- Depth: FST directory nesting ≤ 64; RARC directory nesting ≤ 64.
- Cycles: RARC node walk uses a visited set (rejects node cycles and bad indices); FST subtree-end must satisfy `i+1 ≤ subtreeEnd ≤ count`.
- Names: RARC names ≤ 255 bytes, no `/`, NUL, `.` or `..`; FST names ≤ 255 bytes with no `/` or NUL; both are read from their string tables with bounds checks.
- Fuzzing: `FuzzYaz0` and `FuzzRARC` treat arbitrary bytes as hostile input (no panic; successful Yaz0 output never exceeds the cap). The RARC fuzzer found and fixed a **uint32 wraparound** in RARC offset validation (a table offset could wrap and pass the in-bounds check); offsets are now validated as non-wrapping sums.

**Lazy reads.** `Disc.FS()` reads only the FST table at open; file bytes are served through `ReaderAt` sections bounded by the FST entry (never reading past a section), and RARC file bytes are decompressed on `Open`, not pre-scanned. Absent CISO blocks and trailing disc space read as zeros without touching disk.

**No-copy rule.** Game bytes and extracted text never leave process memory except into the user's private local profile (the configured content store); nothing is uploaded, written to shared locations, or embedded in logs. Tests that touch real images log **counts and sizes only** (e.g. `entries=… bmg_total_bytes=…`), never file contents.

**Real-image evidence.** Against the reference disc image (env-gated via `GAME_NARRATOR_GAME_IMAGE_PATH`): the disc FS walks cleanly with **2,213 files**, and `res/Msg/bmgres.arc` opens as a RARC containing **1 BMG of 639,328 bytes** — matching the on-disc expectations and confirming the FST-absolute-offset and RARC interpretations.

**Removed heuristic.** An earlier build guessed a "FST base offset" (adding an empirical shift to the FST offset from the boot header). That was a **symptom of a CISO mapping bug** — the block map at offset 8 was being mis-parsed, shifting logical reads — not a real format feature. It is removed; the CISO map-at-8 layout is verified directly and the FST offset is used as-is.

## Alternatives considered
- **Shell out to existing tools (cxburner, dump, etc.)**: rejected — adds an unreviewable native dependency, breaks the no-copy rule's memory-locality, and cannot enforce per-layer bounds.
- **Full image copy into memory at open**: rejected — a 1.4 GiB disc image is impractical on the UNO Q; lazy `ReaderAt` sections achieve the same correctness with O(FST) memory.
- **Treat disc images as trusted**: rejected — they are untrusted user input and the only attack surface for path/content injection at ingest time; per-layer bounds are cheap and testable.

## Consequences and risks
- Positive: the runtime gains deterministic, bounded, testable access to disc text with no disk mounts; hostile images fail closed with descriptive errors.
- Cost: 5 container parsers to maintain; each format quirk (e.g. name probe offsets) must stay verified against the reference image.
- Failure modes: an unrecognized CISO variant or multi-disc layout fails at open (acceptable: user is prompted to check the image); a 64 MiB+ single file is refused (no known GC game file is that large uncompressed in text archives).
- Security: hostile-image surface is contained to `internal/gcdisc`; no network, no writes, and the no-copy rule keeps game content out of logs and shared storage.

## Evidence and test plan
- Unit (synthetic): `go test -count=1 -v ./internal/gcdisc/` — `TestCISO`/`TestCISOErrors` (map-at-8 mapping, absent blocks, EOF semantics, trailer, all bad-header cases), `TestYaz0LiteralRoundTrip`, `TestYaz0BackReference`, `TestYaz0Bounds` (cap, maxOut, truncation, bad distance, stop-at-declared-size), `TestRARCOpen`, `TestRARCWholeYaz0`, `TestRARCHostile` (truncation, bad magic, out-of-bounds data, node cycle, bad name, bad counts, flag inconsistency), FST layout tests.
- Fuzz: `go test -run='^$' -fuzz=FuzzYaz0 -fuzztime=20s ./internal/gcdisc/` and the `FuzzRARC` corpus (20 s runs clean; the RARC uint32 wraparound was found and fixed under fuzzing).
- Real image (env-gated, NOT RUN without the image): `GAME_NARRATOR_GAME_IMAGE_PATH=… go test -count=1 -run TestRARCRealImage ./internal/gcdisc/` — logs `files=2213` and `bmgres.arc` → 1 BMG, 639,328 bytes (counts/sizes only, per the no-copy rule).

## Migration / rollback
No persisted state is affected. Revert by removing `internal/gcdisc` and its callers' disc-reading entry point; nothing downstream consumes the reader's output persistently, so no data migration is needed.

## Follow-ups
- Multi-disc CISO sets (disc 2+ images) fail at open today; add support if a multi-disc title enters scope.
- Expose the disc image hash (not the content) in the onboarding profile manifest for provenance.
- Re-verify the CISO map-at-8 layout and FST absolute offsets against a second reference image when one is licensed in, to rule out title-specific CISO variants.
