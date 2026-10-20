import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

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
  Table,
  Waveform,
  readSegmented,
  type TableColumn,
} from ".";

/**
 * The kit's contract (plan.md Phase 0 item 4): accessible semantics
 * (buttons are buttons, the segmented toggle is a tablist, progress is
 * role=progressbar), stable testids, and no inline styles — colours come
 * from the token classes only.
 */

describe("PillButton", () => {
  it("renders a real button with the variant class and fires", () => {
    let fired = 0;
    render(
      <PillButton variant="ink" onClick={() => fired++} testid="t">
        Start
      </PillButton>,
    );
    const button = screen.getByTestId("t");
    expect(button.tagName).toBe("BUTTON");
    expect(button.className).toContain("kit-pill-ink");
    fireEvent.click(button);
    expect(fired).toBe(1);
  });

  it("supports the coral variant and disabled", () => {
    render(
      <>
        <PillButton variant="coral" testid="c">
          Regen
        </PillButton>
        <PillButton variant="ink" disabled testid="d">
          Stopped
        </PillButton>
      </>,
    );
    expect(screen.getByTestId("c").className).toContain("kit-pill-coral");
    expect(screen.getByTestId("d")).toBeDisabled();
  });
});

describe("Chip / NameTab", () => {
  it("renders the peach default and the neutral tone", () => {
    render(
      <>
        <Chip>selected</Chip>
        <Chip tone="neutral">voiced 34/1204</Chip>
      </>,
    );
    expect(screen.getByText("selected").className).toBe("kit-chip");
    expect(screen.getByText("voiced 34/1204").className).toContain("kit-chip-neutral");
  });

  it("renders the name tab with the peach tick", () => {
    render(<NameTab>Mira</NameTab>);
    expect(screen.getByText("Mira").className).toContain("kit-name-tab");
  });
});

describe("DitherCover", () => {
  it("is an image with the five bands 0/12/50/75/100", () => {
    const { container } = render(<DitherCover label="Mira" />);
    const cover = screen.getByTestId("kit-dither-cover");
    expect(cover).toHaveAttribute("role", "img");
    expect(cover).toHaveAttribute("aria-label", "Mira");
    for (const band of ["dither-0", "dither-12", "dither-50", "dither-75", "dither-100"]) {
      expect(container.querySelector(`.${band}`)).not.toBeNull();
    }
  });
});

describe("Segmented", () => {
  const options = [
    { value: "shelf", label: "shelf" },
    { value: "list", label: "list" },
  ] as const;

  it("exposes the tablist contract and reports selection", () => {
    render(
      <Segmented options={options} value="shelf" onValueChange={() => undefined} label="View" />,
    );
    expect(screen.getByRole("tablist")).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "shelf" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tab", { name: "list" })).toHaveAttribute("aria-selected", "false");
  });

  it("persists the choice through the storage seam (UX-08)", () => {
    localStorage.clear();
    const changes: string[] = [];
    render(
      <Segmented
        options={options}
        value="shelf"
        onValueChange={(value) => changes.push(value)}
        label="View"
        storageKey="test-view"
      />,
    );
    // Radix's tab trigger selects on mousedown (its keyboard path is
    // Enter/Space); the focus half can fire the same change a second time,
    // so assert the choice landed and persisted, not the call count.
    fireEvent.mouseDown(screen.getByRole("tab", { name: "list" }), { button: 0 });
    expect(changes.length).toBeGreaterThanOrEqual(1);
    expect(changes.at(-1)).toBe("list");
    expect(localStorage.getItem("test-view")).toBe("list");
  });

  it("falls back to the default on a corrupt or missing stored value", () => {
    localStorage.clear();
    expect(readSegmented(options, "missing", "shelf")).toBe("shelf");
    localStorage.setItem("corrupt", "grid");
    expect(readSegmented(options, "corrupt", "shelf")).toBe("shelf");
  });
});

