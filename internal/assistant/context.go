package assistant

// The screen context of docs/design/plum-peach UX-01: the dock sends it with
// every message so instructions resolve against the current screen ("make her
// older" means the selected character). This file owns its wire shape and its
// rendering into the model-facing user turn.
//
// Trust rules (docs/spec/14): the context is caller-supplied metadata, not
// untrusted game content — but it is model-visible, so every field is
// bounded and validated at the write edge (a hostile or buggy dock cannot
// grow the prompt, and an id that is not a real id is refused rather than
// echoed into the conversation). The context reaches the prompt in the USER
// turn, never the system prompt: the system prompt stays configuration-derived
// (ADR-0025's prompt rule), and a per-turn context is exactly the shape the
// user turn has.

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
)

// Selection is the dock's current selection (UX-01): a character on the cast
// page, a line on a trace, ... The type vocabulary is open (the UI phase
// adds kinds) but every id must be a ULID, so the model never chases a
// free-text target.
type Selection struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Context is the per-message screen payload. Every field optional; a message
// with no context is still a valid turn (the bare screen name, UX-01's
// "selection is null/absent" case).
type Context struct {
	Route      string     `json:"route,omitempty"`
	GameID     string     `json:"game_id,omitempty"`
	ProfileID  string     `json:"profile_id,omitempty"`
	Selection  *Selection `json:"selection,omitempty"`
	VisibleIDs []string   `json:"visible_ids,omitempty"`
}

// Bounds: the context renders into a prompt, so it is bounded the way every
// prompt-facing field is (ADR-0025's maxSectionBytes spirit, per field).
const (
	maxContextRoute = 300
	maxContextID    = 64
	maxVisibleIDs   = 64
	maxVisibleIDLen = 64
	maxTextLen      = 4000
)

var routeRe = regexp.MustCompile(`^/[\w./?=&%-]*$`)

// Validate checks the context at the API's write edge. It refuses nothing it
// cannot bound: unknown selection types are allowed (the vocabulary is open
// with the UI), every id is ULID-shaped, and the route is an absolute
// path-only string (no scheme, no host — a route that is really a URL is a
// client bug, not a dock feature).
func (c Context) Validate() error {
	if len(c.Route) > maxContextRoute {
		return fmt.Errorf("route must be at most %d chars", maxContextRoute)
	}
	if c.Route != "" && !routeRe.MatchString(c.Route) {
		return fmt.Errorf("route must be an absolute path (starting with /) of at most %d chars", maxContextRoute)
	}
	if c.GameID != "" && !games.ValidID(c.GameID) {
		return fmt.Errorf("game_id must be a valid identifier")
	}
	if c.ProfileID != "" && !games.ValidID(c.ProfileID) {
		return fmt.Errorf("profile_id must be a valid identifier")
	}
	if c.Selection != nil {
		if len(c.Selection.Type) > 64 {
			return fmt.Errorf("selection.type must be at most 64 chars")
		}
		if !games.ValidID(c.Selection.ID) {
			return fmt.Errorf("selection.id must be a valid identifier")
		}
	}
	if len(c.VisibleIDs) > maxVisibleIDs {
		return fmt.Errorf("visible_ids must be at most %d entries", maxVisibleIDs)
	}
	for _, v := range c.VisibleIDs {
		if len(v) == 0 || len(v) > maxVisibleIDLen {
			return fmt.Errorf("visible_ids entries must be 1..%d chars", maxVisibleIDLen)
		}
	}
	return nil
}

// has reports whether the context carries anything at all.
func (c Context) has() bool {
	return c.Route != "" || c.GameID != "" || c.ProfileID != "" ||
		c.Selection != nil || len(c.VisibleIDs) > 0
}

// RenderTurn renders one user turn: the delimited screen context (when any)
// followed by the user's text. The delimiters are explicit and machine-
// readable — the model is told in the system prompt what they mean — so a
// context field can never read as an instruction: it sits in a labelled data
// block, the same discipline the untrusted-data delimiters give web content.
//
// The user's text itself is not delimited: it IS the instruction.
func RenderTurn(text string, ctx Context) string {
	text = strings.TrimSpace(text)
	if !ctx.has() {
		return text
	}
	var b strings.Builder
	b.WriteString("[SCREEN CONTEXT]\n")
	if ctx.Route != "" {
		b.WriteString("route: " + clip(ctx.Route, maxContextRoute) + "\n")
	}
	if ctx.GameID != "" {
		b.WriteString("game_id: " + ctx.GameID + "\n")
	}
	if ctx.ProfileID != "" {
		b.WriteString("profile_id: " + ctx.ProfileID + "\n")
	}
	if ctx.Selection != nil {
		b.WriteString(fmt.Sprintf("selection: %s %s\n", ctx.Selection.Type, ctx.Selection.ID))
	}
	if len(ctx.VisibleIDs) > 0 {
		b.WriteString("visible_ids: " + strings.Join(ctx.VisibleIDs, ",") + "\n")
	}
	b.WriteString("[/SCREEN CONTEXT]\n\n")
	b.WriteString(text)
	return b.String()
}

// RenderAnswerMessage renders the durable user message that carries a
// question's answer back into the conversation (UX-04: the answer is applied
// when it arrives — by the attempt that reads this message). For a
// confirmation it names the decision and the staged action's tool, so the
// model re-invokes the tool with the confirmation id; for a question it
// carries the chosen answer.
//
// questionID/prompt/answer are re-rendered from the STORED row, never from
// the caller, and the answer is bounded: a 4000-char answer is a message,
// not a prompt-DoS.
func RenderAnswerMessage(kind, questionID, prompt, answer string) string {
	answer = clip(strings.TrimSpace(answer), maxTextLen)
	var b strings.Builder
	switch kind {
	case "confirmation":
		if strings.EqualFold(answer, "confirm") {
			b.WriteString("The user CONFIRMED the staged action " + questionID +
				" (question: \"" + clip(prompt, 300) + "\"). Apply it now: call the same tool again with " +
				"confirmation=" + questionID + " and no other change to the arguments.")
		} else {
			b.WriteString("The user CANCELLED the staged action " + questionID +
				" (question: \"" + clip(prompt, 300) + "\"). Do not apply it and do not re-stage it; " +
				"tell the user it was cancelled.")
		}
	default: // question
		if answer == "" {
			b.WriteString("The user deferred the question " + questionID +
				" (\"" + clip(prompt, 300) + "\") without an answer: keep going with what is safe to do, and do not ask it again in this run.")
		} else {
			b.WriteString("The user answered the question " + questionID +
				" (\"" + clip(prompt, 300) + "\") with: " + answer + ". Apply the answer and continue.")
		}
	}
	return b.String()
}

// clip bounds one rendered field.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
