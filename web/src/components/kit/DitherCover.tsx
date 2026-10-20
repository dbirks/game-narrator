/** The dithered cover placeholder: the stacked bands 0% → 12% → 50% → 75%
 *  → 100% (docs/design/plum-peach "Dither textures"). Real portrait art
 *  drops into the same frame in Phase 1; the bands are the honest stand-in.
 *  Static recipe, static bands: no dynamic geometry, no inline styles. */
export function DitherCover({
  testid = "kit-dither-cover",
  label,
  className = "",
}: {
  testid?: string;
  /** Accessible name for the placeholder (e.g. the character's name). */
  label?: string;
  /** Frame sizing is the caller's (tile 150px, hero 300px, arch 260x300). */
  className?: string;
}) {
  return (
    <div
      className={`dither-cover ${className}`.trim()}
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
