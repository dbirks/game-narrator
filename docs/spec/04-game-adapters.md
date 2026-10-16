# 04 — Pluggable game adapters, editions, text layouts and profiles

## Portability rule

Never bake Zelda-specific assumptions into the core runtime, source format parser, actor resolver or React UI. Every game has a `GameProfile` and optional `GameAdapter` containing platform-specific extractors and visual/text recognition helpers. Minimal manual profiles must work with no script and a narrator fallback; rich profiles can exploit canonical message IDs and graph links.

## Adapter capability set

`identifyEdition()` optional; `parseArchive()` optional; `extractMessages()`; `decodeMarkup()`; `resolveVariableTokens()`; `proposeRegions()`; `detectTextBox()`; `detectPageCompletion()`; `hintScene()`; `proposeSpeakerVisuals()`; `buildGraph()`; `renderSpeechText()`; `exportProfile()`; `validateProfile()`.

Implement interfaces and a versioned adapter manifest (semver, required permissions, supported game/edition hashes, supported locales, test fixtures, schema compatibility). External executable parser plugins are a future optional capability and must run with limited privileges. First-party adapters are statically linked in Go.

## Visual profiles

A game profile may define multiple region detectors: bottom dialogue box, top banner, sign overlay, inventory item popup, non-boxed subtitles, menu overlay, cutscene captions. Each region has reference aspect ratio, normalized coords, margins, background/text color hints, OCR engine PSM/language, minimum stability time, exclusion masks (portraits, animated cursors, icon area), typewriter/incremental-render behavior and a classifier for whether it is intentionally selected. Coordinates must adapt to 4:3 vs 16:9 stretch, letterboxing, overscan and capture resolution.

## Dialogue graph

Edges from explicit BMG `next` metadata are high provenance, but may be conditional. Additional graph construction can use source order, scene sequence and manually observed transitions; mark lower confidence. Graph traversal must allow branches, cycles, NPC repeats, non-linear travel, item menus, scene resets and “unknown” transitions. Never force the most likely graph edge over strong contrary OCR text.

## Wind Waker reference adapter

- **Only illustrative and user supplied**: its original GameCube game file/script is not shipped in release artifacts.
- Parse BMG generic message sections; support Wind Waker-specific INF1 metadata including message ID, next message ID, textbox style and typewriter flag if present. Treat undocumented fields as opaque bytes and retain raw offsets.
- Import example archived message file(s) via user-supplied image or previously extracted dump. Use cautious extraction and no unauthorized game-file acquisition.
- Decode color/control codes, name placeholders, button icons and pause commands through a lossless AST, then configurable English voice-rendering mapping.
- Character labels may be absent; use scene signals, neighboring message chain, screenshot evidence and operator corrections.
- Test common Wind Waker visual types, e.g. standard bottom textbox, multiple pages, item notifications, signs, cutscenes, dialogue choices, variable button icons, repeated NPC lines. Concrete line text/IDs must come from authorized test copy and be labeled, not hardcoded in spec.

## Generic adapter minimum

Accept user-uploaded TSV/CSV/JSON/plain text, map columns, build message index without message IDs, use text+visual OCR with conservative narrator/unknown speaker, and allow annotating at runtime. The generic adapter can use heuristically discovered ROIs but must remain usable without inference or graph metadata.

## Two-game portability verification

1. Reference user-owned Wind Waker profile with rich graph and varied text kinds.
2. A **synthetic test game profile** with deliberately different layout, language, text reveal, speaker labels and no BMG. It must pass ingestion, recognition, matching, TTS, dev UI and export/import tests unchanged in core code.
3. Optionally import a second authorized real game with a different engine; the core must work with only an adapter/config change, not Zelda-specific conditionals.

## Adapter evolution and repair

Profile inspector shows version and coverage by content type. Curator can move ROI via drag handles; edits yield new region-set revision; test against stored screenshots before publish. Automatic region discovery proposes locations but labels can be corrected. Unknown mappings become first-class `correction` training/test samples rather than ad hoc code patches.
