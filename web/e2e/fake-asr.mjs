#!/usr/bin/env node
// A deterministic ASR stand-in for the audition QA UI test (docs/spec/12
// "UI" mode, SYNTHETIC STUB — AGENTS.md rule 3): it speaks the
// /audio/transcriptions wire that internal/providers/asr verified against the
// deployed qwen3-asr endpoint (ADR 0061) — multipart model/file/
// response_format, a verbose_json answer — but "listens" to the fake TTS's
// tone. The audition clip's frequency is a function of djb2(line|brief|
// model) (fake-tts.mjs), so the stub recovers which of the three fixed
// audition lines it was handed and answers with that line's text (WER 0) —
// or, for the line FAKE_ASR_FAIL_LINE names (1-based, default 3), with a
// planted mishearing, the flagged case the badge spec asserts (FR-10).
//
// It sends no x-request-id header: the client mints its own ULID
// correlation, which is the contract under test
// (internal/providers/asr/requestid.go).
//
// usage: node web/e2e/fake-asr.mjs <port>

import { createServer } from "node:http";

const port = Number(process.argv[2]);
if (!Number.isInteger(port) || port < 1) {
  process.stderr.write("usage: node fake-asr.mjs <port>\n");
  process.exit(2);
}

// The same fixed audition lines the app speaks (internal/voices/doc.go).
const LINES = [
  "Hello there. Let me tell you a little about today.",
  "The morning was calm, and the road ahead was long.",
  "I would like to ask you a small question, if that is all right.",
];
// The briefs the specs use when they create auditions, ";;"-separated (the
// seed couples line + brief + model, so the stub cannot recover a line it was
// designed with an unseeded brief). The first entry is the legacy single
// brief; the cast spec's regeneration brief is the second (its edited brief
// is a genuinely different string, so its run must be seeded too).
const BRIEFS = (process.env.FAKE_ASR_BRIEF ?? "A calm narrator voice.").split(";;");
const failLine = Number(process.env.FAKE_ASR_FAIL_LINE ?? 3);
const DESIGN_MODEL = "fake-voicedesign";
// The planted mishearing: shares no word with any of the three lines, so the
// WER is ~1.0 — unambiguously over the 0.2 bound.
const MISHEARING = "the harbour lights were out that evening";

let requestSeq = 0;

// The same stable 32-bit hash the fake TTS uses (djb2).
function hash(text) {
  let h = 5381;
  for (let i = 0; i < text.length; i++) h = ((h << 5) + h + text.charCodeAt(i)) >>> 0;
  return h >>> 0;
}

// fake-tts.mjs renders seed as a 160 + (seed % 400) Hz tone at 24 kHz.
function frequencyFor(seed) {
  return 160 + (seed % 400);
}

// Recover the tone's frequency by counting zero crossings over the whole
// data region: with N samples at rate r the duration is N/r, so crossings ≈
// 2·f·N/r and f ≈ crossings·r/(2·N) — adjacent integer frequencies differ by
// ~2 crossings per second of audio, robust to the odd miscount.
function frequencyOf(wav) {
  if (wav.length < 44 + 2) return 0;
  const rate = wav.readUInt32LE(24);
  if (!rate) return 0;
  let crossings = 0;
  let prev = wav.readInt16LE(44);
  for (let i = 44 + 2; i + 1 < wav.length; i += 2) {
    const s = wav.readInt16LE(i);
    if ((prev < 0 && s >= 0) || (prev >= 0 && s < 0)) crossings++;
    prev = s;
  }
  const samples = (wav.length - 44) / 2;
  return (crossings / 2) * (rate / samples);
}

