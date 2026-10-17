/**
 * `src/lib/publishHistory.ts` — the mapping between the contract's
 * `PublishAction` (internal/api/openapi/openapi.json) and what the publish
 * page shows: the history row's text and the rollback target.
 *
 * The audit row carries a `profile_id` and a *row version*, not a revision
 * number. `result_row_version` is the profile's concurrency token (the ETag,
 * moved once per write, ADR-0015): it climbs with every status move, so it is
 * not a revision, and rendering it as "r7" makes a publish read as revision 7
 * when the revision is 2. The revision number is resolved from the edition's
 * profile list (`GET /editions/{id}/profiles`), and an id that cannot be
 * resolved is shown as a short id, never as a fabricated revision number.
 *
 * The rollback target is the most recent *previously* published revision —
 * the revision a publish action retired (`superseded_profile_id`, which the
 * store fills in when the incumbent is archived in the same transaction,
 * internal/store `archiveIncumbent`) — never the revision that is live now.
 */
import type { PublishAction } from "../api/client";

/** Profile id → the revision number the edition's profile list reports. */
export type RevisionById = ReadonlyMap<string, number>;

/** The tail of an id: short enough to read in a row, long enough to be unique. */
export function shortId(id: string): string {
  return id.length > 8 ? id.slice(-8) : id;
}

export interface HistoryRowLabel {
  /** The row's primary text, e.g. "published revision 2". */
  text: string;
  /** The audit row's row version, which the page shows only as a secondary detail. */
  rowVersion: number;
}

const VERB: Record<PublishAction["action"], string> = {
  publish: "published",
  archive: "archived",
  rejected: "rejected",
};

/**
 * The row's primary text. The action names the decision — a publish the gate
 * refused is recorded as a `rejected` action (internal/store
 * `recordRejection`), so the text follows the action, not the result.
 */
export function historyRowLabel(
  action: Pick<
    PublishAction,
    "action" | "result_row_version" | "profile_id" | "superseded_profile_id"
  >,
  revisionById: RevisionById = new Map(),
): HistoryRowLabel {
  const number = revisionById.get(action.profile_id);
  const subject = number !== undefined ? `revision ${number}` : shortId(action.profile_id);
  return { text: `${VERB[action.action]} ${subject}`, rowVersion: action.result_row_version };
}

/**
 * The muted "(supersedes …)" detail of a publish row, or null for a row that
 * retired nothing (the edition's first publish).
 */
export function supersededDetail(
  action: Pick<PublishAction, "superseded_profile_id">,
  revisionById: RevisionById = new Map(),
): string | null {
  const id = action.superseded_profile_id;
  if (!id) return null;
  const number = revisionById.get(id);
  return number !== undefined ? `supersedes revision ${number}` : `supersedes ${shortId(id)}`;
}

export interface RollbackTarget {
  profileId: string;
  revisionNum: number | null;
}

/**
 * The revision a rollback derives from: the most recent *previously*
 * published revision, i.e. the `superseded_profile_id` of the newest
 * successful publish that is not the revision that is live now. Null when no
 * such revision exists — the edition has been published at most once — and
 * then there is nothing to roll back to, so the button is hidden.
 *
 * `actions` is the edition's history in the order the API returns it: newest
 * first (`GET /profiles/{id}/publish-actions`).
 */
export function findRollbackTarget(
  actions: PublishAction[] | null | undefined,
  currentPublishedId: string | null | undefined,
  revisionById: RevisionById = new Map(),
): RollbackTarget | null {
  for (const action of actions ?? []) {
    if (action.action !== "publish" || action.result !== "succeeded") continue;
    const id = action.superseded_profile_id;
    if (!id || id === currentPublishedId) continue;
    return { profileId: id, revisionNum: revisionById.get(id) ?? null };
  }
  return null;
}

/** The rollback button's label; never a fabricated revision number. */
export function rollbackButtonLabel(target: RollbackTarget): string {
  return target.revisionNum !== null
    ? `Roll back to revision ${target.revisionNum}`
    : "Roll back to the previous published revision";
}
