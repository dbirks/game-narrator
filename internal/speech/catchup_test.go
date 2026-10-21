package speech

// The catch-up policy's decision core (issue #25, ADR-0122): the speaker-run
// split, the sentence-spacing join, the window-drop rule, the versioned
// merged-id form and the policy vocabulary. Synthetic inputs throughout
// (AGENTS.md rule 3 class (a)): the lines are invented, no game text.

import (
	"testing"
	"time"
)

func tAt(s float64) time.Time { return time.Unix(0, int64(s*float64(time.Second))).UTC() }

func TestValidateCatchUpPolicy(t *testing.T) {
	cases := []struct {
		in, want string
		err      bool
	}{
		{"", CatchUpPolicyCatchUp, false},
		{CatchUpPolicyCatchUp, CatchUpPolicyCatchUp, false},
		{CatchUpPolicyLatestOnly, CatchUpPolicyLatestOnly, false},
		{"catchup", "", true},
		{"LATEST_ONLY", "", true},
		{"batch", "", true},
	}
	for _, c := range cases {
		got, err := ValidateCatchUpPolicy(c.in)
		if c.err != (err != nil) {
			t.Errorf("ValidateCatchUpPolicy(%q) error = %v, want err %v", c.in, err, c.err)
			continue
		}
		if !c.err && got != c.want {
			t.Errorf("ValidateCatchUpPolicy(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeCatchUpOptionsDefaults(t *testing.T) {
	got, err := (CatchUpOptions{}).NormalizeCatchUpOptions()
	if err != nil {
		t.Fatalf("normalize zero options: %v", err)
	}
	if got.Policy != CatchUpPolicyCatchUp || got.BatchWindow != DefaultCatchUpBatchWindow || got.MaxUtteranceAge != DefaultCatchUpMaxUtteranceAge {
		t.Errorf("zero options normalized to %+v, want the documented defaults", got)
	}
	got, err = (CatchUpOptions{Policy: CatchUpPolicyLatestOnly, BatchWindow: time.Second, MaxUtteranceAge: 10 * time.Second}).NormalizeCatchUpOptions()
	if err != nil {
		t.Fatalf("normalize explicit options: %v", err)
	}
	if got.Policy != CatchUpPolicyLatestOnly || got.BatchWindow != time.Second || got.MaxUtteranceAge != 10*time.Second {
		t.Errorf("explicit options not preserved: %+v", got)
	}
	if _, err := (CatchUpOptions{Policy: "nope"}).NormalizeCatchUpOptions(); err == nil {
		t.Fatal("a foreign policy word normalized; it must be a construction error")
	}
}

func TestSplitRunsMergesConsecutiveSameSpeaker(t *testing.T) {
	lines := []CatchUpLine{
		{Speaker: "a", Text: "one", MessageID: "1"},
		{Speaker: "a", Text: "two", MessageID: "2"},
		{Speaker: "b", Text: "three", MessageID: "3"},
		{Speaker: "b", Text: "four", MessageID: "4"},
		{Speaker: "a", Text: "five", MessageID: "5"},
	}
	runs := SplitRuns(lines)
	if len(runs) != 3 {
		t.Fatalf("split %d runs, want 3 (a a / b b / a): %+v", len(runs), runs)
	}
	want := []struct {
		speaker string
		ids     []string
	}{
		{"a", []string{"1", "2"}},
		{"b", []string{"3", "4"}},
		{"a", []string{"5"}},
	}
	for i, w := range want {
		if runs[i].Speaker != w.speaker {
			t.Errorf("run %d speaker = %q, want %q", i, runs[i].Speaker, w.speaker)
		}
		ids := runs[i].MessageIDs()
		for j := range w.ids {
			if j >= len(ids) || ids[j] != w.ids[j] {
				t.Errorf("run %d ids = %v, want %v", i, ids, w.ids)
				break
			}
		}
	}
	// A speaker that returns later is a new run, never folded into the old.
	if runs[2].Speaker != "a" || len(runs[2].Lines) != 1 {
		t.Errorf("the returning speaker must be its own run: %+v", runs[2])
	}
}

func TestSplitRunsAlternatingSpeakersNeverMerges(t *testing.T) {
	lines := []CatchUpLine{
		{Speaker: "a", Text: "one", MessageID: "1"},
		{Speaker: "b", Text: "two", MessageID: "2"},
		{Speaker: "a", Text: "three", MessageID: "3"},
	}
	runs := SplitRuns(lines)
	if len(runs) != 3 {
		t.Fatalf("alternating speakers split %d runs, want 3", len(runs))
	}
	for i, r := range runs {
		if len(r.Lines) != 1 {
			t.Errorf("run %d holds %d lines; a speaker change must always split", i, len(r.Lines))
		}
	}
}

func TestSplitRunsEmptyAndSingle(t *testing.T) {
	if runs := SplitRuns(nil); len(runs) != 0 {
		t.Errorf("empty input split %d runs, want 0", len(runs))
	}
	runs := SplitRuns([]CatchUpLine{{Speaker: "a", Text: "only", MessageID: "x"}})
	if len(runs) != 1 || len(runs[0].Lines) != 1 {
		t.Fatalf("one line split to %+v, want one single-line run", runs)
	}
}

func TestJoinSpeechTextSentenceSpacing(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{in: []string{"One.", "Two.", "Three."}, want: "One. Two. Three."},
		// Lines that lack the final punctuation are closed, so the join is
		// sentences, not one long breath.
		{in: []string{"One", "Two", "Three"}, want: "One. Two. Three."},
		{in: []string{"Really?", "No!", "Maybe…", "Done"}, want: "Really? No! Maybe… Done."},
		// The rule is idempotent on already-punctuated input: joining the
		// same lines twice must not pile on punctuation (the cache key must
		// be stable across the replay and the live run).
		{in: []string{"One.", "Two."}, want: "One. Two."},
		// Whitespace is trimmed; an empty line is skipped, never a dot.
		{in: []string{"  One  ", "", "Two"}, want: "One. Two."},
		{in: []string{}, want: ""},
	}
	for i, c := range cases {
		got := JoinSpeechText(c.in)
		if got != c.want {
			t.Errorf("case %d: JoinSpeechText(%v) = %q, want %q", i, c.in, got, c.want)
		}
	}
	// Guard the idempotence reading: the join of the join's single segment
	// (plus an empty element, which is skipped) is the same text.
	if got := JoinSpeechText(append([]string{JoinSpeechText([]string{"One.", "Two."})}, "")); got != "One. Two." {
		t.Errorf("re-joining the merged text = %q, want %q (idempotent)", got, "One. Two.")
	}
}

func TestSplitRunsCarriesTheMergedText(t *testing.T) {
	runs := SplitRuns([]CatchUpLine{
		{Speaker: "a", Text: "the first", MessageID: "1"},
		{Speaker: "a", Text: "the second", MessageID: "2"},
	})
	if len(runs) != 1 {
		t.Fatalf("want one run, got %d", len(runs))
	}
	if runs[0].Text != "the first. the second." {
		t.Errorf("run text = %q, want the sentence-spaced merge", runs[0].Text)
	}
}

func TestRunCommitAnchors(t *testing.T) {
	runs := SplitRuns([]CatchUpLine{
		{Speaker: "a", MessageID: "1", CommittedAt: tAt(0)},
		{Speaker: "a", MessageID: "2", CommittedAt: tAt(0.9)},
	})
	if got := runs[0].LatestCommittedAt(); got != tAt(0.9) {
		t.Errorf("latest commit = %v, want the newest line's commit (the max-age anchor)", got)
	}
	if got := runs[0].OldestCommittedAt(); got != tAt(0) {
		t.Errorf("oldest commit = %v, want the oldest line's commit", got)
	}
}

func TestMergedOriginalIDForm(t *testing.T) {
	// The bare form is unchanged: one line (or none) never takes the merge
	// form, so existing single-line cache keys are stable.
	if got := MergedOriginalID([]string{"abc"}); got != "abc" {
		t.Errorf("single id form = %q, want the bare id", got)
	}
	if got := MergedOriginalID(nil); got != "" {
		t.Errorf("empty form = %q, want the empty id", got)
	}
	// The merged form: versioned, commit order.
	got := MergedOriginalID([]string{"a1", "b2", "c3"})
	if want := "m1:a1,b2,c3"; got != want {
		t.Errorf("merged form = %q, want %q", got, want)
	}
	if !IsMergedOriginalID(got) {
		t.Errorf("the merged form does not read as merged: %q", got)
	}
	for _, bare := range []string{"", "a1", "m", "m:", "mx:", "1:", "m-1:a1,b2"} {
		if IsMergedOriginalID(bare) {
			t.Errorf("IsMergedOriginalID(%q) = true, want false", bare)
		}
	}
	// Commit order is the identity: a different order of the same ids is a
	// different form (and would compute a different cache key).
	if MergedOriginalID([]string{"b2", "a1"}) == MergedOriginalID([]string{"a1", "b2"}) {
		t.Error("a different commit order produced the same merged form; the order is part of the identity")
	}
	// A version bump is a different form: the scheme version is in the key.
	if got := MergedIDVersion; got != 1 {
		t.Errorf("MergedIDVersion = %d, want 1 (the documented form)", got)
	}
}

func TestMergedIDsInSegments(t *testing.T) {
	id := "m1:aaab,bbcc,ccdd"
	for _, want := range []string{"aaab", "bbcc", "ccdd"} {
		if !MergedIDsIn(id, []string{want}) {
			t.Errorf("MergedIDsIn(%q) misses %q", id, want)
		}
	}
	// Whole-segment only: a prefix of a segment is not the segment.
	if MergedIDsIn(id, []string{"aaa"}) {
		t.Error("a prefix of a segment matched; the segment match must be whole")
	}
	if MergedIDsIn(id, []string{"aaab,bbcc"}) {
		t.Error("a join of two segments matched as one; the split is on the form's commas only")
	}
	// The bare form is an exact match.
	if !MergedIDsIn("aaab", []string{"aaab"}) {
		t.Error("the bare form must exact-match its own id")
	}
	if MergedIDsIn("aaab", []string{"bbcc"}) {
		t.Error("the bare form matched a foreign id")
	}
	// An empty id list matches nothing.
	if MergedIDsIn(id, nil) {
		t.Error("an empty id list matched; nothing was named")
	}
}

func TestBatchWindowDropFencesTheOldPrefix(t *testing.T) {
	window := 600 * time.Millisecond
	pending := []CatchUpLine{
		{CommittedAt: tAt(0)},
		{CommittedAt: tAt(0.2)},
		{CommittedAt: tAt(0.5)},
	}
	// A line at 1.0 s fences the lines at 0.0 and 0.2 (both more than the
	// window before it) and keeps 0.5 (500 ms inside).
	if n := BatchWindowDrop(pending, tAt(1.0), window); n != 2 {
		t.Errorf("drop prefix = %d, want 2 (the window fences the old lines, keeps the recent)", n)
	}
	// A line inside the window of the whole batch fences nothing.
	if n := BatchWindowDrop(pending, tAt(0.6), window); n != 0 {
		t.Errorf("drop prefix = %d, want 0 (every pending line is inside the window)", n)
	}
	// Past the window by a hair, the oldest line is fenced: the bound is
	// "older than the batch window" — at the window it keeps, over it it drops.
	if n := BatchWindowDrop(pending, tAt(0.61), window); n != 1 {
		t.Errorf("the just-past-window drop = %d, want 1 (only the oldest line is older than the window)", n)
	}
	// An empty batch drops nothing.
	if n := BatchWindowDrop(nil, tAt(9), window); n != 0 {
		t.Errorf("empty batch drop = %d, want 0", n)
	}
	// A zero window takes the default, never fences everything: zero is the
	// "unset" value the options normalizer treats that way.
	if n := BatchWindowDrop(pending, tAt(9), 0); n != 3 {
		t.Errorf("zero-window drop = %d, want all three (the default window against a far-new line)", n)
	}
}
