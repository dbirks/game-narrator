import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router";

import {
  onboardingApi,
  SETUP_STAGES,
  type CharacterStagePayload,
  type EventFrame,
  type OnboardingRunState,
  type OnboardingSourceView,
  type OnboardingToolCallView,
  type ProgressPayload,
  type SetupRailCell,
  type SetupStage,
} from "../api/onboarding";
import { useAuth } from "../auth/auth-context";
import { EmptyState, ErrorState, Loading } from "../components/states";
import { Card, Chip, NameTab, PillButton, StatTile } from "../components/kit";
import {
  columnForStage,
  countsByStage,
  isRunComplete,
  mergeBoard,
  rowsForStage,
  voicedProgress,
} from "../lib/setupBoard";

/**
 * `/games/new` — the setup board (docs/design/plum-peach 2c; ADR-0093;
 * ADR-0108, UX-06): the onboarding run rendered as a live board.
 *
 * Two data paths keep the board honest (ADR-0108 "snapshot ≡ replay"):
 *
 * - the `GET /board` snapshot is the page-load half — a fresh load (or the
 *   `?run=` deep link) rebuilds the board from it, and it is refetched when
 *   the SSE stream says the roster grew (an import or a re-stage);
 * - the SSE stream (`setup.character_stage` / `setup.progress`) is the live
 *   half — cards and tiles move only on those events.
 *
 * Nothing here polls: no timer ever calls the board or the run state. The
 * stream's other frames (`agent.step`, `tool.*`, `review.required`) only
 * trigger the run-state refetch that feeds the right agent panel — the
 * conversation, the tool cards, the review boundary and the chat box keep
 * the run's `onb-*` testids (the e2e contract the rework must not break).
 * Everything the run produces is rendered as text (docs/spec/14).
 */

const ONBOARDING_EVENT_NAMES =
  "agent.step,tool.started,tool.completed,review.required,setup.character_stage,setup.progress";

/** Roster-changing tools whose completion may add cards the events never
 *  carry (events record moves, not the initial board): on their completion
 * the snapshot refetches — event-triggered, never on a timer. */
const ROSTER_TOOLS = new Set(["import_user_asset", "import_web_source", "stage_character"]);

/** The run states that are still doing something (or will, on a click). */
function isActive(state: string): boolean {
  return state === "queued" || state === "running";
}

export function NewGamePage() {
  const { atLeast } = useAuth();
  const [searchParams, setSearchParams] = useSearchParams();
  // The board owns the run's identity in the URL (?run=<id>): a reload
  // rebuilds the board from the snapshot (ADR-0108), and a deep link lands
  // straight on it.
  const runId = searchParams.get("run");

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
        <SetupForm onStarted={(id) => setSearchParams({ run: id }, { replace: true })} />
      </main>
    );
  }
  return <SetupBoard runId={runId} />;
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
        imports your script, stages the roster, writes the first voice brief and stops at the
        publish boundary for a human approval. Nothing publishes without that approval.
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
        <PillButton type="submit" variant="ink" disabled={start.isPending} testid="onb-start">
          {start.isPending ? "Starting…" : "Start onboarding"}
        </PillButton>
      </form>
    </div>
  );
}

// ── the setup board ─────────────────────────────────────────────────────────

/** The SSE-driven half of the board state: the newest stage event per
 *  character and the newest run-level progress payload. */
interface BoardEvents {
  byCharacter: Map<string, { stage: SetupStage; lines: number; name: string }>;
  progress: ProgressPayload | null;
}

/** Subscribe the SSE stream for one run; the handler fires on every matching
 *  frame (and the browser resumes with Last-Event-ID on reconnect). The
 *  server names every frame (`event: <name>`), so each name gets its own
 *  listener — the default `onmessage` would only see unnamed frames. */
