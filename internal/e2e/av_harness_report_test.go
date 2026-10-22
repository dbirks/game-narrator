// Package e2e — the A/V harness report (ADR-0143, Amendment 1; ADR-0162 adds
// the phantom section and the per-line attribution columns): the box
// position classification from the profile's placement-aware region rows
// (ADR-0137) and the run's report.md — the human-readable half of the
// timeline.json pair, written next to it. Log-safe per AGENTS.md rule 5:
// ids, counts and timings only — never dialogue text.
package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// avBoxPosition classifies one line's box position from its region's centre
// Y over the frame height (the ADR-0137 placement rows: the box renders in
// the top, the middle or the bottom of the picture depending on the
// scene): under a third is "top", over two thirds is "bottom", the band
// between is "middle". An unknown region (absent from the profile's rows)
// is "" — the honest "not measured", not a guess.
func avBoxPosition(centreY float64, known bool) string {
	if !known {
		return ""
	}
	switch {
	case centreY < 1.0/3.0:
		return "top"
	case centreY > 2.0/3.0:
		return "bottom"
	default:
		return "middle"
	}
}

// avReportMS renders one timeline instant as a table cell: the ms offset
// from T0, or "-" where the instant was never observed (the timeline's 0
// convention is "missing", never "T0").
func avReportMS(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", ms)
}

// avReportWER renders one line's ASR WER vs script as a table cell: the
// score, or "-" where the line was never scored (an unconfigured ASR role
// or a line without ground truth) — a missing score must not read as a
// perfect zero.
func avReportWER(scored bool, wer float64) string {
	if !scored {
		return "-"
	}
	return fmt.Sprintf("%.3f", wer)
}

// avReportQueue renders the line's batched/queued cell: the catch-up run's
// position when the line was spoken inside a merged run, the queueing
// flag when it committed behind another line's audio, both, or "-".
func avReportQueue(l *avLineRecord) string {
	switch {
	case l.Batched && l.QueuedBehindSpeech:
		return fmt.Sprintf("batched@%d+queued", l.RunPosition)
	case l.Batched:
		return fmt.Sprintf("batched@%d", l.RunPosition)
	case l.QueuedBehindSpeech:
		return "queued"
	default:
		return "-"
	}
}

