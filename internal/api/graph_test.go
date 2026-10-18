package api

// The dialogue-graph reader (docs/spec/09 "/games/:id/graph"): server-side
// filters, keyset pagination against the 2 000-node bound, the edges-between-
// visible-nodes rule, and the verified-vs-inferred label the page renders.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// graphDraft imports the synthetic CSV (six lines, two named speakers, one
// area) and adds two store-level edges so the page has something to draw:
// the first line continues into the second, and the second loops back (a
// repeat — the NPC says it again).
func graphDraft(t *testing.T, e *catalogEnv) (profileID string, msgs []games.Message, edges []games.MessageEdge) {
	t.Helper()
	_, _, profileID, _ = newDraft(t, e, "graph", "graph")
	var err error
	msgs, _, err = e.st.ListMessages(e.ctx, profileID, store.MessageFilter{}, "", 100)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("the fixture import left %d messages, want at least two", len(msgs))
	}
	edges = []games.MessageEdge{
		{FromID: msgs[0].ID, ToID: msgs[1].ID, EdgeType: games.EdgeNext, Confidence: 1, CreatedAt: nowTestStamp(t)},
		{FromID: msgs[1].ID, ToID: msgs[0].ID, EdgeType: games.EdgeRepeat, Confidence: 0.5, CreatedAt: nowTestStamp(t)},
	}
	if err := e.st.PutEdges(e.ctx, profileID, edges); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}
	return profileID, msgs, edges
}

// nowTestStamp is a valid domain timestamp (RFC 3339 UTC) for rows a test
// seeds through the store.
func nowTestStamp(t *testing.T) string {
	t.Helper()
	return time.Now().UTC().Format(time.RFC3339)
}

// decodeGraph is the wire shape the assertions read.
type graphDoc struct {
	Nodes []struct {
		ID                    string  `json:"id"`
		Kind                  string  `json:"kind"`
		Text                  string  `json:"text"`
		AttributionStatus     string  `json:"attribution_status"`
		AttributionConfidence float64 `json:"attribution_confidence"`
		Speaker               *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"speaker"`
		Scene *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"scene"`
	} `json:"nodes"`
	Edges []struct {
		From       string  `json:"from"`
		To         string  `json:"to"`
		Kind       string  `json:"kind"`
		Confidence float64 `json:"confidence"`
		Source     *string `json:"source"`
		Evidence   int     `json:"evidence"`
	} `json:"edges"`
	NextCursor string `json:"next_cursor"`
	Total      int    `json:"total"`
}

func (e *catalogEnv) graph(t *testing.T, role auth.Role, target string) graphDoc {
	t.Helper()
	rec := e.get(role, target)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET %s = %d, want 200 (body %s)", target, rec.Code, rec.Body.String())
	}
	var doc graphDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		e.t.Fatalf("decode graph: %v (%s)", err, rec.Body.String())
	}
	return doc
}

func TestGraphPageCarriesNodesEdgesAndLabels(t *testing.T) {
	e := newCatalogEnv(t)
	profileID, msgs, edges := graphDraft(t, e)

	doc := e.graph(t, auth.RoleViewer, "/api/v1/profiles/"+profileID+"/graph?limit=100")
	if doc.Total != len(msgs) {
		t.Errorf("total = %d, want %d (the revision's message count)", doc.Total, len(msgs))
	}
	if len(doc.Nodes) != len(msgs) {
		t.Fatalf("nodes = %d, want %d", len(doc.Nodes), len(msgs))
	}
	// The short text is the normalized text cut to 80 runes, not the raw line.
	attributed := map[string]bool{}
	for _, m := range msgs {
		if m.SpeakerID != "" {
			attributed[m.ID] = true
		}
	}
	for _, n := range doc.Nodes {
		if len([]rune(n.Text)) > 80 {
			t.Errorf("node %s text is %d runes, want ≤ 80", n.ID, len([]rune(n.Text)))
		}
	}
	for _, n := range doc.Nodes {
		if attributed[n.ID] && (n.Speaker == nil || n.Speaker.ID == "") {
			t.Errorf("node %s is attributed in the store but the page shows no speaker", n.ID)
		}
	}
	byID := map[string]bool{}
	for _, n := range doc.Nodes {
		if byID[n.ID] {
			t.Errorf("the page repeated node %s", n.ID)
		}
		byID[n.ID] = true
	}
	// Both store edges are visible: both endpoints of each edge are on the page.
	kinds := map[string]int{}
	for _, ed := range doc.Edges {
		kinds[ed.Kind]++
	}
	if kinds["next"] != 1 || kinds["repeat"] != 1 {
		t.Errorf("edge kinds = %v, want one next and one repeat from the store edges", kinds)
	}
	for _, ed := range doc.Edges {
		if ed.From == edges[0].FromID && ed.To == edges[0].ToID && ed.Confidence != 1 {
			t.Errorf("edge %s→%s confidence = %v, want 1", ed.From, ed.To, ed.Confidence)
		}
	}
}

