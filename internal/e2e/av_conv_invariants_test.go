// Package e2e — the conversation invariants (ADR-0170): pure functions over
// the A/V harness's timeline (avLineRecord, ADR 0143) that check the
// owner's acceptance bar for a spoken conversation — each page spoken
// exactly once, no read-ahead past the page on screen, pages spoken in
// the order shown, queue-don't-cut on the slow-reader drive, and the
// page-break sentence gap reported. Test-only by the harness's own
// convention (the A/V harness is all _test.go); the live harness calls
// them from the shared-clock timeline. Log-safe per AGENTS.md rule 5:
// the issues name ids, ordinals and timings — never dialogue text (a
// flagging word is a single token from the utterance, allowed by the
// id/word allowance; the page texts themselves are never printed).
package e2e

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games/textnorm"
)

// The invariant bounds (ADR-0170).
const (
	// convDuplicateWindow is the ASR-similarity half of the duplicate rule:
	// two utterances of the same page are also two transcripts >= the
	// similarity floor within this window (same message+page, or the text
	// itself, when the ids differ).
	convDuplicateWindow = 20 * time.Second
	// convDuplicateSim is the transcript similarity floor (word-multiset
	// Dice).
	convDuplicateSim = 0.8
	// convQueueStartGap is the queue-don't-cut bound: the next page's audio
	// starts within this of the current page's end.
	convQueueStartGap = 800 * time.Millisecond
	// convQueueFloor is the queue-don't-cut duration bound: the page's
	// acoustic duration is at least this fraction of its synthesized one.
	convQueueFloor = 0.85
	// convPageBreakGap is the page-break flag: the gap between the two
	// utterances of a sentence that crosses the page break, above which the
	// report names it PAGE_BREAK_GAP (report-only, ADR-0170 rule 5).
	convPageBreakGap = 1500 * time.Millisecond
)

// convIssue is one conversation-invariant finding: the kind, the first line
// involved (the report's per-line table index, -1 when the issue is not
// line-bound), the message it names and the human detail (ids, ordinals,
// timings — never page text).
type convIssue struct {
	Kind      string
	Index     int
	MessageID string
	Detail    string
}

func (i convIssue) String() string {
	if i.Index >= 0 {
		return fmt.Sprintf("%s: line %d (%s) — %s", i.Kind, i.Index, i.MessageID, i.Detail)
	}
	return fmt.Sprintf("%s: (%s) — %s", i.Kind, i.MessageID, i.Detail)
}

// convSpoken reports whether the line was spoken: its own utterance's audio
// started (not batched), or a catch-up merged run covers it (the merged
// utterance counts as covering every page it merges, ADR-0170 rule 1).
func convSpoken(l *avLineRecord) bool {
	if l.Batched && l.RunOccurrenceID != "" {
		return true
	}
	return l.TAudioStartEventMS > 0 && !l.Batched
}

// convOwnAudio reports whether the line spoke its OWN audio (not inside a
// merged run): the duplicate rule counts own-audio lines per page.
func convOwnAudio(l *avLineRecord) bool {
	return l.TAudioStartEventMS > 0 && !l.Batched
}

// convPageOrdinal is the ordinal (1-based) of the display page the line
// shows: the line's position among its message's committed occurrences in
// stream order — each page turn mints a new occurrence under the same
// message id (the session's in-place page turn, the timeline's trace join).
func convPageOrdinal(lines []avLineRecord, i int) int {
	ord := 1
	for j := 0; j < i; j++ {
		if lines[j].MessageID == lines[i].MessageID {
			ord++
		}
	}
	return ord
}

