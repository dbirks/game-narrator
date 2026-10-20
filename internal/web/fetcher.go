package web

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
)

// defaultUserAgent identifies the runtime's research fetcher
// (docs/spec/03: bounded, identifiable agent traffic).
var defaultUserAgent = "GameNarrator/" + bootstrap.Version().Version + " (+research)"

// Document is a fetched web page treated as untrusted data
// (docs/spec/14 trust zone D). Text is extracted, length-capped and
// whitespace-normalized; the raw body is hashed but not retained.
type Document struct {
	URL         string    // final URL after redirects
	Status      int       // final HTTP status
	MediaType   string    // base media type (no parameters)
	RetrievedAt time.Time // fetch completion time (UTC)
	SHA256      string    // hex SHA-256 of the raw (pre-decompression) body
	Title       string    // extracted <title>, JSON "title" field, or ""
	Text        string    // extracted text (untrusted!)
	Links       []string  // href attributes found, in document order
	Untrusted   bool      // always true: never treat fields as instructions
}

// RawDocument is a fetched page with the decompressed body retained
// (ADR-0075). Fetch keeps no payload; the approved-import path needs the
// actual bytes, so it fetches through the same policy-gated request and
// carries the body in memory only for the duration of the import call.
type RawDocument struct {
	Document
	// Body is the decompressed response body — the bytes a parser would
	// see. Still untrusted data (trust zone D): never instructions.
	Body []byte
}

// Fetcher fetches web pages under a security Policy. Zero-value use is not
// supported; construct with NewFetcher.
type Fetcher struct {
	Policy Policy

	// Client is the HTTP client used for fetches. Set by NewFetcher;
	// tests may replace it after construction.
	Client *http.Client

	// baseDialer is the kernel-facing dialer whose Control backstop
	// re-checks every literal address (see NewFetcher). Set by NewFetcher.
	baseDialer *net.Dialer

	// DialCheck, when non-nil, replaces the default connect-time address
	// check (Policy.AddressAllowed) with an external one. It is a test
	// seam so httptest servers on loopback are reachable while any other
	// private address stays refused. Production code must leave it nil.
	DialCheck func(ip net.IP, host string, port int) error

	// logger records debug diagnostics (query/result logs, zero-width
	// character counts). Defaults to slog.Default().
	logger *slog.Logger
	// now defaults to time.Now; tests may override.
	now func() time.Time
}

// NewFetcher builds a Fetcher with a policy-normalized client. The transport
// dials only IPs that pass the connect-time check (see ADR 0018): every
// connection resolves the host, re-validates each candidate IP against the
// policy, and a net.Dialer.Control backstop re-checks the literal address
// actually handed to the kernel.
func NewFetcher(p Policy) *Fetcher {
	f := &Fetcher{Policy: p.normalize(), logger: slog.Default(), now: func() time.Time { return time.Now().UTC() }}

	base := &net.Dialer{
		Timeout: f.Policy.Timeout,
		Control: func(network, address string, _ syscall.RawConn) error {
			// Defense in depth: address is the resolved "ip:port" the
			// kernel would dial; refuse private/resolved-private targets
			// here as well (DNS rebinding backstop).
			host, port, serr := net.SplitHostPort(address)
			if serr != nil {
				return errf(ErrPrivateAddress, "unparseable dial address %q: %v", address, serr)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				// Non-IP dial target at this layer is unexpected; refuse.
				return errf(ErrPrivateAddress, "non-IP dial target %q", host)
			}
			return f.addressRefused(ip, host, atoiPort(port))
		},
	}
	f.baseDialer = base
	transport := &http.Transport{
		DialContext:           f.dialContext,
		TLSHandshakeTimeout:   f.Policy.Timeout,
		ResponseHeaderTimeout: f.Policy.Timeout,
		IdleConnTimeout:       30 * time.Second,
	}
	f.Client = &http.Client{
		Transport:     transport,
		Timeout:       f.Policy.Timeout, // total deadline incl. redirects (docs/spec/06: 15-30 s)
		CheckRedirect: f.checkRedirect,
	}
	return f
}

