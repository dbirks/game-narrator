# ADR-0184 — Page-aware local matching: score a message against each of its boxes

- Status: accepted
- Date: 2026-10-24
- Slug: page-aware-match
- Evidence source: UNO Q board, real GameCube, build 3f91310 (2026-10-24)

## Context

An NPC's message spans two display boxes (pages; ADR-0155, `message_pages`,
`<message id>#p<n>`). The player was on page 2. OCR read the on-screen box
faithfully except for one misread word. The local matcher shortlisted the right
message first, but its text component scored **0.44**: the single box on screen
was compared against the *whole two-page message text*, so it abstained
(`COMMITTED_ABSTAINED`). The decision role (Clef) was then offered the whole
message text as the candidate description and answered `choice: none` — the
candidate text did not equal the box on screen. The line was read through as
unmatched with the OCR's misread word, instead of the script's page-2 text in
the character's voice.

Nothing in `internal/matcher`, `internal/pipeline` or the systemone request
builder knew about pages; only speech did (`boxPagesFromOriginal`/`choosePage`,
ADR-0155 amendment).

## Options

1. **Re-derive pages per candidate in the matcher** (read the original text,
   split at box breaks per frame). Rejected: duplicates the derivation, costs
   per frame, and risks the matcher and speech deriving *different* pages.
2. **Score the observation against each stored page, precomputed at index
   build** (chosen). The session already derives pages for the speech wiring
   (`profilePages`); the same map is handed to `matcher.Build`
   (`IndexOptions.Pages`). Page retrieval keys, trigram counts and token sets
   are folded once at build time — the per-frame path only scores against
   stored keys, exactly like the whole-text path.
3. **Leave matching whole-text, only fix the Clef request** (send the page
   text as the candidate). Rejected: the local matcher would keep abstaining,
   so the line would still depend on the model round trip and on Clef's
   agreement that the page equals the box; the local commit is the fast, free,
   deterministic path.

## Decision

1. **Matcher** (`internal/matcher`): `IndexOptions.Pages` carries message id →
   page texts (the same derivation speech uses — `profilePages`, ADR-0155 —
   precomputed by the caller at session start, never per frame). Messages with
   two or more non-empty pages get flat page rows (folded key, trigram count,
   token set) at `Build` time. `scoreOne` scores the observation against each
   page with the same character/token/Levenshtein blend and the same fragment
   containment floor as the whole-text term (containment scaled by the read's
   share of the *page*), and the best reading wins the text component. The
   winner is recorded on the candidate as `Page` (1-based ordinal, 0 = whole
   text won or page-less message) and `PageTextScore`, and in
   `Components["page"]` / `Components["page_text_score"]` only when a page
   actually won, so a page-less corpus keeps the historical component set —
   the component-count invariants (property/fuzz tests) are untouched. The
   fragment ambiguity counter (`fragN`) counts a candidate at most once
   whether its evidence is whole-key or page.
2. **Clef candidate text** (`internal/runtime` decision wiring, the systemone
   request builder): the candidate option carries the best-matching page's
   text when the ranking scored one (`optionText`), else the whole text; the
   candidate question's instructions note that a message may be displayed one
   box (page) at a time, so the model compares the box on screen against the
   option that is it.
3. **Commit payload** (`internal/session` commit payload, `internal/runtime`
   speech wiring): the committed payload carries the matched page
   (`page` key from the committed candidate's `Page`), and `onCommit` gives it
   precedence over `choosePage`'s re-derivation from the OCR read when both
   exist. `choosePage` remains the fallback for commits without a recorded
   page (page-less messages, older payloads) and the matcher's page is the
   commitment's own comparison — one misread word is enough to defeat a
   containment re-derivation.

## Tradeoffs

- The index stores extra rows only for multi-page messages (a few percent of a
  profile; page texts are short), at build time only. Scoring cost per
  candidate grows by the page count for multi-page candidates in the pool —
  bounded (TopK candidates, boxes are at most four lines) and off the path
  entirely for page-less corpora (the pre-change behaviour, bit for bit).
- A read that spans the message boundary (box edge mid-page) falls back to the
  whole-text score, which the fragment floor already handles — the page never
  suppresses evidence, it only adds it.
- The decision request's option text now differs from the whole canonical text
  for multi-box lines; the page text is the same speech text prewarm keys are
  computed from, so the cache identity of the answer is unaffected.

## Validation

- `internal/matcher` unit tests (`page_test.go`, invented strings): a two-page
  message read on page 2 with two misread characters commits at text ≥ 0.8
  with `page = 2` recorded; without page rows the same read stays under-scored
  (documents the pre-fix shape); a page-1 read records `page = 1`.
- `internal/runtime` tests (`decision_pages_internal_test.go`): the candidate
  option is the page text, the fallbacks (no page, out-of-range page,
  page-less message) keep the whole text, the candidate question carries the
  box-at-a-time note, and a commit that names page 2 with an unplaceable junk
  OCR read speaks exactly page 2 under `#p2`.
- `make test-replay` (junit all-pass), race tests of `internal/matcher`,
  `internal/runtime`, `internal/providers/systemone`, `make lint`, and the
  Dolphin deck conversation before/after comparison (see REPORT.md).

## Consequences

- `IndexOptions.Pages` is a new, optional input to `matcher.Build`; callers
  that do not pass it (bench, dev actions, oneshot) keep the whole-text-only
  scoring. The runtime session is the production caller and always passes
  `profilePages(content)`.
- `message.committed` payloads now carry a `page` key (0 when absent from the
  evidence); the developer UI can show which box the match landed on.

## Reversal plan

Drop `IndexOptions.Pages` and the page scoring block in `scoreOne` (the
whole-text path is untouched and remains the fallback), delete
`optionText`/`noteBoxDisplay` (the `textFor` path stays), and remove the
`Page` field from the commit payload — every consumer degrades to the
historical whole-message behaviour, which is a performance/recall loss, not a
correctness change: speech's `choosePage` re-derivation still speaks the
reached page for committed lines.

## Amendment 1 (2026-10-24): box breaks around control tags

First board run on the page-aware build (hardware bundle, real GameCube):
a two-box line read on page 2 still scored against the whole message
(text 0.45, no `page` component, `COMMITTED_ABSTAINED`). Its first box ends
in the tag for the player's name, so the box's last display "word" was a
bare "?" that aligned to nothing, the original-text split returned nil, and
sentence packing made one page. The split now aligns on the box's last real
word, folds `{…}`/`[…]` markup out of canonical tokens, lets a box that ends
in a tag (or has no word at all, e.g. "...") take the placeholder and
punctuation tokens that follow, and keeps those tokens off a box that does
not. On the real profile (4331 messages, 1166 multi-box) unaligned
multi-box messages fall from 96 to 1 (a shop total built only of tags).
Tests: `TestBoxPagesFromOriginalTagEndsABox`,
`TestBoxPagesFromOriginalWordlessBoxes` (invented strings).
