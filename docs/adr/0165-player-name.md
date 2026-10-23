# ADR 0165: Speak the player's name, not the placeholder

- Status: accepted
- Date: 2026-10-22
- Issue: #48

## Context and constraints

The matcher folds every configured player name onto textnorm's placeholder
before indexing (docs/spec/02, `internal/games/textnorm`), so a line whose
script text carries the player's name is findable from the OCR text that still
shows the placeholder. What the runtime's speech text never did was resolve
that placeholder onto a name: the narrator spoke the literal `{player}` (ADR-0162's
"First findings": "`{player}` reached TTS as written and was spoken 'player'").

A session therefore cannot speak the player as the player until it has learned
the name — from the OCR reads of the lines that carry the slot — and until the
speech text it hands to the TTS stage is the substituted text.

## Decision

1. **One resolution seam.** `resolvePlayerPlaceholder(text, name)` resolves a
   spoken text onto the player name: with a name, replace the `{player}` slot;
   without one, drop the slot and tidy the punctuation the drop orphans
   (`"Hurry, {player}!"` → `"Hurry!"`, `"{player}, wait."` → `"Wait."`,
   `"It's you, {player}."` → `"It's you."`).
2. **The commit path speaks the learned name.** On a matched commit whose
   canonical text contains the placeholder, the session learns from the OCR
   read (`d.RawText`) the candidate word at the slot's position in the script
   text (the words before the placeholder count the words before the name), and
   trusts a candidate after two agreeing reads. The matched line's speech text
   is resolved with the learned name; while the session has not trusted one, it
   speaks the dropped form.
3. **The cache key follows the substituted text.** The resolved text is the
   utterance's speech text, so the key the prewarm, the session prewarm and the
   profile pre-synthesis compute is the key the substituted text implies — the
   live commit (learned name) and the profile pre-synthesis (dropped form, it
   has no session to have learned) agree through the same key computation once the
   slot is resolved before the key materialization.

## Options considered

- **Speak the literal placeholder.** Rejected: it is the defect (ADR-0162).
- **Resolve to a configured profile name at import time.** Rejected: the
  narrator's name is the player's own, learned per session from the board's
  reads, not a curator constant.
- **Resolve per utterance with an ad-hoc string replace.** Rejected: the
  punctuation tidy of the dropped form is its own contract and is unit-tested.

## Tradeoffs

- The dropped and the named forms are different cache keys: a session that has
  not learned a name prewarms and speaks the dropped form; a session that has
  learned speaks the named form and synthesizes under it.
- The learning trusts two agreeing reads of one slot; one read is an OCR guess
  and never enough.

## Validation

- `go test -race -timeout 25m ./internal/runtime/` (new: `TestResolvePlayerPlaceholder`,
  `TestPlayerNameCandidate`, `TestValidPlayerName`, `TestLearnPlayerName`).
- `systemd-run --user --scope -q -p MemoryMax=4G make test-replay` (junit line).
- `make lint`.

## Reversal plan

Drop the resolution and the learning; the placeholder flows to the TTS stage
as the script wrote it.

## Consequences

- The narrator speaks the player's name once the session has trusted one, and
  stays silent-free (dropped form) before that.
- The pre-synthesis keeps the dropped form; the live session resolves with the
  learned name, and the key identity is the substituted text's.

## Links / references

- ADR-0023 (speech cache), ADR-0039 (runtime speech pipeline), ADR-0162 (the
  literal-placeholder finding), ADR-0155 (page-scoped keys), ADR-0153 (one key
  identity).