// convWordTokens tokenizes one text into its lowercased word tokens: the
// player-name placeholder is dropped (the spoken form may carry the learned
// name or nothing, while the page text carries the placeholder), and the
// sentence-boundary punctuation is trimmed off each token.
func convWordTokens(s string) []string {
	s = strings.ReplaceAll(s, textnorm.Placeholder, " ")
	s = strings.ToLower(s)
	var out []string
	for _, f := range strings.Fields(s) {
		t := convTrimPunct(f)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// convTrimPunct trims the sentence-boundary punctuation off both ends of one
// token (rune-wise — the curly quotes and ellipsis are not byte-set members).
func convTrimPunct(s string) string {
	cut := func(r rune) bool {
		switch r {
		case '!', '?', ',', '.', ';', ':', '"', '\'', '(', ')', '[', ']', '}', '{',
			'“', '”', '‘', '’', '…':
			return true
		}
		return false
	}
	r := []rune(s)
	for len(r) > 0 && cut(r[0]) {
		r = r[1:]
	}
	for len(r) > 0 && cut(r[len(r)-1]) {
		r = r[:len(r)-1]
	}
	return string(r)
}

// convMultiset counts a token slice's words.
func convMultiset(tokens []string) map[string]int {
	m := make(map[string]int, len(tokens))
	for _, t := range tokens {
		m[t]++
	}
	return m
}

// convDice is the word-multiset Dice similarity of two texts
// (2·|intersection| / (|a| + |b|), 0 when either is empty).
func convDice(a, b string) float64 {
	ta, tb := convWordTokens(a), convWordTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	ma, mb := convMultiset(ta), convMultiset(tb)
	inter := 0
	for w, n := range ma {
		if m := mb[w]; m > 0 {
			if m < n {
				inter += m
			} else {
				inter += n
			}
		}
	}
	return 2 * float64(inter) / float64(len(ta)+len(tb))
}

// convCoveredOrds returns the page ordinals the line's utterance covers:
// the line's own page, or — for a line the catch-up policy batched — every
// page of its run (the merged utterance covers each page it merges).
func convCoveredOrds(lines []avLineRecord, i int) (base string, ords []int) {
	base = lines[i].MessageID
	if !lines[i].Batched || lines[i].RunOccurrenceID == "" {
		return base, []int{convPageOrdinal(lines, i)}
	}
	var runOrds []int
	for j := range lines {
		if lines[j].Batched && lines[j].RunOccurrenceID == lines[i].RunOccurrenceID {
			runOrds = append(runOrds, convPageOrdinal(lines, j))
		}
	}
	return base, runOrds
}

// convTerminalPunct reports whether s ends in sentence-terminal punctuation
// (the page-break rule's predicate: the sentence continues on the next page
// when the page does NOT end terminally).
func convTerminalPunct(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	rs := []rune(s)
	return convTerminal(rs[len(rs)-1]) // the last rune (an ellipsis is one rune, not three bytes)
}

func convTerminal(r rune) bool {
	switch r {
	case '.', '!', '?', ';', '…', '"', '”', ')', ']', '}':
		return true
	}
	return false
}

// convCheckDuplicates is the spoken-exactly-once rule (ADR-0170 rule 1):
// each page the player saw has exactly one utterance covering it. A page
// covered by more than one utterance — the same message+page spoken twice,
// or two transcripts >= convDuplicateSim similar within convDuplicateWindow
// — is a DUPLICATE. A catch-up merged utterance covers every page it merges
// (one merged cover per page is one utterance, not a duplicate); a page
// neither covered nor skipped is the missed-line rule's ground, not this
// rule's.
func convCheckDuplicates(lines []avLineRecord, pages map[string][]string) []convIssue {
	var issues []convIssue
	// own-audio lines per page key
	own := map[string][]int{}
	for i := range lines {
		if convOwnAudio(&lines[i]) {
			key := lines[i].MessageID + "#p" + fmt.Sprint(convPageOrdinal(lines, i))
			own[key] = append(own[key], i)
		}
	}
	for key, idx := range own {
		if len(idx) < 2 {
			continue
		}
		// One cover per page: more than one own-audio line on the page is
		// the page spoken twice.
		seen := map[int]bool{}
		for _, i := range idx {
			if seen[i] {
				continue
			}
			seen[i] = true
			issues = append(issues, convIssue{
				Kind: "DUPLICATE", Index: idx[0], MessageID: key,
				Detail: fmt.Sprintf("page %s has %d own-audio utterances; want exactly one (spoken exactly once)", key, len(idx)),
			})
			break
		}
	}
	// A page covered by more than one UTTERANCE — a merged run's cover
	// competing with an own-audio cover, or two runs covering the same
	// page — is covered twice. The merged run's lines all share the run's
	// occurrence, so the cover identity is the run's occurrence for
	// batched lines and the line itself otherwise.
	covers := map[string]map[string]int{} // page key → utterance id → first line
	for i := range lines {
		l := &lines[i]
		if !convSpoken(l) {
			continue
		}
		utter := fmt.Sprintf("line-%d", i)
		if l.Batched && l.RunOccurrenceID != "" {
			utter = "run:" + l.RunOccurrenceID
		}
		base, ords := convCoveredOrds(lines, i)
		for _, o := range ords {
			key := base + "#p" + fmt.Sprint(o)
			if covers[key] == nil {
				covers[key] = map[string]int{}
			}
			if _, seen := covers[key][utter]; !seen {
				covers[key][utter] = i
			}
		}
	}
	for key, utt := range covers {
		if len(utt) < 2 {
			continue
		}
		// Two OWN covers of the same page key the own-audio rule already
		// named; this map reports the cross-utterance half (a run competing
		// with an own cover, or two runs on one page).
		runs := 0
		for id := range utt {
			if strings.HasPrefix(id, "run:") {
				runs++
			}
		}
		if runs == 0 {
			continue
		}
		first := -1
		for _, i := range utt {
			if first == -1 || i < first {
				first = i
			}
		}
		issues = append(issues, convIssue{
			Kind: "DUPLICATE", Index: first, MessageID: key,
			Detail: fmt.Sprintf("page %s is covered by %d distinct utterances (a merged run and/or another); want exactly one", key, len(utt)),
		})
	}
	// The transcript half: two distinct utterances whose transcripts are
	// >= convDistinctSim similar and whose audio started within
	// convDuplicateWindow of each other. Different messages: the page was
	// spoken twice under different ids. Same message, different ordinals:
	// the box the player had was the same one (the profile's page texts at
	// both ordinals are identical) — a re-spoken page.
	type spokenRow struct {
		idx     int
		startMS int64
		text    string
		msg     string
		ord     int
		pageKey string
	}
	var spoken []spokenRow
	for i := range lines {
		l := &lines[i]
		if !convSpoken(l) || l.ASRTranscript == "" {
			continue
		}
		spoken = append(spoken, spokenRow{idx: i, startMS: l.TAudioStartEventMS, text: l.ASRTranscript,
			msg: l.MessageID, ord: convPageOrdinal(lines, i),
			pageKey: l.MessageID + "#p" + fmt.Sprint(convPageOrdinal(lines, i))})
	}
	pageTextAt := func(msg string, ord int) (string, bool) {
		p, ok := pages[msg]
		if !ok || ord-1 < 0 || ord-1 >= len(p) {
			return "", false
		}
		return p[ord-1], true
	}
	for a := 0; a < len(spoken); a++ {
		for b := a + 1; b < len(spoken); b++ {
			x, y := spoken[a], spoken[b]
			if x.pageKey == y.pageKey {
				continue // the page-key and cover halves already named these
			}
			gap := x.startMS - y.startMS
			if gap < 0 {
				gap = -gap
			}
			if gap > int64(convDuplicateWindow.Milliseconds()) {
				continue
			}
			dup := false
			if x.msg == y.msg {
				// Same message, different ordinal: a duplicate only when the
				// box the player saw was the same one (identical page texts
				// at both ordinals) and the transcripts agree.
				tx, okx := pageTextAt(x.msg, x.ord)
				ty, oky := pageTextAt(y.msg, y.ord)
				if okx && oky && tx == ty {
					dup = convDice(x.text, y.text) >= convDuplicateSim
				} else if okx && oky {
					// Distinct page texts: similar transcripts mean the wiring
					// spoke the SAME text for two DIFFERENT boxes — the page
					// the player had on screen was not spoken (a re-spoken
					// page, the owner's "nothing is spoken twice").
					dup = convDice(x.text, y.text) >= convDuplicateSim
				} else {
					// No page rows: fall back to the transcripts alone.
					dup = convDice(x.text, y.text) >= convDuplicateSim
				}
			} else {
				dup = convDice(x.text, y.text) >= convDuplicateSim
			}
			if dup {
				issues = append(issues, convIssue{
					Kind: "DUPLICATE", Index: x.idx,
					MessageID: x.pageKey,
					Detail: fmt.Sprintf("line %d's transcript is >= %.1f similar to line %d's, audio starts %d ms apart (< %s) — the same words spoken twice",
						x.idx, convDuplicateSim, y.idx, gap, convDuplicateWindow),
				})
			}
		}
	}
	return issues
}

// convCheckReadAhead is the no-read-ahead rule (ADR-0170 rule 2): an
// utterance's words must stay inside the page(s) it covers — the box text
// visible at the audio start. The utterance side is both the ASR
// transcript (what was actually audible) and the requested text the
// wiring asked the voice to speak; the box side is the session's
// committed pages (messageID → the box's OCR reading per page, in
// ordinal order — the committed page text, not the profile's canonical
// form). A word of a page the player had NOT reached (a later ordinal)
// is a READ-AHEAD; a word the requested text carries that no covered
// page carries is the subset half of the same rule (the utterance text
// must be within the page text), excused only where the page's OCR
// reading and the requested text still agree (Dice >= the duplicate
// floor: the misread word, the player-name substitution, the folded
// display wrap). Lines whose page has no committed row are skipped (no
// ground truth — the ASR WER gate still judges their fidelity).
func convCheckReadAhead(lines []avLineRecord, pages map[string][]string) []convIssue {
	var issues []convIssue
	for i := range lines {
		l := &lines[i]
		if !convSpoken(l) {
			continue
		}
		base, ords := convCoveredOrds(lines, i)
		pageRows, known := pages[base]
		if !known || len(pageRows) == 0 {
			continue // no page rows: no box-text ground truth
		}
		// The allowed words: the covered pages' word multisets summed.
		allowed := map[string]int{}
		coveredMax := 0
		missing := false
		for _, o := range ords {
			if o-1 < len(pageRows) && pageRows[o-1] != "" {
				for w, n := range convMultiset(convWordTokens(pageRows[o-1])) {
					allowed[w] += n
				}
				if o > coveredMax {
					coveredMax = o
				}
			} else {
				missing = true
			}
		}
		if missing {
			continue // one of the covered pages has no row: no ground truth
		}
		// The later pages' words: the player had not reached them at the
		// audio start.
		later := map[string]bool{}
		for o := coveredMax + 1; o <= len(pageRows); o++ {
			for w := range convMultiset(convWordTokens(pageRows[o-1])) {
				later[w] = true
			}
		}
		// The utterance words, per side: the words that EXCEED the allowed
		// count are the read-ahead candidates. Words of a page the player
		// had not reached (a later ordinal) gate as READ-AHEAD. The rest
		// (outside every covered page, not a later-page word) split by side:
		// a deterministic requested-side subset miss is READ-AHEAD too — the
		// covered page's ground truth is the box's OCR reading, so the miss
		// is excused when the texts still agree (Dice >= the duplicate
		// floor: a misread word, the player-name substitution, the folded
		// display wrap); an acoustic transcript's stray words are ASR noise
		// the WER gate judges — READ-AHEAD-NOISE, report-only.
		nameAllow := 1 // the one player-name word the substitution admits
		type side struct {
			name string
			text string
		}
		coveredJoined := strings.Join(
			func() []string {
				var rows []string
				for _, o := range ords {
					if o-1 < len(pageRows) {
						rows = append(rows, pageRows[o-1])
					}
				}
				return rows
			}(), " ")
		for _, s := range []side{{"transcript", l.ASRTranscript}, {"requested", l.RequestedText}} {
			if s.text == "" {
				continue
			}
			m := convMultiset(convWordTokens(s.text))
			out := map[string]int{}
			for w, n := range m {
				if n > allowed[w] {
					out[w] = n - allowed[w]
				}
			}
			if len(out) == 0 {
				continue
			}
			// The later-page words: the read-ahead itself.
			for w := range out {
				if later[w] {
					issues = append(issues, convIssue{
						Kind: "READ-AHEAD", Index: i, MessageID: base,
						Detail: fmt.Sprintf("the %s carries %q, a word of a page this utterance had not reached (page ordinal > %d)", s.name, w, coveredMax),
					})
				}
			}
			// The remainder: outside every covered page.
			rest := map[string]bool{}
			for w := range out {
				if !later[w] {
					rest[w] = true
				}
			}
			if len(rest) <= nameAllow {
				continue // the player-name substitution (one word, both sides)
			}
			if s.name == "requested" {
				// The OCR noise floor: the box reading the player saw is the
				// OCR's, not the canonical text's — a misread word shrinks the
				// allowed set. The utterance is inside the page when the texts
				// still agree despite the excess words.
				if convDice(s.text, coveredJoined) >= convDuplicateSim {
					continue
				}
				issues = append(issues, convIssue{
					Kind: "READ-AHEAD", Index: i, MessageID: base,
					Detail: fmt.Sprintf("the %s carries %d word(s) outside the covered page's box text (utterance ⊄ page text)", s.name, len(rest)),
				})
			} else {
				issues = append(issues, convIssue{
					Kind: "READ-AHEAD-NOISE", Index: i, MessageID: base,
					Detail: fmt.Sprintf("the %s carries %d word(s) outside the covered page's box text (ASR noise; the WER gate judges fidelity)", s.name, len(rest)),
				})
			}
		}
	}
	return issues
}

// convCheckOrder is the order rule (ADR-0170 rule 3): pages are spoken in
// the order shown. The spoken lines' own-audio starts must follow the
// commit order (the stream order) — a line whose audio started before an
// older line's is OUT-OF-ORDER. Lines covered by the same merged run share
// the run's stamps and are ordered inside it by position, so they never
// invert; a run that started after the line its commit preceded is the
// inversion the rule names.
func convCheckOrder(lines []avLineRecord) []convIssue {
	var issues []convIssue
	type startRow struct {
		idx int
		ms  int64
	}
	var starts []startRow
	for i := range lines {
		l := &lines[i]
		if !convSpoken(l) {
			continue
		}
		starts = append(starts, startRow{idx: i, ms: l.TAudioStartEventMS})
	}
	for a := 0; a < len(starts); a++ {
		for b := a + 1; b < len(starts); b++ {
			if starts[b].ms < starts[a].ms {
				issues = append(issues, convIssue{
					Kind: "OUT-OF-ORDER", Index: starts[a].idx,
					MessageID: lines[starts[a].idx].MessageID,
					Detail: fmt.Sprintf("line %d's audio started %d ms BEFORE line %d's (commit order %d < %d, audio order inverted)",
						starts[a].idx, starts[a].ms-starts[b].ms, starts[b].idx, starts[a].idx, starts[b].idx),
				})
				break
			}
		}
	}
	return issues
}

// convCheckQueue is the queue-don't-cut rule (ADR-0170 rule 4): the page the
// player is reading is not cut off when the player advances — its terminal
// is ended (not interrupted/skipped), its acoustic duration is at least
// convQueueFloor of its synthesized one, and the next page's audio starts
// within convQueueStartGap of this page's end. It applies to the
// slow-reader drive (the rapid drive's fence cut is by design, ADR-0122),
// so the harness passes it only for GN_AV_DRIVE=slow runs. A line whose
// synthesized duration is unmeasured (no file-sink WAV) keeps the terminal
// check only — the duration half is unmeasured, not failed.
func convCheckQueue(lines []avLineRecord) (gateIssues, reportIssues []convIssue) {
	for i := range lines {
		l := &lines[i]
		if !convSpoken(l) {
			continue
		}
		// The terminal half: the page that played to its terminal must
		// have ended, not been cut.
		if l.TAudioStartEventMS > 0 && l.EndResult != "" && l.EndResult != "ended" {
			gateIssues = append(gateIssues, convIssue{
				Kind: "CUT-OFF", Index: i, MessageID: l.MessageID,
				Detail: fmt.Sprintf("the page's terminal is %q, not %q — the audio was cut (queue, don't cut)", l.EndResult, "ended"),
			})
		}
		// The duration half: the acoustic span over the synthesized one.
		if l.TAudioEndMS > 0 && l.TAudioStartEventMS > 0 && l.SynthDurMS > 0 {
			acoustic := l.TAudioEndMS - l.TAudioStartEventMS
			floor := float64(l.SynthDurMS) * convQueueFloor
			if float64(acoustic) < floor {
				gateIssues = append(gateIssues, convIssue{
					Kind: "CUT-OFF", Index: i, MessageID: l.MessageID,
					Detail: fmt.Sprintf("acoustic duration %d ms is under %.0f%% of the synthesized %d ms (floor %d ms) — the audio was cut",
						acoustic, convQueueFloor*100, l.SynthDurMS, int64(math.Ceil(floor))),
				})
			}
		}
		// The start-gap half (the queue handoff): it applies when the
		// player advanced WHILE this page was still playing — the next
		// page committed inside this page's acoustic window, or started
		// before this page ended. Then the runtime must hand the queued
		// page over within convQueueStartGap of this page's end. When the
		// player waited for the page to finish before advancing (the
		// slow-reader drive's normal case), the next page's start is
		// gated by the player's own press + the box recognition, not by
		// the runtime's queue — the gap is reported, not gated.
		for j := i + 1; j < len(lines); j++ {
			n := &lines[j]
			if !convSpoken(n) {
				continue
			}
			if l.TAudioEndMS <= 0 || n.TAudioStartEventMS <= 0 {
				break // no stamps to measure the gap
			}
			queued := n.QueuedBehindSpeech ||
				(l.TAudioStartEventMS > 0 && n.TCommitMS > 0 && n.TCommitMS >= l.TAudioStartEventMS && n.TCommitMS < l.TAudioEndMS) ||
				n.TAudioStartEventMS < l.TAudioEndMS
			gap := n.TAudioStartEventMS - l.TAudioEndMS
			if queued {
				if gap > int64(convQueueStartGap.Milliseconds()) {
					gateIssues = append(gateIssues, convIssue{
						Kind: "CUT-OFF", Index: i, MessageID: l.MessageID,
						Detail: fmt.Sprintf("the next page (line %d) was queued mid-playback but started %d ms after this page ended (bound %d ms) — the queue stalled",
							j, gap, int64(convQueueStartGap.Milliseconds())),
					})
				}
			} else {
				reportIssues = append(reportIssues, convIssue{
					Kind: "QUEUE-GAP", Index: i, MessageID: l.MessageID,
					Detail: fmt.Sprintf("the next page (line %d) started %d ms after this page ended (the player advanced after the end: the gap is the player's press + the box recognition, not the runtime's queue)", j, gap),
				})
			}
			break
		}
	}
	return gateIssues, reportIssues
}

// convPageBreakGaps reports the page-break sentence rule (ADR-0170 rule 5,
// report-only): for a spoken page whose text ends WITHOUT terminal
// punctuation — the sentence continues on the next page — the gap between
// the two utterances, above which the issue is flagged PAGE_BREAK_GAP. The
// page texts are the profile's rows (messageID → page texts in ordinal
// order); a page without rows keeps no flag (no ground truth).
func convPageBreakGaps(lines []avLineRecord, pages map[string][]string) []convIssue {
	var issues []convIssue
	for i := range lines {
		l := &lines[i]
		if !convSpoken(l) || l.TAudioEndMS <= 0 || l.TAudioStartEventMS <= 0 {
			continue
		}
		ord := convPageOrdinal(lines, i)
		pageRows, ok := pages[l.MessageID]
		if !ok || ord-1 >= len(pageRows) || pageRows[ord-1] == "" {
			continue // no page row: no terminal-punctuation ground truth
		}
		if convTerminalPunct(pageRows[ord-1]) {
			continue // the sentence ended on this page
		}
		// The next spoken line's audio start: the sentence's second half.
		for j := i + 1; j < len(lines); j++ {
			n := &lines[j]
			if !convSpoken(n) || n.TAudioStartEventMS <= 0 {
				continue
			}
			// A run covering this page already speaks the continuation in
			// the same audio: the gap is the run's own (its stamps are the
			// run's, so the next distinct start is the honest one).
			if n.Batched && n.RunOccurrenceID == l.RunOccurrenceID {
				continue
			}
			gap := n.TAudioStartEventMS - l.TAudioEndMS
			if gap < 0 {
				gap = 0
			}
			issues = append(issues, convIssue{
				Kind: "PAGE_BREAK_GAP", Index: i, MessageID: l.MessageID,
				Detail: fmt.Sprintf("the sentence crosses the page break (page ordinal %d of %s ends without terminal punctuation); the gap to the next utterance is %d ms%s",
					ord, l.MessageID, gap, flagWord(gap)),
			})
			break
		}
	}
	return issues
}

// flagWord is the report-only flag a page-break gap above the bound carries.
func flagWord(gapMS int64) string {
	if gapMS > int64(convPageBreakGap.Milliseconds()) {
		return " — PAGE_BREAK_GAP (flag, report-only; bound 1500 ms)"
	}
	return " (within the 1500 ms report bound)"
}

// convCheckConversation runs every ADR-0170 invariant over the timeline:
// the gates (duplicate, read-ahead, order, and — slow drive only —
// queue-don't-cut) and the report-only page-break gaps. It returns the
// issues grouped by gate status: the gate's input is the non-PAGE_BREAK_GAP
// issues (the harness fails the run on any of them); the PAGE_BREAK_GAP
// rows are the report's page-break section.
func convCheckConversation(lines []avLineRecord, pages map[string][]string, slowDrive bool) (gateIssues, reportIssues []convIssue) {
	gateIssues = append(gateIssues, convCheckDuplicates(lines, pages)...)
	gateIssues = append(gateIssues, convCheckOrder(lines)...)
	for _, g := range convCheckReadAhead(lines, pages) {
		if g.Kind == "READ-AHEAD-NOISE" {
			reportIssues = append(reportIssues, g) // ASR noise: judged by the WER gate
		} else {
			gateIssues = append(gateIssues, g)
		}
	}
	if slowDrive {
		qGate, qReport := convCheckQueue(lines)
		gateIssues = append(gateIssues, qGate...)
		reportIssues = append(reportIssues, qReport...)
	}
	reportIssues = append(reportIssues, convPageBreakGaps(lines, pages)...)
	return gateIssues, reportIssues
}

// ── unit tests on synthetic timelines (ADR-0170) ─────────────────────────
// The synthetic strings are invented (AGENTS.md rule 5: no game text); the
// shapes are the harness's real timeline fields.

// convLineFor builds one synthetic line on the shared ms clock.
func convLineFor(idx int, msg string, commitMS, startMS, endMS int64, endResult, transcript string, batched bool, runOcc string) avLineRecord {
	return avLineRecord{
		Index: idx, MessageID: msg, TCommitMS: commitMS,
		TAudioStartEventMS: startMS, TAudioEndMS: endMS,
		EndResult: endResult, ASRTranscript: transcript,
		Batched: batched, RunOccurrenceID: runOcc,
		RequestedText: transcript,
	}
}

func TestConvInvariantSpokenOnce(t *testing.T) {
	// Clean: one own-audio line per page, two pages of one message.
	lines := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "alpha beta gamma", false, ""),
		convLineFor(1, "msg-a", 1000, 1100, 1900, "ended", "delta epsilon zeta", false, ""),
	}
	if got := convCheckDuplicates(lines, nil); len(got) != 0 {
		t.Fatalf("clean timeline: got %v, want none", got)
	}
	// The same page spoken twice (two own-audio lines on the page key).
	dup := append([]avLineRecord{}, lines...)
	dup = append(dup, convLineFor(2, "msg-a", 2200, 2400, 3000, "ended", "eta theta iota kappa", false, ""))
	// Same message, three occurrences → the second copy is ordinal 2;
	// three different pages each spoken once is clean.
	if got := convCheckDuplicates(dup, nil); len(got) != 0 {
		t.Fatalf("three sequential pages: got %v, want none", got)
	}
	// Transcript half: two DIFFERENT messages with near-identical
	// transcripts close together → DUPLICATE.
	rep := []avLineRecord{
		convLineFor(0, "msg-b", 100, 200, 900, "ended", "alpha beta gamma delta", false, ""),
		convLineFor(1, "msg-c", 500, 600, 1400, "ended", "alpha beta gamma delta", false, ""),
	}
	got := convCheckDuplicates(rep, nil)
	if len(got) != 1 || got[0].Kind != "DUPLICATE" {
		t.Fatalf("near-identical transcripts within 20 s: got %v, want one DUPLICATE", got)
	}
	// ... but the same text spoken > 20 s apart is NOT a duplicate
	// (the window rule).
	late := []avLineRecord{
		convLineFor(0, "msg-b", 100, 200, 900, "ended", "alpha beta gamma delta", false, ""),
		convLineFor(1, "msg-c", 100000, 101000, 101800, "ended", "alpha beta gamma delta", false, ""),
	}
	if got := convCheckDuplicates(late, nil); len(got) != 0 {
		t.Fatalf("identical text > 20 s apart: got %v, want none", got)
	}
}

