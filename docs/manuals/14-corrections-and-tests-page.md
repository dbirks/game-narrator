# Corrections and the Tests page

## Purpose

Record user corrections so they become regression fixtures (not just fixes),
and run the scenario harness from the app's **Tests** page
(`/tests`) — durable jobs with passed/failed/blocked verdicts and an
inspectable HTML report — instead of reaching for the shell.

## Preconditions

- A `curator` (or higher) account for the correction flow: the editor's
  speech-text save is a curator write, `GET /api/v1/corrections` requires
  curator, and `POST /api/v1/corrections` requires curator or above (role
  table: docs/adr/0016-authentication-and-sessions.md). A `viewer` is
  refused (403) everywhere on this page's surfaces.
- A `developer` (or `admin`) account for the Tests page: the whole
  `/api/v1/dev/tests/...` surface is under the developer-only `/api/v1/dev`
  prefix of the scope table.
- The server running (the pages read the durable store and enqueue durable
  jobs on the same host).

## Steps — corrections become regression fixtures

1. **Fix a line in the editor.** On `/games/:id/dialogue`, open the message
   and correct its speech text, then save. The save is
   `PATCH /api/v1/profiles/{id}/messages/{mid}` — and inside that write's
   **own transaction** it records an `accepted` `speech_text` correction
   (before, after, curator) (ADR 0052): "the fix landed" and "the label
   landed" are one fact. There is no second step to "make it a fixture".
2. **Read the label back from the two views.** The corrections table is
   served twice, with the same rows and shape but a different reading:
   - `GET /api/v1/corrections` — the **curator's** label list (before/after,
     actor, status, reviewer; list is bounded, 500 rows max).
   - `GET /api/v1/dev/fixtures` — the **developer's** view of the same rows
     as the re-run inputs (docs/spec/08 "fixture recording and browsing").
   The change also shows on the publish page's diff for the draft revision.
3. **Record an explicit label without touching the profile.**
   `POST /api/v1/corrections` (curator+) records a proposed label —
   e.g. the inspector's "text incorrect / speaker incorrect" actions. It
   writes a label only; it **never applies a fix**. Profile changes go
   through the review path (docs/spec/03), not through corrections.
4. **Verify the row is a fixture, not a note.** The same row is what a
   re-run consumes: a correction on a message is a labelled occurrence —
   kind, before/after, actor, evidence — that the scenario re-runs can
   assert against (docs/spec/08 "user fixes become labels").

## Steps — the Tests page

1. **Open `/tests`.** It shows four panels:
   - **Scenario catalog** — every YAML under `testdata/scenarios/`
     (synthetic + dolphin): the docs/test-plan catalog row, file, origin,
     mode, profile, decision/tts/voice, fault count, and `status` —
     `runnable`, `blocked` (with the reason in place), or `load_error`.
   - **Gate results** — each gate's **latest recorded** result from
     `artifacts/gates/` (PASS / FAIL / NOT RUN, test counts, and the
     `instruction` — the make target plus prerequisites, i.e. the
     actionable form of NOT RUN).
   - **Dolphin readiness** — booleans only (`GAME_NARRATOR_DEV_DOLPHIN`,
     dolphin binary, Xvfb, game-image env), never paths (docs/spec/14).
   - **Provider readiness** — which model roles this process has
     configured, by name only.
2. **Run a scenario.** The run button enqueues a **durable job**
   (`POST /api/v1/dev/tests/scenarios/{scenario_id}/run`); the row then
   polls the job (`GET /api/v1/dev/tests/runs/{job_id}`) — `queued` →
   `running` → a terminal state — and reports the verdict:
   `passed`, `failed` or `blocked`, with the run's summary. A run survives
   a server restart (durable job, ADR 0047); rerunning a scenario never
   mutates the scenario file.
3. **Read the report.** The terminal run's report opens in the page's
   **sandboxed iframe** (`sandbox=""` — no scripts, no same-origin access;
   the report is a static verdict page by construction, and so is its
   download, served from `GET /api/v1/dev/tests/reports/{sha256}`). The
   dolphin-origin scenarios' reports link each scenario's frames, OCR,
   candidates, decisions and audio (15-dolphin-fixture-recorder.md).
4. **Read a BLOCKED row honestly.** A `blocked` scenario is a row that
   runs and says BLOCKED with its reason (ADR 0037: blocked is not pass —
   but the page that shows the reason is the point of the page). The two
   dolphin rows blocked today need a fixture re-record
   (15-dolphin-fixture-recorder.md, "What NOT to do").

## Verification

- The Playwright spec `web/e2e/corrections-fixtures.spec.ts` runs on the
  **packaged binary** and asserts: the editor save appears on the publish
  page **and** in both correction views, and a viewer gets refused.
- `web/e2e/m5-settings-tests.spec.ts` asserts the Tests page lists a
  blocked row with its reason, runs a synthetic scenario to a terminal
  `passed` state, and renders the report in the sandboxed iframe.
- The API contract is pinned by the OpenAPI contract test
  (`TestOpenAPIDocMatchesTheRouter`): adding a field without updating the
  generated client fails the build.

## What NOT to do

- Never use `POST /corrections` to *fix* content — it records a label; the
  fix goes through the editor + review path, and papering over a symptom
  without the evidence is how wrong speakers survive (manual 10).
- Do not treat a `blocked` row as skipped-and-fine: the run is the verdict,
  the reason is the work item (the two blocked dolphin scenarios carry
  their unblock steps in the scenario YAML).
- Do not open a report blob outside the sandbox (or "trust" it): the
  `sandbox=""` iframe is the boundary, and report content is a rendering of
  durable events, not a source of truth over `GET /api/v1/dev/events`.
- Do not copy dolphin scenario fixtures into the repo or point the Tests
  page at game text: fixtures live in the external
  `$GAME_NARRATOR_FIXTURE_DIR` and scenarios reference them by id +
  manifest hash only (AGENTS.md rule 5).
