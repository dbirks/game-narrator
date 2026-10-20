import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router";

import {
  onboardingApi,
  type EventFrame,
  type OnboardingRunState,
  type OnboardingSourceView,
  type OnboardingToolCallView,
} from "../api/onboarding";
import { useAuth } from "../auth/auth-context";
import { EmptyState, ErrorState, Loading } from "../components/states";

/**
 * `/games/new` — the onboarding agent (ADR-0028): a conversational panel that
 * drives a durable `agent.run` job.
 *
 * Two data paths keep the panel honest: the REST snapshot (refetched on a
 * short interval) and the SSE stream (`agent.step`, `tool.*`,
 * `review.required`), which only triggers refetches. The snapshot is the
 * authority — a page that missed the stream rebuilds from it.
 *
 * Everything the run produces is rendered as text (docs/spec/14): conversation
 * turns, tool output, source metadata and the quality report are model- and
 * third-party-shaped, and nothing here interprets any of it as HTML.
 */

const ONBOARDING_EVENT_NAMES = "agent.step,tool.started,tool.completed,review.required";

/** The run states that are still doing something (or will, on a click). */
function isActive(state: string): boolean {
  return state === "queued" || state === "running";
}

export function NewGamePage() {
  const { atLeast } = useAuth();
  const [runId, setRunId] = useState<string | null>(null);

  if (!atLeast("curator")) {
    return (
      <main>
        <h1>New game</h1>
        <EmptyState title="Curator access required">
          <p>Onboarding runs create catalogue rows and drafts; the current role is read-only.</p>
        </EmptyState>
      </main>
    );
  }

  if (runId === null) {
    return (
      <main>
        <h1>New game</h1>
        <SetupForm onStarted={setRunId} />
      </main>
    );
  }
  return <RunPanel runId={runId} />;
}

// ── the setup form ──────────────────────────────────────────────────────────

function SetupForm({ onStarted }: { onStarted: (runId: string) => void }) {
  const [title, setTitle] = useState("");
  const [platform, setPlatform] = useState("");
  const [useWeb, setUseWeb] = useState(true);
  const [file, setFile] = useState<File | null>(null);

  const start = useMutation({
    mutationFn: async () => {
      let sha = "";
      let fileName = "";
      if (file) {
        const uploaded = await onboardingApi.upload(file);
        sha = uploaded.sha256;
        fileName = uploaded.file_name;
      }
      return onboardingApi.create({
        title,
        platform: platform || undefined,
        use_web: useWeb,
        staged_asset_sha256: sha || undefined,
        file_name: fileName || undefined,
      });
    },
    onSuccess: (created) => onStarted(created.run_id),
  });

  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    start.mutate();
  };

  return (
    <div className="panel" data-testid="onb-setup">
      <p className="muted">
        The onboarding agent onboards a title into a draft profile: it researches (when allowed),
        imports your script, reports source quality and stops at the publish boundary for a human
        approval. Nothing publishes without that approval.
      </p>
      {start.isError ? (
        <ErrorState
          error={start.error}
          action="Cannot start the onboarding run"
          onRetry={() => {
            start.reset();
            start.mutate();
          }}
        />
      ) : null}
      <form onSubmit={onSubmit} className="stack" data-testid="onb-setup-form">
        <label>
          Title
          <input
            value={title}
            onChange={(event) => setTitle(event.target.value)}
            required
            maxLength={200}
            data-testid="onb-title"
          />
        </label>
        <label>
          Platform
          <input
            value={platform}
            onChange={(event) => setPlatform(event.target.value)}
            maxLength={100}
            placeholder="GameCube"
            data-testid="onb-platform"
          />
        </label>
        <label className="check">
          <input
            type="checkbox"
            checked={useWeb}
            onChange={(event) => setUseWeb(event.target.checked)}
            data-testid="onb-web-toggle"
          />
          Allow web research (web pages are untrusted sources)
        </label>
        <label>
          Script file (optional)
          <input
            type="file"
            onChange={(event) => setFile(event.target.files?.[0] ?? null)}
            data-testid="onb-file"
          />
          {file ? <span className="muted">{file.name}</span> : null}
        </label>
        <button type="submit" disabled={start.isPending} data-testid="onb-start">
          {start.isPending ? "Starting…" : "Start onboarding"}
        </button>
      </form>
    </div>
  );
}

// ── the run panel ───────────────────────────────────────────────────────────

