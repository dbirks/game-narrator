package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// Job lifecycle event names beyond docs/spec/01's list. internal/events
// exposes Register precisely so later milestones can extend the vocabulary
// without editing that package (AGENTS.md: typed interfaces, additive
// vocabulary); the names are dotted lower-case, grouping with `agent.job.
// progress` (kept for progress ticks, as spec'd).
const (
	EventQueued    = "job.queued"
	EventStarted   = "job.started"
	EventSucceeded = "job.succeeded"
	EventFailed    = "job.failed"
	EventCancelled = "job.cancelled"
)

// Defaults for one Runner. The lease is a contract with the database: a
// worker that stops heartbeating for lease duration may have its job handed
// to someone else, so it must comfortably exceed the worst scheduling stall
// while staying short enough that a crashed worker's job is resumable soon
// (docs/spec/13 "Power outage").
const (
	DefaultLease       = 30 * time.Second
	DefaultPoll        = 250 * time.Millisecond
	DefaultDrainGrace  = 5 * time.Second
	minHeartbeatPeriod = 10 * time.Millisecond
	finalizeTimeout    = 10 * time.Second
)

// RegisterOpts tunes one job kind.
type RegisterOpts struct {
	// Concurrency is how many jobs of this kind run at once on this worker
	// pool. Import fan-out wants more than one voice synthesis call at a
	// time; voice registration wants exactly one per provider replica.
	Concurrency int
	// Resumable declares that re-running this kind after a crash is safe and
	// desirable — the handler checkpoints via Job.ResumeToken / payload
	// (docs/spec/02 "Import resume by content hash and job_id"). Only
	// resumable kinds are re-queued by RecoverOnStartup; everything else
	// waits for a human or the agent to decide.
	Resumable bool
	// Timeout bounds one attempt. Zero means none (the job may run for the
	// lifetime of the process — fine for a prewarm, not fine for a request
	// that owes the UI an answer). docs/spec/01: "Deadline/cancellation
	// propagation" — the per-attempt deadline lives here.
	Timeout time.Duration
}

type registration struct {
	kind string
	h    Handler
	opts RegisterOpts
}

