#!/usr/bin/env node
// A deterministic TTS stand-in for the voice-workflow UI test (docs/spec/12
// "UI" mode): it speaks the wire contract internal/providers/tts verified
// against the deployed checkpoints (ADR-0022) — POST /audio/speech for the
// VoiceDesign and Base checkpoints, and the /audio/voices registry on the
// direct Base service — but returns WAVs it synthesises itself, so the
// whole voice setup (audition, approve, register, preview, cache hit) runs
// end to end with no network and no model.
//
// What it refuses, honestly: a Base synthesis for a voice that is not
// registered on this "instance" is a 400, the same way the real Base
// checkpoint answers — so the preview after the registration is what makes
// the page's HIT/MISS flow, not a lenient fake.
//
// usage: node web/e2e/fake-tts.mjs <port>

import { createServer } from "node:http";

const port = Number(process.argv[2]);
if (!Number.isInteger(port) || port < 1) {
  process.stderr.write("usage: node fake-tts.mjs <port>\n");
  process.exit(2);
}

const DESIGN_MODEL = "fake-voicedesign";
const BASE_MODEL = "fake-base";

// The registry state of this one fake "instance". name -> { consent, refText }.
const voices = new Map();
let requestSeq = 0;

// ── WAV synthesis ────────────────────────────────────────────────────────────

// A valid, non-silent, unclipped PCM16 mono WAV. The frequency is a function
// of the seed, so "the same line, same voice" always renders the same bytes —
// the speech cache's miss-then-hit is about the cache, and the fake must not
// make the audio itself vary under a stable key.
function wavFor(seed, rate = 24000, seconds = 2) {
  const freq = 160 + (seed % 400);
  const n = rate * seconds;
  const data = Buffer.alloc(n * 2);
  for (let i = 0; i < n; i++) {
    const s = Math.round(9000 * Math.sin((2 * Math.PI * freq * i) / rate));
    data.writeInt16LE(s, i * 2);
  }
  const header = Buffer.alloc(44);
  header.write("RIFF", 0, "ascii");
  header.writeUInt32LE(36 + data.length, 4);
  header.write("WAVE", 8, "ascii");
  header.write("fmt ", 12, "ascii");
  header.writeUInt32LE(16, 16);
  header.writeUInt16LE(1, 20); // PCM
  header.writeUInt16LE(1, 22); // mono
  header.writeUInt32LE(rate, 24);
  header.writeUInt32LE(rate * 2, 28);
  header.writeUInt16LE(2, 32);
  header.writeUInt16LE(16, 34);
  header.write("data", 36, "ascii");
  header.writeUInt32LE(data.length, 40);
  return Buffer.concat([header, data]);
}

// A stable 32-bit hash of the seed text (djb2).
function hash(text) {
  let h = 5381;
  for (let i = 0; i < text.length; i++) h = ((h << 5) + h + text.charCodeAt(i)) >>> 0;
  return h >>> 0;
}

// ── registry: the multipart form is parsed by hand (three known fields + one
// file part; a dependency for this is the kind of weight the harness avoids) ──

function parseMultipart(body, contentType) {
  const match = /boundary=(?:"([^"]+)"|([^;]+))/.exec(contentType);
  if (!match) return null;
  const boundary = `--${(match[1] ?? match[2]).trim()}`;
  // The boundary itself is ASCII; the audio part is not, but the latin1 ("binary")
  // codec is a lossless one-byte-per-character round-trip, so splitting on the
  // text and converting back yields the exact bytes.
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

function errorBody(status, message) {
  return { status, body: JSON.stringify({ error: { message } }) };
}

// ── server ──────────────────────────────────────────────────────────────────

