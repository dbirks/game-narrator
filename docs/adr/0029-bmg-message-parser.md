# ADR-0029: BMG message parser (internal/games/bmg) — lossless tokens, tag-aware message boundaries
Date: 2026-07-18
Status: accepted
Owners: david
Related requirements/tests: docs/spec/04-game-adapters.md (Wind Waker reference adapter), docs/spec/02 (normalization and markup: byte-faithful AST with source offsets), docs/spec/14 (hostile input safety), `internal/games/bmg` tests (`TestParseSynthetic`, `TestParseHostile`, `FuzzParse`, env-gated `TestRealBMG`), ADR-0027 (disc/RARC access used by the real-file test)

## Context and constraints
The Wind Waker reference adapter must parse BMG files from a user-supplied disc image (`res/Msg/bmgres.arc` → single `.bmg`) into a **lossless AST**: original bytes byte-faithful, control bytes parsed into tokens with source offsets, never regex-removed; undocumented fields kept as opaque raw bytes; game text never committed, logged, or printed (AGENTS.md rule 5).

The task brief described the BMG layout (header, INF1/DAT1/MID1 sections, `0x1A` tags with a length byte). Per AGENTS.md ("own outcomes"), every layout claim was verified **structurally** against the real file (numeric fields only, no text):

- Header 0x20: magic `MESGbmg1` @ 0x00; **u32 @ 0x08 = 19979 is NOT the file size** (true size 639,328) — treated as an opaque reserved field; section count u32 @ 0x0C = 2; encoding byte @ 0x10 = 1 (1-byte text).
- Sections: `INF1` @ 0x20 (size 105,888, incl. 8-byte header) then `DAT1` @ 105,920 (size 533,408); both 32-byte aligned; no MID1/FLW1/FLI1 in this file.
- INF1 body: u16 count = 4,411 | u16 entry size = 24 | u32 reserved | entries of 24 bytes (u32 pool offset + 20 opaque attribute bytes).
- Entry offsets: 4,327 distinct, 85+ entries point at offset 0 (empty messages); offsets are **not monotonically increasing** (out-of-order entries point backward into the pool); every offset lands exactly on a NUL-terminated chunk start.
- The DAT1 pool holds **26,098** NUL-terminated chunks; entries reference only 4,327 of them (the rest are unreferenced content).
- Control tags: every `0x1A` in the pool is followed by a length byte L ∈ {5, 6, 7} (histogram 2,251 / 5,232 / 1,405; total 8,888). Critically, **tag bodies contain zero bytes** (e.g. a length-6 tag whose body is `FF 00 00 01`), so a naive "message ends at the next NUL" rule terminates 2,541 messages mid-tag and misclassifies those tags as truncated.

## Decision
`internal/games/bmg` exposes `Parse(b []byte) (*File, error)` returning `File{Encoding, Sections, Messages}`; each `Message` carries `Index`, `ID` (MID1 if present, else index), `TextOffset`, opaque `Attributes []byte`, `Raw []byte` (exact pool slice), and `Tokens []Token` (`Text | Tag | Invalid` with `Offset`, `Length`, `Group`, `Type`, `Args`, `Raw`). `Message.Reconstruct()` concatenates token `Raw` and **always equals `Message.Raw`** (the losslessness property, asserted in every test and in the fuzzer, plus a token-tiling check: no gaps/overlaps).

Structural decisions verified against the real file:

