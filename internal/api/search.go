package api

// The ⌘K command-palette search (ADR-0098, docs/design/plum-peach UX-02): one
// debounced endpoint that spans games, characters, dialogue lines, traces,
// settings sections and actions. The route itself is viewer — the palette
// sits on every screen, including the kiosk viewer's, like the status bar —
// and the response narrows per result: trace rows come out only for
// developer and above (traces are the developer-evidence tier, docs/spec/08),
// a settings section appears only for a role that may open the page it links
// to (admin, like GET /api/v1/settings), and an action verb appears only for
// a role that satisfies the verb's own role. That is the scope table's rule
// applied to results instead of routes.
//
// The response also carries agent_prompt, the normalised query: UX-02's Tab
// key hands the palette's query to the agent dock, and the UI takes it from
// the response rather than re-deriving it.
//
// Routes in the results follow the plum-peach IA (ADR-0093): the Library
// focuses a game, Cast lives at /games/:id/cast, Traces at /traces, Health at
// /health — not the pre-redesign paths, because the palette ships with the
// redesign, not before it.

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/store"
)

// The palette's canonical group order (UX-02 lists the categories in this
// order); groups serialise in this order and an empty group is omitted.
var kindOrder = []string{"game", "character", "line", "trace", "setting", "action"}

// searchLimits bounds one group. The palette shows at most one screen of any
// one kind, so the ceiling is deliberately tighter than the message-search
// ceiling (store.MaxSearchLimit) a full search page allows.
const (
	searchDefaultLimit = 5
	searchMaxLimit     = 20
)

// recencyWindow is the horizon over which the recency boost (ADR-0098) decays
// to zero: an item older than this gets no boost at all. The boost stays
// below 1.0 — the gap between two match classes is exactly 1.0 — so recency
// can reorder within a class, never across one.
const recencyWindow = 90 * 24 * time.Hour

// searchAction is the "call the API on ↵" half of an action's target.
type searchAction struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// searchResult is one palette row. Exactly one of Route and Action is set:
// either the row navigates (route) or it executes (action).
type searchResult struct {
	Kind     string        `json:"kind"`
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Subtitle string        `json:"subtitle,omitempty"`
	Route    string        `json:"route,omitempty"`
	Action   *searchAction `json:"action,omitempty"`
	Score    float64       `json:"score"`
}

type searchGroup struct {
	Kind  string         `json:"kind"`
	Items []searchResult `json:"items"`
}

// searchResponse is the GET /api/v1/search body. Query and AgentPrompt are
// always the same string after normalisation; the duplication is deliberate
// (the Tab handoff reads agent_prompt, the list header reads query).
type searchResponse struct {
	Query       string        `json:"query"`
	AgentPrompt string        `json:"agent_prompt"`
	Groups      []searchGroup `json:"groups"`
}

// settingsSection is one row of the static /settings registry (ADR-0098):
// one section the /settings page renders, the anchor it lives at, and the
// role that may open the page — the same role GET /api/v1/settings grants.
type settingsSection struct {
	id       string // the /settings anchor the row links to
	title    string
	subtitle string
	keywords []string
	role     auth.Role
}

// The registry mirrors settingsResponse's sections in internal/api/settings.go
// (models, storage, audio, security, imports, update). It is static on
// purpose: the palette's job is to find the section, and the section's live
// content belongs to the admin-gated settings API, not to a viewer-readable
// search body.
var settingsSections = []settingsSection{
	{
		id:       "models",
		title:    "Model roles and gateway",
		subtitle: "provider endpoints by role; configured values stay server-side",
		keywords: []string{"model", "role", "gateway", "provider", "endpoint", "llm", "decision", "tts", "asr"},
		role:     auth.RoleAdmin,
	},
	{
		id:       "storage",
		title:    "Storage",
		subtitle: "data directory, database and asset root",
		keywords: []string{"storage", "database", "disk", "space", "asset", "directory", "sqlite"},
		role:     auth.RoleAdmin,
	},
	{
		id:       "audio",
		title:    "Audio output",
		subtitle: "device, sink kind and master volume",
		keywords: []string{"audio", "output", "volume", "speaker", "sink", "bluetooth", "tone"},
		role:     auth.RoleAdmin,
	},
	{
		id:       "security",
		title:    "Security",
		subtitle: "authentication, roles and API tokens",
		keywords: []string{"security", "auth", "token", "password", "session", "csrf"},
		role:     auth.RoleAdmin,
	},
	{
		id:       "imports",
		title:    "Import policy",
		subtitle: "rights, sources and disc images",
		keywords: []string{"import", "rights", "source", "disc", "image", "policy", "manifest"},
		role:     auth.RoleAdmin,
	},
	{
		id:       "update",
		title:    "Version and updates",
		subtitle: "build information and update notifications",
		keywords: []string{"version", "update", "upgrade", "release", "notification"},
		role:     auth.RoleAdmin,
	},
}

