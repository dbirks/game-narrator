// The contract document, imported rather than re-read at test time: Vite's asset
// pipeline resolves it from outside web/ (server.fs.allow in vite.config.ts), and
// tsconfig.app.json has resolveJsonModule on — which is also how we know the
// generated schema.d.ts and this object come from the same bytes.
import contract from "../../../internal/api/openapi/openapi.json";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  type Job,
  ApiClient,
  ApiError,
  IDEMPOTENT_CREATE_TEMPLATES,
  apiCode,
  apiStatus,
  jobsApi,
  newIdempotencyKey,
  profileResourcePath,
} from "./client";

/**
 * `src/api/client.ts` under a fake fetch (docs/spec/12 "Unit" mode: synthetic
 * stubs, clearly labelled — the real Go server is exercised by
 * web/e2e/catalog.spec.ts, not here). The point of these assertions is the three
 * wire rules the client exists to enforce: CSRF on unsafe methods only,
 * Idempotency-Key on creates only, If-Match on profile writes — plus the shape of
 * a failure, because a page can only branch on `code` if the code survives
 * parsing.
 */

interface Recorded {
  method: string;
  url: string;
  headers: Record<string, string>;
  body?: BodyInit;
}

const CSRF = "csrf-token-value";
const ORIGIN = globalThis.location.origin;

function makeClient(respond: (record: Recorded) => Response | Promise<Response>) {
  const calls: Recorded[] = [];
  const fetchMock = vi.fn(
    async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const record: Recorded = {
        method: (init?.method ?? "GET").toUpperCase(),
        url: typeof input === "string" ? input : input.toString(),
        headers: Object.fromEntries(new Headers(init?.headers).entries()),
        body: init?.body ?? undefined,
      };
      calls.push(record);
      return respond(record);
    },
  );
  const client = new ApiClient({
    fetch: fetchMock as unknown as typeof fetch,
    csrfRefresh: vi.fn(async () => "refreshed-token"),
  });
  client.setCsrfToken(CSRF);
  return { client, calls };
}

function json(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "content-type": "application/json" },
    ...init,
  });
}

function problem(status: number, extra: Record<string, unknown> = {}): Response {
  return new Response(
    JSON.stringify({
      type: "urn:game-narrator:error:validation",
      title: "Bad Request",
      status,
      detail: "slug must be lowercase",
      instance: "/api/v1/games",
      request_id: "req-1",
      ...extra,
    }),
    { status, headers: { "content-type": "application/problem+json", "x-request-id": "req-1" } },
  );
}

beforeEach(() => {
  globalThis.document.cookie = `gn_csrf=; expires=${new Date(0).toUTCString()}`;
});

afterEach(() => {
  cleanupCookies();
});

function cleanupCookies() {
  for (const part of globalThis.document.cookie.split(";")) {
    const name = part.split("=")[0]?.trim();
    if (name) globalThis.document.cookie = `${name}=; expires=${new Date(0).toUTCString()}`;
  }
}

