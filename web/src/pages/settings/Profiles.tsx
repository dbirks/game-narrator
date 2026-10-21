import type { Settings } from "../../api/settings";
import { Card } from "../../components/kit";

/**
 * `/settings` "Profiles and import policies" (ADR-0142 restyle, docs/spec/14):
 * the agent's research surface (the registered web-search provider and the
 * private-endpoint allowlist count — the entries are network topology and
 * stay server-side) and the agent budget. Read-only by design (ADR-0047):
 * the values are what is in force, and the source (build constants,
 * environment) is named.
 */
export function ProfilesSection({ data }: { data: Settings }) {
  const imports = data.imports;
  return (
    <Card title="Profiles and import policies" testid="settings-card-profiles">
      <dl className="grid-defs" data-testid="settings-imports">
        <dt>Web research</dt>
        <dd>
          {imports.web_search.enabled ? "registered" : "not registered"}
          {imports.web_search.provider ? ` (${imports.web_search.provider})` : ""} —{" "}
          {imports.web_search.note}
        </dd>
        <dt>Private endpoints</dt>
        <dd>
          {imports.web_search.private_hosts_allowed} host(s) allowlisted via{" "}
          {imports.web_search.env_private_allow} (count only; the entries are network topology and
          stay server-side)
        </dd>
        <dt>Agent budget</dt>
        <dd>
          {imports.agent.max_steps} steps / {imports.agent.max_tool_calls} tool calls per run (
          {imports.agent.source})
        </dd>
      </dl>
    </Card>
  );
}
