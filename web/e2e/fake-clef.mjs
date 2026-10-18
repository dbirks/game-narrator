#!/usr/bin/env node
// The scripted System One (Clef-Omni) gateway for the developer inspector's
// decision E2E (web/e2e/developer-decision.spec.ts).
//
// It is a synthetic stub, labelled as such: it answers POST /systemone in the
// wire contract internal/providers/systemone pins (docs/spec/06, ADR-0021) —
// the bundled typed questions in, the validated answer envelope out — and
// decides every choice question from the request it was given (no per-request
// scripting), so a re-run replays the same decisions. For the bundled
// adjudication that means: the top candidate (c1) wins the candidate question,
// the textbox questions answer yes/dialogue/complete/safe, and every answer
// carries the probabilities the client's validation demands (exact declared
// option set, sum 1, confidence equal to the chosen option's probability).
//
// The server answers with no request-id response header, exactly like the
// collective gateway's /systemone route (verified live 2026-10-18, ADR-0021
// amendment): the client's own X-Request-Id is the effective correlation id,
// and that is what the spec asserts the inspector shows — client-issued.
//
// Control endpoints (test machinery, same shape as fake-model.mjs):
//   GET  /healthz     { ok, mode }
//   POST /_mode       { mode: "answer" | "down" } — "down" tears every
//            /systemone connection down mid-flight (endpoint unreachable,
//            the degraded local-commit case the inspector must explain);
//   GET  /_requests   the /systemone requests logged, newest last.
//
// usage: node web/e2e/fake-clef.mjs <port>

import { createServer } from "node:http";

const port = Number(process.argv[2]);
if (!Number.isInteger(port) || port < 1) {
  process.stderr.write("usage: node web/e2e/fake-clef.mjs <port>\n");
  process.exit(2);
}

const log = (line) => process.stdout.write(`fake-clef: ${line}\n`);

/** "answer" decides the requests; "down" tears every /systemone socket down. */
let mode = "answer";

/** The /systemone requests this instance answered, in arrival order. */
const requests = [];

const CHOSEN_PROBABILITY = 0.7;

/** One choice answer: the chosen option takes the fixed mass, the rest split
 *  evenly, then the map is renormalised so the sum is exactly 1 (the client
 *  validates within ±0.02, and confidence equals the chosen probability). */
function choiceAnswer(criteria, prefer) {
  const ids = Object.keys(criteria ?? {});
  if (ids.length === 0) return null;
  const choice = prefer && ids.includes(prefer) ? prefer : ids[0];
  const restMass = 1 - CHOSEN_PROBABILITY;
  const perRest = ids.length > 1 ? restMass / (ids.length - 1) : 0;
  const raw = {};
  for (const id of ids) raw[id] = id === choice ? CHOSEN_PROBABILITY : perRest;
  const sum = ids.reduce((acc, id) => acc + raw[id], 0);
  const probabilities = {};
  for (const id of ids) probabilities[id] = Number((raw[id] / sum).toFixed(6));
  return { type: "choice", choice, confidence: probabilities[choice], probabilities };
}

/** Decide the answers for one bundled request. `questions` is the protocol's
 *  questions object: { id: { type, instructions, criteria? } }. */
function decide(body) {
  const questions = body?.questions && typeof body.questions === "object" ? body.questions : {};
  const answers = {};
  for (const [id, q] of Object.entries(questions)) {
    if (q?.type === "choice") {
      const prefer = id === "candidate_message" ? "c1" : undefined;
      const answer = choiceAnswer(q.criteria, prefer);
      if (answer) answers[id] = answer;
    } else if (q?.type === "noul") {
      answers[id] = { type: "noul", noul: id === "safe_to_speak" ? 0.9 : 0.85 };
    }
    // score questions: the bundled adjudication declares none; an unknown
    // question type is left unanswered so the client's validation says so.
  }
  return answers;
}

const server = createServer((req, res) => {
  if (req.method === "GET" && req.url === "/healthz") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ ok: true, mode }));
    return;
  }
  if (req.method === "POST" && req.url === "/_mode") {
    let raw = "";
    req.on("data", (chunk) => (raw += chunk));
    req.on("end", () => {
      try {
        const body = JSON.parse(raw || "{}");
        if (body.mode !== "answer" && body.mode !== "down") throw new Error("unknown mode");
        mode = body.mode;
        log(`mode set to ${mode}`);
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ mode }));
      } catch (error) {
        res.writeHead(422, { "content-type": "application/json" });
        res.end(JSON.stringify({ detail: String(error?.message ?? error) }));
      }
    });
    return;
  }
  if (req.method === "GET" && req.url === "/_requests") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ requests }));
    return;
  }
  if (req.method === "POST" && req.url === "/systemone") {
    if (mode === "down") {
      // The endpoint is down: no answer is ever written. Tearing the socket
      // down mid-read is the client-side transport failure (unreachable),
      // the same class as a dead port.
      log("POST /systemone in down mode → tearing the socket down");
      req.socket.destroy();
      return;
    }
    let raw = "";
    req.on("data", (chunk) => (raw += chunk));
    req.on("end", () => {
      let body;
      try {
        body = JSON.parse(raw || "{}");
      } catch (error) {
        log(`systemone: unreadable body → 422 (${error?.message ?? error})`);
        res.writeHead(422, { "content-type": "application/json" });
        res.end(JSON.stringify({ detail: "invalid JSON body" }));
        return;
      }
      const entry = {
        request_id: req.headers["x-request-id"] ?? "",
        model: String(body?.model ?? ""),
        questions: Object.keys(body?.questions ?? {}),
        candidate_choice: null,
      };
      const answers = decide(body);
      const candidate = answers["candidate_message"];
      entry.candidate_choice = candidate?.choice ?? null;
      requests.push(entry);
      log(
        `POST /systemone (x-request-id ${entry.request_id || "?"}) → candidate ${entry.candidate_choice ?? "n/a"}`,
      );
      res.writeHead(200, { "content-type": "application/json" });
      // No request-id response header: the collective gateway's documented
      // behaviour, so the effective correlation id is the client's.
      res.end(
        JSON.stringify({
          model: String(body?.model ?? ""),
          answers,
          usage: { input_tokens: 0, output_tokens: 0 },
        }),
      );
    });
    return;
  }
  log(`unhandled ${req.method} ${req.url} → 404`);
  res.writeHead(404, { "content-type": "application/json" });
  res.end(JSON.stringify({ detail: "unknown path" }));
});

server.listen(port, "127.0.0.1", () => log(`listening on 127.0.0.1:${port}`));
