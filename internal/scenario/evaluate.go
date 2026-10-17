package scenario

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
)

// Report is the verdict of one scenario against one run.
type Report struct {
	Passed  bool
	Results []Result
}

// Result is one assertion's verdict. Detail is human-readable: it names the
// numbers compared, so a failing report points at the evidence rather than at
// the check.
type Result struct {
	Type   string // the assertion type
	Passed bool
	Detail string
}

// Evaluate runs every assertion in the scenario against the run's event log
// (oldest first), the pipeline's self-report and the clip's ground truth.
//
// All timing math happens on the capture timestamps the events carry, never on
// the wall clock: message_detected compares the commit's transition timestamp
// (the frame's capture time on the recording's source clock, docs/spec/01
// invariant 1) with the first truth row that shows the full line. A replay run
// takes seconds of wall time but its timestamps are the recording's, which is
// exactly what makes the delay assertion meaningful.
func (s *Scenario) Evaluate(evs []events.Envelope, stats pipeline.Stats, truth []Truth) Report {
	rep := Report{Results: make([]Result, 0, len(s.Assertions))}
	for _, a := range s.Assertions {
		passed, detail := evaluateAssertion(a, evs, stats, truth)
		rep.Results = append(rep.Results, Result{Type: a.Type, Passed: passed, Detail: detail})
	}
	rep.Passed = true
	for _, r := range rep.Results {
		if !r.Passed {
			rep.Passed = false
		}
	}
	return rep
}

// evaluateAssertion dispatches one assertion. Load has already validated the
// shape, so an unknown type here is a bug, not author input.
func evaluateAssertion(a Assertion, evs []events.Envelope, stats pipeline.Stats, truth []Truth) (bool, string) {
	switch a.Type {
	case "message_detected":
		return evalMessageDetected(a, evs, truth)
	case "message_committed_count":
		return evalCommittedCount(a, evs)
	case "speech_cancelled":
		return evalSpeechCancelled(a, evs)
	case "no_duplicate_commit":
		return evalNoDuplicateCommit(evs)
	case "no_stale_generation_applied":
		return evalNoStaleGeneration(stats)
	case "events_order":
		return evalEventsOrder(a, evs)
	default:
		return false, fmt.Sprintf("internal: assertion type %q has no evaluator", a.Type)
	}
}

// transitionPayload is the subset of a recognition transition the assertions
// read: the state names and the frame's capture timestamp.
type transitionPayload struct {
	From               string `json:"from"`
	To                 string `json:"to"`
	FrameSeq           int64  `json:"frame_seq"`
	CaptureTimestampNS int64  `json:"capture_timestamp_ns"`
}

// committedPayload is the subset of the message.committed payload the
// assertions read.
type committedPayload struct {
	MessageID            string              `json:"message_id"`
	Text                 string              `json:"text"`
	NormalizedText       string              `json:"normalized_text"`
	RegionID             string              `json:"region_id"`
	Generation           int64               `json:"generation"`
	DialogueOccurrenceID string              `json:"dialogue_occurrence_id"`
	Transitions          []transitionPayload `json:"transitions"`
}

// decodeCommitted reads one message.committed payload.
func decodeCommitted(e events.Envelope) (committedPayload, bool) {
	var p committedPayload
	if err := json.Unmarshal(e.Data, &p); err != nil {
		return committedPayload{}, false
	}
	return p, true
}

// commitTimeNS returns the capture timestamp the commit belongs to: the
// transition that moved the text machine into committed names the frame, and
// the frame carries the source-clock time.
func commitTimeNS(p committedPayload) (int64, bool) {
	for _, tr := range p.Transitions {
		if tr.To == "committed" {
			return tr.CaptureTimestampNS, true
		}
	}
	return 0, false
}

// evalMessageDetected: from the first truth row that shows the committed line
// in full to the commit's capture time, at most max_delay_ms.
func evalMessageDetected(a Assertion, evs []events.Envelope, truth []Truth) (bool, string) {
	var (
		p     committedPayload
		found bool
	)
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		q, ok := decodeCommitted(e)
		if !ok || q.MessageID != a.MessageID {
			continue
		}
		p, found = q, true
		break
	}
	if !found {
		return false, fmt.Sprintf("no message.committed with message_id %q in %d events", a.MessageID, len(evs))
	}
	commitTS, ok := commitTimeNS(p)
	if !ok {
		return false, fmt.Sprintf("message.committed for %q carries no committed transition with a capture timestamp", a.MessageID)
	}

	// The first truth row that shows this exact line in full. Both sides go
	// through the retrieval fold, so a dropped terminal period or a case
	// difference does not read as "the line never appeared".
	target := fold(p.NormalizedText)
	if target == "" {
		target = fold(p.Text)
	}
	var base int64
	haveBase := false
	for _, t := range truth {
		if !t.Full || t.Text == "" || fold(t.Text) != target {
			continue
		}
		base, haveBase = t.TsNS, true
		break
	}
	if !haveBase {
		return false, fmt.Sprintf("truth shows no full-line frame matching the committed text of %q (%q)", a.MessageID, p.Text)
	}

	delayNS := commitTS - base
	if delayNS < 0 {
		return false, fmt.Sprintf("commit at capture ts %d ns precedes the first full-line truth frame at %d ns: check truth or the run", commitTS, base)
	}
	limitNS := int64(a.MaxDelayMS) * int64(time.Millisecond)
	pass := delayNS <= limitNS
	return pass, fmt.Sprintf("first full-line truth at %d ns, commit at %d ns, delay %d ms (limit %d ms)",
		base, commitTS, delayNS/int64(time.Millisecond), a.MaxDelayMS)
}

