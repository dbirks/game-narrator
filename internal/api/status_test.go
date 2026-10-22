package api

// The global status bar's backend (docs/design/plum-peach "Global chrome"):
// the string rendering per state (the bar is a server-rendered string, so
// these are the contract), the aggregation (session, setup runs, the
// cached-probe health rollup), and the SSE stream with Last-Event-ID resume
// and the name filter. The GET also records its success body for the
// OpenAPI contract gate.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/agent/tools"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/pipeline"
	rt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
)

// ── the string, per state ─────────────────────────────────────────────────

func TestStatusStringPerState(t *testing.T) {
	health := func(levels ...string) StatusHealth {
		h := StatusHealth{Components: make([]StatusHealthComponent, 0, len(statusComponents))}
		for i, comp := range statusComponents {
			c := StatusHealthComponent{ID: comp, Level: levels[i], Detail: "detail"}
			h.Components = append(h.Components, c)
			switch c.Level {
			case statusLevelOK:
				h.OK++
			case statusLevelSlow:
				h.Slow++
			case statusLevelFail:
				h.Fail++
			case statusLevelUnknown:
				h.Unknown++
			}
		}
		h.Detail = renderHealthRollup(h)
		return h
	}
	allOK := health(statusLevelOK, statusLevelOK, statusLevelOK, statusLevelOK, statusLevelOK, statusLevelOK)

	cases := []struct {
		name string
		snap StatusSnapshot
		want string
	}{
		{
			name: "the design's live string",
			snap: StatusSnapshot{
				Session: &StatusSession{SessionID: "s1", Mode: "conservative", State: "running", Source: "v4l2", CaptureFPS: 30, FrameToVoiceP50MS: 412},
				Health:  allOK,
			},
			want: "● live · capture 30fps · 412ms frame→voice",
		},
		{
			name: "lines spoken append when they exist",
			snap: StatusSnapshot{
				Session: &StatusSession{SessionID: "s1", Mode: "conservative", State: "running", CaptureFPS: 30, FrameToVoiceP50MS: 412, LinesSpoken: 7},
				Health:  allOK,
			},
			want: "● live · capture 30fps · 412ms frame→voice · 7 lines",
		},
		{
			name: "a paused session names the pause",
			snap: StatusSnapshot{
				Session: &StatusSession{SessionID: "s1", State: "running", Paused: true, CaptureFPS: 30},
				Health:  allOK,
			},
			want: "● paused · capture 30fps",
		},
		{
			name: "a starting session shows its target rate, not zero",
			snap: StatusSnapshot{
				Session: &StatusSession{SessionID: "s1", State: "starting", CaptureFPS: 0, CaptureFPSTarget: 30},
				Health:  allOK,
			},
			want: "● starting · capture 30fps",
		},
		{
			name: "the design's setup string",
			snap: StatusSnapshot{
				Setup:  []StatusSetupRun{{RunID: "r1", State: "running", Voiced: 9, Total: 12, Elapsed: int64((18*time.Minute + 4*time.Second).Milliseconds())}},
				Health: allOK,
			},
			want: "● agent running · 9 of 12 voiced · 18m 04s",
		},
		{
			name: "a review run names the wait",
			snap: StatusSnapshot{
				Setup:  []StatusSetupRun{{RunID: "r1", State: agentStateReview, Voiced: 9, Total: 12, Elapsed: 42000}},
				Health: allOK,
			},
			want: "● agent needs review · 9 of 12 voiced · 42s",
		},
		{
			// The stale-clock lie the superseded-run filter fixes on the
			// data side (ADR-0104 amendment): a blocked run is waiting on a
			// human, not working, so its age never renders as a live timer.
			name: "a blocked run carries no live clock",
			snap: StatusSnapshot{
				Setup:  []StatusSetupRun{{RunID: "r1", State: "blocked", Voiced: 10, Total: 45, Elapsed: 77280000}},
				Health: allOK,
			},
			want: "● agent blocked · 10 of 45 voiced",
		},
		{
			// A pre-profile blocked run with no counts: the word alone, the
			// 21-hour age nowhere in the string.
			name: "a blocked pre-profile run is the word alone",
			snap: StatusSnapshot{
				Setup:  []StatusSetupRun{{RunID: "r1", State: "blocked", Elapsed: 77280000}},
				Health: allOK,
			},
			want: "● agent blocked",
		},
		{
			// The clock is a running run's: the same age renders while the
			// run works.
			name: "a running run keeps its clock at the same age",
			snap: StatusSnapshot{
				Setup:  []StatusSetupRun{{RunID: "r1", State: "running", Voiced: 10, Total: 45, Elapsed: 77280000}},
				Health: allOK,
			},
			want: "● agent running · 10 of 45 voiced · 21h 28m",
		},
		{
			name: "idle is the rollup",
			snap: StatusSnapshot{Health: health(statusLevelOK, statusLevelSlow, statusLevelOK, statusLevelOK, statusLevelOK, statusLevelOK)},
			want: "5 ok · 1 slow · ocr slow",
		},
		{
			name: "a clean box still says a number",
			snap: StatusSnapshot{Health: allOK},
			want: "6 ok",
		},
		{
			name: "a non-ok tile appends to the live string",
			snap: StatusSnapshot{
				Session: &StatusSession{SessionID: "s1", State: "running", CaptureFPS: 30, FrameToVoiceP50MS: 412},
				Health:  health(statusLevelOK, statusLevelOK, statusLevelOK, statusLevelFail, statusLevelOK, statusLevelOK),
			},
			want: "● live · capture 30fps · 412ms frame→voice · tts fail",
		},
		{
			name: "the session beats the setup run",
			snap: StatusSnapshot{
				Session: &StatusSession{SessionID: "s1", State: "running", CaptureFPS: 30},
				Setup:   []StatusSetupRun{{RunID: "r1", State: "running", Elapsed: 42000}},
				Health:  allOK,
			},
			want: "● live · capture 30fps",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderStatus(tc.snap); got != tc.want {
				t.Fatalf("RenderStatus = %q, want %q", got, tc.want)
			}
			if tc.snap.Status != "" {
				t.Fatalf("fixture: do not pre-fill Status; the renderer owns it")
			}
		})
	}
}

