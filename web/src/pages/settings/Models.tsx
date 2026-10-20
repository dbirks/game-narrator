import type { Settings } from "../../api/settings";
import { Card, Table, type TableColumn } from "../../components/kit";

type RoleRow = Settings["models"]["roles"][number];

/**
 * `/settings` "Model roles" (ADR-0109 restyle, docs/spec/09): the roles
 * providers.LoadRoles resolves for this build, each with its state, model,
 * protocol, endpoint and budgets. General settings are read-only by design
 * (ADR-0047): the table shows the effective values, and the shared-gateway
 * line names the variables a change would have to go through.
 *
 * Redaction is the server's job (docs/spec/14: a token or a gateway host in
 * the response is the incident; the handler tests assert it) — the page only
 * renders what the contract allows: presence booleans, variable names, base
 * names. The kit Table keeps the e2e contract: the table's own testid
 * (`settings-roles`) and the per-role pill (`role-<role>`).
 */
export function ModelsSection({ data }: { data: Settings }) {
  const { roles, gateway } = data.models;
  const columns: readonly TableColumn<RoleRow>[] = [
    { key: "role", header: "Role", cell: (row) => row.role },
    {
      key: "state",
      header: "State",
      cell: (row) => (
        <span
          className={`pill ${row.configured ? "pill-ok" : "pill-warn"}`}
          data-testid={`role-${row.role}`}
        >
          {row.state}
        </span>
      ),
    },
    { key: "model", header: "Model", cell: (row) => row.model || "—" },
    { key: "protocol", header: "Protocol", cell: (row) => row.protocol },
    { key: "endpoint", header: "Endpoint", cell: (row) => row.base_url || "—" },
    { key: "effort", header: "Effort", cell: (row) => row.reasoning_effort || "—" },
    { key: "timeout", header: "Timeout", cell: (row) => row.timeout || "—" },
  ];

  return (
    <Card title="Model roles" testid="settings-card-models">
      <div className="table-overflow" tabIndex={0} aria-label="Model roles">
        <Table
          caption="The model roles this build resolves, with their state, model, protocol and endpoint (redacted: base names only)"
          testid="settings-roles"
          columns={columns}
          rows={roles}
          rowKey={(row) => row.role}
        />
      </div>
      <dl className="grid-defs" data-testid="settings-gateway">
        <dt>Shared gateway</dt>
        <dd>
          URL via {gateway.url_env} — {gateway.url_configured ? "configured" : "not set"}; token via{" "}
          {gateway.token_env_name} — {gateway.token_configured ? "configured" : "not set"} (presence
          only; the values never leave the process)
        </dd>
      </dl>
    </Card>
  );
}
