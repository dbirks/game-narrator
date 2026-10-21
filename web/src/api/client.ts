/**
 * `src/api/client.ts` — the typed REST client for the /api/v1 surface.
 *
 * Types come from `schema.d.ts`, generated from
 * `internal/api/openapi/openapi.json` (ADR-0017); `pnpm check:api` fails when the
 * committed types stop matching the contract, so this file cannot quietly drift
 * from the handlers.
 *
 * Three behaviours live here rather than at call sites, because each one fails in
 * a way a user experiences as "the button did nothing":
 *
 * - **CSRF.** The session cookie is `HttpOnly SameSite=Strict` and every unsafe
 *   method must echo `X-CSRF-Token` (ADR-0016 §5). The token is attached from the
 *   value `GET /auth/me` re-echoes, falling back to the non-HttpOnly `gn_csrf`
 *   twin cookie, and refreshed exactly once when a 403 names the CSRF rule.
 * - **Idempotency-Key.** Required on the creates that declare it (docs/spec/08);
 *   `client.test.ts` asserts the local list against the contract document, so an
 *   operation that starts requiring one cannot be missed here.
 * - **ETag / If-Match.** A profile revision's `row_version` is the unit of
 *   concurrency for dialogue edits (ADR-0015). Responses that carry an `ETag`
 *   update a per-profile cache and profile writes read it back, so a stale edit is
 *   a visible 412 instead of a silent overwrite.
 *
 * RFC 9457 problem+json becomes `ApiError`, which keeps `status`, the stable
 * `code` member of docs/spec/08's error contract, the finding list, and the
 * `X-Request-Id` correlation id — so a page branches on
 * `PROFILE_REVISION_CONFLICT` vs `PROFILE_IMMUTABLE` rather than on prose.
 *
 * Text is never interpreted: everything this client returns is rendered with
 * React text children (docs/spec/14 "no HTML/JS from source text").
 */

import type { components, paths } from "./schema";

/** The contract's document types. */
export type Schemas = components["schemas"];

// ── path / method typing ────────────────────────────────────────────────────

type PathTemplate = keyof paths & string;
type MethodKey = "get" | "post" | "put" | "patch" | "delete";

/** The operation object for one (path template, method) pair. */
export type Operation<T extends PathTemplate, M extends MethodKey> = M extends keyof paths[T]
  ? paths[T][M]
  : never;

/** The `{...}` placeholders of a path template, so `params` is checked per call. */
export type PathParams<T extends string> = T extends `${string}{${infer Name}}${infer Tail}`
  ? Name | PathParams<Tail>
  : never;

type QueryOf<Op> = Op extends { parameters: { query?: infer Q } } ? Q : never;
type BodyOf<Op> = Op extends { requestBody: { content: { "application/json"?: infer B } } }
  ? B
  : Op extends { requestBody?: { content?: { "application/json"?: infer B } } }
    ? B
    : never;

type OkCodes = 200 | 201 | 202 | 204 | "200" | "201" | "202" | "204";

/** The JSON body of one response object, present or optional. */
type JsonOf<R> = R extends { content: { "application/json": infer B } }
  ? B
  : R extends { content?: { "application/json"?: infer B } }
    ? B
    : never;

/** The JSON bodies this operation can answer with on a 2xx. */
export type SuccessBody<Op> = Op extends { responses: infer R }
  ? R extends Record<string | number, unknown>
    ? Extract<keyof R, OkCodes> extends infer K
      ? K extends keyof R
        ? JsonOf<R[K]>
        : never
      : never
    : never
  : never;

/** `void` for a 204 (or a body the contract declares as a bare 2xx with no JSON). */
export type ResponseFor<T extends PathTemplate, M extends MethodKey> = [
  SuccessBody<Operation<T, M>>,
] extends [never]
  ? void
  : SuccessBody<Operation<T, M>>;

// ── errors ──────────────────────────────────────────────────────────────────

/**
 * The stable codes of docs/spec/08's error contract as published in the Problem
 * schema's `code` member. `(string & {})` keeps a newer server code readable and
 * comparable instead of narrowing to `never`.
 */
export type ApiErrorCode =
  | "RESOURCE_NOT_FOUND"
  | "RESOURCE_CONFLICT"
  | "VALIDATION_FAILED"
  | "PROFILE_REVISION_CONFLICT"
  | "PROFILE_IMMUTABLE"
  | "PROFILE_TRANSITION_INVALID"
  | "CONTENT_RIGHTS_REVIEW_REQUIRED"
  | "UNKNOWN_GAME_EDITION"
  | "JOB_REQUIRES_INPUT"
  | "IDEMPOTENCY_KEY_REQUIRED"
  | "IDEMPOTENCY_KEY_REUSED"
  | "UPLOAD_TOO_LARGE"
  | "INVALID_PARAMETER"
  | "NOT_SUPPORTED"
  | "SUBSYSTEM_UNAVAILABLE"
  | "INTERNAL"
  | (string & {});

