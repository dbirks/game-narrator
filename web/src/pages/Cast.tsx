/**
 * `src/pages/Cast.tsx` — `/games/:id/cast`, the merged Cast screen
 * (docs/design/plum-peach 2d, ADR-0093; the voice-picking redesign per
 * ADR-0135): one cast, one place to review voices. It replaces the former
 * `/games/:id/characters` (roster, attribution, speaker placeholders) and
 * `/games/:id/voices` (voice brief, auditions, approval, registration,
 * preview) pages; the old paths redirect here (the router's seams in
 * src/App.tsx).
 *
 * The redesign (ADR-0135): the roster reads in the order the work needs —
 * named and voiced first, named needs-a-voice next (line count descending),
 * then the import's "unnamed speaker" rows, and the retired rows hidden
 * behind their own chip (`All` hides them by default) — with filter chips
 * (All · Needs a voice · Voiced · Unnamed · Retired (n)) and a name search;
 * one compact row per character: the avatar, the name (or "Unnamed
 * speaker"), the line count, the voice state and a single primary action
 * (Listen / Pick a voice / Name) that opens the roomy voice picker; the
 * shared voiced count in the header ("n of m voiced", the same definition
 * the Library card and the status bar read from the server); and the bulk
 * work up top — voice everyone with auto-cast (progress as the jobs land)
 * and name the speakers with the agent (the exact model-role setting shown
 * when it is not configured, not a dead button).
 *
 * Layout: the game title with the game tabs and the header controls (▶
 * Start narrating, the per-profile auto-cast dial — on by default,
 * ADR-0109/UX-03 — and the list/cards toggle, persisted per user, UX-08);
 * the roster (a table in list view, name-tab cards in cards view); and the
 * voice picker as the right-hand panel (full-height at desktop widths, a
 * full-screen sheet below 600px — the detail panel and the audition cards
 * live in src/pages/cast/).
 *
 * Selection lives in the URL (`?character=`), the same way the former
 * voices page did; the view toggle persists in localStorage under
 * `gn.cast.view`. A name is the character's identity on the voice side
 * (the voice surface is keyed by name, ADR-0135), so the URL carries the
 * name the roster shows.
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router";

import {
  ApiError,
  catalogApi,
  jobsApi,
  type Character,
  type Edition,
  type Message,
  type Profile,
} from "../api/client";
import { onboardingApi } from "../api/onboarding";
import { fetchAllMessages } from "../api/profiles";
import { fetchSettings } from "../api/settings";
import { voicesApi, type VoiceRevision } from "../api/voices";
import { useAuth } from "../auth/auth-context";
import { setAgentEnrichment } from "../components/agent/agentContext";
import { DitherCover, NameTab, Segmented, Switch, readSegmented } from "../components/kit";
import { EmptyState, ErrorState, Loading } from "../components/states";
import { CastDetailPanel } from "./cast/CastDetailPanel";
import {
  attributionOf,
  attributionStatusList,
  CAST_FILTERS,
  CAST_FILTER_STORAGE_KEY,
  CAST_VIEW_STORAGE_KEY,
  castCounts,
  castFilterCount,
  castFilterOf,
  castOrder,
  castRowAction,
  castSearchMatch,
  DEFAULT_BRIEF,
  currentAndWorking,
  isUnnamedSpeaker,
  revisionsOfAll,
  slugOf,
  voiceStateOf,
  type CastFilter,
  type CastView,
} from "./cast/castShared";

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

/** The bulk work's targets (ADR-0135): the named (not the import's
 *  placeholder), not-retired, speaking characters that carry no live take,
 *  in the roster's own presentation order. */
