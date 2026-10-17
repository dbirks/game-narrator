# Inference gateway connection

## Purpose

Point the app's model roles at an OpenAI-compatible gateway (plus dedicated
endpoints where a role needs one), read the capability findings it produces,
and verify the connection with `doctor` and `make test-models` instead of
guessing.

## Preconditions

- A reachable gateway and its token, out-of-band (never in the repo,
  AGENTS.md rule 5).
- `.env.example` as the starting point: every value there is a placeholder
  on a `.invalid` host, so a forgotten default fails loudly.

## Steps — configure the roles

Shared gateway (one variable set serves every role):

| Env var | Meaning |
| --- | --- |
| `GAME_NARRATOR_GATEWAY_URL` | shared gateway base URL (http/https; userinfo is rejected — credentials go in the token env var) |
| `GAME_NARRATOR_GATEWAY_TOKEN` | default name of the variable holding the bearer token |
| `GAME_NARRATOR_GATEWAY_TOKEN_ENV` | override that name to reuse an existing secret variable |

Per-role overrides (`GAME_NARRATOR_ROLE_<ROLE>_…`), a role is enabled only
when its `_MODEL` is set:

| Suffix | Meaning |
| --- | --- |
| `_MODEL` | model id; empty = role disabled → `NOT CONFIGURED` |
| `_URL` | dedicated endpoint for this role (it then uses its own credential path) |
| `_PROTOCOL` | `openai_chat`, `openai_chat_vision`, `systemone`, `vllm_omni_speech`, `probe_required` |
| `_TIMEOUT` | Go duration bounding one request (defaults: agent_reasoning 30s, fast_reasoning 10s, vision_ocr_fallback 10s, decision 1500ms, voice_design/voice_clone 120s, speech_asr 30s) |
| `_REASONING_EFFORT` | `none` \| `low` \| `medium` \| `high`; empty omits the field. Role defaults: fast_reasoning `none`, vision_ocr_fallback `none`, agent_reasoning `low`, others omitted |

Roles: `agent_reasoning`, `fast_reasoning`, `vision_ocr_fallback`
(OpenAI-compatible chat), `decision` (System One / Clef-Omni, ADR 0021),
`voice_design`/`voice_clone` (Qwen3-TTS, ADR 0022). `speech_asr` has no
adapter and no endpoint yet — it is BLOCKED (docs/status/blocked.md).

## Capability findings

For OpenAI-compatible roles, the adapter and `doctor` record what the
gateway actually accepts (docs/spec/06 negotiation, ADR 0009 amendments;
b89557a, 86e8d48). Each finding is `supported` / `rejected` / `unknown`, and
the app degrades honestly instead of failing:

- **JSON mode** — a 400 on `response_format: json_object` degrades to one
  retry without it plus a "answer with a single JSON object" instruction
  (finding cached per endpoint).
- **`tool_choice=required`** — a 400 degrades to one retry with `tool_choice
  auto` and an instruction to call the tool; a still-missing tool call is a
  typed error (`ErrToolCallMissing`), not a silent pass.
- **`reasoning_effort`** — a 400 naming the field drops it for that role for
  the process lifetime; an omitted effort can burn the whole `max_tokens`
  budget on thinking on reasoning-capable models, which is why the hot-path
  roles pin `none`.

The doctor prints the findings on the role's detail line, e.g.
`... json_mode=rejected tool_choice_required=rejected`; that is a diagnostic,
not a hard requirement.

## Steps — verify

```sh
bin/game-narrator doctor          # providers.<role> line per role: OK / FAIL / NOT CONFIGURED
make test-models                  # live provider contract tests (-run Live); NOT RUN without the env
```

`doctor` sends real probes: a chat probe per OpenAI role (plus a vision probe
for `vision_ocr_fallback` on a controlled image), a System One decision probe
for `decision`, and for the TTS roles a design probe, a registry list
(05-voice-registry-maintenance), a synthesis and a reference probe. Nothing
is reported `OK` on configuration alone.

## What NOT to do

- Never commit tokens, hostnames or URLs (AGENTS.md rule 5); the scanner
  (`scripts/release/scan-artifacts.sh`) fails builds on leaked gateway
  secrets (docs/release.md "Hard rules").
- Do not invent new `reasoning_effort` levels — `LoadRoles` rejects anything
  outside `none`/`low`/`medium`/`high`.
- Do not treat a capability finding of `rejected` as a broken connection:
  the documented fallbacks keep the role usable; what breaks a role is a
  probe that fails outright (`FAIL`, taxonomy kind named).
- Do not assume OpenAI chat semantics for every adapter — the `decision`
  and TTS roles speak different protocols (AGENTS.md rule 8).
