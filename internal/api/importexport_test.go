package api

// Upload → job, export → manifest, manifest → new draft (docs/spec/08 "staged
// source/profiles" + "manifest export with rights filters"; ADR-0014 for the blob
// store, ADR-0015 for never overwriting a revision, ADR-0017 for why an upload
// answers 202).

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
)

// TestImportRunsAsAJobAndReconcilesOnReUpload: the 202 names a job, the job
// reports the parse, and the second upload of the same bytes is a reconciliation
// rather than a second copy of every line (docs/spec/02 "reconcile, not
// duplicate"). The column mapping is sent explicitly here — the synthetic script's
// headers would also auto-map, and an explicit map is what the onboarding screen
// shows a reviewer committing to.
func TestImportRunsAsAJobAndReconcilesOnReUpload(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "reconcile", "reconcile")

	before := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages?limit=200")
	first, _ := before["messages"].([]any)
	if len(first) != 40 {
		t.Fatalf("the fixture revision holds %v messages, want 40", len(first))
	}

	rec := e.uploadCSV(auth.RoleCurator, profileID,
		[2]string{"format", "csv"},
		[2]string{"column_text", "line"}, [2]string{"column_id", "message_id"},
		[2]string{"column_speaker", "speaker"}, [2]string{"column_kind", "kind"},
		[2]string{"column_scene", "area"},
	)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("re-upload = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	accepted := decodeJSON(t, rec)
	if accepted["state"] != "queued" {
		t.Errorf("a fresh job read state %v, want queued", accepted["state"])
	}
	if accepted["profile_id"] != profileID {
		t.Errorf("the job names profile %v, want %s", accepted["profile_id"], profileID)
	}
	raw, sum := mustReadCSV(t)
	if accepted["sha256"] != sum {
		// Provenance is only worth anything if the address cannot be a lie: this is
		// the hash of the bytes, not of a caller's claim about them.
		t.Fatalf("sha256 = %v, want the file's own address %s", accepted["sha256"], sum)
	}
	if got, _ := accepted["size"].(float64); int64(got) != int64(len(raw)) {
		t.Errorf("size = %v, want %d bytes", accepted["size"], len(raw))
	}

	job := e.waitJob(accepted["job_id"].(string), "succeeded")
	result := resultOf(job)
	if !reusedSource(result) {
		t.Errorf("a re-upload of bytes already registered did not report reusing the source: %s", trimJSON(result))
	}
	after := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages?limit=200")
	second, _ := after["messages"].([]any)
	if len(second) != len(first) {
		t.Fatalf("the re-import changed the message count from %d to %d: it duplicated instead of reconciling",
			len(first), len(second))
	}
	if updated, _ := result["messages_updated"].(float64); updated != 40 {
		t.Errorf("the report said %v messages reconciled, want all 40: %s", result["messages_updated"], trimJSON(result))
	}
}