// actionVerb is one row of the typed verb registry (ADR-0098): a verb with a
// target — an SPA route or an API call — and the role it needs. Dynamic verbs
// (dynamic != "") expand over the game/character matches the same query
// already fetched for the palette's groups: the verb row for a game is built
// from the game group's hit, so no extra queries run.
type actionVerb struct {
	id      string
	label   string // the verb text matched against the query, e.g. "start narrating"
	role    auth.Role
	route   string // the SPA route template ({id} fills the entity's id)
	api     *searchAction
	dynamic string // "" (static), "game" or "character"
	// titleOf builds the row's title for a dynamic verb ("start narrating
	// Wind Waker (synthetic)"); nil for static verbs, whose title is label.
	titleOf func(entity string) string
}

var actionVerbs = []actionVerb{
	{
		id:      "start-narrating",
		label:   "start narrating",
		role:    auth.RoleOperator,
		route:   "/play?game={id}",
		dynamic: "game",
		titleOf: func(g string) string { return "start narrating " + g },
	},
	{
		id:      "regenerate-voice",
		label:   "regenerate voice for",
		role:    auth.RoleCurator,
		api:     &searchAction{Method: http.MethodPost, Path: "/api/v1/voices/auditions"},
		dynamic: "character",
		titleOf: func(c string) string { return "regenerate " + c + "'s voice" },
	},
	{
		id:    "open-health",
		label: "open health",
		role:  auth.RoleViewer,
		route: "/health",
	},
	{
		id:    "open-diagnostics",
		label: "open diagnostics",
		role:  auth.RoleViewer,
		route: "/health?section=diagnostics",
	},
	{
		id:    "open-settings",
		label: "open settings",
		role:  auth.RoleAdmin,
		route: "/settings",
	},
	{
		id:    "open-tests",
		label: "open tests",
		role:  auth.RoleDeveloper,
		route: "/tests",
	},
}

// handleSearch answers GET /api/v1/search?q=&kinds=&limit= (viewer).
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	ctx := r.Context()
	query := normalizeQuery(r.URL.Query().Get("q"))

	kinds, ok := s.searchKinds(w, r)
	if !ok {
		return
	}
	limit := s.searchLimit(w, r)
	if limit == 0 {
		return
	}

	role := auth.RoleNone
	if id := IdentityFromContext(ctx); id != nil {
		role = id.Role
	}
	now := time.Now().UTC()

	groups := map[string][]searchResult{}
	if kinds["game"] {
		groups["game"] = s.searchGames(w, r, ctx, query, limit, now)
	}
	if kinds["character"] {
		groups["character"] = s.searchCharacters(w, r, ctx, query, limit, now)
	}
	if kinds["line"] {
		groups["line"] = s.searchLines(w, r, ctx, query, limit, now)
	}
	// Traces are the developer-evidence tier (docs/spec/08): a developer-only
	// result kind on a viewer route, the same way the /api/v1/dev prefix is a
	// developer-only surface. A lower role simply does not get the group.
	if kinds["trace"] && role.AtLeast(auth.RoleDeveloper) {
		groups["trace"] = s.searchTraces(ctx, query, limit)
	}
	if kinds["setting"] {
		groups["setting"] = searchSettings(query, limit, role)
	}
	if kinds["action"] {
		groups["action"] = s.searchActions(w, r, ctx, query, limit, role, groups["game"], groups["character"])
	}

	out := searchResponse{
		Query:       query,
		AgentPrompt: query,
		Groups:      []searchGroup{},
	}
	for _, k := range kindOrder {
		items := groups[k]
		if len(items) == 0 {
			continue
		}
		out.Groups = append(out.Groups, searchGroup{Kind: k, Items: items})
	}
	writeJSON(w, http.StatusOK, out)
}

