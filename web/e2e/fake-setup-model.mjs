#!/usr/bin/env node
// A deterministic scripted model for the setup-board e2e walk
// (setup-board.spec.ts, docs/design/plum-peach plan.md Phase 1 item 4): the
// onboarding agent's agent_reasoning role, speaking the OpenAI chat wire the
// provider adapters verify (the same stand-in boundary as fake-model.mjs —
// the model is scripted; everything around it is the real thing: the API,
// the job queue, the tool chain, the setup.* SSE events, the approval
// boundary).
//
// One flow, in order (the setup board's card walk):
//   1. import_user_asset  — the staged script enters the draft profile;
//   2. source_quality     — the dialogue source quality report;
//   3. stage_character    — Marla is staged (the first card move: finding);
//   4. stage_character    — Corvin is staged (the second card move);
//   5. analyze_character  — Marla's voice brief is written (finding → brief),
//      answered by the brief chat: the same endpoint serves the tool's
//      brief model, and the brief's system prompt is the branch;
//   6. final text         — the run stops; the run's durable state settles.
// The publish boundary is still the human's act: when the approval token
// arrives in a user message, the scripted model calls publish_profile with
// it (the token's single-use spend is shadowed by actedTokens, the same way
// fake-model.mjs does).
//
// usage: node web/e2e/fake-setup-model.mjs <port>

import { createServer } from "node:http";

const port = Number(process.argv[2]);
if (!Number.isInteger(port) || port < 1) {
  process.stderr.write("usage: node fake-setup-model.mjs <port>\n");
  process.exit(2);
}

const log = (...args) => process.stderr.write(`[fake-setup-model] ${args.join(" ")}\n`);

/** Approval tokens the scripted model has already acted on (the shadow of
 *  the token's single-use spend on the appliance side). */
const actedTokens = new Set();

// ── conversation reading ────────────────────────────────────────────────────

const SHA_RE = /sha256\s+([0-9a-f]{64})/;
const PROFILE_RE = /Draft profile:\s*([A-Za-z0-9_-]+)/;
const SOURCE_ID_RE = /"source_id"\s*:\s*"([A-Za-z0-9_-]+)"/;
const CHARACTER_ID_RE = /"character_id"\s*:\s*"([A-Za-z0-9_-]+)"/;
const TOKEN_RE = /gnapp_[0-9a-f]{64}/;
/** The analyze_character brief's system prompt (internal/agent/tools/voice.go
 *  voiceBriefSystemPrompt); the onboarding loop's prompt never carries it. */
const BRIEF_PROMPT_RE = /voice-design briefs/i;

const lastUserText = (messages) => {
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role === "user") return String(messages[i].content ?? "");
  }
  return "";
};

const profileIdOf = (messages) => {
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role !== "user") continue;
    const id = PROFILE_RE.exec(String(messages[i].content ?? ""))?.[1];
    if (id) return id;
  }
  return "";
};

const toolResults = (messages) =>
  messages
    .filter((m) => m.role === "tool")
    .map((m) => {
      const text = String(m.content ?? "");
      const name = /\[TOOL RESULT name=(\S+)/.exec(text)?.[1] ?? "";
      const status = /status=(\S+)\]/.exec(text)?.[1] ?? "";
      return { name, status, text };
    })
    .filter((r) => r.name !== "");

// ── the scripted replies ────────────────────────────────────────────────────

const chatCompletionsResponse = (model, { content = null, toolCall = null } = {}) => {
  const message = { role: "assistant", content };
  if (toolCall) {
    message.tool_calls = [
      {
        id: `call-setup-${Math.random().toString(36).slice(2, 10)}`,
        type: "function",
        function: { name: toolCall.name, arguments: JSON.stringify(toolCall.args) },
      },
    ];
  }
  return {
    id: `chatcmpl-setup-${Date.now()}`,
    object: "chat.completion",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [{ index: 0, message, finish_reason: toolCall ? "tool_calls" : "stop" }],
    usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
  };
};

/** The brief's answer (the shape analyze_character parses): one labelled
 *  voice_brief object, every creative field non-empty. */
const BRIEF_JSON = JSON.stringify({
  voice_brief: {
    vocal_age_impression: "late thirties",
    timbre: "low and steady, slightly hoarse",
    cadence: "measured, with pauses before the important word",
    emotional_baseline: "pragmatic warmth",
    language_notes: "dockside diction, short sentences",
    pronunciation_notes: "drops the terminal e on harbour",
  },
});

