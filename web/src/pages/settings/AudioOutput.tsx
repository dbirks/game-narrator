import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import {
  fetchAudioDevices,
  playTestTone,
  saveAudioSettings,
  type TestToneResponse,
} from "../../api/audio";
import { fetchBluetoothDevices, fetchBluetoothState, runBluetoothAction } from "../../api/bt";
import { apiStatus } from "../../api/client";
import { useAuth } from "../../auth/auth-context";
import { Card } from "../../components/kit";
import { AudioDeviceSelect, PAIR_FIRST_PREFIX, type PairFirst } from "./AudioDeviceSelect";
import { AudioDeviceTable } from "./AudioDeviceTable";
import { AudioVolumeAndTone } from "./AudioVolumeAndTone";
import { PairingPrompt } from "./PairingPrompt";

/**
 * The one runtime-mutable setting (ADR-0074, ADR-0109 restyle): the output
 * device, the sink kind and the device-level master volume. The device list
 * comes from the live enumeration with a backend name — and when the test
 * seam (GAME_NARRATOR_AUDIO_FAKE_DEVICES) stands in for a real backend, the
 * list is labelled synthetic here, never presented as hardware. Every
 * selection part carries its source (env / store / default), so "why does
 * my volume win" is answered by the row, not by guessing. The test tone
 * plays only on the button's click: consent is the gate (docs/spec/13), and
 * the filesink leaves the tone's recorded WAV named in the answer, so a
 * machine with no audible output still gets proof.
 *
 * The selector's shape is the appliance's real sink categories (UX-12): one
 * optgroup each for HDMI (the hub's port), USB and Bluetooth, with
 * everything else under "Other". An unpaired device from the Bluetooth
 * inventory (ADR-0091) appears in the Bluetooth group as a "pair first"
 * option; selecting it renders the pairing prompt instead of persisting an
 * id that has no sink.
 */