func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{42 * time.Second, "42s"},
		{18*time.Minute + 4*time.Second, "18m 04s"},
		{time.Hour + 2*time.Minute, "1h 02m"},
		{49 * time.Hour, "2d 1h"},
		{-time.Second, "0s"},
	}
	for _, tc := range cases {
		if got := formatElapsed(tc.d); got != tc.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestSetupRunStale pins each clause of the superseded-run filter (ADR-0104
// amendment) against the publication facts the bar reads: a run whose
// revision the edition retired must not drive the status line, and a run on
// the edition's live work must stay, whatever its age.
func TestSetupRunStale(t *testing.T) {
	created := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	published := created.Add(2 * time.Hour)

	cases := []struct {
		name      string
		createdAt time.Time
		facts     store.AgentRunProfileFacts
		wantStale bool
	}{
		{
			name:      "an archived revision is the predecessor's leftover",
			createdAt: created,
			facts:     store.AgentRunProfileFacts{Archived: true, PublishedAt: created.Add(time.Hour), CurrentPublishedAt: published},
			wantStale: true,
		},
		{
			name:      "a soft-deleted revision is hidden work, not live work",
			createdAt: created,
			facts:     store.AgentRunProfileFacts{SoftDeleted: true},
			wantStale: true,
		},
		{
			// The revision the run works on was published while the run was
			// still open: publication froze the content, so the run's work is
			// done or abandoned, not happening now.
			name:      "a run predating its own revision's publication is done",
			createdAt: created,
			facts:     store.AgentRunProfileFacts{IsCurrentPublished: true, PublishedAt: published, CurrentPublishedAt: published},
			wantStale: true,
		},
		{
			// A run started after the current publication works the live
			// revision on purpose (a post-publication job): keep it.
			name:      "a run on the current revision started after its publication",
			createdAt: published.Add(time.Minute),
			facts:     store.AgentRunProfileFacts{IsCurrentPublished: true, PublishedAt: published, CurrentPublishedAt: published},
			wantStale: false,
		},
		{
			// An older draft the edition outgrew: the run predates the
			// edition's current publication and does not work on it.
			name:      "a run on an older draft predating the current publication",
			createdAt: created,
			facts:     store.AgentRunProfileFacts{CurrentPublishedAt: published},
			wantStale: true,
		},
		{
			// The same older draft, started after the publication: a
			// deliberate second draft chain is live work.
			name:      "a run on an older draft started after the current publication",
			createdAt: published.Add(time.Minute),
			facts:     store.AgentRunProfileFacts{CurrentPublishedAt: published},
			wantStale: false,
		},
		{
			// Nothing published yet: the draft is the edition's only live
			// work, whatever its age.
			name:      "a draft run on an edition with nothing published",
			createdAt: created,
			facts:     store.AgentRunProfileFacts{},
			wantStale: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := setupRunStale(tc.createdAt, tc.facts); got != tc.wantStale {
				t.Fatalf("setupRunStale(%v, %+v) = %v, want %v", tc.createdAt, tc.facts, got, tc.wantStale)
			}
		})
	}
}

// ── the aggregation seams ──────────────────────────────────────────────────

func TestFrameToVoiceTotal(t *testing.T) {
	both := rt.Info{}
	both.Stats.Latency.FrameToCommitted = pipeline.LatencySummary{Count: 10, P50: 300 * time.Millisecond}
	both.CommitToVoice = rt.CommitToVoiceSummary{Count: 5, P50MS: 112}
	if got, n := frameToVoiceTotal(both); got != 412 || n != 5 {
		t.Fatalf("both halves = (%d, %d), want (412, 5)", got, n)
	}
	frameHalf := rt.Info{}
	frameHalf.Stats.Latency.FrameToCommitted = pipeline.LatencySummary{Count: 10, P50: 300 * time.Millisecond}
	if got, n := frameToVoiceTotal(frameHalf); got != 300 || n != 0 {
		t.Fatalf("frame→committed alone = (%d, %d), want (300, 0)", got, n)
	}
	if got, n := frameToVoiceTotal(rt.Info{}); got != 0 || n != 0 {
		t.Fatalf("no samples = (%d, %d), want (0, 0)", got, n)
	}
}

func TestCaptureHealthFromSession(t *testing.T) {
	cases := []struct {
		ss    StatusSession
		level string
	}{
		{StatusSession{CaptureFPS: 25, CaptureFPSTarget: 30}, statusLevelOK},
		{StatusSession{CaptureFPS: 30, CaptureFPSTarget: 30}, statusLevelOK},
		{StatusSession{CaptureFPS: 10, CaptureFPSTarget: 30}, statusLevelSlow}, // under half the target
		{StatusSession{CaptureFPS: 0, FramesDropped: 5}, statusLevelSlow},
		{StatusSession{CaptureFPS: 0, State: "starting"}, statusLevelOK},
		{StatusSession{}, statusLevelOK},
	}
	for _, tc := range cases {
		v := captureHealthFromSession(&tc.ss)
		if v.Level != tc.level {
			t.Errorf("capture %v → level %q, want %q (%s)", tc.ss, v.Level, tc.level, v.Detail)
		}
	}
}

func TestOCRHealthFromSession(t *testing.T) {
	cases := []struct {
		ss    StatusSession
		level string
	}{
		{StatusSession{OCRCalls: 40, OCRErrors: 0}, statusLevelOK},
		{StatusSession{OCRCalls: 10, OCRErrors: 3}, statusLevelSlow},
		{StatusSession{OCRCalls: 5, OCRErrors: 5}, statusLevelFail},
		{StatusSession{}, statusLevelOK},
	}
	for _, tc := range cases {
		v := ocrHealthFromSession(&tc.ss)
		if v.Level != tc.level {
			t.Errorf("ocr %v → level %q, want %q (%s)", tc.ss, v.Level, tc.level, v.Detail)
		}
	}
}

func TestReadinessFailureDetailNamesTheCheck(t *testing.T) {
	body := map[string]any{
		"database": map[string]any{"status": "fail", "error": "disk I/O error"},
		"audio":    map[string]any{"status": "ok"},
	}
	if got := readinessFailureDetail(body); got != "database: disk I/O error" {
		t.Fatalf("detail = %q, want the named check", got)
	}
	if got := readinessFailureDetail(map[string]any{}); got != "a dependency is not ready" {
		t.Fatalf("unrecognised body = %q, want the honest word", got)
	}
}

// statusTestEnv is the status half of the appliance: real store, real hub,
// the server under test.
type statusTestEnv struct {
	t   *testing.T
	ctx context.Context
	a   *testAuth
	st  *store.Store
	hub *events.Hub
	srv *httptest.Server
	s   *Server
}

func newStatusEnv(t *testing.T, wire func(*statusTestEnv)) *statusTestEnv {
	t.Helper()
	a := newTestAuth(t)
	env := &statusTestEnv{t: t, ctx: context.Background(), a: a, st: a.st}
	env.hub = events.New(events.WithHeartbeat(250*time.Millisecond), events.WithLogger(discardLogger))
	env.s = New(testBuild, testStarted, discardLogger,
		WithWebAssets(uiBuild()),
		WithAuth(a.svc, a.repo),
		WithEvents(env.hub),
		WithStore(a.st),
		// The store seam the snapshot's setup-run half reads (status.go's
		// assembleSetupRuns): stopStatusEngine's nudge goes through it.
		// An env with no active agent run shows no setup run, so the
		// snapshot every other status test asserts on is unchanged.
		WithOnboarding(store.NewJobsStore(a.st), store.NewAgentStore(a.st), tools.NewSQLiteApprovals(a.st)),
	)
	if wire != nil {
		wire(env)
	}
	env.srv = httptest.NewServer(env.s.Handler())
	t.Cleanup(env.srv.Close)
	// The status push engine is process-lived per Server (ADR-0104): its
	// loop exits only when the hub refuses a publish, and a quiescent engine
	// (the displayed shape unchanged since its last frame) publishes
	// nothing — so closing the hub alone would leave it ticking until
	// process exit (issue #19). stopStatusEngine (registered after the
	// server close, so it runs first) changes the displayed shape through
	// the store seam while the store is still open (its Close was registered
	// in newTestAuth, so it runs last), closes the hub, and waits for the
	// engine's next tick to publish the changed shape into the closed hub —
	// the refusal is the loop's exit, bounded by two 500 ms ticks (the
	// coalescing floor) plus the wait's own 50 ms poll step.
	t.Cleanup(env.stopStatusEngine)
	return env
}

// stopStatusEngine ends the env's status engine, if one is running, by the
// engine's own shutdown rule (ADR-0104): it records a setup run the bar
// would display (the snapshot's rollup gains the run's head, read through
// the store seam), closes the hub, and waits for the engine's next tick to
// publish the changed shape into the now-closed hub and exit. The wait runs
// while the store is still open: the moment the store closes the run row
// is unreadable, the displayed shape reverts, and a quiescent engine
// publishes nothing again. An env with no running engine (every status
// test that does not stream status) answers the wait on the first poll.
func (e *statusTestEnv) stopStatusEngine() {
	e.t.Helper()
	run := store.AgentRun{
		ID:            "status-engine-close-nudge",
		State:         store.AgentStateQueued,
		PromptVersion: "test",
	}
	if err := store.NewAgentStore(e.st).CreateAgentRun(e.ctx, run); err != nil {
		e.t.Errorf("record the engine-close nudge run: %v", err)
		return
	}
	if err := e.hub.Close(); err != nil {
		e.t.Errorf("close the env's hub: %v", err)
		return
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		if !statusEngineGoroutineAlive() {
			return
		}
		if time.Now().After(deadline) {
			e.t.Errorf("the status engine is still alive 1.5 s after its hub closed: the loop should exit on the refused publish (issue #19)")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// statusEngineGoroutineAlive reports whether a status engine loop is still
// running in this process: the loop's own frame, not its children's
// created-by chain (the engine spawns none), is the tell.
func statusEngineGoroutineAlive() bool {
	buf := make([]byte, 1<<20)
	return strings.Contains(string(buf[:runtime.Stack(buf, true)]), "api.(*statusEngine).run")
}

// getJSON issues an authenticated GET and answers the parsed body.
func (e *statusTestEnv) getJSON(role auth.Role, target string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.ctx, http.MethodGet, e.srv.URL+target, nil)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.a.bearer(role))
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatalf("GET %s: %v", target, err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	resp.Body.Close()
	return resp, body
}

func TestStatusGetAnswersTheSnapshot(t *testing.T) {
	env := newStatusEnv(t, nil)
	resp, body := env.getJSON(auth.RoleViewer, statusPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d (%s), want 200", statusPath, resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var snap StatusSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("body is not the snapshot: %v (%s)", err, body)
	}
	if snap.Status == "" || snap.Status != RenderStatus(snap) {
		t.Fatalf("status = %q; the string must be the one rendered from this snapshot", snap.Status)
	}
	if snap.Device.Version != testBuild.Version {
		t.Fatalf("device.version = %q, want %q", snap.Device.Version, testBuild.Version)
	}
	if len(snap.Health.Components) != len(statusComponents) {
		t.Fatalf("health carries %d components, want the six canonical ones", len(snap.Health.Components))
	}
	for i, comp := range statusComponents {
		if snap.Health.Components[i].ID != comp {
			t.Fatalf("health[%d] = %q, want the canonical order %q", i, snap.Health.Components[i].ID, comp)
		}
	}
	// No session, no setup run: the bar is the rollup, and the rollup says
	// so honestly (capture and OCR are unknown without a session, not ok).
	if snap.Session != nil || len(snap.Setup) != 0 {
		t.Fatalf("idle box answered session=%v setup=%v, want neither", snap.Session, snap.Setup)
	}
	// The contract gate records the success body against the documented 200.
	recordContractResponse(t, http.MethodGet, statusPath, http.StatusOK, "application/json", body)

	// The method contract: a POST the scope table never grants is refused
	// at the authorization layer before it reaches the router (403 for an
	// authenticated caller: the grant is GET viewer, and an ungranted
	// method+path is fail-closed).
	req, _ := http.NewRequestWithContext(env.ctx, http.MethodPost, env.srv.URL+statusPath, nil)
	req.Header.Set("Authorization", "Bearer "+env.a.bearer(auth.RoleViewer))
	post, err := env.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", statusPath, err)
	}
	if post.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s = %d, want 403 (no POST grant in the scope table)", statusPath, post.StatusCode)
	}
	io.Copy(io.Discard, post.Body)
	post.Body.Close()

	// And the scope table: an unauthenticated request is refused (the
	// route's grant is viewer, not public).
	req, _ = http.NewRequestWithContext(env.ctx, http.MethodGet, env.srv.URL+statusPath, nil)
	unauth, err := env.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("unauthenticated GET: %v", err)
	}
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET %s = %d, want 401", statusPath, unauth.StatusCode)
	}
	io.Copy(io.Discard, unauth.Body)
	unauth.Body.Close()
}

