import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Navigate, useNavigate, useSearchParams } from "react-router";

import { ApiError, apiCode, apiStatus, authApi } from "../api/client";
import { fetchAudioDevices, playTestTone, saveAudioSettings } from "../api/audio";
import {
  fetchDiagnosticsHost,
  fetchDiagnosticsRun,
  formatBytes,
  MODEL_ROLES,
  startDiagnosticsRun,
  type DiagnosticsRole,
} from "../api/diagnostics";
import { fetchCaptureDevices } from "../api/capture";
import { fetchSettings } from "../api/settings";
import { useAuth } from "../auth/auth-context";
import { ErrorState } from "../components/states";

/**
 * `/setup` — the first-run wizard (docs/spec/09 "first-run wizard: detect
 * device, configure auth/storage/gateway roles", ADR-0087).
 *
 * Before an admin exists, the page is exactly what it always was: the claim
 * form, loopback-only, one-time, no session minted (ADR-0016 §7). Once an
 * admin is signed in, the claim step is done and the page becomes the
 * wizard — six steps, each of which reads its own live state on entry (the
 * wizard never caches a step's facts across entries, because the machine can
 * change between steps): devices, storage, models, audio, privacy, done.
 *
 * The wizard stores nothing: resuming means the `?step=` in the URL, and the
 * done summary is re-derived from the same live reads, so a step the operator
 * skipped is marked NOT CONFIGURED (or NOT AVAILABLE) rather than reported as
 * done. A step the API cannot answer (no capture layer, no audio backend) is
 * the honest state, not an error the page must swallow.
 */

export function SetupPage() {
  const auth = useAuth();

  switch (auth.state) {
    case "checking":
      return <p className="muted">Checking this appliance…</p>;
    case "error":
      return (
        <ErrorState
          error={auth.error}
          action="Cannot read this appliance's setup state"
          onRetry={() => {
            void auth.reload();
          }}
        />
      );
    case "setup":
      return <ClaimForm />;
    case "anonymous":
      // Claimed appliance, no session: sign in. (route-coverage's step 4
      // asserts this redirect — a signed-out visitor to /setup lands on
      // /login, not on a wizard they cannot finish.)
      return <Navigate to="/login" replace />;
    case "authenticated":
      if (auth.role !== "admin") return <NotAdminNotice />;
      return <Wizard />;
  }
}

/**
 * The claim form — the step the appliance cannot be used without. Three facts
 * it has to respect, all from ADR-0016 §7: setup is loopback-only (a remote
 * 403 is a real answer, not a retry), it creates an account but mints **no**
 * session (so the wizard must log in afterwards, which proves the password
 * was known), and once an admin exists it is 409 forever.
 */
