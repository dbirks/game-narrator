import { forwardRef, type ButtonHTMLAttributes } from "react";

export type PillVariant = "ink" | "outline" | "coral";

export interface PillButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  /** ink = primary action; outline = secondary; coral = the hot "act now". */
  variant?: PillVariant;
  testid?: string;
}

/** The pill button (999px radius, Fredoka 600). A real <button>: focusable,
 *  keyboard-activatable, its state in the DOM for the e2e suite. */
export const PillButton = forwardRef<HTMLButtonElement, PillButtonProps>(function PillButton(
  { variant = "outline", type = "button", testid = "kit-pill-button", ...rest },
  ref,
) {
  const variantClass =
    variant === "ink"
      ? "kit-pill-ink"
      : variant === "coral"
        ? "kit-pill-coral"
        : "kit-pill-outline";
  return (
    <button
      ref={ref}
      type={type}
      className={`kit-pill ${variantClass}`}
      data-testid={testid}
      {...rest}
    />
  );
});