func TestConvInvariantDuplicateSamePageTwoUtterances(t *testing.T) {
	// A page covered by a merged run AND also spoken by its own separate
	// utterance → the page is covered twice.
	lines := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "", true, "run-1"),
		convLineFor(1, "msg-a", 1000, 1100, 1900, "ended", "", true, "run-1"),
		// Line 2: a LATER page of the same message (ordinal 3) — clean.
		convLineFor(2, "msg-a", 2000, 2200, 3000, "ended", "", false, ""),
	}
	if got := convCheckDuplicates(lines, nil); len(got) != 0 {
		t.Fatalf("merged run covering pages 1-2 + own page 3: got %v, want none", got)
	}
	// The same page spoken twice: the profile gives both ordinals the
	// same box text (a re-shown box) and the transcripts agree.
	lines2 := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "alpha beta", false, ""),
		convLineFor(1, "msg-b", 500, 550, 1200, "ended", "", false, ""), // filler page
		convLineFor(2, "msg-a", 600, 650, 1400, "ended", "alpha beta", false, ""),
	}
	pages2 := map[string][]string{"msg-a": {"alpha beta", "alpha beta"}}
	got := convCheckDuplicates(lines2, pages2)
	if len(got) < 1 {
		t.Fatalf("re-spoken page: got %v, want a DUPLICATE", got)
	}
	if got[0].Kind != "DUPLICATE" {
		t.Fatalf("kind = %q, want DUPLICATE", got[0].Kind)
	}
	// The same transcripts on pages with DIFFERENT box texts: the wiring
	// spoke the same text for two different boxes — the page the player
	// had on screen was never spoken (a re-spoken page). DUPLICATE.
	pagesOther := map[string][]string{"msg-a": {"alpha beta", "delta epsilon"}}
	if got := convCheckDuplicates(lines2, pagesOther); len(got) < 1 || got[0].Kind != "DUPLICATE" {
		t.Fatalf("same text on different boxes: got %v, want a DUPLICATE", got)
	}
	// Genuinely different transcripts on different boxes: clean.
	linesClean := []avLineRecord{lines2[0], lines2[1],
		convLineFor(2, "msg-a", 600, 650, 1400, "ended", "delta epsilon zeta", false, "")}
	if got := convCheckDuplicates(linesClean, pagesOther); len(got) != 0 {
		t.Fatalf("distinct pages, distinct speech: got %v, want none", got)
	}
}