// seedStatusContent is the content the setup-run aggregation reads: a game,
// an edition, a draft profile with twelve characters, nine of whom have a
// non-retired voice revision.
func seedStatusContent(t *testing.T, ctx context.Context, st *store.Store) (profileID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	g := games.Game{
		ID: games.NewID(), Slug: "skies-of-arcadia", Title: "Skies of Arcadia",
		Platform: "dreamcast", UserTags: []byte(`["synthetic"]`), CreatedAt: now,
	}
	if err := st.CreateGame(ctx, g); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	ed := games.Edition{ID: games.NewID(), GameID: g.ID, Region: "US",
		Language: "en", Revision: "v1", Locale: "en_US"}
	if err := st.CreateEdition(ctx, ed); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	prof, err := st.CreateDraftProfile(ctx, ed.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	profileID = prof.ID
	var chars []games.Character
	charIDs := make([]string, 12)
	for i := 0; i < 12; i++ {
		id := games.NewID()
		charIDs[i] = id
		chars = append(chars, games.Character{
			ID: id, ProfileID: profileID, Name: fmt.Sprintf("Character %d", i+1),
			Status: "confirmed", CreatedAt: now,
		})
	}
	if err := st.PutCharacters(ctx, profileID, chars); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}
	// A line per character (ADR 0134's denominator): the count's universe is
	// the active speaking characters, and a roster slot nobody speaks is not
	// a speaker, so every one of the twelve must hold a line to count at all.
	msgs := make([]games.Message, 0, 12)
	for i, id := range charIDs {
		text := fmt.Sprintf("Line for character %d.", i+1)
		msgs = append(msgs, games.Message{
			ID:                    games.NewID(),
			ProfileID:             profileID,
			OriginalText:          text,
			MarkupJSON:            json.RawMessage(`[]`),
			NormalizedText:        text,
			SpeechText:            text,
			Language:              "en",
			TextKind:              games.TextDialogue,
			SpeakerID:             id,
			AttributionStatus:     games.AttributionConfirmed,
			AttributionConfidence: 1.0,
			Flags:                 json.RawMessage(`{}`),
			Revision:              1,
			CreatedAt:             now,
			UpdatedAt:             now,
		})
	}
	if err := st.PutMessages(ctx, profileID, msgs, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	// The first nine characters get a draft voice; character ten's only
	// revision is retired (no voice); eleven and twelve have none.
	// Character nine also gets a fresh second revision on top of its draft:
	// the count is per character, so it must not be double-counted. The voice
	// rows are keyed by NAME the way the voice surface keys them (ADR 0134)
	// — a row id here would be what the old, broken join expected.
	for i := 0; i < 9; i++ {
		if err := st.CreateVoiceRevision(ctx, store.VoiceProfile{
			ID: games.NewID(), Character: fmt.Sprintf("Character %d", i+1), Revision: 1, State: store.VoiceStateDraft,
		}); err != nil {
			t.Fatalf("CreateVoiceRevision %d: %v", i+1, err)
		}
	}
	if err := st.CreateVoiceRevision(ctx, store.VoiceProfile{
		ID: games.NewID(), Character: "Character 9", Revision: 2, State: store.VoiceStateDraft,
	}); err != nil {
		t.Fatalf("CreateVoiceRevision char-9 rev 2: %v", err)
	}
	if err := st.CreateVoiceRevision(ctx, store.VoiceProfile{
		ID: games.NewID(), Character: "Character 10", Revision: 1, State: store.VoiceStateRetired,
	}); err != nil {
		t.Fatalf("CreateVoiceRevision char-10: %v", err)
	}
	return profileID
}

// backdateRun rewinds a run's stamps through a second connection on the same
// file (the library tests' backdatePurgeAfter pattern): the staleness
// comparison is the run's created_at against the publication stamps, and the
// test needs a run that started long before a publication that happens
// seconds later. The store owns the clock; the test plays the hours.
func backdateRun(t *testing.T, path, runID, stamp string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_busy_timeout=5000&_foreign_keys=1")
	if err != nil {
		t.Fatalf("open backdate connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("UPDATE agent_runs SET created_at = ?, updated_at = ? WHERE id = ?",
		stamp, stamp, runID); err != nil {
		t.Fatalf("backdate run %s: %v", runID, err)
	}
}

func TestStatusAggregatesSetupRuns(t *testing.T) {
	env := newStatusEnv(t, func(e *statusTestEnv) {
		e.s.onb = &onbDeps{AgentStore: store.NewAgentStore(e.st)}
	})
	profileID := seedStatusContent(t, env.ctx, env.st)
	agentStore := store.NewAgentStore(env.st)
	planJSON := []byte(`{"title":"Skies of Arcadia"}`)

	// The live run: a running setup with the profile and two tool calls
	// (the newest is the stage the agent is on).
	if err := agentStore.CreateAgentRun(env.ctx, store.AgentRun{
		ID: "run-live", ProfileID: profileID, State: store.AgentStateRunning,
		PlanJSON: planJSON, PromptVersion: "test-prompt",
	}); err != nil {
		t.Fatalf("CreateAgentRun: %v", err)
	}
	finished := time.Now().UTC().Add(-time.Second)
	if _, err := agentStore.InsertAgentToolCall(env.ctx, store.AgentToolCall{
		ID: "tc-1", RunID: "run-live", Name: "create_characters", Status: "succeeded",
		ArgsJSON: []byte(`{}`), ResultJSON: []byte(`{"results":[]}`), FinishedAt: &finished,
	}); err != nil {
		t.Fatalf("InsertAgentToolCall: %v", err)
	}
	if _, err := agentStore.InsertAgentToolCall(env.ctx, store.AgentToolCall{
		ID: "tc-2", RunID: "run-live", Name: "assign_voices", Status: "pending",
		ArgsJSON: []byte(`{}`),
	}); err != nil {
		t.Fatalf("InsertAgentToolCall: %v", err)
	}
	// A terminal run: must not be status data.
	if err := agentStore.CreateAgentRun(env.ctx, store.AgentRun{
		ID: "run-done", State: "succeeded", PlanJSON: planJSON, PromptVersion: "test-prompt",
	}); err != nil {
		t.Fatalf("CreateAgentRun terminal: %v", err)
	}

	resp, body := env.getJSON(auth.RoleViewer, statusPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d (%s)", resp.StatusCode, body)
	}
	var snap StatusSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("body is not the snapshot: %v", err)
	}
	if len(snap.Setup) != 1 {
		t.Fatalf("setup_runs = %v, want exactly the live run", snap.Setup)
	}
	r := snap.Setup[0]
	if r.RunID != "run-live" || r.State != "running" || r.Stage != "assign_voices" {
		t.Fatalf("run view = %+v, want the live run at its newest tool call", r)
	}
	if r.Game != "Skies of Arcadia" {
		t.Fatalf("game = %q, want the profile's game title", r.Game)
	}
	if r.Voiced != 9 || r.Total != 12 {
		t.Fatalf("voiced = %d of %d, want 9 of 12 (the retired-only and the voiceless characters do not count, and a character's two revisions count once)", r.Voiced, r.Total)
	}
	if r.Elapsed > 5000 {
		t.Fatalf("elapsed = %dms for a just-created run, want about 0", r.Elapsed)
	}
	// The bar's string is the setup head, server-rendered.
	if want := "● agent running · 9 of 12 voiced"; !strings.HasPrefix(snap.Status, want) {
		t.Fatalf("status = %q, want the head %q…", snap.Status, want)
	}

	// The pre-profile half: a queued run with no profile names the plan's
	// title instead of failing the snapshot, and carries no counts.
	if err := agentStore.CreateAgentRun(env.ctx, store.AgentRun{
		ID: "run-early", State: store.AgentStateQueued,
		PlanJSON: []byte(`{"title":"Wind Waker"}`), PromptVersion: "test-prompt",
	}); err != nil {
		t.Fatalf("CreateAgentRun pre-profile: %v", err)
	}
	resp, body = env.getJSON(auth.RoleViewer, statusPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d (%s)", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("body is not the snapshot: %v", err)
	}
	if len(snap.Setup) != 2 {
		t.Fatalf("setup_runs = %d rows, want 2", len(snap.Setup))
	}
	var early *StatusSetupRun
	for i := range snap.Setup {
		if snap.Setup[i].RunID == "run-early" {
			early = &snap.Setup[i]
		}
	}
	if early == nil {
		t.Fatalf("the pre-profile run is missing from %v", snap.Setup)
	}
	if early.Game != "Wind Waker" || early.Voiced != 0 || early.Total != 0 || early.Stage != "" {
		t.Fatalf("pre-profile view = %+v, want the plan's title, no counts and no stage", early)
	}
}

// TestStatusExcludesSupersededSetupRuns is the regression for the
// 2026-10-21 evidence: after a new revision was published, old blocked runs
// of the superseded revision still drove the bar ("blocked · 10 of 45 ·
// 21h 28m") while the real profile was fully voiced. The rule under test:
// a run is status data only while its profile revision is the edition's
// live work — the moment the edition moves past it (the revision is
// archived, or is published out from under the run), the run drops out of
// the status line; a blocked run that stays carries the word, not a live
// clock; and the counts of a kept run come from the revision it names.
func TestStatusExcludesSupersededSetupRuns(t *testing.T) {
	env := newStatusEnv(t, func(e *statusTestEnv) {
		e.s.onb = &onbDeps{AgentStore: store.NewAgentStore(e.st)}
	})
	profileID := seedStatusContent(t, env.ctx, env.st)
	agentStore := store.NewAgentStore(env.st)
	planJSON := []byte(`{"title":"Skies of Arcadia"}`)

	mustVersion := func(id string) int64 {
		t.Helper()
		_, rev, err := env.st.GetProfile(env.ctx, id)
		if err != nil {
			t.Fatalf("GetProfile(%s): %v", id, err)
		}
		return rev
	}
	fetch := func() StatusSnapshot {
		t.Helper()
		resp, body := env.getJSON(auth.RoleViewer, statusPath)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET = %d (%s)", resp.StatusCode, body)
		}
		var s StatusSnapshot
		if err := json.Unmarshal(body, &s); err != nil {
			t.Fatalf("body is not the snapshot: %v (%s)", err, body)
		}
		if s.Status != RenderStatus(s) {
			t.Fatalf("status = %q is not the string rendered from this snapshot", s.Status)
		}
		return s
	}
	runByID := func(snap StatusSnapshot, id string) *StatusSetupRun {
		t.Helper()
		for i := range snap.Setup {
			if snap.Setup[i].RunID == id {
				return &snap.Setup[i]
			}
		}
		return nil
	}
	makeRun := func(id, profileID, state string) {
		t.Helper()
		if err := agentStore.CreateAgentRun(env.ctx, store.AgentRun{
			ID: id, ProfileID: profileID, State: state,
			PlanJSON: planJSON, PromptVersion: "test-prompt",
		}); err != nil {
			t.Fatalf("CreateAgentRun(%s): %v", id, err)
		}
	}

	// The evidence shape, before anything is published: a run stalled on
	// the draft the owner is about to move past (started two days ago, the
	// way the evidence's run was), and a pre-profile run abandoned in the
	// same state. Both are the edition's live work now, so both show — and
	// the blocked word carries no live clock.
	makeRun("run-stale", profileID, store.AgentStateBlocked)
	backdateRun(t, env.st.Path(), "run-stale", time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339))
	makeRun("run-orphan", "", store.AgentStateBlocked)
	backdateRun(t, env.st.Path(), "run-orphan", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))

	snap := fetch()
	stale := runByID(snap, "run-stale")
	if stale == nil || stale.Voiced != 9 || stale.Total != 12 || stale.Game != "Skies of Arcadia" {
		t.Fatalf("run-stale view = %+v, want the live draft's 9 of 12 with the game's title", stale)
	}
	orphan := runByID(snap, "run-orphan")
	if orphan == nil || orphan.Game != "Skies of Arcadia" || orphan.Voiced != 0 || orphan.Total != 0 {
		t.Fatalf("run-orphan view = %+v, want the plan's title with no counts", orphan)
	}
	if !strings.HasPrefix(snap.Status, "● agent blocked") || strings.Contains(snap.Status, "of 45") {
		t.Fatalf("status = %q, want the blocked run's head with no clock and no stale count", snap.Status)
	}

	// Publish the revision the stale run works on. The gate needs a
	// narration; seedStatusContent's twelve lines resolve inside the
	// revision, so the publication ships.
	if err := env.st.SetProfileStatus(env.ctx, profileID, games.StatusReview, mustVersion(profileID)); err != nil {
		t.Fatalf("review: %v", err)
	}
	first, err := env.st.Publish(env.ctx, profileID, mustVersion(profileID), "curator@local")
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}

	// The run's own revision is now the one the edition serves — published
	// out from under the run, which froze the content the run works on. The
	// run is over or abandoned, not happening now: it drops out, and the
	// pre-profile run (never superseded) stays as the honest "blocked".
	snap = fetch()
	if got := runByID(snap, "run-stale"); got != nil {
		t.Fatalf("the run whose revision just published is still status data: %+v", got)
	}
	if runByID(snap, "run-orphan") == nil {
		t.Fatalf("the pre-profile run dropped out: %+v", snap.Setup)
	}
	if !strings.HasPrefix(snap.Status, "● agent blocked") {
		t.Fatalf("status = %q, want the pre-profile blocked run's head", snap.Status)
	}

	// Derive a successor and publish it: the predecessor is archived in the
	// same commit the pointer moves — the superseded state the evidence had.
	// A run on a fresh independent draft, started before that publication,
	// is the "older than a newer published revision" clause.
	second, err := env.st.NewRevision(env.ctx, first.ID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	third, err := env.st.CreateDraftProfile(env.ctx, first.EditionID)
	if err != nil {
		t.Fatalf("CreateDraftProfile (independent chain): %v", err)
	}
	makeRun("run-early", third.ID, store.AgentStateBlocked)
	// A day before the publication that follows: the run predates the
	// edition's current publication without working on it.
	backdateRun(t, env.st.Path(), "run-early", time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339))

	if err := env.st.SetProfileStatus(env.ctx, second.ID, games.StatusReview, mustVersion(second.ID)); err != nil {
		t.Fatalf("snapshot to review: %v", err)
	}
	current, err := env.st.Publish(env.ctx, second.ID, mustVersion(second.ID), "curator@local")
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	// Post-publication runs: one on the current published revision (the
	// counts must come from this profile) and one on the older draft
	// started after the publication (deliberate second-chain work). Both
	// are newer than the backdated runs, so one of them owns the head.
	makeRun("run-late", third.ID, store.AgentStateQueued)
	makeRun("run-current", current.ID, store.AgentStateRunning)

	snap = fetch()
	for _, id := range []string{"run-stale", "run-early"} {
		if got := runByID(snap, id); got != nil {
			t.Fatalf("%s survived the supersession: %+v", id, got)
		}
	}
	cur := runByID(snap, "run-current")
	if cur == nil || cur.State != store.AgentStateRunning || cur.Voiced != 9 || cur.Total != 12 || cur.Game != "Skies of Arcadia" {
		t.Fatalf("run-current view = %+v, want the current published profile's 9 of 12", cur)
	}
	if runByID(snap, "run-late") == nil || runByID(snap, "run-orphan") == nil {
		t.Fatalf("the post-publication runs dropped out: %+v", snap.Setup)
	}
	if len(snap.Setup) != 3 {
		t.Fatalf("setup_runs = %d rows, want exactly the three live ones", len(snap.Setup))
	}
	if !strings.HasPrefix(snap.Status, "● agent") || strings.Contains(snap.Status, "blocked") {
		t.Fatalf("status = %q, want a live run's head with no blocked word", snap.Status)
	}
}

