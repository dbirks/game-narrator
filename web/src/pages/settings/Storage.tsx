import type { Settings } from "../../api/settings";
import { Card } from "../../components/kit";

/**
 * `/settings` "Storage" (ADR-0109 restyle): the data directory, the free
 * space, and the asset store's size. Read-only by design (ADR-0047) — the
 * value is what is in force, and the variable a change would have to go
 * through is named.
 */
export function StorageSection({ data }: { data: Settings }) {
  const storage = data.storage;
  return (
    <Card title="Storage" testid="settings-card-storage">
      <dl className="grid-defs" data-testid="settings-storage">
        <dt>Data directory</dt>
        <dd>{storage.data_dir_known ? storage.data_dir : "not known"}</dd>
        <dt>Free space</dt>
        <dd>{storage.free_space_known ? formatBytes(storage.free_space_bytes) : "not measured"}</dd>
        <dt>Asset store</dt>
        <dd>{formatBytes(storage.asset_store_bytes)}</dd>
        <dt>Speech cache</dt>
        <dd>
          {storage.speech_cache.bytes !== null
            ? `${formatBytes(storage.speech_cache.bytes as number | null)} — `
            : ""}
          {storage.speech_cache.note}
        </dd>
      </dl>
    </Card>
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
