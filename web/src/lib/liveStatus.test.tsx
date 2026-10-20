import { act, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { ApiError } from "../api/client";
import {
  STATUS_PLACEHOLDER,
  STATUS_STREAM_PATH,
  LiveStatusProvider,
  parseStatusFrame,
  useLiveStatus,
  type EventSourceLike,
  type StatusSnapshot,
} from "./liveStatus";

type FetchSnapshot = (signal?: AbortSignal) => Promise<StatusSnapshot>;

/**
 * The status feed's contract (plan.md Phase 0 item 5): the SSE frame
 * updates the bar's text (a setup progress tick moves it), a refused
 * stream falls back to the snapshot read, an anonymous 401 shows the
 * neutral placeholder, and a malformed frame is dropped. The feed's seams
 * (eventSourceFactory, fetchSnapshot) make the stream and the read
 * scriptable without a network.
 */

class FakeEventSource implements EventSourceLike {
  readyState = 0; // CONNECTING
  onmessage: ((event: MessageEvent) => void) | null = null;
  onstatus: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;
  closed = false;

  constructor(public readonly url: string) {
    expect(url).toBe(STATUS_STREAM_PATH);
  }

  close(): void {
    this.closed = true;
    this.readyState = 2; // CLOSED
  }

  /** Emit a named status.changed frame (the wire's event name). */
  emit(data: string): void {
    if (this.onstatus !== null) this.onstatus({ data } as MessageEvent);
  }

  /** The browser's refusal (401/403/404): readyState CLOSED, no retry. */
  refuse(): void {
    this.readyState = 2;
    if (this.onerror !== null) this.onerror(new Event("error"));
  }
}

function snapshot(overrides: Partial<StatusSnapshot>): StatusSnapshot {
  return {
    updated_at: "2026-10-20T00:00:00Z",
    status: "5 ok · 1 slow",
    health: { components: [], ok: 5, slow: 1, fail: 0, unknown: 0, detail: "5 ok · 1 slow" },
    device: { version: "v0.9.2", uptime_ms: 0 },
    ...overrides,
  };
}

function session(
  overrides: Partial<StatusSnapshot["session"]> = {},
): NonNullable<StatusSnapshot["session"]> {
  return {
    session_id: "s1",
    mode: "conservative",
    state: "running",
    paused: false,
    source: "v4l2",
    capture_fps: 30,
    frames_dropped: 0,
    lines_spoken: 41,
    frame_to_voice_p50_ms: 412,
    frame_to_voice_samples: 41,
    ocr_calls: 500,
    ocr_errors: 0,
    elapsed_ms: 0,
    created_at: "2026-10-20T00:00:00Z",
    ...overrides,
  };
}

function Probe() {
  const { text, source, snapshot: snap, error } = useLiveStatus();
  return (
    <output>
      <span data-testid="bar-text">{text}</span>
      <span data-testid="bar-source">{source}</span>
      <span data-testid="bar-voiced">{snap?.setup_runs?.[0]?.voiced ?? ""}</span>
      <span data-testid="bar-error">{error ?? ""}</span>
    </output>
  );
}

function renderProbe(feed?: { stream?: FakeEventSource; fetchSnapshot?: FetchSnapshot }) {
  const stream = feed?.stream ?? new FakeEventSource(STATUS_STREAM_PATH);
  const fetchSnapshot: FetchSnapshot =
    feed?.fetchSnapshot ?? (vi.fn(async () => snapshot({})) as unknown as FetchSnapshot);
  render(
    <LiveStatusProvider
      feed={{
        eventSourceFactory: () => stream,
        fetchSnapshot,
        pollIntervalMs: 60_000, // long: the test drives the ticks, not the timer
      }}
    >
      <Probe />
    </LiveStatusProvider>,
  );
  return { stream, fetchSnapshot };
}

describe("parseStatusFrame", () => {
  it("parses the wire shape (SSE data and GET body share it)", () => {
    const frame = parseStatusFrame(JSON.stringify(snapshot({ status: "● live" })));
    expect(frame?.status).toBe("● live");
  });

  it("parses the stream's event envelope (the snapshot under envelope.data)", () => {
    const envelope = {
      schema_version: 1,
      event_id: "01M5CDWFR01TXCQTQQDZ0YK49V",
      seq: 1,
      name: "status.changed",
      occurred_at: "2026-10-20T10:32:02.560Z",
      data: { ...snapshot({ status: "1 ok · 5 unknown" }), session: undefined },
    };
    const frame = parseStatusFrame(JSON.stringify(envelope));
    expect(frame?.status).toBe("1 ok · 5 unknown");
  });

  it("drops malformed frames", () => {
    expect(parseStatusFrame("not json")).toBeNull();
    expect(parseStatusFrame(JSON.stringify({ health: {} }))).toBeNull();
    // an envelope whose payload is not a status snapshot
    expect(
      parseStatusFrame(JSON.stringify({ name: "message.committed", data: { line: 1 } })),
    ).toBeNull();
  });
});

describe("LiveStatusProvider", () => {
  it("renders the server's canonical string and follows a setup progress tick", async () => {
    const { stream } = renderProbe();

    expect(screen.getByTestId("bar-text").textContent).toBe(STATUS_PLACEHOLDER);

    act(() => {
      stream.emit(
        JSON.stringify(
          snapshot({
            status: "● agent running · 9 of 12 voiced · 2m 04s",
            setup_runs: [
              { run_id: "run-1", state: "running", voiced: 9, characters: 12, elapsed_ms: 0 },
            ],
          }),
        ),
      );
    });
    expect(screen.getByTestId("bar-text").textContent).toBe(
      "● agent running · 9 of 12 voiced · 2m 04s",
    );
    expect(screen.getByTestId("bar-source").textContent).toBe("sse");
    expect(screen.getByTestId("bar-voiced").textContent).toBe("9");

    act(() => {
      stream.emit(
        JSON.stringify(
          snapshot({
            status: "● agent running · 10 of 12 voiced · 3m 04s",
            setup_runs: [
              { run_id: "run-1", state: "running", voiced: 10, characters: 12, elapsed_ms: 0 },
            ],
          }),
        ),
      );
    });
    await waitFor(() => expect(screen.getByTestId("bar-text").textContent).toContain("10 of 12"));
    expect(screen.getByTestId("bar-voiced").textContent).toBe("10");
  });

  it("shows a live session's fps and latency when the snapshot carries them", async () => {
    const { stream } = renderProbe();
    act(() => {
      stream.emit(
        JSON.stringify(
          snapshot({
            status: "● live · capture 30fps · 412ms frame→voice",
            session: session({}),
          }),
        ),
      );
    });
    expect(screen.getByTestId("bar-text").textContent).toBe(
      "● live · capture 30fps · 412ms frame→voice",
    );
  });

  it("falls back to the snapshot read when the stream is refused", async () => {
    const fetchSnapshot = vi.fn(async () => snapshot({ status: "6 unknown" }));
    const { stream, fetchSnapshot: read } = renderProbe({
      stream: new FakeEventSource(STATUS_STREAM_PATH),
      fetchSnapshot,
    });
    expect(read).toBe(fetchSnapshot);

    act(() => {
      stream.refuse();
    });
    await waitFor(() => expect(screen.getByTestId("bar-text").textContent).toBe("6 unknown"));
    expect(screen.getByTestId("bar-source").textContent).toBe("poll");
    expect(fetchSnapshot).toHaveBeenCalledTimes(1);
  });

  it("shows the neutral placeholder when the box is anonymous (401)", async () => {
    const unauthenticated = new ApiError({
      status: 401,
      code: "UNAUTHENTICATED",
      title: "Unauthorized",
      detail: "a session cookie or an API token is required",
    });
    const fetchSnapshot = vi.fn(async () => {
      throw unauthenticated;
    });
    const { stream } = renderProbe({ fetchSnapshot });

    act(() => {
      stream.refuse();
    });
    await waitFor(() => expect(fetchSnapshot).toHaveBeenCalled());
    expect(screen.getByTestId("bar-text").textContent).toBe(STATUS_PLACEHOLDER);
    expect(screen.getByTestId("bar-source").textContent).toBe("none");
    // The anonymous state is not an error: the fault line stays empty.
    expect(screen.getByTestId("bar-error").textContent).toBe("");
  });

  it("records non-401 faults without blanking the bar", async () => {
    const fetchSnapshot = vi
      .fn()
      .mockResolvedValueOnce(snapshot({ status: "5 ok" }))
      .mockRejectedValue(
        new ApiError({
          status: 503,
          code: "INTERNAL",
          title: "Service Unavailable",
          detail: "boom",
        }),
      );
    // A short poll cadence so the second tick (the fault) lands inside the test.
    const stream = new FakeEventSource(STATUS_STREAM_PATH);
    render(
      <LiveStatusProvider
        feed={{
          eventSourceFactory: () => stream,
          fetchSnapshot,
          pollIntervalMs: 30,
        }}
      >
        <Probe />
      </LiveStatusProvider>,
    );

    act(() => {
      stream.refuse();
    });
    await waitFor(() => expect(screen.getByTestId("bar-text").textContent).toBe("5 ok"));
    await waitFor(() => expect(screen.getByTestId("bar-error").textContent).toBe("503 boom"));
    // The fault records but the last frame stays on the bar.
    expect(screen.getByTestId("bar-text").textContent).toBe("5 ok");
  });

  it("drops a malformed SSE frame instead of rendering it", async () => {
    const { stream } = renderProbe();
    act(() => {
      stream.emit("not a snapshot");
    });
    expect(screen.getByTestId("bar-text").textContent).toBe(STATUS_PLACEHOLDER);
  });
});
