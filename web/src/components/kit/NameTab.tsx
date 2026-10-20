/** Name tabs: mono 11px 700 uppercase, letter-spacing 0.1em, on ink
 *  (docs/design/plum-peach "Typography" and the cast/play name treatment). */
export function NameTab({
  children,
  testid = "kit-name-tab",
}: {
  children: string;
  testid?: string;
}) {
  return (
    <span className="kit-name-tab" data-testid={testid}>
      {children}
    </span>
  );
}
