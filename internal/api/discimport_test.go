package api

// The disc-import surface (ADR-0066), tested the way the file import is: one
// real migrated store, one asset root, the windwaker reference adapter linked
// into the registry, a durable queue running the import.disc handler, and the
// synthetic disc factory from internal/games/adapter/windwaker — invented
// text only, never game material (AGENTS.md rule 3/5). Every 2xx body is
// validated against the OpenAPI contract on the way through.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/games/adapter/windwaker"
	"github.com/dbirks/game-narrator/internal/gcdisc"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/store"
)

// discLines are the synthetic script's lines, invented here for this test:
// the factory test asserts the fixture carries exactly these and no other.
var discLines = []string{
	"SYNTHETIC WIND WAKER LINE ONE",
	"the synthetic tide is out",
	"line three with a slight wave",
	"SYNTHETIC WIND WAKER LINE FOUR",
}

// newDiscEnv is one wired appliance with the disc-import surface: the
// windwaker adapter is linked next to the generic one, and WithDiscImport
// carries the allowlisted directory (empty for the unconfigured case).
func newDiscEnv(t *testing.T, importDir string) *catalogEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)

	a := newTestAuth(t)
	as, err := assets.NewLocal(filepath.Join(t.TempDir(), "assets"))
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}
	reg := adapter.NewRegistry()
	if err := generic.Register(reg); err != nil {
		t.Fatalf("generic.Register: %v", err)
	}
	if err := windwaker.Register(reg); err != nil {
		t.Fatalf("windwaker.Register: %v", err)
	}
	js := store.NewJobsStore(a.st)
	hub := events.New(events.WithStore(store.NewEventStore(a.st)), events.WithLogger(discardLogger))
	runner, err := jobs.NewRunner(js, hub, discardLogger, jobs.WithPollInterval(2*time.Millisecond))
	if err != nil {
		t.Fatalf("jobs.NewRunner: %v", err)
	}
	if err := runner.Register(JobKindImportDisc, ImportDiscJobHandler(a.st, as, reg),
		jobs.RegisterOpts{Concurrency: 1, Resumable: true, Timeout: time.Minute}); err != nil {
		t.Fatalf("register %s: %v", JobKindImportDisc, err)
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
		importDir: importDir,
		h: a.handler(WithStore(a.st), WithJobs(js), WithEvents(hub), WithImport(as, reg, runner),
			WithDiscImport(importDir)),
	}
}

