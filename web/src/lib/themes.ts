/**
 * The four token sets of docs/design/plum-peach/README.md "Themes", in
 * selection order: Plum & Peach is the default (it is the :root set, so
 * `data-theme` is absent for it). One theme set, one place: the CSS files
 * are the only owners of colour (ADR-0011, ADR-0093).
 */
export const THEMES = ["plum", "ink-apricot", "sherbet", "lilac-night"] as const;

export type Theme = (typeof THEMES)[number];

export const THEME_LABELS: Record<Theme, string> = {
  plum: "Plum & Peach",
  "ink-apricot": "Ink Blue & Apricot",
  sherbet: "Sherbet",
  "lilac-night": "Lilac Night",
};
