/**
 * `src/pages/cast/castShared.ts` — the shared helpers of the merged Cast page
 * (docs/design/plum-peach 2d, ADR-0093): voice-state derivation over a
 * character's revisions, the bounded job poll, attribution badges, and the
 * page's default-profile resolver (the newest revision of the first edition,
 * the same rule the dialogue page applies).
 *
 * "Current" follows the server's runtime rule (ADR-0005/ADR-0109): the
 * highest approved revision is the take the runtime speaks. "Working" is the
 * newest revision of any state — a draft (the job is running) or an
 * auditioning take (pending review) is what the detail panel operates on.
 */

import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";

import {
  catalogApi,
  type Character,
  type Edition,
  type Message,
  type Profile,
} from "../../api/client";
import { voicesApi, type VoiceRevision } from "../../api/voices";

export const DEFAULT_BRIEF =
  "A clear, warm narrator voice with a steady cadence, suited to original synthetic dialogue.";

/** The profile's consent default (docs/spec/14, ADR-0109): the record the
 *  auto-cast approval attributes and the approval's prefilled record. */
export const CONSENT_DEFAULT = "original_synthetic_design";

/**
 * The shared voiced-count definition (ADR-0134), mirrored on the client the
 * way the server derives it (Store.ProfileCharacterVoiceCounts, used by the
 * Library read and the status bar): the denominator is the profile's active
 * speaking characters — rows not retired that hold at least one assigned
 * line (a retired row never counts, and a roster slot nobody speaks is not
 * a speaker) — and the numerator the subset of those carrying at least one
 * voice revision in a state other than retired (a draft or auditioning take
 * counts: the line has a voice; only retired means "no voice"). The voice
 * side joins on the character's **name**, the voice surface's identity
 * (UNIQUE (character, revision), migration 0009) — never the row id.
 *
 * The page's header chip and its filter counts derive from this one pair, so
 * the Cast screen cannot disagree with the Library card about how much of a
 * game has a voice; castShared.test.ts pins the same fixture shape the Go
 * unit test pins.
 */
export function castCounts(
  roster: Character[],
  messages: Message[],
  voices: VoiceRevision[],
): { speakers: number; voiced: number } {
  const lineCount = new Map<string, number>();
  for (const message of messages) {
    if (!message.speaker_id) continue;
    lineCount.set(message.speaker_id, (lineCount.get(message.speaker_id) ?? 0) + 1);
  }
  let speakers = 0;
  let voiced = 0;
  for (const character of roster) {
    if (character.status === "retired") continue;
    if ((lineCount.get(character.id) ?? 0) === 0) continue; // not a speaker
    speakers += 1;
    if (revisionsOfAll(voices, character.name).some((voice) => voice.state !== "retired")) {
      voiced += 1;
    }
  }
  return { speakers, voiced };
}

/** The evidence shape the dump's own linkage writes (method `source_field`):
 *  the import attributed the line to a speaker it names only as an
 *  identifier. The data-driven half of the roster's "unnamed" predicate —
 *  a name pattern here would be a game-specific fact (docs/spec/04 keeps
 *  those out of the UI). */
export function hasSourceFieldEvidence(messages: Message[]): boolean {
  return messages.some((message) =>
    (message.attribution_evidence as Array<Record<string, unknown>> | undefined)?.some(
      (entry) => entry?.method === "source_field",
    ),
  );
}

/**
 * The roster's "unnamed speaker" predicate (ADR-0134, the old speaker-
 * placeholder section's own rule): a proposed row whose assigned lines carry
 * the source's own linkage — the import's placeholder for a speaker the
 * source names only as an identifier. Those rows are the screen's
 * "Unnamed speaker · n lines" rows, and their primary action is **Name**.
 */
export function isUnnamedSpeaker(character: Character, messages: Message[]): boolean {
  if (character.status !== "proposed") return false;
  const assigned = messages.filter((message) => message.speaker_id === character.id);
  return assigned.length > 0 && hasSourceFieldEvidence(assigned);
}

/** The line count of one roster row (the row's "n lines" figure). */
export function lineCountOf(character: Character, messages: Message[]): number {
  return messages.filter((message) => message.speaker_id === character.id).length;
}

/**
 * The roster's presentation order (ADR-0134): named and voiced first
 * (line count descending, then name), then named needs-a-voice (line count
 * descending), then unnamed voiced, then unnamed needs-a-voice. Retired rows
 * sort last, under everything, so the default view ("All") ends with the
 * dead rows and the `Retired (n)` filter is where they live. The server
 * returns the roster by id (the content write's rule); the order is the
 * screen's job.
 */
