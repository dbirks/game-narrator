# ADR-0050: Cap the VoiceDesign instruction derived from the voice brief
Date: 2026-10-18
Status: accepted
Owners: agent worker
Related requirements/tests: docs/spec/03 (structured editable voice_brief, VoiceDesign auditions), docs/spec/06 (provider error taxonomy), docs/spec/07 (voice workflow), ADR-0046 (research-grounded briefs), `TestDesignInstruction*` / `TestCreateAuditionsSendsCappedInstruction` / `TestFillDraftHonoursInstructionCap` (internal/voices), `TestLiveOnboarding` (internal/e2e)

## Context and constraints

The first live end-to-end onboarding run (Wind Waker, real gateway) completed research (12 sources), character staging (Link, Tetra, Ganondorf) and research-grounded briefs (~900 chars each, ADR-0046), then failed at the VoiceDesign audition: `bad_schema (http 400) model=qwen3-tts-voicedesign: tts: Bad Request: Instructions too long (max 500 characters)`.

The voices workflow stores the brief verbatim on the revision (`voice_profiles.design_instruction`) and passed it straight through as the `instructions` payload field (`fillDraft` → `tts.Client.Design`). A research-grounded brief is normally ~900 characters — longer than the live deployment's 500-character cap on `instructions`. The cap is a deployment property: the `instructions` field is part of the shared VoiceDesign contract, but its length limit is not, so it belongs to the caller that knows its provider, not to the brief itself (which the UI edits and the audit trail records in full).

## Decision

One small function in `internal/voices` (`instruction.go`, ADR-0050 is the only ADR this change needs):

1. **`DesignInstruction(brief string, maxRunes int) string`** derives the provider-facing instruction from the stored brief, deterministically, within `maxRunes` **runes** (the gateway counts characters, not bytes; multi-byte briefs must not count double). `maxRunes <= 0` uses the default cap `DesignInstructionCap = 500`.
2. A brief that parses as the structured voice_brief JSON object (optionally wrapped in a `voice_brief` key, unknown string fields sorted by key) is re-serialised with the voice-design fields first — timbre, age, pace, emotion, accent — and the prose notes last. Segments that do not fit the cap are dropped from the lowest priority up: the cap drops prose before it cuts a design field. Only if the single top-priority field exceeds the cap is text cut.
3. **No mid-word cuts.** A cut lands on the last space at or before the cap (trailing punctuation trimmed); the only hard cut is a single word longer than the cap, at a rune boundary — an empty instruction would be a worse failure than a truncated one.
4. **Wiring and configuration.** `Service.fillDraft` sends `DesignInstruction(rev.DesignInstruction, s.InstructionCap)`; the new `Service.InstructionCap int` field (0 → default) makes the cap a per-service setting, so a deployment with a different limit configures it without code changes. The `tts.Client` is unchanged: it sends whatever `DesignRequest.Instructions` it is given and still surfaces an over-long rejection as `bad_schema` (docs/spec/06), which is the correct behaviour for a future limit change. The revision row and the audition rows keep the **full** brief (`design_instruction`, `voice_auditions.brief`); only the checkpoint payload carries the capped derivation.

## Options considered

- **Cap inside the TTS client (`tts.Client.Design`).** Rejected: the client is provider-neutral and already owns two negotiated capabilities (seed, Base task_type); the instruction limit is not something it can probe from a 400 without guessing, and truncating user-authored text inside a transport layer hides a domain rule. The voices domain owns the brief → instruction derivation; the client stays a faithful messenger.
- **Reject long briefs at `CreateDraft` (validation error).** Rejected: it would make the normal ~900-char research brief unusable on this deployment and force the user to shorten their own brief for a provider detail they cannot see. The brief is a document; the instruction is a derived payload.
- **Trailing truncation of the raw brief string.** Rejected: the last fields of the JSON object (or the end of a prose paragraph) would be chopped, leaving a structurally broken instruction; field-priority derivation keeps the voice-design fields whole.
- **Shorten the brief itself in `analyze_character` (fewer/shorter fields).** Rejected: the six-field shape is the docs/spec/03 contract and the brief is the editable unit the UI shows; shrinking it loses information the user expects to edit.

## Consequences and risks

- The live audition now receives up to 500 characters of instruction shaped by the brief's own voice-design fields, in a stable order, for every deployment — including ones whose cap is exactly 500.
- If a deployment's cap is lower than 500, operators set `Service.InstructionCap`; the derivation degrades predictably (prose drops first, then a word-boundary cut).
- The derivation is a pure function of (brief, cap): re-running the same draft yields the same payload, so audition provenance (`origin`, model) stays reproducible and cache keys are unaffected (they key on the approved reference, not the instruction).
- Risk: a cut instruction omits late fields (pronunciation notes) on a long brief. Acceptable and visible: the full brief remains in the revision and audition rows, and the UI edits the brief, from which the derivation always regenerates.
- The `design_voice` agent tool (internal/agent/tools/voice.go) still builds its own `instructions` string up to ~4000 chars and calls the TTS client directly. It is not wired in production agent runs (`TTS: nil` in cmd wiring — see ADR-0046 follow-ups) and the live e2e drives the voices service, so it is out of scope here; if it is ever wired, it should reuse `voices.DesignInstruction`.

## Verification

- Unit tests (`internal/voices/instruction_test.go`): a ~1000-char structured brief yields ≤ 500 runes starting with the top-priority field and ending on a word boundary; five whole design fields survive while a 350-char prose note is dropped; byte-identical output for the same fields in different JSON key orders (Go map iteration is random) and across repeated calls; multi-byte (CJK + emoji) briefs count as runes and never split a rune; prose cuts at a word boundary; the cap is configurable (100/250/500) and a tiny cap cuts rather than empties; wrapped `voice_brief` key, edited unknown fields and no-string-field JSON all behave.
- Service tests: `CreateAuditions` sends the derived instruction to the Design seam while the revision row and the audition `brief` keep the full text; `InstructionCap=100` is honoured end to end.
- Live rerun: `TestLiveOnboarding` (internal/e2e) against the real gateway — the step that failed at "audition 1 for Link: instructions too long" must now produce real WAV bytes for all 3 auditions × 3 characters.
- Live rerun result (2026-10-18, run 01M56MN1V8Y36Z7Z6B3DY38D16): the fix held — 9/9 auditions returned real WAVs (2.8–3.5 s each), voice approved + registered, preview 165 KB / 3.44 s; 25 model calls, 9 m 12 s wall. One finding outside this change's scope: the vLLM-Omni deployment emits **no request-id response header** on `/audio/speech` (verified by direct probe on both 200 and 400; the only headers are `x-vllm-omni-*` token counts and `x-llm-collective-model`), so audition rows record an empty `request_id` there. The e2e assertion was corrected to require the model identity and uniform request-id presence (and to log the deployment characteristic), rather than demand an id the provider never sends. A deployment-side `x-request-id` on the speech route (or a client-side correlation id in `internal/providers/tts`, providers-worker boundary) is the follow-up for full provider-side correlation.

## Rollback / migration

No schema change. Rollback: restore `fillDraft` to pass `rev.DesignInstruction` directly and delete `instruction.go`; the 500-char deployment then rejects long briefs again (the pre-change behaviour). No stored data is affected — the full brief was always what was stored.

## Links

- ADR-0046 (research-grounded briefs that made the ~900-char brief the normal shape)
- ADR-0005 / ADR-0022 / ADR-0023 (voice design/preview workflow and cache keys)
- docs/spec/03 line 61 (voice_brief field list), docs/spec/06 (provider error taxonomy), docs/spec/07 (speaker conditioning)
