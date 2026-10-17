/**
 * `src/lib/profileDiff.test.ts` — synthetic-stub unit tests for the
 * revision diff (docs/spec/12 (a): hand-built fixture rows, no server).
 *
 * The fixture shape is what matters here, not the ids: a derive mints fresh
 * row ids for every line (internal/store `NewRevision`), so the rows in the
 * "draft" side below carry different `id` values than the "published" side
 * for the same source line — exactly the case a store-id diff gets wrong.
 */
import { describe, expect, it } from "vitest";

import type { Character, Message } from "../api/client";
import { diffRevisions, diffSummary, messageKey, type RevisionContent } from "./profileDiff";

function message(overrides: Partial<Message> = {}): Message {
  return {
    id: "01MSGID0000000000000000000",
    profile_id: "01PROFILE000000000000000000",
    original_text: "The harbour bell rang twice.",
    markup: [],
    normalized_text: "the harbour bell rang twice.",
    speech_text: "The harbour bell rang twice.",
    language: "en",
    text_kind: "dialogue",
    attribution_status: "unknown",
    attribution_confidence: 0,
    attribution_evidence: [],
    flags: {},
    revision: 1,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

function character(overrides: Partial<Character> = {}): Character {
  return {
    id: "01CHRID0000000000000000000",
    profile_id: "01PROFILE000000000000000000",
    name: "Marina",
    aliases: ["the keeper's daughter"],
    visual_descriptors: [],
    personality_summary: "Quiet, watchful.",
    role: "protagonist",
    verified_source_refs: ["synthetic:harbour-log"],
    status: "confirmed",
    created_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

/** A "derived" copy of the same content: new row ids, same source ids. */
function derive(rows: Message[]): Message[] {
  return rows.map((row, index) => ({ ...row, id: `01DERIVED00000000000${index}` }));
}

const PUBLISHED: RevisionContent = {
  profileId: "r1",
  revisionNum: 1,
  messages: [
    message({ id: "r1-m1", source_message_id: "demo-page-1" }),
    message({
      id: "r1-m2",
      source_message_id: "demo-page-2",
      original_text: "Nobody in the village had seen that boat before.",
      normalized_text: "nobody in the village had seen that boat before.",
      speech_text: "Nobody in the village had seen that boat before.",
    }),
    message({
      id: "r1-m3",
      source_message_id: "demo-x01",
      original_text: "The harbour bell rang twice before the fog lifted.",
      normalized_text: "the harbour bell rang twice before the fog lifted.",
      speech_text: "The harbour bell rang twice before the fog lifted.",
    }),
  ],
  characters: [character(), character({ id: "r1-c2", name: "Keeper", status: "proposed" })],
};

describe("messageKey", () => {
  it("prefers the source reconciliation key over the text", () => {
    const row = message({ source_message_id: "demo-page-1" });
    expect(messageKey(row)).toBe("sid:demo-page-1");
    expect(messageKey(row)).not.toContain("text:");
  });

  it("falls back to the text for lines with no source row", () => {
    const row = message({ source_message_id: undefined });
    expect(messageKey(row)).toBe("text:the harbour bell rang twice.");
  });
});

describe("diffRevisions (synthetic stubs)", () => {
  it("reports zero changes for a pristine derive (fresh row ids, same content)", () => {
    const draft: RevisionContent = {
      profileId: "r2",
      revisionNum: 2,
      messages: derive(PUBLISHED.messages),
      characters: PUBLISHED.characters.map((c, i) => ({ ...c, id: `01CHRIDERIVED000${i}` })),
    };
    const diff = diffRevisions(PUBLISHED, draft);
    expect(diff.messages.added).toHaveLength(0);
    expect(diff.messages.removed).toHaveLength(0);
    expect(diff.messages.changed).toHaveLength(0);
    expect(diff.messages.unchanged).toBe(3);
    expect(diff.characters.added).toHaveLength(0);
    expect(diff.characters.removed).toHaveLength(0);
    expect(diff.characters.changed).toHaveLength(0);
    expect(diffSummary(diff)).toContain("0 changed");
  });

  it("counts exactly one changed message when one speech text is edited", () => {
    const draft: RevisionContent = {
      profileId: "r2",
      revisionNum: 2,
      messages: derive(PUBLISHED.messages).map((row) =>
        row.source_message_id === "demo-page-1"
          ? { ...row, speech_text: "The harbour bell rang twice, and no one looked up." }
          : row,
      ),
      characters: PUBLISHED.characters,
    };
    const diff = diffRevisions(PUBLISHED, draft);
    expect(diff.messages.changed).toHaveLength(1);
    expect(diff.messages.changed[0].changedFields).toEqual(["speech_text"]);
    expect(diff.messages.added).toHaveLength(0);
    expect(diff.messages.removed).toHaveLength(0);
    expect(diff.messages.unchanged).toBe(2);
  });

  it("ignores the speaker row's re-minted id when the line still belongs to the same name", () => {
    // A derive mints the character rows too, so the line's speaker_id points at
    // a different ULID on each side even though the line still belongs to
    // "Marina". The diff must not report that as a change.
    const published: RevisionContent = {
      profileId: "r1",
      revisionNum: 1,
      messages: [
        message({ id: "r1-m1", source_message_id: "demo-page-1", speaker_id: "r1-marina" }),
      ],
      characters: [character({ id: "r1-marina", name: "Marina" })],
    };
    const draft: RevisionContent = {
      profileId: "r2",
      revisionNum: 2,
      messages: [
        message({ id: "r2-m1", source_message_id: "demo-page-1", speaker_id: "r2-marina" }),
      ],
      characters: [character({ id: "r2-marina", name: "Marina" })],
    };
    const diff = diffRevisions(published, draft);
    expect(diff.messages.unchanged).toBe(1);
    expect(diff.messages.changed).toHaveLength(0);
  });

  it("reports a speaker change when the line moves to a different character name", () => {
    const published: RevisionContent = {
      profileId: "r1",
      revisionNum: 1,
      messages: [
        message({ id: "r1-m1", source_message_id: "demo-page-1", speaker_id: "r1-marina" }),
      ],
      characters: [character({ id: "r1-marina", name: "Marina" })],
    };
    const draft: RevisionContent = {
      profileId: "r2",
      revisionNum: 2,
      messages: [
        message({ id: "r2-m1", source_message_id: "demo-page-1", speaker_id: "r2-keeper" }),
      ],
      characters: [character({ id: "r2-keeper", name: "Keeper", status: "proposed" })],
    };
    const diff = diffRevisions(published, draft);
    expect(diff.messages.changed).toHaveLength(1);
    expect(diff.messages.changed[0].changedFields).toEqual(["speaker_id"]);
  });

  it("reports a speaker change when the roster can no longer resolve the line's speaker", () => {
    const published: RevisionContent = {
      profileId: "r1",
      revisionNum: 1,
      messages: [
        message({ id: "r1-m1", source_message_id: "demo-page-1", speaker_id: "r1-marina" }),
      ],
      characters: [character({ id: "r1-marina", name: "Marina" })],
    };
    // The draft dropped Marina; the line's speaker row now dangles. A dangling
    // reference must not silently match a line that has no speaker at all.
    const draft: RevisionContent = {
      profileId: "r2",
      revisionNum: 2,
      messages: [message({ id: "r2-m1", source_message_id: "demo-page-1" })],
      characters: [],
    };
    const diff = diffRevisions(published, draft);
    expect(diff.messages.changed).toHaveLength(1);
    expect(diff.messages.changed[0].changedFields).toEqual(["speaker_id"]);
  });

  it("sees added and removed lines in the curator's direction", () => {
    const draft: RevisionContent = {
      profileId: "r2",
      revisionNum: 2,
      messages: [
        ...derive(PUBLISHED.messages.slice(0, 1)), // keep page-1, drop the other two
        message({
          id: "r2-m9",
          source_message_id: "demo-new",
          original_text: "A brand new line.",
          normalized_text: "a brand new line.",
          speech_text: "A brand new line.",
        }),
      ],
      characters: PUBLISHED.characters,
    };
    const diff = diffRevisions(PUBLISHED, draft);
    expect(diff.messages.added.map((m) => m.source_message_id)).toEqual(["demo-new"]);
    expect(diff.messages.removed.map((m) => m.source_message_id)).toEqual([
      "demo-page-2",
      "demo-x01",
    ]);
    expect(diff.messages.unchanged).toBe(1);
  });

  it("matches unkeyed lines on their text across a derive", () => {
    const published: RevisionContent = {
      ...PUBLISHED,
      messages: [message({ source_message_id: undefined })],
    };
    const draft: RevisionContent = {
      ...published,
      profileId: "r2",
      revisionNum: 2,
      messages: derive(published.messages),
    };
    const diff = diffRevisions(published, draft);
    expect(diff.messages.unchanged).toBe(1);
    expect(diff.messages.added).toHaveLength(0);
    expect(diff.messages.removed).toHaveLength(0);
  });

  it("diffs the roster by name and names the changed fields", () => {
    const draft: RevisionContent = {
      ...PUBLISHED,
      profileId: "r2",
      revisionNum: 2,
      characters: [
        { ...PUBLISHED.characters[0], status: "retired", aliases: ["M."] },
        { ...PUBLISHED.characters[1], id: "r2-c2" },
        character({ id: "r2-c3", name: "The Boatman", status: "proposed" }),
      ],
    };
    const diff = diffRevisions(PUBLISHED, draft);
    expect(diff.characters.added.map((c) => c.name)).toEqual(["The Boatman"]);
    expect(diff.characters.removed).toHaveLength(0);
    expect(diff.characters.changed).toHaveLength(1);
    expect(diff.characters.changed[0].changedFields.sort()).toEqual(["aliases", "status"]);
    expect(diff.characters.unchanged).toBe(1);
  });
});
