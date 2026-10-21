/**
 * `src/pages/Cast.tsx` — `/games/:id/cast`, the merged Cast screen
 * (docs/design/plum-peach 2d, ADR-0093): one cast, one place to review
 * voices. It replaces the former `/games/:id/characters` (roster,
 * attribution, speaker placeholders) and `/games/:id/voices` (voice brief,
 * auditions, approval, registration, preview) pages; the old paths
 * redirect here (the router's seams in src/App.tsx).
 *
 * Layout: the game title with the game tabs and the header controls
 * (▶ Start narrating, the per-profile auto-cast dial — on by default,
 * ADR-0109/UX-03 — and the list/cards toggle, persisted per user, UX-08);
 * the roster (a table in list view, name-tab cards in cards view); and the
 * 380px detail panel for the `?character=` selection (the detail panel and
 * the audition cards live in src/pages/cast/).
 *
 * Selection lives in the URL (`?character=`), the same way the former voices
 * page did; the view toggle persists in localStorage under `gn.cast.view`.
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router";

import {
  ApiError,
  catalogApi,
  type Character,
  type Edition,
  type Message,
  type Profile,
} from "../api/client";
import { fetchAllMessages } from "../api/profiles";
import { voicesApi, type VoiceRevision } from "../api/voices";
import { useAuth } from "../auth/auth-context";
import { DitherCover, NameTab, Segmented, Switch, readSegmented } from "../components/kit";
import { EmptyState, ErrorState, Loading } from "../components/states";
import { CastDetailPanel } from "./cast/CastDetailPanel";
import {
  attributionOf,
  attributionStatusList,
  CAST_VIEW_STORAGE_KEY,
  currentAndWorking,
  revisionsOfAll,
  slugOf,
  voiceStateOf,
  type CastView,
} from "./cast/castShared";
import { SpeakerPlaceholderSection } from "./cast/SpeakerPlaceholders";

const VIEW_OPTIONS = [
  { value: "list", label: "list" },
  { value: "cards", label: "cards" },
] as const;

/** The header's game tabs (docs/design/plum-peach 2d). Cast is this page. */
const GAME_TABS = [
  { id: "overview", label: "overview" },
  { id: "dialogue", label: "dialogue" },
  { id: "cast", label: "cast" },
  { id: "regions", label: "regions" },
  { id: "publish", label: "publish" },
] as const;

