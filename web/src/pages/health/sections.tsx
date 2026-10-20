import { formatBytes, gatedJson, type HostReport } from "../../api/diagnostics";
import { ApiError } from "../../api/client";
import { ErrorState } from "../../components/states";
import { GatedHint, useGated } from "./gated";

/**
 * `src/pages/health/sections.tsx` — the Health screen's preserved host and
 * video-input sections (docs/spec/09: "video input modes,
 * power/environment warnings, CPU/RAM/storage, …"), moved from the
 * pre-redesign /diagnostics page by ADR-0111 §1 (the route and file rename;
 * the content's contracts and data-testids are unchanged). The gated-read
 * contract they share lives in ./gated.tsx.
 */

// ── section loaders (module-level: stable references for useGated) ─────────

export const loadHost = (signal: AbortSignal) =>
  gatedJson<HostReport>("/api/v1/diagnostics/host", undefined, signal);

export const loadCapture = (signal: AbortSignal) =>
  gatedJson<{
    synthetic: boolean;
    note?: string;
    selected_id?: string;
    devices: {
      id: string;
      name: string;
      path: string;
      state: string;
      link?: { speed_mbps: number };
      chosen?: { w: number; h: number; fps: number; pix_fmt: string; source: string };
      detail?: string;
    }[];
  }>("/api/v1/capture/devices", undefined, signal);

// ── host: CPU, RAM, storage, power/environment ──────────────────────────────

export function HostSection() {
  const host = useGated(loadHost, true);

  return (
    <section className="panel" data-testid="host-vitals">
      <h2>CPU, RAM, storage, environment</h2>
      {host.state.kind === "loading" ? (
        <p className="muted">Reading the host…</p>
      ) : host.state.kind === "error" ? (
        <ErrorState
          error={
            new ApiError({
              status: host.state.status,
              code: "http-" + host.state.status,
              title: "Host read failed",
              detail: host.state.detail,
            })
          }
          action="Cannot read the host vitals"
          onRetry={host.refetch}
        />
      ) : host.state.kind === "ok" ? (
        <HostFacts report={host.state.body} />
      ) : (
        <GatedHint state={host.state} action="Reading the host vitals" refetch={host.refetch} />
      )}
    </section>
  );
}

function HostFacts({ report }: { report: HostReport }) {
  return (
    <div className="stack">
      <dl className="facts">
        <div>
          <dt>CPU</dt>
          <dd>
            {report.cpu.cores} cores
            {report.cpu.load_1m != null
              ? ` · load ${report.cpu.load_1m} / ${report.cpu.load_5m} / ${report.cpu.load_15m}`
              : " · load averages not exposed by this kernel"}
          </dd>
        </div>
        <div>
          <dt>RAM</dt>
          <dd data-testid="host-ram">
            {report.ram
              ? `${formatBytes(report.ram.available_bytes)} of ${formatBytes(report.ram.total_bytes)} available`
              : "not exposed by this kernel"}
          </dd>
        </div>
        <div>
          <dt>Storage</dt>
          <dd data-testid="host-storage">
            {report.storage.known
              ? `${formatBytes(report.storage.free_bytes)} free of ${formatBytes(
                  report.storage.total_bytes,
                )}; asset store ${formatBytes(
                  report.storage.asset_store_bytes,
                )} (data directory: ${report.storage.data_dir || "unknown name"})`
              : "the data directory could not be measured"}
          </dd>
        </div>
        <div>
          <dt>Thermal</dt>
          <dd data-testid="host-thermal">
            {report.thermal.available
              ? report.thermal.zones
                  .map((zone) => `${zone.name}: ${Math.round(zone.celsius * 10) / 10} °C`)
                  .join(" · ")
              : "not exposed by this kernel (unknown, not cool)"}
          </dd>
        </div>
        <div>
          <dt>CPU throttling</dt>
          <dd data-testid="host-throttle">
            {report.cpu.throttle_count != null
              ? `${report.cpu.throttle_count} throttle events since boot (x86)`
              : "not exposed on this platform (the ARM counter is absent)"}
          </dd>
        </div>
      </dl>
      {(report.warnings ?? []).length > 0 ? (
        // role="alert" goes on the wrapper, not the <ul>: an explicit role on
        // the list overrides its implicit list role and the <li>s stop being
        // "contained in a <ul>" for axe (listitem rule, WCAG 1.3.1).
        <div role="alert" data-testid="host-warnings">
          <ul className="host-warning-list">
            {report.warnings.map((warning) => (
              <li key={warning} className="error" data-testid="host-warning">
                {warning}
              </li>
            ))}
          </ul>
        </div>
      ) : (
        <p className="muted" data-testid="host-no-warnings">
          No warning crossed a fixed threshold (1 GiB free, load above cores, 85 °C, 10% RAM
          available).
        </p>
      )}
      <p className="muted">
        USB device error counters are not exposed by this build: the kernel's per-port counters have
        no endpoint here, so a USB fault would show as the capture or audio section's own state
        instead.
      </p>
    </div>
  );
}

