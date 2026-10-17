import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";

import { catalogApi, newIdempotencyKey, type Game } from "../api/client";
import { useAuth } from "../auth/auth-context";
import { Modal } from "../components/Modal";
import { EmptyState, ErrorState, Loading } from "../components/states";

/**
 * `/library` — the games grid of docs/spec/09. Cursor pagination, not "load the
 * catalogue": the API pages by row identity (docs/spec/08) precisely so a
 * concurrent import cannot shift a page boundary, so the UI keeps a stack of the
 * cursors it has seen and walks back with it rather than guessing offsets.
 */
const PAGE_SIZE = 50;

export function LibraryPage() {
  const { atLeast } = useAuth();
  const [creating, setCreating] = useState(false);
  const queryClient = useQueryClient();

  const games = useInfiniteQuery({
    queryKey: ["games", "list"],
    queryFn: ({ pageParam, signal }) =>
      catalogApi.listGames({ limit: PAGE_SIZE, cursor: pageParam }, signal),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor,
  });

  const seen: Game[] = games.data?.pages.flatMap((page) => page.games) ?? [];

  return (
    <main>
      <header className="page-header">
        <h1>Library</h1>
        {atLeast("curator") ? (
          <button type="button" onClick={() => setCreating(true)} data-testid="new-game">
            New game
          </button>
        ) : null}
      </header>

      {games.isPending ? <Loading label="Loading the library…" /> : null}
      {games.isError ? (
        <ErrorState
          error={games.error}
          action="Cannot load the library"
          onRetry={() => void games.refetch()}
        />
      ) : null}
      {games.data && seen.length === 0 ? (
        <EmptyState title="No games yet">
          <p>
            A title family is metadata only: slug, title, platform. Editions, revisions and imported
            text are added from the game&apos;s overview page.
          </p>
        </EmptyState>
      ) : null}

      {seen.length > 0 ? (
        <>
          <ul className="cards" data-testid="game-list">
            {seen.map((game) => (
              <li key={game.id} className="card">
                <Link to={`/games/${game.id}/overview`} data-testid={`game-${game.slug}`}>
                  <strong>{game.title}</strong>
                  <span className="muted">
                    {game.slug}
                    {game.platform ? ` · ${game.platform}` : ""}
                  </span>
                </Link>
                {game.description ? <p className="muted">{game.description}</p> : null}
              </li>
            ))}
          </ul>
          <nav className="pager" aria-label="Library pages">
            <button
              type="button"
              onClick={() => void games.fetchNextPage()}
              disabled={!games.hasNextPage || games.isFetchingNextPage}
              data-testid="games-next"
            >
              {games.isFetchingNextPage ? "Loading…" : "Older games"}
            </button>
          </nav>
        </>
      ) : null}

      {creating ? (
        <NewGameDialog
          onClose={() => setCreating(false)}
          onCreated={() => {
            setCreating(false);
            void queryClient.invalidateQueries({ queryKey: ["games", "list"] });
          }}
        />
      ) : null}
    </main>
  );
}

/**
 * The create dialog. `Idempotency-Key` is generated per *attempt*, not per
 * dialog: a retry of a request the server already answered would otherwise be
 * rejected as a reuse of a key with a different body (docs/spec/08).
 */
function NewGameDialog({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [slug, setSlug] = useState("");
  const [title, setTitle] = useState("");
  const [platform, setPlatform] = useState("");

  const create = useMutation({
    mutationFn: () =>
      catalogApi.createGame({ slug, title, platform: platform || undefined }, newIdempotencyKey()),
    onSuccess: onCreated,
  });

  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    create.mutate();
  };

  return (
    <Modal open onOpenChange={(open) => !open && onClose()} title="New game">
      <p className="muted" id="new-game-help">
        Slug is the stable handle used in URLs and file names; title is what people read.
      </p>
      {create.isError ? (
        <ErrorState
          error={create.error}
          action="Cannot create the game"
          onRetry={() => {
            create.reset();
            create.mutate();
          }}
        />
      ) : null}
      <form onSubmit={onSubmit} className="stack" data-testid="new-game-form">
        <label>
          Slug
          <input
            value={slug}
            onChange={(event) => setSlug(event.target.value)}
            required
            maxLength={80}
            pattern="[a-z0-9][a-z0-9.\-]*"
            aria-describedby="new-game-help"
            data-testid="new-game-slug"
          />
        </label>
        <label>
          Title
          <input
            value={title}
            onChange={(event) => setTitle(event.target.value)}
            required
            maxLength={200}
            data-testid="new-game-title"
          />
        </label>
        <label>
          Platform
          <input
            value={platform}
            onChange={(event) => setPlatform(event.target.value)}
            maxLength={100}
            placeholder="GameCube"
            data-testid="new-game-platform"
          />
        </label>
        <button type="submit" disabled={create.isPending} data-testid="new-game-submit">
          {create.isPending ? "Creating…" : "Create game"}
        </button>
      </form>
    </Modal>
  );
}