describe("the CSRF rule (ADR-0016 §5)", () => {
  it.each(["GET", "POST", "PATCH", "DELETE"] as const)(
    "%s carries X-CSRF-Token exactly when the method is unsafe",
    async (method) => {
      const { client, calls } = makeClient(() => json({}));
      const call = () =>
        method === "GET"
          ? client.get("/api/v1/system/info")
          : method === "POST"
            ? client.post("/api/v1/auth/login", { body: { username: "a", password: "b" } })
            : method === "PATCH"
              ? client.patch("/api/v1/games/{id}", { params: { id: "01X" }, body: { title: "t" } })
              : client.delete("/api/v1/games/{id}", { params: { id: "01X" } });
      await call();
      const hasHeader = "x-csrf-token" in calls[0].headers;
      expect(hasHeader).toBe(method !== "GET");
      if (hasHeader) expect(calls[0].headers["x-csrf-token"]).toBe(CSRF);
    },
  );

  it("falls back to the gn_csrf twin cookie when no principal has been read yet", async () => {
    globalThis.document.cookie = `${CSRF_COOKIE_NAME}=${"cookie-token"}`;
    const { client, calls } = makeClient(() => json({}));
    client.setCsrfToken(null);
    await client.post("/api/v1/auth/logout");
    expect(calls[0].headers["x-csrf-token"]).toBe("cookie-token");
  });

  it("re-reads the token and retries once when a 403 names the CSRF rule", async () => {
    let first = true;
    const { client, calls } = makeClient(() => {
      if (first) {
        first = false;
        return problem(403, {
          type: "urn:game-narrator:error:csrf-check-failed",
          detail: "missing csrf token",
        });
      }
      return json({ id: "01X" });
    });
    await expect(
      client.post("/api/v1/games", { body: { slug: "s", title: "T" } }),
    ).resolves.toBeTruthy();
    expect(calls).toHaveLength(2);
    // The retry must not look like a second create (docs/spec/08: same key,
    // different body is a 422), so the replayed request carries no key at all.
    expect(calls[1].headers["x-csrf-token"]).toBe("refreshed-token");
    expect(calls[1].headers["idempotency-key"]).toBeUndefined();
    expect(calls[0].headers["idempotency-key"]).toBeTruthy();
  });

  it("does not retry a 403 that names a role instead of the CSRF rule", async () => {
    const { client, calls } = makeClient(() =>
      problem(403, {
        type: "urn:game-narrator:error:forbidden",
        detail: "role viewer may not call POST",
      }),
    );
    await expect(client.post("/api/v1/games", { body: { slug: "s", title: "T" } })).rejects.toThrow(
      /role viewer/,
    );
    expect(calls).toHaveLength(1);
  });

  it("hands a 401 to the auth layer once, and never retries it", async () => {
    const onUnauthenticated = vi.fn();
    const calls: Recorded[] = [];
    const client = new ApiClient({
      fetch: vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        calls.push({
          method: (init?.method ?? "GET").toUpperCase(),
          url: typeof input === "string" ? input : input.toString(),
          headers: Object.fromEntries(new Headers(init?.headers).entries()),
        });
        return problem(401, { type: "urn:game-narrator:error:session-not-found" });
      }) as unknown as typeof fetch,
      onUnauthenticated,
      csrfRefresh: vi.fn(async () => "refreshed-token"),
    });
    client.setCsrfToken(CSRF);
    await expect(
      client.post("/api/v1/games", { body: { slug: "s", title: "T" } }),
    ).rejects.toBeInstanceOf(ApiError);
    expect(calls).toHaveLength(1);
    expect(onUnauthenticated).toHaveBeenCalledTimes(1);
    expect(onUnauthenticated.mock.calls[0][0]).toMatchObject({
      status: 401,
      isUnauthenticated: true,
    });
  });
});

const CSRF_COOKIE_NAME = "gn_csrf";

describe("the Idempotency-Key rule (docs/spec/08)", () => {
  it("generates a key only for the creates that declare one", async () => {
    const { client, calls } = makeClient(() => json({}));
    await client.post("/api/v1/games", { body: { slug: "s", title: "T" } });
    await client.post("/api/v1/games/{id}/editions", {
      params: { id: "01X" },
      body: { region: "us" },
    });
    await client.post("/api/v1/auth/login", { body: { username: "a", password: "b" } });
    await client.post("/api/v1/editions/{id}/profiles", { params: { id: "01X" } });
    await client.post("/api/v1/profiles/{id}/publish", { params: { id: "01X" } });
    await client.patch("/api/v1/games/{id}", { params: { id: "01X" }, body: { title: "t" } });

    expect(calls.map((c) => Boolean(c.headers["idempotency-key"]))).toEqual([
      true,
      true,
      false,
      false,
      false,
      false,
    ]);
    expect(calls[0].headers["idempotency-key"]).not.toBe(calls[1].headers["idempotency-key"]);
  });

  it("replays a caller-supplied key unchanged, and sends none when told to omit", async () => {
    const { client, calls } = makeClient(() => json({}));
    await client.post("/api/v1/games", {
      body: { slug: "s", title: "T" },
      idempotencyKey: "caller-key",
    });
    await client.post("/api/v1/games", { body: { slug: "s", title: "T" }, idempotencyKey: null });
    expect(calls[0].headers["idempotency-key"]).toBe("caller-key");
    expect(calls[1].headers["idempotency-key"]).toBeUndefined();
  });

  it("still matches the list of operations the contract declares Idempotency-Key on", () => {
    // The client hard-codes which POSTs want a key, so the hard-coded list is the
    // thing that can go stale. Read the contract and compare, rather than trust it.
    // Required or optional: an optional key the server *honours durably* (the
    // import queue's idempotency_key column) is still one worth always sending.
    const document = contract as unknown as OpenAPILike;
    const declaring = new Set<string>();
    for (const [path, operations] of Object.entries(document.paths ?? {})) {
      for (const [method, operation] of Object.entries(operations)) {
        if (method !== "post") continue;
        const declared = (operation.parameters ?? []).some((parameter) => {
          const named = parameter.name ?? resolveRef(document, parameter.$ref).name;
          expect(typeof parameter.$ref === "string" || typeof parameter.name === "string").toBe(
            true,
          );
          return named === "Idempotency-Key";
        });
        if (declared) declaring.add(path);
      }
    }
    expect(declaring.size, "the contract still documents Idempotency-Key creates").toBeGreaterThan(
      0,
    );
    expect([...IDEMPOTENT_CREATE_TEMPLATES].sort()).toEqual([...declaring].sort());
  });

  it("produces distinct 128-bit-ish keys without a uuid dependency", () => {
    const keys = new Set(Array.from({ length: 200 }, () => newIdempotencyKey()));
    expect(keys.size).toBe(200);
    for (const key of keys) expect(key).toMatch(/^[0-9a-f-]{32,36}$/);
  });
});