// evalCommittedCount: the message committed exactly count times.
func evalCommittedCount(a Assertion, evs []events.Envelope) (bool, string) {
	n := 0
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		p, ok := decodeCommitted(e)
		if ok && p.MessageID == a.MessageID {
			n++
		}
	}
	if n != a.Count {
		return false, fmt.Sprintf("message_id %q committed %d time(s), want %d", a.MessageID, n, a.Count)
	}
	return true, fmt.Sprintf("message_id %q committed %d time(s), as expected", a.MessageID, n)
}

// speechCancelledPayload is the subset of the speech.cancelled payload the
// assertion reads.
type speechCancelledPayload struct {
	Reason    string `json:"reason"`
	MessageID string `json:"message_id"`
}

// evalSpeechCancelled: the cancellations carrying this reason occurred exactly
// count times.
func evalSpeechCancelled(a Assertion, evs []events.Envelope) (bool, string) {
	n, withID := 0, ""
	for _, e := range evs {
		if e.Name != events.SpeechCancelled {
			continue
		}
		var p speechCancelledPayload
		if err := json.Unmarshal(e.Data, &p); err != nil {
			continue
		}
		if p.Reason != a.Reason {
			continue
		}
		n++
		withID = p.MessageID
	}
	if n != a.Count {
		return false, fmt.Sprintf("speech.cancelled with reason %q occurred %d time(s), want %d", a.Reason, n, a.Count)
	}
	if withID != "" {
		return true, fmt.Sprintf("speech.cancelled with reason %q occurred %d time(s), last for message %q", a.Reason, n, withID)
	}
	return true, fmt.Sprintf("speech.cancelled with reason %q occurred %d time(s), as expected", a.Reason, n)
}

// evalNoDuplicateCommit: one dialogue occurrence commits at most once
// (docs/spec/12 "No duplicated speech for repeated observations of same
// dialogue occurrence"). The occurrence id is the identity the session dedupes
// on; if a commit somehow lacks one, the (message, region, generation) triple
// stands in, so a missing id cannot mask a duplicate.
func evalNoDuplicateCommit(evs []events.Envelope) (bool, string) {
	counts := map[string]int{}
	var worst, key string
	total := 0
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		p, ok := decodeCommitted(e)
		if !ok {
			continue
		}
		total++
		id := p.DialogueOccurrenceID
		if id == "" {
			id = fmt.Sprintf("msg:%s@%s@gen:%d", p.MessageID, p.RegionID, p.Generation)
		}
		counts[id]++
		if counts[id] > counts[worst] {
			worst, key = id, id
		}
	}
	if worst != "" && counts[worst] > 1 {
		return false, fmt.Sprintf("occurrence %s committed %d time(s): duplicate commitment", key, counts[worst])
	}
	return true, fmt.Sprintf("%d commitment(s), each occurrence committed at most once", total)
}

// evalNoStaleGeneration: the pipeline's own counter for messages applied
// despite naming a stale generation (docs/spec/01 invariant 2). It must be 0.
func evalNoStaleGeneration(stats pipeline.Stats) (bool, string) {
	if stats.StaleApplied != 0 {
		return false, fmt.Sprintf("pipeline applied %d stale-generation message(s): the generation fence is absolute", stats.StaleApplied)
	}
	return true, "no stale-generation message was applied (stale_generation_applied = 0)"
}

// evalEventsOrder: the named events appear in the log in the given order
// (subsequence, not adjacency — other events may sit between them).
func evalEventsOrder(a Assertion, evs []events.Envelope) (bool, string) {
	pos := 0
	for _, name := range a.Names {
		found := false
		for ; pos < len(evs); pos++ {
			if evs[pos].Name == name {
				found = true
				pos++
				break
			}
		}
		if !found {
			return false, fmt.Sprintf("event %q does not appear after position %d: order %v violated", name, pos, a.Names)
		}
	}
	return true, fmt.Sprintf("events appear in order: %v", a.Names)
}