/** An HTTP failure carrying everything needed to explain it to a curator. */
export class ApiError extends Error {
  readonly status: number;
  /** Problem `code`, or the `urn:…:error:<slug>` suffix the auth gate still uses. */
  readonly code: ApiErrorCode;
  readonly title: string;
  readonly detail: string;
  /** The `X-Request-Id` correlation id (docs/spec/13). */
  readonly requestId: string;
  /** `Retry-After` from the 429, when the server sent one (docs/spec/08). */
  readonly retryAfter: string | null;
  /** Every finding, for the publish gate and the importer report. */
  readonly problems: string[];
  /** The CSRF rule rejected an otherwise authenticated call. */
  readonly isCsrfFailure: boolean;
  /** The session credential is gone: show the sign-in form, not an empty page. */
  readonly isUnauthenticated: boolean;

  constructor(init: {
    status: number;
    code: ApiErrorCode;
    title: string;
    detail: string;
    requestId?: string;
    retryAfter?: string | null;
    problems?: string[];
    isCsrfFailure?: boolean;
    isUnauthenticated?: boolean;
  }) {
    super(init.detail || init.title || `HTTP ${init.status}`);
    this.name = "ApiError";
    this.status = init.status;
    this.code = init.code;
    this.title = init.title;
    this.detail = init.detail;
    this.requestId = init.requestId ?? "";
    this.retryAfter = init.retryAfter ?? null;
    this.problems = init.problems ?? [];
    this.isCsrfFailure = init.isCsrfFailure ?? false;
    this.isUnauthenticated = init.isUnauthenticated ?? false;
  }
}

/** The HTTP status behind an error, or null when it is not an HTTP failure. */
export function apiStatus(error: unknown): number | null {
  return error instanceof ApiError ? error.status : null;
}

/** The stable error code behind an error, or null when there is not one. */
export function apiCode(error: unknown): ApiErrorCode | null {
  return error instanceof ApiError ? error.code : null;
}

/** `problem+json` as the server sent it: RFC 9457 plus `code` and `problems`. */
interface ProblemDocument {
  type?: string;
  title?: string;
  status?: number;
  detail?: string;
  instance?: string;
  request_id?: string;
  code?: string;
  problems?: string[];
}

/** The `urn:game-narrator:error:<slug>` suffix. The authorization gate and the
 *  router predate `code` and answer with only a type (see Problem's description). */
export function slugOf(type: string | undefined): string | null {
  if (!type) return null;
  const match = /^urn:game-narrator:error:(.+)$/.exec(type);
  return match ? match[1] : null;
}

/**
 * Build an ApiError from a failed response. A body that is not problem+json (a
 * proxy's error page, an empty 502) still yields a status, because "the request
 * failed" is a fact the UI must report. The body is consumed here.
 */
export async function apiErrorFrom(response: Response): Promise<ApiError> {
  const headerRequestId = response.headers.get("x-request-id") ?? "";
  const retryAfter = response.headers.get("retry-after");
  let problem: ProblemDocument | null = null;
  try {
    problem = (await response.json()) as ProblemDocument;
  } catch {
    problem = null;
  }
  const slug = slugOf(problem?.type);
  return new ApiError({
    status: response.status,
    code: problem?.code ?? slug ?? `http-${response.status}`,
    title: problem?.title ?? "Request failed",
    detail: problem?.detail ?? `HTTP ${response.status} ${response.statusText}`.trim(),
    requestId: problem?.request_id ?? headerRequestId,
    retryAfter,
    problems: Array.isArray(problem?.problems) ? problem.problems : [],
    isCsrfFailure:
      response.status === 403 && (slug === "csrf-check-failed" || slug === "origin-mismatch"),
    isUnauthenticated: response.status === 401,
  });
}

/**
 * Read a profile revision's current ETag into the client's cache.
 *
 * Profile writes are guarded by `If-Match` and a list read carries no per-row
 * validator (GET /api/v1/editions/{id}/profiles has no ETag: several revisions,
 * one token each), so a page that is about to move one revision's status reads
 * that row first. The response's ETag is captured by the client on the way
 * through, which is the whole point of doing it here rather than at the call site.
 */
export async function refreshProfileEtag(
  profileId: string,
  signal?: AbortSignal,
): Promise<string | undefined> {
  await api.get("/api/v1/profiles/{id}", { params: { id: profileId }, signal });
  return api.etagFor(profileResourcePath(profileId));
}

// ── the client ──────────────────────────────────────────────────────────────

/**
 * Path templates whose POST declares `Idempotency-Key` in the contract:
 * `createGame` and `createEdition` (required) and `importProfileFile`
 * (optional, and honoured durably by the queue). `client.test.ts` compares this
 * list against the OpenAPI document so it cannot go stale silently.
 */