// Runner owns the worker pool. Create it once per process with the jobs
// store and the event hub, Register every kind the build serves, Start it
// with the serve context, and Stop it during shutdown.
//
// Shutdown model, because it is the part every "durable queue" gets wrong:
// cancelling the Start context stops *claiming* only. In-flight handlers keep
// running so Stop can drain them gracefully within its own budget; Stop's
// deadline expiring is what force-cancels them, and each interrupted job is
// then re-queued with backoff through the store — never left `running` by
// choice. That is how a SIGTERM mid-import and a power cut converge on the
// same recovery path (docs/spec/02 "Transaction workflows").
type Runner struct {
	js  *store.JobsStore
	hub *events.Hub
	log *slog.Logger

	workerID string
	lease    time.Duration
	poll     time.Duration
	grace    time.Duration

	mu      sync.Mutex
	regs    map[string]*registration
	kinds   []string // registration order, for stable logs/claims
	started bool
	stopped bool

	parent    context.Context // set by Start; Done stops claiming
	run       context.Context // handler lifetime; Stop force-cancels
	cancelRun context.CancelFunc

	drain    chan struct{} // closed by Stop
	wake     chan struct{} // pokes sleeping workers on foreground-busy=false
	fgBusy   atomic.Bool
	fgSince  time.Time // set under mu: when the current foreground pause began
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// ForegroundPause is the visible state of the foreground pause (ADR-0119,
// issue #27): whether the live session holds the appliance's CPU/audio budget
// (docs/spec/01 invariant 1), when that pause began, and how many queued jobs
// it is holding. The count is a read of the queue at the call (the database is
// the authority on job state, so it is a query, never a counter the worker
// pool keeps), and it is reported only while the pause is active: outside it a
// zero count would be indistinguishable from a query the caller should not pay
// for a box that is not pausing anything.
type ForegroundPause struct {
	// Paused is true while a live session holds the foreground and
	// background kinds wait between jobs.
	Paused bool
	// Since is when the current pause began (zero when not paused). It is
	// stamped on the first SetForegroundBusy(true) and never moved by a
	// repeated true: a second session started after the first ends stamps a
	// fresh pause, not an extension of the old one.
	Since time.Time
	// Queued is the count of queued jobs held by the pause; zero when the
	// pause is inactive (or the count read failed — a pause the count cannot
	// name is still named, with zero).
	Queued int64
}

// Option tweaks Runner internals that tests need to shrink (a 30 s lease is
// untestable; 100 ms is).
type Option func(*Runner)

// WithLease sets the claim lease duration.
func WithLease(d time.Duration) Option {
	return func(r *Runner) {
		if d > 0 {
			r.lease = d
		}
	}
}

// WithPollInterval sets the idle claim poll and foreground-pause cadence.
func WithPollInterval(d time.Duration) Option {
	return func(r *Runner) {
		if d > 0 {
			r.poll = d
		}
	}
}

// WithDrainGrace sets how long Stop waits after a forced cancellation for
// workers to record their retries before declaring them wedged.
func WithDrainGrace(d time.Duration) Option {
	return func(r *Runner) {
		if d > 0 {
			r.grace = d
		}
	}
}

// NewRunner wires a runner to the durable queue and (optionally nil) the
// event hub, and adds the job.* names to the event vocabulary.
func NewRunner(js *store.JobsStore, hub *events.Hub, log *slog.Logger, opts ...Option) (*Runner, error) {
	if js == nil {
		return nil, errors.New("jobs: NewRunner requires a jobs store")
	}
	if log == nil {
		log = slog.Default()
	}
	if err := events.Register(EventQueued, EventStarted, EventSucceeded, EventFailed, EventCancelled); err != nil {
		return nil, fmt.Errorf("jobs: register event names: %w", err)
	}
	r := &Runner{
		js:  js,
		hub: hub,
		log: log,
		// The lease owner is one worker slot, not one process: two runners
		// on one host (a restart that overlapped a zombie, or the dev loop)
		// must still be told apart by Heartbeat's owner check.
		workerID: "gn-" + games.NewID(),
		lease:    DefaultLease,
		poll:     DefaultPoll,
		grace:    DefaultDrainGrace,
		regs:     map[string]*registration{},
		drain:    make(chan struct{}),
		wake:     make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Register binds a handler to a job kind. It must be called before Start:
// the worker pool topology (kinds × concurrency) is fixed at Start so a
// running appliance cannot grow goroutines behind the operator's back.
func (r *Runner) Register(kind string, h Handler, opts RegisterOpts) error {
	if kind == "" {
		return errors.New("jobs: Register requires a kind")
	}
	if h == nil {
		return fmt.Errorf("jobs: Register(%s) requires a handler", kind)
	}
	if opts.Concurrency < 0 {
		return fmt.Errorf("jobs: Register(%s): concurrency must be >= 0 (0 means 1)", kind)
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = 1
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("jobs: Register(%s): runner already started", kind)
	}
	if _, dup := r.regs[kind]; dup {
		return fmt.Errorf("jobs: Register(%s): kind already registered: %w", kind, store.ErrConflict)
	}
	r.regs[kind] = &registration{kind: kind, h: h, opts: opts}
	r.kinds = append(r.kinds, kind)
	return nil
}

// ResumableKinds lists the registered kinds flagged Resumable — the allowlist
// to hand store.JobsStore.RecoverOnStartup so a restart re-runs only the
// work that declared itself safe to re-run.
func (r *Runner) ResumableKinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, k := range r.kinds {
		if r.regs[k].opts.Resumable {
			out = append(out, k)
		}
	}
	return out
}

// Start launches the worker pool. ctx's cancellation stops claiming (the
// graceful half of shutdown); call Stop to finish it.
func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return errors.New("jobs: runner already started")
	}
	if r.stopped {
		r.mu.Unlock()
		return errors.New("jobs: runner already stopped; construct a new one")
	}
	r.started = true
	r.parent = ctx
	r.run, r.cancelRun = context.WithCancel(context.Background())
	total := 0
	for _, k := range r.kinds {
		total += r.regs[k].opts.Concurrency
	}
	r.mu.Unlock()

	if r.parent == nil { // Start(ctx) always sets it; belt for the wait helpers
		return errors.New("jobs: runner Start lost its context")
	}
	r.log.Info("job runner starting",
		"worker_id", r.workerID, "kinds", len(r.kinds), "workers", total,
		"lease_ms", r.lease.Milliseconds(), "poll_ms", r.poll.Milliseconds())

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, kind := range r.kinds {
		for slot := 0; slot < r.regs[kind].opts.Concurrency; slot++ {
			r.wg.Add(1)
			go r.workerLoop(kind, slot)
		}
	}
	return nil
}

