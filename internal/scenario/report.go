package scenario

// report.go renders the M6 HTML report: one index.html per scenario run under
// artifacts/scenarios/<run-id>/ (the gitignored evidence bundle), linking each
// scenario's frames, the merged event timeline (decisions, speech events),
// the assertion verdicts and — where the TTS layer produced audio — the
// synthesized WAV bytes.
//
// What the report deliberately does NOT carry (AGENTS.md rule 5): game text.
// The frames are linked, the decisions and speech events are correlated by
// message id / occurrence id and timestamp, but the narration text itself
// stays in the gitignored fixture only — an event payload's text field is
// stripped before it reaches the page (eventDetail below).

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/dolphin"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/session"
)

// ScenarioReportData is one scenario's report material: its verdict plus the
// evidence the page links (frames with their recorded screen labels, the
// merged timeline, the synthesized audio).
type ScenarioReportData struct {
	Scenario *Scenario
	Result   RunResult
	// Frames names the scenario's replayed slice (manifest rows, in order).
	Frames []dolphin.FixtureFrame
}

// ReportDoc is the whole run's report material.
type ReportDoc struct {
	RunID       string
	GeneratedAt time.Time
	// FixtureDir is the external fixture dir the frames were copied from.
	FixtureDir    string
	FixtureID     string
	ManifestSHA   string
	FrameCount    int
	GameImageSHA  string
	GameImageNote string
}

// ScenarioReport is one scenario's rendered section.
type ScenarioReport struct {
	Slug         string
	ID           string
	Status       string
	Summary      string
	Reason       string
	FaultKinds   string
	Assertions   []Result
	Stats        pipeline.Stats
	SessionStats session.Stats
	Frames       []FrameRow
	Events       []EventRow
	Audio        []AudioRow
	FramesRel    string
	AudioRel     string
}

// FrameRow is one linked frame: the file, its capture time and the recorded
// screen label (the drive's belief — the oracle the manifest carries).
type FrameRow struct {
	Seq    int
	File   string
	TsMS   int64
	Screen string
	Side   string
	SHA    string
}

// EventRow is one merged-timeline event, sanitized: the correlation fields
// (message id, occurrence id, outcome) and the capture timestamp — never the
// text payloads.
type EventRow struct {
	Seq    int64
	Wall   time.Time
	CapMS  int64 // the capture-clock timestamp the payload carries, when it does
	Name   string
	Detail string
}

// AudioRow is one linked utterance.
type AudioRow struct {
	Occurrence string
	File       string
	Bytes      int
}

// WriteHTMLReport writes the report tree under root and returns the index
// path. It copies each scenario's frame slice and WAV bytes into the bundle
// (the report is self-contained evidence: deleting the fixture later must not
// orphan the report's links).
func WriteHTMLReport(root string, doc ReportDoc, runs []ScenarioReportData) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	scens := make([]ScenarioReport, 0, len(runs))
	for _, rd := range runs {
		slug := reportSlug(rd.Scenario.ID)
		sec := ScenarioReport{
			Slug:         slug,
			ID:           rd.Scenario.ID,
			Status:       rd.Result.Status,
			Summary:      rd.Result.Summary(),
			Reason:       rd.Result.Reason,
			FaultKinds:   faultKinds(rd.Scenario.Faults),
			Assertions:   rd.Result.Report.Results,
			Stats:        rd.Result.Stats,
			SessionStats: rd.Result.SessionStats,
		}
		sec.FramesRel = filepath.ToSlash(filepath.Join(slug, "frames"))
		sec.AudioRel = filepath.ToSlash(filepath.Join(slug, "audio"))

		// The frames: copy the slice and record its rows.
		framesDir := filepath.Join(root, slug, "frames")
		if err := os.MkdirAll(framesDir, 0o755); err != nil {
			return "", err
		}
		for i, fr := range rd.Frames {
			src := filepath.Join(doc.FixtureDir, "frames", fr.File)
			if err := copyFileTo(src, filepath.Join(framesDir, fr.File)); err != nil {
				return "", fmt.Errorf("report: copy frame %s: %w", fr.File, err)
			}
			sec.Frames = append(sec.Frames, FrameRow{
				Seq: i + 1, File: fr.File, TsMS: fr.TsNS / int64(time.Millisecond),
				Screen: fr.Screen, Side: fr.Side, SHA: fr.SHA256,
			})
		}

		// The merged timeline, sanitized.
		for _, e := range rd.Result.Timeline {
			sec.Events = append(sec.Events, eventRow(e))
		}

		// The audio, per occurrence.
		if len(rd.Result.AudioByOccurrence) > 0 {
			audioDir := filepath.Join(root, slug, "audio")
			if err := os.MkdirAll(audioDir, 0o755); err != nil {
				return "", err
			}
			var occs []string
			for o := range rd.Result.AudioByOccurrence {
				occs = append(occs, o)
			}
			sort.Strings(occs)
			for _, o := range occs {
				wav := rd.Result.AudioByOccurrence[o]
				name := audioName(o)
				if err := os.WriteFile(filepath.Join(audioDir, name), wav, 0o644); err != nil {
					return "", err
				}
				sec.Audio = append(sec.Audio, AudioRow{Occurrence: o, File: name, Bytes: len(wav)})
			}
		}
		scens = append(scens, sec)
	}
	page, err := renderReportPage(doc, scens)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, "index.html")
	if err := os.WriteFile(path, page, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// eventRow reduces one envelope to the report row: the correlation fields
// and the capture timestamp, with every text payload stripped.
func eventRow(e events.Envelope) EventRow {
	row := EventRow{Seq: e.Seq, Wall: e.OccurredAt, Name: e.Name}
	var p map[string]any
	if err := json.Unmarshal(e.Data, &p); err != nil {
		row.Detail = "(unparsed payload)"
		return row
	}
	// The capture clock the payloads carry (the assertion's time base).
	if v, ok := p["capture_ts_ns"]; ok {
		row.CapMS = asInt64(v) / int64(time.Millisecond)
	}
	var parts []string
	add := func(key, label string) {
		if v, ok := p[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				parts = append(parts, label+"="+s)
			} else if n, ok := v.(float64); ok {
				parts = append(parts, fmt.Sprintf("%s=%g", label, n))
			} else if b, ok := v.(bool); ok {
				parts = append(parts, fmt.Sprintf("%s=%v", label, b))
			}
		}
	}
	add("message_id", "message")
	add("occurrence_id", "occurrence")
	add("generation", "gen")
	add("region_id", "region")
	add("accepted", "accepted")
	add("applied", "applied")
	add("reason", "reason")
	add("error", "error")
	add("kind", "kind")
	add("role", "role")
	add("degraded", "degraded")
	add("voice_id", "voice")
	add("provider", "provider")
	row.Detail = strings.Join(parts, " ")
	return row
}

