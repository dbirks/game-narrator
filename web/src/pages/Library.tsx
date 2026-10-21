import { useInfiniteQuery, useMutation, useQueries, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate } from "react-router";

import { catalogApi, newIdempotencyKey, type Game, type GameLibrary } from "../api/client";
import { sessionsApi } from "../api/sessions";
import { useAuth } from "../auth/auth-context";
import {
  Chip,
  DitherCover,
  PillButton,
  ProgressBar,
  readSegmented,
  Segmented,
  Table,
  type TableColumn,
} from "../components/kit";
import { Modal } from "../components/Modal";
import { EmptyState, ErrorState, Loading } from "../components/states";
import { useLiveStatus } from "../lib/liveStatus";

/**
 * `/library` — the design's 2a (docs/design/plum-peach; ADR-0140): a hero
 * card over the "All games" shelf, with the ⌘K pill and the persisted
 * shelf / list toggle (UX-08).
 *
 * Two data feeds, joined in the view layer:
 *
 * - The catalogue's page of games (cursor pagination, docs/spec/08) — the
 *   rows the shelf and list render.
 * - One per-game library read (`GET /api/v1/games/{id}/library`) each: the
 *   actionable revision (the published one a session pins, else the newest
 *   revision a setup run is filling, else nothing) plus its lines /
 *   characters / voiced counts.
 *
 * The progress on a setting-up tile comes from the live status snapshot's
 * `setup_runs` (UX-07): the same numbers the status bar renders, matched to
 * a game by title, so the bar and the tile cannot disagree.
 *
 * The hero slot (ADR 0093, open question (e)): a game with a published
 * profile takes it; a game still setting up takes it only when no ready
 * game exists.
 */
const PAGE_SIZE = 50;

/** UX-08: the view choice persists per user under this key. */
const VIEW_STORAGE_KEY = "plum.library.view";

type LibraryView = "shelf" | "list";

const VIEW_OPTIONS: ReadonlyArray<{ value: LibraryView; label: string }> = [
  { value: "shelf", label: "Shelf" },
  { value: "list", label: "List" },
];

