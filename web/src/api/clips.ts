/**
 * `src/api/clips.ts` — the managed replay clip store (ADR-0073).
 *
 * The surface is developer-scoped (the scope table grants
 * /api/v1/replay/* at the developer tier): a clip is a recording of gameplay
 * frames, the same evidence class as the dev-trace store, so the UI offers it
 * where developer surfaces live. The upload is multipart/form-data (the
 * browser's shape): the file part carries the archive (tar or zip), the
 * optional name field is the clip id. The response's detail sentence carries
 * the frame_source a session starts with; the Play page's source picker is
 * the product path to it.
 */

import { api } from "./client";

type Schemas = import("./schema").components["schemas"];

/** One clip installed under the managed store. */
export type Clip = Schemas["Clip"];
/** The 201 upload answer: the clip's record plus the detail sentence. */
export type ClipUploaded = Schemas["ClipUploaded"];
/** The store as listed. */
export type ClipList = Schemas["ClipList"];

const CLIPS = "/api/v1/replay/clips";

export const clipsApi = {
  /** The store's clips, in index order. */
  list(signal?: AbortSignal) {
    return api.get(CLIPS, { signal }) as Promise<ClipList>;
  },

  /**
   * Upload one archive. `name` is the clip id when given (a lowercase slug);
   * without it the id comes from the archive's top-level directory, then the
   * filename, then a hash-derived fallback. One archive per request.
   */
  upload(file: File, name: string, signal?: AbortSignal) {
    const form = new FormData();
    form.set("file", file);
    if (name.trim() !== "") form.set("name", name.trim());
    return api.post(CLIPS, { form, signal }) as Promise<ClipUploaded>;
  },

  /** Remove one clip; the id must be a valid clip id (a lowercase slug). */
  remove(id: string, signal?: AbortSignal) {
    return api.delete(`${CLIPS}/{id}`, {
      params: { id },
      signal,
    }) as Promise<void>;
  },
};
