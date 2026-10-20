#!/usr/bin/env node
// The scripted in-app assistant gateway for the Traces e2e
// (web/e2e/traces.spec.ts, ADR-0109 / UX-05).
//
// It answers POST /v1/chat/completions in the OpenAI wire shape, deciding each
// reply from the conversation it was given — no per-request scripting, so a
// resumed attempt replays the same decisions. It drives exactly the UX-05
// wrong-speaker flow:
//
//   1. the operator's wrong-speaker message (the screen context names the
//      line and the target character)  →  find_similar_lines;
//   2. the find_similar_lines result    →  apply_speaker_fix (the first call,
//                                          unconfirmed, stages the gate);
//   3. the confirmation_required result →  a final text: the suggestion the
//                                          operator reviews before anything
//                                          lands (the run ends, the card stays
//                                          open);
//   4. the operator CONFIRMs the staged  →  apply_speaker_fix again with
//      action (the answer arrives as a user message)  confirmation=<id> and
//                                          identical arguments (the gate
//                                          verifies and applies);
//   5. the applied result               →  the final "done" text.
//
// A CANCELLED answer ends with a "cancelled" text and no apply call.
//
// It is test machinery, named for what it is: a synthetic, labelled-stub
// decision model. The real API, the real job runner, the real tools, the
// real store and the real confirmation gate all run against it (AGENTS.md
// rule 3: the model is the stub, everything around it is real). Each request's
// decision is logged so a failed run is debuggable from test-results/.

import { createServer } from "node:http";

const [portArg] = process.argv.slice(2);
const port = Number(portArg ?? 0);
if (!port) {
  process.stderr.write("usage: node web/e2e/fake-assistant.mjs <port>\n");
  process.exit(2);
}

const log = (line) => process.stdout.write(`fake-assistant: ${line}\n`);

// ── conversation reading ────────────────────────────────────────────────────

/** The screen context / user-message facts the flow is built from. */
const PROFILE_RE = /profile_id:?\s*=?\s*([A-Za-z0-9_-]+)/;
const SELECTION_LINE_RE = /selection:\s*line\s+([A-Za-z0-9_-]+)/;
const TARGET_RE = /target_character_id=([A-Za-z0-9_-]+)/;
const CONFIRMATION_RESULT_RE = /"confirmation_id"\s*:\s*"([A-Za-z0-9_-]+)"/;
const APPLIED_RE = /"status"\s*:\s*"applied"/;
const SIMILAR_ID_RE = /"id"\s*:\s*"([A-Za-z0-9_-]+)"/g;
const ANCHOR_RE = /"anchor_id"\s*:\s*"([A-Za-z0-9_-]+)"/;

const userTexts = (messages) =>
  messages.filter((m) => m.role === "user").map((m) => String(m.content ?? ""));

