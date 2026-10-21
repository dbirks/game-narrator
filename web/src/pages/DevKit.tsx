import { useState } from "react";

import { useAuth } from "../auth/auth-context";
import {
  Card,
  Chip,
  DitherCover,
  ErrorCard,
  KitEmptyState,
  NameTab,
  PillButton,
  ProgressBar,
  Segmented,
  Sparkline,
  StatTile,
  Switch,
  Table,
  Waveform,
  readSegmented,
  type TableColumn,
} from "../components/kit";
import { readStoredTheme, setTheme } from "../lib/theme";
import { THEMES, THEME_LABELS } from "../lib/themes";

/**
 * `/dev/kit` — the shared component kit's gallery (plan.md Phase 0 item 4,
 * developer role only). Every kit piece rendered with fixture data so the
 * e2e suite can assert role + testid + aria-* on the real page, and so a
 * theme review pass has a one-screen view of the token set. All quoted copy
 * is invented placeholder text (AGENTS.md rule 5).
 */

interface FixtureRow {
  name: string;
  lines: number;
  state: string;
}

const ROWS: FixtureRow[] = [
  { name: "Mira", lines: 1204, state: "voiced" },
  { name: "The Ferryman", lines: 87, state: "brief" },
  { name: "Old Bell", lines: 41, state: "finding" },
];

const ROW_COLUMNS: TableColumn<FixtureRow>[] = [
  { key: "name", header: "Name", cell: (row) => row.name },
  { key: "lines", header: "Lines", cell: (row) => row.lines },
  { key: "state", header: "State", cell: (row) => <Chip tone="neutral">{row.state}</Chip> },
];

const VIEW_OPTIONS = [
  { value: "shelf", label: "shelf" },
  { value: "list", label: "list" },
] as const;

