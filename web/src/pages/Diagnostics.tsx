import { useQuery } from "@tanstack/react-query";
import { useCallback, useEffect, useState, type ReactNode } from "react";

import { playTestTone, type AudioDevicesView } from "../api/audio";
import {
  formatBytes,
  GatedError,
  gatedJson,
  MODEL_ROLES,
  type DiagnosticsRun,
  type GatedState,
  type HostReport,
  type LogRecord,
} from "../api/diagnostics";
import { ApiError, type Readiness } from "../api/client";
import { fetchSystemInfo } from "../api/system";
import { ErrorState } from "../components/states";

/**
 * `/diagnostics` — docs/spec/09: "video input modes, power/environment
 * warnings, CPU/RAM/storage, network and each model role, voice registry
 * replication, speaker test, system logs (redacted)".
 *
 * The page is public (ADR-0016 §3): build facts, the host vitals and the
 * readiness checks work before an account exists. The sections the scope
 * table gates (video input, the model-role probes, the voice registry, the
 * speaker test, the log tail) read the same API as signed-in users would and
 * render the answer they get: 401 becomes a "sign in" hint, 403 a "this
 * needs a higher role" hint, 503 the subsystem's own reason — never a
 * fabricated row. Every section is backed by an endpoint; the ones the
 * build cannot answer say so (the ADR-0082 picture window, USB error
 * counters) instead of drawing an empty table that reads as "fine".
 */

const JOB_STATES = ["queued", "running", "succeeded", "failed", "cancelled"] as const;

interface JobSummaryRow {
  id: string;
  kind: string;
  state: string;
  created_at?: string;
}

// ── the gated reads ─────────────────────────────────────────────────────────

/** Map a GatedError onto the page's renderable states. */
function mapGated<T>(error: unknown): GatedState<T> {
  if (error instanceof GatedError) {
    if (error.status === 401) return { kind: "unauthenticated" };
    if (error.status === 403) return { kind: "forbidden" };
    return { kind: "error", status: error.status, detail: error.detail };
  }
  return {
    kind: "error",
    status: 0,
    detail: error instanceof Error ? error.message : "network error",
  };
}

/**
 * One of the page's gated reads, as a load/retry state. `load` must be
 * stable (a module-level function) so the effect does not re-run on every
 * render; `refetch` re-runs it. Aborts on unmount and on re-run, so a
 * section the operator left is not still fetching.
 */