function RunPanel({ runId }: { runId: string }) {
  const queryClient = useQueryClient();

  const run = useQuery({
    queryKey: ["onboarding", runId, "state"],
    queryFn: ({ signal }) => onboardingApi.state(runId, signal),
    refetchInterval: 2000,
  });

  // The live half: the SSE stream triggers a refetch whenever the run emits
  // a step, a tool transition or a review request. The stream is filtered to
  // the onboarding event names and the run_id is matched client-side (the
  // stream is global; docs/spec/08).
  useOnboardingStream(runId, () => {
    void queryClient.invalidateQueries({ queryKey: ["onboarding", runId] });
  });

  if (run.isPending) return <Loading label="Loading the run…" />;
  if (run.isError) {
    return (
      <ErrorState
        error={run.error}
        action="Cannot load the onboarding run"
        onRetry={() => void run.refetch()}
      />
    );
  }
  const state = run.data;
  return (
    <main className="onboarding">
      <header className="page-header">
        <h1>
          Onboarding: <span>{state.plan.title}</span>
        </h1>
        <RunControls runId={runId} state={state} />
      </header>
      <RunBanner state={state} />
      <div className="onboarding-columns">
        <section className="panel">
          <h2>Conversation</h2>
          <Conversation state={state} />
          <ChatBox runId={runId} state={state} />
        </section>
        <section className="panel">
          <h2>Tool timeline</h2>
          <ToolTimeline calls={state.tool_calls} />
          <h2>Sources</h2>
          <SourceList sources={state.sources} />
          <h2>Source quality</h2>
          <QualityReport reports={state.quality_reports} />
          <h2>Draft preview</h2>
          {state.draft ? <DraftPreview state={state} /> : <p className="muted">No draft yet.</p>}
          <h2>Review</h2>
          <ReviewItems runId={runId} state={state} />
        </section>
      </div>
    </main>
  );
}

/** Subscribe the SSE stream for one run; the handler fires on every matching
 *  frame (and the browser resumes with Last-Event-ID on reconnect). The server
 *  names every frame (`event: <name>`), so each name gets its own listener —
 *  the default `onmessage` would only see unnamed frames. */
function useOnboardingStream(runId: string, onFrame: () => void) {
  const handlerRef = useRef(onFrame);
  handlerRef.current = onFrame;
  useEffect(() => {
    const source = new EventSource(
      `/api/v1/events?names=${encodeURIComponent(ONBOARDING_EVENT_NAMES)}`,
    );
    const names = ONBOARDING_EVENT_NAMES.split(",");
    const listen = (event: MessageEvent) => {
      let frame: EventFrame;
      try {
        frame = JSON.parse(String(event.data)) as EventFrame;
      } catch {
        return; // a frame we cannot parse is skipped, not a fault
      }
      if (frame.data?.run_id !== runId) return;
      handlerRef.current();
    };
    for (const name of names) {
      source.addEventListener(name, listen);
    }
    return () => {
      for (const name of names) {
        source.removeEventListener(name, listen);
      }
      source.close();
    };
  }, [runId]);
}

function RunControls({ runId, state }: { runId: string; state: OnboardingRunState }) {
  const pause = useMutation({
    mutationFn: (signal?: AbortSignal) => onboardingApi.pause(runId, signal),
  });
  const resume = useMutation({
    mutationFn: (signal?: AbortSignal) => onboardingApi.resume(runId, signal),
  });
  const cancel = useMutation({
    mutationFn: (signal?: AbortSignal) => onboardingApi.cancel(runId, signal),
  });

  const busy = pause.isPending || resume.isPending || cancel.isPending;
  return (
    <div className="onboarding-controls">
      {isActive(state.state) ? (
        <>
          <button
            type="button"
            disabled={busy}
            onClick={() => pause.mutate()}
            data-testid="onb-pause"
          >
            Pause
          </button>
          <button
            type="button"
            disabled={busy}
            onClick={() => cancel.mutate()}
            data-testid="onb-cancel"
          >
            Cancel
          </button>
        </>
      ) : null}
      {!isActive(state.state) && state.state !== "failed" ? (
        <button
          type="button"
          disabled={busy}
          onClick={() => resume.mutate()}
          data-testid="onb-resume"
        >
          Resume
        </button>
      ) : null}
    </div>
  );
}