// ── video input ─────────────────────────────────────────────────────────────

export function VideoInputSection() {
  const capture = useGated(loadCapture, true);

  return (
    <section className="panel" data-testid="video-input">
      <h2>Video input</h2>
      {capture.state.kind === "loading" ? (
        <p className="muted">Enumerating the capture devices…</p>
      ) : capture.state.kind === "unauthenticated" || capture.state.kind === "forbidden" ? (
        <GatedHint
          state={capture.state}
          action="Video input"
          refetch={capture.refetch}
          needs="the operator role"
        />
      ) : capture.state.kind === "error" ? (
        capture.state.status === 503 ? (
          <p className="muted" data-testid="video-input-unavailable">
            No capture layer is wired into this process — no UVC HDMI capture device is attached (
            {capture.state.detail}). The honest state for a machine without the dongle.
          </p>
        ) : (
          <div role="alert" className="error" data-testid="video-input-error">
            <p>
              The capture read answered HTTP {capture.state.status}: {capture.state.detail}
            </p>
            <button type="button" onClick={capture.refetch}>
              Try again
            </button>
          </div>
        )
      ) : (
        <VideoInputFacts capture={capture.state.body} />
      )}
      <p className="muted" data-testid="adr0082-window">
        The ADR-0082 active-picture window (the region of the frame that holds the game's textbox)
        is reported per frame in the developer trace only: there is no stable endpoint that carries
        it here, so this section does not guess one.
      </p>
    </section>
  );
}

function VideoInputFacts({ capture }: { capture: Awaited<ReturnType<typeof loadCapture>> }) {
  if (capture.devices.length === 0) {
    return (
      <p className="muted" data-testid="video-input-none">
        No capture devices found.
      </p>
    );
  }
  return (
    <div className="stack">
      {capture.synthetic ? (
        <p className="muted" data-testid="video-input-synthetic">
          Synthetic enumeration (test seam): {capture.note ?? "labelled synthetic"} — a hardware
          answer would not say this.
        </p>
      ) : null}
      <ul>
        {capture.devices.map((device) => (
          <li key={device.id} data-testid="video-input-device">
            <strong>{device.name}</strong> (<code>{device.id}</code>) —{" "}
            <span
              className={`badge ${device.state === "ready" ? "badge-succeeded" : "badge-failed"}`}
            >
              {device.state}
            </span>
            {device.chosen
              ? ` · negotiated mode ${device.chosen.w}×${device.chosen.h} at ${device.chosen.fps} fps (${device.chosen.pix_fmt}, ${device.chosen.source})`
              : ""}
            {device.link ? ` · USB ${device.link.speed_mbps} Mbps` : ""}
            {device.detail ? <span className="muted"> — {device.detail}</span> : null}
          </li>
        ))}
      </ul>
    </div>
  );
}
