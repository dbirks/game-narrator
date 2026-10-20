package assistant

// The dock's two card events (ADR-0099 decision 8). They join the events
// vocabulary through events.Register in NewJobHandler; the loop's own
// agent.step / tool.started / tool.completed events (already registered)
// carry the step and tool-card tail, so the assistant only owns the two
// cards the loop does not emit: the narrative text and the NEEDS-YOU /
// confirmation question.

const (
	// EventMessage is the finished attempt's narrative answer.
	EventMessage = "assistant.message"
	// EventQuestion is one NEEDS-YOU card or confirmation
	// ({id, prompt, options, kind}).
	EventQuestion = "assistant.question"
)