func TestConvInvariantReadAhead(t *testing.T) {
	pages := map[string][]string{
		"msg-a": {"alpha beta gamma.", "delta epsilon zeta.", "eta theta iota."},
	}
	// Clean: the page-1 utterance stays in page 1.
	lines := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "alpha beta gamma", false, ""),
	}
	if got := convCheckReadAhead(lines, pages); len(got) != 0 {
		t.Fatalf("in-page utterance: got %v, want none", got)
	}
	// Read-ahead: the page-1 utterance speaks page 2's words.
	ra := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "alpha beta gamma delta epsilon zeta", false, ""),
	}
	got := convCheckReadAhead(ra, pages)
	found := false
	for _, g := range got {
		if g.Kind == "READ-AHEAD" && g.Index == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("page-2 words in the page-1 utterance: got %v, want a READ-AHEAD on line 0", got)
	}
	// The merged catch-up run covering pages 1-2 speaking both pages is
	// clean (the covered set is the run's pages).
	batched := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "alpha beta gamma delta epsilon zeta", true, "run-1"),
		convLineFor(1, "msg-a", 400, 200, 900, "ended", "alpha beta gamma delta epsilon zeta", true, "run-1"),
	}
	if got := convCheckReadAhead(batched, pages); len(got) != 0 {
		t.Fatalf("merged run over pages 1-2: got %v, want none", got)
	}
	// The subset half: two words no covered page carries → READ-AHEAD.
	outside := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "alpha beta gamma qwertyzz plorpusk", false, ""),
	}
	got = convCheckReadAhead(outside, pages)
	found = false
	for _, g := range got {
		if g.Kind == "READ-AHEAD" && strings.Contains(g.Detail, "⊄") {
			found = true
		}
	}
	if !found {
		t.Fatalf("words outside every covered page: got %v, want a READ-AHEAD (subset)", got)
	}
	// A single unexplained word (the player-name substitution: the
	// requested text carries the learned name where the page row carries
	// the placeholder) is admitted on both sides.
	name := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "alpha beta gamma aryll", false, ""),
	}
	if got := convCheckReadAhead(name, pages); len(got) != 0 {
		t.Fatalf("single unexplained word (the player name): got %v, want none", got)
	}
	// Transcript strays beyond the name allowance are ASR noise — the
	// report-only READ-AHEAD-NOISE kind, not the gate's READ-AHEAD.
	noise := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "alpha beta gamma qwertyzz plorpusk", false, ""),
	}
	noise[0].RequestedText = "alpha beta gamma" // the deterministic side is clean
	got = convCheckReadAhead(noise, pages)
	if len(got) != 1 || got[0].Kind != "READ-AHEAD-NOISE" {
		t.Fatalf("acoustic strays: got %v, want the report-only READ-AHEAD-NOISE", got)
	}
	// A message without page rows → no ground truth → no flag.
	noPages := map[string][]string{}
	if got := convCheckReadAhead(lines, noPages); len(got) != 0 {
		t.Fatalf("no page rows: got %v, want none (no ground truth)", got)
	}
}

