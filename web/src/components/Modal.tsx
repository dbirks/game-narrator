import * as DialogPrimitive from "@radix-ui/react-dialog";
import { useId, type ReactNode } from "react";

/**
 * `src/components/Modal.tsx` — the app's dialog, on Radix Primitives.
 *
 * ADR-0011 adopts Radix per component and styles it with plain CSS from
 * `[data-state]`/`[role]` selectors; this is the first adoption, so it is the
 * wrapper the ADR's rollback plan promises (swap Radix for something else and
 * only this file and its CSS change). Focus trapping, Escape-to-close and focus
 * return to the trigger are the parts Radix owns; the label wiring is ours
 * because a dialog whose heading is not its accessible name fails a screen
 * reader no matter where the focus went.
 */
export interface ModalProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: ReactNode;
  children: ReactNode;
}

export function Modal({ open, onOpenChange, title, description, children }: ModalProps) {
  const headingId = useId();
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="modal-overlay" />
        <DialogPrimitive.Content
          className="modal-content"
          aria-labelledby={headingId}
          data-testid="modal"
        >
          <DialogPrimitive.Title className="modal-title" id={headingId}>
            {title}
          </DialogPrimitive.Title>
          {description ? (
            <DialogPrimitive.Description asChild>{description}</DialogPrimitive.Description>
          ) : null}
          {children}
          <DialogPrimitive.Close asChild>
            <button type="button" className="link-button" data-testid="modal-close">
              Close
            </button>
          </DialogPrimitive.Close>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

/** A trigger button that opens a dialog and hands it a label. */
export function DialogTriggerButton({
  children,
  onClick,
  disabled,
  variant = "primary",
}: {
  children: ReactNode;
  onClick: () => void;
  disabled?: boolean;
  variant?: "primary" | "ghost";
}) {
  return (
    <button
      type="button"
      className={variant === "ghost" ? "link-button" : undefined}
      onClick={onClick}
      disabled={disabled}
    >
      {children}
    </button>
  );
}