func TestStatusHealthReadsCachedProbesOnly(t *testing.T) {
	probes := 0
	env := newStatusEnv(t, func(e *statusTestEnv) {
		e.s.readiness = func(ctx context.Context) (map[string]any, bool) {
			probes++
			return map[string]any{
				"database": map[string]any{"status": "fail", "error": "disk I/O error"},
			}, false
		}
		// The doctor's cached report (the rollup's model-backed half): the
		// run finished with decision ok, tts degraded, agent blocked.
		run := e.s.diagRuns.create(nil)
		for _, v := range []diagnosticsRoleView{
			{Role: "decision", Configured: true, Status: "OK", Summary: "decision model answered"},
			{Role: "voice_design", Configured: true, Status: "DEGRADED", Summary: "tts latency above budget"},
			{Role: "agent_reasoning", Configured: true, Status: "BLOCKED", Summary: "agent model unavailable"},
		} {
			run.appendRole(v)
		}
		run.finish()
	})

	resp, body := env.getJSON(auth.RoleViewer, statusPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d (%s)", resp.StatusCode, body)
	}
	var snap StatusSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("body is not the snapshot: %v", err)
	}
	byID := map[string]StatusHealthComponent{}
	for _, c := range snap.Health.Components {
		byID[c.ID] = c
	}
	if byID["decision"].Level != statusLevelOK || byID["decision"].Detail != "decision model answered" {
		t.Fatalf("decision tile = %+v, want ok with the doctor's own words", byID["decision"])
	}
	if byID["tts"].Level != statusLevelSlow {
		t.Fatalf("tts tile = %+v, want slow (DEGRADED maps to slow)", byID["tts"])
	}
	if byID["agent"].Level != statusLevelFail {
		t.Fatalf("agent tile = %+v, want fail (BLOCKED maps to fail)", byID["agent"])
	}
	if byID["storage"].Level != statusLevelFail || byID["storage"].Detail != "database: disk I/O error" {
		t.Fatalf("storage tile = %+v, want the probe's own failure text", byID["storage"])
	}
	if probes != 1 {
		t.Fatalf("the readiness probe ran %d times for one snapshot, want exactly 1", probes)
	}

	// The cache: a second snapshot inside the TTL must not re-probe (the
	// rule the redesign sets: the status bar never triggers a probe more
	// often than its cache).
	time.Sleep(10 * time.Millisecond)
	resp, body = env.getJSON(auth.RoleViewer, statusPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d (%s)", resp.StatusCode, body)
	}
	if probes != 1 {
		t.Fatalf("the readiness probe ran %d times for two snapshots inside the TTL, want 1 (cached)", probes)
	}

	// And the rollup says what the tiles say: the non-ok tiles in the
	// string, the unprobed ones counted as unknown, not ok.
	if !strings.Contains(snap.Status, "tts slow") || !strings.Contains(snap.Status, "agent fail") {
		t.Fatalf("status = %q, want the non-ok tiles in the string", snap.Status)
	}
	if !strings.Contains(snap.Status, "2 unknown") {
		t.Fatalf("status = %q, want the unprobed tiles counted as unknown, not ok", snap.Status)
	}
}

