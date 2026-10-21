/**
 * `src/pages/play/SetupForm.tsx` — the session start form (the pre-redesign
 * SetupForm, carried over: its logic, selectors and data-testids are the
 * contract m5-part1 / live-play / sse-reconnect drive, so the rework is the
 * page around it, not this form). One session at a time is a runtime
 * invariant, not a UI limit; the session id lands in the URL (`?session=`)
 * so a refresh reopens the same watch.
 */
import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router";

import {
  api,
  apiStatus,
  catalogApi,
  type Edition,
  type Game,
  type Profile,
} from "../../api/client";
import { fetchCaptureDevices } from "../../api/capture";
import { fetchAllMessages } from "../../api/profiles";
import { clipsApi } from "../../api/clips";
import { sessionsApi } from "../../api/sessions";
import { useAuth } from "../../auth/auth-context";
import { EmptyState, ErrorState, Loading } from "../../components/states";

export const DEFAULT_REPLAY_DIR = "testdata/synthetic/replay/dialogue-advance-001";

type SourceKind = "replay" | "test_pattern" | "dolphin" | "v4l2";
type SessionMode = "conservative" | "predictive";

interface RunOption {
  game: Game;
  edition: Edition;
  profile: Profile;
}

function runLabel(option: RunOption): string {
  const { game, edition, profile } = option;
  return `${game.title} · ${edition.region}/${edition.language} · r${profile.revision_num}`;
}

/** Every published revision the appliance could run, for the start form. */
async function fetchRunOptions(signal?: AbortSignal): Promise<RunOption[]> {
  const gamesPage = (await catalogApi.listGames({ limit: 200 }, signal)) as unknown as {
    games: Game[];
  };
  const options: RunOption[] = [];
  for (const game of gamesPage.games ?? []) {
    const editionsPage = (await catalogApi.listEditions(game.id, signal)) as unknown as {
      editions: Edition[];
    };
    for (const edition of editionsPage.editions ?? []) {
      let profile: Profile | null = null;
      try {
        profile = (await api.get("/api/v1/editions/{id}/published", {
          params: { id: edition.id },
          signal,
        })) as unknown as Profile;
      } catch (error) {
        // Only a 404 means "nothing published for this edition" — the normal
        // shelf state the empty option list renders. Any other failure
        // (transient 5xx, an aborted in-flight read, a network drop) must
        // not be swallowed into a false "Nothing is published": rethrow it so
        // the query errors and the page offers the retryable error state.
        if (apiStatus(error) !== 404) throw error;
        continue;
      }
      if (profile) options.push({ game, edition, profile });
    }
  }
  return options;
}

