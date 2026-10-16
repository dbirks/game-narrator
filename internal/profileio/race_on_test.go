//go:build race

package profileio

// raceEnabled tells the scale test that it is running under the race detector.
//
// internal/store's driver is modernc.org/sqlite (ADR-0008), which is pure Go, so
// race instrumentation applies to the whole SQLite interpreter — and the FTS5
// trigger path that indexes message text is exactly the kind of tight byte
// pushing that slows down by an order of magnitude. Measured on the dev host, the
// same 10 000-message import takes ~4s uninstrumented and ~100s instrumented.
//
// That makes two separate claims that must not be conflated (AGENTS.md "no
// simulation theater": reports record their own stage origins):
//
//   - "the profile store has no data race" — needs -race, and a smaller corpus
//     proves the same thing about the same code paths;
//   - "searching 10k messages answers in under 50ms at p95 without growing the
//     heap" — a *production-performance* claim, which instrumented numbers cannot
//     support in either direction.
//
// So the corpus shrinks under -race and the latency assertion is reported as
// NOT RUN there rather than being quietly kept (it would fail) or quietly dropped
// (it would be a lie).
const raceEnabled = true
