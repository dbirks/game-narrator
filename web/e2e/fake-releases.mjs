#!/usr/bin/env node
// The scripted GitHub-releases endpoint for the update-notification e2e
// (ADR-0088, web/e2e/restore-walk.spec.ts).
//
// It answers GET /repos/:owner/:repo/releases/latest with the wire shape the
// update checker reads (tag_name, name, html_url, published_at, body) and
// counts every releases request so a spec can assert the check really reached
// the endpoint — the notification's evidence, not just its rendering.
// POST /_release {tag_name} replaces the served release (a scenario seam for
// the spec; the appliance only ever reads). It is test machinery, named for
// what it is, and it logs every decision so a failed run is debuggable from
// test-results/e2e-runs/fake-releases.log.

import { createServer } from "node:http";

const [portArg] = process.argv.slice(2);
const port = Number(portArg ?? 0);
if (!port) {
  process.stderr.write("usage: node web/e2e/fake-releases.mjs <port>\n");
  process.exit(2);
}

const log = (line) => process.stdout.write(`fake-releases: ${line}\n`);

// The release the endpoint serves. v9.9.9 is deliberately far beyond any real
// build: the walk's binary is a dev build (no ldflags stamp), and the point of
// the walk is that the section shows the endpoint's latest release with its
// notes link — the comparison's honesty is the Go tests' business.
let latest = {
  tag_name: "v9.9.9",
  name: "v9.9.9",
  html_url: `http://127.0.0.1:${port}/repos/dbirks/game-narrator/releases/tag/v9.9.9`,
  published_at: "2026-10-01T00:00:00Z",
  body: "e2e scripted release notes for the update-notification walk (ADR-0088).",
};

let releasesRequests = 0;

const server = createServer((req, res) => {
  const send = (status, body) => {
    const bytes = Buffer.from(JSON.stringify(body));
    res.writeHead(status, {
      "content-type": "application/json; charset=utf-8",
      "content-length": bytes.length,
    });
    res.end(bytes);
  };

  if (req.method === "GET" && req.url === "/healthz") {
    send(200, { ok: true });
    return;
  }
  if (req.method === "GET" && req.url === "/_stats") {
    send(200, { releases_requests: releasesRequests });
    return;
  }
  if (req.method === "POST" && req.url === "/_release") {
    let body = "";
    req.on("data", (chunk) => {
      body += chunk;
    });
    req.on("end", () => {
      try {
        const next = JSON.parse(body);
        if (typeof next.tag_name === "string") {
          latest = {
            tag_name: next.tag_name,
            name: next.tag_name,
            html_url: `http://127.0.0.1:${port}/repos/dbirks/game-narrator/releases/tag/${next.tag_name}`,
            published_at: new Date().toISOString(),
            body: "e2e scripted release (scenario seam).",
          };
          log(`release replaced by ${latest.tag_name}`);
          send(200, { ok: true });
        } else {
          send(422, { detail: "tag_name is required" });
        }
      } catch {
        send(422, { detail: "invalid JSON" });
      }
    });
    return;
  }
  if (req.method === "GET" && (req.url ?? "").endsWith("/releases/latest")) {
    releasesRequests += 1;
    log(`served ${latest.tag_name} (request ${releasesRequests})`);
    send(200, latest);
    return;
  }
  log(`unrouted ${req.method ?? ""} ${req.url ?? ""}`);
  send(404, { detail: "not found" });
});

server.listen(port, "127.0.0.1", () => log(`serving on 127.0.0.1:${port}`));