func TestConvInvariantReadAheadWholeMessage(t *testing.T) {
	// The owner's acceptance case: a three-page message spoken ALL AT ONCE
	// — the page-1 utterance's requested text carries the whole message.
	// Each of pages 2 and 3's words is a page not yet on screen at the
	// audio start, so the rule must flag the page-1 line.
	pages := map[string][]string{
		"msg-m": {"alpha beta gamma.", "delta epsilon zeta.", "eta theta iota."},
	}
	whole := []avLineRecord{
		convLineFor(0, "msg-m", 100, 200, 900, "ended", "alpha beta gamma", false, ""),
	}
	whole[0].RequestedText = "alpha beta gamma delta epsilon zeta eta theta iota"
	got := convCheckReadAhead(whole, pages)
	found := false
	for _, g := range got {
		if g.Kind == "READ-AHEAD" && g.Index == 0 && strings.Contains(g.Detail, "had not reached") {
			found = true
		}
	}
	if !found {
		t.Fatalf("whole message spoken on page 1: got %v, want a READ-AHEAD (later-page words)", got)
	}
	// The OCR noise floor: the box reading the player saw misread a word
	// (the committed page says \"alpha beta grama.\" while the requested
	// text says \"gamma\"): one excess word is the substitution/misread
	// allowance, and the texts still agree, so the subset clause does not
	// flag a single misread word.
	noise := []avLineRecord{
		convLineFor(0, "msg-m", 100, 200, 900, "ended", "alpha beta gamma", false, ""),
	}
	noisePages := map[string][]string{
		"msg-m": {"alpha beta grama.", "delta epsilon zeta.", "eta theta iota."},
	}
	if got := convCheckReadAhead(noise, noisePages); len(got) != 0 {
		t.Fatalf("misread page word: got %v, want none", got)
	}
	// Two excess words the page does not carry and that are no page's
	// words (the Dice floor is still under — the texts no longer agree):
	// the subset clause flags.
	badTwo := []avLineRecord{
		convLineFor(0, "msg-m", 100, 200, 900, "ended", "alpha beta gamma qwertyzz plorpusk", false, ""),
	}
	badTwo[0].RequestedText = "alpha beta gamma qwertyzz plorpusk"
	if got := convCheckReadAhead(badTwo, pages); len(got) != 2 { // gate + report-side rows for the same excess words
		t.Fatalf("two excess words, texts disagree: got %v, want the READ-AHEAD", got)
	}
}

