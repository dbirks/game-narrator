/**
 * `e2e/support/manifest.ts` — builds a schema-valid `.gnprofile.json`
 * (schema 1.0) manifest out of the synthetic profile seed
 * (`testdata/synthetic/profiles/dialogue-advance-001.json`) and computes its
 * `manifest_digest` the way the server recomputes it
 * (`internal/profileio/canonical.go`, mirrored line for line).
 *
 * Why this exists: the seed file is the games tables' columns in JSON — a
 * fixture format, not a manifest. The only API path that can create the
 * region, the messages and the edge in one transaction is
 * `POST /api/v1/editions/{id}/profiles/import-manifest`, and that route
 * refuses any document whose digest does not re-verify
 * ("the manifest does not hash to its own manifest_digest"). So the spec
 * assembles the manifest around the seed's content and signs it with the same
 * canonical projection the importer applies:
 *
 *   - object keys in byte order, no whitespace, no HTML escaping;
 *   - `manifest_digest`, `profile_id` and `publication` dropped;
 *   - every row id replaced by a content-derived positional alias
 *     (`src#0`, `msg#3`, …) assigned in dependency order;
 *   - sections ordered by their own canonical bytes; message pages by ordinal;
 *   - source rows' `uri` / `retrieved_at` dropped (they name this store's copy).
 *
 * The document built here is also the exact document the spec POSTs: the Go
 * side decodes it into the Manifest struct, normalises empty JSON columns
 * (which this document already fills: `[]` / `{}`) and re-marshals it, so the
 * digest computed here re-verifies on the server side.
 */

import { createHash, randomBytes } from "node:crypto";

/** The synthetic profile seed format (internal/scenario/profile.go), extended
 *  for the m5-part1 walk with a roster and per-line speaker/attribution facts:
 *  the extension fields are optional, so the original seed files keep working. */
export interface SeedCharacter {
  id: string;
  name: string;
  aliases?: string[];
  role?: string;
  summary?: string;
  status?: string;
  verified_source_refs?: string[];
}

export interface SeedProfile {
  id: string;
  origin: string;
  description: string;
  text_kind: string;
  messages: {
    id: string;
    original_text: string;
    scene_id?: string;
    /** A seed character id; names the line's assigned speaker. */
    speaker?: string;
    attribution?: { status: string; confidence: number };
  }[];
  edges: { from: string; to: string; type: string; confidence: number }[];
  regions: { id: string; type: string; x: number; y: number; w: number; h: number }[];
  characters?: SeedCharacter[];
}

/** One message row of the manifest, in the exporter's full spelling. */
interface ManifestMessage {
  id: string;
  source: null;
  source_message_id: string | null;
  kind: string;
  language: string;
  original_text: string | null;
  normalized_text: string | null;
  speech_text: string | null;
  speaker_id: string | null;
  scene_id: null;
  attribution: { status: string; confidence: number };
  markup: unknown[];
  evidence: unknown[];
  flags: Record<string, unknown>;
  redacted_reason: null;
  pages: unknown[];
}

/** One roster row of the manifest, in the exporter's full spelling
 *  (internal/profileio `CharacterInfo`). */
interface ManifestCharacter {
  id: string;
  name: string;
  aliases: string[];
  role: string;
  summary: string;
  visual: unknown[];
  status: string;
  verified_source_refs: string[];
  portrait: null;
  voice_profile_id: null;
}

/** The full manifest document: every field present, as the Go struct renders
 *  it after normalization (that exact shape is what the digest hashes). */
export interface ManifestDocument {
  schema_version: "1.0";
  profile_id: string;
  edition: {
    title: string;
    slug: string;
    platform: string;
    description: string;
    tags: unknown[];
    region: string;
    language: string;
    revision: string;
    locale: string;
    notes: string;
    aspect: number | null;
    identity: string | null;
  };
  publication: {
    status: "draft";
    revision: number;
    parent_revision: null;
    approved_at: null;
    author: string;
    preferred_voice_id: null;
    store_digest: string;
  };
  sources: unknown[];
  characters: unknown[];
  scenes: unknown[];
  regions: unknown[];
  messages: ManifestMessage[];
  message_edges: unknown[];
  speaker_assertions: unknown[];
  manifest_digest: string;
}

export interface ManifestOptions {
  /** The target edition the import lands on (region/language must match). */
  edition: { slug: string; title: string; region: string; language: string; revision: string };
  /** Which engine the region's OCR settings name. */
  ocrEngine?: string;
}

/** Crockford base32, the alphabet ids are spelled in (internal/games/ids.go). */
const CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

