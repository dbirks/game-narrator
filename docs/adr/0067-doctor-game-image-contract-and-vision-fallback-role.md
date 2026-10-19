# ADR-0067: Doctor's fresh-host contract for the game image (NOT CONFIGURED with the exact expectation) and the standard-dev `vision_ocr_fallback` role

Date: 2026-10-19
Status: accepted
Owners: agent worker (doctor-config task, acceptance gaps 6 and 8)
Related requirements/tests: docs/status/acceptance.md Gaps 6 and 8 (FR-04, FR-06, FR-13; M2 bench), AGENTS.md rule 5 (no game image paths in reports), docs/spec/13 ("no generic success on an unprobed service"; the diagnostic lists configured/missing components accurately), docs/spec/06 (role timeouts; the VLM 3-10 s band), ADR-0040 (VLM OCR fallback policy; the 27B checkpoint behind the shared gateway), ADR-0021 (the decision probe's own budget pattern), ADR-0054 (fixture manifests carry `game_image_sha256`, never the path), `internal/doctor` (checks.go, gameimage.go, providers.go), `internal/dolphin` (`FixtureDir`, the manifest schema), `internal/bench` (the captured holdout's image-identity gate), `internal/runtime` (`TestLiveOCRFallbackSession`), `.env.example`, docs/dev-environment.md, docs/manuals/04, docs/manuals/15

## Context and constraints

Two acceptance gaps left the preflight unable to tell a fresh host exactly
what to configure:

1. **Game image (Gap 8).** The captured-frame bench (class b), the real-Dolphin
   gates and the real-disc parse all need the authorized image at
   `GAME_NARRATOR_GAME_IMAGE_PATH`, but the image lives outside the repo
   (AGENTS.md rule 5: its path is never recorded; only its SHA-256, in the
   fixture manifests). A fresh host saw either a bare "not set" row or, with a
   wrong path, a cryptic `FAIL` that named no expectation and no gate. Gap 8's
   acceptance: a fresh host reports `NOT CONFIGURED` with the actionable
   steps, not a cryptic failure.
2. **`vision_ocr_fallback` role (Gap 6).** Sessions engage the VLM OCR
   fallback only when the role's model id is configured, but the standard dev
   env documented no value for it (`.env.example` carried `configure-me`), so
   doctor showed `NOT CONFIGURED` and the session silently ran
   primary-OCR-only. Acceptance: doctor `vision_ocr_fallback` OK with the
   live fallback test green. A first live probe also exposed a budget
   defect: the vision role's check budget (max(ModelTimeout, 4 × the role's
   10 s production timeout) = 60 s) is smaller than the sum of its *warm*
   calls — measured 2026-10-19 on the dev gateway (qwen3-8-27b): model
   listing ~30 ms, chat pong 9.6-18.5 s, controlled-image read 8.8-9.7 s,
   two capability probes on the same order — so a healthy gateway reported
   `FAIL: timeout` on a deliberately-run diagnostic.

Constraints: the image path must never appear in any report stream
(rule 5; `TestGameImageNeverPrintsThePath`); the identity the doctor may
surface is the SHA-256 *prefix* of the value the fixture manifests record
(the manifests live in the gitignored `$GAME_NARRATOR_FIXTURE_DIR`, read at
run time — never hardcoded); the production role timeout (10 s VLM band,
docs/spec/06) is a runtime-loop property and must not change for the sake
of a diagnostic.

## Decision

