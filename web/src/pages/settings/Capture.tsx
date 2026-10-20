import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";

import {
  fetchCaptureDevices,
  fetchCapturePreview,
  runCaptureProbe,
  selectCaptureDevice,
  type CaptureProbe,
} from "../../api/capture";
import { apiStatus } from "../../api/client";
import { Card } from "../../components/kit";
import { CaptureDeviceTable, modeText } from "./CaptureDeviceTable";
import { CapturePreviewPane, CaptureProbePane } from "./CapturePreview";

/**
 * `/settings` "Capture" (ADR-0078, docs/spec/08/10, ADR-0109 restyle): the
 * UVC HDMI capture dongle as a product surface. The selection persists
 * (device always, the mode when pinned; omitting it records "the policy
 * decides"). The preview is one throttled JPEG per poll (the browser's
 * `<img>` is the loop; the server rate-limits and refuses while a session
 * holds the device), and the capture test is the docs/spec/10 probe:
 * grabbed/dropped/fps/freshness plus OCR over a region, degrading to the
 * engine's install action when it is missing.
 *
 * The test seam (GAME_NARRATOR_CAPTURE_FAKE) is LABELLED synthetic in the
 * DOM — the same rule ADR-0074 applies to the audio list — so a fake device
 * is never mistaken for the hardware of this machine.
 */
const PREVIEW_POLL_MS = 2000;

