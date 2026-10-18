# ADR-0055: Live decision session test against the System One gateway

Date: 2026-10-18
Status: accepted
Owners: agent worker
Related requirements/tests: docs/spec/05 (candidate retrieval → graph-aware
scoring → optional Clef adjudication → commitment policy), docs/spec/06
(timeout and cancellation defaults; failure taxonomy), docs/spec/16 (testing
strategies: "real decision tests when configured"; the live provider
contract gate), ADR-0048 (replay pacing and the live clip convention),
ADR-0050 (Clef adjudication in the pipeline: fences, ≤1 in-flight,
reply-time observation seq, decision events), `TestLiveDecision`
(internal/runtime), `TestDecisionProviderDownCommitsOnLocalEvidence` /
`TestDecisionDeadlineIsTheRoleTimeout` (internal/runtime), `make test-models`

## Context and constraints

ADR-0050 put the Clef adjudication into the pipeline: the rank worker asks
the Decider when the local scoring cannot settle the line, at most one
adjudication is in flight per session, the answer is fenced at reply time
(stale observation/text/generation), and the session publishes
`decision.requested` / `decision.resolved` for the developer UI's
frame → OCR → candidates → Clef request → commit chain. The runtime wired
the decision role behind the provider router (`runtime.DecisionOptions`,
`Manager`'s `Decision` field), and `cmd/game-narrator` builds the same
wiring from the decision role's environment.

What was missing was the live gate. Every decision test ran against
`fakeClef`, a synthetic in-process endpoint — proof of the wiring's
semantics, but no proof that the deployed System One / Clef-Omni endpoint
answers and that its answer drives a commit and audio through the real
session. docs/spec/16 names the gate: "real decision tests when
configured". The provider live-test convention (the voices and systemone
live tests) is: the test runs when the gateway is configured and is
skipped — with setup instructions, never failed — when it is not.

Two adjacent defects surfaced while preparing the live test:

- The provider-down and deadline unit tests waited for *N* `message.committed`
  events before snapshotting the session's event log, then asserted on the
  `decision.resolved` count in that snapshot. The resolved event is
  published on the publisher chain just after the commit it explains, so a
  snapshot taken between the two misses it and the count assertion is flaky
  under `-race` (observed: the provider-down test failed once in a full
  suite run, green in every rerun and in 7 consecutive focused runs before
  the cause was found).
- A cold adjudication cannot be proved on the bare synthetic clip: the
  clip's textbox dismisses at 2.3 s of recorded time (12 s at the 5× live
  pace), and a cold gateway call has been observed at 13 s straight after a
  GPU reset. A reply that lands after the box has dismissed is fenced
  `stale_text`, the commit proceeds on local evidence alone, and the run is
  not a live decision proof.

## Decision

1. **`TestLiveDecision`** (`internal/runtime/decision_live_test.go`) runs a
   replay session of the synthetic clip with the decision role wired to the
   deployed System One / Clef-Omni endpoint through the provider router —
   the same `DecisionOptions` shape `cmd/game-narrator`'s
   `newDecisionWiring` builds: the model id from
   `GAME_NARRATOR_ROLE_DECISION_MODEL` (default `clef-omni`), the System
   One protocol, `router.New(providers.NewRegistry(roles))`. The session's
   speech pipeline runs the real TTS (gateway voice design, a `gn_test_`
   clone registered through the direct Base registry and removed on
   cleanup, the shared TTS cache, the file sink as evidence) the way the
   speech live test does.
