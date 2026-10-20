import { describe, expect, it } from "vitest";

import type { SetupBoard, SetupStage } from "../api/onboarding";
import {
  columnForStage,
  countsByStage,
  isRunComplete,
  mergeBoard,
  rowsForStage,
  voicedProgress,
} from "./setupBoard";

/** A minimal snapshot: the roster the import produced (all baseline finding). */
function snapshot(chars: [string, string][], linesRead = 4212): SetupBoard {
  return {
    run_id: "run-1",
    state: "running",
    created_at: "2026-10-20T00:00:00Z",
    characters: chars.map(([id, name], index) => ({
      character_id: id,
      name,
      stage: "finding",
      lines: 10 + index,
    })),
    progress: {
      run_id: "run-1",
      lines_read: linesRead,
      characters: chars.length,
      voiced: 0,
      total: chars.length,
      elapsed: 42,
      stage_rail: [],
    },
  };
}

describe("mergeBoard", () => {
  it("starts from the snapshot baseline", () => {
    const view = mergeBoard(
      snapshot([
        ["c1", "Marla"],
        ["c2", "Corvin"],
      ]),
      new Map(),
    );
    // The order is stable by NAME (the rows do not shuffle), not by snapshot
    // order: Corvin precedes Marla.
    expect(view.cards.map((card) => card.characterId)).toEqual(["c2", "c1"]);
    expect(view.cards.every((card) => card.stage === "finding")).toBe(true);
    expect(view.progress?.lines_read).toBe(4212);
  });

  it("an event overwrites the snapshot row for a known character", () => {
    const view = mergeBoard(
      snapshot([["c1", "Marla"]]),
      new Map([["c1", { stage: "brief", lines: 25, name: "Marla" }]]),
    );
    expect(view.cards[0].stage).toBe("brief");
    expect(view.cards[0].lines).toBe(25);
  });

  it("an event for a character the snapshot never held creates the card", () => {
    const view = mergeBoard(
      null,
      new Map([["c9", { stage: "voicing", lines: 3, name: "Tevance" }]]),
    );
    expect(view.cards).toEqual([
      { characterId: "c9", name: "Tevance", stage: "voicing", lines: 3 },
    ]);
  });

  it("a character with neither a snapshot row nor an event stays out of the board", () => {
    const view = mergeBoard(snapshot([["c1", "Marla"]]), new Map());
    expect(view.cards.map((card) => card.characterId)).toEqual(["c1"]);
  });

  it("keeps a stable name order so the rows do not shuffle", () => {
    const view = mergeBoard(
      snapshot([
        ["c2", "Zed"],
        ["c1", "Ada"],
      ]),
      new Map(),
    );
    expect(view.cards.map((card) => card.name)).toEqual(["Ada", "Zed"]);
  });

  it("tolerates a snapshot whose roster marshalled as null (the empty run)", () => {
    // A run with no characters yet: a server that marshals the empty roster
    // as null must not crash the merge — the board is empty, the run lives.
    const view = mergeBoard(
      { ...snapshot([]), characters: null as unknown as SetupBoard["characters"] },
      new Map(),
    );
    expect(view.cards).toEqual([]);
    expect(view.progress?.lines_read).toBe(4212);
  });
});

describe("board geometry", () => {
  const cards = [
    { characterId: "c1", name: "Ada", stage: "finding" as SetupStage, lines: 1 },
    { characterId: "c2", name: "Bo", stage: "finding" as SetupStage, lines: 2 },
    { characterId: "c3", name: "Cy", stage: "brief" as SetupStage, lines: 3 },
  ];

  it("maps stages to the board's four columns", () => {
    expect(columnForStage("finding")).toBe(0);
    expect(columnForStage("brief")).toBe(1);
    expect(columnForStage("voicing")).toBe(2);
    expect(columnForStage("done")).toBe(3);
  });

  it("assigns the stable row inside a column and clamps past the table", () => {
    const rows = rowsForStage(cards, "finding");
    expect(rows.get("c1")).toBe(0);
    expect(rows.get("c2")).toBe(1);
    const many = Array.from({ length: 9 }, (_, i) => ({
      characterId: `x${i}`,
      name: `N${i}`,
      stage: "finding" as SetupStage,
      lines: 1,
    }));
    const clamped = rowsForStage(many, "finding");
    for (const row of clamped.values()) {
      expect(row).toBeGreaterThanOrEqual(0);
      expect(row).toBeLessThanOrEqual(5);
    }
  });

  it("counts per column for the mono headers", () => {
    expect(countsByStage(cards)).toEqual({ finding: 2, brief: 1, voicing: 0, done: 0 });
  });
});

describe("tiles and completion", () => {
  it("computes the voiced tile's progress without a zero-roster fault", () => {
    expect(voicedProgress(null)).toBe(0);
    const base: SetupBoard["progress"] = {
      run_id: "r",
      lines_read: 10,
      characters: 4,
      voiced: 1,
      total: 4,
      elapsed: 0,
      stage_rail: [],
    };
    expect(voicedProgress(base)).toBe(25);
    expect(voicedProgress({ ...base, total: 0 })).toBe(0);
  });

  it("treats only terminal success as setup complete", () => {
    expect(isRunComplete("succeeded")).toBe(true);
    expect(isRunComplete("review")).toBe(false);
    expect(isRunComplete("running")).toBe(false);
    expect(isRunComplete(undefined)).toBe(false);
  });
});
