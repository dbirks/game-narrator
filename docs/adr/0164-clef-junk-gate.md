# ADR 0164: The clef-junk-gate — never ask Clef about a read that is not dialogue

- Status: accepted (implemented; board validation still pending)
- Date: 2026-10-22
- Issue: #44 (follow-up)

## Context and constraints

The 2026-10-22 UNO Q board session (about 3 h, 2,637 Clef decision requests)
showed the decision traffic the optional Clef adjudication (ADR-0050) was built
to carry had been burning its whole budget on junk reads: 2,586 of the 2,637
requests (98%) went out on OCR reads shorter than 12 characters, the noise reads
of scenes with no dialogue box on screen (HUD icons, rocks, the crop landing on
scenery). The gateway answered 768 of them with `bad_schema` and 15 with
`rate_limited`, and the decision role flapped degraded/recovered 768 times over
the session. Nothing wrong was spoken: Clef answered `none` and the commit held
on the local evidence — the cost was spend, flap and latency, not a wrong
utterance.

The ask fired from the pipeline's rank worker
(`internal/pipeline/runner.go`, `rankWorker`) whenever
`session.Config.NeedsAdjudication(rank)` was true (an abstaining or ambiguous
local ranking) and a Decider was wired — before the commit for that observation.

## Decision

1. **Before the adjudication goes out, the read is screened for
   dialogue-ness (the clef-junk-gate).** A read with fewer than two words of
   three or more letters (Unicode letters) is not dialogue-like and is not
   offered to the Decider.
2. **The skip rides the abstain's handling.** No `Decide` call is made; the
   reply stays the zero `DecisionReply` with reason `"skipped_junk"`; the commit
   proceeds on the local evidence exactly as it would for a local abstain.
3. **The skip is counted.** `Stats.DecisionsSkippedJunk` (`decisions_skipped_junk`)
   books the gate's skips; `Stats.Decisions` keeps counting only the offers that
   actually went out.

A real box whose OCR is garbled still usually draws two words the letters can
hold, so the gate does not swallow those — the full-frame Clef rescue
(ADR-0154) keeps its footing for the garbled-but-real reads.

## Options considered

1. **Skip reads under a raw character length** (the board's "shorter than 12
   characters" observation). Rejected as the criterion: length alone is not
   dialogue-ness; a genuinely short real line would be dropped with the junk.
2. **Reuse the read-through plausibility gate** (`internal/runtime`,
   ADR-0136 + its amendment). Rejected as the home of this gate for now: it is
   the speech side's gate for unmatched read-throughs, and this is the decision
   side's gate for what gets asked; the two could share a predicate later, but
   the decision-side gate is narrower than the speech-side gate (the decision
   must not ask about a read the local policy already abstained on for junk, and
   the speech side must still bound its own reads).
3. **Ask Clef anyway, let the answer be `none`.** The status quo: rejected — the
   spend and the flap were measured, and the answer was never the defect.

## Consequences

- The board's decision traffic no longer spends gateway calls on screens with no
  dialogue box, and the degraded/recovered flap cannot recur on junk reads.
- The gate is a pure text predicate (`readIsDialogueLike` in the pipeline), so
  the behaviour is deterministic and unit-reproducible without a provider.
- The reply the skip synthesises (`"skipped_junk"`) flows through the ordinary
  abstain path, so no decision-path ordering, fence or event changes were needed
  beyond the counter.

## Validation

- `go test -race -timeout 20m ./internal/pipeline/`
- `systemd-run --user --scope -q -p MemoryMax=4G make test-replay` (junit line).
- `make lint`.

## Reversal plan

Lift the two-word dialogue-ness floor or move the predicate to the shared
read-through gate; the skip then becomes an operator-tunable decision-side
setting. The abstain handling the skip rides is unchanged and stays as the
fallback.

## Links / references

- ADR-0050 (Clef adjudication and commit policy), ADR-0115 (escalation),
  ADR-0136 (read-through plausibility), ADR-0154 (full-frame Clef decision),
  docs/spec/05 (Clef adjudication and commit policy), docs/spec/13
  (observability and resilience).