export function castOrder(
  roster: Character[],
  messages: Message[],
  voices: VoiceRevision[],
): Character[] {
  const voicedOf = (character: Character): boolean =>
    revisionsOfAll(voices, character.name).some((voice) => voice.state !== "retired");
  const rank = (character: Character): number => {
    if (character.status === "retired") return 4;
    const unnamed = isUnnamedSpeaker(character, messages);
    const voiced = voicedOf(character);
    if (!unnamed && voiced) return 0;
    if (!unnamed) return 1; // named, needs a voice
    if (voiced) return 2; // unnamed, voiced
    return 3; // unnamed, needs a voice
  };
  return [...roster].sort((a, b) => {
    const rankDiff = rank(a) - rank(b);
    if (rankDiff !== 0) return rankDiff;
    // Within a group: the retired group stays in the server's row order
    // (nothing about a dead row is worth ranking); the live groups rank by
    // line count descending, then name, so the loudest rows lead.
    if (a.status === "retired" && b.status === "retired") return 0;
    const lines = lineCountOf(b, messages) - lineCountOf(a, messages);
    if (lines !== 0) return lines;
    return a.name.localeCompare(b.name);
  });
}

/** The roster's filter chips (ADR-0134): the counts are the chip's own
 *  figures, and `Retired` is the one chip that opens a hidden world —
 *  retired rows are out of every other chip and of the default view. */
export const CAST_FILTERS = [
  { id: "all", label: "All" },
  { id: "needs", label: "Needs a voice" },
  { id: "voiced", label: "Voiced" },
  { id: "unnamed", label: "Unnamed" },
  { id: "retired", label: "Retired" },
] as const;

export type CastFilter = (typeof CAST_FILTERS)[number]["id"];

/** The rows one chip shows: `All` is everything that is not retired (the
 *  retired rows hide behind their own chip), `Retired` is just them. */
export function castFilterOf(
  character: Character,
  filter: CastFilter,
  messages: Message[],
  voices: VoiceRevision[],
): boolean {
  const retired = character.status === "retired";
  if (filter === "retired") return retired;
  if (retired) return false;
  const voiced = revisionsOfAll(voices, character.name).some((voice) => voice.state !== "retired");
  switch (filter) {
    case "all":
      return true;
    case "needs":
      return !voiced;
    case "voiced":
      return voiced;
    case "unnamed":
      return isUnnamedSpeaker(character, messages);
  }
}

/** The chip's own count, over the roster the screen holds. */
export function castFilterCount(
  roster: Character[],
  filter: CastFilter,
  messages: Message[],
  voices: VoiceRevision[],
): number {
  return roster.filter((character) => castFilterOf(character, filter, messages, voices)).length;
}

/** The name search: a case-insensitive substring of the row's display name
 *  (the roster key of an unnamed row is part of the search target too — it
 *  is the only string that distinguishes its rows). */
export function castSearchMatch(character: Character, query: string): boolean {
  const needle = query.trim().toLowerCase();
  if (needle === "") return true;
  return character.name.toLowerCase().includes(needle);
}

/** The storage key of the cast view toggle (UX-08: persisted per user). */
export const CAST_VIEW_STORAGE_KEY = "gn.cast.view";

export type CastView = "list" | "cards";

export const CAST_FILTER_STORAGE_KEY = "gn.cast.filter";

/** The row's one primary action (ADR-0134): the label the row's button
 *  carries — **Listen** for a row the box speaks, **Pick a voice** for a
 *  named row without a live take, **Name** for the import's placeholder
 *  rows, and nothing for a retired row (it has no work to do). */
export function castRowAction(
  character: Character,
  messages: Message[],
  voices: VoiceRevision[],
): "listen" | "pick" | "name" | "none" {
  if (character.status === "retired") return "none";
  if (isUnnamedSpeaker(character, messages)) return "name";
  return revisionsOfAll(voices, character.name).some((voice) => voice.state !== "retired")
    ? "listen"
    : "pick";
}

export const slugOf = (name: string): string => encodeURIComponent(name);

/** One character's revisions, newest first (the page works on this order). */
export function revisionsOfAll(
  voices: VoiceRevision[] | undefined,
  character: string,
): VoiceRevision[] {
  return [...(voices ?? [])]
    .filter((voice) => voice.character === character)
    .sort((a, b) => b.revision - a.revision);
}

