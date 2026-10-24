# Diagnosing wrong speaker

## Purpose

Work out **why** a dialogue occurrence was voiced by the wrong character (or
abstained when it should not have), using the per-occurrence trace in the
developer UI. Every dialogue occurrence has its own trace (14b312d), so the
evidence chain for one line is: capture frame → region → OCR text → ranked
candidates → Clef decision → commit → voice → speech.

## Preconditions

- A `developer`-role account (the `/traces` page and the
  `/api/v1/dev/*` endpoints require it; the role table is in
  docs/adr/0016-authentication-and-sessions.md).
- The session's frames/OCR were retained for dev-trace sampling (the dev
  trace sample mode under its quota; `GET /api/v1/dev/assets/{sha256}`
  serves the sampled crops).

## Steps

1. **Find the occurrence.** Open `/traces` (the old `/developer` URL
   redirects there) and select the session; in the
   event timeline find the wrong moment — the `message.committed` event with
   the wrong character (or the missing `speech.*` events for an abstained
   line). Pin it. The detail panel shows that occurrence's trace — its
   candidates and its commit, with no text-matching across occurrences.
   (Same data via API: `GET /api/v1/dev/traces/{trace_id}`, with
   `GET /api/v1/dev/events` for the durable event log pages.)
2. **Check the OCR first.** Look at `text.observed` for the occurrence's
   frames: a mis-OCR'd textbox poisons everything downstream. If the text is
   wrong, the failure is capture/OCR (font, region calibration, or the VLM
   fallback path — ADR 0040), not speaker logic.
3. **Read the ranked candidates** (`message.candidates` in the pinned
   trace). The candidates table shows, per candidate: each component score
   as a bar, the combined probability (4 dp), the margin, and the matcher's
   reason code — `MATCH_EXACT`, `GRAPH_NEIGHBOR`, `PREDICTIVE_PREFIX`,
   `MATCH_FUZZY` or `ABSTAINED`. Ask: is the *correct* line ranked at all?
   - Correct line missing from the top ranks → retrieval/profile problem
     (message not imported, wrong edition/language, matcher weights — see
     ADR 0035 and `game-narrator bench calibrate` for the temperature fit).
   - Correct line present but outranked → inspect which component bar
     collapsed (text, prefix coverage, scene, edge prior, visibility, text
     kind, …) and the margin; a small margin means calibration/weights, a
     zero bar usually means the profile graph is wrong.
4. **Read the Clef decision** — the detail panel's **decision block** on
   the pinned occurrence (same data: `decision.resolved` on the trace):
   provider and model, the **request id** (the id to search the provider's
   logs by) and **which side issued it** — `client` or `provider`
   (`request_id_source`), the call's **latency**, the **outcome** and the
   **generation** it was applied to. The three outcomes the fences report:
   - `accepted` — the commit carries the model's candidate (commit reason
     `CLEF_DISAMBIGUATED`);
   - `abstain` — the model judged none of the candidates matched; the
     commit stands on the local evidence (message_id unknown,
     `speech_eligible` false);
   - `late-discarded (<reason>)` — the reply arrived after the page
     turned; the fence names itself. That is timing, not
     misclassification.

   In **live sessions** Clef is consulted only when the ranking is
   ambiguous (at most one adjudication in flight per session, bounded by
   the role timeout); with no decision role configured or the endpoint
   down, the row shows `local commit (decision unavailable: <reason>)` —
   the commit ran on the local evidence in degraded mode, and the event
   carries the reason the wiring recorded. Probabilities are raw signals,
   not reliability guarantees (docs/spec/06).
5. **Check the voice** for the committed message: the voice workflow's
   revision for that character (05-voice-registry-maintenance) — a stale
   registration on this instance makes a *correct* commit sound wrong.
   `voice_missing` falls back to the built-in default by design; never a
   different character's voice.

## Verification

- The diagnosis is reproducible from the trace alone: frame/OCR text,
  candidate ranking with reason codes, and the decision payload identify the
  same failure in a replay of the same fixture.
- After the fix (profile edit through review, calibration, or voice
  re-registration), a re-run of the same occurrence shows the correct line
  committed with the correct voice in the new trace.

## What NOT to do

- Never edit a profile's messages/character graph to paper over a symptom
  without the trace evidence — profile changes go through the review path
  (docs/spec/03, the publication flow), not by hand.
- Do not read speaker identity off a single frame: the trace is per
  **occurrence**, and the decision is fenced to the generation it was asked
  under; a reply from an older generation is discarded, not "wrong".
- Do not trust the dev UI over the API: when in doubt, pull
  `/api/v1/dev/traces/{trace_id}` and compare — the UI is a renderer of the
  same durable events.
