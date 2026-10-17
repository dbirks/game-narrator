import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Link, useParams, useSearchParams } from "react-router";

import {
  ApiError,
  api,
  catalogApi,
  messagesApi,
  profileResourcePath,
  type Message,
  type ProfileStatus,
  type TextKind,
} from "../api/client";
import { useAuth } from "../auth/auth-context";
import { EmptyState, ErrorState, Loading } from "../components/states";

/**
 * `/games/:id/dialogue` — the searchable paginated message explorer of
 * docs/spec/09. The rule it exists to obey is "don&apos;t load thousands of
 * messages into React memory" (docs/spec/09 &quot;UX robustness&quot;): one page of
 * 50 rows at a time, walked by the server&apos;s cursors, with search executed by
 * SQLite&apos;s FTS index (GET /messages/search) rather than in the browser.
 *
 * The detail panel is the only writable surface of the catalog API —
 * `speech_text` — and it is the one place where a lost write is corruption rather
 * than a nuisance, so the save carries the revision&apos;s ETag and the two
 * refusals are separate, actionable states: 412 means somebody else moved this
 * revision (reload), 409 means this revision is frozen (derive a new one).
 */
const PAGE_SIZE = 50;

const TEXT_KINDS: readonly TextKind[] = [
  "dialogue",
  "sign",
  "item",
  "menu",
  "tutorial",
  "system",
  "other",
];