describe("StatTile / ProgressBar", () => {
  it("renders the stat with the progressbar foot", () => {
    render(<StatTile label="Voiced" value="9 / 12" progress={75} />);
    const bar = screen.getByTestId("kit-stat-progress");
    expect(bar).toHaveAttribute("role", "progressbar");
    expect(bar).toHaveAttribute("aria-valuenow", "75");
  });

  it("clamps out-of-range progress", () => {
    render(<ProgressBar value={150} label="Setup" />);
    expect(screen.getByTestId("kit-progress")).toHaveAttribute("aria-valuenow", "100");
  });
});

describe("Sparkline / Waveform", () => {
  it("are labelled images", () => {
    render(
      <>
        <Sparkline values={[1, 2, 3]} label="Health window" />
        <Waveform values={[0.2, 0.5]} label="Clip envelope" />
      </>,
    );
    expect(screen.getByLabelText("Health window")).toHaveAttribute("role", "img");
    expect(screen.getByLabelText("Clip envelope")).toHaveAttribute("role", "img");
  });

  it("keeps each value's own height on a fixed scale (max), not the window's max", () => {
    // A uniform fail window on the health tile's fixed scale (100) must
    // render as a quarter bar, not a full bar: scaling to the window's own
    // max would erase the level the fixed scale encodes.
    const { unmount } = render(
      <Sparkline values={[25, 25, 25]} label="Fail window" max={100} testid="fixed" />,
    );
    const heights = Array.from(document.querySelectorAll('[data-testid="fixed"] rect[fill]'))
      .slice(0, 3)
      .map((rect) => rect.getAttribute("height"));
    expect(heights).toEqual(["5", "5", "5"]);
    unmount();
    // Without the fixed scale the same window fills the bar (its own max).
    render(<Sparkline values={[25, 25, 25]} label="Fail window" testid="own" />);
    const own = Array.from(document.querySelectorAll('[data-testid="own"] rect[fill]'))
      .slice(0, 3)
      .map((rect) => rect.getAttribute("height"));
    expect(own).toEqual(["20", "20", "20"]);
  });
});

interface Row {
  name: string;
  state: string;
}

const columns: TableColumn<Row>[] = [
  { key: "name", header: "Name", cell: (row) => row.name },
  { key: "state", header: "State", cell: (row) => row.state },
];

describe("Table", () => {
  it("is a real table with a caption and testable rows", () => {
    const rows = [
      { name: "Mira", state: "voiced" },
      { name: "Old Bell", state: "brief" },
    ];
    render(
      <Table
        caption="Cast fixture"
        columns={columns}
        rows={rows}
        rowKey={(row) => row.name}
        rowTestid={(row) => `row-${row.name}`}
      />,
    );
    expect(screen.getByRole("table")).toBeInTheDocument();
    expect(screen.getByText("Cast fixture")).toBeInTheDocument();
    expect(screen.getByTestId("row-Mira")).toBeInTheDocument();
    expect(screen.getAllByRole("row").length).toBe(3); // header + 2
  });
});

describe("Empty / Error states", () => {
  it("renders the empty state", () => {
    render(<KitEmptyState title="No games yet" />);
    expect(screen.getByText("No games yet")).toBeInTheDocument();
  });

  it("renders the error state with the request id and retry", () => {
    let retried = 0;
    render(
      <ErrorCard
        title="Cannot load the library"
        code="INTERNAL"
        requestId="7f3c91ab"
        onRetry={() => retried++}
      />,
    );
    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(screen.getByTestId("kit-error-meta").textContent).toContain("7f3c91ab");
    fireEvent.click(screen.getByTestId("kit-error-retry"));
    expect(retried).toBe(1);
  });
});

describe("Card", () => {
  it("carries its title and body", () => {
    render(<Card title="Voices">body</Card>);
    expect(screen.getByText("Voices")).toBeInTheDocument();
    expect(screen.getByText("body")).toBeInTheDocument();
  });
});
