import { useQuery } from "@tanstack/react-query";
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
import { EmptyState, ErrorState, Loading } from "../components/states";
import { StatusBadge } from "./GameOverview";

/**
 * `/games/:id/characters` — docs/spec/09: "portrait, verified context,
 * editable voice brief, pronunciations, assigned dialogues and conflict
 * review". This milestone renders what the catalogue API answers: the roster
 * of the selected revision with its aliases, the lines assigned to each name,
 * and the attribution badge that docs/spec/09 "UX robustness" asks for — a
 * line attributed `confirmed` is verified, anything else (proposed, unknown,
 * rejected, or no evidence at all) is labelled inferred, never presented as
 * fact.
 *
 * The `?profile=` param names the revision; without one the page watches the
 * edition's published revision, because that is what a session would actually
 * speak with.
 */
export function CharactersPage() {
  const { id = "" } = useParams();
  const [params] = useSearchParams();
  const profileParam = params.get("profile");

  const gameQuery = useQuery({
    queryKey: ["game", id],
    queryFn: ({ signal }) => catalogApi.getGame(id, signal),
  });

  const resolveProfile = useQuery({
    queryKey: ["characters", "resolve", id, profileParam],
    queryFn: async ({ signal }): Promise<Profile | null> => {
      if (profileParam) return catalogApi.getProfile(profileParam, signal);
      const editions = (
        (await catalogApi.listEditions(id, signal)) as unknown as { editions: Edition[] }
      ).editions;
      for (const edition of editions) {
        try {
          const published = await catalogApi.getPublished(edition.id, signal);
          if (published) return published as Profile;
        } catch (error) {
          if (!(error instanceof ApiError && error.status === 404)) throw error;
        }
      }
      return null;
    },
  });

  const profile = resolveProfile.data;
  const profileId = profile?.id;
  const charactersQuery = useQuery({
    queryKey: ["characters", profileId],
    queryFn: ({ signal }) => catalogApi.getCharacters(profileId as string, signal),
    enabled: Boolean(profileId),
  });
  const messagesQuery = useQuery({
    queryKey: ["characters", "messages", profileId],
    queryFn: ({ signal }) => fetchAllMessages(profileId as string, signal),
    enabled: Boolean(profileId),
  });

  if (gameQuery.isPending || resolveProfile.isPending) {
    return (
      <main>
        <h1>Characters</h1>
        <Loading label="Loading the roster…" />
      </main>
    );
  }
  if (gameQuery.isError) {
    return (
      <main>
        <h1>Characters</h1>
        <ErrorState error={gameQuery.error} action="Cannot load this game" />
      </main>
    );
  }

  return (
    <main>
      <h1 data-testid="characters-title">{`Characters · ${gameQuery.data?.title ?? id}`}</h1>
      <p className="muted">
        <Link to={`/games/${id}/overview`} data-testid="characters-back-link">
          Overview
        </Link>
        {profile ? (
          <>
            {" · revision "}
            <strong data-testid="characters-revision">{profile.revision_num}</strong>{" "}
            <StatusBadge status={profile.status} />
            {" · "}
            <Link
              to={`/games/${id}/voices?profile=${encodeURIComponent(profile.id)}`}
              data-testid="characters-voices-link"
            >
              Voices
            </Link>
          </>
        ) : null}
      </p>

      {profile ? null : (
        <EmptyState title="No revision to show">
          <p>This game has no edition with a published revision yet.</p>
        </EmptyState>
      )}

      {profile && charactersQuery.isPending ? <Loading label="Loading the roster…" /> : null}
      {profile && charactersQuery.isError ? (
        <ErrorState
          error={charactersQuery.error}
          action="Cannot load the roster"
          onRetry={() => void charactersQuery.refetch()}
        />
      ) : null}
      {profile && messagesQuery.isError ? (
        <ErrorState error={messagesQuery.error} action="Cannot load the assigned lines" />
      ) : null}

      {profile && charactersQuery.data && messagesQuery.data ? (
        <Roster characters={charactersQuery.data.characters} messages={messagesQuery.data} />
      ) : null}
    </main>
  );
}

interface RosterProps {
  characters: Character[];
  messages: Message[];
}

function Roster({ characters, messages }: RosterProps) {
  if (characters.length === 0) {
    return (
      <EmptyState title="No characters in this revision">
        <p>
          The roster is empty; import a script or derive a revision and annotate the speakers before
          a voice can be designed.
        </p>
      </EmptyState>
    );
  }
  return (
    <table className="roster" data-testid="character-table">
      <thead>
        <tr>
          <th>Name</th>
          <th>Aliases</th>
          <th>Assigned lines</th>
          <th>Attribution</th>
        </tr>
      </thead>
      <tbody>
        {characters.map((character) => (
          <CharacterRow key={character.id} character={character} messages={messages} />
        ))}
      </tbody>
    </table>
  );
}

/** The attribution badge (docs/spec/09): verified only on confirmed evidence. */
function attributionOf(messages: Message[]): "verified" | "inferred" | "none" {
  const assigned = messages.filter((message) => message.speaker_id);
  if (assigned.length === 0) return "none";
  return assigned.every((message) => message.attribution_status === "confirmed")
    ? "verified"
    : "inferred";
}

function CharacterRow({ character, messages }: { character: Character; messages: Message[] }) {
  const lines = messages.filter((message) => message.speaker_id === character.id);
  const attribution = attributionOf(lines);
  const slug = encodeURIComponent(character.name);
  return (
    <tr className="character-row" data-testid={`character-row-${slug}`}>
      <td>
        <strong>{character.name}</strong>
        <span className={`badge badge-plain character-status`}> {character.status}</span>
      </td>
      <td>
        {character.aliases.length === 0 ? (
          <span className="muted">—</span>
        ) : (
          character.aliases.join(", ")
        )}
      </td>
      <td data-testid={`character-lines-${slug}`}>{lines.length}</td>
      <td data-testid={`character-attribution-${slug}`}>
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
    </tr>
  );
}

function attributionStatusList(lines: Message[]): string {
  const distinct = [...new Set(lines.map((message) => message.attribution_status))].join("/");
  return distinct;
}
