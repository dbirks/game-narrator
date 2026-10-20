import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { fetchSettings, type Settings } from "../api/settings";
import { apiStatus } from "../api/client";
import { ErrorState } from "../components/states";
import { AudioInfoSection } from "./settings/AudioInfo";
import { AudioOutputSection } from "./settings/AudioOutput";
import { BackupRestoreSection } from "./settings/BackupRestore";
import { BluetoothSection } from "./settings/Bluetooth";
import { CaptureSection } from "./settings/Capture";
import { ModelsSection } from "./settings/Models";
import { NetworkSection } from "./settings/Network";
import { ProfilesSection } from "./settings/Profiles";
import { SecuritySection } from "./settings/Security";
import { StorageSection } from "./settings/Storage";
import { ThemeSection } from "./settings/ThemeSection";
import { VersionUpdatesSection } from "./settings/VersionUpdates";

/**
 * `/settings` — docs/spec/09: model endpoints, storage, audio sinks, security,
 * profiles/import policies, backup/export and the update policy. General settings
 * are read-only by design (ADR-0047): every one of them is fixed by the
 * environment or the build, and the page shows the effective values and names
 * the variables a change would have to go through. The one exception is the
 * audio output (ADR-0074): device, sink kind and master volume persist through
 * the "Audio output" card, with every part's source shown so an environment
 * override is visible instead of guessed.
 *
 * The Plum & Peach restyle (ADR-0109): the sections render as kit cards over
 * the same contract — every existing data-testid and role is preserved, and
 * two cards are new: "Theme" (the link to /settings/theme with the current
 * theme named) and "Network" (the tailnet URL, UX-13).
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

/** The read-only document plus the one runtime-mutable setting, in the
 *  design's order: the model roles and their gateway, storage, the audio
 *  surface (the read-only half, the mutable half, the paired radio),
 *  capture, security, the import policies, the theme, the tailnet URL,
 *  backup/restore and the update policy. */
function SettingsView({ data }: { data: Settings }) {
  const mutableNote =
    data.mutable.count === 0
      ? data.mutable.note ||
        `Nothing is mutable at runtime: a change to any of the ${data.mutable.env.length} controlling variables needs an environment edit and a restart. The values below are what is in force.`
      : `${data.mutable.count} setting(s) are mutable at runtime.`;

  return (
    <div className="settings-stack">
      <p className="muted" data-testid="settings-mutable-note">
        {mutableNote}
      </p>
      <ModelsSection data={data} />
      <StorageSection data={data} />
      <AudioInfoSection data={data} />
      <AudioOutputSection />
      <BluetoothSection />
      <CaptureSection />
      <SecuritySection data={data} />
      <ProfilesSection data={data} />
      <ThemeSection />
      <NetworkSection data={data} />
      <BackupRestoreSection />
      <VersionUpdatesSection data={data} />
    </div>
  );
}