export function AudioOutputSection() {
  const { atLeast } = useAuth();
  const canEdit = atLeast("admin");
  const canPlay = atLeast("operator");
  const canAct = atLeast("operator");
  const queryClient = useQueryClient();
  const devicesQuery = useQuery({
    queryKey: ["audio-devices"],
    queryFn: ({ signal }) => fetchAudioDevices(signal),
    retry: false,
    staleTime: 15_000,
  });
  // The Bluetooth inventory (ADR-0091) drives the "pair first" options: it
  // shares the query keys the Bluetooth output card above uses, so both
  // halves of the audio surface read the same live answer.
  const btStateQuery = useQuery({
    queryKey: ["bluetooth-state"],
    queryFn: ({ signal }) => fetchBluetoothState(signal),
    retry: false,
    staleTime: 5_000,
  });
  const btDevicesQuery = useQuery({
    queryKey: ["bluetooth-devices"],
    queryFn: ({ signal }) => fetchBluetoothDevices(signal),
    enabled: (btStateQuery.data?.available ?? false) === true,
    retry: false,
    staleTime: 5_000,
  });
  const [saving, setSaving] = useState(false);
  const [saveNote, setSaveNote] = useState<string | null>(null);
  const [tone, setTone] = useState<TestToneResponse | null>(null);
  const [toneBusy, setToneBusy] = useState(false);
  const [toneError, setToneError] = useState<string | null>(null);
  const [volume, setVolume] = useState<number | null>(null);
  const [pairing, setPairing] = useState<PairFirst | null>(null);
  const [pairingBusy, setPairingBusy] = useState(false);
  const [pairingNote, setPairingNote] = useState<string | null>(null);
  const [pairingError, setPairingError] = useState<string | null>(null);

  const view = devicesQuery.data;
  const selected = view?.selected;
  const masterVolume = volume ?? selected?.master_volume ?? 1;

  // The unpaired devices the selector can offer to pair: a BT device with no
  // pairing state and no A2DP sink row in the audio list. The 503/absent
  // adapter simply yields no options (the section degrades, it does not lie).
  const pairFirst: PairFirst[] = (btDevicesQuery.data?.devices ?? [])
    .filter((device) => device.paired === false)
    .filter((device) => !view?.devices.some((d) => d.id === `bluez_output.${device.address}.1`))
    .map((device) => ({ address: device.address, name: device.alias || device.address }));

  const persist = async (update: Parameters<typeof saveAudioSettings>[0]) => {
    if (!canEdit || saving) return;
    setSaving(true);
    setSaveNote(null);
    try {
      const answer = await saveAudioSettings(update);
      setSaveNote(
        `saved — effective: device ${answer.effective.device} (${answer.effective.device_source}), ` +
          `sink ${answer.effective.sink_kind} (${answer.effective.sink_kind_source}), ` +
          `volume ${Math.round(answer.effective.master_volume * 100)}% (${answer.effective.master_volume_source})`,
      );
      if (update.master_volume !== undefined) setVolume(update.master_volume);
      // Both halves read the row this save just wrote: the table above (the
      // "selected" mark and the select's own value) and the Bluetooth
      // section's badge (the server derives it per request from the same
      // ADR-0074 row). Invalidate both query families so a saved selection
      // that is — or is no longer — a bluez_output.<MAC>.1 sink refreshes
      // here, not on the next navigation.
      await queryClient.invalidateQueries({ queryKey: ["audio-devices"] });
      await queryClient.invalidateQueries({ queryKey: ["bluetooth-devices"] });
    } catch (error) {
      setSaveNote(
        `not saved: ${error instanceof Error ? error.message : (apiStatus(error) ?? "")}`,
      );
    } finally {
      setSaving(false);
    }
  };

  const onDeviceChange = (value: string) => {
    if (value.startsWith(PAIR_FIRST_PREFIX)) {
      // The prompt, not a persist: an unpaired device has no sink yet.
      const address = value.slice(PAIR_FIRST_PREFIX.length);
      const device = pairFirst.find((d) => d.address === address);
      if (device) {
        setPairing(device);
        setPairingNote(null);
        setPairingError(null);
      }
      return;
    }
    setPairing(null);
    setPairingNote(null);
    setPairingError(null);
    void persist({ device: value });
  };

  const onPair = async () => {
    if (!pairing || !canAct || pairingBusy) return;
    setPairingBusy(true);
    setPairingError(null);
    try {
      const answer = await runBluetoothAction(pairing.address, "pair");
      setPairingNote(
        `${pairing.name} paired${
          answer.note ? ` — ${answer.note}` : ""
        }; its A2DP sink now appears in the device list — select it to make it the output.`,
      );
      // The pairing landed in the radio: the inventory gains the paired row
      // and the audio list gains the sink. Invalidate both halves.
      await queryClient.invalidateQueries({ queryKey: ["bluetooth-devices"] });
      await queryClient.invalidateQueries({ queryKey: ["audio-devices"] });
    } catch (error) {
      // The daemon's words, verbatim (a 502 carries the exact reason).
      setPairingError(
        `${pairing.name}: ${error instanceof Error ? error.message : (apiStatus(error) ?? "")}`,
      );
    } finally {
      setPairingBusy(false);
    }
  };

  const onTone = async () => {
    if (!canPlay || toneBusy) return;
    setToneBusy(true);
    setToneError(null);
    try {
      const target = view?.selected.device;
      const answer = await playTestTone(target === "default" ? undefined : target);
      setTone(answer);
    } catch (error) {
      setToneError(
        `the tone did not play: ${error instanceof Error ? error.message : (apiStatus(error) ?? "")}`,
      );
    } finally {
      setToneBusy(false);
    }
  };

  return (
    <Card title="Audio output" testid="settings-audio-output">
      {devicesQuery.error ? (
        <p className="muted" role="alert">
          The device list is unavailable:
          {devicesQuery.error instanceof Error ? devicesQuery.error.message : ""} — the values below
          apply on the next session; the test tone still probes the live sink.
        </p>
      ) : !view ? (
        <p className="muted" data-testid="audio-output-loading">
          Reading the output devices…
        </p>
      ) : (
        <div>
          {view.synthetic && (
            <p className="muted" role="status" data-testid="audio-output-synthetic">
              Synthetic device list (test seam): this is not the hardware of this machine.
            </p>
          )}
          {view.backend === "none" && (
            <p className="muted" role="status">
              No output backend answered{view.note ? ` — ${view.note}` : ""}; the selection below
              still applies on the next session.
            </p>
          )}
          <AudioDeviceTable view={view} selected={selected} />
          <div className="row" data-testid="audio-output-controls">
            <label htmlFor="audio-device-select">Device</label>
            <AudioDeviceSelect
              devices={view.devices}
              pairFirst={pairFirst}
              value={
                pairing ? `${PAIR_FIRST_PREFIX}${pairing.address}` : (selected?.device ?? "default")
              }
              disabled={!canEdit || saving || view.backend === "none"}
              onChange={onDeviceChange}
            />
            <span className="muted" data-testid="audio-device-source">
              {selected?.device_source ?? ""}
            </span>
            <AudioVolumeAndTone
              canEdit={canEdit}
              canPlay={canPlay}
              saving={saving}
              toneBusy={toneBusy}
              value={masterVolume}
              source={selected?.master_volume_source ?? ""}
              onVolumeChange={setVolume}
              onPersistVolume={(v) => void persist({ master_volume: v })}
              onTone={() => void onTone()}
            />
          </div>
          {pairing && (
            <PairingPrompt
              address={pairing.address}
              name={pairing.name}
              canAct={canAct}
              busy={pairingBusy}
              note={pairingNote}
              error={pairingError}
              onPair={() => void onPair()}
            />
          )}
          {saveNote && (
            <p className="muted" role="status" data-testid="audio-output-note">
              {saveNote}
            </p>
          )}
          {!canEdit && (
            <p className="muted">
              The device and master volume persist for an administrator; the test tone is available
              to the operator.
            </p>
          )}
          {tone && (
            <p className="muted" role="status" data-testid="audio-tone-result">
              tone played through {tone.sink}
              {tone.evidence_file ? ` — evidence: ${tone.evidence_file}` : ""} ({tone.duration_ms}{" "}
              ms on {tone.device})
            </p>
          )}
          {toneError && (
            <p role="alert" className="error" data-testid="audio-tone-error">
              {toneError}
            </p>
          )}
        </div>
      )}
    </Card>
  );
}
