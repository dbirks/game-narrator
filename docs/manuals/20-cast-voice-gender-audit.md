# Cast voice-gender audit

## Purpose

Find and fix characters whose voice does not match their gender/age — a
female character spoken by a low adult-male voice, a child by an adult female
voice. The audit measures each assigned voice's reference with the pitch
estimator and compares the measured class (low adult male / adult female /
child-high) with the class the character's profile data expects (ADR-0171).

## Preconditions

- The appliance is running (or a `--data-dir` is available for a one-shot
  run).
- The voice design roles are configured for `--fix` (the same roles the voice
  panel needs; `game-narrator doctor` shows them). The audit itself is
  read-only and needs no model.

## Steps

1. **See what is wrong.** For one profile:
   `game-narrator cast audit --profile <id>`
   Each roster row shows expected class, measured F0 (Hz), measured class,
   and verdict: `OK`, `MISMATCH`, or `UNKNOWN` (no gender words in the
   profile data — not a failure). `--json` for machine-readable output.
   `UNKNOWN` rows mean the profile text and the voice's `speaker_kind` carry
   no gender/age word: add the descriptor to the character's description or
   voice brief, then re-run.
   The doctor reports the same, appliance-wide, read-only:
   `game-narrator doctor` → the `cast.voice_gender` row.
2. **Dry-run the fix.** `game-narrator cast audit --profile <id> --fix`
   prints every MISMATCH and what it would redesign; nothing is written.
3. **Apply the fix.** `game-narrator cast audit --profile <id> --fix --apply`
   re-designs each mismatched voice through the normal voice-design path
   with a brief that states the expected gender/age explicitly, measures the
   new take's pitch, and retries up to two more times while it still reads
   the wrong class. The new take is left **PENDING** — review it in the
   voice panel (or the Cast page's "Voice check" badge) and approve it like
   any other take. Add `--approve` to approve automatically (the profile's
   default consent is attributed).
4. **Idempotent re-runs.** Re-running `--fix --apply` re-measures the
   existing pending fix take instead of opening another draft; only a
   still-wrong take triggers more design.

## Verification

- Re-run the audit: the fixed character reports `OK` with the take's
  measured F0 once the take is approved (the audit measures the voice the
  runtime actually speaks, so an unapproved fix still shows the old
  mismatch).
- The doctor row `cast.voice_gender` reads `ok` when no mismatch remains.
- Exit codes: `0` when nothing is wrong, `1` while mismatches remain —
  scriptable.

## What NOT to do

- Do not edit the reference WAV of an approved voice to make the audit pass:
  an approved reference is frozen (ADR-0005); re-design the voice instead.
- Do not run `--approve` in bulk on a roster you have not heard: a pending
  take is the review step, and the review is the point.
- Do not treat `UNKNOWN` as an error to clear by adding gender words: if a
  character genuinely has no stated gender, `UNKNOWN` is the honest verdict.