// normalizeQuery is the palette's query normalisation: trimmed, internal
// whitespace collapsed to single spaces. It is applied before any matching,
// so the echoed agent_prompt and the match use one spelling.
func normalizeQuery(q string) string {
	return strings.Join(strings.Fields(q), " ")
}

// searchKinds parses the kinds= list: a comma-separated subset of the
// palette's kinds, defaulting to all of them. Unknown kinds are a 400 — a
// silently-ignored kind would be a client bug the response could not show.
func (s *Server) searchKinds(w http.ResponseWriter, r *http.Request) (map[string]bool, bool) {
	want := map[string]bool{}
	for _, k := range kindOrder {
		want[k] = true
	}
	raw := strings.TrimSpace(r.URL.Query().Get("kinds"))
	if raw == "" {
		return want, true
	}
	for _, part := range strings.Split(raw, ",") {
		k := strings.ToLower(strings.TrimSpace(part))
		if !want[k] {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-kind", codeInvalidParameter,
				"Bad Request", "kinds must be a comma-separated subset of game, character, line, trace, setting and action")
			return nil, false
		}
		want[k] = true
	}
	return want, true
}

// searchLimit parses the per-kind limit: default searchDefaultLimit, ceiling
// searchMaxLimit. It is its own parse rather than limitParam because the
// palette's default (5) is not the list default (defaultListLimit).
func (s *Server) searchLimit(w http.ResponseWriter, r *http.Request) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return searchDefaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > searchMaxLimit {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-limit", codeInvalidParameter,
			"Bad Request", "limit must be an integer between 1 and "+strconv.Itoa(searchMaxLimit))
		return 0
	}
	return n
}

// ── the groups ──────────────────────────────────────────────────────────────

// recencyBoost is the ADR-0098 boost: 1.0 for a just-created item, decaying
// linearly to 0 at recencyWindow. A malformed or future timestamp is treated
// as new rather than an error: a search that 500s because one row's clock
// skewed is worse than one that ranks that row first.
func recencyBoost(at string, now time.Time) float64 {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return 1
	}
	age := now.Sub(t)
	if age < 0 {
		age = 0
	}
	if age >= recencyWindow {
		return 0
	}
	return 1 - float64(age)/float64(recencyWindow)
}

// searchGames ranks a query's games by match class then recency. An empty
// query is the palette's recents mode: the most recently added games (id
// order is creation order, ADR-0080's monotonic ULIDs).
func (s *Server) searchGames(w http.ResponseWriter, r *http.Request, ctx context.Context, query string, limit int, now time.Time) []searchResult {
	if query == "" {
		games, _, err := s.store.ListGames(ctx, "", limit)
		if err != nil {
			s.storeProblem(w, r, err, "list games", "game")
			return nil
		}
		out := make([]searchResult, 0, len(games))
		for i := len(games) - 1; i >= 0; i-- {
			g := games[i]
			out = append(out, searchResult{
				Kind:     "game",
				ID:       g.ID,
				Title:    g.Title,
				Subtitle: g.Platform,
				Route:    "/library?game=" + g.ID,
				Score:    recencyBoost(g.CreatedAt, now),
			})
		}
		return out
	}
	hits, err := s.store.FindGamesGlobal(ctx, query, limit)
	if err != nil {
		s.storeProblem(w, r, err, "search games", "game")
		return nil
	}
	out := make([]searchResult, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchResult{
			Kind:     "game",
			ID:       h.Game.ID,
			Title:    h.Game.Title,
			Subtitle: h.Game.Platform,
			Route:    "/library?game=" + h.Game.ID,
			Score:    float64(h.MatchClass) + recencyBoost(h.Game.CreatedAt, now),
		})
	}
	return out
}

