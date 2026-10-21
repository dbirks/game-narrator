# ADR-0095: Successor speech prewarming (FR-11, gap G15)

Date: 2026-10-20
Status: accepted
Owners: agent worker (task g15-prewarm); owner (review)
Related: ADR-0039 (runtime speech pipeline; the cache-key identity and
singleflight the prewarm reuses), ADR-0057 (fence cancels the read at the
source — the same mechanism a batch cancel uses), ADR-0092 (speakable-only
fence; idempotent terminals — prewarming hooks the speakable commit),
`docs/spec/07-voice-audio.md` (FR-11 "Prioritize current committed > likely
next > batch background … Prewarm top weighted successor messages but cap by
scenes/byte quota, graph entropy and resource budget"),
`docs/spec/13` (FR-11 prewarm decision counters),
`docs/status/gap-audit-2026-10-19.md` (row G15),
`internal/speech/limiter.go` (PriorityBackground), `internal/runtime/speech.go`
(`speechWiring.prewarm`), `internal/runtime/speech_fence_test.go`,
`internal/speech/prewarm_test.go`, `internal/matcher` (`Index.Successors`)

## Context

Gap G15 (audit 2026-10-19): the scheduler reserves
`PriorityBackground = "batch prewarming of successor messages"` and the
router keeps a background slot, but nothing enqueues prewarm work — every
first occurrence of a line pays the full TTS round trip before it is
audible. Spec 07's priority order `current committed > likely next > batch
background` is therefore only half-implemented: the priority classes exist,
the "likely next" class is empty.

The building blocks were already in place:

- **Successor weights** — the predictive tracker and the dialogue graph
  (`internal/matcher`) rank a committed line's weighted successors
  (`Index.Successors(messageID, limit)`); the same ranking the decision
  layer uses for "likely next".
- **Key identity** — ADR-0039's cache key is a function of (canonical text,
  message id, voice revision fingerprint, TTS model + backend identity,
  format), so a background synthesis of "line b" lands on exactly the key a
  later commit of "line b" reads.
- **Priority admission** — the speech limiter admits committed speech
  (`PriorityCurrent`) ahead of background jobs, and singleflight dedupes per
  key, so a commit that races a prewarm for the same key shares one
  generation.
- **Source cancellation** — ADR-0057's fence cancels a synthesis read at the
  source; a batch-context cancel does the same for prewarmed work.

## Decision

After each **speakable** commit (the same gate ADR-0092 fences on), the
session's speech wiring enqueues TTS for the top-k (default 3) weighted
successors of the committed line:

- **Priority.** Every prewarm job runs at `PriorityBackground` through the
  shared cache/limiter, so committed speech admits ahead of it and never
  queues behind a prewarm.
- **Same key.** The job synthesises under the identical versioned cache key
  the commit path would use (same speaker resolution, canonical text,
  voice, backend identity). A later commit of the line resolves from the
  cache (`from_cache: true`) instead of paying the round trip.
- **Entropy cap.** The Shannon entropy (bits) of the batch's successor
  weight distribution must be ≤ the cap (default 1.5 bits: a confident next
  line ≈ 0 and a two-way fork ≈ 1.0 prewarm; a flat multi-way fork is
  refused). Prewarming a coin flip would spend the TTS budget on lines that
  probably will not be committed.
- **Byte quota.** Prewarmed bytes are booked per session against a quota
  (default 4 MiB); a commit whose batch would exceed it prewarms nothing —
  the lines then pay the round trip on commit, which is the documented
  degradation, not a failure. The quota is the practical reading of the
  spec's "cap by scenes": one session's scene-length of audio, not the
  whole dialogue tree.
- **Job deadline** (default 10 s) bounds one prewarm's synthesis so a slow
  provider cannot hold the background slot.
- **Cancellation.** A new speakable commit replaces the batch (the fence
  just moved; the old successors are no longer the likely next), and a
  scene change cancels it through the commit watcher. In-flight
  syntheses stop at the source via the batch context. Audio that already
  committed to the cache stays — a hit later is still a hit; only a key that
  reaches no commit is wasted.
- **Stats.** Per session: `prewarm.requested` (jobs enqueued),
  `prewarm.hit` (a commit resolved a prewarmed key), `prewarm.wasted`
  (prewarmed keys that reached no commit), `prewarm.bytes` (committed
  prewarmed bytes), `prewarm.skipped` (batches the entropy or quota gate
  refused). Exposed on the session's audio snapshot
  (`AudioInfo.Prewarm`, JSON `prewarm`) per spec 13's FR-11 counters.

## Options considered

1. **Enqueue from the commit path in the speech wiring (chosen).** The
   fence, the voice/speaker resolution, the key identity and the batch
   lifecycle all live in the wiring; the batch's lifetime is the fence's
   lifetime, and cancellation falls out of the existing fence machinery.
   No new component, no new queue.
2. **A manager-level background service polling the graph.** A separate
   owner would need its own fence/scene-change channel, its own voice
   resolution and its own key computation — duplicating the wiring's state
   and adding a race surface for no benefit.
3. **Prewarm the whole scene at scene start.** Heavier by construction
   (scene-size, not next-line-size), spends budget on branches the entropy
   gate exists to refuse, and still needs per-commit refresh once the fence
   moves. The per-commit top-k batch under a session byte quota delivers the
   spec's "likely next" without the scene-wide spend.

## Tradeoffs

- **Wasted spend is real and bounded.** Every prewarmed key that never
  commits is a TTS call that bought nothing; the entropy cap, the byte
  quota and the top-k bound the exposure, and `prewarm.wasted`/`prewarm.bytes`
  make it visible per session.
- **Background load under a slow provider.** A prewarm holds a limiter slot
  only until the job deadline; committed speech always admits ahead, and
  the existing `sem` backpressure on the commit path is unchanged (a
  prewarm never takes a commit's slot).
- **The counters are session-scoped, not a global time series** — the same
  standing limitation as the other FR-11 counters (see G17 for the metrics
  surface gap).

## Validation

- `internal/runtime/speech_fence_test.go` (scripted graph via
  `matcher.Build`, white-box wiring):
  - `TestSpeechPrewarmNextLineFromCache` — commit of line a prewarms b and c;
    the later commit of b resolves `from_cache: true` with the hit booked on
    the session's prewarm stats.
  - `TestSpeechPrewarmCancelledByNewCommit` — a new commit cancels the
    in-flight batch at the source.
  - `TestSpeechPrewarmByteQuotaSkipsBatch` — a quota below one tone of audio
    refuses the second batch (`skipped 1`); the refused line pays the round
    trip (`from_cache: false`).
  - `TestSpeechPrewarmEntropySkipsBatch` — three equal-weight successors
    (entropy log2(3) ≈ 1.585 bits > 1.5) refuse the whole batch.
- `internal/speech/prewarm_test.go` (cache/limiter boundary):
  - `TestBackgroundYieldsToCommitted` — with the generation pool saturated,
    a committed line admits ahead of a queued background prewarm.
  - `TestBackgroundPrewarmCancelsAtSource` — a cancelled background prewarm
    returns the context error and leaves no cache entry.
- `gofmt`, `go vet`, `go test -race -count=1 ./internal/speech/...
  ./internal/runtime/...` green; `make test-replay` green (stale and
  duplicate speech at 0).

## Consequences

- The first occurrence of a committed line's likely successor is audibly
  faster (cache hit, no provider round trip) — the spec's "p95 < 800 ms from
  committed text for cached lines" target becomes reachable for the lines
  the graph actually ranks likely.
- The dev API's audio snapshot gains the `prewarm` counter block.
- The `Cache` decision counters (`internal/speech/cache.go`) became
  atomically read/written so a live `Stats()` read (diagnostics snapshot,
  session wait loops) never data-races an in-flight generation; the
  lock-free diagnostic intent of the counters is preserved.

## Reversal plan

Prewarming is additive: the wiring prewarms only when a dialogue graph was
handed to it (`idx == nil` disables it entirely, e.g. a profile without a
graph), and the only behavioural change to existing paths is the
atomically-read cache counters. Reversal is a revert of the prewarm hunk in
`internal/runtime/speech.go` (the `prewarm`/`prewarmWorker` batch, the
`PrewarmOptions`/`PrewarmStats` types and the `Prewarm` snapshot field) plus
the test files; the cache-counter change stands on its own (it only removes
a data race) and can stay or go independently. No schema, protocol or
persistence surface is touched, so no migration is required.

## Amendment (2026-10-21): canonical text on matched commits, wider defaults, and speculative prewarm

Status: accepted. Owners: agent worker (task presynth). Builds on the
composition above; the key-identity, priority, cancellation and stats
decisions are unchanged.

**Canonical text on matched commits (the key-mismatch fix).** The commit
payload's `text` is the recognition tracker's output — the comparison fold
(lowercased), a prefix in predictive mode, a typo in a fuzzy match — while
the prewarm synthesised the profile's canonical text under
`speechWiring.prewarmTexts` (`Manager.Start`). A matched line whose commit
computed its TTS key from the folded text therefore missed every prewarmed
byte (the keys differ in case/length), and a predictive commit spoke the
prefix instead of the full line. The wiring's commit path now resolves the
line's TTS text from the canonical prewarm text when the message id has one
(`onCommit`, after the eligibility gate): the commit speaks the canonical
text under the prewarmed key, so the first occurrence of a prewarmed line
resolves from the cache. The unmatched read-through is untouched by design
(ADR-0136: it speaks the screen's text, and an abstained line has no
canonical text in the corpus). `Manager.Start`'s `prewarmTexts` precedence
is the speech text first (what the narrator says, when the profile gives
one), then the original text, then the normalized key — the TTS text of the
line, not the decision wiring's render precedence (which displays the
canonical original and keeps its own order).

**Default retune (wider forks, longer scenes).** `DefaultPrewarmTopK`
3 → 5, `DefaultPrewarmMaxEntropyBits` 1.5 → 2.0 bits (a flat four-way fork
reads exactly 2.0 and prewarms; a flat five-way coin flip, ≈ 2.32 bits,
still refuses), `DefaultPrewarmByteQuota` 4 → 8 MiB (≈ 4 minutes of
16 kHz/16-bit mono speech): dialogue forks and longer scenes stay warm. The
exposure tradeoff (wasted prewarmed bytes) is the same, bounded by the
gates and visible in `prewarm.wasted`/`prewarm.bytes`.

**Speculative prewarm (`PrewarmCandidate` / `PrewarmMessages`).** The wiring
gains two entry points that warm a profile message (or a list of scene
lines) into the shared cache before anything commits it, for callers that
see likely lines before the commit path does (a decision-layer candidate,
an operator's scene list). The job runs the same `prewarmWorker` — same key
identity, `PriorityBackground`, one synthesis per key (a list that overlaps
a batch or itself dedupes on the shared booking) — under the session's end
context rather than a commit batch's: the candidate belongs to no batch,
nothing fences it, and its bytes are still a hit if the line commits. Safe
to call from any goroutine; a nil wiring, an empty or `"unknown"` id, a
closed session, and a message without a canonical text are no-ops.

**Validation.** `internal/runtime/speech_fence_test.go`: `TestSpeechPrewarmCanonicalTextMatchesLowercaseCommit`
(a folded lowercased-prefix commit of the prewarmed line resolves
`from_cache: true` with the prewarm hit booked, and `speech.requested`
carries the full canonical text — the test fails on the pre-fix key
mismatch), `TestSpeechPrewarmCandidateSpeculative` (a speculative candidate
warms before any commit and the commit resolves it; the no-op and
dedupe halves), and `TestSpeechPrewarmEntropySkipsBatch` re-scripted to a
flat five-way fork (≈ 2.32 bits > the 2.0-bit cap). `internal/runtime/
speech_test.go`: `TestSpeechCommitSpeaksCanonicalText` — the full pipeline
(the production `Manager.Start` path): the second line's commit carries the
tracker's folded text while its `speech.requested` carries the canonical
line, and the run makes exactly one TTS call per line (the canonical key
is shared by the prewarm and the commit). `go test -race -count=1
./internal/runtime/... ./internal/speech/...` green.