/** A fresh, well-formed ULID (internal/games/ids.go ValidID: 26 Crockford
 *  characters, first character's value <= 7 — true of any 48-bit millisecond
 *  timestamp before ~year 2242, so the time field yields a valid id). */
export function newUlid(): string {
  const buf = new Uint8Array(16);
  let time = BigInt(Date.now());
  for (let i = 5; i >= 0; i--) {
    buf[i] = Number(time & 0xffn);
    time >>= 8n;
  }
  const rand = randomBytes(10);
  for (let i = 0; i < 10; i++) buf[6 + i] = rand[i];
  let value = 0n;
  for (const byte of buf) value = (value << 8n) | BigInt(byte);
  let out = "";
  for (let i = 25; i >= 0; i--) out += CROCKFORD[Number((value >> BigInt(i * 5)) & 31n)];
  return out;
}

/** The ids a manifest import mints, so the caller can name the rows it just
 *  created (the friendly seed ids travel as source_message_id, not row ids). */
export interface ManifestIds {
  profileId: string;
  /** friendly seed message id -> the ULID row id the import stores. */
  messages: Record<string, string>;
}

/** Assemble the signed manifest document for the seed's content, plus the id
 *  mapping the import produces. */
export function buildManifest(
  seed: SeedProfile,
  options: ManifestOptions,
): { doc: ManifestDocument; ids: ManifestIds } {
  const profileId = newUlid();
  const regionIds: Record<string, string> = {};
  for (const region of seed.regions) regionIds[region.id] = newUlid();
  const messageIds: Record<string, string> = {};
  for (const message of seed.messages) messageIds[message.id] = newUlid();
  const characterIds: Record<string, string> = {};
  for (const character of seed.characters ?? []) characterIds[character.id] = newUlid();
  const characters: ManifestCharacter[] = (seed.characters ?? []).map((character) => ({
    id: characterIds[character.id],
    name: character.name,
    aliases: character.aliases ?? [],
    role: character.role ?? "",
    summary: character.summary ?? "",
    visual: [],
    status: character.status ?? "",
    verified_source_refs: character.verified_source_refs ?? [],
    portrait: null,
    voice_profile_id: null,
  }));
  const messages: ManifestMessage[] = seed.messages.map((message) => ({
    id: messageIds[message.id],
    source: null,
    source_message_id: message.id,
    kind: seed.text_kind,
    language: options.edition.language,
    original_text: message.original_text,
    normalized_text: message.original_text.toLowerCase(),
    speech_text: message.original_text,
    // A speaker names a character row of this same document; the importer
    // remaps the reference to its freshly minted row id (internal/profileio).
    speaker_id: message.speaker ? characterIds[message.speaker] : null,
    scene_id: null,
    attribution: message.attribution ?? { status: "unknown", confidence: 0 },
    markup: [],
    evidence: [],
    flags: {},
    redacted_reason: null,
    pages: [],
  }));
  const regions = seed.regions.map((region) => ({
    id: regionIds[region.id],
    label: "textbox",
    kind: region.type,
    detect: "manual",
    scene: null,
    rect: { x: region.x, y: region.y, w: region.w, h: region.h },
    ocr: { engine: options.ocrEngine ?? "tesseract", language: "eng", psm: 6 },
    mask_assets: [],
    reliability: null,
    min_aspect: null,
    max_aspect: null,
    min_width_px: null,
    min_height_px: null,
  }));
  const edges = seed.edges.map((edge) => ({
    from: messageIds[edge.from],
    to: messageIds[edge.to],
    type: edge.type,
    condition: "",
    source: null,
    confidence: edge.confidence,
    evidence: [],
  }));

  const doc: ManifestDocument = {
    schema_version: "1.0",
    profile_id: profileId,
    edition: {
      title: options.edition.title,
      slug: options.edition.slug,
      platform: "GameCube",
      description: seed.description,
      tags: [],
      region: options.edition.region,
      language: options.edition.language,
      revision: options.edition.revision,
      locale: "",
      notes: "",
      aspect: null,
      identity: null,
    },
    publication: {
      status: "draft",
      revision: 1,
      parent_revision: null,
      approved_at: null,
      author: "",
      preferred_voice_id: null,
      store_digest: "",
    },
    sources: [],
    characters,
    scenes: [],
    regions,
    messages,
    message_edges: edges,
    speaker_assertions: [],
    manifest_digest: "",
  };
  doc.manifest_digest = manifestDigest(doc);
  return { doc, ids: { profileId, messages: messageIds } };
}

// ── the digest (internal/profileio/canonical.go, mirrored) ──────────────────

