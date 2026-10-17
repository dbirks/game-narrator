// Package windwaker is the Wind Waker reference adapter (docs/spec/04
// "Wind Waker reference adapter"): it reads the game's BMG message archive —
// from a user-supplied GameCube disc image (.gcm/.ciso) or a raw .bmg byte
// stream — and turns it into the canonical games.Message rows the importer
// writes.
//
// This package is the only place this game's knowledge may live. docs/spec/04's
// portability rule forbids Zelda-specific logic in the core runtime, the parser
// plumbing or the UI; the bmg package under internal/games stays
// game-agnostic and this adapter is where "Wind Waker" is allowed to mean
// something (INF1 attribute use, tag semantics, region/edition identity).
//
// The layout:
//
//   - adapter.go: the adapter.Adapter surface (manifest, detection, entry
//     points);
//   - parse.go: BMG file → ParseResult mapping (rows, counts, graph note);
//   - escape.go: the original_text escape grammar and its exact inverse;
//   - render.go: the token AST (markup_json) and the normalized/speech
//     renderings;
//   - rules.go: the versioned SpeechRenderingRules table (data, not code);
//   - graph.go: the flow-graph decision (no edges unless FLW1 decodes — and
//     it does not, so no edges, and the coverage note says why).
//
// Game text never leaves the user's machine in a form that can be committed:
// tests assert counts and ratios only, and this package's output is written to
// the local store and asset store (docs/spec/14, AGENTS.md rule 5).
package windwaker
