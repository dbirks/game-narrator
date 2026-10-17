# ADR-0035: Candidate matcher — bounded retrieval, log-linear scoring with fixed weight defaults, reason codes, and an abstain-first acceptance policy
Date: 2026-10-17
Status: accepted (retroactive)
Records decisions made in commit 4f5af9e (`internal/matcher`).
Owners: project
Related requirements/tests: docs/spec/05 "OCR pipeline", "Local candidate retrieval", "Scene belief graph", "Clef adjudication and commit policy", "Predictive playback policy", "Timings"; docs/spec/12 "Property-based and fuzz tests" (deterministic rank ordering, NaN/infinite candidate-ranking values); docs/spec/16 M2 acceptance (10k-message index build < 2 s and < 64 MiB heap growth, Rank p95 < 5 ms over 500 noisy queries); docs/spec/04 "Two-game portability verification"; builds on the `games` domain types (internal/games, textnorm folder)

## Context and constraints

The recognition pipeline (docs/spec/05) places candidate matching between OCR and the decision layer: normalization and prefix aggregation → candidate retrieval → graph-aware scoring → optional Clef adjudication → commitment policy. The matcher must run on the UNO Q appliance — four cores, 4 GB RAM — at 2–5 Hz while a textbox is relevant, on every relevant frame, against a published profile of up to ~10k messages (docs/spec/16 M1 "search 10k synthetic messages without memory blowup").

The constraints that shape the design are:

1. **Bounded work per frame.** Retrieval and scoring cost must be a function of explicit caps, not of corpus size or input length. Hostile input (a 100 KB garbage OCR read) must not cost more than a normal one.
2. **Determinism.** docs/spec/12 requires rank ordering to be stable under identical inputs and settings, because rankings feed audio cache keys, replay fixtures, and a developer UI that a human compares against a recorded trace.
3. **Numeric safety.** docs/spec/12 names "candidate ranking's NaN/infinite values" as an explicit fuzz target; a NaN OCR confidence must be read as "unknown", never propagated into a probability.
4. **Two-game portability (docs/spec/04).** No game-specific logic; everything tunable is a documented default overridable per profile.
5. **Honest abstention.** The runtime either speaks, stays silent, or asks Clef. Speaking a line the player never saw is the failure mode docs/spec/05 puts first; a local abstain that forwards a shortlist is cheap and recoverable.

This package ends at "here are the plausible messages, how much I trust each, and why". The commitment policy (docs/spec/05 "Predictive playback policy") is owned by the caller.

## Decision

`internal/matcher` implements the stage as an immutable, flat index plus a per-observation ranker, with the following settled design:

### Index (built once per published revision, shared without locking)

- Flat parallel slices (ids, keys, keyLens, scenes, kinds, editions) addressed by an `int32` ordinal, plus: an exact-key map (folded key → ordinals ascending), a sorted key list for bounded prefix scans, a trigram inverted index of FNV-1a 32-bit hashes (distinct per message, so shared counts stay set intersections), a per-message sorted unique-token hash set, and the dialogue graph as flat adjacency arrays (one byte per edge kind, `float32` confidence).
- Retrieval keys are OCR-folded and punctuation-stripped by `textnorm` (`IndexOptions{KeepPunctuation, NoOCRFold, PlayerNames, Edition}`); both sides of every comparison go through the same folder, and both the observation and the stored line are truncated to the same `maxTextRunes = 1024` bound.
- `Build` refuses empty input and duplicate/empty message IDs; unresolvable edge endpoints and empty keys are counted in `Stats` (`SkippedMessages`, `SkippedEdges`) rather than failing the build.

### Retrieval (in docs/spec/05's fixed order)

1. Exact normalized match (`exactN` counts the ambiguity).
2. Full-prefix scan, **only when the observation is marked `IsPrefix`** (a typewriter prefix), bounded by `MaxPrefixScan = 256` keys; `prefix_coverage` is the fraction of the stored line revealed.
3. Trigram top-K: shared-trigram accumulation under a total posting-visit budget `MaxPostingWork = 40000`, with a popularity cut-off that skips any trigram present in more than `max(corpus/4, 64)` messages (popular trigrams carry no discriminating power and would eat the budget). Query trigrams are sorted first so the budget is spent identically every run. Keeps the best `RetrievalK = 40`.
4. Graph-neighbor expansion from the last `MaxRecent = 10` confirmed messages, always run even when trigram retrieval is empty. Edge prior = base prior × edge confidence × `0.6^position` recency decay (clipped at position 20); base priors by edge kind: next 1.0, branch 0.7, repeat 0.5, contextual 0.35, inferred 0.2, unknown 0.
5. Global fallback (length-closeness scan, capped at `TopK`), used **only** when stages 1–4 produced nothing.

Candidates arriving by multiple routes are deduplicated into one candidate carrying all signals.

### Scoring

Nine component scores, each in [0,1] (keys exactly as docs/spec/05 names them: `text`, `prefix_coverage`, `scene`, `edge_prior`, `visibility`, `text_kind`, `speaker_context`, `edition`, `source_reliability`):

