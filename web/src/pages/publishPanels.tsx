/**
 * `src/pages/publishPanels.tsx` — the three review panels of the publication
 * page, split from `Publish.tsx` for the file-size policy: source rights with
 * the unpublishable rows highlighted, the per-character voice approval state,
 * and the edition's publication history.
 *
 * The rights panel previews the server's hard gate (internal/store
 * `validateForPublish`: a source the content rests on whose rights position is
 * not `user_supplied` or `licensed` is refused with
 * CONTENT_RIGHTS_REVIEW_REQUIRED). The voice panel is informational: in this
 * build the voice approval is a curation gate the curator checks, and the
 * server's refusal findings — always shown, never truncated — remain the
 * authority on what actually ships.
 */
import type { Character, PublishAction } from "../api/client";
import { EmptyState, ErrorState, Loading } from "../components/states";
import type { ManifestSource } from "../api/profiles";
import type { VoiceRevision } from "../api/voices";
import { historyRowLabel, supersededDetail, type RevisionById } from "../lib/publishHistory";

/** The rights positions that the publish gate accepts (docs/spec/14). */
const PUBLISHABLE_RIGHTS = new Set(["user_supplied", "licensed"]);

export function RightsPanel({ sources }: { sources: ManifestSource[] }) {
  const unpublishable = sources.filter((source) => !PUBLISHABLE_RIGHTS.has(source.rights));
  return (
    <section className="panel" data-testid="rights-panel">
      <h2>Source rights</h2>
      {sources.length === 0 ? (
        <EmptyState title="No sources recorded">
          <p>This revision carries no provenance rows, so the rights gate has nothing to review.</p>
        </EmptyState>
      ) : (
        <table className="rights" data-testid="rights-table">
          <thead>
            <tr>
              <th>Source</th>
              <th>Type</th>
              <th>Rights</th>
              <th>Trust</th>
            </tr>
          </thead>
          <tbody>
            {sources.map((source, index) => {
              const blocked = !PUBLISHABLE_RIGHTS.has(source.rights);
              return (
                <tr
                  key={source.source_id || index}
                  className={blocked ? "rights-row rights-blocked" : "rights-row"}
                  data-testid={`rights-row-${index}`}
                >
                  <td>
                    {source.uri ? (
                      source.uri
                    ) : (
                      <code>{source.sha256.slice(0, 16) || "unnamed"}</code>
                    )}
                  </td>
                  <td>{source.type || "—"}</td>
                  <td>
                    <span className={`badge ${blocked ? "badge-failed" : "badge-succeeded"}`}>
                      {source.rights || "unknown"}
                    </span>
                    {blocked ? (
                      <span className="muted" data-testid={`rights-blocked-${index}`}>
                        {" "}
                        — not publishable
                      </span>
                    ) : null}
                  </td>
                  <td>{typeof source.trust === "number" ? source.trust.toFixed(2) : "—"}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
      {unpublishable.length > 0 ? (
        <p className="error" role="alert" data-testid="rights-warning">
          {unpublishable.length} source{unpublishable.length === 1 ? "" : "s"} carry a rights
          position the publish gate will refuse (docs/spec/14: unknown rights never ship). Resolve
          the position or remove the dependency before publishing.
        </p>
      ) : sources.length > 0 ? (
        <p className="muted">All recorded sources carry a publishable rights position.</p>
      ) : null}
    </section>
  );
}

/** One character's voice state, or null for none. */
export type VoiceStateByCharacter = Map<string, VoiceRevision | null>;

export function VoiceGatePanel({
  byName,
  characters,
}: {
  byName: VoiceStateByCharacter;
  characters: Character[];
}) {
  if (characters.length === 0) {
    return (
      <section className="panel" data-testid="voice-gate-panel">
        <h2>Voice approval</h2>
        <p className="muted">This revision has no roster, so there is nothing to voice.</p>
      </section>
    );
  }
  return (
    <section className="panel" data-testid="voice-gate-panel">
      <h2>Voice approval</h2>
      <p className="muted">
        The voice a character speaks with is approved per character on the voices page; the publish
        gate enforced by the server is the rights and status gate, so this table is the
        curator&apos;s checklist before a release that should speak in voice.
      </p>
      <table className="voice-gate" data-testid="voice-gate-table">
        <thead>
          <tr>
            <th>Character</th>
            <th>Latest voice</th>
            <th>State</th>
          </tr>
        </thead>
        <tbody>
          {characters.map((character) => {
            const voice = byName.get(character.name) ?? null;
            const approved = voice?.state === "approved";
            return (
              <tr
                key={character.id}
                className={approved ? "voice-row voice-approved" : "voice-row"}
                data-testid={`voice-gate-${encodeURIComponent(character.name)}`}
              >
                <td>{character.name}</td>
                <td>{voice ? `revision ${voice.revision}` : "none"}</td>
                <td>
                  <span className={`badge ${approved ? "badge-succeeded" : "badge-pending"}`}>
                    {voice ? voice.state : "no voice"}
                  </span>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </section>
  );
}

/**
 * The edition's publication history. The audit row's `result_row_version` is
 * the profile's row version (its ETag), not a revision number, so the primary
 * text resolves `profile_id` to a revision from `revisionById` (the edition's
 * profile list) and the row version is a secondary detail only.
 */
export function PublishHistory({
  data,
  loading,
  error,
  revisionById,
}: {
  data: { publish_actions: PublishAction[] } | null;
  loading: boolean;
  error: unknown;
  revisionById?: RevisionById;
}) {
  if (loading) {
    return (
      <section className="panel">
        <h2>Publication history</h2>
        <Loading label="Loading the history…" />
      </section>
    );
  }
  if (error) {
    return (
      <section className="panel">
        <h2>Publication history</h2>
        <ErrorState error={error} action="Cannot load the publication history" />
      </section>
    );
  }
  const actions = data?.publish_actions ?? [];
  return (
    <section className="panel" data-testid="publish-history">
      <h2>Publication history</h2>
      {actions.length === 0 ? (
        <p className="muted">No publication activity for this edition yet.</p>
      ) : (
        <ol className="publish-history-list">
          {actions.map((action) => {
            const label = historyRowLabel(action, revisionById);
            const superseded = supersededDetail(action, revisionById);
            return (
              <li
                key={action.id}
                className={`publish-action publish-action-${action.action}`}
                data-testid="publish-action-row"
              >
                <span
                  className={`badge ${action.result === "succeeded" ? "badge-succeeded" : "badge-failed"}`}
                >
                  {action.action}
                </span>{" "}
                <strong>{label.text}</strong>
                {label.rowVersion > 0 ? (
                  <span className="muted"> (row version {label.rowVersion})</span>
                ) : null}
                {superseded ? <span className="muted"> ({superseded})</span> : null}
                {" · "}
                {action.actor} · {action.created_at}
                {action.problems.length > 0 ? (
                  <ul className="problems">
                    {action.problems.map((problem) => (
                      <li key={problem}>{problem}</li>
                    ))}
                  </ul>
                ) : null}
              </li>
            );
          })}
        </ol>
      )}
    </section>
  );
}
