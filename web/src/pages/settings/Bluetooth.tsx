import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import {
  fetchBluetoothDevices,
  fetchBluetoothState,
  runBluetoothAction,
  scanBluetoothDevices,
  type BluetoothDevice,
} from "../../api/bt";
import { apiStatus } from "../../api/client";
import { useAuth } from "../../auth/auth-context";
import { Card, Table, type TableColumn } from "../../components/kit";

/**
 * `/settings` "Bluetooth output" (ADR-0091, issues #2/#3, ADR-0142 restyle):
 * the paired-radio half of the audio surface. The state badge reads the
 * adapter (powered, discovering); the scan is time-bounded (the measured
 * elapsed time is the proof the bound held); the inventory carries BlueZ's
 * own pairing flags and the two page derivations the operator acts on —
 * `selected` (this device's A2DP sink is what the audio selection resolves
 * to) and `needs_repair` (the issue-#3 state: connected, no A2DP unit — the
 * repair button re-pairs it over the classic transport).
 *
 * The honest-degradation rule is the section's existence: the controller
 * answers 503 when the build has no Bluetooth (no bus, no BlueZ), and the
 * page then renders nothing — an empty table would read as "no devices",
 * which is a lie about the hardware. A synthetic controller (the
 * GAME_NARRATOR_BT_FAKE seam) is labelled in the DOM, never presented as
 * hardware.
 */

const BT_ACTIONS = ["pair", "connect", "disconnect", "forget"] as const;

type BtAction = (typeof BT_ACTIONS)[number] | "repair" | "trust";