// writeSynthDisc places one synthetic disc image (invented text, GZLE01) in
// dir under name and returns the path.
func writeSynthDisc(t *testing.T, dir, name string) string {
	t.Helper()
	img, err := windwaker.SynthDiscImage("GZLE01", discLines)
	if err != nil {
		t.Fatalf("SynthDiscImage: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatalf("write synthetic image: %v", err)
	}
	return p
}

func newDiscDraft(t *testing.T, e *catalogEnv) (profileID, editionID, gameID string) {
	t.Helper()
	game := e.createGame(auth.RoleCurator, "disc-import", "Disc Import", "disc-import")
	gameID = game["id"].(string)
	edition := e.createEdition(auth.RoleCurator, gameID, "us", "en", "disc-1", "disc-import-edition")
	editionID = edition["id"].(string)
	created, _ := e.createDraft(auth.RoleCurator, editionID)
	profileID = created["id"].(string)
	return profileID, editionID, gameID
}

// TestDiscImportDirShapeEndToEnd walks the whole dir-shape path: the image
// list names the allowlisted file, the 202 enqueues an import.disc job, the
// job reads the image in place and reports the parse, the second import of the
// same image reconciles by content hash, and the image's own path never
// appears in the server log.
func TestDiscImportDirShapeEndToEnd(t *testing.T) {
	importDir := t.TempDir()
	writeSynthDisc(t, importDir, "synth-windwaker.gcm")

	buf := &bytes.Buffer{}
	e := newDiscEnv(t, importDir)
	// Re-point the server's logger at a buffer: the "path never logged" clause
	// is only checkable if the log is captured.
	log := slog.New(slog.NewTextHandler(buf, nil))
	e.h = newDiscEnvLog(e, log)

	// The list is curator-tier (the /api/v1/import/* grant), viewer is not.
	if rec := e.get(auth.RoleViewer, "/api/v1/import/disc/images"); rec.Code != http.StatusForbidden {
		t.Errorf("GET images as viewer = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	list := e.mustOK(auth.RoleCurator, "/api/v1/import/disc/images")
	if list["configured"] != true {
		t.Errorf("the configured directory reported configured=%v", list["configured"])
	}
	images := mustList(list["images"])
	if len(images) != 1 {
		t.Fatalf("the list holds %v files, want the one synthetic disc", images)
	}
	first := images[0].(map[string]any)
	if first["name"] != "synth-windwaker.gcm" {
		t.Errorf("the list names %v, want the basename", first["name"])
	}
	if !strings.HasPrefix(fmt.Sprint(list["adapter"]), "windwaker") {
		t.Errorf("the list did not name the disc-capable adapter: %v", list["adapter"])
	}
	if strings.Contains(rec_body(list), importDir) {
		t.Errorf("the list answered with the directory's own path: %v", list)
	}

	profileID, _, _ := newDiscDraft(t, e)

	// A draft only: the immutable shape is covered by the 409 test below.
	rec := e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/import/disc",
		`{"image":"synth-windwaker.gcm"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST import/disc = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	accepted := decodeJSON(t, rec)
	if accepted["job_kind"] != JobKindImportDisc {
		t.Errorf("the 202 enqueued kind %v, want %s", accepted["job_kind"], JobKindImportDisc)
	}
	if accepted["source"] != discSourceDir {
		t.Errorf("the 202 named source %v, want dir", accepted["source"])
	}
	job := e.waitJob(accepted["job_id"].(string), "succeeded")
	result := resultOf(job)
	if inserted, _ := result["messages_inserted"].(float64); int(inserted) != len(discLines) {
		t.Errorf("the report imported %v messages, want %d (result %s)",
			result["messages_inserted"], len(discLines), trimJSON(result))
	}
	if result["game_id"] != "GZLE01" {
		t.Errorf("the report names game id %v, want GZLE01", result["game_id"])
	}
	if result["disc_region"] != gcdisc.RegionOfGameID("GZLE01") {
		t.Errorf("the report names region %v, want the game id's own mapping", result["disc_region"])
	}
	if sha, _ := result["image_sha256"].(string); len(sha) != 64 {
		t.Errorf("the report's image hash is %q, want a SHA-256 hex", sha)
	}
	if reusedSource(result) {
		t.Errorf("the first import reported reusing a source: %s", trimJSON(result))
	}

	// The messages are on the profile: the import reached the store, not just
	// the report.
	msgs := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages?limit=100")
	if got := len(mustList(msgs["messages"])); got != len(discLines) {
		t.Errorf("the profile holds %d messages, want %d", got, len(discLines))
	}

	// Re-importing the same image: the bytes were already registered, so this
	// is a reconciliation, not a second copy of every line.
	rec2 := e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/import/disc",
		`{"image":"synth-windwaker.gcm"}`)
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("re-import = %d, want 202 (body %s)", rec2.Code, rec2.Body.String())
	}
	job2 := e.waitJob(decodeJSON(t, rec2)["job_id"].(string), "succeeded")
	if !reusedSource(resultOf(job2)) {
		t.Errorf("the re-import did not report reusing the source: %s", trimJSON(resultOf(job2)))
	}
	msgs2 := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages?limit=100")
	if got := len(mustList(msgs2["messages"])); got != len(discLines) {
		t.Errorf("the re-import left %d messages, want %d: it duplicated instead of reconciling",
			got, len(discLines))
	}

	// The path-never-logged clause: the server's log (handler, hub and the
	// enqueue's audit fields) must not carry the operator's directory.
	if strings.Contains(buf.String(), importDir) {
		t.Errorf("the server log carries the import directory's path:\n%s", buf.String())
	}
}

// newDiscEnvLog rebuilds the env's handler with the captured logger, keeping
// the same store, queue and wiring: only the log sink changes.
func newDiscEnvLog(e *catalogEnv, log *slog.Logger) http.Handler {
	return New(testBuild, testStarted, log,
		WithAuth(e.a.svc, e.a.repo),
		WithStore(e.st), WithJobs(e.js), WithEvents(e.hub),
		WithImport(e.as, e.reg, e.runner),
		WithDiscImport(e.importDir),
	).Handler()
}

func rec_body(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestDiscImportRefusesPathShapedNames is the traversal matrix for the dir
// shape: a crafted basename must not reach outside the allowlisted directory,
// and the refusal must not name the operator's path (AGENTS.md rule 5).
func TestDiscImportRefusesPathShapedNames(t *testing.T) {
	importDir := t.TempDir()
	writeSynthDisc(t, importDir, "synth-windwaker.gcm")
	e := newDiscEnv(t, importDir)
	profileID, _, _ := newDiscDraft(t, e)

	cases := []struct {
		name   string
		want   int
		slug   string
		leakOK bool // the refusal may echo the crafted name; never the directory
	}{
		{"../../etc/passwd", http.StatusBadRequest, "invalid-image-name", true},
		{"..", http.StatusBadRequest, "invalid-image-name", true},
		{"sub/synth-windwaker.gcm", http.StatusBadRequest, "invalid-image-name", true},
		{"/etc/passwd", http.StatusBadRequest, "invalid-image-name", true},
		{"synth-windwaker.gcm\x00", http.StatusBadRequest, "invalid-image-name", true},
		{"..\\..\\windows", http.StatusBadRequest, "invalid-image-name", true},
		{"missing.gcm", http.StatusBadRequest, "invalid-image-name", false},
		{"", http.StatusUnprocessableEntity, "image-required", false},
	}
	for _, tc := range cases {
		rec := e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/import/disc",
			`{"image":`+jsonQuote(tc.name)+`}`)
		if rec.Code != tc.want {
			t.Errorf("image %q = %d, want %d (body %s)", tc.name, rec.Code, tc.want, rec.Body.String())
			continue
		}
		body := rec.Body.String()
		if strings.Contains(body, importDir) {
			t.Errorf("the refusal for %q carries the import directory's path: %s", tc.name, body)
		}
		if tc.slug != "" && !strings.Contains(body, tc.slug) {
			t.Errorf("the refusal for %q lacks the slug %q: %s", tc.name, tc.slug, body)
		}
	}

	// The operator's own symlink is how the image reaches the allowlist on a
	// machine that keeps it elsewhere: it is listed and it imports.
	other := t.TempDir()
	writeSynthDisc(t, other, "elsewhere.gcm")
	if err := os.Symlink(filepath.Join(other, "elsewhere.gcm"), filepath.Join(importDir, "linked.gcm")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	list := e.mustOK(auth.RoleCurator, "/api/v1/import/disc/images")
	names := []string{}
	for _, img := range mustList(list["images"]) {
		names = append(names, img.(map[string]any)["name"].(string))
	}
	if !containsName(names, "linked.gcm") {
		t.Errorf("the symlinked image is not listed: %v", names)
	}
	rec := e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/import/disc", `{"image":"linked.gcm"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the symlinked image was not accepted: %d (body %s)", rec.Code, rec.Body.String())
	}
	job := e.waitJob(decodeJSON(t, rec)["job_id"].(string), "succeeded")
	if inserted, _ := resultOf(job)["messages_inserted"].(float64); int(inserted) != len(discLines) {
		t.Errorf("the symlinked import inserted %v messages, want %d", inserted, len(discLines))
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func containsName(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestDiscImportUnconfiguredDirectoryIsTheUploadPath is the honest degraded
// mode: no directory configured, the images route answers 200 with
// configured:false (a state, not a fault — the overview panel queries it on
// every draft), the dir shape answers 503, and the upload shape still works
// end to end.
func TestDiscImportUnconfiguredDirectoryIsTheUploadPath(t *testing.T) {
	e := newDiscEnv(t, "")
	rec := e.get(auth.RoleCurator, "/api/v1/import/disc/images")
	if rec.Code != http.StatusOK {
		t.Fatalf("unconfigured images = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	list := decodeJSON(t, rec)
	if list["configured"] != false {
		t.Errorf("the unconfigured directory reported configured=%v, want false (body %s)", list["configured"], rec.Body.String())
	}
	if imgs := list["images"]; imgs == nil || (len(imgs.([]any)) != 0) {
		t.Errorf("the unconfigured directory listed %v images, want none", imgs)
	}
	profileID, _, _ := newDiscDraft(t, e)
	rec = e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/import/disc", `{"image":"x.gcm"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured dir shape = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}

	// The upload shape: the synthetic image's bytes as a multipart file part.
	img, err := windwaker.SynthDiscImage("GZLE01", discLines)
	if err != nil {
		t.Fatalf("SynthDiscImage: %v", err)
	}
	rec = e.uploadDisc(auth.RoleCurator, profileID, "synth-windwaker.gcm", img)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("upload shape = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	accepted := decodeJSON(t, rec)
	if accepted["source"] != discSourceUpload {
		t.Errorf("the 202 named source %v, want upload", accepted["source"])
	}
	sum := sha256.Sum256(img)
	if accepted["sha256"] != hex.EncodeToString(sum[:]) {
		t.Errorf("the 202's sha256 = %v, want the image's own hash", accepted["sha256"])
	}
	job := e.waitJob(accepted["job_id"].(string), "succeeded")
	result := resultOf(job)
	if inserted, _ := result["messages_inserted"].(float64); int(inserted) != len(discLines) {
		t.Errorf("the upload import inserted %v messages, want %d (result %s)",
			result["messages_inserted"], len(discLines), trimJSON(result))
	}
	if result["image_sha256"] != hex.EncodeToString(sum[:]) {
		t.Errorf("the report's image hash %v, want the upload's own hash", result["image_sha256"])
	}
}

// uploadDisc posts the upload shape: one multipart request with a `file` part.
func (e *catalogEnv) uploadDisc(role auth.Role, profileID, name string, img []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		e.t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(img); err != nil {
		e.t.Fatalf("write part: %v", err)
	}
	mw.Close()
	return e.send(role, http.MethodPost, "/api/v1/profiles/"+profileID+"/import/disc", &buf,
		"Content-Type", mw.FormDataContentType())
}

// TestDiscImportUploadSizeCapRefusesOverTheSharedCap: the upload shape streams
// under the shared 64 MiB cap and answers 413 rather than discovering a full
// disk mid-stream (docs/spec/14).
func TestDiscImportUploadSizeCapRefusesOverTheSharedCap(t *testing.T) {
	e := newDiscEnv(t, "")
	profileID, _, _ := newDiscDraft(t, e)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "big.gcm")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	big := make([]byte, maxUploadBytes+1)
	if _, err := part.Write(big); err != nil {
		t.Fatalf("write part: %v", err)
	}
	mw.Close()
	rec := e.send(auth.RoleCurator, http.MethodPost, "/api/v1/profiles/"+profileID+"/import/disc", &buf,
		"Content-Type", mw.FormDataContentType())
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an over-cap upload = %d, want 413 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestDiscImportRefusesAnImmutableRevision is the same 409 the file import
// answers: content only enters a mutable revision, and the refusal says so.
// The revision is taken through its real lifecycle — import, review,
// publish — because publication is what makes it immutable.
func TestDiscImportRefusesAnImmutableRevision(t *testing.T) {
	importDir := t.TempDir()
	writeSynthDisc(t, importDir, "synth-windwaker.gcm")
	e := newDiscEnv(t, importDir)
	profileID, _, _ := newDiscDraft(t, e)

	// The first import fills the draft, so publication has content to gate.
	rec := e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/import/disc",
		`{"image":"synth-windwaker.gcm"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the first import = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	e.waitJob(decodeJSON(t, rec)["job_id"].(string), "succeeded")

	// draft → review → published: the immutable state.
	rec = e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/status", `{"to":"review"}`,
		"If-Match", e.etag(profileID))
	if rec.Code != http.StatusOK {
		t.Fatalf("moving the draft to review = %d (body %s)", rec.Code, rec.Body.String())
	}
	rec = e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/publish", "",
		"If-Match", e.etag(profileID))
	if rec.Code != http.StatusOK {
		t.Fatalf("publishing the review revision = %d (body %s)", rec.Code, rec.Body.String())
	}

	rec = e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/import/disc",
		`{"image":"synth-windwaker.gcm"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("import into a published revision = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "draft") {
		t.Errorf("the 409 does not say content needs a draft: %s", rec.Body.String())
	}
}

// TestDiscImportRefusesADiscTheAdapterDoesNotOwn is the provenance refusal
// (AGENTS.md rule 6): a disc whose game id the reference adapter does not own
// fails the job permanently rather than being imported under the wrong
// adapter's rules, and the job's error says which id was refused.
func TestDiscImportRefusesADiscTheAdapterDoesNotOwn(t *testing.T) {
	importDir := t.TempDir()
	arc := gcdisc.AssembleRARC(map[string][]byte{
		"bmg/bmg0.bmg": []byte("not a bmg the adapter will ever read"),
	})
	img := gcdisc.SynthGCMImage("XXXX01", 0x2000,
		[]gcdisc.SynthEntry{
			{IsDir: true, End: 4},
			{IsDir: true, Name: "res", Parent: 0, End: 4},
			{IsDir: true, Name: "Msg", Parent: 1, End: 4},
			{Name: "bmgres.arc", Off: 0x3000, Size: int64(len(arc))},
		},
		map[string][]byte{"bmgres.arc": arc})
	if err := os.WriteFile(filepath.Join(importDir, "foreign.gcm"), img, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	e := newDiscEnv(t, importDir)
	profileID, _, _ := newDiscDraft(t, e)
	rec := e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/import/disc", `{"image":"foreign.gcm"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the enqueue does not know the disc is wrong until the adapter reads it: got %d (body %s)",
			rec.Code, rec.Body.String())
	}
	job := e.waitForTerminal(decodeJSON(t, rec)["job_id"].(string))
	if job["state"] != "failed" {
		t.Fatalf("the foreign disc job = %v, want failed (result %s)", job["state"], trimJSON(job))
	}
	result := resultOf(job)
	errText := fmt.Sprint(result["error"], result["message"])
	if !strings.Contains(errText, "XXXX01") || !strings.Contains(errText, "GZL") {
		t.Errorf("the refusal does not name the foreign id and the owned prefix: %s", errText)
	}
	// Nothing reached the profile: the refusal is before any write.
	msgs := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages?limit=100")
	if got := len(mustList(msgs["messages"])); got != 0 {
		t.Errorf("the refused disc left %d messages on the profile", got)
	}
}

// waitForTerminal is waitJob for the cases where the job is supposed to fail:
// it returns the terminal row instead of treating "failed" as a harness bug.
func (e *catalogEnv) waitForTerminal(jobID string) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		rec := e.get(auth.RoleViewer, "/api/v1/jobs/"+jobID)
		if rec.Code != http.StatusOK {
			e.t.Fatalf("GET /api/v1/jobs/%s = %d (body %s)", jobID, rec.Code, rec.Body.String())
		}
		last = decodeJSON(e.t, rec)
		if state, _ := last["state"].(string); isTerminalJobState(state) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("job %s never reached a terminal state within 60s: %s", jobID, trimJSON(last))
	return nil
}

func isTerminalJobState(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled":
		return true
	}
	return false
}

// TestDiscImportAuthz is the role matrix: the list and the enqueue are
// curator-tier, the viewer reads the job but cannot start one, and nobody
// unauthenticated reaches either (the scope table, not this handler, owns the
// 403/401 — ADR-0016).
func TestDiscImportAuthz(t *testing.T) {
	importDir := t.TempDir()
	writeSynthDisc(t, importDir, "synth-windwaker.gcm")
	e := newDiscEnv(t, importDir)
	profileID, _, _ := newDiscDraft(t, e)

	if rec := e.get(auth.RoleNone, "/api/v1/import/disc/images"); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated images list = %d, want 401", rec.Code)
	}
	if rec := e.post(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/import/disc",
		`{"image":"synth-windwaker.gcm"}`); rec.Code != http.StatusForbidden {
		t.Errorf("viewer enqueue = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := e.post(auth.RoleNone, "/api/v1/profiles/"+profileID+"/import/disc",
		`{"image":"synth-windwaker.gcm"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated enqueue = %d, want 401", rec.Code)
	}
}
