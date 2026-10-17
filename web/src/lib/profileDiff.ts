/**
 * `src/lib/profileDiff.ts` — the client-side diff of one profile revision
 * against another (docs/spec/09 `/games/:id/publish`: "diff draft/published").
 *
 * The contract has no diff endpoint, and a derived revision mints fresh row
 * ids for every message and character (internal/store `NewRevision`), so the
 * stable identity for a line across revisions is its `source_message_id` —
 * the source-side row the importer reconciles on — and, for the lines a manual
 * entry or a no-source import left without one, its text. A diff keyed on the
 * store ids would show every line as removed plus added, which is the failure
 * this page exists to prevent.
 *
 * Pure and total: it is the function the publish page renders and the unit
 * test pins, and it never writes.
 */
import type { Character, Message } from "../api/client";

/** One revision's comparable content, as the REST surface returns it. */
export interface RevisionContent {
  profileId: string;
  revisionNum: number;
  messages: Message[];
  characters: Character[];
}

/** A matched pair whose writable text changed between the two revisions. */
export interface MessageChange {
  key: string;
  published: Message;
  draft: Message;
  /** The fields that differ, named the way the row names them. */
  changedFields: string[];
}

/** A matched character whose editable facts differ. */
export interface CharacterChange {
  key: string;
  published: Character;
  draft: Character;
  changedFields: string[];
}

export interface ProfileDiff {
  messages: {
    added: Message[];
    removed: Message[];
    changed: MessageChange[];
    unchanged: number;
  };
  characters: {
    added: Character[];
    removed: Character[];
    changed: CharacterChange[];
    unchanged: number;
  };
}

/**
 * The identity of a line across revisions. `source_message_id` is the importer's
 * reconciliation key and survives a derive; a line without one (manual entry,
 * no-source import) is identified by its text, because a re-derive of the same
 * text is the same line for curation purposes.
 */
export function messageKey(message: Message): string {
  if (message.source_message_id) return `sid:${message.source_message_id}`;
  const text = message.normalized_text || message.original_text || message.speech_text;
  return `text:${text}`;
}

function sortMessages(rows: Message[]): Message[] {
  return [...rows].sort((a, b) =>
    (a.original_text || a.speech_text).localeCompare(b.original_text || b.speech_text),
  );
}

function sortCharacters(rows: Character[]): Character[] {
  return [...rows].sort((a, b) => a.name.localeCompare(b.name));
}

/** Which of the fields a curator can actually move differ between the pair.
 *
 * `speaker_id` is a store row id and is re-minted on every derive, so it is
 * compared in a revision-stable form: the *name* of the character the row
 * points at (resolved through each revision's own roster), which is what a
 * curator means by "this line belongs to Marina". A line whose speaker row is
 * missing from the roster is compared by the raw id, so a dangling reference
 * still shows as a change rather than silently matching. */
function messageChangedFields(
  published: Message,
  draft: Message,
  publishedSpeaker: (id: string) => string,
  draftSpeaker: (id: string) => string,
): string[] {
  const fields: string[] = [];
  if (draft.speech_text !== published.speech_text) fields.push("speech_text");
  if (draft.original_text !== published.original_text) fields.push("original_text");
  if (draft.text_kind !== published.text_kind) fields.push("text_kind");
  if (
    speakerKey(published.speaker_id, publishedSpeaker) !==
    speakerKey(draft.speaker_id, draftSpeaker)
  ) {
    fields.push("speaker_id");
  }
  return fields;
}

/** The revision-stable identity of a line's speaker: the character's name, or
 *  the raw id when the roster cannot resolve it, or the empty string for a line
 *  with no speaker. */
function speakerKey(speakerId: string | undefined, resolve: (id: string) => string): string {
  if (!speakerId) return "";
  const name = resolve(speakerId);
  return name || `id:${speakerId}`;
}

/** A resolver that maps a character row id to its name within one revision. */
function speakerResolver(characters: Character[]): (id: string) => string {
  const byId = new Map(characters.map((character) => [character.id, character.name]));
  return (id: string) => byId.get(id) ?? "";
}

