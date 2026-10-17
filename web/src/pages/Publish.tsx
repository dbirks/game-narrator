import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router";

import {
  ApiError,
  catalogApi,
  refreshProfileEtag,
  type Character,
  type Edition,
  type Profile,
  type ProfileStatus,
} from "../api/client";
import { fetchAllMessages, fetchManifestSources } from "../api/profiles";
import { voicesApi, type VoiceRevision } from "../api/voices";
import { useAuth } from "../auth/auth-context";
import { EmptyState, ErrorState, Loading, ProblemList } from "../components/states";
import { diffRevisions, type ProfileDiff, type RevisionContent } from "../lib/profileDiff";
import {
  findRollbackTarget,
  rollbackButtonLabel,
  type RollbackTarget,
} from "../lib/publishHistory";
import { StatusBadge } from "./GameOverview";
import { PublishHistory, RightsPanel, VoiceGatePanel } from "./publishPanels";

/**
 * `/games/:id/publish` — docs/spec/09: "diff draft/published, provenance
 * conflicts, source rights and voice approval gate, publish transaction and
 * rollback to the previous published revision".
 *
 * The diff is computed client-side from the two revisions' message and roster
 * lists, because the contract has no diff endpoint and a derive mints fresh row
 * ids (internal/store `NewRevision`): `src/lib/profileDiff.ts` keys lines on
 * their `source_message_id`, the identity that survives a derive.
 *
 * The hard gate is the server's (internal/store `validateForPublish`): the
 * status must be `review`, references must resolve, and every source the
 * content rests on must carry a publishable rights position. Every refusal
 * finding the server lists is shown, not truncated; the rights and voice
 * panels preview the two findings a curator most often has to fix before the
 * button is worth pressing.
 *
 * "Rollback" is implemented the way the revision model allows it: publishing a
 * new revision derived from the previously published one, which the history
 * records as its `superseded_profile_id`. The button derives from that
 * target — never from the revision that is live now — and is hidden when the
 * edition has been published at most once; the explanation says what
 * completes the rollback, because publishing a revision that is not in
 * `review` is a refusal the page must not invite.
 */
