import type { AudioDevice, AudioDevicesView, AudioSelection } from "../../api/audio";
import { Table, type TableColumn } from "../../components/kit";

/**
 * The enumerated device table of the "Audio output" card (ADR-0074): the
 * kit Table with the e2e contract — the table's own testid (`audio-devices`)
 * and one row testid per device (`audio-device-<id>`). The "Marked" column
 * carries the selection mark (the "selected" row, the platform default) so
 * the table and the select's own value cannot disagree.
 */
export function AudioDeviceTable({
  view,
  selected,
}: {
  view: AudioDevicesView;
  selected: AudioSelection | undefined;
}) {
  const columns: readonly TableColumn<AudioDevice>[] = [
    {
      key: "device",
      header: "Device",
      cell: (device) => (
        <>
          <code>{device.id}</code> — {device.name}
        </>
      ),
    },
    { key: "kind", header: "Kind", cell: (device) => device.kind },
    { key: "rate", header: "Rate", cell: (device) => device.sample_rate ?? "—" },
    {
      key: "marked",
      header: "Marked",
      cell: (device) =>
        selected && selected.device === device.id
          ? "selected"
          : device.platform_default
            ? "platform default"
            : "",
    },
  ];
  return (
    <div className="table-overflow" tabIndex={0} aria-label="Audio output devices">
      <Table
        caption="The enumerated output devices with their sink kind and selection mark"
        testid="audio-devices"
        columns={columns}
        rows={view.devices}
        rowKey={(device) => device.id}
        rowTestid={(device) => `audio-device-${device.id}`}
      />
    </div>
  );
}