// Stop drains: no further claims, wait for in-flight handlers until ctx
// expires, then force-cancel them (each interrupted job is re-queued with
// backoff by its worker before exiting) and wait a bounded grace for the
// finalisation writes. A non-nil error means jobs were interrupted — not that
// they are lost: that is exactly what RecoverOnStartup/the re-queued rows are
// for. Stop is idempotent.
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	if r.cancelRun == nil { // never started: nothing to drain
		return nil
	}
	r.stopOnce.Do(func() { close(r.drain) })

	waited := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		r.log.Info("job runner stopped cleanly")
		return nil
	case <-ctx.Done():
	}

	r.log.Warn("job runner drain window expired, cancelling in-flight jobs for retry",
		"force_grace_ms", r.grace.Milliseconds())
	r.cancelRun()
	force := time.NewTimer(r.grace)
	defer force.Stop()
	select {
	case <-waited:
		return fmt.Errorf("jobs: runner stopped after forcing %v: %w", ctx.Err(), ctx.Err())
	case <-force.C:
		return errors.New("jobs: runner workers did not exit after forced cancellation (handlers ignore ctx?)")
	}
}

// SetForegroundBusy is the resource-isolation switch from docs/spec/01
// ("background … bulk pre-generation … paused while a live session needs
// resources"). Background work pauses between jobs while set; in-flight
// jobs are *not* killed — a half-written import that is force-cancelled
// costs more to resume than a few seconds of shared CPU. The milestone that
// owns the session runtime calls true at session start and false at stop.
//
// The pause is also visible (ADR-0119, issue #27): the first true stamps
// ForegroundPause.Since and a repeated true does not move the clock (a
// session that starts while one is already live holds the same pause), while
// false clears both the flag and the stamp.
func (r *Runner) SetForegroundBusy(busy bool) {
	r.mu.Lock()
	switch {
	case busy && !r.fgBusy.Load():
		r.fgSince = time.Now().UTC()
	case !busy:
		r.fgSince = time.Time{}
	}
	r.fgBusy.Store(busy)
	r.mu.Unlock()
	if !busy {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}

// ForegroundPause reports the visible foreground-pause state (ADR-0119,
// issue #27): whether a live session is holding the appliance's budget, when
// the pause began, and how many queued jobs it holds (a read of the queue at
// the call, per ForegroundPause's contract). It is the read the status
// snapshot and the job list serve, so "queued" can no longer mean either
// "waiting in a normal queue" or "blocked for the lifetime of a session":
// the pause names the second of those in both places an operator or an agent
// polls it.
func (r *Runner) ForegroundPause(ctx context.Context) ForegroundPause {
	r.mu.Lock()
	paused := r.fgBusy.Load()
	since := r.fgSince
	r.mu.Unlock()
	if !paused {
		return ForegroundPause{}
	}
	var queued int64
	if n, err := r.js.CountQueued(ctx); err == nil {
		queued = n
	}
	return ForegroundPause{Paused: true, Since: since, Queued: queued}
}

// Enqueue queues work through the runner so the `job.queued` event carries
// the same correlation as the state changes that follow it.
func (r *Runner) Enqueue(ctx context.Context, p store.EnqueueParams) (*store.Job, bool, error) {
	job, created, err := r.js.Enqueue(ctx, p)
	if err != nil {
		return nil, false, err
	}
	if created {
		r.publish(EventQueued, job, map[string]any{
			"kind":     job.Kind,
			"priority": job.Priority,
			"attempts": job.Attempts,
		})
	}
	return job, created, nil
}

// ── worker internals ──────────────────────────────────────────────────────

func (r *Runner) quitting() bool {
	select {
	case <-r.drain:
		return true
	default:
	}
	if r.parent != nil {
		select {
		case <-r.parent.Done():
			return true
		default:
		}
	}
	return false
}

// waitIdle sleeps up to d, or until woken, or until the runner is quitting.
// It returns false when the reason was quitting (the worker must exit).
func (r *Runner) waitIdle(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.wake:
		return true
	case <-r.drain:
		return false
	case <-r.parent.Done():
		return false
	}
}

