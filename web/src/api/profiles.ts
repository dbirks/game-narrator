/**
 * `src/api/profiles.ts` — the cross-revision reads the curation pages share
 * (characters, publish, play): the full message list of a revision and the
 * source rights as the revision's own manifest records them.
 *
 * The message list is cursor-paginated (hard cap 200 per page), so "all the
 * lines of a revision" is a page walk, and the walk stops on a cursor the
 * service refuses rather than looping (an opaque cursor is a service contract,
 * not a number to arithmetic on).
 *
 * The rights list comes from `GET /profiles/{id}/export` on purpose: the
 * catalogue API has no sources endpoint in this build (docs/spec/08), and the
 * export is the revision's own signed view of its sources, so the publish gate
 * reads the same bytes the portability path ships.
 */
import { api, type Message } from "./client";

/** A manifest source row, as the exporter writes it (internal/profileio). */
export interface ManifestSource {
  source_id: string;
  type: string;
  sha256: string;
  rights: string;
  parser?: string;
  uri?: string;
  mime_type?: string;
  trust?: number;
  coverage?: string;
  retrieved_at?: string;
}

/** The hard page size the service answers with; larger is a 400. */
const PAGE_LIMIT = 200;
/** A revision larger than this is a fault, not a page walk to finish. */
const SAFETY_MAX_MESSAGES = 5000;

/** Every message of one revision, page by page. */
export async function fetchAllMessages(
  profileId: string,
  signal?: AbortSignal,
): Promise<Message[]> {
  const messages: Message[] = [];
  let cursor: string | undefined;
  for (;;) {
    const page = (await api.get("/api/v1/profiles/{id}/messages", {
      params: { id: profileId },
      query: { cursor, limit: PAGE_LIMIT },
      signal,
    })) as unknown as { messages: Message[]; next_cursor?: string };
    messages.push(...(page.messages ?? []));
    cursor = page.next_cursor;
    if (!cursor || messages.length > SAFETY_MAX_MESSAGES) break;
  }
  return messages;
}

/** The revision's source rows with their rights positions (the export manifest). */
export async function fetchManifestSources(
  profileId: string,
  signal?: AbortSignal,
): Promise<ManifestSource[]> {
  const response = await fetch(`/api/v1/profiles/${encodeURIComponent(profileId)}/export`, {
    credentials: "same-origin",
    signal,
  });
  if (!response.ok) {
    let detail = `HTTP ${response.status}`;
    try {
      const problem = (await response.json()) as { detail?: string; title?: string };
      detail = problem.detail ?? problem.title ?? detail;
    } catch {
      // A body the error did not shape is still a failure the page can show.
    }
    throw new Error(`the manifest export answered ${response.status}: ${detail}`);
  }
  const manifest = (await response.json()) as { sources?: ManifestSource[] };
  return manifest.sources ?? [];
}
