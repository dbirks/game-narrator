import { useQuery } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";

import { Segmented } from "../components/kit";
import { ErrorState } from "../components/states";
import { fetchSystemInfo } from "../api/system";
import { useLiveStatus, type StatusSnapshot } from "../lib/liveStatus";
import { CaptureSignalBlock } from "./health/signal-block";
import { HealthTiles } from "./health/tiles";
import { HostSection, VideoInputSection } from "./health/sections";
import { ModelRolesSection } from "./health/model-roles";
import { SpeakerTestSection, VoiceRegistrySection } from "./health/registry";
import { JobQueueSection } from "./health/jobs";
import { ReadinessSection, SystemLogsSection } from "./health/logs";

/**
 * `/health` — the Health screen (docs/design/plum-peach README 2i; ADR-0093
 * §4 renames /diagnostics to /health; ADR-0111).
 *
 * The page is public (ADR-0016 §3): build facts, the tile grid (the
 * server-rendered rollup from the one status stream), the host vitals and
 * the readiness checks work before an account exists; the gated sections
 * render their honest signed-out states. The 3×2 grid reads the SAME
 * stream as the status bar (UX-07's one subscription per page), so a
 * non-ok tile and the bar's tail name the same component from the same
 * bytes. The capture tile alone carries the UX-11 signal/preview block.
 *
 * The page's title is the rollup's own summary (README 2i's 36/600 page
 * title; the run-3715836 review fix 4, re-worded to human by the
 * 2026-10-20 review): "2 need attention" when the rollup's tail names slow
 * or fail tiles (the same word the bar carries), "N checks not run yet" for
 * a box nobody has probed yet (its honest word, never a restyled "all
 * fine"), the mock's "Everything's mostly fine" when every
 * tile is ok — and
 * the page's own name while the one stream has not delivered a rollup. The
 * tab row sits to the title's right, on one line when it fits.
 *
 * The tab row (README 2i): line traces · setup runs · **health** · logs.
 * health is the default (grid + the preserved sections); logs is the
 * preserved log tail; setup runs lists the live onboarding runs from the
 * one stream; line traces is the session's line activity or the honest
 * pointer to the Traces screen (ADR-0111 §4 — per-line inspection lives
 * there, and the API has no history to list here). The preserved sections
 * run full-width under the grid — two columns at the desktop width (the
 * run-3715836 review fix 5), one below — so none is crammed into a narrow
 * column; their data-testids are unchanged.
 */

const TABS = [
  { value: "traces", label: "line traces" },
  { value: "setup", label: "setup runs" },
  { value: "health", label: "health" },
  { value: "logs", label: "logs" },
] as const;

type HealthTab = (typeof TABS)[number]["value"];

/**
 * The page's health summary (the design's 36 px title word; the
 * run-3715836 review fix 4). It reads only the rollup's counts — the same
 * numbers the bar's tail renders — and never re-derives a level: attention
 * is the slow+fail count (the rollup's non-ok tail), and an unprobed tile
 * keeps its own word instead of being restyled into a fake "all fine".
 * No rollup at all (the one stream has not delivered a frame) is the page's
 * own name, never a guess.
 */
/** The human word for the rollup (the 2026-10-20 review: "N unprobed" was
 *  developer-speak for the headline): something that needs attention says
 *  how much; an all-ok box with unrun checks says it's mostly fine and
 *  how many checks have not run yet; a clean box says it's fine. */
export function healthSummaryText(
  health: { slow: number; fail: number; unknown: number } | undefined,
): string {
  if (health === undefined) return "Health";
  const attention = health.slow + health.fail;
  const notRunYet =
    health.unknown > 0
      ? ` · ${health.unknown} check${health.unknown === 1 ? "" : "s"} not run yet`
      : "";
  if (attention > 0) {
    return `${attention} need${attention === 1 ? "s" : ""} attention${notRunYet}`;
  }
  if (health.unknown > 0)
    return `${health.unknown} check${health.unknown === 1 ? "" : "s"} not run yet`;
  return "Everything's mostly fine";
}

