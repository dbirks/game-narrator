package api

// The catalog surface, tested over HTTP against a real migrated store: one
// appliance = one SQLite database, one asset root, one adapter registry, one job
// runner and one routed handler (docs/spec/12 "Unit" mode with the real storage,
// and AGENTS.md "no simulation theatre" — a game list served by a fake repository
// would prove nothing about the row_version, cursor or CHECK constraints these
// assertions depend on).
//
// Every request carries a real credential of the role under test, because a
// route test that sent none would be asserting the 401 shape rather than the
// route.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/store"
)

// syntheticCSVPath is the generic adapter's own fixture script: 40 invented lines
// with a speaker, a kind and an area column, and no real game text anywhere
// (AGENTS.md rule 5). It is the same file the live walk-through imports, so the
// HTTP test and the binary test exercise one input.
const syntheticCSVPath = "../games/adapter/generic/testdata/synthetic-adventure.csv"

// catalogEnv is one wired appliance.
type catalogEnv struct {
	t      *testing.T
	ctx    context.Context
	a      *testAuth
	st     *store.Store
	as     *assets.Local
	reg    *adapter.Registry
	js     *store.JobsStore
	hub    *events.Hub
	runner *jobs.Runner
	h      http.Handler
}

func newCatalogEnv(t *testing.T) *catalogEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)

	a := newTestAuth(t) // one store: the credentials and the catalogue share it, as they do in the binary
	as, err := assets.NewLocal(filepath.Join(t.TempDir(), "assets"))
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}
	reg := adapter.NewRegistry()
	if err := generic.Register(reg); err != nil {
		t.Fatalf("generic.Register: %v", err)
	}
	js := store.NewJobsStore(a.st)
	// The hub carries the durable event log for the same reason serve does: the
	// import test below reads its job events back through GET /api/v1/events, and a
	// reconnecting client can only be replayed from the log, not the ring.
	hub := events.New(events.WithStore(store.NewEventStore(a.st)), events.WithLogger(discardLogger))
	runner, err := jobs.NewRunner(js, hub, discardLogger, jobs.WithPollInterval(2*time.Millisecond))
	if err != nil {
		t.Fatalf("jobs.NewRunner: %v", err)
	}
	if err := runner.Register(JobKindImportFile, ImportFileJobHandler(a.st, as, reg),
		jobs.RegisterOpts{Concurrency: 1, Resumable: true, Timeout: time.Minute}); err != nil {
		t.Fatalf("register %s: %v", JobKindImportFile, err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := runner.Stop(stopCtx); err != nil {
			t.Errorf("runner.Stop: %v", err)
		}
		if err := hub.Close(); err != nil {
			t.Errorf("hub.Close: %v", err)
		}
	})

	return &catalogEnv{
		t: t, ctx: ctx, a: a, st: a.st, as: as, reg: reg, js: js, hub: hub, runner: runner,
		h: a.handler(WithStore(a.st), WithJobs(js), WithEvents(hub), WithImport(as, reg, runner)),
	}
}

// ── request plumbing ───────────────────────────────────────────────────────

// send performs one request as a role and, for every 2xx, validates the response
// body against the OpenAPI operation that documents this route (contract clause
// (c)). A handler that starts returning a differently-shaped body therefore fails
// a flow test rather than only a schema test.
func (e *catalogEnv) send(role auth.Role, method, target string, body io.Reader, kv ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, target, body)
	if body != nil && len(kv) == 0 {
		r.Header.Set("Content-Type", ContentTypeJSON)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		r.Header.Set(kv[i], kv[i+1])
	}
	if role != auth.RoleNone {
		r.Header.Set("Authorization", "Bearer "+e.a.bearer(role))
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code >= 200 && rec.Code < 300 {
		recordContractResponse(e.t, method, target, rec.Code,
			rec.Header().Get("Content-Type"), rec.Body.Bytes())
	}
	return rec
}

func (e *catalogEnv) get(role auth.Role, target string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.send(role, http.MethodGet, target, nil)
}

func (e *catalogEnv) post(role auth.Role, target, payload string, kv ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.send(role, http.MethodPost, target, strings.NewReader(payload), kv...)
}

func (e *catalogEnv) patch(role auth.Role, target, payload string, kv ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.send(role, http.MethodPatch, target, strings.NewReader(payload), kv...)
}

// mustOK performs a read and returns its decoded body.
func (e *catalogEnv) mustOK(role auth.Role, target string) map[string]any {
	e.t.Helper()
	rec := e.get(role, target)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET %s = %d, want 200 (body %s)", target, rec.Code, rec.Body.String())
	}
	return decodeJSON(e.t, rec)
}

// requireStatus asserts the status and returns the decoded body.
func (e *catalogEnv) requireStatus(rec *httptest.ResponseRecorder, want int, what string) map[string]any {
	e.t.Helper()
	if rec.Code != want {
		e.t.Fatalf("%s = %d, want %d (body %s)", what, rec.Code, want, rec.Body.String())
	}
	return decodeJSON(e.t, rec)
}

// requireProblem asserts docs/spec/08's error contract on one response: the
// problem+json media type, the status, the stable `code` member and a correlation
// id a user can quote to a support screen.
func requireProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) codedProblem {
	t.Helper()
	var doc codedProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body is not a problem document: %v (%s)", err, rec.Body.String())
	}
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, wantStatus, rec.Body.String())
	}
	requireContentType(t, rec, ContentTypeProblem)
	if doc.Status != wantStatus {
		t.Errorf("problem.status = %d, want %d to agree with the status line", doc.Status, wantStatus)
	}
	if doc.Code != wantCode {
		t.Errorf("problem.code = %q, want %q", doc.Code, wantCode)
	}
	if doc.RequestID == "" || doc.RequestID != rec.Header().Get(HeaderRequestID) {
		t.Errorf("problem.request_id = %q, want the correlation header %q", doc.RequestID, rec.Header().Get(HeaderRequestID))
	}
	if !strings.HasPrefix(doc.Type, "urn:game-narrator:error:") {
		t.Errorf("problem.type = %q, want a urn:game-narrator:error: slug", doc.Type)
	}
	return doc
}