func TestStatusStreamReplaysAndFilters(t *testing.T) {
	env := newStatusEnv(t, nil)
	env.s.listenAddr = "127.0.0.1:4101"

	stream := openStatusStream(t, env, "", nil)
	defer stream.stop()

	// The engine's baseline publish arrives: a status.changed frame whose
	// data is the full snapshot, and whose id is its seq.
	frame, err := stream.next(3 * time.Second)
	if err != nil {
		t.Fatalf("no first frame: %v", err)
	}
	if frame.Name != statusEventName {
		t.Fatalf("first frame's name = %q, want %s", frame.Name, statusEventName)
	}
	first := frame.cursor(t)
	var snap StatusSnapshot
	if err := json.Unmarshal(frame.envelope(t).Data, &snap); err != nil {
		t.Fatalf("frame data is not the snapshot: %v (%q)", err, frame.Data)
	}
	if snap.Device.ListenAddr != "127.0.0.1:4101" {
		t.Fatalf("device.listen_addr = %q, want the wired address", snap.Device.ListenAddr)
	}

	// While the stream is open, a non-status event on the hub must not
	// appear on it: the name filter is the stream's whole reason to exist.
	if _, err := env.hub.Publish(env.ctx, events.Envelope{Name: "message.committed", Data: []byte(`{}`)}); err != nil {
		t.Fatalf("publish the foreign event: %v", err)
	}
	// A second status frame, published like the engine's, does appear.
	secondData, _ := json.Marshal(StatusSnapshot{
		UpdatedAt: "2026-01-01T00:00:00Z",
		Status:    "6 ok",
		Health:    StatusHealth{OK: 6, Detail: "6 ok"},
		Device:    StatusDevice{Version: "test", UptimeMS: 1},
	})
	if _, err := env.hub.Publish(env.ctx, events.Envelope{Name: statusEventName, Data: secondData}); err != nil {
		t.Fatalf("publish the status frame: %v", err)
	}
	frame, err = stream.next(3 * time.Second)
	if err != nil {
		t.Fatalf("no second frame: %v", err)
	}
	if frame.Name != statusEventName {
		t.Fatalf("second frame's name = %q, want only %s on this stream (the foreign event must be filtered)", frame.Name, statusEventName)
	}
	second := frame.cursor(t)
	if second <= first {
		t.Fatalf("second cursor %d is not after the first %d", second, first)
	}
	stream.stop()

	// Reconnect with Last-Event-ID: the client that missed the frame while
	// its tab was closed gets it back from the hub's bounded log.
	reconnect := openStatusStream(t, env, "", map[string]string{"Last-Event-ID": fmt.Sprint(first)})
	defer reconnect.stop()
	replayed, err := reconnect.next(3 * time.Second)
	if err != nil {
		t.Fatalf("no replayed frame: %v", err)
	}
	if got := replayed.cursor(t); got != second {
		t.Fatalf("replayed cursor = %d, want the missed frame %d", got, second)
	}
}

