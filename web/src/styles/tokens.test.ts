import { readdirSync, readFileSync, statSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * The Phase 0 scripted audit (plan.md item 1): no raw hex anywhere in
 * web/src/** outside a theme block (a top-level `:root` or
 * `[data-theme="…"]` selector). The legacy index.css :root tokens are the
 * one remaining pre-redesign theme block; everything new lives in
 * tokens.css, which is where the four themes are defined and re-asserted
 * here against docs/design/plum-peach/README.md.
 */

const SRC = resolve(__dirname, "..");
const HEX = /#[0-9a-fA-F]{3,8}\b/g;

function listCss(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir).sort()) {
    const path = resolve(dir, entry);
    if (entry.endsWith(".css")) out.push(path);
    else if (statSync(path).isDirectory()) listCss(path, out);
  }
  return out;
}

function themeBlocks(css: string): Array<[string, string]> {
  const blocks: Array<[string, string]> = [];
  const re = /^(:root|\[data-theme="[^"]+"\]) \{/gm;
  let match: RegExpExecArray | null;
  while ((match = re.exec(css)) !== null) {
    const name = (match[0].match(/data-theme="([^"]+)"/) ?? [])[1] ?? "default";
    let depth = 1;
    let i = re.lastIndex;
    while (i < css.length && depth > 0) {
      if (css[i] === "{") depth++;
      if (css[i] === "}") depth--;
      i++;
    }
    blocks.push([name, css.slice(re.lastIndex, i - 1)]);
  }
  return blocks;
}

const cssFiles = listCss(SRC);

describe("token file audit", () => {
  it("finds the sheets (tokens.css and the legacy index.css at minimum)", () => {
    expect(cssFiles.some((f) => f.endsWith("styles/tokens.css"))).toBe(true);
    expect(cssFiles.some((f) => f.endsWith("index.css"))).toBe(true);
  });

  it("keeps raw hex inside theme blocks only (the plan's scripted audit)", () => {
    const offenders: string[] = [];
    for (const file of cssFiles) {
      const css = readFileSync(file, "utf8");
      const blocks = themeBlocks(css);
      for (const match of css.matchAll(HEX)) {
        const index = match.index ?? 0;
        const inside = blocks.some(([, body]) => {
          const start = css.indexOf(body);
          return start !== -1 && index >= start && index <= start + body.length;
        });
        if (!inside) {
          const line = css.slice(0, index).split("\n").length;
          offenders.push(`${file.slice(SRC.length + 1)}:${line}: ${match[0]}`);
        }
      }
    }
    expect(offenders).toEqual([]);
  });

  it("defines the plum & peach defaults exactly as the design table", () => {
    const tokens = readFileSync(resolve(SRC, "styles/tokens.css"), "utf8");
    const assertToken = (name: string, value: string) =>
      expect(tokens).toContain(`--${name}: ${value};`);
    assertToken("ink", "#5a2a4a");
    assertToken("paper", "#f8eedd");
    assertToken("card", "#fff7ea");
    assertToken("peach", "#f3b49a");
    assertToken("coral", "#e2725b");
    assertToken("muted", "#7a5468");
    assertToken("sand", "#efdfc8");
    assertToken("hair", "#d9bfa8");
  });

  it("keeps the runner-up themes as token sets (Phase 1 switch is a data attr)", () => {
    const tokens = readFileSync(resolve(SRC, "styles/tokens.css"), "utf8");
    for (const theme of ["ink-apricot", "sherbet", "lilac-night"]) {
      expect(tokens).toContain(`[data-theme="${theme}"]`);
    }
    // Spot-check the documented Lilac Night values.
    expect(tokens).toContain("--ink: #221a33;");
    expect(tokens).toContain("--peach: #c7a9ff;");
    expect(tokens).toContain("--coral: #ffb3c7;");
    expect(tokens).toContain("--paper: #f1e6ff;");
  });

  it("self-hosts the Fredoka 400/500/600/700 and Space Mono 400/700 faces", () => {
    const tokens = readFileSync(resolve(SRC, "styles/tokens.css"), "utf8");
    const faces: string[] = tokens.match(/@font-face \{[\s\S]*?\}/g) ?? [];
    expect(faces).toHaveLength(6);
    const fredoka = faces.filter((f) => f.includes('"Fredoka"'));
    expect(fredoka).toHaveLength(4);
    for (const weight of ["400", "500", "600", "700"]) {
      expect(fredoka.some((f) => f.includes(`font-weight: ${weight};`))).toBe(true);
    }
    const mono = faces.filter((f) => f.includes('"Space Mono"'));
    expect(mono).toHaveLength(2);
    for (const weight of ["400", "700"]) {
      expect(mono.some((f) => f.includes(`font-weight: ${weight};`))).toBe(true);
    }
    for (const face of faces) {
      expect(face).toContain("font-display: swap;");
      expect(face).toMatch(/url\("\/fonts\/[A-Za-z0-9-]+\.woff2"\)/);
    }
  });

  it("carries the dither recipes with the design's densities (12/25/50/75)", () => {
    // Whitespace-collapsed: the formatter may wrap long value lines, and the
    // audit must keep passing as long as the recipe is intact.
    const tokens = readFileSync(resolve(SRC, "styles/tokens.css"), "utf8").replace(/\s+/g, " ");
    // 12%: 0.6px ink dots on a 3px grid.
    expect(tokens).toContain(
      "radial-gradient(var(--dither-ink) 0.6px, transparent 1px); background-size: 3px 3px;",
    );
    // 25%: 0.9px dots on a 4px grid.
    expect(tokens).toContain(
      "radial-gradient(var(--dither-ink) 0.9px, transparent 1.3px); background-size: 4px 4px;",
    );
    // 50%: a 2px checker; 75%: the pair swapped (bg dots on ink).
    expect(tokens).toContain(
      "repeating-conic-gradient(var(--dither-ink) 0 25%, var(--dither-bg) 0 50%) 0 0 / 2px 2px",
    );
    expect(tokens).toContain("radial-gradient(var(--dither-bg) 0.9px, transparent 1.3px)");
  });

  it("applies the 2px coral focus ring (offset 1px) and the body stacks", () => {
    const tokens = readFileSync(resolve(SRC, "styles/tokens.css"), "utf8");
    expect(tokens).toContain("outline: 2px solid var(--coral);");
    expect(tokens).toContain("outline-offset: 1px;");
    expect(tokens).toContain('--font-display: "Fredoka", "Trebuchet MS", "Segoe UI", sans-serif;');
    expect(tokens).toContain('--font-mono: "Space Mono", "Courier New", monospace;');
  });

  it("honors reduced motion by settling every animated surface", () => {
    const tokens = readFileSync(resolve(SRC, "styles/tokens.css"), "utf8");
    expect(tokens).toContain("@media (prefers-reduced-motion: reduce)");
    expect(tokens).toContain("animation-duration: 0.01ms !important;");
    expect(tokens).toContain("transition-duration: 0.01ms !important;");
  });
});