export function HealthPage() {
  const [tab, setTab] = useState<HealthTab>("health");
  const { snapshot } = useLiveStatus();
  const system = useQuery({
    queryKey: ["system", "info"],
    queryFn: ({ signal }) => fetchSystemInfo(signal),
  });
  const title = healthSummaryText(snapshot?.health);

  let body: ReactNode;
  if (system.error) {
    body = (
      <ErrorState
        error={system.error}
        action="Health unavailable"
        onRetry={() => {
          void system.refetch();
        }}
      />
    );
  } else if (!system.data) {
    body = <p className="muted">Loading…</p>;
  } else {
    const info = system.data;
    body = (
      <>
        <dl className="facts">
          <div>
            <dt>Version</dt>
            <dd data-testid="version">{info.version}</dd>
          </div>
          <div>
            <dt>Commit</dt>
            <dd data-testid="commit">{info.commit}</dd>
          </div>
          <div>
            <dt>Go version</dt>
            <dd>{info.go_version}</dd>
          </div>
          <div>
            <dt>Platform</dt>
            <dd data-testid="platform">
              {info.os}/{info.arch}
            </dd>
          </div>
          <div>
            <dt>Started at</dt>
            <dd>{info.started_at}</dd>
          </div>
        </dl>
        {tab === "health" ? (
          <>
            <HealthTiles signalExtra={<CaptureSignalBlock active={tab === "health"} />} />
            {/* Full-width under the grid: two columns at the desktop width,
                one below (the run-3715836 review fix 5). The sections keep
                their own testids; the column reflow is the page's. */}
            <div className="health-sections">
              <HostSection />
              <VideoInputSection />
              <ModelRolesSection />
              <VoiceRegistrySection />
              <SpeakerTestSection />
              <ReadinessSection />
              <JobQueueSection />
            </div>
          </>
        ) : null}
        {tab === "logs" ? <SystemLogsSection /> : null}
        {tab === "setup" ? <SetupRunsTab /> : null}
        {tab === "traces" ? <LineTracesTab /> : null}
      </>
    );
  }

  return (
    <main data-testid="health-page">
      <div className="health-title-row">
        <h1 data-testid="health-title">{title}</h1>
        <Segmented
          options={TABS}
          value={tab}
          onValueChange={setTab}
          label="Health screen"
          testid="health-tabs"
        />
      </div>
      {body}
    </main>
  );
}

/**
 * The setup-runs tab (ADR-0111 §4): the live onboarding runs from the one
 * status stream (state, stage, voiced/total, elapsed) — real data from the
 * same feed the bar reads, with the dashed empty state when none is live.
 */
function SetupRunsTab() {
  const { snapshot } = useLiveStatus();
  const runs = snapshot?.setup_runs ?? [];

  if (runs.length === 0) {
    return (
      <div className="empty" data-testid="health-setup-runs-empty">
        <h2>No setup runs are active</h2>
        <p className="muted">
          An onboarding run shows here from the moment it starts, on the same one status stream the
          bar reads. <a href="/library">Add a game</a> to start one.
        </p>
      </div>
    );
  }

  return (
    <section className="panel" data-testid="health-setup-runs">
      <h2>Setup runs</h2>
      <table>
        <thead>
          <tr>
            <th>Game</th>
            <th>State</th>
            <th>Stage</th>
            <th>Voiced</th>
            <th>Elapsed</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((run) => (
            <tr key={run.run_id} data-testid={`health-setup-run-${run.run_id}`}>
              <td>{run.game || "unnamed game"}</td>
              <td>{run.state}</td>
              <td>{run.stage || "—"}</td>
              <td>
                {run.voiced} of {run.characters || "?"}
              </td>
              <td>{formatElapsed(run.elapsed_ms)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

/** The stream's elapsed-ms number, in the bar's shape (42s / 18m 04s). */
function formatElapsed(ms: number): string {
  const totalSeconds = Math.max(0, Math.floor(ms / 1000));
  const minutes = Math.floor(totalSeconds / 60);
  if (minutes < 1) return `${totalSeconds}s`;
  return `${minutes}m ${String(totalSeconds % 60).padStart(2, "0")}s`;
}

/**
 * The line-traces tab (ADR-0111 §4): the active session's line activity
 * from the status snapshot when one runs, and with no session the honest
 * pointer to the Traces screen — per-line inspection (the 5-step strip,
 * "what it read", "why this line") is the Traces screen's job
 * (ADR-0093), and the API has no line-history list to render here.
 */
function LineTracesTab() {
  const { snapshot } = useLiveStatus();
  const session = snapshot?.session;

  if (session === undefined || session === null) {
    return (
      <div className="empty" data-testid="health-line-traces-empty">
        <h2>No line traces to show</h2>
        <p className="muted">
          Line traces are the per-line records of a live run — frame, read, match, speaker, speech.
          While a session runs this tab shows its line activity; per-line inspection (what it read,
          why this line) lives on the <a href="/traces">traces screen</a>.
        </p>
      </div>
    );
  }

  return (
    <section className="panel" data-testid="health-line-traces-session">
      <h2>Active session's line activity</h2>
      <dl className="facts">
        <div>
          <dt>Session</dt>
          <dd>
            {session.game ?? "unnamed game"} · {session.mode}
          </dd>
        </div>
        <div>
          <dt>State</dt>
          <dd>{session.paused ? "paused" : session.state}</dd>
        </div>
        <div>
          <dt>Lines spoken</dt>
          <dd data-testid="health-line-traces-spoken">{session.lines_spoken}</dd>
        </div>
        <div>
          <dt>Frame source</dt>
          <dd>{session.source}</dd>
        </div>
        <div>
          <dt>Frame→voice p50</dt>
          <dd>{session.frame_to_voice_p50_ms > 0 ? `${session.frame_to_voice_p50_ms} ms` : "—"}</dd>
        </div>
      </dl>
      <p className="muted">
        The per-line records stream to the <a href="/traces">traces screen</a> as the session runs.
      </p>
    </section>
  );
}

/** The snapshot's type surface the tabs read (the feed's contract). */
export type HealthSnapshot = StatusSnapshot;