export function CaptureSection() {
  const query = useQuery({
    queryKey: ["capture-devices"],
    queryFn: ({ signal }) => fetchCaptureDevices(signal),
    retry: false,
  });
  const [selectedId, setSelectedId] = useState<string>("");
  const [modePin, setModePin] = useState<string>("");
  const [saving, setSaving] = useState(false);
  const [saveNote, setSaveNote] = useState<string | null>(null);
  const [previewOn, setPreviewOn] = useState(false);
  const [previewUrl, setPreviewUrl] = useState<string | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [probe, setProbe] = useState<CaptureProbe | null>(null);
  const [probeBusy, setProbeBusy] = useState(false);
  const [probeError, setProbeError] = useState<string | null>(null);

  const view = query.data;
  // The operator's current pick wins the picker's initial value; the answer's
  // selected_id is the persisted selection a session without an explicit
  // device runs, so the page shows what a session will use.
  useEffect(() => {
    if (view && view.selected_id && !selectedId) setSelectedId(view.selected_id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view]);

  // The live preview: one snapshot per poll while on. The server is the
  // throttle (one in flight, 500 ms apart; 429 when exceeded), the page just
  // asks at a polite cadence and keeps the last good frame on a refused poll.
  useEffect(() => {
    if (!previewOn) return;
    let live = true;
    let timer: number | undefined;
    const tick = async () => {
      try {
        const blob = await fetchCapturePreview(selectedId || undefined);
        if (!live) {
          URL.revokeObjectURL(URL.createObjectURL(blob));
          return;
        }
        const url = URL.createObjectURL(blob);
        setPreviewUrl((old) => {
          if (old) URL.revokeObjectURL(old);
          return url;
        });
        setPreviewError(null);
      } catch (error) {
        if (live)
          setPreviewError(
            error instanceof Error
              ? error.message
              : `the preview answered ${apiStatus(error) ?? "?"}`,
          );
      } finally {
        if (live) timer = window.setTimeout(tick, PREVIEW_POLL_MS);
      }
    };
    void tick();
    return () => {
      live = false;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [previewOn, selectedId]);

  // Revoke the object URL when the section unmounts with the preview on.
  useEffect(
    () => () => {
      setPreviewUrl((old) => {
        if (old) URL.revokeObjectURL(old);
        return null;
      });
    },
    [],
  );

  const readyDevices = (view?.devices ?? []).filter((device) => device.state === "ready");
  const selected = view?.devices.find((device) => device.id === selectedId);

  const persist = async () => {
    if (saving || selectedId === "") return;
    setSaving(true);
    setSaveNote(null);
    try {
      const mode =
        modePin === "" || !selected?.chosen
          ? undefined
          : {
              w: selected.chosen.w,
              h: selected.chosen.h,
              fps: selected.chosen.fps,
              pix_fmt: selected.chosen.pix_fmt,
            };
      const saved = await selectCaptureDevice(selectedId, mode);
      setSaveNote(
        `saved — device ${saved.device_id}` +
          (saved.mode
            ? `, pinned mode ${modeText(saved.mode.w, saved.mode.h, saved.mode.fps, saved.mode.pix_fmt)}`
            : ", the policy decides the mode"),
      );
      await query.refetch();
    } catch (error) {
      setSaveNote(
        `not saved: ${error instanceof Error ? error.message : (apiStatus(error) ?? "")}`,
      );
    } finally {
      setSaving(false);
    }
  };

  const onProbe = async () => {
    if (probeBusy) return;
    setProbeBusy(true);
    setProbeError(null);
    try {
      const result = await runCaptureProbe(selectedId ? { device: selectedId } : {});
      setProbe(result);
    } catch (error) {
      setProbeError(
        error instanceof Error ? error.message : `the probe answered ${apiStatus(error) ?? "?"}`,
      );
    } finally {
      setProbeBusy(false);
    }
  };

  return (
    <Card title="Capture" testid="settings-capture">
      {query.error ? (
        <p className="muted" role="alert">
          The capture device list is unavailable:
          {query.error instanceof Error ? query.error.message : ""}
        </p>
      ) : !view ? (
        <p className="muted" data-testid="capture-loading">
          Reading the capture devices…
        </p>
      ) : (
        <div>
          {view.synthetic && (
            <p className="muted" role="status" data-testid="capture-synthetic">
              Synthetic device list (test seam): this is not the hardware of this machine.
              {view.note ? ` — ${view.note}` : ""}
            </p>
          )}
          {view.devices.length === 0 ? (
            <p className="muted" data-testid="capture-none">
              No capture device is attached: connect the UVC HDMI dongle and it will appear here
              (the list re-enumerates on the next read).
            </p>
          ) : (
            <CaptureDeviceTable devices={view.devices} />
          )}
          {readyDevices.length > 0 && (
            <div className="row" data-testid="capture-controls">
              <label htmlFor="capture-device-select">Device</label>
              <select
                id="capture-device-select"
                data-testid="capture-device-select"
                className="kit-select"
                value={selectedId}
                disabled={saving}
                onChange={(event) => {
                  setSelectedId(event.target.value);
                  setModePin("");
                  setProbe(null);
                }}
              >
                <option value="">choose a capture device…</option>
                {readyDevices.map((device) => (
                  <option key={device.id} value={device.id}>
                    {device.name} ({device.id})
                  </option>
                ))}
              </select>
              <label htmlFor="capture-mode-select">Mode</label>
              <select
                id="capture-mode-select"
                data-testid="capture-mode-select"
                value={modePin}
                disabled={!selectedId || saving || !selected?.chosen}
                onChange={(event) => setModePin(event.target.value)}
              >
                <option value="">the policy decides</option>
                {selected?.chosen ? (
                  <option value="chosen">
                    {modeText(
                      selected.chosen.w,
                      selected.chosen.h,
                      selected.chosen.fps,
                      selected.chosen.pix_fmt,
                    )}{" "}
                    (current pick)
                  </option>
                ) : null}
              </select>
              <button
                type="button"
                onClick={() => void persist()}
                disabled={!selectedId || saving}
                data-testid="capture-save"
              >
                {saving ? "Saving…" : "Save selection"}
              </button>
              <button
                type="button"
                onClick={() => setPreviewOn((on) => !on)}
                disabled={!view.devices.some((d) => d.state === "ready")}
                data-testid="capture-preview-toggle"
              >
                {previewOn ? "Stop preview" : "Show live preview"}
              </button>
              <button
                type="button"
                onClick={() => void onProbe()}
                disabled={!view.devices.some((d) => d.state === "ready") || probeBusy}
                data-testid="capture-probe"
              >
                {probeBusy ? "Running the capture test…" : "Run capture test"}
              </button>
            </div>
          )}
          {saveNote && (
            <p className="muted" role="status" data-testid="capture-note">
              {saveNote}
            </p>
          )}
          <CapturePreviewPane
            on={previewOn}
            url={previewUrl}
            error={previewError}
            pollMs={PREVIEW_POLL_MS}
          />
          <CaptureProbePane probe={probe} error={probeError} />
        </div>
      )}
    </Card>
  );
}