// TestImportAcceptsARawBodyWithQueryParameters is the scripted half of the same
// route: the bytes with a Content-Type and the mapping in the query.
func TestImportAcceptsARawBodyWithQueryParameters(t *testing.T) {
	e := newCatalogEnv(t)
	game := e.createGame(auth.RoleCurator, "raw-import", "Raw Import", "raw-import")
	edition := e.createEdition(auth.RoleCurator, game["id"].(string), "us", "en", "rev1", "raw-import-ed")
	profile, _ := e.createDraft(auth.RoleCurator, edition["id"].(string))
	profileID := profile["id"].(string)

	raw, sum := mustReadCSV(t)
	rec := e.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/import?rights=user_supplied&format=csv&column_text=line&column_id=message_id&file_name=synthetic.csv",
		bytes.NewReader(raw), "Content-Type", "text/csv")
	accepted := e.requireStatus(rec, http.StatusAccepted, "a raw-body import")
	if accepted["sha256"] != sum {
		t.Errorf("sha256 = %v, want %s", accepted["sha256"], sum)
	}
	job := e.waitJob(accepted["job_id"].(string), "succeeded")
	if n := importedCount(resultOf(job)); n != 40 {
		t.Errorf("the raw-body import wrote %d messages, want 40: %s", n, trimJSON(job["result"]))
	}
	// The provenance row names the file the caller said, reduced to something a
	// human can read and never opened from (docs/spec/14 "Filesystem/import").
	sources, err := e.st.ListSources(e.ctx, profileID)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(sources) != 1 || !strings.HasSuffix(sources[0].URI, "synthetic.csv") {
		t.Fatalf("the source row reads %v", sources)
	}
	if sources[0].Rights != games.RightsUserSupplied {
		t.Errorf("rights = %q, want the position the caller recorded", sources[0].Rights)
	}
	// A traversal-shaped name cannot survive into provenance.
	traversal := e.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/import?rights=user_supplied&format=csv&file_name=%2F..%2F%2E%2E%2Fetc%2Fpasswd",
		strings.NewReader("line\nanother\n"), "Content-Type", "text/csv")
	if traversal.Code != http.StatusAccepted {
		t.Fatalf("a second raw import = %d (body %s)", traversal.Code, traversal.Body.String())
	}
	e.waitJob(decodeJSON(t, traversal)["job_id"].(string), "succeeded")
	names := []string{}
	for _, s := range mustSources(t, e, profileID) {
		names = append(names, s.URI)
	}
	for _, n := range names {
		if strings.Contains(n, "..") {
			t.Errorf("provenance carries a traversal-shaped name: %v", names)
		}
	}
}

// TestImportRefusals covers the input surface of the route: what it will not
// queue, and with which code.
func TestImportRefusals(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "refusals", "refusals")
	target := "/api/v1/profiles/" + profileID + "/import"

	// No Content-Type at all: neither accepted shape.
	rec := e.send(auth.RoleCurator, http.MethodPost, target, strings.NewReader("a,b\n1,2\n"),
		"Content-Type", "")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("a body with no Content-Type = %d, want 415 (body %s)", rec.Code, rec.Body.String())
	}

	// Multipart with no file part: a job with no bytes to read can only fail.
	requireProblem(t, e.multipartUpload(target, map[string]string{"rights": "user_supplied"}, nil),
		http.StatusUnprocessableEntity, codeJobRequiresInput)

	// rights=unknown is refused at the door rather than stored to resurface as a
	// publish refusal months later (docs/spec/01 invariant 6).
	requireProblem(t, e.multipartUpload(target, map[string]string{"rights": "unknown"}, []byte("text\n")),
		http.StatusUnprocessableEntity, codeJobRequiresInput)

	// A format no adapter in this build can read names the formats it does have.
	bad := requireProblem(t,
		e.multipartUpload(target, map[string]string{"rights": "user_supplied", "format": "xliff"}, []byte("x\n")),
		http.StatusUnprocessableEntity, codeValidationFailed)
	if !strings.Contains(strings.Join(bad.Findings, " "), "csv") {
		t.Errorf("the refusal did not list the supported formats: %v", bad.Findings)
	}

	// Two files in one request: a source row names exactly one set of bytes.
	requireProblem(t, e.multipartUploadTwo(target, []byte("text\n"), []byte("other\n")),
		http.StatusUnprocessableEntity, codeValidationFailed)

	// An unbounded form field is not a free place to hide a megabyte of padding.
	requireProblem(t, e.multipartUpload(target, map[string]string{
		"rights": "user_supplied", "format": "csv", "language": strings.Repeat("l", 40),
	}, []byte("text\n")), http.StatusUnprocessableEntity, codeValidationFailed)

	// A malformed script fails as a *job* that says so, not as a 202 claiming the
	// parse succeeded: the report is where a reviewer reads the refusal.
	garbage := e.multipartUpload(target, map[string]string{"rights": "user_supplied", "format": "json"},
		[]byte("this is not json at all"))
	accepted := e.requireStatus(garbage, http.StatusAccepted, "an upload of unparsable bytes")
	failed := e.waitJob(accepted["job_id"].(string), "failed")
	attempts, _ := failed["attempts"].([]any)
	if len(attempts) != 1 {
		// A permanent failure is not retried into a whole budget: a malformed file
		// is malformed the same way every time (docs/spec/12 "dropped endpoint vs
		// malformed response").
		t.Errorf("an unparsable upload took %d attempts, want 1: %s", len(attempts), trimJSON(failed["attempts"]))
	}
	if len(attempts) == 1 {
		if a, _ := attempts[0].(map[string]any); a["outcome"] == "succeeded" {
			t.Errorf("the attempt history claims success: %v", a)
		}
	}
	// The revision it was aimed at is untouched, and still importable.
	if msgs := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages?limit=200"); len(mustList(msgs["messages"])) != 40 {
		t.Errorf("the failed upload changed the revision's content: %v", msgs["messages"])
	}
	if _, job := e.importCSV(auth.RoleCurator, profileID); job["state"] != "succeeded" {
		t.Errorf("the revision could not be imported into after a failed job: %v", job)
	}
}

