/**
 * `src/pages/play/OperatorFrame.tsx` — the operator view's 4:3 live frame
 * (docs/design/plum-peach 2f, ADR-0093) and the UX-11 half of Play: the
 * HDMI signal state and the active-picture preview from the capture API
 * (ADR-0110).
 *
 * The frame is `GET /api/v1/capture/preview` (a v4l2 session's newest
 * in-memory frame; otherwise a one-shot grab) and the status chips are
 * `GET /api/v1/capture/signal` (signal present/absent, the active-picture
 * judgment, the measured fps, the newest frame's age). A replay or
 * test-pattern run holds no card, so the one-shot path answers against the
 * card — on a machine with no capture device the frame area says so with
 * the route's own operator action (409), never a fake picture. The
 * dialogue-region ROI (coral box) is the signal's `dialogue_crop`; the
 * name-plate box is the design's dashed second region of interest.
 *
 * Geometry is SVG attributes (the kit's pattern, ADR-0011: no inline
 * styles); the crop arrives in frame pixels and the negotiated format's
 * dimensions (parsed from the signal's `mode`, "640x480 MJPEG @ 10 fps")
 * convert it to the overlay's viewBox units.
 */
import { useEffect, useRef, useState } from "react";

import { Chip } from "../../components/kit/Chip";
import { fetchCapturePreview, fetchCaptureSignal, type CaptureSignal } from "../../api/capture";
import { ApiError } from "../../api/client";
import type { CommittedLine } from "../../lib/playReducer";
import { probabilityText } from "./shared";

/** The operator's poll cadence: the server throttles the one-shot path to
 *  500 ms apart and session-mode answers are free; 2.5 s is a review view. */
const POLL_MS = 2_500;

interface FrameState {
  signal: CaptureSignal | null;
  previewUrl: string | null;
  /** The route's own problem text (the 409's operator action, a 503, …). */
  problem: string | null;
  /** The preview's own problem while the signal still answers (no frame yet). */
  previewProblem: string | null;
}

/** "640x480 MJPEG @ 10 fps" → [640, 480]; the ROI's pixel→viewBox conversion. */
function frameDimensions(mode: string): [number, number] | null {
  const match = /^(\d+)x(\d+)/.exec(mode);
  if (!match) return null;
  const width = Number(match[1]);
  const height = Number(match[2]);
  return width > 0 && height > 0 ? [width, height] : null;
}

