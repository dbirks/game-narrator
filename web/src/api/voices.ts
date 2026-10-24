/**
 * `src/api/voices.ts` — the voice setup facade (docs/spec/07, docs/spec/08).
 *
 * The JSON operations go through the shared `api` client (CSRF retry, problem
 * decoding, generated types). The audio operations are the one surface the
 * shared client does not cover: its `decode` reads every body as JSON, and a
 * preview or audition clip is a WAV body whose only structured members are the
 * headers — `ETag` is the content identity and `X-Cache` (preview) is the
 * speech-cache decision the page shows. They fetch the same URL the client
 * builds, with the same credentials and the same CSRF token, and return the
 * blob plus those headers.
 */

import { api, ApiError, apiErrorFrom } from "./client";

type Schemas = import("./schema").components["schemas"];

export type VoiceRevision = Schemas["VoiceRevision"];
export type VoiceDetail = Schemas["VoiceDetail"];
export type VoiceAudition = Schemas["VoiceAudition"];
export type VoiceRegistration = Schemas["VoiceRegistration"];
export type CastAuditRow = Schemas["CastAuditRow"];
export type KindRow = Schemas["KindRow"];

/**
 * The cast audit (ADR-0171): each roster row's expected vs measured voice
 * class, measured from the assigned voice's reference with the YIN estimator.
 * `verdict` is "OK" | "MISMATCH" | "UNKNOWN".
 */
export async function castAudit(
  profileId: string,
  signal?: AbortSignal,
): Promise<{ profile: string; rows: CastAuditRow[] }> {
  return api.get("/api/v1/profiles/{id}/cast/audit", { params: { id: profileId }, signal });
}

/**
 * The generic kind voices (ADR-0158, issue #51): the seven `kind:*` roster
 * keys an unnamed speaker of a confident apparent kind speaks with. Coverage
 * carries one row per kind — missing / pending / approved, the take's
 * measured median F0 and its pitch class.
 */
export async function kindVoices(signal?: AbortSignal): Promise<{ kinds: KindRow[] }> {
  return api.get("/api/v1/voices/kinds", { signal });
}

/**
 * The one-click "Create missing" (ADR-0185): auditions each missing kind with
 * its fixed brief, measures the take's pitch, retries up to 2 times while the
 * class is wrong, and leaves the passing takes PENDING for the operator's
 * approve click. Synchronous — the answer is the coverage after the run.
 */
export async function createMissingKindVoices(signal?: AbortSignal): Promise<{ kinds: KindRow[] }> {
  return api.post("/api/v1/voices/kinds/create-missing", { body: {}, signal });
}

/** One audition clip's audio, addressed by its content identity. */
export interface AuditionAudio {
  blob: Blob;
  sha256: string;
}

/** A preview's audio plus the cache decision the server made. */
export interface PreviewResult {
  blob: Blob;
  sha256: string;
  /** "HIT" | "MISS" | null when the server did not mark the decision. */
  cache: "HIT" | "MISS" | null;
}

async function audioFetch(url: string, init: RequestInit): Promise<Response> {
  const response = await fetch(url, {
    credentials: "same-origin",
    ...init,
  });
  if (response.status === 401) {
    throw new ApiError({
      status: 401,
      code: "unauthenticated",
      title: "Unauthorized",
      detail: "the session is gone; sign in again",
      isUnauthenticated: true,
    });
  }
  if (!response.ok) {
    throw await apiErrorFrom(response);
  }
  return response;
}

function etagSha(etag: string | null): string {
  // The server sets the ETag to the audio's SHA-256 in quotes.
  return (etag ?? "").replace(/^"|"$/g, "");
}

export const voicesApi = {
  listVoices(character?: string, signal?: AbortSignal) {
    return api.get("/api/v1/voices", {
      query: character ? { character } : undefined,
      signal,
    });
  },

  getVoice(id: string, signal?: AbortSignal) {
    return api.get("/api/v1/voices/{id}", { params: { id }, signal });
  },

  /** 202: the draft revision exists; the clips arrive when the job runs.
   *  With `profile_id` (ADR-0109, UX-03) the run is the cast's: the character
   *  is checked against the profile's roster, one take is the default when the
   *  profile's auto_cast dial is on, and the job auto-approves the take after
   *  its ASR QA read-back (consent attributed to the profile default). */
  createAuditions(
    body: { character: string; brief: string; count?: number; profile_id?: string },
    signal?: AbortSignal,
  ) {
    return api.post("/api/v1/voices/auditions", { body, signal });
  },

  /** One-step undo (ADR-0109, UX-03d): the named revision retires and the
   *  character's previous approved take becomes current again; both keeps
   *  its audio. A second undo is a 409 NOTHING_TO_UNDO. */
  undoVoice(id: string, signal?: AbortSignal) {
    return api.post("/api/v1/voices/{id}/undo", { params: { id }, signal });
  },

  approveVoice(id: string, body: { audition_id: string; consent: string }, signal?: AbortSignal) {
    return api.post("/api/v1/voices/{id}/approve", { params: { id }, body, signal });
  },

  registerVoice(id: string, signal?: AbortSignal) {
    return api.post("/api/v1/voices/{id}/register", { params: { id }, signal });
  },

  listRegistrations(id: string, signal?: AbortSignal) {
    return api.get("/api/v1/voices/{id}/registrations", { params: { id }, signal });
  },

  /** One audition clip as audio/wav (its SHA-256 is the ETag). */
  async auditionAudio(id: string, auditionId: string): Promise<AuditionAudio> {
    const url = api.buildUrl("/api/v1/voices/{id}/auditions/{aid}", {
      params: { id, aid: auditionId },
    });
    const response = await audioFetch(url, { method: "GET" });
    return {
      blob: await response.blob(),
      sha256: etagSha(response.headers.get("etag")),
    };
  },

  /** One preview line as audio/wav, with the X-Cache decision. */
  async previewVoice(id: string, text: string): Promise<PreviewResult> {
    const url = api.buildUrl("/api/v1/voices/{id}/preview", { params: { id } });
    const headers: Record<string, string> = { "content-type": "application/json" };
    const token = api.csrfTokenValue();
    if (token) headers["X-CSRF-Token"] = token;
    const response = await audioFetch(url, {
      method: "POST",
      headers,
      body: JSON.stringify({ text }),
    });
    const cache = response.headers.get("x-cache");
    return {
      blob: await response.blob(),
      sha256: etagSha(response.headers.get("etag")),
      cache: cache === "HIT" || cache === "MISS" ? cache : null,
    };
  },
};