- `text = 0.5·trigramDice + 0.3·tokenSetDice + 0.2·bandedLevenshteinSimilarity` (dice over the exact shared-trigram recompute, not the retrieval count; Levenshtein banded at 12 edits over ≤128-rune prefixes — O(len·band), reported as 0 beyond the band).
- `scene`: 1 same scene, 0.5 either unknown, 0 conflict.
- `edge_prior`: the max prior over the candidate's graph entries; `visibility`: the OCR confidence with 0/NaN read as neutral 0.5.
- `speaker_context` and `source_reliability` are neutral placeholders (0.5) until speaker hypotheses and source-provenance scores are plumbed in: 0.5 contributes a constant to every candidate and cancels in softmax, so it is honest rather than optimistic.
- `edition`: 1 when the editions agree or either is unspecified, 0 on a real conflict.

The raw score is a **log-linear sum** `Σ wᵢ·log(ε + cᵢ)` with `ε = 1e-3` — a missing signal multiplies the odds down instead of subtracting a fixed amount, and the epsilon floor caps the "no evidence vs certainty" difference so weight ratios remain arithmetic. Every component and every weight passes through a sanitizer (`unit` clamp; NaN treated as no evidence; non-finite totals repaired to a finite value), so the function is total: `Rank` never returns an error and never panics; an unusable observation yields an abstaining ranking.

Default weights (`DefaultWeights`, a starting point for per-game calibration, not a tuned result): `text 1.6`, `edge_prior 0.55`, `prefix_coverage 0.5`, `scene 0.35`, `visibility 0.25`, `text_kind 0.2`, `edition 0.15`, `speaker_context 0.1`, `source_reliability 0.1`. Two ratios are deliberate and asserted by tests: text outweighs edge prior (docs/spec/04 "Never force the most likely graph edge over strong contrary OCR text"), and all context components are small — tilt, not evidence. Weights are sanitized into [0, 8]; an all-zero or non-finite set falls back to the defaults.

### Ranking, probabilities, acceptance

- Order: raw score descending, then message ordinal ascending, then ID ascending — no map order is ever observable. `TopK` defaults to 10 (max 50; the decision layer forwards at most a bounded list to Clef).
- Probabilities: softmax over the **returned** candidate set (so the list sums to 1 and thresholds are comparable across corpus sizes).
- Acceptance policy (abstain-first, `ABSTAINED` is a normal output):
  - a winner that is a pure global fallback without exact/prefix/graph support and with `text < 0.5` abstains (a single-candidate softmax would otherwise look certain);
  - winner probability < `MinProbability = 0.35` abstains;
  - margin over the runner-up < `MinMargin = 0.12` abstains;
  - low specificity abstains unless graph evidence (`edge_prior ≥ GraphEvidenceFloor = 0.15`) or a deciding scene breaks the tie: fewer than `MinSpecificChars = 5` non-space characters, a prefix observation shorter than `MinTextLenForPredict = 12` non-space characters, or more than `ExactAmbiguityMax = 2` messages sharing the exact (or prefix) key — the "Yes." spoken by four NPCs case.
- Reason codes persist what the package can actually justify: `MATCH_EXACT`, `GRAPH_NEIGHBOR`, `PREDICTIVE_PREFIX`, `MATCH_FUZZY` (a documented local extension), `ABSTAINED`. Decision-layer codes (`CLEF_DISAMBIGUATED`, `UNKNOWN_SPEAKER`, `NO_NARRATABLE_TEXT`) are deliberately absent so a local guess can never be recorded as a model-adjudicated result.
- `RankOptions` sanitize unset fields to defaults ("unset" never means "zero"): thresholds that vanish under a half-filled struct would let the runtime speak a wrong line.

## Alternatives considered

- **Weighted linear sum of components.** Rejected: it subtracts a fixed penalty for missing evidence, so one absent signal (no scene belief) would dominate the ranking regardless of how strong the text evidence is. docs/spec/05 explicitly asks for a "tunable log-linear scorer".
- **Levenshtein as the primary similarity.** Rejected: on a 30-character line one misread glyph already scores 0.97 similarity while a wholly different line sharing the opening words scores 0.6 — it barely separates the cases that matter and is the most expensive term. It is kept at weight 0.2, banded and prefix-capped, as a tie-breaker among strings that share most of their characters.
- **Carrying the retrieval shared-trigram count into scoring.** Rejected: a candidate that arrived by prefix or along a graph edge was never seen by the trigram stage; treating its evidence as zero because of the route it entered is an algorithm artifact, so scoring recomputes the intersection exactly.
- **Full graph canonization of candidate IDs (e.g. Weisfeiler–Lehman-style refinement).** Considered for the matcher's tie-breaking and rejected as unnecessary: the deterministic comparator (score, ordinal, ID) plus sorted query trigrams gives the determinism docs/spec/12 requires at a fixed cost; the residual cost is the same forward-reference limitation the profile manifest canonicalizer documents (see ADR-0036).
- **A learned calibrator instead of fixed defaults.** docs/spec/05 allows "a tunable log-linear scorer or learned calibrator"; with no calibrated evaluation data yet (docs/spec/17), conservative documented defaults plus per-profile overrides are the honest choice. Calibration is a follow-up, not a redesign.

