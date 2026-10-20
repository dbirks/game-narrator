import type { CaptureProbe } from "../../api/capture";

/**
 * The live preview of the "Capture" card (ADR-0078, ADR-0109 restyle): one
 * throttled JPEG per poll (the browser's `<img>` is the loop; the server
 * rate-limits and refuses while a session holds the device). The last good
 * frame is kept on a refused poll; a failed poll surfaces its error.
 */
export function CapturePreviewPane({
  on,
  url,
  error,
  pollMs,
}: {
  on: boolean;
  url: string | null;
  error: string | null;
  pollMs: number;
}) {
  if (!on) return null;
  return (
    <div data-testid="capture-preview">
      {url ? (
        <img
          src={url}
          alt="Capture device preview"
          data-testid="capture-preview-img"
          style={{ maxWidth: "480px", border: "1px solid currentColor" }}
        />
      ) : (
        !error && (
          <p className="muted" data-testid="capture-preview-pending">
            Taking the first snapshot…
          </p>
        )
      )}
      {error && (
        <p className="error" role="alert" data-testid="capture-preview-error">
          {error}
        </p>
      )}
      <p className="muted">
        The preview is one throttled snapshot per {pollMs / 1000} s and is refused while a session
        holds this device (the UVC bus is single-consumer).
      </p>
    </div>
  );
}

/**
 * The capture test's answer (docs/spec/10): grabbed/dropped/fps/freshness
 * plus the OCR the probe ran over its region, degrading to the engine's
 * install action when it is missing. The probe's error, when the run failed.
 */
export function CaptureProbePane({
  probe,
  error,
}: {
  probe: CaptureProbe | null;
  error: string | null;
}) {
  return (
    <>
      {probe && (
        <div data-testid="capture-probe-result">
          <p>
            {probe.device} · {probe.mode} ({probe.mode_source}) — grabbed {probe.grabbed}/
            {probe.requested} · dropped {probe.dropped} · {probe.fps} fps · freshness{" "}
            {probe.freshness_ms} ms · state {probe.state}
            {probe.last_error ? ` — ${probe.last_error}` : ""}
          </p>
          {probe.ocr ? (
            <p data-testid="capture-probe-ocr">
              OCR ({probe.ocr.engine}) on {probe.ocr.region.x},{probe.ocr.region.y}{" "}
              {probe.ocr.region.w}x{probe.ocr.region.h}:
              {probe.ocr.text ? ` “${probe.ocr.text}”` : " (no text read)"}
              {probe.ocr.note ? ` — ${probe.ocr.note}` : ""}
            </p>
          ) : null}
        </div>
      )}
      {error && (
        <p className="error" role="alert" data-testid="capture-probe-error">
          {error}
        </p>
      )}
    </>
  );
}
