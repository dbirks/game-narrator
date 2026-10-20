import { expect, test } from "@playwright/test";
import { existsSync, readdirSync } from "node:fs";
import { join } from "node:path";

import { trackConsoleErrors } from "./support/console";
import { ports } from "./support/ports";
import { expectDefaultDataDirUntouched, serverRun } from "./support/servers";

/**
 * The audio-output product surface (ADR-0074, docs/spec/10 "USB DAC audio as
 * a product surface"), on the packaged binary with the test seam standing in
 * for the enumeration backend:
 *
 *   1. /settings "Audio output" renders the enumerated devices — the fake
 *      list is LABELLED synthetic in the DOM (a fake list must never be
 *      mistaken for hardware), with the kind and the selection's source
 *      shown on each part;
 *   2. choosing a device persists it (PUT /api/v1/settings/audio) and the
 *      API answer reads it back from the store, device_source "store";
 *   3. the master volume persists the same way (0..1, source "store");
 *   4. the test tone plays only on the button's click (consent, docs/spec/13)
 *      and — with the filesink the walk configures — leaves its recorded WAV
 *      on the server's own data directory, which the spec reads from disk:
 *      on a machine with no audible output the file is the proof.
 *
 * The device list is synthetic BY LABELLING (the response's synthetic flag
 * drives the banner), so nothing here is presented as real hardware —
 * the live check is the Go side's (internal/audio, the PipeWire null-sink
 * round trip), which reports NOT RUN where the backend is absent.
 */

const ADMIN = { username: "audio-admin", password: "steady as the tide" };
// The seam's two rows (playwright.config.ts's audio server env): the null sink
// and a USB DAC. The fake lister's device id is the name, so the spec names
// rows by these exact strings.
const NULL_SINK_ID = "auto_null";
const USB_DAC_ID = "JBL-USB-DAC";

interface AudioDevicesAnswer {
  backend: string;
  synthetic: boolean;
  devices: { id: string; name: string; kind: string; selected: boolean }[];
  selected: {
    device: string;
    device_source: string;
    sink_kind: string;
    sink_kind_source: string;
    master_volume: number;
    master_volume_source: string;
  };
}

test("the audio output section enumerates, persists and proves the tone", async ({
  page,
}, testInfo) => {
  test.setTimeout(180_000);
  test.skip(
    testInfo.project.name !== "audio",
    "runs on the audio project's own packaged-binary backend",
  );
  const run = serverRun("audio");
  const origin = `http://127.0.0.1:${ports.audio}`;
  const consoleErrors = trackConsoleErrors(page);

  // ── 0. claim the throwaway appliance and sign the page in ──────────────────
  await page.goto(`${origin}/`);
  await expect(page).toHaveURL(/\/setup$/);
  await page.getByTestId("setup-form").waitFor();
  await page.getByTestId("setup-username").fill(ADMIN.username);
  await page.getByTestId("setup-password").fill(ADMIN.password);
  await page.getByTestId("setup-submit").click();
  await expect(page).toHaveURL(/\/login$/);
  await page.getByTestId("login-username").fill(ADMIN.username);
  await page.getByTestId("login-password").fill(ADMIN.password);
  await page.getByTestId("login-submit").click();
  await expect(page).toHaveURL(/\/library$/);

  // ── 1. /settings: the devices, labelled synthetic, with sources ────────────
  await page.goto(`${origin}/settings`);
  const section = page.getByTestId("settings-audio-output");
  await expect(section).toBeVisible();

  // The seam list is labelled synthetic in the DOM — the walk's whole reason
  // for being honest is that this banner exists.
  await expect(page.getByTestId("audio-output-synthetic")).toHaveText(/Synthetic device list/);

  // Both rows render with their kind; the platform default is the null sink's
  // kind, the DAC's is "usb".
  await expect(page.getByTestId(`audio-device-${NULL_SINK_ID}`)).toHaveText(/null/);
  await expect(page.getByTestId(`audio-device-${USB_DAC_ID}`)).toHaveText(/usb/);
  // The selection's source is visible per part (the env override set the
  // sink; nothing is stored yet, so the device is the built-in default).
  expect(page.getByTestId("audio-device-select")).toHaveValue("default");

  // The API agrees with the DOM: the backend is the synthetic seam and the
  // answer carries the synthetic flag (the flag the UI banner reads).
  const before = (await page.evaluate(async () =>
    (await fetch("/api/v1/audio/devices")).json(),
  )) as AudioDevicesAnswer;
  expect(before.backend).toBe("synthetic");
  expect(before.synthetic).toBe(true);
  expect(before.selected.sink_kind).toBe("filesink");
  expect(before.selected.sink_kind_source).toBe("env"); // the walk's env override wins and says so

  // ── 2. choose the USB DAC: it persists and the API reads it back ───────────
  await page.getByTestId("audio-device-select").selectOption(USB_DAC_ID);
  await expect(page.getByTestId("audio-output-note")).toHaveText(/saved — effective/);
  await expect(page.getByTestId(`audio-device-${USB_DAC_ID}`)).toContainText("selected");

  let saved = (await page.evaluate(async () =>
    (await fetch("/api/v1/audio/devices")).json(),
  )) as AudioDevicesAnswer;
  expect(saved.selected.device).toBe(USB_DAC_ID);
  expect(saved.selected.device_source).toBe("store");

  // ── 3. the master volume persists through the same route ───────────────────
  const slider = page.getByTestId("audio-master-volume");
  await slider.focus();
  // End = 1.0; the keyup is what commits, exactly as a keyboard user's does.
  await slider.press("End");
  await expect(page.getByTestId("audio-master-volume-value")).toHaveText(/100%/);
  await expect(page.getByTestId("audio-output-note")).toHaveText(/saved — effective/);

  saved = (await page.evaluate(async () =>
    (await fetch("/api/v1/audio/devices")).json(),
  )) as AudioDevicesAnswer;
  expect(saved.selected.master_volume).toBe(1);
  expect(saved.selected.master_volume_source).toBe("store");

  // ── 4. the consent-gated test tone, with the recorded WAV as the proof ─────
  await page.getByTestId("audio-test-tone").click();
  await expect(page.getByTestId("audio-tone-result")).toContainText("filesink");
  await expect(page.getByTestId("audio-tone-result")).toContainText(USB_DAC_ID);
  await expect(page.getByTestId("audio-tone-result")).toContainText("evidence:");

  // The filesink wrote the tone's WAV under the server's own throwaway data
  // directory: on a box with no speaker the file is the proof the tone went
  // out. A zero-byte file would be a sink that lied about playing.
  const evidenceDir = join(run.dataDir, "audio-test-tone");
  expect(existsSync(evidenceDir), `no evidence directory under ${run.dataDir}`).toBe(true);
  const wavs = readdirSync(evidenceDir).filter((name) => name.endsWith(".wav"));
  expect(wavs.length).toBeGreaterThanOrEqual(1);

  expect(consoleErrors, `the browser logged faults: ${consoleErrors.join(" | ")}`).toHaveLength(0);
  expectDefaultDataDirUntouched(run);
});