1. **`game.image` is a fresh-host contract, not just an existence check**
   (`internal/doctor/gameimage.go`, the reworked `checkGameImage`):
   - **Unset or empty variable → `NOT CONFIGURED`** with the exact
     expectation in the detail: the env var name; the expected file (the
     user's own legally obtained GameCube disc image, CISO/ISO); how to
     verify it (its SHA-256 must start with the 12-hex-char prefix read at
     run time from the `game_image_sha256` field of the fixture manifests
     under `$GAME_NARRATOR_FIXTURE_DIR` — when the manifests are present;
     otherwise the clause points at the manifests and says none are present);
     and the gates that need the image (the bench captured holdout,
     `make test-dolphin`, the real-disc parse `import-disc`).
   - **Set but wrong (missing file, unreadable, or a directory) → the same
     `NOT CONFIGURED`** with the expectation, plus which fault was measured
     ("does not exist" / "not readable" / "directory"). A wrong path means
     the image is still not configured; a `FAIL` here would be the cryptic
     failure Gap 8 exists to remove. (The old behaviour reported `FAIL`.)
   - **Set, present, manifests with a recorded SHA → the check hashes the
     whole file** (1 MiB chunks, context-checked between chunks) and
     compares: match → `OK` with the shared prefix as evidence; mismatch →
     `FAIL` naming both prefixes (expected and actual) and the remedy
     (point at the recorded image, or re-record the fixtures). Unreadable
     manifest or no manifest at all → `OK` on existence + size, with the
     detail stating the identity is not verified against a fixture
     (no fake assurance, docs/spec/13).
   - The check runs on its own 2-minute budget (`gameImageCheckBudget`): a
     ~1.1 GB image on a cold disk exceeds the 15 s probe deadline; the
     engine's `checkGrace` still bounds a probe that ignores the context,
     and a spent context degrades the row to `DEGRADED` with the reason
     rather than claiming an unverified identity.
2. **The vision role probe gets its own check budget** (`providers.go`,
   `visionProbeTimeout = 3 × time.Minute`, mirroring ADR-0021's
   `decisionProbeTimeout` pattern): for `openai_chat_vision` roles the check
   budget is the larger of the default role budget and 3 minutes. The
   production role timeout (10 s per request) is untouched — the diagnostic
   is a deliberate, user-run probe where a warm 27B-scale VLM legitimately
   takes seconds per request (measured 9-20 s each), and the live session
   test already uses a 2-minute transport allowance for the same reason
   (`TestLiveOCRFallbackSession`). The 5-minute `maxRoleProbeBudget` ceiling
   still caps any single row.
3. **The standard dev env configures the role** (`qwen3-8-27b`, the
   checkpoint the dev gateway serves for the OpenAI-compatible roles —
   ADR-0040's live evidence): `.env.example` now shows
   `GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_MODEL=qwen3-8-27b` with the
   shared gateway; docs/dev-environment.md (the gates table and the verified
   gaps) and docs/manuals/04 document the value and the rule that sessions
   engage the fallback only when the role's model is configured;
   docs/manuals/15 documents the `game.image` expectation.

## Options considered

- **Keep `FAIL` for a set-but-missing path** (the pre-ADR behaviour):
  rejected — the path is the only thing an operator can fix, and the
  failure text (by rule 5) cannot name it; the actionable `NOT CONFIGURED`
  with the expectation is Gap 8's acceptance, and `FAIL` remains
  available for the one case that is genuinely broken: a present image
  whose hash mismatches the fixtures.
- **Print the full SHA-256 in the report:** rejected — the full digest
  stays in the fixture manifests; the 12-char prefix matches the
  manifest-prefix convention the scenario HTML reports already use and is
  enough to verify the file at a terminal.
- **Hash only a file prefix for speed:** rejected — a truncated hash would
  let the doctor report "verified" on an identity the bench gate would
  reject; the full hash is the honest check, bounded by the 2-minute
  budget and the engine's grace.
- **Raise the production role timeout for VLM calls:** rejected — that
  changes runtime-loop behaviour (docs/spec/06's VLM band, the session's
  deadline semantics) for a diagnostic's convenience; the decision-role
  precedent (ADR-0021) is to give the *probe* its own budget.
- **Skip the vision probe and report the role on configuration alone:**
  rejected — docs/spec/13 forbids "generic success on an unprobed
  service"; the probe is a real image the model must read back.

## How to verify (commands and evidence)

- Unit (`internal/doctor`, 2026-10-19, `-race`): `TestGameImageUnsetIsNotConfigured`
  (expectation text: env var, CISO/ISO, prefix `677726191ba8` read from two
  synthetic manifests, "2 fixture manifests", the three gates),
  `TestGameImageUnsetWithoutManifests` (no invented prefix),
  `TestGameImageMissingFileIsNotConfigured` +
  `TestGameImageSetButMissingIsNotConfigured` (not a cryptic FAIL; no file
  name leak), `TestGameImageDirectoryIsNotConfigured`,
  `TestGameImageWrongHashIsFail` (both prefixes present; path absent from
  the text and from `Format`/`FormatJSON` output),
  `TestGameImageMatchingHashIsOK` (prefix evidence in the summary),
  `TestGameImageNoManifestsIsOKSizeOnly` (identity stated unverified),
  `TestGameImageHashBudgetExceededIsDegraded` (spent context → `DEGRADED`
  with the reason), `TestGameImageNeverPrintsThePath` (unchanged, now
  hermetic via the `Options.FixtureRoot` seam).
- Live doctor (2026-10-19, dev gateway, `--dev`):
  `game.image` → `OK … file exists, 1088455232 bytes and matches the fixture
  manifest (SHA-256 prefix 677726191ba8)` (real 1.1 GB image hashed against
  the five local fixture manifests); unset env → `NOT CONFIGURED` with the
  full expectation (JSON row verified field by field).
- Live vision role (2026-10-19): `GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_MODEL=qwen3-8-27b
  go run ./cmd/game-narrator doctor` → `OK providers.vision_ocr_fallback —
  model qwen3-8-27b answered the chat probe in 12.97s and read "NARRATOR 42"
  in 8.192s` (before the budget fix the same run failed at 60 s with
  `timeout`); throwaway latency probe: listing ~30 ms, chat 9.6-18.5 s,
  vision 8.8-9.7 s per request.
- Live fallback session (2026-10-19, role configured,
  `GN_GATE=test-models`): `TestLiveOCRFallbackSession` PASS in 115 s —
  primary Tesseract defeated (low-contrast CER 0.957 ≥ the 0.6 trigger),
  the VLM secondary read the line exactly (best CER 0.000), two genuine
  gateway calls with recorded request ids.

## Positive consequences

- A fresh dev host gets one report row that names the variable, the file
  kind, the verification prefix and the three gates — Gap 8's acceptance,
  testable per clause.
- A wrong image is caught at preflight with both prefixes, before the bench
  or the real-Dolphin gate spends minutes discovering it.
- The standard dev env turns the VLM OCR fallback on by documentation; the
  doctor proves it with a real image read, and the live session test stays
  the end-to-end proof.

## Negative consequences / risks

- The `game.image` check can hash a full disc image (bounded: 2-minute
  budget, `DEGRADED` on a spent context; doctor is a deliberate diagnostic,
  and the bench gate already hashes the same file).
- Two fixtures recorded against different images make the mismatch row name
  several expected prefixes; re-recording one fixture is the remedy.
- The 3-minute vision budget makes a wedged gateway hold that one row for up
  to 3 minutes before `FAIL` (the engine still reports the rest of the
  table; `checkGrace` applies).

## Rollback/migration path

Revert the `game.image` rework (restore the old `checkGameImage`, delete
`internal/doctor/gameimage.go` and its tests) and the two budget/docs
changes independently; no persistence, protocol or schema change, so there
is no migration. The old `FAIL`-on-missing-file behaviour returns, as does
the 60 s vision budget (and its false `FAIL` on a healthy warm gateway).

## Links/references

- docs/status/acceptance.md — Gaps 6 and 8 (this unit closes them);
  FR-04/FR-06/FR-13 rows; M2 bench row (image dependency).
- ADR-0040 (VLM OCR fallback policy and the 27B live evidence), ADR-0021
  (decision probe budget precedent), ADR-0054 (fixture manifests carry the
  image SHA, never the path), ADR-0058 (captured bench holdout's
  image-identity gate), docs/spec/13 (diagnostic contract), docs/spec/06
  (role timeouts), AGENTS.md rule 5 (no image paths).
- `internal/doctor` (checks.go `checkGameImage`, gameimage.go, providers.go
  `visionProbeTimeout`), `internal/dolphin/record.go` (`FixtureDir`,
  `FixtureManifest.GameImageSHA256`), `internal/bench` (captured.go /
  calibrate.go identity checks), `internal/runtime/ocr_live_test.go`
  (`TestLiveOCRFallbackSession`), `.env.example`, docs/dev-environment.md,
  docs/manuals/04-inference-gateway-connection.md,
  docs/manuals/15-dolphin-fixture-recorder.md.

Supersedes / superseded by: none.