function ClaimForm() {
  const status = useQuery({
    queryKey: ["auth", "status"],
    queryFn: ({ signal }) => authApi.status(signal),
    retry: false,
  });
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<ApiError | null>(null);
  const [busy, setBusy] = useState(false);
  const navigate = useNavigate();

  if (status.isPending) return <p className="muted">Checking this appliance…</p>;
  if (status.isError) return <ErrorState error={status.error} action="Cannot read setup state" />;
  if (!status.data.setup_required) return <Navigate to="/login" replace />;

  const onSubmit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await authApi.setup(username, password);
      // Deliberately no session: sign in like anybody else (ADR-0016 §7).
      navigate("/login", { replace: true });
    } catch (caught) {
      setError(
        caught instanceof ApiError
          ? caught
          : new ApiError({
              status: 0,
              code: "network",
              title: "Setup failed",
              detail: String(caught),
            }),
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <main>
      <h1>Claim this appliance</h1>
      <p className="muted">
        No administrator exists yet, so every privileged route refuses. The first account created
        here becomes the appliance&apos;s admin — this must be done on the machine itself, and it
        can only be done once.
      </p>
      {!status.data.loopback_setup ? (
        <p role="alert" className="error" data-testid="setup-not-local">
          This request did not come from the appliance itself. Open{" "}
          <code>http://127.0.0.1:8080/setup</code> on the machine, or run{" "}
          <code>game-narrator admin create</code> there.
        </p>
      ) : null}
      {error ? (
        <ErrorState
          error={error}
          action="Setup refused"
          onRetry={() => {
            void status.refetch();
          }}
        />
      ) : null}
      <form onSubmit={(event) => void onSubmit(event)} className="stack" data-testid="setup-form">
        <label>
          Username
          <input
            value={username}
            onChange={(event) => setUsername(event.target.value)}
            autoComplete="username"
            required
            minLength={3}
            data-testid="setup-username"
          />
        </label>
        <label>
          Password
          <input
            type="password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            autoComplete="new-password"
            required
            minLength={12}
            data-testid="setup-password"
          />
          <span className="muted">
            At least 12 characters. Passwords are stored as PBKDF2 digests.
          </span>
        </label>
        <button
          type="submit"
          disabled={busy || !status.data.loopback_setup}
          data-testid="setup-submit"
        >
          {busy ? "Creating…" : "Create admin and continue"}
        </button>
      </form>
    </main>
  );
}

/** Signed in, but not the role the wizard writes for. */
function NotAdminNotice() {
  const auth = useAuth();
  return (
    <main>
      <h1>Setup wizard</h1>
      <p className="error" role="alert" data-testid="setup-not-admin">
        The setup wizard runs as the admin role. You are signed in as{" "}
        {auth.user?.username ?? "an account"} (role {auth.role}) — ask an admin, or sign in as one,
        to run the wizard.
      </p>
    </main>
  );
}

// ── the wizard ──────────────────────────────────────────────────────────────

interface StepDef {
  id: string;
  label: string;
  blurb: string;
}

const STEPS: StepDef[] = [
  {
    id: "devices",
    label: "Devices",
    blurb: "The capture device and the audio outputs, exactly as the machine reports them.",
  },
  {
    id: "storage",
    label: "Storage",
    blurb: "The data directory, the free space on it, and the asset store's share.",
  },
  {
    id: "models",
    label: "Models",
    blurb: "The gateway and each model role's configuration — with a live probe per role.",
  },
  {
    id: "audio",
    label: "Audio",
    blurb: "Pick the output device and prove it plays the test tone.",
  },
  {
    id: "privacy",
    label: "Privacy",
    blurb: "Where the data lives, what leaves the machine, and how the logs leave it.",
  },
  {
    id: "done",
    label: "Done",
    blurb: "The summary, re-read from the same live sources — skipped steps say so.",
  },
];

const privacyConfirmedKey = ["setup", "privacy-confirmed"] as const;

// ── the per-role live probe (the wizard's one action that costs a model call)

/** One role's probe result, as the doctor's own row carries it. */
type ProbeOutcome = {
  status: DiagnosticsRole["status"];
  summary?: string;
  duration_ms: number;
};
type ProbeState =
  | { running: true }
  | { running: false; result: ProbeOutcome | null; error?: string };

const PROBE_POLL_MS = 1_000;
const PROBE_MAX_POLLS = 300; // ten minutes of polling is the outer wall

/**
 * The per-role live test. The probe runs the doctor's own `providers.<role>`
 * check server-side (ADR-0087 — reuse, never a second probe spelling): POST
 * the run, poll its shape until done, and render the doctor's row: status,
 * summary, the measured latency. The state lives on the wizard (not the step)
 * so a role tested on the Models step is still reported on the Done summary;
 * the timers stop when the wizard unmounts.
 */
