package clips_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/clips"
)

// framePNG renders a small solid PNG; the layout tests need a real decodable
// frame, not a placeholder, because ValidateDir decodes.
func framePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatalf("encoding the frame: %v", err)
	}
	return b.Bytes()
}

// layoutFiles is a minimal clip layout: the manifest plus the two frames it
// names, at the layout root.
func layoutFiles(t *testing.T) map[string][]byte {
	return map[string][]byte{
		"timestamps.json": []byte(`[{"file":"a.png","ts_ns":100000000},{"file":"b.png","ts_ns":200000000}]`),
		"a.png":           framePNG(t),
		"b.png":           framePNG(t),
	}
}

// tarBytes packs files into a tar; under, when non-empty, nests them all in
// one top-level directory (the wrapped shape).
func tarBytes(t *testing.T, files map[string][]byte, under string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if under != "" {
		if err := tw.WriteHeader(&tar.Header{Name: under + "/", Mode: 0o700, Typeflag: tar.TypeDir}); err != nil {
			t.Fatalf("tar dir header: %v", err)
		}
	}
	for name, data := range files {
		full := name
		if under != "" {
			full = under + "/" + name
		}
		if err := tw.WriteHeader(&tar.Header{Name: full, Mode: 0o600, Size: int64(len(data))}); err != nil {
			t.Fatalf("tar header %s: %v", full, err)
		}
		if _, err := io.Copy(tw, bytes.NewReader(data)); err != nil {
			t.Fatalf("tar body %s: %v", full, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar: %v", err)
	}
	return b.Bytes()
}

// zipBytes packs files into a zip; under, when non-empty, nests them all in
// one top-level directory.
func zipBytes(t *testing.T, files map[string][]byte, under string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	if under != "" {
		if _, err := zw.Create(under + "/"); err != nil {
			t.Fatalf("zip dir: %v", err)
		}
	}
	for name, data := range files {
		full := name
		if under != "" {
			full = under + "/" + name
		}
		f, err := zw.Create(full)
		if err != nil {
			t.Fatalf("zip header %s: %v", full, err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatalf("zip body %s: %v", full, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the zip: %v", err)
	}
	return b.Bytes()
}

func extractInto(t *testing.T, archive []byte) (string, clips.Extraction) {
	t.Helper()
	dest := t.TempDir()
	format, r, err := clips.DetectFormat(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("DetectFormat: %v", err)
	}
	if format != clips.FormatTar && format != clips.FormatZip {
		t.Fatalf("format = %v, want tar or zip", format)
	}
	out, err := clips.Extract(t.Context(), r, format, dest, clips.DefaultLimits())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return dest, out
}

func TestExtractTarAndZipBothProduceTheSameLayout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		archive []byte
	}{
		{"tar-root", tarBytes(t, layoutFiles(t), "")},
		{"tar-wrapped", tarBytes(t, layoutFiles(t), "my-clip")},
		{"zip-root", zipBytes(t, layoutFiles(t), "")},
		{"zip-wrapped", zipBytes(t, layoutFiles(t), "my-clip")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest, out := extractInto(t, tc.archive)
			layoutDir := dest
			if strings.HasSuffix(tc.name, "wrapped") {
				if out.TopDir != "my-clip" {
					t.Fatalf("TopDir = %q, want my-clip", out.TopDir)
				}
				layoutDir = filepath.Join(dest, out.TopDir)
			} else if out.TopDir != "" {
				t.Fatalf("TopDir = %q, want empty for a root layout", out.TopDir)
			}
			clip, err := clips.ValidateDir(layoutDir)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			if clip.Frames != 2 {
				t.Fatalf("Frames = %d, want 2", clip.Frames)
			}
			if clip.DurationNS != 100_000_000 {
				t.Fatalf("DurationNS = %d, want 100000000", clip.DurationNS)
			}
		})
	}
}

func TestExtractRefusesTraversalSymlinksAndSpecials(t *testing.T) {
	limit := clips.DefaultLimits()
	run := func(t *testing.T, archive []byte) error {
		format, r, err := clips.DetectFormat(bytes.NewReader(archive))
		if err != nil {
			t.Fatalf("DetectFormat: %v", err)
		}
		_, err = clips.Extract(t.Context(), r, format, t.TempDir(), limit)
		return err
	}

	t.Run("zip-slip dotdot", func(t *testing.T) {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		data := framePNG(t)
		if err := tw.WriteHeader(&tar.Header{Name: "../evil.png", Mode: 0o600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(tw, bytes.NewReader(data))
		_ = tw.Close()
		err := run(t, b.Bytes())
		if err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("err = %v, want the traversal refusal", err)
		}
	})

	t.Run("embedded dotdot", func(t *testing.T) {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		if err := tw.WriteHeader(&tar.Header{Name: "a/../../evil.png", Mode: 0o600, Size: 1}); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(tw, "x")
		_ = tw.Close()
		err := run(t, b.Bytes())
		if err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("err = %v, want the traversal refusal", err)
		}
	})

	t.Run("absolute path", func(t *testing.T) {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		data := framePNG(t)
		if err := tw.WriteHeader(&tar.Header{Name: "/etc/passwd", Mode: 0o600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(tw, bytes.NewReader(data))
		_ = tw.Close()
		err := run(t, b.Bytes())
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("err = %v, want the absolute-path refusal", err)
		}
	})

	t.Run("tar symlink", func(t *testing.T) {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}); err != nil {
			t.Fatal(err)
		}
		_ = tw.Close()
		err := run(t, b.Bytes())
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("err = %v, want the symlink refusal", err)
		}
	})

	t.Run("tar hard link", func(t *testing.T) {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "target", Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		_ = tw.Close()
		err := run(t, b.Bytes())
		if err == nil || !strings.Contains(err.Error(), "hard link") {
			t.Fatalf("err = %v, want the hard-link refusal", err)
		}
	})

	t.Run("zip symlink", func(t *testing.T) {
		// A hostile zip records the symlink in the entry's mode bits; the
		// stdlib header writer carries them through the external attributes.
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		hdr := &zip.FileHeader{Name: "link", Method: zip.Store}
		hdr.SetMode(os.ModeSymlink)
		if _, err := zw.CreateHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		err := run(t, b.Bytes())
		if err == nil || !strings.Contains(err.Error(), "link entry") {
			t.Fatalf("err = %v, want the link-entry refusal", err)
		}
	})

	t.Run("entry count cap", func(t *testing.T) {
		files := map[string][]byte{}
		for i := 0; i < 12; i++ {
			files[string(rune('a'+i))+".png"] = []byte("x")
		}
		format, r, err := clips.DetectFormat(bytes.NewReader(tarBytes(t, files, "")))
		if err != nil {
			t.Fatal(err)
		}
		lim := clips.Limits{MaxUncompressedBytes: 1 << 20, MaxEntries: 5, MaxFileSize: 1 << 20}
		_, err = clips.Extract(t.Context(), r, format, t.TempDir(), lim)
		if err == nil || !strings.Contains(err.Error(), "entries") {
			t.Fatalf("err = %v, want the entry-count refusal", err)
		}
	})
}

