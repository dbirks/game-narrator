/**
 * `castShared.test.ts` — the Cast screen's count definition (ADR-0135),
 * unit-tested against the SAME fixture shape the Go unit test pins
 * (internal/store/status_counts_test.go): named voiced, named needs-a-voice,
 * unnamed placeholder with lines, retired WITH lines and WITH an approved
 * voice, an active character with no lines, and the edge rows (a character
 * whose only voice revision is retired, a voice for a name the roster does
 * not have). The client mirror and the store method must answer the same
 * pair on the same rows, so the Cast header chip cannot disagree with the
 * Library card or the status bar.
 */
import { describe, expect, it } from "vitest";

import type { Character, Message } from "../../api/client";
import type { VoiceRevision } from "../../api/voices";
import {
  castCounts,
  castFilterCount,
  castOrder,
  isUnnamedSpeaker,
  lineCountOf,
} from "./castShared";

const character = (
  id: string,
  name: string,
  status: Character["status"] = "confirmed",
): Character =>
  ({
    id,
    profile_id: "p1",
    name,
    aliases: [],
    visual_descriptors: [],
    personality_summary: "",
    role: "",
    verified_source_refs: [],
    status,
    created_at: "2026-11-21T00:00:00Z",
  }) as unknown as Character;

const message = (
  speakerId: string | null,
  attribution: Message["attribution_status"] = "confirmed",
  evidence: unknown[] = [],
): Message =>
  ({
    id: `m-${Math.random().toString(36).slice(2)}`,
    profile_id: "p1",
    source_message_id: null,
    kind: "dialogue",
    language: "en",
    original_text: "A line.",
    normalized_text: "a line.",
    speech_text: "A line.",
    speaker_id: speakerId,
    scene_id: null,
    attribution_status: attribution,
    attribution_confidence: attribution === "confirmed" ? 1 : 0.5,
    attribution_evidence: evidence,
    markup: [],
    flags: {},
    redacted_reason: null,
    created_at: "2026-11-21T00:00:00Z",
    pages: [],
  }) as unknown as Message;

const voice = (name: string, revision: number, state: VoiceRevision["state"]): VoiceRevision =>
  ({
    id: `v-${name}-${revision}`,
    character: name,
    revision,
    state,
    ref_transcript: "",
    fingerprint: "",
    consent_provenance: "",
    design_instruction: "",
    model_id: "fake",
    latency_ms: 0,
    gain: 1,
    created_at: "2026-11-21T00:00:00Z",
  }) as unknown as VoiceRevision;

const SOURCE_FIELD = [
  { method: "source_field", field: "speaker", key: "spk:1", source_id: "disc", posterior: 1.0 },
];

describe("castCounts (the shared voiced-count definition, ADR-0135)", () => {
  it("counts the active speaking characters only: retired out, 0-line out, the name join", () => {
    // named & voiced: confirmed, lines, an approved take (the board's ten).
    const aryll = character("c-aryll", "Aryll");
    // named, needs a voice: confirmed, lines, no take yet.
    const grandma = character("c-grandma", "Grandma");
    // unnamed placeholder: proposed, lines, an auditioning take.
    const sp1 = character("c-sp1", "spk:1", "proposed");
    const sp2 = character("c-sp2", "spk:2", "proposed");
    // retired WITH lines and WITH an approved voice: the row the old count
    // would have dragged into both numbers.
    const dead = character("c-dead", "Retired One", "retired");
    // active but silent: confirmed, no lines at all — not a speaker.
    const quiet = character("c-quiet", "Quiet");

    const roster = [aryll, grandma, sp1, sp2, dead, quiet];
    const messages = [
      message(aryll.id),
      message(aryll.id),
      message(grandma.id),
      message(sp1.id),
      message(sp1.id, "proposed", SOURCE_FIELD),
      message(sp2.id),
      message(sp2.id, "proposed", SOURCE_FIELD),
      message(dead.id),
    ];
    // The voices, keyed by NAME the way the voice surface does.
    const voices = [
      voice("Aryll", 1, "approved"),
      voice("Grandma", 1, "retired"),
      voice("Grandma", 2, "draft"),
      voice("spk:1", 1, "auditioning"),
      voice("Retired One", 1, "approved"),
      voice("Quiet", 1, "approved"),
      voice("Nobody", 1, "approved"),
    ];

    const { speakers, voiced } = castCounts(roster, messages, voices);
    // The denominator: Aryll, Grandma, spk:1, spk:2 — Retired One is retired
    // and Quiet has no lines.
    expect(speakers).toBe(4);
    // The numerator: Aryll (approved), Grandma (draft in flight), spk:1
    // (auditioning) — Retired One's take and Quiet's take do not count, and
    // "Nobody" has no row.
    expect(voiced).toBe(3);
  });

  it("reads a retired-only voice as unvoiced, and an undo as still voiced", () => {
    const boy = character("c-boy", "Boy");
    const girl = character("c-girl", "Girl");
    const roster = [boy, girl];
    const messages = [message(boy.id), message(girl.id)];
    // Boy: v1 approved, v2 retired — the one-step undo (ADR-0109). The lower
    // approved take still speaks, so Boy stays voiced.
    // Girl: her only take is retired — the row set says "no live take".
    const voices = [
      voice("Boy", 1, "approved"),
      voice("Boy", 2, "retired"),
      voice("Girl", 1, "retired"),
    ];
    expect(castCounts(roster, messages, voices)).toEqual({ speakers: 2, voiced: 1 });
  });
});

