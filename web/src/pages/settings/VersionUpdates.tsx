import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import type { Settings } from "../../api/settings";
import { apiStatus } from "../../api/client";
import { refreshUpdates } from "../../api/settings";
import { Card } from "../../components/kit";

/**
 * `/settings` "Version and updates" (ADR-0088, ADR-0109 restyle): the
 * running build against the latest release of the repository this build
 * belongs to. The check informs, it never installs — auto_update is false
 * in every answer, and the upgrade is the documented manual step
 * (docs/release.md). The section renders the honest states: the fresh
 * answer, the offline-cache fallback, the first check still running, no
 * releases yet, disabled, and the reported error.
 */
export function VersionUpdatesSection({ data }: { data: Settings }) {
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
    <Card title="Version and updates" testid="settings-card-version">
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
    </Card>
  );
}