func TestGraphFiltersAreServerSide(t *testing.T) {
	e := newCatalogEnv(t)
	profileID, msgs, _ := graphDraft(t, e)

	// kind: the fixture mixes dialogue, sign and item lines.
	doc := e.graph(t, auth.RoleViewer, "/api/v1/profiles/"+profileID+"/graph?kind=dialogue&limit=100")
	for _, n := range doc.Nodes {
		if n.Kind != "dialogue" {
			t.Fatalf("kind=dialogue returned a %s node", n.Kind)
		}
	}
	want := 0
	for _, m := range msgs {
		if m.TextKind == games.TextDialogue {
			want++
		}
	}
	if doc.Total != want || len(doc.Nodes) != want {
		t.Errorf("kind=dialogue: total=%d nodes=%d, want %d", doc.Total, len(doc.Nodes), want)
	}

	// kind that is not a vocabulary: 400 naming the rule, not an empty page.
	rec := e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/graph?kind=narration")
	requireProblem(t, rec, http.StatusBadRequest, codeInvalidParameter)

	// speaker: an unknown id is a 400; a real character id filters.
	rec = e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/graph?speaker=not-a-ulid")
	requireProblem(t, rec, http.StatusBadRequest, codeInvalidParameter)

	speaker := ""
	for _, m := range msgs {
		if m.SpeakerID != "" {
			speaker = m.SpeakerID
			break
		}
	}
	if speaker == "" {
		t.Skip("the fixture left no attributed line to filter on")
	}
	doc = e.graph(t, auth.RoleViewer, "/api/v1/profiles/"+profileID+"/graph?speaker="+speaker+"&limit=100")
	if len(doc.Nodes) == 0 {
		t.Fatalf("speaker=%s filtered out every line", speaker)
	}
	for _, n := range doc.Nodes {
		if n.Speaker == nil || n.Speaker.ID != speaker {
			t.Fatalf("speaker=%s returned a node with speaker %v", speaker, n.Speaker)
		}
	}
}

func TestGraphPaginationIsKeysetAndBounded(t *testing.T) {
	e := newCatalogEnv(t)
	profileID, msgs, _ := graphDraft(t, e)

	rec := e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/graph?limit=2001")
	requireProblem(t, rec, http.StatusBadRequest, codeInvalidParameter)

	page1 := e.graph(t, auth.RoleViewer, "/api/v1/profiles/"+profileID+"/graph?limit=2")
	if len(page1.Nodes) != 2 {
		t.Fatalf("page1 = %d nodes, want 2", len(page1.Nodes))
	}
	if page1.NextCursor == "" {
		t.Fatalf("page1 carried no next_cursor with %d more rows behind it", len(msgs)-2)
	}
	page2 := e.graph(t, auth.RoleViewer, "/api/v1/profiles/"+profileID+"/graph?limit=2&cursor="+page1.NextCursor)
	if len(page2.Nodes) == 0 {
		t.Fatalf("page2 is empty: the cursor did not resume")
	}
	seen := map[string]bool{}
	for _, n := range page1.Nodes {
		seen[n.ID] = true
	}
	for _, n := range page2.Nodes {
		if seen[n.ID] {
			t.Fatalf("page2 repeated node %s: a keyset cursor must not re-emit", n.ID)
		}
	}

	// A garbage cursor is a 400, not an empty page.
	rec = e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/graph?cursor=!!")
	requireProblem(t, rec, http.StatusBadRequest, codeInvalidParameter)
}

func TestGraphPageETagMatchesTheProfile(t *testing.T) {
	e := newCatalogEnv(t)
	profileID, _, _ := graphDraft(t, e)
	rec := e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/graph?limit=10")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET graph = %d", rec.Code)
	}
	profileRec := e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID)
	if rec.Header().Get("ETag") != profileRec.Header().Get("ETag") {
		t.Errorf("graph ETag %q differs from the profile's %q: the page and the revision must agree on the token",
			rec.Header().Get("ETag"), profileRec.Header().Get("ETag"))
	}
}
