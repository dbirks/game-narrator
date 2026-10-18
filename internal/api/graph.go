// Package api — the dialogue-graph reader (docs/spec/09 "/games/:id/graph"):
// the virtualized scene/message graph a curator browses, with branch/loop/repeat
// edges, confidence and source labels, filtered by speaker, message kind and
// scene. Reads only; the graph is import- and label-derived, so the write path
// is the message PATCH (speech text) and the import itself.
//
// The page contract: one page of nodes (keyset-cursor paginated, ≤ 2000) plus
// every edge whose two endpoints both land on that page, so a filtered or
// partial page never dangles an edge at a node it did not return. The full
// edge list of the revision is folded in memory — bounded by the revision's
// own import size, which is the same bound the dialogue browser already has.

package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

const (
	// graphMaxLimit bounds one page of the graph: 2 000 nodes keeps the
	// virtualized list fast and the edge fold bounded (docs/spec/09 "virtualized
	// … don't load thousands of messages into React memory").
	graphMaxLimit = 2000
	// graphDefaultLimit is one pageful for a list pane, not the whole revision.
	graphDefaultLimit = 400
	// graphTextRunes caps the node's short text: the list row is a label, and
	// the full text is one message GET away (docs/spec/09 "short normalized
	// text ≤ 80 chars").
	graphTextRunes = 80
)

// textKindsWire is the error message's enumeration: it names the rule a 400
// caller must fix, and it is the closed set the schema CHECK enforces.
const textKindsWire = "dialogue, sign, item, menu, tutorial, system or other"

// graphNodeDTO is one list row: the message's identity, its display names, the
// short normalized text, and the attribution a "verified vs inferred" label
// renders (docs/spec/09 "highly visible … 'inferred' vs 'verified' labels").
type graphNodeDTO struct {
	ID                    string        `json:"id"`
	Kind                  string        `json:"kind"`
	Text                  string        `json:"text"`
	AttributionStatus     string        `json:"attribution_status"`
	AttributionConfidence float64       `json:"attribution_confidence"`
	Speaker               *graphNameDTO `json:"speaker"`
	Scene                 *graphNameDTO `json:"scene"`
}

// graphNameDTO is the display name of a named row (speaker character or
// scene). Null when the message carries no such row: the UI renders
// "unattributed" / "no scene" from the absence, not from a name.
type graphNameDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// graphEdgeDTO is one dialogue transition with its provenance: the kind the
// legend colours, the confidence that ranks it, the source row that established
// it and the number of observations backing it (docs/spec/04 "Dialogue graph").
type graphEdgeDTO struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Kind       string  `json:"kind"`
	Confidence float64 `json:"confidence"`
	Source     *string `json:"source"`
	Evidence   int     `json:"evidence"`
	Condition  string  `json:"condition,omitempty"`
}

// graphResponse is the page a virtualized renderer consumes.
type graphResponse struct {
	Nodes      []graphNodeDTO `json:"nodes"`
	Edges      []graphEdgeDTO `json:"edges"`
	NextCursor string         `json:"next_cursor,omitempty"`
	Total      int            `json:"total"`
}

