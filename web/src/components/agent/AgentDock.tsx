/**
 * `src/components/agent/AgentDock.tsx` — the docked ⌘J panel
 * (docs/design/plum-peach "Ask the agent", ADR-0093 §1).
 *
 * The 360px right panel: header with the context chip (the same seam the
 * pill reads — `screen`, or `screen › selection` once it has resolved), the
 * NEEDS-YOU block (coral; non-blocking questions with answer pills and the
 * "or keep going" escape, UX-04), the condensed tool cards (✓ name · result ·
 * duration; an in-flight card is dashed with a dither dot), the
 * user/agent chat, and the input. The pill stays where it is; this panel is
 * the docked form of it. An empty conversation shows the "try asking…" hints
 * tied to the current screen (the 2026-10-20 review): a short, screen-tied
 * list that prefills the input on a pick.
 *
 * Every state the panel renders is a server fact: the turns and questions
 * are the thread's durable conversation, the tool cards are the run's
 * executed tool calls, the problems are the API's problem+json (with the
 * request_id the design's error state requires). Placeholder copy only —
 * the dock's own chrome never quotes game text (AGENTS.md rule 5).
 */

import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { useLocation } from "react-router";

import { PillButton } from "../kit/PillButton";
import { agentChipText, routeContextChip, useSelectionLabel } from "./agentContext";
import { useAgentDock, type DockEventSource, type DockQuestion } from "./useAgentDock";

const MAX_MESSAGE_LEN = 4000; // the server's assistantMaxText bound

/** The empty dock's "try asking…" hints (the 2026-10-20 review): a short
 *  list tied to the current screen — the chip's own seam names the screen,
 *  so the hints and the context chip can never disagree. Neutral questions
 *  the agent's context payload (UX-01) lets it answer; none quotes game
 *  text (AGENTS.md rule 5). A screen without hints gets the generic pair. */
const SCREEN_SUGGESTIONS: Record<string, readonly string[]> = {
  library: [
    "Which games are ready to narrate?",
    "What's on the shelf, and what's still setting up?",
  ],
  play: ["What's the state of the current session?", "Why did it skip that line?"],
  traces: ["Which lines were slow?", "Why did the matcher rank that one on top?"],
  cast: ["Which characters still need a voice?", "Regenerate the selected character's take"],
  setup: ["What stage is the run at?", "What is blocking the run?"],
  health: ["Which checks need attention?", "Why is the capture tile unknown?"],
  settings: ["Which model roles are not configured?", "What is the effective audio output?"],
  game: ["What is the state of this game's revisions?", "What can I run from this game?"],
};
const GENERIC_SUGGESTIONS: readonly string[] = [
  "What can you see on this screen?",
  "What do you need from me?",
];

/** The hints for the dock's current screen (the chip's seam, not a second
 *  route map). */
export function dockSuggestions(pathname: string): readonly string[] {
  return SCREEN_SUGGESTIONS[routeContextChip(pathname)] ?? GENERIC_SUGGESTIONS;
}

/** One NEEDS-YOU card: the question's prompt, its answer pills (the model's
 *  options, or confirm/cancel for a confirmation), and the "or keep going"
 *  escape (UX-04's deferral). Answered cards render their answer instead of
 *  their pills — the card is the record of the decision. */
function NeedsYouCard({
  question,
  answering,
  onAnswer,
}: {
  question: DockQuestion;
  answering: boolean;
  onAnswer: (questionId: string, answer: string) => void;
}) {
  const pills =
    question.kind === "confirmation"
      ? ["confirm", "cancel"]
      : (question.options ?? []).length > 0
        ? (question.options as string[])
        : ["answer"];
  return (
    <div
      className="dock-needs-you"
      data-testid={`dock-question-${question.id}`}
      aria-label={`Question: ${question.prompt}`}
    >
      <p className="dock-needs-you-prompt">{question.prompt}</p>
      {question.answer !== undefined ? (
        <p className="dock-needs-you-answer" data-testid={`dock-question-answer-${question.id}`}>
          {question.answer === "" ? "kept going" : question.answer}
        </p>
      ) : (
        <div className="dock-needs-you-pills">
          {pills.map((option) => (
            <PillButton
              key={option}
              variant={option === "confirm" ? "coral" : "outline"}
              disabled={answering}
              onClick={() => onAnswer(question.id, option)}
              data-testid={`dock-question-option-${question.id}-${option}`}
            >
              {option}
            </PillButton>
          ))}
          <button
            type="button"
            className="dock-keep-going"
            disabled={answering}
            onClick={() => onAnswer(question.id, "")}
            data-testid={`dock-keep-going-${question.id}`}
          >
            or keep going
          </button>
        </div>
      )}
    </div>
  );
}

