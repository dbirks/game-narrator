import type { Settings } from "../../api/settings";
import { Card } from "../../components/kit";

/**
 * `/settings` "Network" (docs/design/plum-peach UX-13, ADR-0109): the
 * tailnet URL — the Tailscale address that reaches this appliance's web UI.
 * The appliance has no screen of its own, so the URL is what the family
 * types on the laptop beside the TV, and it is copied from the running
 * system (the operator's GAME_NARRATOR_TAILNET_URL pin or the live
 * tailscale status record) so the display always matches the appliance.
 *
 * The not-configured state is explicit, never a blank field: a null URL
 * renders the "not configured" mark and the API's note verbatim (the
 * machine's own reason and the unblock step).
 */
export function NetworkSection({ data }: { data: Settings }) {
  const network = data.network;
  return (
    <Card title="Network — the tailnet URL" testid="settings-card-network">
      <p className="muted">
        The appliance has no screen: this is the address the tailnet resolves to, copied from the
        running system.
      </p>
      <div className="settings-tailnet" data-testid="settings-network">
        {network.tailnet_url !== null ? (
          <code data-testid="tailnet-url">{network.tailnet_url}</code>
        ) : (
          <span className="pill pill-warn" data-testid="tailnet-url">
            not configured
          </span>
        )}
        <span className="muted" data-testid="tailnet-note">
          {network.note}
        </span>
      </div>
    </Card>
  );
}