func TestConvInvariantOrder(t *testing.T) {
	// Clean: audio starts follow the commit order.
	lines := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "x", false, ""),
		convLineFor(1, "msg-b", 500, 1000, 1800, "ended", "y", false, ""),
	}
	if got := convCheckOrder(lines); len(got) != 0 {
		t.Fatalf("ordered starts: got %v, want none", got)
	}
	// Inverted: line 1's audio starts before line 0's.
	bad := []avLineRecord{
		convLineFor(0, "msg-a", 100, 2000, 2600, "ended", "x", false, ""),
		convLineFor(1, "msg-b", 500, 300, 900, "ended", "y", false, ""),
	}
	got := convCheckOrder(bad)
	if len(got) != 1 || got[0].Kind != "OUT-OF-ORDER" {
		t.Fatalf("inverted starts: got %v, want one OUT-OF-ORDER", got)
	}
	// A merged run's covered lines share the run's stamps: no inversion
	// inside the run (positions order the pages, the stamps are equal).
	run := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "x", true, "run-1"),
		convLineFor(1, "msg-b", 400, 200, 900, "ended", "y", true, "run-1"),
	}
	if got := convCheckOrder(run); len(got) != 0 {
		t.Fatalf("merged run's shared stamps: got %v, want none", got)
	}
}