// TestUploadOverTheSizeLimitIsRefusedWith413 is docs/spec/14's "upload sizes".
// The bytes are streamed, so the ceiling is enforced without the process ever
// holding a whole upload in memory.
func TestUploadOverTheSizeLimitIsRefusedWith413(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "toolarge", "toolarge")

	// One byte past the limit, from an infinite reader: nothing allocates 64 MiB to
	// make the point, and the store stops reading at the cap.
	stream := io.LimitReader(repeatingReader('x'), maxUploadBytes+1)
	rec := e.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/import?rights=user_supplied",
		stream, "Content-Type", "text/csv")
	requireProblem(t, rec, http.StatusRequestEntityTooLarge, codeTooLarge)

	// The staging area is left clean: a retried upload must not accumulate
	// abandoned writes (docs/spec/02 "atomic temp+fsync+rename").
	if staged, err := e.as.RecoverStaging(e.ctx); err != nil {
		t.Fatalf("RecoverStaging: %v", err)
	} else if staged != 0 {
		t.Errorf("%d staging entries survived the refusal", staged)
	}
	// An upload at the limit is accepted, so the bound is inclusive as documented.
	atLimit := io.LimitReader(repeatingReader('\n'), maxUploadBytes-1024)
	if rec := e.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/import?rights=user_supplied", atLimit, "Content-Type", "text/csv"); rec.Code != http.StatusAccepted {
		t.Errorf("an upload just under the limit = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestImportHonoursADurableIdempotencyKey: an upload's key is remembered by the
// queue rather than by this process's memory, because the work outlives the
// request that started it (ADR-0017).
func TestImportHonoursADurableIdempotencyKey(t *testing.T) {
	e := newCatalogEnv(t)
	game := e.createGame(auth.RoleCurator, "idem-import", "Idem Import", "idem-import")
	edition := e.createEdition(auth.RoleCurator, game["id"].(string), "us", "en", "rev1", "idem-import-ed")
	profile, _ := e.createDraft(auth.RoleCurator, edition["id"].(string))
	target := "/api/v1/profiles/" + profile["id"].(string) + "/import"

	raw, _ := mustReadCSV(t)
	first := e.send(auth.RoleCurator, http.MethodPost, target, bytes.NewReader(raw),
		"Content-Type", "text/csv", "Idempotency-Key", "upload-1")
	accepted := e.requireStatus(first, http.StatusAccepted, "the first upload")

	retry := e.send(auth.RoleCurator, http.MethodPost, target, bytes.NewReader(raw),
		"Content-Type", "text/csv", "Idempotency-Key", "upload-1")
	replayed := e.requireStatus(retry, http.StatusAccepted, "the retried upload")
	if replayed["job_id"] != accepted["job_id"] {
		t.Fatalf("the retry queued a second import: %v vs %v", replayed["job_id"], accepted["job_id"])
	}
	if detail, _ := replayed["detail"].(string); !strings.Contains(detail, "already queued") {
		t.Errorf("the retry did not say it had been deduplicated: %q", detail)
	}

	// Same key, different bytes: the refusal the key exists to make.
	other := e.send(auth.RoleCurator, http.MethodPost, target, strings.NewReader("line\nsomething else\n"),
		"Content-Type", "text/csv", "Idempotency-Key", "upload-1")
	requireProblem(t, other, http.StatusUnprocessableEntity, codeIdempotencyConflict)

	// The deduplicated import still runs, exactly once.
	if job := e.waitJob(accepted["job_id"].(string), "succeeded"); importedCount(resultOf(job)) != 40 {
		t.Errorf("the deduplicated import did not write the script: %v", job["result"])
	}
	// And the key is scoped to the principal: another curator's "upload-1" is a
	// different request, not a replay of this one.
	asAdmin := e.send(auth.RoleAdmin, http.MethodPost, target, bytes.NewReader(raw),
		"Content-Type", "text/csv", "Idempotency-Key", "upload-1")
	if other2 := e.requireStatus(asAdmin, http.StatusAccepted, "the same key from another principal"); other2["job_id"] == accepted["job_id"] {
		t.Error("two principals sharing a key string were told they had queued the same job")
	}
}

// TestImportIntoAPublishedRevisionIsRefused is ADR-0015 on the upload path: an
// immutable revision takes no content, whatever the upload looks like.
func TestImportIntoAPublishedRevisionIsRefused(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "published-import", "published-import")
	e.requireStatus(e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/status", `{"to":"review"}`,
		"If-Match", etag), http.StatusOK, "review")
	e.requireStatus(e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/publish", "",
		"If-Match", e.etag(profileID)), http.StatusOK, "publish")

	// The refusal comes before the bytes are read: an upload that was stored only
	// to be refused would leave provenance for a revision that cannot use it.
	requireProblem(t, e.uploadCSV(auth.RoleCurator, profileID), http.StatusConflict, codeImmutable)
	// The route refuses without a token too: immutability is not a precondition.
	requireProblem(t, e.post(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/import", "x",
		"Content-Type", "text/csv"), http.StatusConflict, codeImmutable)
}