// The multipart form is parsed by hand (the same three known fields + file
// part the fake TTS handles; a dependency for this is the kind of weight the
// harness avoids).
function parseMultipart(body, contentType) {
  const match = /boundary=(?:"([^"]+)"|([^;]+))/.exec(contentType);
  if (!match) return null;
  const boundary = `--${(match[1] ?? match[2]).trim()}`;
  const text = body.toString("binary");
  const parts = text.split(boundary);
  const out = { fields: new Map(), files: new Map() };
  for (const raw of parts) {
    const headerEnd = raw.indexOf("\r\n\r\n");
    if (headerEnd === -1) continue;
    const header = raw.slice(0, headerEnd);
    let payload = raw.slice(headerEnd + 4);
    if (payload.endsWith("\r\n")) payload = payload.slice(0, -2);
    const name = /name="([^"]*)"/.exec(header)?.[1];
    if (!name) continue;
    if (/Content-Disposition:\s*form-data/i.test(header) && /filename=/i.test(header)) {
      out.files.set(name, Buffer.from(payload, "binary"));
    } else {
      out.fields.set(name, payload.toString("utf8"));
    }
  }
  return out;
}

// Which audition line produced this clip: match the recovered frequency
// against the possible seeds (line × seeded brief, the design model).
// Returns the 0-based line index, or -1 when nothing matches.
function lineFor(wav) {
  const recovered = frequencyOf(wav);
  if (!recovered) return -1;
  let best = -1;
  let bestDelta = Infinity;
  for (let i = 0; i < LINES.length; i++) {
    for (const seeded of BRIEFS) {
      const seed = hash(`${LINES[i]}|${seeded}|${DESIGN_MODEL}`);
      const delta = Math.abs(frequencyFor(seed) - recovered);
      if (delta < bestDelta) {
        bestDelta = delta;
        best = i;
      }
    }
  }
  // Adjacent seeds are ~2 crossings apart per second; allow a full crossing
  // of slack, and fail loudly (the spec asserts a passed badge, which never
  // appears if the stub could not hear what it was handed).
  return bestDelta <= 1.5 ? best : -1;
}

const server = createServer((req, res) => {
  const url = new URL(req.url ?? "/", `http://127.0.0.1:${port}`);
  const path = url.pathname.replace(/^\/v1(?=\/)/, "");

  if (req.method === "GET" && (url.pathname === "/healthz" || path === "/healthz")) {
    res.writeHead(200, { "content-type": "text/plain" });
    res.end("ok");
    return;
  }

  if (req.method === "POST" && path === "/audio/transcriptions") {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      requestSeq += 1;
      const body = Buffer.concat(chunks);
      const parsed = parseMultipart(body, req.headers["content-type"] ?? "");
      const fail = (status, message) => {
        res.writeHead(status, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { message } }));
      };
      if (!parsed) return fail(400, "multipart body required");
      const file = parsed.files.get("file");
      if (!file || file.length === 0) return fail(400, "file part is required");

      const line = lineFor(file);
      const text = line === -1 ? MISHEARING : line + 1 === failLine ? MISHEARING : LINES[line];
      res.writeHead(200, { "content-type": "application/json" });
      // verbose_json shape, no x-request-id header (the client mints its own).
      res.end(
        JSON.stringify({
          task: "transcribe",
          language: "en",
          duration: 2.0,
          text,
        }),
      );
    });
    return;
  }

  const e = {
    status: 404,
    body: JSON.stringify({ error: { message: `no such route: ${req.method} ${path}` } }),
  };
  res.writeHead(e.status, { "content-type": "application/json" });
  res.end(e.body);
});

server.listen(port, "127.0.0.1", () => {
  process.stdout.write(
    `fake-asr listening on 127.0.0.1:${port} (brief ${JSON.stringify(brief)}, failing line ${failLine})\n`,
  );
});
// A handler fault is a test failure at the assertion, not a crashed fixture.
process.on("uncaughtException", (err) => {
  process.stderr.write(`fake-asr: uncaught: ${err?.stack ?? err}\n`);
});
process.on("SIGTERM", () => server.close(() => process.exit(0)));
process.on("SIGINT", () => server.close(() => process.exit(0)));