function KitGallery() {
  const [view, setView] = useState<"shelf" | "list">(() =>
    readSegmented(VIEW_OPTIONS, "dev-kit-view", "shelf"),
  );
  const [theme, setThemeName] = useState<string>(() => readStoredTheme() ?? "plum");

  return (
    <div className="stack" data-testid="devkit-page">
      <header className="page-header">
        <h1>Component kit</h1>
        <span className="muted">Plum &amp; Peach · developer gallery</span>
      </header>

      <Card title="Theme seam (data-theme)">
        <div className="actions">
          {THEMES.map((name) => (
            <PillButton
              key={name}
              variant={theme === name ? "ink" : "outline"}
              testid={`devkit-theme-${name}`}
              aria-pressed={theme === name}
              onClick={() => setThemeName(setTheme(name))}
            >
              {THEME_LABELS[name]}
            </PillButton>
          ))}
        </div>
        <p className="muted">
          Themes are token sets in src/styles/tokens.css selected by the <code>data-theme</code>{" "}
          attribute — the picker page arrives in Phase 1; this row is the seam.
        </p>
      </Card>

      <Card title="Pill buttons">
        <div className="actions">
          <PillButton variant="ink" testid="kit-gallery-pill-ink">
            Start narrating
          </PillButton>
          <PillButton variant="outline" testid="kit-gallery-pill-outline">
            Cast
          </PillButton>
          <PillButton variant="coral" testid="kit-gallery-pill-coral">
            Regenerate voice
          </PillButton>
          <PillButton variant="ink" disabled testid="kit-gallery-pill-disabled">
            Stopped
          </PillButton>
        </div>
      </Card>

      <Card title="Chips and name tabs">
        <div className="actions">
          <Chip>selected</Chip>
          <Chip tone="neutral">voiced 34/1204</Chip>
          <Chip tone="bad">no active picture</Chip>
          <NameTab>Mira</NameTab>
          <NameTab testid="kit-gallery-name-tab-2">The Ferryman</NameTab>
        </div>
      </Card>

      <Card title="Switch (ink pill toggle)">
        <div className="actions">
          <Switch
            label="auto-cast"
            checked
            testid="kit-gallery-switch-on"
            onChange={() => undefined}
          />
          <Switch
            label="show workings"
            checked={false}
            testid="kit-gallery-switch-off"
            onChange={() => undefined}
          />
        </div>
        <p className="muted">
          A real checkbox with <code>role="switch"</code> — the Cast auto-cast dial (ADR-0109) and
          the Play toggles take it.
        </p>
      </Card>

      <Card title="Dithered cover">
        <div className="actions">
          <DitherCover
            label="Mira, portrait placeholder"
            testid="kit-gallery-cover"
            className="devkit-cover"
            tone="peach"
          />
        </div>
      </Card>

      <Card title="Segmented (persisted)">
        <Segmented
          options={VIEW_OPTIONS}
          value={view}
          onValueChange={setView}
          label="Gallery view"
          storageKey="dev-kit-view"
          testid="kit-gallery-segmented"
        />
        <p className="muted" data-testid="kit-gallery-segmented-value">
          current: {view}
        </p>
      </Card>

      <Card title="Stat tiles and progress">
        <div className="devkit-tiles">
          <StatTile label="Lines read" value="4,212" testid="kit-gallery-stat-lines" />
          <StatTile label="Characters" value="12" testid="kit-gallery-stat-characters" />
          <StatTile label="Voiced" value="9 / 12" progress={75} testid="kit-gallery-stat-voiced" />
        </div>
        <ProgressBar value={62} label="Skies of Arcadia setup" testid="kit-gallery-progress" />
      </Card>

      <Card title="Sparkline and waveform">
        <Sparkline
          values={[4, 6, 3, 8, 9, 5, 7, 10, 6, 4, 8, 9]}
          label="Capture health, last twelve windows"
          testid="kit-gallery-sparkline"
        />
        {/* The fixed-scale shape (the health tile's level window): the bars
            keep each level's own height — a flat fail window does not
            render as a full bar, because the scale top is 100, not the
            window's max. */}
        <Sparkline
          values={[100, 100, 55, 25, 0, 100, 100, 100, 100, 25, 55, 100]}
          label="Tile levels, last twelve seconds (fixed scale to 100)"
          max={100}
          testid="kit-gallery-sparkline-levels"
        />
        <Waveform
          values={[
            0.4, 0.7, 0.9, 0.5, 0.8, 0.6, 0.95, 0.3, 0.7, 0.5, 0.85, 0.45, 0.65, 0.9, 0.5, 0.3,
          ]}
          label="Placeholder line for the speaking character."
          testid="kit-gallery-waveform"
        />
      </Card>

      <Card title="Table">
        <Table
          caption="Cast fixture"
          columns={ROW_COLUMNS}
          rows={ROWS}
          rowKey={(row) => row.name}
          rowTestid={(row) => `kit-gallery-row-${row.name.toLowerCase().replace(/\s+/g, "-")}`}
          testid="kit-gallery-table"
        />
      </Card>

      <Card title="Form controls (range and select)">
        <div className="stack">
          <div>
            <label htmlFor="kit-gallery-range">Master volume</label>
            <input
              id="kit-gallery-range"
              className="kit-range"
              data-testid="kit-gallery-range"
              data-volume="0.50"
              type="range"
              min={0}
              max={1}
              step={0.05}
              defaultValue={0.5}
            />
          </div>
          <div>
            <label htmlFor="kit-gallery-select">Audio output device</label>
            <select
              id="kit-gallery-select"
              className="kit-select"
              data-testid="kit-gallery-select"
              defaultValue="hdmi"
            >
              <option value="hdmi">hdmi-out (hdmi)</option>
              <option value="usb">jbl-usb-dac (usb)</option>
              <option value="bluetooth">bluez_output.AA:BB:CC:DD:EE:FF.1 (bluetooth)</option>
            </select>
          </div>
        </div>
      </Card>

      <Card title="Empty and error states">
        <div className="stack">
          <KitEmptyState title="No games yet">
            <p className="muted">Placeholder line for the empty state.</p>
          </KitEmptyState>
          <ErrorCard
            title="Cannot load the library"
            code="INTERNAL"
            requestId="7f3c91ab-4c2e-4b7a-9d11-0e5a6b7c8d9e"
            onRetry={() => undefined}
            testid="kit-gallery-error"
          >
            <p className="muted">Placeholder line for the error state.</p>
          </ErrorCard>
        </div>
      </Card>
    </div>
  );
}

export function DevKitPage() {
  const { atLeast } = useAuth();

  if (!atLeast("developer")) {
    return (
      <main>
        <h1>Component kit</h1>
        <ErrorCard title="Developer role required" testid="devkit-forbidden" code="FORBIDDEN">
          <p className="muted">
            The kit gallery is a developer surface; sign in as a developer or admin to review it.
          </p>
        </ErrorCard>
      </main>
    );
  }

  return (
    <main>
      <KitGallery />
    </main>
  );
}