// ── fixtures ───────────────────────────────────────────────────────────────

func (e *catalogEnv) createGame(role auth.Role, slug, title, key string) map[string]any {
	e.t.Helper()
	payload := fmt.Sprintf(`{"slug":%q,"title":%q,"platform":"gamecube-like"}`, slug, title)
	rec := e.post(role, "/api/v1/games", payload, "Idempotency-Key", key)
	return e.requireStatus(rec, http.StatusCreated, "POST /api/v1/games "+slug)
}

func (e *catalogEnv) createEdition(role auth.Role, gameID, region, language, revision, key string) map[string]any {
	e.t.Helper()
	payload := fmt.Sprintf(`{"region":%q,"language":%q,"revision":%q,"source_identity":"%s","locale":"%s"}`,
		region, language, revision, strings.Repeat("a", 64), language+"-"+strings.ToUpper(region))
	rec := e.post(role, "/api/v1/games/"+gameID+"/editions", payload, "Idempotency-Key", key)
	return e.requireStatus(rec, http.StatusCreated, "POST editions "+region+"/"+language+"/"+revision)
}

// createDraft opens a draft revision and returns it with its ETag.
func (e *catalogEnv) createDraft(role auth.Role, editionID string) (map[string]any, string) {
	e.t.Helper()
	rec := e.post(role, "/api/v1/editions/"+editionID+"/profiles", "")
	created := e.requireStatus(rec, http.StatusCreated, "POST /api/v1/editions/{id}/profiles")
	etag := rec.Header().Get("ETag")
	if etag == "" {
		e.t.Fatalf("a new draft came back with no ETag: %v", rec.Header())
	}
	return created, etag
}

// etag reads the current token of a revision — the way an editor does, by
// re-reading after every write.
func (e *catalogEnv) etag(profileID string) string {
	e.t.Helper()
	rec := e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET /api/v1/profiles/%s = %d (body %s)", profileID, rec.Code, rec.Body.String())
	}
	return rec.Header().Get("ETag")
}

// uploadCSV sends the synthetic script as a multipart form without waiting for
// the job.
func (e *catalogEnv) uploadCSV(role auth.Role, profileID string, extra ...[2]string) *httptest.ResponseRecorder {
	e.t.Helper()
	raw, err := os.ReadFile(syntheticCSVPath)
	if err != nil {
		e.t.Fatalf("read the synthetic script: %v", err)
	}
	return e.uploadFile(role, profileID, "synthetic-adventure.csv", raw, extra...)
}

func (e *catalogEnv) uploadFile(role auth.Role, profileID, name string, raw []byte, extra ...[2]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		e.t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(raw); err != nil {
		e.t.Fatalf("write the file part: %v", err)
	}
	fields := append([][2]string{{"rights", "user_supplied"}}, extra...)
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			e.t.Fatalf("WriteField(%s): %v", f[0], err)
		}
	}
	if err := mw.Close(); err != nil {
		e.t.Fatalf("close the multipart body: %v", err)
	}
	return e.send(role, http.MethodPost, "/api/v1/profiles/"+profileID+"/import", &buf,
		"Content-Type", mw.FormDataContentType())
}

