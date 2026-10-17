/**
 * `src/lib/publishHistory.test.ts` — synthetic-stub unit tests for the
 * publish-history mapping (docs/spec/12 (a): hand-built fixture rows, no
 * server).
 *
 * The contract's `PublishAction` carries a `profile_id` and a row version,
 * not a revision number (internal/api/openapi/openapi.json), so the mapping
 * under test is: action + the edition's profile list → the row's text, and
 * the edition's history → the rollback target. The regression this guards
 * against: rendering `result_row_version` (the ETag's decimal) as a revision
 * number.
 */
import { describe, expect, it } from "vitest";

import type { PublishAction } from "../api/client";
import {
  findRollbackTarget,
  historyRowLabel,
  rollbackButtonLabel,
  shortId,
  supersededDetail,
} from "./publishHistory";

/**
 * The history the walk in e2e/m5-part1.spec.ts produces, newest first:
 * publish revision 2 (retiring revision 1), the retirement's own audit row,
 * then the first publish.
 */
const REV = new Map([
  ["profile-1", 1],
  ["profile-2", 2],
]);

function action(overrides: Partial<PublishAction> = {}): PublishAction {
  return {
    id: "01ACTION000000000000000000",
    edition_id: "01EDITION0000000000000000",
    profile_id: "profile-2",
    action: "publish",
    result: "succeeded",
    actor: "curator",
    result_row_version: 7,
    problems: [],
    created_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

describe("historyRowLabel", () => {
  it("renders the action's verb with the revision resolved from the profile list", () => {
    expect(historyRowLabel(action(), REV)).toEqual({
      text: "published revision 2",
      rowVersion: 7,
    });
    expect(historyRowLabel(action({ action: "archive", profile_id: "profile-1" }), REV).text).toBe(
      "archived revision 1",
    );
    expect(
      historyRowLabel(
        action({ action: "rejected", result: "rejected", profile_id: "profile-1" }),
        REV,
      ).text,
    ).toBe("rejected revision 1");
  });

  it("shows a short id, never a fake revision number, when the profile cannot be resolved", () => {
    const longId = "01AB01CD01EF0123456789";
    const label = historyRowLabel(action({ profile_id: longId }), REV);
    expect(label.text).toBe(`published ${shortId(longId)}`);
    expect(label.text).not.toContain("revision");
  });

  it("keeps the row version out of the primary text", () => {
    const label = historyRowLabel(action({ result_row_version: 42 }), REV);
    expect(label.text).toBe("published revision 2");
    expect(label.rowVersion).toBe(42);
  });
});

describe("supersededDetail", () => {
  it("names the retired revision when the profile list resolves it", () => {
    expect(supersededDetail(action({ superseded_profile_id: "profile-1" }), REV)).toBe(
      "supersedes revision 1",
    );
  });

  it("is null for the edition's first publish and falls back to a short id", () => {
    expect(supersededDetail(action(), REV)).toBeNull();
    expect(supersededDetail(action({ superseded_profile_id: "01AB01CD01EF0123456789" }), REV)).toBe(
      `supersedes ${shortId("01AB01CD01EF0123456789")}`,
    );
  });
});

describe("findRollbackTarget", () => {
  it("picks the revision the newest publish retired, not the live one", () => {
    const history = [
      action({ superseded_profile_id: "profile-1", result_row_version: 7 }),
      action({ action: "archive", profile_id: "profile-1", result_row_version: 8 }),
      action({ profile_id: "profile-1", result_row_version: 4 }),
    ];
    expect(findRollbackTarget(history, "profile-2", REV)).toEqual({
      profileId: "profile-1",
      revisionNum: 1,
    });
  });

  it("is null when the edition has been published at most once", () => {
    const firstOnly = [action({ profile_id: "profile-1", result_row_version: 4 })];
    expect(findRollbackTarget(firstOnly, "profile-1", REV)).toBeNull();
    expect(findRollbackTarget([], "profile-1", REV)).toBeNull();
    expect(findRollbackTarget(null, "profile-1", REV)).toBeNull();
  });

  it("skips the live revision, rejected attempts, and non-publish rows", () => {
    const history = [
      // A superseded id pointing at the live revision and a refused publish:
      // neither names a previously published revision worth rolling back to.
      action({ superseded_profile_id: "profile-2" }),
      action({ action: "rejected", result: "rejected", superseded_profile_id: "profile-1" }),
      action({ action: "archive", profile_id: "profile-1" }),
      // The older publish names the real target.
      action({ profile_id: "profile-1", superseded_profile_id: "profile-0" }),
    ];
    expect(findRollbackTarget(history, "profile-2", new Map([["profile-0", 0]]))).toEqual({
      profileId: "profile-0",
      revisionNum: 0,
    });
  });

  it("reports a null revision number when the profile list cannot resolve the target", () => {
    const history = [action({ superseded_profile_id: "profile-0" })];
    expect(findRollbackTarget(history, "profile-2", new Map())).toEqual({
      profileId: "profile-0",
      revisionNum: null,
    });
  });
});

describe("rollbackButtonLabel", () => {
  it("names the target's revision number", () => {
    expect(rollbackButtonLabel({ profileId: "profile-1", revisionNum: 1 })).toBe(
      "Roll back to revision 1",
    );
  });

  it("never invents a number it cannot resolve", () => {
    expect(rollbackButtonLabel({ profileId: "profile-0", revisionNum: null })).toBe(
      "Roll back to the previous published revision",
    );
  });
});
