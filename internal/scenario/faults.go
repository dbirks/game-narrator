// faults.go is the scenario-only fault-injection layer (docs/spec/16 M7:
// "Exercise concurrency, cache and provider failure injection"). It is
// compiled into the scenario tooling only: no production path under cmd/
// imports internal/scenario, so a fault wrapper can never ship in the
// runtime (AGENTS.md "no simulation theater" — the faults are named on the
// timeline as fault.injected events, and every scenario that carries them is
// origin:generated_synthetic; the loader refuses faults on any other origin).
//
// A fault entry fires at a moment on the clip's capture clock (from_ms/to_ms
// windows) or on the request it names, and the first activation of every
// fault entry is published as one fault.injected event: kind, role and the
// capture-clock time it fired at. Repeated triggers of the same entry (three
// dropped frames inside one window) are the same fault still active; the
// entry-level counters (the source's Stats, the gateway's hit count) carry
// the per-trigger totals.

package scenario

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// Fault kinds: the closed vocabulary of the faults: block.
const (
	FaultDropFrames          = "drop_frames"
	FaultDecisionDelay       = "decision_delay"
	FaultDecisionDuplicate   = "decision_duplicate"
	FaultEndpointDown        = "endpoint_down"
	FaultVoiceMissingReplica = "voice_missing_on_replica"
)

// Fault roles for endpoint_down: the provider the window covers.
const (
	RoleDecision = "decision"
	RoleTTS      = "tts"
)

// Fault is one typed entry of a scenario's faults: block, validated by
// parseFaults the way assertions are: unknown kinds, missing per-kind
// fields and wrong roles are line-numbered load errors.
type Fault struct {
	Type    string
	FromMS  int    // drop_frames, endpoint_down: window start (inclusive)
	ToMS    int    // drop_frames, endpoint_down: window end (exclusive)
	AfterMS int    // decision_delay: requests at or after this capture time are delayed
	DelayMS int    // decision_delay: the delay applied to those requests
	Message string // decision_duplicate: the decision reply (by message id) delivered twice
	Role    string // endpoint_down: decision or tts
	Voice   string // voice_missing_on_replica: the voice the gateway cannot resolve
	Line    int
}

// allowedFaultFields is the exact field set each kind accepts besides type.
var allowedFaultFields = map[string]map[string]bool{
	FaultDropFrames:          {"from_ms": true, "to_ms": true},
	FaultDecisionDelay:       {"after_ms": true, "delay_ms": true},
	FaultDecisionDuplicate:   {"message": true},
	FaultEndpointDown:        {"role": true, "from_ms": true, "to_ms": true},
	FaultVoiceMissingReplica: {"voice": true},
}

