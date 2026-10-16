package api

import (
	"bytes"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// Static-asset serving for the embedded React SPA (docs/spec/09 "Technology",
// "Embedded binary test"). Every request that no API route claims lands here
// (Handler dispatches it): real files from webassets.FS() are streamed, and any
// other extensionless path returns index.html so client-side routes
// (/diagnostics, /games/x/dialogue) work as deep links after a server restart.
//
// /api/ paths never reach this code: they keep the RFC 9457 problem+json
// 404/405 behaviour of router.go.

// staticCSP is sent with every UI response. The app is same-origin only:
// scripts and styles come from the embedded bundle, media/images use blob:/data:
// URLs for capture previews, and framing/script injection are refused
// (docs/spec/09 "Embedded binary test" requires CSP to be exercised, not just
// present — web/e2e/embedded.spec.ts asserts the app still runs under it).
const staticCSP = "default-src 'self'; img-src 'self' data: blob:; " +
	"media-src 'self' blob:; connect-src 'self'; style-src 'self'; " +
	"script-src 'self'; frame-ancestors 'none'; base-uri 'self'"

// Cache policies: Vite emits content-hashed filenames under assets/, so those
// URLs can never go stale; the SPA entry point must be revalidated so a new
// binary is picked up without a hard refresh.
const (
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheNoCache   = "no-cache"
)

// assetDir is Vite's hashed-output directory inside the build.
const assetDir = "assets/"

// indexFile is the SPA entry point and the target of the deep-link fallback.
const indexFile = "index.html"

// NotBuiltDetail is the problem+json detail returned for UI paths when the
// binary was compiled without a Vite build inside.
const NotBuiltDetail = "web UI not built; run make build"

// contentTypes pins media types the UI depends on. mime.TypeByExtension is
// affected by the host's /etc/mime.types, which on some systems maps .js to
// application/javascript (browsers refuse to run a module script served that
// way) or omits .svg — so the values that matter are declared here first.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".js":          "text/javascript",
	".mjs":         "text/javascript",
	".css":         "text/css",
	".svg":         "image/svg+xml",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".txt":         "text/plain; charset=utf-8",
	".wasm":        "application/wasm",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".mp3":         "audio/mpeg",
	".ogg":         "audio/ogg",
	".wav":         "audio/wav",
	".mp4":         "video/mp4",
	".webm":        "video/webm",
}

// staticContentType resolves the media type for an embedded file name.
func staticContentType(name string) string {
	if ct, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(strings.ToLower(path.Ext(name))); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// serveStatic answers every non-API request.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	s.setStaticSecurityHeaders(w)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.writeProblem(w, r, http.StatusMethodNotAllowed, "method-not-allowed",
			"Method Not Allowed",
			"method "+r.Method+" is not allowed on "+r.URL.Path+
				"; static assets are served for GET and HEAD only")
		return
	}

	if !s.webBuilt() {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "web-not-built",
			"Service Unavailable", NotBuiltDetail)
		return
	}

	// name is "" for the SPA root ("/"), which serves index.html.
	name, ok := staticName(r.URL.EscapedPath(), r.URL.Path)
	if !ok {
		s.staticNotFound(w, r)
		return
	}

	if name != "" {
		if f, info, found := s.openAsset(name); found {
			if strings.HasPrefix(name, assetDir) {
				w.Header().Set("Cache-Control", cacheImmutable)
			} else {
				w.Header().Set("Cache-Control", cacheNoCache)
			}
			s.serveFile(w, r, name, f, info)
			return
		}
		// A missing path that looks like a file (it has an extension) is a
		// broken asset reference, not a client-side route: answering with
		// index.html would turn a 404 into unparseable HTML.
		if path.Ext(name) != "" {
			s.staticNotFound(w, r)
			return
		}
	}

	f, info, err := s.openAssetStrict(indexFile)
	if err != nil {
		// webBuilt() said index.html exists, so this is a corrupted bundle.
		s.log.Error("embedded index.html unreadable", "error", err)
		s.writeProblem(w, r, http.StatusInternalServerError, "web-ui-broken",
			"Internal Server Error", "embedded "+indexFile+" could not be read; rebuild the binary")
		return
	}
	w.Header().Set("Cache-Control", cacheNoCache)
	s.serveFile(w, r, indexFile, f, info)
}

// staticName maps a request path to an embedded file name. Escaped paths are
// rejected outright when they hide a dot segment or a separator (%2e, %2f,
// %5c, or a backslash) so traversal attempts never reach the filesystem, and
// the decoded path must survive fs.ValidPath after cleaning. An empty name
// with ok=true means "SPA root".
func staticName(escapedPath, decodedPath string) (string, bool) {
	if decodedPath == "" {
		return "", false
	}
	raw := strings.ToLower(escapedPath)
	if strings.Contains(raw, "%2e") || strings.Contains(raw, "%2f") ||
		strings.Contains(raw, "%5c") || strings.Contains(raw, `\`) {
		return "", false
	}
	if strings.ContainsRune(decodedPath, 0) || strings.Contains(decodedPath, "..") ||
		strings.Contains(decodedPath, `\`) {
		return "", false
	}

	cleaned := path.Clean("/" + strings.TrimPrefix(decodedPath, "/"))
	if cleaned == "/" {
		return "", true
	}
	name := strings.TrimPrefix(cleaned, "/")
	if !fs.ValidPath(name) || name == "." {
		return "", false
	}
	return name, true
}

// openAsset opens an embedded regular file. Directories and errors are
// reported as "not found": the caller decides between a problem+json 404 and
// the index.html deep-link fallback.
func (s *Server) openAsset(name string) (fs.File, fs.FileInfo, bool) {
	f, info, err := s.openAssetStrict(name)
	if err != nil {
		return nil, nil, false
	}
	if info.IsDir() {
		_ = f.Close()
		return nil, nil, false
	}
	return f, info, true
}

func (s *Server) openAssetStrict(name string) (fs.File, fs.FileInfo, error) {
	f, err := s.webFS.Open(name)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// serveFile writes an embedded payload with its media type, letting
// http.ServeContent handle HEAD, Range and conditional requests. Embedded
// files are seekable; anything else is buffered so Seek is still available.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name string, f fs.File, info fs.FileInfo) {
	defer func() {
		if err := f.Close(); err != nil {
			s.log.Error("closing embedded asset", "asset", name, "error", err)
		}
	}()

	w.Header().Set("Content-Type", staticContentType(name))

	body, ok := f.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(f)
		if err != nil {
			s.log.Error("reading embedded asset", "asset", name, "error", err)
			return
		}
		body = bytes.NewReader(data)
	}
	http.ServeContent(w, r, path.Base(name), info.ModTime(), body)
}

// setStaticSecurityHeaders is the UI response header set. Problem+json
// responses on UI paths carry it too, so no UI-path response is unprotected.
func (s *Server) setStaticSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", staticCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// emptyFS is the filesystem used when no UI build is available at all (e.g.
// WithWebAssets(nil)). Every lookup fails, so UI paths answer 503 instead of
// dereferencing a nil filesystem.
type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

// staticNotFound is the problem+json 404 for UI paths; the security headers
// are already on the writer from serveStatic.
func (s *Server) staticNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeProblem(w, r, http.StatusNotFound, "not-found", "Not Found",
		"no embedded asset matches the requested path")
}