export function PublishPage() {
  const { id = "" } = useParams();
  const [params] = useSearchParams();
  const profileParam = params.get("profile");
  const { atLeast } = useAuth();
  const queryClient = useQueryClient();
  const [refused, setRefused] = useState<string[]>([]);
  const [deriveError, setDeriveError] = useState<unknown>(null);

  const gameQuery = useQuery({
    queryKey: ["game", id],
    queryFn: ({ signal }) => catalogApi.getGame(id, signal),
  });

  const draftQuery = useQuery({
    queryKey: ["publish", "draft", id, profileParam],
    queryFn: async ({ signal }): Promise<Profile | null> => {
      if (profileParam) return catalogApi.getProfile(profileParam, signal);
      // No revision named: the newest draft or review revision of the game's
      // first edition is the thing a curator is most likely reviewing.
      const editions = (
        (await catalogApi.listEditions(id, signal)) as unknown as { editions: Edition[] }
      ).editions;
      for (const edition of editions) {
        const profiles = (
          (await catalogApi.listProfiles(edition.id, signal)) as unknown as { profiles: Profile[] }
        ).profiles;
        const candidates = profiles
          .filter((profile) => profile.status === "draft" || profile.status === "review")
          .sort((a, b) => b.revision_num - a.revision_num);
        if (candidates[0]) return candidates[0];
      }
      return null;
    },
  });

  const draft = draftQuery.data;
  const editionQuery = useQuery({
    queryKey: ["publish", "edition", draft?.edition_id],
    queryFn: async ({ signal }) => {
      const editions = (
        (await catalogApi.listEditions(id, signal)) as unknown as { editions: Edition[] }
      ).editions;
      return editions.find((edition) => edition.id === draft?.edition_id) ?? null;
    },
    enabled: Boolean(draft?.edition_id),
  });
  const publishedQuery = useQuery({
    queryKey: ["publish", "published", editionQuery.data?.id],
    queryFn: async ({ signal }): Promise<Profile | null> => {
      if (!editionQuery.data) return null;
      try {
        return (await catalogApi.getPublished(editionQuery.data.id, signal)) as Profile;
      } catch (error) {
        if (error instanceof ApiError && error.status === 404) return null;
        throw error;
      }
    },
    enabled: Boolean(editionQuery.data?.id),
  });
  // Every revision of the edition, so a publication-history row's profile id
  // can be shown by its revision number. The audit row itself carries only a
  // row version (the ETag), which is a concurrency token, not a revision.
  const editionProfilesQuery = useQuery({
    queryKey: ["profiles", editionQuery.data?.id],
    queryFn: async ({ signal }): Promise<Profile[]> => {
      const editionId = editionQuery.data?.id;
      if (!editionId) return [];
      const list = (await catalogApi.listProfiles(editionId, signal)) as unknown as {
        profiles: Profile[];
      };
      return list.profiles;
    },
    enabled: Boolean(editionQuery.data?.id),
  });
  const revisionById = useMemo(
    () =>
      new Map(
        (editionProfilesQuery.data ?? []).map((profile) => [profile.id, profile.revision_num]),
      ),
    [editionProfilesQuery.data],
  );

  const content = (profile: Profile | null): Promise<RevisionContent | null> => {
    if (!profile) return Promise.resolve(null);
    return Promise.all([fetchAllMessages(profile.id), catalogApi.getCharacters(profile.id)]).then(
      ([messages, characters]) =>
        ({
          profileId: profile.id,
          revisionNum: profile.revision_num,
          messages,
          characters: (characters as unknown as { characters: Character[] }).characters,
        }) as RevisionContent,
    );
  };
  const draftContentQuery = useQuery({
    queryKey: ["publish", "content", draft?.id],
    queryFn: () => content(draft ?? null),
    enabled: Boolean(draft?.id),
  });
  const publishedContentQuery = useQuery({
    queryKey: ["publish", "content", publishedQuery.data?.id],
    queryFn: () => content(publishedQuery.data ?? null),
    enabled: Boolean(publishedQuery.data?.id),
  });

  const rightsQuery = useQuery({
    queryKey: ["publish", "rights", draft?.id],
    queryFn: ({ signal }) => fetchManifestSources(draft?.id as string, signal),
    enabled: Boolean(draft?.id),
  });
  const actionsQuery = useQuery({
    queryKey: ["publish", "actions", draft?.id],
    queryFn: ({ signal }) => catalogApi.listPublishActions(draft?.id as string, signal),
    enabled: Boolean(draft?.id),
  });

  const voicesQuery = useQuery({
    queryKey: ["publish", "voices", draft?.id],
    queryFn: async ({ signal }): Promise<Map<string, VoiceRevision | null>> => {
      const characters = (draftContentQuery.data?.characters ?? []) as Character[];
      const byName = new Map<string, VoiceRevision | null>();
      for (const character of characters) {
        try {
          const rows = (await voicesApi.listVoices(
            character.name,
            signal,
          )) as unknown as VoiceRevision[];
          let latest: VoiceRevision | null = null;
          for (const row of rows) {
            if (!latest || row.revision > latest.revision) latest = row;
          }
          byName.set(character.name, latest);
        } catch {
          byName.set(character.name, null);
        }
      }
      return byName;
    },
    enabled: Boolean(draftContentQuery.data),
  });

  const diff: ProfileDiff | null =
    publishedContentQuery.data && draftContentQuery.data
      ? diffRevisions(publishedContentQuery.data, draftContentQuery.data)
      : null;

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["publish"] });
    void queryClient.invalidateQueries({ queryKey: ["profiles"] });
  };

  const move = useMutation({
    mutationFn: async (to: ProfileStatus) => {
      setRefused([]);
      await refreshProfileEtag(draft?.id as string);
      return catalogApi.setStatus(draft?.id as string, to);
    },
    onSuccess: refresh,
  });
  const publish = useMutation({
    mutationFn: async () => {
      setRefused([]);
      await refreshProfileEtag(draft?.id as string);
      return catalogApi.publish(draft?.id as string);
    },
    onSuccess: refresh,
    onError: (error) => {
      if (error instanceof ApiError && error.problems.length > 0) setRefused(error.problems);
    },
  });
  const rollbackTarget: RollbackTarget | null =
    publishedQuery.data && actionsQuery.data
      ? findRollbackTarget(actionsQuery.data.publish_actions, publishedQuery.data.id, revisionById)
      : null;
  const rollback = useMutation({
    mutationFn: (profileId: string) => catalogApi.createRevision(profileId),
    onSuccess: (created) => {
      setDeriveError(null);
      refresh();
      const url = new URL(window.location.href);
      url.searchParams.set("profile", (created as Profile).id);
      window.history.replaceState(null, "", url.toString());
    },
    onError: (error) => {
      setDeriveError(error);
      setRefused([]);
    },
  });

  if (gameQuery.isPending || draftQuery.isPending) {
    return (
      <main>
        <h1>Publication review</h1>
        <Loading label="Loading the revision…" />
      </main>
    );
  }
  if (gameQuery.isError || draftQuery.isError) {
    return (
      <main>
        <h1>Publication review</h1>
        <ErrorState
          error={gameQuery.error ?? draftQuery.error}
          action="Cannot open the publication review"
        />
      </main>
    );
  }

  if (!draft) {
    return (
      <main>
        <h1>Publication review</h1>
        <EmptyState title="Nothing to review">
          <p>
            This game has no draft or review-ready revision. Derive one from a published revision in
            the <Link to={`/games/${id}/overview`}>overview</Link>, then come back.
          </p>
        </EmptyState>
      </main>
    );
  }

  const loading =
    editionQuery.isPending || publishedContentQuery.isPending || draftContentQuery.isPending;
  const canWrite = atLeast("curator");
  const canPublish = draft.next_states.includes("published");
  const canReview = draft.next_states.includes("review");

  return (
    <main>
      <h1 data-testid="publish-title">Publication review</h1>
      <p className="muted">
        <Link to={`/games/${id}/overview`}>{gameQuery.data?.title ?? id}</Link>
        {" · "}draft revision{" "}
        <strong data-testid="publish-draft-revision">{draft.revision_num}</strong>{" "}
        <StatusBadge status={draft.status} />
        {publishedQuery.data ? (
          <>
            {" · published r"}
            <strong data-testid="publish-published-revision">
              {publishedQuery.data.revision_num}
            </strong>
          </>
        ) : (
          " · nothing is published yet"
        )}
      </p>

      {loading ? <Loading label="Comparing the revisions…" /> : null}

      {publishedQuery.data === null ? (
        <p className="muted" data-testid="publish-no-published">
          Nothing is published for this edition yet: the first publish ships this revision as-is, so
          there is no published side to diff against.
        </p>
      ) : diff ? (
        <DiffPanel diff={diff} />
      ) : (
        <p className="muted">The diff appears once both revisions have loaded.</p>
      )}

      {rightsQuery.data ? (
        <RightsPanel sources={rightsQuery.data} />
      ) : rightsQuery.isError ? (
        <ErrorState error={rightsQuery.error} action="Cannot load the source rights" />
      ) : null}

      {voicesQuery.data ? (
        <VoiceGatePanel
          byName={voicesQuery.data}
          characters={draftContentQuery.data?.characters ?? []}
        />
      ) : null}

      <PublishHistory
        data={actionsQuery.data ?? null}
        loading={actionsQuery.isPending}
        error={actionsQuery.error}
        revisionById={revisionById}
      />

      <ProblemList problems={refused} />
      {move.isError ? <ErrorState error={move.error} action="Cannot move the revision" /> : null}
      {publish.isError && refused.length === 0 ? (
        <ErrorState error={publish.error} action="Cannot publish this revision" />
      ) : null}
      {deriveError ? <ErrorState error={deriveError} action="Cannot derive the rollback" /> : null}

      {canWrite ? (
        <div className="publish-actions" data-testid="publish-actions">
          {canReview ? (
            <button
              type="button"
              onClick={() => move.mutate("review")}
              disabled={move.isPending}
              data-testid="publish-to-review"
            >
              {move.isPending ? "Moving…" : "Move to review"}
            </button>
          ) : null}
          {canPublish ? (
            <button
              type="button"
              onClick={() => publish.mutate()}
              disabled={publish.isPending}
              data-testid="publish-button"
            >
              {publish.isPending ? "Publishing…" : "Publish this revision"}
            </button>
          ) : null}
          {rollbackTarget ? (
            <RollbackBlock
              label={rollbackButtonLabel(rollbackTarget)}
              deriving={rollback.isPending}
              onRollback={() => rollback.mutate(rollbackTarget.profileId)}
            />
          ) : publishedQuery.data ? (
            <p className="muted" data-testid="rollback-unavailable">
              No previously published revision to roll back to: this edition&apos;s first publish is
              still live, so there is nothing to go back to.
            </p>
          ) : (
            <p className="muted">
              Rollback needs a published revision to derive from; nothing is live yet.
            </p>
          )}
        </div>
      ) : (
        <p className="muted">
          Publication needs the curator role; the server enforces it either way.
        </p>
      )}
    </main>
  );
}