// importCSV uploads the synthetic script and waits for its job to succeed,
// returning the finished job. Polling the REST resource rather than calling the
// handler in-process is the point: the import must be observable through the same
// endpoint the web UI watches.
func (e *catalogEnv) importCSV(role auth.Role, profileID string) (string, map[string]any) {
	e.t.Helper()
	rec := e.uploadCSV(role, profileID)
	if rec.Code != http.StatusAccepted {
		e.t.Fatalf("POST …/import = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	accepted := decodeJSON(e.t, rec)
	jobID, _ := accepted["job_id"].(string)
	if jobID == "" {
		e.t.Fatalf("the 202 carried no job_id: %v", accepted)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/v1/jobs/"+jobID {
		e.t.Errorf("Location = %q, want the job the response named", loc)
	}
	return jobID, e.waitJob(jobID, "succeeded")
}

// waitJob polls GET /api/v1/jobs/{id} until the job reaches wantState.
func (e *catalogEnv) waitJob(jobID, wantState string) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		rec := e.get(auth.RoleViewer, "/api/v1/jobs/"+jobID)
		if rec.Code != http.StatusOK {
			e.t.Fatalf("GET /api/v1/jobs/%s = %d (body %s)", jobID, rec.Code, rec.Body.String())
		}
		last = decodeJSON(e.t, rec)
		state, _ := last["state"].(string)
		if state == wantState {
			return last
		}
		if state == "failed" || state == "cancelled" {
			e.t.Fatalf("job %s ended in %q, want %s: %s", jobID, state, wantState, trimJSON(last))
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("job %s never reached %q within 60s: %s", jobID, wantState, trimJSON(last))
	return nil
}

// ── requirement 5: the whole flow ──────────────────────────────────────────

// TestCatalogFlowEndToEnd walks the sequence the milestone exists for: create a
// title, add an edition, open a draft, import a script through the durable queue,
// read and search the dialogue, edit the editable half of one line under an
// optimistic token, send it to review, publish it, derive a revision when the
// published one refuses an edit, export the result and re-import that manifest
// into a second edition. Every step is an HTTP call as a specific role, and every
// 2xx body is validated against the OpenAPI contract on the way through.
func TestCatalogFlowEndToEnd(t *testing.T) {
	e := newCatalogEnv(t)
	curator, viewer := auth.RoleCurator, auth.RoleViewer

	// 1. create the game, then replay the identical POST under the same key.
	game := e.createGame(curator, "synthetic-adventure", "Synthetic Adventure", "flow-game")
	gameID, _ := game["id"].(string)
	if !games.ValidID(gameID) {
		t.Fatalf("created game id %q is not a ULID", gameID)
	}
	replay := e.post(curator, "/api/v1/games",
		`{"slug":"synthetic-adventure","title":"Synthetic Adventure","platform":"gamecube-like"}`,
		"Idempotency-Key", "flow-game")
	if replay.Code != http.StatusCreated {
		t.Fatalf("replayed create = %d, want the original 201 (body %s)", replay.Code, replay.Body.String())
	}
	if replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Errorf("the replay did not identify itself: %v", replay.Header())
	}
	if decodeJSON(t, replay)["id"] != gameID {
		t.Errorf("the replay created a second game: %s", replay.Body.String())
	}

	// 2. the edition: region × language × disc revision.
	edition := e.createEdition(curator, gameID, "us", "en", "rev1", "flow-edition")
	editionID, _ := edition["id"].(string)

	// 3. a draft revision.
	profile, etag := e.createDraft(curator, editionID)
	profileID, _ := profile["id"].(string)
	if profile["revision_num"] != float64(1) || profile["status"] != "draft" {
		t.Fatalf("a new draft read as %v", profile)
	}
	if etag != `"1"` {
		t.Errorf("a revision 1 draft carries ETag %s, want \"1\": the token is the row version", etag)
	}

	// 4. import the synthetic script: 202 plus a job, and the job is what reports
	//    the parse. The upload is a multipart form with a `file` part, exactly the
	//    browser's shape.
	jobID, job := e.importCSV(curator, profileID)
	if job["kind"] != JobKindImportFile {
		t.Errorf("job kind = %v, want %s", job["kind"], JobKindImportFile)
	}
	result, ok := job["result"].(map[string]any)
	if !ok {
		t.Fatalf("a succeeded job carried no result object: %v", job)
	}
	if got := importedCount(result); got != 40 {
		t.Fatalf("the import reported %d messages, want the fixture's 40: %s", got, trimJSON(result))
	}
	t.Logf("import job %s succeeded: %s", jobID, trimJSON(result))

	// The import moved the row version — a content write is a write, and the
	// token an editor holds has to be the one the row carries now, not the one it
	// was handed when the draft opened (ADR-0015).
	if after := e.etag(profileID); after == etag {
		t.Fatalf("importing 40 messages left the ETag at %s: the row version did not move", etag)
	} else {
		etag = after
	}

	// 5. read the dialogue back through the list, the filters, the search and the
	//    single-row route.
	listed := e.mustOK(viewer, "/api/v1/profiles/"+profileID+"/messages?limit=10")
	if msgs, _ := listed["messages"].([]any); len(msgs) != 10 {
		t.Fatalf("limit=10 returned %v messages", listed["messages"])
	}
	cursor, _ := listed["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("a full page returned no cursor to continue it")
	}
	if listed["profile_etag"] != etag {
		t.Errorf("profile_etag = %v, want the token the profile read returned (%s)", listed["profile_etag"], etag)
	}
	paged := e.mustOK(viewer, "/api/v1/profiles/"+profileID+"/messages?limit=10&cursor="+url.QueryEscape(cursor))
	if second, _ := paged["messages"].([]any); len(second) != 10 {
		t.Fatalf("the second page returned %v messages", paged["messages"])
	}
	if overlap := intersectIDs(listed["messages"], paged["messages"]); len(overlap) > 0 {
		t.Fatalf("the cursor repeated rows across pages: %v", overlap)
	}
	signs := e.mustOK(viewer, "/api/v1/profiles/"+profileID+"/messages?kind=sign")
	for _, raw := range signs["messages"].([]any) {
		if m, _ := raw.(map[string]any); m["text_kind"] != "sign" {
			t.Fatalf("kind=sign returned a %v: %v", m["text_kind"], m)
		}
	}
	if bad := e.get(viewer, "/api/v1/profiles/"+profileID+"/messages?kind=dialgue"); bad.Code != http.StatusBadRequest {
		t.Errorf("an unknown kind filter = %d, want 400 rather than an unfiltered list", bad.Code)
	}

	hits := e.mustOK(viewer, "/api/v1/profiles/"+profileID+"/messages/search?q=harbourmaster")
	found, _ := hits["hits"].([]any)
	if len(found) == 0 {
		t.Fatalf("a search for a word in the script found nothing: %s", trimJSON(hits))
	}
	firstHit, _ := found[0].(map[string]any)
	hitMsg, _ := firstHit["message"].(map[string]any)
	messageID, _ := hitMsg["id"].(string)
	if messageID == "" {
		t.Fatalf("a search hit carried no message id: %v", firstHit)
	}

	one := e.mustOK(viewer, "/api/v1/profiles/"+profileID+"/messages/"+messageID)
	if msg, _ := one["message"].(map[string]any); msg["id"] != messageID {
		t.Fatalf("GET …/messages/%s returned %v", messageID, one["message"])
	}
	if _, ok := one["pages"].([]any); !ok {
		t.Fatal("a message detail came back without a pages array")
	}

	// 6. edit the speaking text under the token, and watch the token move.
	const rewrite = "The harbourmaster has been counting those crates twice all week."
	original, _ := hitMsg["original_text"].(string)
	edited := e.requireStatus(
		e.patch(curator, "/api/v1/profiles/"+profileID+"/messages/"+messageID,
			fmt.Sprintf(`{"speech_text":%q}`, rewrite), "If-Match", etag),
		http.StatusOK, "PATCH …/messages/{mid}")
	newETag := e.etag(profileID)
	if newETag == etag || newETag == "" {
		t.Fatalf("the write did not move the token: was %s, now %s", etag, newETag)
	}
	editedMsg, _ := edited["message"].(map[string]any)
	if editedMsg["speech_text"] != rewrite {
		t.Errorf("speech_text did not take: %v", editedMsg["speech_text"])
	}
	if editedMsg["original_text"] != original {
		t.Errorf("the edit touched original_text: %q → %q", original, editedMsg["original_text"])
	}
	// Read the row back rather than trusting the response body.
	reread := e.mustOK(viewer, "/api/v1/profiles/"+profileID+"/messages/"+messageID)
	back, _ := reread["message"].(map[string]any)
	if back["original_text"] != original || back["speech_text"] != rewrite {
		t.Fatalf("the stored row disagrees with the response: %v", back)
	}

	// A stale token is refused, and the refusal names the token to use instead.
	stale := e.patch(curator, "/api/v1/profiles/"+profileID+"/messages/"+messageID,
		`{"speech_text":"second writer"}`, "If-Match", etag)
	requireProblem(t, stale, http.StatusPreconditionFailed, codeRevisionConflict)
	if got := stale.Header().Get("ETag"); got != newETag {
		t.Errorf("the 412 carried no current ETag (%q): the client cannot recover in one round trip", got)
	}
	untokened := e.patch(curator, "/api/v1/profiles/"+profileID+"/messages/"+messageID,
		`{"speech_text":"no token"}`)
	requireProblem(t, untokened, http.StatusPreconditionRequired, codeRevisionConflict)

	// 7. review, then publish. A status write cannot stand in for publication.
	statused := e.requireStatus(
		e.post(curator, "/api/v1/profiles/"+profileID+"/status", `{"to":"review"}`, "If-Match", newETag),
		http.StatusOK, "POST …/status")
	if statused["status"] != "review" {
		t.Fatalf("status after the review move = %v", statused["status"])
	}
	reviewETag := e.etag(profileID)

	published := e.requireStatus(
		e.post(curator, "/api/v1/profiles/"+profileID+"/publish", "", "If-Match", reviewETag),
		http.StatusOK, "POST …/publish")
	if published["status"] != "published" {
		t.Fatalf("the published revision reads status %v", published["status"])
	}
	digest, _ := published["manifest_digest"].(string)
	if len(digest) != 64 {
		t.Errorf("manifest_digest = %q, want a sha256 hex digest", digest)
	}
	publishedETag := e.etag(profileID)

	// The edition's published pointer moved, which is the only way a session start
	// can find a script at all.
	if pub := e.mustOK(viewer, "/api/v1/editions/"+editionID+"/published"); pub["id"] != profileID {
		t.Errorf("GET …/published returned %v, want the revision just published", pub["id"])
	}
	actions := e.mustOK(viewer, "/api/v1/profiles/"+profileID+"/publish-actions")
	list, _ := actions["publish_actions"].([]any)
	if len(list) == 0 {
		t.Fatal("the publication left no audit entry")
	}
	if a, _ := list[0].(map[string]any); a["result"] != "succeeded" || a["actor"] != "curator" {
		t.Errorf("the audit entry reads %v, want a succeeded publish by the curator account", a)
	}

	// 8. a published revision is frozen: the same edit now answers 409.
	after := e.patch(curator, "/api/v1/profiles/"+profileID+"/messages/"+messageID,
		`{"speech_text":"editing what is shipped"}`, "If-Match", publishedETag)
	requireProblem(t, after, http.StatusConflict, codeImmutable)

	// 9. derive a revision — the documented way forward — and check the content
	//    came with it.
	child := e.requireStatus(e.post(curator, "/api/v1/profiles/"+profileID+"/revisions", ""),
		http.StatusCreated, "POST …/revisions")
	childID, _ := child["id"].(string)
	if child["revision_num"] != float64(2) || child["status"] != "draft" {
		t.Fatalf("the derived revision reads %v", child)
	}
	// A revision copy mints new row ids (docs/spec/02: the copy is a new
	// revision, not a second name for the same rows), so the line is found by the
	// identity the *source* gave it rather than by the parent's database id.
	sourceMessageID, _ := hitMsg["source_message_id"].(string)
	if sourceMessageID == "" {
		t.Fatalf("the imported line carried no source_message_id to reconcile on: %v", hitMsg)
	}
	childList := e.mustOK(viewer, "/api/v1/profiles/"+childID+"/messages?limit=200")
	var cm map[string]any
	for _, raw := range childList["messages"].([]any) {
		if m, _ := raw.(map[string]any); m["source_message_id"] == sourceMessageID {
			cm = m
		}
	}
	if cm == nil {
		t.Fatalf("the derived revision has no line %s: %v", sourceMessageID, childList["messages"])
	}
	if cm["speech_text"] != rewrite {
		t.Errorf("the derived revision lost the edit: %v", cm["speech_text"])
	}
	if cm["id"] == messageID {
		t.Error("the derived revision reused the parent's row id: a copy is a new revision, not an alias")
	}
	childETag := e.etag(childID)
	childMsgID, _ := cm["id"].(string)
	if okEdit := e.patch(curator, "/api/v1/profiles/"+childID+"/messages/"+childMsgID,
		`{"speech_text":"editable again"}`, "If-Match", childETag); okEdit.Code != http.StatusOK {
		t.Errorf("PATCH on the derived draft = %d, want 200 (body %s)", okEdit.Code, okEdit.Body.String())
	}

	// 9b. publish the derived revision too. Publication is what retires the
	//     incumbent, and the retirement is its own audit row: an edition's history
	//     has to answer "what shipped, and what did it replace" (docs/spec/02).
	e.requireStatus(e.post(curator, "/api/v1/profiles/"+childID+"/status", `{"to":"review"}`,
		"If-Match", e.etag(childID)), http.StatusOK, "child → review")
	e.requireStatus(e.post(curator, "/api/v1/profiles/"+childID+"/publish", "",
		"If-Match", e.etag(childID)), http.StatusOK, "publish the derived revision")
	history := e.mustOK(viewer, "/api/v1/profiles/"+childID+"/publish-actions")
	seenActions := map[string]bool{}
	for _, raw := range history["publish_actions"].([]any) {
		a, _ := raw.(map[string]any)
		seenActions[a["action"].(string)] = true
	}
	for _, want := range []string{"publish", "archive"} {
		if !seenActions[want] {
			t.Errorf("the edition's history has no %q entry: %v", want, history["publish_actions"])
		}
	}
	if pub := e.mustOK(viewer, "/api/v1/editions/"+editionID+"/published"); pub["id"] != childID {
		t.Errorf("after the second publication the edition points at %v, want the derived revision", pub["id"])
	}

	// 9c. the contract itself is served by the router under test.
	if doc := e.mustOK(viewer, "/api/v1/openapi.json"); doc["openapi"] != "3.1.0" {
		t.Errorf("the served document declares openapi %v", doc["openapi"])
	}

	// 10. export the published revision and re-import that manifest into a second
	//     edition of the same game.
	exported := e.get(viewer, "/api/v1/profiles/"+profileID+"/export")
	if exported.Code != http.StatusOK {
		t.Fatalf("GET …/export = %d (body %s)", exported.Code, exported.Body.String())
	}
	requireContentType(t, exported, ContentTypeJSON)
	cd := exported.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, `attachment; filename="gn-`) || !strings.HasSuffix(cd, `.gnprofile.json"`) {
		t.Errorf("Content-Disposition = %q, want an attachment named gn-<id>.gnprofile.json", cd)
	}
	manifest := exported.Body.String()
	var manifestDoc map[string]any
	if err := json.Unmarshal([]byte(manifest), &manifestDoc); err != nil {
		t.Fatalf("the download is not JSON: %v", err)
	}
	// Two fingerprints travel with an export and they are not the same number:
	// `manifest_digest` is profileio's canonical content digest of the document,
	// while `publication.store_digest` is the store's own narration digest from the
	// publication (internal/profileio/manifest.go says why the document carries
	// both). The published revision's ETag-adjacent promise is about the store's.
	if pub, _ := manifestDoc["publication"].(map[string]any); pub["store_digest"] != digest {
		t.Errorf("the export carries store_digest %v, want the published digest %s", pub["store_digest"], digest)
	}
	if self, _ := manifestDoc["manifest_digest"].(string); len(self) != 64 {
		t.Errorf("the exported document's own manifest_digest = %q, want a sha256 hex digest", self)
	}

	second := e.createEdition(curator, gameID, "eu", "en", "rev1", "flow-edition-eu")
	secondID, _ := second["id"].(string)
	clash := e.post(curator, "/api/v1/editions/"+secondID+"/profiles/import-manifest", manifest,
		"Content-Type", ContentTypeJSON)
	requireProblem(t, clash, http.StatusConflict, codeUnknownEdition)

	forced := e.post(curator, "/api/v1/editions/"+secondID+"/profiles/import-manifest?force=true", manifest,
		"Content-Type", ContentTypeJSON)
	imported := e.requireStatus(forced, http.StatusCreated, "POST …/profiles/import-manifest")
	needing, ok := imported["sources_needing_bytes"].([]any)
	if !ok {
		t.Fatalf("the import report has no sources_needing_bytes list: %v", imported)
	}
	// The bytes are on this appliance, so nothing is owed — but the member must be
	// present, because it is how a re-import onto a *different* machine reports
	// what it could not bring with it (the cross-appliance case is
	// TestImportManifestOntoAFreshAppliance).
	if len(needing) != 0 {
		t.Errorf("sources_needing_bytes = %v, want empty: this appliance holds the blob", needing)
	}
	newDraft, _ := imported["profile"].(map[string]any)
	if newDraft["edition_id"] != secondID || newDraft["status"] != "draft" {
		t.Fatalf("the manifest did not become a draft of the target edition: %v", imported)
	}
	if forced.Header().Get("Location") != "/api/v1/profiles/"+fmt.Sprint(newDraft["id"]) {
		t.Errorf("Location = %q, want the new draft", forced.Header().Get("Location"))
	}

	// 11. a viewer may read all of the above and change none of it.
	for _, tc := range []struct{ method, target, payload string }{
		{http.MethodPost, "/api/v1/games", `{"slug":"nope","title":"nope"}`},
		{http.MethodPatch, "/api/v1/games/" + gameID, `{"title":"nope"}`},
		{http.MethodPost, "/api/v1/games/" + gameID + "/editions", `{"region":"jp","language":"ja","revision":"rev1"}`},
		{http.MethodPost, "/api/v1/editions/" + editionID + "/profiles", ""},
		{http.MethodPost, "/api/v1/profiles/" + profileID + "/publish", ""},
		{http.MethodPost, "/api/v1/profiles/" + profileID + "/status", `{"to":"review"}`},
		{http.MethodPost, "/api/v1/profiles/" + profileID + "/revisions", ""},
		{http.MethodPatch, "/api/v1/profiles/" + profileID + "/messages/" + messageID, `{"speech_text":"x"}`},
		{http.MethodPost, "/api/v1/editions/" + secondID + "/profiles/import-manifest", manifest},
	} {
		rec := e.send(viewer, tc.method, tc.target, strings.NewReader(tc.payload))
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403 (body %s)", tc.method, tc.target, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, ContentTypeProblem)
	}
}