1. **Header**: section count from 0x0C (≤ 64), encoding byte recorded verbatim from 0x10; the u32 @ 0x08 is opaque (never validated as a size).
2. **Sections**: scanned sequentially with per-section bounds (size ≥ 8, within EOF, ≤ 64 sections); unknown section magics retained raw. Non-32-aligned gaps trigger a bounded (≤ 32-byte) scan for a printable magic; the real file is fully aligned so this path is a fallback.
3. **Message boundary**: a message runs from its INF1 offset to the first NUL **that is not part of a well-formed `0x1A` tag** — the terminator scan skips over tags with length `2 ≤ L ≤ 8` (real tags: L ∈ {5,6,7}). The length cap prevents a hostile length byte from swallowing a terminator (a tag whose declared length exceeds the message end is instead kept as an `Invalid` token; bytes are never dropped). Verified: with this rule all 8,888 real tags parse as `Tag`, 0 tokens are `Invalid`, and 4,280 of 4,324 non-empty monotone messages end exactly one byte before the next entry offset (the remainder explained by out-of-order/empty entries).
4. **Tokens**: `Group` = first body byte, `Type` = big-endian u16 of body[1:3] (u8 if the body is shorter), `Args` = full body bytes; malformed tags (L < 2, truncated) become `Invalid` tokens retaining every byte.
5. **Bounds** (hostile-input safety): section count ≤ 64, INF1 entry count ≤ 100,000, entry size ∈ [4, 64], entry offsets inside DAT1, message length ≤ 64 KiB; no panics on any input (fuzzed).

**No-text rule.** The real-file test (`TestRealBMG`, env-gated by `GAME_NARRATOR_GAME_IMAGE_PATH`, otherwise `NOT RUN`) logs statistics only: encoding byte, section magics+sizes, message count, entry size, tagged-message count, total tag count, numeric top-10 (group,type) histogram, invalid-token count — and asserts `Reconstruct() == Raw` for every message. Real-file evidence: `encoding=1`, sections `INF1:105888 DAT1:533408`, `messages=4411 entry_size=24`, `tagged_messages=2552 total_tags=8888 invalid_tokens=0`, top pairs `255:0=5232 0:7=1209 0:0=357 …`.

## Alternatives considered
- **Spec-literal "next NUL" boundary** (no tag awareness): rejected — the real pool provably contains zero bytes inside well-formed tag bodies; 2,541 messages would truncate mid-tag and 2,541 control tags would be mislabeled `Invalid`, corrupting the AST that downstream voice rendering depends on.
- **Message span = next INF1 offset**: rejected — entries are out-of-order with 85+ backward/zero pointers, so table-adjacency is not a reliable boundary; NUL termination (tag-aware) plus the INF1 table is consistent with the observed dense tiling.
- **Regex/substring tag stripping**: rejected by spec (docs/spec/02) — the lossless token AST is mandatory.
- **External BMG tooling**: rejected — adds an unreviewable dependency and cannot enforce the bounds/tiling invariants in-process.

## Validation
- `TestParseSynthetic`: synthetic BMG (header + INF1×3 entries of size 8 + MID1 + DAT1) with plain text, a mid-message tag whose body contains zero bytes, and a truncated tag; asserts section layout, IDs from MID1, offsets, attributes, token kinds/offsets, and `Reconstruct() == Raw` for all messages.
- `TestParseHostile`: wrong magic, section size past EOF, entry offset past DAT1, section count 1000, INF1 count 65,535, entry sizes 3/65, missing DAT1, short file → all rejected with descriptive errors.
- `FuzzParse`: no panic on any input; whenever `Parse` succeeds, every message satisfies `Reconstruct() == Raw` and its tokens exactly tile the raw slice. 20-second fuzz run clean.
- `TestRealBMG`: env-gated real-file parse; losslessness asserted for all 4,411 messages; stats-only logging (above).

## Consequences and risks
- The `maxTagLen = 8` cap is dialect-specific (Wind Waker BMG). A future BMG with legitimate longer tags would truncate such messages early; detected via a spike in `Invalid`/tagged stats in `TestRealBMG`, fixable by widening one constant.
- Unreferenced pool chunks (26,098 − 4,327) are intentionally not materialized as messages; if a later adapter needs them (e.g. flow-graph targets), a separate pool-chunk walk can be added without touching the lossless AST.
- `Encoding` bytes other than the 1-byte encodings get a UTF-16BE fallback path (unit size 2) without tag-aware termination; acceptable since this game is 1-byte and the encoding is recorded verbatim for downstream mapping.

## Reversal plan
Revert the package and this ADR; no other component depends on it yet. The tag-aware boundary is isolated in `messageEnd` (tokenize.go), so restoring spec-literal NUL termination is a one-function change if structural evidence ever contradicts it.
