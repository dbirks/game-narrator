package assets

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

type exportEntry struct {
	name string
	body []byte
}

// readTar drains a tar stream into ordered name/body pairs.
func readTar(t *testing.T, r io.Reader) []exportEntry {
	t.Helper()
	tr := tar.NewReader(r)
	var out []exportEntry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar body %s: %v", h.Name, err)
		}
		out = append(out, exportEntry{name: h.Name, body: body})
	}
}

// TestExportContainsExactlyTheRequestedBlobs plus a manifest, each body
// byte-identical to what was committed (docs/spec/02 "Profile portability").
func TestExportContainsExactlyTheRequestedBlobs(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)

	a := commitPut(t, l, []byte("first source blob: a synthetic script dump"))
	b := commitPut(t, l, []byte("second source blob: a captured frame image"))
	kept := commitPut(t, l, []byte("not requested: must not appear in the export"))

	var buf bytes.Buffer
	if err := l.Export(ctx, []string{a.SHA256, b.SHA256}, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}

	entries := readTar(t, bytes.NewReader(buf.Bytes()))
	if len(entries) != 3 {
		t.Fatalf("export entries = %v, want two blobs + manifest", namesOf(entries))
	}
	wantNames := []string{"blobs/" + a.SHA256, "blobs/" + b.SHA256, ManifestName}
	for i, want := range wantNames {
		if entries[i].name != want {
			t.Fatalf("entry %d name = %s, want %s", i, entries[i].name, want)
		}
	}
	for i, want := range [][]byte{
		[]byte("first source blob: a synthetic script dump"),
		[]byte("second source blob: a captured frame image"),
	} {
		if !bytes.Equal(entries[i].body, want) {
			t.Fatalf("blob %d body = %q, want %q", i, entries[i].body, want)
		}
	}
	for _, e := range entries {
		if strings.Contains(e.name, kept.SHA256) {
			t.Fatalf("unrequested blob %s leaked into the export", kept.SHA256)
		}
	}

	// manifest.json: {sha256,size,media_type} per entry, in ref order.
	var manifest []Info
	if err := json.Unmarshal(entries[2].body, &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v\n%s", err, entries[2].body)
	}
	wantManifest := []Info{a, b}
	if len(manifest) != len(wantManifest) {
		t.Fatalf("manifest = %+v, want %+v", manifest, wantManifest)
	}
	for i := range wantManifest {
		if manifest[i] != wantManifest[i] {
			t.Fatalf("manifest[%d] = %+v, want %+v", i, manifest[i], wantManifest[i])
		}
	}
	// The manifest must carry the three field names and nothing else, so an
	// importer can validate it without guessing.
	var raw []map[string]any
	if err := json.Unmarshal(entries[2].body, &raw); err != nil {
		t.Fatalf("decode manifest fields: %v", err)
	}
	for i, m := range raw {
		if len(m) != 3 {
			t.Fatalf("manifest[%d] has %d fields (%v), want sha256, size, media_type", i, len(m), m)
		}
		for _, key := range []string{"sha256", "size", "media_type"} {
			if _, ok := m[key]; !ok {
				t.Fatalf("manifest[%d] missing %q: %v", i, key, m)
			}
		}
	}
}

// TestExportIsByteStable makes bundles hashable: the same refs must produce the
// same bytes, with no wall-clock noise in the archive.
func TestExportIsByteStable(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	a := commitPut(t, l, []byte("stable export content A"))
	b := commitPut(t, l, []byte("stable export content B"))

	first := &bytes.Buffer{}
	if err := l.Export(ctx, []string{a.SHA256, b.SHA256}, first); err != nil {
		t.Fatalf("Export 1: %v", err)
	}
	second := &bytes.Buffer{}
	if err := l.Export(ctx, []string{b.SHA256, a.SHA256}, second); err != nil {
		t.Fatalf("Export 2: %v", err)
	}
	third := &bytes.Buffer{}
	if err := l.Export(ctx, []string{a.SHA256, b.SHA256}, third); err != nil {
		t.Fatalf("Export 3: %v", err)
	}

	if !bytes.Equal(first.Bytes(), third.Bytes()) {
		t.Fatal("two exports of the same refs differ byte-for-byte")
	}
	// Order follows the requested refs, so a caller controls the layout; the
	// set of names is what must be identical.
	names := namesOf(readTar(t, bytes.NewReader(second.Bytes())))
	if names[0] != "blobs/"+b.SHA256 || names[1] != "blobs/"+a.SHA256 {
		t.Fatalf("export did not preserve ref order: %v", names)
	}
}