func TestExtractRefusesDeclaredSizesAboveTheCap(t *testing.T) {
	// A tar that declares more bytes than the per-entry cap is refused before
	// a single byte is written: the declared size is checked first, so an
	// archive cannot even start the stream it is not entitled to.
	lim := clips.Limits{MaxUncompressedBytes: 1 << 20, MaxEntries: 10, MaxFileSize: 64}
	declared := lim.MaxFileSize + 1
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o600, Size: declared}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bytes.Repeat([]byte{0x42}, int(declared))); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	format, r, err := clips.DetectFormat(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	_, err = clips.Extract(t.Context(), r, format, dest, lim)
	if err == nil || !strings.Contains(err.Error(), "per-entry") {
		t.Fatalf("err = %v, want the per-entry declared-size refusal", err)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatalf("a refused archive left %d entries in the staging directory", len(entries))
	}
}

func TestExtractTotalExpansionCap(t *testing.T) {
	// Many small entries whose sum passes the total cap: each entry is under
	// the per-file bound, so only the running total can catch it.
	files := map[string][]byte{}
	for i := 0; i < 6; i++ {
		files[string(rune('a'+i))+".bin"] = bytes.Repeat([]byte{0x01}, 300)
	}
	format, r, err := clips.DetectFormat(bytes.NewReader(tarBytes(t, files, "")))
	if err != nil {
		t.Fatal(err)
	}
	lim := clips.Limits{MaxUncompressedBytes: 1500, MaxEntries: 100, MaxFileSize: 500}
	_, err = clips.Extract(t.Context(), r, format, t.TempDir(), lim)
	if err == nil || !strings.Contains(err.Error(), "limits") {
		t.Fatalf("err = %v, want the total-expansion refusal", err)
	}
}

func TestDetectFormat(t *testing.T) {
	good := tarBytes(t, layoutFiles(t), "")
	if f, _, err := clips.DetectFormat(bytes.NewReader(good)); err != nil || f != clips.FormatTar {
		t.Fatalf("tar: %v %v", f, err)
	}
	z := zipBytes(t, layoutFiles(t), "")
	if f, _, err := clips.DetectFormat(bytes.NewReader(z)); err != nil || f != clips.FormatZip {
		t.Fatalf("zip: %v %v", f, err)
	}
	if f, _, err := clips.DetectFormat(bytes.NewReader([]byte("definitely not an archive"))); err != nil || f != clips.FormatUnknown {
		t.Fatalf("unknown: %v %v", f, err)
	}
	// A gzip prefix is refused, not guessed at: the container must be plain.
	var gz bytes.Buffer
	gz.Write([]byte{0x1f, 0x8b, 0x08, 0x00})
	gz.Write(tarBytes(t, layoutFiles(t), ""))
	if f, _, _ := clips.DetectFormat(bytes.NewReader(gz.Bytes())); f != clips.FormatUnknown {
		t.Fatalf("gzip prefix: got %v, want unknown", f)
	}
}