2. **Test-owned configuration, named in the test.** Two knobs are set for
   the live test and differ from production deliberately: the live budget
   (`120 s` in place of the role's 1500 ms production floor — the systemone
   live budget's precedent, for the cold gateway) and `Margin: 1.0`
   (the unit tests' lever: production leaves the margin at zero, so Clef is
   asked only for a ranking the local scoring abstained on; 1.0 asks the
   model about every non-abstained ranking, which is what sends the clip's
   ambiguous line to the gateway at all). Neither choice changes the
   wiring under test; the margin policy itself is unit-tested.
3. **The clip is re-stamped and padded, not replaced.** The synthetic
   24-frame fixture (docs/spec/11 "Fixture origins") is re-timed at the
   5× live pace (ADR-0048) and padded with 60 copies of the line's last
   full-line frame (frame 18): the padding re-shows the ambiguous line, so
   a cold adjudication lands while the box is still up — the box reappears
   as a new lifetime and its READY observation is the reply-time reading
   the fences apply the answer to. The OCR engine stays the pipeline's
   scripted truth seam (this test is about the decision and audio halves,
   not recognition); every decision and TTS byte is real, and the test
   never prints secrets.
4. **The assertions.** (a) A real call went out: the first
   `decision.requested` carries the ambiguous pair (both `demo-page-1` and
   `demo-x01` among the candidates), and every `decision.resolved` carries
   the gateway's request id and latency. (b) The answer drove the commit:
   every `CLEF_DISAMBIGUATED` commit matches an applied
   `decision.resolved` on `observation_seq` and the chosen message id.
   (c) Speech followed with the right voice: the **newest** CLEF commit's
   generation reaches `speech.ended {result:"ended"}` — only the newest can
   play to the end, because a newer commit cancels the older line's audio
   (docs/spec/01 invariant 2) and the padded box stays up until
   `source_end` — the speech events carry the narrator profile (the real
   `gn_test_` design, no substitute voice), and the file sink wrote a
   decodable WAV with delivered frames.
5. **The flake fix.** The provider-down and deadline waits now demand both
   the commit and its `decision.resolved` in the event log before
   snapshotting, so the assertions read a log that contains what they
   assert.

## Tradeoffs

- The live test spends, per run, one VoiceDesign audition, one clone
  registration (removed afterwards), ~3 Clef adjudication calls and ~3 TTS
  generations (~44 s wall on a warm gateway, up to ~15 min bounded on a
  cold one). It is skipped, not failed, when the gateway or the direct
  registry is unconfigured, so the cost is paid only where the live gate
  runs (`make test-models`, `-run Live`).
- `Margin: 1.0` makes the live test spend more model calls than a
  production session on the same clip would. That is the point — the test
  exists to prove the call path, and the production margin semantics
  (ask only on abstention) are the unit tests' to own.
- Padded full-line frames mean the session sees the line twice more (the
  re-shown line is a new lifetime). The test asserts on every CLEF commit
  rather than a fixed count, so the proof holds whether the padded line
  adjudicates once, twice, or not at all (a fenced padded reply still leaves
  the first two commits as proof).

## Validation

Two consecutive runs on the deployed gateway (both `PASS`, 43.1 s / 43.9 s
wall; OCR the truth seam, every decision and TTS byte real):

- Run 1: request ids `06GMVTNNMZ7VJT8JG8N3HWW1R7` (294 ms),
  `06GMVTP3KCKB46TN6PSSJJ3JB4` (317 ms), `06GMVTPDFYDV7H8BN3EMCMESN5`
  (600 ms), model `clef-omni`; three `CLEF_DISAMBIGUATED` commits at
  observations 15/24/31, generations 1/2/3; newest generation's speech
  `started` with the `gn_test_db18fcae` narrator profile and `ended`
  `result:"ended"`; evidence WAV `gen3-…-3.wav` 161,324 bytes, 24 kHz,
  80,640 frames (~3.36 s delivered).
- Run 2: request ids `06GMVTWTQ41KRQAT4TBCN1GKA6` (606 ms),
  `06GMVTX8J2NQGXAT626W8VDE1` (596 ms), `06GMVTXJGP606RXRZBGZKX2A86`
  (530 ms); same commit shape (observations 15/24/31) and the same 3.36 s
  evidence WAV.

Unit side: `go test -race -count=5 -run 'TestDecision' ./internal/runtime/`
green (the flake reproduced before the fix and not across 7+ focused runs
after it); `go test -race ./internal/runtime/` green in two consecutive
full-suite runs; `go build ./...` and `go vet ./cmd/... ./internal/session/...
./internal/runtime/ ./internal/pipeline/` clean.

## Consequences

- The live decision gate (docs/spec/16) is runnable from the same
  environment as the other provider live tests and is picked up by
  `make test-models` automatically (`-run Live`).
- The production decision wiring (`cmd/game-narrator` → `runtime.Options.Decision`
  → pipeline) is now proven against the deployed endpoint end to end:
  request out, answer fenced and applied, commit with the model's choice,
  audio on the right voice, WAV evidence on disk.
- The event-log join the developer UI relies on (frame → OCR → candidates →
  Clef request → commit, on `observation_seq` and the request id) has its
  live half exercised, not only its faked one.

## Reversal plan

The test is additive: delete `internal/runtime/decision_live_test.go` (and
this ADR) to remove the live gate; nothing in the product path changes.
The flake fix (waiting for the resolved event as well as the commit) stays
independent of this decision. The production wiring itself is ADR-0050's
scope and is unaffected either way.