/** The id-bearing sections in dependency order (aliasSections). */
const ALIAS_SECTIONS: { name: string; idKey: string; prefix: string }[] = [
  { name: "sources", idKey: "source_id", prefix: "src" },
  { name: "characters", idKey: "id", prefix: "chr" },
  { name: "scenes", idKey: "id", prefix: "scn" },
  { name: "regions", idKey: "id", prefix: "reg" },
  { name: "messages", idKey: "id", prefix: "msg" },
  { name: "speaker_assertions", idKey: "id", prefix: "asr" },
];

/** Every array section, in canonical (sorted) order (sectionNames). */
const SECTION_NAMES = [
  "characters",
  "message_edges",
  "messages",
  "regions",
  "scenes",
  "speaker_assertions",
  "sources",
] as const;

type Any = unknown;

/** The manifest's content fingerprint: sha256 of the canonical projection. */
export function manifestDigest(doc: ManifestDocument): string {
  const norm = roundTripManifest(doc);
  const known = knownIDs(norm);
  const aliases = refineAliases(norm, known);

  // Page order is canonicalized before anything reads a message's bytes.
  for (const message of norm.messages) {
    orderPages(message.pages, known, aliases);
  }
  const sections: Record<string, Any> = {};
  for (const name of SECTION_NAMES) {
    const rows = sectionOf(norm, name) ?? [];
    sections[name] = orderRows(rows, known, aliases);
  }

  const projection: Record<string, Any> = { schema_version: norm.schema_version };
  if (norm.edition !== undefined) projection.edition = norm.edition;
  for (const name of SECTION_NAMES) projection[name] = sections[name];

  const projected = substitute(projection, known, aliases) as Record<string, Any>;
  for (const row of rowsOf(projected.sources)) {
    delete row.uri;
    delete row.retrieved_at;
  }
  const bytes = canonical(projected);
  return createHash("sha256").update(bytes).digest("hex");
}

/**
 * Apply the server's decode-then-normalize to a manifest document: Go decodes
 * the JSON into the Manifest struct (missing fields become zero values; the
 * handwritten fixture omits some of them) and then fills empty JSON columns
 * with their table defaults (`[]` / `{}`) and a nil `pages` slice with `[]`
 * (internal/profileio/manifest.go `normalized()`). The digest hashes the
 * round-tripped shape, so the client must reproduce it exactly.
 */
