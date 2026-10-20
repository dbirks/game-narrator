/**
 * `src/api/bt.ts` — the typed access for the Bluetooth output surface
 * (ADR-0091, issues #2/#3): the adapter state, the time-bounded scan, the
 * device inventory (the pairing flags, the A2DP evidence, the selected and
 * needs_repair badges), and the lifecycle operations (pair, trust, connect,
 * disconnect, forget, repair).
 *
 * The product rules the page depends on, straight from the contract:
 *
 * - **Unwired is a 503, never an empty list.** A build without the
 *   controller (or with an unreachable bus) answers 503
 *   `BLUETOOTH_UNAVAILABLE`; the page hides the section on
 *   `available: false` rather than rendering an empty table that reads as
 *   "no devices".
 * - **Synthetic inventories are labelled, not hidden.** `synthetic: true`
 *   means the answer came from the GAME_NARRATOR_BT_FAKE seam; the page
 *   says so and never presents the fake as hardware.
 * - **Daemon failures keep the daemon's words.** A 502 carries the exact
 *   reason BlueZ gave (e.g. a device that needs a PIN) — the page shows it
 *   verbatim instead of inventing its own.
 */

import { api } from "./client";
import type { components, paths } from "./schema";

export type BluetoothState = components["schemas"]["BluetoothState"];
export type BluetoothDevice = components["schemas"]["BluetoothDevice"];
export type BluetoothDevicesView = components["schemas"]["BluetoothDevices"];
export type BluetoothScanRequest = components["schemas"]["BluetoothScanRequest"];
export type BluetoothScan = components["schemas"]["BluetoothScan"];
export type BluetoothAction = components["schemas"]["BluetoothAction"];

type PathTemplate = keyof paths & string;

const statePath = "/api/v1/audio/bluetooth/state" as const;
const scanPath = "/api/v1/audio/bluetooth/scan" as const;
const devicesPath = "/api/v1/audio/bluetooth/devices" as const;

export type BtActionName = "pair" | "trust" | "connect" | "disconnect" | "forget" | "repair";

/** The six device-operation templates (one per operation, the MAC as the path parameter). */
const devicePath: Record<BtActionName, PathTemplate> = {
  pair: "/api/v1/audio/bluetooth/devices/{mac}/pair",
  trust: "/api/v1/audio/bluetooth/devices/{mac}/trust",
  connect: "/api/v1/audio/bluetooth/devices/{mac}/connect",
  disconnect: "/api/v1/audio/bluetooth/devices/{mac}/disconnect",
  forget: "/api/v1/audio/bluetooth/devices/{mac}/forget",
  repair: "/api/v1/audio/bluetooth/devices/{mac}/repair",
};

/** The adapter state (powered, discovering) and the availability note. */
export function fetchBluetoothState(signal?: AbortSignal): Promise<BluetoothState> {
  return api.get(statePath, { signal }) as Promise<BluetoothState>;
}

/**
 * Run the time-bounded scan. The daemon bound is the contract (default 8 s,
 * clamped 1–30 s); the answer carries the discovered set and the measured
 * elapsed time, so the page proves the bound was honoured. Absent body = the
 * default hold.
 */
export function scanBluetoothDevices(
  request: BluetoothScanRequest = {},
  signal?: AbortSignal,
): Promise<BluetoothScan> {
  return api.post(scanPath, { body: request, signal }) as Promise<BluetoothScan>;
}

/** The controller's device inventory (the pairing flags and badges). */
export function fetchBluetoothDevices(signal?: AbortSignal): Promise<BluetoothDevicesView> {
  return api.get(devicesPath, { signal }) as Promise<BluetoothDevicesView>;
}

/** One lifecycle operation on one device (the address must be colon-form). */
export function runBluetoothAction(
  mac: string,
  action: BtActionName,
  signal?: AbortSignal,
): Promise<BluetoothAction> {
  return api.post(devicePath[action], {
    params: { mac },
    body: {},
    signal,
  }) as Promise<BluetoothAction>;
}
