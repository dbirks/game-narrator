import type { CaptureDevicesView } from "../../api/capture";
import { Table, type TableColumn } from "../../components/kit";

export type CaptureDevice = CaptureDevicesView["devices"][number];

/** The mode a session would ask for, as one line (width/height/pixfmt/fps). */
export function modeText(w: number, h: number, fps: number, pixFmt: string): string {
  return `${w}x${h} ${pixFmt} @ ${fps} fps`;
}

/**
 * The capture device table (ADR-0078, ADR-0109 restyle): the kit Table with
 * the e2e contract — the table's own testid (`capture-devices`) and one row
 * testid per device (`capture-device-<id>`). Stable ids, never /dev/videoN;
 * each node's USB link and state, and the mode a session would ask for with
 * its provenance (the operator's pin or the ADR-0076 policy and the
 * policy's reason).
 */
export function CaptureDeviceTable({ devices }: { devices: CaptureDevice[] }) {
  const columns: readonly TableColumn<CaptureDevice>[] = [
    {
      key: "device",
      header: "Device",
      cell: (device) => (
        <>
          <code>{device.id}</code> — {device.name}
        </>
      ),
    },
    { key: "path", header: "Path", cell: (device) => device.path },
    {
      key: "link",
      header: "Link",
      cell: (device) =>
        device.link
          ? `${device.link.speed_mbps} Mb/s (${device.link.class})` +
            (device.link.downgraded ? " — downgraded: re-seat the dongle or try another port" : "")
          : "—",
    },
    {
      key: "state",
      header: "State",
      cell: (device) => (
        <>
          {device.state}
          {device.detail ? <span className="muted"> — {device.detail}</span> : null}
        </>
      ),
    },
    {
      key: "mode",
      header: "Mode a session asks for",
      cell: (device) =>
        device.chosen
          ? `${modeText(device.chosen.w, device.chosen.h, device.chosen.fps, device.chosen.pix_fmt)} (${device.chosen.source}${device.chosen.reason ? `, ${device.chosen.reason}` : ""})`
          : "—",
    },
  ];
  return (
    <div className="table-overflow" tabIndex={0} aria-label="Capture devices">
      <Table
        caption="The capture devices with their USB link, state and the mode a session would ask for"
        testid="capture-devices"
        columns={columns}
        rows={devices}
        rowKey={(device) => device.id}
        rowTestid={(device) => `capture-device-${device.id}`}
      />
    </div>
  );
}