// handleProfileGraph serves GET /api/v1/profiles/{id}/graph?speaker=&kind=&
// scene=&cursor=&limit= → nodes + edges, filters applied server-side, page
// bounded by limit (≤ graphMaxLimit).
func (s *Server) handleProfileGraph(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	q := r.URL.Query()

	// Filters are exact matches on the stored columns. Each is validated before
	// it touches the store so a malformed value is a 400 naming the rule, not a
	// silent empty page that reads as "no such speaker exists".
	var f store.MessageFilter
	if kind := q.Get("kind"); kind != "" {
		tk := games.TextKind(kind)
		if !tk.Valid() {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-kind", codeInvalidParameter,
				"Bad Request", "kind must be one of: "+textKindsWire)
			return
		}
		f.TextKind = tk
	}
	if speaker := q.Get("speaker"); speaker != "" {
		if !games.ValidID(speaker) {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-speaker", codeInvalidParameter,
				"Bad Request", "speaker must be a character id from the revision's roster")
			return
		}
		f.SpeakerID = speaker
	}
	if scene := q.Get("scene"); scene != "" {
		if !games.ValidID(scene) {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-scene", codeInvalidParameter,
				"Bad Request", "scene must be a scene id of this revision")
			return
		}
		f.SceneID = scene
	}

	limit, ok := s.graphLimitParam(w, r)
	if !ok {
		return
	}

	nodes, next, err := s.store.ListGraphNodes(r.Context(), p.ID, f, q.Get("cursor"), limit)
	if err != nil {
		s.storeProblem(w, r, err, "list graph nodes", "message")
		return
	}

	// Edges are the whole revision's transitions folded onto this page's node
	// set: an edge is rendered only when both endpoints are visible, so the
	// page is a connected subgraph rather than a half-edge at its boundary.
	edges, err := s.store.ListEdges(r.Context(), p.ID)
	if err != nil {
		s.storeProblem(w, r, err, "list edges", "edge")
		return
	}
	total, err := s.store.CountGraphNodes(r.Context(), p.ID, f)
	if err != nil {
		s.storeProblem(w, r, err, "count messages", "message")
		return
	}

	visible := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		visible[n.ID] = true
	}
	out := graphResponse{Nodes: make([]graphNodeDTO, 0, len(nodes)), Edges: []graphEdgeDTO{}, Total: total}
	for _, n := range nodes {
		dto := graphNodeDTO{
			ID:                    n.ID,
			Kind:                  string(n.TextKind),
			Text:                  shortText(n.NormalizedText, graphTextRunes),
			AttributionStatus:     string(n.AttributionStatus),
			AttributionConfidence: n.AttributionConfidence,
		}
		if n.SpeakerID != "" {
			dto.Speaker = &graphNameDTO{ID: n.SpeakerID, Name: n.SpeakerName}
		}
		if n.SceneID != "" {
			dto.Scene = &graphNameDTO{ID: n.SceneID, Name: n.SceneName}
		}
		out.Nodes = append(out.Nodes, dto)
	}
	for _, e := range edges {
		if !visible[e.FromID] || !visible[e.ToID] {
			continue
		}
		dto := graphEdgeDTO{
			From:       e.FromID,
			To:         e.ToID,
			Kind:       string(e.EdgeType),
			Confidence: e.Confidence,
			Evidence:   jsonEvidenceCount(e.Evidence),
		}
		if e.SourceID != "" {
			dto.Source = &e.SourceID
		}
		dto.Condition = e.ConditionExpr
		out.Edges = append(out.Edges, dto)
	}
	out.NextCursor = next
	setETag(w, version)
	writeJSON(w, http.StatusOK, out)
}

// jsonEvidenceCount is the evidence array's length: the edge's backing
// observations are stored as a JSON array the schema only checks is valid
// (0002), so the count is derived, never stored.
func jsonEvidenceCount(evidence json.RawMessage) int {
	var arr []json.RawMessage
	if err := json.Unmarshal(evidence, &arr); err != nil {
		return 0
	}
	return len(arr)
}

// graphLimitParam parses ?limit= against graphMaxLimit; an omitted limit is the
// list-pane default, not the store's, because a graph page wants more rows than
// a catalogue page does.
func (s *Server) graphLimitParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return graphDefaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > graphMaxLimit {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-limit", codeInvalidParameter,
			"Bad Request", "limit must be an integer between 1 and "+strconv.Itoa(graphMaxLimit))
		return 0, false
	}
	return n, true
}

// shortText returns at most n runes of text, ellipsized when it is cut.
func shortText(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	if n <= 1 {
		return string(r[:n])
	}
	return strings.TrimRight(string(r[:n-1]), " \t") + "…"
}
