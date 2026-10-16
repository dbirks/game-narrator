package assets

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ManifestName is the entry name carrying the export inventory inside the tar
// stream.
const ManifestName = "manifest.json"

// Export writes a tar stream of the requested blobs plus a manifest. This is the
// transport for profile portability (docs/spec/02 "Profile portability") and
// for support bundles (docs/spec/14 "Privacy"): the caller chooses the refs, so
// a default export can exclude copyrighted source blobs, and nothing is
// resolved implicitly.
//
// Every ref is validated and resolved before the first byte is written, so a
// partial export caused by a missing blob cannot leave a half-written tar on the
// other end of the pipe.
func (l *Local) Export(ctx context.Context, refs []string, w io.Writer) error {
	if w == nil {
		return fmt.Errorf("assets: Export requires a writer")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Validate first, then resolve, then write — three passes, so no error can
	// arrive after the stream has started.
	unique := make([]string, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		h, err := normalizeHash(ref)
		if err != nil {
			return fmt.Errorf("assets: export: %w", err)
		}
		if seen[h] {
			continue // a duplicate ref would put two entries at one tar name
		}
		seen[h] = true
		unique = append(unique, h)
	}

	entries := make([]Info, 0, len(unique))
	for _, h := range unique {
		info, err := l.Stat(ctx, h)
		if err != nil {
			return fmt.Errorf("assets: export: %w", err)
		}
		entries = append(entries, info)
	}

	manifest, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("assets: export: encode manifest: %w", err)
	}
	manifest = append(manifest, '\n')

	tw := tar.NewWriter(w)
	for _, info := range entries {
		if err := ctx.Err(); err != nil {
			tw.Close()
			return err
		}
		if err := writeBlobToTar(ctx, l, tw, info); err != nil {
			tw.Close()
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     ManifestName,
		Size:     int64(len(manifest)),
		Mode:     filePerm,
		// Fixed epoch: an export of the same blobs must produce the same bytes,
		// so a bundle can be hashed, compared, and deduplicated (docs/spec/02
		// "Use strong SHA-256 on raw data and canonical JSON manifests").
		ModTime: time.Unix(0, 0).UTC(),
		Format:  tar.FormatUSTAR,
	}); err != nil {
		tw.Close()
		return fmt.Errorf("assets: export: manifest header: %w", err)
	}
	if _, err := tw.Write(manifest); err != nil {
		tw.Close()
		return fmt.Errorf("assets: export: manifest body: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("assets: export: close tar: %w", err)
	}
	return nil
}

func writeBlobToTar(ctx context.Context, l *Local, tw *tar.Writer, info Info) error {
	r, got, err := l.Get(ctx, info.SHA256)
	if err != nil {
		return fmt.Errorf("assets: export: %w", err)
	}
	defer r.Close()
	if got.Size != info.Size || got.SHA256 != info.SHA256 {
		return fmt.Errorf("assets: export: blob %s changed while exporting", info.SHA256)
	}

	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     blobsDirName + "/" + info.SHA256,
		Size:     info.Size,
		Mode:     filePerm,
		ModTime:  time.Unix(0, 0).UTC(),
		Format:   tar.FormatUSTAR,
	}); err != nil {
		return fmt.Errorf("assets: export: header %s: %w", info.SHA256, err)
	}
	// Size was taken from Stat; a truncated read is a mismatch, not an EOF to be
	// shrugged off, otherwise the archive would silently contain a short blob.
	n, err := io.Copy(tw, &ctxReader{ctx: ctx, r: r})
	if err != nil {
		return fmt.Errorf("assets: export: body %s: %w", info.SHA256, err)
	}
	if n != info.Size {
		return fmt.Errorf("assets: export: %s: %w: copied %d of %d bytes", info.SHA256, ErrCorrupt, n, info.Size)
	}
	return nil
}
