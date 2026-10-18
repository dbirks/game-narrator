import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import type { ReactNode } from "react";

import { apiStatus } from "../api/client";
import { downloadBackup, fetchSettings, type Settings } from "../api/settings";
import { ErrorState } from "../components/states";

/**
 * `/settings` — docs/spec/09: model endpoints, storage, audio sinks, security,
 * profiles/import policies, backup/export and the update policy. The page is
 * **read-only by design**: every setting of this build is fixed by the
 * environment or the build (ADR-0047), so the page shows the effective values,
 * names the variables a change would have to go through, and declines writes
 * with the server's 405 explanation instead of a button that saves nothing.
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
  const [busy, setBusy] = useState(false);
  const [backupError, setBackupError] = useState<string | null>(null);
  const mutableNote =
    data.mutable.count === 0
      ? data.mutable.note ||
        `Nothing is mutable at runtime: a change to any of the ${data.mutable.env.length} controlling variables needs an environment edit and a restart. The values below are what is in force.`
      : `${data.mutable.count} setting(s) are mutable at runtime.`;

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

      <section aria-labelledby="settings-backup">
        <h2 id="settings-backup">Backup and export</h2>
        <p className="muted">
          The backup is a point-in-time SQLite snapshot of the appliance's database. Restoring it is
          a command-line act (stop the appliance, replace the file, start it again) — this page
          never overwrites live data.
        </p>
        <button
          type="button"
          onClick={() => void onBackup()}
          disabled={busy}
          data-testid="backup-download"
        >
          {busy ? "Preparing the download…" : "Download database backup"}
        </button>
        {backupError && (
          <p role="alert" className="error">
            {backupError}
          </p>
        )}
      </section>

      <section aria-labelledby="settings-version">
        <h2 id="settings-version">Version and updates</h2>
        <dl className="grid-defs" data-testid="settings-version">
          <dt>Version</dt>
          <dd>
            {data.version.version} (commit {data.version.commit}) — {data.version.go_version} on{" "}
            {data.version.os}/{data.version.arch}
          </dd>
          <dt>Updates</dt>
          <dd>
            {data.update.auto_update ? "auto-update is enabled" : "no auto-update"} —{" "}
            {data.update.note} ({data.update.docs})
          </dd>
        </dl>
      </section>
    </div>
  );
}

// The page's only non-query state is the in-flight backup download and its
// error; everything else is a pure render of the contract's document.

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
