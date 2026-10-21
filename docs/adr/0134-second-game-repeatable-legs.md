# ADR-0134: The repeatable G12 second-game legs — the live TTS holdout-line test, the second-game e2e project, and the test-full evidence manifest

Date: 2026-10-21
Status: accepted
Owners: agent worker (issue #33, gap audit G12)
Related requirements/tests: ADR-0079 (the second synthetic game: bench set, committed clip, non-Latin abstain), ADR-0065 (gate marker discipline), ADR-0093 (the play-pc e2e project's port placement), ADR-0141 (the renamed dev inspector route), docs/spec/12 (evidence discipline; frame replay determinism), `internal/providers/tts` (secondgame_live_test.go, live_test.go, wav.go), `web/e2e` (second-game.spec.ts, support/ports.ts, support/servers.ts), `web/playwright.config.ts`, `scripts/gates/test-full-manifest.sh`, Makefile `test-full` recipe, `artifacts/second-game-live-tts/` (the one-shot evidence the live test replaces)

## Context and constraints

ADR-0079 landed the second game's recognition leg and its committed
fixtures (the bench-synthetic-002 set, the black-water-001 clip, the
non-Latin abstain path), and the 2026-10-19 acceptance audit recorded one
shot of real TTS evidence for it — `artifacts/second-game-live-tts/`
(manifest + WAV; a `gn_test_*` voice designed, registered on the direct
Base registry, used to speak the holdout line, and removed after). Three
gaps remained in the G12 row:

1. **The TTS leg was not repeatable.** The evidence runner was deleted
   after its run; re-running the evidence required re-writing it.
2. **No UI leg ever opened the second game on the real pages.** No spec
   imported the second set's profile, opened it in the Library or the
   developer inspector, or asserted the second game's upper-left region
   renders where the committed data says — the first game's pages and
   fixtures had carried the whole browser suite.
3. **The bench second-set report never reached test-full's evidence
   output.** The gate printed the report to its log; the machine-readable
   summary carried only the per-gate result lines, so the second game's
   holdout metrics lived in the bench gate's artifacts alone.

Constraints: the other workers share the tree (the live box edits
`Library.tsx` and the library e2e spec and config in flight), the spec
must stay independent of the redesign branches (stable main testids only),
the leg must run on the dev-host e2e appliance (the packaged binary, its
own data dir, the repo's testdata on the replay allowlist), and nothing
may be presented as real that is mocked (AGENTS.md rule 3).

## Options

- **Recreate the one-shot runner on demand.** Rejected: the audit's
  evidence would stay unrepeatable by construction.
- **A second bench flag on the existing `developer` e2e project.**
  Rejected: the developer project's seeded walk and the second game's
  walk need different servers (the seeded walk drives a different
  profile); one project, one server.
- **A dedicated `second-game` Playwright project with its own port and
  webServer, a dedicated live TTS test under the existing test-models
  gate, and a manifest writer the test-full recipe invokes.** Chosen:
  each leg lands where its gate already lives, and each is removable
  without touching the other's surface.

## Decision

1. **`internal/providers/tts/secondgame_live_test.go` —
   `TestLiveSecondGameHoldoutLine`, the repeatable live TTS leg.** It
   runs under `make test-models` (the `GN_GATE=test-models` marker,
   ADR-0065) and is NOT RUN without the gateway env and the direct Base
   registry URL, exactly like its siblings. It pins the committed
   holdout sample `bn0006` / message `bw-02` of bench-synthetic-002 —
   the line the one-shot run voiced, so the evidence stays comparable —
   verifying the pin against both committed documents (a drifted set
   fails at the pin, not mid-synthesis); designs a narrator voice
   (the one-shot run's brief), registers it under the `gn_test_`
   namespace with truthful synthetic consent (cleanup removes it even on
   a failed assertion), and generates the holdout line. Assertions:
   `ValidateAudio` (RIFF/PCM, format, the silence and clip invariants),
   a duration band the line's length admits (1–30 s; the one-shot
   measured 4.4 s), and measured peak/RMS bounds that say speech, not
   silence and not a tone (the data payload is walked with the same
   form-agnostic chunk walk `wav.go` uses; 8/16/24/32-bit PCM). Log
   lines carry model ids, durations, latencies, sample rates and
   sanitised voice names only — no URL or token, per the package's
   convention.

2. **`web/e2e/second-game.spec.ts` on a new `second-game` project
   (port `base + 41`, the next offset past the review band), with its
   own webServer** (the packaged binary, `GAME_NARRATOR_REPLAY_ROOTS`
   admitting the repo's testdata, no speech roles — the leg asserts
   committed lines and the region, not a voice; the walk's speech rows
   stay skipped, the developer project's same shape). The walk, all
   committed synthetic fixture, stable main testids only:
   - the committed bench-synthetic-002 profile is read from
     `testdata/bench/` and its second-game shape checked before import
     (id, origin, the single upper-left region with the box and layout
     in the upper half, six messages) — a wrong file fails before the
     walk, not mid-walk;
   - the profile is imported through the API's own manifest path
     (`POST /editions/{id}/profiles/import-manifest`, the digest
     re-verified server-side), reviewed and published;
   - the Library shows the second game as a first-class citizen (hero,
     shelf tile, published chip, start action);
   - the developer inspector runs a replay session over the committed
     black-water-001 clip with the imported profile: the named commits
     are exactly the two Latin lines, in capture order (`bw-02` then
     `bw-06` — the fuzzy floor's real decision for page A, the exact
     match for page B), and no commit ever names the non-Latin line
     (`bw-05`) — the load-bearing negative assertion, the narrator
     staying silent rather than fabricating;
   - the committed line shows through the dev UI: the timeline renders
     the committed rows, the detail panel names the line by its friendly
     source id with its content class and speech eligibility, and the
     observed frame carries the region's ROI box at the committed
     upper-left position;
   - the Regions editor stores the committed rect on the imported region
     row and renders that row's box at the committed fractions —
     structurally upper-left, where the first game's box (y≈0.756) is
     not.

   Two findings shaped the assertions, both recorded here so the next
   worker does not rediscover them:
   - **The session's pipeline config differs from the bench leg's.**
     The runtime cuts its crops with `DefaultCropOptions` (letterbox
     detection, the ADR-0115 §2 2px inset, the active-picture tracker);
     the bench leg runs cropless. On the non-Latin page the two configs
     read different OCR text, and under the session's crop the unread
     page can end as a `COMMITTED_ABSTAINED` event — a
     `message.committed` with `message_id: "unknown"` and
     `speech_eligible: false`. The total commit count is therefore not
     pinned; the named commits are.
   - **The import mints the region's store row id.** The seed's
     `upper-left` id travels as the seed, not the row, so the rendered
     box's testid is `region-box-<store ULID>`: the spec reads the row
     back through `GET /profiles/{id}/regions` and asserts the stored
     rect equals the committed one before locating the rendered box.

3. **The test-full evidence manifest.** The `test-full` recipe appends
   each gate's judged row (`gate`, `result`, `detail`) to
   `artifacts/gates/test-full-gates.tsv` and, after the summary table,
   invokes `scripts/gates/test-full-manifest.sh`, which writes
   `artifacts/gates/test-full-manifest.json` (the AGENTS.md evidence
   bundle's machine half): the provenance fields the bundle lists
   (git SHA, Go version, OS/arch, timestamp), one entry per gate, and
   the bench *second set's* report as a summary — the set, split, run
   id, sample count and each graded metric's name/value/n/met — naming
   the full report, report.md and run manifest in
   `artifacts/bench-second/` rather than inlining them. When the report
   is absent (the bench gate NOT RUN, or a partial run) the entry
   records the absence; the manifest never fails test-full, and a write
   failure only warns (the gate table stands).

## Tradeoffs

- **Pin the one-shot line, or pick the set's other holdout line?** The
  pin (`bn0006`/`bw-02`) keeps the repeatable leg's evidence comparable
  to the recorded one-shot run (same line, same voice brief); the set's
  grading is unaffected either way — the leg proves the route, not the
  set.
- **Assert `stats.commits == 2`, or the named commits exactly?** The
  session's crop makes the total engine-version-sensitive (the repo
  documents tesseract 5.3 vs 5.5 reading this clip's crops
  differently); the named commits are the stable ground truth the
  ADR-0079 leg grades.
- **Inline the second-set metrics, or name the files?** Summary: the
  manifest stays small and the report stays the source of truth (a
  re-run's report.json is what a reader opens next).

## Validation

- Live run, 2026-10-21 (gateway env configured, never printed):
  `TestLiveSecondGameHoldoutLine` PASS in 2.2 s — the narrator designed
  through `qwen3-tts-voicedesign` (3.28 s WAV), registered as
  `gn_test_c11aeae4`, the holdout line spoken through `qwen3-tts-base`
  (3.68 s, 176684 bytes, 24 kHz mono 16-bit, peak 0.7344, RMS 0.0830 —
  the one-shot reference: peak 0.828, RMS 0.108), the voice removed and
  the post-removal List clean. The offline paths were verified too:
  outside the gate the test skips with the package's gate message;
  inside the gate without the env it is NOT RUN with the setup
  instruction.
- The `second-game` project's spec PASS (wall 14.6 s) on the packaged
  binary: the import, publish, Library, inspector replay (session
  stopped on `source_end`, named commits `[bw-02, bw-06]` in order, no
  commit naming `bw-05`), the committed line in the timeline and detail
  panel, the ROI box upper-left on the observed frame, and the region
  row's box at the committed upper-left rect; the browser console
  stayed clean and the default data dir untouched. Screenshots under
  `test-results/`.
- The manifest: `make test-full` (probe run with an empty gate list)
  wrote `artifacts/gates/test-full-manifest.json` with the
  bench-second entry present (set bench-synthetic-002, 10 samples, 17
  metrics); the absent-report and missing-TSV paths both yield a valid
  manifest.

## Consequences

- The G12 TTS leg is re-runnable on any host with the gateway env under
  `make test-models`; its NOT RUN behaviour keeps the unit gate green
  offline (the junit summariser counts a "NOT RUN"-prefixed skip as NOT
  RUN and a plain skip as SKIP, as for the package's other live tests).
- The browser suite's second-game coverage is the same shape as the
  first game's holdout walk, on a dedicated appliance (its own port,
  data dir, replay allowlist), so it never collides with the other
  projects' servers and never needs the redesign branches' surfaces.
- test-full now ships a machine-readable evidence manifest naming the
  second set's report; CI's absence of the dev-host gates is unchanged
  (the manifest records those gates' NOT RUN rows like the table does).

## Reversal plan

Delete the spec and the four local config edits (`ports.ts` offset +
band entry, `servers.ts` case, `playwright.config.ts` url constant /
project / webServer entry, chromium `testIgnore` token), the Go test
file, the manifest script and the two `test-full` recipe additions. No
persistence, protocol or data change: nothing to migrate.
