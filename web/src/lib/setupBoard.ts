/**
 * `src/lib/setupBoard.ts` — the setup board's client state (ADR-0108, UX-06).
 *
 * One rule of the surface, client half: a card moves only when a
 * `setup.character_stage` SSE event says so. The snapshot
 * (GET /board) is the page-load half — a fresh load, or a roster change the
 * events do not record (events carry moves, not the initial board) — and the
 * two sources merge the way a replaying client would: the snapshot is the
 * baseline, and each character's newest stage event overwrites it (later rows
 * overwrite earlier ones; ADR-0108 "snapshot ≡ replay"). Nothing here polls;
 * the callers drive both sources from the one SSE stream.
 */

import {
  SETUP_STAGES,
  type SetupBoard,
  type SetupBoardCharacter,
  type SetupProgress,
  type SetupRailCell,
  type SetupStage,
} from "../api/onboarding";

/** One card as the board renders it (snapshot row or event row). */
export interface BoardCard {
  characterId: string;
  name: string;
  stage: SetupStage;
  lines: number;
}

/** The board's merge result: the cards the page renders, plus the rail and
 *  the three stat tiles' values. */
export interface SetupBoardView {
  cards: BoardCard[];
  progress: SetupProgress | null;
  rail: SetupRailCell[];
}

/** How many rows the board's card layer sizes (the stylesheet's position
 *  table); cards beyond the clamp share the last row rather than leaving the
 *  board's bounded area. */
export const MAX_CARD_ROWS = 6;
export const MAX_CARD_ROWS_PER_COLUMN = MAX_CARD_ROWS - 1; // 0..5

/**
 * Merge the snapshot baseline with the newest stage event per character.
 *
 * - snapshot characters are the baseline (derived stage, name order);
 * - an event for a known character overwrites its row (the event is newer
 *   by construction: the emitter only publishes forward moves);
 * - an event for a character the snapshot never held creates the card (the
 *   snapshot was read before the roster grew);
 * - a character with neither stays out of the board (the board shows the
 *   roster, not guesses).
 */
export function mergeBoard(
  snapshot: SetupBoard | null,
  eventsByCharacter: Map<string, { stage: SetupStage; lines: number; name: string }>,
): SetupBoardView {
  const cards = new Map<string, BoardCard>();
  if (snapshot !== null) {
    // The contract names characters a required array; the ?? [] is the
    // defense-in-depth against a server that marshals the empty roster as
    // null (a page that crashes on the empty board is the board's fault,
    // not the run's).
    for (const character of snapshot.characters ?? []) {
      const row: SetupBoardCharacter = character;
      cards.set(row.character_id, {
        characterId: row.character_id,
        name: row.name,
        stage: row.stage,
        lines: row.lines,
      });
    }
  }
  for (const [characterId, event] of eventsByCharacter) {
    cards.set(characterId, {
      characterId,
      name: event.name,
      stage: event.stage,
      lines: event.lines,
    });
  }
  // Stable (name, then id) order: the rows inside a column do not shuffle
  // while the board settles.
  const ordered = [...cards.values()].sort((a, b) =>
    a.name < b.name ? -1 : a.name > b.name ? 1 : a.characterId < b.characterId ? -1 : 1,
  );
  return {
    cards: ordered,
    progress: snapshot?.progress ?? null,
    rail: snapshot?.progress?.stage_rail ?? [],
  };
}

/** The column (board index) a card sits in. */
export function columnForStage(stage: SetupStage): number {
  return SETUP_STAGES.indexOf(stage);
}

/**
 * The card's row inside its column: the stable order index, clamped to the
 * stylesheet's position table. Two cards past the clamp share the last row —
 * a bounded board over a bounded roster, never an unbounded page.
 */
export function rowsForStage(cards: BoardCard[], stage: SetupStage): Map<string, number> {
  const inColumn = cards.filter((card) => card.stage === stage);
  const rows = new Map<string, number>();
  inColumn.forEach((card, index) => {
    rows.set(card.characterId, Math.min(index, MAX_CARD_ROWS_PER_COLUMN));
  });
  return rows;
}

/** Per-column counts for the mono column headers. */
export function countsByStage(cards: BoardCard[]): Record<SetupStage, number> {
  const counts: Record<SetupStage, number> = { finding: 0, brief: 0, voicing: 0, done: 0 };
  for (const card of cards) counts[card.stage] += 1;
  return counts;
}

/** The voiced tile's progress (0–100; a zero roster is 0, not NaN). */
export function voicedProgress(progress: SetupProgress | null): number {
  if (progress === null || progress.total <= 0) return 0;
  return Math.round((100 * progress.voiced) / progress.total);
}

/**
 * Run completion (ADR-0093 open question (a), provisional answer): the agent
 * finished its work — the run's durable state is terminal-success. A run in
 * review or running is mid-setup, whatever the cards show.
 */
export function isRunComplete(state: string | null | undefined): boolean {
  return state === "succeeded";
}
