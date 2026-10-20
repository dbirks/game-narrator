import { useCallback, useEffect, useState } from "react";

import { GatedError, type GatedState } from "../../api/diagnostics";

/**
 * `src/pages/health/gated.tsx` — the Health screen's gated-read contract,
 * split out of the preserved sections (ADR-0111 §1): the mapping of a
 * GatedError onto the page's renderable states, the load/retry hook, and
 * the sign-in / role hint a gated section renders for 401 and 403.
 *
 * The honesty rule the old /diagnostics page carried: 401 becomes a "sign
 * in" hint, 403 a "this needs a higher role" hint — never a fabricated row.
 */

/** Map a GatedError onto the page's renderable states. */
export function mapGated<T>(error: unknown): GatedState<T> {
  if (error instanceof GatedError) {
    if (error.status === 401) return { kind: "unauthenticated" };
    if (error.status === 403) return { kind: "forbidden" };
    return { kind: "error", status: error.status, detail: error.detail };
  }
  return {
    kind: "error",
    status: 0,
    detail: error instanceof Error ? error.message : "network error",
  };
}

/**
 * One of the page's gated reads, as a load/retry state. `load` must be
 * stable (a module-level function) so the effect does not re-run on every
 * render; `refetch` re-runs it. Aborts on unmount and on re-run, so a
 * section the operator left is not still fetching.
 */
export function useGated<T>(
  load: (signal: AbortSignal) => Promise<T>,
  enabled: boolean,
): { state: GatedState<T>; refetch: () => void } {
  const [nonce, setNonce] = useState(0);
  const [state, setState] = useState<GatedState<T>>({ kind: "loading" });

  useEffect(() => {
    if (!enabled) return;
    const controller = new AbortController();
    let cancelled = false;
    setState({ kind: "loading" });
    load(controller.signal)
      .then((body) => {
        if (!cancelled) setState({ kind: "ok", body });
      })
      .catch((error) => {
        if (!cancelled && !controller.signal.aborted) setState(mapGated(error));
      });
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [load, enabled, nonce]);

  const refetch = useCallback(() => {
    setNonce((n) => n + 1);
  }, []);
  return { state, refetch };
}

/** The sign-in / role hint a gated section renders for 401 and 403. */
export function GatedHint({
  state,
  action,
  refetch,
  needs,
}: {
  state: GatedState<unknown>;
  action: string;
  refetch: () => void;
  needs?: string;
}) {
  if (state.kind === "unauthenticated") {
    return (
      <p className="muted" data-testid={`${action}-sign-in`}>
        {action} needs a signed-in session. <a href="/login">Sign in</a> to see this section.
      </p>
    );
  }
  if (state.kind === "forbidden") {
    return (
      <p className="muted" data-testid={`${action}-role`}>
        {action} is available to the {needs ?? "operator or higher"} role. Sign in with a matching
        account.
      </p>
    );
  }
  if (state.kind === "error") {
    return (
      <div role="alert" className="error" data-testid={`${action}-error`}>
        <p>
          {action}: {state.detail} (HTTP {state.status})
        </p>
        <button type="button" onClick={refetch}>
          Try again
        </button>
      </div>
    );
  }
  return <p className="muted">{action}…</p>;
}

// ── section loaders (module-level: stable references for useGated) ─────────