function characterChangedFields(published: Character, draft: Character): string[] {
  const fields: string[] = [];
  if (draft.status !== published.status) fields.push("status");
  if (draft.role !== published.role) fields.push("role");
  if (draft.personality_summary !== published.personality_summary) {
    fields.push("personality_summary");
  }
  if (JSON.stringify([...draft.aliases].sort()) !== JSON.stringify([...published.aliases].sort())) {
    fields.push("aliases");
  }
  return fields;
}

/**
 * Compare the draft against the published revision. `added` means "in the draft,
 * not in the published one"; `removed` the reverse — the direction a curator
 * reading "what will this publication change" wants, not a symmetric set.
 */
export function diffRevisions(published: RevisionContent, draft: RevisionContent): ProfileDiff {
  const publishedSpeaker = speakerResolver(published.characters);
  const draftSpeaker = speakerResolver(draft.characters);
  // Group the published side by key, consuming each row at most once so a
  // duplicated key cannot match twice.
  const publishedByKey = new Map<string, Message[]>();
  for (const message of published.messages) {
    const key = messageKey(message);
    const rows = publishedByKey.get(key);
    if (rows) rows.push(message);
    else publishedByKey.set(key, [message]);
  }

  const added: Message[] = [];
  const changed: MessageChange[] = [];
  let unchanged = 0;
  for (const message of draft.messages) {
    const key = messageKey(message);
    const available = publishedByKey.get(key);
    const match = available ? available.shift() : undefined;
    if (!match) {
      added.push(message);
      continue;
    }
    const fields = messageChangedFields(match, message, publishedSpeaker, draftSpeaker);
    if (fields.length > 0)
      changed.push({ key, published: match, draft: message, changedFields: fields });
    else unchanged += 1;
  }
  const removed: Message[] = [];
  for (const rows of publishedByKey.values()) removed.push(...rows);

  // Characters: the roster's identity is the name (the voices API itself is
  // keyed on the character slug, not the row id).
  const publishedChars = new Map<string, Character>();
  for (const character of published.characters) {
    publishedChars.set(character.name.toLowerCase(), character);
  }
  const charAdded: Character[] = [];
  const charChanged: CharacterChange[] = [];
  let charUnchanged = 0;
  const seen = new Set<string>();
  for (const character of draft.characters) {
    const key = character.name.toLowerCase();
    seen.add(key);
    const match = publishedChars.get(key);
    if (!match) {
      charAdded.push(character);
      continue;
    }
    const fields = characterChangedFields(match, character);
    if (fields.length > 0)
      charChanged.push({ key, published: match, draft: character, changedFields: fields });
    else charUnchanged += 1;
  }
  const charRemoved = published.characters.filter(
    (character) => !seen.has(character.name.toLowerCase()),
  );

  return {
    messages: {
      added: sortMessages(added),
      removed: sortMessages(removed),
      changed: sortChanges(changed),
      unchanged,
    },
    characters: {
      added: sortCharacters(charAdded),
      removed: sortCharacters(charRemoved),
      changed: charChanged.sort((a, b) => a.key.localeCompare(b.key)),
      unchanged: charUnchanged,
    },
  };
}

function sortChanges(changes: MessageChange[]): MessageChange[] {
  return [...changes].sort((a, b) =>
    (a.published.original_text || a.published.speech_text).localeCompare(
      b.published.original_text || b.published.speech_text,
    ),
  );
}

/** A one-line human summary the page can put next to the counts. */
export function diffSummary(diff: ProfileDiff): string {
  const parts: string[] = [
    `${diff.messages.added.length} added`,
    `${diff.messages.removed.length} removed`,
    `${diff.messages.changed.length} changed`,
    `${diff.messages.unchanged} unchanged`,
    `· ${diff.characters.added.length + diff.characters.removed.length + diff.characters.changed.length} character changes`,
  ];
  return parts.join(", ");
}