interface Options {
  onClose: () => void;
  /** The SSE opener (test seam; default: the browser EventSource). */
  eventSourceFactory?: (url: string) => DockEventSource;
  /** The ⌘K Tab handoff (UX-02, ADR-0098): the query the palette handed to
   *  the dock. The panel mounts fresh on every open, so seeding the input
   *  state from it is the pre-fill the handoff contract promises. */
  initialQuery?: string;
}

export function AgentDock({ onClose, eventSourceFactory, initialQuery }: Options) {
  const location = useLocation();
  const {
    turns,
    tools,
    questions,
    attemptActive,
    sending,
    answering,
    problem,
    send,
    answer,
    newConversation,
  } = useAgentDock(eventSourceFactory !== undefined ? { eventSourceFactory } : {});

  const [draft, setDraft] = useState(initialQuery ?? "");
  const inputRef = useRef<HTMLTextAreaElement>(null);
  // The empty dock's hints (the 2026-10-20 review): a pick prefills the
  // input and hands it the keyboard — the hint is a start, not a submit.
  const pickSuggestion = (text: string) => {
    setDraft(text);
    inputRef.current?.focus();
  };

  // The chip seam: the bare screen name, or `screen › name` once the screen's
  // selection has resolved to one (UX-01: the chip follows the selection).
  const profileId = new URLSearchParams(location.search).get("profile");
  const selectionName = useSelectionLabel(location, profileId);
  const chip = agentChipText(routeContextChip(location.pathname), selectionName);

  // Esc closes the dock (the pill's aria-haspopup dialog contract).
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === "Escape") {
      event.stopPropagation();
      onClose();
    }
  };

  const submit = () => {
    if (draft.trim() === "" || sending || answering || problem?.threadDead) return;
    send(draft);
    setDraft("");
  };

  // Keep the newest turn in view (the panel's scroll is the container's),
  // and take the keyboard to the input when the panel opens — the same
  // contract the ⌘K palette has (the dock is an input, not a reading pane).
  const chatRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = chatRef.current;
    if (el !== null) el.scrollTop = el.scrollHeight;
  }, [turns.length, tools.length, questions.length]);
  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  // The NEEDS-YOU block stays in the record: an open question shows its
  // pills and the keep-going escape; an answered one shows its answer (a
  // deferral reads "kept going") where the pills were — the card records
  // what happened, it does not vanish (UX-04).
  const hasQuestions = questions.length > 0;

  return (
    <div className="agent-dock-body" data-testid="agent-dock-body" onKeyDown={onKeyDown}>
      {/* The header: the screen name (or `screen › selection`), the same
          seam the pill's chip reads — and the close control (Esc too). */}
      <header className="agent-dock-header" data-testid="agent-dock-header">
        <h2 className="agent-dock-title">Ask the agent</h2>
        <span className="agent-pill-context" data-testid="agent-dock-context">
          {chip}
        </span>
        <button
          type="button"
          className="link-button agent-dock-close"
          onClick={onClose}
          data-testid="agent-dock-close"
        >
          Close (Esc)
        </button>
      </header>

      {/* The scrollable middle: NEEDS-YOU, tool cards, the conversation and
          any problem card — the input row below stays put while this
          scrolls (a long conversation must not push the send off-screen). */}
      <div className="dock-scroll" data-testid="dock-scroll">
        {/* NEEDS-YOU (coral, non-blocking): the questions, newest last.
          The block is present while the thread has questions — answered
          ones stay in the record with their answer in place of the pills.
          An empty dock shows chat and input, not an empty coral frame. */}
        {hasQuestions ? (
          <section className="dock-section" aria-label="Needs you" data-testid="dock-needs-you">
            <h3>Needs you</h3>
            {questions.map((question) => (
              <NeedsYouCard
                key={question.id}
                question={question}
                answering={answering}
                onAnswer={answer}
              />
            ))}
          </section>
        ) : null}

        {/* Tool cards: the run's executed tool calls, condensed (✓ name ·
          result · duration); in-flight cards are dashed with a dither dot. */}
        {tools.length > 0 ? (
          <section className="dock-section" aria-label="Tool calls" data-testid="dock-tools">
            <h3>Tools</h3>
            <ul className="dock-tool-list">
              {tools.map((tool) => (
                <li
                  key={tool.id}
                  className={
                    tool.state === "running" ? "dock-tool-card dock-tool-running" : "dock-tool-card"
                  }
                  data-testid={`dock-tool-${tool.id}`}
                  aria-label={`Tool ${tool.name}: ${tool.state}`}
                >
                  <span aria-hidden="true">
                    {tool.state === "succeeded" ? "✓" : tool.state === "running" ? "·" : "✗"}
                  </span>
                  <span className="dock-tool-name">{tool.name}</span>
                  <span className="dock-tool-state">{tool.state}</span>
                  {tool.durationMs !== undefined ? (
                    <span className="dock-tool-duration">{tool.durationMs}ms</span>
                  ) : null}
                  {tool.error !== undefined ? (
                    <span className="dock-tool-error">{tool.error}</span>
                  ) : null}
                </li>
              ))}
            </ul>
          </section>
        ) : null}

        {/* The conversation: user turns right (ink), agent turns left (card).
          The user's first turn is optimistic — it is their own words — and
          the snapshot replaces its local seq with the durable one. */}
        <div className="dock-chat" ref={chatRef} data-testid="dock-chat" aria-label="Conversation">
          {turns.length === 0 ? (
            <>
              <p className="muted dock-chat-empty" data-testid="dock-chat-empty">
                {problem?.threadDead
                  ? "This conversation ended on a fault; start a new one."
                  : "Ask the agent about this screen. It sees the route, the game, the revision and what you have selected."}
              </p>
              {problem?.threadDead === true ? null : (
                <ul className="dock-suggestions" data-testid="dock-suggestions">
                  {dockSuggestions(location.pathname).map((suggestion, index) => (
                    <li key={suggestion}>
                      <button
                        type="button"
                        className="dock-suggestion"
                        onClick={() => pickSuggestion(suggestion)}
                        data-testid={`dock-suggestion-${index}`}
                      >
                        {suggestion}
                      </button>
                    </li>
                  ))}
                </ul>
              )}
            </>
          ) : (
            turns.map((turn) => (
              <p
                key={`${turn.role}-${turn.seq}`}
                className={
                  turn.role === "user"
                    ? "dock-bubble dock-bubble-user"
                    : "dock-bubble dock-bubble-agent"
                }
                data-testid={
                  turn.role === "user"
                    ? `dock-turn-user-${turn.seq}`
                    : `dock-turn-agent-${turn.seq}`
                }
              >
                {turn.content}
              </p>
            ))
          )}
          {attemptActive ? (
            <p className="dock-working" data-testid="dock-working">
              working…
            </p>
          ) : null}
        </div>

        {/* The problem state: the design's error card — coral border, the
          problem, the request_id (the design's "coral error card with the
          problem+json request_id"). A dead thread offers the new
          conversation; a live one just says what happened. */}
        {problem !== null ? (
          <div
            className="dock-problem"
            data-testid="dock-problem"
            role="alert"
            aria-label="Agent problem"
          >
            <p className="dock-problem-message">{problem.message}</p>
            {problem.requestId !== "" ? (
              <code className="dock-problem-request-id" data-testid="dock-problem-request-id">
                {problem.requestId}
              </code>
            ) : null}
            {problem.threadDead ? (
              <PillButton
                variant="coral"
                onClick={newConversation}
                data-testid="dock-new-conversation"
              >
                start a new conversation
              </PillButton>
            ) : null}
          </div>
        ) : null}
      </div>

      {/* The input: one line grows, the send is the ink pill. Disabled
          while a send or answer is in flight, or on a dead thread. */}
      <div className="dock-input-row" data-testid="dock-input-row">
        <label className="visually-hidden" htmlFor="agent-dock-input">
          Message the agent
        </label>
        <textarea
          id="agent-dock-input"
          ref={inputRef}
          className="dock-input"
          rows={2}
          maxLength={MAX_MESSAGE_LEN}
          placeholder="Placeholder line for the operator to the agent."
          value={draft}
          disabled={problem?.threadDead === true}
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && !event.shiftKey) {
              event.preventDefault();
              submit();
            }
          }}
          data-testid="agent-dock-input"
        />
        <PillButton
          variant="ink"
          onClick={submit}
          disabled={sending || answering || draft.trim() === "" || problem?.threadDead === true}
          data-testid="agent-dock-send"
        >
          {sending ? "sending…" : "send"}
        </PillButton>
      </div>
    </div>
  );
}
