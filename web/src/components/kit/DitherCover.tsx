/**
 * `src/components/kit/DitherCover.tsx` — the dithered cover placeholder:
 * the stacked bands 0% → 12% → 50% → 75% → 100% (docs/design/plum-peach
 * "Dither textures"). Real portrait art drops into the same frame in
 * Phase 1; the bands are the honest stand-in.
 *
 * `tone="peach"` runs the bands on the --peach base: the README names
 * --peach the "portrait base", so character portraits (avatars, strips,
 * the detail panel's cover) use it, and the design's 2a game covers run
 * the stacked bands over the same peach ground (the 2026-10-20 review:
 * the mock's covers start on the peach top band and fade to ink). Static
 * recipe, static bands: no dynamic geometry, no inline styles.
 */
export interface DitherCoverProps {
  testid?: string;
  /** Accessible name for the placeholder (e.g. the character's name). */
  label?: string;
  /** Frame sizing is the caller's (tile 150px, hero 300px, arch 260x300). */
  className?: string;
  /** The bands' base token: paper by default, peach for portraits. */
  tone?: "default" | "peach";
}

export function DitherCover({
  testid = "kit-dither-cover",
  label,
  className = "",
  tone = "default",
}: DitherCoverProps) {
  return (
    <div
      className={`dither-cover ${tone === "peach" ? "dither-cover-peach" : ""} ${className}`.trim()}
      role="img"
      aria-label={label ?? "Portrait placeholder"}
      data-testid={testid}
    >
      <span className="dither-0" />
      <span className="dither-12" />
      <span className="dither-50" />
      <span className="dither-75" />
      <span className="dither-100" />
    </div>
  );
}