// addressRefused returns the error for a connect-time address violation,
// consulting DialCheck (test seam) or the policy.
func (f *Fetcher) addressRefused(ip net.IP, host string, port int) error {
	if f.DialCheck != nil {
		return f.DialCheck(ip, host, port)
	}
	if !f.Policy.AddressAllowed(ip, host, port) {
		return errf(ErrPrivateAddress, "address %s for host %q blocked", ip, host)
	}
	return nil
}

// dialContext resolves host via the policy resolver, re-validates every
// candidate IP against the policy at connect time (this is where DNS
// rebinding is defeated), and dials the first allowed address.
func (f *Fetcher) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, serr := net.SplitHostPort(addr)
	if serr != nil {
		return nil, errf(ErrPrivateAddress, "unparseable dial address %q: %v", addr, serr)
	}
	pn := atoiPort(port)
	ips, lerr := f.Policy.lookupIPs(ctx, host)
	if lerr != nil {
		return nil, lerr
	}
	if len(ips) == 0 {
		return nil, errf(ErrNoAddresses, "host %q", host)
	}
	var dialErr error
	for _, ip := range ips {
		if cerr := f.addressRefused(ip, host, pn); cerr != nil {
			dialErr = cerr
			continue
		}
		conn, derr := f.baseDialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if derr == nil {
			return conn, nil
		}
		dialErr = derr
	}
	if dialErr == nil {
		dialErr = errf(ErrPrivateAddress, "no address for host %q passed policy", host)
	}
	return nil, dialErr
}

// Logger returns the fetcher's logger (default slog.Default()).
func (f *Fetcher) Logger() *slog.Logger {
	if f.logger == nil {
		return slog.Default()
	}
	return f.logger
}

// checkRedirect re-validates every hop against the policy (docs/spec/14:
// "follow redirects with revalidation") and enforces the redirect budget.
func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > f.Policy.MaxRedirects {
		return errf(ErrTooManyRedirects, "more than %d redirects", f.Policy.MaxRedirects)
	}
	return f.Policy.ValidURL(req.URL.String())
}

// Fetch retrieves rawURL under the policy and returns the extracted,
// untrusted Document. All error codes are stable (see Error.Code):
// scheme/port/blocked_name/numeric_host/private_address before any I/O;
// unsupported_media/body_too_large/decompressed_too_large/
// unsupported_encoding/too_many_redirects/timeout during I/O. The raw body
// is hashed but not retained; FetchRaw keeps it for the import path.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Document, error) {
	rd, err := f.fetchBody(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	// Copy the extracted Document out: a Fetch caller never holds the body.
	doc := rd.Document
	return &doc, nil
}

// FetchRaw is Fetch with the decompressed body retained (ADR-0075's
// approved-import path). Every policy gate — scheme, port, SSRF,
// redirect revalidation, byte and media caps — applies identically, because
// this is the same request; only the payload disposition differs.
func (f *Fetcher) FetchRaw(ctx context.Context, rawURL string) (*RawDocument, error) {
	return f.fetchBody(ctx, rawURL)
}