export function CastPage() {
  const { id: gameId = "" } = useParams();
  const { atLeast } = useAuth();
  const [params, setParams] = useSearchParams();
  const profileParam = params.get("profile") ?? "";
  const characterParam = params.get("character") ?? "";
  const queryClient = useQueryClient();

  const game = useQuery({
    queryKey: ["game", gameId],
    queryFn: ({ signal }) => catalogApi.getGame(gameId, signal),
  });

  // The revision the cast works on: ?profile= when given, else the edition's
  // published revision (what a session would speak with), else its newest
  // revision (a game still in setup).
  const resolved = useQuery({
    queryKey: ["cast", "resolve-profile", gameId, profileParam],
    queryFn: async ({ signal }): Promise<Profile | null> => {
      if (profileParam) return catalogApi.getProfile(profileParam, signal);
      const editions = (
        (await catalogApi.listEditions(gameId, signal)) as unknown as { editions: Edition[] }
      ).editions;
      const first = editions[0];
      if (!first) return null;
      try {
        const published = await catalogApi.getPublished(first.id, signal);
        if (published) return published as Profile;
      } catch (error) {
        if (!(error instanceof ApiError && error.status === 404)) throw error;
      }
      const profiles = (
        (await catalogApi.listProfiles(first.id, signal)) as unknown as { profiles: Profile[] }
      ).profiles;
      if (profiles.length === 0) return null;
      const newest = [...profiles].sort((a, b) => b.revision_num - a.revision_num)[0];
      return catalogApi.getProfile(newest.id, signal);
    },
  });

  const profile = resolved.data;
  const profileId = profile?.id ?? "";

  const characters = useQuery({
    queryKey: ["characters", profileId],
    queryFn: ({ signal }) => catalogApi.getCharacters(profileId, signal),
    enabled: profileId !== "",
  });
  const roster = (characters.data as { characters: Character[] } | undefined)?.characters ?? [];

  const messages = useQuery({
    queryKey: ["characters", "messages", profileId],
    queryFn: ({ signal }) => fetchAllMessages(profileId, signal),
    enabled: profileId !== "",
  });

  // Every voice revision of this appliance in one read: the roster's state
  // pills and the detail panel both derive from it, so a cast of a dozen
  // characters costs one query, not twelve.
  const allVoices = useQuery({
    queryKey: ["voices", "all"],
    queryFn: ({ signal }) => voicesApi.listVoices(undefined, signal),
    enabled: profileId !== "",
  });
  // The voice workflow may be unwired on this appliance (503
  // SUBSYSTEM_UNAVAILABLE, docs/spec/13): the roster is still the script's
  // characters — the voice column degrades to "no voice" and the panel says
  // the work is unavailable, instead of the one unwired subsystem erroring
  // the whole screen.
  const voicesUnavailable =
    allVoices.isError && allVoices.error instanceof ApiError && allVoices.error.status === 503;
  const voices = voicesUnavailable
    ? []
    : ((allVoices.data as { voices: VoiceRevision[] } | undefined)?.voices ?? []);

  const selected = useMemo(
    () => roster.find((c) => c.name === characterParam) ?? null,
    [roster, characterParam],
  );
  const selectedRevisions = useMemo(
    () => (selected ? revisionsOfAll(voices, selected.name) : []),
    [voices, selected],
  );
  const { working, current } = useMemo(
    () => currentAndWorking(selectedRevisions),
    [selectedRevisions],
  );

  const workingDetail = useQuery({
    queryKey: ["voice-detail", working?.id],
    queryFn: ({ signal }) => voicesApi.getVoice(working?.id as string, signal),
    enabled: working !== null && working.state !== "approved",
  });
  const currentDetail = useQuery({
    queryKey: ["voice-detail", current?.id],
    queryFn: ({ signal }) => voicesApi.getVoice(current?.id as string, signal),
    enabled: current !== null,
  });

  // The per-profile auto-cast dial (ADR-0109, UX-03): on by default; the
  // toggle answers with the profile the server re-read.
  const autoCast = profile?.auto_cast ?? true;
  const setDial = useMutation({
    mutationFn: (enabled: boolean) => catalogApi.setAutoCast(profileId, enabled),
    onSuccess: (res) => {
      queryClient.setQueryData(["cast", "resolve-profile", gameId, profileParam], res);
    },
  });

  const [view, setView] = useState<CastView>(() =>
    readSegmented([...VIEW_OPTIONS], CAST_VIEW_STORAGE_KEY, "list"),
  );

  const setCharacter = (name: string) => {
    const next = new URLSearchParams(params);
    if (name) next.set("character", name);
    else next.delete("character");
    setParams(next, { replace: true });
  };

  // A stale ?character= (a retired name, another profile's roster) is
  // cleared, not rendered as an error: the roster itself is the answer.
  useEffect(() => {
    if (
      characterParam !== "" &&
      roster.length > 0 &&
      !roster.some((c) => c.name === characterParam)
    ) {
      setParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          next.delete("character");
          return next;
        },
        { replace: true },
      );
    }
  }, [roster, characterParam, setParams]);

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["voices"] });
    void queryClient.invalidateQueries({ queryKey: ["voice-detail"] });
  };

  if (game.isPending || resolved.isPending) {
    return (
      <main>
        <h1 data-testid="cast-title">Cast</h1>
        <Loading label="Loading the cast…" />
      </main>
    );
  }
  if (game.isError) {
    return (
      <main>
        <h1 data-testid="cast-title">Cast</h1>
        <ErrorState error={game.error} action="Cannot load this game" />
      </main>
    );
  }

  return (
    <main data-testid="cast-page">
      <header className="cast-header">
        <p className="muted">
          <Link to={`/games/${gameId}/overview`} data-testid="cast-back">
            ← Overview
          </Link>
        </p>
        <h1 data-testid="cast-title" className="cast-title">
          {game.data?.title ?? gameId}
        </h1>
        <nav className="cast-tabs" aria-label="Game pages">
          {GAME_TABS.map((tab) => (
            <Link
              key={tab.id}
              to={gameTabUrl(gameId, tab.id, profileId)}
              className={tab.id === "cast" ? "cast-tab-active" : undefined}
              aria-current={tab.id === "cast" ? "page" : undefined}
              data-testid={`cast-tab-${tab.id}`}
            >
              {tab.label}
            </Link>
          ))}
        </nav>
        <div className="cast-header-controls">
          <Link to="/play" className="kit-pill kit-pill-ink" data-testid="cast-start-narrating">
            ▶ Start narrating
          </Link>
          <Switch
            className="auto-cast"
            label="auto-cast"
            checked={autoCast}
            disabled={setDial.isPending || profileId === ""}
            testid="auto-cast-switch"
            onChange={(enabled) => setDial.mutate(enabled)}
          />
          {setDial.isError ? (
            <span className="muted" role="alert" data-testid="auto-cast-error">
              the dial did not change: {setDial.error instanceof Error ? setDial.error.message : ""}
            </span>
          ) : null}
          <Segmented
            options={[...VIEW_OPTIONS]}
            value={view}
            onValueChange={setView}
            label="Cast view"
            storageKey={CAST_VIEW_STORAGE_KEY}
            testid="cast-view-toggle"
          />
        </div>
      </header>

      {resolved.isError ? (
        <ErrorState error={resolved.error} action="Cannot resolve the profile" />
      ) : null}
      {profile === null && !resolved.isError ? (
        <EmptyState title="No revision to show">
          <p>
            This game has no edition with a revision yet. Create an edition on the overview page,
            import a script, and the cast appears here.
          </p>
        </EmptyState>
      ) : null}

      {profileId !== "" && (characters.isError || messages.isError) ? (
        <ErrorState error={characters.error ?? messages.error} action="Cannot load the roster" />
      ) : null}
      {profileId !== "" && (characters.isLoading || messages.isLoading || allVoices.isLoading) ? (
        <Loading label="Loading the roster…" />
      ) : null}

      {profileId !== "" && roster.length === 0 && !characters.isLoading ? (
        <EmptyState title="No characters yet">
          <p>
            Import a script with a speaker column and the roster appears here — with a voice for
            every name when auto-cast is on.
          </p>
        </EmptyState>
      ) : null}

      {profile?.status === "draft" && atLeast("curator") && roster.length > 0 && messages.data ? (
        <SpeakerPlaceholderSection
          profileId={profileId}
          characters={roster}
          messages={messages.data}
        />
      ) : null}

      {roster.length > 0 ? (
        <div className="cast-layout">
          <section className="cast-roster" aria-label="Cast roster" data-testid="voice-characters">
            {view === "list" ? (
              <RosterList
                roster={roster}
                messages={messages.data ?? []}
                voices={voices}
                selectedName={selected?.name ?? ""}
                onSelect={setCharacter}
              />
            ) : (
              <RosterCards
                roster={roster}
                voices={voices}
                selectedName={selected?.name ?? ""}
                onSelect={setCharacter}
              />
            )}
          </section>

          {selected ? (
            <CastDetailPanel
              key={selected.id}
              character={selected}
              messages={messages.data ?? []}
              revisions={selectedRevisions}
              workingDetail={workingDetail.data}
              workingDetailLoading={workingDetail.isLoading}
              workingDetailError={workingDetail.error}
              currentDetail={currentDetail.data}
              currentDetailLoading={currentDetail.isLoading}
              currentDetailError={currentDetail.error}
              profileId={profileId}
              autoCast={autoCast}
              voicesUnavailable={voicesUnavailable}
              canCurate={atLeast("curator")}
              onRefresh={refresh}
            />
          ) : (
            <aside className="kit-empty" data-testid="cast-empty">
              <h2>Pick a character</h2>
              <p>
                Choose a name from the roster to review its voice, edit its brief, and generate
                takes.
              </p>
            </aside>
          )}
        </div>
      ) : null}
    </main>
  );
}