export const IDEMPOTENT_CREATE_TEMPLATES: readonly string[] = [
  "/api/v1/games",
  "/api/v1/games/{id}/editions",
  "/api/v1/profiles/{id}/import",
  "/api/v1/profiles/{id}/import/disc",
];

/** The non-HttpOnly CSRF twin cookie (ADR-0016 §4). */
export const CSRF_COOKIE = "gn_csrf";

/** A fresh idempotency key. `crypto.randomUUID` when the runtime has one. */
export function newIdempotencyKey(random: () => number = defaultRandom): string {
  const cryptoObj = globalThis.crypto as Crypto | undefined;
  if (typeof cryptoObj?.randomUUID === "function") return cryptoObj.randomUUID();
  const bytes = new Uint8Array(16);
  if (cryptoObj?.getRandomValues) {
    cryptoObj.getRandomValues(bytes);
  } else {
    for (let i = 0; i < bytes.length; i += 1) bytes[i] = Math.floor(random() * 256);
  }
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

function defaultRandom(): number {
  return Math.random();
}

function readCookie(name: string): string | null {
  const jar = globalThis.document?.cookie ?? "";
  for (const part of jar.split(";")) {
    const index = part.indexOf("=");
    if (index < 0) continue;
    if (part.slice(0, index).trim() === name) return decodeURIComponent(part.slice(index + 1));
  }
  return null;
}

/** Everything under one profile shares that profile's revision token: the message
 *  list, a message row, and the status/publish writes all move the same
 *  `row_version` (internal/api/messages.go explains why it is not per-message). */
export function profileResourcePath(profileId: string): string {
  return `/api/v1/profiles/${encodeURIComponent(profileId)}`;
}

function resourceKeyOf(url: string): string {
  const path = new URL(url, originOf()).pathname;
  const match = /^(\/api\/v1\/profiles\/[^/]+)/.exec(path);
  return match ? (match[1] ?? path) : path;
}

export interface RequestOptions<Query = unknown, Body = unknown> {
  /** Values for the template's `{...}` placeholders. */
  params?: Record<string, string>;
  /** Query parameters; `undefined`, `null` and `""` members are omitted. */
  query?: Query;
  /** JSON body (never for GET). */
  body?: Body;
  /** `multipart/form-data` upload; mutually exclusive with `body`. */
  form?: FormData;
  /** Validator to send, in place of the cached one for this profile. */
  etag?: string;
  /** Key to send. Absent means "generate one" on a create; `null` means do not
   *  send the header at all, which is what a caller that knows better passes. */
  idempotencyKey?: string | null;
  signal?: AbortSignal;
}

export interface ApiClientOptions {
  fetch?: typeof fetch;
  /** Called once per 401 that names a dead or absent session. */
  onUnauthenticated?: (error: ApiError) => void;
  /** Prefix for the contract's path templates, which already start with
   *  /api/v1. Override for tests; the production default is same-origin. */
  basePath?: string;
  /** How the client re-reads the CSRF token after a 403 that names the rule. */
  csrfRefresh?: () => Promise<string | null>;
}

/** A response plus the validator it carried. */
export interface WithEtag<B> {
  body: B;
  etag?: string;
}

/**
 * A small typed fetch wrapper. Deliberately hand-written rather than
 * `openapi-fetch`: the CSRF refresh-and-retry, the per-profile ETag cache and key
 * generation on creates are the reasons a client exists in this app, and they are
 * ~250 lines instead of a dependency whose generated types we would widen anyway.
 */
export class ApiClient {
  private readonly explicitFetch?: typeof fetch;
  private readonly basePath: string;
  private csrfToken: string | null = null;
  private csrfRefresh: (() => Promise<string | null>) | null = null;
  private onUnauthorized: ((error: ApiError) => void) | undefined;
  private readonly etags = new Map<string, string>();

  constructor(options: ApiClientOptions = {}) {
    // Resolved per call, not at construction: a module-level client built during
    // an import would capture whatever fetch existed then, which in a test is
    // usually none (jsdom has no fetch until the test installs one).
    this.explicitFetch = options.fetch;
    this.basePath = options.basePath ?? "";
    this.onUnauthorized = options.onUnauthenticated;
    this.csrfRefresh = options.csrfRefresh ?? null;
  }

  /** Wire the "this session is over" reaction (the auth provider does this). */
  setUnauthenticatedHandler(handler: ((error: ApiError) => void) | undefined): void {
    this.onUnauthorized = handler;
  }

  /** The auth provider hands over the token `GET /auth/me` re-echoed. */
  setCsrfToken(token: string | null): void {
    this.csrfToken = token;
  }

  csrfTokenValue(): string | null {
    return this.csrfToken ?? readCookie(CSRF_COOKIE);
  }

  /** How the client re-reads the token after a 403 that names the CSRF rule. */
  setCsrfRefresh(refresh: () => Promise<string | null>): void {
    this.csrfRefresh = refresh;
  }

  /** The cached validator for a concrete resource path (see profileResourcePath). */
  etagFor(resourcePath: string): string | undefined {
    return this.etags.get(resourcePath);
  }

  /** Forget a validator so the next read re-seeds it (after a 412 or a reload). */
  forgetEtag(resourcePath: string): void {
    this.etags.delete(resourcePath);
  }

  buildUrl<T extends PathTemplate>(
    template: T,
    opts: { params?: Record<string, string>; query?: unknown } = {},
  ): string {
    let path = template as string;
    for (const [name, value] of Object.entries(opts.params ?? {})) {
      path = path.replace(`{${name}}`, encodeURIComponent(value));
    }
    const url = new URL(`${this.basePath}${path}`, originOf());
    for (const [name, value] of Object.entries((opts.query ?? {}) as Record<string, unknown>)) {
      if (value === undefined || value === null || value === "") continue;
      url.searchParams.set(name, String(value));
    }
    return url.toString();
  }

  get<T extends PathTemplate>(
    template: T,
    opts: RequestOptions<QueryOf<Operation<T, "get">>, never> = {},
  ): Promise<ResponseFor<T, "get">> {
    return this.request("get", template, opts) as Promise<ResponseFor<T, "get">>;
  }

  post<T extends PathTemplate>(
    template: T,
    opts: RequestOptions<QueryOf<Operation<T, "post">>, BodyOf<Operation<T, "post">>> = {},
  ): Promise<ResponseFor<T, "post">> {
    return this.request("post", template, opts) as Promise<ResponseFor<T, "post">>;
  }

  /**
   * PUT — the whole-document draft writes (the region set replace is the one
   * the catalog exposes): the body is exhaustive and If-Match rides the
   * profile's row version, exactly like the message PATCH.
   */
  put<T extends PathTemplate>(
    template: T,
    opts: RequestOptions<QueryOf<Operation<T, "put">>, BodyOf<Operation<T, "put">>> = {},
  ): Promise<ResponseFor<T, "put">> {
    return this.request("put", template, opts) as Promise<ResponseFor<T, "put">>;
  }

  patch<T extends PathTemplate>(
    template: T,
    opts: RequestOptions<QueryOf<Operation<T, "patch">>, BodyOf<Operation<T, "patch">>>,
  ): Promise<ResponseFor<T, "patch">> {
    return this.request("patch", template, opts) as Promise<ResponseFor<T, "patch">>;
  }

  delete<T extends PathTemplate>(
    template: T,
    opts: RequestOptions<QueryOf<Operation<T, "delete">>, never> = {},
  ): Promise<ResponseFor<T, "delete">> {
    return this.request("delete", template, opts) as Promise<ResponseFor<T, "delete">>;
  }

  /** The same request, plus the `ETag` the response carried.
   *
   *  Errors behave exactly like `request`: a 412 or 409 is the caller's to
   *  explain (docs/spec/09's conflict states), never a silently decoded body.
   *  Before that was true here, a stale-ETag PATCH resolved with the problem
   *  document, which the dialogue panel then stored as the message and
   *  crashed on when it rendered `speech_text` from it.
   */
  async withEtag<T extends PathTemplate, M extends MethodKey>(
    method: M,
    template: T,
    opts: RequestOptions<QueryOf<Operation<T, M>>, BodyOf<Operation<T, M>>> = {},
  ): Promise<WithEtag<ResponseFor<T, M>>> {
    const url = this.buildUrl(template, opts);
    const response = await this.finalize(method, url, template, opts);
    const etag = response.headers.get("etag") ?? undefined;
    return { body: (await decode(response)) as ResponseFor<T, M>, etag };
  }

  private async request<M extends MethodKey>(
    method: M,
    template: PathTemplate,
    opts: RequestOptions<unknown, unknown>,
  ): Promise<unknown> {
    const url = this.buildUrl(template, opts);
    const response = await this.finalize(method, url, template, opts);
    return await decode(response);
  }

  /** Send the request, then make its failures behave like failures.
   *
   *  A 403 that is a CSRF miss gets exactly one retry with a re-read token —
   *  a session that is genuinely gone answers 401, so a second 403 is a real
   *  fault, not a race. Anything else that is not ok becomes the ApiError the
   *  pages render, with its `status` and problem fields intact.
   */
  private async finalize(
    method: MethodKey,
    url: string,
    template: PathTemplate,
    opts: RequestOptions<unknown, unknown>,
  ): Promise<Response> {
    let response = await this.exchange(method, url, template, opts, false);
    if (response.status === 403) {
      const error = await apiErrorFrom(response);
      if (error.isCsrfFailure && this.csrfRefresh) {
        this.csrfToken = await this.csrfRefresh();
        response = await this.exchange(method, url, template, opts, true);
      } else {
        throw error;
      }
    }
    if (!response.ok) {
      throw await apiErrorFrom(response);
    }
    return response;
  }

  private exchange<M extends MethodKey>(
    method: M,
    url: string,
    template: PathTemplate,
    opts: RequestOptions<unknown, unknown>,
    isRetry: boolean,
  ): Promise<Response> {
    const verb = method.toUpperCase();
    const unsafe = verb !== "GET" && verb !== "HEAD";
    const headers: Record<string, string> = { accept: "application/json" };

    if (unsafe) {
      const token = this.csrfTokenValue();
      if (token) headers["X-CSRF-Token"] = token;
    }
    // A retry must not look like a second create: docs/spec/08 answers "same key,
    // different body" with 422, and a dropped CSRF token is not a new request.
    if (
      verb === "POST" &&
      IDEMPOTENT_CREATE_TEMPLATES.includes(template) &&
      !isRetry &&
      opts.idempotencyKey !== null
    ) {
      headers["Idempotency-Key"] = opts.idempotencyKey || newIdempotencyKey();
    } else if (opts.idempotencyKey) {
      headers["Idempotency-Key"] = opts.idempotencyKey;
    }

    // Profile writes are the ones the store guards with If-Match (ADR-0015).
    // Games and editions deliberately have no row version (docs/spec/08).
    if (unsafe && template.startsWith("/api/v1/profiles/")) {
      const cached = opts.etag ?? this.etags.get(resourceKeyOf(url));
      if (cached) headers["If-Match"] = cached;
    }

    let body: BodyInit | undefined;
    if (opts.form !== undefined) {
      body = opts.form; // fetch computes the multipart Content-Type and boundary
    } else if (opts.body !== undefined) {
      headers["content-type"] = "application/json";
      body = JSON.stringify(opts.body);
    }

    const requestInit: RequestInit = {
      method: verb,
      credentials: "same-origin",
      headers,
      signal: opts.signal,
    };
    if (body !== undefined) requestInit.body = body;

    const doFetch = this.explicitFetch ?? globalThis.fetch.bind(globalThis);
    return doFetch(url, requestInit).then((response) => {
      const etag = response.headers.get("etag");
      if (etag && response.ok) this.etags.set(resourceKeyOf(url), etag);
      if (response.status === 401 && !isRetry) {
        // A dead cookie must stop being resent, and the app must show the form
        // rather than an empty library (ADR-0016 follow-ups).
        this.csrfToken = null;
        this.onUnauthorized?.(
          new ApiError({
            status: 401,
            code: "unauthenticated",
            title: "Unauthorized",
            detail: "the session is gone; sign in again",
            requestId: response.headers.get("x-request-id") ?? "",
            isUnauthenticated: true,
          }),
        );
      }
      return response;
    });
  }
}

async function decode(response: Response): Promise<unknown> {
  if (response.status === 204) return undefined;
  const text = await response.text();
  if (text === "") return undefined;
  return JSON.parse(text) as unknown;
}

function originOf(): string {
  return globalThis.location?.origin ?? "http://127.0.0.1";
}

/** The app-wide client. The auth provider wires its two hooks. */
export const api = new ApiClient();

// ── domain aliases ──────────────────────────────────────────────────────────

export type User = Schemas["User"];
export type Role = Schemas["Role"];
export type Game = Schemas["Game"];
/** The /library screen's per-game read (ADR-0109): the actionable revision's identity plus its counts. */
export type GameLibrary = Schemas["GameLibrary"];
export type Edition = Schemas["Edition"];
export type Character = Schemas["Character"];
export type SpeakerLinkResponse = Schemas["SpeakerLinkResponse"];
export type Profile = Schemas["Profile"];
export type ProfileStatus = Schemas["ProfileStatus"];
export type Message = Schemas["Message"];
export type MessageDetail = Schemas["MessageDetail"];
export type MessagePage = Schemas["MessagePage"];
export type MessageHit = Schemas["MessageHit"];
export type PublishAction = Schemas["PublishAction"];
export type Job = Schemas["Job"];
export type JobDetail = Schemas["JobDetail"];
export type JobState = Schemas["JobState"];
export type ImportAccepted = Schemas["ImportAccepted"];
export type ImportReport = Schemas["ImportReport"];
export type DiscImages = Schemas["DiscImages"];
export type DiscImportAccepted = Schemas["DiscImportAccepted"];
export type DiscImportReport = Schemas["DiscImportReport"];
export type TextKind = Schemas["TextKind"];
export type RightsStatus = Schemas["RightsStatus"];
export type ProfileManifest = Schemas["ProfileManifest"];
export type SystemInfo = Schemas["SystemInfo"];

/**
 * The 201 body of POST /api/v1/editions/{id}/profiles/import-manifest.
 * `sources_needing_bytes` names the source bytes not on this appliance — the
 * documented difference between "imported" and "imported and re-parseable"
 * (docs/spec/02), and the report echoes the manifest's own digest so the
 * screen can say what it imported.
 */
export interface ManifestImportResult {
  profile: Profile;
  sources_needing_bytes: string[];
  report: {
    profile_id: string;
    edition_id: string;
    game_id: string;
    created_catalog: boolean;
    messages_written: number;
    messages_withheld: number;
    sources_registered: number;
    missing_blobs: string[];
    present_blobs: string[];
    digest: string;
    preferred_voice_id?: string;
  };
}

/** The contract's HealthStatus, with `checks` spelled out: it is a free-form
 *  dependency map (`additionalProperties: {type: object}`) rather than a schema. */
export interface Readiness {
  status: string;
  checks: Record<string, Record<string, unknown>>;
}

export interface AuthStatus {
  setup_required: boolean;
  loopback_setup: boolean;
}

export interface AuthMe {
  user: User;
  csrf_token?: string;
  auth_method: "cookie" | "token";
  expires_at?: string;
  grants?: Record<string, string>;
}

export interface LoginResult {
  user: User;
  csrf_token: string;
  expires_at: string;
  idle_expires_at: string;
}

export interface SetupResult {
  user: User;
  message: string;
}

/** One page of a message list: the contract's `MessageList` plus its cursor. */
export interface MessagePageResult {
  messages: Message[];
  profile_etag: string;
  next_cursor?: string;
}

export interface MessageSearchResult {
  query: string;
  hits: MessageHit[];
}

// ── endpoint helpers ────────────────────────────────────────────────────────

export const authApi = {
  status(signal?: AbortSignal): Promise<AuthStatus> {
    return api.get("/api/v1/auth/status", { signal }) as Promise<AuthStatus>;
  },
  me(signal?: AbortSignal): Promise<AuthMe> {
    return api.get("/api/v1/auth/me", { signal }) as Promise<AuthMe>;
  },
  async login(username: string, password: string): Promise<LoginResult> {
    const result = (await api.post("/api/v1/auth/login", {
      body: { username, password },
    })) as LoginResult;
    api.setCsrfToken(result.csrf_token);
    return result;
  },
  setup(username: string, password: string): Promise<SetupResult> {
    return api.post("/api/v1/auth/setup", { body: { username, password } }) as Promise<SetupResult>;
  },
  async logout(): Promise<void> {
    await api.post("/api/v1/auth/logout");
    api.setCsrfToken(null);
  },
};

export const catalogApi = {
  listGames(query?: { cursor?: string; limit?: number }, signal?: AbortSignal) {
    return api.get("/api/v1/games", { query, signal });
  },
  getGame(id: string, signal?: AbortSignal) {
    return api.get("/api/v1/games/{id}", { params: { id }, signal });
  },
  /** The Library's shelf tile and list row read (ADR-0109): the actionable
   *  revision (published preferred, else the newest revision, else absent) plus
   *  the lines / characters / voiced counts of that revision. */
  gameLibrary(id: string, signal?: AbortSignal): Promise<GameLibrary> {
    return api.get("/api/v1/games/{id}/library", {
      params: { id },
      signal,
    }) as Promise<GameLibrary>;
  },
  createGame(
    body: {
      slug: string;
      title: string;
      description?: string;
      platform?: string;
      user_tags?: string[];
    },
    idempotencyKey?: string,
  ) {
    return api.post("/api/v1/games", { body, idempotencyKey });
  },
  listEditions(gameId: string, signal?: AbortSignal) {
    return api.get("/api/v1/games/{id}/editions", { params: { id: gameId }, signal });
  },
  createEdition(
    gameId: string,
    body: {
      region?: string;
      language?: string;
      revision?: string;
      source_identity?: string;
      locale?: string;
      compatibility_notes?: string;
    },
    idempotencyKey?: string,
  ) {
    return api.post("/api/v1/games/{id}/editions", {
      params: { id: gameId },
      body,
      idempotencyKey,
    });
  },
  listProfiles(editionId: string, signal?: AbortSignal) {
    return api.get("/api/v1/editions/{id}/profiles", { params: { id: editionId }, signal });
  },
  /** The edition's published revision; a 404 (code `not-published`) means none. */
  getPublished(editionId: string, signal?: AbortSignal) {
    return api.get("/api/v1/editions/{id}/published", { params: { id: editionId }, signal });
  },
  /** The roster of one revision (always an array; empty, never null). */
  getCharacters(profileId: string, signal?: AbortSignal) {
    return api.get("/api/v1/profiles/{id}/characters", {
      params: { id: profileId },
      signal,
    }) as Promise<{ characters: Character[] }>;
  },
  /**
   * The curator's speaker-link action (ADR-0070, extended by ADR-0135): the
   * placeholder {characterId} is identified as an existing roster row
   * (`target_character_id`) or as a character by `name` — the existing
   * non-retired row of that exact name when one exists, a new confirmed
   * character created in the same transaction when it does not. One of the
   * two is required; both given must name the same row. The client sends the
   * profile's cached ETag as If-Match, so a stale page is a 412, not a second
   * link.
   */
  linkSpeaker(
    profileId: string,
    characterId: string,
    body: { target_character_id?: string; name?: string },
    signal?: AbortSignal,
  ) {
    return api.post("/api/v1/profiles/{id}/characters/{cid}/speaker-link", {
      params: { id: profileId, cid: characterId },
      body,
      signal,
    }) as Promise<SpeakerLinkResponse>;
  },
  createDraft(editionId: string) {
    return api.post("/api/v1/editions/{id}/profiles", { params: { id: editionId } });
  },
  /**
   * Re-import a `.gnprofile.json` export (docs/spec/02 "Profile portability",
   * docs/spec/00 "exports/reimports profile manifest"): the server validates
   * the schema, re-verifies the manifest's own digest against the document's
   * bytes, and writes a brand-new draft — never an overwrite (ADR-0015).
   *
   * The file's bytes must cross the wire unchanged, which is why this bypasses
   * the JSON (re)serialisation the generic client applies: a re-encoded
   * document no longer hashes to its own manifest_digest and the import
   * answers 422. The File goes straight into the fetch body.
   */
  async importManifest(
    editionId: string,
    file: File,
    signal?: AbortSignal,
  ): Promise<ManifestImportResult> {
    const url = api.buildUrl("/api/v1/editions/{id}/profiles/import-manifest", {
      params: { id: editionId },
    });
    const headers: Record<string, string> = {
      accept: "application/json",
      "content-type": "application/json",
    };
    const token = api.csrfTokenValue();
    if (token) headers["x-csrf-token"] = token;
    const response = await fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers,
      body: file,
      signal,
    });
    if (!response.ok) throw await apiErrorFrom(response);
    return (await response.json()) as ManifestImportResult;
  },
  getProfile(id: string, signal?: AbortSignal) {
    return api.get("/api/v1/profiles/{id}", { params: { id }, signal });
  },
  createRevision(id: string) {
    return api.post("/api/v1/profiles/{id}/revisions", { params: { id } });
  },
  listPublishActions(id: string, signal?: AbortSignal) {
    return api.get("/api/v1/profiles/{id}/publish-actions", { params: { id }, signal });
  },
  /** The per-profile auto-cast dial (ADR-0109, UX-03): on by default; an
   *  audition run created for one of the profile's characters auto-approves
   *  its take when the ASR QA read-back is not failed, the consent attributed
   *  to the profile-level default. If-Match like the other profile writes.
   *  The ETag is refreshed first (the dial's write does not bump the row
   *  version, but it answers against the revision the caller last read). */
  async setAutoCast(id: string, enabled: boolean, signal?: AbortSignal) {
    const etag = (await refreshProfileEtag(id, signal)) ?? "";
    return api.post("/api/v1/profiles/{id}/auto-cast", {
      params: { id },
      body: { enabled },
      etag,
      signal,
    });
  },
  /** The profile's narrator fallback dial (ADR-0101): on by default; with it
   *  on, a line whose speaker has no approved voice speaks through the
   *  narrator voice. If-Match like the other profile writes — the ETag is
   *  refreshed first (the dial's write does not bump the row version, but it
   *  answers against the revision the caller last read). */
  async setNarratorFallback(id: string, enabled: boolean, signal?: AbortSignal) {
    const etag = (await refreshProfileEtag(id, signal)) ?? "";
    return api.post("/api/v1/profiles/{id}/narrator-fallback", {
      params: { id },
      body: { enabled },
      etag,
      signal,
    });
  },
  /** Status moves and publication are If-Match writes (ADR-0015). */
  setStatus(id: string, to: ProfileStatus, etag?: string) {
    return api.post("/api/v1/profiles/{id}/status", { params: { id }, body: { to }, etag });
  },
  publish(id: string, etag?: string) {
    return api.post("/api/v1/profiles/{id}/publish", { params: { id }, etag });
  },
};