function useProbes() {
  const [probes, setProbes] = useState<Record<string, ProbeState>>({});
  const timersRef = useRef<Record<string, number>>({});
  const stoppedRef = useRef(new Set<string>());

  useEffect(() => {
    const timers = timersRef.current;
    const stopped = stoppedRef.current;
    return () => {
      for (const [role, id] of Object.entries(timers)) {
        window.clearTimeout(id);
        stopped.add(role);
      }
    };
  }, []);

  const test = useCallback((role: string) => {
    if (timersRef.current[role] !== undefined) {
      window.clearTimeout(timersRef.current[role]);
    }
    stoppedRef.current.delete(role);
    setProbes((current) => ({ ...current, [role]: { running: true } }));

    const finish = (patch: ProbeState) => {
      setProbes((current) => ({ ...current, [role]: patch }));
    };

    const tick = (runId: string, attempt: number) => {
      if (stoppedRef.current.has(role)) return;
      fetchDiagnosticsRun(runId)
        .then((run) => {
          if (stoppedRef.current.has(role)) return;
          if (run.status === "done") {
            const row = run.roles.find((candidate) => candidate.role === role);
            finish(
              row
                ? {
                    running: false,
                    result: {
                      status: row.status,
                      summary: row.summary,
                      duration_ms: row.duration_ms,
                    },
                  }
                : {
                    running: false,
                    result: null,
                    error:
                      "the run finished without this role's row (the bounded store may have evicted it)",
                  },
            );
            return;
          }
          if (attempt >= PROBE_MAX_POLLS) {
            finish({
              running: false,
              result: null,
              error: "the probe is still running after ten minutes of polling",
            });
            return;
          }
          timersRef.current[role] = window.setTimeout(
            () => tick(runId, attempt + 1),
            PROBE_POLL_MS,
          );
        })
        .catch((error) => {
          if (stoppedRef.current.has(role)) return;
          finish({
            running: false,
            result: null,
            error:
              error instanceof ApiError && error.status === 404
                ? "the run expired from the bounded store before it was read back"
                : error instanceof Error
                  ? error.message
                  : "the probe failed to run",
          });
        });
    };

    startDiagnosticsRun([role])
      .then((ref) => {
        tick(ref.run_id, 0);
      })
      .catch((error) => {
        if (stoppedRef.current.has(role)) return;
        finish({
          running: false,
          result: null,
          error:
            error instanceof ApiError
              ? `${error.title} — ${error.detail}`
              : "the probe failed to start",
        });
      });
  }, []);

  return { probes, testProbe: test };
}

function Wizard() {
  const [searchParams, setSearchParams] = useSearchParams();
  const { probes, testProbe } = useProbes();
  const stepId = STEPS.some((step) => step.id === searchParams.get("step"))
    ? (searchParams.get("step") as string)
    : "devices";
  const index = STEPS.findIndex((step) => step.id === stepId);

  const goto = (id: string, replace = false) => {
    const next = new URLSearchParams(searchParams);
    next.set("step", id);
    setSearchParams(next, { replace });
  };

  const active = STEPS[index];

  return (
    <main data-testid="setup-wizard">
      <h1>Setup wizard</h1>
      <p className="muted">
        Six steps. Each one re-reads its own live state when you enter it — the wizard stores
        nothing, so the &ldquo;done&rdquo; summary is the same read, and a step you skip is marked
        NOT CONFIGURED rather than reported as finished.
      </p>
      <nav aria-label="Wizard steps" data-testid="setup-stepnav">
        <ol>
          {STEPS.map((step, i) => (
            <li key={step.id}>
              <button
                type="button"
                onClick={() => {
                  goto(step.id);
                }}
                aria-current={step.id === stepId ? "step" : undefined}
                data-testid={`setup-nav-${step.id}`}
              >
                {i + 1}. {step.label}
              </button>
            </li>
          ))}
        </ol>
      </nav>
      <section aria-labelledby="setup-step-heading" data-testid={`setup-step-${stepId}`}>
        <h2 id="setup-step-heading">
          {index + 1}. {active.label}
        </h2>
        <p className="muted">{active.blurb}</p>
        {stepId === "devices" ? <DevicesStep /> : null}
        {stepId === "storage" ? <StorageStep /> : null}
        {stepId === "models" ? <ModelsStep probes={probes} testProbe={testProbe} /> : null}
        {stepId === "audio" ? <AudioStep /> : null}
        {stepId === "privacy" ? (
          <PrivacyStep
            onFinished={() => {
              goto("done", true);
            }}
          />
        ) : null}
        {stepId === "done" ? <DoneStep probes={probes} /> : null}
      </section>
      <p className="stack">
        {index > 0 ? (
          <button
            type="button"
            onClick={() => {
              goto(STEPS[index - 1].id, true);
            }}
            data-testid="setup-prev"
          >
            ← {STEPS[index - 1].label}
          </button>
        ) : null}
        {index < STEPS.length - 1 && stepId !== "privacy" ? (
          <button
            type="button"
            onClick={() => {
              goto(STEPS[index + 1].id, true);
            }}
            data-testid="setup-next"
          >
            {STEPS[index + 1].label} →
          </button>
        ) : null}
        {stepId === "done" ? (
          <a href="/health" data-testid="setup-to-diagnostics">
            Open the health page →
          </a>
        ) : null}
      </p>
    </main>
  );
}