function useOnboardingStream(runId: string, onFrame: (frame: EventFrame) => void) {
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
      handlerRef.current(frame);
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

function SetupBoard({ runId }: { runId: string }) {
  const queryClient = useQueryClient();
  const [events, setEvents] = useState<BoardEvents>({ byCharacter: new Map(), progress: null });

  const run = useQuery({
    queryKey: ["setup-board", runId, "state"],
    queryFn: ({ signal }) => onboardingApi.state(runId, signal),
    // No refetchInterval: the stream's frames trigger the refetch (the live
    // half is SSE; a timer would be the polling UX-06 rules out). No window-
    // focus refetch either: the only network a card's move may imply is the
    // event that moved it.
    refetchOnWindowFocus: false,
  });
  const board = useQuery({
    queryKey: ["setup-board", runId, "board"],
    queryFn: ({ signal }) => onboardingApi.board(runId, signal),
    refetchOnWindowFocus: false,
  });

  useOnboardingStream(runId, (frame) => {
    switch (frame.name) {
      case "setup.character_stage": {
        const payload = frame.data as unknown as CharacterStagePayload;
        if (typeof payload?.character_id !== "string" || !SETUP_STAGES.includes(payload.stage)) {
          return; // a malformed frame is dropped, not rendered
        }
        setEvents((current) => {
          const next = new Map(current.byCharacter);
          next.set(payload.character_id, {
            stage: payload.stage,
            lines: payload.lines,
            name: payload.name,
          });
          return { ...current, byCharacter: next };
        });
        break;
      }
      case "setup.progress": {
        const payload = frame.data as unknown as ProgressPayload;
        if (typeof payload?.run_id !== "string") return;
        setEvents((current) => ({ ...current, progress: payload }));
        break;
      }
      case "tool.completed": {
        // The roster may have grown with cards no event carries (the
        // baseline): the snapshot refetch is the event-triggered sync.
        // Other tools need no board refetch; the panel's state refetch below
        // follows every frame.
        const name = typeof frame.data.name === "string" ? frame.data.name : "";
        if (ROSTER_TOOLS.has(name) && frame.data.status === "succeeded") {
          void queryClient.invalidateQueries({ queryKey: ["setup-board", runId, "board"] });
        }
        break;
      }
      default:
        break;
    }
    // The right agent panel (conversation, tool cards, review items, run
    // state, draft) is fed by the run-state read; every stream frame may
    // have advanced it, so every frame triggers the refetch.
    void queryClient.invalidateQueries({ queryKey: ["setup-board", runId, "state"] });
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
  const view = mergeBoard(board.data ?? null, events.byCharacter);
  const progress = events.progress ?? view.progress;
  const complete = isRunComplete(state.state);

  return (
    <main className="setup-page">
      <header className="page-header setup-header">
        <h1 data-testid="setup-title">
          Setting up <span>{state.plan.title}</span>
        </h1>
        <div className="setup-actions">
          {isActive(state.state) ? <RunControls runId={runId} state={state} /> : null}
          <Link to="/developer" className="kit-pill kit-pill-outline" data-testid="onb-trace">
            Trace
          </Link>
        </div>
      </header>

      <RunBanner state={state} />
      <StageRail rail={progress?.stage_rail ?? view.rail} />
      <div className="setup-tiles">
        <StatTile label="lines read" value={progress?.lines_read ?? 0} testid="onb-tile-lines" />
        <StatTile
          label="characters"
          value={progress?.characters ?? 0}
          testid="onb-tile-characters"
        />
        <StatTile
          label="voiced"
          value={progress ? `${progress.voiced}/${progress.total}` : "0/0"}
          progress={voicedProgress(progress)}
          testid="onb-tile-voiced"
        />
      </div>

      {complete ? <CompletionPanel state={state} /> : null}

      <div className="setup-main">
        <LiveBoard view={view} title={state.plan.title} />
        <AgentPanel state={state} runId={runId} title={state.plan.title} />
      </div>
    </main>
  );
}

// ── the header controls and banner ─────────────────────────────────────────

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
    <>
      {isActive(state.state) ? (
        <>
          <PillButton
            variant="outline"
            disabled={busy}
            onClick={() => pause.mutate()}
            testid="onb-pause"
          >
            Pause
          </PillButton>
          <PillButton
            variant="outline"
            disabled={busy}
            onClick={() => cancel.mutate()}
            testid="onb-cancel"
          >
            Cancel
          </PillButton>
        </>
      ) : null}
      {!isActive(state.state) && state.state !== "failed" ? (
        <PillButton
          variant="outline"
          disabled={busy}
          onClick={() => resume.mutate()}
          testid="onb-resume"
        >
          Resume
        </PillButton>
      ) : null}
    </>
  );
}

function RunBanner({ state }: { state: OnboardingRunState }) {
  const detail =
    state.state === "blocked"
      ? `Blocked: ${state.stop_reason || "no reason recorded"} — resume when the fault clears.`
      : state.state === "review"
        ? "Stopped at a human review boundary; approve it in the agent panel or steer with a message."
        : state.state === "failed"
          ? `Failed: ${state.stop_reason || "see the job log"}. Start a new run.`
          : state.state === "cancelled"
            ? "Cancelled; the run is resumable."
            : state.state === "succeeded"
              ? "The attempt finished; review the cast, then start narrating."
              : "The agent is working…";
  return (
    <div className={`onboarding-banner banner-${state.state}`} data-testid="onb-run-state">
      <strong>{state.state}</strong>
      <span>{detail}</span>
      {state.steps > 0 ? <span className="muted">{state.steps} steps</span> : null}
    </div>
  );
}

// ── the stage rail and the live board ───────────────────────────────────────

/** The six-cell rail (plan.md item 4): the run-level state the
 *  setup.progress event and the snapshot's progress both carry. */
function StageRail({ rail }: { rail: SetupRailCell[] }) {
  if (rail.length === 0) {
    return (
      <div className="setup-rail" data-testid="setup-rail" aria-busy="true">
        <span className="muted">The stage rail lands with the first progress event.</span>
      </div>
    );
  }
  return (
    <div className="setup-rail" data-testid="setup-rail">
      {rail.map((cell, index) => (
        <div
          key={cell.id}
          className="setup-rail-cell"
          data-testid={`setup-rail-cell-${cell.id}`}
          data-state={cell.state}
        >
          <span className="setup-rail-num">{String(index + 1).padStart(2, "0")}</span>
          <span className="setup-rail-label">{cell.label}</span>
          {cell.value ? <span className="setup-rail-value">{cell.value}</span> : null}
        </div>
      ))}
    </div>
  );
}

/** The four-column board (FINDING · BRIEF · VOICING · DONE). The cards are
 *  absolutely positioned; the stylesheet's position table (data-col/data-row)
 *  gives them their place, and the transition is the 1.1s glide (ADR-0093
 *  2c; it settles instantly under prefers-reduced-motion). A card in
 *  VOICING carries the coral border. */
function LiveBoard({ view, title }: { view: ReturnType<typeof mergeBoard>; title: string }) {
  const counts = countsByStage(view.cards);
  const rowsByStage = {
    finding: rowsForStage(view.cards, "finding"),
    brief: rowsForStage(view.cards, "brief"),
    voicing: rowsForStage(view.cards, "voicing"),
    done: rowsForStage(view.cards, "done"),
  } as const;

  return (
    <section
      className="setup-board"
      data-testid="setup-board"
      aria-label={`Setup board for ${title}`}
    >
      <div className="setup-board-cols">
        {SETUP_STAGES.map((stage) => (
          <div key={stage} className="setup-board-col" data-testid={`setup-column-${stage}`}>
            <span className="setup-board-col-head">{stage.toUpperCase()}</span>
            <span className="setup-board-col-count" data-testid={`setup-column-count-${stage}`}>
              {counts[stage]}
            </span>
          </div>
        ))}
      </div>
      <div className="setup-board-stage-layer" data-testid="setup-board-stage-layer">
        {view.cards.map((card) => (
          <article
            key={card.characterId}
            className="setup-card"
            data-testid={`setup-card-${card.characterId}`}
            data-stage={card.stage}
            data-col={columnForStage(card.stage)}
            data-row={rowsByStage[card.stage].get(card.characterId) ?? 0}
          >
            <NameTab testid={`setup-card-name-${card.characterId}`}>{card.name}</NameTab>
            <span className="setup-card-lines" data-testid={`setup-card-lines-${card.characterId}`}>
              {card.lines} lines
            </span>
          </article>
        ))}
        {view.cards.length === 0 ? (
          <p className="muted setup-board-empty" data-testid="setup-board-empty">
            The roster lands here as the agent stages it.
          </p>
        ) : null}
      </div>
    </section>
  );
}

/** The completion handoff (ADR-0093 open question (a), provisional answer):
 *  setup finished — the prominent "Start narrating" is the way to the Cast
 *  page, where the voices are reviewed and the narration starts. */
function CompletionPanel({ state }: { state: OnboardingRunState }) {
  const navigate = useNavigate();
  const gameId = state.plan.game_id ?? "";
  const to = gameId ? `/games/${gameId}/cast` : "/library";
  return (
    <div className="setup-complete" data-testid="setup-complete">
      <p>
        <strong>Setup complete for {state.plan.title}.</strong> Review the voices on the cast page,
        then start narrating.
      </p>
      <PillButton
        variant="coral"
        testid="onb-start-narrating"
        onClick={() => navigate(to)}
        disabled={gameId === ""}
      >
        Start narrating
      </PillButton>
    </div>
  );
}

// ── the right agent panel (the docked form of the ⌘J pill) ─────────────────

function AgentPanel({
  state,
  runId,
  title,
}: {
  state: OnboardingRunState;
  runId: string;
  title: string;
}) {
  return (
    <aside className="setup-agent" data-testid="setup-agent-panel">
      <div className="setup-agent-head">
        <Chip testid="setup-agent-context">{title} › setup</Chip>
      </div>
      <ReviewItems runId={runId} state={state} />
      <Narrative state={state} />
      <ToolTimeline calls={state.tool_calls} />
      <SourceList sources={state.sources} />
      <QualityReport reports={state.quality_reports} />
      <DraftPreview state={state} />
      <Conversation state={state} />
      <ChatBox runId={runId} state={state} />
    </aside>
  );
}

/** The NEEDS YOU block (docs/design/plum-peach "Ask the agent"): the
 *  non-blocking questions — the review boundary — with answer pills and the
 *  "or keep going" escape (a non-blocking question never blocks the run). */
function ReviewItems({ runId, state }: { runId: string; state: OnboardingRunState }) {
  const pending = state.review_items.filter((item) => item.state === "pending");
  const [busyId, setBusyId] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [dismissed, setDismissed] = useState<Record<string, boolean>>({});

  const visible = pending.filter((item) => !dismissed[item.id]);

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
      setDismissed((current) => ({ ...current, [item.id]: true }));
    } catch (err) {
      setError(err);
    } finally {
      setBusyId(null);
    }
  };

  if (visible.length === 0) return null;
  return (
    <section className="setup-needs-you" data-testid="setup-needs-you">
      <h2 className="setup-needs-you-head">NEEDS YOU</h2>
      {error ? <ErrorState error={error} action="The review decision was not recorded" /> : null}
      <ul className="setup-review-items">
        {visible.map((item) => (
          <li
            key={item.id}
            className="setup-review-item"
            data-testid={`onb-review-item-${item.id}`}
          >
            <p>
              <strong>{item.kind === "publish" ? "Publish the profile" : item.tool}</strong>{" "}
              <span className="muted">— {item.description}</span>
            </p>
            {item.provenance ? (
              <div
                className="setup-review-provenance"
                data-testid={`onb-review-provenance-${item.id}`}
              >
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
            <div className="setup-review-actions">
              <PillButton
                variant="ink"
                disabled={busyId !== null}
                onClick={() => void decide(item, true)}
                testid={`onb-review-approve-${item.id}`}
              >
                {busyId === item.id ? "…" : "Approve"}
              </PillButton>
              <PillButton
                variant="outline"
                disabled={busyId !== null}
                onClick={() => void decide(item, false)}
                testid={`onb-review-reject-${item.id}`}
              >
                Reject
              </PillButton>
              <button
                type="button"
                className="link-button setup-keep-going"
                onClick={() => setDismissed((current) => ({ ...current, [item.id]: true }))}
                data-testid={`setup-keep-going-${item.id}`}
              >
                or keep going
              </button>
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}

/** The narrative: the newest assistant turn, the panel's summary line. */
function Narrative({ state }: { state: OnboardingRunState }) {
  const lastAssistant = [...state.messages]
    .reverse()
    .find((message) => message.role === "assistant");
  if (!lastAssistant) return null;
  return (
    <Card title="Narrative" testid="setup-narrative">
      <p className="setup-narrative-text">{lastAssistant.text}</p>
    </Card>
  );
}

// ── the conversation, the tools, the chat (the run's `onb-*` contract) ─────

function Conversation({ state }: { state: OnboardingRunState }) {
  const boxRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    boxRef.current?.scrollTo({ top: boxRef.current.scrollHeight });
  }, [state.messages.length, state.state]);
  return (
    // The message list scrolls on its own and holds no focusable content of
    // its own, so the region itself takes a tab stop (axe's
    // scrollable-region-focusable: tabindex −1 is not keyboard-reachable;
    // 0 makes the region focusable and its overflow reachable by keyboard).
    <div
      className="onboarding-conversation setup-conversation"
      data-testid="onb-conversation"
      ref={boxRef}
      tabIndex={0}
    >
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
    <form onSubmit={onSubmit} className="onboarding-chat setup-chat" data-testid="onb-chat">
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
      <PillButton
        type="submit"
        variant="ink"
        disabled={send.isPending || text.trim() === ""}
        testid="onb-chat-send"
      >
        {send.isPending ? "Sending…" : "Send"}
      </PillButton>
      {send.isError ? <ErrorState error={send.error} action="Cannot send the message" /> : null}
    </form>
  );
}

/** A short display form for a source id (the 2026-10-20 review: the tool
 *  cards showed raw run ULIDs under each tool). The full id stays on the
 *  element's title attribute — the row names the fact, the id is a
 *  drill-down. */
function shortId(id: string): string {
  return id.length <= 8 ? id : `${id.slice(0, 8)}…`;
}

function ToolTimeline({ calls }: { calls: OnboardingToolCallView[] }) {
  if (calls.length === 0)
    return (
      <p className="muted" data-testid="onb-tool-timeline">
        No tool calls yet.
      </p>
    );
  return (
    <ul className="tool-timeline setup-tool-cards" data-testid="onb-tool-timeline">
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
                <code key={id} title={id}>
                  {shortId(id)}
                </code>
              ))}
            </span>
          ) : null}
          {call.error ? <p className="muted tool-error">{call.error}</p> : null}
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
        <pre key={index} className="quality-report" tabIndex={0}>
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
    <div className="panel setup-draft" data-testid="onb-draft-preview">
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
        <p className="muted">The agent stops here: publishing needs your approval in NEEDS YOU.</p>
      )}
    </div>
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