function DiffPanel({ diff }: { diff: ProfileDiff }) {
  const { messages, characters } = diff;
  return (
    <section className="panel" data-testid="diff-panel">
      <h2>Changes against the published revision</h2>
      <p data-testid="diff-summary">
        <span data-testid="diff-added">{messages.added.length} added</span> ·{" "}
        <span data-testid="diff-removed">{messages.removed.length} removed</span> ·{" "}
        <span data-testid="diff-changed">{messages.changed.length} changed</span> ·{" "}
        <span data-testid="diff-unchanged">{messages.unchanged} unchanged</span> ·{" "}
        <span data-testid="diff-characters">
          {characters.added.length + characters.removed.length + characters.changed.length}{" "}
          character changes
        </span>
      </p>
      {messages.changed.length > 0 ? (
        <table className="diff-changes" data-testid="diff-changed-rows">
          <thead>
            <tr>
              <th>Line</th>
              <th>Was</th>
              <th>Now</th>
            </tr>
          </thead>
          <tbody>
            {messages.changed.map((change) => (
              <tr key={change.key}>
                <td>
                  <code>{change.key}</code>{" "}
                  <span className="muted">({change.changedFields.join(", ")})</span>
                </td>
                <td>{change.published.speech_text || change.published.original_text}</td>
                <td>{change.draft.speech_text || change.draft.original_text}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
      {messages.added.length > 0 ? (
        <ul data-testid="diff-added-rows">
          {messages.added.map((message) => (
            <li key={message.id}>{message.speech_text || message.original_text}</li>
          ))}
        </ul>
      ) : null}
      {messages.removed.length > 0 ? (
        <ul data-testid="diff-removed-rows">
          {messages.removed.map((message) => (
            <li key={message.id}>{message.speech_text || message.original_text}</li>
          ))}
        </ul>
      ) : null}
      {characters.changed.length > 0 ||
      characters.added.length > 0 ||
      characters.removed.length > 0 ? (
        <p data-testid="diff-character-changes">
          {characters.added.map((c) => `${c.name} (added)`).join(", ")}
          {characters.added.length && characters.changed.length ? ", " : ""}
          {characters.changed
            .map((c) => `${c.draft.name} (${c.changedFields.join("/")})`)
            .join(", ")}
          {characters.changed.length && characters.removed.length ? ", " : ""}
          {characters.removed.map((c) => `${c.name} (removed)`).join(", ")}
        </p>
      ) : null}
    </section>
  );
}

function RollbackBlock({
  label,
  deriving,
  onRollback,
}: {
  label: string;
  deriving: boolean;
  onRollback: () => void;
}) {
  return (
    <div className="rollback" data-testid="rollback-block">
      <button type="button" onClick={onRollback} disabled={deriving} data-testid="rollback-derive">
        {deriving ? "Deriving…" : label}
      </button>
      <p className="muted">
        A published revision is immutable, so rollback means publishing a new revision derived from
        the previous one: this derives that draft. Completing the rollback is one &ldquo;move to
        review&rdquo; and one publish of the derived revision — the history keeps both, and nothing
        shipped is rewritten.
      </p>
    </div>
  );
}