// importedCount pulls the message count out of an ImportReport result, which
// names it `stats.imported` (the report is profileio's own shape, passed through).
func importedCount(result map[string]any) int {
	if stats, ok := result["stats"].(map[string]any); ok {
		if n, ok := stats["imported"].(float64); ok {
			return int(n)
		}
	}
	if n, ok := result["messages_written"].(float64); ok {
		return int(n)
	}
	return -1
}

// ── pagination, bodies and the read-only surface ───────────────────────────

// TestPaginationIsBoundedAndCursorStable states docs/spec/08's "pagination with
// cursor and hard upper bounds" as three separate failures: a limit above the
// ceiling, a limit that is not a number, and a page walk that repeats or drops a
// row. Clamping is the bug the first one exists to catch.
func TestPaginationIsBoundedAndCursorStable(t *testing.T) {
	e := newCatalogEnv(t)
	const total = 7

	seen := map[string]bool{}
	for i := 0; i < total; i++ {
		slug := fmt.Sprintf("title-%02d", i)
		e.createGame(auth.RoleCurator, slug, "Title "+strconv.Itoa(i), "seed-"+slug)
		seen[slug] = true
	}

	for _, bad := range []string{"0", "-1", "201", "1000", "1000000000000", "all", "abc", "1.5"} {
		rec := e.get(auth.RoleViewer, "/api/v1/games?limit="+url.QueryEscape(bad))
		requireProblem(t, rec, http.StatusBadRequest, codeInvalidParameter)
		if !strings.Contains(rec.Body.String(), strconv.Itoa(maxListLimit)) {
			t.Errorf("the refusal for limit=%q does not name the bound: %s", bad, rec.Body.String())
		}
	}
	// An omitted limit is the documented default; an empty one is the same thing
	// spelled sloppily, and must not become a 400 on a browser's "?limit=".
	for _, target := range []string{"/api/v1/games", "/api/v1/games?limit="} {
		if rec := e.get(auth.RoleViewer, target); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 with the default page (body %s)", target, rec.Code, rec.Body.String())
		}
	}
	// The ceiling itself is the store's; the two must not drift.
	if maxListLimit != store.MaxListLimit || maxSearchLimit != store.MaxSearchLimit {
		t.Errorf("the API bounds (%d/%d) disagree with the store's (%d/%d)",
			maxListLimit, maxSearchLimit, store.MaxListLimit, store.MaxSearchLimit)
	}
	if rec := e.get(auth.RoleViewer, "/api/v1/games?limit=200"); rec.Code != http.StatusOK {
		t.Errorf("limit=%d (the ceiling) = %d, want 200", maxListLimit, rec.Code)
	}

	// Walk the whole library two rows at a time: every slug exactly once.
	walked := map[string]int{}
	cursor := ""
	for page := 0; page < 20; page++ {
		target := "/api/v1/games?limit=2"
		if cursor != "" {
			target += "&cursor=" + url.QueryEscape(cursor)
		}
		body := e.mustOK(auth.RoleViewer, target)
		list, _ := body["games"].([]any)
		if len(list) > 2 {
			t.Fatalf("limit=2 returned %d rows", len(list))
		}
		for _, raw := range list {
			g, _ := raw.(map[string]any)
			walked[g["slug"].(string)]++
		}
		next, _ := body["next_cursor"].(string)
		cursor = next // "" on the last page: the walk is finished, not suspended
		if cursor == "" {
			break
		}
	}
	if cursor != "" {
		t.Fatal("the walk ended on a cursor, so the last page was never reached")
	}
	for slug := range seen {
		if walked[slug] != 1 {
			t.Errorf("slug %q appeared %d times in the walk, want exactly once", slug, walked[slug])
		}
	}
	if len(walked) != total {
		t.Errorf("the walk returned %d distinct games, want %d", len(walked), total)
	}

	// A made-up cursor is a 400 rather than an empty page (docs/spec/13: an empty
	// list would claim the library is smaller than it is).
	requireProblem(t, e.get(auth.RoleViewer, "/api/v1/games?cursor=not-a-cursor"),
		http.StatusBadRequest, codeInvalidParameter)
}

