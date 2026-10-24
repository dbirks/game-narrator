// Package e2e — the A/V harness gate unit tests (ADR 0143, Amendment 1):
// the gate's rules pinned on synthetic reports (avGateFailures is pure
// over the report, so each rule is tested in isolation), the box-position
// classifier's boundaries, and the report.md writer's shape. It runs in
// the ordinary go test (no GN_GATE, no emulator, no providers).
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// avGateBaselineReport is the passing warm-run report: three idle lines
// with the settled→audio percentiles inside the bounds, no missed, no
// phantom, no stale, no duplicate, every expected voice matched and every
// spoken line's WER under the bound. avGateFailures on it must be empty.
func avGateBaselineReport() avTimelineReport {
	rep := avTimelineReport{
		RunID: "run-av-gate", IdleSamples: 3,
		SettledToAudioP50MS: 200, SettledToAudioP95MS: 600,
		Lines: []avLineRecord{
			{Index: 0, TAudioStartEventMS: 1000, ExpectedVoiceID: "av-voice-char", VoiceID: "av-voice-char", VoiceMatch: true, ASRWERvsScript: 0.05, asrScored: true, EndResult: "ended", BoxPosition: "middle"},
			{Index: 1, TAudioStartEventMS: 2000, ExpectedVoiceID: "av-voice-char", VoiceID: "av-voice-char", VoiceMatch: true, ASRWERvsScript: 0.10, asrScored: true, EndResult: "ended", BoxPosition: "top"},
			{Index: 2, TAudioStartEventMS: 3000, ExpectedVoiceID: "av-voice-narr", VoiceID: "av-voice-narr", VoiceMatch: true, ASRWERvsScript: 0.02, asrScored: true, EndResult: "ended", BoxPosition: "bottom", Batched: true, RunPosition: 0, QueuedBehindSpeech: true},
		},
	}
	return rep
}