func asInt64(v any) int64 {
	n, ok := v.(float64)
	if !ok {
		return 0
	}
	return int64(n)
}

func audioName(occurrence string) string {
	s := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, occurrence)
	if s == "" {
		s = "occurrence"
	}
	return s + ".wav"
}

func reportSlug(id string) string {
	return strings.ReplaceAll(strings.ReplaceAll(id, "/", "-"), "\\", "-")
}

func copyFileTo(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644)
}

// --- the page ----------------------------------------------------------------

// renderReportPage renders the script-free report. Every cell is HTML-escaped;
// the page links relative files only (frames, WAVs), so the bundle is
// portable within the artifacts/ tree.
func renderReportPage(doc ReportDoc, scens []ScenarioReport) ([]byte, error) {
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Game Narrator — dolphin scenario report ` + html.EscapeString(doc.RunID) + `</title>
<style>
body { font-family: system-ui, sans-serif; margin: 2rem auto; max-width: 1100px; color: #1a1a1a; }
h1 { font-size: 1.4rem; } h2 { font-size: 1.1rem; margin-top: 2rem; border-bottom: 2px solid #444; padding-bottom: .3rem; }
table { border-collapse: collapse; margin: .6rem 0; font-size: .85rem; }
th, td { border: 1px solid #bbb; padding: .25rem .5rem; text-align: left; vertical-align: top; }
th { background: #f0f0f0; }
.pass { color: #0a7a2f; font-weight: 600; } .fail { color: #b3001b; font-weight: 600; }
.blocked, .notrun { color: #8a6d00; font-weight: 600; }
pre { background: #f5f5f5; padding: .6rem; overflow-x: auto; font-size: .8rem; }
img.frame { max-width: 480px; max-height: 360px; border: 1px solid #999; }
a { color: #06453c; }
code { background: #f0f0f0; padding: 0 .2rem; }
.note { color: #555; font-size: .85rem; }
</style>
</head>
<body>
`)
	fmt.Fprintf(&b, `<h1>Dolphin scenario replay — run %s</h1>`, html.EscapeString(doc.RunID))
	fmt.Fprintf(&b, `<p class="note">Generated %s. Captured-frame replay (ADR-0037): the frames are real Dolphin screenshots recorded by the fixture recorder; the OCR answers are the real Tesseract readings of record time; the source is <code>dolphin-emulator</code> (real capture), never synthetic. The decision and TTS provider calls in the timelines are RECORDED inference replayed through the fake provider gateways (recorded profile oracle / recorded voice) — never live calls (AGENTS.md rule 3). The report links frames, decisions, speech events and audio — it never renders the narration text (AGENTS.md rule 5).</p>`, doc.GeneratedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(&b, `<p>Fixture <code>%s</code> — manifest sha256 <code>%s…</code>, %d frames; game image sha256 <code>%s</code>%s.</p>`,
		html.EscapeString(doc.FixtureID), html.EscapeString(doc.ManifestSHA[:min(12, len(doc.ManifestSHA))]),
		doc.FrameCount, html.EscapeString(doc.GameImageSHA), doc.GameImageNote)

	b.WriteString(`<h2>Scenarios</h2><table><tr><th>Scenario</th><th>Status</th><th>Summary</th></tr>`)
	for _, s := range scens {
		cls := "pass"
		switch s.Status {
		case "failed":
			cls = "fail"
		case "blocked":
			cls = "blocked"
		case "not_run":
			cls = "notrun"
		}
		fmt.Fprintf(&b, `<tr><td><a href="#%s">%s</a></td><td class="%s">%s</td><td>%s</td></tr>`,
			html.EscapeString(s.Slug), html.EscapeString(s.ID), cls, html.EscapeString(strings.ToUpper(s.Status)), html.EscapeString(s.Summary))
	}
	b.WriteString(`</table>`)

	for _, s := range scens {
		cls := "pass"
		switch s.Status {
		case "failed":
			cls = "fail"
		case "blocked":
			cls = "blocked"
		case "not_run":
			cls = "notrun"
		}
		fmt.Fprintf(&b, `<h2 id="%s">%s <span class="%s">[%s]</span></h2>`,
			html.EscapeString(s.Slug), html.EscapeString(s.ID), cls, html.EscapeString(strings.ToUpper(s.Status)))
		if s.Reason != "" {
			fmt.Fprintf(&b, `<p class="note">%s</p>`, html.EscapeString(s.Reason))
		}
		if s.FaultKinds != "none" {
			fmt.Fprintf(&b, `<p>Faults injected on replay: <code>%s</code></p>`, html.EscapeString(s.FaultKinds))
		}
		if len(s.Assertions) > 0 {
			b.WriteString(`<table><tr><th>Assertion</th><th>Verdict</th><th>Detail</th></tr>`)
			for _, a := range s.Assertions {
				v := "pass"
				if !a.Passed {
					v = "fail"
				}
				fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td class="%s">%s</td><td>%s</td></tr>`,
					html.EscapeString(a.Type), v, strings.ToUpper(v), html.EscapeString(a.Detail))
			}
			b.WriteString(`</table>`)
		}

		if len(s.Frames) > 0 {
			fmt.Fprintf(&b, `<h3>Frames (%d, slice of the fixture recording)</h3><table><tr><th>#</th><th>Frame</th><th>Capture (ms)</th><th>Recorded screen class</th><th>SHA-256 (first 16)</th><th></th></tr>`, len(s.Frames))
			for _, fr := range s.Frames {
				fmt.Fprintf(&b, `<tr><td>%d</td><td><code>%s</code></td><td>%d</td><td>%s%s</td><td><code>%s…</code></td><td><a href="%s/%s"><img class="frame" src="%s/%s" alt="%s"></a></td></tr>`,
					fr.Seq, html.EscapeString(fr.File), fr.TsMS,
					html.EscapeString(fr.Screen), cursorSideNote(fr.Side),
					html.EscapeString(fr.SHA[:min(16, len(fr.SHA))]),
					s.FramesRel, html.EscapeString(fr.File), s.FramesRel, html.EscapeString(fr.File), html.EscapeString(fr.File))
			}
			b.WriteString(`</table>`)
		}

		if len(s.Events) > 0 {
			fmt.Fprintf(&b, `<h3>Merged timeline (%d events; text payloads stripped)</h3><table><tr><th>Seq</th><th>Wall time</th><th>Capture (ms)</th><th>Event</th><th>Detail</th></tr>`, len(s.Events))
			for _, ev := range s.Events {
				cap := "—"
				if ev.CapMS != 0 {
					cap = fmt.Sprintf("%d", ev.CapMS)
				}
				fmt.Fprintf(&b, `<tr><td>%d</td><td>%s</td><td>%s</td><td><code>%s</code></td><td>%s</td></tr>`,
					ev.Seq, ev.Wall.UTC().Format("15:04:05.000"), cap,
					html.EscapeString(ev.Name), html.EscapeString(ev.Detail))
			}
			b.WriteString(`</table>`)
		}

		if len(s.Audio) > 0 {
			b.WriteString(`<h3>Audio (synthesized by the TTS layer)</h3><table><tr><th>Occurrence</th><th>File</th><th>Bytes</th><th></th></tr>`)
			for _, au := range s.Audio {
				fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td><code>%s</code></td><td>%d</td><td><a href="%s/%s">play</a></td></tr>`,
					html.EscapeString(au.Occurrence), html.EscapeString(au.File), au.Bytes,
					s.AudioRel, html.EscapeString(au.File))
			}
			b.WriteString(`</table>`)
		}

		statsJSON, _ := json.Marshal(struct {
			Pipeline pipeline.Stats `json:"pipeline"`
			Session  session.Stats  `json:"session"`
		}{s.Stats, s.SessionStats})
		fmt.Fprintf(&b, `<details><summary>Self-report (pipeline + session stats)</summary><pre>%s</pre></details>`, html.EscapeString(string(statsJSON)))
	}
	b.WriteString(`</body></html>`)
	return []byte(b.String()), nil
}

// cursorSideNote renders the prompt cursor side the drive measured ("(cursor: yes)").
func cursorSideNote(side string) string {
	if side == "" {
		return ""
	}
	return " (cursor: " + side + ")"
}
