// Package matcher turns one textbox observation into a ranked list of canonical
// script messages.
//
// It sits between OCR and the decision layer in the pipeline docs/spec/05
// describes: normalization and prefix aggregation → candidate retrieval →
// graph-aware scoring → optional Clef adjudication → commitment policy. The
// commitment policy is *not* here: this package's job ends at "these are the
// plausible messages, this is how much I trust each, and here is why", because
// the caller owns the risk decision about speaking (docs/spec/05 "Predictive
// playback policy").
//
// Three properties are load-bearing rather than nice-to-have:
//
//   - Bounded work. The runtime is a four-core, 4 GB ARM appliance and matching
//     runs on every relevant frame at 2–5 Hz (docs/spec/05 "Timings"). Every
//     traversal — posting lists, prefix scans, edit distance, candidate
//     expansion — has an explicit cap, and hostile input (100 KB of garbage) is
//     truncated before it is indexed against. Retrieval cost is a function of
//     the caps, not of the corpus.
//
//   - Determinism. docs/spec/12 "Property-based and fuzz tests" requires rank
//     ordering to be stable under identical inputs and settings, because a
//     ranking feeds audio cache keys, replay fixtures and a developer UI that a
//     human compares against a recorded trace. So no map order is ever
//     observable: query trigrams are sorted before traversal, and the final
//     tie-break is message ordinal then message ID.
//
//   - Numeric safety. docs/spec/12 explicitly names "candidate ranking's
//     NaN/infinite values" as a fuzz target. Every component and every weight
//     passes through a sanitizer on the way into the log-linear sum, and no
//     caller-supplied float is trusted: a NaN OCR confidence is read as "unknown",
//     not propagated into a probability.
//
// There is no game-specific logic in this package, and there must never be
// (docs/spec/04 "Two-game portability verification"): the index is built from
// internal/games values, and every tuning constant here is a documented default
// a profile can override through Options.

package matcher