export function roundTripManifest(doc: ManifestDocument): ManifestDocument {
  const out = structuredClone(doc) as ManifestDocument;
  const e = out.edition;
  e.title ??= "";
  e.slug ??= "";
  e.platform ??= "";
  e.description ??= "";
  e.tags = defaultArray(e.tags);
  e.region ??= "";
  e.language ??= "";
  e.revision ??= "";
  e.locale ??= "";
  e.notes ??= "";
  if (e.aspect === undefined) e.aspect = null;
  if (e.identity === undefined) e.identity = null;

  const p = out.publication;
  p.status ??= "draft";
  p.revision ??= 0;
  if (p.parent_revision === undefined) p.parent_revision = null;
  if (p.approved_at === undefined) p.approved_at = null;
  p.author ??= "";
  if (p.preferred_voice_id === undefined) p.preferred_voice_id = null;
  p.store_digest ??= "";

  for (const name of SECTION_NAMES) {
    const rows = sectionOf(out, name);
    (out as unknown as Record<string, Any>)[name] = Array.isArray(rows) ? rows : [];
  }
  for (const row of (out.sources ?? []) as Record<string, Any>[]) {
    row.source_id ??= "";
    row.type ??= "";
    row.sha256 ??= "";
    row.rights ??= "";
    row.parser ??= "";
    row.uri ??= "";
    row.mime_type ??= "";
    row.trust ??= 0;
    row.coverage ??= "";
    row.retrieved_at ??= "";
  }
  for (const row of (out.characters ?? []) as Record<string, Any>[]) {
    row.id ??= "";
    row.name ??= "";
    row.aliases = defaultArray(row.aliases);
    row.role ??= "";
    row.summary ??= "";
    row.visual = defaultArray(row.visual);
    row.status ??= "";
    row.verified_source_refs = defaultArray(row.verified_source_refs);
    if (row.portrait === undefined) row.portrait = null;
    if (row.voice_profile_id === undefined) row.voice_profile_id = null;
  }
  for (const row of (out.scenes ?? []) as Record<string, Any>[]) {
    row.id ??= "";
    row.name ??= "";
    row.aliases = defaultArray(row.aliases);
  }
  for (const row of (out.regions ?? []) as Record<string, Any>[]) {
    row.id ??= "";
    row.label ??= "";
    row.kind ??= "";
    row.detect ??= "";
    if (row.scene === undefined) row.scene = null;
    const rect = (row.rect ?? {}) as Record<string, number>;
    rect.x ??= 0;
    rect.y ??= 0;
    rect.w ??= 0;
    rect.h ??= 0;
    row.rect = rect;
    row.ocr = defaultObject(row.ocr);
    row.mask_assets = defaultArray(row.mask_assets);
    if (row.reliability === undefined) row.reliability = null;
    if (row.min_aspect === undefined) row.min_aspect = null;
    if (row.max_aspect === undefined) row.max_aspect = null;
    if (row.min_width_px === undefined) row.min_width_px = null;
    if (row.min_height_px === undefined) row.min_height_px = null;
  }
  for (const row of out.messages) {
    row.id ??= "";
    if (row.source === undefined) row.source = null;
    if (row.source_message_id === undefined) row.source_message_id = null;
    row.kind ??= "";
    row.language ??= "";
    if (row.original_text === undefined) row.original_text = null;
    if (row.normalized_text === undefined) row.normalized_text = null;
    if (row.speech_text === undefined) row.speech_text = null;
    if (row.speaker_id === undefined) row.speaker_id = null;
    if (row.scene_id === undefined) row.scene_id = null;
    row.attribution ??= { status: "", confidence: 0 };
    row.attribution.status ??= "";
    row.attribution.confidence ??= 0;
    row.markup = defaultArray(row.markup);
    row.evidence = defaultArray(row.evidence);
    row.flags = defaultObject(row.flags);
    if (row.redacted_reason === undefined) row.redacted_reason = null;
    row.pages = Array.isArray(row.pages) ? row.pages : [];
    for (const page of row.pages as Record<string, Any>[]) {
      page.index ??= 0;
      if (page.text === undefined) page.text = null;
      if (page.source_page_id === undefined) page.source_page_id = null;
      page.prefixes = defaultArray(page.prefixes);
      page.controls = defaultArray(page.controls);
      page.timing = defaultObject(page.timing);
    }
  }
  for (const row of (out.message_edges ?? []) as Record<string, Any>[]) {
    row.from ??= "";
    row.to ??= "";
    row.type ??= "";
    row.condition ??= "";
    if (row.source === undefined) row.source = null;
    row.confidence ??= 0;
    row.evidence = defaultArray(row.evidence);
  }
  for (const row of (out.speaker_assertions ?? []) as Record<string, Any>[]) {
    row.id ??= "";
    row.message ??= "";
    row.character ??= "";
    row.method ??= "";
    row.posterior ??= 0;
    if (row.reviewer === undefined) row.reviewer = null;
    row.evidence = defaultArray(row.evidence);
  }
  return out;
}

/** A JSON column that arrived absent, null or empty takes the table default. */
function defaultArray(value: Any): unknown[] {
  return Array.isArray(value) ? value : [];
}

function defaultObject(value: Any): Record<string, Any> {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, Any>)
    : {};
}

function knownIDs(doc: ManifestDocument): Map<string, string> {
  const known = new Map<string, string>();
  for (const section of ALIAS_SECTIONS) {
    for (const row of rowsOf(sectionOf(doc, section.name))) {
      const id = row[section.idKey];
      if (typeof id === "string" && id !== "") known.set(id, section.prefix);
    }
  }
  return known;
}

/** Content-derived aliases, assigned section by section in dependency order.
 *  A row's own id is not aliased yet when its key is computed, so it
 *  canonicalizes to @unresolved and cannot influence its own alias. */
function refineAliases(doc: ManifestDocument, known: Map<string, string>): Map<string, string> {
  const aliases = new Map<string, string>();
  for (const section of ALIAS_SECTIONS) {
    const items = rowsOf(sectionOf(doc, section.name)).map((row) => ({
      key: canonical(substitute(row, known, aliases)),
      row,
    }));
    items.sort((a, b) => byteCompare(a.key, b.key));
    items.forEach((item, index) => {
      const id = item.row[section.idKey];
      if (typeof id === "string" && id !== "") aliases.set(id, `${section.prefix}#${index}`);
    });
  }
  return aliases;
}

/** Rows sorted by their aliased canonical bytes (stable for ties). */
function orderRows(value: Any, known: Map<string, string>, aliases: Map<string, string>): Any {
  if (!Array.isArray(value)) return value;
  return [...value].sort((a, b) =>
    byteCompare(canonical(substitute(a, known, aliases)), canonical(substitute(b, known, aliases))),
  );
}