// searchCharacters ranks a query's characters (latest revision of each
// edition) the same way searchGames does; a row links to the Cast page of its
// game (ADR-0093).
func (s *Server) searchCharacters(w http.ResponseWriter, r *http.Request, ctx context.Context, query string, limit int, now time.Time) []searchResult {
	if query == "" {
		hits, err := s.store.RecentCharactersGlobal(ctx, limit)
		if err != nil {
			s.storeProblem(w, r, err, "recent characters", "character")
			return nil
		}
		return characterResults(hits, now)
	}
	hits, err := s.store.FindCharactersGlobal(ctx, query, limit)
	if err != nil {
		s.storeProblem(w, r, err, "search characters", "character")
		return nil
	}
	out := characterResults(hits, now)
	for i := range out {
		out[i].Score = float64(hits[i].MatchClass) + out[i].Score
	}
	return out
}

// characterResults maps the store's character hits onto palette rows. The
// score slot holds the recency boost; the caller adds the match class.
func characterResults(hits []store.CharacterSearchHit, now time.Time) []searchResult {
	out := make([]searchResult, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchResult{
			Kind:     "character",
			ID:       h.Character.ID,
			Title:    h.Character.Name,
			Subtitle: h.GameTitle,
			Route:    "/games/" + h.GameID + "/cast",
			Score:    recencyBoost(h.Character.CreatedAt, now),
		})
	}
	return out
}

// searchLines ranks a query's dialogue lines by the trigram index's bm25
// (higher-is-better direction: the store passes bm25 through, lower is
// better, so the handler negates it) plus the recency boost. An empty query
// is the recents mode.
func (s *Server) searchLines(w http.ResponseWriter, r *http.Request, ctx context.Context, query string, limit int, now time.Time) []searchResult {
	var (
		hits []store.LineSearchHit
		err  error
	)
	if query == "" {
		hits, err = s.store.RecentLinesGlobal(ctx, limit)
	} else {
		hits, err = s.store.FindLinesGlobal(ctx, query, limit)
	}
	if err != nil {
		s.storeProblem(w, r, err, "search lines", "line")
		return nil
	}
	out := make([]searchResult, 0, len(hits))
	for _, h := range hits {
		subject := strings.TrimSpace(h.Message.NormalizedText)
		if runes := len([]rune(subject)); runes > 80 {
			subject = string([]rune(subject)[:80]) + "…"
		}
		out = append(out, searchResult{
			Kind:     "line",
			ID:       h.Message.ID,
			Title:    subject,
			Subtitle: h.GameTitle,
			Route:    "/traces?line=" + h.Message.ID,
			Score:    -h.BM25 + recencyBoost(h.Message.CreatedAt, now),
		})
	}
	return out
}