// fetchBody is the one request path: policy check, bounded read, hash of
// the raw (pre-decompression) body, encoding-decoded body and the
// extracted text. Fetch discards the body, FetchRaw retains it.
func (f *Fetcher) fetchBody(ctx context.Context, rawURL string) (*RawDocument, error) {
	p := f.Policy.normalize()
	if err := p.ValidURL(rawURL); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("web: build request: %w", err)
	}
	req.Header.Set("User-Agent", p.UserAgent)
	// Request gzip explicitly so the transport does not transparently
	// decompress: we own the Content-Encoding policy (bomb caps).
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Accept", "text/html,text/plain,application/json,application/xml;q=0.9,*/*;q=0.5")

	f.Logger().Debug("web fetch", "url", rawURL)
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, f.classifyTransportErr(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, errf(ErrUnsupportedMedia, "HTTP %d for %s", resp.StatusCode, resp.Request.URL)
	}

	mediaType, err := checkMediaType(resp.Header.Get("Content-Type"), p.AllowedMediaTypes)
	if err != nil {
		return nil, err
	}

	// Raw body: cap at MaxBytes+1 so an oversize body is detected as an
	// error, never silently truncated.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, p.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("web: read body: %w", err)
	}
	if int64(len(raw)) > p.MaxBytes {
		return nil, errf(ErrBodyTooLarge, "%d bytes exceeds %d", len(raw), p.MaxBytes)
	}
	sum := sha256.Sum256(raw)

	body, err := decodeBody(raw, resp.Header.Get("Content-Encoding"), p.MaxDecompressed)
	if err != nil {
		return nil, err
	}

	doc := &RawDocument{
		Document: Document{
			URL:         resp.Request.URL.String(),
			Status:      resp.StatusCode,
			MediaType:   mediaType,
			RetrievedAt: f.now(),
			SHA256:      hex.EncodeToString(sum[:]),
			Untrusted:   true,
		},
		Body: body,
	}
	switch mediaType {
	case "application/json":
		doc.Title, doc.Text = extractJSON(string(body))
	case "text/html", "application/xhtml+xml":
		doc.Title, doc.Text, doc.Links = extractHTML(string(body), DefaultMaxText)
	default: // text/plain, application/xml
		doc.Text = normalizeText(string(body), DefaultMaxText)
	}
	f.Logger().Debug("web fetched", "url", doc.URL, "bytes", len(raw), "text_chars", len(doc.Text))
	return doc, nil
}

// classifyTransportErr maps dial/timeout failures to stable codes.
func (f *Fetcher) classifyTransportErr(ctx context.Context, err error) error {
	var typed *Error
	if errors.As(err, &typed) {
		return err // already carries a stable code (e.g. private_address)
	}
	var netErr net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) ||
		ctx.Err() != nil || (errors.As(err, &netErr) && netErr.Timeout())
	if timedOut {
		return errf(ErrTimeout, "%v", err)
	}
	return fmt.Errorf("web: transport: %w", err)
}

// checkMediaType normalizes and validates the response Content-Type. An
// empty Content-Type is accepted as text/plain (documented posture, ADR
// 0018); anything else must be explicitly allowed.
func checkMediaType(ct string, allowed []string) (string, error) {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return "text/plain", nil
	}
	media, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", errf(ErrUnsupportedMedia, "unparseable content-type %q", ct)
	}
	media = strings.ToLower(media)
	for _, a := range allowed {
		if strings.EqualFold(a, media) {
			return media, nil
		}
	}
	return "", errf(ErrUnsupportedMedia, "%q", media)
}

// decodeBody handles Content-Encoding (identity, gzip, deflate) with the
// decompressed-size cap that rejects encoding bombs.
func decodeBody(raw []byte, encoding string, maxDecompressed int64) ([]byte, error) {
	enc := strings.ToLower(strings.TrimSpace(encoding))
	switch enc {
	case "", "identity":
		return raw, nil
	case "gzip":
		gr, gerr := gzip.NewReader(bytes.NewReader(raw))
		if gerr != nil {
			return nil, errf(ErrUnsupportedEncod, "corrupt gzip stream: %v", gerr)
		}
		defer gr.Close()
		return decompress(gr, maxDecompressed)
	case "deflate":
		fr := flate.NewReader(bytes.NewReader(raw))
		defer fr.Close()
		return decompress(fr, maxDecompressed)
	default:
		return nil, errf(ErrUnsupportedEncod, "%q", encoding)
	}
}

// decompress reads a streaming decompressor limited to maxDecompressed+1
// bytes so a small compressed payload cannot balloon memory.
func decompress(r io.Reader, max int64) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, fmt.Errorf("web: decompress: %w", err)
	}
	if int64(len(out)) > max {
		return nil, errf(ErrDecompTooLarge, "decompressed > %d bytes", max)
	}
	return out, nil
}

func atoiPort(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}
