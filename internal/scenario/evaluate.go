package scenario

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/session"
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
// (oldest first), the pipeline's self-report and the clip's ground truth, and
// appends the implicit replay_not_degraded check to every report (see
// evalReplayNotDegraded).
//
// All timing math happens on the capture timestamps the events carry, never on
// the wall clock: message_detected compares the commit's transition timestamp
// (the frame's capture time on the recording's source clock, docs/spec/01
// invariant 1) with the first truth row that shows the full line. A replay run
// takes seconds of wall time but its timestamps are the recording's, which is
// exactly what makes the delay assertion meaningful.
// EvaluateOpts is the optional context a harness passes EvaluateWith. A zero
// value is exactly Evaluate: no engine scope, no corpus.
type EvaluateOpts struct {
	// OCREngine is the OCR engine the run used (the EvaluateWithOCREngine
	// argument): an assertion scoped with engines: to another engine is
	// reported not-applicable.
	OCREngine string
	// Corpus carries the scenario profile's normalized message texts
	// (message id to fold(original text), Profile.CorpusFold). When present
	// and the commit names a corpus message, message_detected anchors the
	// commit to the corpus line instead of the run's OCR text — the
	// engine-free comparison the assertion's purpose needs: the fixture
	// ground truth and the corpus are generated from the same sentences
	// (docs/spec/11 "Fixture origins"), so the corpus line is what the page
	// showed, and the engine's glyph errors (the CI distro's tesseract 5.3
	// and the dev host's 5.5 read the same preprocessed crop differently:
	// "crossing" vs "crossina" on the Black Water clip) must not decide
	// which truth line a commit belongs to. The ranking already made the
	// message decision; the message_committed_count assertion pins it.
	// Commits the corpus cannot name keep the OCR text as their anchor.
	Corpus map[string]string
	// CatchUp is the session's catch-up narration policy the run applied
	// (issue #25, ADR-0122): catch_up (the product default) speaks a line
	// committed while a previous one is still playing — a completed non-last
	// line is the policy working, not stale speech — while latest_only cuts
	// the older line on the advance, so a completed non-last line there is
	// the stale speech docs/spec/12 forbids. The zero value ("") keeps the
	// historical latest_only reading for harnesses that do not declare it.
	CatchUp string
}

func (s *Scenario) Evaluate(evs []events.Envelope, stats pipeline.Stats, truth []Truth) Report {
	return s.EvaluateWith(evs, stats, truth, EvaluateOpts{})
}

// EvaluateWithOCREngine is EvaluateWith for a harness that declares the OCR
// engine it ran (the M7 truth stub, or the real engine of an M2 replay). An
// assertion scoped with engines: to other engines is reported not-applicable
// — a passing row that says why it was not graded, never a silent skip and
// never a failure for a property the run's engine does not have. The
// property of one engine (the eng model's blindness to a non-Latin page) is
// not a defect of a run whose scripted engine answers from the clip's truth,
// and grading it there would turn the harness into a lie about OCR quality.
func (s *Scenario) EvaluateWithOCREngine(ocrEngine string, evs []events.Envelope, stats pipeline.Stats, truth []Truth) Report {
	return s.EvaluateWith(evs, stats, truth, EvaluateOpts{OCREngine: ocrEngine})
}

// EvaluateWith is the general entry point: opts.OCREngine scopes the
// engine-scoped assertions, and opts.Corpus gives message_detected the
// engine-free anchor (see EvaluateOpts.Corpus). Evaluate and
// EvaluateWithOCREngine are its thin wrappers.
func (s *Scenario) EvaluateWith(evs []events.Envelope, stats pipeline.Stats, truth []Truth, opts EvaluateOpts) Report {
	return s.evaluate(evs, stats, truth, opts)
}