interface RosterProps {
  roster: Character[];
  messages: Message[];
  voices: VoiceRevision[];
  selectedName: string;
  onSelect: (name: string) => void;
}

/** The URL of one header game tab (ADR-0093's tab row): the overview is
 *  bare, the rest carry the revision the cast is working on. */
function gameTabUrl(gameId: string, tab: string, profileId: string): string {
  if (tab === "overview") return `/games/${gameId}/overview`;
  const profile = profileId !== "" ? `?profile=${encodeURIComponent(profileId)}` : "";
  return `/games/${gameId}/${tab}${profile}`;
}

/** List view (docs/design/plum-peach 2d): avatar · name · lines · state
 *  pill · voice brief. The row testids carry the former characters page's
 *  contract (ADR-0093 merge): the table, the rows, the line counts and the
 *  attribution cells keep their ids. */
function RosterList({ roster, messages, voices, selectedName, onSelect }: RosterProps) {
  // At phone width (<=600px) the table reflows into stacked row cards
  // (the index.css media query reads the td data-th labels): the design
  // allows no horizontal page scroll at 390px, and the wide Voice column
  // used to be cut off inside its row. The DOM stays a table, so every
  // row/cell testid keeps working.
  return (
    <div className="table-overflow">
      <table className="roster cast-roster-table" data-testid="character-table">
        <thead>
          <tr>
            <th scope="col"></th>
            <th scope="col">Name</th>
            <th scope="col">Lines</th>
            <th scope="col">State</th>
            <th scope="col">Voice</th>
          </tr>
        </thead>
        <tbody>
          {roster.map((character) => {
            const slug = slugOf(character.name);
            const lines = messages.filter((m) => m.speaker_id === character.id);
            const attribution = attributionOf(lines);
            const state = voiceStateOf(revisionsOfAll(voices, character.name));
            const brief = revisionsOfAll(voices, character.name)[0]?.design_instruction;
            return (
              <tr
                key={character.id}
                className={selectedName === character.name ? "cast-row-selected" : undefined}
                data-testid={`character-row-${slug}`}
              >
                <td data-th="character">
                  <DitherCover
                    tone="peach"
                    testid={`cast-avatar-${slug}`}
                    label={`${character.name} portrait placeholder`}
                    className="cast-row-avatar"
                  />
                </td>
                <td data-th="name">
                  <button
                    type="button"
                    className="cast-row-name"
                    data-testid={`cast-row-name-${slug}`}
                    aria-current={selectedName === character.name}
                    onClick={() => onSelect(character.name)}
                  >
                    <strong>{character.name}</strong>
                    {character.aliases.length > 0 ? (
                      <span className="muted"> · {character.aliases.join(", ")}</span>
                    ) : null}
                    <span className={`badge badge-plain character-status`}>
                      {" "}
                      {character.status}
                    </span>
                  </button>
                </td>
                <td data-th="lines" data-testid={`character-lines-${slug}`}>
                  {lines.length}
                </td>
                <td data-th="state" data-testid={`character-attribution-${slug}`}>
                  {attribution === "none" ? (
                    <span className="muted">no assigned lines</span>
                  ) : (
                    <span className={`badge badge-${attribution}`}>{attribution}</span>
                  )}
                  {attribution === "inferred" ? (
                    <span className="muted">
                      {" "}
                      (speaker attribution is {attributionStatusList(lines)}, not confirmed)
                    </span>
                  ) : null}
                </td>
                <td data-th="voice" data-testid={`character-voice-${slug}`}>
                  <span
                    className={`badge badge-${state.tone === "voiced" ? "published" : state.tone === "pending" ? "draft" : "plain"}`}
                    data-testid={`character-voice-state-${slug}`}
                  >
                    {state.label}
                  </span>
                  {brief ? <span className="muted cast-row-brief">{brief}</span> : null}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/** Cards view (docs/design/plum-peach 2d): name tab over a dithered
 *  portrait strip, the line count and the state. */
function RosterCards({ roster, voices, selectedName, onSelect }: Omit<RosterProps, "messages">) {
  return (
    <div className="cast-cards" data-testid="cast-cards">
      {roster.map((character) => {
        const slug = slugOf(character.name);
        const state = voiceStateOf(revisionsOfAll(voices, character.name));
        return (
          <button
            key={character.id}
            type="button"
            className={
              selectedName === character.name ? "cast-card cast-card-selected" : "cast-card"
            }
            aria-current={selectedName === character.name}
            data-testid={`cast-card-${slug}`}
            onClick={() => onSelect(character.name)}
          >
            <NameTab testid={`cast-card-name-${slug}`}>{character.name}</NameTab>
            <DitherCover
              tone="peach"
              testid={`cast-card-cover-${slug}`}
              label={`${character.name} portrait placeholder`}
              className="cast-card-cover"
            />
            <span
              className={`badge badge-${state.tone === "voiced" ? "published" : state.tone === "pending" ? "draft" : "plain"}`}
              data-testid={`cast-card-state-${slug}`}
            >
              {state.label}
            </span>
          </button>
        );
      })}
    </div>
  );
}
