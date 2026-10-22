# ADR-0161: Thinking-leak normalisation in the OpenAI-compatible adapter and the vision-capability gate for the collective's `flint` model

Date: 2026-10-22
Status: accepted
Owners: agent worker (flint-compat task)
Related requirements/tests: docs/spec/06 "Model-serving version and health negotiation" (capability findings, failure taxonomy), docs/spec/12 "Contract" (recorded vs live), docs/spec/16 (implementation and acceptance); ADR-0009 "Provider roles and the OpenAI-compatible chat adapter" (reasoning-effort amendment of 2026-10-18), ADR-0040 (VLM OCR fallback policy), ADR-0065 (live-gate marker), ADR-0067 (doctor vision role), ADR-0049 (probe budgets); `internal/providers/openai` (client, live tests, fallback tests), `internal/providers/recorded` (live record/replay tests), `internal/vision/ocr` (VLM live test), `internal/runtime` (OCR fallback live session), `internal/agent/tools` (live web-proposal test).

## Context and constraints

Measured 2026-10-22 against the live gateway (`https://llm-collective.e-gineering.com/v1`, model `flint`, the collective's general model since 2026-10-21):

1. **flint always thinks.** `reasoning_effort: "none"` and `chat_template_kwargs: {"enable_thinking": false}` are the documented levers, but the live gateway behaved as if neither took effect for the narrating paths, and its reasoning parser does not split thinking from the answer: `choices[0].message.content` came back leading with the reasoning block and trailing the answer (`"We need answer exactly: pong. User asks Reply with exactly: pong. Must comply. No extra.pong"`), while the thinking text rode in a field the adapter did not read (`reasoning`, vLLM-family spelling; the adapter read `reasoning_content`, the OpenAI-compatible spelling). With a small `max_tokens` the content degenerated into the bare block and `finish_reason` became `length`.
2. **flint takes no image input.** The gateway answered `400 image_input_unsupported: model "flint" has no host that takes image input`, with the image-capable models listed as `justin-dickey/qwen3-8-27b-amd`.
3. **The live gate was failing honestly but wrongly**: `make test-models` reported 26 PASS / 7 FAIL / 1 NOT RUN, with the failures concentrated in the thinking-leak and vision-capability legs.

## Decision

1. **Adapter: split leaked thinking out of the completion content, by the think tags only.** `openai.splitThinkBlock(content) (thinking, answer)` handles three shapes, each moving the thinking into the reasoning field (appended to any server-split reasoning):
   - a leading `<think>…</think>` block;
   - a leading `<think>` that never closes, where the budget ran out mid-thought: there is no answer, so finish_reason `length` classifies as budget exhausted;
   - an orphan `…</think>` with no opening tag before it. This is the tail the server's parser leaves after taking the opening tag, observed live.

   Content without the tags is returned verbatim. A layout-based split (cutting at the first blank line) was drafted and rejected in review: it would cut the first paragraph off any multi-paragraph answer.
2. **Reasoning field accepts both spellings.** The gateway spells the reasoning field `reasoning_content` (OpenAI-compatible, recorded in the cassettes) and the vLLM-family builds spell it `reasoning` (observed live on flint); the adapter reads the first present spelling.
3. **Send the thinking-disable template flag for the `"none"` effort.** When the resolved reasoning effort is `"none"`, the adapter sends `chat_template_kwargs: {"enable_thinking": false}` alongside `reasoning_effort: "none"`. The flag is harmless where the gateway ignores it; the existing `reasoning_effort` 400-degradation path remains as the fallback.
4. **Live vision legs report NOT RUN, not FAIL, on an image-capability gap.** The vision legs (`TestLiveChatVisionNamesBothColors`, `TestLiveRecordThenReplayVisionChat`, `TestLiveVLMOCRFallbackHardFontCrop`, `TestLiveOCRFallbackSession`) resolve the model through `GAME_NARRATOR_LIVE_VISION_MODEL` (default: `GAME_NARRATOR_LIVE_MODEL`). When the gateway answers an image-capability rejection, the test skips with the actionable instruction `NOT RUN: … set GAME_NARRATOR_LIVE_VISION_MODEL to an image-capable model …`; the gate's JUnit/NOT-RUN accounting already classifies skips whose message starts `NOT RUN` as NOT RUN (exit 2 for a NOT-RUN-only gate), so a text-only model can no longer fail the live gate.
5. **The dev environment no longer points the vision fallback role at a text-only model.** `.env.example` comments out `GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_MODEL` with a note to choose an image-capable model.

## Options considered

- **Only send `chat_template_kwargs.enable_thinking` (no content split).** Rejected: the field alone does not repair an already-leaking gateway, and the narrating content would still carry the block.
- **Only split the content (no template flag).** Rejected: the split repairs an existing leak; the flag prevents it on vLLM-style templates; both are cheap and the task requires both.
- **Split at the first blank line, or treat the whole content as reasoning on `finish_reason=length`.** Rejected. Both mangle legitimate answers: a multi-paragraph reply, or a real answer cut short by the budget. The tags are the only reliable evidence of leaked thinking.
- **Fail the live vision legs on a text-only model.** Rejected: the capability gap is environmental (wrong model configured), which the gate's NOT RUN convention exists to express.

## Positive consequences

- Content answers returned to the runtime are the bare answer; the reasoning field carries the think block; the `budget_exhausted` classification is reached correctly for an unterminated block.
- The live text tests assert the content is the bare answer (`pong`) and record reasoning-token findings as logged capability findings rather than failures.
- The live vision legs are honest: a text-only model yields NOT RUN with an actionable message, and the JUnit/NOT-RUN accounting classifies them as NOT RUN (exit 2) instead of FAIL.

## Negative consequences / risks

- The split keys on `<think>`/`</think>`. A model that marks its thinking differently would need its own rule.
- The live text tests can still observe prose answers (the model answering at its own verbosity); the "content is right" assertion is on the fixed probes' expected answer.
- The vision gate now depends on `GAME_NARRATOR_LIVE_VISION_MODEL`; until an image-capable model is configured the live vision legs are NOT RUN.

## How to verify (commands and evidence)

- Unit (offline): `go test -race -timeout 15m ./internal/providers/openai/... ./internal/providers/recorded/... ./internal/vision/ocr/... ./internal/runtime/... ./internal/agent/tools/...` with the canned `httptest` responses for the leaked block, the unterminated block, normal content, mid-content and tool-call turns; `go vet` and `gofmt -l` clean.
- Live (with the live env): `source /tmp/claude-1000/-home-david-dev-game-narrator/9dc5b2e5-7c1b-4bc2-96ea-0f3cdc2119d2/scratchpad/bin/live-env.sh; make test-models` → junit line (after this ADR: vision legs NOT RUN for flint; text legs on the bare answer).
- Live vision legs on an image-capable model: `GAME_NARRATOR_LIVE_VISION_MODEL=justin-dickey/qwen3-8-27b-amd make test-models` (at most one run; the other member's model).

## Rollback/migration path

Revert the adapter split (`splitThinkBlock`), the dual reasoning-field read and the `chat_template_kwargs` emission, and the live-test skips; no persistence, protocol or schema change is involved, so no migration or data rollback is required.

## Links/references

- ADR-0009 (reasoning-effort amendment), ADR-0040 (VLM OCR fallback), ADR-0065 (gate marker), ADR-0067 (doctor vision role), ADR-0049 (probe budgets).
- `internal/providers/openai/client.go` (`splitThinkBlock`, `chatTemplateParams`, reasoning resolution), `internal/providers/openai/live_test.go`, `internal/providers/recorded/live_test.go`, `internal/vision/ocr/vlm_live_test.go`, `internal/runtime/ocr_live_test.go`, `internal/agent/tools/live_webimport_test.go`, `.env.example`.

Supersedes / superseded by: ADR-0009's reasoning-effort amendment is extended by this ADR's adapter split and template-flag decision; ADR-0040's live evidence for the VLM fallback is superseded for the flint deployment by the vision-capability gate in this ADR.