/** Step 1 — the capture devices and the audio outputs, each as the machine reports it. */
function DevicesStep() {
  const capture = useQuery({
    queryKey: ["setup", "capture"],
    queryFn: ({ signal }) => fetchCaptureDevices(signal),
    retry: false,
  });
  const audio = useQuery({
    queryKey: ["setup", "audio"],
    queryFn: ({ signal }) => fetchAudioDevices(signal),
    retry: false,
  });

  return (
    <div className="stack">
      <section aria-label="Capture device">
        <h3>Capture</h3>
        {capture.isPending ? (
          <p className="muted">Enumerating the capture devices…</p>
        ) : capture.isError ? (
          apiStatus(capture.error) === 503 ? (
            <p className="muted" data-testid="setup-capture-unavailable">
              No capture layer is wired into this process — no UVC HDMI capture device is attached
              (code {apiCode(capture.error) ?? "unknown"}). A session can still run from replay or
              test-pattern sources; the capture step is NOT CONFIGURED, which is the honest state
              for a machine without the dongle.
            </p>
          ) : (
            <ErrorState error={capture.error} action="Cannot read the capture devices" />
          )
        ) : capture.data.devices.length === 0 ? (
          <p className="muted" data-testid="setup-capture-none">
            No capture devices found.
          </p>
        ) : (
          <>
            {capture.data.synthetic ? (
              <p className="muted" data-testid="setup-capture-synthetic">
                Synthetic enumeration (test seam): {capture.data.note ?? "labelled synthetic"} — a
                hardware answer would not say this.
              </p>
            ) : null}
            <ul>
              {capture.data.devices.map((device) => (
                <li key={device.id} data-testid="setup-capture-device">
                  <strong>{device.name}</strong>
                  <code>{device.id}</code>
                  <span
                    className={`badge ${device.state === "ready" ? "badge-succeeded" : "badge-failed"}`}
                  >
                    {device.state}
                  </span>
                  {device.chosen
                    ? `, mode ${device.chosen.w}×${device.chosen.h} at ${device.chosen.fps} fps (${device.chosen.pix_fmt}, ${device.chosen.source})`
                    : ""}
                  {device.detail ? <span className="muted"> — {device.detail}</span> : null}
                </li>
              ))}
            </ul>
          </>
        )}
      </section>
      <section aria-label="Audio outputs">
        <h3>Audio outputs</h3>
        {audio.isPending ? (
          <p className="muted">Enumerating the audio outputs…</p>
        ) : audio.isError ? (
          <ErrorState error={audio.error} action="Cannot read the audio devices" />
        ) : (
          <>
            {audio.data.synthetic ? (
              <p className="muted" data-testid="setup-audio-synthetic">
                Synthetic device list (test seam) — the doctor reads this flag and refuses to treat
                it as hardware.
              </p>
            ) : null}
            {audio.data.devices.length === 0 ? (
              <p className="muted" data-testid="setup-audio-none">
                No audio outputs found.{" "}
                {audio.data.note ? (
                  <span data-testid="setup-audio-note">{audio.data.note}</span>
                ) : null}
              </p>
            ) : (
              <>
                <ul>
                  {audio.data.devices.map((device) => (
                    <li key={device.id} data-testid="setup-audio-device">
                      <strong>{device.name}</strong> ({device.kind}
                      {device.sample_rate ? `, ${device.sample_rate} Hz` : ""})
                      {device.selected ? (
                        <span className="badge badge-succeeded">selected</span>
                      ) : null}
                    </li>
                  ))}
                </ul>
                {audio.data.note ? <p className="muted">{audio.data.note}</p> : null}
              </>
            )}
          </>
        )}
      </section>
    </div>
  );
}

