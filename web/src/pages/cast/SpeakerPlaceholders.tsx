/**
 * `src/pages/cast/SpeakerPlaceholders.tsx` — the speaker placeholders of the
 * merged Cast page (ADR-0070, carried over from the former Characters page
 * after the ADR-0093 merge). The rows an import created for speakers the
 * source names only as opaque ids: proposed characters whose assigned lines
 * carry the `source_field` evidence the dump's own linkage writes. The filter
 * is data-driven — a name pattern here would be a game-specific fact, and
 * docs/spec/04 keeps those out of the UI. The curator's link action retires
 * the placeholder and points its lines at the chosen character.
 */

import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { ApiError, catalogApi, type Character, type Message } from "../../api/client";

/**
 * The section: the placeholders an import left for speakers the source names
 * only as identifiers. Shown on the Cast page while the profile is a draft
 * and the curator can link (the page's caller applies both gates).
 */
export function SpeakerPlaceholderSection({
  profileId,
  characters,
  messages,
}: {
  profileId: string;
  characters: Character[];
  messages: Message[];
}) {
  const queryClient = useQueryClient();
  const [notice, setNotice] = useState<{ name: string; text: string } | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [error, setError] = useState<{ id: string; text: string } | null>(null);

  const placeholders = characters.filter((character) => {
    if (character.status !== "proposed") return false;
    return messages.some(
      (message) => message.speaker_id === character.id && hasSourceFieldEvidence(message),
    );
  });
  if (placeholders.length === 0) return null;
  const candidates = characters.filter((character) => character.status !== "retired");

  // The section, not the row, keeps the confirmation: the linked row retires
  // and leaves the section on the next read, and a confirmation that unmounts
  // with it would read as "nothing happened".
  const link = async (character: Character, targetId: string) => {
    if (!targetId) return;
    setBusyId(character.id);
    setError(null);
    try {
      const linked = await catalogApi.linkSpeaker(profileId, character.id, targetId);
      setNotice({
        name: character.name,
        text: `${character.name} is ${
          candidates.find((candidate) => candidate.id === targetId)?.name ?? "the target"
        }: ${linked.repointed_messages} line${linked.repointed_messages === 1 ? "" : "s"} now play as that character, and the placeholder is retired.`,
      });
      await queryClient.invalidateQueries({ queryKey: ["characters"] });
    } catch (err) {
      setError({
        id: character.id,
        text: err instanceof ApiError ? err.detail : "the link did not go through; try again.",
      });
    } finally {
      setBusyId(null);
    }
  };

  return (
    <section aria-label="Speaker placeholders" data-testid="speaker-placeholders">
      <h2>Speaker placeholders</h2>
      <p className="muted">
        The import attributed these lines to speakers the source names only as an identifier. Pick
        the character each one is and link them; the placeholder is retired and its lines move to
        the character you name.
      </p>
      <ul className="speaker-placeholder-list">
        {placeholders.map((character) => (
          <SpeakerPlaceholderRow
            key={character.id}
            character={character}
            candidates={candidates.filter((candidate) => candidate.id !== character.id)}
            lineCount={messages.filter((message) => message.speaker_id === character.id).length}
            busy={busyId === character.id}
            error={error?.id === character.id ? error.text : null}
            onLink={(targetId) => void link(character, targetId)}
          />
        ))}
      </ul>
      {notice ? (
        <p data-testid="speaker-link-last-result" role="status">
          {notice.text}
        </p>
      ) : null}
    </section>
  );
}

/** The evidence shape the dump's own linkage writes (method `source_field`). */
function hasSourceFieldEvidence(message: Message): boolean {
  return (message.attribution_evidence as Array<Record<string, unknown>>).some(
    (entry) => entry?.method === "source_field",
  );
}

function SpeakerPlaceholderRow({
  character,
  candidates,
  lineCount,
  busy,
  error,
  onLink,
}: {
  character: Character;
  candidates: Character[];
  lineCount: number;
  busy: boolean;
  error: string | null;
  onLink: (targetId: string) => void;
}) {
  const [targetId, setTargetId] = useState(candidates[0]?.id ?? "");
  return (
    <li data-testid={`speaker-placeholder-${encodeURIComponent(character.name)}`}>
      <strong>{character.name}</strong>
      <span className="badge badge-plain">
        {" "}
        {lineCount} line{lineCount === 1 ? "" : "s"}
      </span>
      {candidates.length === 0 ? (
        <span className="muted">no other character to link to; add one to the roster first</span>
      ) : (
        <label className="speaker-link-control">
          <span className="muted">is</span>
          <select
            data-testid={`speaker-placeholder-target-${encodeURIComponent(character.name)}`}
            value={targetId}
            onChange={(event) => setTargetId(event.target.value)}
          >
            {candidates.map((candidate) => (
              <option key={candidate.id} value={candidate.id}>
                {candidate.name}
              </option>
            ))}
          </select>
          <button
            data-testid={`speaker-placeholder-link-${encodeURIComponent(character.name)}`}
            onClick={() => onLink(targetId)}
            disabled={busy || !targetId}
          >
            Link
          </button>
        </label>
      )}
      {error ? (
        <span
          role="alert"
          data-testid={`speaker-placeholder-error-${encodeURIComponent(character.name)}`}
        >
          {error}
        </span>
      ) : null}
    </li>
  );
}