export function BluetoothSection() {
  const { atLeast } = useAuth();
  const canAct = atLeast("operator");
  const queryClient = useQueryClient();
  const stateQuery = useQuery({
    queryKey: ["bluetooth-state"],
    queryFn: ({ signal }) => fetchBluetoothState(signal),
    retry: false,
    staleTime: 5_000,
  });
  const devicesQuery = useQuery({
    queryKey: ["bluetooth-devices"],
    queryFn: ({ signal }) => fetchBluetoothDevices(signal),
    enabled: stateQuery.data?.available ?? false,
    retry: false,
    staleTime: 5_000,
  });
  const [scanning, setScanning] = useState(false);
  const [scanNote, setScanNote] = useState<string | null>(null);
  const [scanElapsed, setScanElapsed] = useState<number | null>(null);
  const [acting, setActing] = useState<string | null>(null); // "mac:action"
  const [actionNote, setActionNote] = useState<string | null>(null);

  // The honest-degradation rule: unwired (the 503) or available=false hides
  // the section entirely — never an empty table.
  if (stateQuery.error || stateQuery.data?.available === false) {
    return null;
  }
  const state = stateQuery.data;
  const devices = devicesQuery.data?.devices ?? [];
  const synthetic =
    (stateQuery.data?.synthetic ?? false) || (devicesQuery.data?.synthetic ?? false);

  // Every mutation is physical radio work whose result lands in both halves
  // of the section above: the inventory (a paired/connected row changes
  // shape) and the audio device list (connecting a sink adds the
  // bluez_output.<MAC>.1 row the Audio output section persists; forgetting
  // removes it). Invalidate both query families so neither badge reads a
  // stale row after pair / connect / disconnect / repair / forget / scan —
  // the two halves of the ADR-0074 selection cannot disagree.
  const refresh = () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: ["bluetooth-state"] }),
      queryClient.invalidateQueries({ queryKey: ["bluetooth-devices"] }),
      queryClient.invalidateQueries({ queryKey: ["audio-devices"] }),
    ]);

  const onScan = async () => {
    if (!canAct || scanning) return;
    setScanning(true);
    setScanNote(null);
    setScanElapsed(null);
    try {
      const answer = await scanBluetoothDevices();
      setScanElapsed(answer.elapsed_ms);
      if (answer.devices.length === 0) {
        setScanNote(answer.note || "the scan found nothing — put the headset in pairing mode");
      } else {
        setScanNote(`found ${answer.devices.length} device(s)`);
      }
      await refresh();
    } catch (error) {
      setScanNote(
        `the scan did not complete: ${error instanceof Error ? error.message : (apiStatus(error) ?? "")}`,
      );
    } finally {
      setScanning(false);
    }
  };

  const onAction = async (device: BluetoothDevice, action: BtAction) => {
    if (!canAct || acting) return;
    setActing(`${device.address}:${action}`);
    setActionNote(null);
    try {
      const answer = await runBluetoothAction(device.address, action);
      setActionNote(
        answer.note
          ? `${device.alias || device.address}: ${answer.note}`
          : `${device.alias || device.address}: ${action} acknowledged`,
      );
      // The mutation landed in the radio; the inventory and the sink list
      // above both change shape, so invalidate both halves. Invalidation is
      // also the only way to reach the Audio section's query from here —
      // it lives in another component.
      await refresh();
    } catch (error) {
      // The daemon's words, verbatim (a 502 carries the exact reason —
      // "needs a PIN", "device is already connected", …).
      setActionNote(
        `${device.alias || device.address}: ${error instanceof Error ? error.message : (apiStatus(error) ?? "")}`,
      );
    } finally {
      setActing(null);
    }
  };

  const columns: readonly TableColumn<BluetoothDevice>[] = [
    {
      key: "device",
      header: "Device",
      cell: (device) => (
        <>
          <code>{device.address}</code> — {device.alias || "(no name yet)"}
          {device.rssi !== undefined ? ` (${device.rssi} dBm)` : ""}
        </>
      ),
    },
    {
      key: "state",
      header: "State",
      cell: (device) => (
        <span data-testid={`bt-device-state-${device.address}`}>
          {[device.paired && "paired", device.trusted && "trusted", device.connected && "connected"]
            .filter(Boolean)
            .join(", ") || "unpaired"}
          {device.discovered ? ", discovered" : ""}
        </span>
      ),
    },
    {
      key: "a2dp",
      header: "A2DP",
      cell: (device) => (device.a2dp_sink ? "sink" : device.a2dp_source ? "source" : "—"),
    },
    {
      key: "marked",
      header: "Marked",
      cell: (device) => (
        <span data-testid={`bt-device-marked-${device.address}`}>
          {device.selected ? "selected" : ""}
          {device.needs_repair ? " — needs repair" : ""}
        </span>
      ),
    },
    {
      key: "actions",
      header: "",
      cell: (device) => (
        <span className="row" data-testid={`bt-device-actions-${device.address}`}>
          {!device.paired && (
            <button
              type="button"
              disabled={!canAct || acting !== null}
              onClick={() => void onAction(device, "pair")}
            >
              {acting === `${device.address}:pair` ? "Pairing…" : "Pair"}
            </button>
          )}
          {!device.connected && (
            <button
              type="button"
              disabled={!canAct || acting !== null}
              onClick={() => void onAction(device, "connect")}
            >
              {acting === `${device.address}:connect` ? "Connecting…" : "Connect"}
            </button>
          )}
          {device.connected && !device.needs_repair && (
            <button
              type="button"
              disabled={!canAct || acting !== null}
              onClick={() => void onAction(device, "disconnect")}
            >
              {acting === `${device.address}:disconnect` ? "…" : "Disconnect"}
            </button>
          )}
          {device.needs_repair && (
            <button
              type="button"
              disabled={!canAct || acting !== null}
              onClick={() => void onAction(device, "repair")}
              data-testid="bt-repair"
            >
              {acting === `${device.address}:repair`
                ? "Repairing…"
                : "Repair (re-pair over classic)"}
            </button>
          )}
          <button
            type="button"
            disabled={!canAct || acting !== null}
            onClick={() => void onAction(device, "forget")}
          >
            {acting === `${device.address}:forget` ? "…" : "Forget"}
          </button>
        </span>
      ),
    },
  ];

  return (
    <Card title="Bluetooth output" testid="settings-audio-bluetooth">
      {synthetic && (
        <p className="muted" role="status" data-testid="bt-synthetic">
          Synthetic Bluetooth inventory (test seam GAME_NARRATOR_BT_FAKE): no radio on this machine
          was touched.
        </p>
      )}
      <p className="row" data-testid="bt-state">
        <span role="status">
          adapter: {state?.powered ? "on" : "off"}
          {state?.discovering ? " (scanning)" : ""}
          {state?.alias ? ` — ${state.alias}` : ""}
        </span>
        {state?.note && <span className="muted">{state.note}</span>}
        <button
          type="button"
          onClick={() => void onScan()}
          disabled={!canAct || scanning || !state?.powered}
          data-testid="bt-scan"
        >
          {scanning ? "Scanning…" : "Scan (≤ 30 s)"}
        </button>
        {scanElapsed !== null && (
          <span className="muted" data-testid="bt-scan-elapsed">
            held {scanElapsed} ms
          </span>
        )}
      </p>
      {scanNote && (
        <p className="muted" role="status" data-testid="bt-scan-note">
          {scanNote}
        </p>
      )}
      {devicesQuery.error ? (
        <p className="muted" role="alert">
          The Bluetooth inventory is unavailable:{" "}
          {devicesQuery.error instanceof Error ? devicesQuery.error.message : ""}
        </p>
      ) : devicesQuery.data === undefined ? (
        <p className="muted">Reading the paired devices…</p>
      ) : (
        <div className="table-overflow" tabIndex={0} aria-label="Paired Bluetooth devices">
          <Table
            caption="The Bluetooth inventory with its pairing state and A2DP evidence"
            testid="bt-devices"
            columns={columns}
            rows={devices}
            rowKey={(device) => device.address}
            rowTestid={(device) => `bt-device-${device.address}`}
          />
          {devices.length === 0 && (
            <p className="muted">
              No paired devices yet — scan, then pair the headset (it must be in pairing mode).
            </p>
          )}
        </div>
      )}
      {actionNote && (
        <p className="muted" role="status" data-testid="bt-action-note">
          {actionNote}
        </p>
      )}
      <p className="muted">
        To make a paired headset the narration output, select its sink (bluez_output.&lt;MAC&gt;.1)
        in the Audio output card; the boot auto-reconnect keeps it connected while the selection
        names it (ADR-0091).
      </p>
    </Card>
  );
}