func (r *Runner) workerLoop(kind string, slot int) {
	defer r.wg.Done()
	workerID := fmt.Sprintf("%s/%s#%d", r.workerID, kind, slot)
	for {
		if r.quitting() {
			return
		}
		// Foreground pause is checked before every claim, never mid-job, so
		// a live session starts sharing the appliance within one poll of
		// being opened.
		for r.fgBusy.Load() {
			if !r.waitIdle(r.poll) {
				return
			}
		}
		job, err := r.js.Claim(r.run, workerID, []string{kind}, r.lease)
		if err != nil {
			if r.quitting() {
				return // a cancelled claim mid-shutdown is the ordinary end
			}
			r.log.Warn("job claim failed", "kind", kind, "error", err)
			if !r.waitIdle(r.poll) {
				return
			}
			continue
		}
		if job == nil {
			if !r.waitIdle(r.poll) {
				return
			}
			continue
		}
		r.execute(kind, workerID, job)
	}
}

// execute runs one claimed job with a heartbeat watchdog and turns the
// outcome into the matching state change on the queue and the event log.
func (r *Runner) execute(kind, workerID string, job *store.Job) {
	reg := r.regs[kind]

	hctx := r.run
	var cancel context.CancelFunc
	if reg.opts.Timeout > 0 {
		hctx, cancel = context.WithTimeout(r.run, reg.opts.Timeout)
	} else {
		hctx, cancel = context.WithCancel(r.run)
	}
	defer cancel()

	started := time.Now()
	r.log.Info("job started", "job_id", job.ID, "kind", kind,
		"attempt", job.Attempts, "worker", workerID)
	r.publish(EventStarted, job, map[string]any{
		"kind":     kind,
		"attempts": job.Attempts,
	})

	var cancelledByUser atomic.Bool
	var leaseLost atomic.Bool
	handlerDone := make(chan struct{})
	watchdog := make(chan struct{})
	go func() {
		defer close(watchdog)
		period := r.lease / 3
		if period < minHeartbeatPeriod {
			period = minHeartbeatPeriod
		}
		for {
			select {
			case <-handlerDone:
				return
			case <-hctx.Done():
				return
			case <-time.After(period):
			}
			// The heartbeat must outlive a cancelled handler path, so it
			// carries no request-scoped death — but it must not outrun the
			// process either; r.run bounds it.
			state, err := r.js.Heartbeat(r.run, job.ID, workerID, r.lease)
			if err != nil {
				// Lost the lease (another worker reclaimed it) or the
				// database is unreachable. Either way this worker may no
				// longer finalize the job: stop, and let the lease/owner
				// rules decide the job's fate elsewhere.
				r.log.Warn("job heartbeat failed, abandoning",
					"job_id", job.ID, "worker", workerID, "error", err)
				leaseLost.Store(true)
				cancel()
				return
			}
			if state != store.JobRunning {
				if state == store.JobCancelled {
					r.log.Info("job cancelled out-of-band, cancelling handler",
						"job_id", job.ID, "worker", workerID)
					cancelledByUser.Store(true)
				}
				cancel()
				return
			}
		}
	}()

	progress := func(pct float64, msg string) {
		r.publish(events.AgentJobProgress, job, map[string]any{
			"kind":     kind,
			"percent":  pct,
			"message":  msg,
			"attempts": job.Attempts,
		})
	}

	result, err := invoke(reg, hctx, job, progress)
	close(handlerDone)
	<-watchdog // no heartbeat may fire after we finalize
	cancel()

	// Finalisation must survive the shutdown path that interrupted the job:
	// these calls run detached from r.run (Stop force-cancelled it), with
	// their own bounded deadline.
	detach := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), finalizeTimeout)
	}

	switch {
	case leaseLost.Load():
		// Somebody else owns the job now; writing to it from here is exactly
		// the double-finalization the lease exists to prevent. Drop it.
		r.log.Warn("job abandoned after lease loss", "job_id", job.ID, "kind", kind)
	case cancelledByUser.Load():
		// Cancel already settled the row; publish the event the UI waits for.
		r.log.Info("job cancelled", "job_id", job.ID, "kind", kind,
			"duration_ms", time.Since(started).Milliseconds())
		r.publish(EventCancelled, job, map[string]any{"kind": kind})
	case err == nil:
		ctx, cancelFinal := detach()
		defer cancelFinal()
		if cerr := r.js.Complete(ctx, job.ID, result); cerr != nil {
			r.log.Error("job completed but could not be recorded",
				"job_id", job.ID, "kind", kind, "error", cerr)
			return
		}
		r.log.Info("job succeeded", "job_id", job.ID, "kind", kind,
			"duration_ms", time.Since(started).Milliseconds())
		r.publish(EventSucceeded, job, map[string]any{"kind": kind})
	default:
		timedOut := errors.Is(err, context.DeadlineExceeded) && reg.opts.Timeout > 0
		interrupted := errors.Is(err, context.Canceled) && r.forceStopping()
		reason := err.Error()
		if interrupted {
			reason = "interrupted by shutdown; will retry"
		}
		retryable := !IsPermanent(err) // timeouts and interrupts stay retryable; Permanent errors do not

		ctx, cancelFinal := detach()
		defer cancelFinal()
		if ferr := r.js.Fail(ctx, job.ID, reason, retryable); ferr != nil {
			r.log.Error("job failure could not be recorded",
				"job_id", job.ID, "kind", kind, "error", ferr)
			return
		}
		// Reflect the queue's decision, not our hope: only a job still in
		// `failed` emits job.failed; one sent back to `queued` emits
		// job.queued with the retry reason, so a UI counting terminal
		// outcomes cannot be fooled by a retry.
		fresh, gerr := r.js.GetJob(ctx, job.ID)
		if gerr == nil && fresh.State == store.JobQueued {
			r.log.Warn("job failed, retry scheduled",
				"job_id", job.ID, "kind", kind, "attempt", fresh.Attempts,
				"of", fresh.MaxAttempts, "run_after", stampish(fresh.RunAfter), "error", reason)
			r.publish(EventQueued, job, map[string]any{
				"kind": kind, "reason": retryLabel(timedOut, interrupted),
				"attempts": fresh.Attempts, "run_after": stampish(fresh.RunAfter),
			})
			return
		}
		r.log.Warn("job failed", "job_id", job.ID, "kind", kind,
			"attempts", job.Attempts, "error", reason)
		r.publish(EventFailed, job, map[string]any{
			"kind": kind, "error": reason, "attempts": job.Attempts,
		})
	}
}