func (s *Scenario) evaluate(evs []events.Envelope, stats pipeline.Stats, truth []Truth, opts EvaluateOpts) Report {
	rep := Report{Results: make([]Result, 0, len(s.Assertions)+1)}
	for _, a := range s.Assertions {
		if len(a.Engines) > 0 && opts.OCREngine != "" && !sliceContains(a.Engines, opts.OCREngine) {
			rep.Results = append(rep.Results, Result{
				Type:   a.Type,
				Passed: true,
				Detail: fmt.Sprintf("not applicable: scoped to OCR engine(s) %s, run used OCR engine %q", strings.Join(a.Engines, ", "), opts.OCREngine),
			})
			continue
		}
		passed, detail := evaluateAssertion(a, evs, stats, truth, opts)
		rep.Results = append(rep.Results, Result{Type: a.Type, Passed: passed, Detail: detail})
	}
	// The implicit check is appended, not authorable: a scenario that forgot to
	// ask for it would otherwise pass a replay that lost a line.
	passed, detail := evalReplayNotDegraded(stats)
	rep.Results = append(rep.Results, Result{Type: "replay_not_degraded", Passed: passed, Detail: detail})
	rep.Passed = true
	for _, r := range rep.Results {
		if !r.Passed {
			rep.Passed = false
		}
	}
	return rep
}

