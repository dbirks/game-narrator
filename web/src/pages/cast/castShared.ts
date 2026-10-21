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

import { catalogApi, type Edition, type Message, type Profile } from "../../api/client";
import { voicesApi, type VoiceRevision } from "../../api/voices";

export const DEFAULT_BRIEF =
  "A clear, warm narrator voice with a steady cadence, suited to original synthetic dialogue.";

/** The storage key of the cast view toggle (UX-08: persisted per user). */
export const CAST_VIEW_STORAGE_KEY = "gn.cast.view";

export type CastView = "list" | "cards";

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