// TestExportDuplicateRefsAreDeduplicated: a duplicate ref would otherwise write
// the same tar name twice, which readers resolve to one entry or reject.
func TestExportDuplicateRefsAreDeduplicated(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	a := commitPut(t, l, []byte("duplicated ref content"))

	var buf bytes.Buffer
	if err := l.Export(ctx, []string{a.SHA256, a.SHA256, a.SHA256}, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	entries := readTar(t, bytes.NewReader(buf.Bytes()))
	if len(entries) != 2 {
		t.Fatalf("export entries = %v, want one blob + manifest", namesOf(entries))
	}
}

// TestExportEmptyRefSet yields just a manifest, which is what a rights-filtered
// default export of a profile with no media looks like.
func TestExportEmptyRefSetYieldsManifestOnly(t *testing.T) {
	l := newStore(t)
	var buf bytes.Buffer
	if err := l.Export(testCtx(t), nil, &buf); err != nil {
		t.Fatalf("Export(nil refs): %v", err)
	}
	entries := readTar(t, bytes.NewReader(buf.Bytes()))
	if len(entries) != 1 || entries[0].name != ManifestName {
		t.Fatalf("export entries = %v, want only %s", namesOf(entries), ManifestName)
	}
	var manifest []Info
	if err := json.Unmarshal(entries[0].body, &manifest); err != nil {
		t.Fatalf("empty manifest: %v", err)
	}
	if len(manifest) != 0 {
		t.Fatalf("empty manifest = %+v, want []", manifest)
	}
}

// TestExportMissingRefWritesNothing is the all-or-nothing guarantee: a partial
// bundle that looks complete is worse than an error.
func TestExportMissingRefWritesNothing(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	a := commitPut(t, l, []byte("present blob"))
	missing := hashOf([]byte("absent blob"))

	var buf bytes.Buffer
	err := l.Export(ctx, []string{a.SHA256, missing}, &buf)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Export with missing ref: err = %v, want ErrNotFound", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("Export wrote %d bytes before failing: %q", buf.Len(), buf.String())
	}
}

// TestExportNilWriterRejected.
func TestExportNilWriterRejected(t *testing.T) {
	l := newStore(t)
	if err := l.Export(testCtx(t), nil, nil); err == nil {
		t.Fatal("Export(nil writer): want error, got nil")
	}
}

// TestExportBodiesSurviveVerification ties export back to the address: whatever
// came out of the archive must still hash to the name it was stored under.
func TestExportBodiesSurviveVerification(t *testing.T) {
	l := newStore(t)
	ctx := testCtx(t)
	content := bytes.Repeat([]byte("voice reference wav bytes "), 512)
	info := commitPut(t, l, content)

	var buf bytes.Buffer
	if err := l.Export(ctx, []string{info.SHA256}, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	entries := readTar(t, bytes.NewReader(buf.Bytes()))
	got := hashOf(entries[0].body)
	if got != info.SHA256 {
		t.Fatalf("exported body hashes to %s, blob is %s", got, info.SHA256)
	}

	// And a re-import of the exported bytes lands on the same address (dedup).
	reimported := commitPut(t, l, entries[0].body)
	if reimported.SHA256 != info.SHA256 {
		t.Fatalf("re-import landed at %s, want %s", reimported.SHA256, info.SHA256)
	}
	if len(blobHashes(t, l)) != 1 {
		t.Fatalf("re-import created a duplicate blob: %v", blobHashes(t, l))
	}
}

func namesOf(entries []exportEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.name
	}
	return out
}