export const messagesApi = {
  list(
    profileId: string,
    query?: { kind?: TextKind; scene?: string; speaker?: string; cursor?: string; limit?: number },
    signal?: AbortSignal,
  ): Promise<MessagePageResult> {
    return api.get("/api/v1/profiles/{id}/messages", {
      params: { id: profileId },
      query,
      signal,
    }) as Promise<MessagePageResult>;
  },
  search(
    profileId: string,
    q: string,
    limit?: number,
    signal?: AbortSignal,
  ): Promise<MessageSearchResult> {
    return api.get("/api/v1/profiles/{id}/messages/search", {
      params: { id: profileId },
      query: { q, limit },
      signal,
    }) as Promise<MessageSearchResult>;
  },
  detail(profileId: string, mid: string, signal?: AbortSignal) {
    return api.get("/api/v1/profiles/{id}/messages/{mid}", {
      params: { id: profileId, mid },
      signal,
    });
  },
  /** The write whose 412/409 the dialogue editor has to explain (docs/spec/09). */
  patchSpeechText: async (
    profileId: string,
    mid: string,
    speechText: string,
    etag?: string,
  ): Promise<WithEtag<MessageDetail>> =>
    api.withEtag("patch", "/api/v1/profiles/{id}/messages/{mid}", {
      params: { id: profileId, mid },
      body: { speech_text: speechText },
      etag,
    }),
};