// TestRequestBodyLimitsAndUnknownFields covers requirement 3: bounded bodies,
// DisallowUnknownFields, and problem+json on both.
func TestRequestBodyLimitsAndUnknownFields(t *testing.T) {
	e := newCatalogEnv(t)
	game := e.createGame(auth.RoleCurator, "limits", "Limits", "limits-game")
	gameID := game["id"].(string)

	// One byte over the JSON ceiling is a 413 naming it, not a truncation the
	// decoder would report as invalid JSON.
	oversize := `{"slug":"ok","title":"` + strings.Repeat("x", maxJSONBodyBytes) + `"}`
	rec := e.post(auth.RoleCurator, "/api/v1/games", oversize, "Idempotency-Key", "oversize")
	requireProblem(t, rec, http.StatusRequestEntityTooLarge, codeTooLarge)

	// A field the contract does not define is refused by name: the alternative is a
	// silently empty title on a screen that showed the curator a value.
	unknown := e.post(auth.RoleCurator, "/api/v1/games",
		`{"slug":"typo","title":"Typo","titel":"Typo"}`, "Idempotency-Key", "typo")
	requireProblem(t, unknown, http.StatusBadRequest, codeInvalidParameter)
	if !strings.Contains(unknown.Body.String(), "titel") {
		t.Errorf("the refusal did not name the unknown field: %s", unknown.Body.String())
	}
	if _, err := e.st.GetGameBySlug(e.ctx, "typo"); err == nil {
		t.Error("a request refused for an unknown field still created a game")
	}

	// Two JSON documents in one body is a client bug, not a batch.
	trailing := e.post(auth.RoleCurator, "/api/v1/games",
		`{"slug":"a","title":"A"} {"slug":"b","title":"B"}`, "Idempotency-Key", "trailing")
	requireProblem(t, trailing, http.StatusBadRequest, codeInvalidParameter)

	// No body at all.
	requireProblem(t, e.post(auth.RoleCurator, "/api/v1/games", "", "Idempotency-Key", "empty-body"),
		http.StatusBadRequest, codeInvalidParameter)

	// A create that takes no fields refuses them rather than ignoring them: the
	// revision number is server-owned (docs/spec/02), and silently dropping a
	// caller's `revision_num` would leave them believing they had chosen one.
	edition := e.createEdition(auth.RoleCurator, gameID, "us", "en", "rev1", "limits-edition")
	requireProblem(t, e.post(auth.RoleCurator,
		"/api/v1/editions/"+edition["id"].(string)+"/profiles", `{"revision_num":99}`),
		http.StatusBadRequest, codeInvalidParameter)

	// Text round-trips as JSON text: the API never returns markup, and what the
	// source said survives every character it contained.
	odd := `{"slug":"markup","title":"<script>alert(1)</script> \u00e9"}`
	created := e.requireStatus(
		e.post(auth.RoleCurator, "/api/v1/games", odd, "Idempotency-Key", "markup"),
		http.StatusCreated, "POST /api/v1/games with markup in a title")
	if want := "<script>alert(1)</script> é"; created["title"] != want {
		t.Fatalf("title came back as %q, want %q", created["title"], want)
	}
	requireContentType(t, e.get(auth.RoleViewer, "/api/v1/games/"+gameID), ContentTypeJSON)

	// A game addressed by slug is the same row as by id, and an unknown one is a
	// 404 with the not-found code either way.
	if bySlug := e.mustOK(auth.RoleViewer, "/api/v1/games/markup"); bySlug["id"] != created["id"] {
		t.Errorf("GET /games/markup = %v, want the row just created", bySlug)
	}
	for _, missing := range []string{"/api/v1/games/no-such-title", "/api/v1/games/" + games.NewID()} {
		requireProblem(t, e.get(auth.RoleViewer, missing), http.StatusNotFound, codeNotFound)
	}
	// A malformed id is the same 404, so the route cannot enumerate id shapes.
	requireProblem(t, e.get(auth.RoleViewer, "/api/v1/games/not-a-ULID!"), http.StatusNotFound, codeNotFound)
}