function RunBanner({ state }: { state: OnboardingRunState }) {
  const detail =
    state.state === "blocked"
      ? `Blocked: ${state.stop_reason || "no reason recorded"} — resume when the fault clears.`
      : state.state === "review"
        ? "Stopped at a human review boundary; approve it below or steer with a message."
        : state.state === "failed"
          ? `Failed: ${state.stop_reason || "see the job log"}. Start a new run.`
          : state.state === "cancelled"
            ? "Cancelled; the run is resumable."
            : state.state === "succeeded"
              ? "The attempt finished; the draft awaits your approval to publish."
              : "The agent is working…";
  return (
    <div className={`onboarding-banner banner-${state.state}`} data-testid="onb-run-state">
      <strong>{state.state}</strong>
      <span>{detail}</span>
      {state.steps > 0 ? <span className="muted">{state.steps} steps</span> : null}
    </div>
  );
}

function Conversation({ state }: { state: OnboardingRunState }) {
  const boxRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    boxRef.current?.scrollTo({ top: boxRef.current.scrollHeight });
  }, [state.messages.length, state.state]);
  return (
    <div className="onboarding-conversation" data-testid="onb-conversation" ref={boxRef}>
      {state.messages.length === 0 ? (
        <p className="muted">The conversation starts when the first attempt runs.</p>
      ) : null}
      {state.messages.map((message) => (
        <div
          key={message.seq}
          className={`turn turn-${message.role}`}
          data-testid={`onb-turn-${message.role}`}
        >
          <span className="muted turn-role">{message.role}</span>
          {/* model and source text: rendered as text, never interpreted */}
          <p className="turn-text">{message.text}</p>
        </div>
      ))}
      {isActive(state.state) ? (
        <p className="muted" data-testid="onb-streaming">
          The agent is working…
        </p>
      ) : null}
    </div>
  );
}

function ChatBox({ runId, state }: { runId: string; state: OnboardingRunState }) {
  const [text, setText] = useState("");
  const send = useMutation({
    mutationFn: ({ t, signal }: { t: string; signal?: AbortSignal }) =>
      onboardingApi.message(runId, t, signal),
    onSuccess: () => setText(""),
  });
  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    if (text.trim() === "" || send.isPending) return;
    send.mutate({ t: text.trim() });
  };
  return (
    <form onSubmit={onSubmit} className="onboarding-chat" data-testid="onb-chat">
      <input
        value={text}
        onChange={(event) => setText(event.target.value)}
        placeholder={
          state.state === "failed"
            ? "This run has stopped on a fault; start a new run."
            : "Send the agent a message…"
        }
        disabled={state.state === "failed"}
        data-testid="onb-chat-input"
      />
      <button
        type="submit"
        disabled={send.isPending || text.trim() === ""}
        data-testid="onb-chat-send"
      >
        {send.isPending ? "Sending…" : "Send"}
      </button>
      {send.isError ? <ErrorState error={send.error} action="Cannot send the message" /> : null}
    </form>
  );
}

function ToolTimeline({ calls }: { calls: OnboardingToolCallView[] }) {
  if (calls.length === 0)
    return (
      <p className="muted" data-testid="onb-tool-timeline">
        No tool calls yet.
      </p>
    );
  return (
    <ul className="tool-timeline" data-testid="onb-tool-timeline">
      {calls.map((call) => (
        <li
          key={call.id}
          className={`tool-call tool-${call.status}`}
          data-testid={`onb-tool-${call.name}`}
        >
          <span className="tool-name">{call.name}</span>
          <span
            className={`badge badge-${call.status}`}
            data-testid={`onb-tool-status-${call.name}`}
          >
            {call.status}
          </span>
          {call.duration_ms != null ? (
            <span className="muted tool-duration">{call.duration_ms} ms</span>
          ) : null}
          {call.source_ids.length > 0 ? (
            <span className="tool-sources" data-testid={`onb-tool-sources-${call.name}`}>
              {call.source_ids.map((id) => (
                <code key={id}>{id}</code>
              ))}
            </span>
          ) : null}
          {call.error ? <p className="muted tool-error">{call.error}</p> : null}
        </li>
      ))}
    </ul>
  );
}

