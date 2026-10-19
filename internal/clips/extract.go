// Archive extraction with the containment rules docs/spec/14 "Filesystem"
// applies to every untrusted byte that lands on disk.
//
// The archive is untrusted input (AGENTS.md rule 6: downloads are untrusted;
// rule 5: user content). Every entry is checked before any byte of it is
// written, and the entry bytes are capped while they are written, so the
// classic archive attacks are refused rather than cleaned up after:
//
//   - zip-slip / traversal: an entry whose cleaned name escapes the
//     destination (a leading or embedded "..", an absolute path, a NUL) is
//     refused; the join is re-verified against the destination lexically, the
//     same discipline internal/api.safeDiscImagePath applies to basenames.
//   - symlinks and special files: a link entry — in tar by its typeflag, in
//     zip by the mode bits the archive recorded — or a hard link, device or
//     FIFO is refused outright. A symlink is how an archive points a path
//     inside the managed store at one outside it, so there is no mode in
//     which one is written.
//   - size bombs: per-file, total-uncompressed and entry-count limits are
//     enforced while streaming, with the write itself bounded (io.CopyN), so
//     a declared size that lies small cannot grow a file past the cap.
package clips

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Format names the archive container.
type Format int

const (
	// FormatUnknown is the answer for bytes that are neither: the caller
	// answers 400, never a guess. (gzip-wrapped tar is therefore refused too:
	// the container must be plain tar or zip.)
	FormatUnknown Format = iota
	FormatTar
	FormatZip
)

// String answers "what kind of archive is this", for problem details.
func (f Format) String() string {
	switch f {
	case FormatTar:
		return "tar"
	case FormatZip:
		return "zip"
	default:
		return "unrecognized"
	}
}

// Limits bound one extraction. The defaults sit above the large real case —
// a 10 Hz recording of a long session of 640x480 PNG frames — and below a
// full appliance disk.
type Limits struct {
	// MaxUncompressedBytes caps the total bytes the archive expands to.
	MaxUncompressedBytes int64
	// MaxEntries caps the number of archive entries (files and directories).
	MaxEntries int
	// MaxFileSize caps one entry's uncompressed size.
	MaxFileSize int64
}

// DefaultLimits is the managed clip store's bound.
func DefaultLimits() Limits {
	return Limits{
		MaxUncompressedBytes: 1 << 30,  // 1 GiB extracted
		MaxEntries:           50_000,   // a 14 h recording at 10 Hz plus headroom
		MaxFileSize:          64 << 20, // 64 MiB per frame
	}
}

// Extraction is what Extract did, for the provenance note the installer
// records: how many entries, how many bytes, and the single top-level
// directory the archive wrapped the layout in ("" when the layout is at the
// archive root or the archive has several top-level entries).
type Extraction struct {
	Entries      int
	Uncompressed int64
	TopDir       string
}

// DetectFormat answers from the first bytes only: zip carries its
// "PK\x03\x04" signature; tar is the 512-byte header whose checksum the
// standard reader verifies; anything else is unknown. The returned reader
// hands the consumed prefix back, so the caller streams the same bytes into
// Extract.
func DetectFormat(r io.Reader) (Format, io.Reader, error) {
	hdr := make([]byte, 512)
	n, err := io.ReadFull(r, hdr)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return FormatUnknown, nil, fmt.Errorf("clips: reading the archive header: %w", err)
	}
	hdr = hdr[:n]
	rest := io.MultiReader(bytes.NewReader(hdr), r)
	if len(hdr) >= 4 && bytes.Equal(hdr[:4], []byte{'P', 'K', 0x03, 0x04}) {
		return FormatZip, rest, nil
	}
	if len(hdr) == 512 {
		th, terr := tar.NewReader(bytes.NewReader(hdr)).Next()
		if terr == nil && th != nil && th.Name != "" {
			return FormatTar, rest, nil
		}
	}
	return FormatUnknown, rest, nil
}

// safeEntryName is the lexical containment check for one archive entry name.
// It answers the cleaned relative path the entry must be written at, or a
// refusal that names the rule — not the path — so the error can be shown to
// the uploader.
func safeEntryName(name string) (string, error) {
	if name == "" {
		return "", errors.New("an entry has an empty name")
	}
	if strings.ContainsRune(name, '\x00') {
		return "", errors.New("an entry name carries a NUL byte")
	}
	if strings.ContainsRune(name, '\\') {
		return "", errors.New("an entry name uses a backslash separator")
	}
	if filepath.IsAbs(name) {
		return "", errors.New("an entry is an absolute path")
	}
	cleaned := filepath.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("an entry name escapes the archive root")
	}
	return cleaned, nil
}

// verifyDest is the second, independent containment check: after the join,
// the destination must still sit at or under dest.
func verifyDest(dest, out string) error {
	rel, err := filepath.Rel(dest, out)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("an entry resolves outside the extraction directory")
	}
	return nil
}

// topTracker records whether the archive's entries live under one shared
// top-level directory: the layout-wrapped shape the installer unwraps.
type topTracker struct {
	top string
	// multi is true once two different top-level names were seen.
	multi bool
}

func (t *topTracker) add(rel string) {
	if rel == "" || rel == "." {
		return
	}
	top := strings.SplitN(rel, string(filepath.Separator), 2)[0]
	switch {
	case t.top == "":
		t.top = top
	case t.top != top:
		t.multi = true
	}
}

// finish answers the single-root name ("" when there is none or several).
func (t *topTracker) finish() string {
	if t.multi || t.top == "" {
		return ""
	}
	return t.top
}

