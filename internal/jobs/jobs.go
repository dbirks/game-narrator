// Package jobs runs the durable background-work queue that
// docs/spec/01-system-architecture.md places beside the API ("Go API +
// embedded React SPA + job workers + SQLite") and docs/spec/08 exposes as
// /api/v1/jobs.
//
// The queue itself is the SQLite table (internal/store/jobs_store.go): this
// package is the worker side — a pool of lease-holding pollers per job kind,
// retries with backoff, cooperative cancellation, crash recovery handoff, and
// the event publishing that lets the UI follow a job without polling.
//
// Two rules shape the design:
//
//   - The database is the authority on job state. A worker never assumes it
//     still owns a job: it heartbeats, and a lost lease or a cancelled job is
//     observed there, not by an in-process signal. That is what makes killing
//     the process survivable (docs/spec/13 "Power outage": "Recover
//     transactions and jobs").
//   - Handlers are untrusted work. A panicking handler must not take the
//     runner down; a hung handler must not hold a lease past its expiry; a
//     long import must be pausable while a live session needs the appliance's
//     CPU (docs/spec/01 "Runtime and setup isolation": "background research
//     and bulk pre-generation are rate limited or paused while a live session
//     needs resources"), which is what Runner.SetForegroundBusy is.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
)

// Job is the unit of work handed to a Handler: the identity and inputs a
// handler needs, deliberately omitting lease/bookkeeping columns a handler
// has no business touching (the queue owns state; the handler owns the work).
type Job struct {
	ID          string
	Kind        string
	TraceID     string
	Payload     json.RawMessage
	Attempts    int
	ResumeToken string
}

// ProgressFn reports percent-complete (0..100) plus a human-readable line for
// the developer UI. It is safe to call from multiple goroutines; a nil
// hub-backed runner turns it into a no-op. Progress calls never fail a job
// and never block the handler.
type ProgressFn func(pct float64, msg string)

// Handler does the work for one registered kind. Returning a nil result with
// a nil error completes the job; a returned error is retried with backoff
// unless wrapped in Permanent. ctx is cancelled on job cancellation (observed
// via heartbeat), on the kind's timeout, and on a forced shutdown drain — so
// a handler that respects ctx is also a handler that survives all three.
type Handler func(ctx context.Context, job Job, progress ProgressFn) (json.RawMessage, error)

// permanentError marks an error whose retry will fail identically: bad input,
// an unparseable source, an unregistered voice. docs/spec/08's
// JOB_REQUIRES_INPUT-style terminal errors belong here; transport blips do
// not.
type permanentError struct {
	err error
}

func (p *permanentError) Error() string { return p.err.Error() }
func (p *permanentError) Unwrap() error { return p.err }

// Permanent marks err as non-retryable: the job goes straight to `failed`
// instead of burning its retry budget on the same deterministic error.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err (or anything it wraps) was marked
// Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}