/** The [TOOL RESULT name=X ...] envelopes present in the conversation. */
const toolResults = (messages) =>
  messages
    .filter((m) => m.role === "tool")
    .map((m) => {
      const text = String(m.content ?? "");
      const name = /\[TOOL RESULT name=(\S+)/.exec(text)?.[1] ?? "";
      return { name, text };
    })
    .filter((r) => r.name !== "");

/** The ids a find_similar_lines result named (the anchor + every pattern hit). */
const similarIds = (resultText) => {
  const ids = new Set();
  const anchor = ANCHOR_RE.exec(resultText)?.[1];
  if (anchor) ids.add(anchor);
  for (const match of resultText.matchAll(SIMILAR_ID_RE)) ids.add(match[1]);
  return [...ids];
};

// ── the scripted replies ────────────────────────────────────────────────────

const chatCompletionsResponse = (model, { content = null, toolCall = null } = {}) => {
  const message = { role: "assistant", content };
  if (toolCall) {
    message.tool_calls = [
      {
        id: `call-fake-assistant-${Math.random().toString(36).slice(2, 10)}`,
        type: "function",
        function: { name: toolCall.name, arguments: JSON.stringify(toolCall.args) },
      },
    ];
  }
  return {
    id: `chatcmpl-fake-assistant-${Date.now()}`,
    object: "chat.completion",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [
      {
        index: 0,
        message,
        finish_reason: toolCall ? "tool_calls" : "stop",
      },
    ],
    usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
  };
};

/** Decide the reply for one chat request, given its messages. */
function decide(body) {
  const model = String(body?.model ?? "fake/assistant");
  const messages = Array.isArray(body?.messages) ? body.messages : [];
  const user = userTexts(messages);
  const results = toolResults(messages);
  const last = results.length > 0 ? results[results.length - 1] : null;

  // The flow's facts, from the screen context and the operator's message.
  const profileId = user.map((t) => PROFILE_RE.exec(t)?.[1]).find(Boolean) ?? "";
  const anchorId = user.map((t) => SELECTION_LINE_RE.exec(t)?.[1]).find(Boolean) ?? "";
  const targetId = user.map((t) => TARGET_RE.exec(t)?.[1]).find(Boolean) ?? "";

  const confirmed = user.some((t) => t.includes("CONFIRMED the staged action"));
  const cancelled = user.some((t) => t.includes("CANCELLED the staged action"));

  // A confirmation id the gateway already staged (from the apply_speaker_fix
  // confirmation_required result still in the conversation).
  const stagedConfirmation =
    results
      .map((r) =>
        r.name === "apply_speaker_fix" ? CONFIRMATION_RESULT_RE.exec(r.text)?.[1] : null,
      )
      .find(Boolean) ?? null;

  if (last?.name === "apply_speaker_fix") {
    if (APPLIED_RE.test(last.text)) {
      log("decide: apply_speaker_fix applied → final done text");
      return chatCompletionsResponse(model, {
        content:
          `Done: the confirmed lines are now re-pointed to ${targetId || "the correct speaker"}. ` +
          `The correction is applied and the line list is up to date.`,
      });
    }
    if (CONFIRMATION_RESULT_RE.test(last.text)) {
      if (cancelled) {
        log("decide: confirmation CANCELLED → final cancelled text (no apply)");
        return chatCompletionsResponse(model, {
          content:
            "Cancelled — the bulk speaker change was not applied and the staged action was dropped.",
        });
      }
      if (confirmed && stagedConfirmation) {
        const ids =
          similarIds(results.find((r) => r.name === "find_similar_lines")?.text ?? "") ||
          [anchorId].filter(Boolean);
        log(
          `decide: CONFIRMED → apply_speaker_fix (confirmation=${stagedConfirmation}, ${ids.length} lines, target=${targetId})`,
        );
        return chatCompletionsResponse(model, {
          toolCall: {
            name: "apply_speaker_fix",
            args: {
              profile_id: profileId,
              message_ids: ids,
              target_character_id: targetId,
              confirmation: stagedConfirmation,
            },
          },
        });
      }
      // First apply: the gate staged a confirmation. Suggest; the operator
      // confirms before anything lands (UX-05).
      log("decide: apply_speaker_fix staged a confirmation → final suggestion text");
      return chatCompletionsResponse(model, {
        content:
          `I checked the similar lines: ${targetId ? `they share the corrected pattern` : "a few others do too"}. ` +
          `I have staged a bulk fix that re-points them to the correct speaker; nothing has changed yet. ` +
          `Confirm to apply it, or cancel to leave the lines as they are.`,
      });
    }
    log("decide: unexpected apply_speaker_fix result → final text");
    return chatCompletionsResponse(model, {
      content: "The bulk fix did not come back as expected; I stopped without changing the lines.",
    });
  }

  if (last?.name === "find_similar_lines") {
    const ids = similarIds(last.text);
    log(`decide: find_similar_lines named ${ids.length} lines → apply_speaker_fix (staged)`);
    return chatCompletionsResponse(model, {
      toolCall: {
        name: "apply_speaker_fix",
        args: {
          profile_id: profileId,
          message_ids: ids,
          target_character_id: targetId,
        },
      },
    });
  }

  // The opening turn (or anything unexpected): read the similar lines first.
  log(
    `decide: no tool results (profile=${profileId || "?"}, line=${anchorId || "?"}, target=${targetId || "?"}) → find_similar_lines`,
  );
  return chatCompletionsResponse(model, {
    toolCall: {
      name: "find_similar_lines",
      args: { profile_id: profileId, message_id: anchorId },
    },
  });
}

// ── the HTTP server ─────────────────────────────────────────────────────────

const server = createServer((req, res) => {
  if (req.method === "GET" && req.url === "/healthz") {
    res.writeHead(200, { "content-type": "text/plain" });
    res.end("ok\n");
    return;
  }
  if (req.method === "POST" && req.url === "/v1/chat/completions") {
    let raw = "";
    req.on("data", (chunk) => {
      raw += chunk;
    });
    req.on("end", () => {
      try {
        const body = JSON.parse(raw || "{}");
        const reply = decide(body);
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify(reply));
      } catch (error) {
        log(`chat/completions fault: ${String(error?.message ?? error)}`);
        res.writeHead(500, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { message: String(error?.message ?? error) } }));
      }
    });
    return;
  }
  log(`unhandled ${req.method} ${req.url} → 404`);
  res.writeHead(404, { "content-type": "text/plain" });
  res.end("not found\n");
});

server.listen(port, "127.0.0.1", () => {
  log(`listening on 127.0.0.1:${port} (POST /v1/chat/completions, GET /healthz)`);
});