export function DialoguePage() {
  const { id: gameId = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const { atLeast } = useAuth();
  const queryClient = useQueryClient();

  const profileId = params.get("profile") ?? "";
  const query = params.get("q") ?? "";
  const kind = params.get("kind") ?? "";

  // The route names a game; the script belongs to one revision of one edition.
  // With no ?profile= the page picks a revision to show (published first, then
  // the newest) and puts it in the URL, so a reload — or a link from the
  // overview — lands on the same list rather than on a spinner forever.
  const fallback = useDefaultProfile(gameId, profileId === "");
  useEffect(() => {
    if (profileId === "" && fallback) {
      const next = new URLSearchParams(params);
      next.set("profile", fallback);
      setParams(next, { replace: true });
    }
  }, [profileId, fallback, params, setParams]);

  // Cursors and the selection belong to the revision that issued them: a
  // derived revision copies the script by source identity, so the row ids
  // selected on one revision do not exist on the next, and a cursor this
  // service did not issue for a list is refused (400, docs/spec/08). Keying
  // both to the profile and checking at render time — not in an effect —
  // means the first render after a revision move already sees an empty walk
  // and no selection, so no stale request can fire against the new one.
  const [walk, setWalk] = useState<{ profileId: string; stack: string[] }>({
    profileId: "",
    stack: [],
  });
  const cursorStack = walk.profileId === profileId ? walk.stack : [];
  const cursor = cursorStack.length > 0 ? cursorStack[cursorStack.length - 1] : undefined;
  const [selection, setSelection] = useState<{
    profileId: string;
    messageId: string;
  } | null>(null);
  const selected =
    selection !== null && selection.profileId === profileId ? selection.messageId : null;
  const [etag, setEtag] = useState<string | undefined>(undefined);

  const listing = useQuery({
    queryKey: ["messages", profileId, { kind, cursor }],
    queryFn: ({ signal }) =>
      messagesApi.list(
        profileId,
        { limit: PAGE_SIZE, kind: (kind || undefined) as TextKind | undefined, cursor },
        signal,
      ),
    enabled: profileId !== "" && query === "",
  });

  const search = useQuery({
    queryKey: ["message-search", profileId, query],
    queryFn: ({ signal }) => messagesApi.search(profileId, query, PAGE_SIZE, signal),
    enabled: profileId !== "" && query !== "",
  });

  const profile = useQuery({
    queryKey: ["profile", profileId],
    queryFn: ({ signal }) => catalogApi.getProfile(profileId, signal),
    enabled: profileId !== "",
  });

  const rows: Message[] =
    query !== ""
      ? (search.data?.hits.map((hit) => hit.message) ?? [])
      : (listing.data?.messages ?? []);

  // Any read of the revision re-seeds the validator: the list carries it in the
  // body precisely so an edit does not need a round trip per keystroke.
  useEffect(() => {
    const fresh = listing.data?.profile_etag ?? api.etagFor(profileResourcePath(profileId));
    if (fresh) setEtag(fresh);
  }, [listing.data, profileId]);

  // A new filter or a new revision is a new walk; the cursors of the old one
  // would be a 400 (docs/spec/08: a token this service did not issue for this
  // list is refused, not silently reloaded).
  useEffect(() => {
    setWalk({ profileId, stack: [] });
    setSelection(null);
  }, [profileId, kind, query]);

  const loadState = query !== "" ? search : listing;

  return (
    <main className="dialogue">
      <header className="page-header">
        <h1>Dialogue</h1>
        <p className="muted">
          <Link to={`/games/${gameId}/overview`} data-testid="overview-link">
            {gameId}
          </Link>
          {profile.data ? ` · revision ${profile.data.revision_num} · ` : " · "}
          {profile.data ? (
            <span className={`badge badge-${profile.data.status}`}>{profile.data.status}</span>
          ) : null}
        </p>
      </header>

      <form
        className="filters"
        onSubmit={(event: FormEvent<HTMLFormElement>) => {
          event.preventDefault();
          const data = new FormData(event.currentTarget);
          const next = new URLSearchParams();
          next.set("profile", profileId);
          const text = String(data.get("q") ?? "").trim();
          const chosenKind = String(data.get("kind") ?? "");
          if (text) next.set("q", text);
          if (chosenKind) next.set("kind", chosenKind);
          setParams(next);
        }}
      >
        <label>
          Search this revision
          <input
            type="search"
            name="q"
            defaultValue={query}
            placeholder="server-side full-text search"
            aria-label="Search this revision"
            data-testid="dialogue-search"
          />
        </label>
        <label>
          Kind
          <select
            name="kind"
            defaultValue={kind}
            aria-label="Filter by text kind"
            data-testid="dialogue-kind"
          >
            <option value="">any</option>
            {TEXT_KINDS.map((option) => (
              <option key={option} value={option}>
                {option}
              </option>
            ))}
          </select>
        </label>
        <button type="submit" data-testid="dialogue-filter-submit">
          Apply
        </button>
        {query || kind ? (
          <button
            type="button"
            onClick={() => {
              const next = new URLSearchParams();
              next.set("profile", profileId);
              setParams(next);
            }}
          >
            Clear
          </button>
        ) : null}
      </form>

      {profileId === "" ? <Loading label="Choosing a revision to show…" /> : null}
      {loadState.isPending && profileId !== "" ? <Loading label="Loading 50 messages…" /> : null}
      {loadState.isError ? (
        <ErrorState
          error={loadState.error}
          action="Cannot load this revision's dialogue"
          onRetry={() => void loadState.refetch()}
        />
      ) : null}
      {!loadState.isPending && rows.length === 0 ? (
        <EmptyState
          title={query ? "No message matches that search" : "No messages in this revision"}
        >
          <p>
            {query
              ? "Search runs against one revision's script only; a match is a fact about that edition."
              : "Import a script from the overview page to fill it."}
          </p>
        </EmptyState>
      ) : null}

      {rows.length > 0 ? (
        <>
          <table className="messages" data-testid="message-table">
            <caption className="muted">
              {query !== ""
                ? `Search results for “${query}” (ranked, best first)`
                : `One page of ${PAGE_SIZE} messages${kind ? `, kind ${kind}` : ""}`}
            </caption>
            <thead>
              <tr>
                <th scope="col">Source id</th>
                <th scope="col">Kind</th>
                <th scope="col">Text</th>
                <th scope="col">Speaker</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((message) => (
                <tr
                  key={message.id}
                  data-testid={`message-row-${message.source_message_id ?? message.id}`}
                  aria-current={selected === message.id ? "true" : undefined}
                  className={selected === message.id ? "selected" : undefined}
                >
                  <td>
                    <button
                      type="button"
                      className="link-button"
                      onClick={() => setSelection({ profileId, messageId: message.id })}
                      data-testid={`select-${message.source_message_id ?? message.id}`}
                    >
                      {message.source_message_id ?? message.id}
                    </button>
                  </td>
                  <td>{message.text_kind}</td>
                  <td className="wrap">{message.original_text}</td>
                  <td>
                    {message.speaker_id
                      ? `${message.speaker_id} (${message.attribution_status})`
                      : "unknown"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>

          {query === "" ? (
            <nav className="pager" aria-label="Message pages">
              <button
                type="button"
                onClick={() =>
                  setWalk((w) =>
                    w.profileId === profileId ? { ...w, stack: w.stack.slice(0, -1) } : w,
                  )
                }
                disabled={cursorStack.length === 0}
                data-testid="messages-prev"
              >
                Previous
              </button>
              <span className="muted" data-testid="messages-page">
                page {cursorStack.length + 1}
              </span>
              <button
                type="button"
                onClick={() => {
                  const next = listing.data?.next_cursor;
                  if (next)
                    setWalk((w) => ({
                      profileId,
                      stack: w.profileId === profileId ? [...w.stack, next] : [next],
                    }));
                }}
                disabled={!listing.data?.next_cursor}
                data-testid="messages-next"
              >
                Next
              </button>
            </nav>
          ) : null}
        </>
      ) : null}

      {selected ? (
        <MessageDetailPanel
          profileId={profileId}
          messageId={selected}
          etag={etag}
          status={profile.data?.status ?? null}
          canWrite={atLeast("curator")}
          onEtag={setEtag}
          onChanged={() => {
            void queryClient.invalidateQueries({ queryKey: ["messages", profileId] });
            void queryClient.invalidateQueries({ queryKey: ["message", profileId, selected] });
            void queryClient.invalidateQueries({ queryKey: ["profile", profileId] });
          }}
        />
      ) : (
        <p className="muted">Select a message to read its renderings and edit its speech text.</p>
      )}
    </main>
  );
}

/**
 * The editor. Its three outcomes are the three things a curator can actually do
 * about (docs/spec/09 "manual correction"): save, discover the revision moved, or
 * discover the revision is frozen and derive a new one.
 */
function MessageDetailPanel({
  profileId,
  messageId,
  etag,
  status,
  canWrite,
  onEtag,
  onChanged,
}: {
  profileId: string;
  messageId: string;
  etag: string | undefined;
  status: ProfileStatus | null;
  canWrite: boolean;
  onEtag: (etag: string) => void;
  onChanged: () => void;
}) {
  const detail = useQuery({
    queryKey: ["message", profileId, messageId],
    queryFn: ({ signal }) => messagesApi.detail(profileId, messageId, signal),
  });
  const [draft, setDraft] = useState<string | null>(null);
  const [conflict, setConflict] = useState<"stale" | "immutable" | null>(null);
  const queryClient = useQueryClient();

  useEffect(() => {
    setDraft(null);
    setConflict(null);
  }, [messageId]);

  const save = useMutation({
    mutationFn: (text: string) => messagesApi.patchSpeechText(profileId, messageId, text, etag),
    onSuccess: (result) => {
      setConflict(null);
      // The response carries the revision's new token; keeping it is what makes
      // the next save in the same session possible without a reload.
      if (result.etag) onEtag(result.etag);
      queryClient.setQueryData(["message", profileId, messageId], result.body);
      onChanged();
    },
    onError: (error) => {
      if (error instanceof ApiError && (error.status === 412 || error.status === 428))
        setConflict("stale");
      if (error instanceof ApiError && error.status === 409) setConflict("immutable");
    },
  });

  if (detail.isPending) return <Loading label="Loading the message…" />;
  if (detail.isError) return <ErrorState error={detail.error} action="Cannot load this message" />;

  const message = detail.data.message;
  const pages = detail.data.pages;
  const value = draft ?? message.speech_text;
  const dirty = draft !== null && draft !== message.speech_text;

  return (
    <section className="panel detail" data-testid="message-detail">
      <h2>Message {message.source_message_id ?? message.id}</h2>
      <dl className="facts">
        <div>
          <dt>Original text</dt>
          <dd className="wrap" data-testid="original-text">
            {message.original_text}
          </dd>
        </div>
        <div>
          <dt>Normalized text</dt>
          <dd className="wrap" data-testid="normalized-text">
            {message.normalized_text}
          </dd>
        </div>
        <div>
          <dt>Kind</dt>
          <dd>{message.text_kind}</dd>
        </div>
        <div>
          <dt>Attribution</dt>
          <dd>
            {message.speaker_id || "unknown"} · {message.attribution_status}
            {typeof message.attribution_confidence === "number"
              ? ` (${message.attribution_confidence.toFixed(2)})`
              : ""}
          </dd>
        </div>
        <div>
          <dt>Source</dt>
          <dd>
            <code>{message.source_id ?? "manual entry"}</code>
            {message.source_message_id ? ` · ${message.source_message_id}` : ""}
          </dd>
        </div>
        <div>
          <dt>Row revision</dt>
          <dd>{message.revision}</dd>
        </div>
      </dl>

      {pages.length > 0 ? (
        <div className="pages">
          <h3>Display pages</h3>
          <ol>
            {pages.map((page) => (
              <li key={`${page.message_id}-${page.ordinal}`}>
                <span className="muted">page {page.ordinal}: </span>
                {page.page_text}
                {page.visible_prefixes.length > 0 ? (
                  <em> (prefixes: {page.visible_prefixes.join(", ")})</em>
                ) : null}
              </li>
            ))}
          </ol>
        </div>
      ) : null}

      <h3>Speech text</h3>
      {status === "published" || status === "archived" ? (
        <p className="muted" data-testid="immutable-notice">
          This revision is {status}: its text is frozen. Save will explain how to derive a new
          revision.
        </p>
      ) : null}
      <textarea
        value={value}
        rows={4}
        onChange={(event) => setDraft(event.target.value)}
        aria-label="Speech text"
        data-testid="speech-text"
      />
      <p className="actions">
        <button
          type="button"
          disabled={!dirty || save.isPending || !canWrite}
          onClick={() => save.mutate(value)}
          data-testid="speech-save"
        >
          {save.isPending ? "Saving…" : "Save"}
        </button>
        {dirty ? (
          <button type="button" onClick={() => setDraft(null)} data-testid="speech-discard">
            Discard
          </button>
        ) : null}
        {!canWrite ? (
          <span className="muted">Your role may read the script but not correct it.</span>
        ) : null}
      </p>

      {conflict === "stale" ? (
        <div role="alert" className="error" data-testid="conflict-stale">
          <p>This revision changed — reload.</p>
          <p className="muted">
            Someone else moved this revision while it was open; your text is kept below so nothing
            is lost.
          </p>
          <p className="actions">
            <button
              type="button"
              onClick={() => {
                void queryClient.invalidateQueries({ queryKey: ["messages", profileId] });
                void queryClient.invalidateQueries({ queryKey: ["message", profileId, messageId] });
                void queryClient.invalidateQueries({ queryKey: ["profile", profileId] });
                setConflict(null);
              }}
              data-testid="conflict-reload"
            >
              Reload this revision
            </button>
          </p>
        </div>
      ) : null}

      {conflict === "immutable" ? (
        <div role="alert" className="error" data-testid="conflict-immutable">
          <p>
            This revision is published or archived, so its text is frozen (docs/spec/02: what a
            session pinned must not move under it).
          </p>
          <p className="actions">
            <NewRevisionButton profileId={profileId} />
          </p>
        </div>
      ) : null}

      {save.isError && conflict === null ? (
        <ErrorState error={save.error} action="Cannot save the speech text" />
      ) : null}
    </section>
  );
}

/** Derive a draft from a frozen revision and show the dialogue at that revision. */
function NewRevisionButton({ profileId }: { profileId: string }) {
  const queryClient = useQueryClient();
  const [params, setParams] = useSearchParams();
  const [error, setError] = useState<unknown>(null);

  const derive = useMutation({
    mutationFn: () => catalogApi.createRevision(profileId),
    onSuccess: (created) => {
      void queryClient.invalidateQueries({ queryKey: ["profiles"] });
      const next = new URLSearchParams(params);
      next.set("profile", created.id);
      setParams(next);
    },
    onError: setError,
  });

  return (
    <>
      <button
        type="button"
        onClick={() => derive.mutate()}
        disabled={derive.isPending}
        data-testid="derive-revision"
      >
        {derive.isPending ? "Creating…" : "Create new revision"}
      </button>
      {derive.isError ? (
        <ErrorState error={derive.error} action="Cannot derive a revision" />
      ) : null}
      {error ? <ErrorState error={error} action="Cannot derive a revision" /> : null}
    </>
  );
}

/**
 * Pick a revision to show when the link did not name one: what an edition
 * publishes is what a player hears, so the published revision wins; otherwise the
 * highest revision number is the one a curator is working on.
 */
function useDefaultProfile(gameId: string, enabled: boolean): string | null {
  const editions = useQuery({
    queryKey: ["editions", gameId],
    queryFn: ({ signal }) => catalogApi.listEditions(gameId, signal),
    enabled,
  });
  const editionId = editions.data?.editions[0]?.id;
  const profiles = useQuery({
    queryKey: ["profiles", editionId],
    queryFn: ({ signal }) => catalogApi.listProfiles(editionId as string, signal),
    enabled: editionId !== undefined,
  });
  return useMemo(() => {
    const list = profiles.data?.profiles ?? [];
    if (list.length === 0) return null;
    const published = list.find((profile) => profile.status === "published");
    return (published ?? list[list.length - 1]).id;
  }, [profiles.data]);
}