func parseFaults(items []*ynode) ([]Fault, error) {
	out := make([]Fault, 0, len(items))
	for i, item := range items {
		if item.kind != ymap {
			return nil, fmt.Errorf("line %d: fault %d must be a mapping", item.line, i+1)
		}
		typ, line, ok := item.scalarStr("kind")
		if !ok {
			return nil, fmt.Errorf("line %d: fault %d: kind is required (string)", item.line, i+1)
		}
		allowed, known := allowedFaultFields[typ]
		if !known {
			return nil, fmt.Errorf("line %d: unknown fault kind %q (known: drop_frames, decision_delay, decision_duplicate, endpoint_down, voice_missing_on_replica)", line, typ)
		}
		f := Fault{Type: typ, Line: line}
		for _, k := range item.keys {
			if k == "kind" {
				continue
			}
			if !allowed[k] {
				l, _ := item.child(k)
				return nil, fmt.Errorf("line %d: unexpected field %q for fault kind %q", l.line, k, typ)
			}
			switch k {
			case "from_ms", "to_ms", "after_ms", "delay_ms":
				v, l, ok := item.scalarInt(k)
				if !ok || v < 0 {
					return nil, fmt.Errorf("line %d: %s must be a non-negative integer", l, k)
				}
				switch k {
				case "from_ms":
					f.FromMS = v
				case "to_ms":
					f.ToMS = v
				case "after_ms":
					f.AfterMS = v
				case "delay_ms":
					f.DelayMS = v
				}
			case "message", "voice":
				v, l, ok := item.scalarStr(k)
				if !ok || v == "" {
					return nil, fmt.Errorf("line %d: %s is required (non-empty string)", l, k)
				}
				if k == "message" {
					f.Message = v
				} else {
					f.Voice = v
				}
			case "role":
				v, l, ok := item.scalarStr(k)
				if !ok || (v != RoleDecision && v != RoleTTS) {
					return nil, fmt.Errorf("line %d: role must be decision or tts (got %q)", l, v)
				}
				f.Role = v
			}
		}
		// Required-per-kind fields: a window with one end is not a window, and a
		// zero delay is not a delay.
		switch typ {
		case FaultDropFrames, FaultEndpointDown:
			if !item.hasKey("from_ms") || !item.hasKey("to_ms") {
				return nil, fmt.Errorf("line %d: from_ms and to_ms are required for %s", line, typ)
			}
			if f.ToMS <= f.FromMS {
				return nil, fmt.Errorf("line %d: %s window must satisfy to_ms > from_ms", line, typ)
			}
			if typ == FaultEndpointDown && f.Role == "" {
				return nil, fmt.Errorf("line %d: role is required for endpoint_down", line)
			}
		case FaultDecisionDelay:
			if !item.hasKey("after_ms") || !item.hasKey("delay_ms") {
				return nil, fmt.Errorf("line %d: after_ms and delay_ms are required for decision_delay", line)
			}
			if f.DelayMS == 0 {
				return nil, fmt.Errorf("line %d: delay_ms must be > 0", line)
			}
		case FaultDecisionDuplicate:
			if f.Message == "" {
				return nil, fmt.Errorf("line %d: message is required for decision_duplicate", line)
			}
		case FaultVoiceMissingReplica:
			if f.Voice == "" {
				return nil, fmt.Errorf("line %d: voice is required for voice_missing_on_replica", line)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// faultInjector reports the first activation of each fault entry exactly
// once, on the capture clock. Index addresses the fault entry in the
// scenario's faults: list so a report can name the authoring line.
type faultInjector struct {
	mu    sync.Mutex
	fired map[int]bool
	// report is called with the entry's index, kind, capture-clock time in
	// milliseconds and a human detail; nil disables reporting (unit tests).
	report func(index int, kind string, timeMS int64, role string, detail string)
}

func newFaultInjector(n int, report func(index int, kind string, timeMS int64, role string, detail string)) *faultInjector {
	return &faultInjector{fired: make(map[int]bool, n), report: report}
}

// fire reports the first activation of entry i. It returns true when this
// call is the activation the event records.
func (fi *faultInjector) fire(i int, f Fault, timeMS int64, detail string) bool {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if fi.fired[i] {
		return false
	}
	fi.fired[i] = true
	if fi.report != nil {
		fi.report(i, f.Type, timeMS, faultRole(f), detail)
	}
	return true
}

func faultRole(f Fault) string {
	switch f.Type {
	case FaultDropFrames:
		return "frames"
	case FaultDecisionDelay, FaultDecisionDuplicate:
		return RoleDecision
	case FaultEndpointDown:
		return f.Role
	case FaultVoiceMissingReplica:
		return RoleTTS
	}
	return ""
}

// DropFrameSource wraps a capture.Source and withholds every frame whose
// capture timestamp falls inside [fromNS, toNS): the synthetic stand-in for
// a capture layer losing video (AGENTS.md failure-injection list: "dropped
// frames"). Frames keep their Seq gaps, which is what makes the loss visible
// downstream; the wrapper's Stats add its own drops to the inner source's
// DroppedSlot so the drop rate is honest.
type DropFrameSource struct {
	inner capture.Source
	// FromNS/ToNS are the window on the frames' capture clock, nanoseconds.
	FromNS, ToNS int64
	// FaultIndex and Injector report the first drop as a fault.injected event.
	FaultIndex int
	Injector   *faultInjector
	// Detail is the human text the activation event carries.
	Detail string

	mu      sync.Mutex
	dropped int
}

// ID names the wrapper so a log line shows the fault, not the clean source.
func (s *DropFrameSource) ID() string { return "fault:drop_frames:" + s.inner.ID() }

// Start delivers the inner frames minus the window.
func (s *DropFrameSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	in, err := s.inner.Start(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan capture.Frame, 32)
	go func() {
		defer close(out)
		for f := range in {
			if f.CaptureTimestampNS >= s.FromNS && f.CaptureTimestampNS < s.ToNS {
				s.mu.Lock()
				s.dropped++
				s.mu.Unlock()
				if s.Injector != nil {
					s.Injector.fire(s.FaultIndex, Fault{Type: FaultDropFrames, FromMS: int(s.FromNS / 1e6), ToMS: int(s.ToNS / 1e6)}, f.CaptureTimestampNS/1e6, s.Detail)
				}
				continue
			}
			select {
			case out <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Stats reports the inner source's stats plus this wrapper's injected drops.
func (s *DropFrameSource) Stats() capture.Stats {
	s.mu.Lock()
	d := s.dropped
	s.mu.Unlock()
	st := s.inner.Stats()
	st.DroppedSlot += uint64(d)
	return st
}

// Close delegates to the inner source.
func (s *DropFrameSource) Close() error { return s.inner.Close() }

// Dropped counts the frames this wrapper withheld.
func (s *DropFrameSource) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// inFaultWindow reports whether capture time ms sits inside [fromMS, toMS).
func inFaultWindow(fromMS, toMS, ms int) bool { return ms >= fromMS && ms < toMS }

// delayFor returns the fault's delay for a request issued at capture time
// ms, zero when the request is not covered.
func delayFor(f Fault, ms int64) time.Duration {
	if f.Type != FaultDecisionDelay || ms < int64(f.AfterMS) {
		return 0
	}
	return time.Duration(f.DelayMS) * time.Millisecond
}
