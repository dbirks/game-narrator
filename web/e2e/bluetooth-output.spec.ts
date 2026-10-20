import { expect, test } from "@playwright/test";

import { trackConsoleErrors } from "./support/console";
import { ports } from "./support/ports";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The Bluetooth output walk (ADR-0091, issues #2/#3), on the packaged binary
 * with the labelled-synthetic controller (GAME_NARRATOR_BT_FAKE) standing in
 * for the radio — the same ADR-0074/0078 seam pattern as the audio and
 * capture walks:
 *
 *   1. /settings "Bluetooth output" renders the adapter state, LABELLED
 *      synthetic in the DOM (a fake radio must never read as hardware);
 *   2. the inventory shows the fake devices with BlueZ's own pairing flags;
 *   3. the scan is bounded (the measured elapsed time renders in the DOM);
 *   4. the lifecycle walk: pair → connect → disconnect → forget the fresh
 *      device (forget removes the row);
 *   5. selecting the headset's A2DP sink (bluez_output.<MAC>.1) through the
 *      Audio output section persists the selection AND badges the device
 *      "selected" — the two halves of the ADR-0074 selection cannot
 *      disagree;
 *   6. the issue-#3 state: the connected device with no A2DP unit is
 *      marked "needs repair" and the repair button re-pairs it (the A2DP
 *      unit comes back, the mark goes away).
 *
 * The hardware gate (a real board + a real headset, hwsmoke) is the only
 * place a real radio is asserted; it is NOT RUN until it runs, and nothing
 * here is presented as real hardware — the synthetic label is the walk's
 * honesty rule.
 */

// The fake controller's inventory (internal/bt's DefaultFake, the process
// seam): the addresses are the obviously-fake fixture constants (the
// public-repo rule keeps real hardware addresses out of the tree).
const BT_A2DP = "AA:BB:CC:DD:EE:FF"; // paired, connected, the A2DP unit up
const BT_FRESH = "11:22:33:44:55:66"; // discoverable only: pair it in step 4
const BT_LEONLY = "99:88:77:66:55:44"; // the issue-#3 state: LE-only
const BT_SINK_ID = `bluez_output.${BT_A2DP}.1`;

const ADMIN = { username: "bt-admin", password: "steady as the tide" };
// The audio-output spec (same project, same server, alphabetical order)
// claims the appliance first with its own admin; when the claim is already
// taken this spec signs in as that admin instead.
const AUDIO_ADMIN = { username: "audio-admin", password: "steady as the tide" };

interface BluetoothStateAnswer {
  available: boolean;
  synthetic: boolean;
  powered: boolean;
  discovering: boolean;
}

interface BluetoothDeviceRow {
  address: string;
  paired: boolean;
  trusted: boolean;
  connected: boolean;
  a2dp_sink: boolean;
  selected: boolean;
  needs_repair?: boolean;
}

interface BluetoothDevicesAnswer {
  synthetic: boolean;
  devices: BluetoothDeviceRow[];
}

test("the bluetooth output section scans, pairs, selects and repairs", async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(180_000);
  test.skip(
    testInfo.project.name !== "audio",
    "runs on the audio project's own packaged-binary backend",
  );
  const run = serverRun("audio");
  const origin = `http://127.0.0.1:${ports.audio}`;
  const consoleErrors = trackConsoleErrors(page);

  // ── 0. claim the throwaway appliance (or reuse the audio walk's) and sign in
  const claimed = await request.post(`${origin}/api/v1/auth/setup`, {
    data: { username: ADMIN.username, password: ADMIN.password },
  });
  const admin = claimed.status() === 201 ? ADMIN : AUDIO_ADMIN;

  await page.goto(`${origin}/`);
  await page.getByTestId("login-username").waitFor();
  await page.getByTestId("login-username").fill(admin.username);
  await page.getByTestId("login-password").fill(admin.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/library$/);

  // ── 1. the section renders, labelled synthetic, with the adapter state ─────
  await page.goto(`${origin}/settings`);
  const section = page.getByTestId("settings-audio-bluetooth");
  await expect(section).toBeVisible();
  await expect(page.getByTestId("bt-synthetic")).toContainText("GAME_NARRATOR_BT_FAKE");
  await expect(page.getByTestId("bt-state")).toContainText("adapter: on");

  // The API agrees: available + synthetic (the flag the banner reads).
  const state = (await page.evaluate(async () =>
    (await fetch("/api/v1/audio/bluetooth/state")).json(),
  )) as BluetoothStateAnswer;
  expect(state.available).toBe(true);
  expect(state.synthetic).toBe(true);
  expect(state.powered).toBe(true);

  // The two known fixture rows are in the inventory before any scan
  // (bluez's record); the discoverable-only one joins it when the scan
  // finds it — the honest "the inquiry puts it in the record" semantics.
  await expect(page.getByTestId(`bt-device-${BT_A2DP}`)).toBeVisible();
  await expect(page.getByTestId(`bt-device-${BT_LEONLY}`)).toBeVisible();
  await expect(page.getByTestId(`bt-device-${BT_FRESH}`)).toHaveCount(0);
  await expect(page.getByTestId(`bt-device-state-${BT_A2DP}`)).toContainText("connected");
  await expect(page.getByTestId(`bt-device-state-${BT_A2DP}`)).toContainText("paired");

  // ── 2. the bounded scan: the button, the measured hold, the devices ────────
  await page.getByTestId("bt-scan").click();
  await expect(page.getByTestId("bt-scan-elapsed")).toContainText("held");
  // The fresh device answered the inquiry and is now in the inventory.
  await expect(page.getByTestId(`bt-device-${BT_FRESH}`)).toBeVisible();
  const scanNote = page.getByTestId("bt-scan-note");
  // The note reports the find (one or more devices) — an empty find would
  // carry the pairing-mode hint instead, which the fixture cannot produce.
  await expect(scanNote).toContainText(/found \d+/);

  // ── 3. the lifecycle walk on the fresh device: pair → connect →
  // disconnect → forget (the forget removes the row from the inventory) ──────
  await page
    .getByTestId(`bt-device-actions-${BT_FRESH}`)
    .getByRole("button", { name: "Pair" })
    .click();
  await expect(page.getByTestId("bt-action-note")).toContainText("paired and trusted");
  await page
    .getByTestId(`bt-device-actions-${BT_FRESH}`)
    .getByRole("button", { name: "Connect" })
    .click();
  await expect(page.getByTestId(`bt-device-state-${BT_FRESH}`)).toContainText("connected");
  await page
    .getByTestId(`bt-device-actions-${BT_FRESH}`)
    .getByRole("button", { name: "Disconnect" })
    .click();
  await expect(page.getByTestId(`bt-device-state-${BT_FRESH}`)).not.toContainText("connected");
  await page
    .getByTestId(`bt-device-actions-${BT_FRESH}`)
    .getByRole("button", { name: "Forget" })
    .click();
  await expect(page.getByTestId(`bt-device-${BT_FRESH}`)).toHaveCount(0);

  // ── 4. the selection integration: picking the A2DP sink in the Audio
  // output section badges the device "selected" (both halves read the same
  // ADR-0074 row) ─────────────────────────────────────────────────────────────
  await page.getByTestId("audio-device-select").selectOption(BT_SINK_ID);
  await expect(page.getByTestId("audio-output-note")).toHaveText(/saved — effective/);
  await expect(page.getByTestId(`bt-device-marked-${BT_A2DP}`)).toContainText("selected");

  const devices = (await page.evaluate(async () =>
    (await fetch("/api/v1/audio/bluetooth/devices")).json(),
  )) as BluetoothDevicesAnswer;
  expect(devices.synthetic).toBe(true);
  const selected = devices.devices.find((d) => d.address === BT_A2DP);
  expect(selected?.selected).toBe(true);
  // And the audio half agrees: the effective device is the sink id.
  const audioDevices = (await page.evaluate(async () =>
    (await fetch("/api/v1/audio/devices")).json(),
  )) as { selected: { device: string } };
  expect(audioDevices.selected.device).toBe(BT_SINK_ID);

  // ── 5. the issue-#3 state: connected, no A2DP unit → "needs repair" and
  // the repair button re-pairs it over the classic transport ────────────────
  await expect(page.getByTestId(`bt-device-marked-${BT_LEONLY}`)).toContainText("needs repair");
  const leBefore = (await page.evaluate(async () =>
    (await fetch("/api/v1/audio/bluetooth/devices")).json(),
  )) as BluetoothDevicesAnswer;
  expect(leBefore.devices.find((d) => d.address === BT_LEONLY)?.a2dp_sink).toBe(false);

  await page.getByTestId("bt-repair").click();
  await expect(page.getByTestId(`bt-device-marked-${BT_LEONLY}`)).not.toContainText("needs repair");
  const leAfter = (await page.evaluate(async () =>
    (await fetch("/api/v1/audio/bluetooth/devices")).json(),
  )) as BluetoothDevicesAnswer;
  const repaired = leAfter.devices.find((d) => d.address === BT_LEONLY);
  expect(repaired?.a2dp_sink).toBe(true);
  expect(repaired?.needs_repair).toBeFalsy();

  expect(consoleErrors, `the browser logged faults: ${consoleErrors.join(" | ")}`).toHaveLength(0);
  expectDefaultDataDirUntouched(run);
});