// TestGamesListRefusesAForeignQueryParameter: library-wide text search does not
// exist in this build, and an unfiltered list answered to ?q= would tell the UI
// "no title matches" when the truth is "this build does not search titles"
// (docs/spec/13). The refusal and the documented 501 belong together.
func TestGamesListRefusesAForeignQueryParameter(t *testing.T) {
	e := newCatalogEnv(t)
	e.createGame(auth.RoleCurator, "query-param", "Query Param", "query-param")
	rec := e.get(auth.RoleViewer, "/api/v1/games?q=harvest")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("GET /api/v1/games?q=… = %d, want 501 (body %s)", rec.Code, rec.Body.String())
	}
	requireProblem(t, rec, http.StatusNotImplemented, "NOT_SUPPORTED")
	// The unfiltered list still works, so the refusal is about the parameter.
	if ok := e.mustOK(auth.RoleViewer, "/api/v1/games"); len(mustList(ok["games"])) == 0 {
		t.Error("the library itself came back empty")
	}
	// And the contract says so: a client reading the document learns the bound
	// rather than discovering it at runtime.
	operation, ok := openapiOperation(t, http.MethodGet, "/api/v1/games")
	if !ok {
		t.Fatal("GET /api/v1/games is not documented")
	}
	if responses, _ := operation["responses"].(map[string]any); responses["501"] == nil {
		t.Error("the 501 answer to ?q= is not documented for GET /api/v1/games")
	}
}