// searchTraces scans the durable event log the way handleDevTraces does —
// bounded pages, the developer's evidence — and matches a query against the
// trace id (exact, then prefix), the event name and the payload bytes (the
// "line text or speaker" case, case-insensitively, without parsing every
// event shape). One row per trace id: the best match for that id wins, and
// the recency boost comes from the match's seq, so the newest trace leads.
func (s *Server) searchTraces(ctx context.Context, query string, limit int) []searchResult {
	if s.devEvents == nil {
		return nil // no event log wired: the group is absent, like an unwired surface
	}

	const scanPages = 50 // the handleDevTraces bound: a trace that spans more is broken, not slow
	const pageSize = 1000

	type best struct {
		cls int
		seq int64
	}
	pick := map[string]best{}
	seq := int64(0)
	maxSeq := int64(0)
	q := strings.ToLower(query)

	for i := 0; i < scanPages; i++ {
		page, err := s.devEvents.After(ctx, seq, events.Filter{}, pageSize)
		if err != nil {
			return nil // an unreadable log degrades to an empty group, not a 500 on a search
		}
		for _, e := range page {
			if e.Seq > maxSeq {
				maxSeq = e.Seq
			}
			if e.TraceID == "" {
				continue
			}
			cls := traceMatchClass(q, e)
			if cls == 0 {
				continue
			}
			cur, ok := pick[e.TraceID]
			if !ok || cls > cur.cls || (cls == cur.cls && e.Seq > cur.seq) {
				pick[e.TraceID] = best{cls: cls, seq: e.Seq}
			}
		}
		if len(page) < pageSize {
			break
		}
		seq = page[len(page)-1].Seq
	}

	out := make([]searchResult, 0, len(pick))
	for id, b := range pick {
		var boost float64
		if maxSeq > 0 {
			boost = 1 - float64(maxSeq-b.seq)/float64(maxSeq)
		}
		out = append(out, searchResult{
			Kind:     "trace",
			ID:       id,
			Title:    id,
			Subtitle: "trace",
			Route:    "/traces?trace=" + id,
			Score:    float64(b.cls) + boost,
		})
	}
	sortResults(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// traceMatchClass scores one envelope against the lower-cased query: 3 for an
// exact trace id, 2 for a prefix, 1 when the query occurs in the event name
// or the payload bytes (the "line text or speaker" case), 0 otherwise. An
// empty query scores every trace 1 — the recents mode, where seq alone then
// orders the group.
func traceMatchClass(query string, e events.Envelope) int {
	if query == "" {
		return 1
	}
	id := strings.ToLower(e.TraceID)
	if id == query {
		return 3
	}
	if strings.HasPrefix(id, query) {
		return 2
	}
	if strings.Contains(strings.ToLower(e.Name), query) {
		return 1
	}
	if len(e.Data) > 0 && strings.Contains(strings.ToLower(string(e.Data)), query) {
		return 1
	}
	return 0
}

// sortResults orders a group by score, then id: a deterministic order the
// UI can rely on for keyboard navigation.
func sortResults(out []searchResult) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
}

// searchSettings matches the query against the static /settings registry. A
// section is offered only to a role that may open the page it links to.
// Exact title match beats a prefix, which beats a keyword or subtitle hit;
// an empty query offers the whole registry (the palette's recents mode for a
// static list is the list itself).
func searchSettings(query string, limit int, role auth.Role) []searchResult {
	term := strings.ToLower(query)
	out := []searchResult{}
	for _, sec := range settingsSections {
		if !role.AtLeast(sec.role) {
			continue
		}
		cls := 0
		if term != "" {
			t := strings.ToLower(sec.title)
			switch {
			case t == term:
				cls = store.MatchExact
			case strings.HasPrefix(t, term):
				cls = store.MatchPrefix
			case strings.Contains(t, term) || containsKeyword(sec, term):
				cls = store.MatchContains
			}
			if cls == 0 {
				continue
			}
		}
		out = append(out, searchResult{
			Kind:     "setting",
			ID:       sec.id,
			Title:    sec.title,
			Subtitle: sec.subtitle,
			Route:    "/settings#" + sec.id,
			Score:    float64(cls),
		})
	}
	sortResults(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// containsKeyword reports whether the query (or a word of it) occurs in one
// of the section's keywords or its subtitle.
func containsKeyword(sec settingsSection, term string) bool {
	if strings.Contains(strings.ToLower(sec.subtitle), term) {
		return true
	}
	for _, kw := range sec.keywords {
		if strings.Contains(kw, term) {
			return true
		}
	}
	return false
}

// searchActions builds the palette's action group. A static verb matches the
// query against the verb text ("open health" shows for "health" and for
// "open health"). A dynamic verb has two ways in: the query matched an
// entity (the game/character group already fetched for the same query), or
// the query starts with the verb's label ("start narrating zelda"), in which
// case the entity is looked up on the query's tail — the one case where the
// group runs an extra query, because the verb prefix means the caller typed
// the action, not the entity. Each verb is offered only to a role that
// satisfies it; an empty query offers every allowed verb over its recents.
func (s *Server) searchActions(w http.ResponseWriter, r *http.Request, ctx context.Context, query string, limit int, role auth.Role, gameHits, characterHits []searchResult) []searchResult {
	out := []searchResult{}
	for _, v := range actionVerbs {
		if !role.AtLeast(v.role) {
			continue
		}
		switch v.dynamic {
		case "game", "character":
			group := gameHits
			if v.dynamic == "character" {
				group = characterHits
			}
			if len(group) > 0 {
				for _, e := range group {
					out = append(out, verbResult(v, e.ID, e.Title))
				}
			}
			// The verb-prefix form: "start narrating zelda" (or just
			// "start narrating", whose tail is the recents of the entity
			// kind). It runs only when the entity group came back empty —
			// a title that contains the whole query already produced the
			// verb rows above, and a bare query (including the empty one)
			// filled the group from the recents.
			if len(group) == 0 && strings.HasPrefix(strings.ToLower(query), strings.ToLower(v.label)) {
				tail := strings.TrimSpace(strings.TrimPrefix(query, v.label))
				var found []searchResult
				switch v.dynamic {
				case "game":
					found = s.verbEntityGames(w, r, ctx, tail, limit)
				default:
					found = s.verbEntityCharacters(w, r, ctx, tail, limit)
				}
				for _, e := range found {
					if !hasResultID(out, v.id+":"+e.ID) {
						out = append(out, verbResult(v, e.ID, e.Title))
					}
				}
			}
		default:
			if query == "" || strings.HasPrefix(strings.ToLower(query), strings.ToLower(v.label)) ||
				strings.Contains(strings.ToLower(v.label), strings.ToLower(query)) {
				out = append(out, verbResult(v, v.id, v.label))
			}
		}
	}
	sortResults(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// verbEntityGames/verbEntityCharacters fetch the entity rows a verb-prefix
// query expands over: the tail of the query when one was typed, the recents
// when the bare verb was.
func (s *Server) verbEntityGames(w http.ResponseWriter, r *http.Request, ctx context.Context, tail string, limit int) []searchResult {
	if tail == "" {
		games, _, err := s.store.ListGames(ctx, "", limit)
		if err != nil {
			s.storeProblem(w, r, err, "list games", "game")
			return nil
		}
		out := make([]searchResult, 0, len(games))
		for i := len(games) - 1; i >= 0; i-- {
			out = append(out, searchResult{ID: games[i].ID, Title: games[i].Title})
		}
		return out
	}
	hits, err := s.store.FindGamesGlobal(ctx, tail, limit)
	if err != nil {
		s.storeProblem(w, r, err, "search games", "game")
		return nil
	}
	out := make([]searchResult, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchResult{ID: h.Game.ID, Title: h.Game.Title})
	}
	return out
}

func (s *Server) verbEntityCharacters(w http.ResponseWriter, r *http.Request, ctx context.Context, tail string, limit int) []searchResult {
	var (
		h   []store.CharacterSearchHit
		err error
	)
	if tail == "" {
		h, err = s.store.RecentCharactersGlobal(ctx, limit)
	} else {
		h, err = s.store.FindCharactersGlobal(ctx, tail, limit)
	}
	if err != nil {
		s.storeProblem(w, r, err, "search characters", "character")
		return nil
	}
	out := make([]searchResult, 0, len(h))
	for _, c := range h {
		out = append(out, searchResult{ID: c.Character.ID, Title: c.Character.Name})
	}
	return out
}

// hasResultID reports whether a row with that id is already in the group:
// the two ways in above can meet (a query that is both a verb prefix and an
// entity match), and a duplicate row would read as a bug in the palette.
func hasResultID(out []searchResult, id string) bool {
	for _, r := range out {
		if r.ID == id {
			return true
		}
	}
	return false
}

// verbResult builds one action row for a verb and (for dynamic verbs) its
// entity. A dynamic row's id is "verb:entity" so two verbs over one entity
// and one verb over two entities stay distinct; a static verb's id is just
// the verb id.
func verbResult(v actionVerb, entityID, entityTitle string) searchResult {
	title := v.label
	if v.titleOf != nil {
		title = v.titleOf(entityTitle)
	}
	id := v.id
	if v.dynamic != "" {
		id = v.id + ":" + entityID
	}
	res := searchResult{
		Kind:  "action",
		ID:    id,
		Title: title,
	}
	if v.route != "" {
		res.Route = strings.Replace(v.route, "{id}", entityID, 1)
		res.Subtitle = "open " + v.label
	} else if v.api != nil {
		res.Action = &searchAction{Method: v.api.Method, Path: v.api.Path}
		res.Subtitle = "run " + v.api.Method + " " + v.api.Path
	}
	res.Score = 2 // verbs sit above keyword matches: they are things to do, not things to read
	return res
}