// Extract streams the archive into dest (created if missing), enforcing lim
// and the security rules above. dest must be a fresh, server-chosen staging
// path; Extract never removes anything and never follows a link it did not
// create. On error the caller removes dest; Extract leaves it in place.
func Extract(ctx context.Context, r io.Reader, format Format, dest string, lim Limits) (Extraction, error) {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return Extraction{}, fmt.Errorf("clips: creating the staging directory: %w", err)
	}
	var (
		out Extraction
		tt  topTracker
	)
	var err error
	switch format {
	case FormatTar:
		err = extractTar(ctx, r, dest, lim, &out, &tt)
	case FormatZip:
		err = extractZip(ctx, r, dest, lim, &out, &tt)
	default:
		return Extraction{}, errors.New("clips: the archive is neither tar nor zip")
	}
	if err != nil {
		return Extraction{}, err
	}
	out.TopDir = tt.finish()
	return out, nil
}

// writeEntryFile streams one file entry at rel under dest under the size
// caps. rel must name a file (never the destination itself).
func writeEntryFile(ctx context.Context, src io.Reader, dest, rel string, declared int64, lim Limits, out *Extraction) error {
	if rel == "" || rel == "." {
		return errors.New("a file entry names the extraction root")
	}
	if declared > lim.MaxFileSize {
		return fmt.Errorf("an entry is declared to be %d bytes, above the %d byte per-entry limit", declared, lim.MaxFileSize)
	}
	dir := filepath.Dir(filepath.Join(dest, rel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the entry's directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dest, rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating the entry file: %w", err)
	}
	defer f.Close()
	// The write is bounded one byte past both caps; a declared size that lies
	// small is caught the moment the stream exceeds the bound.
	bound := lim.MaxFileSize
	if remaining := lim.MaxUncompressedBytes - out.Uncompressed; remaining < bound {
		bound = remaining
	}
	n, cerr := io.CopyN(f, src, bound+1)
	out.Uncompressed += n
	switch {
	case n > bound:
		return fmt.Errorf("the archive exceeds its limits: an entry is larger than %d bytes or the expansion passes %d bytes total", lim.MaxFileSize, lim.MaxUncompressedBytes)
	case cerr != nil && !errors.Is(cerr, io.EOF):
		return fmt.Errorf("writing the entry: %w", cerr)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing the entry: %w", err)
	}
	return nil
}

func extractTar(ctx context.Context, r io.Reader, dest string, lim Limits, out *Extraction, tt *topTracker) error {
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the tar: %w", err)
		}
		if out.Entries >= lim.MaxEntries {
			return fmt.Errorf("the archive has more than %d entries", lim.MaxEntries)
		}
		rel, err := safeEntryName(h.Name)
		if err != nil {
			return err
		}
		if err := verifyDest(dest, filepath.Join(dest, rel)); err != nil {
			return err
		}
		out.Entries++
		switch h.Typeflag {
		case tar.TypeDir:
			tt.add(rel)
			if err := os.MkdirAll(filepath.Join(dest, rel), 0o700); err != nil {
				return fmt.Errorf("creating the entry directory: %w", err)
			}
		case tar.TypeReg:
			tt.add(rel)
			if err := writeEntryFile(ctx, tr, dest, rel, h.Size, lim, out); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo, tar.TypeCont:
			return fmt.Errorf("the archive carries a %s entry; only plain files and directories are admitted", kindName(h.Typeflag))
		default:
			// Sparse and GNU extensions are not needed for a frame recording
			// and are refused rather than half-understood.
			return fmt.Errorf("the archive carries an unsupported entry type (%c)", h.Typeflag)
		}
	}
}

func extractZip(ctx context.Context, r io.Reader, dest string, lim Limits, out *Extraction, tt *topTracker) error {
	// The central-directory walk reads the whole archive; the API layer has
	// already capped the archive bytes it will accept, so this allocation is
	// bounded by the same number the upload answered.
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading the zip archive: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("reading the zip: %w", err)
	}
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if out.Entries >= lim.MaxEntries {
			return fmt.Errorf("the archive has more than %d entries", lim.MaxEntries)
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 || mode&os.ModeCharDevice != 0 ||
			mode&os.ModeNamedPipe != 0 || mode&os.ModeSocket != 0 || mode&os.ModeDevice != 0 {
			return errors.New("the archive carries a special or link entry; only plain files and directories are admitted")
		}
		rel, err := safeEntryName(f.Name)
		if err != nil {
			return err
		}
		if err := verifyDest(dest, filepath.Join(dest, rel)); err != nil {
			return err
		}
		out.Entries++
		if f.FileInfo().IsDir() {
			tt.add(rel)
			if err := os.MkdirAll(filepath.Join(dest, rel), 0o700); err != nil {
				return fmt.Errorf("creating the entry directory: %w", err)
			}
			continue
		}
		if strings.HasSuffix(f.Name, "/") {
			return errors.New("a non-directory entry's name ends with a slash")
		}
		tt.add(rel)
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("opening the zip entry: %w", err)
		}
		werr := writeEntryFile(ctx, rc, dest, rel, int64(f.UncompressedSize64), lim, out)
		rc.Close()
		if werr != nil {
			return werr
		}
	}
	return nil
}

func kindName(tf byte) string {
	switch tf {
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeFifo:
		return "FIFO"
	case tar.TypeCont:
		return "continuation"
	default:
		return "special"
	}
}