// avReportCell renders one report cell: the route segment / voice / character
// identifier, or "-" when the run carried none (the timeline's 0 convention is
// "missing", never "T0").
func avReportCell(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// avReportBool renders one report cell: the ADR-0162 flag as yes/no, "-" when
// the run did not observe it.
func avReportBool(b bool) string {
	if !b {
		return "no"
	}
	return "yes"
}

// avWriteReport writes the run's report.md next to the timeline.json
// (ADR-0143, Amendment 1; ADR-0162 adds the phantom section and the
// per-line attribution columns): the summary numbers the gate reads, then
// the PHANTOM rows (when any — at the top of the report, before the
// per-line table, because a phantom is the run's loudest defect), then the
// per-line table — one row per line in stream order, one column per stage,
// so a failed gate names a line the reader can find in the table (AGENTS.md
// evidence bundle: the report is the timeline's readable half). Log-safe per
// AGENTS.md rule 5: ids, counts and timings only — never dialogue text.
func avWriteReport(t *testing.T, path string, rep avTimelineReport) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# A/V harness report (ADR-0143)\n\n")
	fmt.Fprintf(&b, "Run `%s`, session `%s`.\n\n", rep.RunID, rep.SessionID)
	b.WriteString("## Summary\n\n")
	fmt.Fprintf(&b, "- lines: %d\n", len(rep.Lines))
	fmt.Fprintf(&b, "- idle samples: %d\n", rep.IdleSamples)
	fmt.Fprintf(&b, "- settled→audio p50/p95: %d/%d ms\n", rep.SettledToAudioP50MS, rep.SettledToAudioP95MS)
	fmt.Fprintf(&b, "- appear→audio p50/p95: %d/%d ms\n", rep.AppearToAudioP50MS, rep.AppearToAudioP95MS)
	fmt.Fprintf(&b, "- commit→audio p50/p95: %d/%d ms\n", rep.CommitToAudioP50MS, rep.CommitToAudioP95MS)
	fmt.Fprintf(&b, "- queued commit→audio p50/p95: %d/%d ms\n", rep.QueuedCommitToAudioP50MS, rep.QueuedCommitToAudioP95MS)
	fmt.Fprintf(&b, "- missed lines: %d\n", rep.MissedLines)
	fmt.Fprintf(&b, "- duplicate commits: %d\n", rep.DuplicateCommits)
	fmt.Fprintf(&b, "- stale starts: %d\n", rep.StaleStarts)
	fmt.Fprintf(&b, "- phantom utterances: %d (read-through: %d)\n", rep.PhantomUtterances, rep.PhantomReadThroughs)
	fmt.Fprintf(&b, "- skipped lines: %d\n", rep.SkippedLines)
	fmt.Fprintf(&b, "- cache hits: %d\n\n", rep.CacheHits)
	b.WriteString("## Phantom utterances\n\n")
	if rep.PhantomUtterances > 0 {
		b.WriteString("Phantoms (ADR-0162): utterances whose audio had no dialogue box on screen at the audio start, or that were spoken inside a route segment marked `walk*` — the top rows of this report, because a phantom is the run's loudest defect. A route run fails on phantoms (and on the latency gate) behind the same opt-out envs the harness already has.\n\n")
		b.WriteString("| occurred | segment | box on screen | voice | pitch | read-through |\n")
		b.WriteString("|----------|---------|--------------|-------|-------|-------------|\n")
		for _, r := range rep.PhantomRows {
			pitch := "-"
			if r.PitchScored {
				pitch = fmt.Sprintf("%.0f Hz %s", r.F0, r.VoiceClass)
			}
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s |\n",
				r.OccurredAt, avReportCell(r.RouteSegment), avReportBool(r.BoxOnScreen), r.VoiceID, pitch, avReportBool(r.ReadThrough)))
		}
	} else {
		b.WriteString("No phantom utterances.\n\n")
	}
	b.WriteString("## Per-line table\n\n")
	b.WriteString("Instants are ms from the shared-clock start (T0); `-` is an instant the run never observed.\n\n")
	b.WriteString("| # | seg | box | appear | settled | commit | clef | tts_ready | audio_event | audio_rms | settled→audio | appear→audio | cache | voice ok | voice | char | pitch | WER vs script | batched/queued | end |\n")
	b.WriteString("|---|-----|-----|--------|---------|--------|------|-----------|-------------|-----------|---------------|--------------|-------|----------|------|-----|--------|------------|-----------------|-----|\n")
	for i := range rep.Lines {
		l := &rep.Lines[i]
		voice := "-"
		if l.VoiceID != "" {
			voice = "no"
			if l.VoiceMatch {
				voice = "yes"
			}
		}
		cache := "no"
		if l.FromCache {
			cache = "yes"
		}
		end := l.EndResult
		if end == "" {
			end = "-"
		}
		box := l.BoxPosition
		if box == "" {
			box = "-"
		}
		pitch := "-"
		if l.PitchScored {
			pitch = fmt.Sprintf("%.0f Hz %s", l.F0, l.VoiceClass)
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			l.Index, avReportCell(l.RouteSegment), box,
			avReportMS(l.TBoxAppearMS), avReportMS(l.TTextSettledMS), avReportMS(l.TCommitMS),
			avReportMS(l.TClefDoneMS), avReportMS(l.TTSReadyMS),
			avReportMS(l.TAudioStartEventMS), avReportMS(l.TAudioStartRMSMS),
			avReportMS(l.SettledToAudioMS), avReportMS(l.AppearToAudioMS),
			cache, voice, avReportCell(l.VoiceID), avReportCell(l.Character), pitch,
			avReportWER(l.asrScored, l.ASRWERvsScript),
			avReportQueue(l), end)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("av report: write %s: %v", path, err)
	}
	t.Logf("av e2e: report.md written (%d lines, %d phantom utterances)", len(rep.Lines), rep.PhantomUtterances)
}