func TestConvInvariantQueue(t *testing.T) {
	// Clean: page N ends, page N+1 starts 120 ms later; both ended; the
	// acoustic spans ride the full synthesized duration.
	lines := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "x", false, ""),
		convLineFor(1, "msg-b", 500, 1020, 1700, "ended", "y", false, ""),
	}
	lines[0].SynthDurMS = 700
	lines[1].SynthDurMS = 680
	if gate, report := convCheckQueue(lines); len(gate) != 0 || len(report) != 0 {
		t.Fatalf("clean queue: got %v / %v, want none", gate, report)
	}
	// Cut off: page 0's terminal is interrupted and its acoustic span is
	// under the floor; the start-gap is fine.
	cut := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 500, "interrupted", "x", false, ""),
		convLineFor(1, "msg-b", 500, 620, 1300, "ended", "y", false, ""),
	}
	cut[0].SynthDurMS = 700
	cut[1].SynthDurMS = 680
	var gate, report []convIssue
	gate, report = convCheckQueue(cut)
	termCut, shortCut := false, false
	for _, g := range gate {
		if g.Kind != "CUT-OFF" || g.Index != 0 {
			continue
		}
		if strings.Contains(g.Detail, "interrupted") {
			termCut = true
		}
		if strings.Contains(g.Detail, "under") {
			shortCut = true
		}
	}
	if !termCut || !shortCut {
		t.Fatalf("cut page: got %v, want the terminal and the duration CUT-OFFs", gate)
	}
	// The stalled queue: page 1 was committed mid-playback (inside page 0's
	// acoustic window) and its audio starts 2 s after page 0 ended.
	stall := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "x", false, ""),
		convLineFor(1, "msg-b", 500, 2900, 3600, "ended", "y", false, ""),
	}
	stall[0].SynthDurMS = 700
	stall[1].SynthDurMS = 700
	gate, _ = convCheckQueue(stall)
	stalled := false
	for _, g := range gate {
		if g.Kind == "CUT-OFF" && g.Index == 0 && strings.Contains(g.Detail, "stalled") {
			stalled = true
		}
	}
	if !stalled {
		t.Fatalf("2 s start gap: got %v, want the stalled-queue CUT-OFF", gate)
	}
	// The player waited: page 1 committed AFTER page 0 ended. The start gap
	// is then the player's own pace (the slow drive's press + the box
	// recognition), not the runtime's queue: report-only, never gated.
	waited := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "x", false, ""),
		convLineFor(1, "msg-b", 1400, 2900, 3600, "ended", "y", false, ""),
	}
	waited[0].SynthDurMS = 700
	waited[1].SynthDurMS = 700
	if gate, report := convCheckQueue(waited); len(gate) != 0 || len(report) != 1 || report[0].Kind != "QUEUE-GAP" {
		t.Fatalf("waited advance: got %v / %v, want no gate issues and one QUEUE-GAP report row", gate, report)
	}
	// A line whose synthesized duration is unmeasured (no file-sink WAV):
	// the terminal check still runs, the duration half does not.
	noWAV := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 300, "ended", "x", false, ""),
		convLineFor(1, "msg-b", 250, 420, 1100, "ended", "y", false, ""),
	}
	noWAV[1].SynthDurMS = 0 // line 0 has no WAV → no duration finding
	if gate, report := convCheckQueue(noWAV); len(gate) != 0 || len(report) != 0 {
		t.Fatalf("unmeasured duration: got %v / %v, want none", gate, report)
	}
	// Queued behind speech the timeline's correlation recorded (the runtime
	// set the flag): the 800 ms bound gates even though the commit stamps
	// alone would not show the overlap.
	flagged := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "x", false, ""),
		convLineFor(1, "msg-b", 1400, 2900, 3600, "ended", "y", false, ""),
	}
	flagged[1].QueuedBehindSpeech = true
	flagged[0].SynthDurMS = 700
	flagged[1].SynthDurMS = 700
	gate, report = convCheckQueue(flagged)
	stalled = false
	for _, g := range gate {
		if g.Kind == "CUT-OFF" && g.Index == 0 && strings.Contains(g.Detail, "stalled") {
			stalled = true
		}
	}
	if !stalled || len(report) != 0 {
		t.Fatalf("queued flag: got %v / %v, want the stalled-queue CUT-OFF and no report row", gate, report)
	}
}