function decide(body) {
  const model = String(body?.model ?? "fake/setup-board");
  const messages = Array.isArray(body?.messages) ? body.messages : [];
  const lastUser = lastUserText(messages);
  const results = toolResults(messages);
  const last = results.length > 0 ? results[results.length - 1] : null;
  const profileId = profileIdOf(messages);

  // The brief chat: analyze_character's model call carries the brief's
  // system prompt; answer with the brief JSON, whatever else the turn says.
  if (messages.some((m) => BRIEF_PROMPT_RE.test(String(m.content ?? "")))) {
    log("decide: brief chat → voice_brief JSON");
    return chatCompletionsResponse(model, { content: BRIEF_JSON });
  }

  // The approval turn: the human approval token travels in a user message
  // only; the scripted model spends it once, through publish_profile.
  let tokenMessage = null;
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m.role === "user" && TOKEN_RE.test(String(m.content ?? ""))) tokenMessage = m;
  }
  if (tokenMessage) {
    const content = String(tokenMessage.content ?? "");
    const token = TOKEN_RE.exec(content)[0];
    if (actedTokens.has(token) || last?.name === "publish_profile") {
      log("decide: publish attempted → final text (one-call budget, no retry)");
      return chatCompletionsResponse(model, {
        content:
          "The publish attempt came back; the approval token is single-use, so I am stopping instead of retrying.",
      });
    }
    actedTokens.add(token);
    log(`decide: approval token present → publish_profile (profile ${profileId})`);
    return chatCompletionsResponse(model, {
      toolCall: {
        name: "publish_profile",
        args: { profile_id: profileId, approval_token: token },
      },
    });
  }

  // The staged flow.
  if (!last) {
    const sha = SHA_RE.exec(lastUser)?.[1];
    if (!sha) {
      log("decide: no staged sha → final text (nothing to import)");
      return chatCompletionsResponse(model, {
        content: "No staged script was provided, so there is nothing to import yet.",
      });
    }
    log(`decide: no tool results → import_user_asset (sha ${sha.slice(0, 12)}…)`);
    return chatCompletionsResponse(model, {
      toolCall: {
        name: "import_user_asset",
        args: {
          profile_id: profileId,
          staged_asset_sha256: sha,
          role: "dialogue_script",
          rights: "user_supplied",
          file_name: "dialogue.csv",
        },
      },
    });
  }
  if (last.name === "import_user_asset") {
    const sourceId = SOURCE_ID_RE.exec(last.text)?.[1] ?? "";
    log(`decide: import done → source_quality (source ${sourceId || "?"})`);
    return chatCompletionsResponse(model, {
      toolCall: { name: "source_quality", args: { profile_id: profileId, source_id: sourceId } },
    });
  }
  if (last.name === "source_quality") {
    const sourceId = SOURCE_ID_RE.exec(last.text)?.[1] ?? "";
    log("decide: quality done → stage_character (Marla)");
    return chatCompletionsResponse(model, {
      toolCall: {
        name: "stage_character",
        args: {
          profile_id: profileId,
          name: "Marla",
          role: "harbour guide",
          personality_summary:
            "Pragmatic harbour guide who keeps the docks moving and watches the tide closely.",
          source_ids: [sourceId],
        },
      },
    });
  }
  if (last.name === "stage_character") {
    const staged = results.filter((r) => r.name === "stage_character");
    // The import result carries the source id the staging cites.
    const importResult = results.find((r) => r.name === "import_user_asset")?.text ?? "";
    const sourceId = SOURCE_ID_RE.exec(importResult)?.[1] ?? "";
    if (staged.length < 2) {
      log("decide: Marla staged → stage_character (Corvin)");
      return chatCompletionsResponse(model, {
        toolCall: {
          name: "stage_character",
          args: {
            profile_id: profileId,
            name: "Corvin",
            role: "dock hand",
            personality_summary: "Quiet dock hand who knows every rope and bell on the pier.",
            source_ids: [sourceId],
          },
        },
      });
    }
    // Marla was staged first: her character id is the first stage result's.
    const marlaId = CHARACTER_ID_RE.exec(staged[0].text)?.[1] ?? "";
    if (!marlaId) {
      log("decide: no character_id on the first stage result → final text");
      return chatCompletionsResponse(model, {
        content: "The staging did not return a character id, so I am stopping here.",
      });
    }
    log(`decide: Corvin staged → analyze_character (Marla ${marlaId})`);
    return chatCompletionsResponse(model, {
      toolCall: {
        name: "analyze_character",
        args: { profile_id: profileId, character_id: marlaId },
      },
    });
  }
  if (last.name === "analyze_character") {
    log("decide: brief written → final text (the setup is complete for this run)");
    return chatCompletionsResponse(model, {
      content:
        "The roster is staged and Marla's voice brief is written from her cited lines; " +
        "the draft is ready for you. Approve the publish review when you are ready, " +
        "then review the voices on the cast page.",
    });
  }
  log(`decide: unexpected last tool ${last.name} → final text`);
  return chatCompletionsResponse(model, {
    content: "I have stopped; the timeline shows what I did. Send a message to continue.",
  });
}

const server = createServer((req, res) => {
  if (req.method === "GET" && req.url === "/healthz") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ ok: true }));
    return;
  }
  if (req.method === "POST" && req.url === "/v1/chat/completions") {
    let raw = "";
    req.on("data", (chunk) => (raw += chunk));
    req.on("end", () => {
      try {
        const body = JSON.parse(raw || "{}");
        const response = decide(body);
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify(response));
      } catch (error) {
        log(`chat/completions fault: ${String(error?.message ?? error)}`);
        res.writeHead(400, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { message: String(error?.message ?? error) } }));
      }
    });
    return;
  }
  log(`unhandled ${req.method} ${req.url} → 404`);
  res.writeHead(404, { "content-type": "application/json" });
  res.end(JSON.stringify({ detail: "unknown path" }));
});

server.listen(port, "127.0.0.1", () => log(`listening on 127.0.0.1:${port}`));