## Consequences and risks

- The ranking is fully explainable: every candidate carries its nine component scores and a reason code, which the developer UI, event traces, and the future calibration dataset read by name (exported constants, not loose strings).
- Abstaining is the default for fragments, ambiguous phrases, and unreadable screens; this costs some coverage in favour of never speaking an unseen line. `PREDICTIVE_PREFIX` exists but requires the caller's explicit opt-in policy.
- 32-bit trigram/token hashes can collide (on the order of 1e-1 per corpus): a collision can nudge a retrieval count or a component by one unit but cannot affect an exact match, which never consults the inverted index.
- `speaker_context` and `source_reliability` are placeholders (0.5); until speaker hypotheses and source provenance are plumbed in, the ranking cannot use them. The weights for them are deliberately the smallest.
- A future component or weight that smuggles in NaN is caught by the sanitizer, not by the caller; the "unreachable" guard in `combine` is asserted by fuzz, not trusted.
- Risk: thresholds (`MinProbability`, `MinMargin`, the specificity limits) are calibrated for the default `TopK = 10` candidate set; changing `TopK` without re-checking the thresholds changes what "accept" means.

## Evidence and test plan

All in `internal/matcher` (synthetic 10k corpus via `bigCorpus`, a mini scripted corpus, and real profile values through `internal/games`):

- **Scale gates (docs/spec/16 M2), asserted only in the uninstrumented build** (`timingAsserted` is true with the plain build tag and false under `-race`; the race runtime measures the detector, not the matcher — `scale_norace_test.go`, `scale_race_test.go`):
  - `TestScaleBuildWithinBudget`: 10k-message build < 2 s and heap growth < 64 MiB (logs the measured numbers plus `Stats`).
  - `TestScaleRankLatency`: Rank p95 < 5 ms over 750 queries (500 noisy observations + 250 deliberately corrupted real lines so retrieval always finds rivals); logs p50/p95/max/mean.
  - `TestScaleNoPerQueryMemoryGrowth`: per-query allocations on the 10k corpus must not scale with the corpus (>1.5×+400 of the 2k corpus fails).
  - Skipped under `-short`; run by name uninstrumented for the real gate.
- **Determinism and invariants (property tests):** `TestRankingIsDeterministic` (same input/settings → identical ranking), `TestPropertyInvariants` (finite scores in documented ranges, probabilities sum to 1, top-K respected), `TestPropertyUniqueLineAlwaysRecoverable` (a line unique in the corpus always ranks first on its exact text).
- **Behavioural tests (`rank_test.go`):** exact hit, OCR near-miss, unique/shared prefix while revealing, prefix not reported when too short or unmarked, graph successor breaking a tie, **strong text beats edge prior** (the weight-ratio invariant), loop/repeat edges, branch selection not guessed, short-phrase holdoff ("Yes."), punctuation-only difference still matches, empty and hostile observations safe, abstain on the probability/margin thresholds, global fallback only when nothing else produced candidates, and a fallback candidate can never report itself as confident.
- **Component tests (`textutil_test.go`):** banded Levenshtein matches a full DP on bounded inputs and its cost stays bounded; trigram Dice separates like lines; the token component ignores order but not content; non-finite inputs are treated as no evidence.
- **Fuzz (`FuzzRank`):** hostile observations (control characters, invalid UTF-8, 100 KB+ strings) with the index built once outside the loop; asserts no panic, all numbers finite and in range, probabilities sum to 1, and reproducibility for identical input — the docs/spec/12 "NaN/infinite values" target.
- **Index tests (`index_test.go`):** bad input rejected with sentinel errors, skips reported without failing, stats determinism, fold agreement between stored keys and the exported `Fold` (so the developer UI's "why did it not match" uses one folder), player-name placeholder round-trip, edition-from-flags, and concurrent `Rank` safety on one shared index.

## Migration / rollback

The package is additive and self-contained: it reads `internal/games` values and exports an `Index`/`Ranking` API; nothing upstream depends on its internals. Reverting removes the package and the call site that feeds it into the pipeline runner; no stored data is affected (the index is rebuilt from the published revision on demand and carries no schema of its own). Changing the default weights or thresholds is a per-profile configuration change via `RankOptions`/`IndexOptions`, not a migration.

## Follow-ups

- Calibrate the weight ratios and the acceptance thresholds against human-corrected evaluation data when it exists (docs/spec/05 "human-corrected evaluation data calibrates acceptance"; docs/spec/17), and record any change with an ADR and the measured delta.
- Plumb a speaker hypothesis into `speaker_context` (today a 0.5 placeholder) once the decision layer's speaker questions land.
- Add a source-reliability signal to `source_reliability` when the schema carries provenance scores (today a 0.5 placeholder).
- Validate the scale numbers on UNO Q hardware (docs/spec/12 "Benchmark CPU/RSS/temp on UNO Q"): the dev-host numbers logged by the scale tests are an upper bound, not the acceptance itself.
- ADR-0036 documents the sibling design decision (profile manifest canonicalization); the matcher's forward-reference note above and that ADR's limitation are the same class of choice, stated per component.