// TestCatalogRoutesSaySoWhenNothingIsWired is the honest-degradation rule applied
// to the new surface: an empty game list from a process with no database reports
// the wrong fact, so it answers 503 instead (docs/spec/13).
func TestCatalogRoutesSaySoWhenNothingIsWired(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler() // no WithStore, no WithImport
	for _, target := range []string{
		"/api/v1/games",
		"/api/v1/games/" + games.NewID(),
		"/api/v1/games/" + games.NewID() + "/editions",
		"/api/v1/editions/" + games.NewID() + "/profiles",
		"/api/v1/editions/" + games.NewID() + "/published",
		"/api/v1/profiles/" + games.NewID(),
		"/api/v1/profiles/" + games.NewID() + "/messages",
		"/api/v1/profiles/" + games.NewID() + "/messages/search?q=x",
		"/api/v1/profiles/" + games.NewID() + "/characters",
		"/api/v1/profiles/" + games.NewID() + "/regions",
		"/api/v1/profiles/" + games.NewID() + "/publish-actions",
		"/api/v1/profiles/" + games.NewID() + "/export",
	} {
		rec := doReq(t, h, a.as(auth.RoleCurator, getReq(target)))
		requireProblem(t, rec, http.StatusServiceUnavailable, "SUBSYSTEM_UNAVAILABLE")
	}
	rec := doReq(t, h, a.as(auth.RoleCurator, jsonRequest(http.MethodPost, "/api/v1/games", `{"slug":"x","title":"x"}`)))
	requireProblem(t, rec, http.StatusServiceUnavailable, "SUBSYSTEM_UNAVAILABLE")
}

