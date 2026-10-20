import { useAuth } from "../../auth/auth-context";
import { signalWords, useCapturePreview, useCaptureSignal } from "./signal";

/**
 * `src/pages/health/signal-block.tsx` — the capture tile's UX-11 block
 * (ADR-0111 §3): the two status words with their testids, the JPEG picture
 * when one arrives, and the honest states the route's contract names
 * (sign-in/role hint, no capture layer, the 409's operator action).
 * Rendered inside the capture tile by the grid. The polling loops it drives
 * live in ./signal.tsx.
 */
export function CaptureSignalBlock({ active }: { active: boolean }) {
  const { state: authState } = useAuth();
  const signedIn = authState === "authenticated";
  const state = useCaptureSignal(active && signedIn);
  const preview = useCapturePreview(active && signedIn && state.kind === "ok" && state.body.signal);

  if (!signedIn) {
    return (
      <p className="muted" data-testid="health-tile-capture-signal-sign-in">
        The HDMI signal read needs a signed-in session (operator role or higher).{" "}
        <a href="/login">Sign in</a> to see it.
      </p>
    );
  }
  if (state.kind === "loading") {
    return (
      <p className="muted" data-testid="health-tile-capture-signal-loading">
        Reading the HDMI signal…
      </p>
    );
  }
  if (state.kind === "unauthenticated") {
    return (
      <p className="muted" data-testid="health-tile-capture-signal-sign-in">
        The HDMI signal read needs a signed-in session. <a href="/login">Sign in</a> to see it.
      </p>
    );
  }
  if (state.kind === "forbidden") {
    return (
      <p className="muted" data-testid="health-tile-capture-signal-role">
        Reading the HDMI signal needs the operator role or higher; this session does not hold it.
      </p>
    );
  }
  if (state.kind === "unavailable") {
    return (
      <p className="muted" data-testid="health-tile-capture-signal-unavailable">
        No capture layer is wired into this process — no UVC HDMI capture device is attached (
        {state.detail}). The honest state for a machine without the dongle.
      </p>
    );
  }
  if (state.kind === "conflict") {
    return (
      <p className="muted" data-testid="health-tile-capture-signal-conflict">
        {state.detail}
      </p>
    );
  }
  if (state.kind === "error") {
    return (
      <div role="alert" className="error" data-testid="health-tile-capture-signal-error">
        <p>The signal read failed: {state.detail}</p>
      </div>
    );
  }

  const words = signalWords(state.body);
  return (
    <div className="health-signal" data-testid="health-tile-capture-signal-block">
      <div className="health-signal-words">
        <span data-testid="health-tile-capture-signal">{words.signal}</span>
        <span data-testid="health-tile-capture-picture">{words.picture}</span>
      </div>
      {preview.url !== null ? (
        <img
          src={preview.url}
          alt="Capture preview: the newest frame from the HDMI input"
          className="health-preview"
          data-testid="health-tile-capture-preview"
        />
      ) : (
        <div
          className="health-preview health-preview-waiting"
          data-testid="health-tile-capture-preview"
        >
          {preview.failed ? "no preview yet" : "…"}
        </div>
      )}
      <p className="muted health-signal-meta">
        {state.body.source === "session" ? "from the running session" : "one-shot frame"}
        {state.body.fps_actual !== null && state.body.fps_actual !== undefined
          ? ` · ${state.body.fps_actual.toFixed(1)} fps`
          : ""}
        {state.body.synthetic ? " · synthetic seam (labelled, not hardware)" : ""}
      </p>
    </div>
  );
}