export function SetupForm() {
  const { atLeast } = useAuth();
  const [, setParams] = useSearchParams();
  const runs = useQuery({
    queryKey: ["play", "run-options"],
    queryFn: ({ signal }) => fetchRunOptions(signal),
  });
  const [profileId, setProfileId] = useState("");
  const [sourceKind, setSourceKind] = useState<SourceKind>("replay");
  // Developer trace stores the sampled frames and ROI crops the developer
  // can pin in the inspector (docs/spec/13); developer role only.
  const [devTrace, setDevTrace] = useState(false);
  const [replayDir, setReplayDir] = useState(DEFAULT_REPLAY_DIR);
  // A managed-store clip id (ADR-0073) outranks the directory; the picker is
  // developer-scoped (the list is), so it only exists for developer and up.
  const [replayClip, setReplayClip] = useState("");
  // The capture device a kind v4l2 session runs on (ADR-0078): the stable id
  // from the device list — never a path; empty means the persisted selection.
  const [captureDevice, setCaptureDevice] = useState("");
  const [mode, setMode] = useState<SessionMode>("conservative");
  const [startError, setStartError] = useState<unknown>(null);

  const operator = atLeast("operator");
  const developer = atLeast("developer");

  // The selected revision's script: after a publish, "the play page lists
  // the messages" is visible state (ADR-0066). Only the head is rendered —
  // a 10k-line profile is a count, not a DOM.
  const selectedMessages = useQuery({
    queryKey: ["play", "setup-messages", profileId],
    queryFn: ({ signal }) => fetchAllMessages(profileId, signal),
    enabled: Boolean(profileId),
  });

  const clipList = useQuery({
    queryKey: ["play", "replay-clips"],
    queryFn: ({ signal }) => clipsApi.list(signal),
    enabled: sourceKind === "replay" && developer,
  });

  // The capture device list (ADR-0078) for the v4l2 source picker; the
  // answer's selected_id preselects the persisted choice.
  const captureList = useQuery({
    queryKey: ["play", "capture-devices"],
    queryFn: ({ signal }) => fetchCaptureDevices(signal),
    enabled: sourceKind === "v4l2" && operator,
    retry: false,
  });
  // The answer's selected_id preselects the persisted choice exactly once
  // (the operator's later choice is never overridden by a refetch).
  const preselected = useRef(false);
  useEffect(() => {
    if (!captureList.data || preselected.current) return;
    preselected.current = true;
    const ready = captureList.data.devices.filter((device) => device.state === "ready");
    setCaptureDevice(captureList.data.selected_id ?? ready[0]?.id ?? "");
  }, [captureList.data]);
  const captureReady = (captureList.data?.devices ?? []).filter(
    (device) => device.state === "ready",
  );
  // A v4l2 session without a ready device cannot start: say so instead of
  // letting the server's 503 be the only answer.
  const captureBlocked =
    sourceKind === "v4l2" &&
    (captureList.isLoading || captureList.isError || captureReady.length === 0);

  const start = useMutation({
    mutationFn: () =>
      sessionsApi.start({
        profile_id: profileId,
        frame_source:
          sourceKind === "replay"
            ? replayClip !== ""
              ? { kind: "replay", clip_id: replayClip }
              : { kind: "replay", dir: replayDir }
            : sourceKind === "dolphin"
              ? { kind: "dolphin" }
              : sourceKind === "v4l2"
                ? { kind: "v4l2", ...(captureDevice !== "" ? { dir: captureDevice } : {}) }
                : { kind: "test_pattern" },
        mode,
        dev_trace: devTrace,
      }),
    onSuccess: (session) => setParams({ session: session.session_id }),
    onError: (error) => setStartError(error),
  });

  return (
    <section className="panel" data-testid="play-setup">
      <p className="muted">
        Pick the published revision to run and where the frames come from. One session at a time is
        a runtime invariant, not a UI limit.
      </p>
      {runs.isPending ? <Loading label="Finding published profiles…" /> : null}
      {runs.isError ? (
        <ErrorState
          error={runs.error}
          action="Cannot list published profiles"
          onRetry={() => void runs.refetch()}
        />
      ) : null}
      {runs.data && runs.data.length === 0 ? (
        <EmptyState title="Nothing is published">
          <p>
            A session runs an edition&apos;s published revision. Curate a profile and publish it
            from <Link to="/library">the library</Link> first.
          </p>
        </EmptyState>
      ) : null}
      {runs.data && runs.data.length > 0 ? (
        <form
          className="stack"
          onSubmit={(event) => {
            event.preventDefault();
            start.mutate();
          }}
          data-testid="play-start-form"
        >
          <label>
            Published profile
            <select
              value={profileId}
              onChange={(event) => setProfileId(event.target.value)}
              data-testid="play-profile"
            >
              <option value="">choose a published revision…</option>
              {runs.data.map((option) => (
                <option key={option.profile.id} value={option.profile.id}>
                  {runLabel(option)}
                </option>
              ))}
            </select>
          </label>
          <label>
            Frame source
            <select
              value={sourceKind}
              onChange={(event) => setSourceKind(event.target.value as SourceKind)}
              data-testid="play-source-kind"
            >
              <option value="replay">replay clip</option>
              <option value="test_pattern">test pattern</option>
              <option value="dolphin">dolphin emulator (dev host)</option>
              <option value="v4l2">capture device (HDMI dongle)</option>
            </select>
          </label>
          {sourceKind === "v4l2" ? (
            <div data-testid="play-capture-picker">
              {captureList.isError ? (
                <p className="error" role="alert" data-testid="play-capture-error">
                  The capture device list could not be read:
                  {captureList.error instanceof Error ? captureList.error.message : ""} — the device
                  is configured on the Settings page (Capture section).
                </p>
              ) : captureReady.length === 0 ? (
                <p className="muted" data-testid="play-capture-none">
                  No ready capture device is attached: connect the UVC HDMI dongle (the Settings
                  page's Capture section shows its state and runs the capture test).
                </p>
              ) : (
                <label>
                  Capture device
                  <select
                    value={captureDevice}
                    onChange={(event) => setCaptureDevice(event.target.value)}
                    data-testid="play-capture-device"
                  >
                    {captureReady.map((device) => (
                      <option key={device.id} value={device.id}>
                        {device.name} ({device.id})
                        {captureList.data?.selected_id === device.id ? " — selected" : ""}
                      </option>
                    ))}
                  </select>
                  <span className="muted" data-testid="play-capture-note">
                    {captureList.data?.synthetic
                      ? "Synthetic device list (test seam): this is not the hardware of this " +
                        "machine. "
                      : ""}
                    Empty selection runs the persisted selection, then the first ready device; the
                    session snapshot names the device actually used.
                  </span>
                </label>
              )}
            </div>
          ) : null}
          {sourceKind === "replay" ? (
            <>
              {developer ? (
                <label>
                  Clip from the managed store
                  <select
                    value={replayClip}
                    onChange={(event) => setReplayClip(event.target.value)}
                    data-testid="play-replay-clip"
                  >
                    <option value="">(replay directory instead)</option>
                    {clipList.data?.clips.map((clip) => (
                      <option key={clip.id} value={clip.id} disabled={Boolean(clip.missing)}>
                        {clip.id} · {clip.missing ? "missing" : `${clip.frames} frames`}
                      </option>
                    ))}
                  </select>
                  <span className="muted">
                    {clipList.isPending
                      ? "Listing clips…"
                      : clipList.isError
                        ? "The clip store could not be listed; a replay directory still works."
                        : (clipList.data?.clips.length ?? 0) === 0
                          ? "No clips installed yet: upload one from the Replay clips page."
                          : "A selected clip outranks the directory below (the store is a replay root)."}
                  </span>
                </label>
              ) : null}
              <label>
                Replay directory
                <input
                  type="text"
                  value={replayDir}
                  disabled={replayClip !== ""}
                  onChange={(event) => setReplayDir(event.target.value)}
                  data-testid="play-replay-dir"
                />
                <span className="muted">
                  Must sit under the configured replay allowlist; the dev checkout admits its
                  testdata. Ignored while a managed-store clip is selected.
                </span>
              </label>
            </>
          ) : null}
          <label>
            Mode
            <select
              value={mode}
              onChange={(event) => setMode(event.target.value as SessionMode)}
              data-testid="play-mode"
            >
              <option value="conservative">conservative</option>
              <option value="predictive">predictive</option>
            </select>
          </label>
          {developer ? (
            <label className="row">
              <input
                type="checkbox"
                data-testid="play-dev-trace"
                checked={devTrace}
                onChange={(event) => setDevTrace(event.target.checked)}
              />
              <span>Developer trace (store sampled frames and ROI crops)</span>
            </label>
          ) : null}
          {startError ? <ErrorState error={startError} action="Cannot start the session" /> : null}
          <p>
            <button
              type="submit"
              disabled={!operator || start.isPending || !profileId || captureBlocked}
              data-testid="play-start"
            >
              {start.isPending ? "Starting…" : "Start session"}
            </button>
            {!operator ? (
              <span className="muted" data-testid="play-role-note">
                Starting or stopping a session needs the operator role (the server enforces it).
              </span>
            ) : null}
          </p>
          {profileId ? (
            <div data-testid="play-profile-messages">
              {selectedMessages.isLoading ? <p className="muted">Loading the script…</p> : null}
              {selectedMessages.data ? (
                <>
                  <p>
                    <strong data-testid="play-message-count">{selectedMessages.data.length}</strong>{" "}
                    messages in this revision
                  </p>
                  <ol className="recent-lines" data-testid="play-message-list">
                    {selectedMessages.data.slice(0, 8).map((message) => (
                      <li
                        key={message.id}
                        data-testid={`play-message-${message.source_message_id}`}
                      >
                        {message.original_text}
                      </li>
                    ))}
                  </ol>
                  {selectedMessages.data.length > 8 ? (
                    <p className="muted">
                      … and {selectedMessages.data.length - 8} more; the dialogue page lists all of
                      them.
                    </p>
                  ) : null}
                </>
              ) : null}
            </div>
          ) : null}
        </form>
      ) : null}
    </section>
  );
}
