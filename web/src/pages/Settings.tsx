import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import type { ReactNode } from "react";

import {
  fetchAudioDevices,
  playTestTone,
  saveAudioSettings,
  type TestToneResponse,
} from "../api/audio";
import { apiStatus } from "../api/client";
import {
  fetchBluetoothDevices,
  fetchBluetoothState,
  runBluetoothAction,
  scanBluetoothDevices,
  type BluetoothDevice,
} from "../api/bt";
import {
  fetchCaptureDevices,
  fetchCapturePreview,
  runCaptureProbe,
  selectCaptureDevice,
  type CaptureProbe,
} from "../api/capture";
import {
  downloadBackup,
  fetchRestoreStatus,
  fetchSettings,
  refreshUpdates,
  uploadRestore,
  type RestoreScheduled,
  type RestoreStatus,
  type Settings,
} from "../api/settings";
import { useAuth } from "../auth/auth-context";
import { ErrorState } from "../components/states";

/**
 * `/settings` — docs/spec/09: model endpoints, storage, audio sinks, security,
 * profiles/import policies, backup/export and the update policy. General settings
 * are read-only by design (ADR-0047): every one of them is fixed by the
 * environment or the build, and the page shows the effective values and names
 * the variables a change would have to go through. The one exception is the
 * audio output (ADR-0074): device, sink kind and master volume persist through
 * the "Audio output" section below, with every part's source shown so an
 * environment override is visible instead of guessed.
 *
 * Redaction is the server's job (docs/spec/14: a token or a gateway host in
 * the response is the incident; the handler tests assert it) and the page only
 * ever renders what the contract allows: presence booleans, variable names,
 * base names.
 */

export function SettingsPage() {
  const query = useQuery({
    queryKey: ["settings"],
    queryFn: ({ signal }) => fetchSettings(signal),
    retry: false,
  });

  let body: ReactNode;
  if (query.error) {
    const status = apiStatus(query.error);
    if (status === 401 || status === 403) {
      body = (
        <p className="muted" data-testid="settings-needs-admin">
          Settings are an administrator view: sign in with an admin account to read them, or ask the
          operator to review the values with the CLI.
        </p>
      );
    } else {
      body = (
        <ErrorState
          error={query.error}
          action="Settings unavailable"
          onRetry={() => void query.refetch()}
        />
      );
    }
  } else if (!query.data) {
    body = (
      <p className="muted" data-testid="settings-loading">
        Loading settings…
      </p>
    );
  } else {
    body = <SettingsView data={query.data} />;
  }

  return (
    <main className="page">
      <h1>Settings</h1>
      {body}
    </main>
  );
}

