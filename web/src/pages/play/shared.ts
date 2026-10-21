/**
 * `src/pages/play/shared.ts` — small pure helpers the Play views share
 * (docs/design/plum-peach 2e/2f/2g): the deterministic waveform envelope for
 * a committed line (a visual twin of the text twin — the line's own bytes
 * pick the bars, so the same line always draws the same shape) and the
 * short clock the event table's time column uses.
 */

/** FNV-1a: cheap, stable across runs and platforms (no Math.random anywhere
 *  near the design surface). */
function fnv1a(text: string): number {
  let hash = 0x811c9dc5;
  for (let i = 0; i < text.length; i++) {
    hash ^= text.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  return hash >>> 0;
}

/**
 * A 48-bar envelope (0–1) for the line's text: deterministic per line, with
 * a speech-like shape (a rise, a body, a fall) so the waveform reads as a
 * voice rather than noise. A placeholder twin — the real clip's envelope
 * lands with the voice work, and the text stays the caption either way.
 */
export function waveformValues(text: string, bars = 48): number[] {
  const seed = fnv1a(text || "…");
  const values: number[] = [];
  let state = seed || 0x9e3779b9;
  const next = (): number => {
    state = Math.imul(state ^ (state >>> 15), 2246822507);
    state = Math.imul(state ^ (state >>> 13), 3266489909);
    state ^= state >>> 16;
    return (state >>> 0) / 0xffffffff;
  };
  for (let i = 0; i < bars; i++) {
    // The envelope: a fast rise over the first 10%, a body, a fall over the
    // last 20% — multiplied by the line's own per-bar jitter.
    const position = i / (bars - 1);
    const rise = position < 0.1 ? position / 0.1 : 1;
    const fall = position > 0.8 ? (1 - position) / 0.2 : 1;
    const jitter = 0.35 + 0.65 * next();
    values.push(Math.min(1, Math.max(0.08, rise * fall * jitter)));
  }
  return values;
}

/**
 * One ellipsised snippet of a line's text for the operator's candidate
 * rows (the review's fix for the raw-ULID rows: the row names the line,
 * and the message id retreats to the row's title/tooltip). Whitespace
 * collapses to single spaces; the cut keeps the final cell tidy.
 */
export function lineSnippet(text: string, width = 40): string {
  const flat = text.replace(/\s+/g, " ").trim();
  if (flat.length <= width) return flat;
  return `${flat.slice(0, width - 1).replace(/\s+$/, "")}…`;
}

/** "2026-10-20T14:03:22Z" → "14:03:22" (the table's clock column). */
export function clockTime(iso: string): string {
  const match = /T(\d{2}:\d{2}:\d{2})/.exec(iso);
  return match ? match[1] : iso;
}

/** One short display form for a probability (the candidate bars' caption). */
export function probabilityText(probability: number): string {
  return `${Math.round(probability * 100)}%`;
}