// avGateWantsExactly checks the failure set's size and that every wanted
// substring occurs in exactly one failure message (one message may carry
// several of them — a message that names the rule, the number and the line).
func avGateWantsExactly(t *testing.T, rep avTimelineReport, wantCount int, want []string) {
	t.Helper()
	fails := avGateFailures(rep, false)
	if len(fails) != wantCount {
		t.Fatalf("avGateFailures = %d failure(s), want %d:\n%s", len(fails), wantCount, strings.Join(fails, "\n"))
	}
	for _, w := range want {
		found := 0
		for _, f := range fails {
			if strings.Contains(f, w) {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("want exactly one failure containing %q; found %d in:\n%s", w, found, strings.Join(fails, "\n"))
		}
	}
}

func TestAVHarnessGateRules(t *testing.T) {
	// The baseline passes: the warm run the gate exists to protect.
	if fails := avGateFailures(avGateBaselineReport(), false); len(fails) != 0 {
		t.Fatalf("baseline report: %d failure(s), want 0:\n%s", len(fails), strings.Join(fails, "\n"))
	}

	t.Run("too few idle lines", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.IdleSamples = 2 // the 2/2 ms run: not a distribution
		avGateWantsExactly(t, rep, 1, []string{"idle samples = 2", "too few idle lines to measure"})
	})
	t.Run("settled p50 over bound", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.SettledToAudioP50MS = 501
		avGateWantsExactly(t, rep, 1, []string{"settled→audio p50 = 501 ms > 500 ms"})
	})
	t.Run("settled p95 over bound", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.SettledToAudioP95MS = 801
		avGateWantsExactly(t, rep, 1, []string{"settled→audio p95 = 801 ms > 800 ms"})
	})
	t.Run("missed lines", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.MissedLines = 1
		avGateWantsExactly(t, rep, 1, []string{"1 missed line(s)"})
	})
	t.Run("phantom utterances", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.PhantomUtterances = 2
		rep.PhantomReadThroughs = 1
		avGateWantsExactly(t, rep, 1, []string{"2 phantom utterance(s) (1 read-through)"})
	})
	t.Run("stale starts", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.StaleStarts = 1
		avGateWantsExactly(t, rep, 1, []string{"1 stale start(s)"})
	})
	t.Run("duplicate commits", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.DuplicateCommits = 1
		avGateWantsExactly(t, rep, 1, []string{"1 duplicate commit(s)"})
	})
	t.Run("too few committed lines", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.Lines = rep.Lines[:1]
		avGateWantsExactly(t, rep, 1, []string{"1 committed lines; want >= 2"})
	})
	t.Run("voice mismatch names the line", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.Lines[1].VoiceID = "av-voice-narr" // unattributed in the character's line
		rep.Lines[1].VoiceMatch = false
		avGateWantsExactly(t, rep, 1, []string{"line 1: spoken in voice \"av-voice-narr\"", "expected voice is \"av-voice-char\""})
	})
	t.Run("a line without an expected voice is skipped", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.Lines[2].ExpectedVoiceID = "" // no expectation: the rule cannot fail it
		rep.Lines[2].VoiceMatch = false
		if fails := avGateFailures(rep, false); len(fails) != 0 {
			t.Fatalf("no-expected-voice line: %d failure(s), want 0:\n%s", len(fails), strings.Join(fails, "\n"))
		}
	})
	t.Run("spoken line WER over bound names the line", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.Lines[2].ASRWERvsScript = 0.40
		avGateWantsExactly(t, rep, 1, []string{"line 2: ASR WER vs script = 0.400 > 0.35"})
	})
	t.Run("an unspoken line's WER is not gated", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.Lines[2].TAudioStartEventMS = 0 // never audible: nothing was misread
		rep.Lines[2].ASRWERvsScript = 0.40
		if fails := avGateFailures(rep, false); len(fails) != 0 {
			t.Fatalf("unspoken line: %d failure(s), want 0:\n%s", len(fails), strings.Join(fails, "\n"))
		}
	})
	t.Run("route mode fails on phantoms and the latency gate only", func(t *testing.T) {
		// The ADR-0162 play-loop route run: the baseline (no phantoms, the
		// latency in bounds) passes the route gate; a phantom run fails it.
		if fails := avGateFailures(avGateBaselineReport(), true); len(fails) != 0 {
			t.Fatalf("route baseline: %d failure(s), want 0:\n%s", len(fails), strings.Join(fails, "\n"))
		}
		rep := avGateBaselineReport()
		rep.PhantomUtterances = 1
		rep.PhantomReadThroughs = 1
		avGateWantsExactly(t, rep, 1, []string{"1 phantom utterance(s) (1 read-through)"})
	})
	t.Run("route mode ignores the conversation-shape rules", func(t *testing.T) {
		// The play loop iterates on audio phantoms and latency, not on the
		// catch-up conversation's shape: a route run with one committed line
		// and a thin idle set is not held to the multi-box floor.
		rep := avGateBaselineReport()
		rep.Lines = rep.Lines[:1]
		rep.IdleSamples = 2
		rep.MissedLines = 1
		if fails := avGateFailures(rep, true); len(fails) != 0 {
			t.Fatalf("route run with a thin conversation: %d failure(s), want 0 (phantoms + latency only):\n%s", len(fails), strings.Join(fails, "\n"))
		}
	})
	t.Run("several rules fail together", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.MissedLines = 1
		rep.PhantomUtterances = 1
		rep.Lines[0].VoiceMatch = false
		avGateWantsExactly(t, rep, 3, []string{"1 missed line(s)", "1 phantom utterance(s)", "line 0: spoken in voice"})
	})
	t.Run("a conversation-invariant gate issue fails the run", func(t *testing.T) {
		// ADR-0170: every ConvGateIssues finding fails the run, one failure
		// per issue; the report-only rows never gate.
		rep := avGateBaselineReport()
		rep.ConvDrive = "slow"
		rep.ConvGateIssues = []convIssue{
			{Kind: "DUPLICATE", Index: 0, MessageID: "msg-a#p1", Detail: "page msg-a#p1 has 2 own-audio utterances"},
			{Kind: "CUT-OFF", Index: 1, MessageID: "msg-b#p2", Detail: "the page's terminal is \"interrupted\""},
		}
		rep.ConvReportIssues = []convIssue{{Kind: "PAGE_BREAK_GAP", Index: 2, MessageID: "msg-c#p1", Detail: "the gap to the next utterance is 1800 ms"}}
		avGateWantsExactly(t, rep, 2, []string{"conversation invariant: DUPLICATE", "conversation invariant: CUT-OFF"})
	})
	t.Run("report-only conversation findings never gate", func(t *testing.T) {
		rep := avGateBaselineReport()
		rep.ConvDrive = "rapid"
		rep.ConvReportIssues = []convIssue{{Kind: "PAGE_BREAK_GAP", Index: 0, MessageID: "msg-a#p1", Detail: "the gap to the next utterance is 1800 ms"}}
		if fails := avGateFailures(rep, false); len(fails) != 0 {
			t.Fatalf("report-only findings: %d failure(s), want 0:\n%s", len(fails), strings.Join(fails, "\n"))
		}
	})
}