function SettingsView({ data }: { data: Settings }) {
  const mutableNote =
    data.mutable.count === 0
      ? data.mutable.note ||
        `Nothing is mutable at runtime: a change to any of the ${data.mutable.env.length} controlling variables needs an environment edit and a restart. The values below are what is in force.`
      : `${data.mutable.count} setting(s) are mutable at runtime.`;

  return (
    <div>
      <p className="muted" data-testid="settings-mutable-note">
        {mutableNote}
      </p>

      <section aria-labelledby="settings-models">
        <h2 id="settings-models">Model roles</h2>
        <table className="table" data-testid="settings-roles">
          <thead>
            <tr>
              <th>Role</th>
              <th>State</th>
              <th>Model</th>
              <th>Protocol</th>
              <th>Endpoint</th>
              <th>Effort</th>
              <th>Timeout</th>
            </tr>
          </thead>
          <tbody>
            {data.models.roles.map((role) => (
              <tr key={role.role}>
                <td>{role.role}</td>
                <td>
                  <span
                    className={`pill ${role.configured ? "pill-ok" : "pill-warn"}`}
                    data-testid={`role-${role.role}`}
                  >
                    {role.state}
                  </span>
                </td>
                <td>{role.model || "—"}</td>
                <td>{role.protocol}</td>
                <td>{role.base_url || "—"}</td>
                <td>{role.reasoning_effort || "—"}</td>
                <td>{role.timeout || "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <dl className="grid-defs" data-testid="settings-gateway">
          <dt>Shared gateway</dt>
          <dd>
            URL via {data.models.gateway.url_env} —{" "}
            {data.models.gateway.url_configured ? "configured" : "not set"}; token via{" "}
            {data.models.gateway.token_env_name} —{" "}
            {data.models.gateway.token_configured ? "configured" : "not set"} (presence only; the
            values never leave the process)
          </dd>
        </dl>
      </section>

      <section aria-labelledby="settings-storage">
        <h2 id="settings-storage">Storage</h2>
        <dl className="grid-defs" data-testid="settings-storage">
          <dt>Data directory</dt>
          <dd>{data.storage.data_dir_known ? data.storage.data_dir : "not known"}</dd>
          <dt>Free space</dt>
          <dd>
            {data.storage.free_space_known
              ? formatBytes(data.storage.free_space_bytes)
              : "not measured"}
          </dd>
          <dt>Asset store</dt>
          <dd>{formatBytes(data.storage.asset_store_bytes)}</dd>
          <dt>Speech cache</dt>
          <dd>
            {data.storage.speech_cache.bytes !== null
              ? `${formatBytes(data.storage.speech_cache.bytes as number | null)} — `
              : ""}
            {data.storage.speech_cache.note}
          </dd>
        </dl>
      </section>

      <section aria-labelledby="settings-audio">
        <h2 id="settings-audio">Audio</h2>
        <dl className="grid-defs" data-testid="settings-audio">
          <dt>Sink</dt>
          <dd>
            {data.audio.sink}
            {data.audio.note ? ` — ${data.audio.note}` : ""}
          </dd>
          <dt>Device</dt>
          <dd>{data.audio.device || "default"}</dd>
          <dt>Variables</dt>
          <dd>{data.audio.env.join(", ")}</dd>
        </dl>
      </section>

      <AudioOutputSection />

      <BluetoothSection />

      <CaptureSection />

      <section aria-labelledby="settings-security">
        <h2 id="settings-security">Security</h2>
        <dl className="grid-defs" data-testid="settings-security">
          <dt>Bind address</dt>
          <dd>
            {data.security.listen} ({data.security.listen_env})
          </dd>
          <dt>Auth</dt>
          <dd>
            {data.security.auth.mode}
            {data.security.auth.admin_configured
              ? " — an administrator is configured"
              : " — no administrator created yet"}
          </dd>
        </dl>
      </section>

      <section aria-labelledby="settings-imports">
        <h2 id="settings-imports">Profiles and import policies</h2>
        <dl className="grid-defs" data-testid="settings-imports">
          <dt>Web research</dt>
          <dd>
            {data.imports.web_search.enabled ? "registered" : "not registered"}
            {data.imports.web_search.provider
              ? ` (${data.imports.web_search.provider})`
              : ""} —{" "}
            {data.imports.web_search.note}
          </dd>
          <dt>Private endpoints</dt>
          <dd>
            {data.imports.web_search.private_hosts_allowed} host(s) allowlisted via{" "}
            {data.imports.web_search.env_private_allow} (count only; the entries are network
            topology and stay server-side)
          </dd>
          <dt>Agent budget</dt>
          <dd>
            {data.imports.agent.max_steps} steps / {data.imports.agent.max_tool_calls} tool calls
            per run ({data.imports.agent.source})
          </dd>
        </dl>
      </section>

      <BackupRestoreSection />

      <UpdatesSection data={data} />
    </div>
  );
}

// The page's only non-query state is the in-flight restore scheduling and its
// answer; everything else is a pure render of the contract's document.

/**
 * The backup/restore pair (ADR-0090): the backup download (a point-in-time
 * SQLite snapshot) and the restore — the upload validates server-side
 * (format, schema marker, integrity, forward-migrations only), takes an
 * automatic pre-restore copy, and the appliance restarts to apply it. The
 * file's hash and size are shown before the confirm button, so the operator
 * reviews what the restart will install. The post-restart status is this
 * page's banner: none / pending / applied / failed, with the recorded
 * evidence — a restore is never silent.
 */
function BackupRestoreSection() {
  const statusQuery = useQuery({
    queryKey: ["restore-status"],
    queryFn: ({ signal }) => fetchRestoreStatus(signal),
    retry: false,
  });
  const [file, setFile] = useState<File | null>(null);
  const [fileSha, setFileSha] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [backupError, setBackupError] = useState<string | null>(null);
  const [restoreError, setRestoreError] = useState<string | null>(null);
  const [scheduled, setScheduled] = useState<RestoreScheduled | null>(null);

  // The operator reviews the file's own hash and size before the confirm
  // button — the 202 then repeats the hash the server staged, so a file that
  // changed in flight is visible as a mismatch.
  useEffect(() => {
    setScheduled(null);
    if (!file) {
      setFileSha(null);
      return;
    }
    let live = true;
    void file.arrayBuffer().then((bytes) =>
      crypto.subtle.digest("SHA-256", bytes).then((digest) => {
        if (!live) return;
        setFileSha(
          Array.from(new Uint8Array(digest))
            .map((b) => b.toString(16).padStart(2, "0"))
            .join(""),
        );
      }),
    );
    return () => {
      live = false;
    };
  }, [file]);

  const onBackup = async () => {
    setBusy(true);
    setBackupError(null);
    try {
      await downloadBackup();
    } catch (error) {
      setBackupError(
        error instanceof Error ? error.message : `the backup answered ${apiStatus(error) ?? "?"}`,
      );
    } finally {
      setBusy(false);
    }
  };

  const onRestore = async () => {
    if (!file || busy) return;
    setBusy(true);
    setRestoreError(null);
    try {
      const answer = await uploadRestore(file);
      setScheduled(answer);
      void statusQuery.refetch();
    } catch (error) {
      setRestoreError(
        error instanceof Error ? error.message : `the restore answered ${apiStatus(error) ?? "?"}`,
      );
    } finally {
      setBusy(false);
    }
  };

  const status = statusQuery.data;

  return (
    <section aria-labelledby="settings-backup">
      <h2 id="settings-backup">Backup and restore</h2>
      <RestoreBanner status={status} refetching={statusQuery.isFetching} />
      <p className="muted">
        The backup is a point-in-time SQLite snapshot of the appliance's database. A restore
        replaces the appliance's catalogue with a backup file: the server validates it first (SQLite
        format, the Game Narrator schema marker, integrity, and this build's migrations — a backup
        newer than the running binary is refused), copies the live database aside as the rollback
        artifact, and the appliance restarts to apply it.
      </p>
      <div className="row" data-testid="backup-restore-controls">
        <button
          type="button"
          onClick={() => void onBackup()}
          disabled={busy}
          data-testid="backup-download"
        >
          {busy ? "Preparing the download…" : "Download database backup"}
        </button>
        <label htmlFor="restore-file" className="muted">
          Restore from a backup
        </label>
        <input
          id="restore-file"
          type="file"
          accept=".sqlite,.sqlite3,.db"
          data-testid="restore-file"
          onChange={(event) => setFile(event.target.files?.[0] ?? null)}
        />
        <button
          type="button"
          onClick={() => void onRestore()}
          disabled={!file || !fileSha || busy}
          data-testid="restore-upload"
        >
          {busy ? "Scheduling the restore…" : "Schedule restore and restart"}
        </button>
      </div>
      {file && fileSha && !scheduled && (
        <p className="muted" role="status" data-testid="restore-review">
          Selected <code>{file.name}</code> — {file.size} bytes, SHA-256 {fileSha.slice(0, 16)}…
          Scheduling takes the automatic pre-restore copy of the live database and restarts the
          appliance; the restore is applied by the restarted process.
        </p>
      )}
      {backupError && (
        <p role="alert" className="error" data-testid="backup-error">
          {backupError}
        </p>
      )}
      {restoreError && (
        <p role="alert" className="error" data-testid="restore-error">
          {restoreError}
        </p>
      )}
      {scheduled && (
        <p role="status" className="note" data-testid="restore-scheduled">
          Restore scheduled — staged SHA-256 {scheduled.staged_sha256.slice(0, 16)}… (
          {scheduled.staged_bytes} bytes, schema {scheduled.backup_schema_version}), pre-restore
          copy {scheduled.pre_restore_backup}. {scheduled.note}
        </p>
      )}
    </section>
  );
}

// The post-restart banner: the recorded outcome of the last restore. It is
// read from the data directory, so the process that just applied the swap is
// the one that reports it — and a plan the restart never applied is reported
// as pending, with its evidence, not as success.
function RestoreBanner({
  status,
  refetching,
}: {
  status: RestoreStatus | undefined;
  refetching: boolean;
}) {
  if (!status || status.status === "none") return null;
  const plan = status.plan;
  return (
    <div
      data-testid={`restore-banner-${status.status}`}
      role={status.status === "failed" ? "alert" : "status"}
    >
      <strong>
        {status.status === "pending" && "A restore is waiting for the restart"}
        {status.status === "applied" && `Restore applied ${status.at ?? ""}`.trim()}
        {status.status === "failed" && "The last restore failed"}
      </strong>{" "}
      {plan && plan.staged_sha256 && (
        <span className="muted">
          (backup SHA-256 {plan.staged_sha256.slice(0, 16)}…,
          {plan.backup_schema_version !== undefined ? ` schema ${plan.backup_schema_version}` : ""}
          {plan.pre_restore_backup ? `, pre-restore copy ${plan.pre_restore_backup}` : ""})
        </span>
      )}
      {status.status === "applied" &&
        status.schema_version_after != null &&
        status.schema_version_after > 0 && (
          <span className="muted">
            {" "}
            — schema {status.schema_version_before || "?"} → {status.schema_version_after}
            {status.migrations_applied?.length
              ? `, migrations: ${status.migrations_applied.join(", ")}`
              : ""}
          </span>
        )}
      {status.status === "failed" && status.error && <p className="error">{status.error}</p>}
      {status.status === "failed" && status.recovered_from_pre_restore && (
        <p className="muted">
          The appliance recovered from the pre-restore copy and is running on the pre-restore state;
          the rollback artifact is {plan?.pre_restore_backup ?? "the pre-restore copy"}.
        </p>
      )}
      {refetching && status.status === "pending" && <span className="muted"> (checking…)</span>}
    </div>
  );
}

/**
 * The version-and-updates section (ADR-0088): the running build against the
 * latest release of the repository this build belongs to. The check informs,
 * it never installs — auto_update is false in every answer, and the upgrade
 * is the documented manual step (docs/release.md). The section renders the
 * honest states: the fresh answer, the offline-cache fallback, the first
 * check still running, no releases yet, disabled, and the reported error.
 */
function UpdatesSection({ data }: { data: Settings }) {
  const update = data.update;
  const queryClient = useQueryClient();
  const [refreshing, setRefreshing] = useState(false);
  const [refreshNote, setRefreshNote] = useState<string | null>(null);

  const onRefresh = async () => {
    if (refreshing) return;
    setRefreshing(true);
    setRefreshNote(null);
    try {
      await refreshUpdates();
      // The section renders the settings query's document; the refresh wrote
      // a fresh answer, so re-read the document rather than render a stale one.
      void queryClient.invalidateQueries({ queryKey: ["settings"] });
    } catch (error) {
      setRefreshNote(
        `the re-check answered ${
          error instanceof Error ? error.message : (apiStatus(error) ?? "?")
        } — the section keeps its last answer`,
      );
    } finally {
      setRefreshing(false);
    }
  };

  return (
    <section aria-labelledby="settings-version">
      <h2 id="settings-version">Version and updates</h2>
      <dl className="grid-defs" data-testid="settings-version">
        <dt>Version</dt>
        <dd>
          {data.version.version} (commit {data.version.commit}) — {data.version.go_version} on{" "}
          {data.version.os}/{data.version.arch}
        </dd>
        <dt>Updates</dt>
        <dd data-testid="settings-update-state">
          {update.auto_update ? "auto-update is enabled" : "no auto-update"} — {update.note}
          {update.latest ? (
            <>
              {" "}
              · latest release{" "}
              <a href={update.latest.url} data-testid="update-notes-link">
                {update.latest.version}
              </a>
              {update.available
                ? " — a newer version is available (manual replacement: docs/release.md)"
                : ""}
            </>
          ) : (
            <>
              {update.checking ? " — the first update check is still running" : ""}
              {update.no_releases ? " — the release endpoint has no releases yet" : ""}
              {update.disabled ? " — the update check is disabled" : ""}
            </>
          )}
          {update.latest && update.checked_at
            ? ` · checked ${new Date(update.checked_at).toLocaleString()}`
            : ""}
          {update.from_cache ? " (from the offline cache — the fresh check failed)" : ""}
          {update.error ? ` — ${update.error}` : ""} · control variables:{" "}
          {(update.env ?? []).join(", ")} ({update.docs})
        </dd>
      </dl>
      <button
        type="button"
        onClick={() => void onRefresh()}
        disabled={refreshing}
        data-testid="updates-refresh"
      >
        {refreshing ? "Re-checking…" : "Re-check for updates"}
      </button>
      {refreshNote && (
        <p className="muted" role="status" data-testid="updates-refresh-note">
          {refreshNote}
        </p>
      )}
    </section>
  );
}

/**
 * The one runtime-mutable setting (ADR-0074): the output device, the sink kind
 * and the device-level master volume. The device list comes from the live
 * enumeration with a backend name — and when the test seam
 * (GAME_NARRATOR_AUDIO_FAKE_DEVICES) stands in for a real backend, the list is
 * labelled synthetic here, never presented as hardware. Every selection part
 * carries its source (env / store / default), so "why does my volume win" is
 * answered by the row, not by guessing. The test tone plays only on the
 * button's click: consent is the gate (docs/spec/13), and the filesink leaves
 * the tone's recorded WAV named in the answer, so a machine with no audible
 * output still gets proof.
 */
function AudioOutputSection() {
  const { atLeast } = useAuth();
  const canEdit = atLeast("admin");
  const canPlay = atLeast("operator");
  const queryClient = useQueryClient();
  const devicesQuery = useQuery({
    queryKey: ["audio-devices"],
    queryFn: ({ signal }) => fetchAudioDevices(signal),
    retry: false,
    staleTime: 15_000,
  });
  const [saving, setSaving] = useState(false);
  const [saveNote, setSaveNote] = useState<string | null>(null);
  const [tone, setTone] = useState<TestToneResponse | null>(null);
  const [toneBusy, setToneBusy] = useState(false);
  const [toneError, setToneError] = useState<string | null>(null);
  const [volume, setVolume] = useState<number | null>(null);

  const view = devicesQuery.data;
  const selected = view?.selected;
  const masterVolume = volume ?? selected?.master_volume ?? 1;

  const persist = async (update: Parameters<typeof saveAudioSettings>[0]) => {
    if (!canEdit || saving) return;
    setSaving(true);
    setSaveNote(null);
    try {
      const answer = await saveAudioSettings(update);
      setSaveNote(
        `saved — effective: device ${answer.effective.device} (${answer.effective.device_source}), ` +
          `sink ${answer.effective.sink_kind} (${answer.effective.sink_kind_source}), ` +
          `volume ${Math.round(answer.effective.master_volume * 100)}% (${answer.effective.master_volume_source})`,
      );
      if (update.master_volume !== undefined) setVolume(update.master_volume);
      // Both halves read the row this save just wrote: the table above (the
      // "selected" mark and the select's own value) and the Bluetooth
      // section's badge (the server derives it per request from the same
      // ADR-0074 row). Invalidate both query families so a saved selection
      // that is — or is no longer — a bluez_output.<MAC>.1 sink refreshes
      // here, not on the next navigation.
      await queryClient.invalidateQueries({ queryKey: ["audio-devices"] });
      await queryClient.invalidateQueries({ queryKey: ["bluetooth-devices"] });
    } catch (error) {
      setSaveNote(
        `not saved: ${error instanceof Error ? error.message : (apiStatus(error) ?? "")}`,
      );
    } finally {
      setSaving(false);
    }
  };

  const onTone = async () => {
    if (!canPlay || toneBusy) return;
    setToneBusy(true);
    setToneError(null);
    try {
      const target = view?.selected.device;
      const answer = await playTestTone(target === "default" ? undefined : target);
      setTone(answer);
    } catch (error) {
      setToneError(
        `the tone did not play: ${error instanceof Error ? error.message : (apiStatus(error) ?? "")}`,
      );
    } finally {
      setToneBusy(false);
    }
  };

  return (
    <section aria-labelledby="settings-audio-output" data-testid="settings-audio-output">
      <h2 id="settings-audio-output">Audio output</h2>
      {devicesQuery.error ? (
        <p className="muted" role="alert">
          The device list is unavailable:
          {devicesQuery.error instanceof Error ? devicesQuery.error.message : ""} — the values below
          apply on the next session; the test tone still probes the live sink.
        </p>
      ) : !view ? (
        <p className="muted" data-testid="audio-output-loading">
          Reading the output devices…
        </p>
      ) : (
        <div>
          {view.synthetic && (
            <p className="muted" role="status" data-testid="audio-output-synthetic">
              Synthetic device list (test seam): this is not the hardware of this machine.
            </p>
          )}
          {view.backend === "none" && (
            <p className="muted" role="status">
              No output backend answered{view.note ? ` — ${view.note}` : ""}; the selection below
              still applies on the next session.
            </p>
          )}
          <table className="table" data-testid="audio-devices">
            <thead>
              <tr>
                <th>Device</th>
                <th>Kind</th>
                <th>Rate</th>
                <th>Marked</th>
              </tr>
            </thead>
            <tbody>
              {view.devices.map((device) => (
                <tr key={device.id} data-testid={`audio-device-${device.id}`}>
                  <td>
                    <code>{device.id}</code> — {device.name}
                  </td>
                  <td>{device.kind}</td>
                  <td>{device.sample_rate ?? "—"}</td>
                  <td>
                    {selected && selected.device === device.id
                      ? "selected"
                      : device.platform_default
                        ? "platform default"
                        : ""}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="row" data-testid="audio-output-controls">
            <label htmlFor="audio-device-select">Device</label>
            <select
              id="audio-device-select"
              data-testid="audio-device-select"
              value={selected?.device ?? "default"}
              disabled={!canEdit || saving || view.backend === "none"}
              onChange={(event) => void persist({ device: event.target.value })}
            >
              <option value="default">default (the platform's own routing)</option>
              {view.devices
                .filter((device) => device.id !== "default")
                .map((device) => (
                  <option key={device.id} value={device.id}>
                    {device.name} ({device.kind})
                  </option>
                ))}
            </select>
            <span className="muted" data-testid="audio-device-source">
              {selected?.device_source ?? ""}
            </span>
            <label htmlFor="audio-master-volume">Master volume</label>
            <input
              id="audio-master-volume"
              data-testid="audio-master-volume"
              type="range"
              min={0}
              max={1}
              step={0.05}
              value={masterVolume}
              disabled={!canEdit || saving}
              onChange={(event) => setVolume(Number(event.target.value))}
              onMouseUp={() => void persist({ master_volume: masterVolume })}
              onKeyUp={() => void persist({ master_volume: masterVolume })}
            />
            <span className="muted" data-testid="audio-master-volume-value">
              {Math.round(masterVolume * 100)}% ({selected?.master_volume_source ?? ""})
            </span>
            <button
              type="button"
              onClick={() => void onTone()}
              disabled={!canPlay || toneBusy}
              data-testid="audio-test-tone"
            >
              {toneBusy ? "Playing…" : "Play test tone (440 Hz, 1 s)"}
            </button>
          </div>
          {saveNote && (
            <p className="muted" role="status" data-testid="audio-output-note">
              {saveNote}
            </p>
          )}
          {!canEdit && (
            <p className="muted">
              The device and master volume persist for an administrator; the test tone is available
              to the operator.
            </p>
          )}
          {tone && (
            <p className="muted" role="status" data-testid="audio-tone-result">
              tone played through {tone.sink}
              {tone.evidence_file ? ` — evidence: ${tone.evidence_file}` : ""} ({tone.duration_ms}{" "}
              ms on {tone.device})
            </p>
          )}
          {toneError && (
            <p role="alert" className="error" data-testid="audio-tone-error">
              {toneError}
            </p>
          )}
        </div>
      )}
    </section>
  );
}

/**
 * `/settings` "Bluetooth output" (ADR-0091, issues #2/#3): the paired-radio
 * half of the audio surface. The state badge reads the adapter (powered,
 * discovering); the scan is time-bounded (the measured elapsed time is the
 * proof the bound held); the inventory carries BlueZ's own pairing flags and
 * the two page derivations the operator acts on — `selected` (this device's
 * A2DP sink is what the audio selection resolves to) and `needs_repair`
 * (the issue-#3 state: connected, no A2DP unit — the repair button
 * re-pairs it over the classic transport).
 *
 * The honest-degradation rule is the section's existence: the controller
 * answers 503 when the build has no Bluetooth (no bus, no BlueZ), and the
 * page then renders nothing — an empty table would read as "no devices",
 * which is a lie about the hardware. A synthetic controller (the
 * GAME_NARRATOR_BT_FAKE seam) is labelled in the DOM, never presented as
 * hardware.
 */

const BT_ACTIONS = ["pair", "connect", "disconnect", "forget"] as const;

type BtAction = (typeof BT_ACTIONS)[number] | "repair" | "trust";

function BluetoothSection() {
  const { atLeast } = useAuth();
  const canAct = atLeast("operator");
  const queryClient = useQueryClient();
  const stateQuery = useQuery({
    queryKey: ["bluetooth-state"],
    queryFn: ({ signal }) => fetchBluetoothState(signal),
    retry: false,
    staleTime: 5_000,
  });
  const devicesQuery = useQuery({
    queryKey: ["bluetooth-devices"],
    queryFn: ({ signal }) => fetchBluetoothDevices(signal),
    enabled: stateQuery.data?.available ?? false,
    retry: false,
    staleTime: 5_000,
  });
  const [scanning, setScanning] = useState(false);
  const [scanNote, setScanNote] = useState<string | null>(null);
  const [scanElapsed, setScanElapsed] = useState<number | null>(null);
  const [acting, setActing] = useState<string | null>(null); // "mac:action"
  const [actionNote, setActionNote] = useState<string | null>(null);

  // The honest-degradation rule: unwired (the 503) or available=false hides
  // the section entirely — never an empty table.
  if (stateQuery.error || stateQuery.data?.available === false) {
    return null;
  }
  const state = stateQuery.data;
  const devices = devicesQuery.data?.devices ?? [];
  const synthetic =
    (stateQuery.data?.synthetic ?? false) || (devicesQuery.data?.synthetic ?? false);

  // Every mutation is physical radio work whose result lands in both halves
  // of the section above: the inventory (a paired/connected row changes
  // shape) and the audio device list (connecting a sink adds the
  // bluez_output.<MAC>.1 row the Audio output section persists; forgetting
  // removes it). Invalidate both query families so neither badge reads a
  // stale row after pair / connect / disconnect / repair / forget / scan —
  // the two halves of the ADR-0074 selection cannot disagree.
  const refresh = () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: ["bluetooth-state"] }),
      queryClient.invalidateQueries({ queryKey: ["bluetooth-devices"] }),
      queryClient.invalidateQueries({ queryKey: ["audio-devices"] }),
    ]);

  const onScan = async () => {
    if (!canAct || scanning) return;
    setScanning(true);
    setScanNote(null);
    setScanElapsed(null);
    try {
      const answer = await scanBluetoothDevices();
      setScanElapsed(answer.elapsed_ms);
      if (answer.devices.length === 0) {
        setScanNote(answer.note || "the scan found nothing — put the headset in pairing mode");
      } else {
        setScanNote(`found ${answer.devices.length} device(s)`);
      }
      await refresh();
    } catch (error) {
      setScanNote(
        `the scan did not complete: ${error instanceof Error ? error.message : (apiStatus(error) ?? "")}`,
      );
    } finally {
      setScanning(false);
    }
  };

  const onAction = async (device: BluetoothDevice, action: BtAction) => {
    if (!canAct || acting) return;
    setActing(`${device.address}:${action}`);
    setActionNote(null);
    try {
      const answer = await runBluetoothAction(device.address, action);
      setActionNote(
        answer.note
          ? `${device.alias || device.address}: ${answer.note}`
          : `${device.alias || device.address}: ${action} acknowledged`,
      );
      // The mutation landed in the radio; the inventory and the sink list
      // above both change shape, so invalidate both halves. Invalidation is
      // also the only way to reach the Audio section's query from here —
      // it lives in another component.
      await refresh();
    } catch (error) {
      // The daemon's words, verbatim (a 502 carries the exact reason —
      // "needs a PIN", "device is already connected", …).
      setActionNote(
        `${device.alias || device.address}: ${error instanceof Error ? error.message : (apiStatus(error) ?? "")}`,
      );
    } finally {
      setActing(null);
    }
  };

  return (
    <section aria-labelledby="settings-audio-bluetooth" data-testid="settings-audio-bluetooth">
      <h2 id="settings-audio-bluetooth">Bluetooth output</h2>
      {synthetic && (
        <p className="muted" role="status" data-testid="bt-synthetic">
          Synthetic Bluetooth inventory (test seam GAME_NARRATOR_BT_FAKE): no radio on this machine
          was touched.
        </p>
      )}
      <p className="row" data-testid="bt-state">
        <span role="status">
          adapter: {state?.powered ? "on" : "off"}
          {state?.discovering ? " (scanning)" : ""}
          {state?.alias ? ` — ${state.alias}` : ""}
        </span>
        {state?.note && <span className="muted">{state.note}</span>}
        <button
          type="button"
          onClick={() => void onScan()}
          disabled={!canAct || scanning || !state?.powered}
          data-testid="bt-scan"
        >
          {scanning ? "Scanning…" : "Scan (≤ 30 s)"}
        </button>
        {scanElapsed !== null && (
          <span className="muted" data-testid="bt-scan-elapsed">
            held {scanElapsed} ms
          </span>
        )}
      </p>
      {scanNote && (
        <p className="muted" role="status" data-testid="bt-scan-note">
          {scanNote}
        </p>
      )}
      {devicesQuery.error ? (
        <p className="muted" role="alert">
          The Bluetooth inventory is unavailable:{" "}
          {devicesQuery.error instanceof Error ? devicesQuery.error.message : ""}
        </p>
      ) : devicesQuery.data === undefined ? (
        <p className="muted">Reading the paired devices…</p>
      ) : (
        <table className="table" data-testid="bt-devices">
          <thead>
            <tr>
              <th>Device</th>
              <th>State</th>
              <th>A2DP</th>
              <th>Marked</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {devices.map((device) => (
              <tr key={device.address} data-testid={`bt-device-${device.address}`}>
                <td>
                  <code>{device.address}</code> — {device.alias || "(no name yet)"}
                  {device.rssi !== undefined ? ` (${device.rssi} dBm)` : ""}
                </td>
                <td data-testid={`bt-device-state-${device.address}`}>
                  {[
                    device.paired && "paired",
                    device.trusted && "trusted",
                    device.connected && "connected",
                  ]
                    .filter(Boolean)
                    .join(", ") || "unpaired"}
                  {device.discovered ? ", discovered" : ""}
                </td>
                <td>{device.a2dp_sink ? "sink" : device.a2dp_source ? "source" : "—"}</td>
                <td data-testid={`bt-device-marked-${device.address}`}>
                  {device.selected ? "selected" : ""}
                  {device.needs_repair ? " — needs repair" : ""}
                </td>
                <td className="row" data-testid={`bt-device-actions-${device.address}`}>
                  {!device.paired && (
                    <button
                      type="button"
                      disabled={!canAct || acting !== null}
                      onClick={() => void onAction(device, "pair")}
                    >
                      {acting === `${device.address}:pair` ? "Pairing…" : "Pair"}
                    </button>
                  )}
                  {!device.connected && (
                    <button
                      type="button"
                      disabled={!canAct || acting !== null}
                      onClick={() => void onAction(device, "connect")}
                    >
                      {acting === `${device.address}:connect` ? "Connecting…" : "Connect"}
                    </button>
                  )}
                  {device.connected && !device.needs_repair && (
                    <button
                      type="button"
                      disabled={!canAct || acting !== null}
                      onClick={() => void onAction(device, "disconnect")}
                    >
                      {acting === `${device.address}:disconnect` ? "…" : "Disconnect"}
                    </button>
                  )}
                  {device.needs_repair && (
                    <button
                      type="button"
                      disabled={!canAct || acting !== null}
                      onClick={() => void onAction(device, "repair")}
                      data-testid="bt-repair"
                    >
                      {acting === `${device.address}:repair`
                        ? "Repairing…"
                        : "Repair (re-pair over classic)"}
                    </button>
                  )}
                  <button
                    type="button"
                    disabled={!canAct || acting !== null}
                    onClick={() => void onAction(device, "forget")}
                  >
                    {acting === `${device.address}:forget` ? "…" : "Forget"}
                  </button>
                </td>
              </tr>
            ))}
            {devices.length === 0 && (
              <tr>
                <td colSpan={5} className="muted">
                  No paired devices yet — scan, then pair the headset (it must be in pairing mode).
                </td>
              </tr>
            )}
          </tbody>
        </table>
      )}
      {actionNote && (
        <p className="muted" role="status" data-testid="bt-action-note">
          {actionNote}
        </p>
      )}
      <p className="muted">
        To make a paired headset the narration output, select its sink (bluez_output.&lt;MAC&gt;.1)
        in the Audio output section above; the boot auto-reconnect keeps it connected while the
        selection names it (ADR-0091).
      </p>
    </section>
  );
}

/**
 * `/settings` "Capture" (ADR-0078, docs/spec/08/10): the UVC HDMI capture
 * dongle as a product surface. The device list (stable ids, never
 * /dev/videoN) with each node's USB link, state and the mode a session would
 * ask for (with its provenance — the operator's pin or the ADR-0076 policy
 * and the policy's reason). The selection persists (device always, the mode
 * when pinned; omitting it records "the policy decides"). The preview is one
 * throttled JPEG per poll (the browser's `<img>` is the loop; the server
 * rate-limits and refuses while a session holds the device), and the capture
 * test is the docs/spec/10 probe: grabbed/dropped/fps/freshness plus OCR over
 * a region, degrading to the engine's install action when it is missing.
 *
 * The test seam (GAME_NARRATOR_CAPTURE_FAKE) is LABELLED synthetic in the DOM
 * — the same rule ADR-0074 applies to the audio list — so a fake device is
 * never mistaken for the hardware of this machine.
 */
const PREVIEW_POLL_MS = 2000;

function modeText(w: number, h: number, fps: number, pixFmt: string): string {
  return `${w}x${h} ${pixFmt} @ ${fps} fps`;
}

function CaptureSection() {
  const query = useQuery({
    queryKey: ["capture-devices"],
    queryFn: ({ signal }) => fetchCaptureDevices(signal),
    retry: false,
  });
  const [selectedId, setSelectedId] = useState<string>("");
  const [modePin, setModePin] = useState<string>("");
  const [saving, setSaving] = useState(false);
  const [saveNote, setSaveNote] = useState<string | null>(null);
  const [previewOn, setPreviewOn] = useState(false);
  const [previewUrl, setPreviewUrl] = useState<string | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [probe, setProbe] = useState<CaptureProbe | null>(null);
  const [probeBusy, setProbeBusy] = useState(false);
  const [probeError, setProbeError] = useState<string | null>(null);

  const view = query.data;
  // The operator's current pick wins the picker's initial value; the answer's
  // selected_id is the persisted selection a session without an explicit
  // device runs, so the page shows what a session will use.
  useEffect(() => {
    if (view && view.selected_id && !selectedId) setSelectedId(view.selected_id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view]);

  // The live preview: one snapshot per poll while on. The server is the
  // throttle (one in flight, 500 ms apart; 429 when exceeded), the page just
  // asks at a polite cadence and keeps the last good frame on a refused poll.
  useEffect(() => {
    if (!previewOn) return;
    let live = true;
    let timer: number | undefined;
    const tick = async () => {
      try {
        const blob = await fetchCapturePreview(selectedId || undefined);
        if (!live) {
          URL.revokeObjectURL(URL.createObjectURL(blob));
          return;
        }
        const url = URL.createObjectURL(blob);
        setPreviewUrl((old) => {
          if (old) URL.revokeObjectURL(old);
          return url;
        });
        setPreviewError(null);
      } catch (error) {
        if (live)
          setPreviewError(
            error instanceof Error
              ? error.message
              : `the preview answered ${apiStatus(error) ?? "?"}`,
          );
      } finally {
        if (live) timer = window.setTimeout(tick, PREVIEW_POLL_MS);
      }
    };
    void tick();
    return () => {
      live = false;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [previewOn, selectedId]);

  // Revoke the object URL when the section unmounts with the preview on.
  useEffect(
    () => () => {
      setPreviewUrl((old) => {
        if (old) URL.revokeObjectURL(old);
        return null;
      });
    },
    [],
  );

  const readyDevices = (view?.devices ?? []).filter((device) => device.state === "ready");
  const selected = view?.devices.find((device) => device.id === selectedId);

  const persist = async () => {
    if (saving || selectedId === "") return;
    setSaving(true);
    setSaveNote(null);
    try {
      const mode =
        modePin === "" || !selected?.chosen
          ? undefined
          : {
              w: selected.chosen.w,
              h: selected.chosen.h,
              fps: selected.chosen.fps,
              pix_fmt: selected.chosen.pix_fmt,
            };
      const saved = await selectCaptureDevice(selectedId, mode);
      setSaveNote(
        `saved — device ${saved.device_id}` +
          (saved.mode
            ? `, pinned mode ${modeText(saved.mode.w, saved.mode.h, saved.mode.fps, saved.mode.pix_fmt)}`
            : ", the policy decides the mode"),
      );
      await query.refetch();
    } catch (error) {
      setSaveNote(
        `not saved: ${error instanceof Error ? error.message : (apiStatus(error) ?? "")}`,
      );
    } finally {
      setSaving(false);
    }
  };

  const onProbe = async () => {
    if (probeBusy) return;
    setProbeBusy(true);
    setProbeError(null);
    try {
      const result = await runCaptureProbe(selectedId ? { device: selectedId } : {});
      setProbe(result);
    } catch (error) {
      setProbeError(
        error instanceof Error ? error.message : `the probe answered ${apiStatus(error) ?? "?"}`,
      );
    } finally {
      setProbeBusy(false);
    }
  };

  return (
    <section aria-labelledby="settings-capture" data-testid="settings-capture">
      <h2 id="settings-capture">Capture</h2>
      {query.error ? (
        <p className="muted" role="alert">
          The capture device list is unavailable:
          {query.error instanceof Error ? query.error.message : ""}
        </p>
      ) : !view ? (
        <p className="muted" data-testid="capture-loading">
          Reading the capture devices…
        </p>
      ) : (
        <div>
          {view.synthetic && (
            <p className="muted" role="status" data-testid="capture-synthetic">
              Synthetic device list (test seam): this is not the hardware of this machine.
              {view.note ? ` — ${view.note}` : ""}
            </p>
          )}
          {view.devices.length === 0 ? (
            <p className="muted" data-testid="capture-none">
              No capture device is attached: connect the UVC HDMI dongle and it will appear here
              (the list re-enumerates on the next read).
            </p>
          ) : (
            <table className="table" data-testid="capture-devices">
              <thead>
                <tr>
                  <th>Device</th>
                  <th>Path</th>
                  <th>Link</th>
                  <th>State</th>
                  <th>Mode a session asks for</th>
                </tr>
              </thead>
              <tbody>
                {view.devices.map((device) => (
                  <tr key={device.id} data-testid={`capture-device-${device.id}`}>
                    <td>
                      <code>{device.id}</code> — {device.name}
                    </td>
                    <td>{device.path}</td>
                    <td>
                      {device.link
                        ? `${device.link.speed_mbps} Mb/s (${device.link.class})` +
                          (device.link.downgraded
                            ? " — downgraded: re-seat the dongle or try another port"
                            : "")
                        : "—"}
                    </td>
                    <td>
                      {device.state}
                      {device.detail ? <span className="muted"> — {device.detail}</span> : null}
                    </td>
                    <td>
                      {device.chosen
                        ? `${modeText(device.chosen.w, device.chosen.h, device.chosen.fps, device.chosen.pix_fmt)} (${device.chosen.source}${device.chosen.reason ? `, ${device.chosen.reason}` : ""})`
                        : "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {readyDevices.length > 0 && (
            <div className="row" data-testid="capture-controls">
              <label htmlFor="capture-device-select">Device</label>
              <select
                id="capture-device-select"
                data-testid="capture-device-select"
                value={selectedId}
                disabled={saving}
                onChange={(event) => {
                  setSelectedId(event.target.value);
                  setModePin("");
                  setProbe(null);
                }}
              >
                <option value="">choose a capture device…</option>
                {readyDevices.map((device) => (
                  <option key={device.id} value={device.id}>
                    {device.name} ({device.id})
                  </option>
                ))}
              </select>
              <label htmlFor="capture-mode-select">Mode</label>
              <select
                id="capture-mode-select"
                data-testid="capture-mode-select"
                value={modePin}
                disabled={!selectedId || saving || !selected?.chosen}
                onChange={(event) => setModePin(event.target.value)}
              >
                <option value="">the policy decides</option>
                {selected?.chosen ? (
                  <option value="chosen">
                    {modeText(
                      selected.chosen.w,
                      selected.chosen.h,
                      selected.chosen.fps,
                      selected.chosen.pix_fmt,
                    )}{" "}
                    (current pick)
                  </option>
                ) : null}
              </select>
              <button
                type="button"
                onClick={() => void persist()}
                disabled={!selectedId || saving}
                data-testid="capture-save"
              >
                {saving ? "Saving…" : "Save selection"}
              </button>
              <button
                type="button"
                onClick={() => setPreviewOn((on) => !on)}
                disabled={!view.devices.some((d) => d.state === "ready")}
                data-testid="capture-preview-toggle"
              >
                {previewOn ? "Stop preview" : "Show live preview"}
              </button>
              <button
                type="button"
                onClick={() => void onProbe()}
                disabled={!view.devices.some((d) => d.state === "ready") || probeBusy}
                data-testid="capture-probe"
              >
                {probeBusy ? "Running the capture test…" : "Run capture test"}
              </button>
            </div>
          )}
          {saveNote && (
            <p className="muted" role="status" data-testid="capture-note">
              {saveNote}
            </p>
          )}
          {previewOn && (
            <div data-testid="capture-preview">
              {previewUrl ? (
                <img
                  src={previewUrl}
                  alt="Capture device preview"
                  data-testid="capture-preview-img"
                  style={{ maxWidth: "480px", border: "1px solid currentColor" }}
                />
              ) : (
                !previewError && (
                  <p className="muted" data-testid="capture-preview-pending">
                    Taking the first snapshot…
                  </p>
                )
              )}
              {previewError && (
                <p className="error" role="alert" data-testid="capture-preview-error">
                  {previewError}
                </p>
              )}
              <p className="muted">
                The preview is one throttled snapshot per {PREVIEW_POLL_MS / 1000} s and is refused
                while a session holds this device (the UVC bus is single-consumer).
              </p>
            </div>
          )}
          {probe && (
            <div data-testid="capture-probe-result">
              <p>
                {probe.device} · {probe.mode} ({probe.mode_source}) — grabbed {probe.grabbed}/
                {probe.requested} · dropped {probe.dropped} · {probe.fps} fps · freshness{" "}
                {probe.freshness_ms} ms · state {probe.state}
                {probe.last_error ? ` — ${probe.last_error}` : ""}
              </p>
              {probe.ocr ? (
                <p data-testid="capture-probe-ocr">
                  OCR ({probe.ocr.engine}) on {probe.ocr.region.x},{probe.ocr.region.y}{" "}
                  {probe.ocr.region.w}x{probe.ocr.region.h}:
                  {probe.ocr.text ? ` “${probe.ocr.text}”` : " (no text read)"}
                  {probe.ocr.note ? ` — ${probe.ocr.note}` : ""}
                </p>
              ) : null}
            </div>
          )}
          {probeError && (
            <p className="error" role="alert" data-testid="capture-probe-error">
              {probeError}
            </p>
          )}
        </div>
      )}
    </section>
  );
}

function formatBytes(bytes: number | null): string {
  if (bytes === null) return "not measured";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let unit = "B";
  for (const name of units) {
    if (value < 1024) break;
    value /= 1024;
    unit = name;
  }
  return `${value.toFixed(unit === "B" ? 0 : 1)} ${unit}`;
}
