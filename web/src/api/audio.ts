/**
 * `src/api/audio.ts` — the typed access for the audio output surface
 * (ADR-0074): the device list with its sources, the consent-gated test tone,
 * and the persisted selection (device, sink kind, master volume).
 *
 * The two product rules the page depends on, straight from the contract:
 *
 * - **Synthetic lists are labelled, not hidden.** `AudioDevices.synthetic` is
 *   true when the list came from the test seam
 *   (GAME_NARRATOR_AUDIO_FAKE_DEVICES); the UI says so and the doctor refuses
 *   to treat it as hardware.
 * - **Every selection part carries its source** (env / store / default), so
 *   the page can say why a value wins — the environment override beats the
 *   stored value, and that is visible instead of guessed.
 */

import { api } from "./client";
import type { components } from "./schema";

export type AudioDevicesView = components["schemas"]["AudioDevices"];
export type AudioDevice = components["schemas"]["AudioDevice"];
export type AudioSelection = components["schemas"]["AudioSelection"];
export type TestToneResponse = components["schemas"]["TestToneResponse"];
export type AudioSettingsAnswer = components["schemas"]["AudioSettingsAnswer"];

const devicesPath = "/api/v1/audio/devices" as const;
const testTonePath = "/api/v1/audio/test-tone" as const;
const settingsPath = "/api/v1/settings/audio" as const;

/** The enumerated output devices and the effective selection. */
export function fetchAudioDevices(signal?: AbortSignal): Promise<AudioDevicesView> {
  return api.get(devicesPath, { signal }) as Promise<AudioDevicesView>;
}

/**
 * Play the deterministic test tone (1.0 s / 440 Hz / 48 kHz).
 *
 * `consent: true` is the gate: the caller only invokes this from the page's
 * explicit "play" action (docs/spec/13 — playback is a consented action).
 * When `device` names an enumerated id, that device is probed; absent, the
 * selected device plays.
 */
export function playTestTone(device?: string, signal?: AbortSignal): Promise<TestToneResponse> {
  return api.post(testTonePath, {
    body: { consent: true, ...(device ? { device } : {}) },
    signal,
  }) as Promise<TestToneResponse>;
}

/** The partial body of the persisted audio selection (ADR-0074). */
export type AudioSettingsUpdate = components["schemas"]["AudioSettingsUpdate"];

/**
 * Persist the audio output selection. A field left out keeps the stored
 * value; the answer carries the row now stored and the effective selection
 * (with every part's source, so an env override is visible).
 */
export function saveAudioSettings(
  update: AudioSettingsUpdate,
  signal?: AbortSignal,
): Promise<AudioSettingsAnswer> {
  return api.put(settingsPath, { body: update, signal }) as Promise<AudioSettingsAnswer>;
}

/** The per-voice gain of docs/spec/07 (ADR-0074): 0..10, applied at playback. */
export function setVoiceGain(
  voiceId: string,
  gain: number,
  signal?: AbortSignal,
): Promise<{ voice_id: string; gain: number }> {
  return api.post("/api/v1/voices/{id}/gain", {
    params: { id: voiceId },
    body: { gain },
    signal,
  }) as Promise<{ voice_id: string; gain: number }>;
}
