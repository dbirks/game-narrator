import { readdirSync, readFileSync, statSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * WCAG 1.4.3 gate for token pairs, pure (no browser).
 *
 * The cast page's failure badge (coral #e2725b on cream paper, 2.68:1)
 * showed that "the default theme looked fine" is not a contrast strategy:
 * every theme must hold 4.5:1 on its own text pairs. This test parses the
 * theme blocks of styles/tokens.css, collects every token the CSS actually
 * uses as a TEXT colour (`color: var(--…)` across the sheets), resolves it
 * per theme, and asserts the contrast against each surface that token's
 * text is painted on:
 *
 * - default surfaces: --paper (the page) and --card (the card ground);
 * - `peach`: the active nav item of the 28px status bar, whose background
 *   is --ink (tokens.css `.status-bar`) — never paper/card;
 * - `on-ink-solid`: the kit primary-control fill pair, background
 *   --ink-solid (tokens.css legacy remap), never paper/card.
 *
 * Pairs are skipped when they cannot render:
 * - the token resolves to the same value as the surface (a token is the
 *   ground on that theme);
 * - the token resolves to the theme's --bg (it is the page background, not
 *   text);
 * - the surface resolves to the theme's --fg (that token is a foreground
 *   on that theme — under Lilac Night --paper is the text, so "coral on
 *   paper" is not a real pair there).
 *
 * Threshold: 4.5:1 for every current usage — all of them are small text
 * (the largest is 16px, and none reaches WCAG's large-text bar of
 * 18.66px / 14pt bold). When the CSS introduces large or bold-large text,
 * raise that pair's bar to 3:1 here with a comment, per the task.
 *
 * Regression this file exists to catch: a per-theme text token (e.g. the
 * badges' --danger-text) may exist while another colour rule in the same
 * theme still paints the low-contrast fill colour as text.
 */

const SRC = resolve(__dirname, "..");
const TOKENS = resolve(__dirname, "tokens.css");

/* ── CSS scanning (mirrors the helpers tokens.test.ts uses) ─────────────── */

function listCss(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir).sort()) {
    const path = resolve(dir, entry);
    if (entry.endsWith(".css")) out.push(path);
    else if (statSync(path).isDirectory()) listCss(path, out);
  }
  return out;
}

function themeBlocks(css: string): Array<{ name: string; body: string }> {
  const blocks: Array<{ name: string; body: string }> = [];
  // A selector list is `:root`, a `[data-theme="…"]` block, or the two
  // comma-joined (`:root, [data-theme="plum"]` — the default palette applies
  // to the bare-root default theme and the plum one alike).
  const re = /^((?::root|\[data-theme="[^"]+"\])(,\s*(?::root|\[data-theme="[^"]+"\]))*)\s*\{/gm;
  let match: RegExpExecArray | null;
  while ((match = re.exec(css)) !== null) {
    const names = new Set<string>();
    for (const sel of match[1].split(",")) {
      const m = sel.match(/data-theme="([^"]+)"/);
      names.add(m === null ? "default" : m[1]);
    }
    let depth = 1;
    let i = re.lastIndex;
    while (i < css.length && depth > 0) {
      if (css[i] === "{") depth++;
      if (css[i] === "}") depth--;
      i++;
    }
    const body = css.slice(re.lastIndex, i - 1);
    for (const name of names) blocks.push({ name, body });
  }
  return blocks;
}

/** Every `--name: value;` declaration inside a theme block body. */
function declarations(body: string): Map<string, string> {
  const map = new Map<string, string>();
  for (const m of body.matchAll(/^(\s*)--([a-z0-9-]+):\s*([^;]+);/gm)) {
    map.set(`--${m[2]}`, m[3].trim());
  }
  return map;
}

/* ── per-theme token maps (later blocks override earlier ones) ──────────── */

function themes(tokensCss: string): Map<string, Map<string, string>> {
  const out = new Map<string, Map<string, string>>();
  for (const { name, body } of themeBlocks(tokensCss)) {
    let map = out.get(name);
    if (map === undefined) {
      map = new Map();
      out.set(name, map);
    }
    for (const [k, v] of declarations(body)) map.set(k, v);
  }
  // A non-default theme inherits every :root value it does not override
  // (tokens.css stacks `:root, [data-theme="plum"]`, per-theme blocks, and
  // per-theme override blocks on top of the :root legacy remap).
  const root = out.get("default") ?? new Map();
  for (const [name, map] of out) {
    if (name === "default") continue;
    for (const [k, v] of root) if (!map.has(k)) map.set(k, v);
  }
  return out;
}