/** The generic adapter's column mapping: which source key or header names the
 * message's fields. Empty means "the adapter's own defaults" (Text/ID/…),
 * which is why every member is optional on the wire. */
export interface ColumnMapping {
  text?: string;
  id?: string;
  speaker?: string;
  kind?: string;
  scene?: string;
  page?: string;
}

const COLUMN_FIELDS: Record<keyof ColumnMapping, string> = {
  text: "column_text",
  id: "column_id",
  speaker: "column_speaker",
  kind: "column_kind",
  scene: "column_scene",
  page: "column_page",
};

export const portabilityApi = {
  importFile(
    profileId: string,
    file: File,
    options: {
      rights?: RightsStatus;
      format?: string;
      defaultKind?: TextKind;
      columns?: ColumnMapping;
    } = {},
  ) {
    const form = new FormData();
    form.set("file", file);
    form.set("rights", options.rights ?? "user_supplied");
    if (options.format) form.set("format", options.format);
    if (options.defaultKind) form.set("default_kind", options.defaultKind);
    // A file whose headers or keys do not match the adapter's defaults names
    // its columns here (docs/spec/04: mapping by configuration, not by a
    // condition in shared code). Blank values are omitted so the defaults
    // stay the defaults.
    for (const [name, value] of Object.entries(options.columns ?? {})) {
      if (value) form.set(COLUMN_FIELDS[name as keyof ColumnMapping], value);
    }
    return api.post("/api/v1/profiles/{id}/import", { params: { id: profileId }, form });
  },
  /** A plain link: the browser carries the session cookie and names the file. */
  exportUrl(profileId: string): string {
    return `/api/v1/profiles/${encodeURIComponent(profileId)}/export`;
  },
};