// TestStatusEnginePublishesOnChangeAtMostTwiceASecond pins the push
// contract: the engine publishes only when the displayed shape changed,
// coalesced to at most one publish per statusEngineMinPush. A chatty box
// (a line committed every 200 ms) cannot beat the ≤2/s the redesign sets,
// and a change that lands inside the floor is coalesced: the frame that
// arrives carries the latest shape, not the intermediate ones.
func TestStatusEnginePublishesOnChangeAtMostTwiceASecond(t *testing.T) {
	env := newStatusEnv(t, nil)
	eng := env.s.statusEngineRef()
	sub, err := env.hub.Subscribe(env.ctx, events.Filter{Names: []string{statusEventName}}, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	// Frame one: the baseline publish.
	if !eng.tick(env.ctx) {
		t.Fatal("the baseline tick refused to publish")
	}
	e1, got, err := sub.Next(env.ctx, 2*time.Second)
	if err != nil || !got {
		t.Fatalf("no baseline frame: got=%v err=%v", got, err)
	}
	if e1.Name != statusEventName {
		t.Fatalf("baseline frame's name = %q, want %s", e1.Name, statusEventName)
	}

	// Changes that land inside the push floor: coalesced, unpublished.
	// The version field is in the compare key, so a build-string change is
	// a display change for the engine's purposes.
	env.s.build.Version = "v0.0.2-test"
	if !eng.tick(env.ctx) {
		t.Fatal("tick refused")
	}
	env.s.build.Version = "v0.0.3-test"
	if !eng.tick(env.ctx) {
		t.Fatal("tick refused")
	}
	if _, got, _ := sub.Next(env.ctx, 150*time.Millisecond); got {
		t.Fatal("a change inside the push floor published early: at most one frame per 500 ms is the contract")
	}

	// The floor clears: the coalesced change publishes once, with the
	// latest shape (the intermediate v0.0.2 is never on the wire).
	time.Sleep(560 * time.Millisecond)
	if !eng.tick(env.ctx) {
		t.Fatal("tick refused")
	}
	e2, got, err := sub.Next(env.ctx, 2*time.Second)
	if err != nil || !got {
		t.Fatalf("no coalesced frame after the floor cleared: got=%v err=%v", got, err)
	}
	var snap StatusSnapshot
	if err := json.Unmarshal(e2.Data, &snap); err != nil {
		t.Fatalf("coalesced frame is not the snapshot: %v", err)
	}
	if snap.Device.Version != "v0.0.3-test" {
		t.Fatalf("coalesced frame's version = %q, want the latest shape v0.0.3-test", snap.Device.Version)
	}

	// And an unchanged snapshot publishes nothing even when the floor is
	// clear: push on change, not on tick.
	if !eng.tick(env.ctx) {
		t.Fatal("tick refused")
	}
	if _, got, _ := sub.Next(env.ctx, 150*time.Millisecond); got {
		t.Fatal("an unchanged snapshot published: the bar must not churn on a box that did nothing")
	}
}

// openStatusStream issues GET /api/v1/status/stream the way the events
// tests open theirs: a live client on a real connection, cancelled on
// cleanup so the server's Close cannot wait on an open SSE.
func openStatusStream(t *testing.T, env *statusTestEnv, query string, headers map[string]string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.srv.URL+statusStreamPath+query, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+env.a.bearer(auth.RoleViewer))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := env.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", statusStreamPath, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, b)
	}
	return &sseStream{t: t, resp: resp, cancel: cancel, r: bufio.NewReader(resp.Body)}
}

