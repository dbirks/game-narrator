import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError, api } from "../../api/client";
import { fetchCapturePreview } from "../../api/capture";
import type { components } from "../../api/schema";

/**
 * `src/pages/health/signal.tsx` — the capture tile's UX-11 block
 * (docs/design/plum-peach UX-11; ADR-0110 §1/§2; ADR-0111 §3).
 *
 * The two halves the requirement names:
 *
 * - **signal present / no signal** from `GET /api/v1/capture/signal`
 *   (the route the operator tier carries: a running v4l2 session answers
 *   from memory, otherwise a one-shot grab through the throwaway path;
 *   409 `capture-signal-unavailable` is the "no session" state with the
 *   operator action in the detail, 503 the "no capture layer" box);
 * - **active picture / no active picture** from the answer's
 *   `picture.active` (the tracker's non-black judgment on the newest
 *   frame — a card delivering a black pattern is signal PRESENT, the
 *   picture half is what says "no active picture"), and the throttled
 *   640 px JPEG from `GET /api/v1/capture/preview` as the tile's picture.
 *
 * The reads share the server's one-shot throttle (one in flight, 500 ms
 * apart), so the page polls at 2.5 s — inside the budget with margin —
 * only while signed in, only while the health tab is on screen, and
 * aborts on unmount; object URLs are revoked on replace and unmount.
 */

export type CaptureSignalView = components["schemas"]["CaptureSignal"];

const SIGNAL_PATH = "/api/v1/capture/signal";
/** Inside the server's 500 ms one-shot throttle with margin (ADR-0110 §1). */
const POLL_MS = 2_500;

export type SignalState =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "forbidden" }
  | { kind: "unavailable"; detail: string }
  | { kind: "conflict"; detail: string }
  | { kind: "error"; detail: string }
  | { kind: "ok"; body: CaptureSignalView };

/** The preview's first-tick offset: a half-period behind the signal's tick,
 *  so the two reads that share the server's 500 ms one-shot throttle land
 *  1.25 s apart, not in the same window (the 429 the shared throttle
 *  answers is non-fatal, but a read that never 429s is the calm state). */
const PREVIEW_FIRST_TICK_MS = POLL_MS / 2;

/**
 * The signal read's polling loop (ADR-0111 §3): one read at a time (an
 * in-flight read is not re-armed), 2.5 s cadence, gated on `active`
 * (signed in + the health tab), aborted on unmount. A 429 (the shared
 * throttle's answer) is not a fault: the next tick retries.
 */
export function useCaptureSignal(active: boolean): SignalState {
  const [state, setState] = useState<SignalState>({ kind: "loading" });

  useEffect(() => {
    if (!active) return;
    const stopping = { flag: false };
    let timer: number | undefined;
    let inFlight: AbortController | null = null;

    const schedule = () => {
      if (stopping.flag) return;
      timer = window.setTimeout(tick, POLL_MS);
    };

    const tick = () => {
      if (stopping.flag) return;
      inFlight = new AbortController();
      void api.get(SIGNAL_PATH, { signal: inFlight.signal }).then(
        (body) => {
          if (stopping.flag) return;
          setState({ kind: "ok", body: body as unknown as CaptureSignalView });
          schedule(); // the signal moves (a session starts or ends): keep watching
        },
        (error: unknown) => {
          if (stopping.flag || (error instanceof Error && error.name === "AbortError")) return;
          if (error instanceof ApiError) {
            switch (error.status) {
              case 401:
                setState({ kind: "unauthenticated" });
                return; // a 401 will not go away on the next tick
              case 403:
                setState({ kind: "forbidden" });
                return;
              case 503:
                // no capture layer is a box property, not a tick's
                setState({ kind: "unavailable", detail: error.detail });
                return;
              case 409:
                // "no session" (ADR-0110 §1): keep the operator action on
                // screen but stay on the cadence — a session may start.
                setState({ kind: "conflict", detail: error.detail });
                break;
              case 429:
                break; // the shared throttle: the state holds, the tick retries
              default:
                setState({ kind: "error", detail: `${error.status} ${error.detail}` });
            }
          } else {
            setState({
              kind: "error",
              detail: error instanceof Error ? error.message : "network error",
            });
          }
          schedule();
        },
      );
    };

    tick();
    return () => {
      stopping.flag = true;
      inFlight?.abort();
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [active]);

  return state;
}

/**
 * The preview picture's polling loop (ADR-0111 §3): a JPEG through the
 * production encoder at the same cadence; the object URL is revoked on
 * replace and on unmount (the frame is served from memory, never stored —
 * the client must not hold it past the tile either).
 */
export function useCapturePreview(active: boolean): { url: string | null; failed: boolean } {
  const [url, setUrl] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  const holder = useRef<string | null>(null);

  const replace = useCallback((next: string | null) => {
    if (holder.current !== null) URL.revokeObjectURL(holder.current);
    holder.current = next;
    setUrl(next);
  }, []);

  useEffect(() => {
    if (!active) return;
    const stopping = { flag: false };
    let timer: number | undefined;
    let inFlight = new AbortController();

    const tick = () => {
      if (stopping.flag) return;
      inFlight = new AbortController();
      fetchCapturePreview(undefined, inFlight.signal)
        .then((blob) => {
          if (stopping.flag) return;
          replace(URL.createObjectURL(blob));
          setFailed(false);
        })
        .catch((error: unknown) => {
          if (stopping.flag || (error instanceof Error && error.name === "AbortError")) return;
          // A 429 (the shared throttle) or a 409 (no frame yet) is not a
          // fault: the next tick retries; anything else is the honest fail.
          if (error instanceof ApiError && (error.status === 429 || error.status === 409)) return;
          setFailed(true);
        });
      timer = window.setTimeout(tick, POLL_MS);
    };

    // The half-period offset (PREVIEW_FIRST_TICK_MS) keeps this read's ticks
    // 1.25 s away from the signal read's, so neither lands inside the
    // shared throttle's 500 ms window.
    timer = window.setTimeout(tick, PREVIEW_FIRST_TICK_MS);
    return () => {
      stopping.flag = true;
      inFlight.abort();
      if (timer !== undefined) window.clearTimeout(timer);
      replace(null);
    };
  }, [active, replace]);

  return { url, failed };
}

/** The UX-11 status words (ADR-0111 §3): the exact strings the spec's
 *  acceptance asserts by data-testid. */
export function signalWords(body: CaptureSignalView): { signal: string; picture: string } {
  return {
    signal: body.signal ? "signal present" : "no signal",
    picture: body.picture?.active ? "active picture" : "no active picture",
  };
}