function SourceList({ sources }: { sources: OnboardingSourceView[] }) {
  if (sources.length === 0)
    return (
      <p className="muted" data-testid="onb-source-list">
        No sources yet.
      </p>
    );
  return (
    <ul className="source-cards" data-testid="onb-source-list">
      {sources.map((source) => (
        <li key={source.id} className="source-card" data-testid={`onb-source-${source.id}`}>
          <div className="source-head">
            <strong>{source.type}</strong>
            {source.untrusted ? (
              <span className="badge badge-untrusted" data-testid="onb-source-untrusted">
                untrusted
              </span>
            ) : null}
          </div>
          {source.uri ? <p className="muted source-uri">{source.uri}</p> : null}
          <p className="muted">
            rights <code>{source.rights}</code> · trust {source.trust}
          </p>
          {source.coverage_notes ? <p className="muted">{source.coverage_notes}</p> : null}
        </li>
      ))}
    </ul>
  );
}

/** The source-quality reports (docs/spec/03): structured evidence, rendered
 *  as text. The quality report is the run's coverage claim, so it is shown
 *  whole rather than summarised. */
function QualityReport({ reports }: { reports: OnboardingRunState["quality_reports"] }) {
  if (reports.length === 0) {
    return (
      <p className="muted" data-testid="onb-quality-report">
        No quality report yet.
      </p>
    );
  }
  return (
    <div data-testid="onb-quality-report">
      {reports.map((report, index) => (
        <pre key={index} className="quality-report">
          {JSON.stringify(report, null, 2)}
        </pre>
      ))}
    </div>
  );
}

function DraftPreview({ state }: { state: OnboardingRunState }) {
  const draft = state.draft;
  if (!draft) return null;
  return (
    <div className="panel" data-testid="onb-draft-preview">
      <div className="revision-head">
        <strong>Profile {draft.profile_id}</strong>
        <span className={`badge badge-${draft.status}`} data-testid="onb-draft-status">
          {draft.status}
        </span>
      </div>
      <p className="muted">
        revision {draft.revision_num} · {draft.message_count} messages · {draft.character_count}{" "}
        characters
      </p>
      {draft.status === "published" ? (
        <p>
          Published. <Link to="/library">See it in the library</Link>.
        </p>
      ) : (
        <p className="muted">The agent stops here: publishing needs your approval below.</p>
      )}
    </div>
  );
}

function ReviewItems({ runId, state }: { runId: string; state: OnboardingRunState }) {
  const pending = state.review_items.filter((item) => item.state === "pending");
  const [busyId, setBusyId] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);

  const decide = async (item: OnboardingRunState["review_items"][number], approve: boolean) => {
    setBusyId(item.id);
    setError(null);
    try {
      if (approve) {
        await onboardingApi.approve(runId, item.id);
      } else {
        // Rejection is a human decision recorded in the conversation: the
        // run resumes and the model is told what the curator declined.
        await onboardingApi.message(
          runId,
          `The curator rejected the review item "${item.id}" (${item.description}). Do not proceed with it; state what you will do instead and stop.`,
        );
      }
    } catch (err) {
      setError(err);
    } finally {
      setBusyId(null);
    }
  };

  if (pending.length === 0) {
    return (
      <p className="muted" data-testid="onb-review-list">
        No pending review items.
      </p>
    );
  }
  return (
    <div data-testid="onb-review-list">
      {error ? <ErrorState error={error} action="The review decision was not recorded" /> : null}
      <ul className="review-items">
        {pending.map((item) => (
          <li key={item.id} className="review-item" data-testid={`onb-review-item-${item.id}`}>
            <p>
              <strong>{item.kind === "publish" ? "Publish the profile" : item.tool}</strong>{" "}
              <span className="muted">— {item.description}</span>
            </p>
            {item.provenance ? (
              <div className="review-provenance" data-testid={`onb-review-provenance-${item.id}`}>
                <p>
                  <span className="muted">Source</span> {item.provenance.url}
                </p>
                <p className="muted">
                  raw-body sha256{" "}
                  {item.provenance.sha256 ? `${item.provenance.sha256.slice(0, 16)}…` : "n/a"} ·
                  retrieved {item.provenance.retrieved_at} · rights {item.provenance.rights}
                </p>
              </div>
            ) : null}
            <div className="review-actions">
              <button
                type="button"
                disabled={busyId !== null}
                onClick={() => void decide(item, true)}
                data-testid={`onb-review-approve-${item.id}`}
              >
                {busyId === item.id ? "…" : "Approve"}
              </button>
              <button
                type="button"
                disabled={busyId !== null}
                onClick={() => void decide(item, false)}
                data-testid={`onb-review-reject-${item.id}`}
              >
                Reject
              </button>
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}