// sliceContains reports whether s contains v. The engines scope is a closed,
// two-value list, so a linear scan is the whole implementation.
func sliceContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// evaluateAssertion dispatches one assertion. Load has already validated the
// shape, so an unknown type here is a bug, not author input.
func evaluateAssertion(a Assertion, evs []events.Envelope, stats pipeline.Stats, truth []Truth, opts EvaluateOpts) (bool, string) {
	switch a.Type {
	case "message_detected":
		return evalMessageDetected(a, evs, truth, opts.Corpus)
	case "message_committed_count":
		return evalCommittedCount(a, evs)
	case "committed_kind":
		return evalCommittedKind(a, evs)
	case "speech_cancelled":
		return evalSpeechCancelled(a, evs)
	case "no_duplicate_commit":
		return evalNoDuplicateCommit(evs)
	case "no_stale_generation_applied":
		return evalNoStaleGeneration(stats)
	case "events_order":
		return evalEventsOrder(a, evs)
	case "no_stale_speech":
		return evalNoStaleSpeech(a, evs, opts.CatchUp)
	case "no_duplicate_speech":
		return evalNoDuplicateSpeech(a, evs)
	case "late_result_discarded":
		return evalLateResultDiscarded(a, evs)
	case "duplicate_result_discarded":
		return evalDuplicateResultDiscarded(a, evs)
	case "no_voice_substitution":
		return evalNoVoiceSubstitution(a, evs)
	case "fault_count":
		return evalFaultCount(a, evs)
	case "committed_only":
		return evalCommittedOnly(a, evs)
	case "provider_degraded":
		return evalProviderDegraded(a, evs)
	case "speech_spoken_count":
		return evalSpeechSpokenCount(a, evs)
	case "speech_merged":
		return evalSpeechMerged(a, evs)
	case "speech_skipped":
		return evalSpeechSkipped(a, evs)
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
	// TextKind is the corpus's content class for the committed message
	// (dialogue, sign, item, …): "" when the corpus could not name it. The
	// narrated-class veto's own input, and the developer UI's class display.
	TextKind string `json:"text_kind"`
	// SpeechEligible is the session's commitment policy's verdict on the
	// commit (the narrated-class veto, the abstain, the decision's
	// safe_to_speak veto). The harness's speak gate and the runtime's speech
	// layer both read this flag.
	SpeechEligible bool `json:"speech_eligible"`
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
func evalMessageDetected(a Assertion, evs []events.Envelope, truth []Truth, corpus map[string]string) (bool, string) {
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
	// difference does not read as "the line never appeared". The anchor is
	// the committed line's identity, not the run's OCR text: when the
	// harness carries the profile's corpus and the commit names a corpus
	// message, the anchor is the corpus line (see EvaluateOpts.Corpus for
	// why the engine's read must not be the anchor). Both sides of the
	// comparison are fixture artifacts, so the check is a fact about the
	// clip and the commit, never about which tesseract build the runner
	// happens to ship.
	target := fold(p.NormalizedText)
	if target == "" {
		target = fold(p.Text)
	}
	if corpus != nil {
		if ct := corpus[p.MessageID]; ct != "" {
			target = ct
		}
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

// evalCommittedKind: the committed message's content class (docs/spec/05
// "Message class and speech rules") is the one the scenario names. The class
// is the corpus's stored text_kind for the committed message, read off the
// message.committed payload's text_kind field (the session's commitKind), so
// a sign that commits as dialogue — the matcher's region hint and the corpus
// class disagreeing — fails here rather than being spoken with the wrong
// voice and discovered later by a human.
func evalCommittedKind(a Assertion, evs []events.Envelope) (bool, string) {
	var kinds []string
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		p, ok := decodeCommitted(e)
		if !ok || p.MessageID != a.MessageID {
			continue
		}
		kinds = append(kinds, p.TextKind)
	}
	if len(kinds) == 0 {
		return false, fmt.Sprintf("no message.committed for message_id %q in %d events", a.MessageID, len(evs))
	}
	for _, k := range kinds {
		if k != a.Kind {
			return false, fmt.Sprintf("message_id %q committed with text_kind %q, want %q (all commits: %v)", a.MessageID, k, a.Kind, kinds)
		}
	}
	return true, fmt.Sprintf("message_id %q committed with text_kind %q (%d commit(s))", a.MessageID, a.Kind, len(kinds))
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

// evalReplayNotDegraded is the implicit replay-integrity assertion:
// a frame replay must be the recording it claims to be (docs/spec/12 "Frame
// replay"), so a frame whose OCR read the replay could not recover after its
// retry budget is a failed run, not a degraded one. It is appended to every
// report by Evaluate rather than listed in the scenario YAML, because a
// scenario that forgot to ask for it would pass a replay that lost a line; a
// run that degraded nothing reads zero and passes silently.
func evalReplayNotDegraded(stats pipeline.Stats) (bool, string) {
	if stats.ReplayDegradedFrames == 0 {
		return true, "no frame lost its OCR read (replay_degraded_frames = 0)"
	}
	return false, fmt.Sprintf("%d frame(s) lost their OCR read even after the replay retry policy: frame seq(s) %v",
		stats.ReplayDegradedFrames, stats.ReplayDegradedFrameSeqs)
}

// --- M7 provider-fault assertions -------------------------------------------
//
// These read the merged timeline: the pipeline's recognition events, the
// sidecar session's speech lifecycle and the harness's decision/provider
// events. They are the assertions AGENTS.md's failure-injection list owes
// ("dropped frames, delayed/duplicated model responses, endpoint loss, voice
// missing on a replica, rapid textbox advances") in a form a report can show.

// speechPayload is the subset of the speech event payloads the assertions
// read. The session's speech.cancelled payload has no voice_id: the
// cancellation names the occurrence, and the occurrence's voice is the one
// the ready/started events carried.
type speechPayload struct {
	MessageID  string `json:"message_id"`
	Occurrence string `json:"dialogue_occurrence_id"`
	VoiceID    string `json:"voice_id"`
	Reason     string `json:"reason"`
	Result     string `json:"result"`
	Error      string `json:"error"`
}

func decodeSpeech(e events.Envelope) (speechPayload, bool) {
	var p speechPayload
	if err := json.Unmarshal(e.Data, &p); err != nil {
		return speechPayload{}, false
	}
	return p, true
}

type decisionPayload struct {
	MessageID string `json:"message_id"`
	Accepted  bool   `json:"accepted"`
	Applied   bool   `json:"applied"`
	Reason    string `json:"reason"`
}

func decodeDecision(e events.Envelope) (decisionPayload, bool) {
	var p decisionPayload
	if err := json.Unmarshal(e.Data, &p); err != nil {
		return decisionPayload{}, false
	}
	return p, true
}

// lastCommittedMessageID is the message of the newest message.committed in
// the log: the line the dialogue is standing on at the end of the run.
func lastCommittedMessageID(evs []events.Envelope) string {
	last := ""
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		p, ok := decodeCommitted(e)
		if ok {
			last = p.MessageID
		}
	}
	return last
}

// evalNoStaleSpeech: no completed utterance (speech.ended with the terminal
// result "ended") for a line the dialogue has moved past. The last committed
// line legitimately ends; under latest_only every other line that finished
// speaking after the advance is the stale speech docs/spec/12 forbids — a
// line that was cancelled (speech.cancelled) is the correct behaviour, not a
// failure. Under catch_up the policy speaks the non-last line the player
// tapped while the previous one was still playing (ADR-0122 decision 2: the
// line that is playing keeps playing to the end), so a completed non-last
// line is the policy working, not stale speech; a line the batch window
// fenced never completes (it ends skipped) and is graded by the
// speech_spoken_count assertions instead.
func evalNoStaleSpeech(a Assertion, evs []events.Envelope, catchUp string) (bool, string) {
	last := lastCommittedMessageID(evs)
	catchUpMode := catchUp == session.CatchUpPolicyCatchUp
	var offenders []string
	endedTotal := 0
	for _, e := range evs {
		if e.Name != events.SpeechEnded {
			continue
		}
		p, ok := decodeSpeech(e)
		if !ok || p.MessageID != a.MessageID {
			continue
		}
		endedTotal++
		// A completed non-last line is stale only under latest_only, where the
		// advance is supposed to have cut it; under catch_up the policy
		// deliberately spoke it (the line that is playing runs out), so it is
		// not a violation (ADR-0122 decision 2).
		if p.Result == "ended" && a.MessageID != last && !catchUpMode {
			offenders = append(offenders, fmt.Sprintf("speech.ended result=ended for %q (last committed line is %q)", a.MessageID, last))
		}
	}
	if len(offenders) > 0 {
		return false, strings.Join(offenders, "; ")
	}
	if endedTotal == 0 {
		return true, fmt.Sprintf("no utterance completed for %q (cancelled or never started)", a.MessageID)
	}
	if catchUpMode {
		return true, fmt.Sprintf("no stale utterance for %q (%d speech.ended event(s); catch_up speaks the line it committed, a fenced one ends skipped and is graded by speech_spoken_count)", a.MessageID, endedTotal)
	}
	return true, fmt.Sprintf("no stale utterance for %q (%d speech.ended event(s), none a completed stale line)", a.MessageID, endedTotal)
}

// evalNoDuplicateSpeech: one utterance per occurrence. A duplicated provider
// reply must not become two speakings of the same line: the session's
// occurrence and phase fences refuse the second lifecycle, and this
// assertion is the timeline-side proof.
func evalNoDuplicateSpeech(a Assertion, evs []events.Envelope) (bool, string) {
	started, endedOK := map[string]int{}, map[string]int{}
	for _, e := range evs {
		var p speechPayload
		var ok bool
		switch e.Name {
		case events.SpeechStarted:
			p, ok = decodeSpeech(e)
			if !ok || p.MessageID != a.MessageID {
				continue
			}
			started[p.Occurrence]++
		case events.SpeechEnded:
			p, ok = decodeSpeech(e)
			if !ok || p.MessageID != a.MessageID || p.Result != "ended" {
				continue
			}
			endedOK[p.Occurrence]++
		}
	}
	var offenders []string
	for occ, n := range started {
		if n > 1 {
			offenders = append(offenders, fmt.Sprintf("occurrence %s started %d times", occ, n))
		}
	}
	for occ, n := range endedOK {
		if n > 1 {
			offenders = append(offenders, fmt.Sprintf("occurrence %s completed %d times", occ, n))
		}
	}
	if len(offenders) > 0 {
		return false, fmt.Sprintf("message %q: %s", a.MessageID, strings.Join(offenders, "; "))
	}
	return true, fmt.Sprintf("message %q: at most one utterance per occurrence (%d occurrence(s) started)", a.MessageID, len(started))
}

// --- Catch-up narration assertions (issue #25, ADR-0122) ---------------------
//
// These read the merged timeline the catch-up policy writes into: the
// session's own speech events (the immediate path's lifecycle) and the
// harness's catch-up records (the batched line's request, the merged run's
// request and its started, the fenced line's skipped terminal). A line is
// "spoken" by exactly the utterances that started with it in their covered
// ids: its own occurrence for an immediate line, the merged id's id list
// for a merged line.

// catchUpPayload is the subset of the catch-up policy's event payloads the
// assertions read: the run's covered ids (the merged cache key's id form,
// ADR-0122 decision 6) and the policy marker that separates the harness's
// catch-up records from the session's own speech events.
type catchUpPayload struct {
	MessageID  string   `json:"message_id"`
	MessageIDs []string `json:"message_ids"`
	Occurrence string   `json:"dialogue_occurrence_id"`
	CatchUp    string   `json:"catch_up"`
	Reason     string   `json:"reason"`
	Result     string   `json:"result"`
}

func decodeCatchUp(e events.Envelope) (catchUpPayload, bool) {
	var p catchUpPayload
	if err := json.Unmarshal(e.Data, &p); err != nil {
		return catchUpPayload{}, false
	}
	return p, true
}

// coversMessage reports whether one speech record covers the named line:
// the record's own message id (a bare form, exact match) or one of the
// merged run's covered ids (whole-segment, the merged form's exact match —
// the store's whole-segment rule, ADR-0122 decision 6).
func coversMessage(p catchUpPayload, id string) bool {
	if p.MessageID == id {
		return true
	}
	for _, seg := range p.MessageIDs {
		if seg == id {
			return true
		}
	}
	return false
}

// evalSpeechSpokenCount: the named line was spoken exactly count times —
// one started utterance per line the dialogue showed (the issue's "every
// message is spoken exactly once"): the session's speech.started for an
// immediate line, the merged run's speech.started for a merged line (the
// run's started carries its covered ids). A line the batch fenced or the
// run's end skipped has no started, so it counts zero: the fence is the
// point.
func evalSpeechSpokenCount(a Assertion, evs []events.Envelope) (bool, string) {
	var spoken int
	var speakers []string
	for _, e := range evs {
		if e.Name != events.SpeechStarted {
			continue
		}
		var p catchUpPayload
		if err := json.Unmarshal(e.Data, &p); err != nil || !coversMessage(p, a.MessageID) {
			continue
		}
		spoken++
		speakers = append(speakers, fmt.Sprintf("%s (message_id %s)", p.Occurrence, p.MessageID))
	}
	if spoken != a.Count {
		return false, fmt.Sprintf("message %q was spoken %d time(s), want %d: %s", a.MessageID, spoken, a.Count, strings.Join(speakers, ", "))
	}
	return true, fmt.Sprintf("message %q was spoken exactly %d time(s)", a.MessageID, spoken)
}

// evalSpeechMerged: one catch-up request covered exactly the named lines, in
// the named order — the merge the issue asks for ("merge consecutive
// same-speaker lines into one TTS request"): the harness's merged request
// carries the covered ids in commit order, the merged cache key's id form.
func evalSpeechMerged(a Assertion, evs []events.Envelope) (bool, string) {
	var found int
	for _, e := range evs {
		if e.Name != events.SpeechRequested {
			continue
		}
		p, ok := decodeCatchUp(e)
		if !ok || p.CatchUp != "merged" || len(p.MessageIDs) != len(a.MessageIDs) {
			continue
		}
		same := true
		for i := range a.MessageIDs {
			if p.MessageIDs[i] != a.MessageIDs[i] {
				same = false
				break
			}
		}
		if same {
			found++
		}
	}
	if found == 0 {
		return false, fmt.Sprintf("no catch-up request merged exactly %v in order", a.MessageIDs)
	}
	return true, fmt.Sprintf("catch-up request merged exactly %v in order (%d time(s))", a.MessageIDs, found)
}

// evalSpeechSkipped: the named line reached its one terminal skipped count
// times, the fence the issue owes ("Fencing (ADR 0092) still applies to
// anything older than the batch window", the max-age drop, the run's end):
// the speech.ended result=skipped the harness emits for the fenced line.
func evalSpeechSkipped(a Assertion, evs []events.Envelope) (bool, string) {
	var skipped int
	for _, e := range evs {
		if e.Name != events.SpeechEnded {
			continue
		}
		p, ok := decodeCatchUp(e)
		if !ok || p.Result != "skipped" || p.MessageID != a.MessageID {
			continue
		}
		if a.Reason != "" && p.Reason != a.Reason {
			continue
		}
		skipped++
	}
	if skipped != a.Count {
		return false, fmt.Sprintf("message %q was skipped %d time(s), want %d", a.MessageID, skipped, a.Count)
	}
	return true, fmt.Sprintf("message %q was skipped exactly %d time(s)", a.MessageID, skipped)
}

// staleReasons are the session fence reasons that mark a late provider result
// as discarded (docs/spec/01 invariant 2): the generation moved on, or the
// observation the answer describes was superseded.
var staleReasons = map[string]bool{"stale_generation": true, "stale_observation": true}

// evalLateResultDiscarded: a decision reply for the named line arrived after
// the dialogue advanced, and the session's fence discarded it. The discard is
// the safety property (docs/spec/17 "record(stale), discard"); this
// assertion reads the harness's decision.resolved record of it.
func evalLateResultDiscarded(a Assertion, evs []events.Envelope) (bool, string) {
	var found int
	var why string
	for _, e := range evs {
		if e.Name != events.DecisionResolved {
			continue
		}
		p, ok := decodeDecision(e)
		if !ok || p.MessageID != a.MessageID {
			continue
		}
		if !p.Applied && staleReasons[p.Reason] {
			found++
			why = p.Reason
		}
	}
	if found == 0 {
		return false, fmt.Sprintf("no decision.resolved discarding the late result for %q (want applied=false with reason stale_generation/stale_observation)", a.MessageID)
	}
	return true, fmt.Sprintf("late decision result for %q discarded %d time(s), last reason %q", a.MessageID, found, why)
}

// evalDuplicateResultDiscarded: the same decision reply was delivered twice
// and the session's duplicate-seq fence discarded the second delivery.
func evalDuplicateResultDiscarded(a Assertion, evs []events.Envelope) (bool, string) {
	var found int
	for _, e := range evs {
		if e.Name != events.DecisionResolved {
			continue
		}
		p, ok := decodeDecision(e)
		if !ok || p.MessageID != a.MessageID {
			continue
		}
		if !p.Applied && p.Reason == "duplicate_seq" {
			found++
		}
	}
	if found == 0 {
		return false, fmt.Sprintf("no decision.resolved discarding the duplicated delivery for %q (want applied=false with reason duplicate_seq)", a.MessageID)
	}
	return true, fmt.Sprintf("duplicated decision delivery for %q discarded %d time(s) by the duplicate-seq fence", a.MessageID, found)
}

// evalNoVoiceSubstitution: every speech event names the configured voice, and
// the voice layer reached a terminal outcome for it — either the correct
// voice completed (result "ended") or silence with a reason (result
// "failed" naming the voice-missing fault). A speech event naming any other
// voice — a builtin fallback, a different speaker — is the silent
// substitution docs/spec/16 M7 forbids.
func evalNoVoiceSubstitution(a Assertion, evs []events.Envelope) (bool, string) {
	var foreign []string
	terminalOK, terminalFailed := 0, 0
	for _, e := range evs {
		switch e.Name {
		case events.SpeechReady, events.SpeechStarted, events.SpeechEnded:
		default:
			continue
		}
		p, ok := decodeSpeech(e)
		if !ok {
			continue
		}
		if p.VoiceID == "" {
			continue
		}
		if p.VoiceID != a.Voice {
			foreign = append(foreign, fmt.Sprintf("%s names voice %q", e.Name, p.VoiceID))
			continue
		}
		if e.Name == events.SpeechEnded {
			switch {
			case p.Result == "ended":
				terminalOK++
			case p.Result == "failed" && p.Error != "":
				// A failure with a reason is a terminal outcome too:
				// "silence with a reason" (the utterance ended, the error
				// names the cause, the voice named is the requested one).
				// voice_missing failures — the canonical case — land here.
				terminalFailed++
			}
		}
	}
	if len(foreign) > 0 {
		return false, fmt.Sprintf("speech event(s) name a different voice: %s", strings.Join(foreign, "; "))
	}
	if terminalOK+terminalFailed == 0 {
		return false, fmt.Sprintf("no terminal speech outcome for voice %q (want result ended, or a failed naming the voice that caused the silence)", a.Voice)
	}
	return true, fmt.Sprintf("voice %q: no substitution (completed %d, silence-with-reason %d)", a.Voice, terminalOK, terminalFailed)
}

type faultPayload struct {
	Kind string `json:"kind"`
}

// evalFaultCount: the fault.injected events of the named kind occurred exactly
// count times. One event per fault entry (first activation), so a scenario
// with one drop_frames entry counts one; the unit tests in faults_test.go
// prove each fault fires at its configured time.
func evalFaultCount(a Assertion, evs []events.Envelope) (bool, string) {
	n := 0
	for _, e := range evs {
		if e.Name != eventNameFaultInjected {
			continue
		}
		var p faultPayload
		if err := json.Unmarshal(e.Data, &p); err != nil {
			continue
		}
		if p.Kind == a.Kind {
			n++
		}
	}
	if n != a.Count {
		return false, fmt.Sprintf("fault %q injected %d time(s), want %d", a.Kind, n, a.Count)
	}
	return true, fmt.Sprintf("fault %q injected %d time(s), as expected", a.Kind, n)
}

// evalCommittedOnly: every committed message names one of the listed lines.
// The matcher can only commit profile messages, but an assertion that proves
// it keeps the report honest about \"no invented line\" (GN-T014's
// wording) when a provider fault tempts a fallback path.
func evalCommittedOnly(a Assertion, evs []events.Envelope) (bool, string) {
	allowed := make(map[string]bool, len(a.Names))
	for _, n := range a.Names {
		allowed[n] = true
	}
	var offenders []string
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
		if !allowed[p.MessageID] {
			offenders = append(offenders, p.MessageID)
		}
	}
	if len(offenders) > 0 {
		return false, fmt.Sprintf("commit(s) outside the allowed lines: %v (allowed %v)", offenders, a.Names)
	}
	return true, fmt.Sprintf("%d commit(s), all within %v", total, a.Names)
}

type degradedPayload struct {
	Role     string `json:"role"`
	Degraded bool   `json:"degraded"`
}

// evalProviderDegraded: the named role went degraded on the timeline, and —
// when the assertion asks for it — recovered: a later provider.degraded with
// degraded=false for the same role. The pair is the \"clean degraded state\"
// docs/spec/13 owes: the failure is visible, and the recovery is not a
// silent guess.
func evalProviderDegraded(a Assertion, evs []events.Envelope) (bool, string) {
	var degradedAt int
	recoveredAt := -1
	for i, e := range evs {
		if e.Name != events.ProviderDegraded {
			continue
		}
		var p degradedPayload
		if err := json.Unmarshal(e.Data, &p); err != nil || p.Role != a.Role {
			continue
		}
		if p.Degraded {
			if degradedAt < 0 {
				degradedAt = i
			}
		} else if degradedAt >= 0 && i > degradedAt {
			recoveredAt = i
		}
	}
	if degradedAt < 0 {
		return false, fmt.Sprintf("no provider.degraded with degraded=true for role %q", a.Role)
	}
	if a.Recovered && recoveredAt < 0 {
		return false, fmt.Sprintf("provider %q degraded (event %d) but no later provider.degraded with degraded=false", a.Role, degradedAt)
	}
	return true, fmt.Sprintf("provider %q: degradation visible%s", a.Role, map[bool]string{true: ", recovery after it", false: ""}[a.Recovered])
}