function bulkVoiceTargets(
  roster: Character[],
  messages: Message[],
  voices: VoiceRevision[],
): Character[] {
  return castOrder(roster, messages, voices).filter(
    (character) =>
      character.status !== "retired" &&
      !isUnnamedSpeaker(character, messages) &&
      messages.some((message) => message.speaker_id === character.id) &&
      !revisionsOfAll(voices, character.name).some((voice) => voice.state !== "retired"),
  );
}

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

  // The profile's narrator dial (ADR-0101): the voice work's other escape
  // hatch — a line whose speaker has no approved take speaks through the
  // narrator instead of going silent. It lives with the auto-cast dial in
  // the header: both are profile-level, and the picker's fold at the review
  // width keeps the take work above it (ADR-0135).
  const setNarrator = useMutation({
    mutationFn: (enabled: boolean) => catalogApi.setNarratorFallback(profileId, enabled),
    onSuccess: (res) => {
      queryClient.setQueryData(["cast", "resolve-profile", gameId, profileParam], res);
    },
  });

  // The presentation order (ADR-0135) and the roster's filters: the chips
  // and the name search are the screen's read on the roster, and the
  // `Retired (n)` chip is where the dead rows live — `All` hides them by
  // default, so the work rows lead.
  const [view, setView] = useState<CastView>(() =>
    readSegmented([...VIEW_OPTIONS], CAST_VIEW_STORAGE_KEY, "list"),
  );
  const [filter, setFilter] = useState<CastFilter>(() => {
    const stored = window.localStorage.getItem(CAST_FILTER_STORAGE_KEY) as CastFilter | null;
    return stored != null && CAST_FILTERS.some((chip) => chip.id === stored) ? stored : "all";
  });
  const chooseFilter = (next: CastFilter) => {
    setFilter(next);
    window.localStorage.setItem(CAST_FILTER_STORAGE_KEY, next);
  };
  const [search, setSearch] = useState("");
  const rows = useMemo(
    () =>
      castOrder(roster, messages.data ?? [], voices).filter(
        (character) =>
          castFilterOf(character, filter, messages.data ?? [], voices) &&
          castSearchMatch(character, search),
      ),
    [roster, messages.data, voices, filter, search],
  );

  // The shared voiced count (ADR-0135): the client mirror of the read the
  // Library card and the status bar take from the server, over the rows the
  // screen holds — the header chip and the filter chips derive from this
  // one pair, so the Cast screen cannot disagree with the Library about how
  // much of the game has a voice.
  const counts = useMemo(
    () => castCounts(roster, messages.data ?? [], voices),
    [roster, messages.data, voices],
  );

  const setCharacter = (name: string) => {
    const next = new URLSearchParams(params);
    if (name) next.set("character", name);
    else next.delete("character");
    setParams(next, { replace: true });
  };

  // A stale ?character= (a retired name, another profile's roster) is
  // cleared, not rendered as an error: the roster itself is the answer. A
  // legacy row-id selection — the old /voices and /characters links the Cast
  // redirect keeps (ADR-0093 §4) — is rewritten to the character's name, the
  // screen's convention, so a saved link still selects the same character.
  // The guard skips while the roster read refetches: a roster write (the
  // speaker-link's own invalidation) settles with the renamed row in, so a
  // fresh read is the only one that may clear the selection.
  useEffect(() => {
    if (characterParam === "" || roster.length === 0 || characters.isFetching) return;
    if (roster.some((c) => c.name === characterParam)) return;
    const legacy = roster.find((c) => c.id === characterParam);
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (legacy !== undefined) next.set("character", legacy.name);
        else next.delete("character");
        return next;
      },
      { replace: true },
    );
  }, [roster, characterParam, characters.isFetching, setParams]);

  // The agent dock's screen context (UX-01): the roster's rows in order, and
  // the screen's own selection — which the URL names by character name, so
  // the dock's payload carries the resolved row id the server will accept.
  useEffect(() => {
    setAgentEnrichment({
      visibleIds: roster.map((c) => c.id),
      selection: selected !== null ? { type: "character", id: selected.id } : undefined,
    });
    return () => setAgentEnrichment({});
  }, [roster, selected]);

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["voices"] });
    void queryClient.invalidateQueries({ queryKey: ["voice-detail"] });
  };

  // ── the bulk work (ADR-0135) ──────────────────────────────────────────────
  // "Voice everyone with auto-cast": one audition run per named, unvoiced,
  // speaking character with the page's default brief, enqueued one at a
  // time (the job queue's lane is the screen's own backpressure) and
  // reported as the jobs land. "Name speakers with the agent": the
  // onboarding run that names the import's placeholder rows (docs/spec/03's
  // autonomous research) — gated on its model role's configuration, which
  // the screen shows verbatim rather than letting a dead button do it.
  const canCurate = atLeast("curator");
  const bulkTargets = useMemo(
    () => bulkVoiceTargets(roster, messages.data ?? [], voices),
    [roster, messages.data, voices],
  );
  const [bulk, setBulk] = useState<{ name: string; jobId: string }[] | null>(null);
  const [bulkSettled, setBulkSettled] = useState<Record<string, "succeeded" | "failed">>({});
  const bulkSucceeded = Object.values(bulkSettled).filter((value) => value === "succeeded").length;
  const bulkFailedNames = Object.entries(bulkSettled)
    .filter(([, value]) => value === "failed")
    .map(([name]) => name);
  const bulkAllSettled =
    bulk != null && bulk.length > 0 && bulk.every((entry) => bulkSettled[entry.name] != null);
  const bulkRunning = bulk != null && !bulkAllSettled;
  const startBulkVoice = useCallback(() => {
    if (bulkRunning || bulkTargets.length === 0 || profileId === "") return;
    setBulk(bulkTargets.map((character) => ({ name: character.name, jobId: "" })));
    setBulkSettled({});
    void (async () => {
      for (const target of bulkTargets) {
        try {
          const result = await voicesApi.createAuditions({
            character: target.name,
            brief: DEFAULT_BRIEF,
            profile_id: profileId,
          });
          setBulk((prev) =>
            prev == null
              ? prev
              : prev.map((entry) =>
                  entry.name === target.name
                    ? { ...entry, jobId: (result as { job_id: string }).job_id }
                    : entry,
                ),
          );
        } catch {
          // A refusal (the row already carries a live take, a dial that
          // raced a manual run) is the name the bulk did not reach, named in
          // the note — not a crash.
          setBulkSettled((prev) => ({ ...prev, [target.name]: "failed" }));
        }
      }
    })();
  }, [bulkRunning, bulkTargets, profileId]);

  // The bulk's poller: every unsettled job is read until it is terminal.
  // The settled map is keyed by the character's name and a settled name is
  // never re-read, so the tick is idempotent — the interval's staleness is a
  // spare read at worst, never a double count.
  const bulkRef = useRef(bulk);
  bulkRef.current = bulk;
  const settledRef = useRef(bulkSettled);
  settledRef.current = bulkSettled;
  useEffect(() => {
    if (bulk == null) return;
    const timer = window.setInterval(async () => {
      const current = bulkRef.current ?? [];
      for (const entry of current) {
        if (entry.jobId === "" || settledRef.current[entry.name] != null) continue;
        try {
          const job = (await jobsApi.get(entry.jobId)) as unknown as { state: string };
          const settled: "succeeded" | "failed" = job.state === "failed" ? "failed" : "succeeded";
          if (job.state === "succeeded" || job.state === "failed") {
            setBulkSettled((prev) =>
              prev[entry.name] != null ? prev : { ...prev, [entry.name]: settled },
            );
          }
        } catch {
          // A read that fails is retried on the next tick; the job record is
          // durable and the tick is the screen's own backoff.
        }
      }
    }, 1500);
    return () => window.clearInterval(timer);
  }, [bulk]);
  useEffect(() => {
    if (!bulkAllSettled) return;
    refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [bulkAllSettled]);

  // "Name speakers with the agent": the run's own gate (it runs work on a
  // draft revision) and its model role's configuration, read the way the
  // settings surface reads them (GET /api/v1/settings is the admin read; a
  // curator without it gets the remedy the docs spell instead of a dead
  // button). The run the start answers with is the setup board's own deep
  // link (?run=), so the screen never hides where the work is.
  const settingsQuery = useQuery({
    queryKey: ["settings"],
    queryFn: ({ signal }) => fetchSettings(signal),
    enabled: canCurate && profile?.status === "draft" && roster.length > 0,
    retry: false,
  });
  const agentRole = settingsQuery.data?.models?.roles?.find(
    (role) => role.role === "agent_reasoning",
  );
  const nameSpeakerNote = (() => {
    if (profile?.status !== "draft") {
      return `onboarding runs work on a draft revision — this profile is ${profile?.status}`;
    }
    if (settingsQuery.isPending) return "checking the agent's model role…";
    if (settingsQuery.isError) {
      return "the agent's reasoning role must be configured — set GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL";
    }
    if (agentRole == null || !agentRole.configured) {
      return agentRole?.state ?? "the agent's reasoning role is not configured";
    }
    return null;
  })();
  const [namingRun, setNamingRun] = useState<string | null>(null);
  const startNaming = useMutation({
    mutationFn: () =>
      onboardingApi.create({
        title: `Naming the cast of ${game.data?.title ?? gameId}`,
        profile_id: profileId,
        use_web: false,
      }),
    onSuccess: (run) => setNamingRun((run as { run_id: string }).run_id),
  });

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
          {/* The profile's other voice dial (ADR-0101). */}
          <Switch
            className="narrator"
            label="narrator"
            checked={profile?.narrator_fallback ?? true}
            disabled={setNarrator.isPending || profileId === ""}
            testid="narrator-fallback-toggle"
            onChange={(enabled) => setNarrator.mutate(enabled)}
          />
          {setNarrator.isError ? (
            <span className="muted" role="alert" data-testid="narrator-fallback-error">
              the dial did not change:{" "}
              {setNarrator.error instanceof Error ? setNarrator.error.message : ""}
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
          {/* The shared voiced count (ADR-0135): the header's one number
              about the work, the same definition the Library card and the
              status bar read from the server. It waits on the roster's
              lines and the voice read; the 503 says so rather than
              guessing. */}
          {profile !== null &&
          roster.length > 0 &&
          !characters.isLoading &&
          !allVoices.isLoading ? (
            <span className="cast-count-chip">
              {voicesUnavailable ? (
                <span className="muted" data-testid="cast-voiced-count">
                  the voice count waits on the speech services
                </span>
              ) : (
                <span className="cast-voiced-chip" data-testid="cast-voiced-count">
                  {counts.voiced} of {counts.speakers} voiced
                </span>
              )}
              <span className="muted">
                · {roster.length} characters, {messages.data?.length ?? 0} lines
              </span>
            </span>
          ) : null}
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

      {/* The bulk work (ADR-0135): the screen's two one-go actions, on for a
          draft the curator can move. */}
      {profile?.status === "draft" && canCurate && roster.length > 0 && messages.data ? (
        <div className="cast-bulk-bar" data-testid="cast-bulk-bar">
          <button
            type="button"
            className="kit-pill kit-pill-ink"
            data-testid="cast-bulk-voice-all"
            disabled={bulkRunning || bulkTargets.length === 0 || voicesUnavailable}
            title={
              bulkTargets.length === 0
                ? "every named speaker already carries a live take"
                : `voice ${bulkTargets.length} named speakers with auto-cast (the page's default brief)`
            }
            onClick={startBulkVoice}
          >
            Voice everyone with auto-cast
          </button>
          {bulk != null ? (
            <span className="cast-bulk-progress" role="status" data-testid="cast-bulk-progress">
              {bulkRunning ? "voicing" : "voiced"} {bulkSucceeded} of {bulk.length}
              {bulkFailedNames.length > 0 ? ` — did not reach: ${bulkFailedNames.join(", ")}` : ""}
            </span>
          ) : null}
          <span className="cast-bulk-sep" aria-hidden="true">
            ·
          </span>
          <button
            type="button"
            className="kit-pill"
            data-testid="cast-bulk-name-speakers"
            disabled={nameSpeakerNote !== null || startNaming.isPending}
            onClick={() => startNaming.mutate()}
          >
            {startNaming.isPending ? "Starting the naming run…" : "Name speakers with the agent"}
          </button>
          {nameSpeakerNote !== null ? (
            <span className="muted" data-testid="cast-agent-role-note">
              {nameSpeakerNote}
            </span>
          ) : null}
          {startNaming.isError ? (
            <span className="muted" role="alert" data-testid="cast-bulk-name-error">
              the naming run did not start:{" "}
              {startNaming.error instanceof Error
                ? startNaming.error.message
                : String(startNaming.error)}
            </span>
          ) : null}
          {namingRun !== null ? (
            <span data-testid="cast-bulk-name-run">
              the naming run is going —{" "}
              <Link to={`/games/new?run=${encodeURIComponent(namingRun)}`}>
                open the setup board
              </Link>
            </span>
          ) : null}
        </div>
      ) : null}

      {roster.length > 0 ? (
        <div className="cast-layout">
          <section className="cast-roster" aria-label="Cast roster" data-testid="voice-characters">
            {/* The filter chips (ADR-0135): All hides the retired rows by
                default (the Retired chip is where they live), and each chip
                carries its own count over the roster the screen holds. */}
            <div className="cast-filters" role="group" aria-label="Filter the roster">
              {CAST_FILTERS.map((chip) => (
                <button
                  key={chip.id}
                  type="button"
                  className={
                    filter === chip.id
                      ? "cast-filter-chip cast-filter-chip-active"
                      : "cast-filter-chip"
                  }
                  aria-pressed={filter === chip.id}
                  onClick={() => chooseFilter(chip.id)}
                  data-testid={`cast-filter-${chip.id}`}
                >
                  {chip.label}{" "}
                  <span className="cast-filter-count">
                    {castFilterCount(roster, chip.id, messages.data ?? [], voices)}
                  </span>
                </button>
              ))}
              <input
                type="search"
                className="cast-name-search"
                placeholder="Search by name"
                aria-label="Search the roster by name"
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                data-testid="cast-name-search"
              />
            </div>

            {view === "list" ? (
              <RosterList
                roster={rows}
                messages={messages.data ?? []}
                voices={voices}
                selectedName={selected?.name ?? ""}
                onSelect={setCharacter}
              />
            ) : (
              <RosterCards
                roster={rows}
                messages={messages.data ?? []}
                voices={voices}
                selectedName={selected?.name ?? ""}
                onSelect={setCharacter}
              />
            )}
            {rows.length === 0 ? (
              <p className="muted" data-testid="roster-filter-empty">
                No rows match the filter{search.trim() !== "" ? " and the search" : ""}.
              </p>
            ) : null}
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
              narratorFallback={profile?.narrator_fallback ?? true}
              onNarratorChange={(enabled) => setNarrator.mutate(enabled)}
              voicesUnavailable={voicesUnavailable}
              canCurate={canCurate}
              onRefresh={refresh}
              onClose={() => setCharacter("")}
              onNamed={(name) => setCharacter(name)}
            />
          ) : (
            <aside className="kit-empty" data-testid="cast-empty">
              <h2>Pick a character</h2>
              <p>
                Choose a name from the roster to open its voice work: the brief, the takes, the
                approval and the registration live in the panel.
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

/** List view (docs/design/plum-peach 2d, ADR-0135's compact row): avatar ·
 *  name (or "Unnamed speaker") · lines · state pill · voice state and
 *  brief · the row's one primary action. The row testids carry the former
 *  characters page's contract (ADR-0093 merge): the table, the rows, the
 *  line counts and the attribution cells keep their ids. */
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
            <th scope="col">
              <span className="sr-only">Action</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {roster.map((character) => {
            const slug = slugOf(character.name);
            const lines = messages.filter((m) => m.speaker_id === character.id);
            const attribution = attributionOf(lines);
            const state = voiceStateOf(revisionsOfAll(voices, character.name));
            const brief = revisionsOfAll(voices, character.name)[0]?.design_instruction;
            const unnamed = isUnnamedSpeaker(character, lines);
            const action = castRowAction(character, lines, voices);
            return (
              <tr
                key={character.id}
                className={selectedName === character.name ? "cast-row-selected" : undefined}
                data-testid={`character-row-${slug}`}
                title={
                  unnamed
                    ? `Roster key ${character.name} — a speaker the source names only as an identifier (the import's placeholder); the row's action is Name`
                    : undefined
                }
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
                    <strong>{unnamed ? "Unnamed speaker" : character.name}</strong>
                    {unnamed ? (
                      <span className="muted"> · {character.name}</span>
                    ) : character.aliases.length > 0 ? (
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
                <td data-th="action">
                  {action === "none" ? (
                    <span className="muted cast-row-action-none" aria-hidden="true">
                      —
                    </span>
                  ) : (
                    <button
                      type="button"
                      className="cast-row-action"
                      data-testid={`cast-row-action-${slug}`}
                      onClick={() => onSelect(character.name)}
                    >
                      {action === "listen" ? "Listen" : action === "name" ? "Name" : "Pick a voice"}
                    </button>
                  )}
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
function RosterCards({ roster, messages, voices, selectedName, onSelect }: RosterProps) {
  return (
    <div className="cast-cards" data-testid="cast-cards">
      {roster.map((character) => {
        const slug = slugOf(character.name);
        const state = voiceStateOf(revisionsOfAll(voices, character.name));
        const unnamed = isUnnamedSpeaker(character, messages);
        const lineCount = messages.filter((m) => m.speaker_id === character.id).length;
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
            <NameTab testid={`cast-card-name-${slug}`}>
              {unnamed ? "Unnamed speaker" : character.name}
            </NameTab>
            <DitherCover
              tone="peach"
              testid={`cast-card-cover-${slug}`}
              label={`${character.name} portrait placeholder`}
              className="cast-card-cover"
            />
            <span className="muted">
              {lineCount} line{lineCount === 1 ? "" : "s"}
            </span>
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
