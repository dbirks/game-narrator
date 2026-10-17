package tools

// source_quality — the "Dialogue source quality" report of docs/spec/03:
// per-source message_count, unique_ids, nonempty_text_pct,
// speaker_labeled_pct, graph_linked_pct, conflicts, and the standing
// coverage note that a source is incomplete by default.
//
// Conflict handling (docs/spec/03 scenario 4 and 10): when the same
// source_message_id carries different text under two sources, the conflict
// is quarantined — the lower-trust rows get a durable `quarantined` flag —
// and the tool asks the run to stop at the review boundary. A directly
// extracted, user-supplied row (higher trust) is preferred over a generated
// or fetched one; on a tie every conflicting row is quarantined.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/games"
)

// conflictNote bounds the text quoted in a conflict report (a full message
// is 8 000 chars; the report is model- and UI-facing).
const conflictNote = 200

// sourceQualityReport is the structured output.
type sourceQualityReport struct {
	SourceID              string           `json:"source_id"`
	SourceType            string           `json:"source_type"`
	MessageCount          int              `json:"message_count"`
	UniqueIDs             int              `json:"unique_ids"`
	NonemptyTextPct       float64          `json:"nonempty_text_pct"`
	SpeakerLabeledPct     float64          `json:"speaker_labeled_pct"`
	GraphLinkedPct        float64          `json:"graph_linked_pct"`
	LanguageConsistency   float64          `json:"language_consistency"`
	Conflicts             []conflictReport `json:"conflicts"`
	QuarantinedMessageIDs []string         `json:"quarantined_message_ids"`
	CoverageNote          string           `json:"coverage_note"`
}

type conflictReport struct {
	SourceMessageID    string            `json:"source_message_id"`
	PreferredMessageID string            `json:"preferred_message_id,omitempty"`
	PreferredSourceID  string            `json:"preferred_source_id,omitempty"`
	PreferReason       string            `json:"prefer_reason,omitempty"`
	Variants           []conflictVariant `json:"variants"`
}

type conflictVariant struct {
	SourceID    string `json:"source_id"`
	MessageID   string `json:"message_id"`
	Text        string `json:"text"`
	Quarantined bool   `json:"quarantined"`
}