// TestExportWithholdsTextWhoseRightsAreUnresolved is docs/spec/14's copyright
// filter seen from the wire: the pointers travel, the derived text does not, and
// the document says why in a member a review screen can render.
func TestExportWithholdsTextWhoseRightsAreUnresolved(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "export-rights", "export-rights")
	seedUnlicensedSource(t, e, profileID)

	rec := e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/export")
	manifest := e.requireStatus(rec, http.StatusOK, "GET …/export")
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, `attachment; filename="gn-`) || !strings.HasSuffix(cd, `.gnprofile.json"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if digest := rec.Header().Get("X-Profile-Sha256"); len(digest) != 64 {
		t.Errorf("X-Profile-Sha256 = %q, want the manifest's content digest", digest)
	}
	msgs := mustList(manifest["messages"])
	if len(msgs) != 41 { // the fixture's 40 plus the seeded unlicensed line
		t.Fatalf("the export carried %d messages, want 41", len(msgs))
	}
	var withheld, carried int
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if reason, _ := m["redacted_reason"].(string); reason != "" {
			withheld++
			if text, _ := m["original_text"].(string); text != "" {
				t.Errorf("a redacted message still carries its text: %v", m)
			}
			if !strings.Contains(reason, "rights") {
				t.Errorf("redacted_reason = %q, which does not name the cause", reason)
			}
			continue
		}
		carried++
	}
	if withheld != 1 {
		t.Errorf("%d messages were withheld, want the one whose source has no rights position", withheld)
	}
	if carried != 40 {
		t.Errorf("the export carried %d messages, want the 40 whose rights position is settled", carried)
	}
	// The manifest is self-verifying, which is what lets the receiving appliance
	// refuse a truncated file instead of importing half a script.
	if self, _ := manifest["manifest_digest"].(string); len(self) != 64 {
		t.Errorf("manifest_digest = %q", manifest["manifest_digest"])
	}
}

// TestExportPackNeedsAnAcknowledgementAndCarriesTheBlobs covers the .gnpack: the
// route refuses the bytes without a human saying so, and when it does send them
// they arrive under the addresses the manifest names.
func TestExportPackNeedsAnAcknowledgementAndCarriesTheBlobs(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "export-pack", "export-pack")

	requireProblem(t, e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/export?bytes=true"),
		http.StatusUnprocessableEntity, codeRightsReview)

	acknowledged := e.get(auth.RoleViewer,
		"/api/v1/profiles/"+profileID+"/export?bytes=true&acknowledge_copyright=true")
	if acknowledged.Code != http.StatusOK {
		t.Fatalf("the .gnpack export = %d (body %s)", acknowledged.Code, acknowledged.Body.String())
	}
	requireContentType(t, acknowledged, "application/x-tar")
	cd := acknowledged.Header().Get("Content-Disposition")
	if !strings.Contains(cd, ".gnpack") {
		t.Errorf("Content-Disposition = %q, want a .gnpack filename", cd)
	}
	names, blobs := readTar(t, acknowledged.Body.Bytes())
	if !containsStr(names, "manifest.json") {
		t.Errorf("the pack has no manifest entry: %v", names)
	}
	// The blob is addressed by its own hash and is byte-identical to the source, so
	// the receiving appliance needs no metadata to trust it (ADR-0014).
	if len(blobs) != 1 || blobs[0] != hashOf(mustCSVBytes(t)) {
		t.Errorf("the pack carries blobs %v, want exactly the script at its own address", blobs)
	}
}

// TestExportOfAnUnknownRevision is the 404 half of the route.
func TestExportOfAnUnknownRevision(t *testing.T) {
	e := newCatalogEnv(t)
	requireProblem(t, e.get(auth.RoleViewer, "/api/v1/profiles/"+games.NewID()+"/export"),
		http.StatusNotFound, codeNotFound)
}

// TestImportManifestOntoAFreshAppliance is the portability case the manifest
// exists for: the second machine gets the text, keeps the provenance *pointers*,
// and is told exactly which bytes it does not have (docs/spec/02 "Profile
// portability").
func TestImportManifestOntoAFreshAppliance(t *testing.T) {
	src := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, src, "portable", "portable")
	manifest := src.get(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/export").Body.Bytes()
	if !json.Valid(manifest) {
		t.Fatalf("the export is not JSON: %s", trimJSON(string(manifest)))
	}
	sourceIDs := map[string]bool{}
	for _, m := range mustList(src.mustOK(auth.RoleViewer,
		"/api/v1/profiles/"+profileID+"/messages?limit=200")["messages"]) {
		mm, _ := m.(map[string]any)
		sourceIDs[mm["id"].(string)] = true
	}

	dst := newCatalogEnv(t) // a different database *and* a different asset root
	game := dst.createGame(auth.RoleCurator, "portable", "Draft for portable", "portable-dst")
	edition := dst.createEdition(auth.RoleCurator, game["id"].(string), "us", "en", "rev1", "portable-dst-ed")

	imported := dst.send(auth.RoleCurator, http.MethodPost,
		"/api/v1/editions/"+edition["id"].(string)+"/profiles/import-manifest",
		bytes.NewReader(manifest), "Content-Type", ContentTypeJSON)
	report := dst.requireStatus(imported, http.StatusCreated, "import-manifest onto a fresh appliance")
	needing, ok := report["sources_needing_bytes"].([]any)
	if !ok {
		t.Fatalf("sources_needing_bytes = %v, want a list", report["sources_needing_bytes"])
	}
	// The manifest travelled without its bytes, so the receiving appliance must say
	// which source it cannot re-parse or re-audit — silence here is the difference
	// between "imported" and "imported and re-parseable".
	if len(needing) != 1 {
		t.Errorf("sources_needing_bytes = %v, want the one script the manifest points at", report["sources_needing_bytes"])
	}
	profile, _ := report["profile"].(map[string]any)
	newID, _ := profile["id"].(string)
	if profile["status"] != "draft" {
		t.Errorf("a re-import produced %v, want a draft (ADR-0015)", profile["status"])
	}
	list := mustList(dst.mustOK(auth.RoleViewer,
		"/api/v1/profiles/"+newID+"/messages?limit=200")["messages"])
	if len(list) != 40 {
		t.Errorf("the imported revision carries %d messages, want the exported 40", len(list))
	}
	// No row id survived the trip: a re-import is a new revision, not an alias, and
	// an id naming the same row on two machines would let a correction overlay
	// point at the wrong text.
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		if sourceIDs[m["id"].(string)] {
			t.Fatalf("the imported revision reused the exporting machine's row id %v", m["id"])
		}
	}
	if rep, _ := report["report"].(map[string]any); rep["digest"] == "" {
		t.Error("the import report names no digest, so the two machines cannot be shown agreeing")
	}
	// The voice half of the report rides on every import (ADR-0101): this
	// fixture profile carries no approved voices, so every count is zero and
	// every list empty — but the fields are present, and a manifest whose
	// voices section names references the board does not hold is the shape
	// voice_refs_missing exists to name.
	rep, _ := report["report"].(map[string]any)
	if rep == nil {
		t.Fatal("the import response carries no report object")
	}
	if rep["voices_written"] != float64(0) {
		t.Errorf("voices_written = %v, want 0 (the fixture profile has no approved voices)", rep["voices_written"])
	}
	if list, ok := rep["voice_refs_missing"].([]any); !ok || len(list) != 0 {
		t.Errorf("voice_refs_missing = %v, want an empty list", rep["voice_refs_missing"])
	}
	if list, ok := rep["voices_conflicts"].([]any); !ok || len(list) != 0 {
		t.Errorf("voices_conflicts = %v, want an empty list", rep["voices_conflicts"])
	}
}

// TestImportManifestRefusals: a body that is not a manifest, and one whose own
// digest does not match, are both refused before anything is written.
func TestImportManifestRefusals(t *testing.T) {
	e := newCatalogEnv(t)
	game := e.createGame(auth.RoleCurator, "manifest-refusals", "Manifest Refusals", "manifest-refusals")
	edition := e.createEdition(auth.RoleCurator, game["id"].(string), "us", "en", "rev1", "manifest-refusals-ed")
	target := "/api/v1/editions/" + edition["id"].(string) + "/profiles/import-manifest"

	requireProblem(t, e.post(auth.RoleCurator, target, `{"not":"a manifest"}`,
		"Content-Type", ContentTypeJSON), http.StatusUnprocessableEntity, codeValidationFailed)
	requireProblem(t, e.post(auth.RoleCurator, target, `hello`,
		"Content-Type", ContentTypeJSON), http.StatusUnprocessableEntity, codeValidationFailed)

	// A real manifest with one byte of its digest changed is a truncated or edited
	// file; importing it would build a revision nobody can verify.
	_, _, profileID, _ := newDraft(t, e, "manifest-source", "manifest-source")
	doc := e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/export").Body.String()
	tampered := strings.Replace(doc, `"manifest_digest":"`, `"manifest_digest":"0`, 1)
	if tampered == doc {
		t.Fatal("the tampering did not change the document")
	}
	requireProblem(t, e.post(auth.RoleCurator, target, tampered,
		"Content-Type", ContentTypeJSON), http.StatusUnprocessableEntity, codeValidationFailed)

	// A refused import wrote nothing: the edition still has no revisions.
	listed := e.mustOK(auth.RoleViewer, "/api/v1/editions/"+edition["id"].(string)+"/profiles")
	if got, _ := listed["profiles"].([]any); len(got) != 0 {
		t.Errorf("a refused manifest import left %d revisions behind", len(got))
	}
	// An unknown edition is the same 404 as anywhere else.
	requireProblem(t, e.post(auth.RoleCurator,
		"/api/v1/editions/"+games.NewID()+"/profiles/import-manifest", `{}`,
		"Content-Type", ContentTypeJSON), http.StatusNotFound, codeUnknownEdition)
}

// TestImportProgressIsVisibleOnTheEventStream is the other half of "the work is
// the job's": a browser watching GET /api/v1/events must see the same import the
// REST resource reports, with the same job id, replayed from the durable log
// rather than from whatever the hub happens to remember (docs/spec/08 "Event stream
// reliability").
func TestImportProgressIsVisibleOnTheEventStream(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "events", "events")
	jobID, job := e.importCSV(auth.RoleCurator, profileID)
	if job["state"] != "succeeded" {
		t.Fatalf("the fixture import did not succeed: %v", job)
	}

	// A reconnecting client's own request: replay from the beginning of the log.
	ctx, cancel := context.WithTimeout(e.ctx, 750*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	req.Header.Set("Last-Event-ID", "0")
	req.Header.Set("Authorization", "Bearer "+e.a.bearer(auth.RoleDeveloper))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	text := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/events = %d", rec.Code)
	}
	for _, want := range []string{"event: job.queued", "event: job.started", "event: job.succeeded",
		`"job_id":"` + jobID + `"`, `"kind":"` + JobKindImportFile + `"`, "id: "} {
		if !strings.Contains(text, want) {
			t.Errorf("the stream carried no %q frame; got %d bytes:\n%s", want, len(text), trimJSON(text[:min(len(text), 800)]))
		}
	}
	// The frames name the job, and the job names the revision: the event stream is a
	// pointer to the resource, not a second copy of it, which is what keeps a
	// reconnecting client from needing the stream to be lossless (docs/spec/08).
	followed := e.mustOK(auth.RoleViewer, "/api/v1/jobs/"+jobID)
	if got, _ := followed["payload"].(map[string]any); got["profile_id"] != profileID {
		t.Errorf("following the streamed job id leads to a job for %v, want %s", got["profile_id"], profileID)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

// multipartUpload sends a form with the given fields and at most one file part. A
// nil content means no file part at all.
func (e *catalogEnv) multipartUpload(target string, fields map[string]string, content []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	var payloads [][]byte
	if content != nil {
		payloads = append(payloads, content)
	}
	return e.uploadWithFiles(target, fields, payloads, "script.csv")
}

func (e *catalogEnv) multipartUploadTwo(target string, a, b []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.uploadWithFiles(target, map[string]string{"rights": "user_supplied"}, [][]byte{a, b}, "script.csv")
}

func (e *catalogEnv) uploadWithFiles(target string, fields map[string]string, files [][]byte, name string) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i, content := range files {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			e.t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := fw.Write(content); err != nil {
			e.t.Fatalf("write part %d: %v", i, err)
		}
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			e.t.Fatalf("WriteField(%s): %v", k, err)
		}
	}
	if err := mw.Close(); err != nil {
		e.t.Fatalf("close: %v", err)
	}
	return e.send(auth.RoleCurator, http.MethodPost, target, &buf, "Content-Type", mw.FormDataContentType())
}

func mustReadCSV(t *testing.T) ([]byte, string) {
	t.Helper()
	raw := mustCSVBytes(t)
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func mustCSVBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(syntheticCSVPath)
	if err != nil {
		t.Fatalf("read the synthetic script: %v", err)
	}
	return raw
}

func mustSources(t *testing.T, e *catalogEnv, profileID string) []games.Source {
	t.Helper()
	rows, err := e.st.ListSources(e.ctx, profileID)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	return rows
}

func hashOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// repeatingReader yields one byte forever, so a size test can send 64 MiB+1
// without anybody allocating it.
func repeatingReader(b byte) io.Reader {
	return readerFunc(func(p []byte) (int, error) {
		for i := range p {
			p[i] = b
		}
		return len(p), nil
	})
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// readTar lists a pack's entries and returns the address of each blob it carries.
func readTar(t *testing.T, raw []byte) (names []string, blobs []string) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names, blobs
		}
		if err != nil {
			t.Fatalf("read the pack: %v", err)
		}
		names = append(names, hdr.Name)
		if !strings.Contains(hdr.Name, "blobs/") {
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, tr); err != nil {
			t.Fatalf("read a blob: %v", err)
		}
		blobs = append(blobs, hex.EncodeToString(h.Sum(nil)))
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want || strings.HasSuffix(s, "/"+want) {
			return true
		}
	}
	return false
}

func resultOf(job map[string]any) map[string]any {
	r, _ := job["result"].(map[string]any)
	return r
}

func reusedSource(result map[string]any) bool {
	v, _ := result["reused_source"].(bool)
	return v
}

func mustList(v any) []any {
	list, _ := v.([]any)
	return list
}