// forceStopping distinguishes "our handler ctx was cancelled because Stop
// gave up and forced" (re-queue the job) from "the handler itself reported
// cancellation" (its own business decision, honor it as a failure).
func (r *Runner) forceStopping() bool {
	return r.run != nil && r.run.Err() != nil
}

func retryLabel(timedOut, interrupted bool) string {
	switch {
	case timedOut:
		return "timed_out"
	case interrupted:
		return "interrupted"
	default:
		return "retry"
	}
}

func stampish(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// invoke runs the handler with panic containment: one poisoned job must not
// take down the appliance's whole process (docs/spec/13 graceful degradation)
// — the panic becomes a retryable failure of that one attempt.
func invoke(reg *registration, ctx context.Context, job *store.Job, progress ProgressFn) (result json.RawMessage, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			result = nil
			err = fmt.Errorf("job handler panicked: %v", rec)
		}
	}()
	return reg.h(ctx, Job{
		ID:          job.ID,
		Kind:        job.Kind,
		TraceID:     job.TraceID,
		Payload:     job.Payload,
		Attempts:    job.Attempts,
		ResumeToken: job.ResumeToken,
	}, progress)
}

// publish fans one job event onto the hub. Job events are global (no
// session_id): they belong to the appliance, not one recognition session
// (migration 0003: "NULL for global events"). A nil or closed hub is not an
// error path worth logging loudly at every state change — the queue is the
// source of truth; the stream is a view of it.
func (r *Runner) publish(name string, job *store.Job, data map[string]any) {
	if r.hub == nil {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["job_id"] = job.ID
	raw, err := json.Marshal(data)
	if err != nil { // only reachable with unmarshalable data, which map[string]any of scalars is not
		r.log.Error("cannot encode job event", "job_id", job.ID, "name", name, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := r.hub.Publish(ctx, events.Envelope{
		Name:    name,
		TraceID: job.TraceID,
		Data:    raw,
	}); err != nil {
		if errors.Is(err, events.ErrClosed) {
			r.log.Debug("job event dropped during shutdown", "job_id", job.ID, "name", name)
			return
		}
		r.log.Warn("job event publish failed", "job_id", job.ID, "name", name, "error", err)
	}
}
