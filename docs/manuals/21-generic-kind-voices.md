# Generic kind voices (unnamed speakers)

## Purpose

Give the unnamed speakers a voice per apparent kind. When Clef cannot name a
speaker but is confident of the speaker's kind — an adult man, a child, a
creature — the line is spoken with the voice approved under the generic key
`kind:adult_male`, `kind:boy`, and so on (ADR-0158). A kind with no approved
voice falls back to the narrator; this page's work is how the seven kinds get
their voices in one click.

## Preconditions

- The appliance is running with the voice workflow configured
  (voice design/clone roles; `game-narrator doctor` shows them). Without it
  the Cast page hides the section rather than guessing.

## Steps

1. **Open the Cast page** of any edition
   (game → overview → the edition → *Cast*). Below the roster is the
   **Generic kind voices** section: one row per kind (`adult male`,
   `adult female`, `elder male`, `elder female`, `boy`, `girl`, `creature`)
   with its state — `missing`, `pending` (a take waiting on your approval),
   or `approved` — and, once a take exists, its measured median F0 (Hz) and
   pitch class (the cast audit's classifier, ADR-0171: low adult male /
   adult female / child).
2. **Create the missing ones.** **Create missing (n)** auditions each
   missing kind with its fixed brief, measures the take's pitch, and retries
   up to two more times while the measured class does not match the kind —
   the retry brief restates the expected pitch more forcefully. The creature
   has no pitch class to match; its take is kept as is.
3. **Review and approve.** Passing takes are left **pending**: open the
   kind's take (the voice workflow's approve flow — `GET /api/v1/voices`
   lists the `kind:*` revisions, the approve and register endpoints are the
   ordinary ones) and approve it. A take that is still out of class after
   three attempts is flagged on the row (`the pitch stayed out of class
   after 3 attempts`) — it is never auto-approved; re-run or hand-audition
   with your own brief (ADR-0158).

## What the run will not do

- Approve anything (AGENTS.md rule 6: voice selection is reviewable).
- Touch a kind that already has an approved voice or a pending take.
- Fail the page: per-kind problems (a refused design call, an unreadable
  take) are reported on the row's note, not as a page error.
