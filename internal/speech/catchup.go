package speech

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Catch-up narration and the no-backlog speech policy (issue #25, ADR-0122).
//
// This file is the shared decision core of the policy: the vocabulary, the
// options with their documented defaults, the batch's window-drop rule, the
// speaker-run split, the sentence-spacing join, the versioned merged-id form
// for the cache key, and the box-change hook's interface. The runtime's
// speech wiring and the scenario harness's sidecar both drive this core, so
// the merge and staleness decisions have one implementation and one set of
// unit tests; the sides own the voice resolution, the TTS calls and the
// events.

// CatchUpPolicy names the session's catch-up narration policy (issue #25).
const (
	// CatchUpPolicyCatchUp batches the lines committed while an utterance is
	// playing and speaks them back-to-back, merged per speaker (the default).
	CatchUpPolicyCatchUp = "catch_up"
	// CatchUpPolicyLatestOnly keeps the historical behaviour: a speakable
	// commit cancels the playing/queued audio of older generations and
	// speaks the newest line at once (ADR-0092's immediate shape).
	CatchUpPolicyLatestOnly = "latest_only"
)

// ValidateCatchUpPolicy normalizes one policy word: the empty word is the
// default (catch_up), a known word is returned as given, and anything else
// is refused — the settings route answers 400 on a foreign word, like the
// decision mode.
func ValidateCatchUpPolicy(policy string) (string, error) {
	switch policy {
	case "":
		return CatchUpPolicyCatchUp, nil
	case CatchUpPolicyCatchUp, CatchUpPolicyLatestOnly:
		return policy, nil
	default:
		return "", fmt.Errorf("speech: catch-up policy %q is not %q or %q", policy, CatchUpPolicyCatchUp, CatchUpPolicyLatestOnly)
	}
}

// Defaults for the catch-up policy (ADR-0122).
const (
	// DefaultCatchUpBatchWindow is how long a line committed while an
	// utterance is playing stays speakable: a newer line more than the
	// window after it fences it (the issue's "Fencing (ADR 0092) still
	// applies to anything older than the batch window"), and the window is
	// also the flush timer (the batch flushes when it elapses since the
	// batch's first line). 900 ms is the measured 600 ms default widened to
	// admit the recorded Dolphin rapid-A commit cadence (~779 ms apart, the
	// frame-capture + OCR cadence of the live catch-up runs): consecutive
	// commits batch and merge per speaker instead of each flushing its own
	// utterance, while anything older than 900 ms still fences.
	DefaultCatchUpBatchWindow = 900 * time.Millisecond
	// DefaultCatchUpMaxUtteranceAge is the per-utterance max age (issue #25
	// item 1): an utterance whose audio is ready more than this long after
	// the newest line it covers committed is dropped before it starts. It
	// is the dismiss-without-successor backstop ADR-0092 deferred — the
	// box is gone, no commit moved the fence, and only the age bound keeps
	// the late audio off the sink.
	DefaultCatchUpMaxUtteranceAge = 3 * time.Second
	// DefaultCatchUpApprovedCap bounds the fence's approved set of catch-up
	// batch generations (ADR-0122 decision 4): a live batch holds a handful
	// of generations at most, and the cap keeps the fence's extra state
	// bounded the way the wiring's own cancel tables are.
	DefaultCatchUpApprovedCap = 16
	// DefaultCatchUpBacklogCap is the catch-up backlog safety cap (ADR-0175
	// amendment, the owner's "say all that"): a page the player passed is
	// never dropped for staleness any more — every passed page is spoken
	// once, in order — but the narrator must not lag unboundedly. The
	// pending batch's estimated audio beyond this budget is dropped from
	// the OLDEST pages (reason catch_up_backlog_cap, counted and logged),
	// so a sustained mash keeps speaking the newest ~45 s of the backlog
	// instead of queueing minutes.
	DefaultCatchUpBacklogCap = 45 * time.Second
)

// CatchUpOptions is one session's catch-up policy tuning. Zero fields take
// the documented defaults.
type CatchUpOptions struct {
	// Policy: CatchUpPolicyCatchUp (default) or CatchUpPolicyLatestOnly.
	Policy string
	// BatchWindow: the staleness and flush window (0 selects
	// DefaultCatchUpBatchWindow).
	BatchWindow time.Duration
	// MaxUtteranceAge: the per-utterance max age (0 selects
	// DefaultCatchUpMaxUtteranceAge). The catch_up policy no longer drops on
	// it (ADR-0175 amendment): a passed page is spoken, however late it
	// flushes — the bound remains for the sidecars that keep the no-backlog
	// answer.
	MaxUtteranceAge time.Duration
	// BacklogCap: the catch-up backlog safety cap — the oldest pending pages
	// beyond this much estimated audio are dropped (reason
	// catch_up_backlog_cap) so the narrator never lags unboundedly (0
	// selects DefaultCatchUpBacklogCap).
	BacklogCap time.Duration
}

// NormalizeCatchUpOptions resolves the zero fields to the documented
// defaults and validates the policy word (the same rule the settings route
// enforces, so a construction that names a foreign word is a programming
// error, not a policy).
func (o CatchUpOptions) NormalizeCatchUpOptions() (CatchUpOptions, error) {
	policy, err := ValidateCatchUpPolicy(o.Policy)
	if err != nil {
		return CatchUpOptions{}, err
	}
	c := CatchUpOptions{Policy: policy}
	if o.BatchWindow > 0 {
		c.BatchWindow = o.BatchWindow
	} else {
		c.BatchWindow = DefaultCatchUpBatchWindow
	}
	if o.MaxUtteranceAge > 0 {
		c.MaxUtteranceAge = o.MaxUtteranceAge
	} else {
		c.MaxUtteranceAge = DefaultCatchUpMaxUtteranceAge
	}
	if o.BacklogCap > 0 {
		c.BacklogCap = o.BacklogCap
	} else {
		c.BacklogCap = DefaultCatchUpBacklogCap
	}
	return c, nil
}

// CatchUpLine is one committed line the catch-up batcher may merge. The
// sides fill the speaker (the line's resolved speaker character: the
// assigned character, or the narrator when unattributed) and the committed
// wall clock (the instant the commit reached the speech side — the batch
// decisions are wall-clock decisions, like the max age).
type CatchUpLine struct {
	Generation  int64
	Occurrence  string
	MessageID   string
	RegionID    string
	Text        string
	Speaker     string
	CommittedAt time.Time
}

// CatchUpRun is one merged utterance: a maximal run of consecutive
// same-speaker lines in commit order. One run is one TTS request (ADR-0122
// decision 3: the merge is the budget win).
type CatchUpRun struct {
	// Speaker is the run's speaker character (all of the run's lines agree).
	Speaker string
	// Text is the merged speech text (JoinSpeechText of the run's lines).
	Text string
	// Lines is the run's lines in commit order — the identity the events and
	// the merged cache key carry.
	Lines []CatchUpLine
}

// MessageIDs is the run's covered message ids in commit order (the merged
// cache key's id form, ADR-0122 decision 6).
func (r CatchUpRun) MessageIDs() []string {
	ids := make([]string, len(r.Lines))
	for i, l := range r.Lines {
		ids[i] = l.MessageID
	}
	return ids
}

// LatestCommittedAt is the newest commit in the run — the max-age anchor
// (ADR-0122 decision 5): the run's age is measured from the newest line it
// covers, because that line is the box the player was on when the run's
// audio became ready.
func (r CatchUpRun) LatestCommittedAt() time.Time {
	var t time.Time
	for _, l := range r.Lines {
		if l.CommittedAt.After(t) {
			t = l.CommittedAt
		}
	}
	return t
}

// OldestCommittedAt is the oldest commit in the run (the report's
// staleness reading).
func (r CatchUpRun) OldestCommittedAt() time.Time {
	var t time.Time
	for i, l := range r.Lines {
		if i == 0 || l.CommittedAt.Before(t) {
			t = l.CommittedAt
		}
	}
	return t
}

// SplitRuns groups commit-ordered lines into maximal consecutive
// same-speaker runs (ADR-0122 decision 3). A run of one line is still a run:
// the merge is about the TTS budget, and a speaker change always splits,
// whatever the line lengths.
func SplitRuns(lines []CatchUpLine) []CatchUpRun {
	var runs []CatchUpRun
	var cur []CatchUpLine
	flush := func() {
		if len(cur) == 0 {
			return
		}
		runs = append(runs, CatchUpRun{
			Speaker: cur[0].Speaker,
			Text:    JoinSpeechText(textsOf(cur)),
			Lines:   append([]CatchUpLine(nil), cur...),
		})
		cur = nil
	}
	for _, l := range lines {
		if len(cur) > 0 && cur[0].Speaker != l.Speaker {
			flush()
		}
		cur = append(cur, l)
	}
	flush()
	return runs
}

func textsOf(lines []CatchUpLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

// JoinSpeechText merges a run's lines into one speech text with sentence
// spacing (ADR-0122 decision 3): every line is closed to sentence-final
// punctuation when it lacks it (a line that ends mid-phrase would otherwise
// run into the next line's first word as one long breath), and the lines are
// joined with a single space. The rule is deterministic and idempotent on
// already-punctuated lines, so a replay and a live run of the same lines
// compute the same text — and the same cache key.
func JoinSpeechText(texts []string) string {
	parts := make([]string, 0, len(texts))
	for _, raw := range texts {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if !endsSentenceFinal(s) {
			s += "."
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

func endsSentenceFinal(s string) bool {
	if s == "" {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s)
	switch r {
	case '.', '!', '?', '…':
		return true
	}
	return false
}

// MergedIDVersion is the merge-scheme version of the merged-id form
// (ADR-0122 decision 6): a changed merge scheme (a different join, a
// different ordering) bumps the version so the same lines under a new
// scheme compute a different cache key instead of silently reusing the old
// bytes.
const MergedIDVersion = 1

// MergedOriginalID is the KeyInput.OriginalMessageID form of a merged run:
// "m<version>:id1,id2,…" — the run's covered message ids in commit order
// under the merge-scheme version. A single-line run keeps the bare id
// (unchanged, so existing cache keys are stable); an empty run names
// nothing.
func MergedOriginalID(messageIDs []string) string {
	if len(messageIDs) == 1 {
		return messageIDs[0]
	}
	if len(messageIDs) == 0 {
		return ""
	}
	return fmt.Sprintf("m%d:%s", MergedIDVersion, strings.Join(messageIDs, ","))
}

// IsMergedOriginalID reports whether an OriginalMessageID (a cache key's
// input, or a provenance column) is a merged form: "m<digits>:…" with at
// least one digit and a non-empty id list.
func IsMergedOriginalID(id string) bool {
	rest, ok := strings.CutPrefix(id, "m")
	if !ok {
		return false
	}
	digits, after, found := strings.Cut(rest, ":")
	if !found || after == "" || digits == "" {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// MergedIDsIn reports whether the (possibly merged) id form names any of
// ids as one of its segments. It is the provenance match the ADR-0107 purge
// needs for merged rows: the bare form is an exact match, and the merged
// form matches when any covered id is named — whole-segment only, so an id
// that is a prefix of another segment cannot match it (the id alphabet has
// no separators or LIKE metacharacters, which is what makes the store's
// LIKE fallback an exact-segment match).
func MergedIDsIn(id string, ids []string) bool {
	if !IsMergedOriginalID(id) {
		for _, want := range ids {
			if id == want {
				return true
			}
		}
		return false
	}
	_, after, _ := strings.Cut(id, ":")
	for _, seg := range strings.Split(after, ",") {
		for _, want := range ids {
			if seg == want {
				return true
			}
		}
	}
	return false
}

// BatchWindowDrop reports which of the pending lines (commit order, oldest
// first) a new line landing at committedAt fences (ADR-0122 decision 3):
// the drop is a prefix of the list — every pending line the new line is
// more than the window after. The caller terminalizes exactly those lines
// (reason catch_up_stale) and keeps the rest, then appends the new line.
// A zero window fences every older line at the first newer line; the
// window is a policy value, so zero here means "unset" and takes the
// default, the same rule NormalizeCatchUpOptions applies.
func BatchWindowDrop(pending []CatchUpLine, committedAt time.Time, window time.Duration) int {
	if window <= 0 {
		window = DefaultCatchUpBatchWindow
	}
	n := 0
	for n < len(pending) && committedAt.Sub(pending[n].CommittedAt) > window {
		n++
	}
	return n
}

// catchUpEstimatePerWord is the backlog-cap estimate's speech rate: 150 wpm
// (400 ms per word) — the conservative side of ordinary speech, so a batch
// that fits the cap at the estimate fits it in practice at a faster voice.
const catchUpEstimatePerWord = 400 * time.Millisecond

// EstimateCatchUpAudioDuration estimates a batch's total playback from its
// texts (the backlog cap's input, where the audio is not synthesised yet —
// the estimate rides on the text, so it is deterministic on the synthetic
// event pages the tests drive). Every line counts at least one word: a
// blank text is still a page the player passed.
func EstimateCatchUpAudioDuration(lines []CatchUpLine) time.Duration {
	var total time.Duration
	for _, l := range lines {
		words := len(strings.Fields(l.Text))
		if words < 1 {
			words = 1
		}
		total += time.Duration(words) * catchUpEstimatePerWord
	}
	return total
}

// BacklogCapDrop reports which of the pending lines (commit order, oldest
// first) fall beyond the backlog cap (ADR-0175 amendment): the oldest-first
// prefix whose removal leaves the remainder's estimated audio at or under
// the cap. The caller terminalises exactly those lines (reason
// catch_up_backlog_cap, counted and logged); the rest keep waiting to be
// spoken. A zero cap takes the default, the same unset rule
// NormalizeCatchUpOptions applies.
func BacklogCapDrop(pending []CatchUpLine, limit time.Duration) int {
	if limit <= 0 {
		limit = DefaultCatchUpBacklogCap
	}
	n := 0
	for n < len(pending) && EstimateCatchUpAudioDuration(pending[n:]) > limit {
		n++
	}
	return n
}

// BoxChangeNotifier is the speech side's seam for the issue's item-2
// box-change priority (ADR-0122 decision 7): the moment the capture/pipeline
// side detects a dialogue box appear or change — ahead of the OCR read and
// the commit that follows — it may notify the session's speech pipeline,
// which flushes a pending catch-up batch immediately so the merged playback
// starts on the new box instead of at the window's end. The pipeline holds
// this interface and wires its detector to it in a later change; a nil
// notifier is a no-op.
type BoxChangeNotifier interface {
	// NotifyBoxChange reports a dialogue box appearing or changing on
	// screen. It must be cheap and non-blocking: the caller is on the
	// capture path.
	NotifyBoxChange()
}