/** Step 2 — the storage facts, from the public host report. */
function StorageStep() {
  const host = useQuery({
    queryKey: ["setup", "host"],
    queryFn: ({ signal }) => fetchDiagnosticsHost(signal),
    retry: false,
  });

  if (host.isPending) return <p className="muted">Reading the host…</p>;
  if (host.isError) return <ErrorState error={host.error} action="Cannot read the host report" />;

  const storage = host.data.storage;
  return (
    <dl className="facts" data-testid="setup-storage-facts">
      <div>
        <dt>Data directory</dt>
        <dd>{storage.known ? storage.data_dir : "unknown (the path stays server-side)"}</dd>
      </div>
      <div>
        <dt>Free space</dt>
        <dd data-testid="setup-storage-free">{formatBytes(storage.free_bytes)}</dd>
      </div>
      <div>
        <dt>Filesystem total</dt>
        <dd>{formatBytes(storage.total_bytes)}</dd>
      </div>
      <div>
        <dt>Asset store</dt>
        <dd data-testid="setup-storage-assets">{formatBytes(storage.asset_store_bytes)}</dd>
      </div>
    </dl>
  );
}

// ── step 3: the gateway and the model roles, with a live probe per role ─────

function ModelsStep({
  probes,
  testProbe,
}: {
  probes: Record<string, ProbeState>;
  testProbe: (role: string) => void;
}) {
  const settings = useQuery({
    queryKey: ["settings"],
    queryFn: ({ signal }) => fetchSettings(signal),
    retry: false,
  });

  if (settings.isPending) return <p className="muted">Reading the effective configuration…</p>;
  if (settings.isError)
    return <ErrorState error={settings.error} action="Cannot read the model configuration" />;

  const gateway = settings.data.models.gateway;
  const roles = settings.data.models.roles;

  return (
    <div className="stack">
      <section aria-label="Gateway">
        <h3>Gateway</h3>
        <dl className="facts" data-testid="setup-models-gateway">
          <div>
            <dt>Endpoint</dt>
            <dd>
              {gateway.url_configured ? (
                <>
                  set (in <code>{gateway.url_env}</code>; the value stays server-side)
                </>
              ) : (
                <>
                  not set — set <code>{gateway.url_env}</code>
                </>
              )}
            </dd>
          </div>
          <div>
            <dt>Token</dt>
            <dd>
              {gateway.token_configured ? (
                <>
                  set (in <code>{gateway.token_env_name}</code>; the value never crosses this
                  boundary)
                </>
              ) : (
                <>
                  not set — set <code>{gateway.token_env_name}</code>
                </>
              )}
            </dd>
          </div>
        </dl>
      </section>
      <section aria-label="Model roles">
        <h3>Model roles</h3>
        <table>
          <thead>
            <tr>
              <th>Role</th>
              <th>State</th>
              <th>Model</th>
              <th>Live test</th>
            </tr>
          </thead>
          <tbody>
            {roles.map((row) => {
              const probe = probes[row.role];
              return (
                <tr key={row.role} data-testid={`setup-models-row-${row.role}`}>
                  <td>
                    <code>{row.role}</code>
                  </td>
                  <td>{row.state}</td>
                  <td>{row.configured ? row.model : "—"}</td>
                  <td>
                    <button
                      type="button"
                      onClick={() => {
                        testProbe(row.role);
                      }}
                      disabled={probe?.running === true}
                      data-testid={`setup-models-test-${row.role}`}
                    >
                      {probe?.running === true ? "Probing…" : "Test"}
                    </button>{" "}
                    {probe && !probe.running && probe.result ? (
                      <span
                        className={`badge ${probe.result.status === "OK" ? "badge-succeeded" : "badge-failed"}`}
                        data-testid={`setup-models-result-${row.role}`}
                      >
                        {probe.result.status} · {probe.result.duration_ms} ms
                      </span>
                    ) : null}
                    {probe && !probe.running && probe.error ? (
                      <span className="error" data-testid={`setup-models-result-${row.role}`}>
                        {probe.error}
                      </span>
                    ) : null}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
        <p className="muted">
          &ldquo;Test&rdquo; runs the doctor&apos;s own check for that role on the server (the same
          probe <code>game-narrator doctor</code> runs) and reports its status and the measured
          answer latency. A role with no model answers NOT CONFIGURED — that is an answer, not an
          error.
        </p>
      </section>
    </div>
  );
}

// ── step 4: the output device and the consent-gated test tone ───────────────

function AudioStep() {
  const audio = useQuery({
    queryKey: ["setup", "audio"],
    queryFn: ({ signal }) => fetchAudioDevices(signal),
    retry: false,
  });
  const [tone, setTone] = useState<Awaited<ReturnType<typeof playTestTone>> | null>(null);
  const [toneBusy, setToneBusy] = useState(false);
  const [toneError, setToneError] = useState<ApiError | null>(null);
  const devices = audio.data?.devices ?? [];
  const selectedId = audio.data?.selected.device;

  const select = useMutation({
    mutationFn: (device: string) => saveAudioSettings({ device }),
  });

  const play = () => {
    setToneBusy(true);
    setToneError(null);
    setTone(null);
    playTestTone()
      .then((response) => {
        setTone(response);
      })
      .catch((error) => {
        setToneError(
          error instanceof ApiError
            ? error
            : new ApiError({
                status: 0,
                code: "network",
                title: "Test tone failed",
                detail: String(error),
              }),
        );
      })
      .finally(() => {
        setToneBusy(false);
      });
  };

  if (audio.isPending) return <p className="muted">Enumerating the audio outputs…</p>;
  if (audio.isError)
    return <ErrorState error={audio.error} action="Cannot read the audio devices" />;

  return (
    <div className="stack">
      {audio.data.synthetic ? (
        <p className="muted" data-testid="setup-audio-synthetic">
          Synthetic device list (test seam) — the tone plays the synthetic sink, and the page says
          so.
        </p>
      ) : null}
      {devices.length === 0 ? (
        <p className="muted" data-testid="setup-audio-none">
          No audio outputs found — the test tone cannot play on this machine.{" "}
          {audio.data.note ? audio.data.note : ""}
        </p>
      ) : (
        <>
          <label>
            Output device
            <select
              value={selectedId ?? ""}
              onChange={(event) => {
                select.mutate(event.target.value);
              }}
              data-testid="setup-audio-select"
            >
              {devices.map((device) => (
                <option key={device.id} value={device.id}>
                  {device.name} ({device.kind})
                </option>
              ))}
            </select>
          </label>
          {select.data ? (
            <p className="muted" data-testid="setup-audio-saved">
              Saved: the stored selection is now {select.data.saved.device}.
            </p>
          ) : null}
          {select.isError ? (
            <ErrorState error={select.error} action="Saving the selection failed" />
          ) : null}
          <button type="button" onClick={play} disabled={toneBusy} data-testid="setup-tone-play">
            {toneBusy ? "Playing…" : "Play the test tone (1.0 s / 440 Hz)"}
          </button>
          {toneError ? <ErrorState error={toneError} action="The test tone failed" /> : null}
          {tone ? (
            <p data-testid="setup-tone-result">
              Played on <strong>{tone.device}</strong> via sink <code>{tone.sink}</code> for{" "}
              {tone.duration_ms} ms.
              {tone.evidence_file ? ` Recorded evidence: ${tone.evidence_file}.` : ""}
              {tone.note ? ` ${tone.note}` : ""}
            </p>
          ) : null}
          <p className="muted">
            Playback is a consented action (docs/spec/13): the tone only plays from this button,
            which is why it is its own control.
          </p>
        </>
      )}
    </div>
  );
}

// ── step 5: the data and privacy review ─────────────────────────────────────

function PrivacyStep({ onFinished }: { onFinished: () => void }) {
  const queryClient = useQueryClient();
  const [confirmed, setConfirmed] = useState(
    () => queryClient.getQueryData<boolean>(privacyConfirmedKey) === true,
  );

  const finish = () => {
    queryClient.setQueryData(privacyConfirmedKey, true);
    onFinished();
  };

  return (
    <div className="stack">
      <section aria-label="Data and privacy review">
        <ul>
          <li>
            <strong>Everything stays on this appliance.</strong> Games, dialogue, voices and the
            speech cache live in the local data directory. Nothing is uploaded.
          </li>
          <li>
            <strong>What leaves: only the model calls.</strong> When a role is configured, its
            requests (text in, text out) go to the endpoint that role's environment names, with that
            role's token. The tokens and endpoint values are never shown on this page or any API
            route (docs/spec/14).
          </li>
          <li>
            <strong>Logs are redacted before they are read.</strong> The diagnostics page's log tail
            passes every value through the support-bundle redactor (URLs, tokens and private IPs
            replaced) before it leaves the process (AGENTS.md rule 5).
          </li>
          <li>
            <strong>Third-party content is yours.</strong> The appliance only ever narrates scripts
            and voices you supplied (docs/spec/00 user-supplied content); it does not commit,
            download or redistribute anything else.
          </li>
        </ul>
      </section>
      <label>
        <input
          type="checkbox"
          checked={confirmed}
          onChange={(event) => {
            setConfirmed(event.target.checked);
          }}
          data-testid="setup-privacy-confirm"
        />
        I have read the data and privacy review
      </label>
      <button
        type="button"
        onClick={finish}
        disabled={!confirmed}
        data-testid="setup-privacy-finish"
      >
        Finish setup
      </button>
    </div>
  );
}

// ── step 6: the done summary, re-read from the same live sources ────────────

type DoneWord = "CONFIGURED" | "NOT CONFIGURED" | "NOT AVAILABLE";

function DoneStep({ probes }: { probes: Record<string, ProbeState> }) {
  const auth = useAuth();
  const queryClient = useQueryClient();
  const privacyConfirmed = queryClient.getQueryData<boolean>(privacyConfirmedKey) === true;

  const capture = useQuery({
    queryKey: ["setup", "capture"],
    queryFn: ({ signal }) => fetchCaptureDevices(signal),
    retry: false,
  });
  const host = useQuery({
    queryKey: ["setup", "host"],
    queryFn: ({ signal }) => fetchDiagnosticsHost(signal),
    retry: false,
  });
  const settings = useQuery({
    queryKey: ["settings"],
    queryFn: ({ signal }) => fetchSettings(signal),
    retry: false,
  });
  const audio = useQuery({
    queryKey: ["setup", "audio"],
    queryFn: ({ signal }) => fetchAudioDevices(signal),
    retry: false,
  });

  const pending = capture.isPending || host.isPending || settings.isPending || audio.isPending;

  const rows: { id: string; label: string; word: DoneWord; detail: string }[] = [];

  rows.push({
    id: "admin",
    label: "Administrator",
    word: "CONFIGURED",
    detail: `signed in as ${auth.user?.username ?? "the admin"} (role admin)`,
  });

  if (!capture.isError) {
    rows.push(
      capture.data && capture.data.devices.length > 0
        ? {
            id: "capture",
            label: "Capture",
            word: "CONFIGURED",
            detail: `${capture.data.devices.length} device(s) detected${capture.data.synthetic ? " (synthetic seam)" : ""}`,
          }
        : {
            id: "capture",
            label: "Capture",
            word: capture.isError ? "NOT AVAILABLE" : "NOT CONFIGURED",
            detail: capture.isError
              ? "no capture layer is wired into this process"
              : "no capture devices found",
          },
    );
  } else {
    rows.push({
      id: "capture",
      label: "Capture",
      word: "NOT AVAILABLE",
      detail:
        "the capture read failed: " +
        (capture.error instanceof Error ? capture.error.message : "unknown"),
    });
  }

  if (host.data) {
    rows.push({
      id: "storage",
      label: "Storage",
      word: "CONFIGURED",
      detail: `${formatBytes(host.data.storage.free_bytes)} free; asset store ${formatBytes(host.data.storage.asset_store_bytes)}`,
    });
  }

  if (settings.data) {
    const configured = settings.data.models.roles.filter((row) => row.configured).length;
    const probed = settings.data.models.roles.flatMap((row) => {
      const probe = probes[row.role];
      if (!probe || probe.running === true || probe.result === null) return [];
      return [`${row.role} ${probe.result.status}`];
    });
    const testedDetail = probed.length > 0 ? " (live-tested: " + probed.join(", ") + ")" : "";
    rows.push({
      id: "models",
      label: "Models",
      word: configured > 0 ? "CONFIGURED" : "NOT CONFIGURED",
      detail: `${configured} of ${MODEL_ROLES.length} roles configured${testedDetail}`,
    });
  }

  if (audio.data) {
    const selected = audio.data.devices.find((device) => device.selected);
    rows.push(
      audio.data.devices.length > 0
        ? {
            id: "audio",
            label: "Audio",
            word: "CONFIGURED",
            detail: `output device ${selected?.name ?? audio.data.selected.device}${audio.data.synthetic ? " (synthetic seam)" : ""}`,
          }
        : {
            id: "audio",
            label: "Audio",
            word: "NOT CONFIGURED",
            detail: "no audio outputs found",
          },
    );
  }

  rows.push({
    id: "privacy",
    label: "Privacy",
    word: privacyConfirmed ? "CONFIGURED" : "NOT CONFIGURED",
    detail: privacyConfirmed
      ? "review read and confirmed this session"
      : "the review has not been confirmed in this session",
  });

  if (pending) return <p className="muted">Re-reading the live state…</p>;

  return (
    <div data-testid="setup-done-summary">
      <table>
        <thead>
          <tr>
            <th>Step</th>
            <th>State</th>
            <th>Detail</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.id} data-testid={`setup-done-row-${row.id}`}>
              <td>{row.label}</td>
              <td>
                <span
                  className={`badge ${row.word === "CONFIGURED" ? "badge-succeeded" : row.word === "NOT AVAILABLE" ? "badge-plain" : "badge-failed"}`}
                >
                  {row.word}
                </span>
              </td>
              <td>{row.detail}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="muted">
        This summary is a live read, not a record: a step that was skipped is marked NOT CONFIGURED
        (or NOT AVAILABLE where the machine cannot answer), and re-entering any step re-reads its
        own state. Continuously checkable things live on the diagnostics page.
      </p>
    </div>
  );
}