function useGated<T>(
  load: (signal: AbortSignal) => Promise<T>,
  enabled: boolean,
): { state: GatedState<T>; refetch: () => void } {
  const [nonce, setNonce] = useState(0);
  const [state, setState] = useState<GatedState<T>>({ kind: "loading" });

  useEffect(() => {
    if (!enabled) return;
    const controller = new AbortController();
    let cancelled = false;
    setState({ kind: "loading" });
    load(controller.signal)
      .then((body) => {
        if (!cancelled) setState({ kind: "ok", body });
      })
      .catch((error) => {
        if (!cancelled && !controller.signal.aborted) setState(mapGated(error));
      });
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [load, enabled, nonce]);

  const refetch = useCallback(() => {
    setNonce((n) => n + 1);
  }, []);
  return { state, refetch };
}

/** The sign-in / role hint a gated section renders for 401 and 403. */
function GatedHint({
  state,
  action,
  refetch,
  needs,
}: {
  state: GatedState<unknown>;
  action: string;
  refetch: () => void;
  needs?: string;
}) {
  if (state.kind === "unauthenticated") {
    return (
      <p className="muted" data-testid={`${action}-sign-in`}>
        {action} needs a signed-in session. <a href="/login">Sign in</a> to see this section.
      </p>
    );
  }
  if (state.kind === "forbidden") {
    return (
      <p className="muted" data-testid={`${action}-role`}>
        {action} is available to the {needs ?? "operator or higher"} role. Sign in with a matching
        account.
      </p>
    );
  }
  if (state.kind === "error") {
    return (
      <div role="alert" className="error" data-testid={`${action}-error`}>
        <p>
          {action}: {state.detail} (HTTP {state.status})
        </p>
        <button type="button" onClick={refetch}>
          Try again
        </button>
      </div>
    );
  }
  return <p className="muted">{action}…</p>;
}

// ── section loaders (module-level: stable references for useGated) ─────────

const loadHost = (signal: AbortSignal) =>
  gatedJson<HostReport>("/api/v1/diagnostics/host", undefined, signal);

const loadCapture = (signal: AbortSignal) =>
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

const loadAudio = (signal: AbortSignal) =>
  gatedJson<AudioDevicesView>("/api/v1/audio/devices", undefined, signal);

const loadVoices = (signal: AbortSignal) =>
  gatedJson<{
    voices: {
      id: string;
      character: string;
      revision: number;
      state: string;
      registrations?: {
        instance_id: string;
        registered_name: string;
        state: string;
        err?: string;
        last_verified_at?: string;
      }[];
    }[];
  }>("/api/v1/voices", undefined, signal);

const loadLogs = (signal: AbortSignal) =>
  gatedJson<{ limit: number; count: number; records: LogRecord[]; note: string }>(
    "/api/v1/diagnostics/logs?limit=200",
    undefined,
    signal,
  );

const startProbeRun = (signal?: AbortSignal) =>
  gatedJson<{ run_id: string }>(
    "/api/v1/diagnostics/run",
    {
      method: "POST",
      body: { roles: [...MODEL_ROLES] },
    },
    signal,
  );

const loadRun = (id: string, signal?: AbortSignal) =>
  gatedJson<DiagnosticsRun>(`/api/v1/diagnostics/${encodeURIComponent(id)}`, undefined, signal);

// ── the page ────────────────────────────────────────────────────────────────

export function DiagnosticsPage() {
  const system = useQuery({
    queryKey: ["system", "info"],
    queryFn: ({ signal }) => fetchSystemInfo(signal),
  });

  let body: ReactNode;
  if (system.error) {
    body = (
      <ErrorState
        error={system.error}
        action="Diagnostics unavailable"
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
        <HostSection />
        <VideoInputSection />
        <ModelRolesSection />
        <VoiceRegistrySection />
        <SpeakerTestSection />
        <SystemLogsSection />
        <ReadinessSection />
        <JobQueueSection />
      </>
    );
  }

  return (
    <main data-testid="diagnostics-page">
      <h1>Diagnostics</h1>
      {body}
    </main>
  );
}

// ── host: CPU, RAM, storage, power/environment ──────────────────────────────

function HostSection() {
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
              ? `${formatBytes(report.storage.free_bytes)} free of ${formatBytes(report.storage.total_bytes)}; asset store ${formatBytes(report.storage.asset_store_bytes)} (data directory: ${report.storage.data_dir || "unknown name"})`
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

function VideoInputSection() {
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

// ── model roles ─────────────────────────────────────────────────────────────

const PROBE_POLL_MS = 1_000;
const PROBE_MAX_POLLS = 300;

function ModelRolesSection() {
  const [runId, setRunId] = useState<string | null>(null);
  const [run, setRun] = useState<GatedState<DiagnosticsRun>>({ kind: "loading" });
  const [startState, setStartState] = useState<GatedState<{ run_id: string }>>({
    kind: "loading",
  });

  // Poll the run while it is running; stop on done, 404 (evicted) or an error.
  useEffect(() => {
    if (!runId) return;
    const controller = new AbortController();
    let timer: number | undefined;
    let cancelled = false;
    let attempt = 0;
    const tick = () => {
      loadRun(runId, controller.signal)
        .then((body) => {
          if (cancelled) return;
          setRun({ kind: "ok", body });
          if (body.status === "running") {
            attempt += 1;
            if (attempt < PROBE_MAX_POLLS) {
              timer = window.setTimeout(tick, PROBE_POLL_MS);
            } else {
              setRun({
                kind: "error",
                status: 0,
                detail: "the run is still running after ten minutes of polling",
              });
            }
          }
        })
        .catch((error) => {
          if (cancelled || controller.signal.aborted) return;
          if (error instanceof GatedError && error.status === 404) {
            setRun({
              kind: "error",
              status: 404,
              detail: "the run expired from the bounded store (32 newest, one-hour TTL)",
            });
            return;
          }
          setRun(mapGated(error));
        });
    };
    tick();
    return () => {
      cancelled = true;
      controller.abort();
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [runId]);

  const probeAll = () => {
    setStartState({ kind: "loading" });
    startProbeRun(undefined)
      .then((ref) => {
        setStartState({ kind: "ok", body: ref });
        setRun({ kind: "loading" });
        setRunId(ref.run_id);
      })
      .catch((error) => {
        setStartState(mapGated(error));
      });
  };

  const roles = (run.kind === "ok" ? run.body.roles : []) ?? [];
  const requested = (run.kind === "ok" ? run.body.requested : []) ?? [];

  return (
    <section className="panel" data-testid="model-roles">
      <h2>Model roles</h2>
      <p className="muted">
        Each row is the doctor&apos;s own <code>providers.&lt;role&gt;</code> check run on the
        server (the same probe <code>game-narrator doctor</code> runs): reachability, the measured
        answer latency, and the status. A role with no model answers NOT CONFIGURED — an answer, not
        an error.
      </p>
      <button type="button" onClick={probeAll} data-testid="model-roles-probe-all">
        Probe all roles
      </button>
      {startState.kind === "unauthenticated" ? (
        <p className="muted" data-testid="model-roles-sign-in">
          Probing the roles needs a signed-in session (operator role or higher).{" "}
          <a href="/login">Sign in</a> to run the probes.
        </p>
      ) : null}
      {startState.kind === "forbidden" ? (
        <p className="muted" data-testid="model-roles-role">
          Probing the roles needs the operator role or higher; this session does not hold it.
        </p>
      ) : null}
      {startState.kind === "error" ? (
        <div role="alert" className="error" data-testid="model-roles-error">
          <p>
            The probe did not start (HTTP {startState.status}): {startState.detail}
          </p>
        </div>
      ) : null}
      {runId ? (
        <>
          {run.kind === "ok" ? (
            <>
              <p className="muted" data-testid="model-roles-run-id">
                Run {run.body.id} —{" "}
                {run.body.status === "done"
                  ? `finished ${run.body.finished_at ? new Date(run.body.finished_at).toLocaleTimeString() : ""}`
                  : "running"}
                .
              </p>
              <table>
                <thead>
                  <tr>
                    <th>Role</th>
                    <th>Configured</th>
                    <th>Status</th>
                    <th>Latency</th>
                    <th>Detail</th>
                  </tr>
                </thead>
                <tbody>
                  {MODEL_ROLES.map((role) => {
                    const row = roles.find((candidate) => candidate.role === role);
                    return (
                      <tr key={role} data-testid={`model-role-row-${role}`}>
                        <td>
                          <code>{role}</code>
                        </td>
                        <td>
                          {row
                            ? row.configured
                              ? "yes"
                              : "no"
                            : requested.includes(role)
                              ? "…"
                              : "—"}
                        </td>
                        <td>
                          {row ? (
                            <span
                              className={`badge ${row.status === "OK" ? "badge-succeeded" : "badge-failed"}`}
                            >
                              {row.status}
                            </span>
                          ) : requested.includes(role) ? (
                            "…"
                          ) : (
                            "—"
                          )}
                        </td>
                        <td>{row ? `${row.duration_ms} ms` : ""}</td>
                        <td className="muted">{row?.summary ?? ""}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </>
          ) : run.kind === "loading" ? (
            <p className="muted" data-testid="model-roles-loading">
              Probing — the doctor's checks run server-side…
            </p>
          ) : run.kind === "error" ? (
            <div role="alert" className="error" data-testid="model-roles-run-error">
              <p>
                The run could not be read (HTTP {run.status}): {run.detail}
              </p>
            </div>
          ) : (
            <GatedHint state={run} action="Reading the probe run" refetch={() => undefined} />
          )}
        </>
      ) : (
        <p className="muted" data-testid="model-roles-idle">
          No probe has run on this page visit yet.
        </p>
      )}
    </section>
  );
}

// ── voice registry ──────────────────────────────────────────────────────────

function VoiceRegistrySection() {
  const voices = useGated(loadVoices, true);

  return (
    <section className="panel" data-testid="voice-registry">
      <h2>Voice registry</h2>
      <p className="muted" data-testid="voice-registry-single-node">
        This build is a single node: there is no peer replication to verify. The registry below is
        this appliance&apos;s voice registry, with each voice&apos;s registration state on the
        provider it was registered with.
      </p>
      {voices.state.kind === "loading" ? (
        <p className="muted">Reading the voice registry…</p>
      ) : voices.state.kind === "unauthenticated" ? (
        <p className="muted" data-testid="voice-registry-sign-in">
          The voice registry needs a signed-in session (viewer role or higher).{" "}
          <a href="/login">Sign in</a> to see it.
        </p>
      ) : voices.state.kind === "forbidden" ? (
        <p className="muted" data-testid="voice-registry-role">
          Reading the voice registry needs at least the viewer role.
        </p>
      ) : voices.state.kind === "error" ? (
        <div role="alert" className="error" data-testid="voice-registry-error">
          <p>
            The registry read answered HTTP {voices.state.status}: {voices.state.detail}
          </p>
          <button type="button" onClick={voices.refetch}>
            Try again
          </button>
        </div>
      ) : (
        <VoiceRegistryFacts voices={voices.state.body.voices} />
      )}
    </section>
  );
}

function VoiceRegistryFacts({
  voices,
}: {
  voices: Awaited<ReturnType<typeof loadVoices>>["voices"];
}) {
  if (voices.length === 0) {
    return (
      <p className="muted" data-testid="voice-registry-empty">
        No voices exist yet — the registry is empty, which is the honest state before the first
        audition.
      </p>
    );
  }
  return (
    <div className="stack">
      <p data-testid="voice-registry-count">
        {voices.length} voice{voices.length === 1 ? "" : "s"} in the registry.
      </p>
      <table>
        <thead>
          <tr>
            <th>Character</th>
            <th>Revision</th>
            <th>State</th>
            <th>Registrations</th>
          </tr>
        </thead>
        <tbody>
          {voices.map((voice) => (
            <tr key={voice.id} data-testid="voice-row">
              <td>{voice.character}</td>
              <td>r{voice.revision}</td>
              <td>
                <span
                  className={`badge ${voice.state === "approved" ? "badge-succeeded" : "badge-plain"}`}
                >
                  {voice.state}
                </span>
              </td>
              <td>
                {(voice.registrations ?? []).length === 0
                  ? "not registered"
                  : (voice.registrations ?? [])
                      .map(
                        (registration) =>
                          `${registration.state}${registration.err ? ` (${registration.err})` : ""}`,
                      )
                      .join(" · ")}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ── speaker test ────────────────────────────────────────────────────────────

function SpeakerTestSection() {
  const audio = useGated(loadAudio, true);
  const [tone, setTone] = useState<Awaited<ReturnType<typeof playTestTone>> | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<GatedError | null>(null);

  const play = () => {
    setBusy(true);
    setError(null);
    setTone(null);
    playTestTone()
      .then((response) => {
        setTone(response);
      })
      .catch((caught) => {
        setError(
          caught instanceof ApiError
            ? new GatedError(caught.status, `${caught.title} — ${caught.detail}`)
            : new GatedError(0, String(caught)),
        );
      })
      .finally(() => {
        setBusy(false);
      });
  };

  return (
    <section className="panel" data-testid="speaker-test">
      <h2>Speaker test</h2>
      <p className="muted">
        Plays the deterministic test tone (1.0 s / 440 Hz) on the selected output. Playback is a
        consented action (docs/spec/13): the tone only plays from this button.
      </p>
      {audio.state.kind === "loading" ? (
        <p className="muted">Enumerating the audio outputs…</p>
      ) : audio.state.kind === "unauthenticated" ? (
        <p className="muted" data-testid="speaker-test-sign-in">
          The speaker test needs a signed-in session (operator role or higher).{" "}
          <a href="/login">Sign in</a> to play the tone.
        </p>
      ) : audio.state.kind === "forbidden" ? (
        <p className="muted" data-testid="speaker-test-role">
          Playing the tone needs the operator role or higher; this session does not hold it.
        </p>
      ) : audio.state.kind === "error" ? (
        <div role="alert" className="error" data-testid="speaker-test-error">
          <p>
            The audio read answered HTTP {audio.state.status}: {audio.state.detail}
          </p>
          <button type="button" onClick={audio.refetch}>
            Try again
          </button>
        </div>
      ) : audio.state.body.devices.length === 0 ? (
        <p className="muted" data-testid="speaker-test-none">
          No audio outputs found — the tone cannot play on this machine.
        </p>
      ) : (
        <button type="button" onClick={play} disabled={busy} data-testid="speaker-test-play">
          {busy ? "Playing…" : "Play the test tone"}
        </button>
      )}
      {error ? (
        <div role="alert" className="error" data-testid="speaker-test-failed">
          <p>
            The test tone failed (HTTP {error.status}): {error.message}
          </p>
        </div>
      ) : null}
      {tone ? (
        <p data-testid="speaker-test-result">
          Played on <strong>{tone.device}</strong> via sink <code>{tone.sink}</code> for{" "}
          {tone.duration_ms} ms.
          {tone.evidence_file ? ` Recorded evidence: ${tone.evidence_file}.` : ""}
          {tone.note ? ` ${tone.note}` : ""}
        </p>
      ) : null}
    </section>
  );
}

// ── system logs ─────────────────────────────────────────────────────────────

function SystemLogsSection() {
  const logs = useGated(loadLogs, true);

  return (
    <section className="panel" data-testid="system-logs">
      <h2>System logs</h2>
      <p className="muted">
        The newest records of the process log, every value passed the support-bundle redactor (URLs,
        tokens and private IPs replaced) before it left the process. The tail is process-local: a
        restart clears it, and the durable redacted path is the support bundle.
      </p>
      {logs.state.kind === "loading" ? (
        <p className="muted">Reading the log tail…</p>
      ) : logs.state.kind === "unauthenticated" ? (
        <p className="muted" data-testid="system-logs-sign-in">
          The log tail needs a signed-in session. <a href="/login">Sign in</a> to read it.
        </p>
      ) : logs.state.kind === "forbidden" ? (
        <p className="muted" data-testid="system-logs-developer">
          The log tail is developer tier: system logs can carry provider request detail, so the
          scope table grants it to the developer role only.
        </p>
      ) : logs.state.kind === "error" ? (
        <div role="alert" className="error" data-testid="system-logs-error">
          <p>
            The log read answered HTTP {logs.state.status}: {logs.state.detail}
          </p>
          <button type="button" onClick={logs.refetch}>
            Try again
          </button>
        </div>
      ) : (
        <LogsTable logs={logs.state.body} />
      )}
    </section>
  );
}

function LogsTable({
  logs,
}: {
  logs: { limit: number; count: number; records: LogRecord[]; note: string };
}) {
  if (logs.count === 0) {
    return (
      <p className="muted" data-testid="system-logs-empty">
        No records in the ring yet — the process has not logged anything since it booted (or the
        ring rolled over).
      </p>
    );
  }
  const newest = logs.records.slice(-20).reverse();
  return (
    <div className="stack">
      <p className="muted">
        {logs.count} record{logs.count === 1 ? "" : "s"} in the ring; showing the newest{" "}
        {newest.length}. {logs.note}
      </p>
      <table>
        <thead>
          <tr>
            <th>Time</th>
            <th>Level</th>
            <th>Message</th>
          </tr>
        </thead>
        <tbody>
          {newest.map((record, i) => (
            <tr key={`${record.time}-${i}`} data-testid="log-row">
              <td>{new Date(record.time).toLocaleTimeString()}</td>
              <td>{record.level}</td>
              <td>
                {record.msg}
                {Object.entries(record.attrs ?? {})
                  .map(([key, value]) => `${key}=${value}`)
                  .join(" ")}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ── readiness checks and the job queue (the pre-existing sections) ─────────

function ReadinessSection() {
  const readiness = useQuery({
    queryKey: ["health", "ready"],
    queryFn: async ({ signal }) => {
      const response = await fetch("/api/v1/health/ready", { credentials: "same-origin", signal });
      if (!response.ok) throw new Error(`the readiness probe answered ${response.status}`);
      return (await response.json()) as Readiness;
    },
    refetchInterval: 10_000,
  });

  if (readiness.isError) {
    return (
      <section className="panel" data-testid="readiness-checks">
        <h2>Readiness checks</h2>
        <ErrorState error={readiness.error} action="Cannot read readiness" />
      </section>
    );
  }
  if (!readiness.data) {
    return (
      <section className="panel" data-testid="readiness-checks">
        <h2>Readiness checks</h2>
        <p className="muted">Loading…</p>
      </section>
    );
  }
  const ready = readiness.data;
  const checks = Object.entries(ready.checks ?? {});
  return (
    <section className="panel" data-testid="readiness-checks">
      <h2>
        Readiness checks{" "}
        <span className="muted" data-testid="readiness">
          {ready.status}
        </span>
      </h2>
      {checks.length === 0 ? (
        <p className="muted">
          No dependency has probed yet: the check map is empty, which the API reports as
          &ldquo;nothing is wired&rdquo; rather than &ldquo;everything works&rdquo;.
        </p>
      ) : (
        <table className="checks">
          <thead>
            <tr>
              <th>Dependency</th>
              <th>Status</th>
              <th>Detail</th>
            </tr>
          </thead>
          <tbody>
            {checks.map(([name, detail]) => (
              <CheckRow key={name} name={name} detail={detail} />
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function CheckRow({ name, detail }: { name: string; detail: Record<string, unknown> }) {
  const status = typeof detail.status === "string" ? detail.status : "unprobed";
  const extras = Object.entries(detail).filter(([key]) => key !== "status");
  return (
    <tr className={`check-row check-${status}`} data-testid={`readiness-check-${name}`}>
      <td>{name}</td>
      <td>
        <span className={`badge ${status === "ok" ? "badge-succeeded" : "badge-failed"}`}>
          {status}
        </span>
      </td>
      <td>
        {extras.length === 0
          ? "—"
          : extras
              .map(
                ([key, value]) =>
                  `${key}: ${typeof value === "string" ? value : JSON.stringify(value)}`,
              )
              .join(" · ")}
      </td>
    </tr>
  );
}

function JobQueueSection() {
  const jobs = useQuery({
    queryKey: ["diagnostics", "jobs"],
    queryFn: async ({ signal }) => {
      const page = (await fetch("/api/v1/jobs?limit=200", {
        credentials: "same-origin",
        signal,
      })) as Response;
      if (page.status === 401) return { unauthenticated: true } as const;
      if (!page.ok) throw new Error(`the job queue answered ${page.status}`);
      const body = (await page.json()) as { jobs: JobSummaryRow[] };
      return { jobs: body.jobs ?? [] } as const;
    },
    retry: false,
  });

  const rows = "jobs" in (jobs.data ?? {}) ? (jobs.data as { jobs: JobSummaryRow[] }).jobs : [];
  const counts = new Map<string, number>();
  for (const row of rows) counts.set(row.state, (counts.get(row.state) ?? 0) + 1);
  return (
    <section className="panel" data-testid="job-queue">
      <h2>Job queue</h2>
      {jobs.data && "unauthenticated" in jobs.data ? (
        <p className="muted" data-testid="jobs-unauthenticated">
          The queue needs a signed-in session; sign in to see its state.
        </p>
      ) : jobs.isError ? (
        <p className="error" role="alert">
          Cannot read the job queue:{" "}
          {jobs.error instanceof Error ? jobs.error.message : "network error"}
        </p>
      ) : rows.length === 0 ? (
        <p className="muted" data-testid="jobs-empty">
          No jobs recorded.
        </p>
      ) : (
        <>
          <p data-testid="jobs-summary">
            {JOB_STATES.filter((state) => counts.has(state)).map((state) => (
              <span key={state} className="badge badge-plain" data-testid={`jobs-count-${state}`}>
                {state}: {counts.get(state)}
              </span>
            ))}
          </p>
          <table className="jobs">
            <thead>
              <tr>
                <th>Job</th>
                <th>Kind</th>
                <th>State</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr key={row.id} data-testid="job-row">
                  <td>
                    <code>{row.id}</code>
                  </td>
                  <td>{row.kind}</td>
                  <td>{row.state}</td>
                  <td>{row.created_at ?? "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </section>
  );
}