func TestAVHarnessBoxPosition(t *testing.T) {
	cases := []struct {
		centreY float64
		known   bool
		want    string
	}{
		{0.10, true, "top"},
		{0.33, true, "top"},         // just under a third
		{1.0 / 3.0, true, "middle"}, // the third itself is the middle band's edge
		{0.50, true, "middle"},
		{2.0 / 3.0, true, "middle"}, // two thirds is the bottom band's edge
		{0.66, true, "middle"},      // just under two thirds
		{0.67, true, "bottom"},      // just over two thirds
		{0.90, true, "bottom"},
		{0.50, false, ""}, // an unknown region is not a guess
	}
	for _, c := range cases {
		if got := avBoxPosition(c.centreY, c.known); got != c.want {
			t.Fatalf("avBoxPosition(%v, %v) = %q; want %q", c.centreY, c.known, got, c.want)
		}
	}
}

func TestAVHarnessReportWriter(t *testing.T) {
	rep := avGateBaselineReport()
	rep.MissedLines = 0
	rep.SettledToAudioP50MS = 200
	rep.SettledToAudioP95MS = 600
	rep.AppearToAudioP50MS = 300
	rep.AppearToAudioP95MS = 700
	rep.CommitToAudioP50MS = 250
	rep.CommitToAudioP95MS = 650
	rep.QueuedCommitToAudioP50MS = 900
	rep.QueuedCommitToAudioP95MS = 950
	rep.PhantomUtterances = 1
	rep.PhantomReadThroughs = 1
	rep.SkippedLines = 1
	rep.CacheHits = 2
	rep.PhantomRows = []avPhantomRow{{
		OccurredAt:   "2026-10-21T10:00:05.000Z",
		RouteSegment: "",
		BoxOnScreen:  false,
		VoiceID:      "av-voice-char",
		F0:           440,
		VoiceClass:   "mid (adult female)",
		PitchScored:  true,
		ReadThrough:  true,
	}}
	line := &rep.Lines[0]
	line.TBoxAppearMS, line.TTextSettledMS = 1000, 1500
	line.TCommitMS, line.TClefDoneMS = 1600, 1550
	line.TTSReadyMS, line.TAudioStartEventMS = 1700, 1800
	line.TAudioStartRMSMS = 1820
	line.SettledToAudioMS, line.AppearToAudioMS = 320, 820
	line.FromCache = true
	rep.Lines[1].TClefDoneMS = 0 // line 1: the Clef was skipped (the cell must be "-")

	path := filepath.Join(t.TempDir(), "report.md")
	avWriteReport(t, path, rep)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	s := string(body)
	// The summary numbers.
	for _, want := range []string{
		"- idle samples: 3",
		"- settled→audio p50/p95: 200/600 ms",
		"- phantom utterances: 1 (read-through: 1)",
		"- skipped lines: 1",
		"- cache hits: 2",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("report.md missing %q\n%s", want, s)
		}
	}
	// The phantom section (the top rows when the run carried phantoms) and
	// the per-line table: line 0's full row (every stage cell filled) and
	// line 1's skipped Clef cell.
	if !strings.Contains(s, "## Phantom utterances") || !strings.Contains(s, "## Per-line table") {
		t.Fatalf("report.md missing the phantom / per-line sections\n%s", s)
	}
	if !strings.Contains(s, "| occurred | segment | box on screen | voice | pitch | read-through |") {
		t.Fatalf("report.md missing the phantom table header\n%s", s)
	}
	if !strings.Contains(s, "| 2026-10-21T10:00:05.000Z | - | no | av-voice-char | 440 Hz mid (adult female) | yes |") {
		t.Fatalf("report.md missing the phantom row (the read-through's flags)\n%s", s)
	}
	row0 := "| 0 | - | middle | 1000 | 1500 | 1600 | 1550 | 1700 | 1800 | 1820 | 320 | 820 | yes | yes | av-voice-char | - | - | 0.050 | - | ended |"
	if !strings.Contains(s, row0) {
		t.Fatalf("report.md missing line 0's row:\n%s\n(want: %s)", s, row0)
	}
	if !strings.Contains(s, "| 1 | - | top | - | - | - | - | - | 2000 | - | - | - | no | yes | av-voice-char | - | - | 0.100 | - | ended |") {
		t.Fatalf("report.md missing line 1's row (the unobserved cells are \"-\")\n%s", s)
	}
	// Line 2: the batched + queued cell.
	if !strings.Contains(s, "batched@0+queued") {
		t.Fatalf("report.md missing line 2's batched/queued cell\n%s", s)
	}
}