export function OperatorFrame({ line }: { line: CommittedLine | null }) {
  const [state, setState] = useState<FrameState>({
    signal: null,
    previewUrl: null,
    problem: null,
    previewProblem: null,
  });
  const stateRef = useRef(state);
  stateRef.current = state;
  const inflight = useRef(false);
  const round = useRef(0);

  useEffect(() => {
    let disposed = false;
    let timer: number | undefined;
    let objectUrl: string | null = null;

    const tick = async () => {
      if (inflight.current || disposed) return;
      inflight.current = true;
      // The server throttles the one-shot path to a single in-flight capture
      // shared by the signal and the preview (500 ms minimum apart). Firing
      // the pair in parallel loses one of every pair — and the signal, fired
      // first, wins the free slot every time, starving the preview (its
      // one-shot grab never runs). Alternating which one leads and spacing
      // the two past the throttle's minimum interval gives each endpoint the
      // free slot on alternating ticks.
      const previewFirst = round.current % 2 === 1;
      round.current += 1;
      try {
        const base = stateRef.current;
        let signal = base.signal;
        let problem = base.problem;
        let previewUrl = base.previewUrl;
        let previewProblem = base.previewProblem;

        const doSignal = async () => {
          try {
            signal = await fetchCaptureSignal();
            problem = null;
          } catch (reason) {
            if (reason instanceof ApiError && reason.status === 429) {
              // Throttled: keep the last answer, retry on the next tick.
            } else if (reason instanceof ApiError) {
              signal = null;
              problem = `${reason.status} ${reason.detail}`;
            }
          }
        };
        const doPreview = async () => {
          try {
            const frame = await fetchCapturePreview();
            const url = URL.createObjectURL(frame);
            if (objectUrl !== null) URL.revokeObjectURL(objectUrl);
            objectUrl = url;
            previewUrl = url;
            previewProblem = null;
          } catch (reason) {
            if (reason instanceof ApiError && reason.status === 429) {
              // Throttled: keep the last frame on screen.
            } else if (reason instanceof ApiError) {
              previewProblem = `${reason.status} ${reason.detail}`;
            }
            // A failed grab keeps the last frame on screen (a 409 "no frame
            // yet" over a live session is progress, not a fault to blank out).
          }
        };

        if (previewFirst) {
          await doPreview();
          await new Promise((resolve) => {
            window.setTimeout(resolve, 600); // the shared throttle's 500 ms floor
          });
          await doSignal();
        } else {
          await doSignal();
          await new Promise((resolve) => {
            window.setTimeout(resolve, 600);
          });
          await doPreview();
        }

        if (!disposed) setState({ signal, previewUrl, problem, previewProblem });
      } finally {
        inflight.current = false;
        if (!disposed) timer = window.setTimeout(() => void tick(), POLL_MS);
      }
    };
    void tick();

    return () => {
      disposed = true;
      window.clearTimeout(timer);
      if (objectUrl !== null) URL.revokeObjectURL(objectUrl);
    };
    // The first tick reads the current state; the poll chain is self-sustaining.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const { signal, previewUrl, problem, previewProblem } = state;
  const dimensions = signal ? frameDimensions(signal.mode) : null;
  const crop = signal?.dialogue_crop;
  const hasCrop = crop !== undefined && dimensions !== null;
  const cropBox = hasCrop
    ? {
        x: (crop.x / dimensions[0]) * 100,
        y: (crop.y / dimensions[1]) * 75,
        width: (crop.w / dimensions[0]) * 100,
        height: (crop.h / dimensions[1]) * 75,
      }
    : null;
  const ocrLabel =
    line?.confidence !== null && line?.confidence !== undefined
      ? `textbox · OCR ${probabilityText(line.confidence)}`
      : "textbox";
  const fpsLabel =
    signal && signal.fps_actual !== null
      ? `fps ${round1(signal.fps_actual)}/${signal.fps_target || "–"}`
      : "fps —";

  return (
    <div data-testid="play-operator-frame">
      <div className="pp-frame">
        {previewUrl ? (
          <img
            className="pp-frame-img"
            src={previewUrl}
            alt="Live capture frame (the picture the operator sees)"
            data-testid="play-frame-image"
          />
        ) : null}
        {!previewUrl ? (
          <div className="pp-frame-empty" data-testid="play-frame-unavailable">
            <p data-testid="play-frame-unavailable-reason">
              {problem ?? previewProblem ?? "no frame yet"}
            </p>
          </div>
        ) : null}
        {previewUrl ? (
          <svg
            className="pp-frame-overlay"
            viewBox="0 0 100 75"
            preserveAspectRatio="none"
            aria-hidden="true"
            data-testid="play-frame-overlay"
          >
            {cropBox ? (
              <g data-testid="play-roi-textbox">
                <rect
                  className="pp-frame-roi"
                  x={cropBox.x}
                  y={cropBox.y}
                  width={cropBox.width}
                  height={cropBox.height}
                />
                <rect
                  className="pp-frame-roi-label-bg"
                  x={cropBox.x}
                  y={Math.max(0, cropBox.y - 4.4)}
                  width={ocrLabel.length * 1.75 + 1.5}
                  height={3.6}
                />
                <text className="pp-frame-roi-label" x={cropBox.x + 0.6} y={cropBox.y - 1.7}>
                  {ocrLabel}
                </text>
              </g>
            ) : null}
            <g data-testid="play-roi-nameplate">
              <rect className="pp-frame-nameplate" x={5} y={60} width={24} height={10} />
              <text className="pp-frame-nameplate-label" x={7} y={66.5}>
                name plate
              </text>
            </g>
          </svg>
        ) : null}
      </div>

      <div className="pp-frame-status" data-testid="play-frame-status">
        <Chip tone={signal?.signal ? "peach" : "bad"} testid="play-signal-state">
          {signal ? (signal.signal ? "signal present" : "no signal") : "signal unknown"}
        </Chip>
        <Chip tone={signal?.picture.active ? "peach" : "bad"} testid="play-picture-state">
          {signal
            ? signal.picture.active
              ? "active picture"
              : "no active picture"
            : "picture unknown"}
        </Chip>
        <Chip tone="neutral" testid="play-fps">
          {signal ? fpsLabel : "fps —"}
        </Chip>
        {signal ? (
          <Chip tone="neutral" testid="play-frame-age">
            frame {signal.age_ms}ms old
          </Chip>
        ) : null}
        {signal?.synthetic ? (
          <Chip tone="neutral" testid="play-frame-synthetic">
            synthetic source
          </Chip>
        ) : null}
        {signal ? (
          <Chip tone="neutral" testid="play-frame-source">
            {signal.source}
          </Chip>
        ) : null}
      </div>
    </div>
  );
}

function round1(value: number): string {
  return (Math.round(value * 10) / 10).toString();
}