/** Pages re-sorted by their ordinal; ties by the rows' aliased canonical bytes. */
function orderPages(pages: Any, known: Map<string, string>, aliases: Map<string, string>): void {
  if (!Array.isArray(pages) || pages.length < 2) return;
  const items = pages.map((page) => {
    const row = page as Record<string, Any>;
    const ordinal = typeof row.index === "number" ? row.index : Number.NaN;
    return { ordinal, key: canonical(substitute(page, known, aliases)), value: page };
  });
  items.sort((a, b) => {
    if (a.ordinal !== b.ordinal) return a.ordinal < b.ordinal ? -1 : 1;
    return byteCompare(a.key, b.key);
  });
  pages.splice(0, pages.length, ...items.map((item) => item.value));
}

/** Deep copy with every document row id replaced by its alias (or
 *  @unresolved while the alias pass has not reached the section yet). */
function substitute(v: Any, known: Map<string, string>, aliases: Map<string, string>): Any {
  if (Array.isArray(v)) return v.map((item) => substitute(item, known, aliases));
  if (v !== null && typeof v === "object") {
    const out: Record<string, Any> = {};
    for (const [key, value] of Object.entries(v)) out[key] = substitute(value, known, aliases);
    return out;
  }
  if (typeof v === "string" && known.has(v)) {
    return aliases.get(v) ?? "@unresolved";
  }
  return v;
}

function rowsOf(value: Any): Record<string, Any>[] {
  if (!Array.isArray(value)) return [];
  return value.filter((row) => row !== null && typeof row === "object") as Record<string, Any>[];
}

// ── canonical JSON emission ─────────────────────────────────────────────────

/** Canonical JSON: keys in byte order, array order preserved, no whitespace,
 *  no HTML escaping, numbers exactly as the Go struct would write them. */
export function canonical(v: Any): string {
  const parts: string[] = [];
  writeCanonical(parts, v);
  return parts.join("");
}

function writeCanonical(parts: string[], v: Any): void {
  if (v === null) {
    parts.push("null");
    return;
  }
  switch (typeof v) {
    case "boolean":
      parts.push(v ? "true" : "false");
      return;
    case "string":
      parts.push(escapeGo(v));
      return;
    case "number":
      parts.push(goNumber(v));
      return;
    case "object":
      break;
    default:
      parts.push("null");
      return;
  }
  if (Array.isArray(v)) {
    parts.push("[");
    v.forEach((item, index) => {
      if (index > 0) parts.push(",");
      writeCanonical(parts, item);
    });
    parts.push("]");
    return;
  }
  const keys = Object.keys(v as Record<string, Any>).sort(byteCompare);
  parts.push("{");
  keys.forEach((key, index) => {
    if (index > 0) parts.push(",");
    parts.push(escapeGo(key), ":");
    writeCanonical(parts, (v as Record<string, Any>)[key]);
  });
  parts.push("}");
}

/** One section of the document, typed loosely: the digest walks sections by
 *  name, and the ManifestDocument type spells them out one by one. */
function sectionOf(doc: ManifestDocument, name: string): Any {
  return (doc as unknown as Record<string, Any>)[name];
}

/** escapeGo: encoding/json with HTML escaping turned off. */
export function escapeGo(s: string): string {
  let out = '"';
  for (let i = 0; i < s.length; i += 1) {
    const unit = s.charCodeAt(i);
    if (unit >= 0x80 || (unit >= 0x20 && unit !== 0x22 && unit !== 0x5c)) {
      out += s[i];
      continue;
    }
    switch (unit) {
      case 0x5c: // backslash
        out += "\\\\";
        break;
      case 0x22: // quote
        out += '\\"';
        break;
      case 0x0a:
        out += "\\n";
        break;
      case 0x0d:
        out += "\\r";
        break;
      case 0x09:
        out += "\\t";
        break;
      default:
        out += `\\u00${unit.toString(16).padStart(2, "0")}`;
    }
  }
  return `${out}"`;
}

/** goNumber: the shortest decimal that round-trips, the way Go marshals a
 *  float64 or int (our values are far from the exponent-notation cutoffs). */
function goNumber(n: number): string {
  return String(n);
}

/** Byte (UTF-8) order, the order Go's sort.Strings uses. ASCII content is
 *  identical to code-unit order; encode for the non-ASCII case. */
function byteCompare(a: string, b: string): number {
  if (a === b) return 0;
  const aBytes = new TextEncoder().encode(a);
  const bBytes = new TextEncoder().encode(b);
  const length = Math.min(aBytes.length, bBytes.length);
  for (let i = 0; i < length; i += 1) {
    const x = aBytes[i] ?? 0;
    const y = bBytes[i] ?? 0;
    if (x !== y) return x - y;
  }
  return aBytes.length - bBytes.length;
}
