/**
 * `src/api/capture.ts` — typed access for the UVC HDMI capture dongle surface
 * (ADR-0078, docs/spec/08/10): the device list with its links and chosen
 * modes, the persisted selection, the throttled preview snapshot and the
 * capture test (probe).
 *
 * The two product rules the page depends on, straight from the contract:
 *
 * - **The enumeration is the allowlist.** A session or a selection may name
 *   a device only by the stable id this module's list reports — never a raw
 *   /dev/videoN — so the client cannot open a node the operator did not
 *   enumerate.
 * - **Synthetic lists are labelled, not hidden.** `CaptureDevices.synthetic`
 *   is true when the list came from the test seam
 *   (GAME_NARRATOR_CAPTURE_FAKE); the UI says so and nothing is mistaken for
 *   hardware.
 */

import { api, ApiError } from "./client";
import type { components } from "./schema";

export type CaptureDevicesView = components["schemas"]["CaptureDevices"];
export type CaptureDevice = components["schemas"]["CaptureDevice"];
export type CaptureMode = components["schemas"]["CaptureMode"];
export type CaptureChosen = components["schemas"]["CaptureChosen"];
export type CaptureSelection = components["schemas"]["CaptureSelection"];
export type CaptureSelectionRequest = components["schemas"]["CaptureSelectionRequest"];
export type CaptureBox = components["schemas"]["CaptureBox"];
export type CaptureSignal = components["schemas"]["CaptureSignal"];
export type CaptureProbeRequest = components["schemas"]["CaptureProbeRequest"];
export type CaptureProbe = components["schemas"]["CaptureProbe"];

const devicesPath = "/api/v1/capture/devices" as const;
const devicePath = "/api/v1/capture/devices/{id}" as const;
const previewPath = "/api/v1/capture/preview" as const;
const signalPath = "/api/v1/capture/signal" as const;
const probePath = "/api/v1/capture/probe" as const;

/** The enumerated capture devices, the current selection and the synthetic flag. */
export function fetchCaptureDevices(signal?: AbortSignal): Promise<CaptureDevicesView> {
  return api.get(devicesPath, { signal }) as Promise<CaptureDevicesView>;
}

/**
 * Persist the operator's selection: the device always, the mode when pinned.
 * Omitting `mode` records "the policy decides for this device". The answer is
 * the saved row, validated against the live enumeration (an unknown device or
 * a non-offered mode is a 400, never a saved row a session would refuse).
 *
 * The id is a single path segment: by-id and by-path ids are full device
 * paths and contain slashes, so the `buildUrl` encoder percent-encodes them
 * (a '/' becomes %2F) and the router decodes it back — an unencoded id is a
 * 400 `device-id-not-encoded`, not a 307 (issue #10). Callers pass the id
 * exactly as the device list reports it.
 */
export function selectCaptureDevice(
  id: string,
  mode?: CaptureMode,
  signal?: AbortSignal,
): Promise<CaptureSelection> {
  const body: CaptureSelectionRequest = mode ? { mode } : {};
  return api.put(devicePath, { params: { id }, body, signal }) as Promise<CaptureSelection>;
}

/**
 * The HDMI signal state (ADR-0110 §1, UX-11): the JSON sibling of the
 * preview. A running v4l2 session answers from the session's live state
 * (measured fps, the active-picture window, the last dialogue crop, the
 * newest frame's age); without one, a one-shot frame grab answers through
 * the throwaway path. When neither is possible the route is the 409
 * `capture-signal-unavailable` the page renders as its honest state. It
 * shares the preview's global throttle (429 + Retry-After on the overrun).
 */
export function fetchCaptureSignal(signal?: AbortSignal): Promise<CaptureSignal> {
  return api.get(signalPath, { signal }) as Promise<CaptureSignal>;
}

/**
 * The throttled preview snapshot. Unlike the JSON routes this one returns an
 * `image/jpeg` body, so it goes through fetch (credentials + the CSRF token)
 * and hands back the bytes — the page builds an object URL for its `<img>`.
 * Throttled server-side (one in flight, 500 ms apart) with 429 + Retry-After;
 * 503 `capture-busy` while a running session holds the device.
 */
export async function fetchCapturePreview(device?: string, signal?: AbortSignal): Promise<Blob> {
  const url = api.buildUrl(previewPath, { query: device ? { device } : undefined });
  const response = await fetch(url, {
    method: "GET",
    credentials: "same-origin",
    signal,
  });
  if (!response.ok) {
    let detail = `the preview answered HTTP ${response.status}`;
    try {
      const problem = (await response.json()) as { detail?: string };
      if (problem.detail) detail = problem.detail;
    } catch {
      // the body was not a problem document; keep the status text
    }
    throw new ApiError({
      status: response.status,
      code: "http-" + response.status,
      title: "Preview failed",
      detail,
    });
  }
  return response.blob();
}

/**
 * Run the docs/spec/10 capture test on the resolved device: grab `frames`
 * (0 = default 20, capped 200), report the freshness/drop stats and OCR the
 * requested region (absent = the centre third) of the last frame. OCR is
 * degradable — a missing engine degrades the `ocr` block to
 * `engine: "unavailable"` with the install action, never failing the probe.
 */
export function runCaptureProbe(
  body: CaptureProbeRequest = {},
  signal?: AbortSignal,
): Promise<CaptureProbe> {
  return api.post(probePath, { body, signal }) as Promise<CaptureProbe>;
}