/**
 * The two revisions the detail panel cares about (ADR-0109, UX-03d):
 * `current` is the highest approved revision (the take the runtime speaks,
 * the one the player plays and the undo restores), `working` is the run the
 * panel is moving through: the newest draft or auditioning revision when a
 * run is in flight, otherwise the current itself. A retired newest revision
 * (the take an undo put away) is not a run — the panel keeps showing the
 * take that speaks.
 */
export function currentAndWorking(revisions: VoiceRevision[]): {
  current: VoiceRevision | null;
  working: VoiceRevision | null;
} {
  const current = revisions.find((r) => r.state === "approved") ?? null;
  const working =
    revisions.find((r) => r.state === "draft" || r.state === "auditioning") ?? current;
  return { current, working };
}

/** A lower approved take exists: the one-step undo is available (UX-03d). */
export function hasPreviousApproved(revisions: VoiceRevision[], current: VoiceRevision): boolean {
  return revisions.some(
    (r) => r.state === "approved" && r.revision < current.revision && r.id !== current.id,
  );
}

/** The roster's per-row voice state (the list view's state pill). The states
 *  follow docs/design/plum-peach UX-03: a take that is queued for its design
 *  job reads "queued"; a take with clips waiting for review reads
 *  "pending review"; an approved take reads voiced with its version. */
export function voiceStateOf(revisions: VoiceRevision[]): {
  label: string;
  tone: "none" | "pending" | "voiced";
} {
  const { current, working } = currentAndWorking(revisions);
  if (working !== null && working.state !== "approved") {
    return { label: working.state === "draft" ? "queued" : "pending review", tone: "pending" };
  }
  if (current !== null) return { label: `voiced · v${current.revision}`, tone: "voiced" };
  return { label: "no voice", tone: "none" };
}

/** The badge class of a voice revision state (index.css's legacy badges). */
export function badgeClass(state: string): string {
  if (state === "approved" || state === "ready") return "published";
  if (state === "invalid" || state === "retired") return "archived";
  if (state === "auditioning") return "review";
  return "draft";
}

/** The attribution badge (docs/spec/09): verified only on confirmed evidence. */
export function attributionOf(messages: Message[]): "verified" | "inferred" | "none" {
  const assigned = messages.filter((message) => message.speaker_id);
  if (assigned.length === 0) return "none";
  return assigned.every((message) => message.attribution_status === "confirmed")
    ? "verified"
    : "inferred";
}

export function attributionStatusList(messages: Message[]): string {
  return [...new Set(messages.map((message) => message.attribution_status))].join("/");
}

/**
 * Poll one revision until it leaves the draft state (the audition job lands
 * the clips), then hand the refresh off. Bounded: a job that never lands is a
 * server fault, not a page that spins forever.
 */
export async function pollVoice(voiceId: string, done: () => void, tries = 24): Promise<void> {
  for (let i = 0; i < tries; i++) {
    try {
      const d = await voicesApi.getVoice(voiceId);
      if (d.state !== "draft") {
        done();
        return;
      }
    } catch {
      // The draft row may lag the 202 by a tick; the poll retries rather than
      // treating a 404 as a failure.
    }
    await new Promise((r) => setTimeout(r, 750));
  }
  done();
}

// The page's own default-profile resolver: the newest revision of the first
// edition, the same rule the dialogue page applies.
export function useDefaultProfile(gameId: string, enabled: boolean): string | null {
  const editions = useQuery({
    queryKey: ["editions", gameId],
    queryFn: ({ signal }) => catalogApi.listEditions(gameId, signal),
    enabled,
  });
  const editionId = (editions.data as { editions: Edition[] } | undefined)?.editions?.[0]?.id;
  const profiles = useQuery({
    queryKey: ["profiles", editionId],
    queryFn: ({ signal }) => catalogApi.listProfiles(editionId as string, signal),
    enabled: editionId !== undefined,
  });
  return useMemo(() => {
    const list = (profiles.data as { profiles: Profile[] } | undefined)?.profiles ?? [];
    if (list.length === 0) return null;
    const sorted = [...list].sort((a, b) =>
      a.revision_num > b.revision_num ? -1 : a.revision_num < b.revision_num ? 1 : 0,
    );
    return sorted[0]?.id ?? null;
  }, [profiles.data]);
}
