import * as TabsPrimitive from "@radix-ui/react-tabs";

export interface SegmentedOption<T extends string> {
  value: T;
  label: string;
}

export interface SegmentedProps<T extends string> {
  options: readonly SegmentedOption<T>[];
  value: T;
  onValueChange: (value: T) => void;
  /** The accessible name of the control (e.g. "Library view"). */
  label: string;
  /** When set, the choice persists per user in localStorage (UX-08):
   *  read on mount, written on change, both safe on storage exceptions
   *  (private mode, quota — the app must not crash on a denied write). */
  storageKey?: string;
  testid?: string;
}

function readStored<T extends string>(
  key: string,
  options: readonly SegmentedOption<T>[],
  fallback: T,
): T {
  try {
    const value = globalThis.localStorage.getItem(key);
    return options.some((option) => option.value === value) ? (value as T) : fallback;
  } catch {
    return fallback;
  }
}

function writeStored(key: string, value: string): void {
  try {
    globalThis.localStorage.setItem(key, value);
  } catch {
    // A denied write degrades to per-session persistence; the toggle still
    // works for this visit.
  }
}

/** The segmented toggle (shelf/list, list/cards, careful/eager): the design's
 *  view switch with the active segment ink-filled. Radix tabs carry the
 *  a11y contract (role=tablist, role=tab, aria-selected, arrow-key motion);
 *  the control persists its choice when given a storageKey (UX-08). */
export function Segmented<T extends string>({
  options,
  value,
  onValueChange,
  label,
  storageKey,
  testid = "kit-segmented",
}: SegmentedProps<T>) {
  const handleValueChange = (next: string) => {
    const typed = next as T;
    onValueChange(typed);
    if (storageKey !== undefined) writeStored(storageKey, typed);
  };

  return (
    <TabsPrimitive.Root value={value} onValueChange={handleValueChange}>
      <TabsPrimitive.List className="kit-segmented" aria-label={label} data-testid={testid}>
        {options.map((option) => (
          <TabsPrimitive.Trigger
            key={option.value}
            value={option.value}
            className="kit-segmented-option"
          >
            {option.label}
          </TabsPrimitive.Trigger>
        ))}
      </TabsPrimitive.List>
      {options.map((option) => (
        <TabsPrimitive.Content
          key={option.value}
          value={option.value}
          className="kit-segmented-panel"
        />
      ))}
    </TabsPrimitive.Root>
  );
}

/** The initial persisted value for a storage-backed toggle (read-only half
 *  of Segmented's UX-08 contract; storage exceptions fall back). */
export function readSegmented<T extends string>(
  options: readonly SegmentedOption<T>[],
  storageKey: string,
  fallback: T,
): T {
  return readStored(storageKey, options, fallback);
}
