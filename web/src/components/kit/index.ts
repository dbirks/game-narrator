/**
 * The Plum & Peach shared component kit (docs/design/plum-peach, plan.md
 * Phase 0 item 4). Every piece: theme-agnostic (tokens only), a stable
 * `data-testid`, accessible semantics (buttons are buttons, the segmented
 * toggle is Radix's tablist, the progress bar is role=progressbar), and the
 * design's restyled empty (dashed) / error (coral + request_id) states.
 *
 * The dev gallery route (`/dev/kit`, developer role only) renders each with
 * fixture data; its e2e assertions live in e2e/redesign-shell.spec.ts.
 */
export { Card, type CardProps } from "./Card";
export { PillButton, type PillButtonProps, type PillVariant } from "./PillButton";
export { Chip, type ChipProps } from "./Chip";
export { NameTab } from "./NameTab";
export { DitherCover, type DitherCoverProps } from "./DitherCover";
export { Switch, type SwitchProps } from "./Switch";
export { Segmented, readSegmented, type SegmentedOption, type SegmentedProps } from "./Segmented";
export { StatTile, type StatTileProps } from "./StatTile";
export { ProgressBar, type ProgressBarProps } from "./ProgressBar";
export { Sparkline, type SparklineProps } from "./Sparkline";
export { Waveform, type WaveformProps } from "./Waveform";
export { Table, type TableProps, type TableColumn } from "./Table";
export { KitEmptyState, type KitEmptyStateProps } from "./EmptyState";
export { ErrorCard, type ErrorCardProps } from "./ErrorCard";