// TestGamePatchIsPartialAndValidated: an omitted field is left alone, which is
// what makes PATCH PATCH rather than PUT.
func TestGamePatchIsPartialAndValidated(t *testing.T) {
	e := newCatalogEnv(t)
	game := e.createGame(auth.RoleCurator, "partial", "Partial", "partial")
	id := game["id"].(string)

	got := e.requireStatus(e.patch(auth.RoleCurator, "/api/v1/games/"+id, `{"description":"added later"}`),
		http.StatusOK, "PATCH /api/v1/games/{id}")
	if got["title"] != "Partial" || got["description"] != "added later" {
		t.Fatalf("PATCH dropped a field: %v", got)
	}
	requireProblem(t, e.patch(auth.RoleCurator, "/api/v1/games/"+id, `{"slug":""}`),
		http.StatusUnprocessableEntity, codeValidationFailed)
	requireProblem(t, e.patch(auth.RoleCurator, "/api/v1/games/"+games.NewID(), `{"title":"x"}`),
		http.StatusNotFound, codeNotFound)
}

// TestEditionIdentityIsUniqueAndRejectedTwice: the unique (game, region,
// language, revision) index is what makes a re-import of the same disc reconcile
// rather than duplicate, so the second POST must be a 409 that names the fact.
func TestEditionIdentityIsUniqueAndRejectedTwice(t *testing.T) {
	e := newCatalogEnv(t)
	game := e.createGame(auth.RoleCurator, "regions", "Regions", "regions")
	id := game["id"].(string)
	e.createEdition(auth.RoleCurator, id, "us", "en", "rev1", "ed-a")

	second := e.post(auth.RoleCurator, "/api/v1/games/"+id+"/editions",
		`{"region":"us","language":"en","revision":"rev1"}`, "Idempotency-Key", "ed-b")
	requireProblem(t, second, http.StatusConflict, codeConflict)

	// A different revision of the same disc is a different edition.
	if rec := e.post(auth.RoleCurator, "/api/v1/games/"+id+"/editions",
		`{"region":"us","language":"en","revision":"rev2"}`, "Idempotency-Key", "ed-c"); rec.Code != http.StatusCreated {
		t.Errorf("a second disc revision = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	listed := e.mustOK(auth.RoleViewer, "/api/v1/games/"+id+"/editions")
	if got, _ := listed["editions"].([]any); len(got) != 2 {
		t.Fatalf("the game lists %v editions, want 2", len(got))
	}
	// An unknown game has no editions to list.
	requireProblem(t, e.get(auth.RoleViewer, "/api/v1/games/"+games.NewID()+"/editions"),
		http.StatusNotFound, codeNotFound)
	// And an unknown edition answers with docs/spec/08's own code.
	requireProblem(t, e.get(auth.RoleViewer, "/api/v1/editions/"+games.NewID()+"/profiles"),
		http.StatusNotFound, codeUnknownEdition)
}

// TestUnauthenticatedCatalogCallsAreRefused keeps the new routes inside the
// ADR-0016 gate: a viewer-level read is not a public read. The contract route is
// the one new public surface, asserted in openapi_contract_test.go.
func TestUnauthenticatedCatalogCallsAreRefused(t *testing.T) {
	e := newCatalogEnv(t)
	for _, target := range []string{"/api/v1/games", "/api/v1/games/" + games.NewID()} {
		rec := e.send(auth.RoleNone, http.MethodGet, target, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401 (body %s)", target, rec.Code, rec.Body.String())
		}
	}
}

// TestSessionCookieAndCSRFGateACatalogWrite proves the catalog routes are really
// behind the cookie+CSRF gate rather than reachable by any browser with a
// session: the same POST works with the token and is refused without it
// (docs/spec/14 "state-changing calls require CSRF protection under cookie auth").
func TestSessionCookieAndCSRFGateACatalogWrite(t *testing.T) {
	e := newCatalogEnv(t)
	cookies, csrf := login(t, e.h, "admin", testPassword)

	noToken := e.send(auth.RoleNone, http.MethodPost, "/api/v1/games",
		strings.NewReader(`{"slug":"csrf","title":"CSRF"}`),
		"Cookie", cookies, "Idempotency-Key", "csrf-1")
	if noToken.Code != http.StatusForbidden {
		t.Fatalf("a session POST with no CSRF token = %d, want 403 (body %s)", noToken.Code, noToken.Body.String())
	}
	created := e.send(auth.RoleNone, http.MethodPost, "/api/v1/games",
		strings.NewReader(`{"slug":"csrf","title":"CSRF"}`),
		"Cookie", cookies, "X-CSRF-Token", csrf, "Idempotency-Key", "csrf-1")
	e.requireStatus(created, http.StatusCreated, "a session POST with the CSRF token")
	// A read needs no token: only unsafe methods do.
	if rec := e.send(auth.RoleNone, http.MethodGet, "/api/v1/games", nil, "Cookie", cookies); rec.Code != http.StatusOK {
		t.Errorf("a session GET = %d, want 200", rec.Code)
	}
}

// ── small helpers ──────────────────────────────────────────────────────────

// intersectIDs reports ids that appear in two pages, which is the property a
// keyset cursor exists to guarantee against: a page boundary that shifts under a
// concurrent write either repeats or drops a row (docs/spec/12).
func intersectIDs(a, b any) []string {
	left := map[string]bool{}
	for _, raw := range asList(a) {
		if m, ok := raw.(map[string]any); ok {
			left[fmt.Sprint(m["id"])] = true
		}
	}
	var out []string
	for _, raw := range asList(b) {
		if m, ok := raw.(map[string]any); ok {
			if left[fmt.Sprint(m["id"])] {
				out = append(out, fmt.Sprint(m["id"]))
			}
		}
	}
	return out
}

func asList(v any) []any {
	list, _ := v.([]any)
	return list
}

// trimJSON renders a decoded body for a failure message: long enough to diagnose,
// short enough that a failing test stays readable.
func trimJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(raw) > 500 {
		return string(raw[:500]) + "…"
	}
	return string(raw)
}