func (d Deps) sourceQuality(ctx context.Context, call agent.Call) (agent.Result, error) {
	m, err := parseArgs(call)
	if err != nil {
		return agent.Result{}, err
	}
	profileID, err := require(m, "profile_id")
	if err != nil {
		return agent.Result{}, err
	}
	sourceID, err := require(m, "source_id")
	if err != nil {
		return agent.Result{}, err
	}

	sources, err := d.Store.ListSources(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: source_quality: list sources: %w", err)
	}
	var target games.Source
	sourceTrust := map[string]float64{}
	sourceType := map[string]games.SourceType{}
	for _, s := range sources {
		sourceTrust[s.ID] = s.Trust
		sourceType[s.ID] = s.SourceType
		if s.ID == sourceID {
			target = s
		}
	}
	if target.ID == "" {
		return agent.Result{}, fmt.Errorf("source %s is not registered on profile %s", sourceID, profileID)
	}

	msgs, err := d.listMessages(ctx, profileID)
	if err != nil {
		return agent.Result{}, err
	}
	edges, err := d.Store.ListEdges(ctx, profileID)
	if err != nil {
		return agent.Result{}, fmt.Errorf("tools: source_quality: list edges: %w", err)
	}
	edgeEndpoints := map[string]bool{}
	for _, e := range edges {
		edgeEndpoints[e.FromID] = true
		edgeEndpoints[e.ToID] = true
	}

	// Per-source statistics.
	var (
		count, nonempty, labeled, linked int
		ids                              = map[string]bool{}
		langCounts                       = map[string]int{}
	)
	for _, msg := range msgs {
		if msg.SourceID != sourceID {
			continue
		}
		count++
		if msg.SourceMessageID != "" {
			ids[msg.SourceMessageID] = true
		}
		if strings.TrimSpace(msg.OriginalText) != "" {
			nonempty++
		}
		if msg.SpeakerID != "" {
			labeled++
		}
		if msg.SceneID != "" || edgeEndpoints[msg.ID] {
			linked++
		}
		lang := msg.Language
		if lang == "" {
			lang = "(unspecified)"
		}
		langCounts[lang]++
	}
	consistency := 1.0
	if count > 0 {
		var dominant int
		for _, n := range langCounts {
			if n > dominant {
				dominant = n
			}
		}
		consistency = round4(float64(dominant) / float64(count))
	}
	rep := sourceQualityReport{
		SourceID:              sourceID,
		SourceType:            string(target.SourceType),
		MessageCount:          count,
		UniqueIDs:             len(ids),
		NonemptyTextPct:       pct(count, nonempty),
		SpeakerLabeledPct:     pct(count, labeled),
		GraphLinkedPct:        pct(count, linked),
		LanguageConsistency:   consistency,
		Conflicts:             []conflictReport{},
		QuarantinedMessageIDs: []string{},
		// docs/spec/03: "Source is incomplete by default until strong
		// evidence confirms otherwise."
		CoverageNote: "incomplete by default until strong evidence confirms otherwise",
	}

	// Conflict scan: same source_message_id, different text, across sources.
	bySideID := map[string][]games.Message{}
	for _, msg := range msgs {
		if msg.SourceID == "" || msg.SourceMessageID == "" {
			continue
		}
		bySideID[msg.SourceMessageID] = append(bySideID[msg.SourceMessageID], msg)
	}
	var toQuarantine []games.Message
	for sideID, rows := range bySideID {
		texts := map[string]bool{}
		for _, r := range rows {
			texts[textOf(r)] = true
		}
		if len(texts) <= 1 {
			continue
		}
		// This conflict is reported when it involves the requested source.
		involves := false
		for _, r := range rows {
			if r.SourceID == sourceID {
				involves = true
			}
		}
		if !involves {
			continue
		}

		// Preference: strictly higher rank wins; a user_file row at equal
		// trust is the directly-extracted row (docs/spec/03 scenario 10:
		// extracted ids beat generated text). Ties quarantine everything.
		best, bestRank := rows[0], rankRow(rows[0], sourceTrust, sourceType)
		for _, r := range rows[1:] {
			rr := rankRow(r, sourceTrust, sourceType)
			if rr > bestRank {
				best, bestRank = r, rr
			}
		}
		// A strict winner is the unique maximum: every other row ranks
		// strictly below it (a tie, even at float-epsilon scale, is a
		// conflict for a human, not a choice).
		strict := true
		for _, r := range rows {
			if r.ID != best.ID && rankRow(r, sourceTrust, sourceType) >= bestRank {
				strict = false
				break
			}
		}
		preferredID := ""
		if strict {
			preferredID = best.ID
		}

		variants := make([]conflictVariant, 0, len(rows))
		for _, r := range rows {
			q := preferredID == "" || preferredID != r.ID
			if q {
				toQuarantine = append(toQuarantine, r)
			}
			variants = append(variants, conflictVariant{
				SourceID: r.SourceID, MessageID: r.ID,
				Text: clip(textOf(r), conflictNote), Quarantined: q,
			})
		}
		cr := conflictReport{SourceMessageID: sideID, Variants: variants}
		if preferredID != "" {
			cr.PreferredMessageID = preferredID
			cr.PreferReason = "higher source trust (extracted, user-supplied material beats generated or fetched text)"
			for _, r := range rows {
				if r.ID == preferredID {
					cr.PreferredSourceID = r.SourceID
				}
			}
		} else {
			cr.PreferReason = "no strict winner; all conflicting rows quarantined for human review"
		}
		rep.Conflicts = append(rep.Conflicts, cr)
	}

	if len(toQuarantine) > 0 {
		if err := d.quarantine(ctx, profileID, toQuarantine); err != nil {
			return agent.Result{}, err
		}
		for _, r := range toQuarantine {
			rep.QuarantinedMessageIDs = append(rep.QuarantinedMessageIDs, r.ID)
		}
	}
	res, err := okResult(rep)
	if err != nil {
		return agent.Result{}, err
	}
	res.SourceIDs = []string{sourceID}
	if len(rep.Conflicts) > 0 {
		res.ReviewRequired = true
	}
	return res, nil
}

// textOf is the text a conflict compares: normalized when present (the
// speech-facing form), otherwise the original.
func textOf(m games.Message) string {
	if t := strings.TrimSpace(m.NormalizedText); t != "" {
		return t
	}
	return strings.TrimSpace(m.OriginalText)
}

// rankRow scores a conflicting row: trust first, then user_file provenance
// (a directly extracted row beats a fetched page at equal trust).
func rankRow(m games.Message, trust map[string]float64, typ map[string]games.SourceType) float64 {
	r := trust[m.SourceID]
	if typ[m.SourceID] == games.SourceUserFile {
		r += 1e-9 // tie-break within float precision
	}
	return r
}

// quarantine marks the given rows with a durable flags entry. The rows are
// upserted whole (the store's message upsert is a revision-bumped replace),
// so the existing flags are preserved and merged, not clobbered.
func (d Deps) quarantine(ctx context.Context, profileID string, rows []games.Message) error {
	for i := range rows {
		m := rows[i]
		flags := map[string]any{}
		if len(m.Flags) > 0 {
			_ = json.Unmarshal(m.Flags, &flags)
		}
		flags["quarantined"] = true
		flags["quarantine_reason"] = "conflicting text for source_message_id (docs/spec/03 scenarios 4 and 10)"
		m.Flags, _ = json.Marshal(flags)
		m.UpdatedAt = nowStamp()
		if err := d.Store.PutMessages(ctx, profileID, []games.Message{m}, nil); err != nil {
			return fmt.Errorf("tools: source_quality: quarantine %s: %w", m.ID, err)
		}
	}
	return nil
}

func pct(total, n int) float64 {
	if total == 0 {
		return 0
	}
	return round4(float64(n) / float64(total))
}

func round4(f float64) float64 {
	if f == 0 || f == 1 {
		return f
	}
	return float64(int64(f*10000+0.5)) / 10000
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
