import type { Settings } from "../../api/settings";
import { Card } from "../../components/kit";

/**
 * `/settings` "Security" (ADR-0142 restyle, docs/spec/14): the bound listen
 * address and the auth mode. Read-only by design (ADR-0047): the page shows
 * the effective values and names the variable a change would have to go
 * through. The bound address is an address, not a credential — it already
 * crosses this boundary (ADR-0142 keeps the tailnet section's note on the
 * same footing).
 */
export function SecuritySection({ data }: { data: Settings }) {
  const security = data.security;
  return (
    <Card title="Security" testid="settings-card-security">
      <dl className="grid-defs" data-testid="settings-security">
        <dt>Bind address</dt>
        <dd>
          {security.listen} ({security.listen_env})
        </dd>
        <dt>Auth</dt>
        <dd>
          {security.auth.mode}
          {security.auth.admin_configured
            ? " — an administrator is configured"
            : " — no administrator created yet"}
        </dd>
      </dl>
    </Card>
  );
}