describe("the roster's presentation and filters (ADR-0135)", () => {
  const tessa = character("c-tessa", "Tessa");
  const oldBram = character("c-bram", "Old Bram");
  const spk1 = character("c-spk1", "spk:1", "proposed");
  const spk2 = character("c-spk2", "spk:2", "proposed");
  const dead = character("c-dead", "Retired One", "retired");
  const quiet = character("c-quiet", "Quiet");

  const sourceField = (n: number): Message[] =>
    Array.from({ length: n }, () => message(spk1.id, "proposed", SOURCE_FIELD));

  const messages = [
    ...Array.from({ length: 5 }, () => message(tessa.id)),
    ...Array.from({ length: 9 }, () => message(oldBram.id)),
    ...sourceField(136),
    message(spk2.id, "proposed", SOURCE_FIELD),
    ...Array.from({ length: 3 }, () => message(dead.id)),
  ];
  const roster = [dead, quiet, spk2, spk1, oldBram, tessa];

  it("orders named-voiced first, then named needs-a-voice, then unnamed, retired last", () => {
    const voices = [voice("Tessa", 1, "approved"), voice("spk:2", 1, "approved")];
    const order = castOrder(roster, messages, voices).map((c) => c.name);
    // Tessa (named, voiced) leads; Old Bram (named, no voice, 9 lines) next;
    // spk:2 (unnamed, voiced, 1 line) before spk:1 (unnamed, no voice);
    // Retired One last, and Quiet (0 lines) before the dead row only in the
    // server order within its own group — Quiet is named and unvoiced with 0
    // lines, so it sorts under Old Bram by line count.
    expect(order[0]).toBe("Tessa");
    expect(order).toContain("Old Bram");
    expect(order.indexOf("Old Bram")).toBeLessThan(order.indexOf("spk:2"));
    expect(order.indexOf("spk:2")).toBeLessThan(order.indexOf("spk:1"));
    expect(order[order.length - 1]).toBe("Retired One");
    // Within the named-needs group, line count descending.
    expect(order.indexOf("Old Bram")).toBeLessThan(order.indexOf("Quiet"));
  });

  it("isUnnamedSpeaker is the data-driven predicate: proposed + the source's own linkage", () => {
    expect(isUnnamedSpeaker(spk1, messages)).toBe(true);
    expect(isUnnamedSpeaker(spk2, messages)).toBe(true);
    // A proposed row WITHOUT the source_field evidence is not the import's
    // placeholder (a parser-minted row the curator can rename on the row).
    const parserProposed = character("c-x", "X", "proposed");
    const msgs = [message(parserProposed.id)];
    expect(isUnnamedSpeaker(parserProposed, msgs)).toBe(false);
    // A confirmed row with source_field evidence is a named character whose
    // lines the import attributed, not an unnamed speaker.
    const confirmed = character("c-y", "Y");
    expect(isUnnamedSpeaker(confirmed, [message(confirmed.id, "proposed", SOURCE_FIELD)])).toBe(
      false,
    );
  });

  it("the filter chips: All hides the retired rows, Retired holds only them, the counts add up", () => {
    const voices = [voice("Tessa", 1, "approved")];
    expect(castFilterCount(roster, "all", messages, voices)).toBe(5);
    expect(castFilterCount(roster, "retired", messages, voices)).toBe(1);
    expect(castFilterCount(roster, "voiced", messages, voices)).toBe(1);
    expect(castFilterCount(roster, "needs", messages, voices)).toBe(4);
    expect(castFilterCount(roster, "unnamed", messages, voices)).toBe(2);
    // The live chips partition the non-retired roster.
    expect(
      castFilterCount(roster, "voiced", messages, voices) +
        castFilterCount(roster, "needs", messages, voices),
    ).toBe(castFilterCount(roster, "all", messages, voices));
    // lineCountOf reads the row's own assigned lines.
    expect(lineCountOf(spk1, messages)).toBe(136);
    expect(lineCountOf(quiet, messages)).toBe(0);
  });
});
