# ADR-0072: The Clef adjudication bundle renders speaker options as positional wire aliases (sp1..spN), mirroring the candidate aliases

Date: 2026-10-19
Status: accepted
Owners: agent worker (browser-live-play)
Related requirements/tests: docs/spec/05 (Clef adjudication and commit policy), docs/spec/12 (correlation proof), docs/spec/13 (developer inspector), ADR-0050 (the bundled adjudication; c1..cN candidate aliases), ADR-0055 (the developer decision view), ADR-0070 (windwaker speaker identity is the opaque key `ww:spk:<n>`), ADR-0013 (recorded provider cassettes), AGENTS.md rules 2/3/8. Tests: `internal/providers/systemone/` (validate_test.go, cassette_test.go + testdata/decision.jsonl), `internal/runtime/decision_test.go`, `internal/e2e` (TestLivePlayUI — the live proof).

## Context and constraints

The bundled System One adjudication (ADR-0050) carries six questions. Candidate options were always rendered as positional wire aliases `c1..cN` plus `none`, because the protocol's choice-option id grammar is `[a-z0-9_]{1,32}` (`internal/providers/systemone/types.go`, `idPattern`) and message ids are ULIDs: the adapter validates the request before anything leaves the process and maps the aliases back through the candidates slice in `Interpret`.

Speaker options were not treated the same way: `BuildAdjudication` used `Speaker.ID` directly as the option id. That is legal for wire-safe ids (the cassette fixture uses `npc_a`/`narrator`), but ADR-0070 established that the windwaker adapter's speaker identity is the opaque dump key `ww:spk:<n>` — which carries colons and fails the option-id grammar.

The live proof found it (2026-10-19, TestLivePlayUI: real Dolphin, real gateway `clef-omni`, real browser UI): with the disc-imported windwaker profile (4320 lines, `ww:spk:*` speakers), **every** adjudication failed local request validation in 0 ms as `provider_error:bad_schema` ("question speaker option N has an invalid id"), the session committed every line locally, no provider request id ever existed, and the developer inspector's decision row read "(no provider call)". The browser spec — which asserts the decision's provider request id as the docs/spec/12 correlation proof — then waited on a testid the page can never render.

## Options

1. **Positional speaker aliases `sp1..spN` + `unknown`** (adopted): mirror the established candidate pattern exactly. `Speaker.ID` stays the mapping key (like `Candidate.ID`); `Interpret` receives the speakers slice and maps `spN` back. The wire never carries opaque identities.
2. Escape the speaker id at the wire boundary (colon → underscore) — rejected: it invents a lossy transform of an identity that ADR-0070 defines as exact; two distinct keys could collide after escaping, and it is a second, different aliasing rule beside c1..cN.
3. Drop the speaker question when an id is not wire-safe — rejected: it silently discards attribution precisely for the adapter that provides the game's own speaker truth (ADR-0070), and hides the protocol violation from the developer UI.
4. Widen the option-id grammar to accept colons — rejected: the wire grammar is the protocol's contract with the deployed checkpoint (AGENTS.md rule 8: verify the actual contract, do not assume); changing it to suit one adapter's ids is the inverse of the alias design.

## Decision

- `BuildAdjudication` renders the speaker question's declared-speaker options as `sp1..spN` (position in the speakers slice) plus the `unknown` option, with the speaker's description as the option description — the exact mirror of `c1..cN` + `none` for candidates.
- `Interpret` gains the speakers slice as its third argument and maps the chosen `spN` (and the per-option probabilities) back to `Speaker.ID`; an unmapped option id passes through unchanged (the documented raw-id behaviour for a caller that built the request itself), and `unknown` keeps meaning "cannot be established".
- The runtime (the only production caller) passes its speaker list through unchanged; the session still receives the opaque key as the decision's speaker id, so the ADR-0070 roster binding is untouched.
- The committed cassette `internal/providers/systemone/testdata/decision.jsonl` is regenerated with the documented `UPDATE_CASSETTE=1` mechanism (deterministic local fake harness, ADR-0013 — no live provider is involved in the re-record).
- Known, pre-existing, out of scope: the runtime wiring leaves `Speaker.Description` empty, so the model sees `spN` with an empty description for windwaker keys (it saw the raw key with an empty description before). The wire shape is now protocol-legal; enriching the description (e.g. the key's provenance) is a separate curation improvement.

## Tradeoffs

- The recorded wire request changes (cassette re-record); any external consumer of the old cassette would replay a request shape the checkpoint has already seen, so the cassette is the single source and is regenerated in place.
- A caller that passes wire-safe speaker ids (the cassette fixture, live_test.go) sees the same model-visible question with aliased ids; answers map back identically, so no behavioural change for them beyond the recorded bytes.

## Validation

- `go test ./internal/providers/systemone/ ./internal/runtime/` — the validation tests, the cassette round trip (live fake == record == replay) and the runtime decision tests pass; the cassette test fails without the re-record and passes with it.
- Live proof: `TestLivePlayUI` (real Dolphin + real gateway + real UI) — the inspector's decision row carries a genuine provider request id, and the browser spec's `trace-decision-request` assertion passes.

## Reversal plan

Revert `BuildAdjudication`/`Interpret` to raw speaker ids and re-record the cassette. That restores the `bad_schema` local failure for every opaque-keyed profile, so this ADR would be superseded (not deleted) by whatever protocol change made the raw ids legal.