// TestStatusStreamWithoutHubAnswers503 is the honest-degradation rule the
// event stream follows: a stream that opened 200 with no publisher behind
// it would be a bar that silently froze.
func TestStatusStreamWithoutHubAnswers503(t *testing.T) {
	a := newTestAuth(t)
	s := New(testBuild, testStarted, discardLogger,
		WithWebAssets(uiBuild()),
		WithAuth(a.svc, a.repo),
	) // no hub
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+statusStreamPath, nil)
	req.Header.Set("Authorization", "Bearer "+a.bearer(auth.RoleViewer))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no hub wired)", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// TestStatusExcludesAbandonedSetupRuns is the issue #45 regression on the
// status line: a run the operator abandoned (terminal) is no longer status
// data, so the bar, the Library and the Health surfaces stop naming it — the
// abandoned run must not appear in the snapshot's setup_runs, the way a
// superseded run does not (ADR-0104 amendment).
func TestStatusExcludesAbandonedSetupRuns(t *testing.T) {
	env := newStatusEnv(t, func(e *statusTestEnv) {
		e.s.onb = &onbDeps{AgentStore: store.NewAgentStore(e.st)}
	})
	profileID := seedStatusContent(t, env.ctx, env.st)
	agentStore := store.NewAgentStore(env.st)
	if err := agentStore.CreateAgentRun(env.ctx, store.AgentRun{
		ID: "run-abandoned", ProfileID: profileID, State: store.AgentStateAbandoned, PlanJSON: []byte(`{"title":"Skies of Arcadia"}`), PromptVersion: "test-prompt",
	}); err != nil {
		t.Fatalf("CreateAgentRun: %v", err)
	}
	resp, body := env.getJSON(auth.RoleViewer, statusPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d (%s)", resp.StatusCode, body)
	}
	var snap StatusSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("body is not the snapshot: %v (%s)", err, body)
	}
	if len(snap.Setup) != 0 {
		t.Fatalf("setup_runs = %v, want the abandoned run excluded", snap.Setup)
	}
}