func TestConvInvariantPageBreakGap(t *testing.T) {
	pages := map[string][]string{
		"msg-a": {"alpha beta gamma", "delta epsilon."}, // page 1: no terminal
	}
	// Clean: the continuation starts 300 ms after page 1 ended.
	lines := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "alpha beta gamma", false, ""),
		convLineFor(1, "msg-a", 1000, 1200, 1900, "ended", "delta epsilon", false, ""),
	}
	got := convPageBreakGaps(lines, pages)
	if len(got) != 1 || got[0].Kind != "PAGE_BREAK_GAP" {
		t.Fatalf("gap report row: got %v, want one PAGE_BREAK_GAP", got)
	}
	if strings.Contains(got[0].Detail, "PAGE_BREAK_GAP (flag") {
		t.Fatalf("300 ms gap must not carry the flag: %s", got[0].Detail)
	}
	// The flagged gap: the continuation starts 2 s later.
	late := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "alpha beta gamma", false, ""),
		convLineFor(1, "msg-a", 1000, 2900, 3600, "ended", "delta epsilon", false, ""),
	}
	got = convPageBreakGaps(late, pages)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "PAGE_BREAK_GAP (flag") {
		t.Fatalf("2 s gap: got %v, want the flagged PAGE_BREAK_GAP", got)
	}
	// A page that ends terminally keeps no row.
	term := map[string][]string{"msg-a": {"alpha beta gamma.", "delta epsilon."}}
	if got := convPageBreakGaps(lines, term); len(got) != 0 {
		t.Fatalf("terminal-punctuation page: got %v, want none", got)
	}
	// A merged run already speaks the continuation in the same audio: the
	// next distinct start is the one measured (no self-gap).
	merged := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "ended", "alpha beta gamma delta epsilon", true, "run-1"),
		convLineFor(1, "msg-a", 400, 200, 900, "ended", "alpha beta gamma delta epsilon", true, "run-1"),
		convLineFor(2, "msg-b", 1500, 1100, 1800, "ended", "zeta", false, ""),
	}
	got = convPageBreakGaps(merged, pages)
	if len(got) != 1 || got[0].Index != 0 {
		t.Fatalf("merged continuation: got %v, want the gap to line 2 only", got)
	}
}

func TestConvInvariantConversationGateSplit(t *testing.T) {
	// The gate split: the gate issues carry the real defects, the report
	// issues carry the page-break rows; the queue rule runs on the slow
	// drive only.
	lines := []avLineRecord{
		convLineFor(0, "msg-a", 100, 200, 900, "interrupted", "alpha beta gamma delta", false, ""),
		convLineFor(1, "msg-a", 500, 1100, 1800, "ended", "delta epsilon.", false, ""),
	}
	lines[0].SynthDurMS = 700
	pages := map[string][]string{"msg-a": {"alpha beta gamma", "delta epsilon."}}
	gate, report := convCheckConversation(lines, pages, true) // slow drive
	kinds := map[string]bool{}
	for _, g := range gate {
		kinds[g.Kind] = true
	}
	if !kinds["READ-AHEAD"] || !kinds["CUT-OFF"] {
		t.Fatalf("slow drive gate: got %v, want the READ-AHEAD and the CUT-OFFs", gate)
	}
	for _, g := range gate {
		if g.Kind == "PAGE_BREAK_GAP" {
			t.Fatalf("page-break rows are report-only: %v in the gate set", g)
		}
	}
	if len(report) != 1 || report[0].Kind != "PAGE_BREAK_GAP" {
		t.Fatalf("report set: got %v, want the one PAGE_BREAK_GAP row", report)
	}
	// The rapid drive skips the queue rule (the fence cut is by design).
	gateRapid, _ := convCheckConversation(lines, pages, false)
	for _, g := range gateRapid {
		if g.Kind == "CUT-OFF" {
			t.Fatalf("rapid drive: the queue rule must not run: %v", g)
		}
	}
}