/**
 * The disc-import surface (ADR-0066): the operator's allowlisted directory holds
 * the user's disc images, the UI chooses one by basename (or uploads a small
 * image), and the parse runs as the import.disc job whose report is read back
 * the way the file import's is.
 */
export const discImportApi = {
  /** What the allowlisted directory holds. 200 with configured:false when the operator configured none. */
  listImages(signal?: AbortSignal) {
    return api.get("/api/v1/import/disc/images", { signal });
  },
  /** Dir shape: a basename from the allowlisted directory, read in place. */
  importDir(profileId: string, image: string) {
    return api.post("/api/v1/profiles/{id}/import/disc", {
      params: { id: profileId },
      body: { image },
    });
  },
  /** Upload shape: the raw image (CISO or plain GCM) streamed to the asset store. */
  importUpload(profileId: string, file: File) {
    const form = new FormData();
    form.set("file", file);
    return api.post("/api/v1/profiles/{id}/import/disc", {
      params: { id: profileId },
      form,
    });
  },
  /** The disc report is the `result` member of a succeeded `import.disc` job. */
  report(job: Job | JobDetail | null | undefined): DiscImportReport | null {
    const result = job?.result as Record<string, unknown> | undefined;
    if (!result || typeof result !== "object" || !("messages_inserted" in result)) return null;
    // The Go nil-slice → JSON null normalisation the file import applies; a
    // report with zero warnings must not crash the render that reads .length.
    return { ...result, warnings: result.warnings ?? [] } as unknown as DiscImportReport;
  },
};

export const jobsApi = {
  get(id: string, signal?: AbortSignal) {
    return api.get("/api/v1/jobs/{id}", { params: { id }, signal });
  },
  /** The import report is the `result` member of a succeeded `import.file` job. */
  importReport(job: Job | JobDetail | null | undefined): ImportReport | null {
    const result = job?.result as Record<string, unknown> | undefined;
    if (!result || typeof result !== "object" || !("messages_inserted" in result)) return null;
    // The server marshals Go's nil slice as JSON null, which the contract's
    // array type does not admit: a report with zero warnings arrives as
    // `warnings: null`, and reading `.length` on it during render would
    // crash the whole tree. Normalise at the boundary so every consumer sees
    // the type the contract promises.
    return { ...result, warnings: result.warnings ?? [] } as unknown as ImportReport;
  },
};