export function LibraryPage() {
  const { atLeast } = useAuth();
  const canCurate = atLeast("curator");
  const canOperate = atLeast("operator");
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [view, setView] = useState<LibraryView>(() =>
    readSegmented(VIEW_OPTIONS, VIEW_STORAGE_KEY, "shelf"),
  );

  const games = useInfiniteQuery({
    queryKey: ["games", "list"],
    queryFn: ({ pageParam, signal }) =>
      catalogApi.listGames({ limit: PAGE_SIZE, cursor: pageParam }, signal),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor,
  });

  const seen: Game[] = games.data?.pages.flatMap((page) => page.games) ?? [];

  // One library read per visible game (ADR-0140): the actionable revision
  // and its counts. Keyed by game id so the react-query cache keeps them
  // across the view toggle and reloads within the query window.
  const libraryReads = useQueries({
    queries: seen.map((game) => ({
      queryKey: ["game-library", game.id],
      queryFn: ({ signal }: { signal: AbortSignal }) => catalogApi.gameLibrary(game.id, signal),
      staleTime: 5_000,
    })),
  });
  const libraries = new Map<string, GameLibrary | undefined>();
  seen.forEach((game, index) => {
    const read = libraryReads[index]?.data;
    if (read !== undefined) libraries.set(game.id, read);
  });

  // The status feed's setup run (UX-07): matched to a game by title — the
  // run names the game the bar already reads it.
  const { snapshot } = useLiveStatus();
  const setupRun = snapshot?.setup_runs?.[0];
  const setupGameTitle = setupRun?.game;
  const isSettingUp = (game: Game): boolean =>
    setupGameTitle !== undefined &&
    setupGameTitle !== "" &&
    game.title.toLowerCase() === setupGameTitle.toLowerCase();

  const startSession = useStartSession(navigate);

  // The hero: the ready game (newest publication); only when the library has
  // no ready game does the setting-up one take the slot (ADR 0093 (e)).
  const hero = chooseHero(seen, libraries, setupGameTitle);

  return (
    <main>
      <header className="page-header">
        <h1>Library</h1>
        {canCurate ? (
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
          {hero === null ? (
            <div className="lib-hero lib-hero-loading" data-testid="lib-hero-loading" />
          ) : (
            <HeroCard
              game={hero.game}
              library={libraries.get(hero.game.id)}
              settingUp={isSettingUp(hero.game)}
              setupRun={setupRun}
              canOperate={canOperate}
              onStart={(profileId) => startSession.start(profileId)}
              starting={startSession.pendingProfileId !== undefined}
            />
          )}

          <div className="lib-all-games" data-testid="lib-all-games-row">
            <h2>All games</h2>
            <PillButton
              variant="outline"
              data-testid="lib-kbd-pill"
              aria-label="Search anything"
              onClick={() => {
                // The shell's global ⌘K handler owns the palette (Phase 0
                // stub; item 2 lands the results): the pill dispatches the
                // same keyboard contract rather than a second copy of it.
                window.dispatchEvent(
                  new KeyboardEvent("keydown", { key: "k", metaKey: true, bubbles: true }),
                );
              }}
            >
              ⌘K <span className="lib-kbd-hint">search anything</span>
            </PillButton>
            <Segmented
              options={VIEW_OPTIONS}
              value={view}
              onValueChange={(next) => setView(next)}
              label="Library view"
              storageKey={VIEW_STORAGE_KEY}
              testid="lib-view-toggle"
            />
          </div>

          <div data-testid="game-list">
            {view === "shelf" ? (
              <ShelfView
                games={seen}
                libraries={libraries}
                isSettingUp={isSettingUp}
                canCurate={canCurate}
                canOperate={canOperate}
                onAddGame={() => setCreating(true)}
                onStart={(profileId) => startSession.start(profileId)}
                startingProfileId={startSession.pendingProfileId}
              />
            ) : (
              <ListView
                games={seen}
                libraries={libraries}
                isSettingUp={isSettingUp}
                canOperate={canOperate}
                onStart={(profileId) => startSession.start(profileId)}
                startingProfileId={startSession.pendingProfileId}
              />
            )}
          </div>

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

      {startSession.error !== null ? (
        <ErrorState
          error={startSession.error}
          action="Cannot start the session"
          onRetry={() => startSession.reset()}
        />
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

// ── the hero ────────────────────────────────────────────────────────────────

type HeroPick = { game: Game; library: GameLibrary | undefined } | null;

/** The hero slot (ADR 0093 open question (e)): the ready game — the newest
 *  publication; the tie on the clock falls to the id so the pick is
 *  deterministic. When no ready game exists, the game a live setup run names
 *  (by title, the same match the tile chips use) takes the slot. */
function chooseHero(
  games: Game[],
  libraries: Map<string, GameLibrary | undefined>,
  setupGameTitle: string | undefined,
): HeroPick {
  let best: HeroPick = null;
  for (const game of games) {
    if (game.disabled) continue;
    const library = libraries.get(game.id);
    if (library?.profile_status !== "published") continue;
    if (best === null) {
      best = { game, library };
      continue;
    }
    const previous = libraries.get(best.game.id);
    const later =
      (library.published_at ?? "") > (previous?.published_at ?? "") ||
      ((library.published_at ?? "") === (previous?.published_at ?? "") && game.id > best.game.id);
    if (later) best = { game, library };
  }
  if (best !== null) return best;
  if (setupGameTitle === undefined || setupGameTitle === "") return null;
  const wanted = setupGameTitle.toLowerCase();
  for (const game of games) {
    if (!game.disabled && game.title.toLowerCase() === wanted) {
      return { game, library: libraries.get(game.id) };
    }
  }
  return null;
}

function HeroCard({
  game,
  library,
  settingUp,
  setupRun,
  canOperate,
  onStart,
  starting,
}: {
  game: Game;
  library: GameLibrary | undefined;
  settingUp: boolean;
  setupRun?: { voiced: number; characters: number };
  canOperate: boolean;
  onStart: (profileId: string) => void;
  starting: boolean;
}) {
  const ready = library?.profile_status === "published";
  const summary =
    ready && library !== undefined
      ? librarySummary(library)
      : settingUp
        ? `Setting up — ${
            setupRun
              ? `${setupRun.voiced} of ${setupRun.characters} characters voiced`
              : "an onboarding run is filling its draft"
          }`
        : "";
  return (
    <section className="lib-hero" data-testid="lib-hero" aria-label={`Pick up: ${game.title}`}>
      <DitherCover
        className="lib-hero-cover"
        label={`${game.title} cover`}
        tone="peach"
        testid="lib-hero-cover"
      />
      <div className="lib-hero-body">
        <p className="lib-hero-eyebrow">Pick up where you left off</p>
        <h2 className="lib-hero-title" data-testid="lib-hero-title">
          {game.title}
        </h2>
        {summary !== "" ? (
          <p className="lib-hero-summary" data-testid="lib-hero-summary">
            {summary}
          </p>
        ) : null}
        <div className="lib-hero-actions">
          {ready && library?.profile_id !== undefined ? (
            <PillButton
              variant="ink"
              data-testid="lib-hero-start"
              disabled={!canOperate || starting}
              onClick={() => onStart(library.profile_id as string)}
            >
              ▶ Start narrating
            </PillButton>
          ) : null}
          {ready ? (
            <>
              <PillLink to={`/games/${game.id}/characters`} testid="lib-hero-cast">
                Cast
              </PillLink>
              <PillLink to={`/games/${game.id}/dialogue`} testid="lib-hero-dialogue">
                Dialogue
              </PillLink>
            </>
          ) : (
            <PillLink to="/games/new" testid="lib-hero-setup">
              View setup
            </PillLink>
          )}
          {!canOperate && ready ? (
            <span className="lib-hero-note">Starting a session needs the operator role.</span>
          ) : null}
        </div>
      </div>
    </section>
  );
}

/** The outline pill as a link (the kit's PillButton is a <button>; the
 *  navigation pills are anchors with the same class). */
function PillLink({ to, testid, children }: { to: string; testid: string; children: ReactNode }) {
  return (
    <Link to={to} className="kit-pill kit-pill-outline" data-testid={testid}>
      {children}
    </Link>
  );
}

function librarySummary(library: GameLibrary): string {
  const voicedAll = library.voiced >= library.characters && library.characters > 0;
  const voiced = voicedAll ? "all voiced" : `${library.voiced} of ${library.characters} voiced`;
  return `${library.lines} lines · ${library.characters} characters, ${voiced}.`;
}

// ── the shelf ───────────────────────────────────────────────────────────────

function ShelfView({
  games,
  libraries,
  isSettingUp,
  canCurate,
  canOperate,
  onAddGame,
  onStart,
  startingProfileId,
}: {
  games: Game[];
  libraries: Map<string, GameLibrary | undefined>;
  isSettingUp: (game: Game) => boolean;
  canCurate: boolean;
  canOperate: boolean;
  onAddGame: () => void;
  onStart: (profileId: string) => void;
  startingProfileId: string | undefined;
}) {
  return (
    <ul className="lib-shelf" data-testid="lib-shelf">
      {canCurate ? (
        <li className="lib-shelf-tile lib-shelf-add" data-testid="lib-add-game-tile">
          <button
            type="button"
            className="lib-shelf-add-btn"
            onClick={onAddGame}
            data-testid="lib-add-game"
          >
            <span className="lib-add-plus" aria-hidden="true" />
            Add a game
          </button>
        </li>
      ) : null}
      {games.map((game) => (
        <ShelfTile
          key={game.id}
          game={game}
          library={libraries.get(game.id)}
          settingUp={isSettingUp(game)}
          canOperate={canOperate}
          starting={startingProfileId !== undefined}
          onStart={onStart}
        />
      ))}
    </ul>
  );
}

function ShelfTile({
  game,
  library,
  settingUp,
  canOperate,
  starting,
  onStart,
}: {
  game: Game;
  library: GameLibrary | undefined;
  settingUp: boolean;
  canOperate: boolean;
  starting: boolean;
  onStart: (profileId: string) => void;
}) {
  const published = library?.profile_status === "published";
  return (
    <li className="lib-shelf-tile" data-testid={`lib-tile-${game.slug}`}>
      <DitherCover
        className="lib-tile-cover"
        label={`${game.title} cover`}
        tone="peach"
        testid={`lib-tile-cover-${game.slug}`}
      />
      <div className="lib-tile-body">
        <Link
          to={`/games/${game.id}/overview`}
          className="lib-tile-title"
          data-testid={`game-${game.slug}`}
        >
          {game.title}
        </Link>
        {stateChip(game, library, settingUp)}
      </div>
      {published && library?.profile_id !== undefined ? (
        <button
          type="button"
          className="lib-play-circle"
          aria-label={`Start ${game.title}`}
          disabled={!canOperate || starting}
          onClick={() => onStart(library.profile_id as string)}
          data-testid={`lib-play-${game.slug}`}
        >
          ▶
        </button>
      ) : null}
    </li>
  );
}

/** The tile's state chip (the design's `--peach` chip): the run's progress
 *  when it is setting up, else the actionable revision's status, else the
 *  honest "no profile yet". */
function stateChip(game: Game, library: GameLibrary | undefined, settingUp: boolean): ReactNode {
  if (game.disabled) {
    return (
      <Chip tone="neutral" testid={`lib-chip-${game.slug}`}>
        disabled
      </Chip>
    );
  }
  if (settingUp) {
    // The library read's counts are the same numbers the status bar renders
    // (both read ProfileCharacterVoiceCounts); the run's are the live ones.
    return (
      <Chip testid={`lib-chip-${game.slug}`}>
        {library ? `setting up · ${library.voiced} of ${library.characters} voiced` : "setting up"}
      </Chip>
    );
  }
  if (library?.profile_status === "published") {
    return <Chip testid={`lib-chip-${game.slug}`}>published</Chip>;
  }
  if (library?.profile_status === "review") {
    return (
      <Chip tone="neutral" testid={`lib-chip-${game.slug}`}>
        in review
      </Chip>
    );
  }
  if (library?.profile_status === "draft") {
    return (
      <Chip tone="neutral" testid={`lib-chip-${game.slug}`}>
        draft
      </Chip>
    );
  }
  return (
    <Chip tone="neutral" testid={`lib-chip-${game.slug}`}>
      no profile yet
    </Chip>
  );
}

// ── the list ────────────────────────────────────────────────────────────────

function ListView({
  games,
  libraries,
  isSettingUp,
  canOperate,
  onStart,
  startingProfileId,
}: {
  games: Game[];
  libraries: Map<string, GameLibrary | undefined>;
  isSettingUp: (game: Game) => boolean;
  canOperate: boolean;
  onStart: (profileId: string) => void;
  startingProfileId: string | undefined;
}) {
  const columns: ReadonlyArray<TableColumn<Game>> = [
    {
      key: "cover",
      header: "",
      cell: (game) => (
        <DitherCover
          className="lib-list-cover"
          label={`${game.title} cover`}
          tone="peach"
          testid={`lib-list-cover-${game.slug}`}
        />
      ),
    },
    {
      key: "game",
      header: "Game",
      cell: (game) => (
        <span className="lib-list-game">
          <Link to={`/games/${game.id}/overview`} data-testid={`game-${game.slug}`}>
            {game.title}
          </Link>
          {stateChip(game, libraries.get(game.id), isSettingUp(game))}
        </span>
      ),
    },
    {
      key: "platform",
      header: "Platform",
      cell: (game) => game.platform || "—",
    },
    {
      key: "lines",
      header: "Lines",
      cell: (game) => {
        const library = libraries.get(game.id);
        return library === undefined ? "…" : String(library.lines);
      },
    },
    {
      key: "cast",
      header: "Cast",
      cell: (game) => {
        const library = libraries.get(game.id);
        return library === undefined ? "…" : String(library.characters);
      },
    },
    {
      key: "voiced",
      header: "Voiced",
      cell: (game) => {
        const library = libraries.get(game.id);
        if (library === undefined) return "…";
        const percent =
          library.characters > 0 ? Math.round((library.voiced / library.characters) * 100) : 0;
        return (
          <span className="lib-list-voiced">
            <span className="lib-list-voiced-count" data-testid={`lib-voiced-${game.slug}`}>
              {library.voiced}/{library.characters}
            </span>
            <ProgressBar
              value={percent}
              label={`${game.title} voiced: ${library.voiced} of ${library.characters}`}
              testid={`lib-voiced-bar-${game.slug}`}
            />
          </span>
        );
      },
    },
    {
      key: "play",
      header: "",
      cell: (game) => {
        const library = libraries.get(game.id);
        if (library?.profile_status !== "published" || library.profile_id === undefined) {
          return "—";
        }
        const profileId = library.profile_id;
        return (
          <button
            type="button"
            className="lib-play-circle"
            aria-label={`Start ${game.title}`}
            disabled={!canOperate || startingProfileId !== undefined}
            onClick={() => onStart(profileId)}
            data-testid={`lib-play-${game.slug}`}
          >
            ▶
          </button>
        );
      },
    },
  ];
  return (
    <Table<Game>
      caption="All games"
      columns={columns}
      rows={games}
      rowKey={(game) => game.id}
      rowTestid={(game) => `lib-row-${game.slug}`}
      testid="lib-list"
    />
  );
}

// ── the start action ────────────────────────────────────────────────────────

/** ▶ on any game: start a session for that game's published profile and land
 *  on /play. The frame source is omitted so the runtime resolves the default
 *  (ADR-0119) — a usable capture device runs v4l2, else the test pattern in
 *  developer mode, else the start answers 409 no_frame_source. The Play page
 *  then shows the resolved source. One shared mutation: the profile id is the
 *  target, the session id lands in the URL (the Play page's
 *  session-survives-refresh seam). */
function useStartSession(navigate: (to: string) => void) {
  const [targetProfileId, setTargetProfileId] = useState<string | undefined>(undefined);
  const mutation = useMutation({
    mutationFn: (profileId: string) =>
      sessionsApi.start({
        profile_id: profileId,
        dev_trace: false,
      }),
    onSuccess: (session) => {
      setTargetProfileId(undefined);
      navigate(`/play?session=${encodeURIComponent(session.session_id)}`);
    },
    onError: () => setTargetProfileId(undefined),
  });
  return {
    pendingProfileId: mutation.isPending ? targetProfileId : undefined,
    error: mutation.error,
    start: (profileId: string) => {
      setTargetProfileId(profileId);
      mutation.mutate(profileId);
    },
    reset: () => {
      setTargetProfileId(undefined);
      mutation.reset();
    },
  };
}

// ── the create dialog (unchanged behaviour, kept testids) ───────────────────

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
