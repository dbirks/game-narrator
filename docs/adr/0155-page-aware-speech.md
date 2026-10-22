# ADR 0155: Speak the reached box, not the whole page of dialogue

- Status: accepted (import derivation, runtime page selection and pre-synthesis implemented; replay validation pending)
- Date: 2026-10-22

## Context

Wind Waker scripts a conversation as a sequence of on-screen boxes ("pages"). A
single script message often spans three or four boxes; the narrator speaks one
box, then the next box of the same message, as the player advances through it.

Before this change the first box a player read committed the whole message and
the narrator spoke the full text — the boxes the player had not reached yet
included, as upcoming dialogue. A measured owner session (2026-10-22, Dolphin
emulator, Wind Waker script fixture) had the first box of a three-page message
match: the narrator spoke all three pages, about 9.0 s of speech against about
3.4 s for the one box actually being spoken. The next boxes then re-matched the
same message id and were skipped or merged as duplicates.

The cost is not only pacing: the unplayed boxes were spoken, and every later
box of the same script message was treated as a duplicate of the message rather
than as a new utterance of its own page.

## Decision

1. **Pages are derived at import.**
   - The Wind Waker adapter derives a page per display box at BMG parse time
     (`internal/games/adapter/windwaker/pages.go`): display lines (the pool's
     0x0A line break per ADR-0031) are grouped into boxes of at most
     `MaxBoxLines` (4) lines, and each box becomes one `games.MessagePage`
     row (`<message id>#p<ordinal>`), written through the existing
     `message_pages` import path (`internal/profileio/importfile.go`).
   - A message that fits one box yields a single page whose text is the
     message text — the pre-change identity, unchanged.
2. **The commit speaks the page the player reached.**
   - At commit (`internal/runtime/speech.go` `onCommit`) the wiring asks the
     page source for the message's pages and picks the page whose text contains
     the OCR reading (the matcher's fold, normalized containment), speaking
     only that page.
   - The utterance's cache identity becomes the page key
     (`<message id>#p<ordinal>`) for multi-page messages; a single-page message
     keeps the bare message id, so its behaviour is exactly as before.
   - A later box of the same message is therefore a different utterance (a
     different page key), not a duplicate of the first box.
3. **Runtime fallback for profiles without page rows.**
   - A profile that has no `message_pages` rows for a message falls back to
     splitting the message's canonical speech text into pages (sentence
     boundaries, packed into boxes of at most four lines).
   - Existing profiles therefore benefit without re-importing.
4. **Pre-synthesis follows the pages.**
   - `PresynthesizeProfile` emits one cache entry per page of a multi-page
     message (bare message key for single-page messages) and synthesizes each
     through the shared cache key, so a pre-synthesized page is a hit for the
     commit that speaks it.

## Tradeoffs

- **Identity:** page-scoped utterance ids (`m<ULID>#p<ordinal>`) flow through
  the cache key, the successor graph prewarm source and the catch-up runs; the
  successor edges stay message-level (ADR-0155's prewarm mapping strips the page
  suffix back to the source message).
- **Fallback duplication:** the fallback splitter duplicates the adapter's box
  packing heuristic in the runtime; a future adapter change moves the boundary
  into shared code.
- **Cache:** one pre-synthesis pass per page costs one provider request per
  page; the per-message byte quota in the prewarm options is unaffected (the
  quota counts synthesised bytes, which now count per page).

## Validation

- Adapter: `TestMessagePagesDerivesBoxes` (one page, four lines fit one box,
  five lines split, nine lines split) and the empty-message case.
- Runtime: committing a three-page message's page 2 speaks page 2's text under
  the `m<message>#p2` key while page 1's box of the same message speaks page 1's
  text under `m<message>#p1`; the page keys differ and each commit speaks only
  the reached box.
- Pre-synthesis: `TestPresynthProfileSynthesizesPerPage` — a published profile
  with a three-page message yields three pre-synthesis cache entries, one per
  page.

## Reversal

Revert the page-scoped key and the commit-time page choice; the adapter's
page derivation becomes inert because `message_pages` rows simply stop being
written, and the fallback keeps older profiles speaking full messages again.