const server = createServer((req, res) => {
  const url = new URL(req.url ?? "/", `http://127.0.0.1:${port}`);
  const path = url.pathname.replace(/^\/v1(?=\/)/, "");

  if (req.method === "GET" && (url.pathname === "/healthz" || path === "/healthz")) {
    res.writeHead(200, { "content-type": "text/plain" });
    res.end("ok");
    return;
  }

  if (req.method === "GET" && path === "/audio/voices") {
    const names = [...voices.keys()];
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ voices: names }));
    return;
  }

  if (req.method === "DELETE" && path.startsWith("/audio/voices/")) {
    const name = decodeURIComponent(path.slice("/audio/voices/".length));
    if (!voices.has(name)) {
      const e = errorBody(404, `voice '${name}' not found`);
      res.writeHead(e.status, { "content-type": "application/json" });
      res.end(e.body);
      return;
    }
    voices.delete(name);
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ success: true, message: `removed ${name}` }));
    return;
  }

  if (req.method === "POST" && path === "/audio/voices") {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const body = Buffer.concat(chunks);
      const parsed = parseMultipart(body, req.headers["content-type"] ?? "");
      if (!parsed) {
        const e = errorBody(400, "multipart body required");
        res.writeHead(e.status, { "content-type": "application/json" });
        res.end(e.body);
        return;
      }
      const name = parsed.fields.get("name") ?? "";
      const consent = parsed.fields.get("consent") ?? "";
      const audio = parsed.files.get("audio_sample");
      // Only the app's own namespace is writable here (AGENTS.md rule 5): the
      // real registry is shared across tenants, and the client refuses to
      // reach outside gn_ too.
      if (!name.startsWith("gn_")) {
        const e = errorBody(400, `name '${name}' is outside the app's namespace`);
        res.writeHead(e.status, { "content-type": "application/json" });
        res.end(e.body);
        return;
      }
      if (!consent.trim()) {
        const e = errorBody(400, "consent is required");
        res.writeHead(e.status, { "content-type": "application/json" });
        res.end(e.body);
        return;
      }
      if (!audio || audio.length === 0) {
        const e = errorBody(400, "audio_sample is required");
        res.writeHead(e.status, { "content-type": "application/json" });
        res.end(e.body);
        return;
      }
      if (voices.has(name)) {
        const e = errorBody(409, `voice '${name}' already exists`);
        res.writeHead(e.status, { "content-type": "application/json" });
        res.end(e.body);
        return;
      }
      voices.set(name, { consent, refText: parsed.fields.get("ref_text") ?? "" });
      requestSeq += 1;
      res.writeHead(200, {
        "content-type": "application/json",
        "x-request-id": `fake-tts-${requestSeq}`,
      });
      res.end(
        JSON.stringify({
          success: true,
          message: "",
          voice: {
            name,
            consent,
            created_at: Date.now(),
            file_size: audio.length,
            mime_type: "audio/wav",
            ref_text: parsed.fields.get("ref_text") ?? "",
            speaker_description: "",
            embedding_source: "fake",
            embedding_dim: 256,
          },
        }),
      );
    });
    return;
  }

  if (req.method === "POST" && path === "/audio/speech") {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      requestSeq += 1;
      const requestID = `fake-tts-${requestSeq}`;
      let payload;
      try {
        payload = JSON.parse(Buffer.concat(chunks).toString("utf8"));
      } catch {
        const e = errorBody(400, "invalid JSON body");
        res.writeHead(e.status, { "content-type": "application/json" });
        res.end(e.body);
        return;
      }
      const { model, input, voice, task_type, ref_audio } = payload;
      const headers = { "content-type": "audio/wav", "x-request-id": requestID };

      const finish = (seed) => {
        res.writeHead(200, headers);
        res.end(wavFor(seed));
      };

      if (model === DESIGN_MODEL || task_type === "VoiceDesign") {
        // The design clip is a function of the line and the brief: the same
        // audition regenerated after a restart renders identically.
        finish(hash(`${input}|${payload.instructions ?? ""}|${model}`));
        return;
      }

      if (model === BASE_MODEL) {
        if (ref_audio) {
          // Inline reference (docs/spec/06 fallback): the reference itself
          // shapes the bytes, so the preview's cache key (fingerprint) is what
          // keeps it stable.
          finish(hash(`ref|${input}|${voice ?? ""}`));
          return;
        }
        if (voice && !voices.has(voice)) {
          const e = errorBody(400, `voice '${voice}' not found on this instance`);
          res.writeHead(e.status, { "content-type": "application/json" });
          res.end(e.body);
          return;
        }
        finish(hash(`${input}|${voice ?? "default"}`));
        return;
      }

      const e = errorBody(400, `model '${model ?? ""}' is not served by this instance`);
      res.writeHead(e.status, { "content-type": "application/json" });
      res.end(e.body);
    });
    return;
  }

  const e = errorBody(404, `no such route: ${req.method} ${path}`);
  res.writeHead(e.status, { "content-type": "application/json" });
  res.end(e.body);
});

server.listen(port, "127.0.0.1", () => {
  process.stdout.write(
    `fake-tts listening on 127.0.0.1:${port} (design ${DESIGN_MODEL}, base ${BASE_MODEL})\n`,
  );
});
// A handler fault is a test failure at the assertion, not a crashed fixture:
// keep serving so the spec's next step (or its error context) shows the real
// state instead of a dead port.
process.on("uncaughtException", (err) => {
  process.stderr.write(`fake-tts: uncaught: ${err?.stack ?? err}\n`);
});
process.on("SIGTERM", () => server.close(() => process.exit(0)));
process.on("SIGINT", () => server.close(() => process.exit(0)));