/** Resolve a token value to a hex colour, following var() chains. */
function resolveHex(map: Map<string, string>, token: string, depth = 0): string {
  const raw = map.get(token);
  if (raw === undefined) throw new Error(`token ${token} is not defined`);
  if (/^#[0-9a-f]{6}$/i.test(raw)) return raw.toLowerCase();
  const ref = raw.match(/^var\(([a-z0-9-]+)\)$/);
  if (ref !== null) {
    if (depth > 8) throw new Error(`var() loop at ${token}`);
    return resolveHex(map, ref[1], depth + 1);
  }
  throw new Error(`token ${token} is not a plain hex colour: ${raw}`);
}

/* ── WCAG relative luminance / contrast ratio ───────────────────────────── */

function luminance(hex: string): number {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255);
  const lin = (c: number) => (c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4));
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

function contrast(a: string, b: string): number {
  const la = luminance(a);
  const lb = luminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

/* ── which tokens does the CSS actually use as text colours? ────────────── */

function textTokens(): Map<string, string[]> {
  // token → ordered var() fallback chain (the badges use
  // `var(--danger-text, var(--danger))`); the first token defined on the
  // theme is the colour that renders.
  const used = new Map<string, string[]>();
  for (const file of listCss(SRC)) {
    const css = readFileSync(file, "utf8");
    for (const m of css.matchAll(/^\s*color:\s*var\(([^)]*)\)/gm)) {
      const chain = [...m[1].matchAll(/\s*(--[a-z0-9-]+)/g)].map((c) => c[1]);
      if (chain.length === 0) throw new Error(`unparseable var() in ${file}`);
      const first = chain[0];
      const known = used.get(first);
      if (known === undefined) used.set(first, chain);
      else if (known.length !== chain.length || known.some((t, i) => t !== chain[i])) {
        throw new Error(`token ${first} is declared with two different fallback chains`);
      }
    }
  }
  return used;
}

/**
 * The surfaces a text token's text is painted on. Every token defaults to
 * the page and the card; the two documented exceptions are tokens whose
 * rules never sit on paper/card (see the file header).
 */
const SURFACES: Record<string, string[]> = {
  "--peach": ["--ink"],
  "--on-ink-solid": ["--ink-solid"],
};

const THRESHOLD = 4.5;

/* ── the check ──────────────────────────────────────────────────────────── */

describe("WCAG 1.4.3 per-theme text token contrast", () => {
  it("covers every theme in tokens.css", () => {
    const names = [...themes(readFileSync(TOKENS, "utf8")).keys()].sort();
    expect(names).toEqual(["default", "ink-apricot", "lilac-night", "plum", "sherbet"]);
  });

  it("uses at least one text token per theme (the scan is not vacuous)", () => {
    expect(textTokens().size).toBeGreaterThanOrEqual(4);
  });

  it("holds 4.5:1 on every text-token pair the CSS actually uses", () => {
    const tokenMap = themes(readFileSync(TOKENS, "utf8"));
    const colours = textTokens();
    const failures: string[] = [];
    for (const [theme, map] of tokenMap) {
      const bg = resolveHex(map, "--bg");
      const fg = resolveHex(map, "--fg");
      for (const [token, chain] of [...colours].sort(([a], [b]) => a.localeCompare(b))) {
        const colour = chain.find((candidate) => map.has(candidate));
        if (colour === undefined) {
          failures.push(
            `${theme}: ${token} — no candidate of the chain [${chain.join(", ")}] is defined`,
          );
          continue;
        }
        const colourHex = resolveHex(map, colour);
        for (const surface of SURFACES[token] ?? ["--paper", "--card"]) {
          const ground = resolveHex(map, surface);
          if (colourHex === ground) continue; // the token is the ground there
          if (colourHex === bg) continue; // page background, not text
          if (ground === fg) continue; // foreground token, not a surface
          const ratio = contrast(colourHex, ground);
          if (ratio < THRESHOLD) {
            failures.push(
              `${theme}: ${token} (${colourHex}) on ${surface} (${ground}) = ` +
                `${ratio.toFixed(2)}:1 (< ${THRESHOLD}:1)`,
            );
          }
        }
      }
    }
    expect(failures).toEqual([]);
  });
});
