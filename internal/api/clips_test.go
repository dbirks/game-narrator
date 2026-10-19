package api

// The managed replay clip store's HTTP surface (ADR-0073): upload a tar/zip of
// the replay layout into the asset store under its content hash, extract it
// confined into a staging directory, validate the layout, install it under the
// managed store directory the runtime always treats as a replay root, list the
// store, and delete one clip by id.
//
// The tests run the real extraction, the real layout validation and the real
// index reconciliation against a temporary store — the no-simulation-theater
// rule applied to the install path: a zip-slip archive that "installs" green in
// a mock is the exact incident this surface exists to prevent. The contract
// clause (c) bodies (the 201 and the 200) land in recordContractResponse like
// every other route's, so a response that stops matching the OpenAPI document
// fails here.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/clips"
)

// clipsEnv is the clip surface as serve wires it: the asset store for the
// provenance, the managed directory for the installs, and the auth harness for
// the role ladder.
type clipsEnv struct {
	t   *testing.T
	a   *testAuth
	as  assets.Store
	dir string
	h   http.Handler
}

func newClipsEnv(t *testing.T) *clipsEnv {
	t.Helper()
	a := newTestAuth(t)
	as, err := assets.NewLocal(filepath.Join(t.TempDir(), "assets"))
	if err != nil {
		t.Fatalf("assets.NewLocal: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "replay-clips")
	return &clipsEnv{
		t: t, a: a, as: as, dir: dir,
		h: a.handler(WithClips(as, dir)),
	}
}

func (e *clipsEnv) send(role auth.Role, method, target string, body []byte, kv ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
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

// framePNG is one small valid PNG frame: the layout validation and the replay
// source both decode, so a test frame that is not really an image would test
// the wrong thing. png.Encode of a fresh RGBA cannot fail, so the helper is
// t-less; the frames every layout shares.
func framePNG() []byte {
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	if err := png.Encode(&buf, img); err != nil {
		panic(err) // unreachable
	}
	return buf.Bytes()
}

// manifestJSON is the clip layout's timestamps.json for nFrames frames.
func manifestJSON(nFrames int) []byte {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < nFrames; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"file":"frame-%03d.png","ts_ns":%d}`, i+1, int64(1000000000*(i+1)))
	}
	sb.WriteByte(']')
	return []byte(sb.String())
}

// clipFiles is one clip layout's flat file map: the manifest plus its frames.
// The frames are all the same valid PNG, which the layout rules do not forbid
// (the timestamps differ); framePNG once is a frame the decoder really decodes.
func clipFiles(nFrames int) map[string][]byte {
	out := map[string][]byte{"timestamps.json": manifestJSON(nFrames)}
	for i := 1; i <= nFrames; i++ {
		out[fmt.Sprintf("frame-%03d.png", i)] = framePNG()
	}
	return out
}

// tarArchive packs files (path → bytes) into a tar; under, when non-empty,
// nests them under one top-level directory (the layout-wrapped shape).
func tarArchive(t *testing.T, files map[string][]byte, under string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		full := name
		if under != "" {
			full = under + "/" + name
		}
		if err := tw.WriteHeader(&tar.Header{Name: full, Mode: 0o600, Size: int64(len(content))}); err != nil {
			t.Fatalf("tar header %s: %v", full, err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("tar body %s: %v", full, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

// zipArchive is tarArchive's zip twin.
func zipArchive(t *testing.T, files map[string][]byte, under string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		full := name
		if under != "" {
			full = under + "/" + name
		}
		w, err := zw.Create(full)
		if err != nil {
			t.Fatalf("zip create %s: %v", full, err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("zip write %s: %v", full, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// uploadClip posts one archive as multipart/form-data with the optional name.
func (e *clipsEnv) uploadClip(t *testing.T, role auth.Role, archive []byte, fileName, name string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if name != "" {
		if err := mw.WriteField("name", name); err != nil {
			t.Fatalf("write name: %v", err)
		}
	}
	fw, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := fw.Write(archive); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return e.send(role, http.MethodPost, "/api/v1/replay/clips", buf.Bytes(),
		"Content-Type", mw.FormDataContentType())
}

type clipDoc struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Frames        int    `json:"frames"`
	ArchiveSHA256 string `json:"archive_sha256"`
	ArchiveSize   int64  `json:"archive_size"`
	ArchiveFormat string `json:"archive_format"`
	Detail        string `json:"detail"`
}

type clipListDoc struct {
	Clips []struct {
		ID        string `json:"id"`
		Frames    int    `json:"frames"`
		Missing   bool   `json:"missing"`
		CreatedBy string `json:"created_by"`
	} `json:"clips"`
}

// TestClipInstallListDelete is the product walk the sustained device script
// used to do by hand: upload the recording, see it listed, replay it by id,
// and remove it when the evidence is served.
func TestClipInstallListDelete(t *testing.T) {
	e := newClipsEnv(t)
	files := clipFiles(3)
	archive := tarArchive(t, files, "dialogue-advance-001")
	sum := sha256.Sum256(archive)

	rec := e.uploadClip(t, auth.RoleDeveloper, archive, "dialogue-advance-001.tar", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", rec.Code, rec.Body.String())
	}
	var up clipDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatalf("201 body: %v", err)
	}
	if up.ID != "dialogue-advance-001" {
		t.Fatalf("id = %q, want the archive's top-level directory name", up.ID)
	}
	if up.ArchiveSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("archive_sha256 = %s, want the upload's content hash", up.ArchiveSHA256)
	}
	if up.Frames != 3 {
		t.Fatalf("frames = %d, want 3 (the manifest's count)", up.Frames)
	}
	// The installed layout is where the runtime reads it from.
	if _, err := os.Stat(filepath.Join(e.dir, "dialogue-advance-001", "timestamps.json")); err != nil {
		t.Fatalf("the clip did not install under the store: %v", err)
	}
	// The provenance row is recorded.
	idx, err := clips.Load(e.dir)
	if err != nil {
		t.Fatalf("clip index: %v", err)
	}
	if rec0, ok := idx.Find("dialogue-advance-001"); !ok || rec0.SHA256 != up.ArchiveSHA256 {
		t.Fatalf("index row = %+v, want the uploaded hash", rec0)
	}
	// The archive is in the asset store under its hash: the provenance an
	// auditor re-verifies.
	if _, _, err := e.as.Get(t.Context(), up.ArchiveSHA256); err != nil {
		t.Fatalf("the committed archive is not retrievable: %v", err)
	}
	// No staging directory survived the install.
	if n, err := clips.PruneStaging(e.dir); err != nil || n != 0 {
		t.Fatalf("staging after install: removed %d, err %v; want none left", n, err)
	}

	// The list shows the clip, and nothing in the answer carries a host path.
	rec = e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/replay/clips", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var list clipListDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if len(list.Clips) != 1 || list.Clips[0].ID != "dialogue-advance-001" || list.Clips[0].Frames != 3 {
		t.Fatalf("list = %+v, want the one installed clip", list.Clips)
	}
	if strings.Contains(rec.Body.String(), e.dir) {
		t.Fatal("the list answered with the store's host path; the id is the whole address")
	}
	if strings.Contains(rec.Body.String(), "replay-clips/") {
		t.Fatal("the list answered with a store-relative path")
	}

	// Delete removes the directory and the row; a second delete 404s.
	rec = e.send(auth.RoleDeveloper, http.MethodDelete, "/api/v1/replay/clips/dialogue-advance-001", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(e.dir, "dialogue-advance-001")); !os.IsNotExist(err) {
		t.Fatal("the clip directory survived its delete")
	}
	rec = e.send(auth.RoleDeveloper, http.MethodDelete, "/api/v1/replay/clips/dialogue-advance-001", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}
	rec = e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/replay/clips", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list after delete = %d", rec.Code)
	}
	var after clipListDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Clips) != 0 {
		t.Fatalf("the list still shows the deleted clip: %+v", after.Clips)
	}
}

// TestClipInstallAcceptsZip is the container the upload path must take as
// readily as tar: the same layout, the same id, the same provenance.
func TestClipInstallAcceptsZip(t *testing.T) {
	e := newClipsEnv(t)
	archive := zipArchive(t, clipFiles(2), "zip-clip")
	rec := e.uploadClip(t, auth.RoleDeveloper, archive, "zip-clip.zip", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("zip upload = %d: %s", rec.Code, rec.Body.String())
	}
	var up clipDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if up.ArchiveFormat != "zip" || up.ID != "zip-clip" {
		t.Fatalf("201 = %+v, want the zip under its top-level name", up)
	}
}

// TestClipInstallNameAndCollision: the name field is the id's first source,
// and a second install of the same id is a 409 — never a silent rename, which
// is how a clip gets replaced by the wrong recording.
func TestClipInstallNameAndCollision(t *testing.T) {
	e := newClipsEnv(t)
	archive := tarArchive(t, clipFiles(1), "some-recording")
	rec := e.uploadClip(t, auth.RoleDeveloper, archive, "some-recording.tar", "my-clip")
	if rec.Code != http.StatusCreated {
		t.Fatalf("named upload = %d: %s", rec.Code, rec.Body.String())
	}
	var up clipDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if up.ID != "my-clip" || up.Name != "my-clip" {
		t.Fatalf("201 = %+v, want the caller's name as id", up)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "my-clip", "timestamps.json")); err != nil {
		t.Fatalf("the clip did not install under its name: %v", err)
	}

	// The same id again is a conflict, with the store untouched.
	rec = e.uploadClip(t, auth.RoleDeveloper, archive, "other-name.tar", "my-clip")
	if rec.Code != http.StatusConflict {
		t.Fatalf("collision = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// TestClipUploadRefusals is the security table: each malformed or hostile
// archive is refused with a named 4xx and installs nothing.
func TestClipUploadRefusals(t *testing.T) {
	cases := []struct {
		name    string
		archive []byte
		file    string
		status  int
	}{
		{"neither tar nor zip", []byte("definitely not an archive"), "blob.bin", http.StatusBadRequest},
		{"gzip wrapped tar is refused", gzipBytes(t, tarArchive(t, clipFiles(1), "")), "clip.tgz", http.StatusBadRequest},
		{"no manifest at root", tarArchive(t, map[string][]byte{"frame-001.png": framePNG()}, ""), "nomani.tar", http.StatusBadRequest},
		{"manifest names a missing frame", tarArchive(t, map[string][]byte{"timestamps.json": manifestJSON(2)}, ""), "missing.tar", http.StatusBadRequest},
		{"manifest entry traverses", tarArchive(t, map[string][]byte{
			"timestamps.json": []byte(`[{"file":"../etc/passwd","ts_ns":1}]`),
		}, ""), "traverse-manifest.tar", http.StatusBadRequest},
		{"zip slip entry", zipArchive(t, map[string][]byte{"evil.txt": []byte("x")}, "../"), "slip.zip", http.StatusBadRequest},
		{"tar entry escapes", escapeTar(t), "escape.tar", http.StatusBadRequest},
		{"tar symlink entry", symlinkTar(t), "link.tar", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newClipsEnv(t)
			before, _ := dirCount(e.dir)
			rec := e.uploadClip(t, auth.RoleDeveloper, tc.archive, tc.file, "")
			if rec.Code != tc.status {
				t.Fatalf("upload = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			after, _ := dirCount(e.dir)
			if after != before {
				t.Fatalf("a refused upload left %d store directories behind", after-before)
			}
		})
	}

	// A name that normalizes to nothing is a client error, not a guess. A
	// traversal-shaped name is a different case: it normalizes to the safe slug
	// it contains and is admitted (the id still cannot carry a separator).
	t.Run("bad name", func(t *testing.T) {
		e := newClipsEnv(t)
		rec := e.uploadClip(t, auth.RoleDeveloper, tarArchive(t, clipFiles(1), ""), "x.tar", "///")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("bad name = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})
	// The body must be multipart at all.
	t.Run("not multipart", func(t *testing.T) {
		e := newClipsEnv(t)
		rec := e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/replay/clips",
			[]byte(`{"file":"x"}`), "Content-Type", ContentTypeJSON)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("json body = %d, want 415: %s", rec.Code, rec.Body.String())
		}
	})
}

// escapeTar is an archive whose entry name climbs out of the root.
func escapeTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "../outside.txt", Mode: 0o600, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// symlinkTar carries a symlink entry the extractor must refuse outright.
func symlinkTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "timestamps.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gzipBytes wraps data in a gzip stream: the shape .tgz uploads arrive in.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.NoCompression)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if _, err := gz.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// dirCount counts the top-level entries of the store directory (0 when it does
// not exist yet): the assertion that a refused upload installs nothing.
func dirCount(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

// TestClipListReconcilesTheStore: a directory the index lost is adopted on the
// next list (created_by recovered), and an index row whose directory is gone
// is listed as missing — the list shows the appliance's state, including its
// losses, and the delete answers the cleanup.
func TestClipListReconcilesTheStore(t *testing.T) {
	e := newClipsEnv(t)
	// Install one clip through the API…
	rec := e.uploadClip(t, auth.RoleDeveloper, tarArchive(t, clipFiles(2), "kept"), "kept.tar", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup upload = %d: %s", rec.Code, rec.Body.String())
	}
	// …and place a second directory by hand, without a provenance row.
	adopted := filepath.Join(e.dir, "adopted")
	if err := os.MkdirAll(filepath.Join(adopted, "frames"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := clipFiles(2)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(adopted, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	rec = e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/replay/clips", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var list clipListDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for _, c := range list.Clips {
		byID[c.ID]++
		if c.ID == "adopted" && c.CreatedBy != "recovered" {
			t.Fatalf("the adopted clip's created_by = %q, want recovered", c.CreatedBy)
		}
	}
	if len(list.Clips) != 2 || byID["kept"] != 1 || byID["adopted"] != 1 {
		t.Fatalf("list = %+v, want the installed clip and the adopted directory", list.Clips)
	}

	// The directory disappears; the row stays, flagged.
	if err := os.RemoveAll(filepath.Join(e.dir, "kept")); err != nil {
		t.Fatal(err)
	}
	rec = e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/replay/clips", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list after removal = %d", rec.Code)
	}
	var after clipListDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	for _, c := range after.Clips {
		if c.ID == "kept" && !c.Missing {
			t.Fatalf("the lost clip is not flagged missing: %+v", c)
		}
	}
	// The delete is the cleanup: 204 and the row is gone.
	rec = e.send(auth.RoleDeveloper, http.MethodDelete, "/api/v1/replay/clips/kept", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cleanup delete = %d: %s", rec.Code, rec.Body.String())
	}
	rec = e.send(auth.RoleDeveloper, http.MethodGet, "/api/v1/replay/clips", nil)
	var final clipListDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &final); err != nil {
		t.Fatal(err)
	}
	for _, c := range final.Clips {
		if c.ID == "kept" {
			t.Fatalf("the row survived the cleanup delete: %+v", c)
		}
	}
}

// TestClipRolesAndTraversal is the authorization and path-safety table: the
// operator tier cannot touch the store (a clip is gameplay recording), and a
// delete whose id is not a clip id is refused before any filesystem access.
func TestClipRolesAndTraversal(t *testing.T) {
	e := newClipsEnv(t)
	// Seed one clip so the delete tests have a real target.
	rec := e.uploadClip(t, auth.RoleDeveloper, tarArchive(t, clipFiles(1), "target"), "target.tar", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup upload = %d: %s", rec.Code, rec.Body.String())
	}

	// The operator tier is denied on all three verbs (docs/spec/08: a clip is
	// a recording of gameplay frames, the developer-evidence tier).
	for _, tc := range []struct{ method, target string }{
		{http.MethodPost, "/api/v1/replay/clips"},
		{http.MethodGet, "/api/v1/replay/clips"},
		{http.MethodDelete, "/api/v1/replay/clips/target"},
	} {
		var r *http.Request
		if tc.method == http.MethodPost {
			r = jsonRequest(tc.method, tc.target, `{}`)
		} else {
			r = httptest.NewRequest(tc.method, tc.target, nil)
		}
		r = e.a.as(auth.RoleOperator, r)
		out := doReq(t, e.h, r)
		if out.Code != http.StatusForbidden {
			t.Fatalf("%s %s as operator = %d, want 403: %s", tc.method, tc.target, out.Code, out.Body.String())
		}
	}

	// The admin passes by the role order: the device scripts' token.
	rec = e.send(auth.RoleAdmin, http.MethodGet, "/api/v1/replay/clips", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list as admin = %d, want 200 (the role order)", rec.Code)
	}

	// A traversal-shaped id is refused: 400 from the handler's own check, or
	// 404 if the mux's path cleaning answers first — either way nothing is
	// deleted, and the store is untouched.
	rec = e.send(auth.RoleDeveloper, http.MethodDelete, "/api/v1/replay/clips/..%2F..%2Fetc", nil)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Fatalf("traversal delete = %d, want 400 or 404: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(e.dir, "target")); err != nil {
		t.Fatalf("the traversal delete touched the store: %v", err)
	}
}

// TestClipStoreUnwired: a server without the clip store answers 503 with the
// named code — not an empty list, which would hide the configuration.
func TestClipStoreUnwired(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler() // no WithClips at all
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/replay/clips"},
		{http.MethodPost, "/api/v1/replay/clips"},
		{http.MethodDelete, "/api/v1/replay/clips/whatever"},
	} {
		var r *http.Request
		if tc.method == http.MethodPost {
			r = jsonRequest(tc.method, tc.target, `{}`)
		} else {
			r = httptest.NewRequest(tc.method, tc.target, nil)
		}
		out := doReq(t, h, a.as(auth.RoleDeveloper, r))
		if out.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s unwired = %d, want 503: %s", tc.method, tc.target, out.Code, out.Body.String())
		}
		if !strings.Contains(out.Body.String(), "clips-unavailable") {
			t.Fatalf("the 503 does not name the code: %s", out.Body.String())
		}
	}
}