interface OpenAPILike {
  paths?: Record<string, Record<string, { parameters?: { name?: string; $ref?: string }[] }>>;
  components?: { parameters?: Record<string, { name?: string; required?: boolean }> };
}

function resolveRef(
  document: OpenAPILike,
  ref: string | undefined,
): { name?: string; required?: boolean } {
  if (!ref) return {};
  const segments = ref.replace(/^#\//, "").split("/");
  let node: unknown = document;
  for (const segment of segments) {
    node = (node as Record<string, unknown>)[segment];
  }
  return (node ?? {}) as { name?: string; required?: boolean };
}

describe("ETag capture and If-Match (ADR-0015, ADR-0017)", () => {
  it("captures a profile's ETag from a read and replays it on that revision's writes", async () => {
    const { client, calls } = makeClient((record) =>
      record.method === "GET"
        ? json({ id: "01P", status: "draft" }, { headers: { etag: '"7"' } })
        : json({ id: "01P", status: "review" }),
    );
    await client.get("/api/v1/profiles/{id}", { params: { id: "01P" } });
    expect(client.etagFor(profileResourcePath("01P"))).toBe('"7"');

    await client.post("/api/v1/profiles/{id}/status", {
      params: { id: "01P" },
      body: { to: "review" },
    });
    expect(calls[1].headers["if-match"]).toBe('"7"');

    await client.patch("/api/v1/profiles/{id}/messages/{mid}", {
      params: { id: "01P", mid: "01M" },
      body: { speech_text: "x" },
    });
    expect(calls[2].headers["if-match"]).toBe('"7"');
  });

  it("carries the token across a differently-shaped read of the same revision", async () => {
    const { client, calls } = makeClient((record) =>
      record.url.includes("/messages/search")
        ? json({ query: "harbour", hits: [] }, { headers: { etag: '"4"' } })
        : json({ ok: true }),
    );
    await client.get("/api/v1/profiles/{id}/messages/search", {
      params: { id: "01P" },
      query: { q: "harbour" },
    });
    await client.post("/api/v1/profiles/{id}/status", {
      params: { id: "01P" },
      body: { to: "review" },
    });
    expect(calls[1].headers["if-match"]).toBe('"4"');
  });

  it("sends the page's token when the caller supplies one, and none outside profile writes", async () => {
    const { client, calls } = makeClient(() => json({ ok: true }));
    await client.post("/api/v1/profiles/{id}/publish", { params: { id: "01P" }, etag: '"9"' });
    expect(calls[0].headers["if-match"]).toBe('"9"');

    await client.patch("/api/v1/games/{id}", { params: { id: "01G" }, body: { title: "t" } });
    expect(calls[1].headers["if-match"]).toBeUndefined();
  });

  it("does not adopt an ETag from a failed response, so a 412 stays a reload and not a silent clobber", async () => {
    const { client, calls } = makeClient((record) =>
      record.method === "GET"
        ? json({}, { status: 200, headers: { etag: '"7"' } })
        : new Response("{}", {
            status: 412,
            headers: { etag: '"8"', "content-type": "application/json" },
          }),
    );
    await client.get("/api/v1/profiles/{id}", { params: { id: "01P" } });
    await expect(
      client.post("/api/v1/profiles/{id}/status", {
        params: { id: "01P" },
        body: { to: "review" },
      }),
    ).rejects.toThrow();
    expect(calls[1].headers["if-match"]).toBe('"7"');
    expect(client.etagFor(profileResourcePath("01P"))).toBe('"7"');
  });

  it("forgets a token on request so the next read reseeds it", async () => {
    const { client } = makeClient(() => json({}, { headers: { etag: '"2"' } }));
    await client.get("/api/v1/profiles/{id}", { params: { id: "01P" } });
    client.forgetEtag(profileResourcePath("01P"));
    expect(client.etagFor(profileResourcePath("01P"))).toBeUndefined();
  });
});

describe("problem+json failures", () => {
  it("parses code, problems and the correlation id into a typed error", async () => {
    const { client } = makeClient(() =>
      problem(422, {
        code: "CONTENT_RIGHTS_REVIEW_REQUIRED",
        problems: ["source s1 has an unknown rights status"],
      }),
    );
    const error = await client
      .post("/api/v1/profiles/{id}/publish", { params: { id: "01P" } })
      .catch((e) => e);
    expect(error).toBeInstanceOf(ApiError);
    expect(apiStatus(error)).toBe(422);
    expect(apiCode(error)).toBe("CONTENT_RIGHTS_REVIEW_REQUIRED");
    expect(error.title).toBe("Bad Request");
    expect(error.detail).toBe("slug must be lowercase");
    expect(error.requestId).toBe("req-1");
    expect(error.problems).toEqual(["source s1 has an unknown rights status"]);
  });

  it("falls back to the urn slug where the auth gate answers without a code", async () => {
    const { client } = makeClient(() =>
      problem(403, { type: "urn:game-narrator:error:forbidden" }),
    );
    const error = await client
      .post("/api/v1/games", { body: { slug: "s", title: "T" } })
      .catch((e) => e);
    expect(apiCode(error)).toBe("forbidden");
    expect(error.isCsrfFailure).toBe(false);
  });

  it("keeps a status when the body is not problem+json at all", async () => {
    const { client } = makeClient(
      () => new Response("<h1>502</h1>", { status: 502, statusText: "Bad Gateway" }),
    );
    const error = await client.get("/api/v1/games").catch((e) => e);
    expect(apiStatus(error)).toBe(502);
    expect(error.detail).toContain("502");
  });

  it("surfaces Retry-After on a rate-limited login", async () => {
    const { client } = makeClient(
      () =>
        new Response(
          JSON.stringify({
            type: "urn:game-narrator:error:login-rate-limited",
            title: "Too Many Requests",
            status: 429,
            detail: "slow down",
          }),
          {
            status: 429,
            headers: { "content-type": "application/problem+json", "retry-after": "840" },
          },
        ),
    );
    const error = await client
      .post("/api/v1/auth/login", { body: { username: "a", password: "b" } })
      .catch((e) => e);
    expect(error.retryAfter).toBe("840");
  });
});

describe("urls and bodies", () => {
  it("substitutes path parameters, escapes them, and drops empty query members", async () => {
    const { client, calls } = makeClient(() => json({ games: [] }));
    await client.get("/api/v1/profiles/{id}/messages", {
      params: { id: "01P" },
      query: { limit: 50, kind: "sign", cursor: undefined },
    });
    expect(calls[0].url).toBe(`${ORIGIN}/api/v1/profiles/01P/messages?limit=50&kind=sign`);
    await client.get("/api/v1/games/{id}", { params: { id: "a b/c" } });
    expect(calls[1].url).toBe(`${ORIGIN}/api/v1/games/a%20b%2Fc`);
  });

  it("sends JSON with a content-type, and FormData without one so fetch can set the boundary", async () => {
    const { client, calls } = makeClient(() => json({ ok: true }));
    await client.patch("/api/v1/profiles/{id}/messages/{mid}", {
      params: { id: "01P", mid: "01M" },
      body: { speech_text: "hello" },
      etag: '"1"',
    });
    expect(calls[0].headers["content-type"]).toBe("application/json");
    expect(calls[0].body).toBe(JSON.stringify({ speech_text: "hello" }));

    const form = new FormData();
    form.set("file", new Blob(["a,b\n"], { type: "text/csv" }), "s.csv");
    await client.post("/api/v1/profiles/{id}/import", { params: { id: "01P" }, form });
    expect(calls[1].headers["content-type"]).toBeUndefined();
    expect(calls[1].body).toBe(form);
  });

  it("resolves a 204 to undefined rather than trying to parse a body", async () => {
    const { client } = makeClient(() => new Response(null, { status: 204 }));
    await expect(client.post("/api/v1/auth/logout")).resolves.toBeUndefined();
  });

  it("returns the response ETag to a caller that needs to keep editing", async () => {
    const { client } = makeClient(
      () =>
        new Response(JSON.stringify({ message: { id: "01M" }, pages: [] }), {
          status: 200,
          headers: { "content-type": "application/json", etag: '"12"' },
        }),
    );
    const result = await client.withEtag("patch", "/api/v1/profiles/{id}/messages/{mid}", {
      params: { id: "01P", mid: "01M" },
      body: { speech_text: "x" },
      etag: '"11"',
    });
    expect(result.etag).toBe('"12"');
    expect(result.body).toEqual({ message: { id: "01M" }, pages: [] });
  });

  it("throws the ApiError on a 412 through withEtag, instead of resolving with the problem body", async () => {
    // Regression: withEtag once decoded whatever came back, so a stale-ETag
    // PATCH resolved with the 412 problem document. The dialogue panel then
    // stored that in the message cache and crashed on `speech_text`, unmounting
    // the whole tree instead of showing its conflict state.
    const { client } = makeClient(
      () =>
        new Response(
          JSON.stringify({
            type: "https://game-narrator.example/errors/stale-etag",
            status: 412,
            code: "stale-etag",
            title: "Revision changed",
          }),
          { status: 412, headers: { "content-type": "application/problem+json" } },
        ),
    );
    await expect(
      client.withEtag("patch", "/api/v1/profiles/{id}/messages/{mid}", {
        params: { id: "01P", mid: "01M" },
        body: { speech_text: "x" },
        etag: '"11"',
      }),
    ).rejects.toMatchObject({ status: 412, code: "stale-etag" });
  });
});

describe("the import report (the importer's own verdict)", () => {
  it("refuses to be a report when the job has no import result", () => {
    expect(jobsApi.importReport(null)).toBeNull();
    expect(
      jobsApi.importReport({ id: "j", kind: "system.noop", state: "succeeded" } as Job),
    ).toBeNull();
  });

  it("normalises a null warnings list, because the server marshals Go's nil slice as JSON null", () => {
    // The contract's ImportReport types `warnings` as a plain array, but the
    // server answers `"warnings": null` when nothing was skipped; the old
    // code read `.length` on that during render and took the whole app down
    // with it. The boundary must make the type true.
    const report = jobsApi.importReport({
      id: "j",
      kind: "import.file",
      state: "succeeded",
      result: {
        source_id: "s",
        sha256: "a".repeat(64),
        size: 1,
        media_type: "text/csv",
        adapter: "generic",
        format: "",
        stats: { rows: 40, imported: 40, skipped: 0 },
        warnings: null,
        warnings_truncated: false,
        messages_inserted: 40,
        messages_updated: 0,
        pages_written: 0,
        write_batches: 1,
        reused_source: false,
        duration: "1s",
      },
    } as unknown as Job);
    expect(report).not.toBeNull();
    expect(report?.warnings).toEqual([]);
    expect(report?.warnings.length).toBe(0);
    expect(report?.messages_inserted).toBe(40);
  });

  it("keeps real warnings intact", () => {
    const report = jobsApi.importReport({
      id: "j",
      kind: "import.file",
      state: "succeeded",
      result: {
        source_id: "s",
        sha256: "a".repeat(64),
        size: 1,
        media_type: "text/csv",
        adapter: "generic",
        format: "",
        stats: { rows: 2, imported: 1, skipped: 1 },
        warnings: [{ line: 3, msg: "duplicate id l-001" }],
        warnings_truncated: false,
        messages_inserted: 1,
        messages_updated: 0,
        pages_written: 0,
        write_batches: 1,
        reused_source: false,
        duration: "1s",
      },
    } as unknown as Job);
    expect(report?.warnings).toEqual([{ line: 3, msg: "duplicate id l-001" }]);
  });
});
